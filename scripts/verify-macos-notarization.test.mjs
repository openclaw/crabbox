import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

function fixture(t, settings = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-notary-diagnostic-"));
  t.after(() => {
    const file = path.join(root, "events");
    if (fs.existsSync(file)) {
      for (const event of fs.readFileSync(file, "utf8").trim().split("\n").map(JSON.parse)) {
        if (event.event !== "observer") continue;
        const command = spawnSync("/bin/ps", ["-p", String(event.pid), "-o", "command="], { encoding: "utf8" }).stdout ?? "";
        if (command.includes(path.join(root, "bin", "log"))) {
          try { process.kill(-event.pid, "SIGKILL"); } catch { /* Already exited. */ }
        }
      }
    }
    fs.rmSync(root, { recursive: true, force: true });
  });
  for (const dir of ["bin", "scripts", "home"]) fs.mkdirSync(path.join(root, dir));
  for (const file of ["verify-macos-binary.sh", "release-config.sh"]) {
    fs.copyFileSync(path.join(import.meta.dirname, file), path.join(root, "scripts", file));
  }
  // Replace only the two absolute native metadata tools inside the isolated fixture.
  const helper = fs.readFileSync(path.join(import.meta.dirname, "verify-macos-notarization.mjs"), "utf8")
    .replaceAll('"/usr/bin/log"', JSON.stringify(path.join(root, "bin", "log")))
    .replaceAll('"/usr/bin/sw_vers"', JSON.stringify(path.join(root, "bin", "sw_vers")));
  fs.writeFileSync(path.join(root, "scripts", "verify-macos-notarization.mjs"), helper);
  fs.symlinkSync(process.execPath, path.join(root, "bin", "node"));
  fs.writeFileSync(path.join(root, "settings.json"), JSON.stringify(settings));
  fs.writeFileSync(path.join(root, "bin", "tool"), String.raw`#!/usr/bin/env node
const fs = require("node:fs");
const path = require("node:path");
const root = path.dirname(__dirname);
const settings = JSON.parse(fs.readFileSync(path.join(root, "settings.json")));
const tool = path.basename(process.argv[1]);
const args = process.argv.slice(2);
const record = (event) => fs.appendFileSync(path.join(root, "events"), JSON.stringify({event, pid:process.pid, args})+"\n");
const requirement = 'identifier "org.openclaw.crabbox" and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "FWJYW4S8P8"';
if (tool === "uname") console.log(args[0] === "-s" ? "Darwin" : "x86_64");
else if (tool === "lipo") console.log("x86_64");
else if (tool === "csreq") console.log(args[1]);
else if (tool === "sw_vers") console.log("24A123");
else if (tool === "plutil") process.exit(99);
else if (tool === "codesign") {
  if (args.includes("--check-notarization")) {
    record("online"); fs.writeFileSync(path.join(root, "check-pid"), String(process.pid));
    if (settings.checkSignal) process.kill(process.pid, settings.checkSignal);
    else if (settings.streamMode) {
      const done = setInterval(() => {
        if (fs.existsSync(path.join(root, "stream-complete"))) {
          clearInterval(done); process.exit(settings.onlineStatus ?? 0);
        }
      }, 20);
      setTimeout(() => process.exit(98), 10000);
    }
    else setTimeout(() => process.exit(settings.onlineStatus ?? 0), 100);
  } else if (args[0] === "-dvvv") process.stderr.write([
    "Identifier=org.openclaw.crabbox", "Authority=Developer ID Application: OpenClaw Foundation (FWJYW4S8P8)",
    "TeamIdentifier=FWJYW4S8P8", "CodeDirectory v=20500 size=1 flags=0x10000(runtime)",
    "Timestamp=Jan 1, 2026 at 00:00:00", "CDHash="+"a".repeat(40), "",
  ].join("\n"));
  else if (args.includes("-r-")) process.stderr.write("designated => "+requirement+"\n");
  else { record("signature"); process.exit(settings.signatureStatus ?? 0); }
} else if (tool === "log") {
  record("observer");
  if (settings.observer === "error") process.exit(1);
  if (settings.cancelBeforeReady) { process.kill(process.ppid, "SIGTERM"); setInterval(() => {},1000); return; }
  if (settings.observer !== "silent") console.log("Filtering the log data using fixture predicate");
  if (settings.observer === "early") process.exit(0);
  process.on("SIGINT", () => {
    if (settings.cancelOnDrain) { process.kill(process.ppid,"SIGTERM"); return; }
    if (settings.observer !== "silent") process.exit(0);
  });
  const timer = setInterval(() => {
    const p = path.join(root, "check-pid"); if (!fs.existsSync(p)) return;
    clearInterval(timer);
    const pid = Number(fs.readFileSync(p));
    const output = [null,
      {processID:pid, eventMessage:"trace é Error checking with notarization daemon: -67050 Error Domain=NSURLErrorDomain Code=-1005 PRIVATE_PATH https://private.invalid/token"},
      {processID:999999, eventMessage:"Error Domain=NSURLErrorDomain Code=-1001 PRIVATE_PATH"},
      {processID:999998, eventMessage:"ticket not available for "+"A".repeat(40)+" PRIVATE_PATH"},
      {processID:999997, eventMessage:"ticket not available for b"+"a".repeat(40)+"c PRIVATE_PATH"},
    ].map(value => JSON.stringify(value)+"\n").join("");
    const complete = () => fs.writeFileSync(path.join(root,"stream-complete"),"yes");
    const emitTarget = () => process.stdout.write(output, complete);
    if (settings.streamMode === "flood") {
      const prefix = '{"processID":999996,"eventMessage":"';
      const suffix = '"}\n';
      const unrelated = prefix+"x".repeat(64-prefix.length-suffix.length)+suffix;
      let sent = 0;
      const flood = setInterval(() => {
        process.stdout.write(unrelated);
        if (++sent === 4500) { clearInterval(flood); setTimeout(emitTarget,20); }
      }, 1);
    } else if (settings.streamMode === "oversized") {
      process.stdout.write("x".repeat(65537), () => {
        process.stdout.write(JSON.stringify({processID:pid,eventMessage:"Error checking with notarization daemon: -9999"})+"\n", emitTarget);
      });
    } else if (settings.streamMode === "split") {
      const bytes = Buffer.from(output);
      const split = bytes.indexOf(Buffer.from("é"))+1;
      process.stdout.write(bytes.subarray(0,split), () => {
        setTimeout(() => process.stdout.write(bytes.subarray(split),complete),20);
      });
    } else if (settings.streamMode === "groups") {
      for (let code=1000; code<1040; code++) {
        process.stdout.write(JSON.stringify({processID:pid,eventMessage:"Error checking with notarization daemon: "+code})+"\n");
      }
      emitTarget();
    } else if (settings.streamMode?.endsWith("eof")) {
      const target = JSON.stringify({processID:pid,eventMessage:"Error checking with notarization daemon: -67050"});
      const tail = settings.streamMode === "oversized-eof" ? "x".repeat(65537)+target
        : settings.streamMode === "incomplete-eof" ? target.slice(0,-1) : target;
      process.stdout.end(tail, () => {
        complete();
        if (settings.streamMode !== "open-eof") process.exit(0);
      });
    } else emitTarget();
    setInterval(() => {},1000);
  },10);
}
`, { mode: 0o755 });
  for (const name of ["uname", "lipo", "csreq", "sw_vers", "plutil", "codesign", "log"]) {
    fs.symlinkSync("tool", path.join(root, "bin", name));
  }
  const binary = path.join(root, "crabbox");
  fs.writeFileSync(binary, `#!/bin/bash\necho executed >> '${root}/executed'\n`, { mode: 0o755 });
  const result = spawnSync("/bin/bash", [path.join(root, "scripts", "verify-macos-binary.sh"),
    "org.openclaw.crabbox", "x86_64", binary, "--execute"], {
    encoding: "utf8", timeout: 15_000,
    env: { PATH: `${root}/bin:/usr/bin:/bin`, HOME: `${root}/home`, TMPDIR: root, LANG: "C", LC_ALL: "C" },
  });
  const events = fs.existsSync(path.join(root, "events"))
    ? fs.readFileSync(path.join(root, "events"), "utf8").trim().split("\n").map(JSON.parse) : [];
  for (const event of events.filter((e) => e.event === "observer")) {
    assert.throws(() => process.kill(event.pid, 0), { code: "ESRCH" });
  }
  const line = result.stderr.split("\n").find((value) => value.startsWith('{"type":"notarization-diagnostic"'));
  return { ...result, events, diagnostic: line ? JSON.parse(line) : null, executed: fs.existsSync(path.join(root, "executed")) };
}

test("signature rejection preserves its status without starting online verification or execution", (t) => {
  const r = fixture(t, { signatureStatus: 7 });
  assert.equal(r.status, 7, r.stderr);
  assert.deepEqual(r.events.map((e) => e.event), ["signature"]);
  assert.equal(r.executed, false);
});

test("online failure emits only attributed allowlisted fields and never executes", (t) => {
  const r = fixture(t, { onlineStatus: 3 });
  assert.equal(r.status, 3, r.stderr);
  assert.equal(r.events.filter((e) => e.event === "online").length, 1);
  assert.equal(r.executed, false);
  assert.equal(r.diagnostic.exitCode, 3);
  assert.equal(r.diagnostic.observerState, "captured");
  assert.equal(r.diagnostic.attributedEvents, 2);
  assert.equal(r.diagnostic.events.some((e) => e.daemonCode === -67050 && e.domain === "NSURLErrorDomain" && e.code === -1005), true);
  assert.equal(r.diagnostic.events.some((e) => e.code === -1001), false);
  assert.doesNotMatch(r.stderr, /PRIVATE_PATH|private\.invalid/);
});

for (const streamMode of ["flood", "oversized", "split"]) {
  test(`late attributed errors survive ${streamMode} records`, (t) => {
    const r = fixture(t, { onlineStatus: 3, streamMode });
    assert.equal(r.status, 3, r.stderr);
    assert.equal(r.executed, false);
    assert.equal(r.events.filter((e) => e.event === "online").length, 1);
    assert.equal(r.diagnostic.attributedEvents, 2);
    assert.equal(r.diagnostic.truncated, streamMode === "oversized");
    assert.equal(r.diagnostic.observerState, streamMode === "oversized" ? "partial" : "captured");
    assert.equal(r.diagnostic.events.some((e) => e.daemonCode === -67050 && e.domain === "NSURLErrorDomain" && e.code === -1005), true);
    assert.equal(r.diagnostic.events.some((e) => e.daemonCode === -9999), false);
    assert.doesNotMatch(r.stderr, /PRIVATE_PATH|private\.invalid/);
  });
}

test("attributed stream aggregation stays bounded and reports omitted groups", (t) => {
  const r = fixture(t, { onlineStatus: 3, streamMode: "groups" });
  assert.equal(r.status, 3, r.stderr);
  assert.equal(r.executed, false);
  assert.equal(r.diagnostic.events.length, 32);
  assert.equal(r.diagnostic.attributedEvents, 42);
  assert.equal(r.diagnostic.omittedEvents, 10);
});

for (const streamMode of ["eof", "open-eof", "oversized-eof", "incomplete-eof"]) {
  test(`stream completion handles ${streamMode} without losing framing`, (t) => {
    const r = fixture(t, { onlineStatus: 3, streamMode });
    const validTail = streamMode === "eof" || streamMode === "open-eof";
    assert.equal(r.status, 3, r.stderr);
    assert.equal(r.executed, false);
    assert.equal(r.events.filter((e) => e.event === "online").length, 1);
    assert.equal(r.diagnostic.truncated, !validTail);
    assert.equal(r.diagnostic.observerState, streamMode === "open-eof" ? "captured" : "partial");
    assert.equal(r.diagnostic.attributedEvents, validTail ? 1 : 0);
    assert.equal(r.diagnostic.events.some((e) => e.daemonCode === -67050), validTail);
  });
}

for (const observer of ["error", "early", "silent"]) {
  test(`observer ${observer} cannot mask online rejection`, (t) => {
    const r = fixture(t, { onlineStatus: 3, observer });
    assert.equal(r.status, 3, r.stderr);
    assert.equal(r.executed, false);
    assert.notEqual(r.diagnostic.observerState, "captured");
  });
}

test("successful online verification runs once and permits execution without diagnostics", (t) => {
  const r = fixture(t);
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.executed, true);
  assert.equal(r.events.filter((e) => e.event === "online").length, 1);
  assert.equal(r.diagnostic, null);
});

test("termination while draining the observer never permits candidate execution", (t) => {
  const r = fixture(t, { cancelOnDrain: true });
  assert.equal(r.status, 143, r.stderr);
  assert.equal(r.executed, false);
});

test("a signaled online checker keeps the conventional signal exit status", (t) => {
  const r = fixture(t, { checkSignal: "SIGKILL" });
  assert.equal(r.status, 137, r.stderr);
  assert.equal(r.executed, false);
});

test("cancellation before observer readiness stops its group without launching a check", (t) => {
  const r = fixture(t, { cancelBeforeReady: true });
  assert.equal(r.status, 143, r.stderr);
  assert.equal(r.events.filter((e) => e.event === "online").length, 0);
  assert.equal(r.executed, false);
  const observerPID = r.events.find((e) => e.event === "observer").pid;
  assert.throws(() => process.kill(observerPID, 0), { code: "ESRCH" });
});
