import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { once } from "node:events";
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { promisify } from "node:util";

import { canonicalJSON } from "./aws-image-candidate.mjs";

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(import.meta.dirname, "..");
const publisher = path.join(repoRoot, "scripts", "publish-aws-image-promotion-receipt.sh");
const registryScript = path.join(repoRoot, "scripts", "test-oci-registry.mjs");
const materialSource = path.join(repoRoot, "scripts", "generate-test-sigstore-material", "main.go");
const realOras = process.env.CRABBOX_ORAS;
const realCosign = process.env.CRABBOX_COSIGN;
const available = Boolean(realOras && realCosign);
const workflowRef =
  "example-org/crabbox/.github/workflows/devtools-image-promote.yml@refs/heads/main";
const identity = `https://github.com/${workflowRef}`;
const issuer = "https://token.actions.githubusercontent.com";
const digest = `sha256:${"a".repeat(64)}`;

async function stop(child) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const closed = once(child, "close");
  child.kill("SIGTERM");
  await closed;
}

async function startRegistry() {
  const child = spawn(process.execPath, [registryScript], {
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
  const address = await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error(`registry timeout: ${stderr}`)), 5000);
    child.stdout.on("data", () => {
      const newline = stdout.indexOf("\n");
      if (newline < 0) return;
      clearTimeout(timeout);
      resolve(JSON.parse(stdout.slice(0, newline)));
    });
    child.once("exit", (code) => reject(new Error(`registry exited ${code}: ${stderr}`)));
  });
  return { child, host: `${address.host}:${address.port}` };
}

async function wrappers(root) {
  const bin = path.join(root, "bin");
  await mkdir(bin);
  await writeFile(
    path.join(bin, "oras"),
    `#!/usr/bin/env node
const { spawnSync } = require("node:child_process");
const host = process.env.CRABBOX_TEST_REGISTRY;
let args = process.argv.slice(2).map((arg) => arg.replace(/^ghcr[.]io\\//, host + "/"));
if (args[0] === "manifest" || args[0] === "blob") args = [args[0], args[1], "--plain-http", ...args.slice(2)];
else if (args[0] !== "version") args = [args[0], "--plain-http", ...args.slice(1)];
const result = spawnSync(process.env.CRABBOX_REAL_ORAS, args, { encoding: "utf8", env: process.env });
if (result.error) throw result.error;
process.stdout.write((result.stdout || "").split(host + "/").join("ghcr.io/"));
process.stderr.write((result.stderr || "").split(host + "/").join("ghcr.io/"));
process.exit(result.status ?? 1);
`,
  );
  await writeFile(
    path.join(bin, "cosign"),
    `#!/usr/bin/env node
const { spawnSync } = require("node:child_process");
const path = require("node:path");
const host = process.env.CRABBOX_TEST_REGISTRY;
const material = process.env.CRABBOX_TEST_SIGSTORE_DIR;
let args = process.argv.slice(2).map((arg) => arg.replace(/^ghcr[.]io\\//, host + "/"));
if (args[0] === "sign") {
  args = ["sign", "--allow-http-registry", "--key", path.join(material, "cosign.key"),
    "--certificate", path.join(material, "cosign.crt"), "--certificate-chain",
    path.join(material, "chain.crt"), "--tlog-upload=false", ...args.slice(1)];
} else if (args[0] === "verify") {
  args = ["verify", "--allow-http-registry", "--trusted-root",
    path.join(material, "trusted-root.json"), "--insecure-ignore-tlog",
    "--insecure-ignore-sct", ...args.slice(1)];
}
const result = spawnSync(process.env.CRABBOX_REAL_COSIGN, args, {
  stdio: "inherit",
  env: { ...process.env, COSIGN_EXPERIMENTAL: "1", COSIGN_PASSWORD: "crabbox-test-only" },
});
if (result.error) throw result.error;
process.exit(result.status ?? 1);
`,
  );
  await chmod(path.join(bin, "oras"), 0o755);
  await chmod(path.join(bin, "cosign"), 0o755);
  return bin;
}

async function sigstoreMaterial(root) {
  const material = path.join(root, "sigstore");
  await mkdir(material);
  await execFileAsync(
    realCosign,
    ["generate-key-pair", "--output-key-prefix", path.join(material, "cosign")],
    {
      cwd: repoRoot,
      env: { ...process.env, COSIGN_PASSWORD: "crabbox-test-only" },
    },
  );
  await execFileAsync(
    "go",
    [
      "run",
      materialSource,
      "--output",
      material,
      "--public-key",
      path.join(material, "cosign.pub"),
      "--identity",
      identity,
      "--issuer",
      issuer,
    ],
    {
      cwd: repoRoot,
      env: { ...process.env, GOCACHE: path.join(root, "go-cache") },
    },
  );
  return material;
}

function attempt() {
  const desired = {
    state: "present",
    imageId: "ami-123abc",
    accountId: "123456789012",
    snapshotIds: ["snap-123abc"],
  };
  return {
    schema: "crabbox-image-promotion-attempt/v1",
    idempotencyKey: "run-200-attempt-1",
    inputDigest: "c".repeat(64),
    operation: "promote",
    phase: "completed",
    version: 3,
    claimVersion: 1,
    outcome: "promoted_verified",
    expected: { state: "absent" },
    before: { state: "absent" },
    beforeBinding: { state: "absent" },
    after: { state: "present", imageId: desired.imageId, revision: "revision-1" },
    desired,
    evidence: {
      schema: "crabbox-image-promotion-evidence/v1",
      qualification: {
        reference: `ghcr.io/example-org/qualification@${digest}`,
        digest,
        receiptDigest: digest,
        inputDigest: digest,
        signatureDigest: digest,
      },
      candidate: {
        reference: `ghcr.io/example-org/candidate@${digest}`,
        digest,
        recordDigest: digest,
      },
      source: { repository: "example-org/crabbox", commit: "b".repeat(40) },
      workflow: {
        identity:
          "example-org/crabbox/.github/workflows/devtools-image-qualify.yml@refs/heads/main",
        runId: "100",
        runAttempt: "1",
      },
      scope: {
        provider: "aws",
        target: "linux",
        region: "eu-west-1",
        instanceType: "c7i.4xlarge",
        architecture: "x86_64",
        os: "ubuntu:26.04",
        profile: "default",
        recipeDigest: digest,
      },
      desired: {
        imageId: desired.imageId,
        accountId: desired.accountId,
        snapshotIds: desired.snapshotIds,
      },
    },
    workflow: { runId: "200", runAttempt: "1" },
    mutation: { status: "succeeded", applied: true },
    verification: {
      status: "passed",
      codes: [],
      image: {
        id: desired.imageId,
        provider: "aws",
        source: "promoted",
        region: "eu-west-1",
        revision: "revision-1",
      },
    },
    cleanup: { status: "passed" },
    createdAt: "2026-08-21T00:00:00.000Z",
    updatedAt: "2026-08-21T00:10:00.000Z",
  };
}

test(
  "publishes a real signed receipt only to a localhost OCI registry",
  { skip: available ? false : "set CRABBOX_ORAS and CRABBOX_COSIGN" },
  async () => {
    const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-promotion-receipt-oci-"));
    const registry = await startRegistry();
    try {
      const bin = await wrappers(root);
      const material = await sigstoreMaterial(root);
      const attemptFile = path.join(root, "attempt.json");
      await writeFile(attemptFile, `${canonicalJSON(attempt())}\n`);
      const env = {
        ...process.env,
        PATH: `${bin}${path.delimiter}${process.env.PATH}`,
        CRABBOX_REAL_ORAS: realOras,
        CRABBOX_REAL_COSIGN: realCosign,
        CRABBOX_TEST_REGISTRY: registry.host,
        CRABBOX_TEST_SIGSTORE_DIR: material,
        GOCACHE: path.join(root, "publisher-go-cache"),
      };
      const result = await execFileAsync(
        "bash",
        [
          publisher,
          "--attempt",
          attemptFile,
          "--repository",
          "ghcr.io/example-org/crabbox-aws-image-promotions",
          "--certificate-identity",
          identity,
        ],
        { cwd: repoRoot, env },
      );
      const publication = JSON.parse(result.stdout);
      assert.match(publication.immutableRef, /@sha256:[0-9a-f]{64}$/);
      const pulled = path.join(root, "pulled");
      await mkdir(pulled);
      await execFileAsync(
        path.join(bin, "oras"),
        ["pull", publication.immutableRef, "--output", pulled],
        { cwd: repoRoot, env },
      );
      const receipt = JSON.parse(await readFile(path.join(pulled, "promotion-receipt.json")));
      assert.equal(receipt.outcome, "promoted_verified");
      const discovered = await execFileAsync(
        path.join(bin, "oras"),
        ["discover", publication.immutableRef, "--depth", "1", "--format", "json"],
        { cwd: repoRoot, env },
      );
      assert.match(discovered.stdout, /application\/vnd\.dev\.sigstore\.bundle\.v0\.3\+json/);
    } finally {
      await stop(registry.child);
      await rm(root, { recursive: true, force: true });
    }
  },
);
