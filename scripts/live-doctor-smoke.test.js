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
