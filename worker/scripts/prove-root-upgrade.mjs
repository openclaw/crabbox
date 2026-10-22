// Run only with a new, owned loopback PostgreSQL fixture. See the proof guide.
import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
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
const database = new URL(config.database);
assert.equal(database.hostname, "127.0.0.1");
assert.equal(database.pathname, "/crabbox_root_upgrade_test");
assert.equal(Number(process.versions.node.split(".")[0]), 24);
const evidence = config.evidence;
const release = JSON.parse(await fs.readFile(path.join(root, "release/records/v0.79.1.json")));
assert.equal(release.tag, "v0.79.1");
const sha = (bytes) => createHash("sha256").update(bytes).digest("hex");
const save = (name, value) =>
  fs.writeFile(path.join(evidence, name + ".json"), JSON.stringify(value, null, 2) + "\n", {
    mode: 0o600,
  });
const source = (revision, file) =>
  execFileSync("git", ["show", revision + ":" + file], { cwd: root, maxBuffer: 16 * 1024 * 1024 });
const report = {
  syntheticAWS: true,
  release,
  candidate: config.candidate,
  node: process.version,
  builds: [],
  processes: [],
  requests: [],
  assertions: [],
};
let active;
const pool = new Pool({ connectionString: database.href });
const snapshot = async (name) => {
  const rows = (
    await pool.query(
      "select key,value,value_text,updated_at::text,value_text_updated_at::text from crabbox.coordinator_kv order by key",
    )
  ).rows;
  await save(name, rows);
  return rows;
};
const row = (rows, id) => {
  const value = rows.find((r) => r.key === "lease:" + id);
  assert.ok(value, "missing persisted lease " + id);
  return JSON.parse(value.value_text ?? JSON.stringify(value.value));
};
const fixture = await fs.readFile(path.join(worker, "test/fixtures/root-upgrade-host.mjs"), "utf8");
async function bundle(label, checkout, revision) {
  const outdir = path.join(evidence, label);
  await fs.mkdir(outdir, { recursive: true, mode: 0o700 });
  await fs.symlink(path.join(checkout, "worker/node_modules"), path.join(outdir, "node_modules"));
  const outfile = path.join(outdir, "host.mjs");
  const result = await build({
    stdin: {
      contents: fixture,
      resolveDir: path.join(checkout, "worker/test/fixtures"),
      sourcefile: "root-upgrade-host.mjs",
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
    if (absolute === path.join(checkout, "worker/test/fixtures/root-upgrade-host.mjs")) continue;
    const relative = path.relative(checkout, absolute);
    assert.ok(!relative.startsWith(".."), "unexpected source outside selected release");
    // Bound file reads to one source at a time while checking its committed object.
    // oxlint-disable-next-line eslint/no-await-in-loop
    const bytes = await fs.readFile(absolute);
    assert.equal(sha(bytes), sha(source(revision, relative)), "source differs: " + relative);
    inputs.push({ file: relative, sha256: sha(bytes) });
  }
  const lock = await fs.readFile(path.join(checkout, "worker/package-lock.json"));
  assert.equal(sha(lock), sha(source(revision, "worker/package-lock.json")));
  const identity = {
    label,
    revision,
    packageVersion: JSON.parse(await fs.readFile(path.join(checkout, "worker/package.json")))
      .version,
    lockSHA256: sha(lock),
    bundleSHA256: sha(await fs.readFile(outfile)),
    fixtureSHA256: sha(fixture),
    inputs,
  };
  report.builds.push(identity);
  await save(label + "-build", identity);
  return outfile;
}
async function start(file, phase) {
  const events = [],
    stdout = [],
    stderr = [];
  const child = spawn(process.execPath, [file, configPath, phase], {
    stdio: ["ignore", "pipe", "pipe", "ipc"],
    env: { PATH: process.env.PATH, HOME: evidence, NO_PROXY: "127.0.0.1" },
  });
  child.stdout.on("data", (b) => stdout.push(b));
  child.stderr.on("data", (b) => stderr.push(b));
  const exited = new Promise((resolve) =>
    child.on("exit", (code, signal) => resolve({ code, signal })),
  );
  const state = { child, exited, phase, events, stdout, stderr };
  active = state;
  const ready = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("coordinator start timeout")), 30_000);
    child.on("error", reject);
    child.on("exit", () => {
      clearTimeout(timer);
      reject(new Error("coordinator exited before ready"));
    });
    child.on("message", (event) => {
      events.push(event);
      if (event.type === "ready") {
        clearTimeout(timer);
        resolve(event);
      }
    });
  });
  state.origin = ready.origin;
  report.processes.push({ phase, ...ready });
  await save("processes", report.processes);
  return state;
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
      }, 20_000);
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
  assert.equal(result.code, 0, "coordinator did not shut down cleanly");
  assert.ok(state.events.some((e) => e.type === "stopped"));
  await assert.rejects(fetch(state.origin + "/v1/health"));
}
async function request(method, route, body, admin = true, expected = 200, token) {
  const response = await fetch(active.origin + route, {
    method,
    headers: {
      authorization: "Bearer " + (token ?? (admin ? config.adminToken : config.sharedToken)),
      "content-type": "application/json",
      "x-crabbox-owner":
        body?.leaseID === oldID || route.includes(oldID) ? "unknown" : "alice@example.com",
      "x-crabbox-org": "example-org",
    },
    ...(body ? { body: JSON.stringify(body) } : {}),
    signal: AbortSignal.timeout(20_000),
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
const oldID = "cbx_000000000001",
  ordinaryID = "cbx_000000000002",
  newID = "cbx_000000000003",
  conflictID = "cbx_000000000004";
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
try {
  let currentBundle, after;
  if (config.resumeReplacement) {
    const previousBytes = await fs.readFile(
      path.join(config.resumeReplacement, "PROOF-REPORT.json"),
    );
    const previous = JSON.parse(previousBytes);
    assert.equal(previous.candidate, config.candidate);
    assert.equal(previous.release.sourceCommit, release.sourceCommit);
    assert.equal(previous.status, "failed");
    assert.match(previous.error, /ownership does not match lease/);
    for (const phase of ["old", "upgrade", "restart", "replacement"]) {
      const receipt = JSON.parse(
        // Check each stopped predecessor before accepting its continuation.
        // oxlint-disable-next-line eslint/no-await-in-loop
        await fs.readFile(path.join(config.resumeReplacement, phase + "-process.json")),
      );
      assert.equal(receipt.code, 0);
      assert.ok(receipt.events.some((e) => e.type === "stopped"));
    }
    after = JSON.parse(
      await fs.readFile(path.join(config.resumeReplacement, "after-restart.json")),
    );
    assert.deepEqual(await snapshot("resume-storage"), after);
    report.previousProof = {
      directory: config.resumeReplacement,
      sha256: sha(previousBytes),
      completedPhases: ["old", "upgrade", "restart"],
    };
    report.assertions.push(...previous.assertions);
    currentBundle = await bundle("candidate", root, config.candidate);
  } else {
    const resumedIDs = new Set();
    if (config.resumeOld) {
      const previous = JSON.parse(
        await fs.readFile(path.join(config.resumeOld, "PROOF-REPORT.json")),
      );
      const receipt = JSON.parse(
        await fs.readFile(path.join(config.resumeOld, "old-process.json")),
      );
      assert.equal(previous.status, "failed");
      assert.equal(previous.release.sourceCommit, release.sourceCommit);
      assert.ok(previous.processes.every((p) => p.phase === "old"));
      assert.equal(receipt.code, 0);
      assert.ok(receipt.events.some((e) => e.type === "stopped"));
      const stopped = await fs.readFile(path.join(config.resumeOld, "stopped-storage.json"));
      assert.deepEqual(await snapshot("resume-storage"), JSON.parse(stopped));
      for (const id of previous.resumedOld?.completedLeaseIDs ?? []) resumedIDs.add(id);
      for (const r of previous.requests) {
        if (r.method === "POST" && r.route === "/v1/leases" && r.status === 201)
          resumedIDs.add(r.response.lease.id);
      }
      assert.ok([...resumedIDs].every((id) => [oldID, ordinaryID].includes(id)));
      report.resumedOld = {
        directory: config.resumeOld,
        snapshotSHA256: sha(stopped),
        completedLeaseIDs: [...resumedIDs],
      };
    } else if (
      (await pool.query("select to_regclass('crabbox.coordinator_kv') as relation")).rows[0]
        .relation !== null
    ) {
      assert.equal(
        (await pool.query("select count(*)::int as count from crabbox.coordinator_kv")).rows[0]
          .count,
        0,
        "refusing to reseed populated state",
      );
    }
    const oldBundle = await bundle("released", config.oldSource, release.sourceCommit);
    currentBundle = await bundle("candidate", root, config.candidate);
    await start(oldBundle, "old");
    await request("GET", "/v1/leases", undefined, false, 401, "invalid-fixture-token");
    if (!resumedIDs.has(oldID))
      await request(
        "POST",
        "/v1/leases",
        {
          ...leaseInput(oldID, "existing-retained"),
          lifetimePolicy: { mode: "enforce", maxAgeSeconds: 7200, retainSeconds: 3600 },
        },
        true,
        201,
      );
    if (!resumedIDs.has(ordinaryID))
      await request("POST", "/v1/leases", leaseInput(ordinaryID, "existing-ordinary"), true, 201);
    const oldRead = await request("GET", "/v1/leases/" + oldID);
    await request("GET", "/v1/admin/leases/" + oldID + "/lifetime-readback", undefined, true);
    await stop();
    const before = await snapshot("before-upgrade");
    assert.equal(row(before, oldID).awsOriginalRootDisk, undefined);
    assert.equal(row(before, ordinaryID).awsOriginalRootDisk, undefined);
    assert.ok(row(before, oldID).lifetimePolicy.readiness.identity);
    assert.equal(row(before, ordinaryID).lifetimePolicy, undefined);
    report.assertions.push(
      "v0.79.1 produced legacy rows via authenticated HTTP; old process joined before upgrade",
    );
    await start(currentBundle, "upgrade");
    const upgradedRead = await request("GET", "/v1/leases/" + oldID);
    assert.deepEqual(upgradedRead, oldRead);
    await request("GET", "/v1/leases/" + ordinaryID);
    await request("GET", "/v1/admin/leases/" + oldID + "/lifetime-readback", undefined, true);
    const missing = await request(
      "GET",
      "/v1/admin/leases/" + ordinaryID + "/lifetime-readback",
      undefined,
      true,
      409,
    );
    assert.match(missing.message, /historical evidence.*unavailable/);
    await request("POST", "/v1/leases", leaseInput(newID, "new-original-root"), true, 201);
    await request("GET", "/v1/admin/leases/" + newID + "/lifetime-readback", undefined, true);
    const conflict = await request(
      "POST",
      "/v1/leases",
      leaseInput(conflictID, "conflicting-root"),
      true,
      500,
    );
    assert.equal(conflict.error, "original AWS root disk changed during acquisition");
    await stop();
    after = await snapshot("after-upgrade");
    for (const id of [oldID, ordinaryID]) assert.deepEqual(row(after, id), row(before, id));
    const newLease = row(after, newID),
      conflicted = row(after, conflictID);
    assert.equal(newLease.awsOriginalRootDisk.volumeID, "vol-00000000000000003");
    assert.equal(newLease.lifetimePolicy, undefined);
    assert.equal(conflicted.awsOriginalRootDisk.volumeID, "vol-00000000000000004");
    assert.equal(conflicted.provisioningResourceMayExist, true);
    assert.equal(conflicted.cleanupCompletedAt, undefined);
    report.assertions.push(
      "all legacy lease fields unchanged; new acquisition persists original disk without lifetime enrollment; conflicting publication refused",
    );
    await start(currentBundle, "restart");
    assert.deepEqual(await request("GET", "/v1/leases/" + oldID), oldRead);
    await request("GET", "/v1/admin/leases/" + oldID + "/lifetime-readback", undefined, true);
    await request("GET", "/v1/admin/leases/" + newID + "/lifetime-readback", undefined, true);
    await stop();
    const restarted = await snapshot("after-restart");
    for (const id of [oldID, ordinaryID, newID, conflictID])
      assert.deepEqual(row(restarted, id), row(after, id));
  }
  await start(currentBundle, "replacement");
  const replaced = await request(
    "GET",
    "/v1/admin/leases/" + newID + "/lifetime-readback",
    undefined,
    true,
    409,
  );
  assert.match(replaced.message, /current AWS root disk does not match original identity/);
  await stop();
  const replacement = await snapshot("after-replacement-refused");
  for (const id of [oldID, ordinaryID, newID, conflictID])
    assert.deepEqual(row(replacement, id), row(after, id));
  report.assertions.push(
    "cold restart reads old and new bindings; replacement disk rejected without stored mutation",
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
    process.exitCode = 1;
  }
  await pool.end();
  await save("PROOF-REPORT", report);
  console.log(
    JSON.stringify({ status: report.status, error: report.error, assertions: report.assertions }),
  );
}
