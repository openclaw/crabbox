import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..");

function writeExecutable(file, body) {
  fs.writeFileSync(file, body, "utf8");
  fs.chmodSync(file, 0o755);
}

function makeHelper(dir, behavior) {
  const helper = path.join(dir, "helper");
  writeExecutable(
    helper,
    `#!/usr/bin/env node
const fs = require("node:fs");
const path = require("node:path");
const args = process.argv.slice(2);
const get = (flag) => {
  const index = args.indexOf(flag);
  return index >= 0 ? args[index + 1] : "";
};
const summaryPath = get("--summary");
const summary = (${behavior})(args, process.env);
if (summaryPath) {
  fs.mkdirSync(path.dirname(summaryPath), { recursive: true });
  fs.writeFileSync(summaryPath, JSON.stringify(summary, null, 2) + String.fromCharCode(10), "utf8");
}
process.stdout.write(JSON.stringify(summary) + String.fromCharCode(10));
process.exit(Number(summary.exit_code || 0));
`,
  );
  return helper;
}

test("xcpng iso e2e smoke prints help with stable contract", () => {
  const result = spawnSync("bash", ["scripts/xcpng-iso-e2e-smoke.sh", "--help"], {
    cwd: repoRoot,
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /--read-only\|--mutate/);
  assert.match(result.stdout, /--os linux\|windows/);
  assert.match(result.stdout, /--iso <path-or-vdi>/);
  assert.match(result.stdout, /CRABBOX_XCP_NG_ISO_E2E_MUTATE=1/);
});

test("xcpng iso e2e smoke rejects password-like arguments", () => {
  const result = spawnSync(
    "bash",
    ["scripts/xcpng-iso-e2e-smoke.sh", "--read-only", "--os", "linux", "--iso", "secret.iso", "--password=bad"],
    { cwd: repoRoot, encoding: "utf8" },
  );

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(result.stderr, /refusing password-like argument/);
});

test("xcpng iso e2e smoke env file does not override existing process env", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-iso-e2e-"));
  const envFile = path.join(dir, "xcpng.env");
  const evidenceDir = path.join(dir, "evidence");
  const helper = makeHelper(dir, `(args, env) => ({
    classification: "read_only_passed",
    mutation: false,
    os: args[args.indexOf("--os") + 1],
    iso: args[args.indexOf("--iso") + 1],
    phase: "read_only_validation",
    cleanup: "not_needed",
    details: { username: env.CRABBOX_XCP_NG_USERNAME },
    evidence: {}
  })`);

  fs.writeFileSync(envFile, "CRABBOX_XCP_NG_USERNAME=stale-env-file-user\n", "utf8");

  const result = spawnSync(
    "bash",
    ["scripts/xcpng-iso-e2e-smoke.sh", "--read-only", "--os", "linux", "--iso", "ubuntu.iso"],
    {
      cwd: repoRoot,
      env: {
        ...process.env,
        CRABBOX_XCP_NG_ENV_FILE: envFile,
        CRABBOX_XCP_NG_ISO_E2E_DIR: evidenceDir,
        CRABBOX_XCP_NG_ISO_E2E_HELPER: helper,
        CRABBOX_XCP_NG_USERNAME: "current-env-user",
      },
      encoding: "utf8",
    },
  );

  assert.equal(result.status, 0, result.stdout + result.stderr);
  const summary = JSON.parse(result.stdout);
  assert.equal(summary.classification, "read_only_passed");
  assert.equal(summary.details.username, "<redacted>");

  const stdoutFiles = fs.readdirSync(evidenceDir).filter((name) => name.endsWith("-stdout.log"));
  assert.equal(stdoutFiles.length, 1);
  const stdoutLog = fs.readFileSync(path.join(evidenceDir, stdoutFiles[0]), "utf8");
  assert.doesNotMatch(stdoutLog, /stale-env-file-user/);
});

test("xcpng iso e2e smoke requires mutation gate for mutate mode", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-iso-e2e-"));
  const evidenceDir = path.join(dir, "evidence");
  const helper = makeHelper(dir, `(args, env) => ({
    classification: env.CRABBOX_XCP_NG_ISO_E2E_MUTATE === "1" ? "live_boot_passed" : "environment_blocked",
    mutation: true,
    os: args[args.indexOf("--os") + 1],
    iso: args[args.indexOf("--iso") + 1],
    phase: env.CRABBOX_XCP_NG_ISO_E2E_MUTATE === "1" ? "booted_installer" : "gate",
    cleanup: env.CRABBOX_XCP_NG_ISO_E2E_MUTATE === "1" ? "cleaned" : "not_started",
    reason: env.CRABBOX_XCP_NG_ISO_E2E_MUTATE === "1" ? undefined : "mutation_gate_missing",
    evidence: {},
    exit_code: env.CRABBOX_XCP_NG_ISO_E2E_MUTATE === "1" ? 0 : 3
  })`);

  const result = spawnSync(
    "bash",
    ["scripts/xcpng-iso-e2e-smoke.sh", "--mutate", "--os", "linux", "--iso", "ubuntu.iso"],
    {
      cwd: repoRoot,
      env: {
        ...process.env,
        CRABBOX_XCP_NG_ISO_E2E_DIR: evidenceDir,
        CRABBOX_XCP_NG_ISO_E2E_HELPER: helper,
      },
      encoding: "utf8",
    },
  );

  assert.equal(result.status, 3, result.stdout + result.stderr);
  const summary = JSON.parse(result.stdout);
  assert.equal(summary.classification, "environment_blocked");
  assert.equal(summary.reason, "mutation_gate_missing");
});

test("xcpng iso e2e smoke redacts secrets from helper output files", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-iso-e2e-"));
  const evidenceDir = path.join(dir, "evidence");
  const helper = path.join(dir, "helper");
  writeExecutable(
    helper,
    `#!/usr/bin/env bash
set -euo pipefail
summary=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --summary)
      shift
      summary="$1"
      ;;
  esac
  shift
done
cat >"$summary" <<'JSON'
{"classification":"environment_blocked","mutation":false,"os":"windows","iso":"Win11.iso","phase":"installer_iso","cleanup":"not_needed","reason":"password=super-secret session_id=OpaqueRef:secret https://api-user:api-password@private-pool.example.test/jsonrpc","evidence":{}}
JSON
printf '{"classification":"environment_blocked","mutation":false,"os":"windows","iso":"Win11.iso","phase":"installer_iso","cleanup":"not_needed","reason":"password=super-secret session_id=OpaqueRef:secret https://api-user:api-password@private-pool.example.test/jsonrpc","evidence":{}}\n'
exit 3
`,
  );

  const result = spawnSync(
    "bash",
    ["scripts/xcpng-iso-e2e-smoke.sh", "--read-only", "--os", "windows", "--iso", "Win11.iso"],
    {
      cwd: repoRoot,
      env: {
        ...process.env,
        CRABBOX_XCP_NG_API_URL: "https://private-pool.example.test/jsonrpc",
        CRABBOX_XCP_NG_ISO_E2E_DIR: evidenceDir,
        CRABBOX_XCP_NG_ISO_E2E_HELPER: helper,
      },
      encoding: "utf8",
    },
  );

  assert.equal(result.status, 3, result.stdout + result.stderr);
  const stdoutFiles = fs.readdirSync(evidenceDir).filter((name) => name.endsWith("-stdout.log"));
  const summaryFiles = fs.readdirSync(evidenceDir).filter((name) => name.endsWith("-summary.json"));
  assert.equal(stdoutFiles.length, 1);
  assert.equal(summaryFiles.length, 1);
  const stdoutLog = fs.readFileSync(path.join(evidenceDir, stdoutFiles[0]), "utf8");
  const summaryLog = fs.readFileSync(path.join(evidenceDir, summaryFiles[0]), "utf8");
  for (const text of [stdoutLog, summaryLog]) {
    assert.doesNotMatch(text, /super-secret/);
    assert.doesNotMatch(text, /private-pool/);
    assert.doesNotMatch(text, /api-user/);
    assert.doesNotMatch(text, /api-password/);
    assert.match(text, /<redacted>/);
    assert.match(text, /https:\/\/xcp-pool\.example\.test/);
  }
});

test("xcpng iso e2e smoke emits required summary keys", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-iso-e2e-"));
  const evidenceDir = path.join(dir, "evidence");
  const helper = makeHelper(dir, `(args) => ({
    classification: "read_only_passed",
    mutation: false,
    os: args[args.indexOf("--os") + 1],
    iso: args[args.indexOf("--iso") + 1],
    phase: "read_only_validation",
    cleanup: "not_needed",
    evidence: { helper: "ok" }
  })`);

  const result = spawnSync(
    "bash",
    ["scripts/xcpng-iso-e2e-smoke.sh", "--read-only", "--os", "linux", "--iso", "ubuntu.iso"],
    {
      cwd: repoRoot,
      env: {
        ...process.env,
        CRABBOX_XCP_NG_ISO_E2E_DIR: evidenceDir,
        CRABBOX_XCP_NG_ISO_E2E_HELPER: helper,
      },
      encoding: "utf8",
    },
  );

  assert.equal(result.status, 0, result.stdout + result.stderr);
  const summary = JSON.parse(result.stdout);
  for (const key of ["classification", "mutation", "os", "iso", "phase", "cleanup", "evidence"]) {
    assert.ok(key in summary, `missing ${key}: ${result.stdout}`);
  }
});

test("xcpng iso e2e smoke preserves repeated run artifacts", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-iso-e2e-"));
  const evidenceDir = path.join(dir, "evidence");
  const helper = makeHelper(dir, `(args) => ({
    classification: "read_only_passed",
    mutation: false,
    os: args[args.indexOf("--os") + 1],
    iso: args[args.indexOf("--iso") + 1],
    phase: "read_only_validation",
    cleanup: "not_needed",
    evidence: {}
  })`);
  const options = {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_XCP_NG_ISO_E2E_DIR: evidenceDir,
      CRABBOX_XCP_NG_ISO_E2E_HELPER: helper,
    },
    encoding: "utf8",
  };

  for (let run = 0; run < 2; run += 1) {
    const result = spawnSync(
      "bash",
      ["scripts/xcpng-iso-e2e-smoke.sh", "--read-only", "--os", "linux", "--iso", "ubuntu.iso"],
      options,
    );
    assert.equal(result.status, 0, result.stdout + result.stderr);
  }

  const files = fs.readdirSync(evidenceDir);
  assert.equal(files.filter((name) => name.endsWith("-summary.json")).length, 2);
  assert.equal(files.filter((name) => name.endsWith("-stdout.log")).length, 2);
  assert.equal(files.filter((name) => name.endsWith("-stderr.log")).length, 2);
});
