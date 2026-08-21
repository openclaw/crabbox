#!/usr/bin/env node

import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const phases = new Set(["pending", "mutating", "awaiting_verification", "verifying", "completed"]);
const attemptKeys = [
  "schema",
  "idempotencyKey",
  "inputDigest",
  "operation",
  "phase",
  "version",
  "claimVersion",
  "claim",
  "outcome",
  "expected",
  "before",
  "beforeBinding",
  "after",
  "plannedAfter",
  "desired",
  "rollbackTarget",
  "evidence",
  "workflow",
  "mutation",
  "verification",
  "cleanup",
  "pendingVerification",
  "createdAt",
  "updatedAt",
];
const responseKeys = ["image", "attempt", "error", "message", "expected", "current", "scope"];

function fail(message) {
  throw new Error(message);
}

function options(argv) {
  const parsed = {};
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!name?.startsWith("--") || value === undefined) fail(`invalid argument ${name ?? ""}`);
    const key = name.slice(2);
    if (Object.hasOwn(parsed, key)) fail(`duplicate argument --${key}`);
    parsed[key] = value;
  }
  return parsed;
}

function assertClosed(value, allowed, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail(`invalid ${label}`);
  if (Object.keys(value).some((key) => !allowed.includes(key))) fail(`invalid ${label}`);
}

function assertState(value, label) {
  if (value?.state === "absent" && Object.keys(value).length === 1) return;
  if (
    value?.state === "present" &&
    typeof value.imageId === "string" &&
    typeof value.revision === "string" &&
    value.imageId.length > 0 &&
    value.revision.length > 0 &&
    Object.keys(value).every((key) => ["state", "imageId", "revision"].includes(key))
  )
    return;
  fail(`invalid ${label}`);
}

function assertTarget(value, label) {
  if (value?.state === "absent" && Object.keys(value).length === 1) return;
  assertClosed(value, ["state", "imageId", "accountId", "snapshotIds"], label);
  if (
    value.state !== "present" ||
    !/^ami-[0-9a-z]+$/.test(value.imageId ?? "") ||
    !/^\d{12}$/.test(value.accountId ?? "") ||
    !Array.isArray(value.snapshotIds) ||
    value.snapshotIds.length === 0 ||
    value.snapshotIds.some((snapshot) => !/^snap-[0-9a-z]+$/.test(snapshot))
  ) {
    fail(`invalid ${label}`);
  }
}

function assertBoundState(value, label) {
  if (value?.state === "absent" && Object.keys(value).length === 1) return;
  assertClosed(value, ["state", "imageId", "revision", "accountId", "snapshotIds"], label);
  if (
    value.state !== "present" ||
    !/^ami-[0-9a-z]+$/.test(value.imageId ?? "") ||
    typeof value.revision !== "string" ||
    value.revision.length === 0 ||
    !/^\d{12}$/.test(value.accountId ?? "") ||
    !Array.isArray(value.snapshotIds) ||
    value.snapshotIds.length === 0 ||
    value.snapshotIds.some((snapshot) => !/^snap-[0-9a-z]+$/.test(snapshot))
  ) {
    fail(`invalid ${label}`);
  }
}

function assertEvidence(value) {
  assertClosed(
    value,
    ["schema", "qualification", "candidate", "source", "workflow", "scope", "desired"],
    "promotion evidence",
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
  const digests = [
    value.qualification.digest,
    value.qualification.receiptDigest,
    value.qualification.inputDigest,
    value.qualification.signatureDigest,
    value.candidate.digest,
    value.candidate.recordDigest,
    value.scope.recipeDigest,
  ];
  if (
    value.schema !== "crabbox-image-promotion-evidence/v1" ||
    !/^ghcr\.io\/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$/.test(value.qualification.reference ?? "") ||
    !/^ghcr\.io\/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$/.test(value.candidate.reference ?? "") ||
    digests.some((digest) => !/^sha256:[0-9a-f]{64}$/.test(digest ?? "")) ||
    !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(value.source.repository ?? "") ||
    !/^[0-9a-f]{40}$/.test(value.source.commit ?? "") ||
    !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/\.github\/workflows\/devtools-image-qualify\.yml@refs\/heads\/[A-Za-z0-9._/-]+$/.test(
      value.workflow.identity ?? "",
    ) ||
    !value.workflow.identity.startsWith(`${value.source.repository}/`) ||
    !/^[1-9][0-9]*$/.test(value.workflow.runId ?? "") ||
    !/^[1-9][0-9]*$/.test(value.workflow.runAttempt ?? "") ||
    value.scope.provider !== "aws" ||
    value.scope.target !== "linux" ||
    !/^[a-z]{2}(?:-[a-z0-9]+)+-\d$/.test(value.scope.region ?? "") ||
    !/^[a-z0-9][a-z0-9.-]{0,63}$/.test(value.scope.instanceType ?? "") ||
    !["x86_64", "arm64"].includes(value.scope.architecture) ||
    !/^ubuntu:\d{2}\.\d{2}$/.test(value.scope.os ?? "") ||
    !/^[a-z0-9][a-z0-9._-]{0,63}$/.test(value.scope.profile ?? "") ||
    !/^ami-[0-9a-z]+$/.test(value.desired.imageId ?? "") ||
    !/^\d{12}$/.test(value.desired.accountId ?? "") ||
    !Array.isArray(value.desired.snapshotIds) ||
    value.desired.snapshotIds.length === 0 ||
    value.desired.snapshotIds.some((snapshot) => !/^snap-[0-9a-z]+$/.test(snapshot))
  ) {
    fail("invalid promotion evidence");
  }
}

function assertVerification(value) {
  assertClosed(value, ["status", "codes", "image"], "promotion verification");
  if (!["passed", "failed"].includes(value.status) || !Array.isArray(value.codes)) {
    fail("invalid promotion verification");
  }
  if (value.codes.some((code) => !/^[a-z0-9_]{1,64}$/.test(code))) {
    fail("invalid promotion verification");
  }
  if (value.image !== undefined) {
    assertClosed(
      value.image,
      ["id", "provider", "source", "region", "revision"],
      "promotion verification image",
    );
    if (
      !/^ami-[0-9a-z]+$/.test(value.image.id ?? "") ||
      value.image.provider !== "aws" ||
      value.image.source !== "promoted" ||
      typeof value.image.region !== "string" ||
      typeof value.image.revision !== "string"
    ) {
      fail("invalid promotion verification image");
    }
  }
}

function assertCleanup(value) {
  assertClosed(value, ["status", "code"], "promotion cleanup");
  if (
    !["passed", "failed"].includes(value.status) ||
    (value.code !== undefined && !/^[a-z0-9_]{1,64}$/.test(value.code))
  ) {
    fail("invalid promotion cleanup");
  }
}

function assertVerificationResult(value) {
  assertClosed(
    value,
    ["outcome", "status", "verification", "cleanup"],
    "pending promotion verification",
  );
  if (
    !["promoted_verified", "verification_failed", "rolled_back"].includes(value.outcome) ||
    ![200, 409].includes(value.status)
  ) {
    fail("invalid pending promotion verification");
  }
  assertVerification(value.verification);
  assertCleanup(value.cleanup);
}

function assertAttempt(value) {
  assertClosed(value, attemptKeys, "promotion attempt");
  if (
    value.schema !== "crabbox-image-promotion-attempt/v1" ||
    !phases.has(value.phase) ||
    !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$/.test(value.idempotencyKey ?? "") ||
    !/^[0-9a-f]{64}$/.test(value.inputDigest ?? "") ||
    !["promote", "rollback"].includes(value.operation) ||
    !Number.isInteger(value.version) ||
    !Number.isInteger(value.claimVersion) ||
    (value.outcome !== undefined &&
      ![
        "evidence_rejected",
        "precondition_failed",
        "promoted_verified",
        "verification_failed",
        "rolled_back",
        "rollback_precondition_failed",
        "outcome_unknown",
      ].includes(value.outcome))
  ) {
    fail("invalid promotion attempt");
  }
  assertState(value.expected, "expected state");
  assertState(value.before, "before state");
  if (value.after !== undefined) assertState(value.after, "after state");
  assertBoundState(value.beforeBinding, "before binding");
  if (value.plannedAfter !== undefined) assertBoundState(value.plannedAfter, "planned after");
  assertTarget(value.desired, "desired target");
  if (value.rollbackTarget !== undefined) assertTarget(value.rollbackTarget, "rollback target");
  assertEvidence(value.evidence);
  assertClosed(value.workflow, ["runId", "runAttempt"], "promotion workflow");
  if (
    !/^[1-9][0-9]*$/.test(value.workflow.runId ?? "") ||
    !/^[1-9][0-9]*$/.test(value.workflow.runAttempt ?? "")
  ) {
    fail("invalid promotion workflow");
  }
  assertClosed(value.mutation, ["status", "applied", "code"], "promotion mutation");
  if (
    !["pending", "succeeded", "rejected", "unknown"].includes(value.mutation.status) ||
    typeof value.mutation.applied !== "boolean" ||
    (value.mutation.code !== undefined && !/^[a-z0-9_]{1,64}$/.test(value.mutation.code))
  ) {
    fail("invalid promotion mutation");
  }
  if (value.claim !== undefined) {
    assertClosed(value.claim, ["id", "version", "claimedAt", "expiresAt"], "promotion claim");
    if (
      typeof value.claim.id !== "string" ||
      !Number.isInteger(value.claim.version) ||
      !Number.isFinite(Date.parse(value.claim.claimedAt ?? "")) ||
      !Number.isFinite(Date.parse(value.claim.expiresAt ?? ""))
    ) {
      fail("invalid promotion claim");
    }
  }
  if (value.verification !== undefined) assertVerification(value.verification);
  if (value.cleanup !== undefined) assertCleanup(value.cleanup);
  if (value.pendingVerification !== undefined) {
    assertVerificationResult(value.pendingVerification);
  }
}

function assertImage(value) {
  assertClosed(
    value,
    ["id", "state", "provider", "target", "region", "revision"],
    "promoted image",
  );
  if (
    typeof value.id !== "string" ||
    value.state !== "available" ||
    value.provider !== "aws" ||
    value.target !== "linux" ||
    typeof value.region !== "string" ||
    typeof value.revision !== "string"
  ) {
    fail("invalid promoted image");
  }
}

function projectedResponse(value) {
  assertClosed(value, responseKeys, "coordinator response");
  if (!value.attempt) {
    const code =
      typeof value.error === "string" && /^[a-z0-9_]{1,64}$/.test(value.error)
        ? value.error
        : "request_failed";
    fail(`coordinator request failed (${code})`);
  }
  assertAttempt(value.attempt);
  if (value.image !== undefined) assertImage(value.image);
  return {
    ...(value.image ? { image: value.image } : {}),
    attempt: value.attempt,
  };
}

function coordinatorConfig() {
  const endpoint = new URL(process.env.CRABBOX_COORDINATOR || "");
  const localHTTP =
    endpoint.protocol === "http:" && ["localhost", "127.0.0.1", "::1"].includes(endpoint.hostname);
  if (endpoint.protocol !== "https:" && !localHTTP) fail("invalid coordinator URL");
  const token = process.env.CRABBOX_COORDINATOR_PROMOTION_TOKEN || "";
  if (!token) fail("promotion token is required");
  return { endpoint, token };
}

async function requestJSON(method, route, body) {
  const { endpoint, token } = coordinatorConfig();
  const response = await fetch(new URL(route, endpoint), {
    method,
    headers: {
      authorization: `Bearer ${token}`,
      ...(body === undefined ? {} : { "content-type": "application/json" }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    signal: AbortSignal.timeout(30_000),
  });
  const value = await response.json().catch(() => fail("coordinator returned invalid JSON"));
  return { status: response.status, value };
}

function pollConfig() {
  const polls = Number.parseInt(process.env.CRABBOX_PROMOTION_CLIENT_POLLS || "12", 10);
  const delay = Number.parseInt(process.env.CRABBOX_PROMOTION_CLIENT_POLL_MS || "5000", 10);
  if (!Number.isInteger(polls) || polls < 1 || polls > 120) fail("invalid poll count");
  if (!Number.isInteger(delay) || delay < 0 || delay > 60_000) fail("invalid poll delay");
  return { polls, delay };
}

async function settleAttempt(initial, idempotencyKey) {
  let result = projectedResponse(initial);
  const { polls, delay } = pollConfig();
  for (let index = 0; index < polls; index += 1) {
    if (["awaiting_verification", "completed"].includes(result.attempt.phase)) return result;
    if (!["pending", "mutating", "verifying"].includes(result.attempt.phase)) {
      fail("coordinator returned an invalid pending phase");
    }
    if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
    const recovered = await requestJSON(
      "POST",
      `/v1/image-promotions/${encodeURIComponent(idempotencyKey)}/recover`,
    );
    result = projectedResponse(recovered.value);
  }
  fail(`promotion attempt remained pending in phase ${result.attempt.phase}`);
}

function expectedState(input) {
  const image = input["expected-current-image"];
  const revision = input["expected-current-revision"] || "";
  if (image === "none" && !revision) return { state: "absent" };
  if (typeof image === "string" && image.length > 0 && revision) {
    return { state: "present", imageId: image, revision };
  }
  fail("invalid expected image state");
}

async function mutate(input) {
  const evidence = JSON.parse(await readFile(input.evidence, "utf8"));
  assertEvidence(evidence);
  if (evidence.qualification.reference !== input["qualification-ref"]) {
    fail("qualification reference does not match evidence");
  }
  if (!["promote", "rollback"].includes(input.operation)) fail("invalid operation");
  if (!/^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$/.test(input["idempotency-key"] ?? "")) {
    fail("invalid idempotency key");
  }
  if (
    !/^[1-9][0-9]*$/.test(input["workflow-run-id"] ?? "") ||
    !/^[1-9][0-9]*$/.test(input["workflow-run-attempt"] ?? "")
  ) {
    fail("invalid workflow identity");
  }
  const body = {
    schema: "crabbox-image-promotion-request/v1",
    operation: input.operation,
    expected: expectedState(input),
    evidence,
    idempotencyKey: input["idempotency-key"],
    workflowRunId: input["workflow-run-id"],
    workflowRunAttempt: input["workflow-run-attempt"],
  };
  const response = await requestJSON("POST", "/v1/image-promotions", body);
  return settleAttempt(response.value, body.idempotencyKey);
}

async function recover(input) {
  const key = input["idempotency-key"];
  if (!/^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$/.test(key ?? "")) {
    fail("invalid idempotency key");
  }
  const response = await requestJSON(
    "POST",
    `/v1/image-promotions/${encodeURIComponent(key)}/recover`,
  );
  return settleAttempt(response.value, key);
}

async function verify(input) {
  const key = input["idempotency-key"];
  const leaseId = input["lease-id"];
  const failureCode = input["failure-code"];
  if (!/^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$/.test(key ?? "")) {
    fail("invalid idempotency key");
  }
  if (!!leaseId === !!failureCode) fail("verify requires exactly one lease or failure code");
  if (leaseId && !/^cbx_[a-z0-9]+$/.test(leaseId)) fail("invalid verification lease");
  if (
    failureCode &&
    ![
      "lease_start_failed",
      "lease_id_unavailable",
      "probe_not_run",
      "probe_command_failed",
    ].includes(failureCode)
  ) {
    fail("invalid verification failure code");
  }
  const body = leaseId
    ? { schema: "crabbox-image-promotion-verification/v1", leaseId }
    : {
        schema: "crabbox-image-promotion-verification/v1",
        probeStatus: "failed",
        failureCode,
      };
  const response = await requestJSON(
    "POST",
    `/v1/image-promotions/${encodeURIComponent(key)}/verify`,
    body,
  );
  const result = await settleAttempt(response.value, key);
  if (result.attempt.phase !== "completed") fail("verification did not reach a terminal phase");
  return result;
}

async function defaultState(input) {
  if (
    !["aws", "azure"].includes(input.provider) ||
    !["linux", "macos", "windows"].includes(input.target) ||
    typeof input.region !== "string" ||
    !input.region ||
    typeof input.architecture !== "string" ||
    !input.architecture ||
    typeof input["server-type"] !== "string" ||
    !input["server-type"]
  ) {
    fail("invalid default state scope");
  }
  const query = new URLSearchParams({
    provider: input.provider,
    target: input.target,
    region: input.region,
    architecture: input.architecture,
    serverType: input["server-type"],
    ...(input.os ? { os: input.os } : {}),
  });
  const response = await requestJSON("GET", `/v1/image-promotions/default-state?${query}`);
  assertClosed(response.value, ["provider", "scope", "state"], "default state response");
  assertClosed(
    response.value.scope,
    ["provider", "target", "os", "region", "architecture", "serverType"],
    "default state scope",
  );
  assertState(response.value.state, "default state");
  return response.value;
}

async function main() {
  const [command, ...argv] = process.argv.slice(2);
  const input = options(argv);
  let result;
  if (command === "mutate") result = await mutate(input);
  else if (command === "recover") result = await recover(input);
  else if (command === "verify") result = await verify(input);
  else if (command === "default-state") result = await defaultState(input);
  else fail("usage: aws-image-promotion-client.mjs <mutate|recover|verify|default-state>");
  process.stdout.write(`${JSON.stringify(result)}\n`);
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
