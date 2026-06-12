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
  const smokeScript = path.join(tempScripts, "live-anthropic-sandbox-runtime-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(
    path.join(repoRoot, "scripts", "live-anthropic-sandbox-runtime-smoke.sh"),
    smokeScript,
  );
  fs.chmodSync(smokeScript, 0o755);
  return { tempRoot, smokeScript };
}

function smokeEnv(dir, bin, extra = {}) {
  const env = {
    ...process.env,
    PATH: `${bin}${path.delimiter}/bin${path.delimiter}/usr/bin`,
    TMPDIR: dir,
    ...extra,
  };
  if (!Object.hasOwn(extra, "CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI")) {
    delete env.CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI;
  }
  return env;
}

test("live Anthropic Sandbox Runtime smoke proves run and enforcement paths", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-srt-smoke-"));
  const bin = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(bin);

  writeExecutable(path.join(bin, "srt"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(path.join(bin, "curl"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    path.join(bin, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
mkdir -p bin
cat >bin/crabbox <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "$*" in
  "doctor --provider anthropic-sandbox-runtime")
    printf 'ok      srt_help provider=anthropic-sandbox-runtime mutation=false\\n'
    ;;
  "run --provider anthropic-sandbox-runtime -- echo ok")
    printf 'ok\\n'
    ;;
  run\\ --provider\\ anthropic-sandbox-runtime\\ --anthropic-sandbox-runtime-settings*allowed*)
    printf 'ok\\n'
    ;;
  run\\ --provider\\ anthropic-sandbox-runtime\\ --anthropic-sandbox-runtime-settings*cat*)
    printf 'Operation not permitted\\n' >&2
    exit 5
    ;;
  run\\ --provider\\ anthropic-sandbox-runtime\\ --anthropic-sandbox-runtime-settings*curl*)
    printf 'Connection blocked by network allowlist\\n' >&2
    exit 7
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
SCRIPT
chmod +x bin/crabbox
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: smokeEnv(dir, bin),
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_anthropic_sandbox_runtime_smoke_passed/);
  assert.match(result.stdout, /ok/);
  assert.match(result.stderr, /Operation not permitted/);
  assert.match(result.stderr, /Connection blocked by network allowlist/);
  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen.length, 5, JSON.stringify(seen));
  assert.equal(seen[0], "doctor --provider anthropic-sandbox-runtime");
  assert.equal(seen[1], "run --provider anthropic-sandbox-runtime -- echo ok");
});

test("live Anthropic Sandbox Runtime smoke classifies missing srt", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-srt-missing-"));
  const bin = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(bin);

  writeExecutable(
    path.join(bin, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
mkdir -p bin
cat >bin/crabbox <<'SCRIPT'
#!/usr/bin/env bash
exit 0
SCRIPT
chmod +x bin/crabbox
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: smokeEnv(dir, bin),
    encoding: "utf8",
  });

  assert.equal(result.status, 127, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(result.stderr, /srt not found at configured path srt/);
});

test("live Anthropic Sandbox Runtime smoke honors configured srt cli path", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-srt-custom-"));
  const bin = path.join(dir, "bin");
  const custom = path.join(dir, "custom-srt");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(bin);

  writeExecutable(custom, "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(path.join(bin, "curl"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    path.join(bin, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
mkdir -p bin
cat >bin/crabbox <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  "doctor --provider anthropic-sandbox-runtime")
    printf 'ok      srt_help provider=anthropic-sandbox-runtime mutation=false\\n'
    ;;
  "run --provider anthropic-sandbox-runtime -- echo ok")
    printf 'ok\\n'
    ;;
  run\\ --provider\\ anthropic-sandbox-runtime\\ --anthropic-sandbox-runtime-settings*allowed*)
    printf 'ok\\n'
    ;;
  run\\ --provider\\ anthropic-sandbox-runtime\\ --anthropic-sandbox-runtime-settings*cat*)
    printf 'Operation not permitted\\n' >&2
    exit 5
    ;;
  run\\ --provider\\ anthropic-sandbox-runtime\\ --anthropic-sandbox-runtime-settings*curl*)
    printf 'Connection blocked by network allowlist\\n' >&2
    exit 7
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
SCRIPT
chmod +x bin/crabbox
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: smokeEnv(dir, bin, {
      CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI: custom,
    }),
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_anthropic_sandbox_runtime_smoke_passed/);
});

test("live Anthropic Sandbox Runtime smoke rejects unexpected success for denied checks", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-srt-validation-"));
  const bin = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(bin);

  writeExecutable(path.join(bin, "srt"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(path.join(bin, "curl"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    path.join(bin, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
mkdir -p bin
cat >bin/crabbox <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  "doctor --provider anthropic-sandbox-runtime")
    printf 'ok      srt_help provider=anthropic-sandbox-runtime mutation=false\\n'
    ;;
  "run --provider anthropic-sandbox-runtime -- echo ok")
    printf 'ok\\n'
    ;;
  run\\ --provider\\ anthropic-sandbox-runtime\\ --anthropic-sandbox-runtime-settings*)
    printf 'ok\\n'
    ;;
esac
SCRIPT
chmod +x bin/crabbox
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: smokeEnv(dir, bin),
    encoding: "utf8",
  });

  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /command unexpectedly succeeded/);
});
