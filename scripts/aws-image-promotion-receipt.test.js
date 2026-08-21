import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdtemp, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { promisify } from "node:util";

import { canonicalJSON } from "./aws-image-candidate.mjs";

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(import.meta.dirname, "..");
const script = path.join(repoRoot, "scripts", "aws-image-promotion-receipt.mjs");
const schema = path.join(repoRoot, "recipes", "aws", "v1", "promotion-receipt.schema.json");
const digest = `sha256:${"a".repeat(64)}`;

function attempt(overrides = {}) {
  return {
    schema: "crabbox-image-promotion-attempt/v1",
    idempotencyKey: "run-200-attempt-1",
    inputDigest: "c".repeat(64),
    operation: "promote",
    phase: "completed",
    version: 3,
    claimVersion: 1,
    outcome: "promoted_verified",
    expected: { state: "absent" },
    before: { state: "absent" },
    beforeBinding: { state: "absent" },
    after: { state: "present", imageId: "ami-123abc", revision: "revision-1" },
    desired: {
      state: "present",
      imageId: "ami-123abc",
      accountId: "123456789012",
      snapshotIds: ["snap-123abc"],
    },
    evidence: {
      schema: "crabbox-image-promotion-evidence/v1",
      qualification: {
        reference: `ghcr.io/example-org/qualification@${digest}`,
        digest,
        receiptDigest: digest,
        inputDigest: digest,
        signatureDigest: digest,
      },
      candidate: {
        reference: `ghcr.io/example-org/candidate@${digest}`,
        digest,
        recordDigest: digest,
      },
      source: { repository: "example-org/crabbox", commit: "b".repeat(40) },
      workflow: {
        identity:
          "example-org/crabbox/.github/workflows/devtools-image-qualify.yml@refs/heads/main",
        runId: "100",
        runAttempt: "1",
      },
      scope: {
        provider: "aws",
        target: "linux",
        region: "eu-west-1",
        instanceType: "c7i.4xlarge",
        architecture: "x86_64",
        os: "ubuntu:26.04",
        profile: "default",
        recipeDigest: digest,
      },
      desired: {
        imageId: "ami-123abc",
        accountId: "123456789012",
        snapshotIds: ["snap-123abc"],
      },
    },
    workflow: { runId: "200", runAttempt: "1" },
    mutation: { status: "succeeded", applied: true },
    verification: {
      status: "passed",
      codes: [],
      image: {
        id: "ami-123abc",
        provider: "aws",
        source: "promoted",
        region: "eu-west-1",
        revision: "revision-1",
      },
    },
    cleanup: { status: "passed" },
    createdAt: "2026-08-21T00:00:00.000Z",
    updatedAt: "2026-08-21T00:10:00.000Z",
    ...overrides,
  };
}

async function build(value) {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-promotion-receipt-"));
  const input = path.join(root, "attempt.json");
  const output = path.join(root, "receipt.json");
  await writeFile(input, `${canonicalJSON(JSON.parse(JSON.stringify(value)))}\n`);
  const result = await execFileAsync(process.execPath, [
    script,
    "build",
    "--attempt",
    input,
    "--output",
    output,
  ]);
  return { ...result, output };
}

test("builds a schema-valid receipt from an authoritative persisted attempt", async () => {
  const result = await build(attempt());
  const receipt = JSON.parse(await readFile(result.output, "utf8"));
  assert.equal(receipt.schema, "crabbox-aws-image-promotion-receipt/v1");
  assert.equal(receipt.outcome, "promoted_verified");
  assert.deepEqual(receipt.before, { state: "absent" });
  assert.deepEqual(receipt.after, {
    state: "present",
    imageId: "ami-123abc",
    revision: "revision-1",
  });
  await execFileAsync("go", ["run", "./scripts/validate-json-schema", schema, result.output], {
    cwd: repoRoot,
    env: {
      ...process.env,
      GOCACHE: path.join(os.tmpdir(), "crabbox-q3-promotion-receipt-go-cache"),
    },
  });
});

test("supports every terminal outcome in the receipt contract", async () => {
  for (const outcome of [
    "evidence_rejected",
    "precondition_failed",
    "promoted_verified",
    "verification_failed",
    "rolled_back",
    "rollback_precondition_failed",
    "outcome_unknown",
  ]) {
    const terminal = attempt({
      outcome,
      operation:
        outcome.startsWith("rollback") || outcome === "rolled_back" ? "rollback" : "promote",
      mutation: ["promoted_verified", "verification_failed", "rolled_back"].includes(outcome)
        ? { status: "succeeded", applied: true }
        : outcome === "outcome_unknown"
          ? { status: "unknown", applied: false, code: "mutation_response_not_persisted" }
          : { status: "rejected", applied: false, code: "precondition_failed" },
      ...(["evidence_rejected", "precondition_failed", "rollback_precondition_failed"].includes(
        outcome,
      )
        ? { after: undefined }
        : {}),
    });
    const result = await build(terminal);
    assert.equal(JSON.parse(await readFile(result.output, "utf8")).outcome, outcome);
  }
});

test("builds a signed-receipt input for a failed probe after an applied mutation", async () => {
  const result = await build(
    attempt({
      outcome: "verification_failed",
      mutation: { status: "succeeded", applied: true },
      verification: { status: "failed", codes: ["lease_start_failed"] },
      cleanup: { status: "failed", code: "cleanup_unverified" },
    }),
  );
  const receipt = JSON.parse(await readFile(result.output, "utf8"));
  assert.equal(receipt.outcome, "verification_failed");
  assert.equal(receipt.mutation.applied, true);
  assert.deepEqual(receipt.verification.codes, ["lease_start_failed"]);
});

test("rejects private paths, lease identifiers, raw errors, and nonterminal attempts", async () => {
  for (const value of [
    attempt({ phase: "awaiting_verification", outcome: undefined }),
    attempt({ leaseId: "cbx_secret" }),
    attempt({ mutation: { status: "rejected", applied: false, rawError: "provider failed" } }),
    attempt({
      mutation: {
        status: "rejected",
        applied: false,
        code: ["", "Users", "example-user", "private"].join("/"),
      },
    }),
    attempt({ mutation: { status: "succeeded", applied: false } }),
  ]) {
    await assert.rejects(
      build(value),
      /invalid attempt|authoritative terminal|authoritative mutation|forbidden field|private data|mutation application state/,
    );
  }
});

test("rejects extra receipt fields and privacy-sensitive values", async () => {
  for (const value of [
    attempt({ arbitraryCallerField: "not-signed" }),
    attempt({
      evidence: { ...attempt().evidence, extra: "not-allowed" },
    }),
    attempt({
      verification: { ...attempt().verification, authorization: "Bearer secret" },
    }),
    attempt({
      cleanup: { ...attempt().cleanup, password: "secret" },
    }),
    attempt({
      mutation: { status: "rejected", applied: false, code: "failed" },
      outcome: "evidence_rejected",
      operator: "alice@example.com",
    }),
    attempt({
      mutation: { status: "rejected", applied: false, code: "failed" },
      outcome: "evidence_rejected",
      diagnostic: [10, 0, 0, 5].join("."),
    }),
    attempt({
      mutation: { status: "rejected", applied: false, code: "failed" },
      outcome: "evidence_rejected",
      service: ["coordinator", "corp", "internal"].join("."),
    }),
  ]) {
    await assert.rejects(
      build(value),
      /invalid attempt|invalid evidence|invalid verification|invalid cleanup|forbidden field|private data/,
    );
  }
});
