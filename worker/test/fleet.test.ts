import { Script, createContext } from "node:vm";

import { afterEach, describe, expect, it, vi } from "vitest";

import type { LeaseConfig } from "../src/config";
import {
  FleetDurableObject,
  bridgeTicketFromRequest,
  codeForwardHeaders,
  codeResponseHeaders,
  flushPendingWebVNC,
  forwardOrBufferWebVNC,
  resetWebVNCBridge,
  shouldActivateEgressSession,
  type WebVNCBuffer,
} from "../src/fleet";
import { portalCode } from "../src/portal";
import type {
  Env,
  ExternalRunnerRecord,
  LeaseRecord,
  ProvisioningAttempt,
  RunRecord,
} from "../src/types";

afterEach(() => {
  vi.unstubAllGlobals();
});

class MemoryStorage {
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

  async deleteAlarm(): Promise<void> {}

  async setAlarm(_time: number): Promise<void> {}

  async list<T>({ prefix = "" }: { prefix?: string } = {}): Promise<Map<string, T>> {
    const matches = new Map<string, T>();
    for (const [key, value] of this.values) {
      if (key.startsWith(prefix)) {
        matches.set(key, value as T);
      }
    }
    return matches;
  }

  seed<T>(key: string, value: T): void {
    this.values.set(key, value);
  }

  value<T>(key: string): T | undefined {
    return this.values.get(key) as T | undefined;
  }
}

class FakeWebSocket {
  readyState = WebSocket.OPEN;
  private attachment: unknown;
  private readonly sent: string[] = [];

  constructor(attachment?: unknown) {
    this.attachment = attachment;
  }

  send(data: string): void {
    this.sent.push(data);
  }

  close(): void {
    this.readyState = WebSocket.CLOSED;
  }

  accept(): void {}

  addEventListener(): void {}

  serializeAttachment(attachment: unknown): void {
    this.attachment = attachment;
  }

  deserializeAttachment(): unknown {
    return this.attachment;
  }

  sentJSON(): unknown[] {
    return this.sent.map((value) => JSON.parse(value) as unknown);
  }
}

describe("fleet lease identity and idle", () => {
  it("creates leases through the public route with slug and idle metadata", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      hetzner: fakeProvider(),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          slug: "Blue Lobster",
          provider: "hetzner",
          class: "standard",
          serverType: "cpx62",
          ttlSeconds: 1200,
          idleTimeoutSeconds: 360,
          keep: true,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    const { lease } = (await create.json()) as { lease: LeaseRecord };
    expect(lease.id).toBe("cbx_abcdef123456");
    expect(lease.slug).toBe("blue-lobster");
    expect(lease.idleTimeoutSeconds).toBe(360);
    expect(lease.ttlSeconds).toBe(1200);
    expect(lease.lastTouchedAt).toBeTruthy();
    expect(Date.parse(lease.expiresAt)).toBeGreaterThan(Date.parse(lease.createdAt));

    const bySlug = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(bySlug.status).toBe(200);
    const found = (await bySlug.json()) as { lease: LeaseRecord };
    expect(found.lease.id).toBe("cbx_abcdef123456");
    expect(found.lease.slug).toBe("blue-lobster");
  });

  it("shares leases with explicit users or the owning org", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const ownerHeaders = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    const friendHeaders = {
      "x-crabbox-owner": "friend@example.com",
      "x-crabbox-org": "openclaw",
    };
    const strangerHeaders = {
      "x-crabbox-owner": "stranger@example.com",
      "x-crabbox-org": "elsewhere",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        desktop: true,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const hidden = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster", { headers: friendHeaders }),
    );
    expect(hidden.status).toBe(404);

    const shared = await fleet.fetch(
      request("PUT", "/v1/leases/blue-lobster/share", {
        headers: ownerHeaders,
        body: { users: { "Friend@Example.com": "use" } },
      }),
    );
    expect(shared.status).toBe(200);
    await expect(shared.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      share: { users: { "friend@example.com": "use" } },
    });

    const friendLease = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster", { headers: friendHeaders }),
    );
    expect(friendLease.status).toBe(200);
    await expect(friendLease.json()).resolves.toMatchObject({
      lease: { id: "cbx_000000000001", share: { users: { "friend@example.com": "use" } } },
    });

    const friendTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/webvnc/ticket", {
        headers: friendHeaders,
        body: {},
      }),
    );
    expect(friendTicket.status).toBe(403);

    const ownerTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/webvnc/ticket", {
        headers: ownerHeaders,
        body: {},
      }),
    );
    expect(ownerTicket.status).toBe(200);

    const friendCodeTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/code/ticket", {
        headers: friendHeaders,
        body: {},
      }),
    );
    expect(friendCodeTicket.status).toBe(403);

    const friendEgressTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/egress/ticket", {
        headers: friendHeaders,
        body: { role: "host" },
      }),
    );
    expect(friendEgressTicket.status).toBe(403);

    const friendRelease = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/release", {
        headers: friendHeaders,
        body: {},
      }),
    );
    expect(friendRelease.status).toBe(403);

    const orgShared = await fleet.fetch(
      request("PUT", "/v1/leases/blue-lobster/share", {
        headers: ownerHeaders,
        body: { users: { "friend@example.com": "use" }, org: "manage" },
      }),
    );
    expect(orgShared.status).toBe(200);
    await expect(orgShared.json()).resolves.toMatchObject({
      share: { users: { "friend@example.com": "use" }, org: "manage" },
    });

    const friendSharePage = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/share", { headers: friendHeaders }),
    );
    expect(friendSharePage.status).toBe(200);
    const friendShareBody = await friendSharePage.text();
    expect(friendShareBody).toContain("share blue-lobster");
    expect(friendShareBody).toContain("share-shell");
    expect(friendShareBody).toContain("back to lease");
    expect(friendShareBody).toContain('class="button action" type="submit">save</button>');
    expect(friendShareBody).toContain('class="button action" type="submit">add</button>');

    const embeddedSharePage = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/share?embed=1", { headers: friendHeaders }),
    );
    expect(embeddedSharePage.status).toBe(200);
    expect(embeddedSharePage.headers.get("content-security-policy")).toContain(
      "frame-ancestors 'self'",
    );
    const embeddedShareBody = await embeddedSharePage.text();
    expect(embeddedShareBody).toContain("share-shell-embedded");
    expect(embeddedShareBody).toContain("/portal/leases/cbx_000000000001/share?embed=1");
    expect(embeddedShareBody).not.toContain("back to lease");

    const shareJSON = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/share?format=json", {
        headers: friendHeaders,
      }),
    );
    expect(shareJSON.status).toBe(200);
    await expect(shareJSON.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      slug: "blue-lobster",
      owner: "peter@example.com",
      org: "openclaw",
      share: { users: { "friend@example.com": "use" }, org: "manage" },
    });

    const shareJSONUpdate = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/share?format=json", {
        headers: friendHeaders,
        body: { users: { "Teammate@Example.com": "manage" }, org: "use" },
      }),
    );
    expect(shareJSONUpdate.status).toBe(200);
    await expect(shareJSONUpdate.json()).resolves.toMatchObject({
      share: { users: { "teammate@example.com": "manage" }, org: "use" },
    });

    const stranger = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster", { headers: strangerHeaders }),
    );
    expect(stranger.status).toBe(404);
  });

  it("mints brokered Tailscale keys, records non-secret metadata, and accepts readiness updates", async () => {
    const storage = new MemoryStorage();
    let providerConfig:
      | {
          tailscale?: boolean;
          tailscaleAuthKey?: string;
          tailscaleHostname?: string;
          tailscaleTags?: string[];
          tailscaleExitNode?: string;
          tailscaleExitNodeAllowLanAccess?: boolean;
        }
      | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "https://api.tailscale.com/api/v2/oauth/token") {
          return jsonResponse({ access_token: "oauth-token" });
        }
        if (url === "https://api.tailscale.com/api/v2/tailnet/-/keys") {
          return jsonResponse({ key: "tskey-oneoff" });
        }
        return jsonResponse({ message: `unexpected ${url}` }, 500);
      }),
    );
    const fleet = testFleet(
      storage,
      {
        hetzner: fakeProvider((config) => {
          providerConfig = config;
        }),
      },
      {
        CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
        CRABBOX_TAILSCALE_CLIENT_SECRET: "client-secret",
        CRABBOX_TAILSCALE_TAGS: "tag:crabbox,tag:ci",
      },
    );
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          slug: "Blue Lobster",
          provider: "hetzner",
          tailscale: true,
          tailscaleTags: ["tag:ci"],
          tailscaleHostname: "crabbox-{slug}",
          tailscaleExitNode: "mac-studio.tailnet.ts.net",
          tailscaleExitNodeAllowLanAccess: true,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    const { lease } = (await create.json()) as { lease: LeaseRecord };
    expect(lease.tailscale).toEqual({
      enabled: true,
      hostname: "crabbox-blue-lobster",
      tags: ["tag:ci"],
      state: "requested",
      exitNode: "mac-studio.tailnet.ts.net",
      exitNodeAllowLanAccess: true,
    });
    expect(JSON.stringify(lease)).not.toContain("tskey-oneoff");
    expect(providerConfig).toMatchObject({
      tailscale: true,
      tailscaleAuthKey: "tskey-oneoff",
      tailscaleHostname: "crabbox-blue-lobster",
      tailscaleTags: ["tag:ci"],
      tailscaleExitNode: "mac-studio.tailnet.ts.net",
      tailscaleExitNodeAllowLanAccess: true,
    });

    const update = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/tailscale", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          enabled: true,
          hostname: "crabbox-blue-lobster",
          fqdn: "crabbox-blue-lobster.example.ts.net",
          ipv4: "100.64.0.10",
          exitNode: "mac-studio.tailnet.ts.net",
          exitNodeAllowLanAccess: true,
          state: "ready",
        },
      }),
    );
    expect(update.status).toBe(200);
    const updated = (await update.json()) as { lease: LeaseRecord };
    expect(updated.lease.tailscale?.ipv4).toBe("100.64.0.10");
    expect(updated.lease.tailscale?.exitNode).toBe("mac-studio.tailnet.ts.net");
    expect(updated.lease.tailscale?.state).toBe("ready");
  });

  it("rejects brokered Tailscale tags outside the coordinator allowlist", async () => {
    const fleet = testFleet(
      new MemoryStorage(),
      { hetzner: fakeProvider() },
      {
        CRABBOX_TAILSCALE_CLIENT_ID: "client-id",
        CRABBOX_TAILSCALE_CLIENT_SECRET: "client-secret",
        CRABBOX_TAILSCALE_TAGS: "tag:crabbox",
      },
    );
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "hetzner",
          tailscale: true,
          tailscaleTags: ["tag:prod"],
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(400);
    await expect(create.json()).resolves.toMatchObject({
      error: "invalid_tailscale_tags",
      message: "tailscale tags not allowed: tag:prod",
    });
  });

  it("reports brokered Tailscale disabled when OAuth secrets are absent", async () => {
    const fleet = testFleet(new MemoryStorage(), { hetzner: fakeProvider() });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "hetzner",
          tailscale: true,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(403);
    await expect(create.json()).resolves.toMatchObject({
      error: "tailscale_disabled",
      message: "Tailscale is disabled for this coordinator",
    });
  });

  it("passes the Cloudflare request source IP as AWS SSH ingress CIDR", async () => {
    let awsCIDRs: string[] = [];
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider((config) => {
        awsCIDRs = config.awsSSHCIDRs;
      }),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          class: "standard",
          serverType: "c7a.8xlarge",
          ttlSeconds: 1200,
          idleTimeoutSeconds: 360,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    expect(awsCIDRs).toEqual(["203.0.113.7/32"]);
  });

  it("honors requested AWS SSH ingress CIDRs over request source IP", async () => {
    let awsCIDRs: string[] = [];
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider((config) => {
        awsCIDRs = config.awsSSHCIDRs;
      }),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "cf-connecting-ip": "203.0.113.7",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          class: "standard",
          serverType: "c7a.8xlarge",
          awsSSHCIDRs: ["198.51.100.0/24"],
          ttlSeconds: 1200,
          idleTimeoutSeconds: 360,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    expect(awsCIDRs).toEqual(["198.51.100.0/24"]);
  });

  it("records per-request GCP project without filling Worker-owned defaults", async () => {
    let seenConfig: LeaseConfig | undefined;
    const fleet = testFleet(new MemoryStorage(), {
      gcp: fakeProvider(
        (config) => {
          seenConfig = config;
        },
        {
          provider: "gcp",
          serverType: "c4-standard-192",
          cloudID: "crabbox-gcp-test",
        },
      ),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "gcp",
          gcpProject: "request-project",
          class: "beast",
          serverType: "c4-standard-192",
          ttlSeconds: 1200,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    expect(seenConfig?.gcpProject).toBe("request-project");
    expect(seenConfig?.gcpZone).toBe("");
    expect(seenConfig?.gcpImage).toBe("");
    expect(seenConfig?.gcpNetwork).toBe("");
    expect(seenConfig?.gcpTags).toEqual([]);
    expect(seenConfig?.gcpRootGB).toBe(0);
    const { lease } = (await create.json()) as { lease: LeaseRecord };
    expect(lease.providerProject).toBe("request-project");
    expect(lease.region).toBe("us-central1-a");
  });

  it("records requested type and provider fallback attempts on resolved leases", async () => {
    const attempts: ProvisioningAttempt[] = [
      {
        region: "eu-west-1",
        serverType: "c7a.48xlarge",
        market: "spot",
        category: "quota",
        message: "quota L-34B43A08 in eu-west-1 is 64 vCPUs; c7a.48xlarge needs 192 vCPUs",
      },
    ];
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(undefined, {
        provider: "aws",
        serverType: "c7i.24xlarge",
        cloudID: "i-123",
        market: "on-demand",
        attempts,
      }),
    });
    const create = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          ttlSeconds: 1200,
          idleTimeoutSeconds: 360,
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(create.status).toBe(201);
    const { lease } = (await create.json()) as { lease: LeaseRecord };
    expect(lease.requestedServerType).toBe("c7a.48xlarge");
    expect(lease.serverType).toBe("c7i.24xlarge");
    expect(lease.market).toBe("on-demand");
    expect(lease.provisioningAttempts).toEqual(attempts);
    expect(lease.capacityHints?.map((hint) => hint.code)).toEqual([
      "aws_capacity_routed",
      "aws_quota_pressure",
      "aws_on_demand_fallback",
      "capacity_large_class",
    ]);
    expect(lease.capacityHints?.[0]?.regionsTried).toEqual(["eu-west-1", "eu-west-2"]);
  });

  it("scopes non-admin usage to the current owner", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        owner: "peter@example.com",
        org: "openclaw",
        estimatedHourlyUSD: 1,
        maxEstimatedUSD: 1,
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        owner: "friend@example.com",
        org: "openclaw",
        estimatedHourlyUSD: 1,
        maxEstimatedUSD: 1,
      }),
    );
    const usage = await fleet.fetch(
      request("GET", "/v1/usage?scope=all&owner=peter@example.com", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(usage.status).toBe(200);
    const body = (await usage.json()) as {
      usage: { scope: string; owner: string; leases: number };
    };
    expect(body.usage.scope).toBe("user");
    expect(body.usage.owner).toBe("friend@example.com");
    expect(body.usage.leases).toBe(1);
  });

  it("resolves owner-scoped slugs and heartbeat extends idle expiry", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const touchedAt = new Date(Date.now() - 10 * 60 * 1000);
    const expiresAt = new Date(touchedAt.getTime() + 1800 * 1000);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        createdAt: touchedAt.toISOString(),
        updatedAt: touchedAt.toISOString(),
        lastTouchedAt: touchedAt.toISOString(),
        ttlSeconds: 5400,
        idleTimeoutSeconds: 1800,
        expiresAt: expiresAt.toISOString(),
      }),
    );

    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/heartbeat", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          idleTimeoutSeconds: 2400,
          telemetry: {
            capturedAt: "2026-05-05T01:02:03Z",
            source: "ssh-linux",
            load1: 0.42,
            memoryUsedBytes: 1024,
            memoryTotalBytes: 2048,
            memoryPercent: 50,
          },
        },
      }),
    );
    expect(heartbeat.status).toBe(200);
    const { lease } = (await heartbeat.json()) as { lease: LeaseRecord };
    expect(lease.id).toBe("cbx_000000000001");
    expect(lease.slug).toBe("blue-lobster");
    expect(lease.idleTimeoutSeconds).toBe(2400);
    expect(lease.telemetry).toMatchObject({
      capturedAt: "2026-05-05T01:02:03.000Z",
      source: "ssh-linux",
      load1: 0.42,
      memoryUsedBytes: 1024,
      memoryTotalBytes: 2048,
      memoryPercent: 50,
    });
    expect(lease.telemetryHistory).toHaveLength(1);
    expect(lease.telemetryHistory?.[0]).toMatchObject({ load1: 0.42, memoryPercent: 50 });
    expect(Date.parse(lease.expiresAt)).toBeGreaterThan(expiresAt.getTime());

    const secondHeartbeat = await fleet.fetch(
      request("POST", "/v1/leases/cbx_000000000001/heartbeat", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          telemetry: {
            capturedAt: "2026-05-05T01:03:03Z",
            source: "ssh-linux",
            load1: 0.84,
            memoryPercent: 55,
          },
        },
      }),
    );
    expect(secondHeartbeat.status).toBe(200);
    const second = (await secondHeartbeat.json()) as { lease: LeaseRecord };
    expect(second.lease.telemetry).toMatchObject({
      capturedAt: "2026-05-05T01:03:03.000Z",
      load1: 0.84,
      memoryPercent: 55,
    });
    expect(second.lease.telemetryHistory?.map((sample) => sample.load1)).toEqual([0.42, 0.84]);
    expect(second.lease.telemetryHistory?.map((sample) => sample.capturedAt)).toEqual([
      "2026-05-05T01:02:03.000Z",
      "2026-05-05T01:03:03.000Z",
    ]);
  });

  it("keeps lease telemetry history bounded to the latest samples", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
        telemetryHistory: Array.from({ length: 60 }, (_, index) => ({
          capturedAt: new Date(Date.UTC(2026, 4, 5, 1, index, 0)).toISOString(),
          source: "ssh-linux",
          load1: index,
        })),
      }),
    );

    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/heartbeat", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          telemetry: {
            capturedAt: "2026-05-05T02:00:00Z",
            source: "ssh-linux",
            load1: 61,
          },
        },
      }),
    );

    expect(heartbeat.status).toBe(200);
    const { lease } = (await heartbeat.json()) as { lease: LeaseRecord };
    expect(lease.telemetryHistory).toHaveLength(60);
    expect(lease.telemetryHistory?.[0]?.capturedAt).toBe("2026-05-05T01:01:00.000Z");
    expect(lease.telemetryHistory?.at(-1)).toMatchObject({
      capturedAt: "2026-05-05T02:00:00.000Z",
      load1: 61,
    });
  });

  it("hides exact lease IDs and lists from other non-admin users", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "amber-krill",
        owner: "friend@example.com",
        org: "openclaw",
      }),
    );
    const friendHeaders = {
      "x-crabbox-owner": "friend@example.com",
      "x-crabbox-org": "openclaw",
    };

    const byExactID = await fleet.fetch(
      request("GET", "/v1/leases/cbx_000000000001", { headers: friendHeaders }),
    );
    expect(byExactID.status).toBe(404);

    const heartbeat = await fleet.fetch(
      request("POST", "/v1/leases/cbx_000000000001/heartbeat", {
        headers: friendHeaders,
        body: {},
      }),
    );
    expect(heartbeat.status).toBe(404);

    const list = await fleet.fetch(request("GET", "/v1/leases", { headers: friendHeaders }));
    const body = (await list.json()) as { leases: LeaseRecord[] };
    expect(body.leases.map((lease) => lease.id)).toEqual(["cbx_000000000002"]);
  });

  it("renders the portal with only the current owner leases", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        desktop: true,
        code: true,
        telemetry: {
          capturedAt: new Date(Date.now() - 15_000).toISOString(),
          source: "ssh-linux",
          load1: 0.42,
          load5: 0.24,
          load15: 0.12,
          memoryUsedBytes: 1024,
          memoryTotalBytes: 2048,
          memoryPercent: 50,
          diskUsedBytes: 1024 * 1024 * 1024,
          diskTotalBytes: 4 * 1024 * 1024 * 1024,
          diskPercent: 25,
          uptimeSeconds: 3600,
        },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "amber-krill",
        owner: "friend@example.com",
        org: "openclaw",
        desktop: true,
      }),
    );
    storage.seed(
      "lease:cbx_000000000003",
      testLease({
        id: "cbx_000000000003",
        slug: "old-clam",
        owner: "peter@example.com",
        org: "openclaw",
        desktop: true,
        code: true,
        state: "released",
        releasedAt: "2026-05-01T00:20:00.000Z",
        endedAt: "2026-05-01T00:20:00.000Z",
      }),
    );
    storage.seed(
      "lease:cbx_000000000004",
      testLease({
        id: "cbx_000000000004",
        slug: "silver-window",
        owner: "peter@example.com",
        org: "openclaw",
        provider: "aws",
        target: "windows",
        windowsMode: "normal",
      }),
    );
    storage.seed(
      "lease:cbx_000000000005",
      testLease({
        id: "cbx_000000000005",
        slug: "wsl-window",
        owner: "peter@example.com",
        org: "openclaw",
        provider: "aws",
        target: "windows",
        windowsMode: "wsl2",
      }),
    );
    storage.seed(
      "lease:cbx_000000000006",
      testLease({
        id: "cbx_000000000006",
        slug: "azure-box",
        owner: "peter@example.com",
        org: "openclaw",
        provider: "azure",
        target: "linux",
      }),
    );
    await fleet.fetch(
      request("POST", "/v1/runners/sync", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          provider: "blacksmith-testbox",
          runners: [
            {
              id: "tbx_01testbox",
              status: "ready",
              repo: "openclaw",
              workflow: ".github/workflows/ci-check-testbox.yml",
              job: "check",
              ref: "main",
              createdAt: "2026-05-05T10:00:00.000Z",
              actionsRepo: "openclaw/openclaw",
              actionsRunID: "123456",
              actionsRunURL: "https://github.com/openclaw/openclaw/actions/runs/123456",
              actionsRunStatus: "in_progress",
              actionsWorkflowName: "ci-check-testbox",
              actionsWorkflowURL:
                "https://github.com/openclaw/openclaw/actions/workflows/ci-check-testbox.yml",
            },
          ],
        },
      }),
    );
    await fleet.fetch(
      request("POST", "/v1/runners/sync", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          provider: "blacksmith-testbox",
          runners: [
            {
              id: "tbx_friendbox",
              status: "ready",
              repo: "openclaw",
              workflow: ".github/workflows/ci-check-testbox.yml",
              job: "check",
              ref: "main",
            },
          ],
        },
      }),
    );

    const response = await fleet.fetch(
      request("GET", "/portal", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(response.status).toBe(200);
    const body = await response.text();
    expect(body).toContain('class="portal-shell"');
    expect(body).toContain("<h1>🦀 crabbox</h1>");
    expect(body).toContain('class="portal-actions"');
    expect(body).toContain("table-scroll");
    expect(body).toContain(".lease-table th:nth-child(1)");
    expect(body).toContain(
      'data-filter-buttons="active:active,ended:ended,external:external,stale:stale,stuck:stuck,aws:aws,azure:azure,hetzner:hetzner,blacksmith-testbox:blacksmith,linux:linux,macos:macos,windows:windows,all:all"',
    );
    expect(body).toContain('data-filter-default="active"');
    expect(body).not.toContain("external runners");
    expect(body).toContain("1 external");
    expect(body).toContain('class="external-row"');
    expect(body).toContain("no box access");
    expect(body).toContain("stuck");
    expect(body).toContain(
      'data-filter-tags="active stuck actions mine external blacksmith-testbox ready in_progress',
    );
    expect(body).toContain("tbx_01testbox");
    expect(body).toContain("/portal/runners/blacksmith-testbox/tbx_01testbox");
    expect(body).toContain("blacksmith-testbox");
    expect(body).toContain("ci-check-testbox.yml");
    expect(body).toContain("https://github.com/openclaw/openclaw/actions/runs/123456");
    expect(body).toContain(
      "https://github.com/openclaw/openclaw/actions/workflows/ci-check-testbox.yml",
    );
    expect(body).toContain('class="row-link"');
    expect(body).toContain(
      'data-copy-value="crabbox stop --provider blacksmith-testbox tbx_01testbox"',
    );
    expect(body).not.toContain("tbx_friendbox");
    expect(body).toContain('data-provider="azure"');
    expect(body).toContain('data-provider="hetzner"');
    expect(body).toContain('data-target="linux"');
    expect(body).toContain('data-target="windows"');
    expect(body).toContain("<span>win</span>");
    expect(body).toContain("<span>win (wsl2)</span>");
    expect(body).toContain('data-filter-tags="active mine hetzner linux"');
    expect(body).toContain('data-filter-tags="active mine azure linux"');
    expect(body).toContain('class="access-cell"');
    expect(body).toContain('title="server"');
    expect(body).toContain('data-access="vscode"');
    expect(body).toContain('data-access="vnc"');
    expect(body).toContain("data-sort=");
    expect(body).toContain("<time datetime=");
    expect(body).not.toContain("windows / normal");
    expect(body).toContain("blue-lobster");
    expect(body).toContain("old-clam");
    expect(body).toContain("released");
    expect(body).toContain("/portal/leases/cbx_000000000001");
    expect(body).toContain("/portal/leases/cbx_000000000001/vnc");
    expect(body).toContain("/portal/leases/cbx_000000000001/code/");
    expect(body).not.toContain("amber-krill");
  });

  it("syncs external runner visibility and marks missing runners stale", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };

    const sync = await fleet.fetch(
      request("POST", "/v1/runners/sync", {
        headers,
        body: {
          provider: "blacksmith-testbox",
          runners: [
            {
              id: "tbx_01kqyahxh67z6qtwtsdkt5xcst",
              status: "ready",
              repo: "openclaw",
              workflow: ".github/workflows/ci-check-testbox.yml",
              job: "check",
              ref: "main",
              createdAt: "2026-05-06T09:45:16.000000Z",
              actionsRunURL: "https://github.com/openclaw/openclaw/actions/runs/123456",
              actionsWorkflowURL:
                "https://github.com/openclaw/openclaw/actions/workflows/ci-check-testbox.yml",
            },
          ],
        },
      }),
    );
    expect(sync.status).toBe(200);
    const synced = (await sync.json()) as { runners: ExternalRunnerRecord[] };
    expect(synced.runners).toHaveLength(1);
    expect(synced.runners[0]).toMatchObject({
      id: "tbx_01kqyahxh67z6qtwtsdkt5xcst",
      provider: "blacksmith-testbox",
      status: "ready",
      repo: "openclaw",
      owner: "peter@example.com",
      org: "openclaw",
      actionsRunURL: "https://github.com/openclaw/openclaw/actions/runs/123456",
    });

    const friendList = await fleet.fetch(
      request("GET", "/v1/runners", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    const friendBody = (await friendList.json()) as { runners: ExternalRunnerRecord[] };
    expect(friendBody.runners).toHaveLength(0);

    const staleSync = await fleet.fetch(
      request("POST", "/v1/runners/sync", {
        headers,
        body: {
          provider: "blacksmith-testbox",
          runners: [],
        },
      }),
    );
    expect(staleSync.status).toBe(200);
    const staleBody = (await staleSync.json()) as { stale: ExternalRunnerRecord[] };
    expect(staleBody.stale).toHaveLength(1);
    expect(staleBody.stale[0]).toMatchObject({
      id: "tbx_01kqyahxh67z6qtwtsdkt5xcst",
      status: "missing",
      stale: true,
    });
  });

  it("renders external runner detail pages for visible runners", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    const sync = await fleet.fetch(
      request("POST", "/v1/runners/sync", {
        headers,
        body: {
          provider: "blacksmith-testbox",
          runners: [
            {
              id: "tbx_detail",
              status: "ready",
              repo: "openclaw",
              workflow: ".github/workflows/ci-check-testbox.yml",
              job: "check",
              ref: "main",
              createdAt: "2026-05-06T09:45:16.000000Z",
              actionsRepo: "openclaw/openclaw",
              actionsRunID: "123456",
              actionsRunURL: "https://github.com/openclaw/openclaw/actions/runs/123456",
              actionsRunStatus: "queued",
              actionsWorkflowName: "Blacksmith Testbox",
              actionsWorkflowURL:
                "https://github.com/openclaw/openclaw/actions/workflows/ci-check-testbox.yml",
            },
          ],
        },
      }),
    );
    expect(sync.status).toBe(200);

    const detail = await fleet.fetch(
      request("GET", "/portal/runners/blacksmith-testbox/tbx_detail", { headers }),
    );
    expect(detail.status).toBe(200);
    const body = await detail.text();
    expect(body).toContain("tbx_detail");
    expect(body).toContain("actions owner");
    expect(body).toContain("Blacksmith Testbox");
    expect(body).toContain("https://github.com/openclaw/openclaw/actions/runs/123456");
    expect(body).toContain("crabbox stop --provider blacksmith-testbox tbx_detail");
    expect(body).toContain("visibility only");

    const hidden = await fleet.fetch(
      request("GET", "/portal/runners/blacksmith-testbox/tbx_detail", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(hidden.status).toBe(404);
  });

  it("shows non-owned runner leases only in the admin portal", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "testbox-runner",
        owner: "blacksmith",
        org: "openclaw",
        provider: "aws",
        class: "standard",
      }),
    );

    const userResponse = await fleet.fetch(
      request("GET", "/portal", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    const userBody = await userResponse.text();
    expect(userBody).toContain("blue-lobster");
    expect(userBody).not.toContain("testbox-runner");
    expect(userBody).not.toContain("system:system");

    const adminResponse = await fleet.fetch(
      request("GET", "/portal", {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(adminResponse.status).toBe(200);
    const adminBody = await adminResponse.text();
    expect(adminBody).toContain("blue-lobster");
    expect(adminBody).toContain("testbox-runner");
    expect(adminBody).toContain("1 system");
    expect(adminBody).toContain("mine:mine,system:system");
    expect(adminBody).toContain('data-filter-tags="active mine hetzner linux"');
    expect(adminBody).toContain('data-filter-tags="active system aws linux"');
    expect(adminBody).toContain("cbx_000000000002 · blacksmith");

    const detail = await fleet.fetch(
      request("GET", "/portal/leases/cbx_000000000002", {
        headers: {
          "x-crabbox-admin": "true",
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(detail.status).toBe(200);
  });

  it("defaults the portal lease table to all leases when none are active", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000003",
      testLease({
        id: "cbx_000000000003",
        slug: "old-clam",
        owner: "peter@example.com",
        org: "openclaw",
        state: "expired",
        endedAt: "2026-05-01T00:20:00.000Z",
      }),
    );

    const response = await fleet.fetch(
      request("GET", "/portal", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(response.status).toBe(200);
    const body = await response.text();
    expect(body).toContain('data-filter-default="all"');
    expect(body).toContain("old-clam");
    expect(body).toContain("expired");
    expect(body).not.toContain("no leases visible");
  });

  it("renders lease detail pages with run logs and stop controls", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, { hetzner: fakeProvider() });
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        desktop: true,
        code: true,
        telemetry: {
          capturedAt: new Date(Date.now() - 15_000).toISOString(),
          source: "ssh-linux",
          load1: 0.42,
          load5: 0.24,
          load15: 0.12,
          memoryUsedBytes: 1024,
          memoryTotalBytes: 2048,
          memoryPercent: 50,
          diskUsedBytes: 1024 * 1024 * 1024,
          diskTotalBytes: 4 * 1024 * 1024 * 1024,
          diskPercent: 25,
          uptimeSeconds: 3600,
        },
        telemetryHistory: [
          {
            capturedAt: new Date(Date.now() - 45_000).toISOString(),
            source: "ssh-linux",
            load1: 0.22,
            memoryPercent: 42,
            diskPercent: 24,
          },
          {
            capturedAt: new Date(Date.now() - 30_000).toISOString(),
            source: "ssh-linux",
            load1: 0.32,
            memoryPercent: 47,
            diskPercent: 25,
          },
        ],
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "run:run_000000000001",
      testRun({
        id: "run_000000000001",
        leaseID: "cbx_000000000001",
        owner: "peter@example.com",
        org: "openclaw",
        command: ["go", "test", "./..."],
        state: "failed",
        phase: "failed",
        exitCode: 1,
        durationMs: 1234,
        logBytes: 11,
        telemetry: {
          start: {
            capturedAt: "2026-05-01T00:00:00.000Z",
            source: "ssh-linux",
            load1: 0.12,
            memoryUsedBytes: 1024,
            memoryTotalBytes: 2048,
            memoryPercent: 50,
            diskUsedBytes: 1024 * 1024,
            diskTotalBytes: 4 * 1024 * 1024,
            diskPercent: 25,
          },
          end: {
            capturedAt: "2026-05-01T00:00:02.000Z",
            source: "ssh-linux",
            load1: 0.42,
            load5: 0.24,
            load15: 0.12,
            memoryUsedBytes: 1536,
            memoryTotalBytes: 2048,
            memoryPercent: 75,
            diskUsedBytes: 2 * 1024 * 1024,
            diskTotalBytes: 4 * 1024 * 1024,
            diskPercent: 50,
          },
        },
        results: {
          format: "junit",
          files: ["junit.xml"],
          suites: 1,
          tests: 2,
          failures: 1,
          errors: 0,
          skipped: 0,
          timeSeconds: 0.42,
          failed: [
            {
              suite: "portal",
              name: "renders detail",
              message: "expected detail page",
              kind: "failure",
            },
          ],
        },
      }),
    );
    storage.seed(
      "run:run_000000000002",
      testRun({
        id: "run_000000000002",
        leaseID: "cbx_000000000001",
        owner: "friend@example.com",
        org: "openclaw",
      }),
    );
    storage.seed("runlog:run_000000000001", "portal log\n");
    storage.seed("runevent:run_000000000001:000000000001", {
      runID: "run_000000000001",
      seq: 1,
      type: "command.finished",
      phase: "failed",
      createdAt: "2026-05-01T00:00:01.000Z",
    });

    const page = await fleet.fetch(request("GET", "/portal/leases/blue-lobster", { headers }));
    expect(page.status).toBe(200);
    const body = await page.text();
    expect(body).toContain("crabbox ssh --id blue-lobster");
    expect(body).toContain("crabbox run --id blue-lobster -- &lt;command&gt;");
    expect(body).toContain(
      "crabbox webvnc --provider hetzner --target linux --id blue-lobster --open",
    );
    expect(body).toContain("crabbox code --id blue-lobster --open");
    expect(body).toContain("data-copy-command");
    expect(body).toContain('querySelector("code")');
    expect(body).toContain('class="portal-shell lease-shell"');
    expect(body).toContain("<h1>🦀 crabbox</h1>");
    expect(body).toContain("blue-lobster · hetzner linux lease");
    expect(body).toContain('data-search-placeholder="search runs"');
    expect(body).toContain(
      'data-filter-buttons="succeeded:succeeded,failed:failed,running:running,all:all"',
    );
    expect(body).not.toContain("<th>phase</th>");
    expect(body).not.toContain("<th>log</th>");
    expect(body).toContain('title="2026-05-01T00:00:00Z"');
    expect(body).toContain('data-provider="hetzner"');
    expect(body).toContain('data-target="linux"');
    expect(body).toContain("<dt>load</dt><dd>0.42 / 0.24 / 0.12</dd>");
    expect(body).toContain("<dt>memory</dt><dd>1.0 KiB / 2.0 KiB (50%)</dd>");
    expect(body).toContain("<dt>disk</dt><dd>1.0 GiB / 4.0 GiB (25%)</dd>");
    expect(body).toContain("<dt>uptime</dt><dd>1h</dd>");
    expect(body).toContain("box telemetry");
    expect(body).toContain('class="telemetry-chart"');
    expect(body).toContain("<span>0.42</span>");
    expect(body).toContain("<span>50%</span>");
    expect(body).toContain("load 0.42 · mem 75% · +512 B");
    expect(body).toContain("table-search");
    expect(body).toContain("/portal/runs/run_000000000001");
    expect(body).toContain("/portal/runs/run_000000000001/logs");
    expect(body).toContain("/portal/runs/run_000000000001/events");
    expect(body).toContain("/portal/leases/cbx_000000000001/release");
    expect(body).not.toContain("run_000000000002");

    const logs = await fleet.fetch(
      request("GET", "/portal/runs/run_000000000001/logs", { headers }),
    );
    expect(logs.status).toBe(200);
    expect(logs.headers.get("content-type")).toBe("text/plain; charset=utf-8");
    expect(await logs.text()).toBe("portal log\n");

    const runPage = await fleet.fetch(request("GET", "/portal/runs/run_000000000001", { headers }));
    expect(runPage.status).toBe(200);
    expect(runPage.headers.get("content-type")).toBe("text/html; charset=utf-8");
    const runBody = await runPage.text();
    expect(runBody).toContain('class="portal-shell run-shell"');
    expect(runBody).toContain('class="panel detail-card run-summary-card"');
    expect(runBody).toContain(
      ".run-shell .meta-grid { grid-template-columns:repeat(3,minmax(0,1fr)); }",
    );
    expect(runBody).toContain("<h1>🦀 crabbox</h1>");
    expect(runBody).toContain(
      ".portal-header-meta { flex:1 1 auto; min-width:0; overflow:hidden; }",
    );
    expect(runBody).toContain(".command-row > div { min-width:0; overflow:hidden; }");
    expect(runBody).toContain("run_000000000001 · cbx_000000000001 · failed");
    expect(runBody).not.toContain('<span class="mono">go test ./...</span>');
    expect(runBody).toContain("run_000000000001");
    expect(runBody).toContain("go test ./...");
    expect(runBody).toContain("data-copy-command");
    expect(runBody).toContain("portal log");
    expect(runBody).toContain('data-copy-target="#run-log-tail"');
    expect(runBody).toContain('data-search-placeholder="search events"');
    expect(runBody).toContain(
      'data-filter-buttons="run:run,command:command,sync:sync,stdout:stdout,stderr:stderr,all:all"',
    );
    expect(runBody).toContain('data-filter-tags="command failed"');
    expect(runBody).toContain('class="run-telemetry-grid"');
    expect(runBody).toContain(".run-artifact-card .button { width:100%; }");
    expect(runBody).toContain("@media (max-width: 980px)");
    expect(runBody).toContain(
      ".run-telemetry-grid { grid-template-columns:repeat(2,minmax(0,1fr)); }",
    );
    expect(runBody).toContain("<span>load</span>");
    expect(runBody).toContain("<strong>0.42 / 0.24 / 0.12</strong>");
    expect(runBody).toContain("<strong>1.5 KiB / 2.0 KiB (75%)</strong>");
    expect(runBody).toContain("<small>delta +512 B</small>");
    expect(runBody).toContain("table-search");
    expect(runBody).toContain("renders detail");
    expect(runBody).toContain("/portal/leases/cbx_000000000001");
    expect(runBody).toContain("/portal/runs/run_000000000001/logs");

    const events = await fleet.fetch(
      request("GET", "/portal/runs/run_000000000001/events", { headers }),
    );
    expect(events.status).toBe(200);
    await expect(events.json()).resolves.toMatchObject({
      events: [{ runID: "run_000000000001", type: "command.finished" }],
    });

    const stop = await fleet.fetch(
      request("POST", "/portal/leases/blue-lobster/release", { headers }),
    );
    expect(stop.status).toBe(303);
    expect(stop.headers.get("location")).toBe("/portal");
    expect(storage.value<LeaseRecord>("lease:cbx_000000000001")).toMatchObject({
      state: "released",
      keep: false,
    });
  });

  it("serves code pages only for code leases and requires a bridge ticket", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        code: true,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "plain-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        code: false,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const page = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/code/", { headers }),
    );
    expect(page.status).toBe(200);
    const pageBody = await page.text();
    expect(pageBody).toContain("crabbox code --id blue-lobster --open");
    expect(pageBody).toContain('class="vnc-page code-wait-page"');
    expect(pageBody).toContain("<h1>🦀 crabbox</h1>");
    expect(pageBody).toContain("code blue-lobster");
    expect(pageBody).toContain('id="code-status"');
    expect(pageBody).toContain('id="code-copy"');
    expect(pageBody).toContain("/portal/leases/cbx_000000000001/code/health");
    expect(pageBody).toContain("window.location.reload()");
    expect(pageBody).toContain("terminalStatusCodes");
    expect(pageBody).toContain("stopPolling(message)");

    const health = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/code/health", { headers }),
    );
    expect(health.status).toBe(200);
    const healthBody = (await health.json()) as {
      lease: { id: string; code: boolean };
      code: { agentConnected: boolean };
    };
    expect(healthBody.lease).toMatchObject({ id: "cbx_000000000001", code: true });
    expect(healthBody.code.agentConnected).toBe(false);

    const plain = await fleet.fetch(
      request("GET", "/portal/leases/plain-lobster/code/", { headers }),
    );
    expect(plain.status).toBe(409);

    const ticket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/code/ticket", { headers, body: {} }),
    );
    expect(ticket.status).toBe(200);
    const ticketBody = (await ticket.json()) as { ticket: string; leaseID: string };
    expect(ticketBody.ticket).toMatch(/^code_[a-f0-9]{32}$/);
    expect(ticketBody.leaseID).toBe("cbx_000000000001");

    const agent = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/code/agent", { headers }),
    );
    expect(agent.status).toBe(426);

    const missingTicket = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/code/agent", {
        headers: { upgrade: "websocket" },
      }),
    );
    expect(missingTicket.status).toBe(401);
  });

  it("stops code bridge polling after terminal status responses", async () => {
    const page = await portalCode(
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        code: true,
      }),
    ).text();
    const runtime = await runCodePortalScript(page, {
      ok: false,
      status: 409,
      json: async () => ({ error: "code_unavailable", message: "lease is not active" }),
    });

    expect(runtime.fetches).toEqual([
      "https://example.test/portal/leases/cbx_000000000001/code/health",
    ]);
    expect(runtime.elements["code-status"]?.textContent).toBe("bridge unavailable");
    expect(runtime.elements["code-status"]?.dataset.tone).toBe("bad");
    expect(runtime.elements["code-hint"]?.textContent).toBe("lease is not active");
    expect(runtime.timers).toEqual([]);
  });

  it("accepts bridge tickets in authorization before falling back to query strings", () => {
    expect(
      bridgeTicketFromRequest(
        request("GET", "/v1/leases/blue-lobster/code/agent?ticket=code_query", {
          headers: { authorization: "Bearer code_header" },
        }),
      ),
    ).toBe("code_header");
    expect(
      bridgeTicketFromRequest(
        request("GET", "/v1/leases/blue-lobster/code/agent?ticket=code_query"),
      ),
    ).toBe("code_query");
  });

  it("uses a VS Code-compatible CSP for code proxy responses", () => {
    const headers = codeResponseHeaders({
      "content-security-policy": "default-src 'none'; script-src 'self'",
      "content-length": "123",
      "content-type": "text/html",
      "cache-control": "public, max-age=31536000",
    });

    const csp = headers.get("content-security-policy") || "";
    expect(csp).toContain("script-src 'self' 'unsafe-inline' 'unsafe-eval' blob:");
    expect(csp).toContain("https://static.cloudflareinsights.com");
    expect(csp).toContain("worker-src 'self' data: blob:");
    expect(headers.get("content-length")).toBeNull();
    expect(headers.get("content-type")).toBe("text/html");
    expect(headers.get("cache-control")).toBe("no-store, no-transform");
  });

  it("forwards only the VS Code token cookie to code-server", () => {
    const headers = codeForwardHeaders(
      new Headers({
        cookie: "crabbox_session=secret; vscode-tkn=remote-token; other=value",
        origin: "https://crabbox.openclaw.ai",
      }),
    );

    expect(headers["cookie"]).toBe("vscode-tkn=remote-token");
    expect(headers["cookie"]).not.toContain("crabbox_session");
    expect(headers.origin).toBe("https://crabbox.openclaw.ai");
  });

  it("creates scoped egress tickets and reports bridge status", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const invalidRole = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/egress/ticket", {
        headers,
        body: { role: "viewer" },
      }),
    );
    expect(invalidRole.status).toBe(400);

    const ticket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/egress/ticket", {
        headers,
        body: {
          role: "host",
          sessionID: "egress_test123",
          profile: "discord",
          allow: ["discord.com", "*.discordcdn.com"],
        },
      }),
    );
    expect(ticket.status).toBe(200);
    const ticketBody = (await ticket.json()) as {
      ticket: string;
      leaseID: string;
      role: string;
      sessionID: string;
    };
    expect(ticketBody.ticket).toMatch(/^egress_[a-f0-9]{32}$/);
    expect(ticketBody.leaseID).toBe("cbx_000000000001");
    expect(ticketBody.role).toBe("host");
    expect(ticketBody.sessionID).toBe("egress_test123");

    const camelTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/egress/ticket", {
        headers,
        body: {
          role: "client",
          sessionId: "egress_camel123",
          allow: ["discord.com"],
        },
      }),
    );
    expect(camelTicket.status).toBe(200);
    await expect(camelTicket.json()).resolves.toMatchObject({
      role: "client",
      sessionID: "egress_camel123",
    });

    const status = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/egress/status", { headers }),
    );
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      hostConnected: false,
      clientConnected: false,
    });

    const portalPage = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster", { headers }),
    );
    expect(portalPage.status).toBe(200);
    const portalBody = await portalPage.text();
    expect(portalBody).toContain("<strong>egress</strong><small>waiting for host</small>");
    expect(portalBody).toContain("discord · discord.com");
    expect(portalBody).toContain("crabbox egress status --id blue-lobster");
    expect(portalBody).toContain("crabbox egress stop --id blue-lobster");

    const missingTicket = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/egress/host", {
        headers: { upgrade: "websocket" },
      }),
    );
    expect(missingTicket.status).toBe(401);
  });

  it("keeps egress status on the latest ticketed session", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const staleTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/egress/ticket", {
        headers,
        body: { role: "host", sessionID: "egress_old001", allow: ["example.com"] },
      }),
    );
    expect(staleTicket.status).toBe(200);
    const staleTicketBody = (await staleTicket.json()) as { ticket: string };
    await new Promise((resolve) => setTimeout(resolve, 2));

    const currentTicket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/egress/ticket", {
        headers,
        body: { role: "client", sessionID: "egress_new001", allow: ["example.com"] },
      }),
    );
    expect(currentTicket.status).toBe(200);

    const status = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/egress/status", { headers }),
    );
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({
      sessionID: "egress_new001",
      hostConnected: false,
      clientConnected: false,
    });
    expect(staleTicketBody.ticket).toMatch(/^egress_[a-f0-9]{32}$/);
  });

  it("does not let an older egress session replace a newer current session", () => {
    expect(
      shouldActivateEgressSession(
        { sessionID: "egress_new", createdAt: "2026-05-07T10:00:00.000Z" },
        "egress_old",
        "2026-05-07T09:59:59.000Z",
      ),
    ).toBe(false);
    expect(
      shouldActivateEgressSession(
        { sessionID: "egress_new", createdAt: "2026-05-07T10:00:00.000Z" },
        "egress_new",
        "2026-05-07T09:59:59.000Z",
      ),
    ).toBe(true);
  });

  it("serves WebVNC pages only for desktop leases and requires an agent upgrade", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        desktop: true,
        share: {
          users: {
            "friend@example.com": "use",
            "teammate@example.com": "manage",
          },
          org: "use",
        },
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "lease:cbx_000000000002",
      testLease({
        id: "cbx_000000000002",
        slug: "plain-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        desktop: false,
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );

    const page = await fleet.fetch(request("GET", "/portal/leases/blue-lobster/vnc", { headers }));
    expect(page.status).toBe(200);
    expect(page.headers.get("content-security-policy")).toContain("script-src 'self' 'nonce-");
    const pageBody = await page.text();
    expect(pageBody).toContain(
      "crabbox webvnc --provider hetzner --target linux --id blue-lobster --open",
    );
    expect(pageBody).toContain("/portal/assets/novnc/rfb.js");
    expect(pageBody).toContain("<h1>🦀 crabbox</h1>");
    expect(pageBody).toContain("WebVNC blue-lobster");
    expect(pageBody).toContain("function scheduleRetry");
    expect(pageBody).toContain("/portal/leases/cbx_000000000001/vnc/status");
    expect(pageBody).toContain("/portal/leases/cbx_000000000001/share");
    expect(pageBody).toContain("/portal/leases/cbx_000000000001/share?format=json");
    expect(pageBody).toContain("vnc-share-dialog");
    expect(pageBody).toContain("People with access");
    expect(pageBody).toContain("General access");
    expect(pageBody).toContain("copy WebVNC link");
    expect(pageBody).toContain("saveShare");
    expect(pageBody).toContain("teammate@example.com");
    expect(pageBody).toContain("shareableWebVNCURL");
    expect(pageBody).toContain('linkFragment.set("username", username)');
    expect(pageBody).toContain('linkFragment.set("password", password)');
    expect(pageBody).toContain("await refreshShareState()");
    expect(pageBody).toContain("writeClipboardText(shareableWebVNCURL())");
    expect(pageBody).toContain('document.getElementById("vnc-share")');
    expect(pageBody).toContain("vnc-copy-remote");
    expect(pageBody).toContain("vnc-paste");
    expect(pageBody).toContain("vnc-copy");
    expect(pageBody).toContain('addEventListener("clipboard"');
    expect(pageBody).toContain("remote clipboard ready");
    expect(pageBody).toContain("clipboardPasteFrom");
    expect(pageBody).toContain("rfb.showDotCursor = true");
    expect(pageBody).toContain('target === "macos"');
    expect(pageBody).toContain("MetaLeft");
    expect(pageBody).toContain("ControlLeft");
    expect(pageBody).toContain("position:sticky");
    expect(pageBody).toContain('data-provider="hetzner"');
    expect(pageBody).toContain('data-target="linux"');
    expect(pageBody).toContain("WebVNC daemon not running; run the bridge command below");
    expect(pageBody).toContain("waiting for an available WebVNC observer slot");
    expect(pageBody).toContain("/portal/leases/cbx_000000000001/vnc/control");
    expect(pageBody).toContain("vnc-takeover");
    expect(pageBody).toContain("vnc-control");
    expect(pageBody).toContain("take control");
    expect(pageBody).toContain("you control");
    expect(pageBody).toContain('aria-label="WebVNC display" tabindex="0"');
    expect(pageBody).toContain('screen.addEventListener("contextmenu"');
    expect(pageBody).toContain('screen.addEventListener("pointerdown", captureVNCInput');
    expect(pageBody).toContain("rfb?.focus?.({ preventScroll: true })");
    expect(pageBody).toContain("window.setTimeout(focusVNC, 0)");
    expect(pageBody).toContain("rfb.focusOnClick = true");
    expect(pageBody).not.toContain("vnc-role");
    expect(pageBody).not.toContain("status-pill vnc-role");
    expect(pageBody).toContain("rfb.viewOnly = !controlling");
    expect(pageBody).toContain("state?.terminal");
    expect(pageBody).toContain("stopPolling(state.message");
    expect(pageBody).toContain('fragment.get("username")');
    expect(pageBody).toContain('types.includes("username")');
    expect(pageBody).not.toContain("cdn.jsdelivr.net");

    const friendPage = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/vnc", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(friendPage.status).toBe(200);
    const friendPageBody = await friendPage.text();
    expect(friendPageBody).not.toContain('<button id="vnc-share"');
    expect(friendPageBody).not.toContain('<dialog id="vnc-share-dialog"');
    expect(friendPageBody).not.toContain("People with access");
    expect(friendPageBody).not.toContain("copy WebVNC link");
    expect(friendPageBody).not.toContain("friend@example.com");
    expect(friendPageBody).not.toContain("teammate@example.com");
    expect(friendPageBody).not.toContain('"org":"use"');

    const status = await fleet.fetch(
      request("GET", "/portal/leases/blue-lobster/vnc/status", { headers }),
    );
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      slug: "blue-lobster",
      bridgeConnected: false,
      viewerConnected: false,
      command: "crabbox webvnc --provider hetzner --target linux --id blue-lobster --open",
      events: [],
      message:
        "WebVNC daemon not running; run: crabbox webvnc --provider hetzner --target linux --id blue-lobster --open",
    });

    const apiStatus = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/webvnc/status", { headers }),
    );
    expect(apiStatus.status).toBe(200);
    await expect(apiStatus.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      bridgeConnected: false,
      viewerConnected: false,
      events: [],
    });

    const reset = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/webvnc/reset", { headers, body: {} }),
    );
    expect(reset.status).toBe(200);
    await expect(reset.json()).resolves.toMatchObject({
      leaseID: "cbx_000000000001",
      bridgeWasConnected: false,
      viewerWasConnected: false,
      command: "crabbox webvnc --provider hetzner --target linux --id blue-lobster --open",
      events: [{ event: "reset", reason: "WebVNC reset requested" }],
    });

    const plain = await fleet.fetch(
      request("GET", "/portal/leases/plain-lobster/vnc", { headers }),
    );
    expect(plain.status).toBe(409);

    const ticket = await fleet.fetch(
      request("POST", "/v1/leases/blue-lobster/webvnc/ticket", { headers, body: {} }),
    );
    expect(ticket.status).toBe(200);
    const ticketBody = (await ticket.json()) as { ticket: string; leaseID: string };
    expect(ticketBody.ticket).toMatch(/^wvnc_[a-f0-9]{32}$/);
    expect(ticketBody.leaseID).toBe("cbx_000000000001");

    const agent = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/webvnc/agent", { headers }),
    );
    expect(agent.status).toBe(426);

    const missingTicket = await fleet.fetch(
      request("GET", "/v1/leases/blue-lobster/webvnc/agent", {
        headers: { upgrade: "websocket" },
      }),
    );
    expect(missingTicket.status).toBe(401);
  });

  it("buffers initial WebVNC bridge bytes until the viewer attaches", async () => {
    const buffers = new Map<string, WebVNCBuffer>();
    const sent: Array<string | ArrayBuffer> = [];
    const viewer = {
      readyState: WebSocket.OPEN,
      send(data: string | ArrayBuffer) {
        sent.push(data);
      },
    } as WebSocket;

    await forwardOrBufferWebVNC("RFB 003.008\n", undefined, buffers, "cbx_000000000001");
    expect(sent).toEqual([]);
    expect(buffers.get("cbx_000000000001")).toMatchObject({
      chunks: ["RFB 003.008\n"],
      bytes: 12,
    });

    flushPendingWebVNC(buffers, "cbx_000000000001", viewer);
    expect(sent).toEqual(["RFB 003.008\n"]);
    expect(buffers.has("cbx_000000000001")).toBe(false);
  });

  it("converts WebVNC Blob frames before forwarding", async () => {
    const buffers = new Map<string, WebVNCBuffer>();
    const sent: Array<string | ArrayBuffer> = [];
    const viewer = {
      readyState: WebSocket.OPEN,
      send(data: string | ArrayBuffer) {
        sent.push(data);
      },
    } as WebSocket;

    await forwardOrBufferWebVNC(new Blob(["RFB 003.008\n"]), viewer, buffers, "cbx_000000000001");

    expect(sent).toHaveLength(1);
    expect(new TextDecoder().decode(sent[0] as ArrayBuffer)).toBe("RFB 003.008\n");
    expect(buffers.has("cbx_000000000001")).toBe(false);
  });

  it("resets the WebVNC bridge when the viewer goes away", () => {
    const buffers = new Map<string, WebVNCBuffer>();
    buffers.set("cbx_000000000001", { chunks: ["RFB 003.008\n"], bytes: 12 });
    buffers.set("cbx_000000000001:agent_a", { chunks: ["RFB 003.008\n"], bytes: 12 });
    const closed: Array<{ code: number; reason: string }> = [];
    const agents = new Map<string, Map<string, WebSocket>>();
    agents.set(
      "cbx_000000000001",
      new Map([
        [
          "agent_a",
          {
            readyState: WebSocket.OPEN,
            close(code: number, reason: string) {
              closed.push({ code, reason });
            },
          } as WebSocket,
        ],
      ]),
    );

    resetWebVNCBridge(agents, buffers, "cbx_000000000001", 1011, "WebVNC viewer disconnected");

    expect(closed).toEqual([{ code: 1011, reason: "WebVNC viewer disconnected" }]);
    expect(agents.has("cbx_000000000001")).toBe(false);
    expect(buffers.has("cbx_000000000001")).toBe(false);
    expect(buffers.has("cbx_000000000001:agent_a")).toBe(false);
  });

  it("keeps pool inventory admin-only", async () => {
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(),
      hetzner: fakeProvider(),
    });
    const denied = await fleet.fetch(
      request("GET", "/v1/pool", {
        headers: {
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(denied.status).toBe(403);

    const allowed = await fleet.fetch(
      request("GET", "/v1/pool", {
        headers: { "x-crabbox-admin": "true" },
      }),
    );
    expect(allowed.status).toBe(200);
  });

  it("creates, waits, and promotes AWS images through admin routes", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage, {
      aws: fakeProvider(),
    });
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        cloudID: "i-123",
        region: "eu-west-1",
      }),
    );

    const denied = await fleet.fetch(
      request("POST", "/v1/images", {
        body: { leaseID: "cbx_000000000001", name: "openclaw-crabbox-test" },
      }),
    );
    expect(denied.status).toBe(403);

    const created = await fleet.fetch(
      request("POST", "/v1/images", {
        headers: { "x-crabbox-admin": "true" },
        body: { leaseID: "cbx_000000000001", name: "openclaw-crabbox-test" },
      }),
    );
    expect(created.status).toBe(201);
    const createdBody = (await created.json()) as { image: { id: string; state: string } };
    expect(createdBody.image).toEqual(
      expect.objectContaining({ id: "ami-000000000001", state: "pending" }),
    );

    const promoted = await fleet.fetch(
      request("POST", "/v1/images/ami-000000000001/promote", {
        headers: { "x-crabbox-admin": "true" },
        body: {},
      }),
    );
    expect(promoted.status).toBe(200);
    expect(storage.value("image:aws:promoted")).toEqual(
      expect.objectContaining({ id: "ami-000000000001", state: "available" }),
    );
  });

  it("mints broker-owned artifact upload URLs without exposing secrets", async () => {
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        CRABBOX_ARTIFACTS_BACKEND: "r2",
        CRABBOX_ARTIFACTS_BUCKET: "qa-artifacts",
        CRABBOX_ARTIFACTS_PREFIX: "qa",
        CRABBOX_ARTIFACTS_BASE_URL: "https://artifacts.example.com",
        CRABBOX_ARTIFACTS_REGION: "auto",
        CRABBOX_ARTIFACTS_ENDPOINT_URL: "https://account.r2.cloudflarestorage.com",
        CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "access-key",
        CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "super-secret",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/artifacts/uploads", {
        headers: { "x-crabbox-owner": "peter@example.com" },
        body: {
          prefix: "pr-42",
          files: [
            {
              name: "screenshots/after.png",
              size: 123,
              contentType: "image/png",
              sha256: await sha256HexForTest("after"),
            },
          ],
        },
      }),
    );

    expect(response.status).toBe(201);
    const body = (await response.json()) as {
      backend: string;
      bucket: string;
      prefix: string;
      files: Array<{
        name: string;
        key: string;
        url: string;
        upload: { url: string; headers: Record<string, string> };
      }>;
    };
    expect(body.backend).toBe("r2");
    expect(body.bucket).toBe("qa-artifacts");
    expect(body.prefix).toBe("qa/peter@example.com/pr-42");
    expect(body.files[0].key).toBe("qa/peter@example.com/pr-42/screenshots/after.png");
    expect(body.files[0].url).toBe(
      "https://artifacts.example.com/qa/peter%40example.com/pr-42/screenshots/after.png",
    );
    expect(body.files[0].upload.headers["content-length"]).toBe("123");
    expect(body.files[0].upload.headers["content-type"]).toBe("image/png");
    expect(body.files[0].upload.url).toContain("X-Amz-Signature=");
    expect(new URL(body.files[0].upload.url).searchParams.get("X-Amz-SignedHeaders")).toContain(
      "content-length",
    );
    expect(JSON.stringify(body)).not.toContain("super-secret");
  });

  it("reports artifact broker setup errors without provider-specific local credentials", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(
      request("POST", "/v1/artifacts/uploads", {
        body: { files: [{ name: "screenshot.png", size: 1 }] },
      }),
    );
    const body = (await response.json()) as { error: string; message: string };
    expect(response.status).toBe(400);
    expect(body.error).toBe("artifact_upload_unavailable");
    expect(body.message).toContain("artifact broker is not configured");
  });

  it("requires an R2 endpoint before minting artifact upload URLs", async () => {
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        CRABBOX_ARTIFACTS_BACKEND: "r2",
        CRABBOX_ARTIFACTS_BUCKET: "qa-artifacts",
        CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "access-key",
        CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "super-secret",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/artifacts/uploads", {
        body: { files: [{ name: "screenshot.png", size: 1 }] },
      }),
    );
    const body = (await response.json()) as { error: string; message: string };
    expect(response.status).toBe(400);
    expect(body.error).toBe("artifact_upload_unavailable");
    expect(body.message).toContain("CRABBOX_ARTIFACTS_ENDPOINT_URL");
  });

  it("caps aggregate artifact upload bytes before minting grants", async () => {
    const fleet = testFleet(
      new MemoryStorage(),
      {},
      {
        CRABBOX_ARTIFACTS_BACKEND: "r2",
        CRABBOX_ARTIFACTS_BUCKET: "qa-artifacts",
        CRABBOX_ARTIFACTS_ENDPOINT_URL: "https://account.r2.cloudflarestorage.com",
        CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "access-key",
        CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "super-secret",
      },
    );

    const response = await fleet.fetch(
      request("POST", "/v1/artifacts/uploads", {
        body: {
          files: Array.from({ length: 6 }, (_, index) => ({
            name: `video-${index}.mp4`,
            size: 1024 * 1024 * 1024,
          })),
        },
      }),
    );
    const body = (await response.json()) as { error: string; message: string };
    expect(response.status).toBe(400);
    expect(body.error).toBe("artifact_upload_unavailable");
    expect(body.message).toContain("5368709120 bytes");
  });
});

describe("fleet run history", () => {
  it("creates early run sessions and appends durable events", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        provider: "aws",
        serverType: "t3.small",
      }),
    );
    const ownerHeaders = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        headers: ownerHeaders,
        body: {
          provider: "aws",
          class: "standard",
          serverType: "t3.small",
          command: ["pnpm", "test"],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: { id: string; phase: string } };
    expect(run.phase).toBe("starting");

    const attached = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/events`, {
        headers: ownerHeaders,
        body: {
          type: "lease.created",
          leaseID: "cbx_000000000001",
          slug: "blue-lobster",
          provider: "aws",
          class: "standard",
          serverType: "t3.small",
        },
      }),
    );
    expect(attached.status).toBe(201);

    const stdout = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/events`, {
        headers: ownerHeaders,
        body: { type: "stdout", stream: "stdout", data: "ok\n" },
      }),
    );
    expect(stdout.status).toBe(201);

    const read = await fleet.fetch(request("GET", `/v1/runs/${run.id}`, { headers: ownerHeaders }));
    const readBody = (await read.json()) as {
      run: { leaseID: string; slug: string; phase: string; eventCount: number };
    };
    expect(readBody.run.leaseID).toBe("cbx_000000000001");
    expect(readBody.run.slug).toBe("blue-lobster");
    expect(readBody.run.phase).toBe("command");
    expect(readBody.run.eventCount).toBe(3);

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        headers: ownerHeaders,
        body: { exitCode: 0, log: "ok\n" },
      }),
    );
    expect(finish.status).toBe(200);

    const events = await fleet.fetch(
      request("GET", `/v1/runs/${run.id}/events`, { headers: ownerHeaders }),
    );
    const eventsBody = (await events.json()) as {
      events: Array<{ seq: number; type: string; data?: string }>;
    };
    expect(eventsBody.events.map((event) => event.type)).toEqual([
      "run.started",
      "lease.created",
      "stdout",
      "command.finished",
    ]);
    expect(eventsBody.events.map((event) => event.seq)).toEqual([1, 2, 3, 4]);

    const pagedEvents = await fleet.fetch(
      request("GET", `/v1/runs/${run.id}/events?after=1&limit=2`, {
        headers: ownerHeaders,
      }),
    );
    expect(pagedEvents.status).toBe(200);
    const pagedEventsBody = (await pagedEvents.json()) as {
      events: Array<{ seq: number; type: string }>;
    };
    expect(pagedEventsBody.events.map((event) => [event.seq, event.type])).toEqual([
      [2, "lease.created"],
      [3, "stdout"],
    ]);
  });

  it("streams run events and lease heartbeats over a control websocket", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const headers = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
        expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    );
    storage.seed(
      "run:run_000000000001",
      testRun({
        id: "run_000000000001",
        leaseID: "cbx_000000000001",
        owner: "peter@example.com",
        org: "openclaw",
        eventCount: 1,
      }),
    );
    storage.seed("runevent:run_000000000001:000000000001", {
      runID: "run_000000000001",
      seq: 1,
      type: "run.started",
      phase: "starting",
      createdAt: "2026-05-01T00:00:00.000Z",
    });
    const socket = new FakeWebSocket({
      kind: "control",
      clientID: "ctrl_1",
      owner: "peter@example.com",
      org: "openclaw",
      subscriptions: {},
    });
    (
      fleet as unknown as {
        controlSockets: Map<string, WebSocket>;
      }
    ).controlSockets.set("ctrl_1", socket as unknown as WebSocket);

    await fleet.webSocketMessage(
      socket as unknown as WebSocket,
      JSON.stringify({ type: "subscribe_run", runID: "run_000000000001", after: 0 }),
    );
    expect(socket.sentJSON()[0]).toMatchObject({
      type: "run_events",
      runID: "run_000000000001",
      nextSeq: 1,
      events: [{ seq: 1, type: "run.started" }],
    });

    await fleet.fetch(
      request("POST", "/v1/runs/run_000000000001/events", {
        headers,
        body: { type: "stdout", stream: "stdout", data: "ok\n" },
      }),
    );
    expect(socket.sentJSON()[1]).toMatchObject({
      type: "run_events",
      runID: "run_000000000001",
      nextSeq: 2,
      events: [{ seq: 2, type: "stdout", data: "ok\n" }],
    });

    await fleet.webSocketMessage(
      socket as unknown as WebSocket,
      JSON.stringify({ type: "heartbeat", leaseID: "blue-lobster", idleTimeoutSeconds: 900 }),
    );
    expect(socket.sentJSON()[2]).toMatchObject({
      type: "heartbeat",
      leaseID: "cbx_000000000001",
      ok: true,
    });
    expect(storage.value<LeaseRecord>("lease:cbx_000000000001")?.idleTimeoutSeconds).toBe(900);
  });

  it("records finished runs and serves logs", async () => {
    const fleet = testFleet();
    const ownerHeaders = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        headers: ownerHeaders,
        body: {
          leaseID: "cbx_000000000001",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          command: ["go", "test", "./..."],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: { id: string } };

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        headers: ownerHeaders,
        body: {
          exitCode: 0,
          syncMs: 12,
          commandMs: 34,
          log: "ok\n",
          telemetry: {
            start: {
              capturedAt: "2026-05-01T00:00:00Z",
              source: "ssh-linux",
              load1: 0.1,
              memoryUsedBytes: 1024,
              memoryTotalBytes: 2048,
              memoryPercent: 50,
            },
            end: {
              capturedAt: "2026-05-01T00:00:02Z",
              source: "ssh-linux",
              load1: 0.2,
              memoryUsedBytes: 1536,
              memoryTotalBytes: 2048,
              memoryPercent: 75,
            },
          },
          results: {
            format: "junit",
            files: ["junit.xml"],
            suites: 1,
            tests: 2,
            failures: 1,
            errors: 0,
            skipped: 0,
            timeSeconds: 1.2,
            failed: [{ suite: "pkg", name: "fails", kind: "failure" }],
          },
        },
      }),
    );
    expect(finish.status).toBe(200);
    const finished = (await finish.json()) as {
      run: {
        state: string;
        logBytes: number;
        results?: { tests: number };
        telemetry?: { end?: { load1?: number; memoryPercent?: number } };
      };
    };
    expect(finished.run.state).toBe("succeeded");
    expect(finished.run.logBytes).toBe(3);
    expect(finished.run.results?.tests).toBe(2);
    expect(finished.run.telemetry?.end).toMatchObject({ load1: 0.2, memoryPercent: 75 });

    const listed = await fleet.fetch(
      request("GET", "/v1/runs?leaseID=cbx_000000000001", { headers: ownerHeaders }),
    );
    const listBody = (await listed.json()) as { runs: Array<{ id: string; owner: string }> };
    expect(listBody.runs).toHaveLength(1);
    expect(listBody.runs[0]?.id).toBe(run.id);
    expect(listBody.runs[0]?.owner).toBe("peter@example.com");

    const logs = await fleet.fetch(
      request("GET", `/v1/runs/${run.id}/logs`, { headers: ownerHeaders }),
    );
    expect(await logs.text()).toBe("ok\n");
  });

  it("appends live run telemetry samples and preserves them on finish", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const ownerHeaders = {
      "x-crabbox-owner": "peter@example.com",
      "x-crabbox-org": "openclaw",
    };
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        slug: "blue-lobster",
        owner: "peter@example.com",
        org: "openclaw",
      }),
    );
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        headers: ownerHeaders,
        body: { leaseID: "cbx_000000000001", command: ["sleep", "60"] },
      }),
    );
    const { run } = (await create.json()) as { run: RunRecord };

    const firstSample = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/telemetry`, {
        headers: ownerHeaders,
        body: {
          telemetry: {
            capturedAt: "2026-05-01T00:00:10Z",
            source: "ssh-linux",
            load1: 0.4,
            memoryPercent: 40,
          },
        },
      }),
    );
    expect(firstSample.status).toBe(200);
    const sampled = (await firstSample.json()) as { run: RunRecord };
    expect(sampled.run.telemetry?.start).toMatchObject({ load1: 0.4, memoryPercent: 40 });
    expect(sampled.run.telemetry?.samples).toHaveLength(1);

    await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/telemetry`, {
        headers: ownerHeaders,
        body: {
          telemetry: {
            capturedAt: "2026-05-01T00:00:20Z",
            source: "ssh-linux",
            load1: 0.9,
            memoryPercent: 55,
          },
        },
      }),
    );

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        headers: ownerHeaders,
        body: {
          exitCode: 0,
          telemetry: {
            end: {
              capturedAt: "2026-05-01T00:00:30Z",
              source: "ssh-linux",
              load1: 1.2,
              memoryPercent: 60,
            },
          },
        },
      }),
    );
    expect(finish.status).toBe(200);
    const finished = (await finish.json()) as { run: RunRecord };
    expect(finished.run.telemetry?.end).toMatchObject({ load1: 1.2, memoryPercent: 60 });
    expect(finished.run.telemetry?.samples?.map((sample) => sample.load1)).toEqual([0.4, 0.9]);
  });

  it("accepts Go nil slices in passing test results", async () => {
    const fleet = testFleet();
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        body: {
          leaseID: "cbx_000000000001",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          command: ["go", "test", "./..."],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: { id: string } };

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        body: {
          exitCode: 0,
          log: "ok\n",
          results: {
            format: "junit",
            files: null,
            suites: 1,
            tests: 1,
            failures: 0,
            errors: 0,
            skipped: 0,
            timeSeconds: 0.001,
            failed: null,
          },
        },
      }),
    );
    expect(finish.status).toBe(200);
    const finished = (await finish.json()) as {
      run: { results?: { files: string[]; failed: unknown[] } };
    };
    expect(finished.run.results?.files).toEqual([]);
    expect(finished.run.results?.failed).toEqual([]);
  });

  it("records chunked run logs so failures do not disappear from long output", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        body: {
          leaseID: "cbx_000000000001",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          command: ["pnpm", "test"],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: { id: string } };
    const chunkA = `${"a".repeat(70_000)}\nFAIL src/example.test.ts\n`;
    const chunkB = `${"b".repeat(70_000)}\nELIFECYCLE Test failed\n`;

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        body: {
          exitCode: 1,
          log: "fallback tail only\n",
          logChunks: [chunkA, chunkB],
        },
      }),
    );
    expect(finish.status).toBe(200);
    const finished = (await finish.json()) as {
      run: { state: string; logBytes: number; logTruncated: boolean };
    };
    expect(finished.run.state).toBe("failed");
    expect(finished.run.logBytes).toBe(chunkA.length + chunkB.length);
    expect(finished.run.logTruncated).toBe(false);
    expect(storage.value<string>(`runlog:${run.id}`)).toBe("");

    const logs = await fleet.fetch(request("GET", `/v1/runs/${run.id}/logs`));
    const logText = await logs.text();
    expect(logText).toContain("FAIL src/example.test.ts");
    expect(logText).toContain("ELIFECYCLE Test failed");
    expect(logText).not.toContain("fallback tail only");
  });

  it("records resolved lease metadata instead of caller-supplied fallback guesses", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "lease:cbx_000000000001",
      testLease({
        id: "cbx_000000000001",
        provider: "aws",
        class: "beast",
        serverType: "c7i.24xlarge",
        owner: "peter@example.com",
        org: "openclaw",
      }),
    );
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_000000000001",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          command: ["go", "test", "./..."],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: RunRecord };
    expect(run.provider).toBe("aws");
    expect(run.class).toBe("beast");
    expect(run.serverType).toBe("c7i.24xlarge");
  });

  it("hides run records and logs from other non-admin users", async () => {
    const storage = new MemoryStorage();
    const fleet = testFleet(storage);
    storage.seed(
      "run:run_000000000001",
      testRun({
        id: "run_000000000001",
        leaseID: "cbx_000000000001",
        owner: "peter@example.com",
        org: "openclaw",
      }),
    );
    storage.seed("runlog:run_000000000001", "secret log\n");
    storage.seed(
      "run:run_000000000002",
      testRun({
        id: "run_000000000002",
        leaseID: "cbx_000000000002",
        owner: "friend@example.com",
        org: "openclaw",
      }),
    );
    const friendHeaders = {
      "x-crabbox-owner": "friend@example.com",
      "x-crabbox-org": "openclaw",
    };

    const list = await fleet.fetch(request("GET", "/v1/runs", { headers: friendHeaders }));
    const listBody = (await list.json()) as { runs: RunRecord[] };
    expect(listBody.runs.map((run) => run.id)).toEqual(["run_000000000002"]);

    const read = await fleet.fetch(
      request("GET", "/v1/runs/run_000000000001", { headers: friendHeaders }),
    );
    expect(read.status).toBe(404);

    const logs = await fleet.fetch(
      request("GET", "/v1/runs/run_000000000001/logs", { headers: friendHeaders }),
    );
    expect(logs.status).toBe(404);

    const finish = await fleet.fetch(
      request("POST", "/v1/runs/run_000000000001/finish", {
        headers: friendHeaders,
        body: { exitCode: 0, log: "overwrite\n" },
      }),
    );
    expect(finish.status).toBe(404);
    expect(storage.value<string>("runlog:run_000000000001")).toBe("secret log\n");
  });

  it("bounds stored result summaries", async () => {
    const fleet = testFleet();
    const create = await fleet.fetch(
      request("POST", "/v1/runs", {
        body: {
          leaseID: "cbx_000000000001",
          provider: "aws",
          class: "beast",
          serverType: "c7a.48xlarge",
          command: ["go", "test", "./..."],
        },
      }),
    );
    expect(create.status).toBe(201);
    const { run } = (await create.json()) as { run: { id: string } };
    const failed = Array.from({ length: 150 }, (_, index) => ({
      suite: "pkg",
      name: `fails-${index}`,
      kind: "failure" as const,
      message: "x".repeat(5000),
    }));

    const finish = await fleet.fetch(
      request("POST", `/v1/runs/${run.id}/finish`, {
        body: {
          exitCode: 1,
          log: "",
          results: {
            format: "junit",
            files: Array.from({ length: 80 }, (_, index) => `junit-${index}.xml`),
            suites: 1,
            tests: 150,
            failures: 150,
            errors: 0,
            skipped: 0,
            timeSeconds: 1.2,
            failed,
          },
        },
      }),
    );
    expect(finish.status).toBe(200);
    const finished = (await finish.json()) as {
      run: { results?: { files: string[]; failed: Array<{ message?: string }> } };
    };
    expect(finished.run.results?.files).toHaveLength(50);
    expect(finished.run.results?.failed).toHaveLength(100);
    expect(
      new TextEncoder().encode(finished.run.results?.failed[0]?.message ?? "").byteLength,
    ).toBe(4096);
  });
});

describe("fleet identity", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("reports owner and org from request context", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(
      request("GET", "/v1/whoami", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(await response.json()).toEqual({
      owner: "peter@example.com",
      org: "openclaw",
      auth: "bearer",
    });
  });

  it("reports forwarded GitHub auth mode", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(
      request("GET", "/v1/whoami", {
        headers: {
          "x-crabbox-auth": "github",
          "x-crabbox-owner": "friend@example.com",
          "x-crabbox-org": "openclaw",
        },
      }),
    );
    expect(await response.json()).toEqual({
      owner: "friend@example.com",
      org: "openclaw",
      auth: "github",
    });
  });

  it("rejects admin routes without an admin token context", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(request("GET", "/v1/admin/leases"));
    expect(response.status).toBe(403);
  });

  it("starts GitHub login and keeps polling secret server-side", async () => {
    const storage = new MemoryStorage();
    const fleet = new FleetDurableObject(
      { storage } as unknown as DurableObjectState,
      {
        CRABBOX_DEFAULT_ORG: "openclaw",
        CRABBOX_GITHUB_CLIENT_ID: "github-client",
        CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
        CRABBOX_SHARED_TOKEN: "shared",
      } as Env,
    );
    const pollSecret = "local-poll-secret";
    const start = await fleet.fetch(
      request("POST", "/v1/auth/github/start", {
        body: {
          pollSecretHash: await sha256HexForTest(pollSecret),
          provider: "aws",
        },
      }),
    );
    expect(start.status).toBe(200);
    const body = (await start.json()) as { loginID: string; url: string };
    expect(body.loginID).toMatch(/^login_/);
    const url = new URL(body.url);
    expect(url.origin + url.pathname).toBe("https://github.com/login/oauth/authorize");
    expect(url.searchParams.get("client_id")).toBe("github-client");
    expect(url.searchParams.get("scope")).toBe("read:user user:email read:org");

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID: body.loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(200);
    await expect(poll.json()).resolves.toMatchObject({ status: "pending" });
  });

  it("sets a portal session cookie after GitHub login", async () => {
    const storage = new MemoryStorage();
    const fleet = new FleetDurableObject(
      { storage } as unknown as DurableObjectState,
      {
        CRABBOX_DEFAULT_ORG: "openclaw",
        CRABBOX_GITHUB_CLIENT_ID: "github-client",
        CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
        CRABBOX_SHARED_TOKEN: "shared",
        CRABBOX_SESSION_SECRET: "session-secret",
      } as Env,
    );
    const start = await fleet.fetch(
      request("GET", "/portal/login?returnTo=/portal/leases/cbx_000000000001/vnc"),
    );
    expect(start.status).toBe(302);
    const location = start.headers.get("location") ?? "";
    const state = new URL(location).searchParams.get("state");
    expect(state).toBeTruthy();

    vi.stubGlobal("fetch", githubFetchMock({ member: true }));
    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(302);
    expect(callback.headers.get("location")).toBe("/portal/leases/cbx_000000000001/vnc");
    expect(callback.headers.get("set-cookie")).toContain("crabbox_session=cbxu_");
  });

  it("clears portal session on logout without restarting OAuth", async () => {
    const fleet = testFleet();
    const logout = await fleet.fetch(request("GET", "/portal/logout"));
    expect(logout.status).toBe(200);
    expect(logout.headers.get("location")).toBeNull();
    expect(logout.headers.get("set-cookie")).toContain("crabbox_session=");
    expect(logout.headers.get("set-cookie")).toContain("Max-Age=0");
    const body = await logout.text();
    expect(body).toContain("Crabbox logged out");
    expect(body).toContain("/portal/login");
  });

  it("cleans expired GitHub login attempts before rate limiting", async () => {
    const storage = new MemoryStorage();
    const fleet = new FleetDurableObject(
      { storage } as unknown as DurableObjectState,
      {
        CRABBOX_DEFAULT_ORG: "openclaw",
        CRABBOX_GITHUB_CLIENT_ID: "github-client",
        CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
        CRABBOX_SHARED_TOKEN: "shared",
      } as Env,
    );
    storage.seed("oauth:login_old", {
      id: "login_old",
      state: "state_old",
      pollSecretHash: "0".repeat(64),
      createdAt: "2026-05-01T00:00:00.000Z",
      expiresAt: "2026-05-01T00:00:00.000Z",
    });
    storage.seed("oauth_state:state_old", "login_old");

    const start = await fleet.fetch(
      request("POST", "/v1/auth/github/start", {
        body: {
          pollSecretHash: await sha256HexForTest("new-secret"),
          provider: "aws",
        },
      }),
    );
    expect(start.status).toBe(200);
    expect(storage.value("oauth:login_old")).toBeUndefined();
    expect(storage.value("oauth_state:state_old")).toBeUndefined();
  });

  it("requires GitHub org membership before completing login", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin();
    vi.stubGlobal("fetch", githubFetchMock({ member: false }));

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(403);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(400);
    await expect(poll.json()).resolves.toMatchObject({
      status: "failed",
      error: "GitHub user friend is not an active member of openclaw.",
    });
  });

  it("mints GitHub login tokens for allowed org members", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin();
    vi.stubGlobal("fetch", githubFetchMock({ member: true }));

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(200);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(200);
    const body = (await poll.json()) as {
      status: string;
      token?: string;
      owner?: string;
      org?: string;
      login?: string;
    };
    expect(body).toMatchObject({
      status: "complete",
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
    });
    expect(body.token).toMatch(/^cbxu_/);
  });

  it("requires configured GitHub team membership before completing login", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin({
      CRABBOX_GITHUB_ALLOWED_TEAMS: "maintainers",
    });
    vi.stubGlobal(
      "fetch",
      githubFetchMock({
        member: true,
        teams: [{ slug: "contributors", organization: { login: "openclaw" } }],
      }),
    );

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(403);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(400);
    await expect(poll.json()).resolves.toMatchObject({
      status: "failed",
      error: "GitHub user friend is not a member of an allowed team in openclaw.",
    });
  });

  it("mints GitHub login tokens for allowed team members", async () => {
    const { fleet, loginID, state, pollSecret } = await startGitHubLogin({
      CRABBOX_GITHUB_ALLOWED_TEAMS: "openclaw/maintainers,openclaw/release-captains",
    });
    vi.stubGlobal(
      "fetch",
      githubFetchMock({
        member: true,
        teams: [{ slug: "maintainers", organization: { login: "openclaw" } }],
      }),
    );

    const callback = await fleet.fetch(
      request("GET", `/v1/auth/github/callback?code=ok&state=${state}`),
    );
    expect(callback.status).toBe(200);

    const poll = await fleet.fetch(
      request("POST", "/v1/auth/github/poll", {
        body: {
          loginID,
          pollSecret,
        },
      }),
    );
    expect(poll.status).toBe(200);
    await expect(poll.json()).resolves.toMatchObject({
      status: "complete",
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
    });
  });

  it("reports provider secret readiness without exposing secret values", async () => {
    const fleet = testFleet(undefined, {}, { HETZNER_TOKEN: "hcloud-token" });
    const hetzner = await fleet.fetch(request("GET", "/v1/providers/hetzner/readiness"));
    expect(hetzner.status).toBe(200);
    await expect(hetzner.json()).resolves.toMatchObject({
      provider: "hetzner",
      configured: true,
      missing: [],
    });

    const azure = await fleet.fetch(request("GET", "/v1/providers/azure/readiness"));
    expect(azure.status).toBe(200);
    await expect(azure.json()).resolves.toMatchObject({
      provider: "azure",
      configured: false,
      missing: [
        "AZURE_TENANT_ID",
        "AZURE_CLIENT_ID",
        "AZURE_CLIENT_SECRET",
        "AZURE_SUBSCRIPTION_ID",
      ],
    });
  });

  it("fails brokered Azure leases with provider_not_configured before constructing Azure", async () => {
    const fleet = testFleet();
    const response = await fleet.fetch(
      request("POST", "/v1/leases", {
        headers: {
          "x-crabbox-owner": "peter@example.com",
          "x-crabbox-org": "openclaw",
        },
        body: {
          leaseID: "cbx_abcdef123456",
          provider: "azure",
          target: "windows",
          windowsMode: "normal",
          class: "standard",
          serverType: "Standard_D16ads_v6",
          sshPublicKey: "ssh-ed25519 test",
        },
      }),
    );
    expect(response.status).toBe(424);
    await expect(response.json()).resolves.toMatchObject({
      error: "provider_not_configured",
      provider: "azure",
      missing: expect.arrayContaining(["AZURE_TENANT_ID"]),
    });
  });
});

async function startGitHubLogin(env: Partial<Env> = {}): Promise<{
  fleet: FleetDurableObject;
  loginID: string;
  pollSecret: string;
  state: string;
}> {
  const storage = new MemoryStorage();
  const fleet = new FleetDurableObject(
    { storage } as unknown as DurableObjectState,
    {
      CRABBOX_DEFAULT_ORG: "openclaw",
      CRABBOX_GITHUB_CLIENT_ID: "github-client",
      CRABBOX_GITHUB_CLIENT_SECRET: "github-secret",
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SESSION_SECRET: "session-secret",
      ...env,
    } as Env,
  );
  const pollSecret = "local-poll-secret";
  const start = await fleet.fetch(
    request("POST", "/v1/auth/github/start", {
      body: {
        pollSecretHash: await sha256HexForTest(pollSecret),
        provider: "aws",
      },
    }),
  );
  expect(start.status).toBe(200);
  const body = (await start.json()) as { loginID: string; url: string };
  const url = new URL(body.url);
  const state = url.searchParams.get("state");
  expect(state).toBeTruthy();
  return { fleet, loginID: body.loginID, pollSecret, state: state || "" };
}

function githubFetchMock({
  member,
  teams = [],
}: {
  member: boolean;
  teams?: Array<{ slug: string; organization: { login: string } }>;
}) {
  return vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
    const url =
      typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
    if (url === "https://github.com/login/oauth/access_token") {
      return jsonResponse({ access_token: "github-access-token" });
    }
    if (url === "https://api.github.com/user") {
      return jsonResponse({ login: "friend", name: "Friendly User", email: null });
    }
    if (url === "https://api.github.com/user/emails") {
      return jsonResponse([{ email: "friend@example.com", primary: true, verified: true }]);
    }
    if (url === "https://api.github.com/user/memberships/orgs/openclaw") {
      return member
        ? jsonResponse({ state: "active", organization: { login: "openclaw" } })
        : jsonResponse({ message: "Not Found" }, 404);
    }
    if (url === "https://api.github.com/user/teams?per_page=100&page=1") {
      return jsonResponse(teams);
    }
    return jsonResponse({ message: `unexpected ${url}` }, 500);
  });
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function testFleet(
  storage = new MemoryStorage(),
  providers = {},
  env: Partial<Env> = {},
): FleetDurableObject {
  return new FleetDurableObject(
    { storage } as unknown as DurableObjectState,
    { CRABBOX_DEFAULT_ORG: "default-org", ...env } as Env,
    providers,
  );
}

function fakeProvider(
  onCreate?: (config: LeaseConfig) => void,
  result: {
    provider?: "hetzner" | "aws" | "gcp";
    serverType?: string;
    cloudID?: string;
    market?: string;
    attempts?: ProvisioningAttempt[];
  } = {},
) {
  return {
    async listCrabboxServers() {
      return [];
    },
    async createServerWithFallback(config: LeaseConfig, _leaseID: string, slug: string) {
      onCreate?.(config);
      return {
        server: {
          provider: result.provider ?? "hetzner",
          id: 123,
          cloudID: result.cloudID ?? "123",
          name: `crabbox-${slug}`,
          status: "running",
          serverType: result.serverType ?? "cpx62",
          host: "192.0.2.10",
          region:
            result.provider === "aws"
              ? "eu-west-2"
              : result.provider === "gcp"
                ? "us-central1-a"
                : undefined,
          labels: {},
        },
        serverType: result.serverType ?? "cpx62",
        market: result.market,
        attempts: result.attempts,
      };
    },
    async deleteServer() {},
    async createImage(_instanceID: string, name: string) {
      return { id: "ami-000000000001", name, state: "pending", region: "eu-west-1" };
    },
    async getImage(imageID: string) {
      return {
        id: imageID,
        name: "openclaw-crabbox-test",
        state: "available",
        region: "eu-west-1",
      };
    },
    async deleteSSHKey() {},
    async hourlyPriceUSD() {
      return 0.1;
    },
  };
}

function testLease(overrides: Partial<LeaseRecord>): LeaseRecord {
  return {
    id: "cbx_000000000000",
    provider: "hetzner",
    cloudID: "123",
    owner: "peter@example.com",
    org: "openclaw",
    profile: "default",
    class: "beast",
    serverType: "ccx63",
    serverID: 123,
    serverName: "crabbox-blue-lobster",
    providerKey: "crabbox-cbx-000000000000",
    host: "192.0.2.1",
    sshUser: "crabbox",
    sshPort: "2222",
    sshFallbackPorts: ["22"],
    workRoot: "/work/crabbox",
    keep: true,
    ttlSeconds: 5400,
    estimatedHourlyUSD: 1,
    maxEstimatedUSD: 1.5,
    state: "active",
    createdAt: "2026-05-01T00:00:00.000Z",
    updatedAt: "2026-05-01T00:00:00.000Z",
    expiresAt: "2026-05-01T01:30:00.000Z",
    ...overrides,
  };
}

type CodePortalRuntime = {
  elements: Record<string, DOMElementStub>;
  fetches: string[];
  timers: Array<{ delay: number }>;
};

type DOMElementStub = {
  dataset: Record<string, string>;
  textContent: string;
  disabled: boolean;
  addEventListener: ReturnType<typeof vi.fn>;
};

function elementStub(textContent = ""): DOMElementStub {
  return {
    dataset: {},
    textContent,
    disabled: false,
    addEventListener: vi.fn<() => void>(),
  };
}

async function runCodePortalScript(
  page: string,
  response: { ok: boolean; status: number; json: () => Promise<unknown> },
): Promise<CodePortalRuntime> {
  const script = inlineScript(page, 'const status = document.getElementById("code-status")');
  const elements: Record<string, DOMElementStub> = {
    "code-status": elementStub("checking bridge"),
    "code-hint": elementStub("Run the command below."),
    "code-reload": elementStub(),
    "code-copy": elementStub(),
    "code-bridge-cmd": elementStub("crabbox code --id blue-lobster --open"),
  };
  const fetches: string[] = [];
  const timers: Array<{ delay: number }> = [];
  const context = createContext({
    URL,
    document: {
      getElementById: (id: string) => elements[id] ?? null,
      createRange: () => ({ selectNodeContents: vi.fn<(element: unknown) => void>() }),
    },
    fetch: vi.fn<(url: URL) => Promise<typeof response>>(async (url) => {
      fetches.push(url.toString());
      return response;
    }),
    navigator: { clipboard: { writeText: vi.fn<(text: string) => Promise<void>>() } },
    window: {
      location: { href: "https://example.test/portal/leases/blue-lobster/code/" },
      clearTimeout: vi.fn<(timer?: unknown) => void>(),
      setTimeout: (_callback: () => void, delay: number) => {
        timers.push({ delay });
        return timers.length;
      },
      addEventListener: vi.fn<() => void>(),
      getSelection: () => ({
        removeAllRanges: vi.fn<() => void>(),
        addRange: vi.fn<(range: unknown) => void>(),
      }),
    },
  });

  new Script(script).runInContext(context);
  await new Promise((resolve) => setTimeout(resolve, 0));
  await Promise.resolve();
  return { elements, fetches, timers };
}

function inlineScript(page: string, marker: string): string {
  const scripts = [...page.matchAll(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/g)];
  const match = scripts.find((script) => script[1]?.includes(marker));
  if (!match?.[1]) {
    throw new Error(`script marker not found: ${marker}`);
  }
  return match[1];
}

function testRun(overrides: Partial<RunRecord>): RunRecord {
  return {
    id: "run_000000000000",
    leaseID: "cbx_000000000000",
    owner: "peter@example.com",
    org: "openclaw",
    provider: "hetzner",
    class: "standard",
    serverType: "cpx62",
    command: ["echo", "ok"],
    state: "running",
    logBytes: 0,
    logTruncated: false,
    startedAt: "2026-05-01T00:00:00.000Z",
    ...overrides,
  };
}

function request(
  method: string,
  path: string,
  init: { headers?: Record<string, string>; body?: unknown } = {},
): Request {
  return new Request(`https://crabbox.test${path}`, {
    method,
    headers: {
      ...(init.body === undefined ? {} : { "content-type": "application/json" }),
      ...init.headers,
    },
    body: init.body === undefined ? undefined : JSON.stringify(init.body),
  });
}

async function sha256HexForTest(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}
