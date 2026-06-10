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

test("go module discovery failure aborts before reporting success", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-test-go-modules-"));
  const bin = path.join(dir, "bin");
  const goLog = path.join(dir, "go.log");
  fs.mkdirSync(bin);

  writeExecutable(
    path.join(bin, "find"),
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\0' "$1/go.mod"
printf 'find: partial traversal failure\\n' >&2
exit 7
`,
  );
  writeExecutable(
    path.join(bin, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${goLog}"
exit 0
`,
  );

  const result = spawnSync("bash", ["scripts/test-go-modules.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
    },
    encoding: "utf8",
  });

  assert.notEqual(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /failed to discover go\.mod files/);
  assert.equal(fs.existsSync(goLog), false, "go test should not run after incomplete discovery");
});
