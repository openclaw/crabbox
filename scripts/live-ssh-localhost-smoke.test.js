import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const smokeScript = path.join(import.meta.dirname, "live-ssh-localhost-smoke.sh");
const diagnostic = "static SSH architecture historical=amd64 (offline; refreshed before execution)";

function runSmoke(t, options = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-ssh-smoke-test-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const bin = path.join(dir, "bin");
  const fixtureHome = path.join(dir, "home");
  const temporary = path.join(dir, "tmp");
  for (const directory of [bin, path.join(fixtureHome, ".ssh"), temporary]) {
    fs.mkdirSync(directory, { recursive: true });
  }
  const authorizedKeys = path.join(fixtureHome, ".ssh", "authorized_keys");
  const existingEntry = "fixture-existing-entry\n";
  fs.writeFileSync(authorizedKeys, existingEntry);
  fs.writeFileSync(path.join(dir, "options.json"), JSON.stringify(options));
  const script = path.join(dir, "smoke.sh");
  fs.copyFileSync(smokeScript, script);
  const python = spawnSync("python3", ["-c", "import sys; print(sys.executable)"], { encoding: "utf8" });
  assert.equal(python.status, 0, python.stderr);

  // Validators and tar fixtures use real Python. No SSH, service, or network command is real.
  const stub = path.join(bin, "stub.cjs");
  fs.writeFileSync(stub, `#!/usr/bin/env node
const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const { spawnSync } = require("node:child_process");
const dir = process.env.CRABBOX_FIXTURE_DIR;
const options = JSON.parse(fs.readFileSync(path.join(dir, "options.json"), "utf8"));
const args = process.argv.slice(2);
const tool = path.basename(process.argv[1]);
function daemon() {
  process.on("SIGINT", () => process.exit(0));
  process.on("SIGTERM", () => process.exit(0));
  fs.appendFileSync(path.join(dir, "pids"), process.pid + "\\n");
  setInterval(() => {}, 1000);
}
switch (tool) {
  case "ssh-keygen":
    fs.writeFileSync(args[args.indexOf("-f") + 1] + ".pub", "fixture-disposable-entry\\n");
    break;
  case "ssh-keyscan":
    break;
  case "curl":
    console.log("crabbox-ssh-tunnel-ok");
    break;
  case "python3":
    if (args[0] === "-m") daemon();
    else if (args[1].startsWith("import socket;")) console.log("12345");
    else process.exit(spawnSync(process.env.CRABBOX_FIXTURE_PYTHON, args, { stdio: "inherit" }).status ?? 99);
    break;
  case "crabbox": {
    const command = args[0];
    fs.appendFileSync(path.join(dir, "calls.jsonl"), JSON.stringify(args) + "\\n");
    fs.appendFileSync(path.join(dir, "cwds.jsonl"), JSON.stringify(process.cwd()) + "\\n");
    const repoRoot = path.join(dir, "repo-root");
    if (command === "warmup") fs.writeFileSync(repoRoot, process.cwd());
    if (command === "run" && fs.readFileSync(repoRoot, "utf8") !== process.cwd()) {
      throw new Error("lease RepoRoot differs from warmup fixture cwd");
    }
    if (command === options.failCommand) {
      process.stdout.write(options.stdout);
      process.stderr.write(options.stderr);
      process.exit(options.exitCode);
    }
    switch (command) {
      case "doctor": console.log("doctor ok"); break;
      case "warmup": fs.writeFileSync(path.join(dir, "slug"), args[args.indexOf("--slug") + 1]); break;
      case "status": break;
      case "run": {
        if (!args.includes("--script")) {
          console.log("crabbox-ssh-localhost-ok");
          break;
        }
        const source = args[args.indexOf("--script") + 1];
        const data = fs.readFileSync(source);
        const name = crypto.createHash("sha256").update(data).digest("hex").slice(0, 12) + "-" + path.basename(source).replace(/[^a-zA-Z0-9._-]/g, "");
        const remote = path.join(process.env.CRABBOX_STATIC_WORK_ROOT, "repo");
        const store = path.join(remote, ".crabbox/scripts");
        fs.mkdirSync(store, { recursive: true });
        const uploaded = path.join(store, name);
        fs.writeFileSync(uploaded, data);
        const workload = spawnSync("/bin/sh", [uploaded], { cwd: remote, encoding: "utf8" });
        process.stdout.write(workload.stdout);
        process.stderr.write(workload.stderr);
        if (!args.includes("--keep-on-failure")) {
          if (workload.status !== 0) throw new Error("first fixture workload failed: " + workload.stderr);
          break;
        }
        if (workload.status !== 23) throw new Error("fixture workload failed: " + workload.stderr);
        const selected = args[args.indexOf("--download-on-failure") + 1].split("=")[1];
        fs.copyFileSync(path.join(store, "prior.log"), selected);
        const project = process.cwd();
        const captures = path.join(project, ".crabbox/captures");
        fs.mkdirSync(captures, { recursive: true });
        const members = {
          ["crabbox-artifacts/remote/.crabbox/scripts/" + name]: options.wrongScript ? "wrong bytes" : data.toString(),
          "crabbox-artifacts/remote/test-results/failure.log": "current report\\n",
          "crabbox-artifacts/crabbox-run.json": JSON.stringify({ exitCode: options.wrongMetadata ? 0 : 23 }),
          "crabbox-artifacts/stdout.log": workload.stdout,
          "crabbox-artifacts/stderr.log": workload.stderr,
        };
        if (options.staleEntry) members["crabbox-artifacts/remote/.crabbox/scripts/prior.log"] = "stale";
        const pack = [
          "import io, json, sys, tarfile",
          "members = json.load(sys.stdin)",
          "with tarfile.open(sys.argv[1], 'w:gz') as archive:",
          "    for name, value in members.items():",
          "        data = value.encode()",
          "        header = tarfile.TarInfo(name)",
          "        header.size = len(data)",
          "        archive.addfile(header, io.BytesIO(data))",
        ].join("\\n");
        const tar = spawnSync(process.env.CRABBOX_FIXTURE_PYTHON, ["-c", pack, path.join(captures, "fixture.tar.gz")], { input: JSON.stringify(members), encoding: "utf8" });
        if (tar.status !== 0) throw new Error(tar.stderr);
        if (options.remoteArchive) fs.writeFileSync(path.join(remote, ".crabbox/leaked.tar.gz"), "leaked");
        if (options.missingRetainedUpload) {
          for (const entry of fs.readdirSync(store)) {
            if (entry.endsWith("-firstsuccessscript.sh")) fs.unlinkSync(path.join(store, entry));
          }
        }
        // A retained reused run does not attempt cleanup, so these fields are omitted.
        process.stderr.write(JSON.stringify({ exitCode: 23, leaseStopped: options.badRetention, leaseStopError: options.stopError }) + "\\n");
        process.exit(options.workloadExit ?? 23);
      }
      case "cp": {
        const [source, destination] = args.slice(-2).map(value => value.replace(/^SANDBOX:/, ""));
        fs.copyFileSync(source, destination);
        fs.chmodSync(destination, 0o644);
        break;
      }
      case "tunnel": daemon(); console.log("http://127.0.0.1:12345"); break;
      case "list": {
        process.stderr.write(options.listStderr ?? "");
        const slug = fs.readFileSync(path.join(dir, "slug"), "utf8");
        console.log(options.listOutput ?? JSON.stringify([{ labels: { slug } }]));
        break;
      }
      case "stop": break;
      default: throw new Error("unexpected crabbox command: " + command);
    }
    break;
  }
  default: throw new Error("forbidden real service command: " + tool);
}
`, { mode: 0o755 });
  fs.symlinkSync(process.execPath, path.join(bin, "node"));
  for (const tool of ["crabbox", "ssh-keygen", "ssh-keyscan", "python3", "curl", "ssh", "sshd", "sudo", "systemctl", "service"]) {
    fs.symlinkSync(stub, path.join(bin, tool));
  }

  const result = spawnSync("/bin/bash", [script], {
    cwd: dir,
    env: {
      PATH: `${bin}:/usr/bin:/bin`,
      HOME: fixtureHome,
      TMPDIR: temporary,
      CRABBOX_BIN: options.relativeBin ? "./bin/crabbox" : path.join(bin, "crabbox"),
      CRABBOX_SSH_LOCALHOST_USER: "fixture",
      CRABBOX_FIXTURE_DIR: dir,
      CRABBOX_FIXTURE_PYTHON: python.stdout.trim(),
    },
    encoding: "utf8",
    timeout: 15000,
    killSignal: "SIGKILL",
  });
  // Reap only fixture daemons if a failing test interrupted the shell's EXIT cleanup.
  if (result.error && fs.existsSync(path.join(dir, "pids"))) {
    for (const pid of fs.readFileSync(path.join(dir, "pids"), "utf8").trim().split("\n")) {
      try { process.kill(Number(pid), "SIGKILL"); } catch (error) {
        if (error.code !== "ESRCH") throw error;
      }
    }
  }
  assert.ifError(result.error);
  assert.equal(fs.readFileSync(authorizedKeys, "utf8"), existingEntry);
  assert.deepEqual(fs.readdirSync(temporary), [], "smoke must remove its work directory and captures");
  const calls = fs.readFileSync(path.join(dir, "calls.jsonl"), "utf8").trim().split("\n").map(JSON.parse);
  const cwds = fs.readFileSync(path.join(dir, "cwds.jsonl"), "utf8").trim().split("\n").map(JSON.parse);
  assert.equal(new Set(cwds).size, 1, "every CLI invocation must use the same fixture RepoRoot");
  assert.ok(cwds[0].startsWith(fs.realpathSync(temporary) + path.sep), "CLI cwd must be inside the owned temporary tree");
  return { ...result, calls };
}

test("SSH localhost smoke accepts list JSON with diagnostics kept on stderr", (t) => {
  const result = runSmoke(t, { listStderr: diagnostic + "\n" });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_ssh_localhost_smoke_passed .*cp=roundtrip tunnel=ready cleanup=complete/);
  assert.ok(result.stderr.includes(diagnostic + "\n"));
  assert.match(result.stdout, /failure_bundle=passed exit=23 .*stale=absent neighbors=absent .*explicit_download=passed retained=true remote_archives=0/);
  assert.doesNotMatch(result.stderr, /classification=environment_blocked/);
  assert.ok(!result.stdout.includes(diagnostic));
  const payload = JSON.parse(result.stdout.split("\n").find(line => line.startsWith("[")));
  const slug = result.calls.find(args => args[0] === "warmup")[4];
  assert.equal(payload[0].labels.slug, slug);
  assert.deepEqual(result.calls.map(args => args[0]), ["doctor", "warmup", "status", "run", "run", "run", "status", "cp", "cp", "tunnel", "list", "stop"]);
});

test("SSH localhost smoke resolves a relative binary before entering its fixture", (t) => {
  const result = runSmoke(t, { relativeBin: true });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /failure_bundle=passed/);
  assert.match(result.stdout, /live_ssh_localhost_smoke_passed .*cleanup=complete/);
});

for (const { name, listOutput, error } of [
  { name: "malformed JSON", listOutput: "not-json", error: /invalid JSON:/ },
  { name: "a missing slug", listOutput: "[]", error: /list JSON did not include slug/ },
]) {
  test(`SSH localhost smoke still rejects ${name}`, (t) => {
    const result = runSmoke(t, { listOutput, listStderr: diagnostic + "\n" });
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.match(result.stderr, /classification=validation_failed/);
    assert.match(result.stderr, error);
    assert.ok(result.stderr.includes(diagnostic));
    assert.doesNotMatch(result.stdout, /live_ssh_localhost_smoke_passed/);
    assert.equal(result.calls.at(-1)[0], "stop");
  });
}

for (const fixture of [
  { failCommand: "doctor", exitCode: 23, stdout: "doctor partial output\n", stderr: "connection refused\n", classification: "environment_blocked" },
  { failCommand: "warmup", exitCode: 37, stdout: "warmup partial output\n", stderr: "capacity exhausted\n", classification: "quota_blocked" },
  { failCommand: "list", exitCode: 42, stdout: "quota exhausted\n", stderr: "list failed\n", classification: "quota_blocked" },
]) {
  test(`SSH localhost smoke preserves ${fixture.failCommand} failure streams and exit status`, (t) => {
    const result = runSmoke(t, fixture);
    assert.equal(result.status, fixture.exitCode, result.stdout + result.stderr);
    assert.match(result.stderr, new RegExp(`classification=${fixture.classification} .*exit=${fixture.exitCode}`));
    assert.ok(result.stderr.includes(fixture.stdout));
    assert.ok(result.stderr.includes(fixture.stderr));
    assert.ok(!result.stdout.includes(fixture.stdout));
    assert.doesNotMatch(result.stderr, /classification=validation_failed/);
    assert.doesNotMatch(result.stdout, /live_ssh_localhost_smoke_passed/);
    assert.deepEqual(result.calls.map(args => args[0]), fixture.failCommand === "doctor"
      ? ["doctor"]
      : fixture.failCommand === "warmup"
        ? ["doctor", "warmup", "stop"]
        : ["doctor", "warmup", "status", "run", "run", "run", "status", "cp", "cp", "tunnel", "list", "stop"]);
  });
}

for (const options of [
  { staleEntry: true }, { wrongScript: true }, { wrongMetadata: true },
  { badRetention: true }, { badRetention: false }, { stopError: "fixture cleanup failed" },
  { missingRetainedUpload: true }, { remoteArchive: true }, { workloadExit: 0 }, { workloadExit: 29 },
]) {
  test(`SSH localhost failure bundle rejects ${JSON.stringify(options)} and cleans up`, (t) => {
    const result = runSmoke(t, options);
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.match(result.stderr, /classification=validation_failed/);
    assert.doesNotMatch(result.stderr, /classification=environment_blocked/);
    assert.doesNotMatch(result.stdout, /failure_bundle=passed|live_ssh_localhost_smoke_passed/);
    assert.equal(result.calls.at(-1)[0], "stop");
  });
}
