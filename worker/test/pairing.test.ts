import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { routeCoordinatorRequest } from "../src/coordinator-entry";
import {
  type CoordinatorRuntime,
  type CoordinatorSocketHandlers,
  type CoordinatorStorage,
  type CoordinatorWebSocketUpgradeOptions,
} from "../src/coordinator-runtime";
import { FleetCoordinator } from "../src/fleet";
import { orgKeyForLabel } from "../src/org-identity";
import { deviceTokenTTLSeconds, pairingGrantTTLSeconds } from "../src/pairing";
import type { Env, LeaseRecord } from "../src/types";

const origin = "https://coordinator.example.test";
const owner = "alice@example.com";
const org = "example-org";

class MemoryStorage implements CoordinatorStorage {
  private readonly values = new Map<string, unknown>();

  get<T>(key: string): Promise<T | undefined> {
    return Promise.resolve(this.values.get(key) as T | undefined);
  }

  put<T>(key: string, value: T): Promise<void> {
    this.values.set(key, value);
    return Promise.resolve();
  }

  delete(key: string): Promise<void> {
    this.values.delete(key);
    return Promise.resolve();
  }

  list<T>({ prefix = "" }: { prefix?: string } = {}): Promise<Map<string, T>> {
    return Promise.resolve(
      new Map(
        [...this.values]
          .filter(([key]) => key.startsWith(prefix))
          .map(([key, value]) => [key, value as T]),
      ),
    );
  }

  take<T>(key: string): Promise<T | undefined> {
    const value = this.values.get(key) as T | undefined;
    this.values.delete(key);
    return Promise.resolve(value);
  }

  serialized(): string {
    return JSON.stringify([...this.values]);
  }
}

class MemoryRuntime implements CoordinatorRuntime {
  readonly storage = new MemoryStorage();
  readonly ephemeralWebSocketMaxPayloadBytes = 1024 * 1024;
  private exclusive = Promise.resolve();

  async runExclusive<T>(callback: () => Promise<T>): Promise<T> {
    const previous = this.exclusive;
    let release!: () => void;
    this.exclusive = new Promise<void>((resolve) => {
      release = resolve;
    });
    await previous;
    try {
      return await callback();
    } finally {
      release();
    }
  }

  createWebSocketUpgrade(_options?: CoordinatorWebSocketUpgradeOptions): never {
    throw new Error("websocket upgrade is not expected");
  }

  getWebSockets(): Iterable<WebSocket> {
    return [];
  }

  socketAttachment<T>(_socket: WebSocket): T | undefined {
    return undefined;
  }

  setSocketAttachment(_socket: WebSocket, _attachment: unknown): void {}

  acceptWebSocket(
    _socket: WebSocket,
    _attachment: unknown,
    _tags: string[],
    _handlers: CoordinatorSocketHandlers,
  ): void {}

  acceptEphemeralWebSocket(_socket: WebSocket, _handlers: CoordinatorSocketHandlers): void {}

  take<T>(key: string): Promise<T | undefined> {
    return this.storage.take<T>(key);
  }

  getAlarm(): Promise<number | undefined> {
    return Promise.resolve(undefined);
  }

  scheduleAlarm(_time: number): Promise<void> {
    return Promise.resolve();
  }

  clearAlarm(): Promise<void> {
    return Promise.resolve();
  }
}

interface PairingFixture {
  env: Env;
  runtime: MemoryRuntime;
  request(
    path: string,
    init?: RequestInit,
    requestOrigin?: string,
    destinationOrigin?: string,
  ): Promise<Response>;
  ownerRequest(path: string, init?: RequestInit, requestOrigin?: string): Promise<Response>;
  pair(name?: string): Promise<{ deviceID: string; grant: string; token: string }>;
}

describe("coordinator device pairing", () => {
  beforeEach(() => {
    vi.useRealTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("exchanges an owner grant for a read-only token and reuses lease visibility", async () => {
    const fixture = pairingFixture();
    await seedLease(fixture.runtime, lease("cbx_000000000001", owner));
    await seedLease(
      fixture.runtime,
      lease("cbx_000000000002", "bob@example.com", { users: { [owner]: "use" } }),
    );
    await seedLease(fixture.runtime, lease("cbx_000000000003", "carol@example.com"));

    const { deviceID, grant, token } = await fixture.pair("Alice's phone");
    expect(fixture.runtime.storage.serialized()).not.toContain(grant);
    expect(fixture.runtime.storage.serialized()).not.toContain(token);

    const list = await fixture.request("/v1/leases", deviceRequest(token));
    expect(list.status).toBe(200);
    const listBody = (await list.json()) as { leases: LeaseRecord[] };
    expect(listBody.leases.map((item) => item.id).toSorted()).toEqual([
      "cbx_000000000001",
      "cbx_000000000002",
    ]);

    const inspect = await fixture.request("/v1/leases/cbx_000000000002", deviceRequest(token));
    expect(inspect.status).toBe(200);
    await expect(inspect.json()).resolves.toMatchObject({
      lease: { id: "cbx_000000000002", owner: "bob@example.com" },
    });

    const devices = await fixture.ownerRequest("/v1/devices");
    expect(devices.status).toBe(200);
    expect(devices.headers.get("cache-control")).toBe("no-store");
    const deviceText = await devices.text();
    expect(deviceText).not.toContain(token);
    expect(JSON.parse(deviceText)).toEqual({
      devices: [
        expect.objectContaining({
          id: deviceID,
          audience: "crabbox-device",
          scope: ["leases:read"],
          name: "Alice's phone",
        }),
      ],
    });
  });

  it("expires grants, consumes successful grants once, and rejects replay", async () => {
    vi.useFakeTimers({ now: new Date("2026-07-27T12:00:00Z") });
    const fixture = pairingFixture();
    const expiredGrant = await issueGrant(fixture);
    await vi.advanceTimersByTimeAsync(pairingGrantTTLSeconds * 1000 + 1);

    const expired = await exchangeGrant(fixture, expiredGrant);
    expect(expired.status).toBe(401);
    await expect(expired.json()).resolves.toEqual({ error: "pairing_grant_expired" });

    const liveGrant = await issueGrant(fixture);
    const accepted = await exchangeGrant(fixture, liveGrant);
    expect(accepted.status).toBe(200);
    const replayed = await exchangeGrant(fixture, liveGrant);
    expect(replayed.status).toBe(401);
    await expect(replayed.json()).resolves.toEqual({ error: "pairing_grant_invalid" });
  });

  it("revokes individual devices and all owner devices immediately", async () => {
    const fixture = pairingFixture();
    await seedLease(fixture.runtime, lease("cbx_000000000001", owner));
    const first = await fixture.pair("First phone");
    const second = await fixture.pair("Second phone");

    const revokeOne = await fixture.ownerRequest(`/v1/devices/${first.deviceID}`, {
      method: "DELETE",
    });
    expect(revokeOne.status).toBe(204);
    expect((await fixture.request("/v1/leases", deviceRequest(first.token))).status).toBe(401);
    expect((await fixture.request("/v1/leases", deviceRequest(second.token))).status).toBe(200);

    const revokeAll = await fixture.ownerRequest("/v1/devices", { method: "DELETE" });
    expect(revokeAll.status).toBe(200);
    await expect(revokeAll.json()).resolves.toEqual({ revoked: 1 });
    expect((await fixture.request("/v1/leases", deviceRequest(second.token))).status).toBe(401);
  });

  it("expires device tokens and removes their stored verifier", async () => {
    vi.useFakeTimers({ now: new Date("2026-07-27T12:00:00Z") });
    const fixture = pairingFixture();
    const { token } = await fixture.pair();
    await vi.advanceTimersByTimeAsync(deviceTokenTTLSeconds * 1000 + 1);

    const expired = await fixture.request("/v1/leases", deviceRequest(token));
    expect(expired.status).toBe(401);
    await expect(expired.json()).resolves.toEqual({ error: "device_token_expired" });
    expect(fixture.runtime.storage.serialized()).not.toContain("device-token:");
  });

  it.each([
    ["create lease", "/v1/leases", "POST"],
    ["release lease", "/v1/leases/cbx_000000000001/release", "POST"],
    ["change sharing", "/v1/leases/cbx_000000000001/share", "PUT"],
    ["read admin data", "/v1/admin/leases", "GET"],
    ["read other APIs", "/v1/whoami", "GET"],
    [
      "request authoritative metadata",
      "/v1/leases/cbx_000000000001?providerMetadata=authoritative",
      "GET",
    ],
  ])("returns 403 when a device token attempts to %s", async (_label, path, method) => {
    const fixture = pairingFixture();
    const { token } = await fixture.pair();
    const response = await fixture.request(path, deviceRequest(token, method));
    expect(response.status).toBe(403);
    await expect(response.json()).resolves.toEqual({ error: "device_scope_forbidden" });
  });

  it("rejects forged and non-canonical device credentials", async () => {
    const fixture = pairingFixture();
    const { token } = await fixture.pair();
    const [prefix, secret] = token.split(".") as [string, string];
    const deviceID = prefix.slice("cbxd_".length);
    const forgeries = [
      `${prefix}.${secret.slice(0, -1)}${secret.endsWith("A") ? "B" : "A"}`,
      `cbxd_${crypto.randomUUID()}.${secret}`,
      `${token}.ignored`,
      `cbxd_${deviceID.toUpperCase()}.${secret}`,
    ];

    const responses = await Promise.all(
      forgeries.map((forgery) => fixture.request("/v1/leases", deviceRequest(forgery))),
    );
    await Promise.all(
      responses.map(async (response) => {
        expect(response.status).toBe(401);
        await expect(response.json()).resolves.toEqual({ error: "device_token_invalid" });
      }),
    );
  });

  it("requires the exact configured origin without redirecting credentials", async () => {
    const fixture = pairingFixture();
    const missingOrigin = await fixture.ownerRequest(
      "/v1/pairing/grants",
      {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: "{}",
      },
      "",
    );
    expect(missingOrigin.status).toBe(403);

    const wrongBrowserOrigin = await fixture.ownerRequest(
      "/v1/pairing/grants",
      { method: "POST", headers: { "content-type": "application/json" }, body: "{}" },
      "https://evil.example.test",
    );
    expect(wrongBrowserOrigin.status).toBe(403);

    const grant = await issueGrant(fixture);
    const wrongDestination = await fixture.request(
      "/v1/pairing/exchange",
      {
        method: "POST",
        headers: { "content-type": "application/json", origin },
        body: JSON.stringify({ grant }),
      },
      origin,
      "https://alias.example.test",
    );
    expect(wrongDestination.status).toBe(403);
    expect(wrongDestination.headers.get("location")).toBeNull();

    const { token } = await fixture.pair();
    const wrongAPIOrigin = await fixture.request(
      "/v1/leases",
      deviceRequest(token, "GET", "https://evil.example.test"),
    );
    expect(wrongAPIOrigin.status).toBe(403);
  });

  it("fails closed after lease sharing changes and isolates device management by owner", async () => {
    const fixture = pairingFixture();
    const shared = lease("cbx_000000000001", "bob@example.com", {
      users: { [owner]: "use" },
    });
    await seedLease(fixture.runtime, shared);
    const { deviceID, token } = await fixture.pair();
    expect((await fixture.request(`/v1/leases/${shared.id}`, deviceRequest(token))).status).toBe(
      200,
    );

    shared.share = { users: {} };
    await seedLease(fixture.runtime, shared);
    expect((await fixture.request(`/v1/leases/${shared.id}`, deviceRequest(token))).status).toBe(
      404,
    );

    const bob = pairingFixture(fixture.runtime, "bob@example.com");
    const bobDevices = await bob.ownerRequest("/v1/devices");
    await expect(bobDevices.json()).resolves.toEqual({ devices: [] });
    expect((await bob.ownerRequest(`/v1/devices/${deviceID}`, { method: "DELETE" })).status).toBe(
      404,
    );
    expect((await fixture.request("/v1/leases", deviceRequest(token))).status).toBe(200);
  });

  it("requires a non-admin owner session to issue pairing grants", async () => {
    const fixture = pairingFixture();
    const anonymous = await fixture.request("/v1/pairing/grants", jsonPost({}));
    expect(anonymous.status).toBe(401);

    const admin = await fixture.request("/v1/pairing/grants", {
      ...jsonPost({}),
      headers: {
        ...jsonPost({}).headers,
        authorization: "Bearer admin-token",
        origin,
      },
    });
    expect(admin.status).toBe(403);
    await expect(admin.json()).resolves.toEqual({ error: "owner_session_required" });
  });
});

function pairingFixture(
  existingRuntime = new MemoryRuntime(),
  sharedOwner = owner,
): PairingFixture {
  const env = {
    CRABBOX_PUBLIC_URL: origin,
    CRABBOX_SHARED_TOKEN: "owner-token",
    CRABBOX_SHARED_OWNER: sharedOwner,
    CRABBOX_ADMIN_TOKEN: "admin-token",
    CRABBOX_DEFAULT_ORG: org,
  } as Env;
  const fleet = new FleetCoordinator(existingRuntime, env);
  const request = async (
    path: string,
    init: RequestInit = {},
    requestOrigin = origin,
    destinationOrigin = origin,
  ): Promise<Response> =>
    await routeCoordinatorRequest(
      new Request(`${destinationOrigin}${path}`, init),
      env,
      async (prepared) => await fleet.fetch(prepared),
    );
  const ownerRequest = async (
    path: string,
    init: RequestInit = {},
    requestOrigin = origin,
  ): Promise<Response> => {
    const headers = new Headers(init.headers);
    headers.set("authorization", "Bearer owner-token");
    if (requestOrigin) headers.set("origin", requestOrigin);
    return await request(path, { ...init, headers });
  };
  const fixture: PairingFixture = {
    env,
    runtime: existingRuntime,
    request,
    ownerRequest,
    async pair(name?: string) {
      const grant = await issueGrant(fixture, name);
      const response = await exchangeGrant(fixture, grant);
      expect(response.status).toBe(200);
      expect(response.headers.get("cache-control")).toBe("no-store");
      const body = (await response.json()) as { device: { id: string }; token: string };
      expect(body.token).toMatch(/^cbxd_/);
      return { deviceID: body.device.id, grant, token: body.token };
    },
  };
  return fixture;
}

async function issueGrant(fixture: PairingFixture, name?: string): Promise<string> {
  const response = await fixture.ownerRequest("/v1/pairing/grants", jsonPost({ name }));
  expect(response.status).toBe(200);
  expect(response.headers.get("cache-control")).toBe("no-store");
  const body = (await response.json()) as { grant: string };
  expect(body.grant).toMatch(/^cbxp_/);
  return body.grant;
}

async function exchangeGrant(fixture: PairingFixture, grant: string): Promise<Response> {
  return await fixture.request("/v1/pairing/exchange", jsonPost({ grant }));
}

function jsonPost(body: unknown): RequestInit {
  return {
    method: "POST",
    headers: { "content-type": "application/json", origin },
    body: JSON.stringify(body),
  };
}

function deviceRequest(token: string, method = "GET", requestOrigin = origin): RequestInit {
  return { method, headers: { authorization: `Bearer ${token}`, origin: requestOrigin } };
}

async function seedLease(runtime: MemoryRuntime, value: LeaseRecord): Promise<void> {
  await runtime.storage.put(`lease:${value.id}`, value);
}

function lease(id: string, leaseOwner: string, share?: LeaseRecord["share"]): LeaseRecord {
  const now = new Date().toISOString();
  return {
    id,
    provider: "external",
    lifecycle: "registered",
    target: "linux",
    cloudID: `external-${id}`,
    owner: leaseOwner,
    org: orgKeyForLabel(org),
    ...(share ? { share } : {}),
    profile: "default",
    class: "default",
    serverType: "external",
    serverID: 0,
    serverName: id,
    providerKey: `external-${id}`,
    host: "127.0.0.1",
    sshUser: "crabbox",
    sshPort: "22",
    workRoot: "/work/crabbox",
    keep: true,
    ttlSeconds: 3600,
    estimatedHourlyUSD: 0,
    maxEstimatedUSD: 0,
    state: "active",
    createdAt: now,
    updatedAt: now,
    expiresAt: new Date(Date.now() + 3600_000).toISOString(),
  };
}
