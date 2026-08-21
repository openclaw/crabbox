#!/usr/bin/env node

import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { canonicalJSON, digestBytes } from "./aws-image-candidate.mjs";

const outcomes = new Set([
  "evidence_rejected",
  "precondition_failed",
  "promoted_verified",
  "verification_failed",
  "rolled_back",
  "rollback_precondition_failed",
  "outcome_unknown",
]);
const forbiddenKey =
  /(?:rawError|error|host|hostname|endpoint|token|leaseId|privatePath|workRoot|secret|password|authorization|cookie|credential)/i;
const privateValue =
  /(?:\/Users\/|\/home\/|\/private\/|https?:\/\/(?:localhost|127\.0\.0\.1|\[::1\])|(?:^|[^0-9])(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3})(?:$|[^0-9])|[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}|(?:^|[./_-])(?:internal|intranet|corp|private)(?:[./_-]|$)|\.(?:internal|local)(?:[/:]|$))/i;

function fail(message) {
  throw new Error(message);
}

function options(argv) {
  const parsed = {};
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!name?.startsWith("--") || value === undefined) fail(`invalid argument ${name ?? ""}`);
    parsed[name.slice(2)] = value;
  }
  return parsed;
}

function assertPrivacy(value, label = "receipt") {
  if (Array.isArray(value)) {
    value.forEach((item, index) => assertPrivacy(item, `${label}[${index}]`));
    return;
  }
  if (!value || typeof value !== "object") {
    if (typeof value === "string" && privateValue.test(value))
      fail(`${label} contains private data`);
    return;
  }
  for (const [key, item] of Object.entries(value)) {
    if (forbiddenKey.test(key)) fail(`${label} contains forbidden field ${key}`);
    assertPrivacy(item, `${label}.${key}`);
  }
}

function assertState(value, label) {
  if (value?.state === "absent" && Object.keys(value).length === 1) return;
  if (
    value?.state === "present" &&
    /^ami-[0-9a-z]+$/.test(value.imageId ?? "") &&
    typeof value.revision === "string" &&
    value.revision.length > 0 &&
    Object.keys(value).every((key) => ["state", "imageId", "revision"].includes(key))
  )
    return;
  fail(`invalid ${label}`);
}

function assertMutation(value, outcome) {
  if (
    !value ||
    typeof value !== "object" ||
    !["pending", "succeeded", "rejected", "unknown"].includes(value.status) ||
    typeof value.applied !== "boolean" ||
    Object.keys(value).some((key) => !["status", "applied", "code"].includes(key)) ||
    (value.code !== undefined && !/^[a-z0-9_]{1,64}$/.test(value.code))
  ) {
    fail("invalid authoritative mutation state");
  }
  const appliedOutcomes = ["promoted_verified", "verification_failed", "rolled_back"];
  if (
    value.applied !== appliedOutcomes.includes(outcome) ||
    value.applied !== (value.status === "succeeded")
  ) {
    fail("mutation application state does not match terminal outcome");
  }
}

function assertClosed(value, keys, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail(`invalid ${label}`);
  if (Object.keys(value).some((key) => !keys.includes(key))) fail(`invalid ${label}`);
}

function projectTarget(value, label) {
  if (value?.state === "absent") {
    assertClosed(value, ["state"], label);
    return { state: "absent" };
  }
  assertClosed(value, ["state", "imageId", "accountId", "snapshotIds"], label);
  if (
    value?.state !== "present" ||
    !/^ami-[0-9a-z]+$/.test(value.imageId ?? "") ||
    !/^\d{12}$/.test(value.accountId ?? "") ||
    !Array.isArray(value.snapshotIds) ||
    value.snapshotIds.length === 0 ||
    value.snapshotIds.some((snapshot) => !/^snap-[0-9a-z]+$/.test(snapshot))
  )
    fail(`invalid ${label}`);
  return {
    state: "present",
    imageId: value.imageId,
    accountId: value.accountId,
    snapshotIds: [...value.snapshotIds],
  };
}

function assertBoundState(value, label) {
  if (value?.state === "absent") {
    assertClosed(value, ["state"], label);
    return;
  }
  assertClosed(value, ["state", "imageId", "revision", "accountId", "snapshotIds"], label);
  if (
    value?.state !== "present" ||
    !/^ami-[0-9a-z]+$/.test(value.imageId ?? "") ||
    typeof value.revision !== "string" ||
    value.revision.length === 0 ||
    !/^\d{12}$/.test(value.accountId ?? "") ||
    !Array.isArray(value.snapshotIds) ||
    value.snapshotIds.length === 0 ||
    value.snapshotIds.some((snapshot) => !/^snap-[0-9a-z]+$/.test(snapshot))
  )
    fail(`invalid ${label}`);
}

function projectEvidence(value) {
  assertClosed(
    value,
    ["schema", "qualification", "candidate", "source", "workflow", "scope", "desired"],
    "evidence",
  );
  assertClosed(
    value.qualification,
    ["reference", "digest", "receiptDigest", "inputDigest", "signatureDigest"],
    "qualification evidence",
  );
  assertClosed(value.candidate, ["reference", "digest", "recordDigest"], "candidate evidence");
  assertClosed(value.source, ["repository", "commit"], "source evidence");
  assertClosed(value.workflow, ["identity", "runId", "runAttempt"], "workflow evidence");
  assertClosed(
    value.scope,
    [
      "provider",
      "target",
      "region",
      "instanceType",
      "architecture",
      "os",
      "profile",
      "recipeDigest",
    ],
    "scope evidence",
  );
  assertClosed(value.desired, ["imageId", "accountId", "snapshotIds"], "desired evidence");
  return {
    schema: value.schema,
    qualification: { ...value.qualification },
    candidate: { ...value.candidate },
    source: { ...value.source },
    workflow: { ...value.workflow },
    scope: { ...value.scope },
    desired: { ...value.desired, snapshotIds: [...value.desired.snapshotIds] },
  };
}

function projectVerification(value) {
  assertClosed(value, ["status", "codes", "image"], "verification");
  if (!["passed", "failed"].includes(value.status) || !Array.isArray(value.codes))
    fail("invalid verification");
  const projected = { status: value.status, codes: [...value.codes] };
  if (value.image !== undefined) {
    assertClosed(
      value.image,
      ["id", "provider", "source", "region", "revision"],
      "verification image",
    );
    projected.image = { ...value.image };
  }
  return projected;
}

function projectCleanup(value) {
  assertClosed(value, ["status", "code"], "cleanup");
  if (!["passed", "failed"].includes(value.status)) fail("invalid cleanup");
  return value.code === undefined
    ? { status: value.status }
    : { status: value.status, code: value.code };
}

function receiptFromAttempt(attempt, bytes) {
  assertClosed(
    attempt,
    [
      "schema",
      "idempotencyKey",
      "inputDigest",
      "operation",
      "phase",
      "version",
      "claimVersion",
      "outcome",
      "expected",
      "before",
      "beforeBinding",
      "after",
      "desired",
      "rollbackTarget",
      "evidence",
      "workflow",
      "mutation",
      "verification",
      "cleanup",
      "createdAt",
      "updatedAt",
    ],
    "attempt",
  );
  if (
    attempt?.schema !== "crabbox-image-promotion-attempt/v1" ||
    attempt.phase !== "completed" ||
    !outcomes.has(attempt.outcome) ||
    !["promote", "rollback"].includes(attempt.operation) ||
    !/^[0-9a-f]{64}$/.test(attempt.inputDigest ?? "")
  ) {
    fail("attempt is not an authoritative terminal promotion record");
  }
  assertState(attempt.expected, "expected state");
  assertState(attempt.before, "before state");
  assertBoundState(attempt.beforeBinding, "before binding");
  if (!Number.isInteger(attempt.claimVersion) || attempt.claimVersion < 0) {
    fail("invalid attempt claim version");
  }
  if (attempt.after !== undefined) assertState(attempt.after, "after state");
  assertMutation(attempt.mutation, attempt.outcome);
  const desired = projectTarget(attempt.desired, "desired target");
  const rollbackTarget =
    attempt.rollbackTarget === undefined
      ? undefined
      : projectTarget(attempt.rollbackTarget, "rollback target");
  const evidence = projectEvidence(attempt.evidence);
  const verification =
    attempt.verification === undefined ? undefined : projectVerification(attempt.verification);
  const cleanup = attempt.cleanup === undefined ? undefined : projectCleanup(attempt.cleanup);
  if (
    ["promoted_verified", "rolled_back", "verification_failed", "outcome_unknown"].includes(
      attempt.outcome,
    ) &&
    attempt.after === undefined
  ) {
    fail("terminal mutation outcome requires after state");
  }
  assertPrivacy(attempt, "attempt");
  return {
    schema: "crabbox-aws-image-promotion-receipt/v1",
    createdAt: attempt.updatedAt,
    attemptDigest: digestBytes(bytes),
    idempotencyKey: attempt.idempotencyKey,
    operation: attempt.operation,
    outcome: attempt.outcome,
    scope: attempt.evidence.scope,
    expected: attempt.expected,
    before: attempt.before,
    ...(attempt.after ? { after: attempt.after } : {}),
    desired,
    ...(rollbackTarget ? { rollbackTarget } : {}),
    evidence,
    workflow: { runId: attempt.workflow.runId, runAttempt: attempt.workflow.runAttempt },
    mutation: {
      status: attempt.mutation.status,
      applied: attempt.mutation.applied,
      ...(attempt.mutation.code ? { code: attempt.mutation.code } : {}),
    },
    ...(verification ? { verification } : {}),
    ...(cleanup ? { cleanup } : {}),
  };
}

async function build(input) {
  const attemptFile = input.attempt;
  const output = input.output;
  if (!attemptFile || !output) fail("build requires --attempt and --output");
  const bytes = await readFile(attemptFile);
  const attempt = JSON.parse(bytes);
  const receipt = receiptFromAttempt(attempt, bytes);
  await writeFile(output, `${canonicalJSON(receipt)}\n`, { mode: 0o600 });
  process.stdout.write(
    `${canonicalJSON({ receiptDigest: digestBytes(Buffer.from(`${canonicalJSON(receipt)}\n`)) })}\n`,
  );
}

async function verify(input) {
  if (!input.file) fail("verify requires --file");
  const bytes = await readFile(input.file);
  const receipt = JSON.parse(bytes);
  if (
    receipt?.schema !== "crabbox-aws-image-promotion-receipt/v1" ||
    !outcomes.has(receipt.outcome)
  ) {
    fail("unsupported promotion receipt");
  }
  assertClosed(
    receipt,
    [
      "schema",
      "createdAt",
      "attemptDigest",
      "idempotencyKey",
      "operation",
      "outcome",
      "scope",
      "expected",
      "before",
      "after",
      "desired",
      "rollbackTarget",
      "evidence",
      "workflow",
      "mutation",
      "verification",
      "cleanup",
    ],
    "receipt",
  );
  assertState(receipt.expected, "expected state");
  assertState(receipt.before, "before state");
  if (receipt.after !== undefined) assertState(receipt.after, "after state");
  assertMutation(receipt.mutation, receipt.outcome);
  assertClosed(
    receipt.scope,
    [
      "provider",
      "target",
      "region",
      "instanceType",
      "architecture",
      "os",
      "profile",
      "recipeDigest",
    ],
    "scope",
  );
  assertClosed(receipt.workflow, ["runId", "runAttempt"], "workflow");
  projectTarget(receipt.desired, "desired target");
  if (receipt.rollbackTarget !== undefined)
    projectTarget(receipt.rollbackTarget, "rollback target");
  projectEvidence(receipt.evidence);
  if (receipt.verification !== undefined) projectVerification(receipt.verification);
  if (receipt.cleanup !== undefined) projectCleanup(receipt.cleanup);
  assertPrivacy(receipt);
  process.stdout.write(
    `${canonicalJSON({ receiptDigest: digestBytes(bytes), outcome: receipt.outcome })}\n`,
  );
}

async function main() {
  const [command, ...argv] = process.argv.slice(2);
  if (command === "build") return build(options(argv));
  if (command === "verify") return verify(options(argv));
  fail("usage: aws-image-promotion-receipt.mjs <build|verify> [options]");
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === path.resolve(fileURLToPath(import.meta.url))
) {
  main().catch((error) => {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  });
}
