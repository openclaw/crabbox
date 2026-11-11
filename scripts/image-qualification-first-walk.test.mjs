import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import relay from "./image-qualification-relay-worker.mjs";
import {
  firstWalkRecipeSha256,
  readFirstWalkRecipe,
  stageFirstWalkRecipe,
  verifyQualificationEvidence,
} from "./image-qualification-control.mjs";

const root = path.resolve(import.meta.dirname, "..");
const digest = (bytes) => crypto.createHash("sha256").update(bytes).digest("hex");
// Inert fixture only: the production recipe stays unchanged in protected main.
const recipe = fs.readFileSync(path.join(root, "scripts/fixtures/first-walk-reviewed-recipe.sh"));
assert.equal(digest(recipe), firstWalkRecipeSha256);
const runner = fs.readFileSync(path.join(root, "scripts/qualification-first-walk-guest.py"));

// Execute the emitted shell with a privilege-boundary model: systemd's output
// is unreadable to ordinary cleanup, while the modeled sudo cleanup can remove
// it. No actual sudo, systemd, device or ownership operation runs on this host.
async function checkGuestCleanup(script, directory) {
  const bin = path.join(directory, "fake-sudo");
  fs.mkdirSync(bin);
  const log = path.join(directory, "sudo-calls.jsonl");
  fs.writeFileSync(
    path.join(bin, "sudo"),
    `#!${process.execPath}
import fs from 'node:fs';
const args = process.argv.slice(2);
fs.appendFileSync(${JSON.stringify(log)}, JSON.stringify(args)+'\\n');
if (args[0] === 'systemd-run') {
  const output = args[args.indexOf('--output') + 1];
  fs.mkdirSync(output); fs.writeFileSync(output+'/private', 'fixture'); fs.chmodSync(output, 0);
} else if (args[0] === 'rm') {
  const base = args.at(-1); fs.chmodSync(base+'/evidence', 0o700); fs.rmSync(base, {recursive:true});
} else process.exit(99);
`,
    { mode: 0o700 },
  );
  const exit = await new Promise((resolve, reject) => {
    const child = spawn("bash", ["-s"], {
      env: { PATH: `${bin}:${process.env.PATH}` },
      stdio: ["pipe", "ignore", "inherit"],
    });
    child.on("error", reject);
    child.on("exit", resolve);
    child.stdin.end(script);
  });
  assert.equal(exit, 0);
  const calls = fs.readFileSync(log, "utf8").trim().split("\n").map(JSON.parse);
  assert.deepEqual(
    calls.map((c) => c[0]),
    ["systemd-run", "rm"],
  );
  assert.equal(fs.existsSync(calls[1].at(-1)), false);
}

for (const badResult of [false, true])
  test(`credentialless executor ${badResult ? "rejects failed first walk without replay" : "captures exactly once and dies for independent cleanup"}`, async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "first-walk-contract-"));
    const calls = [],
      commands = [];
    const result = {
      completed: !badResult,
      unmounted: true,
      volumeID: "vol-33333333",
      recipeSha256: digest(recipe),
      ownership: { exit: badResult ? 1 : 0, summary: "state=complete" },
      verification: { mismatched: 0 },
    };
    const server = http.createServer(async (req, res) => {
      let raw = "";
      for await (const chunk of req) raw += chunk;
      calls.push({ method: req.method, url: req.url, body: raw && JSON.parse(raw) });
      let data;
      if (/\/v1\/leases\//.test(req.url)) data = { lease: { workRoot: "/workspaces/crabbox" } };
      else if (req.url.endsWith("/use")) data = { claim: "fixture-claim" };
      else if (req.url.endsWith("/first-walk"))
        data = { phase: "attached", volumeID: result.volumeID };
      else data = { checkpoint: { state: "ready", image: { snapshotIDs: ["snap-22222222"] } } };
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify(data));
    });
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    try {
      const recipeDir = path.join(dir, "candidate/recipes/bootstrap/v1");
      fs.mkdirSync(recipeDir, { recursive: true });
      fs.writeFileSync(path.join(recipeDir, "linuxOwnershipRepair.sh"), recipe);
      fs.mkdirSync(path.join(dir, "bin"));
      const commandFile = path.join(dir, "commands");
      const binary = `#!${process.execPath}\nimport fs from 'node:fs';\nlet input=''; for await (const chunk of process.stdin) input+=chunk;\nfs.appendFileSync(${JSON.stringify(commandFile)},JSON.stringify({args:process.argv.slice(2),input})+'\\n');\nconsole.log(input.includes('systemd-run') ? 'CRABBOX_FIRST_WALK_RESULT='+${JSON.stringify(JSON.stringify(result))} : 'stock-ready');\n`;
      fs.writeFileSync(path.join(dir, "bin/crabbox"), binary, { mode: 0o700 });
      const proof = path.join(dir, "proof");
      const env = {
        PATH: process.env.PATH,
        HOME: dir,
        GITHUB_RUN_ID: "123",
        GITHUB_RUN_ATTEMPT: "1",
        QUALIFICATION_RELAY_URL: `http://127.0.0.1:${server.address().port}`,
        QUALIFICATION_EXECUTOR_TOKEN: "e".repeat(64),
        QUALIFICATION_PROOF_DIR: proof,
        QUALIFICATION_ARTIFACT_DIR: dir,
        QUALIFICATION_AWS_REGION: "us-east-1",
        QUALIFICATION_EXPIRES_AT: new Date(Date.now() + 120 * 60_000).toISOString(),
      };
      const outcome = await new Promise((resolve, reject) => {
        const child = spawn(
          process.execPath,
          [path.join(root, "scripts/image-qualification-first-walk.mjs")],
          { env, stdio: "ignore" },
        );
        child.on("error", reject);
        child.on("exit", (code, signal) => resolve({ code, signal }));
      });
      assert.deepEqual(
        outcome,
        badResult ? { code: 1, signal: null } : { code: null, signal: "SIGKILL" },
      );
      commands.push(...fs.readFileSync(commandFile, "utf8").trim().split("\n").map(JSON.parse));
      assert.equal(commands.filter((c) => c.args[0] === "warmup").length, 2);
      assert.equal(commands.filter((c) => c.args[0] === "stop").length, 1);
      const walkCommands = commands.filter((c) => c.input.includes("systemd-run"));
      assert.equal(walkCommands.length, 1);
      assert.ok(walkCommands[0].input.includes(digest(recipe)));
      assert.ok(
        commands.filter((c) => c.args[0] === "warmup").every((c) => c.args.includes("--lease-id")),
      );
      assert.equal(
        calls.filter((c) => c.method === "POST" && c.url === "/v1/checkpoints").length,
        1,
      );
      const prepare = calls.find((c) => c.url.endsWith("/first-walk"));
      assert.equal(prepare.body.claim, "fixture-claim");
      assert.match(prepare.body.readinessReceiptSha256, /^[a-f0-9]{64}$/);
      assert.ok(fs.existsSync(path.join(proof, "checksums.sha256")));
      if (!badResult) {
        await checkGuestCleanup(walkCommands[0].input, dir);
        const proofObject = JSON.parse(fs.readFileSync(path.join(proof, "first-walk.json")));
        const attestation = {
          finalized: true,
          firstWalk: {
            finalized: true,
            phase: "attached",
            volumeID: result.volumeID,
            request: prepare.body,
          },
          finalReceipt: {
            completedAt: new Date().toISOString(),
            failureCodes: [],
            finalCounts: { volumes: 0 },
          },
          operations: [
            "RunInstances",
            "RunInstances",
            "CreateImage",
            "CreateVolume",
            "AttachVolume",
          ].map((action) => ({ action, signerDispatches: [{ outcome: "accepted" }] })),
        };
        const completeProof = { ...proofObject, result };
        assert.doesNotThrow(() =>
          verifyQualificationEvidence(attestation, completeProof, undefined, true),
        );
        for (const altered of [
          { ...completeProof, result: { ...result, unmounted: false } },
          {
            ...completeProof,
            firstWalk: { ...proofObject.firstWalk, recipeSha256: "a".repeat(64) },
          },
          {
            ...completeProof,
            firstWalk: {
              ...proofObject.firstWalk,
              runnerSha256: digest(Buffer.concat([runner, Buffer.from("changed")])),
            },
          },
        ])
          assert.throws(() => verifyQualificationEvidence(attestation, altered, undefined, true));
        assert.throws(() =>
          verifyQualificationEvidence(
            { ...attestation, firstWalk: { ...attestation.firstWalk, volumeID: "vol-44444444" } },
            completeProof,
            undefined,
            true,
          ),
        );
      }
    } finally {
      server.closeAllConnections();
      await new Promise((resolve) => server.close(resolve));
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

test("first-walk relay admission is opt-in and has no shared-principal route", async () => {
  let dispatches = 0;
  const env = {
    EXECUTOR_TOKEN: "t".repeat(64),
    CANDIDATE_ADMIN_TOKEN: "a".repeat(64),
    QUALIFICATION_EXPIRES_AT: new Date(Date.now() + 60_000).toISOString(),
    CANDIDATE: {
      fetch: async () => {
        dispatches++;
        return Response.json({ ok: true });
      },
    },
  };
  const request = (prefix) =>
    new Request(`https://relay.invalid${prefix}/v1/checkpoints/chk_test/first-walk`, {
      method: "POST",
      headers: {
        authorization: `Bearer ${env.EXECUTOR_TOKEN}`,
        "content-type": "application/json",
      },
      body: JSON.stringify({ action: "prepare" }),
    });
  assert.equal((await relay.fetch(request(""), env)).status, 404);
  env.QUALIFICATION_MODE = "first-walk";
  assert.equal((await relay.fetch(request(""), env)).status, 200);
  assert.equal((await relay.fetch(request("/qualification/shared"), env)).status, 404);
  assert.equal(dispatches, 1);
});

test("first-walk staging rejects missing, substituted, oversized and linked candidate recipes", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "first-walk-recipe-"));
  const source = path.join(dir, "source");
  const artifact = path.join(dir, "artifact");
  const recipeDir = path.join(source, "recipes/bootstrap/v1");
  const file = path.join(recipeDir, "linuxOwnershipRepair.sh");
  try {
    fs.mkdirSync(recipeDir, { recursive: true });
    assert.throws(() => stageFirstWalkRecipe(source, artifact), /ENOENT/);
    fs.writeFileSync(file, "substituted recipe");
    assert.throws(() => stageFirstWalkRecipe(source, artifact), /protected reviewed digest/);
    fs.writeFileSync(file, Buffer.alloc(16 * 1024 + 1));
    assert.throws(() => stageFirstWalkRecipe(source, artifact), /bounded regular file/);
    fs.rmSync(file);
    fs.symlinkSync(path.join(root, "scripts/fixtures/first-walk-reviewed-recipe.sh"), file);
    assert.throws(() => stageFirstWalkRecipe(source, artifact), /must not contain links/);
    fs.rmSync(file);
    fs.rmdirSync(recipeDir);
    fs.symlinkSync(path.join(root, "scripts/fixtures"), recipeDir);
    assert.throws(() => stageFirstWalkRecipe(source, artifact), /must not contain links/);
    fs.unlinkSync(recipeDir);
    fs.mkdirSync(recipeDir);
    fs.writeFileSync(file, recipe);
    stageFirstWalkRecipe(source, artifact);
    assert.deepEqual(readFirstWalkRecipe(path.join(artifact, "candidate")), recipe);
    assert.throws(() => stageFirstWalkRecipe(source, artifact), /EEXIST/);
    fs.appendFileSync(
      path.join(artifact, "candidate/recipes/bootstrap/v1/linuxOwnershipRepair.sh"),
      "tamper",
    );
    assert.throws(
      () => readFirstWalkRecipe(path.join(artifact, "candidate")),
      /protected reviewed digest/,
    );
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
