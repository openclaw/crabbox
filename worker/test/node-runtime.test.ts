import { EventEmitter } from "node:events";

import { beforeEach, describe, expect, it, vi } from "vitest";

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
});
