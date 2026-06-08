import { readFile } from "node:fs/promises";
import { createServer, STATUS_CODES, type IncomingMessage, type ServerResponse } from "node:http";
import { extname, resolve, sep } from "node:path";
import type { Duplex } from "node:stream";
import { fileURLToPath } from "node:url";

import { routeCoordinatorRequest } from "../src/coordinator-entry";
import { FleetCoordinator } from "../src/fleet";
import type { Env } from "../src/types";
import { NodeCoordinatorRuntime, type NodeUpgradeContext } from "./node-runtime";

const noop = () => {};
const maxRequestBodyBytes = 16 * 1024 * 1024;

class RequestBodyTooLargeError extends Error {}

class AsyncMutex {
  private tail: Promise<void> = Promise.resolve();

  async run<T>(callback: () => Promise<T>): Promise<T> {
    const previous = this.tail;
    let release = noop;
    this.tail = new Promise<void>((resolvePromise) => {
      release = resolvePromise;
    });
    await previous;
    try {
      return await callback();
    } finally {
      release();
    }
  }
}

const databaseURL = requiredEnv("DATABASE_URL");
const port = positiveInt(process.env["PORT"], 8080);
const env = process.env as unknown as Env;
const runtime = new NodeCoordinatorRuntime(databaseURL);
const coordinator = new FleetCoordinator(runtime, env);
const mutationMutex = new AsyncMutex();
const publicDirectory = resolve(fileURLToPath(new URL("../public", import.meta.url)));

runtime.setOperationRunner((callback) => mutationMutex.run(callback));
await runtime.start(() => mutationMutex.run(() => coordinator.alarm()));

const server = createServer(async (request, response) => {
  try {
    const webRequest = await webRequestFromNode(request);
    if (new URL(webRequest.url).pathname === "/v1/ready") {
      await runtime.storage.ready();
      await writeResponse(response, Response.json({ ok: true }));
      return;
    }
    const asset = await staticAsset(webRequest);
    if (asset) {
      await writeResponse(response, asset);
      return;
    }
    const result = await routeCoordinatorRequest(webRequest, env, runFleetRequest);
    await writeResponse(response, result);
  } catch (error) {
    if (error instanceof RequestBodyTooLargeError) {
      await writeResponse(response, Response.json({ error: "request_too_large" }, { status: 413 }));
      return;
    }
    console.error("coordinator request failed", error);
    await writeResponse(response, Response.json({ error: "internal_error" }, { status: 500 }));
  }
});

server.on("upgrade", async (request, socket, head) => {
  const context: NodeUpgradeContext = { request, socket, head, upgraded: false };
  try {
    const webRequest = await webRequestFromNode(request);
    const response = await runtime.runWithUpgrade(context, () =>
      routeCoordinatorRequest(webRequest, env, runFleetRequest),
    );
    if (!context.upgraded) {
      await writeUpgradeResponse(socket, response);
    }
  } catch (error) {
    console.error("coordinator websocket upgrade failed", error);
    if (!context.upgraded) {
      await writeUpgradeResponse(
        socket,
        Response.json({ error: "internal_error" }, { status: 500 }),
      );
    }
  }
});

server.listen(port, () => {
  console.log(`crabbox coordinator listening on ${port}`);
});

process.on("SIGTERM", () => {
  void shutdown();
});
process.on("SIGINT", () => {
  void shutdown();
});

async function runFleetRequest(request: Request): Promise<Response> {
  return mutationMutex.run(() => coordinator.fetch(request));
}

async function shutdown(): Promise<void> {
  server.close();
  await runtime.stop();
  process.exit(0);
}

async function webRequestFromNode(request: IncomingMessage): Promise<Request> {
  const protocol = firstHeader(request.headers["x-forwarded-proto"]) || "http";
  const host =
    firstHeader(request.headers["x-forwarded-host"]) || request.headers.host || "localhost";
  const url = `${protocol}://${host}${request.url || "/"}`;
  const headers = new Headers();
  for (const [name, value] of Object.entries(request.headers)) {
    if (Array.isArray(value)) {
      for (const item of value) headers.append(name, item);
    } else if (value !== undefined) {
      headers.set(name, value);
    }
  }
  const method = request.method || "GET";
  const body = method === "GET" || method === "HEAD" ? undefined : await readRequestBody(request);
  const init: RequestInit = { method, headers };
  if (body !== undefined) {
    init.body = body;
  }
  return new Request(url, init);
}

async function readRequestBody(request: IncomingMessage): Promise<ArrayBuffer | undefined> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of request) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    size += buffer.byteLength;
    if (size > maxRequestBodyBytes) {
      throw new RequestBodyTooLargeError();
    }
    chunks.push(buffer);
  }
  return chunks.length > 0 ? Uint8Array.from(Buffer.concat(chunks)).buffer : undefined;
}

async function writeResponse(response: ServerResponse, result: Response): Promise<void> {
  response.statusCode = result.status;
  result.headers.forEach((value, name) => response.setHeader(name, value));
  const body = Buffer.from(await result.arrayBuffer());
  if (!response.hasHeader("content-length")) {
    response.setHeader("content-length", body.byteLength);
  }
  response.end(body);
}

async function writeUpgradeResponse(socket: Duplex, response: Response): Promise<void> {
  const body = Buffer.from(await response.arrayBuffer());
  const headers = new Headers(response.headers);
  headers.set("connection", "close");
  headers.set("content-length", String(body.byteLength));
  const statusText = STATUS_CODES[response.status] || "Error";
  const lines = [`HTTP/1.1 ${response.status} ${statusText}`];
  headers.forEach((value, name) => lines.push(`${name}: ${value}`));
  socket.end(`${lines.join("\r\n")}\r\n\r\n${body.toString()}`);
}

async function staticAsset(request: Request): Promise<Response | undefined> {
  if (request.method !== "GET" && request.method !== "HEAD") {
    return undefined;
  }
  const pathname = decodeURIComponent(new URL(request.url).pathname);
  if (!pathname.startsWith("/portal/assets/")) {
    return undefined;
  }
  const path = resolve(publicDirectory, `.${pathname}`);
  if (!path.startsWith(`${publicDirectory}${sep}`)) {
    return Response.json({ error: "not_found" }, { status: 404 });
  }
  try {
    const body = await readFile(path);
    return new Response(request.method === "HEAD" ? null : body, {
      headers: {
        "cache-control": "public, max-age=3600",
        "content-type": contentType(path),
      },
    });
  } catch {
    return Response.json({ error: "not_found" }, { status: 404 });
  }
}

function contentType(path: string): string {
  switch (extname(path)) {
    case ".css":
      return "text/css; charset=utf-8";
    case ".html":
      return "text/html; charset=utf-8";
    case ".js":
      return "text/javascript; charset=utf-8";
    case ".json":
      return "application/json; charset=utf-8";
    case ".svg":
      return "image/svg+xml";
    default:
      return "application/octet-stream";
  }
}

function firstHeader(value: string | string[] | undefined): string {
  return Array.isArray(value) ? (value[0] ?? "") : ((value ?? "").split(",")[0]?.trim() ?? "");
}

function requiredEnv(name: string): string {
  const value = process.env[name];
  if (!value) {
    throw new Error(`${name} is required`);
  }
  return value;
}

function positiveInt(value: string | undefined, fallback: number): number {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : fallback;
}
