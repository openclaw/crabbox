import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { verifyCoordinatorDeployment } from "./verify-coordinator-deployment.mjs";

const sha = "a".repeat(40);
const config = { accountId: "b".repeat(32), apiToken: "synthetic-api-token", sha };
const versionId = "11111111-1111-4111-8111-111111111111";
const deployment = {
  id: "22222222-2222-4222-8222-222222222222",
  strategy: "percentage",
  versions: [{ version_id: versionId, percentage: 100 }],
};
const api = (latest = deployment) => ({ success: true, result: { deployments: [latest] } });
const health = { ok: true, service: "crabbox-coordinator", version: { id: versionId, tag: sha } };

function fixture(replies = [api(), health, api()]) {
  const calls = [];
  return {
    calls,
    run: (overrides = {}) =>
      verifyCoordinatorDeployment({
        ...config,
        ...overrides,
        fetchImpl: async (url, options) => {
          calls.push({ url, options });
          assert.ok(calls.length <= replies.length, "unexpected extra request");
          const reply = replies[calls.length - 1];
          if (reply instanceof Error) throw reply;
          return reply instanceof Response ? reply : Response.json(reply);
        },
      }),
  };
}

test("confirms public health matches the sole active version across the check", async () => {
  const f = fixture();
  assert.deepEqual(await f.run(), { sha, deploymentId: deployment.id, versionId });
  assert.equal(f.calls.length, 3);
  assert.equal(f.calls[0].url, f.calls[2].url);
  assert.equal(new URL(f.calls[0].url).origin, "https://api.cloudflare.com");
  assert.equal(new URL(f.calls[1].url).origin, "https://crabbox.openclaw.ai");
  assert.equal(new URL(f.calls[1].url).searchParams.get("deployment"), sha);
  for (const [index, { options }] of f.calls.entries()) {
    assert.equal(options.method ?? "GET", "GET");
    assert.equal(options.redirect, "error");
    assert.equal(options.cache, "no-store");
    assert.ok(options.signal instanceof AbortSignal);
    assert.deepEqual(
      options.headers,
      index === 1 ? {} : { authorization: `Bearer ${config.apiToken}` },
    );
  }
});

for (const [label, override] of [
  ["missing token", { apiToken: "" }],
  ["missing account", { accountId: undefined }],
  ["invalid account path", { accountId: "../synthetic-api-token" }],
  ["abbreviated SHA", { sha: "a".repeat(7) }],
  ["missing SHA", { sha: undefined }],
]) {
  test(`rejects ${label} before making any request`, async () => {
    const f = fixture();
    await assert.rejects(f.run(override), /before verification|full commit SHA/);
    assert.equal(f.calls.length, 0);
  });
}

for (const [label, data] of [
  ["Cloudflare API rejection", { success: false, errors: [{ message: config.apiToken }] }],
  ["empty deployment history", { success: true, result: { deployments: [] } }],
  ["missing result", { success: true }],
  ["missing deployment identity", api({ ...deployment, id: "" })],
  ["missing version identity", api({ ...deployment, versions: [{ percentage: 100 }] })],
  [
    "partial deployment",
    api({ ...deployment, versions: [{ version_id: versionId, percentage: 50 }] }),
  ],
  [
    "gradual rollout",
    api({
      ...deployment,
      versions: [
        { version_id: versionId, percentage: 90 },
        { version_id: "previous-version", percentage: 10 },
      ],
    }),
  ],
  ["malformed versions", api({ ...deployment, versions: {} })],
]) {
  test(`fails visibly on ${label}`, async () => {
    const f = fixture([data]);
    await assert.rejects(f.run(), /Expected one active coordinator version at 100%/);
    assert.equal(f.calls.length, 1);
  });
}

test("uses the latest deployment rather than a matching historical deployment", async () => {
  const f = fixture([
    {
      success: true,
      result: { deployments: [{ ...deployment, versions: [] }, deployment] },
    },
  ]);
  await assert.rejects(f.run(), /Expected one active coordinator version/);
});

for (const [label, body] of [
  ["stale commit", { ...health, version: { ...health.version, tag: "c".repeat(40) } }],
  ["wrong serving version", { ...health, version: { ...health.version, id: "previous-version" } }],
  ["untagged version after secret change", { ...health, version: { id: versionId } }],
  ["missing metadata", { ok: true, service: health.service }],
  ["unhealthy coordinator", { ...health, ok: false }],
  ["wrong service", { ...health, service: "another-worker" }],
  ["null health", null],
]) {
  test(`rejects ${label}`, async () => {
    const f = fixture([api(), body]);
    await assert.rejects(f.run(), /health did not confirm the active version and requested commit/);
    assert.equal(f.calls.length, 2);
  });
}

for (const [label, latest] of [
  ["deployment changed", { ...deployment, id: "replacement-deployment" }],
  [
    "version changed",
    { ...deployment, versions: [{ version_id: "replacement-version", percentage: 100 }] },
  ],
]) {
  test(`rejects a concurrent rollout when ${label}`, async () => {
    await assert.rejects(
      fixture([api(), health, api(latest)]).run(),
      /changed during verification/,
    );
  });
}

for (const index of [0, 1, 2]) {
  for (const [label, response, expected] of [
    ["HTTP failure", new Response(config.apiToken, { status: 403 }), /request failed \(HTTP 403\)/],
    ["redirect", new Error(`redirect: ${config.apiToken}`), /request failed; check connectivity/],
    [
      "network failure",
      new Error(`network: ${config.apiToken}`),
      /request failed; check connectivity/,
    ],
    ["invalid JSON", new Response(config.apiToken), /returned invalid JSON/],
  ]) {
    test(`sanitizes ${label} at request ${index + 1}`, async () => {
      const replies = [api(), health, api()];
      replies[index] = response;
      await assert.rejects(fixture(replies).run(), (error) => {
        assert.match(error.message, expected);
        assert.doesNotMatch(error.message, /synthetic-api-token/);
        return true;
      });
    });
  }
}

test("CLI fails without credentials instead of reporting success", () => {
  const result = spawnSync(
    process.execPath,
    [new URL("./verify-coordinator-deployment.mjs", import.meta.url).pathname],
    { env: {}, encoding: "utf8" },
  );
  assert.ifError(result.error);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, "");
  assert.match(result.stderr, /::error::Set CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN/);
});
