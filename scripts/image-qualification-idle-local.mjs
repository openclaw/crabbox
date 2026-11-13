// Local only: exact runtime build and raw controller; all outbound denied.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { Miniflare } from "../worker/node_modules/miniflare/dist/src/index.js";
const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
if (process.argv.length !== 3)
  throw new Error("requires the reviewed private-authority bundle path");
const runtime = path.resolve(process.argv[2]);
const token = "a".repeat(64),
  challenge = "b".repeat(64),
  props = { deploymentHash: "c".repeat(64) };
let passed = 0;
for (const [name, bound, metadata, status] of [
  ["exact", props, true, 200],
  ["wrong-props", { ...props, extra: true }, true, 409],
  ["missing-version", props, false, 409],
]) {
  const worker = (name, file, env, flags = []) => ({
    config: {
      name,
      compatibilityDate: "2026-09-03",
      compatibilityFlags: flags,
      manifest: {
        mainModule: "worker.mjs",
        modules: { "worker.mjs": { type: "esm", contents: fs.readFileSync(file, "utf8") } },
      },
      env,
      exports: {},
    },
    dev: {
      cacheAPI: false,
      outboundService: {
        type: "fetcher",
        handler: () => {
          throw Error("outbound forbidden");
        },
      },
    },
  });
  const mf = new Miniflare({
    host: "127.0.0.1",
    port: 0,
    cf: false,
    telemetry: { enabled: false },
    workers: [
      worker("controller", `${root}/scripts/image-qualification-controller-worker.mjs`, {
        CONTROLLER_TOKEN: { type: "text", value: token },
        IDLE_CONTROLLER_ONLY: { type: "text", value: "1" },
        ...(metadata ? { CF_VERSION_METADATA: { type: "version-metadata" } } : {}),
        AUTHORITY: {
          type: "worker",
          worker: "authority",
          exportName: "AWSQualificationController",
          props: bound,
        },
      }),
      worker("authority", runtime, { CF_VERSION_METADATA: { type: "version-metadata" } }, [
        "nodejs_compat",
      ]),
    ],
  });
  try {
    const request = async (path, body, auth = token) =>
      mf.dispatchFetch(`http://fixture.invalid/${path}`, {
        method: "POST",
        headers: { authorization: `Bearer ${auth}` },
        body: JSON.stringify(body),
      });
    assert.equal((await request("binding-identity", { challenge }, "wrong")).status, 403);
    passed++;
    assert.equal((await request("claim", {})).status, 404);
    passed++;
    assert.equal((await request("binding-identity", { challenge, expected: props })).status, 409);
    passed++;
    const response = await request("binding-identity", { challenge });
    assert.equal(response.status, status);
    passed++;
    if (status === 200) {
      const reply = await response.json();
      assert.deepEqual(reply.identity.props, props);
      assert.equal(reply.identity.challenge, challenge);
      assert.equal(reply.identity.role, "controller");
      assert.equal(reply.identity.protocol, 1);
      assert.match(reply.identity.authorityVersion, /^[a-f0-9-]{36}$/);
      assert.match(reply.controllerVersion, /^[a-f0-9-]{36}$/);
      assert.equal(response.headers.get("cache-control"), "no-store");
      passed++;
    }
  } finally {
    await mf.dispose();
  }
}
console.log(
  JSON.stringify({
    passed,
    failed: 0,
    exactRuntimeBundle: runtime,
    exactController: true,
    cloud: false,
    network: "loopback only; all Worker outbound denied",
    registryBindings: "absent: pure identity must not access them",
    liveProviderProof: false,
  }),
);
