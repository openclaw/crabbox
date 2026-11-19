import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

// Do not import the CLI before the child's fetch interceptor is installed:
// its entrypoint deliberately runs when argv names the control script.
let canonical;
let digest;
if (!process.env.QUALIFICATION_HANDOFF_TEST_STATE) {
  ({ canonical, digest } = await import("./image-qualification-control.mjs"));
}

const root = path.resolve(import.meta.dirname, "..");
const control = path.join(root, "scripts/image-qualification-control.mjs");
const candidateSha = "a".repeat(40);
const workflowSha = "b".repeat(40);
const authoritySha = "c".repeat(40);
const policyHash = "d".repeat(64);
const workerVersion = "11111111-1111-4111-8111-111111111111";
const capsulePaths = [
  "internal/cli/actions.go",
  "recipes/devtools/v1/linux-x86_64.json",
  "recipes/devtools/v1/recipe.schema.json",
  "recipes/linux/v1/linux-builder.json",
  "recipes/linux/v1/linux-minimal.json",
  "recipes/linux/v1/manifest.schema.json",
  "recipes/linux/v1/profile.schema.json",
  "scripts/devtools-image-contract.mjs",
  "scripts/devtools-image-smoke-linux.sh",
  "scripts/generate-linux-readiness.mjs",
  "scripts/install-linux-developer-tools.sh",
  "scripts/linux-readiness.generated.sh",
  "scripts/mint-aws-devtools-image.sh",
];

function installAPI(file) {
  const state = JSON.parse(fs.readFileSync(file, "utf8"));
  Date.now = () => state.now;
  process.on("exit", () => fs.writeFileSync(file, JSON.stringify(state)));
  const json = (body, status = 200) => new Response(JSON.stringify(body), { status });
  const api = (result, status = 200) => json({ success: status === 200, result }, status);
  globalThis.fetch = async (input, init = {}) => {
    const url = new URL(input);
    const method = init.method ?? "GET";
    const route = url.pathname;
    state.events.push(`${method} ${url.hostname}${route}`);
    if (url.hostname === "api.github.com") {
      if (route.endsWith("/pulls/7")) {
        return json({
          state: "open",
          head: { sha: state.candidateSha ?? candidateSha, repo: { full_name: "example/repo" } },
          base: { sha: workflowSha },
        });
      }
      if (route.endsWith("/commits/main")) return json({ sha: workflowSha });
      if (route.endsWith("/actions/runs/42")) {
        return json({
          id: 42,
          run_attempt: 1,
          event: "workflow_dispatch",
          head_sha: workflowSha,
          head_repository: { full_name: "example/repo" },
          path: ".github/workflows/image-qualification.yml",
        });
      }
      const artifact = route.match(/\/actions\/artifacts\/(9|10)$/)?.[1];
      if (artifact) {
        return json({
          id: Number(artifact),
          name:
            artifact === "9"
              ? "image-qualification-candidate-42"
              : "image-qualification-handoff-42-1",
          expired: false,
          digest: `sha256:${artifact === "9" ? "e".repeat(64) : "f".repeat(64)}`,
          workflow_run: { id: 42, head_sha: workflowSha },
          ...(artifact === "10" ? state.handoffMetadata : {}),
        });
      }
      throw new Error(`unexpected GitHub fixture route: ${route}`);
    }
    if (url.hostname.endsWith(".example.workers.dev")) {
      if (route === "/claim") {
        state.identity = JSON.parse(init.body).identity;
        state.enrolledAt = new Date(state.now).toISOString();
        return json({ ...state.identity, cleanupState: "claimed" });
      }
      if (route === "/discover") {
        return json({ run: state.identity ? { ...state.identity, cleanupState: "claimed" } : null });
      }
      if (route === "/begin-finalization") {
        state.finalizing = true;
        return json({ finalizing: true });
      }
      if (route === "/finalize") {
        assert.equal(state.finalizing, true);
        state.finalized = true;
        return json({ finalized: true });
      }
      if (route === "/retire") {
        assert.equal(state.finalized, true);
        delete state.identity;
        return json({ retired: true });
      }
      if (route === "/attest") {
        if (state.armDelay) state.now += state.armDelay;
        return json({
          version: 1,
          ...state.identity,
          authoritySha,
          authorityVersion: "v1",
          policyHash,
          enrolledAt: state.enrolledAt,
          finalized: state.finalized ?? false,
          ...(state.finalized ? { finalReceipt: { finalCounts: { instances: 0 } } } : {}),
        });
      }
      throw new Error(`unexpected controller fixture route: ${route}`);
    }
    assert.equal(url.hostname, "api.cloudflare.com", "real network access is forbidden");
    const cfRoute = route.replace(/^\/client\/v4\/accounts\/0{32}/, "");
    const match = cfRoute.match(/^\/workers\/scripts\/([^/]+)(.*)$/);
    if (match) {
      const [, name, suffix] = match;
      if (!suffix && method === "PUT") {
        const metadata = JSON.parse(await init.body.get("metadata").text());
        state.workers[name] = {
          metadata,
          subdomain: state.workers[name]?.subdomain ?? { enabled: true, previews_enabled: false },
        };
        if (state.uploadDelay) state.now += state.uploadDelay;
        return api({ deployment_id: workerVersion });
      }
      if (!suffix && method === "DELETE") {
        delete state.workers[name];
        return api({});
      }
      if (suffix === "/settings") return state.workers[name] ? api(state.workers[name].metadata) : api(null, 404);
      if (suffix === "/versions") return api({ items: [{ id: workerVersion }] });
      if (suffix === "/subdomain") {
        if (method === "POST") state.workers[name].subdomain = JSON.parse(init.body);
        return api(state.workers[name].subdomain);
      }
      if (suffix === "/schedules") return api([]);
    }
    if (cfRoute === "/workers/subdomain") return api({ subdomain: "example" });
    if (cfRoute === "/workers/durable_objects/namespaces") {
      return api(
        Object.entries(state.workers)
          .filter(([, worker]) =>
            worker.metadata.bindings.some((binding) => binding.name === "FLEET"),
          )
          .map(([script]) => ({ script, class: "FleetDurableObject" })),
      );
    }
    if (cfRoute.endsWith("/routes") || cfRoute === "/workers/domains/records") return api([]);
    throw new Error(`unexpected Cloudflare fixture route: ${cfRoute}`);
  };
}

function fixture(t, mode = "retained") {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-handoff-test-"));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const artifact = path.join(directory, "candidate");
  const handoffDir = path.join(directory, "handoff");
  const write = (name, data) => {
    const target = path.join(artifact, name);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, typeof data === "string" ? data : JSON.stringify(data));
  };
  // The bundle is inert. Give its installer the supported declaration layout;
  // no candidate CLI, shell source, or build hook is executed by this fixture.
  const sourceFiles = capsulePaths.map((name) => {
    const data =
      name === "scripts/install-linux-developer-tools.sh"
        ? ["node_pnpm", "go", "bun", "rust", "uv"]
            .map((tool) => `${tool}_smoke_script() {\n  echo fixture\n}\n`)
            .join("")
        : fs.readFileSync(path.join(root, name), "utf8");
    write(`candidate/${name}`, data);
    return {
      path: name,
      mode: "100644",
      blob: crypto.createHash("sha1").update(`blob ${Buffer.byteLength(data)}\0${data}`).digest("hex"),
      bytes: Buffer.byteLength(data),
      sha256: digest(data),
    };
  });
  const capsuleSha256 = digest(canonical(sourceFiles));
  write("build-inputs.json", {
    version: 1,
    candidateSha,
    workflowSha,
    workerSourceSha256: "1".repeat(64),
    protectedBuildInputsSha256: "2".repeat(64),
  });
  if (mode === "retained") {
    write("retained-capsule.json", {
      version: 1,
      sourceSha: "3".repeat(40),
      candidateSha,
      sourceCapsuleSha256: capsuleSha256,
      sourceFiles,
      candidateFiles: sourceFiles,
      bakedInputsIdentical: true,
    });
  }
  write("bin/crabbox", "not executable");
  write("worker/index.js", "throw new Error('candidate must not execute');");
  const files = fs.readdirSync(artifact, { recursive: true })
    .filter((name) => fs.statSync(path.join(artifact, name)).isFile())
    .map((name) => ({
      path: name.split(path.sep).join("/"),
      bytes: fs.statSync(path.join(artifact, name)).size,
      sha256: digest(fs.readFileSync(path.join(artifact, name))),
    }));
  const manifest = { version: 1, candidateSha, workflowSha, mainModule: "worker/index.js", files };
  write("manifest.json", { ...manifest, manifestSha256: digest(canonical(manifest)) });
  const stateFile = path.join(directory, "state.json");
  fs.writeFileSync(stateFile, JSON.stringify({ now: Date.now(), events: [], workers: {} }));
  const state = () => JSON.parse(fs.readFileSync(stateFile, "utf8"));
  const update = (values) => fs.writeFileSync(stateFile, JSON.stringify({ ...state(), ...values }));
  const outputs = {};
  const env = {
    HOME: directory,
    RUNNER_TEMP: directory,
    GH_TOKEN: "synthetic-github-token",
    GITHUB_REPOSITORY: "example/repo",
    GITHUB_RUN_ID: "42",
    GITHUB_RUN_ATTEMPT: "1",
    GITHUB_WORKFLOW_REF: "example/repo/.github/workflows/image-qualification.yml@refs/heads/main",
    QUALIFICATION_PULL_REQUEST: "7",
    QUALIFICATION_CANDIDATE_SHA: candidateSha,
    QUALIFICATION_WORKFLOW_SHA: workflowSha,
    QUALIFICATION_DEFAULT_BRANCH: "main",
    QUALIFICATION_CONFIRM: "qualify",
    QUALIFICATION_MODE: mode,
    QUALIFICATION_ARTIFACT_DIR: artifact,
    QUALIFICATION_HANDOFF_DIR: handoffDir,
    QUALIFICATION_CANDIDATE_ARTIFACT_ID: "9",
    QUALIFICATION_CANDIDATE_ARTIFACT_DIGEST: `sha256:${"e".repeat(64)}`,
    QUALIFICATION_CANDIDATE_RUN_ID: "42",
    QUALIFICATION_HANDOFF_ARTIFACT_ID: "10",
    QUALIFICATION_HANDOFF_ARTIFACT_DIGEST: `sha256:${"f".repeat(64)}`,
    QUALIFICATION_AUTHORITY_SHA: authoritySha,
    QUALIFICATION_AUTHORITY_VERSION: "v1",
    QUALIFICATION_EXPECTED_POLICY_HASH: policyHash,
    CLOUDFLARE_ACCOUNT_ID: "0".repeat(32),
    QUALIFICATION_AWS_REGION: "eu-west-1",
    QUALIFICATION_SUBNET_ID: "subnet-00000000000000000",
    QUALIFICATION_SECURITY_GROUP_ID: "sg-00000000000000000",
    QUALIFICATION_BASE_AMI_ID: "ami-00000000000000001",
    QUALIFICATION_ROOT_GB: "20",
    QUALIFICATION_HANDOFF_TEST_STATE: stateFile,
    ...(mode === "retained" ? {
      QUALIFICATION_RETAINED_IMAGE_ID: "ami-00000000000000002",
      QUALIFICATION_RETAINED_SNAPSHOT_ID: "snap-00000000000000000",
      QUALIFICATION_RETAINED_SOURCE_SHA: "3".repeat(40),
      QUALIFICATION_RETAINED_CAPSULE_SHA256: capsuleSha256,
    } : {}),
  };
  function invoke(command, overrides = {}) {
    const output = path.join(directory, `${command}.outputs`);
    fs.writeFileSync(output, "");
    const child = spawnSync(process.execPath, ["--import", import.meta.url, control, command], {
      encoding: "utf8",
      timeout: 10_000,
      env: {
        ...env,
        ...(command !== "admit" ? {
          CLOUDFLARE_API_TOKEN: "synthetic-cloudflare-token",
          QUALIFICATION_CONTROLLER_TOKEN: "synthetic-controller-token",
        } : {}),
        QUALIFICATION_HANDOFF_SHA256: outputs.handoff_sha256 ?? "",
        GITHUB_OUTPUT: output,
        GITHUB_STEP_SUMMARY: path.join(directory, "summary"),
        QUALIFICATION_PROOF_DIR: path.join(directory, `${command}-proof`),
        ...overrides,
      },
    });
    assert.equal(child.error, undefined);
    for (const line of fs.readFileSync(output, "utf8").trim().split("\n").filter(Boolean)) {
      const split = line.indexOf("=");
      outputs[line.slice(0, split)] = line.slice(split + 1);
    }
    return child;
  }
  const prepared = () => JSON.parse(fs.readFileSync(path.join(handoffDir, "handoff.json"), "utf8"));
  const armEnv = () => Object.fromEntries([
    ["RUN_ID", "run_id"], ["CANDIDATE_SHA", "candidate_sha"],
    ["CANDIDATE_WORKER", "candidate_worker"], ["CANDIDATE_VERSION", "candidate_version"],
    ["RELAY_WORKER", "relay_worker"], ["RELAY_VERSION", "relay_version"],
    ["RELAY_BINDING_DIGEST", "relay_binding_digest"], ["DEPLOYMENT_HASH", "deployment_hash"],
    ["MANIFEST_SHA", "manifest_sha"], ["BINDING_DIGEST", "binding_digest"],
    ["AUTHORITY_SHA", "authority_sha"], ["AUTHORITY_VERSION", "authority_version"],
    ["POLICY_HASH", "policy_hash"], ["ENROLLED_AT", "enrolled_at"],
    ["EXPIRES_AT", "expires_at"], ["EXECUTION_MANIFEST_DIGEST", "execution_manifest_digest"],
  ].map(([key, output]) => [`QUALIFICATION_EXPECTED_${key}`, outputs[output]]));
  return { invoke, state, update, prepared, env, outputs, armEnv, handoffDir, directory };
}

if (process.env.QUALIFICATION_HANDOFF_TEST_STATE) {
  installAPI(process.env.QUALIFICATION_HANDOFF_TEST_STATE);
} else {
  for (const mode of ["retained", "mint"]) {
    test(`${mode} CLI publishes one immutable handoff before deployment and preserves it through arm`, (t) => {
      const f = fixture(t, mode);
      const admitted = f.invoke("admit");
      assert.equal(admitted.status, 0, admitted.stderr);
      assert.ok(fs.existsSync(path.join(f.handoffDir, "handoff.json")), "admission must export identity before any credential-dependent claim");
      const handoff = f.prepared();
      assert.equal(f.state().events.some((event) => /cloudflare|workers\.dev/.test(event)), false);
      assert.equal(handoff.owner, "image-qualification-42-1@example.invalid");
      assert.equal(Date.parse(handoff.expiresAt) - Date.parse(handoff.startsAt), (mode === "retained" ? 38 : 120) * 60_000);
      const projection = path.join(f.handoffDir, "iam-inputs.json");
      assert.equal(fs.existsSync(projection), mode === "retained");
      if (mode === "retained") {
        assert.deepEqual(JSON.parse(fs.readFileSync(projection, "utf8")), {
          authority_owner: handoff.owner, run_id: handoff.runId, source_sha: candidateSha,
          starts_at: handoff.startsAt, creation_expires_at: handoff.workExpiresAt,
          cleanup_expires_at: handoff.expiresAt,
        });
      }
      const publicText = fs.readFileSync(path.join(f.handoffDir, "handoff.json"), "utf8") + fs.readFileSync(path.join(f.directory, "summary"), "utf8");
      for (const privateValue of [f.env.CLOUDFLARE_ACCOUNT_ID, f.env.QUALIFICATION_SUBNET_ID, f.env.QUALIFICATION_SECURITY_GROUP_ID, f.env.QUALIFICATION_BASE_AMI_ID]) {
        assert.equal(publicText.includes(privateValue), false);
      }
      assert.notEqual(f.invoke("admit").status, 0, "preparation cannot replace its output");
      f.update({ now: f.state().now + 120_000 });
      const deployed = f.invoke("deploy");
      assert.equal(deployed.status, 0, deployed.stderr);
      assert.equal(f.state().identity.expiresAt, handoff.expiresAt);
      assert.equal(f.state().identity.owner, handoff.owner);
      assert.equal(f.outputs.expires_at, handoff.expiresAt);
      const armed = f.invoke("arm", f.armEnv());
      assert.equal(armed.status, 0, armed.stderr);
      assert.deepEqual(f.prepared(), handoff);
    });
  }

  const providerEvents = (f) => f.state().events.filter((event) => /cloudflare|workers\.dev/.test(event));
  for (const [name, overrides] of [
    ["rerun", { GITHUB_RUN_ATTEMPT: "2" }],
    ["other run", { GITHUB_RUN_ID: "43" }],
    ["other repository", { GITHUB_REPOSITORY: "example/other" }],
    ["other workflow", { QUALIFICATION_WORKFLOW_SHA: "9".repeat(40) }],
    ["other subnet", { QUALIFICATION_SUBNET_ID: "subnet-11111111111111111" }],
    ["other Cloudflare account", { CLOUDFLARE_ACCOUNT_ID: "9".repeat(32) }],
    ["other authority", { QUALIFICATION_AUTHORITY_SHA: "9".repeat(40) }],
    ["other capsule", { QUALIFICATION_RETAINED_CAPSULE_SHA256: "9".repeat(64) }],
    ["wrong artifact digest", { QUALIFICATION_HANDOFF_ARTIFACT_DIGEST: `sha256:${"9".repeat(64)}` }],
  ]) {
    test(`deployment refuses ${name} before provider effects`, (t) => {
      const f = fixture(t);
      assert.equal(f.invoke("admit").status, 0);
      assert.notEqual(f.invoke("deploy", overrides).status, 0);
      assert.deepEqual(providerEvents(f), []);
    });
  }

  for (const mutation of [
    { startsAt: "not-a-date" },
    { startsAt: "2999-01-01T00:00:00.000Z" },
    { expiresAt: "2999-01-01T00:00:00.000Z" },
    { workExpiresAt: "2999-01-01T00:00:00.000Z" },
    { owner: "other@example.invalid" },
    { attempt: 2 },
    { privateAccount: "should-not-be-here" },
  ]) {
    test(`handoff schema and derivation reject ${Object.keys(mutation)[0]}`, (t) => {
      const f = fixture(t);
      assert.equal(f.invoke("admit").status, 0);
      const changed = { ...f.prepared(), ...mutation };
      fs.writeFileSync(path.join(f.handoffDir, "handoff.json"), JSON.stringify(changed));
      // Even a matching file digest cannot authorize noncanonical producer fields.
      f.outputs.handoff_sha256 = digest(canonical(changed));
      assert.notEqual(f.invoke("deploy").status, 0);
      assert.deepEqual(providerEvents(f), []);
    });
  }

  for (const metadata of [
    { name: "other-handoff" }, { expired: true }, { workflow_run: { id: 43, head_sha: workflowSha } },
    { workflow_run: { id: 42, head_sha: candidateSha } },
  ]) {
    test(`deployment rejects substituted handoff artifact ${JSON.stringify(metadata)}`, (t) => {
      const f = fixture(t);
      assert.equal(f.invoke("admit").status, 0);
      f.update({ handoffMetadata: metadata });
      assert.notEqual(f.invoke("deploy").status, 0);
      assert.deepEqual(providerEvents(f), []);
    });
  }

  test("preparation refuses credentials, including empty inherited bindings", (t) => {
    const f = fixture(t);
    const result = f.invoke("admit", { AWS_ACCESS_KEY_ID: "" });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /must be absent from preparation/);
    assert.equal(fs.existsSync(path.join(f.handoffDir, "handoff.json")), false);
    assert.deepEqual(providerEvents(f), []);
  });

  test("missing preparation cannot be synthesized by deployment", (t) => {
    const f = fixture(t);
    assert.notEqual(f.invoke("deploy").status, 0);
    assert.deepEqual(providerEvents(f), []);
  });

  test("approval delay consumes the work window before any deployment", (t) => {
    const f = fixture(t);
    assert.equal(f.invoke("admit").status, 0);
    f.update({ now: Date.parse(f.prepared().workExpiresAt) });
    assert.match(f.invoke("deploy").stderr, /work window is not active/);
    assert.deepEqual(providerEvents(f), []);
  });

  test("Cloudflare upload delay cannot enroll after the retained work cutoff", (t) => {
    const f = fixture(t);
    assert.equal(f.invoke("admit").status, 0);
    f.update({ uploadDelay: 7 * 60_000 });
    assert.match(f.invoke("deploy").stderr, /work window is not active/);
    assert.equal(f.state().identity, undefined);
    assert.ok(providerEvents(f).length > 0);
    assert.equal(f.state().events.some((event) => event.endsWith("/claim")), false);
  });

  for (const mode of ["retained", "mint"]) {
    test(`${mode} late arm refuses execution without obstructing cancellation cleanup`, (t) => {
      const f = fixture(t, mode);
      assert.equal(f.invoke("admit").status, 0);
      assert.equal(f.invoke("deploy").status, 0);
      const handoff = f.prepared();
      f.update({
        now: Date.parse(handoff.workExpiresAt) - 1,
        armDelay: 1,
      });
      const result = f.invoke("arm", f.armEnv());
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /work window is not active/);
      assert.equal(fs.existsSync(path.join(f.directory, "arm-proof", "execution-manifest.json")), false);
      f.update({ now: Date.parse(handoff.expiresAt) + 60_000, armDelay: 0 });
      const cleanup = f.invoke("reap");
      assert.equal(cleanup.status, 0, cleanup.stderr);
      assert.equal(f.state().finalized, true);
      assert.deepEqual(f.state().workers, {});
      assert.equal(f.state().identity, undefined);
    });
  }
}
