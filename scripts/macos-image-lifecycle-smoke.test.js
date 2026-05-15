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

if [[ "$1" == "admin" && "$2" == "aws-policy" ]]; then
  printf '{"Statement":[{"Action":"ec2:RunInstances"}]}\\n'
  exit 0
fi

if [[ "$1" == "admin" && "$2" == "mac-hosts" ]]; then
  case "$3" in
    policy)
      printf '{"Statement":[{"Action":"ec2:AllocateHosts"}]}\\n'
      ;;
    offerings)
      if [[ "\${CRABBOX_FAKE_OFFERINGS_404:-0}" == "1" ]]; then
        printf 'coordinator GET /v1/admin/mac-hosts/offerings?region=eu-west-1&type=mac2.metal: http 404: {"error":"not_found"}\\n' >&2
        exit 1
      fi
      printf 'eu-west-1    eu-west-1b     mac2.metal\\n'
      ;;
    list)
      if [[ "\${CRABBOX_FAKE_NO_HOST:-0}" == "1" && ! -f "$state_dir/host" ]]; then
        printf '[]\\n'
      else
        printf '[{"id":"h-mock","instanceType":"mac2.metal","state":"available"}]\\n'
      fi
      ;;
    allocate)
      if [[ " $* " == *" --dry-run "* ]]; then
        if [[ "\${CRABBOX_FAKE_DRY_RUN:-allow}" == "deny" ]]; then
          printf '[{"region":"eu-west-1","availabilityZone":"eu-west-1b","instanceType":"mac2.metal","ok":false,"message":"UnauthorizedOperation: coordinator AWS identity needs EC2 Mac host lifecycle permissions, including ec2:AllocateHosts and ec2:CreateTags"}]\\n'
        else
          printf '[{"region":"eu-west-1","availabilityZone":"eu-west-1b","instanceType":"mac2.metal","ok":true,"message":"DryRunOperation"}]\\n'
        fi
      else
        : >"$state_dir/host"
        printf '[{"id":"h-mock","instanceType":"mac2.metal","state":"available"}]\\n'
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
    `${run.fake} admin aws-identity --region eu-west-1`,
    `${run.fake} admin mac-hosts policy`,
    `${run.fake} admin aws-policy`,
    `${run.fake} admin mac-hosts allocate --region eu-west-1 --type mac2.metal --dry-run --json`,
  ]);
  await assertFileContains(summary.evidence.awsProviderPolicy, /ec2:RunInstances/);
  await assertFileContains(summary.evidence.macHostPolicy, /ec2:AllocateHosts/);
  await assertFileContains(summary.evidence.hostOfferings, /mac2\.metal/);
  await assertFileContains(summary.evidence.hostList, /^\[\]\n?$/);
  await assertFileContains(summary.evidence.hostDryRun, /UnauthorizedOperation/);
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
    await assertFileContains(summary.evidence.hostWait[label], /host h-mock is available/);
    await assertFileContains(summary.evidence.warmup[label], /"leaseId":"cbx_/);
    await assertFileContains(summary.evidence.webvncStatus[label], /portal bridge: connected=true/);
  }
  await assertFileContains(summary.evidence.imageCreate, /ami-mock/);
  await assertFileContains(summary.evidence.imagePromote, /"target":"macos"/);
  await assertFileContains(summary.evidence.awsProviderPolicy, /ec2:RunInstances/);
  await assertFileContains(summary.evidence.macHostPolicy, /ec2:AllocateHosts/);

  const evidenceFiles = await readdir(path.join(run.artifacts, "evidence"));
  assert.deepEqual(
    evidenceFiles.filter((name) => name.startsWith("webvnc-status-")).sort(),
    ["webvnc-status-candidate.log", "webvnc-status-promoted.log", "webvnc-status-source.log"],
  );

  const fakeLog = await readFile(run.fakeLog, "utf8");
  assert.equal((fakeLog.match(/^warmup\b/gm) ?? []).length, 3);
  assert.equal((fakeLog.match(/^webvnc status\b/gm) ?? []).length, 3);
  assert.match(fakeLog, /^admin mac-hosts release h-mock --region eu-west-1 --force$/m);
});
