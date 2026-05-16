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

if [[ "$1" == "admin" && "$2" == "aws-policy" ]]; then
  if [[ " $* " == *" --mac-hosts "* ]]; then
    printf '{"Statement":[{"Action":["ec2:RunInstances","ec2:AllocateHosts"]}]}\\n'
  else
    printf '{"Statement":[{"Action":"ec2:RunInstances"}]}\\n'
  fi
  exit 0
fi

if [[ "$1" == "admin" && "$2" == "mac-hosts" ]]; then
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
    count=0
    if [[ -f "$state_dir/warmup-count" ]]; then
      count="$(cat "$state_dir/warmup-count")"
    fi
    count="$((count + 1))"
    printf '%s\\n' "$count" >"$state_dir/warmup-count"
    case "$count" in
      1) printf '{"leaseId":"cbx_source"}\\n' ;;
      2) printf '{"leaseId":"cbx_candidate"}\\n' ;;
      *) printf '{"leaseId":"cbx_promoted"}\\n' ;;
    esac
    ;;
  run)
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

async function readJSON(file) {
  return JSON.parse(await readFile(file, "utf8"));
}

async function assertFileContains(file, expected) {
  const text = await readFile(file, "utf8");
  assert.match(text, expected);
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
  assert.equal(summary.result, "blocked");
  assert.equal(summary.phase, "host-offerings");
  assert.match(summary.blocker.message, /does not expose EC2 Mac host lifecycle admin endpoints/);
  assert.match(summary.blocker.message, /\/v1\/admin\/mac-hosts/);
  assert.match(summary.blocker.remediation, /Deploy a coordinator/);
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
  assert.match(summary.blocker.message, /ec2:AllocateHosts/);
  assert.match(summary.blocker.message, /ec2:CreateTags/);
  assert.match(summary.blocker.remediation, /Apply the EC2 Mac host lifecycle policy/);
  assert.deepEqual(summary.blocker.commands, [
    "crabbox admin aws-identity --region eu-west-1",
    "crabbox admin aws-policy --mac-hosts",
    "coordinator_account=$(crabbox admin aws-identity --region eu-west-1 --json | jq -r .account)",
    "local_account=$(aws sts get-caller-identity --query Account --output text)",
    'test "$local_account" = "$coordinator_account"',
    "crabbox admin mac-hosts allocate --region eu-west-1 --type mac2.metal --dry-run --json",
  ]);
  await assertFileContains(summary.evidence.awsProviderPolicy, /ec2:RunInstances/);
  await assertFileContains(summary.evidence.macHostPolicy, /ec2:AllocateHosts/);
  await assertFileContains(summary.evidence.macosImagePolicy, /ec2:AllocateHosts/);
  await assertFileContains(summary.evidence.hostOfferings, /mac2\.metal/);
  await assertFileContains(summary.evidence.hostList, /^\[\]\n?$/);
  await assertFileContains(summary.evidence.hostDryRun, /UnauthorizedOperation/);
  await assertFileContains(summary.evidence.hostQuota, /Running Dedicated mac2 Hosts/);
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
  assert.match(summary.blocker.message, /ec2:AllocateHosts/);
  assert.match(summary.blocker.message, /quota preflight also failed/);
  assert.match(summary.blocker.remediation, /servicequotas:ListServiceQuotas/);
  assert.deepEqual(summary.blocker.commands, [
    "crabbox admin aws-identity --region eu-west-1",
    "crabbox admin aws-policy --mac-hosts",
    "coordinator_account=$(crabbox admin aws-identity --region eu-west-1 --json | jq -r .account)",
    "local_account=$(aws sts get-caller-identity --query Account --output text)",
    'test "$local_account" = "$coordinator_account"',
    "crabbox admin mac-hosts quota --region eu-west-1 --type mac2.metal --json",
    "crabbox admin mac-hosts allocate --region eu-west-1 --type mac2.metal --dry-run --json",
  ]);
  await assertFileContains(summary.evidence.hostQuota, /servicequotas:ListServiceQuotas/);
  await assertFileContains(summary.evidence.hostDryRun, /UnauthorizedOperation/);
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
  await assertFileContains(summary.evidence.hostQuota, /Running Dedicated mac2 Hosts/);
  assert.equal(summary.evidence.hostAllocate, null);

  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.match(fakeLog, /^admin mac-hosts quota --region eu-west-1 --type mac2\.metal --json$/m);
  assert.doesNotMatch(fakeLog, /^admin mac-hosts allocate --region eu-west-1 --type mac2\.metal --force --json$/m);
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
  await assertFileContains(summary.evidence.regionPreflight, /"selectedRegion": "us-west-2"/);

  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.match(fakeLog, /^admin mac-hosts allocate --region eu-west-1 --type mac2\.metal --dry-run --json$/m);
  assert.match(fakeLog, /^admin mac-hosts allocate --region us-west-2 --type mac2\.metal --dry-run --json$/m);
  assert.match(fakeLog, /^admin mac-hosts quota --region us-west-2 --type mac2\.metal --json$/m);
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
  await assertFileContains(summary.evidence.regionPreflight, /"selectedInstanceType": "mac1.metal"/);

  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.match(fakeLog, /^admin mac-hosts allocate --region us-west-2 --type mac1\.metal --dry-run --json$/m);
  assert.match(fakeLog, /^admin mac-hosts quota --region us-west-2 --type mac1\.metal --json$/m);
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
    CRABBOX_MACOS_RELEASE_HOST: "1",
    CRABBOX_MACOS_ARTIFACT_DIR: run.artifacts,
    CRABBOX_MACOS_IMAGE_NAME: "full",
    CRABBOX_MACOS_WEBVNC_START_GRACE: "0s",
  });

  assert.equal(result.code, 0, result.stdout + result.stderr);
  const summary = await readJSON(path.join(run.artifacts, "summary.json"));
  assert.equal(summary.result, "passed");
  assert.equal(summary.phase, "promoted");
  assert.equal(summary.host.id, "h-mock");
  assert.equal(summary.host.allocatedByScript, true);
  assert.equal(summary.host.released, true);
  assert.equal(summary.image.amiId, "ami-mock");

  for (const label of ["source", "candidate", "promoted"]) {
    assert.equal(summary.artifacts[label], path.join(run.artifacts, label));
    await readdir(summary.artifacts[label]);
    await assertFileContains(summary.evidence.hostWait[label], /host h-mock is available/);
    await assertFileContains(summary.evidence.warmup[label], /"leaseId":"cbx_/);
    await assertFileContains(summary.evidence.webvncDaemon[label], /webvnc daemon: ready/);
    await assertFileContains(summary.evidence.webvncStatus[label], /portal bridge: connected=true/);
  }
  await assertFileContains(summary.evidence.imageCreate, /ami-mock/);
  await assertFileContains(summary.evidence.imagePromote, /"target":"macos"/);
  await assertFileContains(summary.evidence.awsProviderPolicy, /ec2:RunInstances/);
  await assertFileContains(summary.evidence.macHostPolicy, /ec2:AllocateHosts/);
  await assertFileContains(summary.evidence.macosImagePolicy, /ec2:AllocateHosts/);
  await assertFileContains(summary.evidence.hostQuota, /Running Dedicated mac2 Hosts/);

  const evidenceFiles = await readdir(path.join(run.artifacts, "evidence"));
  assert.deepEqual(
    evidenceFiles.filter((name) => name.startsWith("webvnc-status-")).sort(),
    ["webvnc-status-candidate.log", "webvnc-status-promoted.log", "webvnc-status-source.log"],
  );
  assert.deepEqual(
    evidenceFiles.filter((name) => name.startsWith("webvnc-daemon-")).sort(),
    ["webvnc-daemon-candidate.log", "webvnc-daemon-promoted.log", "webvnc-daemon-source.log"],
  );

  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.equal((fakeLog.match(/^warmup\b/gm) ?? []).length, 3);
  assert.equal((fakeLog.match(/^webvnc daemon start\b/gm) ?? []).length, 3);
  assert.equal((fakeLog.match(/^webvnc status\b/gm) ?? []).length, 3);
  assert.match(fakeLog, /^admin mac-hosts quota --region eu-west-1 --type mac2\.metal --json$/m);
  assert.match(fakeLog, /^admin mac-hosts release h-mock --region eu-west-1 --force$/m);
});
