import { EventEmitter } from "node:events";
import { readFile } from "node:fs/promises";

import { beforeEach, describe, expect, it, vi } from "vitest";

import type { CoordinatorStorageView } from "../src/coordinator-runtime";
import { ProvisioningTestStorage } from "./provisioning-fixtures";

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
    transaction:
      vi.fn<
        (callback: (transaction: CoordinatorStorageView) => Promise<unknown>) => Promise<unknown>
      >(),
    list: vi.fn<(...args: unknown[]) => Promise<Map<string, unknown>>>(),
    initialize: vi.fn<() => Promise<void>>(async () => {}),
    close: vi.fn<() => Promise<void>>(async () => {}),
    get: vi.fn<(key: string) => Promise<unknown>>(async () => undefined),
    put: vi.fn<(key: string, value: unknown) => Promise<void>>(async () => {}),
    delete: vi.fn<(key: string) => Promise<void>>(async () => {}),
    take: vi.fn<(key: string) => Promise<unknown>>(async () => undefined),
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
import { AsyncMutex } from "../node/server-support";

describe("NodeCoordinatorRuntime", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    const storage = new ProvisioningTestStorage();
    mocks.storage.get.mockImplementation((key) => storage.get(key));
    mocks.storage.put.mockImplementation((key, value) => storage.put(key, value));
    mocks.storage.delete.mockImplementation((key) => storage.delete(key));
    mocks.storage.transaction.mockImplementation((callback) => storage.transaction(callback));
    mocks.storage.list.mockImplementation((options) =>
      storage.list(options as Parameters<CoordinatorStorageView["list"]>[0]),
    );
  });

  it("allows an active alarm to enqueue one successor", async () => {
    const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");

    await runtime.start(async () => {});

    expect(mocks.boss.createQueue).toHaveBeenCalledWith(
      "coordinator-alarm",
      expect.objectContaining({ policy: "short" }),
    );
  });

  it("persists the scheduled alarm time across Node coordinator restarts", async () => {
    const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
    mocks.storage.get.mockResolvedValueOnce(1234);

    await expect(runtime.getAlarm()).resolves.toBe(1234);
    await runtime.scheduleAlarm(5678);
    await expect(runtime.getAlarm()).resolves.toBe(5678);
    await runtime.clearAlarm();

    await expect(runtime.getAlarm()).resolves.toBeUndefined();
    expect(mocks.storage.transaction).toHaveBeenCalledTimes(2);
  });

  it("repairs a committed due marker with no queued job at startup and during slow maintenance", async () => {
    vi.useFakeTimers();
    const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
    const at = Date.now();
    const due = `provisioning-due:${at.toString().padStart(16, "0")}:lease`;
    const log = vi.spyOn(console, "error").mockImplementation(() => {});
    mocks.boss.send.mockRejectedValueOnce(new Error("lost queue notification"));
    await runtime.commitAndWake(async (transaction) => {
      await transaction.put(due, { operationID: "lease", at });
    });
    await runtime.stop();
    const restarted = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
    const ticks = vi.fn<() => Promise<void>>(async () => {
      await restarted.storage.delete(due);
    });
    restarted.registerProvisioningTick(ticks);
    await restarted.start(async () => {});
    expect(ticks).toHaveBeenCalledTimes(1);
    const maintenance = deferred<void>();
    restarted.ownMaintenance(maintenance.promise);
    await restarted.storage.put(due, { operationID: "lease", at: Date.now() });
    await vi.advanceTimersByTimeAsync(1000);
    expect(ticks).toHaveBeenCalledTimes(2);
    maintenance.resolve();
    await restarted.stop();
    log.mockRestore();
    vi.useRealTimers();
  });

  it("keeps scanning due work when the queue hint is stalled", async () => {
    vi.useFakeTimers();
    const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
    const ticks = vi.fn<() => Promise<void>>(async () => {});
    runtime.registerProvisioningTick(ticks);
    await runtime.start(async () => {});
    const stalled = deferred<void>();
    mocks.boss.deleteQueuedJobs.mockImplementationOnce(() => stalled.promise);
    const at = Date.now();
    const committed = runtime.commitAndWake(async (transaction) => {
      await transaction.put(`provisioning-due:${at.toString().padStart(16, "0")}:lease`, {
        operationID: "lease",
        at,
      });
    });
    await vi.advanceTimersByTimeAsync(1000);
    await committed;
    expect(ticks).toHaveBeenCalled();
    stalled.resolve();
    await runtime.stop();
    vi.useRealTimers();
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

  it("accepts ephemeral sockets through the Node websocket runtime", async () => {
    const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
    const socket = Object.assign(new EventEmitter(), {
      close: vi.fn<(code?: number, reason?: string) => void>(),
      terminate: vi.fn<() => void>(),
    });
    const message = vi.fn<() => Promise<void>>(async () => {});

    runtime.acceptEphemeralWebSocket(socket as unknown as WebSocket, {
      message,
      close: vi.fn<(code: number, reason: string) => void>(),
      error: vi.fn<() => void>(),
    });
    socket.emit("message", Buffer.from("terminal"), false);

    await vi.waitFor(() => {
      expect(message).toHaveBeenCalledWith("terminal");
    });
    expect((socket as unknown as { accept?: unknown }).accept).toBeUndefined();
  });

  it("delegates workspace terminal acceptance to the coordinator runtime", async () => {
    const source = await readFile(new URL("../src/fleet.ts", import.meta.url), "utf8");
    const start = source.indexOf("private async workspaceTerminal");
    const end = source.indexOf("private async connectWorkspaceTerminal", start);
    const terminalRoute = source.slice(start, end);

    expect(terminalRoute).toContain(
      "createWebSocketUpgrade({\n        maxPayload: workspaceTerminalMaxBufferedBytes",
    );
    expect(terminalRoute).toContain("return await this.state.runExclusive");
    expect(terminalRoute.indexOf("trackWorkspaceTerminal")).toBeLessThan(
      terminalRoute.indexOf("connectWorkspaceTerminal"),
    );
    expect(terminalRoute).not.toContain("socket.accept()");
  });

  it("defaults terminal bootstrap fields for persisted workspaces", async () => {
    const source = await readFile(new URL("../src/fleet.ts", import.meta.url), "utf8");
    const start = source.indexOf("function workspaceTerminalBootstrapCommand");
    const end = source.indexOf("function shellQuote", start);
    const bootstrap = source.slice(start, end);

    expect(bootstrap).toContain('workspace.branch?.trim() || "main"');
    expect(bootstrap).toContain('workspace.command?.trim() || "exec bash -l"');
    expect(bootstrap).not.toContain("checkout -B");
    expect(bootstrap).not.toContain("fetch --depth=1");
    expect(bootstrap).toContain("Workspace command exited with status %s");
    expect(bootstrap).toContain("exec bash -l");
  });

  it("bounds terminal input by bytes and queued frame count", async () => {
    const source = await readFile(new URL("../src/fleet.ts", import.meta.url), "utf8");
    const start = source.indexOf("private async connectWorkspaceTerminal");
    const end = source.indexOf("private trackWorkspaceTerminal", start);
    const terminal = source.slice(start, end);
    const sshStart = source.indexOf("async function connectWorkspaceSSH");
    const sshEnd = source.indexOf("async function readWorkspaceVNCPassword", sshStart);
    const ssh = source.slice(sshStart, sshEnd);

    expect(terminal).toContain("workspaceTerminalMaxBufferedFrames");
    expect(source).toContain("workspaceTerminalTransportMemoryBudgetBytes");
    expect(source).toContain("this.state.ephemeralWebSocketMaxPayloadBytes");
    expect(terminal).toContain("pending.length + queuedInputFrames");
    expect(terminal).toContain("queuedInputFrames -= 1");
    expect(terminal).toContain("if (length === 0) return");
    expect(ssh).toContain("observedHostKey = fingerprint");
    expect(ssh).toContain("return expectedHostKey === fingerprint");
    expect(ssh).toContain('cipher: ["aes128-ctr", "aes192-ctr", "aes256-ctr"]');
    expect(ssh).toContain('"hmac-sha2-256-etm@openssh.com"');
    expect(ssh).toContain('"hmac-sha2-512"');
    expect(ssh).toContain("workspaceTerminalSSHReadyTimeoutMs");
    expect(ssh).toContain("for (const port of ports)");
    expect(ssh).toContain("await new Promise<void>((resolve) => setTimeout(resolve, 2_000))");
    expect(terminal).not.toContain("!workspace.sshHostKeySha256");
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

  it("lets runtime-adapter replies bypass the lifecycle queue", async () => {
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

    runtime.acceptWebSocket(socket as unknown as WebSocket, { kind: "runtime-adapter-agent" }, [], {
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

  it.each([
    { kind: "control", payload: "{}", isBinary: false },
    { kind: "control", payload: '{"type":"subscribe"}', isBinary: false },
    { kind: "control", payload: '{"type":"heartbeat"', isBinary: false },
    { kind: "control", payload: '{"type":"heartbeat"}', isBinary: true },
    { kind: "unknown", payload: '{"type":"heartbeat"}', isBinary: false },
  ])(
    "serializes $kind messages ($payload, binary=$isBinary) with lifecycle operations",
    async ({ kind, payload, isBinary }) => {
      const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
      const socket = Object.assign(new EventEmitter(), {
        close: vi.fn<(code?: number, reason?: string) => void>(),
        terminate: vi.fn<() => void>(),
      });
      const mutex = new AsyncMutex();
      const operationRunner = vi.fn<OperationRunner>((callback) => mutex.run(callback));
      runtime.setOperationRunner(operationRunner);
      const lifecycleDone = deferred<void>();
      const lifecycle = runtime.runExclusive(async () => lifecycleDone.promise);
      const message = vi.fn<() => Promise<void>>(async () => {});

      runtime.acceptWebSocket(socket as unknown as WebSocket, { kind }, [], {
        message,
        close: vi.fn<(code: number, reason: string) => void>(),
        error: vi.fn<() => void>(),
      });
      socket.emit("message", Buffer.from(payload), isBinary);

      await new Promise<void>((resolve) => setImmediate(resolve));
      expect(message).not.toHaveBeenCalled();
      lifecycleDone.resolve();
      await lifecycle;

      await vi.waitFor(() => {
        expect(message).toHaveBeenCalledWith(
          isBinary ? Uint8Array.from(Buffer.from(payload)).buffer : payload,
        );
      });
      expect(operationRunner).toHaveBeenCalledTimes(2);
    },
  );

  it.each(["code-agent", "runtime-adapter-agent"])(
    "keeps %s data-plane close callbacks behind earlier messages",
    async (kind) => {
      const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
      const socket = Object.assign(new EventEmitter(), {
        close: vi.fn<(code?: number, reason?: string) => void>(),
        terminate: vi.fn<() => void>(),
      });
      const order: string[] = [];
      const messageDone = deferred<void>();

      runtime.acceptWebSocket(socket as unknown as WebSocket, { kind }, [], {
        message: async () => {
          order.push("message");
          await messageDone.promise;
        },
        close: () => {
          order.push("close");
        },
        error: vi.fn<() => void>(),
      });
      socket.emit("message", Buffer.from('{"type":"heartbeat"}'), false);
      socket.emit("message", Buffer.from("{}"), false);
      socket.emit("close", 1000, Buffer.from("done"));

      await vi.waitFor(() => {
        expect(order).toEqual(["message"]);
      });
      messageDone.resolve();
      await vi.waitFor(() => {
        expect(order).toEqual(["message", "message", "close"]);
      });
    },
  );

  it.each(["code-agent", "runtime-adapter-agent", "control"])(
    "drains %s socket operations that own lifecycle transactions before stopping jobs and closing storage",
    async (kind) => {
      const runtime = new NodeCoordinatorRuntime("postgresql://example.invalid/test");
      const mutex = new AsyncMutex();
      runtime.setOperationRunner((callback) => mutex.run(callback));
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
      let heartbeatCompleted = false;
      const message = vi.fn<() => Promise<void>>(async () => {
        await runtime.runExclusive(async () => {
          await runtime.scheduleAlarm(Date.now() + 60_000);
        });
        heartbeatCompleted = true;
        await messageDone.promise;
      });

      runtime.acceptWebSocket(socket as unknown as WebSocket, { kind }, [], {
        message,
        close: vi.fn<(code: number, reason: string) => void>(),
        error: vi.fn<() => void>(),
      });
      socket.emit("message", Buffer.from('{"type":"heartbeat"}'), false);
      await vi.waitFor(() => expect(heartbeatCompleted).toBe(true));
      const followingLifecycle = vi.fn<() => Promise<void>>(async () => {});
      await runtime.runExclusive(followingLifecycle);
      await mutex.drain();
      expect(followingLifecycle).toHaveBeenCalledOnce();

      runtime.beginShutdown();
      expect(close).toHaveBeenCalledOnce();
      const stopped = runtime.stop();
      await new Promise<void>((resolve) => setImmediate(resolve));
      expect(mocks.boss.stop).not.toHaveBeenCalled();
      expect(mocks.storage.close).not.toHaveBeenCalled();
      messageDone.resolve();
      await stopped;
      await mutex.drain();

      expect(mocks.boss.send).toHaveBeenCalledWith(
        "coordinator-alarm",
        null,
        expect.objectContaining({ singletonKey: "fleet" }),
      );
      expect(mocks.boss.send.mock.invocationCallOrder.at(-1)).toBeLessThan(
        mocks.boss.stop.mock.invocationCallOrder.at(-1) ?? 0,
      );
      expect(mocks.storage.close).toHaveBeenCalledOnce();
    },
  );
});
