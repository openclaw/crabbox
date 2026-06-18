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
  const smokeScript = path.join(tempScripts, "live-nebius-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-nebius-smoke.sh"), smokeScript);
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
if [[ ! -d "$(dirname "$out")" ]]; then
  printf 'output directory missing: %s\\n' "$(dirname "$out")" >&2
  exit 88
fi
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

test("live nebius smoke skips unless globally opted in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nebius-skip-"));
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
      CRABBOX_LIVE_PROVIDERS: "nebius",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
});

test("live nebius smoke skips unless nebius is selected", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nebius-provider-skip-"));
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
      CRABBOX_LIVE_PROVIDERS: "digitalocean",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=nebius_not_selected/);
});

test("live nebius smoke runs guarded lifecycle without real credentials", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nebius-"));
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
    printf 'auth=ready control_plane=ready inventory=ready leases=0 runtime=unchecked\\n'
    ;;
  warmup)
    arg_after --slug "$@" >"${slugFile}"
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
      printf '[{"labels":{"crabbox_slug":"%s"},"provider":"nebius"}]\\n' "$slug"
    fi
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
    ;;
  cleanup)
    printf 'skip nebius instance id=none name=none reason=missing labels\\n'
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
      CRABBOX_LIVE_PROVIDERS: " nebius ",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_nebius_smoke_passed/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider nebius");
  assert.equal(seen[1], "list --provider nebius --json");
  assert.match(seen[2], /^warmup --provider nebius --slug nebius-smoke-\d+-\d+-[0-9a-f]{8} --keep --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider nebius --id nebius-smoke-\d+-\d+-[0-9a-f]{8} --wait --wait-timeout 300s$/);
  assert.match(seen[4], /^run --provider nebius --id nebius-smoke-\d+-\d+-[0-9a-f]{8} --no-sync -- echo ok$/);
  assert.equal(seen[5], "list --provider nebius --json");
  assert.match(seen[6], /^stop --provider nebius nebius-smoke-\d+-\d+-[0-9a-f]{8}$/);
  assert.equal(seen[7], "cleanup --provider nebius --dry-run");
  assert.equal(seen[8], "list --provider nebius --json");
});

test("live nebius smoke attempts cleanup after partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nebius-fail-"));
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
  printf 'created vm before failing\\n' >&2
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
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "nebius",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(result.stderr, /created vm before failing/);
  assert.match(fs.readFileSync(calls, "utf8"), /warmup .* --keep /);
  assert.match(fs.readFileSync(calls, "utf8"), /stop --provider nebius nebius-smoke-\d+-\d+-[0-9a-f]{8}/);
});

test("live nebius smoke validates list JSON contains the smoke slug", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nebius-bad-list-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
${shellArgHelper}
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
    printf 'ok\\n'
    ;;
  list)
    printf '[{"labels":{"slug":"different"}}]\\n'
    ;;
  stop)
    exit 0
    ;;
  cleanup)
    exit 0
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
      CRABBOX_LIVE_PROVIDERS: "nebius",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /list JSON did not include slug/);
});

test("live nebius smoke classifies quota and capacity failures", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nebius-quota-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "doctor" || "$1" == "list" ]]; then
  [[ "$1" == "list" ]] && printf '[]\\n' || printf 'auth=ready\\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  printf 'quota limit exceeded for platform\\n' >&2
  exit 42
fi
if [[ "$1" == "stop" ]]; then
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
      CRABBOX_LIVE_PROVIDERS: "nebius",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 42, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=quota_blocked/);
});

test("live nebius smoke reports targeted cleanup failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nebius-cleanup-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "doctor" || "$1" == "list" ]]; then
  [[ "$1" == "list" ]] && printf '[]\\n' || printf 'auth=ready\\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  printf 'created vm before failing\\n' >&2
  exit 37
fi
if [[ "$1" == "stop" ]]; then
  printf 'nebius instance still deleting\\n' >&2
  exit 5
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
      CRABBOX_LIVE_PROVIDERS: "nebius",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /reason=cleanup_failed/);
  assert.match(result.stderr, /nebius instance still deleting/);
});
