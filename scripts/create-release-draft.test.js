import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const scripts = import.meta.dirname;
const credentials = [
  "GH_TOKEN", "GITHUB_TOKEN", "HOMEBREW_TAP_GITHUB_TOKEN", "HOMEBREW_GITHUB_API_TOKEN",
  "ACTIONS_RUNTIME_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "CODESIGN_IDENTITY",
  "MAC_RELEASE_CODESIGN_IDENTITY", "NOTARYTOOL_KEYCHAIN_PROFILE",
];

function executable(file, source) {
  fs.writeFileSync(file, source, { mode: 0o755 });
}

// Only fixture tools and explicitly selected local utilities are reachable.
// No real git, gh, signing tools, network clients, or candidate code can run.
function fixture(t, options = {}) {
  const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-draft-cleanup-")));
  t.after(() => {
    function writable(directory) {
      fs.chmodSync(directory, 0o700);
      for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
        if (entry.isDirectory()) writable(path.join(directory, entry.name));
      }
    }
    writable(root);
    fs.rmSync(root, { recursive: true, force: true });
  });
  for (const name of ["scripts", "bin", "tmp", "operator-home", "outside-cache", "assets"]) {
    fs.mkdirSync(path.join(root, name));
  }
  for (const name of ["create-release-draft.sh", "release-config.sh", "extract-release-notes.sh"]) {
    fs.copyFileSync(path.join(scripts, name), path.join(root, "scripts", name));
  }
  executable(path.join(root, "scripts", "verify-release-source.sh"), "#!/bin/sh\nexit 0\n");
  for (const name of ["bash", "env", "dirname", "mktemp", "find", "chmod", "rm", "awk", "sort", "shasum", "cmp", "mkdir"]) {
    const tool = execFileSync("/bin/sh", ["-c", 'command -v "$1"', "fixture", name], {
      encoding: "utf8",
    }).trim();
    fs.symlinkSync(tool, path.join(root, "bin", name));
  }
  fs.symlinkSync(process.execPath, path.join(root, "bin", "node"));
  const outside = path.join(root, "outside-cache");
  fs.writeFileSync(path.join(outside, "sentinel"), "outside cache must survive\n", { mode: 0o444 });
  fs.chmodSync(outside, 0o555);
  const settings = { ...options, credentials };
  fs.writeFileSync(path.join(root, "settings.json"), JSON.stringify(settings));

  executable(path.join(root, "scripts", "verify-release.sh"), `#!/usr/bin/env node
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const root = path.dirname(__dirname);
const settings = JSON.parse(fs.readFileSync(path.join(root, "settings.json")));
for (const name of [...settings.credentials, "GOMODCACHE", "GOPATH", "GOTOOLCHAIN", "GOENV"]) {
  assert.equal(process.env[name], undefined, name + " leaked into static verification");
}
assert.equal(process.env.CRABBOX_VERIFY_MODE, "static");
assert.equal(process.env.HOME, process.env.TMPDIR);
assert.equal(path.dirname(process.env.HOME), path.join(root, "tmp"));
const home = process.env.HOME;
fs.writeFileSync(path.join(root, "verify-home"), home);
const cache = path.join(home, "go/pkg/mod");
const toolchain = path.join(cache, "golang.org/toolchain@v0.0.1-go1.99.0.fixture");
const nested = path.join(toolchain, "pkg/include");
fs.mkdirSync(nested, { recursive: true });
fs.writeFileSync(path.join(nested, "textflag.h"), "fixture", { mode: 0o444 });
fs.symlinkSync(path.join(root, "outside-cache"), path.join(toolchain, "outside-directory"));
fs.symlinkSync(path.join(root, "outside-cache/sentinel"), path.join(toolchain, "outside-file"));
fs.symlinkSync(path.join(root, "missing"), path.join(toolchain, "dangling"));
fs.linkSync(path.join(root, "outside-cache/sentinel"), path.join(toolchain, "hardlink"));
for (const directory of [nested, path.dirname(nested), toolchain, cache]) {
  fs.chmodSync(directory, 0o555);
}
if (settings.blockParent) fs.chmodSync(path.dirname(home), 0o500);
process.exit(settings.verifyExit ?? 0);
`);

  // Model the external services entirely in the fixture. The real draft script
  // still checks its record and all eight downloaded byte streams itself.
  executable(path.join(root, "bin", "fixture-tool"), `#!/usr/bin/env node
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const root = path.dirname(__dirname);
const settings = JSON.parse(fs.readFileSync(path.join(root, "settings.json")));
const tool = path.basename(process.argv[1]);
const args = process.argv.slice(2);
const recordFile = path.join(root, "record.json");
const output = (text) => process.stdout.write(text + "\\n");
if (tool === "uname") {
  assert.ok(["-s", "-m"].includes(args[0]));
  output(args[0] === "-s" ? "Darwin" : "arm64");
} else if (tool === "git") {
  assert.equal(args[0], "-C");
  assert.equal(args[1], root);
  const command = args.slice(2).join(" ");
  if (command === "rev-parse HEAD") output("c".repeat(40));
  else if (command === "status --porcelain --untracked-files=normal") {}
  else if (command === "ls-remote --tags origin refs/tags/v1.2.3 refs/tags/v1.2.3^{}") {
    output("a".repeat(40) + "\\trefs/tags/v1.2.3\\n" + "b".repeat(40) + "\\trefs/tags/v1.2.3^{}");
  } else if (command === "ls-remote origin refs/heads/main") output("c".repeat(40) + "\\trefs/heads/main");
  else if (command === "show " + "b".repeat(40) + ":CHANGELOG.md") output("# Changelog\\n\\n## 1.2.3\\n\\n- Synthetic fixture.\\n");
  else throw new Error("unexpected fixture git command: " + command);
} else if (tool === "gh") {
  assert.equal(process.env.GH_TOKEN, "synthetic-test-only");
  if (args[0] === "release" && args[1] === "view") {
    if (!fs.existsSync(recordFile)) process.exit(1);
    if (settings.readbackExit) process.exit(settings.readbackExit);
    output(JSON.stringify({ databaseId: 123 }));
  } else if (args[0] === "release" && args[1] === "create") {
    if (settings.createExit) process.exit(settings.createExit);
    const notes = fs.readFileSync(args[args.indexOf("--notes-file") + 1], "utf8");
    const assets = args.slice(-8).map((file, index) => {
      assert.equal(path.dirname(file), path.join(root, "assets"));
      const bytes = fs.readFileSync(file);
      return { id: index + 1, name: path.basename(file), size: bytes.length, state: "uploaded",
        digest: "sha256:" + crypto.createHash("sha256").update(bytes).digest("hex") };
    });
    fs.writeFileSync(recordFile, JSON.stringify({ id: 123, tag_name: "v1.2.3", name: "v1.2.3",
      body: notes, draft: true, immutable: false, prerelease: false, published_at: null, assets }));
  } else if (args[0] === "api") {
    const record = JSON.parse(fs.readFileSync(recordFile));
    const endpoint = args.at(-1);
    if (endpoint.endsWith("/releases/123")) output(JSON.stringify(record));
    else {
      const asset = record.assets.find((asset) => endpoint.endsWith("/releases/assets/" + asset.id));
      assert.ok(asset, "unexpected fixture endpoint");
      fs.appendFileSync(path.join(root, "downloads"), asset.name + "\\n");
      process.stdout.write(fs.readFileSync(path.join(root, "assets", asset.name)));
    }
  } else throw new Error("unexpected fixture gh command");
} else if (tool === "jq") {
  assert.equal(args[0], "-r");
  const value = JSON.parse(fs.readFileSync(args[2]));
  if (args[1] === ".databaseId") output(String(value.databaseId));
  else {
    assert.equal(args[1], ".assets[] | [.id, .name, .size, .digest] | @tsv");
    for (const asset of value.assets) output([asset.id, asset.name, asset.size, asset.digest].join("\\t"));
  }
} else if (tool === "stat") {
  assert.deepEqual(args.slice(0, 2), ["-f", "%z"]);
  output(String(fs.statSync(args[2]).size));
} else throw new Error("unexpected fixture tool");
`);
  for (const name of ["uname", "git", "gh", "jq", "stat"]) {
    fs.symlinkSync("fixture-tool", path.join(root, "bin", name));
  }
  if (options.rmExit) {
    fs.unlinkSync(path.join(root, "bin", "rm"));
    executable(path.join(root, "bin", "rm"), `#!/bin/sh\necho 'fixture rm failure' >&2\nexit ${options.rmExit}\n`);
  }
  const assets = ["darwin_amd64.tar.gz", "darwin_arm64.tar.gz", "linux_amd64.tar.gz",
    "linux_arm64.tar.gz", "windows_amd64.zip", "windows_arm64.zip"]
    .map((platform) => `crabbox_1.2.3_${platform}`).concat(["checksums.txt", "provenance.json"]);
  for (const name of assets) fs.writeFileSync(path.join(root, "assets", name), `synthetic ${name}\n`);
  const result = spawnSync("/bin/bash", [
    path.join(root, "scripts", "create-release-draft.sh"), "v1.2.3",
    "a".repeat(40), "b".repeat(40), "c".repeat(40), path.join(root, "assets"), "v1.2.3",
  ], {
    cwd: root,
    encoding: "utf8",
    env: {
      PATH: path.join(root, "bin"), HOME: path.join(root, "operator-home"),
      TMPDIR: path.join(root, "tmp"), GOMODCACHE: outside, GOPATH: outside,
      GOTOOLCHAIN: "auto", GOENV: path.join(outside, "sentinel"),
      ...Object.fromEntries(credentials.map((name) => [name, "synthetic-test-only"])),
    },
  });
  assert.equal(result.error, undefined);
  assert.equal(fs.readFileSync(path.join(outside, "sentinel"), "utf8"), "outside cache must survive\n");
  assert.equal(fs.statSync(outside).mode & 0o777, 0o555);
  assert.equal(fs.statSync(path.join(outside, "sentinel")).mode & 0o777, 0o444);
  assert.deepEqual(fs.readdirSync(outside), ["sentinel"]);
  assert.deepEqual(fs.readdirSync(path.join(root, "operator-home")), []);
  assert.ok(fs.existsSync(path.join(root, "verify-home")), result.stderr);
  const home = fs.readFileSync(path.join(root, "verify-home"), "utf8");
  return { root, home, result };
}

test("draft cleanup removes read-only private caches, preserves outside links, and isolates credentials", (t) => {
  const { root, home, result } = fixture(t);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(fs.existsSync(home), false);
  assert.equal(fs.readFileSync(path.join(root, "downloads"), "utf8").trim().split("\n").length, 8);
  assert.match(result.stdout, /Created immutable private draft release_id=123 tag=v1\.2\.3/);
});

for (const [phase, code] of [["verify", 23], ["create", 31], ["readback", 37]]) {
  test(`draft cleanup preserves ${phase} failure status`, (t) => {
    const { home, result } = fixture(t, { [`${phase}Exit`]: code });
    assert.equal(result.status, code, result.stderr);
    assert.equal(fs.existsSync(home), false);
    assert.doesNotMatch(result.stdout, /Created immutable private draft/);
  });
}

for (const primary of [0, 23]) {
  test(`draft cleanup reports real permission failure with primary status ${primary}`, (t) => {
    if (process.getuid?.() === 0) {
      t.skip("root bypasses directory write permissions; injected rm failures are tested separately");
      return;
    }
    const { home, result } = fixture(t, { blockParent: true, verifyExit: primary });
    assert.equal(result.status, primary || 1, result.stderr);
    assert.equal(fs.existsSync(home), true);
    assert.match(result.stderr, /[Pp]ermission denied/);
    assert.ok(result.stderr.includes(`failed to remove draft verification home: ${home}`));
    assert.doesNotMatch(result.stdout, /Created immutable private draft/);
  });

  test(`draft cleanup reports rm failure with primary status ${primary}`, (t) => {
    const { home, result } = fixture(t, { rmExit: 73, verifyExit: primary });
    assert.equal(result.status, primary || 73, result.stderr);
    assert.equal(fs.existsSync(home), true);
    assert.match(result.stderr, /fixture rm failure/);
    assert.ok(result.stderr.includes(`failed to remove draft verification home: ${home}`));
    assert.doesNotMatch(result.stdout, /Created immutable private draft/);
  });
}
