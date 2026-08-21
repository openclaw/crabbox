import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { once } from "node:events";
import {
  access,
  chmod,
  mkdir,
  mkdtemp,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import { promisify } from "node:util";
import { canonicalJSON, digestJSON } from "./aws-image-candidate.mjs";

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(import.meta.dirname, "..");
const consumer = path.join(repoRoot, "scripts", "consume-aws-image-candidate.sh");
const candidateScript = path.join(repoRoot, "scripts", "aws-image-candidate.mjs");
const registryScript = path.join(repoRoot, "scripts", "test-oci-registry.mjs");
const materialSource = path.join(
  repoRoot,
  "scripts",
  "generate-test-sigstore-material",
  "main.go",
);
const realOras = process.env.CRABBOX_ORAS;
const realCosign = process.env.CRABBOX_COSIGN;
const integrationAvailable = Boolean(realOras && realCosign);
const sourceRepository = "example-org/crabbox";
const sourceCommit = (
  await execFileAsync("git", ["rev-parse", "HEAD"], { cwd: repoRoot })
).stdout.trim();
const workflowRef =
  "example-org/crabbox/.github/workflows/devtools-image-publish.yml@refs/heads/main";
const workflowIdentity = `https://github.com/${workflowRef}`;
const githubIssuer = "https://token.actions.githubusercontent.com";

async function run(file, args, options = {}) {
  try {
    const result = await execFileAsync(file, args, {
      cwd: repoRoot,
      ...options,
    });
    return { code: 0, ...result };
  } catch (error) {
    return {
      code: error.code,
      stdout: error.stdout ?? "",
      stderr: error.stderr ?? "",
    };
  }
}

async function stopChild(child) {
  if (child.pid === undefined || child.exitCode !== null || child.signalCode !== null) return;
  const closed = once(child, "close");
  child.kill("SIGTERM");
  const stopped = await Promise.race([
    closed.then(() => true),
    new Promise((resolve) => setTimeout(() => resolve(false), 2000)),
  ]);
  if (stopped || child.exitCode !== null || child.signalCode !== null) return;
  const killed = once(child, "close");
  child.kill("SIGKILL");
  const killedInTime = await Promise.race([
    killed.then(() => true),
    new Promise((resolve) => setTimeout(() => resolve(false), 2000)),
  ]);
  if (!killedInTime) throw new Error(`registry child ${child.pid} did not stop`);
}

async function startRegistry(options = {}) {
  const child = spawn(options.command ?? process.execPath, options.args ?? [registryScript], {
    cwd: repoRoot,
    stdio: ["ignore", "pipe", "pipe"],
  });
  options.onSpawn?.(child);
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
  let address;
  try {
    address = await new Promise((resolve, reject) => {
      let settled = false;
      const finish = (callback, value) => {
        if (settled) return;
        settled = true;
        clearTimeout(timeout);
        child.stdout.off("data", inspect);
        child.off("error", failStart);
        child.off("exit", failExit);
        callback(value);
      };
      const inspect = () => {
        const newline = stdout.indexOf("\n");
        if (newline < 0) return;
        try {
          const parsed = JSON.parse(stdout.slice(0, newline));
          if (
            parsed?.host !== "127.0.0.1" ||
            !Number.isInteger(parsed.port) ||
            parsed.port <= 0
          ) {
            throw new Error("registry emitted an invalid localhost address");
          }
          finish(resolve, parsed);
        } catch (error) {
          finish(reject, error);
        }
      };
      const failStart = (error) => finish(reject, error);
      const failExit = (code) =>
        finish(reject, new Error(`registry exited ${code}: ${stderr}`));
      const timeout = setTimeout(
        () => finish(reject, new Error(`registry start timed out: ${stderr}`)),
        options.timeoutMs ?? 5000,
      );
      child.stdout.on("data", inspect);
      child.once("error", failStart);
      child.once("exit", failExit);
    });
  } catch (error) {
    await stopChild(child);
    throw error;
  }
  return {
    host: `${address.host}:${address.port}`,
    diagnostics() {
      return stderr;
    },
    async stop() {
      await stopChild(child);
    },
  };
}

async function withRegistryFixture(callback, options = {}) {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-oci-integration-"));
  options.onRoot?.(root);
  let registry;
  try {
    registry = await startRegistry(options.registry);
    return await callback(root, registry);
  } finally {
    await registry?.stop();
    await rm(root, { recursive: true, force: true });
  }
}

async function writeWrappers(root) {
  const bin = path.join(root, "bin");
  await mkdir(bin);
  await writeFile(
    path.join(bin, "oras"),
    `#!/usr/bin/env node
const fs = require("node:fs");
const { spawnSync } = require("node:child_process");
const path = require("node:path");
const real = process.env.CRABBOX_REAL_ORAS;
const host = process.env.CRABBOX_TEST_REGISTRY;
let args = process.argv.slice(2).map((arg) =>
  arg.replace(/^ghcr[.]io\\//, host + "/")
);
if (args[0] === "manifest" || args[0] === "blob") {
  args = [args[0], args[1], "--plain-http", ...args.slice(2)];
} else if (args[0] !== "version") {
  args = [args[0], "--plain-http", ...args.slice(1)];
}
const result = spawnSync(real, args, { encoding: "utf8", env: process.env });
if (result.error) throw result.error;
const restoreLogicalRegistry = (value) =>
  (value || "").split(host + "/").join("ghcr.io/");
const stdout = restoreLogicalRegistry(result.stdout);
if (process.env.CRABBOX_TEST_ORAS_LOG && stdout) {
  fs.appendFileSync(process.env.CRABBOX_TEST_ORAS_LOG, stdout + "\\n");
}
process.stdout.write(stdout);
process.stderr.write(restoreLogicalRegistry(result.stderr));
if (result.status !== 0) process.exit(result.status ?? 1);
if (args[0] === "blob" && args[1] === "fetch") {
  const output = args[args.indexOf("--output") + 1];
  if (process.env.CRABBOX_TEST_TAMPER_BUNDLE === "bytes") {
    fs.appendFileSync(output, " ");
  }
  if (process.env.CRABBOX_TEST_TAMPER_BUNDLE === "signature") {
    const value = JSON.parse(fs.readFileSync(output, "utf8"));
    let changed = false;
    const visit = (node) => {
      if (!node || typeof node !== "object" || changed) return;
      for (const [key, item] of Object.entries(node)) {
        if ((key === "sig" || key === "signature") && typeof item === "string" && item.length > 3) {
          node[key] = (item[0] === "A" ? "B" : "A") + item.slice(1);
          changed = true;
          return;
        }
        visit(item);
      }
    };
    visit(value);
    if (!changed) throw new Error("test bundle did not contain a signature");
    fs.writeFileSync(output, JSON.stringify(value));
  }
}
`,
  );
  await writeFile(
    path.join(bin, "cosign"),
    `#!/usr/bin/env node
const fs = require("node:fs");
const { spawnSync } = require("node:child_process");
const path = require("node:path");
const real = process.env.CRABBOX_REAL_COSIGN;
const host = process.env.CRABBOX_TEST_REGISTRY;
const material = process.env.CRABBOX_TEST_SIGSTORE_DIR;
let args = process.argv.slice(2).map((arg) =>
  arg.replace(/^ghcr[.]io\\//, host + "/")
);
if (process.env.CRABBOX_TEST_COSIGN_LOG) {
  fs.appendFileSync(process.env.CRABBOX_TEST_COSIGN_LOG, args.join("\\t") + "\\n");
}
if (args[0] === "sign") {
  args = [
    "sign",
    "--allow-http-registry",
    "--key", path.join(material, "cosign.key"),
    "--certificate", path.join(material, "cosign.crt"),
    "--certificate-chain", path.join(material, "chain.crt"),
    "--tlog-upload=false",
    ...args.slice(1),
  ];
} else if (args[0] === "verify" || args[0] === "verify-blob") {
  args = [
    args[0],
    ...(args[0] === "verify" ? ["--allow-http-registry"] : []),
    "--trusted-root", path.join(material, "trusted-root.json"),
    "--insecure-ignore-tlog",
    "--insecure-ignore-sct",
    ...args.slice(1),
  ];
}
const result = spawnSync(real, args, {
  stdio: "inherit",
  env: {
    ...process.env,
    COSIGN_EXPERIMENTAL: "1",
    COSIGN_PASSWORD: "crabbox-test-only",
  },
});
if (result.error) throw result.error;
process.exit(result.status ?? 1);
`,
  );
  await chmod(path.join(bin, "oras"), 0o755);
  await chmod(path.join(bin, "cosign"), 0o755);
  return bin;
}

function processExists(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    if (error.code === "ESRCH") return false;
    throw error;
  }
}

async function assertStartupCleanup(registryOptions, errorPattern) {
  let root;
  let pid;
  await assert.rejects(
    withRegistryFixture(
      async () => {
        assert.fail("registry callback must not run after startup failure");
      },
      {
        onRoot(value) {
          root = value;
        },
        registry: {
          ...registryOptions,
          onSpawn(child) {
            pid = child.pid;
          },
        },
      },
    ),
    errorPattern,
  );
  if (pid !== undefined) assert.equal(processExists(pid), false);
  await assert.rejects(access(root), { code: "ENOENT" });
}

test("registry spawn failure cleans its temporary directory without waiting for exit", async () => {
  await assertStartupCleanup(
    {
      command: path.join(os.tmpdir(), `crabbox-missing-registry-${process.pid}`),
      args: [],
      timeoutMs: 1000,
    },
    /ENOENT/,
  );
});

test("registry startup timeout cleans its child and temporary directory", async () => {
  await assertStartupCleanup(
    {
      args: ["-e", "setInterval(() => {}, 1000)"],
      timeoutMs: 50,
    },
    /timed out/,
  );
});

test("registry startup parse failure cleans its child and temporary directory", async () => {
  await assertStartupCleanup(
    {
      args: ["-e", 'console.log("not-json"); setInterval(() => {}, 1000)'],
      timeoutMs: 1000,
    },
    /Unexpected token|JSON/,
  );
});

test("registry startup early exit cleans its child and temporary directory", async () => {
  await assertStartupCleanup(
    {
      args: ["-e", "process.exit(7)"],
      timeoutMs: 1000,
    },
    /registry exited 7/,
  );
});

async function generateMaterial(root, identity, issuer) {
  const output = path.join(root, `sigstore-${Math.random().toString(16).slice(2)}`);
  await mkdir(output);
  const key = await run(
    realCosign,
    ["generate-key-pair", "--output-key-prefix", path.join(output, "cosign")],
    {
      env: {
        ...process.env,
        COSIGN_PASSWORD: "crabbox-test-only",
      },
    },
  );
  assert.equal(key.code, 0, key.stderr);
  const result = await run(
    "go",
    [
      "run",
      materialSource,
      "--output",
      output,
      "--public-key",
      path.join(output, "cosign.pub"),
      "--identity",
      identity,
      "--issuer",
      issuer,
    ],
    {
      env: {
        ...process.env,
        GOCACHE: path.join(root, "go-build"),
      },
    },
  );
  assert.equal(result.code, 0, result.stderr);
  return output;
}

async function createBundle(root, repository) {
  const checkpoint = path.join(root, `checkpoint-${Math.random()}.json`);
  const scrub = path.join(root, `scrub-${Math.random()}.json`);
  const checks = path.join(root, `checks-${Math.random()}.json`);
  const bundle = path.join(root, `bundle-${Math.random()}`);
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
        snapshotIds: ["snap-abc123"],
      },
    }),
  );
  const scrubEvidence = {
    schema: "crabbox-aws-image-scrub/v1",
    target: "linux",
    removed: { credentials: 1, hostIdentity: 1, workspaces: 1 },
    findings: [],
  };
  await writeFile(
    scrub,
    `${canonicalJSON({
      ...scrubEvidence,
      evidenceDigest: digestJSON(scrubEvidence),
    })}\n`,
  );
  await writeFile(
    checks,
    JSON.stringify({
      schema: "crabbox-aws-image-checks/v1",
      checks: [
        { name: "source-smoke", status: "passed" },
        { name: "scrub", status: "passed", evidenceDigest: digestJSON(scrubEvidence) },
        { name: "candidate-boot", status: "passed" },
        { name: "candidate-smoke", status: "passed" },
      ],
    }),
  );
  const result = await run(process.execPath, [
    candidateScript,
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
    bundle,
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
    "--source-repository",
    sourceRepository,
    "--source-commit",
    sourceCommit,
    "--workflow-ref",
    workflowRef,
    "--workflow-run-id",
    "12345",
    "--workflow-run-attempt",
    "1",
    "--oci-repository",
    repository,
    "--created-at",
    "2026-08-21T00:00:00.000Z",
  ]);
  assert.equal(result.code, 0, result.stderr);
  return { bundle, summary: JSON.parse(result.stdout) };
}

function toolEnv(root, bin, registry, material, extra = {}) {
  return {
    ...process.env,
    PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
    CRABBOX_REAL_ORAS: realOras,
    CRABBOX_REAL_COSIGN: realCosign,
    CRABBOX_TEST_REGISTRY: registry.host,
    CRABBOX_TEST_SIGSTORE_DIR: material,
    CRABBOX_TEST_ORAS_LOG: path.join(root, "oras-output.log"),
    ...extra,
  };
}

async function publish(root, bin, registry, repository, material) {
  const candidate = await createBundle(root, repository);
  const taggedRef = `${repository}:${candidate.summary.immutableTag}`;
  const env = toolEnv(root, bin, registry, material);
  const files = [
    ["bundle.json", "application/vnd.crabbox.aws-image-evidence-manifest.v1+json"],
    ["candidate.json", "application/vnd.crabbox.aws-image-candidate.v1+json"],
    ["recipe.json", "application/json"],
    ["sbom.spdx.json", "application/spdx+json"],
    ["provenance.intoto.jsonl", "application/vnd.in-toto+json"],
    ["scrub-report.json", "application/json"],
  ];
  const pushed = await run(
    path.join(bin, "oras"),
    [
      "push",
      taggedRef,
      "--artifact-type",
      "application/vnd.crabbox.aws-image-evidence.v1",
      ...files.map(([name, mediaType]) => `${name}:${mediaType}`),
    ],
    { env, cwd: candidate.bundle },
  );
  assert.equal(pushed.code, 0, `${pushed.stderr}\nregistry:\n${registry.diagnostics()}`);
  const resolved = await run(path.join(bin, "oras"), ["resolve", taggedRef], { env });
  assert.equal(resolved.code, 0, resolved.stderr);
  const candidateRef = `${repository}@${resolved.stdout.trim()}`;
  const signed = await run(
    path.join(bin, "cosign"),
    [
      "sign",
      "--new-bundle-format",
      "--registry-referrers-mode",
      "oci-1-1",
      "--yes",
      candidateRef,
    ],
    { env },
  );
  assert.equal(signed.code, 0, signed.stderr);
  return { ...candidate, candidateRef, env };
}

function consumeArgs(candidateRef, repository, output) {
  return [
    consumer,
    "--candidate-ref",
    candidateRef,
    "--repository",
    repository,
    "--source-repository",
    sourceRepository,
    "--source-commit",
    sourceCommit,
    "--workflow-ref",
    workflowRef,
    "--workflow-run-id",
    "12345",
    "--workflow-run-attempt",
    "1",
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
    "--ami-id",
    "ami-123abc",
    "--profile",
    "linux-builder",
    "--image-recipe",
    "recipes/aws/v1/linux-devtools.json",
    "--readiness-recipe",
    "recipes/linux/v1/linux-builder.json",
    "--output",
    output,
  ];
}

test(
  "real ORAS and Cosign consume signed localhost OCI evidence and reject cryptographic tampering",
  { skip: integrationAvailable ? false : "set CRABBOX_ORAS and CRABBOX_COSIGN" },
  async () =>
    withRegistryFixture(async (root, registry) => {
      const bin = await writeWrappers(root);
      const orasVersion = await run(realOras, ["version"]);
      const cosignVersion = await run(realCosign, ["version"]);
      assert.match(orasVersion.stdout, /Version:\s+1[.]3[.]3/);
      assert.match(cosignVersion.stdout, /GitVersion:\s+v2[.]6[.]5/);

      const validMaterial = await generateMaterial(root, workflowIdentity, githubIssuer);
      const validRepository = "ghcr.io/example-org/q1-valid";
      const valid = await publish(root, bin, registry, validRepository, validMaterial);
      const output = path.join(root, "qualification-input.json");
      const success = await run("bash", consumeArgs(valid.candidateRef, validRepository, output), {
        env: valid.env,
      });
      assert.equal(
        success.code,
        0,
        `${success.stderr}\nORAS:\n${await readFile(path.join(root, "oras-output.log"), "utf8")}`,
      );
      assert.equal(JSON.parse(await readFile(output, "utf8")).artifact.reference, valid.candidateRef);

      for (const tamper of ["bytes", "signature"]) {
        const tamperedOutput = path.join(root, `tampered-${tamper}.json`);
        const result = await run(
          "bash",
          consumeArgs(valid.candidateRef, validRepository, tamperedOutput),
          {
            env: toolEnv(root, bin, registry, validMaterial, {
              CRABBOX_TEST_TAMPER_BUNDLE: tamper,
            }),
          },
        );
        assert.notEqual(result.code, 0);
        assert.match(result.stderr, /signature bundle does not match/);
        await assert.rejects(readFile(tamperedOutput));
      }

      const cryptoRepository = "ghcr.io/example-org/q1-invalid-signature";
      const cryptoValue = await publish(
        root,
        bin,
        registry,
        cryptoRepository,
        validMaterial,
      );
      const subject = cryptoValue.candidateRef.slice(cryptoValue.candidateRef.indexOf("@") + 1);
      const tamperResponse = await fetch(
        `http://${registry.host}/__test__/tamper-signature?repository=${encodeURIComponent(cryptoRepository.slice("ghcr.io/".length))}&subject=${encodeURIComponent(subject)}`,
        { method: "POST" },
      );
      assert.equal(tamperResponse.status, 200, await tamperResponse.text());
      const cryptoOutput = path.join(root, "invalid-signature.json");
      const cosignLog = path.join(root, "invalid-signature-cosign.log");
      const cryptoResult = await run(
        "bash",
        consumeArgs(cryptoValue.candidateRef, cryptoRepository, cryptoOutput),
        {
          env: toolEnv(root, bin, registry, validMaterial, {
            CRABBOX_TEST_COSIGN_LOG: cosignLog,
          }),
        },
      );
      assert.notEqual(cryptoResult.code, 0);
      assert.match(cryptoResult.stderr, /signature|verification|verify/i);
      assert.match(await readFile(cosignLog, "utf8"), /^verify-blob\t/m);
      await assert.rejects(readFile(cryptoOutput));

      const wrongDigest = valid.candidateRef.replace(
        /sha256:[0-9a-f]{64}$/,
        `sha256:${"f".repeat(64)}`,
      );
      const digestOutput = path.join(root, "wrong-digest.json");
      const digestResult = await run(
        "bash",
        consumeArgs(wrongDigest, validRepository, digestOutput),
        { env: valid.env },
      );
      assert.notEqual(digestResult.code, 0);
      await assert.rejects(readFile(digestOutput));

      for (const failure of [
        {
          name: "identity",
          identity: "https://github.com/example-org/crabbox/.github/workflows/other.yml@refs/heads/main",
          issuer: githubIssuer,
        },
        {
          name: "issuer",
          identity: workflowIdentity,
          issuer: "https://issuer.example.invalid",
        },
      ]) {
        const material = await generateMaterial(root, failure.identity, failure.issuer);
        const repository = `ghcr.io/example-org/q1-wrong-${failure.name}`;
        const value = await publish(root, bin, registry, repository, material);
        const failureOutput = path.join(root, `wrong-${failure.name}.json`);
        const result = await run(
          "bash",
          consumeArgs(value.candidateRef, repository, failureOutput),
          { env: value.env },
        );
        assert.notEqual(result.code, 0);
        assert.match(result.stderr, /expected identities|certificate|issuer|identity/i);
        await assert.rejects(readFile(failureOutput));
      }
    }),
);
