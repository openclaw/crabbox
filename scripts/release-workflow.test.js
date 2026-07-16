import assert from "node:assert/strict";
import crypto from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const read = (file) => fs.readFileSync(path.join(repoRoot, file), "utf8");

test("release workflow is verifier-only, protected-default, dual-native, and token-bounded", () => {
  const workflow = read(".github/workflows/release-assets.yml");
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.doesNotMatch(workflow, /repository_dispatch:|^  push:|^  release:/m);
  assert.match(workflow, /name: guard-protected-release-policy/);
  assert.match(workflow, /expected_workflow_ref="\$GITHUB_REPOSITORY\/\.github\/workflows\/release-assets\.yml@\$expected_ref"/);
  assert.match(workflow, /\[\[ "\$GITHUB_WORKFLOW_REF" == "\$expected_workflow_ref" \]\]/);
  assert.match(workflow, /verify-github-release-policy\.mjs/);
  assert.match(workflow, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(workflow, /persist-credentials: false/);
  assert.match(workflow, /runner: macos-15\n\s+arch: arm64/);
  assert.match(workflow, /runner: macos-15-intel\n\s+arch: x86_64/);
  assert.match(workflow, /cancel-in-progress: false/);
  assert.equal((workflow.match(/GH_TOKEN:/g) ?? []).length, 2);
  assert.equal((workflow.match(/secrets\.CRABBOX_RULESET_READ_TOKEN/g) ?? []).length, 1);
  assert.equal((workflow.match(/contents: write/g) ?? []).length, 1);
  assert.match(workflow, /GH_TOKEN: \$\{\{ secrets\.CRABBOX_RULESET_READ_TOKEN \}\}/);
  assert.match(workflow, /GH_TOKEN: \$\{\{ github\.token \}\}/);
  assert.match(workflow, /gh api --method GET[\s\S]*releases\/\$RELEASE_ID/);
  assert.match(workflow, /releases\/assets\/\$asset_id/);
  assert.match(workflow, /name: Statically verify with no release credentials[\s\S]*env -i/);
  assert.match(
    workflow,
    /name: Freeze exact static proof before candidate execution[\s\S]*env -i[\s\S]*PATH="\$PATH" VERIFY_ARCH="\$VERIFY_ARCH" node/,
  );
  assert.match(workflow, /name: Execute candidate in isolated clean job without release credentials[\s\S]*exec env -i/);
  assert.match(workflow, /CRABBOX_VERIFY_EXEC_ARCH="\$VERIFY_ARCH"/);
  assert.match(workflow, /scripts\/verify-release\.sh/);
  assert.match(workflow, /name: release-input/);
  assert.match(workflow, /name: verified-assets-\$\{\{ matrix\.arch \}\}/);
  assert.equal((workflow.match(/retention-days: 30/g) ?? []).length, 2);
  assert.doesNotMatch(workflow, /retention-days: (?:[0-9]|1[0-9]|2[0-9])\b/);
  assert.notEqual(
    fs.statSync(path.join(repoRoot, "scripts/materialize-release-input.sh")).mode & 0o111,
    0,
    "workflow materializer must be executable",
  );
  assert.doesNotMatch(workflow, /target_commitish\s*===\s*process\.env\.RELEASE_COMMIT/);
  assert.doesNotMatch(
    workflow,
    /gh release (?:create|upload|edit|delete)|--method (?:DELETE|PATCH|POST|PUT)|HOMEBREW_TAP_GITHUB_TOKEN/,
  );
  assert.match(
    read(".github/CODEOWNERS"),
    /^\/scripts\/render-homebrew-formula\.mjs @openclaw\/openclaw-secops$/m,
  );
});

test("Homebrew verifier keeps downloaded proof inputs outside the protected checkout", () => {
  const workflow = read(".github/workflows/verify-homebrew.yml");
  const proofDownloadStart = workflow.indexOf("      - name: Download immutable native proof ZIPs");
  const proofDownloadEnd = workflow.indexOf(
    "      - name: Verify public Homebrew install without credentials",
  );
  assert.notEqual(proofDownloadStart, -1);
  assert.notEqual(proofDownloadEnd, -1);
  const proofDownloadStep = workflow.slice(
    proofDownloadStart,
    proofDownloadEnd,
  );
  const verifyStart = workflow.indexOf(
    "      - name: Verify public Homebrew install without credentials",
  );
  const verifyStep = workflow.slice(verifyStart);
  assert.match(workflow, /WORKFLOW_SHA: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(workflow, /RUN_SHA: \$\{\{ github\.sha \}\}/);
  assert.match(workflow, /\[\[ "\$WORKFLOW_SHA" == "\$RUN_SHA" \]\]/);
  assert.match(
    workflow,
    /name: Set up Go for build-info inspection\n\s+uses: actions\/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16/,
  );
  assert.match(workflow, /go-version-file: go\.mod/);
  assert.match(workflow, /go-version-file: go\.mod\n\s+cache: false/);
  assert.match(workflow, /name: Preserve pinned Go in the frozen verifier path/);
  assert.match(workflow, /tools="\$RUNNER_TEMP\/release-tools"/);
  assert.match(workflow, /brew_path=\$\(command -v brew\)/);
  assert.match(workflow, /exec \\\"\$brew_path\\\" \\\"\\\$@\\\"/);
  assert.match(workflow, /chmod 700 "\$tools\/brew"/);
  assert.match(workflow, /ln -s "\$\(command -v go\)" "\$tools\/go"/);
  assert.match(workflow, /printf '%s\\n' "\$tools" >>"\$GITHUB_PATH"/);
  assert.match(workflow, /assets_dir="\$RUNNER_TEMP\/release-assets"/);
  assert.match(workflow, /proofs_dir="\$RUNNER_TEMP\/public-proofs"/);
  assert.match(
    proofDownloadStep,
    /gh api --method GET --header 'Accept: application\/vnd\.github\+json' \\\s+"repos\/\$GITHUB_REPOSITORY\/actions\/artifacts\/\$artifact_id\/zip"/,
  );
  assert.doesNotMatch(proofDownloadStep, /application\/octet-stream/);
  assert.match(workflow, /"\$RUNNER_TEMP\/release-assets"/);
  assert.match(workflow, /"\$RUNNER_TEMP\/public-proofs"/);
  assert.doesNotMatch(workflow, /"\$PWD\/(?:release-assets|public-proofs)"/);
  assert.doesNotMatch(workflow, /mkdir -m 700 (?:release-assets|public-proofs)/);
  assert.match(verifyStep, /unset ACTIONS_ID_TOKEN_REQUEST_TOKEN ACTIONS_RUNTIME_TOKEN GH_TOKEN GITHUB_TOKEN/);
  assert.match(verifyStep, /unset HOMEBREW_GITHUB_API_TOKEN HOMEBREW_TAP_GITHUB_TOKEN/);
  assert.match(verifyStep, /HOMEBREW_NO_AUTO_UPDATE=1 brew tap openclaw\/tap/);
  assert.ok(
    verifyStep.indexOf("unset HOMEBREW_GITHUB_API_TOKEN HOMEBREW_TAP_GITHUB_TOKEN") <
      verifyStep.indexOf("HOMEBREW_NO_AUTO_UPDATE=1 brew tap openclaw/tap"),
  );
  assert.ok(
    verifyStep.indexOf("HOMEBREW_NO_AUTO_UPDATE=1 brew tap openclaw/tap") <
      verifyStep.indexOf("scripts/verify-homebrew-release.sh"),
  );
});

test("script CI fetches signed release tags for publication fixtures", () => {
  const ci = read(".github/workflows/ci.yml");
  const scriptsJob = ci.slice(ci.indexOf("  scripts:"), ci.indexOf("  docs:"));
  assert.match(scriptsJob, /uses: actions\/checkout@[^\n]+\n\s+with:\n\s+fetch-depth: 0/);
});

test("GoReleaser is credential-free build-only with exact binary archives", () => {
  const config = read(".goreleaser.yaml");
  assert.match(config, /release:\n\s+disable: true/);
  assert.doesNotMatch(config, /^brews:|HOMEBREW|github_token|GITHUB_TOKEN/m);
  assert.equal((config.match(/- -trimpath/g) ?? []).length, 2);
  assert.match(config, /files:\n\s+- none\*/);
  assert.match(config, /allow_different_binary_count: true/);
  assert.match(config, /crabbox-apple-vm-helper[\s\S]*- -tags=vmdembed/);

  const build = read("scripts/build-release-candidate.sh");
  assert.match(build, /env -i[\s\S]*goreleaser release --clean --skip=publish/);
  assert.match(build, /git clone --quiet --no-local --no-checkout/);
  assert.match(build, /git -C "\$SOURCE" checkout --quiet --detach "\$TAG_COMMIT"/);
  assert.match(build, /run_goreleaser\(\) \{[\s\S]*env -i "\$@"/);
  assert.match(build, /run_goreleaser "DEVELOPER_DIR=\$DEVELOPER_DIR"/);
  assert.match(build, /else\s+run_goreleaser\s+fi/);
  assert.match(build, /chmod -R u\+w "\$path"[\s\S]*rm -rf "\$path"/);
  assert.doesNotMatch(build, /gh release|HOMEBREW_TAP_GITHUB_TOKEN=.*\$\{/);
});

test("release config emits the exact immutable eight-asset inventory", () => {
  const config = path.join(repoRoot, "scripts", "release-config.sh");
  const output = execFileSync(config, ["assets", "v0.37.0"], { encoding: "utf8" })
    .trim()
    .split("\n")
    .sort();
  assert.deepEqual(output, [
    "checksums.txt",
    "crabbox_0.37.0_darwin_amd64.tar.gz",
    "crabbox_0.37.0_darwin_arm64.tar.gz",
    "crabbox_0.37.0_linux_amd64.tar.gz",
    "crabbox_0.37.0_linux_arm64.tar.gz",
    "crabbox_0.37.0_windows_amd64.zip",
    "crabbox_0.37.0_windows_arm64.zip",
    "provenance.json",
  ]);
  assert.notEqual(spawnSync(config, ["assets", "0.37.0-rc.1"]).status, 0);
});

test("release source guard pins an allowed signed tag object while permitting later verifier hardening", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-release-source-"));
  const guard = path.join(repoRoot, "scripts", "verify-release-source.sh");
  const key = path.join(root, "release-key");
  const allowed = path.join(root, "allowed-signers");
  const record = path.join(root, "release-record.json");
  const git = (...args) => execFileSync("git", args, { cwd: root, stdio: "pipe" });
  const runGuard = (overrides = {}) =>
    spawnSync(guard, [], {
      cwd: root,
      env: {
        ...process.env,
        DEFAULT_BRANCH: "main",
        RELEASE_TAG: "v1.2.3",
        EXPECTED_TAG_OBJECT: tagObject,
        EXPECTED_TAG_COMMIT: tagCommit,
        TRUSTED_HEAD: "HEAD",
        ALLOWED_SIGNERS: allowed,
        RELEASE_RECORD: record,
        ...overrides,
      },
      encoding: "utf8",
    });

  let tagObject;
  let tagCommit;
  try {
    execFileSync("ssh-keygen", ["-q", "-t", "ed25519", "-N", "", "-f", key]);
    git("init", "-b", "main");
    git("config", "user.name", "Release Test");
    git("config", "user.email", "release@example.test");
    git("config", "gpg.format", "ssh");
    git("config", "user.signingkey", key);
    fs.writeFileSync(path.join(root, "source.txt"), "source\n");
    git("add", "source.txt");
    git("commit", "-m", "source");
    git("tag", "-s", "v1.2.3", "-m", "v1.2.3");
    tagObject = git("rev-parse", "refs/tags/v1.2.3").toString().trim();
    tagCommit = git("rev-parse", "refs/tags/v1.2.3^{}").toString().trim();
    const publicKey = fs.readFileSync(`${key}.pub`, "utf8").trim();
    fs.writeFileSync(allowed, `release@example.test ${publicKey}\n`);
    fs.writeFileSync(
      record,
      `${JSON.stringify({
        schemaVersion: 1,
        repository: "openclaw/crabbox",
        tag: "v1.2.3",
        tagObject,
        sourceCommit: tagCommit,
        publicationStatus: "ready",
      })}\n`,
    );

    fs.writeFileSync(path.join(root, "verifier.txt"), "protected verifier\n");
    git("add", "verifier.txt");
    git("commit", "-m", "verifier hardening");

    const valid = runGuard();
    assert.equal(valid.status, 0, valid.stderr || valid.stdout);
    assert.equal(valid.stdout.trim(), `${tagObject} ${tagCommit}`);

    const wrongObject = runGuard({ EXPECTED_TAG_OBJECT: "a".repeat(40) });
    assert.equal(wrongObject.status, 1);
    assert.match(`${wrongObject.stdout}${wrongObject.stderr}`, /protected release record/);

    const wrongCommit = runGuard({ EXPECTED_TAG_COMMIT: "b".repeat(40) });
    assert.equal(wrongCommit.status, 1);
    assert.match(`${wrongCommit.stdout}${wrongCommit.stderr}`, /protected release record/);

    fs.writeFileSync(allowed, `release@example.test ssh-ed25519 ${"A".repeat(68)}\n`);
    const wrongSigner = runGuard();
    assert.equal(wrongSigner.status, 1);
    assert.match(wrongSigner.stdout, /not signed by a repository-allowed key/);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("release notes extraction is exact and rejects missing sections", () => {
  const extractor = path.join(repoRoot, "scripts", "extract-release-notes.sh");
  const changelog = [
    "# Changelog",
    "",
    "## 1.2.3 - 2026-07-10",
    "",
    "- Exact note.",
    "",
    "",
    "## 1.2.2 - 2026-07-01",
    "",
    "- Older.",
    "",
  ].join("\n");
  const result = spawnSync(extractor, ["v1.2.3"], { input: changelog, encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, "## 1.2.3 - 2026-07-10\n\n- Exact note.\n");
  const missing = spawnSync(extractor, ["v9.9.9"], { input: changelog, encoding: "utf8" });
  assert.notEqual(missing.status, 0);
});

test("provenance binds the explicit producer manifest, separate packager, notarization IDs, and archive bytes", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-provenance-"));
  const candidate = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-candidate-"));
  const script = path.join(repoRoot, "scripts", "release-provenance.mjs");
  const notes = path.join(directory, "notes.md");
  const tagObject = "a".repeat(40);
  const sourceCommit = "b".repeat(40);
  const verifierCommit = "c".repeat(40);
  const entitlementsSha256 = crypto
    .createHash("sha256")
    .update(fs.readFileSync(path.join(repoRoot, "internal/applevmhelper/vmd-entitlements.plist")))
    .digest("hex");
  const archives = [
    "crabbox_1.2.3_darwin_amd64.tar.gz",
    "crabbox_1.2.3_darwin_arm64.tar.gz",
    "crabbox_1.2.3_linux_amd64.tar.gz",
    "crabbox_1.2.3_linux_arm64.tar.gz",
    "crabbox_1.2.3_windows_amd64.zip",
    "crabbox_1.2.3_windows_arm64.zip",
  ];
  fs.mkdirSync(path.join(candidate, ".components"), { mode: 0o700 });
  for (const name of archives) fs.writeFileSync(path.join(candidate, name), `unsigned:${name}\n`);
  const rawVmd = path.join(candidate, ".components", "crabbox-apple-vm-vmd");
  fs.writeFileSync(rawVmd, "unsigned-vmd\n", { mode: 0o755 });
  fs.chmodSync(rawVmd, 0o755);
  const candidateManifestSha256 = execFileSync(
    process.execPath,
    [
      script,
      "candidate-write",
      "--dir",
      candidate,
      "--tag",
      "v1.2.3",
      "--tag-object",
      tagObject,
      "--source-commit",
      sourceCommit,
      "--verifier-commit",
      verifierCommit,
      "--producer-os",
      "15.5",
      "--producer-arch",
      "arm64",
      "--go-version",
      "go1.26.4",
      "--goreleaser-version",
      "2.17.0",
      "--swift-version",
      "Apple Swift version 6.1 (swiftlang-test)",
      "--xcode-version",
      "16.4",
      "--xcode-build",
      "16F6",
    ],
    { encoding: "utf8" },
  ).trim();
  const writeArgs = [
    "write",
    "--dir",
    directory,
    "--tag",
    "v1.2.3",
    "--tag-object",
    tagObject,
    "--source-commit",
    sourceCommit,
    "--verifier-commit",
    verifierCommit,
    "--notes",
    notes,
    "--candidate-dir",
    candidate,
    "--candidate-manifest-sha256",
    candidateManifestSha256,
    "--embedded-vmd-sha256",
    "d".repeat(64),
    "--embedded-vmd-size",
    "123456",
    "--vmd-entitlements-sha256",
    entitlementsSha256,
    "--notary-cli-amd64",
    "11111111-1111-4111-8111-111111111111",
    "--notary-cli-arm64",
    "22222222-2222-4222-8222-222222222222",
    "--notary-helper-arm64",
    "33333333-3333-4333-8333-333333333333",
    "--notary-vmd-arm64",
    "44444444-4444-4444-8444-444444444444",
    "--packager-go-version",
    "go1.26.4",
    "--packager-os",
    "15.5",
    "--packager-arch",
    "arm64",
    "--packager-xcode-version",
    "16.4",
    "--packager-xcode-build",
    "16F6",
  ];
  const verifyArgs = [
    "verify",
    "--dir",
    directory,
    "--tag",
    "v1.2.3",
    "--tag-object",
    tagObject,
    "--source-commit",
    sourceCommit,
    "--verifier-commit",
    verifierCommit,
    "--notes",
    notes,
  ];
  try {
    fs.writeFileSync(notes, "## 1.2.3 - 2026-07-10\n\n- Release.\n");
    for (const name of archives) {
      fs.writeFileSync(path.join(directory, name), `fixture:${name}\n`);
    }
    execFileSync(process.execPath, [script, ...writeArgs]);
    assert.doesNotThrow(() => execFileSync(process.execPath, [script, ...verifyArgs]));
    const provenance = JSON.parse(fs.readFileSync(path.join(directory, "provenance.json")));
    assert.equal(provenance.producer.manifestSha256, candidateManifestSha256);
    assert.equal(provenance.producer.swift, "Apple Swift version 6.1 (swiftlang-test)");
    assert.equal(provenance.producer.inputs.length, 7);
    assert.equal(provenance.packager.go, "go1.26.4");
    assert.equal(
      provenance.payloads
        .flatMap((entry) => entry.binaries)
        .find((entry) => entry.name === "crabbox-apple-vm-helper").embeddedVmd.size,
      123456,
    );
    fs.appendFileSync(path.join(directory, "crabbox_1.2.3_linux_arm64.tar.gz"), "drift");
    assert.notEqual(spawnSync(process.execPath, [script, ...verifyArgs]).status, 0);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
    fs.rmSync(candidate, { recursive: true, force: true });
  }
});

test("candidate manifest rejects byte, mode, and pinned source drift before signing", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-candidate-drift-"));
  const script = path.join(repoRoot, "scripts", "release-provenance.mjs");
  const tagObject = "a".repeat(40);
  const sourceCommit = "b".repeat(40);
  const verifierCommit = "c".repeat(40);
  const archiveNames = [
    "crabbox_1.2.3_darwin_amd64.tar.gz",
    "crabbox_1.2.3_darwin_arm64.tar.gz",
    "crabbox_1.2.3_linux_amd64.tar.gz",
    "crabbox_1.2.3_linux_arm64.tar.gz",
    "crabbox_1.2.3_windows_amd64.zip",
    "crabbox_1.2.3_windows_arm64.zip",
  ];
  const verifyArgs = [
    script,
    "candidate-verify",
    "--dir",
    directory,
    "--tag",
    "v1.2.3",
    "--tag-object",
    tagObject,
    "--source-commit",
    sourceCommit,
    "--verifier-commit",
    verifierCommit,
  ];
  try {
    fs.mkdirSync(path.join(directory, ".components"), { mode: 0o700 });
    for (const name of archiveNames) fs.writeFileSync(path.join(directory, name), `input:${name}\n`);
    const vmd = path.join(directory, ".components", "crabbox-apple-vm-vmd");
    fs.writeFileSync(vmd, "raw-vmd\n", { mode: 0o755 });
    fs.chmodSync(vmd, 0o755);
    execFileSync(process.execPath, [
      script,
      "candidate-write",
      "--dir",
      directory,
      "--tag",
      "v1.2.3",
      "--tag-object",
      tagObject,
      "--source-commit",
      sourceCommit,
      "--verifier-commit",
      verifierCommit,
      "--producer-os",
      "15.5",
      "--producer-arch",
      "arm64",
      "--go-version",
      "go1.26.4",
      "--goreleaser-version",
      "2.17.0",
      "--swift-version",
      "Apple Swift version 6.1 (swiftlang-test)",
      "--xcode-version",
      "16.4",
      "--xcode-build",
      "16F6",
    ]);
    assert.equal(spawnSync(process.execPath, verifyArgs).status, 0);

    const changedArchive = path.join(directory, archiveNames[0]);
    const originalArchive = fs.readFileSync(changedArchive);
    fs.appendFileSync(changedArchive, "drift");
    assert.notEqual(spawnSync(process.execPath, verifyArgs).status, 0);
    fs.writeFileSync(changedArchive, originalArchive);
    assert.equal(spawnSync(process.execPath, verifyArgs).status, 0);

    fs.chmodSync(vmd, 0o700);
    assert.notEqual(spawnSync(process.execPath, verifyArgs).status, 0);
    fs.chmodSync(vmd, 0o755);
    assert.equal(spawnSync(process.execPath, verifyArgs).status, 0);

    const wrongSource = [...verifyArgs];
    wrongSource[wrongSource.indexOf("--source-commit") + 1] = "d".repeat(40);
    assert.notEqual(spawnSync(process.execPath, wrongSource).status, 0);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

test("Go binary proof checks clean exact VCS and target build info", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-go-buildinfo-"));
  const binary = path.join(root, "fixture");
  const verifier = path.join(repoRoot, "scripts", "verify-go-release-binary.mjs");
  const git = (...args) => execFileSync("git", args, { cwd: root, stdio: "pipe" });
  try {
    git("init", "-b", "main");
    git("config", "user.name", "Build Info Test");
    git("config", "user.email", "build@example.test");
    fs.writeFileSync(path.join(root, "go.mod"), "module example.test/release\n\ngo 1.26\n");
    fs.writeFileSync(path.join(root, "main.go"), "package main\nfunc main() {}\n");
    git("add", "go.mod", "main.go");
    git("commit", "-m", "fixture");
    const commit = git("rev-parse", "HEAD").toString().trim();
    execFileSync("go", ["build", "-trimpath", "-buildvcs=true", "-o", binary, "."], {
      cwd: root,
      env: { ...process.env, CGO_ENABLED: "0", GOOS: process.platform === "darwin" ? "darwin" : "linux", GOARCH: process.arch === "arm64" ? "arm64" : "amd64" },
    });
    const goVersion = execFileSync("go", ["env", "GOVERSION"], { encoding: "utf8" }).trim();
    const goos = process.platform === "darwin" ? "darwin" : "linux";
    const goarch = process.arch === "arm64" ? "arm64" : "amd64";
    assert.doesNotThrow(() =>
      execFileSync(process.execPath, [verifier, binary, "example.test/release", commit, goos, goarch, goVersion]),
    );
    assert.notEqual(
      spawnSync(process.execPath, [
        verifier,
        binary,
        "example.test/release",
        "f".repeat(40),
        goos,
        goarch,
        goVersion,
      ]).status,
      0,
    );
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("signing and verification enforce Foundation identity, runtime, timestamp, and online notarization", () => {
  const config = read("scripts/release-config.sh");
  const signer = read("scripts/codesign-macos.sh");
  const verifier = read("scripts/verify-macos-binary.sh");
  const packager = read("scripts/package-release.sh");
  assert.match(config, /Developer ID Application: OpenClaw Foundation/);
  assert.match(config, /FWJYW4S8P8/);
  assert.match(config, /org\.openclaw\.crabbox/);
  assert.match(config, /org\.openclaw\.crabbox\.apple-vm-helper/);
  assert.match(signer, /--options runtime/);
  assert.match(signer, /--timestamp/);
  assert.match(signer, /notarytool submit/);
  assert.match(signer, /--keychain "\$MAC_RELEASE_CODESIGN_KEYCHAIN"/);
  assert.match(signer, /NOTARY_STATUS.*Accepted/);
  for (const script of [signer, verifier]) {
    assert.match(script, /--verify --strict --check-notarization -R=notarized/);
    assert.match(script, /Authority=\$CRABBOX_RELEASE_AUTHORITY/);
    assert.match(script, /TeamIdentifier=\$CRABBOX_RELEASE_TEAM_ID/);
    assert.doesNotMatch(script, /\bspctl\b|stapler/);
  }
  assert.match(packager, /ALLOWED_SIGNERS="\$ROOT\/\.github\/release-allowed-signers"/);
  assert.match(packager, /RELEASE_RECORD="\$ROOT\/release\/records\/\$TAG\.json"/);
  assert.match(packager, /remote_main=.*ls-remote origin/);
  assert.match(packager, /protected remote main must exactly equal the verifier commit before signing/);
  assert.match(packager, /ALLOWED_SIGNERS RELEASE_RECORD/);
});

test("release documentation forbids automatic publication, deletion, and Homebrew coupling", () => {
  const readme = read("README.md");
  const operations = read("docs/operations.md");
  const security = read("docs/security.md");
  const release = read("docs/RELEASING.md");
  assert.doesNotMatch(`${readme}\n${operations}\n${security}`, /repository_dispatch.*publish/s);
  assert.match(release, /Never delete a partial draft or release/);
  assert.match(release, /Publish with one draft-state transition/);
  assert.match(release, /Update and prove Homebrew/);
  assert.match(
    release,
    /gh api --method GET \\\s+--header 'Accept: application\/vnd\.github\+json' \\\s+"repos\/openclaw\/crabbox\/actions\/artifacts\/\$ARTIFACT_ID\/zip"/,
  );
  assert.match(release, /Developer ID Application: OpenClaw Foundation \(FWJYW4S8P8\)/);
  assert.match(release, /PACKAGE_SCRIPT_SHA256/);
  assert.match(
    release,
    /PACKAGE_SCRIPT_SHA256=\$\(git --no-pager show \\\n  "\$\{VERIFIER_COMMIT\}:scripts\/package-release\.sh"/,
  );
  assert.match(release, /mac-release[\s\S]*\/bin\/bash -c/);
  const secretGateStart = release.indexOf("codesign-run --with-package-secrets --");
  const secretGateEnd = release.indexOf("' crabbox-protected-package", secretGateStart);
  assert.ok(secretGateStart >= 0 && secretGateEnd > secretGateStart);
  const secretGate = release.slice(secretGateStart, secretGateEnd);
  assert.match(secretGate, /core\.fsmonitor=false/);
  assert.match(secretGate, /rev-parse HEAD/);
  assert.match(secretGate, /status --porcelain --untracked-files=all/);
  assert.match(secretGate, /remote get-url origin/);
  assert.match(secretGate, /ls-remote https:\/\/github\.com\/openclaw\/crabbox/);
  assert.match(secretGate, /awk "\{print \\\$1\}"/);
  assert.doesNotMatch(secretGate, /awk "\{print \\\\\\$1\}"/);
  assert.ok(secretGate.indexOf("status --porcelain") < secretGate.indexOf("exec /bin/bash"));
});

test("preserved v0.37.0 identity is pinned and publication-blocked without rewriting the tag", () => {
  const record = JSON.parse(read("release/records/v0.37.0.json"));
  assert.equal(record.tag, "v0.37.0");
  assert.equal(record.tagObject, "d3e0da6a0355372bb3600ef9f2360983acd8272e");
  assert.equal(record.sourceCommit, "99c82134c62e0da795b6165efa6affe7140c20dd");
  assert.equal(record.publicationStatus, "blocked");
  assert.match(record.blocker, /ad-hoc re-signs its embedded VMD/);
  for (const file of [
    "scripts/package-release.sh",
    "scripts/create-release-draft.sh",
    ".github/workflows/release-assets.yml",
  ]) {
    assert.match(read(file), /REQUIRE_PUBLISHABLE/);
  }
});

test("v0.37.1 is pinned to the signed trust-repair source and ready for publication", () => {
  const record = JSON.parse(read("release/records/v0.37.1.json"));
  assert.equal(record.tag, "v0.37.1");
  assert.equal(record.tagObject, "8ce4ec011cc1027622552d1e952b8e0ee3e60198");
  assert.equal(record.sourceCommit, "205dcfc52b60a09239e5ef0267e4a1cdef06b7d3");
  assert.equal(record.publicationStatus, "ready");
});

test("v0.38.0 is pinned to the corrected signed source and ready for publication", () => {
  const record = JSON.parse(read("release/records/v0.38.0.json"));
  assert.equal(record.tag, "v0.38.0");
  assert.equal(record.tagObject, "22f47502e0eafe876ed02b684bf17f951cc2238f");
  assert.equal(record.sourceCommit, "ae1f6b46117c5e067f1370f413e1e6fdf1538d25");
  assert.equal(record.publicationStatus, "ready");
});

test("v0.38.1 is pinned to the signed ready-pool source and ready for publication", () => {
  const record = JSON.parse(read("release/records/v0.38.1.json"));
  assert.equal(record.tag, "v0.38.1");
  assert.equal(record.tagObject, "326b5fa1e4f9543b11bde0583635f37d00f13f0a");
  assert.equal(record.sourceCommit, "3f83f4c58a65d2546620a8b31257f53375fabab2");
  assert.equal(record.publicationStatus, "ready");
});

test("v0.38.2 is pinned and publication-blocked after its immutable tag failed policy", () => {
  const record = JSON.parse(read("release/records/v0.38.2.json"));
  assert.equal(record.tag, "v0.38.2");
  assert.equal(record.tagObject, "b11d63f5a3353ed8117bbfbc92fca0bc2512d1f9");
  assert.equal(record.sourceCommit, "d009660c442c7f072d3058097d1c2a86067c47c1");
  assert.equal(record.publicationStatus, "blocked");
  assert.match(record.blocker, /tag annotation does not exactly equal v0\.38\.2/);
});

test("v0.38.3 is pinned to the cancellation-fix source and ready for publication", () => {
  const record = JSON.parse(read("release/records/v0.38.3.json"));
  assert.equal(record.tag, "v0.38.3");
  assert.equal(record.tagObject, "2c4bee30c6af3039bae986c1a6784d9a2f4ee15c");
  assert.equal(record.sourceCommit, "9c3df396c94fef304975e221d5f0dc26e65d9860");
  assert.equal(record.publicationStatus, "ready");
});

test("v0.38.4 is pinned to the sparse-sync source and ready for publication", () => {
  const record = JSON.parse(read("release/records/v0.38.4.json"));
  assert.equal(record.tag, "v0.38.4");
  assert.equal(record.tagObject, "dd6aa78ed373393c1e9887e9354dd24b449b2f8b");
  assert.equal(record.sourceCommit, "b288e613bd8267ea365c0098da7494b143242d8e");
  assert.equal(record.publicationStatus, "ready");
});

test("managed Foundation signing and notary configuration is repository-owned and secret-free", () => {
  const manifest = read(".mac-release.env");
  const codeowners = read(".github/CODEOWNERS");
  assert.match(
    manifest,
    /MAC_RELEASE_CODESIGN_IDENTITY='Developer ID Application: OpenClaw Foundation \(FWJYW4S8P8\)'/,
  );
  assert.match(manifest, /MAC_RELEASE_OP_FIELDS=NOTARYTOOL_KEYCHAIN_PROFILE/);
  assert.match(manifest, /MAC_RELEASE_CODESIGN_KEYCHAIN_MANAGED=1/);
  assert.match(manifest, /MAC_RELEASE_CODESIGN_PASSWORDLESS=1/);
  assert.match(manifest, /MAC_RELEASE_OP_USE_SERVICE_ACCOUNT=0/);
  assert.doesNotMatch(manifest, /(?:PASSWORD|TOKEN|SECRET)=/);
  assert.match(codeowners, /^\/\.mac-release\.env @openclaw\/openclaw-secops$/m);
});

test("credential-free producer captures tool output before parsing under pipefail", () => {
  const producer = read("scripts/build-release-candidate.sh");
  assert.match(producer, /goreleaser_version_output=\$\(goreleaser --version\)/);
  assert.match(producer, /producer_swift_version_output=\$\(swift --version\)/);
  assert.match(producer, /producer_xcode_version_output=\$\(xcodebuild -version\)/);
  assert.match(producer, /unsigned_vmd_build=\$\(vtool -show-build/);
  assert.doesNotMatch(producer, /(?:goreleaser --version|swift --version|xcodebuild -version|vtool -show-build[^\n]*) \|/);
});

test("credential-bearing packager is pipefail-safe and removes read-only Go toolchains", () => {
  const packager = read("scripts/package-release.sh");
  assert.match(packager, /unsigned_vmd_build=\$\(vtool -show-build/);
  assert.match(packager, /packager_xcode_version_output=\$\(xcodebuild -version\)/);
  assert.doesNotMatch(packager, /(?:xcodebuild -version|vtool -show-build[^\n]*) \|/);
  assert.match(packager, /chmod -R u\+w "\$path"/);
  assert.match(packager, /remove_tree "\$WORK"/);
});

test("draft creation performs static-only verification and never deletes or replaces partial records", () => {
  const script = read("scripts/create-release-draft.sh");
  const verifyIndex = script.indexOf('env -i');
  const listIndex = script.indexOf('gh api --paginate');
  const createIndex = script.indexOf('gh release create');
  assert.ok(verifyIndex >= 0 && verifyIndex < listIndex && listIndex < createIndex);
  assert.match(script, /CRABBOX_VERIFY_MODE=static/);
  assert.doesNotMatch(script, /CRABBOX_VERIFY_MODE=execute/);
  assert.match(script, /--draft/);
  assert.match(script, /--verify-tag/);
  assert.doesNotMatch(script, /--target/);
  assert.doesNotMatch(script, /target_commitish !== process\.env\.RELEASE_COMMIT/);
  assert.match(script, /--notes-file "\$notes"/);
  assert.match(script, /refusing to delete or replace it/);
  assert.doesNotMatch(script, /--method (?:DELETE|PATCH|PUT)|gh release (?:delete|edit|upload)/);
});
