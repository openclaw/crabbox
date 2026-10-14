import { spawn, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { arch, constants } from "node:os";

const [binary, binaryArch, cdhash] = process.argv.slice(2);
if (!binary || !["arm64", "x86_64"].includes(binaryArch)) {
  process.exit(2);
}

const lineLimit = 64 * 1024;
const validCDHash = /^[0-9a-f]{40}$/.test(cdhash ?? "") ? cdhash : null;
const retryDelays = [5000, 15000];
let interrupted;
let cancelAttempt;
let cancelBackoff;
for (const signal of ["SIGINT", "SIGTERM"]) {
  process.once(signal, () => {
    interrupted = signal;
    process.exitCode = 128 + constants.signals[signal];
    cancelAttempt?.(signal);
    cancelBackoff?.();
  });
}

function isTransient(status, diagnostic) {
  if (status !== 3 || diagnostic?.observerState !== "captured" || diagnostic.omittedEvents !== 0) return false;
  // Ordinary trace messages are unclassified; they cannot authorize a retry.
  const classified = diagnostic.events.filter((event) => event.category !== "unclassified");
  return classified.some((event) => event.attribution === "codesign-pid") &&
    classified.every((event) => event.category === "notarization-daemon-error" && event.daemonCode === 3);
}

async function backoff(milliseconds) {
  if (interrupted) return;
  await new Promise((resolve) => {
    const timer = setTimeout(resolve, milliseconds);
    cancelBackoff = () => { clearTimeout(timer); resolve(); };
  });
  cancelBackoff = undefined;
}

async function runAttempt(attempt) {
  const events = new Map();
  let attributed = 0;
  let omittedEvents = 0;
  let partialLine = "";
  let discardLine = false;
  let sawObserverReady = false;
  let collectorError = false;
  let checkPID;
  let observerError = false;
  let truncated = false;
  let stoppingObserver = false;
  let observerEndedEarly = false;
  let observerClosed = false;
  const observer = spawn(
    "/usr/bin/log",
    [
      "stream", "--style", "ndjson", "--level", "debug", "--timeout", "120",
      "--predicate", 'process == "codesign" OR process == "syspolicyd" OR subsystem == "com.apple.syspolicy"',
    ],
    { stdio: ["ignore", "pipe", "ignore"], detached: true },
  );
  observer.on("error", () => { observerError = true; });
  observer.on("close", () => {
    observerClosed = true;
    if (!stoppingObserver) observerEndedEarly = true;
  });
  observer.stdout.setEncoding("utf8");
  observer.stdout.on("data", (chunk) => {
    try { consumeChunk(chunk); } catch { collectorError = true; }
  });
  observer.stdout.on("end", () => {
    if (partialLine && !discardLine) {
      try { JSON.parse(partialLine); observeLine(partialLine); } catch { truncated = true; }
    }
    partialLine = "";
  });

  function observeLine(line) {
    if (line.startsWith("Filtering the log data using ")) {
      sawObserverReady = true;
      return;
    }
    let value;
    try { value = JSON.parse(line); } catch { return; }
    if (value === null || typeof value !== "object") return;
    const message = value.eventMessage;
    if (typeof message !== "string") return;
    sawObserverReady = true;
    const matchesHash = validCDHash !== null && (message.toLowerCase().match(/[0-9a-f]+/g)?.includes(validCDHash) ?? false);
    const matchesPID = Number.isSafeInteger(checkPID) && Number(value.processID) === checkPID;
    if (!matchesHash && !matchesPID) return;
    attributed++;
    let category = "unclassified";
    if (/invalid signature|signature (?:is )?invalid|not signed at all|wrong team|(?:unsatisfied|failed|invalid) designated requirement|does not satisfy.*(?:designated|signing) requirement/i.test(message)) category = "signature-rejection";
    else if (/ticket (?:is )?not available/i.test(message)) category = "ticket-unavailable";
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
    if (!events.has(key) && events.size >= 32) { omittedEvents++; return; }
    events.set(key, (events.get(key) ?? 0) + 1);
  }

  function consumeChunk(chunk) {
    let start = 0;
    while (start < chunk.length) {
      const newline = chunk.indexOf("\n", start);
      const part = chunk.slice(start, newline < 0 ? chunk.length : newline);
      if (!discardLine) {
        if (Buffer.byteLength(partialLine) + Buffer.byteLength(part) > lineLimit) {
          // Do not interpret a suffix of an oversized record as a new event.
          partialLine = "";
          discardLine = true;
          truncated = true;
        } else {
          partialLine += part;
        }
      }
      if (newline < 0) return;
      if (!discardLine) observeLine(partialLine);
      partialLine = "";
      discardLine = false;
      start = newline + 1;
    }
  }

  function signalObserver(signal) {
    if (observer.pid && observer.exitCode === null && observer.signalCode === null) {
      try { process.kill(-observer.pid, signal); } catch { /* Already exited. */ }
    }
  }

  async function stopObserver() {
    stoppingObserver = true;
    if (observerClosed || observerError) return;
    const closed = new Promise((resolve) => observer.once("close", resolve));
    signalObserver("SIGINT");
    const timer = setTimeout(() => signalObserver("SIGKILL"), 500);
    const deadline = setTimeout(() => { truncated = true; observer.stdout.destroy(); }, 1500);
    let drain;
    await Promise.race([closed, new Promise((resolve) => { drain = setTimeout(resolve, 2000); })]);
    clearTimeout(timer);
    clearTimeout(deadline);
    clearTimeout(drain);
  }

  let check;
  cancelAttempt = (signal) => {
    check?.kill(signal);
    signalObserver("SIGKILL");
  };
  const observerReady = await new Promise((resolve) => {
    const finish = (ready) => {
      clearTimeout(timer);
      observer.stdout.off("data", onData);
      observer.off("error", unavailable);
      observer.off("close", unavailable);
      resolve(ready);
    };
    const unavailable = () => finish(false);
    const onData = () => { if (sawObserverReady) finish(true); };
    const timer = setTimeout(() => finish(false), 2000);
    observer.stdout.on("data", onData);
    observer.once("error", unavailable);
    observer.once("close", unavailable);
  });
  if (interrupted) {
    await stopObserver();
    return { status: 128 + constants.signals[interrupted], checked: false };
  }
  const startedAt = new Date().toISOString();
  check = spawn(
    "codesign",
    ["--verify", "--strict", "--check-notarization", "-R=notarized", "--verbose=4", binary],
    { stdio: "inherit" },
  );
  checkPID = check.pid;
  const status = await new Promise((resolve) => {
    check.once("error", () => resolve(127));
    check.once("exit", (code, signal) => resolve(code ?? (128 + (constants.signals[signal] ?? 1))));
  });
  const completedAt = new Date().toISOString();
  await stopObserver();

  let diagnostic;
  if (status !== 0) {
    try {
      const build = spawnSync("/usr/bin/sw_vers", ["-buildVersion"], { encoding: "utf8", timeout: 2000, maxBuffer: 1024 });
      const osBuild = /^[A-Za-z0-9.]+\s*$/.test(build.stdout ?? "") ? build.stdout.trim() : null;
      diagnostic = {
        type: "notarization-diagnostic", attempt, startedAt, completedAt, checkPID, exitCode: status,
        binaryArch, hostArch: arch(), osBuild, cdhash: validCDHash,
        binarySHA256: createHash("sha256").update(readFileSync(binary)).digest("hex"),
        evidence: attributed ? "attributed-events" : "unknown",
        observerState: observerError || !observerReady ? "unavailable" : observerEndedEarly || truncated || collectorError ? "partial" : "captured",
        truncated, omittedEvents, attributedEvents: attributed,
        ...(collectorError ? { collectorError: true } : {}),
        events: [...events].map(([event, count]) => ({ ...JSON.parse(event), count })),
      };
    } catch {
      diagnostic = { type: "notarization-diagnostic", attempt, exitCode: status, evidence: "unknown", collectorError: true };
    }
    console.error(JSON.stringify(diagnostic));
  }
  cancelAttempt = undefined;
  return { status, checked: true, diagnostic };
}

const exitCodes = [];
let status = 0;
for (let attempt = 1; attempt <= retryDelays.length + 1; attempt++) {
  const result = await runAttempt(attempt);
  status = result.status;
  if (result.checked) exitCodes.push(status);
  if (interrupted || attempt > retryDelays.length || !isTransient(status, result.diagnostic)) break;
  await backoff(retryDelays[attempt - 1]);
  if (interrupted) break;
}
process.exitCode = interrupted ? 128 + constants.signals[interrupted] : status;
console.error(JSON.stringify({
  type: "notarization-summary", attempts: exitCodes.length, exitCodes,
  exitCode: process.exitCode, verdict: process.exitCode === 0 ? "pass" : "fail",
}));
