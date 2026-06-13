import type { IncomingMessage, Server } from "node:http";
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
