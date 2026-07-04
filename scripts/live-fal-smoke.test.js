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
  const smokeScript = path.join(tempScripts, "live-fal-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-fal-smoke.sh"), smokeScript);
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

test("live fal smoke skips unless opted in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-fal-skip-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: { ...process.env, CRABBOX_LIVE: "", CRABBOX_FAL_KEY: "", FAL_KEY: "" },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
});

test("live fal smoke skips unless provider filter selects fal", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-fal-filter-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "aws,digitalocean",
      CRABBOX_FAL_KEY: "test-secret-token",
      FAL_KEY: "",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=fal_not_selected/);
});

test("live fal smoke requires token before building", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-fal-token-"));
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
      CRABBOX_LIVE_PROVIDERS: "fal",
      CRABBOX_FAL_KEY: "",
      FAL_KEY: "",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=FAL_KEY_missing/);
});

test("live fal smoke runs guarded lifecycle and redacts token", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-fal-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
if [[ "\${CRABBOX_FAL_KEY:-}" != "test-secret-token" ]]; then
  printf 'missing token\\n' >&2
  exit 91
fi
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready api=list mutation=false api_key=test-secret-token url=https://example.test/?token=abc\\n'
    ;;
  warmup)
    printf '%s\\n' "$5" >"${slugFile}"
    ;;
  status)
    printf 'status=ready\\n'
    ;;
  run)
    printf 'ok\\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"},"provider":"fal"}]\\n' "$slug"
    fi
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
    ;;
  cleanup)
    printf 'skip server id=none name=none reason=missing labels\\n'
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
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "fal",
      CRABBOX_FAL_KEY: "test-secret-token",
      FAL_KEY: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_fal_smoke_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-secret-token|token=abc/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider fal");
  assert.equal(seen[1], "list --provider fal --json");
  assert.match(seen[2], /^warmup --provider fal --slug fal-smoke-\d{14}-\d+ --keep --fal-instance-type gpu_1x_h100_sxm5 --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider fal --id fal-smoke-\d{14}-\d+ --wait --wait-timeout 600s$/);
  assert.match(seen[4], /^run --provider fal --id fal-smoke-\d{14}-\d+ --no-sync -- echo ok$/);
  assert.equal(seen[5], "list --provider fal --json");
  assert.match(seen[6], /^stop --provider fal fal-smoke-\d{14}-\d+$/);
  assert.equal(seen[7], "cleanup --provider fal --dry-run");
  assert.equal(seen[8], "list --provider fal --json");
});

test("live fal smoke attempts cleanup after partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-fal-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
if [[ "$1" == "doctor" ]]; then
  printf 'auth=ready\\n'
  exit 0
fi
if [[ "$1" == "list" ]]; then
  printf '[]\\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  printf 'capacity unavailable after partial create\\n' >&2
  exit 37
fi
if [[ "$1" == "stop" ]]; then
  printf '%s\\n' "$4" >>"${stopped}"
  exit 0
fi
exit 99
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "fal",
      CRABBOX_FAL_KEY: "test-secret-token",
      FAL_KEY: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=capacity_blocked/);
  assert.match(result.stderr, /capacity unavailable after partial create/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^fal-smoke-\d{14}-\d+\n$/);
  assert.doesNotMatch(fs.readFileSync(calls, "utf8"), /^cleanup /m);
});

test("live fal smoke treats post-create lifecycle failure as validation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-fal-status-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  doctor)
    printf 'auth=ready\\n'
    ;;
  list)
    printf '[]\\n'
    ;;
  warmup)
    printf '%s\\n' "$5" >"${slugFile}"
    ;;
  status)
    printf 'status never became ready\\n' >&2
    exit 42
    ;;
  stop)
    printf '%s\\n' "$4" >>"${stopped}"
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
      CRABBOX_LIVE_PROVIDERS: "fal",
      CRABBOX_FAL_KEY: "test-secret-token",
      FAL_KEY: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 42, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /status never became ready/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^fal-smoke-\d{14}-\d+\n$/);
});

test("live fal smoke requires remote command output", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-fal-output-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  doctor)
    printf 'auth=ready\n'
    ;;
  list)
    printf '[]\n'
    ;;
  warmup)
    printf '%s\n' "$5" >"${slugFile}"
    ;;
  status)
    printf 'status=ready\n'
    ;;
  run)
    printf 'unexpected-output\n'
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
      CRABBOX_LIVE_PROVIDERS: "fal",
      CRABBOX_FAL_KEY: "test-secret-token",
      FAL_KEY: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /expected output line: ok/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^fal-smoke-\d{14}-\d+\n$/);
});
