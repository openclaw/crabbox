// Trusted operator composition for the existing cleanup owner. No new runtime,
// resource lifecycle, enrollment, IAM mutation, or forward credential route.
import fs from "node:fs";
import path from "node:path";
import { canonical, digest, authorityName } from "./image-qualification-isolation.mjs";
import {
  validateRunSession,
  runSessionTags,
  cleanupSessionPolicy,
} from "./image-qualification-run-session.mjs";
import { sessionChild } from "./image-qualification-session-replacement.mjs";
import { readPrivateJSON } from "./image-qualification-journal.mjs";

const same = (a, b) => canonical(a) === canonical(b);
const sha = /^[0-9a-f]{64}$/;
const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/;
const names = ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"];
const fail = () => {
  throw new Error("cleanup session refused; preserve original owner, intent and custody");
};
const stamp = (v) => (typeof v === "string" ? Date.parse(v) : NaN);

export function validateCleanupSession(plan, handoff) {
  const c = plan.cleanupSession;
  if (!c) fail();
  validateRunSession(c.scope);
  if (
    c.profile !== "same-owner-cleanup-session/v1" ||
    !sha.test(c.authoritySettingsSha256 ?? "") ||
    !sha.test(c.preCustodySha256 ?? "") ||
    !/^[0-9a-f]{32}$/.test(c.tokenId ?? "") ||
    !path.isAbsolute(c.directory ?? "") ||
    !path.isAbsolute(c.aws?.path ?? "") ||
    !sha.test(c.aws?.sha256 ?? "") ||
    !Number.isFinite(stamp(c.startsAt)) ||
    stamp(c.startsAt) < stamp(plan.handoffStartedAt) ||
    stamp(c.deliveryStopsAt) !== stamp(c.startsAt) + 20 * 60_000 ||
    stamp(c.deliveryStopsAt) > stamp(plan.deadline) - 60 * 60_000 ||
    c.scope.startsAt !== plan.handoffStartedAt ||
    c.scope.recoveryExpiresAt !== plan.deadline ||
    c.scope.expiresAt !== plan.binding.expiresAt ||
    c.scope.runId !== plan.binding.runId ||
    c.scope.candidateSha !== plan.binding.candidateSha
  )
    fail();
  if (
    handoff &&
    (handoff.mode !== "first-walk" ||
      [
        "repository",
        "workflowSha",
        "candidateSha",
        "runId",
        "candidateArtifactId",
        "candidateArtifactDigest",
        "manifestSha256",
        "startsAt",
        "workExpiresAt",
        "expiresAt",
      ].some((key) => handoff[key] !== c.scope[key]))
  )
    fail();
  return c;
}

// Only a reconciled record may change serving-version admission. Original
// caller ownership and every other authority coordinate remain pinned.
export function cleanupSessionAdmission(plan, state) {
  const next = structuredClone(plan.correctedAdmission),
    s = state.cleanupSession;
  if (!s) return next;
  validateCleanupSession(plan);
  if (
    !["issuance-intent", "delivery-intent", "delivery-confirmed", "reconciled"].includes(s.phase) ||
    s.beforeVersion !== next.authority.version ||
    s.scopeSha256 !== digest(canonical(plan.cleanupSession.scope)) ||
    s.policySha256 !== digest(canonical(cleanupSessionPolicy(plan.cleanupSession.scope)))
  )
    fail();
  if (s.phase === "reconciled") {
    if (
      !uuid.test(s.afterVersion ?? "") ||
      s.afterVersion === s.beforeVersion ||
      !sha.test(s.afterSettingsSha256 ?? "") ||
      !(stamp(s.sessionExpiresAt) >= stamp(plan.deadline) + 3600_000)
    )
      fail();
    next.authority.version = s.afterVersion;
  }
  return next;
}

// This trusted local assertion is not authenticated history. It is required
// alongside live owner/module/config/caller checks, never as their replacement.
function custody(plan, value, now, version, before) {
  const c = plan.cleanupSession;
  if (
    value?.profile !== c.profile ||
    value.runId !== plan.binding.runId ||
    value.sourceSha !== plan.sourceSha ||
    value.authorityVersion !== version ||
    value.exclusiveUntil !== plan.deadline ||
    ![
      "continuousExclusiveCustody",
      "noConcurrentWritersOrCallers",
      "noActiveWorkflowWriters",
      "noOtherSessionConsumers",
      "fallbackReady",
    ].every((key) => value[key] === true) ||
    !(0 <= now - stamp(value.observedAt) && now - stamp(value.observedAt) <= 900_000) ||
    (before && digest(canonical(value)) !== c.preCustodySha256)
  )
    fail();
}

const safe = () => ({ PATH: "/usr/bin:/bin", LANG: "C.UTF-8", CI: "true" });
const awsEnv = (credentials, region) => ({
  ...safe(),
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
const tool = (c) => {
  if (
    fs.realpathSync(c.aws.path) !== c.aws.path ||
    !fs.statSync(c.aws.path).isFile() ||
    digest(fs.readFileSync(c.aws.path)) !== c.aws.sha256
  )
    fail();
};

export async function manageCleanupSession(
  plan,
  state,
  {
    mode,
    cf,
    verify,
    observe,
    save,
    readCustody,
    workdir,
    now,
    signal,
    command = sessionChild,
    env = process.env,
  },
) {
  const c = validateCleanupSession(plan);
  const wall = now,
    started = now(),
    monotonic = performance.now();
  now = () => Math.max(wall(), started + Math.ceil(performance.now() - monotonic));
  if (
    !["deliver", "inspect"].includes(mode) ||
    workdir !== c.directory ||
    state.retired ||
    state.settled ||
    state.attestation ||
    (mode === "deliver" && state.cleanupSession) ||
    (mode === "inspect" &&
      !["delivery-intent", "delivery-confirmed", "reconciled"].includes(
        state.cleanupSession?.phase,
      ))
  )
    fail();
  const check = () => {
    if (
      signal?.aborted ||
      now() < stamp(c.startsAt) ||
      now() >= stamp(mode === "deliver" ? c.deliveryStopsAt : plan.deadline)
    )
      fail();
  };
  const persist = async (delta) => {
    state.cleanupSession = { ...state.cleanupSession, ...delta };
    await save();
  };
  const beforeVersion = plan.correctedAdmission.authority.version;
  const token = async () => {
    check();
    const value = await cf.request("/tokens/verify");
    if (
      value?.id !== c.tokenId ||
      value.status !== "active" ||
      !(stamp(value.expires_on) >= stamp(plan.deadline) + 900_000)
    )
      fail();
    return stamp(value.expires_on);
  };
  const fence = async (version, after) => {
    check();
    await verify(version);
    const settings = await cf.settings(authorityName),
      before = structuredClone(settings);
    if (after && before.annotations?.["workers/triggered_by"] === "secret")
      before.annotations["workers/triggered_by"] = "version_upload";
    if (
      digest(canonical(settings)) !== c.authoritySettingsSha256 &&
      (!after || digest(canonical(before)) !== c.authoritySettingsSha256)
    )
      fail();
    if (
      !Array.isArray(settings.bindings) ||
      new Set(settings.bindings.map((b) => b.name)).size !== settings.bindings.length ||
      names.some(
        (name) =>
          !same(
            settings.bindings.find((b) => b.name === name),
            { name, type: "secret_text" },
          ),
      )
    )
      fail();
    // Unknown consumers cannot inherit replacement credentials or namespaces.
    const scripts = await cf.list("/workers/scripts");
    if (
      !Array.isArray(scripts) ||
      scripts.length > 100 ||
      new Set(scripts.map((s) => s.id ?? s.name)).size !== scripts.length
    )
      fail();
    for (const script of scripts) {
      const name = script.id ?? script.name;
      if (!/^[A-Za-z0-9_-]{1,63}$/.test(name ?? "")) fail();
      if (name === authorityName || plan.callers.some((v) => v.name === name && !v.absent))
        continue;
      const other = await cf.settings(name);
      if (
        !Array.isArray(other.bindings) ||
        other.bindings.some(
          (b) =>
            b.service === authorityName ||
            b.script_name === authorityName ||
            plan.correctedAdmission.authority.namespaces.some((n) => n.id === b.namespace_id),
        )
      )
        fail();
    }
    await observe();
    check();
    return digest(canonical(settings));
  };
  check();
  if (mode === "inspect") {
    if (
      !same(readPrivateJSON(path.join(workdir, "cleanup-session-issuance.json")), {
        planSha256: state.planSha256,
      })
    )
      fail();
    const s = state.cleanupSession;
    const version = await cf.latestVersion(authorityName);
    if (
      !uuid.test(version ?? "") ||
      version === beforeVersion ||
      (s.afterVersion && s.afterVersion !== version) ||
      !(stamp(s.sessionExpiresAt) >= stamp(plan.deadline) + 3600_000)
    )
      fail();
    // No issuance or PATCH here. A lost response is reconciled only under the
    // same exclusive custody, complete live fences and original cleanup mode.
    custody(plan, await readCustody("post"), now(), version, false);
    await token();
    const afterSettingsSha256 = await fence(version, true);
    custody(plan, await readCustody("post"), now(), version, false);
    await persist({ phase: "reconciled", afterVersion: version, afterSettingsSha256 });
    return;
  }
  if (
    env.AWS_SESSION_TOKEN !== undefined ||
    !env.AWS_ACCESS_KEY_ID ||
    !env.AWS_SECRET_ACCESS_KEY ||
    !env.CLOUDFLARE_API_TOKEN
  )
    fail();
  custody(plan, await readCustody("pre"), now(), beforeVersion, true);
  const tokenExpiry = await token();
  await fence(beforeVersion, false);
  tool(c);
  await persist({
    phase: "issuance-intent",
    beforeVersion,
    scopeSha256: digest(canonical(c.scope)),
    policySha256: digest(canonical(cleanupSessionPolicy(c.scope))),
  });
  // A separate durable exclusive marker also survives lost/recreated journals.
  const marker = path.join(workdir, "cleanup-session-issuance.json");
  const fd = fs.openSync(
    marker,
    fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL | fs.constants.O_NOFOLLOW,
    0o600,
  );
  try {
    fs.writeFileSync(fd, canonical({ planSha256: state.planSha256 }));
    fs.fsyncSync(fd);
  } finally {
    fs.closeSync(fd);
  }
  const directory = fs.openSync(workdir, "r");
  try {
    fs.fsyncSync(directory);
  } finally {
    fs.closeSync(directory);
  }
  const run = async (args, credentials) => {
    check();
    tool(c);
    return JSON.parse(
      await command(
        [c.aws.path, "sts", ...args, "--region", cf.admission.aws.region, "--output", "json"],
        awsEnv(credentials, cf.admission.aws.region),
        undefined,
        {
          cwd: workdir,
          signal,
          timeout: Math.max(1, Math.min(180_000, stamp(c.deliveryStopsAt) - now())),
        },
      ),
    );
  };
  const role = cf.admission.aws.roleArn,
    account = cf.admission.aws.accountId;
  const assumed = await run(
    [
      "assume-role",
      "--role-arn",
      role,
      "--role-session-name",
      c.scope.runId,
      "--duration-seconds",
      "43200",
      "--tags",
      JSON.stringify(
        Object.entries(runSessionTags(c.scope)).map(([Key, Value]) => ({ Key, Value })),
      ),
      "--policy",
      JSON.stringify(cleanupSessionPolicy(c.scope)),
    ],
    { AWS_ACCESS_KEY_ID: env.AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY: env.AWS_SECRET_ACCESS_KEY },
  );
  const credentials = assumed.Credentials,
    expiry = stamp(credentials?.Expiration);
  if (
    (assumed.PackedPolicySize !== undefined &&
      (!Number.isInteger(assumed.PackedPolicySize) ||
        assumed.PackedPolicySize < 0 ||
        assumed.PackedPolicySize > 100)) ||
    !(expiry - now() >= 10 * 3600_000 && expiry - now() <= 12 * 3600_000 + 60_000) ||
    expiry < stamp(plan.deadline) + 3600_000 ||
    expiry > tokenExpiry ||
    [credentials?.AccessKeyId, credentials?.SecretAccessKey, credentials?.SessionToken].some(
      (v) => typeof v !== "string" || !v,
    )
  )
    fail();
  const session = Object.fromEntries(
    names.map((n, i) => [
      n,
      [credentials.AccessKeyId, credentials.SecretAccessKey, credentials.SessionToken][i],
    ]),
  );
  const identity = await run(["get-caller-identity"], session);
  if (
    identity.Account !== account ||
    identity.Arn !==
      `arn:aws:sts::${account}:assumed-role/${role.split("/").at(-1)}/${c.scope.runId}`
  )
    fail();
  custody(plan, await readCustody("pre"), now(), beforeVersion, true);
  await token();
  await fence(beforeVersion, false);
  await persist({ phase: "delivery-intent", sessionExpiresAt: credentials.Expiration });
  check();
  tool(c);
  const delivered = await cf.request(
    `/workers/scripts/${authorityName}/secrets-bulk`,
    {
      method: "PATCH",
      redirect: "error",
      headers: { "Content-Type": "application/merge-patch+json" },
      body: JSON.stringify({
        secrets: Object.fromEntries(
          names.map((name) => [name, { name, type: "secret_text", text: session[name] }]),
        ),
      }),
      signal: AbortSignal.any([
        AbortSignal.timeout(Math.max(1, Math.min(15_000, stamp(c.deliveryStopsAt) - now()))),
        ...(signal ? [signal] : []),
      ]),
    },
    { envelope: true, boundedBytes: 128 * 1024 },
  );
  if (delivered?.success !== true) fail();
  await persist({ phase: "delivery-confirmed" });
}
