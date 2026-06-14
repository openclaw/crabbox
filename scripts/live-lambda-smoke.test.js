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
if [[ "$*" == *"urllib.request"* && "$*" == *"/ssh-keys"* ]]; then
  printf '[]\\n'
  exit 0
fi
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
  const smokeScript = path.join(tempScripts, "live-lambda-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-lambda-smoke.sh"), smokeScript);
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

test("live lambda smoke skips unless opted in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-lambda-skip-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: { ...process.env, CRABBOX_LIVE: "", LAMBDA_API_KEY: "" },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
});

test("live lambda smoke skips unless provider filter selects lambda", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-lambda-filter-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "aws,digitalocean",
      LAMBDA_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=lambda_not_selected/);
});

test("live lambda smoke requires token before building", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-lambda-token-"));
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
      LAMBDA_API_KEY: "",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=LAMBDA_API_KEY_missing/);
});

test("live lambda smoke runs guarded lifecycle and redacts token", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-lambda-"));
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
if [[ "\${LAMBDA_API_KEY:-}" != "test-secret-token" ]]; then
  printf 'missing token\\n' >&2
  exit 91
fi
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready api=list mutation=false leases=0 runtime=unchecked default_type=gpu_1x_a10 region=us-west-1 image_family=lambda-stack-24-04 api_key=test-secret-token jupyter_url=https://example.test/?token=abc\\n'
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
    printf 'skip instance id=none name=none reason=missing labels\\n'
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
      CRABBOX_LIVE_PROVIDERS: "lambda",
      LAMBDA_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_lambda_smoke_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-secret-token|token=abc/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider lambda");
  assert.equal(seen[1], "list --provider lambda --json");
  assert.match(seen[2], /^warmup --provider lambda --slug lambda-smoke-\d{14}-\d+ --keep --type gpu_1x_a10 --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider lambda --id lambda-smoke-\d{14}-\d+ --wait --wait-timeout 600s$/);
  assert.match(seen[4], /^run --provider lambda --id lambda-smoke-\d{14}-\d+ --no-sync -- echo ok$/);
  assert.equal(seen[5], "list --provider lambda --json");
  assert.match(seen[6], /^stop --provider lambda lambda-smoke-\d{14}-\d+$/);
  assert.equal(seen[7], "cleanup --provider lambda --dry-run");
  assert.equal(seen[8], "list --provider lambda --json");
});

test("live lambda smoke attempts cleanup after partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-lambda-fail-"));
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
  printf 'instance-operations/launch/insufficient-capacity after partial create\\n' >&2
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
      CRABBOX_LIVE_PROVIDERS: "lambda",
      LAMBDA_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=capacity_blocked/);
  assert.match(result.stderr, /insufficient-capacity after partial create/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^lambda-smoke-\d{14}-\d+\n$/);
  assert.doesNotMatch(fs.readFileSync(calls, "utf8"), /^cleanup /m);
});

test("live lambda smoke treats post-create lifecycle failure as validation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-lambda-status-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });
  writeRawInventoryPythonStub(binDir, 1);

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
      CRABBOX_LIVE_PROVIDERS: "lambda",
      LAMBDA_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 42, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /status never became ready/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^lambda-smoke-\d{14}-\d+\n$/);
});

test("live lambda smoke retries cleanup through ambiguous-create recovery", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-lambda-recovery-"));
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
    printf 'transport closed during launch\\n' >&2
    exit 37
    ;;
  stop)
    printf 'attempt\\n' >>"${stopAttempts}"
    attempts="$(wc -l <"${stopAttempts}" | tr -d ' ')"
    if [[ "$attempts" -lt 4 ]]; then
      printf 'lease/lambda instance not found: %s\\n' "$4" >&2
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
      CRABBOX_LIVE_PROVIDERS: "lambda",
      LAMBDA_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.equal(fs.readFileSync(stopAttempts, "utf8"), "attempt\nattempt\nattempt\nattempt\n");
  assert.doesNotMatch(result.stderr, /classification=cleanup_failed/);
});

test("live lambda smoke keeps cleanup armed until final inventory is empty", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-lambda-post-list-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");
  const stopAttempts = path.join(dir, "stop-attempts.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeRawInventoryPythonStub(binDir, 1);

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
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
    elif [[ -f "${slugFile}.post-list-seen" ]]; then
      printf '[]\\n'
    elif [[ -f "${slugFile}.stopped" ]]; then
      printf '[{"labels":{"slug":"%s"}}]\\n' "$slug"
      touch "${slugFile}.post-list-seen"
    else
      printf '[{"labels":{"slug":"%s"}}]\\n' "$slug"
    fi
    ;;
  stop)
    printf 'attempt\\n' >>"${stopAttempts}"
    touch "${slugFile}.stopped"
    ;;
  cleanup)
    printf 'cleanup dry run\\n'
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
      CRABBOX_LIVE_PROVIDERS: "lambda",
      LAMBDA_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.equal(fs.readFileSync(stopAttempts, "utf8"), "attempt\nattempt\n");
});

test("live lambda smoke fails ambiguous cleanup when managed keys remain", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-lambda-key-leak-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const keySnapshots = path.join(dir, "key-snapshots.log");
  fs.mkdirSync(binDir, { recursive: true });
  const python = spawnSync("sh", ["-c", "command -v python3"], { encoding: "utf8" }).stdout.trim();
  assert.ok(python, "python3 is required for smoke tests");
  writeExecutable(
    path.join(binDir, "python3"),
    `#!/usr/bin/env bash
if [[ "$*" == *"urllib.request"* && "$*" == *"/ssh-keys"* ]]; then
  printf 'snapshot\\n' >>${JSON.stringify(keySnapshots)}
  count="$(wc -l <${JSON.stringify(keySnapshots)} | tr -d ' ')"
  if [[ "$count" -eq 1 ]]; then
    printf '[]\\n'
  else
    printf '[{"id":"key-leaked","name":"crabbox-cbx-leaked","public_key":"ssh-ed25519 AAAA"}]\\n'
  fi
  exit 0
fi
if [[ "$*" == *"urllib.request"* ]]; then
  exit 1
fi
exec ${JSON.stringify(python)} "$@"
`,
  );
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
    printf 'transport closed during launch\\n' >&2
    exit 37
    ;;
  stop)
    printf 'lease/lambda instance not found: %s\\n' "$4" >&2
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
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "lambda",
      LAMBDA_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=cleanup_failed/);
  assert.equal(fs.readFileSync(keySnapshots, "utf8").split("\n").filter(Boolean).length, 66);
});

test("live lambda smoke refuses a non-empty inventory before mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-lambda-nonempty-"));
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
      CRABBOX_LIVE_PROVIDERS: "lambda",
      LAMBDA_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /inventory is not empty/);
  assert.deepEqual(fs.readFileSync(calls, "utf8").trim().split("\n"), [
    "doctor --provider lambda",
    "list --provider lambda --json",
  ]);
});

test("live lambda smoke classifies billing and quota blockers", () => {
  for (const [name, stderr, classification] of [
    ["billing", "global/account-inactive invalid billing", "billing_blocked"],
    ["quota", "global/quota-exceeded account limit", "quota_blocked"],
  ]) {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), `crabbox-live-lambda-${name}-`));
    const binDir = path.join(dir, "bin");
    const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
    fs.mkdirSync(binDir, { recursive: true });
    writeGoStub(
      binDir,
      `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "doctor" ]]; then
  printf '%s\\n' ${JSON.stringify(stderr)} >&2
  exit 12
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
        CRABBOX_LIVE_PROVIDERS: "lambda",
        LAMBDA_API_KEY: "test-secret-token",
      },
      encoding: "utf8",
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stderr, new RegExp(`classification=${classification}`));
  }
});
