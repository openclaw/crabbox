import assert from "node:assert/strict";
import test from "node:test";

import relay from "./image-qualification-relay-worker.mjs";

function fixture() {
  const calls = [];
  const receipt = { receipt: { signature: "fixture-signature", run_id: "run_fixture" } };
  const env = {
    EXECUTOR_TOKEN: "e".repeat(64),
    CANDIDATE_ADMIN_TOKEN: "a".repeat(64),
    CANDIDATE_SHARED_TOKEN: "s".repeat(64),
    QUALIFICATION_MODE: "first-walk",
    QUALIFICATION_OWNER: "fixture@example.invalid",
    QUALIFICATION_ORG: "example-org",
    QUALIFICATION_EXPIRES_AT: new Date(Date.now() + 60_000).toISOString(),
    CANDIDATE: {
      async fetch(request) {
        calls.push(request);
        return Response.json(receipt);
      },
    },
  };
  const request = (suffix = "/v1/runs/run_fixture/receipt", options = {}) =>
    new Request(`https://relay.invalid${suffix}`, {
      headers: { authorization: `Bearer ${env.EXECUTOR_TOKEN}` },
      ...options,
    });
  return { calls, receipt, env, request };
}

test("first-walk forwards the terminal receipt read without changing its signed payload", async () => {
  const f = fixture();
  const response = await relay.fetch(f.request(), f.env);
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), f.receipt);
  assert.equal(f.calls.length, 1);
  const forwarded = f.calls[0];
  assert.equal(forwarded.url, "https://candidate.invalid/v1/runs/run_fixture/receipt");
  assert.equal(forwarded.method, "GET");
  assert.equal(forwarded.body, null);
  assert.equal(forwarded.headers.get("authorization"), `Bearer ${f.env.CANDIDATE_ADMIN_TOKEN}`);
  assert.equal(forwarded.headers.get("x-crabbox-owner"), f.env.QUALIFICATION_OWNER);
  assert.equal(forwarded.headers.get("x-crabbox-org"), f.env.QUALIFICATION_ORG);
});

test("receipt reads remain first-walk-only and have no shared-principal route", async () => {
  const f = fixture();
  for (const mode of [undefined, "mint", "retained"])
    assert.equal(
      (await relay.fetch(f.request(), { ...f.env, QUALIFICATION_MODE: mode })).status,
      404,
    );
  assert.equal(
    (await relay.fetch(f.request("/qualification/shared/v1/runs/run_fixture/receipt"), f.env))
      .status,
    404,
  );
  assert.equal(f.calls.length, 0);
});

test("receipt admission preserves the method, canonical path, query and header restrictions", async () => {
  const f = fixture();
  for (const suffix of [
    "/v1/runs/run_fixture/receipt?other=1",
    "/v1/runs/run_fixture/receipt/extra",
    "/v1/runs/run_fixture%2freceipt",
    `/v1/runs/${"r".repeat(129)}/receipt`,
  ])
    assert.equal((await relay.fetch(f.request(suffix), f.env)).status, 404);
  assert.equal((await relay.fetch(f.request(undefined, { method: "POST" }), f.env)).status, 404);
  assert.equal(
    (
      await relay.fetch(
        f.request(undefined, {
          headers: {
            authorization: `Bearer ${f.env.EXECUTOR_TOKEN}`,
            "x-http-method-override": "DELETE",
          },
        }),
        f.env,
      )
    ).status,
    404,
  );
  assert.equal(f.calls.length, 0);
});

test("receipt admission retains authentication, expiry and bounded candidate responses", async () => {
  const f = fixture();
  assert.equal(
    (await relay.fetch(f.request(undefined, { headers: { authorization: "Bearer wrong" } }), f.env))
      .status,
    403,
  );
  assert.equal(
    (
      await relay.fetch(f.request(), {
        ...f.env,
        QUALIFICATION_EXPIRES_AT: new Date(0).toISOString(),
      })
    ).status,
    403,
  );
  assert.equal(f.calls.length, 0);
  for (const response of [
    () => new Response("not JSON"),
    () => Response.json({ receipt: "x".repeat(64 * 1024) }),
  ])
    assert.equal(
      (
        await relay.fetch(f.request(), {
          ...f.env,
          CANDIDATE: { fetch: async () => response() },
        })
      ).status,
      502,
    );
});
