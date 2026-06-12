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

test("live auth smoke removes bearer curl config after curl transport failure", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-auth-smoke-"));
	const bin = path.join(dir, "bin");
	const config = path.join(dir, "crabbox.yaml");
	const curlConfigPath = path.join(dir, "curl-config-path.txt");
	fs.mkdirSync(bin);
	fs.writeFileSync(
		config,
		[
			"broker:",
			"  token: shared-secret-token",
			"  adminToken: admin-secret-token",
			"  url: https://coordinator.example.test",
			"",
		].join("\n"),
	);

	const fakeCrabbox = path.join(dir, "crabbox");
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == "whoami --json" ]]; then
  printf 'warning: fake diagnostic on stderr\\n' >&2
  printf '{"auth":"bearer","owner":"alice@example.com","org":"example-org"}\\n'
  exit 0
fi
printf 'unexpected crabbox args: %s\\n' "$*" >&2
exit 64
`,
	);

	writeExecutable(
		path.join(bin, "curl"),
		`#!/usr/bin/env bash
set -euo pipefail
if [[ "\${1:-}" != "--config" || -z "\${2:-}" ]]; then
  printf 'unexpected curl args: %s\\n' "$*" >&2
  exit 64
fi
grep -q 'Authorization: Bearer shared-secret-token' "$2"
printf '%s' "$2" >"$CRABBOX_FAKE_CURL_CONFIG_PATH"
exit 7
`,
	);

	const result = spawnSync("bash", ["scripts/live-auth-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
			HOME: dir,
			TMPDIR: dir,
			CRABBOX_LIVE: "1",
			CRABBOX_CONFIG: config,
			CRABBOX_BIN: fakeCrabbox,
			CRABBOX_FAKE_CURL_CONFIG_PATH: curlConfigPath,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 7, result.stderr || result.stdout);
	assert.doesNotMatch(result.stderr, /failed coordinator whoami shape/);
	assert.match(result.stderr, /warning: fake diagnostic on stderr/);
	const leakedPath = fs.readFileSync(curlConfigPath, "utf8");
	assert.equal(fs.existsSync(leakedPath), false, `expected curl config to be removed: ${leakedPath}`);
});
