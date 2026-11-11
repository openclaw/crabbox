import assert from "node:assert/strict";
import test from "node:test";
import { Cloudflare } from "./image-qualification-control.mjs";
import {
  bindingShape,
  canonical,
  digest,
  readAdmission,
  verifyAuthority,
  requireAbsent,
  verifyOwned,
  authorityName,
  controllerName,
} from "./image-qualification-isolation.mjs";
import { fixtureAdmission, ownedSettings, version } from "./fixtures/qualification-isolation.mjs";

function fixture() {
  const f = fixtureAdmission();
  const worker = "crabbox-image-qualification-42-1";
  const transport = {
    name: "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
    type: "service",
    service: authorityName,
    props: {
      runId: "image-qualification-42-1",
      candidateWorker: worker,
      deploymentHash: "b".repeat(64),
    },
  };
  const fleet = {
    name: "FLEET",
    type: "durable_object_namespace",
    class_name: "FleetDurableObject",
    namespace_id: "3".repeat(32),
  };
  const state = {
    version,
    workers: {
      [authorityName]: { bindings: f.bindings },
      [worker]: ownedSettings(f.admission, worker, [transport, fleet]),
    },
    namespaces: [
      ...f.record.authority.namespaces,
      { script: worker, class: "FleetDurableObject", id: fleet.namespace_id },
    ],
  };
  const events = [];
  const cf = {
    admission: f.admission,
    settings: async (name) => state.workers[name],
    latestVersion: async () => state.version,
    list: async (route) => (route.startsWith("/workers/durable_objects") ? state.namespaces : []),
    request: async (route, init = {}) => {
      events.push(`${init.method ?? "GET"} ${route}`);
      assert.equal(init.method, undefined, "isolation admission must be read only");
      if (route.endsWith("/settings")) return state.workers[route.split("/")[3]];
      if (route.endsWith("/subdomain")) return { enabled: false, previews_enabled: false };
      if (route.endsWith("/schedules")) return { schedules: [] };
      return [];
    },
  };
  return { ...f, worker, transport, fleet, state, events, cf };
}

test("admission binds exact qualification identities and production exclusions", () => {
  const f = fixture();
  assert.equal(
    readAdmission({ ...f.env, CLOUDFLARE_ACCOUNT_ID: f.record.accountId }).sha256,
    f.admission.sha256,
  );
  assert.throws(() => readAdmission({}), /record missing/);
  assert.throws(
    () => readAdmission({ ...f.env, CLOUDFLARE_ACCOUNT_ID: "0".repeat(32) }),
    /invalid admitted/,
  );
  assert.throws(
    () =>
      readAdmission({
        ...f.env,
        CLOUDFLARE_ACCOUNT_ID: f.record.accountId,
        QUALIFICATION_SUBNET_ID: "subnet-99999999",
      }),
    /differs from admission/,
  );
});

for (const [key, identity] of [
  ["workers", authorityName],
  ["workers", controllerName],
  ["workers", "crabbox-image-qualification-42-1"],
  ["namespaceIds", "1".repeat(32)],
  ["roleArns", "arn:aws:iam::123456789012:role/Qualification"],
  ["subnetIds", "subnet-00000000000000000"],
  ["securityGroupIds", "sg-00000000000000000"],
])
  test(`production ${key} collision fails closed`, () => {
    const f = fixture();
    f.record.production[key].push(identity);
    assert.throws(
      () =>
        readAdmission({
          CLOUDFLARE_ACCOUNT_ID: f.record.accountId,
          QUALIFICATION_ISOLATION_JSON: JSON.stringify(f.record),
          QUALIFICATION_ISOLATION_SHA256: digest(canonical(f.record)),
        }),
      /overlap/,
    );
  });

test("live authority version, settings, private ingress and owned namespaces are checked", async () => {
  const f = fixture();
  await verifyAuthority(f.cf);
  await verifyOwned(f.cf, f.worker);
  assert.ok(f.events.every((event) => event.startsWith("GET ")));
});

for (const [name, change] of [
  [
    "wrong active authority version",
    (f) => {
      f.state.version = "22222222-2222-4222-8222-222222222222";
    },
  ],
  [
    "production broker binding",
    (f) => {
      f.state.workers[authorityName].bindings.push({
        name: "BROKER",
        type: "service",
        service: "production-coordinator",
      });
    },
  ],
  [
    "production namespace",
    (f) => {
      f.state.workers[authorityName].bindings[9].namespace_id = "9".repeat(32);
    },
  ],
  [
    "extra namespace class",
    (f) => {
      f.state.namespaces.push({ id: "4".repeat(32), script: authorityName, class: "Unexpected" });
    },
  ],
  [
    "wrong namespace owner",
    (f) => {
      f.state.namespaces[0].script = "production-coordinator";
    },
  ],
])
  test(`${name} stops admission without writes`, async () => {
    const f = fixture();
    change(f);
    await assert.rejects(verifyAuthority(f.cf), /qualification isolation/);
    assert.ok(f.events.every((event) => event.startsWith("GET ")));
  });

for (const [name, change] of [
  [
    "missing ownership marker",
    (f) => {
      f.state.workers[f.worker].tags = [];
    },
  ],
  [
    "production namespace substitution",
    (f) => {
      f.fleet.namespace_id = "9".repeat(32);
    },
  ],
  [
    "external namespace owner",
    (f) => {
      f.fleet.script_name = "production-coordinator";
    },
  ],
  [
    "external namespace environment",
    (f) => {
      f.fleet.environment = "production";
    },
  ],
  [
    "candidate name/run mismatch",
    (f) => {
      f.transport.props.runId = "image-qualification-99-1";
    },
  ],
  [
    "production service",
    (f) => {
      f.transport.service = "production-coordinator";
    },
  ],
  [
    "production service environment",
    (f) => {
      f.transport.environment = "production";
    },
  ],
  [
    "unexpected secret",
    (f) => {
      f.state.workers[f.worker].bindings.push({
        name: "AWS_SECRET_ACCESS_KEY",
        type: "secret_text",
      });
    },
  ],
  [
    "dispatch namespace",
    (f) => {
      f.state.workers[f.worker].bindings.push({
        name: "BROKER",
        type: "dispatch_namespace",
        namespace: "production",
      });
    },
  ],
  [
    "extra owned namespace class",
    (f) => {
      f.state.namespaces.push({ script: f.worker, class: "Other", id: "5".repeat(32) });
    },
  ],
])
  test(`cleanup refuses ${name}`, async () => {
    const f = fixture();
    change(f);
    // Even accidentally restamped metadata cannot authorize a production target.
    if (name !== "missing ownership marker")
      f.state.workers[f.worker] = ownedSettings(
        f.admission,
        f.worker,
        f.state.workers[f.worker].bindings,
      );
    await assert.rejects(verifyOwned(f.cf, f.worker), /qualification isolation/);
    assert.ok(f.events.every((event) => event.startsWith("GET ")));
  });

test("existing names and orphan namespaces cannot be adopted", async () => {
  const f = fixture();
  await assert.rejects(requireAbsent(f.cf, f.worker), /name collision/);
  delete f.state.workers[f.worker];
  await assert.rejects(requireAbsent(f.cf, f.worker), /name collision/);
  await assert.rejects(verifyOwned(f.cf, f.worker, { allowAbsent: true }), /residue/);
});

test("binding digest retains every non-secret resource coordinate", () => {
  const binding = {
    name: "FLEET",
    type: "durable_object_namespace",
    class_name: "FleetDurableObject",
    namespace_id: "1".repeat(32),
    script_name: "external",
    environment: "production",
    dispatch_namespace: "production",
  };
  assert.deepEqual(bindingShape([binding]), [binding]);
  assert.deepEqual(bindingShape([{ name: "TOKEN", type: "secret_text", text: "synthetic" }]), [
    { name: "TOKEN", type: "secret_text" },
  ]);
});

test("Cloudflare script inventory reads the unpaginated endpoint once for large shared accounts", async (t) => {
  const original = globalThis.fetch;
  t.after(() => {
    globalThis.fetch = original;
  });
  const calls = [];
  const workers = Array.from({ length: 125 }, (_, index) => ({ id: `unrelated-${index}` }));
  globalThis.fetch = async (url) => {
    calls.push(url);
    assert.equal(new URL(url).search, "");
    return new Response(JSON.stringify({ success: true, result: workers }));
  };
  const cf = new Cloudflare("a".repeat(32), "synthetic");
  assert.deepEqual(await cf.list("/workers/scripts"), workers);
  assert.equal(calls.length, 1);
});

test("Cloudflare namespace inventory follows result_info through the final page", async (t) => {
  const original = globalThis.fetch;
  t.after(() => {
    globalThis.fetch = original;
  });
  const pages = [];
  globalThis.fetch = async (value) => {
    const page = Number(new URL(value).searchParams.get("page"));
    pages.push(page);
    return new Response(
      JSON.stringify({
        success: true,
        result: [{ id: String(page) }],
        result_info: { page, per_page: 100, total_pages: 2 },
      }),
    );
  };
  const cf = new Cloudflare("a".repeat(32), "synthetic");
  assert.deepEqual(await cf.list("/workers/durable_objects/namespaces"), [
    { id: "1" },
    { id: "2" },
  ]);
  assert.deepEqual(pages, [1, 2]);
});

for (const schedules of [[], {}, { schedules: [{ cron: "* * * * *" }] }])
  test(`authority schedule shape fails closed: ${JSON.stringify(schedules)}`, async () => {
    const f = fixture();
    const request = f.cf.request;
    f.cf.request = async (route, ...args) =>
      route.endsWith("/schedules") ? schedules : request(route, ...args);
    await assert.rejects(verifyAuthority(f.cf), /authority is public/);
  });
