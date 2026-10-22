// Explicit opt-in, private loopback fixture. See the diagnostic guide.
import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import { createHash, generateKeyPairSync, sign } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { build } from "esbuild";
import { Pool } from "pg";

const worker = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const root = path.dirname(worker);
const configPath = path.resolve(process.argv[2]);
assert.equal((await fs.stat(configPath)).mode & 0o077, 0);
const config = JSON.parse(await fs.readFile(configPath, "utf8"));
assert.equal(Number(process.versions.node.split(".")[0]), 24);
for (const [field, name] of [
  ["database", "crabbox_finish_upgrade_test"],
  ["freshDatabase", "crabbox_finish_fresh_test"],
]) {
  const url = new URL(config[field]);
  assert.equal(url.hostname, "127.0.0.1");
  assert.equal(url.pathname, "/" + name);
}
assert.ok(Number.isInteger(config.port) && config.port > 1024);
const evidence = config.evidence;
const sha = (b) => createHash("sha256").update(b).digest("hex");
const digest = (b) => "sha256:" + sha(b);
const save = (name, value) =>
  fs.writeFile(path.join(evidence, name + ".json"), JSON.stringify(value, null, 2) + "\n", {
    mode: 0o600,
  });
const source = (revision, file) =>
  execFileSync("git", ["show", revision + ":" + file], { cwd: root, maxBuffer: 16 * 1024 * 1024 });
const release = JSON.parse(await fs.readFile(path.join(root, "release/records/v0.79.1.json")));
assert.equal(release.tag, "v0.79.1");
const report = {
  release,
  candidate: config.candidate,
  node: process.version,
  syntheticOnly: true,
  builds: [],
  processes: [],
  requests: [],
  assertions: [],
  cli: [],
};
const pools = {
  upgrade: new Pool({ connectionString: config.database }),
  fresh: new Pool({ connectionString: config.freshDatabase }),
};
let active;
const fixture = await fs.readFile(
  path.join(worker, "test/fixtures/finish-upgrade-host.mjs"),
  "utf8",
);
async function bundle(label, checkout, revision) {
  const outdir = path.join(evidence, label);
  await fs.mkdir(outdir, { mode: 0o700 });
  await fs.symlink(path.join(checkout, "worker/node_modules"), path.join(outdir, "node_modules"));
  const outfile = path.join(outdir, "host.mjs");
  const result = await build({
    stdin: {
      contents: fixture,
      resolveDir: path.join(checkout, "worker/test/fixtures"),
      sourcefile: "finish-upgrade-host.mjs",
      loader: "js",
    },
    outfile,
    bundle: true,
    packages: "external",
    platform: "node",
    format: "esm",
    target: "node24",
    metafile: true,
  });
  const inputs = [];
  for (const input of Object.keys(result.metafile.inputs)) {
    const absolute = path.resolve(input);
    if (absolute === path.join(checkout, "worker/test/fixtures/finish-upgrade-host.mjs")) continue;
    const relative = path.relative(checkout, absolute);
    assert.ok(!relative.startsWith(".."));
    // Verify each production source against the recorded commit, not its label.
    // oxlint-disable-next-line eslint/no-await-in-loop
    const bytes = await fs.readFile(absolute);
    assert.equal(sha(bytes), sha(source(revision, relative)), relative);
    inputs.push({ file: relative, sha256: sha(bytes) });
  }
  const lock = await fs.readFile(path.join(checkout, "worker/package-lock.json"));
  assert.equal(sha(lock), sha(source(revision, "worker/package-lock.json")));
  const identity = {
    label,
    revision,
    fixtureSHA256: sha(fixture),
    runnerSHA256: sha(await fs.readFile(fileURLToPath(import.meta.url))),
    bundleSHA256: sha(await fs.readFile(outfile)),
    lockSHA256: sha(lock),
    inputs,
  };
  report.builds.push(identity);
  await save(label + "-build", identity);
  return outfile;
}
async function start(file, phase, fresh = false) {
  const privatePath = path.join(evidence, phase + "-private.json");
  await fs.writeFile(
    privatePath,
    JSON.stringify({ ...config, database: fresh ? config.freshDatabase : config.database }),
    { mode: 0o600, flag: "wx" },
  );
  const child = spawn(process.execPath, [file, privatePath], {
    stdio: ["ignore", "pipe", "pipe", "ipc"],
    env: { PATH: process.env.PATH, HOME: evidence, NO_PROXY: "127.0.0.1" },
  });
  const state = { child, phase, events: [], stdout: [], stderr: [] };
  active = state;
  child.stdout.on("data", (b) => state.stdout.push(b));
  child.stderr.on("data", (b) => state.stderr.push(b));
  state.exited = new Promise((resolve) =>
    child.once("exit", (code, signal) => resolve({ code, signal })),
  );
  const ready = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("coordinator start timeout")), 30000);
    child.once("error", reject);
    child.once("exit", () => {
      clearTimeout(timer);
      reject(new Error("coordinator exited before ready"));
    });
    child.on("message", (event) => {
      state.events.push(event);
      if (event.type === "ready") {
        clearTimeout(timer);
        resolve(event);
      }
    });
  });
  state.origin = ready.origin;
  assert.equal(state.origin, "http://127.0.0.1:" + config.port);
  report.processes.push({ phase, ...ready });
  await save("processes", report.processes);
}
async function stop() {
  if (!active) return;
  const state = active;
  if (state.child.exitCode === null && state.child.signalCode === null) state.child.kill("SIGTERM");
  let timer;
  const result = await Promise.race([
    state.exited,
    new Promise((_, reject) => {
      timer = setTimeout(() => {
        state.child.kill("SIGKILL");
        reject(new Error("graceful stop timeout"));
      }, 20000);
    }),
  ]).finally(() => clearTimeout(timer));
  await fs.writeFile(path.join(evidence, state.phase + ".stdout"), Buffer.concat(state.stdout), {
    mode: 0o600,
  });
  await fs.writeFile(path.join(evidence, state.phase + ".stderr"), Buffer.concat(state.stderr), {
    mode: 0o600,
  });
  await save(state.phase + "-process", { pid: state.child.pid, ...result, events: state.events });
  active = undefined;
  assert.equal(result.code, 0);
  assert.ok(state.events.some((e) => e.type === "stopped"));
  if (state.origin) await assert.rejects(fetch(state.origin + "/v1/health"));
}
async function snapshot(name, fresh = false) {
  const rows = (
    await pools[fresh ? "fresh" : "upgrade"].query(
      "select key,value,value_text,updated_at::text,value_text_updated_at::text from crabbox.coordinator_kv order by key",
    )
  ).rows;
  await save(name, rows);
  return rows;
}
const record = (rows, key) => {
  const row = rows.find((r) => r.key === key);
  assert.ok(row, "missing " + key);
  return JSON.parse(row.value_text ?? JSON.stringify(row.value));
};
async function request(method, route, body, expected = 200, raw = false) {
  const response = await fetch(active.origin + route, {
    method,
    headers: {
      authorization: "Bearer " + config.adminToken,
      "content-type": "application/json",
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    },
    ...(body ? { body: raw ? body : JSON.stringify(body) } : {}),
    signal: AbortSignal.timeout(20000),
  });
  const value = await response.json();
  report.requests.push({
    phase: active.phase,
    method,
    route,
    status: response.status,
    response: value,
  });
  await save("requests", report.requests);
  assert.equal(response.status, expected, JSON.stringify(value));
  return value;
}
const leaseInput = (id, slug) => ({
  leaseID: id,
  slug,
  provider: "aws",
  target: "linux",
  awsRegion: "eu-west-1",
  awsUseStockImage: true,
  capacity: { market: "on-demand" },
  serverType: "t3.small",
  sshPublicKey: "ssh-ed25519 test",
  workRoot: "/workspace/preserved",
  ttlSeconds: 7200,
  idleTimeoutSeconds: 1800,
  keep: true,
});
function payload(prefix, values) {
  return Buffer.concat([
    Buffer.from(prefix),
    ...values.flatMap((value) => {
      const bytes = Buffer.from(value),
        length = Buffer.alloc(4);
      length.writeUInt32BE(bytes.length);
      return [length, bytes];
    }),
  ]);
}
async function admit(id, label, leaseID) {
  const command = ["fixture-only", label];
  const { run } = await request(
    "PUT",
    "/v1/runs/" + id,
    { command, provider: "aws", target: "linux", ...(leaseID ? { leaseID } : {}) },
    201,
  );
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  const rawKey = Buffer.from(publicKey.export({ format: "jwk" }).x, "base64url");
  const log = "synthetic local fixture output\n",
    endedAt = new Date().toISOString();
  const receipt = {
    schema_version: 2,
    receipt_type: "terminal",
    started_at: run.startedAt,
    ended_at: endedAt,
    provider: run.provider,
    ...(leaseID ? { lease_id: leaseID, slug: run.slug } : {}),
    run_id: id,
    command: command.join(" "),
    command_sha256: digest(payload("crabbox-command-v1\0", command)),
    exit_code: 0,
    sync_ms: 0,
    command_ms: 0,
    duration_ms: Date.parse(endedAt) - Date.parse(run.startedAt),
    log_sha256: digest(Buffer.from(log)),
    retained_log_sha256: digest(Buffer.from(log)),
    log_truncated: false,
    public_key: rawKey.toString("base64"),
    signer: digest(rawKey),
    signature: "",
  };
  const fields = [
    "schema_version",
    "receipt_type",
    "started_at",
    "ended_at",
    "provider",
    "lease_id",
    "slug",
    "run_id",
    "command",
    "command_sha256",
    "exit_code",
    "sync_ms",
    "command_ms",
    "duration_ms",
    "log_sha256",
    "retained_log_sha256",
    "log_truncated",
    "public_key",
    "signer",
  ];
  receipt.signature = sign(
    null,
    payload(
      "crabbox-terminal-receipt-v2\0",
      fields.map((f) => String(receipt[f] ?? "")),
    ),
    privateKey,
  ).toString("base64");
  const body = Buffer.from(
    JSON.stringify(
      {
        exitCode: 0,
        syncMs: 0,
        commandMs: 0,
        log,
        logChunks: [log],
        logTruncated: false,
        results: null,
        receipt,
      },
      null,
      2,
    ) + "\n",
  );
  const file = path.join(evidence, label + ".pending.json");
  const envelope = Buffer.from(
    JSON.stringify({
      schema_version: 1,
      coordinator: active.origin,
      run_id: id,
      owner: run.owner,
      org: run.org,
      body: body.toString("base64"),
      body_sha256: digest(body),
    }),
  );
  await fs.writeFile(file, envelope, { mode: 0o600, flag: "wx" });
  await fs.writeFile(path.join(evidence, label + ".original-body"), body, {
    mode: 0o600,
    flag: "wx",
  });
  await save(label + "-original", {
    id,
    run,
    receipt,
    bodySHA256: digest(body),
    envelopeSHA256: sha(envelope),
  });
  return { id, run, receipt, body, envelope, file };
}
async function cli(label, original, recover = true, binary = config.cli) {
  const home = path.join(evidence, "home-" + label);
  await fs.mkdir(home, { mode: 0o700 });
  const args = [
    "receipt",
    original.id,
    ...(recover
      ? ["--recover", original.file, "--expected-finish-sha256", digest(original.body)]
      : []),
    "--expected-signer",
    original.receipt.signer,
  ];
  const startIndex = active.events.length;
  const result = await new Promise((resolve, reject) => {
    const proc = spawn(binary, args, {
      stdio: ["ignore", "pipe", "pipe"],
      env: {
        PATH: process.env.PATH,
        HOME: home,
        XDG_CONFIG_HOME: home,
        XDG_STATE_HOME: home,
        CRABBOX_COORDINATOR: active.origin,
        CRABBOX_COORDINATOR_TOKEN: config.sharedToken,
        CRABBOX_OWNER: "alice@example.com",
        CRABBOX_ORG: "example-org",
      },
    });
    let stdout = "",
      stderr = "";
    proc.stdout.on("data", (b) => (stdout += b));
    proc.stderr.on("data", (b) => (stderr += b));
    const timer = setTimeout(() => proc.kill("SIGKILL"), 30000);
    proc.once("error", reject);
    proc.once("close", (code, signal) => {
      clearTimeout(timer);
      resolve({ code, signal, stdout, stderr });
    });
  });
  const requests = active.events.slice(startIndex).filter((e) => e.type === "request");
  const receipt = {
    label,
    binarySHA256: sha(await fs.readFile(binary)),
    args,
    ...result,
    requests,
  };
  report.cli.push(receipt);
  await save(label + "-cli", receipt);
  assert.equal(result.code, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout), original.receipt);
  assert.deepEqual(await fs.readFile(original.file), original.envelope);
  if (recover) {
    const posts = requests.filter((r) => r.method === "POST");
    assert.equal(posts.length, 1);
    assert.equal(posts[0].status, 200);
    assert.equal(posts[0].bodySHA256, digest(original.body));
  }
}
const pendingLease = "cbx_000000000011",
  legacyLease = "cbx_000000000012";
const pendingID = "run_00000000000000000000000000000011",
  legacyID = "run_00000000000000000000000000000012",
  freshID = "run_00000000000000000000000000000013";
const relevant = (rows) =>
  rows.filter((r) =>
    [pendingLease, legacyLease, pendingID, legacyID].some((id) => r.key.includes(id)),
  );
const legacyRows = (rows) =>
  rows.filter((r) => [legacyLease, legacyID].some((id) => r.key.includes(id)));
try {
  // Only an initialized-but-empty predecessor may continue after a transport
  // setup failure. Never replay a create or reset a database with business rows.
  if (config.resumeEmpty) {
    const bytes = await fs.readFile(path.join(config.resumeEmpty, "PROOF-REPORT.json"));
    const prior = JSON.parse(bytes);
    assert.equal(prior.status, "failed");
    assert.equal(prior.candidate, config.candidate);
    assert.equal(prior.release.sourceCommit, release.sourceCommit);
    assert.deepEqual(prior.requests, []);
    assert.deepEqual(prior.cli, []);
    assert.deepEqual(
      prior.processes.map((p) => p.phase),
      ["old"],
    );
    const stopped = JSON.parse(
      await fs.readFile(path.join(config.resumeEmpty, "old-process.json")),
    );
    assert.equal(stopped.code, 0);
    assert.deepEqual(
      stopped.events.map((e) => e.type),
      ["ready", "stopped"],
    );
    const rows = (await pools.upgrade.query("select key from crabbox.coordinator_kv")).rows;
    assert.deepEqual(rows, []);
    report.continuation = {
      previousReportSHA256: sha(bytes),
      businessRows: 0,
      completedCreates: 0,
    };
  }
  // A rerun against a used database is refused, never reset or replayed.
  await Promise.all(
    Object.entries(pools).map(async ([name, pool]) => {
      const before = (
        await pool.query(
          "select to_regclass('crabbox.coordinator_kv') as relation, version() as version",
        )
      ).rows[0];
      await save(name + "-database-before", before);
      assert.equal(
        before.relation,
        name === "upgrade" && config.resumeEmpty ? "crabbox.coordinator_kv" : null,
      );
    }),
  );
  const oldBundle = await bundle("old", config.oldCheckout, release.sourceCommit);
  const currentBundle = await bundle("candidate", root, config.candidate);
  await start(oldBundle, "old");
  await request("POST", "/v1/leases", leaseInput(pendingLease, "existing-pending"), 201);
  await request("POST", "/v1/leases", leaseInput(legacyLease, "existing-settings"), 201);
  const legacy = await admit(legacyID, "legacy-completed", legacyLease);
  await request("POST", "/v1/runs/" + legacy.id + "/finish", legacy.body, 200, true);
  await cli("old-legacy-read", legacy, false, config.oldCLI);
  const pending = await admit(pendingID, "old-pending", pendingLease);
  await stop();
  const before = await snapshot("before-upgrade");
  const settings = record(before, "lease:" + legacyLease);
  assert.equal(settings.keep, true);
  assert.equal(settings.ttlSeconds, 7200);
  assert.equal(settings.idleTimeoutSeconds, 1800);
  assert.equal(settings.workRoot, "/workspace/preserved");
  assert.equal(record(before, "run:" + pendingID).state, "running");
  await save("settings-before", settings);
  report.assertions.push(
    "v0.79.1 created both leases, nondefault settings, completed signed run and pending run through real APIs",
  );
  await start(currentBundle, "upgrade");
  assert.deepEqual(relevant(await snapshot("candidate-before-recovery")), relevant(before));
  await cli("upgraded-legacy-read", legacy, false);
  await cli("upgraded-recovery", pending);
  await stop();
  const committed = await snapshot("after-upgrade");
  assert.deepEqual(legacyRows(committed), legacyRows(before));
  const terminal = record(committed, "run:" + pendingID);
  assert.equal(terminal.state, "succeeded");
  assert.equal(terminal.eventCount, 2);
  assert.deepEqual(terminal.terminalReceipt, pending.receipt);
  report.assertions.push(
    "candidate imported exact retained bytes for old admitted run; unrelated physical settings/data unchanged",
  );
  await start(currentBundle, "restart");
  await cli("restart-read", pending, false);
  await cli("restart-idempotent", pending);
  await stop();
  const restarted = await snapshot("after-restart");
  assert.deepEqual(relevant(restarted), relevant(committed));
  await save("settings-after", record(restarted, "lease:" + legacyLease));
  report.assertions.push(
    "separate restarted coordinator retained exact receipt; idempotent original POST did not change run/log/event rows or settings",
  );
  await start(currentBundle, "fresh", true);
  const fresh = await admit(freshID, "fresh-install");
  await cli("fresh-recovery", fresh);
  await stop();
  const freshRows = await snapshot("fresh-after", true);
  assert.deepEqual(record(freshRows, "run:" + freshID).terminalReceipt, fresh.receipt);
  assert.equal(record(freshRows, "run:" + freshID).state, "succeeded");
  report.assertions.push(
    "candidate native initializer on genuinely empty database, CLI recovery and physical terminal readback",
  );
  report.status = "passed";
} catch (error) {
  report.status = "failed";
  report.error = String(error.stack ?? error);
  process.exitCode = 1;
} finally {
  try {
    await stop();
  } catch (error) {
    report.cleanupError = String(error);
    report.status = "failed";
    process.exitCode = 1;
  }
  await Promise.all(Object.values(pools).map((pool) => pool.end()));
  await save("PROOF-REPORT", report);
}
