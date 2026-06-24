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

function setupFakeCrabbox() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-nomad-live-smoke-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const calls = path.join(dir, "calls.log");
  const state = path.join(dir, "lease.state");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    printf '{"provider":"nomad","status":"ok","checks":[]}\\n'
    ;;
  warmup)
    requested_slug=""
    while [[ "$#" -gt 0 ]]; do
      case "$1" in
        --slug)
          requested_slug="\${2:-}"
          shift 2
          ;;
        *)
          shift
          ;;
      esac
    done
    printf '%s\\n' "$requested_slug" >"${state}"
    if [[ "\${FAKE_CRABBOX_FAIL_WARMUP:-0}" == "1" ]]; then
      printf 'no eligible node for placement\\n' >&2
      exit 1
    fi
    printf 'leased cbx_123456789abc slug=%s provider=nomad job=crabbox-123456789abc allocation=alloc-a task=crabbox workdir=/workspace/crabbox\\n' "$requested_slug"
    ;;
  status)
    test -f "${state}"
    printf '{"id":"cbx_123456789abc","slug":"%s","provider":"nomad","state":"running"}\\n' "$(cat "${state}")"
    ;;
  list)
    if [[ -f "${state}" ]]; then
      printf '[{"Provider":"nomad","slug":"%s","state":"running","labels":{"lease":"cbx_123456789abc","slug":"%s"}}]\\n' "$(cat "${state}")" "$(cat "${state}")"
    elif [[ "\${FAKE_CRABBOX_FAIL_EMPTY_LIST:-0}" == "1" ]]; then
      printf 'inventory unavailable\\n' >&2
      exit 1
    else
      printf '[]\\n'
    fi
    ;;
  run)
    test -f "${state}"
    if [[ "$*" == *"NOMAD_SMOKE_V1_OK"* ]]; then
      if [[ "\${FAKE_CRABBOX_FAIL_INITIAL:-0}" == "1" ]]; then
        printf 'unexpected exec failure\\n' >&2
        exit 1
      fi
      printf 'NOMAD_SMOKE_V1_OK\\n'
      printf '{"provider":"nomad","leaseId":"cbx_123456789abc"}\\n' >&2
    elif [[ "$*" == *"NOMAD_SMOKE_V2_OK"* ]]; then
      printf 'NOMAD_SMOKE_V2_OK\\n'
      printf '{"provider":"nomad","leaseId":"cbx_123456789abc"}\\n' >&2
    elif [[ "$*" == *"NOMAD_SMOKE_NOSYNC_OK"* ]]; then
      printf 'NOMAD_SMOKE_NOSYNC_OK\\n'
    else
      printf 'unexpected run command\\n' >&2
      exit 64
    fi
    ;;
  stop)
    if [[ "\${FAKE_CRABBOX_FAIL_STOP:-0}" == "1" ]]; then
      printf 'transient stop failure\\n' >&2
      exit 1
    fi
    rm -f "${state}"
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );
  return { fakeCrabbox, calls, state };
}

test("Nomad smoke is non-mutating without live opt-in", () => {
  const fake = setupFakeCrabbox();
  const result = spawnSync("bash", ["scripts/live-nomad-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fake.fakeCrabbox,
      CRABBOX_LIVE: "0",
      CRABBOX_LIVE_PROVIDERS: "nomad",
      NOMAD_ADDR: "https://nomad.example.test:4646",
      NOMAD_TOKEN: "secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
  assert.equal(fs.existsSync(fake.calls), false);
});

test("Nomad smoke requires explicit provider selection", () => {
  const fake = setupFakeCrabbox();
  const result = spawnSync("bash", ["scripts/live-nomad-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fake.fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "aws",
      NOMAD_ADDR: "https://nomad.example.test:4646",
      NOMAD_TOKEN: "secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=provider_not_selected/);
  assert.equal(fs.existsSync(fake.calls), false);
});

test("Nomad smoke requires address before mutation", () => {
  const fake = setupFakeCrabbox();
  const result = spawnSync("bash", ["scripts/live-nomad-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fake.fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "nomad",
      NOMAD_ADDR: "",
      NOMAD_TOKEN: "secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=missing_NOMAD_ADDR_or_nomad.address/);
  assert.equal(fs.existsSync(fake.calls), false);
});

test("Nomad smoke requires env-only token before mutation", () => {
  const fake = setupFakeCrabbox();
  const result = spawnSync("bash", ["scripts/live-nomad-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fake.fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "nomad",
      NOMAD_ADDR: "https://nomad.example.test:4646",
      NOMAD_TOKEN: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=missing_NOMAD_TOKEN/);
  assert.equal(fs.existsSync(fake.calls), false);
});

test("Nomad smoke runs retained lifecycle and stops the created lease", () => {
  const fake = setupFakeCrabbox();
  const result = spawnSync("bash", ["scripts/live-nomad-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: path.relative(repoRoot, fake.fakeCrabbox),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "nomad",
      NOMAD_ADDR: "https://nomad.example.test:4646",
      NOMAD_TOKEN: "secret-token",
      CRABBOX_NOMAD_SMOKE_SLUG: "nomad-smoke-test",
      CRABBOX_NOMAD_CLEANUP_RETRY_DELAY_SECONDS: "0",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_nomad_smoke_passed/);
  assert.equal(fs.existsSync(fake.state), false);
  const calls = fs.readFileSync(fake.calls, "utf8");
  assert.match(calls, /^doctor --provider nomad --nomad-address https:\/\/nomad\.example\.test:4646 --nomad-token-env NOMAD_TOKEN --json$/m);
  assert.match(calls, /^warmup --provider nomad .* --slug nomad-smoke-test --timing-json$/m);
  assert.match(calls, /^status --provider nomad .* --id cbx_123456789abc --wait --json$/m);
  assert.match(calls, /^list --provider nomad .* --json$/m);
  assert.match(calls, /^run --provider nomad .* --id cbx_123456789abc --timing-json --allow-env CRABBOX_NOMAD_SMOKE_VALUE -- /m);
  assert.match(calls, /^run --provider nomad .* --id cbx_123456789abc --no-sync -- /m);
  assert.match(calls, /^stop --provider nomad .* cbx_123456789abc$/m);
  assert.doesNotMatch(calls, /secret-token/);
});

test("Nomad smoke cleans up a lease left by a failed run", () => {
  const fake = setupFakeCrabbox();
  const result = spawnSync("bash", ["scripts/live-nomad-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fake.fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "nomad",
      NOMAD_ADDR: "https://nomad.example.test:4646",
      NOMAD_TOKEN: "secret-token",
      CRABBOX_NOMAD_CLEANUP_RETRY_DELAY_SECONDS: "0",
      FAKE_CRABBOX_FAIL_INITIAL: "1",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 1);
  assert.match(result.stdout, /classification=diagnostic_only reason=initial_run_failed/);
  assert.equal(fs.existsSync(fake.state), false);
  assert.match(fs.readFileSync(fake.calls, "utf8"), /^stop --provider nomad .* cbx_123456789abc$/m);
});

test("Nomad smoke classifies placement failures as quota blocked", () => {
  const fake = setupFakeCrabbox();
  const result = spawnSync("bash", ["scripts/live-nomad-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fake.fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "nomad",
      NOMAD_ADDR: "https://nomad.example.test:4646",
      NOMAD_TOKEN: "secret-token",
      CRABBOX_NOMAD_CLEANUP_RETRY_DELAY_SECONDS: "0",
      FAKE_CRABBOX_FAIL_WARMUP: "1",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=quota_blocked reason=warmup_failed/);
});

test("Nomad smoke fails when cleanup cannot be confirmed", () => {
  const fake = setupFakeCrabbox();
  const result = spawnSync("bash", ["scripts/live-nomad-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fake.fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "nomad",
      NOMAD_ADDR: "https://nomad.example.test:4646",
      NOMAD_TOKEN: "secret-token",
      CRABBOX_NOMAD_CLEANUP_RETRY_DELAY_SECONDS: "0",
      FAKE_CRABBOX_FAIL_STOP: "1",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 1);
  assert.match(result.stderr, /cleanup=failed provider=nomad id=cbx_123456789abc attempts=3/);
  const stopCalls = fs
    .readFileSync(fake.calls, "utf8")
    .split("\n")
    .filter((line) => line.startsWith("stop --provider nomad "));
  assert.equal(stopCalls.length, 3);
});
