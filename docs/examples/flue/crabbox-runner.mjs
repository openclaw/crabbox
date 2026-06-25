#!/usr/bin/env node
import { spawn } from "node:child_process";
import { mkdtemp, mkdir, readFile, rm } from "node:fs/promises";
import { isAbsolute, join, normalize, relative, resolve } from "node:path";
import { tmpdir } from "node:os";

const PROTOCOL_VERSION = 1;
const OPERATION = "run";
const DEFAULT_STDOUT_LIMIT = 10 * 1024 * 1024;
const DEFAULT_STDERR_LIMIT = 10 * 1024 * 1024;

main().catch((error) => {
  const message = error instanceof Error ? error.message : String(error);
  process.stdout.write(
    JSON.stringify({
      protocolVersion: PROTOCOL_VERSION,
      operation: OPERATION,
      exitCode: 1,
      stderr: `${message}\n`,
      error: message,
      timing: { totalMs: 0, runMs: 0 }
    }) + "\n"
  );
  process.exitCode = 1;
});

async function main() {
  const started = Date.now();
  const input = parseFlueInput(process.argv.slice(2));
  const request = validateRequest(JSON.parse(await readFile(input.requestFile, "utf8")));
  const limits = {
    stdoutBytes: request.outputLimits?.stdoutBytes || DEFAULT_STDOUT_LIMIT,
    stderrBytes: request.outputLimits?.stderrBytes || DEFAULT_STDERR_LIMIT
  };
  const stagingRoot = await mkdtemp(join(tmpdir(), "crabbox-flue-runner-"));
  try {
    await validateArchiveEntries(request.workspaceArchive);
    const workspace = workspacePath(stagingRoot, request.workspace);
    await mkdir(workspace, { recursive: true });
    await runChecked("tar", ["-xzf", request.workspaceArchive, "-C", workspace], {
      cwd: stagingRoot,
      env: {},
      limits
    });
    const runStarted = Date.now();
    const command = request.command;
    const result = await runCommand(command[0], command.slice(1), {
      cwd: workspace,
      env: commandEnv(request.env || {}),
      limits,
      timeoutMs: request.timeoutMs || 0
    });
    process.stdout.write(
      JSON.stringify({
        protocolVersion: PROTOCOL_VERSION,
        operation: OPERATION,
        leaseId: request.leaseId,
        slug: request.slug,
        exitCode: result.exitCode,
        stdout: result.stdout,
        stderr: result.stderr,
        timing: {
          runMs: Date.now() - runStarted,
          totalMs: Date.now() - started
        },
        error: result.error
      }) + "\n"
    );
    process.exitCode = result.exitCode === 0 ? 0 : result.exitCode;
  } finally {
    await rm(stagingRoot, { recursive: true, force: true });
  }
}

function parseFlueInput(args) {
  const inputIndex = args.indexOf("--input");
  if (inputIndex < 0 || inputIndex + 1 >= args.length) {
    throw new Error("missing --input JSON");
  }
  const input = JSON.parse(args[inputIndex + 1]);
  if (!input || typeof input.requestFile !== "string" || input.requestFile.trim() === "") {
    throw new Error("input.requestFile is required");
  }
  return input;
}

function validateRequest(request) {
  if (request.protocolVersion !== PROTOCOL_VERSION) {
    throw new Error(`unsupported protocolVersion ${request.protocolVersion}`);
  }
  if (request.operation !== OPERATION) {
    throw new Error(`unsupported operation ${request.operation}`);
  }
  if (request.target !== "node") {
    throw new Error(`unsupported target ${request.target}`);
  }
  if (typeof request.workspaceArchive !== "string" || request.workspaceArchive.trim() === "") {
    throw new Error("workspaceArchive is required");
  }
  if (typeof request.workspace !== "string" || request.workspace.trim() === "") {
    throw new Error("workspace is required");
  }
  if (!Array.isArray(request.command) || request.command.length === 0 || typeof request.command[0] !== "string") {
    throw new Error("command must be a non-empty argv array");
  }
  if (request.timeoutMs && (!Number.isInteger(request.timeoutMs) || request.timeoutMs < 0)) {
    throw new Error("timeoutMs must be non-negative");
  }
  return request;
}

async function validateArchiveEntries(archivePath) {
  const names = await runChecked("tar", ["-tzf", archivePath], {
    cwd: tmpdir(),
    env: {},
    limits: { stdoutBytes: DEFAULT_STDOUT_LIMIT, stderrBytes: DEFAULT_STDERR_LIMIT }
  });
  if (names.stdoutTruncated) {
    throw new Error("archive listing exceeded validation output limit");
  }
  for (const rawEntry of names.stdout.split("\n")) {
    const entry = rawEntry.trim();
    if (!entry) continue;
    const normalized = normalize(entry);
    if (isAbsolute(entry) || normalized === ".." || normalized.startsWith("../")) {
      throw new Error(`unsafe archive entry ${entry}`);
    }
  }

  const verbose = await runChecked("tar", ["-tvzf", archivePath], {
    cwd: tmpdir(),
    env: {},
    limits: { stdoutBytes: DEFAULT_STDOUT_LIMIT, stderrBytes: DEFAULT_STDERR_LIMIT }
  });
  if (verbose.stdoutTruncated) {
    throw new Error("archive verbose listing exceeded validation output limit");
  }
  for (const line of verbose.stdout.split("\n")) {
    const type = line.trimStart()[0];
    if (type === "l" || type === "h") {
      throw new Error("archive contains link entries");
    }
  }
}

function workspacePath(stagingRoot, requestedWorkspace) {
  const suffix = requestedWorkspace.replace(/^[/\\]+/, "");
  const workspace = resolve(stagingRoot, suffix || "workspace");
  if (relative(stagingRoot, workspace).startsWith("..")) {
    throw new Error("workspace escaped staging root");
  }
  return workspace;
}

function runChecked(command, args, options) {
  return runCommand(command, args, options).then((result) => {
    if (result.exitCode !== 0) {
      throw new Error(`${command} failed with exit ${result.exitCode}: ${result.stderr || result.stdout}`);
    }
    return result;
  });
}

function runCommand(command, args, options) {
  return new Promise((resolveRun) => {
    const child = spawn(command, args, {
      cwd: options.cwd,
      env: options.env,
      detached: process.platform !== "win32",
      stdio: ["ignore", "pipe", "pipe"]
    });
    let stdout = "";
    let stderr = "";
    let stdoutTruncated = false;
    let stderrTruncated = false;
    let timedOut = false;
    const timer =
      options.timeoutMs > 0
        ? setTimeout(() => {
            timedOut = true;
            terminateChild(child, "SIGTERM");
            setTimeout(() => {
              if (child.exitCode === null && child.signalCode === null) {
                terminateChild(child, "SIGKILL");
              }
            }, 5000).unref();
          }, options.timeoutMs)
        : null;
    child.stdout.on("data", (chunk) => {
      const next = appendLimited(stdout, chunk, options.limits.stdoutBytes);
      stdout = next.value;
      stdoutTruncated = stdoutTruncated || next.truncated;
    });
    child.stderr.on("data", (chunk) => {
      const next = appendLimited(stderr, chunk, options.limits.stderrBytes);
      stderr = next.value;
      stderrTruncated = stderrTruncated || next.truncated;
    });
    child.on("error", (error) => {
      if (timer) clearTimeout(timer);
      resolveRun({ exitCode: 1, stdout, stderr, stdoutTruncated, stderrTruncated, error: error.message });
    });
    child.on("close", (code, signal) => {
      if (timer) clearTimeout(timer);
      const exitCode = timedOut ? 124 : code || (signal ? 1 : 0);
      resolveRun({
        exitCode,
        stdout,
        stderr,
        stdoutTruncated,
        stderrTruncated,
        error: timedOut ? "command timed out" : exitCode === 0 ? "" : `command exited ${exitCode}`
      });
    });
  });
}

function terminateChild(child, signal) {
  if (process.platform !== "win32" && child.pid) {
    try {
      process.kill(-child.pid, signal);
      return;
    } catch {
      // Fall through to the direct child for platforms or launchers that do
      // not preserve the process group.
    }
  }
  child.kill(signal);
}

function commandEnv(requestEnv) {
  const env = {};
  for (const key of ["PATH", "HOME", "TMPDIR", "TEMP", "TMP"]) {
    if (process.env[key]) {
      env[key] = process.env[key];
    }
  }
  return { ...env, ...requestEnv };
}

function appendLimited(current, chunk, limit) {
  const next = current + chunk.toString("utf8");
  if (next.length <= limit) return { value: next, truncated: false };
  return { value: next.slice(0, limit), truncated: true };
}
