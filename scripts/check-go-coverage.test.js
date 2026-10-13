import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { promisify } from "node:util";

const repoRoot = path.resolve(import.meta.dirname, "..");

function runCoverage(env, threshold = "90.0", args = [], cwd = repoRoot) {
	return new Promise((resolve, reject) => {
		const child = spawn("bash", [path.join(repoRoot, "scripts/check-go-coverage.sh"), threshold, ...args], {
			cwd,
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

test("Go coverage check isolates concurrent profile files", async (t) => {
	const dir = await mkdtemp(path.join(os.tmpdir(), "crabbox-go-coverage-test-"));
	t.after(() => rm(dir, { recursive: true, force: true }));
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

test("native Go merges complementary atomic profiles and rejects incomplete or corrupt inputs", async (t) => {
	const dir = await mkdtemp(path.join(os.tmpdir(), "crabbox-coverage-native-"));
	t.after(() => rm(dir, { recursive: true, force: true }));
	const profiles = path.join(dir, "profiles");
	await mkdir(profiles);
	await mkdir(path.join(dir, "internal/cli"), { recursive: true });
	await mkdir(path.join(dir, "other"));
	await writeFile(path.join(dir, "go.mod"), "module github.com/openclaw/crabbox\n\ngo 1.26.0\n");
	await writeFile(path.join(dir, "internal/cli/bootstrap.go"), `package cli
func branch(yes bool) int { if yes { return 1 }; return 2 }
`);
	await writeFile(path.join(dir, "internal/cli/bootstrap_test.go"), `package cli
import "testing"
func TestTrue(t *testing.T) { if branch(true) != 1 { t.Fatal("true") } }
func TestFalse(t *testing.T) { if branch(false) != 2 { t.Fatal("false") } }
`);
	await writeFile(path.join(dir, "other/other.go"), "package other\nfunc value() int { return 3 }\n");
	await writeFile(path.join(dir, "other/other_test.go"), "package other\nimport \"testing\"\nfunc TestValue(t *testing.T) { if value() != 3 { t.Fatal(\"value\") } }\n");
	const env = { ...process.env, GOPROXY: "off", GOTOOLCHAIN: "local", GOWORK: "off", GOFLAGS: "-mod=readonly" };
	const go = (args) => promisify(execFile)("go", args, { cwd: dir, env });
	for (const [name, pkg, selection] of [
		["cli-0", "./internal/cli", "^TestTrue$"],
		["cli-1", "./internal/cli", "^TestFalse$"],
		["cli-2", "./internal/cli", "^$"],
		["core", "./other", "."],
	]) {
		await go(["test", "-count=1", "-covermode=atomic", `-coverprofile=${profiles}/${name}.out`, "-run", selection, pkg]);
	}
	const check = () => runCoverage(env, "90.0", ["--profiles", profiles], dir);
	const merged = await check();
	assert.equal(merged.code, 0, merged.stderr);
	assert.match(merged.stdout, /Go core coverage 100\.0% >= 90\.0%/);
	const standalone = await runCoverage(env, "90.0", [], dir);
	assert.equal(standalone.code, 0, standalone.stderr);
	assert.match(standalone.stdout, /Go core coverage 100\.0% >= 90\.0%/);
	const original = await readFile(path.join(profiles, "cli-1.out"), "utf8");
	const first = await readFile(path.join(profiles, "cli-0.out"), "utf8");
	const core = await readFile(path.join(profiles, "core.out"), "utf8");
	await writeFile(path.join(profiles, "cli-1.out"), first);
	const uncovered = await check();
	assert.equal(uncovered.code, 1);
	assert.match(uncovered.stdout, /below 90\.0%/);
	await writeFile(path.join(profiles, "cli-1.out"), original);
	for (const content of ["", "mode: atomic\n", core.replace("mode: atomic", "mode: set"), core + "malformed excluded record\n"]) {
		await writeFile(path.join(profiles, "core.out"), content);
		assert.notEqual((await check()).code, 0, JSON.stringify(content));
	}
	await writeFile(path.join(profiles, "core.out"), core);
	await writeFile(path.join(profiles, "cli-1.out"), original.replace(/ (\d+) (\d+)\n/, (_, statements, count) => ` ${Number(statements) + 1} ${count}\n`));
	const inconsistent = await check();
	assert.notEqual(inconsistent.code, 0);
	assert.match(inconsistent.stderr, /inconsistent NumStmt/);
	await writeFile(path.join(profiles, "cli-1.out"), original);
	for (const name of ["core", "cli-0", "cli-1", "cli-2"]) {
		const file = path.join(profiles, `${name}.out`);
		const content = await readFile(file);
		await rm(file);
		assert.notEqual((await check()).code, 0, `missing ${name}`);
		await writeFile(file, content);
	}
});
