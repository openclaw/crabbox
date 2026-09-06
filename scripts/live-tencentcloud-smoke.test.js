import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { copySmokeRepo, writeExecutable } from "./test-support/smoke-fixtures.mjs";

function runSmoke(t, overrides = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox tencent smoke-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const { smokeScript } = copySmokeRepo(
    dir,
    path.join(import.meta.dirname, "live-tencentcloud-smoke.sh"),
    ["lib/live-smoke-common.sh"],
  );
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);
  const crabbox = path.join(bin, "crabbox");
  const calls = path.join(dir, "calls.log");
  writeExecutable(
    path.join(bin, "sleep"),
    '#!/usr/bin/env bash\nprintf "%s\\n" "$*" >>"$SMOKE_DIR/sleeps"\n',
  );
  writeExecutable(
    crabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"$SMOKE_DIR/calls.log"
if [[ "$1" == warmup ]]; then
  printf '%s' "$5" >"$SMOKE_DIR/slug"
fi
if [[ "$1" == "\${TEST_FAIL_COMMAND:-}" ]]; then
  printf '%s\\n' "\${TEST_FAILURE_OUTPUT:-command failed}" >&2
  exit 37
fi
case "$1" in
  config) printf '{"tencentcloud":{"image":"image-fixture"}}\\n' ;;
  doctor|status) printf 'ready\\n' ;;
  warmup) printf 'created\\n' ;;
  run) printf '%s\\n' "\${TEST_RUN_OUTPUT:-ok}" ;;
  list)
    if [[ ! -f "$SMOKE_DIR/slug" ]]; then
      printf '%s\\n' "\${TEST_INITIAL_LIST:-[]}"
    elif [[ ! -f "$SMOKE_DIR/stopped" ]]; then
      printf '{"nested":[{"labels":{"slug":"%s"}}]}\\n' "$(cat "$SMOKE_DIR/slug")"
    elif [[ "\${TEST_SETTLE_ONCE:-}" == 1 && ! -f "$SMOKE_DIR/settled" ]]; then
      touch "$SMOKE_DIR/settled"
      printf '[{"name":"still-visible"}]\\n'
    else
      printf '[]\\n'
    fi
    ;;
  stop) touch "$SMOKE_DIR/stopped"; printf 'stopped\\n' ;;
  cleanup) printf 'dry-run\\n' ;;
  *) exit 99 ;;
esac
`,
  );
  const result = spawnSync("bash", [smokeScript], {
    cwd: dir,
    env: {
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      HOME: dir,
      CRABBOX_BIN: crabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "tencentcloud",
      CRABBOX_LIVE_TENCENTCLOUD_IMAGE: "image-fixture",
      TENCENTCLOUD_SECRET_ID: "test-id",
      TENCENTCLOUD_SECRET_KEY: "test-key",
      SMOKE_DIR: dir,
      ...overrides,
    },
    encoding: "utf8",
  });
  return {
    ...result,
    calls: fs.existsSync(calls) ? fs.readFileSync(calls, "utf8").trim().split("\n") : [],
    slug: fs.existsSync(path.join(dir, "slug"))
      ? fs.readFileSync(path.join(dir, "slug"), "utf8")
      : "",
    sleeps: fs.existsSync(path.join(dir, "sleeps"))
      ? fs.readFileSync(path.join(dir, "sleeps"), "utf8")
      : "",
  };
}

test("Tencent smoke gates opt-in, provider selection, and credentials before invoking the CLI", (t) => {
  for (const [env, reason] of [
    [{ CRABBOX_LIVE: "" }, "CRABBOX_LIVE_not_enabled"],
    [{ CRABBOX_LIVE_PROVIDERS: "aws" }, "tencentcloud_not_selected"],
    [{ TENCENTCLOUD_SECRET_ID: "" }, "TENCENTCLOUD_SECRET_ID_or_SECRET_KEY_missing"],
    [{ TENCENTCLOUD_SECRET_KEY: "" }, "TENCENTCLOUD_SECRET_ID_or_SECRET_KEY_missing"],
  ]) {
    const result = runSmoke(t, env);
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, new RegExp(`reason=${reason}`));
    assert.deepEqual(result.calls, []);
  }
});

test("Tencent smoke preserves guarded lifecycle order and waits for empty inventory", (t) => {
  const result = runSmoke(t, { TEST_SETTLE_ONCE: "1" });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_tencentcloud_smoke_passed/);
  assert.match(result.slug, /^tencentcloud-smoke-\d{14}-\d+$/);
  assert.deepEqual(result.calls, [
    "doctor --provider tencentcloud",
    "list --provider tencentcloud --json",
    `warmup --provider tencentcloud --slug ${result.slug} --keep --tencentcloud-image image-fixture --tencentcloud-type SA5.MEDIUM2 --ttl 20m --idle-timeout 5m`,
    `status --provider tencentcloud --id ${result.slug} --wait --wait-timeout 300s`,
    `run --provider tencentcloud --id ${result.slug} --no-sync -- echo ok`,
    "list --provider tencentcloud --json",
    `stop --provider tencentcloud ${result.slug}`,
    "list --provider tencentcloud --json",
    "list --provider tencentcloud --json",
    "cleanup --provider tencentcloud --dry-run",
    "list --provider tencentcloud --json",
  ]);
  assert.equal(result.sleeps, "10\n");
});

test("Tencent smoke retains provider aliases and config image lookup", (t) => {
  for (const provider of ["tencent", "tencent-cvm", "cvm"]) {
    const result = runSmoke(t, {
      CRABBOX_LIVE_PROVIDERS: `aws, ${provider}`,
      CRABBOX_LIVE_TENCENTCLOUD_IMAGE: "",
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.equal(result.calls[0], "config show --provider tencentcloud --json");
    assert.match(result.stdout, /image=image-fixture/);
  }
});

test("Tencent smoke rejects malformed or non-empty initial inventory before mutation", (t) => {
  for (const payload of ['{"broken":', "{}", '[{"name":"existing"}]']) {
    const result = runSmoke(t, { TEST_INITIAL_LIST: payload });
    assert.equal(result.status, 1, result.stderr);
    assert.match(result.stderr, /classification=validation_failed/);
    assert.match(result.stderr, /invalid JSON:|Tencent Cloud Crabbox inventory is not empty/);
    assert.deepEqual(result.calls, [
      "doctor --provider tencentcloud",
      "list --provider tencentcloud --json",
    ]);
  }
});

test("Tencent smoke preserves failed command status and performs only targeted cleanup", (t) => {
  for (const [command, output, classification] of [
    ["warmup", "quota exhausted", "quota_blocked"],
    ["status", "connection failed", "environment_blocked"],
    ["run", "command failed", "environment_blocked"],
  ]) {
    const result = runSmoke(t, { TEST_FAIL_COMMAND: command, TEST_FAILURE_OUTPUT: output });
    assert.equal(result.status, 37, result.stderr);
    assert.match(result.stderr, new RegExp(`classification=${classification} .*exit=37`));
    assert.match(result.stderr, new RegExp(output));
    assert.equal(result.calls.at(-1), `stop --provider tencentcloud ${result.slug}`);
    assert.equal(result.calls.filter((call) => call.startsWith("stop ")).length, 1);
    assert.ok(!result.calls.some((call) => call.startsWith("cleanup ")));
  }
});

test("Tencent smoke classifies invalid run output and cleans up the current slug", (t) => {
  const result = runSmoke(t, { TEST_RUN_OUTPUT: "unexpected result" });
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /unexpected result/);
  assert.equal(result.calls.at(-1), `stop --provider tencentcloud ${result.slug}`);
  assert.ok(!result.calls.some((call) => call.startsWith("cleanup ")));
});
