import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import crypto from "node:crypto";
import test from "node:test";

const controller = "crabbox-image-qualification-controller";
const candidate = "crabbox-image-qualification-42-1";
const staleCandidate = "crabbox-image-qualification-41-1";
const authority = "crabbox-aws-qualification-authority";
const deploymentHash = "b".repeat(64);
const version = "11111111-1111-4111-8111-111111111111";
const changedVersion = "22222222-2222-4222-8222-222222222222";
const token = "synthetic-controller-token-for-tests";
const control = path.join(import.meta.dirname, "image-qualification-control.mjs");

function bindings(hash) {
  return [
    { name: "CONTROLLER_TOKEN", type: "secret_text" },
    {
      name: "AUTHORITY",
      type: "service",
      service: authority,
      entrypoint: "AWSQualificationController",
      props: { deploymentHash: hash },
    },
  ];
}

function initialState(options = {}) {
  return {
    options,
    events: [],
    candidate: options.candidate ?? false,
    staleCandidate: options.staleCandidate ?? false,
    controller: options.controller
      ? { bindings: bindings(deploymentHash), version, tags: [] }
      : null,
    run: options.active
      ? {
          runId: "image-qualification-42-1",
          candidateWorker: candidate,
          deploymentHash,
        }
      : null,
  };
}

function installAPI(file) {
  const state = JSON.parse(fs.readFileSync(file, "utf8"));
  const { options, events } = state;
  const json = (value, status = 200) => new Response(JSON.stringify(value), { status });
  const api = (result, status = 200) => json({ success: status === 200, result }, status);
  const missing = () => api(null, 404);
  let controllerReads = 0;
  let scriptLists = 0;
  process.on("exit", () => fs.writeFileSync(file, JSON.stringify(state)));
  globalThis.fetch = async (input, init = {}) => {
    const url = new URL(input);
    const method = init.method ?? "GET";
    const route = url.pathname.replace(/^\/client\/v4\/accounts\/a{32}/, "");
    events.push(`${method} ${route}`);
    if (url.hostname === `${controller}.example.workers.dev`) {
      assert.equal(init.headers.authorization, `Bearer ${token}`);
      assert.equal(method, "POST");
      if (options.discoveryStatus) return json({ error: "forbidden" }, options.discoveryStatus);
      if (Object.hasOwn(options, "discovery")) return json(options.discovery);
      assert.ok(state.controller);
      const hash = state.controller.bindings.find((item) => item.name === "AUTHORITY").props
        .deploymentHash;
      if (state.run && hash !== state.run.deploymentHash) {
        return json({ error: "controller_error" }, 409);
      }
      if (route === "/discover") {
        const result = { run: state.run };
        if (options.changeAfterDiscovery) state.controller.version = changedVersion;
        if (options.changeBindingsAfterDiscovery)
          state.controller.bindings = bindings("c".repeat(64));
        return json(result);
      }
      assert.ok(state.run, "no active mutation without a registered run");
      if (route === "/begin-finalization") return json({ finalizing: true });
      if (route === "/network") {
        if (options.networkFailure) return json({ error: "unavailable" }, 503);
        return json({});
      }
      if (route === "/finalize") {
        if (options.computeFailure) return json({ error: "unavailable" }, 503);
        return json({ finalized: true });
      }
      if (route === "/attest") {
        return json({ finalized: true, finalReceipt: { finalCounts: { instances: 0 } } });
      }
      if (route === "/retire") {
        state.run = null;
        return json({ retired: true });
      }
      throw new Error(`unexpected controller request ${route}`);
    }
    assert.equal(url.hostname, "api.cloudflare.com", "no real network access");
    assert.equal(init.headers.authorization, "Bearer synthetic-cloudflare-token");
    if (route === "/workers/subdomain") return api({ subdomain: "example" });
    if (route === "/workers/durable_objects/namespaces") return api([]);
    if (route === "/workers/scripts") {
      scriptLists += 1;
      if (options.idleCleanupFailure && scriptLists > 1) return api(null, 503);
      return api([
        ...(state.staleCandidate ? [{ id: staleCandidate }] : []),
        ...(state.candidate ? [{ id: candidate }] : []),
      ]);
    }
    if (route === `/workers/scripts/${staleCandidate}/settings`) {
      return api({
        bindings: [
          {
            name: "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
            type: "service",
            service: authority,
            props: { candidateWorker: staleCandidate, deploymentHash: "c".repeat(64) },
          },
        ],
      });
    }
    if (route === `/workers/scripts/${candidate}/settings`) {
      return state.candidate
        ? api({
            bindings: [
              {
                name: "CRABBOX_AWS_QUALIFICATION_TRANSPORT",
                type: "service",
                service: authority,
                props: { candidateWorker: candidate, deploymentHash },
              },
            ],
          })
        : missing();
    }
    if (route === `/workers/scripts/${candidate}`) {
      if (method === "PUT") return api({});
      assert.equal(method, "DELETE");
      state.candidate = false;
      return new Response(null, { status: 204 });
    }
    if (route === "/workers/scripts/crabbox-image-qualification-relay-42-1/settings") {
      return missing();
    }
    if (route === `/workers/scripts/${controller}/settings`) {
      if (options.settingsStatus) return api(null, options.settingsStatus);
      if (options.settingsMalformed) return json({ success: true });
      controllerReads += 1;
      if (options.controllerAppears && controllerReads === 2) {
        state.controller = {
          bindings: bindings(deploymentHash),
          version: changedVersion,
          tags: [],
        };
      }
      return state.controller ? api(state.controller) : missing();
    }
    if (route === `/workers/scripts/${controller}/versions`) {
      if (options.versionStatus) return api(null, options.versionStatus);
      return api({ items: [{ id: state.controller.version }] });
    }
    if (route === `/workers/scripts/${controller}/subdomain`) {
      if (!state.controller) return missing();
      if (options.enableFailure && method === "POST") return api(null, 503);
      if (method === "POST") state.controller.subdomain = JSON.parse(init.body);
      return api(state.controller.subdomain ?? { enabled: false, previews_enabled: false });
    }
    if (route === `/workers/scripts/${controller}`) {
      if (method === "PUT") {
        if (options.uploadFailure === "before") throw new Error("upload response lost");
        const metadata = JSON.parse(await init.body.get("metadata").text());
        state.controller = {
          bindings: metadata.bindings.map(({ text, ...binding }) => binding),
          version,
          tags: metadata.tags ?? [],
        };
        if (options.uploadOwnershipLost) {
          state.controller.tags = [];
          state.controller.version = changedVersion;
          throw new Error("upload response lost");
        }
        if (options.uploadFailure === "after") throw new Error("upload response lost");
        return api({ id: controller });
      }
      assert.equal(method, "DELETE");
      if (options.deleteFailure) return api(null, 503);
      if (!options.deleteResidue) state.controller = null;
      return new Response(null, { status: 204 });
    }
    throw new Error(`unexpected Cloudflare request ${method} ${route}`);
  };
}

if (process.env.QUALIFICATION_TEST_STATE) {
  installAPI(process.env.QUALIFICATION_TEST_STATE);
} else {
  function fixture(t, options) {
    const directory = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-reaper-test-"));
    t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
    const stateFile = path.join(directory, "state.json");
    fs.writeFileSync(stateFile, JSON.stringify(initialState(options)));
    return (command = "reap") => {
      const proofDirectory = path.join(directory, "proof");
      fs.rmSync(proofDirectory, { recursive: true, force: true });
      const child = spawnSync(process.execPath, ["--import", import.meta.url, control, command], {
        encoding: "utf8",
        timeout: 10_000,
        env: {
          HOME: directory,
          RUNNER_TEMP: directory,
          CLOUDFLARE_ACCOUNT_ID: "a".repeat(32),
          CLOUDFLARE_API_TOKEN: "synthetic-cloudflare-token",
          QUALIFICATION_CONTROLLER_TOKEN: token,
          QUALIFICATION_PROOF_DIR: proofDirectory,
          QUALIFICATION_TEST_STATE: stateFile,
        },
      });
      assert.equal(child.error, undefined);
      const state = JSON.parse(fs.readFileSync(stateFile, "utf8"));
      const proof = path.join(proofDirectory, "finalization.json");
      return {
        ...child,
        state,
        result: fs.existsSync(proof) ? JSON.parse(fs.readFileSync(proof, "utf8")) : undefined,
      };
    };
  }

  const uploads = (state) =>
    state.events.filter((event) => event === `PUT /workers/scripts/${controller}`);
  const deletes = (state) =>
    state.events.filter((event) => event === `DELETE /workers/scripts/${controller}`);
  const calls = (state) => state.events.filter((event) => /^POST \/(?!workers)/.test(event));

  for (const failure of ["networkFailure", "computeFailure"]) {
    test(`cleanup continues across ${failure} and retains durable recovery`, (t) => {
      const result = fixture(t, {
        active: true,
        controller: true,
        candidate: true,
        [failure]: true,
      })();
      assert.equal(result.status, 1);
      assert.ok(result.state.events.includes("POST /network"));
      assert.ok(result.state.events.includes("POST /finalize"));
      assert.ok(result.state.events.includes(`DELETE /workers/scripts/${candidate}`));
      assert.ok(result.state.run);
      assert.ok(result.state.controller);
      assert.ok(!result.state.events.includes("POST /retire"));
    });
  }

  test("a second reap is idle after successful finalization removed the controller", (t) => {
    const reap = fixture(t, { active: true, controller: true, candidate: true });
    const first = reap();
    assert.equal(first.status, 0, first.stderr);
    assert.equal(first.result.result, "finalized");
    assert.equal(first.state.controller, null);
    assert.equal(first.state.candidate, false);
    assert.equal(first.state.run, null);
    const second = reap();
    assert.equal(second.status, 0, second.stderr);
    assert.equal(second.result.result, "idle");
    assert.equal(second.state.controller, null);
    assert.deepEqual(calls(second.state).slice(calls(first.state).length), ["POST /discover"]);
  });

  test("empty registry discovery leaves no temporary controller", (t) => {
    const result = fixture(t)();
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.result.result, "idle");
    assert.equal(result.state.controller, null);
    assert.equal(uploads(result.state).length, 1);
    assert.equal(deletes(result.state).length, 1);
    assert.deepEqual(calls(result.state), ["POST /discover"]);
    assert.ok(
      result.state.events.indexOf(`GET /workers/scripts/${controller}/settings`) <
        result.state.events.indexOf(`PUT /workers/scripts/${controller}`),
    );
  });

  test("a candidate binding still recovers an active run", (t) => {
    const result = fixture(t, { active: true, candidate: true })();
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.result.result, "finalized");
    assert.equal(result.state.run, null);
    assert.equal(result.state.candidate, false);
    assert.deepEqual(calls(result.state), [
      "POST /discover",
      "POST /begin-finalization",
      "POST /network",
      "POST /finalize",
      "POST /attest",
      "POST /finalize",
      "POST /attest",
      "POST /retire",
      "POST /discover",
    ]);
  });

  test("a stale candidate hash cannot prevent recovery of the active candidate", (t) => {
    const result = fixture(t, { active: true, candidate: true, staleCandidate: true })();
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.result.result, "finalized");
    assert.equal(result.state.run, null);
    assert.equal(result.state.candidate, false);
    assert.equal(result.state.staleCandidate, true);
    assert.equal(uploads(result.state).length, 2);
    assert.equal(deletes(result.state).length, 2);
    assert.deepEqual(calls(result.state).slice(0, 3), [
      "POST /discover",
      "POST /discover",
      "POST /begin-finalization",
    ]);
    const firstDelete = result.state.events.indexOf(`DELETE /workers/scripts/${controller}`);
    const secondUpload = result.state.events.lastIndexOf(`PUT /workers/scripts/${controller}`);
    assert.ok(firstDelete < secondUpload);
    assert.ok(
      result.state.events
        .slice(firstDelete, secondUpload)
        .includes(`GET /workers/scripts/${controller}/settings`),
    );
  });

  for (const options of [
    { active: true },
    { discoveryStatus: 403 },
    { discoveryStatus: 503 },
    { discovery: [] },
    { discovery: {} },
    { discovery: null },
    { discovery: { run: false } },
    { discovery: { run: null, unexpected: true } },
    { discovery: { run: { deploymentHash } } },
    {
      discovery: {
        run: {
          runId: "image-qualification-42-1",
          candidateWorker: candidate,
          deploymentHash: crypto
            .createHash("sha256")
            .update("crabbox/image-qualification/idle-discovery/v1")
            .digest("hex"),
        },
      },
    },
  ]) {
    test(`idle probe fails closed: ${JSON.stringify(options)}`, (t) => {
      const result = fixture(t, options)();
      assert.equal(result.status, 1);
      assert.equal(result.result.result, "failed");
      assert.equal(result.state.controller, null);
      assert.equal(uploads(result.state).length, 1);
      assert.deepEqual(calls(result.state), ["POST /discover"]);
      if (options.active) assert.ok(result.state.run);
    });
  }

  for (const options of [
    { controller: true, candidate: true, discoveryStatus: 403 },
    { controller: true, candidate: true, enableFailure: true },
    { settingsStatus: 403 },
    { settingsStatus: 503 },
    { settingsMalformed: true },
    { controllerAppears: true },
    { controller: true, discovery: {} },
    { controller: true, discovery: { run: false } },
  ]) {
    test(`controller uncertainty never authorizes replacement: ${JSON.stringify(options)}`, (t) => {
      const result = fixture(t, options)();
      assert.equal(result.status, 1);
      assert.equal(uploads(result.state).length, 0);
      assert.equal(deletes(result.state).length, 0);
    });
  }

  for (const options of [
    { uploadFailure: "before" },
    { uploadFailure: "after" },
    { enableFailure: true },
  ]) {
    test(`partial idle deployment is cleaned but remains failed: ${JSON.stringify(options)}`, (t) => {
      const result = fixture(t, options)();
      assert.equal(result.status, 1);
      assert.equal(result.result.result, "failed");
      assert.equal(result.state.controller, null);
      assert.equal(uploads(result.state).length, 1);
      assert.deepEqual(calls(result.state), []);
    });
  }

  for (const options of [
    { changeAfterDiscovery: true },
    { changeBindingsAfterDiscovery: true },
    { versionStatus: 503 },
    { uploadOwnershipLost: true },
  ]) {
    test(`uncertain probe ownership preserves evidence: ${JSON.stringify(options)}`, (t) => {
      const result = fixture(t, options)();
      assert.equal(result.status, 1);
      assert.equal(result.result.result, "failed");
      assert.ok(result.state.controller);
      assert.equal(deletes(result.state).length, 0);
    });
  }

  test("failed candidate recovery with lost ownership cannot be overwritten", (t) => {
    const result = fixture(t, {
      candidate: true,
      active: true,
      staleCandidate: true,
      uploadOwnershipLost: true,
    })();
    assert.equal(result.status, 1);
    assert.equal(result.result.result, "failed");
    assert.match(result.result.failure, /upload response lost.*ownership.*refusing to replace/);
    assert.ok(result.state.controller);
    assert.ok(result.state.run);
    assert.equal(result.state.candidate, true);
    assert.equal(uploads(result.state).length, 1);
    assert.equal(deletes(result.state).length, 0);
    assert.deepEqual(calls(result.state), []);
  });

  test("failed owned candidate discovery is cleaned without claiming idle", (t) => {
    const result = fixture(t, { candidate: true, active: true, discoveryStatus: 403 })();
    assert.equal(result.status, 1);
    assert.match(result.result.failure, /discover failed: 403/);
    assert.equal(result.state.controller, null);
    assert.ok(result.state.run);
    assert.equal(result.state.candidate, true);
    assert.equal(uploads(result.state).length, 1);
    assert.equal(deletes(result.state).length, 1);
    assert.deepEqual(calls(result.state), ["POST /discover"]);
  });

  test("idle cleanup failure retains a finalization evidence artifact", (t) => {
    const result = fixture(t, { idleCleanupFailure: true })();
    assert.equal(result.status, 1);
    assert.equal(result.result.result, "failed");
    assert.match(result.result.failure, /idle cleanup:.*503/);
    assert.equal(result.state.controller, null);
  });

  test("finalize retains both identity and discovery failures", (t) => {
    const result = fixture(t, { controller: true, discoveryStatus: 403 })("finalize");
    assert.equal(result.status, 1);
    assert.equal(result.result.result, "failed");
    assert.match(result.result.failure, /invalid or missing GITHUB_REPOSITORY/);
    assert.match(result.result.failure, /discover failed: 403/);
    assert.equal(uploads(result.state).length, 0);
    assert.equal(deletes(result.state).length, 0);
  });

  test("discovery and cleanup failures are both reported", (t) => {
    const result = fixture(t, { discoveryStatus: 403, deleteFailure: true })();
    assert.equal(result.status, 1);
    assert.match(result.result.failure, /discover failed: 403/);
    assert.match(result.result.failure, /cleanup:.*503/);
    assert.ok(result.state.controller);
  });

  test("successful delete response without observable absence is not idle", (t) => {
    const result = fixture(t, { deleteResidue: true })();
    assert.equal(result.status, 1);
    assert.match(result.result.failure, /deletion was not observable/);
    assert.ok(result.state.controller);
  });
}
