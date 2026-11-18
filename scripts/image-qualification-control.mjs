#!/usr/bin/env node

import { execFileSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";

import {
  executorCredentialIdentity,
  readExecutorCredential,
} from "./image-qualification-executor-token.mjs";
import {
  readAdmission,
  bindingShape,
  namespaces,
  ownedNamespaces,
  verifyAuthority,
  requireAbsent,
  ownershipTags,
  verifyOwned,
  assertOwnedSettings,
} from "./image-qualification-isolation.mjs";
import {
  readProbeResponse,
  managementEndpoint,
  managementFailure,
  recordManagementFailure,
} from "./image-qualification-binding-probe.mjs";
import { withQualificationJournal } from "./image-qualification-journal.mjs";
import {
  handoffSession,
  privateQualificationMode,
  qualificationTaggingContract,
} from "./image-qualification-run-session.mjs";
import {
  ControllerHTTPError,
  requestControllerJSON,
  observeControllerDiscovery,
} from "./image-qualification-controller-http.mjs";
import {
  packageFile,
  qualificationPackagePath,
  qualificationIdentity,
  prepareQualificationBuild,
} from "./image-qualification-package.mjs";
import {
  transportMarkers,
  transportProfile,
  transportBindingShape,
  proveCandidateTransport,
  settleCandidateTransport,
} from "./image-qualification-transport.mjs";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const sha40 = /^[0-9a-f]{40}$/;
const sha64 = /^[0-9a-f]{64}$/;
const runIdPattern = /^image-qualification-[0-9]+-[0-9]+$/;
const workerNamePattern = /^crabbox-image-qualification-[0-9]+-[0-9]+$/;
const relayNamePattern = /^crabbox-image-qualification-relay-[0-9]+-[0-9]+$/;
const maxRunMs = 120 * 60 * 1000;
const firstWalkRunMs = 150 * 60 * 1000;
const retainedRunMs = 38 * 60 * 1000;
const retainedCleanupMs = 8 * 60 * 1000;
const controllerName = "crabbox-image-qualification-controller";
const authorityName = "crabbox-aws-qualification-authority";
let executorTokenForRedaction;
const qualificationWorkflowPath = ".github/workflows/image-qualification.yml";
// Reviewed diagnostic recipe, carried as sealed candidate data rather than
// installed in the production coordinator by the tooling prerequisite.
export const firstWalkRecipeSha256 =
  "eb26f129e9a7cb77ef2197688fcfb3b05356be86b889473e9808287cf016bbf4";
const firstWalkRecipePath = "recipes/bootstrap/v1/linuxOwnershipRepair.sh";

export function readFirstWalkRecipe(directory) {
  let file = directory;
  for (const segment of firstWalkRecipePath.split("/")) {
    file = path.join(file, segment);
    const stat = fs.lstatSync(file);
    if (
      stat.isSymbolicLink() ||
      (file !== path.join(directory, firstWalkRecipePath) && !stat.isDirectory())
    ) {
      throw new Error("first-walk recipe path must not contain links or non-directories");
    }
  }
  const stat = fs.lstatSync(file);
  if (!stat.isFile() || stat.size > 16 * 1024) {
    throw new Error("first-walk recipe is not a bounded regular file");
  }
  const recipe = fs.readFileSync(file);
  if (digest(recipe) !== firstWalkRecipeSha256) {
    throw new Error("first-walk recipe does not match the protected reviewed digest");
  }
  return recipe;
}

function readPackagedRecipe(directory) {
  const recipe = packageFile(
    directory,
    `${qualificationPackagePath}/linuxOwnershipRepair.sh`,
    16 * 1024,
  );
  if (digest(recipe) !== firstWalkRecipeSha256)
    throw new Error("first-walk recipe does not match the protected reviewed digest");
  return recipe;
}

export function stageFirstWalkRecipe(candidateDir, artifactDir) {
  const recipe = readPackagedRecipe(candidateDir);
  const destination = path.join(artifactDir, "candidate", firstWalkRecipePath);
  fs.mkdirSync(path.dirname(destination), { recursive: true, mode: 0o700 });
  fs.writeFileSync(destination, recipe, { mode: 0o600, flag: "wx" });
}

const controllerSource = path.join(root, "scripts/image-qualification-controller-worker.mjs");
const relaySource = path.join(root, "scripts/image-qualification-relay-worker.mjs");
const imageCapsulePaths = [
  "internal/cli/actions.go",
  "recipes/devtools/v1/linux-x86_64.json",
  "recipes/devtools/v1/recipe.schema.json",
  "recipes/linux/v1/linux-builder.json",
  "recipes/linux/v1/linux-minimal.json",
  "recipes/linux/v1/manifest.schema.json",
  "recipes/linux/v1/profile.schema.json",
  "scripts/devtools-image-contract.mjs",
  "scripts/devtools-image-smoke-linux.sh",
  "scripts/generate-linux-readiness.mjs",
  "scripts/install-linux-developer-tools.sh",
  "scripts/linux-readiness.generated.sh",
  "scripts/mint-aws-devtools-image.sh",
];

function relayNameForRun(runId) {
  const match = runId.match(/^image-qualification-([0-9]+-[0-9]+)$/);
  if (!match) throw new Error("qualification run ID is invalid");
  return `crabbox-image-qualification-relay-${match[1]}`;
}

function workerVersion(value) {
  if (/^[0-9a-f]{32}$/i.test(value ?? "")) {
    return `${value.slice(0, 8)}-${value.slice(8, 12)}-${value.slice(12, 16)}-${value.slice(16, 20)}-${value.slice(20)}`;
  }
  if (/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value ?? "")) {
    return value.toLowerCase();
  }
  throw new Error("Cloudflare returned an invalid Worker version");
}

export function canonical(value) {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

export function digest(value) {
  const data = Buffer.isBuffer(value) ? value : Buffer.from(String(value));
  return crypto.createHash("sha256").update(data).digest("hex");
}

function required(name, pattern) {
  const value = process.env[name]?.trim() ?? "";
  if (!value || (pattern && !pattern.test(value))) throw new Error(`invalid or missing ${name}`);
  return value;
}

function boundedString(name, max = 256) {
  const value = required(name);
  if (Buffer.byteLength(value) > max) throw new Error(`${name} is too long`);
  return value;
}

function readJSON(file) {
  if (fs.statSync(file).size > 1024 * 1024) throw new Error(`JSON file is too large: ${file}`);
  return JSON.parse(fs.readFileSync(file, "utf8"));
}

function writeJSON(file, value, mode = 0o600) {
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`, { mode });
}

function appendOutput(name, value) {
  const output = required("GITHUB_OUTPUT");
  fs.appendFileSync(output, `${name}=${String(value).replaceAll("\n", "")}\n`, { mode: 0o600 });
}

function objectAt(value, name) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${name} is invalid`);
  }
  return value;
}

export function retainedImageFromEnv() {
  const mode = process.env.QUALIFICATION_MODE || "mint";
  const names = [
    "QUALIFICATION_RETAINED_IMAGE_ID",
    "QUALIFICATION_RETAINED_SNAPSHOT_ID",
    "QUALIFICATION_RETAINED_SOURCE_SHA",
    "QUALIFICATION_RETAINED_CAPSULE_SHA256",
  ];
  if (mode === "mint" || privateQualificationMode(mode)) {
    if (names.some((name) => process.env[name])) {
      throw new Error("mint mode cannot accept retained-image inputs");
    }
    return undefined;
  }
  if (mode !== "retained") throw new Error("qualification mode is invalid");
  return {
    imageId: required(names[0], /^ami-[0-9a-f]+$/),
    snapshotId: required(names[1], /^snap-[0-9a-f]+$/),
    sourceSha: required(names[2], sha40),
    capsuleSha256: required(names[3], sha64),
  };
}

function imageRecord(value, name) {
  const image = objectAt(value, name);
  if (
    !/^ami-[0-9a-f]+$/.test(image.id ?? "") ||
    typeof image.revision !== "string" ||
    image.revision.length === 0 ||
    image.revision.length > 128
  ) {
    throw new Error(`${name} identity is invalid`);
  }
  return image;
}

export function verifyCatalogRollbackEvidence({
  seed,
  seededReadback,
  promotion,
  rollback,
  restoredReadback,
  failedReadback,
  failedStatus,
  staleResponse,
  staleStatus,
  staleReadback,
  staleFailedReadback,
  staleFailedStatus,
}) {
  const prior = imageRecord(seed, "seed promotion");
  const seeded = imageRecord(
    objectAt(seededReadback, "seed readback").image,
    "seed readback image",
  );
  const promoted = imageRecord(objectAt(promotion, "promotion receipt").image, "failed promotion");
  const promotedPrevious = objectAt(promotion.previous, "promotion previous state");
  const rollbackImage = imageRecord(objectAt(rollback, "rollback receipt").image, "rollback image");
  const rollbackPrevious = objectAt(rollback.previous, "rollback previous state");
  const restored = imageRecord(
    objectAt(restoredReadback, "restored readback").image,
    "restored readback image",
  );
  const stale = objectAt(staleResponse, "stale CAS response");
  const staleExpected = objectAt(stale.expected, "stale CAS expected state");
  const staleCurrent = objectAt(stale.current, "stale CAS current state");
  if (prior.id !== seeded.id || prior.revision !== seeded.revision) {
    throw new Error("candidate API did not capture the exact seeded default revision");
  }
  if (
    promoted.id === prior.id ||
    promoted.revision === prior.revision ||
    promotedPrevious.state !== "present" ||
    promotedPrevious.imageId !== prior.id ||
    promotedPrevious.revision !== prior.revision ||
    !Array.isArray(promotedPrevious.aliases) ||
    promotedPrevious.aliases.length === 0
  ) {
    throw new Error("promotion receipt did not capture and advance from the seeded default");
  }
  if (
    rollbackPrevious.state !== "present" ||
    rollbackPrevious.imageId !== promoted.id ||
    rollbackPrevious.revision !== promoted.revision ||
    rollbackImage.id !== prior.id ||
    rollbackImage.revision !== prior.revision
  ) {
    throw new Error("rollback receipt did not restore the exact seeded default revision");
  }
  if (restored.id !== rollbackImage.id || restored.revision !== rollbackImage.revision) {
    throw new Error("rollback receipt and restored candidate API readback do not match");
  }
  const failed =
    Number(failedStatus) === 404
      ? undefined
      : objectAt(objectAt(failedReadback, "failed readback").image, "failed readback image");
  if (
    (Number(failedStatus) !== 200 && Number(failedStatus) !== 404) ||
    (failed &&
      (failed.id !== promoted.id ||
        Object.hasOwn(failed, "revision") ||
        Object.hasOwn(failed, "promotedAt") ||
        Object.hasOwn(failed, "catalogOnly")))
  ) {
    throw new Error("failed image revision remains in the candidate catalog");
  }
  if (
    Number(staleStatus) !== 409 ||
    stale.error !== "image_promotion_precondition_failed" ||
    staleExpected.state !== "present" ||
    staleExpected.imageId !== promoted.id ||
    staleExpected.revision !== promoted.revision ||
    staleCurrent.state !== "present" ||
    staleCurrent.imageId !== rollbackImage.id ||
    staleCurrent.revision !== rollbackImage.revision
  ) {
    throw new Error("stale CAS response did not report the restored base image revision");
  }
  if (
    canonical(staleReadback) !== canonical(restoredReadback) ||
    Number(staleFailedStatus) !== Number(failedStatus) ||
    canonical(staleFailedReadback) !== canonical(failedReadback)
  ) {
    throw new Error("stale CAS changed candidate catalog, default, or FSR state");
  }
  return {
    version: 1,
    priorImageDigest: digest(prior.id),
    priorRevisionDigest: digest(prior.revision),
    failedImageDigest: digest(promoted.id),
    failedRevisionDigest: digest(promoted.revision),
    restoredRevisionDigest: digest(rollbackImage.revision),
    seededDefaultReadback: true,
    priorDefaultImageRestored: true,
    priorDefaultRevisionRestored: true,
    failedCatalogRevisionRetired: true,
    staleCASRejected: true,
    staleReadbackUnchanged: true,
  };
}

export function verifyRetainedSelection(lease, promotion, region) {
  const image = imageRecord(objectAt(promotion, "promotion receipt").image, "promotion image");
  if (
    !/^cbx_[A-Za-z0-9_-]+$/.test(lease?.id ?? "") ||
    lease.provider !== "aws" ||
    lease.target !== "linux" ||
    lease.region !== region ||
    lease.serverType !== "t3.small" ||
    !/^i-[0-9a-f]+$/.test(lease.cloudID ?? "") ||
    lease.image?.id !== image.id ||
    lease.image?.source !== "promoted" ||
    lease.image?.kind !== "aws-ami" ||
    lease.image?.region !== region ||
    lease.image?.revision !== image.revision
  ) {
    throw new Error("lease did not record the exact normally selected promotion revision");
  }
  return {
    normalSelection: true,
    exactPromotedRevision: true,
    leaseDigest: digest(lease.id),
    instanceDigest: digest(lease.cloudID),
    selectedImageDigest: digest(image.id),
    selectedRevisionDigest: digest(image.revision),
  };
}

async function github(pathname) {
  const token = required("GH_TOKEN");
  const response = await fetch(`https://api.github.com${pathname}`, {
    headers: {
      accept: "application/vnd.github+json",
      authorization: `Bearer ${token}`,
      "user-agent": "crabbox-image-qualification",
      "x-github-api-version": "2022-11-28",
    },
  });
  if (!response.ok) throw new Error(`GitHub API ${pathname} returned ${response.status}`);
  return await response.json();
}

export function workflowRunPathMatches(value, expected, branch) {
  if (value === expected) return true;
  return value === `${expected}@refs/heads/${branch}`;
}

export function normalizeArtifactDigest(value) {
  const digest = String(value ?? "")
    .trim()
    .toLowerCase();
  if (/^[0-9a-f]{64}$/.test(digest)) return `sha256:${digest}`;
  if (/^sha256:[0-9a-f]{64}$/.test(digest)) return digest;
  throw new Error("candidate artifact digest is absent or invalid");
}

async function candidateArtifact(repository, workflowSha, defaultBranch) {
  const artifactDigest = normalizeArtifactDigest(
    required("QUALIFICATION_CANDIDATE_ARTIFACT_DIGEST"),
  );
  const artifactId = required("QUALIFICATION_CANDIDATE_ARTIFACT_ID", /^[1-9][0-9]*$/);
  const runId = required("QUALIFICATION_CANDIDATE_RUN_ID", /^[1-9][0-9]*$/);
  if (runId !== required("GITHUB_RUN_ID", /^[1-9][0-9]*$/)) {
    throw new Error("candidate artifact is not from this protected workflow run");
  }
  const [run, artifact] = await Promise.all([
    github(`/repos/${repository}/actions/runs/${runId}`),
    github(`/repos/${repository}/actions/artifacts/${artifactId}`),
  ]);
  if (
    String(run.id) !== runId ||
    run.run_attempt !== 1 ||
    run.event !== "workflow_dispatch" ||
    run.head_sha !== workflowSha ||
    run.head_repository?.full_name !== repository ||
    !workflowRunPathMatches(run.path, qualificationWorkflowPath, defaultBranch) ||
    String(artifact.id) !== artifactId ||
    artifact.name !== `image-qualification-candidate-${runId}` ||
    artifact.expired !== false ||
    artifact.digest !== artifactDigest ||
    String(artifact.workflow_run?.id) !== runId ||
    artifact.workflow_run?.head_sha !== workflowSha
  ) {
    throw new Error("candidate build or artifact changed");
  }
  return {
    artifactDigest,
    artifactId,
    runId,
  };
}

export async function verifyCandidateIdentity({ artifact = false, cleanup = false } = {}) {
  retainedImageFromEnv();
  const firstWalk = privateQualificationMode(process.env.QUALIFICATION_MODE);
  const repository = required("GITHUB_REPOSITORY", /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/);
  const number = firstWalk ? undefined : required("QUALIFICATION_PULL_REQUEST", /^[1-9][0-9]*$/);
  if (firstWalk && process.env.QUALIFICATION_PULL_REQUEST?.trim())
    throw new Error("first-walk selects an accepted source commit, not a pull request");
  const candidateSha = required("QUALIFICATION_CANDIDATE_SHA", sha40);
  const workflowSha = required("QUALIFICATION_WORKFLOW_SHA", sha40);
  const workflowRef = required("GITHUB_WORKFLOW_REF");
  const defaultBranch = boundedString("QUALIFICATION_DEFAULT_BRANCH", 128);
  const expectedRef = `${repository}/.github/workflows/image-qualification.yml@refs/heads/${defaultBranch}`;
  if (!cleanup) {
    if (required("QUALIFICATION_CONFIRM") !== "qualify") throw new Error("confirmation mismatch");
    if (required("GITHUB_RUN_ATTEMPT") !== "1") throw new Error("reruns are not allowed");
    if (workflowRef !== expectedRef)
      throw new Error("workflow is not the protected default-branch copy");
  }
  const [source, branch] = await Promise.all([
    github(
      firstWalk
        ? `/repos/${repository}/compare/${candidateSha}...${workflowSha}`
        : `/repos/${repository}/pulls/${number}`,
    ),
    github(`/repos/${repository}/commits/${encodeURIComponent(defaultBranch)}`),
  ]);
  const exact =
    branch.sha === workflowSha &&
    (firstWalk
      ? source.base_commit?.sha === candidateSha &&
        source.merge_base_commit?.sha === candidateSha &&
        source.status === (candidateSha === workflowSha ? "identical" : "ahead")
      : source.state === "open" &&
        source.head?.repo?.full_name === repository &&
        source.head?.sha === candidateSha &&
        source.base?.sha === workflowSha);
  if (!exact)
    throw new Error(
      firstWalk
        ? "first-walk source admission failed: select a compatible accepted ancestor SHA; finish any existing-run cleanup before a fresh dispatch from current protected main"
        : "pull request, candidate SHA, or protected workflow SHA changed",
    );
  const identity = { candidateSha, ...(firstWalk ? {} : { number }), repository, workflowSha };
  if (!artifact) return identity;
  const candidate = await candidateArtifact(repository, workflowSha, defaultBranch);
  return { ...identity, ...candidate };
}

function filesUnder(directory, label = "artifact") {
  const result = [];
  const pending = [directory];
  while (pending.length > 0) {
    const current = pending.pop();
    for (const entry of fs
      .readdirSync(current, { withFileTypes: true })
      .sort((a, b) => b.name.localeCompare(a.name))) {
      const absolute = path.join(current, entry.name);
      if (entry.isDirectory()) pending.push(absolute);
      else if (entry.isFile()) result.push(absolute);
      else throw new Error(`${label} contains a non-file entry: ${absolute}`);
    }
  }
  return result.sort((left, right) => left.localeCompare(right));
}

function checkoutSha(directory) {
  return execFileSync("git", ["rev-parse", "HEAD"], {
    cwd: directory,
    encoding: "utf8",
  }).trim();
}

function imageCapsuleRecords(directory) {
  const entries = execFileSync("git", ["ls-tree", "-z", "HEAD", "--", ...imageCapsulePaths], {
    cwd: directory,
    encoding: "utf8",
  })
    .split("\0")
    .filter(Boolean);
  const records = entries
    .map((entry) => {
      const match = entry.match(/^(100644|100755) blob ([0-9a-f]{40})\t(.+)$/);
      if (!match || !imageCapsulePaths.includes(match[3])) {
        throw new Error("retained capsule contains an invalid Git entry");
      }
      const file = path.join(directory, match[3]);
      const stat = fs.lstatSync(file);
      if (
        !stat.isFile() ||
        stat.size > 1024 * 1024 ||
        Boolean(stat.mode & 0o111) !== (match[1] === "100755")
      ) {
        throw new Error("retained capsule input is not a bounded regular file");
      }
      const data = fs.readFileSync(file);
      const blob = crypto
        .createHash("sha1")
        .update(`blob ${data.length}\0`)
        .update(data)
        .digest("hex");
      if (blob !== match[2])
        throw new Error("retained capsule input differs from its exact checkout");
      return { path: match[3], mode: match[1], blob, bytes: data.length, sha256: digest(data) };
    })
    .sort((left, right) => left.path.localeCompare(right.path));
  if (canonical(records.map((record) => record.path)) !== canonical(imageCapsulePaths)) {
    throw new Error("retained capsule requires all 13 source inputs");
  }
  return records;
}

export function createRetainedCapsule(sourceDir, candidateDir, receiptFile) {
  const retained = retainedImageFromEnv();
  if (!retained) throw new Error("retained capsule requires retained mode");
  const candidateSha = required("QUALIFICATION_CANDIDATE_SHA", sha40);
  if (checkoutSha(sourceDir) !== retained.sourceSha || checkoutSha(candidateDir) !== candidateSha) {
    throw new Error("retained capsule checkout SHA mismatch");
  }
  const sourceFiles = imageCapsuleRecords(sourceDir);
  const candidateFiles = imageCapsuleRecords(candidateDir);
  const sourceCapsuleSha256 = digest(canonical(sourceFiles));
  if (sourceCapsuleSha256 !== retained.capsuleSha256) {
    throw new Error("retained source capsule differs from the reviewed receipt");
  }
  // actions.go is the CLI proof identity, not a baked input. Preserve both
  // records without pretending a newly built CLI equals the historical binary.
  const baked = (records) => records.filter((record) => record.path !== "internal/cli/actions.go");
  if (canonical(baked(sourceFiles)) !== canonical(baked(candidateFiles))) {
    throw new Error("candidate changed a retained image recipe, runtime, or proof input");
  }
  const receipt = {
    version: 1,
    sourceSha: retained.sourceSha,
    candidateSha,
    sourceCapsuleSha256,
    sourceFiles,
    candidateFiles,
    bakedInputsIdentical: true,
  };
  writeJSON(receiptFile, receipt);
  return receipt;
}

function verifyRetainedCapsule(artifactDir, candidateSha) {
  const retained = retainedImageFromEnv();
  if (!retained) return;
  const receipt = readJSON(path.join(artifactDir, "retained-capsule.json"));
  if (
    receipt.version !== 1 ||
    receipt.sourceSha !== retained.sourceSha ||
    receipt.candidateSha !== candidateSha ||
    receipt.sourceCapsuleSha256 !== retained.capsuleSha256 ||
    digest(canonical(receipt.sourceFiles)) !== retained.capsuleSha256 ||
    canonical(receipt.sourceFiles?.map((entry) => entry.path)) !== canonical(imageCapsulePaths) ||
    canonical(receipt.candidateFiles?.map((entry) => entry.path)) !==
      canonical(imageCapsulePaths) ||
    canonical(receipt.sourceFiles.slice(1)) !== canonical(receipt.candidateFiles.slice(1)) ||
    receipt.bakedInputsIdentical !== true
  ) {
    throw new Error("retained capsule identity mismatch");
  }
  for (const record of receipt.candidateFiles) {
    const file = path.join(artifactDir, "candidate", record.path);
    if (fs.existsSync(file) && digest(fs.readFileSync(file)) !== record.sha256) {
      throw new Error("candidate runtime differs from retained capsule");
    }
  }
}

function trackedWorkerBuildInputs(directory) {
  return execFileSync("git", ["ls-files", "-z", "--", "worker"], {
    cwd: directory,
    encoding: "utf8",
  })
    .split("\0")
    .filter((file) => file && !file.startsWith("worker/src/") && !file.startsWith("worker/test/"))
    .sort();
}

function fileRecords(directory, files) {
  return files.map((file) => {
    const absolute = path.join(directory, file);
    const stat = fs.lstatSync(absolute);
    if (!stat.isFile()) throw new Error(`protected build input is not a regular file: ${file}`);
    return {
      path: file,
      bytes: stat.size,
      sha256: digest(fs.readFileSync(absolute)),
    };
  });
}

function workerSourceRecords(candidateDir) {
  const sourceDir = path.join(candidateDir, "worker", "src");
  const sourceFiles = filesUnder(sourceDir, "candidate Worker source");
  const sourceRecords = sourceFiles.map((file) => ({
    path: path.relative(sourceDir, file).split(path.sep).join("/"),
    bytes: fs.statSync(file).size,
    sha256: digest(fs.readFileSync(file)),
  }));
  if (
    sourceRecords.length === 0 ||
    sourceRecords.length > 1024 ||
    sourceRecords.some((file) => file.bytes > 4 * 1024 * 1024) ||
    sourceRecords.reduce((total, file) => total + file.bytes, 0) > 32 * 1024 * 1024
  ) {
    throw new Error("candidate Worker source exceeds the file or byte limit");
  }
  return { sourceDir, sourceFiles, sourceRecords };
}

export function prepareCandidateBuild(
  harnessDir,
  candidateDir,
  receiptFile,
  candidateSha,
  workflowSha,
  baselineDir,
) {
  if (!sha40.test(candidateSha) || !sha40.test(workflowSha)) {
    throw new Error("invalid protected build SHA");
  }
  if (checkoutSha(harnessDir) !== workflowSha || checkoutSha(candidateDir) !== candidateSha) {
    throw new Error("protected build checkout SHA mismatch");
  }
  if (privateQualificationMode(process.env.QUALIFICATION_MODE)) {
    if (!baselineDir) throw new Error("first-walk build requires pinned baseline checkout");
    const privateBuild = prepareQualificationBuild(harnessDir, candidateDir, baselineDir);
    const receipt = {
      version: 2,
      candidateSha,
      workflowSha,
      workerSourceSha256: privateBuild.qualification.workerSourceSha256,
      protectedBuildInputsSha256: privateBuild.productionInputsSha256,
      qualification: privateBuild.qualification,
    };
    writeJSON(receiptFile, receipt);
    return receipt;
  }
  // Candidate source remains data: every executable build control comes from
  // the protected checkout, so a PR cannot smuggle a build hook into the bundle.
  const fixedFiles = ["go.mod", "go.sum", "scripts/image-qualification-network.ts"];
  const harnessWorkerInputs = trackedWorkerBuildInputs(harnessDir);
  const candidateWorkerInputs = trackedWorkerBuildInputs(candidateDir);
  if (canonical(harnessWorkerInputs) !== canonical(candidateWorkerInputs)) {
    throw new Error("candidate changed protected Worker build inputs");
  }
  const protectedFiles = [...fixedFiles, ...harnessWorkerInputs];
  const harnessInputs = fileRecords(harnessDir, protectedFiles);
  const candidateInputs = fileRecords(candidateDir, protectedFiles);
  if (canonical(harnessInputs) !== canonical(candidateInputs)) {
    throw new Error("candidate changed protected build inputs");
  }

  const { sourceDir, sourceFiles, sourceRecords } = workerSourceRecords(candidateDir);

  const buildSourceDir = path.join(harnessDir, "worker", "src");
  fs.rmSync(buildSourceDir, { recursive: true, force: true });
  for (const source of sourceFiles) {
    const relative = path.relative(sourceDir, source);
    const destination = path.join(buildSourceDir, relative);
    fs.mkdirSync(path.dirname(destination), { recursive: true });
    fs.copyFileSync(source, destination);
    fs.chmodSync(destination, fs.statSync(source).mode & 0o777);
  }

  const receipt = {
    version: 1,
    candidateSha,
    workflowSha,
    workerSourceSha256: digest(canonical(sourceRecords)),
    protectedBuildInputsSha256: digest(canonical(harnessInputs)),
  };
  writeJSON(receiptFile, receipt);
  return receipt;
}

function verifyBuildReceipt(candidateDir, artifactDir, candidateSha, workflowSha) {
  const receipt = readJSON(path.join(artifactDir, "build-inputs.json"));
  const privateBuild = privateQualificationMode(process.env.QUALIFICATION_MODE);
  const expected = privateBuild ? qualificationIdentity(root) : null;
  const sourceSha = privateBuild
    ? expected.workerSourceSha256
    : digest(canonical(workerSourceRecords(candidateDir).sourceRecords));
  if (
    receipt.version !== (privateBuild ? 2 : 1) ||
    (privateBuild && canonical(receipt.qualification) !== canonical(expected)) ||
    receipt.candidateSha !== candidateSha ||
    receipt.workflowSha !== workflowSha ||
    receipt.workerSourceSha256 !== sourceSha ||
    !sha64.test(receipt.protectedBuildInputsSha256)
  ) {
    throw new Error("candidate build-input receipt mismatch");
  }
  return receipt;
}

export function createManifest(candidateDir, artifactDir, candidateSha, workflowSha) {
  if (!sha40.test(candidateSha) || !sha40.test(workflowSha))
    throw new Error("invalid manifest SHA");
  const actualSha = checkoutSha(candidateDir);
  if (actualSha !== candidateSha) throw new Error("candidate checkout SHA mismatch");
  const buildReceipt = verifyBuildReceipt(candidateDir, artifactDir, candidateSha, workflowSha);
  verifyRetainedCapsule(artifactDir, candidateSha);
  if (privateQualificationMode(process.env.QUALIFICATION_MODE)) {
    if (
      !readPackagedRecipe(candidateDir).equals(
        readFirstWalkRecipe(path.join(artifactDir, "candidate")),
      )
    ) {
      throw new Error("sealed first-walk recipe differs from candidate source");
    }
  }
  const files = filesUnder(artifactDir)
    .filter((file) => path.basename(file) !== "manifest.json")
    .map((file) => ({
      path: path.relative(artifactDir, file).split(path.sep).join("/"),
      bytes: fs.statSync(file).size,
      sha256: digest(fs.readFileSync(file)),
    }));
  if (
    files.length > 1024 ||
    files.some((file) => file.bytes > 128 * 1024 * 1024) ||
    files.reduce((total, file) => total + file.bytes, 0) > 128 * 1024 * 1024
  ) {
    throw new Error("candidate artifact exceeds the file or byte limit");
  }
  if (!files.some((file) => file.path === "bin/crabbox"))
    throw new Error("candidate CLI is absent");
  const modules = files.filter(
    (file) => file.path.startsWith("worker/") && /\.(?:m?js)$/.test(file.path),
  );
  if (modules.length === 0) throw new Error("candidate Worker bundle is absent");
  if (
    buildReceipt.qualification &&
    !files.some((file) => file.path === "authority/aws-qualification-authority.js")
  )
    throw new Error("private authority bundle is absent");
  const manifest = {
    version: buildReceipt.version,
    ...(buildReceipt.qualification ? { qualification: buildReceipt.qualification } : {}),
    candidateSha,
    workflowSha,
    createdAt: new Date().toISOString(),
    mainModule: modules.find((file) => /(^|\/)index\.js$/.test(file.path))?.path ?? modules[0].path,
    files,
  };
  manifest.manifestSha256 = digest(canonical(manifest));
  writeJSON(path.join(artifactDir, "manifest.json"), manifest);
  return manifest;
}

export function verifyManifest(artifactDir, expectedCandidate, expectedWorkflow) {
  const manifest = readJSON(path.join(artifactDir, "manifest.json"));
  const receipt = readJSON(path.join(artifactDir, "build-inputs.json"));
  const privateBuild = privateQualificationMode(process.env.QUALIFICATION_MODE);
  const expectedPackage = privateBuild ? qualificationIdentity(root) : null;
  const unsigned = { ...manifest };
  delete unsigned.manifestSha256;
  if (
    manifest.version !== (privateBuild ? 2 : 1) ||
    (privateBuild &&
      (canonical(manifest.qualification) !== canonical(expectedPackage) ||
        canonical(receipt.qualification) !== canonical(expectedPackage) ||
        receipt.workerSourceSha256 !== expectedPackage.workerSourceSha256)) ||
    manifest.candidateSha !== expectedCandidate ||
    manifest.workflowSha !== expectedWorkflow ||
    !sha64.test(manifest.manifestSha256) ||
    digest(canonical(unsigned)) !== manifest.manifestSha256 ||
    receipt.version !== (privateBuild ? 2 : 1) ||
    receipt.candidateSha !== expectedCandidate ||
    receipt.workflowSha !== expectedWorkflow ||
    !sha64.test(receipt.workerSourceSha256) ||
    !sha64.test(receipt.protectedBuildInputsSha256)
  ) {
    throw new Error("candidate manifest identity mismatch");
  }
  const expected = new Set(["manifest.json"]);
  for (const entry of manifest.files) {
    if (!/^[A-Za-z0-9_.\/-]+$/.test(entry.path) || entry.path.includes("..")) {
      throw new Error("candidate manifest path is unsafe");
    }
    const file = path.join(artifactDir, entry.path);
    expected.add(entry.path);
    if (
      !fs.statSync(file).isFile() ||
      fs.statSync(file).size !== entry.bytes ||
      digest(fs.readFileSync(file)) !== entry.sha256
    ) {
      throw new Error(`candidate artifact digest mismatch: ${entry.path}`);
    }
  }
  const actual = filesUnder(artifactDir).map((file) =>
    path.relative(artifactDir, file).split(path.sep).join("/"),
  );
  if (actual.some((file) => !expected.has(file)))
    throw new Error("candidate artifact has extra files");
  verifyRetainedCapsule(artifactDir, expectedCandidate);
  if (privateQualificationMode(process.env.QUALIFICATION_MODE)) {
    readFirstWalkRecipe(path.join(artifactDir, "candidate"));
    if (!manifest.files.some((file) => file.path === "authority/aws-qualification-authority.js"))
      throw new Error("private authority bundle is absent");
  }
  if (retainedImageFromEnv()) {
    const installer = path.join(artifactDir, "candidate/scripts/install-linux-developer-tools.sh");
    if (fs.statSync(installer).size > 256 * 1024) {
      throw new Error("candidate retained installer exceeds the source byte limit");
    }
    const source = fs.readFileSync(installer, "utf8");
    // Match the reviewed multiline generator layout without sourcing candidate
    // code in credentialed admission. This checks compatibility, not shell safety.
    for (const name of ["node_pnpm", "go", "bun", "rust", "uv"]) {
      const declaration = new RegExp(
        `^${name}_smoke_script\\(\\) \\{\\r?\\n(?:[ \\t]+[^\\r\\n]*\\r?\\n)+\\}\\r?$`,
        "m",
      );
      if (!declaration.test(source)) {
        throw new Error(`candidate retained installer lacks ${name}_smoke_script generator`);
      }
    }
  }
  return manifest;
}

export function verifyPublisherContract(source) {
  if (typeof source !== "string" || Buffer.byteLength(source) > 256 * 1024) {
    throw new Error("candidate publisher source is invalid");
  }
  const requiredPatterns = [
    /CRABBOX_BIN="\$\{CRABBOX_BIN:-/,
    /trap cleanup EXIT/,
    /rollback_promoted_image\(\)/,
    /--restore-receipt "\$receipt" "\$current_id"/,
    /promote_args=\(image promote .*--expected-current-image capture\)/,
    /\.previous\.aliases \| length > 0/,
    /restored previous default image=%s/,
  ];
  if (requiredPatterns.some((pattern) => !pattern.test(source))) {
    throw new Error("candidate publisher lacks the transactional rollback contract");
  }
  // Admission checks the audited publisher layout, not arbitrary shell semantics.
  // Keep failure handling inside cleanup: promotion stays armed through teardown.
  const cleanup = source.match(/^cleanup\(\) \{\n([\s\S]*?)^\}\ntrap cleanup EXIT$/m);
  if (!cleanup) {
    throw new Error("candidate publisher cleanup contract is missing");
  }
  const main = source.slice(cleanup.index + cleanup[0].length);
  const rollback = `  if [[ "$rollback_pending" == "1" && "$exit_status" != "0" ]]; then
    rollback_pending=0
    if rollback_promoted_image "$promotion_log"; then
      rollback_status="succeeded"
    else
      rollback_status="failed"
      finalizer_status=1
    fi
  fi`;
  const cleanupOrdered = [
    "  local exit_status=$?\n  trap - EXIT",
    rollback,
    '  if [[ "$keep_lease" != "1" ]]; then',
    '    cleanup_leases=("$measurement_lease" "$promoted_lease" "$candidate_lease" "$source_lease")',
    `      if ! "$CRABBOX_BIN" stop --provider aws --target "$target" "$lease"; then
        cleanup_status="failed"
        finalizer_status=1
      fi`,
    `  if [[ "$exit_status" == "0" && "$finalizer_status" != "0" ]]; then
    exit_status="$finalizer_status"
  fi`,
    rollback,
    '  if [[ "$measured" == "1" && -n "$measurement_dir" && -n "$public_outcome" ]]; then',
    '    outcome_candidate="$(mktemp "${public_outcome}.candidate.XXXXXX")" || proof_status=$?',
    `    node "$ROOT/scripts/devtools-image-proof.mjs" finalize \\
      "$outcome_candidate" "$measurement_dir/policy.json" "$measurement_dir" \\
      "$outcome_stage" "$exit_status" "$rollback_status" "$cleanup_status" "$receipt_path" ||
      proof_status=$?`,
    '      mv -f "$outcome_candidate" "$public_outcome" || proof_status=$?',
    '    if [[ "$proof_status" != "0" ]]; then',
    `      if [[ "$exit_status" == "0" ]]; then
        exit_status="$proof_status"
      fi`,
    `      if [[ "$rollback_pending" == "1" ]]; then
        rollback_pending=0
        if rollback_promoted_image "$promotion_log"; then
          rollback_status="succeeded"
        else
          rollback_status="failed"
          finalizer_status=1
        fi
      fi`,
    '    elif [[ "$exit_status" == "0" ]]; then\n      rollback_pending=0',
    '  exit "$exit_status"\n',
  ];
  const mainOrdered = [
    'run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$candidate_lease"',
    'candidate_lease=""',
    "rollback_pending=1",
    'run_json_tee "$promotion_log" "$CRABBOX_BIN" "${promote_args[@]}"',
    'promoted_lease="$(warmup promoted)"',
    'smoke "$promoted_lease"',
  ];
  for (const [text, ordered] of [
    [cleanup[1], cleanupOrdered],
    [main, mainOrdered],
  ]) {
    let cursor = 0;
    for (const marker of ordered) {
      const next = text.indexOf(marker, cursor);
      if (next === -1) {
        throw new Error("candidate publisher rollback ordering is not admissible");
      }
      cursor = next + marker.length;
    }
  }
  if (
    (cleanup[1].match(/^\s*rollback_pending=0$/gm) ?? []).length !== 4 ||
    /^\s*rollback_pending=0$/m.test(main.slice(main.indexOf("rollback_pending=1")))
  ) {
    throw new Error("candidate publisher disarms rollback outside cleanup outcomes");
  }
}

export class Cloudflare {
  constructor(accountId, token) {
    this.accountId = accountId;
    this.token = token;
    this.base = `https://api.cloudflare.com/client/v4/accounts/${accountId}`;
  }

  async request(
    pathname,
    init = {},
    {
      allow404 = false,
      envelope = false,
      probeContentIdentity = false,
      contentIdentityBytes,
      boundedBytes,
      responseMetadata = false,
    } = {},
  ) {
    let response;
    try {
      response = await fetch(`${this.base}${pathname}`, {
        ...init,
        ...(this.discoverySignal
          ? {
              signal: init.signal
                ? AbortSignal.any([init.signal, this.discoverySignal])
                : this.discoverySignal,
              redirect: "error",
            }
          : {}),
        headers: { authorization: `Bearer ${this.token}`, ...(init.headers ?? {}) },
      });
    } catch (error) {
      throw recordManagementFailure(
        error,
        pathname,
        init.signal?.aborted ? "cancelled" : "request-failed",
      );
    }
    const ray = response.headers.get("cf-ray");
    const responseInfo = {
      status: response.status,
      ...(ray && /^[a-zA-Z0-9-]{1,128}$/.test(ray) ? { ray } : {}),
    };
    if (allow404 && response.status === 404) {
      await response.body?.cancel();
      return responseMetadata ? { result: undefined, response: responseInfo } : undefined;
    }
    if ((probeContentIdentity || contentIdentityBytes) && response.ok) {
      const probe = await import("./image-qualification-binding-probe.mjs");
      try {
        return await probe.probeContentIdentity(response, contentIdentityBytes);
      } catch (error) {
        throw recordManagementFailure(
          error,
          pathname,
          init.signal?.aborted ? "cancelled" : "body-read-error",
          responseInfo,
          contentIdentityBytes ?? 64 * 1024,
        );
      }
    }
    // Ordinary Worker deletion can acknowledge with an empty HTTP204. It
    // still requires the subsequent exact absence checks, never an invented body.
    let text;
    try {
      text =
        init.method === "DELETE" && response.status === 204 && response.body === null
          ? ""
          : boundedBytes
            ? await readProbeResponse(response, boundedBytes)
            : await response.text();
    } catch (error) {
      throw recordManagementFailure(
        error,
        pathname,
        init.signal?.aborted ? "cancelled" : "body-read-error",
        responseInfo,
        boundedBytes,
      );
    }
    let parsed;
    try {
      parsed = text ? JSON.parse(text) : {};
      if (boundedBytes && (!parsed || typeof parsed !== "object" || Array.isArray(parsed)))
        throw new Error("invalid envelope");
    } catch {
      if (boundedBytes)
        throw recordManagementFailure(
          new Error("Cloudflare management response is not a JSON object"),
          pathname,
          "invalid-json",
          responseInfo,
          boundedBytes,
        );
      parsed = { raw: text.slice(0, 200) };
    }
    if (!response.ok || parsed.success === false) {
      throw recordManagementFailure(
        Object.assign(new Error(`Cloudflare API ${pathname} returned ${response.status}`), {
          status: response.status,
          responseInfo,
          pathname,
        }),
        pathname,
        response.ok ? "api-error" : "http-error",
        responseInfo,
        boundedBytes,
      );
    }
    if (allow404 && (init.method ?? "GET") === "GET" && parsed.result === undefined) {
      throw recordManagementFailure(
        new Error(`Cloudflare API ${pathname} did not confirm existence or absence`),
        pathname,
        "unconfirmed-existence",
        responseInfo,
        boundedBytes,
      );
    }
    if (responseMetadata)
      return { result: envelope ? parsed : parsed.result, response: responseInfo };
    return envelope ? parsed : parsed.result;
  }

  async list(pathname) {
    if (pathname === "/workers/scripts") {
      const scripts = await this.request(pathname);
      if (!Array.isArray(scripts))
        throw recordManagementFailure(
          new Error("Cloudflare script inventory was not an array"),
          pathname,
          "invalid-inventory",
        );
      return scripts;
    }
    const items = [];
    for (let page = 1; page <= 1000; page++) {
      const result = await this.request(
        `${pathname}${pathname.includes("?") ? "&" : "?"}page=${page}&per_page=100`,
        {},
        { envelope: true },
      );
      const batch = result.result?.items ?? result.result;
      if (!Array.isArray(batch)) throw new Error("Cloudflare inventory was not an array");
      items.push(...batch);
      if (
        result.result_info?.total_pages !== undefined
          ? page >= result.result_info.total_pages
          : batch.length < 100
      )
        return items;
    }
    throw new Error("Cloudflare inventory pagination exceeded bound");
  }

  async upload(name, moduleFile, metadata, extraModules = [], options = {}) {
    let retained = [];
    if (metadata.keep_bindings !== undefined) {
      if (canonical(metadata.keep_bindings) !== canonical(["secret_text"]))
        throw new Error("qualification upload may retain only verified secret bindings");
      const existing = await verifyOwned(this, name);
      retained = bindingShape(existing.bindings.filter((b) => b.type === "secret_text"));
      if (retained.some((b) => metadata.bindings.some((n) => n.name === b.name)))
        throw new Error("qualification upload cannot replace a retained secret binding");
    }
    // Secrets are retained by type without reading or resending their values.
    // Bind ownership to the effective returned settings, including those names.
    metadata = {
      ...metadata,
      tags: ownershipTags(this, name, [...metadata.bindings, ...retained]),
    };
    const form = new FormData();
    form.set("metadata", new Blob([JSON.stringify(metadata)], { type: "application/json" }));
    form.set(
      metadata.main_module,
      new Blob([fs.readFileSync(moduleFile)], { type: "application/javascript+module" }),
      metadata.main_module,
    );
    for (const module of extraModules) {
      form.set(
        module.name,
        new Blob([fs.readFileSync(module.file)], { type: module.type }),
        module.name,
      );
    }
    return await this.request(`/workers/scripts/${name}`, { method: "PUT", body: form }, options);
  }

  async settings(name) {
    return await this.request(`/workers/scripts/${name}/settings`);
  }

  async latestVersion(name) {
    const deployments = await this.request(`/workers/scripts/${name}/deployments`);
    const active = (deployments?.deployments ?? deployments?.items ?? deployments)?.[0];
    if (active?.versions?.length !== 1 || active.versions[0].percentage !== 100)
      throw new Error("Cloudflare did not confirm one active Worker version");
    return workerVersion(active.versions[0].version_id);
  }

  async verifyController() {
    const version = await this.latestVersion(controllerName);
    await verifyOwned(this, controllerName);
    if (this.beforeRecoveryCall) await this.beforeRecoveryCall("controller-content");
    const contentSignals = [this.discoverySignal, this.recoverySignal?.()].filter(Boolean);
    const identity = await this.request(
      `/workers/scripts/${controllerName}/content/v2`,
      contentSignals.length ? { signal: AbortSignal.any(contentSignals), redirect: "error" } : {},
      { contentIdentityBytes: 64 * 1024 },
    );
    if (
      identity.count !== 1 ||
      identity.module !== "controller.mjs" ||
      identity.sha256 !== digest(fs.readFileSync(controllerSource)) ||
      (await this.latestVersion(controllerName)) !== version
    )
      throw new Error("controller source or version changed");
    if (this.controllerVersion && this.controllerVersion !== version)
      throw new Error("controller ownership version changed");
    this.controllerVersion = version;
    return version;
  }

  async configureSubdomain(name, enabled) {
    await this.request(`/workers/scripts/${name}/subdomain`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ enabled, previews_enabled: false }),
    });
    const actual = await this.request(`/workers/scripts/${name}/subdomain`);
    if (actual?.enabled !== enabled || actual?.previews_enabled !== false) {
      throw new Error(`Cloudflare Worker subdomain settings did not persist for ${name}`);
    }
  }

  async enableSubdomain(name) {
    await this.configureSubdomain(name, true);
    const subdomain = await this.request("/workers/subdomain");
    if (!subdomain?.subdomain)
      throw new Error("Cloudflare account workers.dev subdomain is absent");
    return `https://${name}.${subdomain.subdomain}.workers.dev`;
  }

  async disableSubdomain(name) {
    await this.configureSubdomain(name, false);
  }

  async deleteScript(name) {
    // References to any owned caller must block ordinary deletion.
    await this.request(`/workers/scripts/${name}`, { method: "DELETE" }, { allow404: true });
    if (
      (await this.request(`/workers/scripts/${name}/settings`, {}, { allow404: true })) !==
      undefined
    ) {
      throw new Error(`Cloudflare Worker deletion was not observable for ${name}`);
    }
    if (name === controllerName) this.controllerVersion = undefined;
  }
}

function qualificationConfig() {
  const retainedImage = retainedImageFromEnv();
  const rootGB = retainedImage
    ? 400
    : Number(required("QUALIFICATION_ROOT_GB", /^(?:[89]|1[0-9]|20)$/));
  return {
    region: required("QUALIFICATION_AWS_REGION", /^[a-z]{2}-[a-z]+-[0-9]+$/),
    subnetId: required("QUALIFICATION_SUBNET_ID", /^subnet-[0-9a-f]+$/),
    securityGroupId: required("QUALIFICATION_SECURITY_GROUP_ID", /^sg-[0-9a-f]+$/),
    baseAmiId: required("QUALIFICATION_BASE_AMI_ID", /^ami-[0-9a-f]+$/),
    rootGB,
    instanceTypes: ["t3.small", "t3a.small"],
    maxMinutes: privateQualificationMode(process.env.QUALIFICATION_MODE)
      ? 150
      : retainedImage
        ? 38
        : 120,
    maxMonthlyUSD: 10,
    maxConcurrentInstances: 1,
    maxLaunches: privateQualificationMode(process.env.QUALIFICATION_MODE)
      ? 2
      : retainedImage
        ? 1
        : 3,
    fastSnapshotRestore: false,
    ...(privateQualificationMode(process.env.QUALIFICATION_MODE)
      ? { firstWalk: true, taggingContract: qualificationTaggingContract }
      : {}),
    ...(process.env.QUALIFICATION_MODE === "key-authorization"
      ? { keyAuthorizationOnly: true }
      : {}),
    ...(retainedImage ? { retainedImage } : {}),
  };
}

function qualificationHandoff(identityCheck, manifestSha256, startsAt) {
  const config = qualificationConfig();
  const session = handoffSession(process.env, identityCheck, manifestSha256);
  if (session) startsAt = session.scope.startsAt;
  const runId = `image-qualification-${identityCheck.runId}-${required("GITHUB_RUN_ATTEMPT")}`;
  const expiresAt = new Date(
    Date.parse(startsAt) +
      (config.firstWalk ? firstWalkRunMs : config.retainedImage ? retainedRunMs : maxRunMs),
  ).toISOString();
  return {
    version: 1,
    repository: identityCheck.repository,
    workflowSha: identityCheck.workflowSha,
    candidateSha: identityCheck.candidateSha,
    runId,
    attempt: 1,
    owner: `${runId}@example.invalid`,
    candidateWorker: `crabbox-${runId}`,
    relayWorker: relayNameForRun(runId),
    candidateArtifactId: identityCheck.artifactId,
    candidateArtifactDigest: identityCheck.artifactDigest,
    manifestSha256,
    authoritySha: required("QUALIFICATION_AUTHORITY_SHA", sha40),
    authorityVersion: boundedString("QUALIFICATION_AUTHORITY_VERSION", 64),
    policyHash: required("QUALIFICATION_EXPECTED_POLICY_HASH", sha64),
    // Private deployment coordinates are inputs, never published handoff fields.
    configurationSha256: digest(
      canonical({
        account: required("CLOUDFLARE_ACCOUNT_ID", /^[0-9a-f]{32}$/),
        config,
        isolation: readAdmission().sha256,
        ...(session ? { awsSessionScopeSha256: session.sha256 } : {}),
      }),
    ),
    mode: config.keyAuthorizationOnly
      ? "key-authorization"
      : config.firstWalk
        ? "first-walk"
        : config.retainedImage
          ? "retained"
          : "mint",
    ...(config.retainedImage ? { retainedCapsuleSha256: config.retainedImage.capsuleSha256 } : {}),
    startsAt,
    workExpiresAt: new Date(
      Date.parse(expiresAt) -
        (config.firstWalk ? 20 * 60_000 : config.retainedImage ? retainedCleanupMs : 0),
    ).toISOString(),
    expiresAt,
  };
}

function assertHandoffWindow(handoff) {
  const now = Date.now();
  if (
    !Number.isFinite(Date.parse(handoff.startsAt)) ||
    Date.parse(handoff.startsAt) > now ||
    now >= Date.parse(handoff.workExpiresAt)
  ) {
    throw new Error("qualification handoff work window is not active");
  }
}

function publishHandoff(identityCheck, manifest) {
  for (const name of [
    "AWS_ACCESS_KEY_ID",
    "AWS_SECRET_ACCESS_KEY",
    "AWS_SESSION_TOKEN",
    "CLOUDFLARE_API_TOKEN",
    "QUALIFICATION_CONTROLLER_TOKEN",
    "QUALIFICATION_EXECUTOR_CREDENTIAL",
  ]) {
    if (Object.hasOwn(process.env, name))
      throw new Error(`${name} must be absent from preparation`);
  }
  const handoff = qualificationHandoff(
    identityCheck,
    manifest.manifestSha256,
    new Date(Date.now()).toISOString(),
  );
  assertHandoffWindow(handoff);
  const handoffSha256 = digest(canonical(handoff));
  const executorIdentity = executorCredentialIdentity({
    ...process.env,
    QUALIFICATION_HANDOFF_SHA256: handoffSha256,
    QUALIFICATION_EXPIRES_AT: handoff.expiresAt,
  });
  const directory = path.resolve(required("QUALIFICATION_HANDOFF_DIR"));
  fs.mkdirSync(directory, { recursive: true, mode: 0o700 });
  // Never replace a prepared identity, even when a caller repeats admission locally.
  fs.writeFileSync(path.join(directory, "handoff.json"), `${canonical(handoff)}\n`, {
    flag: "wx",
    mode: 0o600,
  });
  fs.writeFileSync(
    path.join(directory, "executor-identity.json"),
    `${canonical(executorIdentity)}\n`,
    {
      flag: "wx",
      mode: 0o600,
    },
  );
  let projection;
  if (handoff.mode === "retained") {
    projection = {
      authority_owner: handoff.owner,
      run_id: handoff.runId,
      source_sha: handoff.candidateSha,
      starts_at: handoff.startsAt,
      creation_expires_at: handoff.workExpiresAt,
      cleanup_expires_at: handoff.expiresAt,
    };
    fs.writeFileSync(path.join(directory, "iam-inputs.json"), `${canonical(projection)}\n`, {
      flag: "wx",
      mode: 0o600,
    });
  }
  appendOutput("handoff_sha256", digest(canonical(handoff)));
  if (process.env.GITHUB_STEP_SUMMARY) {
    fs.appendFileSync(
      process.env.GITHUB_STEP_SUMMARY,
      `## Qualification handoff\n\nRun: \`${handoff.runId}\`\n\n` +
        `Work cutoff: ${handoff.workExpiresAt}; absolute expiry: ${handoff.expiresAt}.\n\n` +
        "Preparation, credential delivery, approvals and deployment consume this window. " +
        "Approve deployment only after the exact binding and cleanup custody are verified. " +
        "Expired preparation requires a newly authorized run; it cannot be extended.\n\n" +
        (projection ? `\`\`\`json\n${JSON.stringify(projection, null, 2)}\n\`\`\`\n` : ""),
      { mode: 0o600 },
    );
  }
}

async function verifyHandoff(identityCheck, manifestSha256) {
  const handoff = readJSON(path.join(required("QUALIFICATION_HANDOFF_DIR"), "handoff.json"));
  const startsAt = handoff.startsAt;
  if (
    typeof startsAt !== "string" ||
    !Number.isFinite(Date.parse(startsAt)) ||
    new Date(startsAt).toISOString() !== startsAt
  ) {
    throw new Error("qualification handoff start is invalid");
  }
  const expected = qualificationHandoff(identityCheck, manifestSha256, startsAt);
  if (
    canonical(handoff) !== canonical(expected) ||
    digest(canonical(handoff)) !== required("QUALIFICATION_HANDOFF_SHA256", sha64)
  ) {
    throw new Error("qualification handoff identity changed");
  }
  const artifactId = required("QUALIFICATION_HANDOFF_ARTIFACT_ID", /^[1-9][0-9]*$/);
  const artifact = await github(
    `/repos/${identityCheck.repository}/actions/artifacts/${artifactId}`,
  );
  if (
    String(artifact.id) !== artifactId ||
    artifact.name !== `image-qualification-handoff-${identityCheck.runId}-1` ||
    artifact.expired !== false ||
    artifact.digest !==
      normalizeArtifactDigest(required("QUALIFICATION_HANDOFF_ARTIFACT_DIGEST")) ||
    String(artifact.workflow_run?.id) !== identityCheck.runId ||
    artifact.workflow_run?.head_sha !== identityCheck.workflowSha
  ) {
    throw new Error("qualification handoff artifact changed");
  }
  assertHandoffWindow(handoff);
  return handoff;
}

function transportV2Enabled() {
  const profile = process.env.QUALIFICATION_TRANSPORT_PROFILE;
  if (profile !== undefined && profile !== "" && profile !== "candidate-transport-v2")
    throw new Error("unknown transport profile");
  return profile === "candidate-transport-v2";
}

function candidateBindings(identity, adminToken, sharedToken, config, stage = false) {
  const vars = {
    CRABBOX_DEFAULT_ORG: "image-qualification",
    CRABBOX_GITHUB_ALLOWED_ORG: "example-org.invalid",
    CRABBOX_WORKSPACE_PROVIDER: "aws",
    CRABBOX_WORKSPACE_CLASS: "standard",
    CRABBOX_WORKSPACE_PREWARM_COUNT: "0",
    CRABBOX_AWS_REGION: config.region,
    CRABBOX_CAPACITY_REGIONS: config.region,
    CRABBOX_AWS_AMI: config.retainedImage ? "" : config.baseAmiId,
    CRABBOX_AWS_SUBNET_ID: config.subnetId,
    CRABBOX_AWS_SECURITY_GROUP_ID: config.securityGroupId,
    CRABBOX_AWS_ROOT_GB: String(config.rootGB),
    CRABBOX_AWS_INSTANCE_PROFILE: "",
    CRABBOX_AWS_ORPHAN_SWEEP_ENABLED: "0",
    CRABBOX_AWS_ORPHAN_SWEEP_DELETE: "0",
    CRABBOX_MAX_ACTIVE_LEASES: "1",
    CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER: "1",
    CRABBOX_MAX_ACTIVE_LEASES_PER_ORG: "1",
    CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN: "1",
    CRABBOX_MAX_CHECKPOINTS: "1",
    CRABBOX_MAX_CHECKPOINTS_PER_OWNER: "1",
    CRABBOX_MAX_CHECKPOINTS_PER_ORG: "1",
    CRABBOX_MAX_MONTHLY_USD: "10",
    CRABBOX_MAX_MONTHLY_USD_PER_OWNER: "10",
    CRABBOX_MAX_MONTHLY_USD_PER_ORG: "10",
  };
  return [
    ...Object.entries(vars).map(([name, text]) => ({ name, type: "plain_text", text })),
    { name: "CRABBOX_ADMIN_TOKEN", type: "secret_text", text: adminToken },
    { name: "CRABBOX_SHARED_TOKEN", type: "secret_text", text: sharedToken },
    ...(transportV2Enabled() && stage
      ? []
      : [
          {
            name: "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
            type: "service",
            service: authorityName,
            props: identity,
          },
        ]),
    ...(transportV2Enabled() ? transportMarkers(stage ? "stage-v2" : "final-v2", identity) : []),
    { name: "FLEET", type: "durable_object_namespace", class_name: "FleetDurableObject" },
    { name: "CF_VERSION_METADATA", type: "version_metadata" },
  ];
}

function candidateMetadata(mainModule, bindings, createNamespace = false) {
  return {
    main_module: mainModule.replace(/^worker\//, ""),
    compatibility_date: "2026-09-03",
    compatibility_flags: ["nodejs_compat"],
    bindings,
    ...(createNamespace
      ? { migrations: { new_tag: "qualification-v1", new_sqlite_classes: ["FleetDurableObject"] } }
      : {}),
  };
}

function controllerMetadata(deploymentHash, token, v2 = transportV2Enabled()) {
  return {
    main_module: "controller.mjs",
    compatibility_date: "2026-09-03",
    bindings: [
      ...(v2 ? transportMarkers("controller-v2", { deploymentHash }) : []),
      { name: "CONTROLLER_TOKEN", type: "secret_text", text: token },
      {
        name: "AUTHORITY",
        type: "service",
        service: authorityName,
        entrypoint: "AWSQualificationController",
        props: { deploymentHash },
      },
    ],
  };
}

function relayMetadata(
  candidateWorker,
  executorToken,
  adminToken,
  sharedToken,
  config,
  owner,
  expiresAt,
) {
  return {
    main_module: "relay.mjs",
    compatibility_date: "2026-09-03",
    bindings: [
      ...(transportV2Enabled()
        ? transportMarkers("relay-v2", { candidateWorker, deploymentHash: "0".repeat(64) })
        : []),
      { name: "EXECUTOR_TOKEN", type: "secret_text", text: executorToken },
      { name: "CANDIDATE_ADMIN_TOKEN", type: "secret_text", text: adminToken },
      { name: "CANDIDATE_SHARED_TOKEN", type: "secret_text", text: sharedToken },
      { name: "AWS_REGION", type: "plain_text", text: config.region },
      { name: "QUALIFICATION_OWNER", type: "plain_text", text: owner },
      { name: "QUALIFICATION_ORG", type: "plain_text", text: "image-qualification" },
      { name: "QUALIFICATION_EXPIRES_AT", type: "plain_text", text: expiresAt },
      ...(config.firstWalk || config.retainedImage
        ? [
            {
              name: "QUALIFICATION_MODE",
              type: "plain_text",
              text: config.keyAuthorizationOnly
                ? "key-authorization"
                : config.firstWalk
                  ? "first-walk"
                  : "retained",
            },
          ]
        : []),
      { name: "CANDIDATE", type: "service", service: candidateWorker },
    ],
  };
}

const publicBindingShape = (bindings) => bindingShape(transportBindingShape(bindings));

function executionManifest(identity) {
  // The final version cannot be embedded in its own Worker bindings. Bind it
  // here, then re-read both version and bindings immediately before execution.
  return {
    version: 1,
    runId: identity.runId,
    candidateSha: identity.candidateSha,
    candidateWorker: identity.candidateWorker,
    candidateVersion: identity.candidateVersion,
    relayWorker: identity.relayWorker,
    relayVersion: identity.relayVersion,
    relayBindingDigest: identity.relayBindingDigest,
    deploymentHash: identity.deploymentHash,
    manifestSha256: identity.manifestSha256,
    bindingDigest: identity.bindingDigest,
    authoritySha: identity.authoritySha,
    authorityVersion: identity.authorityVersion,
    policyHash: identity.policyHash,
    enrolledAt: identity.enrolledAt,
    expiresAt: identity.expiresAt,
    ...(identity.retainedImage ? { retainedImage: identity.retainedImage } : {}),
    ...(identity.firstWalk ? { firstWalk: true } : {}),
    ...(identity.taggingContract ? { taggingContract: identity.taggingContract } : {}),
    ...(identity.keyAuthorizationOnly ? { keyAuthorizationOnly: true } : {}),
  };
}

export function executionManifestDigest(identity) {
  return digest(canonical(executionManifest(identity)));
}

function qualificationExpectedFromEnv({ optional = false } = {}) {
  const runId = process.env.QUALIFICATION_EXPECTED_RUN_ID?.trim() ?? "";
  if (optional && !runId) return undefined;
  const retainedImage = retainedImageFromEnv();
  return {
    runId: required("QUALIFICATION_EXPECTED_RUN_ID", runIdPattern),
    candidateSha: required("QUALIFICATION_EXPECTED_CANDIDATE_SHA", sha40),
    candidateWorker: required("QUALIFICATION_EXPECTED_CANDIDATE_WORKER", workerNamePattern),
    candidateVersion: boundedString("QUALIFICATION_EXPECTED_CANDIDATE_VERSION", 128),
    relayWorker: required("QUALIFICATION_EXPECTED_RELAY_WORKER", relayNamePattern),
    relayVersion: boundedString("QUALIFICATION_EXPECTED_RELAY_VERSION", 128),
    relayBindingDigest: required("QUALIFICATION_EXPECTED_RELAY_BINDING_DIGEST", sha64),
    deploymentHash: required("QUALIFICATION_EXPECTED_DEPLOYMENT_HASH", sha64),
    manifestSha256: required("QUALIFICATION_EXPECTED_MANIFEST_SHA", sha64),
    bindingDigest: required("QUALIFICATION_EXPECTED_BINDING_DIGEST", sha64),
    authoritySha: required("QUALIFICATION_EXPECTED_AUTHORITY_SHA", sha40),
    authorityVersion: boundedString("QUALIFICATION_EXPECTED_AUTHORITY_VERSION", 64),
    policyHash: required("QUALIFICATION_EXPECTED_POLICY_HASH", sha64),
    enrolledAt: required("QUALIFICATION_EXPECTED_ENROLLED_AT"),
    expiresAt: required("QUALIFICATION_EXPECTED_EXPIRES_AT"),
    executionManifestDigest: required("QUALIFICATION_EXPECTED_EXECUTION_MANIFEST_DIGEST", sha64),
    ...(retainedImage ? { retainedImage } : {}),
    ...(privateQualificationMode(process.env.QUALIFICATION_MODE)
      ? { firstWalk: true, taggingContract: qualificationTaggingContract }
      : {}),
    ...(process.env.QUALIFICATION_MODE === "key-authorization"
      ? { keyAuthorizationOnly: true }
      : {}),
  };
}

export function verifyAttestationIdentity(attestation, expected, { finalized } = {}) {
  if (!expected) throw new Error("protected qualification identity is absent");
  const fields = [
    "runId",
    "candidateSha",
    "candidateWorker",
    "deploymentHash",
    "authoritySha",
    "authorityVersion",
    "policyHash",
    "enrolledAt",
    "expiresAt",
    "taggingContract",
  ];
  if (fields.some((field) => attestation?.[field] !== expected[field])) {
    throw new Error("authority attestation identity does not match protected expectations");
  }
  if (Boolean(attestation?.keyAuthorizationOnly) !== Boolean(expected.keyAuthorizationOnly))
    throw new Error("authority key-only purpose differs from protected expectations");
  const enrolledAt = Date.parse(attestation.enrolledAt);
  const expiresAt = Date.parse(attestation.expiresAt);
  const finalizingAt = Date.parse(attestation.finalizingAt ?? "");
  const finalizedAt = Date.parse(attestation.finalizedAt ?? "");
  if (
    attestation.version !== 1 ||
    !Number.isFinite(enrolledAt) ||
    !Number.isFinite(expiresAt) ||
    enrolledAt >= expiresAt ||
    (finalized === true &&
      (!attestation.finalized ||
        !Number.isFinite(finalizingAt) ||
        !Number.isFinite(finalizedAt) ||
        finalizingAt < enrolledAt ||
        finalizedAt < finalizingAt))
  ) {
    throw new Error("authority attestation timestamps or finalized state are invalid");
  }
}

async function verifyExecutionDeployment(cf, expected) {
  if (
    expected.candidateWorker !== `crabbox-${expected.runId}` ||
    expected.relayWorker !== relayNameForRun(expected.runId)
  ) {
    throw new Error("relay Worker does not match the registered qualification run");
  }
  const [settings, candidateVersion, relaySettings, relayVersion] = await Promise.all([
    verifyOwned(cf, expected.candidateWorker),
    cf.latestVersion(expected.candidateWorker),
    verifyOwned(cf, expected.relayWorker),
    cf.latestVersion(expected.relayWorker),
  ]);
  await Promise.all([
    assertWorkerIsolation(cf, expected.candidateWorker, 1, false),
    assertWorkerIsolation(cf, expected.relayWorker, 0, true),
  ]);
  if (candidateVersion !== expected.candidateVersion) {
    throw new Error("candidate Worker version changed after protected deployment");
  }
  if (relayVersion !== expected.relayVersion) {
    throw new Error("qualification relay version changed after protected deployment");
  }
  const bindings = publicBindingShape(settings.bindings ?? []);
  const relayBindings = publicBindingShape(relaySettings.bindings ?? []);
  const profile = transportProfile(settings.bindings ?? []);
  const transport = bindings.find(
    (binding) =>
      binding.name === "CRABBOX_AWS_QUALIFICATION_TRANSPORT" &&
      binding.type === "service" &&
      binding.service === authorityName,
  );
  const transportProps = profile?.profile === "final-v2" ? profile.identity : transport?.props;
  if (profile?.profile === "final-v2") {
    await cf.verifyController();
    const controllerURL = await cf.enableSubdomain(controllerName);
    const reply = await controllerCall(
      cf,
      controllerURL,
      required("QUALIFICATION_CONTROLLER_TOKEN"),
      "transport-discover",
    );
    const proof = reply?.probe;
    if (
      !proof ||
      proof.protocol !== 1 ||
      proof.state !== "verified" ||
      canonical(proof.identity) !== canonical(profile.identity) ||
      proof.candidateVersion !== candidateVersion ||
      proof.authorityVersion !== cf.admission.authority.version
    )
      throw new Error("execution lacks canonical candidate transport proof");
  }
  const relayExpiry = relayBindings.find(
    (binding) => binding.name === "QUALIFICATION_EXPIRES_AT" && binding.type === "plain_text",
  );
  if (
    digest(canonical(bindings)) !== expected.bindingDigest ||
    transportProps?.runId !== expected.runId ||
    transportProps?.candidateSha !== expected.candidateSha ||
    transportProps?.candidateWorker !== expected.candidateWorker ||
    transportProps?.deploymentHash !== expected.deploymentHash ||
    transportProps?.expiresAt !== expected.expiresAt ||
    transportProps?.taggingContract !== expected.taggingContract ||
    Boolean(transportProps?.firstWalk) !== Boolean(expected.firstWalk) ||
    Boolean(transportProps?.keyAuthorizationOnly) !== Boolean(expected.keyAuthorizationOnly) ||
    canonical(transportProps?.retainedImage ?? null) !==
      canonical(expected.retainedImage ?? null) ||
    relayExpiry?.text !== expected.expiresAt ||
    digest(canonical(relayBindings)) !== expected.relayBindingDigest ||
    relayBindings.some(
      (binding) =>
        binding.type === "service" &&
        (binding.name !== "CANDIDATE" || binding.service !== expected.candidateWorker),
    ) ||
    executionManifestDigest(expected) !== expected.executionManifestDigest
  ) {
    throw new Error("candidate Worker content, settings, or execution manifest changed");
  }
  return {
    candidateVersion,
    bindingDigest: digest(canonical(bindings)),
    relayVersion,
    relayBindingDigest: digest(canonical(relayBindings)),
  };
}

export async function controllerCall(cf, url, token, action, value = {}, authorityCF = cf) {
  const discovery = ["discover", "transport-discover"].includes(action);
  if (
    discovery &&
    (!value ||
      Object.getPrototypeOf(value) !== Object.prototype ||
      Reflect.ownKeys(value).length !== 0)
  )
    throw new Error("discovery requires an empty input");
  const signal = discovery ? cf.recoverySignal?.(action) : undefined;
  const request = async (observation = {}) => {
    const previousSignals = [cf.discoverySignal, authorityCF.discoverySignal];
    if (observation.signal) {
      cf.discoverySignal = observation.signal;
      authorityCF.discoverySignal = observation.signal;
    }
    try {
      observation.signal?.throwIfAborted();
      await verifyAuthority(authorityCF);
      await cf.verifyController();
      if (cf.beforeRecoveryCall) await cf.beforeRecoveryCall(action);
      observation.signal?.throwIfAborted();
      const deadlines = [
        cf.controllerDeadline,
        observation.deadline,
        action === "transport-open" ? Date.parse(value.expiresAt) : undefined,
        action === "claim" ? Date.parse(value.identity?.expiresAt) : undefined,
      ].filter((value) => value !== undefined);
      try {
        return await requestControllerJSON({
          url,
          token,
          action,
          value,
          signal: observation.signal ?? cf.recoverySignal?.(action),
          ...(deadlines.length ? { deadline: Math.min(...deadlines) } : {}),
        });
      } catch (error) {
        if (error instanceof ControllerHTTPError && process.env.QUALIFICATION_PROOF_DIR) {
          try {
            writeJSON(
              path.join(
                path.resolve(process.env.QUALIFICATION_PROOF_DIR),
                `controller-http-failure-${crypto.randomUUID()}.json`,
              ),
              error.receipt,
            );
          } catch {
            console.error("qualification controller HTTP failure receipt could not be written");
          }
        }
        throw error;
      }
    } finally {
      if (observation.signal) {
        cf.discoverySignal = previousSignals[0];
        authorityCF.discoverySignal = previousSignals[1];
      }
    }
  };
  if (!discovery) return await request();
  const result = await observeControllerDiscovery(request, {
    action,
    deadline: cf.controllerDeadline,
    signal,
  });
  if (action === "discover") discoveredRun(result);
  else if (Object.keys(result).join(",") !== "probe")
    throw new Error("invalid transport discovery");
  return result;
}

async function cloudflareFromEnv() {
  const admission = readAdmission();
  const cf = new Cloudflare(
    required("CLOUDFLARE_ACCOUNT_ID", /^[0-9a-f]{32}$/),
    required("CLOUDFLARE_API_TOKEN"),
  );
  cf.admission = admission;
  await verifyAuthority(cf);
  return cf;
}

async function deployController(cf, deploymentHash, token) {
  await requireAbsent(cf, controllerName);
  await cf.upload(controllerName, controllerSource, controllerMetadata(deploymentHash, token));
  await cf.verifyController();
  return await cf.enableSubdomain(controllerName);
}

async function preparePrivateCandidate(cf, worker, bindings) {
  const bootstrap = path.join(
    process.env.RUNNER_TEMP ?? "/tmp",
    `qualification-private-bootstrap-${process.pid}.mjs`,
  );
  fs.writeFileSync(
    bootstrap,
    "export class FleetDurableObject {}\nexport default {fetch(){return new Response(null,{status:404})}};\n",
    { mode: 0o600 },
  );
  try {
    await cf.upload(worker, bootstrap, candidateMetadata("private-bootstrap.mjs", bindings, true));
    await cf.disableSubdomain(worker);
  } finally {
    fs.rmSync(bootstrap, { force: true });
  }
}

async function assertWorkerIsolation(cf, worker, durableObjectCount, workersDev) {
  const [routes, schedules, domains, namespaceCount, subdomain] = await Promise.all([
    cf.request(`/workers/services/${worker}/environments/production/routes?show_zonename=true`),
    cf.request(`/workers/scripts/${worker}/schedules`),
    cf.list(`/workers/domains?service=${worker}`),
    namespaceResidue(cf, worker),
    cf.request(`/workers/scripts/${worker}/subdomain`),
  ]);
  if (
    (routes ?? []).length !== 0 ||
    !Array.isArray(schedules?.schedules) ||
    schedules.schedules.length !== 0 ||
    (domains ?? []).length !== 0 ||
    namespaceCount !== durableObjectCount ||
    subdomain?.enabled !== workersDev ||
    subdomain?.previews_enabled !== false
  ) {
    throw new Error(`Worker isolation verification failed for ${worker}`);
  }
}

// Only first-upload visibility for this run's relay is reconciled. No writes,
// credential changes, enrollment, deadline resets or general API retries here.
function assertRelayReadbackReserve(handoff) {
  assertHandoffWindow(handoff);
  const end = Date.parse(handoff.workExpiresAt) - 60_000;
  if (Date.now() >= end) throw new Error("relay readback has no subsequent-gate reserve");
  return end;
}

function relayDeploymentList(value) {
  if (Array.isArray(value)) return value;
  if (!value || typeof value !== "object") return undefined;
  if (Object.hasOwn(value, "deployments")) return value.deployments;
  return value.items;
}

// Persist structure and typed identifiers only, never arbitrary provider fields.
function relayDeploymentSummary(value) {
  const list = relayDeploymentList(value);
  if (!Array.isArray(list)) return { classification: "malformed", count: null };
  if (list.length === 0) return { classification: "empty", count: 0 };
  const identifier = (id) => {
    if (typeof id !== "string") return null;
    try {
      return workerVersion(id);
    } catch {
      return null;
    }
  };
  const active = list[0];
  const versions = Array.isArray(active?.versions) ? active.versions : null;
  return {
    classification: versions ? "nonempty" : "malformed",
    count: list.length,
    activeDeploymentId: identifier(active?.id),
    versionCount: versions?.length ?? null,
    versions:
      versions?.slice(0, 2).map((version) => ({
        id: identifier(version?.version_id),
        percentage:
          typeof version?.percentage === "number" && Number.isFinite(version.percentage)
            ? version.percentage
            : null,
      })) ?? null,
  };
}

async function readbackRelay(cf, worker, upload, expectedBindings, handoff) {
  const directory = path.resolve(required("QUALIFICATION_PROOF_DIR"));
  const readbackStopsAt = Date.now() + 10_000;
  const receipt = {
    version: 1,
    worker,
    upload: upload.response,
    // Acknowledged creation is never proof of serving state.
    state: "unresolved",
    reads: [],
  };
  return await withQualificationJournal(
    directory,
    {
      stateName: "relay-readback.json",
      lockName: "relay-readback.lock",
      create: true,
      initial: () => receipt,
    },
    async (retained, save) => {
      if (canonical(retained) !== canonical(receipt))
        throw new Error(
          "relay readback receipt already exists; use recovery, never replay deployment",
        );
      receipt.acknowledgedVersion = workerVersion(upload.result?.deployment_id);
      await save(receipt);
      const stopsAt = Math.min(readbackStopsAt, assertRelayReadbackReserve(handoff));
      const stop = new AbortController();
      const cancel = () => stop.abort();
      const signal = AbortSignal.any([
        stop.signal,
        AbortSignal.timeout(Math.max(1, stopsAt - Date.now())),
      ]);
      process.once("SIGINT", cancel);
      process.once("SIGTERM", cancel);
      const check = () => {
        if (signal.aborted || Date.now() >= stopsAt)
          throw new Error("qualification relay readback cancelled or deadline exhausted");
      };
      const read = async (suffix) => {
        check();
        const observed = { endpoint: suffix, state: "pending" };
        receipt.reads.push(observed);
        await save(receipt);
        // HTTP404 and a valid empty deployment list are unresolved visibility.
        // Denial, malformed data and nonempty drift still fail without retries.
        let result;
        try {
          const response = await cf.request(
            `/workers/scripts/${worker}/${suffix}`,
            { signal, redirect: "error" },
            { allow404: true, boundedBytes: 64 * 1024, responseMetadata: true },
          );
          observed.response = response.response;
          result = response.result;
          observed.state = result === undefined ? "http404" : "visible";
          if (suffix === "deployments" && result !== undefined)
            observed.deployment = relayDeploymentSummary(result);
        } catch (error) {
          observed.state = "failed";
          if (Number.isInteger(error.status)) observed.response = { status: error.status };
          await save(receipt);
          throw error;
        }
        await save(receipt);
        check();
        return result;
      };
      try {
        for (let round = 0; round < 6; round++) {
          const settings = await read("settings");
          if (settings !== undefined) {
            assertOwnedSettings(cf, worker, settings);
            if (canonical(publicBindingShape(settings.bindings)) !== canonical(expectedBindings))
              throw new Error(
                "qualification relay settings do not match the reviewed binding contract",
              );
          }
          const deployments = await read("deployments");
          let deploymentVerified = false;
          if (deployments !== undefined) {
            const list = relayDeploymentList(deployments);
            if (!Array.isArray(list))
              throw new Error("qualification relay deployments are malformed");
            if (list.length !== 0) {
              const active = list[0];
              if (
                !Array.isArray(active?.versions) ||
                active.versions.length !== 1 ||
                active.versions[0]?.percentage !== 100 ||
                workerVersion(active.versions[0]?.version_id) !== receipt.acknowledgedVersion
              )
                throw new Error(
                  "qualification relay active version differs from acknowledged upload",
                );
              deploymentVerified = true;
            }
          }
          if (settings !== undefined && deploymentVerified) {
            check();
            receipt.state = "verified";
            await save(receipt);
            return receipt.acknowledgedVersion;
          }
          if (round < 5) await delay(250, undefined, { signal });
        }
        throw new Error("qualification relay readback remains unresolved after six rounds");
      } finally {
        process.removeListener("SIGINT", cancel);
        process.removeListener("SIGTERM", cancel);
      }
    },
  );
}

// This gate belongs only to the initial, acknowledged relay deployment. A
// stale read may be observed again; an uncertain exposure write is never replayed.
export async function enableRelaySubdomain(
  cf,
  worker,
  expectedVersion,
  expectedBindings,
  handoff,
  { signal: callerSignal, pause = delay } = {},
) {
  if (
    !runIdPattern.test(handoff.runId ?? "") ||
    worker !== handoff.relayWorker ||
    worker !==
      `crabbox-image-qualification-relay-${handoff.runId.slice("image-qualification-".length)}` ||
    !Number.isFinite(Date.parse(handoff.workExpiresAt))
  )
    throw new Error("qualification relay exposure identity is invalid");
  workerVersion(expectedVersion);
  const startedAt = Date.now();
  const stopsAt = Math.min(startedAt + 60_000, assertRelayReadbackReserve(handoff));
  const receipt = {
    version: 1,
    worker,
    runId: handoff.runId,
    expectedVersion,
    bindingDigest: digest(canonical(expectedBindings)),
    state: "new",
    observations: [],
  };
  return await withQualificationJournal(
    path.resolve(required("QUALIFICATION_PROOF_DIR")),
    {
      stateName: "relay-exposure.json",
      lockName: "relay-exposure.lock",
      create: true,
      initial: () => receipt,
    },
    async (retained, save) => {
      if (canonical(retained) !== canonical(receipt))
        throw new Error(
          "relay exposure receipt already exists; use recovery, never replay deployment",
        );
      // Claim local custody before any request, including on a no-write failure.
      receipt.state = "checking";
      await save(receipt);
      const stop = new AbortController();
      const timer = setTimeout(() => stop.abort(), Math.max(1, stopsAt - Date.now()));
      const cancel = () => stop.abort();
      const signal = AbortSignal.any([stop.signal, ...(callerSignal ? [callerSignal] : [])]);
      process.once("SIGINT", cancel);
      process.once("SIGTERM", cancel);
      let requests = 0;
      const check = () => {
        if (signal.aborted || Date.now() >= stopsAt)
          throw new Error("qualification relay exposure cancelled or deadline exhausted");
        assertRelayReadbackReserve(handoff);
      };
      const request = async (pathname, init = {}, options = {}) => {
        check();
        if (requests >= 400)
          throw new Error("qualification relay exposure request budget exhausted");
        requests++;
        const result = await cf.request(
          pathname,
          { ...init, signal, redirect: "error" },
          {
            ...options,
            boundedBytes: 64 * 1024,
          },
        );
        check();
        return result;
      };
      // Reuse the existing validators with bounded, GET-only management access.
      // Do not mutate the original client's signals or generic subdomain methods.
      const reader = Object.create(cf);
      reader.request = async (pathname, init = {}, options = {}) => {
        if ((init.method ?? "GET") !== "GET") throw new Error("relay exposure fence is read-only");
        return await request(pathname, init, options);
      };
      const fence = async () => {
        await verifyAuthority(reader);
        if ((await reader.latestVersion(worker)) !== expectedVersion)
          throw new Error("qualification relay exposure version drifted");
        const settings = await verifyOwned(reader, worker);
        if (canonical(publicBindingShape(settings.bindings)) !== canonical(expectedBindings))
          throw new Error("qualification relay exposure bindings drifted");
        const [routes, schedules, domains] = await Promise.all([
          reader.request(
            `/workers/services/${worker}/environments/production/routes?show_zonename=true`,
          ),
          reader.request(`/workers/scripts/${worker}/schedules`),
          reader.list(`/workers/domains?service=${worker}`),
        ]);
        if (
          canonical(routes) !== "[]" ||
          canonical(schedules?.schedules) !== "[]" ||
          domains.length !== 0 ||
          (await reader.latestVersion(worker)) !== expectedVersion
        )
          throw new Error("qualification relay exposure isolation or version drifted");
      };
      const exposure = (value) => {
        if (typeof value?.enabled !== "boolean" || typeof value?.previews_enabled !== "boolean")
          throw new Error("qualification relay exposure readback is malformed");
        return { enabled: value.enabled, previews_enabled: value.previews_enabled };
      };
      const endpoint = `/workers/scripts/${worker}/subdomain`;
      try {
        await fence();
        receipt.initial = exposure(await reader.request(endpoint));
        receipt.state = "write-intent";
        await save(receipt);
        // No catch/retry path can issue another POST, even after an uncertain response.
        const ack = await request(
          endpoint,
          {
            method: "POST",
            headers: { "content-type": "application/json" },
            body: JSON.stringify({ enabled: true, previews_enabled: false }),
          },
          { envelope: true },
        );
        if (ack?.success !== true)
          throw new Error("qualification relay exposure write is unconfirmed");
        receipt.state = "acknowledged";
        await save(receipt);
        for (let round = 0; round < 12; round++) {
          await fence();
          const result = await reader.request(endpoint, {}, { responseMetadata: true });
          const actual = exposure(result.result);
          receipt.observations.push({
            exposure: actual,
            response: result.response,
            elapsedMs: Date.now() - startedAt,
          });
          await save(receipt);
          if (actual.enabled === true && actual.previews_enabled === false) {
            // Bracket the successful exposure read with the same complete fence.
            await fence();
            const account = await reader.request("/workers/subdomain");
            if (
              typeof account?.subdomain !== "string" ||
              !/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(account.subdomain)
            )
              throw new Error("Cloudflare account workers.dev subdomain is invalid");
            receipt.state = "verified";
            await save(receipt);
            check();
            return `https://${worker}.${account.subdomain}.workers.dev`;
          }
          if (canonical(actual) !== canonical(receipt.initial))
            throw new Error("qualification relay exposure changed to an unexpected state");
          if (round < 11) {
            check();
            await pause(5_000, undefined, { signal });
            check();
          }
        }
        throw new Error(
          "qualification relay exposure remains unresolved after twelve observations",
        );
      } finally {
        stop.abort();
        clearTimeout(timer);
        process.removeListener("SIGINT", cancel);
        process.removeListener("SIGTERM", cancel);
        receipt.requests = requests;
        await save(receipt);
      }
    },
  );
}

async function deploy() {
  const identityCheck = await verifyCandidateIdentity({ artifact: true });
  const artifactDir = path.resolve(required("QUALIFICATION_ARTIFACT_DIR"));
  const manifest = verifyManifest(
    artifactDir,
    identityCheck.candidateSha,
    identityCheck.workflowSha,
  );
  verifyPublisherContract(
    fs.readFileSync(path.join(artifactDir, "candidate/scripts/mint-aws-devtools-image.sh"), "utf8"),
  );
  const handoff = await verifyHandoff(identityCheck, manifest.manifestSha256);
  const { runId, candidateWorker, relayWorker, owner, expiresAt } = handoff;
  if (!workerNamePattern.test(candidateWorker)) throw new Error("candidate Worker name is invalid");
  if (!relayNamePattern.test(relayWorker)) throw new Error("relay Worker name is invalid");
  const config = qualificationConfig();
  const authoritySha = required("QUALIFICATION_AUTHORITY_SHA", sha40);
  const authorityVersion = boundedString("QUALIFICATION_AUTHORITY_VERSION", 64);
  const policyHash = required("QUALIFICATION_EXPECTED_POLICY_HASH", sha64);
  const baseIdentity = {
    runId,
    owner,
    candidateSha: identityCheck.candidateSha,
    candidateWorker,
    expiresAt,
    ...(config.retainedImage ? { retainedImage: config.retainedImage } : {}),
    ...(config.firstWalk ? { firstWalk: true, taggingContract: qualificationTaggingContract } : {}),
    ...(config.keyAuthorizationOnly ? { keyAuthorizationOnly: true } : {}),
  };
  const adminToken = crypto.randomBytes(32).toString("hex");
  const sharedToken = crypto.randomBytes(32).toString("hex");
  const { token: executorToken, tokenSha256: executorTokenSha256 } = readExecutorCredential({
    ...process.env,
    QUALIFICATION_EXPIRES_AT: expiresAt,
  });
  executorTokenForRedaction = executorToken;
  const controllerToken = required("QUALIFICATION_CONTROLLER_TOKEN");
  const placeholderIdentity = { ...baseIdentity, deploymentHash: "0".repeat(64) };
  const stagingBindings = candidateBindings(
    placeholderIdentity,
    adminToken,
    sharedToken,
    config,
    true,
  );
  const mainFile = path.join(artifactDir, manifest.mainModule);
  const extraModules = manifest.files
    .filter(
      (entry) =>
        entry.path.startsWith("worker/") &&
        entry.path !== manifest.mainModule &&
        /\.(?:m?js|wasm)$/.test(entry.path),
    )
    .map((entry) => ({
      name: entry.path.replace(/^worker\//, ""),
      file: path.join(artifactDir, entry.path),
      type: entry.path.endsWith(".wasm") ? "application/wasm" : "application/javascript+module",
    }));
  const cf = await cloudflareFromEnv();
  cf.controllerDeadline = Date.parse(handoff.workExpiresAt);
  await Promise.all(
    [candidateWorker, relayWorker, controllerName].map((name) => requireAbsent(cf, name)),
  );
  // Only this inert trusted bootstrap can be briefly public on first creation.
  // Candidate bytes are uploaded only after workers.dev is observably disabled.
  await requireAbsent(cf, candidateWorker);
  await preparePrivateCandidate(cf, candidateWorker, stagingBindings);
  const privateSettings = await cf.settings(candidateWorker);
  await ownedNamespaces(cf, candidateWorker, privateSettings.bindings);
  const namespaceId = privateSettings.bindings.find(
    (binding) => binding.name === "FLEET",
  )?.namespace_id;
  stagingBindings.find((binding) => binding.name === "FLEET").namespace_id = namespaceId;
  await verifyOwned(cf, candidateWorker);
  await cf.upload(
    candidateWorker,
    mainFile,
    candidateMetadata(manifest.mainModule, stagingBindings),
    extraModules,
  );
  const [stagingSettings, stagingVersion] = await Promise.all([
    cf.settings(candidateWorker),
    cf.latestVersion(candidateWorker),
  ]);
  const stagingShape = {
    bindings: publicBindingShape(stagingSettings.bindings ?? []),
    compatibilityDate: stagingSettings.compatibility_date,
    compatibilityFlags: stagingSettings.compatibility_flags ?? [],
  };
  if (canonical(stagingShape.bindings) !== canonical(publicBindingShape(stagingBindings))) {
    throw new Error("staging Worker settings do not match the reviewed binding contract");
  }
  const deploymentHash = digest(
    canonical({
      version: 1,
      manifestSha256: manifest.manifestSha256,
      candidateArtifact: {
        digest: identityCheck.artifactDigest,
        id: identityCheck.artifactId,
        runId: identityCheck.runId,
      },
      config,
      stagingVersion,
      stagingSettings: stagingShape,
      authority: { name: authorityName, sha: authoritySha, version: authorityVersion },
      props: baseIdentity,
      relay: {
        name: relayWorker,
        sourceSha256: digest(fs.readFileSync(relaySource)),
        settings: publicBindingShape(
          relayMetadata(
            candidateWorker,
            executorToken,
            adminToken,
            sharedToken,
            config,
            owner,
            expiresAt,
          ).bindings,
        ),
      },
      secretDigests: {
        admin: digest(adminToken),
        shared: digest(sharedToken),
        executor: digest(executorToken),
      },
    }),
  );
  const identity = { ...baseIdentity, deploymentHash };
  const bindings = candidateBindings(identity, adminToken, sharedToken, config);
  bindings.find((binding) => binding.name === "FLEET").namespace_id = namespaceId;
  await verifyOwned(cf, candidateWorker);
  const finalUpload = await cf.upload(
    candidateWorker,
    mainFile,
    candidateMetadata(manifest.mainModule, bindings),
    extraModules,
  );
  const [settings, candidateVersion] = await Promise.all([
    cf.settings(candidateWorker),
    cf.latestVersion(candidateWorker),
  ]);
  const actualBindings = publicBindingShape(settings.bindings ?? []);
  const expectedBindings = publicBindingShape(bindings);
  if (
    workerVersion(finalUpload.deployment_id) !== candidateVersion ||
    canonical(actualBindings) !== canonical(expectedBindings)
  ) {
    throw new Error("candidate Worker settings do not match the reviewed binding contract");
  }
  const bindingDigest = digest(canonical(expectedBindings));
  const expectedRelayMetadata = relayMetadata(
    candidateWorker,
    executorToken,
    adminToken,
    sharedToken,
    config,
    owner,
    expiresAt,
  );
  await requireAbsent(cf, relayWorker);
  // Acknowledgement is required: uncertain PUTs never enter a replay/retry path.
  assertRelayReadbackReserve(handoff);
  const relayUpload = await cf.upload(relayWorker, relaySource, expectedRelayMetadata, [], {
    responseMetadata: true,
  });
  const expectedRelayBindings = publicBindingShape(expectedRelayMetadata.bindings);
  const relayVersion = await readbackRelay(
    cf,
    relayWorker,
    relayUpload,
    expectedRelayBindings,
    handoff,
  );
  const relayURL = await enableRelaySubdomain(
    cf,
    relayWorker,
    relayVersion,
    expectedRelayBindings,
    handoff,
  );
  const relayBindingDigest = digest(canonical(expectedRelayBindings));
  const controllerURL = await deployController(cf, deploymentHash, controllerToken);
  await Promise.all([
    assertWorkerIsolation(cf, candidateWorker, 1, false),
    assertWorkerIsolation(cf, relayWorker, 0, true),
    assertWorkerIsolation(cf, controllerName, 0, true),
  ]);
  await verifyCandidateIdentity({ artifact: true });
  assertHandoffWindow(handoff);
  if (transportV2Enabled()) {
    const request = {
      identity,
      candidateVersion,
      authorityVersion: cf.admission.authority.version,
      challenge: crypto.randomBytes(32).toString("hex"),
      expiresAt: new Date(Math.min(Date.now() + 10 * 60_000, Date.parse(expiresAt))).toISOString(),
    };
    const proofDirectory = path.join(
      path.resolve(required("QUALIFICATION_PROOF_DIR")),
      "transport-custody",
    );
    const verifyFences = async () => {
      await verifyAuthority(cf);
      await cf.verifyController();
      for (const [worker, version, namespaceCount, publicURL] of [
        [candidateWorker, candidateVersion, 1, false],
        [relayWorker, relayVersion, 0, true],
      ]) {
        await verifyOwned(cf, worker);
        await assertWorkerIsolation(cf, worker, namespaceCount, publicURL);
        if ((await cf.latestVersion(worker)) !== version)
          throw new Error("transport proof deployment drift");
      }
    };
    await withQualificationJournal(
      proofDirectory,
      {
        stateName: "transport-state.json",
        lockName: "transport-state.lock",
        create: true,
        initial: () => ({ request, phase: "prepared" }),
      },
      async (state, save) => {
        if (canonical(state.request) !== canonical(request))
          throw new Error("transport intent already exists; recovery required");
        await proveCandidateTransport({
          request,
          save,
          controller: (action, input) =>
            controllerCall(cf, controllerURL, controllerToken, action, input),
          verifyFences,
          invokeCandidate: async (challenge) => {
            const response = await fetch(`${relayURL}/v1/qualification/transport-probe`, {
              method: "POST",
              headers: {
                authorization: `Bearer ${executorToken}`,
                "content-type": "application/json",
              },
              body: JSON.stringify({ challenge }),
              signal: AbortSignal.timeout(10_000),
              redirect: "error",
            });
            // Never inspect or accept a candidate-supplied identity, status or body.
            await response.body?.cancel();
          },
        });
      },
    );
    await verifyFences();
  }
  const record = await controllerCall(cf, controllerURL, controllerToken, "claim", { identity });
  if (record.runId !== runId || record.cleanupState !== "claimed")
    throw new Error("authority claim readback mismatch");
  const attestation = await controllerCall(cf, controllerURL, controllerToken, "attest", { runId });
  const protectedIdentity = {
    ...identity,
    candidateVersion,
    relayWorker,
    relayVersion,
    relayBindingDigest,
    manifestSha256: manifest.manifestSha256,
    bindingDigest,
    authoritySha,
    authorityVersion,
    policyHash,
    enrolledAt: attestation.enrolledAt,
  };
  verifyAttestationIdentity(attestation, protectedIdentity, { finalized: false });
  const manifestDigest = executionManifestDigest(protectedIdentity);
  const proofDir = path.resolve(required("QUALIFICATION_PROOF_DIR"));
  writeJSON(path.join(proofDir, "deployment.json"), {
    version: 1,
    handoffSha256: digest(canonical(handoff)),
    executorTokenSha256,
    runId,
    candidateSha: identity.candidateSha,
    workflowSha: identityCheck.workflowSha,
    deploymentHash,
    manifestSha256: manifest.manifestSha256,
    candidateArtifactDigest: identityCheck.artifactDigest,
    candidateArtifactId: identityCheck.artifactId,
    candidateRunId: identityCheck.runId,
    candidateWorker,
    candidateVersionDigest: digest(candidateVersion),
    relayWorker,
    relayVersionDigest: digest(relayVersion),
    executionManifestDigest: manifestDigest,
    authoritySha,
    authorityVersion,
    policyHash,
    enrolledAt: attestation.enrolledAt,
    expiresAt,
    isolation: {
      candidate: {
        workersDev: false,
        previewURLs: false,
        routes: 0,
        customDomains: 0,
        schedules: 0,
        fleetDurableObjectNamespaces: 1,
      },
      relay: {
        workersDev: true,
        previewURLs: false,
        routes: 0,
        customDomains: 0,
        schedules: 0,
        durableObjectNamespaces: 0,
      },
    },
    limits: {
      instanceTypes: config.instanceTypes,
      maxMinutes: config.maxMinutes,
      maxMonthlyUSD: config.maxMonthlyUSD,
      maxConcurrentInstances: config.maxConcurrentInstances,
      maxLaunches: config.maxLaunches,
      fastSnapshotRestore: config.fastSnapshotRestore,
      rootGB: config.rootGB,
    },
    policyConfigDigest: digest(canonical(config)),
    bindingDigest,
    relayBindingDigest,
  });
  appendOutput("run_id", runId);
  appendOutput("candidate_worker", candidateWorker);
  appendOutput("relay_worker", relayWorker);
  appendOutput("relay_version", relayVersion);
  appendOutput("relay_binding_digest", relayBindingDigest);
  appendOutput("relay_url", relayURL);
  appendOutput("executor_token_sha256", executorTokenSha256);
  appendOutput("deployment_hash", deploymentHash);
  appendOutput("candidate_sha", identity.candidateSha);
  appendOutput("candidate_version", candidateVersion);
  appendOutput("manifest_sha", manifest.manifestSha256);
  appendOutput("binding_digest", bindingDigest);
  appendOutput("authority_sha", authoritySha);
  appendOutput("authority_version", authorityVersion);
  appendOutput("policy_hash", policyHash);
  appendOutput("enrolled_at", attestation.enrolledAt);
  appendOutput("execution_manifest_digest", manifestDigest);
  appendOutput("expires_at", expiresAt);
  appendOutput("region", config.region);
  appendOutput("base_ami_id", config.baseAmiId);
}

async function arm() {
  const expected = qualificationExpectedFromEnv();
  const identityCheck = await verifyCandidateIdentity({ artifact: true });
  const handoff = await verifyHandoff(identityCheck, expected.manifestSha256);
  for (const field of [
    "runId",
    "candidateSha",
    "candidateWorker",
    "relayWorker",
    "expiresAt",
    "authoritySha",
    "authorityVersion",
    "policyHash",
  ]) {
    if (expected[field] !== handoff[field])
      throw new Error("deployment differs from prepared handoff");
  }
  const cf = await cloudflareFromEnv();
  cf.controllerDeadline = Date.parse(handoff.workExpiresAt);
  await verifyExecutionDeployment(cf, expected);
  const token = required("QUALIFICATION_CONTROLLER_TOKEN");
  await cf.verifyController();
  const controllerURL = await cf.enableSubdomain(controllerName);
  const discovered = await controllerCall(cf, controllerURL, token, "discover");
  if (
    discovered.run?.runId !== expected.runId ||
    discovered.run?.candidateSha !== expected.candidateSha ||
    discovered.run?.candidateWorker !== expected.candidateWorker ||
    discovered.run?.deploymentHash !== expected.deploymentHash ||
    discovered.run?.expiresAt !== expected.expiresAt ||
    discovered.run?.taggingContract !== expected.taggingContract ||
    Boolean(discovered.run?.keyAuthorizationOnly) !== Boolean(expected.keyAuthorizationOnly) ||
    discovered.run?.cleanupState !== "claimed"
  ) {
    throw new Error("authority registry identity does not match protected expectations");
  }
  const attestation = await controllerCall(cf, controllerURL, token, "attest", {
    runId: expected.runId,
  });
  verifyAttestationIdentity(attestation, expected, { finalized: false });
  assertHandoffWindow(handoff);
  const armedAt = new Date().toISOString();
  const proofDir = path.resolve(required("QUALIFICATION_PROOF_DIR"));
  writeJSON(path.join(proofDir, "execution-manifest.json"), {
    version: 1,
    runId: expected.runId,
    candidateSha: expected.candidateSha,
    deploymentHash: expected.deploymentHash,
    candidateVersionDigest: digest(expected.candidateVersion),
    relayWorker: expected.relayWorker,
    relayVersionDigest: digest(expected.relayVersion),
    relayBindingDigest: expected.relayBindingDigest,
    executionManifestDigest: expected.executionManifestDigest,
    authoritySha: expected.authoritySha,
    authorityVersion: expected.authorityVersion,
    policyHash: expected.policyHash,
    enrolledAt: expected.enrolledAt,
    expiresAt: expected.expiresAt,
    armedAt,
  });
  appendOutput("armed_at", armedAt);
  if (expected.keyAuthorizationOnly) {
    // Protected code alone owns this fixed diagnostic; no candidate execution.
    const result = await controllerCall(cf, controllerURL, token, "key-authorization", {
      runId: expected.runId,
    });
    if (result?.passed !== true || result?.checks !== 13 || Object.keys(result).length !== 2)
      throw new Error("key-only authorization proof is incomplete; finalize original owner");
    writeJSON(path.join(proofDir, "key-authorization.json"), {
      version: 2,
      taggingContract: qualificationTaggingContract,
      passed: true,
      checks: 13,
    });
  }
}

async function namespaceResidue(cf, worker) {
  return (await namespaces(cf)).filter((namespace) => namespace.script === worker).length;
}

async function deleteCandidate(cf, worker) {
  if (!workerNamePattern.test(worker)) throw new Error("refusing to delete an unexpected Worker");
  const existing = await verifyOwned(cf, worker, { allowAbsent: true });
  if (!existing) return;
  const tombstone = path.join(
    process.env.RUNNER_TEMP ?? "/tmp",
    `qualification-tombstone-${process.pid}.mjs`,
  );
  fs.writeFileSync(
    tombstone,
    "export default {fetch(){return new Response(null,{status:410})}};\n",
    { mode: 0o600 },
  );
  try {
    if (existing.bindings.some((binding) => binding.type === "durable_object_namespace"))
      await cf.upload(worker, tombstone, {
        main_module: "tombstone.mjs",
        compatibility_date: "2026-09-03",
        keep_bindings: ["secret_text"],
        bindings: existing.bindings.filter((binding) =>
          [
            "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
            "QUALIFICATION_TRANSPORT_PROFILE",
            "QUALIFICATION_TRANSPORT_IDENTITY",
          ].includes(binding.name),
        ),
        migrations: {
          old_tag: "qualification-v1",
          new_tag: "qualification-delete",
          deleted_classes: ["FleetDurableObject"],
        },
      });
    await verifyOwned(cf, worker);
    await cf.deleteScript(worker);
  } finally {
    fs.rmSync(tombstone, { force: true });
  }
  const settings = await cf.request(`/workers/scripts/${worker}/settings`, {}, { allow404: true });
  if (settings !== undefined || (await namespaceResidue(cf, worker)) !== 0) {
    throw new Error("candidate Worker or Fleet Durable Object residue remains");
  }
}

async function deleteRelay(cf, worker) {
  if (!relayNamePattern.test(worker)) throw new Error("refusing to delete an unexpected relay");
  const existing = await verifyOwned(cf, worker, { allowAbsent: true });
  if (existing === undefined) {
    if ((await namespaceResidue(cf, worker)) !== 0) {
      throw new Error("qualification relay Durable Object residue remains");
    }
    return;
  }
  await cf.disableSubdomain(worker);
  await assertWorkerIsolation(cf, worker, 0, false);
  await cf.deleteScript(worker);
  const settings = await cf.request(`/workers/scripts/${worker}/settings`, {}, { allow404: true });
  if (settings !== undefined || (await namespaceResidue(cf, worker)) !== 0) {
    throw new Error("qualification relay Worker or Durable Object residue remains");
  }
}

async function qualificationCandidates(cf) {
  const scripts = await cf.list("/workers/scripts");
  const candidates = [];
  for (const worker of scripts
    .map((item) => item.id ?? item.name)
    .filter((name) => workerNamePattern.test(name))) {
    const settings = await verifyOwned(cf, worker);
    const transport = (settings.bindings ?? []).find(
      (binding) =>
        binding.name === "CRABBOX_AWS_QUALIFICATION_TRANSPORT" &&
        binding.service === authorityName &&
        binding.props?.candidateWorker === worker &&
        sha64.test(binding.props?.deploymentHash ?? ""),
    );
    const profile = transportProfile(settings.bindings ?? []);
    if (profile)
      candidates.push({ worker, deploymentHash: profile.identity.deploymentHash, v2: true });
    else if (transport) candidates.push({ worker, deploymentHash: transport.props.deploymentHash });
  }
  return candidates;
}

async function qualificationRelays(cf) {
  const scripts = await cf.list("/workers/scripts");
  const relays = [];
  for (const worker of scripts
    .map((item) => item.id ?? item.name)
    .filter((name) => relayNamePattern.test(name))) {
    const settings = await verifyOwned(cf, worker);
    const candidateWorker = worker.replace(
      "crabbox-image-qualification-relay-",
      "crabbox-image-qualification-",
    );
    const candidate = (settings.bindings ?? []).find(
      (binding) =>
        binding.name === "CANDIDATE" &&
        binding.type === "service" &&
        binding.service === candidateWorker,
    );
    if (candidate) relays.push({ worker, candidateWorker });
  }
  return relays;
}

function discoveredRun(value) {
  const response = objectAt(value, "authority registry discovery");
  if (Object.keys(response).length !== 1 || !Object.hasOwn(response, "run")) {
    throw new Error("authority registry discovery is invalid");
  }
  if (response.run === null) return null;
  const run = objectAt(response.run, "authority registry run");
  if (
    !runIdPattern.test(run.runId ?? "") ||
    !workerNamePattern.test(run.candidateWorker ?? "") ||
    run.candidateWorker !== `crabbox-${run.runId}` ||
    !sha64.test(run.deploymentHash ?? "")
  ) {
    throw new Error("authority registry run identity is invalid");
  }
  return run;
}

async function requireControllerAbsent(cf) {
  const settings = await cf.request(
    `/workers/scripts/${controllerName}/settings`,
    {},
    { allow404: true },
  );
  if (settings !== undefined) {
    throw new Error("qualification controller exists; refusing to replace it");
  }
}

async function discoveryControllerVersion(cf, metadata, expectedVersion) {
  const version = await cf.verifyController();
  const settings = await cf.settings(controllerName);
  if (
    (expectedVersion !== undefined && version !== expectedVersion) ||
    canonical([...(settings.tags ?? [])].sort()) !== canonical([...metadata.tags].sort()) ||
    canonical(publicBindingShape(settings.bindings ?? [])) !==
      canonical(publicBindingShape(metadata.bindings)) ||
    (await cf.latestVersion(controllerName)) !== version
  ) {
    throw new Error("discovery controller ownership changed or is unverified");
  }
  return version;
}

function safeControllerResponse(response) {
  return {
    status: response.status,
    ...(/^[a-f0-9]{16}-[A-Z]{3}$/.test(response.ray ?? "") ? { ray: response.ray } : {}),
  };
}

// The upload API's deployment_id is the serving version identifier, as in the
// existing relay readback contract. Missing acknowledgement never permits replay.
async function observeControllerManagement(cf, metadata, expectedVersion, pause) {
  const until = Math.min(Date.now() + 60_000, cf.controllerObservationDeadline);
  const originalSignal = cf.discoverySignal;
  const observation = new AbortController();
  const timer = setTimeout(() => observation.abort(), Math.max(1, until - Date.now()));
  cf.discoverySignal = AbortSignal.any([originalSignal, observation.signal]);
  try {
    for (let attempt = 0; attempt < 12; attempt++) {
      if (Date.now() >= until || cf.discoverySignal.aborted)
        throw new Error("controller management observation cancelled or exhausted");
      await verifyAuthority(cf);
      // Validate visible settings even while the deployment view is unavailable.
      const settings = await cf.request(
        `/workers/scripts/${controllerName}/settings`,
        {},
        { allow404: true },
      );
      if (settings !== undefined) {
        assertOwnedSettings(cf, controllerName, settings);
        if (
          canonical(publicBindingShape(settings.bindings ?? [])) !==
            canonical(publicBindingShape(metadata.bindings)) ||
          canonical([...(settings.tags ?? [])].sort()) !== canonical([...metadata.tags].sort())
        )
          throw new Error("controller upload settings drifted");
        await ownedNamespaces(cf, controllerName, settings.bindings);
      }
      // ONLY this owned first-upload deployment view's HTTP404 is observable again.
      // Malformed/empty/split/foreign deployments, denial and other endpoints fail.
      try {
        const version = await discoveryControllerVersion(cf, metadata, expectedVersion);
        await verifyAuthority(cf);
        cf.discoverySignal.throwIfAborted();
        if (Date.now() >= until) throw new Error("controller management observation exhausted");
        return version;
      } catch (error) {
        if (
          error.status !== 404 ||
          error.pathname !== `/workers/scripts/${controllerName}/deployments`
        )
          throw error;
        if (attempt === 11) throw error;
        await pause(Math.min(5_000, Math.max(1, until - Date.now())), undefined, {
          signal: cf.discoverySignal,
        });
      }
    }
  } finally {
    clearTimeout(timer);
    cf.discoverySignal = originalSignal;
  }
}

export async function discoverWithOwnedController(
  originalCF,
  deploymentHash,
  token,
  { idleOnly = false, probeOnly = false, v2 = transportV2Enabled(), signal, pause = delay } = {},
) {
  const metadata = {
    ...controllerMetadata(deploymentHash, token, v2),
    tags: ownershipTags(
      originalCF,
      controllerName,
      controllerMetadata(deploymentHash, token, v2).bindings,
    ),
  };
  const identity = digest(
    canonical({
      authority: originalCF.admission.authority,
      metadata: {
        ...metadata,
        bindings: publicBindingShape(metadata.bindings),
      },
    }),
  );
  const directory = path.join(
    path.resolve(required("QUALIFICATION_PROOF_DIR")),
    `controller-${identity}`,
  );
  const started = Date.now();
  const cycleEnd = Math.min(started + 180_000, originalCF.controllerDeadline ?? Infinity);
  const forwardEnd = Math.min(started + 120_000, cycleEnd - 60_000);
  const receipt = { version: 1, identity, stage: "new", settlement: "not-needed", events: [] };
  return await withQualificationJournal(
    directory,
    {
      stateName: "provisioning.json",
      lockName: "provisioning.lock",
      create: true,
      initial: () => receipt,
    },
    async (retained, save) => {
      if (canonical(retained) !== canonical(receipt))
        throw new Error(
          "controller provisioning receipt exists; use recovery, never replay upload",
        );
      const forward = new AbortController(),
        cleanup = new AbortController();
      const cancel = () => forward.abort();
      const timer = setTimeout(cancel, Math.max(1, forwardEnd - Date.now()));
      const cleanupTimer = setTimeout(() => cleanup.abort(), Math.max(1, cycleEnd - Date.now()));
      process.once("SIGINT", cancel);
      process.once("SIGTERM", cancel);
      const cf = Object.create(originalCF);
      cf.discoverySignal = AbortSignal.any([
        forward.signal,
        ...(signal ? [signal] : []),
        ...(originalCF.discoverySignal ? [originalCF.discoverySignal] : []),
      ]);
      cf.controllerDeadline = forwardEnd;
      cf.controllerObservationDeadline = forwardEnd;
      // Canonical discovery temporarily installs its own observation signal.
      // Keep provisioning cancellation in that signal and in runtime requests.
      cf.recoverySignal = (action) =>
        AbortSignal.any([
          cf.discoverySignal,
          ...(originalCF.recoverySignal ? [originalCF.recoverySignal(action)] : []),
        ]);
      let cleaning = false;
      const request = originalCF.request;
      cf.request = async (pathname, init = {}, options = {}) => {
        const active = cf.discoverySignal;
        if (
          active.aborted ||
          Date.now() >= (cleaning ? cycleEnd : forwardEnd) ||
          receipt.events.length >= (cleaning ? 800 : 600)
        )
          throw new Error("controller provisioning request cancelled or bound exhausted");
        const own = `/workers/scripts/${controllerName}`;
        const event = {
          phase: receipt.stage,
          method: init.method ?? "GET",
          endpoint: pathname.startsWith(own)
            ? pathname.slice(own.length) || "upload"
            : "isolation-fence",
          category: managementEndpoint(pathname),
          elapsedMs: Date.now() - started,
          outcome: "unconfirmed",
        };
        receipt.events.push(event);
        await save(receipt);
        try {
          const content = options.contentIdentityBytes || options.probeContentIdentity;
          const result = await request.call(
            cf,
            pathname,
            {
              ...init,
              redirect: "error",
              signal: AbortSignal.any([active, ...(init.signal ? [init.signal] : [])]),
            },
            {
              ...options,
              // The Worker list is unpaginated and includes shared-account metadata.
              // Keep acknowledgements and all other management reads at 64KiB.
              boundedBytes:
                pathname === "/workers/scripts" && (init.method ?? "GET") === "GET"
                  ? 1024 * 1024
                  : 64 * 1024,
              ...(!content ? { responseMetadata: true } : {}),
            },
          );
          event.outcome = "acknowledged";
          if (!content) event.response = safeControllerResponse(result.response);
          else event.contentSha256 = result.sha256;
          await save(receipt);
          return content || options.responseMetadata ? result : result.result;
        } catch (error) {
          event.outcome = "failed";
          event.failure = managementFailure(error) ?? {
            endpoint: managementEndpoint(pathname),
            reason: active.aborted ? "cancelled" : "request-failed",
          };
          if (error.responseInfo) event.response = safeControllerResponse(error.responseInfo);
          await save(receipt);
          throw error;
        }
      };
      let version,
        failure,
        controllerURL,
        discovered,
        retainForFinalization = false,
        uploadAttempted = false;
      try {
        receipt.stage = "absence";
        await save(receipt);
        await verifyAuthority(cf);
        await requireAbsent(cf, controllerName);
        // A previous exact caller may have been retired by transport recovery.
        // This new upload is bound to its own acknowledgement below.
        cf.controllerVersion = undefined;
        receipt.stage = "upload";
        receipt.settlement = "required";
        await save(receipt);
        uploadAttempted = true;
        const ack = await cf.upload(controllerName, controllerSource, metadata, [], {
          envelope: true,
          responseMetadata: true,
        });
        receipt.upload = {
          response: safeControllerResponse(ack.response),
          acknowledged: ack.result?.success === true,
        };
        await save(receipt);
        if (ack.result?.success !== true)
          throw new Error("controller upload acknowledgement is unconfirmed");
        receipt.acknowledgedVersion = workerVersion(ack.result?.result?.deployment_id);
        await save(receipt);
        receipt.stage = "management-readiness";
        await save(receipt);
        version = await observeControllerManagement(
          cf,
          metadata,
          receipt.acknowledgedVersion,
          pause,
        );
        receipt.verifiedVersion = version;
        receipt.stage = "exposure";
        await save(receipt);
        controllerURL = await cf.enableSubdomain(controllerName);
        await discoveryControllerVersion(cf, metadata, version);
        receipt.stage = "discovery";
        await save(receipt);
        if (probeOnly) {
          discovered = await controllerCall(cf, controllerURL, token, "transport-discover");
          if (!discovered || Object.keys(discovered).join(",") !== "probe")
            throw new Error("invalid probe discovery");
        } else {
          if (v2 && !idleOnly) await recoverTransportProbe(cf, controllerURL, token);
          discovered = await controllerCall(cf, controllerURL, token, "discover");
        }
        const run = probeOnly ? null : discoveredRun(discovered);
        if (idleOnly && run !== null)
          throw new Error("idle discovery found an active authority registry run");
        cf.discoverySignal.throwIfAborted();
        receipt.discovery = probeOnly ? "probe-observed" : run === null ? "idle" : "active";
        retainForFinalization = !idleOnly && !probeOnly;
      } catch (error) {
        failure = error instanceof Error ? error.message : String(error);
        receipt.failedStage = receipt.stage;
        receipt.failure = managementFailure(error) ?? { reason: "verification-failed" };
      } finally {
        if (uploadAttempted && !retainForFinalization) {
          cleaning = true;
          receipt.stage = "cleanup";
          // User cancellation stops forward work; the original owner retains its
          // reserved cleanup time. No upload or deletion is ever replayed.
          cf.discoverySignal = AbortSignal.any([
            cleanup.signal,
            ...(originalCF.discoverySignal ? [originalCF.discoverySignal] : []),
          ]);
          cf.controllerObservationDeadline = cycleEnd;
          try {
            await save(receipt);
            await verifyAuthority(cf);
            const settings = await cf.request(
              `/workers/scripts/${controllerName}/settings`,
              {},
              { allow404: true },
            );
            if (settings !== undefined || receipt.acknowledgedVersion) {
              const expected = version ?? receipt.acknowledgedVersion;
              if (expected) await observeControllerManagement(cf, metadata, expected, pause);
              else await discoveryControllerVersion(cf, metadata);
              await cf.deleteScript(controllerName);
            }
            await requireAbsent(cf, controllerName);
            const deployment = await cf.request(
              `/workers/scripts/${controllerName}/deployments`,
              {},
              { allow404: true },
            );
            const scripts = await cf.list("/workers/scripts");
            if (
              deployment !== undefined ||
              scripts.some((s) => (s.id ?? s.name) === controllerName)
            )
              throw new Error("controller cleanup remains unconfirmed");
            receipt.settlement =
              receipt.acknowledgedVersion || settings !== undefined
                ? "deleted-and-absent"
                : "absent-without-upload-acknowledgement";
          } catch (error) {
            receipt.settlement = "unresolved";
            receipt.cleanupFailure = managementFailure(error) ?? { reason: "verification-failed" };
            const message = error instanceof Error ? error.message : String(error);
            failure = failure
              ? `${failure}; probe cleanup: ${message}`
              : `probe cleanup: ${message}`;
          }
        } else if (retainForFinalization) {
          // Transport recovery may have replaced this caller under its exact
          // canonical owner; controllerCall reverified that replacement.
          originalCF.controllerVersion = cf.controllerVersion;
          receipt.retainedVersion = cf.controllerVersion;
          receipt.settlement = "retained-for-original-finalizer";
        }
        receipt.stage = "complete";
        receipt.outcome = failure ? "failed" : "verified";
        try {
          await save(receipt);
        } finally {
          clearTimeout(timer);
          clearTimeout(cleanupTimer);
          process.removeListener("SIGINT", cancel);
          process.removeListener("SIGTERM", cancel);
        }
      }
      if (failure) throw new Error(failure);
      return { controllerURL, discovered };
    },
  );
}

async function recoverTransportProbe(cf, controllerURL, token) {
  const reply = await controllerCall(cf, controllerURL, token, "transport-discover");
  if (!reply || Object.keys(reply).join(",") !== "probe")
    throw new Error("invalid transport discovery");
  if (reply.probe === null) return;
  const { protocol, state, observedAt, ...request } = reply.probe;
  if (protocol !== 1 || !["pending", "observed", "verified", "cancelled"].includes(state))
    throw new Error("invalid canonical transport probe");
  // A retained tombstone is historical custody, not this controller's mutation
  // target. It must not block cleanup of a later pre-probe partial deployment.
  if (["verified", "cancelled"].includes(state)) return;
  const settings = await verifyOwned(cf, controllerName);
  const owner = transportProfile(settings.bindings ?? [])?.identity.deploymentHash;
  if (owner !== request.identity.deploymentHash) {
    // No candidate/registry mutation and no force: replace only our exact trusted
    // caller, then let the existing recovery path settle the canonical owner.
    await cf.verifyController();
    await cf.disableSubdomain(controllerName);
    await assertWorkerIsolation(cf, controllerName, 0, false);
    await cf.deleteScript(controllerName);
    await requireControllerAbsent(cf);
    await discoverWithOwnedController(cf, request.identity.deploymentHash, token, { v2: true });
    return;
  }
  await settleCandidateTransport({
    request,
    controller: (action, input) => controllerCall(cf, controllerURL, token, action, input),
    save: async () => {}, // canonical settlement survives loss of local artifacts
  });
}

async function cleanupRun({
  reaper = false,
  requireProof = false,
  expected,
  initialQualificationFailure,
  recovery,
} = {}) {
  const cf = recovery?.callerCF ?? (await cloudflareFromEnv());
  const token = recovery?.token ?? required("QUALIFICATION_CONTROLLER_TOKEN");
  const call = (url, action, value = {}) =>
    controllerCall(cf, url, token, action, value, recovery?.authorityCF ?? cf);
  let controllerURL;
  let discovered;
  let idleProbe = false;
  try {
    const settings = await cf.request(
      `/workers/scripts/${controllerName}/settings`,
      {},
      { allow404: true },
    );
    if (settings !== undefined) {
      await cf.verifyController();
      controllerURL = await cf.enableSubdomain(controllerName);
      if (!recovery && transportProfile(settings.bindings ?? [])?.profile === "controller-v2")
        await recoverTransportProbe(cf, controllerURL, token);
      discovered = await call(controllerURL, "discover");
    } else {
      if (recovery) {
        if (!recovery.state.retired)
          throw new Error("original controller unavailable before verified retirement");
        await recovery.verifyAbsentCallers();
        return {
          result: "finalized",
          runId: expected.runId,
          attestation: recovery.state.attestation,
        };
      }
      if (!reaper) throw new Error("qualification controller is unavailable");
      const candidates = await qualificationCandidates(cf);
      for (const candidate of candidates) {
        try {
          ({ controllerURL, discovered } = await discoverWithOwnedController(
            cf,
            candidate.deploymentHash,
            token,
            { v2: Boolean(candidate.v2) },
          ));
          break;
        } catch (error) {
          // A failed recovery may have left a controller. Do not overwrite it
          // with another candidate or an idle probe without confirmed absence.
          try {
            await requireControllerAbsent(cf);
          } catch (ownershipError) {
            throw new Error(`${error.message}; recovery: ${ownershipError.message}`);
          }
          if (candidate === candidates.at(-1)) throw error;
        }
      }
      if (candidates.length === 0 && transportV2Enabled()) {
        // The canonical registry outlives lost candidate/relay/local artifacts.
        // Discover without mutation, delete that temporary caller, then bind only
        // to the exact owner returned by the trusted authority for settlement.
        const observation = await discoverWithOwnedController(
          cf,
          digest("crabbox/image-qualification/transport-discovery/v2"),
          token,
          { probeOnly: true, v2: true },
        );
        if (observation.discovered.probe !== null) {
          ({ controllerURL, discovered } = await discoverWithOwnedController(
            cf,
            observation.discovered.probe.identity.deploymentHash,
            token,
            { v2: true },
          ));
        }
      }
      if (candidates.length === 0 && !discovered) {
        // A shape-valid controller can discover only an empty registry. Never
        // use this domain-separated hash to finalize an active run.
        ({ discovered } = await discoverWithOwnedController(
          cf,
          digest("crabbox/image-qualification/idle-discovery/v1"),
          token,
          { idleOnly: true },
        ));
        idleProbe = true;
      }
    }
    discoveredRun(discovered);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    return {
      result: "failed",
      failure: initialQualificationFailure
        ? `${initialQualificationFailure}; discovery: ${message}`
        : message,
    };
  }
  if (discovered.run === null) {
    if (recovery) {
      if (!recovery.state.attestation)
        throw new Error("idle registry without original finalization proof");
      await recovery.checkpoint("retired", recovery.state.attestation);
      await deleteRelay(cf, relayNameForRun(expected.runId));
      await deleteCandidate(cf, expected.candidateWorker);
      await cf.verifyController();
      await cf.deleteScript(controllerName);
      await recovery.verifyAbsentCallers();
      return {
        result: "finalized",
        runId: expected.runId,
        attestation: recovery.state.attestation,
      };
    }
    try {
      for (const relay of await qualificationRelays(cf)) {
        await deleteRelay(cf, relay.worker);
      }
      for (const candidate of await qualificationCandidates(cf)) {
        await deleteCandidate(cf, candidate.worker);
      }
      if (!idleProbe) {
        await cf.verifyController();
        await cf.deleteScript(controllerName);
      }
    } catch (error) {
      const message = `idle cleanup: ${error instanceof Error ? error.message : String(error)}`;
      return {
        result: "failed",
        failure: initialQualificationFailure
          ? `${initialQualificationFailure}; ${message}`
          : message,
      };
    }
    if (initialQualificationFailure || requireProof) {
      return {
        result: "failed",
        failure:
          initialQualificationFailure ?? "authority registry has no active qualification run",
        cleanup: "idle",
      };
    }
    return { result: "idle", reason: "registry has no active run" };
  }
  const run = discovered.run;
  if (recovery) {
    for (const key of ["runId", "candidateSha", "candidateWorker", "deploymentHash", "expiresAt"])
      if (run[key] !== expected[key]) throw new Error("recovery registry owner differs");
    const original = await call(controllerURL, "attest", { runId: run.runId });
    verifyAttestationIdentity(original, expected);
    if (!original.finalizingAt) throw new Error("recovery owner is not already finalizing");
  }
  const relayWorker = relayNameForRun(run.runId);
  let failure;
  let firstAttestation;
  let qualificationFailure = initialQualificationFailure;
  // Qualification drift must fail the run, but only after teardown has had its
  // full chance to finalize every protected resource.
  const recordQualificationFailure = (error) => {
    const message = error instanceof Error ? error.message : String(error);
    qualificationFailure = qualificationFailure ? `${qualificationFailure}; ${message}` : message;
  };
  if (requireProof) {
    try {
      if (!expected) throw new Error("protected qualification identity is absent");
      if (
        run.runId !== expected.runId ||
        run.candidateSha !== expected.candidateSha ||
        run.candidateWorker !== expected.candidateWorker ||
        run.deploymentHash !== expected.deploymentHash ||
        run.expiresAt !== expected.expiresAt ||
        run.taggingContract !== expected.taggingContract ||
        Boolean(run.keyAuthorizationOnly) !== Boolean(expected.keyAuthorizationOnly)
      ) {
        throw new Error("authority registry identity does not match protected expectations");
      }
      await verifyExecutionDeployment(cf, expected);
    } catch (error) {
      recordQualificationFailure(error);
    }
  }
  try {
    // Persist the authority fence before revoking ingress. Requests already admitted
    // by the relay can then reach neither the signer nor a reopened run.
    await cf.verifyController();
    await call(controllerURL, "begin-finalization", { runId: run.runId });
    await deleteRelay(cf, relayWorker);
    await call(controllerURL, "finalize", { runId: run.runId });
    firstAttestation = await call(controllerURL, "attest", {
      runId: run.runId,
    });
    if (requireProof) {
      try {
        verifyAttestationIdentity(firstAttestation, expected, { finalized: true });
        verifyQualificationEvidence(
          firstAttestation,
          expected?.keyAuthorizationOnly ? {} : readExecutionProof(),
          expected?.retainedImage,
          expected?.firstWalk,
          expected?.keyAuthorizationOnly,
        );
      } catch (error) {
        recordQualificationFailure(error);
      }
    }
    await deleteCandidate(cf, run.candidateWorker);
    await call(controllerURL, "finalize", { runId: run.runId });
    const finalAttestation = await call(controllerURL, "attest", {
      runId: run.runId,
    });
    if (
      !finalAttestation.finalized ||
      Object.values(finalAttestation.finalReceipt?.finalCounts ?? {}).some(Number)
    ) {
      throw new Error("authority did not attest zero AWS residue");
    }
    if (recovery) {
      verifyAttestationIdentity(finalAttestation, expected, { finalized: true });
      if (
        canonical(finalAttestation.finalReceipt?.finalCounts) !==
          canonical({ images: 0, instances: 0, keyPairs: 0, snapshots: 0, volumes: 0 }) ||
        finalAttestation.finalReceipt?.failureCodes?.length
      )
        throw new Error("recovery final receipt is incomplete");
      await recovery.checkpoint("finalized", finalAttestation);
    }
    if (requireProof) {
      try {
        verifyAttestationIdentity(finalAttestation, expected, { finalized: true });
      } catch (error) {
        recordQualificationFailure(error);
      }
    }
    await call(controllerURL, "retire", { runId: run.runId });
    const after = await call(controllerURL, "discover");
    if (after.run !== null) throw new Error("authority registry still has an active run");
    if (recovery) await recovery.checkpoint("retired", finalAttestation);
    await cf.verifyController();
    await cf.deleteScript(controllerName);
    if (recovery) await recovery.verifyAbsentCallers();
    if (qualificationFailure) {
      return {
        result: "failed",
        runId: run.runId,
        failure: qualificationFailure,
        cleanup: "finalized",
        attestation: finalAttestation,
      };
    }
    return { result: "finalized", runId: run.runId, attestation: finalAttestation };
  } catch (error) {
    failure = error instanceof Error ? error.message : String(error);
    try {
      await deleteRelay(cf, relayWorker);
    } catch (relayError) {
      const relayFailure = relayError instanceof Error ? relayError.message : String(relayError);
      failure = `${failure}; relay cleanup: ${relayFailure}`;
    }
    return {
      result: "failed",
      runId: run.runId,
      failure: qualificationFailure ? `${qualificationFailure}; cleanup: ${failure}` : failure,
      attestation: firstAttestation,
    };
  }
}

// Only the dedicated transition owner supplies this dependency; normal reaper,
// deployment and candidate paths never admit a second identity record.
export async function recoverActiveQualification(recovery, expected) {
  return await cleanupRun({ recovery, expected });
}

// Read through the existing, authenticated original controller. Never create a
// caller or enable a hostname while deciding whether cleanup issuance is safe.
export async function observeRecoveryOwner({ callerCF, authorityCF, token }, expected) {
  const domain = await callerCF.request("/workers/subdomain");
  const exposure = await callerCF.request(`/workers/scripts/${controllerName}/subdomain`);
  if (
    !/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(domain?.subdomain ?? "") ||
    exposure?.enabled !== true ||
    exposure.previews_enabled !== false
  )
    throw new Error("original recovery controller is unavailable");
  const url = `https://${controllerName}.${domain.subdomain}.workers.dev`;
  const discovered = await controllerCall(callerCF, url, token, "discover", {}, authorityCF);
  for (const key of ["runId", "candidateSha", "candidateWorker", "deploymentHash", "expiresAt"])
    if (discovered?.run?.[key] !== expected[key])
      throw new Error("original recovery registry owner differs");
  const attestation = await controllerCall(
    callerCF,
    url,
    token,
    "attest",
    { runId: expected.runId },
    authorityCF,
  );
  verifyAttestationIdentity(attestation, expected);
  const finalizing = Date.parse(attestation.finalizingAt ?? "");
  if (
    attestation.finalized !== false ||
    !Number.isFinite(finalizing) ||
    finalizing < Date.parse(attestation.enrolledAt)
  )
    throw new Error("original recovery owner is not awaiting finalization");
  return attestation;
}

function readExecutionProof() {
  const directory = process.env.QUALIFICATION_EXECUTION_PROOF_DIR?.trim();
  if (!directory || !fs.existsSync(directory))
    throw new Error("execution proof artifact is absent");
  const checksumFile = path.join(directory, "checksums.sha256");
  const lines = fs.readFileSync(checksumFile, "utf8").trim().split("\n").filter(Boolean);
  for (const line of lines) {
    const match = line.match(/^([0-9a-f]{64})  (\.\/[A-Za-z0-9_.-]+)$/);
    if (!match) throw new Error("execution proof checksum manifest is invalid");
    const file = path.join(directory, match[2].slice(2));
    if (digest(fs.readFileSync(file)) !== match[1]) {
      throw new Error("execution proof checksum mismatch");
    }
  }
  if (privateQualificationMode(process.env.QUALIFICATION_MODE)) {
    // A failed executor may stop before either observation file exists. Keep
    // this a qualification failure; cleanupRun still exports its independent
    // authoritative cleanup evidence and settles every admitted resource.
    if (
      !fs.existsSync(path.join(directory, "first-walk.json")) ||
      !fs.existsSync(path.join(directory, "first-walk-result.json"))
    )
      throw new Error("first-walk observation unavailable: execution proof is incomplete");
    return {
      ...readJSON(path.join(directory, "first-walk.json")),
      result: readJSON(path.join(directory, "first-walk-result.json")),
    };
  }
  return {
    spoof: readJSON(path.join(directory, "spoofed-admin.json")),
    fsr: readJSON(path.join(directory, "fsr-denial.json")),
    catalog: readJSON(path.join(directory, "catalog-rollback.json")),
    execution: readJSON(path.join(directory, "execution-state.json")),
    hardKill: readJSON(path.join(directory, "executor-hard-kill.json")),
    log: fs.readFileSync(path.join(directory, "candidate-execution.log"), "utf8"),
  };
}

// A fixed public data profile, prepared before capture with no repository hooks.
// These identities also bind the sealed operational request to the exact workload.
export function verifyFirstWalkPreparation(prepared) {
  if (
    prepared?.completed !== true ||
    prepared.profile !== "fixed-node-monorepo-v1" ||
    prepared.sourceCommit !== "8436c62514241a0afdef84bc2abb658dbdf57d40" ||
    prepared.sourceArchiveSha256 !==
      "ebaaa5f50f6b3ba54f825eaed7c602838e0a5eea936aee9b5212085591084dda" ||
    prepared.lockSha256 !== "0c50a34a1f4f78e7fa5da6340878f54da7f6b9f36a0c04c3f67c61ab8fbb7d06" ||
    prepared.nodeSha256 !== "6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6" ||
    prepared.pnpmSha256 !== "bcbd17511de1718a262be1acc42066559bbce8fcfb1adb584d1f5e8ad70fe218" ||
    prepared.scriptsDisabled !== true ||
    prepared.hooksDisabled !== true ||
    prepared.sameFilesystem !== true ||
    prepared.cachePopulated !== true ||
    !Number.isSafeInteger(prepared.inventory?.entries) ||
    prepared.inventory.entries <= 0 ||
    prepared.inventory.entries > 600_000 ||
    !Number.isSafeInteger(prepared.inventory?.allocatedBytes) ||
    prepared.inventory.allocatedBytes < 0 ||
    prepared.inventory.allocatedBytes > 12 * 1024 ** 3 ||
    !Number.isFinite(prepared.elapsedSeconds) ||
    prepared.elapsedSeconds < 0 ||
    prepared.elapsedSeconds > 900
  )
    throw new Error("fixed source preparation did not qualify");
}

export function verifyQualificationEvidence(
  attestation,
  proof,
  retainedImage,
  firstWalk = false,
  keyAuthorizationOnly = false,
) {
  if (keyAuthorizationOnly) {
    const checks = attestation?.authorizationChecks;
    const counts = attestation?.finalReceipt?.finalCounts;
    const expectedProbes = [
      "tags-immutable",
      ...Array.from({ length: 4 }, (_, i) => [
        `tag-missing-${i}`,
        `tag-changed-${i}`,
        `tag-removed-${i}`,
      ]).flat(),
    ];
    if (
      !counts ||
      Object.keys(counts).sort().join(",") !== "images,instances,keyPairs,snapshots,volumes" ||
      attestation?.keyAuthorizationOnly !== true ||
      attestation.taggingContract !== qualificationTaggingContract ||
      !attestation.finalized ||
      !Array.isArray(checks) ||
      checks.length !== 13 ||
      checks.some(
        (c, i) =>
          c.probe !== expectedProbes[i] ||
          c.outcome !== "passed" ||
          c.action !== (i > 0 && i % 3 === 0 ? "DeleteTags" : "CreateTags") ||
          c.code !== "UnauthorizedOperation",
      ) ||
      attestation?.operations?.length !== 1 ||
      attestation.operations[0].action !== "ImportKeyPair" ||
      Object.values(attestation?.finalReceipt?.finalCounts ?? {}).some(Number)
    )
      throw new Error("key-only diagnostic or complete settlement is unverified");
    return;
  }
  if (firstWalk) {
    const operations = attestation?.operations ?? [];
    const receipt = attestation?.finalReceipt;
    const walk = proof?.firstWalk;
    const result = proof?.result;
    verifyFirstWalkPreparation(walk?.sourcePreparation);
    const accepted = (action) =>
      operations.filter(
        (op) =>
          op.action === action &&
          op.signerDispatches?.some((dispatch) => dispatch.outcome === "accepted"),
      );
    if (
      attestation?.taggingContract !== qualificationTaggingContract ||
      !attestation?.finalized ||
      !receipt?.completedAt ||
      receipt.failureCodes?.length !== 0 ||
      !receipt.finalCounts ||
      Object.values(receipt.finalCounts).some(Number) ||
      walk?.completed !== true ||
      walk.launchCount !== 2 ||
      walk.exit !== 0 ||
      walk.recipeSha256 !== firstWalkRecipeSha256 ||
      walk.runnerSha256 !==
        digest(fs.readFileSync(path.join(root, "scripts/qualification-first-walk-guest.py"))) ||
      walk.workloadSha256 !==
        digest(fs.readFileSync(path.join(root, "scripts/qualification-first-walk-workload.py"))) ||
      walk.resultSha256 !== digest(JSON.stringify(result)) ||
      result?.completed !== true ||
      result.unmounted !== true ||
      result.ownership?.exit !== 0 ||
      result.verification?.mismatched !== 0 ||
      result.recipeSha256 !== walk.recipeSha256 ||
      !result.ownership.summary?.includes("state=complete") ||
      attestation.firstWalk?.finalized !== true ||
      attestation.firstWalk.phase !== "attached" ||
      result.volumeID !== attestation.firstWalk.volumeID ||
      walk.readinessReceiptSha256 !== attestation.firstWalk.request?.readinessReceiptSha256 ||
      accepted("RunInstances").length !== 2 ||
      accepted("CreateImage").length !== 1 ||
      accepted("CreateVolume").length !== 1 ||
      accepted("AttachVolume").length !== 1
    )
      throw new Error("attestation does not prove one-copy first-walk settlement");
    return;
  }
  const operations = attestation?.operations ?? [];
  const millisecondTimestamp = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/;
  const probeStarted = proof?.spoof?.startedAt ?? "";
  const probeCompleted = proof?.spoof?.completedAt ?? "";
  const probeStartedAt = Date.parse(probeStarted);
  const probeCompletedAt = Date.parse(probeCompleted);
  // The surrounding catalog reads may sign DescribeImages. Only evidence
  // timestamped inside the exact spoof request window invalidates the probe.
  const operationInsideProbeWindow = operations.some((operation) => {
    if (typeof operation.requestedAt !== "string") return true;
    const timestamps = [operation.requestedAt];
    for (const dispatch of operation.signerDispatches ?? []) {
      if (typeof dispatch.beforeAt !== "string") return true;
      timestamps.push(dispatch.beforeAt);
      if (dispatch.afterAt !== undefined) {
        if (typeof dispatch.afterAt !== "string") return true;
        timestamps.push(dispatch.afterAt);
      }
    }
    return timestamps.some((timestamp) => {
      const parsed = Date.parse(timestamp);
      return !Number.isFinite(parsed) || (parsed >= probeStartedAt && parsed <= probeCompletedAt);
    });
  });
  if (
    proof?.spoof?.status !== 403 ||
    proof?.spoof?.catalogUnchanged !== true ||
    !millisecondTimestamp.test(probeStarted) ||
    !millisecondTimestamp.test(probeCompleted) ||
    !Number.isFinite(probeStartedAt) ||
    !Number.isFinite(probeCompletedAt) ||
    probeStartedAt > probeCompletedAt ||
    operationInsideProbeWindow
  ) {
    throw new Error(
      "evidence does not prove the spoofed admin probe left signer sequence unchanged",
    );
  }
  const fsr = operations.find((operation) => operation.action.includes("FastSnapshotRestore"));
  if (!fsr || fsr.denialReason !== "policy-denied" || (fsr.signerDispatches ?? []).length !== 0) {
    throw new Error("attestation does not prove pre-signer Fast Snapshot Restore denial");
  }
  const accepted = (action) =>
    operations
      .map((operation, index) => ({ operation, index }))
      .filter(
        ({ operation }) =>
          operation.action === action &&
          operation.signerDispatches?.some((dispatch) => dispatch.outcome === "accepted"),
      );
  const launches = accepted("RunInstances");
  const images = accepted("CreateImage");
  const launchDispatches = operations
    .filter((operation) => operation.action === "RunInstances")
    .flatMap((operation) => operation.signerDispatches ?? []);
  if (retainedImage) {
    if (
      launches.length !== 1 ||
      launchDispatches.length !== 1 ||
      operations.some(
        (operation) => operation.action === "CreateImage" && operation.signerDispatches?.length,
      ) ||
      proof?.execution?.normalSelection !== true ||
      proof?.execution?.exactPromotedRevision !== true ||
      proof?.execution?.selectedImageDigest !== digest(retainedImage.imageId) ||
      proof?.execution?.selectedRevisionDigest !== proof?.catalog?.failedRevisionDigest ||
      !attestation.finalReceipt?.verification?.some(
        (entry) => entry.action === "RetainedImagePreserved" && entry.outcome === "accepted",
      ) ||
      attestation.finalReceipt?.cleanupAttempts?.some(
        (entry) => entry.action === "DeregisterImage" || entry.action === "DeleteSnapshot",
      )
    ) {
      throw new Error(
        "attestation does not prove one retained launch and borrowed-image preservation",
      );
    }
  } else if (
    launches.length !== 3 ||
    images.length !== 1 ||
    !(
      launches[0].index < images[0].index &&
      images[0].index < launches[1].index &&
      launches[1].index < launches[2].index
    )
  ) {
    throw new Error("attestation does not prove source/image/candidate/promoted launch order");
  }
  const receipt = attestation.finalReceipt;
  if (
    !attestation.finalized ||
    !receipt ||
    receipt.failureCodes?.length !== 0 ||
    (retainedImage
      ? (receipt.resourcesAtStart?.images ?? 0) !== 0 ||
        (receipt.resourcesAtStart?.snapshots ?? 0) !== 0
      : (receipt.resourcesAtStart?.images ?? 0) < 1) ||
    proof?.fsr?.rejected !== true ||
    proof?.catalog?.seededDefaultReadback !== true ||
    proof?.catalog?.priorDefaultImageRestored !== true ||
    proof?.catalog?.priorDefaultRevisionRestored !== true ||
    proof?.catalog?.failedCatalogRevisionRetired !== true ||
    proof?.catalog?.staleCASRejected !== true ||
    proof?.catalog?.staleReadbackUnchanged !== true ||
    !sha64.test(proof?.catalog?.priorImageDigest ?? "") ||
    !sha64.test(proof?.catalog?.priorRevisionDigest ?? "") ||
    !sha64.test(proof?.catalog?.failedImageDigest ?? "") ||
    !sha64.test(proof?.catalog?.failedRevisionDigest ?? "") ||
    !sha64.test(proof?.catalog?.restoredRevisionDigest ?? "") ||
    proof?.execution?.mintExit !== 86 ||
    proof?.execution?.injectedAfterPromotedSmoke !== true ||
    proof?.execution?.launchCount !== (retainedImage ? 1 : 3) ||
    proof?.execution?.smokeCount < (retainedImage ? 1 : 3) ||
    proof?.hardKill?.executorKilled !== true ||
    proof?.hardKill?.cloudCredentialsPresent !== false
  ) {
    throw new Error("attestation does not prove owned image recovery after executor exit");
  }
}

function sanitizeEvidence(value) {
  if (Array.isArray(value)) return value.slice(0, 256).map(sanitizeEvidence);
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value)
        .filter(
          ([key]) =>
            !/(token|secret|url|account|resource.?id|instance.?id|image.?id|snapshot.?id|key.?id|ip|user.?data)/i.test(
              key,
            ),
        )
        .slice(0, 256)
        .map(([key, child]) => [key, sanitizeEvidence(child)]),
    );
  }
  return typeof value === "string" ? redactText(value).slice(0, 512) : value;
}

function redactText(value) {
  if (executorTokenForRedaction)
    value = value.replaceAll(executorTokenForRedaction, "[executor-token]");
  return value
    .replace(/https?:\/\/[^\s"<>]+/g, "[url]")
    .replace(/\b(?:ami|i|vol|snap|key)-[0-9a-f]{8,}\b/gi, "[aws-resource]")
    .replace(/\b(?:\d{1,3}\.){3}\d{1,3}\b/g, "[ip]")
    .replace(/\b\d{12}\b/g, "[aws-account]");
}

function writeEvidence(result) {
  const proofDir = path.resolve(required("QUALIFICATION_PROOF_DIR"));
  writeJSON(path.join(proofDir, "finalization.json"), sanitizeEvidence(result));
  const files = filesUnder(proofDir).filter((file) => path.basename(file) !== "checksums.sha256");
  const checksums = files
    .map(
      (file) =>
        `${digest(fs.readFileSync(file))}  ${path.relative(proofDir, file).split(path.sep).join("/")}`,
    )
    .join("\n");
  fs.writeFileSync(path.join(proofDir, "checksums.sha256"), `${checksums}\n`, { mode: 0o600 });
}

async function main() {
  const [command, ...args] = process.argv.slice(2);
  if (["binding-probe-plan", "binding-probe-run", "binding-probe-recover"].includes(command)) {
    const { bindingProbeCommand } = await import("./image-qualification-binding-probe.mjs");
    return await bindingProbeCommand(command, args, Cloudflare);
  }
  if (["idle-controller-run", "idle-controller-recover"].includes(command)) {
    const { idleControllerCommand } = await import("./image-qualification-idle-controller.mjs");
    return await idleControllerCommand(command, args, Cloudflare);
  }
  if (command === "retained-capsule") {
    createRetainedCapsule(path.resolve(args[0]), path.resolve(args[1]), path.resolve(args[2]));
    return;
  }
  if (command === "retained-selection") {
    writeJSON(
      args[2],
      verifyRetainedSelection(
        readJSON(args[0]).lease,
        readJSON(args[1]),
        required("QUALIFICATION_AWS_REGION"),
      ),
    );
    return;
  }
  if (command === "authorize") {
    const result = await verifyCandidateIdentity();
    appendOutput("candidate_sha", result.candidateSha);
    return;
  }
  if (command === "admit") {
    const result = await verifyCandidateIdentity({ artifact: true });
    const artifactDir = path.resolve(required("QUALIFICATION_ARTIFACT_DIR"));
    const manifest = verifyManifest(artifactDir, result.candidateSha, result.workflowSha);
    verifyPublisherContract(
      fs.readFileSync(
        path.join(artifactDir, "candidate/scripts/mint-aws-devtools-image.sh"),
        "utf8",
      ),
    );
    publishHandoff(result, manifest);
    return;
  }
  if (command === "manifest") {
    createManifest(
      path.resolve(args[0]),
      path.resolve(args[1]),
      required("QUALIFICATION_CANDIDATE_SHA", sha40),
      required("QUALIFICATION_WORKFLOW_SHA", sha40),
    );
    return;
  }
  if (command === "first-walk-recipe") {
    if (args.length !== 2 || !privateQualificationMode(process.env.QUALIFICATION_MODE)) {
      throw new Error("first-walk-recipe requires first-walk mode and two arguments");
    }
    stageFirstWalkRecipe(path.resolve(args[0]), path.resolve(args[1]));
    return;
  }
  if (command === "prepare-build") {
    if (args.length !== (privateQualificationMode(process.env.QUALIFICATION_MODE) ? 4 : 3))
      throw new Error("prepare-build requires source checkouts and receipt");
    prepareCandidateBuild(
      path.resolve(args[0]),
      path.resolve(args[1]),
      path.resolve(args[2]),
      required("QUALIFICATION_CANDIDATE_SHA", sha40),
      required("QUALIFICATION_WORKFLOW_SHA", sha40),
      args[3],
    );
    return;
  }
  if (command === "verify-catalog") {
    if (args.length !== 13) throw new Error("verify-catalog requires thirteen arguments");
    const result = verifyCatalogRollbackEvidence({
      seed: readJSON(path.resolve(args[0])),
      seededReadback: readJSON(path.resolve(args[1])),
      promotion: readJSON(path.resolve(args[2])),
      rollback: readJSON(path.resolve(args[3])),
      restoredReadback: readJSON(path.resolve(args[4])),
      failedReadback: readJSON(path.resolve(args[5])),
      failedStatus: args[6],
      staleResponse: readJSON(path.resolve(args[7])),
      staleStatus: args[8],
      staleReadback: readJSON(path.resolve(args[9])),
      staleFailedReadback: readJSON(path.resolve(args[10])),
      staleFailedStatus: args[11],
    });
    writeJSON(path.resolve(args[12]), result);
    return;
  }
  if (command === "deploy") return await deploy();
  if (command === "arm") return await arm();
  if (command === "finalize") {
    let identityFailure;
    try {
      await verifyCandidateIdentity({ cleanup: true });
    } catch (error) {
      identityFailure = error instanceof Error ? error.message : String(error);
    }
    let expected;
    try {
      expected = qualificationExpectedFromEnv({ optional: true });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      identityFailure = identityFailure ? `${identityFailure}; ${message}` : message;
    }
    const result = await cleanupRun({
      reaper: true,
      requireProof: true,
      expected,
      initialQualificationFailure: identityFailure,
    });
    writeEvidence(result);
    if (result.result === "failed") throw new Error(result.failure);
    return;
  }
  if (command === "reap") {
    const result = await cleanupRun({ reaper: true });
    writeEvidence(result);
    if (result.result === "failed") throw new Error(result.failure);
    return;
  }
  throw new Error(
    "usage: image-qualification-control.mjs authorize|admit|manifest|verify-catalog|deploy|arm|finalize|reap",
  );
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(redactText(error instanceof Error ? error.message : String(error)));
    process.exitCode = 1;
  });
}
