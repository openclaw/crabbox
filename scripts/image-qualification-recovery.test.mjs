import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";

import {
  fixtureAdmission,
  ownedSettings,
  controllerContent,
  version,
} from "./fixtures/qualification-isolation.mjs";
import { Cloudflare, recoverActiveQualification } from "./image-qualification-control.mjs";
import {
  authorityName,
  controllerName,
  bindingShape,
  canonical,
  digest,
} from "./image-qualification-isolation.mjs";
import { readPrivateJSON } from "./image-qualification-journal.mjs";
import {
  recoveryBinding,
  validateRecoveryPlan,
  initialRecoveryState,
  recoverTransition,
} from "./image-qualification-recovery.mjs";
import { validateCleanupSession } from "./image-qualification-cleanup-session.mjs";
import { cleanupSessionPolicy, runSessionTags } from "./image-qualification-run-session.mjs";

function fixture(options = {}) {
  const original = fixtureAdmission();
  const sourceSha = options.sourceSha ?? "e".repeat(40),
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
    options.controllerSource ??
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
        { name: "CRABBOX_ADMIN_TOKEN", type: "secret_text" },
        { name: "CRABBOX_SHARED_TOKEN", type: "secret_text" },
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
  let active = options.active ?? true,
    finalized = options.finalized ?? false,
    clock = start + 120 * 60_000;
  let loseSession = false,
    loseUpload = false,
    loseDelete = false,
    loseRetire = false,
    loseControllerDelete = false,
    blockCandidateDelete = false;
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
      if (init.method === "PATCH") {
        assert.equal(tail, "/secrets-bulk");
        assert.equal(name, authorityName);
        assert.deepEqual(
          Object.keys(JSON.parse(init.body).secrets).sort(),
          ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"].sort(),
        );
        w.version = "44444444-4444-4444-8444-444444444444";
        w.settings.annotations = { "workers/triggered_by": "secret" };
        if (loseSession) {
          loseSession = false;
          throw Error("lost synthetic secret delivery");
        }
        return { success: true };
      }
      if (init.method === "PUT") {
        const metadata = JSON.parse(await init.body.get("metadata").text()),
          module = init.body.get(metadata.main_module);
        workers[name] = {
          settings: {
            // The script upload endpoint preserves existing secrets omitted
            // from metadata. Model the observed response, not a replacement.
            bindings: [
              ...metadata.bindings,
              ...w.settings.bindings.filter(
                (b) =>
                  b.type === "secret_text" && !metadata.bindings.some((n) => n.name === b.name),
              ),
            ],
            tags: metadata.tags,
          },
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
        if (blockCandidateDelete && name === binding.candidateWorker)
          throw Error("referenced Worker cannot be deleted");
        delete workers[name];
        if (loseControllerDelete && name === controllerName) {
          ((loseControllerDelete = false), (blockCandidateDelete = false));
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
      if (route === "/tokens/verify")
        return {
          id: "d".repeat(32),
          status: "active",
          expires_on: new Date(start + 3 * 86400000).toISOString(),
        };
      if (route === "/workers/scripts") return Object.keys(workers).map((id) => ({ id }));
      if (route === "/workers/subdomain") return { subdomain: "example" };
      if (tail === "/settings") return w?.settings;
      if (tail === "/content/v2") {
        if (name === controllerName)
          assert.ok([64 * 1024, 1024 * 1024].includes(options.contentIdentityBytes));
        else
          assert.equal(
            options.contentIdentityBytes,
            (name === binding.candidateWorker ? 4 : 1) * 1024 * 1024,
          );
        return w.module;
      }
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
    remoteState: () => ({ active, finalized, workers }),
    events,
    saved,
    fetch,
    state: options.state ?? initialRecoveryState(plan),
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
    loseSession: () => {
      loseSession = true;
    },
    loseControllerDelete: () => {
      loseControllerDelete = true;
    },
    loseUpload: () => {
      loseUpload = true;
    },
    blockCandidateDelete: (value) => {
      blockCandidateDelete = value;
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

function cleanupFixture(t, options) {
  const f = fixture(options),
    dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "qualification-cleanup-session-")));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const start = Date.parse(f.plan.handoffStartedAt),
    at = (minutes) => new Date(start + minutes * 60_000).toISOString();
  const authority = f.workers[authorityName];
  authority.settings.bindings.push(
    ...["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"].map((name) => ({
      name,
      type: "secret_text",
    })),
  );
  authority.settings.annotations = { "workers/triggered_by": "version_upload" };
  f.plan.correctedAdmission.authority.bindingDigest = digest(
    canonical(bindingShape(authority.settings.bindings)),
  );
  f.workers[controllerName].public = true;
  const aws = path.join(dir, "aws");
  fs.writeFileSync(aws, "fake-aws", { mode: 0o600 });
  const scope = {
    profile: "first-walk-aws-session/v2",
    repository: "example/repo",
    workflowSha: "1".repeat(40),
    candidateSha: f.plan.binding.candidateSha,
    runId: f.plan.binding.runId,
    candidateArtifactId: "9",
    candidateArtifactDigest: `sha256:${"2".repeat(64)}`,
    manifestSha256: "3".repeat(64),
    startsAt: at(0),
    workExpiresAt: at(130),
    expiresAt: at(150),
    recoveryExpiresAt: at(270),
  };
  f.plan.binding.taggingContract = "creation-only/v1";
  f.plan.cleanupSession = {
    profile: "same-owner-cleanup-session/v1",
    directory: dir,
    scope,
    tokenId: "d".repeat(32),
    startsAt: at(115),
    deliveryStopsAt: at(135),
    aws: { path: aws, sha256: digest("fake-aws") },
    authoritySettingsSha256: digest(canonical(authority.settings)),
  };
  const pre = {
    profile: f.plan.cleanupSession.profile,
    runId: f.plan.binding.runId,
    sourceSha: f.plan.sourceSha,
    authorityVersion: authority.version,
    exclusiveUntil: f.plan.deadline,
    observedAt: at(120),
    continuousExclusiveCustody: true,
    noConcurrentWritersOrCallers: true,
    noActiveWorkflowWriters: true,
    noOtherSessionConsumers: true,
    fallbackReady: true,
  };
  f.plan.cleanupSession.preCustodySha256 = digest(canonical(pre));
  f.state = initialRecoveryState(f.plan);
  const commands = [];
  const opts = (mode) => ({
    ...f.options(),
    sessionMode: mode,
    sessionOptions: {
      workdir: dir,
      env: {
        AWS_ACCESS_KEY_ID: "synthetic-bootstrap",
        AWS_SECRET_ACCESS_KEY: "synthetic-bootstrap-secret",
        CLOUDFLARE_API_TOKEN: "synthetic-cf",
      },
      readCustody: async (stage) =>
        stage === "pre" ? pre : { ...pre, authorityVersion: authority.version },
      command: async (argv, env, input, options) => {
        commands.push({ argv, env });
        assert.equal(options.cwd, dir);
        assert.equal(input, undefined);
        assert.equal(env.AWS_MAX_ATTEMPTS, "1");
        assert.ok(!options.signal?.aborted);
        assert.equal(f.saved.at(-1).cleanupSession.phase, "issuance-intent");
        assert.ok(fs.existsSync(path.join(dir, "cleanup-session-issuance.json")));
        if (argv.includes("assume-role")) {
          assert.equal(env.AWS_SESSION_TOKEN, undefined);
          assert.equal(argv[argv.indexOf("--duration-seconds") + 1], "43200");
          assert.deepEqual(
            JSON.parse(argv[argv.indexOf("--tags") + 1]),
            Object.entries(runSessionTags(scope)).map(([Key, Value]) => ({ Key, Value })),
          );
          assert.deepEqual(
            JSON.parse(argv[argv.indexOf("--policy") + 1]),
            cleanupSessionPolicy(scope),
          );
          return JSON.stringify({
            Credentials: {
              AccessKeyId: "synthetic-new-id",
              SecretAccessKey: "synthetic-new-secret",
              SessionToken: "synthetic-new-session",
              Expiration: at(840),
            },
          });
        }
        assert.ok(argv.includes("get-caller-identity"));
        assert.equal(env.AWS_SESSION_TOKEN, "synthetic-new-session");
        return JSON.stringify({
          Account: f.plan.correctedAdmission.aws.accountId,
          Arn: `arn:aws:sts::${f.plan.correctedAdmission.aws.accountId}:assumed-role/Qualification/${scope.runId}`,
        });
      },
    },
  });
  return { ...f, opts, commands, pre, dir };
}

test("same-owner cleanup session uses original tags, reconciles a changed version and existing finalizer", async (t) => {
  const f = cleanupFixture(t),
    original = structuredClone(f.plan);
  await local(f, () => recoverTransition(f.plan, f.state, f.opts("deliver")));
  assert.equal(f.state.cleanupSession.phase, "delivery-confirmed");
  assert.equal(f.commands.length, 2);
  assert.ok(!f.events.some((v) => /RPC \/(?:finalize|retire)|^(PUT|DELETE|POST) /.test(v)));
  await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
  await local(f, () => recoverTransition(f.plan, f.state, f.opts("inspect")));
  assert.equal(f.state.cleanupSession.phase, "reconciled");
  const requests = f.state.requests;
  await local(f, () => recoverTransition(f.plan, f.state, f.options()));
  assert.equal(f.state.settled, true);
  assert.ok(f.state.requests > requests);
  assert.deepEqual(f.plan, original);
  assert.deepEqual(Object.keys(f.workers), [authorityName]);
  const receipt = JSON.stringify(f.saved);
  for (const secret of [
    "synthetic-bootstrap",
    "synthetic-new-id",
    "synthetic-new-secret",
    "synthetic-new-session",
  ])
    assert.ok(!receipt.includes(secret));
});

test("lost cleanup delivery response is read-only reconciled, never reissued or retried", async (t) => {
  const f = cleanupFixture(t);
  f.loseSession();
  await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.opts("deliver"))));
  assert.equal(f.state.cleanupSession.phase, "delivery-intent");
  // Simulate a fresh owner loading only its durable nonsecret journal.
  f.state = JSON.parse(JSON.stringify(f.saved.at(-1)));
  await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.opts("deliver"))));
  await local(f, () => recoverTransition(f.plan, f.state, f.opts("inspect")));
  await local(f, () => recoverTransition(f.plan, f.state, f.options()));
  assert.equal(f.state.settled, true);
  assert.equal(f.commands.length, 2);
  assert.equal(f.events.filter((v) => v.startsWith("PATCH ")).length, 1);
});

for (const [title, change] of [
  [
    "foreign run",
    (f) => {
      f.plan.cleanupSession.scope.runId = "image-qualification-99-1";
    },
  ],
  [
    "extended deadline",
    (f) => {
      f.plan.cleanupSession.scope.recoveryExpiresAt = f.plan.sessionExpiresAt;
    },
  ],
  [
    "missing scope",
    (f) => {
      delete f.plan.cleanupSession.scope;
    },
  ],
  [
    "insufficient settlement reserve",
    (f) => {
      f.plan.cleanupSession.startsAt = f.plan.cleanupSession.scope.workExpiresAt;
      f.plan.cleanupSession.deliveryStopsAt = f.plan.deadline;
    },
  ],
  [
    "chained issuer",
    (_f, o) => {
      o.sessionOptions.env.AWS_SESSION_TOKEN = "foreign-session";
    },
  ],
  [
    "custody loss",
    (f) => {
      f.pre.noConcurrentWritersOrCallers = false;
    },
  ],
  [
    "changed module",
    (f) => {
      f.workers[authorityName].module = {
        ...f.workers[authorityName].module,
        sha256: "0".repeat(64),
      };
    },
  ],
  [
    "changed namespace",
    (f) => {
      f.workers[authorityName].settings.bindings.find(
        (b) => b.type === "durable_object_namespace",
      ).namespace_id = "0".repeat(32);
    },
  ],
  [
    "foreign consumer",
    (f) => {
      f.workers["foreign-consumer"] = { settings: { bindings: [{ service: authorityName }] } };
    },
  ],
  [
    "unknown recovery binding",
    (f) => {
      f.workers[authorityName].settings.bindings.find(
        (b) => b.name === "CRABBOX_AWS_QUALIFICATION_RECOVERY_JSON",
      ).text = "{}";
    },
  ],
  [
    "absent controller",
    (f) => {
      delete f.workers[controllerName];
    },
  ],
])
  test(`cleanup issuance rejects ${title} before STS or secret mutation`, async (t) => {
    const f = cleanupFixture(t),
      o = f.opts("deliver");
    change(f, o);
    f.state = initialRecoveryState(f.plan);
    await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, o)));
    assert.equal(f.commands.length, 0);
    assert.ok(!f.events.some((e) => /^(PATCH|PUT|DELETE|POST) /.test(e)));
  });

test("cleanup scope binds every protected handoff field", (t) => {
  const f = cleanupFixture(t),
    handoff = { ...f.plan.cleanupSession.scope, mode: "first-walk" };
  assert.ok(validateCleanupSession(f.plan, handoff));
  for (const key of [
    "repository",
    "workflowSha",
    "candidateArtifactId",
    "candidateArtifactDigest",
    "manifestSha256",
    "startsAt",
    "workExpiresAt",
    "expiresAt",
  ])
    assert.throws(() => validateCleanupSession(f.plan, { ...handoff, [key]: "foreign" }));
});

test("cleanup session profiles cannot change the original tagging contract", (t) => {
  for (const mode of ["first-walk", "key-tag-authorization"]) {
    const f = cleanupFixture(t);
    if (mode === "key-tag-authorization") f.plan.binding.keyAuthorizationOnly = true;
    f.plan.cleanupSession.scope.profile = `${mode}-aws-session/v2`;
    assert.ok(validateCleanupSession(f.plan));
    delete f.plan.binding.taggingContract;
    assert.throws(() => validateCleanupSession(f.plan));
    f.plan.cleanupSession.scope.profile = `${mode}-aws-session/v1`;
    assert.ok(validateCleanupSession(f.plan));
    f.plan.binding.taggingContract = "creation-only/v1";
    assert.throws(() => validateCleanupSession(f.plan));
    f.plan.binding.taggingContract = "unknown/v1";
    assert.throws(() => validateCleanupSession(f.plan));
  }
});

for (const drift of ["custody", "settings", "module", "caller", "marker"])
  test(`lost delivery with ${drift} drift refuses version adoption and finalization`, async (t) => {
    const f = cleanupFixture(t);
    f.loseSession();
    await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.opts("deliver"))));
    const o = f.opts("inspect");
    if (drift === "custody")
      o.sessionOptions.readCustody = async () => ({ ...f.pre, noOtherSessionConsumers: false });
    if (drift === "settings") f.workers[authorityName].settings.annotations.extra = "changed";
    if (drift === "module")
      f.workers[authorityName].module = {
        ...f.workers[authorityName].module,
        sha256: "0".repeat(64),
      };
    if (drift === "caller")
      f.workers[controllerName].version = "55555555-5555-4555-8555-555555555555";
    if (drift === "marker") fs.unlinkSync(path.join(f.dir, "cleanup-session-issuance.json"));
    await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, o)));
    assert.equal(f.state.cleanupSession.phase, "delivery-intent");
    assert.ok(!f.events.includes("RPC /finalize"));
    assert.equal(f.commands.length, 2);
  });

test("uncertain STS issuance, cancellation and no-change delivery stay closed with a durable intent", async (t) => {
  for (const failure of ["issuance", "cancel", "no-change"]) {
    const f = cleanupFixture(t),
      o = f.opts("deliver"),
      controller = new AbortController();
    const command = o.sessionOptions.command;
    o.signal = controller.signal;
    o.sessionOptions.command = async (...args) => {
      if (failure === "issuance") throw Error("synthetic issuance response lost");
      const result = await command(...args);
      if (failure === "cancel") controller.abort();
      return result;
    };
    if (failure === "no-change") {
      const request = o.authorityCF.request.bind(o.authorityCF);
      o.authorityCF.request = (route, init, options) => {
        if (init?.method === "PATCH") throw Error("synthetic request not applied");
        return request(route, init, options);
      };
    }
    await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, o)));
    assert.ok(fs.existsSync(path.join(f.dir, "cleanup-session-issuance.json")));
    await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.opts("deliver"))));
    await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.opts("inspect"))));
    await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
    assert.equal(f.state.settled, false);
  }
});

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

test("already-revoked relay settles without inventing a missing version", async () => {
  const f = fixture(),
    name = f.plan.binding.candidateWorker.replace("qualification-", "qualification-relay-");
  delete f.workers[name];
  f.plan.callers = f.plan.callers.map((c) => (c.name === name ? { name, absent: true } : c));
  f.state = initialRecoveryState(f.plan);
  await local(f, () => recoverTransition(f.plan, f.state, f.options()));
  assert.equal(f.state.settled, true);
  assert.ok(!f.events.some((event) => event.startsWith("PUT /workers/scripts/" + name)));
});

test("an absent caller cannot reappear or replace the required controller", async () => {
  const f = fixture(),
    name = f.plan.binding.candidateWorker.replace("qualification-", "qualification-relay-");
  f.plan.callers = f.plan.callers.map((c) => (c.name === name ? { name, absent: true } : c));
  f.state = initialRecoveryState(f.plan);
  await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
  assert.ok(!f.events.some((event) => /^(PUT|DELETE|RPC)/.test(event)));
  const g = fixture();
  g.plan.callers = g.plan.callers.map((c) =>
    c.name === controllerName ? { name: controllerName, absent: true } : c,
  );
  assert.throws(() => validateRecoveryPlan(g.plan));
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
  assert.equal(
    f.events.filter((e) => e === `PUT /workers/scripts/${f.plan.binding.candidateWorker}`).length,
    1,
  );
});

for (const [label, change] of [
  ["added", (bindings) => bindings.push({ name: "UNEXPECTED_SECRET", type: "secret_text" })],
  [
    "removed",
    (bindings) =>
      bindings.splice(
        bindings.findIndex((b) => b.type === "secret_text"),
        1,
      ),
  ],
  [
    "changed",
    (bindings) => {
      bindings.find((b) => b.type === "secret_text").type = "plain_text";
    },
  ],
])
  test(`${label} secret metadata after an uncertain upload blocks all further mutations`, async () => {
    const f = fixture();
    f.loseUpload();
    await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
    const name = f.plan.binding.candidateWorker;
    assert.deepEqual(
      f.state.uploadIntents[name].bindings.filter((b) => b.type === "secret_text"),
      [
        { name: "CRABBOX_ADMIN_TOKEN", type: "secret_text" },
        { name: "CRABBOX_SHARED_TOKEN", type: "secret_text" },
      ],
    );
    change(f.workers[name].settings.bindings);
    const count = f.events.length;
    await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
    assert.ok(f.events.slice(count).every((e) => e.startsWith("GET ")));
    assert.ok(f.workers[name]);
    assert.equal(f.state.settled, false);
  });

test("ordinary deletion dependency failure preserves custody and resumes without force", async () => {
  const f = fixture();
  f.blockCandidateDelete(true);
  await local(f, () => assert.rejects(recoverTransition(f.plan, f.state, f.options())));
  assert.ok(f.workers[f.plan.binding.candidateWorker]);
  assert.equal(f.state.settled, false);
  assert.ok(!f.events.some((e) => e.includes("force")));
  f.blockCandidateDelete(false);
  await local(f, () => recoverTransition(f.plan, f.state, f.options()));
  assert.equal(f.state.settled, true);
  assert.equal(
    f.events.filter((e) => e === `PUT /workers/scripts/${f.plan.binding.candidateWorker}`).length,
    1,
  );
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

test("fresh finalization gets five minutes while the original deadline still clips it", async (t) => {
  const f = fixture(),
    limits = [],
    timeout = AbortSignal.timeout.bind(AbortSignal);
  t.mock.method(AbortSignal, "timeout", (ms) => {
    limits.push(ms);
    return timeout(ms);
  });
  await local(f, () => recoverTransition(f.plan, f.state, f.options()));
  assert.ok(limits.includes(300_000));
  assert.ok(limits.includes(30_000));
  const g = fixture();
  limits.length = 0;
  await local(g, () =>
    recoverTransition(g.plan, g.state, {
      ...g.options(),
      now: () => Date.parse(g.plan.deadline) - 45_000,
    }),
  );
  assert.ok(!limits.includes(300_000));
  assert.ok(limits.includes(45_000));
});

for (const keep of [["service"], ["secret_text", "service"]])
  test(`upload rejects unsupported retention ${keep.join("+")} before network`, async () => {
    const f = fixture();
    await assert.rejects(
      f.options().callerCF.upload(f.plan.binding.candidateWorker, "/unused", {
        keep_bindings: keep,
        bindings: [],
      }),
      /retain only verified secret bindings/,
    );
    assert.equal(f.events.length, 0);
  });

test("upload cannot replace a verified retained secret name", async () => {
  const f = fixture(),
    cf = f.options().callerCF;
  cf.admission = fixtureAdmission().admission;
  await assert.rejects(
    cf.upload(f.plan.binding.candidateWorker, "/unused", {
      keep_bindings: ["secret_text"],
      bindings: [{ name: "CRABBOX_ADMIN_TOKEN", type: "plain_text", text: "replacement" }],
    }),
    /cannot replace a retained secret/,
  );
  assert.ok(f.events.every((e) => e.startsWith("GET ")));
});

function recoveryCommandFixture(t, cleanup) {
  const directory = fs.mkdtempSync(
    path.join(fs.realpathSync(os.tmpdir()), "qualification-recovery-command-"),
  );
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const checkout = path.join(directory, "checkout"),
    journal = path.join(directory, "journal");
  fs.mkdirSync(checkout);
  fs.mkdirSync(journal, { mode: 0o700 });
  // A real clean Git checkout preserves the command's source/dirty-tree gates.
  // Only public test/source scripts are copied; no credentials or cloud calls.
  const tracked = execFileSync("git", ["ls-files", "-z", "--", "scripts"], {
    cwd: path.dirname(import.meta.dirname),
    encoding: "utf8",
  })
    .split("\0")
    .filter(Boolean);
  for (const name of tracked) {
    const from = path.join(path.dirname(import.meta.dirname), name),
      to = path.join(checkout, name);
    assert.ok(fs.lstatSync(from).isFile(), "fixture source must be a regular tracked file");
    fs.mkdirSync(path.dirname(to), { recursive: true });
    fs.copyFileSync(from, to);
  }
  const git = (...args) =>
    execFileSync("git", args, {
      cwd: checkout,
      encoding: "utf8",
      env: { PATH: process.env.PATH, HOME: directory, GIT_CONFIG_NOSYSTEM: "1" },
      stdio: ["ignore", "pipe", "pipe"],
    }).trim();
  git("init");
  git("add", "scripts");
  git(
    "-c",
    "user.name=Qualification Fixture",
    "-c",
    "user.email=fixture@example.invalid",
    "-c",
    "commit.gpgsign=false",
    "commit",
    "-m",
    "fixture",
  );
  const sourceSha = git("rev-parse", "HEAD"),
    f = cleanup ? cleanupFixture(t, { sourceSha }) : fixture({ sourceSha });
  if (cleanup) {
    const aws = path.join(directory, "aws");
    fs.writeFileSync(
      aws,
      `#!${process.execPath}\nconst argv = process.argv;\nconsole.log(JSON.stringify(argv.includes('assume-role') ? {Credentials:{AccessKeyId:'synthetic-new-id',SecretAccessKey:'synthetic-new-secret',SessionToken:'synthetic-new-session',Expiration:${JSON.stringify(new Date(Date.parse(f.plan.handoffStartedAt) + 840 * 60_000).toISOString())}}} : {Account:${JSON.stringify(f.plan.correctedAdmission.aws.accountId)},Arn:${JSON.stringify(`arn:aws:sts::${f.plan.correctedAdmission.aws.accountId}:assumed-role/Qualification/${f.plan.binding.runId}`)}}));\n`,
      { mode: 0o700 },
    );
    f.plan.cleanupSession.directory = journal;
    f.plan.cleanupSession.aws = { path: aws, sha256: digest(fs.readFileSync(aws)) };
  }
  const write = (name, value) => {
    const bytes = `${JSON.stringify(value)}\n`;
    fs.writeFileSync(path.join(journal, name), bytes, { mode: 0o600 });
    return digest(bytes);
  };
  f.plan.originalAttestationSha256 = write("original-attestation.json", {
    version: 1,
    ...f.plan.binding,
    finalizingAt: f.plan.binding.enrolledAt,
    finalized: false,
  });
  f.plan.originalHandoffSha256 = write("original-handoff.json", {
    ...(cleanup ? f.plan.cleanupSession.scope : {}),
    ...f.plan.binding,
    mode: "first-walk",
    startsAt: f.plan.handoffStartedAt,
  });
  if (cleanup) {
    write("pre-cleanup-custody.json", f.pre);
    write("post-cleanup-custody.json", {
      ...f.pre,
      authorityVersion: "44444444-4444-4444-8444-444444444444",
    });
  }
  write("recovery-plan.json", f.plan);
  // Explicit serialized v1 state, not a live object shared with the command.
  write("recovery-state.json", {
    profile: "active-cleanup-transition/v1",
    planSha256: digest(canonical(f.plan)),
    sequence: 0,
    requests: 0,
    uploadIntents: {},
    attestation: null,
    retired: false,
    settled: false,
  });
  const remote = path.join(directory, "provider.json"),
    preload = path.join(directory, "provider.mjs"),
    moduleURL = (name) => pathToFileURL(path.join(checkout, "scripts", name)).href;
  fs.writeFileSync(
    remote,
    JSON.stringify({
      sourceSha,
      ...f.remoteState(),
      events: [],
      loseUpload: !cleanup,
      loseSession: cleanup,
    }),
  );
  // Reuse the same strict synthetic management/RPC fixture at the provider boundary.
  // This exercises the real CLI, receipt readers, locks and durable journal;
  // it is not live Cloudflare/AWS behavior proof or an authority key-deletion test.
  fs.writeFileSync(
    preload,
    `
import assert from "node:assert/strict";
import fs from "node:fs";
import { Cloudflare, recoverActiveQualification } from ${JSON.stringify(moduleURL("image-qualification-control.mjs"))};
import { authorityName, controllerName, bindingShape, canonical, digest } from ${JSON.stringify(moduleURL("image-qualification-isolation.mjs"))};
import { fixtureAdmission, ownedSettings, controllerContent, version } from ${JSON.stringify(moduleURL("fixtures/qualification-isolation.mjs"))};
${fixture.toString()}
const file = process.env.QUALIFICATION_TEST_PROVIDER;
const input = JSON.parse(fs.readFileSync(file));
const f = fixture({...input, state: {}, controllerSource: new URL(${JSON.stringify(moduleURL("image-qualification-controller-worker.mjs"))})});
for (const key of Object.keys(f.workers)) delete f.workers[key];
Object.assign(f.workers, input.workers);
if (input.loseUpload) f.loseUpload();
if (input.loseSession) f.loseSession();
const provider = f.options().callerCF;
Cloudflare.prototype.request = (...args) => provider.request(...args);
Cloudflare.prototype.list = function (route) { return this.request(route); };
globalThis.fetch = f.fetch;
Date.now = f.options().now;
process.on("exit", () => fs.writeFileSync(file, JSON.stringify({ sourceSha: input.sourceSha, ...f.remoteState(), events: [...input.events, ...f.events], loseUpload: false })));
`,
  );
  const command = path.join(checkout, "scripts/image-qualification-recovery.mjs");
  const run = (overrides = {}, mode) =>
    spawnSync(process.execPath, ["--import", preload, command, journal, ...(mode ? [mode] : [])], {
      cwd: directory,
      encoding: "utf8",
      timeout: 15_000,
      env: {
        PATH: process.env.PATH,
        HOME: directory,
        GIT_CONFIG_NOSYSTEM: "1",
        CLOUDFLARE_ACCOUNT_ID: f.plan.correctedAdmission.accountId,
        CLOUDFLARE_API_TOKEN: "synthetic-cloudflare-token",
        QUALIFICATION_CONTROLLER_TOKEN: "x".repeat(64),
        QUALIFICATION_RECOVERY_PLAN_SHA256: digest(canonical(f.plan)),
        QUALIFICATION_TEST_PROVIDER: remote,
        ...overrides,
      },
    });
  const dirty = path.join(checkout, "untracked-change");
  fs.writeFileSync(dirty, "refuse dirty source\n");
  const dirtyResult = run();
  assert.equal(dirtyResult.error, undefined);
  assert.equal(dirtyResult.status, 1);
  assert.deepEqual(JSON.parse(fs.readFileSync(remote)).events, []);
  fs.unlinkSync(dirty);
  const refused = run({ QUALIFICATION_RECOVERY_PLAN_SHA256: "0".repeat(64) });
  assert.equal(refused.error, undefined);
  assert.equal(refused.status, 1);
  assert.deepEqual(JSON.parse(fs.readFileSync(remote)).events, []);
  // The rejected preflight must not consume the later lost-response discriminator.
  const before = JSON.parse(fs.readFileSync(remote));
  before.loseUpload = !cleanup;
  before.loseSession = cleanup;
  fs.writeFileSync(remote, JSON.stringify(before));
  const first = run(
    cleanup
      ? {
          AWS_ACCESS_KEY_ID: "synthetic-bootstrap",
          AWS_SECRET_ACCESS_KEY: "synthetic-bootstrap-secret",
        }
      : {},
    cleanup ? "deliver-session" : undefined,
  );
  assert.equal(first.error, undefined);
  assert.equal(first.status, 1, first.stderr);
  const interrupted = readPrivateJSON(path.join(journal, "recovery-state.json"));
  assert.equal(interrupted.settled, false);
  if (cleanup) {
    assert.equal(interrupted.cleanupSession.phase, "delivery-intent", first.stderr);
    const inspect = run({}, "inspect-session");
    assert.equal(inspect.status, 0, inspect.stderr);
    assert.equal(
      readPrivateJSON(path.join(journal, "recovery-state.json")).cleanupSession.phase,
      "reconciled",
    );
  } else {
    assert.ok(
      interrupted.uploadIntents[f.plan.binding.candidateWorker],
      JSON.stringify({
        events: JSON.parse(fs.readFileSync(remote)).events,
        stderr: first.stderr,
        state: interrupted,
      }),
    );
    assert.deepEqual(
      interrupted.uploadIntents[f.plan.binding.candidateWorker].bindings.filter(
        (b) => b.type === "secret_text",
      ),
      [
        { name: "CRABBOX_ADMIN_TOKEN", type: "secret_text" },
        { name: "CRABBOX_SHARED_TOKEN", type: "secret_text" },
      ],
    );
  }
  const second = run();
  assert.equal(second.error, undefined);
  assert.equal(second.status, 0, second.stderr);
  assert.match(second.stdout, /Original qualification owner settled/);
  const settled = readPrivateJSON(path.join(journal, "recovery-state.json")),
    provider = JSON.parse(fs.readFileSync(remote));
  assert.equal(settled.settled, true);
  assert.equal(settled.retired, true);
  assert.ok(settled.requests > interrupted.requests);
  assert.deepEqual(settled.attestation.runId, f.plan.binding.runId);
  assert.equal(settled.attestation.authoritySha, f.plan.originalAdmission.authority.sourceSha);
  assert.deepEqual(settled.attestation.finalReceipt.finalCounts, {
    images: 0,
    instances: 0,
    keyPairs: 0,
    snapshots: 0,
    volumes: 0,
  });
  assert.deepEqual(Object.keys(provider.workers), [authorityName]);
  assert.equal(provider.active, false);
  assert.equal(
    provider.events.filter((e) => e === `PUT /workers/scripts/${f.plan.binding.candidateWorker}`)
      .length,
    1,
  );
  assert.ok(!provider.events.some((e) => /force|enroll|transport-open/.test(e)));
  assert.ok(!fs.existsSync(path.join(journal, "recovery.lock")));
  for (const result of [dirtyResult, refused, first, second])
    assert.doesNotMatch(result.stdout + result.stderr, /synthetic-cloudflare-token|x{64}/);
}

for (const cleanup of [false, true])
  test(`recovery command reloads original receipts and settles ${cleanup ? "lost cleanup-session delivery" : "lost secret-preserving upload"} across processes`, (t) =>
    recoveryCommandFixture(t, cleanup));

test("recovery preserves immutable key-only purpose and rejects a false marker", () => {
  const a = { runId: "image-qualification-42-1", keyAuthorizationOnly: true };
  assert.equal(recoveryBinding(a).keyAuthorizationOnly, true);
  assert.equal(
    Object.hasOwn(
      recoveryBinding({ ...a, keyAuthorizationOnly: undefined }),
      "keyAuthorizationOnly",
    ),
    false,
  );
  const f = fixture();
  f.plan.binding.keyAuthorizationOnly = false;
  assert.throws(() => validateRecoveryPlan(f.plan));
});
