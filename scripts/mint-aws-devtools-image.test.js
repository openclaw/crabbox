import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

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
      1) printf '{"leaseId":"cbx_source"}\\n' ;;
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
          printf '{"image":{"id":"ami-devtools","revision":"rev-new"},"previous":{"state":"absent","aliases":[{"alias":"regional","state":"absent"}]}}\\n'
        else
          printf '{"image":{"id":"ami-devtools","revision":"rev-new"},"previous":{"state":"present","imageId":"ami-previous","revision":"rev-old","aliases":[{"alias":"regional","state":"present","image":{"id":"ami-previous","name":"previous","state":"available","provider":"aws","promotedAt":"2026-09-01T00:00:00Z","revision":"rev-old"}}]}}\\n'
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

function runScript(args, env) {
  return new Promise((resolve, reject) => {
    const child = spawn("bash", [script, ...args], {
      cwd: repoRoot,
      env: { ...process.env, ...env },
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
  assert.match(text, /pnpm --version\ntrufflehog --no-update --version\ndocker --version/);
  assert.match(
    text,
    /command -v pnpm\ncommand -v trufflehog\ntrufflehog --no-update --version\ncommand -v docker\nnode --version\n/,
  );
  assert.match(text, /corepack --version\npnpm --version\ndocker_group_member/);
  assert.match(text, /trap 'exit 130' INT\ntrap 'exit 143' TERM/);
  assert.match(text, /rollback_pending=1\nrun_json_tee "\$promotion_log"/);
});

test("AWS Linux image production stages and invokes only the generated readiness producer", async () => {
  const source = await readFile(script, "utf8");
  assert.match(source, /--script "\$ROOT\/scripts\/linux-readiness\.generated\.sh" -- --install/);
  assert.equal((source.match(/--script "\$ROOT\/scripts\/linux-readiness\.generated\.sh"/g) ?? []).length, 1);
  assert.match(source, /--shell -- \/usr\/local\/libexec\/crabbox\/linux-readiness\.generated\.sh/);
  assert.match(source, /test -f \/var\/lib\/crabbox-readiness\/linux\.json/);
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
  assert.match(log, /env CRABBOX_AWS_REGION=us-west-2 AWS_REGION=us-west-2 CRABBOX_AWS_AMI= args warmup --provider aws --target linux/);
  assert.match(log, /env CRABBOX_AWS_REGION=us-west-2 AWS_REGION=us-west-2 CRABBOX_AWS_AMI=ami-devtools args warmup --provider aws --target linux/);
  assert.match(log, /--class standard/);
  assert.match(log, /--browser/);
  assert.doesNotMatch(log, /warmup .*--region us-west-2/);
  assert.match(log, /run --provider aws --target linux --id cbx_source --no-sync --script/);
  assert.match(log, /linux-readiness\.generated\.sh -- --install/);
  assert.equal((log.match(/linux-readiness\.generated\.sh/g) ?? []).length, 2);
  assert.match(log, /run --provider aws --target linux --id cbx_source --no-sync --shell -- \/usr\/local\/libexec\/crabbox\/linux-readiness\.generated\.sh/);
  assert.match(log, /run --provider aws --target linux --id cbx_source --no-sync --shell -- set -euo pipefail/);
  assert.equal((log.match(/corepack --version/g) ?? []).length, 3);
  assert.equal((log.match(/\n  offline_node_pnpm_probe\nfi\necho devtools-smoke-ok/g) ?? []).length, 3);
  assert.equal((log.match(/\npnpm --version\n/g) ?? []).length, 3);
  assert.match(log, /docker image inspect hello-world ubuntu:24\.04 node:24-bookworm/);
  assert.match(log, /env CRABBOX_AWS_REGION=us-west-2 AWS_REGION=us-west-2 CRABBOX_AWS_AMI= args checkpoint create --provider aws --target linux --id cbx_source --name crabbox-linux-devtools-/);
  assert.match(log, /--mode native --strategy image --no-reboot=false --wait --wait-timeout 60m/);
  assert.match(log, /image promote --target linux --json --expected-current-image capture --region us-west-2 --fast-snapshot-restore --fsr-az us-west-2a ami-devtools/);
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
const versions = JSON.parse(process.env.FIXTURE_NODE_VERSIONS);
const result = spawnSync("bash", ["-c", checks[0]], {
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
    const result = await runScript(["--target", "linux", "--run", "--no-promote", "--name", "shared-devtools", "--prep-script", fake.linuxPrep], {
      CRABBOX_BIN: fake.fake,
      CRABBOX_FAKE_LOG: fake.log,
      CRABBOX_IMAGE_LOG_DIR: logDir,
      CRABBOX_IMAGE_WINDOWS_WARMUP_SETTLE_SECONDS: "0",
      CRABBOX_IMAGE_REBOOT_READY_SETTLE_SECONDS: "0",
      CRABBOX_IMAGE_PREP_WAIT_TIMEOUT: "5s",
    });
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
  assert.match(
    log,
    /image promote --json --target linux --restore-receipt \S+ ami-devtools/,
  );
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
  assert.match(result.stderr, /CAS rollback failed or was rejected; a newer default was not overwritten/);
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
  assert.match(
    log,
    /image promote --json --target linux --restore-receipt \S+ ami-devtools/,
  );
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
  const result = await runScript(["--target", "linux", "--run", "--no-promote", "--prep-script", fake.linuxPrep], {
    CRABBOX_BIN: fake.fake,
    CRABBOX_FAKE_LOG: fake.log,
    CRABBOX_FAKE_CAPTURE_RUN_SCRIPT: smokeScript,
  });
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
  await writeTool("node", "#!/usr/bin/env bash\n[[ \"${1:-}\" == \"--version\" ]] && printf 'v24.0.0\\n'\nexit 0\n");
  await writeTool("id", "#!/usr/bin/env bash\ncase \"$*\" in -nG) printf 'users\\n';; -u) printf '1000\\n';; esac\n");
  await writeTool("whoami", "#!/usr/bin/env bash\nprintf 'alice\\n'\n");
  await writeTool("getent", "#!/usr/bin/env bash\n[[ \"$*\" == \"group docker\" ]] && printf 'docker:x:999:alice,bob\\n'\n");
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
  assert.match(log, /env CRABBOX_AWS_REGION=us-east-1 AWS_REGION=us-east-1 CRABBOX_AWS_AMI= args warmup --provider aws --target windows/);
  assert.match(log, /--windows-mode normal/);
  assert.doesNotMatch(log, /--desktop/);
  assert.doesNotMatch(log, /--browser/);
  assert.doesNotMatch(log, /warmup .*--region us-east-1/);
  assert.match(log, /run --provider aws --target windows --id cbx_source --no-sync --shell -- Write-Output "windows-ssh-ready"/);
  assert.match(log, /run --provider aws --target windows --id cbx_source --no-sync --shell -- New-Item/);
  assert.match(log, /run --provider aws --target windows --id cbx_source --no-sync --shell -- Set-Content/);
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
  assert.match(log, /run --provider aws --target windows --id cbx_source --no-sync --shell -- if \(Test-Path/);
  assert.match(log, /run --provider aws --target windows --id cbx_source --no-sync --shell -- shutdown \/r \/t 5 \/f/);
  assert.match(log, /run --provider aws --target windows --id cbx_source --no-sync --shell -- Write-Output "windows-ssh-ready"/);
  assert.match(log, /run --provider aws --target windows --id cbx_source --no-sync --shell -- Set-Content/);
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
  assert.match(log, /run --provider aws --target windows --id cbx_source --no-sync --shell -- Write-Output "windows-ssh-ready"/);
  assert.match(log, /run --provider aws --target windows --id cbx_source --no-sync --shell -- if \(Test-Path/);
  assert.match(log, /run --provider aws --target windows --id cbx_source --no-sync --shell -- shutdown \/r \/t 5 \/f/);
  assert.match(log, /checkpoint create --provider aws --target windows --id cbx_source --name crabbox-windows-devtools-/);
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
