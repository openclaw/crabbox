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
	const active = path.join(dir, "active");
	const leftRemoteOnce = path.join(dir, "left-remote-once");
	fs.mkdirSync(path.join(caller, "bin"), { recursive: true });
	fs.mkdirSync(path.join(target, "bin"), { recursive: true });
	fs.mkdirSync(tools);

	writeExecutable(
		path.join(caller, "bin", "crabbox"),
		`#!/usr/bin/env bash
set -euo pipefail
printf '%s|%s\\n' "$PWD" "$*" >>"${calls}"
case "\${1:-}" in
  doctor)
    exit 0
    ;;
  run)
    lease_output=""
    args=("$@")
    for ((i = 0; i < \${#args[@]}; i++)); do
      if [[ "\${args[$i]}" == "--lease-output" ]]; then
        lease_output="\${args[$((i + 1))]}"
      fi
    done
    if [[ -n "$lease_output" ]]; then
      mkdir -p "$XDG_STATE_HOME/crabbox/claims"
      printf '{"leaseId":"sb-live"}\\n' >"$lease_output"
      printf '{"leaseID":"sb-live"}\\n' >"$XDG_STATE_HOME/crabbox/claims/sb-live.json"
      : >"${active}"
    elif [[ ! -f "$XDG_STATE_HOME/crabbox/claims/sb-live.json" ]]; then
      printf 'missing claim for reuse\\n' >&2
      exit 4
    fi
    ;;
  status)
    test -f "$XDG_STATE_HOME/crabbox/claims/sb-live.json"
    ;;
  stop)
    if [[ ! -f "$XDG_STATE_HOME/crabbox/claims/sb-live.json" ]]; then
      printf 'wandb sandbox "sb-live" has no matching local ownership claim\\n' >&2
      exit 4
    fi
    rm -f "$XDG_STATE_HOME/crabbox/claims/sb-live.json"
    if [[ "\${CRABBOX_FAKE_LEAVE_REMOTE:-}" == "1" && ! -e "${leftRemoteOnce}" ]]; then
	  : >"${leftRemoteOnce}"
	else
      rm -f "${active}"
    fi
    ;;
  list)
    if [[ -e "${active}" ]]; then
      printf '[{"id":"friendly-name","CloudID":"sb-live"}]\\n'
    else
      printf '[]\\n'
    fi
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
case "\${1:-}" in
  -r)
    sed -n 's/.*"leaseId":"\\([^"]*\\)".*/\\1/p' "\${3:?}"
    ;;
  -e)
    input="$(cat)"
    case "$*" in
      *'.CloudID // .id'*)
        if [[ "$input" == *'"CloudID":"sb-live"'* ]]; then
          exit 1
        fi
        ;;
      *'.id // .CloudID'*)
        # A display id masks CloudID with this ordering, reproducing the bug.
        ;;
      *)
        exit 64
        ;;
    esac
    ;;
  *)
    exit 64
    ;;
esac
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
	assert.match(callLog, /\|run --provider wandb --no-sync --keep --lease-output .+ --wandb-max-lifetime 60 -- echo crabbox-wandb-ok/);
	assert.match(callLog, /\|list --provider wandb --json/);
	const seen = callLog
		.trim()
		.split("\n")
		.map((line) => line.slice(target.length + 1));
	assert.equal(seen.length, 8, JSON.stringify(seen));
	assert.equal(seen[0], "doctor --provider wandb");
	assert.equal(seen[2], "status --provider wandb --id sb-live");
	assert.equal(seen[3], "stop --provider wandb --id sb-live");
	assert.equal(seen[4], "status --provider wandb --id sb-live");
	assert.equal(
		seen[5],
		"run --provider wandb --no-sync --id sb-live -- echo crabbox-wandb-reuse-ok",
	);
	assert.equal(seen[6], "stop --provider wandb --id sb-live");
	assert.equal(seen[7], "list --provider wandb --json");

	const residueResult = spawnSync("bash", [path.join(repoRoot, "scripts", "wandb-smoke.sh")], {
		cwd: caller,
		env: {
			...process.env,
			PATH: `${tools}${path.delimiter}${process.env.PATH ?? ""}`,
			CRABBOX_LIVE: "1",
			CRABBOX_BIN: "./bin/crabbox",
			CRABBOX_LIVE_REPO: target,
			CRABBOX_FAKE_LEAVE_REMOTE: "1",
			WANDB_API_KEY: "test-key",
			WANDB_ENTITY_NAME: "example-org",
		},
		encoding: "utf8",
	});
	assert.equal(residueResult.status, 1, residueResult.stderr || residueResult.stdout);
	assert.match(residueResult.stderr, /wandb smoke stop left active remote inventory residue/);
	assert.equal(fs.existsSync(active), false, "cleanup retry left remote sandbox residue");
});
