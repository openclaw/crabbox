import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

test("static Go inspection forces the local toolchain and still validates build metadata", (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-static-go-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const binary = path.join(root, "binary.json");
  const commit = "a".repeat(40);
  fs.writeFileSync(path.join(root, "go.mod"), "module example.test/fixture\n\ngo 1.99.0\n");
  // A fake inspector is sufficient to check argv/env and metadata rejection,
  // with no installed Go version, build, toolchain download, or binary execution.
  fs.writeFileSync(path.join(root, "go"), `#!${process.execPath}
const assert = require("node:assert/strict");
const fs = require("node:fs");
assert.equal(process.env.GOTOOLCHAIN, "local");
assert.deepEqual(process.argv.slice(2, 5), ["version", "-m", "-json"]);
process.stdout.write(fs.readFileSync(process.argv[5]));
`, { mode: 0o755 });
  const info = {
    Path: "example.test/fixture",
    GoVersion: "go1.26.4",
    Settings: Object.entries({
      "-trimpath": "true", CGO_ENABLED: "0", GOOS: "linux", GOARCH: "amd64",
      "vcs.revision": commit, "vcs.modified": "false",
    }).map(([Key, Value]) => ({ Key, Value })),
  };
  const run = (value) => {
    fs.writeFileSync(binary, JSON.stringify(value));
    return spawnSync(process.execPath, [
      path.join(import.meta.dirname, "verify-go-release-binary.mjs"), binary,
      "example.test/fixture", commit, "linux", "amd64", "go1.26.4",
    ], { cwd: root, encoding: "utf8", env: { HOME: root, PATH: root, GOTOOLCHAIN: "auto" } });
  };
  const valid = run(info);
  assert.equal(valid.status, 0, valid.stderr);
  const wrongGo = run({ ...info, GoVersion: "go1.26.3" });
  assert.notEqual(wrongGo.status, 0);
  assert.match(wrongGo.stderr, /Go version .* does not equal go1\.26\.4/);
  for (const { Key } of info.Settings) {
    const invalid = run({ ...info, Settings: info.Settings.filter((setting) => setting.Key !== Key) });
    assert.notEqual(invalid.status, 0);
    assert.ok(invalid.stderr.includes(`build setting ${Key}=undefined`), invalid.stderr);
  }
  const wrongPath = run({ ...info, Path: "example.test/other" });
  assert.notEqual(wrongPath.status, 0);
  assert.match(wrongPath.stderr, /package path .* does not equal example.test\/fixture/);
});
