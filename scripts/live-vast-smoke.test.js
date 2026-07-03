import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");

function writeExecutable(file, body) {
  fs.writeFileSync(file, body, "utf8");
  fs.chmodSync(file, 0o755);
}

function prepareSmokeRepo(dir) {
  const tempRoot = path.join(dir, "repo");
  const tempScripts = path.join(tempRoot, "scripts");
  const smokeScript = path.join(tempScripts, "live-vast-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-vast-smoke.sh"), smokeScript);
  fs.chmodSync(smokeScript, 0o755);
  return { tempRoot, smokeScript };
}

function writeGoStub(binDir, scriptBody) {
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
${scriptBody}
SCRIPT
chmod +x "$out"
`,
  );
}

const shellArgHelper = `
arg_after() {
  local want="$1"
  shift
  while [[ "$#" -gt 0 ]]; do
    if [[ "$1" == "$want" ]]; then
      printf '%s' "$2"
      return 0
    fi
    shift
  done
  return 1
}
`;

function vastLiveEnv(overrides = {}) {
  return {
    ...process.env,
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "vast",
    CRABBOX_VAST_API_KEY: "test-secret-token",
    CRABBOX_LIVE_VAST_GPU_COUNT: "1",
    CRABBOX_LIVE_VAST_MAX_DPH_TOTAL: "0.50",
    CRABBOX_LIVE_VAST_RELEASE_ACTION: "destroy",
    ...overrides,
  };
}

test("live vast smoke skips unless globally opted in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-vast-skip-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(binDir, { recursive: true });
  writeExecutable(path.join(binDir, "go"), "#!/usr/bin/env bash\nexit 99\n");

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "",
      CRABBOX_LIVE_PROVIDERS: "vast",
      CRABBOX_VAST_API_KEY: "",
      VAST_API_KEY: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
});

test("live vast smoke skips unless vast is selected", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-vast-provider-skip-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(binDir, { recursive: true });
  writeExecutable(path.join(binDir, "go"), "#!/usr/bin/env bash\nexit 99\n");

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "lambda",
      CRABBOX_VAST_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=vast_not_selected/);
});

test("live vast smoke requires token before building", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-vast-token-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(binDir, { recursive: true });
  writeExecutable(path.join(binDir, "go"), "#!/usr/bin/env bash\nexit 99\n");

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "vast",
      CRABBOX_VAST_API_KEY: "",
      VAST_API_KEY: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=VAST_API_KEY_missing/);
});

test("live vast smoke rejects unsafe billing and cleanup settings before building", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-vast-safety-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const buildMarker = path.join(dir, "build-called");
  fs.mkdirSync(binDir, { recursive: true });
  writeExecutable(path.join(binDir, "go"), `#!/usr/bin/env bash\ntouch "${buildMarker}"\nexit 99\n`);

  const cases = [
    [{ CRABBOX_LIVE_VAST_MAX_DPH_TOTAL: "" }, /VAST_cost_cap_missing/],
    [{ CRABBOX_LIVE_VAST_MAX_DPH_TOTAL: "0" }, /VAST_cost_cap_invalid/],
    [{ CRABBOX_LIVE_VAST_MAX_DPH_TOTAL: "-1" }, /VAST_cost_cap_invalid/],
    [{ CRABBOX_LIVE_VAST_MAX_DPH_TOTAL: "NaN" }, /VAST_cost_cap_invalid/],
    [{ CRABBOX_LIVE_VAST_MAX_DPH_TOTAL: "Infinity" }, /VAST_cost_cap_invalid/],
    [{ CRABBOX_LIVE_VAST_GPU_COUNT: "" }, /VAST_gpu_count_invalid/],
    [{ CRABBOX_LIVE_VAST_GPU_COUNT: "0" }, /VAST_gpu_count_invalid/],
    [{ CRABBOX_LIVE_VAST_GPU_COUNT: "1.5" }, /VAST_gpu_count_invalid/],
    [{ CRABBOX_LIVE_VAST_GPU_COUNT: "many" }, /VAST_gpu_count_invalid/],
    [{ CRABBOX_LIVE_VAST_RELEASE_ACTION: "stop" }, /VAST_release_action_not_destroy/],
    [{ CRABBOX_LIVE_VAST_RELEASE_ACTION: "keep" }, /VAST_release_action_not_destroy/],
  ];

  for (const [overrides, reason] of cases) {
    const result = spawnSync("bash", [smokeScript], {
      cwd: tempRoot,
      env: vastLiveEnv({
        PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
        ...overrides,
      }),
      encoding: "utf8",
    });

    assert.equal(result.status, 2, result.stdout + result.stderr);
    assert.match(result.stderr, /classification=validation_failed/);
    assert.match(result.stderr, reason);
    assert.doesNotMatch(result.stdout + result.stderr, /classification=live_vast_smoke_passed/);
    assert.equal(fs.existsSync(buildMarker), false);
  }
});

test("live vast smoke runs guarded lifecycle and redacts secret material", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-vast-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
${shellArgHelper}
printf '%s\\n' "$*" >>"${calls}"
if [[ "\${CRABBOX_VAST_API_KEY:-}" != "test-secret-token" ]]; then
  printf 'missing token\\n' >&2
  exit 91
fi
case "$1" in
  doctor)
    grep -q '^  gpuCount: 1$' "$CRABBOX_CONFIG"
    grep -q '^  maxDphTotal: 0.50$' "$CRABBOX_CONFIG"
    grep -q '^  releaseAction: destroy$' "$CRABBOX_CONFIG"
    printf 'auth=ready control_plane=ready inventory=ready api=list mutation=false leases=0 runtime=unchecked api_key=test-secret-token instance_api_key=visible jupyter_url=https://example.test/?token=abc\\n'
    ;;
  warmup)
    arg_after --slug "$@" >"${slugFile}"
    ;;
  status)
    printf 'status=ready\\n'
    ;;
  run)
    printf 'GPU 0: NVIDIA RTX Test (UUID: GPU-test)\\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"},"provider":"vast"}]\\n' "$slug"
    fi
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
    ;;
  cleanup)
    printf 'skip vast instance id=none name=none reason=missing labels\\n'
    ;;
  *)
    printf 'unexpected args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: vastLiveEnv({
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE_PROVIDERS: " vast-ai ",
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(
    result.stdout,
    /classification=live_vast_smoke_passed slug=vast-smoke-\d{14}-\d+ minimum_gpu_count=1 actual_gpu_count=1 max_dph_total=0\.50 release_action=destroy pre_owned=0 post_owned=0 cleanup=complete/,
  );
  assert.doesNotMatch(result.stdout + result.stderr, /test-secret-token|visible|token=abc/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider vast");
  assert.equal(seen[1], "list --provider vast --json");
  assert.match(seen[2], /^warmup --provider vast --slug vast-smoke-\d{14}-\d+ --keep --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider vast --id vast-smoke-\d{14}-\d+ --wait --wait-timeout 600s$/);
  assert.match(seen[4], /^run --provider vast --id vast-smoke-\d{14}-\d+ --no-sync -- nvidia-smi -L$/);
  assert.equal(seen[5], "list --provider vast --json");
  assert.match(seen[6], /^stop --provider vast vast-smoke-\d{14}-\d+$/);
  assert.equal(seen[7], "cleanup --provider vast --dry-run");
  assert.equal(seen[8], "list --provider vast --json");
});

test("live vast smoke blocks provisioning when owned inventory is not empty", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-vast-preexisting-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    printf 'auth=ready\\n'
    ;;
  list)
    printf '[{"labels":{"slug":"existing-owned"}}]\\n'
    ;;
  *)
    printf 'unexpected mutation: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: vastLiveEnv({ PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}` }),
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /Vast Crabbox inventory is not empty/);
  assert.doesNotMatch(result.stdout + result.stderr, /classification=live_vast_smoke_passed/);
  assert.doesNotMatch(fs.readFileSync(calls, "utf8"), /warmup|stop/);
});

test("live vast smoke keeps cleanup armed until final inventory is empty", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-vast-residue-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
${shellArgHelper}
printf '%s\\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    printf 'auth=ready\\n'
    ;;
  warmup)
    arg_after --slug "$@" >"${slugFile}"
    ;;
  status)
    printf 'status=ready\\n'
    ;;
  run)
    printf 'GPU 0: NVIDIA Test\\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"}}]\\n' "$slug"
    fi
    ;;
  stop)
    ;;
  cleanup)
    printf 'cleanup dry-run\\n'
    ;;
  *)
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: vastLiveEnv({ PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}` }),
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /Vast Crabbox inventory is not empty/);
  assert.doesNotMatch(result.stdout + result.stderr, /classification=live_vast_smoke_passed/);
  const stopCalls = fs
    .readFileSync(calls, "utf8")
    .split("\n")
    .filter((line) => line.startsWith("stop --provider vast "));
  assert.equal(stopCalls.length, 2);
});

test("live vast smoke attempts cleanup after partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-vast-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
if [[ "$1" == "doctor" || "$1" == "list" ]]; then
  [[ "$1" == "list" ]] && printf '[]\\n' || printf 'auth=ready\\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  printf 'no eligible offers after partial create\\n' >&2
  exit 37
fi
if [[ "$1" == "stop" ]]; then
  exit 0
fi
exit 99
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: vastLiveEnv({
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE_PROVIDERS: "vast",
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=capacity_blocked/);
  assert.match(result.stderr, /no eligible offers/);
  assert.doesNotMatch(result.stdout + result.stderr, /classification=live_vast_smoke_passed/);
  assert.match(fs.readFileSync(calls, "utf8"), /warmup .* --keep /);
  assert.match(fs.readFileSync(calls, "utf8"), /stop --provider vast vast-smoke-\d{14}-\d+/);
});

test("live vast smoke preserves capacity classification when pre-acquire cleanup finds no lease", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-vast-no-lease-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
if [[ "$1" == "doctor" || "$1" == "list" ]]; then
  [[ "$1" == "list" ]] && printf '[]\\n' || printf 'auth=ready\\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  printf 'no eligible offers found\\n' >&2
  exit 37
fi
if [[ "$1" == "stop" ]]; then
  printf 'lease not found\\n' >&2
  exit 44
fi
exit 99
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: vastLiveEnv({
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE_PROVIDERS: "vast",
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=capacity_blocked/);
  assert.doesNotMatch(result.stderr, /classification=cleanup_failed/);
  assert.doesNotMatch(result.stdout + result.stderr, /classification=live_vast_smoke_passed/);
  assert.match(fs.readFileSync(calls, "utf8"), /stop --provider vast vast-smoke-\d{14}-\d+/);
});

test("live vast smoke validates minimum GPU count and cleans up on mismatch", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-vast-bad-run-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const slugFile = path.join(dir, "slug.txt");
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
${shellArgHelper}
printf '%s\\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    printf 'auth=ready\\n'
    ;;
  warmup)
    arg_after --slug "$@" >"${slugFile}"
    ;;
  status)
    printf 'status=ready\\n'
    ;;
  run)
    printf 'no GPUs reported\\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"}}]\\n' "$slug"
    fi
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
    ;;
  cleanup)
    printf 'cleanup dry-run\\n'
    ;;
  *)
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: vastLiveEnv({
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE_PROVIDERS: "vast",
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /remote GPU count mismatch: expected at least 1, got 0/);
  assert.doesNotMatch(result.stdout + result.stderr, /classification=live_vast_smoke_passed/);
  assert.match(fs.readFileSync(calls, "utf8"), /stop --provider vast vast-smoke-\d{14}-\d+/);
});
