#!/usr/bin/env node

import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import { realpathSync } from "node:fs";
import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { loadDevtoolsRecipe } from "./devtools-image-contract.mjs";
import { canonicalJSON, parseStrictJSON } from "./generate-linux-readiness.mjs";

const scriptPath = fileURLToPath(import.meta.url);
const root = resolve(dirname(scriptPath), "..");
const phases = ["baseline", "candidate", "promoted"];
const stages = [
  "preflight",
  "baseline",
  "source_prepare",
  "candidate_create",
  "candidate_smoke",
  "candidate_measure",
  "promotion",
  "promoted_smoke",
  "promoted_measure",
  "proof",
  "complete",
];
const outcomeStatuses = ["initialized", "failed", "passed"];
const rollbackStatuses = ["not_required", "succeeded", "failed"];
const cleanupStatuses = ["not_started", "succeeded", "failed"];
const policyInputKeys = [
  "sourceRevision",
  "recipeDigest",
  "region",
  "machineType",
  "serverClass",
  "maxP95RunnerTotalMs",
  "ttl",
  "idleTimeout",
];
const policyKeys = [
  ...policyInputKeys,
  "commandFingerprint",
  "syncPolicy",
  "capabilities",
  "samplesPerCohort",
  "plannedLeaseCount",
];
const cohortKeys = [
  "phase",
  "observations",
  "successfulSamples",
  "runnerTotalN",
  "p95RunnerTotalMs",
  "medianRunnerTotalMs",
  "medianSyncMs",
  "policyApplied",
  "policyPassed",
];
const comparisonKeys = [
  "kind",
  "baselineP95RunnerTotalMs",
  "candidateP95RunnerTotalMs",
  "promotedP95RunnerTotalMs",
];
const outcomeKeys = [
  "schema",
  "status",
  "stage",
  "sourceRevision",
  "recipeDigest",
  "policyDigest",
  "plannedLeaseCount",
  "maxP95RunnerTotalMs",
  "promotionBindingDigest",
  "cohorts",
  "comparison",
  "candidateSelection",
  "promotedSelection",
  "rollbackStatus",
  "cleanupStatus",
  "exitCode",
];

const digest = (value) =>
  `sha256:${createHash("sha256").update(canonicalJSON(value)).digest("hex")}`;
const isDigest = (value) => typeof value === "string" && /^sha256:[0-9a-f]{64}$/.test(value);
const integer = (value, minimum = 0) =>
  assert.ok(Number.isSafeInteger(value) && value >= minimum, "invalid numeric evidence");
const nullableInteger = (value, minimum = 0) => {
  if (value !== null) integer(value, minimum);
};

function exactKeys(value, keys, label) {
  assert.ok(value && typeof value === "object" && !Array.isArray(value), `${label} must be an object`);
  assert.deepEqual(Object.keys(value).sort(), [...keys].sort(), `${label} keys changed`);
}

export function measurementPolicy(input) {
  const isPolicy = Object.hasOwn(input, "commandFingerprint");
  exactKeys(input, isPolicy ? policyKeys : policyInputKeys, "measurement policy");
  assert.match(input.sourceRevision, /^[0-9a-f]{40}$/);
  assert.ok(isDigest(input.recipeDigest), "invalid recipe digest");
  assert.ok(
    Number.isSafeInteger(input.maxP95RunnerTotalMs) &&
      input.maxP95RunnerTotalMs > 0 &&
      input.maxP95RunnerTotalMs <= 9_223_372_036_854,
    "an explicit positive integer millisecond threshold is required",
  );
  for (const key of ["region", "machineType", "serverClass", "ttl", "idleTimeout"]) {
    assert.ok(typeof input[key] === "string" && input[key].length > 0, `missing policy ${key}`);
  }
  const policy = {
    sourceRevision: input.sourceRevision,
    recipeDigest: input.recipeDigest,
    region: input.region,
    machineType: input.machineType,
    serverClass: input.serverClass,
    maxP95RunnerTotalMs: input.maxP95RunnerTotalMs,
    ttl: input.ttl,
    idleTimeout: input.idleTimeout,
    commandFingerprint: digest(["true"]),
    syncPolicy: "full-resync-no-hydrate",
    capabilities: ["browser", "desktop"],
    samplesPerCohort: 3,
    plannedLeaseCount: 12,
  };
  if (isPolicy) assert.deepEqual(input, policy, "measurement policy invariants changed");
  return policy;
}

function durationString(milliseconds) {
  if (milliseconds < 1000) return `${milliseconds}ms`;
  const hours = Math.floor(milliseconds / 3_600_000);
  const minutes = Math.floor((milliseconds % 3_600_000) / 60_000);
  const seconds = (milliseconds % 60_000) / 1000;
  return `${hours ? `${hours}h` : ""}${hours || minutes ? `${minutes}m` : ""}${seconds}s`;
}

function cohortIdentity(record) {
  return canonicalJSON({
    repoFingerprint: record.benchmark.repoFingerprint,
    repoHead: record.benchmark.repoHead,
    commandFingerprint: record.benchmark.commandFingerprint,
    machineType: record.timing.machineType,
    syncMode: record.timing.syncMode ?? "",
  });
}

export function observedSelection(leases, leaseId) {
  assert.ok(Array.isArray(leases) && leases.length <= 100, "invalid bounded lease listing");
  const matches = leases.filter((lease) => lease.id === leaseId);
  assert.equal(matches.length, 1, "missing or ambiguous exact-lease observation");
  const lease = matches[0];
  assert.ok(lease.image, "missing recorded image selection");
  for (const key of ["id", "provider", "target", "region", "serverType", "cloudID"]) {
    assert.ok(typeof lease[key] === "string" && lease[key].length > 0, `missing observed ${key}`);
  }
  for (const key of ["id", "source", "kind", "region"]) {
    assert.ok(
      typeof lease.image[key] === "string" && lease.image[key].length > 0,
      `missing observed image ${key}`,
    );
  }
  return {
    id: lease.id,
    provider: lease.provider,
    target: lease.target,
    region: lease.region,
    serverType: lease.serverType,
    cloudID: lease.cloudID,
    image: {
      id: lease.image.id,
      source: lease.image.source,
      kind: lease.image.kind,
      region: lease.image.region,
      promotedAt: lease.image.promotedAt ?? "",
    },
  };
}

function validateSelection(policy, phase, selection, candidateImage, baseline) {
  assert.equal(selection.provider, "aws");
  assert.equal(selection.target, "linux");
  assert.equal(selection.serverType, policy.machineType);
  assert.equal(selection.region, policy.region, "observed region differs from requested region");
  assert.equal(
    selection.image.region,
    policy.region,
    "observed image region differs from requested region",
  );
  assert.equal(selection.image.kind, "aws-ami");
  assert.match(selection.image.id, /^ami-[a-z0-9]+$/);
  if (phase === "baseline") {
    assert.ok(
      ["stock", "promoted"].includes(selection.image.source),
      "baseline must use normal image selection",
    );
    assert.deepEqual(selection.image, baseline.image, "mixed baseline image selection");
  } else {
    assert.equal(selection.image.id, candidateImage, "selected image differs from captured candidate");
    assert.equal(
      selection.image.source,
      phase === "candidate" ? "explicit" : "promoted",
      "image selection source changed",
    );
  }
}

export function validatePromotionReceipt(policyInput, baseline, receipt, candidateImage) {
  const policy = measurementPolicy(policyInput);
  validateSelection(policy, "baseline", baseline, candidateImage, baseline);
  assert.equal(receipt.image.id, candidateImage, "receipt differs from captured candidate");
  assert.equal(receipt.image.region, policy.region, "receipt image region changed");
  assert.ok(
    typeof receipt.image.revision === "string" && receipt.image.revision.length > 0,
    "missing promoted image revision",
  );
  if (baseline.image.source === "stock") {
    assert.equal(
      receipt.previous.state,
      "absent",
      "baseline differs from the captured previous default",
    );
  } else {
    assert.equal(
      receipt.previous.state,
      "present",
      "baseline differs from the captured previous default",
    );
    assert.equal(
      receipt.previous.imageId,
      baseline.image.id,
      "baseline differs from the captured previous default",
    );
    assert.ok(
      receipt.previous.aliases.some(
        ({ state, image }) =>
          state === "present" &&
          image.id === baseline.image.id &&
          image.region === baseline.image.region &&
          (!baseline.image.promotedAt || image.promotedAt === baseline.image.promotedAt),
      ),
      "baseline revision differs from the captured previous default",
    );
  }
}

function validateRecordIdentity(policy, record) {
  assert.equal(record.schemaVersion, 1);
  assert.equal(record.source, "run");
  assert.equal(record.benchmark.repoHead, policy.sourceRevision, "source identity changed");
  assert.equal(
    record.benchmark.commandFingerprint,
    policy.commandFingerprint,
    "command identity changed",
  );
  assert.ok(isDigest(record.benchmark.repoFingerprint), "missing repository identity");
  assert.equal(record.benchmark.coldRun, true, "measurement must acquire a fresh lease");
  assert.equal(record.timing.provider, "aws");
  assert.equal(record.timing.machineType, policy.machineType, "machine identity changed");
  assert.match(record.timing.leaseId, /^cbx_[0-9a-f]{12}$/, "missing measurement lease");
  assert.ok(
    typeof record.timing.runId === "string" && record.timing.runId.length > 0,
    "missing run identity",
  );
  assert.ok(Number.isSafeInteger(record.timing.exitCode), "missing run exit code");
  assert.equal(record.timing.syncSkipped, false, "measurement sync was skipped");
  if (record.timing.runnerTotalMs !== undefined) integer(record.timing.runnerTotalMs, 1);
  if (record.timing.syncMs !== undefined) integer(record.timing.syncMs);
}

export function validateCohort(
  policyInput,
  phase,
  { records, report, check, cleanupIds, selections, handles },
  priorCohorts = [],
  candidateImage,
) {
  const policy = measurementPolicy(policyInput);
  assert.ok(phases.includes(phase), "invalid cohort phase");
  assert.equal(records.length, policy.samplesPerCohort, "insufficient cohort samples");
  assert.equal(selections.length, records.length, "missing sample image observations");
  assert.equal(handles.length, records.length, "missing sample lease handles");
  for (const selection of selections) {
    validateSelection(policy, phase, selection, candidateImage, selections[0]);
  }
  assert.deepEqual(
    cleanupIds,
    records.map((record) => record.timing.leaseId),
    "measurement cleanup was not confirmed",
  );
  const priorRecords = priorCohorts.flatMap((cohort) => cohort.records);
  const identity = cohortIdentity(priorRecords[0] ?? records[0]);
  const leases = new Set(priorRecords.map((record) => record.timing.leaseId));
  const clouds = new Set(
    priorCohorts.flatMap((cohort) => cohort.selections.map((lease) => lease.cloudID)),
  );
  const runs = new Set(priorRecords.map((record) => record.timing.runId));
  for (const [index, record] of records.entries()) {
    const handle = handles[index];
    const selection = selections[index];
    validateRecordIdentity(policy, record);
    assert.equal(handle.provider, "aws");
    assert.equal(handle.kept, true);
    assert.equal(handle.reused, false, "measurement reused a lease");
    assert.equal(handle.leaseId, record.timing.leaseId, "handle/timing lease mismatch");
    assert.ok(
      typeof handle.runId === "string" && handle.runId.length > 0,
      "missing handle run identity",
    );
    assert.equal(handle.runId, record.timing.runId, "handle/timing run mismatch");
    assert.equal(selection.id, handle.leaseId, "selection/handle lease mismatch");
    assert.match(selection.cloudID, /^i-[0-9a-f]{8,17}$/, "missing observed instance identity");
    assert.ok(!clouds.has(selection.cloudID), "reused provider instance");
    assert.ok(!runs.has(handle.runId), "reused run identity");
    assert.ok(!leases.has(record.timing.leaseId), "reused measurement lease");
    clouds.add(selection.cloudID);
    runs.add(handle.runId);
    leases.add(record.timing.leaseId);
    assert.equal(cohortIdentity(record), identity, "mixed cohort identity");
    assert.equal(record.timing.exitCode, 0);
    integer(record.timing.runnerTotalMs, 1);
    integer(record.timing.syncMs);
  }
  assert.equal(report.schemaVersion, 1);
  assert.equal(report.observationCount, 3);
  assert.equal(report.matchedCount, 3);
  assert.equal(report.groups.length, 1, "mixed benchmark groups");
  const group = report.groups[0];
  assert.equal(group.source, "run");
  assert.equal(group.provider, "aws");
  assert.equal(group.machineType, policy.machineType);
  assert.equal(group.commandFingerprint, policy.commandFingerprint);
  assert.equal(group.coldRun, true);
  for (const key of ["n", "observationCount", "runnerTotalN"]) assert.equal(group[key], 3);
  assert.equal(group.failureCount, 0);
  integer(group.p95RunnerTotalMs, 1);
  integer(group.medianRunnerTotalMs, 1);
  integer(group.medianSyncMs);
  if (phase === "baseline") {
    assert.equal(check, undefined, "baseline is evidence-only");
  } else {
    assert.equal(check.schemaVersion, 1);
    assert.equal(check.matchedCount, 3);
    assert.equal(check.groupCount, 1);
    assert.equal(check.groups.length, 1);
    assert.equal(check.passed, true, "benchmark policy failed");
    assert.deepEqual(check.reasons, []);
    assert.equal(check.policy.minSamples, 3);
    assert.equal(check.policy.requiredRunnerTotalSamples, 3);
    assert.equal(check.policy.maxFailures, 0);
    assert.equal(
      check.policy.maxP95RunnerTotal,
      durationString(policy.maxP95RunnerTotalMs),
      "benchmark threshold differs from the predeclared policy",
    );
    const checked = check.groups[0];
    for (const key of [
      "source",
      "provider",
      "machineType",
      "coldRun",
      "failureCount",
      "runnerTotalN",
      "observationCount",
      "p95RunnerTotalMs",
    ]) {
      assert.equal(checked[key], group[key], "benchmark check/report mismatch");
    }
    assert.equal(checked.successfulSamples, 3);
    assert.equal(checked.passed, true);
    assert.deepEqual(checked.reasons, []);
  }
  return validateCohortSummary({
    phase,
    observations: 3,
    successfulSamples: 3,
    runnerTotalN: 3,
    p95RunnerTotalMs: group.p95RunnerTotalMs,
    medianRunnerTotalMs: group.medianRunnerTotalMs,
    medianSyncMs: group.medianSyncMs,
    policyApplied: phase !== "baseline",
    policyPassed: phase === "baseline" ? null : true,
  });
}

export function partialCohort(policyInput, phase, records) {
  const policy = measurementPolicy(policyInput);
  assert.ok(phases.includes(phase), "invalid cohort phase");
  assert.ok(Array.isArray(records) && records.length <= policy.samplesPerCohort);
  const leases = new Set();
  const runs = new Set();
  let identity = "";
  for (const record of records) {
    validateRecordIdentity(policy, record);
    const currentIdentity = cohortIdentity(record);
    if (!identity) identity = currentIdentity;
    assert.equal(currentIdentity, identity, "mixed cohort identity");
    assert.ok(!leases.has(record.timing.leaseId), "reused measurement lease");
    assert.ok(!runs.has(record.timing.runId), "reused run identity");
    leases.add(record.timing.leaseId);
    runs.add(record.timing.runId);
  }
  const successful = records.filter((record) => record.timing.exitCode === 0);
  return validateCohortSummary({
    phase,
    observations: records.length,
    successfulSamples: successful.length,
    runnerTotalN: successful.filter((record) => Number.isSafeInteger(record.timing.runnerTotalMs))
      .length,
    p95RunnerTotalMs: null,
    medianRunnerTotalMs: null,
    medianSyncMs: null,
    policyApplied: phase !== "baseline",
    policyPassed: phase === "baseline" ? null : false,
  });
}

export function validateCohortSummary(cohort) {
  exactKeys(cohort, cohortKeys, "public cohort");
  assert.ok(phases.includes(cohort.phase), "invalid cohort phase");
  for (const key of ["observations", "successfulSamples", "runnerTotalN"]) integer(cohort[key]);
  assert.ok(cohort.observations <= 3, "too many cohort observations");
  assert.ok(cohort.successfulSamples <= cohort.observations, "invalid successful sample count");
  assert.ok(cohort.runnerTotalN <= cohort.successfulSamples, "invalid runner sample count");
  for (const key of ["p95RunnerTotalMs", "medianRunnerTotalMs", "medianSyncMs"])
    nullableInteger(cohort[key]);
  assert.equal(cohort.policyApplied, cohort.phase !== "baseline", "cohort policy scope changed");
  if (cohort.phase === "baseline") {
    assert.equal(cohort.policyPassed, null, "baseline is evidence-only");
  } else {
    assert.equal(typeof cohort.policyPassed, "boolean");
  }
  if (cohort.policyPassed === true) {
    assert.equal(cohort.observations, 3);
    assert.equal(cohort.successfulSamples, 3);
    assert.equal(cohort.runnerTotalN, 3);
    integer(cohort.p95RunnerTotalMs, 1);
  }
  const measures = [
    cohort.p95RunnerTotalMs,
    cohort.medianRunnerTotalMs,
    cohort.medianSyncMs,
  ];
  if (measures.some((value) => value !== null)) {
    assert.ok(measures.every((value) => value !== null), "incomplete cohort measures");
    assert.equal(cohort.observations, 3);
    assert.equal(cohort.successfulSamples, 3);
    assert.equal(cohort.runnerTotalN, 3);
  }
  if (cohort.policyPassed === false)
    assert.ok(measures.every((value) => value === null), "failed policy exposed final measures");
  return cohort;
}

function promotionBinding(receipt) {
  if (!receipt) return null;
  assert.match(receipt.image?.id, /^ami-[a-z0-9]+$/, "invalid promoted image identity");
  assert.ok(
    typeof receipt.image?.revision === "string" && receipt.image.revision.length > 0,
    "missing promoted image revision",
  );
  return digest(["aws-image-promotion", receipt.image.id, receipt.image.revision]);
}

export function projectOutcome(policyInput, state = {}) {
  const policy = measurementPolicy(policyInput);
  const {
    status = "initialized",
    stage = "preflight",
    exitCode = null,
    rollbackStatus = "not_required",
    cleanupStatus = "not_started",
    cohorts = [],
    promotionReceipt = null,
  } = state;
  assert.ok(outcomeStatuses.includes(status), "invalid outcome status");
  assert.ok(stages.includes(stage), "invalid outcome stage");
  assert.ok(rollbackStatuses.includes(rollbackStatus), "invalid rollback status");
  assert.ok(cleanupStatuses.includes(cleanupStatus), "invalid cleanup status");
  if (exitCode !== null) {
    integer(exitCode);
    assert.ok(exitCode <= 255, "invalid outcome exit code");
  }
  assert.deepEqual(
    cohorts.map((cohort) => validateCohortSummary(cohort).phase),
    phases.slice(0, cohorts.length),
    "cohorts must be ordered without gaps",
  );
  const byPhase = Object.fromEntries(cohorts.map((cohort) => [cohort.phase, cohort]));
  const outcome = {
    schema: "crabbox-devtools-image-proof/v2",
    status,
    stage,
    sourceRevision: policy.sourceRevision,
    recipeDigest: policy.recipeDigest,
    policyDigest: digest(policy),
    plannedLeaseCount: policy.plannedLeaseCount,
    maxP95RunnerTotalMs: policy.maxP95RunnerTotalMs,
    promotionBindingDigest: promotionBinding(promotionReceipt),
    cohorts,
    comparison: {
      kind: "descriptive_only",
      baselineP95RunnerTotalMs: byPhase.baseline?.p95RunnerTotalMs ?? null,
      candidateP95RunnerTotalMs: byPhase.candidate?.p95RunnerTotalMs ?? null,
      promotedP95RunnerTotalMs: byPhase.promoted?.p95RunnerTotalMs ?? null,
    },
    candidateSelection: byPhase.candidate?.policyPassed === true ? "explicit" : "not_observed",
    promotedSelection: byPhase.promoted?.policyPassed === true ? "promoted" : "not_observed",
    rollbackStatus,
    cleanupStatus,
    exitCode,
  };
  return validateOutcome(outcome);
}

export function projectManifest(policy, cohorts, promotionReceipt) {
  return projectOutcome(policy, {
    status: "passed",
    stage: "complete",
    exitCode: 0,
    rollbackStatus: "not_required",
    cleanupStatus: "succeeded",
    cohorts,
    promotionReceipt,
  });
}

export function validateOutcome(outcome) {
  exactKeys(outcome, outcomeKeys, "public outcome");
  assert.equal(outcome.schema, "crabbox-devtools-image-proof/v2");
  assert.ok(outcomeStatuses.includes(outcome.status), "invalid outcome status");
  assert.ok(stages.includes(outcome.stage), "invalid outcome stage");
  assert.match(outcome.sourceRevision, /^[0-9a-f]{40}$/);
  for (const key of ["recipeDigest", "policyDigest"]) assert.ok(isDigest(outcome[key]));
  if (outcome.promotionBindingDigest !== null)
    assert.ok(isDigest(outcome.promotionBindingDigest), "invalid promotion binding digest");
  assert.equal(outcome.plannedLeaseCount, 12);
  integer(outcome.maxP95RunnerTotalMs, 1);
  assert.ok(Array.isArray(outcome.cohorts) && outcome.cohorts.length <= 3);
  assert.deepEqual(
    outcome.cohorts.map((cohort) => validateCohortSummary(cohort).phase),
    phases.slice(0, outcome.cohorts.length),
    "cohorts must be ordered without gaps",
  );
  exactKeys(outcome.comparison, comparisonKeys, "public comparison");
  assert.equal(outcome.comparison.kind, "descriptive_only");
  for (const key of comparisonKeys.slice(1)) nullableInteger(outcome.comparison[key]);
  assert.ok(["not_observed", "explicit"].includes(outcome.candidateSelection));
  assert.ok(["not_observed", "promoted"].includes(outcome.promotedSelection));
  const byPhase = Object.fromEntries(outcome.cohorts.map((cohort) => [cohort.phase, cohort]));
  assert.deepEqual(outcome.comparison, {
    kind: "descriptive_only",
    baselineP95RunnerTotalMs: byPhase.baseline?.p95RunnerTotalMs ?? null,
    candidateP95RunnerTotalMs: byPhase.candidate?.p95RunnerTotalMs ?? null,
    promotedP95RunnerTotalMs: byPhase.promoted?.p95RunnerTotalMs ?? null,
  });
  assert.equal(
    outcome.candidateSelection,
    byPhase.candidate?.policyPassed === true ? "explicit" : "not_observed",
  );
  assert.equal(
    outcome.promotedSelection,
    byPhase.promoted?.policyPassed === true ? "promoted" : "not_observed",
  );
  assert.ok(rollbackStatuses.includes(outcome.rollbackStatus));
  assert.ok(cleanupStatuses.includes(outcome.cleanupStatus));
  const cohortComplete = (cohort) =>
    cohort.observations === 3 &&
    cohort.successfulSamples === 3 &&
    cohort.runnerTotalN === 3 &&
    cohort.p95RunnerTotalMs !== null &&
    cohort.medianRunnerTotalMs !== null &&
    cohort.medianSyncMs !== null &&
    (cohort.phase === "baseline" || cohort.policyPassed === true);
  const stageIndex = stages.indexOf(outcome.stage);
  const minimumCohorts =
    stageIndex >= stages.indexOf("proof")
      ? 3
      : stageIndex >= stages.indexOf("promotion")
        ? 2
        : stageIndex >= stages.indexOf("source_prepare")
          ? 1
          : 0;
  const maximumCohorts =
    stageIndex >= stages.indexOf("promoted_measure")
      ? 3
      : stageIndex >= stages.indexOf("candidate_measure")
        ? 2
        : stageIndex >= stages.indexOf("baseline")
          ? 1
          : 0;
  assert.ok(
    outcome.cohorts.length >= minimumCohorts && outcome.cohorts.length <= maximumCohorts,
    "cohort evidence conflicts with publication stage",
  );
  assert.ok(
    outcome.cohorts.slice(0, minimumCohorts).every(cohortComplete),
    "publication stage requires complete prior cohorts",
  );
  if (stageIndex < stages.indexOf("promotion"))
    assert.equal(outcome.promotionBindingDigest, null, "promotion preceded its stage");
  if (stageIndex >= stages.indexOf("promoted_smoke"))
    assert.ok(isDigest(outcome.promotionBindingDigest), "missing promotion binding");
  if (stageIndex < stages.indexOf("promotion"))
    assert.equal(outcome.rollbackStatus, "not_required", "rollback preceded promotion");
  if (outcome.status === "initialized") {
    assert.equal(outcome.stage, "preflight");
    assert.equal(outcome.exitCode, null);
    assert.equal(outcome.cleanupStatus, "not_started");
    assert.equal(outcome.rollbackStatus, "not_required");
    assert.deepEqual(outcome.cohorts, []);
  } else {
    integer(outcome.exitCode, outcome.status === "failed" ? 1 : 0);
    assert.notEqual(outcome.cleanupStatus, "not_started");
  }
  if (outcome.status === "passed") {
    assert.equal(outcome.stage, "complete");
    assert.equal(outcome.exitCode, 0);
    assert.equal(outcome.cleanupStatus, "succeeded");
    assert.equal(outcome.rollbackStatus, "not_required");
    assert.ok(isDigest(outcome.promotionBindingDigest), "missing promotion binding");
    assert.equal(outcome.cohorts.length, 3);
    assert.equal(outcome.cohorts[1].policyPassed, true);
    assert.equal(outcome.cohorts[2].policyPassed, true);
    assert.equal(outcome.candidateSelection, "explicit");
    assert.equal(outcome.promotedSelection, "promoted");
  }
  if (outcome.rollbackStatus !== "not_required")
    assert.ok(isDigest(outcome.promotionBindingDigest), "rollback is not bound to a promotion");
  return outcome;
}

async function writeAtomic(path, value) {
  await mkdir(dirname(path), { recursive: true });
  const pending = `${path}.tmp-${process.pid}-${randomBytes(6).toString("hex")}`;
  await writeFile(pending, `${canonicalJSON(value)}\n`, { mode: 0o600 });
  await rename(pending, path);
}

async function preflight(args) {
  const { digest: recipeDigest, recipe } = await loadDevtoolsRecipe(root);
  const [
    prep,
    region,
    machineType,
    serverClass,
    threshold,
    desktop,
    browser,
    promote,
    keep,
    fsr,
    ttl,
    idleTimeout,
  ] = args;
  assert.equal(args.length, 12);
  assert.equal(
    realpathSync(prep),
    resolve(root, recipe.execution.arguments[0]),
    "measured prep must use the bundled recipe",
  );
  assert.deepEqual(
    [desktop, browser, promote, keep, fsr],
    ["1", "1", "1", "0", "0"],
    "measured mode requires desktop/browser, promotion, cleanup, and no FSR",
  );
  assert.ok(
    !Object.keys(process.env).some((key) => key.startsWith("CRABBOX_LINUX_")),
    "measured recipe does not permit Linux installer overrides",
  );
  assert.ok(
    process.env.CRABBOX_OWNER?.trim() && process.env.CRABBOX_ORG?.trim(),
    "measured mode requires CRABBOX_OWNER and CRABBOX_ORG for bounded lease evidence",
  );
  const git = (gitArgs) =>
    execFileSync("git", ["-C", root, ...gitArgs], { encoding: "utf8" }).trim();
  const sourceRevision = git(["rev-parse", "HEAD"]);
  assert.equal(
    git(["status", "--porcelain", "--untracked-files=all"]),
    "",
    "measured source must be clean",
  );
  return measurementPolicy({
    sourceRevision,
    recipeDigest,
    region,
    machineType,
    serverClass,
    maxP95RunnerTotalMs: Number(threshold),
    ttl,
    idleTimeout,
  });
}

const json = async (path) => parseStrictJSON(await readFile(path, "utf8"), "measurement evidence");
function handleLeaseId(handle) {
  assert.equal(handle.provider, "aws");
  assert.match(handle.leaseId, /^cbx_[0-9a-f]{12}$/, "missing measurement lease");
  return handle.leaseId;
}
const recordsFrom = async (path) =>
  (await readFile(path, "utf8"))
    .trim()
    .split("\n")
    .filter(Boolean)
    .map((line) => parseStrictJSON(line, "measurement record"));
async function loadCohort(directory, phase) {
  const records = await recordsFrom(resolve(directory, `${phase}.jsonl`));
  const cleanupIds = (await readFile(resolve(directory, `${phase}-cleanup.ids`), "utf8"))
    .trim()
    .split("\n")
    .filter(Boolean);
  const result = {
    records,
    cleanupIds,
    handles: await Promise.all(
      records.map((_, index) => json(resolve(directory, `${phase}-${index + 1}.session.json`))),
    ),
    selections: await Promise.all(
      records.map((_, index) => json(resolve(directory, `${phase}-${index + 1}.selection.json`))),
    ),
    report: await json(resolve(directory, `${phase}-report.json`)),
  };
  if (phase !== "baseline") result.check = await json(resolve(directory, `${phase}-check.json`));
  return result;
}

async function loadProjectedCohorts(directory) {
  if (!directory || directory === "-") return [];
  const cohorts = [];
  for (const phase of phases) {
    try {
      cohorts.push(validateCohortSummary(await json(resolve(directory, `${phase}-cohort.json`))));
    } catch (error) {
      if (error.code === "ENOENT") break;
      throw error;
    }
  }
  return cohorts;
}

async function readStdin() {
  let input = "";
  process.stdin.setEncoding("utf8");
  for await (const chunk of process.stdin) input += chunk;
  return input;
}

async function main([command, ...args]) {
  if (command === "preflight") return preflight(args);
  if (command === "initialize") {
    const [output, ...preflightArgs] = args;
    const policy = await preflight(preflightArgs);
    await writeAtomic(output, projectOutcome(policy));
    return;
  }
  if (command === "initialize-policy") {
    const [output, policyPath] = args;
    await writeAtomic(output, projectOutcome(await json(policyPath)));
    return;
  }
  if (command === "select") {
    return observedSelection(parseStrictJSON(await readStdin(), "lease listing"), args[0]);
  }
  if (command === "handle") return { leaseId: handleLeaseId(await json(args[0])) };
  if (command === "sample") {
    const [store, expectedCount, handlePath] = args;
    let handle;
    try {
      handle = await json(handlePath);
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
    }
    let leaseId;
    if (handle) leaseId = handleLeaseId(handle);
    else {
      const records = await recordsFrom(store);
      assert.equal(records.length, Number(expectedCount), "missing measurement record");
      leaseId = records.at(-1).timing.leaseId;
    }
    assert.match(leaseId, /^cbx_[0-9a-f]{12}$/, "missing measurement lease");
    return { leaseId };
  }
  if (command === "selection") {
    const [policyPath, selectionPath, phase, candidateImage, baselinePath] = args;
    assert.ok(phases.includes(phase), "invalid cohort phase");
    validateSelection(
      measurementPolicy(await json(policyPath)),
      phase,
      await json(selectionPath),
      candidateImage,
      phase === "baseline" ? await json(baselinePath) : undefined,
    );
    return { status: "passed" };
  }
  if (command === "receipt") {
    const [policyPath, baselinePath, receiptPath, candidateImage] = args;
    validatePromotionReceipt(
      await json(policyPath),
      await json(baselinePath),
      await json(receiptPath),
      candidateImage,
    );
    return { status: "passed" };
  }
  if (command === "partial") {
    const [policyPath, store, phase, output] = args;
    const summary = partialCohort(await json(policyPath), phase, await recordsFrom(store));
    await writeAtomic(output, summary);
    return;
  }
  if (command === "cohort") {
    const [policyPath, directory, phase, candidateImage, output] = args;
    const policy = measurementPolicy(await json(policyPath));
    const prior = [];
    for (const current of phases.slice(0, phases.indexOf(phase))) {
      prior.push(await loadCohort(directory, current));
    }
    const summary = validateCohort(
      policy,
      phase,
      await loadCohort(directory, phase),
      prior,
      candidateImage,
    );
    if (output) await writeAtomic(output, summary);
    return summary;
  }
  if (command === "finalize") {
    const [
      output,
      policyPath,
      directory,
      stage,
      exitCode,
      rollbackStatus,
      cleanupStatus,
      receiptPath,
    ] = args;
    const code = Number(exitCode);
    const outcome = projectOutcome(await json(policyPath), {
      status: code === 0 ? "passed" : "failed",
      stage,
      exitCode: code,
      rollbackStatus,
      cleanupStatus,
      cohorts: await loadProjectedCohorts(directory),
      promotionReceipt: receiptPath && receiptPath !== "-" ? await json(receiptPath) : null,
    });
    await writeAtomic(output, outcome);
    return;
  }
  if (command === "validate") return validateOutcome(await json(args[0]));
  assert.fail("unknown proof command");
}

if (process.argv[1] && realpathSync(process.argv[1]) === realpathSync(scriptPath)) {
  const result = await main(process.argv.slice(2));
  if (result !== undefined) process.stdout.write(`${canonicalJSON(result)}\n`);
}
