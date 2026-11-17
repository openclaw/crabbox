import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { Cloudflare, enableRelaySubdomain } from "./image-qualification-control.mjs";
import { authorityName, bindingShape } from "./image-qualification-isolation.mjs";
import { fixtureAdmission, ownedSettings, version } from "./fixtures/qualification-isolation.mjs";

const worker = "crabbox-image-qualification-relay-42-1";
const pending = { enabled: false, previews_enabled: false };
const desired = { enabled: true, previews_enabled: false };
const immediate = async () => {};
const privateMarker = "synthetic-private-body";

// Real TCP/HTTP, native fetch and the real management adapter, without cloud
// credentials or external egress. Faults affect provider replies, not validators.
async function fixture(t, { reply, exposure = () => desired, pause = immediate } = {}) {
  const { admission, bindings } = fixtureAdmission();
  const relayBindings = [
    { name: "CANDIDATE", type: "service", service: "crabbox-image-qualification-42-1" },
  ];
  const cf = Object.assign(new Cloudflare(admission.accountId, "synthetic-management-token"), {
    admission,
  });
  const directory = fs.realpathSync(
    fs.mkdtempSync(path.join(os.tmpdir(), "qualification-exposure-")),
  );
  fs.chmodSync(directory, 0o700);
  const priorProof = process.env.QUALIFICATION_PROOF_DIR;
  process.env.QUALIFICATION_PROOF_DIR = directory;
  t.after(() => {
    if (priorProof === undefined) delete process.env.QUALIFICATION_PROOF_DIR;
    else process.env.QUALIFICATION_PROOF_DIR = priorProof;
  });
  const state = {
    posts: 0,
    reads: 0,
    requests: [],
    settings: ownedSettings(admission, worker, relayBindings),
  };
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, "http://fixture.invalid");
    const route = url.pathname.split(`/accounts/${admission.accountId}`)[1];
    const send = (result, status = 200) => {
      res.writeHead(status, {
        "content-type": "application/json",
        "cf-ray": "0123456789abcdef-DFW",
      });
      res.end(JSON.stringify({ success: status === 200, result }));
    };
    try {
      assert(route, "account must remain pinned");
      assert.equal(req.headers.authorization, "Bearer synthetic-management-token");
      state.requests.push({ route, method: req.method });
      if (req.method === "POST") {
        state.posts++;
        assert.equal(route, `/workers/scripts/${worker}/subdomain`);
        let body = "";
        for await (const chunk of req) body += chunk;
        assert.deepEqual(JSON.parse(body), desired);
      } else assert.equal(req.method, "GET", "no other mutation is admitted");
      if (reply && (await reply({ req, res, route, send, state }))) return;
      if (route === "/workers/subdomain") return send({ subdomain: "example" });
      if (route === "/workers/durable_objects/namespaces")
        return send(admission.authority.namespaces);
      if (route.includes("/domains") || route.includes("/routes")) return send([]);
      const [, name, tail] = /^\/workers\/scripts\/([^/]+)(.*)$/.exec(route) ?? [];
      assert([authorityName, worker].includes(name), `unexpected Worker ${name}`);
      if (tail === "/settings") return send(name === authorityName ? { bindings } : state.settings);
      if (tail === "/deployments")
        return send({ deployments: [{ versions: [{ version_id: version, percentage: 100 }] }] });
      if (tail === "/schedules") return send({ schedules: [] });
      if (tail === "/subdomain") {
        if (name === authorityName) return send(pending);
        if (req.method === "POST") return send(desired);
        if (state.posts === 0) return send(pending);
        return send(exposure(++state.reads, state));
      }
      assert.fail(`unexpected route ${route}`);
    } catch (error) {
      state.serverError = error;
      if (!res.headersSent) res.writeHead(500);
      res.end();
    }
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const nativeFetch = globalThis.fetch;
  t.mock.method(globalThis, "fetch", (input, init = {}) => {
    const url = new URL(input);
    assert.equal(url.origin, "https://api.cloudflare.com", "external egress is forbidden");
    assert.equal(init.redirect, "error");
    assert(init.signal instanceof AbortSignal);
    return nativeFetch(
      `http://127.0.0.1:${server.address().port}${url.pathname}${url.search}`,
      init,
    );
  });
  t.after(async () => {
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
    fs.rmSync(directory, { recursive: true, force: true });
    assert.ifError(state.serverError);
  });
  const handoff = {
    runId: "image-qualification-42-1",
    relayWorker: worker,
    startsAt: new Date(Date.now() - 1000).toISOString(),
    workExpiresAt: new Date(Date.now() + 120_000).toISOString(),
  };
  return {
    cf,
    state,
    handoff,
    directory,
    run: (options = {}) =>
      enableRelaySubdomain(cf, worker, version, bindingShape(relayBindings), handoff, {
        pause,
        ...options,
      }),
    receipt: () => JSON.parse(fs.readFileSync(path.join(directory, "relay-exposure.json"))),
  };
}

test("native HTTP: exact acknowledged exposure succeeds with one POST and bracketed fences", async (t) => {
  const f = await fixture(t);
  assert.equal(await f.run(), `https://${worker}.example.workers.dev`);
  assert.equal(f.state.posts, 1);
  assert.equal(f.state.reads, 1);
  assert.equal(f.receipt().state, "verified");
  assert.equal(f.receipt().requests, f.state.requests.length);
  const exposureRead = f.state.requests.findLastIndex(
    (r) => r.route === `/workers/scripts/${worker}/subdomain`,
  );
  assert(
    f.state.requests
      .slice(exposureRead + 1)
      .some((r) => r.route === `/workers/scripts/${authorityName}/settings`),
  );
  assert(!JSON.stringify(f.receipt()).includes("synthetic-management-token"));
});

test("native HTTP: only the exact initial exposure is pending, with fresh fences before each read", async (t) => {
  let waits = 0;
  const f = await fixture(t, {
    exposure: (n) => (n < 3 ? pending : desired),
    pause: async (ms, _, { signal }) => {
      assert.equal(ms, 5000);
      assert(!signal.aborted);
      waits++;
    },
  });
  await f.run();
  assert.equal(f.state.posts, 1);
  assert.equal(waits, 2);
  assert.deepEqual(
    f.receipt().observations.map((r) => r.exposure),
    [pending, pending, desired],
  );
  assert.equal(
    f.state.requests.filter((r) => r.route === `/workers/scripts/${authorityName}/settings`).length,
    5,
  );
});

test("persistent pending exposure stops at twelve observations without rewriting", async (t) => {
  const f = await fixture(t, { exposure: () => pending });
  await assert.rejects(f.run(), /twelve observations/);
  assert.equal(f.state.reads, 12);
  assert.equal(f.state.posts, 1);
  assert(f.receipt().requests <= 400);
  await assert.rejects(f.run(), /receipt already exists/);
  assert.equal(f.state.posts, 1);
});

for (const [name, value] of [
  ["preview exposure", { enabled: true, previews_enabled: true }],
  ["missing flag", { enabled: true }],
  ["string flag", { enabled: "true", previews_enabled: false }],
  ["null", null],
])
  test(`${name} fails immediately`, async (t) => {
    const f = await fixture(t, { exposure: () => value });
    await assert.rejects(f.run(), /malformed|unexpected state/);
    assert.equal(f.state.reads, 1);
    assert.equal(f.state.posts, 1);
  });

for (const status of [403, 404, 503])
  test(`HTTP ${status} is not a propagation retry`, async (t) => {
    const f = await fixture(t, {
      reply: ({ state, route, req, send }) => {
        if (
          state.posts &&
          req.method === "GET" &&
          route === `/workers/scripts/${worker}/subdomain`
        ) {
          send({ error: privateMarker }, status);
          return true;
        }
      },
    });
    await assert.rejects(f.run(), new RegExp(`returned ${status}`));
    assert.equal(f.state.posts, 1);
    assert(!JSON.stringify(f.receipt()).includes(privateMarker));
  });

for (const fault of ["lost-ack", "malformed-ack", "denied-write"])
  test(`${fault} never replays the POST`, async (t) => {
    const f = await fixture(t, {
      reply: ({ req, res }) => {
        if (req.method !== "POST") return false;
        if (fault === "lost-ack") res.destroy();
        else if (fault === "malformed-ack") res.end("{");
        else {
          res.writeHead(403);
          res.end();
        }
        return true;
      },
    });
    await assert.rejects(f.run());
    await assert.rejects(f.run(), /receipt already exists/);
    assert.equal(f.state.posts, 1);
    assert.equal(f.state.reads, 0);
    assert.equal(f.receipt().state, "write-intent");
  });

for (const fault of [
  "authority",
  "version",
  "bindings",
  "ownership",
  "namespace",
  "route",
  "schedule",
  "domain",
])
  test(`${fault} drift after acknowledgement stops before exposure read`, async (t) => {
    const f = await fixture(t, {
      reply: ({ req, route, state, send }) => {
        if (!state.posts || req.method !== "GET") return false;
        let result;
        if (
          (fault === "authority" && route === `/workers/scripts/${authorityName}/deployments`) ||
          (fault === "version" && route === `/workers/scripts/${worker}/deployments`)
        )
          result = {
            deployments: [
              {
                versions: [{ version_id: "22222222-2222-4222-8222-222222222222", percentage: 100 }],
              },
            ],
          };
        if (fault === "bindings" && route === `/workers/scripts/${worker}/settings`)
          result = ownedSettings(f.cf.admission, worker, [
            ...state.settings.bindings,
            { name: "EXTRA", type: "plain_text", text: "extra" },
          ]);
        if (fault === "ownership" && route === `/workers/scripts/${worker}/settings`)
          result = { ...state.settings, tags: [] };
        if (fault === "namespace" && route === "/workers/durable_objects/namespaces")
          result = [
            ...f.cf.admission.authority.namespaces,
            { id: "3".repeat(32), script: worker, class: "Unexpected" },
          ];
        if (fault === "route" && route.includes(`/services/${worker}/`))
          result = [{ pattern: "example.invalid/*" }];
        if (fault === "schedule" && route === `/workers/scripts/${worker}/schedules`)
          result = { schedules: [{}] };
        if (fault === "domain" && route === "/workers/domains") result = [{ service: worker }];
        if (result !== undefined) {
          send(result);
          return true;
        }
      },
    });
    await assert.rejects(f.run());
    assert.equal(f.state.reads, 0);
    assert.equal(f.state.posts, 1);
  });

test("wrong run, expired work window and exhausted reserve make no requests", async (t) => {
  const f = await fixture(t);
  f.handoff.runId = "image-qualification-43-1";
  await assert.rejects(f.run(), /identity/);
  f.handoff.runId = "image-qualification-42-1";
  f.handoff.workExpiresAt = new Date(Date.now() - 1).toISOString();
  await assert.rejects(f.run(), /window/);
  f.handoff.workExpiresAt = new Date(Date.now() + 59_000).toISOString();
  await assert.rejects(f.run(), /reserve/);
  assert.equal(f.state.requests.length, 0);
});

for (const stage of ["before", "fetch", "body", "wait"])
  test(`cancellation during ${stage} cannot issue another mutation`, async (t) => {
    const stop = new AbortController();
    if (stage === "before") stop.abort();
    const f = await fixture(t, {
      exposure: () => pending,
      reply: ({ req, res, route, state }) => {
        if (
          !state.posts ||
          req.method !== "GET" ||
          route !== `/workers/scripts/${worker}/subdomain`
        )
          return false;
        if (stage === "fetch" || stage === "body") {
          if (stage === "body") {
            res.writeHead(200, { "content-type": "application/json" });
            res.write('{"success":true,"result":');
          }
          setTimeout(() => stop.abort(), 10);
          return true;
        }
      },
      pause: async () => stop.abort(),
    });
    await assert.rejects(f.run({ signal: stop.signal }));
    assert.equal(f.state.posts, stage === "before" ? 0 : 1);
  });

test("the original work reserve clips a hung read instead of restarting the clock", async (t) => {
  const f = await fixture(t, { reply: () => true });
  f.handoff.workExpiresAt = new Date(Date.now() + 60_075).toISOString();
  const start = Date.now();
  await assert.rejects(f.run());
  assert(Date.now() - start < 2000);
  assert.equal(f.state.posts, 0);
});

test("generic controller and cleanup exposure retain single-shot behavior", async (t) => {
  const cf = new Cloudflare("a".repeat(32), "synthetic");
  const calls = [];
  t.mock.method(cf, "request", async (route, init = {}) => {
    calls.push([route, init.method ?? "GET"]);
    return pending;
  });
  await assert.rejects(
    cf.enableSubdomain("crabbox-image-qualification-controller"),
    /did not persist/,
  );
  assert.equal(calls.length, 2);
  calls.length = 0;
  await cf.disableSubdomain(worker);
  assert.equal(calls.length, 2);
});

test("native HTTP with the real five-second wait observes propagation once, without a second write", async (t) => {
  const f = await fixture(t, { exposure: (n) => (n === 1 ? pending : desired) });
  const start = Date.now();
  await f.run({ pause: undefined });
  assert(Date.now() - start >= 5000);
  assert.equal(f.state.posts, 1);
  assert.equal(f.state.reads, 2);
  const receipt = f.receipt();
  assert(receipt.observations[1].elapsedMs - receipt.observations[0].elapsedMs >= 5000);
  assert(receipt.observations[1].elapsedMs < 60_000);
});

for (const budget of ["sixty seconds", "original reserve"])
  test(`${budget} is never reset by a pending observation`, async (t) => {
    let now = Date.now();
    t.mock.method(Date, "now", () => now);
    const f = await fixture(t, {
      exposure: () => pending,
      pause: async () => {
        now += 60_000;
      },
    });
    if (budget === "original reserve")
      f.handoff.workExpiresAt = new Date(now + 61_000).toISOString();
    await assert.rejects(f.run(), /deadline/);
    assert.equal(f.state.posts, 1);
    assert.equal(f.state.reads, 1);
  });

for (const fault of ["malformed JSON", "oversized body", "redirect", "invalid hostname"])
  test(`${fault} is rejected over native HTTP`, async (t) => {
    const f = await fixture(t, {
      reply: ({ req, res, route, state, send }) => {
        if (!state.posts || req.method !== "GET") return false;
        if (fault === "invalid hostname" && route === "/workers/subdomain") {
          send({ subdomain: "example.invalid/path" });
          return true;
        }
        if (route !== `/workers/scripts/${worker}/subdomain` || fault === "invalid hostname")
          return false;
        if (fault === "redirect") {
          res.writeHead(302, { location: "https://example.invalid" });
          res.end();
        } else res.end(fault === "malformed JSON" ? "{" : "x".repeat(65537));
        return true;
      },
    });
    await assert.rejects(f.run());
    assert.equal(f.state.posts, 1);
    assert.equal(
      f.state.requests.filter(
        (r) => r.route === `/workers/scripts/${worker}/subdomain` && r.method === "GET",
      ).length,
      2,
    );
  });

test("an interrupted exposure write before dispatch is not replayed", async (t) => {
  const f = await fixture(t);
  const original = f.cf.request.bind(f.cf);
  let attempts = 0;
  t.mock.method(f.cf, "request", async (route, init, options) => {
    if (init?.method === "POST") {
      attempts++;
      throw new Error("synthetic before-dispatch failure");
    }
    return original(route, init, options);
  });
  await assert.rejects(f.run(), /before-dispatch/);
  await assert.rejects(f.run(), /receipt already exists/);
  assert.equal(attempts, 1);
  assert.equal(f.state.posts, 0);
});

test("unbounded inventory pagination fails at the finite management request budget", async (t) => {
  const f = await fixture(t, {
    reply: ({ route, res }) => {
      if (route !== "/workers/durable_objects/namespaces") return false;
      res.end(JSON.stringify({ success: true, result: [], result_info: { total_pages: 1000 } }));
      return true;
    },
  });
  await assert.rejects(f.run(), /request budget/);
  assert.equal(f.state.requests.length, 400);
  assert.equal(f.state.posts, 0);
});
