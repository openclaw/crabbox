import { describe, expect, it, vi } from "vitest";

import {
  CloudflareCoordinatorRuntime,
  coordinatorRequestQueue,
  type CoordinatorRuntime,
  type CoordinatorSocketHandlers,
  type CoordinatorStorage,
} from "../src/coordinator-runtime";
import { FleetCoordinator } from "../src/fleet";
import type { Env } from "../src/types";

class MemoryStorage implements CoordinatorStorage {
  private readonly values = new Map<string, unknown>();

  async get<T>(key: string): Promise<T | undefined> {
    return this.values.get(key) as T | undefined;
  }

  async put<T>(key: string, value: T): Promise<void> {
    this.values.set(key, value);
  }

  async delete(key: string): Promise<void> {
    this.values.delete(key);
  }

  async list<T>({ prefix = "" }: { prefix?: string } = {}): Promise<Map<string, T>> {
    return new Map(
      [...this.values]
        .filter(([key]) => key.startsWith(prefix))
        .map(([key, value]) => [key, value as T]),
    );
  }
}

class MemoryRuntime implements CoordinatorRuntime {
  readonly storage = new MemoryStorage();
  alarmTime?: number;
  private readonly attachments = new WeakMap<WebSocket, unknown>();

  runExclusive<T>(callback: () => Promise<T>): Promise<T> {
    return callback();
  }

  createWebSocketUpgrade(): never {
    throw new Error("websocket upgrade not configured");
  }

  getWebSockets(): Iterable<WebSocket> {
    return [];
  }

  socketAttachment<T>(socket: WebSocket): T | undefined {
    return this.attachments.get(socket) as T | undefined;
  }

  setSocketAttachment(socket: WebSocket, attachment: unknown): void {
    this.attachments.set(socket, attachment);
  }

  acceptWebSocket(
    socket: WebSocket,
    attachment: unknown,
    _tags: string[],
    _handlers: CoordinatorSocketHandlers,
  ): void {
    this.attachments.set(socket, attachment);
  }

  async scheduleAlarm(time: number): Promise<void> {
    this.alarmTime = time;
  }

  async clearAlarm(): Promise<void> {
    this.alarmTime = undefined;
  }
}

describe("coordinator runtimes", () => {
  it("keeps provider-backed portal requests outside the lifecycle queue", () => {
    for (const [method, path] of [
      ["GET", "/portal"],
      ["GET", "/portal/admin/health"],
      ["GET", "/portal/hosts/aws/h-123"],
      ["POST", "/portal/hosts/aws/h-123/vnc"],
      ["POST", "/portal/leases/example/release"],
    ]) {
      expect(
        coordinatorRequestQueue(new Request(`https://coordinator.test${path}`, { method })),
      ).toBe("direct");
    }
    expect(coordinatorRequestQueue(new Request("https://coordinator.test/portal/login"))).toBe(
      "lifecycle",
    );
  });

  it("runs the fleet coordinator without a Durable Object", async () => {
    const coordinator = new FleetCoordinator(new MemoryRuntime(), {
      CRABBOX_DEFAULT_ORG: "example-org",
    } as Env);

    const response = await coordinator.fetch(new Request("https://coordinator.test/v1/health"));

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toEqual({ ok: true, fleet: "default" });
  });

  it("maps Cloudflare alarms and hibernating socket attachments", async () => {
    const storage = {
      setAlarm: vi.fn<(time: number) => Promise<void>>(async () => {}),
      deleteAlarm: vi.fn<() => Promise<void>>(async () => {}),
    };
    const acceptWebSocket = vi.fn<(socket: WebSocket, tags?: string[]) => void>();
    const state = {
      storage,
      acceptWebSocket,
      getWebSockets: () => [],
    } as unknown as DurableObjectState;
    const runtime = new CloudflareCoordinatorRuntime(state);
    const serializeAttachment = vi.fn<(value: unknown) => void>();
    const socket = { serializeAttachment } as unknown as WebSocket;
    const attachment = { kind: "control", clientID: "client-1" };

    runtime.acceptWebSocket(socket, attachment, ["control:client-1"], {
      message: () => {},
      close: () => {},
      error: () => {},
    });
    await runtime.scheduleAlarm(1234);
    await runtime.clearAlarm();

    expect(acceptWebSocket).toHaveBeenCalledWith(socket, ["control:client-1"]);
    expect(serializeAttachment).toHaveBeenCalledWith(attachment);
    expect(runtime.socketAttachment(socket)).toBe(attachment);
    expect(storage.setAlarm).toHaveBeenCalledWith(1234);
    expect(storage.deleteAlarm).toHaveBeenCalledOnce();
  });

  it("serializes Cloudflare coordinator state transitions", async () => {
    const state = {
      storage: {},
    } as unknown as DurableObjectState;
    const runtime = new CloudflareCoordinatorRuntime(state);
    const order: string[] = [];
    let releaseFirst!: () => void;
    const firstBlocked = new Promise<void>((resolve) => {
      releaseFirst = resolve;
    });

    const first = runtime.runExclusive(async () => {
      order.push("first:start");
      await firstBlocked;
      order.push("first:end");
    });
    const second = runtime.runExclusive(async () => {
      order.push("second");
    });

    await Promise.resolve();
    expect(order).toEqual(["first:start"]);
    releaseFirst();
    await Promise.all([first, second]);
    expect(order).toEqual(["first:start", "first:end", "second"]);
  });

  it("serializes fallback control socket messages with state transitions", async () => {
    const state = {
      storage: {},
    } as unknown as DurableObjectState;
    const runtime = new CloudflareCoordinatorRuntime(state);
    const listeners = new Map<string, EventListener>();
    const socket = {
      accept: vi.fn<() => void>(),
      addEventListener: vi.fn<(type: string, listener: EventListener) => void>((type, listener) => {
        listeners.set(type, listener);
      }),
    } as unknown as WebSocket;
    const message = vi.fn<() => Promise<void>>(async () => {});
    let releaseTransition!: () => void;
    const transitionBlocked = new Promise<void>((resolve) => {
      releaseTransition = resolve;
    });
    const transition = runtime.runExclusive(async () => transitionBlocked);
    runtime.acceptWebSocket(socket, { kind: "control" }, [], {
      message,
      close: () => {},
      error: () => {},
    });

    listeners.get("message")?.({ data: "{}" } as MessageEvent);
    await Promise.resolve();
    expect(message).not.toHaveBeenCalled();

    releaseTransition();
    await transition;
    await vi.waitFor(() => expect(message).toHaveBeenCalledOnce());
  });
});
