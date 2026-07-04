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
  const smokeScript = path.join(tempScripts, "live-runpod-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-runpod-smoke.sh"), smokeScript);
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

test("live RunPod smoke embedded cleanup Python compiles", () => {
  const script = fs.readFileSync(path.join(repoRoot, "scripts", "live-runpod-smoke.sh"), "utf8");
  const match = script.match(/raw_runpod_delete_slug\(\) \{\n  CRABBOX_SMOKE_SLUG="\$slug" python3 -c '\n([\s\S]*?)\n'\n\}/);
  assert.ok(match, "raw_runpod_delete_slug Python block should be extractable");
  const result = spawnSync("python3", ["-c", `compile(${JSON.stringify(match[1])}, "raw_runpod_delete_slug", "exec")`], {
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
});

test("live RunPod smoke skips unless opted in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-runpod-skip-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: { ...process.env, CRABBOX_LIVE: "", CRABBOX_RUNPOD_API_KEY: "", RUNPOD_API_KEY: "" },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
});

test("live RunPod smoke skips unless provider filter selects RunPod", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-runpod-filter-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "aws,digitalocean",
      RUNPOD_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=runpod_not_selected/);
});

test("live RunPod smoke requires a token before building", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-runpod-token-"));
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
      CRABBOX_LIVE_PROVIDERS: "runpod",
      CRABBOX_RUNPOD_API_KEY: "",
      RUNPOD_API_KEY: "",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=RUNPOD_API_KEY_missing/);
});

test("live RunPod smoke runs guarded lifecycle without exposing the token", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-runpod-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });
  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${calls}"
if [[ "\${RUNPOD_API_KEY:-}" != "test-secret-token" ]]; then
  exit 91
fi
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready mutation=false leases=0\n'
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
  *)
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "runpod",
      CRABBOX_RUNPOD_API_KEY: "test-secret-token",
      RUNPOD_API_KEY: "",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_runpod_smoke_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-secret-token/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider runpod");
  assert.equal(seen[1], "list --provider runpod --json");
  assert.match(seen[2], /^warmup --provider runpod --slug runpod-smoke-\d{14}-\d+ --keep --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider runpod --id runpod-smoke-\d{14}-\d+ --wait --wait-timeout 600s$/);
  assert.match(seen[4], /^run --provider runpod --id runpod-smoke-\d{14}-\d+ --no-sync -- echo ok$/);
  assert.equal(seen[5], "list --provider runpod --json");
  assert.match(seen[6], /^stop --provider runpod runpod-smoke-\d{14}-\d+$/);
  assert.equal(seen[7], "list --provider runpod --json");
});

test("live RunPod smoke attempts cleanup after a partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-runpod-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  doctor|list)
    printf '[]\n'
    ;;
  warmup)
    printf 'created RunPod pod before failing\n' >&2
    exit 37
    ;;
  stop)
    printf '%s\n' "$4" >>"${stopped}"
    ;;
  *)
    exit 99
    ;;
esac
`,
  );
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "runpod",
      RUNPOD_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^runpod-smoke-\d{14}-\d+\n$/);
});

test("live RunPod smoke refuses non-empty inventory before mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-runpod-nonempty-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    printf 'auth=ready\n'
    ;;
  list)
    printf '[{"labels":{"slug":"existing"}}]\n'
    ;;
  *)
    exit 99
    ;;
esac
`,
  );
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "runpod",
      RUNPOD_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /inventory is not empty/);
  assert.deepEqual(fs.readFileSync(calls, "utf8").trim().split("\n"), [
    "doctor --provider runpod",
    "list --provider runpod --json",
  ]);
});
