import assert from "node:assert/strict";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import test from "node:test";

import controller from "./image-qualification-controller-worker.mjs";
import { networkCLI, qualificationNetwork } from "./image-qualification-network.mjs";

const receipt = {
  runId: "image-qualification-42-1",
  attempt: "1",
  owner: "qualification@example.invalid",
  candidateSha: "a".repeat(40),
  deploymentHash: "b".repeat(64),
  attemptId: "c".repeat(64),
  accountId: "123456789012",
  region: "us-west-2",
  securityGroupId: "sg-12345678",
  ipv4: "203.0.113.7",
  expiresAt: new Date(Date.now() + 60_000).toISOString(),
  cleanupNotAfter: new Date(Date.now() + 31 * 60_000).toISOString(),
};

function fixture({ lost = false, foreign = false, delayed = 0, account = receipt.accountId } = {}) {
  const calls = [];
  const requests = [];
  let rules = foreign
    ? [
        {
          SecurityGroupRuleId: "sgr-9999",
          GroupId: receipt.securityGroupId,
          IsEgress: false,
          IpProtocol: "tcp",
          FromPort: 22,
          ToPort: 22,
          CidrIpv4: `${receipt.ipv4}/32`,
          Tags: [],
        },
      ]
    : [];
  let fenced = false;
  return {
    calls,
    requests,
    dispatch: async () => {
      fenced = true;
      calls.push("dispatch");
      return { dispatchedUntil: new Date(Date.now() + 60_000).toISOString() };
    },
    sleep: async () => {},
    call: async (service, operation, input, region) => {
      calls.push(operation);
      requests.push({ service, operation, input: structuredClone(input), region });
      if (service === "sts") return { Account: account };
      if (operation === "describe-security-group-rules") {
        if (rules.length && delayed-- > 0) return { SecurityGroupRules: [] };
        return { SecurityGroupRules: structuredClone(rules) };
      }
      if (operation === "authorize-security-group-ingress") {
        assert.equal(fenced, true);
        assert.equal(input.GroupId, receipt.securityGroupId);
        assert.deepEqual(input.IpPermissions, [
          {
            IpProtocol: "tcp",
            FromPort: 22,
            ToPort: 22,
            IpRanges: [{ CidrIp: `${receipt.ipv4}/32` }],
          },
        ]);
        rules = [
          {
            SecurityGroupRuleId: "sgr-1234",
            GroupId: input.GroupId,
            IsEgress: false,
            IpProtocol: "tcp",
            FromPort: 22,
            ToPort: 22,
            CidrIpv4: `${receipt.ipv4}/32`,
            Tags: input.TagSpecifications[0].Tags,
          },
        ];
        if (lost) throw new Error("response lost after effect");
        return { SecurityGroupRules: structuredClone(rules) };
      }
      if (operation === "revoke-security-group-ingress") {
        assert.deepEqual(input, {
          GroupId: receipt.securityGroupId,
          SecurityGroupRuleIds: ["sgr-1234"],
        });
        rules = [];
        return {};
      }
      throw new Error("unexpected API");
    },
  };
}

test("admission persists dispatch before exact /32 authorization and recovers a lost response", async () => {
  const api = fixture({ lost: true, delayed: 2 });
  assert.deepEqual(await qualificationNetwork(receipt, api), { ruleId: "sgr-1234" });
  assert.equal(
    api.calls.filter((action) => action === "authorize-security-group-ingress").length,
    1,
  );
  assert.ok(api.calls.indexOf("dispatch") < api.calls.indexOf("authorize-security-group-ingress"));
  assert.deepEqual(
    await qualificationNetwork({ ...receipt, ruleId: "sgr-1234" }, { ...api, remove: true }),
    { cleared: true },
  );
  assert.equal(api.calls.filter((action) => action === "revoke-security-group-ingress").length, 1);
  const capture = process.env.QUALIFICATION_POLICY_FIXTURE_DIR;
  if (capture) {
    mkdirSync(capture, { recursive: true });
    writeFileSync(
      path.join(capture, "network-requests.json"),
      JSON.stringify({ receipt, requests: api.requests }, null, 2),
    );
  }
});

test("a dispatch acknowledgement delayed past completed cleanup cannot create ingress", async () => {
  const api = fixture();
  let clock = Date.now();
  const initial = clock;
  const dispatchedUntil = new Date(initial + 60_000).toISOString();
  let cleared = false;
  const dispatch = async () => {
    await api.dispatch();
    clock = initial + 120_000;
    await qualificationNetwork(
      { ...receipt, dispatchedUntil },
      { ...api, remove: true, now: () => clock },
    );
    cleared = true;
    clock = initial + 130_000;
    return { dispatchedUntil };
  };
  await assert.rejects(
    qualificationNetwork(
      {
        ...receipt,
        expiresAt: new Date(initial + 120 * 60_000).toISOString(),
        cleanupNotAfter: new Date(initial + 150 * 60_000).toISOString(),
      },
      { ...api, dispatch, now: () => clock },
    ),
    /dispatch deadline expired/,
  );
  assert.equal(cleared, true);
  assert.ok(!api.calls.includes("authorize-security-group-ingress"));
});

test("admission reserves the full native-call budget in both absolute cutoffs", async () => {
  const clock = Date.now();
  await Promise.all(
    [
      [24_999, 60_000, /dispatch deadline expired/],
      [60_000, 24_999, /admission expired before dispatch/],
    ].map(async ([dispatchMs, expiryMs, error]) => {
      const api = fixture();
      await assert.rejects(
        qualificationNetwork(
          {
            ...receipt,
            expiresAt: new Date(clock + expiryMs).toISOString(),
          },
          {
            ...api,
            now: () => clock,
            dispatch: async () => ({ dispatchedUntil: new Date(clock + dispatchMs).toISOString() }),
          },
        ),
        error,
      );
      assert.ok(!api.calls.includes("authorize-security-group-ingress"));
    }),
  );
});

test("foreign duplicate ingress is neither adopted nor deleted", async () => {
  const api = fixture({ foreign: true });
  await assert.rejects(qualificationNetwork(receipt, api), /foreign ingress/);
  await qualificationNetwork(receipt, { ...api, remove: true });
  assert.ok(!api.calls.includes("authorize-security-group-ingress"));
  assert.ok(!api.calls.includes("revoke-security-group-ingress"));
});

test("wrong account and ambiguous no-effect dispatch cannot authorize", async () => {
  const wrong = fixture({ account: "999999999999" });
  await assert.rejects(qualificationNetwork(receipt, wrong), /account mismatch/);
  assert.deepEqual(wrong.calls, ["get-caller-identity"]);
  const ambiguous = fixture();
  await assert.rejects(
    qualificationNetwork({ ...receipt, dispatchedUntil: new Date().toISOString() }, ambiguous),
    /ambiguous/,
  );
  assert.ok(!ambiguous.calls.includes("authorize-security-group-ingress"));
});

test("network inventory follows pagination and refuses repeated cursors", async () => {
  const api = fixture();
  const original = api.call;
  api.call = async (service, action, input, region) =>
    action === "describe-security-group-rules"
      ? { SecurityGroupRules: [], NextToken: "repeated" }
      : original(service, action, input, region);
  await assert.rejects(qualificationNetwork(receipt, api), /cursor repeated/);
});

test("cleanup waits for dispatch and rejects a recorded rule with lost ownership", async () => {
  const api = fixture({ foreign: true });
  const delays = [];
  await assert.rejects(
    qualificationNetwork(
      {
        ...receipt,
        ruleId: "sgr-9999",
        dispatchedUntil: new Date(Date.now() + 20_000).toISOString(),
      },
      { ...api, remove: true, sleep: async (ms) => delays.push(ms) },
    ),
    /no longer owned/,
  );
  assert.ok(delays[0] > 0);
  assert.ok(!api.calls.includes("revoke-security-group-ingress"));
});

test("hosted network credentials require a session and explicit recovery lifetime", () => {
  assert.throws(() => networkCLI({}), /credentials are absent/);
  assert.throws(
    () =>
      networkCLI(
        {
          AWS_ACCESS_KEY_ID: "synthetic",
          AWS_SECRET_ACCESS_KEY: "synthetic",
          AWS_SESSION_TOKEN: "synthetic",
          QUALIFICATION_NETWORK_CREDENTIAL_EXPIRES_AT: new Date(Date.now() + 60_000).toISOString(),
        },
        Date.now() + 120_000,
      ),
    /expiry is insufficient/,
  );
});

test("a three-hour session covers the authoritative two-hour run and thirty-minute cleanup", () => {
  const start = Date.now();
  const cleanupNotAfter = start + 150 * 60_000;
  const env = {
    AWS_ACCESS_KEY_ID: "synthetic",
    AWS_SECRET_ACCESS_KEY: "synthetic",
    AWS_SESSION_TOKEN: "synthetic",
    QUALIFICATION_NETWORK_CREDENTIAL_EXPIRES_AT: new Date(start + 180 * 60_000).toISOString(),
  };
  assert.equal(typeof networkCLI(env, cleanupNotAfter), "function");
  assert.throws(
    () =>
      networkCLI(
        {
          ...env,
          QUALIFICATION_NETWORK_CREDENTIAL_EXPIRES_AT: new Date(cleanupNotAfter - 1).toISOString(),
        },
        cleanupNotAfter,
      ),
    /expiry is insufficient/,
  );
});

function edgeRequest(headers = {}, input = { runId: receipt.runId }) {
  const request = new Request("https://controller.example.workers.dev/executor", {
    method: "POST",
    headers: {
      authorization: `Bearer ${"d".repeat(64)}`,
      "cf-connecting-ip": receipt.ipv4,
      ...headers,
    },
    body: JSON.stringify(input),
  });
  Object.defineProperty(request, "cf", { value: { colo: "TEST" } });
  return request;
}

test("preflight forwards only edge-observed IP and exposes no controller capability", async () => {
  const calls = [];
  const env = {
    AUTHORITY: {
      executorReady: async (...args) => {
        calls.push(args);
        return { armed: false };
      },
    },
  };
  assert.equal((await controller.fetch(edgeRequest(), env)).status, 200);
  assert.deepEqual(calls, [[receipt.runId, "d".repeat(64), receipt.ipv4]]);
  await Promise.all(
    ["cf-worker", "x-real-ip", "cf-connecting-ipv6", "cf-pseudo-ipv4"].map(async (name) => {
      assert.equal((await controller.fetch(edgeRequest({ [name]: "override" }), env)).status, 403);
    }),
  );
  assert.equal(
    (await controller.fetch(edgeRequest({}, { runId: receipt.runId, ipv4: "198.51.100.1" }), env))
      .status,
    409,
  );
  const notEdge = new Request("https://controller.example.workers.dev/executor", {
    method: "POST",
  });
  assert.equal((await controller.fetch(notEdge, env)).status, 403);
  assert.equal(calls.length, 1);
});
