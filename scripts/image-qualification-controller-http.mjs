import { setTimeout as delay } from "node:timers/promises";

// Trusted qualification controller transport only. A failed reply never permits
// replay: the original journal and canonical owner still decide settlement.
const actions = new Set([
  "claim",
  "key-authorization",
  "attest",
  "discover",
  "finalize",
  "begin-finalization",
  "retire",
  "transport-open",
  "transport-discover",
  "transport-settle",
]);
export const controllerResponseMaxBytes = 4 * 1024 * 1024;

function contentType(response) {
  const mime = response.headers
    .get("content-type")
    ?.slice(0, 256)
    .split(";", 1)[0]
    .trim()
    .toLowerCase();
  if (!mime) return "missing";
  if (mime === "application/json") return "json";
  if (mime === "text/html") return "html";
  if (mime === "text/plain") return "text";
  return "other";
}

export class ControllerHTTPError extends Error {
  constructor(receipt) {
    // No URL, request/body bytes, server message or underlying exception enters
    // this error, its cause, or its public diagnostic receipt.
    super(`qualification controller request failed: ${JSON.stringify(receipt)}`);
    this.receipt = Object.freeze(receipt);
  }
}

export async function requestControllerJSON({
  url,
  token,
  action,
  value = {},
  signal,
  deadline,
  timeoutMs = ["finalize", "key-authorization"].includes(action) ? 300_000 : 30_000,
  invoke = fetch,
}) {
  if (
    !actions.has(action) ||
    !Number.isSafeInteger(timeoutMs) ||
    timeoutMs < 1 ||
    timeoutMs > (["finalize", "key-authorization"].includes(action) ? 300_000 : 30_000) ||
    (deadline !== undefined && !Number.isFinite(deadline))
  )
    throw new Error("invalid qualification controller request bounds or action");
  const started = Date.now();
  const elapsedStart = performance.now();
  const remaining = deadline === undefined ? Infinity : deadline - started;
  const stop = new AbortController();
  const combined = signal ? AbortSignal.any([signal, stop.signal]) : stop.signal;
  const receipt = {
    version: 1,
    action,
    reason: "network-error",
    status: null,
    contentType: "missing",
    bodyFormat: "unread",
    dispatched: false,
    outcome: "not-dispatched",
    elapsedMs: 0,
  };
  let timer;
  let reader;
  let cancellation;
  const cancelBody = () => {
    // Retain and join the FIRST cancellation promise. A second cancel on a
    // closed stream is not evidence that underlying cancellation settled.
    if (reader && !cancellation) cancellation = reader.cancel().catch(() => {});
  };
  const check = () => {
    if (signal?.aborted) {
      receipt.reason = "cancelled";
      throw new Error();
    }
    if (combined.aborted || Date.now() >= deadline) {
      receipt.reason = remaining <= timeoutMs ? "deadline" : "timeout";
      throw new Error();
    }
  };
  try {
    if (remaining <= 0) stop.abort();
    else timer = setTimeout(() => stop.abort(), Math.min(timeoutMs, remaining));
    check();
    const body = JSON.stringify(value);
    receipt.dispatched = true;
    receipt.outcome = "unconfirmed";
    const response = await invoke(`${url}/${action}`, {
      method: "POST",
      redirect: "error",
      signal: combined,
      headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
      body,
    });
    receipt.status = response.status;
    receipt.contentType = contentType(response);
    const ray = response.headers.get("cf-ray");
    if (ray && /^[a-f0-9]{16}-[A-Z]{3}$/.test(ray)) receipt.ray = ray;
    reader = response.body?.getReader();
    combined.addEventListener("abort", cancelBody, { once: true });
    check();
    const chunks = [];
    let size = 0;
    receipt.reason = "body-read-error";
    if (!reader) throw new Error();
    for (;;) {
      const { done, value: chunk } = await reader.read();
      check();
      if (done) break;
      size += chunk.byteLength;
      if (size > controllerResponseMaxBytes) {
        receipt.reason = "body-too-large";
        throw new Error();
      }
      chunks.push(chunk);
    }
    const text = Buffer.concat(chunks).toString("utf8");
    receipt.bodyFormat = /^\s*(?:<!doctype\s+html\b|<html\b)/i.test(text) ? "html" : "other";
    let data;
    try {
      data = JSON.parse(text);
      receipt.bodyFormat = "json";
    } catch {
      receipt.reason = response.ok ? "invalid-json" : "http-error";
      throw new Error();
    }
    if (!response.ok) {
      receipt.reason = "http-error";
      throw new Error();
    }
    if (!data || typeof data !== "object" || Array.isArray(data)) {
      receipt.reason = "invalid-json-shape";
      throw new Error();
    }
    check();
    return data;
  } catch {
    // Fetch and body errors may contain URLs or reflected credentials. Preserve
    // only fixed reason codes and allowlisted response metadata.
    if (combined.aborted) {
      receipt.reason = signal?.aborted
        ? "cancelled"
        : remaining <= timeoutMs
          ? "deadline"
          : "timeout";
    }
    receipt.elapsedMs = Math.max(0, Math.round(performance.now() - elapsedStart));
    throw new ControllerHTTPError(receipt);
  } finally {
    clearTimeout(timer);
    combined.removeEventListener("abort", cancelBody);
    cancelBody();
    await cancellation;
    reader?.releaseLock();
  }
}

// Only canonical discovery is read-only. Never wrap a mutating controller action
// in this observation loop, even when its HTTP result is unconfirmed.
export async function observeControllerDiscovery(
  observe,
  { deadline = Infinity, signal, pause = delay } = {},
) {
  if (typeof observe !== "function" || !(Number.isFinite(deadline) || deadline === Infinity))
    throw new Error("invalid discovery observation bounds");
  const stopsAt = Math.min(Date.now() + 60_000, deadline);
  const stop = new AbortController();
  const combined = signal ? AbortSignal.any([signal, stop.signal]) : stop.signal;
  const check = () => {
    if (combined.aborted || Date.now() >= stopsAt)
      throw new Error("controller discovery cancelled or readiness deadline exhausted");
  };
  const timer = setTimeout(() => stop.abort(), Math.max(1, stopsAt - Date.now()));
  try {
    for (let attempt = 0; attempt < 12; attempt++) {
      check();
      try {
        const result = await observe({ deadline: stopsAt, signal: combined });
        check();
        return result;
      } catch (error) {
        const r = error instanceof ControllerHTTPError ? error.receipt : undefined;
        // HTML404 can precede workers.dev activation. It is never evidence of
        // an idle registry. Preserve the original error for all other outcomes.
        if (
          r?.action !== "discover" ||
          r.reason !== "http-error" ||
          r.status !== 404 ||
          r.contentType !== "html" ||
          r.bodyFormat !== "html" ||
          r.dispatched !== true ||
          r.outcome !== "unconfirmed" ||
          attempt === 11
        )
          throw error;
        check();
        await pause(Math.min(5_000, stopsAt - Date.now()), undefined, { signal: combined });
      }
    }
  } finally {
    clearTimeout(timer);
    stop.abort();
  }
}
