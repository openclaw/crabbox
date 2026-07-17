#!/usr/bin/env node
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import path from "node:path";
import test from "node:test";

const script = path.join(import.meta.dirname, "live-cua-smoke.sh");

test("CUA smoke is syntax-valid and diagnostic-only", () => {
  const result = spawnSync("bash", ["-n", script], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);

  const source = readFileSync(script, "utf8");
  assert.doesNotMatch(source, /CRABBOX_CUA_LIVE/);
  assert.match(source, /provisioning is disabled/);
  assert.match(source, /diagnostic_only mode=experimental_non_provisioning/);
  assert.match(source, /invalid_or_unclassified_doctor_result/);
  assert.match(source, /JSON\.parse/);
  assert.match(source, /item\?\.details\?\.provider === "cua"/);
  assert.doesNotMatch(source, /\b(run|stop|cleanup) --provider cua\b/);
});
