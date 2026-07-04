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
import type { Env, LeaseRecord } from "../src/types";

afterEach(() => {
  vi.unstubAllGlobals();
});

class MemoryStorage implements CoordinatorStorage {
  private readonly values = new Map<string, unknown>();
  beforeGet?: (key: string) => Promise<void>;

  async get<T>(key: string): Promise<T | undefined> {
    await this.beforeGet?.(key);
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
  onAcceptWebSocket?: (attachment: unknown) => void;
  readonly socketCloses: Array<{ code?: number; reason?: string }> = [];
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
        close: (code?: number, reason?: string) => {
          this.socketCloses.push({ code, reason });
        },
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
    this.onAcceptWebSocket?.(attachment);
  }

  acceptEphemeralWebSocket(_socket: WebSocket, _handlers: CoordinatorSocketHandlers): void {}

  async take<T>(key: string): Promise<T | undefined> {
    const value = await this.storage.get<T>(key);
    if (value !== undefined) {
      await this.storage.delete(key);
    }
    return value;
  }

  async getAlarm(): Promise<number | undefined> {
    return this.alarmTime;
  }

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

  it("binds egress sockets to their ticket principal and reauthorizes before acceptance", async () => {
    const runtime = new MemoryRuntime();
    const coordinator = new FleetCoordinator(runtime, {
      CRABBOX_DEFAULT_ORG: "example-org",
    } as Env);
    const lease: LeaseRecord = {
      id: "cbx_000000000001",
      slug: "shared-egress",
      provider: "external",
      lifecycle: "registered",
      target: "linux",
      cloudID: "external-shared-egress",
      owner: "owner@example.com",
      org: "example-org",
      share: { users: { "manager@example.com": "manage" } },
      profile: "default",
      class: "default",
      serverType: "external",
      serverID: 0,
      serverName: "shared-egress",
      providerKey: "external-shared-egress",
      host: "127.0.0.1",
      sshUser: "crabbox",
      sshPort: "22",
      workRoot: "/work/crabbox",
      keep: true,
      ttlSeconds: 3600,
      estimatedHourlyUSD: 0,
      maxEstimatedUSD: 0,
      state: "active",
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    };
    await runtime.storage.put(`lease:${lease.id}`, lease);
    const managerHeaders = {
      "x-crabbox-owner": "manager@example.com",
      "x-crabbox-org": "example-org",
    };
    const ownerHeaders = {
      "x-crabbox-owner": "owner@example.com",
      "x-crabbox-org": "example-org",
    };
    const createTicket = async (sessionID: string): Promise<string> => {
      const response = await coordinator.fetch(
        new Request("https://coordinator.test/v1/leases/shared-egress/egress/ticket", {
          method: "POST",
          headers: { ...managerHeaders, "content-type": "application/json" },
          body: JSON.stringify({ role: "host", sessionID }),
        }),
      );
      expect(response.status).toBe(200);
      return ((await response.json()) as { ticket: string }).ticket;
    };

    const acceptedTicket = await createTicket("egress_accepted");
    const accepted = await coordinator.fetch(
      new Request("https://coordinator.test/v1/leases/shared-egress/egress/host", {
        headers: {
          upgrade: "websocket",
          authorization: `Bearer ${acceptedTicket}`,
        },
      }),
    );
    expect(accepted.status).toBe(200);
    expect(runtime.acceptedAttachment).toEqual({
      kind: "egress-host",
      leaseID: lease.id,
      sessionID: "egress_accepted",
      owner: "manager@example.com",
      org: "example-org",
      admin: false,
    });

    const revokedTicket = await createTicket("egress_revoked");
    const downgraded = await coordinator.fetch(
      new Request("https://coordinator.test/v1/leases/shared-egress/share", {
        method: "PUT",
        headers: { ...ownerHeaders, "content-type": "application/json" },
        body: JSON.stringify({ users: { "manager@example.com": "use" } }),
      }),
    );
    expect(downgraded.status).toBe(200);
    runtime.acceptedAttachment = undefined;
    const rejected = await coordinator.fetch(
      new Request("https://coordinator.test/v1/leases/shared-egress/egress/host", {
        headers: {
          upgrade: "websocket",
          authorization: `Bearer ${revokedTicket}`,
        },
      }),
    );
    expect(rejected.status).toBe(401);
    expect(runtime.acceptedAttachment).toBeUndefined();
    expect(runtime.storage.value(`egress-ticket:${revokedTicket}`)).toBeUndefined();

    const restored = await coordinator.fetch(
      new Request("https://coordinator.test/v1/leases/shared-egress/share", {
        method: "PUT",
        headers: { ...ownerHeaders, "content-type": "application/json" },
        body: JSON.stringify({ users: { "manager@example.com": "manage" } }),
      }),
    );
    expect(restored.status).toBe(200);
    const raceTicket = await createTicket("egress_race");
    runtime.socketCloses.length = 0;
    let releaseTicketRead!: () => void;
    const ticketReadBlocked = new Promise<void>((resolve) => {
      releaseTicketRead = resolve;
    });
    let signalTicketRead!: () => void;
    const ticketReadStarted = new Promise<void>((resolve) => {
      signalTicketRead = resolve;
    });
    runtime.storage.beforeGet = async (key) => {
      if (key === `egress-ticket:${raceTicket}`) {
        signalTicketRead();
        await ticketReadBlocked;
      }
    };
    const acceptance = coordinator.fetch(
      new Request("https://coordinator.test/v1/leases/shared-egress/egress/host", {
        headers: {
          upgrade: "websocket",
          authorization: `Bearer ${raceTicket}`,
        },
      }),
    );
    await ticketReadStarted;
    const concurrentDowngrade = coordinator.fetch(
      new Request("https://coordinator.test/v1/leases/shared-egress/share", {
        method: "PUT",
        headers: { ...ownerHeaders, "content-type": "application/json" },
        body: JSON.stringify({ users: { "manager@example.com": "use" } }),
      }),
    );
    runtime.storage.beforeGet = undefined;
    releaseTicketRead();
    const [raceAccepted, raceDowngraded] = await Promise.all([acceptance, concurrentDowngrade]);
    expect(raceAccepted.status).toBe(200);
    expect(raceDowngraded.status).toBe(200);
    expect(runtime.socketCloses).toContainEqual({ code: 1008, reason: "lease access revoked" });
  });

  it("reauthorizes WebVNC and Code agent tickets through socket acceptance", async () => {
    const runtime = new MemoryRuntime();
    const coordinator = new FleetCoordinator(runtime, {
      CRABBOX_DEFAULT_ORG: "example-org",
      CRABBOX_GITHUB_ADMIN_LOGINS: "admin",
    } as Env);
    const lease: LeaseRecord = {
      id: "cbx_000000000001",
      slug: "shared-bridges",
      provider: "external",
      lifecycle: "registered",
      target: "linux",
      cloudID: "external-shared-bridges",
      owner: "owner@example.com",
      org: "example-org",
      share: { users: { "manager@example.com": "manage" } },
      profile: "default",
      class: "default",
      serverType: "external",
      serverID: 0,
      serverName: "shared-bridges",
      providerKey: "external-shared-bridges",
      host: "127.0.0.1",
      sshUser: "crabbox",
      sshPort: "22",
      workRoot: "/work/crabbox",
      keep: true,
      ttlSeconds: 3600,
      estimatedHourlyUSD: 0,
      maxEstimatedUSD: 0,
      state: "active",
      desktop: true,
      code: true,
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    };
    await runtime.storage.put(`lease:${lease.id}`, lease);
    const managerHeaders = {
      "x-crabbox-owner": "manager@example.com",
      "x-crabbox-org": "example-org",
    };
    const ownerHeaders = {
      "x-crabbox-owner": "owner@example.com",
      "x-crabbox-org": "example-org",
    };
    const adminHeaders = {
      "x-crabbox-owner": "admin@example.com",
      "x-crabbox-org": "example-org",
      "x-crabbox-admin": "true",
      "x-crabbox-auth": "github",
      "x-crabbox-github-login": "admin",
    };
    const createTicket = async (
      kind: "webvnc" | "code",
      headers: Record<string, string> = managerHeaders,
    ): Promise<string> => {
      const response = await coordinator.fetch(
        new Request(`https://coordinator.test/v1/leases/shared-bridges/${kind}/ticket`, {
          method: "POST",
          headers,
        }),
      );
      expect(response.status).toBe(200);
      return ((await response.json()) as { ticket: string }).ticket;
    };
    const connect = async (kind: "webvnc" | "code", ticket: string): Promise<Response> =>
      await coordinator.fetch(
        new Request(`https://coordinator.test/v1/leases/shared-bridges/${kind}/agent`, {
          headers: {
            upgrade: "websocket",
            authorization: `Bearer ${ticket}`,
          },
        }),
      );

    const acceptedWebVNCTicket = await createTicket("webvnc");
    expect((await connect("webvnc", acceptedWebVNCTicket)).status).toBe(200);
    expect(runtime.acceptedAttachment).toMatchObject({
      kind: "webvnc-agent",
      leaseID: lease.id,
    });

    const acceptedCodeTicket = await createTicket("code");
    expect((await connect("code", acceptedCodeTicket)).status).toBe(200);
    expect(runtime.acceptedAttachment).toEqual({ kind: "code-agent", leaseID: lease.id });

    expect((await connect("webvnc", await createTicket("webvnc", adminHeaders))).status).toBe(200);
    expect((await connect("code", await createTicket("code", adminHeaders))).status).toBe(200);

    const revokedWebVNCTicket = await createTicket("webvnc");
    const revokedCodeTicket = await createTicket("code");
    const downgraded = await coordinator.fetch(
      new Request("https://coordinator.test/v1/leases/shared-bridges/share", {
        method: "PUT",
        headers: { ...ownerHeaders, "content-type": "application/json" },
        body: JSON.stringify({ users: { "manager@example.com": "use" } }),
      }),
    );
    expect(downgraded.status).toBe(200);

    runtime.acceptedAttachment = undefined;
    expect((await connect("webvnc", revokedWebVNCTicket)).status).toBe(401);
    expect(runtime.acceptedAttachment).toBeUndefined();
    expect(runtime.storage.value(`webvnc-ticket:${revokedWebVNCTicket}`)).toBeUndefined();

    expect((await connect("code", revokedCodeTicket)).status).toBe(401);
    expect(runtime.acceptedAttachment).toBeUndefined();
    expect(runtime.storage.value(`code-ticket:${revokedCodeTicket}`)).toBeUndefined();

    const expectAtomicAcceptance = async (kind: "webvnc" | "code") => {
      const restored = await coordinator.fetch(
        new Request("https://coordinator.test/v1/leases/shared-bridges/share", {
          method: "PUT",
          headers: { ...ownerHeaders, "content-type": "application/json" },
          body: JSON.stringify({ users: { "manager@example.com": "manage" } }),
        }),
      );
      expect(restored.status).toBe(200);
      const ticket = await createTicket(kind);
      let ticketRead!: () => void;
      let allowTicketRead!: () => void;
      const ticketReadStarted = new Promise<void>((resolve) => {
        ticketRead = resolve;
      });
      const ticketReadMayFinish = new Promise<void>((resolve) => {
        allowTicketRead = resolve;
      });
      runtime.storage.beforeGet = async (key) => {
        if (key === `${kind}-ticket:${ticket}`) {
          ticketRead();
          await ticketReadMayFinish;
        }
      };

      runtime.acceptedAttachment = undefined;
      let roleAtAccept: string | undefined;
      runtime.onAcceptWebSocket = (attachment) => {
        if ((attachment as { kind?: string }).kind === `${kind}-agent`) {
          roleAtAccept = runtime.storage.value<LeaseRecord>(`lease:${lease.id}`)?.share?.users?.[
            "manager@example.com"
          ];
        }
      };
      const connecting = connect(kind, ticket);
      await ticketReadStarted;
      const revoking = coordinator.fetch(
        new Request("https://coordinator.test/v1/leases/shared-bridges/share", {
          method: "PUT",
          headers: { ...ownerHeaders, "content-type": "application/json" },
          body: JSON.stringify({ users: { "manager@example.com": "use" } }),
        }),
      );
      allowTicketRead();

      expect((await connecting).status).toBe(200);
      expect((await revoking).status).toBe(200);
      expect(runtime.acceptedAttachment).toMatchObject({
        kind: `${kind}-agent`,
        leaseID: lease.id,
      });
      expect(roleAtAccept).toBe("manage");
      expect(
        runtime.storage.value<LeaseRecord>(`lease:${lease.id}`)?.share?.users?.[
          "manager@example.com"
        ],
      ).toBe("use");
      expect(runtime.storage.value(`${kind}-ticket:${ticket}`)).toBeUndefined();
      runtime.storage.beforeGet = undefined;
      runtime.onAcceptWebSocket = undefined;
    };
    await expectAtomicAcceptance("webvnc");
    await expectAtomicAcceptance("code");
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
          return Response.json({ login: "friend", email: "public@example.com" });
        }
        if (url === "https://api.github.com/user/emails") {
          return Response.json([{ email: "friend@example.com", primary: true, verified: true }]);
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
    const transactionGet = vi.fn<() => Promise<unknown>>(async () => ({ ticket: "one-time" }));
    const transactionDelete = vi.fn<() => Promise<void>>(async () => {});
    const storage = {
      getAlarm: vi.fn<() => Promise<number | null>>(async () => 1234),
      setAlarm: vi.fn<(time: number) => Promise<void>>(async () => {}),
      deleteAlarm: vi.fn<() => Promise<void>>(async () => {}),
      transaction: vi.fn<
        (callback: (transaction: unknown) => Promise<unknown>) => Promise<unknown>
      >(async (callback) => callback({ get: transactionGet, delete: transactionDelete })),
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
    await expect(runtime.take("ticket:one-time")).resolves.toEqual({ ticket: "one-time" });
    await expect(runtime.getAlarm()).resolves.toBe(1234);
    await runtime.scheduleAlarm(1234);
    await runtime.clearAlarm();

    expect(acceptWebSocket).toHaveBeenCalledWith(socket, ["control:client-1"]);
    expect(serializeAttachment).toHaveBeenCalledWith(attachment);
    expect(runtime.socketAttachment(socket)).toBe(attachment);
    expect(storage.getAlarm).toHaveBeenCalledOnce();
    expect(transactionGet).toHaveBeenCalledWith("ticket:one-time");
    expect(transactionDelete).toHaveBeenCalledWith("ticket:one-time");
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
