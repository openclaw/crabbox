import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

test("live tailscale smoke supports help", () => {
	const result = spawnSync("bash", ["scripts/live-tailscale-smoke.sh", "--help"], {
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /Usage: scripts\/live-tailscale-smoke\.sh/);
});

test("live tailscale smoke emits disabled json without live credentials", () => {
	const result = spawnSync("bash", ["scripts/live-tailscale-smoke.sh", "--json"], {
		env: {
			...process.env,
			CRABBOX_TAILSCALE_ENABLED: "0",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	const body = JSON.parse(result.stdout);
	assert.equal(body.tailscale.status, "disabled");
	assert.equal(body.tailscale.enabled, false);
});

test("live tailscale smoke fails on application-level preflight errors", () => {
	const dir = mkdtempSync(join(tmpdir(), "crabbox-tailscale-smoke-"));
	const curl = join(dir, "curl");
	writeFileSync(
		curl,
		`#!/usr/bin/env bash
set -euo pipefail
cfg="$2"
output="$(ruby -rjson -e 'line = File.readlines(ARGV[0]).find { |item| item.start_with?("output = ") }; print JSON.parse(line.split("=", 2)[1].strip)' "$cfg")"
printf '{"tailscale":{"status":"missing_oauth_credentials","enabled":true}}\\n' >"$output"
printf '200'
`,
	);
	chmodSync(curl, 0o755);

	try {
		const result = spawnSync("bash", ["scripts/live-tailscale-smoke.sh", "--json"], {
			env: {
				...process.env,
				PATH: `${dir}:${process.env.PATH}`,
				CRABBOX_LIVE: "1",
				CRABBOX_COORDINATOR: "https://broker.example.com",
				CRABBOX_ADMIN_TOKEN: "test-token",
			},
			encoding: "utf8",
		});

		assert.equal(result.status, 1, result.stderr || result.stdout);
		assert.match(result.stdout, /"status":"missing_oauth_credentials"/);
		assert.match(result.stderr, /preflight failed status=missing_oauth_credentials/);
	} finally {
		rmSync(dir, { recursive: true, force: true });
	}
});
