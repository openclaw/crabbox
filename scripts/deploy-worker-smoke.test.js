import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { writeExecutable } from "./test-support/smoke-fixtures.mjs";

const repoRoot = path.resolve(import.meta.dirname, "..");

for (const scenario of [
	{ name: "success", warmupStatus: 0, runStatus: 0, stopStatus: 0, expectedStatus: 0, stops: 1 },
	{ name: "command failure", warmupStatus: 0, runStatus: 17, stopStatus: 0, expectedStatus: 17, stops: 1 },
	{ name: "unavailable instance", warmupStatus: 23, runStatus: 0, stopStatus: 0, expectedStatus: 23, stops: 0 },
	{ name: "cleanup failure", warmupStatus: 0, runStatus: 0, stopStatus: 29, expectedStatus: 29, stops: 2 },
]) {
	test(`deploy worker smoke pins the smallest supported AWS instance: ${scenario.name}`, (t) => {
		const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-deploy-worker-smoke-"));
		t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
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
validate_smoke_target() {
  local type="\${CRABBOX_SERVER_TYPE:-}"
  local target="\${CRABBOX_TARGET:-}"
  local arch="\${CRABBOX_ARCH:-}"
  while [[ "$#" -gt 0 ]]; do
    case "$1" in
      --type) type="$2"; shift ;;
      --target) target="$2"; shift ;;
      --arch) arch="$2"; shift ;;
    esac
    shift
  done
  [[ "$type" == "t3.small" && "$target" == "linux" && "$arch" == "amd64" ]] || exit 64
}
case "\${1:-}" in
  warmup)
    validate_smoke_target "$@"
    [[ "${scenario.warmupStatus}" == "0" ]] || exit ${scenario.warmupStatus}
    sleep 0.1
    printf '{"leaseId":"cbx_smoke"}\\n' >&2
    ;;
  run)
    validate_smoke_target "$@"
    exit ${scenario.runStatus}
    ;;
  stop)
    exit ${scenario.stopStatus}
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
				CRABBOX_COORDINATOR: "https://broker.crabbox.test",
				CRABBOX_DEPLOY_SMOKE_URLS: "https://broker.crabbox.test/v1/health",
				CRABBOX_DEPLOY_SMOKE_AWS: "1",
				CRABBOX_LIVE_REPO: liveRepo,
				CRABBOX_SERVER_TYPE: "c7a.8xlarge",
				CRABBOX_TARGET: "windows",
				CRABBOX_ARCH: "arm64",
			},
			encoding: "utf8",
			timeout: 10_000,
		});

		assert.equal(result.status, scenario.expectedStatus, result.stderr || result.stdout);
		const callLog = fs.readFileSync(calls, "utf8");
		const warmups = callLog.split("\n").filter((line) => line.startsWith("crabbox warmup "));
		assert.deepEqual(warmups, [
			"crabbox warmup --provider aws --target linux --arch amd64 --class tiny --type t3.small --market on-demand --ttl 20m --idle-timeout 6m --reclaim --timing-json",
		]);
		assert.equal(callLog.split("\n").filter((line) => line === "crabbox stop cbx_smoke").length, scenario.stops);
		if (scenario.warmupStatus !== 0) {
			assert.doesNotMatch(callLog, /crabbox (run|stop) /);
			return;
		}
		assert.match(result.stdout, /aws deploy smoke lease=cbx_smoke/);
		assert.match(callLog, /^crabbox run --provider aws --target linux --arch amd64 --class tiny --type t3.small --market on-demand --id cbx_smoke --no-sync --timing-json -- uname -a$/m);
	});
}
