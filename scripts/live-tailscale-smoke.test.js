import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
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
