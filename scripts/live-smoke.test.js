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

test("Tenki live smoke proves paused status waits do not resume the session", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-tenki-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const fakeTenki = path.join(bin, "tenki");
  const crabboxLog = path.join(dir, "crabbox.log");
  const tenkiLog = path.join(dir, "tenki.log");
  const stateFile = path.join(dir, "tenki-state");
  fs.mkdirSync(bin);
  fs.writeFileSync(stateFile, "RUNNING\n", "utf8");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  config)
    exit 0
    ;;
  doctor)
    printf 'ok provider=tenki\\n'
    ;;
  warmup)
    printf 'provisioning provider=tenki lease=cbx_123456789abc slug=tenki-smoke-test session=crabbox-tenki-smoke-test keep=true\\n'
    printf 'provisioned lease=cbx_123456789abc tenki_session=00000000-0000-0000-0000-000000000001 state=ready\\n'
    ;;
  status)
    if [[ "$*" == *"--wait-timeout 2s"* ]]; then
      printf 'timed out waiting for lease tenki-smoke-test to become ready\\n' >&2
      exit 5
    fi
    printf 'lease=cbx_123456789abc slug=tenki-smoke-test provider=tenki state=ready ready=true\\n'
    ;;
  run)
    printf 'crabbox-tenki-ok\\n'
    ;;
  list)
    printf 'crabbox list warning\\n' >&2
    printf '[{"id":"cbx_123456789abc","serverId":"00000000-0000-0000-0000-000000000001","slug":"tenki-smoke-test","provider":"tenki","state":"ready"}]\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  writeExecutable(
    fakeTenki,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${TENKI_FAKE_LOG:?}"
case "$1" in
  --version)
    printf 'tenki version v0.test\\n'
    ;;
  status)
    printf 'tenki status warning\\n' >&2
    printf '{"status":"Logged in (API key)","api_endpoint":"https://api.tenki.test","workspace_id":"ws_test","project_id":"proj_test"}\\n'
    ;;
  sandbox)
    case "$2" in
      pause)
        printf 'PAUSED\\n' >"\${TENKI_FAKE_STATE:?}"
        printf '{"state":"PAUSED"}\\n'
        ;;
      get)
        printf '{"id":"00000000-0000-0000-0000-000000000001","state":"%s"}\\n' "$(tr -d '\\n' <"\${TENKI_FAKE_STATE:?}")"
        ;;
      *)
        printf 'unexpected tenki sandbox args: %s\\n' "$*" >&2
        exit 98
        ;;
    esac
    ;;
  *)
    printf 'unexpected tenki args: %s\\n' "$*" >&2
    exit 97
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "tenki",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_TENKI_ENDPOINT: "https://sandbox.tenki.test",
      TENKI_CLI: fakeTenki,
      TENKI_FAKE_LOG: tenkiLog,
      TENKI_FAKE_STATE: stateFile,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-tenki-ok/);
  assert.match(result.stdout, /paused-session readiness check preserved state=paused/);
  assert.match(result.stderr, /tenki status warning/);
  assert.match(result.stderr, /crabbox list warning/);

  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /doctor --provider tenki/);
  assert.match(crabboxCalls, /warmup --provider tenki --slug tenki-smoke-/);
  assert.match(crabboxCalls, /status --provider tenki --id tenki-smoke-test --wait --wait-timeout 120s/);
  assert.match(crabboxCalls, /run --provider tenki --id tenki-smoke-test --no-sync -- echo crabbox-tenki-ok/);
  assert.match(crabboxCalls, /list --provider tenki --json/);
  assert.match(crabboxCalls, /status --provider tenki --id tenki-smoke-test --wait --wait-timeout 2s/);
  assert.match(crabboxCalls, /stop --provider tenki tenki-smoke-test/);

  const tenkiCalls = fs.readFileSync(tenkiLog, "utf8");
  assert.match(
    tenkiCalls,
    /sandbox pause --endpoint https:\/\/sandbox\.tenki\.test --session 00000000-0000-0000-0000-000000000001/,
  );
  assert.match(
    tenkiCalls,
    /sandbox get --endpoint https:\/\/sandbox\.tenki\.test --output json 00000000-0000-0000-0000-000000000001/,
  );
  assert.doesNotMatch(tenkiCalls, /sandbox resume/);
  assert.equal(fs.readFileSync(stateFile, "utf8").trim(), "PAUSED");
});

test("external live smoke accepts declarative lifecycle configuration", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-external-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  const config = path.join(dir, "crabbox.yaml");
  fs.mkdirSync(bin);
  fs.writeFileSync(
    config,
    `provider: external
external:
  lifecycle:
    acquire:
      argv: [devboxctl, new, "{{name}}"]
    list:
      argv: [devboxctl, list]
      output: json-name-array
    release:
      argv: [devboxctl, rm, "{{name}}"]
  connection:
    ssh:
      user: developer
      host: "{{name}}"
`,
    "utf8",
  );

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf 'external_command=%s args=%s\\n' "\${CRABBOX_EXTERNAL_COMMAND:-}" "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  doctor)
    printf 'ok provider=external\\n'
    ;;
  warmup)
    printf 'provisioning provider=external lease=cbx_123456789abc slug=external-smoke-test\\n'
    ;;
  status)
    printf 'lease=cbx_123456789abc slug=external-smoke-test provider=external state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_123456789abc","slug":"external-smoke-test","provider":"external","state":"ready"}\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  list)
    printf '[{"id":"cbx_123456789abc","slug":"external-smoke-test","provider":"external","state":"ready"}]\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: config,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "external",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /args=doctor --provider external/);
  assert.match(crabboxCalls, /args=warmup --provider external/);
  assert.match(crabboxCalls, /args=stop --provider external external-smoke-test/);
  assert.doesNotMatch(crabboxCalls, /external_command=[^ \n]+/);
});
