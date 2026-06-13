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

function makeTempHarness(name, crabboxBody, nscBody = defaultNSCBody()) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `crabbox-live-${name}-`));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const fakeNSC = path.join(bin, "nsc");
  const crabboxLog = path.join(dir, "crabbox.log");
  const nscLog = path.join(dir, "nsc.log");
  fs.mkdirSync(bin);
  writeExecutable(fakeCrabbox, crabboxBody);
  writeExecutable(fakeNSC, nscBody);
  return { bin, crabboxLog, dir, fakeCrabbox, fakeNSC, nscLog };
}

function runLiveSmoke(harness, env = {}) {
  return spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${harness.bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_CONFIG: path.join(harness.dir, "missing-crabbox.yaml"),
      CRABBOX_FAKE_LOG: harness.crabboxLog,
      CRABBOX_LIVE_COORDINATOR: "0",
      NSC_FAKE_LOG: harness.nscLog,
      ...env,
    },
    encoding: "utf8",
  });
}

function defaultNSCBody() {
  return `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${NSC_FAKE_LOG:?}"
case "$*" in
  "auth check-login")
    printf 'logged in\\n'
    ;;
  *)
    printf 'unexpected nsc args: %s\\n' "$*" >&2
    exit 98
    ;;
esac
`;
}

function lifecycleCrabboxBody({ doctorFailure = false, failAfterWarmup = false } = {}) {
  return `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  config)
    exit 0
    ;;
  doctor)
    [[ "$*" == "doctor --provider namespace-instance" ]] || exit 97
    ${doctorFailure ? "printf 'nsc auth check-login failed\\n' >&2\n    exit 2" : ""}
    printf 'ok provider=namespace-instance\\n'
    ;;
  warmup)
    printf 'provisioning provider=namespace-instance lease=cbx_123456789abc slug=namespace-instance-smoke-test type=linux-small keep=false\\n'
    printf 'provisioned lease=cbx_123456789abc slug=namespace-instance-smoke-test state=ready\\n'
    ;;
  status)
    ${failAfterWarmup ? "printf 'status failed after create\\n' >&2\n    exit 42" : "printf 'lease=cbx_123456789abc slug=namespace-instance-smoke-test provider=namespace-instance state=ready ready=true\\n'"}
    ;;
  run)
    printf 'crabbox-namespace-instance-ok\\n'
    ;;
  list)
    printf '[{"id":"cbx_123456789abc","slug":"namespace-instance-smoke-test","provider":"namespace-instance","state":"ready"}]\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`;
}

test("default live smoke does not run namespace-instance", () => {
  const harness = makeTempHarness("namespace-instance-default", lifecycleCrabboxBody());
  const env = { ...process.env };
  delete env.CRABBOX_LIVE_PROVIDERS;

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...env,
      PATH: `${harness.bin}${path.delimiter}${env.PATH ?? ""}`,
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_CONFIG: path.join(harness.dir, "missing-crabbox.yaml"),
      CRABBOX_FAKE_LOG: harness.crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_REPO: repoRoot,
      NSC_FAKE_LOG: harness.nscLog,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 99, result.stdout + result.stderr);
  const calls = fs.readFileSync(harness.crabboxLog, "utf8");
  assert.match(calls, /^warmup --provider aws/m);
  assert.doesNotMatch(calls, /--provider namespace-instance/);
  assert.doesNotMatch(calls, /^doctor --provider namespace-instance$/m);
});

test("namespace-instance live smoke requires CRABBOX_LIVE", () => {
  const harness = makeTempHarness("namespace-instance-no-live", lifecycleCrabboxBody());
  const result = runLiveSmoke(harness, {
    CRABBOX_LIVE_PROVIDERS: "namespace-instance",
    CRABBOX_LIVE_REPO: repoRoot,
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(result.stderr, /set CRABBOX_LIVE=1/);
  assert.equal(fs.existsSync(harness.crabboxLog), false);
});

test("namespace-instance live smoke requires explicit repo path", () => {
  const harness = makeTempHarness("namespace-instance-no-repo", lifecycleCrabboxBody());
  const result = runLiveSmoke(harness, {
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "namespace-instance",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(result.stderr, /requires CRABBOX_LIVE_REPO/);
  assert.equal(fs.existsSync(harness.crabboxLog), false);
});

test("namespace-instance live smoke requires nsc on PATH", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-namespace-instance-missing-nsc-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);
  writeExecutable(fakeCrabbox, lifecycleCrabboxBody());
  writeExecutable(path.join(bin, "jq"), "#!/usr/bin/env bash\ncat >/dev/null\n");
  writeExecutable(path.join(bin, "rg"), "#!/usr/bin/env bash\nexit 0\n");

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      PATH: `${bin}${path.delimiter}/bin${path.delimiter}/usr/bin`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "namespace-instance",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(result.stderr, /requires the authenticated Namespace nsc CLI/);
  assert.equal(fs.existsSync(crabboxLog), false);
});

test("namespace-instance live smoke uses routed doctor readiness", () => {
  const harness = makeTempHarness(
    "namespace-instance-doctor-readiness",
    lifecycleCrabboxBody({ doctorFailure: true }),
  );
  const result = runLiveSmoke(harness, {
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "namespace-instance",
    CRABBOX_LIVE_REPO: repoRoot,
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(result.stderr, /nsc auth check-login failed/);
  assert.match(fs.readFileSync(harness.crabboxLog, "utf8"), /^doctor --provider namespace-instance$/m);
  assert.equal(fs.existsSync(harness.nscLog), false);
});

test("namespace-instance live smoke dispatches the lifecycle sequence", () => {
  const harness = makeTempHarness("namespace-instance-lifecycle", lifecycleCrabboxBody());
  const result = runLiveSmoke(harness, {
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "namespace-instance",
    CRABBOX_LIVE_REPO: repoRoot,
    CRABBOX_LIVE_NAMESPACE_INSTANCE_SLUG: "namespace-instance-smoke-test",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-namespace-instance-ok/);
  const calls = fs.readFileSync(harness.crabboxLog, "utf8");
  assert.match(calls, /^doctor --provider namespace-instance$/m);
  assert.match(calls, /^warmup --provider namespace-instance --slug namespace-instance-smoke-test --ttl 15m --idle-timeout 5m --namespace-instance-duration 15m --namespace-instance-machine-type linux-small --timing-json$/m);
  assert.match(calls, /^status --provider namespace-instance --id namespace-instance-smoke-test --wait --wait-timeout 5m$/m);
  assert.match(calls, /^run --provider namespace-instance --id namespace-instance-smoke-test --no-sync -- echo crabbox-namespace-instance-ok$/m);
  assert.match(calls, /^list --provider namespace-instance --json$/m);
  assert.match(calls, /^stop --provider namespace-instance namespace-instance-smoke-test$/m);
  assert.equal(fs.existsSync(harness.nscLog), false);
});

test("namespace-instance live smoke cleanup trap stops created lease", () => {
  const harness = makeTempHarness("namespace-instance-cleanup", lifecycleCrabboxBody({ failAfterWarmup: true }));
  const result = runLiveSmoke(harness, {
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "namespace-instance",
    CRABBOX_LIVE_REPO: repoRoot,
    CRABBOX_LIVE_NAMESPACE_INSTANCE_SLUG: "namespace-instance-smoke-test",
  });

  assert.equal(result.status, 42, result.stdout + result.stderr);
  assert.match(result.stderr, /status failed after create/);
  const calls = fs.readFileSync(harness.crabboxLog, "utf8");
  assert.match(calls, /^warmup --provider namespace-instance/m);
  assert.match(calls, /^status --provider namespace-instance --id namespace-instance-smoke-test --wait --wait-timeout 5m$/m);
  assert.match(calls, /^stop --provider namespace-instance namespace-instance-smoke-test$/m);
});
