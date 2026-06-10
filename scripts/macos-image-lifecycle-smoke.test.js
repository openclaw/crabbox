import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmod, mkdtemp, readFile, readdir, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..");
const lifecycleScript = path.join(scriptDir, "macos-image-lifecycle-smoke.sh");

async function makeFakeCrabbox(dir) {
  const fake = path.join(dir, "fake-crabbox");
  await writeFile(
    fake,
    `#!/usr/bin/env bash
set -euo pipefail

log="\${CRABBOX_FAKE_LOG:?}"
state_dir="\${CRABBOX_FAKE_STATE:?}"
mkdir -p "$state_dir"
printf '%s\\n' "$*" >>"$log"
region="eu-west-1"
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

if [[ "$1" == "admin" && "$2" == "providers" && "$3" == "policy" ]]; then
  if [[ " $* " == *" --target macos "* || " $* " == *" --host-lifecycle "* ]]; then
    printf '{"Statement":[{"Action":["ec2:RunInstances","ec2:AllocateHosts"]}]}\\n'
  else
    printf '{"Statement":[{"Action":"ec2:RunInstances"}]}\\n'
  fi
  exit 0
fi

if [[ "$1" == "admin" && "$2" == "providers" && "$3" == "identity" ]]; then
  printf '{"account":"123456789012","arn":"arn:aws:iam::123456789012:user/crabbox-runner","userId":"AIDAEXAMPLE","region":"%s","policyTarget":{"type":"user","name":"crabbox-runner","source":"iam-user"}}\\n' "$region"
  exit 0
fi

if [[ "$1" == "admin" && "$2" == "aws-policy" ]]; then
  if [[ " $* " == *" --mac-hosts "* ]]; then
    printf '{"Statement":[{"Action":["ec2:RunInstances","ec2:AllocateHosts"]}]}\\n'
  else
    printf '{"Statement":[{"Action":"ec2:RunInstances"}]}\\n'
  fi
  exit 0
fi

if [[ "$1" == "admin" && ( "$2" == "mac-hosts" || "$2" == "hosts" ) ]]; then
  case "$3" in
    policy)
      printf '{"Statement":[{"Action":"ec2:AllocateHosts"}]}\\n'
      ;;
    offerings)
      if [[ "\${CRABBOX_FAKE_OFFERINGS_404:-0}" == "1" ]]; then
        printf 'coordinator GET /v1/admin/mac-hosts/offerings?region=%s&type=%s: http 404: {"error":"not_found"}\\n' "$region" "$type" >&2
        exit 1
      fi
      printf '%s    %sa     %s\\n' "$region" "$region" "$type"
      ;;
    quota)
      if [[ "\${CRABBOX_FAKE_QUOTA_FAIL:-0}" == "1" ]]; then
        printf 'coordinator GET /v1/admin/mac-hosts/quota?region=%s&type=%s: http 502: {"error":"mac_host_quota_failed","message":"AWS authorization failure: coordinator AWS identity needs servicequotas:ListServiceQuotas to inspect EC2 Mac Dedicated Host quotas"}\\n' "$region" "$type" >&2
        exit 1
      fi
      printf '[{"serviceCode":"ec2","quotaCode":"L-MAC","quotaName":"Running Dedicated %s Hosts","value":%s,"adjustable":true,"globalQuota":false,"unit":"None"}]\\n' "\${type%.metal}" "\${CRABBOX_FAKE_QUOTA_VALUE:-1}"
      ;;
    list)
      if [[ "\${CRABBOX_FAKE_EXISTING_REGION:-}" == "$region" && "\${CRABBOX_FAKE_EXISTING_TYPE:-mac2.metal}" == "$type" ]]; then
        printf '[{"id":"h-existing","instanceType":"%s","state":"available","region":"%s"}]\\n' "$type" "$region"
      elif [[ "\${CRABBOX_FAKE_NO_HOST:-0}" == "1" && ! -f "$state_dir/host" ]]; then
        printf '[]\\n'
      else
        printf '[{"id":"h-mock","instanceType":"%s","state":"available"}]\\n' "$type"
      fi
      ;;
    allocate)
      if [[ " $* " == *" --dry-run "* ]]; then
        if [[ -n "\${CRABBOX_FAKE_DRY_REGION:-}" && ("\${CRABBOX_FAKE_DRY_REGION:-}" != "$region" || "\${CRABBOX_FAKE_DRY_TYPE:-mac2.metal}" != "$type") ]]; then
          printf '[{"region":"%s","availabilityZone":"%sa","instanceType":"%s","ok":false,"message":"UnauthorizedOperation: coordinator AWS identity needs EC2 Mac host lifecycle permissions, including ec2:AllocateHosts and ec2:CreateTags"}]\\n' "$region" "$region" "$type"
        elif [[ "\${CRABBOX_FAKE_DRY_RUN:-allow}" == "deny" ]]; then
          printf '[{"region":"%s","availabilityZone":"%sa","instanceType":"%s","ok":false,"message":"UnauthorizedOperation: coordinator AWS identity needs EC2 Mac host lifecycle permissions, including ec2:AllocateHosts and ec2:CreateTags"}]\\n' "$region" "$region" "$type"
        else
          printf '[{"region":"%s","availabilityZone":"%sa","instanceType":"%s","ok":true,"message":"DryRunOperation"}]\\n' "$region" "$region" "$type"
        fi
      else
        : >"$state_dir/host"
        if [[ "\${CRABBOX_FAKE_ALLOCATE_FAIL:-0}" == "1" ]]; then
          printf 'coordinator POST /v1/admin/hosts?provider=aws&region=%s&target=macos: http 502: {"error":"mac_host_allocation_failed","message":"aws AllocateHosts: http 500: service unavailable"}\\n' "$region" >&2
          exit 1
        fi
        printf '[{"id":"h-mock","instanceType":"%s","state":"available"}]\\n' "$type"
      fi
      ;;
    release)
      printf 'released %s\\n' "$4"
      ;;
  esac
  exit 0
fi

case "$1" in
  warmup)
    printf 'env CRABBOX_AWS_REGION=%s\\n' "\${CRABBOX_AWS_REGION:-}" >>"$log"
    count=0
    if [[ -f "$state_dir/warmup-count" ]]; then
      count="$(cat "$state_dir/warmup-count")"
    fi
    count="$((count + 1))"
    printf '%s\\n' "$count" >"$state_dir/warmup-count"
    if [[ "\${CRABBOX_FAKE_WARMUP_FAIL_AT:-}" == "$count" ]]; then
      printf 'warmup failed at %s\\n' "$count" >&2
      exit 42
    fi
    if [[ "\${CRABBOX_FAKE_WARMUP_SPLIT_LOG:-0}" == "1" ]]; then
      for n in $(seq 1 25); do
        printf 'warmup-stdout-record-%02d\\n' "$n"
        printf 'warmup-stderr-record-%02d\\n' "$n" >&2
      done
    fi
    case "$count" in
      1) printf '{"leaseId":"cbx_source"}\\n' ;;
      2) printf '{"leaseId":"cbx_candidate"}\\n' ;;
      *) printf '{"leaseId":"cbx_promoted"}\\n' ;;
    esac
    ;;
  run)
    if [[ "\${CRABBOX_FAKE_RUN_SPLIT_LOG:-0}" == "1" && " $* " == *" --script "* ]]; then
      for n in $(seq 1 50); do
        printf 'stdout-record-%02d\\n' "$n"
        printf 'stderr-record-%02d\\n' "$n" >&2
      done
    fi
    printf 'macos-smoke-ok\\n'
    ;;
  webvnc)
    if [[ "$2" == "status" ]]; then
      printf 'portal bridge: connected=true slots=1\\n'
    elif [[ "$2" == "daemon" && "$3" == "start" ]]; then
      printf 'webvnc daemon: ready\\n'
    elif [[ "$2" == "daemon" && "$3" == "stop" ]]; then
      printf 'webvnc daemon: stopped\\n'
    fi
    ;;
  artifacts)
    out=""
    while [[ "$#" -gt 0 ]]; do
      if [[ "$1" == "--output" ]]; then
        out="$2"
        shift 2
      else
        shift
      fi
    done
    mkdir -p "$out"
    printf '{"ok":true,"output":"%s"}\\n' "$out"
    ;;
  image)
    if [[ "$2" == "create" ]]; then
      printf '{"id":"ami-mock"}\\n'
    elif [[ "$2" == "promote" ]]; then
      printf '{"id":"ami-mock","target":"macos","region":"eu-west-1"}\\n'
    fi
    ;;
  checkpoint)
    if [[ "$2" == "create" ]]; then
      printf 'checkpoint created id=chk_macos kind=aws-ami resource=ami-checkpoint state=available region=%s workdir=/Users/ec2-user/crabbox/crabbox\\n' "$region"
    elif [[ "$2" == "fork" ]]; then
      if [[ "\${CRABBOX_FAKE_CHECKPOINT_FORK_FAIL_ONCE:-0}" == "1" && ! -f "$state_dir/checkpoint-fork-failed" ]]; then
        : >"$state_dir/checkpoint-fork-failed"
        printf 'coordinator POST /v1/leases: http 500: transient host recycle\\n' >&2
        exit 42
      fi
      printf 'checkpoint forked id=%s lease=cbx_checkpoint slug=checkpoint image=ami-checkpoint workdir=/Users/ec2-user/crabbox/crabbox\\n' "$3"
    elif [[ "$2" == "delete" ]]; then
      printf 'checkpoint deleted id=%s kind=aws-ami\\n' "$3"
    fi
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
esac
`,
  );
  await chmod(fake, 0o755);
  return fake;
}

function runLifecycle(env) {
  return new Promise((resolve, reject) => {
    const child = spawn("bash", [lifecycleScript], {
      cwd: repoRoot,
      env: { ...process.env, CRABBOX_MACOS_HOST_WAIT_INTERVAL: "0s", ...env },
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
  const dir = await mkdtemp(path.join(os.tmpdir(), "crabbox-macos-smoke-test-"));
  const fake = await makeFakeCrabbox(dir);
  return {
    dir,
    fake,
    artifacts: path.join(dir, "artifacts"),
    fakeLog: path.join(dir, "fake.log"),
    fakeState: path.join(dir, "state"),
  };
}

async function makeBlockedRegionPreflight(dir) {
  const script = path.join(dir, "blocked-region-preflight.sh");
  await writeFile(
    script,
    `#!/usr/bin/env bash
set -euo pipefail
cat <<'JSON'
{
  "result": "blocked",
  "instanceType": "mac2.metal",
  "selectedInstanceType": null,
  "selectedRegion": null,
  "blocker": {
    "message": "no configured region/type has an available EC2 Mac Dedicated Host or quota-backed no-spend allocation dry-run",
    "remediation": "Apply crabbox admin providers policy --provider aws --target macos to the coordinator AWS identity, verify regional EC2 Mac Dedicated Host quota, then rerun this preflight before paid allocation.",
    "commands": [
      "crabbox admin providers identity --provider aws --region eu-west-1",
      "crabbox admin providers identity --provider aws --region eu-west-1 --json > provider-identity.json",
      "crabbox admin providers policy --provider aws --target macos > macos-image-policy.json",
      "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto",
      "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply",
      "scripts/macos-host-region-preflight.sh"
    ]
  },
  "regions": []
}
JSON
exit 1
`,
  );
  await chmod(script, 0o755);
  return script;
}

async function readJSON(file) {
  return JSON.parse(await readFile(file, "utf8"));
}

async function assertFileContains(file, expected) {
  const text = await readFile(file, "utf8");
  assert.match(text, expected);
}

function summaryPath(artifactRoot, value) {
  if (!value) return value;
  return path.isAbsolute(value) ? value : path.join(artifactRoot, value);
}

async function assertSummaryFileContains(artifactRoot, value, expected) {
  await assertFileContains(summaryPath(artifactRoot, value), expected);
}

function assertSummaryOmitsArtifactRoot(summary, artifactRoot) {
  assert.equal(JSON.stringify(summary).includes(artifactRoot), false);
}

test("macOS lifecycle smoke reports a missing coordinator mac-host endpoint before paid work", async () => {
  const run = await setupRun();
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_OFFERINGS_404: "1",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "missing-endpoint",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 1, result.stdout + result.stderr);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assertSummaryOmitsArtifactRoot(summary, run.artifacts);
  assert.equal(summary.result, "blocked");
  assert.equal(summary.phase, "host-offerings");
  assert.match(summary.blocker.message, /does not expose provider-neutral host lifecycle admin endpoints/);
  assert.match(summary.blocker.remediation, /\/v1\/admin\/hosts/);
  assert.match(summary.blocker.remediation, /Deploy a coordinator/);
});

test("macOS lifecycle smoke preserves region preflight remediation commands", async () => {
  const run = await setupRun();
  const regionPreflight = await makeBlockedRegionPreflight(run.dir);
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "region-preflight-blocked",
    CRABBOX_MACOS_REGIONS: "eu-west-1,us-east-1",
    CRABBOX_MACOS_REGION_PREFLIGHT_SCRIPT: regionPreflight,
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 1, result.stdout + result.stderr);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assertSummaryOmitsArtifactRoot(summary, run.artifacts);
  assert.equal(summary.result, "blocked");
  assert.equal(summary.phase, "region-preflight");
  assert.match(summary.blocker.message, /quota-backed/);
  assert.deepEqual(summary.blocker.commands, [
    "crabbox admin providers identity --provider aws --region eu-west-1",
    "crabbox admin providers identity --provider aws --region eu-west-1 --json > provider-identity.json",
    "crabbox admin providers policy --provider aws --target macos > macos-image-policy.json",
    "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto",
    "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply",
    "scripts/macos-host-region-preflight.sh",
  ]);
  assert.equal(
    summary.blocker.commands.some((command) => command.includes("coordinator_account")),
    false,
  );
  assert.equal(summary.evidence.regionPreflight, "evidence/mac-host-region-preflight.json");
  await assertSummaryFileContains(run.artifacts, summary.evidence.regionPreflight, /scripts\/apply-macos-image-iam-policy\.sh/);
});

test("macOS lifecycle smoke writes a blocked IAM summary before paid work", async () => {
  const run = await setupRun();
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_NO_HOST: "1",
    CRABBOX_FAKE_DRY_RUN: "deny",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "blocked",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 1, result.stdout + result.stderr);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assert.equal(summary.result, "blocked");
  assert.equal(summary.phase, "host-dry-run");
  assert.match(summary.blocker.reason, /ec2:AllocateHosts/);
  assert.match(summary.blocker.message, /ec2:AllocateHosts/);
  assert.match(summary.blocker.message, /ec2:CreateTags/);
  assert.match(summary.blocker.remediation, /Apply the EC2 Mac host lifecycle policy/);
  assert.deepEqual(summary.blocker.commands, [
    "crabbox admin providers identity --provider aws --region eu-west-1",
    "crabbox admin providers identity --provider aws --region eu-west-1 --json > provider-identity.json",
    "crabbox admin providers policy --provider aws --target macos > macos-image-policy.json",
    "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto",
    "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply",
    "crabbox admin hosts allocate --provider aws --target macos --region eu-west-1 --type mac2.metal --dry-run --json",
  ]);
  assert.equal(summary.artifactRoot, ".");
  assert.equal(summary.evidence.providerIdentity, "evidence/provider-identity.json");
  await assertSummaryFileContains(run.artifacts, summary.evidence.providerIdentity, /crabbox-runner/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.awsProviderPolicy, /ec2:RunInstances/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.macHostPolicy, /ec2:AllocateHosts/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.macosImagePolicy, /ec2:AllocateHosts/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.hostOfferings, /mac2\.metal/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.hostList, /^\[\]\n?$/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.hostDryRun, /UnauthorizedOperation/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.hostQuota, /Running Dedicated mac2 Hosts/);
  assert.equal(summary.evidence.hostAllocate, null);
  assert.equal(summary.evidence.webvncDaemon.source, null);
  assert.equal(summary.evidence.webvncStatus.source, null);
  assert.equal(summary.artifacts.source, null);
  assert.equal(summary.artifacts.candidate, null);
  assert.equal(summary.artifacts.promoted, null);

  const evidenceFiles = await readdir(path.join(run.artifacts, "evidence"));
  assert.deepEqual(
    evidenceFiles.filter((name) => name.startsWith("webvnc-daemon-")),
    [],
  );
});

test("macOS lifecycle smoke preserves quota IAM evidence when dry-run is also blocked", async () => {
  const run = await setupRun();
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_NO_HOST: "1",
    CRABBOX_FAKE_DRY_RUN: "deny",
    CRABBOX_FAKE_QUOTA_FAIL: "1",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "quota-and-dry-run-blocked",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 1, result.stdout + result.stderr);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assert.equal(summary.result, "blocked");
  assert.equal(summary.phase, "host-dry-run");
  assert.match(summary.blocker.reason, /quota preflight also failed/);
  assert.match(summary.blocker.message, /ec2:AllocateHosts/);
  assert.match(summary.blocker.message, /quota preflight also failed/);
  assert.match(summary.blocker.remediation, /servicequotas:ListServiceQuotas/);
  assert.deepEqual(summary.blocker.commands, [
    "crabbox admin providers identity --provider aws --region eu-west-1",
    "crabbox admin providers identity --provider aws --region eu-west-1 --json > provider-identity.json",
    "crabbox admin providers policy --provider aws --target macos > macos-image-policy.json",
    "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto",
    "scripts/apply-macos-image-iam-policy.sh --identity provider-identity.json --policy macos-image-policy.json --profile auto --apply",
    "crabbox admin hosts quota --provider aws --target macos --region eu-west-1 --type mac2.metal --json",
    "crabbox admin hosts allocate --provider aws --target macos --region eu-west-1 --type mac2.metal --dry-run --json",
  ]);
  await assertSummaryFileContains(run.artifacts, summary.evidence.hostQuota, /servicequotas:ListServiceQuotas/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.hostDryRun, /UnauthorizedOperation/);
});

test("macOS lifecycle smoke blocks on missing Mac host quota before paid work", async () => {
  const run = await setupRun();
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_NO_HOST: "1",
    CRABBOX_FAKE_QUOTA_VALUE: "0",
    CRABBOX_MACOS_ALLOCATE: "1",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "quota-blocked",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 1, result.stdout + result.stderr);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assert.equal(summary.result, "blocked");
  assert.equal(summary.phase, "host-quota");
  assert.match(summary.blocker.message, /quota is below 1/);
  assert.deepEqual(summary.blocker.commands, [
    "crabbox admin providers identity --provider aws --region eu-west-1 --json > provider-identity.json",
    "crabbox admin hosts quota --provider aws --target macos --region eu-west-1 --type mac2.metal --json > mac-host-quota.json",
    "scripts/request-macos-host-quota.sh --identity provider-identity.json --quota mac-host-quota.json --region eu-west-1 --profile auto",
    "scripts/request-macos-host-quota.sh --identity provider-identity.json --quota mac-host-quota.json --region eu-west-1 --profile auto --apply",
    "scripts/macos-host-region-preflight.sh",
  ]);
  await assertSummaryFileContains(run.artifacts, summary.evidence.hostQuota, /Running Dedicated mac2 Hosts/);
  assert.equal(summary.evidence.hostAllocate, null);

  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.match(fakeLog, /^admin hosts quota --provider aws --target macos --region eu-west-1 --type mac2\.metal --json$/m);
  assert.doesNotMatch(fakeLog, /^admin hosts allocate --provider aws --target macos --region eu-west-1 --type mac2\.metal --force --json$/m);
});

test("macOS lifecycle smoke records paid host allocation failures", async () => {
  const run = await setupRun();
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_NO_HOST: "1",
    CRABBOX_FAKE_ALLOCATE_FAIL: "1",
    CRABBOX_MACOS_ALLOCATE: "1",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "allocation-blocked",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 1, result.stdout + result.stderr);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assert.equal(summary.result, "blocked");
  assert.equal(summary.phase, "host-allocation");
  assert.match(summary.blocker.message, /mac host allocation failed/);
  assert.match(summary.blocker.message, /AllocateHosts/);
  assert.match(summary.blocker.remediation, /Retry the allocation/);
  assert.equal(summary.evidence.hostAllocate, "evidence/mac-host-allocate.json");
  await assertSummaryFileContains(run.artifacts, summary.evidence.hostAllocate, /mac_host_allocation_failed/);

  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.match(fakeLog, /^admin hosts allocate --provider aws --target macos --region eu-west-1 --type mac2\.metal --force --json$/m);
  assert.doesNotMatch(fakeLog, /^warmup /m);
});

test("macOS lifecycle smoke selects a dry-run-ready configured region before paid work", async () => {
  const run = await setupRun();
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_NO_HOST: "1",
    CRABBOX_FAKE_DRY_REGION: "us-west-2",
    CRABBOX_MACOS_REGIONS: "eu-west-1,us-west-2",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "region-selected",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 0, result.stdout + result.stderr);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assert.equal(summary.result, "ready");
  assert.equal(summary.phase, "allocation");
  assert.equal(summary.region, "us-west-2");
  await assertSummaryFileContains(run.artifacts, summary.evidence.regionPreflight, /"selectedRegion": "us-west-2"/);

  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.match(fakeLog, /^admin hosts allocate --provider aws --target macos --region eu-west-1 --type mac2\.metal --dry-run --json$/m);
  assert.match(fakeLog, /^admin hosts allocate --provider aws --target macos --region us-west-2 --type mac2\.metal --dry-run --json$/m);
  assert.match(fakeLog, /^admin hosts quota --provider aws --target macos --region us-west-2 --type mac2\.metal --json$/m);
});

test("macOS lifecycle smoke adopts the type selected by region preflight", async () => {
  const run = await setupRun();
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_NO_HOST: "1",
    CRABBOX_FAKE_DRY_REGION: "us-west-2",
    CRABBOX_FAKE_DRY_TYPE: "mac1.metal",
    CRABBOX_MACOS_REGIONS: "eu-west-1,us-west-2",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "type-selected",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 0, result.stdout + result.stderr);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assert.equal(summary.result, "ready");
  assert.equal(summary.phase, "allocation");
  assert.equal(summary.region, "us-west-2");
  assert.equal(summary.instanceType, "mac1.metal");
  await assertSummaryFileContains(run.artifacts, summary.evidence.regionPreflight, /"selectedInstanceType": "mac1.metal"/);

  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.match(fakeLog, /^admin hosts allocate --provider aws --target macos --region us-west-2 --type mac1\.metal --dry-run --json$/m);
  assert.match(fakeLog, /^admin hosts quota --provider aws --target macos --region us-west-2 --type mac1\.metal --json$/m);
});

test("macOS lifecycle smoke preserves full mock lifecycle evidence", async () => {
  const run = await setupRun();
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_NO_HOST: "1",
    CRABBOX_MACOS_ALLOCATE: "1",
    CRABBOX_MACOS_PROMOTE: "1",
    CRABBOX_FAKE_WARMUP_SPLIT_LOG: "1",
    CRABBOX_MACOS_RELEASE_HOST: "1",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "full",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 0, result.stdout + result.stderr);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assertSummaryOmitsArtifactRoot(summary, run.artifacts);
  assert.equal(summary.result, "passed");
  assert.equal(summary.phase, "promoted");
  assert.equal(summary.host.id, "h-mock");
  assert.equal(summary.host.allocatedByScript, true);
  assert.equal(summary.host.released, true);
  assert.equal(summary.image.amiId, "ami-mock");
  assert.equal(summary.checkpoint.id, "chk_macos");
  assert.equal(summary.checkpoint.deleted, true);
  assert.equal(summary.leases.checkpointFork, "cbx_checkpoint");
  assert.equal(summary.artifactRoot, ".");
  assert.equal(summary.evidence.providerIdentity, "evidence/provider-identity.json");

  for (const label of ["source", "candidate", "promoted"]) {
    assert.equal(summary.artifacts[label], label);
    await readdir(summaryPath(run.artifacts, summary.artifacts[label]));
    await assertSummaryFileContains(run.artifacts, summary.evidence.hostWait[label], /host h-mock is available/);
    await assertSummaryFileContains(run.artifacts, summary.evidence.warmup[label], /"leaseId":"cbx_/);
    await assertSummaryFileContains(run.artifacts, summary.evidence.warmup[label], /warmup-stdout-record-25/);
    await assertSummaryFileContains(run.artifacts, summary.evidence.warmup[label], /warmup-stderr-record-25/);
    await assertSummaryFileContains(run.artifacts, summary.evidence.webvncDaemon[label], /webvnc daemon: ready/);
    await assertSummaryFileContains(run.artifacts, summary.evidence.webvncStatus[label], /portal bridge: connected=true/);
  }
  assert.equal(summary.artifacts.checkpointFork, "checkpoint");
  await readdir(summaryPath(run.artifacts, summary.artifacts.checkpointFork));
  await assertSummaryFileContains(run.artifacts, summary.evidence.hostWait.checkpointFork, /host h-mock is available/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.webvncDaemon.checkpointFork, /webvnc daemon: ready/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.webvncStatus.checkpointFork, /portal bridge: connected=true/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.imageCreate, /ami-mock/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.imagePromote, /"target":"macos"/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.checkpointCreate, /chk_macos/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.checkpointFork, /cbx_checkpoint/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.checkpointDelete, /checkpoint deleted/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.providerIdentity, /crabbox-runner/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.awsProviderPolicy, /ec2:RunInstances/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.macHostPolicy, /ec2:AllocateHosts/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.macosImagePolicy, /ec2:AllocateHosts/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.hostQuota, /Running Dedicated mac2 Hosts/);

  const evidenceFiles = await readdir(path.join(run.artifacts, "evidence"));
  assert.deepEqual(
    evidenceFiles.filter((name) => name.startsWith("webvnc-status-")).sort(),
    [
      "webvnc-status-candidate.log",
      "webvnc-status-checkpoint.log",
      "webvnc-status-promoted.log",
      "webvnc-status-source.log",
    ],
  );
  assert.deepEqual(
    evidenceFiles.filter((name) => name.startsWith("webvnc-daemon-")).sort(),
    [
      "webvnc-daemon-candidate.log",
      "webvnc-daemon-checkpoint.log",
      "webvnc-daemon-promoted.log",
      "webvnc-daemon-source.log",
    ],
  );

  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.equal((fakeLog.match(/^warmup\b/gm) ?? []).length, 3);
  assert.equal((fakeLog.match(/^webvnc daemon start\b/gm) ?? []).length, 4);
  assert.equal((fakeLog.match(/^webvnc status\b/gm) ?? []).length, 4);
  assert.match(fakeLog, /required_macos_major=14/);
  assert.match(fakeLog, /required_swift_tools=6\.0/);
  assert.match(fakeLog, /require_xcode=0/);
  assert.match(fakeLog, /xcode-select -p/);
  assert.match(fakeLog, /xcrun --sdk macosx --show-sdk-path/);
  assert.match(fakeLog, /Swift tools %s\+ required/);
  assert.match(fakeLog, /command -v brew/);
  assert.match(fakeLog, /command -v node/);
  assert.match(fakeLog, /command -v corepack/);
  assert.match(fakeLog, /command -v pnpm/);
  assert.match(fakeLog, /^checkpoint create --id cbx_source --name full-checkpoint --mode native --strategy image --wait --wait-timeout 60m$/m);
  assert.match(fakeLog, /^checkpoint fork chk_macos --desktop$/m);
  assert.match(fakeLog, /^checkpoint delete chk_macos$/m);
  assert.match(fakeLog, /^admin hosts quota --provider aws --target macos --region eu-west-1 --type mac2\.metal --json$/m);
  assert.match(fakeLog, /^admin hosts release h-mock --provider aws --target macos --region eu-west-1 --force$/m);
});

test("macOS lifecycle smoke uses stricter defaults for mac-m host families", async () => {
  const run = await setupRun();
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_NO_HOST: "1",
    CRABBOX_MACOS_ALLOCATE: "1",
    CRABBOX_MACOS_CREATE_IMAGE: "0",
    CRABBOX_MACOS_TYPE: "mac-m4.metal",
    CRABBOX_MACOS_REQUIRE_XCODE: "1",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "m4-toolchain",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 0, result.stdout + result.stderr);
  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.match(fakeLog, /required_macos_major=15/);
  assert.match(fakeLog, /required_swift_tools=6\.2/);
  assert.match(fakeLog, /require_xcode=1/);
  assert.match(fakeLog, /full Xcode developer directory required/);
  assert.match(fakeLog, /xcodebuild -version/);
});

test("macOS lifecycle smoke runs source prep before the source smoke", async () => {
  const run = await setupRun();
  const prep = path.join(run.dir, "prep.sh");
  await writeFile(
    prep,
    `#!/usr/bin/env bash
set -euo pipefail
echo prep-ok
`,
  );
  await chmod(prep, 0o755);

  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_NO_HOST: "1",
    CRABBOX_MACOS_ALLOCATE: "1",
    CRABBOX_MACOS_CREATE_IMAGE: "0",
    CRABBOX_FAKE_RUN_SPLIT_LOG: "1",
    CRABBOX_MACOS_SOURCE_PREP_SCRIPT: prep,
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "source-prep",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 0, result.stdout + result.stderr);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assert.equal(summary.result, "passed");
  assert.equal(summary.phase, "source");
  assert.equal(summary.evidence.sourcePrep, "evidence/source-prep.log");
  await assertSummaryFileContains(run.artifacts, summary.evidence.sourcePrep, /macos-smoke-ok/);
  const sourcePrepLog = await readFile(summaryPath(run.artifacts, summary.evidence.sourcePrep), "utf8");
  for (let n = 1; n <= 50; n += 1) {
    const label = String(n).padStart(2, "0");
    assert.equal((sourcePrepLog.match(new RegExp(`^stdout-record-${label}$`, "gm")) ?? []).length, 1);
    assert.equal((sourcePrepLog.match(new RegExp(`^stderr-record-${label}$`, "gm")) ?? []).length, 1);
  }

  const fakeLog = await readFile(run.fakeLog, "utf8");
  const prepIndex = fakeLog.indexOf(`run --provider aws --target macos --id cbx_source --no-sync --script ${prep}`);
  const smokeIndex = fakeLog.indexOf("run --provider aws --target macos --id cbx_source --no-sync --shell --");
  assert.notEqual(prepIndex, -1);
  assert.notEqual(smokeIndex, -1);
  assert.equal(prepIndex < smokeIndex, true);
});

test("macOS lifecycle smoke retries checkpoint fork after transient host recycle", async () => {
  const run = await setupRun();
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_NO_HOST: "1",
    CRABBOX_FAKE_CHECKPOINT_FORK_FAIL_ONCE: "1",
    CRABBOX_MACOS_ALLOCATE: "1",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "checkpoint-fork-retry",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 0, result.stdout + result.stderr);
  assert.match(result.stdout + result.stderr, /retrying checkpoint fork attempt 2\/2/);

  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assert.equal(summary.result, "passed");
  assert.equal(summary.phase, "candidate");
  assert.equal(summary.leases.checkpointFork, "cbx_checkpoint");
  await assertSummaryFileContains(run.artifacts, summary.evidence.hostWait.checkpointFork, /stable_count=2/);
  await assertSummaryFileContains(run.artifacts, summary.evidence.checkpointFork, /cbx_checkpoint/);

  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.equal((fakeLog.match(/^checkpoint fork chk_macos --desktop$/gm) ?? []).length, 2);
});

test("macOS lifecycle smoke forwards the selected region into warmup", async () => {
  const run = await setupRun();
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_NO_HOST: "1",
    CRABBOX_MACOS_ALLOCATE: "1",
    CRABBOX_MACOS_CREATE_IMAGE: "0",
    CRABBOX_MACOS_REGION: "us-west-2",
    CRABBOX_MACOS_RELEASE_HOST: "1",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "region-forward",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 0, result.stdout + result.stderr);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assert.equal(summary.result, "passed");
  assert.equal(summary.phase, "source");
  assert.equal(summary.region, "us-west-2");

  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.match(fakeLog, /^env CRABBOX_AWS_REGION=us-west-2$/m);
  assert.match(fakeLog, /^admin hosts release h-mock --provider aws --target macos --region us-west-2 --force$/m);
});

test("macOS lifecycle smoke releases script-allocated hosts after failures", async () => {
  const run = await setupRun();
  const result = await runLifecycle({
    CRABBOX_BIN: run.fake,
    CRABBOX_FAKE_LOG: run.fakeLog,
    CRABBOX_FAKE_STATE: run.fakeState,
    CRABBOX_FAKE_NO_HOST: "1",
    CRABBOX_FAKE_WARMUP_FAIL_AT: "1",
    CRABBOX_MACOS_ALLOCATE: "1",
    CRABBOX_MACOS_RELEASE_HOST: "1",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "failed-cleanup",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.notEqual(result.code, 0);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assert.equal(summary.result, "failed");
  assert.equal(summary.host.allocatedByScript, true);
  assert.equal(summary.host.released, true);
  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.match(fakeLog, /^admin hosts allocate --provider aws --target macos --region eu-west-1 --type mac2\.metal --force --json$/m);
  assert.match(fakeLog, /^admin hosts release h-mock --provider aws --target macos --region eu-west-1 --force$/m);
});
