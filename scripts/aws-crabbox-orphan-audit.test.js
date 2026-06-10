import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const root = path.resolve(import.meta.dirname, "..");

function writeExecutable(file, body) {
  fs.writeFileSync(file, body, "utf8");
  fs.chmodSync(file, 0o755);
}

function setupFakes({ active = false, invalidLeases = false } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-aws-orphan-audit-"));
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);
  const calls = path.join(dir, "calls.jsonl");

  writeExecutable(
    path.join(bin, "aws"),
    `#!/usr/bin/env node
const fs = require("node:fs");
const args = process.argv.slice(2);
fs.appendFileSync(process.env.CRABBOX_FAKE_CALLS, JSON.stringify(["aws", ...args]) + "\\n");

if (args[0] === "sts" && args[1] === "get-caller-identity") {
  process.stdout.write(JSON.stringify({ Account: "123456789012" }) + "\\n");
  process.exit(0);
}

if (args[0] === "ec2" && args[1] === "describe-instances") {
  process.stdout.write(JSON.stringify({
    Reservations: [{
      Instances: [{
        InstanceId: "i-orphan",
        State: { Name: "running" },
        InstanceType: "t3.small",
        LaunchTime: "2026-01-01T00:00:00Z",
        PublicIpAddress: "203.0.113.10",
        Tags: [
          { Key: "crabbox", Value: "true" },
          { Key: "lease", Value: "cbx-live" },
          { Key: "created_at", Value: "1" },
          { Key: "expires_at", Value: "1" }
        ]
      }]
    }]
  }) + "\\n");
  process.exit(0);
}

if (args[0] === "ec2" && args[1] === "terminate-instances") {
  process.stderr.write("terminate must not be called by aws-crabbox-orphan-audit\\n");
  process.exit(70);
}

process.stderr.write("unexpected aws args: " + JSON.stringify(args) + "\\n");
process.exit(64);
`,
  );

  const fakeCrabbox = path.join(dir, "crabbox");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env node
const fs = require("node:fs");
fs.appendFileSync(process.env.CRABBOX_FAKE_CALLS, JSON.stringify(["crabbox", ...process.argv.slice(2)]) + "\\n");

if (process.argv.slice(2).join(" ") !== "admin leases --json -state active -limit 1000") {
  process.stderr.write("unexpected crabbox args: " + JSON.stringify(process.argv.slice(2)) + "\\n");
  process.exit(64);
}

if (process.env.CRABBOX_FAKE_INVALID_LEASES === "1") {
  process.stdout.write(JSON.stringify([{ id: "cbx-live", cloudID: "i-orphan" }, "bad-record"]) + "\\n");
} else if (process.env.CRABBOX_FAKE_ACTIVE === "1") {
  process.stdout.write(JSON.stringify([{ id: "cbx-live", cloudID: "i-orphan" }]) + "\\n");
} else {
  process.stdout.write("[]\\n");
}
`,
  );

  return {
    calls,
    env: {
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      HOME: process.env.HOME ?? dir,
      TMPDIR: process.env.TMPDIR ?? os.tmpdir(),
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_FAKE_ACTIVE: active ? "1" : "0",
      CRABBOX_FAKE_INVALID_LEASES: invalidLeases ? "1" : "0",
      CRABBOX_FAKE_CALLS: calls,
      CRABBOX_AWS_ORPHAN_AUDIT_GRACE_SECONDS: "0",
    },
  };
}

function runAudit(fake, extraArgs = []) {
  return spawnSync(
    "bash",
    ["scripts/aws-crabbox-orphan-audit.sh", "--profile", "test", "--region", "us-east-1", ...extraArgs],
    {
      cwd: root,
      env: { ...process.env, ...fake.env },
      encoding: "utf8",
    },
  );
}

function readCalls(fake) {
  if (!fs.existsSync(fake.calls)) {
    return [];
  }
  return fs
    .readFileSync(fake.calls, "utf8")
    .trim()
    .split("\n")
    .filter(Boolean)
    .map((line) => JSON.parse(line));
}

test("AWS orphan audit refuses destructive termination", () => {
  const fake = setupFakes();
  const result = runAudit(fake, ["--terminate"]);

  assert.equal(result.status, 2, result.stderr || result.stdout);
  assert.match(result.stderr, /--terminate is disabled/);
  assert.deepEqual(readCalls(fake), []);
});

test("AWS orphan audit reports stale unmanaged candidates in read-only mode", () => {
  const fake = setupFakes();
  const result = runAudit(fake);

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /"instanceId":"i-orphan"/);
  assert.match(result.stdout, /"reason":"expired-and-orphaned"/);
  assert.equal(
    readCalls(fake).some((args) => args[0] === "aws" && args[1] === "ec2" && args[2] === "terminate-instances"),
    false,
  );
});

test("AWS orphan audit fails closed on malformed active lease data", () => {
  const fake = setupFakes({ invalidLeases: true });
  const result = runAudit(fake);

  assert.equal(result.status, 1, result.stderr || result.stdout);
  assert.match(result.stderr, /invalid active coordinator lease response/);
  assert.equal(
    readCalls(fake).some((args) => args[0] === "aws"),
    false,
    "AWS should not be queried when coordinator lease data is malformed",
  );
});

test("AWS orphan audit suppresses candidates with an active cloud lease", () => {
  const fake = setupFakes({ active: true });
  const result = runAudit(fake);

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.equal(result.stdout.trim(), "");
  assert.equal(
    readCalls(fake).some((args) => args[0] === "aws" && args[1] === "ec2" && args[2] === "terminate-instances"),
    false,
  );
});
