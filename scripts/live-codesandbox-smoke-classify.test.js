const assert = require("node:assert/strict");
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
});
