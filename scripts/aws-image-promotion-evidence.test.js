import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdtemp, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { promisify } from "node:util";
import { canonicalJSON, digestBytes, digestJSON } from "./aws-image-candidate.mjs";

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(import.meta.dirname, "..");
const verifier = path.join(repoRoot, "scripts", "aws-image-promotion-evidence.mjs");
const fixture = path.join(repoRoot, "scripts", "fixtures", "aws-image-qualification-input.json");
const digest = (digit) => `sha256:${digit.repeat(64)}`;

async function setup() {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-promotion-evidence-"));
  const qualificationInputBytes = await readFile(fixture);
  const qualificationInput = JSON.parse(qualificationInputBytes);
  const candidate = qualificationInput.evidence.find((item) => item.name === "candidate.json");
  const identity = {
    schema: "crabbox-ready-pool-identity/v1",
    profile: qualificationInput.readiness.profile,
    recipeDigest: qualificationInput.readiness.recipeDigest,
    inventoryDigest: digest("4"),
    imageID: qualificationInput.target.amiId,
    architecture: "amd64",
    seedDigest: digest("5"),
    cacheABIDigest: digest("6"),
  };
  const receipt = {
    schema: "crabbox-aws-image-qualification/v1",
    createdAt: "2026-08-21T00:00:00.000Z",
    candidate: {
      artifactDigest: qualificationInput.artifact.digest,
      candidateRecordDigest: candidate.digest,
      qualificationInputDigest: digestBytes(qualificationInputBytes),
    },
    target: {
      amiId: qualificationInput.target.amiId,
      region: qualificationInput.target.region,
      instanceType: qualificationInput.target.instanceType,
      architecture: qualificationInput.target.architecture,
      osSelector: "ubuntu:26.04",
      market: "on-demand",
      profile: qualificationInput.readiness.profile,
      recipeDigest: qualificationInput.readiness.recipeDigest,
    },
    pool: { identity, identityDigest: digestJSON(identity) },
    gates: {
      negative: ["ami", "architecture", "recipe", "type"].map((dimension) => ({
        dimension,
        status: "passed",
      })),
      positive: { status: "passed" },
      osSelector: { status: "passed" },
      cleanOverlay: {
        status: "passed",
        mode: "overlay",
        fallback: false,
        trackedTransfer: { files: 0, bytes: 0 },
      },
      dirtyOverlay: {
        status: "passed",
        mode: "overlay",
        fallback: false,
        trackedTransfer: { files: 1, bytes: 40 },
        fixtureDigest: digest("8"),
      },
    },
    cache: {
      advertised: false,
      status: "skipped",
      reason: "provider_capability_not_advertised",
    },
    cleanup: { status: "passed" },
    workflow: {
      identity: "example-org/crabbox/.github/workflows/devtools-image-qualify.yml@refs/heads/main",
      runId: "456",
      runAttempt: "2",
    },
  };
  const receiptBytes = Buffer.from(`${canonicalJSON(receipt)}\n`);
  const manifest = {
    schemaVersion: 2,
    mediaType: "application/vnd.oci.image.manifest.v1+json",
    artifactType: "application/vnd.crabbox.aws-image-qualification.v1",
    config: {
      mediaType: "application/vnd.oci.empty.v1+json",
      digest: digest("0"),
      size: 2,
    },
    layers: [
      {
        mediaType: "application/vnd.crabbox.aws-image-qualification.v1+json",
        digest: digestBytes(receiptBytes),
        size: receiptBytes.length,
      },
      {
        mediaType: "application/vnd.crabbox.aws-image-qualification-input.v1+json",
        digest: digestBytes(qualificationInputBytes),
        size: qualificationInputBytes.length,
      },
    ],
  };
  const manifestBytes = Buffer.from(JSON.stringify(manifest));
  const referrers = {
    referrers: [
      {
        mediaType: "application/vnd.oci.image.manifest.v1+json",
        artifactType: "application/vnd.dev.sigstore.bundle.v0.3+json",
        digest: digest("b"),
        size: 1234,
      },
    ],
  };
  const files = {
    input: path.join(root, "qualification-input.json"),
    receipt: path.join(root, "qualification.json"),
    manifest: path.join(root, "manifest.raw.json"),
    referrers: path.join(root, "referrers.json"),
  };
  await Promise.all([
    writeFile(files.input, qualificationInputBytes),
    writeFile(files.receipt, receiptBytes),
    writeFile(files.manifest, manifestBytes),
    writeFile(files.referrers, JSON.stringify(referrers)),
  ]);
  return {
    root,
    files,
    manifest,
    manifestBytes,
    qualificationInput,
    qualificationInputBytes,
    receipt,
    receiptBytes,
    referrers,
  };
}

async function run(value, overrides = {}) {
  const qualificationDigest = digestBytes(value.manifestBytes);
  const args = [
    verifier,
    "verify",
    "--qualification-ref",
    `ghcr.io/example-org/crabbox-aws-image-qualifications@${qualificationDigest}`,
    "--manifest-raw",
    value.files.manifest,
    "--referrers",
    value.files.referrers,
    "--qualification",
    value.files.receipt,
    "--qualification-input",
    value.files.input,
    "--source-repository",
    "example-org/crabbox",
    "--source-commit",
    "a".repeat(40),
    "--workflow-ref",
    "example-org/crabbox/.github/workflows/devtools-image-qualify.yml@refs/heads/main",
    "--workflow-run-id",
    "456",
    "--workflow-run-attempt",
    "2",
    "--aws-account-id",
    "123456789012",
  ];
  for (const [name, replacement] of Object.entries(overrides)) {
    const index = args.indexOf(`--${name}`);
    args[index + 1] = replacement;
  }
  try {
    const result = await execFileAsync(process.execPath, args, { cwd: repoRoot });
    return { code: 0, ...result };
  } catch (error) {
    return { code: error.code, stdout: error.stdout ?? "", stderr: error.stderr ?? "" };
  }
}

test("derives the protected AWS promotion target from exact signed qualification layers", async () => {
  const value = await setup();
  const result = await run(value);
  assert.equal(result.code, 0, result.stderr);
  const output = JSON.parse(result.stdout);
  assert.deepEqual(output.scope, {
    provider: "aws",
    target: "linux",
    region: "us-west-2",
    instanceType: "m7i.large",
    architecture: "x86_64",
    os: "ubuntu:26.04",
    profile: "linux-builder",
    recipeDigest: digest("3"),
  });
  assert.deepEqual(output.desired, {
    imageId: "ami-123abc",
    accountId: "123456789012",
    snapshotIds: ["snap-123abc"],
  });
  assert.equal(output.source.commit, "a".repeat(40));
  assert.equal(output.qualification.signatureDigest, digest("b"));
});

test("rejects source, workflow, account, layer, target, and referrer mismatches", async () => {
  for (const [name, mutate, overrides, expected] of [
    ["source commit", undefined, { "source-commit": "b".repeat(40) }, /protected source/],
    ["workflow run", undefined, { "workflow-run-id": "999" }, /workflow identity/],
    ["AWS account", undefined, { "aws-account-id": "123" }, /12 digits/],
    [
      "layer digest",
      async (value) => {
        value.manifest.layers[0].digest = digest("f");
        value.manifestBytes = Buffer.from(JSON.stringify(value.manifest));
        await writeFile(value.files.manifest, value.manifestBytes);
      },
      {},
      /descriptor does not match/,
    ],
    [
      "target image",
      async (value) => {
        value.receipt.target.amiId = "ami-other";
        value.receiptBytes = Buffer.from(`${canonicalJSON(value.receipt)}\n`);
        value.manifest.layers[0].digest = digestBytes(value.receiptBytes);
        value.manifest.layers[0].size = value.receiptBytes.length;
        value.manifestBytes = Buffer.from(JSON.stringify(value.manifest));
        await writeFile(value.files.receipt, value.receiptBytes);
        await writeFile(value.files.manifest, value.manifestBytes);
      },
      {},
      /target amiId mismatch/,
    ],
    [
      "extra referrer",
      async (value) => {
        value.referrers.referrers.push({ ...value.referrers.referrers[0], digest: digest("c") });
        await writeFile(value.files.referrers, JSON.stringify(value.referrers));
      },
      {},
      /exactly one Sigstore/,
    ],
  ]) {
    const value = await setup();
    await mutate?.(value);
    const result = await run(value, overrides);
    assert.notEqual(result.code, 0, `${name} unexpectedly succeeded`);
    assert.match(result.stderr, expected, `${name}: ${result.stderr}`);
  }
});
