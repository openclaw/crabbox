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

test("live Crownest smoke drives doctor, pnpm run, keep, status, reuse, and stop", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-crownest-smoke-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const calls = path.join(dir, "calls.log");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    printf 'ok      provider provider=crownest auth=ready api=ready\\n'
    ;;
  run)
    if [[ "$*" == *"--lease-output"* ]]; then
      while [[ "$#" -gt 0 ]]; do
        if [[ "$1" == "--lease-output" ]]; then
          printf '{"slug":"smoke-slug","leaseId":"cnsbx_sbx_test"}\\n' >"$2"
          break
        fi
        shift
      done
      printf 'keep-ok'
    elif [[ "$*" == *"--id smoke-slug"* ]]; then
      printf 'reuse-ok\n'
    else
      printf 'hello-from-crabbox-crownest\\n'
    fi
    ;;
  status|stop)
    ;;
  *)
    printf 'unexpected args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [path.join(repoRoot, "scripts/live-crownest-smoke.sh")], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CROWNEST_API_KEY: "cn_test_key",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_crownest_smoke_passed/);
  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen.length, 6, JSON.stringify(seen));
  assert.equal(seen[0], "doctor --provider crownest");
  assert.equal(
    seen[1],
    "run --provider crownest --crownest-template python-node --crownest-timeout-secs 120 -- pnpm test",
  );
  assert.match(
    seen[2],
    /^run --provider crownest --crownest-template python-node --crownest-timeout-secs 120 --keep --lease-output .+ -- sh -lc printf keep-ok$/,
  );
  assert.equal(
    seen[3],
    "status --provider crownest --crownest-template python-node --id smoke-slug",
  );
  assert.equal(
    seen[4],
    'run --provider crownest --crownest-template python-node --id smoke-slug --crownest-timeout-secs 120 -- sh -lc printf "reuse-ok\\n"',
  );
  assert.equal(seen[5], "stop --provider crownest --crownest-template python-node smoke-slug");
});
