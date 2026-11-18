import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { Cloudflare, discoverWithOwnedController } from "./image-qualification-control.mjs";
import { authorityName, controllerName, bindingShape } from "./image-qualification-isolation.mjs";
import { fixtureAdmission, version } from "./fixtures/qualification-isolation.mjs";

const marker = "synthetic-private-body-and-token",
  immediate = async () => {};
const foreign = "22222222-2222-4222-8222-222222222222";
async function fixture(t, { reply, pause = immediate, maxUploads = 1 } = {}) {
  const { admission, bindings } = fixtureAdmission();
  const cf = Object.assign(new Cloudflare(admission.accountId, marker), { admission });
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "controller-provisioning-")));
  fs.chmodSync(dir, 0o700);
  const old = process.env.QUALIFICATION_PROOF_DIR;
  process.env.QUALIFICATION_PROOF_DIR = dir;
  t.after(() => {
    if (old === undefined) delete process.env.QUALIFICATION_PROOF_DIR;
    else process.env.QUALIFICATION_PROOF_DIR = old;
  });
  const state = {
    puts: 0,
    deletes: 0,
    posts: 0,
    rpc: 0,
    requests: [],
    present: false,
    version,
    exposure: { enabled: false, previews_enabled: false },
  };
  const source = fs.readFileSync(
    new URL("./image-qualification-controller-worker.mjs", import.meta.url),
  );
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
      assert.equal(req.headers.authorization, `Bearer ${marker}`);
      state.requests.push({ route: route ?? url.pathname, method: req.method });
      if (!route) {
        assert.equal(req.method, "POST");
        state.rpc++;
        if (reply && (await reply({ req, res, route: url.pathname, state, send }))) return;
        return sendRuntime();
      }
      const own = `/workers/scripts/${controllerName}`;
      if (req.method === "PUT") {
        assert.equal(route, own);
        state.puts++;
        assert(state.puts <= maxUploads);
        const chunks = [];
        for await (const chunk of req) chunks.push(chunk);
        const form = await new Request("http://fixture.invalid", {
          method: "PUT",
          headers: { "content-type": req.headers["content-type"] },
          body: Buffer.concat(chunks),
        }).formData();
        const meta = JSON.parse(await form.get("metadata").text());
        assert(Buffer.from(await form.get("controller.mjs").arrayBuffer()).equals(source));
        assert.equal(meta.bindings.find((b) => b.name === "CONTROLLER_TOKEN").text, marker);
        state.settings = { ...meta, bindings: bindingShape(meta.bindings) };
        state.present = true;
      }
      if (req.method === "DELETE") {
        assert.equal(route, own);
        state.deletes++;
      }
      if (req.method === "POST") {
        assert.equal(route, own + "/subdomain");
        state.posts++;
        let b = "";
        for await (const c of req) b += c;
        state.exposure = JSON.parse(b);
      }
      if (reply && (await reply({ req, res, route, state, send }))) return;
      if (req.method === "PUT") return send({ deployment_id: state.version, id: controllerName });
      if (req.method === "DELETE") {
        state.present = false;
        return send({});
      }
      if (route === "/workers/scripts")
        return send([{ id: authorityName }, ...(state.present ? [{ id: controllerName }] : [])]);
      if (route === "/workers/durable_objects/namespaces")
        return send(admission.authority.namespaces);
      if (route === "/workers/subdomain") return send({ subdomain: "example" });
      if (route.includes("/domains") || route.includes("/routes")) return send([]);
      const [, name, tail] = /^\/workers\/scripts\/([^/]+)(.*)$/.exec(route) ?? [];
      assert([authorityName, controllerName].includes(name), `unexpected Worker ${name}`);
      if (name === controllerName && !state.present) return send({}, 404);
      if (tail === "/settings") return send(name === authorityName ? { bindings } : state.settings);
      if (tail === "/deployments")
        return send({
          deployments: [
            {
              versions: [
                { version_id: name === authorityName ? version : state.version, percentage: 100 },
              ],
            },
          ],
        });
      if (tail === "/schedules") return send({ schedules: [] });
      if (tail === "/subdomain")
        return send(
          name === authorityName ? { enabled: false, previews_enabled: false } : state.exposure,
        );
      if (tail === "/content/v2") {
        const form = new FormData();
        form.set("controller.mjs", new Blob([state.source ?? source]), "controller.mjs");
        const response = new Response(form);
        res.writeHead(200, Object.fromEntries(response.headers));
        res.end(Buffer.from(await response.arrayBuffer()));
        return;
      }
      assert.fail(`unexpected route ${route}`);
    } catch (error) {
      state.error = error;
      if (!res.headersSent) res.writeHead(500);
      res.end();
    }
    function sendRuntime() {
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        state.rpcBody = body;
        res.writeHead(200, { "content-type": "application/json" });
        res.end(
          JSON.stringify(
            state.rpcReply ??
              (url.pathname === "/transport-discover" ? { probe: null } : { run: null }),
          ),
        );
      });
    }
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const native = globalThis.fetch;
  t.mock.method(globalThis, "fetch", (input, init = {}) => {
    const url = new URL(input);
    assert(
      ["https://api.cloudflare.com", `https://${controllerName}.example.workers.dev`].includes(
        url.origin,
      ),
    );
    assert.equal(init.redirect, "error");
    assert(init.signal instanceof AbortSignal);
    return native(`http://127.0.0.1:${server.address().port}${url.pathname}${url.search}`, init);
  });
  t.after(async () => {
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
    fs.rmSync(dir, { recursive: true, force: true });
    assert.ifError(state.error);
  });
  const invoke = (options = {}) =>
    discoverWithOwnedController(cf, "d".repeat(64), marker, {
      idleOnly: true,
      v2: false,
      pause,
      ...options,
    });
  const receipt = () => {
    const dirs = fs
      .readdirSync(dir, { withFileTypes: true })
      .filter((d) => d.isDirectory())
      .map((d) => d.name);
    assert.equal(dirs.length, 1);
    return JSON.parse(fs.readFileSync(path.join(dir, dirs[0], "provisioning.json")));
  };
  return { cf, state, invoke, receipt, dir };
}

test("HTTP: acknowledged upload, exact readback, discovery and verified deletion", async (t) => {
  const f = await fixture(t);
  assert.deepEqual((await f.invoke()).discovered, { run: null });
  assert.equal(f.state.puts, 1);
  assert.equal(f.state.deletes, 1);
  assert.equal(f.state.rpc, 1);
  const r = f.receipt();
  assert.equal(r.acknowledgedVersion, version);
  assert.equal(r.settlement, "deleted-and-absent");
  assert.equal(r.discovery, "idle");
  assert.equal(r.outcome, "verified");
  assert(!JSON.stringify(r).includes(marker));
  assert(r.events.some((e) => e.contentSha256));
  await assert.rejects(f.invoke(), /receipt exists/);
  assert.equal(f.state.puts, 1);
});

test("HTTP: empty204 deletion acknowledgement requires exact subsequent absence", async (t) => {
  const f = await fixture(t, {
    reply: async ({ req, res, state }) => {
      if (req.method === "DELETE") {
        state.present = false;
        res.writeHead(204);
        res.end();
        return true;
      }
    },
  });
  await f.invoke();
  assert.equal(f.state.deletes, 1);
  const r = f.receipt();
  assert(r.events.some((e) => e.method === "DELETE" && e.response.status === 204));
  assert.equal(r.settlement, "deleted-and-absent");
  assert.equal(r.outcome, "verified");
});

test("HTTP: empty204 with controller residue remains unresolved without delete replay", async (t) => {
  const f = await fixture(t, {
    reply: async ({ req, res }) => {
      if (req.method === "DELETE") {
        res.writeHead(204);
        res.end();
        return true;
      }
    },
  });
  await assert.rejects(f.invoke(), /deletion was not observable/);
  assert.equal(f.state.puts, 1);
  assert.equal(f.state.deletes, 1);
  const r = f.receipt();
  assert.notEqual(r.settlement, "deleted-and-absent");
  assert.equal(r.settlement, "unresolved");
  assert.equal(r.outcome, "failed");
  await assert.rejects(f.invoke(), /receipt exists/);
  assert.equal(f.state.deletes, 1);
});

test("HTTP: real five-second observation after deployment404, no duplicate upload", async (t) => {
  let missing = true;
  const f = await fixture(t, {
    pause: undefined,
    reply: async ({ route, state, send }) => {
      if (missing && state.present && route.endsWith(`/${controllerName}/deployments`)) {
        missing = false;
        send({}, 404);
        return true;
      }
    },
  });
  const start = Date.now();
  await f.invoke({ pause: undefined });
  assert(Date.now() - start >= 4900);
  assert.equal(f.state.puts, 1);
  assert.equal(f.state.deletes, 1);
  const failed = f
    .receipt()
    .events.find((e) => e.response?.status === 404 && e.phase === "management-readiness");
  assert(failed);
  assert.equal(failed.response.ray, "0123456789abcdef-DFW");
});

for (const kind of [
  "denied",
  "malformed",
  "empty",
  "split",
  "wrong-version",
  "wrong-bindings",
  "wrong-content",
]) {
  test(`HTTP: ${kind} is fatal, never treated as pending`, async (t) => {
    let faulty = true;
    const f = await fixture(t, {
      reply: async ({ route, state, send }) => {
        if (state.present && route.endsWith(`/${controllerName}/deployments`) && faulty) {
          if (kind === "denied") {
            faulty = false;
            send({}, 403);
            return true;
          }
          if (kind === "malformed") {
            faulty = false;
            send({ bad: true });
            return true;
          }
          if (kind === "empty") {
            faulty = false;
            send({ deployments: [] });
            return true;
          }
          if (kind === "split") {
            faulty = false;
            send({
              deployments: [
                {
                  versions: [
                    { version_id: version, percentage: 50 },
                    { version_id: foreign, percentage: 50 },
                  ],
                },
              ],
            });
            return true;
          }
          if (kind === "wrong-version") {
            faulty = false;
            send({ deployments: [{ versions: [{ version_id: foreign, percentage: 100 }] }] });
            return true;
          }
        }
        if (
          state.present &&
          route.endsWith(`/${controllerName}/settings`) &&
          faulty &&
          kind === "wrong-bindings"
        ) {
          faulty = false;
          send({ ...state.settings, bindings: [] });
          return true;
        }
        if (
          state.present &&
          route.endsWith(`/${controllerName}/content/v2`) &&
          faulty &&
          kind === "wrong-content"
        ) {
          faulty = false;
          state.source = Buffer.from(marker);
        }
      },
    });
    await assert.rejects(f.invoke());
    assert.equal(f.state.puts, 1);
    assert.equal(f.state.rpc, 0);
    assert.equal(f.receipt().outcome, "failed");
  });
}

for (const kind of [
  "lost",
  "missing-version",
  "missing-success",
  "invalid-json",
  "oversize",
  "redirect",
]) {
  test(`HTTP: ${kind} acknowledgement cannot replay upload; original owner cleans up`, async (t) => {
    const f = await fixture(t, {
      reply: async ({ req, res, route, state, send }) => {
        if (req.method !== "PUT") return;
        if (kind === "lost") {
          req.socket.destroy();
          return true;
        }
        if (kind === "missing-version") {
          send({ id: controllerName });
          return true;
        }
        if (kind === "missing-success") {
          res.writeHead(200, { "content-type": "application/json" });
          res.end(JSON.stringify({ result: { deployment_id: version } }));
          return true;
        }
        if (kind === "invalid-json") {
          res.writeHead(200);
          res.end(marker);
          return true;
        }
        if (kind === "oversize") {
          res.writeHead(200);
          res.end(marker.repeat(7000));
          return true;
        }
        if (kind === "redirect") {
          res.writeHead(302, { location: "https://not-allowed.invalid/" });
          res.end();
          return true;
        }
      },
    });
    await assert.rejects(f.invoke());
    assert.equal(f.state.puts, 1);
    assert.equal(f.state.deletes, 1);
    assert.equal(f.state.rpc, 0);
    assert.equal(f.state.present, false);
    assert.equal(f.receipt().outcome, "failed");
    assert(!JSON.stringify(f.receipt()).includes(marker));
    await assert.rejects(f.invoke(), /receipt exists/);
    assert.equal(f.state.puts, 1);
  });
}

test("HTTP: cancellation during pending readiness still settles the exact uploaded controller", async (t) => {
  const stop = new AbortController();
  let pending = true;
  const f = await fixture(t, {
    pause: async () => {
      pending = false;
      stop.abort();
    },
    reply: async ({ route, state, send }) => {
      if (pending && state.present && route.endsWith(`/${controllerName}/deployments`)) {
        send({}, 404);
        return true;
      }
    },
  });
  await assert.rejects(f.invoke({ signal: stop.signal }));
  assert.equal(f.state.puts, 1);
  assert.equal(f.state.deletes, 1);
  assert.equal(f.receipt().settlement, "deleted-and-absent");
});

test("HTTP: pre-dispatch cancellation causes no upload", async (t) => {
  const f = await fixture(t);
  const stop = new AbortController();
  stop.abort();
  await assert.rejects(f.invoke({ signal: stop.signal }));
  assert.equal(f.state.puts, 0);
  assert.equal(f.receipt().settlement, "not-needed");
});

test("HTTP: original deadline retains cleanup reserve, no forward effect when too short", async (t) => {
  const f = await fixture(t);
  f.cf.controllerDeadline = Date.now() + 59_000;
  await assert.rejects(f.invoke());
  assert.equal(f.state.puts, 0);
});

test("HTTP: lost delete response records unresolved settlement without a second deletion", async (t) => {
  const f = await fixture(t, {
    reply: async ({ req, state }) => {
      if (req.method === "DELETE") {
        state.present = false;
        req.socket.destroy();
        return true;
      }
    },
  });
  await assert.rejects(f.invoke(), /cleanup/);
  assert.equal(f.state.deletes, 1);
  assert.equal(f.state.present, false);
  assert.equal(f.receipt().settlement, "unresolved");
});

test("HTTP: changed serving identity blocks cleanup deletion", async (t) => {
  const f = await fixture(t, {
    reply: async ({ route, state }) => {
      if (route.endsWith(`/${controllerName}/subdomain`) && state.posts) {
        state.version = foreign;
      }
    },
  });
  await assert.rejects(f.invoke());
  assert.equal(f.state.deletes, 0);
  assert.equal(f.receipt().settlement, "unresolved");
});

test("HTTP: a visible pre-existing controller is never overwritten or deleted", async (t) => {
  const f = await fixture(t);
  f.state.present = true;
  f.state.settings = { bindings: [], tags: [] };
  await assert.rejects(f.invoke(), /collision/);
  assert.equal(f.state.puts, 0);
  assert.equal(f.state.deletes, 0);
});

test("HTTP: transport-v2 discovery uses the same provisioning and cleanup owner", async (t) => {
  const f = await fixture(t);
  assert.deepEqual((await f.invoke({ v2: true, probeOnly: true, idleOnly: false })).discovered, {
    probe: null,
  });
  assert.equal(f.state.puts, 1);
  assert.equal(f.state.deletes, 1);
});

test("HTTP: persistent deployment404 exhausts both bounded observations and retains recovery custody", async (t) => {
  let reads = 0;
  const f = await fixture(t, {
    reply: async ({ route, state, send }) => {
      if (state.present && route.endsWith(`/${controllerName}/deployments`)) {
        reads++;
        send({}, 404);
        return true;
      }
    },
  });
  await assert.rejects(f.invoke(), /404/);
  assert.equal(reads, 24);
  assert.equal(f.state.puts, 1);
  assert.equal(f.state.deletes, 0);
  assert.equal(f.state.rpc, 0);
  assert.equal(f.receipt().settlement, "unresolved");
  assert(f.receipt().events.length <= 800);
});

for (const phase of ["upload", "content"]) {
  test(`HTTP: cancellation during ${phase} joins forward failure and preserves cleanup`, async (t) => {
    const stop = new AbortController();
    let cancelled = false;
    const f = await fixture(t, {
      reply: async ({ req, res, route, state }) => {
        if (
          !cancelled &&
          ((phase === "upload" && req.method === "PUT") ||
            (phase === "content" && route.endsWith(`/${controllerName}/content/v2`)))
        ) {
          cancelled = true;
          res.writeHead(200, { "content-type": "application/json" });
          res.write("{");
          stop.abort();
          return true;
        }
      },
    });
    await assert.rejects(f.invoke({ signal: stop.signal }));
    assert.equal(f.state.puts, 1);
    assert.equal(f.state.deletes, 1);
    assert.equal(f.state.present, false);
    assert.equal(f.receipt().outcome, "failed");
  });
}

test("HTTP: observation deadline aborts a stalled management body and preserves cleanup", async (t) => {
  let now = Date.now();
  t.mock.method(Date, "now", () => now);
  let stalled = false;
  const f = await fixture(t, {
    reply: async ({ req, res, route, state }) => {
      // Advance the wall clock without advancing the already armed outer timer.
      // The observation must independently abort the body within its remainder.
      if (req.method === "PUT") now += 119_000;
      if (!stalled && state.present && route.endsWith(`/${controllerName}/deployments`)) {
        stalled = true;
        res.writeHead(200, { "content-type": "application/json" });
        res.write("{");
        return true;
      }
    },
  });
  const start = performance.now();
  await assert.rejects(f.invoke());
  assert(performance.now() - start < 30_000);
  assert(stalled);
  assert.equal(f.state.rpc, 0);
  assert.equal(f.state.puts, 1);
  assert.equal(f.state.deletes, 1);
  assert.equal(f.receipt().settlement, "deleted-and-absent");
  assert.equal(f.receipt().outcome, "failed");
});

test("HTTP: late successful management read cannot pass the observation deadline", async (t) => {
  let now = Date.now();
  t.mock.method(Date, "now", () => now);
  let advanced = false;
  const f = await fixture(t, {
    reply: async ({ route, state }) => {
      if (!advanced && state.present && route.endsWith(`/${controllerName}/content/v2`)) {
        advanced = true;
        now += 60_001;
      }
    },
  });
  await assert.rejects(f.invoke(), /observation exhausted/);
  assert(advanced);
  assert.equal(f.state.rpc, 0);
  assert.equal(f.state.puts, 1);
  assert.equal(f.state.deletes, 1);
  assert.equal(f.receipt().settlement, "deleted-and-absent");
});

test("HTTP: original deadline exhaustion cannot reset the cleanup clock", async (t) => {
  let now = Date.now();
  t.mock.method(Date, "now", () => now);
  let pending = true;
  const f = await fixture(t, {
    pause: async () => {
      now += 180_001;
    },
    reply: async ({ route, state, send }) => {
      if (pending && state.present && route.endsWith(`/${controllerName}/deployments`)) {
        pending = false;
        send({}, 404);
        return true;
      }
    },
  });
  await assert.rejects(f.invoke());
  assert.equal(f.state.puts, 1);
  assert.equal(f.state.deletes, 0);
  assert.equal(f.receipt().settlement, "unresolved");
});

test("HTTP: reflected private bytes in metadata headers never enter the provisioning receipt", async (t) => {
  const f = await fixture(t, {
    reply: async ({ req, res, send }) => {
      if (req.method === "PUT") {
        res.setHeader("cf-ray", marker);
        res.writeHead(200, { "content-type": "application/json" });
        res.end(
          JSON.stringify({ success: true, result: { deployment_id: version, private: marker } }),
        );
        return true;
      }
    },
  });
  await f.invoke();
  assert(!JSON.stringify(f.receipt()).includes(marker));
  assert.equal(f.receipt().upload.response.ray, undefined);
});

test("HTTP: oversized source body fails closed and cannot authorize deletion", async (t) => {
  const f = await fixture(t, {
    reply: async ({ res, route, state }) => {
      if (state.present && route.endsWith(`/${controllerName}/content/v2`)) {
        res.writeHead(200, { "content-type": "multipart/form-data; boundary=test" });
        res.end(marker.repeat(7000));
        return true;
      }
    },
  });
  await assert.rejects(f.invoke());
  assert.equal(f.state.deletes, 0);
  assert.equal(f.receipt().settlement, "unresolved");
  assert(!JSON.stringify(f.receipt()).includes(marker));
});

for (const phase of ["management", "runtime"])
  test(`HTTP: cancellation during canonical discovery ${phase} retains cleanup reserve`, async (t) => {
    const stop = new AbortController();
    let f,
      interrupted = false;
    f = await fixture(t, {
      reply: async ({ route, res, state }) => {
        const target =
          phase === "runtime" ? "/discover" : `/workers/scripts/${controllerName}/content/v2`;
        if (
          !interrupted &&
          state.present &&
          route === target &&
          f.receipt().stage === "discovery"
        ) {
          interrupted = true;
          res.writeHead(200, { "content-type": "application/json" });
          res.write("{");
          setTimeout(() => {
            if (phase === "runtime") process.emit("SIGTERM");
            else stop.abort();
          }, 10);
          return true;
        }
      },
    });
    await assert.rejects(f.invoke({ signal: stop.signal, idleOnly: false }));
    assert(interrupted);
    assert.equal(f.state.puts, 1);
    assert.equal(f.state.deletes, 1);
    assert.equal(f.state.present, false);
    assert.equal(f.receipt().failedStage, "discovery");
    assert.equal(f.receipt().settlement, "deleted-and-absent");
    assert.equal(f.receipt().outcome, "failed");
  });

test("HTTP: recursive transport recovery retains the replacement version for original finalization", async (t) => {
  let probes = 0;
  const f = await fixture(t, {
    maxUploads: 2,
    reply: async ({ route, req, state }) => {
      if (req.method === "PUT" && state.puts === 2) state.version = foreign;
      if (route === "/transport-discover")
        state.rpcReply = {
          probe:
            ++probes === 1
              ? {
                  protocol: 1,
                  state: "pending",
                  observedAt: new Date().toISOString(),
                  identity: { deploymentHash: "e".repeat(64) },
                }
              : null,
        };
      if (route === "/discover") state.rpcReply = { run: null };
    },
  });
  // Each original provisioning identity uploads once, including the canonical
  // owner's replacement. The HTTP fixture otherwise rejects repeated uploads.
  await f.invoke({ v2: true, idleOnly: false });
  assert.equal(f.state.puts, 2);
  assert.equal(f.state.deletes, 1);
  assert.equal(f.cf.controllerVersion, foreign);
  const receipts = fs
    .readdirSync(f.dir)
    .map((d) => JSON.parse(fs.readFileSync(path.join(f.dir, d, "provisioning.json"))));
  assert.equal(receipts.length, 2);
  assert(
    receipts.every(
      (r) => r.retainedVersion === foreign && r.settlement === "retained-for-original-finalizer",
    ),
  );
  f.cf.discoverySignal = new AbortController().signal;
  assert.equal(await f.cf.verifyController(), foreign);
  await f.cf.deleteScript(controllerName);
  assert.equal(f.state.present, false);
});

function runtimeHTML404(res) {
  res.writeHead(404, { "content-type": "text/html", "cf-ray": "0123456789abcdef-DFW" });
  res.end(`<!doctype html><p>${marker}</p>`);
}

test("HTTP: transport discovery HTML404 then canonical probe uses one upload and settles", async (t) => {
  let failed = false;
  const f = await fixture(t, {
    reply: async ({ route, res }) => {
      if (route === "/transport-discover" && !failed) {
        failed = true;
        runtimeHTML404(res);
        return true;
      }
    },
  });
  assert.deepEqual((await f.invoke({ probeOnly: true })).discovered, { probe: null });
  assert(failed);
  assert.equal(f.state.rpc, 2);
  assert.equal(f.state.puts, 1);
  assert.equal(f.state.deletes, 1);
  assert.equal(f.state.present, false);
  const r = f.receipt();
  assert.equal(r.acknowledgedVersion, r.verifiedVersion);
  assert.equal(r.discovery, "probe-observed");
  assert.equal(r.outcome, "verified");
  assert.equal(r.settlement, "deleted-and-absent");
  const failures = fs.readdirSync(f.dir).filter((n) => n.startsWith("controller-http-failure-"));
  assert.equal(failures.length, 1);
  const failure = JSON.parse(fs.readFileSync(path.join(f.dir, failures[0])));
  assert.equal(failure.action, "transport-discover");
  assert.equal(failure.status, 404);
  assert(!JSON.stringify(failure).includes(marker));
  await assert.rejects(f.invoke({ probeOnly: true }), /receipt exists/);
  assert.equal(f.state.puts, 1);
});

for (const kind of ["JSON404", "network", "cancel", "deadline"]) {
  test(`HTTP: transport discovery ${kind} stops and original cleanup settles`, async (t) => {
    let now = Date.now();
    t.mock.method(Date, "now", () => now);
    let failed = false;
    const stop = new AbortController();
    const f = await fixture(t, {
      reply: async ({ route, req, res }) => {
        if (route !== "/transport-discover") return;
        assert(!failed);
        failed = true;
        if (kind === "network") req.socket.destroy();
        else if (kind === "cancel") {
          res.writeHead(200, { "content-type": "application/json" });
          res.write("{");
          setTimeout(() => stop.abort(), 10);
        } else if (kind === "deadline") {
          now += 60_001;
          runtimeHTML404(res);
        } else {
          res.writeHead(404, { "content-type": "application/json" });
          res.end(JSON.stringify({ error: marker }));
        }
        return true;
      },
    });
    await assert.rejects(f.invoke({ v2: true, idleOnly: false, signal: stop.signal }));
    assert(failed);
    assert.equal(f.state.rpc, 1);
    assert.equal(f.state.puts, 1);
    assert.equal(f.state.deletes, 1);
    assert.equal(f.state.present, false);
    assert.equal(f.receipt().failedStage, "discovery");
    assert.equal(f.receipt().outcome, "failed");
    assert.equal(f.receipt().settlement, "deleted-and-absent");
    assert(
      !f.state.requests.some((r) =>
        ["/discover", "/transport-open", "/transport-settle"].includes(r.route),
      ),
    );
    await assert.rejects(f.invoke(), /receipt exists/);
    assert.equal(f.state.puts, 1);
    assert.equal(f.state.deletes, 1);
  });
}

test("HTTP: reaper transport discovery readiness precedes registry observation and original finalizer", async (t) => {
  let failed = false;
  const f = await fixture(t, {
    reply: async ({ route, res }) => {
      if (route === "/transport-discover" && !failed) {
        failed = true;
        runtimeHTML404(res);
        return true;
      }
    },
  });
  assert.deepEqual((await f.invoke({ v2: true, idleOnly: false })).discovered, { run: null });
  assert(failed);
  assert.deepEqual(
    f.state.requests
      .filter((r) => r.method === "POST" && !r.route.includes("/workers/"))
      .map((r) => r.route),
    ["/transport-discover", "/transport-discover", "/discover"],
  );
  assert.equal(f.state.puts, 1);
  assert.equal(f.state.deletes, 0);
  assert.equal(f.receipt().settlement, "retained-for-original-finalizer");
  // The admitted caller stays with the original finalizer and exact version.
  f.cf.discoverySignal = new AbortController().signal;
  assert.equal(await f.cf.verifyController(), version);
  await f.cf.deleteScript(controllerName);
  assert.equal(f.state.deletes, 1);
  assert.equal(f.state.present, false);
});
