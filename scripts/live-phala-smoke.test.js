import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");

function writeExecutable(file, body) {
  fs.writeFileSync(file, body, "utf8");
  fs.chmodSync(file, 0o755);
}

test("Phala smoke is non-mutating without live opt-in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-phala-no-live-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
printf '%s\\n' "$*" >>"${crabboxLog}"
exit 99
`,
  );

  const result = spawnSync("bash", ["scripts/live-phala-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "0",
      CRABBOX_LIVE_PROVIDERS: "phala",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^environment_blocked reason=CRABBOX_LIVE_not_enabled/m);
  assert.equal(fs.existsSync(crabboxLog), false);
});

test("Phala smoke requires explicit provider selection", () => {
  const result = spawnSync("bash", ["scripts/live-phala-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "aws",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^environment_blocked reason=provider_not_selected/m);
});

test("Phala smoke classifies an unauthenticated CLI as environment_blocked", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-phala-unauth-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
printf '%s\\n' "$*" >>"${crabboxLog}"
case "$1" in
  doctor)
    printf 'phala status failed: not logged in; run phala login\\n' >&2
    exit 1
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-phala-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "phala",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^environment_blocked .*not logged in/m);
  // The doctor probe ran; no mutating deploy was attempted.
  const calls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(calls, /doctor --provider phala/);
  assert.doesNotMatch(calls, /\brun --provider phala/);
});

test("Phala smoke accepts provider aliases", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-phala-alias-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
printf '%s\\n' "$*" >>"${crabboxLog}"
case "$1" in
  doctor)
    printf 'phala status failed: not logged in; run phala login\\n' >&2
    exit 1
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-phala-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "dstack",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^environment_blocked .*not logged in/m);
  const calls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(calls, /doctor --provider phala/);
  assert.doesNotMatch(result.stdout, /provider_not_selected/);
});

test("Phala smoke classifies an unfunded account as quota_blocked", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-phala-unfunded-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
case "$1" in
  doctor)
    printf 'phala deploy failed: insufficient balance to provision CVM\\n' >&2
    exit 1
    ;;
  *)
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-phala-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "phala",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^quota_blocked .*insufficient balance/m);
});

test("Phala smoke runs the lease lifecycle and tears down the created slug", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-phala-success-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${crabboxLog}"
case "$1" in
  doctor)
    printf 'phala ready\\n'
    ;;
  run)
    printf 'provisioned lease=cbx_smoke12345678 phala_cvm=cvm-smoke state=ready\\n' >&2
    printf 'PHALA_SMOKE_OK\\n'
    ;;
  status)
    printf 'ready\\n'
    ;;
  list)
    printf '[]\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-phala-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: path.relative(repoRoot, fakeCrabbox),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "phala",
      CRABBOX_PHALA_SMOKE_SLUG: "phala-smoke-test",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^live_phala_smoke_passed$/m);

  const calls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(calls, /doctor --provider phala/);
  assert.match(calls, /run --provider phala .*--slug phala-smoke-test/);
  assert.match(calls, /status --provider phala .*--id cbx_smoke12345678/);
  assert.match(calls, /stop --provider phala .*cbx_smoke12345678/);
});
