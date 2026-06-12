import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, realpath, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..");
const quotaScript = path.join(scriptDir, "request-macos-host-quota.sh");

async function setup(account = "123456789012", quotaValue = 0, adjustable = true, quotaEntries = null) {
  const dir = await mkdtemp(path.join(os.tmpdir(), "crabbox-quota-request-test-"));
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
if [[ "$1" == "service-quotas" && "$2" == "request-service-quota-increase" ]]; then
  printf '{"RequestedQuota":{"Id":"case-123"}}\\n'
  exit 0
fi
printf 'unexpected aws args: %s\\n' "$*" >&2
exit 2
`,
  );
  await chmod(aws, 0o755);
  const identity = path.join(dir, "provider-identity.json");
  const quota = path.join(dir, "mac-host-quota.json");
  await writeFile(
    identity,
    JSON.stringify({
      account,
      arn: `arn:aws:iam::${account}:user/crabbox-runner`,
      region: "eu-west-1",
      policyTarget: { type: "user", name: "crabbox-runner", source: "iam-user" },
    }),
  );
  await writeFile(
    quota,
    JSON.stringify(
      quotaEntries ?? [
        {
          serviceCode: "ec2",
          quotaCode: "L-5D8DADF5",
          quotaName: "Running Dedicated mac2 Hosts",
          value: quotaValue,
          adjustable,
          unit: "None",
        },
      ],
    ),
  );
  return { dir, bin, log, identity, quota };
}

function runQuota(ctx, args = [], env = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(
      "bash",
      [quotaScript, "--quota", ctx.quota, "--region", "eu-west-1", ...args],
      {
        cwd: repoRoot,
        env: { ...process.env, PATH: `${ctx.bin}:${process.env.PATH}`, ...env },
        stdio: ["ignore", "pipe", "pipe"],
      },
    );
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

test("quota request helper dry-runs the Service Quotas request", async () => {
  const ctx = await setup();
  const result = await runQuota(ctx, ["--identity", ctx.identity, "--desired-value", "1"]);

  assert.equal(result.code, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /quota=Running Dedicated mac2 Hosts/);
  assert.match(result.stdout, /current_value=0/);
  assert.match(result.stdout, /desired_value=1/);
  assert.match(result.stdout, /coordinator_account=123456789012/);
  assert.match(result.stdout, /dry-run: aws service-quotas request-service-quota-increase/);
  assert.match(result.stdout, /--quota-code L-5D8DADF5/);
  assert.match(result.stdout, /--region eu-west-1/);

  const log = await readFile(ctx.log, "utf8");
  assert.match(log, /sts get-caller-identity/);
  assert.doesNotMatch(log, /request-service-quota-increase/);
});

test("quota request helper submits only with --apply and account guard", async () => {
  const ctx = await setup();
  const result = await runQuota(ctx, ["--identity", ctx.identity, "--profile", "prod", "--apply"]);

  assert.equal(result.code, 0, result.stdout + result.stderr);
  const log = await readFile(ctx.log, "utf8");
  assert.match(log, /--profile prod sts get-caller-identity --query Account --output text/);
  assert.match(log, /--profile prod service-quotas request-service-quota-increase --service-code ec2 --quota-code L-5D8DADF5 --desired-value 1 --region eu-west-1/);
});

test("quota request helper ignores unrelated quotas before requesting", async () => {
  const ctx = await setup("123456789012", 0, true, [
    {
      serviceCode: "vpc",
      quotaCode: "L-UNRELATED",
      quotaName: "VPCs per Region",
      value: 0,
      adjustable: true,
      unit: "None",
    },
    {
      serviceCode: "ec2",
      quotaCode: "L-5D8DADF5",
      quotaName: "Running Dedicated mac2 Hosts",
      value: 0,
      adjustable: true,
      unit: "None",
    },
  ]);
  const result = await runQuota(ctx, ["--identity", ctx.identity, "--profile", "prod", "--apply"]);

  assert.equal(result.code, 0, result.stdout + result.stderr);
  const log = await readFile(ctx.log, "utf8");
  assert.match(log, /request-service-quota-increase --service-code ec2 --quota-code L-5D8DADF5/);
  assert.doesNotMatch(log, /L-UNRELATED/);
});

test("quota request helper refuses ambiguous Mac host quota files", async () => {
  const ctx = await setup("123456789012", 0, true, [
    {
      serviceCode: "ec2",
      quotaCode: "L-5D8DADF5",
      quotaName: "Running Dedicated mac2 Hosts",
      value: 0,
      adjustable: true,
      unit: "None",
    },
    {
      serviceCode: "ec2",
      quotaCode: "L-A8448DC5",
      quotaName: "Running Dedicated mac1 Hosts",
      value: 0,
      adjustable: true,
      unit: "None",
    },
  ]);
  const result = await runQuota(ctx, ["--identity", ctx.identity, "--profile", "prod", "--apply"]);

  assert.equal(result.code, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /multiple EC2 Mac host quotas/);
  const log = await readFile(ctx.log, "utf8").catch(() => "");
  assert.doesNotMatch(log, /request-service-quota-increase/);
});

test("quota request helper auto-selects a matching profile", async () => {
  const ctx = await setup("123456789012");
  const result = await runQuota(ctx, ["--identity", ctx.identity, "--profile", "auto"], {
    CRABBOX_FAKE_AWS_ACCOUNT: "999999999999",
    CRABBOX_FAKE_AWS_PROFILES: "default\nprod",
    CRABBOX_FAKE_MATCH_PROFILE: "prod",
  });

  assert.equal(result.code, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /checked_profile=default account=999999999999/);
  assert.match(result.stderr, /checked_profile=prod account=123456789012/);
  assert.match(result.stdout, /aws_profile=prod/);
  assert.match(result.stdout, /dry-run: aws --profile prod service-quotas request-service-quota-increase/);
});

test("quota request helper auto-selects default credentials", async () => {
  const ctx = await setup("123456789012");
  const result = await runQuota(ctx, ["--identity", ctx.identity, "--profile", "auto"], {
    CRABBOX_FAKE_AWS_PROFILES: "",
  });

  assert.equal(result.code, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /checked_profile=default-credentials account=123456789012/);
  assert.doesNotMatch(result.stdout, /aws_profile=/);
  assert.match(result.stdout, /dry-run: aws service-quotas request-service-quota-increase/);
});

test("quota request helper refuses mismatched accounts", async () => {
  const ctx = await setup("123456789012");
  const result = await runQuota(ctx, ["--identity", ctx.identity, "--apply"], {
    CRABBOX_FAKE_AWS_ACCOUNT: "999999999999",
  });

  assert.equal(result.code, 1);
  assert.match(result.stderr, /local AWS account 999999999999 does not match coordinator account 123456789012/);
  const log = await readFile(ctx.log, "utf8");
  assert.doesNotMatch(log, /request-service-quota-increase/);
});

test("quota request helper exits cleanly when quota is already sufficient", async () => {
  const ctx = await setup("123456789012", 2);
  const result = await runQuota(ctx, ["--desired-value", "1"]);

  assert.equal(result.code, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /quota already sufficient/);
  const log = await readFile(ctx.log, "utf8").catch(() => "");
  assert.equal(log, "");
});

test("quota request helper exits cleanly when non-adjustable quota is already sufficient", async () => {
  const ctx = await setup("123456789012", 2, false);
  const result = await runQuota(ctx, ["--desired-value", "1"]);

  assert.equal(result.code, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /quota already sufficient/);
  const log = await readFile(ctx.log, "utf8").catch(() => "");
  assert.equal(log, "");
});

test("quota request helper refuses non-adjustable quotas", async () => {
  const ctx = await setup("123456789012", 0, false);
  const result = await runQuota(ctx);

  assert.equal(result.code, 1);
  assert.match(result.stderr, /quota is not adjustable/);
});
