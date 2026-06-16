#!/usr/bin/env node
import { spawn, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const repoRoot = path.resolve(import.meta.dirname, "..");
const bin = process.env.CRABBOX_BIN || path.join(repoRoot, "bin", process.platform === "win32" ? "crabbox.exe" : "crabbox");
const token = process.env.CRABBOX_CODESANDBOX_API_KEY || process.env.CSB_API_KEY || "";
const slug = `codesandbox-smoke-${timestamp()}-${process.pid}`;
const smokeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-codesandbox-smoke-"));
const smokeRepo = path.join(smokeRoot, "repo");
const smokeEnv = {
  ...process.env,
  XDG_STATE_HOME: path.join(smokeRoot, "state"),
};
let cleanupArmed = false;

async function main() {
  if (!token.trim()) {
    emit("environment_blocked", "reason=missing_CRABBOX_CODESANDBOX_API_KEY_or_CSB_API_KEY");
    cleanup();
    return;
  }

  try {
    ensureBinary();
    prepareRepo();
    run("doctor", ["doctor", "--provider", "codesandbox"]);
    run("warmup", ["warmup", "--provider", "codesandbox", "--slug", slug, "--keep", "--timing-json"], { cwd: smokeRepo });
    cleanupArmed = true;
    run("status", ["status", "--provider", "codesandbox", "--id", slug, "--wait", "--wait-timeout", "300s"], { cwd: smokeRepo });
    const noSync = run("run no-sync", ["run", "--provider", "codesandbox", "--id", slug, "--no-sync", "--", "/bin/sh", "-lc", "printf CODESANDBOX_SMOKE_NOSYNC_OK"], { cwd: smokeRepo });
    requireOutput(noSync, "CODESANDBOX_SMOKE_NOSYNC_OK", "no-sync command");
    const failed = runExpectExit("run expected failure", ["run", "--provider", "codesandbox", "--id", slug, "--no-sync", "--", "/bin/sh", "-lc", "printf CODESANDBOX_SMOKE_EXIT_OK; exit 7"], 7, { cwd: smokeRepo });
    requireOutput(failed, "CODESANDBOX_SMOKE_EXIT_OK", "nonzero command");
    fs.writeFileSync(path.join(smokeRepo, "proof.txt"), "v1\n", "utf8");
    run("run sync-only", ["run", "--provider", "codesandbox", "--id", slug, "--sync-only"], { cwd: smokeRepo });
    const sync = run("run sync proof", ["run", "--provider", "codesandbox", "--id", slug, "--", "/bin/sh", "-lc", "test \"$(cat proof.txt)\" = v1 && printf CODESANDBOX_SMOKE_SYNC_OK"], { cwd: smokeRepo });
    requireOutput(sync, "CODESANDBOX_SMOKE_SYNC_OK", "sync proof command");
    run("pause", ["pause", "--provider", "codesandbox", slug], { cwd: smokeRepo });
    run("resume", ["resume", "--provider", "codesandbox", slug], { cwd: smokeRepo });
    const preview = startPreviewServer();
    try {
      await waitForPreviewStart(preview);
      const ports = run("ports", ["ports", "--provider", "codesandbox", "--id", slug, "--publish", "3000", "--json"], { cwd: smokeRepo });
      const previewURL = requirePortURL(ports.stdout);
      await requirePreviewResponse(previewURL);
    } finally {
      await stopPreviewServer(preview);
    }
    stopAndVerifyEmpty();
    emit("live_codesandbox_smoke_passed");
  } catch (error) {
    const message = error instanceof SmokeError ? error.message : String(error?.message || error);
    const classification = classify(message);
    emit(classification, redact(message));
    if (classification !== "environment_blocked") {
      process.exitCode = 1;
    }
  } finally {
    cleanup();
  }
}

function ensureBinary() {
  if (fs.existsSync(bin)) return;
  fs.mkdirSync(path.dirname(bin), { recursive: true });
  const result = spawnSync("go", ["build", "-trimpath", "-o", bin, "./cmd/crabbox"], {
    cwd: repoRoot,
    env: smokeEnv,
    encoding: "utf8",
  });
  if (result.status !== 0) {
    throw new SmokeError("go build failed", result);
  }
}

function prepareRepo() {
  fs.mkdirSync(smokeRepo, { recursive: true });
  fs.writeFileSync(path.join(smokeRepo, ".crabbox.yaml"), "provider: codesandbox\n", "utf8");
  fs.writeFileSync(path.join(smokeRepo, "proof.txt"), "seed\n", "utf8");
  runLocal("git init -q", ["git", "init", "-q"], smokeRepo);
  runLocal("git config user.email", ["git", "config", "user.email", "smoke@example.com"], smokeRepo);
  runLocal("git config user.name", ["git", "config", "user.name", "Crabbox CodeSandbox Smoke"], smokeRepo);
  runLocal("git add", ["git", "add", ".crabbox.yaml", "proof.txt"], smokeRepo);
  runLocal("git commit", ["git", "commit", "-qm", "test: seed CodeSandbox smoke fixture"], smokeRepo);
}

function run(label, args, options = {}) {
  const result = spawnSync(bin, args, {
    cwd: options.cwd || repoRoot,
    env: smokeEnv,
    encoding: "utf8",
    maxBuffer: 1024 * 1024 * 8,
  });
  if (result.status !== 0) {
    throw new SmokeError(`${label} failed`, result);
  }
  return {
    stdout: result.stdout || "",
    stderr: result.stderr || "",
  };
}

function runExpectExit(label, args, expectedStatus, options = {}) {
  const result = spawnSync(bin, args, {
    cwd: options.cwd || repoRoot,
    env: smokeEnv,
    encoding: "utf8",
    maxBuffer: 1024 * 1024 * 8,
  });
  if (result.status !== expectedStatus) {
    throw new SmokeError(`${label} returned ${result.status}, want ${expectedStatus}`, result);
  }
  return {
    stdout: result.stdout || "",
    stderr: result.stderr || "",
  };
}

function runLocal(label, args, cwd) {
  const result = spawnSync(args[0], args.slice(1), {
    cwd,
    env: smokeEnv,
    encoding: "utf8",
  });
  if (result.status !== 0) {
    throw new SmokeError(`${label} failed`, result);
  }
}

function startPreviewServer() {
  const child = spawn(
    bin,
    [
      "run",
      "--provider",
      "codesandbox",
      "--id",
      slug,
      "--no-sync",
      "--",
      "/bin/sh",
      "-lc",
      "exec node -e 'require(\"http\").createServer((_,res)=>res.end(\"CODESANDBOX_SMOKE_PORT_OK\")).listen(3000,\"0.0.0.0\")'",
    ],
    {
      cwd: smokeRepo,
      env: smokeEnv,
      detached: process.platform !== "win32",
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  const preview = { child, stdout: "", stderr: "", spawnError: null };
  child.stdout.on("data", (chunk) => {
    preview.stdout = appendOutput(preview.stdout, chunk);
  });
  child.stderr.on("data", (chunk) => {
    preview.stderr = appendOutput(preview.stderr, chunk);
  });
  child.on("error", (error) => {
    preview.spawnError = error;
  });
  return preview;
}

async function waitForPreviewStart(preview) {
  await delay(5000);
  if (preview.spawnError) {
    throw new Error(`start preview server failed: ${preview.spawnError.message}`);
  }
  if (preview.child.exitCode !== null || preview.child.signalCode !== null) {
    throw new SmokeError("start preview server exited early", previewResult(preview));
  }
}

async function stopPreviewServer(preview) {
  if (!preview || preview.child.exitCode !== null || preview.child.signalCode !== null) return;
  signalChild(preview.child, "SIGTERM");
  await waitForExit(preview.child, 5000);
  if (preview.child.exitCode === null && preview.child.signalCode === null) {
    signalChild(preview.child, "SIGKILL");
    await waitForExit(preview.child, 5000);
  }
}

function signalChild(child, signal) {
  try {
    if (process.platform === "win32") {
      child.kill(signal);
    } else {
      process.kill(-child.pid, signal);
    }
  } catch (error) {
    if (error?.code !== "ESRCH") throw error;
  }
}

function waitForExit(child, timeoutMs) {
  if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve();
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, timeoutMs);
    child.once("exit", () => {
      clearTimeout(timer);
      resolve();
    });
  });
}

function delay(timeoutMs) {
  return new Promise((resolve) => setTimeout(resolve, timeoutMs));
}

function appendOutput(current, chunk) {
  return `${current}${chunk}`.slice(-1024 * 1024);
}

function previewResult(preview) {
  return {
    status: preview.child.exitCode,
    stdout: preview.stdout,
    stderr: preview.stderr || (preview.child.signalCode ? `signal=${preview.child.signalCode}` : ""),
  };
}

function stopAndVerifyEmpty() {
  run("stop", ["stop", "--provider", "codesandbox", slug], { cwd: smokeRepo });
  cleanupArmed = false;
  const listed = run("list", ["list", "--provider", "codesandbox", "--json"], { cwd: smokeRepo });
  if (jsonContainsSlug(listed.stdout, slug)) {
    throw new Error(`cleanup_failed: final list still contains smoke slug ${slug}`);
  }
}

function cleanup() {
  if (cleanupArmed && fs.existsSync(bin)) {
    spawnSync(bin, ["stop", "--provider", "codesandbox", slug], {
      cwd: fs.existsSync(smokeRepo) ? smokeRepo : repoRoot,
      env: smokeEnv,
      encoding: "utf8",
      maxBuffer: 1024 * 1024,
    });
    cleanupArmed = false;
  }
  fs.rmSync(smokeRoot, { recursive: true, force: true });
}

function requireOutput(result, sentinel, label) {
  if (!`${result.stdout}\n${result.stderr}`.includes(sentinel)) {
    throw new Error(`diagnostic_only: ${label} did not print ${sentinel}`);
  }
}

function requirePortURL(stdout) {
  let payload;
  try {
    payload = JSON.parse(stdout);
  } catch (error) {
    throw new Error(`diagnostic_only: ports output was not JSON: ${error.message}`);
  }
  if (!Array.isArray(payload) || payload.length !== 1) {
    throw new Error("diagnostic_only: ports output did not contain exactly one port");
  }
  const item = payload[0];
  const host = String(item.host || item.url || "");
  if (item.port !== 3000 || !/^https:\/\/.+\.csb\.app/.test(host)) {
    throw new Error(`diagnostic_only: ports output did not include a CodeSandbox URL for port 3000: ${JSON.stringify(payload)}`);
  }
  return host;
}

async function requirePreviewResponse(url) {
  let lastError;
  for (let attempt = 0; attempt < 10; attempt++) {
    try {
      const response = await fetch(url, { signal: AbortSignal.timeout(10000) });
      const body = await response.text();
      if (response.ok && body.includes("CODESANDBOX_SMOKE_PORT_OK")) return;
      lastError = new Error(`HTTP ${response.status}: ${body.slice(0, 200)}`);
    } catch (error) {
      lastError = error;
    }
    await delay(2000);
  }
  throw new Error(`diagnostic_only: CodeSandbox preview request failed: ${lastError?.message || "unknown error"}`);
}

function jsonContainsSlug(text, want) {
  let value;
  try {
    value = JSON.parse(text);
  } catch {
    return true;
  }
  return hasSlug(value, want);
}

function hasSlug(value, want) {
  if (Array.isArray(value)) return value.some((item) => hasSlug(item, want));
  if (!value || typeof value !== "object") return false;
  if (value.slug === want || value.id === want || value.name === want || value.leaseId === want) return true;
  if (value.labels && typeof value.labels === "object" && value.labels.slug === want) return true;
  return Object.values(value).some((item) => hasSlug(item, want));
}

function classify(message) {
  const lower = message.toLowerCase();
  if (lower.includes("diagnostic_only:")) {
    return "diagnostic_only";
  }
  if (lower.includes("quota") || lower.includes("rate limit") || lower.includes("too many requests") || lower.includes("429") || lower.includes("capacity")) {
    return "environment_blocked";
  }
  if (lower.includes("api key") || lower.includes("auth") || lower.includes("forbidden") || lower.includes("unauthorized") || lower.includes("timeout") || lower.includes("enotfound") || lower.includes("econn")) {
    return "environment_blocked";
  }
  return lower.includes("cleanup_failed") ? "diagnostic_only" : "diagnostic_only";
}

function emit(classification, details = "") {
  if (details) {
    console.log(`classification=${classification} ${details}`);
  } else {
    console.log(`classification=${classification}`);
  }
}

function redact(text) {
  let value = String(text || "");
  for (const secret of [process.env.CRABBOX_CODESANDBOX_API_KEY, process.env.CSB_API_KEY]) {
    if (secret) value = value.split(secret).join("[redacted]");
  }
  return value.replace(/\s+/g, " ").trim();
}

function timestamp() {
  const now = new Date();
  const pad = (value) => String(value).padStart(2, "0");
  return `${now.getUTCFullYear()}${pad(now.getUTCMonth() + 1)}${pad(now.getUTCDate())}${pad(now.getUTCHours())}${pad(now.getUTCMinutes())}${pad(now.getUTCSeconds())}`;
}

class SmokeError extends Error {
  constructor(message, result) {
    const output = `${result?.stdout || ""}${result?.stderr || ""}`.trim();
    super(`${message}: exit=${result?.status ?? "unknown"} ${output}`);
  }
}

export { classify };

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
