import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");

function setupFake() {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-lambda-microvm-smoke-"));
	const calls = path.join(dir, "calls.log");
	const state = path.join(dir, "state");
	const fake = path.join(dir, "crabbox");
	fs.writeFileSync(
		fake,
		`#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>${JSON.stringify(calls)}
state=${JSON.stringify(state)}
case "$1" in
  doctor)
    if [[ "\${FAKE_DOCTOR_FAIL:-0}" == 1 ]]; then
      printf 'credential unavailable\n' >&2
      exit 1
    fi
    printf '{"provider":"aws-lambda-microvm"}\n'
    ;;
  run)
    if [[ "$*" == *LAMBDA_MICROVM_SYNC_OK* ]]; then
      slug=""
      while [[ $# -gt 0 ]]; do
        if [[ "$1" == --slug ]]; then slug="$2"; break; fi
        shift
      done
      printf '%s\n' "$slug" >"$state"
      if [[ "\${FAKE_INITIAL_FAIL:-0}" == 1 ]]; then
        printf 'runner protocol failed\n' >&2
        exit 7
      fi
      printf 'LAMBDA_MICROVM_SYNC_OK\n'
    else
      test -f "$state"
      printf 'LAMBDA_MICROVM_REUSE_OK\n'
    fi
    ;;
  status)
    test -f "$state"
	vm_state="$(sed -n '2p' "$state")"
	printf '{"state":"%s"}\n' "\${vm_state:-RUNNING}"
    ;;
  list)
    if [[ -f "$state" ]]; then
      printf '[{"slug":"%s"}]\n' "$(sed -n '1p' "$state")"
    else
      printf '[]\n'
    fi
    ;;
  pause)
    test -f "$state"
    sed -n '1p' "$state" >"$state.tmp"
    printf 'SUSPENDED\n' >>"$state.tmp"
    mv "$state.tmp" "$state"
    ;;
  resume)
    test -f "$state"
    sed -n '1p' "$state" >"$state.tmp"
    printf 'RUNNING\n' >>"$state.tmp"
    mv "$state.tmp" "$state"
    ;;
  stop)
    rm -f "$state"
    ;;
  *) exit 64 ;;
esac
`,
		"utf8",
	);
	fs.chmodSync(fake, 0o755);
	return { dir, calls, state, fake };
}

function runSmoke(fake, overrides = {}, args = []) {
	return spawnSync("bash", ["scripts/live-aws-lambda-microvm-smoke.sh", ...args], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fake,
			CRABBOX_LIVE: "",
			CRABBOX_LIVE_PROVIDERS: "",
			CRABBOX_AWS_LAMBDA_MICROVM_IMAGE: "",
			...overrides,
		},
		encoding: "utf8",
	});
}

test("dry-run prints planned doctor command without invoking crabbox", () => {
	const fake = setupFake();
	const result = runSmoke(fake, {}, ["--dry-run"]);
	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=dry_run provider=aws-lambda-microvm mutation=false/);
	assert.match(result.stdout, new RegExp(`command=${fake.fake.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")} doctor --provider aws-lambda-microvm --json`));
	assert.equal(fs.existsSync(fake.calls), false);
});

test("requires explicit live opt-in before any command", () => {
	const fake = setupFake();
	const result = runSmoke(fake, { CRABBOX_LIVE_PROVIDERS: "aws-lambda-microvm" });
	assert.equal(result.status, 0, result.stderr);
	assert.match(result.stdout, /classification=environment_blocked reason=set_CRABBOX_LIVE=1/);
	assert.equal(fs.existsSync(fake.calls), false);
});

test("requires an explicit runner image before any command", () => {
	const fake = setupFake();
	const result = runSmoke(fake, { CRABBOX_LIVE: "1", CRABBOX_LIVE_PROVIDERS: "aws-lambda-microvm" });
	assert.equal(result.status, 0, result.stderr);
	assert.match(result.stdout, /missing_CRABBOX_AWS_LAMBDA_MICROVM_IMAGE/);
	assert.equal(fs.existsSync(fake.calls), false);
});

test("proves retained run, pause, resume, and cleanup", () => {
	const fake = setupFake();
	const result = runSmoke(fake, {
		CRABBOX_LIVE: "1",
		CRABBOX_LIVE_PROVIDERS: "aws-lambda-microvm",
		CRABBOX_AWS_LAMBDA_MICROVM_IMAGE: "arn:aws:lambda:eu-west-1:123456789012:microvm-image:runner",
	});
	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=live_aws_lambda_microvm_smoke_passed reason=lifecycle_complete/);
	assert.equal(fs.existsSync(fake.state), false);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.match(calls, /^doctor --provider aws-lambda-microvm --json$/m);
	assert.match(calls, /^run --provider aws-lambda-microvm --keep --slug lambda-microvm-live-/m);
	assert.match(calls, /^run --provider aws-lambda-microvm --id lambda-microvm-live-.* --no-sync -- printf LAMBDA_MICROVM_REUSE_OK$/m);
	assert.match(calls, /^pause --provider aws-lambda-microvm lambda-microvm-live-/m);
	assert.match(calls, /^resume --provider aws-lambda-microvm lambda-microvm-live-/m);
	assert.match(calls, /^stop --provider aws-lambda-microvm lambda-microvm-live-/m);
});

test("cleans up a lease created by a failed initial run", () => {
	const fake = setupFake();
	const result = runSmoke(fake, {
		CRABBOX_LIVE: "1",
		CRABBOX_LIVE_PROVIDERS: "aws-lambda-microvm",
		CRABBOX_AWS_LAMBDA_MICROVM_IMAGE: "arn:aws:lambda:eu-west-1:123456789012:microvm-image:runner",
		FAKE_INITIAL_FAIL: "1",
	});
	assert.equal(result.status, 1, result.stdout);
	assert.match(result.stdout, /classification=diagnostic_only reason=initial_run_failed/);
	assert.equal(fs.existsSync(fake.state), false);
	assert.match(fs.readFileSync(fake.calls, "utf8"), /^stop --provider aws-lambda-microvm --aws-lambda-microvm-forget-missing lambda-microvm-live-/m);
});

test("top-level live smoke dispatches the provider-specific script", () => {
	const source = fs.readFileSync(path.join(repoRoot, "scripts/live-smoke.sh"), "utf8");
	assert.match(source, /has_provider aws-lambda-microvm; then\n  "\$root\/scripts\/live-aws-lambda-microvm-smoke\.sh"/);
});
