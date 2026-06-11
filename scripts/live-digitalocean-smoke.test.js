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
  const smokeScript = path.join(tempScripts, "live-digitalocean-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-digitalocean-smoke.sh"), smokeScript);
  fs.chmodSync(smokeScript, 0o755);
  return { tempRoot, smokeScript };
}

test("live digitalocean smoke skips unless opted in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-do-skip-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: { ...process.env, CRABBOX_LIVE: "", DIGITALOCEAN_TOKEN: "" },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
});

test("live digitalocean smoke runs guarded lifecycle and redacts token", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-do-"));
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
if [[ "\${DIGITALOCEAN_TOKEN:-}" != "test-secret-token" ]]; then
  printf 'missing token\n' >&2
  exit 91
fi
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready api=list mutation=false leases=0 runtime=unchecked default_type=s-1vcpu-1gb region=nyc3 image=ubuntu-24-04-x64\n'
    ;;
  warmup)
    printf '%s\n' "$5" >"${slugFile}"
    ;;
  run)
    printf 'ok\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -f "${slugFile}.stopped" ]]; then
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
      CRABBOX_LIVE_PROVIDERS: "digitalocean",
      DIGITALOCEAN_TOKEN: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_digitalocean_smoke_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-secret-token/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider digitalocean");
  assert.match(seen[1], /^warmup --provider digitalocean --slug digitalocean-smoke-\d{14}-\d+ --keep --type s-1vcpu-1gb --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[2], /^run --provider digitalocean --id digitalocean-smoke-\d{14}-\d+ --no-sync -- echo ok$/);
  assert.equal(seen[3], "list --provider digitalocean --json");
  assert.match(seen[4], /^stop --provider digitalocean digitalocean-smoke-\d{14}-\d+$/);
  assert.equal(seen[5], "cleanup --provider digitalocean --dry-run");
  assert.equal(seen[6], "list --provider digitalocean --json");
});

test("live digitalocean smoke attempts cleanup after partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-do-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
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
if [[ "$1" == "doctor" ]]; then
  printf 'auth=ready\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  printf 'created droplet before failing\n' >&2
  exit 37
fi
if [[ "$1" == "stop" ]]; then
  printf '%s\n' "$4" >>"${stopped}"
  exit 0
fi
if [[ "$1" == "cleanup" ]]; then
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
      CRABBOX_LIVE_PROVIDERS: "digitalocean",
      DIGITALOCEAN_TOKEN: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(result.stderr, /created droplet before failing/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^digitalocean-smoke-\d{14}-\d+\n$/);
});
