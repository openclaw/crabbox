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
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-proxmox-live-smoke-"));
	const tools = path.join(dir, "tools");
	const calls = path.join(dir, "calls.log");
	const proof = path.join(dir, "proof");
	fs.mkdirSync(tools);
	fs.mkdirSync(proof);
	const fakeCrabbox = path.join(dir, "crabbox");
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    printf '{"ok":true,"provider":"proxmox","checks":[{"status":"ok","check":"auth","message":"url=%s token=%s secret=%s"}]}\\n' "\${CRABBOX_PROXMOX_API_URL:-}" "\${CRABBOX_PROXMOX_TOKEN_ID:-}" "\${CRABBOX_PROXMOX_TOKEN_SECRET:-}"
    ;;
  list)
    printf '[{"provider":"proxmox","name":"crabbox-test","note":"/tmp/private/api.md"}]\\n'
    ;;
  warmup)
    printf 'leased cbx_test123 slug=proxmox-live-smoke provider=proxmox server=100 type=template-9400 ip=192.0.2.10 idle_timeout=30m expires=later\\n'
    printf 'ready ssh=crabbox@192.0.2.10:22 network=public workroot=/work/crabbox\\n'
    ;;
  status)
    printf '{"id":"cbx_test123","provider":"proxmox","state":"ready","ready":true}\\n'
    ;;
  ssh)
    printf 'ssh -i /tmp/key crabbox@192.0.2.10\\n'
    ;;
  stop)
    printf 'released lease=cbx_test123 server=100\\n'
    ;;
  cleanup)
    if [[ "$*" == *"--dry-run"* ]]; then
      printf 'cleanup dry-run provider=proxmox delete=0\\n'
    else
      printf 'cleanup provider=proxmox delete=0\\n'
    fi
    ;;
  *)
    printf 'unexpected command %s\\n' "$1" >&2
    exit 64
    ;;
esac
`,
	);
	writeExecutable(
		path.join(tools, "ssh"),
		`#!/usr/bin/env bash
set -euo pipefail
printf 'ssh %s\\n' "$*" >>"${calls}"
printf '{"data":{"version":"test"}}\\n'
`,
	);
	return { dir, tools, calls, proof, fakeCrabbox };
}

test("preflight mode is read-only and redacts local proof logs", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			PATH: `${fake.tools}${path.delimiter}${process.env.PATH ?? ""}`,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
			CRABBOX_PROXMOX_API_URL: "https://pve.secret.example:8006",
			CRABBOX_PROXMOX_SSH_INVENTORY_HOST: "pve.secret.example",
			CRABBOX_PROXMOX_TOKEN_ID: "crabbox@pve!ci",
			CRABBOX_PROXMOX_TOKEN_SECRET: "super-secret-token",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=external_user_owned/);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.match(calls, /^doctor --provider proxmox --json$/m);
	assert.match(calls, /^list --provider proxmox --json$/m);
	assert.doesNotMatch(calls, /warmup|stop|cleanup/);
	const redacted = [
		fs.readFileSync(path.join(fake.proof, "doctor.redacted.log"), "utf8"),
		fs.readFileSync(path.join(fake.proof, "list-before.redacted.log"), "utf8"),
		fs.readFileSync(path.join(fake.proof, "node-ssh-inventory.redacted.log"), "utf8"),
	].join("\n");
	assert.doesNotMatch(redacted, /super-secret-token|crabbox@pve!ci|pve\.secret|api\.md/);
	assert.match(redacted, /<proxmox-token-secret>|<proxmox-token-id>|<proxmox-api-url>|<credential-file>/);
});

test("live mode runs lifecycle and dry-run cleanup before optional cleanup", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE: "1",
			CRABBOX_PROXMOX_LIVE_SMOKE_CLEANUP: "1",
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=live_proof_complete/);
	const calls = fs.readFileSync(fake.calls, "utf8").trim().split("\n");
	const dryRunIndex = calls.indexOf("cleanup --provider proxmox --dry-run");
	const cleanupIndex = calls.indexOf("cleanup --provider proxmox");
	assert.ok(calls.indexOf("warmup --provider proxmox --slug proxmox-live-smoke --keep") > -1);
	assert.ok(calls.indexOf("status --provider proxmox --id cbx_test123 --json") > -1);
	assert.ok(calls.indexOf("ssh --provider proxmox --id cbx_test123") > -1);
	assert.ok(calls.indexOf("stop --provider proxmox --id cbx_test123") > -1);
	assert.ok(dryRunIndex > -1, "dry-run cleanup should run");
	assert.ok(cleanupIndex > dryRunIndex, "real cleanup should run after dry-run cleanup");
});
