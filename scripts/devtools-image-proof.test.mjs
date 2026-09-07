import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";

import {
  measurementPolicy,
  observedSelection,
  projectManifest,
  validateCohort,
  validatePromotionReceipt,
} from "./devtools-image-proof.mjs";

const fingerprint = (value) =>
  `sha256:${createHash("sha256").update(JSON.stringify(value)).digest("hex")}`;
const policy = measurementPolicy({
  sourceRevision: "a".repeat(40),
  recipeDigest: `sha256:${"b".repeat(64)}`,
  region: "us-west-2",
  machineType: "m7i.large",
  serverClass: "standard",
  maxP95RunnerTotalMs: 600000,
  ttl: "2h",
  idleTimeout: "30m",
});

function cohort(offset = 0) {
  const records = Array.from({ length: 3 }, (_, index) => ({
    schemaVersion: 1,
    source: "run",
    benchmark: {
      repoHead: policy.sourceRevision,
      repoFingerprint: fingerprint("fixture"),
      commandFingerprint: fingerprint(["true"]),
      coldRun: true,
    },
    timing: {
      provider: "aws",
      machineType: policy.machineType,
      leaseId: `cbx_${(offset + index + 1).toString(16).padStart(12, "0")}`,
      runId: `run_${offset + index + 1}`,
      exitCode: 0,
      runnerTotalMs: 1000,
      syncMs: 0,
      syncSkipped: false,
      leaseStopped: false,
    },
  }));
  const report = {
    schemaVersion: 1,
    observationCount: 3,
    matchedCount: 3,
    groups: [
      {
        source: "run",
        provider: "aws",
        machineType: policy.machineType,
        commandFingerprint: fingerprint(["true"]),
        coldRun: true,
        n: 3,
        observationCount: 3,
        failureCount: 0,
        runnerTotalN: 3,
        medianSyncMs: 0,
        medianRunnerTotalMs: 1000,
        p95RunnerTotalMs: 1000,
      },
    ],
  };
  const check = {
    schemaVersion: 1,
    matchedCount: 3,
    groupCount: 1,
    passed: true,
    reasons: [],
    policy: {
      minSamples: 3,
      requiredRunnerTotalSamples: 3,
      maxFailures: 0,
      maxP95RunnerTotal: "10m0s",
    },
    groups: [
      {
        source: "run",
        provider: "aws",
        machineType: policy.machineType,
        coldRun: true,
        observationCount: 3,
        successfulSamples: 3,
        failureCount: 0,
        runnerTotalN: 3,
        p95RunnerTotalMs: 1000,
        passed: true,
        reasons: [],
      },
    ],
  };
  const selections = records.map((record, index) => ({
    id: record.timing.leaseId,
    provider: "aws",
    target: "linux",
    region: policy.region,
    serverType: policy.machineType,
    cloudID: `i-${(offset + index + 1).toString(16).padStart(17, "0")}`,
    image: {
      id: offset ? "ami-candidate" : "ami-previous",
      source: offset === 3 ? "explicit" : "promoted",
      kind: "aws-ami",
      region: policy.region,
      promotedAt: "2026-09-01T00:00:00Z",
    },
  }));
  return {
    records,
    report,
    check,
    selections,
    handles: records.map(({ timing }) => ({
      provider: "aws",
      leaseId: timing.leaseId,
      runId: timing.runId,
      reused: false,
      kept: true,
      cleanupCommand: "crabbox stop " + timing.leaseId,
    })),
    cleanupIds: records.map((record) => record.timing.leaseId),
  };
}

test("measured policy requires an explicit positive threshold and names 12 planned leases", () => {
  assert.equal(policy.samplesPerCohort, 3);
  assert.equal(policy.plannedLeaseCount, 12);
  for (const threshold of [undefined, 0, -1, NaN, Infinity, "600000"]) {
    assert.throws(
      () => measurementPolicy({ ...policy, maxP95RunnerTotalMs: threshold }),
      /threshold/,
    );
  }
});

test("cohorts accept measured zero-duration sync but reject missing, mixed, or reused evidence", () => {
  const input = cohort();
  assert.equal(validateCohort(policy, "baseline", input).medianSyncMs, 0);
  for (const mutate of [
    (value) => value.records.pop(),
    (value) => {
      delete value.records[0].timing.runnerTotalMs;
    },
    (value) => {
      value.records[0].benchmark.repoHead = "c".repeat(40);
    },
    (value) => {
      value.records[0].benchmark.commandFingerprint = fingerprint(["false"]);
    },
    (value) => {
      value.records[0].timing.machineType = "different";
    },
    (value) => {
      value.records[0].benchmark.coldRun = false;
    },
    (value) => {
      value.records[0].timing.syncSkipped = true;
    },
    (value) => {
      value.cleanupIds.pop();
    },
    (value) => {
      value.report.groups.push(value.report.groups[0]);
    },
    (value) => {
      value.check.passed = false;
    },
    (value) => {
      value.check.policy.maxP95RunnerTotal = "1h0m0s";
    },
    (value) => {
      value.check.groups[0].runnerTotalN = 2;
    },
    (value) => {
      value.handles[0].kept = false;
    },
    (value) => {
      value.handles[0].reused = true;
    },
    (value) => {
      value.handles[0].leaseId = value.handles[1].leaseId;
    },
    (value) => {
      value.handles[0].runId = "run_other";
    },
    (value) => {
      value.selections[0].id = value.selections[1].id;
    },
    (value) => {
      value.selections[1].cloudID = value.selections[0].cloudID;
    },
  ]) {
    const mutated = structuredClone(input);
    mutate(mutated);
    assert.throws(() => validateCohort(policy, "baseline", mutated));
  }
  assert.throws(() => validateCohort(policy, "baseline", input, [input]), /reused/);
  const mixed = cohort(3);
  mixed.records[0].benchmark.repoFingerprint = fingerprint("another checkout");
  assert.throws(
    () => validateCohort(policy, "candidate", mixed, [input], "ami-candidate"),
    /identity/,
  );
  const reused = cohort(3);
  reused.selections[0].cloudID = input.selections[0].cloudID;
  assert.throws(
    () => validateCohort(policy, "candidate", reused, [input], "ami-candidate"),
    /reused provider instance/,
  );
});

test("selection must be an actual unambiguous observation, not requested values", () => {
  const selection = cohort().selections[0];
  assert.equal(observedSelection([selection], selection.id).region, policy.region);
  for (const listing of [[], [selection, selection], [{ ...selection, id: "another" }]]) {
    assert.throws(() => observedSelection(listing, selection.id), /observation/);
  }
  assert.throws(
    () => observedSelection("image selected id=ami-requested", selection.id),
    /listing/,
  );
  assert.throws(
    () => observedSelection([{ ...selection, region: undefined }], selection.id),
    /observed region/,
  );
  for (const mutate of [
    (input) => {
      input.selections.pop();
    },
    (input) => {
      input.selections[1].region = "us-east-1";
    },
    (input) => {
      input.selections[1].image.region = "us-east-1";
    },
    (input) => {
      input.selections[1].image.id = "ami-changed";
    },
    (input) => {
      input.selections[1].image.source = "explicit";
    },
    (input) => {
      input.selections[1].image.promotedAt = "2026-09-02T00:00:00Z";
    },
  ]) {
    const input = cohort();
    input.records[1].timing.region = policy.region;
    mutate(input);
    assert.throws(() => validateCohort(policy, "baseline", input));
  }
  for (const phase of ["candidate", "promoted"]) {
    const input = cohort(phase === "candidate" ? 3 : 6);
    assert.throws(() => validateCohort(policy, phase, input, [], "ami-different"), /candidate/);
    input.selections[0].region = "us-east-1";
    assert.throws(() => validateCohort(policy, phase, input, [], "ami-candidate"), /region/);
  }
});

test("baseline selection reconciles with the original promotion receipt", () => {
  const baseline = cohort().selections[0];
  const receipt = {
    image: { id: "ami-candidate", region: policy.region },
    previous: {
      state: "present",
      imageId: baseline.image.id,
      aliases: [{ state: "present", image: { ...baseline.image } }],
    },
  };
  validatePromotionReceipt(policy, baseline, receipt, "ami-candidate");
  validatePromotionReceipt(
    policy,
    { ...baseline, image: { ...baseline.image, source: "stock" } },
    {
      ...receipt,
      previous: { state: "absent" },
    },
    "ami-candidate",
  );
  for (const mutate of [
    (value) => {
      value.previous.state = "absent";
    },
    (value) => {
      value.previous.imageId = "ami-changed";
    },
    (value) => {
      value.previous.aliases[0].image.region = "us-east-1";
    },
    (value) => {
      value.previous.aliases[0].image.promotedAt = "2026-09-02T00:00:00Z";
    },
    (value) => {
      value.image.id = "ami-changed";
    },
    (value) => {
      value.image.region = "us-east-1";
    },
  ]) {
    const changed = structuredClone(receipt);
    mutate(changed);
    assert.throws(() => validatePromotionReceipt(policy, baseline, changed, "ami-candidate"));
  }
});

test("public proof is an allowlisted projection, not raw reports or arbitrary strings", () => {
  const poison = "PRIVATE_fixture_hostname_token_path";
  const inputs = ["baseline", "candidate", "promoted"].map((phase, index) => {
    const input = cohort(index * 3);
    input.report.storePath = poison;
    input.report.groups[0].providerFamily = poison;
    input.report.groups[0].runnerPhases = [{ name: poison, ms: 1000 }];
    input.check.groups[0].providerCategory = poison;
    input.records[0].benchmark.commandDisplay = poison;
    input.records[0].timing.repoPath = poison;
    return validateCohort(policy, phase, input, [], "ami-candidate");
  });
  const manifest = projectManifest(policy, inputs);
  assert.doesNotMatch(JSON.stringify(manifest), new RegExp(poison));
  assert.equal(manifest.comparison.kind, "descriptive_only");
  assert.equal(manifest.rollback, "not_needed");
  assert.equal(manifest.plannedLeaseCount, 12);
  assert.throws(() => projectManifest(policy, inputs.slice(0, 2)));
  const bad = cohort();
  bad.check.reasons = [poison];
  assert.throws(() => validateCohort(policy, "baseline", bad));
  assert.throws(() => validateCohort(policy, poison, cohort()));
  assert.throws(() =>
    projectManifest(
      policy,
      inputs.map((input) => ({ ...input, checkReasons: [poison] })),
    ),
  );
});
