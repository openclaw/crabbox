#!/usr/bin/env node

import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync, realpathSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { loadDevtoolsRecipe } from "./devtools-image-contract.mjs";
import { canonicalJSON, parseStrictJSON } from "./generate-linux-readiness.mjs";

const scriptPath = fileURLToPath(import.meta.url);
const root = resolve(dirname(scriptPath), "..");
const phases = ["baseline", "candidate", "promoted"];
const digest = (value) =>
  `sha256:${createHash("sha256").update(canonicalJSON(value)).digest("hex")}`;
const isDigest = (value) => typeof value === "string" && /^sha256:[0-9a-f]{64}$/.test(value);
const number = (value, minimum = 0) =>
  assert.ok(Number.isSafeInteger(value) && value >= minimum, "invalid numeric evidence");

export function measurementPolicy(input) {
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
  return {
    sourceRevision: input.sourceRevision,
    recipeDigest: input.recipeDigest,
    region: input.region,
    machineType: input.machineType,
    serverClass: input.serverClass,
    ttl: input.ttl,
    idleTimeout: input.idleTimeout,
    maxP95RunnerTotalMs: input.maxP95RunnerTotalMs,
    commandFingerprint: digest(["true"]),
    syncPolicy: "full-resync-no-hydrate",
    capabilities: ["browser", "desktop"],
    samplesPerCohort: 3,
    plannedLeaseCount: 12,
  };
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
  // Discard other leases, owners, hosts, and provider metadata before persisting private evidence.
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
    assert.equal(
      selection.image.id,
      candidateImage,
      "selected image differs from captured candidate",
    );
    assert.equal(
      selection.image.source,
      phase === "candidate" ? "explicit" : "promoted",
      "image selection source changed",
    );
  }
}

export function validatePromotionReceipt(policy, baseline, receipt, candidateImage) {
  validateSelection(policy, "baseline", baseline, candidateImage, baseline);
  assert.equal(receipt.image.id, candidateImage, "receipt differs from captured candidate");
  assert.equal(receipt.image.region, policy.region, "receipt image region changed");
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

export function validateCohort(
  policy,
  phase,
  { records, report, check, cleanupIds, selections, handles },
  priorCohorts = [],
  candidateImage,
) {
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
    clouds.add(selection.cloudID);
    runs.add(handle.runId);
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
    assert.equal(cohortIdentity(record), identity, "mixed cohort identity");
    assert.match(record.timing.leaseId, /^cbx_[0-9a-f]{12}$/, "missing measurement lease");
    assert.ok(!leases.has(record.timing.leaseId), "reused measurement lease");
    leases.add(record.timing.leaseId);
    assert.equal(record.timing.exitCode, 0);
    assert.equal(record.timing.syncSkipped, false, "measurement sync was skipped");
    number(record.timing.runnerTotalMs, 1);
    number(record.timing.syncMs);
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
  number(group.p95RunnerTotalMs, 1);
  number(group.medianRunnerTotalMs, 1);
  number(group.medianSyncMs);
  return {
    phase,
    status: "passed",
    observations: 3,
    successfulSamples: 3,
    runnerTotalN: 3,
    p95RunnerTotalMs: group.p95RunnerTotalMs,
    medianRunnerTotalMs: group.medianRunnerTotalMs,
    medianSyncMs: group.medianSyncMs,
    checkReasons: [],
  };
}

export function projectManifest(input, cohorts) {
  const policy = measurementPolicy(input);
  assert.deepEqual(
    cohorts.map((cohort) => cohort.phase),
    phases,
  );
  // Never spread reports, receipts, phase labels, paths, or provider metadata into public proof.
  const projected = cohorts.map((cohort, index) => {
    assert.equal(cohort.status, "passed");
    assert.deepEqual(cohort.checkReasons, []);
    for (const key of ["observations", "successfulSamples", "runnerTotalN"])
      assert.equal(cohort[key], 3);
    for (const key of ["p95RunnerTotalMs", "medianRunnerTotalMs", "medianSyncMs"])
      number(cohort[key]);
    return {
      phase: phases[index],
      status: "passed",
      observations: 3,
      successfulSamples: 3,
      runnerTotalN: 3,
      p95RunnerTotalMs: cohort.p95RunnerTotalMs,
      medianRunnerTotalMs: cohort.medianRunnerTotalMs,
      medianSyncMs: cohort.medianSyncMs,
      checkReasons: [],
    };
  });
  return {
    schema: "crabbox-devtools-image-proof/v1",
    status: "passed",
    sourceRevision: policy.sourceRevision,
    recipeDigest: policy.recipeDigest,
    policyDigest: digest(policy),
    plannedLeaseCount: 12,
    maxP95RunnerTotalMs: policy.maxP95RunnerTotalMs,
    cohorts: projected,
    comparison: {
      kind: "descriptive_only",
      baselineP95RunnerTotalMs: projected[0].p95RunnerTotalMs,
      candidateP95RunnerTotalMs: projected[1].p95RunnerTotalMs,
      promotedP95RunnerTotalMs: projected[2].p95RunnerTotalMs,
    },
    candidateSelection: "explicit",
    promotedSelection: "promoted",
    rollback: "not_needed",
  };
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
  const git = (args) => execFileSync("git", ["-C", root, ...args], { encoding: "utf8" }).trim();
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
    .split("\n");
  return {
    records,
    cleanupIds,
    handles: await Promise.all(
      records.map((_, index) => json(resolve(directory, `${phase}-${index + 1}.session.json`))),
    ),
    selections: await Promise.all(
      records.map((_, index) => json(resolve(directory, `${phase}-${index + 1}.selection.json`))),
    ),
    report: await json(resolve(directory, `${phase}-report.json`)),
    check: await json(resolve(directory, `${phase}-check.json`)),
  };
}

async function main([command, ...args]) {
  if (command === "preflight") return preflight(args);
  if (command === "select") {
    return observedSelection(parseStrictJSON(readFileSync(0, "utf8"), "lease listing"), args[0]);
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
    // Failed admission can precede handle publication; only the attempted timing row may recover it.
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
      measurementPolicy(await json(policyPath)),
      await json(baselinePath),
      await json(receiptPath),
      candidateImage,
    );
    return { status: "passed" };
  }
  assert.ok(command === "cohort" || command === "manifest", "unknown proof command");
  const [policyPath, directory, phaseOrReceipt, candidateImage] = args;
  const policy = measurementPolicy(await json(policyPath));
  const selected =
    command === "manifest" ? phases : phases.slice(0, phases.indexOf(phaseOrReceipt) + 1);
  assert.ok(selected.length > 0, "invalid cohort phase");
  const prior = [];
  const summaries = [];
  for (const current of selected) {
    const input = await loadCohort(directory, current);
    summaries.push(validateCohort(policy, current, input, prior, candidateImage));
    prior.push(input);
  }
  if (command === "manifest") {
    validatePromotionReceipt(
      policy,
      await json(resolve(directory, "baseline-1.selection.json")),
      await json(phaseOrReceipt),
      candidateImage,
    );
    return projectManifest(policy, summaries);
  }
  return summaries.at(-1);
}

if (process.argv[1] && realpathSync(process.argv[1]) === realpathSync(scriptPath)) {
  const result = await main(process.argv.slice(2));
  process.stdout.write(`${canonicalJSON(result)}\n`);
}
