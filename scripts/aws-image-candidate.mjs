#!/usr/bin/env node

import { createHash } from "node:crypto";
import {
  access,
  mkdir,
  readdir,
  readFile,
  rename,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const scriptPath = fileURLToPath(import.meta.url);
const repoRoot = path.resolve(path.dirname(scriptPath), "..");
const candidateSchema = "crabbox-aws-image-candidate/v1";
const scrubSchema = "crabbox-aws-image-scrub/v1";
const checksSchema = "crabbox-aws-image-checks/v1";
const recipeSchema = "crabbox-aws-image-recipe/v1";
const bundleSchema = "crabbox-aws-image-evidence-bundle/v1";
const artifactType = "application/vnd.crabbox.aws-image-evidence.v1";
const candidateMediaType =
  "application/vnd.crabbox.aws-image-candidate.v1+json";
const requiredChecks = ["candidate-boot", "candidate-smoke", "scrub", "source-smoke"];

export function canonicalJSON(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonicalJSON(value[key])}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

export function canonicalJSONLine(value) {
  return `${canonicalJSON(value)}\n`;
}

export function digestBytes(bytes) {
  return `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
}

export function digestJSON(value) {
  return digestBytes(canonicalJSON(value));
}

export function digestJSONLine(value) {
  return digestBytes(canonicalJSONLine(value));
}

function fail(message) {
  throw new Error(message);
}

function assertExactKeys(value, keys, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail(`${label} must be an object`);
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    fail(`${label} must contain exactly: ${expected.join(", ")}`);
  }
}

function assertDigest(value, label) {
  if (!/^sha256:[0-9a-f]{64}$/.test(value ?? "")) fail(`${label} must be a SHA-256 digest`);
}

function assertIdentifier(value, pattern, label) {
  if (!pattern.test(value ?? "")) fail(`invalid ${label}`);
}

async function readJSON(file, label) {
  try {
    return JSON.parse(await readFile(file, "utf8"));
  } catch (error) {
    fail(`${label} is not valid JSON: ${error.message}`);
  }
}

function parseOptions(argv) {
  const options = {};
  for (let index = 0; index < argv.length; index += 1) {
    const name = argv[index];
    if (!name.startsWith("--") || index + 1 >= argv.length) fail(`invalid argument ${name}`);
    options[name.slice(2)] = argv[index + 1];
    index += 1;
  }
  return options;
}

function required(options, name) {
  if (!options[name]) fail(`--${name} is required`);
  return options[name];
}

function validateRecipe(recipe, target, expectedPath) {
  assertExactKeys(
    recipe,
    ["schema", "path", "target", "profile", "readinessRecipe", "inputs", "components"],
    "recipe",
  );
  if (
    recipe.schema !== recipeSchema ||
    recipe.path !== expectedPath ||
    recipe.target !== target
  ) {
    fail("recipe schema, path, or target mismatch");
  }
  if (
    !/^recipes\/aws\/v1\/[A-Za-z0-9._-]+[.]json$/.test(recipe.path) ||
    recipe.path.includes("..")
  ) {
    fail("recipe path must identify a versioned AWS recipe");
  }
  assertIdentifier(recipe.profile, /^[a-z0-9][a-z0-9-]*$/, "recipe profile");
  if (
    !/^recipes\/[A-Za-z0-9._/-]+[.]json$/.test(recipe.readinessRecipe) ||
    recipe.readinessRecipe.includes("..") ||
    !recipe.inputs.includes(recipe.readinessRecipe)
  ) {
    fail("recipe readiness input must be a listed repository JSON file");
  }
  if (
    !Array.isArray(recipe.inputs) ||
    recipe.inputs.length === 0 ||
    new Set(recipe.inputs).size !== recipe.inputs.length
  ) {
    fail("recipe inputs must be unique and non-empty");
  }
  for (const input of recipe.inputs) {
    if (!/^(?:recipes|scripts)\/[A-Za-z0-9._/-]+$/.test(input) || input.includes("..")) {
      fail(`invalid recipe input ${input}`);
    }
  }
  if (!Array.isArray(recipe.components) || recipe.components.length === 0) {
    fail("recipe components must be non-empty");
  }
  for (const [index, component] of recipe.components.entries()) {
    assertExactKeys(component, ["name", "version", "source"], `recipe component ${index}`);
    assertIdentifier(component.name, /^[A-Za-z0-9][A-Za-z0-9+._-]*$/, "component name");
    if (!component.version || !component.source) fail("component version and source are required");
  }
}

function validateScrubReport(report, target) {
  assertExactKeys(
    report,
    ["schema", "target", "removed", "findings", "evidenceDigest"],
    "scrub report",
  );
  if (report.schema !== scrubSchema || report.target !== target) fail("scrub report target mismatch");
  assertDigest(report.evidenceDigest, "scrub evidence digest");
  if (!Array.isArray(report.findings) || report.findings.some((item) => typeof item !== "string")) {
    fail("scrub findings must be an array of names");
  }
  if (report.findings.length !== 0) {
    fail(`scrub report contains residual findings: ${report.findings.join(", ")}`);
  }
  const evidence = {
    schema: report.schema,
    target: report.target,
    removed: report.removed,
    findings: report.findings,
  };
  if (digestJSON(evidence) !== report.evidenceDigest) fail("scrub evidence digest mismatch");
  for (const [name, count] of Object.entries(report.removed ?? {})) {
    if (!/^[A-Za-z][A-Za-z0-9]*$/.test(name) || !Number.isSafeInteger(count) || count < 0) {
      fail("scrub report counts are invalid");
    }
  }
}

function validateChecks(value) {
  assertExactKeys(value, ["schema", "checks"], "checks");
  if (value.schema !== checksSchema || !Array.isArray(value.checks)) fail("invalid checks schema");
  const names = [];
  for (const [index, check] of value.checks.entries()) {
    const keys = check.evidenceDigest
      ? ["name", "status", "evidenceDigest"]
      : ["name", "status"];
    assertExactKeys(check, keys, `check ${index}`);
    if (!requiredChecks.includes(check.name) || check.status !== "passed") {
      fail("all required candidate checks must pass");
    }
    if (check.evidenceDigest) assertDigest(check.evidenceDigest, `${check.name} evidence`);
    names.push(check.name);
  }
  if (
    names.length !== requiredChecks.length ||
    [...names].sort().some((name, index) => name !== requiredChecks[index])
  ) {
    fail(`checks must contain exactly: ${requiredChecks.join(", ")}`);
  }
}

function validateScrubCheck(checks, scrubReport) {
  const scrubCheck = checks.checks.find((check) => check.name === "scrub");
  if (scrubCheck?.evidenceDigest !== scrubReport.evidenceDigest) {
    fail("scrub check evidence digest does not match scrub report");
  }
}

function validateCheckpoint(record, options) {
  if (record.kind !== "aws-ami" || record.provider !== "aws") fail("checkpoint must be an AWS AMI");
  if (record.targetOS !== options.target) fail("checkpoint target does not match candidate target");
  const native = record.native;
  if (!native || native.provider !== "aws" || native.kind !== "aws-ami") {
    fail("checkpoint native image metadata is incomplete");
  }
  assertIdentifier(native.imageId, /^ami-[0-9a-z]+$/, "AMI id");
  if (native.region !== options.region) fail("checkpoint region does not match candidate region");
  if (native.architecture !== options.architecture) {
    fail("checkpoint architecture does not match candidate architecture");
  }
  const snapshots = native.snapshotIds ?? [];
  if (
    !Array.isArray(snapshots) ||
    snapshots.length === 0 ||
    snapshots.some((id) => !/^snap-[0-9a-z]+$/.test(id)) ||
    new Set(snapshots).size !== snapshots.length
  ) {
    fail("checkpoint must contain unique AWS snapshot ids");
  }
  return { amiId: native.imageId, snapshotIds: [...snapshots].sort() };
}

async function recipeDependencies(recipe, sourceRepository, sourceCommit) {
  const dependencies = [];
  for (const relative of recipe.inputs) {
    const file = path.resolve(repoRoot, relative);
    if (!file.startsWith(`${repoRoot}${path.sep}`)) fail("recipe input escaped repository root");
    const metadata = await stat(file);
    if (!metadata.isFile()) fail(`recipe input is not a file: ${relative}`);
    dependencies.push({
      uri: `git+https://github.com/${sourceRepository}@${sourceCommit}#path=${relative}`,
      digest: { sha256: createHash("sha256").update(await readFile(file)).digest("hex") },
    });
  }
  return dependencies;
}

function spdxID(name, index) {
  return `SPDXRef-Package-${index}-${name.replaceAll(/[^A-Za-z0-9.-]/g, "-")}`;
}

function sbom(recipe, candidate, createdAt) {
  return {
    spdxVersion: "SPDX-2.3",
    dataLicense: "CC0-1.0",
    SPDXID: "SPDXRef-DOCUMENT",
    name: `crabbox-${candidate.target.platform}-${candidate.image.amiId}`,
    documentNamespace: `https://github.com/${candidate.source.repository}/aws-image-candidates/${candidate.source.commit}/${candidate.image.amiId}`,
    creationInfo: {
      created: createdAt,
      creators: ["Tool: scripts/aws-image-candidate.mjs"],
    },
    packages: recipe.components.map((component, index) => ({
      SPDXID: spdxID(component.name, index),
      name: component.name,
      versionInfo: component.version,
      downloadLocation: "NOASSERTION",
      filesAnalyzed: false,
      licenseConcluded: "NOASSERTION",
      licenseDeclared: "NOASSERTION",
      supplier: `Organization: ${component.source}`,
    })),
  };
}

function validateCandidateRecord(candidate) {
  assertExactKeys(
    candidate,
    ["schema", "createdAt", "source", "readiness", "target", "image", "previousDefault", "checks"],
    "candidate record",
  );
  if (candidate.schema !== candidateSchema || Number.isNaN(Date.parse(candidate.createdAt))) {
    fail("invalid CandidateRecordV1 header");
  }
  assertExactKeys(candidate.source, ["repository", "commit", "workflow"], "candidate source");
  assertIdentifier(candidate.source.repository, /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/, "source repository");
  assertIdentifier(candidate.source.commit, /^[0-9a-f]{40}$/, "source commit");
  assertExactKeys(candidate.source.workflow, ["ref", "runId", "runAttempt"], "candidate workflow");
  assertIdentifier(
    candidate.source.workflow.ref,
    new RegExp(
      `^${candidate.source.repository.replaceAll(/[.*+?^${}()|[\]\\]/g, "\\$&")}/\\.github/workflows/[A-Za-z0-9._/-]+\\.ya?ml@refs/heads/[A-Za-z0-9._/-]+$`,
    ),
    "workflow ref",
  );
  assertIdentifier(candidate.source.workflow.runId, /^(?:local|[1-9][0-9]*)$/, "workflow run id");
  assertIdentifier(candidate.source.workflow.runAttempt, /^[1-9][0-9]*$/, "workflow run attempt");
  assertExactKeys(candidate.readiness, ["profile", "recipeDigest"], "candidate readiness");
  assertIdentifier(candidate.readiness.profile, /^[a-z0-9][a-z0-9-]*$/, "readiness profile");
  assertDigest(candidate.readiness.recipeDigest, "readiness recipe digest");
  assertExactKeys(
    candidate.target,
    ["platform", "region", "instanceType", "architecture", "baseImage"],
    "candidate target",
  );
  if (!["linux", "windows"].includes(candidate.target.platform)) fail("invalid candidate platform");
  assertIdentifier(candidate.target.region, /^[a-z]{2}(?:-gov)?-[a-z]+-[0-9]+$/, "AWS region");
  assertIdentifier(candidate.target.instanceType, /^[A-Za-z0-9.-]+$/, "instance type");
  assertIdentifier(candidate.target.architecture, /^(?:x86_64|arm64)$/, "architecture");
  assertIdentifier(candidate.target.baseImage, /^ami-[0-9a-z]+$/, "base image");
  assertExactKeys(candidate.image, ["amiId", "snapshotIds"], "candidate image");
  assertIdentifier(candidate.image.amiId, /^ami-[0-9a-z]+$/, "AMI id");
  if (
    !Array.isArray(candidate.image.snapshotIds) ||
    candidate.image.snapshotIds.length === 0 ||
    new Set(candidate.image.snapshotIds).size !== candidate.image.snapshotIds.length ||
    candidate.image.snapshotIds.some((id) => !/^snap-[0-9a-z]+$/.test(id))
  ) {
    fail("candidate image must contain unique AWS snapshot ids");
  }
  if (candidate.previousDefault !== null) {
    assertExactKeys(candidate.previousDefault, ["imageId", "source"], "previous default");
    assertIdentifier(candidate.previousDefault.imageId, /^ami-[0-9a-z]+$/, "previous default");
    if (candidate.previousDefault.source !== "operator-input") fail("invalid previous default source");
  }
  validateChecks({ schema: checksSchema, checks: candidate.checks });
}

async function writeBundle(options) {
  const target = required(options, "target");
  if (!["linux", "windows"].includes(target)) fail("--target must be linux or windows");
  const region = required(options, "region");
  const instanceType = required(options, "instance-type");
  const architecture = required(options, "architecture");
  const baseImage = required(options, "base-image");
  const sourceRepository = required(options, "source-repository");
  const sourceCommit = required(options, "source-commit");
  const workflowRef = required(options, "workflow-ref");
  const workflowRunId = options["workflow-run-id"] || "local";
  const workflowRunAttempt = options["workflow-run-attempt"] || "1";
  const [sourceOwner, sourceName] = sourceRepository.toLowerCase().split("/");
  const ociRepository =
    options["oci-repository"] || `ghcr.io/${sourceOwner}/${sourceName}-aws-image-candidates`;
  const createdAt = options["created-at"] || new Date().toISOString();
  assertIdentifier(region, /^[a-z]{2}(?:-gov)?-[a-z]+-[0-9]+$/, "AWS region");
  assertIdentifier(instanceType, /^[A-Za-z0-9.-]+$/, "instance type");
  assertIdentifier(architecture, /^(?:x86_64|arm64)$/, "architecture");
  assertIdentifier(baseImage, /^ami-[0-9a-z]+$/, "base image");
  assertIdentifier(sourceRepository, /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/, "source repository");
  assertIdentifier(sourceCommit, /^[0-9a-f]{40}$/, "source commit");
  assertIdentifier(
    workflowRef,
    new RegExp(
      `^${sourceRepository.replaceAll(/[.*+?^${}()|[\]\\]/g, "\\$&")}/\\.github/workflows/[A-Za-z0-9._/-]+\\.ya?ml@refs/heads/[A-Za-z0-9._/-]+$`,
    ),
    "workflow ref",
  );
  assertIdentifier(
    ociRepository,
    /^ghcr\.io\/[a-z0-9_.-]+(?:\/[a-z0-9_.-]+)+$/,
    "OCI repository",
  );
  assertIdentifier(workflowRunId, /^(?:local|[1-9][0-9]*)$/, "workflow run id");
  assertIdentifier(workflowRunAttempt, /^[1-9][0-9]*$/, "workflow run attempt");
  if (Number.isNaN(Date.parse(createdAt))) fail("invalid candidate creation time");

  const recipeFile = path.resolve(required(options, "recipe"));
  const recipePath = path.relative(repoRoot, recipeFile).split(path.sep).join("/");
  if (recipePath.startsWith("../") || path.isAbsolute(recipePath)) {
    fail("recipe path escaped repository root");
  }
  const recipe = await readJSON(recipeFile, "recipe");
  validateRecipe(recipe, target, recipePath);
  const scrubReport = await readJSON(required(options, "scrub-report"), "scrub report");
  validateScrubReport(scrubReport, target);
  const checks = await readJSON(required(options, "checks"), "checks");
  validateChecks(checks);
  validateScrubCheck(checks, scrubReport);
  const checkpoint = await readJSON(required(options, "checkpoint"), "checkpoint");
  const image = validateCheckpoint(checkpoint, {
    target,
    region,
    architecture,
  });
  const previousDefault = options["previous-default"]
    ? { imageId: options["previous-default"], source: "operator-input" }
    : null;
  if (previousDefault) assertIdentifier(previousDefault.imageId, /^ami-[0-9a-z]+$/, "previous default");

  const readinessRecipePath = path.resolve(repoRoot, recipe.readinessRecipe);
  const readinessRecipe = JSON.parse(await readFile(readinessRecipePath, "utf8"));
  const candidate = {
    schema: candidateSchema,
    createdAt,
    source: {
      repository: sourceRepository,
      commit: sourceCommit,
      workflow: {
        ref: workflowRef,
        runId: workflowRunId,
        runAttempt: workflowRunAttempt,
      },
    },
    readiness: {
      profile: recipe.profile,
      recipeDigest: digestJSON(readinessRecipe),
    },
    target: {
      platform: target,
      region,
      instanceType,
      architecture,
      baseImage,
    },
    image,
    previousDefault,
    checks: [...checks.checks].sort((left, right) => left.name.localeCompare(right.name)),
  };
  const candidateText = `${canonicalJSON(candidate)}\n`;
  const candidateDigest = digestBytes(candidateText);
  const immutableTag = candidateDigest.replace("sha256:", "sha256-");
  const dependencies = await recipeDependencies(recipe, sourceRepository, sourceCommit);
  const provenance = {
    _type: "https://in-toto.io/Statement/v1",
    subject: [
      {
        name: "candidate.json",
        digest: { sha256: candidateDigest.slice("sha256:".length) },
      },
    ],
    predicateType: "https://slsa.dev/provenance/v1",
    predicate: {
      buildDefinition: {
        buildType: "urn:crabbox:aws-image-candidate:v1",
        externalParameters: {
          readiness: candidate.readiness,
          target: candidate.target,
        },
        internalParameters: {},
        resolvedDependencies: dependencies,
      },
      runDetails: {
        builder: {
          id: `https://github.com/${sourceRepository}/actions/runs/${candidate.source.workflow.runId}`,
        },
        metadata: {
          invocationId: `${candidate.source.workflow.runId}/${candidate.source.workflow.runAttempt}`,
          startedOn: createdAt,
          finishedOn: createdAt,
        },
      },
    },
  };

  const artifacts = {
    "candidate.json": candidateText,
    "recipe.json": canonicalJSONLine(recipe),
    "scrub-report.json": `${canonicalJSON(scrubReport)}\n`,
    "sbom.spdx.json": `${canonicalJSON(sbom(recipe, candidate, createdAt))}\n`,
    "provenance.intoto.jsonl": `${canonicalJSON(provenance)}\n`,
  };
  const files = Object.entries(artifacts)
    .map(([name, content]) => ({
      name,
      mediaType:
        name === "candidate.json"
          ? candidateMediaType
          : name.endsWith(".jsonl")
            ? "application/vnd.in-toto+json"
            : name.includes("spdx")
              ? "application/spdx+json"
              : "application/json",
      digest: digestBytes(content),
      size: Buffer.byteLength(content),
    }))
    .sort((left, right) => left.name.localeCompare(right.name));
  const bundle = {
    schema: bundleSchema,
    artifactType,
    candidate: {
      mediaType: candidateMediaType,
      digest: candidateDigest,
      size: Buffer.byteLength(candidateText),
    },
    files,
    oci: {
      repository: ociRepository,
      immutableTag,
      signature: {
        mode: "keyless",
        oidcIssuer: "https://token.actions.githubusercontent.com",
        certificateIdentity: `https://github.com/${workflowRef}`,
      },
    },
  };
  artifacts["bundle.json"] = `${canonicalJSON(bundle)}\n`;

  const output = path.resolve(required(options, "out-dir"));
  try {
    await access(output);
    fail(`candidate output already exists: ${output}`);
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }
  const parent = path.dirname(output);
  await mkdir(parent, { recursive: true });
  const temporary = path.join(parent, `.${path.basename(output)}.${process.pid}.${Date.now()}.tmp`);
  await mkdir(temporary, { mode: 0o700 });
  try {
    await Promise.all(
      Object.entries(artifacts).map(([name, content]) =>
        writeFile(path.join(temporary, name), content, { mode: 0o644 }),
      ),
    );
    await rename(temporary, output);
  } catch (error) {
    await rm(temporary, { recursive: true, force: true });
    throw error;
  }
  process.stdout.write(
    `${canonicalJSON({
      schema: "crabbox-aws-image-candidate-bundle/v1",
      candidateDigest,
      immutableTag,
      ociRepository,
    })}\n`,
  );
}

async function verifyBundle(options) {
  const directory = path.resolve(required(options, "dir"));
  const expected = [
    "bundle.json",
    "candidate.json",
    "provenance.intoto.jsonl",
    "recipe.json",
    "sbom.spdx.json",
    "scrub-report.json",
  ];
  const actual = (await readdir(directory)).sort();
  if (
    actual.length !== expected.length ||
    actual.some((name, index) => name !== expected[index])
  ) {
    fail(`candidate bundle must contain exactly: ${expected.join(", ")}`);
  }
  const values = {};
  for (const name of expected) {
    const bytes = await readFile(path.join(directory, name));
    JSON.parse(bytes);
    values[name] = digestBytes(bytes);
  }
  const bundle = await readJSON(path.join(directory, "bundle.json"), "bundle manifest");
  assertExactKeys(bundle, ["schema", "artifactType", "candidate", "files", "oci"], "bundle manifest");
  if (bundle.schema !== bundleSchema || bundle.artifactType !== artifactType) {
    fail("invalid OCI evidence bundle contract");
  }
  const candidate = await readJSON(path.join(directory, "candidate.json"), "candidate record");
  validateCandidateRecord(candidate);
  const recipe = await readJSON(path.join(directory, "recipe.json"), "recipe");
  validateRecipe(recipe, candidate.target.platform, recipe.path);
  const scrubReport = await readJSON(path.join(directory, "scrub-report.json"), "scrub report");
  validateScrubReport(scrubReport, candidate.target.platform);
  validateScrubCheck({ schema: checksSchema, checks: candidate.checks }, scrubReport);
  const readinessRecipe = await readJSON(
    path.resolve(repoRoot, recipe.readinessRecipe),
    "readiness recipe",
  );
  if (
    recipe.profile !== candidate.readiness.profile ||
    digestJSON(readinessRecipe) !== candidate.readiness.recipeDigest
  ) {
    fail("candidate readiness recipe mismatch");
  }
  assertExactKeys(bundle.candidate, ["mediaType", "digest", "size"], "candidate descriptor");
  if (
    bundle.candidate.mediaType !== candidateMediaType ||
    bundle.candidate?.digest !== values["candidate.json"] ||
    bundle.candidate.size !== (await stat(path.join(directory, "candidate.json"))).size ||
    bundle.oci?.immutableTag !== values["candidate.json"].replace("sha256:", "sha256-")
  ) {
    fail("bundle candidate digest or immutable tag mismatch");
  }
  assertExactKeys(bundle.oci, ["repository", "immutableTag", "signature"], "bundle OCI contract");
  assertIdentifier(
    bundle.oci.repository,
    /^ghcr\.io\/[a-z0-9_.-]+(?:\/[a-z0-9_.-]+)+$/,
    "OCI repository",
  );
  assertExactKeys(
    bundle.oci.signature,
    ["mode", "oidcIssuer", "certificateIdentity"],
    "bundle signature contract",
  );
  if (
    bundle.oci.signature.mode !== "keyless" ||
    bundle.oci.signature.oidcIssuer !== "https://token.actions.githubusercontent.com" ||
    bundle.oci.signature.certificateIdentity !==
      `https://github.com/${candidate.source.workflow.ref}`
  ) {
    fail("invalid keyless signature contract");
  }
  const descriptors = new Map((bundle.files ?? []).map((item) => [item.name, item]));
  if (bundle.files?.length !== 5 || descriptors.size !== 5) {
    fail("bundle must contain exactly five unique file descriptors");
  }
  const mediaTypes = {
    "candidate.json": candidateMediaType,
    "provenance.intoto.jsonl": "application/vnd.in-toto+json",
    "recipe.json": "application/json",
    "sbom.spdx.json": "application/spdx+json",
    "scrub-report.json": "application/json",
  };
  for (const name of expected.filter((item) => item !== "bundle.json")) {
    const descriptor = descriptors.get(name);
    const bytes = await readFile(path.join(directory, name));
    assertExactKeys(descriptor, ["name", "mediaType", "digest", "size"], `descriptor ${name}`);
    if (
      descriptor.name !== name ||
      descriptor.mediaType !== mediaTypes[name] ||
      descriptor.digest !== values[name] ||
      descriptor.size !== bytes.length
    ) {
      fail(`bundle descriptor mismatch for ${name}`);
    }
  }
  process.stdout.write(
    `${canonicalJSON({
      schema: "crabbox-aws-image-candidate-bundle/v1",
      candidateDigest: values["candidate.json"],
      immutableTag: values["candidate.json"].replace("sha256:", "sha256-"),
      ociRepository: bundle.oci.repository,
      certificateIdentity: bundle.oci.signature.certificateIdentity,
      files: values,
    })}\n`,
  );
}

async function main() {
  const [command, ...argv] = process.argv.slice(2);
  const options = parseOptions(argv);
  if (command === "create") await writeBundle(options);
  else if (command === "verify") await verifyBundle(options);
  else fail("usage: aws-image-candidate.mjs create|verify [options]");
}

if (process.argv[1] && path.resolve(process.argv[1]) === scriptPath) {
  try {
    await main();
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  }
}
