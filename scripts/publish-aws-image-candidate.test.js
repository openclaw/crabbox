import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import { promisify } from "node:util";
import { canonicalJSON, digestJSON } from "./aws-image-candidate.mjs";

const repoRoot = path.resolve(import.meta.dirname, "..");
const script = path.join(repoRoot, "scripts", "publish-aws-image-candidate.sh");
const candidateScript = path.join(repoRoot, "scripts", "aws-image-candidate.mjs");
const execFileAsync = promisify(execFile);

async function setup(existing = false) {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-candidate-publish-"));
  const bundle = path.join(root, "bundle");
  const bin = path.join(root, "bin");
  const log = path.join(root, "commands.log");
  await mkdir(bin);
  const checkpoint = path.join(root, "checkpoint.json");
  const scrub = path.join(root, "scrub.json");
  const checks = path.join(root, "checks.json");
  await writeFile(checkpoint, JSON.stringify({
    kind: "aws-ami",
    provider: "aws",
    targetOS: "linux",
    native: {
      provider: "aws",
      kind: "aws-ami",
      imageId: "ami-123abc",
      region: "us-west-2",
      architecture: "x86_64",
      snapshotIds: ["snap-123abc"],
    },
  }));
  const scrubEvidence = {
    schema: "crabbox-aws-image-scrub/v1",
    target: "linux",
    removed: { credentials: 1 },
    findings: [],
  };
  await writeFile(scrub, `${canonicalJSON({
    ...scrubEvidence,
    evidenceDigest: digestJSON(scrubEvidence),
  })}\n`);
  await writeFile(checks, JSON.stringify({
    schema: "crabbox-aws-image-checks/v1",
    checks: [
      { name: "source-smoke", status: "passed" },
      { name: "scrub", status: "passed", evidenceDigest: digestJSON(scrubEvidence) },
      { name: "candidate-boot", status: "passed" },
      { name: "candidate-smoke", status: "passed" },
    ],
  }));
  await execFileAsync(process.execPath, [
    candidateScript,
    "create",
    "--checkpoint", checkpoint,
    "--scrub-report", scrub,
    "--checks", checks,
    "--recipe", path.join(repoRoot, "recipes", "aws", "v1", "linux-devtools.json"),
    "--out-dir", bundle,
    "--target", "linux",
    "--region", "us-west-2",
    "--instance-type", "m7i.large",
    "--architecture", "x86_64",
    "--base-image", "ami-base123",
    "--source-repository", "example-org/crabbox",
    "--source-commit", "0123456789abcdef0123456789abcdef01234567",
    "--workflow-ref", "example-org/crabbox/.github/workflows/devtools-image-publish.yml@refs/heads/main",
    "--workflow-run-id", "12345",
    "--workflow-run-attempt", "1",
    "--oci-repository", "ghcr.io/example-org/crabbox-aws-devtools-candidates",
    "--created-at", "2026-08-21T00:00:00.000Z",
  ], { cwd: repoRoot });
  await writeFile(
    path.join(bin, "oras"),
    `#!/usr/bin/env bash
set -euo pipefail
printf 'oras %s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "\${1:-}" in
  manifest)
    [[ "\${CRABBOX_FAKE_EXISTING:-0}" == "1" ]] && exit 0
    exit 1
    ;;
  resolve)
    printf 'sha256:%064d\\n' 7
    ;;
esac
`,
  );
  await writeFile(
    path.join(bin, "cosign"),
    `#!/usr/bin/env bash
set -euo pipefail
printf 'cosign %s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
`,
  );
  await chmod(path.join(bin, "oras"), 0o755);
  await chmod(path.join(bin, "cosign"), 0o755);
  return { root, bundle, bin, log, existing };
}

function run(value) {
  return new Promise((resolve, reject) => {
    const child = spawn(
      "bash",
      [
        script,
        "--bundle",
        value.bundle,
        "--repository",
        "ghcr.io/example-org/crabbox-aws-devtools-candidates",
        "--certificate-identity",
        "https://github.com/example-org/crabbox/.github/workflows/devtools-image-publish.yml@refs/heads/main",
      ],
      {
        cwd: repoRoot,
        env: {
          ...process.env,
          PATH: `${value.bin}${path.delimiter}${process.env.PATH ?? ""}`,
          CRABBOX_FAKE_LOG: value.log,
          CRABBOX_FAKE_EXISTING: value.existing ? "1" : "0",
        },
        stdio: ["ignore", "pipe", "pipe"],
      },
    );
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

test("publisher pushes once, resolves the digest, then keylessly signs and verifies", async () => {
  const value = await setup();
  const result = await run(value);
  assert.equal(result.code, 0, result.stderr);
  const publication = JSON.parse(result.stdout);
  assert.match(publication.tag, /^sha256-[0-9a-f]{64}$/);
  assert.match(publication.ociDigest, /^sha256:[0-9a-f]{64}$/);
  assert.equal(
    publication.immutableRef,
    `${publication.repository}@${publication.ociDigest}`,
  );

  const commands = (await readFile(value.log, "utf8")).trim().split("\n");
  assert.match(commands[0], /^oras manifest fetch /);
  assert.match(commands[1], /^oras push /);
  assert.match(
    commands[1],
    /candidate\.json:application\/vnd\.crabbox\.aws-image-candidate/,
  );
  assert.match(commands[2], /^oras resolve /);
  assert.match(commands[3], /^cosign sign --yes .*@sha256:/);
  assert.match(commands[4], /^cosign verify --certificate-identity /);
});

test("publisher refuses an existing immutable tag before push or signing", async () => {
  const value = await setup(true);
  const result = await run(value);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /refusing to overwrite existing candidate tag/);
  const commands = (await readFile(value.log, "utf8")).trim().split("\n");
  assert.equal(commands.length, 1);
  assert.match(commands[0], /^oras manifest fetch /);
});
