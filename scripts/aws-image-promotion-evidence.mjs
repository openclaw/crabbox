#!/usr/bin/env node

import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { canonicalJSON, digestBytes } from "./aws-image-candidate.mjs";

const qualificationArtifactType = "application/vnd.crabbox.aws-image-qualification.v1";
const qualificationMediaType = "application/vnd.crabbox.aws-image-qualification.v1+json";
const qualificationInputMediaType = "application/vnd.crabbox.aws-image-qualification-input.v1+json";
const sigstoreBundleType = "application/vnd.dev.sigstore.bundle.v0.3+json";
const digestPattern = /^sha256:[0-9a-f]{64}$/;

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
    if (parsed[key] !== undefined) fail(`duplicate argument ${name}`);
    parsed[key] = value;
  }
  return parsed;
}

function required(input, name) {
  const value = input[name];
  if (!value) fail(`--${name} is required`);
  return value;
}

async function jsonFile(file, label) {
  const bytes = await readFile(file);
  try {
    return { bytes, value: JSON.parse(bytes) };
  } catch (error) {
    fail(`${label} is not valid JSON: ${error.message}`);
  }
}

function exactKeys(value, keys, label) {
  if (!value || typeof value !== "object" || Array.isArray(value))
    fail(`${label} must be an object`);
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    fail(`${label} must contain exactly: ${expected.join(", ")}`);
  }
}

function assertDigest(value, label) {
  if (!digestPattern.test(value ?? "")) fail(`invalid ${label}`);
}

function descriptorByMediaType(manifest, mediaType) {
  return manifest.layers.filter((layer) => layer?.mediaType === mediaType);
}

function assertLayer(descriptor, bytes, mediaType, label) {
  exactKeys(descriptor, ["mediaType", "digest", "size"], `${label} descriptor`);
  if (
    descriptor.mediaType !== mediaType ||
    descriptor.digest !== digestBytes(bytes) ||
    descriptor.size !== bytes.length
  ) {
    fail(`${label} descriptor does not match exact bytes`);
  }
}

function candidateDescriptor(input) {
  const matches = input.evidence?.filter((item) => item?.name === "candidate.json") ?? [];
  if (matches.length !== 1) fail("qualification input must bind exactly one candidate.json");
  return matches[0];
}

function assertQualificationBinding(receipt, input) {
  const candidate = candidateDescriptor(input);
  if (
    receipt.candidate?.artifactDigest !== input.artifact?.digest ||
    receipt.candidate?.candidateRecordDigest !== candidate.digest
  ) {
    fail("qualification receipt does not bind the candidate evidence");
  }
  const expectedTarget = {
    amiId: input.target?.amiId,
    region: input.target?.region,
    instanceType: input.target?.instanceType,
    architecture: input.target?.architecture,
    profile: input.readiness?.profile,
    recipeDigest: input.readiness?.recipeDigest,
  };
  for (const [name, value] of Object.entries(expectedTarget)) {
    if (receipt.target?.[name] !== value) fail(`qualification target ${name} mismatch`);
  }
  if (
    input.target?.platform !== "linux" ||
    receipt.target?.market !== "on-demand" ||
    !["ubuntu:26.04", "ubuntu:24.04"].includes(receipt.target?.osSelector)
  ) {
    fail("qualification evidence is not an AWS/Linux on-demand target");
  }
  if (
    receipt.pool?.identity?.imageID !== input.target.amiId ||
    receipt.pool?.identity?.profile !== input.readiness.profile ||
    receipt.pool?.identity?.recipeDigest !== input.readiness.recipeDigest
  ) {
    fail("typed ready-pool identity does not bind the promotion target");
  }
}

function assertSigstoreReferrer(referrers, qualificationDigest) {
  const manifests = referrers.referrers ?? referrers.manifests ?? [];
  if (!Array.isArray(manifests)) fail("OCI referrers response is invalid");
  const signatures = manifests.filter(
    (descriptor) => descriptor?.artifactType === sigstoreBundleType,
  );
  if (signatures.length !== 1 || manifests.length !== 1) {
    fail("qualification artifact must have exactly one Sigstore v0.3 referrer");
  }
  assertDigest(signatures[0].digest, "Sigstore referrer digest");
  const subject = signatures[0].subject;
  if (subject !== undefined && subject?.digest !== qualificationDigest) {
    fail("Sigstore referrer subject does not match qualification digest");
  }
  return signatures[0].digest;
}

async function verify(input) {
  const qualificationRef = required(input, "qualification-ref");
  const sourceRepository = required(input, "source-repository");
  const sourceCommit = required(input, "source-commit");
  const workflowRef = required(input, "workflow-ref");
  const workflowRunID = required(input, "workflow-run-id");
  const workflowRunAttempt = required(input, "workflow-run-attempt");
  const accountID = required(input, "aws-account-id");
  const match = /^(ghcr\.io\/[a-z0-9._-]+(?:\/[a-z0-9._-]+)+)@(sha256:[0-9a-f]{64})$/.exec(
    qualificationRef,
  );
  if (!match) fail("qualification reference must be an immutable lowercase GHCR digest reference");
  if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(sourceRepository)) {
    fail("source repository is invalid");
  }
  if (!/^[0-9a-f]{40}$/.test(sourceCommit)) fail("source commit is invalid");
  if (!/^\d{12}$/.test(accountID)) fail("AWS account id must contain 12 digits");
  if (!/^[1-9][0-9]*$/.test(workflowRunID) || !/^[1-9][0-9]*$/.test(workflowRunAttempt)) {
    fail("qualification workflow run identity is invalid");
  }

  const manifestRecord = await jsonFile(required(input, "manifest-raw"), "OCI manifest");
  const receiptRecord = await jsonFile(required(input, "qualification"), "qualification receipt");
  const qualificationInputRecord = await jsonFile(
    required(input, "qualification-input"),
    "qualification input",
  );
  const referrersRecord = await jsonFile(required(input, "referrers"), "OCI referrers");
  const manifest = manifestRecord.value;
  const receipt = receiptRecord.value;
  const qualificationInput = qualificationInputRecord.value;
  if (digestBytes(manifestRecord.bytes) !== match[2]) fail("OCI manifest digest mismatch");
  exactKeys(
    manifest,
    ["schemaVersion", "mediaType", "artifactType", "config", "layers"],
    "OCI qualification manifest",
  );
  if (
    manifest.schemaVersion !== 2 ||
    manifest.mediaType !== "application/vnd.oci.image.manifest.v1+json" ||
    manifest.artifactType !== qualificationArtifactType ||
    !Array.isArray(manifest.layers) ||
    manifest.layers.length !== 2
  ) {
    fail("invalid OCI qualification manifest");
  }
  const receiptLayers = descriptorByMediaType(manifest, qualificationMediaType);
  const inputLayers = descriptorByMediaType(manifest, qualificationInputMediaType);
  if (receiptLayers.length !== 1 || inputLayers.length !== 1) {
    fail("OCI qualification manifest must contain exact receipt and input layers");
  }
  assertLayer(receiptLayers[0], receiptRecord.bytes, qualificationMediaType, "qualification");
  assertLayer(
    inputLayers[0],
    qualificationInputRecord.bytes,
    qualificationInputMediaType,
    "qualification input",
  );
  if (receipt.candidate?.qualificationInputDigest !== digestBytes(qualificationInputRecord.bytes)) {
    fail("qualification input digest does not match receipt");
  }
  assertQualificationBinding(receipt, qualificationInput);
  if (
    qualificationInput.source?.repository !== sourceRepository ||
    qualificationInput.source?.commit !== sourceCommit
  ) {
    fail("qualification input source does not match the protected source");
  }
  if (
    receipt.workflow?.identity !== workflowRef ||
    receipt.workflow?.runId !== workflowRunID ||
    receipt.workflow?.runAttempt !== workflowRunAttempt
  ) {
    fail("qualification workflow identity does not match expectations");
  }
  const publisherWorkflow = `${sourceRepository}/.github/workflows/devtools-image-publish.yml@refs/heads/main`;
  if (
    qualificationInput.source?.workflow?.ref !== publisherWorkflow ||
    qualificationInput.signature?.workflowIdentity !== publisherWorkflow ||
    qualificationInput.signature?.issuer !== "github-actions" ||
    qualificationInput.signature?.mode !== "keyless"
  ) {
    fail("candidate publisher identity is invalid");
  }
  const snapshots = qualificationInput.target?.snapshotIds;
  if (
    !Array.isArray(snapshots) ||
    snapshots.length === 0 ||
    new Set(snapshots).size !== snapshots.length ||
    snapshots.some((snapshot) => !/^snap-[0-9a-z]+$/.test(snapshot))
  ) {
    fail("qualification input backing snapshots are invalid");
  }
  const signatureDigest = assertSigstoreReferrer(referrersRecord.value, match[2]);
  const output = {
    schema: "crabbox-image-promotion-evidence/v1",
    qualification: {
      reference: qualificationRef,
      digest: match[2],
      receiptDigest: digestBytes(receiptRecord.bytes),
      inputDigest: digestBytes(qualificationInputRecord.bytes),
      signatureDigest,
    },
    candidate: {
      reference: qualificationInput.artifact.reference,
      digest: qualificationInput.artifact.digest,
      recordDigest: receipt.candidate.candidateRecordDigest,
    },
    source: { repository: sourceRepository, commit: sourceCommit },
    workflow: {
      identity: workflowRef,
      runId: workflowRunID,
      runAttempt: workflowRunAttempt,
    },
    scope: {
      provider: "aws",
      target: "linux",
      region: receipt.target.region,
      instanceType: receipt.target.instanceType,
      architecture: receipt.target.architecture,
      os: receipt.target.osSelector,
      profile: receipt.target.profile,
      recipeDigest: receipt.target.recipeDigest,
    },
    desired: {
      imageId: receipt.target.amiId,
      accountId: accountID,
      snapshotIds: [...snapshots].sort(),
    },
  };
  process.stdout.write(`${canonicalJSON(output)}\n`);
}

async function main() {
  const [command, ...argv] = process.argv.slice(2);
  if (command !== "verify") fail("usage: aws-image-promotion-evidence.mjs verify [options]");
  await verify(options(argv));
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
