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

test("live doctor smoke stops when temporary directory creation fails", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-doctor-smoke-"));
	const tools = path.join(dir, "tools");
	const calls = path.join(dir, "calls.log");
	fs.mkdirSync(tools);

	const fakeCrabbox = path.join(dir, "crabbox");
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf 'crabbox invoked: %s\\n' "$*" >>"${calls}"
exit 0
`,
	);
	writeExecutable(
		path.join(tools, "mktemp"),
		`#!/usr/bin/env bash
set -euo pipefail
printf 'mktemp failed\\n' >&2
exit 73
`,
	);

	const result = spawnSync("bash", ["scripts/live-doctor-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			PATH: `${tools}${path.delimiter}${process.env.PATH ?? ""}`,
			CRABBOX_BIN: fakeCrabbox,
			CRABBOX_LIVE_DOCTOR_PROVIDERS: "aws",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 2, result.stderr || result.stdout);
	assert.match(result.stderr, /could not create temporary directory/);
	assert.equal(fs.existsSync(calls), false, "crabbox should not run after mktemp failure");
});

test("live doctor smoke validates json stdout separately from stderr", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-doctor-smoke-"));
	const fakeCrabbox = path.join(dir, "crabbox");

	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
provider=""
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --provider)
      provider="\${2:-}"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
case "$provider" in
  valid-json)
    printf 'warning: diagnostic on stderr\\n' >&2
    printf '{"ok":true,"provider":"valid-json","checks":[]}\\n'
    ;;
  malformed-json)
    printf 'not json\\n'
    ;;
  nonzero-malformed)
    printf 'still not json\\n'
    exit 1
    ;;
  invalid-object)
    printf '{}\\n'
    ;;
  json-stream)
    printf '{"provider":"json-stream"}\\n{"extra":true}\\n'
    ;;
  json-array)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected provider: %s\\n' "$provider" >&2
    exit 64
    ;;
esac
`,
	);

	const result = spawnSync("bash", ["scripts/live-doctor-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fakeCrabbox,
			CRABBOX_LIVE_DOCTOR_PROVIDERS: "valid-json,malformed-json,nonzero-malformed,invalid-object,json-stream,json-array",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1, result.stderr || result.stdout);
	assert.match(result.stdout, /valid-json\s+pass .*warning: diagnostic on stderr/);
	assert.match(result.stdout, /malformed-json\s+fail .*not json/);
	assert.match(result.stdout, /nonzero-malformed\s+fail .*still not json/);
	assert.match(result.stdout, /invalid-object\s+fail/);
	assert.match(result.stdout, /json-stream\s+fail/);
	assert.match(result.stdout, /json-array\s+fail/);
	assert.match(result.stdout, /summary pass=1 fail=5 unsupported=0/);
});
