import { readFile } from "node:fs/promises";
import { createServer, STATUS_CODES, type IncomingMessage, type ServerResponse } from "node:http";
import { extname, resolve, sep } from "node:path";
import type { Duplex } from "node:stream";
import { fileURLToPath, URL as NodeURL } from "node:url";

import { prepareCoordinatorRequest, routeCoordinatorRequest } from "../src/coordinator-entry";
import { FleetCoordinator } from "../src/fleet";
import {
  createAWSDeploymentGuard,
  nodeCoordinatorEnv,
  requiresAWSDeploymentReadiness,
} from "./aws-deployment";
import { NodeCoordinatorRuntime, type NodeUpgradeContext } from "./node-runtime";
import {
  AsyncMutex,
  AsyncOperationTracker,
  RequestBodyTooLargeError,
  closeServer,
  drainAndStop,
  fleetRequestQueue,
  isReadinessRequestMethod,
  isTrustedProxySource,
  nodeResponseHeaders,
  nodeRequestAbortSignal,
  readNodeRequestBody,
  requestSourceIP,
  requestBodyLimit,
  settlesWithin,
  shouldReadUnauthenticatedRequestBody,
  unauthenticatedRequestBodyBytes,
  writeNodeResponseBody,
} from "./server-support";

const databaseURL = requiredEnv("DATABASE_URL");
const port = positiveInt(process.env["PORT"], 8080);
const shutdownTimeoutMs = positiveInt(process.env["CRABBOX_SHUTDOWN_TIMEOUT_MS"], 120_000);
const env = nodeCoordinatorEnv(process.env);
const awsDeployment = createAWSDeploymentGuard(env);
const runtime = new NodeCoordinatorRuntime(databaseURL);
const coordinator = new FleetCoordinator(runtime, env);
const lifecycleMutex = new AsyncMutex();
const activeRequests = new AsyncOperationTracker();
const publicDirectory = resolve(fileURLToPath(new NodeURL("../public", import.meta.url)));
let shutdownPromise: Promise<void> | undefined;

runtime.setOperationRunner((callback) => lifecycleMutex.run(callback));
await awsDeployment.start();
await runtime.start(() => coordinator.alarm());

const server = createServer((request, response) => {
  void activeRequests
    .run(() => handleRequest(request, response))
    .catch((error) => {
      console.error("coordinator request handler failed", error);
      response.destroy();
    });
});

async function handleRequest(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const cancellation = nodeRequestAbortSignal(request, response);
  try {
    const requestContext = nodeRequestContext(request);
    const requestMetadata = webRequestFromNode(request, requestContext, cancellation.signal);
    const authContext = { trustedProxy: requestContext.trustedProxy };
    if (new URL(requestMetadata.url).pathname === "/v1/ready") {
      let result: Response;
      if (isReadinessRequestMethod(requestMetadata.method)) {
        try {
          await Promise.all([runtime.storage.ready(), awsDeployment.ready()]);
          result =
            requestMetadata.method === "HEAD"
              ? new Response(null, { status: 200 })
              : Response.json({ ok: true });
        } catch {
          result =
            requestMetadata.method === "HEAD"
              ? new Response(null, { status: 503 })
              : Response.json({ ok: false, error: "dependency_unavailable" }, { status: 503 });
        }
      } else {
        result = Response.json(
          { error: "method_not_allowed" },
          { status: 405, headers: { allow: "GET, HEAD" } },
        );
      }
      await writeResponse(response, result, true);
      request.destroy();
      return;
    }
    const asset = await staticAsset(requestMetadata);
    if (asset) {
      await writeResponse(response, asset);
      return;
    }
    const prepared = await prepareCoordinatorRequest(requestMetadata, env, authContext);
    if ("response" in prepared) {
      const readBody = shouldReadUnauthenticatedRequestBody(request.method);
      if (readBody) {
        await readNodeRequestBody(request, unauthenticatedRequestBodyBytes);
      }
      await writeResponse(response, prepared.response, !readBody);
      if (!readBody) {
        request.destroy();
      }
      return;
    }
    if (requiresAWSDeploymentReadiness(prepared.request)) {
      try {
        await awsDeployment.ready();
      } catch {
        await writeResponse(
          response,
          Response.json({ error: "dependency_unavailable" }, { status: 503 }),
          true,
        );
        request.destroy();
        return;
      }
    }
    const webRequest = await requestWithNodeBody(
      prepared.request,
      request,
      prepared.bodyLimit ?? requestBodyLimit(prepared.request, prepared.authenticated),
    );
    const result = await runFleetRequest(webRequest);
    await writeResponse(response, result);
  } catch (error) {
    if (error instanceof RequestBodyTooLargeError) {
      response.setHeader("connection", "close");
      await writeResponse(response, Response.json({ error: "request_too_large" }, { status: 413 }));
      request.destroy();
      return;
    }
    console.error("coordinator request failed", error);
    await writeResponse(response, Response.json({ error: "internal_error" }, { status: 500 }));
  } finally {
    cancellation.dispose();
  }
}

server.on("upgrade", (request, socket, head) => {
  void activeRequests
    .run(() => handleUpgrade(request, socket, head))
    .catch((error) => {
      console.error("coordinator websocket upgrade handler failed", error);
      socket.destroy();
    });
});

async function handleUpgrade(
  request: IncomingMessage,
  socket: Duplex,
  head: Buffer,
): Promise<void> {
  const context: NodeUpgradeContext = { request, socket, head, upgraded: false };
  try {
    const requestContext = nodeRequestContext(request);
    const webRequest = webRequestFromNode(request, requestContext);
    const authContext = { trustedProxy: requestContext.trustedProxy };
    const response = await runtime.runWithUpgrade(context, () =>
      routeCoordinatorRequest(webRequest, env, runFleetRequest, authContext),
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
}

server.listen(port, () => {
  console.log(`crabbox coordinator listening on ${port}`);
});

process.on("SIGTERM", () => {
  void shutdown().catch(shutdownFailed);
});
process.on("SIGINT", () => {
  void shutdown().catch(shutdownFailed);
});

async function runFleetRequest(request: Request): Promise<Response> {
  switch (fleetRequestQueue(request)) {
    case "direct":
      return coordinator.fetch(request);
    case "lifecycle":
      return lifecycleMutex.run(() => coordinator.fetch(request));
  }
}

async function shutdown(): Promise<void> {
  if (shutdownPromise) {
    return shutdownPromise;
  }
  shutdownPromise = orderlyShutdown();
  return shutdownPromise;
}

async function orderlyShutdown(): Promise<void> {
  runtime.beginShutdown();
  const serverClosed = closeServer(server);
  server.closeIdleConnections?.();
  const drained = await settlesWithin(
    drainAndStop(activeRequests, lifecycleMutex, () => runtime.stop(), serverClosed),
    shutdownTimeoutMs,
  );
  if (!drained) {
    console.error(`coordinator shutdown exceeded ${shutdownTimeoutMs}ms`);
    process.exit(1);
  }
  process.exit(0);
}

function shutdownFailed(error: unknown): void {
  console.error("coordinator shutdown failed", error);
  process.exit(1);
}

interface NodeRequestContext {
  sourceIP: string | undefined;
  trustedProxy: boolean;
}

function nodeRequestContext(request: IncomingMessage): NodeRequestContext {
  const configuredCIDRs = env.CRABBOX_TRUSTED_PROXY_CIDRS;
  const peerAddress = request.socket.remoteAddress;
  return {
    sourceIP: requestSourceIP(
      peerAddress,
      joinedHeader(request.headers["x-forwarded-for"]),
      configuredCIDRs,
    ),
    trustedProxy: isTrustedProxySource(peerAddress, configuredCIDRs),
  };
}

function webRequestFromNode(
  request: IncomingMessage,
  context: NodeRequestContext,
  signal?: AbortSignal,
): Request {
  const protocol = context.trustedProxy
    ? firstHeader(request.headers["x-forwarded-proto"]) || "http"
    : "http";
  const forwardedHost = context.trustedProxy
    ? firstHeader(request.headers["x-forwarded-host"])
    : "";
  const host = forwardedHost || request.headers.host || "localhost";
  const url = `${protocol}://${host}${request.url || "/"}`;
  const headers = new Headers();
  for (const [name, value] of Object.entries(request.headers)) {
    if (Array.isArray(value)) {
      for (const item of value) headers.append(name, item);
    } else if (value !== undefined) {
      headers.set(name, value);
    }
  }
  headers.delete("cf-connecting-ip");
  if (context.sourceIP) headers.set("cf-connecting-ip", context.sourceIP);
  const method = request.method || "GET";
  return new Request(url, { method, headers, ...(signal ? { signal } : {}) });
}

async function requestWithNodeBody(
  request: Request,
  nodeRequest: IncomingMessage,
  limit: number,
): Promise<Request> {
  if (request.method === "GET" || request.method === "HEAD") return request;
  const body = await readNodeRequestBody(nodeRequest, limit);
  // oxlint-disable-next-line unicorn/no-invalid-fetch-options -- GET and HEAD return above.
  return body === undefined ? request : new Request(request, { body });
}

async function writeResponse(
  response: ServerResponse,
  result: Response,
  closeConnection = false,
): Promise<void> {
  response.statusCode = result.status;
  for (const [name, value] of nodeResponseHeaders(result.headers)) {
    response.setHeader(name, value);
  }
  if (closeConnection) {
    response.setHeader("connection", "close");
  }
  const body = Buffer.from(await result.arrayBuffer());
  if (!response.hasHeader("content-length")) {
    response.setHeader("content-length", body.byteLength);
  }
  await writeNodeResponseBody(response, body);
}

async function writeUpgradeResponse(socket: Duplex, response: Response): Promise<void> {
  const body = Buffer.from(await response.arrayBuffer());
  const headers = new Headers(response.headers);
  headers.set("connection", "close");
  headers.set("content-length", String(body.byteLength));
  const statusText = STATUS_CODES[response.status] || "Error";
  const lines = [`HTTP/1.1 ${response.status} ${statusText}`];
  for (const [name, value] of nodeResponseHeaders(headers)) {
    for (const item of Array.isArray(value) ? value : [value]) {
      lines.push(`${name}: ${item}`);
    }
  }
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

function joinedHeader(value: string | string[] | undefined): string {
  return Array.isArray(value) ? value.join(",") : (value ?? "");
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
