#!/usr/bin/env node
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import path from "node:path";
import test from "node:test";

const script = path.join(import.meta.dirname, "live-cua-smoke.sh");

test("CUA live smoke skips without explicit live opt-in", () => {
  const result = spawnSync("bash", [script], {
    env: {
      ...process.env,
      CRABBOX_CUA_LIVE: "",
      CRABBOX_CUA_API_KEY: "",
      CUA_API_KEY: "",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0);
  assert.match(result.stdout, /^skipped reason=missing_CRABBOX_CUA_LIVE\n$/);
});

test("CUA live smoke classifies opt-in without credentials as environment_blocked", () => {
  const result = spawnSync("bash", [script], {
    env: {
      ...process.env,
      CRABBOX_CUA_LIVE: "1",
      CRABBOX_CUA_API_KEY: "",
      CUA_API_KEY: "",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0);
  assert.match(result.stdout, /^environment_blocked reason=missing_CRABBOX_CUA_API_KEY_or_CUA_API_KEY\n$/);
});
