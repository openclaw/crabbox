import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const verifier = path.join(repoRoot, "scripts", "verify-homebrew-release.sh");
const source = fs.readFileSync(verifier, "utf8");
const credentialGuards = `${source}\n${fs.readFileSync(
  path.join(repoRoot, "scripts", "release-config.sh"),
  "utf8",
)}`;

const hashes = {
  darwinAmd64: "a".repeat(64),
  darwinArm64: "b".repeat(64),
  linuxAmd64: "c".repeat(64),
  linuxArm64: "d".repeat(64),
};
const forbiddenCredentials = [
  "GH_TOKEN",
  "GITHUB_TOKEN",
  "GH_ENTERPRISE_TOKEN",
  "GITHUB_ENTERPRISE_TOKEN",
  "HOMEBREW_GITHUB_API_TOKEN",
  "HOMEBREW_GITHUB_PACKAGES_TOKEN",
  "HOMEBREW_TAP_GITHUB_TOKEN",
  "HOMEBREW_TAP_TOKEN",
  "ACTIONS_RUNTIME_TOKEN",
  "ACTIONS_ID_TOKEN_REQUEST_TOKEN",
  "CODESIGN_IDENTITY",
  "CODESIGN_KEYCHAIN",
  "MACOS_CODESIGN_IDENTITY",
  "MACOS_SIGNING_CERT_BASE64",
  "MACOS_SIGNING_CERT_PASSWORD",
  "MAC_RELEASE_OP_ACCOUNT",
  "MAC_RELEASE_OP_FIELDS",
  "MAC_RELEASE_OP_ITEM",
  "MAC_RELEASE_OP_VAULT",
  "MAC_RELEASE_CODESIGN_IDENTITY",
  "MAC_RELEASE_CODESIGN_KEYCHAIN",
  "MAC_RELEASE_CODESIGN_KEYCHAIN_PASSWORD",
  "MAC_RELEASE_CODESIGN_OP_ACCOUNT",
  "MAC_RELEASE_CODESIGN_OP_ITEM",
  "MAC_RELEASE_CODESIGN_OP_VAULT",
  "NOTARYTOOL_KEYCHAIN_PROFILE",
  "NOTARYTOOL_APPLE_ID",
  "NOTARYTOOL_PASSWORD",
  "NOTARYTOOL_TEAM_ID",
  "APPLE_ID",
  "APPLE_API_ISSUER",
  "APPLE_API_KEY",
  "APPLE_APP_SPECIFIC_PASSWORD",
  "APPLE_TEAM_ID",
  "AC_USERNAME",
  "AC_PASSWORD",
  "AC_PROVIDER",
  "ASC_KEY_ID",
  "ASC_ISSUER_ID",
  "ASC_PRIVATE_KEY",
  "OP_SERVICE_ACCOUNT_TOKEN",
];

function formulaMetadata(checksum = hashes.darwinArm64, arch = "arm64") {
  return { formulae: [{
    name: "crabbox", full_name: "openclaw/tap/crabbox", tap: "openclaw/tap",
    versions: { stable: "1.2.3" },
    urls: { stable: {
      url: `https://github.com/openclaw/crabbox/releases/download/v1.2.3/crabbox_1.2.3_darwin_${arch}.tar.gz`,
      checksum,
    } },
  }] };
}

function verifyFormula(metadata) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-homebrew-formula-"));
  const metadataPath = path.join(root, "formula.json");
  fs.writeFileSync(metadataPath, JSON.stringify(metadata));
  try {
    return spawnSync("/bin/bash", [
      "-c", 'source "$1"; verify_homebrew_formula "$2" "$3" v1.2.3 crabbox_1.2.3_darwin_arm64.tar.gz "$4"',
      "homebrew-formula-test", verifier, process.execPath, metadataPath, hashes.darwinArm64,
    ], { encoding: "utf8" });
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
}

function sha256(file) {
  return createHash("sha256").update(fs.readFileSync(file)).digest("hex");
}

function shellQuote(value) {
  return `'${String(value).replaceAll("'", `'"'"'`)}'`;
}

function writeExecutable(file, contents) {
  fs.writeFileSync(file, contents);
  fs.chmodSync(file, 0o755);
}

function runMockedHomebrewPhase({
  useLauncher = false,
  nativeArch = "arm64",
  alreadyInstalled = false,
  installedArch,
  helperMissing = false,
  helperOnIntel = false,
  helperByteMismatch = false,
  fetchFailure = false,
  installedByteMismatch = false,
  macosVerifierFailure = false,
  useNativeVerifier = false,
  signatureFailure = false,
  notarizationFailure = false,
  releaseTrust = true,
  versionOutput = "1.2.3",
} = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-homebrew-phase-"));
  const assets = path.join(root, "assets");
  const payload = path.join(root, "payload");
  const fakeRoot = path.join(root, "repo");
  const fakeScripts = path.join(fakeRoot, "scripts");
  const mockBin = path.join(root, "bin");
  const prefix = path.join(root, "prefix");
  const work = path.join(root, "work");
  const home = path.join(work, "home");
  const cache = path.join(work, "cache");
  const log = path.join(root, "calls.log");
  for (const directory of [
    assets,
    payload,
    fakeScripts,
    mockBin,
    prefix,
    work,
    home,
    cache,
  ]) {
    fs.mkdirSync(directory, { recursive: true, mode: 0o700 });
  }

  const credentialCanary = forbiddenCredentials.concat("UNRELATED_SECRET").map(
    (name) => `[ -z "\${${name}+x}" ] || exit 95`,
  ).join("\n") + "\n";
  const cli = path.join(payload, "crabbox");
  const helper = path.join(payload, "crabbox-apple-vm-helper");
  writeExecutable(
    cli,
    `#!/bin/sh\n${credentialCanary}[ "\${1:-}" = --version ] || exit 64\nprintf '%s\\n' ${shellQuote(versionOutput)}\n`,
  );
  writeExecutable(
    helper,
    `#!/bin/sh\n${credentialCanary}[ "\${1:-}" = vmd-info ] || exit 64\nprintf '%s\\n' ${shellQuote(
      JSON.stringify({
        embedded: true,
        releaseTrust,
        trustPolicyVersion: 1,
        sha256: "e".repeat(64),
      }),
    )}\n`,
  );

  const archivePaths = {
    darwinAmd64: path.join(assets, "crabbox_1.2.3_darwin_amd64.tar.gz"),
    darwinArm64: path.join(assets, "crabbox_1.2.3_darwin_arm64.tar.gz"),
    linuxAmd64: path.join(assets, "crabbox_1.2.3_linux_amd64.tar.gz"),
    linuxArm64: path.join(assets, "crabbox_1.2.3_linux_arm64.tar.gz"),
  };
  execFileSync("tar", ["-czf", archivePaths.darwinAmd64, "-C", payload, "crabbox"]);
  execFileSync("tar", [
    "-czf",
    archivePaths.darwinArm64,
    "-C",
    payload,
    "crabbox",
    "crabbox-apple-vm-helper",
  ]);
  fs.writeFileSync(archivePaths.linuxAmd64, "mock linux amd64 archive\n");
  fs.writeFileSync(archivePaths.linuxArm64, "mock linux arm64 archive\n");
  const archiveHashes = Object.fromEntries(
    Object.entries(archivePaths).map(([key, file]) => [key, sha256(file)]),
  );
  const formulaPath = path.join(root, "formula.json");
  fs.writeFileSync(formulaPath, JSON.stringify(formulaMetadata(
    nativeArch === "arm64" ? archiveHashes.darwinArm64 : archiveHashes.darwinAmd64,
    nativeArch === "arm64" ? "arm64" : "amd64",
  )));
  fs.writeFileSync(
    path.join(assets, "provenance.json"),
    JSON.stringify({
      payloads: [
        {
          binaries: [
            {
              name: "crabbox-apple-vm-helper",
              embeddedVmd: { sha256: "e".repeat(64) },
            },
          ],
        },
      ],
    }),
  );

  writeExecutable(
    path.join(fakeScripts, "verify-release.sh"),
    `#!/bin/sh\n${credentialCanary}[ "$CRABBOX_VERIFY_MODE" = static ] || exit 96\nprintf 'verify-release:%s\\n' "$*" >>${shellQuote(log)}\n`,
  );
  writeExecutable(
    path.join(fakeScripts, "verify-macos-binary.sh"),
    `#!/bin/sh\nprintf 'verify-macos:%s\\n' "$*" >>${shellQuote(log)}\n${
      macosVerifierFailure ? "exit 73\n" : ""
    }${useNativeVerifier ? `exec /bin/bash ${shellQuote(path.join(repoRoot, "scripts/verify-macos-binary.sh"))} "$@"\n` : ""}`,
  );
  writeExecutable(
    path.join(mockBin, "uname"),
    `#!/bin/sh\ncase "\${1:-}" in\n  -s) printf 'Darwin\\n' ;;\n  -m) printf '${nativeArch}\\n' ;;\n  *) exit 64 ;;\nesac\n`,
  );
  writeExecutable(path.join(mockBin, "lipo"), `#!/bin/sh
[ "$1" = -archs ] && [ "$#" = 2 ] || exit 64
printf '${installedArch ?? nativeArch}\\n'
`);

  writeExecutable(path.join(mockBin, "csreq"), `#!/bin/sh
[ "$1" = -r ] && [ "$3" = -t ] && [ "$#" = 3 ] || exit 98
printf '%s\\n' "$2"
`);
  // Required by the native verifier, but only invoked for VMD entitlements.
  writeExecutable(path.join(mockBin, "plutil"), `#!/bin/sh
printf 'unexpected plutil invocation\\n' >&2
exit 98
`);
  writeExecutable(path.join(mockBin, "codesign"), `#!/bin/bash
set -eu
${credentialCanary}printf 'codesign:%s\\n' "$*" >>${shellQuote(log)}
source ${shellQuote(path.join(repoRoot, "scripts/release-config.sh"))}
binary=\${!#}
case "$binary" in
  */bin/crabbox) identifier=$CRABBOX_RELEASE_CLI_IDENTIFIER ;;
  */bin/crabbox-apple-vm-helper) identifier=$CRABBOX_RELEASE_HELPER_IDENTIFIER ;;
  *) exit 98 ;;
esac
case "$1" in
  --verify)
    if [[ "$*" == *--check-notarization* ]]; then
      [[ "$*" == "--verify --strict --check-notarization -R=notarized --verbose=2 $binary" ]] || exit 98
      ${notarizationFailure ? "exit 74" : "exit 0"}
    fi
    requirement=$(crabbox_release_designated_requirement "$identifier")
    [[ "$*" == "--verify --strict -R=$requirement --verbose=2 $binary" ]] || exit 98
    ${signatureFailure ? "exit 73" : "exit 0"}
    ;;
  -dvvv)
    [[ "$#" == 2 ]] || exit 98
    printf '%s\\n' "Identifier=$identifier" "Authority=$CRABBOX_RELEASE_AUTHORITY" "TeamIdentifier=$CRABBOX_RELEASE_TEAM_ID" 'CodeDirectory flags=0x10000(runtime)' 'Timestamp=Sep 1, 2026'
    ;;
  -d)
    [[ "$2" == -r- && "$#" == 3 ]] || exit 98
    printf 'designated => %s\\n' "$(crabbox_release_designated_requirement "$identifier")"
    ;;
  *) exit 98 ;;
esac
`);

  const installedCli = path.join(prefix, "bin", "crabbox");
  const installedHelper = path.join(prefix, "bin", "crabbox-apple-vm-helper");
  const corruptInstall = installedByteMismatch
    ? `printf '# changed\\n' >>${shellQuote(installedCli)}\n`
    : "";
  writeExecutable(
    path.join(mockBin, "brew"),
    `#!/bin/sh
${credentialCanary}printf 'brew:%s\\n' "$*" >>${shellQuote(log)}
case "\${1:-}" in
  tap)
    [ "$*" = "tap openclaw/tap" ] || exit 80
    ;;
  update)
    [ "\${2:-}" = --force ] || exit 81
    ;;
  info)
    [ "$*" = "info --json=v2 --formula openclaw/tap/crabbox" ] || exit 82
    /bin/cat ${shellQuote(formulaPath)}
    ;;
  fetch)
    [ "\${2:-}" = --force ] || exit 83
    [ "\${3:-}" = --formula ] || exit 84
    [ "\${4:-}" = openclaw/tap/crabbox ] || exit 85
    [ "$HOME" = "\${HOMEBREW_CACHE%/cache}/home" ] || exit 86
    [ -d "$HOME" ] && [ -d "$HOMEBREW_CACHE" ] || exit 87
    [ -z "$(/usr/bin/find "$HOMEBREW_CACHE" -mindepth 1 -print -quit)" ] || exit 88
    ${fetchFailure ? "exit 74" : ""}
    /usr/bin/touch "$HOMEBREW_CACHE/fetched"
    ;;
  list)
    [ "$*" = "list --formula openclaw/tap/crabbox" ] || exit 82
    exit ${alreadyInstalled ? 0 : 1}
    ;;
  install|reinstall)
    [ "\${2:-}" = openclaw/tap/crabbox ] || exit 82
    [ -f "$HOMEBREW_CACHE/fetched" ] || exit 89
    /bin/mkdir -p ${shellQuote(path.join(prefix, "bin"))}
    /bin/cp ${shellQuote(cli)} ${shellQuote(installedCli)}
    ${(nativeArch === "arm64" && !helperMissing) || helperOnIntel ? `/bin/cp ${shellQuote(helper)} ${shellQuote(installedHelper)}` : ""}
    ${helperByteMismatch ? `printf '# changed\\n' >>${shellQuote(installedHelper)}` : ""}
    ${corruptInstall};;
  --prefix)
    [ "$*" = "--prefix openclaw/tap/crabbox" ] || exit 82
    printf '%s\\n' ${shellQuote(prefix)}
    ;;
  test)
    [ "$*" = "test openclaw/tap/crabbox" ] || exit 82
    ;;
  *)
    exit 90
    ;;
esac
`,
  );

  const command = `
source "$1"
ROOT=$2
MOCK_LOG=$3
require_publishable_source() {
  printf 'verify-source:%s\\n' "$*" >>"$MOCK_LOG"
}
homebrew_phase v1.2.3 "$4" "${"a".repeat(40)}" "${"b".repeat(40)}" "${"c".repeat(
    40,
  )}" ${nativeArch} "$5" "$6" "$7"
`;
  const pathValue = `${mockBin}:${path.dirname(process.execPath)}:/usr/bin:/bin:/usr/sbin:/sbin`;
  let launcher;
  if (useLauncher) {
    launcher = path.join(root, "launcher.sh");
    writeExecutable(launcher, `#!/bin/bash
source ${shellQuote(verifier)}
ROOT=${shellQuote(fakeRoot)}
SCRIPT_PATH=${shellQuote(launcher)}
require_publishable_source() { printf 'verify-source:%s\\n' "$*" >>${shellQuote(log)}; }
require_protected_homebrew_tooling() { :; }
freeze_public_release() { mkdir -m 700 "$7/public-assets"; cp "$2/"* "$7/public-assets/"; }
if [[ "\${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
`);
  }
  let result;
  try {
    result = spawnSync(
      "/usr/bin/env",
      [
        "-i",
        `HOME=${home}`,
        `HOMEBREW_CACHE=${cache}`,
        "USER=crabbox-test",
        "LOGNAME=crabbox-test",
        `PATH=${pathValue}`,
        `TMPDIR=${os.tmpdir()}`,
        "LC_ALL=C",
        "HOMEBREW_NO_ANALYTICS=1",
        "HOMEBREW_NO_AUTO_UPDATE=1",
        "HOMEBREW_NO_ENV_HINTS=1",
        "HOMEBREW_NO_INSTALL_CLEANUP=1",
        "NONINTERACTIVE=1",
        "CRABBOX_HOMEBREW_CLEAN_CHILD=1",
        "/bin/bash",
        "-c",
        command,
        "mock-homebrew-phase",
        verifier,
        fakeRoot,
        log,
        assets,
        path.join(mockBin, "brew"),
        process.execPath,
        work,
      ],
      { cwd: repoRoot, encoding: "utf8" },
    );
    if (useLauncher) {
      fs.writeFileSync(log, "");
      fs.rmSync(prefix, { recursive: true, force: true });
      fs.mkdirSync(prefix);
      result = spawnSync("/bin/bash", [launcher, "v1.2.3", assets, "a".repeat(40), "b".repeat(40), "c".repeat(40), "123"], {
        cwd: repoRoot, encoding: "utf8", env: {
          PATH: `${mockBin}:${process.env.PATH}`, TMPDIR: root,
          HOME: root, UNRELATED_SECRET: "synthetic-secret-canary",
        },
      });
    }
    return {
      ...result,
      calls: fs.existsSync(log) ? fs.readFileSync(log, "utf8") : "",
    };
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
}

test("Homebrew verifier checks immutable bytes before credential-free native installation", () => {
  const main = source.slice(source.indexOf("main() {"));
  const phase = source.slice(source.indexOf("homebrew_phase() {"), source.indexOf("main() {"));

  assert.match(source, /^FORMULA=openclaw\/tap\/crabbox$/m);
  assert.match(source, /REQUIRE_PUBLISHABLE=1/);
  assert.match(source, /validate-release-publication\.mjs" public-release/);
  assert.doesNotMatch(source, /public-verifier-run-id|proof ZIP|public-proof|witness|postflight|actions\/runs/);
  assert.match(phase, /CRABBOX_VERIFY_MODE=static/);
  assert.match(source, /git -C "\$ROOT" remote get-url origin/);
  assert.match(source, /git ls-remote "\$expected_remote"/);
  assert.match(source, /fetch --quiet --no-tags/);
  assert.match(source, /merge-base --is-ancestor "\$tooling_commit" "\$remote_commit"/);
  assert.match(source, /protected tooling commit is not in canonical default-branch history/);
  assert.match(
    source,
    /CRABBOX_VERIFY_EXEC_ARCH="\$native_arch"[\s\S]*scripts\/verify-release\.sh/,
  );
  assert.match(source, /\/usr\/bin\/env -i/);
  assert.match(source, /homebrew_home="\$work\/home"/);
  assert.match(source, /homebrew_cache="\$work\/cache"/);
  assert.match(source, /HOME="\$homebrew_home"/);
  assert.match(source, /HOMEBREW_CACHE="\$homebrew_cache"/);
  assert.match(source, /go_bin=\$\(command -v go\)/);
  assert.match(
    source,
    /clean_path="\$\{brew_bin%\/\*\}:\$\{node_bin%\/\*\}:\$\{go_bin%\/\*\}:\/usr\/bin:\/bin:\/usr\/sbin:\/sbin"/,
  );
  assert.ok(main.indexOf("go_bin=$(command -v go)") < main.indexOf("/usr/bin/env -i"));
  assert.doesNotMatch(source, /^\s*HOME="\$HOME"/m);
  for (const credential of forbiddenCredentials) {
    assert.match(credentialGuards, new RegExp(`\\b${credential}\\b`));
  }
  assert.doesNotMatch(source, /\bgh(?:\s|$)|workflow run|update-formula|git push/);
  assert.doesNotMatch(source, /== --homebrew-phase/);

  assert.ok(main.indexOf("require_publishable_source") < main.indexOf("/usr/bin/env -i"));
  assert.ok(phase.indexOf("require_publishable_source") < phase.indexOf('"$brew_bin" update'));
  assert.ok(phase.indexOf("scripts/verify-release.sh") < phase.indexOf('"$brew_bin" update'));
  assert.ok(phase.indexOf('"$brew_bin" update --force') < phase.indexOf('"$brew_bin" info'));
  assert.ok(phase.indexOf("verify_homebrew_formula") < phase.indexOf('"$brew_bin" reinstall'));
  assert.ok(phase.indexOf("verify_homebrew_formula") < phase.indexOf('"$brew_bin" fetch'));
  assert.ok(phase.indexOf('"$brew_bin" fetch --force --formula') < phase.indexOf('"$brew_bin" install'));
  assert.ok(phase.indexOf("verify-macos-binary.sh") < phase.indexOf('"$brew_bin" test'));
  assert.ok(phase.indexOf('"$brew_bin" test') < phase.indexOf('"$installed_cli" --version'));
});

test("Homebrew verifier cleanup survives main function scope", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-homebrew-cleanup-"));
  const result = spawnSync(
    "/bin/bash",
    [
      "-c",
      'source "$1"; CRABBOX_HOMEBREW_VERIFY_WORK=$2; cleanup_homebrew_work',
      "homebrew-cleanup-test",
      verifier,
      root,
    ],
    { encoding: "utf8" },
  );
  assert.equal(result.status, 0, result.stderr);
  assert.equal(fs.existsSync(root), false);
});

test("run-free public validation and freeze bind the complete immutable asset inventory", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-public-proof-"));
  const assetDirectory = path.join(root, "assets");
  fs.mkdirSync(assetDirectory);
  const names = execFileSync(path.join(repoRoot, "scripts", "release-config.sh"), [
    "assets",
    "1.2.3",
  ], { encoding: "utf8" }).trim().split("\n").sort();
  const files = Object.fromEntries(
    names.map((name, index) => {
      const bytes = Buffer.from(`public asset ${index} ${name}\n`);
      fs.writeFileSync(path.join(assetDirectory, name), bytes);
      return [name, bytes];
    }),
  );
  const notes = "## 1.2.3\n\nexact notes\n";
  const verifierCommit = "c".repeat(40);
  fs.writeFileSync(path.join(root, "names.txt"), `${names.join("\n")}\n`);
  fs.writeFileSync(path.join(root, "notes.md"), notes);
  const assets = names.map((name, index) => ({
    id: 1000 + index,
    name,
    size: files[name].length,
    state: "uploaded",
    digest: `sha256:${createHash("sha256").update(files[name]).digest("hex")}`,
    updated_at: "2026-07-10T10:00:00Z",
    url: `https://api.github.com/repos/openclaw/crabbox/releases/assets/${1000 + index}`,
  }));
  const release = {
    id: 123,
    tag_name: "v1.2.3",
    target_commitish: "main",
    name: "v1.2.3",
    body: notes,
    draft: false,
    immutable: true,
    prerelease: false,
    created_at: "2026-07-10T09:00:00Z",
    updated_at: "2026-07-10T10:05:00Z",
    published_at: "2026-07-10T10:05:00Z",
    assets,
  };
  const writeJson = (name, value) =>
    fs.writeFileSync(path.join(root, name), `${JSON.stringify(value)}\n`);
  writeJson("release.json", release);
  const args = [
    path.join(repoRoot, "scripts", "validate-release-publication.mjs"),
    "public-release",
    ...["release.json", "names.txt", "notes.md"].map((name) => path.join(root, name)),
    assetDirectory,
  ];
  const env = {
    CRABBOX_PUBLISH_REPOSITORY: "openclaw/crabbox",
    CRABBOX_PUBLISH_RELEASE_ID: "123",
    CRABBOX_PUBLISH_TAG: "v1.2.3",
    CRABBOX_PUBLISH_TAG_OBJECT: "a".repeat(40),
    CRABBOX_PUBLISH_SOURCE_COMMIT: "b".repeat(40),
    CRABBOX_PUBLISH_VERIFIER_COMMIT: verifierCommit,
  };
  try {
    const valid = spawnSync(process.execPath, args, { encoding: "utf8", env });
    assert.equal(valid.status, 0, valid.stderr);
    assert.equal(JSON.parse(valid.stdout).releaseId, 123);
    assert.equal(JSON.parse(valid.stdout).assets.length, 8);
    const bin = path.join(root, "bin");
    fs.mkdirSync(bin);
    writeExecutable(path.join(bin, "git"), `#!/bin/sh
[ "$*" = "-C ${repoRoot} show ${"b".repeat(40)}:CHANGELOG.md" ] || exit 98
printf '## 1.2.3\\n\\nexact notes\\n'
`);
    writeExecutable(path.join(bin, "curl"), `#!/bin/bash
set -eu
[[ "$*" == '--disable --fail --silent --show-error --location --retry 3 --header Accept: application/vnd.github+json --header X-GitHub-Api-Version: 2026-03-10 https://api.github.com/repos/openclaw/crabbox/releases/123' ]] || exit 98
/bin/cat ${shellQuote(path.join(root, "release.json"))}
`);
    const frozen = path.join(root, "frozen");
    const runFreeze = () => {
      fs.rmSync(frozen, { recursive: true, force: true });
      fs.mkdirSync(frozen);
      return spawnSync("/bin/bash", [
        "-c", 'source "$1"; shift; freeze_public_release "$@"', "freeze-test", verifier,
        "v1.2.3", assetDirectory, "a".repeat(40), "b".repeat(40), verifierCommit,
        "123", frozen, process.execPath,
      ], { encoding: "utf8", env: { PATH: `${bin}:${process.env.PATH}`, HOME: root, TMPDIR: root } });
    };
    const frozenResult = runFreeze();
    assert.equal(frozenResult.status, 0, frozenResult.stderr);
    assert.deepEqual(fs.readdirSync(path.join(frozen, "public-assets")).sort(), names);
    fs.writeFileSync(path.join(assetDirectory, "extra"), "unexpected");
    const extra = runFreeze();
    assert.notEqual(extra.status, 0);
    assert.match(extra.stderr, /public asset inventory is not exact/);
    fs.unlinkSync(path.join(assetDirectory, "extra"));
    const check = (record, pattern) => {
      writeJson("release.json", record);
      const result = spawnSync(process.execPath, args, { encoding: "utf8", env });
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, pattern);
    };
    for (const patch of [
      { draft: true }, { immutable: false }, { id: 124 }, { tag_name: "v1.2.4" },
      { body: "different notes" }, { prerelease: true }, { published_at: null },
    ]) check({ ...release, ...patch }, /immutable|drifted|timestamp/);
    for (const patch of [
      { name: "../escape" }, { id: assets[1].id }, { size: 9999 },
      { digest: "sha256:" + "0".repeat(64) }, { state: "new" },
      { url: assets[0].url + "?token=x" },
      { url: assets[0].url.replace("github.com", "github.com@evil.test") },
      { url: assets[0].url.replace("openclaw/crabbox", "other/crabbox") },
    ]) check({ ...release, assets: [{ ...assets[0], ...patch }, ...assets.slice(1)] },
      /asset|digest/);
    writeJson("release.json", release);
    fs.writeFileSync(path.join(assetDirectory, names[0]), "changed");
    const changed = spawnSync(process.execPath, args, { encoding: "utf8", env });
    assert.notEqual(changed.status, 0);
    assert.match(changed.stderr, /local public asset identity/);
    fs.writeFileSync(path.join(assetDirectory, names[0]), files[names[0]]);
    fs.writeFileSync(path.join(assetDirectory, "extra"), "unexpected");
    assert.notEqual(spawnSync(process.execPath, args, { encoding: "utf8", env }).status, 0);
    fs.unlinkSync(path.join(assetDirectory, "extra"));
    fs.unlinkSync(path.join(assetDirectory, names[0]));
    fs.symlinkSync(path.join(assetDirectory, names[1]), path.join(assetDirectory, names[0]));
    const symlink = spawnSync(process.execPath, args, { encoding: "utf8", env });
    assert.notEqual(symlink.status, 0);
    assert.match(symlink.stderr, /local public asset identity/);
    for (const patch of [
      { CRABBOX_PUBLISH_TAG: "v1.2.3/../../escape" },
      { CRABBOX_PUBLISH_REPOSITORY: "other/crabbox" },
    ]) {
      const result = spawnSync(process.execPath, args, { encoding: "utf8", env: { ...env, ...patch } });
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /canonical repository and stable tag/);
    }
    fs.writeFileSync(path.join(root, "names.txt"), ["../escape", ...names.slice(1)].sort().join("\n") + "\n");
    const unsafeNames = spawnSync(process.execPath, args, { encoding: "utf8", env });
    assert.notEqual(unsafeNames.status, 0);
    assert.match(unsafeNames.stderr, /inventory is not canonical/);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("TAG-only ordinary tap handoff validates public metadata and supports independent retries", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-tap-handoff-"));
  try {
    const doc = fs.readFileSync(path.join(repoRoot, "docs/RELEASING.md"), "utf8");
    const handoff = doc.split("<!-- ordinary-tap-handoff -->\n\n```sh\n")[1]?.split("\n```")[0];
    assert.ok(handoff);
    assert.doesNotMatch(handoff, /scripts\/|\bgit\b|\bgo\b|\bnode\b|mktemp|FINAL_ASSETS|TAG_OBJECT|TAG_COMMIT|VERIFIER_COMMIT|RELEASE_ID|PUBLIC_VERIFIER/);
    const bin = path.join(root, "bin");
    const operator = path.join(root, "operator");
    fs.mkdirSync(bin);
    fs.mkdirSync(operator);
    const metadata = path.join(root, "release.json");
    const dispatchLog = path.join(root, "dispatches.jsonl");
    const curlLog = path.join(root, "curl.log");
    const unexpectedLog = path.join(root, "unexpected-command");
    const failOnce = path.join(root, "fail-once");
    const fetchFailure = path.join(root, "fetch-failure");
    const current = path.join(root, "tap-current");
    const assets = ["darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"].map((target, index) => ({
      name: `crabbox_1.2.3_${target}.tar.gz`, state: "uploaded",
      digest: "sha256:" + "abcd"[index].repeat(64),
    }));
    const release = { tag_name: "v1.2.3", draft: false, prerelease: false, immutable: true, assets };
    const expectedAssets = Object.fromEntries(assets.map((asset) => [
      asset.name.slice("crabbox_1.2.3_".length, -".tar.gz".length),
      { name: asset.name, sha256: asset.digest.slice(7) },
    ]));
    for (const tool of ["git", "go", "node", "brew", "mktemp"]) {
      writeExecutable(path.join(bin, tool), `#!/bin/sh
printf '%s\\n' ${shellQuote(tool)} >>${shellQuote(unexpectedLog)}
echo unexpected-command >&2
exit 98
`);
    }
    writeExecutable(path.join(bin, "curl"), `#!/bin/bash
set -eu
printf 'fetch\\n' >>${shellQuote(curlLog)}
[[ "$*" == '--disable --fail --silent --show-error --location --retry 3 https://api.github.com/repos/openclaw/crabbox/releases/tags/v1.2.3' ]] || exit 98
[[ ! -f ${shellQuote(fetchFailure)} ]] || exit 22
/bin/cat ${shellQuote(metadata)}
`);
    writeExecutable(path.join(bin, "gh"), `#!${process.execPath}
const assert = require("node:assert/strict");
const fs = require("node:fs");
const args = process.argv.slice(2);
fs.appendFileSync(${JSON.stringify(dispatchLog)}, JSON.stringify(args) + "\\n");
for (const name of ${JSON.stringify(forbiddenCredentials)}) assert.ok(!(name in process.env), name);
assert.deepEqual(args.slice(0, -2), ["workflow", "run", "update-formula.yml", "--repo", "openclaw/homebrew-tap", "--ref", "main", "-f", "formula=crabbox", "-f", "tag=v1.2.3", "-f", "repository=openclaw/crabbox"]);
assert.equal(args.at(-2), "-f");
assert.ok(args.at(-1).startsWith("assets="));
assert.deepEqual(JSON.parse(args.at(-1).slice(7)), ${JSON.stringify(expectedAssets)});
if (fs.existsSync(${JSON.stringify(failOnce)})) {
  fs.unlinkSync(${JSON.stringify(failOnce)});
  process.exit(1);
}
if (fs.existsSync(${JSON.stringify(current)})) console.log("already current");
else fs.writeFileSync(${JSON.stringify(current)}, "updated");
`);
    const runHandoff = (value = release, tag = "v1.2.3") => {
      fs.writeFileSync(metadata, typeof value === "string" ? value : JSON.stringify(value));
      return spawnSync("/bin/bash", ["-c", handoff], {
        cwd: operator, encoding: "utf8",
        env: { TAG: tag, PATH: `${bin}:${process.env.PATH}`, TMPDIR: root },
      });
    };
    fs.writeFileSync(failOnce, "fail first dispatch");
    const failed = runHandoff();
    assert.equal(failed.status, 1, failed.stderr);
    assert.equal(fs.existsSync(failOnce), false, "initial attempt reached the dispatcher");
    const retry = runHandoff();
    assert.equal(retry.status, 0, retry.stderr);
    const repeated = runHandoff();
    assert.equal(repeated.status, 0, repeated.stderr);
    assert.match(repeated.stdout, /already current/);
    const dispatched = fs.readFileSync(dispatchLog, "utf8");
    assert.equal(dispatched.trim().split("\n").length, 3);

    const invalid = [
      "not JSON", {}, { ...release, tag_name: "v1.2.4" },
      { ...release, draft: true }, { ...release, prerelease: true },
      { ...release, immutable: false }, { ...release, immutable: null },
      { ...release, assets: null }, { ...release, assets: {} },
    ];
    for (const asset of assets) {
      invalid.push({ ...release, assets: assets.filter((value) => value !== asset) });
      invalid.push({ ...release, assets: [...assets, asset] });
      for (const patch of [
        { state: "new" }, { state: null },
        { digest: null }, { digest: 123 }, { digest: "sha256:" + "a".repeat(63) },
        { digest: "sha256:" + "A".repeat(64) }, { digest: asset.digest + "\n" },
        { digest: asset.digest.replace("sha256:", "sha512:") },
      ]) invalid.push({ ...release, assets: assets.map((value) => value === asset ? { ...asset, ...patch } : value) });
    }
    for (const value of invalid) {
      const result = runHandoff(value);
      assert.notEqual(result.status, 0, JSON.stringify(value));
      assert.equal(fs.readFileSync(dispatchLog, "utf8"), dispatched, "invalid metadata dispatched gh");
    }
    fs.writeFileSync(fetchFailure, "fail fetch");
    assert.notEqual(runHandoff().status, 0);
    assert.equal(fs.readFileSync(dispatchLog, "utf8"), dispatched);
    fs.unlinkSync(fetchFailure);
    const fetched = fs.readFileSync(curlLog, "utf8");
    for (const tag of ["", "1.2.3", "v1.2.3/../../escape", "v1.2.3?other", "v1.2.3\n"]) {
      assert.notEqual(runHandoff(release, tag).status, 0, tag);
      assert.equal(fs.readFileSync(curlLog, "utf8"), fetched, "unsafe tag reached curl");
      assert.equal(fs.readFileSync(dispatchLog, "utf8"), dispatched);
    }
    assert.equal(fs.existsSync(unexpectedLog), false);
    assert.deepEqual(fs.readdirSync(operator), [], "handoff wrote local files");
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("mocked Homebrew phase proves fresh fetch, frozen bytes, trust, and final execution", () => {
  const result = runMockedHomebrewPhase();
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /Verified Homebrew v1\.2\.3 on arm64/);
  assert.match(result.calls, /^verify-source:v1\.2\.3 /m);
  assert.match(result.calls, /^verify-release:v1\.2\.3 /m);
  assert.match(result.calls, /^brew:update --force$/m);
  assert.match(result.calls, /^brew:fetch --force --formula openclaw\/tap\/crabbox$/m);
  assert.match(result.calls, /^brew:install openclaw\/tap\/crabbox$/m);
  assert.match(result.calls, /^verify-macos:org\.openclaw\.crabbox arm64 /m);
  assert.match(
    result.calls,
    /^verify-macos:org\.openclaw\.crabbox\.apple-vm-helper arm64 /m,
  );
  assert.match(result.calls, /^brew:test openclaw\/tap\/crabbox$/m);
  assert.ok(result.calls.indexOf("brew:fetch") < result.calls.indexOf("brew:install"));
  assert.ok(result.calls.indexOf("verify-macos:") < result.calls.indexOf("brew:test"));
});

test("mocked Homebrew phase rejects installed bytes that differ from the release", () => {
  const result = runMockedHomebrewPhase({ installedByteMismatch: true });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /CLI differs from the frozen release archive/);
  assert.doesNotMatch(result.calls, /^verify-macos:/m);
  assert.doesNotMatch(result.calls, /^brew:test /m);
});

test("mocked Homebrew phase stops when the forced public fetch fails", () => {
  const result = runMockedHomebrewPhase({ fetchFailure: true });
  assert.notEqual(result.status, 0);
  assert.match(result.calls, /^brew:fetch --force --formula openclaw\/tap\/crabbox$/m);
  assert.doesNotMatch(result.calls, /^brew:install /m);
  assert.doesNotMatch(result.calls, /^verify-macos:/m);
});

test("mocked Homebrew phase propagates native signature verifier failure", () => {
  const result = runMockedHomebrewPhase({ macosVerifierFailure: true });
  assert.notEqual(result.status, 0);
  assert.match(result.calls, /^verify-macos:org\.openclaw\.crabbox arm64 /m);
  assert.doesNotMatch(
    result.calls,
    /^verify-macos:org\.openclaw\.crabbox\.apple-vm-helper arm64 /m,
  );
  assert.doesNotMatch(result.calls, /^brew:test /m);
});

test("mocked Homebrew phase rejects helper embedded-VMD trust mismatch", () => {
  const result = runMockedHomebrewPhase({ releaseTrust: false });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /wrong embedded VMD trust policy/);
  assert.match(result.calls, /^verify-macos:org\.openclaw\.crabbox arm64 /m);
  assert.match(
    result.calls,
    /^verify-macos:org\.openclaw\.crabbox\.apple-vm-helper arm64 /m,
  );
  assert.match(result.calls, /^brew:test openclaw\/tap\/crabbox$/m);
});

test("mocked Homebrew phase rejects the installed CLI version last", () => {
  const result = runMockedHomebrewPhase({ versionOutput: "1.2.4" });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /version mismatch: 1\.2\.4/);
  assert.match(result.calls, /^brew:test openclaw\/tap\/crabbox$/m);
});

test("six-argument launcher strips arbitrary credentials before all Homebrew and candidate commands", () => {
  const result = runMockedHomebrewPhase({ useLauncher: true });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.calls, /brew:info --json=v2 --formula/);
  assert.match(result.calls, /brew:test/);
  assert.match(result.stdout, /Verified Homebrew/);
  assert.doesNotMatch(result.stdout + result.stderr, /synthetic-secret-canary/);
});

test("installed native signature and online notarization gates run before brew test", () => {
  const valid = runMockedHomebrewPhase({ useNativeVerifier: true });
  assert.equal(valid.status, 0, valid.stderr);
  assert.match(valid.calls, /codesign:--verify --strict --check-notarization -R=notarized/);
  for (const option of ["signatureFailure", "notarizationFailure"]) {
    const failed = runMockedHomebrewPhase({ useNativeVerifier: true, [option]: true });
    assert.notEqual(failed.status, 0);
    assert.match(failed.calls, /codesign:--verify --strict/);
    assert.doesNotMatch(failed.calls, /brew:test/);
  }
});

test("Intel installation omits the helper and an existing install uses reinstall", () => {
  const result = runMockedHomebrewPhase({ nativeArch: "x86_64", alreadyInstalled: true });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /Verified Homebrew v1.2.3 on x86_64/);
  assert.match(result.calls, /brew:reinstall openclaw\/tap\/crabbox/);
  assert.match(result.calls, /verify-macos:org.openclaw.crabbox x86_64/);
  assert.doesNotMatch(result.calls, /verify-macos:.*apple-vm-helper/);
});

for (const [options, message] of [
  [{ installedArch: "x86_64" }, /CLI architecture is not arm64/],
  [{ helperMissing: true }, /missing the Apple VM helper/],
  [{ helperByteMismatch: true }, /helper differs from the frozen release archive/],
  [{ nativeArch: "x86_64", helperOnIntel: true }, /arm-only Apple VM helper on Intel/],
]) {
  test(`native install rejects ${JSON.stringify(options)}`, () => {
    const result = runMockedHomebrewPhase(options);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, message);
    assert.doesNotMatch(result.calls, /^brew:test /m);
  });
}

test("formula metadata accepts maintained content and rejects wrong native identity or route", () => {
  const maintained = formulaMetadata();
  maintained.formulae[0].desc = "Maintained description";
  maintained.formulae[0].revision = 2;
  maintained.formulae[0].ruby_source_checksum = { sha256: "f".repeat(64) };
  maintained.formulae[0].caveats = "Maintained caveat";
  const accepted = verifyFormula(maintained);
  assert.equal(accepted.status, 0, accepted.stderr);

  for (const mutate of [
    (f) => { f.name = "other"; },
    (f) => { f.full_name = "other/tap/crabbox"; },
    (f) => { f.tap = "other/tap"; },
    (f) => { f.versions.stable = "1.2.4"; },
    (f) => { f.urls.stable.checksum = "e".repeat(64); },
    (f) => { f.urls.stable.checksum = hashes.darwinArm64.toUpperCase(); },
    (f) => { f.urls.stable.url = f.urls.stable.url.replace("arm64", "amd64"); },
    (f) => { f.urls.stable.url = f.urls.stable.url.replace("openclaw/crabbox", "other/crabbox"); },
    (f) => { f.urls.stable.url += "?download=1"; },
    (f) => { f.urls.stable.url = f.urls.stable.url.replace("github.com", "github.com@evil.test"); },
  ]) {
    const value = formulaMetadata();
    mutate(value.formulae[0]);
    const rejected = verifyFormula(value);
    assert.notEqual(rejected.status, 0);
    assert.match(rejected.stderr, /metadata does not match/);
  }
  for (const value of [{ formulae: [] }, { formulae: [maintained.formulae[0], maintained.formulae[0]] }]) {
    assert.notEqual(verifyFormula(value).status, 0);
  }
});

test("blocked v0.37.0 record stops before the mocked brew executable", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-homebrew-blocked-"));
  const assets = path.join(root, "assets");
  const mockBin = path.join(root, "bin");
  const marker = path.join(root, "brew-ran");
  fs.mkdirSync(assets);
  fs.mkdirSync(mockBin);
  const mockBrew = path.join(mockBin, "brew");
  fs.writeFileSync(mockBrew, `#!/bin/sh\nprintf ran >"${marker}"\nexit 99\n`);
  fs.chmodSync(mockBrew, 0o755);
  writeExecutable(
    path.join(mockBin, "uname"),
    `#!/bin/sh\ncase "\${1:-}" in\n  -s) printf 'Darwin\\n' ;;\n  -m) printf 'arm64\\n' ;;\n  *) exit 64 ;;\nesac\n`,
  );
  try {
    const { tagObject, sourceCommit } = JSON.parse(fs.readFileSync(
      path.join(repoRoot, "release/records/v0.37.0.json"), "utf8",
    ));
    const verifierCommit = "c".repeat(40);
    writeExecutable(path.join(mockBin, "git"), `#!/bin/sh
case "$*" in
  "-C ${repoRoot} status --porcelain=v1 --untracked-files=all -- "* | \
  "-C ${repoRoot} diff --quiet ${verifierCommit} -- "*) exit 0 ;;
  "-C ${repoRoot} remote get-url origin") printf 'https://github.com/openclaw/crabbox\\n' ;;
  "ls-remote https://github.com/openclaw/crabbox refs/heads/main") printf '${verifierCommit} refs/heads/main\\n' ;;
  "-C ${repoRoot} -c fetch.writeCommitGraph=false fetch --quiet --no-tags https://github.com/openclaw/crabbox ${verifierCommit}") exit 0 ;;
  "-C ${repoRoot} merge-base --is-ancestor ${verifierCommit} ${verifierCommit}" | \
  "-C ${repoRoot} merge-base --is-ancestor ${sourceCommit} ${verifierCommit}") exit 0 ;;
  "-C ${repoRoot} rev-parse HEAD") printf '${verifierCommit}\\n' ;;
  *) echo unexpected-git >&2; exit 98 ;;
esac
`);
    const result = spawnSync(
      verifier,
      ["v0.37.0", assets, tagObject, sourceCommit, verifierCommit, "1"],
      {
        cwd: repoRoot,
        encoding: "utf8",
        env: {
          HOME: process.env.HOME,
          PATH: `${mockBin}:${process.env.PATH}`,
          TMPDIR: os.tmpdir(),
        },
      },
    );
    assert.notEqual(result.status, 0);
    assert.match(
      result.stderr,
      /release v0\.37\.0 is blocked/,
    );
    assert.equal(fs.existsSync(marker), false, "brew ran despite the protected blocked record");
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("credential presence fails before source or Homebrew execution", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-homebrew-token-"));
  const assets = path.join(root, "assets");
  const mockBin = path.join(root, "bin");
  const marker = path.join(root, "brew-ran");
  fs.mkdirSync(assets);
  fs.mkdirSync(mockBin);
  const mockBrew = path.join(mockBin, "brew");
  fs.writeFileSync(mockBrew, `#!/bin/sh\nprintf ran >"${marker}"\nexit 99\n`);
  fs.chmodSync(mockBrew, 0o755);
  try {
    for (const credential of forbiddenCredentials) {
      fs.rmSync(marker, { force: true });
      const result = spawnSync(
        verifier,
        [
          "v1.2.3",
          assets,
          "a".repeat(40),
          "b".repeat(40),
          "c".repeat(40),
          "1",
        ],
        {
          cwd: repoRoot,
          encoding: "utf8",
          env: {
            HOME: process.env.HOME,
            PATH: `${mockBin}:${process.env.PATH}`,
            [credential]: "synthetic-credential-canary",
          },
        },
      );
      assert.notEqual(result.status, 0, `${credential} was accepted`);
      assert.match(result.stderr, new RegExp(`${credential} must be unset`));
      assert.equal(fs.existsSync(marker), false, `brew ran with ${credential} present`);
      assert.doesNotMatch(result.stdout + result.stderr, /synthetic-credential-canary/);
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("internal Homebrew phase refuses an unsanitized direct call", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-homebrew-direct-"));
  const marker = path.join(root, "brew-ran");
  const mockBrew = path.join(root, "brew");
  fs.writeFileSync(mockBrew, `#!/bin/sh\nprintf ran >"${marker}"\nexit 99\n`);
  fs.chmodSync(mockBrew, 0o755);
  try {
    const result = spawnSync(
      "/bin/bash",
      [
        "-c",
        'source "$1"; homebrew_phase v1.2.3 "$2" "$3" "$4" "$5" "$(uname -m)" "$6" "$7" "$2"',
        "homebrew-direct-test",
        verifier,
        root,
        "a".repeat(40),
        "b".repeat(40),
        "c".repeat(40),
        mockBrew,
        process.execPath,
      ],
      { cwd: repoRoot, encoding: "utf8" },
    );
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /must run through the credential-free launcher/);
    assert.equal(fs.existsSync(marker), false);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
