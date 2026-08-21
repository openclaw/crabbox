import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import { canonicalJSON, digestJSON } from "./aws-image-candidate.mjs";

const repoRoot = path.resolve(import.meta.dirname, "..");
const script = path.join(repoRoot, "scripts", "aws-image-candidate.mjs");

function run(args) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [script, ...args], {
      cwd: repoRoot,
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");
    child.stdout.on("data", (chunk) => {
      stdout += chunk;
    });
    child.stderr.on("data", (chunk) => {
      stderr += chunk;
    });
    child.on("error", reject);
    child.on("close", (code) => resolve({ code, stdout, stderr }));
  });
}

async function fixture() {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-aws-candidate-"));
  const checkpoint = path.join(root, "checkpoint.json");
  const scrub = path.join(root, "scrub.json");
  const checks = path.join(root, "checks.json");
  const output = path.join(root, "bundle");
  await writeFile(
    checkpoint,
    JSON.stringify({
      kind: "aws-ami",
      provider: "aws",
      targetOS: "linux",
      native: {
        provider: "aws",
        kind: "aws-ami",
        imageId: "ami-123abc",
        region: "us-west-2",
        architecture: "x86_64",
        snapshotIds: ["snap-def456", "snap-abc123"],
      },
    }),
  );
  const evidence = {
    schema: "crabbox-aws-image-scrub/v1",
    target: "linux",
    findings: [],
    removed: {
      authorizedKeys: 2,
      cloudInitState: 2,
      credentials: 3,
      hostIdentity: 1,
      prepArtifacts: 1,
      shellHistory: 1,
      sshHostKeys: 4,
      workspaces: 1,
    },
  };
  await writeFile(
    scrub,
    `${canonicalJSON({ ...evidence, evidenceDigest: digestJSON(evidence) })}\n`,
  );
  await writeFile(
    checks,
    JSON.stringify({
      schema: "crabbox-aws-image-checks/v1",
      checks: [
        { name: "source-smoke", status: "passed" },
        { name: "scrub", status: "passed", evidenceDigest: digestJSON(evidence) },
        { name: "candidate-boot", status: "passed" },
        { name: "candidate-smoke", status: "passed" },
      ],
    }),
  );
  const args = [
    "create",
    "--checkpoint",
    checkpoint,
    "--scrub-report",
    scrub,
    "--checks",
    checks,
    "--recipe",
    path.join(repoRoot, "recipes", "aws", "v1", "linux-devtools.json"),
    "--out-dir",
    output,
    "--target",
    "linux",
    "--region",
    "us-west-2",
    "--instance-type",
    "m7i.large",
    "--architecture",
    "x86_64",
    "--base-image",
    "ami-base123",
    "--previous-default",
    "ami-old123",
    "--source-repository",
    "example-org/crabbox",
    "--source-commit",
    "0123456789abcdef0123456789abcdef01234567",
    "--workflow-ref",
    "example-org/crabbox/.github/workflows/devtools-image-publish.yml@refs/heads/main",
    "--workflow-run-id",
    "12345",
    "--workflow-run-attempt",
    "2",
    "--created-at",
    "2026-08-21T00:00:00.000Z",
  ];
  return { root, output, checks, args };
}

test("candidate creator writes a complete immutable evidence bundle", async () => {
  const value = await fixture();
  const result = await run(value.args);
  assert.equal(result.code, 0, result.stderr);
  const summary = JSON.parse(result.stdout);
  assert.match(summary.candidateDigest, /^sha256:[0-9a-f]{64}$/);
  assert.equal(summary.immutableTag, summary.candidateDigest.replace("sha256:", "sha256-"));

  const candidate = JSON.parse(await readFile(path.join(value.output, "candidate.json"), "utf8"));
  assert.equal(candidate.schema, "crabbox-aws-image-candidate/v1");
  assert.equal(candidate.image.amiId, "ami-123abc");
  assert.deepEqual(candidate.image.snapshotIds, ["snap-abc123", "snap-def456"]);
  assert.equal(candidate.previousDefault.imageId, "ami-old123");
  assert.equal(candidate.readiness.profile, "linux-builder");
  assert.match(candidate.readiness.recipeDigest, /^sha256:[0-9a-f]{64}$/);

  for (const file of [
    "bundle.json",
    "candidate.json",
    "recipe.json",
    "sbom.spdx.json",
    "provenance.intoto.jsonl",
    "scrub-report.json",
  ]) {
    JSON.parse(await readFile(path.join(value.output, file), "utf8"));
  }
  const all = await Promise.all(
    [
      "bundle.json",
      "candidate.json",
      "recipe.json",
      "sbom.spdx.json",
      "provenance.intoto.jsonl",
      "scrub-report.json",
    ].map((file) => readFile(path.join(value.output, file), "utf8")),
  );
  assert.doesNotMatch(all.join("\n"), /super-secret-value|authorized_keys|credentials\/token/);

  const bundle = JSON.parse(await readFile(path.join(value.output, "bundle.json"), "utf8"));
  assert.equal(
    bundle.artifactType,
    "application/vnd.crabbox.aws-image-evidence.v1",
  );
  assert.equal(bundle.oci.immutableTag, summary.immutableTag);
  assert.equal(
    bundle.oci.repository,
    "ghcr.io/example-org/crabbox-aws-image-candidates",
  );
  assert.equal(
    bundle.oci.signature.certificateIdentity,
    "https://github.com/example-org/crabbox/.github/workflows/devtools-image-publish.yml@refs/heads/main",
  );

  const verify = await run(["verify", "--dir", value.output]);
  assert.equal(verify.code, 0, verify.stderr);
  assert.equal(JSON.parse(verify.stdout).candidateDigest, summary.candidateDigest);
});

test("candidate verifier rejects a mismatched keyless identity", async () => {
  const value = await fixture();
  const result = await run(value.args);
  assert.equal(result.code, 0, result.stderr);
  const bundlePath = path.join(value.output, "bundle.json");
  const bundle = JSON.parse(await readFile(bundlePath, "utf8"));
  bundle.oci.signature.certificateIdentity =
    "https://github.com/example-org/crabbox/.github/workflows/other.yml@refs/heads/main";
  await writeFile(bundlePath, `${canonicalJSON(bundle)}\n`);
  const verify = await run(["verify", "--dir", value.output]);
  assert.equal(verify.code, 1);
  assert.match(verify.stderr, /invalid keyless signature contract/);
});

test("candidate verifier rejects scrub evidence not bound to the scrub check", async () => {
  const value = await fixture();
  const result = await run(value.args);
  assert.equal(result.code, 0, result.stderr);
  const reportPath = path.join(value.output, "scrub-report.json");
  const report = JSON.parse(await readFile(reportPath, "utf8"));
  report.removed.workspaces += 1;
  const evidence = {
    schema: report.schema,
    target: report.target,
    removed: report.removed,
    findings: report.findings,
  };
  report.evidenceDigest = digestJSON(evidence);
  await writeFile(reportPath, `${canonicalJSON(report)}\n`);
  const verify = await run(["verify", "--dir", value.output]);
  assert.equal(verify.code, 1);
  assert.match(verify.stderr, /scrub check evidence digest does not match scrub report/);
});

test("candidate creator writes nothing until every required gate passes", async () => {
  const value = await fixture();
  await writeFile(
    value.checks,
    JSON.stringify({
      schema: "crabbox-aws-image-checks/v1",
      checks: [
        { name: "source-smoke", status: "passed" },
        { name: "scrub", status: "passed" },
        { name: "candidate-boot", status: "passed" },
      ],
    }),
  );
  const result = await run(value.args);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /checks must contain exactly/);
  await assert.rejects(readFile(path.join(value.output, "candidate.json"), "utf8"), {
    code: "ENOENT",
  });
});

test("candidate creator refuses to overwrite an immutable bundle", async () => {
  const value = await fixture();
  const first = await run(value.args);
  assert.equal(first.code, 0, first.stderr);
  const before = await readFile(path.join(value.output, "candidate.json"), "utf8");
  const second = await run(value.args);
  assert.equal(second.code, 1);
  assert.match(second.stderr, /candidate output already exists/);
  assert.equal(await readFile(path.join(value.output, "candidate.json"), "utf8"), before);
});

test("candidate creator rejects malformed checkpoint JSON without output", async () => {
  const value = await fixture();
  const checkpointIndex = value.args.indexOf("--checkpoint") + 1;
  await writeFile(value.args[checkpointIndex], "{not-json");
  const result = await run(value.args);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /checkpoint is not valid JSON/);
  await assert.rejects(readFile(path.join(value.output, "candidate.json"), "utf8"), {
    code: "ENOENT",
  });
});

test("candidate creator rejects target and region mismatches", async () => {
  const value = await fixture();
  const checkpointIndex = value.args.indexOf("--checkpoint") + 1;
  const checkpoint = JSON.parse(await readFile(value.args[checkpointIndex], "utf8"));
  checkpoint.native.region = "us-east-1";
  await writeFile(value.args[checkpointIndex], JSON.stringify(checkpoint));
  const result = await run(value.args);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /checkpoint region does not match/);
});

test("candidate creator rejects scrub findings", async () => {
  const value = await fixture();
  const scrubIndex = value.args.indexOf("--scrub-report") + 1;
  const report = JSON.parse(await readFile(value.args[scrubIndex], "utf8"));
  report.findings = ["credentials"];
  const evidence = {
    schema: report.schema,
    target: report.target,
    removed: report.removed,
    findings: report.findings,
  };
  report.evidenceDigest = digestJSON(evidence);
  await writeFile(value.args[scrubIndex], `${canonicalJSON(report)}\n`);
  const result = await run(value.args);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /residual findings: credentials/);
});

test("candidate creator rejects missing snapshot evidence", async () => {
  const value = await fixture();
  const checkpointIndex = value.args.indexOf("--checkpoint") + 1;
  const checkpoint = JSON.parse(await readFile(value.args[checkpointIndex], "utf8"));
  checkpoint.native.snapshotIds = [];
  await writeFile(value.args[checkpointIndex], JSON.stringify(checkpoint));
  const result = await run(value.args);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /must contain unique AWS snapshot ids/);
});

test("candidate creator rejects a failed candidate smoke", async () => {
  const value = await fixture();
  const checksIndex = value.args.indexOf("--checks") + 1;
  const checks = JSON.parse(await readFile(value.args[checksIndex], "utf8"));
  checks.checks.find((item) => item.name === "candidate-smoke").status = "failed";
  await writeFile(value.args[checksIndex], JSON.stringify(checks));
  const result = await run(value.args);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /all required candidate checks must pass/);
});
