import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..");
const auditScript = path.join(scriptDir, "macos-coordinator-remediation-audit.sh");

async function setup(account = "123456789012") {
  const dir = await mkdtemp(path.join(os.tmpdir(), "crabbox-macos-remediation-audit-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const fakeAWS = path.join(bin, "aws");
  const artifacts = path.join(dir, "artifacts");
  await mkdir(bin);
  await writeFile(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2 $3" == "admin providers identity" ]]; then
  if [[ "\${CRABBOX_FAKE_IDENTITY_FAIL:-0}" == "1" ]]; then
    printf 'identity failed\\n' >&2
    exit 1
  fi
  printf '{"account":"%s","arn":"arn:aws:iam::%s:user/crabbox-runner","region":"eu-west-1","policyTarget":{"type":"user","name":"crabbox-runner","source":"iam-user"}}\\n' "${account}" "${account}"
  exit 0
fi
if [[ "$1 $2 $3" == "admin providers policy" ]]; then
  printf '{"Statement":[{"Action":["ec2:RunInstances","ec2:AllocateHosts"]}]}\\n'
  exit 0
fi
if [[ "$1 $2 $3" == "admin hosts quota" ]]; then
  value="\${CRABBOX_FAKE_QUOTA_VALUE:-0}"
  printf '[{"serviceCode":"ec2","quotaCode":"L-5D8DADF5","quotaName":"Running Dedicated mac2 Hosts","value":%s,"adjustable":true,"unit":"None"}]\\n' "$value"
  exit 0
fi
if [[ "$1 $2 $3" == "admin hosts allocate" ]]; then
  if [[ "\${CRABBOX_FAKE_DRY_OK:-0}" == "1" ]]; then
    printf '[{"region":"eu-west-1","availabilityZone":"eu-west-1a","instanceType":"mac2.metal","ok":true,"message":"DryRunOperation"}]\\n'
  else
    printf '[{"region":"eu-west-1","availabilityZone":"eu-west-1a","instanceType":"mac2.metal","ok":false,"message":"UnauthorizedOperation: coordinator AWS identity needs EC2 Mac host lifecycle permissions, including ec2:AllocateHosts and ec2:CreateTags"}]\\n'
  fi
  exit 0
fi
printf 'unexpected crabbox args: %s\\n' "$*" >&2
exit 2
`,
  );
  await writeFile(
    fakeAWS,
    `#!/usr/bin/env bash
set -euo pipefail
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
  if [[ -n "\${CRABBOX_FAKE_MATCH_PROFILE:-}" && "$profile" == "\${CRABBOX_FAKE_MATCH_PROFILE}" ]]; then
    printf '%s\\n' "${account}"
  else
    printf '%s\\n' "\${CRABBOX_FAKE_AWS_ACCOUNT:-${account}}"
  fi
  exit 0
fi
if [[ "$1" == "iam" || "$1" == "service-quotas" ]]; then
  printf '{"ok":true}\\n'
  exit 0
fi
printf 'unexpected aws args: %s\\n' "$*" >&2
exit 2
`,
  );
  await chmod(fakeCrabbox, 0o755);
  await chmod(fakeAWS, 0o755);
  return { dir, bin, fakeCrabbox, artifacts };
}

function runAudit(ctx, env = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(
      "bash",
      [
        auditScript,
        "--artifact-dir",
        ctx.artifacts,
        "--region",
        "eu-west-1",
        "--type",
        "mac2.metal",
      ],
      {
        cwd: repoRoot,
        env: {
          ...process.env,
          PATH: `${ctx.bin}:${process.env.PATH}`,
          CRABBOX_BIN: ctx.fakeCrabbox,
          ...env,
        },
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

test("macOS coordinator remediation audit records IAM, quota, and local profile blockers", async () => {
  const ctx = await setup("123456789012");
  const result = await runAudit(ctx, {
    CRABBOX_FAKE_AWS_ACCOUNT: "999999999999",
    CRABBOX_FAKE_AWS_PROFILES: "default\nprod",
  });

  assert.equal(result.code, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /macOS coordinator remediation audit:/);
  const summary = JSON.parse(await readFile(path.join(ctx.artifacts, "summary.json"), "utf8"));
  assert.equal(summary.result, "blocked");
  assert.equal(summary.ready.hostDryRun, false);
  assert.equal(summary.ready.hostQuota, false);
  assert.equal(summary.ready.localCoordinatorAWSProfile, false);
  assert.ok(summary.blockers.includes("host-iam"));
	assert.ok(summary.blockers.includes("host-quota"));
	assert.ok(summary.blockers.includes("local-coordinator-aws-profile"));
	assert.ok(
		summary.remediation.commands.includes(
			"scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto",
		),
	);
	assert.ok(
		summary.remediation.commands.includes(
			"scripts/request-macos-host-quota.sh --identity provider-identity.json --quota mac-host-quota.json --region eu-west-1 --profile auto",
		),
	);
	assert.doesNotMatch(summary.remediation.commands.join("\n"), new RegExp(repoRoot.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
	assert.equal(summary.evidence.providerIdentity.stdout, "evidence/provider-identity.out");
	assert.equal(summary.evidence.macHostDryRun.status, 0);
	assert.match(summary.evidence.macHostDryRun.stdoutText, /UnauthorizedOperation/);
});

test("macOS coordinator remediation audit reports ready when dry-run, quota, and account guard pass", async () => {
  const ctx = await setup("123456789012");
  const result = await runAudit(ctx, {
    CRABBOX_FAKE_DRY_OK: "1",
    CRABBOX_FAKE_QUOTA_VALUE: "1",
    CRABBOX_FAKE_AWS_PROFILES: "",
  });

  assert.equal(result.code, 0, result.stdout + result.stderr);
  const summary = JSON.parse(await readFile(path.join(ctx.artifacts, "summary.json"), "utf8"));
  assert.equal(summary.result, "ready-for-paid-smoke");
  assert.equal(summary.ready.hostDryRun, true);
  assert.equal(summary.ready.hostQuota, true);
  assert.equal(summary.ready.localCoordinatorAWSProfile, true);
  assert.deepEqual(summary.blockers, []);
  assert.match(summary.evidence.iamApplyDryRun.stdoutText, /dry-run: aws iam put-user-policy/);
  assert.match(summary.evidence.quotaRequestDryRun.stdoutText, /quota already sufficient/);
});

test("macOS coordinator remediation audit blocks when required evidence failed", async () => {
  const ctx = await setup("123456789012");
  const result = await runAudit(ctx, {
    CRABBOX_FAKE_DRY_OK: "1",
    CRABBOX_FAKE_QUOTA_VALUE: "1",
    CRABBOX_FAKE_IDENTITY_FAIL: "1",
  });

  assert.equal(result.code, 1, result.stdout + result.stderr);
  const summary = JSON.parse(await readFile(path.join(ctx.artifacts, "summary.json"), "utf8"));
  assert.equal(summary.result, "blocked");
  assert.equal(summary.ready.hostDryRun, true);
  assert.equal(summary.ready.hostQuota, true);
  assert.ok(summary.blockers.includes("provider-identity"));
  assert.ok(summary.blockers.includes("iam-apply-dry-run"));
  assert.ok(summary.blockers.includes("quota-request-dry-run"));
});

test("macOS coordinator remediation audit reports explicit profile account mismatch", async () => {
  const ctx = await setup("123456789012");
  const result = await runAudit(ctx, {
    CRABBOX_FAKE_DRY_OK: "1",
    CRABBOX_FAKE_QUOTA_VALUE: "1",
    CRABBOX_FAKE_AWS_ACCOUNT: "999999999999",
    CRABBOX_MACOS_REMEDIATION_PROFILE: "prod",
  });

  assert.equal(result.code, 1, result.stdout + result.stderr);
  const summary = JSON.parse(await readFile(path.join(ctx.artifacts, "summary.json"), "utf8"));
  assert.equal(summary.result, "blocked");
  assert.equal(summary.ready.hostDryRun, true);
  assert.equal(summary.ready.hostQuota, true);
  assert.equal(summary.ready.localCoordinatorAWSProfile, false);
  assert.ok(summary.blockers.includes("local-coordinator-aws-profile"));
  assert.match(summary.evidence.iamApplyDryRun.stderrText, /does not match coordinator account/);
});
