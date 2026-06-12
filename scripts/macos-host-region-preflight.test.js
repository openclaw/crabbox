import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmod, mkdtemp, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..");
const preflightScript = path.join(scriptDir, "macos-host-region-preflight.sh");

async function makeFakeCrabbox(dir) {
  const fake = path.join(dir, "fake-crabbox");
  await writeFile(
    fake,
    `#!/usr/bin/env bash
set -euo pipefail

region=""
type="mac2.metal"
for ((i = 1; i <= $#; i++)); do
  if [[ "\${!i}" == "--region" ]]; then
    next=$((i + 1))
    region="\${!next}"
  elif [[ "\${!i}" == "--type" ]]; then
    next=$((i + 1))
    type="\${!next}"
  fi
done

if [[ "\${CRABBOX_FAKE_STDERR_DIAGNOSTICS:-}" == "1" ]]; then
  printf 'warning: diagnostic for %s %s\\n' "$region" "$type" >&2
fi

if [[ "$1" == "admin" && ( "$2" == "mac-hosts" || "$2" == "hosts" ) ]]; then
  case "$3" in
    list)
      if [[ "\${CRABBOX_FAKE_EXISTING_REGION:-}" == "$region" && "\${CRABBOX_FAKE_EXISTING_TYPE:-mac2.metal}" == "$type" ]]; then
        printf '[{"id":"h-existing","instanceType":"%s","state":"available","region":"%s"}]\\n' "$type" "$region"
      else
        printf '[]\\n'
      fi
      ;;
    allocate)
      if [[ " $* " == *" --dry-run "* ]]; then
        if [[ "\${CRABBOX_FAKE_DRY_REGION:-}" == "$region" && "\${CRABBOX_FAKE_DRY_TYPE:-mac2.metal}" == "$type" ]]; then
          printf '[{"region":"%s","availabilityZone":"%sa","instanceType":"%s","ok":true,"message":"DryRunOperation"}]\\n' "$region" "$region" "$type"
        elif [[ "$region" == "eu-central-1" ]]; then
          printf 'coordinator POST /v1/admin/mac-hosts/dry-run?region=%s: http 400: {"error":"no_mac_host_offerings"}\\n' "$region" >&2
          exit 1
        else
          printf '[{"region":"%s","availabilityZone":"%sa","instanceType":"%s","ok":false,"message":"UnauthorizedOperation: coordinator AWS identity needs EC2 Mac host lifecycle permissions, including ec2:AllocateHosts and ec2:CreateTags"}]\\n' "$region" "$region" "$type"
        fi
      fi
      ;;
    quota)
      if [[ "\${CRABBOX_FAKE_QUOTA_FAIL_REGION:-}" == "$region" ]]; then
        printf 'coordinator GET /v1/admin/mac-hosts/quota?region=%s&type=%s: http 502: {"error":"mac_host_quota_failed","message":"AWS authorization failure: coordinator AWS identity needs servicequotas:ListServiceQuotas to inspect EC2 Mac Dedicated Host quotas"}\\n' "$region" "$type" >&2
        exit 1
      fi
      if [[ "\${CRABBOX_FAKE_QUOTA_ZERO_REGION:-}" == "$region" ]]; then
        printf '[{"serviceCode":"ec2","quotaCode":"L-MAC","quotaName":"Running Dedicated %s Hosts","value":0,"adjustable":true,"globalQuota":false,"unit":"None"}]\\n' "\${type%.metal}"
      else
        printf '[{"serviceCode":"ec2","quotaCode":"L-MAC","quotaName":"Running Dedicated %s Hosts","value":1,"adjustable":true,"globalQuota":false,"unit":"None"}]\\n' "\${type%.metal}"
      fi
      ;;
  esac
  exit 0
fi

printf 'unexpected command: %s\\n' "$*" >&2
exit 2
`,
  );
  await chmod(fake, 0o755);
  return fake;
}

function runPreflight(env) {
  return new Promise((resolve, reject) => {
    const child = spawn("bash", [preflightScript], {
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

async function setupRun() {
  const dir = await mkdtemp(path.join(os.tmpdir(), "crabbox-macos-region-preflight-"));
  return { dir, fake: await makeFakeCrabbox(dir) };
}

test("macOS host region preflight selects an existing host without paid allocation", async () => {
  const run = await setupRun();
  const result = await runPreflight({
    CRABBOX_BIN: run.fake,
    CRABBOX_MACOS_REGIONS: "eu-west-1,us-east-1",
    CRABBOX_FAKE_EXISTING_REGION: "us-east-1",
  });

  assert.equal(result.code, 0, result.stderr);
  const summary = JSON.parse(result.stdout);
  assert.equal(summary.result, "ready-existing-host");
  assert.equal(summary.selectedRegion, "us-east-1");
  assert.equal(summary.selectedInstanceType, "mac2.metal");
  assert.equal(summary.existingHost, "h-existing");
});

test("macOS host region preflight parses stdout JSON when commands emit stderr diagnostics", async () => {
  const run = await setupRun();
  const result = await runPreflight({
    CRABBOX_BIN: run.fake,
    CRABBOX_MACOS_REGIONS: "us-east-1",
    CRABBOX_FAKE_EXISTING_REGION: "us-east-1",
    CRABBOX_FAKE_STDERR_DIAGNOSTICS: "1",
  });

  assert.equal(result.code, 0, result.stderr);
  const summary = JSON.parse(result.stdout);
  assert.equal(summary.result, "ready-existing-host");
  assert.equal(summary.selectedRegion, "us-east-1");
  assert.equal(summary.existingHost, "h-existing");
  assert.equal(summary.regions[0].list.ok, true);
  assert.match(summary.regions[0].list.stdout, /h-existing/);
  assert.match(summary.regions[0].list.stderr, /warning: diagnostic/);
  assert.equal(summary.regions[0].list.parseError, null);
});

test("macOS host region preflight falls back to mac1 when no mac2 host is ready", async () => {
  const run = await setupRun();
  const result = await runPreflight({
    CRABBOX_BIN: run.fake,
    CRABBOX_MACOS_REGIONS: "eu-west-1,us-east-1",
    CRABBOX_FAKE_EXISTING_REGION: "us-east-1",
    CRABBOX_FAKE_EXISTING_TYPE: "mac1.metal",
  });

  assert.equal(result.code, 0, result.stderr);
  const summary = JSON.parse(result.stdout);
  assert.equal(summary.result, "ready-existing-host");
  assert.deepEqual(summary.instanceTypes, ["mac2.metal", "mac1.metal"]);
  assert.equal(summary.instanceType, "mac1.metal");
  assert.equal(summary.selectedInstanceType, "mac1.metal");
  assert.equal(summary.selectedRegion, "us-east-1");
  assert.equal(summary.existingHost, "h-existing");
});

test("macOS host region preflight expands all known Mac host families on request", async () => {
  const run = await setupRun();
  const result = await runPreflight({
    CRABBOX_BIN: run.fake,
    CRABBOX_MACOS_REGIONS: "eu-west-1",
    CRABBOX_MACOS_TYPES: "all",
    CRABBOX_FAKE_EXISTING_REGION: "eu-west-1",
    CRABBOX_FAKE_EXISTING_TYPE: "mac-m4.metal",
  });

  assert.equal(result.code, 0, result.stderr);
  const summary = JSON.parse(result.stdout);
  assert.deepEqual(summary.instanceTypes, [
    "mac2.metal",
    "mac2-m2.metal",
    "mac2-m2pro.metal",
    "mac-m4.metal",
    "mac-m4pro.metal",
    "mac-m4max.metal",
    "mac2-m1ultra.metal",
    "mac-m3ultra.metal",
    "mac1.metal",
  ]);
  assert.equal(summary.result, "ready-existing-host");
  assert.equal(summary.selectedInstanceType, "mac-m4.metal");
  assert.equal(summary.existingHost, "h-existing");
});

test("macOS host region preflight selects the first quota-backed no-spend dry-run", async () => {
  const run = await setupRun();
  const result = await runPreflight({
    CRABBOX_BIN: run.fake,
    CRABBOX_MACOS_REGIONS: "eu-west-1,eu-central-1,us-west-2",
    CRABBOX_FAKE_DRY_REGION: "us-west-2",
  });

  assert.equal(result.code, 0, result.stderr);
  const summary = JSON.parse(result.stdout);
  assert.equal(summary.result, "ready-allocation");
  assert.equal(summary.selectedRegion, "us-west-2");
  assert.equal(summary.selectedInstanceType, "mac2.metal");
  assert.equal(summary.existingHost, null);
  assert.equal(summary.regions.length, 6);
  assert.equal(summary.regions[2].quota.ok, true);
});

test("macOS host region preflight blocks when dry-run succeeds but quota is unavailable", async () => {
  const run = await setupRun();
  const result = await runPreflight({
    CRABBOX_BIN: run.fake,
    CRABBOX_MACOS_REGIONS: "us-west-2",
    CRABBOX_FAKE_DRY_REGION: "us-west-2",
    CRABBOX_FAKE_QUOTA_ZERO_REGION: "us-west-2",
  });

  assert.equal(result.code, 1);
  const summary = JSON.parse(result.stdout);
  assert.equal(summary.result, "blocked");
  assert.equal(summary.selectedRegion, null);
  assert.equal(summary.selectedInstanceType, null);
  assert.equal(summary.regions[0].dryRun.ok, true);
  assert.equal(summary.regions[0].quota.ok, false);
  assert.deepEqual(summary.blocker.commands, [
    "crabbox admin providers identity --provider aws --region us-west-2",
    "crabbox admin providers identity --provider aws --region us-west-2 --json > provider-identity.json",
    "crabbox admin providers policy --provider aws --target macos > macos-image-policy.json",
    "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto",
    "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply",
    "crabbox admin hosts quota --provider aws --target macos --region us-west-2 --type mac2.metal --json > mac-host-quota.json",
    "scripts/request-macos-host-quota.sh --identity provider-identity.json --quota mac-host-quota.json --region us-west-2 --profile auto",
    "scripts/request-macos-host-quota.sh --identity provider-identity.json --quota mac-host-quota.json --region us-west-2 --profile auto --apply",
    "scripts/macos-host-region-preflight.sh",
  ]);
  assert.match(summary.regions[0].quota.output, /Running Dedicated mac2 Hosts/);
});

test("macOS host region preflight points quota remediation at the dry-run-ready candidate", async () => {
  const run = await setupRun();
  const result = await runPreflight({
    CRABBOX_BIN: run.fake,
    CRABBOX_MACOS_REGIONS: "eu-central-1,us-west-2",
    CRABBOX_FAKE_DRY_REGION: "us-west-2",
    CRABBOX_FAKE_QUOTA_ZERO_REGION: "us-west-2",
  });

  assert.equal(result.code, 1);
  const summary = JSON.parse(result.stdout);
  assert.equal(summary.result, "blocked");
  assert.equal(summary.regions[0].dryRun.status, 1);
  assert.equal(summary.regions[1].dryRun.ok, true);
  assert.equal(summary.regions[1].quota.ok, false);
  assert.deepEqual(summary.blocker.commands, [
    "crabbox admin providers identity --provider aws --region us-west-2",
    "crabbox admin providers identity --provider aws --region us-west-2 --json > provider-identity.json",
    "crabbox admin providers policy --provider aws --target macos > macos-image-policy.json",
    "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto",
    "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply",
    "crabbox admin hosts quota --provider aws --target macos --region us-west-2 --type mac2.metal --json > mac-host-quota.json",
    "scripts/request-macos-host-quota.sh --identity provider-identity.json --quota mac-host-quota.json --region us-west-2 --profile auto",
    "scripts/request-macos-host-quota.sh --identity provider-identity.json --quota mac-host-quota.json --region us-west-2 --profile auto --apply",
    "scripts/macos-host-region-preflight.sh",
  ]);
});

test("macOS host region preflight blocks when every region is unavailable", async () => {
  const run = await setupRun();
  const result = await runPreflight({
    CRABBOX_BIN: run.fake,
    CRABBOX_MACOS_REGIONS: "eu-west-1,eu-central-1",
  });

  assert.equal(result.code, 1);
  const summary = JSON.parse(result.stdout);
  assert.equal(summary.result, "blocked");
  assert.equal(summary.selectedRegion, null);
  assert.match(summary.blocker.message, /quota-backed/);
  assert.match(summary.regions[0].dryRun.output, /UnauthorizedOperation/);
  assert.match(summary.regions[1].dryRun.output, /no_mac_host_offerings/);
  assert.match(summary.regions[0].quota.output, /Running Dedicated mac2 Hosts/);
  assert.deepEqual(summary.blocker.commands, [
    "crabbox admin providers identity --provider aws --region eu-west-1",
    "crabbox admin providers identity --provider aws --region eu-west-1 --json > provider-identity.json",
    "crabbox admin providers policy --provider aws --target macos > macos-image-policy.json",
    "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto",
    "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply",
    "crabbox admin hosts quota --provider aws --target macos --region eu-west-1 --type mac2.metal --json > mac-host-quota.json",
    "scripts/request-macos-host-quota.sh --identity provider-identity.json --quota mac-host-quota.json --region eu-west-1 --profile auto",
    "scripts/request-macos-host-quota.sh --identity provider-identity.json --quota mac-host-quota.json --region eu-west-1 --profile auto --apply",
    "scripts/macos-host-region-preflight.sh",
  ]);
});
