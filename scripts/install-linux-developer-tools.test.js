import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

import { loadRecipes } from "./generate-linux-readiness.mjs";
import { writeExecutable } from "./test-support/smoke-fixtures.mjs";

const repoRoot = path.resolve(import.meta.dirname, "..");
const nodesourceSigningKeyFingerprint = "6F71F525282841EEDAF851B42F59B5F99B1BE0B4";
const dockerSigningKeyFingerprint = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88";
const googleLinuxSigningKeyFingerprint = "EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796";

test("linux developer image installs every readiness package from the generated effective builder profile", async () => {
  const { minimal, builder } = await loadRecipes();
  const source = fs.readFileSync(path.join(repoRoot, "scripts/install-linux-developer-tools.sh"), "utf8");
  const producer = path.join(repoRoot, "scripts/linux-readiness.generated.sh");
  const printed = spawnSync(producer, ["--print-packages", "linux-builder"], { encoding: "utf8" });
  assert.equal(printed.status, 0, printed.stderr || printed.stdout);
  const packages = printed.stdout.trim().split(/\s+/u);
  assert.deepEqual(packages, builder.aptPackages);
  for (const name of minimal.aptPackages) assert.ok(packages.includes(name), name);
  assert.match(source, /"\$readiness_producer" --print-packages linux-builder/);
  assert.match(source, /apt_install "\$\{apt_get_base\[@\]\}" "\$\{readiness_packages\[@\]\}"/);
  assert.doesNotMatch(source, /\beval\b/);
  assert.match(source, /"\$readiness_producer"/);
  assert.doesNotMatch(source, /(?:>|tee\s+).*\/var\/lib\/crabbox(?:\/image-ready|-readiness\/linux\.json)/);
});

test("linux developer image executes the standalone producer and stops when capability proof fails", (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-linux-readiness-installer-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const scriptRoot = path.join(root, "scripts");
  const marker = path.join(root, "producer.called");
  const cleaned = path.join(root, "cleanup.called");
  fs.mkdirSync(scriptRoot);
  fs.copyFileSync(
    path.join(repoRoot, "scripts/install-linux-developer-tools.sh"),
    path.join(scriptRoot, "install.sh"),
  );
  writeExecutable(
    path.join(scriptRoot, "linux-readiness.generated.sh"),
    `#!/usr/bin/env bash\nprintf 'verified\\n' >${JSON.stringify(marker)}\nexit \"\${CRABBOX_FAKE_PRODUCER_EXIT:-0}\"\n`,
  );
  const shell = `set -euo pipefail
source ${JSON.stringify(path.join(scriptRoot, "install.sh"))}
install() { return 0; }
systemctl() { return 0; }
clean_cloud_init_state() { printf 'cleaned\\n' >${JSON.stringify(cleaned)}; }
sync() { return 0; }
prepare_fast_boot`;
  const successful = spawnSync("bash", ["-c", shell], { cwd: repoRoot, encoding: "utf8" });
  assert.equal(successful.status, 0, successful.stderr || successful.stdout);
  assert.equal(fs.readFileSync(marker, "utf8"), "verified\n");
  assert.equal(fs.readFileSync(cleaned, "utf8"), "cleaned\n");
  fs.unlinkSync(cleaned);
  const failed = spawnSync("bash", ["-c", shell], {
    cwd: repoRoot,
    encoding: "utf8",
    env: { ...process.env, CRABBOX_FAKE_PRODUCER_EXIT: "73" },
  });
  assert.equal(failed.status, 73, failed.stderr || failed.stdout);
  assert.equal(fs.existsSync(cleaned), false);
});

test("linux developer image cloud-init cleanup preserves current-boot facts", async (t) => {
  const python = spawnSync("python3", ["-c", "import sys; print(sys.executable)"], {
    encoding: "utf8",
  });
  assert.equal(python.status, 0, python.stderr);
  const source = fs.readFileSync(
    path.join(repoRoot, "scripts/install-linux-developer-tools.sh"),
    "utf8",
  );
  assert.equal(source.split("/usr/bin/python3").length, 2, "one distro Python invocation");
  for (const scenario of [
    { name: "completed boot survives cache and seed cleanup", cleaned: true },
    { name: "cleanup failure after cache deletion", failure: "clean", cleaned: true },
    { name: "completion status changes after cleanup", failure: "after-clean", cleaned: true },
    { name: "initialization still running", failure: "running" },
    { name: "initialization not started", failure: "notstarted" },
    { name: "cloud-init disabled", failure: "disabled" },
    { name: "status command fails", failure: "status" },
    { name: "malformed status", failure: "malformed" },
    { name: "persistent runtime filesystem", failure: "disk" },
    { name: "filesystem inspection fails", failure: "stat" },
    { name: "runtime inside cleaned cache", failure: "overlap" },
    { name: "runtime symlink inside cleaned cache", failure: "overlap-link" },
    { name: "second completion copy fails", failure: "copy" },
    { name: "cloud-init absent", failure: "absent" },
    { name: "hostile fixture paths preserve completed boot", cleaned: true, hostilePath: true },
    {
      name: "hostile fixture paths stop after failed cleanup",
      failure: "clean",
      cleaned: true,
      hostilePath: true,
    },
  ]) {
    await t.test(scenario.name, (t) => {
      const cwd = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-cloud-init-")));
      t.after(() => fs.rmSync(cwd, { recursive: true, force: true }));
      // Substitutions may only create file canaries in this fixture's working directory.
      const root = scenario.hostilePath
        ? path.join(cwd, "spaces $(>dollar-canary) `>backtick-canary` ' \" quotes")
        : cwd;
      const assertNoSubstitution = () => {
        for (const name of ["dollar-canary", "backtick-canary"]) {
          assert.equal(
            fs.existsSync(path.join(cwd, name)),
            false,
            `fixture paths must not execute shell substitutions: ${name}`,
          );
        }
      };
      const bin = path.join(root, "bin");
      const cache = path.join(root, "cloud");
      const data = path.join(cache, "data");
      const seed = path.join(cache, "seed");
      const runtime =
        scenario.failure === "overlap"
          ? path.join(cache, "runtime")
          : path.join(root, "run", "cloud-init");
      for (const directory of [bin, data, seed, runtime])
        fs.mkdirSync(directory, { recursive: true });
      const interpreter = scenario.hostilePath ? path.join(root, "python") : python.stdout.trim();
      if (scenario.hostilePath) fs.symlinkSync(python.stdout.trim(), interpreter);
      fs.writeFileSync(path.join(seed, "user-data"), "synthetic seed\n");
      let configuredRuntime = runtime;
      if (scenario.failure === "overlap-link") {
        configuredRuntime = path.join(cache, "runtime-link");
        fs.symlinkSync(runtime, configuredRuntime);
      }
      const facts = {
        "status.json": '{"v1":{"modules-final":{"finished":123}}}\n',
        "result.json": '{"v1":{"errors":[]}}\n',
      };
      const originals = {};
      for (const [name, contents] of Object.entries(facts)) {
        const target = path.join(data, name);
        fs.writeFileSync(target, contents);
        fs.chmodSync(target, name === "status.json" ? 0o644 : 0o640);
        originals[name] = fs.statSync(target);
        fs.symlinkSync(path.relative(runtime, target), path.join(runtime, name));
      }
      const events = path.join(root, "events");
      const cloudInit = path.join(bin, "cloud-init");
      const cli = path.join(root, "cloud-init.py");
      fs.writeFileSync(
        cli,
        `import json, os, pathlib, shutil, sys
args = sys.argv[1:]
runtime = pathlib.Path(os.environ["FIXTURE_RUN"])
failure = os.environ["FIXTURE_FAILURE"]
if args == ["status", "--format=json"]:
    if failure == "status":
        sys.exit(4)
    if failure == "malformed":
        print("not json")
        sys.exit(0)
    status = failure if failure in ("running", "notstarted", "disabled") else "done"
    if failure == "after-clean" and "clean\\n" in pathlib.Path(os.environ["FIXTURE_EVENTS"]).read_text():
        status = "notstarted"
    if not all((runtime / name).is_file() for name in ("status.json", "result.json")):
        status = "notstarted"
    print(json.dumps({"status": status}))
elif args == ["clean", "--logs", "--seed"]:
    with open(os.environ["FIXTURE_EVENTS"], "a") as log:
        log.write("clean\\n")
    shutil.rmtree(os.environ["FIXTURE_DATA"], ignore_errors=True)
    shutil.rmtree(os.environ["FIXTURE_SEED"], ignore_errors=True)
    if failure == "clean":
        sys.exit(7)
else:
    sys.exit("unexpected cloud-init arguments")
`,
      );
      if (scenario.failure !== "absent") {
        writeExecutable(
          cloudInit,
          '#!/bin/sh\nexec "$FIXTURE_PYTHON" -I "$FIXTURE_CLOUD_INIT_SCRIPT" "$@"\n',
        );
      }
      // Exercise the installer body with the existing checkpoint fixture's distro-module boundary.
      const fixture = path.join(root, "fixture.py");
      fs.writeFileSync(
        fixture,
        `import os, shutil, subprocess, sys, types
module = types.ModuleType("cloudinit.cmd.devel")
module.read_cfg_paths = lambda: types.SimpleNamespace(run_dir=os.environ["FIXTURE_RUN"], cloud_dir=os.environ["FIXTURE_CACHE"])
sys.modules["cloudinit.cmd.devel"] = module
run = subprocess.run
def fixture_run(args, **kwargs):
    if args[:4] == [sys.executable, "-I", "-m", "cloudinit.cmd.main"]:
        args = [os.environ["FIXTURE_CLI"]] + args[4:]
    return run(args, **kwargs)
subprocess.run = fixture_run
copy = shutil.copy2
def fixture_copy(source, target):
    if os.environ["FIXTURE_FAILURE"] == "copy" and source.name == "result.json":
        raise OSError("injected second-copy failure")
    return copy(source, target)
shutil.copy2 = fixture_copy
assert sys.argv[1:] == ["-I", "-"]
exec(compile(sys.stdin.read(), "installer-cloud-init-preparation", "exec"))
`,
      );
      const wrapper = path.join(bin, "python-wrapper");
      writeExecutable(
        wrapper,
        '#!/bin/sh\nexec "$FIXTURE_PYTHON" -I "$FIXTURE_MODULE_SCRIPT" "$@"\n',
      );
      writeExecutable(
        path.join(bin, "stat"),
        `#!/bin/sh
if [ "$FIXTURE_FAILURE" = stat ]; then exit 9; fi
if [ "$FIXTURE_FAILURE" = disk ]; then printf 'ext4\\n'; else printf 'tmpfs\\n'; fi
`,
      );
      const producer = path.join(bin, "producer");
      writeExecutable(producer, "#!/bin/sh\nexit 0\n");
      const installer = path.join(root, "install.sh");
      fs.writeFileSync(installer, source.replace("/usr/bin/python3", '"$FIXTURE_PYTHON_WRAPPER"'));
      const shell = `set -euo pipefail
source "$1"
readiness_producer_path() { printf '%s\\n' "$FIXTURE_PRODUCER"; }
install() { return 0; }
systemctl() { return 0; }
sync() { printf 'sync\\n' >>"$FIXTURE_EVENTS"; }
print_versions() { printf 'versions\\n' >>"$FIXTURE_EVENTS"; }
prepare_fast_boot
print_versions`;
      const env = {
        PATH: bin,
        HOME: root,
        FIXTURE_FAILURE: scenario.failure ?? "",
        FIXTURE_RUN: configuredRuntime,
        FIXTURE_CACHE: cache,
        FIXTURE_DATA: data,
        FIXTURE_SEED: seed,
        FIXTURE_CLI: cloudInit,
        FIXTURE_EVENTS: events,
        FIXTURE_PRODUCER: producer,
        FIXTURE_PYTHON: interpreter,
        FIXTURE_CLOUD_INIT_SCRIPT: cli,
        FIXTURE_MODULE_SCRIPT: fixture,
        FIXTURE_PYTHON_WRAPPER: wrapper,
      };
      const fails = Boolean(scenario.failure && scenario.failure !== "absent");
      for (let attempt = 0; attempt < 2; attempt++) {
        fs.writeFileSync(events, "");
        const result = spawnSync("/bin/bash", ["-c", shell, "installer-fixture", installer], {
          cwd,
          env,
          encoding: "utf8",
          timeout: 10000,
        });
        assertNoSubstitution();
        assert.equal(result.error, undefined);
        assert.equal(result.signal, null);
        if (fails) assert.notEqual(result.status, 0, `${scenario.name}: cleanup must fail closed`);
        else assert.equal(result.status, 0, result.stderr || result.stdout);
        if (scenario.cleaned) {
          const status = spawnSync(cloudInit, ["status", "--format=json"], {
            cwd,
            env: { ...env, FIXTURE_FAILURE: "" },
            encoding: "utf8",
            timeout: 10000,
          });
          assertNoSubstitution();
          assert.equal(status.status, 0, status.stderr);
          assert.equal(
            JSON.parse(status.stdout).status,
            "done",
            "clean must not leave dangling current-boot completion records",
          );
        }
        for (const [name, contents] of Object.entries(facts)) {
          const file = path.join(runtime, name);
          assert.equal(fs.readFileSync(file, "utf8"), contents);
          const original = originals[name];
          const actual = fs.statSync(file);
          assert.equal(actual.mode & 0o777, original.mode & 0o777);
          assert.equal(actual.uid, original.uid);
          assert.equal(actual.gid, original.gid);
          if (scenario.cleaned)
            assert.ok(fs.lstatSync(file).isFile(), "preserved facts must be regular runtime files");
        }
        assert.equal(fs.existsSync(data), !scenario.cleaned, "disk completion cache cleanup");
        assert.equal(fs.existsSync(seed), !scenario.cleaned, "seed cleanup");
        assert.deepEqual(
          fs.readFileSync(events, "utf8").trim().split("\n").filter(Boolean),
          [...(scenario.cleaned ? ["clean"] : []), ...(!fails ? ["sync", "versions"] : [])],
          "failed preparation must not sync or print success versions",
        );
        assert.deepEqual(
          fs.readdirSync(runtime).sort(),
          Object.keys(facts).sort(),
          "no temporary completion copies may remain",
        );
      }
      if (scenario.cleaned) {
        // A new boot does not inherit tmpfs facts or the cleaned disk cache/seed.
        for (const name of Object.keys(facts)) fs.unlinkSync(path.join(runtime, name));
        const nextBoot = spawnSync(cloudInit, ["status", "--format=json"], {
          cwd,
          env: { ...env, FIXTURE_FAILURE: "" },
          encoding: "utf8",
          timeout: 10000,
        });
        assertNoSubstitution();
        assert.equal(nextBoot.status, 0, nextBoot.stderr);
        assert.equal(JSON.parse(nextBoot.stdout).status, "notstarted");
      }
    });
  }
});

function writeFakeGPG(bin) {
	writeExecutable(
		path.join(bin, "gpg"),
		`#!/usr/bin/env bash
set -euo pipefail
mode=""
value=""
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --batch|--with-colons)
      shift
      ;;
    --import|--fingerprint|--export)
      mode="\${1#--}"
      value="\${2:-}"
      shift 2
      ;;
    *)
      printf 'unexpected gpg arg: %s\\n' "$1" >&2
      exit 64
      ;;
  esac
done
case "$mode" in
  import)
    [[ -s "$value" ]] || exit 66
    ;;
  fingerprint)
    printf 'pub:-:4096:1:7721F63BD38B4796:0::::::scSC::::::23::0:\\n'
    printf 'fpr:::::::::%s:\\n' "\${CRABBOX_FAKE_GPG_FINGERPRINT:-$value}"
    ;;
  export)
    printf 'fake-export:%s\\n' "$value"
    printf 'export:%s\\n' "$value" >>"$CRABBOX_FAKE_GPG_LOG"
    ;;
  *)
    exit 67
    ;;
esac
`,
	);
}

function runNeedRootFixture(uid, args = [], env = {}) {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-linux-tools-sudo-"));
	const bin = path.join(dir, "bin");
	const sudoLog = path.join(dir, "sudo.log");
	fs.mkdirSync(bin);
	writeExecutable(
		path.join(bin, "id"),
		`#!/usr/bin/env bash
[[ "$*" == "-u" ]] || exit 64
printf '${uid}\n'
`,
	);
	writeExecutable(
		path.join(bin, "sudo"),
		`#!/usr/bin/env bash
set -euo pipefail
printf 'arg=%s\n' "$@" >"$CRABBOX_FAKE_SUDO_LOG"
preserve_arg=""
for arg in "$@"; do
  case "$arg" in
    --preserve-env=*) preserve_arg="\${arg#--preserve-env=}" ;;
  esac
done
IFS=',' read -ra preserved <<<"$preserve_arg"
for name in "\${preserved[@]}"; do
  printf 'env=%s=%s\n' "$name" "\${!name-}" >>"$CRABBOX_FAKE_SUDO_LOG"
done
`,
	);

	return {
		result: spawnSync(
			"bash",
			["-c", `source scripts/install-linux-developer-tools.sh\nneed_root ${args.join(" ")}`],
			{
				cwd: repoRoot,
				env: {
					...process.env,
					...env,
					PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
					CRABBOX_FAKE_SUDO_LOG: sudoLog,
				},
				encoding: "utf8",
			},
		),
		sudoLog,
	};
}

test("linux developer tool setup isolates root HOME and preserves approved configuration", () => {
	const script = fs.readFileSync(
		path.join(repoRoot, "scripts/install-linux-developer-tools.sh"),
		"utf8",
	);
	const configured = [
		...script.matchAll(/\$\{(CRABBOX_LINUX_[A-Z0-9_]+):-/g),
	].map((match) => match[1]);
	const proxyEnv = [
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"NO_PROXY",
		"http_proxy",
		"https_proxy",
		"no_proxy",
		"ALL_PROXY",
		"all_proxy",
	];
	const expectedEnv = Object.fromEntries(
		[...configured, ...proxyEnv].map((name, index) => [
			name,
			`${name.toLowerCase()}-sentinel-${index}`,
		]),
	);
	const unrelatedName = "CRABBOX_UNRELATED_SENTINEL";
	const unrelatedValue = "must-not-cross-sudo";
	const { result, sudoLog } = runNeedRootFixture(1000, ["sentinel"], {
		...expectedEnv,
		[unrelatedName]: unrelatedValue,
	});
	assert.equal(result.status, 0, result.stderr || result.stdout);
	const lines = fs.readFileSync(sudoLog, "utf8").trim().split("\n");
	const args = lines.filter((line) => line.startsWith("arg=")).map((line) => line.slice(4));
	assert.equal(args[0], "-H");
	assert.doesNotMatch(args.join("\n"), /^-E$/m);
	const preserveArg = args.find((arg) => arg.startsWith("--preserve-env="));
	assert.ok(preserveArg, "sudo re-exec must preserve approved installer configuration");
	const preserved = preserveArg.slice("--preserve-env=".length).split(",");
	assert.deepEqual([...preserved].sort(), [...configured, ...proxyEnv].sort());
	assert.equal(preserved.includes("HOME"), false);
	assert.equal(preserved.includes(unrelatedName), false);
	const forwarded = Object.fromEntries(
		lines.filter((line) => line.startsWith("env=")).map((line) => {
			const separator = line.indexOf("=", 4);
			return [line.slice(4, separator), line.slice(separator + 1)];
		}),
	);
	assert.deepEqual(forwarded, expectedEnv);
	assert.doesNotMatch(lines.join("\n"), new RegExp(`${unrelatedName}|${unrelatedValue}`));
});

test("linux developer tool setup does not invoke sudo when already root", () => {
	const { result, sudoLog } = runNeedRootFixture(0);
	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.equal(fs.existsSync(sudoLog), false);
});

test("linux developer tool repository setup rewrites keyrings idempotently", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-linux-tools-"));
	const bin = path.join(dir, "bin");
	const keyrings = path.join(dir, "keyrings");
	const sources = path.join(dir, "sources");
	const aptConf = path.join(dir, "apt-conf");
	const chromePolicy = path.join(dir, "chrome-policy");
	const chromiumPolicy = path.join(dir, "chromium-policy");
	const browserBin = path.join(dir, "browser-bin");
	const browserState = path.join(dir, "browser-state");
	const chromeDefaults = path.join(dir, "defaults", "google-chrome");
	const osRelease = path.join(dir, "os-release");
	const log = path.join(dir, "gpg.log");
	fs.mkdirSync(bin);
	fs.writeFileSync(osRelease, "ID='ubuntu'\nVERSION_CODENAME='noble'\n", "utf8");

	writeExecutable(
		path.join(bin, "curl"),
		`#!/usr/bin/env bash
set -euo pipefail
printf 'fake-key:%s\\n' "$*"
`,
	);
	writeFakeGPG(bin);
	writeExecutable(
		path.join(bin, "node"),
		`#!/usr/bin/env bash
printf 'v24.0.0\\n'
`,
	);
	writeExecutable(
		path.join(bin, "google-chrome"),
		`#!/usr/bin/env bash
exit 0
`,
	);
	writeExecutable(
		path.join(bin, "dpkg"),
		`#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == "--print-architecture" ]]; then
  printf 'amd64\\n'
  exit 0
fi
exit 64
`,
	);
	writeExecutable(
		path.join(bin, "dpkg-query"),
		`#!/usr/bin/env bash
exit 1
`,
	);
	writeExecutable(
		path.join(bin, "apt-get"),
		`#!/usr/bin/env bash
exit 0
`,
	);
	writeExecutable(
		path.join(bin, "apt-cache"),
		`#!/usr/bin/env bash
exit 1
`,
	);

	const result = spawnSync(
		"bash",
		[
			"-c",
			[
				"set -euo pipefail",
				// Model the broken image: matching Node, but no npm or corepack.
				"command() {",
					'  if [[ "$*" == "-v npm" || "$*" == "-v corepack" ]]; then return 1; fi',
					'  builtin command "$@"',
				"}",
				"source scripts/install-linux-developer-tools.sh",
				"add_nodesource",
				"add_nodesource",
				"add_docker_repo",
				"add_docker_repo",
				"install_chrome_or_chromium",
				"install_chrome_or_chromium",
			].join("\n"),
		],
		{
			cwd: repoRoot,
			env: {
				...process.env,
				PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
				CRABBOX_FAKE_GPG_LOG: log,
				CRABBOX_LINUX_NODE_MAJOR: "22",
				CRABBOX_LINUX_APT_KEYRINGS_DIR: keyrings,
				CRABBOX_LINUX_APT_SOURCES_DIR: sources,
				CRABBOX_LINUX_APT_CONF_DIR: aptConf,
				CRABBOX_LINUX_OS_RELEASE_FILE: osRelease,
				CRABBOX_LINUX_CHROME_POLICY_DIR: chromePolicy,
				CRABBOX_LINUX_CHROMIUM_POLICY_DIR: chromiumPolicy,
				CRABBOX_LINUX_BROWSER_BIN_DIR: browserBin,
				CRABBOX_LINUX_BROWSER_STATE_DIR: browserState,
				CRABBOX_LINUX_CHROME_DEFAULTS_FILE: chromeDefaults,
			},
			encoding: "utf8",
		},
	);

	assert.equal(result.status, 0, result.stderr || result.stdout);
	for (const name of ["nodesource.gpg", "docker.gpg", "google-linux.gpg"]) {
		assert.equal(fs.existsSync(path.join(keyrings, name)), true, `missing ${name}`);
	}
	assert.equal(
		fs.readdirSync(keyrings).filter((name) => name.includes(".tmp.")).length,
		0,
		"temporary keyring directories should be removed",
	);
	assert.equal(fs.readFileSync(log, "utf8").trim().split("\n").length, 6);
	assert.match(fs.readFileSync(path.join(sources, "nodesource.list"), "utf8"), new RegExp(`signed-by=${keyrings}/nodesource.gpg`));
	assert.match(fs.readFileSync(path.join(sources, "docker.list"), "utf8"), new RegExp(`signed-by=${keyrings}/docker.gpg`));
	assert.match(fs.readFileSync(path.join(sources, "crabbox-google-chrome.list"), "utf8"), new RegExp(`signed-by=${keyrings}/google-linux.gpg`));
	assert.equal(fs.existsSync(path.join(sources, "google-chrome.list")), false);
	assert.equal(fs.existsSync(path.join(sources, "google-chrome.sources")), false);
	assert.match(fs.readFileSync(chromeDefaults, "utf8"), /repo_add_once="false"/);
	assert.match(fs.readFileSync(chromeDefaults, "utf8"), /repo_reenable_on_distupgrade="false"/);
	assert.equal(fs.existsSync(path.join(chromePolicy, "crabbox.json")), true);
	assert.equal(fs.existsSync(path.join(chromiumPolicy, "crabbox.json")), true);
	assert.equal(fs.existsSync(path.join(browserBin, "crabbox-browser")), true);
	assert.match(fs.readFileSync(path.join(browserState, "browser.env"), "utf8"), new RegExp(`CHROME_BIN=${browserBin}/crabbox-browser`));
	assert.equal(
		fs.readFileSync(path.join(keyrings, "nodesource.gpg"), "utf8"),
		`fake-export:${nodesourceSigningKeyFingerprint}\n`,
	);
	assert.equal(
		fs.readFileSync(path.join(keyrings, "docker.gpg"), "utf8"),
		`fake-export:${dockerSigningKeyFingerprint}\n`,
	);
	assert.equal(
		fs.readFileSync(path.join(keyrings, "google-linux.gpg"), "utf8"),
		`fake-export:${googleLinuxSigningKeyFingerprint}\n`,
	);
});

for (const repository of [
	{
		name: "NodeSource",
		functionCall: "add_nodesource",
		keyring: "nodesource.gpg",
		source: "nodesource.list",
		expectedFingerprint: nodesourceSigningKeyFingerprint,
	},
	{
		name: "Docker",
		functionCall: "docker_packages_installed() { return 0; }; add_docker_repo",
		keyring: "docker.gpg",
		source: "docker.list",
		expectedFingerprint: dockerSigningKeyFingerprint,
	},
]) {
	test(`linux developer tool setup preserves installed ${repository.name} trust files on fingerprint mismatch`, () => {
		const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-linux-tools-repo-mismatch-"));
		const bin = path.join(dir, "bin");
		const keyrings = path.join(dir, "keyrings");
		const sources = path.join(dir, "sources");
		const osRelease = path.join(dir, "os-release");
		const target = path.join(keyrings, repository.keyring);
		const source = path.join(sources, repository.source);
		const log = path.join(dir, "gpg.log");
		fs.mkdirSync(bin);
		fs.mkdirSync(keyrings);
		fs.mkdirSync(sources);
		fs.writeFileSync(osRelease, "ID='ubuntu'\nVERSION_CODENAME='noble'\n", "utf8");
		fs.writeFileSync(target, "existing-keyring\n", "utf8");
		fs.writeFileSync(source, "existing-source\n", "utf8");
		fs.writeFileSync(log, "", "utf8");
		writeExecutable(
			path.join(bin, "curl"),
			`#!/usr/bin/env bash
set -euo pipefail
printf 'unexpected-key\n'
`,
		);
		writeExecutable(
			path.join(bin, "dpkg"),
			`#!/usr/bin/env bash
[[ "$*" == "--print-architecture" ]] && printf 'amd64\n'
`,
		);
		writeFakeGPG(bin);

		const result = spawnSync(
			"bash",
			["-c", ["set -euo pipefail", "source scripts/install-linux-developer-tools.sh", repository.functionCall].join("\n")],
			{
				cwd: repoRoot,
				env: {
					...process.env,
					PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
					CRABBOX_FAKE_GPG_FINGERPRINT: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
					CRABBOX_FAKE_GPG_LOG: log,
					CRABBOX_LINUX_NODE_MAJOR: "22",
					CRABBOX_LINUX_APT_KEYRINGS_DIR: keyrings,
					CRABBOX_LINUX_APT_SOURCES_DIR: sources,
					CRABBOX_LINUX_OS_RELEASE_FILE: osRelease,
				},
				encoding: "utf8",
			},
		);

		assert.notEqual(result.status, 0, "mismatched repository keys must fail closed");
		assert.equal(fs.readFileSync(target, "utf8"), "existing-keyring\n");
		assert.equal(fs.readFileSync(source, "utf8"), "existing-source\n");
		assert.equal(fs.readFileSync(log, "utf8"), "", "mismatched keys should not be exported");
		assert.equal(
			fs.readdirSync(keyrings).filter((name) => name.includes(".tmp.")).length,
			0,
			"temporary keyring directories should be removed",
		);
	});

	test(`linux developer tool setup refreshes installed ${repository.name} trust files after fingerprint verification`, () => {
		const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-linux-tools-repo-refresh-"));
		const bin = path.join(dir, "bin");
		const keyrings = path.join(dir, "keyrings");
		const sources = path.join(dir, "sources");
		const osRelease = path.join(dir, "os-release");
		const target = path.join(keyrings, repository.keyring);
		const source = path.join(sources, repository.source);
		const log = path.join(dir, "gpg.log");
		fs.mkdirSync(bin);
		fs.mkdirSync(keyrings);
		fs.mkdirSync(sources);
		fs.writeFileSync(osRelease, "ID='ubuntu'\nVERSION_CODENAME='noble'\n", "utf8");
		fs.writeFileSync(target, "existing-keyring\n", "utf8");
		fs.writeFileSync(source, "existing-source\n", "utf8");
		fs.writeFileSync(log, "", "utf8");
		writeExecutable(
			path.join(bin, "curl"),
			`#!/usr/bin/env bash
set -euo pipefail
printf 'reviewed-key\n'
`,
		);
		writeExecutable(
			path.join(bin, "dpkg"),
			`#!/usr/bin/env bash
[[ "$*" == "--print-architecture" ]] && printf 'amd64\n'
`,
		);
		writeFakeGPG(bin);

		const result = spawnSync(
			"bash",
			["-c", ["set -euo pipefail", "source scripts/install-linux-developer-tools.sh", repository.functionCall].join("\n")],
			{
				cwd: repoRoot,
				env: {
					...process.env,
					PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
					CRABBOX_FAKE_GPG_LOG: log,
					CRABBOX_LINUX_NODE_MAJOR: "22",
					CRABBOX_LINUX_APT_KEYRINGS_DIR: keyrings,
					CRABBOX_LINUX_APT_SOURCES_DIR: sources,
					CRABBOX_LINUX_OS_RELEASE_FILE: osRelease,
				},
				encoding: "utf8",
			},
		);

		assert.equal(result.status, 0, result.stderr || result.stdout);
		assert.equal(fs.readFileSync(target, "utf8"), `fake-export:${repository.expectedFingerprint}\n`);
		assert.notEqual(fs.readFileSync(source, "utf8"), "existing-source\n");
		assert.match(fs.readFileSync(source, "utf8"), new RegExp(`signed-by=${keyrings}/${repository.keyring}`));
	});
}

test("linux developer tool setup preserves Google trust files and falls back on fingerprint mismatch", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-linux-tools-mismatch-"));
	const bin = path.join(dir, "bin");
	const keyrings = path.join(dir, "keyrings");
	const sources = path.join(dir, "sources");
	const chromePolicy = path.join(dir, "chrome-policy");
	const chromiumPolicy = path.join(dir, "chromium-policy");
	const browserBin = path.join(dir, "browser-bin");
	const browserState = path.join(dir, "browser-state");
	const target = path.join(keyrings, "google-linux.gpg");
	const source = path.join(sources, "crabbox-google-chrome.list");
	const log = path.join(dir, "gpg.log");
	fs.mkdirSync(bin);
	fs.mkdirSync(keyrings);
	fs.mkdirSync(sources);
	fs.writeFileSync(target, "existing-keyring\n", "utf8");
	fs.writeFileSync(source, "existing-source\n", "utf8");
	fs.writeFileSync(log, "", "utf8");
	writeExecutable(
		path.join(bin, "curl"),
		`#!/usr/bin/env bash
set -euo pipefail
printf 'fake-key:%s\\n' "$*"
`,
	);
	writeExecutable(
		path.join(bin, "dpkg"),
		`#!/usr/bin/env bash
[[ "$*" == "--print-architecture" ]] && printf 'amd64\\n'
`,
	);
	writeExecutable(
		path.join(bin, "apt-cache"),
		`#!/usr/bin/env bash
[[ "$*" == "show chromium" ]]
`,
	);
	writeExecutable(
		path.join(bin, "apt-get"),
		`#!/usr/bin/env bash
exit 0
`,
	);
	writeExecutable(
		path.join(bin, "chromium"),
		`#!/usr/bin/env bash
exit 0
`,
	);
	writeFakeGPG(bin);

	const result = spawnSync(
		"bash",
		[
			"-c",
			[
				"set -euo pipefail",
				"source scripts/install-linux-developer-tools.sh",
				"install_chrome_or_chromium",
			].join("\n"),
		],
		{
			cwd: repoRoot,
			env: {
				...process.env,
				PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
				CRABBOX_FAKE_GPG_FINGERPRINT: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
				CRABBOX_FAKE_GPG_LOG: log,
				CRABBOX_LINUX_APT_KEYRINGS_DIR: keyrings,
				CRABBOX_LINUX_APT_SOURCES_DIR: sources,
				CRABBOX_LINUX_CHROME_POLICY_DIR: chromePolicy,
				CRABBOX_LINUX_CHROMIUM_POLICY_DIR: chromiumPolicy,
				CRABBOX_LINUX_BROWSER_BIN_DIR: browserBin,
				CRABBOX_LINUX_BROWSER_STATE_DIR: browserState,
			},
			encoding: "utf8",
		},
	);

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stderr, /verification failed; trying Chromium fallback/);
	assert.equal(fs.readFileSync(target, "utf8"), "existing-keyring\n");
	assert.equal(fs.readFileSync(source, "utf8"), "existing-source\n");
	assert.equal(fs.readFileSync(log, "utf8"), "", "mismatched keys should not be exported");
	assert.match(fs.readFileSync(path.join(browserBin, "crabbox-browser"), "utf8"), new RegExp(`${bin}/chromium`));
	assert.equal(
		fs.readdirSync(keyrings).filter((name) => name.includes(".tmp.")).length,
		0,
		"temporary keyring directories should be removed",
	);
});

function installTruffleHogFixture(dir) {
	const bin = path.join(dir, "bin");
	const targetBin = path.join(dir, "target-bin");
	const downloadLog = path.join(dir, "download.log");
	const checksumLog = path.join(dir, "checksum.log");
	fs.mkdirSync(bin);
	fs.mkdirSync(targetBin);
	writeExecutable(
		path.join(bin, "dpkg"),
		`#!/usr/bin/env bash
[[ "$*" == "--print-architecture" ]] && printf 'amd64\n'
`,
	);
	writeExecutable(
		path.join(bin, "curl"),
		`#!/usr/bin/env bash
set -euo pipefail
output=""
printf '%s\n' "$*" > "$CRABBOX_FAKE_DOWNLOAD_LOG"
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    -o|--output)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
printf 'fake archive\n' > "$output"
`,
	);
	writeExecutable(
		path.join(bin, "sha256sum"),
		`#!/usr/bin/env bash
set -euo pipefail
cat > "$CRABBOX_FAKE_CHECKSUM_LOG"
[[ "\${CRABBOX_FAKE_CHECKSUM_RESULT:-pass}" == "pass" ]]
`,
	);
	writeExecutable(
		path.join(bin, "tar"),
		`#!/usr/bin/env bash
set -euo pipefail
output_dir=""
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    -C)
      output_dir="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
cat > "$output_dir/trufflehog" <<'SCRIPT'
#!/usr/bin/env bash
printf 'trufflehog %s\n' "\${CRABBOX_FAKE_TRUFFLEHOG_VERSION:-3.95.9}"
SCRIPT
chmod 0755 "$output_dir/trufflehog"
`,
	);
	return { bin, targetBin, downloadLog, checksumLog };
}

test("linux developer image installs pinned TruffleHog after checksum verification", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-linux-trufflehog-"));
	const fixture = installTruffleHogFixture(dir);
	const result = spawnSync(
		"bash",
		["-c", "set -euo pipefail\nsource scripts/install-linux-developer-tools.sh\ninstall_trufflehog"],
		{
			cwd: repoRoot,
			env: {
				...process.env,
				PATH: `${fixture.bin}${path.delimiter}${process.env.PATH ?? ""}`,
				CRABBOX_FAKE_CHECKSUM_LOG: fixture.checksumLog,
				CRABBOX_FAKE_DOWNLOAD_LOG: fixture.downloadLog,
				CRABBOX_LINUX_TRUFFLEHOG_BIN_DIR: fixture.targetBin,
			},
			encoding: "utf8",
		},
	);

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(
		fs.readFileSync(fixture.downloadLog, "utf8"),
		/trufflehog\/releases\/download\/v3\.95\.9\/trufflehog_3\.95\.9_linux_amd64\.tar\.gz/,
	);
	assert.match(
		fs.readFileSync(fixture.checksumLog, "utf8"),
		/^f6d1106b85107d79527ed7a5b98b592beadd8b770dc3c9e8c1ad99e1b2cf127e  trufflehog_3\.95\.9_linux_amd64\.tar\.gz\n$/,
	);
	assert.equal(
		spawnSync(path.join(fixture.targetBin, "trufflehog"), ["--version"], {
			encoding: "utf8",
		}).stdout,
		"trufflehog 3.95.9\n",
	);
});

test("linux developer image re-verifies an existing same-version TruffleHog binary", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-linux-trufflehog-existing-"));
	const fixture = installTruffleHogFixture(dir);
	const target = path.join(fixture.targetBin, "trufflehog");
	writeExecutable(target, "#!/usr/bin/env bash\nprintf 'trufflehog 3.95.9\\n'\nprintf 'untrusted\\n'\n");
	const result = spawnSync(
		"bash",
		["-c", "set -euo pipefail\nsource scripts/install-linux-developer-tools.sh\ninstall_trufflehog"],
		{
			cwd: repoRoot,
			env: {
				...process.env,
				PATH: `${fixture.bin}${path.delimiter}${process.env.PATH ?? ""}`,
				CRABBOX_FAKE_CHECKSUM_LOG: fixture.checksumLog,
				CRABBOX_FAKE_DOWNLOAD_LOG: fixture.downloadLog,
				CRABBOX_LINUX_TRUFFLEHOG_BIN_DIR: fixture.targetBin,
			},
			encoding: "utf8",
		},
	);

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(fs.readFileSync(fixture.downloadLog, "utf8"), /trufflehog_3\.95\.9_linux_amd64/);
	assert.equal(
		spawnSync(target, ["--version"], { encoding: "utf8" }).stdout,
		"trufflehog 3.95.9\n",
	);
});

test("linux developer image keeps the existing TruffleHog binary when verification fails", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-linux-trufflehog-failure-"));
	const fixture = installTruffleHogFixture(dir);
	const target = path.join(fixture.targetBin, "trufflehog");
	writeExecutable(target, "#!/usr/bin/env bash\nprintf 'trufflehog 3.95.8\\n'\n");
	const result = spawnSync(
		"bash",
		["-c", "set -euo pipefail\nsource scripts/install-linux-developer-tools.sh\ninstall_trufflehog"],
		{
			cwd: repoRoot,
			env: {
				...process.env,
				PATH: `${fixture.bin}${path.delimiter}${process.env.PATH ?? ""}`,
				CRABBOX_FAKE_CHECKSUM_LOG: fixture.checksumLog,
				CRABBOX_FAKE_CHECKSUM_RESULT: "fail",
				CRABBOX_FAKE_DOWNLOAD_LOG: fixture.downloadLog,
				CRABBOX_LINUX_TRUFFLEHOG_BIN_DIR: fixture.targetBin,
			},
			encoding: "utf8",
		},
	);

	assert.notEqual(result.status, 0, "checksum failures must stop image preparation");
	assert.equal(fs.readFileSync(target, "utf8"), "#!/usr/bin/env bash\nprintf 'trufflehog 3.95.8\\n'\n");
});

test("linux developer image keeps the existing TruffleHog binary when candidate validation fails", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-linux-trufflehog-invalid-"));
	const fixture = installTruffleHogFixture(dir);
	const target = path.join(fixture.targetBin, "trufflehog");
	const existing = "#!/usr/bin/env bash\nprintf 'trufflehog 3.95.8\\n'\n";
	writeExecutable(target, existing);
	const result = spawnSync(
		"bash",
		["-c", "set -euo pipefail\nsource scripts/install-linux-developer-tools.sh\ninstall_trufflehog"],
		{
			cwd: repoRoot,
			env: {
				...process.env,
				PATH: `${fixture.bin}${path.delimiter}${process.env.PATH ?? ""}`,
				CRABBOX_FAKE_CHECKSUM_LOG: fixture.checksumLog,
				CRABBOX_FAKE_DOWNLOAD_LOG: fixture.downloadLog,
				CRABBOX_FAKE_TRUFFLEHOG_VERSION: "0.0.0",
				CRABBOX_LINUX_TRUFFLEHOG_BIN_DIR: fixture.targetBin,
			},
			encoding: "utf8",
		},
	);

	assert.notEqual(result.status, 0, "invalid downloaded binaries must stop image preparation");
	assert.equal(fs.readFileSync(target, "utf8"), existing);
});

test("linux developer image reports TruffleHog from the configured install directory", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-linux-trufflehog-version-"));
	const fixture = installTruffleHogFixture(dir);
	const osRelease = path.join(dir, "os-release");
	fs.writeFileSync(osRelease, "PRETTY_NAME='Test Linux'\n", "utf8");
	writeExecutable(
		path.join(fixture.targetBin, "trufflehog"),
		"#!/usr/bin/env bash\nprintf 'trufflehog 3.95.9\\n'\n",
	);
	for (const command of ["git", "gh", "jq", "rg", "fd", "python3", "node", "npm", "corepack", "pnpm", "docker"]) {
		writeExecutable(
			path.join(fixture.bin, command),
			`#!/usr/bin/env bash\nprintf '${command} test-version\\n'\n`,
		);
	}
	const result = spawnSync(
		"bash",
		["-c", "set -euo pipefail\nsource scripts/install-linux-developer-tools.sh\nprint_versions"],
		{
			cwd: repoRoot,
			env: {
				...process.env,
				PATH: `${fixture.bin}${path.delimiter}/usr/bin:/bin`,
				CRABBOX_LINUX_OS_RELEASE_FILE: osRelease,
				CRABBOX_LINUX_TRUFFLEHOG_BIN_DIR: fixture.targetBin,
			},
			encoding: "utf8",
		},
	);

	assert.equal(result.status, 0, result.stderr || result.stdout);
	assert.match(result.stdout, /trufflehog 3\.95\.9/);
});

test("linux developer image keeps pinned TruffleHog probes update-free", () => {
	const script = fs.readFileSync(path.join(repoRoot, "scripts/install-linux-developer-tools.sh"), "utf8");
	assert.match(script, /"\$binary" --no-update --version/);
	assert.match(script, /"\$trufflehog_bin_dir\/trufflehog" --no-update --version/);
});
