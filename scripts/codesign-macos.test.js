import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { writeExecutable } from "./test-support/smoke-fixtures.mjs";

const scripts = import.meta.dirname;
const receipt = "11111111-1111-4111-8111-111111111111";
const deadline = "Error: abortedUpload(resumeRequest: synthetic, error: HTTPClientError.deadlineExceeded)";
const aborted = { code: 1, error: deadline };

function fixture(t, settings = {}) {
  const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-notary-upload-")));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  for (const name of ["scripts", "bin", "tmp", "home"]) fs.mkdirSync(path.join(root, name));
  for (const name of ["codesign-macos.sh", "release-config.sh"]) {
    fs.copyFileSync(path.join(scripts, name), path.join(root, "scripts", name));
  }
  // Only fixture Apple tools and selected local utilities are reachable.
  // Real credentials, signing tools, and network clients cannot be used.
  for (const name of ["env", "dirname", "mktemp", "rm", "grep", "sed", "basename", "shasum", "cat"]) {
    const tool = execFileSync("/bin/sh", ["-c", 'command -v "$1"', "fixture", name], {
      encoding: "utf8",
    }).trim();
    fs.symlinkSync(tool, path.join(root, "bin", name));
  }
  fs.symlinkSync("/bin/bash", path.join(root, "bin", "bash"));
  fs.symlinkSync(process.execPath, path.join(root, "bin", "node"));
  fs.writeFileSync(path.join(root, "settings.json"), JSON.stringify({ receipt, ...settings }));
  const binary = path.join(root, "crabbox");
  fs.writeFileSync(binary, "synthetic executable\n", { mode: 0o755 });
  writeExecutable(path.join(root, "bin", "fixture-tool"), String.raw`#!/usr/bin/env node
const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const fs = require("node:fs");
const path = require("node:path");
const root = path.dirname(__dirname);
const settings = JSON.parse(fs.readFileSync(path.join(root, "settings.json")));
const args = process.argv.slice(2);
const tool = path.basename(process.argv[1]);
const eventsFile = path.join(root, "events.jsonl");
const events = () => fs.existsSync(eventsFile)
  ? fs.readFileSync(eventsFile, "utf8").trim().split("\n").map(JSON.parse) : [];
const record = (entry) => fs.appendFileSync(eventsFile, JSON.stringify(entry) + "\n");
const output = (value) => process.stdout.write(value + "\n");
const requirement = 'identifier "org.openclaw.crabbox" and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "FWJYW4S8P8"';
for (const name of ["GH_TOKEN", "GITHUB_TOKEN", "OP_SERVICE_ACCOUNT_TOKEN", "MOLTY_OP_SERVICE_ACCOUNT_TOKEN"]) {
  assert.equal(process.env[name], undefined, name + " reached fixture");
}
if (tool === "uname") output("Darwin");
else if (tool === "lipo") output("x86_64");
else if (tool === "csreq") output(args[1]);
else if (tool === "ditto") {
  record({ event: "archive" });
  fs.copyFileSync(args.at(-2), args.at(-1));
} else if (tool === "codesign") {
  if (args.includes("--force")) {
    record({ event: "sign" });
    fs.appendFileSync(args.at(-1), "synthetic signature\n");
  } else if (args.includes("--check-notarization")) {
    record({ event: "ticket" });
    if (settings.ticketFailure) process.exit(1);
  } else if (args[0] === "-dvvv") {
    process.stderr.write([
      "Identifier=org.openclaw.crabbox", "Authority=Developer ID Application: OpenClaw Foundation (FWJYW4S8P8)",
      "TeamIdentifier=FWJYW4S8P8", "CodeDirectory v=20500 size=1 flags=0x10000(runtime)",
      "Timestamp=Jan 1, 2026 at 00:00:00", "",
    ].join("\n"));
  } else if (args.includes("-r-")) process.stderr.write("designated => " + requirement + "\n");
  else assert.ok(args.includes("--verify"), "unexpected codesign call");
} else if (tool === "plutil") {
  assert.equal(args[0], "-extract");
  const result = JSON.parse(fs.readFileSync(args.at(-1), "utf8"));
  if (result[args[1]] === undefined) process.exit(1);
  output(result[args[1]]);
} else if (tool === "sleep") {
  record({ event: "sleep", seconds: args[0] });
  if (settings.cancelWait) process.kill(process.pid, "SIGTERM");
} else if (tool === "xcrun") {
  assert.deepEqual(args.slice(0, 2), ["notarytool", "submit"]);
  assert.equal(args[args.indexOf("--keychain-profile") + 1], "synthetic-profile");
  assert.equal(args[args.indexOf("--keychain") + 1], "/synthetic/keychain");
  assert.ok(args.includes("--wait"));
  assert.equal(args[args.indexOf("--output-format") + 1], "json");
  const calls = events().filter((entry) => entry.event === "submit");
  const step = settings.steps?.[calls.length] ?? {};
  const modes = args.filter((arg) => arg === "--no-s3-acceleration" || arg === "--s3-acceleration");
  assert.equal(modes.length, 1);
  record({ event: "submit", mode: modes[0], archive: args[2],
    sha256: crypto.createHash("sha256").update(fs.readFileSync(args[2])).digest("hex") });
  if (step.mutateArchive) fs.appendFileSync(args[2], "changed after submission\n");
  if (step.error) process.stderr.write(step.error + "\n");
  if (step.result !== undefined) process.stdout.write(step.result);
  else if (!step.code && !step.signal) output(JSON.stringify({ status: "Accepted", id: settings.receipt }));
  if (step.signal) process.kill(process.pid, step.signal);
  else process.exit(step.code ?? 0);
} else throw new Error("unexpected fixture tool: " + tool);
`);
  for (const name of ["uname", "lipo", "csreq", "ditto", "codesign", "plutil", "sleep", "xcrun"]) {
    fs.symlinkSync("fixture-tool", path.join(root, "bin", name));
  }
  return {
    root,
    run() {
      const result = spawnSync("/bin/bash", [path.join(root, "scripts", "codesign-macos.sh"),
        "org.openclaw.crabbox", "x86_64", binary], {
        encoding: "utf8", timeout: 20_000,
        env: {
          PATH: path.join(root, "bin"), HOME: path.join(root, "home"), TMPDIR: path.join(root, "tmp"),
          LANG: "C", LC_ALL: "C", CODESIGN_IDENTITY: "Developer ID Application: OpenClaw Foundation (FWJYW4S8P8)",
          NOTARYTOOL_KEYCHAIN_PROFILE: "synthetic-profile", MAC_RELEASE_CODESIGN_KEYCHAIN: "/synthetic/keychain",
          ...(settings.acceleration === undefined ? {} : { CRABBOX_NOTARY_S3_ACCELERATION: settings.acceleration }),
        },
      });
      const file = path.join(root, "events.jsonl");
      const events = fs.existsSync(file) ? fs.readFileSync(file, "utf8").trim().split("\n").map(JSON.parse) : [];
      return { ...result, events, submissions: events.filter((entry) => entry.event === "submit") };
    },
  };
}

test("ordinary notarization uses one regional upload and verifies its online ticket", (t) => {
  const result = fixture(t).run();
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.submissions.map((call) => call.mode), ["--no-s3-acceleration"]);
  assert.equal(result.stdout, `Notarization accepted: ${receipt}\n`);
  assert.ok(result.events.some((event) => event.event === "ticket"));
});

test("regional upload deadline retries the same signed archive once through acceleration", (t) => {
  const result = fixture(t, { steps: [aborted, {}] }).run();
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.submissions.map((call) => call.mode), ["--no-s3-acceleration", "--s3-acceleration"]);
  assert.equal(result.submissions[0].archive, result.submissions[1].archive);
  assert.equal(result.submissions[0].sha256, result.submissions[1].sha256);
  assert.equal(result.events.filter((event) => event.event === "sign").length, 1);
  assert.equal(result.events.filter((event) => event.event === "archive").length, 1);
  assert.equal(result.stdout, `Notarization accepted: ${receipt}\n`);
  assert.ok(result.stderr.includes(deadline));
});

test("a second upload failure stops without a receipt or online-ticket check", (t) => {
  const result = fixture(t, { steps: [aborted, aborted] }).run();
  assert.notEqual(result.status, 0);
  assert.equal(result.submissions.length, 2);
  assert.equal(result.stdout, "");
  assert.equal(result.events.filter((event) => event.event === "ticket").length, 0);
});

test("explicit acceleration uses one upload without revisiting the regional route", async (t) => {
  await t.test("success", (t) => {
    const result = fixture(t, { acceleration: "1" }).run();
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(result.submissions.map((call) => call.mode), ["--s3-acceleration"]);
    assert.equal(result.stdout, `Notarization accepted: ${receipt}\n`);
  });
  await t.test("failure", (t) => {
    const result = fixture(t, { acceleration: "1", steps: [aborted] }).run();
    assert.notEqual(result.status, 0);
    assert.deepEqual(result.submissions.map((call) => call.mode), ["--s3-acceleration"]);
    assert.equal(result.stdout, "");
  });
  await t.test("invalid setting", (t) => {
    const result = fixture(t, { acceleration: "invalid" }).run();
    assert.equal(result.status, 2);
    assert.equal(result.submissions.length, 0);
    assert.equal(result.events.length, 0);
  });
});

test("ambiguous, unrelated, receipted, and signalled failures are never retried", async (t) => {
  const cases = [
    { name: "authentication", step: { code: 1, error: "Error: invalid credentials" } },
    { name: "extra diagnostic", step: { code: 1, error: deadline + "\nError: another failure" } },
    { name: "polling timeout", step: { code: 1, error: "Error: timed out waiting for notarization" } },
    { name: "receipt", step: { ...aborted, result: JSON.stringify({ id: receipt, status: "In Progress" }) } },
    { name: "whitespace result", step: { ...aborted, result: " " } },
    { name: "signal", step: { error: deadline, signal: "SIGTERM" } },
  ];
  for (const entry of cases) await t.test(entry.name, (t) => {
    const result = fixture(t, { steps: [entry.step] }).run();
    assert.notEqual(result.status, 0);
    assert.equal(result.submissions.length, 1);
    assert.equal(result.stdout, "");
  });
});

test("archive mutation and cancellation during backoff stop before the second upload", async (t) => {
  for (const settings of [{ steps: [{ ...aborted, mutateArchive: true }] }, { steps: [aborted], cancelWait: true }]) {
    await t.test(settings.cancelWait ? "cancelled" : "changed archive", (t) => {
      const result = fixture(t, settings).run();
      assert.notEqual(result.status, 0);
      assert.equal(result.submissions.length, 1);
      assert.equal(result.stdout, "");
    });
  }
});

test("fallback still requires acceptance and a valid submission ID", async (t) => {
  for (const response of [{ status: "Invalid", id: receipt }, { status: "Accepted" }, { status: "Accepted", id: "bad-id" }]) {
    await t.test(JSON.stringify(response), (t) => {
      const result = fixture(t, { steps: [aborted, { result: JSON.stringify(response) }] }).run();
      assert.notEqual(result.status, 0);
      assert.equal(result.submissions.length, 2);
      assert.equal(result.stdout, "");
      assert.equal(result.events.filter((event) => event.event === "ticket").length, 0);
    });
  }
});

test("no receipt is emitted until accepted notarization verifies online", (t) => {
  const result = fixture(t, { steps: [aborted, {}], ticketFailure: true }).run();
  assert.notEqual(result.status, 0);
  assert.equal(result.submissions.length, 2);
  assert.equal(result.events.filter((event) => event.event === "ticket").length, 12);
  assert.equal(result.stdout, "");
});

test("packaging rejects a failed signer even when it prints a valid-looking receipt", (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-notary-receipt-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  fs.mkdirSync(path.join(root, "scripts"));
  writeExecutable(path.join(root, "scripts", "codesign-macos.sh"),
    `#!/bin/sh\nprintf '%s\\n' 'Notarization accepted: ${receipt}'\nexit 7\n`);
  const packager = fs.readFileSync(path.join(scripts, "package-release.sh"), "utf8");
  const capture = packager.match(/^sign_and_capture_notary_id\(\) \{[\s\S]*?^\}/m)?.[0];
  assert.ok(capture, "packager signing receipt boundary is present");
  const result = spawnSync("/bin/bash", ["-c", `set -euo pipefail\nROOT=$1\n${capture}\nreceipt=$(sign_and_capture_notary_id fixture x86_64 fixture)\nprintf '%s\\n' "$receipt"`, "fixture", root], {
    encoding: "utf8", env: { PATH: "/usr/bin:/bin", LANG: "C", LC_ALL: "C" },
  });
  assert.equal(result.status, 7, result.stderr);
  assert.equal(result.stdout, "");
});
