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
  const smokeScript = path.join(tempScripts, "live-nvidia-brev-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-nvidia-brev-smoke.sh"), smokeScript);
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

test("live nvidia brev smoke skips unless explicitly opted in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nbrev-skip-"));
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
      CRABBOX_NVIDIA_BREV_LIVE: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_NVIDIA_BREV_LIVE_not_enabled/);
});

test("live nvidia brev smoke runs guarded lifecycle without real credentials", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nbrev-"));
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
    printf 'NVIDIA-SMI 555.55.55\\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"},"provider":"nvidia-brev"}]\\n' "$slug"
    fi
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
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
      CRABBOX_NVIDIA_BREV_LIVE: "1",
      CRABBOX_NVIDIA_BREV_CLEANUP_ATTEMPTS: "2",
      CRABBOX_NVIDIA_BREV_CLEANUP_POLL_SECONDS: "0",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_nvidia_brev_smoke_passed/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider nvidia-brev --nvidia-brev-cli brev");
  assert.equal(seen[1], "list --provider nvidia-brev --nvidia-brev-cli brev --json");
  assert.match(seen[2], /^warmup --provider nvidia-brev --nvidia-brev-cli brev --nvidia-brev-release-action delete --slug nbrev-smoke-\d+-\d+-[0-9a-f]{8} --keep=false --ttl 20m --idle-timeout 5m$/);
  assert.ok(seen[2].match(/--slug (\S+)/)[1].length <= 48);
  assert.match(seen[3], /^status --provider nvidia-brev --nvidia-brev-cli brev --nvidia-brev-release-action delete --id nbrev-smoke-\d+-\d+-[0-9a-f]{8} --wait --wait-timeout 300s$/);
  assert.match(seen[4], /^run --provider nvidia-brev --nvidia-brev-cli brev --nvidia-brev-release-action delete --id nbrev-smoke-\d+-\d+-[0-9a-f]{8} --no-sync -- nvidia-smi$/);
  assert.equal(seen[5], "list --provider nvidia-brev --nvidia-brev-cli brev --json");
  assert.match(seen[6], /^stop --provider nvidia-brev --nvidia-brev-cli brev --nvidia-brev-release-action delete nbrev-smoke-\d+-\d+-[0-9a-f]{8}$/);
});

test("live nvidia brev smoke attempts targeted cleanup after partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nbrev-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
${shellArgHelper}
printf '%s\\n' "$*" >>"${calls}"
if [[ "$1" == "doctor" || "$1" == "list" ]]; then
  [[ "$1" == "list" ]] && printf '[]\\n' || printf 'auth=ready\\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  printf 'created workspace before failing\\n' >&2
  exit 37
fi
if [[ "$1" == "stop" ]]; then
  printf 'nvidia-brev workspace not found: %s\\n' "\${@: -1}" >&2
  exit 4
fi
exit 99
`,
  );
  writeExecutable(
    path.join(binDir, "brev"),
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "ls" ]]; then
  printf '{"workspaces":[]}\\n'
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
      CRABBOX_NVIDIA_BREV_LIVE: "1",
      CRABBOX_NVIDIA_BREV_CLEANUP_ATTEMPTS: "2",
      CRABBOX_NVIDIA_BREV_CLEANUP_POLL_SECONDS: "0",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(result.stderr, /created workspace before failing/);
  assert.match(fs.readFileSync(calls, "utf8"), /warmup .* --keep=false /);
  assert.match(fs.readFileSync(calls, "utf8"), /stop --provider nvidia-brev .* nbrev-smoke-\d+-\d+-[0-9a-f]{8}/);
  assert.doesNotMatch(result.stderr, /reason=cleanup_failed/);
});

test("live nvidia brev smoke deletes an unclaimed workspace by unique prefix", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nbrev-unclaimed-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const slugFile = path.join(dir, "slug.txt");
  const brevCalls = path.join(dir, "brev-calls.log");
  const brevListCount = path.join(dir, "brev-list-count.txt");
  const crabboxCalls = path.join(dir, "crabbox-calls.log");
  const customBrev = path.join(binDir, "custom-brev");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
${shellArgHelper}
printf '%s\\n' "$*" >>"${crabboxCalls}"
if [[ "$1" == "doctor" || "$1" == "list" ]]; then
  [[ "$1" == "list" ]] && printf '[]\\n' || printf 'auth=ready\\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  arg_after --slug "$@" >"${slugFile}"
  printf 'rollback cleanup failed\\n' >&2
  exit 37
fi
if [[ "$1" == "stop" ]]; then
  printf 'refusing to release nvidia-brev workspace ws-orphan without a local Crabbox claim\\n' >&2
  exit 2
fi
exit 99
`,
  );
  writeExecutable(
    customBrev,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${brevCalls}"
if [[ "$1" == "ls" ]]; then
  count="$(cat "${brevListCount}" 2>/dev/null || printf '0')"
  count=$((count + 1))
  printf '%s' "$count" >"${brevListCount}"
  if [[ "$count" -eq 1 ]]; then
    printf '{"workspaces":[]}\\n'
    exit 0
  fi
  slug="$(cat "${slugFile}")"
  printf '{"workspaces":[{"name":"crabbox-%s-abcdef123456"}]}\\n' "$slug"
  exit 0
fi
if [[ "$1" == "delete" ]]; then
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
      CRABBOX_NVIDIA_BREV_LIVE: "1",
      CRABBOX_NVIDIA_BREV_CLI: customBrev,
      CRABBOX_NVIDIA_BREV_CLEANUP_ATTEMPTS: "2",
      CRABBOX_NVIDIA_BREV_CLEANUP_POLL_SECONDS: "0",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.doesNotMatch(result.stderr, /reason=cleanup_failed/);
  const calls = fs.readFileSync(brevCalls, "utf8");
  assert.equal(calls.match(/^ls --json --all$/gm)?.length, 2);
  assert.match(calls, /^delete crabbox-nbrev-smoke-\d+-\d+-[0-9a-f]{8}-abcdef123456$/m);
  assert.ok(fs.readFileSync(crabboxCalls, "utf8").includes(`--nvidia-brev-cli ${customBrev}`));
});

test("live nvidia brev smoke refuses ambiguous fallback cleanup", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nbrev-ambiguous-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const slugFile = path.join(dir, "slug.txt");
  const brevCalls = path.join(dir, "brev-calls.log");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
${shellArgHelper}
if [[ "$1" == "doctor" || "$1" == "list" ]]; then
  [[ "$1" == "list" ]] && printf '[]\\n' || printf 'auth=ready\\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  arg_after --slug "$@" >"${slugFile}"
  exit 37
fi
if [[ "$1" == "stop" ]]; then
  printf 'refusing to release nvidia-brev workspace ws-orphan without a local Crabbox claim\\n' >&2
  exit 2
fi
exit 99
`,
  );
  writeExecutable(
    path.join(binDir, "brev"),
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${brevCalls}"
if [[ "$1" == "ls" ]]; then
  slug="$(cat "${slugFile}")"
  printf '{"workspaces":[{"name":"crabbox-%s-first"},{"name":"crabbox-%s-second"}]}\\n' "$slug" "$slug"
  exit 0
fi
if [[ "$1" == "delete" ]]; then
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
      CRABBOX_NVIDIA_BREV_LIVE: "1",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /reason=cleanup_inventory_invalid/);
  assert.match(result.stderr, /multiple workspaces match cleanup prefix/);
  assert.doesNotMatch(fs.readFileSync(brevCalls, "utf8"), /^delete /m);
});

test("live nvidia brev smoke reports unrelated cleanup failures", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nbrev-cleanup-fail-"));
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
  printf 'created workspace before failing\\n' >&2
  exit 37
fi
if [[ "$1" == "stop" ]]; then
  printf 'exec: brev: executable file not found\\n' >&2
  exit 4
fi
exit 99
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_NVIDIA_BREV_LIVE: "1",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /reason=cleanup_failed/);
  assert.match(result.stderr, /executable file not found/);
});

test("live nvidia brev smoke classifies capacity failures", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nbrev-capacity-"));
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
  printf 'requested GPU not available\\n' >&2
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
      CRABBOX_NVIDIA_BREV_LIVE: "1",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 42, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=capacity_blocked/);
});
