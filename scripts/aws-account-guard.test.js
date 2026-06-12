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

function setupFakeAWS() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-aws-account-guard-"));
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin, { recursive: true });
  writeExecutable(
    path.join(bin, "aws"),
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "\${1:-}" == "--profile" ]]; then
  shift 2
fi
if [[ "$1 $2" == "sts get-caller-identity" ]]; then
  if [[ "\${CRABBOX_FAKE_STS_FAIL:-0}" == "1" ]]; then
    printf 'sts unavailable\\n' >&2
    exit 42
  fi
  if [[ "\${CRABBOX_FAKE_AWS_ACCOUNT_EMPTY:-0}" == "1" ]]; then
    printf '\\n'
    exit 0
  fi
  printf '%s\\n' "\${CRABBOX_FAKE_AWS_ACCOUNT:-123456789012}"
  exit 0
fi
if [[ "$1 $2" == "configure list-profiles" ]]; then
  if [[ "\${CRABBOX_FAKE_LIST_PROFILES_FAIL:-0}" == "1" ]]; then
    printf 'profiles unavailable\\n' >&2
    exit 55
  fi
  printf 'dev\\n'
  exit 0
fi
printf 'unexpected aws args: %s\\n' "$*" >&2
exit 99
`,
  );
  return { dir, bin };
}

function runGuard(body, env = {}) {
  const { dir, bin } = setupFakeAWS();
  return spawnSync("bash", ["-c", `source scripts/lib/aws-account-guard.sh\n${body}`], {
    cwd: repoRoot,
    env: {
      ...process.env,
      ...env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      TMPDIR: process.env.TMPDIR ?? dir,
    },
    encoding: "utf8",
  });
}

test("selected AWS profile guard fails closed when caller identity lookup fails", () => {
  for (const coordinatorAccount of ["", "123456789012"]) {
    const result = runGuard(
      `aws_guard_account_for_selected_profile "" "${coordinatorAccount}" "perform protected action"`,
      { CRABBOX_FAKE_STS_FAIL: "1" },
    );

    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.equal(result.stdout, "");
    assert.match(result.stderr, /failed to read local AWS caller identity/);
  }
});

test("selected AWS profile guard rejects empty caller identity output", () => {
  const result = runGuard(`aws_guard_account_for_selected_profile "" "" "perform protected action"`, {
    CRABBOX_FAKE_AWS_ACCOUNT_EMPTY: "1",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.equal(result.stdout, "");
  assert.match(result.stderr, /returned an empty account/);
});

test("AWS profile selector distinguishes profile enumeration failure", () => {
  const result = runGuard(`aws_guard_select_profile_for_account "123456789012" "apply policy"`, {
    CRABBOX_FAKE_STS_FAIL: "1",
    CRABBOX_FAKE_LIST_PROFILES_FAIL: "1",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.equal(result.stdout, "");
  assert.match(result.stderr, /failed to enumerate local AWS profiles/);
  assert.doesNotMatch(result.stderr, /no local AWS profile matches/);
});
