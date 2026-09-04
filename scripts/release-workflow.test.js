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
  assert.match(workflow, /workflow_commit:\n\s+description: "Current protected default-branch workflow commit SHA"\n\s+required: true/);
  assert.match(workflow, /\[\[ "\$\{\{ github\.workflow_sha \}\}" == "\$WORKFLOW_COMMIT" \]\]/);
  assert.match(workflow, /git merge-base --is-ancestor "\$VERIFIER_COMMIT" "\$WORKFLOW_COMMIT"/);
  assert.match(workflow, /git merge-base --is-ancestor "\$SOURCE_COMMIT" "\$VERIFIER_COMMIT"/);
  assert.match(workflow, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(workflow, /persist-credentials: false/);
  assert.match(
    workflow,
    /name: Set up Go for Apple VM source verification\n\s+uses: actions\/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16[\s\S]*name: Verify shipped Apple VM image source[\s\S]*git show "\$RELEASE_COMMIT:internal\/cli\/os_image[.]go"[\s\S]*git show "\$RELEASE_COMMIT:internal\/providers\/applevm\/backend[.]go"[\s\S]*go run [.][/]scripts\/apple-vm-image-source "\$source_root"/,
  );
  assert.match(workflow, /runner: macos-15\n\s+arch: arm64/);
  assert.match(workflow, /runner: macos-15-intel\n\s+arch: x86_64/);
  assert.match(workflow, /cancel-in-progress: false/);
  assert.equal((workflow.match(/GH_TOKEN:/g) ?? []).length, 2);
  assert.equal((workflow.match(/secrets\.CRABBOX_RULESET_READ_TOKEN/g) ?? []).length, 1);
  assert.equal((workflow.match(/contents: write/g) ?? []).length, 1);
  assert.match(workflow, /GH_TOKEN: \$\{\{ secrets\.CRABBOX_RULESET_READ_TOKEN \}\}/);
  assert.match(workflow, /GH_TOKEN: \$\{\{ inputs.draft && github\.token \|\| '' \}\}/);
  assert.match(workflow, /gh api --method GET[\s\S]*releases\/\$RELEASE_ID/);
  assert.match(workflow, /releases\/assets\/\$asset_id/);
  assert.match(workflow, /name: Statically verify with no release credentials[\s\S]*env -i/);
  assert.match(
    workflow,
    /name: Freeze exact static proof before candidate execution[\s\S]*env -i[\s\S]*PATH="\$PATH" VERIFY_ARCH="\$VERIFY_ARCH" node/,
  );
  assert.match(workflow, /name: Execute candidate in isolated clean job without release credentials[\s\S]*exec env -i/);
  assert.match(workflow, /CRABBOX_VERIFY_EXEC_ARCH="\$VERIFY_ARCH"/);
  assert.equal((workflow.match(/CRABBOX_VERIFY_TOOLING_COMMIT="\$WORKFLOW_COMMIT"/g) ?? []).length, 2);
  assert.match(workflow, /verifierCommit: process\.env\.VERIFIER_COMMIT,\n\s+workflowCommit: process\.env\.WORKFLOW_COMMIT,/);
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
    /^\/\.github\/workflows\/verify-homebrew\.yml @openclaw\/openclaw-secops$/m,
  );
});

test("release verifier rejects provenance that is not an ancestor of protected tooling", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-verifier-ancestry-"));
  try {
    fs.mkdirSync(path.join(directory, "scripts"));
    fs.copyFileSync(
      path.join(repoRoot, "scripts", "verify-release.sh"),
      path.join(directory, "scripts", "verify-release.sh"),
    );
    fs.writeFileSync(path.join(directory, "scripts", "release-config.sh"), "#!/usr/bin/env bash\n");
    execFileSync("git", ["init", "-b", "main"], { cwd: directory, stdio: "ignore" });
    execFileSync("git", ["config", "user.name", "Release Test"], { cwd: directory });
    execFileSync("git", ["config", "user.email", "release@example.test"], { cwd: directory });
    execFileSync("git", ["add", "scripts"], { cwd: directory });
    execFileSync("git", ["commit", "-m", "tooling base"], { cwd: directory, stdio: "ignore" });
    const base = execFileSync("git", ["rev-parse", "HEAD"], { cwd: directory, encoding: "utf8" }).trim();
    fs.writeFileSync(path.join(directory, "tooling.txt"), "protected tooling\n");
    execFileSync("git", ["add", "tooling.txt"], { cwd: directory });
    execFileSync("git", ["commit", "-m", "tooling head"], { cwd: directory, stdio: "ignore" });
    const tooling = execFileSync("git", ["rev-parse", "HEAD"], { cwd: directory, encoding: "utf8" }).trim();
    execFileSync("git", ["switch", "--detach", base], { cwd: directory, stdio: "ignore" });
    fs.writeFileSync(path.join(directory, "unrelated.txt"), "unrelated verifier\n");
    execFileSync("git", ["add", "unrelated.txt"], { cwd: directory });
    execFileSync("git", ["commit", "-m", "unrelated verifier"], { cwd: directory, stdio: "ignore" });
    const unrelated = execFileSync("git", ["rev-parse", "HEAD"], { cwd: directory, encoding: "utf8" }).trim();
    execFileSync("git", ["switch", "--detach", tooling], { cwd: directory, stdio: "ignore" });

    const bin = path.join(directory, "bin");
    fs.mkdirSync(bin);
    fs.writeFileSync(
      path.join(bin, "uname"),
      "#!/bin/sh\ncase \"${1:-}\" in -s) echo Darwin ;; -m) echo arm64 ;; *) exit 64 ;; esac\n",
    );
    fs.chmodSync(path.join(bin, "uname"), 0o755);
    for (const tool of ["codesign", "lipo"]) {
      fs.writeFileSync(path.join(bin, tool), "#!/bin/sh\nexit 0\n");
      fs.chmodSync(path.join(bin, tool), 0o755);
    }
    const result = spawnSync(
      "bash",
      [
        path.join(directory, "scripts", "verify-release.sh"),
        "v1.2.3",
        directory,
        "a".repeat(40),
        "b".repeat(40),
        unrelated,
      ],
      {
        cwd: directory,
        encoding: "utf8",
        env: {
          HOME: process.env.HOME,
          PATH: `${bin}:${process.env.PATH}`,
          CRABBOX_VERIFY_EXEC_ARCH: "arm64",
          CRABBOX_VERIFY_MODE: "static",
          CRABBOX_VERIFY_TOOLING_COMMIT: tooling,
        },
      },
    );
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /provenance verifier commit is not an ancestor of protected tooling/);

    const sourceDrift = spawnSync(
      "bash",
      [
        path.join(directory, "scripts", "verify-release.sh"),
        "v1.2.3",
        directory,
        "a".repeat(40),
        unrelated,
        base,
      ],
      {
        cwd: directory,
        encoding: "utf8",
        env: {
          HOME: process.env.HOME,
          PATH: `${bin}:${process.env.PATH}`,
          CRABBOX_VERIFY_EXEC_ARCH: "arm64",
          CRABBOX_VERIFY_MODE: "static",
          CRABBOX_VERIFY_TOOLING_COMMIT: tooling,
        },
      },
    );
    assert.notEqual(sourceDrift.status, 0);
    assert.match(sourceDrift.stderr, /release source commit is not an ancestor of the provenance verifier/);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

function workflowStep(workflow, name) {
  const start = workflow.indexOf(`      - name: ${name}\n`);
  assert.ok(start >= 0, name);
  const end = workflow.indexOf("\n      - name:", start + 1);
  return workflow.slice(start, end < 0 ? undefined : end).split("\n  verify-native:")[0];
}

function workflowShell(step) {
  const body = step.split("        run: |\n")[1];
  assert.ok(body, "step has shell body");
  return body.split("\n").map((line) => line.replace(/^          /, "")).join("\n");
}

test("Homebrew smoke uses protected native tooling and only anonymous fixed-repository assets", () => {
  const workflow = read(".github/workflows/verify-homebrew.yml");
  assert.doesNotMatch(workflow, /public_verifier_run_id|proof ZIP|witness|postflight|actions\/(?:runs|workflows)|GH_TOKEN:|contents: write|curl_bin|update-formula/);
  assert.match(workflow, /expected_workflow_ref="\$GITHUB_REPOSITORY\/\.github\/workflows\/verify-homebrew.yml@\$expected_ref"/);
  assert.match(workflow, /\[\[ "\$REF_PROTECTED" == true \]\]/);
  assert.match(workflow, /\[\[ "\$WORKFLOW_SHA" == "\$RUN_SHA" \]\]/);
  assert.match(workflow, /git merge-base --is-ancestor "\$SOURCE_COMMIT" "\$VERIFIER_COMMIT"/);
  assert.match(workflow, /git merge-base --is-ancestor "\$VERIFIER_COMMIT" "\$WORKFLOW_SHA"/);
  assert.match(workflow, /runner: macos-15\n\s+arch: arm64/);
  assert.match(workflow, /runner: macos-15-intel\n\s+arch: x86_64/);
  assert.match(workflow, /persist-credentials: false/);
  assert.match(workflow, /ref: \$\{\{ github.workflow_sha \}\}/);
  assert.match(workflow, /assets_dir="\$RUNNER_TEMP\/release-assets"/);
  assert.match(workflow, /https:\/\/github.com\/openclaw\/crabbox\/releases\/download\/\$RELEASE_TAG\/\$asset/);
  const verify = workflowStep(workflow, "Verify public Homebrew install without credentials");
  assert.match(verify, /unset ACTIONS_ID_TOKEN_REQUEST_TOKEN ACTIONS_RUNTIME_TOKEN GH_TOKEN GITHUB_TOKEN/);
  assert.match(verify, /"\$TAG_OBJECT" "\$SOURCE_COMMIT" "\$VERIFIER_COMMIT" \\\n\s+"\$RELEASE_ID"/);
  assert.doesNotMatch(verify, /brew tap/); // Formula evaluation is inside the clean launcher.
});

test("public download mode hashes fixed canonical assets without native approval artifacts", () => {
  const workflow = read(".github/workflows/release-assets.yml");
  for (const name of ["Freeze exact static proof before candidate execution", "Preserve exact verified asset identity"]) {
    assert.match(workflowStep(workflow, name), /\n        if: inputs.draft\n/);
  }
  const execution = workflow.slice(workflow.indexOf("  execute-native:"));
  assert.match(execution, /needs: \[guard, download-draft, verify-native\]/);
  assert.doesNotMatch(execution, /verified-assets|upload-artifact/);
  assert.match(execution, /name: release-input/);
  const download = workflowStep(workflow, "Download exact numeric release without executing its bytes");
  assert.match(download, /GH_TOKEN: \$\{\{ inputs.draft && github.token \|\| '' \}\}/);
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-public-download-"));
  try {
    const bin = path.join(root, "bin");
    fs.mkdirSync(bin);
    const names = execFileSync(path.join(repoRoot, "scripts/release-config.sh"), ["assets", "v1.2.3"], { encoding: "utf8" }).trim().split("\n");
    const bytes = Buffer.from("opaque fixture bytes");
    const digest = "sha256:" + crypto.createHash("sha256").update(bytes).digest("hex");
    const notes = "notes\n";
    const release = {
      id: 123, tag_name: "v1.2.3", name: "v1.2.3", body: notes,
      draft: false, immutable: true, prerelease: false, published_at: "2026-08-01T00:00:00Z",
      assets: names.map((name, index) => ({
        name, id: 100 + index, size: bytes.length, state: "uploaded", digest,
        url: `https://api.github.com/repos/openclaw/crabbox/releases/assets/${100 + index}`,
      })),
    };
    const metadata = path.join(root, "release.json");
    const payload = path.join(root, "payload");
    fs.writeFileSync(payload, bytes);
    const calls = path.join(root, "calls");
    const quote = (value) => `'${value.replaceAll("'", `'"'"'`)}'`;
    const writeTool = (name, body) => {
      fs.writeFileSync(path.join(bin, name), body, { mode: 0o755 });
    };
    writeTool("gh", "#!/bin/sh\necho unexpected-gh >&2\nexit 98\n");
    writeTool("curl", `#!/bin/bash
set -eu
[[ "$*" != *Authorization* && -z "\${GH_TOKEN:-}" && "$1" == --disable ]] || exit 97
url=\${!#}
printf '%s\\n' "$url" >>${quote(calls)}
case "$url" in
  https://api.github.com/repos/openclaw/crabbox/releases/123) cat ${quote(metadata)} ;;
  ${names.map((name) => `https://github.com/openclaw/crabbox/releases/download/v1.2.3/${name}`).join("|")}) cat ${quote(payload)} ;;
  *) echo unexpected-endpoint >&2; exit 96 ;;
esac
`);
    const run = (value, tag = "v1.2.3") => {
      fs.writeFileSync(metadata, JSON.stringify(value));
      fs.rmSync(path.join(root, "release-input"), { recursive: true, force: true });
      fs.writeFileSync(calls, "");
      return spawnSync("/bin/bash", ["-c", workflowShell(download)], {
        encoding: "utf8", env: {
          PATH: `${bin}:${process.env.PATH}`, RUNNER_TEMP: root, RELEASE_TAG: tag,
          GITHUB_REPOSITORY: "openclaw/crabbox", RELEASE_ID: "123", EXPECTED_DRAFT: "false",
          EXPECTED_NOTES_BYTES: String(Buffer.byteLength(notes)),
          EXPECTED_NOTES_SHA256: crypto.createHash("sha256").update(notes).digest("hex"),
        },
      });
    };
    const valid = run(release);
    assert.equal(valid.status, 0, valid.stderr);
    assert.equal(fs.readFileSync(calls, "utf8").trim().split("\n").length, 9);
    for (const patch of [
      { name: "../escape" },
      { url: release.assets[0].url + "?x=y" },
      { url: release.assets[0].url.replace("github.com", "github.com@evil.test") },
    ]) {
      const result = run({ ...release, assets: [{ ...release.assets[0], ...patch }, ...release.assets.slice(1)] });
      assert.notEqual(result.status, 0);
      assert.equal(fs.readFileSync(calls, "utf8").trim().split("\n").length, 1);
    }
    const wrongBytes = run({ ...release, assets: release.assets.map((a) => ({ ...a, digest: "sha256:" + "0".repeat(64) })) });
    assert.notEqual(wrongBytes.status, 0);
    const unsafeTag = run(release, "v1.2.3/../../escape");
    assert.notEqual(unsafeTag.status, 0);
    assert.equal(fs.readFileSync(calls, "utf8"), "");
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("script CI fetches signed release tags for publication fixtures", () => {
  const ci = read(".github/workflows/ci.yml");
  const scriptsJob = ci.slice(ci.indexOf("  scripts:"), ci.indexOf("  docs:"));
  assert.match(scriptsJob, /uses: actions\/checkout@[^\n]+\n\s+with:\n\s+fetch-depth: 0/);
  assert.match(scriptsJob, /timeout-minutes: 10/);
});

test("Crabbox CI cannot redefine the organization-required release check", () => {
  const ci = read(".github/workflows/ci.yml");
  assert.doesNotMatch(ci, /^\s+name: Release Check$/m);
  assert.doesNotMatch(ci, /goreleaser\/goreleaser-action/);
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

test("versioned Go installation is hermetic and precedes release builds", () => {
  const gate = read("scripts/verify-go-install.sh");
  assert.match(gate, /go install "\$MODULE\/cmd\/crabbox@\$VERSION"/);
  assert.match(gate, /GOPROXY="file:\/\/\$PROXY" GOSUMDB=off GOTOOLCHAIN=local GOWORK=off/);
  assert.match(gate, /GOPROXY=https:\/\/proxy\.golang\.org GOSUMDB=sum\.golang\.org/);
  assert.doesNotMatch(gate, /proxy\.golang\.org,direct/);
  assert.match(gate, /GOENV=off GOMODCACHE="\$INSTALL_MODCACHE" GOPATH="\$INSTALL_GOPATH"/);
  assert.match(gate, /go version -m -json "\$BINARY"/);
  assert.match(gate, /github\.com\/steipete\/jsonschema\/v6/);
  assert.match(gate, /dependency\.Replace != nil/);
  assert.match(gate, /"\$BINARY" --help/);
  assert.match(gate, /"\$BINARY" run --help/);
  assert.doesNotMatch(gate, /-mindepth|< <\(find|find "\$ZIP_SOURCE"/);
  assert.match(gate, /func sanitizeNestedModules\(root string\)[\s\S]*filepath\.Walk\(root/);
  assert.match(gate, /path != rootGoMod[\s\S]*os\.RemoveAll\(dir\)/);
  assert.match(gate, /go run "\$VERIFY_GO" sanitize-zip "\$ZIP_SOURCE"/);

  const ci = read(".github/workflows/ci.yml");
  assert.match(ci, /scripts\/verify-go-install\.sh v0\.0\.0 "\$\(git rev-parse HEAD\)"/);

  const producer = read("scripts/build-release-candidate.sh");
  const gateIndex = producer.indexOf('verify-go-install.sh" "$TAG" "$TAG_COMMIT"');
  const releaseIndex = producer.indexOf("goreleaser release --clean --skip=publish");
  assert.ok(gateIndex >= 0 && gateIndex < releaseIndex);

  const codeowners = read(".github/CODEOWNERS");
  assert.match(codeowners, /^\/scripts\/verify-go-install\.sh @openclaw\/openclaw-secops$/m);
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

test("release notes extraction ignores Unreleased, is exact, and rejects missing sections", () => {
  const extractor = path.join(repoRoot, "scripts", "extract-release-notes.sh");
  const changelog = [
    "# Changelog",
    "",
    "## Unreleased",
    "",
    "- Pending change, not part of this release.",
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

test("release documentation authorizes normal continuation from one full request and preserves safety gates", () => {
  const readme = read("README.md");
  const operations = read("docs/operations.md");
  const security = read("docs/security.md");
  const release = read("docs/RELEASING.md");
  const documents = {
    "docs/RELEASING.md": release,
    "docs/operations.md": operations,
    "AGENTS.md": read("AGENTS.md"),
    "README.md": readme,
    "docs/security.md": security,
  };
  const repeatedApproval = [
    /approval for one gate does not authorize the next/i,
    /\b(?:obtain|with|requires?|needs?) (?:a |its )?(?:(?:final|own|separate|new|explicit|tap-update|chat) ){1,4}(?:authorization|approval)\b/i,
    /\bseparately authorized (?:tap(?:-update)?|Homebrew|publication)\b/i,
    /\b(?:a separate tap-update authorization|grant a separate tap-update gate)\b/i,
    /\bauthorize publication[^.!?]{0,80}\bseparate gates\b/i,
    /\b(?:stop again|stop for (?:an explicit |the )?publication gate)\b/i,
    /\bpublication requires a new explicit gate\b/i,
  ];
  const contradictions = [];
  for (const [file, source] of Object.entries(documents)) {
    const prose = source.replace(/\s+/g, " ");
    for (const pattern of repeatedApproval) {
      const match = prose.match(pattern);
      if (match) contradictions.push(`${file}: ${match[0]}`);
    }
  }
  assert.deepEqual(contradictions, [], "normal release stages must not require repeated chat approval");
  for (const [file, source] of Object.entries(documents)) {
    const prose = source.replace(/\s+/g, " ");
    assert.match(prose, /\b(?:one|a single) explicit full (?:release(?:\/publish)?|publish) request authorizes\b/i, file);
    assert.match(prose, /without (?:asking again|renewed chat approval)/i, file);
    assert.match(prose, /narrow(?:er)? requests (?:remain|stay) narrow/i, file);
  }
  const opening = release.split("## Trust Anchors", 1)[0].replace(/\s+/g, " ");
  for (const stage of [
    /preparation/i, /tagging/i, /build/i, /sign/i, /private draft/i, /upload/i,
    /native (?:dispatch|verification)/i, /proof/i, /publication/i,
    /independent public-download\/native verification/i, /public Go installation/i,
    /installed-Homebrew smokes/i, /closeout/i,
  ]) {
    assert.match(opening, stage);
  }
  assert.match(opening, /publication, ordinary Homebrew update, independent/);
  assert.match(opening, /original explicit (?:release\/publish |release )?request[^.!?]{0,80}authorization/i);
  assert.match(opening, /GitHub events alone[^.!?]{0,80}(?:not|never)[^.!?]{0,40}authoriz/i);
  const releaseProse = release.replace(/\s+/g, " ");
  assert.match(releaseProse, /no particular approval ruleset or release-team bypass is required/i);
  assert.match(releaseProse, /no administrative freeze or serialization attestation is required/i);
  assert.match(releaseProse, /final GET plus PATCH is not an atomic compare-and-swap/i);
  assert.match(releaseProse, /does not claim exclusive-writer guarantees/i);
  assert.doesNotMatch(release, /CRABBOX_RELEASE_SERIALIZATION_CONFIRMED/);
  assert.match(releaseProse, /explicit cancellation[^.!?]{0,100}renewed direction/i);
  assert.match(releaseProse, /normal continuation[^.!?]{0,100}original (?:release )?authorization/i);
  assert.match(operations.replace(/\s+/g, " "), /bounded[^.!?]{0,100}smoke[^.!?]{0,100}do not[^.!?]{0,100}unrelated provider mutations/i);
  assert.doesNotMatch(`${readme}\n${operations}\n${security}`, /repository_dispatch.*publish/s);
  assert.match(release, /Never delete a partial draft or release/);
  assert.match(release, /Publish with one draft-state transition/);
  assert.match(release, /Update and prove Homebrew/);
  assert.match(release, /gh workflow run update-formula\.yml/);
  assert.match(release, /Publication establishes eligibility/);
  assert.doesNotMatch(release, /render-homebrew|PUBLIC_PROOFS|public_verifier_run_id/);
  assert.match(release, /already-current tap is success/);
  assert.match(release, /metadata check is not a\nRuby sandbox/);
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
  assert.match(
    manifest,
    /^MAC_RELEASE_OP_ITEM='Release - App Store Connect API key \(3373VBN2P4\) - notarization'$/m,
  );
  assert.match(manifest, /MAC_RELEASE_OP_FIELDS=NOTARYTOOL_KEYCHAIN_PROFILE/);
  assert.match(manifest, /^MAC_RELEASE_OP_VAULT=Molty$/m);
  assert.match(manifest, /^MAC_RELEASE_OP_USE_SERVICE_ACCOUNT=1$/m);
  assert.match(manifest, /^MAC_RELEASE_OP_TMUX_SESSION=op-work$/m);
  assert.match(
    manifest,
    /^MAC_RELEASE_CODESIGN_OP_ITEM='Release - macOS signing keychain ref - OpenClaw Foundation'$/m,
  );
  assert.match(manifest, /^MAC_RELEASE_CODESIGN_OP_VAULT=Molty$/m);
  assert.match(manifest, /^MAC_RELEASE_CODESIGN_OP_USE_SERVICE_ACCOUNT=1$/m);
  assert.match(manifest, /MAC_RELEASE_CODESIGN_KEYCHAIN_MANAGED=1/);
  assert.match(manifest, /MAC_RELEASE_CODESIGN_PASSWORDLESS=1/);
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
  const lookupIndex = script.indexOf('gh release view "$TAG"');
  const createIndex = script.indexOf('gh release create');
  assert.ok(verifyIndex >= 0 && verifyIndex < lookupIndex && lookupIndex < createIndex);
  assert.match(script, /CRABBOX_VERIFY_MODE=static/);
  assert.doesNotMatch(script, /CRABBOX_VERIFY_MODE=execute/);
  assert.match(script, /--draft/);
  assert.match(script, /--verify-tag/);
  assert.doesNotMatch(script, /--target/);
  assert.doesNotMatch(script, /target_commitish !== process\.env\.RELEASE_COMMIT/);
  assert.match(script, /--notes-file "\$notes"/);
  assert.match(script, /--json databaseId/);
  assert.match(script, /releases\/\$release_id/);
  assert.doesNotMatch(script, /gh api --paginate/);
  assert.match(script, /refusing to delete or replace it/);
  assert.doesNotMatch(script, /--method (?:DELETE|PATCH|PUT)|gh release (?:delete|edit|upload)/);
});
