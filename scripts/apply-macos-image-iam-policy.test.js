import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, realpath, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..");
const applyScript = path.join(scriptDir, "apply-macos-image-iam-policy.sh");

async function setup(targetType = "user", account = "123456789012") {
  const dir = await mkdtemp(path.join(os.tmpdir(), "crabbox-iam-apply-test-"));
  const bin = path.join(dir, "bin");
  const log = path.join(dir, "aws.log");
  const aws = path.join(bin, "aws");
  await mkdir(bin);
  await writeFile(
    aws,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
if [[ "$1" == "configure" && "$2" == "list-profiles" ]]; then
  printf '%s\\n' \${CRABBOX_FAKE_AWS_PROFILES:-default}
  exit 0
fi
profile=""
if [[ "\${1:-}" == "--profile" ]]; then
  profile="$2"
  shift 2
fi
if [[ "$1" == "sts" && "$2" == "get-caller-identity" ]]; then
  if [[ -n "\${CRABBOX_FAKE_UNUSABLE_PROFILE:-}" && "$profile" == "\${CRABBOX_FAKE_UNUSABLE_PROFILE}" ]]; then
    printf 'profile is unusable\\n' >&2
    exit 254
  fi
  if [[ -n "\${CRABBOX_FAKE_MATCH_PROFILE:-}" && "$profile" == "\${CRABBOX_FAKE_MATCH_PROFILE}" ]]; then
    printf '%s\\n' "${account}"
  else
    printf '%s\\n' "\${CRABBOX_FAKE_AWS_ACCOUNT:-${account}}"
  fi
  exit 0
fi
if [[ "$1" == "iam" && ( "$2" == "put-user-policy" || "$2" == "put-role-policy" ) ]]; then
  printf '{"ok":true}\\n'
  exit 0
fi
printf 'unexpected aws args: %s\\n' "$*" >&2
exit 2
`,
  );
  await chmod(aws, 0o755);
  const identity = path.join(dir, "provider-identity.json");
  const policy = path.join(dir, "macos-image-policy.json");
  await writeFile(
    identity,
    JSON.stringify({
      account,
      arn:
        targetType === "role"
          ? `arn:aws:iam::${account}:role/crabbox-runner`
          : `arn:aws:iam::${account}:user/crabbox-runner`,
      region: "eu-west-1",
      policyTarget: { type: targetType, name: "crabbox-runner", source: `iam-${targetType}` },
    }),
  );
  await writeFile(policy, '{"Statement":[{"Action":["ec2:RunInstances","ec2:AllocateHosts"]}]}\n');
  return { dir, bin, log, identity, policy };
}

function runApply(ctx, args = [], env = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn("bash", [applyScript, "--identity", ctx.identity, "--policy", ctx.policy, ...args], {
      cwd: repoRoot,
      env: { ...process.env, PATH: `${ctx.bin}:${process.env.PATH}`, ...env },
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

test("IAM apply helper dry-runs user policy with account guard", async () => {
  const ctx = await setup("user");
  const result = await runApply(ctx);

  assert.equal(result.code, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /coordinator_account=123456789012/);
  assert.match(result.stdout, /policy_target=user\/crabbox-runner/);
  assert.match(result.stdout, /dry-run: aws iam put-user-policy/);
  assert.match(result.stdout, /--user-name crabbox-runner/);
  const policyPath = await realpath(ctx.policy);
  assert.match(result.stdout, new RegExp(`file://${policyPath.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}`));

  const log = await readFile(ctx.log, "utf8");
  assert.match(log, /sts get-caller-identity/);
  assert.doesNotMatch(log, /put-user-policy/);
});

test("IAM apply helper writes user policy only with --apply", async () => {
  const ctx = await setup("user");
  const result = await runApply(ctx, ["--apply", "--profile", "prod"]);

  assert.equal(result.code, 0, result.stdout + result.stderr);
  const log = await readFile(ctx.log, "utf8");
  assert.match(log, /--profile prod sts get-caller-identity --query Account --output text/);
  assert.match(log, /--profile prod iam put-user-policy --user-name crabbox-runner/);
});

test("IAM apply helper auto-selects matching profile", async () => {
  const ctx = await setup("user", "123456789012");
  const result = await runApply(ctx, ["--profile", "auto"], {
    CRABBOX_FAKE_AWS_ACCOUNT: "999999999999",
    CRABBOX_FAKE_AWS_PROFILES: "default\nprod",
    CRABBOX_FAKE_MATCH_PROFILE: "prod",
  });

  assert.equal(result.code, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /checked_profile=default account=999999999999/);
  assert.match(result.stderr, /checked_profile=prod account=123456789012/);
  assert.match(result.stdout, /aws_profile=prod/);
  assert.match(result.stdout, /dry-run: aws --profile prod iam put-user-policy/);
});

test("IAM apply helper reports when auto profile has no match", async () => {
  const ctx = await setup("user", "123456789012");
  const result = await runApply(ctx, ["--profile", "auto"], {
    CRABBOX_FAKE_AWS_ACCOUNT: "999999999999",
    CRABBOX_FAKE_AWS_PROFILES: "default\nprod",
    CRABBOX_FAKE_UNUSABLE_PROFILE: "default",
  });

  assert.equal(result.code, 1);
  assert.match(result.stderr, /checked_profile=default status=unusable/);
  assert.match(result.stderr, /checked_profile=prod account=999999999999/);
  assert.match(result.stderr, /no local AWS profile matches coordinator account 123456789012 after checking 2 profile\(s\)/);
  const log = await readFile(ctx.log, "utf8");
  assert.doesNotMatch(log, /put-user-policy/);
});

test("IAM apply helper writes role policy for role targets", async () => {
  const ctx = await setup("role");
  const result = await runApply(ctx, ["--apply"]);

  assert.equal(result.code, 0, result.stdout + result.stderr);
  const log = await readFile(ctx.log, "utf8");
  assert.match(log, /iam put-role-policy --role-name crabbox-runner/);
});

test("IAM apply helper refuses mismatched AWS account", async () => {
  const ctx = await setup("user", "123456789012");
  const result = await runApply(ctx, ["--apply"], { CRABBOX_FAKE_AWS_ACCOUNT: "999999999999" });

  assert.equal(result.code, 1);
  assert.match(result.stderr, /local AWS account 999999999999 does not match coordinator account 123456789012/);
  const log = await readFile(ctx.log, "utf8");
  assert.doesNotMatch(log, /put-user-policy/);
});
