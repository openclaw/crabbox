import type { IncomingMessage, Server } from "node:http";
import { BlockList, isIP } from "node:net";
import type { Writable } from "node:stream";
import { finished } from "node:stream/promises";

import { coordinatorRequestQueue, type CoordinatorRequestQueue } from "../src/coordinator-runtime";

export const unauthenticatedRequestBodyBytes = 1024 * 1024;
export const authenticatedRequestBodyBytes = 16 * 1024 * 1024;
export const runFinishRequestBodyBytes = 64 * 1024 * 1024;

const noop = () => {};

export class RequestBodyTooLargeError extends Error {}

export class AsyncMutex {
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

  async drain(): Promise<void> {
    await this.tail;
  }
}

export class AsyncOperationTracker {
  private readonly active = new Set<Promise<unknown>>();

  async run<T>(callback: () => Promise<T>): Promise<T> {
    const operation = callback();
    this.active.add(operation);
    try {
      return await operation;
    } finally {
      this.active.delete(operation);
    }
  }

  async drain(): Promise<void> {
    const active = [...this.active];
    if (active.length === 0) return;
    await Promise.allSettled(active);
    return this.drain();
  }
}

export type FleetRequestQueue = CoordinatorRequestQueue;

export function requestBodyLimit(request: Request, authenticated: boolean): number {
  if (!authenticated) return unauthenticatedRequestBodyBytes;
  const url = new URL(request.url);
  const path = url.pathname.split("/").filter(Boolean);
  if (
    request.method.toUpperCase() === "POST" &&
    path.length === 4 &&
    path[0] === "v1" &&
    path[1] === "runs" &&
    path[2] &&
    path[3] === "finish"
  ) {
    return runFinishRequestBodyBytes;
  }
  return authenticatedRequestBodyBytes;
}

export function shouldReadUnauthenticatedRequestBody(method: string | undefined): boolean {
  const normalized = (method || "GET").toUpperCase();
  return normalized !== "GET" && normalized !== "HEAD";
}

export function isReadinessRequestMethod(method: string | undefined): boolean {
  const normalized = (method || "GET").toUpperCase();
  return normalized === "GET" || normalized === "HEAD";
}

export function isTrustedProxySource(
  address: string | undefined,
  configuredCIDRs: string | undefined,
): boolean {
  const normalizedAddress = normalizeIPAddress(address);
  const family = isIP(normalizedAddress);
  if (family === 0 || !configuredCIDRs?.trim()) return false;

  const blockList = new BlockList();
  try {
    for (const rawEntry of configuredCIDRs.split(",")) {
      const entry = rawEntry.trim();
      if (!entry) continue;
      const separator = entry.lastIndexOf("/");
      const subnet = normalizeIPAddress(separator === -1 ? entry : entry.slice(0, separator));
      const subnetFamily = isIP(subnet);
      if (subnetFamily === 0) return false;
      const type = subnetFamily === 4 ? "ipv4" : "ipv6";
      if (separator === -1) {
        blockList.addAddress(subnet, type);
        continue;
      }
      const prefix = Number(entry.slice(separator + 1));
      const maxPrefix = subnetFamily === 4 ? 32 : 128;
      if (!Number.isInteger(prefix) || prefix < 0 || prefix > maxPrefix) return false;
      blockList.addSubnet(subnet, prefix, type);
    }
  } catch {
    return false;
  }
  return blockList.check(normalizedAddress, family === 4 ? "ipv4" : "ipv6");
}

export function requestSourceIP(
  peerAddress: string | undefined,
  forwardedFor: string | undefined,
  configuredCIDRs: string | undefined,
): string | undefined {
  const peer = normalizeIPAddress(peerAddress);
  if (isIP(peer) === 0) return undefined;
  if (!isTrustedProxySource(peer, configuredCIDRs)) return peer;

  const chain = (forwardedFor ?? "")
    .split(",")
    .map((entry) => normalizeIPAddress(entry))
    .filter((entry) => isIP(entry) !== 0);
  for (let index = chain.length - 1; index >= 0; index -= 1) {
    const candidate = chain[index];
    if (candidate && !isTrustedProxySource(candidate, configuredCIDRs)) return candidate;
  }
  return peer;
}

function normalizeIPAddress(address: string | undefined): string {
  const value = address?.trim() ?? "";
  const mappedIPv4 = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/i.exec(value);
  return mappedIPv4?.[1] ?? value;
}

export async function readNodeRequestBody(
  request: IncomingMessage,
  limit: number,
): Promise<ArrayBuffer | undefined> {
  const rawLength = request.headers["content-length"];
  const declaredLength = Number(Array.isArray(rawLength) ? rawLength[0] : rawLength);
  if (Number.isFinite(declaredLength) && declaredLength > limit) {
    throw new RequestBodyTooLargeError();
  }
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of request) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    size += buffer.byteLength;
    if (size > limit) {
      throw new RequestBodyTooLargeError();
    }
    chunks.push(buffer);
  }
  return chunks.length > 0 ? Uint8Array.from(Buffer.concat(chunks)).buffer : undefined;
}

export async function writeNodeResponseBody(stream: Writable, body: Buffer): Promise<void> {
  const completion = finished(stream, { cleanup: true, readable: false });
  stream.end(body);
  await completion;
}

export function fleetRequestQueue(request: Request): FleetRequestQueue {
  return coordinatorRequestQueue(request);
}

export function closeServer(server: Server): Promise<void> {
  return new Promise((resolve, reject) => {
    server.close((error) => {
      if (error) {
        reject(error);
        return;
      }
      resolve();
    });
  });
}

export async function settlesWithin(
  operation: Promise<unknown>,
  timeoutMs: number,
): Promise<boolean> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      operation.then(() => true),
      new Promise<boolean>((resolve) => {
        timer = setTimeout(() => resolve(false), timeoutMs);
        timer.unref?.();
      }),
    ]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

export async function drainAndStop(
  activeRequests: Pick<AsyncOperationTracker, "drain">,
  lifecycleMutex: Pick<AsyncMutex, "drain">,
  stopRuntime: () => Promise<void>,
  serverClosed: Promise<void>,
): Promise<void> {
  await Promise.all([activeRequests.drain(), lifecycleMutex.drain()]);
  await stopRuntime();
  await serverClosed;
}
