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

test("deploy worker smoke parses completed warmup log before cleanup", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-deploy-worker-smoke-"));
	const bin = path.join(dir, "bin");
	const liveRepo = path.join(dir, "live-repo");
	const calls = path.join(dir, "calls.log");
	fs.mkdirSync(bin);
	fs.mkdirSync(liveRepo);

	writeExecutable(
		path.join(bin, "npm"),
		`#!/usr/bin/env bash
set -euo pipefail
printf 'npm %s\\n' "$*" >>"${calls}"
exit 0
`,
	);
	writeExecutable(
		path.join(bin, "curl"),
		`#!/usr/bin/env bash
set -euo pipefail
printf '{"ok":true,"service":"crabbox-coordinator"}\\n'
`,
	);
	const fakeCrabbox = path.join(dir, "crabbox");
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf 'crabbox %s\\n' "$*" >>"${calls}"
case "\${1:-}" in
  warmup)
    sleep 0.1
    printf '{"leaseId":"cbx_smoke"}\\n' >&2
    ;;
  run)
    exit 17
    ;;
  stop)
    exit 0
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 64
    ;;
esac
`,
	);

	const result = spawnSync("bash", ["scripts/deploy-worker-smoke.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
			HOME: dir,
			CRABBOX_BIN: fakeCrabbox,
			CRABBOX_DEPLOY_SMOKE_URLS: "https://broker.crabbox.test/v1/health",
			CRABBOX_DEPLOY_SMOKE_AWS: "1",
			CRABBOX_LIVE_REPO: liveRepo,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 17, result.stderr || result.stdout);
	assert.match(result.stdout, /aws deploy smoke lease=cbx_smoke/);
	const callLog = fs.readFileSync(calls, "utf8");
	assert.match(callLog, /crabbox warmup --provider aws --ttl 20m --idle-timeout 6m --reclaim --timing-json/);
	assert.match(callLog, /crabbox run --id cbx_smoke --no-sync --timing-json -- uname -a/);
	assert.match(callLog, /crabbox stop cbx_smoke/);
});
