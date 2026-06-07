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

test("xcpng live smoke env file does not override existing process env", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-smoke-test-"));
  const envFile = path.join(dir, "xcpng.env");
  const evidenceDir = path.join(dir, "evidence");
  const fakeCrabbox = path.join(dir, "crabbox");

  fs.writeFileSync(
    envFile,
    "CRABBOX_XCP_NG_USERNAME=stale-env-file-user\n",
    "utf8",
  );
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env node
if (process.argv.slice(2).join(" ") !== "doctor --provider xcp-ng --json") {
  process.stderr.write("unexpected command: " + process.argv.slice(2).join(" ") + "\\n");
  process.exit(2);
}
process.stdout.write(JSON.stringify({
  username: process.env.CRABBOX_XCP_NG_USERNAME,
  status: "ok",
}) + "\\n");
`,
  );

  const result = spawnSync("bash", ["scripts/xcpng-live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      HOME: dir,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_XCP_NG_ENV_FILE: envFile,
      CRABBOX_XCP_NG_SMOKE_DIR: evidenceDir,
      CRABBOX_XCP_NG_USERNAME: "current-env-user",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /classification=read_only_doctor_passed/);

  const doctorLog = result.stdout.match(/^doctor_log=(.+)$/m)?.[1];
  assert.ok(doctorLog, `expected doctor_log in ${result.stdout}`);
  const doctorPath = path.isAbsolute(doctorLog)
    ? doctorLog
    : path.join(repoRoot, doctorLog);
  assert.match(
    fs.readFileSync(doctorPath, "utf8"),
    /current-env-user/,
  );
});

test("xcpng live smoke redacts http pool URLs in evidence", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-live-smoke-"));
  const evidence = path.join(dir, "evidence");
  const fakeCrabbox = path.join(dir, "crabbox");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == "doctor --provider xcp-ng --json" ]]; then
  printf 'doctor failed for http://private-pool.example.test/jsonrpc password=pool-secret\\n' >&2
  exit 4
fi
exit 0
`,
  );

  const result = spawnSync("bash", ["scripts/xcpng-live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_XCP_NG_API_URL: "https://private-pool.example.test/jsonrpc",
      CRABBOX_XCP_NG_SMOKE_DIR: evidence,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 3, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=environment_blocked/);

  const doctorLogs = fs
    .readdirSync(evidence)
    .filter((name) => name.endsWith("-doctor.json"));
  assert.equal(doctorLogs.length, 1);

  const doctorLog = fs.readFileSync(path.join(evidence, doctorLogs[0]), "utf8");
  assert.match(doctorLog, /https:\/\/xcp-pool\.example\.test/);
  assert.doesNotMatch(doctorLog, /private-pool/);
  assert.doesNotMatch(doctorLog, /pool-secret/);
  assert.match(doctorLog, /password=<redacted>/);
});

test("xcpng live smoke redacts configured pool URLs case-insensitively and preserves unrelated URLs", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-live-smoke-"));
  const evidence = path.join(dir, "evidence");
  const fakeCrabbox = path.join(dir, "crabbox");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == "doctor --provider xcp-ng --json" ]]; then
  printf 'doctor failed for HTTP://Private-Pool.example.test/jsonrpc password=pool-secret docs=https://docs.example.test/xcp-ng\\n' >&2
  exit 4
fi
exit 0
`,
  );

  const result = spawnSync("bash", ["scripts/xcpng-live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_XCP_NG_API_URL: "https://private-pool.example.test/jsonrpc",
      CRABBOX_XCP_NG_SMOKE_DIR: evidence,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 3, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=environment_blocked/);

  const doctorLogs = fs
    .readdirSync(evidence)
    .filter((name) => name.endsWith("-doctor.json"));
  assert.equal(doctorLogs.length, 1);

  const doctorLog = fs.readFileSync(path.join(evidence, doctorLogs[0]), "utf8");
  assert.match(doctorLog, /https:\/\/xcp-pool\.example\.test/);
  assert.match(doctorLog, /docs=https:\/\/docs\.example\.test\/xcp-ng/);
  assert.doesNotMatch(doctorLog, /Private-Pool\.example\.test/);
  assert.doesNotMatch(doctorLog, /pool-secret/);
  assert.match(doctorLog, /password=<redacted>/);
});
