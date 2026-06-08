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

test("live docker sandbox smoke honors configured alternate sbx path", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-sbx-smoke-"));
  const bin = path.join(dir, "bin");
  const tempRoot = path.join(dir, "repo");
  const tempScripts = path.join(tempRoot, "scripts");
  const fakeSbx = path.join(dir, "fake-sbx");
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(bin);
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(
    path.join(repoRoot, "scripts", "live-docker-sandbox-smoke.sh"),
    path.join(tempScripts, "live-docker-sandbox-smoke.sh"),
  );
  fs.chmodSync(path.join(tempScripts, "live-docker-sandbox-smoke.sh"), 0o755);

  writeExecutable(
    fakeSbx,
    `#!/usr/bin/env bash
set -euo pipefail
exit 0
`,
  );

  writeExecutable(
    path.join(bin, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
mkdir -p bin
cat <<'EOF' > bin/crabbox
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${calls}"
if [[ "$1" == "doctor" ]]; then
  if [[ -z "\${CRABBOX_DOCKER_SANDBOX_CLI:-}" || ! -x "\${CRABBOX_DOCKER_SANDBOX_CLI}" ]]; then
    printf 'missing configured docker sandbox cli\n' >&2
    exit 92
  fi
  printf 'ok      sbx_version provider=docker-sandbox version=sbx client fake\n'
fi
exit 0
EOF
chmod +x bin/crabbox
`,
  );

  const result = spawnSync("bash", [path.join(tempScripts, "live-docker-sandbox-smoke.sh")], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}/bin${path.delimiter}/usr/bin`,
      CRABBOX_DOCKER_SANDBOX_CLI: fakeSbx,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /classification=live_sbx_smoke_passed/);
  assert.match(result.stdout, /sbx_version/);
  assert.doesNotMatch(result.stderr, /sbx not found on PATH/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen.length, 6, JSON.stringify(seen));
  assert.equal(seen[0], "doctor --provider docker-sandbox");
  assert.match(seen[1], /^warmup --provider docker-sandbox --slug docker-sandbox-smoke-\d{14}-\d+ --keep$/);
  assert.match(seen[2], /^run --provider docker-sandbox --id docker-sandbox-smoke-\d{14}-\d+ -- echo ok$/);
  assert.match(seen[3], /^run --provider docker-sandbox --id docker-sandbox-smoke-\d{14}-\d+ -- pwd$/);
  assert.match(seen[4], /^list --provider docker-sandbox --json$/);
  assert.match(seen[5], /^stop --provider docker-sandbox docker-sandbox-smoke-\d{14}-\d+$/);
});

test("live docker sandbox smoke classifies provider preflight failures", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-docker-sandbox-"));
  const binDir = path.join(dir, "bin");
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
if [[ "$1 $2 $3" == "doctor --provider docker-sandbox" ]]; then
  printf 'virtualization unavailable\n' >&2
  exit 23
fi
printf 'unexpected crabbox args: %s\n' "$*" >&2
exit 99
SCRIPT
chmod +x "$out"
`,
  );

  const result = spawnSync("bash", ["scripts/live-docker-sandbox-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      HOME: process.env.HOME ?? dir,
      TMPDIR: process.env.TMPDIR ?? os.tmpdir(),
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 23, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(result.stderr, /doctor\\ --provider\\ docker-sandbox/);
  assert.match(result.stderr, /virtualization unavailable/);
});

test("live docker sandbox smoke classifies quota-like provider blockers", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-docker-sandbox-quota-"));
  const binDir = path.join(dir, "bin");
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
if [[ "$1 $2 $3" == "doctor --provider docker-sandbox" ]]; then
  printf 'Docker Sandbox quota exceeded for this account\n' >&2
  exit 29
fi
printf 'unexpected crabbox args: %s\n' "$*" >&2
exit 99
SCRIPT
chmod +x "$out"
`,
  );

  const result = spawnSync("bash", ["scripts/live-docker-sandbox-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      HOME: process.env.HOME ?? dir,
      TMPDIR: process.env.TMPDIR ?? os.tmpdir(),
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 29, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=quota_blocked/);
  assert.match(result.stderr, /quota exceeded/);
});
