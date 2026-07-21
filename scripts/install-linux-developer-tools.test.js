import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const nodesourceSigningKeyFingerprint = "6F71F525282841EEDAF851B42F59B5F99B1BE0B4";
const dockerSigningKeyFingerprint = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88";
const googleLinuxSigningKeyFingerprint = "EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796";

function writeExecutable(file, body) {
	fs.writeFileSync(file, body, "utf8");
	fs.chmodSync(file, 0o755);
}

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
		functionCall: "node_toolchain_ready() { return 0; }; add_nodesource",
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
