import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { writeExecutable } from "./test-support/smoke-fixtures.mjs";

const repoRoot = path.resolve(import.meta.dirname, "..");

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
case "$*" in
  'list ./internal/cli')
    printf '%s\\n' "\${GO_CLI_PACKAGE-example.test/fixture/internal/cli}"
    exit "\${GO_CLI_LIST_EXIT:-0}"
    ;;
  'list ./...')
    printf '%s\\n' "\${GO_ROOT_PACKAGES-example.test/fixture/internal/cli
example.test/fixture/core
example.test/fixture/internal/cli-extra}"
    exit "\${GO_ROOT_LIST_EXIT:-0}"
    ;;
esac
exit "\${GO_TEST_EXIT:-0}"
`,
  );

  return {
    root,
    modules,
    calls: () => (fs.existsSync(goLog) ? fs.readFileSync(goLog, "utf8").trimEnd().split("\n") : []),
    run: (exitCode = 0, args = [], env = {}) => {
      const result = spawnSync("bash", ["scripts/test-go-modules.sh", ...args], {
        cwd: root,
        env: {
          ...process.env,
          PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
          GO_TEST_LOG: goLog,
          GO_TEST_EXIT: String(exitCode),
          ...env,
        },
        encoding: "utf8",
      });
      assert.ifError(result.error);
      return result;
    },
  };
}

for (const discovery of ["real find", "fake find"]) {
  test(`all modules including root run once with a 20m timeout and spaced paths (${discovery})`, (t) => {
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
        .map((module) => `${fs.realpathSync(path.join(fixture.root, module))}\ttest -timeout=20m ./...`)
        .sort(),
    );
    assert.match(result.stdout, /go test -timeout=20m \.\/\.\.\./);
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
  assert.deepEqual(fixture.calls(), [`${fs.realpathSync(fixture.root)}\ttest -timeout=20m ./...`]);
});

test("CI partition excludes only the exact root CLI package and preserves every nested module", (t) => {
  const fixture = moduleFixture(t);
  fs.mkdirSync(path.join(fixture.root, "nested module/internal/cli"), { recursive: true });
  const result = fixture.run(0, ["--exclude-root-cli"]);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  const root = fs.realpathSync(fixture.root);
  assert.deepEqual(fixture.calls().slice(0, 2), [
    `${root}\tlist ./internal/cli`,
    `${root}\tlist ./...`,
  ]);
  assert.deepEqual(fixture.calls().slice(2).sort(), [
    `${root}\ttest -count=1 -timeout=20m example.test/fixture/core example.test/fixture/internal/cli-extra`,
    ...fixture.modules.slice(1).map((module) => `${fs.realpathSync(path.join(root, module))}\ttest -timeout=20m ./...`),
  ].sort());
});

for (const [name, env] of [
  ["CLI discovery failure", { GO_CLI_LIST_EXIT: "9" }],
  ["root discovery failure", { GO_ROOT_LIST_EXIT: "9" }],
  ["empty CLI import", { GO_CLI_PACKAGE: "" }],
  ["multiple CLI imports", { GO_CLI_PACKAGE: "first\nsecond" }],
  ["missing root CLI", { GO_ROOT_PACKAGES: "example.test/fixture/core" }],
  ["duplicate root CLI", { GO_ROOT_PACKAGES: "example.test/fixture/internal/cli\nexample.test/fixture/internal/cli\nexample.test/fixture/core" }],
  ["empty non-CLI root", { GO_ROOT_PACKAGES: "example.test/fixture/internal/cli" }],
]) {
  test(`CI partition rejects ${name} before running tests`, (t) => {
    const fixture = moduleFixture(t);
    const result = fixture.run(0, ["--exclude-root-cli"], env);
    assert.notEqual(result.status, 0, result.stdout + result.stderr);
    assert.equal(fixture.calls().some((call) => call.includes("\ttest ")), false);
  });
}

test("CI partition rejects incomplete module discovery before Go discovery", (t) => {
  const fixture = moduleFixture(t, `printf '%s\\0' "$1/go.mod"; exit 7\n`);
  const result = fixture.run(0, ["--exclude-root-cli"]);
  assert.notEqual(result.status, 0);
  assert.deepEqual(fixture.calls(), []);
});

test("CI partition propagates ordinary test failure without running later modules", (t) => {
  const fixture = moduleFixture(t, `printf '%s\\0' "$1/go.mod" "$1/nested module/go.mod"\n`);
  const result = fixture.run(23, ["--exclude-root-cli"]);
  assert.equal(result.status, 23, result.stdout + result.stderr);
  assert.equal(fixture.calls().filter((call) => call.includes("\ttest ")).length, 1);
});

test("unknown or repeated partition arguments fail before execution", (t) => {
  for (const args of [["--unknown"], ["--exclude-root-cli", "--exclude-root-cli"]]) {
    const fixture = moduleFixture(t);
    const result = fixture.run(0, args);
    assert.equal(result.status, 2, result.stdout + result.stderr);
    assert.deepEqual(fixture.calls(), []);
  }
});
