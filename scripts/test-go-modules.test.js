import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");

function writeExecutable(file, body) {
  fs.writeFileSync(file, body, "utf8");
  fs.chmodSync(file, 0o755);
}

function moduleFixture(t, findScript) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-test-go-modules-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const root = path.join(dir, "repo with spaces");
  const bin = path.join(dir, "bin");
  const goLog = path.join(dir, "go.log");
  fs.mkdirSync(bin);
  fs.mkdirSync(path.join(root, "scripts"), { recursive: true });
  fs.copyFileSync(
    path.join(repoRoot, "scripts/test-go-modules.sh"),
    path.join(root, "scripts/test-go-modules.sh"),
  );
  const modules = [".", "nested module", "other/deep module"];
  for (const module of modules) {
    fs.mkdirSync(path.join(root, module), { recursive: true });
    fs.writeFileSync(path.join(root, module, "go.mod"), "module example.test/fixture\n");
  }
  if (findScript) {
    writeExecutable(path.join(bin, "find"), `#!/usr/bin/env bash\nset -euo pipefail\n${findScript}`);
  }

  writeExecutable(
    path.join(bin, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\t%s\\n' "$PWD" "$*" >>"$GO_TEST_LOG"
exit "\${GO_TEST_EXIT:-0}"
`,
  );

  return {
    root,
    modules,
    calls: () => (fs.existsSync(goLog) ? fs.readFileSync(goLog, "utf8").trimEnd().split("\n") : []),
    run: (exitCode = 0) => {
      const result = spawnSync("bash", ["scripts/test-go-modules.sh"], {
        cwd: root,
        env: {
          ...process.env,
          PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
          GO_TEST_LOG: goLog,
          GO_TEST_EXIT: String(exitCode),
        },
        encoding: "utf8",
      });
      assert.ifError(result.error);
      return result;
    },
  };
}

for (const discovery of ["real find", "fake find"]) {
  test(`all modules including root run once with a 15m timeout and spaced paths (${discovery})`, (t) => {
    const fixture = moduleFixture(
      t,
      discovery === "fake find"
        ? `printf '%s\\0' "$1/go.mod" "$1/nested module/go.mod" "$1/other/deep module/go.mod"\n`
        : undefined,
    );
    for (const excluded of [".git", "node_modules", "dist", "dist-cloudflare"]) {
      fs.mkdirSync(path.join(fixture.root, excluded));
      fs.writeFileSync(path.join(fixture.root, excluded, "go.mod"), "module example.test/excluded\n");
    }
    const result = fixture.run();
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.deepEqual(
      fixture.calls().sort(),
      fixture.modules
        .map((module) => `${fs.realpathSync(path.join(fixture.root, module))}\ttest -timeout=15m ./...`)
        .sort(),
    );
    assert.match(result.stdout, /go test -timeout=15m \.\/\.\.\./);
  });
}

test("go module discovery failure aborts before reporting success", (t) => {
  const fixture = moduleFixture(
    t,
    `printf '%s\\0' "$1/go.mod"
printf 'find: partial traversal failure\\n' >&2
exit 7
`,
  );
  const result = fixture.run();

  assert.notEqual(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /failed to discover go\.mod files/);
  assert.deepEqual(fixture.calls(), [], "go test should not run after incomplete discovery");
});

test("empty go module discovery fails closed", (t) => {
  const fixture = moduleFixture(t, "exit 0\n");
  const result = fixture.run();
  assert.notEqual(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /no go\.mod files found/);
  assert.deepEqual(fixture.calls(), []);
});

test("ordinary module test failure propagates without running later modules", (t) => {
  const fixture = moduleFixture(
    t,
    `printf '%s\\0' "$1/go.mod" "$1/nested module/go.mod"\n`,
  );
  const result = fixture.run(23);
  assert.equal(result.status, 23, result.stdout + result.stderr);
  assert.deepEqual(fixture.calls(), [`${fs.realpathSync(fixture.root)}\ttest -timeout=15m ./...`]);
});
