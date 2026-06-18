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
  const smokeScript = path.join(tempScripts, "live-scaleway-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-scaleway-smoke.sh"), smokeScript);
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

const validEnv = {
  CRABBOX_LIVE: "1",
  CRABBOX_LIVE_PROVIDERS: "scaleway",
  SCW_ACCESS_KEY: "test-scaleway-access",
  SCW_SECRET_KEY: "test-scaleway-secret",
  SCW_DEFAULT_ORGANIZATION_ID: "org-test",
  SCW_DEFAULT_PROJECT_ID: "project-test",
  SCW_DEFAULT_REGION: "fr-par",
  SCW_DEFAULT_ZONE: "fr-par-1",
};

test("live scaleway smoke skips unless opted in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-scaleway-skip-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: { ...process.env, CRABBOX_LIVE: "", SCW_SECRET_KEY: "" },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
});

test("live scaleway smoke requires provider selection", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-scaleway-filter-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      ...validEnv,
      CRABBOX_LIVE_PROVIDERS: "aws,digitalocean",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=scaleway_not_selected/);
});

test("live scaleway smoke validates required env before building", () => {
  const cases = [
    ["SCW_ACCESS_KEY", /reason=SCW_ACCESS_KEY_missing/],
    ["SCW_SECRET_KEY", /reason=SCW_SECRET_KEY_missing/],
    ["SCW_DEFAULT_ORGANIZATION_ID", /reason=SCW_DEFAULT_ORGANIZATION_ID_missing/],
    ["SCW_DEFAULT_PROJECT_ID", /reason=SCW_DEFAULT_PROJECT_ID_missing/],
    ["SCW_DEFAULT_REGION", /reason=SCW_DEFAULT_REGION_missing/],
    ["SCW_DEFAULT_ZONE", /reason=SCW_DEFAULT_ZONE_missing/],
  ];

  for (const [missing, reason] of cases) {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), `crabbox-live-scaleway-${missing}-`));
    const binDir = path.join(dir, "bin");
    const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
    fs.mkdirSync(binDir, { recursive: true });
    writeExecutable(path.join(binDir, "go"), "#!/usr/bin/env bash\nexit 99\n");
    const result = spawnSync("bash", [smokeScript], {
      cwd: tempRoot,
      env: {
        ...process.env,
        ...validEnv,
        PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
        [missing]: "",
      },
      encoding: "utf8",
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, reason);
  }
});

test("live scaleway smoke runs guarded lifecycle and redacts keys", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-scaleway-"));
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
if [[ "\${SCW_ACCESS_KEY:-}" != "test-scaleway-access" || "\${SCW_SECRET_KEY:-}" != "test-scaleway-secret" ]]; then
  printf 'missing scaleway auth\\n' >&2
  exit 91
fi
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready api=list mutation=false leases=0 region=fr-par zone=fr-par-1 type=DEV1-S\\n'
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
      printf '[{"labels":{"slug":"%s"}}]\\n' "$slug"
    fi
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
    ;;
  cleanup)
    printf 'skip scaleway_server=none reason=missing labels\\n'
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
      ...validEnv,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_scaleway_smoke_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-scaleway-access|test-scaleway-secret/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider scaleway");
  assert.equal(seen[1], "list --provider scaleway --json");
  assert.match(seen[2], /^warmup --provider scaleway --slug scaleway-smoke-\d{14}-\d+ --keep --type DEV1-S --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider scaleway --id scaleway-smoke-\d{14}-\d+ --wait --wait-timeout 300s$/);
  assert.match(seen[4], /^run --provider scaleway --id scaleway-smoke-\d{14}-\d+ --no-sync -- echo ok$/);
  assert.equal(seen[5], "list --provider scaleway --json");
  assert.match(seen[6], /^stop --provider scaleway scaleway-smoke-\d{14}-\d+$/);
  assert.equal(seen[7], "cleanup --provider scaleway --dry-run");
  assert.equal(seen[8], "list --provider scaleway --json");
});

test("live scaleway smoke builds from the script root when cwd differs", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-scaleway-cwd-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$PWD:$*" >>"${calls}"
case "$1" in
  doctor|cleanup)
    printf 'auth=ready\\n'
    ;;
  warmup)
    printf '%s\\n' "$5" >"${slugFile}"
    ;;
  status|run|stop)
    printf 'ok\\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"}}]\\n' "$slug"
      touch "${slugFile}.stopped"
    fi
    ;;
  *)
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: dir,
    env: {
      ...process.env,
      ...validEnv,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_scaleway_smoke_passed/);
  const firstCall = fs.readFileSync(calls, "utf8").trim().split("\n")[0];
  const normalizeMacTmpPath = (value) => value.replace(/^\/private\/var\//, "/var/");
  assert.equal(
    normalizeMacTmpPath(firstCall),
    `${normalizeMacTmpPath(tempRoot)}:doctor --provider scaleway`,
  );
});

test("live scaleway smoke honors CRABBOX_BIN and CRABBOX_LIVE_REPO overrides", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-scaleway-bin-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const candidateDir = path.join(dir, "candidate");
  const candidateBin = path.join(candidateDir, "candidate-crabbox");
  const liveRepo = path.join(dir, "live-repo");
  const calls = path.join(dir, "candidate-calls.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(candidateDir, { recursive: true });
  fs.mkdirSync(liveRepo, { recursive: true });

  writeExecutable(
    candidateBin,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$PWD:$*" >>"${calls}"
case "$1" in
  doctor|cleanup)
    printf 'auth=ready\\n'
    ;;
  warmup)
    printf '%s\\n' "$5" >"${slugFile}"
    ;;
  status|run|stop)
    printf 'ok\\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"}}]\\n' "$slug"
      touch "${slugFile}.stopped"
    fi
    ;;
  *)
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: dir,
    env: {
      ...process.env,
      ...validEnv,
      CRABBOX_BIN: candidateBin,
      CRABBOX_LIVE_REPO: liveRepo,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_scaleway_smoke_passed/);
  const firstCall = fs.readFileSync(calls, "utf8").trim().split("\n")[0];
  const normalizeMacTmpPath = (value) => value.replace(/^\/private\/var\//, "/var/");
  assert.equal(
    normalizeMacTmpPath(firstCall),
    `${normalizeMacTmpPath(liveRepo)}:doctor --provider scaleway`,
  );
  assert.equal(fs.existsSync(path.join(tempRoot, "bin", "crabbox")), false);
});

test("live scaleway smoke attempts targeted cleanup after partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-scaleway-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(binDir, { recursive: true });

  writeExecutable(path.join(binDir, "sleep"), "#!/usr/bin/env bash\nexit 0\n");
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
  printf 'created scaleway instance before failing\\n' >&2
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
      ...validEnv,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(result.stderr, /created scaleway instance before failing/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^scaleway-smoke-\d{14}-\d+\n$/);
  assert.doesNotMatch(fs.readFileSync(calls, "utf8"), /^cleanup /m);
});

test("live scaleway smoke refuses non-empty initial inventory before mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-scaleway-nonempty-"));
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
    printf '[{"labels":{"slug":"existing"}}]\\n'
    ;;
  *)
    printf 'mutation must not run\\n' >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      ...validEnv,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /Scaleway Crabbox inventory is not empty/);
  assert.deepEqual(fs.readFileSync(calls, "utf8").trim().split("\n"), [
    "doctor --provider scaleway",
    "list --provider scaleway --json",
  ]);
});

test("live scaleway smoke classifies quota output and redacts leaked keys", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-scaleway-quota-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
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
    printf 'quota exceeded for key %s secret %s\\n' "\${SCW_ACCESS_KEY}" "\${SCW_SECRET_KEY}" >&2
    exit 42
    ;;
  stop)
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
      ...validEnv,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 42, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=quota_blocked/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-scaleway-access|test-scaleway-secret/);
  assert.match(result.stderr, /\[redacted\]/);
});

test("live scaleway smoke treats not-found cleanup after quota as nothing to clean", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-scaleway-quota-notfound-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(binDir, { recursive: true });
  writeExecutable(path.join(binDir, "sleep"), "#!/usr/bin/env bash\nexit 0\n");

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
    printf 'quota exceeded before server allocation\\n' >&2
    exit 42
    ;;
  stop)
    printf 'lease/scaleway server not found: %s\\n' "$4" >&2
    exit 4
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
      ...validEnv,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 42, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=quota_blocked/);
  assert.doesNotMatch(result.stderr, /classification=cleanup_failed/);
});

test("live scaleway smoke keeps cleanup armed until final inventory is empty", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-scaleway-final-list-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopAttempts = path.join(dir, "stop-attempts.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });

  writeExecutable(path.join(binDir, "sleep"), "#!/usr/bin/env bash\nexit 0\n");
  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  doctor)
    printf 'auth=ready\\n'
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
    if [[ -z "$slug" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"}}]\\n' "$slug"
    fi
    ;;
  stop)
    printf '%s\\n' "$4" >>"${stopAttempts}"
    ;;
  cleanup)
    printf 'skip scaleway_server=none reason=missing labels\\n'
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
      ...validEnv,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.equal(fs.readFileSync(stopAttempts, "utf8").trim().split("\n").length, 2);
});
