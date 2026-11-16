/* eslint-disable no-await-in-loop -- Effect ordering, pagination and process settlement must stay sequential. */
// Operator tooling only. Registry truth remains with the existing private owner.
import { spawn, execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { Cloudflare } from "./image-qualification-control.mjs";
import { validateIdlePlan } from "./image-qualification-idle-controller.mjs";
import {
  authorityName,
  canonical,
  digest,
  readAdmission,
  verifyAuthority,
} from "./image-qualification-isolation.mjs";
import { readPrivateJSON, withQualificationJournal } from "./image-qualification-journal.mjs";
import {
  validateRunSession,
  runSessionTags,
  runSessionPolicy,
} from "./image-qualification-run-session.mjs";

const directory = path.dirname(fileURLToPath(import.meta.url));
const root = path.dirname(directory);
const secretNames = ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"];
const sha = /^[0-9a-f]{64}$/;
const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/;
const equal = (a, b) => canonical(a) === canonical(b);
const fail = () => {
  throw new Error(
    "qualification session replacement refused; preserve original intent and custody",
  );
};
// Only this local error can render specific operator guidance; cloud/tool errors
// keep the fixed redacted failure below.
class SessionNotActive extends Error {
  constructor(seconds) {
    super(
      `Session not active. Retry after ${seconds} seconds only within the original delivery window; keep the frozen plan and scope unchanged. No issuance was attempted.`,
    );
  }
}
const time = (v) => (typeof v === "string" ? Date.parse(v) : NaN);
const recordHash = ({ integrity: _integrity, ...v }) => digest(canonical(v));
const seal = (v) => ({ ...v, integrity: recordHash(v) });
const sourceFiles = [
  "image-qualification-session-replacement.mjs",
  "image-qualification-control.mjs",
  "image-qualification-idle-controller.mjs",
  "image-qualification-isolation.mjs",
  "image-qualification-journal.mjs",
  "image-qualification-controller-worker.mjs",
  "image-qualification-binding-probe.mjs",
  "image-qualification-transport.mjs",
  "image-qualification-network.ts",
  "image-qualification-executor-token.mjs",
  "image-qualification-run-session.mjs",
];
export const replacementSources = () =>
  Object.fromEntries(
    sourceFiles.map((name) => [name, digest(fs.readFileSync(path.join(directory, name)))]),
  );
export const replacementInitialState = (plan) =>
  seal({
    profile: plan.profile,
    planHash: digest(canonical(plan)),
    phase: "prepared",
    sequence: 0,
  });

// Only this known predecessor needs the provenance reconciliation correction.
// Every other effect-bearing dependency must still match the current source.
const recoveryPredecessor = "cecdf9b2a5e7a70fb7c2ff6e6dc6598db85b17900635fff5f52eb1d69ccdb994";
export function replacementRecoverySources(plan, recovery) {
  const expected = {
    ...replacementSources(),
    "image-qualification-session-replacement.mjs": recoveryPredecessor,
  };
  if (
    !recovery ||
    !/^[0-9a-f]{40}$/.test(recovery.sourceSha ?? "") ||
    recovery.sourceSha === plan.sourceSha ||
    recovery.originalSourceSha !== plan.sourceSha ||
    !path.isAbsolute(recovery.originalRoot ?? "") ||
    !equal(recovery.sources, expected) ||
    !equal(plan.sources, expected)
  )
    fail();
  return expected;
}

export function validateReplacementPlan(plan, sources = replacementSources()) {
  if (
    !plan ||
    plan.profile !== "idle-session-replacement/v1" ||
    !/^[0-9a-f]{24}$/.test(plan.attemptId ?? "") ||
    !/^[0-9a-f]{24}$/.test(plan.postIdleAttemptId ?? "") ||
    plan.postIdleAttemptId === plan.attemptId ||
    !/^[0-9a-f]{40}$/.test(plan.sourceSha ?? "") ||
    !/^[0-9a-f]{32}$/.test(plan.tokenId ?? "") ||
    !path.isAbsolute(plan.recoveryDirectory ?? "") ||
    !equal(plan.sources, sources) ||
    !sha.test(plan.preCustodySha256 ?? "") ||
    !sha.test(plan.authoritySettingsSha256 ?? "") ||
    !sha.test(plan.authorityModule?.sha256 ?? "") ||
    plan.authorityModule?.count !== 1 ||
    !/^[a-zA-Z0-9_.-]+$/.test(plan.authorityModule?.module ?? "") ||
    !Number.isFinite(time(plan.startsAt)) ||
    time(plan.deliveryStopsAt) !== time(plan.startsAt) + 20 * 60_000 ||
    time(plan.recoveryStopsAt) !== time(plan.startsAt) + 60 * 60_000 ||
    !Number.isFinite(time(plan.previousSessionExpiresAt)) ||
    (plan.allowUnexpiredSessionReplacement !== undefined &&
      typeof plan.allowUnexpiredSessionReplacement !== "boolean") ||
    (time(plan.previousSessionExpiresAt) > time(plan.startsAt) &&
      plan.allowUnexpiredSessionReplacement !== true) ||
    !(time(plan.recoveryStopsAt) < time(plan.cycleRecoveryBy)) ||
    !(time(plan.cycleRecoveryBy) <= time(plan.startsAt) + 8 * 3600_000) ||
    !(time(plan.cycleRecoveryBy) <= time(plan.authorityRetentionEndsAt))
  )
    fail();
  const admission = readAdmission({
    CLOUDFLARE_ACCOUNT_ID: plan.isolation?.accountId,
    QUALIFICATION_ISOLATION_JSON: JSON.stringify(plan.isolation),
    QUALIFICATION_ISOLATION_SHA256: digest(canonical(plan.isolation)),
  });
  if (plan.runSession !== undefined) {
    const scope = validateRunSession(plan.runSession);
    if (
      time(scope.startsAt) < time(plan.startsAt) ||
      time(scope.startsAt) > time(plan.recoveryStopsAt) ||
      scope.recoveryExpiresAt !== plan.cycleRecoveryBy
    )
      fail();
  }
  if (
    !equal(Object.keys(plan.tools ?? {}).toSorted(), ["aws", "node"]) ||
    Object.values(plan.tools).some(
      (tool) => !path.isAbsolute(tool.path ?? "") || !sha.test(tool.sha256 ?? ""),
    )
  )
    fail();
  return admission;
}

function tools(plan) {
  for (const tool of Object.values(plan.tools)) {
    if (
      fs.realpathSync(tool.path) !== tool.path ||
      !fs.statSync(tool.path).isFile() ||
      digest(fs.readFileSync(tool.path)) !== tool.sha256
    )
      fail();
  }
}

export function claimSessionIssuance(plan) {
  const file = path.join(plan.recoveryDirectory, "session-issuance-intent.json");
  const fd = fs.openSync(
    file,
    fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL | fs.constants.O_NOFOLLOW,
    0o600,
  );
  try {
    fs.writeFileSync(
      fd,
      canonical({ attemptId: plan.attemptId, planHash: digest(canonical(plan)) }),
    );
    fs.fsyncSync(fd);
  } finally {
    fs.closeSync(fd);
  }
  const directoryFd = fs.openSync(plan.recoveryDirectory, "r");
  try {
    fs.fsyncSync(directoryFd);
  } finally {
    fs.closeSync(directoryFd);
  }
}

function custody(plan, value, now, version, before) {
  if (
    !value ||
    value.profile !== "idle-session-custody/v1" ||
    value.attemptId !== plan.attemptId ||
    value.sourceSha !== plan.sourceSha ||
    value.authorityVersion !== version ||
    value.exclusiveUntil !== plan.recoveryStopsAt ||
    ![
      "continuousExclusiveCustody",
      "noConcurrentWritersOrCallers",
      "noActiveWorkflowWriters",
      "previousRunsSettled",
      "fallbackReady",
    ].every((key) => value[key] === true) ||
    // Rotation does not revoke the previous session. Its consumers must already
    // be stopped, in addition to the existing idle and exclusive-custody checks.
    (time(plan.previousSessionExpiresAt) > time(plan.startsAt) &&
      value.previousSessionUseStopped !== true) ||
    !(0 <= now - time(value.observedAt) && now - time(value.observedAt) <= 900_000) ||
    (before && digest(canonical(value)) !== plan.preCustodySha256)
  )
    fail();
  // This is an explicitly trusted operator assertion, not authenticated history.
}

export function verifyReplacementIdle(
  plan,
  proof,
  isolation,
  now,
  after = false,
  settingsSha256 = plan.authoritySettingsSha256,
) {
  const { ownerPlan, state } = proof ?? {};
  validateIdlePlan(ownerPlan);
  if (
    ownerPlan.sourceSha !== plan.sourceSha ||
    ownerPlan.tokenId !== plan.tokenId ||
    !equal(ownerPlan.isolation, isolation) ||
    !equal(ownerPlan.authorityModule, plan.authorityModule) ||
    !sha.test(settingsSha256 ?? "") ||
    ownerPlan.authoritySettingsSha256 !== settingsSha256 ||
    (after && ownerPlan.attemptId !== plan.postIdleAttemptId) ||
    state?.profile !== ownerPlan.profile ||
    state?.planHash !== digest(canonical(ownerPlan)) ||
    state?.integrity !== recordHash(state) ||
    !Number.isSafeInteger(state.sequence) ||
    state.sequence < 1 ||
    state.phase !== "settled" ||
    state.settled !== true ||
    state.failed !== false ||
    state.discovery !== "idle" ||
    !sha.test(state.identityDigest ?? "") ||
    !Array.isArray(state.events)
  )
    fail();
  const observed = state.events.findLast((event) => event.phase === "discovery-complete");
  if (!(0 <= now - time(observed?.at) && now - time(observed.at) <= 900_000)) fail();
  return time(observed.at);
}

// Bounded, sanitized, POSIX process groups. Neither stderr nor raw errors escape.
// Trusted AWS/Node tools must not detach descendants into other groups.
export async function sessionChild(
  argv,
  env,
  input,
  { signal, timeout = 180_000, cwd, importKeyDryRun = false } = {},
) {
  if (process.platform === "win32" || signal?.aborted) fail();
  const child = spawn(argv[0], argv.slice(1), {
    env,
    cwd,
    detached: true,
    stdio: ["pipe", "pipe", importKeyDryRun ? "pipe" : "ignore"],
  });
  let timer,
    failed = false,
    size = 0,
    exitCode;
  const chunks = [],
    errors = [];
  const kill = () => {
    if (child.pid)
      try {
        process.kill(-child.pid, "SIGKILL");
      } catch (e) {
        if (e.code !== "ESRCH") failed = true;
      }
  };
  const stop = () => {
    failed = true;
    kill();
  };
  const closed = new Promise((resolve) => child.on("close", resolve));
  const exited = new Promise((resolve) => {
    child.on("error", () => {
      failed = true;
      resolve();
    });
    child.on("exit", (code) => {
      exitCode = code;
      if (code !== (importKeyDryRun ? 254 : 0)) failed = true;
      resolve();
    });
  });
  child.stdout.on("data", (data) => {
    size += data.length;
    if (size > 128 * 1024) stop();
    else chunks.push(data);
  });
  child.stderr?.on("data", (data) => {
    size += data.length;
    if (size > 128 * 1024) stop();
    else errors.push(data);
  });
  child.stdin.on("error", stop);
  try {
    signal?.addEventListener("abort", stop, { once: true });
    timer = setTimeout(stop, timeout);
    if (signal?.aborted) stop();
    child.stdin.end(input ?? "");
    await exited;
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener("abort", stop);
    if (child.pid) {
      kill();
      // Settlement is deliberately not cancelled when the caller is cancelled.
      const end = performance.now() + 5000;
      for (;;) {
        try {
          process.kill(-child.pid, 0);
        } catch (e) {
          if (e.code === "ESRCH") break;
          fail();
        }
        if (performance.now() >= end) {
          child.stdout.destroy();
          child.stdin.destroy();
          child.stderr?.destroy();
          fail();
        }
        await new Promise((resolve) => setTimeout(resolve, 10));
      }
    }
    await closed;
  }
  if (failed) fail();
  if (importKeyDryRun) {
    // CLI v2 service errors are exit254. Never accept substrings, stdout, local
    // errors or a successful mutation; never return the raw message/encoded detail.
    const match =
      /^\n?(?:aws: \[ERROR\]: )?An error occurred \((DryRunOperation|UnauthorizedOperation)\) when calling the ImportKeyPair operation: [^\r\n]+\r?\n?$/.exec(
        Buffer.concat(errors).toString("utf8"),
      );
    if (exitCode !== 254 || chunks.length || !match) fail();
    return match[1];
  }
  return Buffer.concat(chunks).toString("utf8");
}

// The RFC8032 public test vector is only syntax for DryRun, never a usable
// qualification login key. No private key is generated or accepted here.
const dryRunPublicKey = Buffer.from(
  "ssh-ed25519 " +
    Buffer.concat([
      Buffer.from("0000000b7373682d6564323535313900000020", "hex"),
      Buffer.from("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a", "hex"),
    ]).toString("base64"),
).toString("base64");

async function importKeyPreflight(plan, run, sessionEnv, signal, now) {
  const tags = runSessionTags(plan.runSession);
  const otherRun = plan.runSession.runId.replace(
    /([1-9])[0-9]*-1$/,
    (value) => (value[0] === "1" ? "2" : "1") + value.slice(1),
  );
  const mismatches = [
    otherRun,
    `${otherRun}@example.invalid`,
    (plan.runSession.candidateSha[0] === "0" ? "1" : "0") + plan.runSession.candidateSha.slice(1),
    new Date(time(plan.runSession.expiresAt) + 60_000).toISOString(),
  ];
  const cases = [{ name: "same-run-before", tags, expected: "DryRunOperation" }];
  for (const [index, key] of Object.keys(tags).entries()) {
    const missing = { ...tags };
    delete missing[key];
    cases.push(
      { name: `missing-${key}`, tags: missing, expected: "UnauthorizedOperation" },
      {
        name: `mismatched-${key}`,
        tags: { ...tags, [key]: mismatches[index] },
        expected: "UnauthorizedOperation",
      },
    );
  }
  cases.push({ name: "same-run-after", tags, expected: "DryRunOperation" });
  const bounded = AbortSignal.any([AbortSignal.timeout(120_000), ...(signal ? [signal] : [])]);
  const startedAt = new Date(now()).toISOString(),
    observations = [];
  for (const item of cases) {
    const code = await run(
      [
        plan.tools.aws.path,
        "ec2",
        "import-key-pair",
        "--dry-run",
        "--key-name",
        `${plan.runSession.runId}-issuer-preflight`,
        "--public-key-material",
        dryRunPublicKey,
        "--cli-binary-format",
        "base64",
        "--cli-error-format",
        "legacy",
        "--tag-specifications",
        JSON.stringify([
          {
            ResourceType: "key-pair",
            Tags: Object.entries(item.tags).map(([Key, Value]) => ({ Key, Value })),
          },
        ]),
        "--region",
        plan.isolation.aws.region,
        "--output",
        "json",
      ],
      sessionEnv,
      undefined,
      { importKeyDryRun: true, signal: bounded, timeout: 10_000 },
    );
    if (bounded.aborted || code !== item.expected) fail();
    observations.push({ name: item.name, code });
  }
  return {
    profile: "qualification-import-key-dry-run/v1",
    startedAt,
    completedAt: new Date(now()).toISOString(),
    observations,
  };
}

const safeEnv = () => ({ PATH: "/usr/bin:/bin", LANG: "C.UTF-8", CI: "true" });
const awsEnv = (credentials, region) => ({
  ...safeEnv(),
  ...credentials,
  AWS_CONFIG_FILE: "/dev/null",
  AWS_SHARED_CREDENTIALS_FILE: "/dev/null",
  AWS_EC2_METADATA_DISABLED: "true",
  AWS_DEFAULT_REGION: region,
  AWS_PAGER: "",
  AWS_MAX_ATTEMPTS: "1",
  AWS_RETRY_MODE: "standard",
  AWS_CLI_AUTO_PROMPT: "off",
});

// Effects are injected for offline tests. The CLI below fixes every live effect.
export async function replaceIdleSession(
  plan,
  state,
  {
    mode,
    cf,
    save,
    readProof,
    readCustody,
    command = sessionChild,
    env = process.env,
    now = Date.now,
    signal,
    workdir,
    claimIssuance = claimSessionIssuance,
    recovery,
  },
) {
  const admission = validateReplacementPlan(
    plan,
    recovery ? replacementRecoverySources(plan, recovery) : replacementSources(),
  );
  const initialWall = now(),
    initialMono = performance.now(),
    wall = now;
  now = () => Math.max(wall(), initialWall + performance.now() - initialMono);
  if (
    !["deliver", "inspect", "reseal"].includes(mode) ||
    typeof save !== "function" ||
    state.profile !== plan.profile ||
    state.planHash !== digest(canonical(plan)) ||
    workdir !== plan.recoveryDirectory ||
    state.integrity !== recordHash(state) ||
    !Number.isSafeInteger(state.sequence) ||
    state.sequence < 0 ||
    (state.afterSettingsSha256 !== undefined && !sha.test(state.afterSettingsSha256)) ||
    (state.recoverySourceSha !== undefined && state.recoverySourceSha !== recovery?.sourceSha) ||
    (recovery &&
      (mode === "deliver" ||
        !["delivery-confirmed", "reseal-candidate", "resealed"].includes(state.phase))) ||
    ![
      "prepared",
      "issuance-intent",
      "delivery-intent",
      "delivery-confirmed",
      "reseal-candidate",
      "resealed",
    ].includes(state.phase) ||
    (mode === "deliver" && (state.phase !== "prepared" || state.sequence !== 0))
  )
    fail();
  const clock = (forward = false) => {
    if (
      now() < time(plan.startsAt) ||
      now() >= time(forward ? plan.deliveryStopsAt : plan.recoveryStopsAt) ||
      signal?.aborted
    )
      fail();
  };
  const persist = async (phase, delta = {}) => {
    const next = seal({ ...structuredClone(state), ...delta, phase, sequence: state.sequence + 1 });
    await save(next);
    Object.assign(state, next);
  };
  let requests = 0;
  const api = Object.create(cf);
  api.admission = admission;
  api.request = async (route, init = {}, options = {}) => {
    clock(mode === "deliver");
    if (++requests > 400 || (init.method ?? "GET") !== "GET") fail();
    return cf.request(
      route,
      {
        ...init,
        redirect: "error",
        signal: AbortSignal.any([
          AbortSignal.timeout(Math.max(1, Math.min(15_000, time(plan.recoveryStopsAt) - now()))),
          ...(signal ? [signal] : []),
        ]),
      },
      { ...options, boundedBytes: 1024 * 1024 },
    );
  };
  api.list = async (route) => {
    const all = [];
    for (let page = 1; page <= 100; page++) {
      const reply = await api.request(
        `${route}${route.includes("?") ? "&" : "?"}page=${page}&per_page=100`,
        {},
        { envelope: true },
      );
      const items = reply.result?.items ?? reply.result,
        pages = reply.result_info?.total_pages;
      if (
        !Array.isArray(items) ||
        (pages !== undefined && (!Number.isSafeInteger(pages) || pages < page || pages > 100))
      )
        fail();
      all.push(...items);
      if (pages !== undefined ? page === pages : items.length < 100) return all;
    }
    fail();
  };
  const token = async () => {
    const value = await api.request("/tokens/verify");
    if (
      value.id !== plan.tokenId ||
      value.status !== "active" ||
      !(time(value.expires_on) >= time(plan.cycleRecoveryBy) + 900_000)
    )
      fail();
    return time(value.expires_on);
  };
  const fence = async (isolation, after = false) => {
    api.admission = readAdmission({
      CLOUDFLARE_ACCOUNT_ID: isolation.accountId,
      QUALIFICATION_ISOLATION_JSON: JSON.stringify(isolation),
      QUALIFICATION_ISOLATION_SHA256: digest(canonical(isolation)),
    });
    await verifyAuthority(api);
    const settings = await api.settings(authorityName);
    const settingsSha256 = digest(canonical(settings));
    // Bulk-secret delivery can change this Cloudflare provenance annotation.
    // Reconstruct ONLY the observed version_upload -> secret transition, then
    // compare the entire original settings digest. No other metadata is ignored.
    const beforeSettings = structuredClone(settings);
    if (after && beforeSettings.annotations?.["workers/triggered_by"] === "secret")
      beforeSettings.annotations["workers/triggered_by"] = "version_upload";
    if (
      (settingsSha256 !== plan.authoritySettingsSha256 &&
        (!after || digest(canonical(beforeSettings)) !== plan.authoritySettingsSha256)) ||
      (after &&
        state.phase !== "delivery-confirmed" &&
        settingsSha256 !== (state.afterSettingsSha256 ?? plan.authoritySettingsSha256)) ||
      new Set(settings.bindings.map((b) => b.name)).size !== settings.bindings.length ||
      secretNames.some(
        (name) =>
          !equal(
            settings.bindings.find((b) => b.name === name),
            { name, type: "secret_text" },
          ),
      ) ||
      !equal(
        await api.request(
          `/workers/scripts/${authorityName}/content/v2`,
          {},
          { contentIdentityBytes: 1024 * 1024 },
        ),
        plan.authorityModule,
      )
    )
      fail();
    const scripts = await api.list("/workers/scripts");
    const names = scripts.map((v) => v.id ?? v.name);
    if (
      names.length > 100 ||
      new Set(names).size !== names.length ||
      names.some((v) => !/^[a-zA-Z0-9_-]{1,63}$/.test(v ?? ""))
    )
      fail();
    for (const name of names)
      if (name !== authorityName) {
        const clientSettings = await api.settings(name);
        if (
          !Array.isArray(clientSettings.bindings) ||
          clientSettings.bindings.some(
            (b) =>
              b.service === authorityName ||
              b.script_name === authorityName ||
              isolation.authority.namespaces.some((n) => n.id === b.namespace_id),
          )
        )
          fail();
      }
    if (
      (await api.request(`/workers/services/${authorityName}`))?.default_environment
        ?.environment !== "production" ||
      (await api.latestVersion(authorityName)) !== isolation.authority.version
    )
      fail();
    return settingsSha256;
  };
  const run = async (argv, childEnv, input, preflight = {}) => {
    clock(mode === "deliver");
    if (preflight.signal?.aborted) fail();
    tools(plan);
    return command(argv, childEnv, input, {
      signal: preflight.signal ?? signal,
      importKeyDryRun: preflight.importKeyDryRun ?? false,
      timeout: Math.max(
        1,
        Math.min(
          preflight.timeout ?? 180_000,
          time(mode === "deliver" ? plan.deliveryStopsAt : plan.recoveryStopsAt) - now(),
        ),
      ),
      cwd: workdir,
    });
  };
  clock(mode === "deliver");
  tools(plan);
  if (state.phase !== "prepared" && state.beforeVersion !== plan.isolation.authority.version)
    fail();
  if (
    mode !== "deliver" &&
    ["delivery-confirmed", "reseal-candidate", "resealed"].includes(state.phase) &&
    !equal(readPrivateJSON(path.join(workdir, "session-issuance-intent.json")), {
      attemptId: plan.attemptId,
      planHash: digest(canonical(plan)),
    })
  )
    fail();
  if (mode === "deliver") {
    if (
      plan.runSession === undefined ||
      env.AWS_SESSION_TOKEN !== undefined ||
      !env.AWS_ACCESS_KEY_ID ||
      !env.AWS_SECRET_ACCESS_KEY ||
      !env.CLOUDFLARE_API_TOKEN
    )
      fail();
    const activationWait = time(plan.runSession.startsAt) - now();
    if (activationWait > 0) throw new SessionNotActive(Math.ceil(activationWait / 1000));
    const pre = await readProof("pre");
    verifyReplacementIdle(plan, pre, plan.isolation, now());
    custody(plan, await readCustody("pre"), now(), plan.isolation.authority.version, true);
    const tokenExpiry = await token();
    await fence(plan.isolation);
    await persist("issuance-intent", {
      beforeVersion: plan.isolation.authority.version,
      previousSessionExpiresAt: plan.previousSessionExpiresAt,
      unexpiredSessionReplacement: time(plan.previousSessionExpiresAt) > time(plan.startsAt),
    });
    // Separate exclusive marker survives a lost/recreated state journal. Copies
    // cannot choose a new destination: the reviewed plan binds this directory.
    claimIssuance(plan);
    // Only this trusted issuer chooses scope/tags/policy. Executors never get
    // its bootstrap key; the installed role cannot AssumeRole or TagSession.
    const sessionName = plan.runSession.runId;
    const assumed = JSON.parse(
      await run(
        [
          plan.tools.aws.path,
          "sts",
          "assume-role",
          "--role-arn",
          admission.aws.roleArn,
          "--role-session-name",
          sessionName,
          "--duration-seconds",
          "43200",
          "--tags",
          JSON.stringify(
            Object.entries(runSessionTags(plan.runSession)).map(([Key, Value]) => ({ Key, Value })),
          ),
          "--policy",
          JSON.stringify(runSessionPolicy(plan.runSession)),
          "--region",
          admission.aws.region,
          "--output",
          "json",
        ],
        awsEnv(
          {
            AWS_ACCESS_KEY_ID: env.AWS_ACCESS_KEY_ID,
            AWS_SECRET_ACCESS_KEY: env.AWS_SECRET_ACCESS_KEY,
          },
          admission.aws.region,
        ),
      ),
    );
    const c = assumed.Credentials,
      expiry = time(c?.Expiration);
    if (
      (assumed.PackedPolicySize !== undefined &&
        (!Number.isInteger(assumed.PackedPolicySize) ||
          assumed.PackedPolicySize > 100 ||
          assumed.PackedPolicySize < 0)) ||
      !(expiry - now() >= 10 * 3600_000 && expiry - now() <= 12 * 3600_000 + 60_000) ||
      expiry < time(plan.cycleRecoveryBy) + 3600_000 ||
      expiry > tokenExpiry ||
      [c?.AccessKeyId, c?.SecretAccessKey, c?.SessionToken].some((v) => typeof v !== "string" || !v)
    )
      fail();
    const session = Object.fromEntries(
      secretNames.map((name, i) => [name, [c.AccessKeyId, c.SecretAccessKey, c.SessionToken][i]]),
    );
    const identity = JSON.parse(
      await run(
        [
          plan.tools.aws.path,
          "sts",
          "get-caller-identity",
          "--region",
          admission.aws.region,
          "--output",
          "json",
        ],
        awsEnv(session, admission.aws.region),
      ),
    );
    const roleName = admission.aws.roleArn.split("/").at(-1);
    if (
      identity.Account !== admission.aws.accountId ||
      identity.Arn !==
        `arn:aws:sts::${admission.aws.accountId}:assumed-role/${roleName}/${sessionName}`
    )
      fail();
    const authorizationPreflight = await importKeyPreflight(
      plan,
      run,
      awsEnv(session, admission.aws.region),
      signal,
      now,
    );
    verifyReplacementIdle(plan, pre, plan.isolation, now());
    custody(plan, await readCustody("pre"), now(), plan.isolation.authority.version, true);
    await fence(plan.isolation);
    await token();
    await persist("delivery-intent", {
      authorizationPreflight,
      sessionExpiresAt: c.Expiration,
      runSessionSha256: digest(canonical(plan.runSession)),
      sessionPolicySha256: digest(canonical(runSessionPolicy(plan.runSession))),
    });
    // The documented bulk endpoint is the same operation used by Wrangler, but
    // without its Worker-not-found -> draft-Worker creation fallback, dotenv
    // loading or persistent debug logs. Exactly one request; never retry it.
    clock(true);
    tools(plan);
    const delivered = await cf.request(
      `/workers/scripts/${authorityName}/secrets-bulk`,
      {
        method: "PATCH",
        redirect: "error",
        headers: { "Content-Type": "application/merge-patch+json" },
        body: JSON.stringify({
          secrets: Object.fromEntries(
            secretNames.map((name) => [name, { name, type: "secret_text", text: session[name] }]),
          ),
        }),
        signal: AbortSignal.any([
          AbortSignal.timeout(Math.max(1, Math.min(15_000, time(plan.deliveryStopsAt) - now()))),
          ...(signal ? [signal] : []),
        ]),
      },
      { envelope: true, boundedBytes: 128 * 1024 },
    );
    if (delivered?.success !== true) fail();
    await persist("delivery-confirmed", { deliveryConfirmedAt: new Date(now()).toISOString() });
    return state;
  }
  if (recovery && state.recoverySourceSha === undefined)
    await persist(state.phase, { recoverySourceSha: recovery.sourceSha });
  // Reconciliation never issues credentials or repeats a delivery, even after a
  // lost successful response. Unknown delivery stays permanently unadmitted.
  await token();
  const observedVersion = await api.latestVersion(authorityName);
  if (
    !uuid.test(observedVersion ?? "") ||
    (state.afterVersion && state.afterVersion !== observedVersion)
  )
    fail();
  if (!["delivery-confirmed", "reseal-candidate", "resealed"].includes(state.phase)) {
    await persist(state.phase, { observedVersion, admissionDenied: true });
    return state;
  }
  if (
    observedVersion === state.beforeVersion ||
    !(time(state.sessionExpiresAt) >= time(plan.cycleRecoveryBy) + 3600_000)
  )
    fail();
  const isolation = structuredClone(plan.isolation);
  isolation.authority.version = observedVersion;
  const afterSettingsSha256 = await fence(isolation, true);
  if (state.phase === "delivery-confirmed")
    await persist("reseal-candidate", {
      afterVersion: observedVersion,
      afterSettingsSha256,
      candidateIsolation: isolation,
    });
  if (mode === "inspect") return state;
  const proof = await readProof("post");
  const observed = verifyReplacementIdle(plan, proof, isolation, now(), true, afterSettingsSha256);
  if (observed < time(state.deliveryConfirmedAt)) fail();
  custody(plan, await readCustody("post"), now(), observedVersion, false);
  const recoveryStartedAt = now();
  await run(
    [
      plan.tools.node.path,
      path.join(
        recovery ? path.join(recovery.originalRoot, "scripts") : directory,
        "image-qualification-control.mjs",
      ),
      "idle-controller-recover",
      path.join(workdir, "post-idle"),
    ],
    {
      ...safeEnv(),
      CLOUDFLARE_ACCOUNT_ID: admission.accountId,
      CLOUDFLARE_API_TOKEN: env.CLOUDFLARE_API_TOKEN,
      QUALIFICATION_IDLE_SOURCE_SHA: plan.sourceSha,
      QUALIFICATION_IDLE_PLAN_SHA256: digest(canonical(proof.ownerPlan)),
    },
  );
  const recovered = await readProof("post");
  verifyReplacementIdle(plan, recovered, isolation, now(), true, afterSettingsSha256);
  if (
    !equal(recovered.ownerPlan, proof.ownerPlan) ||
    recovered.state.sequence <= proof.state.sequence ||
    !(time(recovered.state.events.at(-1)?.at) >= recoveryStartedAt)
  )
    fail();
  await fence(isolation, true);
  await token();
  await persist("resealed", {
    afterVersion: observedVersion,
    afterSettingsSha256,
    candidateIsolation: isolation,
    postIdleProofSha256: digest(canonical(proof)),
    recoveryProofSha256: digest(canonical(recovered)),
    enrollmentAdmitted: false,
  });
  return state;
}

export async function replacementCommand(args, env = process.env) {
  if (
    !["deliver", "inspect", "reseal"].includes(args[0]) ||
    (args.length !== 2 &&
      !(args.length === 4 && args[0] !== "deliver" && args[2] === "--original-source"))
  )
    fail();
  const workdir = path.resolve(args[1]);
  const plan = readPrivateJSON(path.join(workdir, "session-replacement-plan.json"));
  if (workdir !== plan.recoveryDirectory) fail();
  const sourceSha = execFileSync("git", ["rev-parse", "HEAD"], {
    cwd: root,
    encoding: "utf8",
  }).trim();
  let recovery;
  if (args.length === 4) {
    const originalRoot = args[3];
    if (!path.isAbsolute(originalRoot) || fs.realpathSync(originalRoot) !== originalRoot) fail();
    const git = (cwd, argv) => execFileSync("git", argv, { cwd, encoding: "utf8" }).trim();
    recovery = {
      sourceSha,
      originalRoot,
      originalSourceSha: git(originalRoot, ["rev-parse", "HEAD"]),
      sources: Object.fromEntries(
        sourceFiles.map((name) => [
          name,
          digest(fs.readFileSync(path.join(originalRoot, "scripts", name))),
        ]),
      ),
    };
    replacementRecoverySources(plan, recovery);
    if (
      git(originalRoot, ["status", "--porcelain", "--untracked-files=all"]) ||
      git(root, [
        "diff",
        "--name-only",
        plan.sourceSha,
        sourceSha,
        "--",
        ".",
        ":(exclude)scripts/image-qualification-session-replacement.mjs",
        ":(exclude)scripts/image-qualification-session-replacement.test.mjs",
        ":(exclude)qualification/idle-session/README.md",
      ])
    )
      fail();
  }
  validateReplacementPlan(
    plan,
    recovery ? replacementRecoverySources(plan, recovery) : replacementSources(),
  );
  if (
    (!recovery && plan.sourceSha !== sourceSha) ||
    env.QUALIFICATION_SESSION_SOURCE_SHA !== sourceSha ||
    env.QUALIFICATION_SESSION_PLAN_SHA256 !== digest(canonical(plan)) ||
    env.CLOUDFLARE_ACCOUNT_ID !== plan.isolation.accountId ||
    !env.CLOUDFLARE_API_TOKEN ||
    fs.realpathSync(process.execPath) !== plan.tools.node.path ||
    execFileSync("git", ["status", "--porcelain", "--untracked-files=all"], {
      cwd: root,
      encoding: "utf8",
    }).trim()
  )
    fail();
  const stop = new AbortController(),
    cancel = () => stop.abort();
  process.on("SIGINT", cancel);
  process.on("SIGTERM", cancel);
  try {
    return await withQualificationJournal(
      workdir,
      {
        stateName: "session-replacement-state.json",
        lockName: "session-replacement.lock",
        create: args[0] === "deliver",
        initial: () => replacementInitialState(plan),
      },
      (state, save) =>
        replaceIdleSession(plan, state, {
          mode: args[0],
          cf: new Cloudflare(plan.isolation.accountId, env.CLOUDFLARE_API_TOKEN),
          save,
          env,
          signal: stop.signal,
          workdir,
          recovery,
          readCustody: (stage) => readPrivateJSON(path.join(workdir, `${stage}-custody.json`)),
          readProof: (stage) => ({
            ownerPlan: readPrivateJSON(
              path.join(workdir, `${stage}-idle`, "idle-controller-plan.json"),
            ),
            state: readPrivateJSON(
              path.join(workdir, `${stage}-idle`, "idle-controller-state.json"),
            ),
          }),
        }),
    );
  } finally {
    process.removeListener("SIGINT", cancel);
    process.removeListener("SIGTERM", cancel);
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  replacementCommand(process.argv.slice(2)).then(
    (state) =>
      console.log(`Session operation: ${state.phase}. Enrollment remains separately gated.`),
    (error) => {
      console.error(
        error instanceof SessionNotActive
          ? error.message
          : "Session operation stopped. Preserve the original intent; no blind retry. No raw diagnostic or credential was printed.",
      );
      process.exitCode = 1;
    },
  );
}
