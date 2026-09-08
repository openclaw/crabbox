import assert from "node:assert/strict";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import {
  chmod,
  copyFile,
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  rm,
  writeFile,
} from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..");
const script = path.join(scriptDir, "mint-aws-devtools-image.sh");

async function setupFakeCrabbox() {
  const dir = await mkdtemp(path.join(os.tmpdir(), "crabbox-aws-image-mint-test-"));
  const log = path.join(dir, "fake.log");
  const fake = path.join(dir, "crabbox");
  const linuxPrep = path.join(dir, "linux.sh");
  const windowsPrep = path.join(dir, "windows.ps1");
  await writeFile(linuxPrep, "#!/usr/bin/env bash\nexit 0\n");
  await chmod(linuxPrep, 0o755);
  await writeFile(windowsPrep, "exit 0\n");
  await writeFile(
    fake,
    `#!/usr/bin/env bash
set -euo pipefail
printf 'env CRABBOX_AWS_REGION=%s AWS_REGION=%s CRABBOX_AWS_AMI=%s args %s\\n' "\${CRABBOX_AWS_REGION:-}" "\${AWS_REGION:-}" "\${CRABBOX_AWS_AMI:-}" "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  warmup)
    count_file="\${CRABBOX_FAKE_LOG}.count"
    count=0
    [[ -f "$count_file" ]] && count="$(cat "$count_file")"
    count="$((count + 1))"
    printf '%s\\n' "$count" >"$count_file"
    case "$count" in
      1)
        printf 'image selected id=ami-previous source=promoted kind=aws-ami region=%s promoted_at=2026-09-01T00:00:00Z\\n' "\${CRABBOX_AWS_REGION:-eu-west-1}"
        printf '{"leaseId":"cbx_source"}\\n'
        ;;
      2)
        printf 'image selected id=%s source=explicit kind=aws-ami region=%s promoted_at=-\\n' "\${CRABBOX_AWS_AMI:-}" "\${CRABBOX_AWS_REGION:-eu-west-1}"
        printf '{"leaseId":"cbx_candidate"}\\n'
        ;;
      *)
        printf 'image selected id=ami-devtools source=promoted kind=aws-ami region=%s promoted_at=2026-07-31T00:00:00Z\\n' "\${CRABBOX_AWS_REGION:-eu-west-1}"
        printf '{"leaseId":"cbx_promoted"}\\n'
        ;;
    esac
    if [[ "\${CRABBOX_FAKE_WARMUP_FAIL_AFTER_LEASE:-0}" == "1" ]]; then
      exit 23
    fi
    ;;
  run)
    if [[ " $* " == *" --allow-env CRABBOX_LINUX_NODE_MAJOR,CRABBOX_LINUX_PNPM_VERSION "* ]]; then
      printf 'builder overrides node=%s pnpm=%s\\n' "\${CRABBOX_LINUX_NODE_MAJOR:-}" "\${CRABBOX_LINUX_PNPM_VERSION:-}" >>"\${CRABBOX_FAKE_LOG}"
    fi
    if [[ -n "\${CRABBOX_FAKE_SMOKE_FAIL_LEASE:-}" && " $* " == *" --id \${CRABBOX_FAKE_SMOKE_FAIL_LEASE} "* && "\${@: -1}" == *"docker_probe="* ]]; then
      printf 'offline artifact smoke failed\\n' >&2
      exit 73
    fi
    if [[ "\${CRABBOX_FAKE_READINESS_FAIL:-0}" == "1" && "$*" == *"linux-readiness.generated.sh"* && "$*" != *"-- --install"* ]]; then
      printf 'Linux readiness producer: minimal capability proof failed\\n' >&2
      exit 74
    fi
    if [[ -n "\${CRABBOX_FAKE_CAPTURE_RUN_SCRIPT:-}" ]]; then
      last_arg="\${@: -1}"
      if [[ "$last_arg" == *"docker_probe="* ]]; then
        printf '%s\\n' "$last_arg" >"\${CRABBOX_FAKE_CAPTURE_RUN_SCRIPT}"
      fi
    fi
    if [[ -n "\${CRABBOX_FAKE_NODE_CHECK_RUNNER:-}" && "\${@: -1}" == *"docker_probe="* ]]; then
      lease=""
      previous=""
      for arg in "$@"; do
        if [[ "$previous" == "--id" ]]; then lease="$arg"; break; fi
        previous="$arg"
      done
      "\${CRABBOX_FAKE_NODE_CHECK_RUNNER}" "$lease" "\${@: -1}" || exit $?
    fi
    if [[ "$*" == *"Test-Path 'C:\\ProgramData\\crabbox\\image-prep-reboot-required'"* ]]; then
      if [[ "\${CRABBOX_FAKE_WINDOWS_REBOOT:-0}" == "1" && ! -f "\${CRABBOX_FAKE_LOG}.rebooted" ]]; then
        printf 'crabbox-reboot-required\\n'
      else
        printf 'crabbox-reboot-not-required\\n'
      fi
      exit 0
    fi
    if [[ "$*" == *"shutdown /r"* ]]; then
      touch "\${CRABBOX_FAKE_LOG}.rebooted"
      printf 'reboot scheduled\\n'
      exit 0
    fi
    if [[ "$*" == *"FromBase64String"* && "\${CRABBOX_FAKE_WINDOWS_PREP_DISCONNECT:-0}" == "1" && ! -f "\${CRABBOX_FAKE_LOG}.prep-disconnected" ]]; then
      touch "\${CRABBOX_FAKE_LOG}.prep-disconnected"
      exit 255
    fi
    if [[ "$*" == *"Start-ScheduledTask"* ]]; then
      touch "\${CRABBOX_FAKE_LOG}.prep-started"
      printf 'crabbox-prep-started\\n'
      exit 0
    fi
    if [[ "$*" == *"image-prep.done"* ]]; then
      printf 'crabbox-prep-done\\n0\\n'
      exit 0
    fi
    printf 'devtools-smoke-ok\\n'
    ;;
  checkpoint)
    if [[ "$2" == "create" ]]; then
      printf 'checkpoint created id=chk_devtools kind=aws-ami resource=ami-devtools state=available region=us-west-2 workdir=-\\n'
    fi
    ;;
  image)
    if [[ "$2" == "promote" ]]; then
      if [[ " $* " == *" --expected-current-image capture "* ]]; then
        if [[ "\${CRABBOX_FAKE_PROMOTION_FAIL:-0}" == "1" ]]; then
          printf 'transactional promotion unavailable\\n' >&2
          exit 55
        fi
        if [[ "\${CRABBOX_FAKE_PREVIOUS_ABSENT:-0}" == "1" ]]; then
          printf '{"image":{"id":"ami-devtools","region":"us-west-2","revision":"rev-new"},"previous":{"state":"absent","aliases":[{"alias":"regional","state":"absent"}]}}\\n'
        else
          printf '{"image":{"id":"ami-devtools","region":"us-west-2","revision":"rev-new"},"previous":{"state":"present","imageId":"%s","revision":"rev-old","aliases":[{"alias":"regional","state":"present","image":{"id":"%s","region":"us-west-2","name":"previous","state":"available","provider":"aws","promotedAt":"2026-09-01T00:00:00Z","revision":"rev-old"}}]}}\\n' "\${CRABBOX_FAKE_PREVIOUS_IMAGE:-ami-previous}" "\${CRABBOX_FAKE_PREVIOUS_IMAGE:-ami-previous}"
        fi
      elif [[ "\${CRABBOX_FAKE_ROLLBACK_FAIL:-0}" == "1" ]]; then
        printf 'coordinator: http 409: image default changed\\n' >&2
        exit 19
      else
        printf '{"image":{"id":"ami-previous","revision":"rev-restored"},"previous":{"state":"present","imageId":"ami-devtools","revision":"rev-new"}}\\n'
      fi
    fi
    ;;
  stop)
    if [[ "\${CRABBOX_FAKE_STOP_FAIL_LEASE:-}" == "\${*: -1}" ]]; then
      printf 'stop failed for %s\\n' "\${*: -1}" >&2
      exit 27
    fi
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  status)
    printf 'ready\\n'
    ;;
esac
`,
  );
  await chmod(fake, 0o755);
  return { dir, fake, log, linuxPrep, windowsPrep };
}

function runScript(args, env, scriptPath = script, onOutput) {
  return new Promise((resolve, reject) => {
    const child = spawn("bash", [scriptPath, ...args], {
      cwd: path.resolve(path.dirname(scriptPath), ".."),
      env: { ...process.env, ...env },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");
    child.stdout.on("data", (chunk) => {
      stdout += chunk;
      onOutput?.(chunk, child);
    });
    child.stderr.on("data", (chunk) => {
      stderr += chunk;
    });
    child.on("error", reject);
    child.on("close", (code) => resolve({ code, stdout, stderr }));
  });
}

test("AWS devtools mint wrapper defaults to dry plan", async () => {
  const fake = await setupFakeCrabbox();
  const result = await runScript(["--prep-script", fake.linuxPrep], {
    CRABBOX_BIN: fake.fake,
    CRABBOX_FAKE_LOG: fake.log,
  });
  assert.equal(result.code, 0, result.stderr);
  assert.match(result.stdout, /dry plan only/);
  await assert.rejects(readFile(fake.log, "utf8"));
});

test("AWS developer image smoke executes package managers and requires TruffleHog", async () => {
  const text = await readFile(script, "utf8");
  const windowsSmoke = await readFile(
    path.join(scriptDir, "devtools-image-smoke-windows.ps1"),
    "utf8",
  );
  const linuxSmoke = await readFile(path.join(scriptDir, "devtools-image-smoke-linux.sh"), "utf8");
  assert.match(windowsSmoke, /pnpm --version\ntrufflehog --no-update --version\ndocker --version/);
  assert.match(
    linuxSmoke,
    /command -v pnpm\ncommand -v trufflehog\ntrufflehog --no-update --version\ncommand -v docker\nnode --version\nnode -e .*\ncorepack --version\npnpm --version\n/,
  );
  assert.match(linuxSmoke, /corepack --version\npnpm --version\ndocker_group_member/);
  assert.match(windowsSmoke, /docker image inspect .*\nif \(\$LASTEXITCODE -ne 0\).*throw.*\nWrite-Output "devtools-smoke-ok"\n$/);
  assert.match(text, /trap 'exit 130' INT\ntrap 'exit 143' TERM/);
  assert.match(text, /rollback_pending=1\nrun_json_tee "\$promotion_log"/);
  assert.doesNotMatch(text, /\bBASHPID\b/);
});

test("AWS Linux image production stages and invokes only the generated readiness producer", async () => {
  const source = await readFile(script, "utf8");
  const smoke = await readFile(path.join(scriptDir, "devtools-image-smoke-linux.sh"), "utf8");
  assert.match(source, /--script "\$ROOT\/scripts\/linux-readiness\.generated\.sh" -- --install/);
  assert.equal(
    (source.match(/--script "\$ROOT\/scripts\/linux-readiness\.generated\.sh"/g) ?? []).length,
    1,
  );
  assert.match(source, /--shell -- \/usr\/local\/libexec\/crabbox\/linux-readiness\.generated\.sh/);
  assert.match(smoke, /test -f \/var\/lib\/crabbox-readiness\/linux\.json/);
  assert.doesNotMatch(source, /sudo tee \/var\/lib\/crabbox\/image-ready/);
  assert.doesNotMatch(source, /printf 'crabbox-devtools-v1/);
});

test("AWS Linux image capture refuses a failed minimal readiness proof", async () => {
  const fake = await setupFakeCrabbox();
  const result = await runScript(
    ["--target", "linux", "--run", "--no-promote", "--prep-script", fake.linuxPrep],
    { CRABBOX_BIN: fake.fake, CRABBOX_FAKE_LOG: fake.log, CRABBOX_FAKE_READINESS_FAIL: "1" },
  );
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /minimal capability proof failed/);
  const log = await readFile(fake.log, "utf8");
  assert.match(log, /linux-readiness\.generated\.sh -- --install/);
  assert.doesNotMatch(log, /checkpoint create/);
});

test("AWS devtools mint wrapper runs linux source candidate and promoted proof", async () => {
  const fake = await setupFakeCrabbox();
  const result = await runScript(
    [
      "--target",
      "linux",
      "--region",
      "us-west-2",
      "--type",
      "m7i.large",
      "--run",
      "--fast-snapshot-restore",
      "--fsr-az",
      "us-west-2a",
    ],
    {
      CRABBOX_BIN: fake.fake,
      CRABBOX_FAKE_LOG: fake.log,
      CRABBOX_IMAGE_WINDOWS_WARMUP_SETTLE_SECONDS: "0",
      CRABBOX_IMAGE_REBOOT_READY_SETTLE_SECONDS: "0",
      CRABBOX_IMAGE_PREP_WAIT_TIMEOUT: "5s",
    },
  );
  assert.equal(result.code, 0, result.stderr);
  assert.match(result.stdout, /candidate AMI smoke passed: ami-devtools/);
  assert.match(result.stdout, /promoted image selection proved: ami-devtools/);
  assert.match(result.stdout, /promoted linux developer image passed: ami-devtools/);
  const log = await readFile(fake.log, "utf8");
  assert.match(
    log,
    /env CRABBOX_AWS_REGION=us-west-2 AWS_REGION=us-west-2 CRABBOX_AWS_AMI= args warmup --provider aws --target linux/,
  );
  assert.match(
    log,
    /env CRABBOX_AWS_REGION=us-west-2 AWS_REGION=us-west-2 CRABBOX_AWS_AMI=ami-devtools args warmup --provider aws --target linux/,
  );
  assert.match(log, /--class standard/);
  assert.match(log, /--browser/);
  assert.doesNotMatch(log, /warmup .*--region us-west-2/);
  assert.match(log, /run --provider aws --target linux --id cbx_source --no-sync --script/);
  assert.match(log, /linux-readiness\.generated\.sh -- --install/);
  assert.equal((log.match(/linux-readiness\.generated\.sh/g) ?? []).length, 2);
  assert.match(
    log,
    /run --provider aws --target linux --id cbx_source --no-sync --shell -- \/usr\/local\/libexec\/crabbox\/linux-readiness\.generated\.sh/,
  );
  assert.match(
    log,
    /run --provider aws --target linux --id cbx_source --no-sync --shell -- set -euo pipefail/,
  );
  assert.equal((log.match(/corepack --version/g) ?? []).length, 3);
  assert.equal((log.match(/\n  offline_node_pnpm_probe\nfi\n}/g) ?? []).length, 3);
  assert.equal((log.match(/\ndeveloper_archive_probe\necho devtools-smoke-ok/g) ?? []).length, 3);
  assert.equal((log.match(/\npnpm --version\n/g) ?? []).length, 3);
  assert.match(log, /docker image inspect hello-world ubuntu:24\.04 node:24-bookworm/);
  assert.match(
    log,
    /env CRABBOX_AWS_REGION=us-west-2 AWS_REGION=us-west-2 CRABBOX_AWS_AMI= args checkpoint create --provider aws --target linux --id cbx_source --name crabbox-linux-devtools-/,
  );
  assert.match(log, /--mode native --strategy image --no-reboot=false --wait --wait-timeout 60m/);
  assert.match(
    log,
    /image promote --target linux --json --expected-current-image capture --region us-west-2 --fast-snapshot-restore --fsr-az us-west-2a ami-devtools/,
  );
});

for (const lease of ["cbx_source", "cbx_candidate", "cbx_promoted"]) {
  test(`AWS image mint stops at failed offline smoke on ${lease}`, async (t) => {
    const fake = await setupFakeCrabbox();
    t.after(() => rm(fake.dir, { recursive: true, force: true }));
    const result = await runScript(["--target", "linux", "--run"], {
      CRABBOX_BIN: fake.fake,
      CRABBOX_FAKE_LOG: fake.log,
      CRABBOX_FAKE_SMOKE_FAIL_LEASE: lease,
    });
    assert.equal(result.code, 73, result.stderr);
    assert.match(result.stderr, /offline artifact smoke failed/);
    assert.doesNotMatch(result.stdout, /promoted linux developer image passed/);
    const log = await readFile(fake.log, "utf8");
    assert.match(log, new RegExp(`stop --provider aws --target linux ${lease}`));
    if (lease === "cbx_source") assert.doesNotMatch(log, /checkpoint create/);
    if (lease !== "cbx_promoted") {
      assert.doesNotMatch(log, /image promote/);
    } else {
      assert.match(log, /image promote --json --target linux --restore-receipt \S+ ami-devtools/);
      assert.match(result.stderr, /restored previous default image=ami-previous/);
    }
  });
}

async function writeNodeVersionFixture(bin) {
  const file = path.join(bin, "node");
  await writeFile(
    file,
    `#!${process.execPath}
const vm = require("node:vm");
const args = process.argv.slice(2);
const version = process.env.FIXTURE_NODE_VERSION || "24.0.0";
if (args[0] === "--version") {
  console.log("v" + version);
} else {
  if (args[0] !== "-e") throw new Error("unexpected Node fixture invocation");
  const argv = args.slice(args[2] === "--" ? 3 : 2);
  vm.runInNewContext(args[1], {
    process: { version: "v" + version, versions: { node: version },
      argv: [process.execPath, ...argv], env: process.env },
    console,
  });
}
`,
  );
  await chmod(file, 0o755);
}

async function runNodeMajorMint(
  t,
  { major = "24", actual = "24.0.0", versions = {}, customPrep = false, ambient = "99" } = {},
) {
  const fake = await setupFakeCrabbox();
  t.after(() => rm(fake.dir, { recursive: true, force: true }));
  const bin = path.join(fake.dir, "bin");
  await mkdir(bin);
  await writeNodeVersionFixture(bin);
  const runner = path.join(fake.dir, "node-check.cjs");
  await writeFile(
    runner,
    `#!${process.execPath}
const { spawnSync } = require("node:child_process");
const [lease, source] = process.argv.slice(2);
const checks = source.split("\\n").filter(line => line.startsWith("node -e "));
if (checks.length !== 1) throw new Error("expected one generated Node version check");
const selections = source.split("\\n").filter(line => line.startsWith("expected_node_major="));
if (selections.length !== 1) throw new Error("expected one frozen Node selection");
const versions = JSON.parse(process.env.FIXTURE_NODE_VERSIONS);
const result = spawnSync("bash", ["-c", selections[0] + "\\n" + checks[0]], {
  env: {
    PATH: ${JSON.stringify(bin)} + ":" + process.env.PATH,
    HOME: ${JSON.stringify(fake.dir)},
    FIXTURE_NODE_VERSION: versions[lease] || process.env.FIXTURE_NODE_VERSION,
    CRABBOX_LINUX_NODE_MAJOR: process.env.FIXTURE_GUEST_NODE_MAJOR,
  },
  stdio: "inherit",
});
if (result.error) throw result.error;
if (result.status === 0) console.log("node-major-checked " + lease);
process.exit(result.status ?? 1);
`,
  );
  await chmod(runner, 0o755);
  const args = ["--target", "linux", "--run"];
  if (customPrep) args.push("--prep-script", fake.linuxPrep);
  const result = await runScript(args, {
    CRABBOX_BIN: fake.fake,
    CRABBOX_FAKE_LOG: fake.log,
    CRABBOX_IMAGE_LOG_DIR: path.join(fake.dir, "logs"),
    CRABBOX_FAKE_NODE_CHECK_RUNNER: runner,
    CRABBOX_LINUX_NODE_MAJOR: major,
    FIXTURE_NODE_VERSION: actual,
    FIXTURE_NODE_VERSIONS: JSON.stringify(versions),
    FIXTURE_GUEST_NODE_MAJOR: ambient,
  });
  return { fake, result, log: await readFile(fake.log, "utf8") };
}

for (const major of ["22", "26"]) {
  test(`Node-major smoke accepts selected ${major} through source, candidate, and promoted checks`, async (t) => {
    const { result, log } = await runNodeMajorMint(t, { major, actual: `${major}.0.0` });
    assert.equal(result.code, 0, result.stderr);
    for (const lease of ["cbx_source", "cbx_candidate", "cbx_promoted"]) {
      assert.match(result.stdout, new RegExp(`node-major-checked ${lease}`));
    }
    assert.match(log, /checkpoint create/);
    assert.match(log, /image promote/);
    assert.match(result.stdout, /promoted linux developer image passed/);
  });
}

for (const lease of ["cbx_source", "cbx_candidate", "cbx_promoted"]) {
  test(`Node-major smoke rejects mismatched selected 26 on ${lease}`, async (t) => {
    const { result, log } = await runNodeMajorMint(t, {
      major: "26",
      actual: "26.0.0",
      versions: { [lease]: "24.0.0" },
      ambient: "24",
    });
    assert.notEqual(result.code, 0);
    assert.match(result.stderr, /Node\.js.*26.*required.*v24\.0\.0/);
    assert.doesNotMatch(result.stdout, /promoted linux developer image passed/);
    assert.match(log, new RegExp(`stop --provider aws --target linux ${lease}`));
    if (lease === "cbx_source") assert.doesNotMatch(log, /checkpoint create/);
    if (lease !== "cbx_promoted") assert.doesNotMatch(log, /image promote/);
    else assert.match(log, /image promote --json --target linux --restore-receipt/);
  });
}

for (const [name, options, passes] of [
  ["default below 24", { actual: "22.0.0" }, false],
  ["default newer major", { actual: "26.0.0" }, true],
  ["custom prep ignores selected 22", { major: "22", actual: "26.0.0", customPrep: true }, true],
  [
    "custom prep still rejects below 24",
    { major: "22", actual: "22.0.0", customPrep: true },
    false,
  ],
  [
    "ambient major cannot replace selected 22",
    { major: "22", actual: "26.0.0", ambient: "26" },
    false,
  ],
  [
    "shell-sensitive selection stays data",
    { major: '22; touch "$HOME/injected"', actual: "26.0.0" },
    false,
  ],
]) {
  test(`Node-major smoke ${name}`, async (t) => {
    const { fake, result, log } = await runNodeMajorMint(t, options);
    assert.equal(result.code === 0, passes, result.stderr);
    if (passes) assert.match(result.stdout, /promoted linux developer image passed/);
    else {
      assert.match(result.stderr, /Node\.js.*required/);
      assert.doesNotMatch(log, /checkpoint create|image promote/);
    }
    await assert.rejects(readFile(path.join(fake.dir, "injected")), { code: "ENOENT" });
  });
}

async function linuxSmokeFixture(t, { major = "24", pnpm = "11.1.0", customPrep = false } = {}) {
  const fake = await setupFakeCrabbox();
  t.after(() => rm(fake.dir, { recursive: true, force: true }));
  const captured = path.join(fake.dir, "smoke.sh");
  const args = ["--target", "linux", "--run", "--no-promote"];
  if (customPrep) args.push("--prep-script", fake.linuxPrep);
  const result = await runScript(args, {
    CRABBOX_BIN: fake.fake, CRABBOX_FAKE_LOG: fake.log, CRABBOX_FAKE_CAPTURE_RUN_SCRIPT: captured,
    CRABBOX_LINUX_NODE_MAJOR: major, CRABBOX_LINUX_PNPM_VERSION: pnpm,
  });
  assert.equal(result.code, 0, result.stderr);
  const log = await readFile(fake.log, "utf8");
  if (customPrep) {
    assert.doesNotMatch(log, /builder overrides/);
  } else {
    assert.ok(log.includes(`builder overrides node=${major} pnpm=${pnpm}\n`));
    assert.match(log, /--allow-env CRABBOX_LINUX_NODE_MAJOR,CRABBOX_LINUX_PNPM_VERSION --script/);
  }
  const bin = path.join(fake.dir, "bin");
  await mkdir(bin);
  const tmp = path.join(fake.dir, "tmp");
  await mkdir(tmp);
  const writeTool = async (name, body) => {
    const file = path.join(bin, name);
    await writeFile(file, `#!/usr/bin/env bash\n${body}\n`);
    await chmod(file, 0o755);
  };
  for (const name of ["git", "gh", "jq", "rg", "fd", "npm", "trufflehog", "docker"]) {
    await writeTool(name, "exit 0");
  }
  await writeNodeVersionFixture(bin);
  await writeTool("corepack", 'exit "${FIXTURE_TOOL_EXIT:-0}"');
  await writeTool("id", 'printf "%s\\n" "$FIXTURE_UID"');
  await writeTool("dpkg", '[[ "$*" == "--print-architecture" ]] || exit 65\nprintf "%s\\n" "$FIXTURE_ARCH"');
  await writeTool("pnpm", '[[ "$*" == "--version" ]] || exit 65\nprintf "%s\\n" "$HOME" >>"$HOME/normal-pnpm.called"\nexit "${FIXTURE_PNPM_EXIT:-0}"');
  const generated = (await readFile(captured, "utf8"))
    .replace("test -d /var/cache/crabbox/pnpm", "true")
    .replace("test -f /var/lib/crabbox-readiness/linux.json", "true")
    .replace("test -f /var/lib/crabbox/image-ready", "true")
    .replace(/^public_toolchain_archive_dir=.*$/m, 'public_toolchain_archive_dir="$HOME/missing-archives"');
  const execute = (env = {}, source = generated) => spawnSync("bash", ["-c", source], {
    cwd: fake.dir,
    env: {
      PATH: `${bin}${path.delimiter}${process.env.PATH}`, HOME: fake.dir, TMPDIR: tmp,
      FIXTURE_UID: "1000", FIXTURE_ARCH: "amd64", FIXTURE_NODE_VERSION: `${major}.0.0`, ...env,
    },
    encoding: "utf8",
    timeout: 10_000,
  });
  return { fake, tmp, generated, execute };
}

test("generated Linux smoke emits success only after nonroot, tool, and offline artifact checks", async (t) => {
  const { fake, tmp, generated, execute } = await linuxSmokeFixture(t);
  for (const [env, diagnostic] of [
    [{ FIXTURE_UID: "0" }, /requires a nonroot user/],
    [{ FIXTURE_TOOL_EXIT: "46" }, null],
    [{}, /verified public toolchain archive unavailable offline/],
  ]) {
    const smoke = execute(env);
    assert.notEqual(smoke.status, 0, JSON.stringify(env));
    assert.doesNotMatch(smoke.stdout, /devtools-smoke-ok/);
    if (diagnostic) assert.match(smoke.stderr, diagnostic);
    assert.deepEqual(await readdir(tmp), [], "failed smoke must remove private staging");
  }
  await mkdir(path.join(fake.dir, "missing-archives"));
  await writeFile(path.join(fake.dir, "missing-archives", "node-v24.19.0-linux-x64.tar.xz"), "corrupt");
  await writeFile(path.join(fake.dir, "missing-archives", "x64.complete"), "");
  const corrupt = execute({ CRABBOX_LINUX_NODE_MAJOR: "26" });
  assert.notEqual(corrupt.status, 0);
  assert.match(corrupt.stderr, /checksum mismatch/);
  assert.doesNotMatch(corrupt.stdout, /devtools-smoke-ok/);
  assert.deepEqual(await readdir(tmp), []);
  // The real artifact probe is covered separately; this assertion owns marker ordering.
  const successful = execute({}, generated.replace(/\n[ \t]*offline_node_pnpm_probe\n/, "\nprintf 'offline-proof-done\\n'\n"));
  assert.equal(successful.status, 0, successful.stderr);
  assert.match(successful.stdout, /offline-proof-done\ndevtools-smoke-ok\n$/);
});

for (const route of [
  { name: "ARM guest", arch: "arm64" },
  { name: "ARM selected Node 22", arch: "arm64", major: "22" },
  { name: "Node major override", arch: "amd64", major: "26" },
  { name: "custom prep", arch: "amd64", customPrep: true },
]) {
  test(`generated Linux smoke preserves the ${route.name} route without x64 archives`, async (t) => {
    const { fake, execute } = await linuxSmokeFixture(t, route);
    const result = execute({ FIXTURE_ARCH: route.arch, CRABBOX_LINUX_NODE_MAJOR: "24" });
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /devtools-smoke-ok/);
    assert.equal((await readFile(path.join(fake.dir, "normal-pnpm.called"), "utf8")).trim(), fake.dir);
  });
}

test("generated Linux smoke requires the normal nonroot pnpm command even when private probes pass", async (t) => {
  const { fake, generated, execute } = await linuxSmokeFixture(t);
  const result = execute(
    { FIXTURE_PNPM_EXIT: "48" },
    generated.replace(/\n[ \t]*offline_node_pnpm_probe\n/, "\ntrue\n"),
  );
  assert.equal(result.status, 48, result.stderr);
  assert.doesNotMatch(result.stdout, /devtools-smoke-ok/);
  assert.equal((await readFile(path.join(fake.dir, "normal-pnpm.called"), "utf8")).trim(), fake.dir);
});

test("generated Linux smoke keeps required x64 archives under a pnpm override", async (t) => {
  const { execute } = await linuxSmokeFixture(t, { pnpm: "12.3.4" });
  const result = execute();
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /verified public toolchain archive unavailable offline/);
  assert.doesNotMatch(result.stdout, /devtools-smoke-ok/);
});

test("AWS devtools mint wrapper isolates warmup logs from explicit image names", async () => {
  const logDir = await mkdtemp(path.join(os.tmpdir(), "crabbox-aws-image-mint-logs-"));
  for (let i = 0; i < 2; i += 1) {
    const fake = await setupFakeCrabbox();
    const result = await runScript(
      [
        "--target",
        "linux",
        "--run",
        "--no-promote",
        "--name",
        "shared-devtools",
        "--prep-script",
        fake.linuxPrep,
      ],
      {
        CRABBOX_BIN: fake.fake,
        CRABBOX_FAKE_LOG: fake.log,
        CRABBOX_IMAGE_LOG_DIR: logDir,
        CRABBOX_IMAGE_WINDOWS_WARMUP_SETTLE_SECONDS: "0",
        CRABBOX_IMAGE_REBOOT_READY_SETTLE_SECONDS: "0",
        CRABBOX_IMAGE_PREP_WAIT_TIMEOUT: "5s",
      },
    );
    assert.equal(result.code, 0, result.stderr);
  }

  const files = (await readdir(logDir)).filter((name) => name.startsWith("image-mint-"));
  assert.equal(files.length, 4);
  assert.equal(new Set(files).size, 4);
  for (const file of files) {
    assert.match(file, /^image-mint-shared-devtools-(source|candidate)-/);
  }
});

test("AWS devtools mint wrapper fails when promoted selection is not proved", async () => {
  const fake = await setupFakeCrabbox();
  const text = await readFile(fake.fake, "utf8");
  await writeFile(
    fake.fake,
    text.replace(
      "image selected id=ami-devtools source=promoted",
      "image selected id=ami-other source=promoted",
    ),
  );
  const result = await runScript(["--target", "linux", "--run", "--prep-script", fake.linuxPrep], {
    CRABBOX_BIN: fake.fake,
    CRABBOX_FAKE_LOG: fake.log,
  });
  assert.equal(result.code, 1, result.stderr);
  assert.match(
    result.stderr,
    /warmup did not prove image selection id=ami-devtools source=promoted/,
  );
  const log = await readFile(fake.log, "utf8");
  assert.match(log, /image promote --json --target linux --restore-receipt \S+ ami-devtools/);
  assert.match(result.stderr, /restored previous default image=ami-previous/);
});

test("AWS devtools mint wrapper reports rollback rejection without hiding smoke failure", async () => {
  const fake = await setupFakeCrabbox();
  const text = await readFile(fake.fake, "utf8");
  await writeFile(
    fake.fake,
    text.replace(
      "image selected id=ami-devtools source=promoted",
      "image selected id=ami-other source=promoted",
    ),
  );
  const result = await runScript(["--target", "linux", "--run", "--prep-script", fake.linuxPrep], {
    CRABBOX_BIN: fake.fake,
    CRABBOX_FAKE_LOG: fake.log,
    CRABBOX_FAKE_ROLLBACK_FAIL: "1",
  });

  assert.equal(result.code, 1, result.stderr);
  assert.match(
    result.stderr,
    /CAS rollback failed or was rejected; a newer default was not overwritten/,
  );
});

test("AWS devtools mint wrapper clears a newly introduced default after smoke failure", async () => {
  const fake = await setupFakeCrabbox();
  const text = await readFile(fake.fake, "utf8");
  await writeFile(
    fake.fake,
    text.replace(
      "image selected id=ami-devtools source=promoted",
      "image selected id=ami-other source=promoted",
    ),
  );
  const result = await runScript(["--target", "linux", "--run", "--prep-script", fake.linuxPrep], {
    CRABBOX_BIN: fake.fake,
    CRABBOX_FAKE_LOG: fake.log,
    CRABBOX_FAKE_PREVIOUS_ABSENT: "1",
  });

  assert.equal(result.code, 1, result.stderr);
  const log = await readFile(fake.log, "utf8");
  assert.match(log, /image promote --json --target linux --restore-receipt \S+ ami-devtools/);
});

test("AWS devtools mint wrapper finishes fallible candidate cleanup before promotion", async () => {
  const fake = await setupFakeCrabbox();
  const result = await runScript(["--target", "linux", "--run", "--prep-script", fake.linuxPrep], {
    CRABBOX_BIN: fake.fake,
    CRABBOX_FAKE_LOG: fake.log,
    CRABBOX_FAKE_STOP_FAIL_LEASE: "cbx_candidate",
  });

  assert.equal(result.code, 27, result.stderr);
  const log = await readFile(fake.log, "utf8");
  assert.doesNotMatch(log, /image promote/);
});

test("AWS devtools mint wrapper preserves promotion failure while attempting receipt recovery", async () => {
  const fake = await setupFakeCrabbox();
  const result = await runScript(["--target", "linux", "--run", "--prep-script", fake.linuxPrep], {
    CRABBOX_BIN: fake.fake,
    CRABBOX_FAKE_LOG: fake.log,
    CRABBOX_FAKE_PROMOTION_FAIL: "1",
  });

  assert.equal(result.code, 55, result.stderr);
  assert.match(result.stderr, /transactional promotion receipt is unavailable for rollback/);
});

test("AWS devtools mint wrapper uses sg for first docker group member", async () => {
  const fake = await setupFakeCrabbox();
  const smokeScript = path.join(fake.dir, "smoke.sh");
  const result = await runScript(
    ["--target", "linux", "--run", "--no-promote", "--prep-script", fake.linuxPrep],
    {
      CRABBOX_BIN: fake.fake,
      CRABBOX_FAKE_LOG: fake.log,
      CRABBOX_FAKE_CAPTURE_RUN_SCRIPT: smokeScript,
    },
  );
  assert.equal(result.code, 0, result.stderr);

  const bin = path.join(fake.dir, "smoke-bin");
  await mkdir(bin);
  const sgMarker = path.join(fake.dir, "sg-used");
  const sudoMarker = path.join(fake.dir, "sudo-used");
  const writeTool = async (name, body) => {
    const file = path.join(bin, name);
    await writeFile(file, body);
    await chmod(file, 0o755);
  };
  for (const name of [
    "git",
    "gh",
    "jq",
    "rg",
    "fd",
    "python3",
    "npm",
    "corepack",
    "pnpm",
    "trufflehog",
  ]) {
    await writeTool(name, "#!/usr/bin/env bash\nexit 0\n");
  }
  await writeTool(
    "node",
    '#!/usr/bin/env bash\n[[ "${1:-}" == "--version" ]] && printf \'v24.0.0\\n\'\nexit 0\n',
  );
  await writeTool(
    "id",
    '#!/usr/bin/env bash\ncase "$*" in -nG) printf \'users\\n\';; -u) printf \'1000\\n\';; esac\n',
  );
  await writeTool("whoami", "#!/usr/bin/env bash\nprintf 'alice\\n'\n");
  await writeTool(
    "getent",
    '#!/usr/bin/env bash\n[[ "$*" == "group docker" ]] && printf \'docker:x:999:alice,bob\\n\'\n',
  );
  await writeTool(
    "docker",
    `#!/usr/bin/env bash
if [[ "\${CRABBOX_FAKE_IN_SG:-0}" == "1" ]]; then
  exit 0
fi
exit 1
`,
  );
  await writeTool(
    "sg",
    `#!/usr/bin/env bash
touch "${sgMarker}"
shift
[[ "\${1:-}" == "-c" ]] || exit 64
shift
CRABBOX_FAKE_IN_SG=1 bash -c "$1"
`,
  );
  await writeTool(
    "sudo",
    `#!/usr/bin/env bash
touch "${sudoMarker}"
exit 80
`,
  );

  const generated = (await readFile(smokeScript, "utf8"))
    .replace("test -d /var/cache/crabbox/pnpm", "true")
    .replace("test -f /var/lib/crabbox-readiness/linux.json", "true")
    .replace("test -f /var/lib/crabbox/image-ready", "true")
    // Artifact consumption has its own real-archive proof; this fixture owns group fallback.
    .replace(/\noffline_node_pnpm_probe\n/, "\ntrue\n");
  const smoke = await new Promise((resolve, reject) => {
    const child = spawn("bash", ["-c", generated], {
      cwd: repoRoot,
      env: {
        ...process.env,
        PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");
    child.stdout.on("data", (chunk) => {
      stdout += chunk;
    });
    child.stderr.on("data", (chunk) => {
      stderr += chunk;
    });
    child.on("error", reject);
    child.on("close", (code) => resolve({ code, stdout, stderr }));
  });

  assert.equal(smoke.code, 0, smoke.stderr || smoke.stdout);
  assert.equal(await readFile(sgMarker, "utf8"), "");
  await assert.rejects(readFile(sudoMarker, "utf8"));
});

test("AWS devtools mint wrapper maps windows flags", async () => {
  const fake = await setupFakeCrabbox();
  const result = await runScript(
    [
      "--target",
      "windows",
      "--region",
      "us-east-1",
      "--type",
      "m7i.large",
      "--windows-mode",
      "normal",
      "--run",
      "--no-promote",
      "--prep-script",
      fake.windowsPrep,
    ],
    {
      CRABBOX_BIN: fake.fake,
      CRABBOX_FAKE_LOG: fake.log,
      CRABBOX_IMAGE_WINDOWS_WARMUP_SETTLE_SECONDS: "0",
    },
  );
  assert.equal(result.code, 0, result.stderr);
  const log = await readFile(fake.log, "utf8");
  assert.match(
    log,
    /env CRABBOX_AWS_REGION=us-east-1 AWS_REGION=us-east-1 CRABBOX_AWS_AMI= args warmup --provider aws --target windows/,
  );
  assert.match(log, /--windows-mode normal/);
  assert.doesNotMatch(log, /--desktop/);
  assert.doesNotMatch(log, /--browser/);
  assert.doesNotMatch(log, /warmup .*--region us-east-1/);
  assert.match(
    log,
    /run --provider aws --target windows --id cbx_source --no-sync --shell -- Write-Output "windows-ssh-ready"/,
  );
  assert.match(
    log,
    /run --provider aws --target windows --id cbx_source --no-sync --shell -- New-Item/,
  );
  assert.match(
    log,
    /run --provider aws --target windows --id cbx_source --no-sync --shell -- Set-Content/,
  );
  assert.match(log, /FromBase64String/);
  assert.doesNotMatch(log, /image promote/);
});

test("AWS devtools mint wrapper reboots windows source when prep requires it", async () => {
  const fake = await setupFakeCrabbox();
  const result = await runScript(
    [
      "--target",
      "windows",
      "--region",
      "us-east-1",
      "--type",
      "m7i.large",
      "--run",
      "--no-promote",
      "--prep-script",
      fake.windowsPrep,
    ],
    {
      CRABBOX_BIN: fake.fake,
      CRABBOX_FAKE_LOG: fake.log,
      CRABBOX_FAKE_WINDOWS_REBOOT: "1",
      CRABBOX_IMAGE_REBOOT_SETTLE_SECONDS: "0",
      CRABBOX_IMAGE_REBOOT_READY_SETTLE_SECONDS: "0",
      CRABBOX_IMAGE_WINDOWS_WARMUP_SETTLE_SECONDS: "0",
      CRABBOX_IMAGE_PREP_WAIT_TIMEOUT: "5s",
    },
  );
  assert.equal(result.code, 0, result.stderr);
  const log = await readFile(fake.log, "utf8");
  assert.match(
    log,
    /run --provider aws --target windows --id cbx_source --no-sync --shell -- if \(Test-Path/,
  );
  assert.match(
    log,
    /run --provider aws --target windows --id cbx_source --no-sync --shell -- shutdown \/r \/t 5 \/f/,
  );
  assert.match(
    log,
    /run --provider aws --target windows --id cbx_source --no-sync --shell -- Write-Output "windows-ssh-ready"/,
  );
  assert.match(
    log,
    /run --provider aws --target windows --id cbx_source --no-sync --shell -- Set-Content/,
  );
  assert.match(log, /FromBase64String/);
});

test("AWS devtools mint wrapper retries windows prep upload disconnects", async () => {
  const fake = await setupFakeCrabbox();
  const result = await runScript(
    [
      "--target",
      "windows",
      "--region",
      "us-east-1",
      "--type",
      "m7i.large",
      "--run",
      "--no-promote",
      "--prep-script",
      fake.windowsPrep,
    ],
    {
      CRABBOX_BIN: fake.fake,
      CRABBOX_FAKE_LOG: fake.log,
      CRABBOX_FAKE_WINDOWS_PREP_DISCONNECT: "1",
      CRABBOX_FAKE_WINDOWS_REBOOT: "1",
      CRABBOX_IMAGE_REBOOT_SETTLE_SECONDS: "0",
      CRABBOX_IMAGE_REBOOT_READY_SETTLE_SECONDS: "0",
      CRABBOX_IMAGE_WINDOWS_WARMUP_SETTLE_SECONDS: "0",
      CRABBOX_IMAGE_PREP_WAIT_TIMEOUT: "5s",
    },
  );
  assert.equal(result.code, 0, result.stderr);
  assert.match(result.stderr, /Windows command failed during prep upload image-prep\.part-/);
  const log = await readFile(fake.log, "utf8");
  assert.match(
    log,
    /run --provider aws --target windows --id cbx_source --no-sync --shell -- Write-Output "windows-ssh-ready"/,
  );
  assert.match(
    log,
    /run --provider aws --target windows --id cbx_source --no-sync --shell -- if \(Test-Path/,
  );
  assert.match(
    log,
    /run --provider aws --target windows --id cbx_source --no-sync --shell -- shutdown \/r \/t 5 \/f/,
  );
  assert.match(
    log,
    /checkpoint create --provider aws --target windows --id cbx_source --name crabbox-windows-devtools-/,
  );
});

test("AWS devtools mint wrapper cleans up lease when warmup fails after allocation", async () => {
  const fake = await setupFakeCrabbox();
  const result = await runScript(["--target", "linux", "--run", "--prep-script", fake.linuxPrep], {
    CRABBOX_BIN: fake.fake,
    CRABBOX_FAKE_LOG: fake.log,
    CRABBOX_FAKE_WARMUP_FAIL_AFTER_LEASE: "1",
  });
  assert.equal(result.code, 23, result.stderr);
  const log = await readFile(fake.log, "utf8");
  assert.match(log, /warmup --provider aws --target linux/);
  assert.match(log, /stop --provider aws --target linux cbx_source/);
});

async function measuredFixture(t) {
  const fake = await setupFakeCrabbox();
  t.after(() => rm(fake.dir, { recursive: true, force: true }));
  const root = path.join(fake.dir, "source");
  for (const file of [
    "scripts/mint-aws-devtools-image.sh",
    "scripts/devtools-image-proof.mjs",
    "scripts/devtools-image-contract.mjs",
    "scripts/devtools-image-smoke-linux.sh",
    "scripts/devtools-image-smoke-windows.ps1",
    "scripts/generate-linux-readiness.mjs",
    "scripts/install-linux-developer-tools.sh",
    "scripts/linux-readiness.generated.sh",
    "recipes/devtools/v1/linux-x86_64.json",
    "recipes/devtools/v1/recipe.schema.json",
  ]) {
    await mkdir(path.dirname(path.join(root, file)), { recursive: true });
    await copyFile(path.join(repoRoot, file), path.join(root, file));
  }
  const git = (args) => execFileSync("git", ["-C", root, ...args], { encoding: "utf8" }).trim();
  git(["init", "--quiet"]);
  git(["add", "."]);
  git([
    "-c",
    "user.name=Fixture",
    "-c",
    "user.email=fixture@example.invalid",
    "-c",
    "commit.gpgsign=false",
    "commit",
    "--quiet",
    "-m",
    "fixture",
  ]);
  const mock = path.join(fake.dir, "measurement.mjs");
  await writeFile(
    mock,
    `
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
const args = process.argv.slice(2);
const option = name => args[args.indexOf(name) + 1];
const hash = value => "sha256:" + createHash("sha256").update(JSON.stringify(value)).digest("hex");
const leaseFile = process.env.CRABBOX_FAKE_LOG + ".leases.json";
const leases = fs.existsSync(leaseFile) ? JSON.parse(fs.readFileSync(leaseFile, "utf8")) : [];
if (args[0] === "admin") {
  const mode = process.env.CRABBOX_FAKE_SELECTION_MODE;
  if (mode === "admin-failure") process.exit(53);
  if (mode === "missing-selection") leases.pop();
  if (mode === "duplicate-selection") leases.push(leases.at(-1));
  fs.writeSync(1, JSON.stringify(leases) + "\\n");
  process.exit(0);
}
const saveLease = (phase, sample, leaseId, index) => {
  const selection = {
    id: phase === "baseline" ? "ami-previous" : "ami-devtools",
    source: phase === "candidate" ? "explicit" : "promoted",
    region: process.env.CRABBOX_AWS_REGION, kind: "aws-ami",
    promotedAt: "2026-09-01T00:00:00Z",
  };
  if (phase === "baseline") {
    selection.source = process.env.CRABBOX_FAKE_BASELINE_SOURCE ||
      (process.env.CRABBOX_FAKE_PREVIOUS_ABSENT === "1" ? "stock" : "promoted");
  }
  const mode = process.env.CRABBOX_FAKE_SELECTION_MODE;
  if (phase === "baseline" && sample === 2 && mode === "baseline-image") selection.id = "ami-other";
  if (phase === "baseline" && sample === 2 && mode === "baseline-source") selection.source = "stock";
  if (mode === phase + "-region") selection.region = "us-east-1";
  if (phase === "candidate" && mode === "candidate-image") selection.id = "ami-other";
  if (phase === "promoted" && process.env.CRABBOX_FAKE_WARMUP_WRONG_IMAGE === "1") selection.id = "ami-other";
  if (mode === "coordinator-override") selection.source = "explicit";
  if (selection.source !== "promoted") delete selection.promotedAt;
  const row = {id: leaseId, provider: "aws", target: "linux",
    region: selection.region, serverType: "m7i.large",
    cloudID: "i-" + index.toString(16).padStart(17, "0"), image: selection,
    owner: "PRIVATE_OWNER_POISON", host: "PRIVATE_HOST_POISON"};
  if (phase === "candidate" && mode === "reused-instance") row.cloudID = leases[0].cloudID;
  if (phase === "candidate" && mode === "image-region") row.image.region = "us-east-1";
  leases.push(row);
  fs.writeFileSync(leaseFile, JSON.stringify(leases));
};
if (args[0] === "warmup") {
  const countFile = process.env.CRABBOX_FAKE_LOG + ".count";
  const count = fs.existsSync(countFile) ? Number(fs.readFileSync(countFile, "utf8")) : 0;
  saveLease(["baseline", "candidate", "promoted"][count], 1,
    ["cbx_source", "cbx_candidate", "cbx_promoted"][count], 100 + count);
  process.exit(0);
}
const store = option(args[0] === "run" ? "--timing-record" : "--store");
const phase = path.basename(store, ".jsonl");
let records = fs.existsSync(store) ? fs.readFileSync(store, "utf8").trim().split("\\n").filter(Boolean).map(JSON.parse) : [];
if (args[0] === "run") {
  const offset = {baseline: 0, candidate: 3, promoted: 6}[phase];
  const sample = records.length + 1;
  const failed = phase === process.env.CRABBOX_FAKE_RUN_FAIL_PHASE && sample === 2;
  const leaseId = "cbx_" + (offset + sample).toString(16).padStart(12, "0");
  const runId = "run_" + (offset + sample);
  const record = {schemaVersion: 1, source: "run", benchmark: {
    repoHead: process.env.CRABBOX_FAKE_SOURCE_HEAD, repoFingerprint: hash("fixture"),
    commandFingerprint: hash(["true"]), coldRun: true
  }, timing: {provider: "aws", machineType: "m7i.large",
    leaseId, runId,
    exitCode: failed ? 41 : 0, runnerTotalMs: 1000, syncMs: 0, syncSkipped: false, leaseStopped: false}};
  if (args.includes("--lease-output") && !(failed && process.env.CRABBOX_FAKE_FAILED_HANDLE_MISSING === "1")) {
    fs.writeFileSync(option("--lease-output"), JSON.stringify({
      provider: "aws", leaseId, runId, reused: false, kept: true,
      cleanupCommand: "crabbox stop " + leaseId,
    }));
  }
  if (phase === process.env.CRABBOX_FAKE_INTERRUPT_PHASE && sample === 2) {
    console.log("retained-handle-ready");
    await new Promise(resolve => setTimeout(resolve, 200));
    process.exit(0);
  }
  if (phase === "candidate") {
    if (process.env.CRABBOX_FAKE_TIMING_MODE === "missing") delete record.timing.runnerTotalMs;
    if (process.env.CRABBOX_FAKE_TIMING_MODE === "mixed") record.benchmark.repoHead = "c".repeat(40);
  }
  if (!(phase === "candidate" && process.env.CRABBOX_FAKE_TIMING_MODE === "insufficient" && records.length === 2) &&
      !(failed && process.env.CRABBOX_FAKE_FAILED_RECORD_MISSING === "1")) {
    fs.appendFileSync(store, JSON.stringify(record) + "\\n");
  }
  saveLease(phase, sample, leaseId, offset + sample);
  console.log("image selected id=ami-requested source=promoted kind=aws-ami region=us-west-2 promoted_at=-");
  if (failed) process.exit(41);
} else {
  const group = {source: "run", provider: "aws", machineType: "m7i.large",
    commandFingerprint: hash(["true"]), coldRun: true, n: records.length,
    observationCount: records.length, failureCount: 0, runnerTotalN: records.filter(r => r.timing.runnerTotalMs > 0).length,
    p95RunnerTotalMs: 1000, medianRunnerTotalMs: 1000, medianSyncMs: 0};
  if (args[1] === "report") console.log(JSON.stringify({schemaVersion: 1, observationCount: records.length, matchedCount: records.length, groups: [group]}));
  else {
    const fail = process.env.CRABBOX_FAKE_CHECK_FAIL_PHASE === phase;
    console.log(JSON.stringify({schemaVersion: 1, matchedCount: records.length, groupCount: 1,
      passed: !fail, reasons: [], policy: {minSamples: 3, requiredRunnerTotalSamples: 3, maxFailures: 0, maxP95RunnerTotal: "10m0s"},
      groups: [{...group, successfulSamples: records.length, passed: !fail, reasons: []}]}));
    if (fail) process.exit(37);
  }
}
`,
  );
  const original = await readFile(fake.fake, "utf8");
  await writeFile(
    fake.fake,
    original.replace(
      'case "$1" in',
      `
if [[ "$1" == config ]]; then
  printf '{"provider":"aws","aws":{"region":"%s","ami":"%s"},"coordinator":"https://coordinator.example.invalid","brokerMode":"managed","brokerAuth":"configured","brokerAdminAuth":"configured","sshKey":"PRIVATE_CONFIG_POISON"}\\n' "$CRABBOX_AWS_REGION" "\${CRABBOX_FAKE_CONFIG_AMI:-}"
  exit 0
fi
if [[ "$1" == warmup ]]; then
  node "$CRABBOX_FAKE_MEASUREMENT" "$@"
fi
if [[ "$1" == bench || "$1" == admin || " $* " == *" --timing-record "* ]]; then
  node "$CRABBOX_FAKE_MEASUREMENT" "$@"
  exit $?
fi
case "$1" in`,
    ),
  );
  const env = {
    CRABBOX_BIN: fake.fake,
    CRABBOX_FAKE_LOG: fake.log,
    CRABBOX_FAKE_MEASUREMENT: mock,
    CRABBOX_FAKE_SOURCE_HEAD: git(["rev-parse", "HEAD"]),
    CRABBOX_IMAGE_LOG_DIR: path.join(fake.dir, "diagnostics"),
    CRABBOX_IMAGE_PUBLIC_OUTCOME: path.join(fake.dir, "public", "manifest.json"),
    CRABBOX_OWNER: "publisher@example.invalid",
    CRABBOX_ORG: "example-org",
  };
  const args = [
    "--target",
    "linux",
    "--region",
    "us-west-2",
    "--type",
    "m7i.large",
    "--measured",
    "--max-p95-runner-total-ms",
    "600000",
    "--run",
  ];
  return {
    ...fake,
    root,
    env,
    args,
    script: path.join(root, "scripts/mint-aws-devtools-image.sh"),
    outcome: env.CRABBOX_IMAGE_PUBLIC_OUTCOME,
  };
}

for (const failure of ["missing smoke owner", "archive probe rendering"]) {
  test(`AWS mint stops before capture when ${failure} fails`, async (t) => {
    const fake = await measuredFixture(t);
    if (failure === "missing smoke owner") {
      await rm(path.join(fake.root, "scripts/devtools-image-smoke-linux.sh"));
    } else {
      const installer = path.join(fake.root, "scripts/install-linux-developer-tools.sh");
      await writeFile(
        installer,
        (await readFile(installer, "utf8")) +
          "\nnode_pnpm_smoke_script() { echo partial-probe; return 47; }\n",
      );
    }
    const result = await runScript(["--target", "linux", "--run", "--no-promote"], fake.env, fake.script);
    assert.equal(result.code, failure === "missing smoke owner" ? 1 : 47, result.stderr);
    const log = await readFile(fake.log, "utf8");
    assert.match(log, /stop --provider aws --target linux cbx_source/);
    assert.doesNotMatch(log, /checkpoint create|image promote|docker_probe=|partial-probe/);
  });
}

test("measured Linux publication runs nine fresh samples and publishes only allowlisted proof", async (t) => {
  const fake = await measuredFixture(t);
  const result = await runScript(
    fake.args,
    { ...fake.env, CRABBOX_AWS_AMI: "ami-ambient" },
    fake.script,
  );
  assert.equal(result.code, 0, result.stderr);
  assert.match(result.stderr, /12 planned leases/);
  const log = await readFile(fake.log, "utf8");
  assert.equal((log.match(/args warmup /g) ?? []).length, 3);
  assert.equal((log.match(/args run .*--timing-record /g) ?? []).length, 9);
  assert.equal((log.match(/args bench check /g) ?? []).length, 2);
  assert.equal(
    (log.match(/args stop --provider aws --target linux cbx_[0-9a-f]{12}/g) ?? []).length,
    9,
  );
  assert.match(log, /--full-resync --no-hydrate --keep --stop-after never --lease-output/);
  assert.doesNotMatch(log, /CRABBOX_AWS_AMI=ami-ambient/);
  const manifestPath = result.stdout.match(/public measurement proof: (.+)/)?.[1];
  assert.equal(manifestPath, fake.outcome);
  const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
  assert.equal(manifest.schema, "crabbox-devtools-image-proof/v2");
  assert.equal(manifest.status, "passed");
  assert.equal(manifest.stage, "complete");
  assert.equal(manifest.plannedLeaseCount, 12);
  assert.equal(manifest.cohorts[0].medianSyncMs, 0);
  assert.equal(manifest.cohorts[0].policyApplied, false);
  assert.equal(manifest.cohorts[0].policyPassed, null);
  assert.equal(manifest.cohorts[1].policyPassed, true);
  assert.equal(manifest.cohorts[2].policyPassed, true);
  assert.match(manifest.promotionBindingDigest, /^sha256:[0-9a-f]{64}$/);
  assert.doesNotMatch(JSON.stringify(manifest), /ami-devtools|cbx_|m7i|us-west|diagnostics/);
  assert.doesNotMatch(result.stdout + result.stderr, /PRIVATE_(OWNER|HOST|CONFIG)_POISON|--owner/);
  for (const file of await readdir(path.dirname(manifestPath))) {
    if (file.endsWith(".selection.json")) {
      assert.doesNotMatch(
        await readFile(path.join(path.dirname(manifestPath), file), "utf8"),
        /PRIVATE_/,
      );
    }
  }
});

test("measured baseline is descriptive while candidate and promoted cohorts enforce policy", async (t) => {
  const baseline = await measuredFixture(t);
  const baselineResult = await runScript(
    baseline.args,
    { ...baseline.env, CRABBOX_FAKE_CHECK_FAIL_PHASE: "baseline" },
    baseline.script,
  );
  assert.equal(baselineResult.code, 0, baselineResult.stderr);

  const candidate = await measuredFixture(t);
  const candidateResult = await runScript(
    candidate.args,
    { ...candidate.env, CRABBOX_FAKE_CHECK_FAIL_PHASE: "candidate" },
    candidate.script,
  );
  assert.equal(candidateResult.code, 37, candidateResult.stderr);
  const candidateOutcome = JSON.parse(await readFile(candidate.outcome, "utf8"));
  assert.equal(candidateOutcome.status, "failed");
  assert.equal(candidateOutcome.stage, "candidate_measure");
  assert.equal(candidateOutcome.cohorts[0].policyApplied, false);
  assert.equal(candidateOutcome.cohorts[0].policyPassed, null);
  assert.equal(candidateOutcome.cohorts[1].policyApplied, true);
  assert.equal(candidateOutcome.cohorts[1].policyPassed, false);
  assert.doesNotMatch(await readFile(candidate.log, "utf8"), /image promote/);
});

test("measured invalid recipe and changed prep fail before any CLI operation", async (t) => {
  for (const mode of ["recipe", "prep", "override"]) {
    await t.test(mode, async (t) => {
      const fake = await measuredFixture(t);
      if (mode === "recipe")
        await writeFile(
          path.join(fake.root, "scripts/install-linux-developer-tools.sh"),
          "changed",
        );
      const args = mode === "prep" ? [...fake.args, "--prep-script", fake.linuxPrep] : fake.args;
      const env = mode === "override" ? { ...fake.env, CRABBOX_LINUX_NODE_MAJOR: "26" } : fake.env;
      const result = await runScript(args, env, fake.script);
      assert.notEqual(result.code, 0);
      assert.match(
        result.stderr,
        mode === "recipe"
          ? /SHA-256 mismatch/
          : mode === "prep"
            ? /measured prep must use the bundled recipe/
            : /does not permit Linux installer overrides/,
      );
      await assert.rejects(readFile(fake.log, "utf8"));
    });
  }
});

test("measured planning makes no CLI calls and rejects incompatible execution inputs", async (t) => {
  const fake = await measuredFixture(t);
  const dry = await runScript(
    fake.args.filter((arg) => arg !== "--run"),
    fake.env,
    fake.script,
  );
  assert.equal(dry.code, 0, dry.stderr);
  assert.match(dry.stderr, /12 planned leases/);
  assert.match(dry.stderr, /no hard attempt or dollar cap is enforced/);
  await assert.rejects(readFile(fake.log, "utf8"));
  for (const extra of [
    ["--max-p95-runner-total-ms", "0"],
    ["--no-browser"],
    ["--no-desktop"],
    ["--keep-lease"],
    ["--no-promote"],
    ["--fast-snapshot-restore"],
    ["--target", "windows"],
  ]) {
    const result = await runScript([...fake.args, ...extra], fake.env, fake.script);
    assert.notEqual(result.code, 0);
    await assert.rejects(readFile(fake.log, "utf8"));
  }
  await writeFile(path.join(fake.root, "untracked-input"), "dirty");
  const dirty = await runScript(fake.args, fake.env, fake.script);
  assert.notEqual(dirty.code, 0);
  assert.match(dirty.stderr, /measured source must be clean/);
  await assert.rejects(readFile(fake.log, "utf8"));
});

test("measured incomplete or mixed cohorts block promotion", async (t) => {
  for (const mode of ["missing", "mixed", "insufficient"]) {
    await t.test(mode, async (t) => {
      const fake = await measuredFixture(t);
      const result = await runScript(
        fake.args,
        { ...fake.env, CRABBOX_FAKE_TIMING_MODE: mode },
        fake.script,
      );
      assert.notEqual(result.code, 0);
      assert.doesNotMatch(await readFile(fake.log, "utf8"), /image promote/);
      const outcome = JSON.parse(await readFile(fake.outcome, "utf8"));
      assert.equal(outcome.status, "failed");
      assert.equal(outcome.stage, "candidate_measure");
      assert.equal(outcome.cleanupStatus, "succeeded");
      assert.doesNotMatch(
        JSON.stringify(outcome),
        /ami-|cbx_|m7i|us-west|PRIVATE_|diagnostics/,
      );
    });
  }
});

test("measured candidate teardown failure blocks transactional promotion", async (t) => {
  const fake = await measuredFixture(t);
  const result = await runScript(
    fake.args,
    { ...fake.env, CRABBOX_FAKE_STOP_FAIL_LEASE: "cbx_candidate" },
    fake.script,
  );
  assert.equal(result.code, 27, result.stderr);
  assert.doesNotMatch(await readFile(fake.log, "utf8"), /image promote/);
});

test("measured pending cleanup must be confirmed before another sample or promotion", async (t) => {
  const fake = await measuredFixture(t);
  const result = await runScript(
    fake.args,
    { ...fake.env, CRABBOX_FAKE_STOP_FAIL_LEASE: "cbx_000000000004" },
    fake.script,
  );
  assert.equal(result.code, 27, result.stderr);
  const log = await readFile(fake.log, "utf8");
  assert.doesNotMatch(log, /image promote/);
  assert.equal((log.match(/args run .*--timing-record /g) ?? []).length, 4);
});

test("measured failed runs recover only the attempted lease and preserve the run failure", async (t) => {
  for (const mode of [
    "pending",
    "stop-failure",
    "missing-record",
    "missing-handle",
    "missing-both",
  ]) {
    await t.test(mode, async (t) => {
      const fake = await measuredFixture(t);
      const result = await runScript(
        fake.args,
        {
          ...fake.env,
          CRABBOX_FAKE_RUN_FAIL_PHASE: "baseline",
          CRABBOX_FAKE_STOP_FAIL_LEASE: mode === "stop-failure" ? "cbx_000000000002" : "",
          CRABBOX_FAKE_FAILED_RECORD_MISSING: ["missing-record", "missing-both"].includes(mode)
            ? "1"
            : "",
          CRABBOX_FAKE_FAILED_HANDLE_MISSING: ["missing-handle", "missing-both"].includes(mode)
            ? "1"
            : "",
        },
        fake.script,
      );
      assert.equal(result.code, 41, result.stderr);
      const log = await readFile(fake.log, "utf8");
      assert.equal((log.match(/args run .*--timing-record /g) ?? []).length, 2);
      assert.doesNotMatch(log, /image promote/);
      if (mode === "missing-both") {
        assert.equal((log.match(/args stop /g) ?? []).length, 1);
        assert.match(result.stderr, /could not recover the attempted measurement lease/);
      } else {
        assert.match(log, /args stop --provider aws --target linux cbx_000000000002/);
        if (mode === "stop-failure")
          assert.match(result.stderr, /measurement cleanup remains unconfirmed/);
      }
    });
  }
});

test("measured local image overrides are rejected before paid operations", async (t) => {
  const fake = await measuredFixture(t);
  const result = await runScript(
    fake.args,
    { ...fake.env, CRABBOX_FAKE_CONFIG_AMI: "ami-configured" },
    fake.script,
  );
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /effective aws.ami override/);
  const log = await readFile(fake.log, "utf8");
  assert.match(log, /args config show --provider aws --json/);
  assert.doesNotMatch(log, /args (run|warmup|image|checkpoint) /);
});

test("measured tee failure still stops the lease and never replaces a run failure", async (t) => {
  for (const runFails of [false, true]) {
    await t.test(runFails ? "run and tee fail" : "tee fails", async (t) => {
      const fake = await measuredFixture(t);
      const bin = path.join(fake.dir, "bin");
      await mkdir(bin);
      const nativeTee = execFileSync("sh", ["-c", "command -v tee"], { encoding: "utf8" }).trim();
      const tee = path.join(bin, "tee");
      await writeFile(
        tee,
        `#!/usr/bin/env bash
"${nativeTee}" "$@"
if [[ "$1" == *baseline-${runFails ? 2 : 1}.log ]]; then exit 61; fi
`,
      );
      await chmod(tee, 0o755);
      const result = await runScript(
        fake.args,
        {
          ...fake.env,
          PATH: `${bin}${path.delimiter}${process.env.PATH}`,
          CRABBOX_FAKE_RUN_FAIL_PHASE: runFails ? "baseline" : "",
        },
        fake.script,
      );
      assert.equal(result.code, runFails ? 41 : 61, result.stderr);
      const log = await readFile(fake.log, "utf8");
      assert.match(
        log,
        new RegExp(`args stop --provider aws --target linux cbx_00000000000${runFails ? 2 : 1}`),
      );
      assert.doesNotMatch(log, /image promote/);
      const outcome = JSON.parse(await readFile(fake.outcome, "utf8"));
      assert.equal(outcome.status, "failed");
      assert.equal(outcome.stage, "baseline");
      assert.equal(outcome.exitCode, runFails ? 41 : 61);
      assert.equal(outcome.rollbackStatus, "not_required");
      assert.equal(outcome.cleanupStatus, "succeeded");
    });
  }
});

test("measured interruption recovers a retained handle without a final timing row", async (t) => {
  for (const [phase, signal, stopFails] of [
    ["baseline", "SIGTERM", false],
    ["baseline", "SIGINT", true],
    ["promoted", "SIGTERM", false],
  ]) {
    await t.test(`${phase} ${signal}`, async (t) => {
      const fake = await measuredFixture(t);
      const lease = phase === "baseline" ? "cbx_000000000002" : "cbx_000000000008";
      let sent = false;
      const result = await runScript(
        fake.args,
        {
          ...fake.env,
          CRABBOX_FAKE_INTERRUPT_PHASE: phase,
          CRABBOX_FAKE_STOP_FAIL_LEASE: stopFails ? lease : "",
        },
        fake.script,
        (chunk, child) => {
          if (!sent && chunk.includes("retained-handle-ready")) {
            sent = true;
            child.kill(signal);
          }
        },
      );
      assert.equal(sent, true);
      assert.equal(result.code, signal === "SIGINT" ? 130 : 143, result.stderr);
      const log = await readFile(fake.log, "utf8");
      assert.match(log, new RegExp(`args stop --provider aws --target linux ${lease}`));
      const store = log.match(new RegExp(`--timing-record (\\S+/${phase}\\.jsonl)`))[1];
      assert.equal((await readFile(store, "utf8")).trim().split("\n").length, 1);
      if (phase === "promoted") assert.match(log, /--restore-receipt \S+ ami-devtools/);
      else assert.doesNotMatch(log, /image promote/);
      if (stopFails) assert.match(result.stderr, /stop failed/);
      const outcome = JSON.parse(await readFile(fake.outcome, "utf8"));
      assert.equal(outcome.status, "failed");
      assert.equal(outcome.stage, phase === "promoted" ? "promoted_measure" : "baseline");
      assert.equal(outcome.exitCode, signal === "SIGINT" ? 130 : 143);
      assert.equal(outcome.rollbackStatus, phase === "promoted" ? "succeeded" : "not_required");
      assert.equal(outcome.cleanupStatus, stopFails ? "failed" : "succeeded");
      assert.doesNotMatch(result.stdout, /public measurement proof:/);
    });
  }
});

test("measured stock and promoted baselines must match prior-default presence", async (t) => {
  for (const mode of ["stock-no-prior", "stock-unexpected-prior", "promoted-missing-prior"]) {
    await t.test(mode, async (t) => {
      const fake = await measuredFixture(t);
      const result = await runScript(
        fake.args,
        {
          ...fake.env,
          CRABBOX_FAKE_PREVIOUS_ABSENT: mode === "stock-unexpected-prior" ? "0" : "1",
          CRABBOX_FAKE_BASELINE_SOURCE: mode === "promoted-missing-prior" ? "promoted" : "stock",
        },
        fake.script,
      );
      const log = await readFile(fake.log, "utf8");
      if (mode === "stock-no-prior") {
        assert.equal(result.code, 0, result.stderr);
        assert.match(result.stdout, /public measurement proof:/);
        assert.doesNotMatch(log, /--restore-receipt/);
      } else {
        assert.notEqual(result.code, 0);
        assert.match(result.stderr, /baseline differs from the captured previous default/);
        assert.match(log, /--restore-receipt \S+ ami-devtools/);
        assert.equal((log.match(/args warmup /g) ?? []).length, 2);
      }
    });
  }
});

test("measured image and region evidence must be observed and coherent", async (t) => {
  for (const mode of [
    "baseline-image",
    "baseline-source",
    "baseline-region",
    "candidate-region",
    "promoted-region",
    "candidate-image",
    "coordinator-override",
    "missing-selection",
    "duplicate-selection",
    "reused-instance",
    "image-region",
    "admin-failure",
  ]) {
    await t.test(mode, async (t) => {
      const fake = await measuredFixture(t);
      const result = await runScript(
        fake.args,
        { ...fake.env, CRABBOX_FAKE_SELECTION_MODE: mode },
        fake.script,
      );
      assert.notEqual(result.code, 0);
      if (mode === "admin-failure") assert.equal(result.code, 53);
      const log = await readFile(fake.log, "utf8");
      assert.match(log, /args stop --provider aws --target linux cbx_[0-9a-f]{12}/);
      if (mode === "promoted-region") assert.match(log, /--restore-receipt/);
      else assert.doesNotMatch(log, /image promote/);
      if (
        [
          "missing-selection",
          "duplicate-selection",
          "coordinator-override",
          "admin-failure",
        ].includes(mode)
      ) {
        assert.equal((log.match(/args run .*--timing-record /g) ?? []).length, 1);
      }
      assert.doesNotMatch(result.stdout, /public measurement proof:/);
    });
  }
});

test("measured baseline must match the default captured by the original promotion receipt", async (t) => {
  const fake = await measuredFixture(t);
  const result = await runScript(
    fake.args,
    { ...fake.env, CRABBOX_FAKE_PREVIOUS_IMAGE: "ami-concurrent" },
    fake.script,
  );
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /baseline differs from the captured previous default/);
  assert.match(await readFile(fake.log, "utf8"), /--restore-receipt \S+ ami-devtools/);
  const outcome = JSON.parse(await readFile(fake.outcome, "utf8"));
  assert.equal(outcome.status, "failed");
  assert.equal(outcome.stage, "promotion");
  assert.equal(outcome.rollbackStatus, "succeeded");
  assert.match(outcome.promotionBindingDigest, /^sha256:[0-9a-f]{64}$/);
  assert.doesNotMatch(result.stdout, /public measurement proof:/);
});

test("measured receipt-less promotion failures retain the completed cohorts", async (t) => {
  const fake = await measuredFixture(t);
  const result = await runScript(
    fake.args,
    {
      ...fake.env,
      CRABBOX_FAKE_PROMOTION_FAIL: "1",
    },
    fake.script,
  );
  assert.equal(result.code, 55, result.stderr);
  assert.match(result.stderr, /transactional promotion receipt is unavailable for rollback/);
  const outcome = JSON.parse(await readFile(fake.outcome, "utf8"));
  assert.equal(outcome.status, "failed");
  assert.equal(outcome.stage, "promotion");
  assert.equal(outcome.exitCode, 55);
  assert.equal(outcome.rollbackStatus, "failed");
  assert.equal(outcome.promotionBindingDigest, null);
  assert.deepEqual(
    outcome.cohorts.map((cohort) => cohort.phase),
    ["baseline", "candidate"],
  );
});

test("measured warmup cleanup failures remain visible to finalization", async (t) => {
  const fake = await measuredFixture(t);
  const result = await runScript(
    fake.args,
    {
      ...fake.env,
      CRABBOX_FAKE_WARMUP_FAIL_AFTER_LEASE: "1",
      CRABBOX_FAKE_STOP_FAIL_LEASE: "cbx_source",
    },
    fake.script,
  );
  assert.equal(result.code, 23, result.stderr);
  const log = await readFile(fake.log, "utf8");
  assert.ok((log.match(/stop --provider aws --target linux cbx_source/g) ?? []).length >= 2);
  const outcome = JSON.parse(await readFile(fake.outcome, "utf8"));
  assert.equal(outcome.status, "failed");
  assert.equal(outcome.stage, "source_prepare");
  assert.equal(outcome.cleanupStatus, "failed");
});

test("measured wrong promoted selection stops the allocated lease before rollback", async (t) => {
  const fake = await measuredFixture(t);
  const result = await runScript(
    fake.args,
    {
      ...fake.env,
      CRABBOX_FAKE_WARMUP_WRONG_IMAGE: "1",
    },
    fake.script,
  );
  assert.equal(result.code, 1, result.stderr);
  const log = await readFile(fake.log, "utf8");
  assert.match(log, /args stop --provider aws --target linux cbx_promoted/);
  assert.match(log, /--restore-receipt \S+ ami-devtools/);
});

test("measured post-promotion failures preserve the original publisher status", async (t) => {
  for (const mode of ["benchmark", "outcome", "rollback"]) {
    await t.test(mode, async (t) => {
      const fake = await measuredFixture(t);
      const env = { ...fake.env, CRABBOX_FAKE_CHECK_FAIL_PHASE: "promoted" };
      if (mode === "rollback") env.CRABBOX_FAKE_ROLLBACK_FAIL = "1";
      if (mode === "outcome") {
        delete env.CRABBOX_FAKE_CHECK_FAIL_PHASE;
        const bin = path.join(fake.dir, "bin");
        await mkdir(bin);
        const node = path.join(bin, "node");
        await writeFile(
          node,
          `#!/usr/bin/env bash\nif [[ "$1" == *devtools-image-proof.mjs && "\${2:-}" == finalize ]]; then exit 66; fi\nexec "${process.execPath}" "$@"\n`,
        );
        await chmod(node, 0o755);
        env.PATH = `${bin}${path.delimiter}${process.env.PATH}`;
      }
      const result = await runScript(fake.args, env, fake.script);
      assert.equal(result.code, mode === "outcome" ? 66 : 37, result.stderr);
      const log = await readFile(fake.log, "utf8");
      if (mode === "outcome") {
        assert.match(log, /image promote --json --target linux .*--restore-receipt \S+ ami-devtools/);
      } else {
        assert.match(log, /image promote --json --target linux .*--restore-receipt \S+ ami-devtools/);
      }
      if (mode === "rollback") assert.match(result.stderr, /newer default was not overwritten/);
      assert.doesNotMatch(result.stdout, /public measurement proof:/);
    });
  }
});
