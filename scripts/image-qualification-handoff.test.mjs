import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import { qualificationIdentity } from "./image-qualification-package.mjs";
import { transportProfile } from "./image-qualification-transport.mjs";

const encodeExecutorCredential = (value) =>
  Buffer.from(JSON.stringify(value)).toString("base64url");

// Do not import the CLI before the child's fetch interceptor is installed:
// its entrypoint deliberately runs when argv names the control script.
let canonical;
let digest;
if (!process.env.QUALIFICATION_HANDOFF_TEST_STATE) {
  ({ canonical, digest } = await import("./image-qualification-control.mjs"));
}

import {
  fixtureAdmission,
  namespaceId,
  controllerContent,
} from "./fixtures/qualification-isolation.mjs";

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
  const isolation = fixtureAdmission("0".repeat(32));
  Date.now = () => state.now;
  process.on("exit", () => fs.writeFileSync(file, JSON.stringify(state)));
  const json = (body, status = 200) => new Response(JSON.stringify(body), { status });
  const api = (result, status = 200) => json({ success: status === 200, result }, status);
  globalThis.fetch = async (input, init = {}) => {
    const url = new URL(input);
    const method = init.method ?? "GET";
    if (url.hostname === "api.cloudflare.com" && method === "DELETE")
      assert.equal(url.search, "", "all deletion must preserve dependency checks");
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
      if (route === `/repos/example/repo/compare/${candidateSha}...${workflowSha}`) {
        return json({
          status: "ahead",
          base_commit: { sha: candidateSha },
          merge_base_commit: { sha: state.rejectSource ? "0".repeat(40) : candidateSha },
        });
      }
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
      if (route === "/transport-open") {
        state.probe = { protocol: 1, ...JSON.parse(init.body), state: "pending" };
        return json(state.probe);
      }
      if (route === "/transport-discover") return json({ probe: state.probe ?? null });
      if (route === "/transport-settle") {
        const input = JSON.parse(init.body);
        assert.equal(input.challenge, state.probe.challenge);
        state.probe.state = input.verified ? "verified" : "cancelled";
        return json(state.probe);
      }
      if (route === "/v1/qualification/transport-probe") {
        assert.equal(JSON.parse(init.body).challenge, state.probe.challenge);
        if (!state.forgeProbe) {
          state.probe.state = "observed";
          state.probe.observedAt = new Date(state.now).toISOString();
        }
        return json({ identity: state.probe.identity, challenge: state.probe.challenge });
      }
      if (route === "/claim") {
        if (state.probe) assert.equal(state.probe.state, "verified");
        state.identity = JSON.parse(init.body).identity;
        state.enrolledAt = new Date(state.now).toISOString();
        return json({ ...state.identity, cleanupState: "claimed" });
      }
      if (route === "/discover") {
        return json({
          run: state.identity ? { ...state.identity, cleanupState: "claimed" } : null,
        });
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
    if (cfRoute.startsWith("/workers/scripts/crabbox-aws-qualification-authority/")) {
      if (cfRoute.endsWith("/settings")) return api({ bindings: isolation.bindings });
      if (cfRoute.endsWith("/deployments"))
        return api({
          deployments: [{ versions: [{ version_id: workerVersion, percentage: 100 }] }],
        });
      if (cfRoute.endsWith("/subdomain")) return api({ enabled: false, previews_enabled: false });
      if (cfRoute.endsWith("/schedules")) return api({ schedules: [] });
    }
    const match = cfRoute.match(/^\/workers\/scripts\/([^/]+)(.*)$/);
    if (match) {
      const [, name, suffix] = match;
      if (!suffix && method === "PUT") {
        if (state.echoExecutorToken)
          throw new Error(
            `upload failed: ${JSON.parse(Buffer.from(process.env.QUALIFICATION_EXECUTOR_CREDENTIAL, "base64url").toString("utf8")).token}`,
          );
        const isRelay = name.startsWith("crabbox-image-qualification-relay-");
        if (isRelay && state.relayFault === "put-before")
          throw new Error("relay upload failed before create");
        const metadata = JSON.parse(await init.body.get("metadata").text());
        // Validate the wire format, rather than accepting Wrangler config here.
        // The upload API takes one migration object; config uses an array.
        if (metadata.main_module === "private-bootstrap.mjs") {
          assert.deepEqual(metadata.migrations, {
            new_tag: "qualification-v1",
            new_sqlite_classes: ["FleetDurableObject"],
          });
        } else if (metadata.main_module === "tombstone.mjs") {
          assert.deepEqual(metadata.keep_bindings, ["secret_text"]);
          assert.ok(!metadata.bindings.some((b) => b.type === "secret_text"));
          assert.deepEqual(metadata.migrations, {
            old_tag: "qualification-v1",
            new_tag: "qualification-delete",
            deleted_classes: ["FleetDurableObject"],
          });
        } else {
          assert.equal(metadata.migrations, undefined);
        }
        if (
          metadata.main_module === "private-bootstrap.mjs" &&
          state.failPrivateUpload === "before"
        )
          throw new Error("private upload failed before create");
        for (const binding of metadata.bindings)
          if (binding.name === "FLEET") binding.namespace_id ??= namespaceId(name);
        const profile = transportProfile(metadata.bindings);
        if (profile) {
          state.profileHistory ??= [];
          state.profileHistory.push({
            name,
            profile: profile.profile,
            services: metadata.bindings.filter((b) => b.type === "service").length,
          });
        }
        state.workers[name] = {
          metadata: {
            ...metadata,
            bindings: [
              ...metadata.bindings.map((b) => {
                if (b.type !== "secret_text") return b;
                const { text, ...shape } = b;
                return shape;
              }),
              ...(state.workers[name]?.metadata.bindings ?? []).filter(
                (b) =>
                  b.type === "secret_text" && !metadata.bindings.some((n) => n.name === b.name),
              ),
            ],
          },
          subdomain: state.workers[name]?.subdomain ?? { enabled: true, previews_enabled: false },
        };
        if (metadata.main_module === "private-bootstrap.mjs" && state.failPrivateUpload === "after")
          throw new Error("private upload response lost after create");
        if (state.tombstoneDrift && metadata.main_module === "tombstone.mjs")
          state.workers[name].metadata.bindings.push({
            name: "UNEXPECTED_SECRET",
            type: "secret_text",
          });
        if (state.tombstoneLoss && metadata.main_module === "tombstone.mjs") {
          state.tombstoneLoss = false;
          throw new Error("tombstone upload response lost");
        }
        if (state.uploadDelay) state.now += state.uploadDelay;
        if (isRelay && state.relayFault === "put-lost")
          throw new Error("relay upload response lost after create");
        return api({
          deployment_id: isRelay && state.relayFault === "ack-invalid" ? "invalid" : workerVersion,
        });
      }
      if (!suffix && method === "DELETE") {
        delete state.workers[name];
        return api({});
      }
      const relayRead =
        name.startsWith("crabbox-image-qualification-relay-") &&
        state.workers[name] &&
        method === "GET" &&
        ["/settings", "/deployments"].includes(suffix);
      if (relayRead && state.relayFault) {
        state.relayReads ??= {};
        state.relayReads[suffix] = (state.relayReads[suffix] ?? 0) + 1;
        const count = state.relayReads[suffix];
        const fault = state.relayFault;
        if (fault === "404-both" && count === 1) return api(null, 404);
        if (fault === "404-settings" && suffix === "/settings" && count === 1)
          return api(null, 404);
        if (fault === "404-deployments" && suffix === "/deployments" && count === 1)
          return api(null, 404);
        if (fault === "404-persistent") return api(null, 404);
        if (fault === "404-settings-wrong-version" && suffix === "/settings") return api(null, 404);
        if (fault === "wrong-version" || fault === "404-settings-wrong-version") {
          if (suffix === "/deployments")
            return api({
              deployments: [
                {
                  versions: [
                    { version_id: "22222222-2222-4222-8222-222222222222", percentage: 100 },
                  ],
                },
              ],
            });
        }
        if (suffix === "/settings") {
          if (fault === "denied") return api(null, 403);
          if (fault === "unauthorized") return api(null, 401);
          if (fault === "server-error") return api(null, 503);
          if (fault === "malformed-json") return new Response("not JSON");
          if (fault === "oversize") return new Response("x".repeat(70 * 1024));
          if (fault === "malformed-settings") return api({ tags: [], bindings: null });
          if (fault === "deadline") state.now += 10_000;
          if (fault === "clipped-deadline") state.now += 1001;
          if (fault === "cancel") process.emit("SIGTERM");
          if (fault === "cancel-backoff") {
            setTimeout(() => process.emit("SIGTERM"), 25);
            return api(null, 404);
          }
          if (fault === "cancel-body")
            return new Response(
              new ReadableStream({
                start(controller) {
                  const cancel = () => controller.error(init.signal.reason);
                  init.signal.addEventListener("abort", cancel, { once: true });
                  setTimeout(() => process.emit("SIGTERM"), 25);
                },
              }),
            );
          if (fault === "hung-fetch")
            return await new Promise((resolve, reject) => {
              assert(init.signal, "readback must bound requests");
              const keepAlive = setInterval(() => {}, 1000);
              init.signal.addEventListener(
                "abort",
                () => {
                  clearInterval(keepAlive);
                  reject(init.signal.reason);
                },
                { once: true },
              );
            });
        }
        if (fault === "malformed-deployments" && suffix === "/deployments")
          return api({ deployments: [] });
      }
      if (suffix === "/settings") {
        if (!state.workers[name]) return api(null, 404);
        const metadata = structuredClone(state.workers[name].metadata);
        if (relayRead && state.relayFault === "ownership-drift") metadata.tags = [];
        if (relayRead && state.relayFault === "binding-drift")
          metadata.bindings.push({ name: "UNEXPECTED", type: "plain_text", text: "extra" });
        if (transportProfile(metadata.bindings))
          for (const binding of metadata.bindings) {
            if (binding.type === "service") {
              delete binding.props;
              binding.environment = "production";
            }
          }
        return api(metadata);
      }
      if (suffix === "/deployments")
        return api({
          deployments: [{ versions: [{ version_id: workerVersion, percentage: 100 }] }],
        });
      if (suffix === "/content/v2")
        return controllerContent(
          fs.readFileSync(path.join(root, "scripts/image-qualification-controller-worker.mjs")),
        );
      if (suffix === "/subdomain") {
        if (method === "POST") state.workers[name].subdomain = JSON.parse(init.body);
        return api(state.workers[name].subdomain);
      }
      if (suffix === "/schedules") return api({ schedules: [] });
    }
    if (cfRoute === "/workers/scripts")
      return api(Object.keys(state.workers).map((id) => ({ id })));
    if (cfRoute === "/workers/subdomain") return api({ subdomain: "example" });
    if (cfRoute === "/workers/durable_objects/namespaces") {
      return api([
        ...isolation.record.authority.namespaces,
        ...Object.entries(state.workers)
          .filter(([, worker]) =>
            worker.metadata.bindings.some((binding) => binding.name === "FLEET"),
          )
          .map(([script]) => ({ id: namespaceId(script), script, class: "FleetDurableObject" })),
      ]);
    }
    if (cfRoute.endsWith("/routes") || cfRoute === "/workers/domains") return api([]);
    throw new Error(`unexpected Cloudflare fixture route: ${cfRoute}`);
  };
}

function fixture(t, mode = "retained") {
  const directory = fs.realpathSync(
    fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-handoff-test-")),
  );
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
      blob: crypto
        .createHash("sha1")
        .update(`blob ${Buffer.byteLength(data)}\0${data}`)
        .digest("hex"),
      bytes: Buffer.byteLength(data),
      sha256: digest(data),
    };
  });
  const capsuleSha256 = digest(canonical(sourceFiles));
  const qualification = mode === "first-walk" ? qualificationIdentity(root) : undefined;
  write("build-inputs.json", {
    version: qualification ? 2 : 1,
    ...(qualification ? { qualification } : {}),
    candidateSha,
    workflowSha,
    workerSourceSha256: qualification?.workerSourceSha256 ?? "1".repeat(64),
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
  if (mode === "first-walk") {
    write(
      "authority/aws-qualification-authority.js",
      "throw new Error('inert authority fixture');",
    );
    write(
      "candidate/recipes/bootstrap/v1/linuxOwnershipRepair.sh",
      fs.readFileSync(path.join(root, "scripts/fixtures/first-walk-reviewed-recipe.sh"), "utf8"),
    );
  }
  write("bin/crabbox", "not executable");
  write("worker/index.js", "throw new Error('candidate must not execute');");
  const files = fs
    .readdirSync(artifact, { recursive: true })
    .filter((name) => fs.statSync(path.join(artifact, name)).isFile())
    .map((name) => ({
      path: name.split(path.sep).join("/"),
      bytes: fs.statSync(path.join(artifact, name)).size,
      sha256: digest(fs.readFileSync(path.join(artifact, name))),
    }));
  const manifest = {
    version: qualification ? 2 : 1,
    ...(qualification ? { qualification } : {}),
    candidateSha,
    workflowSha,
    mainModule: "worker/index.js",
    files,
  };
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
    GITHUB_REPOSITORY_ID: "123",
    GITHUB_EVENT_NAME: "workflow_dispatch",
    GITHUB_REF: "refs/heads/main",
    GITHUB_SHA: workflowSha,
    GITHUB_RUN_ID: "42",
    GITHUB_RUN_ATTEMPT: "1",
    GITHUB_WORKFLOW_REF: "example/repo/.github/workflows/image-qualification.yml@refs/heads/main",
    QUALIFICATION_PULL_REQUEST: mode === "first-walk" ? "" : "7",
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
    ...fixtureAdmission("0".repeat(32)).env,
    QUALIFICATION_AWS_REGION: "eu-west-1",
    QUALIFICATION_SUBNET_ID: "subnet-00000000000000000",
    QUALIFICATION_SECURITY_GROUP_ID: "sg-00000000000000000",
    QUALIFICATION_BASE_AMI_ID: "ami-00000000000000001",
    QUALIFICATION_ROOT_GB: "20",
    QUALIFICATION_HANDOFF_TEST_STATE: stateFile,
    ...(mode === "retained"
      ? {
          QUALIFICATION_RETAINED_IMAGE_ID: "ami-00000000000000002",
          QUALIFICATION_RETAINED_SNAPSHOT_ID: "snap-00000000000000000",
          QUALIFICATION_RETAINED_SOURCE_SHA: "3".repeat(40),
          QUALIFICATION_RETAINED_CAPSULE_SHA256: capsuleSha256,
        }
      : {}),
  };
  function invoke(command, overrides = {}) {
    const output = path.join(directory, `${command}.outputs`);
    fs.writeFileSync(output, "");
    const child = spawnSync(process.execPath, ["--import", import.meta.url, control, command], {
      encoding: "utf8",
      timeout: 15_000,
      env: {
        ...env,
        ...(command !== "admit"
          ? {
              CLOUDFLARE_API_TOKEN: "synthetic-cloudflare-token",
              QUALIFICATION_CONTROLLER_TOKEN: "synthetic-controller-token",
            }
          : {}),
        ...(command === "deploy" && fs.existsSync(path.join(handoffDir, "executor-identity.json"))
          ? {
              QUALIFICATION_EXECUTOR_CREDENTIAL: encodeExecutorCredential({
                ...JSON.parse(
                  fs.readFileSync(path.join(handoffDir, "executor-identity.json"), "utf8"),
                ),
                token: "6".repeat(64),
              }),
            }
          : {}),
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
  const armEnv = () =>
    Object.fromEntries(
      [
        ["RUN_ID", "run_id"],
        ["CANDIDATE_SHA", "candidate_sha"],
        ["CANDIDATE_WORKER", "candidate_worker"],
        ["CANDIDATE_VERSION", "candidate_version"],
        ["RELAY_WORKER", "relay_worker"],
        ["RELAY_VERSION", "relay_version"],
        ["RELAY_BINDING_DIGEST", "relay_binding_digest"],
        ["DEPLOYMENT_HASH", "deployment_hash"],
        ["MANIFEST_SHA", "manifest_sha"],
        ["BINDING_DIGEST", "binding_digest"],
        ["AUTHORITY_SHA", "authority_sha"],
        ["AUTHORITY_VERSION", "authority_version"],
        ["POLICY_HASH", "policy_hash"],
        ["ENROLLED_AT", "enrolled_at"],
        ["EXPIRES_AT", "expires_at"],
        ["EXECUTION_MANIFEST_DIGEST", "execution_manifest_digest"],
      ].map(([key, output]) => [`QUALIFICATION_EXPECTED_${key}`, outputs[output]]),
    );
  return { invoke, state, update, prepared, env, outputs, armEnv, handoffDir, directory };
}

if (process.env.QUALIFICATION_HANDOFF_TEST_STATE) {
  installAPI(process.env.QUALIFICATION_HANDOFF_TEST_STATE);
} else {
  for (const fault of [
    "404-settings",
    "404-deployments",
    "404-both",
    "404-persistent",
    "404-settings-wrong-version",
    "wrong-version",
    "binding-drift",
    "ownership-drift",
    "denied",
    "unauthorized",
    "server-error",
    "malformed-json",
    "malformed-settings",
    "malformed-deployments",
    "oversize",
    "deadline",
    "clipped-deadline",
    "cancel",
    "cancel-backoff",
    "cancel-body",
    "hung-fetch",
    "put-lost",
    "put-before",
    "ack-invalid",
  ]) {
    test(`relay acknowledged-upload readback: ${fault}`, (t) => {
      const f = fixture(t, "first-walk");
      f.env.QUALIFICATION_TRANSPORT_PROFILE = "candidate-transport-v2";
      assert.equal(f.invoke("admit").status, 0);
      f.update({ relayFault: fault });
      if (fault === "clipped-deadline")
        f.update({ now: Date.parse(f.prepared().workExpiresAt) - 61_000 });
      const started = performance.now();
      const deployed = f.invoke("deploy");
      const elapsed = performance.now() - started;
      const success = ["404-settings", "404-deployments", "404-both"].includes(fault);
      assert.equal(deployed.status, success ? 0 : 1, deployed.stderr);
      const relay = "crabbox-image-qualification-relay-42-1";
      const puts = () =>
        f
          .state()
          .events.filter(
            (event) =>
              event ===
              `PUT api.cloudflare.com/client/v4/accounts/${"0".repeat(32)}/workers/scripts/${relay}`,
          ).length;
      assert.equal(puts(), 1, "no create/upload replay");
      const receiptPath = path.join(f.directory, "deploy-proof/relay-readback.json");
      if (["put-lost", "put-before"].includes(fault)) {
        assert.equal(
          fs.existsSync(receiptPath),
          false,
          "uncertain PUT cannot enter acknowledged path",
        );
        assert.match(deployed.stderr, /relay upload/);
      } else {
        const receipt = JSON.parse(fs.readFileSync(receiptPath));
        assert.equal(receipt.state, success ? "verified" : "unresolved");
        assert.equal(receipt.upload.status, 200);
        assert(receipt.reads.length <= 12);
        if (fault !== "ack-invalid") assert.equal(receipt.acknowledgedVersion, workerVersion);
        if (fault === "404-persistent") {
          assert.equal(receipt.reads.length, 12);
          assert(receipt.reads.every((r) => r.state === "http404" && r.response.status === 404));
        } else if (!success) {
          assert(
            receipt.reads.length <= 2,
            "visible drift, denial, malformed data and cancellation fail immediately",
          );
        }
        if (fault === "denied") assert.equal(receipt.reads[0].response.status, 403);
        const bytes = fs.readFileSync(receiptPath, "utf8");
        for (const secret of [
          "synthetic-cloudflare-token",
          "synthetic-controller-token",
          "6".repeat(64),
        ])
          assert(!bytes.includes(secret), "receipt must not contain credentials");
        if (fault === "hung-fetch") {
          assert(elapsed >= 9500 && elapsed < 13_000, `request timeout elapsed ${elapsed}`);
          assert.equal(receipt.reads.length, 1);
        }
      }
      if (!success)
        assert.equal(f.state().identity, undefined, "no authority claim on unresolved readback");
      if (fault === "404-persistent") {
        const refused = f.invoke("reap");
        assert.equal(refused.status, 1);
        assert(f.state().workers[relay], "unknown ownership must still block deletion");
      }
      // Explicit restoration of metadata in this model, never a live cause claim.
      f.update({ relayFault: undefined });
      const settled = f.invoke("reap");
      assert.equal(settled.status, 0, settled.stderr);
      assert.deepEqual(f.state().workers, {});
      assert.equal(f.state().identity, undefined);
      assert.equal(puts(), 1);
    });
  }

  test("exhausted readback reserve refuses relay upload and remains recoverable", (t) => {
    const f = fixture(t, "first-walk");
    assert.equal(f.invoke("admit").status, 0);
    f.update({ now: Date.parse(f.prepared().workExpiresAt) - 60_000 });
    const deployed = f.invoke("deploy");
    assert.equal(deployed.status, 1);
    assert.match(deployed.stderr, /subsequent-gate reserve/);
    assert(
      !f.state().events.some((event) => event.startsWith("PUT ") && event.includes("relay-42-1")),
    );
    assert.equal(f.state().identity, undefined);
    const settled = f.invoke("reap");
    assert.equal(settled.status, 0, settled.stderr);
    assert.deepEqual(f.state().workers, {});
  });

  test("first-walk source-admission failure still fences and settles the admitted run", (t) => {
    const f = fixture(t, "first-walk");
    assert.equal(f.invoke("admit").status, 0);
    assert.equal(f.invoke("deploy").status, 0);
    assert.equal(f.invoke("arm", f.armEnv()).status, 0);
    assert.ok(f.state().events.every((event) => !event.includes("/pulls/")));
    f.update({ rejectSource: true });
    const result = f.invoke("finalize", f.armEnv());
    assert.equal(result.status, 1, "admission drift must remain qualification failure");
    assert.match(result.stderr, /source admission/);
    assert.equal(f.state().finalized, true);
    assert.deepEqual(f.state().workers, {});
    assert.equal(f.state().identity, undefined);
    assert.ok(f.state().events.some((event) => event.endsWith("/begin-finalization")));
  });

  for (const phase of ["before", "after"])
    test(`failed private upload ${phase} create settles with empty deploy outputs`, (t) => {
      const f = fixture(t, "first-walk");
      f.env.QUALIFICATION_TRANSPORT_PROFILE = "candidate-transport-v2";
      assert.equal(f.invoke("admit").status, 0);
      f.update({ failPrivateUpload: phase });
      assert.equal(f.invoke("deploy").status, 1);
      assert.equal(f.outputs.run_id, undefined);
      assert.equal(Object.keys(f.state().workers).length, phase === "after" ? 1 : 0);
      const missingOutputs = Object.fromEntries(Object.keys(f.armEnv()).map((key) => [key, ""]));
      for (const policyHash of ["", "0".repeat(64)]) {
        const before = f
          .state()
          .events.filter((event) => /^[A-Z]+ api\.cloudflare\.com\//.test(event));
        const refused = f.invoke("finalize", {
          ...missingOutputs,
          QUALIFICATION_EXPECTED_POLICY_HASH: policyHash,
        });
        assert.equal(refused.status, 1);
        assert.match(refused.stderr, /QUALIFICATION_EXPECTED_POLICY_HASH differs from admission/);
        assert.deepEqual(
          f.state().events.filter((event) => /^[A-Z]+ api\.cloudflare\.com\//.test(event)),
          before,
          "missing or mismatched policy must still fail before provider effects",
        );
      }
      const result = f.invoke("finalize", {
        ...missingOutputs,
        // Mirrors the protected workflow's fallback when deploy has no output.
        QUALIFICATION_EXPECTED_POLICY_HASH: f.env.QUALIFICATION_EXPECTED_POLICY_HASH,
      });
      assert.equal(result.status, 1, "failed qualification must not become successful");
      const receipt = JSON.parse(
        fs.readFileSync(path.join(f.directory, "finalize-proof/finalization.json"), "utf8"),
      );
      assert.equal(receipt.cleanup, "idle");
      assert.deepEqual(f.state().workers, {});
      assert.equal(f.state().identity, undefined);
      assert.equal(
        f.state().events.some((event) => event.endsWith("/claim")),
        false,
      );
    });

  for (const forgeProbe of [false, true])
    test(`v2 CLI canonical proof and recovery with omitted props; forgedReply=${forgeProbe}`, (t) => {
      const f = fixture(t, "first-walk");
      f.env.QUALIFICATION_TRANSPORT_PROFILE = "candidate-transport-v2";
      f.update({ forgeProbe });
      assert.equal(f.invoke("admit").status, 0);
      const deployed = f.invoke("deploy");
      assert.equal(deployed.status, forgeProbe ? 1 : 0, deployed.stderr);
      const state = f.state();
      assert.ok(
        state.profileHistory.filter((p) => p.profile === "stage-v2").every((p) => p.services === 0),
      );
      assert.equal(state.profileHistory.find((p) => p.profile === "final-v2").services, 1);
      const claim = state.events.findIndex((event) => event.endsWith("/claim"));
      if (forgeProbe) assert.equal(claim, -1);
      else {
        assert.ok(state.events.findIndex((event) => event.endsWith("/transport-settle")) < claim);
        const armed = f.invoke("arm", f.armEnv());
        assert.equal(armed.status, 0, armed.stderr);
      }
      const cleanup = f.invoke("reap");
      assert.equal(cleanup.status, 0, cleanup.stderr);
      assert.equal(f.state().probe.state, forgeProbe ? "cancelled" : "verified");
      assert.deepEqual(f.state().workers, {});
    });

  for (const mode of ["retained", "mint", "first-walk"]) {
    test(`${mode} CLI publishes one immutable handoff before deployment and preserves it through arm`, (t) => {
      const f = fixture(t, mode);
      const admitted = f.invoke("admit");
      assert.equal(admitted.status, 0, admitted.stderr);
      assert.ok(
        fs.existsSync(path.join(f.handoffDir, "handoff.json")),
        "admission must export identity before any credential-dependent claim",
      );
      const handoff = f.prepared();
      assert.equal(
        f.state().events.some((event) => /cloudflare|workers\.dev/.test(event)),
        false,
      );
      assert.equal(handoff.owner, "image-qualification-42-1@example.invalid");
      assert.equal(
        Date.parse(handoff.expiresAt) - Date.parse(handoff.startsAt),
        (mode === "first-walk" ? 150 : mode === "retained" ? 38 : 120) * 60_000,
      );
      if (mode === "first-walk")
        assert.equal(
          Date.parse(handoff.expiresAt) - Date.parse(handoff.workExpiresAt),
          20 * 60_000,
        );
      const projection = path.join(f.handoffDir, "iam-inputs.json");
      assert.equal(fs.existsSync(projection), mode === "retained");
      if (mode === "retained") {
        assert.deepEqual(JSON.parse(fs.readFileSync(projection, "utf8")), {
          authority_owner: handoff.owner,
          run_id: handoff.runId,
          source_sha: candidateSha,
          starts_at: handoff.startsAt,
          creation_expires_at: handoff.workExpiresAt,
          cleanup_expires_at: handoff.expiresAt,
        });
      }
      const publicText =
        fs.readFileSync(path.join(f.handoffDir, "handoff.json"), "utf8") +
        fs.readFileSync(path.join(f.directory, "summary"), "utf8");
      for (const privateValue of [
        f.env.CLOUDFLARE_ACCOUNT_ID,
        f.env.QUALIFICATION_SUBNET_ID,
        f.env.QUALIFICATION_SECURITY_GROUP_ID,
        f.env.QUALIFICATION_BASE_AMI_ID,
      ]) {
        assert.equal(publicText.includes(privateValue), false);
      }
      assert.notEqual(f.invoke("admit").status, 0, "preparation cannot replace its output");
      f.update({ now: f.state().now + 120_000 });
      const deployed = f.invoke("deploy");
      assert.equal(deployed.status, 0, deployed.stderr);
      assert.equal(f.state().identity.expiresAt, handoff.expiresAt);
      assert.equal(f.state().identity.owner, handoff.owner);
      assert.equal(f.outputs.expires_at, handoff.expiresAt);
      assert.equal(f.outputs.executor_token_sha256, digest("6".repeat(64)));
      assert.equal(f.outputs.executor_token, undefined);
      const proof = fs.readFileSync(
        path.join(f.directory, "deploy-proof", "deployment.json"),
        "utf8",
      );
      assert.equal(JSON.parse(proof).executorTokenSha256, digest("6".repeat(64)));
      assert.equal(proof.includes("6".repeat(64)), false);
      assert.equal(
        fs.readFileSync(path.join(f.directory, "deploy.outputs"), "utf8").includes("6".repeat(64)),
        false,
      );
      const armed = f.invoke("arm", f.armEnv());
      assert.equal(armed.status, 0, armed.stderr);
      assert.deepEqual(f.prepared(), handoff);
    });
  }

  const providerEvents = (f) =>
    f.state().events.filter((event) => /cloudflare|workers\.dev/.test(event));
  for (const field of [
    "repositoryId",
    "workflowSha",
    "candidateSha",
    "runId",
    "runAttempt",
    "handoffSha256",
    "expiresAt",
  ]) {
    test(`deployment rejects manual credential ${field} mismatch before provider effects`, (t) => {
      const f = fixture(t);
      assert.equal(f.invoke("admit").status, 0);
      const identity = JSON.parse(
        fs.readFileSync(path.join(f.handoffDir, "executor-identity.json"), "utf8"),
      );
      assert.equal(Object.hasOwn(identity, "token"), false);
      const credential = encodeExecutorCredential({
        ...identity,
        [field]: "wrong",
        token: "6".repeat(64),
      });
      const result = f.invoke("deploy", { QUALIFICATION_EXECUTOR_CREDENTIAL: credential });
      assert.notEqual(result.status, 0);
      assert.equal(result.stderr.includes("6".repeat(64)), false);
      assert.deepEqual(providerEvents(f), []);
    });
  }
  test("missing manual credential stops deployment before provider effects", (t) => {
    const f = fixture(t);
    assert.equal(f.invoke("admit").status, 0);
    assert.notEqual(f.invoke("deploy", { QUALIFICATION_EXECUTOR_CREDENTIAL: "" }).status, 0);
    assert.deepEqual(providerEvents(f), []);
  });
  test("provider error cannot echo the supplied token into ordinary diagnostics", (t) => {
    const f = fixture(t);
    assert.equal(f.invoke("admit").status, 0);
    f.update({ echoExecutorToken: true });
    const result = f.invoke("deploy");
    assert.notEqual(result.status, 0);
    assert.equal(result.stderr.includes("6".repeat(64)), false);
    assert.match(result.stderr, /upload failed: \[executor-token\]/);
    assert.equal(f.state().identity, undefined);
    assert.deepEqual(f.state().workers, {});
  });
  test("executor credential mismatch after enrollment does not obstruct independent recovery", (t) => {
    const f = fixture(t, "first-walk");
    assert.equal(f.invoke("admit").status, 0);
    assert.equal(f.invoke("deploy").status, 0);
    const identity = JSON.parse(
      fs.readFileSync(path.join(f.handoffDir, "executor-identity.json"), "utf8"),
    );
    const child = spawnSync(
      process.execPath,
      [path.join(root, "scripts/image-qualification-executor-token.mjs")],
      {
        encoding: "utf8",
        timeout: 10_000,
        env: {
          ...f.env,
          QUALIFICATION_HANDOFF_SHA256: f.outputs.handoff_sha256,
          QUALIFICATION_EXPIRES_AT: f.outputs.expires_at,
          QUALIFICATION_RELAY_URL: f.outputs.relay_url,
          QUALIFICATION_EXECUTOR_TOKEN_SHA256: f.outputs.executor_token_sha256,
          QUALIFICATION_EXECUTOR_CREDENTIAL: encodeExecutorCredential({
            ...identity,
            token: "7".repeat(64),
          }),
        },
      },
    );
    assert.equal(child.status, 1);
    assert.match(child.stderr, /authentication failed/);
    f.update({ now: Date.parse(f.prepared().expiresAt) + 60_000 });
    const cleanup = f.invoke("reap");
    assert.equal(cleanup.status, 0, cleanup.stderr);
    assert.equal(f.state().finalized, true);
    assert.deepEqual(f.state().workers, {});
    assert.equal(f.state().identity, undefined);
  });
  test("empty workflow transport selector preserves legacy deployment", (t) => {
    const f = fixture(t, "first-walk");
    f.env.QUALIFICATION_TRANSPORT_PROFILE = "";
    assert.equal(f.invoke("admit").status, 0);
    const result = f.invoke("deploy");
    assert.equal(result.status, 0, result.stderr);
    assert.ok(f.state().identity);
    assert.ok(!f.state().events.some((event) => /transport-open|transport-settle/.test(event)));
    f.update({ now: Date.parse(f.prepared().expiresAt) + 60_000 });
    const cleanup = f.invoke("reap");
    assert.equal(cleanup.status, 0, cleanup.stderr);
    assert.deepEqual(f.state().workers, {});
  });

  test("unknown workflow transport selector fails before provider effects", (t) => {
    const f = fixture(t, "first-walk");
    f.env.QUALIFICATION_TRANSPORT_PROFILE = "candidate-transport-v3";
    assert.equal(f.invoke("admit").status, 0);
    const result = f.invoke("deploy");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /unknown transport profile/);
    assert.deepEqual(providerEvents(f), []);
  });

  for (const [name, overrides] of [
    ["rerun", { GITHUB_RUN_ATTEMPT: "2" }],
    ["other run", { GITHUB_RUN_ID: "43" }],
    ["other repository", { GITHUB_REPOSITORY: "example/other" }],
    ["other workflow", { QUALIFICATION_WORKFLOW_SHA: "9".repeat(40) }],
    ["other subnet", { QUALIFICATION_SUBNET_ID: "subnet-11111111111111111" }],
    ["other Cloudflare account", { CLOUDFLARE_ACCOUNT_ID: "9".repeat(32) }],
    ["other authority", { QUALIFICATION_AUTHORITY_SHA: "9".repeat(40) }],
    ["other capsule", { QUALIFICATION_RETAINED_CAPSULE_SHA256: "9".repeat(64) }],
    [
      "wrong artifact digest",
      { QUALIFICATION_HANDOFF_ARTIFACT_DIGEST: `sha256:${"9".repeat(64)}` },
    ],
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
    { name: "other-handoff" },
    { expired: true },
    { workflow_run: { id: 43, head_sha: workflowSha } },
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
    assert.equal(
      f.state().events.some((event) => event.endsWith("/claim")),
      false,
    );
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
      assert.equal(
        fs.existsSync(path.join(f.directory, "arm-proof", "execution-manifest.json")),
        false,
      );
      f.update({ now: Date.parse(handoff.expiresAt) + 60_000, armDelay: 0 });
      const cleanup = f.invoke("reap");
      assert.equal(cleanup.status, 0, cleanup.stderr);
      assert.equal(f.state().finalized, true);
      assert.deepEqual(f.state().workers, {});
      assert.equal(f.state().identity, undefined);
    });
  }
}

if (!process.env.QUALIFICATION_HANDOFF_TEST_STATE) {
  for (const name of [
    "crabbox-image-qualification-42-1",
    "crabbox-image-qualification-relay-42-1",
    "crabbox-image-qualification-controller",
  ])
    test(`preexisting ${name} cannot be overwritten`, (t) => {
      const f = fixture(t, "first-walk");
      assert.equal(f.invoke("admit").status, 0);
      f.update({ workers: { [name]: { metadata: { bindings: [] } } } });
      const result = f.invoke("deploy");
      assert.equal(result.status, 1);
      assert.match(result.stderr, /name collision/);
      assert.deepEqual(
        f.state().events.filter((event) => /^(PUT|POST|DELETE) api.cloudflare/.test(event)),
        [],
      );
    });
}

if (!process.env.QUALIFICATION_HANDOFF_TEST_STATE) {
  test("lost tombstone response retains exact recovery identity without replaying the migration", (t) => {
    const f = fixture(t, "first-walk");
    assert.equal(f.invoke("admit").status, 0);
    const deployed = f.invoke("deploy");
    assert.equal(deployed.status, 0, deployed.stderr);
    f.update({ tombstoneLoss: true });
    const first = f.invoke("reap");
    assert.equal(first.status, 1);
    assert.match(first.stderr, /tombstone upload response lost/);
    const partial = f.state().workers["crabbox-image-qualification-42-1"].metadata;
    assert.equal(partial.main_module, "tombstone.mjs");
    assert.deepEqual(
      partial.bindings.filter((b) => b.type === "secret_text"),
      [
        { name: "CRABBOX_ADMIN_TOKEN", type: "secret_text" },
        { name: "CRABBOX_SHARED_TOKEN", type: "secret_text" },
      ],
    );
    assert.equal(partial.bindings[0].props.deploymentHash, f.outputs.deployment_hash);
    const recovered = f.invoke("reap");
    assert.equal(recovered.status, 0, recovered.stderr);
    assert.deepEqual(f.state().workers, {});
  });
}

if (!process.env.QUALIFICATION_HANDOFF_TEST_STATE) {
  test("normal cleanup refuses binding drift returned by tombstone upload", (t) => {
    const f = fixture(t, "first-walk");
    assert.equal(f.invoke("admit").status, 0);
    const deployed = f.invoke("deploy");
    assert.equal(deployed.status, 0, deployed.stderr);
    f.update({ tombstoneDrift: true });
    const first = f.invoke("reap");
    assert.equal(first.status, 1);
    assert.match(first.stderr, /unowned or changed Worker/);
    assert.ok(f.state().workers["crabbox-image-qualification-42-1"]);
    assert.ok(
      !f
        .state()
        .events.includes(
          "DELETE api.cloudflare.com/client/v4/accounts/00000000000000000000000000000000/workers/scripts/crabbox-image-qualification-42-1",
        ),
    );
  });
}
