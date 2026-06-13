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

function prepareSmokeRepo(dir) {
  const tempRoot = path.join(dir, "repo");
  const tempScripts = path.join(tempRoot, "scripts");
  const smokeScript = path.join(tempScripts, "live-ovh-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-ovh-smoke.sh"), smokeScript);
  fs.chmodSync(smokeScript, 0o755);
  return { tempRoot, smokeScript };
}

test("live ovh smoke skips unless opted in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ovh-skip-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: { ...process.env, CRABBOX_LIVE: "", OVH_APPLICATION_KEY: "" },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
});

test("live ovh smoke requires provider selection and credentials", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ovh-guard-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const unselected = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: { ...process.env, CRABBOX_LIVE: "1", CRABBOX_LIVE_PROVIDERS: "digitalocean" },
    encoding: "utf8",
  });
  assert.equal(unselected.status, 0, unselected.stderr);
  assert.match(unselected.stdout, /classification=environment_blocked reason=ovh_not_selected/);

  const missingKey = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: { ...process.env, CRABBOX_LIVE: "1", CRABBOX_LIVE_PROVIDERS: "ovh" },
    encoding: "utf8",
  });
  assert.equal(missingKey.status, 0, missingKey.stderr);
  assert.match(missingKey.stdout, /classification=environment_blocked reason=OVH_APPLICATION_KEY_missing/);
});

test("live ovh smoke runs guarded lifecycle and redacts credentials", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ovh-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });

  writeExecutable(
    path.join(binDir, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
out=""
while [[ "$#" -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then
    out="$2"
    shift 2
    continue
  fi
  shift
done
mkdir -p "$(dirname "$out")"
cat >"$out" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${calls}"
if [[ "\${OVH_APPLICATION_SECRET:-}" != "test-ovh-secret" || "\${OVH_CONSUMER_KEY:-}" != "test-consumer-key" ]]; then
  printf 'missing ovh auth\n' >&2
  exit 91
fi
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready mutation=false leases=0 endpoint=https://api.us.ovhcloud.com/1.0 region=BHS5 image=Ubuntu 24.04 flavor=b3-8\n'
    ;;
  warmup)
    printf '%s\n' "$5" >"${slugFile}"
    ;;
  status)
    printf 'status=ready\n'
    ;;
  run)
    printf 'ok\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\n'
    else
      printf '[{"labels":{"slug":"%s"}}]\n' "$slug"
    fi
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
    ;;
  cleanup)
    printf 'skip server id=none name=none reason=missing labels\n'
    ;;
  *)
    printf 'unexpected args: %s\n' "$*" >&2
    exit 99
    ;;
esac
SCRIPT
chmod +x "$out"
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "ovh",
      OVH_APPLICATION_KEY: "test-app-key",
      OVH_APPLICATION_SECRET: "test-ovh-secret",
      OVH_CONSUMER_KEY: "test-consumer-key",
      CRABBOX_OVH_PROJECT_ID: "project-test",
      CRABBOX_OVH_REGION: "BHS5",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_ovh_smoke_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-ovh-secret|test-consumer-key/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider ovh");
  assert.equal(seen[1], "list --provider ovh --json");
  assert.match(seen[2], /^warmup --provider ovh --slug ovh-smoke-\d{14}-\d+ --keep --type b3-8 --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider ovh --id ovh-smoke-\d{14}-\d+ --wait --wait-timeout 300s$/);
  assert.match(seen[4], /^run --provider ovh --id ovh-smoke-\d{14}-\d+ --no-sync -- echo ok$/);
  assert.equal(seen[5], "list --provider ovh --json");
  assert.match(seen[6], /^stop --provider ovh ovh-smoke-\d{14}-\d+$/);
  assert.equal(seen[7], "cleanup --provider ovh --dry-run");
  assert.equal(seen[8], "list --provider ovh --json");
});

test("live ovh smoke attempts targeted cleanup after partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ovh-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(binDir, { recursive: true });

  writeExecutable(
    path.join(binDir, "sleep"),
    `#!/usr/bin/env bash
exit 0
`,
  );
  writeExecutable(
    path.join(binDir, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
out=""
while [[ "$#" -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then out="$2"; shift 2; continue; fi
  shift
done
mkdir -p "$(dirname "$out")"
cat >"$out" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${calls}"
if [[ "$1" == "doctor" ]]; then
  printf 'auth=ready\n'
  exit 0
fi
if [[ "$1" == "list" ]]; then
  printf '[]\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  printf 'created ovh instance before failing\n' >&2
  exit 37
fi
if [[ "$1" == "stop" ]]; then
  printf '%s\n' "$4" >>"${stopped}"
  exit 0
fi
exit 99
SCRIPT
chmod +x "$out"
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "ovh",
      OVH_APPLICATION_KEY: "test-app-key",
      OVH_APPLICATION_SECRET: "test-ovh-secret",
      OVH_CONSUMER_KEY: "test-consumer-key",
      CRABBOX_OVH_PROJECT_ID: "project-test",
      CRABBOX_OVH_REGION: "BHS5",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(result.stderr, /created ovh instance before failing/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^ovh-smoke-\d{14}-\d+\n$/);
  assert.doesNotMatch(fs.readFileSync(calls, "utf8"), /^cleanup /m);
});

test("live ovh smoke cleanup retries beyond ambiguous-create grace", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ovh-retry-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const attempts = path.join(dir, "attempts.txt");
  fs.mkdirSync(binDir, { recursive: true });

  writeExecutable(path.join(binDir, "sleep"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    path.join(binDir, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
out=""
while [[ "$#" -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then out="$2"; shift 2; continue; fi
  shift
done
mkdir -p "$(dirname "$out")"
cat >"$out" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  doctor|list) [[ "$1" == "list" ]] && printf '[]\n' || printf 'auth=ready\n' ;;
  warmup) exit 37 ;;
  stop)
    count="$(cat "${attempts}" 2>/dev/null || printf 0)"
    count=$((count + 1))
    printf '%s' "$count" >"${attempts}"
    [[ "$count" -ge 61 ]]
    ;;
  *) exit 99 ;;
esac
SCRIPT
chmod +x "$out"
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "ovh",
      OVH_APPLICATION_KEY: "test-app-key",
      OVH_APPLICATION_SECRET: "test-ovh-secret",
      OVH_CONSUMER_KEY: "test-consumer-key",
      CRABBOX_OVH_PROJECT_ID: "project-test",
      CRABBOX_OVH_REGION: "BHS5",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.equal(fs.readFileSync(attempts, "utf8"), "61");
  assert.doesNotMatch(result.stderr, /classification=cleanup_failed/);
});

test("live ovh smoke classifies quota and validation failures", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ovh-classify-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(binDir, { recursive: true });

  writeExecutable(
    path.join(binDir, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
out=""
while [[ "$#" -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then out="$2"; shift 2; continue; fi
  shift
done
mkdir -p "$(dirname "$out")"
cat >"$out" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  doctor)
    printf 'auth=ready\n'
    ;;
  list)
    if [[ "\${CRABBOX_TEST_NONEMPTY:-}" == "1" ]]; then
      printf '[{"labels":{"slug":"existing"}}]\n'
    else
      printf '[]\n'
    fi
    ;;
  warmup)
    printf 'quota exceeded for requested flavor\n' >&2
    exit 42
    ;;
  stop)
    exit 0
    ;;
  *)
    exit 99
    ;;
esac
SCRIPT
chmod +x "$out"
`,
  );

  const baseEnv = {
    ...process.env,
    PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "ovh",
    OVH_APPLICATION_KEY: "test-app-key",
    OVH_APPLICATION_SECRET: "test-ovh-secret",
    OVH_CONSUMER_KEY: "test-consumer-key",
    CRABBOX_OVH_PROJECT_ID: "project-test",
    CRABBOX_OVH_REGION: "BHS5",
  };

  const quota = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: baseEnv,
    encoding: "utf8",
  });
  assert.equal(quota.status, 42, quota.stdout + quota.stderr);
  assert.match(quota.stderr, /classification=quota_blocked/);

  const nonempty = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: { ...baseEnv, CRABBOX_TEST_NONEMPTY: "1" },
    encoding: "utf8",
  });
  assert.equal(nonempty.status, 1, nonempty.stdout + nonempty.stderr);
  assert.match(nonempty.stderr, /classification=validation_failed/);
  assert.match(nonempty.stderr, /OVH Crabbox inventory is not empty/);
});
