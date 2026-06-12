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

test("xcpng live smoke env file fills empty exported process env", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-smoke-test-"));
  const envFile = path.join(dir, "xcpng.env");
  const evidenceDir = path.join(dir, "evidence");
  const fakeCrabbox = path.join(dir, "crabbox");

  fs.writeFileSync(
    envFile,
    "CRABBOX_XCP_NG_USERNAME=file-user\n",
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
      CRABBOX_XCP_NG_USERNAME: "",
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
  assert.match(fs.readFileSync(doctorPath, "utf8"), /file-user/);
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

test("xcpng live smoke redacts configured pool URLs with userinfo in evidence", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-live-smoke-"));
  const evidence = path.join(dir, "evidence");
  const fakeCrabbox = path.join(dir, "crabbox");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == "doctor --provider xcp-ng --json" ]]; then
  printf 'doctor failed for https://api-user:api-password@private-pool.example.test/jsonrpc password=pool-secret\\n' >&2
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
      CRABBOX_XCP_NG_API_URL: "https://api-user:api-password@private-pool.example.test/jsonrpc",
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
  for (const secret of ["private-pool", "api-user", "api-password", "pool-secret"]) {
    assert.doesNotMatch(doctorLog, new RegExp(secret));
  }
  assert.match(doctorLog, /password=<redacted>/);
});

test("xcpng live smoke redacts configured pool URLs loaded from config file", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-live-smoke-"));
  const evidence = path.join(dir, "evidence");
  const fakeCrabbox = path.join(dir, "crabbox");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  "config show --json")
    printf '{"xcpNg":{"apiUrl":"https://private-pool.example.test/jsonrpc"}}\\n'
    ;;
  "doctor --provider xcp-ng --json")
    printf 'doctor failed for https://private-pool.example.test/jsonrpc password=pool-secret\\n' >&2
    exit 4
    ;;
  *)
    printf 'unexpected command: %s\\n' "$*" >&2
    exit 2
    ;;
esac
`,
  );

  const env = { ...process.env };
  delete env.CRABBOX_XCP_NG_API_URL;
  const result = spawnSync("bash", ["scripts/xcpng-live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...env,
      CRABBOX_BIN: fakeCrabbox,
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

test("xcpng live smoke does not export diagnostic-redacted config URL", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-live-smoke-"));
  const evidence = path.join(dir, "evidence");
  const fakeCrabbox = path.join(dir, "crabbox");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  "config show --json")
    printf '{"xcpNg":{"apiUrl":"<redacted>@private-pool.example.test/jsonrpc"}}\\n'
    ;;
  "doctor --provider xcp-ng --json")
    if [[ -n "\${CRABBOX_XCP_NG_API_URL:-}" ]]; then
      printf 'unexpected exported api url: %s\\n' "$CRABBOX_XCP_NG_API_URL" >&2
      exit 2
    fi
    printf 'doctor failed for https://private-pool.example.test/jsonrpc password=pool-secret\\n' >&2
    exit 4
    ;;
  *)
    printf 'unexpected command: %s\\n' "$*" >&2
    exit 2
    ;;
esac
`,
  );

  const env = { ...process.env };
  delete env.CRABBOX_XCP_NG_API_URL;
  const result = spawnSync("bash", ["scripts/xcpng-live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_XCP_NG_SMOKE_DIR: evidence,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 3, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=environment_blocked/);
  assert.doesNotMatch(result.stderr, /unexpected exported api url/);

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

test("xcpng live smoke classifies stop failures after a mutating run", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-live-smoke-"));
  const evidence = path.join(dir, "evidence");
  const fakeCrabbox = path.join(dir, "crabbox");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  "doctor --provider xcp-ng --json")
    printf '{"status":"ok"}\\n'
    ;;
  "warmup --provider xcp-ng --keep --slug xcp-ng-live-smoke --timing-json")
    printf 'leased cbx_abcdef123456 slug=xcp-ng-live-smoke\\n'
    ;;
  "run --provider xcp-ng --id cbx_abcdef123456 --no-sync -- echo xcp-ng-ok")
    printf 'xcp-ng-ok\\n'
    ;;
  "stop --provider xcp-ng cbx_abcdef123456")
    printf 'stop failed password=stop-secret\\n' >&2
    exit 4
    ;;
  *)
    printf 'unexpected command: %s\\n' "$*" >&2
    exit 2
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/xcpng-live-smoke.sh", "--mutate"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_XCP_NG_LIVE_MUTATE: "1",
      CRABBOX_XCP_NG_SMOKE_DIR: evidence,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 3, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=environment_blocked/);
  assert.match(result.stdout, /reason=stop_failed/);
  assert.match(result.stdout, /^stop_log=.+-stop\.log$/m);

  const stopLogs = fs
    .readdirSync(evidence)
    .filter((name) => name.endsWith("-stop.log"));
  assert.equal(stopLogs.length, 1);

  const stopLog = fs.readFileSync(path.join(evidence, stopLogs[0]), "utf8");
  assert.match(stopLog, /password=<redacted>/);
  assert.doesNotMatch(stopLog, /stop-secret/);
});
