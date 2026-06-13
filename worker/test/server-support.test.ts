import type { IncomingMessage } from "node:http";
import { Writable } from "node:stream";

import { describe, expect, it, vi } from "vitest";

import {
  AsyncMutex,
  AsyncOperationTracker,
  RequestBodyTooLargeError,
  drainAndStop,
  authenticatedRequestBodyBytes,
  fleetRequestQueue,
  isReadinessRequestMethod,
  readNodeRequestBody,
  requestBodyLimit,
  runFinishRequestBodyBytes,
  settlesWithin,
  shouldReadUnauthenticatedRequestBody,
  unauthenticatedRequestBodyBytes,
  writeNodeResponseBody,
} from "../node/server-support";

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

describe("Node server support", () => {
  it("keeps provider I/O, maintenance, and code proxy traffic off the lifecycle queue", () => {
    expect(
      fleetRequestQueue(new Request("https://coordinator.test/v1/leases", { method: "POST" })),
    ).toBe("direct");
    expect(
      fleetRequestQueue(
        new Request("https://coordinator.test/v1/internal/scheduled", { method: "POST" }),
      ),
    ).toBe("direct");
    expect(
      fleetRequestQueue(
        new Request("https://coordinator.test/portal/leases/cbx_1/code/assets/app.js"),
      ),
    ).toBe("direct");
    expect(
      fleetRequestQueue(
        new Request("https://coordinator.test/v1/leases/cbx_1/heartbeat", { method: "POST" }),
      ),
    ).toBe("direct");
    expect(
      fleetRequestQueue(
        new Request("https://coordinator.test/v1/leases/cbx_1/release", { method: "POST" }),
      ),
    ).toBe("direct");
    expect(
      fleetRequestQueue(new Request("https://coordinator.test/v1/images", { method: "POST" })),
    ).toBe("direct");
    expect(
      fleetRequestQueue(
        new Request("https://coordinator.test/v1/admin/aws-orphan-sweep", { method: "POST" }),
      ),
    ).toBe("direct");
  });

  it("waits for queued and active work to drain", async () => {
    const mutex = new AsyncMutex();
    const tracker = new AsyncOperationTracker();
    const done = deferred<void>();
    const operation = tracker.run(() => mutex.run(() => done.promise));
    let drained = false;
    const drain = (async () => {
      await Promise.all([tracker.drain(), mutex.drain()]);
      drained = true;
    })();

    await Promise.resolve();
    expect(drained).toBe(false);
    done.resolve();
    await operation;
    await drain;
    expect(drained).toBe(true);
  });

  it("allows the worst-case encoded retained run log", () => {
    const retainedLogBytes = 8 * 1024 * 1024;
    const fallbackPreviewBytes = 64 * 1024;
    const worstCaseJSONBytes = (retainedLogBytes + fallbackPreviewBytes) * 6 + 4096;
    const request = new Request("https://coordinator.test/v1/runs/run_1/finish", {
      method: "POST",
    });
    expect(requestBodyLimit(request, true)).toBe(runFinishRequestBodyBytes);
    expect(
      requestBodyLimit(
        new Request("https://coordinator.test//v1/runs/run_1//finish/", { method: "POST" }),
        true,
      ),
    ).toBe(runFinishRequestBodyBytes);
    expect(runFinishRequestBodyBytes).toBeGreaterThan(worstCaseJSONBytes);
  });

  it("keeps unauthenticated and ordinary authenticated body limits smaller", () => {
    const request = new Request("https://coordinator.test/v1/runs/run_1/finish", {
      method: "POST",
    });
    expect(requestBodyLimit(request, false)).toBe(unauthenticatedRequestBodyBytes);
    expect(
      requestBodyLimit(new Request("https://coordinator.test/v1/leases", { method: "POST" }), true),
    ).toBe(authenticatedRequestBodyBytes);
  });

  it("caps bodies drained before authentication completes", async () => {
    const request = {
      headers: {},
      async *[Symbol.asyncIterator]() {
        yield Buffer.alloc(unauthenticatedRequestBodyBytes);
        yield Buffer.from("overflow");
      },
    } as unknown as IncomingMessage;

    await expect(
      readNodeRequestBody(request, unauthenticatedRequestBodyBytes),
    ).rejects.toBeInstanceOf(RequestBodyTooLargeError);
  });

  it("does not wait for unauthenticated GET or HEAD bodies", () => {
    expect(shouldReadUnauthenticatedRequestBody("GET")).toBe(false);
    expect(shouldReadUnauthenticatedRequestBody("HEAD")).toBe(false);
    expect(shouldReadUnauthenticatedRequestBody("get")).toBe(false);
    expect(shouldReadUnauthenticatedRequestBody("POST")).toBe(true);
  });

  it("only serves readiness over GET or HEAD", () => {
    expect(isReadinessRequestMethod("GET")).toBe(true);
    expect(isReadinessRequestMethod("HEAD")).toBe(true);
    expect(isReadinessRequestMethod("POST")).toBe(false);
  });

  it("rejects declared oversized bodies without reading their stream", async () => {
    const iterator = vi.fn<() => AsyncIterator<unknown>>();
    const request = {
      headers: { "content-length": String(unauthenticatedRequestBodyBytes + 1) },
      [Symbol.asyncIterator]: iterator,
    } as unknown as IncomingMessage;

    await expect(
      readNodeRequestBody(request, unauthenticatedRequestBodyBytes),
    ).rejects.toBeInstanceOf(RequestBodyTooLargeError);
    expect(iterator).not.toHaveBeenCalled();
  });

  it("settles response writes when the client disconnects", async () => {
    const response = new Writable({
      write() {
        this.destroy();
      },
    });

    await expect(writeNodeResponseBody(response, Buffer.from("payload"))).rejects.toThrow(
      "Premature close",
    );
  });

  it("bounds shutdown waits", async () => {
    expect(await settlesWithin(Promise.resolve(), 100)).toBe(true);
    expect(await settlesWithin(new Promise(() => {}), 1)).toBe(false);
  });

  it("drains HTTP work before stopping sockets and awaiting server closure", async () => {
    const order: string[] = [];
    let finishRequests!: () => void;
    let finishServerClose!: () => void;
    const requestsDrained = new Promise<void>((resolve) => {
      finishRequests = resolve;
    });
    const serverClosed = new Promise<void>((resolve) => {
      finishServerClose = resolve;
    });
    const shutdown = drainAndStop(
      { drain: async () => requestsDrained },
      { drain: async () => {} },
      async () => {
        order.push("sockets:closed");
      },
      serverClosed.then(() => {
        order.push("server:closed");
        return undefined;
      }),
    );

    await Promise.resolve();
    expect(order).toEqual([]);
    finishRequests();
    await vi.waitFor(() => expect(order).toEqual(["sockets:closed"]));
    finishServerClose();
    await shutdown;
    expect(order).toEqual(["sockets:closed", "server:closed"]);
  });
});
