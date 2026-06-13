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

function writeRawInventoryPythonStub(binDir, rawStatus) {
  const python = spawnSync("sh", ["-c", "command -v python3"], { encoding: "utf8" }).stdout.trim();
  assert.ok(python, "python3 is required for smoke tests");
  writeExecutable(
    path.join(binDir, "python3"),
    `#!/usr/bin/env bash
if [[ "$*" == *"urllib.request"* ]]; then
  exit ${rawStatus}
fi
exec ${JSON.stringify(python)} "$@"
`,
  );
}

function prepareSmokeRepo(dir) {
  const tempRoot = path.join(dir, "repo");
  const tempScripts = path.join(tempRoot, "scripts");
  const smokeScript = path.join(tempScripts, "live-linode-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-linode-smoke.sh"), smokeScript);
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

test("live linode smoke skips unless opted in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-linode-skip-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: { ...process.env, CRABBOX_LIVE: "", LINODE_TOKEN: "" },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
});

test("live linode smoke skips unless provider filter selects linode", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-linode-filter-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "aws,digitalocean",
      LINODE_TOKEN: "test-secret-token",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=linode_not_selected/);
});

test("live linode smoke requires token before building", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-linode-token-"));
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
      CRABBOX_LIVE_PROVIDERS: "linode",
      LINODE_TOKEN: "",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=LINODE_TOKEN_missing/);
});

test("live linode smoke runs guarded lifecycle and redacts token", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-linode-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });
  writeRawInventoryPythonStub(binDir, 1);

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
if [[ "\${LINODE_TOKEN:-}" != "test-secret-token" ]]; then
  printf 'missing token\\n' >&2
  exit 91
fi
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready api=list mutation=false leases=0 runtime=unchecked default_type=g6-standard-1 region=us-ord image=linode/ubuntu24.04\\n'
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
      CRABBOX_LIVE_PROVIDERS: "linode",
      LINODE_TOKEN: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_linode_smoke_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-secret-token/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider linode");
  assert.equal(seen[1], "list --provider linode --json");
  assert.match(seen[2], /^warmup --provider linode --slug linode-smoke-\d{14}-\d+ --keep --type g6-standard-1 --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider linode --id linode-smoke-\d{14}-\d+ --wait --wait-timeout 300s$/);
  assert.match(seen[4], /^run --provider linode --id linode-smoke-\d{14}-\d+ --no-sync -- echo ok$/);
  assert.equal(seen[5], "list --provider linode --json");
  assert.match(seen[6], /^stop --provider linode linode-smoke-\d{14}-\d+$/);
  assert.equal(seen[7], "cleanup --provider linode --dry-run");
  assert.equal(seen[8], "list --provider linode --json");
});

test("live linode smoke attempts cleanup after partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-linode-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeRawInventoryPythonStub(binDir, 1);

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
  printf 'created linode before failing\\n' >&2
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
      CRABBOX_LIVE_PROVIDERS: "linode",
      LINODE_TOKEN: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(result.stderr, /created linode before failing/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^linode-smoke-\d{14}-\d+\n$/);
  assert.doesNotMatch(fs.readFileSync(calls, "utf8"), /^cleanup /m);
});

test("live linode smoke retries cleanup through ambiguous-create recovery", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-linode-recovery-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopAttempts = path.join(dir, "stop-attempts.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeRawInventoryPythonStub(binDir, 0);
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
    printf 'indeterminate create\\n' >&2
    exit 37
    ;;
  stop)
    printf 'attempt\\n' >>"${stopAttempts}"
    attempts="$(wc -l <"${stopAttempts}" | tr -d ' ')"
    if [[ "$attempts" -lt 4 ]]; then
      printf 'lease/linode not found: %s\\n' "$4" >&2
      exit 4
    fi
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
      CRABBOX_LIVE_PROVIDERS: "linode",
      LINODE_TOKEN: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.equal(fs.readFileSync(stopAttempts, "utf8"), "attempt\nattempt\nattempt\nattempt\n");
  assert.doesNotMatch(result.stderr, /classification=cleanup_failed/);
});

test("live linode smoke refuses a non-empty inventory before mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-linode-nonempty-"));
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
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "linode",
      LINODE_TOKEN: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /inventory is not empty/);
  assert.deepEqual(fs.readFileSync(calls, "utf8").trim().split("\n"), [
    "doctor --provider linode",
    "list --provider linode --json",
  ]);
});
