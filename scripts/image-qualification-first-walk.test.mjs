import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { spawn, spawnSync } from "node:child_process";
import test from "node:test";
import relay from "./image-qualification-relay-worker.mjs";
import {
  firstWalkRecipeSha256,
  readFirstWalkRecipe,
  stageFirstWalkRecipe,
  verifyQualificationEvidence,
  verifyFirstWalkPreparation,
} from "./image-qualification-control.mjs";

const root = path.resolve(import.meta.dirname, "..");
const digest = (bytes) => crypto.createHash("sha256").update(bytes).digest("hex");
// Inert fixture only: the production recipe stays unchanged in protected main.
const recipe = fs.readFileSync(path.join(root, "scripts/fixtures/first-walk-reviewed-recipe.sh"));
assert.equal(digest(recipe), firstWalkRecipeSha256);
const runner = fs.readFileSync(path.join(root, "scripts/qualification-first-walk-guest.py"));

const prepared = {
  completed: true,
  profile: "fixed-node-monorepo-v1",
  sourceCommit: "8436c62514241a0afdef84bc2abb658dbdf57d40",
  sourceArchiveSha256: "ebaaa5f50f6b3ba54f825eaed7c602838e0a5eea936aee9b5212085591084dda",
  lockSha256: "0c50a34a1f4f78e7fa5da6340878f54da7f6b9f36a0c04c3f67c61ab8fbb7d06",
  nodeSha256: "6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6",
  pnpmSha256: "bcbd17511de1718a262be1acc42066559bbce8fcfb1adb584d1f5e8ad70fe218",
  scriptsDisabled: true,
  hooksDisabled: true,
  sameFilesystem: true,
  cachePopulated: true,
  inventory: { entries: 200_000, allocatedBytes: 6 * 1024 ** 3 },
  elapsedSeconds: 60,
};

test("preparation proof requires the fixed identities, both roots, no hooks and bounded work", () => {
  assert.doesNotThrow(() => verifyFirstWalkPreparation(prepared));
  for (const field of Object.keys(prepared)) {
    const changed = { ...prepared };
    delete changed[field];
    if (field === "meaning") continue;
    assert.throws(() => verifyFirstWalkPreparation(changed));
  }
  for (const changed of [
    { ...prepared, sourceCommit: "a".repeat(40) },
    { ...prepared, cachePopulated: false },
    { ...prepared, hooksDisabled: false },
    { ...prepared, sameFilesystem: false },
    { ...prepared, elapsedSeconds: 901 },
    ...[0, 600001, NaN].map((entries) => ({
      ...prepared,
      inventory: { ...prepared.inventory, entries },
    })),
    ...[-1, 12 * 1024 ** 3 + 1, Infinity].map((allocatedBytes) => ({
      ...prepared,
      inventory: { ...prepared.inventory, allocatedBytes },
    })),
  ])
    assert.throws(() => verifyFirstWalkPreparation(changed));
});

test("first-walk refuses a queue-consumed executor reserve before any allocation", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "first-walk-reserve-"));
  try {
    const target = path.join(directory, "candidate/recipes/bootstrap/v1/linuxOwnershipRepair.sh");
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, recipe);
    let stderr = "";
    const code = await new Promise((resolve, reject) => {
      const child = spawn(
        process.execPath,
        [path.join(root, "scripts/image-qualification-first-walk.mjs")],
        {
          env: {
            PATH: process.env.PATH,
            QUALIFICATION_RELAY_URL: "https://must-not-contact.invalid",
            QUALIFICATION_EXECUTOR_TOKEN: "fixture-token",
            QUALIFICATION_PROOF_DIR: path.join(directory, "proof"),
            QUALIFICATION_ARTIFACT_DIR: directory,
            QUALIFICATION_AWS_REGION: "us-east-1",
            // Positive remaining work time, but less than the full executor reserve.
            QUALIFICATION_EXPIRES_AT: new Date(Date.now() + 119 * 60_000).toISOString(),
          },
          stdio: ["ignore", "ignore", "pipe"],
          timeout: 5000,
        },
      );
      child.stderr.on("data", (part) => (stderr += part));
      child.on("error", reject);
      child.on("close", resolve);
    });
    assert.equal(code, 1);
    assert.match(stderr, /full 100-minute executor reserve before allocation/);
    assert.equal(fs.existsSync(path.join(directory, "proof")), false);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

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

for (const { label, badResult = false, badPreparation = false, evidenceFault } of [
  { label: "captures exactly once and dies for independent cleanup" },
  { label: "rejects failed first walk without replay", badResult: true },
  { label: "rejects incomplete preparation before capture", badPreparation: true },
  { label: "rejects a log replaced by a key symlink", evidenceFault: "symlink" },
  { label: "rejects a log replaced by a directory", evidenceFault: "directory" },
  { label: "rejects a log replaced by a FIFO without blocking", evidenceFault: "fifo" },
  { label: "removes private state after a missing log", evidenceFault: "missing" },
  { label: "rejects a log replaced by another regular file", evidenceFault: "replacement" },
  { label: "removes private state after an evidence write failure", evidenceFault: "write" },
])
  test(`credentialless executor ${label}`, async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "first-walk-contract-"));
    const commandFile = path.join(dir, "commands");
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
      const privateMarker = "INERT_PRIVATE_KEY_DO_NOT_EXPORT";
      const unexpectedMarker = "UNREGISTERED_FILE_DO_NOT_EXPORT";
      const binary = `#!${process.execPath}
import fs from 'node:fs';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
let input=''; for await (const chunk of process.stdin) input+=chunk;
fs.appendFileSync(${JSON.stringify(commandFile)},JSON.stringify({args:process.argv.slice(2),input,env:process.env,cwd:process.cwd()})+'\\n');
const keyDir=path.join(process.env.XDG_CONFIG_HOME,'crabbox/testboxes/cbx_fixture');
fs.mkdirSync(keyDir,{recursive:true});
const key=path.join(keyDir,'id_ed25519');
fs.writeFileSync(key,${JSON.stringify(privateMarker)},{mode:0o600});
fs.writeFileSync('unexpected.log',${JSON.stringify(unexpectedMarker)});
if(input.includes('--volume')) {
  const fault=${JSON.stringify(evidenceFault ?? "")};
  const name='source-stock-ready.log';
  if(fault==='write') fs.mkdirSync(path.join(${JSON.stringify(path.join(dir, "proof"))},name));
  else if(fault) {
    // Allocate replacement before removing the original so inode reuse cannot
    // turn the regular-file substitution into a flaky identity test.
    if(fault==='replacement') fs.writeFileSync('replacement',${JSON.stringify(privateMarker)});
    fs.unlinkSync(name);
    if(fault==='symlink') fs.symlinkSync(key,name);
    if(fault==='directory') fs.mkdirSync(name);
    if(fault==='fifo' && spawnSync('mkfifo',[name]).status!==0) throw new Error('mkfifo failed');
    if(fault==='replacement') fs.renameSync('replacement',name);
  }
}
console.log(input.includes('--work-root') ? 'CRABBOX_WORKLOAD_PREPARED='+${JSON.stringify(JSON.stringify({ ...prepared, cachePopulated: !badPreparation }))} : input.includes('--volume') ? 'CRABBOX_FIRST_WALK_RESULT='+${JSON.stringify(JSON.stringify(result))} : 'stock-ready');
`;
      // Artifact downloads restore ordinary files without executable permissions.
      fs.writeFileSync(path.join(dir, "bin/crabbox"), binary, { mode: 0o644 });
      fs.chmodSync(path.join(dir, "bin/crabbox"), 0o644);
      const proof = path.join(dir, "proof");
      const env = {
        PATH: process.env.PATH,
        HOME: dir,
        AWS_PROFILE: "production",
        AWS_WEB_IDENTITY_TOKEN_FILE: "/synthetic/production-token",
        CRABBOX_BROKER_URL: "https://production.invalid",
        GH_TOKEN: "synthetic-inherited-token",
        GITHUB_RUN_ID: "123",
        GITHUB_RUN_ATTEMPT: "1",
        QUALIFICATION_RELAY_URL: `http://127.0.0.1:${server.address().port}`,
        QUALIFICATION_EXECUTOR_TOKEN: "e".repeat(64),
        QUALIFICATION_PROOF_DIR: proof,
        QUALIFICATION_ARTIFACT_DIR: dir,
        QUALIFICATION_AWS_REGION: "us-east-1",
        QUALIFICATION_EXPIRES_AT: new Date(Date.now() + 150 * 60_000).toISOString(),
      };
      let stderr = "";
      const outcome = await new Promise((resolve, reject) => {
        const child = spawn(
          process.execPath,
          [path.join(root, "scripts/image-qualification-first-walk.mjs")],
          { env, stdio: ["ignore", "ignore", "pipe"], timeout: 15_000, killSignal: "SIGKILL" },
        );
        child.stderr.on("data", (chunk) => (stderr += chunk));
        child.on("error", reject);
        child.on("close", (code, signal) => resolve({ code, signal }));
      });
      assert.deepEqual(
        outcome,
        badResult || badPreparation || evidenceFault
          ? { code: 1, signal: null }
          : { code: null, signal: "SIGKILL" },
      );
      if (["directory", "fifo", "replacement"].includes(evidenceFault))
        assert.match(
          stderr,
          /first-walk log was replaced; let the independent finalizer settle the run and investigate the changed log before any separately authorized retry/,
        );
      assert.equal(stderr.includes(privateMarker), false);
      assert.equal(stderr.includes(env.QUALIFICATION_EXECUTOR_TOKEN), false);
      assert.equal(fs.statSync(path.join(dir, "bin/crabbox")).mode & 0o777, 0o700);
      commands.push(...fs.readFileSync(commandFile, "utf8").trim().split("\n").map(JSON.parse));
      for (const command of commands) {
        for (const key of [
          "AWS_PROFILE",
          "AWS_WEB_IDENTITY_TOKEN_FILE",
          "CRABBOX_BROKER_URL",
          "GH_TOKEN",
        ])
          assert.equal(command.env[key], undefined);
        assert.equal(command.env.CRABBOX_COORDINATOR, env.QUALIFICATION_RELAY_URL);
        assert.notEqual(command.env.HOME, dir);
        assert.equal(command.cwd, command.env.HOME);
        assert.equal(command.env.XDG_CONFIG_HOME, path.join(command.cwd, "config"));
        assert.equal(fs.existsSync(command.cwd), false, "private scratch must be removed");
      }
      if (badPreparation) {
        assert.equal(commands.filter((c) => c.args[0] === "warmup").length, 1);
        assert.equal(
          calls.some((c) => c.method === "POST"),
          false,
          "preparation failure cannot capture or attach",
        );
        return;
      }
      const population = commands.filter((c) => c.input.includes("--work-root"));
      assert.equal(population.length, 1);
      assert.match(population[0].input, /--uid=crabbox --gid=crabbox/);
      assert.match(population[0].input, /RuntimeMaxSec=900/);
      assert.match(population[0].input, /KillMode=control-group/);
      assert.equal(commands.filter((c) => c.args[0] === "warmup").length, 2);
      assert.equal(commands.filter((c) => c.args[0] === "stop").length, 1);
      const walkCommands = commands.filter((c) => c.input.includes("--volume"));
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
      assert.equal(fs.existsSync(path.join(proof, "checksums.sha256")), !evidenceFault);
      assert.equal(fs.existsSync(path.join(proof, "config")), false);
      assert.equal(fs.existsSync(path.join(proof, "unexpected.log")), false);
      for (const name of fs.readdirSync(proof)) {
        const file = path.join(proof, name);
        if (fs.lstatSync(file).isFile()) {
          const text = fs.readFileSync(file, "utf8");
          assert.equal(text.includes(privateMarker), false);
          assert.equal(text.includes(unexpectedMarker), false);
          assert.equal(text.includes(env.QUALIFICATION_EXECUTOR_TOKEN), false);
        }
      }
      if (!badResult && !evidenceFault) {
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
            firstWalk: { ...proofObject.firstWalk, workloadSha256: "a".repeat(64) },
          },
          {
            ...completeProof,
            firstWalk: {
              ...proofObject.firstWalk,
              sourcePreparation: { ...prepared, cachePopulated: false },
            },
          },
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
      // Preserve failing assertions without leaking the deliberately broken
      // executor's private fixtures when this regression runs against old code.
      if (fs.existsSync(commandFile)) {
        const rows = fs.readFileSync(commandFile, "utf8").trim().split("\n").map(JSON.parse);
        for (const cwd of new Set(rows.map((row) => row.cwd))) {
          assert.match(path.basename(cwd), /^first-walk-executor-/);
          fs.rmSync(cwd, { recursive: true, force: true });
        }
      }
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
  const recipeDir = path.join(source, "qualification/first-walk/v1");
  const file = path.join(recipeDir, "linuxOwnershipRepair.sh");
  try {
    fs.mkdirSync(recipeDir, { recursive: true });
    assert.throws(() => stageFirstWalkRecipe(source, artifact), /ENOENT/);
    fs.writeFileSync(file, "substituted recipe");
    assert.throws(() => stageFirstWalkRecipe(source, artifact), /protected reviewed digest/);
    fs.writeFileSync(file, Buffer.alloc(16 * 1024 + 1));
    assert.throws(() => stageFirstWalkRecipe(source, artifact), /package file is invalid/);
    fs.rmSync(file);
    fs.symlinkSync(path.join(root, "scripts/fixtures/first-walk-reviewed-recipe.sh"), file);
    assert.throws(() => stageFirstWalkRecipe(source, artifact), /link or non-directory/);
    fs.rmSync(file);
    fs.rmdirSync(recipeDir);
    fs.symlinkSync(path.join(root, "scripts/fixtures"), recipeDir);
    assert.throws(() => stageFirstWalkRecipe(source, artifact), /link or non-directory/);
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

test("first-walk shell reserves cleanup and caps execution despite early admission", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "first-walk-shell-"));
  try {
    fs.writeFileSync(
      path.join(directory, "timeout"),
      `#!${process.execPath}\nconsole.log(JSON.stringify(process.argv.slice(2)));\n`,
      { mode: 0o700 },
    );
    for (const minutes of [119, 121, 149, 152]) {
      const child = spawnSync("bash", [path.join(root, "scripts/image-qualification-execute.sh")], {
        env: {
          PATH: `${directory}:${process.env.PATH}`,
          QUALIFICATION_MODE: "first-walk",
          QUALIFICATION_RELAY_URL: "https://must-not-contact.invalid",
          QUALIFICATION_EXECUTOR_TOKEN: "fixture-token",
          QUALIFICATION_BASE_AMI_ID: "ami-00000000",
          QUALIFICATION_AWS_REGION: "us-east-1",
          QUALIFICATION_ARTIFACT_DIR: directory,
          QUALIFICATION_PROOF_DIR: directory,
          QUALIFICATION_EXPIRES_AT: new Date(Date.now() + minutes * 60_000).toISOString(),
        },
        encoding: "utf8",
        timeout: 5000,
      });
      if (minutes === 119 || minutes === 152) {
        assert.notEqual(child.status, 0);
        assert.equal(child.stdout, "");
      } else {
        assert.equal(child.status, 0, child.stderr);
        const args = JSON.parse(child.stdout);
        assert.deepEqual(args.slice(0, 3), ["--signal=TERM", "--kill-after=1200", "6000"]);
        assert.match(args.at(-1), /image-qualification-first-walk\.mjs$/);
      }
    }
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});
