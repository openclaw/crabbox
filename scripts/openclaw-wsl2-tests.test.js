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

test("OpenClaw WSL2 test requires an explicit repository path", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-openclaw-wsl2-missing-repo-"));
	const bin = path.join(dir, "bin");
	const fakeCrabbox = path.join(bin, "crabbox");
	fs.mkdirSync(bin);
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf 'unexpected crabbox args: %s\\n' "$*" >&2
exit 99
`,
	);

	const env = {
		...process.env,
		PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
		HOME: dir,
		CRABBOX_LIVE: "1",
		CRABBOX_BIN: fakeCrabbox,
	};
	delete env.CRABBOX_OPENCLAW_REPO;
	delete env.CRABBOX_LIVE_REPO;

	const result = spawnSync("bash", ["scripts/openclaw-wsl2-tests.sh"], {
		cwd: repoRoot,
		env,
		encoding: "utf8",
	});

	assert.equal(result.status, 2, result.stdout + result.stderr);
	assert.match(result.stderr, /OpenClaw repo path is required/);
	assert.match(result.stderr, /CRABBOX_OPENCLAW_REPO=\/path\/to\/openclaw/);
	assert.doesNotMatch(result.stderr, /\/Users\/[^/\s]+/);
});

test("OpenClaw WSL2 test resolves generated-slug cleanup from list diff", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-openclaw-wsl2-"));
	const repo = path.join(dir, "repo");
	const bin = path.join(dir, "bin");
	const calls = path.join(dir, "calls.log");
	const warmupSlug = path.join(dir, "warmup-slug.txt");
	fs.mkdirSync(path.join(repo, ".git"), { recursive: true });
	fs.mkdirSync(bin);
	const fakeCrabbox = path.join(bin, "crabbox");
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "\${1:-}" in
  list)
    if [[ -f "\${CRABBOX_FAKE_WARMUP_SLUG}" ]]; then
      slug="$(cat "\${CRABBOX_FAKE_WARMUP_SLUG}")"
      printf '[{"id":0,"labels":{"lease":"cbx_created","slug":"%s"}}]\\n' "$slug"
    else
      printf '[]\\n'
    fi
    exit 0
    ;;
  warmup)
    while [[ "$#" -gt 0 ]]; do
      if [[ "$1" == "--slug" ]]; then
        printf '%s\\n' "$2" >"\${CRABBOX_FAKE_WARMUP_SLUG}"
        break
      fi
      shift
    done
    printf 'warmup succeeded without parseable lease\\n'
    exit 0
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

	const result = spawnSync("bash", ["scripts/openclaw-wsl2-tests.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
			HOME: dir,
			CRABBOX_LIVE: "1",
			CRABBOX_BIN: fakeCrabbox,
			CRABBOX_OPENCLAW_REPO: repo,
			CRABBOX_OPENCLAW_WSL2_STOP: "1",
			CRABBOX_FAKE_WARMUP_SLUG: warmupSlug,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1, result.stderr || result.stdout);
	assert.match(result.stderr, /cleanup resolved new lease cbx_created from list/);
	const log = fs.readFileSync(calls, "utf8");
	const warmup = log.match(/warmup .* --slug ([^ ]+)/);
	assert.ok(warmup, log);
	assert.match(warmup[1], /^wsl2-tests-/);
	assert.ok(warmup[1].length <= 41, `generated slug too long: ${warmup[1]}`);
	assert.match(log, /stop cbx_created/);
	assert.doesNotMatch(log, new RegExp(`stop ${warmup[1]}`));
	assert.doesNotMatch(log, /actions hydrate/);
});

test("OpenClaw WSL2 test does not stop an unconfirmed user-requested slug", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-openclaw-wsl2-"));
	const repo = path.join(dir, "repo");
	const bin = path.join(dir, "bin");
	const calls = path.join(dir, "calls.log");
	fs.mkdirSync(path.join(repo, ".git"), { recursive: true });
	fs.mkdirSync(bin);
	const fakeCrabbox = path.join(bin, "crabbox");
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "\${1:-}" in
  list)
    printf '[{"id":"cbx_old","slug":"shared-live-lease"}]\\n'
    exit 0
    ;;
  warmup)
    printf 'warmup succeeded without parseable lease\\n'
    exit 0
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

	const result = spawnSync("bash", ["scripts/openclaw-wsl2-tests.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
			HOME: dir,
			CRABBOX_LIVE: "1",
			CRABBOX_BIN: fakeCrabbox,
			CRABBOX_OPENCLAW_REPO: repo,
			CRABBOX_OPENCLAW_WSL2_STOP: "1",
			CRABBOX_OPENCLAW_WSL2_SLUG: "shared-live-lease",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1, result.stderr || result.stdout);
	assert.match(result.stderr, /refusing to stop unconfirmed requested slug shared-live-lease/);
	const log = fs.readFileSync(calls, "utf8");
	assert.match(log, /warmup --provider aws --target windows --windows-mode wsl2 --slug shared-live-lease/);
	assert.doesNotMatch(log, /stop shared-live-lease/);
});

test("OpenClaw WSL2 test refuses warmup after baseline list failure", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-openclaw-wsl2-"));
	const repo = path.join(dir, "repo");
	const bin = path.join(dir, "bin");
	const calls = path.join(dir, "calls.log");
	const listCount = path.join(dir, "list-count");
	fs.mkdirSync(path.join(repo, ".git"), { recursive: true });
	fs.mkdirSync(bin);
	const fakeCrabbox = path.join(bin, "crabbox");
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "\${1:-}" in
  list)
    count=0
    [[ -f "${listCount}" ]] && count="$(cat "${listCount}")"
    count="$((count + 1))"
    printf '%s\\n' "$count" >"${listCount}"
    if [[ "$count" == "1" ]]; then
      printf 'temporary list failure\\n' >&2
      exit 20
    fi
    printf '[{"id":"cbx_existing","slug":"wsl2-tests-000000-1-1"}]\\n'
    exit 0
    ;;
  warmup)
    printf 'warmup succeeded without parseable lease\\n'
    exit 0
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

	const result = spawnSync("bash", ["scripts/openclaw-wsl2-tests.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
			HOME: dir,
			CRABBOX_LIVE: "1",
			CRABBOX_BIN: fakeCrabbox,
			CRABBOX_OPENCLAW_REPO: repo,
			CRABBOX_OPENCLAW_WSL2_STOP: "1",
			CRABBOX_OPENCLAW_WSL2_SLUG: "wsl2-tests-000000-1-1",
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 1, result.stderr || result.stdout);
	assert.match(result.stderr, /refusing to create WSL2 lease without cleanup baseline/);
	const log = fs.readFileSync(calls, "utf8");
	assert.doesNotMatch(log, /warmup /);
	assert.doesNotMatch(log, /stop cbx_existing/);
	assert.doesNotMatch(log, /stop wsl2-tests-000000-1-1/);
});

test("OpenClaw WSL2 test selects lease id from timing JSON without stderr token pollution", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-openclaw-wsl2-"));
	const repo = path.join(dir, "repo");
	const bin = path.join(dir, "bin");
	const calls = path.join(dir, "calls.log");
	fs.mkdirSync(path.join(repo, ".git"), { recursive: true });
	fs.mkdirSync(bin);
	const fakeCrabbox = path.join(bin, "crabbox");
	writeExecutable(
		fakeCrabbox,
		`#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "\${1:-}" in
  list)
    printf '[]\\n'
    exit 0
    ;;
  warmup)
    printf 'prewarm complete id=cbx_correct\\n'
    printf 'warning: stale slug=old-lease cbx_badbadbadbad from diagnostics\\n' >&2
    printf '{"leaseId":"cbx_correct","provider":"aws","exitCode":0}\\n' >&2
    exit 0
    ;;
  actions)
    exit 0
    ;;
  run)
    exit 0
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

	const result = spawnSync("bash", ["scripts/openclaw-wsl2-tests.sh"], {
		cwd: repoRoot,
		env: {
			...process.env,
			PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
			HOME: dir,
			CRABBOX_LIVE: "1",
			CRABBOX_BIN: fakeCrabbox,
			CRABBOX_OPENCLAW_REPO: repo,
		},
		encoding: "utf8",
	});

	assert.equal(result.status, 0, result.stderr || result.stdout);
	const log = fs.readFileSync(calls, "utf8");
	assert.match(log, /actions hydrate --id cbx_correct/);
	assert.match(log, /run --id cbx_correct/);
	assert.doesNotMatch(log, /--id old-lease/);
	assert.doesNotMatch(log, /--id cbx_badbadbadbad/);
});
