import crypto from "node:crypto";
import { realpathSync } from "node:fs";
import { fileURLToPath } from "node:url";

function invalid() {
  throw new Error("invalid qualification executor credential");
}

// Public template: no credential is generated or exported by the workflow.
export function executorCredentialIdentity(env) {
  const expected = {
    version: 1,
    repositoryId: env.GITHUB_REPOSITORY_ID,
    workflowSha: env.QUALIFICATION_WORKFLOW_SHA,
    candidateSha: env.QUALIFICATION_CANDIDATE_SHA,
    runId: env.GITHUB_RUN_ID,
    runAttempt: env.GITHUB_RUN_ATTEMPT,
    handoffSha256: env.QUALIFICATION_HANDOFF_SHA256,
    expiresAt: env.QUALIFICATION_EXPIRES_AT,
  };
  const ref = `refs/heads/${env.QUALIFICATION_DEFAULT_BRANCH}`;
  if (
    !/^[1-9][0-9]*$/.test(expected.repositoryId ?? "") ||
    !/^[1-9][0-9]*$/.test(expected.runId ?? "") ||
    expected.runAttempt !== "1" ||
    !/^[0-9a-f]{40}$/.test(expected.workflowSha ?? "") ||
    !/^[0-9a-f]{40}$/.test(expected.candidateSha ?? "") ||
    !/^[0-9a-f]{64}$/.test(expected.handoffSha256 ?? "") ||
    !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/.test(expected.expiresAt ?? "") ||
    !Number.isFinite(Date.parse(expected.expiresAt)) ||
    new Date(expected.expiresAt).toISOString() !== expected.expiresAt ||
    !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(env.GITHUB_REPOSITORY ?? "") ||
    !env.QUALIFICATION_DEFAULT_BRANCH ||
    env.GITHUB_REF !== ref ||
    env.GITHUB_SHA !== expected.workflowSha ||
    env.GITHUB_EVENT_NAME !== "workflow_dispatch" ||
    env.GITHUB_WORKFLOW_REF !==
      `${env.GITHUB_REPOSITORY}/.github/workflows/image-qualification.yml@${ref}`
  )
    invalid();
  return expected;
}

export function maskExecutorSecret(value) {
  const escaped = value.replaceAll("%", "%25").replaceAll("\r", "%0D").replaceAll("\n", "%0A");
  process.stdout.write(`::add-mask::${escaped}\n`);
}

export function readExecutorCredential(
  env,
  { now = Date.now(), mask = maskExecutorSecret, expectedDigest } = {},
) {
  const expected = executorCredentialIdentity(env);
  let supplied;
  const raw = env.QUALIFICATION_EXECUTOR_CREDENTIAL;
  if (typeof raw !== "string" || !raw || Buffer.byteLength(raw) > 4096) invalid();
  // The secret is one opaque line; register it before decoding any sensitive value.
  mask(raw);
  if (!/^[A-Za-z0-9_-]+$/.test(raw)) invalid();
  const decoded = Buffer.from(raw, "base64url");
  if (decoded.toString("base64url") !== raw) invalid();
  try {
    supplied = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(decoded));
  } catch {
    invalid();
  }
  if (!supplied || typeof supplied !== "object" || Array.isArray(supplied)) invalid();
  if (typeof supplied.token !== "string" || !/^[0-9a-f]{64}$/.test(supplied.token)) invalid();
  // Also register the decoded token before validating or using it.
  mask(supplied.token);
  const keys = Object.keys(supplied).sort();
  if (
    keys.join(",") !== [...Object.keys(expected), "token"].sort().join(",") ||
    Object.entries(expected).some(([key, value]) => supplied[key] !== value) ||
    !Number.isFinite(now) ||
    Date.parse(expected.expiresAt) <= now ||
    Date.parse(expected.expiresAt) - now > 120 * 60_000
  )
    invalid();
  const tokenSha256 = crypto.createHash("sha256").update(supplied.token).digest("hex");
  if (
    expectedDigest !== undefined &&
    (!/^[0-9a-f]{64}$/.test(expectedDigest) || expectedDigest !== tokenSha256)
  )
    invalid();
  return { token: supplied.token, tokenSha256 };
}

export function executorEnvironment(env, token) {
  const child = { ...env, QUALIFICATION_EXECUTOR_TOKEN: token };
  for (const key of Object.keys(child)) {
    if (/^(ACTIONS_|GITHUB_TOKEN$|GH_TOKEN$|QUALIFICATION_EXECUTOR_CREDENTIAL$)/.test(key))
      delete child[key];
  }
  for (const name of [
    "AWS_ACCESS_KEY_ID",
    "AWS_SECRET_ACCESS_KEY",
    "AWS_SESSION_TOKEN",
    "CLOUDFLARE_API_TOKEN",
    "QUALIFICATION_CONTROLLER_TOKEN",
    "QUALIFICATION_ADMIN_TOKEN",
    "QUALIFICATION_SHARED_TOKEN",
  ]) {
    if (Object.hasOwn(child, name)) invalid();
  }
  return child;
}

export function executorToken(env) {
  const relay = new URL(env.QUALIFICATION_RELAY_URL);
  if (
    relay.protocol !== "https:" ||
    relay.username ||
    relay.password ||
    relay.port ||
    relay.pathname !== "/" ||
    relay.search ||
    relay.hash ||
    !/^crabbox-image-qualification-relay-[0-9]+-1\.[a-z0-9-]+\.workers\.dev$/.test(
      relay.hostname,
    ) ||
    !relay.hostname.startsWith(`crabbox-image-qualification-relay-${env.GITHUB_RUN_ID}-1.`) ||
    !/^[0-9a-f]{64}$/.test(env.QUALIFICATION_EXECUTOR_TOKEN_SHA256 ?? "")
  )
    invalid();
  return readExecutorCredential(env, { expectedDigest: env.QUALIFICATION_EXECUTOR_TOKEN_SHA256 })
    .token;
}

if (process.argv[1] && realpathSync(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const token = executorToken(process.env);
    process.execve(
      "/bin/bash",
      ["bash", fileURLToPath(new URL("./image-qualification-execute.sh", import.meta.url))],
      executorEnvironment(process.env, token),
    );
  } catch {
    console.error("qualification executor authentication failed");
    process.exitCode = 1;
  }
}
