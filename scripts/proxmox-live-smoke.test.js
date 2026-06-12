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
	const leaseState = path.join(dir, "lease.state");
	const extraLeaseState = path.join(dir, "extra-lease.state");
	fs.mkdirSync(tools);
	fs.mkdirSync(proof, { mode: 0o700 });
	const fakeCrabbox = path.join(dir, "crabbox");
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "$1" in
  config)
    printf '{"proxmox":{"apiUrl":"https://config-only.secret.example:8006","tokenId":"config-only@pve!ci"}}\\n'
    ;;
  doctor)
    if [[ "\${FAKE_CRABBOX_FAIL_DOCTOR:-0}" == "1" ]]; then
      printf '{"ok":false,"provider":"proxmox","checks":[{"status":"failed","check":"storage","message":"url=https://config-only.secret.example:8006 lookup config-only.secret.example principal=config-only@pve!ci"}]}\\n'
      exit 1
    fi
    printf '{"ok":true,"provider":"proxmox","checks":[{"status":"ok","check":"auth","message":"url=%s token=%s secret=%s"}]}\\n' "\${CRABBOX_PROXMOX_API_URL:-}" "\${CRABBOX_PROXMOX_TOKEN_ID:-}" "\${CRABBOX_PROXMOX_TOKEN_SECRET:-}"
    ;;
  list)
    if [[ -f "${leaseState}" ]]; then
      created_slug="$(cat "${leaseState}")"
      if [[ "\${FAKE_CRABBOX_SUFFIXED_SLUG:-0}" == "1" ]]; then
        created_slug="\${created_slug}-abcd"
      fi
      printf '[{"Provider":"proxmox","CloudID":"99","name":"crabbox-existing","labels":{"lease":"cbx_existing","slug":"existing","provider":"proxmox"},"note":"/tmp/private/api.md"},{"Provider":"proxmox","CloudID":"100","name":"crabbox-proxmox-live-smoke","labels":{"lease":"cbx_test123","slug":"%s","provider":"proxmox"}}]\\n' "$created_slug"
    elif [[ -f "${extraLeaseState}" ]]; then
      extra_slug="$(cat "${extraLeaseState}")"
      printf '[{"Provider":"proxmox","CloudID":"99","name":"crabbox-existing","labels":{"lease":"cbx_existing","slug":"existing","provider":"proxmox"},"note":"/tmp/private/api.md"},{"Provider":"proxmox","CloudID":"101","name":"crabbox-proxmox-live-smoke-retry","labels":{"lease":"cbx_leaked","slug":"%s","provider":"proxmox"}}]\\n' "$extra_slug"
    else
      printf '[{"Provider":"proxmox","CloudID":"99","name":"crabbox-existing","labels":{"lease":"cbx_existing","slug":"existing","provider":"proxmox"},"note":"/tmp/private/api.md"}]\\n'
    fi
    ;;
  warmup)
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
    if [[ "\${FAKE_CRABBOX_FAIL_WARMUP_WITH_LEASE:-0}" == "1" ]]; then
      printf '%s\\n' "$requested_slug" >"${leaseState}"
      printf 'warmup failed after leased cbx_test123\\n' >&2
      exit 1
    fi
    if [[ "\${FAKE_CRABBOX_FAIL_WARMUP:-0}" == "1" ]]; then
      printf 'warmup failed before lease creation\\n' >&2
      exit 1
    fi
    printf '%s\\n' "$requested_slug" >"${leaseState}"
    if [[ "\${FAKE_CRABBOX_EXTRA_LEAK:-0}" == "1" ]]; then
      printf '%s-retry\\n' "$requested_slug" >"${extraLeaseState}"
      printf 'leased cbx_leaked slug=%s-retry provider=proxmox server=101\\n' "$requested_slug"
    elif [[ "\${FAKE_CRABBOX_FOREIGN_SUFFIX:-0}" == "1" ]]; then
      printf '%s-foreign\\n' "$requested_slug" >"${extraLeaseState}"
    fi
    if [[ "\${FAKE_CRABBOX_OMIT_LEASE_OUTPUT:-0}" != "1" ]]; then
      printf 'leased cbx_test123 slug=proxmox-live-smoke provider=proxmox server=100 type=template-9400 ip=192.0.2.10 idle_timeout=30m expires=later\\n'
    fi
    printf 'ready ssh=crabbox@192.0.2.10:22 network=public workroot=/work/crabbox\\n'
    ;;
  status)
    printf '{"id":"cbx_test123","provider":"proxmox","state":"ready","host":"192.0.2.10","sshKey":"/Users/tester/Library/Application Support/crabbox/testboxes/cbx_test123/id_ed25519","ready":true}\\n'
    ;;
  ssh)
    printf 'ssh -i /tmp/crabbox-ssh-test/key crabbox@192.0.2.10\\n'
    ;;
  stop)
    if [[ "$*" == *"cbx_leaked"* ]]; then
      if [[ "\${FAKE_CRABBOX_KEEP_RECONCILED_LEAK:-0}" != "1" ]]; then
        rm -f "${extraLeaseState}"
      fi
    else
      rm -f "${leaseState}"
    fi
    printf 'released lease=cbx_test123 server=100\\n'
    ;;
  cleanup)
    if [[ "$*" == *"--dry-run"* ]]; then
      if [[ "\${FAKE_CRABBOX_FAIL_CLEANUP_DRY_RUN:-0}" == "1" ]]; then
        printf 'cleanup dry-run failed\\n' >&2
        exit 1
      fi
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
	return { dir, tools, calls, proof, leaseState, extraLeaseState, fakeCrabbox };
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
		fs.readFileSync(path.join(fake.proof, "summary.redacted.log"), "utf8"),
	].join("\n");
	assert.doesNotMatch(redacted, /super-secret-token|crabbox@pve!ci|pve\.secret|api\.md/);
	assert.doesNotMatch(redacted, new RegExp(fake.proof.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
	assert.match(redacted, /<proxmox-token-secret>|<proxmox-token-id>|<proxmox-api-url>|<credential-file>/);
	assert.match(redacted, /log=<proof-dir>\/doctor\.redacted\.log/);
});

test("live mode runs lifecycle and read-only cleanup proof", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE: "1",
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=live_proof_complete/);
	const calls = fs.readFileSync(fake.calls, "utf8").trim().split("\n");
	const dryRunIndex = calls.indexOf("cleanup --provider proxmox --dry-run");
	assert.ok(calls.some((line) => /^warmup --provider proxmox --slug proxmox-live-smoke-\d+-[0-9a-f]{12} --keep$/.test(line)));
	assert.ok(calls.indexOf("status --provider proxmox --id cbx_test123 --json") > -1);
	assert.ok(calls.indexOf("ssh --provider proxmox --id cbx_test123") > -1);
	assert.ok(calls.indexOf("stop --provider proxmox --id cbx_test123") > -1);
	assert.ok(dryRunIndex > -1, "dry-run cleanup should run");
	assert.equal(calls.indexOf("cleanup --provider proxmox"), -1, "provider-wide cleanup must not run");
	const redacted = [
		fs.readFileSync(path.join(fake.proof, "warmup.redacted.log"), "utf8"),
		fs.readFileSync(path.join(fake.proof, "status.redacted.log"), "utf8"),
		fs.readFileSync(path.join(fake.proof, "ssh-command.redacted.log"), "utf8"),
		fs.readFileSync(path.join(fake.proof, "summary.redacted.log"), "utf8"),
	].join("\n");
	assert.doesNotMatch(redacted, /192\.0\.2\.10|\/Users\/tester|Application Support|\/tmp\/crabbox-ssh-test/);
	assert.doesNotMatch(redacted, new RegExp(fake.proof.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
	assert.match(redacted, /<ip>|<local-home-path>|<local-temp-path>/);
	assert.match(redacted, /classification=live_proof_complete proof_dir=<proof-dir>/);
	for (const name of fs.readdirSync(fake.proof)) {
		const mode = fs.statSync(path.join(fake.proof, name)).mode & 0o777;
		assert.equal(mode & 0o077, 0, `${name} should not be accessible by group or other users`);
	}
});

test("live mode does not stop or cleanup when warmup fails before lease ownership", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE: "1",
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
			FAKE_CRABBOX_FAIL_WARMUP: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	assert.match(result.stdout, /reason=warmup_failed/);
	assert.match(result.stdout, /reason=no_new_matching_lease/);
	assert.match(result.stdout, /classification=environment_blocked/);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.match(calls, /^warmup --provider proxmox --slug proxmox-live-smoke-\d+-[0-9a-f]{12} --keep$/m);
	assert.match(calls, /^list --provider proxmox --json$/m);
	assert.doesNotMatch(calls, /^status |^ssh |^stop |^cleanup /m);
});

test("live mode releases a uniquely created lease after warmup fails", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE: "1",
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
			FAKE_CRABBOX_FAIL_WARMUP_WITH_LEASE: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	assert.match(result.stdout, /reason=warmup_failed/);
	assert.match(result.stdout, /step=stop-reconciled status=pass/);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.match(calls, /^stop --provider proxmox --id cbx_test123$/m);
	assert.equal(fs.existsSync(path.join(fake.dir, "lease.state")), false);
});

test("live mode reconciles a collision-suffixed lease slug", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE: "1",
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
			FAKE_CRABBOX_FAIL_WARMUP_WITH_LEASE: "1",
			FAKE_CRABBOX_SUFFIXED_SLUG: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	assert.match(result.stdout, /step=stop-reconciled status=pass/);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.match(calls, /^stop --provider proxmox --id cbx_test123$/m);
	assert.equal(fs.existsSync(path.join(fake.dir, "lease.state")), false);
});

test("live mode reconciles a leaked earlier attempt after a successful lifecycle", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE: "1",
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
			FAKE_CRABBOX_EXTRA_LEAK: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /status=attempt lease=cbx_leaked/);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.match(calls, /^stop --provider proxmox --id cbx_leaked$/m);
	assert.equal(fs.existsSync(path.join(fake.dir, "extra-lease.state")), false);
});

test("live mode does not reconcile an unrecorded suffix-shaped lease", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE: "1",
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
			FAKE_CRABBOX_FOREIGN_SUFFIX: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.doesNotMatch(calls, /^stop --provider proxmox --id cbx_leaked$/m);
	assert.equal(fs.existsSync(fake.extraLeaseState), true);
});

test("live mode fails when final inventory still contains a reconciled lease", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE: "1",
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
			FAKE_CRABBOX_EXTRA_LEAK: "1",
			FAKE_CRABBOX_KEEP_RECONCILED_LEAK: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	assert.match(result.stdout, /reason=new_matching_leases_remain/);
	assert.match(result.stdout, /classification=environment_blocked/);
});

test("live mode releases a uniquely created lease when warmup output omits its ID", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE: "1",
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
			FAKE_CRABBOX_OMIT_LEASE_OUTPUT: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	assert.match(result.stdout, /reason=warmup output did not include an owned lease id/);
	assert.match(result.stdout, /status=attempt lease=cbx_test123/);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.match(calls, /^stop --provider proxmox --id cbx_test123$/m);
	assert.equal(fs.existsSync(path.join(fake.dir, "lease.state")), false);
});

test("live mode reports cleanup dry-run failure without mutating cleanup", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE: "1",
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
			FAKE_CRABBOX_FAIL_CLEANUP_DRY_RUN: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.match(calls, /^cleanup --provider proxmox --dry-run$/m);
	assert.doesNotMatch(calls, /^cleanup --provider proxmox$/m);
});

test("live mode does not mutate when readiness preflight fails", () => {
	const fake = setupFakeCrabbox();
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE: "1",
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
			FAKE_CRABBOX_FAIL_DOCTOR: "1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	assert.match(result.stdout, /reason=preflight_failed/);
	assert.match(result.stdout, /classification=environment_blocked/);
	const calls = fs.readFileSync(fake.calls, "utf8");
	assert.match(calls, /^doctor --provider proxmox --json$/m);
	assert.doesNotMatch(calls, /^warmup |^status |^ssh |^stop |^cleanup /m);
	const redacted = fs.readFileSync(path.join(fake.proof, "doctor.redacted.log"), "utf8");
	assert.doesNotMatch(redacted, /config-only\.secret\.example|config-only@pve!ci/);
	assert.match(redacted, /<proxmox-api-url>.*<proxmox-api-host>.*<proxmox-token-id>/);
});

test("generated proof directory paths are redacted", () => {
	const fake = setupFakeCrabbox();
	const tempRoot = path.join(fake.dir, "var", "folders", "private");
	fs.mkdirSync(tempRoot, { recursive: true });
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			PATH: `${fake.tools}${path.delimiter}${process.env.PATH ?? ""}`,
			TMPDIR: tempRoot,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_SSH_INVENTORY_HOST: "pve.secret.example",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	const proofName = fs.readdirSync(tempRoot).find((name) => name.startsWith("crabbox-proxmox-live-proof."));
	assert.ok(proofName, "generated proof directory should exist");
	const proof = path.join(tempRoot, proofName);
	const redacted = fs.readFileSync(path.join(proof, "node-ssh-inventory.redacted.log"), "utf8");
	assert.doesNotMatch(redacted, new RegExp(proof.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
	assert.match(redacted, /UserKnownHostsFile=<proof-dir>\/proxmox-node-known-hosts/);
});

test("redaction failure fails the proof and clears public logs", () => {
	const fake = setupFakeCrabbox();
	writeExecutable(
		path.join(fake.tools, "perl"),
		`#!/usr/bin/env bash
exit 73
`,
	);
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			PATH: `${fake.tools}${path.delimiter}${process.env.PATH ?? ""}`,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	assert.match(result.stdout, /reason=redaction_failed/);
	assert.match(result.stdout, /classification=external_user_owned/);
	assert.equal(fs.readFileSync(path.join(fake.proof, "doctor.redacted.log"), "utf8"), "");
	assert.equal(fs.readFileSync(path.join(fake.proof, "list-before.redacted.log"), "utf8"), "");
});

test("caller-supplied proof directories replace pre-existing log symlinks", () => {
	const fake = setupFakeCrabbox();
	const external = path.join(fake.dir, "external.log");
	fs.writeFileSync(external, "do not overwrite", { mode: 0o644 });
	fs.symlinkSync(external, path.join(fake.proof, "doctor.raw.log"));
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
			CRABBOX_PROXMOX_TOKEN_SECRET: "super-secret-token",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.equal(fs.readFileSync(external, "utf8"), "do not overwrite");
	assert.equal(fs.lstatSync(path.join(fake.proof, "doctor.raw.log")).isSymbolicLink(), false);
	assert.equal(fs.statSync(path.join(fake.proof, "doctor.raw.log")).mode & 0o077, 0);
});

test("caller-supplied proof directory symlinks are rejected without chmodding the target", () => {
	const fake = setupFakeCrabbox();
	const victim = path.join(fake.dir, "victim");
	const link = path.join(fake.dir, "proof-link");
	fs.mkdirSync(victim, { mode: 0o755 });
	fs.symlinkSync(victim, link);
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: link,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 2);
	assert.match(result.stderr, /refusing symlink proof directory/);
	assert.equal(fs.statSync(victim).mode & 0o777, 0o755);
});

test("caller-supplied nested proof directories are created privately", () => {
	const fake = setupFakeCrabbox();
	const proof = path.join(fake.dir, "nested", "proof", "run-1");
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: proof,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.equal(fs.statSync(proof).mode & 0o777, 0o700);
	assert.equal(fs.existsSync(path.join(proof, "summary.redacted.log")), true);
});

test("caller-supplied proof directories support GNU stat fallback", () => {
	const fake = setupFakeCrabbox();
	writeExecutable(
		path.join(fake.tools, "stat"),
		`#!/usr/bin/env bash
if [[ "$1" == "-f" ]]; then
  printf '?p\\n'
  exit 0
fi
if [[ "$1" == "-c" ]]; then
  printf '700\\n'
  exit 0
fi
exit 2
`,
	);
	const result = spawnSync("bash", ["scripts/proxmox-live-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			PATH: `${fake.tools}${path.delimiter}${process.env.PATH ?? ""}`,
			CRABBOX_BIN: fake.fakeCrabbox,
			CRABBOX_PROXMOX_LIVE_SMOKE_DIR: fake.proof,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=external_user_owned/);
});
