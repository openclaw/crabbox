import { afterEach, describe, expect, it, vi } from "vitest";

import {
  CloudflareCoordinatorRuntime,
  coordinatorRequestQueue,
  type CoordinatorRuntime,
  type CoordinatorSocketHandlers,
  type CoordinatorStorage,
  type CoordinatorWebSocketUpgradeOptions,
} from "../src/coordinator-runtime";
import { FleetCoordinator } from "../src/fleet";
import { githubAuthRoute } from "../src/oauth";
import { runtimeAdapterRelayFrameLimit } from "../src/runtime-adapter-relay";
import type { Env } from "../src/types";

afterEach(() => {
  vi.unstubAllGlobals();
});

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

  value<T>(key: string): T | undefined {
    return this.values.get(key) as T | undefined;
  }
}

class MemoryRuntime implements CoordinatorRuntime {
  readonly storage = new MemoryStorage();
  readonly ephemeralWebSocketMaxPayloadBytes = 1024 * 1024;
  alarmTime?: number;
  upgradeOptions?: CoordinatorWebSocketUpgradeOptions;
  acceptedTags?: string[];
  acceptedAttachment?: unknown;
  private readonly attachments = new WeakMap<WebSocket, unknown>();
  private exclusiveTail: Promise<void> = Promise.resolve();

  async runExclusive<T>(callback: () => Promise<T>): Promise<T> {
    const predecessor = this.exclusiveTail;
    let release!: () => void;
    this.exclusiveTail = new Promise<void>((resolve) => {
      release = resolve;
    });
    await predecessor;
    try {
      return await callback();
    } finally {
      release();
    }
  }

  createWebSocketUpgrade(options?: CoordinatorWebSocketUpgradeOptions): {
    socket: WebSocket;
    response: Response;
  } {
    this.upgradeOptions = options;
    return {
      socket: {
        readyState: WebSocket.OPEN,
        send: () => undefined,
        close: () => undefined,
        serializeAttachment: () => undefined,
      } as unknown as WebSocket,
      response: new Response(null),
    };
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
    tags: string[],
    _handlers: CoordinatorSocketHandlers,
  ): void {
    this.attachments.set(socket, attachment);
    this.acceptedTags = tags;
    this.acceptedAttachment = attachment;
  }

  acceptEphemeralWebSocket(_socket: WebSocket, _handlers: CoordinatorSocketHandlers): void {}

  async scheduleAlarm(time: number): Promise<void> {
    this.alarmTime = time;
  }

  async clearAlarm(): Promise<void> {
    this.alarmTime = undefined;
  }
}

describe("coordinator runtimes", () => {
  it("applies the protocol frame limit to runtime adapter upgrades", async () => {
    const runtime = new MemoryRuntime();
    const coordinator = new FleetCoordinator(runtime, {
      CRABBOX_DEFAULT_ORG: "example-org",
    } as Env);
    const headers = {
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
    };
    const ticketResponse = await coordinator.fetch(
      new Request("https://coordinator.test/v1/adapters/example-adapter/ticket", {
        method: "POST",
        headers: { ...headers, "content-type": "application/json" },
        body: JSON.stringify({ desktopTimeoutMs: 180_000 }),
      }),
    );
    const { ticket } = (await ticketResponse.json()) as { ticket: string };

    const response = await coordinator.fetch(
      new Request("https://coordinator.test/v1/adapters/example-adapter/agent", {
        headers: {
          ...headers,
          upgrade: "websocket",
          authorization: `Bearer ${ticket}`,
        },
      }),
    );

    expect(response.status).toBe(200);
    expect(runtime.upgradeOptions).toEqual({ maxPayload: runtimeAdapterRelayFrameLimit });
    expect(runtime.acceptedTags).toEqual(["adapter:example-adapter", "runtime-adapter-agent"]);
    expect(runtime.acceptedAttachment).toMatchObject({ desktopTimeoutMs: 180_000 });
    expect(runtime.storage.value("runtime-adapter-identity:example-adapter")).toMatchObject({
      claimVersion: 1,
      claimState: "confirmed",
      confirmedAt: expect.any(String),
    });
  });

  it("keeps provider-backed portal requests outside the lifecycle queue", () => {
    for (const [method, path] of [
      ["GET", "/portal"],
      ["GET", "/portal/admin/health"],
      ["GET", "/portal/hosts/aws/h-123"],
      ["POST", "/portal/hosts/aws/h-123/vnc"],
      ["POST", "/portal/leases/example/release"],
      ["POST", "/v1/workspaces"],
      ["GET", "/v1/workspaces/fleet-is-101"],
      ["DELETE", "/v1/workspaces/fleet-is-101"],
      ["GET", "/v1/native-vnc/handoff"],
      ["GET", "/v1/adapters/applied-alice"],
      ["POST", "/v1/adapters/applied-alice/proxy/v1/workspaces"],
    ]) {
      expect(
        coordinatorRequestQueue(new Request(`https://coordinator.test${path}`, { method })),
      ).toBe("direct");
    }
    expect(coordinatorRequestQueue(new Request("https://coordinator.test/portal/login"))).toBe(
      "lifecycle",
    );
    expect(
      coordinatorRequestQueue(
        new Request("https://coordinator.test/v1/auth/github/callback?code=x&state=y"),
      ),
    ).toBe("direct");
    expect(
      coordinatorRequestQueue(
        new Request("https://coordinator.test/v1/auth/github/start", { method: "POST" }),
      ),
    ).toBe("lifecycle");
    expect(
      coordinatorRequestQueue(
        new Request("https://coordinator.test/v1/adapters/applied-alice/ticket", {
          method: "POST",
        }),
      ),
    ).toBe("lifecycle");
  });

  it("does not hold the lifecycle queue across GitHub OAuth requests", async () => {
    const runtime = new MemoryRuntime();
    const env = {
      CRABBOX_DEFAULT_ORG: "example-org",
      CRABBOX_GITHUB_CLIENT_ID: "github-client",
      CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SESSION_SECRET: "session-secret",
    } as Env;
    const start = await runtime.runExclusive(() =>
      githubAuthRoute(
        new Request("https://coordinator.test/v1/auth/github/start", {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ pollSecretHash: "0".repeat(64) }),
        }),
        "start",
        runtime,
        env,
      ),
    );
    const startBody = (await start.json()) as { url: string };
    const state = new URL(startBody.url).searchParams.get("state");
    expect(state).toBeTruthy();

    let releaseTokenExchange!: () => void;
    const tokenExchangeBlocked = new Promise<void>((resolve) => {
      releaseTokenExchange = resolve;
    });
    let signalTokenExchange!: () => void;
    const tokenExchangeStarted = new Promise<void>((resolve) => {
      signalTokenExchange = resolve;
    });
    vi.stubGlobal(
      "fetch",
      vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
        const url =
          typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
        if (url === "https://github.com/login/oauth/access_token") {
          signalTokenExchange();
          await tokenExchangeBlocked;
          return Response.json({ access_token: "github-access-token" });
        }
        if (url === "https://api.github.com/user") {
          return Response.json({ login: "friend", email: "friend@example.com" });
        }
        if (url === "https://api.github.com/user/emails") {
          return Response.json([]);
        }
        if (url === "https://api.github.com/user/memberships/orgs/example-org") {
          return Response.json({
            state: "active",
            organization: { login: "example-org" },
          });
        }
        throw new Error(`unexpected GitHub URL: ${url}`);
      }),
    );

    const callbackRequest = new Request(
      `https://coordinator.test/v1/auth/github/callback?code=ok&state=${state}`,
    );
    const callback = githubAuthRoute(callbackRequest, "callback", runtime, env);
    await tokenExchangeStarted;

    await expect(runtime.runExclusive(async () => "lifecycle-completed")).resolves.toBe(
      "lifecycle-completed",
    );
    const duplicate = await githubAuthRoute(callbackRequest, "callback", runtime, env);
    expect(duplicate.status).toBe(409);

    releaseTokenExchange();
    await expect(callback).resolves.toMatchObject({ status: 200 });
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

  it("accepts ephemeral sockets without Durable Object hibernation", () => {
    const acceptWebSocket = vi.fn<(socket: WebSocket, tags?: string[]) => void>();
    const state = {
      storage: {},
      acceptWebSocket,
    } as unknown as DurableObjectState;
    const runtime = new CloudflareCoordinatorRuntime(state);
    const listeners = new Map<string, EventListener>();
    const socket = {
      accept: vi.fn<() => void>(),
      addEventListener: vi.fn<(type: string, listener: EventListener) => void>((type, listener) => {
        listeners.set(type, listener);
      }),
    } as unknown as WebSocket;

    runtime.acceptEphemeralWebSocket(socket, {
      message: () => {},
      close: () => {},
      error: () => {},
    });

    expect(socket.accept).toHaveBeenCalledOnce();
    expect(acceptWebSocket).not.toHaveBeenCalled();
    expect([...listeners.keys()]).toEqual(expect.arrayContaining(["message", "close", "error"]));
  });
});
