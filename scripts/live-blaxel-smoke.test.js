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

function prepareSmokeScript(dir) {
  const tempRoot = path.join(dir, "repo");
  const tempScripts = path.join(tempRoot, "scripts");
  const smokeScript = path.join(tempScripts, "live-blaxel-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-blaxel-smoke.sh"), smokeScript);
  fs.chmodSync(smokeScript, 0o755);
  return { tempRoot, smokeScript };
}

function runSmoke(smokeScript, tempRoot, env = {}) {
  return spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      HOME: env.HOME ?? tempRoot,
      TMPDIR: env.TMPDIR ?? os.tmpdir(),
      ...env,
    },
    encoding: "utf8",
  });
}

test("live blaxel smoke skips mutation without explicit live gate", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-blaxel-skip-"));
  const { tempRoot, smokeScript } = prepareSmokeScript(dir);
  const calls = path.join(dir, "calls.log");
  const fakeCrabbox = path.join(dir, "crabbox");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
if [[ "$1 $2 $3" == "doctor --provider blaxel" ]]; then
  printf '{"provider":"blaxel","ok":true}\\n'
  exit 0
fi
if [[ "$1 $2 $3" == "list --provider blaxel" ]]; then
  printf '[]\\n'
  exit 0
fi
printf 'unexpected mutation: %s\\n' "$*" >&2
exit 99
`,
  );

  const result = runSmoke(smokeScript, tempRoot, {
    CRABBOX_BIN: fakeCrabbox,
    CRABBOX_BLAXEL_API_KEY: "secret-key",
    CRABBOX_BLAXEL_WORKSPACE: "workspace-secret",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /classification=skipped/);
  assert.doesNotMatch(result.stdout + result.stderr, /secret-key|workspace-secret/);
  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.deepEqual(seen, ["doctor --provider blaxel --json", "list --provider blaxel --json"]);
});

test("live blaxel smoke classifies missing credentials as environment blocked", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-blaxel-missing-"));
  const { tempRoot, smokeScript } = prepareSmokeScript(dir);
  const fakeCrabbox = path.join(dir, "crabbox");
  writeExecutable(fakeCrabbox, "#!/usr/bin/env bash\nexit 99\n");

  const result = runSmoke(smokeScript, tempRoot, {
    CRABBOX_BIN: fakeCrabbox,
    CRABBOX_BLAXEL_API_KEY: "",
    BL_API_KEY: "",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /classification=environment_blocked/);
  assert.match(result.stdout, /missing CRABBOX_BLAXEL_API_KEY or BL_API_KEY/);
});

test("live blaxel smoke classifies quota-like preflight failures", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-blaxel-quota-"));
  const { tempRoot, smokeScript } = prepareSmokeScript(dir);
  const fakeCrabbox = path.join(dir, "crabbox");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2 $3" == "doctor --provider blaxel" ]]; then
  printf 'Blaxel quota exceeded for this workspace\\n' >&2
  exit 29
fi
printf 'unexpected args: %s\\n' "$*" >&2
exit 99
`,
  );

  const result = runSmoke(smokeScript, tempRoot, {
    CRABBOX_BIN: fakeCrabbox,
    CRABBOX_BLAXEL_API_KEY: "secret-key",
    CRABBOX_BLAXEL_WORKSPACE: "workspace-secret",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /classification=quota_blocked/);
  assert.doesNotMatch(result.stdout + result.stderr, /secret-key|workspace-secret/);
});

test("live blaxel smoke runs gated lifecycle and redacts public output", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-blaxel-ok-"));
  const { tempRoot, smokeScript } = prepareSmokeScript(dir);
  const fakeCrabbox = path.join(dir, "crabbox");
  const calls = path.join(dir, "calls.log");
  const stopped = path.join(dir, "stopped.log");
  const slugFile = path.join(dir, "slug.txt");
  const listCount = path.join(dir, "list-count.txt");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
if [[ "$1 $2 $3" == "doctor --provider blaxel" ]]; then
  printf '{"provider":"blaxel","ok":true,"workspace":"%s"}\\n' "\${CRABBOX_BLAXEL_WORKSPACE:-}"
  exit 0
fi
if [[ "$1 $2 $3" == "list --provider blaxel" ]]; then
  count=0
  if [[ -f "${listCount}" ]]; then
    count="$(cat "${listCount}")"
  fi
  count=$((count + 1))
  printf '%s\\n' "$count" >"${listCount}"
  if [[ "$count" -eq 1 ]]; then
    printf '[]\\n'
  else
    slug="$(cat "${slugFile}")"
    printf '[{"provider":"blaxel","slug":"%s"}]\\n' "$slug"
  fi
  exit 0
fi
if [[ "$1" == "run" && "$*" == *"--keep"* ]]; then
  while [[ "$#" -gt 0 ]]; do
    if [[ "$1" == "--slug" ]]; then
      printf '%s\\n' "$2" >"${slugFile}"
      break
    fi
    shift
  done
  printf 'BLAXEL_SMOKE_V1_OK\\n'
  printf '{"name":"blaxel_sync","ms":1}\\n' >&2
  exit 0
fi
if [[ "$1" == "run" && "$*" == *"exit 17"* ]]; then
  exit 17
fi
if [[ "$1 $2 $3" == "status --provider blaxel" ]]; then
  slug="$(cat "${slugFile}")"
  printf '{"provider":"blaxel","slug":"%s","ready":true}\\n' "$slug"
  exit 0
fi
if [[ "$1" == "list" ]]; then
  slug="$(cat "${slugFile}")"
  printf '[{"provider":"blaxel","slug":"%s"}]\\n' "$slug"
  exit 0
fi
if [[ "$1" == "stop" ]]; then
  printf '%s\\n' "$4" >>"${stopped}"
  exit 0
fi
if [[ "$1" == "cleanup" ]]; then
  printf 'would delete none\\n'
  exit 0
fi
printf 'unexpected args with secret super-secret-key workspace workspace-secret url https://api.secret.example.test: %s\\n' "$*" >&2
exit 99
`,
  );

  const result = runSmoke(smokeScript, tempRoot, {
    CRABBOX_BIN: fakeCrabbox,
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "blaxel",
    CRABBOX_BLAXEL_API_KEY: "super-secret-key",
    CRABBOX_BLAXEL_WORKSPACE: "workspace-secret",
    CRABBOX_BLAXEL_API_URL: "https://api.secret.example.test",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /classification=live_blaxel_smoke_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /super-secret-key|workspace-secret|api\.secret\.example|\/Users\//);
  const seen = fs.readFileSync(calls, "utf8");
  assert.match(seen, /doctor --provider blaxel --json/);
  assert.match(seen, /run --provider blaxel --keep --slug blaxel-live-smoke-/);
  assert.match(seen, /run --provider blaxel --id blaxel-live-smoke-/);
  assert.match(seen, /status --provider blaxel --id blaxel-live-smoke-/);
  assert.match(seen, /stop --provider blaxel blaxel-live-smoke-/);
  assert.match(seen, /cleanup --provider blaxel --dry-run/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^blaxel-live-smoke-/);
});

test("live blaxel smoke stops an owned sandbox after validation failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-blaxel-validation-"));
  const { tempRoot, smokeScript } = prepareSmokeScript(dir);
  const fakeCrabbox = path.join(dir, "crabbox");
  const stopped = path.join(dir, "stopped.log");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2 $3" == "doctor --provider blaxel" ]]; then
  printf '{"provider":"blaxel","ok":true}\\n'
  exit 0
fi
if [[ "$1 $2 $3" == "list --provider blaxel" ]]; then
  printf '[]\\n'
  exit 0
fi
if [[ "$1" == "run" && "$*" == *"--keep"* ]]; then
  printf 'missing marker\\n'
  printf '{"name":"sync","ms":1}\\n' >&2
  exit 0
fi
if [[ "$1" == "stop" ]]; then
  printf '%s\\n' "$4" >>"${stopped}"
  exit 0
fi
printf 'unexpected args: %s\\n' "$*" >&2
exit 99
`,
  );

  const result = runSmoke(smokeScript, tempRoot, {
    CRABBOX_BIN: fakeCrabbox,
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "blaxel",
    CRABBOX_BLAXEL_API_KEY: "secret-key",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=validation_failed/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^blaxel-live-smoke-/);
});
