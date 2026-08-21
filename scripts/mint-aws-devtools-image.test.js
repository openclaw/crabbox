import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, readdir, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { canonicalJSON, digestJSON } from "./aws-image-candidate.mjs";

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
  const scrubEvidence = {
    schema: "crabbox-aws-image-scrub/v1",
    target: "linux",
    removed: {
      authorizedKeys: 1,
      cloudInitState: 1,
      credentials: 1,
      hostIdentity: 1,
      prepArtifacts: 1,
      shellHistory: 1,
      sshHostKeys: 1,
      workspaces: 1,
    },
    findings: [],
  };
  const scrubReport = canonicalJSON({
    ...scrubEvidence,
    evidenceDigest: digestJSON(scrubEvidence),
  });
  const windowsScrubEvidence = { ...scrubEvidence, target: "windows" };
  const windowsScrubReport = canonicalJSON({
    ...windowsScrubEvidence,
    evidenceDigest: digestJSON(windowsScrubEvidence),
  });
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
        printf 'image selected id=ami-base123 source=resolved kind=aws-ami region=%s promoted_at=-\\n' "\${CRABBOX_AWS_REGION:-eu-west-1}"
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
    if [[ "$*" == *"scrub-aws-image.mjs"* || "$*" == *"scrub-aws-windows-image.ps1"* ]]; then
      if [[ "\${CRABBOX_FAKE_SCRUB_FINDINGS:-0}" == "1" ]]; then
        printf '{"evidenceDigest":"sha256:%064d","findings":["credentials"],"removed":{},"schema":"crabbox-aws-image-scrub/v1","target":"%s"}\\n' 1 "\${CRABBOX_FAKE_TARGET:-linux}"
      elif [[ "$*" == *"--target windows"* ]]; then
        printf '%s\\n' '${windowsScrubReport}'
      else
        printf '%s\\n' '${scrubReport}'
      fi
      exit 0
    fi
    if [[ -n "\${CRABBOX_FAKE_CAPTURE_RUN_SCRIPT:-}" ]]; then
      last_arg="\${@: -1}"
      if [[ -f "$last_arg" && "$last_arg" == *"smoke-aws-"*"-devtools-image."* ]]; then
        cp "$last_arg" "\${CRABBOX_FAKE_CAPTURE_RUN_SCRIPT}"
      elif [[ "$last_arg" == *"docker_probe="* ]]; then
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
    if [[ "$*" == *"smoke-aws-"*"-devtools-image."* ]]; then
      cat "\${@: -1}" >>"\${CRABBOX_FAKE_LOG:?}"
      smoke_count_file="\${CRABBOX_FAKE_LOG}.smoke-count"
      smoke_count=0
      [[ -f "$smoke_count_file" ]] && smoke_count="$(cat "$smoke_count_file")"
      smoke_count="$((smoke_count + 1))"
      printf '%s\\n' "$smoke_count" >"$smoke_count_file"
      if [[ "\${CRABBOX_FAKE_SMOKE_FAIL_ON:-0}" == "$smoke_count" ]]; then
        exit 29
      fi
    fi
    printf 'devtools-smoke-ok\\n'
    ;;
  checkpoint)
    if [[ "$2" == "create" ]]; then
      if [[ "\${CRABBOX_FAKE_CHECKPOINT_MALFORMED:-0}" == "1" ]]; then
        printf '{not-json\\n'
        exit 0
      fi
      architecture='"architecture":"x86_64",'
      [[ "\${CRABBOX_FAKE_MISSING_ARCHITECTURE:-0}" == "1" ]] && architecture=""
      snapshots='"snapshotIds":["snap-root123"]'
      [[ "\${CRABBOX_FAKE_MISSING_SNAPSHOTS:-0}" == "1" ]] && snapshots='"snapshotIds":[]'
      target=linux
      [[ "$*" == *"--target windows"* ]] && target=windows
      printf '{"id":"chk_devtools","kind":"aws-ami","provider":"aws","targetOS":"%s","serverType":"m7i.large","native":{"provider":"aws","kind":"aws-ami","imageId":"ami-devtools","region":"%s",%s%s}}\\n' "$target" "\${CRABBOX_FAKE_CHECKPOINT_REGION:-\${CRABBOX_AWS_REGION:-us-west-2}}" "$architecture" "$snapshots"
    fi
    ;;
  image)
    if [[ "$2" == "promote" ]]; then
      printf '{"image":{"id":"ami-devtools"}}\\n'
    fi
    ;;
  stop)
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
  const fakeRoot = env.CRABBOX_FAKE_LOG
    ? path.dirname(env.CRABBOX_FAKE_LOG)
    : os.tmpdir();
  return new Promise((resolve, reject) => {
    const child = spawn("bash", [script, ...args], {
      cwd: repoRoot,
      env: {
        ...process.env,
        CRABBOX_IMAGE_REGION: "us-west-2",
        CRABBOX_IMAGE_TYPE: "m7i.large",
        CRABBOX_IMAGE_BASE_IMAGE: "ami-base123",
        CRABBOX_IMAGE_SOURCE_REPOSITORY: "example-org/crabbox",
        CRABBOX_IMAGE_SOURCE_COMMIT: "0123456789abcdef0123456789abcdef01234567",
        CRABBOX_IMAGE_WORKFLOW_REF:
          "example-org/crabbox/.github/workflows/devtools-image-publish.yml@refs/heads/main",
        CRABBOX_IMAGE_CANDIDATE_OUTPUT: path.join(fakeRoot, "candidate"),
        CRABBOX_IMAGE_LOG_DIR: path.join(fakeRoot, "logs"),
        ...env,
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
  const linux = await readFile(
    path.join(scriptDir, "smoke-aws-linux-devtools-image.sh"),
    "utf8",
  );
  const windows = await readFile(
    path.join(scriptDir, "smoke-aws-windows-devtools-image.ps1"),
    "utf8",
  );
  assert.match(windows, /function Invoke-NativeChecked/);
  assert.match(windows, /\$LASTEXITCODE -ne 0/);
  assert.match(windows, /OsName -notmatch "Windows Server 2022"/);
  assert.match(windows, /OsBuildNumber -ne "20348"/);
  assert.match(windows, /InstallationType -ne "Server"/);
  assert.match(windows, /Invoke-NativeChecked "pnpm" @\("--version"\)/);
  assert.match(windows, /Invoke-NativeChecked "docker" @\("version"\)/);
  assert.match(windows, /\$GhVersion = Invoke-NativeChecked "gh" @\("--version"\)/);
  assert.match(windows, /\$RgVersion = Invoke-NativeChecked "rg" @\("--version"\)/);
  assert.doesNotMatch(windows, /Invoke-NativeChecked "(?:gh|rg)".*\|\s*Select-Object/);
  assert.match(
    linux,
    /command -v pnpm\ncommand -v trufflehog\ntrufflehog --no-update --version\ncommand -v docker\nnode --version\nnode -e .*\ncorepack --version\npnpm --version\n/,
  );
});

test("candidate-only mint runs the exact gated lifecycle and writes evidence last", async () => {
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
      "--no-promote",
      "--prep-script",
      fake.linuxPrep,
    ],
    {
      CRABBOX_BIN: fake.fake,
      CRABBOX_FAKE_LOG: fake.log,
    },
  );
  assert.equal(result.code, 0, result.stderr);
  const lines = (await readFile(fake.log, "utf8")).trim().split("\n");
  const index = (pattern) => lines.findIndex((line) => pattern.test(line));
  const sourceWarmup = index(/args warmup .*--target linux/);
  const prep = index(new RegExp(`args run .*--id cbx_source .*--script ${fake.linuxPrep}`));
  const sourceSmoke = index(
    /args run .*--id cbx_source .*--script .*smoke-aws-linux-devtools-image\.sh/,
  );
  const sourcePrepare = lines.findIndex(
    (line, position) =>
      /args run .*--id cbx_source .*--shell -- set -euo pipefail/.test(line) &&
      /command -v cloud-init/.test(lines[position + 1] ?? ""),
  );
  const scrub = index(/args run .*--id cbx_source .*scrub-aws-image\.mjs/);
  const create = index(/args checkpoint create .*--id cbx_source .*--json/);
  const sourceStop = index(/args stop .*cbx_source$/);
  const candidateWarmup = index(/CRABBOX_AWS_AMI=ami-devtools args warmup/);
  const candidateSmoke = lines.findIndex(
    (line, position) =>
      position > candidateWarmup &&
      /args run .*--id cbx_candidate .*--script .*smoke-aws-linux-devtools-image\.sh/.test(
        line,
      ),
  );
  const candidateStop = index(/args stop .*cbx_candidate$/);
  assert.ok(
    sourceWarmup < prep &&
      prep < sourceSmoke &&
      sourceSmoke < sourcePrepare &&
      sourcePrepare < scrub &&
      scrub < create &&
      create < sourceStop &&
      sourceStop < candidateWarmup &&
      candidateWarmup < candidateSmoke &&
      candidateSmoke < candidateStop,
    lines.join("\n"),
  );
  assert.equal(lines.filter((line) => /args warmup /.test(line)).length, 2);
  assert.match(lines[scrub], /--require-root/);
  assert.match(lines[create], /--source-prepared/);
  assert.doesNotMatch(lines.join("\n"), /image promote|fast-snapshot-restore|fsr-az/);
  const candidate = JSON.parse(
    await readFile(path.join(fake.dir, "candidate", "candidate.json"), "utf8"),
  );
  assert.equal(candidate.image.amiId, "ami-devtools");
  assert.deepEqual(candidate.image.snapshotIds, ["snap-root123"]);
});

test("candidate-only mint leaves no record after malformed checkpoint JSON", async () => {
  const fake = await setupFakeCrabbox();
  const result = await runScript(
    ["--target", "linux", "--region", "us-west-2", "--run", "--no-promote", "--prep-script", fake.linuxPrep],
    {
      CRABBOX_BIN: fake.fake,
      CRABBOX_FAKE_LOG: fake.log,
      CRABBOX_FAKE_CHECKPOINT_MALFORMED: "1",
    },
  );
  assert.notEqual(result.code, 0);
  await assert.rejects(readFile(path.join(fake.dir, "candidate", "candidate.json"), "utf8"));
});

test("candidate-only mint rejects checkpoint mismatch and missing snapshots before boot", async () => {
  for (const failureEnv of [
    { CRABBOX_FAKE_CHECKPOINT_REGION: "us-east-1" },
    { CRABBOX_FAKE_MISSING_ARCHITECTURE: "1" },
    { CRABBOX_FAKE_MISSING_SNAPSHOTS: "1" },
  ]) {
    const fake = await setupFakeCrabbox();
    const result = await runScript(
      ["--target", "linux", "--region", "us-west-2", "--run", "--no-promote", "--prep-script", fake.linuxPrep],
      {
        CRABBOX_BIN: fake.fake,
        CRABBOX_FAKE_LOG: fake.log,
        ...failureEnv,
      },
    );
    assert.notEqual(result.code, 0);
    const log = await readFile(fake.log, "utf8");
    assert.equal((log.match(/args warmup /g) ?? []).length, 1);
    await assert.rejects(readFile(path.join(fake.dir, "candidate", "candidate.json"), "utf8"));
  }
});

test("candidate-only mint rejects scrub findings before image creation", async () => {
  const fake = await setupFakeCrabbox();
  const result = await runScript(
    ["--target", "linux", "--region", "us-west-2", "--run", "--no-promote", "--prep-script", fake.linuxPrep],
    {
      CRABBOX_BIN: fake.fake,
      CRABBOX_FAKE_LOG: fake.log,
      CRABBOX_FAKE_SCRUB_FINDINGS: "1",
    },
  );
  assert.notEqual(result.code, 0);
  const log = await readFile(fake.log, "utf8");
  assert.doesNotMatch(log, /args checkpoint create/);
  await assert.rejects(readFile(path.join(fake.dir, "candidate", "candidate.json"), "utf8"));
});

test("candidate-only mint rejects candidate smoke failure before evidence commit", async () => {
  const fake = await setupFakeCrabbox();
  const result = await runScript(
    ["--target", "linux", "--region", "us-west-2", "--run", "--no-promote", "--prep-script", fake.linuxPrep],
    {
      CRABBOX_BIN: fake.fake,
      CRABBOX_FAKE_LOG: fake.log,
      CRABBOX_FAKE_SMOKE_FAIL_ON: "2",
    },
  );
  assert.equal(result.code, 29);
  const log = await readFile(fake.log, "utf8");
  assert.match(log, /args stop .*cbx_candidate$/m);
  await assert.rejects(readFile(path.join(fake.dir, "candidate", "candidate.json"), "utf8"));
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
      "--promote",
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
  assert.match(log, /env CRABBOX_AWS_REGION=us-west-2 AWS_REGION=us-west-2 CRABBOX_AWS_AMI=ami-base123 args warmup --provider aws --target linux/);
  assert.match(log, /env CRABBOX_AWS_REGION=us-west-2 AWS_REGION=us-west-2 CRABBOX_AWS_AMI=ami-devtools args warmup --provider aws --target linux/);
  assert.match(log, /--class standard/);
  assert.match(log, /--browser/);
  assert.doesNotMatch(log, /warmup .*--region us-west-2/);
  assert.match(log, /run --provider aws --target linux --id cbx_source --no-sync --script/);
  assert.match(log, /run --provider aws --target linux --id cbx_source --no-sync --script .*smoke-aws-linux-devtools-image\.sh/);
  assert.equal((log.match(/corepack --version/g) ?? []).length, 3);
  assert.equal((log.match(/pnpm --version/g) ?? []).length, 3);
  assert.match(log, /docker image inspect hello-world ubuntu:24\.04 node:24-bookworm/);
  assert.match(log, /env CRABBOX_AWS_REGION=us-west-2 AWS_REGION=us-west-2 CRABBOX_AWS_AMI= args checkpoint create --provider aws --target linux --id cbx_source --name crabbox-linux-devtools-/);
  assert.match(log, /--mode native --strategy image --no-reboot=false --source-prepared --wait --wait-timeout 60m/);
  assert.match(log, /image promote --target linux --json --region us-west-2 --fast-snapshot-restore --fsr-az us-west-2a ami-devtools/);
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
  const result = await runScript(["--target", "linux", "--run", "--promote", "--prep-script", fake.linuxPrep], {
    CRABBOX_BIN: fake.fake,
    CRABBOX_FAKE_LOG: fake.log,
  });
  assert.equal(result.code, 1, result.stderr);
  assert.match(
    result.stderr,
    /warmup did not prove image selection id=ami-devtools source=promoted/,
  );
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
  await writeTool("id", "#!/usr/bin/env bash\n[[ \"$*\" == \"-nG\" ]] && printf 'users\\n'\n");
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
    .replace("test -f /var/lib/crabbox/image-ready", "true");
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
  assert.match(log, /env CRABBOX_AWS_REGION=us-east-1 AWS_REGION=us-east-1 CRABBOX_AWS_AMI=ami-base123 args warmup --provider aws --target windows/);
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
