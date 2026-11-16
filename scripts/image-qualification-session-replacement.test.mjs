/* eslint-disable no-await-in-loop -- Failure/recovery sequences and process settlement deliberately run in order. */
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import { idleFixture } from "./fixtures/qualification-idle-controller.mjs";
import { runSessionTags, runSessionPolicy } from "./image-qualification-run-session.mjs";
import { fixtureAdmission, version } from "./fixtures/qualification-isolation.mjs";
import { Cloudflare } from "./image-qualification-control.mjs";
import {
  authorityName,
  canonical,
  digest,
  bindingShape,
} from "./image-qualification-isolation.mjs";
import { readPrivateJSON, withQualificationJournal } from "./image-qualification-journal.mjs";
import {
  replacementSources,
  replacementInitialState,
  replaceIdleSession,
  validateReplacementPlan,
  verifyReplacementIdle,
  claimSessionIssuance,
  sessionChild,
  replacementRecoverySources,
} from "./image-qualification-session-replacement.mjs";

const newVersion = "33333333-3333-4333-8333-333333333333";
const hashState = ({ integrity: _integrity, ...value }) => digest(canonical(value));
const sealed = (value) => ({ ...value, integrity: hashState(value) });

function fixture(t, { secretProvenance = false } = {}) {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "qualification-session-")));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const start = Date.now();
  const f = fixtureAdmission();
  f.bindings.push(
    { name: "CF_VERSION_METADATA", type: "version_metadata" },
    ...["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"].map((name) => ({
      name,
      type: "secret_text",
    })),
  );
  f.record.authority.bindingDigest = digest(canonical(bindingShape(f.bindings)));
  const settings = {
    bindings: f.bindings,
    ...(secretProvenance ? { annotations: { "workers/triggered_by": "version_upload" } } : {}),
  };
  const model = {
    now: start + 1000,
    version,
    calls: [],
    commands: [],
    saves: [],
    tokens: "active",
  };
  const tools = {};
  for (const name of ["aws", "node"]) {
    const file = path.join(dir, name + ".json");
    const bytes = `fake-${name}`;
    fs.writeFileSync(file, bytes, { mode: 0o600 });
    tools[name] = { path: file, sha256: digest(bytes) };
  }
  const plan = {
    profile: "idle-session-replacement/v1",
    attemptId: "a".repeat(24),
    postIdleAttemptId: "b".repeat(24),
    sourceSha: "c".repeat(40),
    tokenId: "d".repeat(32),
    sources: replacementSources(),
    recoveryDirectory: dir,
    isolation: f.record,
    authoritySettingsSha256: digest(canonical(settings)),
    authorityModule: { module: "authority.js", count: 1, sha256: "e".repeat(64) },
    startsAt: new Date(start).toISOString(),
    deliveryStopsAt: new Date(start + 20 * 60_000).toISOString(),
    recoveryStopsAt: new Date(start + 60 * 60_000).toISOString(),
    cycleRecoveryBy: new Date(start + 6 * 3600_000).toISOString(),
    authorityRetentionEndsAt: new Date(start + 24 * 3600_000).toISOString(),
    previousSessionExpiresAt: new Date(start - 3600_000).toISOString(),
    tools,
  };
  plan.runSession = {
    profile: "first-walk-aws-session/v1",
    repository: "example/repo",
    workflowSha: "1".repeat(40),
    candidateSha: "2".repeat(40),
    runId: "image-qualification-42-1",
    candidateArtifactId: "9",
    candidateArtifactDigest: `sha256:${"3".repeat(64)}`,
    manifestSha256: "4".repeat(64),
    startsAt: new Date(start).toISOString(),
    workExpiresAt: new Date(start + 130 * 60_000).toISOString(),
    expiresAt: new Date(start + 150 * 60_000).toISOString(),
    recoveryExpiresAt: new Date(start + 270 * 60_000).toISOString(),
  };
  plan.cycleRecoveryBy = plan.runSession.recoveryExpiresAt;
  const makeCustody = (authorityVersion) => ({
    profile: "idle-session-custody/v1",
    attemptId: plan.attemptId,
    sourceSha: plan.sourceSha,
    authorityVersion,
    exclusiveUntil: plan.recoveryStopsAt,
    observedAt: new Date(model.now).toISOString(),
    continuousExclusiveCustody: true,
    noConcurrentWritersOrCallers: true,
    noActiveWorkflowWriters: true,
    previousRunsSettled: true,
    fallbackReady: true,
    ...(plan.allowUnexpiredSessionReplacement === true ? { previousSessionUseStopped: true } : {}),
  });
  const preCustody = makeCustody(version);
  plan.preCustodySha256 = digest(canonical(preCustody));
  const makeProof = (isolation, after = false) => {
    const ownerPlan = {
      ...idleFixture().plan,
      sourceSha: plan.sourceSha,
      tokenId: plan.tokenId,
      attemptId: after ? plan.postIdleAttemptId : "f".repeat(24),
      isolation,
      authorityModule: plan.authorityModule,
      authoritySettingsSha256: plan.authoritySettingsSha256,
      startsAt: new Date(model.now - 1000).toISOString(),
      forwardStopsAt: new Date(model.now - 1000 + 600000).toISOString(),
      deadline: new Date(model.now - 1000 + 1200000).toISOString(),
    };
    const state = sealed({
      profile: ownerPlan.profile,
      planHash: digest(canonical(ownerPlan)),
      sequence: 10,
      phase: "settled",
      settled: true,
      failed: false,
      discovery: "idle",
      identityDigest: "f".repeat(64),
      events: [
        { phase: "discovery-complete", at: new Date(model.now).toISOString() },
        { phase: "settled", at: new Date(model.now).toISOString() },
      ],
    });
    return { ownerPlan, state };
  };
  const pre = makeProof(plan.isolation);
  let post;
  class FakeCloudflare extends Cloudflare {
    constructor() {
      super(plan.isolation.accountId, "fake-cf-token");
    }
    async request(route, init = {}, options = {}) {
      model.calls.push({ route, method: init.method ?? "GET" });
      if (init.method === "PATCH") {
        assert.equal(route, `/workers/scripts/${authorityName}/secrets-bulk`);
        assert.equal(model.saves.at(-1).phase, "delivery-intent");
        assert.equal(init.redirect, "error");
        assert.ok(!init.signal.aborted);
        model.delivery = JSON.parse(init.body);
        model.version = newVersion;
        if (secretProvenance) settings.annotations["workers/triggered_by"] = "secret";
        if (model.lostDelivery) throw new Error("fake-secret-in-raw-error");
        return { success: true };
      }
      assert.equal(init.method ?? "GET", "GET");
      assert.equal(init.redirect, "error");
      if (init.signal.aborted) throw new Error("aborted");
      await model.before?.(route);
      const pathname = new URL(route, "https://fixture.invalid").pathname;
      const list = (result) =>
        options.envelope ? { result, result_info: { total_pages: 1 } } : result;
      if (pathname === "/tokens/verify")
        return {
          id: plan.tokenId,
          status: model.tokens,
          expires_on: new Date(start + 2 * 86400000).toISOString(),
        };
      if (pathname === "/workers/scripts")
        return list([{ id: authorityName }, ...(model.client ? [{ id: "foreign-client" }] : [])]);
      if (pathname === "/workers/durable_objects/namespaces")
        return list(plan.isolation.authority.namespaces);
      if (pathname === "/workers/domains") return list([]);
      if (pathname.endsWith("/routes")) return [];
      if (pathname.endsWith("/schedules")) return { schedules: [] };
      if (pathname.endsWith("/subdomain"))
        return { enabled: false, previews_enabled: false, ...model.exposure };
      if (pathname.endsWith("/content/v2")) return model.module ?? plan.authorityModule;
      if (pathname.endsWith("/deployments"))
        return { deployments: [{ versions: [{ version_id: model.version, percentage: 100 }] }] };
      if (pathname.endsWith("/settings"))
        return pathname.includes("foreign-client")
          ? { bindings: [{ type: "service", service: authorityName }] }
          : settings;
      if (pathname === `/workers/services/${authorityName}`)
        return { default_environment: { environment: "production" } };
      assert.fail(route);
    }
  }
  let state = replacementInitialState(plan);
  const context = {
    cf: new FakeCloudflare(),
    now: () => model.now,
    workdir: dir,
    env: {
      AWS_ACCESS_KEY_ID: "fake-bootstrap-id",
      AWS_SECRET_ACCESS_KEY: "fake-bootstrap-secret",
      CLOUDFLARE_API_TOKEN: "fake-cf-token",
    },
    readCustody: async (stage) =>
      structuredClone(stage === "pre" ? preCustody : makeCustody(newVersion)),
    readProof: async (stage) => structuredClone(stage === "pre" ? pre : post),
    save: async (value) => {
      await model.save?.(value);
      model.saves.push(structuredClone(value));
    },
    command: async (argv, env, input, options) => {
      model.commands.push({ argv, env, input });
      assert.equal(options.cwd, dir);
      await model.command?.(argv, env, input);
      if (argv.includes("assume-role")) {
        assert.deepEqual(
          JSON.parse(argv[argv.indexOf("--tags") + 1]),
          Object.entries(runSessionTags(plan.runSession)).map(([Key, Value]) => ({ Key, Value })),
        );
        assert.deepEqual(
          JSON.parse(argv[argv.indexOf("--policy") + 1]),
          runSessionPolicy(plan.runSession),
        );
        assert.equal(argv[argv.indexOf("--role-session-name") + 1], plan.runSession.runId);
        assert.equal(model.saves.at(-1).phase, "issuance-intent");
        assert.ok(fs.existsSync(path.join(dir, "session-issuance-intent.json")));
        assert.equal(env.AWS_SESSION_TOKEN, undefined);
        assert.equal(env.AWS_MAX_ATTEMPTS, "1");
        return JSON.stringify({
          Credentials: {
            AccessKeyId: "fake-new-id",
            SecretAccessKey: "fake-new-secret",
            SessionToken: "fake-new-token",
            Expiration: new Date(start + 12 * 3600_000).toISOString(),
          },
          ...model.assumed,
        });
      }
      if (argv.includes("get-caller-identity"))
        return JSON.stringify({
          Account: f.record.aws.accountId,
          Arn: `arn:aws:sts::${f.record.aws.accountId}:assumed-role/Qualification/${plan.runSession.runId}`,
          ...model.identity,
        });
      if (argv.includes("idle-controller-recover")) {
        assert.equal(env.AWS_ACCESS_KEY_ID, undefined);
        model.now += 1000;
        post.state = sealed({
          ...post.state,
          sequence: post.state.sequence + 1,
          events: [
            ...post.state.events,
            { phase: "settled", at: new Date(model.now).toISOString() },
          ],
        });
        return "";
      }
      assert.fail("unexpected command");
    },
  };
  const reset = () => {
    state = replacementInitialState(plan);
  };
  const authorizeEarlyReplacement = () => {
    plan.previousSessionExpiresAt = new Date(start + 5 * 3600_000).toISOString();
    plan.allowUnexpiredSessionReplacement = true;
    preCustody.previousSessionUseStopped = true;
    plan.preCustodySha256 = digest(canonical(preCustody));
    reset();
  };
  const run = (mode) => replaceIdleSession(plan, state, { ...context, mode });
  const preparePost = () => {
    model.now += 1000;
    const isolation = structuredClone(plan.isolation);
    isolation.authority.version = newVersion;
    post = makeProof(isolation, true);
    post.ownerPlan.authoritySettingsSha256 = digest(canonical(settings));
    post.state = sealed({
      ...post.state,
      planHash: digest(canonical(post.ownerPlan)),
    });
    return post;
  };
  const prepareRecovery = () => {
    plan.sources["image-qualification-session-replacement.mjs"] =
      "cecdf9b2a5e7a70fb7c2ff6e6dc6598db85b17900635fff5f52eb1d69ccdb994";
    context.recovery = {
      sourceSha: "9".repeat(40),
      originalSourceSha: plan.sourceSha,
      originalRoot: dir,
      sources: structuredClone(plan.sources),
    };
    state = sealed({
      ...replacementInitialState(plan),
      phase: "delivery-confirmed",
      sequence: 3,
      beforeVersion: version,
      sessionExpiresAt: new Date(start + 12 * 3600_000).toISOString(),
      deliveryConfirmedAt: new Date(model.now).toISOString(),
    });
    claimSessionIssuance(plan);
    model.version = newVersion;
    if (secretProvenance) settings.annotations["workers/triggered_by"] = "secret";
  };
  return {
    plan,
    model,
    context,
    pre,
    preCustody,
    settings,
    dir,
    start,
    run,
    reset,
    authorizeEarlyReplacement,
    preparePost,
    prepareRecovery,
    get state() {
      return state;
    },
    get post() {
      return post;
    },
  };
}

test("exact predecessor recovery preserves original plan/intent/clock and uses its idle owner", async (t) => {
  const f = fixture(t, { secretProvenance: true });
  f.prepareRecovery();
  const before = canonical(f.plan);
  const intent = fs.readFileSync(path.join(f.dir, "session-issuance-intent.json"), "utf8");
  await f.run("inspect");
  assert.equal(f.state.recoverySourceSha, f.context.recovery.sourceSha);
  assert.equal(f.state.phase, "reseal-candidate");
  f.preparePost();
  await f.run("reseal");
  assert.equal(f.state.phase, "resealed");
  assert.equal(f.state.enrollmentAdmitted, false);
  assert.equal(canonical(f.plan), before);
  assert.equal(fs.readFileSync(path.join(f.dir, "session-issuance-intent.json"), "utf8"), intent);
  assert.equal(f.state.planHash, digest(before));
  assert.equal(f.model.calls.filter((c) => c.method !== "GET").length, 0);
  assert.equal(f.model.commands.length, 1);
  assert.equal(
    f.model.commands[0].argv[1],
    path.join(f.dir, "scripts/image-qualification-control.mjs"),
  );
  assert.equal(f.model.commands[0].env.QUALIFICATION_IDLE_SOURCE_SHA, f.plan.sourceSha);
});

for (const [label, mutate] of Object.entries({
  unknownPredecessor: (f) => {
    f.plan.sources["image-qualification-session-replacement.mjs"] = "0".repeat(64);
    f.context.recovery.sources = structuredClone(f.plan.sources);
  },
  changedOwner: (f) => {
    f.context.recovery.sources["image-qualification-idle-controller.mjs"] = "0".repeat(64);
  },
  changedPlanDependency: (f) => {
    f.plan.sources["image-qualification-control.mjs"] = "0".repeat(64);
  },
  unknownOriginal: (f) => {
    f.context.recovery.originalSourceSha = "0".repeat(40);
  },
  sameSource: (f) => {
    f.context.recovery.sourceSha = f.plan.sourceSha;
  },
  relativeOwner: (f) => {
    f.context.recovery.originalRoot = "relative";
  },
})) {
  test(`predecessor reconciliation rejects ${label} before effects`, async (t) => {
    const f = fixture(t, { secretProvenance: true });
    f.prepareRecovery();
    mutate(f);
    assert.throws(() => replacementRecoverySources(f.plan, f.context.recovery));
    await assert.rejects(f.run("inspect"));
    assert.equal(f.model.calls.length, 0);
    assert.equal(f.model.commands.length, 0);
  });
}

for (const phase of ["prepared", "issuance-intent", "delivery-intent"]) {
  test(`predecessor reconciliation cannot promote ${phase} or issue a session`, async (t) => {
    const f = fixture(t, { secretProvenance: true });
    f.prepareRecovery();
    await assert.rejects(f.run("deliver"));
    Object.assign(f.state, sealed({ ...f.state, phase }));
    await assert.rejects(f.run("inspect"));
    assert.equal(f.model.calls.length, 0);
    assert.equal(f.model.commands.length, 0);
  });
}

test("predecessor reconciliation binds the recovery executor and preserves deadline refusal", async (t) => {
  const f = fixture(t, { secretProvenance: true });
  f.prepareRecovery();
  await f.run("inspect");
  f.context.recovery.sourceSha = "8".repeat(40);
  await assert.rejects(f.run("inspect"));
  f.context.recovery.sourceSha = "9".repeat(40);
  f.model.now = Date.parse(f.plan.recoveryStopsAt);
  await assert.rejects(f.run("inspect"));
  assert.equal(f.model.commands.length, 0);
  assert.equal(f.model.calls.filter((c) => c.method !== "GET").length, 0);
});

test("confirmed secret provenance transition pins all raw post settings through recovery", async (t) => {
  const f = fixture(t, { secretProvenance: true });
  await f.run("deliver");
  assert.equal(f.settings.annotations["workers/triggered_by"], "secret");
  const after = digest(canonical(f.settings));
  assert.notEqual(after, f.plan.authoritySettingsSha256);
  await f.run("inspect");
  assert.equal(f.state.afterSettingsSha256, after);
  assert.equal(f.plan.authoritySettingsSha256, f.pre.ownerPlan.authoritySettingsSha256);
  f.preparePost();
  await f.run("reseal");
  assert.equal(f.state.phase, "resealed");
  assert.equal(f.state.afterSettingsSha256, after);
  assert.equal(f.state.enrollmentAdmitted, false);
  assert.equal(f.model.commands.filter((c) => c.argv.includes("assume-role")).length, 1);
  assert.equal(f.model.calls.filter((c) => c.method === "PATCH").length, 1);
  await assert.rejects(f.run("deliver"));
});

for (const [label, mutate] of Object.entries({
  unexpectedProvenance: (f) => {
    f.settings.annotations["workers/triggered_by"] = "upload";
  },
  anotherAnnotation: (f) => {
    f.settings.annotations["workers/message"] = "changed";
  },
  removedAnnotations: (f) => {
    delete f.settings.annotations;
  },
  logpush: (f) => {
    f.settings.logpush = true;
  },
  compatibility: (f) => {
    f.settings.compatibility_date = "2099-01-01";
  },
  secretShape: (f) => {
    f.settings.bindings.find((b) => b.type === "secret_text").text = "fake-value";
  },
  module: (f) => {
    f.model.module = { ...f.plan.authorityModule, sha256: "0".repeat(64) };
  },
  incomingClient: (f) => {
    f.model.client = true;
  },
  publicExposure: (f) => {
    f.model.exposure = { enabled: true };
  },
})) {
  test(`secret provenance exception still refuses ${label} without replay`, async (t) => {
    const f = fixture(t, { secretProvenance: true });
    await f.run("deliver");
    mutate(f);
    await assert.rejects(f.run("inspect"));
    assert.equal(f.state.phase, "delivery-confirmed");
    assert.equal(f.model.calls.filter((c) => c.method === "PATCH").length, 1);
  });
}

test("provenance exception is unavailable before delivery", async (t) => {
  const f = fixture(t, { secretProvenance: true });
  f.settings.annotations["workers/triggered_by"] = "secret";
  await assert.rejects(f.run("deliver"));
  assert.equal(f.model.commands.length, 0);
  assert.equal(f.model.calls.filter((c) => c.method === "PATCH").length, 0);
});

test("post idle evidence must bind full raw post settings, not the pre-delivery digest", async (t) => {
  const f = fixture(t, { secretProvenance: true });
  await f.run("deliver");
  await f.run("inspect");
  f.preparePost();
  f.post.ownerPlan.authoritySettingsSha256 = f.plan.authoritySettingsSha256;
  f.post.state = sealed({ ...f.post.state, planHash: digest(canonical(f.post.ownerPlan)) });
  await assert.rejects(f.run("reseal"));
  assert.equal(f.state.phase, "reseal-candidate");
});

test("raw post settings stay pinned even if provenance reverts to original value", async (t) => {
  const f = fixture(t, { secretProvenance: true });
  await f.run("deliver");
  await f.run("inspect");
  f.settings.annotations["workers/triggered_by"] = "version_upload";
  assert.equal(digest(canonical(f.settings)), f.plan.authoritySettingsSha256);
  await assert.rejects(f.run("inspect"));
  assert.equal(f.state.phase, "reseal-candidate");
});

test("unchanged legacy reseal state without a post digest remains exact", async (t) => {
  const f = fixture(t);
  await f.run("deliver");
  await f.run("inspect");
  delete f.state.afterSettingsSha256;
  Object.assign(f.state, sealed(f.state));
  f.preparePost();
  await f.run("reseal");
  assert.equal(f.state.phase, "resealed");
});

test("one scoped issuance/delivery, exact new version, independent owner recovery and resealing", async (t) => {
  const f = fixture(t);
  await f.run("deliver");
  assert.equal(f.state.phase, "delivery-confirmed");
  assert.equal(f.state.enrollmentAdmitted, undefined);
  await assert.rejects(f.run("deliver"), /refused/);
  await f.run("inspect");
  assert.equal(f.state.phase, "reseal-candidate");
  f.preparePost();
  await f.run("reseal");
  assert.equal(f.state.phase, "resealed");
  assert.equal(f.state.enrollmentAdmitted, false);
  assert.equal(f.state.candidateIsolation.authority.version, newVersion);
  assert.deepEqual(
    f.state.candidateIsolation.authority.namespaces,
    f.plan.isolation.authority.namespaces,
  );
  assert.equal(f.model.commands.filter((c) => c.argv.includes("assume-role")).length, 1);
  assert.equal(f.model.calls.filter((c) => c.method === "PATCH").length, 1);
  assert.deepEqual(f.model.delivery, {
    secrets: Object.fromEntries(
      [
        ["AWS_ACCESS_KEY_ID", "fake-new-id"],
        ["AWS_SECRET_ACCESS_KEY", "fake-new-secret"],
        ["AWS_SESSION_TOKEN", "fake-new-token"],
      ].map(([name, text]) => [name, { name, text, type: "secret_text" }]),
    ),
  });
  for (const secret of [
    "fake-new-id",
    "fake-new-secret",
    "fake-new-token",
    "fake-bootstrap-secret",
    "fake-cf-token",
  ]) {
    assert.ok(!JSON.stringify(f.model.saves).includes(secret));
    assert.ok(!JSON.stringify(f.model.commands.map((c) => c.argv)).includes(secret));
  }
});

test("authorized idle replacement completes while the old session may remain valid", async (t) => {
  const f = fixture(t);
  f.authorizeEarlyReplacement();
  await f.run("deliver");
  assert.equal(f.state.previousSessionExpiresAt, f.plan.previousSessionExpiresAt);
  assert.equal(f.state.unexpiredSessionReplacement, true);
  assert.equal(
    f.model.saves.find((s) => s.phase === "issuance-intent").unexpiredSessionReplacement,
    true,
  );
  assert.ok(Date.parse(f.state.previousSessionExpiresAt) > f.model.now);
  await assert.rejects(f.run("deliver"));
  await f.run("inspect");
  f.preparePost();
  await f.run("reseal");
  assert.equal(f.state.phase, "resealed");
  assert.equal(f.state.enrollmentAdmitted, false);
  assert.equal(f.state.candidateIsolation.authority.version, newVersion);
  assert.deepEqual(
    f.state.candidateIsolation.authority.namespaces,
    f.plan.isolation.authority.namespaces,
  );
  assert.equal(f.model.commands.filter((c) => c.argv.includes("assume-role")).length, 1);
  assert.equal(
    f.model.commands.filter((c) => c.argv.includes("idle-controller-recover")).length,
    1,
  );
  assert.equal(f.model.calls.filter((c) => c.method === "PATCH").length, 1);
});

for (const [label, mutate] of Object.entries({
  missingAuthorization: (f) => {
    delete f.plan.allowUnexpiredSessionReplacement;
  },
  deniedAuthorization: (f) => {
    f.plan.allowUnexpiredSessionReplacement = false;
  },
  malformedAuthorization: (f) => {
    f.plan.allowUnexpiredSessionReplacement = "true";
  },
  missingStoppedUse: (f) => {
    delete f.preCustody.previousSessionUseStopped;
  },
  ongoingUse: (f) => {
    f.preCustody.previousSessionUseStopped = false;
  },
  activeCaller: (f) => {
    f.model.client = true;
  },
  activeRun: (f) => {
    f.pre.state = sealed({ ...f.pre.state, discovery: "active-or-unknown" });
  },
}))
  test(`early replacement refuses ${label} before issuance`, async (t) => {
    const f = fixture(t);
    f.authorizeEarlyReplacement();
    mutate(f);
    f.plan.preCustodySha256 = digest(canonical(f.preCustody));
    f.reset();
    await assert.rejects(f.run("deliver"));
    assert.equal(f.model.commands.length, 0);
    assert.equal(f.model.calls.filter((c) => c.method === "PATCH").length, 0);
  });

test("early replacement rechecks stopped previous-session use before delivery and resealing", async (t) => {
  for (const stage of ["before-delivery", "before-reseal"]) {
    const f = fixture(t);
    f.authorizeEarlyReplacement();
    if (stage === "before-delivery") {
      f.model.command = async (argv) => {
        if (argv.includes("get-caller-identity")) delete f.preCustody.previousSessionUseStopped;
      };
      await assert.rejects(f.run("deliver"));
      assert.equal(f.model.calls.filter((c) => c.method === "PATCH").length, 0);
    } else {
      await f.run("deliver");
      f.preparePost();
      const read = f.context.readCustody;
      f.context.readCustody = async (name) => ({
        ...(await read(name)),
        previousSessionUseStopped: false,
      });
      await assert.rejects(f.run("reseal"));
      assert.notEqual(f.state.phase, "resealed");
      assert.equal(f.state.enrollmentAdmitted, undefined);
      assert.equal(f.model.calls.filter((c) => c.method === "PATCH").length, 1);
    }
  }
});

for (const [label, mutate] of Object.entries({
  source: (f) => {
    f.plan.sources["image-qualification-control.mjs"] = "0".repeat(64);
  },
  futureOldSession: (f) => {
    f.plan.previousSessionExpiresAt = new Date(f.start + 60000).toISOString();
  },
  invalidOldSessionExpiry: (f) => {
    f.plan.previousSessionExpiresAt = "invalid";
    f.plan.allowUnexpiredSessionReplacement = true;
  },
  namespace: (f) => {
    f.plan.isolation.authority.namespaces[0].id = "0".repeat(32);
  },
  publicExposure: (f) => {
    f.model.exposure = { enabled: true };
  },
  activeCaller: (f) => {
    f.model.client = true;
  },
  inactiveToken: (f) => {
    f.model.tokens = "disabled";
  },
  staleCustody: (f) => {
    f.preCustody.observedAt = new Date(f.start - 1000000).toISOString();
  },
  custody: (f) => {
    f.preCustody.fallbackReady = false;
  },
  unknownProbeOrRun: (f) => {
    f.pre.state = sealed({ ...f.pre.state, discovery: "active-or-unknown" });
  },
  unsettledCaller: (f) => {
    f.pre.state = sealed({ ...f.pre.state, settled: false });
  },
  badProofHash: (f) => {
    f.pre.state.sequence++;
  },
  missingSecret: (f) => {
    f.settings.bindings.pop();
  },
  tool: (f) => {
    fs.appendFileSync(f.plan.tools.aws.path, "changed");
  },
  extraDeliveryTool: (f) => {
    f.plan.tools.wrangler = f.plan.tools.aws;
  },
  copiedDestination: (f) => {
    f.context.workdir = path.join(f.dir, "copy");
  },
  deadline: (f) => {
    f.model.now = Date.parse(f.plan.deliveryStopsAt);
  },
  inheritedSession: (f) => {
    f.context.env.AWS_SESSION_TOKEN = "fake-old-session";
  },
}))
  test(`preflight refuses ${label} before issuance`, async (t) => {
    const f = fixture(t);
    mutate(f);
    f.reset();
    await assert.rejects(f.run("deliver"));
    assert.equal(f.model.commands.length, 0);
  });

test("failed journal or exclusive intent persistence prevents issuance", async (t) => {
  for (const claim of [false, true]) {
    const f = fixture(t);
    if (claim) claimSessionIssuance(f.plan);
    else
      f.model.save = async () => {
        throw new Error("disk full");
      };
    await assert.rejects(f.run("deliver"));
    assert.equal(f.model.commands.length, 0);
  }
});

test("exclusive issuance marker prevents minting after state loss or a fresh process", async (t) => {
  const f = fixture(t);
  await f.run("deliver");
  f.reset();
  await assert.rejects(f.run("deliver"));
  assert.equal(f.model.commands.filter((c) => c.argv.includes("assume-role")).length, 1);
  const script = path.join(f.dir, "claim.mjs");
  fs.writeFileSync(
    script,
    `import {claimSessionIssuance} from ${JSON.stringify(new URL("./image-qualification-session-replacement.mjs", import.meta.url).href)}; claimSessionIssuance(JSON.parse(process.argv[2]));`,
  );
  const child = spawnSync(process.execPath, [script, JSON.stringify(f.plan)], {
    env: {},
    encoding: "utf8",
  });
  assert.notEqual(child.status, 0);
  assert.match(child.stderr, /EEXIST/);
});

for (const early of [false, true])
  for (const stage of ["issuance", "delivery", "confirmed-save"])
    test(`uncertain ${stage} cannot reissue/redeliver/admit${early ? " during early replacement" : ""}`, async (t) => {
      const f = fixture(t);
      if (early) f.authorizeEarlyReplacement();
      if (stage === "issuance")
        f.model.command = async (argv) => {
          if (argv.includes("assume-role")) throw new Error("lost STS response");
        };
      if (stage === "delivery") f.model.lostDelivery = true;
      if (stage === "confirmed-save")
        f.model.save = async (next) => {
          if (next.phase === "delivery-confirmed") throw new Error("lost durable acknowledgement");
        };
      await assert.rejects(f.run("deliver"));
      await assert.rejects(f.run("deliver"));
      const count = f.model.commands.length;
      await f.run("inspect");
      assert.equal(f.state.admissionDenied, true);
      assert.equal(f.state.candidateIsolation, undefined);
      assert.equal(f.model.commands.length, count);
    });

for (const [label, mutate] of Object.entries({
  principal: (f) => {
    f.model.identity = { Account: "999999999999" };
  },
  lifetime: (f) => {
    f.model.assumed = { Credentials: {} };
  },
  versionChanged: (f) => {
    f.model.command = async (argv) => {
      if (argv.includes("get-caller-identity")) f.model.version = newVersion;
    };
  },
  deliveryPersistence: (f) => {
    f.model.save = async (value) => {
      if (value.phase === "delivery-intent") throw new Error("disk full");
    };
  },
}))
  test(`post-STS ${label} refuses delivery`, async (t) => {
    const f = fixture(t);
    mutate(f);
    await assert.rejects(f.run("deliver"));
    assert.equal(f.model.calls.filter((c) => c.method === "PATCH").length, 0);
    await assert.rejects(f.run("deliver"));
  });

for (const [label, mutate] of Object.entries({
  moduleDrift: (f) => {
    f.model.module = { ...f.plan.authorityModule, sha256: "0".repeat(64) };
  },
  oldVersion: (f) => {
    f.model.version = version;
  },
  oldProof: (f) => {
    f.context.readProof = async () => f.pre;
  },
  failedRecovery: (f) => {
    f.model.command = async (argv) => {
      if (argv.includes("idle-controller-recover")) throw new Error("failed");
    };
  },
  missingRecoveryTransition: (f) => {
    f.context.command = async () => "";
  },
  staleProof: (f) => {
    f.model.now += 1000000;
  },
}))
  test(`resealing refuses ${label} without inheriting success`, async (t) => {
    const f = fixture(t);
    await f.run("deliver");
    f.preparePost();
    mutate(f);
    await assert.rejects(f.run("reseal"));
    assert.notEqual(f.state.phase, "resealed");
    assert.equal(f.state.enrollmentAdmitted, undefined);
    assert.equal(f.model.calls.filter((c) => c.method === "PATCH").length, 1);
  });

test("selected new version cannot silently move again", async (t) => {
  const f = fixture(t);
  await f.run("deliver");
  await f.run("inspect");
  f.model.version = "44444444-4444-4444-8444-444444444444";
  await assert.rejects(f.run("inspect"));
  assert.equal(f.state.afterVersion, newVersion);
});

test("cancelled command retains issuance fence and prevents forward retry", async (t) => {
  const f = fixture(t),
    stop = new AbortController();
  f.context.signal = stop.signal;
  f.model.command = async () => {
    stop.abort();
    throw new Error("cancelled");
  };
  await assert.rejects(f.run("deliver"));
  assert.equal(f.state.phase, "issuance-intent");
  await assert.rejects(f.run("deliver"));
  assert.equal(f.model.commands.length, 1);
});

test("secret-safe child bounds output, failure, timeout, cancellation and inherited environment", async () => {
  const argv = (script) => [process.execPath, "-e", script];
  assert.equal(
    await sessionChild(
      argv('process.stdout.write(process.env.FAKE_VALUE); process.stderr.write("fake-secret")'),
      { FAKE_VALUE: "ok" },
      "",
    ),
    "ok",
  );
  for (const args of [
    [argv('console.error("fake-secret");process.exit(1)'), {}, ""],
    [argv('process.stdout.write("a".repeat(200000))'), {}, ""],
    [argv("setInterval(()=>{},1000)"), {}, "", { timeout: 30 }],
    [["/does-not-exist/fake"], {}, ""],
  ])
    await assert.rejects(sessionChild(...args), (error) => !error.message.includes("fake-secret"));
  const stop = new AbortController();
  const result = sessionChild(argv("setInterval(()=>{},1000)"), {}, "", { signal: stop.signal });
  stop.abort();
  await assert.rejects(result, /refused/);
});

test("plan and proof validators reject changed identities without a live effect", (t) => {
  const f = fixture(t);
  validateReplacementPlan(f.plan);
  verifyReplacementIdle(f.plan, f.pre, f.plan.isolation, f.model.now);
  f.pre.ownerPlan.sourceSha = "0".repeat(40);
  assert.throws(() => verifyReplacementIdle(f.plan, f.pre, f.plan.isolation, f.model.now));
});

test("documented bulk request has no create fallback or retry on absence, denial, malformed success or lost response", async (t) => {
  for (const outcome of ["absent", "denied", "lost", "malformed"]) {
    const f = fixture(t);
    const original = f.context.cf.request.bind(f.context.cf);
    let writes = 0;
    f.context.cf.request = async (route, init, options) => {
      if (init?.method !== "PATCH") return original(route, init, options);
      writes++;
      assert.equal(route, `/workers/scripts/${authorityName}/secrets-bulk`);
      if (outcome === "malformed") return {};
      throw new Error(outcome);
    };
    await assert.rejects(f.run("deliver"));
    await assert.rejects(f.run("deliver"));
    assert.equal(writes, 1);
    assert.equal(f.state.phase, "delivery-intent");
    await f.run("inspect");
    assert.equal(f.state.admissionDenied, true);
    assert.equal(writes, 1);
  }
});

test("real management client sends one fixed-endpoint bounded request without persistent secret files", async (t) => {
  const f = fixture(t),
    original = f.context.cf.request.bind(f.context.cf);
  const requests = [];
  t.mock.method(globalThis, "fetch", async (url, init) => {
    requests.push({ url, init });
    return new Response(JSON.stringify({ success: true, result: {} }), { status: 200 });
  });
  f.context.cf.request = async (route, init, options) => {
    if (init?.method !== "PATCH") return original(route, init, options);
    const response = await Cloudflare.prototype.request.call(f.context.cf, route, init, options);
    f.model.version = newVersion;
    return response;
  };
  await f.run("deliver");
  assert.equal(requests.length, 1);
  assert.equal(
    requests[0].url,
    `https://api.cloudflare.com/client/v4/accounts/${f.plan.isolation.accountId}/workers/scripts/${authorityName}/secrets-bulk`,
  );
  assert.equal(requests[0].init.redirect, "error");
  assert.equal(requests[0].init.headers["Content-Type"], "application/merge-patch+json");
  assert.equal(
    f.model.commands.length,
    2,
    "only STS and identity children; no delivery CLI/dotenv/logs",
  );
  assert.deepEqual(fs.readdirSync(f.dir).toSorted(), [
    "aws.json",
    "node.json",
    "session-issuance-intent.json",
  ]);
});

test("fresh process reconciles original durable journal and retains issuance fence", async (t) => {
  const f = fixture(t);
  const journal = {
    stateName: "session-replacement-state.json",
    lockName: "session-replacement.lock",
    create: true,
    initial: () => replacementInitialState(f.plan),
  };
  f.model.lostDelivery = true;
  await assert.rejects(
    withQualificationJournal(f.dir, journal, (state, save) =>
      replaceIdleSession(f.plan, state, {
        ...f.context,
        mode: "deliver",
        save: async (value) => {
          await f.context.save(value);
          await save(value);
        },
      }),
    ),
  );
  const original = readPrivateJSON(path.join(f.dir, journal.stateName));
  assert.equal(original.phase, "delivery-intent");
  const script = path.join(f.dir, "reconcile.mjs");
  fs.writeFileSync(
    script,
    `import {readPrivateJSON,withQualificationJournal} from ${JSON.stringify(new URL("./image-qualification-journal.mjs", import.meta.url).href)};
import {replaceIdleSession} from ${JSON.stringify(new URL("./image-qualification-session-replacement.mjs", import.meta.url).href)};
const plan=JSON.parse(process.argv[2]);
await withQualificationJournal(plan.recoveryDirectory, ${JSON.stringify({ stateName: journal.stateName, lockName: journal.lockName })}, async (state,save)=>{
await replaceIdleSession(plan,state,{mode:"inspect",save,workdir:plan.recoveryDirectory,
cf:{request:async(route)=>{if(route==="/tokens/verify")return {id:plan.tokenId,status:"active",expires_on:new Date(Date.parse(plan.cycleRecoveryBy)+86400000).toISOString()};if(route.endsWith("/deployments"))return {deployments:[{versions:[{version_id:${JSON.stringify(newVersion)},percentage:100}]}]};throw Error("unexpected read");},latestVersion:async()=>${JSON.stringify(newVersion)}},
command:async()=>{throw Error("must never execute credential effect");}});
});`,
  );
  const child = spawnSync(process.execPath, [script, JSON.stringify(f.plan)], {
    env: {},
    encoding: "utf8",
  });
  assert.equal(child.status, 0, child.stderr);
  const reconciled = readPrivateJSON(path.join(f.dir, journal.stateName));
  assert.equal(reconciled.phase, "delivery-intent");
  assert.equal(reconciled.admissionDenied, true);
  assert.ok(reconciled.sequence > original.sequence);
  assert.ok(fs.existsSync(path.join(f.dir, "session-issuance-intent.json")));
});

test(
  "real descendant group settles on success, timeout and cancellation",
  { skip: process.platform === "win32" },
  async (t) => {
    const dir = fs.realpathSync(
      fs.mkdtempSync(path.join(os.tmpdir(), "qualification-session-process-")),
    );
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    for (const mode of ["success", "timeout", "cancel"]) {
      const pidfile = path.join(dir, mode);
      const script = `const {spawn}=require("node:child_process");const fs=require("node:fs");
const c=spawn(process.execPath,["-e","setInterval(()=>{},1000)"],{stdio:"ignore"});
fs.writeFileSync(${JSON.stringify(pidfile)},String(c.pid));
${mode === "success" ? "process.exit(0)" : "setInterval(()=>{},1000)"}`;
      const stop = new AbortController();
      const result = sessionChild([process.execPath, "-e", script], {}, "", {
        timeout: mode === "timeout" ? 500 : 10000,
        signal: stop.signal,
      });
      // Attach immediately so an early failure never escapes as an unhandled promise.
      const observed = result.then(
        () => ({ ok: true }),
        (error) => ({ ok: false, error }),
      );
      const until = performance.now() + 5000;
      while (!fs.existsSync(pidfile) && performance.now() < until)
        await new Promise((r) => setTimeout(r, 10));
      assert.ok(fs.existsSync(pidfile));
      const pid = Number(fs.readFileSync(pidfile, "utf8"));
      if (mode === "cancel") stop.abort();
      const outcome = await observed;
      assert.equal(outcome.ok, mode === "success");
      if (!outcome.ok) assert.match(outcome.error.message, /refused/);
      // Group absence is established by sessionChild. On macOS the killed
      // orphan PID can briefly remain visible while the kernel finishes reaping;
      // a successful existence check followed by an absent ps sample reproduced
      // this race. Require bounded eventual PID absence, without another signal.
      const reapBy = performance.now() + 1000;
      let reaped = false;
      do {
        try {
          process.kill(pid, 0);
        } catch (error) {
          assert.equal(error.code, "ESRCH");
          reaped = true;
          break;
        }
        await new Promise((resolve) => setTimeout(resolve, 10));
      } while (performance.now() < reapBy);
      assert.ok(reaped, `${mode}: killed descendant did not reap within one second`);
    }
  },
);

for (const [label, alter] of Object.entries({
  missingScope: (p) => {
    delete p.runSession;
  },
  foreignRecoveryCutoff: (p) => {
    p.runSession.recoveryExpiresAt = p.runSession.expiresAt;
  },
  malformedRun: (p) => {
    p.runSession.runId = "image-qualification-*-1";
  },
  lateScope: (p) => {
    p.runSession.startsAt = p.authorityRetentionEndsAt;
  },
}))
  test(`run-scoped issuance refuses ${label} without issuing or delivering`, async (t) => {
    const f = fixture(t);
    alter(f.plan);
    f.reset();
    await assert.rejects(() => f.run("deliver"));
    assert.equal(f.model.commands.length, 0);
    assert.equal(f.model.delivery, undefined);
  });
