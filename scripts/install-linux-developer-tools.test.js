import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
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
output=""
value=""
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --batch|--yes|--with-colons)
      shift
      ;;
    --dearmor)
      mode="dearmor"
      shift
      ;;
    --import|--fingerprint|--export)
      mode="\${1#--}"
      value="\${2:-}"
      shift 2
      ;;
    -o)
      output="\${2:-}"
      shift 2
      ;;
    *)
      printf 'unexpected gpg arg: %s\\n' "$1" >&2
      exit 64
      ;;
  esac
done
case "$mode" in
  dearmor)
    [[ -n "$output" ]] || exit 65
    cat >"$output"
    printf '%s\\n' "$output" >>"$CRABBOX_FAKE_GPG_LOG"
    ;;
  import)
    [[ -s "$value" ]] || exit 66
    ;;
  fingerprint)
    printf 'pub:-:4096:1:7721F63BD38B4796:0::::::scSC::::::23::0:\\n'
    printf 'fpr:::::::::%s:\\n' "\${CRABBOX_FAKE_GPG_FINGERPRINT:-${googleLinuxSigningKeyFingerprint}}"
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
	assert.match(fs.readFileSync(path.join(sources, "google-chrome.list"), "utf8"), new RegExp(`signed-by=${keyrings}/google-linux.gpg`));
	assert.equal(fs.existsSync(path.join(chromePolicy, "crabbox.json")), true);
	assert.equal(fs.existsSync(path.join(chromiumPolicy, "crabbox.json")), true);
	assert.equal(fs.existsSync(path.join(browserBin, "crabbox-browser")), true);
	assert.match(fs.readFileSync(path.join(browserState, "browser.env"), "utf8"), new RegExp(`CHROME_BIN=${browserBin}/crabbox-browser`));
	assert.equal(
		fs.readFileSync(path.join(keyrings, "google-linux.gpg"), "utf8"),
		`fake-export:${googleLinuxSigningKeyFingerprint}\n`,
	);
});

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
	const source = path.join(sources, "google-chrome.list");
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
