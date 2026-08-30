#!/usr/bin/env node
// Explicit operator login only; the Go provider never invokes this helper.
import https from "node:https";
import { open, lstat, unlink } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { setTimeout as delay } from "node:timers/promises";

const hardLimitMS = 10 * 60 * 1000;
const responseLimit = 64 * 1024;

export function consoleOrigin(raw = "https://app.boxd.sh") {
  let url;
  try { url = new URL(raw); } catch { throw new Error("Boxd login requires an HTTPS console origin"); }
  if (!/^https:\/\/[^/?#\\@\s]+\/?$/.test(raw) || url.protocol !== "https:" || !url.hostname || url.username || url.password || url.pathname !== "/") {
    throw new Error("Boxd login requires an HTTPS origin without credentials, path, query or fragment");
  }
  return url.origin;
}

export function loginURL(raw, origin, session) {
  let url;
  try { url = new URL(raw); } catch { throw new Error("Invalid Boxd device login URL"); }
  if (url.origin !== origin || url.protocol !== "https:" || url.username || url.password ||
      url.pathname !== "/link" || url.hash || url.searchParams.getAll("session").length !== 1 ||
      url.searchParams.get("session") !== session || [...url.searchParams.keys()].some((key) => key !== "session")) {
    throw new Error("Boxd device login URL did not match the requested console and session");
  }
  return url.href;
}

// https.request never follows redirects. Neither response bodies nor transport
// errors (which may include the session URL) become operator diagnostics.
export function requestJSON(url, { body, token, signal, agent } = {}) {
  return new Promise((resolveRequest, reject) => {
    if (url.protocol !== "https:") { reject(new Error("Boxd login requires HTTPS")); return; }
    const data = body === undefined ? undefined : JSON.stringify(body);
    const headers = { Accept: "application/json" };
    if (data !== undefined) { headers["Content-Type"] = "application/json"; headers["Content-Length"] = Buffer.byteLength(data); }
    if (token) headers.Authorization = `Bearer ${token}`;
    const request = https.request(url, { method: data === undefined ? "GET" : "POST", headers, signal, agent }, (response) => {
      if (response.statusCode < 200 || response.statusCode >= 300) {
        response.destroy(); reject(new Error("Boxd HTTPS login request was rejected; redirects are not allowed")); return;
      }
      let size = 0;
      const chunks = [];
      response.on("data", (chunk) => {
        size += chunk.length;
        if (size > responseLimit) { response.destroy(); reject(new Error("Boxd login response exceeded its size limit")); return; }
        chunks.push(chunk);
      });
      response.on("error", () => reject(new Error("Boxd HTTPS login response failed")));
      response.on("end", () => {
        try { resolveRequest(JSON.parse(Buffer.concat(chunks).toString("utf8"))); }
        catch { reject(new Error("Invalid Boxd HTTPS login response")); }
      });
    });
    const timeout = setTimeout(() => request.destroy(), 15000);
    request.on("close", () => clearTimeout(timeout));
    request.on("error", () => reject(new Error("Boxd HTTPS login request failed or was canceled")));
    request.end(data);
  });
}

export async function deviceLogin({ output, apiURL, signal, prompt, agent }) {
  const origin = consoleOrigin(apiURL);
  if (!output || typeof prompt !== "function") throw new Error("An explicit private output file and operator prompt are required");
  // Exclusive creation refuses existing files and symlinks before any auth request.
  let file;
  try { file = await open(output, "wx", 0o600); }
  catch { throw new Error("Cannot create private output file; choose a new file in an existing directory"); }
  const identity = await file.stat();
  let saved = false;
  const sameFile = async () => {
    const current = await lstat(output).catch(() => null);
    return current?.isFile() && current.dev === identity.dev && current.ino === identity.ino;
  };
  try {
    await file.chmod(0o600);
    if (((await file.stat()).mode & 0o777) !== 0o600) throw new Error("Boxd login requires a filesystem that enforces mode 0600");
    const hardSignal = AbortSignal.timeout(hardLimitMS);
    const initialSignal = signal ? AbortSignal.any([signal, hardSignal]) : hardSignal;
    const started = Date.now();
    const device = await requestJSON(new URL("/api/v1/auth/device", origin), { body: {}, signal: initialSignal, agent });
    if (typeof device?.session !== "string" || !device.session || device.session.length > 4096 ||
        !Number.isFinite(device.expires_in) || device.expires_in <= 0) throw new Error("Invalid Boxd device session response");
    const remaining = Math.floor(Math.min(device.expires_in * 1000, hardLimitMS) - (Date.now() - started));
    if (remaining <= 0) throw new Error("Boxd device session expired; run login again");
    const pollSignal = AbortSignal.any([initialSignal, AbortSignal.timeout(remaining)]);
    // The URL is a session secret: show it only in the explicit private prompt.
    prompt(loginURL(device.url, origin, device.session));
    const pollURL = new URL("/api/v1/auth/device/poll", origin);
    pollURL.searchParams.set("session", device.session);
    while (!pollSignal.aborted) {
      const result = await requestJSON(pollURL, { signal: pollSignal, agent });
      if (result?.status === "pending" && (result.token == null || result.token === "")) {
        await delay(1000, undefined, { signal: pollSignal });
        continue;
      }
      if (result?.token !== undefined) {
        if (typeof result.status !== "string" || !result.status || typeof result.token !== "string" || !/^[\x21-\x7e]{1,16384}$/.test(result.token) || result.token.startsWith("bxd_") ||
            !result.user_id || ["denied", "expired", "pending"].includes(result.status)) throw new Error("Invalid Boxd interactive session response");
        const identity = await requestJSON(new URL("/api/v1/whoami", origin), { token: result.token, signal: pollSignal, agent });
        if (typeof identity?.user_id !== "string" || identity.user_id !== result.user_id) throw new Error("Boxd authenticated account did not match the device session");
        if (pollSignal.aborted) throw new Error("Boxd device session expired or was canceled");
        if (!await sameFile()) throw new Error("Boxd output file was replaced during login");
        await file.writeFile(result.token + "\n", "utf8");
        await file.sync();
        if (!await sameFile()) throw new Error("Boxd output file was replaced during login");
        saved = true;
        return;
      }
      throw new Error("Boxd device session was not authorized; run login again");
    }
    throw new Error("Boxd device session expired or was canceled");
  } finally {
    await file.close();
    if (!saved && await sameFile()) await unlink(output);
  }
}

export async function main(args = process.argv.slice(2)) {
  if (args.length === 1 && args[0] === "--help") {
    process.stdout.write("Usage: node scripts/boxd-login.mjs --output <new-private-file> [--api-url <https-origin>]\nRequires a private interactive terminal; writes only the token to a new mode-0600 file.\n");
    return;
  }
  const options = {};
  for (let i = 0; i < args.length; i += 2) {
    if (!["--output", "--api-url"].includes(args[i]) || !args[i + 1] || options[args[i]]) throw new Error("Invalid Boxd login arguments; use --help");
    options[args[i]] = args[i + 1];
  }
  if (!process.stdin.isTTY || !process.stderr.isTTY) throw new Error("Run Boxd login in a private interactive terminal; the approval URL is a session secret");
  const controller = new AbortController();
  const cancel = () => controller.abort();
  process.once("SIGINT", cancel); process.once("SIGTERM", cancel);
  try {
    await deviceLogin({ output: options["--output"], apiURL: options["--api-url"] ?? process.env.CRABBOX_BOXD_API_URL ?? undefined, signal: controller.signal,
      prompt: (url) => process.stderr.write(`Open this private URL in your signed-in console browser (do not share or log it):\n${url}\n`) });
    process.stderr.write("Boxd interactive session saved to the requested private file. No token was printed.\n");
  } finally {
    process.removeListener("SIGINT", cancel); process.removeListener("SIGTERM", cancel);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main().catch(() => {
    // No arbitrary error objects: an injected filesystem or transport error can
    // contain secrets. --help documents usage without starting authentication.
    process.stderr.write("Boxd login failed. Use --help, a private terminal, a valid HTTPS origin and a new output file; approve the device before it expires.\n");
    process.exitCode = 1;
  });
}
