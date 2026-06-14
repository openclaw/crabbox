const assert = require("node:assert/strict");
const path = require("node:path");
const { spawnSync } = require("node:child_process");
const test = require("node:test");

const { classify } = require("./live-codesandbox-smoke.test.js");

test("CodeSandbox smoke treats explicit environment blockers as non-diagnostic", () => {
  for (const message of [
    "API key missing",
    "quota exceeded",
    "429 too many requests",
    "capacity unavailable",
    "request timeout",
    "ECONNRESET",
  ]) {
    assert.equal(classify(message), "environment_blocked", message);
  }
});

test("CodeSandbox smoke does not hide SDK contract failures", () => {
  assert.equal(classify("codesandbox bridge sdk_error: vmTier was not a VMTier object"), "diagnostic_only");
  assert.equal(classify("codesandbox bridge sdk_error: commands.run is not a function"), "diagnostic_only");
  assert.equal(classify("diagnostic_only: preview failed: HTTP 401 Unauthorized"), "diagnostic_only");
});

test("CodeSandbox smoke classifies early command failures without masking them", () => {
  const script = path.join(__dirname, "live-codesandbox-smoke.test.js");
  const result = spawnSync(process.execPath, [script], {
    env: {
      ...process.env,
      CRABBOX_BIN: process.execPath,
      CRABBOX_CODESANDBOX_API_KEY: "test-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 1);
  assert.match(result.stdout, /classification=diagnostic_only/);
  assert.doesNotMatch(result.stderr, /ReferenceError/);
});
