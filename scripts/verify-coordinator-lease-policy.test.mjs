import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import test from "node:test";

import { verifyCoordinatorLeasePolicy } from "./verify-coordinator-lease-policy.mjs";

const bindingName = "CRABBOX_LEASE_POLICY_OVERRIDES";
const desired = { orgs: { "example-org": { maxTTLSeconds: 7200 } } };
const config = { accountId: "a".repeat(32), apiToken: "synthetic-api-token" };
const plaintext = (value) => ({
  name: bindingName,
  type: "plain_text",
  text: JSON.stringify(value),
});
const settings = (...bindings) => ({ success: true, result: { bindings } });

function fixture(reply, desiredPolicy) {
  const calls = [];
  return {
    calls,
    run: (overrides = {}) =>
      verifyCoordinatorLeasePolicy({
        ...config,
        desiredOverrides: JSON.stringify(desiredPolicy),
        ...overrides,
        fetchImpl: async (url, options) => {
          calls.push({ url, options });
          if (reply instanceof Error) throw reply;
          return reply instanceof Response ? reply : Response.json(reply);
        },
      }),
  };
}

test("reads only exact Worker settings without forwarding credentials", async () => {
  const f = fixture(settings({ name: "OTHER_SECRET", type: "secret_text" }));
  await f.run();
  assert.equal(f.calls.length, 1);
  const [{ url, options }] = f.calls;
  assert.equal(
    url,
    `https://api.cloudflare.com/client/v4/accounts/${config.accountId}/workers/scripts/crabbox-coordinator/settings`,
  );
  assert.equal(options.method ?? "GET", "GET");
  assert.equal(options.redirect, "error");
  assert.equal(options.cache, "no-store");
  assert.ok(options.signal instanceof AbortSignal);
  assert.deepEqual(options.headers, { authorization: `Bearer ${config.apiToken}` });
});

for (const [label, live, source] of [
  ["absent documents", settings(), undefined],
  ["absent live and empty source", settings(), {}],
  ["empty live and absent source", settings(plaintext({})), undefined],
  ["empty live groups", settings(plaintext({ orgs: {}, owners: {} })), undefined],
  ["empty source groups", settings(), { orgs: {}, owners: {} }],
  ["equal policies", settings(plaintext(desired)), desired],
  [
    "equal policies regardless of key order",
    settings(
      plaintext({
        orgs: desired.orgs,
        owners: { "github:12345": { extension: "deny", maxTTLSeconds: 7200 } },
      }),
    ),
    { owners: { "github:12345": { maxTTLSeconds: 7200, extension: "deny" } }, orgs: desired.orgs },
  ],
]) {
  test(`permits ${label}`, async () => {
    assert.equal(await fixture(live, source).run(), undefined);
  });
}

for (const [label, live, source, summary] of [
  ["removed org", desired, undefined, '"orgs"["example-org"]'],
  ["added org", undefined, desired, '"orgs"["example-org"]'],
  [
    "changed policy",
    desired,
    { orgs: { "example-org": { maxTTLSeconds: 14400 } } },
    '"orgs"["example-org"]',
  ],
  [
    "removed owner",
    { owners: { "github:12345": { maxTTLSeconds: 7200 } } },
    undefined,
    '"owners"["github:12345"]',
  ],
  ["empty owner entry", { owners: { "github:12345": {} } }, undefined, '"owners"["github:12345"]'],
  ["unknown field", { additional: "synthetic-policy-value" }, undefined, '"additional"'],
  ["unknown empty object", { additional: {} }, undefined, '"additional"'],
  ["prototype key", JSON.parse('{"owners":{"__proto__":{}}}'), undefined, '"owners"["__proto__"]'],
  [
    "multiple groups",
    { owners: { "github:12345": { extension: "synthetic-policy-value" } }, orgs: desired.orgs },
    undefined,
    '"orgs"["example-org"], "owners"["github:12345"]',
  ],
]) {
  test(`requires acknowledgment and reports only keys: ${label}`, async () => {
    const f = fixture(live === undefined ? settings() : settings(plaintext(live)), source);
    let failure;
    await assert.rejects(f.run(), (error) => {
      failure = error.message;
      assert.ok(failure.includes(`Live and source lease policy differ at: ${summary}.`));
      assert.match(failure, /CRABBOX_ACKNOWLEDGE_LEASE_POLICY_REPLACEMENT=1/);
      assert.doesNotMatch(failure, /synthetic-|7200|14400/);
      return true;
    });
    const acknowledged = await f.run({ acknowledgeReplacement: true });
    assert.equal(acknowledged, `Live and source lease policy differ at: ${summary}.`);
    assert.ok(failure.startsWith(acknowledged));
  });
}

for (const [label, live, message] of [
  [
    "secret-valued override",
    settings({ name: bindingName, type: "secret_text" }),
    /not one readable plaintext/,
  ],
  [
    "missing plaintext",
    settings({ name: bindingName, type: "plain_text" }),
    /not one readable plaintext/,
  ],
  [
    "duplicate binding",
    settings(plaintext(desired), plaintext(desired)),
    /not one readable plaintext/,
  ],
  [
    "invalid override JSON",
    settings({ name: bindingName, type: "plain_text", text: "synthetic-private-invalid" }),
    /not readable JSON/,
  ],
  [
    "empty override text",
    settings({ name: bindingName, type: "plain_text", text: "" }),
    /not readable JSON/,
  ],
  ["null document", settings(plaintext(null)), /invalid document shape/],
  ["array document", settings(plaintext([])), /invalid document shape/],
  ["array group", settings(plaintext({ owners: [] })), /invalid document shape/],
  ["null group", settings(plaintext({ orgs: null })), /invalid document shape/],
  [
    "invalid entry",
    settings(plaintext({ owners: { "github:12345": 123 } })),
    /invalid document shape/,
  ],
  ["missing bindings", { success: true, result: {} }, /did not confirm/],
  ["API rejection", { success: false, errors: [{ message: config.apiToken }] }, /did not confirm/],
  ["HTTP failure", new Response(config.apiToken, { status: 403 }), /HTTP 403/],
  ["non-JSON reply", new Response(config.apiToken), /returned invalid JSON/],
  ["transport failure", new Error(config.apiToken), /request failed/],
]) {
  for (const acknowledgeReplacement of [false, true]) {
    test(`fails closed without disclosing values (${acknowledgeReplacement}): ${label}`, async () => {
      await assert.rejects(fixture(live).run({ acknowledgeReplacement }), (error) => {
        assert.match(error.message, message);
        assert.doesNotMatch(error.message, /synthetic-/);
        return true;
      });
    });
  }
}

for (const overrides of [
  { accountId: "../synthetic-value" },
  { apiToken: "" },
  { desiredOverrides: "synthetic-invalid" },
  { desiredOverrides: "null" },
  { desiredOverrides: JSON.stringify({ orgs: [] }) },
]) {
  test(`rejects invalid source or authority before reading settings: ${JSON.stringify(overrides)}`, async () => {
    const f = fixture(settings());
    await assert.rejects(f.run({ ...overrides, acknowledgeReplacement: true }));
    assert.equal(f.calls.length, 0);
  });
}

test("CLI rejects arguments before reading settings", () => {
  const result = spawnSync(
    process.execPath,
    [new URL("./verify-coordinator-lease-policy.mjs", import.meta.url).pathname, "unexpected"],
    { encoding: "utf8" },
  );
  assert.equal(result.status, 1);
  assert.match(result.stderr, /takes no arguments/);
});
