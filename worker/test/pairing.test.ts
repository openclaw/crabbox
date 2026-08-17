import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { issuePortalToken, issueUserToken, type AuthRequestContext } from "../src/auth";
import { routeCoordinatorRequest } from "../src/coordinator-entry";
import {
  type CoordinatorRuntime,
  type CoordinatorSocketHandlers,
  type CoordinatorStorage,
  type CoordinatorStorageView,
  type CoordinatorWebSocketUpgradeOptions,
} from "../src/coordinator-runtime";
import { deviceMembershipCacheTTLMS, FleetCoordinator } from "../src/fleet";
import { GitHubCredentialError } from "../src/github-membership";
import { orgKeyForLabel } from "../src/org-identity";
import {
  deviceTokenTTLSeconds,
  deviceTokenKey,
  maxDeviceTokensPerOwner,
  maxPairingGrantsPerOwner,
  pairingGrantTTLSeconds,
  type DeviceTokenRecord,
} from "../src/pairing";
import type { Env, LeaseRecord } from "../src/types";

const origin = "https://coordinator.example.test";
const owner = "github:12345";
const ownerLogin = "alice";
const otherOwner = "github:67890";
const org = "example-org";

class MemoryStorage implements CoordinatorStorage {
  private readonly values = new Map<string, unknown>();
  deleteFailureKey?: string;
  writes = 0;
  readonly listPrefixes: string[] = [];

  get<T>(key: string): Promise<T | undefined> {
    return Promise.resolve(this.values.get(key) as T | undefined);
  }

  put<T>(key: string, value: T): Promise<void> {
    this.writes += 1;
    this.values.set(key, value);
    return Promise.resolve();
  }

  delete(key: string): Promise<void> {
    this.writes += 1;
    if (key === this.deleteFailureKey) return Promise.reject(new Error("injected delete failure"));
    this.values.delete(key);
    return Promise.resolve();
  }

  list<T>({
    prefix = "",
    limit,
    startAfter,
  }: {
    prefix?: string;
    limit?: number;
    startAfter?: string;
    noCache?: boolean;
  } = {}): Promise<Map<string, T>> {
    this.listPrefixes.push(prefix);
    const entries = [...this.values]
      .toSorted(([left], [right]) => left.localeCompare(right))
      .filter(([key]) => key.startsWith(prefix) && (!startAfter || key > startAfter));
    return Promise.resolve(
      new Map(
        (limit === undefined ? entries : entries.slice(0, limit)).map(([key, value]) => [
          key,
          value as T,
        ]),
      ),
    );
  }

  take<T>(key: string): Promise<T | undefined> {
    this.writes += 1;
    const value = this.values.get(key) as T | undefined;
    this.values.delete(key);
    return Promise.resolve(value);
  }

  transaction<T>(callback: (transaction: CoordinatorStorageView) => Promise<T>): Promise<T> {
    return callback(this);
  }

  resetObservations(): void {
    this.writes = 0;
    this.listPrefixes.length = 0;
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
  membershipAllowed: { value: boolean };
  membershipFailure: { value?: Error };
  membershipChecks: ReturnType<typeof vi.fn>;
  request(
    path: string,
    init?: RequestInit,
    requestOrigin?: string,
    destinationOrigin?: string,
  ): Promise<Response>;
  ownerRequest(path: string, init?: RequestInit, requestOrigin?: string): Promise<Response>;
  bearerRequest(path: string, init?: RequestInit): Promise<Response>;
  userBearerRequest(path: string, init?: RequestInit): Promise<Response>;
  portalBearerRequest(path: string, init?: RequestInit): Promise<Response>;
  userCookieRequest(path: string, init?: RequestInit): Promise<Response>;
  pair(name?: string): Promise<{ deviceID: string; grant: string; token: string }>;
}

describe("coordinator device pairing", () => {
  beforeEach(() => {
    vi.useRealTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("exchanges a browser grant for a distinct credential-free device principal", async () => {
    const fixture = await pairingFixture();
    await seedLease(fixture.runtime, lease("cbx_000000000001", owner));
    await seedLease(
      fixture.runtime,
      lease("cbx_000000000002", otherOwner, { users: { [owner]: "use" } }),
    );
    await seedLease(fixture.runtime, lease("cbx_000000000003", "github:99999"));

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
    for (const item of listBody.leases) {
      expect(item).toMatchObject({
        host: "<redacted>",
        sshUser: "<redacted>",
        sshPort: "<redacted>",
        workRoot: "<redacted>",
      });
      expect(item).not.toHaveProperty("sshHostKey");
      expect(item).not.toHaveProperty("providerAccessExpiresAt");
      expect(item).not.toHaveProperty("tailscale");
    }

    const inspect = await fixture.request("/v1/leases/cbx_000000000002", deviceRequest(token));
    expect(inspect.status).toBe(200);
    await expect(inspect.json()).resolves.toMatchObject({
      lease: {
        id: "cbx_000000000002",
        owner: otherOwner,
        host: "<redacted>",
        sshUser: "<redacted>",
      },
    });

    fixture.runtime.storage.resetObservations();
    const devices = await fixture.ownerRequest("/v1/devices", {}, "");
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
    expect(fixture.runtime.storage.listPrefixes).toContainEqual(
      expect.stringMatching(/^device-owner:/),
    );
    expect(fixture.runtime.storage.listPrefixes).not.toContain("device-token:");
  });

  it("never refreshes provider access or writes storage during device detail reads", async () => {
    const fixture = await pairingFixture();
    const managed = lease("cbx_000000000004", owner);
    managed.provider = "daytona";
    managed.lifecycle = "managed";
    managed.host = "ssh.app.daytona.io";
    managed.sshUser = "live-provider-access-token";
    managed.sshPort = "22";
    managed.sshFallbackPorts = ["2222"];
    managed.sshHostKey = "SHA256:host-key";
    managed.providerAccessExpiresAt = new Date(Date.now() + 60_000).toISOString();
    managed.tailscale = { enabled: true, ipv4: "100.64.0.1", state: "ready" };
    await seedLease(fixture.runtime, managed);
    const { token } = await fixture.pair();
    const providerFetch = vi
      .spyOn(globalThis, "fetch")
      .mockRejectedValue(new Error("provider called"));
    fixture.runtime.storage.resetObservations();

    const response = await fixture.request(`/v1/leases/${managed.id}`, deviceRequest(token));

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      lease: {
        host: "<redacted>",
        sshUser: "<redacted>",
        sshPort: "<redacted>",
        workRoot: "<redacted>",
      },
    });
    expect(providerFetch).not.toHaveBeenCalled();
    expect(fixture.runtime.storage.writes).toBe(0);
  });

  it("expires grants, consumes successful grants once, and rejects replay", async () => {
    vi.useFakeTimers({ now: new Date("2026-07-27T12:00:00Z") });
    const fixture = await pairingFixture();
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
    const fixture = await pairingFixture();
    await seedLease(fixture.runtime, lease("cbx_000000000001", owner));
    const first = await fixture.pair("First phone");
    const second = await fixture.pair("Second phone");
    expect((await fixture.request("/v1/leases", deviceRequest(first.token))).status).toBe(200);

    const revokeOne = await fixture.ownerRequest(`/v1/devices/${first.deviceID}`, {
      method: "DELETE",
    });
    expect(revokeOne.status).toBe(204);
    const revoked = await fixture.request("/v1/leases", deviceRequest(first.token));
    expect(revoked.status).toBe(401);
    await expect(revoked.json()).resolves.toEqual({ error: "device_token_invalid" });
    expect((await fixture.request("/v1/leases", deviceRequest(second.token))).status).toBe(200);

    const revokeAll = await fixture.ownerRequest("/v1/devices", { method: "DELETE" });
    expect(revokeAll.status).toBe(200);
    await expect(revokeAll.json()).resolves.toEqual({ revoked: 1 });
    expect((await fixture.request("/v1/leases", deviceRequest(second.token))).status).toBe(401);
  });

  it("keeps a device indexed when verifier deletion fails", async () => {
    const fixture = await pairingFixture();
    const { deviceID, token } = await fixture.pair();
    fixture.runtime.storage.deleteFailureKey = `device-token:${deviceID}`;

    const failed = await fixture.ownerRequest(`/v1/devices/${deviceID}`, { method: "DELETE" });
    expect(failed.status).toBe(500);
    expect(fixture.runtime.storage.serialized()).toContain(`device-token:${deviceID}`);
    expect(fixture.runtime.storage.serialized()).toContain("device-owner:");

    fixture.runtime.storage.deleteFailureKey = undefined;
    expect(
      (await fixture.ownerRequest(`/v1/devices/${deviceID}`, { method: "DELETE" })).status,
    ).toBe(204);
    expect((await fixture.request("/v1/leases", deviceRequest(token))).status).toBe(401);
  });

  it("expires device tokens without mutating storage from the device request", async () => {
    vi.useFakeTimers({ now: new Date("2026-07-27T12:00:00Z") });
    const fixture = await pairingFixture();
    const { token } = await fixture.pair();
    await vi.advanceTimersByTimeAsync(deviceTokenTTLSeconds * 1000 + 1);
    fixture.runtime.storage.resetObservations();

    const expired = await fixture.request("/v1/leases", deviceRequest(token));
    expect(expired.status).toBe(401);
    await expect(expired.json()).resolves.toEqual({ error: "device_token_expired" });
    expect(fixture.runtime.storage.writes).toBe(0);
    expect(fixture.runtime.storage.serialized()).toContain("device-token:");
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
    const fixture = await pairingFixture();
    const { token } = await fixture.pair();
    const response = await fixture.request(path, deviceRequest(token, method));
    expect(response.status).toBe(403);
    await expect(response.json()).resolves.toEqual({ error: "device_scope_forbidden" });
  });

  it("rejects forged and non-canonical device credentials", async () => {
    const fixture = await pairingFixture();
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
    const fixture = await pairingFixture();
    const missingOrigin = await fixture.ownerRequest(
      "/v1/pairing/grants",
      { method: "POST", headers: { "content-type": "application/json" }, body: "{}" },
      "",
    );
    expect(missingOrigin.status).toBe(403);

    const wrongBrowserOrigin = await fixture.ownerRequest(
      "/v1/pairing/grants",
      { method: "POST", headers: { "content-type": "application/json" }, body: "{}" },
      "https://evil.example.test",
    );
    expect(wrongBrowserOrigin.status).toBe(403);
    const wrongDeviceListOrigin = await fixture.ownerRequest(
      "/v1/devices",
      {},
      "https://evil.example.test",
    );
    expect(wrongDeviceListOrigin.status).toBe(403);

    const grant = await issueGrant(fixture);
    const wrongDestination = await fixture.request(
      "/v1/pairing/exchange",
      jsonPost({ grant }),
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

  it("fails closed after membership or lease sharing changes", async () => {
    vi.useFakeTimers({ now: new Date("2026-07-27T12:00:00Z") });
    const fixture = await pairingFixture();
    const shared = lease("cbx_000000000001", otherOwner, { users: { [owner]: "use" } });
    await seedLease(fixture.runtime, shared);
    const { token } = await fixture.pair();
    expect((await fixture.request(`/v1/leases/${shared.id}`, deviceRequest(token))).status).toBe(
      200,
    );

    shared.share = { users: {} };
    await seedLease(fixture.runtime, shared);
    expect((await fixture.request(`/v1/leases/${shared.id}`, deviceRequest(token))).status).toBe(
      404,
    );

    fixture.membershipAllowed.value = false;
    expect((await fixture.request("/v1/leases", deviceRequest(token))).status).toBe(200);
    await vi.advanceTimersByTimeAsync(deviceMembershipCacheTTLMS + 1);
    const offboarded = await fixture.request("/v1/leases", deviceRequest(token));
    expect(offboarded.status).toBe(401);
    await expect(offboarded.json()).resolves.toEqual({ error: "device_owner_unauthorized" });
  });

  it("caches successful membership checks for 60 seconds per device token", async () => {
    vi.useFakeTimers({ now: new Date("2026-07-27T12:00:00Z") });
    const fixture = await pairingFixture();
    const { token } = await fixture.pair();
    fixture.membershipChecks.mockClear();

    expect((await fixture.request("/v1/leases", deviceRequest(token))).status).toBe(200);
    expect((await fixture.request("/v1/leases", deviceRequest(token))).status).toBe(200);
    expect(fixture.membershipChecks).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(deviceMembershipCacheTTLMS + 1);
    expect((await fixture.request("/v1/leases", deviceRequest(token))).status).toBe(200);
    expect(fixture.membershipChecks).toHaveBeenCalledTimes(2);
  });

  it.each([
    ["malformed", "operators,"],
    ["qualified for another organization", "partner-org/contractors"],
  ])("invalidates a warm device membership when policy becomes %s", async (_label, teams) => {
    const fixture = await pairingFixture();
    fixture.env.CRABBOX_GITHUB_ALLOWED_TEAMS = "example-org/operators";
    const { token } = await fixture.pair();
    fixture.membershipChecks.mockClear();

    expect((await fixture.request("/v1/leases", deviceRequest(token))).status).toBe(200);
    expect(fixture.membershipChecks).toHaveBeenCalledTimes(1);

    fixture.env.CRABBOX_GITHUB_ALLOWED_TEAMS = teams;
    const denied = await fixture.request("/v1/leases", deviceRequest(token));

    expect(denied.status).toBe(401);
    await expect(denied.json()).resolves.toEqual({ error: "device_owner_unauthorized" });
    expect(fixture.membershipChecks).toHaveBeenCalledTimes(1);
  });

  it("does not reuse a warm device proof after changing valid team A to team B", async () => {
    const fixture = await pairingFixture();
    fixture.env.CRABBOX_GITHUB_ALLOWED_TEAMS = "example-org/operators";
    const { token } = await fixture.pair();
    fixture.membershipChecks.mockClear();

    expect((await fixture.request("/v1/leases", deviceRequest(token))).status).toBe(200);
    expect(fixture.membershipChecks).toHaveBeenCalledTimes(1);

    fixture.env.CRABBOX_GITHUB_ALLOWED_TEAMS = "example-org/release-captains";
    fixture.membershipAllowed.value = false;
    const denied = await fixture.request("/v1/leases", deviceRequest(token));

    expect(denied.status).toBe(401);
    await expect(denied.json()).resolves.toEqual({ error: "device_owner_unauthorized" });
    expect(fixture.membershipChecks).toHaveBeenCalledTimes(2);
  });

  it("fails closed on a lookup error after a warm membership entry expires", async () => {
    vi.useFakeTimers({ now: new Date("2026-07-27T12:00:00Z") });
    const fixture = await pairingFixture();
    const { token } = await fixture.pair();
    fixture.membershipChecks.mockClear();
    expect((await fixture.request("/v1/leases", deviceRequest(token))).status).toBe(200);

    fixture.membershipFailure.value = new Error("GitHub unavailable");
    await vi.advanceTimersByTimeAsync(deviceMembershipCacheTTLMS + 1);
    const failed = await fixture.request("/v1/leases", deviceRequest(token));

    expect(failed.status).toBe(401);
    await expect(failed.json()).resolves.toEqual({ error: "device_owner_unauthorized" });
    expect(fixture.membershipChecks).toHaveBeenCalledTimes(2);
  });

  it("returns a distinct reauthentication error for expired or revoked OAuth grants", async () => {
    const expiredFixture = await pairingFixture();
    const expired = await expiredFixture.pair();
    const stored = await expiredFixture.runtime.storage.get<DeviceTokenRecord>(
      deviceTokenKey(expired.deviceID),
    );
    if (!stored) throw new Error("device record was not stored");
    await expiredFixture.runtime.storage.put(deviceTokenKey(expired.deviceID), {
      ...stored,
      ownerGrant: { ...stored.ownerGrant, expiresAt: new Date(Date.now() - 1).toISOString() },
    });

    const expiredResponse = await expiredFixture.request(
      "/v1/leases",
      deviceRequest(expired.token),
    );
    expect(expiredResponse.status).toBe(401);
    await expect(expiredResponse.json()).resolves.toEqual({
      error: "pairing_reauth_required",
    });

    const revokedFixture = await pairingFixture();
    const revoked = await revokedFixture.pair();
    revokedFixture.membershipFailure.value = new GitHubCredentialError("Bad credentials");
    const revokedResponse = await revokedFixture.request(
      "/v1/leases",
      deviceRequest(revoked.token),
    );
    expect(revokedResponse.status).toBe(401);
    await expect(revokedResponse.json()).resolves.toEqual({
      error: "pairing_reauth_required",
    });
  });

  it.each([
    {
      label: "SAML authorization required",
      endpoint: "membership" as const,
      githubResponse: () =>
        Response.json(
          {
            message:
              "Resource protected by organization SAML enforcement. You must grant your OAuth token access to this organization.",
            documentation_url:
              "https://docs.github.com/rest/authentication/authenticating-to-the-rest-api#saml-sso-authentication",
          },
          {
            status: 403,
            headers: { "x-github-sso": "required; url=https://github.com/orgs/example-org/sso" },
          },
        ),
      expectedError: "pairing_reauth_required",
    },
    {
      label: "revoked credential",
      endpoint: "user" as const,
      githubResponse: () =>
        Response.json(
          {
            message: "Bad credentials",
            documentation_url:
              "https://docs.github.com/rest/authentication/authenticating-to-the-rest-api",
          },
          { status: 403 },
        ),
      expectedError: "pairing_reauth_required",
    },
    {
      label: "genuine non-member",
      endpoint: "membership" as const,
      githubResponse: () =>
        Response.json(
          {
            message: "You are not an active member of this organization.",
            documentation_url:
              "https://docs.github.com/rest/orgs/members#get-organization-membership-for-a-user",
          },
          { status: 403 },
        ),
      expectedError: "device_owner_unauthorized",
    },
    {
      label: "unrecognized forbidden response",
      endpoint: "user" as const,
      githubResponse: () =>
        Response.json(
          { message: "Forbidden", documentation_url: "https://docs.github.com/rest" },
          {
            status: 403,
            headers: { "x-github-sso": "partial-results; organizations=12345" },
          },
        ),
      expectedError: "device_owner_unauthorized",
    },
  ])("classifies a GitHub 403 for $label", async ({ endpoint, githubResponse, expectedError }) => {
    const failure: {
      endpoint?: "user" | "membership";
      response?: () => Response;
    } = {};
    vi.stubGlobal(
      "fetch",
      vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
        const url = String(input);
        if (failure.endpoint === "user" && url === "https://api.github.com/user") {
          return failure.response!();
        }
        if (
          failure.endpoint === "membership" &&
          url === "https://api.github.com/user/memberships/orgs/example-org"
        ) {
          return failure.response!();
        }
        if (url === "https://api.github.com/user") {
          return Response.json({ id: 12345, login: ownerLogin });
        }
        if (url === "https://api.github.com/user/memberships/orgs/example-org") {
          return Response.json({ state: "active", organization: { login: org } });
        }
        throw new Error(`unexpected GitHub request ${url}`);
      }),
    );
    const fixture = await pairingFixture(new MemoryRuntime(), owner, ownerLogin, {});
    const { token } = await fixture.pair();
    failure.endpoint = endpoint;
    failure.response = githubResponse;

    const response = await fixture.request("/v1/leases", deviceRequest(token));

    expect(response.status).toBe(401);
    await expect(response.json()).resolves.toEqual({ error: expectedError });
  });

  it("does not share cached membership between device tokens", async () => {
    const fixture = await pairingFixture();
    const first = await fixture.pair("First phone");
    const second = await fixture.pair("Second phone");
    fixture.membershipChecks.mockClear();

    expect((await fixture.request("/v1/leases", deviceRequest(first.token))).status).toBe(200);
    fixture.membershipAllowed.value = false;
    const secondResponse = await fixture.request("/v1/leases", deviceRequest(second.token));

    expect(secondResponse.status).toBe(401);
    await expect(secondResponse.json()).resolves.toEqual({ error: "device_owner_unauthorized" });
    expect(fixture.membershipChecks).toHaveBeenCalledTimes(2);
  });

  it("isolates device management indexes by owner", async () => {
    const fixture = await pairingFixture();
    const { deviceID, token } = await fixture.pair();
    const bob = await pairingFixture(fixture.runtime, otherOwner, "bob");

    const bobDevices = await bob.ownerRequest("/v1/devices");
    await expect(bobDevices.json()).resolves.toEqual({ devices: [] });
    expect((await bob.ownerRequest(`/v1/devices/${deviceID}`, { method: "DELETE" })).status).toBe(
      404,
    );
    expect((await fixture.request("/v1/leases", deviceRequest(token))).status).toBe(200);
  });

  it("requires a GitHub browser session rather than any API bearer", async () => {
    const fixture = await pairingFixture();
    const anonymous = await fixture.request("/v1/pairing/grants", jsonPost({}));
    expect(anonymous.status).toBe(401);

    const shared = await fixture.bearerRequest("/v1/pairing/grants", jsonPost({}));
    expect(shared.status).toBe(403);
    await expect(shared.json()).resolves.toEqual({ error: "owner_session_required" });

    const signedBearer = await fixture.userBearerRequest("/v1/pairing/grants", jsonPost({}));
    expect(signedBearer.status).toBe(403);
    await expect(signedBearer.json()).resolves.toEqual({ error: "owner_session_required" });

    const copiedCookie = await fixture.userCookieRequest("/v1/pairing/grants", jsonPost({}));
    expect(copiedCookie.status).toBe(403);
    await expect(copiedCookie.json()).resolves.toEqual({ error: "owner_session_required" });

    const portalBearer = await fixture.portalBearerRequest("/v1/leases");
    expect(portalBearer.status).toBe(401);

    const admin = await fixture.request("/v1/pairing/grants", {
      ...jsonPost({}),
      headers: { "content-type": "application/json", authorization: "Bearer admin-token", origin },
    });
    expect(admin.status).toBe(403);
    await expect(admin.json()).resolves.toEqual({ error: "owner_session_required" });
  });

  it("preserves configured bearer precedence for device-shaped static tokens", async () => {
    const fixture = await pairingFixture();
    const configured = `cbxd_${crypto.randomUUID()}.${"A".repeat(43)}`;
    fixture.env.CRABBOX_SHARED_TOKEN = configured;

    const response = await fixture.request("/v1/leases", deviceRequest(configured));

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toEqual({ leases: [] });
  });

  it("ignores forged fleet-auth headers on device-shaped credentials", async () => {
    const fixture = await pairingFixture();
    const forged = `cbxd_${crypto.randomUUID()}.${"B".repeat(43)}`;

    const response = await fixture.request("/v1/leases", {
      headers: {
        authorization: `Bearer ${forged}`,
        origin,
        "x-crabbox-auth": "bearer",
        "x-crabbox-owner": owner,
        "x-crabbox-org": org,
      },
    });

    expect(response.status).toBe(401);
    await expect(response.json()).resolves.toEqual({ error: "device_token_invalid" });
  });

  it("caps active devices per owner", async () => {
    const fixture = await pairingFixture();
    for (let index = 0; index < maxDeviceTokensPerOwner - 1; index += 1) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- each pairing consumes its one-use grant.
      await fixture.pair(`Device ${index + 1}`);
    }

    const grants = await Promise.all([
      issueGrant(fixture, "Concurrent device A"),
      issueGrant(fixture, "Concurrent device B"),
    ]);
    const exchanges = await Promise.all(grants.map((grant) => exchangeGrant(fixture, grant)));
    expect(exchanges.map((response) => response.status).toSorted()).toEqual([200, 409]);
    const rejected = exchanges.find((response) => response.status === 409)!;
    await expect(rejected.json()).resolves.toEqual({
      error: "device_limit_reached",
      limit: maxDeviceTokensPerOwner,
    });
    const devices = await fixture.ownerRequest("/v1/devices");
    const body = (await devices.json()) as { devices: unknown[] };
    expect(body.devices).toHaveLength(maxDeviceTokensPerOwner);
  });

  it("bounds active pairing grants per owner", async () => {
    const fixture = await pairingFixture();
    const responses = await Promise.all(
      Array.from({ length: maxPairingGrantsPerOwner + 1 }, (_, index) =>
        fixture.ownerRequest(
          "/v1/pairing/grants",
          jsonPost({ name: `Concurrent pending device ${index + 1}` }),
        ),
      ),
    );
    expect(responses.map((response) => response.status).toSorted()).toEqual([
      ...Array.from({ length: maxPairingGrantsPerOwner }, () => 200),
      429,
    ]);
    const rejected = responses.find((response) => response.status === 429)!;
    expect(rejected.status).toBe(429);
    await expect(rejected.json()).resolves.toEqual({ error: "pairing_grant_limit_reached" });
  });
});

async function pairingFixture(
  existingRuntime = new MemoryRuntime(),
  sessionOwner = owner,
  login = ownerLogin,
  authContextOverride?: AuthRequestContext,
): Promise<PairingFixture> {
  const env = {
    CRABBOX_PUBLIC_URL: origin,
    CRABBOX_SHARED_TOKEN: "shared-token",
    CRABBOX_SHARED_OWNER: "automation@example.com",
    CRABBOX_ADMIN_TOKEN: "admin-token",
    CRABBOX_SESSION_SECRET: "session-secret",
    CRABBOX_DEFAULT_ORG: org,
    CRABBOX_GITHUB_ALLOWED_ORG: org,
    CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "0",
  } as Env;
  const membershipAllowed = { value: true };
  const membershipFailure: { value?: Error } = {};
  const membershipChecks = vi.fn<() => Promise<void>>(async (): Promise<void> => {
    if (membershipFailure.value) throw membershipFailure.value;
    if (!membershipAllowed.value) throw new Error("membership removed");
  });
  const authContext = authContextOverride ?? { githubMembership: membershipChecks };
  const tokenInput = {
    owner: sessionOwner,
    ownerSource: "github-verified-email" as const,
    org,
    login,
    githubAccessToken: `github-access-token-${sessionOwner}`,
  };
  const [userToken, portalToken] = await Promise.all([
    issueUserToken(env, tokenInput),
    issuePortalToken(env, tokenInput),
  ]);
  const fleet = new FleetCoordinator(existingRuntime, env, {}, authContext);
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
      authContext,
    );
  const ownerRequest = async (
    path: string,
    init: RequestInit = {},
    requestOrigin = origin,
  ): Promise<Response> => {
    const headers = new Headers(init.headers);
    headers.set("cookie", `__Host-crabbox_session=${encodeURIComponent(portalToken)}`);
    if (requestOrigin) headers.set("origin", requestOrigin);
    return await request(path, { ...init, headers });
  };
  const withBearer = async (token: string, path: string, init: RequestInit = {}) => {
    const headers = new Headers(init.headers);
    headers.set("authorization", `Bearer ${token}`);
    headers.set("origin", origin);
    return await request(path, { ...init, headers });
  };
  const fixture: PairingFixture = {
    env,
    runtime: existingRuntime,
    membershipAllowed,
    membershipFailure,
    membershipChecks,
    request,
    ownerRequest,
    bearerRequest: async (path, init) => await withBearer("shared-token", path, init),
    userBearerRequest: async (path, init) => await withBearer(userToken, path, init),
    portalBearerRequest: async (path, init) => await withBearer(portalToken, path, init),
    userCookieRequest: async (path, init = {}) => {
      const headers = new Headers(init.headers);
      headers.set("cookie", `__Host-crabbox_session=${encodeURIComponent(userToken)}`);
      headers.set("origin", origin);
      return await request(path, { ...init, headers });
    },
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
    sshFallbackPorts: ["2222"],
    sshHostKey: "SHA256:test-host-key",
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
