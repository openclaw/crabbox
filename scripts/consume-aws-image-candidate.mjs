#!/usr/bin/env node

import { createHash } from "node:crypto";
import { execFile } from "node:child_process";
import { link, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import {
  canonicalJSON,
  digestBytes,
  digestJSON,
  digestJSONLine,
} from "./aws-image-candidate.mjs";

const scriptPath = fileURLToPath(import.meta.url);
const repoRoot = path.resolve(path.dirname(scriptPath), "..");
const execFileAsync = promisify(execFile);
const qualificationSchema = "crabbox-aws-image-qualification-input/v1";
const candidateSchema = "crabbox-aws-image-candidate/v1";
const bundleSchema = "crabbox-aws-image-evidence-bundle/v1";
const recipeSchema = "crabbox-aws-image-recipe/v1";
const scrubSchema = "crabbox-aws-image-scrub/v1";
const artifactType = "application/vnd.crabbox.aws-image-evidence.v1";
const manifestMediaType = "application/vnd.oci.image.manifest.v1+json";
const emptyConfigMediaType = "application/vnd.oci.empty.v1+json";
const emptyConfigDigest = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a";
const candidateMediaType = "application/vnd.crabbox.aws-image-candidate.v1+json";
const signatureArtifactType = "application/vnd.dev.sigstore.bundle.v0.3+json";
const expectedFiles = new Map([
  ["bundle.json", "application/vnd.crabbox.aws-image-evidence-manifest.v1+json"],
  ["candidate.json", candidateMediaType],
  ["provenance.intoto.jsonl", "application/vnd.in-toto+json"],
  ["recipe.json", "application/json"],
  ["sbom.spdx.json", "application/spdx+json"],
  ["scrub-report.json", "application/json"],
]);

function fail(message) {
  throw new Error(message);
}

function parseOptions(argv) {
  const options = {};
  for (let index = 0; index < argv.length; index += 1) {
    const name = argv[index];
    if (!name.startsWith("--") || index + 1 >= argv.length) {
      fail(`invalid argument ${name}`);
    }
    if (options[name.slice(2)] !== undefined) fail(`duplicate argument ${name}`);
    options[name.slice(2)] = argv[index + 1];
    index += 1;
  }
  return options;
}

function required(options, name) {
  if (!options[name]) fail(`--${name} is required`);
  return options[name];
}

function assertExactKeys(value, keys, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    fail(`${label} must be an object`);
  }
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    fail(`${label} must contain exactly: ${expected.join(", ")}`);
  }
}

function assertPattern(value, pattern, label) {
  if (!pattern.test(value ?? "")) fail(`invalid ${label}`);
}

function assertDigest(value, label) {
  assertPattern(value, /^sha256:[0-9a-f]{64}$/, label);
}

function assertIntegerString(value, label) {
  assertPattern(value, /^[1-9][0-9]*$/, label);
}

async function readJSON(file, label) {
  try {
    return JSON.parse(await readFile(file, "utf8"));
  } catch (error) {
    fail(`${label} is not valid JSON: ${error.message}`);
  }
}

async function readJSONLines(file, label) {
  const text = await readFile(file, "utf8");
  try {
    return JSON.parse(text);
  } catch {
    // JSONL evidence emits one complete object per line.
  }
  const lines = text
    .split("\n")
    .map((line) => line.trim())
    .filter(Boolean);
  if (lines.length !== 1) fail(`${label} must contain exactly one record`);
  try {
    return JSON.parse(lines[0]);
  } catch (error) {
    fail(`${label} is not valid JSON: ${error.message}`);
  }
}

function assertRecipePath(value, label) {
  assertPattern(
    value,
    /^recipes\/(?:[A-Za-z0-9][A-Za-z0-9._-]*\/)*[A-Za-z0-9][A-Za-z0-9._-]*[.]json$/,
    label,
  );
  return value;
}

async function gitBytes(args, label) {
  try {
    const { stdout } = await execFileAsync(
      "git",
      ["--no-replace-objects", "-C", repoRoot, ...args],
      {
        encoding: null,
        maxBuffer: 1024 * 1024,
      },
    );
    return stdout;
  } catch (error) {
    const detail = Buffer.isBuffer(error.stderr)
      ? error.stderr.toString("utf8").trim()
      : String(error.stderr ?? "").trim();
    fail(`${label}: ${detail || error.message}`);
  }
}

async function readRecipeBlob(commit, recipePath, label) {
  const objectType = (await gitBytes(["cat-file", "-t", commit], "source commit is unavailable"))
    .toString("utf8")
    .trim();
  if (objectType !== "commit") fail("source commit must identify a commit object");

  const treeEntry = await gitBytes(
    ["ls-tree", "-z", "--full-tree", commit, "--", recipePath],
    `${label} is unavailable at source commit`,
  );
  const records = treeEntry.toString("utf8").split("\0").filter(Boolean);
  if (records.length !== 1) fail(`${label} must identify exactly one source-commit blob`);
  const match = records[0].match(/^([0-9]{6}) ([a-z]+) ([0-9a-f]{40})\t(.+)$/);
  if (
    !match ||
    !["100644", "100755"].includes(match[1]) ||
    match[2] !== "blob" ||
    match[4] !== recipePath
  ) {
    fail(`${label} must be a regular source-commit blob`);
  }
  const bytes = await gitBytes(["cat-file", "blob", match[3]], `${label} blob is unavailable`);
  try {
    return JSON.parse(bytes);
  } catch (error) {
    fail(`${label} source-commit blob is not valid JSON: ${error.message}`);
  }
}

async function validateExpected(options) {
  const expected = {
    repository: required(options, "repository"),
    sourceRepository: required(options, "source-repository"),
    sourceCommit: required(options, "source-commit"),
    workflowRef: required(options, "workflow-ref"),
    workflowRunId: required(options, "workflow-run-id"),
    workflowRunAttempt: required(options, "workflow-run-attempt"),
    target: required(options, "target"),
    region: required(options, "region"),
    instanceType: required(options, "instance-type"),
    architecture: required(options, "architecture"),
    baseImage: required(options, "base-image"),
    amiId: required(options, "ami-id"),
    profile: required(options, "profile"),
    imageRecipe: required(options, "image-recipe"),
    readinessRecipe: required(options, "readiness-recipe"),
  };
  assertPattern(
    expected.repository,
    /^ghcr\.io\/[a-z0-9_.-]+(?:\/[a-z0-9_.-]+)+$/,
    "expected GHCR repository",
  );
  assertPattern(
    expected.sourceRepository,
    /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/,
    "source repository",
  );
  assertPattern(expected.sourceCommit, /^[0-9a-f]{40}$/, "source commit");
  assertPattern(
    expected.workflowRef,
    new RegExp(
      `^${expected.sourceRepository.replaceAll(/[.*+?^${}()|[\]\\]/g, "\\$&")}/\\.github/workflows/[A-Za-z0-9._/-]+\\.ya?ml@refs/heads/[A-Za-z0-9._/-]+$`,
    ),
    "workflow ref",
  );
  assertIntegerString(expected.workflowRunId, "workflow run id");
  assertIntegerString(expected.workflowRunAttempt, "workflow run attempt");
  if (!["linux", "windows"].includes(expected.target)) fail("invalid target");
  assertPattern(expected.region, /^[a-z]{2}(?:-gov)?-[a-z]+-[0-9]+$/, "AWS region");
  assertPattern(expected.instanceType, /^[A-Za-z0-9.-]+$/, "instance type");
  assertPattern(expected.architecture, /^(?:x86_64|arm64)$/, "architecture");
  assertPattern(expected.baseImage, /^ami-[0-9a-z]+$/, "base AMI");
  assertPattern(expected.amiId, /^ami-[0-9a-z]+$/, "candidate AMI");
  assertPattern(expected.profile, /^[a-z0-9][a-z0-9-]*$/, "profile");
  assertRecipePath(expected.imageRecipe, "image recipe");
  assertRecipePath(expected.readinessRecipe, "readiness recipe");
  const imageRecipe = await readRecipeBlob(
    expected.sourceCommit,
    expected.imageRecipe,
    "expected image recipe",
  );
  const readinessRecipe = await readRecipeBlob(
    expected.sourceCommit,
    expected.readinessRecipe,
    "expected readiness recipe",
  );
  expected.imageRecipeDigest = digestJSONLine(imageRecipe);
  expected.readinessRecipeDigest = digestJSON(readinessRecipe);
  return expected;
}

async function validateManifest(options, candidateRef, candidateDigest) {
  const metadata = await readJSON(required(options, "manifest"), "ORAS manifest metadata");
  const raw = await readFile(required(options, "manifest-raw"));
  if (digestBytes(raw) !== candidateDigest) {
    fail("raw OCI manifest digest does not match candidate reference");
  }
  const manifest = JSON.parse(raw);
  assertExactKeys(
    metadata,
    ["reference", "mediaType", "digest", "size", "content"],
    "ORAS manifest metadata",
  );
  if (
    metadata.reference !== candidateRef ||
    metadata.digest !== candidateDigest ||
    metadata.mediaType !== manifestMediaType ||
    metadata.size !== raw.length ||
    canonicalJSON(metadata.content) !== canonicalJSON(manifest)
  ) {
    fail("ORAS manifest metadata does not match candidate reference");
  }
  const manifestKeys = ["schemaVersion", "mediaType", "artifactType", "config", "layers"];
  if (manifest.annotations !== undefined) manifestKeys.push("annotations");
  assertExactKeys(manifest, manifestKeys, "OCI manifest");
  if (
    manifest.schemaVersion !== 2 ||
    manifest.mediaType !== manifestMediaType ||
    manifest.artifactType !== artifactType
  ) {
    fail("invalid OCI evidence artifact manifest");
  }
  if (manifest.annotations !== undefined) {
    assertExactKeys(
      manifest.annotations,
      ["org.opencontainers.image.created"],
      "OCI manifest annotations",
    );
    if (Number.isNaN(Date.parse(manifest.annotations["org.opencontainers.image.created"]))) {
      fail("invalid OCI manifest creation annotation");
    }
  }
  assertExactKeys(manifest.config, ["mediaType", "digest", "size", "data"], "OCI config");
  if (
    manifest.config.mediaType !== emptyConfigMediaType ||
    manifest.config.digest !== emptyConfigDigest ||
    manifest.config.size !== 2 ||
    manifest.config.data !== "e30="
  ) {
    fail("OCI evidence artifact must use the canonical empty config");
  }
  if (!Array.isArray(manifest.layers) || manifest.layers.length !== expectedFiles.size) {
    fail(`OCI manifest must contain exactly ${expectedFiles.size} evidence layers`);
  }
  const descriptors = new Map();
  for (const descriptor of manifest.layers) {
    assertExactKeys(
      descriptor,
      ["mediaType", "digest", "size", "annotations"],
      "OCI layer descriptor",
    );
    assertExactKeys(
      descriptor.annotations,
      ["org.opencontainers.image.title"],
      "OCI layer annotations",
    );
    const name = descriptor.annotations["org.opencontainers.image.title"];
    if (!expectedFiles.has(name) || descriptors.has(name)) {
      fail("OCI manifest contains missing, extra, or duplicate evidence layers");
    }
    if (
      descriptor.mediaType !== expectedFiles.get(name) ||
      !Number.isSafeInteger(descriptor.size) ||
      descriptor.size <= 0
    ) {
      fail(`OCI layer descriptor mismatch for ${name}`);
    }
    assertDigest(descriptor.digest, `${name} layer digest`);
    descriptors.set(name, descriptor);
  }
  return { metadata, descriptors };
}

async function validateReferrers(options, candidateRef, candidateDigest, manifest) {
  const value = await readJSON(required(options, "referrers"), "ORAS referrers");
  if (
    value.reference !== candidateRef ||
    value.mediaType !== manifest.metadata.mediaType ||
    value.digest !== candidateDigest ||
    value.size !== manifest.metadata.size
  ) {
    fail("ORAS referrer subject does not match candidate artifact");
  }
  if (!Array.isArray(value.referrers) || value.referrers.length !== 1) {
    fail("candidate artifact must have exactly one signature referrer");
  }
  const signature = value.referrers[0];
  const allowedKeys = ["reference", "mediaType", "digest", "size", "artifactType"];
  if (signature.annotations !== undefined) allowedKeys.push("annotations");
  if (signature.referrers !== undefined) allowedKeys.push("referrers");
  assertExactKeys(signature, allowedKeys, "signature referrer");
  if (
    signature.referrers !== undefined &&
    (!Array.isArray(signature.referrers) || signature.referrers.length !== 0)
  ) {
    fail("signature referrer depth boundary must be empty");
  }
  if (
    signature.mediaType !== manifestMediaType ||
    signature.artifactType !== signatureArtifactType ||
    !Number.isSafeInteger(signature.size) ||
    signature.size <= 0
  ) {
    fail("invalid signature referrer descriptor");
  }
  assertDigest(signature.digest, "signature referrer digest");
  const repository = candidateRef.slice(0, candidateRef.indexOf("@"));
  if (signature.reference !== `${repository}@${signature.digest}`) {
    fail("signature referrer repository or digest mismatch");
  }
  if (
    signature.annotations?.["dev.sigstore.bundle.predicateType"] !==
    "https://sigstore.dev/cosign/sign/v1"
  ) {
    fail("Sigstore bundle referrer is not a Cosign signature");
  }
  return signature;
}

async function validateSignatureManifest(
  options,
  candidateRef,
  candidateDigest,
  candidateManifest,
  signature,
) {
  const metadata = await readJSON(
    required(options, "signature-manifest"),
    "signature manifest metadata",
  );
  const raw = await readFile(required(options, "signature-manifest-raw"));
  if (digestBytes(raw) !== signature.digest) {
    fail("raw signature manifest digest does not match discovered referrer");
  }
  const manifest = JSON.parse(raw);
  assertExactKeys(
    metadata,
    ["reference", "mediaType", "digest", "size", "content"],
    "signature manifest metadata",
  );
  if (
    metadata.reference !== signature.reference ||
    metadata.digest !== signature.digest ||
    metadata.mediaType !== manifestMediaType ||
    metadata.size !== raw.length ||
    canonicalJSON(metadata.content) !== canonicalJSON(manifest)
  ) {
    fail("signature manifest metadata does not match discovered referrer");
  }
  assertExactKeys(
    manifest,
    ["schemaVersion", "mediaType", "artifactType", "config", "layers", "subject", "annotations"],
    "signature manifest",
  );
  if (
    manifest.schemaVersion !== 2 ||
    manifest.mediaType !== manifestMediaType ||
    manifest.artifactType !== signatureArtifactType
  ) {
    fail("invalid signature artifact manifest");
  }
  assertExactKeys(
    manifest.annotations,
    [
      "org.opencontainers.image.created",
      "dev.sigstore.bundle.content",
      "dev.sigstore.bundle.predicateType",
    ],
    "signature manifest annotations",
  );
  if (
    Number.isNaN(Date.parse(manifest.annotations["org.opencontainers.image.created"])) ||
    manifest.annotations["dev.sigstore.bundle.content"] !== "dsse-envelope" ||
    manifest.annotations["dev.sigstore.bundle.predicateType"] !==
      "https://sigstore.dev/cosign/sign/v1"
  ) {
    fail("invalid signature manifest annotations");
  }
  assertExactKeys(
    manifest.config,
    ["mediaType", "artifactType", "digest", "size"],
    "signature config",
  );
  if (
    manifest.config.mediaType !== emptyConfigMediaType ||
    manifest.config.artifactType !== signatureArtifactType ||
    manifest.config.digest !== emptyConfigDigest ||
    manifest.config.size !== 2
  ) {
    fail("signature artifact must use the canonical empty image config");
  }
  assertExactKeys(manifest.subject, ["mediaType", "digest", "size"], "signature subject");
  if (
    manifest.subject.mediaType !== candidateManifest.metadata.mediaType ||
    manifest.subject.digest !== candidateDigest ||
    manifest.subject.size !== candidateManifest.metadata.size ||
    candidateRef.slice(0, candidateRef.indexOf("@")) !==
      signature.reference.slice(0, signature.reference.indexOf("@"))
  ) {
    fail("signature manifest subject does not match candidate artifact");
  }
  if (!Array.isArray(manifest.layers) || manifest.layers.length !== 1) {
    fail("signature artifact must contain exactly one bundle layer");
  }
  const layer = manifest.layers[0];
  assertExactKeys(layer, ["mediaType", "digest", "size"], "signature bundle layer");
  if (
    layer.mediaType !== signatureArtifactType ||
    !Number.isSafeInteger(layer.size) ||
    layer.size <= 0
  ) {
    fail("invalid signature bundle layer descriptor");
  }
  assertDigest(layer.digest, "signature bundle layer digest");
  return layer;
}

async function validateSignatureBundle(options, layer) {
  const bytes = await readFile(required(options, "signature-bundle"));
  if (bytes.length !== layer.size || digestBytes(bytes) !== layer.digest) {
    fail("signature bundle does not match its immutable layer descriptor");
  }
  try {
    const bundle = JSON.parse(bytes);
    if (!bundle || typeof bundle !== "object" || Array.isArray(bundle)) {
      fail("signature bundle must be a JSON object");
    }
  } catch (error) {
    fail(`signature bundle is not valid JSON: ${error.message}`);
  }
}

async function inspectSignature(options, includeManifest, includeBundle) {
  const candidateRef = required(options, "candidate-ref");
  const referenceMatch = candidateRef.match(
    /^(ghcr\.io\/[a-z0-9._-]+(?:\/[a-z0-9._-]+)+)@(sha256:[0-9a-f]{64})$/,
  );
  if (!referenceMatch) fail("candidate reference must be an immutable GHCR digest");
  const [, repository, candidateDigest] = referenceMatch;
  const manifest = await validateManifest(options, candidateRef, candidateDigest);
  const signature = await validateReferrers(options, candidateRef, candidateDigest, manifest);
  if (!includeManifest) return { repository, candidateDigest, manifest, signature };
  const layer = await validateSignatureManifest(
    options,
    candidateRef,
    candidateDigest,
    manifest,
    signature,
  );
  if (includeBundle) await validateSignatureBundle(options, layer);
  return { repository, candidateDigest, manifest, signature, layer };
}

function validateCandidate(candidate, expected) {
  assertExactKeys(
    candidate,
    ["schema", "createdAt", "source", "readiness", "target", "image", "previousDefault", "checks"],
    "candidate record",
  );
  if (candidate.schema !== candidateSchema || Number.isNaN(Date.parse(candidate.createdAt))) {
    fail("invalid CandidateRecordV1 header");
  }
  if (
    candidate.source?.repository !== expected.sourceRepository ||
    candidate.source?.commit !== expected.sourceCommit ||
    candidate.source?.workflow?.ref !== expected.workflowRef ||
    candidate.source?.workflow?.runId !== expected.workflowRunId ||
    candidate.source?.workflow?.runAttempt !== expected.workflowRunAttempt
  ) {
    fail("candidate source or workflow identity mismatch");
  }
  if (
    candidate.target?.platform !== expected.target ||
    candidate.target?.region !== expected.region ||
    candidate.target?.instanceType !== expected.instanceType ||
    candidate.target?.architecture !== expected.architecture ||
    candidate.target?.baseImage !== expected.baseImage
  ) {
    fail("candidate target identity mismatch");
  }
  if (candidate.image?.amiId !== expected.amiId) {
    fail("candidate AMI identity mismatch");
  }
  if (
    !Array.isArray(candidate.image.snapshotIds) ||
    candidate.image.snapshotIds.length === 0 ||
    new Set(candidate.image.snapshotIds).size !== candidate.image.snapshotIds.length
  ) {
    fail("candidate snapshot identity is incomplete");
  }
  if (
    candidate.readiness?.profile !== expected.profile ||
    candidate.readiness?.recipeDigest !== expected.readinessRecipeDigest
  ) {
    fail("candidate readiness identity mismatch");
  }
}

function validateRecipe(recipe, expected, candidate) {
  assertExactKeys(
    recipe,
    ["schema", "path", "target", "profile", "readinessRecipe", "inputs", "components"],
    "image recipe",
  );
  if (
    recipe.schema !== recipeSchema ||
    recipe.path !== expected.imageRecipe ||
    recipe.target !== expected.target ||
    recipe.profile !== expected.profile ||
    recipe.readinessRecipe !== expected.readinessRecipe ||
    recipe.profile !== candidate.readiness.profile
  ) {
    fail("image recipe identity mismatch");
  }
  if (
    !Array.isArray(recipe.inputs) ||
    recipe.inputs.length === 0 ||
    new Set(recipe.inputs).size !== recipe.inputs.length ||
    !recipe.inputs.includes(recipe.readinessRecipe)
  ) {
    fail("image recipe inputs must be unique and include the readiness recipe");
  }
  if (
    !Array.isArray(recipe.components) ||
    recipe.components.length === 0 ||
    new Set(recipe.components.map((item) => canonicalJSON(item))).size !== recipe.components.length
  ) {
    fail("image recipe components must be unique and non-empty");
  }
}

function validateProvenance(value, candidate, recipe) {
  assertExactKeys(value, ["_type", "subject", "predicateType", "predicate"], "provenance");
  if (
    value._type !== "https://in-toto.io/Statement/v1" ||
    value.predicateType !== "https://slsa.dev/provenance/v1"
  ) {
    fail("invalid provenance statement");
  }
  if (
    !Array.isArray(value.subject) ||
    value.subject.length !== 1 ||
    value.subject[0]?.name !== "candidate.json" ||
    value.subject[0]?.digest?.sha256 !== digestBytes(`${canonicalJSON(candidate)}\n`).slice(7)
  ) {
    fail("provenance subject does not bind candidate.json");
  }
  const build = value.predicate?.buildDefinition;
  const run = value.predicate?.runDetails;
  if (
    build?.buildType !== "urn:crabbox:aws-image-candidate:v1" ||
    canonicalJSON(build?.externalParameters) !==
      canonicalJSON({ readiness: candidate.readiness, target: candidate.target }) ||
    canonicalJSON(build?.internalParameters) !== "{}"
  ) {
    fail("provenance build definition mismatch");
  }
  if (
    run?.builder?.id !==
      `https://github.com/${candidate.source.repository}/actions/runs/${candidate.source.workflow.runId}` ||
    run?.metadata?.invocationId !==
      `${candidate.source.workflow.runId}/${candidate.source.workflow.runAttempt}` ||
    run?.metadata?.startedOn !== candidate.createdAt ||
    run?.metadata?.finishedOn !== candidate.createdAt
  ) {
    fail("provenance workflow run mismatch");
  }
  const dependencies = build?.resolvedDependencies;
  if (!Array.isArray(dependencies) || dependencies.length !== recipe.inputs.length) {
    fail("provenance dependencies do not match image recipe inputs");
  }
  const expectedURIs = new Set(
    recipe.inputs.map(
      (input) =>
        `git+https://github.com/${candidate.source.repository}@${candidate.source.commit}#path=${input}`,
    ),
  );
  const actualURIs = new Set();
  for (const dependency of dependencies) {
    assertExactKeys(dependency, ["uri", "digest"], "provenance dependency");
    assertExactKeys(dependency.digest, ["sha256"], "provenance dependency digest");
    assertPattern(dependency.digest.sha256, /^[0-9a-f]{64}$/, "dependency digest");
    if (!expectedURIs.has(dependency.uri) || actualURIs.has(dependency.uri)) {
      fail("provenance contains missing, extra, or duplicate recipe dependencies");
    }
    actualURIs.add(dependency.uri);
  }
}

function validateSBOM(value, candidate, recipe) {
  assertExactKeys(
    value,
    [
      "spdxVersion",
      "dataLicense",
      "SPDXID",
      "name",
      "documentNamespace",
      "creationInfo",
      "packages",
    ],
    "SBOM",
  );
  assertExactKeys(value.creationInfo, ["created", "creators"], "SBOM creation info");
  const expectedNamespace =
    `https://github.com/${candidate.source.repository}/aws-image-candidates/` +
    `${candidate.source.commit}/${candidate.image.amiId}`;
  if (
    value.spdxVersion !== "SPDX-2.3" ||
    value.dataLicense !== "CC0-1.0" ||
    value.SPDXID !== "SPDXRef-DOCUMENT" ||
    value.name !== `crabbox-${candidate.target.platform}-${candidate.image.amiId}` ||
    value.documentNamespace !== expectedNamespace ||
    value.creationInfo?.created !== candidate.createdAt ||
    !Array.isArray(value.creationInfo?.creators) ||
    value.creationInfo.creators.length !== 1 ||
    value.creationInfo.creators[0] !== "Tool: scripts/aws-image-candidate.mjs"
  ) {
    fail("SBOM identity does not match candidate");
  }
  if (!Array.isArray(value.packages) || value.packages.length !== recipe.components.length) {
    fail("SBOM packages do not match recipe components");
  }
  for (const [index, component] of recipe.components.entries()) {
    const item = value.packages[index];
    assertExactKeys(
      item,
      [
        "SPDXID",
        "name",
        "versionInfo",
        "downloadLocation",
        "filesAnalyzed",
        "licenseConcluded",
        "licenseDeclared",
        "supplier",
      ],
      `SBOM package ${index}`,
    );
    if (
      item?.name !== component.name ||
      item?.versionInfo !== component.version ||
      item?.supplier !== `Organization: ${component.source}` ||
      item?.filesAnalyzed !== false ||
      item?.downloadLocation !== "NOASSERTION" ||
      item?.licenseConcluded !== "NOASSERTION" ||
      item?.licenseDeclared !== "NOASSERTION"
    ) {
      fail("SBOM package does not match recipe component");
    }
  }
}

function validateScrub(value, candidate) {
  assertExactKeys(
    value,
    ["schema", "target", "removed", "findings", "evidenceDigest"],
    "scrub report",
  );
  if (
    value.schema !== scrubSchema ||
    value.target !== candidate.target.platform ||
    !Array.isArray(value.findings) ||
    value.findings.length !== 0
  ) {
    fail("scrub evidence is not qualification-safe");
  }
  const evidence = {
    schema: value.schema,
    target: value.target,
    removed: value.removed,
    findings: value.findings,
  };
  if (digestJSON(evidence) !== value.evidenceDigest) {
    fail("scrub evidence digest mismatch");
  }
  const scrubCheck = candidate.checks.find((item) => item.name === "scrub");
  if (scrubCheck?.evidenceDigest !== value.evidenceDigest) {
    fail("candidate scrub check does not bind scrub evidence");
  }
}

async function validateBundle(options, expected, manifest) {
  const directory = path.resolve(required(options, "bundle"));
  const bundleVerification = await readJSON(
    required(options, "bundle-verification"),
    "candidate bundle verification",
  );
  assertExactKeys(
    bundleVerification,
    ["schema", "candidateDigest", "immutableTag", "ociRepository", "certificateIdentity", "files"],
    "candidate bundle verification",
  );
  const values = {};
  for (const [name, mediaType] of expectedFiles) {
    const bytes = await readFile(path.join(directory, name));
    const descriptor = manifest.descriptors.get(name);
    if (descriptor.digest !== digestBytes(bytes) || descriptor.size !== bytes.length) {
      fail(`downloaded evidence does not match OCI descriptor for ${name}`);
    }
    values[name] = {
      mediaType,
      digest: descriptor.digest,
      size: descriptor.size,
    };
  }
  const bundle = await readJSON(path.join(directory, "bundle.json"), "bundle manifest");
  const candidate = await readJSON(path.join(directory, "candidate.json"), "candidate record");
  const recipe = await readJSON(path.join(directory, "recipe.json"), "image recipe");
  const scrub = await readJSON(path.join(directory, "scrub-report.json"), "scrub report");
  const sbom = await readJSON(path.join(directory, "sbom.spdx.json"), "SBOM");
  const provenance = await readJSONLines(
    path.join(directory, "provenance.intoto.jsonl"),
    "provenance",
  );
  if (bundle.schema !== bundleSchema || bundle.artifactType !== artifactType) {
    fail("invalid evidence bundle schema");
  }
  if (
    bundleVerification.schema !== "crabbox-aws-image-candidate-bundle/v1" ||
    bundle.oci?.repository !== expected.repository ||
    bundleVerification.ociRepository !== expected.repository ||
    bundleVerification.candidateDigest !== values["candidate.json"].digest ||
    bundleVerification.immutableTag !==
      values["candidate.json"].digest.replace("sha256:", "sha256-") ||
    bundleVerification.certificateIdentity !== `https://github.com/${expected.workflowRef}` ||
    canonicalJSON(bundleVerification.files) !==
      canonicalJSON(
        Object.fromEntries(
          Object.entries(values).map(([name, descriptor]) => [name, descriptor.digest]),
        ),
      )
  ) {
    fail("bundle repository or candidate digest mismatch");
  }
  validateCandidate(candidate, expected);
  validateRecipe(recipe, expected, candidate);
  if (digestBytes(`${canonicalJSON(recipe)}\n`) !== expected.imageRecipeDigest) {
    fail("image recipe digest mismatch");
  }
  validateProvenance(provenance, candidate, recipe);
  validateSBOM(sbom, candidate, recipe);
  validateScrub(scrub, candidate);
  return { candidate, recipe, values };
}

function assertSafeOutput(value) {
  const text = canonicalJSON(value);
  if (
    /\/Users\/|\/home\/[^/]+\/|[A-Za-z]:\\Users\\/i.test(text) ||
    /(?:^|[^0-9])(?:10|127)\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}(?:[^0-9]|$)/.test(text) ||
    /(?:^|[^0-9])192\.168\.[0-9]{1,3}\.[0-9]{1,3}(?:[^0-9]|$)/.test(text) ||
    /(?:^|[^0-9])172\.(?:1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}(?:[^0-9]|$)/.test(text)
  ) {
    fail("qualification input contains private machine data");
  }
  if (/https?:\/\//.test(text)) {
    fail("qualification input contains a mutable URL");
  }
}

async function writeAtomic(file, value) {
  const destination = path.resolve(file);
  const parent = path.dirname(destination);
  await mkdir(parent, { recursive: true });
  const temporary = path.join(
    parent,
    `.${path.basename(destination)}.${process.pid}.${Date.now()}.tmp`,
  );
  try {
    await writeFile(temporary, `${canonicalJSON(value)}\n`, {
      encoding: "utf8",
      mode: 0o600,
      flag: "wx",
    });
    await link(temporary, destination);
  } catch (error) {
    await rm(temporary, { force: true });
    if (error?.code === "EEXIST") {
      fail(`qualification output already exists: ${destination}`);
    }
    throw error;
  }
  await rm(temporary, { force: true });
}

async function validateQualificationInputFile(options) {
  const value = await readJSON(required(options, "file"), "qualification input");
  assertSafeOutput(value);
}

async function verify(options) {
  const candidateRef = required(options, "candidate-ref");
  const referenceMatch = candidateRef.match(
    /^(ghcr\.io\/[a-z0-9._-]+(?:\/[a-z0-9._-]+)+)@(sha256:[0-9a-f]{64})$/,
  );
  if (!referenceMatch) fail("candidate reference must be an immutable GHCR digest");
  const [, repository, candidateDigest] = referenceMatch;
  const expected = await validateExpected(options);
  if (repository !== expected.repository) fail("candidate repository mismatch");
  const signatureEvidence = await inspectSignature(options, true, true);
  const manifest = signatureEvidence.manifest;
  const signature = signatureEvidence.signature;
  const verified = await validateBundle(options, expected, manifest);
  const candidate = verified.candidate;
  const output = {
    schema: qualificationSchema,
    artifact: {
      reference: candidateRef,
      repository,
      digest: candidateDigest,
      mediaType: manifestMediaType,
      artifactType,
      signatureDigest: signature.digest,
      signatureType: signature.artifactType,
    },
    source: {
      repository: candidate.source.repository,
      commit: candidate.source.commit,
      workflow: {
        ref: candidate.source.workflow.ref,
        runId: candidate.source.workflow.runId,
        runAttempt: candidate.source.workflow.runAttempt,
      },
    },
    target: {
      platform: candidate.target.platform,
      region: candidate.target.region,
      instanceType: candidate.target.instanceType,
      architecture: candidate.target.architecture,
      baseImage: candidate.target.baseImage,
      amiId: candidate.image.amiId,
      snapshotIds: [...candidate.image.snapshotIds].sort(),
    },
    readiness: {
      profile: candidate.readiness.profile,
      recipe: expected.readinessRecipe,
      recipeDigest: candidate.readiness.recipeDigest,
    },
    imageRecipe: {
      path: expected.imageRecipe,
      digest: expected.imageRecipeDigest,
    },
    evidence: [...expectedFiles.keys()].sort().map((name) => ({ name, ...verified.values[name] })),
    signature: {
      mode: "keyless",
      issuer: "github-actions",
      workflowIdentity: candidate.source.workflow.ref,
    },
  };
  assertSafeOutput(output);
  await writeAtomic(required(options, "output"), output);
  process.stdout.write(`${canonicalJSON(output)}\n`);
}

async function main() {
  const [command, ...argv] = process.argv.slice(2);
  const options = parseOptions(argv);
  if (command === "preflight") {
    const candidateRef = required(options, "candidate-ref");
    const referenceMatch = candidateRef.match(
      /^(ghcr\.io\/[a-z0-9._-]+(?:\/[a-z0-9._-]+)+)@(sha256:[0-9a-f]{64})$/,
    );
    if (!referenceMatch) fail("candidate reference must be an immutable GHCR digest");
    const expected = await validateExpected(options);
    if (referenceMatch[1] !== expected.repository) fail("candidate repository mismatch");
    return;
  }
  if (command === "inspect-signature") {
    const inspected = await inspectSignature(options, false, false);
    process.stdout.write(`${inspected.signature.reference}\n`);
    return;
  }
  if (command === "inspect-signature-manifest") {
    const inspected = await inspectSignature(options, true, false);
    process.stdout.write(`${inspected.repository}@${inspected.layer.digest}\n`);
    return;
  }
  if (command === "verify-signature-evidence") {
    await inspectSignature(options, true, true);
    return;
  }
  if (command === "verify") {
    await verify(options);
    return;
  }
  if (command === "validate-qualification-input") {
    await validateQualificationInputFile(options);
    return;
  }
  fail(
    "usage: consume-aws-image-candidate.mjs preflight|inspect-signature|inspect-signature-manifest|verify-signature-evidence|verify|validate-qualification-input [options]",
  );
}

if (process.argv[1] && path.resolve(process.argv[1]) === scriptPath) {
  try {
    await main();
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  }
}
