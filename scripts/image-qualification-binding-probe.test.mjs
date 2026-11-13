import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { Cloudflare } from "./image-qualification-control.mjs";
import { canonical, digest } from "./image-qualification-isolation.mjs";
import {
  bindingProbe,
  prepareBindingProbe,
  withBindingProbeJournal,
  readProbeResponse,
  probeContentIdentity,
} from "./image-qualification-binding-probe.mjs";
import caller from "./image-qualification-binding-caller.mjs";

const start = Date.parse("2026-11-01T00:00:00.000Z");
const key = "a".repeat(64);
const plan = () =>
  prepareBindingProbe({
    accountId: "a".repeat(32),
    tokenId: "b".repeat(32),
    sourceSha: "c".repeat(40),
    id: "123456abcdef",
    startsAt: new Date(start).toISOString(),
  });
const clone = (value) => structuredClone(value);

class MockCloudflare extends Cloudflare {
  constructor(p, options = {}) {
    super(p.accountId, "fake-api-token");
    this.plan = p;
    this.state = { workers: {}, calls: [], options, invocations: 0, saved: [], time: start + 1000 };
  }
  async request(route, init = {}, options = {}) {
    const s = this.state;
    const method = init.method ?? "GET";
    const url = new URL(route, "https://fixture.invalid");
    const pathname = url.pathname;
    const role = Object.entries(this.plan.workers).find(([, w]) => pathname.includes(w.name))?.[0];
    const call = { method, pathname, search: url.search, role };
    s.calls.push(call);
    await s.options.before?.(call, s, init);
    if (init.signal?.aborted) throw new Error("cancelled");
    if (pathname === "/tokens/verify")
      return {
        id: this.plan.tokenId,
        status: "active",
        expires_on: new Date(start + 86400000).toISOString(),
        ...s.options.credential,
      };
    if (pathname === "/workers/scripts") {
      const result = [
        ...Object.values(this.plan.workers)
          .filter((w) => s.workers[w.name])
          .map((w) => ({ id: w.name })),
        ...(s.options.foreignScripts ?? []).map((id) => ({ id })),
      ];
      return options.envelope ? { result, result_info: { total_pages: 1 } } : result;
    }
    if (["/workers/durable_objects/namespaces", "/workers/domains"].includes(pathname)) {
      const result = pathname.includes("namespaces") ? (s.options.namespaces ?? []) : [];
      return options.envelope ? { result, result_info: { total_pages: 1 } } : result;
    }
    if (pathname === "/workers/subdomain") return { subdomain: "fixture" };
    if (s.options.foreignScripts?.some((name) => pathname === `/workers/scripts/${name}/settings`))
      return {
        bindings: s.options.incoming
          ? [{ type: "service", service: this.plan.workers.caller.name }]
          : [],
      };
    assert.ok(role, `unexpected route ${route}`);
    const name = this.plan.workers[role].name;
    if (method === "PUT") {
      assert.ok(s.saved.at(-1).intents[role].uncertain, "durable intent precedes upload");
      const metadata = JSON.parse(await init.body.get("metadata").text());
      const module = Buffer.from(await init.body.get(metadata.main_module).arrayBuffer());
      assert.equal(digest(module), this.plan.workers[role].moduleSha256);
      if (s.options.lostUploadAbsent === role) throw new Error("upload response lost");
      s.workers[name] = {
        metadata,
        module,
        exposure: { enabled: true, previews_enabled: true },
        version:
          role === "receiver"
            ? "11111111-1111-4111-8111-111111111111"
            : "22222222-2222-4222-8222-222222222222",
        created: new Date(s.time).toISOString(),
      };
      if (s.options.lostUpload === role) throw new Error("upload response lost");
      return { id: name };
    }
    const w = s.workers[name];
    if (!w) {
      if (options.allow404) return undefined;
      throw new Error("absent");
    }
    const bindings = () =>
      w.metadata.bindings.map(({ text, ...b }) =>
        b.type === "service"
          ? {
              ...b,
              ...(s.options.keepProps ? {} : { props: undefined }),
              environment: "production",
              ...s.options.bindingOverride,
            }
          : b,
      );
    if (method === "DELETE") {
      assert.equal(url.search, "", "never force deletion");
      assert.ok(s.saved.at(-1).intents[role].deleteIntent, "durable delete intent");
      if (s.options.refuseDelete === role) throw new Error("references refuse deletion");
      delete s.workers[name];
      if (s.options.lostDelete === role) throw new Error("delete reply lost");
      return {};
    }
    if (pathname.endsWith("/settings")) return { ...w.metadata, bindings: bindings() };
    if (pathname === `/workers/services/${name}`)
      return { default_environment: { environment: "production" } };
    if (pathname.endsWith("/deployments"))
      return { deployments: [{ versions: [{ version_id: w.version, percentage: 100 }] }] };
    if (pathname.endsWith(`/versions/${w.version}`))
      return {
        id: w.version,
        number: 1,
        metadata: { author_id: this.plan.tokenId, created_on: w.created },
        resources: { bindings: bindings() },
      };
    if (pathname.endsWith("/versions")) return { items: [{ id: w.version }] };
    if (pathname.endsWith("/content/v2"))
      return { module: w.metadata.main_module, count: 1, sha256: digest(w.module) };
    if (pathname.endsWith("/subdomain")) {
      if (method === "POST") {
        w.exposure = JSON.parse(init.body);
        if (s.options.lostEnable && w.exposure.enabled) throw new Error("enable response lost");
      }
      return clone(w.exposure);
    }
    if (pathname.endsWith("/routes")) return [];
    if (pathname.endsWith("/schedules")) return { schedules: [] };
    if (pathname.endsWith("/bindings")) {
      if (s.options.denyEnvironment) throw new Error("denied");
      return bindings();
    }
    assert.fail(`unhandled ${method} ${route}`);
  }
}

async function fixture(options = {}) {
  const p = plan();
  const cf = new MockCloudflare(p, options);
  const ledger = { planHash: digest(canonical(p)), intents: {} };
  const save = async (value) => {
    cf.state.saved.push(clone(value));
  };
  const invoke = async (url, init) => {
    cf.state.invocations++;
    assert.equal(url, `https://${p.workers.caller.name}.fixture.workers.dev/observe`);
    assert.equal(init.headers.authorization, `Bearer ${key}`);
    assert.equal(init.redirect, "error");
    await options.invoke?.(cf.state, init);
    return Response.json(
      options.runtime ?? { protocol: "qualification-binding-probe-v1", props: p.props },
    );
  };
  const runOptions = { token: key, save, now: () => cf.state.time, invoke, signal: options.signal };
  const run = () => bindingProbe(cf, p, ledger, runOptions);
  const recover = () => bindingProbe(cf, p, ledger, { ...runOptions, mode: "recover" });
  return { p, cf, ledger, run, recover };
}

test("same-version omitted props observation settles both stateless Workers without force", async () => {
  const f = await fixture();
  await f.run();
  assert.equal(f.ledger.failed, undefined);
  assert.equal(f.ledger.observation.runtimePropsMatch, true);
  assert.equal(f.ledger.observation.complete, true);
  assert.equal(f.ledger.settled, true);
  assert.deepEqual(Object.keys(f.cf.state.workers), []);
  assert.equal(f.cf.state.invocations, 1);
  assert.deepEqual(
    f.cf.state.calls.filter((c) => c.method === "PUT").map((c) => c.role),
    ["receiver", "caller"],
  );
  assert.deepEqual(
    f.cf.state.calls.filter((c) => c.method === "DELETE").map((c) => c.role),
    ["caller", "receiver"],
  );
  assert.ok(!JSON.stringify(f.ledger).includes(key));
  assert.ok(
    !f.cf.state.calls.some((c) => /authority|qualification-controller|aws/.test(c.pathname)),
  );
});

for (const options of [
  { keepProps: true },
  { runtime: { protocol: "qualification-binding-probe-v1", props: {} } },
  { bindingOverride: { props: { altered: true } } },
])
  test(`characterizes props without trusting them for cleanup ${JSON.stringify(options)}`, async () => {
    const f = await fixture(options);
    await f.run();
    assert.equal(f.ledger.failed, undefined);
    assert.equal(f.ledger.settled, true);
    assert.equal(f.ledger.observation.runtimePropsMatch, !options.runtime);
  });

test("denied alternate read remains incomplete while cleanup succeeds", async () => {
  const f = await fixture({ denyEnvironment: true });
  await f.run();
  assert.equal(f.ledger.observation.complete, false);
  assert.equal(f.ledger.settled, true);
});

for (const role of ["receiver", "caller"])
  test(`lost ${role} upload reconciles exact identity, no repeat upload`, async () => {
    const f = await fixture({ lostUpload: role });
    await f.run();
    assert.equal(f.ledger.failed, true);
    assert.equal(f.ledger.settled, true);
    assert.equal(f.cf.state.invocations, 0);
    assert.equal(f.cf.state.calls.filter((c) => c.method === "PUT" && c.role === role).length, 1);
  });

for (const role of ["receiver", "caller"])
  test(`absent uncertain ${role} upload stays unknown through repeated recovery`, async () => {
    const f = await fixture({ lostUploadAbsent: role });
    await f.run();
    assert.equal(f.ledger.settled, false);
    assert.equal(f.ledger.intents[role].settled, undefined);
    const puts = f.cf.state.calls.filter((c) => c.method === "PUT").length;
    await f.recover();
    assert.equal(f.ledger.settled, false);
    assert.equal(f.cf.state.calls.filter((c) => c.method === "PUT").length, puts);
    assert.equal(f.cf.state.invocations, 0);
  });

for (const option of [{ lostDelete: "caller" }, { lostEnable: true }])
  test(`uncertain acknowledged state reconciles ${JSON.stringify(option)}`, async () => {
    const f = await fixture(option);
    await f.run();
    assert.equal(f.ledger.settled, true);
  });

test("refused deletion retains caller and its receiver, then recovery settles without invoking or recreating", async () => {
  const f = await fixture({ refuseDelete: "caller" });
  await f.run();
  assert.equal(f.ledger.settled, false);
  assert.equal(Object.keys(f.cf.state.workers).length, 2);
  f.cf.state.options.refuseDelete = undefined;
  const before = f.cf.state.calls.length;
  await f.recover();
  assert.equal(f.ledger.settled, true);
  assert.ok(!f.cf.state.calls.slice(before).some((c) => c.method === "PUT" || c.method === "POST"));
  await f.recover();
  assert.equal(f.ledger.settled, true);
});

test("cancellation during invocation preserves cleanup reserve and settles", async () => {
  const controller = new AbortController();
  const f = await fixture({
    signal: controller.signal,
    invoke() {
      controller.abort();
      throw new Error("cancel");
    },
  });
  await f.run();
  assert.equal(f.ledger.failed, true);
  assert.equal(f.ledger.settled, true);
});

test("forward deadline interrupts observation but settlement retains original cycle", async () => {
  const f = await fixture({
    invoke(s) {
      s.time = start + 10 * 60000;
    },
  });
  await f.run();
  assert.equal(f.ledger.failed, true);
  assert.equal(f.ledger.settled, true);
  assert.equal(f.ledger.withinOriginalBound, true);
});

for (const defect of ["source", "version", "binding", "incoming", "namespace", "inventory-denied"])
  test(`changed ${defect} blocks unsafe deletion`, async () => {
    const f = await fixture({
      invoke(s) {
        const w = s.workers[f.p.workers.caller.name];
        if (defect === "source") w.module = Buffer.from("foreign");
        if (defect === "version") w.version = "33333333-3333-4333-8333-333333333333";
        if (defect === "binding")
          w.metadata.bindings.push({
            name: "PRIVILEGED",
            type: "durable_object_namespace",
            namespace_id: "d".repeat(32),
          });
        if (defect === "incoming") {
          s.options.foreignScripts = ["foreign"];
          s.options.incoming = true;
        }
        if (defect === "namespace")
          s.options.namespaces = [{ id: "d".repeat(32), script: f.p.workers.caller.name }];
        if (defect === "inventory-denied")
          s.options.before = (c) => {
            if (c.pathname === "/workers/scripts") throw new Error("denied");
          };
      },
    });
    await f.run();
    assert.equal(f.ledger.settled, false);
    assert.ok(f.cf.state.workers[f.p.workers.caller.name]);
    assert.equal(f.cf.state.calls.filter((c) => c.method === "DELETE").length, 0);
  });

for (const override of [
  { environment: "foreign" },
  { service: "production-service" },
  { entrypoint: "Admin" },
  { namespace: "foreign" },
])
  test(`capability-coordinate mismatch refuses cleanup ${JSON.stringify(override)}`, async () => {
    const f = await fixture({ bindingOverride: override });
    await f.run();
    assert.equal(f.ledger.failed, true);
    assert.equal(f.ledger.settled, false);
    assert.equal(f.cf.state.invocations, 0);
  });

test("expired/wrong token and name collisions fail before effects", async () => {
  for (const credential of [
    { id: "f".repeat(32) },
    { status: "disabled" },
    { expires_on: new Date(start).toISOString() },
  ]) {
    const f = await fixture({ credential });
    await assert.rejects(f.run(), /credential/);
    assert.equal(f.cf.state.calls.filter((c) => c.method !== "GET").length, 0);
  }
  const f = await fixture();
  f.cf.state.options.foreignScripts = [f.p.workers.receiver.name];
  await assert.rejects(f.run(), /collision/);
  assert.equal(Object.keys(f.ledger.intents).length, 0);
});

test("run is single-use and source/plan tampering fails closed", async () => {
  const f = await fixture();
  await f.run();
  await assert.rejects(f.run(), /cannot be retried/);
  const g = await fixture();
  g.p.workers.receiver.moduleSha256 = "0".repeat(64);
  await assert.rejects(g.run(), /changed frozen plan/);
  assert.equal(g.cf.state.calls.length, 0);
});

test("bounded streamed bodies preserve multipart identity and reject excess", async () => {
  const content = Buffer.from("// unicode: 🦀\nexport default {};");
  const form = new FormData();
  form.set("module.mjs", new Blob([content]), "module.mjs");
  assert.deepEqual(await probeContentIdentity(new Response(form)), {
    module: "module.mjs",
    count: 1,
    sha256: digest(content),
  });
  await assert.rejects(readProbeResponse(new Response("x".repeat(8193))), /exceeds bound/);
});

test("journal is private, durable, exclusive and rejects symlink custody", async (t) => {
  const base = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "binding-probe-test-")));
  t.after(() => fs.rmSync(base, { recursive: true, force: true }));
  const dir = path.join(base, "journal");
  const p = plan();
  await withBindingProbeJournal(dir, p, async (ledger, save) => {
    ledger.marker = "committed";
    await save(ledger);
    assert.equal(
      JSON.parse(fs.readFileSync(path.join(dir, "binding-probe.json"))).marker,
      "committed",
    );
    assert.equal(fs.statSync(path.join(dir, "binding-probe.json")).mode & 0o777, 0o600);
    await assert.rejects(
      withBindingProbeJournal(dir, p, () => {}),
      /EEXIST/,
    );
  });
  await withBindingProbeJournal(dir, p, async (ledger) => assert.equal(ledger.marker, "committed"));
  const link = path.join(base, "link");
  fs.symlinkSync(dir, link);
  await assert.rejects(
    withBindingProbeJournal(link, p, () => {}),
    /symlinks/,
  );
});

test("caller authentication and routing cannot select other capabilities", async () => {
  let calls = 0;
  const env = {
    PROBE_TOKEN: key,
    RECEIVER: {
      describe() {
        calls++;
        return { props: "synthetic" };
      },
    },
  };
  for (const [method, route, auth, status] of [
    ["POST", "/observe", "", 403],
    ["POST", "/observe", `Bearer ${"b".repeat(64)}`, 403],
    ["GET", "/observe", `Bearer ${key}`, 405],
    ["POST", "/admin", `Bearer ${key}`, 405],
    ["POST", "/observe", `Bearer ${key}`, 200],
  ]) {
    const r = await caller.fetch(
      new Request(`https://fixture.invalid${route}`, { method, headers: { authorization: auth } }),
      env,
    );
    assert.equal(r.status, status);
  }
  assert.equal(calls, 1);
});

test("full account readback fits the reserved request budget without foreign writes", async () => {
  const f = await fixture({
    foreignScripts: Array.from({ length: 54 }, (_, n) => `existing-${n}`),
  });
  await f.run();
  assert.equal(f.ledger.failed, undefined);
  assert.equal(f.ledger.settled, true);
  assert.ok(f.cf.state.calls.length < f.p.maxRequests);
  assert.ok(f.cf.state.calls.filter((c) => c.method !== "GET").every((c) => c.role));
});

test("shared transport keeps ordinary JSON behavior and bounds raw module identity", async (t) => {
  const original = globalThis.fetch;
  t.after(() => {
    globalThis.fetch = original;
  });
  const source = Buffer.from("export default {};\n");
  globalThis.fetch = async (url, init) => {
    assert.equal(init.headers.authorization, "Bearer synthetic-only");
    if (url.endsWith("/settings"))
      return Response.json({ success: true, result: { bindings: [] } });
    const form = new FormData();
    form.set("module.mjs", new Blob([source]), "module.mjs");
    return new Response(form);
  };
  const cf = new Cloudflare("a".repeat(32), "synthetic-only");
  assert.deepEqual(await cf.request("/settings"), { bindings: [] });
  assert.deepEqual(await cf.request("/content/v2", {}, { probeContentIdentity: true }), {
    module: "module.mjs",
    count: 1,
    sha256: digest(source),
  });
});

test("late recovery records a missed original deadline without forward work", async () => {
  const f = await fixture({ refuseDelete: "caller" });
  await f.run();
  f.cf.state.options.refuseDelete = undefined;
  f.cf.state.time = Date.parse(f.p.deadline) + 60000;
  const before = f.cf.state.calls.length;
  await f.recover();
  assert.equal(f.ledger.settled, true);
  assert.equal(f.ledger.withinOriginalBound, false);
  assert.equal(f.p.deadline, new Date(start + 20 * 60000).toISOString());
  assert.ok(!f.cf.state.calls.slice(before).some((c) => c.method === "PUT" || c.method === "POST"));
});
