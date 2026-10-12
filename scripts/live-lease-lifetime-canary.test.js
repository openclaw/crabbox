import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { copySmokeRepo, writeExecutable } from "./test-support/smoke-fixtures.mjs";

const script = path.join(import.meta.dirname, "live-lease-lifetime-canary.sh");

function fixture(t, scenario = "success") {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "cbx-lifetime-canary-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const { tempRoot, smokeScript } = copySmokeRepo(dir, script);
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);
  const calls = path.join(dir, "calls.jsonl");
  const state = path.join(dir, "state.json");
  const cb = path.join(bin, "crabbox");
  writeExecutable(path.join(bin, "sleep"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(cb, `#!${process.execPath}
const fs = require("node:fs");
const args = process.argv.slice(2);
fs.appendFileSync(process.env.CANARY_CALLS, JSON.stringify(args) + "\\n");
const file = process.env.CANARY_STATE;
const state = fs.existsSync(file) ? JSON.parse(fs.readFileSync(file, "utf8")) : {reads: 0, statusReads: 0};
const scenario = process.env.CANARY_SCENARIO;
const value = flag => args[args.indexOf(flag) + 1];
const save = () => fs.writeFileSync(file, JSON.stringify(state));
const output = data => { save(); console.log(JSON.stringify(data)); };
const fail = message => { save(); console.error(message); process.exit(1); };
const id = value("--id");
function policy() {
  const phase = state.reads >= 4 ? "complete" : state.reads >= 2 ? "retained" : "due";
  return {source: "admin-canary", mode: "enforce", status: phase,
    readiness: scenario === "readiness-failed" ? {status: "failed"} : scenario === "readiness-delayed" && state.statusReads < 2 ? undefined : {status: "ready"},
    deadlineAt: state.deadline, effectiveStopAt: state.extension || state.deadline,
    extension: state.extension ? {expiresAt: state.extension} : undefined,
    stoppedAt: state.reads >= 2 ? new Date().toISOString() : undefined,
    deleteAfterAt: state.reads >= 2 ? new Date(Date.now() + 300000).toISOString() : undefined,
    warnings: scenario === "no-warning" ? [] : [state.reads >= 2 ? "DELETION: retained root disk will be permanently deleted" : "Compute will be stopped"]};
}
if (args[0] === "whoami") output({owner: "alice@example.invalid"});
else if (args[0] === "warmup") {
  // Mirror the real CLI: warmup rejects unknown flags and reads the Region from the environment.
  if (args.includes("--region")) fail("flag provided but not defined: -region");
  if (process.env.CRABBOX_AWS_REGION !== "eu-west-1") fail("AWS Region was not pinned to eu-west-1");
  if (scenario === "control-create-rejected" && !args.includes("--lifetime-canary")) fail("injected create rejection before admission");
  if (args.includes("--lifetime-canary")) {
    state.canary = value("--lease-id");
    state.deadline = new Date(Date.now() + 360000).toISOString();
    if (scenario === "create-failure") fail("injected ambiguous create failure");
  } else state.control = value("--lease-id");
  output({created: true});
} else if (args[0] === "status") {
  if (scenario === "transient-1101" && id === state.canary && !state.blipped) {
    state.blipped = true;
    fail("coordinator GET /v1/leases/" + id + ": http 500: error code: 1101");
  }
  if (id === state.canary) { state.statusReads++; output({id, state: "active", lifetimePolicy: policy()}); }
  else output({id, state: "active", lifetimePolicy: scenario === "control-enrolled" ? policy() : undefined});
} else if (args[0] === "run") {
  state.runs = (state.runs ?? 0) + 1;
  if (scenario === "control-deadline" && state.runs === 1) fail("run admission run_x unavailable before command: context deadline exceeded");
  if (scenario === "control-touch-fails" && state.runs === 1) fail("warning: heartbeat failed: lease busy");
  output({ok: true});
}
else if (args[0] === "stop") {
  if (scenario === "cleanup-failure" && id === state.canary) fail("injected cleanup failure");
  if (scenario === "control-create-rejected") fail("coordinator POST /v1/leases/" + id + "/release: http 404: {\\"error\\":\\"not_found\\"}");
  output({stopped: true});
} else if (args[0] === "admin" && args[1] === "leases") {
  if (scenario === "not-admin") fail("HTTP 403 admin required");
  output([]);
} else if (args[0] === "admin" && args[1] === "extend-lifetime") {
  if (state.reads >= 2 && scenario !== "accept-after-stop") {
    fail(scenario === "unrelated-extension-failure" ? "HTTP 500 transport failed" : scenario === "generic-conflict" ? "HTTP 409 conflict" : "HTTP 400 invalid_lifetime_extension: a stopped lease or in-flight cleanup cannot be extended");
  }
  // Match CLI RFC3339 serialization followed by coordinator ISO serialization.
  state.extension = new Date(Math.floor(Date.parse(value("--expires-at")) / 1000) * 1000).toISOString();
  output({lease: {lifetimePolicy: policy()}});
} else if (args[0] === "admin" && args[1] === "lifetime-readback") {
  const canary = id === state.canary;
  if (canary) state.reads++;
  const instanceState = !canary || state.reads < 2 ? "running" : state.reads < 4 ? "stopped" : "terminated";
  output({leaseId: id, provider: "aws", region: "eu-west-1",
    instanceId: canary ? "i-canary" : "i-control",
    rootVolumeId: canary ? "vol-canary" : "vol-control", instanceState,
    rootVolumePresent: scenario === "disk-residue" || instanceState !== "terminated",
    observedAt: new Date().toISOString()});
} else fail("unexpected command: " + args.join(" "));
`);
  const env = {
    ...process.env,
    PATH: `${bin}${path.delimiter}${process.env.PATH}`,
    CRABBOX_BIN: cb,
    CRABBOX_COORDINATOR: "https://coordinator.example.invalid",
    CRABBOX_LIVE_CONFIRM: "lifetime-canary",
    CRABBOX_COORDINATOR_ADMIN_TOKEN: "fixture-admin-secret",
    CANARY_STATE: state,
    CANARY_CALLS: calls,
    CANARY_SCENARIO: scenario,
  };
  return {
    run: overrides => spawnSync("bash", [smokeScript], {cwd: tempRoot, env: {...env, ...overrides}, encoding: "utf8", timeout: 20000}),
    calls: () => fs.existsSync(calls) ? fs.readFileSync(calls, "utf8").trim().split("\n").map(JSON.parse) : [],
  };
}

test("lifetime canary refuses writes without exact confirmation", t => {
  const f = fixture(t);
  for (const confirmation of ["", "1", "lifetime"]) {
    const result = f.run({CRABBOX_LIVE_CONFIRM: confirmation});
    assert.equal(result.status, 2, result.stderr);
    assert.match(result.stderr, /CRABBOX_LIVE_CONFIRM=lifetime-canary/);
  }
  assert.deepEqual(f.calls(), []);
});

test("lifetime canary requires explicit coordinator before credentials or writes", t => {
  const f = fixture(t);
  const result = f.run({CRABBOX_COORDINATOR: ""});
  assert.equal(result.status, 2);
  assert.match(result.stderr, /CRABBOX_COORDINATOR/);
  assert.deepEqual(f.calls(), []);
});

test("lifetime canary proves extension, stop, disk retention, deletion and unenrolled control", t => {
  const f = fixture(t);
  const result = f.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /extension accepted/);
  assert.match(result.stdout, /retained canary=.*instance=i-canary volume=vol-canary delete=/);
  assert.match(result.stdout, /post-stop extension rejected/);
  assert.match(result.stdout, /control=.*instance=i-control volume=vol-control running=true unenrolled=true/);
  assert.match(result.stdout, /classification=live_lease_lifetime_canary_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /fixture-admin-secret/);
  const calls = f.calls();
  const creates = calls.filter(args => args[0] === "warmup");
  assert.equal(creates.length, 2);
  for (const args of creates) {
    for (const [flag, value] of [["--provider", "aws"], ["--target", "linux"], ["--class", "tiny"], ["--type", "t3.small"], ["--market", "on-demand"], ["--ttl", "45m"], ["--idle-timeout", "10m"]]) {
      assert.equal(args[args.indexOf(flag) + 1], value);
    }
    assert.ok(args.includes("--keep"));
    assert.match(args[args.indexOf("--lease-id") + 1], /^cbx_[a-f0-9]{12}$/);
  }
  assert.ok(!creates[0].includes("--lifetime-canary"));
  assert.equal(creates[1][creates[1].indexOf("--lifetime-canary") + 1], "6m");
  assert.equal(creates[1][creates[1].indexOf("--lifetime-retention") + 1], "5m");
  assert.equal(calls.filter(args => args[0] === "admin" && args[1] === "extend-lifetime").length, 2);
  assert.ok(calls.some(args => args[0] === "run" && args.includes("--keep")));
  const stops = calls.filter(args => args[0] === "stop");
  assert.deepEqual(stops.map(args => args[args.indexOf("--id") + 1]).sort(), creates.map(args => args[args.indexOf("--lease-id") + 1]).sort());
});

test("lifetime canary rejects non-admin before allocation", t => {
  const f = fixture(t, "not-admin");
  const result = f.run();
  assert.notEqual(result.status, 0);
  assert.ok(!f.calls().some(args => args[0] === "warmup"));
});

for (const [scenario, expected] of [
  ["create-failure", /injected ambiguous create failure/],
  ["no-warning", /owner-visible pre-stop warning missing/],
  ["readiness-failed", /canary permission\/readiness check failed/],
  ["accept-after-stop", /extension after stopping was accepted/],
  ["unrelated-extension-failure", /without a server-side lifecycle rejection/],
  ["generic-conflict", /without a server-side lifecycle rejection/],
  ["disk-residue", /root-volume-absence proof/],
  ["cleanup-failure", /CLEANUP FAILED canary=/],
]) {
  test(`lifetime canary fails closed and cleans both leases: ${scenario}`, t => {
    const f = fixture(t, scenario);
    const result = f.run();
    assert.notEqual(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout + result.stderr, expected);
    assert.doesNotMatch(result.stdout, /classification=live_lease_lifetime_canary_passed/);
    const calls = f.calls();
    const creates = calls.filter(args => args[0] === "warmup");
    const stops = calls.filter(args => args[0] === "stop");
    assert.equal(creates.length, 2);
    assert.deepEqual(stops.map(args => args[args.indexOf("--id") + 1]).sort(), creates.map(args => args[args.indexOf("--lease-id") + 1]).sort());
  });
}

test("lifetime canary treats a lease rejected before admission as nothing to clean up", t => {
  const f = fixture(t, "control-create-rejected");
  const result = f.run();
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /injected create rejection before admission/);
  assert.match(result.stdout, /cleanup control=cbx_[a-f0-9]{12} not found; nothing was allocated/);
  assert.doesNotMatch(result.stdout + result.stderr, /CLEANUP FAILED/);
  assert.equal(f.calls().filter(args => args[0] === "warmup").length, 1);
  assert.equal(f.calls().filter(args => args[0] === "stop").length, 1);
});

test("lifetime canary retries a transient coordinator Worker reset", t => {
  const f = fixture(t, "transient-1101");
  const result = f.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /transient coordinator error attempt=1: .*error code: 1101/);
  assert.match(result.stdout, /classification=live_lease_lifetime_canary_passed/);
});

test("lifetime canary retries a control touch that hits a coordinator deadline", t => {
  const f = fixture(t, "control-deadline");
  const result = f.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /transient coordinator error attempt=1: .*context deadline exceeded/);
  assert.match(result.stdout, /classification=live_lease_lifetime_canary_passed/);
});

test("lifetime canary treats a failed control touch as housekeeping, not proof", t => {
  const f = fixture(t, "control-touch-fails");
  const result = f.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /control touch failed; retrying on the next poll/);
  assert.match(result.stdout, /control=.*running=true unenrolled=true/);
  assert.match(result.stdout, /classification=live_lease_lifetime_canary_passed/);
});

test("lifetime canary waits for coordinator IAM readiness before extending", t => {
  const f = fixture(t, "readiness-delayed");
  const result = f.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /waiting for coordinator StopInstances DryRun readiness/);
  const calls = f.calls();
  const extension = calls.findIndex(args => args[0] === "admin" && args[1] === "extend-lifetime");
  assert.equal(calls.slice(0, extension).filter(args => args[0] === "status").length, 3);
});

test("lifetime canary stops an unexpectedly enrolled control before creating canary", t => {
  const f = fixture(t, "control-enrolled");
  const result = f.run();
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /control was unexpectedly enrolled/);
  assert.equal(f.calls().filter(args => args[0] === "warmup").length, 1);
  assert.equal(f.calls().filter(args => args[0] === "stop").length, 1);
});

test("lifetime canary has a hard command budget and bounded cleanup", () => {
  const text = fs.readFileSync(script, "utf8");
  assert.match(text, /deadline=\$\(\(started_at \+ 2100\)\)/);
  assert.match(text, /deadline=\$\(\(\$\(date \+%s\) \+ 660\)\)/);
  assert.match(text, /process\.wait\(timeout=remaining\)/);
  assert.match(text, /os\.killpg\(process\.pid, signal\.SIGKILL\)/);
  assert.equal((text.match(/run_cb 300 stop/g) ?? []).length, 1);
  assert.equal((text.match(/release_lease (canary|control) /g) ?? []).length, 2);
  assert.match(text, /trap cleanup EXIT/);
  assert.doesNotMatch(text, /aws (ec2|sts)|curl|broker\.adminToken|config_value/);
});
