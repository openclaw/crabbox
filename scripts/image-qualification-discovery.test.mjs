import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { Cloudflare, controllerCall } from "./image-qualification-control.mjs";
import {
  ControllerHTTPError,
  observeControllerDiscovery,
  requestControllerJSON,
} from "./image-qualification-controller-http.mjs";
import { authorityName, controllerName } from "./image-qualification-isolation.mjs";
import {
  fixtureAdmission,
  ownedSettings,
  controllerContent,
  version,
} from "./fixtures/qualification-isolation.mjs";

const marker = "synthetic-private-response";
const html404 = () =>
  new Response(`<!doctype html><p>${marker}</p>`, {
    status: 404,
    headers: { "content-type": "text/html", "cf-ray": "0123456789abcdef-DFW" },
  });
const url = "https://controller.invalid";
const token = "synthetic-controller-token";
const immediate = async () => {};
const discoveryRequest = (invoke, observation) =>
  requestControllerJSON({
    url,
    token,
    action: "discover",
    ...observation,
    invoke,
  });

function fixture(t, reply, { separateAuthority = false, management } = {}) {
  const { admission, bindings } = fixtureAdmission();
  const cf = new Cloudflare(admission.accountId, "synthetic-management-token");
  cf.admission = admission;
  const authorityCF = separateAuthority
    ? Object.assign(new Cloudflare(admission.accountId, "synthetic-authority-token"), { admission })
    : cf;
  const settings = ownedSettings(admission, controllerName, [
    { name: "CONTROLLER_TOKEN", type: "secret_text" },
    {
      name: "AUTHORITY",
      type: "service",
      service: authorityName,
      entrypoint: "AWSQualificationController",
      props: { deploymentHash: "d".repeat(64) },
    },
  ]);
  const state = {
    authorityVersion: version,
    controllerVersion: version,
    settings,
    rpc: 0,
    gets: [],
  };
  const source = fs.readFileSync(
    new URL("./image-qualification-controller-worker.mjs", import.meta.url),
  );
  t.mock.method(globalThis, "fetch", async (input, init = {}) => {
    const u = new URL(input);
    if (u.origin === url) {
      assert.equal(init.method, "POST");
      assert.equal(init.headers.authorization, `Bearer ${token}`);
      state.rpc++;
      return await reply(u.pathname, init, state);
    }
    assert.equal(u.origin, "https://api.cloudflare.com", "all other egress forbidden");
    assert.equal(init.method ?? "GET", "GET", "no management mutation allowed");
    const route = u.pathname.split(`/accounts/${admission.accountId}`)[1];
    assert(route);
    state.gets.push(route);
    if (management) {
      const response = await management(route, init, state);
      if (response) return response;
    }
    const m = /^\/workers\/scripts\/([^/]+)(.*)$/.exec(route);
    const name = m?.[1],
      tail = m?.[2];
    if (tail === "/content/v2") {
      assert.equal(name, controllerName);
      return controllerContent(source);
    }
    let result;
    if (tail === "/settings") result = name === authorityName ? { bindings } : state.settings;
    else if (tail === "/deployments")
      result = {
        deployments: [
          {
            versions: [
              {
                version_id:
                  name === authorityName ? state.authorityVersion : state.controllerVersion,
                percentage: 100,
              },
            ],
          },
        ],
      };
    else if (route.startsWith("/workers/durable_objects/namespaces"))
      result = admission.authority.namespaces;
    else if (tail === "/subdomain") result = { enabled: false, previews_enabled: false };
    else if (tail === "/schedules") result = { schedules: [] };
    else if (route.includes("/routes") || route.includes("/domains")) result = [];
    else assert.fail(`unexpected management route ${route}`);
    return Response.json({ success: true, result });
  });
  return {
    cf,
    authorityCF,
    state,
    call: (action = "discover", value = {}) =>
      controllerCall(cf, url, token, action, value, authorityCF),
  };
}

test("HTML404 discovery is observed again with the same immutable deadline and no reflected bytes", async () => {
  let calls = 0;
  const deadlines = [],
    receipts = [];
  const result = await observeControllerDiscovery(
    async (observation) => {
      deadlines.push(observation.deadline);
      try {
        return await discoveryRequest(
          async () => (++calls < 3 ? html404() : Response.json({ run: null })),
          observation,
        );
      } catch (e) {
        receipts.push(e.receipt);
        throw e;
      }
    },
    { pause: immediate },
  );
  assert.deepEqual(result, { run: null });
  assert.equal(calls, 3);
  assert.equal(new Set(deadlines).size, 1);
  assert.equal(receipts.length, 2);
  assert(!JSON.stringify(receipts).includes(marker));
});

test("the default readiness budget is at most sixty seconds", async () => {
  const started = Date.now();
  await observeControllerDiscovery(async ({ deadline, signal }) => {
    assert(deadline <= Date.now() + 60_000);
    assert(deadline >= started + 60_000);
    assert(signal instanceof AbortSignal);
    return { run: null };
  });
});

test("persistent HTML404 stops at twelve observations and preserves the last failure", async () => {
  let calls = 0,
    last;
  await assert.rejects(
    observeControllerDiscovery(
      async (observation) => {
        calls++;
        try {
          return await discoveryRequest(async () => html404(), observation);
        } catch (e) {
          last = e;
          throw e;
        }
      },
      { pause: immediate },
    ),
    (e) => e === last,
  );
  assert.equal(calls, 12);
});

for (const [name, response] of [
  ["forbidden", () => Response.json({ error: marker }, { status: 403 })],
  ["conflict", () => Response.json({ error: marker }, { status: 409 })],
  ["JSON404", () => Response.json({ error: marker }, { status: 404 })],
  [
    "HTML503",
    () =>
      new Response("<!doctype html>", { status: 503, headers: { "content-type": "text/html" } }),
  ],
  ["malformed success", () => new Response("{")],
  ["wrong content type", () => new Response("<!doctype html>", { status: 404 })],
]) {
  test(`${name} fails immediately without another observation`, async () => {
    let calls = 0;
    await assert.rejects(
      observeControllerDiscovery(
        (observation) =>
          discoveryRequest(async () => {
            calls++;
            return response();
          }, observation),
        { pause: immediate },
      ),
      ControllerHTTPError,
    );
    assert.equal(calls, 1);
  });
}

test("arbitrary errors never cause observations to repeat", async () => {
  const failure = new Error("fixed local failure");
  let calls = 0;
  await assert.rejects(
    observeControllerDiscovery(
      async () => {
        calls++;
        throw failure;
      },
      {
        pause: immediate,
      },
    ),
    (e) => e === failure,
  );
  assert.equal(calls, 1);
});

test("expired deadline and cancellation prevent even management observations", async () => {
  for (const options of [{ deadline: Date.now() - 1 }, { signal: AbortSignal.abort() }]) {
    let calls = 0;
    await assert.rejects(
      observeControllerDiscovery(async () => calls++, options),
      /cancelled|deadline/,
    );
    assert.equal(calls, 0);
  }
});

test("the original deadline bounds observation and is not reset after a failure", async () => {
  let calls = 0;
  const deadline = Date.now() + 30;
  await assert.rejects(
    observeControllerDiscovery(
      (observation) => {
        assert.equal(observation.deadline, deadline);
        calls++;
        return discoveryRequest(async () => html404(), observation);
      },
      { deadline },
    ),
  );
  assert.equal(calls, 1);
});

test("cancellation during the readiness pause prevents a second dispatch", async () => {
  const c = new AbortController();
  let calls = 0;
  await assert.rejects(
    observeControllerDiscovery(
      (observation) =>
        discoveryRequest(async () => {
          calls++;
          setTimeout(() => c.abort(), 10);
          return html404();
        }, observation),
      { signal: c.signal },
    ),
  );
  assert.equal(calls, 1);
});

test("a pending discovery body joins cancellation before returning", async () => {
  let cancelled = 0;
  await assert.rejects(
    observeControllerDiscovery(
      (observation) =>
        discoveryRequest(
          async () =>
            new Response(
              new ReadableStream({
                cancel() {
                  cancelled++;
                },
              }),
            ),
          observation,
        ),
      {
        deadline: Date.now() + 25,
      },
    ),
  );
  assert.equal(cancelled, 1);
});

test("actual control fences repeat before each RPC and retain sanitized failed observations", async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "qualification-discovery-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const previous = process.env.QUALIFICATION_PROOF_DIR;
  process.env.QUALIFICATION_PROOF_DIR = dir;
  t.after(() =>
    previous === undefined
      ? delete process.env.QUALIFICATION_PROOF_DIR
      : (process.env.QUALIFICATION_PROOF_DIR = previous),
  );
  let priorGets = 0;
  const f = fixture(
    t,
    async (route, _init, state) => {
      assert.equal(route, "/discover");
      const fresh = state.gets.slice(priorGets);
      assert(fresh.includes(`/workers/scripts/${authorityName}/settings`));
      assert(fresh.includes(`/workers/scripts/${controllerName}/content/v2`));
      priorGets = state.gets.length;
      return state.rpc === 1 ? html404() : Response.json({ run: null });
    },
    { separateAuthority: true },
  );
  assert.deepEqual(await f.call(), { run: null });
  assert.equal(f.state.rpc, 2);
  assert.equal(f.cf.discoverySignal, undefined);
  assert.equal(f.authorityCF.discoverySignal, undefined);
  const files = fs.readdirSync(dir);
  assert.equal(files.length, 1);
  const receipt = fs.readFileSync(path.join(dir, files[0]), "utf8");
  assert(!receipt.includes(marker));
  assert(!receipt.includes(token));
  assert.equal(JSON.parse(receipt).status, 404);
});

for (const drift of ["authorityVersion", "controllerVersion", "ownership"]) {
  test(`${drift} drift between observations blocks another runtime dispatch`, async (t) => {
    const f = fixture(t, async (_route, _init, state) => {
      if (drift === "ownership") state.settings.tags = [];
      else state[drift] = "22222222-2222-4222-8222-222222222222";
      return html404();
    });
    await assert.rejects(f.call(), /drifted|changed|unowned/);
    assert.equal(f.state.rpc, 1);
    assert.equal(f.cf.discoverySignal, undefined);
  });
}

for (const routeSuffix of ["/settings", "/content/v2"]) {
  test(`deadline cancels management fence ${routeSuffix} before runtime dispatch`, async (t) => {
    let aborted = false;
    const f = fixture(t, async () => assert.fail("RPC before fences"), {
      management: async (route, init) => {
        if (route.endsWith(routeSuffix))
          return await new Promise((_resolve, reject) => {
            assert(init.signal instanceof AbortSignal);
            init.signal.addEventListener(
              "abort",
              () => {
                aborted = true;
                reject(new Error("management cancelled"));
              },
              { once: true },
            );
          });
      },
    });
    f.cf.controllerDeadline = Date.now() + 30;
    await assert.rejects(f.call());
    assert.equal(aborted, true);
    assert.equal(f.state.rpc, 0);
    assert.equal(f.cf.discoverySignal, undefined);
  });
}

test("successful but invalid canonical discovery fails closed without a retry", async (t) => {
  const f = fixture(t, async () => Response.json({ run: null, unexpected: true }));
  await assert.rejects(f.call(), /registry discovery is invalid/);
  assert.equal(f.state.rpc, 1);
});

test("invalid discovery input is rejected before management or runtime dispatch", async (t) => {
  const f = fixture(t, async () => assert.fail("invalid input dispatched"));
  for (const value of [
    null,
    [],
    new Date(),
    "",
    { run: "other" },
    { [Symbol()]: true },
    Object.create(null),
  ])
    await assert.rejects(f.call("discover", value), /empty input/);
  assert.equal(f.state.rpc, 0);
  assert.equal(f.state.gets.length, 0);
});

for (const action of [
  "claim",
  "key-authorization",
  "attest",
  "finalize",
  "begin-finalization",
  "retire",
  "transport-open",
  "transport-discover",
  "transport-settle",
]) {
  test(`${action} remains single-shot even on HTML404`, async (t) => {
    const f = fixture(t, async () => html404());
    let signalCalls = 0;
    f.cf.recoverySignal = () => {
      signalCalls++;
      return new AbortController().signal;
    };
    const value =
      action === "claim"
        ? { identity: { expiresAt: new Date(Date.now() + 60_000).toISOString() } }
        : action === "transport-open"
          ? { expiresAt: new Date(Date.now() + 60_000).toISOString() }
          : {};
    await assert.rejects(f.call(action, value), ControllerHTTPError);
    assert.equal(f.state.rpc, 1);
    // Existing content-fence signal plus the original action-time signal.
    assert.equal(signalCalls, 2);
    assert.equal(f.cf.discoverySignal, undefined);
  });
}
