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
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-superserve-live-smoke-"));
	const calls = path.join(dir, "calls.log");
	const state = path.join(dir, "lease.state");
	const fakeCrabbox = path.join(dir, "crabbox");
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    if [[ "\${FAKE_CRABBOX_FAIL_DOCTOR:-0}" == "1" ]]; then
      printf 'forbidden api key\\n' >&2
      exit 1
    fi
    printf '{"ok":true,"provider":"superserve"}\\n'
    ;;
  run)
    if [[ "$*" == *"SUPERSERVE_SMOKE_V1_OK"* ]]; then
      if [[ "\${FAKE_CRABBOX_FAIL_BEFORE_CREATE:-0}" == "1" ]]; then
        printf 'unauthorized api key\\n' >&2
        exit 1
      fi
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
      if [[ "\${FAKE_CRABBOX_FAIL_INITIAL:-0}" == "1" ]]; then
        printf 'unexpected create failure\\n' >&2
        exit 1
      fi
      printf 'SUPERSERVE_SMOKE_STDOUT\\n'
      printf 'SUPERSERVE_SMOKE_STDERR\\n' >&2
      printf 'SUPERSERVE_SMOKE_V1_OK\\n'
      printf '{"provider":"superserve","leaseId":"ssbx_test"}\\n' >&2
    elif [[ "$*" == *"SUPERSERVE_SMOKE_V2_OK"* ]]; then
      test -f "${state}"
      printf 'SUPERSERVE_SMOKE_V2_OK\\n'
      printf '{"provider":"superserve","leaseId":"ssbx_test"}\\n' >&2
    elif [[ "$*" == *"SUPERSERVE_SMOKE_EXIT_23"* ]]; then
      test -f "${state}"
      printf 'SUPERSERVE_SMOKE_EXIT_23\\n'
      exit 23
    else
      printf 'unexpected run command\\n' >&2
      exit 64
    fi
    ;;
  status)
    test -f "${state}"
    printf '{"id":"ssbx_test","slug":"%s","provider":"superserve","state":"running"}\\n' "$(cat "${state}")"
    ;;
  list)
    if [[ -f "${state}" ]]; then
      printf '[{"provider":"superserve","slug":"%s","state":"running"}]\\n' "$(cat "${state}")"
    elif [[ "\${FAKE_CRABBOX_FAIL_EMPTY_LIST:-0}" == "1" ]]; then
      printf 'inventory unavailable\\n' >&2
      exit 1
    else
      printf '[]\\n'
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
    printf 'unexpected command %s\\n' "$1" >&2
    exit 64
    ;;
esac
`,
	);
	return { dir, calls, state, fakeCrabbox };
}

test("requires explicit live opt-in before mutation", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/live-superserve-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_LIVE_PROVIDERS: "superserve",
			CRABBOX_SUPERSERVE_API_KEY: "ss_live_test",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=environment_blocked reason=set_CRABBOX_LIVE=1/);
	assert.equal(fs.existsSync(fake.calls), false);
});

test("requires superserve provider filter before mutation", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/live-superserve-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_LIVE: "1",
			CRABBOX_LIVE_PROVIDERS: "smolvm",
			CRABBOX_SUPERSERVE_API_KEY: "ss_live_test",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=environment_blocked reason=set_CRABBOX_LIVE_PROVIDERS=superserve/);
	assert.equal(fs.existsSync(fake.calls), false);
});

test("classifies missing key as environment blocked before mutation", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/live-superserve-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_LIVE: "1",
			CRABBOX_LIVE_PROVIDERS: "superserve",
			CRABBOX_SUPERSERVE_API_KEY: "",
			SUPERSERVE_API_KEY: "",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=environment_blocked reason=missing_superserve_api_key/);
	assert.equal(fs.existsSync(fake.calls), false);
});

test("classifies doctor auth failure before creating a sandbox", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/live-superserve-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_LIVE: "1",
			CRABBOX_LIVE_PROVIDERS: "superserve",
			CRABBOX_SUPERSERVE_API_KEY: "ss_live_test",
			FAKE_CRABBOX_FAIL_DOCTOR: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=environment_blocked reason=doctor_failed/);
	assert.equal(fs.existsSync(fake.state), false);
	assert.doesNotMatch(fs.readFileSync(fake.calls, "utf8"), /^run --provider superserve /m);
});

test("runs retained reuse lifecycle and verifies cleanup", () => {
	const fake = setupFakeCrabbox();
	const secret = "ss_live_secret_value";
	const result = spawnSync("bash", ["scripts/live-superserve-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: path.relative(repoRoot, fake.fakeCrabbox),
			CRABBOX_LIVE: "1",
			CRABBOX_LIVE_PROVIDERS: "superserve",
			CRABBOX_SUPERSERVE_API_KEY: secret,
			CRABBOX_SUPERSERVE_CLEANUP_RETRY_DELAY_SECONDS: "0",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=live_superserve_smoke_passed/);
	assert.match(result.stdout, /cleanup=confirmed provider=superserve slug=ss-live-/);
	assert.doesNotMatch(result.stdout + result.stderr, new RegExp(secret));
	assert.equal(fs.existsSync(fake.state), false);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.match(calls, /^doctor --provider superserve --json$/m);
	assert.match(calls, /^run --provider superserve --keep --slug ss-live-/m);
	assert.match(calls, /^status --provider superserve --id ss-live-.* --wait --json$/m);
	assert.match(calls, /^run --provider superserve --id ss-live-.* --no-sync -- /m);
	assert.match(calls, /^stop --provider superserve ss-live-/m);
});

test("cleans up a lease left by a failed initial run", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/live-superserve-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_LIVE: "1",
			CRABBOX_LIVE_PROVIDERS: "superserve",
			CRABBOX_SUPERSERVE_API_KEY: "ss_live_test",
			CRABBOX_SUPERSERVE_CLEANUP_RETRY_DELAY_SECONDS: "0",
			FAKE_CRABBOX_FAIL_INITIAL: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	assert.match(result.stdout, /classification=diagnostic_only reason=initial_run_failed/);
	assert.equal(fs.existsSync(fake.state), false);
	assert.match(fs.readFileSync(fake.calls, "utf8"), /^stop --provider superserve ss-live-/m);
});

test("preserves an auth blocker when no lease was created", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/live-superserve-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_LIVE: "1",
			CRABBOX_LIVE_PROVIDERS: "superserve",
			CRABBOX_SUPERSERVE_API_KEY: "ss_live_test",
			CRABBOX_SUPERSERVE_CLEANUP_RETRY_DELAY_SECONDS: "0",
			FAKE_CRABBOX_FAIL_BEFORE_CREATE: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=environment_blocked reason=initial_run_failed/);
	assert.equal(fs.existsSync(fake.state), false);
	assert.doesNotMatch(fs.readFileSync(fake.calls, "utf8"), /^stop --provider superserve /m);
});

test("fails after three targeted cleanup attempts", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/live-superserve-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_LIVE: "1",
			CRABBOX_LIVE_PROVIDERS: "superserve",
			CRABBOX_SUPERSERVE_API_KEY: "ss_live_test",
			CRABBOX_SUPERSERVE_CLEANUP_RETRY_DELAY_SECONDS: "0",
			FAKE_CRABBOX_FAIL_INITIAL: "1",
			FAKE_CRABBOX_FAIL_STOP: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	assert.match(result.stderr, /cleanup=failed provider=superserve .* attempts=3/);
	const stopCalls = fs
		.readFileSync(fake.calls, "utf8")
		.split("\n")
		.filter((line) => line.startsWith("stop --provider superserve ss-live-"));
	assert.equal(stopCalls.length, 3);
});

test("does not pass when post-stop inventory cannot be confirmed", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/live-superserve-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_LIVE: "1",
			CRABBOX_LIVE_PROVIDERS: "superserve",
			CRABBOX_SUPERSERVE_API_KEY: "ss_live_test",
			CRABBOX_SUPERSERVE_CLEANUP_RETRY_DELAY_SECONDS: "0",
			FAKE_CRABBOX_FAIL_EMPTY_LIST: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	assert.doesNotMatch(result.stdout, /classification=live_superserve_smoke_passed/);
	assert.match(result.stdout, /classification=diagnostic_only reason=lease_cleanup_unconfirmed/);
	assert.match(result.stderr, /cleanup=failed provider=superserve/);
});
