import { spawn, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { arch, constants } from "node:os";

const [binary, binaryArch, cdhash] = process.argv.slice(2);
if (!binary || !["arm64", "x86_64"].includes(binaryArch)) {
  process.exit(2);
}

const limit = 256 * 1024;
let logData = "";
let observerError = false;
let truncated = false;
let stoppingObserver = false;
let observerEndedEarly = false;
const observer = spawn(
  "/usr/bin/log",
  [
    "stream", "--style", "ndjson", "--level", "debug", "--timeout", "120",
    "--predicate", 'process == "codesign" OR process == "syspolicyd" OR subsystem == "com.apple.syspolicy"',
  ],
  { stdio: ["ignore", "pipe", "ignore"], detached: true },
);
observer.on("error", () => { observerError = true; });
observer.on("close", () => { if (!stoppingObserver) observerEndedEarly = true; });
observer.stdout.on("data", (chunk) => {
  if (Buffer.byteLength(logData) + chunk.length <= limit) {
    logData += chunk.toString();
  } else {
    truncated = true;
  }
});

function signalObserver(signal) {
  if (observer.pid && observer.exitCode === null && observer.signalCode === null) {
    try { process.kill(-observer.pid, signal); } catch { /* Already exited. */ }
  }
}

async function stopObserver() {
  stoppingObserver = true;
  if (observer.exitCode !== null || observer.signalCode !== null || observerError) return;
  const closed = new Promise((resolve) => observer.once("close", resolve));
  signalObserver("SIGINT");
  const timer = setTimeout(() => signalObserver("SIGKILL"), 500);
  const deadline = setTimeout(() => observer.stdout.destroy(), 1500);
  let drain;
  await Promise.race([closed, new Promise((resolve) => { drain = setTimeout(resolve, 2000); })]);
  clearTimeout(timer);
  clearTimeout(deadline);
  clearTimeout(drain);
}

let interrupted;
let check;
for (const signal of ["SIGINT", "SIGTERM"]) {
  process.once(signal, () => {
    interrupted = signal;
    process.exitCode = 128 + constants.signals[signal];
    check?.kill(signal);
    signalObserver("SIGKILL");
  });
}
const observerReady = await new Promise((resolve) => {
  const finish = (ready) => {
    clearTimeout(timer);
    observer.stdout.off("data", onData);
    observer.off("error", unavailable);
    observer.off("close", unavailable);
    resolve(ready);
  };
  const unavailable = () => finish(false);
  const onData = () => {
    const ready = logData.split("\n").some((line) => {
      if (line.startsWith("Filtering the log data using ")) return true;
      try { return typeof JSON.parse(line)?.eventMessage === "string"; } catch { return false; }
    });
    if (ready) finish(true);
  };
  const timer = setTimeout(() => finish(false), 2000);
  observer.stdout.on("data", onData);
  observer.once("error", unavailable);
  observer.once("close", unavailable);
});
if (interrupted) {
  await stopObserver();
  process.exit(128 + constants.signals[interrupted]);
}
const startedAt = new Date().toISOString();
check = spawn(
  "codesign",
  ["--verify", "--strict", "--check-notarization", "-R=notarized", "--verbose=4", binary],
  { stdio: "inherit" },
);
const checkPID = check.pid;
const status = await new Promise((resolve) => {
  check.once("error", () => resolve(127));
  check.once("exit", (code, signal) => resolve(code ?? (128 + (constants.signals[signal] ?? 1))));
});
const completedAt = new Date().toISOString();
await stopObserver();

if (status !== 0) {
  try {
    const validCDHash = /^[0-9a-f]{40}$/.test(cdhash ?? "") ? cdhash : null;
    const events = new Map();
    let attributed = 0;
    let omittedEvents = 0;
    for (const line of logData.split("\n")) {
      let value;
      try { value = JSON.parse(line); } catch { continue; }
      if (value === null || typeof value !== "object") continue;
      const message = value.eventMessage;
      if (typeof message !== "string") continue;
      const matchesHash = validCDHash !== null && (message.toLowerCase().match(/[0-9a-f]+/g)?.includes(validCDHash) ?? false);
      const matchesPID = Number.isSafeInteger(checkPID) && Number(value.processID) === checkPID;
      if (!matchesHash && !matchesPID) continue;
      attributed++;
      let category = "unclassified";
      if (/ticket (?:is )?not available/i.test(message)) category = "ticket-unavailable";
      else if (/Notarization daemon found revoked hash:/i.test(message)) category = "revoked-hash";
      else if (/Error checking with notarization daemon/i.test(message)) category = "notarization-daemon-error";
      const daemonCode = message.match(/Error checking with notarization daemon:\s*(-?\d{1,10})\b/i);
      const domainCode = message.match(/\b(NSURLErrorDomain|NSOSStatusErrorDomain|NSPOSIXErrorDomain|kCFErrorDomainCFNetwork)\b.{0,80}?\bCode\s*=\s*(-?\d{1,10})\b/);
      const event = {
        attribution: matchesHash ? "cdhash" : "codesign-pid",
        category,
        daemonCode: Number(daemonCode?.[1] ?? NaN),
        domain: domainCode?.[1] ?? null,
        code: Number(domainCode?.[2] ?? NaN),
      };
      const key = JSON.stringify(event);
      if (!events.has(key) && events.size >= 32) { omittedEvents++; continue; }
      events.set(key, (events.get(key) ?? 0) + 1);
    }
    const build = spawnSync("/usr/bin/sw_vers", ["-buildVersion"], { encoding: "utf8", timeout: 2000, maxBuffer: 1024 });
    const osBuild = /^[A-Za-z0-9.]+\s*$/.test(build.stdout ?? "") ? build.stdout.trim() : null;
    console.error(JSON.stringify({
      type: "notarization-diagnostic", startedAt, completedAt, checkPID, exitCode: status,
      binaryArch, hostArch: arch(), osBuild, cdhash: validCDHash,
      binarySHA256: createHash("sha256").update(readFileSync(binary)).digest("hex"),
      evidence: attributed ? "attributed-events" : "unknown",
      observerState: observerError || !observerReady ? "unavailable" : observerEndedEarly || truncated ? "partial" : "captured",
      truncated, omittedEvents, attributedEvents: attributed,
      events: [...events].map(([event, count]) => ({ ...JSON.parse(event), count })),
    }));
  } catch {
    console.error('{"type":"notarization-diagnostic","evidence":"unknown","collectorError":true}');
  }
}
process.exitCode = interrupted ? 128 + constants.signals[interrupted] : status;
