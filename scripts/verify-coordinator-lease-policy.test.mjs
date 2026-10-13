import assert from "node:assert/strict";
import test from "node:test";

import { verifyCoordinatorLeasePolicy } from "./verify-coordinator-lease-policy.mjs";

const owner = "github:12345";
const bindingName = "CRABBOX_LEASE_POLICY_OVERRIDES";
const desired = { owners: { [owner]: { maxTTLSeconds: 86400 } } };
const config = { accountId: "a".repeat(32), apiToken: "synthetic-api-token", owner };
const plaintext = (value) => ({
  name: bindingName,
  type: "plain_text",
  text: JSON.stringify(value),
});
const settings = (...bindings) => ({ success: true, result: { bindings } });

function fixture(reply, desiredPolicy = desired) {
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
  ["absent override", settings(), desired],
  ["empty document", settings(plaintext({})), desired],
  ["empty maps", settings(plaintext({ orgs: {}, owners: {} })), desired],
  [
    "existing owner maximum",
    settings(plaintext({ owners: { [owner]: { maxTTLSeconds: 14400 } } })),
    desired,
  ],
  ["already desired", settings(plaintext(desired)), desired],
  [
    "preserved unrelated policies regardless of key order",
    settings(
      plaintext({
        orgs: { example: { extension: "deny" } },
        owners: {
          [owner]: { autoExtend: "deny", maxTTLSeconds: 14400 },
          other: { maxTTLSeconds: 7200 },
        },
      }),
    ),
    {
      owners: {
        other: { maxTTLSeconds: 7200 },
        [owner]: { maxTTLSeconds: 86400, autoExtend: "deny" },
      },
      orgs: { example: { extension: "deny" } },
    },
  ],
]) {
  test(`permits ${label}`, async () => {
    await fixture(live, source).run();
  });
}

for (const [label, live, message] of [
  [
    "unknown owner",
    settings(plaintext({ owners: { other: { maxTTLSeconds: 7200 } } })),
    /not preserved/,
  ],
  [
    "unknown organization",
    settings(plaintext({ orgs: { example: { maxTTLSeconds: 7200 } } })),
    /not preserved/,
  ],
  [
    "other target-owner field",
    settings(plaintext({ owners: { [owner]: { extension: "deny" } } })),
    /not preserved/,
  ],
  [
    "unknown top-level field",
    settings(plaintext({ additional: { private: "synthetic-policy-value" } })),
    /not preserved/,
  ],
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
  ["invalid override shape", settings(plaintext({ owners: [] })), /invalid document shape/],
  ["missing bindings", { success: true, result: {} }, /did not confirm/],
  ["API rejection", { success: false, errors: [{ message: config.apiToken }] }, /did not confirm/],
  ["HTTP failure", new Response(config.apiToken, { status: 403 }), /HTTP 403/],
  ["non-JSON reply", new Response(config.apiToken), /returned invalid JSON/],
  ["transport failure", new Error(config.apiToken), /request failed/],
]) {
  test(`stops deployment without disclosing values: ${label}`, async () => {
    await assert.rejects(fixture(live).run(), (error) => {
      assert.match(error.message, message);
      assert.doesNotMatch(error.message, /synthetic-/);
      return true;
    });
  });
}

test("rejects changed non-target fields even when both documents declare them", async () => {
  const live = { owners: { [owner]: { maxTTLSeconds: 14400, autoExtend: "deny" } } };
  const source = { owners: { [owner]: { maxTTLSeconds: 86400, autoExtend: "allow" } } };
  await assert.rejects(fixture(settings(plaintext(live)), source).run(), /not preserved/);
});

test("rejects invalid source or authority before reading settings", async () => {
  await Promise.all(
    [
      { accountId: "../synthetic-value" },
      { apiToken: "" },
      { owner: "" },
      { desiredOverrides: "synthetic-invalid" },
      { desiredOverrides: JSON.stringify({ owners: { [owner]: {} } }) },
    ].map(async (overrides) => {
      const f = fixture(settings());
      await assert.rejects(f.run(overrides));
      assert.equal(f.calls.length, 0);
    }),
  );
});
