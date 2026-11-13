// Exact built modules and raw relay/controller, local only. All Worker outbound
// denied. Controlled metadata IDs are fixtures, not live Cloudflare attestations.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { spawn, spawnSync } from "node:child_process";
import { Miniflare } from "../worker/node_modules/miniflare/dist/src/index.js";
import {
  proveCandidateTransport,
  settleCandidateTransport,
} from "./image-qualification-transport.mjs";
const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const authority = path.resolve(
  process.argv[2] ?? "worker/dist-aws-qualification-authority/aws-qualification-authority.js",
);
const candidate = path.resolve(process.argv[3] ?? "worker/dist/index.js");
const mode = process.argv[4];
const custody = process.argv[5];
const directory = mode
  ? path.join(custody, "store")
  : fs.mkdtempSync(path.join(os.tmpdir(), "qualification-transport-local-"));
fs.mkdirSync(directory, { recursive: true });
const version = "11111111-1111-1111-1111-111111111111";
const token = "a".repeat(64),
  challenge = "b".repeat(64),
  deploymentHash = "d".repeat(64);
const identity = {
  runId: "image-qualification-42-1",
  owner: "maintainer",
  candidateSha: "c".repeat(40),
  candidateWorker: "crabbox-image-qualification-42-1",
  deploymentHash,
  expiresAt: new Date(Date.now() + 3600000).toISOString(),
  firstWalk: true,
};
const request = mode
  ? JSON.parse(fs.readFileSync(path.join(custody, "request.json"), "utf8"))
  : {
      identity,
      challenge,
      authorityVersion: version,
      candidateVersion: version,
      expiresAt: new Date(Date.now() + 540000).toISOString(),
    };
const text = (value) => ({ type: "text", value });
const json = (value) => ({ type: "json", value });
const worker = (name, file, env, exports = {}) => ({
  config: {
    name,
    compatibilityDate: "2026-09-03",
    compatibilityFlags: ["nodejs_compat"],
    manifest: {
      mainModule: "worker.mjs",
      modules: { "worker.mjs": { type: "esm", contents: fs.readFileSync(file, "utf8") } },
    },
    env,
    exports,
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
let outbound = 0,
  mf;
async function start(props = request.identity) {
  const options = {
    host: "127.0.0.1",
    port: 0,
    cf: false,
    telemetry: { enabled: false },
    resourcePersistencePath: directory,
    workers: [
      worker("controller", `${root}/scripts/image-qualification-controller-worker.mjs`, {
        CONTROLLER_TOKEN: text(token),
        AUTHORITY: {
          type: "worker",
          worker: "authority",
          exportName: "AWSQualificationController",
          props: { deploymentHash },
        },
      }),
      worker("relay", `${root}/scripts/image-qualification-relay-worker.mjs`, {
        EXECUTOR_TOKEN: text(token),
        CANDIDATE_ADMIN_TOKEN: text(token),
        CANDIDATE_SHARED_TOKEN: text("e".repeat(64)),
        AWS_REGION: text("us-east-1"),
        QUALIFICATION_OWNER: text("maintainer"),
        QUALIFICATION_ORG: text("image-qualification"),
        QUALIFICATION_EXPIRES_AT: text(request.identity.expiresAt),
        CANDIDATE: { type: "worker", worker: "candidate" },
      }),
      worker("candidate", candidate, {
        CRABBOX_ADMIN_TOKEN: text(token),
        CRABBOX_DEFAULT_ORG: text("image-qualification"),
        CF_VERSION_METADATA: json({ id: version }),
        CRABBOX_AWS_QUALIFICATION_TRANSPORT: { type: "worker", worker: "authority", props },
      }),
      worker(
        "authority",
        authority,
        {
          CF_VERSION_METADATA: json({ id: version }),
          AWS_QUALIFICATION_REGISTRY: {
            type: "durable-object",
            worker: "authority",
            exportName: "AWSQualificationRegistry",
          },
          AWS_QUALIFICATION_RUNS: {
            type: "durable-object",
            worker: "authority",
            exportName: "AWSQualificationRun",
          },
        },
        {
          AWSQualificationRegistry: { type: "durable-object", storage: "sqlite" },
          AWSQualificationRun: { type: "durable-object", storage: "sqlite" },
        },
      ),
    ],
  };
  for (const w of options.workers)
    w.dev.outboundService.handler = () => {
      outbound++;
      throw Error("outbound forbidden");
    };
  mf = new Miniflare(options);
  await mf.ready;
}
async function controller(action, input = {}) {
  const response = await mf.dispatchFetch(`http://fixture.invalid/${action}`, {
    method: "POST",
    headers: { authorization: `Bearer ${token}` },
    body: JSON.stringify(input),
  });
  if (!response.ok) throw Error(`controller ${action} ${response.status}`);
  return await response.json();
}
async function invokeCandidate(value) {
  const relay = await mf.getWorker("relay");
  const response = await relay.fetch("https://fixture.invalid/v1/qualification/transport-probe", {
    method: "POST",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    body: JSON.stringify({ challenge: value }),
  });
  await response.body?.cancel();
}
let checks = 0;
async function crashRecovery(stage) {
  const dir = path.join(directory, stage);
  fs.mkdirSync(dir);
  fs.writeFileSync(path.join(dir, "request.json"), JSON.stringify(request), { mode: 0o600 });
  const args = [fileURLToPath(import.meta.url), authority, candidate, "--crash", dir, stage];
  const child = spawn(process.execPath, args, {
    detached: true,
    stdio: ["ignore", "pipe", "pipe", "ipc"],
    env: { PATH: process.env.PATH },
  });
  let diagnostics = "";
  child.stderr.on("data", (x) => {
    diagnostics += x;
  });
  child.stdout.resume();
  const exited = new Promise((resolve) =>
    child.once("exit", (code, signal) => resolve({ code, signal })),
  );
  try {
    await new Promise((resolve, reject) => {
      const timer = setTimeout(
        () => reject(Error("local crash boundary timeout " + diagnostics)),
        20000,
      );
      child.once("message", (value) => {
        clearTimeout(timer);
        value === stage ? resolve() : reject(Error("wrong crash boundary"));
      });
      child.once("exit", () => {
        clearTimeout(timer);
        reject(Error("premature child exit " + diagnostics));
      });
    });
  } finally {
    try {
      process.kill(-child.pid, "SIGKILL");
    } catch (error) {
      if (error.code !== "ESRCH") throw error;
    }
    await exited;
  }
  const result = spawnSync(
    process.execPath,
    [fileURLToPath(import.meta.url), authority, candidate, "--recover", dir],
    { encoding: "utf8", timeout: 20000, env: { PATH: process.env.PATH } },
  );
  assert.equal(result.error, undefined);
  assert.equal(result.status, 0, result.stderr);
  const receipt = JSON.parse(result.stdout.trim().split("\n").at(-1));
  assert.equal(receipt.state, stage === "verified" ? "verified" : "cancelled");
  assert.equal(receipt.outboundCalls, 0);
  checks++;
}
try {
  await start();
  if (mode === "--crash") {
    await controller("transport-open", request);
    if (["observed", "verified"].includes(process.argv[6]))
      await invokeCandidate(request.challenge);
    if (process.argv[6] === "verified")
      await controller("transport-settle", { challenge: request.challenge, verified: true });
    process.send(process.argv[6]);
    await new Promise(() => {});
  } else if (mode === "--recover") {
    const result = await settleCandidateTransport({ request, controller, save: async () => {} });
    assert.equal(outbound, 0);
    console.log(JSON.stringify({ state: result.state, outboundCalls: outbound }));
  } else {
    await proveCandidateTransport({
      request,
      controller,
      invokeCandidate,
      verifyFences: async () => {},
      save: async () => {},
    });
    checks++;
    assert.equal((await controller("transport-discover")).probe.state, "verified");
    checks++;
    await mf.dispose();
    mf = undefined;
    await start();
    await settleCandidateTransport({ request, controller, save: async () => {} });
    checks++;
    assert.equal((await controller("transport-discover")).probe.state, "verified");
    checks++;
    assert.equal(outbound, 0);
    checks++;
    await mf.dispose();
    mf = undefined;
    for (const stage of ["pending", "observed", "verified"]) await crashRecovery(stage);
    console.log(
      JSON.stringify({
        checks,
        failed: 0,
        cloud: false,
        authority,
        candidate,
        rawController: true,
        rawRelay: true,
        actualServiceBoundProps: true,
        persistentRegistryRestart: true,
        processGroupKillBoundaries: ["pending", "observed", "verified"],
        freshProcessRecovery: true,
        controlledVersionMetadata: true,
        outboundCalls: outbound,
        liveProviderProof: false,
      }),
    );
  }
} finally {
  await mf?.dispose();
  if (!mode) fs.rmSync(directory, { recursive: true, force: true });
}
