import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmod, mkdir, mkdtemp, readdir, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");

function runCoverage(env, threshold = "90.0") {
	return new Promise((resolve, reject) => {
		const child = spawn("bash", ["scripts/check-go-coverage.sh", threshold], {
			cwd: repoRoot,
			env: { ...process.env, ...env },
			stdio: ["ignore", "pipe", "pipe"],
		});
		let stdout = "";
		let stderr = "";
		child.stdout.setEncoding("utf8");
		child.stderr.setEncoding("utf8");
		child.stdout.on("data", (chunk) => {
			stdout += chunk;
		});
		child.stderr.on("data", (chunk) => {
			stderr += chunk;
		});
		child.on("error", reject);
		child.on("close", (code) => resolve({ code, stdout, stderr }));
	});
}

test("Go coverage check isolates concurrent profile files", async () => {
	const dir = await mkdtemp(path.join(os.tmpdir(), "crabbox-go-coverage-test-"));
	const bin = path.join(dir, "bin");
	await mkdir(bin);
	const fakeGo = path.join(bin, "go");
	await writeFile(
		fakeGo,
		`#!/usr/bin/env bash
set -euo pipefail
if [[ "\${1:-}" == "test" ]]; then
  profile=""
  for arg in "$@"; do
    case "$arg" in
      -coverprofile=*) profile="\${arg#-coverprofile=}" ;;
    esac
  done
  [[ -n "$profile" ]] || exit 64
  {
    printf 'mode: atomic\\n'
    printf 'github.com/openclaw/crabbox/internal/cli/bootstrap.go:1.1,1.2 1 %s\\n' "\${CRABBOX_FAKE_COVERAGE:?}"
  } >"$profile"
  sleep "\${CRABBOX_FAKE_GO_DELAY:-0}"
  exit 0
fi
if [[ "\${1:-}" == "tool" && "\${2:-}" == "cover" ]]; then
  profile="\${3#-func=}"
  coverage="$(awk 'NR == 2 { print $3 }' "$profile")"
  printf 'total:\\t(statements)\\t%s%%\\n' "$coverage"
  exit 0
fi
printf 'unexpected go args: %s\\n' "$*" >&2
exit 64
`,
	);
	await chmod(fakeGo, 0o755);

	const sharedEnv = {
		PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
		TMPDIR: dir,
	};
	const first = runCoverage({ ...sharedEnv, CRABBOX_FAKE_COVERAGE: "91.0", CRABBOX_FAKE_GO_DELAY: "0.4" });
	await new Promise((resolve) => setTimeout(resolve, 50));
	const second = runCoverage({ ...sharedEnv, CRABBOX_FAKE_COVERAGE: "97.0" });
	const [firstResult, secondResult] = await Promise.all([first, second]);

	assert.equal(firstResult.code, 0, firstResult.stderr || firstResult.stdout);
	assert.equal(secondResult.code, 0, secondResult.stderr || secondResult.stdout);
	assert.match(firstResult.stdout, /Go core coverage 91\.0% >= 90\.0%/);
	assert.match(secondResult.stdout, /Go core coverage 97\.0% >= 90\.0%/);
	const leftovers = await readdir(dir);
	assert.equal(leftovers.includes("crabbox-go-coverage.out"), false);
	assert.equal(leftovers.includes("crabbox-go-core-coverage.out"), false);
});

test("Go coverage check rejects invalid thresholds", async () => {
	for (const threshold of ["abc", "-1", "101"]) {
		const result = await runCoverage({}, threshold);
		assert.equal(result.code, 2, `threshold=${threshold} stdout=${result.stdout} stderr=${result.stderr}`);
		assert.match(result.stderr, new RegExp(`invalid coverage threshold: ${threshold.replace("-", "\\-")}`));
	}
});
