import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { readPrivateJSON } from "./image-qualification-journal.mjs";
import test from "node:test";
import { Cloudflare, recoverActiveQualification } from "./image-qualification-control.mjs";
import {
  validateRecoveryPlan,
  initialRecoveryState,
  recoverTransition,
} from "./image-qualification-recovery.mjs";
import {
  authorityName,
  controllerName,
  bindingShape,
  canonical,
  digest,
} from "./image-qualification-isolation.mjs";
import {
  fixtureAdmission,
  ownedSettings,
  controllerContent,
  version,
} from "./fixtures/qualification-isolation.mjs";

function fixture() {
  const original = fixtureAdmission();
  const sourceSha = "e".repeat(40),
    hash = "b".repeat(64),
    start = Date.parse("2026-11-15T00:00:00Z");
  const binding = {
    runId: "image-qualification-42-1",
    candidateSha: "a".repeat(40),
    candidateWorker: "crabbox-image-qualification-42-1",
    deploymentHash: hash,
    expiresAt: new Date(start + 150 * 60_000).toISOString(),
    enrolledAt: new Date(start + 1000).toISOString(),
    authoritySha: original.record.authority.sourceSha,
    authorityVersion: original.record.authority.logicalVersion,
    policyHash: original.record.authority.policyHash,
  };
  const relay = binding.candidateWorker.replace("qualification-", "qualification-relay-");
  const corrected = structuredClone(original.record);
  Object.assign(corrected.authority, {
    version: "22222222-2222-4222-8222-222222222222",
    sourceSha,
    logicalVersion: "recovery-v1",
  });
  const authorityBindings = structuredClone(original.bindings);
  authorityBindings.find((b) => b.name.endsWith("AUTHORITY_SHA")).text = sourceSha;
  authorityBindings.find((b) => b.name.endsWith("AUTHORITY_VERSION")).text = "recovery-v1";
  authorityBindings.push({
    name: "CRABBOX_AWS_QUALIFICATION_RECOVERY_JSON",
    type: "plain_text",
    text: canonical(binding),
  });
  corrected.authority.bindingDigest = digest(canonical(bindingShape(authorityBindings)));
  const controllerSource = fs.readFileSync(
    new URL("./image-qualification-controller-worker.mjs", import.meta.url),
    "utf8",
  );
  const workers = {
    [authorityName]: {
      settings: { bindings: authorityBindings },
      version: corrected.authority.version,
      module: { module: "authority.js", count: 1, sha256: "f".repeat(64) },
    },
    [controllerName]: {
      settings: ownedSettings(original.admission, controllerName, [
        { name: "CONTROLLER_TOKEN", type: "secret_text" },
        {
          name: "AUTHORITY",
          type: "service",
          service: authorityName,
          entrypoint: "AWSQualificationController",
          props: { deploymentHash: hash },
        },
      ]),
      version,
      module: { module: "controller.mjs", count: 1, sha256: digest(controllerSource) },
    },
    [binding.candidateWorker]: {
      settings: ownedSettings(original.admission, binding.candidateWorker, [
        {
          name: "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
          type: "service",
          service: authorityName,
          props: {
            runId: binding.runId,
            candidateWorker: binding.candidateWorker,
            deploymentHash: hash,
          },
        },
        {
          name: "FLEET",
          type: "durable_object_namespace",
          class_name: "FleetDurableObject",
          namespace_id: "3".repeat(32),
        },
      ]),
      version,
      module: { module: "candidate.js", count: 1, sha256: "4".repeat(64) },
    },
    [relay]: {
      settings: ownedSettings(original.admission, relay, [
        { name: "CANDIDATE", type: "service", service: binding.candidateWorker },
      ]),
      version,
      module: { module: "relay.mjs", count: 1, sha256: "5".repeat(64) },
    },
  };
  const plan = {
    profile: "active-cleanup-transition/v1",
    sourceSha,
    originalAttestationSha256: "6".repeat(64),
    originalHandoffSha256: "7".repeat(64),
    originalAdmission: original.record,
    correctedAdmission: corrected,
    binding,
    callers: [controllerName, binding.candidateWorker, relay].map((name) => ({
      name,
      version,
      module: workers[name].module,
      settingsSha256: digest(
        canonical({
          bindings: bindingShape(workers[name].settings.bindings),
          tags: [...workers[name].settings.tags].sort(),
        }),
      ),
    })),
    authorityModule: workers[authorityName].module,
    handoffStartedAt: new Date(start).toISOString(),
    deadline: new Date(start + 270 * 60_000).toISOString(),
    sessionExpiresAt: new Date(start + 12 * 3600_000).toISOString(),
    maxRequests: 800,
  };
  const events = [],
    saved = [];
  let active = true,
    finalized = false,
    clock = start + 120 * 60_000;
  let loseUpload = false,
    loseDelete = false,
    loseRetire = false,
    loseControllerDelete = false;
  const attestation = () => ({
    version: 1,
    ...binding,
    finalizingAt: new Date(start + 60_000).toISOString(),
    finalized,
    ...(finalized
      ? {
          finalizedAt: new Date(start + 120 * 60_000).toISOString(),
          finalReceipt: {
            finalCounts: { images: 0, instances: 0, keyPairs: 0, snapshots: 0, volumes: 0 },
            failureCodes: [],
          },
        }
      : {}),
  });
  class FakeCloud extends Cloudflare {
    constructor() {
      super(corrected.accountId, "synthetic-token");
    }
    async request(route, init = {}, options = {}) {
      events.push(`${init.method ?? "GET"} ${route}`);
      const m = /^\/workers\/scripts\/([^/?]+)(.*)$/.exec(route),
        name = m?.[1],
        tail = m?.[2];
      const w = workers[name];
      if (init.method === "PUT") {
        const metadata = JSON.parse(await init.body.get("metadata").text()),
          module = init.body.get(metadata.main_module);
        workers[name] = {
          settings: { bindings: metadata.bindings, tags: metadata.tags },
          version: "33333333-3333-4333-8333-333333333333",
          module: {
            module: metadata.main_module,
            count: 1,
            sha256: digest(Buffer.from(await module.arrayBuffer())),
          },
        };
        if (loseUpload) {
          loseUpload = false;
          throw Error("lost upload reply");
        }
        return {};
      }
      if (init.method === "DELETE") {
        delete workers[name];
        if (loseControllerDelete && name === controllerName) {
          loseControllerDelete = false;
          throw Error("lost last deletion reply");
        }
        if (loseDelete) {
          loseDelete = false;
          throw Error("lost delete reply");
        }
        return {};
      }
      if (init.method === "POST" && tail === "/subdomain") {
        w.public = JSON.parse(init.body).enabled;
        return {};
      }
      if (route.startsWith("/workers/durable_objects/namespaces"))
        return [
          ...original.record.authority.namespaces,
          ...Object.entries(workers).flatMap(([script, value]) =>
            value.settings.bindings
              .filter((b) => b.type === "durable_object_namespace" && script !== authorityName)
              .map((b) => ({ script, class: b.class_name, id: b.namespace_id })),
          ),
        ];
      if (route === "/workers/subdomain") return { subdomain: "example" };
      if (tail === "/settings") return w?.settings;
      if (tail === "/content/v2") return w.module;
      if (tail === "/deployments")
        return { deployments: [{ versions: [{ version_id: w.version, percentage: 100 }] }] };
      if (tail === "/subdomain") return { enabled: Boolean(w?.public), previews_enabled: false };
      if (tail === "/schedules") return { schedules: [] };
      if (route.includes("/routes") || route.includes("/domains")) return [];
      throw Error(`unexpected management request ${route}`);
    }
    async list(route) {
      return await this.request(route);
    }
  }
  const fetch = async (input, init = {}) => {
    const url = new URL(input);
    if (
      url.hostname === "api.cloudflare.com" &&
      url.pathname.endsWith(`/${controllerName}/content/v2`)
    )
      return controllerContent(controllerSource);
    assert.equal(
      url.hostname,
      `${controllerName}.example.workers.dev`,
      "all other outbound forbidden",
    );
    events.push(`RPC ${url.pathname}`);
    assert.ok(
      ["/discover", "/attest", "/begin-finalization", "/finalize", "/retire"].includes(
        url.pathname,
      ),
    );
    assert.ok(workers[controllerName]);
    const json = (value) => Response.json(value);
    if (url.pathname === "/discover")
      return json({ run: active ? { ...binding, cleanupState: "finalizing" } : null });
    if (url.pathname === "/attest") return json(attestation());
    if (url.pathname === "/finalize") {
      finalized = true;
      return json({});
    }
    if (url.pathname === "/retire") {
      assert.equal(finalized, true);
      active = false;
      if (loseRetire) {
        loseRetire = false;
        throw Error("lost retirement reply");
      }
      return json({ retired: true });
    }
    return json({ finalizing: true });
  };
  return {
    plan,
    workers,
    events,
    saved,
    fetch,
    state: initialRecoveryState(plan),
    options: () => ({
      run: async (...args) => {
        const result = await recoverActiveQualification(...args);
        assert.equal(result.result, "finalized", result.failure);
        return result;
      },
      callerCF: new FakeCloud(),
      authorityCF: new FakeCloud(),
      token: "x".repeat(64),
      save: async (value) => saved.push(structuredClone(value)),
      now: () => clock,
    }),
    expire: () => {
      clock = Date.parse(plan.deadline);
    },
    loseRetire: () => {
      loseRetire = true;
    },
    loseControllerDelete: () => {
      loseControllerDelete = true;
    },
    loseUpload: () => {
      loseUpload = true;
    },
    loseDelete: () => {
      loseDelete = true;
    },
  };
}
async function local(f, run) {
  const old = globalThis.fetch;
  globalThis.fetch = f.fetch;
  try {
    return await run();
  } finally {
    globalThis.fetch = old;
  }
}

test("exact dual identity runs existing finalizer, namespace migration and ordinary deletion", async () => {
  const f = fixture();
  await local(f, () => recoverTransition(f.plan, f.state, f.options()));
  assert.equal(f.state.settled, true);
  assert.equal(f.state.retired, true);
  assert.deepEqual(Object.keys(f.workers), [authorityName]);
  assert.ok(f.events.includes("RPC /retire"));
  assert.ok(!f.events.some((v) => /force|claim|transport-open/.test(v)));
  assert.equal(f.state.attestation.authoritySha, f.plan.originalAdmission.authority.sourceSha);
  assert.ok(f.saved.some((s) => s.attestation && !s.retired));
});

for (const [name, mutate] of [
  [
    "different namespaces",
    (f) => (f.plan.correctedAdmission.authority.namespaces[0].id = "8".repeat(32)),
  ],
  ["different role", (f) => (f.plan.correctedAdmission.aws.roleArn += "Other")],
  ["different run", (f) => (f.plan.binding.runId = "image-qualification-99-1")],
  ["deadline extension", (f) => (f.plan.deadline = "2099-01-01T00:00:00Z")],
  [
    "wrong authority source",
    (f) => (f.plan.correctedAdmission.authority.sourceSha = "1".repeat(40)),
  ],
])
  test(`${name} invalidates transition plan`, () => {
    const f = fixture();
    mutate(f);
    assert.throws(() => validateRecoveryPlan(f.plan));
  });

for (const [name, mutate] of [
  [
    "module drift",
    (f) =>
      (f.workers[authorityName].module = {
        ...f.workers[authorityName].module,
        sha256: "0".repeat(64),
      }),
  ],
  [
    "caller version drift",
    (f) => (f.workers[controllerName].version = "44444444-4444-4444-8444-444444444444"),
  ],
  ["caller tags rewritten", (f) => (f.workers[controllerName].settings.tags = [])],
  ["missing recovery binding", (f) => f.workers[authorityName].settings.bindings.pop()],
  ["deadline", (f) => f.expire()],
])
  test(`${name} stops without mutations`, async () => {
    const f = fixture();
    mutate(f);
    await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
    assert.ok(f.events.every((v) => v.startsWith("GET ")));
  });

test("cancelled transition stops without mutation", async () => {
  const f = fixture(),
    abort = new AbortController();
  abort.abort();
  await assert.rejects(
    recoverTransition(f.plan, f.state, { ...f.options(), signal: abort.signal }),
  );
  assert.equal(f.events.length, 0);
});

test("uncertain tombstone upload resumes exact original owner and settles", async () => {
  const f = fixture();
  f.loseUpload();
  await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
  assert.equal(f.state.settled, false);
  assert.ok(f.state.uploadIntents[f.plan.binding.candidateWorker]);
  await local(f, () => recoverTransition(f.plan, f.state, f.options()));
  assert.equal(f.state.settled, true);
});

test("uncertain relay deletion reconciles absence without replacement", async () => {
  const f = fixture();
  f.loseDelete();
  await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
  await local(f, () => recoverTransition(f.plan, f.state, f.options()));
  assert.equal(f.state.settled, true);
});

test("settled journal rechecks exact absence; an unrelated worker is preserved", async () => {
  const f = fixture();
  await local(f, () => recoverTransition(f.plan, f.state, f.options()));
  f.workers["production-other"] = { settings: { bindings: [] } };
  await local(f, () => recoverTransition(f.plan, f.state, f.options()));
  assert.ok(f.workers["production-other"]);
});

for (const lost of ["loseRetire", "loseControllerDelete"])
  test(`${lost} resumes canonical retirement and complete caller absence`, async () => {
    const f = fixture();
    f[lost]();
    await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
    assert.ok(f.state.attestation);
    assert.equal(f.state.settled, false);
    await local(f, () => recoverTransition(f.plan, f.state, f.options()));
    assert.equal(f.state.settled, true);
  });
test("request budget survives restart and cannot reset", async () => {
  const f = fixture();
  f.state.requests = 799;
  await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
  assert.equal(f.state.requests, 800);
  const n = f.events.length;
  await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
  assert.equal(f.events.length, n);
});
test("failed journal write refuses before network", async () => {
  const f = fixture();
  await assert.rejects(
    recoverTransition(f.plan, f.state, {
      ...f.options(),
      save: async () => {
        throw Error("disk unavailable");
      },
    }),
  );
  assert.equal(f.events.length, 0);
});

test("original receipts are read once with exact byte digest binding", () => {
  const directory = fs.mkdtempSync(
    path.join(fs.realpathSync(os.tmpdir()), "qualification-receipt-"),
  );
  const file = path.join(directory, "receipt.json"),
    bytes = '{"value":1}\n';
  try {
    fs.writeFileSync(file, bytes, { mode: 0o600 });
    assert.deepEqual(readPrivateJSON(file, digest(bytes)), { value: 1 });
    assert.throws(() => readPrivateJSON(file, "0".repeat(64)), /digest mismatch/);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});
test("missing original controller without retirement receipt cannot be replaced", async () => {
  const f = fixture();
  delete f.workers[controllerName];
  await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
  assert.ok(!f.events.some((event) => /^(PUT|DELETE|RPC)/.test(event)));
});
