#!/usr/bin/env node

import { link, readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { canonicalJSON, digestBytes, digestJSON } from "./aws-image-candidate.mjs";

const schema = "crabbox-aws-image-qualification/v1";
const inputSchema = "crabbox-aws-image-qualification-input/v1";
const identitySchema = "crabbox-ready-pool-identity/v1";
const negativeSchema = "crabbox-aws-image-qualification-negative/v1";
const statusSchema = "crabbox-aws-image-qualification-status/v1";
const cacheSchema = "crabbox-aws-image-qualification-cache/v1";
const digestPattern = /^sha256:[0-9a-f]{64}$/;
const linuxOSSelectors = new Set(["ubuntu:26.04", "ubuntu:24.04"]);

function fail(message) {
  throw new Error(message);
}

function parseOptions(argv) {
  const options = {};
  for (let index = 0; index < argv.length; index += 1) {
    const name = argv[index];
    if (!name.startsWith("--") || index + 1 >= argv.length) fail(`invalid argument ${name}`);
    if (options[name.slice(2)] !== undefined) fail(`duplicate argument ${name}`);
    options[name.slice(2)] = argv[index + 1];
    index += 1;
  }
  return options;
}

function required(options, name) {
  const value = options[name];
  if (!value) fail(`--${name} is required`);
  return value;
}

function exactKeys(value, keys, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail(`${label} must be an object`);
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    fail(`${label} must contain exactly: ${expected.join(", ")}`);
  }
}

async function readJSON(file, label) {
  let text;
  try {
    text = await readFile(file, "utf8");
  } catch (error) {
    fail(`read ${label}: ${error.message}`);
  }
  try {
    return { value: JSON.parse(text), bytes: Buffer.from(text) };
  } catch (error) {
    fail(`${label} is not valid JSON: ${error.message}`);
  }
}

async function readTiming(file, label) {
  const text = await readFile(file, "utf8");
  const candidates = [text, ...text.trim().split("\n").reverse()];
  for (const candidate of candidates) {
    try {
      const value = JSON.parse(candidate);
      if (value && Array.isArray(value.runnerPhases)) return value;
    } catch {
      // Continue until the final timing record is found.
    }
  }
  fail(`${label} has no TimingReport JSON record`);
}

async function readEvents(file, label) {
  const value = (await readJSON(file, label)).value;
  if (!Array.isArray(value)) fail(`${label} must be an array`);
  return value;
}

function assertDigest(value, label) {
  if (!digestPattern.test(value ?? "")) fail(`invalid ${label}`);
}

function validateInput(input) {
  exactKeys(
    input,
    ["schema", "artifact", "source", "target", "readiness", "imageRecipe", "evidence", "signature"],
    "qualification input",
  );
  if (input.schema !== inputSchema) fail("unsupported qualification input schema");
  if (input.target?.platform !== "linux") fail("qualification requires a Linux candidate");
  if (input.target?.architecture !== "x86_64" && input.target?.architecture !== "arm64") {
    fail("qualification input architecture is invalid");
  }
  if (!/^ami-[0-9a-z]+$/.test(input.target?.amiId ?? "")) fail("qualification input AMI is invalid");
  if (!/^[A-Za-z0-9.-]+$/.test(input.target?.instanceType ?? "")) {
    fail("qualification input instance type is invalid");
  }
  assertDigest(input.artifact?.digest, "candidate artifact digest");
  assertDigest(input.readiness?.recipeDigest, "readiness recipe digest");
  const candidate = input.evidence.find((item) => item?.name === "candidate.json");
  if (!candidate || input.evidence.filter((item) => item?.name === "candidate.json").length !== 1) {
    fail("qualification input must bind exactly one candidate.json");
  }
  assertDigest(candidate.digest, "candidate record digest");
  return candidate;
}

function inputSummary(input) {
  const candidate = validateInput(input);
  return {
    amiId: input.target.amiId,
    region: input.target.region,
    instanceType: input.target.instanceType,
    architecture: input.target.architecture,
    profile: input.readiness.profile,
    recipeDigest: input.readiness.recipeDigest,
    artifactDigest: input.artifact.digest,
    candidateRecordDigest: candidate.digest,
  };
}

function validateIdentity(identity, input) {
  exactKeys(
    identity,
    [
      "schema",
      "profile",
      "recipeDigest",
      "inventoryDigest",
      "imageID",
      "architecture",
      "seedDigest",
      "cacheABIDigest",
    ],
    "ready-pool identity",
  );
  if (identity.schema !== identitySchema) fail("unsupported ready-pool identity schema");
  for (const field of ["recipeDigest", "inventoryDigest", "seedDigest", "cacheABIDigest"]) {
    assertDigest(identity[field], `ready-pool identity ${field}`);
  }
  const expectedArchitecture = input.target.architecture === "x86_64" ? "amd64" : "arm64";
  if (
    identity.imageID !== input.target.amiId ||
    identity.architecture !== expectedArchitecture ||
    identity.profile !== input.readiness.profile ||
    identity.recipeDigest !== input.readiness.recipeDigest
  ) {
    fail("ready-pool identity does not match the qualification input");
  }
}

function validateReceiptTarget(target) {
  exactKeys(
    target,
    [
      "amiId",
      "region",
      "instanceType",
      "architecture",
      "osSelector",
      "market",
      "profile",
      "recipeDigest",
    ],
    "qualification target",
  );
  if (!/^ami-[0-9a-z]+$/.test(target.amiId ?? "")) fail("qualification target AMI is invalid");
  if (!/^[a-z]{2}(?:-gov)?-[a-z]+-[0-9]+$/.test(target.region ?? "")) {
    fail("qualification target region is invalid");
  }
  if (!/^[A-Za-z0-9.-]+$/.test(target.instanceType ?? "")) {
    fail("qualification target instance type is invalid");
  }
  if (target.architecture !== "x86_64" && target.architecture !== "arm64") {
    fail("qualification target architecture is invalid");
  }
  if (!linuxOSSelectors.has(target.osSelector)) fail("qualification target OS selector is invalid");
  if (target.market !== "on-demand") fail("qualification target market is invalid");
  if (!/^[a-z0-9][a-z0-9-]*$/.test(target.profile ?? "")) {
    fail("qualification target profile is invalid");
  }
  assertDigest(target.recipeDigest, "qualification target recipe digest");
}

function validateNegative(value) {
  exactKeys(value, ["schema", "gates"], "negative gate evidence");
  if (value.schema !== negativeSchema || !Array.isArray(value.gates)) {
    fail("negative gate evidence schema is invalid");
  }
  const expected = ["ami", "architecture", "recipe", "type"];
  const dimensions = value.gates.map((gate) => {
    exactKeys(gate, ["dimension", "status"], "negative gate");
    if (gate.status !== "passed") fail(`negative ${gate.dimension} gate did not pass`);
    return gate.dimension;
  });
  if (dimensions.length !== expected.length || dimensions.sort().join(",") !== expected.join(",")) {
    fail("negative gates must cover ami, architecture, recipe, and type exactly once");
  }
  return expected.map((dimension) => ({ dimension, status: "passed" }));
}

function validatePassedStatus(value, label) {
  exactKeys(value, ["schema", "status"], label);
  if (value.schema !== statusSchema || value.status !== "passed") fail(`${label} did not pass`);
  return { status: "passed" };
}

function validateReceiptOverlay(value, expectedFiles, label, dirty = false) {
  exactKeys(
    value,
    dirty
      ? ["status", "mode", "fallback", "trackedTransfer", "fixtureDigest"]
      : ["status", "mode", "fallback", "trackedTransfer"],
    label,
  );
  if (value.status !== "passed" || value.mode !== "overlay" || value.fallback !== false) {
    fail(`${label} is invalid`);
  }
  exactKeys(value.trackedTransfer, ["files", "bytes"], `${label} tracked transfer`);
  if (
    value.trackedTransfer.files !== expectedFiles ||
    !Number.isSafeInteger(value.trackedTransfer.bytes) ||
    value.trackedTransfer.bytes < 0 ||
    (dirty && value.trackedTransfer.bytes === 0) ||
    (!dirty && value.trackedTransfer.bytes !== 0)
  ) {
    fail(`${label} tracked transfer is invalid`);
  }
  if (dirty) assertDigest(value.fixtureDigest, `${label} fixture digest`);
}

function overlayEvidence(report, events, expectedFiles, expectedBytes, label) {
  if (report.exitCode !== 0 || report.syncSkipped !== false) fail(`${label} did not execute a synced run`);
  if (!/^run_[0-9a-z]+$/.test(report.runId ?? "")) fail(`${label} is missing its recorded run id`);
  const sync = report.runnerPhases.filter((phase) => phase?.name === "workspace.sync");
  const overlay = report.runnerPhases.filter((phase) => phase?.name === "workspace.overlay");
  if (sync.length !== 0 || overlay.length !== 1) fail(`${label} did not use the Git overlay path exactly once`);
  const phase = overlay[0];
  if (phase.fallback === true || phase.reason) fail(`${label} used an overlay fallback`);
  const phaseFiles = phase.transferCount === undefined ? 0 : phase.transferCount;
  const phaseBytes = phase.transferBytes === undefined ? 0 : phase.transferBytes;
  if (
    !Number.isSafeInteger(phaseFiles) ||
    phaseFiles < 0 ||
    !Number.isSafeInteger(phaseBytes) ||
    phaseBytes < 0
  ) {
    fail(`${label} timing phase transfer counters are invalid`);
  }
  if (events.length !== 1) fail(`${label} must bind exactly one sync.finished event`);
  const event = events[0];
  if (
    event.runID !== report.runId ||
    event.type !== "sync.finished" ||
    event.phase !== "synced"
  ) {
    fail(`${label} sync event does not match the timing report`);
  }
  const match = /^duration=\S+ skipped=false mode=overlay files=([0-9]+) bytes=([0-9]+)$/.exec(
    event.message ?? "",
  );
  if (!match) fail(`${label} sync event omitted exact overlay transfer evidence`);
  const files = Number(match[1]);
  const bytes = Number(match[2]);
  if (files !== phaseFiles || bytes !== phaseBytes) {
    fail(`${label} timing phase transfer counters do not match its sync event`);
  }
  if (files !== expectedFiles || bytes !== expectedBytes) {
    fail(`${label} tracked transfer was ${files} files/${bytes} bytes, expected ${expectedFiles}/${expectedBytes}`);
  }
  return {
    status: "passed",
    mode: "overlay",
    fallback: false,
    trackedTransfer: { files, bytes },
  };
}

function validateCache(value) {
  exactKeys(value, ["schema", "advertised", "status", "reason"], "cache evidence");
  if (value.schema !== cacheSchema || typeof value.advertised !== "boolean") {
    fail("cache evidence schema is invalid");
  }
  if (
    value.advertised === false &&
    (value.status !== "skipped" || value.reason !== "provider_capability_not_advertised")
  ) {
    fail("unadvertised cache capability must be explicitly skipped");
  }
  if (value.advertised === true && (value.status !== "passed" || value.reason !== "verified")) {
    fail("advertised cache capability must pass verification");
  }
  return {
    advertised: value.advertised,
    status: value.status,
    reason: value.reason,
  };
}

function assertPublicReceipt(value) {
  const bannedKeys = /(?:lease|host|ssh|workroot|owner|org|tenant|token|secret|email)/i;
  const bannedValues = [
    /(?:^|[^a-z0-9])cbx_[0-9a-z]+/i,
    /\/Users\//,
    /\/home\/[^/]+/,
    /(?:^|[^0-9])(?:10|127|169[.]254|172[.](?:1[6-9]|2[0-9]|3[01])|192[.]168)[.][0-9.]+/,
  ];
  const visit = (item, parent = "") => {
    if (Array.isArray(item)) return item.forEach((child) => visit(child, parent));
    if (item && typeof item === "object") {
      for (const [key, child] of Object.entries(item)) {
        if (bannedKeys.test(key)) fail(`qualification receipt contains private field ${parent}${key}`);
        visit(child, `${parent}${key}.`);
      }
      return;
    }
    if (typeof item === "string" && bannedValues.some((pattern) => pattern.test(item))) {
      fail(`qualification receipt contains private value at ${parent.slice(0, -1)}`);
    }
  };
  visit(value);
}

async function writeAtomic(destination, value) {
  const data = `${canonicalJSON(value)}\n`;
  const temporary = `${destination}.${process.pid}.${Date.now()}.tmp`;
  await writeFile(temporary, data, { mode: 0o600, flag: "wx" });
  try {
    await link(temporary, destination);
  } catch (error) {
    await rm(temporary, { force: true });
    if (error.code === "EEXIST") fail(`qualification output already exists: ${destination}`);
    throw error;
  }
  await rm(temporary, { force: true });
}

async function build(options) {
  const inputRecord = await readJSON(required(options, "input"), "qualification input");
  const input = inputRecord.value;
  const candidate = validateInput(input);
  const identity = (await readJSON(required(options, "identity"), "ready-pool identity")).value;
  validateIdentity(identity, input);
  const negative = validateNegative(
    (await readJSON(required(options, "negative"), "negative gate evidence")).value,
  );
  const positive = validatePassedStatus(
    (await readJSON(required(options, "positive"), "positive gate evidence")).value,
    "positive gate evidence",
  );
  const fixture = await readFile(required(options, "dirty-fixture"));
  const dirtyOutput = await readFile(required(options, "dirty-output"));
  if (!dirtyOutput.equals(fixture)) fail("dirty overlay command output does not match the fixture bytes");
  const cleanReport = await readTiming(required(options, "clean-timing"), "clean timing");
  const dirtyReport = await readTiming(required(options, "dirty-timing"), "dirty timing");
  const cleanEvents = await readEvents(required(options, "clean-events"), "clean sync events");
  const dirtyEvents = await readEvents(required(options, "dirty-events"), "dirty sync events");
  const cleanOverlay = overlayEvidence(cleanReport, cleanEvents, 0, 0, "clean overlay");
  const dirtyOverlay = {
    ...overlayEvidence(dirtyReport, dirtyEvents, 1, fixture.length, "dirty overlay"),
    fixtureDigest: digestBytes(fixture),
  };
  const cache = validateCache((await readJSON(required(options, "cache"), "cache evidence")).value);
  const osSelectorGate = validatePassedStatus(
    (await readJSON(required(options, "os-evidence"), "OS selector evidence")).value,
    "OS selector evidence",
  );
  const cleanup = validatePassedStatus(
    (await readJSON(required(options, "cleanup"), "cleanup evidence")).value,
    "cleanup evidence",
  );
  const workflowIdentity = required(options, "workflow-ref");
  if (
    !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/[.]github\/workflows\/devtools-image-qualify[.]yml@refs\/heads\/[A-Za-z0-9._/-]+$/.test(
      workflowIdentity,
    )
  ) {
    fail("qualification workflow identity is invalid");
  }
  const runId = required(options, "workflow-run-id");
  const runAttempt = required(options, "workflow-run-attempt");
  if (!/^[1-9][0-9]*$/.test(runId) || !/^[1-9][0-9]*$/.test(runAttempt)) {
    fail("qualification workflow run identity is invalid");
  }
  const createdAt = required(options, "created-at");
  if (Number.isNaN(Date.parse(createdAt))) fail("--created-at must be an RFC3339 timestamp");
  const osSelector = required(options, "os-selector");
  if (!linuxOSSelectors.has(osSelector)) fail("--os-selector must be ubuntu:26.04 or ubuntu:24.04");
  const output = {
    schema,
    createdAt: new Date(createdAt).toISOString(),
    candidate: {
      artifactDigest: input.artifact.digest,
      candidateRecordDigest: candidate.digest,
      qualificationInputDigest: digestBytes(inputRecord.bytes),
    },
    target: {
      amiId: input.target.amiId,
      region: input.target.region,
      instanceType: input.target.instanceType,
      architecture: input.target.architecture,
      osSelector,
      market: "on-demand",
      profile: input.readiness.profile,
      recipeDigest: input.readiness.recipeDigest,
    },
    pool: {
      identity,
      identityDigest: digestJSON(identity),
    },
    gates: {
      negative,
      positive,
      osSelector: osSelectorGate,
      cleanOverlay,
      dirtyOverlay,
    },
    cache,
    cleanup,
    workflow: {
      identity: workflowIdentity,
      runId,
      runAttempt,
    },
  };
  assertPublicReceipt(output);
  await writeAtomic(required(options, "output"), output);
  process.stdout.write(`${canonicalJSON(output)}\n`);
}

async function verify(options) {
  const record = await readJSON(required(options, "file"), "qualification receipt");
  exactKeys(
    record.value,
    ["schema", "createdAt", "candidate", "target", "pool", "gates", "cache", "cleanup", "workflow"],
    "qualification receipt",
  );
  if (record.value.schema !== schema) fail("unsupported qualification receipt schema");
  if (
    typeof record.value.createdAt !== "string" ||
    Number.isNaN(Date.parse(record.value.createdAt)) ||
    new Date(record.value.createdAt).toISOString() !== record.value.createdAt
  ) {
    fail("qualification receipt timestamp is invalid");
  }
  exactKeys(
    record.value.candidate,
    ["artifactDigest", "candidateRecordDigest", "qualificationInputDigest"],
    "qualification candidate",
  );
  assertDigest(record.value.candidate?.artifactDigest, "candidate artifact digest");
  assertDigest(record.value.candidate?.candidateRecordDigest, "candidate record digest");
  assertDigest(record.value.candidate?.qualificationInputDigest, "qualification input digest");
  validateReceiptTarget(record.value.target);
  exactKeys(record.value.pool, ["identity", "identityDigest"], "qualification pool");
  assertDigest(record.value.pool?.identityDigest, "ready-pool identity digest");
  if (digestJSON(record.value.pool?.identity) !== record.value.pool.identityDigest) {
    fail("qualification receipt identity digest mismatch");
  }
  validateIdentity(record.value.pool.identity, {
    target: {
      amiId: record.value.target.amiId,
      architecture: record.value.target.architecture,
    },
    readiness: {
      profile: record.value.target.profile,
      recipeDigest: record.value.target.recipeDigest,
    },
  });
  exactKeys(
    record.value.gates,
    ["negative", "positive", "osSelector", "cleanOverlay", "dirtyOverlay"],
    "qualification gates",
  );
  validateNegative({ schema: negativeSchema, gates: record.value.gates?.negative });
  validatePassedStatus(
    { schema: statusSchema, ...record.value.gates?.positive },
    "positive gate evidence",
  );
  validatePassedStatus(
    { schema: statusSchema, ...record.value.gates?.osSelector },
    "OS selector evidence",
  );
  validateReceiptOverlay(record.value.gates.cleanOverlay, 0, "clean overlay");
  validateReceiptOverlay(record.value.gates.dirtyOverlay, 1, "dirty overlay", true);
  validateCache({ schema: cacheSchema, ...record.value.cache });
  validatePassedStatus({ schema: statusSchema, ...record.value.cleanup }, "cleanup evidence");
  exactKeys(record.value.workflow, ["identity", "runId", "runAttempt"], "qualification workflow");
  if (
    !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/[.]github\/workflows\/devtools-image-qualify[.]yml@refs\/heads\/[A-Za-z0-9._/-]+$/.test(
      record.value.workflow.identity ?? "",
    ) ||
    !/^[1-9][0-9]*$/.test(record.value.workflow.runId ?? "") ||
    !/^[1-9][0-9]*$/.test(record.value.workflow.runAttempt ?? "")
  ) {
    fail("qualification workflow identity is invalid");
  }
  assertPublicReceipt(record.value);
  process.stdout.write(
    `${canonicalJSON({
      schema: record.value.schema,
      receiptDigest: digestBytes(record.bytes),
      identityDigest: record.value.pool?.identityDigest,
    })}\n`,
  );
}

async function inspectInput(options) {
  const record = await readJSON(required(options, "file"), "qualification input");
  process.stdout.write(`${canonicalJSON(inputSummary(record.value))}\n`);
}

async function mutateIdentity(options) {
  const identity = (await readJSON(required(options, "identity"), "ready-pool identity")).value;
  const dimension = required(options, "dimension");
  if (dimension === "ami") identity.imageID = "ami-00000000000000000";
  else if (dimension === "architecture") {
    identity.architecture = identity.architecture === "amd64" ? "arm64" : "amd64";
  } else if (dimension === "recipe") {
    identity.recipeDigest = `sha256:${"0".repeat(64)}`;
  } else fail("--dimension must be ami, architecture, or recipe");
  await writeAtomic(required(options, "output"), identity);
}

async function main() {
  const [command, ...argv] = process.argv.slice(2);
  const options = parseOptions(argv);
  if (command === "build") return build(options);
  if (command === "verify") return verify(options);
  if (command === "inspect-input") return inspectInput(options);
  if (command === "mutate-identity") return mutateIdentity(options);
  fail("usage: aws-image-qualification.mjs build|verify|inspect-input|mutate-identity [options]");
}

if (process.argv[1] && path.resolve(process.argv[1]) === path.resolve(fileURLToPath(import.meta.url))) {
  main().catch((error) => {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  });
}
