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

function setupFakeTools() {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-vercel-sandbox-live-smoke-"));
	const calls = path.join(dir, "calls.log");
	const state = path.join(dir, "lease.state");
	const fakeCrabbox = path.join(dir, "crabbox");
	const fakeSandbox = path.join(dir, "sandbox");
	writeExecutable(
		fakeSandbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf 'sandbox %s\\n' "$*" >>"${calls}"
case "\${1:-}" in
  --help)
    if [[ "\${FAKE_SANDBOX_FAIL_HELP:-0}" == "1" ]]; then
      printf 'sandbox help unavailable\\n' >&2
      exit 1
    fi
    printf 'sandbox help\\n'
    ;;
  list)
    if [[ "\${FAKE_SANDBOX_FAIL_LIST:-0}" == "1" ]]; then
      printf 'login required\\n' >&2
      exit 1
    fi
    printf 'No sandboxes found\\n'
    ;;
  *)
    printf 'unexpected sandbox command %s\\n' "\${1:-}" >&2
    exit 64
    ;;
esac
`,
	);
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf 'crabbox %s\\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    if [[ "\${FAKE_CRABBOX_FAIL_DOCTOR:-0}" == "1" ]]; then
      printf 'missing @vercel/sandbox SDK\\n' >&2
      exit 1
    fi
    printf '{"ok":true,"provider":"vercel-sandbox"}\\n'
    ;;
  run)
    if [[ "$*" == *"VERCEL_SANDBOX_SMOKE_V1_OK"* ]]; then
      if [[ "\${FAKE_CRABBOX_FAIL_BEFORE_CREATE:-0}" == "1" ]]; then
        printf 'unauthorized token\\n' >&2
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
      printf 'VERCEL_SANDBOX_SMOKE_STDOUT\\n'
      printf 'VERCEL_SANDBOX_SMOKE_STDERR\\n' >&2
      printf 'VERCEL_SANDBOX_SMOKE_V1_OK\\n'
      printf '{"provider":"vercel-sandbox","leaseId":"vsbx_test"}\\n' >&2
    elif [[ "$*" == *"VERCEL_SANDBOX_SMOKE_V2_OK"* ]]; then
      test -f "${state}"
      printf 'VERCEL_SANDBOX_SMOKE_V2_OK\\n'
      printf '{"provider":"vercel-sandbox","leaseId":"vsbx_test"}\\n' >&2
    elif [[ "$*" == *"VERCEL_SANDBOX_SMOKE_EXIT_23"* ]]; then
      test -f "${state}"
      printf 'VERCEL_SANDBOX_SMOKE_EXIT_23\\n'
      exit 23
    else
      printf 'unexpected run command\\n' >&2
      exit 64
    fi
    ;;
  status)
    test -f "${state}"
    printf '{"id":"vsbx_test","slug":"%s","provider":"vercel-sandbox","state":"running"}\\n' "$(cat "${state}")"
    ;;
  list)
    if [[ -f "${state}" ]]; then
      printf '[{"provider":"vercel-sandbox","slug":"%s","state":"running"}]\\n' "$(cat "${state}")"
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
	return { dir, calls, state, fakeCrabbox, fakeSandbox };
}

function runSmoke(fake, env) {
	return spawnSync("bash", ["scripts/live-vercel-sandbox-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			PATH: `${fake.dir}${path.delimiter}${process.env.PATH}`,
			CRABBOX_BIN: fake.fakeCrabbox,
			...env,
		},
		encoding: "utf8",
	});
}

test("requires explicit live opt-in before mutation or sandbox preflight", () => {
	const fake = setupFakeTools();
	const result = runSmoke(fake, {
		CRABBOX_LIVE_PROVIDERS: "vercel-sandbox",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=environment_blocked reason=set_CRABBOX_LIVE=1/);
	assert.equal(fs.existsSync(fake.calls), false);
});

test("requires vercel-sandbox provider filter before mutation or sandbox preflight", () => {
	const fake = setupFakeTools();
	const result = runSmoke(fake, {
		CRABBOX_LIVE: "1",
		CRABBOX_LIVE_PROVIDERS: "superserve",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=environment_blocked reason=set_CRABBOX_LIVE_PROVIDERS=vercel-sandbox/);
	assert.equal(fs.existsSync(fake.calls), false);
});

test("classifies sandbox CLI auth failure before Crabbox mutation", () => {
	const fake = setupFakeTools();
	const result = runSmoke(fake, {
		CRABBOX_LIVE: "1",
		CRABBOX_LIVE_PROVIDERS: "vercel-sandbox",
		FAKE_SANDBOX_FAIL_LIST: "1",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=environment_blocked reason=sandbox_auth_preflight_failed/);
	assert.equal(fs.existsSync(fake.state), false);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.match(calls, /^sandbox --help$/m);
	assert.match(calls, /^sandbox list --all --limit 1$/m);
	assert.doesNotMatch(calls, /^crabbox run --provider vercel-sandbox /m);
});

test("classifies doctor SDK failure before creating a sandbox", () => {
	const fake = setupFakeTools();
	const result = runSmoke(fake, {
		CRABBOX_LIVE: "1",
		CRABBOX_LIVE_PROVIDERS: "vercel-sandbox",
		FAKE_CRABBOX_FAIL_DOCTOR: "1",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=environment_blocked reason=doctor_failed/);
	assert.equal(fs.existsSync(fake.state), false);
	assert.doesNotMatch(fs.readFileSync(fake.calls, "utf8"), /^crabbox run --provider vercel-sandbox /m);
});

test("runs retained reuse lifecycle and verifies cleanup", () => {
	const fake = setupFakeTools();
	const secret = "vercel_secret_value_for_redaction";
	const result = runSmoke(fake, {
		CRABBOX_BIN: path.relative(repoRoot, fake.fakeCrabbox),
		CRABBOX_LIVE: "1",
		CRABBOX_LIVE_PROVIDERS: "vercel-sandbox",
		CRABBOX_VERCEL_SANDBOX_AUTH_TOKEN: secret,
		CRABBOX_VERCEL_SANDBOX_PROJECT_ID: "prj_test",
		CRABBOX_VERCEL_SANDBOX_CLEANUP_RETRY_DELAY_SECONDS: "0",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=live_vercel_sandbox_smoke_passed/);
	assert.match(result.stdout, /cleanup=confirmed provider=vercel-sandbox slug=vs-live-/);
	assert.doesNotMatch(result.stdout + result.stderr, new RegExp(secret));
	assert.equal(fs.existsSync(fake.state), false);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.match(calls, /^sandbox --help$/m);
	assert.match(calls, /^sandbox list --all --limit 1$/m);
	assert.match(calls, /^crabbox doctor --provider vercel-sandbox --json$/m);
	assert.match(calls, /^crabbox run --provider vercel-sandbox --keep --slug vs-live-/m);
	assert.match(calls, /^crabbox status --provider vercel-sandbox --id vs-live-.* --wait --json$/m);
	assert.match(calls, /^crabbox run --provider vercel-sandbox --id vs-live-.* --no-sync -- /m);
	assert.match(calls, /^crabbox stop --provider vercel-sandbox vs-live-/m);
});

test("cleans up a lease left by a failed initial run", () => {
	const fake = setupFakeTools();
	const result = runSmoke(fake, {
		CRABBOX_LIVE: "1",
		CRABBOX_LIVE_PROVIDERS: "vercel-sandbox",
		CRABBOX_VERCEL_SANDBOX_CLEANUP_RETRY_DELAY_SECONDS: "0",
		FAKE_CRABBOX_FAIL_INITIAL: "1",
	});

	assert.equal(result.status, 1);
	assert.match(result.stdout, /classification=diagnostic_only reason=initial_run_failed/);
	assert.equal(fs.existsSync(fake.state), false);
	assert.match(fs.readFileSync(fake.calls, "utf8"), /^crabbox stop --provider vercel-sandbox vs-live-/m);
});

test("preserves an auth blocker when no lease was created", () => {
	const fake = setupFakeTools();
	const result = runSmoke(fake, {
		CRABBOX_LIVE: "1",
		CRABBOX_LIVE_PROVIDERS: "vercel-sandbox",
		CRABBOX_VERCEL_SANDBOX_CLEANUP_RETRY_DELAY_SECONDS: "0",
		FAKE_CRABBOX_FAIL_BEFORE_CREATE: "1",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=environment_blocked reason=initial_run_failed/);
	assert.equal(fs.existsSync(fake.state), false);
	assert.doesNotMatch(fs.readFileSync(fake.calls, "utf8"), /^crabbox stop --provider vercel-sandbox /m);
});

test("fails after three targeted cleanup attempts", () => {
	const fake = setupFakeTools();
	const result = runSmoke(fake, {
		CRABBOX_LIVE: "1",
		CRABBOX_LIVE_PROVIDERS: "vercel-sandbox",
		CRABBOX_VERCEL_SANDBOX_CLEANUP_RETRY_DELAY_SECONDS: "0",
		FAKE_CRABBOX_FAIL_INITIAL: "1",
		FAKE_CRABBOX_FAIL_STOP: "1",
	});

	assert.equal(result.status, 1);
	assert.match(result.stderr, /cleanup=failed provider=vercel-sandbox .* attempts=3/);
	const stopCalls = fs
		.readFileSync(fake.calls, "utf8")
		.split("\n")
		.filter((line) => line.startsWith("crabbox stop --provider vercel-sandbox vs-live-"));
	assert.equal(stopCalls.length, 3);
});

test("does not pass when post-stop inventory cannot be confirmed", () => {
	const fake = setupFakeTools();
	const result = runSmoke(fake, {
		CRABBOX_LIVE: "1",
		CRABBOX_LIVE_PROVIDERS: "vercel-sandbox",
		CRABBOX_VERCEL_SANDBOX_CLEANUP_RETRY_DELAY_SECONDS: "0",
		FAKE_CRABBOX_FAIL_EMPTY_LIST: "1",
	});

	assert.equal(result.status, 1);
	assert.doesNotMatch(result.stdout, /classification=live_vercel_sandbox_smoke_passed/);
	assert.match(result.stdout, /classification=diagnostic_only reason=lease_cleanup_unconfirmed/);
	assert.match(result.stderr, /cleanup=failed provider=vercel-sandbox/);
});
