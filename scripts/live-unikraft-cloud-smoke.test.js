import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawn, spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const smokeScript = path.join(repoRoot, "scripts", "live-unikraft-cloud-smoke.sh");
const bashPath = process.platform === "darwin" ? "/bin/bash" : "bash";
const existingUUID = "11111111-2222-3333-4444-555555555555";
const createdUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const createdLease = "ukc_a1b2c3d4e5f6";
const createdName = "crabbox-ukc-a1b2c3d4e5f6";
const temporaryDirectories = new Set();

test.afterEach(() => {
  for (const dir of temporaryDirectories) {
    fs.rmSync(dir, { recursive: true, force: true });
  }
  temporaryDirectories.clear();
});

function temporaryDirectory(prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  temporaryDirectories.add(dir);
  return dir;
}

async function waitForFile(file, timeoutMs = 5_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (fs.existsSync(file)) return;
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  throw new Error(`timed out waiting for ${path.basename(file)}`);
}

function writeExecutable(file, body) {
  fs.writeFileSync(file, body, "utf8");
  fs.chmodSync(file, 0o755);
}

function embeddedRawHelper() {
  const source = fs.readFileSync(smokeScript, "utf8");
  const match = source.match(/cat >"\$raw_helper" <<'PY'\n([\s\S]*?)\nPY\n  \); then/);
  assert.ok(match, "embedded raw helper not found");
  return match[1];
}

function baseEnv(overrides = {}) {
  return {
    ...process.env,
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "unikraft-cloud",
    CRABBOX_UNIKRAFT_CLOUD_API_KEY: "",
    UNIKRAFT_CLOUD_API_KEY: "",
    UKC_API_KEY: "",
    UKC_TOKEN: "smoke-secret-token",
    CRABBOX_UNIKRAFT_CLOUD_API_URL: "",
    UNIKRAFT_CLOUD_API_URL: "",
    CRABBOX_UNIKRAFT_CLOUD_METRO: "",
    UNIKRAFT_CLOUD_METRO: "",
    UKC_METRO: "",
    CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_ATTEMPTS: "2",
    CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_POLL_SECONDS: "0",
    ...overrides,
  };
}

function prepareHarness(name, crabboxBody) {
  const dir = temporaryDirectory(name);
  const proofDir = path.join(dir, "proof");
  const fakeCrabbox = path.join(dir, "crabbox");
  const rawHelper = path.join(dir, "raw-helper");
  const calls = path.join(dir, "crabbox-calls.log");
  const rawCalls = path.join(dir, "raw-calls.log");
  const remote = path.join(dir, "remote.json");
  const slugFile = path.join(dir, "slug");
  const inventoryCounter = path.join(dir, "inventory-counter");
  const deleteAttempts = path.join(dir, "delete-attempts");
  const baseline = `[{"name":"existing-service","uuid":"${existingUUID}"}]\n`;
  fs.mkdirSync(proofDir, { mode: 0o700 });
  fs.writeFileSync(remote, baseline);
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>${JSON.stringify(calls)}
${crabboxBody}
`,
  );
  writeExecutable(
    rawHelper,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>${JSON.stringify(rawCalls)}
if [[ "$*" == *"smoke-secret-token"* ]]; then
  printf 'token reached argv\n' >&2
  exit 90
fi
case "$1" in
  inventory)
    count=0
    if [[ -f ${JSON.stringify(inventoryCounter)} ]]; then
      count="$(cat ${JSON.stringify(inventoryCounter)})"
    fi
    count=$((count + 1))
    printf '%s' "$count" >${JSON.stringify(inventoryCounter)}
    if [[ -n "\${FAKE_DELAY_VISIBILITY_AT:-}" && "$count" -eq "$FAKE_DELAY_VISIBILITY_AT" ]]; then
      printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"},{"name":"${createdName}","uuid":"${createdUUID}"}]\n' >${JSON.stringify(remote)}
    fi
    cp ${JSON.stringify(remote)} "$2"
    ;;
  compare)
    cmp -s "$2" "$3"
    ;;
  validate)
    if [[ -n "\${FAKE_RAW_VALIDATE_SLEEP:-}" ]]; then
      sleep "$FAKE_RAW_VALIDATE_SLEEP"
    fi
    if [[ -n "\${FAKE_RAW_NOISY_PID_FILE:-}" ]]; then
      yes noisy-output &
      printf '%s' "$!" >"$FAKE_RAW_NOISY_PID_FILE"
      wait
    fi
    ;;
  claim)
    printf '%s' '${createdLease}' >"$4"
    printf '%s' "\${FAKE_CLAIM_UUID-${createdUUID}}" >"$5"
    printf '%s' '${createdName}' >"$6"
    ;;
  owned)
    if [[ -n "\${FAKE_RAW_OWNED_STARTED:-}" && ! -e "$FAKE_RAW_OWNED_STARTED" ]]; then
      : >"$FAKE_RAW_OWNED_STARTED"
      sleep 20
    fi
    if grep -q '${createdUUID}' "$3"; then
      printf '%s\n' '${createdUUID}'
    else
      exit 1
    fi
    ;;
  delete)
    [[ "$2" == '${createdUUID}' && "$3" == '${createdName}' ]]
    attempts=0
    if [[ -f ${JSON.stringify(deleteAttempts)} ]]; then
      attempts="$(cat ${JSON.stringify(deleteAttempts)})"
    fi
    attempts=$((attempts + 1))
    printf '%s' "$attempts" >${JSON.stringify(deleteAttempts)}
    if [[ "$attempts" -le "\${FAKE_DELETE_FAILURES:-0}" ]]; then
      printf 'transient delete failure\n' >&2
      exit 75
    fi
    printf '%s' '${baseline}' >${JSON.stringify(remote)}
    ;;
  absent)
    if grep -q "$2" ${JSON.stringify(remote)}; then
      exit 1
    fi
    ;;
  absent-name)
    if grep -q "$2" ${JSON.stringify(remote)}; then
      exit 1
    fi
    ;;
  *)
    printf 'unexpected raw helper args: %s\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );
  return {
    dir,
    proofDir,
    fakeCrabbox,
    rawHelper,
    calls,
    rawCalls,
    remote,
    slugFile,
    inventoryCounter,
    deleteAttempts,
    baseline,
  };
}

test("Unikraft Cloud smoke is non-mutating without live opt-in", () => {
  const dir = temporaryDirectory("crabbox-ukc-disabled-");
  const fakeCrabbox = path.join(dir, "crabbox");
  const calls = path.join(dir, "calls.log");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash\nprintf called >>${JSON.stringify(calls)}\nexit 99\n`,
  );

  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({ CRABBOX_BIN: fakeCrabbox, CRABBOX_LIVE: "0" }),
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
  assert.equal(fs.existsSync(calls), false);
});

test("Unikraft Cloud smoke requires explicit provider selection", () => {
  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({ CRABBOX_LIVE_PROVIDERS: "aws, linode" }),
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=provider_not_selected/);
});

test("Unikraft Cloud smoke accepts the registered unikraftcloud alias", () => {
  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_LIVE_PROVIDERS: "unikraftcloud",
      CRABBOX_UNIKRAFT_CLOUD_API_KEY: "",
      UNIKRAFT_CLOUD_API_KEY: "",
      UKC_API_KEY: "",
      UKC_TOKEN: "",
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(
    result.stdout,
    /classification=environment_blocked reason=unikraft_cloud_token_missing/,
  );
  assert.doesNotMatch(result.stdout, /provider_not_selected/);
});

test("Unikraft Cloud smoke checks credentials and supplied binary before API access", () => {
  const missingToken = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: "/definitely/missing/crabbox",
      CRABBOX_UNIKRAFT_CLOUD_API_KEY: "",
      UNIKRAFT_CLOUD_API_KEY: "",
      UKC_API_KEY: "",
      UKC_TOKEN: "",
    }),
    encoding: "utf8",
  });
  assert.equal(missingToken.status, 0, missingToken.stdout + missingToken.stderr);
  assert.match(
    missingToken.stdout,
    /classification=environment_blocked reason=unikraft_cloud_token_missing/,
  );

  const missingBinary = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({ CRABBOX_BIN: "/definitely/missing/crabbox" }),
    encoding: "utf8",
  });
  assert.equal(missingBinary.status, 0, missingBinary.stdout + missingBinary.stderr);
  assert.match(
    missingBinary.stdout,
    /classification=environment_blocked reason=crabbox_binary_missing_or_not_executable/,
  );
});

test("raw helper calls outside cleanup have a wall-clock deadline", () => {
  const harness = prepareHarness("crabbox-ukc-raw-deadline-", "exit 99");
  const startedAt = Date.now();
  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
      CRABBOX_UNIKRAFT_CLOUD_SMOKE_HTTP_TIMEOUT: "1",
      FAKE_RAW_VALIDATE_SLEEP: "20",
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(
    result.stdout,
    /classification=environment_blocked reason=invalid_unikraft_cloud_api_url/,
  );
  assert.ok(Date.now() - startedAt < 4_000, result.stdout + result.stderr);
  assert.equal(fs.existsSync(harness.calls), false);
});

test("command capture is byte-bounded and kills noisy descendants", () => {
  const harness = prepareHarness("crabbox-ukc-capture-limit-", "exit 99");
  const noisyPIDFile = path.join(harness.dir, "noisy-pid");
  const startedAt = Date.now();
  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
      FAKE_RAW_NOISY_PID_FILE: noisyPIDFile,
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(
    result.stdout,
    /classification=environment_blocked reason=invalid_unikraft_cloud_api_url/,
  );
  assert.ok(Date.now() - startedAt < 4_000, result.stdout + result.stderr);
  const noisyPID = Number(fs.readFileSync(noisyPIDFile, "utf8"));
  assert.throws(() => process.kill(noisyPID, 0), { code: "ESRCH" });
  const residue = fs
    .readdirSync(harness.proofDir, { recursive: true })
    .map(String)
    .filter((file) => file.endsWith(".capture") || file.endsWith(".overflow"));
  assert.deepEqual(residue, []);
});

test("Unikraft Cloud smoke serializes hostile config values without YAML injection", () => {
  const hostileImage = 'registry.example/image:tag"\nprovider: aws\nvalue: [broken';
  const harness = prepareHarness(
    "crabbox-ukc-config-",
    `case "$1" in
  doctor)
    python3 - "$CRABBOX_CONFIG" <<'PY'
import json
import os
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    value = json.load(handle)
assert value["provider"] == "unikraft-cloud"
assert value["unikraftCloud"]["image"] == os.environ["EXPECTED_HOSTILE_IMAGE"]
PY
    printf '%s' "$CRABBOX_CONFIG" >${JSON.stringify("$CONFIG_VALIDATED")}
    printf 'API key was rejected\n' >&2
    exit 3
    ;;
  *) exit 99 ;;
esac`,
  );
  const validated = path.join(harness.dir, "config-validated");
  const parentSentinel = path.join(harness.proofDir, "baseline-inventory.json");
  fs.writeFileSync(parentSentinel, "parent sentinel\n");
  const crabbox = fs
    .readFileSync(harness.fakeCrabbox, "utf8")
    .replaceAll('"$CONFIG_VALIDATED"', JSON.stringify(validated));
  fs.writeFileSync(harness.fakeCrabbox, crabbox);

  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_IMAGE: hostileImage,
      EXPECTED_HOSTILE_IMAGE: hostileImage,
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(
    result.stdout,
    /classification=environment_blocked reason=preflight-doctor_failed_exit_3/,
  );
  assert.equal(fs.existsSync(validated), true);
  const configPath = fs.readFileSync(validated, "utf8");
  assert.ok(path.dirname(configPath).startsWith(`${harness.proofDir}/run.`));
  assert.equal(fs.readFileSync(parentSentinel, "utf8"), "parent sentinel\n");
  assert.doesNotMatch(result.stdout + result.stderr, /provider: aws/);
});

test("proof log collisions fail closed before mutation", () => {
  const harness = prepareHarness(
    "crabbox-ukc-proof-collision-",
    `case "$1" in
  doctor)
    : >"$(dirname "$CRABBOX_CONFIG")/preflight-doctor.redacted.log"
    printf 'ready\n'
    ;;
  *)
    printf 'unexpected command after proof failure\n' >&2
    exit 99
    ;;
esac`,
  );

  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /preflight-doctor_failed_exit_125/);
  assert.doesNotMatch(fs.readFileSync(harness.calls, "utf8"), /^(list|warmup) /m);
});

test("Unikraft Cloud smoke preserves a nonempty baseline through the exact lifecycle", () => {
  const harness = prepareHarness(
    "crabbox-ukc-success-",
    `case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready\n'
    ;;
  warmup)
    printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"},{"name":"${createdName}","uuid":"${createdUUID}"}]\n' >${JSON.stringify("$FAKE_REMOTE")}
    while [[ "$#" -gt 0 ]]; do
      if [[ "$1" == "--slug" ]]; then
        printf '%s' "$2" >${JSON.stringify("$FAKE_SLUG")}
        shift 2
        continue
      fi
      shift
    done
    printf 'leased ${createdLease} slug=%s provider=unikraft-cloud instance=${createdUUID} state=running fqdn=example.invalid\n' "$(cat ${JSON.stringify("$FAKE_SLUG")})"
    ;;
  status)
    if [[ -f ${JSON.stringify("$FAKE_STOPPED")} ]]; then
      printf 'instance not found\n' >&2
      exit 4
    fi
    printf '{"id":"${createdLease}","slug":"%s","provider":"unikraft-cloud","serverId":"${createdUUID}","state":"running","ready":true}\n' "$(cat ${JSON.stringify("$FAKE_SLUG")})"
    ;;
  list)
    if ! grep -q '${createdUUID}' ${JSON.stringify("$FAKE_REMOTE")}; then
      printf '[]\n'
    else
      printf '[{"CloudID":"${createdUUID}","Provider":"unikraft-cloud","name":"${createdName}","labels":{"lease":"${createdLease}","slug":"%s"}}]\n' "$(cat ${JSON.stringify("$FAKE_SLUG")})"
    fi
    ;;
  stop)
    printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"}]\n' >${JSON.stringify("$FAKE_REMOTE")}
    : >${JSON.stringify("$FAKE_STOPPED")}
    ;;
  *) exit 99 ;;
esac`,
  );
  const stopped = path.join(harness.dir, "stopped");
  // Substitute harness paths after constructing the readable fake body.
  let crabbox = fs.readFileSync(harness.fakeCrabbox, "utf8");
  crabbox = crabbox
    .replaceAll('"$FAKE_REMOTE"', JSON.stringify(harness.remote))
    .replaceAll('"$FAKE_STOPPED"', JSON.stringify(stopped))
    .replaceAll('"$FAKE_SLUG"', JSON.stringify(harness.slugFile));
  fs.writeFileSync(harness.fakeCrabbox, crabbox);

  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_SLUG: "unikraft-cloud-live-smoke-test",
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_unikraft_cloud_smoke_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /smoke-secret-token/);
  assert.equal(fs.readFileSync(harness.remote, "utf8"), harness.baseline);
  const generatedSlug = fs.readFileSync(harness.slugFile, "utf8");
  assert.ok(generatedSlug.length <= 41, generatedSlug);
  assert.match(generatedSlug, /^[a-z0-9]+(?:-[a-z0-9]+)*$/);
  const calls = fs.readFileSync(harness.calls, "utf8");
  assert.match(calls, /^doctor --provider unikraft-cloud$/m);
  assert.match(calls, /^list --provider unikraft-cloud --all --json$/m);
  assert.match(calls, /^warmup --provider unikraft-cloud --slug .* --keep$/m);
  assert.match(
    calls,
    new RegExp(
      `^status --provider unikraft-cloud --id ${createdLease} --wait --wait-timeout 300s --json$`,
      "m",
    ),
  );
  assert.match(calls, new RegExp(`^stop --provider unikraft-cloud ${createdLease}$`, "m"));
  assert.match(
    calls,
    new RegExp(`^status --provider unikraft-cloud --id ${createdUUID} --json$`, "m"),
  );
  assert.doesNotMatch(fs.readFileSync(harness.rawCalls, "utf8"), /^delete /m);
});

test("deleted status rejects unrelated command failures", () => {
  const harness = prepareHarness(
    "crabbox-ukc-deleted-status-",
    `case "$1" in
  doctor) printf 'ready\n' ;;
  warmup)
    printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"},{"name":"${createdName}","uuid":"${createdUUID}"}]\n' >${JSON.stringify("$FAKE_REMOTE")}
    while [[ "$#" -gt 0 ]]; do
      if [[ "$1" == "--slug" ]]; then
        printf '%s' "$2" >${JSON.stringify("$FAKE_SLUG")}
        shift 2
        continue
      fi
      shift
    done
    printf 'leased ${createdLease} slug=%s provider=unikraft-cloud instance=${createdUUID} state=running\n' "$(cat ${JSON.stringify("$FAKE_SLUG")})"
    ;;
  status)
    if [[ -f ${JSON.stringify("$FAKE_STOPPED")} ]]; then
      printf 'unauthorized\n' >&2
      exit 3
    fi
    printf '{"id":"${createdLease}","slug":"%s","provider":"unikraft-cloud","serverId":"${createdUUID}","state":"running","ready":true}\n' "$(cat ${JSON.stringify("$FAKE_SLUG")})"
    ;;
  list)
    if grep -q '${createdUUID}' ${JSON.stringify("$FAKE_REMOTE")}; then
      printf '[{"CloudID":"${createdUUID}","Provider":"unikraft-cloud","name":"${createdName}","labels":{"lease":"${createdLease}","slug":"%s"}}]\n' "$(cat ${JSON.stringify("$FAKE_SLUG")})"
    else
      printf '[]\n'
    fi
    ;;
  stop)
    printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"}]\n' >${JSON.stringify("$FAKE_REMOTE")}
    : >${JSON.stringify("$FAKE_STOPPED")}
    ;;
  *) exit 99 ;;
esac`,
  );
  const stopped = path.join(harness.dir, "stopped");
  const crabbox = fs
    .readFileSync(harness.fakeCrabbox, "utf8")
    .replaceAll('"$FAKE_REMOTE"', JSON.stringify(harness.remote))
    .replaceAll('"$FAKE_STOPPED"', JSON.stringify(stopped))
    .replaceAll('"$FAKE_SLUG"', JSON.stringify(harness.slugFile));
  fs.writeFileSync(harness.fakeCrabbox, crabbox);

  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_SLUG: "unikraft-cloud-live-smoke-test",
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(
    result.stdout,
    /classification=validation_failed reason=deleted_status_did_not_prove_not_found/,
  );
  assert.equal(fs.readFileSync(harness.remote, "utf8"), harness.baseline);
});

test("lifecycle JSON proof requires UUID and slug on the same record", () => {
  const harness = prepareHarness(
    "crabbox-ukc-correlated-json-",
    `case "$1" in
  doctor) printf 'ready\n' ;;
  warmup)
    printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"},{"name":"${createdName}","uuid":"${createdUUID}"}]\n' >${JSON.stringify("$FAKE_REMOTE")}
    while [[ "$#" -gt 0 ]]; do
      if [[ "$1" == "--slug" ]]; then
        printf '%s' "$2" >${JSON.stringify("$FAKE_SLUG")}
        shift 2
        continue
      fi
      shift
    done
    printf 'leased ${createdLease} slug=%s provider=unikraft-cloud instance=${createdUUID} state=running\n' "$(cat ${JSON.stringify("$FAKE_SLUG")})"
    ;;
  status)
    printf '{"id":"${createdLease}","slug":"%s","provider":"unikraft-cloud","serverId":"${createdUUID}","ready":true}\n' "$(cat ${JSON.stringify("$FAKE_SLUG")})"
    ;;
  list)
    if grep -q '${createdUUID}' ${JSON.stringify("$FAKE_REMOTE")}; then
      printf '[{"CloudID":"${createdUUID}","Provider":"unikraft-cloud","labels":{"lease":"${createdLease}","slug":"wrong-record"}},{"CloudID":"${existingUUID}","Provider":"unikraft-cloud","labels":{"lease":"other","slug":"%s"}}]\n' "$(cat ${JSON.stringify("$FAKE_SLUG")})"
    else
      printf '[]\n'
    fi
    ;;
  stop)
    printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"}]\n' >${JSON.stringify("$FAKE_REMOTE")}
    ;;
  *) exit 99 ;;
esac`,
  );
  let crabbox = fs.readFileSync(harness.fakeCrabbox, "utf8");
  crabbox = crabbox
    .replaceAll('"$FAKE_REMOTE"', JSON.stringify(harness.remote))
    .replaceAll('"$FAKE_SLUG"', JSON.stringify(harness.slugFile));
  fs.writeFileSync(harness.fakeCrabbox, crabbox);

  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(
    result.stdout,
    /classification=validation_failed reason=claimed_list_identity_missing/,
  );
  assert.equal(fs.readFileSync(harness.remote, "utf8"), harness.baseline);
  const allLists = fs
    .readFileSync(harness.calls, "utf8")
    .split("\n")
    .filter((line) => line === "list --provider unikraft-cloud --all --json");
  assert.equal(allLists.length, 1);
});

test("Unikraft Cloud smoke uses exact-name raw cleanup after ambiguous create failure", () => {
  const harness = prepareHarness(
    "crabbox-ukc-cleanup-",
    `case "$1" in
  doctor) printf 'ready\n' ;;
  list)
    printf '[{"CloudID":"${createdUUID}","Provider":"unikraft-cloud","name":"${createdName}","labels":{"lease":"${createdLease}","slug":"unikraft-cloud-live-smoke-test"}}]\n'
    ;;
  warmup)
    printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"},{"name":"${createdName}","uuid":"${createdUUID}"}]\n' >${JSON.stringify("$FAKE_REMOTE")}
    printf 'create timed out; recovery claim ${createdLease} retained\n' >&2
    exit 5
    ;;
  stop)
    printf 'delete confirmation unavailable\n' >&2
    exit 5
    ;;
  *) exit 99 ;;
esac`,
  );
  let crabbox = fs.readFileSync(harness.fakeCrabbox, "utf8");
  crabbox = crabbox.replaceAll('"$FAKE_REMOTE"', JSON.stringify(harness.remote));
  fs.writeFileSync(harness.fakeCrabbox, crabbox);

  const startedAt = Date.now();
  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_SLUG: "unikraft-cloud-live-smoke-test",
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_POLL_SECONDS: "999999999999",
      FAKE_DELETE_FAILURES: "1",
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=validation_failed reason=warmup_failed_exit_5/);
  assert.doesNotMatch(result.stderr, /classification=cleanup_failed/);
  assert.equal(fs.readFileSync(harness.remote, "utf8"), harness.baseline);
  assert.match(
    fs.readFileSync(harness.rawCalls, "utf8"),
    new RegExp(`^delete ${createdUUID} ${createdName}$`, "m"),
  );
  assert.equal(fs.readFileSync(harness.deleteAttempts, "utf8"), "2");
  assert.ok(Date.now() - startedAt < 12_000, result.stdout + result.stderr);
  assert.doesNotMatch(
    result.stdout + result.stderr + fs.readFileSync(harness.rawCalls, "utf8"),
    /smoke-secret-token/,
  );
});

test("ambiguous create cleanup waits for delayed provider visibility", () => {
  const attempted = temporaryDirectory("crabbox-ukc-delayed-marker-");
  const attemptedFile = path.join(attempted, "attempted");
  const harness = prepareHarness(
    "crabbox-ukc-delayed-",
    `case "$1" in
  doctor) printf 'ready\n' ;;
  list)
    if [[ -f ${JSON.stringify(attemptedFile)} ]]; then
      printf '[{"name":"${createdName}","labels":{"lease":"${createdLease}","slug":"unikraft-cloud-live-smoke-test"}}]\n'
    else
      printf '[]\n'
    fi
    ;;
  warmup)
    : >${JSON.stringify(attemptedFile)}
    printf 'create timed out; recovery claim ${createdLease} retained\n' >&2
    exit 5
    ;;
  stop)
    printf 'create outcome still pending\n' >&2
    exit 5
    ;;
  *) exit 99 ;;
esac`,
  );

  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_SLUG: "unikraft-cloud-live-smoke-test",
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_UNCERTAINTY_SECONDS: "5",
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_TIMEOUT_SECONDS: "10",
      FAKE_CLAIM_UUID: "",
      FAKE_DELAY_VISIBILITY_AT: "4",
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=validation_failed reason=warmup_failed_exit_5/);
  assert.doesNotMatch(result.stderr, /classification=cleanup_failed/);
  assert.equal(fs.readFileSync(harness.remote, "utf8"), harness.baseline);
  assert.ok(Number(fs.readFileSync(harness.inventoryCounter, "utf8")) >= 4);
  assert.equal(fs.readFileSync(harness.deleteAttempts, "utf8"), "1");
});

test("successful warmup cleanup waits for delayed provider visibility", () => {
  const attempted = temporaryDirectory("crabbox-ukc-success-delayed-marker-");
  const attemptedFile = path.join(attempted, "attempted");
  const harness = prepareHarness(
    "crabbox-ukc-success-delayed-",
    `case "$1" in
  doctor) printf 'ready\n' ;;
  list)
    if [[ -f ${JSON.stringify(attemptedFile)} ]]; then
      printf '[{"CloudID":"${createdUUID}","Provider":"unikraft-cloud","name":"${createdName}","labels":{"lease":"${createdLease}","slug":"unikraft-cloud-live-smoke-test"}}]\n'
    else
      printf '[]\n'
    fi
    ;;
  warmup)
    : >${JSON.stringify(attemptedFile)}
    printf 'leased ${createdLease} slug=unikraft-cloud-live-smoke-test provider=unikraft-cloud instance=${createdUUID} state=running\n'
    ;;
  stop)
    printf 'create outcome still pending\n' >&2
    exit 5
    ;;
  *) exit 99 ;;
esac`,
  );

  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_SLUG: "unikraft-cloud-live-smoke-test",
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_UNCERTAINTY_SECONDS: "5",
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_TIMEOUT_SECONDS: "10",
      FAKE_DELAY_VISIBILITY_AT: "5",
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(
    result.stdout,
    /classification=validation_failed reason=created_instance_raw_identity_mismatch/,
  );
  assert.doesNotMatch(result.stderr, /classification=cleanup_failed/);
  assert.equal(fs.readFileSync(harness.remote, "utf8"), harness.baseline);
  assert.ok(Number(fs.readFileSync(harness.inventoryCounter, "utf8")) >= 5);
  assert.equal(fs.readFileSync(harness.deleteAttempts, "utf8"), "1");
});

test("successful warmup never accepts a never-observed baseline as cleanup", () => {
  const attempted = temporaryDirectory("crabbox-ukc-success-unseen-marker-");
  const attemptedFile = path.join(attempted, "attempted");
  const harness = prepareHarness(
    "crabbox-ukc-success-unseen-",
    `case "$1" in
  doctor) printf 'ready\n' ;;
  list)
    if [[ -f ${JSON.stringify(attemptedFile)} ]]; then
      printf '[{"CloudID":"${createdUUID}","Provider":"unikraft-cloud","name":"${createdName}","labels":{"lease":"${createdLease}","slug":"unikraft-cloud-live-smoke-test"}}]\n'
    else
      printf '[]\n'
    fi
    ;;
  warmup)
    : >${JSON.stringify(attemptedFile)}
    printf 'leased ${createdLease} slug=unikraft-cloud-live-smoke-test provider=unikraft-cloud instance=${createdUUID} state=running\n'
    ;;
  stop)
    printf 'create outcome still pending\n' >&2
    exit 5
    ;;
  *) exit 99 ;;
esac`,
  );

  const startedAt = Date.now();
  const result = spawnSync(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_SLUG: "unikraft-cloud-live-smoke-test",
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_POLL_SECONDS: "0.1",
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_TIMEOUT_SECONDS: "10",
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(
    result.stdout,
    /classification=validation_failed reason=created_instance_raw_identity_mismatch/,
  );
  assert.match(result.stderr, /classification=cleanup_failed reason=exact_baseline_not_restored/);
  assert.ok(Date.now() - startedAt >= 8_000, result.stdout + result.stderr);
  assert.equal(fs.readFileSync(harness.remote, "utf8"), harness.baseline);
  assert.equal(fs.existsSync(harness.deleteAttempts), false);
});

test("TERM before cleanup arming redacts and removes the active capture", async () => {
  const harness = prepareHarness(
    "crabbox-ukc-preflight-signal-",
    `case "$1" in
  doctor)
    for ((i = 0; i < 5000; i++)); do
      printf '%s\n' "$UKC_TOKEN"
    done
    : >${JSON.stringify("$DOCTOR_STARTED")}
    sleep 20
    ;;
  *) exit 99 ;;
esac`,
  );
  const doctorStarted = path.join(harness.dir, "doctor-started");
  const crabbox = fs
    .readFileSync(harness.fakeCrabbox, "utf8")
    .replaceAll('"$DOCTOR_STARTED"', JSON.stringify(doctorStarted));
  fs.writeFileSync(harness.fakeCrabbox, crabbox);

  const child = spawn(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
    }),
    stdio: ["ignore", "pipe", "pipe"],
  });
  let output = "";
  child.stdout.on("data", (chunk) => {
    output += chunk;
  });
  child.stderr.on("data", (chunk) => {
    output += chunk;
  });
  await waitForFile(doctorStarted);
  child.kill("SIGTERM");
  const exit = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(new Error(`preflight did not exit after TERM\n${output}`));
    }, 7_000);
    child.once("exit", (code, signal) => {
      clearTimeout(timer);
      resolve({ code, signal });
    });
  });

  assert.deepEqual(exit, { code: 143, signal: null }, output);
  const runDir = fs
    .readdirSync(harness.proofDir, { withFileTypes: true })
    .find((entry) => entry.isDirectory() && entry.name.startsWith("run."));
  assert.ok(runDir, output);
  const proofRun = path.join(harness.proofDir, runDir.name);
  const interruptedLog = path.join(proofRun, "interrupted-command.redacted.log");
  assert.equal(fs.existsSync(interruptedLog), true, output);
  assert.match(fs.readFileSync(interruptedLog, "utf8"), /<redacted>/);
  const proofEntries = fs.readdirSync(proofRun, { recursive: true }).map(String);
  assert.deepEqual(
    proofEntries.filter((entry) => entry.endsWith(".capture") || entry.endsWith(".overflow")),
    [],
  );
  const proofText = proofEntries
    .map((entry) => path.join(proofRun, entry))
    .filter((entry) => fs.statSync(entry).isFile())
    .map((entry) => fs.readFileSync(entry, "utf8"))
    .join("\n");
  assert.doesNotMatch(proofText + output, /smoke-secret-token/);
});

test("TERM interrupts an active warmup and starts zero-residue cleanup promptly", async () => {
  const harness = prepareHarness(
    "crabbox-ukc-signal-",
    `case "$1" in
  doctor) printf 'ready\n' ;;
  list)
    if grep -q '${createdUUID}' ${JSON.stringify("$FAKE_REMOTE")}; then
      printf '[{"CloudID":"${createdUUID}","Provider":"unikraft-cloud","name":"${createdName}","labels":{"lease":"${createdLease}","slug":"unikraft-cloud-live-smoke-test"}}]\n'
    else
      printf '[]\n'
    fi
    ;;
  warmup)
    printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"},{"name":"${createdName}","uuid":"${createdUUID}"}]\n' >${JSON.stringify("$FAKE_REMOTE")}
    printf 'leased ${createdLease} slug=unikraft-cloud-live-smoke-test provider=unikraft-cloud instance=${createdUUID} state=running\n'
    : >${JSON.stringify("$WARMUP_STARTED")}
    (trap '' TERM; while :; do sleep 1; done) &
    printf '%s' "$!" >${JSON.stringify("$SLEEP_PID")}
    wait
    ;;
  stop)
    printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"}]\n' >${JSON.stringify("$FAKE_REMOTE")}
    : >${JSON.stringify("$CLEANUP_RAN")}
    ;;
  *) exit 99 ;;
esac`,
  );
  const warmupStarted = path.join(harness.dir, "warmup-started");
  const sleepPIDFile = path.join(harness.dir, "sleep-pid");
  const cleanupRan = path.join(harness.dir, "cleanup-ran");
  let crabbox = fs.readFileSync(harness.fakeCrabbox, "utf8");
  crabbox = crabbox
    .replaceAll('"$FAKE_REMOTE"', JSON.stringify(harness.remote))
    .replaceAll('"$WARMUP_STARTED"', JSON.stringify(warmupStarted))
    .replaceAll('"$SLEEP_PID"', JSON.stringify(sleepPIDFile))
    .replaceAll('"$CLEANUP_RAN"', JSON.stringify(cleanupRan));
  fs.writeFileSync(harness.fakeCrabbox, crabbox);

  const child = spawn(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_SLUG: "unikraft-cloud-live-smoke-test",
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_TIMEOUT_SECONDS: "10",
    }),
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk) => {
    stdout += chunk;
  });
  child.stderr.on("data", (chunk) => {
    stderr += chunk;
  });
  await waitForFile(warmupStarted);
  const interruptedAt = Date.now();
  child.kill("SIGTERM");
  const exit = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(new Error(`smoke did not exit after TERM\n${stdout}\n${stderr}`));
    }, 7_000);
    child.once("exit", (code, signal) => {
      clearTimeout(timer);
      resolve({ code, signal });
    });
  });

  assert.deepEqual(exit, { code: 143, signal: null }, stdout + stderr);
  assert.ok(Date.now() - interruptedAt < 7_000, stdout + stderr);
  assert.equal(fs.existsSync(cleanupRan), true, stdout + stderr);
  assert.equal(fs.readFileSync(harness.remote, "utf8"), harness.baseline);
  assert.doesNotMatch(stderr, /classification=cleanup_failed/);
  const sleepPID = Number(fs.readFileSync(sleepPIDFile, "utf8"));
  assert.throws(() => process.kill(sleepPID, 0), { code: "ESRCH" });
});

test("TERM interrupts owned-inventory proof without command-substitution deferral", async () => {
  const harness = prepareHarness(
    "crabbox-ukc-owned-signal-",
    `case "$1" in
  doctor) printf 'ready\n' ;;
  list)
    if grep -q '${createdUUID}' ${JSON.stringify("$FAKE_REMOTE")}; then
      printf '[{"CloudID":"${createdUUID}","Provider":"unikraft-cloud","labels":{"lease":"${createdLease}","slug":"unikraft-cloud-live-smoke-test"}}]\n'
    else
      printf '[]\n'
    fi
    ;;
  warmup)
    printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"},{"name":"${createdName}","uuid":"${createdUUID}"}]\n' >${JSON.stringify("$FAKE_REMOTE")}
    printf 'leased ${createdLease} slug=unikraft-cloud-live-smoke-test provider=unikraft-cloud instance=${createdUUID} state=running\n'
    ;;
  stop)
    printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"}]\n' >${JSON.stringify("$FAKE_REMOTE")}
    ;;
  *) exit 99 ;;
esac`,
  );
  const ownedStarted = path.join(harness.dir, "owned-started");
  const crabbox = fs
    .readFileSync(harness.fakeCrabbox, "utf8")
    .replaceAll('"$FAKE_REMOTE"', JSON.stringify(harness.remote));
  fs.writeFileSync(harness.fakeCrabbox, crabbox);

  const child = spawn(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_SLUG: "unikraft-cloud-live-smoke-test",
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_TIMEOUT_SECONDS: "10",
      FAKE_RAW_OWNED_STARTED: ownedStarted,
    }),
    stdio: ["ignore", "pipe", "pipe"],
  });
  let output = "";
  child.stdout.on("data", (chunk) => {
    output += chunk;
  });
  child.stderr.on("data", (chunk) => {
    output += chunk;
  });
  await waitForFile(ownedStarted);
  child.kill("SIGTERM");
  const exit = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(new Error(`owned proof did not interrupt promptly\n${output}`));
    }, 7_000);
    child.once("exit", (code, signal) => {
      clearTimeout(timer);
      resolve({ code, signal });
    });
  });

  assert.deepEqual(exit, { code: 143, signal: null }, output);
  assert.equal(fs.readFileSync(harness.remote, "utf8"), harness.baseline);
  assert.doesNotMatch(output, /classification=cleanup_failed/);
  const source = fs.readFileSync(smokeScript, "utf8");
  assert.doesNotMatch(source, /\$\([^\n]*find_owned_uuid/);
});

test("TERM during cleanup-stop continues bounded raw cleanup", async () => {
  const harness = prepareHarness(
    "crabbox-ukc-cleanup-signal-",
    `case "$1" in
  doctor) printf 'ready\n' ;;
  list)
    if grep -q '${createdUUID}' ${JSON.stringify("$FAKE_REMOTE")}; then
      printf '[{"CloudID":"${createdUUID}","Provider":"unikraft-cloud","labels":{"lease":"${createdLease}","slug":"unikraft-cloud-live-smoke-test"}}]\n'
    else
      printf '[]\n'
    fi
    ;;
  warmup)
    printf '%s' '[{"name":"existing-service","uuid":"${existingUUID}"},{"name":"${createdName}","uuid":"${createdUUID}"}]\n' >${JSON.stringify("$FAKE_REMOTE")}
    printf 'create timed out; recovery claim ${createdLease} retained\n' >&2
    exit 5
    ;;
  stop)
    : >${JSON.stringify("$CLEANUP_STOP_STARTED")}
    sleep 20
    ;;
  *) exit 99 ;;
esac`,
  );
  const cleanupStopStarted = path.join(harness.dir, "cleanup-stop-started");
  let crabbox = fs.readFileSync(harness.fakeCrabbox, "utf8");
  crabbox = crabbox
    .replaceAll('"$FAKE_REMOTE"', JSON.stringify(harness.remote))
    .replaceAll('"$CLEANUP_STOP_STARTED"', JSON.stringify(cleanupStopStarted));
  fs.writeFileSync(harness.fakeCrabbox, crabbox);

  const child = spawn(bashPath, [smokeScript], {
    cwd: repoRoot,
    env: baseEnv({
      CRABBOX_BIN: harness.fakeCrabbox,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR: harness.proofDir,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER: harness.rawHelper,
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_SLUG: "unikraft-cloud-live-smoke-test",
      CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_TIMEOUT_SECONDS: "10",
    }),
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk) => {
    stdout += chunk;
  });
  child.stderr.on("data", (chunk) => {
    stderr += chunk;
  });
  await waitForFile(cleanupStopStarted);
  child.kill("SIGTERM");
  const exit = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(new Error(`cleanup did not continue after TERM\n${stdout}\n${stderr}`));
    }, 7_000);
    child.once("exit", (code, signal) => {
      clearTimeout(timer);
      resolve({ code, signal });
    });
  });

  assert.deepEqual(exit, { code: 1, signal: null }, stdout + stderr);
  assert.match(stderr, /cleanup_interrupted_by_TERM_continuing/);
  assert.doesNotMatch(stderr, /classification=cleanup_failed/);
  assert.equal(fs.readFileSync(harness.remote, "utf8"), harness.baseline);
  assert.equal(fs.readFileSync(harness.deleteAttempts, "utf8"), "1");
});

test("raw cleanup helper sources authorization from environment and refuses redirects", () => {
  const source = fs.readFileSync(smokeScript, "utf8");
  assert.match(source, /"Authorization": "Bearer " \+ TOKEN/);
  assert.match(source, /class NoRedirect/);
  assert.match(source, /ProxyHandler\(\{\}\)/);
  assert.match(source, /request\("DELETE", "\/v1\/instances", \[\{"uuid": uuid\}\]\)/);
  assert.doesNotMatch(source, /"timeout_s"|"dont_retain"/);
  assert.match(source, /if \(delay > 5\) delay = 5/);
  assert.match(source, /if \(delay > remaining\) delay = remaining/);
  assert.doesNotMatch(source, /--(?:api-key|token)/);
});

test("main live-smoke dispatcher routes Unikraft Cloud aliases to the dedicated proof", () => {
  const source = fs.readFileSync(path.join(repoRoot, "scripts", "live-smoke.sh"), "utf8");
  assert.match(
    source,
    /if has_provider unikraft-cloud \|\| has_provider unikraftcloud \|\| has_provider ukc; then\n  "\$root\/scripts\/live-unikraft-cloud-smoke\.sh"/,
  );
});

test("generated raw cleanup helper is valid Python", () => {
  const dir = temporaryDirectory("crabbox-ukc-helper-syntax-");
  const helper = path.join(dir, "helper.py");
  fs.writeFileSync(helper, embeddedRawHelper());
  const result = spawnSync("python3", ["-m", "py_compile", helper], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stdout + result.stderr);
});

test("raw helper deletes only the exact new UUID and proves strong absence", async () => {
  const dir = temporaryDirectory("crabbox-ukc-helper-live-");
  const helper = path.join(dir, "helper.py");
  const serverFile = path.join(dir, "server.cjs");
  const stateFile = path.join(dir, "state.json");
  const requestLog = path.join(dir, "requests.log");
  const baselineFile = path.join(dir, "baseline.json");
  const currentFile = path.join(dir, "current.json");
  const finalFile = path.join(dir, "final.json");
  const badDeleteNameFile = path.join(dir, "bad-delete-name");
  const badAbsentIdentityFile = path.join(dir, "bad-absent-identity");
  const redirectInventoryFile = path.join(dir, "redirect-inventory");
  const redirectTargetFile = path.join(dir, "redirect-target-reached");
  fs.writeFileSync(helper, embeddedRawHelper());
  fs.chmodSync(helper, 0o755);
  fs.writeFileSync(
    baselineFile,
    JSON.stringify([{ name: "existing-service", uuid: existingUUID }]) + "\n",
  );
  fs.writeFileSync(
    stateFile,
    JSON.stringify([
      { name: "existing-service", uuid: existingUUID },
      { name: createdName, uuid: createdUUID },
    ]),
  );
  fs.writeFileSync(
    serverFile,
    `const fs = require("node:fs");
const http = require("node:http");
const stateFile = process.argv[2];
const requestLog = process.argv[3];
const badDeleteNameFile = process.argv[4];
const badAbsentIdentityFile = process.argv[5];
const redirectInventoryFile = process.argv[6];
const redirectTargetFile = process.argv[7];
const createdUUID = ${JSON.stringify(createdUUID)};
const createdName = ${JSON.stringify(createdName)};
let missingGetCount = 0;
function reply(response, value) {
  response.writeHead(200, { "content-type": "application/json" });
  response.end(JSON.stringify(value));
}
const server = http.createServer((request, response) => {
  if (request.headers.authorization !== "Bearer functional-secret") {
    response.writeHead(401, { "content-type": "application/json" });
    response.end(JSON.stringify({ status: "error", message: "unauthorized" }));
    return;
  }
  let body = "";
  request.on("data", (chunk) => { body += chunk; });
  request.on("end", () => {
    fs.appendFileSync(requestLog, request.method + " " + request.url + "\\n");
    let state = JSON.parse(fs.readFileSync(stateFile, "utf8"));
    if (request.method === "GET" && request.url === "/v1/instances") {
      if (fs.existsSync(redirectInventoryFile)) {
        response.writeHead(307, { "content-type": "application/json", location: "/redirect-target" });
        response.end(JSON.stringify({ status: "error", message: "redirect refused" }));
        return;
      }
      reply(response, { status: "success", data: { instances: state } });
      return;
    }
    if (request.method === "GET" && request.url === "/redirect-target") {
      fs.writeFileSync(redirectTargetFile, "reached");
      reply(response, { status: "success", data: { instances: state } });
      return;
    }
    if (request.method === "GET" && request.url === "/v1/instances/" + createdUUID) {
      const item = state.find((value) => value.uuid === createdUUID);
      if (item) {
        reply(response, { status: "success", data: { instances: [{ ...item, status: "success" }] } });
      } else if (fs.existsSync(badAbsentIdentityFile)) {
        reply(response, { status: "error", data: { instances: [{ uuid: "ffffffff-ffff-ffff-ffff-ffffffffffff", status: "error", error: 8 }] } });
      } else {
        missingGetCount += 1;
        if (missingGetCount === 1) {
          reply(response, { status: "success", data: { instances: [{ uuid: createdUUID, status: "error", error: 8 }] } });
        } else {
          response.writeHead(404, { "content-type": "application/json" });
          response.end(JSON.stringify({ status: "error", message: "instance not found" }));
        }
      }
      return;
    }
    if (request.method === "GET" && request.url === "/v1/instances/" + createdName) {
      if (fs.existsSync(badAbsentIdentityFile)) {
        reply(response, { status: "error", data: { instances: [{ uuid: "ffffffff-ffff-ffff-ffff-ffffffffffff", status: "error", error: 8 }] } });
      } else {
        response.writeHead(404, { "content-type": "application/json" });
        response.end(JSON.stringify({ status: "error", message: "instance not found" }));
      }
      return;
    }
    if (request.method === "DELETE" && request.url === "/v1/instances") {
      const parsed = JSON.parse(body);
      if (!Array.isArray(parsed) || parsed.length !== 1 || Object.keys(parsed[0]).length !== 1 || parsed[0].uuid !== createdUUID) {
        response.writeHead(400, { "content-type": "application/json" });
        response.end(JSON.stringify({ status: "error", message: "bad delete body" }));
        return;
      }
      if (fs.existsSync(badDeleteNameFile)) {
        reply(response, { status: "success", data: { instances: [{ uuid: createdUUID, name: "crabbox-ukc-ffffffffffff", status: "success" }] } });
        return;
      }
      state = state.filter((value) => value.uuid !== createdUUID);
      fs.writeFileSync(stateFile, JSON.stringify(state));
      reply(response, { status: "success", data: { instances: [{ uuid: createdUUID, name: createdName, status: "success" }] } });
      return;
    }
    response.writeHead(404, { "content-type": "application/json" });
    response.end(JSON.stringify({ status: "error", message: "unexpected request" }));
  });
});
server.listen(0, "127.0.0.1", () => process.stdout.write(String(server.address().port) + "\\n"));
process.on("SIGTERM", () => server.close(() => process.exit(0)));
`,
  );
  const server = spawn(
    process.execPath,
    [
      serverFile,
      stateFile,
      requestLog,
      badDeleteNameFile,
      badAbsentIdentityFile,
      redirectInventoryFile,
      redirectTargetFile,
    ],
    { stdio: ["ignore", "pipe", "pipe"] },
  );
  const port = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("fake API did not start")), 5_000);
    server.once("error", reject);
    server.stdout.once("data", (chunk) => {
      clearTimeout(timer);
      resolve(String(chunk).trim());
    });
  });
  const env = {
    ...process.env,
    CRABBOX_UNIKRAFT_CLOUD_SMOKE_TOKEN: "functional-secret",
    CRABBOX_UNIKRAFT_CLOUD_SMOKE_API_URL: `http://127.0.0.1:${port}`,
    HTTP_PROXY: "http://127.0.0.1:1",
    HTTPS_PROXY: "http://127.0.0.1:1",
    NO_PROXY: "",
    http_proxy: "http://127.0.0.1:1",
    https_proxy: "http://127.0.0.1:1",
    no_proxy: "",
  };
  const raw = (...args) => spawnSync(helper, args, { env, encoding: "utf8" });
  try {
    assert.equal(raw("validate").status, 0);
    fs.writeFileSync(redirectInventoryFile, "1");
    const redirectedInventory = raw("inventory", currentFile);
    assert.notEqual(redirectedInventory.status, 0);
    assert.equal(fs.existsSync(redirectTargetFile), false);
    fs.rmSync(redirectInventoryFile);
    const inventory = raw("inventory", currentFile);
    assert.equal(inventory.status, 0, inventory.stdout + inventory.stderr);
    const invalidDestination = path.join(dir, "inventory-destination-directory");
    fs.mkdirSync(invalidDestination);
    const failedInventory = raw("inventory", invalidDestination);
    assert.notEqual(failedInventory.status, 0);
    assert.deepEqual(
      fs.readdirSync(dir).filter((entry) => entry.startsWith(".inventory.")),
      [],
    );
    const claimView = path.join(dir, "claim-view.json");
    const claimLease = path.join(dir, "claim-lease");
    const claimUUID = path.join(dir, "claim-uuid");
    const claimName = path.join(dir, "claim-name");
    const zeroClaimView = path.join(dir, "zero-claim-view.json");
    fs.writeFileSync(zeroClaimView, "[]\n");
    const zeroClaim = raw("claim", zeroClaimView, "proof-slug", claimLease, claimUUID, claimName);
    assert.notEqual(zeroClaim.status, 0);
    assert.equal(fs.existsSync(claimLease), false);
    assert.equal(fs.existsSync(claimUUID), false);
    assert.equal(fs.existsSync(claimName), false);
    const duplicateClaimView = path.join(dir, "duplicate-claim-view.json");
    fs.writeFileSync(
      duplicateClaimView,
      JSON.stringify([
        { CloudID: createdUUID, Name: createdName, labels: { lease: createdLease, slug: "proof-slug" } },
        { CloudID: existingUUID, Name: "other", labels: { lease: "ukc_ffffffffffff", slug: "proof-slug" } },
      ]),
    );
    const duplicateClaim = raw(
      "claim",
      duplicateClaimView,
      "proof-slug",
      claimLease,
      claimUUID,
      claimName,
    );
    assert.notEqual(duplicateClaim.status, 0);
    assert.equal(fs.existsSync(claimLease), false);
    assert.equal(fs.existsSync(claimUUID), false);
    assert.equal(fs.existsSync(claimName), false);
    fs.writeFileSync(
      claimView,
      JSON.stringify([
        {
          CloudID: createdUUID,
          Name: createdName,
          labels: { lease: createdLease, slug: "proof-slug" },
        },
      ]),
    );
    fs.writeFileSync(claimUUID, "collision sentinel");
    const claimCollision = raw("claim", claimView, "proof-slug", claimLease, claimUUID, claimName);
    assert.notEqual(claimCollision.status, 0);
    assert.equal(fs.existsSync(claimLease), false);
    assert.equal(fs.readFileSync(claimUUID, "utf8"), "collision sentinel");
    assert.equal(fs.existsSync(claimName), false);
    fs.rmSync(claimUUID);
    const claimed = raw("claim", claimView, "proof-slug", claimLease, claimUUID, claimName);
    assert.equal(claimed.status, 0, claimed.stdout + claimed.stderr);
    assert.equal(fs.readFileSync(claimLease, "utf8"), createdLease);
    assert.equal(fs.readFileSync(claimUUID, "utf8"), createdUUID);
    assert.equal(fs.readFileSync(claimName, "utf8"), createdName);
    const owned = raw("owned", baselineFile, currentFile, createdName);
    assert.equal(owned.status, 0, owned.stdout + owned.stderr);
    assert.equal(owned.stdout.trim(), createdUUID);
    const unownedDelete = raw("delete", createdUUID, "crabbox-ukc-ffffffffffff");
    assert.notEqual(unownedDelete.status, 0);
    assert.doesNotMatch(fs.readFileSync(requestLog, "utf8"), /^DELETE \/v1\/instances$/m);
    fs.writeFileSync(badDeleteNameFile, "1");
    const conflictingDelete = raw("delete", createdUUID, createdName);
    assert.notEqual(conflictingDelete.status, 0);
    assert.equal(JSON.parse(fs.readFileSync(stateFile, "utf8")).length, 2);
    fs.rmSync(badDeleteNameFile);
    const deleted = raw("delete", createdUUID, createdName);
    assert.equal(deleted.status, 0, deleted.stdout + deleted.stderr);
    fs.writeFileSync(badAbsentIdentityFile, "1");
    const mismatchedAbsent = raw("absent", createdUUID);
    assert.notEqual(mismatchedAbsent.status, 0);
    const mismatchedNameAbsent = raw("absent-name", createdName);
    assert.notEqual(mismatchedNameAbsent.status, 0);
    fs.rmSync(badAbsentIdentityFile);
    const absent = raw("absent", createdUUID);
    assert.equal(absent.status, 0, absent.stdout + absent.stderr);
    const absentName = raw("absent-name", createdName);
    assert.equal(absentName.status, 0, absentName.stdout + absentName.stderr);
    const final = raw("inventory", finalFile);
    assert.equal(final.status, 0, final.stdout + final.stderr);
    assert.equal(raw("compare", baselineFile, finalFile).status, 0);
    assert.deepEqual(JSON.parse(fs.readFileSync(stateFile, "utf8")), [
      { name: "existing-service", uuid: existingUUID },
    ]);
    const requests = fs.readFileSync(requestLog, "utf8");
    assert.equal(
      (requests.match(new RegExp(`GET /v1/instances/${createdUUID}`, "g")) ?? []).length,
      3,
    );
    assert.equal(
      (requests.match(new RegExp(`GET /v1/instances/${createdName}`, "g")) ?? []).length,
      3,
    );
    assert.match(requests, /^DELETE \/v1\/instances$/m);
    assert.equal((requests.match(/^DELETE \/v1\/instances$/gm) ?? []).length, 2);
  } finally {
    await new Promise((resolve) => {
      const timer = setTimeout(() => {
        server.kill("SIGKILL");
        resolve();
      }, 2_000);
      server.once("exit", () => {
        clearTimeout(timer);
        resolve();
      });
      server.kill("SIGTERM");
    });
  }
});
