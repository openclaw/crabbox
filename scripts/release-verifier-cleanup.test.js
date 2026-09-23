import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const scripts = import.meta.dirname;

function executable(file, contents) {
  fs.writeFileSync(file, contents, { mode: 0o755 });
}

function fixture(t, kind, options = {}) {
  const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-verifier-cleanup-")));
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
  for (const name of ["scripts", "bin", "tmp", "home", "outside-cache", "assets"]) {
    fs.mkdirSync(path.join(root, name));
  }
  for (const name of ["verify-release.sh", "verify-homebrew-release.sh", "release-config.sh", "extract-release-notes.sh"]) {
    fs.copyFileSync(path.join(scripts, name), path.join(root, "scripts", name));
  }
  // Real local filesystem tools, but no Git, signing tools, or network clients.
  for (const name of ["bash", "env", "dirname", "mktemp", "find", "chmod", "rm", "awk", "sort", "shasum", "basename", "mkdir", "tar", "unzip"]) {
    const tool = execFileSync("/bin/sh", ["-c", 'command -v "$1"', "fixture", name], { encoding: "utf8" }).trim();
    fs.symlinkSync(tool, path.join(root, "bin", name));
  }
  fs.symlinkSync(process.execPath, path.join(root, "bin", "node"));
  for (const tool of ["codesign", "go", "lipo"]) {
    executable(path.join(root, "bin", tool), "#!/bin/sh\nexit 0\n");
  }
  fs.writeFileSync(path.join(root, "settings.json"), JSON.stringify(options));
  const outside = path.join(root, "outside-cache");
  fs.writeFileSync(path.join(outside, "sentinel"), "outside cache must survive\n", { mode: 0o444 });
  fs.chmodSync(outside, 0o555);
  executable(path.join(root, "scripts", "make-cache"), `#!/usr/bin/env node
const fs = require("node:fs");
const path = require("node:path");
const root = path.dirname(__dirname);
const settings = JSON.parse(fs.readFileSync(path.join(root, "settings.json")));
const work = process.argv[2];
fs.mkdirSync(work, { recursive: true });
fs.writeFileSync(path.join(root, "verify-work"), work);
const cache = path.join(work, "runtime-tool/gomodcache");
const toolchain = path.join(cache, "golang.org/toolchain@v0.0.1-go1.99.0.fixture");
const nested = path.join(toolchain, "pkg/include");
fs.mkdirSync(nested, { recursive: true });
fs.writeFileSync(path.join(nested, "textflag.h"), "fixture", { mode: 0o444 });
fs.symlinkSync(path.join(root, "outside-cache"), path.join(toolchain, "outside-directory"));
fs.symlinkSync(path.join(root, "outside-cache/sentinel"), path.join(toolchain, "outside-file"));
fs.symlinkSync(path.join(root, "missing"), path.join(toolchain, "dangling"));
fs.linkSync(path.join(root, "outside-cache/sentinel"), path.join(toolchain, "hardlink"));
for (const directory of [nested, path.dirname(nested), toolchain, cache]) fs.chmodSync(directory, 0o555);
if (settings.blockParent) fs.chmodSync(path.dirname(work), 0o500);
process.exit(settings.primaryExit ?? 0);
`);
  if (options.rmExit) {
    fs.unlinkSync(path.join(root, "bin", "rm"));
    executable(path.join(root, "bin", "rm"), `#!/bin/sh\necho 'fixture rm failure' >&2\nexit ${options.rmExit}\n`);
  }
  executable(path.join(root, "bin", "uname"), `#!/bin/sh
case "$1" in
  -s) printf 'Darwin\\n' ;;
  -m) printf 'arm64\\n' ;;
  *) exit 98 ;;
esac
`);
  executable(path.join(root, "bin", "git"), `#!/usr/bin/env node
const assert = require("node:assert/strict");
const path = require("node:path");
assert.deepEqual(process.argv.slice(2, 4), ["-C", path.dirname(__dirname)]);
const command = process.argv.slice(4).join(" ");
if (command === "rev-parse HEAD") console.log("c".repeat(40));
else if (command === "status --porcelain --untracked-files=normal") {}
else if (command.startsWith("merge-base --is-ancestor ")) {}
else if (command === "show " + "b".repeat(40) + ":CHANGELOG.md") console.log("# Changelog\\n\\n## 1.2.3\\n\\n- Synthetic fixture.\\n");
else if (command === "cat-file -e " + "b".repeat(40) + ":cmd/crabbox-runtime/main.go") process.exit(1);
else throw new Error("unexpected fixture git command: " + command);
`);
  for (const name of ["verify-release-source.sh", "verify-macos-binary.sh"]) {
    executable(path.join(root, "scripts", name), "#!/bin/sh\nexit 0\n");
  }
  fs.writeFileSync(path.join(root, "scripts", "release-policy.mjs"), 'console.log("go1.26.5");\n');
  for (const name of ["verify-go-release-binary.mjs", "release-provenance.mjs"]) {
    fs.writeFileSync(path.join(root, "scripts", name), "process.exit(0);\n");
  }
  fs.writeFileSync(path.join(root, "scripts", "extract-release-vmd.mjs"), 'import fs from "node:fs"; fs.writeFileSync(process.argv[4], "synthetic VMD");\n');
  executable(path.join(root, "scripts", "build-release-runtime-tool.sh"), `#!/bin/bash
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
"$root/scripts/make-cache" "\${1%/runtime-tool}"
/bin/cp "$root/scripts/fixture-runtime-artifacts" "$1/runtime-artifacts"
`);
  executable(path.join(root, "scripts", "fixture-runtime-artifacts"), `#!/usr/bin/env node
const assert = require("node:assert/strict");
const fs = require("node:fs");
const args = process.argv.slice(2);
assert.equal(args[0], "extract");
const destination = args[args.indexOf("--directory") + 1];
fs.mkdirSync(destination);
console.log("{}");
`);
  const assets = ["darwin_amd64.tar.gz", "darwin_arm64.tar.gz", "linux_amd64.tar.gz", "linux_arm64.tar.gz", "windows_amd64.zip", "windows_arm64.zip"]
    .map((platform) => `crabbox_1.2.3_${platform}`).concat("provenance.json");
  const checksumLines = assets.map((name) => {
    const bytes = name === "provenance.json" ? '{"payloads":[]}' : `synthetic ${name}\n`;
    fs.writeFileSync(path.join(root, "assets", name), bytes);
    return `${createHash("sha256").update(bytes).digest("hex")}  ${name}\n`;
  });
  fs.writeFileSync(path.join(root, "assets", "checksums.txt"), checksumLines.join(""));
  const args = kind === "release"
    ? [path.join(root, "scripts", "verify-release.sh"), "v1.2.3", path.join(root, "assets"), "a".repeat(40), "b".repeat(40), "c".repeat(40)]
    : ["-c", `source "$1/scripts/verify-homebrew-release.sh"
CRABBOX_HOMEBREW_VERIFY_WORK=$(mktemp -d "$1/tmp/crabbox-homebrew-verify.XXXXXX")
trap cleanup_homebrew_work EXIT
"$1/scripts/make-cache" "$CRABBOX_HOMEBREW_VERIFY_WORK"
`, "homebrew-cleanup-test", root];
  const result = spawnSync("/bin/bash", args, {
    cwd: root, encoding: "utf8",
    env: {
      PATH: path.join(root, "bin"), HOME: path.join(root, "home"), TMPDIR: path.join(root, "tmp"),
      CRABBOX_VERIFY_EXEC_ARCH: "arm64", CRABBOX_VERIFY_MODE: "static",
    },
  });
  assert.equal(result.error, undefined);
  assert.equal(fs.readFileSync(path.join(outside, "sentinel"), "utf8"), "outside cache must survive\n");
  assert.equal(fs.statSync(outside).mode & 0o777, 0o555);
  assert.equal(fs.statSync(path.join(outside, "sentinel")).mode & 0o777, 0o444);
  assert.deepEqual(fs.readdirSync(outside), ["sentinel"]);
  assert.deepEqual(fs.readdirSync(path.join(root, "home")), []);
  assert.ok(fs.existsSync(path.join(root, "verify-work")), result.stderr);
  const work = fs.readFileSync(path.join(root, "verify-work"), "utf8");
  return { work, result };
}

for (const kind of ["release", "homebrew"]) {
  for (const primaryExit of [0, 23]) {
    test(`${kind} verifier removes read-only caches without crossing links and preserves status ${primaryExit}`, (t) => {
      const { work, result } = fixture(t, kind, { primaryExit });
      assert.equal(result.status, primaryExit, result.stderr);
      assert.equal(fs.existsSync(work), false);
      if (kind === "release" && primaryExit === 0) assert.match(result.stdout, /Statically verified v1\.2\.3/);
    });

    test(`${kind} verifier reports cleanup failure and preserves primary status ${primaryExit}`, (t) => {
      const { work, result } = fixture(t, kind, { primaryExit, rmExit: 73 });
      assert.equal(result.status, primaryExit || 73, result.stderr);
      assert.equal(fs.existsSync(work), true);
      assert.match(result.stderr, /fixture rm failure/);
      assert.ok(result.stderr.includes(`verification work directory: ${work}`));
    });

    test(`${kind} verifier reports actual cleanup permission failure with primary status ${primaryExit}`, (t) => {
      if (process.getuid?.() === 0) {
        t.skip("root bypasses directory permissions; injected failures are tested separately");
        return;
      }
      const { work, result } = fixture(t, kind, { primaryExit, blockParent: true });
      assert.equal(result.status, primaryExit || 1, result.stderr);
      assert.equal(fs.existsSync(work), true);
      assert.match(result.stderr, /[Pp]ermission denied/);
      assert.ok(result.stderr.includes(`verification work directory: ${work}`));
    });
  }
}
