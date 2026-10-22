import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import { createRequire } from "node:module";
import os from "node:os";
import path from "node:path";

// Use the exact workerd/Miniflare dependency owned by the locked Wrangler install.
const require = createRequire(import.meta.url);
const wrangler = createRequire(require.resolve("wrangler/package.json"));
const { Miniflare } = wrangler("miniflare");
const output = await fs.mkdtemp(path.join(os.tmpdir(), "capacity-workerd-"));
execFileSync(process.execPath, [
  path.join(path.dirname(require.resolve("wrangler/package.json")), "bin/wrangler.js"),
  "deploy", "--dry-run", "--config", "test/fixtures/capacity-workerd.jsonc",
  "--outdir", output,
], { stdio: "inherit", env: { ...process.env, WRANGLER_SEND_METRICS: "false" } });
const bundles = (await fs.readdir(output)).filter((name) => name.endsWith(".js"));
assert.equal(bundles.length, 1);
const script = await fs.readFile(path.join(output, bundles[0]), "utf8");
const mf = new Miniflare({
  modules: true,
  script,
  compatibilityDate: "2026-04-30",
  compatibilityFlags: ["nodejs_compat"],
  cf: false,
  durableObjects: { CAPACITY: { className: "CapacityProof", useSQLite: true } },
});
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
try {
  const namespace = await mf.getDurableObjectNamespace("CAPACITY");
  const stub = namespace.get(namespace.idFromName("synthetic-proof"));
  const request = (route) => stub.fetch("https://fixture.test" + route);
  assert.equal((await request("/seed")).status, 200);
  const stable = await request("/capacity");
  assert.equal(stable.status, 200);
  assert.equal((await stable.json()).activeLeases, 1);

  const blocking = request("/blocking-control");
  await sleep(50);
  const controlStart = performance.now();
  await request("/heartbeat");
  const controlMs = performance.now() - controlStart;
  assert.ok(controlMs >= 200, "native input-gate control did not block event delivery");
  await blocking;

  for (const lifecycle of ["/heartbeat", "/finish"]) {
    await request("/pause");
    const pending = request("/capacity");
    await sleep(50);
    const start = performance.now();
    assert.equal((await request(lifecycle)).status, 200);
    assert.ok(performance.now() - start < 1000, "capacity held native event delivery");
    assert.equal((await pending).status, 503, "overlapping write was not rejected");
  }

  const responses = [];
  await Promise.all([
    (async () => {
      for (let i = 0; i < 40; i++) {
        const response = await request("/capacity");
        responses.push(response.status);
        if (response.status === 200) assert.equal((await response.json()).activeLeases, 1);
        else assert.equal(response.status, 503);
        await sleep(10);
      }
    })(),
    (async () => {
      for (let i = 0; i < 12; i++) {
        await request(i % 2 ? "/heartbeat" : "/finish");
        await sleep(30);
      }
    })(),
  ]);
  const successful = responses.filter((status) => status === 200).length;
  assert.ok(successful >= 20, "bounded mixed traffic starved capacity reads");
  const stats = await (await request("/stats")).json();
  assert.ok(stats.lists.every((prefix) => prefix !== "lease:"), "retained history scanned");
  assert.ok(stats.gets <= 100, "unexpected canonical hydration");
  console.log(JSON.stringify({
    status: "passed",
    source: process.env.GITHUB_SHA,
    bundleSHA256: createHash("sha256").update(script).digest("hex"),
    seeded: 5001,
    controlMs,
    mixedTraffic: { attempts: 40, successful, conflicts: 40 - successful },
    stats,
    nativeLifecycleEvents: ["heartbeat", "finish"],
    syntheticOnly: true,
  }));
} finally {
  await mf.dispose();
  await fs.rm(output, { recursive: true, force: true });
}
