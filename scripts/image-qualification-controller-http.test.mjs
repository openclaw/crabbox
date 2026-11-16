import assert from "node:assert/strict";
import test from "node:test";
import fs from "node:fs";
import { createHash } from "node:crypto";
import { idleSourceIdentity } from "./image-qualification-idle-controller.mjs";
import { replacementSources } from "./image-qualification-session-replacement.mjs";
import {
  ControllerHTTPError,
  controllerResponseMaxBytes,
  requestControllerJSON,
} from "./image-qualification-controller-http.mjs";

const options = {
  url: "https://controller.invalid",
  token: "synthetic-controller-secret",
  action: "transport-open",
};
const marker = "synthetic-private-response-secret";
async function failed(invoke, extra = {}) {
  let calls = 0;
  let error;
  await assert.rejects(
    requestControllerJSON({
      ...options,
      ...extra,
      invoke: async (...args) => {
        calls++;
        return invoke(...args);
      },
    }),
    (value) => {
      error = value;
      return value instanceof ControllerHTTPError;
    },
  );
  assert.equal(calls, extra.expectedCalls ?? 1);
  assert(!JSON.stringify(error).includes(marker));
  assert(!error.message.includes(marker));
  assert(!error.message.includes(options.token));
  assert(!error.message.includes(options.url));
  assert.equal(error.cause, undefined);
  assert(Object.isFrozen(error.receipt));
  assert(Number.isSafeInteger(error.receipt.elapsedMs));
  return error.receipt;
}

for (const status of [200, 503]) {
  test(`HTML ${status} retains status and safe metadata without reflecting response bytes`, async () => {
    const r = await failed(
      async () =>
        new Response(`<!DOCTYPE html><p>${marker}</p>`, {
          status,
          headers: {
            "content-type": "text/html; charset=utf-8",
            "cf-ray": "0123456789abcdef-DFW",
            location: `https://private.invalid/${marker}`,
          },
        }),
    );
    assert.equal(r.status, status);
    assert.equal(r.reason, status === 200 ? "invalid-json" : "http-error");
    assert.equal(r.contentType, "html");
    assert.equal(r.bodyFormat, "html");
    assert.equal(r.ray, "0123456789abcdef-DFW");
    assert.equal(r.outcome, "unconfirmed");
  });
}

test("JSON errors and arbitrary headers cannot become public diagnostics", async () => {
  const r = await failed(async () =>
    Response.json(
      { message: marker, error: marker, token: options.token },
      {
        status: 403,
        headers: { "content-type": `application/${marker}`, "cf-ray": marker },
      },
    ),
  );
  assert.equal(r.status, 403);
  assert.equal(r.reason, "http-error");
  assert.equal(r.bodyFormat, "json");
  assert.equal(r.contentType, "other");
  assert(!Object.hasOwn(r, "ray"));
});

for (const body of ["{", `{"value":"${marker}",`, "null", "[]", "42"]) {
  test(`malformed or non-object JSON fails closed (${body.length} bytes)`, async () => {
    const r = await failed(async () => new Response(body));
    assert.match(r.reason, /^(invalid-json|invalid-json-shape)$/);
  });
}

test("valid JSON objects preserve existing replies regardless of declared content type", async () => {
  for (const headers of [{}, { "content-type": "application/json" }]) {
    let calls = 0;
    const data = { protocol: 1, state: "pending", nested: { preserved: true } };
    const actual = await requestControllerJSON({
      ...options,
      invoke: async (url, init) => {
        calls++;
        assert.equal(url, options.url + "/transport-open");
        assert.equal(init.method, "POST");
        assert.equal(init.redirect, "error");
        assert.equal(init.headers.authorization, `Bearer ${options.token}`);
        assert(init.signal instanceof AbortSignal);
        return new Response(JSON.stringify(data), { headers });
      },
    });
    assert.deepEqual(actual, data);
    assert.equal(calls, 1);
  }
});

test("oversize failure joins the original asynchronous body cancellation", async () => {
  let release, entered;
  const cancelled = new Promise((resolve) => {
    entered = resolve;
  });
  const settled = new Promise((resolve) => {
    release = resolve;
  });
  let cancellations = 0,
    returned = false;
  const attempt = failed(
    async () =>
      new Response(
        new ReadableStream({
          start(c) {
            c.enqueue(new Uint8Array(controllerResponseMaxBytes + 1));
          },
          cancel() {
            cancellations++;
            entered();
            return settled;
          },
        }),
      ),
  ).then((r) => {
    returned = true;
    return r;
  });
  await cancelled;
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(returned, false);
  assert.equal(cancellations, 1);
  release();
  const receipt = await attempt;
  assert.equal(receipt.reason, "body-too-large");
  assert.equal(cancellations, 1);
});

test("body read failure never exposes the underlying exception", async () => {
  const r = await failed(
    async () =>
      new Response(
        new ReadableStream({
          start(c) {
            c.error(new Error(marker));
          },
        }),
      ),
  );
  assert.equal(r.reason, "body-read-error");
});

test("timeout covers a pending response body and joins cancellation before returning", async () => {
  let release, entered;
  const cancelled = new Promise((resolve) => {
    entered = resolve;
  });
  const settled = new Promise((resolve) => {
    release = resolve;
  });
  let returned = false,
    cancellations = 0;
  const attempt = failed(
    async () =>
      new Response(
        new ReadableStream({
          cancel() {
            cancellations++;
            entered();
            return settled;
          },
        }),
      ),
    { timeoutMs: 10 },
  ).then((r) => {
    returned = true;
    return r;
  });
  await cancelled;
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(returned, false);
  release();
  const receipt = await attempt;
  assert.equal(receipt.reason, "timeout");
  assert.equal(cancellations, 1);
});

test("network failures, including refused redirects, never replay or reflect errors", async () => {
  const r = await failed(async (_url, init) => {
    assert.equal(init.redirect, "error");
    throw new Error(marker);
  });
  assert.equal(r.status, null);
  assert.equal(r.reason, "network-error");
  assert.equal(r.outcome, "unconfirmed");
});

test("expired deadline and pre-cancellation prevent dispatch", async () => {
  const r = await failed(
    async () => {
      assert.fail("expired request dispatched");
    },
    { deadline: Date.now() - 1, expectedCalls: 0 },
  );
  assert.equal(r.reason, "deadline");
  assert.equal(r.dispatched, false);
  assert.equal(r.outcome, "not-dispatched");
  const c = new AbortController();
  c.abort(new Error(marker));
  const cancelled = await failed(
    async () => {
      assert.fail("cancelled request dispatched");
    },
    { signal: c.signal, expectedCalls: 0 },
  );
  assert.equal(cancelled.reason, "cancelled");
});

test("the original deadline shortens the request timeout and is never reset", async () => {
  const r = await failed(
    async (_url, init) =>
      new Promise((_resolve, reject) => {
        init.signal.addEventListener("abort", () => reject(new Error(marker)), { once: true });
      }),
    { deadline: Date.now() + 20 },
  );
  assert.equal(r.reason, "deadline");
});

test("external cancellation aborts a dispatched body and joins its original settlement", async () => {
  const c = new AbortController();
  let release, entered;
  const started = new Promise((resolve) => {
    entered = resolve;
  });
  const settled = new Promise((resolve) => {
    release = resolve;
  });
  let returned = false,
    cancellations = 0;
  const attempt = failed(
    async () =>
      new Response(
        new ReadableStream({
          pull() {
            entered();
          },
          cancel() {
            cancellations++;
            return settled;
          },
        }),
      ),
    { signal: c.signal },
  ).then((r) => {
    returned = true;
    return r;
  });
  await started;
  c.abort(new Error(marker));
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(returned, false);
  release();
  assert.equal((await attempt).reason, "cancelled");
  assert.equal(cancellations, 1);
});

test("missing/malformed actions or deadline bounds cannot dispatch", async () => {
  for (const extra of [
    { action: undefined },
    { action: marker },
    { deadline: NaN },
    { timeoutMs: 0 },
    { timeoutMs: 30_001 },
  ]) {
    await assert.rejects(
      requestControllerJSON({
        ...options,
        ...extra,
        invoke: () => assert.fail("invalid request dispatched"),
      }),
      /invalid qualification/,
    );
  }
  assert.deepEqual(
    await requestControllerJSON({
      ...options,
      action: "finalize",
      timeoutMs: 300_000,
      invoke: async () => Response.json({ finalized: true }),
    }),
    { finalized: true },
  );
});

test("existing cleanup fencing actions remain available without accepting arbitrary actions", async () => {
  for (const action of ["discover", "begin-finalization", "attest", "finalize", "retire"])
    assert.deepEqual(
      await requestControllerJSON({
        ...options,
        action,
        invoke: async () => Response.json({ accepted: true }),
      }),
      { accepted: true },
    );
});

test("idle and issuer source contracts pin the exact controller transport dependency", () => {
  const expected = createHash("sha256")
    .update(fs.readFileSync(new URL("./image-qualification-controller-http.mjs", import.meta.url)))
    .digest("hex");
  for (const sources of [idleSourceIdentity(), replacementSources()])
    assert.equal(sources["image-qualification-controller-http.mjs"], expected);
});
