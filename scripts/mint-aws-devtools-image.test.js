import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
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
    if [[ -n "\${CRABBOX_FAKE_READINESS_FAIL_LEASE:-}" && "$*" == *"--id \${CRABBOX_FAKE_READINESS_FAIL_LEASE} "* && "$*" == *"-- --verify linux-builder"* ]]; then
      printf 'Linux readiness verification: linux-builder manifest required\\n' >&2
      exit 74
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
  assert.match(text, /trap 'exit 130' INT\ntrap 'exit 143' TERM/);
  assert.match(text, /rollback_pending=1\nrun_json_tee "\$promotion_log"/);
  assert.doesNotMatch(text, /\bBASHPID\b/);
});

test("AWS Linux image verification uses trusted source bytes instead of the installed producer", async () => {
  const source = await readFile(script, "utf8");
  const smoke = await readFile(path.join(scriptDir, "devtools-image-smoke-linux.sh"), "utf8");
  assert.match(source, /--script "\$ROOT\/scripts\/linux-readiness\.generated\.sh" -- --install/);
  assert.equal(
    (source.match(/--script "\$ROOT\/scripts\/linux-readiness\.generated\.sh"/g) ?? []).length,
    2,
  );
  assert.match(source, /--script "\$ROOT\/scripts\/linux-readiness\.generated\.sh" -- --verify linux-builder/);
  assert.doesNotMatch(source, /--shell -- \/usr\/local\/libexec\/crabbox\/linux-readiness\.generated\.sh/);
  assert.doesNotMatch(smoke, /test -f \/var\/lib\/crabbox.*(?:linux\.json|image-ready)/);
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
      "--prep-script",
      fake.linuxPrep,
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
  assert.equal((log.match(/linux-readiness\.generated\.sh/g) ?? []).length, 4);
  for (const phase of ["source", "candidate", "promoted"]) {
    assert.ok(log.includes(`run --provider aws --target linux --id cbx_${phase} --no-sync --script ${path.join(scriptDir, "linux-readiness.generated.sh")} -- --verify linux-builder`));
  }
  assert.match(
    log,
    /run --provider aws --target linux --id cbx_source --no-sync --shell -- export CRABBOX_LINUX_DESKTOP_TOOLS=1 CRABBOX_LINUX_BROWSER=1\nset -euo pipefail/,
  );
  assert.equal((log.match(/corepack --version/g) ?? []).length, 3);
  assert.equal((log.match(/pnpm --version/g) ?? []).length, 3);
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

async function runLinuxSmoke(t, options = {}) {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-linux-smoke-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const bin = path.join(root, "bin");
  const scratch = path.join(root, "scratch");
  const caches = path.join(root, "caches");
  const events = path.join(root, "events");
  const browserPID = path.join(root, "browser.pid");
  for (const directory of [bin, scratch, ...["pnpm", "npm", "corepack", "docker"].map((name) => path.join(caches, name))]) {
    await mkdir(directory, { recursive: true });
  }
  if (options.fail === "cache") await rm(path.join(caches, "pnpm"), { recursive: true });
  const writeTool = async (name, body) => {
    const file = path.join(bin, name);
    await writeFile(file, `#!/usr/bin/env bash
set -eu
printf '%s %s\\n' '${name}' "$*" >>"$CRABBOX_SMOKE_EVENTS"
[[ "\${CRABBOX_SMOKE_FAIL:-}" != '${name}' ]] || exit 37
${body}
`);
    await chmod(file, 0o755);
  };
  for (const name of ["git", "gh", "jq", "rg", "fd", "python3", "trufflehog", "xset"]) {
    await writeTool(name, "exit 0");
  }
  for (const name of ["npm", "corepack", "pnpm"]) {
    await writeTool(name, '[[ "$COREPACK_ENABLE_NETWORK" == 0 && "$npm_config_offline" == true ]]');
  }
  await writeTool("node", `exec ${JSON.stringify(process.execPath)} "$@"`);
  await writeTool("cc", `[[ "$2" == -o ]]
printf '#!/bin/sh\\nexit 0\\n' >"$3"
chmod +x "$3"`);
  await writeTool("id", 'case "$*" in -u) echo 1000 ;; -nG) echo users ;; esac');
  await writeTool("whoami", "echo alice");
  await writeTool("getent", `[[ "\${CRABBOX_SMOKE_FAIL:-}" != access ]] || exit 1
echo 'docker:x:999:alice,bob'`);
  await writeTool("docker", `if [[ "$1" == version && "\${CRABBOX_SMOKE_REFRESH:-0}" == 1 && "\${CRABBOX_FAKE_IN_SG:-0}" != 1 ]]; then exit 1; fi
if [[ "\${CRABBOX_SMOKE_FAIL:-}" == access && "$1" == version ]]; then exit 1; fi
if [[ "\${CRABBOX_SMOKE_FAIL:-}" == buildx && "$1 \${2:-}" == "buildx build" ]]; then exit 37; fi
if [[ "\${CRABBOX_SMOKE_FAIL:-}" == compose && "$*" == *" run --rm "* ]]; then exit 37; fi
exit 0`);
  await writeTool("sg", '[[ "$1 $2" == "docker -c" ]]\nCRABBOX_FAKE_IN_SG=1 sh -c "$3"');
  await writeTool("sudo", "exit 80");
  await writeTool("timeout", '[[ "$1" != --kill-after=* ]] || shift\nshift\nexec "$@"');
  await writeTool("crabbox-browser", `if [[ "$*" == *"--headless"* ]]; then echo '<p>crabbox-browser-smoke</p>'; exit 0; fi
exec ${JSON.stringify(process.execPath)} -e 'const fs=require("node:fs"); const file=process.env.CRABBOX_SMOKE_BROWSER_PID; process.on("SIGTERM",()=>{fs.unlinkSync(file); process.exit(0)}); fs.writeFileSync(file,String(process.pid)); setInterval(()=>{},1000)'`);
  await writeTool("xdotool", `if [[ "$1" == search ]]; then
  [[ "\${CRABBOX_SMOKE_FAIL:-}" != window && -f "$CRABBOX_SMOKE_BROWSER_PID" ]] || exit 1
  echo 12345
fi`);
  await writeTool("ffmpeg", `exec ${JSON.stringify(process.execPath)} -e 'require("node:fs").writeFileSync(process.argv[1],Buffer.alloc(768,Buffer.from(process.env.CRABBOX_SMOKE_FAIL === "render" ? [0,0,0] : [25,163,91])))' "\${*: -1}"`);
  await writeTool("sleep", "/bin/sleep 0.05");
  await writeTool("scrot", 'printf png >"$1"');
  await writeTool("ffprobe", "echo 1280,720");
  const smoke = path.join(root, "smoke.sh");
  const source = await readFile(path.join(scriptDir, "devtools-image-smoke-linux.sh"), "utf8");
  await writeFile(smoke, source.replaceAll("/var/cache/crabbox", caches));
  const result = await runScript([], {
    PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
    TMPDIR: scratch,
    DISPLAY: ":99",
    CRABBOX_SMOKE_EVENTS: events,
    CRABBOX_SMOKE_BROWSER_PID: browserPID,
    CRABBOX_SMOKE_FAIL: options.fail ?? "",
    CRABBOX_SMOKE_REFRESH: options.refresh ? "1" : "0",
    CRABBOX_LINUX_BROWSER: options.browser ?? "1",
    CRABBOX_LINUX_DESKTOP_TOOLS: options.desktop ?? "1",
  }, smoke);
  assert.deepEqual(await readdir(scratch), [], "smoke must remove all disposable fixtures");
  for (const cache of await readdir(caches)) {
    assert.deepEqual(await readdir(path.join(caches, cache)), [], "cache write probes must be removed");
  }
  await assert.rejects(readFile(browserPID), { code: "ENOENT" }, "smoke must stop its own visible browser");
  return { ...result, events: await readFile(events, "utf8") };
}

test("Linux image smoke runs offline functional checks and cleans its fixtures", async (t) => {
  const result = await runLinuxSmoke(t);
  assert.equal(result.code, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /devtools-smoke-ok/);
  assert.match(result.events, /npm --offline run check/);
  assert.match(result.events, /pnpm run check/);
  assert.match(result.events, /docker run --rm --pull=never --network=none/);
  assert.match(result.events, /docker buildx build --builder default --pull=false --network=none/);
  assert.match(result.events, /docker compose .* run --rm --no-deps --pull never check/);
  assert.match(result.events, /docker compose .* down --remove-orphans/);
  assert.match(result.events, /docker image rm crabbox-smoke-/);
  assert.match(result.events, /crabbox-browser --headless/);
  assert.match(result.events, /scrot /);
  assert.match(result.events, /xdotool search --onlyvisible --limit 1 --name crabbox-render-/);
  assert.match(result.events, /ffmpeg .* -f x11grab -draw_mouse 0 -window_id 12345 -i :99 /);
  assert.doesNotMatch(result.events, /sudo |docker pull |apt-get |npm install/);
});

test("Linux image smoke refreshes the first Docker group member without root execution", async (t) => {
  const result = await runLinuxSmoke(t, { refresh: true });
  assert.equal(result.code, 0, result.stderr || result.stdout);
  assert.match(result.events, /sg docker -c/);
  assert.doesNotMatch(result.events, /sudo /);
});

test("Linux image smoke rejects failed capabilities without success output and cleans fixtures", async (t) => {
  for (const fail of ["cc", "python3", "npm", "pnpm", "cache", "buildx", "compose", "access", "crabbox-browser", "xset", "ffprobe", "window", "render"]) {
    await t.test(fail, async (subtest) => {
      const result = await runLinuxSmoke(subtest, { fail });
      assert.notEqual(result.code, 0, result.stderr || result.stdout);
      assert.doesNotMatch(result.stdout, /devtools-smoke-ok/);
      assert.doesNotMatch(result.events, /sudo /);
      if (fail === "buildx" || fail === "compose") {
        assert.equal(result.code, 37, "cleanup must preserve the original failure");
        assert.match(result.events, /docker image rm crabbox-smoke-/);
      }
      if (fail === "window" || fail === "render") {
        assert.match(result.stderr, /browser did not render the local fixture/);
      }
    });
  }
});

test("Linux image smoke respects disabled browser and desktop capabilities", async (t) => {
  for (const [browser, desktop] of [["0", "0"], ["1", "0"], ["0", "1"]]) {
    await t.test(`browser=${browser} desktop=${desktop}`, async (subtest) => {
      const result = await runLinuxSmoke(subtest, { browser, desktop });
      assert.equal(result.code, 0, result.stderr || result.stdout);
      if (browser === "0") assert.doesNotMatch(result.events, /crabbox-browser/);
      if (desktop === "0") assert.doesNotMatch(result.events, /xset|xdotool|scrot|ffprobe/);
      assert.doesNotMatch(result.events, /ffmpeg |--app=/);
    });
  }
});

test("publisher checks the builder profile in every Linux lifecycle phase", async (t) => {
  for (const phase of ["source", "candidate", "promoted"]) {
    await t.test(phase, async (subtest) => {
      const fake = await setupFakeCrabbox();
      subtest.after(() => rm(fake.dir, { recursive: true, force: true }));
      const result = await runScript(["--target", "linux", "--run", "--prep-script", fake.linuxPrep], {
        CRABBOX_BIN: fake.fake,
        CRABBOX_FAKE_LOG: fake.log,
        CRABBOX_IMAGE_LOG_DIR: path.join(fake.dir, "logs"),
        CRABBOX_FAKE_READINESS_FAIL_LEASE: `cbx_${phase}`,
      });
      assert.equal(result.code, 74, result.stderr);
      const log = await readFile(fake.log, "utf8");
      assert.match(log, new RegExp(`stop --provider aws --target linux cbx_${phase}`));
      if (phase === "source") assert.doesNotMatch(log, /checkpoint create/);
      if (phase === "candidate") assert.doesNotMatch(log, /image promote/);
      if (phase === "promoted") assert.match(log, /image promote .*--restore-receipt/);
    });
  }
});

test("publisher carries disabled Linux capability flags into every smoke", async (t) => {
  const fake = await setupFakeCrabbox();
  t.after(() => rm(fake.dir, { recursive: true, force: true }));
  const result = await runScript(["--target", "linux", "--run", "--no-browser", "--no-desktop", "--prep-script", fake.linuxPrep], {
    CRABBOX_BIN: fake.fake,
    CRABBOX_FAKE_LOG: fake.log,
    CRABBOX_IMAGE_LOG_DIR: path.join(fake.dir, "logs"),
  });
  assert.equal(result.code, 0, result.stderr);
  const log = await readFile(fake.log, "utf8");
  assert.equal((log.match(/export CRABBOX_LINUX_DESKTOP_TOOLS=0 CRABBOX_LINUX_BROWSER=0/g) ?? []).length, 3);
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
