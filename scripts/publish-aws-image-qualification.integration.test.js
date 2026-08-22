import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { once } from "node:events";
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(import.meta.dirname, "..");
const publisher = path.join(repoRoot, "scripts", "publish-aws-image-qualification.sh");
const inputFixture = path.join(
  repoRoot,
  "scripts",
  "fixtures",
  "aws-image-qualification-input.json",
);
const registryScript = path.join(repoRoot, "scripts", "test-oci-registry.mjs");
const materialSource = path.join(repoRoot, "scripts", "generate-test-sigstore-material", "main.go");
const realOras = process.env.CRABBOX_ORAS;
const realCosign = process.env.CRABBOX_COSIGN;
const available = Boolean(realOras && realCosign);
const workflowRef =
  "example-org/crabbox/.github/workflows/devtools-image-qualify.yml@refs/heads/main";
const identity = `https://github.com/${workflowRef}`;
const issuer = "https://token.actions.githubusercontent.com";

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
  return { child, host: `${address.host}:${address.port}`, diagnostics: () => stderr };
}

async function writeWrappers(root) {
  const bin = path.join(root, "bin");
  await mkdir(bin);
  await writeFile(
    path.join(bin, "oras"),
    `#!/usr/bin/env node
const { spawnSync } = require("node:child_process");
const real = process.env.CRABBOX_REAL_ORAS;
const host = process.env.CRABBOX_TEST_REGISTRY;
let args = process.argv.slice(2).map((arg) => arg.replace(/^ghcr[.]io\\//, host + "/"));
if (args[0] === "manifest" || args[0] === "blob") args = [args[0], args[1], "--plain-http", ...args.slice(2)];
else if (args[0] !== "version") args = [args[0], "--plain-http", ...args.slice(1)];
const result = spawnSync(real, args, { encoding: "utf8", env: process.env });
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
const real = process.env.CRABBOX_REAL_COSIGN;
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
const result = spawnSync(real, args, {
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

async function generateMaterial(root) {
  const material = path.join(root, "sigstore");
  await mkdir(material);
  await execFileAsync(realCosign, ["generate-key-pair", "--output-key-prefix", path.join(material, "cosign")], {
    cwd: repoRoot,
    env: { ...process.env, COSIGN_PASSWORD: "crabbox-test-only" },
  });
  await execFileAsync(
    "go",
    [
      "run", materialSource,
      "--output", material,
      "--public-key", path.join(material, "cosign.pub"),
      "--identity", identity,
      "--issuer", issuer,
    ],
    { cwd: repoRoot, env: { ...process.env, GOCACHE: path.join(root, "go-build") } },
  );
  return material;
}

test(
  "publishes a real OCI 1.1 qualification artifact and Sigstore v0.3 referrer",
  { skip: available ? false : "set CRABBOX_ORAS and CRABBOX_COSIGN" },
  async () => {
    const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-qualification-oci-"));
    const registry = await startRegistry();
    try {
      const bin = await writeWrappers(root);
      const material = await generateMaterial(root);
      const receipt = path.join(root, "qualification.json");
      const input = path.join(root, "qualification-input.json");
      const digest = (digit) => `sha256:${digit.repeat(64)}`;
      const inputBytes = await readFile(inputFixture);
      const qualificationInput = JSON.parse(inputBytes);
      const candidateEvidence = qualificationInput.evidence.find(
        (item) => item.name === "candidate.json",
      );
      const poolIdentity = {
        schema: "crabbox-ready-pool-identity/v1",
        profile: qualificationInput.readiness.profile,
        recipeDigest: qualificationInput.readiness.recipeDigest,
        inventoryDigest: digest("4"),
        imageID: "ami-123abc",
        architecture: "amd64",
        seedDigest: digest("5"),
        cacheABIDigest: digest("6"),
      };
      const { canonicalJSON, digestBytes, digestJSON } = await import("./aws-image-candidate.mjs");
      await writeFile(input, inputBytes);
      await writeFile(receipt, `${canonicalJSON({
        schema: "crabbox-aws-image-qualification/v1",
        createdAt: "2026-08-21T00:00:00.000Z",
        candidate: {
          artifactDigest: qualificationInput.artifact.digest,
          candidateRecordDigest: candidateEvidence.digest,
          qualificationInputDigest: digestBytes(inputBytes),
        },
        target: {
          amiId: qualificationInput.target.amiId,
          region: qualificationInput.target.region,
          instanceType: qualificationInput.target.instanceType,
          architecture: qualificationInput.target.architecture,
          osSelector: "ubuntu:26.04",
          market: "on-demand",
          profile: qualificationInput.readiness.profile,
          recipeDigest: qualificationInput.readiness.recipeDigest,
        },
        pool: { identity: poolIdentity, identityDigest: digestJSON(poolIdentity) },
        gates: {
          negative: ["ami", "architecture", "recipe", "type"].map((dimension) => ({
            dimension, status: "passed",
          })),
          positive: { status: "passed" },
          osSelector: { status: "passed" },
          cleanOverlay: {
            status: "passed", mode: "overlay", fallback: false,
            trackedTransfer: { files: 0, bytes: 0 },
          },
          dirtyOverlay: {
            status: "passed", mode: "overlay", fallback: false,
            trackedTransfer: { files: 1, bytes: 40 }, fixtureDigest: digest("8"),
          },
        },
        cache: {
          advertised: false, status: "skipped",
          reason: "provider_capability_not_advertised",
        },
        cleanup: { status: "passed" },
        workflow: { identity: workflowRef, runId: "123", runAttempt: "1" },
      })}\n`);
      const env = {
        ...process.env,
        PATH: `${bin}${path.delimiter}${process.env.PATH}`,
        CRABBOX_REAL_ORAS: realOras,
        CRABBOX_REAL_COSIGN: realCosign,
        CRABBOX_TEST_REGISTRY: registry.host,
        CRABBOX_TEST_SIGSTORE_DIR: material,
      };
      const result = await execFileAsync(
        "bash",
        [
          publisher, "--receipt", receipt,
          "--qualification-input", input,
          "--repository", "ghcr.io/example-org/crabbox-aws-image-qualifications",
          "--certificate-identity", identity,
        ],
        { cwd: repoRoot, env },
      );
      const publication = JSON.parse(result.stdout);
      assert.match(publication.ociDigest, /^sha256:[0-9a-f]{64}$/);
      const pulled = path.join(root, "pulled");
      await mkdir(pulled);
      await execFileAsync(
        path.join(bin, "oras"),
        ["pull", publication.immutableRef, "--output", pulled],
        { cwd: repoRoot, env },
      );
      assert.deepEqual(await readFile(path.join(pulled, "qualification-input.json")), inputBytes);
      assert.deepEqual(
        await readFile(path.join(pulled, "qualification.json")),
        await readFile(receipt),
      );
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
