import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";

import { bindingName, policyDigest } from "./verify-coordinator-lease-policy.mjs";

// Execute the shipped CLI and pinned Wrangler with real HTTP/multipart I/O to a
// loopback API fixture. All credentials, identities and resources are synthetic;
// this proves the upload contract, not a live Cloudflare deployment.
const repo = path.resolve(import.meta.dirname, "..");
const account = "a".repeat(32);
const prefix = `/client/v4/accounts/${account}/workers/scripts/crabbox-coordinator`;
const originalVersion = "11111111-1111-4111-8111-111111111111";
const uploadedVersion = "22222222-2222-4222-8222-222222222222";
const originalDeployment = "33333333-3333-4333-8333-333333333333";
const uploadedDeployment = "44444444-4444-4444-8444-444444444444";
const policy = {
  owners: { "github:1000000099": { maxTTLSeconds: 43200, autoExtend: "deny" } },
  orgs: { "example-org": { defaultTTLSeconds: 2400, defaultIdleTimeoutSeconds: 900 } },
};
const binding = (doc) => ({ name: bindingName, type: "plain_text", text: JSON.stringify(doc) });

async function fixture(t, options = {}) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "coordinator-wire-test-"));
  const worker = path.join(directory, "worker");
  fs.mkdirSync(path.join(directory, "scripts"));
  fs.mkdirSync(path.join(worker, "src"), { recursive: true });
  fs.mkdirSync(path.join(directory, "home"));
  fs.symlinkSync(path.join(repo, "worker/node_modules"), path.join(worker, "node_modules"), "dir");
  for (const file of [
    "scripts/deploy-coordinator.mjs",
    "scripts/verify-coordinator-lease-policy.mjs",
    "worker/src/lease-policy-overrides.mjs",
  ])
    fs.copyFileSync(path.join(repo, file), path.join(directory, file));
  fs.writeFileSync(
    path.join(worker, "package.json"),
    JSON.stringify({ type: "module", scripts: { predeploy: 'node -e "process.exit(0)"' } }),
  );
  fs.writeFileSync(
    path.join(worker, "src/index.js"),
    'export default { fetch() { return new Response("fixture"); } };',
  );
  const source = JSON.stringify({
    name: "crabbox-coordinator",
    main: "src/index.js",
    compatibility_date: "2026-04-30",
    workers_dev: false,
    preview_urls: false,
    vars: { CRABBOX_LEASE_MAX_TTL_SECONDS: "14400", GIT_OWNED_SETTING: "new-git-value" },
  });
  fs.writeFileSync(path.join(worker, "wrangler.jsonc"), source);
  const requests = [];
  const unexpected = [];
  const uploads = [];
  const errors = [];
  const state = {
    version: originalVersion,
    deployment: originalDeployment,
    bindings: options.absent ? [] : [binding(policy)],
    tags: [],
  };
  const versions = new Map([[originalVersion, { bindings: structuredClone(state.bindings) }]]);
  let guardReads = 0;
  const server = http.createServer((req, res) => {
    (async () => {
      const route = new URL(req.url, "http://fixture").pathname;
      requests.push({ method: req.method, route });
      assert.equal(req.headers.authorization, "Bearer synthetic-wire-token");
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const body = Buffer.concat(chunks);
      let result;
      if (req.method === "GET" && route === prefix + "/deployments") {
        guardReads++;
        if (options.revoke && guardReads === 3) {
          state.bindings = [];
          state.version = uploadedVersion;
          state.deployment = uploadedDeployment;
          versions.set(uploadedVersion, { bindings: [] });
        }
        result = {
          deployments: [
            {
              id: state.deployment,
              versions: [{ version_id: state.version, percentage: 100 }],
              source: "wrangler",
            },
          ],
        };
      } else if (req.method === "GET" && route === prefix + "/settings") {
        result = { bindings: state.bindings, tags: state.tags };
      } else if (req.method === "GET" && route.startsWith(prefix + "/versions/")) {
        const id = route.split("/").at(-1);
        assert.ok(versions.has(id));
        result = {
          id,
          resources: { bindings: versions.get(id).bindings, script: { etag: "fixture-etag" } },
          metadata: { author_email: "operator@example.com", source: "wrangler" },
          annotations: {},
        };
      } else if (
        req.method === "GET" &&
        route === `/client/v4/accounts/${account}/workers/services/crabbox-coordinator`
      ) {
        result = {
          default_environment: {
            environment: "production",
            script: { tag: "fixture-tag", tags: [], last_deployed_from: "wrangler" },
          },
        };
      } else if (
        req.method === "GET" &&
        route === `/client/v4/accounts/${account}/workers/workers/crabbox-coordinator`
      ) {
        result = { id: "fixture-worker", subdomain: { enabled: false, previews_enabled: false } };
      } else if (req.method === "GET" && route === prefix + "/secrets") {
        result = [];
      } else if (req.method === "POST" && route === prefix + "/versions") {
        if (options.rejectUpload) {
          res.writeHead(400, { "content-type": "application/json" });
          res.end(
            JSON.stringify({
              success: false,
              errors: [{ code: 10021, message: "Private fixture policy github:1000000099" }],
            }),
          );
          return;
        }
        const form = await new Request("http://fixture", {
          method: "POST",
          body,
          headers: { "content-type": req.headers["content-type"] },
        }).formData();
        const part = form.get("metadata");
        const metadata = JSON.parse(typeof part === "string" ? part : await part.text());
        uploads.push(metadata);
        assert.ok(form.has(metadata.main_module));
        versions.set(uploadedVersion, { bindings: metadata.bindings });
        result = {
          id: uploadedVersion,
          resources: { script: { etag: "fixture-etag" } },
          startup_time_ms: 1,
        };
      } else if (req.method === "POST" && route === prefix + "/deployments") {
        const traffic = JSON.parse(body.toString());
        assert.deepEqual(traffic.versions, [{ version_id: uploadedVersion, percentage: 100 }]);
        state.version = uploadedVersion;
        state.deployment = uploadedDeployment;
        state.bindings = options.losePolicy ? [] : versions.get(uploadedVersion).bindings;
        if (options.losePolicy) versions.set(uploadedVersion, { bindings: [] });
        result = { id: uploadedDeployment, versions: traffic.versions };
      } else if (req.method === "PATCH" && route === prefix + "/script-settings") {
        result = {};
      } else if (req.method === "POST" && route === prefix + "/subdomain") {
        result = { enabled: false, previews_enabled: false };
      } else if (req.method === "GET" && route === prefix + "/subdomain") {
        result = { enabled: false, previews_enabled: false };
      } else if (req.method === "PUT" && route === prefix + "/schedules") {
        result = { schedules: [] };
      } else {
        unexpected.push(`${req.method} ${route}`);
        res.writeHead(404, { "content-type": "application/json" });
        res.end(
          JSON.stringify({
            success: false,
            errors: [{ code: 10007, message: "Unsupported fixture route" }],
          }),
        );
        return;
      }
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ success: true, result, errors: [], messages: [] }));
    })().catch((error) => {
      errors.push(error);
      res.writeHead(500, { "content-type": "application/json" });
      res.end(
        JSON.stringify({
          success: false,
          errors: [{ code: 10000, message: "Fixture assertion failed" }],
        }),
      );
    });
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(async () => {
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
    fs.rmSync(directory, { recursive: true, force: true });
  });
  const api = `http://127.0.0.1:${server.address().port}`;
  const hook = path.join(directory, "transport.mjs");
  fs.writeFileSync(
    hook,
    `
const originalFetch = globalThis.fetch;
const local = new URL(process.env.CLOUDFLARE_API_BASE_URL);
globalThis.fetch = (input, init) => {
  const url = new URL(typeof input === "string" ? input : input.url);
  if (url.origin === "https://api.cloudflare.com")
    return originalFetch(new URL(url.pathname + url.search, local.origin), init);
  if (url.origin !== local.origin) throw new Error("External fetch forbidden in fixture");
  return originalFetch(input, init);
};`,
  );
  const eventFile = path.join(directory, "event.json");
  const run = async (inputs, args = [], eventName = inputs ? "workflow_dispatch" : "push") => {
    fs.writeFileSync(eventFile, JSON.stringify({ inputs: inputs ?? {} }), { mode: 0o600 });
    const env = {
      PATH: `${path.dirname(process.execPath)}:/usr/bin:/bin`,
      HOME: path.join(directory, "home"),
      CI: "true",
      GITHUB_ACTIONS: "true",
      GITHUB_EVENT_NAME: eventName,
      GITHUB_EVENT_PATH: eventFile,
      GITHUB_SHA: "b".repeat(40),
      GITHUB_OUTPUT: path.join(directory, "github-output"),
      CLOUDFLARE_ACCOUNT_ID: account,
      CLOUDFLARE_API_TOKEN: "synthetic-wire-token",
      CLOUDFLARE_API_BASE_URL: api + "/client/v4",
      WRANGLER_SEND_METRICS: "false",
      NODE_OPTIONS: `--import=${pathToFileURL(hook).href}`,
      WRANGLER_LOG_PATH: path.join(directory, "wrangler.log"),
    };
    const result = await new Promise((resolve, reject) => {
      const child = spawn(
        process.execPath,
        [path.join(directory, "scripts/deploy-coordinator.mjs"), ...args],
        { cwd: worker, env, stdio: ["ignore", "pipe", "pipe"] },
      );
      let stdout = "",
        stderr = "";
      child.stdout.on("data", (chunk) => {
        stdout += chunk;
      });
      child.stderr.on("data", (chunk) => {
        stderr += chunk;
      });
      child.on("error", reject);
      child.on("close", (code) => resolve({ code, stdout, stderr }));
    });
    assert.equal(fs.readFileSync(path.join(worker, "wrangler.jsonc"), "utf8"), source);
    assert.equal(
      fs.readdirSync(worker).some((name) => name.startsWith(".crabbox-coordinator-deploy-")),
      false,
    );
    assert.doesNotMatch(
      result.stdout + result.stderr,
      /1000000099|synthetic-wire-token|43200|example-org/,
    );
    assert.deepEqual(errors, []);
    return result;
  };
  return { directory, worker, state, requests, unexpected, uploads, run, eventFile, api };
}

test("shipped CLI and real Wrangler preserve the complete existing policy over HTTP (base guard fails, head passes)", async (t) => {
  const f = await fixture(t);
  const base = execFileSync(
    "git",
    [
      "show",
      "bde928e4f6855217503c32d62a9c5a66872c68eb:scripts/verify-coordinator-lease-policy.mjs",
    ],
    { cwd: repo },
  );
  const file = path.join(f.directory, "scripts/base-policy-guard.mjs");
  fs.writeFileSync(file, base);
  // The parent guard sees the same existing policy and source with no override.
  // Its CLI rejects before any write; the current shipped deploy CLI must carry it.
  const baseCheck = await new Promise((resolve, reject) => {
    const hook = path.join(f.directory, "transport.mjs");
    const child = spawn(process.execPath, [file], {
      env: {
        PATH: `${path.dirname(process.execPath)}:/usr/bin:/bin`,
        CLOUDFLARE_ACCOUNT_ID: account,
        CLOUDFLARE_API_TOKEN: "synthetic-wire-token",
        CLOUDFLARE_API_BASE_URL: f.api + "/client/v4",
        NODE_OPTIONS: `--import=${pathToFileURL(hook).href}`,
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stderr = "";
    child.stderr.on("data", (chunk) => {
      stderr += chunk;
    });
    child.on("error", reject);
    child.on("close", (code) => resolve({ code, stderr }));
  });
  assert.equal(baseCheck.code, 1);
  assert.match(baseCheck.stderr, /Live and source lease policy differ/);
  assert.equal(f.requests.length, 1);
  assert.ok(f.requests.every((r) => r.method === "GET"));
  const result = await f.run();
  assert.equal(result.code, 0, result.stderr + JSON.stringify(f.requests));
  assert.deepEqual(f.unexpected, []);
  assert.equal(f.uploads.length, 1);
  const metadata = f.uploads[0];
  assert.deepEqual(JSON.parse(metadata.bindings.find((b) => b.name === bindingName).text), policy);
  assert.equal(metadata.bindings.find((b) => b.name === "GIT_OWNED_SETTING").text, "new-git-value");
  assert.ok(!metadata.keep_bindings?.includes("plain_text"));
  assert.match(
    fs.readFileSync(path.join(f.directory, "github-output"), "utf8"),
    new RegExp(policyDigest(policy)),
  );
});

test("real Wrangler receives a reviewed event-file replacement without policy in CLI output", async (t) => {
  const f = await fixture(t);
  const replacement = {
    ...policy,
    owners: { ...policy.owners, "github:1000000088": { maxTTLSeconds: 3600 } },
  };
  const result = await f.run({
    replace_live_lease_policy: "true",
    lease_policy_replacement: JSON.stringify(replacement),
    lease_policy_expected_sha256: policyDigest(policy),
  });
  assert.equal(result.code, 0, result.stderr + JSON.stringify(f.requests));
  assert.deepEqual(f.unexpected, []);
  assert.deepEqual(
    JSON.parse(f.state.bindings.find((b) => b.name === bindingName).text),
    replacement,
  );
});

test("a stale reviewed baseline prevents real Wrangler upload before any write", async (t) => {
  const f = await fixture(t);
  const result = await f.run({
    replace_live_lease_policy: "true",
    lease_policy_replacement: "{}",
    lease_policy_expected_sha256: policyDigest({}),
  });
  assert.equal(result.code, 1);
  assert.match(result.stderr, /reviewed replacement baseline/);
  assert.ok(f.requests.every((r) => r.method === "GET"));
  assert.equal(f.uploads.length, 0);
});

test("an exception revoked during capture prevents upload through the shipped CLI", async (t) => {
  const f = await fixture(t, { revoke: true });
  const result = await f.run();
  assert.equal(result.code, 1);
  assert.match(result.stderr, /changed/);
  assert.ok(f.requests.every((r) => r.method === "GET"));
  assert.equal(f.uploads.length, 0);
});

test("wire-level readback policy loss fails after the one upload without retry", async (t) => {
  const f = await fixture(t, { losePolicy: true });
  const result = await f.run();
  assert.equal(result.code, 1);
  assert.match(result.stderr, /Post-deployment version\/policy mismatch/);
  assert.equal(f.uploads.length, 1);
});

test("a real Wrangler upload failure reports safe stage and recovery guidance", async (t) => {
  const f = await fixture(t, { rejectUpload: true });
  const result = await f.run();
  assert.equal(result.code, 1);
  assert.match(result.stderr, /Coordinator Wrangler upload failed \(exit 1\)/);
  assert.match(result.stderr, /Cloudflare deployment and audit log/);
  assert.match(result.stderr, /production may have changed/);
  assert.doesNotMatch(result.stderr, /Private fixture policy/);
  assert.equal(
    f.requests.filter((r) => r.method === "POST" && r.route === prefix + "/versions").length,
    1,
  );
  assert.equal(
    f.requests.some((r) => r.method === "POST" && r.route === prefix + "/deployments"),
    false,
  );
  assert.deepEqual(f.unexpected, []);
});

for (const absent of [true, false]) {
  test(`real Wrangler ${absent ? "preserves absent policy" : "removes exceptions only after reviewed acknowledgment"}`, async (t) => {
    const f = await fixture(t, { absent });
    const result = await f.run(
      absent
        ? undefined
        : {
            replace_live_lease_policy: "true",
            lease_policy_replacement: "{}",
            lease_policy_expected_sha256: policyDigest(policy),
          },
    );
    assert.equal(result.code, 0, result.stderr + JSON.stringify(f.requests));
    assert.deepEqual(f.unexpected, []);
    assert.equal(f.uploads.length, 1);
    assert.equal(
      f.uploads[0].bindings.some((b) => b.name === bindingName),
      false,
    );
  });
}
