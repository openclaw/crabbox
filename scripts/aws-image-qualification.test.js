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
const script = path.join(repoRoot, "scripts", "aws-image-qualification.mjs");
const digest = (digit) => `sha256:${digit.repeat(64)}`;

function input() {
  return {
    schema: "crabbox-aws-image-qualification-input/v1",
    artifact: { digest: digest("1") },
    source: {},
    target: {
      platform: "linux",
      region: "us-west-2",
      instanceType: "m7i.large",
      architecture: "x86_64",
      amiId: "ami-123abc",
    },
    readiness: { profile: "linux-builder", recipeDigest: digest("2") },
    imageRecipe: {},
    evidence: [{ name: "candidate.json", digest: digest("3") }],
    signature: {},
  };
}

function identity() {
  return {
    schema: "crabbox-ready-pool-identity/v1",
    profile: "linux-builder",
    recipeDigest: digest("2"),
    inventoryDigest: digest("4"),
    imageID: "ami-123abc",
    architecture: "amd64",
    seedDigest: digest("5"),
    cacheABIDigest: digest("6"),
  };
}

async function fixture(overrides = {}) {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-qualification-"));
  const files = {
    input: input(),
    identity: identity(),
    negative: {
      schema: "crabbox-aws-image-qualification-negative/v1",
      gates: ["ami", "architecture", "recipe", "type"].map((dimension) => ({
        dimension,
        status: "passed",
      })),
    },
    positive: { schema: "crabbox-aws-image-qualification-status/v1", status: "passed" },
    os: { schema: "crabbox-aws-image-qualification-status/v1", status: "passed" },
    cache: {
      schema: "crabbox-aws-image-qualification-cache/v1",
      advertised: false,
      status: "skipped",
      reason: "provider_capability_not_advertised",
    },
    cleanup: { schema: "crabbox-aws-image-qualification-status/v1", status: "passed" },
  };
  Object.assign(files, overrides);
  for (const [name, value] of Object.entries(files)) {
    await writeFile(path.join(root, `${name}.json`), `${canonicalJSON(value)}\n`);
  }
  const dirty = Buffer.from("crabbox qualification dirty overlay v1\n");
  await writeFile(path.join(root, "dirty.fixture"), dirty);
  await writeFile(path.join(root, "dirty.out"), dirty);
  await writeFile(
    path.join(root, "clean.timing"),
    `${JSON.stringify({
      exitCode: 0,
      syncSkipped: false,
      runId: "run_clean123",
      runnerPhases: [
        {
          name: "workspace.overlay",
        },
      ],
    })}\n`,
  );
  await writeFile(
    path.join(root, "clean.events.json"),
    `${JSON.stringify([
      {
        runID: "run_clean123",
        type: "sync.finished",
        phase: "synced",
        message: "duration=1ms skipped=false mode=overlay files=0 bytes=0",
      },
    ])}\n`,
  );
  await writeFile(
    path.join(root, "dirty.timing"),
    `${JSON.stringify({
      exitCode: 0,
      syncSkipped: false,
      runId: "run_dirty123",
      runnerPhases: [
        {
          name: "workspace.overlay",
          transferCount: 1,
          transferBytes: dirty.length,
        },
      ],
    })}\n`,
  );
  await writeFile(
    path.join(root, "dirty.events.json"),
    `${JSON.stringify([
      {
        runID: "run_dirty123",
        type: "sync.finished",
        phase: "synced",
        message: `duration=2ms skipped=false mode=overlay files=1 bytes=${dirty.length}`,
      },
    ])}\n`,
  );
  return { root, output: path.join(root, "qualification.json"), files, dirty };
}

function buildArgs(value) {
  return [
    "build",
    "--input", path.join(value.root, "input.json"),
    "--identity", path.join(value.root, "identity.json"),
    "--negative", path.join(value.root, "negative.json"),
    "--positive", path.join(value.root, "positive.json"),
    "--os-evidence", path.join(value.root, "os.json"),
    "--dirty-fixture", path.join(value.root, "dirty.fixture"),
    "--dirty-output", path.join(value.root, "dirty.out"),
    "--clean-timing", path.join(value.root, "clean.timing"),
    "--dirty-timing", path.join(value.root, "dirty.timing"),
    "--clean-events", path.join(value.root, "clean.events.json"),
    "--dirty-events", path.join(value.root, "dirty.events.json"),
    "--cache", path.join(value.root, "cache.json"),
    "--cleanup", path.join(value.root, "cleanup.json"),
    "--workflow-ref", "example-org/crabbox/.github/workflows/devtools-image-qualify.yml@refs/heads/main",
    "--workflow-run-id", "123",
    "--workflow-run-attempt", "1",
    "--created-at", "2026-08-21T00:00:00Z",
    "--os-selector", "ubuntu:26.04",
    "--output", value.output,
  ];
}

test("build binds candidate, input bytes, typed identity, fixed gates, and cleanup", async () => {
  const value = await fixture();
  await execFileAsync(process.execPath, [script, ...buildArgs(value)], { cwd: repoRoot });
  const bytes = await readFile(value.output);
  const receipt = JSON.parse(bytes);
  assert.equal(receipt.schema, "crabbox-aws-image-qualification/v1");
  assert.equal(receipt.candidate.artifactDigest, digest("1"));
  assert.equal(receipt.candidate.candidateRecordDigest, digest("3"));
  assert.equal(receipt.target.osSelector, "ubuntu:26.04");
  assert.equal(receipt.gates.osSelector.status, "passed");
  assert.equal(
    receipt.candidate.qualificationInputDigest,
    digestBytes(await readFile(path.join(value.root, "input.json"))),
  );
  assert.equal(receipt.pool.identityDigest, digestJSON(identity()));
  assert.deepEqual(receipt.gates.cleanOverlay.trackedTransfer, { files: 0, bytes: 0 });
  assert.deepEqual(receipt.gates.dirtyOverlay.trackedTransfer, {
    files: 1,
    bytes: value.dirty.length,
  });
  assert.equal(receipt.cleanup.status, "passed");
  assert.doesNotMatch(bytes.toString(), /cbx_|\/Users\/|sshHost|borrowToken/);
  const verification = await execFileAsync(
    process.execPath,
    [script, "verify", "--file", value.output],
    { cwd: repoRoot },
  );
  assert.equal(JSON.parse(verification.stdout).receiptDigest, digestBytes(bytes));
});

test("build rejects fallback telemetry, dirty byte drift, and failed cleanup", async () => {
  const cases = [
    async (value) => {
      await writeFile(
        path.join(value.root, "clean.timing"),
        JSON.stringify({
          exitCode: 0,
          syncSkipped: false,
          runnerPhases: [{ name: "workspace.overlay", fallback: true }],
        }),
      );
    },
    async (value) => {
      await writeFile(path.join(value.root, "dirty.out"), "wrong\n");
    },
    async (value) => {
      await writeFile(
        path.join(value.root, "cleanup.json"),
        JSON.stringify({
          schema: "crabbox-aws-image-qualification-status/v1",
          status: "failed",
        }),
      );
    },
  ];
  for (const mutate of cases) {
    const value = await fixture();
    await mutate(value);
    await assert.rejects(
      execFileAsync(process.execPath, [script, ...buildArgs(value)], { cwd: repoRoot }),
    );
  }
});

test("build rejects clean overlays without an explicit matching sync event", async () => {
  const value = await fixture();
  await writeFile(
    path.join(value.root, "clean.events.json"),
    JSON.stringify([]),
  );
  await assert.rejects(
    execFileAsync(process.execPath, [script, ...buildArgs(value)], { cwd: repoRoot }),
    /exactly one sync\.finished event/,
  );
});

test("build normalizes omitted clean overlay phase counters to zero", async () => {
  const value = await fixture();
  await execFileAsync(process.execPath, [script, ...buildArgs(value)], { cwd: repoRoot });
  const receipt = JSON.parse(await readFile(value.output, "utf8"));
  assert.deepEqual(receipt.gates.cleanOverlay.trackedTransfer, { files: 0, bytes: 0 });
});

test("build rejects explicit null overlay phase counters", async () => {
  for (const field of ["transferCount", "transferBytes"]) {
    const value = await fixture();
    const report = JSON.parse(await readFile(path.join(value.root, "clean.timing"), "utf8"));
    report.runnerPhases[0][field] = null;
    await writeFile(path.join(value.root, "clean.timing"), JSON.stringify(report));
    await assert.rejects(
      execFileAsync(process.execPath, [script, ...buildArgs(value)], { cwd: repoRoot }),
      /timing phase transfer counters are invalid/,
    );
  }
});

test("build rejects overlay timing phase and sync event counter drift", async () => {
  for (const mutate of [
    async (value) => {
      const report = JSON.parse(await readFile(path.join(value.root, "clean.timing"), "utf8"));
      report.runnerPhases[0].transferCount = 1;
      await writeFile(path.join(value.root, "clean.timing"), JSON.stringify(report));
    },
    async (value) => {
      const report = JSON.parse(await readFile(path.join(value.root, "dirty.timing"), "utf8"));
      delete report.runnerPhases[0].transferCount;
      delete report.runnerPhases[0].transferBytes;
      await writeFile(path.join(value.root, "dirty.timing"), JSON.stringify(report));
    },
    async (value) => {
      const report = JSON.parse(await readFile(path.join(value.root, "dirty.timing"), "utf8"));
      report.runnerPhases[0].transferBytes += 1;
      await writeFile(path.join(value.root, "dirty.timing"), JSON.stringify(report));
    },
  ]) {
    const value = await fixture();
    await mutate(value);
    await assert.rejects(
      execFileAsync(process.execPath, [script, ...buildArgs(value)], { cwd: repoRoot }),
      /timing phase transfer counters do not match its sync event/,
    );
  }
});

test("build refuses overwrite and receipt verification detects identity tampering", async () => {
  const value = await fixture();
  await execFileAsync(process.execPath, [script, ...buildArgs(value)], { cwd: repoRoot });
  await assert.rejects(
    execFileAsync(process.execPath, [script, ...buildArgs(value)], { cwd: repoRoot }),
    /already exists/,
  );
  const receipt = JSON.parse(await readFile(value.output));
  receipt.pool.identity.imageID = "ami-tampered";
  await writeFile(value.output, `${canonicalJSON(receipt)}\n`);
  await assert.rejects(
    execFileAsync(process.execPath, [script, "verify", "--file", value.output]),
    /identity digest mismatch/,
  );
});

test("receipt verification rejects an unbound Linux OS selector", async () => {
  const value = await fixture();
  await execFileAsync(process.execPath, [script, ...buildArgs(value)], { cwd: repoRoot });
  const receipt = JSON.parse(await readFile(value.output));
  receipt.target.osSelector = "ubuntu:rolling";
  await writeFile(value.output, `${canonicalJSON(receipt)}\n`);
  await assert.rejects(
    execFileAsync(process.execPath, [script, "verify", "--file", value.output]),
    /OS selector is invalid/,
  );
});

test("receipt verification rejects missing overlay proof and target-identity drift", async () => {
  for (const mutate of [
    (receipt) => {
      delete receipt.gates.cleanOverlay;
    },
    (receipt) => {
      receipt.target.amiId = "ami-other";
    },
  ]) {
    const value = await fixture();
    await execFileAsync(process.execPath, [script, ...buildArgs(value)], { cwd: repoRoot });
    const receipt = JSON.parse(await readFile(value.output));
    mutate(receipt);
    await writeFile(value.output, `${canonicalJSON(receipt)}\n`);
    await assert.rejects(
      execFileAsync(process.execPath, [script, "verify", "--file", value.output]),
    );
  }
});
