import { EventEmitter } from "node:events";

import { beforeEach, describe, expect, it, vi } from "vitest";

type OperationRunner = <T>(callback: () => Promise<T>) => Promise<T>;

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

const mocks = vi.hoisted(() => {
  const boss = {
    on: vi.fn<(...args: unknown[]) => unknown>(),
    start: vi.fn<() => Promise<void>>(async () => {}),
    stop: vi.fn<(...args: unknown[]) => Promise<void>>(async () => {}),
    createQueue: vi.fn<(...args: unknown[]) => Promise<void>>(async () => {}),
    work: vi.fn<(...args: unknown[]) => Promise<string>>(async () => "worker-id"),
    schedule: vi.fn<(...args: unknown[]) => Promise<void>>(async () => {}),
    send: vi.fn<(...args: unknown[]) => Promise<string>>(async () => "job-id"),
    deleteQueuedJobs: vi.fn<(...args: unknown[]) => Promise<void>>(async () => {}),
  };
  const storage = {
    initialize: vi.fn<() => Promise<void>>(async () => {}),
    close: vi.fn<() => Promise<void>>(async () => {}),
  };
  return { boss, storage };
});

vi.mock("pg-boss", () => ({
  PgBoss: function PgBoss() {
    return mocks.boss;
  },
}));

vi.mock("../node/postgres-storage", () => ({
  PostgresCoordinatorStorage: function PostgresCoordinatorStorage() {
    return mocks.storage;
  },
}));

import { NodeCoordinatorRuntime } from "../node/node-runtime";

describe("NodeCoordinatorRuntime", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("allows an active alarm to enqueue one successor", async () => {
    const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");

    await runtime.start(async () => {});

    expect(mocks.boss.createQueue).toHaveBeenCalledWith(
      "coordinator-alarm",
      expect.objectContaining({ policy: "short" }),
    );
  });

  it("contains WebSocket message handler failures to the offending socket", async () => {
    const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
    const socket = Object.assign(new EventEmitter(), {
      close: vi.fn<(code?: number, reason?: string) => void>(),
      terminate: vi.fn<() => void>(),
    });
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});

    runtime.acceptWebSocket(socket as unknown as WebSocket, {}, [], {
      message: async () => {
        throw new Error("invalid peer frame");
      },
      close: vi.fn<(code: number, reason: string) => void>(),
      error: vi.fn<() => void>(),
    });
    socket.emit("message", Buffer.from("bad"), false);

    await vi.waitFor(() => {
      expect(socket.close).toHaveBeenCalledWith(1011, "coordinator handler failed");
    });
    expect(errorLog).toHaveBeenCalledWith(
      "coordinator websocket message handler failed",
      expect.any(Error),
    );
    errorLog.mockRestore();
  });

  it("lets code-agent replies bypass the lifecycle queue", async () => {
    const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
    const socket = Object.assign(new EventEmitter(), {
      close: vi.fn<(code?: number, reason?: string) => void>(),
      terminate: vi.fn<() => void>(),
    });
    const operationRunner = vi.fn<OperationRunner>(
      async <T>(_callback: () => Promise<T>): Promise<T> => await new Promise<T>(() => {}),
    );
    const message = vi.fn<() => Promise<void>>(async () => {});
    runtime.setOperationRunner(operationRunner);

    runtime.acceptWebSocket(socket as unknown as WebSocket, { kind: "code-agent" }, [], {
      message,
      close: vi.fn<(code: number, reason: string) => void>(),
      error: vi.fn<() => void>(),
    });
    socket.emit("message", Buffer.from("{}"), false);

    await vi.waitFor(() => {
      expect(message).toHaveBeenCalledOnce();
    });
    expect(operationRunner).not.toHaveBeenCalled();
  });

  it("serializes control messages with lifecycle operations", async () => {
    const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
    const socket = Object.assign(new EventEmitter(), {
      close: vi.fn<(code?: number, reason?: string) => void>(),
      terminate: vi.fn<() => void>(),
    });
    const operationRunner = vi.fn<OperationRunner>(
      async <T>(callback: () => Promise<T>): Promise<T> => callback(),
    );
    const message = vi.fn<() => Promise<void>>(async () => {});
    runtime.setOperationRunner(operationRunner);

    runtime.acceptWebSocket(socket as unknown as WebSocket, { kind: "control" }, [], {
      message,
      close: vi.fn<(code: number, reason: string) => void>(),
      error: vi.fn<() => void>(),
    });
    socket.emit("message", Buffer.from("{}"), false);

    await vi.waitFor(() => {
      expect(message).toHaveBeenCalledOnce();
    });
    expect(operationRunner).toHaveBeenCalledOnce();
  });

  it("keeps data-plane close callbacks behind earlier messages", async () => {
    const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
    const socket = Object.assign(new EventEmitter(), {
      close: vi.fn<(code?: number, reason?: string) => void>(),
      terminate: vi.fn<() => void>(),
    });
    const order: string[] = [];
    const messageDone = deferred<void>();

    runtime.acceptWebSocket(socket as unknown as WebSocket, { kind: "code-agent" }, [], {
      message: async () => {
        order.push("message");
        await messageDone.promise;
      },
      close: () => {
        order.push("close");
      },
      error: vi.fn<() => void>(),
    });
    socket.emit("message", Buffer.from("{}"), false);
    socket.emit("close", 1000, Buffer.from("done"));

    await vi.waitFor(() => {
      expect(order).toEqual(["message"]);
    });
    messageDone.resolve();
    await vi.waitFor(() => {
      expect(order).toEqual(["message", "close"]);
    });
  });

  it("drains socket operations before stopping jobs and closing storage", async () => {
    const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
    const messageDone = deferred<void>();
    const socket = new EventEmitter();
    const close = vi.fn<() => void>(() => {
      queueMicrotask(() => socket.emit("close", 1000, Buffer.from("shutdown")));
    });
    Object.assign(socket, {
      close,
      terminate: vi.fn<() => void>(),
      readyState: 1,
    });
    const message = vi.fn<() => Promise<void>>(async () => {
      await messageDone.promise;
      await runtime.scheduleAlarm(Date.now() + 60_000);
    });

    runtime.acceptWebSocket(socket as unknown as WebSocket, { kind: "code-agent" }, [], {
      message,
      close: vi.fn<(code: number, reason: string) => void>(),
      error: vi.fn<() => void>(),
    });
    socket.emit("message", Buffer.from("{}"), false);
    await vi.waitFor(() => expect(message).toHaveBeenCalledOnce());

    runtime.beginShutdown();
    expect(close).not.toHaveBeenCalled();
    const stopped = runtime.stop();
    await vi.waitFor(() => expect(close).toHaveBeenCalledOnce());
    expect(mocks.boss.stop).not.toHaveBeenCalled();
    expect(mocks.storage.close).not.toHaveBeenCalled();
    messageDone.resolve();
    await stopped;

    expect(mocks.boss.send).toHaveBeenCalledWith(
      "coordinator-alarm",
      null,
      expect.objectContaining({ singletonKey: "fleet" }),
    );
    expect(mocks.boss.send.mock.invocationCallOrder.at(-1)).toBeLessThan(
      mocks.boss.stop.mock.invocationCallOrder.at(-1) ?? 0,
    );
    expect(mocks.storage.close).toHaveBeenCalledOnce();
  });
});
