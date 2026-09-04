import { afterEach, describe, expect, it, vi } from "vitest";

import { adminGrantVersion, issueUserToken, sha256Hex } from "../src/auth";
import type { BridgeTicketKind, LeaseBridgeTicketRecord } from "../src/bridge-tickets";
import { CloudflareCoordinatorRuntime } from "../src/coordinator-runtime";
import { FleetCoordinator } from "../src/fleet";
import { orgKeyForLabel } from "../src/org-identity";
import type { Env, LeaseRecord } from "../src/types";
import { ProvisioningTestStorage } from "./provisioning-fixtures";

afterEach(() => {
  vi.unstubAllGlobals();
});

class TicketStorage extends ProvisioningTestStorage {}

class TicketRuntime extends CloudflareCoordinatorRuntime {
  readonly accepted: unknown[] = [];

  constructor(storage: TicketStorage) {
    super({
      storage,
      blockConcurrencyWhile<T>(callback: () => Promise<T>): Promise<T> {
        return callback();
      },
      waitUntil() {},
    } as unknown as DurableObjectState);
  }
  override createWebSocketUpgrade() {
    const socket = {
      readyState: 1,
      send: vi.fn<(data: string) => void>(),
      close: vi.fn<() => void>(),
      serializeAttachment: vi.fn<(attachment: unknown) => void>(),
    } as unknown as WebSocket;
    // Node's Response does not support 101; accepted sockets are asserted separately.
    return { socket, response: new Response(null) };
  }
  override acceptWebSocket(socket: WebSocket, attachment: unknown): void {
    this.setSocketAttachment(socket, attachment);
    this.accepted.push(attachment);
  }
}

const cases = [
  {
    kind: "webvnc-agent",
    path: "webvnc",
    endpoint: "agent",
    namespace: "webvnc-ticket:",
    prefix: "wvnc_",
    body: {},
  },
  {
    kind: "code-agent",
    path: "code",
    endpoint: "agent",
    namespace: "code-ticket:",
    prefix: "code_",
    body: {},
  },
  {
    kind: "egress-host",
    path: "egress",
    endpoint: "host",
    namespace: "egress-ticket:",
    prefix: "egress_",
    body: { role: "host", sessionID: "egress_contract", allow: ["example.com"] },
  },
  {
    kind: "egress-client",
    path: "egress",
    endpoint: "client",
    namespace: "egress-ticket:",
    prefix: "egress_",
    body: { role: "client", sessionID: "egress_contract", allow: ["example.com"] },
  },
] satisfies {
  kind: BridgeTicketKind;
  path: string;
  endpoint: string;
  namespace: string;
  prefix: string;
  body: object;
}[];

type TicketCase = (typeof cases)[number];

async function fixture(item: TicketCase) {
  const storage = new TicketStorage();
  const env = {
    CRABBOX_DEFAULT_ORG: "example-org",
    CRABBOX_SHARED_TOKEN: "fixture-shared-token",
    CRABBOX_ADMIN_TOKEN: "fixture-admin-token",
  } as Env;
  const lease: LeaseRecord = {
    id: "cbx_000000000001",
    slug: "ticket-contract",
    provider: "hetzner",
    target: "linux",
    desktop: true,
    code: true,
    cloudID: "123",
    owner: "alice@example.com",
    org: orgKeyForLabel("example-org"),
    profile: "default",
    class: "beast",
    serverType: "ccx63",
    serverID: 123,
    serverName: "my-app",
    providerKey: "my-app-key",
    host: "192.0.2.1",
    sshUser: "crabbox",
    sshPort: "2222",
    workRoot: "/work/my-app",
    keep: true,
    ttlSeconds: 5400,
    estimatedHourlyUSD: 1,
    maxEstimatedUSD: 1.5,
    state: "active",
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    expiresAt: new Date(Date.now() + 3600000).toISOString(),
    share: { users: { "manager@example.com": "manage", "viewer@example.com": "use" } },
  };
  await storage.put(`lease:${lease.id}`, lease);
  const runtime = new TicketRuntime(storage);
  const fleet = new FleetCoordinator(runtime, env);
  const headers = {
    authorization: `Bearer ${env.CRABBOX_SHARED_TOKEN}`,
    "x-crabbox-auth": "bearer",
    "x-crabbox-owner": lease.owner,
    "x-crabbox-org": "example-org",
  };
  const create = (extra: Record<string, string> = {}, method = "POST", body: object = item.body) =>
    fleet.fetch(
      new Request(`https://coordinator.test/v1/leases/${lease.id}/${item.path}/ticket`, {
        method,
        headers: { "content-type": "application/json", ...headers, ...extra },
        ...(method === "POST" ? { body: JSON.stringify(body) } : {}),
      }),
    );
  const mint = async (extra: Record<string, string> = {}, body: object = item.body) => {
    const response = await create(extra, "POST", body);
    expect(response.status).toBe(200);
    return (await response.json()) as {
      ticket: string;
      leaseID: string;
      expiresAt: string;
      role?: string;
      sessionID?: string;
    };
  };
  const connect = async (
    ticket: string,
    identifier = lease.id,
    target = item,
    coordinator = fleet,
  ) =>
    coordinator.fetch(
      new Request(
        `https://coordinator.test/v1/leases/${identifier}/${target.path}/${target.endpoint}`,
        {
          headers: {
            upgrade: "websocket",
            "x-crabbox-bridge-ticket": ticket,
            "x-crabbox-admin-grant-version": await adminGrantVersion(env),
          },
        },
      ),
    );
  return { storage, env, lease, runtime, fleet, create, mint, connect };
}

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

describe.each(cases)("$kind ticket contract", (item) => {
  it("keeps its persisted shape, namespace, TTL, principal and one-use behavior", async () => {
    const f = await fixture(item);
    const ticket = await f.mint();
    expect(ticket.ticket).toMatch(new RegExp(`^${item.prefix}[a-f0-9]{32}$`));
    expect(Object.keys(ticket).toSorted()).toEqual(
      (item.path === "egress"
        ? ["ticket", "leaseID", "role", "sessionID", "expiresAt"]
        : ["ticket", "leaseID", "expiresAt"]
      ).toSorted(),
    );
    const stored = await f.storage.get<LeaseBridgeTicketRecord>(
      `${item.namespace}${ticket.ticket}`,
    );
    expect(stored).toMatchObject({
      leaseID: f.lease.id,
      owner: f.lease.owner,
      org: f.lease.org,
      admin: false,
      auth: "bearer",
    });
    expect(stored).not.toHaveProperty("kind");
    expect(Date.parse(stored!.expiresAt) - Date.parse(stored!.createdAt)).toBe(120000);
    expect((await f.connect(ticket.ticket, f.lease.slug)).status).toBe(200);
    expect(f.runtime.accepted).toEqual([
      expect.objectContaining({
        kind: item.kind,
        leaseID: f.lease.id,
        owner: f.lease.owner,
        org: f.lease.org,
      }),
    ]);
    expect(f.storage.values.has(`${item.namespace}${ticket.ticket}`)).toBe(false);
    expect((await f.connect(ticket.ticket)).status).toBe(401);
  });

  it("accepts at most one concurrent attempt while a storage read is suspended", async () => {
    const f = await fixture(item);
    const { ticket } = await f.mint();
    const entered = deferred();
    const release = deferred();
    const reads = vi.fn<() => void>();
    f.storage.beforeGet = async (key) => {
      if (key === `${item.namespace}${ticket}`) {
        reads();
        entered.resolve();
        await release.promise;
      }
    };
    const first = f.connect(ticket);
    await entered.promise;
    const remaining = Array.from({ length: 7 }, () => f.connect(ticket));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(reads).toHaveBeenCalledTimes(1);
    release.resolve();
    const responses = await Promise.all([first, ...remaining]);
    expect(responses.map((r) => r.status)).toEqual([200, ...Array(7).fill(401)]);
    expect(f.runtime.accepted).toHaveLength(1);
  });

  it("releases the lock after a storage failure without consuming the ticket", async () => {
    const f = await fixture(item);
    const { ticket } = await f.mint();
    f.storage.beforeGet = async (key) => {
      if (key === `${item.namespace}${ticket}`) throw new Error("storage unavailable");
    };
    expect((await f.connect(ticket)).status).toBe(500);
    expect(f.storage.values.has(`${item.namespace}${ticket}`)).toBe(true);
    f.storage.beforeGet = async () => {};
    expect((await f.connect(ticket)).status).toBe(200);
  });

  it("preserves a ticket after wrong lease and wrong kind/role attempts", async () => {
    const f = await fixture(item);
    const { ticket } = await f.mint();
    expect((await f.connect(ticket, "different-lease")).status).toBe(404);
    await Promise.all(
      cases
        .filter((other) => other.kind !== item.kind)
        .map(async (other) => {
          expect((await f.connect(ticket, f.lease.id, other)).status).toBe(401);
        }),
    );
    expect(f.storage.values.has(`${item.namespace}${ticket}`)).toBe(true);
    expect((await f.connect(ticket)).status).toBe(200);
  });

  it("fails closed on missing records, malformed tokens and mismatched stored tokens without consuming another record", async () => {
    const f = await fixture(item);
    const { ticket } = await f.mint();
    expect((await f.connect("malformed")).status).toBe(401);
    expect((await f.connect(`${item.prefix}${"0".repeat(32)}`)).status).toBe(401);
    const key = `${item.namespace}${ticket}`;
    const stored = await f.storage.get<LeaseBridgeTicketRecord>(key);
    await f.storage.put(key, { ...stored, ticket: "different-token" });
    expect((await f.connect(ticket)).status).toBe(401);
    expect(f.storage.values.has(key)).toBe(true);
  });

  it("deletes expired tickets before considering endpoint binding", async () => {
    const f = await fixture(item);
    const { ticket } = await f.mint();
    const key = `${item.namespace}${ticket}`;
    await f.storage.put(key, {
      ...(await f.storage.get<LeaseBridgeTicketRecord>(key)),
      expiresAt: new Date(0).toISOString(),
    });
    expect((await f.connect(ticket, "different-lease")).status).toBe(401);
    expect(f.storage.values.has(key)).toBe(false);
  });

  it("cleans expired records only in its namespace before minting", async () => {
    const f = await fixture(item);
    const { ticket } = await f.mint();
    const key = `${item.namespace}${ticket}`;
    const expired = {
      ...(await f.storage.get<LeaseBridgeTicketRecord>(key)),
      expiresAt: new Date(0).toISOString(),
    };
    await f.storage.put(key, expired);
    await f.storage.put("unrelated-ticket:expired", expired);
    await f.mint();
    expect(f.storage.values.has(key)).toBe(false);
    expect(f.storage.values.has("unrelated-ticket:expired")).toBe(true);
  });

  it("accepts manage sharing but rejects use-only issuance and revoked manage access", async () => {
    const f = await fixture(item);
    expect((await f.create({ "x-crabbox-owner": "viewer@example.com" })).status).toBe(403);
    const { ticket } = await f.mint({ "x-crabbox-owner": "manager@example.com" });
    await f.storage.put(`lease:${f.lease.id}`, {
      ...f.lease,
      share: { users: { "manager@example.com": "use" } },
    });
    expect((await f.connect(ticket)).status).toBe(401);
    expect(f.storage.values.has(`${item.namespace}${ticket}`)).toBe(false);
  });

  it.each(["ticket", "lease"])("rejects legacy org identity on the %s", async (record) => {
    const f = await fixture(item);
    const { ticket } = await f.mint();
    const key = record === "lease" ? `lease:${f.lease.id}` : `${item.namespace}${ticket}`;
    await f.storage.put(key, {
      ...(await f.storage.get<LeaseBridgeTicketRecord | LeaseRecord>(key)),
      org: "example-org",
    });
    expect((await f.connect(ticket)).status).toBe(401);
    expect(f.storage.values.has(`${item.namespace}${ticket}`)).toBe(false);
  });

  it("revalidates shared-token rotation and cached admin-grant versions", async () => {
    const f = await fixture(item);
    const shared = await f.mint();
    f.env.CRABBOX_SHARED_TOKEN = "rotated-shared-token";
    expect((await f.connect(shared.ticket)).status).toBe(401);
    const adminHeaders = {
      authorization: `Bearer ${f.env.CRABBOX_ADMIN_TOKEN}`,
      "x-crabbox-admin": "true",
      "x-crabbox-owner": "admin@example.com",
      "x-crabbox-org": "admin-org",
      "x-crabbox-admin-grant-version": await adminGrantVersion(f.env),
    };
    const currentAdmin = await f.mint(adminHeaders);
    expect((await f.connect(currentAdmin.ticket)).status).toBe(200);
    const admin = await f.mint(adminHeaders);
    f.env.CRABBOX_ADMIN_TOKEN = "rotated-admin-token";
    expect((await f.connect(admin.ticket)).status).toBe(401);
    expect(f.storage.values.has(`${item.namespace}${admin.ticket}`)).toBe(false);
  });

  it.each([
    "current",
    "membership lost",
    "GitHub error",
    "identity changed",
    "logout",
    "emergency revocation",
    "missing binding",
  ])("revalidates GitHub grants: %s", async (outcome) => {
    const f = await fixture(item);
    Object.assign(f.env, {
      CRABBOX_SESSION_SECRET: "fixture-session-secret",
      CRABBOX_GITHUB_ALLOWED_ORG: "example-org",
      CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "0",
    });
    const owner = "github:12345";
    await f.storage.put(`lease:${f.lease.id}`, { ...f.lease, owner });
    const token = await issueUserToken(f.env, {
      owner,
      ownerSource: "github-verified-email",
      org: "example-org",
      login: "alice",
      githubAccessToken: "fixture-github-access-token",
    });
    const payload = JSON.parse(
      Buffer.from(token.slice("cbxu_".length).split(".")[0]!, "base64url").toString(),
    ) as {
      jti: string;
      githubCredential: string;
      exp: number;
    };
    const { ticket } = await f.mint({
      authorization: `Bearer ${token}`,
      "x-crabbox-auth": "github",
      "x-crabbox-owner": owner,
      "x-crabbox-github-login": "alice",
      "x-crabbox-github-token-id": payload.jti,
      "x-crabbox-github-sealed-credential": payload.githubCredential,
      "x-crabbox-token-expires-at": new Date(payload.exp * 1000).toISOString(),
    });
    const key = `${item.namespace}${ticket}`;
    expect(JSON.stringify(await f.storage.get(key))).not.toContain("fixture-github-access-token");
    if (outcome === "logout") {
      const portalSessionHash = await sha256Hex(token);
      await f.storage.put(`code-viewer-session-revocation:${portalSessionHash}`, {
        portalSessionHash,
        createdAt: new Date().toISOString(),
        expiresAt: f.lease.expiresAt,
      });
    }
    if (outcome === "emergency revocation") f.env.CRABBOX_GITHUB_REVOKED_USERS = owner;
    if (outcome === "missing binding") {
      const stored = await f.storage.get<LeaseBridgeTicketRecord>(key);
      delete stored!.portalSessionHash;
      await f.storage.put(key, stored);
    }
    const github = vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
      if (outcome === "GitHub error") throw new Error("GitHub unavailable");
      const url = String(input);
      if (url === "https://api.github.com/user") {
        return Response.json({
          id: outcome === "identity changed" ? 99999 : 12345,
          login: "alice",
        });
      }
      if (url === "https://api.github.com/user/memberships/orgs/example-org") {
        return outcome === "membership lost"
          ? new Response(null, { status: 404 })
          : Response.json({ state: "active", organization: { login: "example-org" } });
      }
      throw new Error(`unexpected GitHub request: ${url}`);
    });
    vi.stubGlobal("fetch", github);
    expect((await f.connect(ticket)).status).toBe(outcome === "current" ? 200 : 401);
    expect(f.storage.values.has(key)).toBe(false);
    expect(github.mock.calls.length > 0).toBe(
      !["logout", "emergency revocation", "missing binding"].includes(outcome),
    );
  });

  it("preserves method, unavailable capability, and invalid grant responses", async () => {
    const f = await fixture(item);
    expect((await f.create({}, "GET")).status).toBe(404);
    expect((await f.create({ "x-crabbox-auth": "github" })).status).toBe(401);
    await f.storage.put(`lease:${f.lease.id}`, { ...f.lease, state: "released" });
    const unavailable = await f.create();
    expect(unavailable.status).toBe(409);
    expect(await unavailable.json()).toEqual({
      error: `${item.path}_unavailable`,
      message: "lease is not active",
    });
  });

  it("consumes persisted tickets after a coordinator restart without schema migration", async () => {
    const f = await fixture(item);
    const { ticket } = await f.mint();
    const restarted = new FleetCoordinator(new TicketRuntime(f.storage), f.env);
    expect((await f.connect(ticket, f.lease.id, item, restarted)).status).toBe(200);
    expect((await f.connect(ticket, f.lease.id, item, restarted)).status).toBe(401);
  });
});

it.each(cases.filter((item) => item.path !== "egress"))(
  "$kind consumes the ticket before checking capability at connection",
  async (item) => {
    const f = await fixture(item);
    const { ticket } = await f.mint();
    await f.storage.put(`lease:${f.lease.id}`, { ...f.lease, desktop: false, code: false });
    expect((await f.connect(ticket)).status).toBe(409);
    expect(f.storage.values.has(`${item.namespace}${ticket}`)).toBe(false);
    expect((await f.create()).status).toBe(409);
  },
);

it.each(cases.filter((item) => item.path === "egress"))(
  "$kind retains role/session/allowlist binding and replacement across restart",
  async (item) => {
    const f = await fixture(item);
    const previous = await f.mint();
    const current = await f.mint(
      {},
      {
        role: item.endpoint,
        sessionId: "egress_replacement",
        profile: " custom ",
        allow: [" example.com ", "example.com", "api.example.com"],
      },
    );
    const record = await f.storage.get(`${item.namespace}${current.ticket}`);
    expect(record).toMatchObject({
      role: item.endpoint,
      sessionID: "egress_replacement",
      profile: "custom",
      allow: ["example.com", "api.example.com"],
    });
    const restarted = new FleetCoordinator(new TicketRuntime(f.storage), f.env);
    expect((await f.connect(previous.ticket, f.lease.id, item, restarted)).status).toBe(409);
    expect(f.storage.values.has(`${item.namespace}${previous.ticket}`)).toBe(false);
    expect((await f.connect(current.ticket, f.lease.id, item, restarted)).status).toBe(200);
    const beforeRemint = (await f.storage.list({ prefix: item.namespace })).size;
    expect((await f.create()).status).toBe(409);
    expect((await f.storage.list({ prefix: item.namespace })).size).toBe(beforeRemint);
  },
);
