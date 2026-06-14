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

function setupFakeCrabbox({ stdout = "", stderr = "", exitCode = 0 }) {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-firecracker-smoke-"));
	const calls = path.join(dir, "calls.log");
	const stdoutFile = path.join(dir, "doctor.stdout");
	const stderrFile = path.join(dir, "doctor.stderr");
	const fakeCrabbox = path.join(dir, "crabbox");

	fs.writeFileSync(stdoutFile, stdout, "utf8");
	fs.writeFileSync(stderrFile, stderr, "utf8");
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
if [[ "$*" != "doctor --provider firecracker --json" ]]; then
  printf 'unexpected crabbox args: %s\\n' "$*" >&2
  exit 64
fi
cat "${stdoutFile}"
cat "${stderrFile}" >&2
exit ${exitCode}
`,
	);

	return { calls, fakeCrabbox };
}

test("help prints usage without requiring a crabbox binary", () => {
	const result = spawnSync("bash", ["scripts/live-firecracker-smoke.sh", "--help"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: path.join(os.tmpdir(), "missing-crabbox-binary"),
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /Run the read-only Firecracker readiness smoke/);
	assert.match(result.stdout, /--dry-run/);
});

test("dry-run prints the planned doctor command without invoking crabbox", () => {
	const missingBinary = path.join(os.tmpdir(), "missing-crabbox-binary");
	const result = spawnSync("bash", ["scripts/live-firecracker-smoke.sh", "--dry-run"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: missingBinary,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=dry_run provider=firecracker mutation=false/);
	assert.match(
		result.stdout,
		new RegExp(`command=${missingBinary.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")} doctor --provider firecracker --json`),
	);
});

test("readiness mode reports a passing doctor contract", () => {
	const fake = setupFakeCrabbox({
		stdout: JSON.stringify({
			ok: true,
			provider: "firecracker",
			checks: [
				{ status: "ok", check: "git", details: { tool: "git" } },
				{ status: "ok", check: "host", provider: "firecracker", details: { mutation: "false", os: "linux" } },
				{ status: "ok", check: "kvm", provider: "firecracker", details: { mutation: "false", path: "/dev/kvm" } },
				{
					status: "ok",
					check: "binary",
					provider: "firecracker",
					details: { mutation: "false", field: "firecracker.binary", path: "/usr/local/bin/firecracker" },
				},
				{
					status: "skip",
					check: "jailer",
					provider: "firecracker",
					details: { mutation: "false", configured: "", jailer: "disabled" },
				},
				{
					status: "ok",
					check: "kernel",
					provider: "firecracker",
					details: { mutation: "false", path: "/var/lib/crabbox/firecracker/vmlinux" },
				},
				{
					status: "ok",
					check: "rootfs",
					provider: "firecracker",
					details: { mutation: "false", path: "/var/lib/crabbox/firecracker/rootfs.ext4" },
				},
				{
					status: "ok",
					check: "network",
					provider: "firecracker",
					details: { mutation: "false", mode: "cni", cniNetwork: "crabbox-firecracker" },
				},
			],
		}),
	});

	const result = spawnSync("bash", ["scripts/live-firecracker-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /classification=readiness_passed provider=firecracker mutation=false/);
	assert.match(
		result.stdout,
		/checks=host=ok,kvm=ok,binary=ok,jailer=skip,kernel=ok,rootfs=ok,network=ok/,
	);
	assert.match(fs.readFileSync(fake.calls, "utf8"), /^doctor --provider firecracker --json$/m);
});

test("readiness mode classifies blocked Firecracker prerequisites", () => {
	const fake = setupFakeCrabbox({
		stdout: JSON.stringify({
			ok: false,
			provider: "firecracker",
			checks: [
				{ status: "ok", check: "git", details: { tool: "git" } },
				{
					status: "failed",
					check: "host",
					provider: "firecracker",
					details: { class: "environment_blocked", mutation: "false", os: "darwin" },
				},
				{
					status: "skip",
					check: "kvm",
					provider: "firecracker",
					details: { mutation: "false", path: "/dev/kvm", reason: "unsupported_host" },
				},
				{
					status: "failed",
					check: "binary",
					provider: "firecracker",
					details: { class: "environment_blocked", mutation: "false", field: "firecracker.binary" },
				},
				{
					status: "failed",
					check: "kernel",
					provider: "firecracker",
					details: { class: "environment_blocked", mutation: "false", field: "firecracker.kernel" },
				},
				{
					status: "failed",
					check: "rootfs",
					provider: "firecracker",
					details: { class: "environment_blocked", mutation: "false", field: "firecracker.rootfs" },
				},
				{
					status: "failed",
					check: "network",
					provider: "firecracker",
					details: { class: "environment_blocked", mutation: "false", mode: "cni" },
				},
			],
		}),
		stderr: "doctor found problems\n",
		exitCode: 1,
	});

	const result = spawnSync("bash", ["scripts/live-firecracker-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	assert.match(result.stdout, /classification=environment_blocked provider=firecracker mutation=false/);
	assert.match(result.stdout, /checks=host=failed,kvm=skip,binary=failed,kernel=failed,rootfs=failed,network=failed/);
	assert.match(result.stdout, /blocking_checks=host:environment_blocked,binary:environment_blocked,kernel:environment_blocked,rootfs:environment_blocked,network:environment_blocked/);
	assert.match(result.stdout, /stderr=doctor found problems/);
});

test("readiness mode fails closed when doctor JSON is malformed", () => {
	const fake = setupFakeCrabbox({
		stdout: "not json\n",
		stderr: "doctor exploded\n",
		exitCode: 1,
	});

	const result = spawnSync("bash", ["scripts/live-firecracker-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			CRABBOX_BIN: fake.fakeCrabbox,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1);
	assert.match(result.stdout, /classification=validation_failed provider=firecracker mutation=false reason=malformed_doctor_json doctor_exit=1/);
	assert.match(result.stdout, /stderr=doctor exploded/);
});
