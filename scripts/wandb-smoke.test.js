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

test("wandb smoke resolves relative CRABBOX_BIN before changing repo", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-wandb-smoke-"));
	const caller = path.join(dir, "caller");
	const target = path.join(dir, "target");
	const tools = path.join(dir, "tools");
	const calls = path.join(dir, "calls.log");
	const trap = path.join(dir, "trap.log");
	fs.mkdirSync(path.join(caller, "bin"), { recursive: true });
	fs.mkdirSync(path.join(target, "bin"), { recursive: true });
	fs.mkdirSync(tools);

	writeExecutable(
		path.join(caller, "bin", "crabbox"),
		`#!/usr/bin/env bash
set -euo pipefail
printf '%s|%s\\n' "$PWD" "$*" >>"${calls}"
case "\${1:-}" in
  doctor|run)
    exit 0
    ;;
  list)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 64
    ;;
esac
`,
	);
	writeExecutable(
		path.join(target, "bin", "crabbox"),
		`#!/usr/bin/env bash
set -euo pipefail
printf 'target binary executed\\n' >>"${trap}"
exit 66
`,
	);
	writeExecutable(
		path.join(tools, "jq"),
		`#!/usr/bin/env bash
set -euo pipefail
cat >/dev/null
printf '[]\\n'
`,
	);

	const result = spawnSync("bash", [path.join(repoRoot, "scripts", "wandb-smoke.sh")], {
		cwd: caller,
		env: {
			...process.env,
			PATH: `${tools}${path.delimiter}${process.env.PATH ?? ""}`,
			CRABBOX_LIVE: "1",
			CRABBOX_BIN: "./bin/crabbox",
			CRABBOX_LIVE_REPO: target,
			WANDB_API_KEY: "test-key",
			WANDB_ENTITY_NAME: "example-org",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.equal(fs.existsSync(trap), false, "target repo relative CRABBOX_BIN was executed");
	const callLog = fs.readFileSync(calls, "utf8");
	assert.match(callLog, new RegExp(`${target.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\|doctor --provider wandb`));
	assert.match(callLog, /\|run --provider wandb --no-sync --wandb-max-lifetime 60 -- echo crabbox-wandb-ok/);
	assert.match(callLog, /\|list --provider wandb --json/);
});
