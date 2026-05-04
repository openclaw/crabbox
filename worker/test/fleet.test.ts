import { afterEach, describe, expect, it, vi } from "vitest";

import {
  FleetDurableObject,
  flushPendingWebVNC,
  forwardOrBufferWebVNC,
  resetWebVNCBridge,
  type WebVNCBuffer,
} from "../src/fleet";
import type { Env, LeaseRecord, ProvisioningAttempt, RunRecord } from "../src/types";

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

  it("mints brokered Tailscale keys, records non-secret metadata, and accepts readiness updates", async () => {
    const storage = new MemoryStorage();
    let providerConfig:
      | {
          tailscale?: boolean;
          tailscaleAuthKey?: string;
          tailscaleHostname?: string;
          tailscaleTags?: string[];
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
    });
    expect(JSON.stringify(lease)).not.toContain("tskey-oneoff");
    expect(providerConfig).toMatchObject({
      tailscale: true,
      tailscaleAuthKey: "tskey-oneoff",
      tailscaleHostname: "crabbox-blue-lobster",
      tailscaleTags: ["tag:ci"],
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
          state: "ready",
        },
      }),
    );
    expect(update.status).toBe(200);
    const updated = (await update.json()) as { lease: LeaseRecord };
    expect(updated.lease.tailscale?.ipv4).toBe("100.64.0.10");
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

  it("records requested type and provider fallback attempts on resolved leases", async () => {
    const attempts: ProvisioningAttempt[] = [
      {
        serverType: "c7a.48xlarge",
        market: "spot",
        category: "policy",
        message: "InvalidParameterCombination: not eligible",
      },
    ];
    const fleet = testFleet(new MemoryStorage(), {
      aws: fakeProvider(undefined, {
        provider: "aws",
        serverType: "c7i.24xlarge",
        cloudID: "i-123",
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
    expect(lease.provisioningAttempts).toEqual(attempts);
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
        body: { idleTimeoutSeconds: 2400 },
      }),
    );
    expect(heartbeat.status).toBe(200);
    const { lease } = (await heartbeat.json()) as { lease: LeaseRecord };
    expect(lease.id).toBe("cbx_000000000001");
    expect(lease.slug).toBe("blue-lobster");
    expect(lease.idleTimeoutSeconds).toBe(2400);
    expect(Date.parse(lease.expiresAt)).toBeGreaterThan(expiresAt.getTime());
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
    expect(body).toContain("blue-lobster");
    expect(body).toContain("/portal/leases/cbx_000000000001/vnc");
    expect(body).not.toContain("amber-krill");
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
    expect(pageBody).toContain("crabbox webvnc --id blue-lobster --open");
    expect(pageBody).toContain("/portal/assets/novnc/rfb.js");
    expect(pageBody).toContain("function scheduleRetry");
    expect(pageBody).toContain('fragment.get("username")');
    expect(pageBody).toContain('types.includes("username")');
    expect(pageBody).not.toContain("cdn.jsdelivr.net");

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

  it("buffers initial WebVNC bridge bytes until the viewer attaches", () => {
    const buffers = new Map<string, WebVNCBuffer>();
    const sent: Array<string | ArrayBuffer> = [];
    const viewer = {
      readyState: WebSocket.OPEN,
      send(data: string | ArrayBuffer) {
        sent.push(data);
      },
    } as WebSocket;

    forwardOrBufferWebVNC("RFB 003.008\n", undefined, buffers, "cbx_000000000001");
    expect(sent).toEqual([]);
    expect(buffers.get("cbx_000000000001")).toMatchObject({
      chunks: ["RFB 003.008\n"],
      bytes: 12,
    });

    flushPendingWebVNC(buffers, "cbx_000000000001", viewer);
    expect(sent).toEqual(["RFB 003.008\n"]);
    expect(buffers.has("cbx_000000000001")).toBe(false);
  });

  it("resets the WebVNC bridge when the viewer goes away", () => {
    const buffers = new Map<string, WebVNCBuffer>();
    buffers.set("cbx_000000000001", { chunks: ["RFB 003.008\n"], bytes: 12 });
    const closed: Array<{ code: number; reason: string }> = [];
    const agents = new Map<string, WebSocket>();
    agents.set("cbx_000000000001", {
      readyState: WebSocket.OPEN,
      close(code: number, reason: string) {
        closed.push({ code, reason });
      },
    } as WebSocket);

    resetWebVNCBridge(agents, buffers, "cbx_000000000001", 1011, "WebVNC viewer disconnected");

    expect(closed).toEqual([{ code: 1011, reason: "WebVNC viewer disconnected" }]);
    expect(agents.has("cbx_000000000001")).toBe(false);
    expect(buffers.has("cbx_000000000001")).toBe(false);
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
      run: { state: string; logBytes: number; results?: { tests: number } };
    };
    expect(finished.run.state).toBe("succeeded");
    expect(finished.run.logBytes).toBe(3);
    expect(finished.run.results?.tests).toBe(2);

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
  onCreate?: (config: {
    awsSSHCIDRs: string[];
    tailscale?: boolean;
    tailscaleAuthKey?: string;
    tailscaleHostname?: string;
    tailscaleTags?: string[];
  }) => void,
  result: {
    provider?: "hetzner" | "aws";
    serverType?: string;
    cloudID?: string;
    attempts?: ProvisioningAttempt[];
  } = {},
) {
  return {
    async listCrabboxServers() {
      return [];
    },
    async createServerWithFallback(
      config: { awsSSHCIDRs: string[] },
      _leaseID: string,
      slug: string,
    ) {
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
          labels: {},
        },
        serverType: result.serverType ?? "cpx62",
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
