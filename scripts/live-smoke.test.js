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

test("blacksmith live smoke requires an explicit organization", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-blacksmith-missing-org-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const config = path.join(dir, "crabbox.yaml");
  fs.mkdirSync(bin);
  fs.writeFileSync(
    config,
    `blacksmith:
  workflow: .github/workflows/blacksmith-testbox.yml
  job: go
`,
    "utf8",
  );
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf 'unexpected crabbox args: %s\\n' "$*" >&2
exit 99
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_BLACKSMITH_ORG: "",
      CRABBOX_CONFIG: config,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "blacksmith-testbox",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(result.stderr, /requires CRABBOX_BLACKSMITH_ORG, blacksmith\.org, or actions\.repo/);
});

test("blacksmith live smoke derives organization from actions repo", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-blacksmith-actions-org-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  const config = path.join(dir, "crabbox.yaml");
  fs.mkdirSync(bin);
  fs.writeFileSync(
    config,
    `blacksmith:
  workflow: .github/workflows/blacksmith-testbox.yml
  job: go
actions:
  repo: example-org/my-app
`,
    "utf8",
  );
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  list)
    printf '[]\\n'
    ;;
  run)
    printf 'blacksmith-crabbox-ok\\n'
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
      CRABBOX_BLACKSMITH_ORG: "",
      CRABBOX_CONFIG: config,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "blacksmith-testbox",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /blacksmith-crabbox-ok/);
  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /run --provider blacksmith-testbox --blacksmith-org example-org/);
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

test("default live smoke keeps Morph opt-in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-default-providers-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  warmup)
    printf 'provisioning provider=test lease=cbx_123456789abc slug=default-smoke-test\\n'
    printf 'provisioned lease=cbx_123456789abc slug=default-smoke-test state=ready\\n'
    ;;
  status)
    printf 'lease=cbx_123456789abc slug=default-smoke-test provider=test state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_123456789abc","slug":"default-smoke-test","provider":"test","state":"ready","serverType":"type","host":"example.test","ready":true,"lastTouchedAt":"2026-06-10T00:00:00Z","expiresAt":"2026-06-10T00:15:00Z"}\\n'
    ;;
  ssh)
    exit 0
    ;;
  cache)
    printf '[]\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  history)
    printf 'history ok\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    if [[ "\${CRABBOX_FAKE_ADMIN_FAIL:-0}" == "1" ]]; then
      printf 'admin endpoint unavailable\\n' >&2
      exit 42
    fi
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const env = { ...process.env };
  delete env.CRABBOX_LIVE_PROVIDERS;
  delete env.CRABBOX_MORPH_API_KEY;
  delete env.MORPH_API_KEY;

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...env,
      PATH: `${bin}${path.delimiter}${env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /warmup --provider aws/);
  assert.match(crabboxCalls, /warmup --provider hetzner/);
  assert.doesNotMatch(crabboxCalls, /--provider morph/);
  assert.doesNotMatch(result.stderr, /CRABBOX_MORPH_API_KEY|MORPH_API_KEY|morph\.apiKey/);

  const failedAudit = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...env,
      PATH: `${bin}${path.delimiter}${env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_FAKE_ADMIN_FAIL: "1",
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_ADMIN_AUDIT: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });
  assert.equal(failedAudit.status, 42, failedAudit.stdout + failedAudit.stderr);
  assert.match(failedAudit.stderr, /error: admin active-lease check failed: admin endpoint unavailable/);
  assert.doesNotMatch(failedAudit.stderr, /unbound variable/);
});

test("live smoke fails when final active lease audit fails", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-admin-audit-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  fs.mkdirSync(bin);

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  admin)
    printf 'admin endpoint unavailable\\n' >&2
    exit 42
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
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_ADMIN_AUDIT: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 42, result.stdout + result.stderr);
  assert.match(result.stderr, /error: admin active-lease check failed: admin endpoint unavailable/);
});

test("live smoke skips final active lease audit when coordinator is disabled", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-admin-skip-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  fs.mkdirSync(bin);

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf 'unexpected crabbox args: %s\\n' "$*" >&2
exit 99
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /admin active-lease check skipped/);
  assert.match(result.stdout, /^0\n?$/);
});

test("morph live smoke dispatches the expected argv to crabbox", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-morph-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);

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
    printf 'ok provider=morph\\n'
    ;;
  warmup)
    printf 'provisioning provider=morph lease=cbx_1a2b3c4d5e6f slug=morph-smoke-test\\n'
    printf 'provisioned lease=cbx_1a2b3c4d5e6f slug=morph-smoke-test state=ready\\n'
    ;;
  status)
    printf 'lease=cbx_1a2b3c4d5e6f slug=morph-smoke-test provider=morph state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_1a2b3c4d5e6f","slug":"morph-smoke-test","provider":"morph","state":"ready","serverType":"snapshot_test","host":"ssh.cloud.morph.so","ready":true,"lastTouchedAt":"2026-06-09T20:00:00Z","expiresAt":"2026-06-09T20:15:00Z"}\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  list)
    printf '[{"id":"cbx_1a2b3c4d5e6f","slug":"morph-smoke-test","provider":"morph","state":"ready"}]\\n'
    ;;
  stop)
    [[ "\${CRABBOX_MORPH_DELETE_ON_RELEASE:-}" == "1" ]] || exit 96
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
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "morph",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_MORPH_API_KEY: "dummy-morph-key",
      CRABBOX_LIVE_MORPH_SNAPSHOT: "snapshot_test",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /^doctor$/m);
  assert.match(crabboxCalls, /^warmup --keep=false --slug morph-smoke-\d+ --ttl 15m --idle-timeout 5m$/m);
  assert.match(crabboxCalls, /^status --id morph-smoke-test --wait --wait-timeout 120s$/m);
  assert.match(crabboxCalls, /^inspect --id morph-smoke-test --json$/m);
  assert.match(crabboxCalls, /^run --id morph-smoke-test --shell --/m);
  assert.match(crabboxCalls, /^list --json$/m);
  assert.match(crabboxCalls, /^stop morph-smoke-test$/m);
  assert.doesNotMatch(crabboxCalls, /dummy-morph-key/);
});

test("morph live smoke accepts the API key from config", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-morph-config-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  const config = path.join(dir, "crabbox.yaml");
  fs.mkdirSync(bin);
  fs.writeFileSync(
    config,
    `morph:
  apiKey: config-backed-morph-key
`,
    "utf8",
  );

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  doctor)
    printf 'ok provider=morph\\n'
    ;;
  warmup)
    printf 'provisioning provider=morph lease=cbx_1a2b3c4d5e6f slug=morph-smoke-test\\n'
    printf 'provisioned lease=cbx_1a2b3c4d5e6f slug=morph-smoke-test state=ready\\n'
    ;;
  status)
    printf 'lease=cbx_1a2b3c4d5e6f slug=morph-smoke-test provider=morph state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_1a2b3c4d5e6f","slug":"morph-smoke-test","provider":"morph","state":"ready","serverType":"snapshot_test","host":"ssh.cloud.morph.so","ready":true,"lastTouchedAt":"2026-06-09T20:00:00Z","expiresAt":"2026-06-09T20:15:00Z"}\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  list)
    printf '[{"id":"cbx_1a2b3c4d5e6f","slug":"morph-smoke-test","provider":"morph","state":"ready"}]\\n'
    ;;
  stop)
    [[ "\${CRABBOX_MORPH_DELETE_ON_RELEASE:-}" == "1" ]] || exit 96
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

  const env = { ...process.env };
  delete env.CRABBOX_MORPH_API_KEY;
  delete env.MORPH_API_KEY;

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...env,
      PATH: `${bin}${path.delimiter}${env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: config,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "morph",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_LIVE_MORPH_SNAPSHOT: "snapshot_test",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /^doctor$/m);
  assert.match(crabboxCalls, /^warmup --keep=false --slug morph-smoke-\d+ --ttl 15m --idle-timeout 5m$/m);
  assert.match(crabboxCalls, /^stop morph-smoke-test$/m);
  assert.doesNotMatch(crabboxCalls, /config-backed-morph-key/);
});

test("morph live smoke aborts cleanly when no API key is configured", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-morph-nokey-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
exit 0
`,
  );

  const env = { ...process.env };
  delete env.CRABBOX_MORPH_API_KEY;
  delete env.MORPH_API_KEY;

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...env,
      PATH: `${bin}${path.delimiter}${env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "morph",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_LIVE_MORPH_SNAPSHOT: "snapshot_test",
    },
    encoding: "utf8",
  });

  assert.notEqual(result.status, 0, "expected non-zero exit when morph key is missing");
  assert.match(result.stderr, /CRABBOX_MORPH_API_KEY/);
  assert.match(result.stderr, /MORPH_API_KEY/);
  assert.match(result.stderr, /morph\.apiKey/);
  const calls = fs.existsSync(crabboxLog) ? fs.readFileSync(crabboxLog, "utf8") : "";
  assert.doesNotMatch(calls, /--provider morph/, "no morph-specific crabbox call may be issued when the key is missing");
});
