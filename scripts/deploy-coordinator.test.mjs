import assert from "node:assert/strict";
import fs from "node:fs";
import { createRequire } from "node:module";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import { deployCoordinator, policyReplacementOptions } from "./deploy-coordinator.mjs";
import { bindingName, policyDigest } from "./verify-coordinator-lease-policy.mjs";

const worker = path.resolve(import.meta.dirname, "../worker");
const require = createRequire(path.join(worker, "package.json"));
const { unstable_readConfig } = require("wrangler");
const versionId = "11111111-1111-4111-8111-111111111111";
const deploymentId = "22222222-2222-4222-8222-222222222222";
const uploadedId = "33333333-3333-4333-8333-333333333333";
const changedId = "44444444-4444-4444-8444-444444444444";
const scoped = {
  owners: {
    "github:1000000001": { maxTTLSeconds: 43200 },
    "github:1000000002": { defaultTTLSeconds: 2400, extension: "deny", autoExtend: "deny" },
  },
  orgs: { "example-org": { maxTTLSeconds: 7200, defaultIdleTimeoutSeconds: 900 } },
};
const plaintext = (document) => ({
  name: bindingName,
  type: "plain_text",
  text: JSON.stringify(document),
});

function fixture(t, document = scoped, options = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "coordinator-policy-deploy-test-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  fs.symlinkSync(path.join(worker, "node_modules"), path.join(dir, "node_modules"), "dir");
  fs.mkdirSync(path.join(dir, "public"));
  fs.mkdirSync(path.join(dir, "src"));
  fs.writeFileSync(path.join(dir, "package.json"), '{"type":"module"}');
  const source = {
    name: "crabbox-coordinator",
    main: "src/index.ts",
    compatibility_date: "2026-04-30",
    assets: { directory: "./public" },
    alias: { example: "./src/example.ts" },
    vars: {
      CRABBOX_LEASE_MAX_TTL_SECONDS: "14400",
      CRABBOX_LEASE_DEFAULT_TTL_SECONDS: "1800",
      GIT_OWNED_SETTING: "git-value",
    },
    ...options.source,
  };
  const sourcePath = path.join(dir, "wrangler.jsonc");
  fs.writeFileSync(sourcePath, JSON.stringify(source));
  const before = fs.readFileSync(sourcePath, "utf8");
  const state = {
    versionId,
    deploymentId,
    bindings: document === undefined ? [] : [plaintext(document)],
    ...options.state,
  };
  const calls = [];
  const commands = [];
  let submitted;
  let configPath;
  const fetchImpl = async (url, init) => {
    const suffix = new URL(url).pathname.split("/crabbox-coordinator")[1];
    calls.push(suffix);
    assert.equal(new URL(url).origin, "https://api.cloudflare.com");
    assert.equal(init.method ?? "GET", "GET");
    assert.equal(init.redirect, "error");
    assert.equal(init.cache, "no-store");
    assert.deepEqual(init.headers, { authorization: "Bearer synthetic-api-token" });
    const override = options.onRequest?.({ suffix, state, count: calls.length, dir });
    if (override instanceof Error) throw override;
    if (override instanceof Response) return override;
    let result;
    if (suffix === "/deployments")
      result = {
        deployments: [
          { id: state.deploymentId, versions: [{ version_id: state.versionId, percentage: 100 }] },
        ],
      };
    else if (suffix === "/settings") result = { bindings: state.bindings };
    else if (suffix === `/versions/${state.versionId}`)
      result = { resources: { bindings: options.servingBindings ?? state.bindings } };
    else assert.fail(`Unexpected route ${suffix}`);
    return Response.json({ success: true, result });
  };
  const run = async (command, args, init) => {
    commands.push({ command, args });
    if (command === "npm") {
      assert.deepEqual(args, ["run", "predeploy"]);
      return;
    }
    assert.equal(command, process.execPath);
    assert.equal(args[1], "deploy");
    configPath = args[args.indexOf("--config") + 1];
    assert.equal(path.dirname(configPath), dir);
    assert.equal(fs.statSync(configPath).mode & 0o777, 0o600);
    submitted = JSON.parse(fs.readFileSync(configPath, "utf8"));
    const normalized = unstable_readConfig({ config: configPath }, { hideWarnings: true });
    const original = unstable_readConfig({ config: sourcePath }, { hideWarnings: true });
    assert.equal(normalized.main, original.main);
    assert.deepEqual(normalized.assets, original.assets);
    assert.deepEqual(submitted.vars.GIT_OWNED_SETTING, source.vars.GIT_OWNED_SETTING);
    const rest = structuredClone(submitted);
    delete rest.vars[bindingName];
    assert.deepEqual(rest, source, "Only the runtime-owned exception binding may change");
    state.versionId = uploadedId;
    state.deploymentId = changedId;
    state.bindings = Object.hasOwn(submitted.vars, bindingName)
      ? [{ name: bindingName, type: "plain_text", text: submitted.vars[bindingName] }]
      : [];
    fs.writeFileSync(
      init.env.WRANGLER_OUTPUT_FILE_PATH,
      JSON.stringify({
        type: "deploy",
        version: 1,
        worker_name: "crabbox-coordinator",
        version_id: uploadedId,
      }) + "\n",
    );
    await options.onDeploy?.({ state, configPath, init });
  };
  return {
    calls,
    commands,
    state,
    submitted: () => submitted,
    configPath: () => configPath,
    run: async (overrides = {}) => {
      try {
        return await deployCoordinator({
          accountId: "a".repeat(32),
          apiToken: "synthetic-api-token",
          sha: "b".repeat(40),
          workerDirectory: dir,
          env: {},
          fetchImpl,
          run,
          ...overrides,
        });
      } finally {
        assert.equal(fs.readFileSync(sourcePath, "utf8"), before, "Source stays unchanged");
        assert.equal(
          fs.readdirSync(dir).some((name) => name.startsWith(".crabbox-coordinator-deploy-")),
          false,
          "Private generated config must be removed on success and failure",
        );
      }
    },
  };
}

test("ordinary deployment preserves the complete live owner/org document in the actual Wrangler config", async (t) => {
  const f = fixture(t);
  assert.deepEqual(await f.run(), {
    deploymentId: changedId,
    versionId: uploadedId,
    policySHA256: policyDigest(scoped),
  });
  assert.deepEqual(JSON.parse(f.submitted().vars[bindingName]), scoped);
  assert.equal(f.commands.length, 2);
  assert.equal(
    f.calls.length,
    12,
    "Capture, final guard and readback each bind policy to a stable deployment",
  );
  assert.equal(fs.existsSync(f.configPath()), false);
});

for (const [label, doc] of [
  ["absent", undefined],
  ["empty", {}],
  ["empty groups", { owners: {}, orgs: {} }],
]) {
  test(`ordinary deployment normalizes ${label} policy consistently`, async (t) => {
    const f = fixture(t, doc, doc === undefined ? { state: { bindings: [] } } : {});
    assert.equal((await f.run()).policySHA256, policyDigest({}));
    assert.equal(Object.hasOwn(f.submitted().vars, bindingName), false);
  });
}

test("preserves empty owner entries and prototype-looking identity keys", async (t) => {
  const doc = JSON.parse('{"owners":{"__proto__":{},"constructor":{"maxTTLSeconds":43200}}}');
  const f = fixture(t, doc);
  await f.run();
  assert.deepEqual(JSON.parse(f.submitted().vars[bindingName]), doc);
});

for (const [label, bindings] of [
  ["invalid duration", [plaintext({ owners: { synthetic: { maxTTLSeconds: "43200" } } })]],
  [
    "invalid switch",
    [plaintext({ orgs: { synthetic: { extension: "synthetic-private-value" } } })],
  ],
  ["unknown field", [plaintext({ owners: { synthetic: { unknown: 1 } } })]],
  ["unreadable secret", [{ name: bindingName, type: "secret_text" }]],
  ["JSON binding", [{ name: bindingName, type: "json", json: scoped }]],
  ["missing text", [{ name: bindingName, type: "plain_text" }]],
  ["duplicate binding", [plaintext(scoped), plaintext(scoped)]],
  ["empty plaintext", [{ name: bindingName, type: "plain_text", text: "" }]],
  ["bad JSON", [{ name: bindingName, type: "plain_text", text: "synthetic-private-value" }]],
  ["array document", [plaintext([])]],
  ["invalid shape", [plaintext({ owners: [] })]],
]) {
  test(`ordinary deployment refuses ${label} before upload without disclosing values`, async (t) => {
    const f = fixture(t, undefined, { state: { bindings } });
    await assert.rejects(f.run(), (error) => {
      assert.doesNotMatch(error.message, /synthetic-|43200/);
      return true;
    });
    assert.equal(f.commands.length, 1);
  });
}

for (const [label, replacement] of [
  ["replacement", { owners: { "github:1000000003": { maxTTLSeconds: 43200 } } }],
  ["removal", {}],
]) {
  test(`intentional ${label} requires a reviewed baseline and uses the same guarded config`, async (t) => {
    const f = fixture(t);
    await f.run({
      acknowledgeReplacement: true,
      replacementOverrides: JSON.stringify(replacement),
      expectedLivePolicySHA256: policyDigest(scoped),
    });
    assert.deepEqual(JSON.parse(f.submitted().vars[bindingName] ?? "{}"), replacement);
  });
}

test("explicit replacement can repair readable shape-valid but runtime-invalid fields", async (t) => {
  const doc = { owners: { synthetic: { maxTTLSeconds: 86401 } } };
  const f = fixture(t, doc);
  await f.run({
    acknowledgeReplacement: true,
    replacementOverrides: "{}",
    expectedLivePolicySHA256: policyDigest(doc),
  });
});

for (const overrides of [
  { acknowledgeReplacement: true },
  { replacementOverrides: "{}" },
  { acknowledgeReplacement: true, replacementOverrides: "{}" },
  {
    acknowledgeReplacement: true,
    replacementOverrides: "[]",
    expectedLivePolicySHA256: "a".repeat(64),
  },
  {
    acknowledgeReplacement: true,
    replacementOverrides: "synthetic-private-value",
    expectedLivePolicySHA256: "a".repeat(64),
  },
]) {
  test(`rejects incomplete/invalid replacement authority before any command: ${JSON.stringify(overrides)}`, async (t) => {
    const f = fixture(t);
    await assert.rejects(f.run(overrides));
    assert.equal(f.commands.length, 0);
    assert.equal(f.calls.length, 0);
  });
}

test("a replacement cannot silently overwrite a policy changed since operator review", async (t) => {
  const f = fixture(t);
  await assert.rejects(
    f.run({
      acknowledgeReplacement: true,
      replacementOverrides: "{}",
      expectedLivePolicySHA256: policyDigest({}),
    }),
    /reviewed replacement baseline/,
  );
  assert.equal(f.commands.length, 1);
});

for (const [label, onRequest] of [
  [
    "deployment during capture",
    ({ count, state }) => {
      if (count === 4) state.deploymentId = changedId;
    },
  ],
  [
    "policy before final guard",
    ({ count, state }) => {
      if (count === 5) state.bindings = [plaintext({})];
    },
  ],
  [
    "deployment before final guard",
    ({ count, state }) => {
      if (count === 5) state.deploymentId = changedId;
    },
  ],
  ["API failure", () => new Response("synthetic-api-token", { status: 403 })],
  ["invalid response", () => new Response("synthetic-api-token")],
  ["transport failure", () => new Error("synthetic-api-token")],
]) {
  test(`fails closed for ${label}`, async (t) => {
    const f = fixture(t, scoped, { onRequest });
    await assert.rejects(f.run(), (error) => {
      assert.doesNotMatch(error.message, /synthetic-/);
      return true;
    });
    assert.equal(f.commands.length, 1);
  });
}

test("staged/settings policy differing from the serving version cannot be deployed", async (t) => {
  const f = fixture(t, scoped, { servingBindings: [] });
  await assert.rejects(f.run(), /Settings and serving lease policy differ/);
  assert.equal(f.commands.length, 1);
});

for (const [label, onDeploy] of [
  [
    "policy lost",
    ({ state }) => {
      state.bindings = [];
    },
  ],
  [
    "concurrent deployment",
    ({ state }) => {
      state.versionId = changedId;
    },
  ],
  [
    "missing upload receipt",
    ({ init }) => {
      fs.unlinkSync(init.env.WRANGLER_OUTPUT_FILE_PATH);
    },
  ],
  [
    "unreadable readback",
    ({ state }) => {
      state.bindings = [{ name: bindingName, type: "secret_text" }];
    },
  ],
]) {
  test(`reports ${label} after upload without rolling back or retrying`, async (t) => {
    const f = fixture(t, scoped, { onDeploy });
    await assert.rejects(f.run());
    assert.equal(f.commands.length, 2);
  });
}

for (const source of [
  { keep_vars: true },
  { name: "another-worker" },
  { account_id: "b".repeat(32) },
  { vars: { [bindingName]: JSON.stringify(scoped) } },
]) {
  test(`rejects unsafe/source-owned deployment settings: ${JSON.stringify(source)}`, async (t) => {
    const f = fixture(t, scoped, { source });
    await assert.rejects(f.run(), /Production source must target/);
    assert.equal(f.commands.length, 0);
  });
}

test("the ordinary npm deployment entrypoint uses the guarded helper", () => {
  const pkg = JSON.parse(fs.readFileSync(path.join(worker, "package.json"), "utf8"));
  assert.equal(pkg.scripts.deploy, "node ../scripts/deploy-coordinator.mjs");
});

test("upload failure removes private config and warns that production may have changed", async (t) => {
  const f = fixture(t, scoped, {
    onDeploy: () => {
      throw new Error("synthetic command failed");
    },
  });
  await assert.rejects(f.run(), /Deployment was attempted; production may have changed/);
  assert.equal(f.commands.length, 2);
});

test("a generated configuration changed after the guard is never uploaded", async (t) => {
  const f = fixture(t, scoped, {
    onRequest: ({ count, dir }) => {
      if (count === 8) {
        const name = fs
          .readdirSync(dir)
          .find((entry) => entry.startsWith(".crabbox-coordinator-deploy-"));
        fs.appendFileSync(path.join(dir, name), " ");
      }
    },
  });
  await assert.rejects(f.run(), /configuration changed after preparation/);
  assert.equal(f.commands.length, 1);
});

test("dispatch reads the complete policy from the event file without a policy environment variable", (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "coordinator-dispatch-test-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const event = path.join(dir, "event.json");
  fs.writeFileSync(
    event,
    JSON.stringify({
      inputs: {
        replace_live_lease_policy: "true",
        lease_policy_replacement: JSON.stringify(scoped),
        lease_policy_expected_sha256: policyDigest({}),
      },
    }),
    { mode: 0o600 },
  );
  const env = {
    GITHUB_ACTIONS: "true",
    GITHUB_EVENT_NAME: "workflow_dispatch",
    GITHUB_EVENT_PATH: event,
  };
  assert.deepEqual(policyReplacementOptions(env), {
    acknowledgeReplacement: true,
    replacementOverrides: JSON.stringify(scoped),
    expectedLivePolicySHA256: policyDigest({}),
  });
  assert.equal(
    Object.values(env).some((value) => value.includes("github:")),
    false,
  );
  assert.deepEqual(
    policyReplacementOptions({
      ...env,
      GITHUB_EVENT_NAME: "push",
      CRABBOX_ACKNOWLEDGE_LEASE_POLICY_REPLACEMENT: "1",
      CRABBOX_LEASE_POLICY_REPLACEMENT: "{}",
    }),
    {},
  );
});

for (const document of [
  "synthetic-private-policy",
  "null",
  "{}",
  '{"inputs":[]}',
  '{"inputs":{"lease_policy_replacement":{}}}',
]) {
  test(`unreadable or invalid dispatch event fails without disclosing input: ${document}`, (t) => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "coordinator-dispatch-test-"));
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    const event = path.join(dir, "event.json");
    fs.writeFileSync(event, document);
    assert.throws(
      () =>
        policyReplacementOptions({
          GITHUB_ACTIONS: "true",
          GITHUB_EVENT_NAME: "workflow_dispatch",
          GITHUB_EVENT_PATH: event,
        }),
      (error) => {
        assert.match(error.message, /no deployment performed/);
        assert.doesNotMatch(error.message, /synthetic-private-policy/);
        return true;
      },
    );
  });
}

test("manual replacement environment remains supported outside Actions", () => {
  assert.deepEqual(
    policyReplacementOptions({
      CRABBOX_ACKNOWLEDGE_LEASE_POLICY_REPLACEMENT: "1",
      CRABBOX_LEASE_POLICY_REPLACEMENT: "{}",
      CRABBOX_EXPECTED_LIVE_LEASE_POLICY_SHA256: policyDigest(scoped),
    }),
    {
      acknowledgeReplacement: true,
      replacementOverrides: "{}",
      expectedLivePolicySHA256: policyDigest(scoped),
    },
  );
});

test("a real failed preparation command reports its stage and safe recovery without child output", async (t) => {
  const f = fixture(t);
  await assert.rejects(f.run({ run: undefined, env: { PATH: process.env.PATH } }), (error) => {
    assert.match(error.message, /Coordinator asset preparation/);
    assert.match(error.message, /credential-free checkout/);
    assert.match(error.message, /npm run predeploy --prefix worker/);
    assert.doesNotMatch(error.message, /synthetic-api-token|Missing script/);
    return true;
  });
  assert.equal(f.calls.length, 0);
});
