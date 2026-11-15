import { describe, expect, it, vi, afterEach } from "vitest";

import { prepareCoordinatorRequest } from "../src/coordinator-entry";
import type { CoordinatorRuntime } from "../src/coordinator-runtime";
import { sha256Hex } from "../src/encoding";
import { leaseProviderCleanupConfirmed } from "../src/lease-cleanup";
import { orgKeyForLabel } from "../src/org-identity";
import { publicLeaseRecord } from "../src/org-records";
import type { PoolAccessGrant } from "../src/ready-pool-access";
import { CloudflareServeRelayBackend } from "../src/serve-relay";
import { ServeRelayControl } from "../src/serve-relay-control";
import { SSHHostCoordinator, hostPrefix, type SSHHost } from "../src/ssh-host";
import { sshHostCloudProvider } from "../src/ssh-host-provider";
// oxlint-disable eslint/no-await-in-loop -- ordered lifecycle assertions.
import { mintHostRelay, authorizeHostRelay } from "../src/ssh-host-relay";
import type { Env, LeaseRecord } from "../src/types";
import { ProvisioningTestStorage } from "./provisioning-fixtures";
const hostID = "11111111-1111-4111-8111-111111111111";
const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";
const env = { CRABBOX_DEFAULT_ORG: "example-org" } as Env;
function req(
  owner = "operator@example.com",
  admin = false,
  org = "example-org",
  method = "POST",
  body: unknown = {},
) {
  return new Request("http://localhost/v1/ssh-hosts", {
    method,
    headers: {
      "x-crabbox-owner": owner,
      "x-crabbox-org": org,
      "x-crabbox-admin": admin ? "true" : "false",
      "content-type": "application/json",
    },
    ...(method === "GET" ? {} : { body: JSON.stringify(body) }),
  });
}
function fixture() {
  const storage = new ProvisioningTestStorage();
  const runtime = {
    storage,
    runExclusive: async <T>(f: () => Promise<T>) => f(),
  } as CoordinatorRuntime;
  const coordinator = new SSHHostCoordinator(runtime, env);
  const route = (
    parts: string[],
    body: unknown = {},
    owner = "operator@example.com",
    admin = true,
    method = "POST",
    org = "example-org",
  ) => coordinator.route(req(owner, admin, org, method, body), ["v1", "ssh-hosts", ...parts]);
  const create = (id = "cbx_000000000001", owner = "borrower@example.com", extra = {}) =>
    coordinator.create(req(owner), {
      provider: "ssh-host",
      leaseID: id,
      sshPublicKey: key,
      target: "linux",
      architecture: "amd64",
      ...extra,
    });
  const beat = (report?: unknown) => route([hostID, "heartbeat"], { hostKey: key, report });
  const setup = async () => {
    expect(
      (
        await route([], {
          id: hostID,
          name: "builder",
          target: "linux",
          architecture: "amd64",
          capabilities: ["gpu"],
          template: "test-v1",
          runtime: "native-user",
        })
      ).status,
    ).toBe(201);
    expect((await beat()).ok).toBe(true);
    expect((await route([hostID], { enabled: true }, undefined, true, "PATCH")).ok).toBe(true);
  };
  const lease = () => storage.get<LeaseRecord>("lease:cbx_000000000001");
  const report = (state: string, extra = {}) => ({
    leaseID: "cbx_000000000001",
    generation: 1,
    accessGeneration: 0,
    state,
    ...extra,
  });
  return { storage, runtime, coordinator, route, create, beat, setup, lease, report };
}
afterEach(() => vi.useRealTimers());
describe("managed host allocation", () => {
  it("requires operator enrollment and starts disabled", async () => {
    const f = fixture();
    expect((await f.route([], {}, "borrower@example.com", false)).status).toBe(403);
    await f.setup();
    expect((await f.route([hostID], { enabled: false }, undefined, true, "PATCH")).ok).toBe(true);
    expect((await f.create()).status).toBe(409);
  });
  it.each([false, true])(
    "allocates exclusively across competing borrowers and retries (defer first hash=%s)",
    async (deferFirstHash) => {
      const f = fixture();
      await f.setup();
      const borrowers = Array.from({ length: 12 }, (_, i) => ({
        id: `cbx_${(i + 1).toString(16).padStart(12, "0")}`,
        owner: `borrower-${i}@example.com`,
      }));
      const release = Promise.withResolvers<void>();
      const digest = crypto.subtle.digest.bind(crypto.subtle);
      let firstHash = true;
      const spy = deferFirstHash
        ? vi.spyOn(crypto.subtle, "digest").mockImplementation(async (...args) => {
            if (firstHash) {
              firstHash = false;
              await release.promise;
            }
            return digest(...args);
          })
        : undefined;
      const pending = borrowers.map(({ id, owner }) => f.create(id, owner));
      try {
        if (deferFirstHash) await Promise.all(pending.slice(1));
        release.resolve();
        const results = await Promise.all(pending);
        expect(results.filter((r) => r.status === 202)).toHaveLength(1);
        expect(results.filter((r) => r.status === 409)).toHaveLength(11);
        expect(results[0]!.status).toBeOneOf(deferFirstHash ? [409] : [202, 409]);
        const winner = borrowers[results.findIndex((r) => r.status === 202)]!;
        const loser = borrowers[results.findIndex((r) => r.status === 409)]!;
        expect((await f.create(winner.id, winner.owner)).status).toBe(202);
        expect((await f.create(loser.id, loser.owner)).status).toBe(409);
        expect((await f.create(winner.id, "other@example.com")).status).toBe(409);
      } finally {
        release.resolve();
        await Promise.allSettled(pending);
        spy?.mockRestore();
      }
    },
  );
  it("matches organization, platform, architecture, and capability", async () => {
    const f = fixture();
    await f.setup();
    for (const extra of [
      { target: "windows" },
      { architecture: "arm64" },
      { hostCapabilities: ["missing"] },
      { sshHost: "other" },
    ])
      expect((await f.create(undefined, undefined, extra)).status).toBe(409);
    expect(
      (
        await f.coordinator.create(req("outsider", false, "other-org"), {
          provider: "ssh-host",
          leaseID: "cbx_000000000001",
          sshPublicKey: key,
        })
      ).status,
    ).toBe(409);
    expect((await f.create(undefined, undefined, { hostCapabilities: ["gpu"] })).status).toBe(202);
  });
  it("does not free custody on missing reports, failure, or stale cleanup", async () => {
    const f = fixture();
    await f.setup();
    await f.create();
    await f.beat();
    expect((await f.create("cbx_000000000002")).status).toBe(409);
    await f.beat(f.report("failed"));
    expect((await f.create("cbx_000000000002")).status).toBe(409);
    expect((await f.beat(f.report("clean", { generation: 2 }))).status).toBe(200);
    expect((await f.create("cbx_000000000002")).status).toBe(409);
    await f.beat(f.report("clean"));
    expect(leaseProviderCleanupConfirmed((await f.lease())!)).toBe(true);
    expect((await f.create("cbx_000000000002")).status).toBe(202);
  });
  it("expires without reallocation until exact cleanup, including coordinator restart", async () => {
    vi.useFakeTimers();
    const f = fixture();
    await f.setup();
    await f.create(undefined, undefined, { ttlSeconds: 60, idleTimeoutSeconds: 60 });
    const l = (await f.lease())!;
    l.expiresAt = new Date(Date.now() + 1000).toISOString();
    await f.storage.put("lease:" + l.id, l);
    vi.advanceTimersByTime(2000);
    const restarted = new SSHHostCoordinator(f.runtime, env);
    const response = await restarted.route(
      req(undefined, true, undefined, "POST", { hostKey: key }),
      ["v1", "ssh-hosts", hostID, "heartbeat"],
    );
    expect((await response.json()).assignment.operation).toBe("clean");
    expect((await f.create("cbx_000000000002")).status).toBe(409);
    await f.beat(f.report("clean"));
    expect((await f.create("cbx_000000000002")).status).toBe(202);
  });
  it("drain prevents new placement, removal refuses allocated hosts", async () => {
    const f = fixture();
    await f.setup();
    await f.create();
    expect((await f.route([hostID], {}, undefined, true, "DELETE")).status).toBe(409);
    await f.route([hostID], { draining: true }, undefined, true, "PATCH");
    await f.coordinator.release((await f.lease())!);
    await f.beat(f.report("clean"));
    expect((await f.create("cbx_000000000002")).status).toBe(409);
    expect((await f.route([hostID], {}, undefined, true, "DELETE")).ok).toBe(true);
  });
  it("atomically rolls back lease and host writes on storage failure", async () => {
    const f = fixture();
    await f.setup();
    f.storage.failKey = hostPrefix + hostID;
    await expect(f.create()).rejects.toThrow("injected storage failure");
    expect(await f.lease()).toBeUndefined();
    expect((await f.storage.get<SSHHost>(hostPrefix + hostID))?.leaseID).toBeUndefined();
    f.storage.failKey = undefined;
    expect((await f.create()).status).toBe(202);
  });
  it("publishes only pinned ready endpoints, hides agent custody in public leases", async () => {
    const f = fixture();
    await f.setup();
    await f.create();
    expect(
      (
        await f.beat(
          f.report("ready", { sshHostKey: key, sshUser: "crabbox", workRoot: "/work/crabbox" }),
        )
      ).ok,
    ).toBe(true);
    const l = (await f.lease())!;
    expect(l.state).toBe("active");
    expect(l.sshJump?.relay).toBe(true);
    expect(publicLeaseRecord(l)).not.toHaveProperty("sshHostAllocation");
    expect(
      (await f.route([hostID, "heartbeat"], { hostKey: key }, "borrower@example.com", false))
        .status,
    ).toBe(404);
  });
});

describe("managed host relay authorization", () => {
  it("binds relay admission to the borrower, operator, and allocation generation", async () => {
    const f = fixture();
    await f.setup();
    await f.create();
    await f.beat(
      f.report("ready", { sshHostKey: key, sshUser: "crabbox", workRoot: "/work/crabbox" }),
    );
    const sessions = new Map<string, string>();
    const relayEnv = {
      ...env,
      serveRelayBackend: {
        prepare: async (d: { sessionID: string }, token: string) => {
          sessions.set(d.sessionID, token);
        },
        alive: async (id: string) => sessions.has(id),
        pendingToken: async (id: string) => sessions.get(id),
        close: async (id: string) => {
          sessions.delete(id);
        },
        attach: async () => new Response(null, { status: 501 }),
      },
    } as Env;
    const l = (await f.lease())!;
    expect((await mintHostRelay(f.storage, relayEnv, req("other@example.com"), l)).status).toBe(
      403,
    );
    const response = await mintHostRelay(f.storage, relayEnv, req("borrower@example.com"), l);
    expect(response.status).toBe(200);
    const { sessionID } = (await response.json()) as { sessionID: string };
    const upgrade = (owner: string, role: string, token?: string) => {
      const r = req(owner, false, "example-org", "GET");
      r.headers.set("upgrade", "websocket");
      if (role === "agent") {
        r.headers.set("x-crabbox-serve-machine", hostID);
        r.headers.set("x-crabbox-relay-token", token ?? "");
      }
      return r;
    };
    expect(
      (
        await authorizeHostRelay(
          f.storage,
          relayEnv,
          upgrade("other@example.com", "client"),
          sessionID,
          "client",
        )
      )?.status,
    ).toBe(403);
    expect(
      (
        await authorizeHostRelay(
          f.storage,
          relayEnv,
          upgrade("borrower@example.com", "agent", sessions.get(sessionID)),
          sessionID,
          "agent",
        )
      )?.status,
    ).toBe(403);
    expect(
      (
        await authorizeHostRelay(
          f.storage,
          relayEnv,
          upgrade("operator@example.com", "agent", "wrong"),
          sessionID,
          "agent",
        )
      )?.status,
    ).toBe(403);
    expect(
      (
        await authorizeHostRelay(
          f.storage,
          relayEnv,
          upgrade("operator@example.com", "agent", sessions.get(sessionID)),
          sessionID,
          "agent",
        )
      )?.status,
    ).toBe(200);
    const current = (await f.lease())!;
    current.sshHostAllocation!.accessGeneration++;
    await f.storage.put("lease:" + l.id, current);
    expect(
      (
        await authorizeHostRelay(
          f.storage,
          relayEnv,
          upgrade("borrower@example.com", "client"),
          sessionID,
          "client",
        )
      )?.status,
    ).toBe(403);
  });
  it("delivers a new generation despite an obsolete agent receipt without accepting it", async () => {
    const f = fixture();
    await f.setup();
    await f.create();
    const l = (await f.lease())!;
    l.sshHostAllocation!.accessGeneration = 1;
    await f.storage.put("lease:" + l.id, l);
    const response = await f.beat(f.report("clean"));
    expect(response.status).toBe(200);
    expect((await response.json()).assignment.accessGeneration).toBe(1);
    expect((await f.lease())!.sshHostAllocation!.cleanGeneration).toBeUndefined();
    expect((await f.create("cbx_000000000002")).status).toBe(409);
  });
});

describe("scoped host credentials", () => {
  const token = `cbxh_${hostID}_1_${"ab".repeat(32)}`;
  const hostInput = {
    id: hostID,
    name: "scoped-builder",
    target: "linux",
    architecture: "amd64",
    capabilities: ["build"],
    template: "tools-v1",
    runtime: "native-user",
    hostKey: key,
  };
  const credential = async (generation = 1, secret = token) => ({
    purpose: "agent" as const,
    version: 1 as const,
    generation,
    tokenHash: await sha256Hex(secret),
    expiresAt: new Date(Date.now() + 86400_000).toISOString(),
  });
  const request = (path: string, method = "POST", body?: unknown, secret = token) =>
    new Request(`https://broker.example.com${path}`, {
      method,
      headers: {
        authorization: `Bearer ${secret}`,
        "content-type": "application/json",
        "x-crabbox-owner": "forged@example.com",
        "x-crabbox-org": "forged",
        "x-crabbox-admin": "true",
        "x-crabbox-auth": "bearer",
      },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
  const beat = async (
    f: ReturnType<typeof fixture>,
    secret = token,
    hostKey = key,
    report?: unknown,
  ) => {
    const r = request(`/v1/ssh-hosts/${hostID}/heartbeat`, "POST", { hostKey, report }, secret);
    const prepared = await prepareCoordinatorRequest(r, env);
    expect("request" in prepared).toBe(true);
    if (!("request" in prepared)) return prepared.response;
    return f.coordinator.route(prepared.request, ["v1", "ssh-hosts", hostID, "heartbeat"]);
  };
  const setup = async () => {
    const f = fixture();
    const c = await credential();
    const response = await f.route([], { ...hostInput, credential: c });
    expect(response.status).toBe(201);
    return { f, c, response };
  };
  it.each([
    ["GET", "/v1/ssh-hosts"],
    ["POST", "/v1/ssh-hosts"],
    ["PATCH", `/v1/ssh-hosts/${hostID}`],
    ["DELETE", `/v1/ssh-hosts/${hostID}`],
    ["POST", `/v1/ssh-hosts/${hostID}/credential/revoke`],
    ["POST", `/v1/ssh-hosts/${hostID}/credential/reissue`],
    ["POST", `/v1/ssh-hosts/${hostID}/credential/recover`],
    ["GET", "/v1/admin/leases"],
    ["POST", "/v1/leases"],
    ["GET", "/v1/capacity"],
    ["POST", "/v1/serve/heartbeat"],
    ["POST", "/v1/serve/leases/cbx_000000000001/relay"],
    ["GET", `/v1/serve/relay/${"1".repeat(32)}/client`],
    ["POST", `/v1/ssh-hosts/${hostID}/heartbeat/extra`],
    ["GET", `/v1/ssh-hosts/${hostID}/heartbeat`],
    ["GET", "/portal"],
    ["POST", "/v1/auth/logout"],
  ])(
    "cannot enter user/admin/borrower route %s %s even with forged admin headers",
    async (method, path) => {
      const prepared = await prepareCoordinatorRequest(request(path, method), {
        ...env,
        CRABBOX_ADMIN_TOKEN: token,
      });
      expect("response" in prepared && prepared.response.status).toBe(403);
    },
  );
  it("requires relay upgrade and strips supplied identity on the two exact routes", async () => {
    const path = `/v1/serve/relay/${"1".repeat(32)}/agent`;
    const r = request(path, "GET");
    expect("response" in (await prepareCoordinatorRequest(r, env))).toBe(true);
    r.headers.set("upgrade", "websocket");
    for (const candidate of [r, request(`/v1/ssh-hosts/${hostID}/heartbeat`)]) {
      const prepared = await prepareCoordinatorRequest(candidate, env);
      expect("request" in prepared).toBe(true);
      if (!("request" in prepared)) continue;
      expect(prepared.authenticated).toBe(false);
      for (const name of ["x-crabbox-admin", "x-crabbox-owner", "x-crabbox-org", "x-crabbox-auth"])
        expect(prepared.request.headers.has(name)).toBe(false);
    }
  });
  it("pins enrollment, returns only public credential status, and safely retries the same hash", async () => {
    const { f, c, response } = await setup();
    const text = await response.text();
    expect(text).not.toContain(token);
    expect(text).not.toContain(c.tokenHash);
    const out = JSON.parse(text);
    expect(out.host.enabled).toBe(false);
    expect(out.identity.owner).toBe("operator@example.com");
    expect(out.identity.org).toBe(orgKeyForLabel("example-org"));
    expect(
      (
        await f.route([], {
          ...hostInput,
          credential: {
            purpose: "agent",
            expiresAt: c.expiresAt,
            tokenHash: c.tokenHash,
            generation: 1,
            version: 1,
          },
        })
      ).status,
    ).toBe(200);
    expect(
      (await f.route([], { ...hostInput, credential: { ...c, tokenHash: "cd".repeat(32) } }))
        .status,
    ).toBe(409);
    expect(
      (await f.route([], { ...hostInput, hostKey: key + " changed", credential: c })).status,
    ).toBe(409);
    const stored = await f.storage.get<SSHHost>(hostPrefix + hostID);
    expect(stored?.frontDoor?.hostKey).toBe(key);
    expect(JSON.stringify(stored)).not.toContain(token);
    expect((await beat(f)).status).toBe(200);
    expect((await f.beat()).status).toBe(401); // Scoped hosts never accept their old operator principal.
  });
  it("rejects wrong secret, host, generation, pinned key and expired credentials", async () => {
    const { f } = await setup();
    for (const bad of [
      token.replace(/ab$/, "ac"),
      token.replace("cbxh_111", "cbxh_222"),
      token.replace("_1_", "_2_"),
    ])
      expect((await beat(f, bad)).status).toBe(401);
    expect((await beat(f, token, key + " changed")).status).toBe(409);
    const stored = (await f.storage.get<SSHHost>(hostPrefix + hostID))!;
    stored.credential!.expiresAt = new Date(Date.now() - 1).toISOString();
    stored.enabled = true;
    stored.lastHeartbeat = Date.now();
    await f.storage.put(hostPrefix + hostID, stored);
    expect((await beat(f)).status).toBe(401);
    expect((await f.create()).status).toBe(409);
    expect((await f.route([hostID], { enabled: true }, undefined, true, "PATCH")).status).toBe(409);
  });
  it("rejects new portable-pool access after credential expiry without changing custody", async () => {
    const { f } = await setup();
    await beat(f);
    await f.route([hostID], { enabled: true }, undefined, true, "PATCH");
    await f.create();
    await beat(
      f,
      token,
      key,
      f.report("ready", { sshHostKey: key, sshUser: "crabbox", workRoot: "/work/crabbox" }),
    );
    const l = (await f.lease())!;
    l.sshHostAllocation!.operation = "idle";
    l.sshHostAllocation!.cleanGeneration = 0;
    await f.storage.put("lease:" + l.id, l);
    const h = (await f.storage.get<SSHHost>(hostPrefix + hostID))!;
    h.credential!.expiresAt = new Date(Date.now() - 1).toISOString();
    await f.storage.put(hostPrefix + hostID, h);
    const access = sshHostCloudProvider(f.runtime, env).poolAccess!();
    const grant = {
      leaseID: l.id,
      generation: 1,
      publicKey: key,
      expiresAt: new Date(Date.now() + 60000).toISOString(),
      binding: { resourceID: hostID, scope: l.region, user: l.sshUser },
    } as PoolAccessGrant;
    await expect(access.install(grant)).rejects.toThrow("host access not eligible");
    expect((await f.lease())?.sshHostAllocation).toEqual(l.sshHostAllocation);
  });

  it("requires admin, retained generation, and clean drained idle custody to reissue", async () => {
    const { f } = await setup();
    const nextToken = token.replace("_1_", "_2_");
    const next = await credential(2, nextToken);
    const input = { expectedGeneration: 1, hostKey: key, credential: next };
    const route = [hostID, "credential", "reissue"];
    expect((await f.route(route, input, undefined, false)).status).toBe(403);
    expect((await f.route(route, input)).status).toBe(409);
    await f.route([hostID], { draining: true }, undefined, true, "PATCH");
    expect((await f.route(route, { ...input, hostKey: key + " changed" })).status).toBe(400);
    expect((await f.route(route, input)).status).toBe(200);
    expect((await f.route(route, input)).status).toBe(200);
    expect((await beat(f)).status).toBe(401);
    expect((await beat(f, nextToken)).status).toBe(200);
    expect(
      (await f.route(route, { ...input, credential: { ...next, tokenHash: "cd".repeat(32) } }))
        .status,
    ).toBe(409);
  });
  it("keeps scoped mode fail closed if durable credential data is lost", async () => {
    const { f } = await setup();
    await beat(f);
    const h = (await f.storage.get<SSHHost>(hostPrefix + hostID))!;
    expect(h.authMode).toBe("host");
    delete h.credential;
    h.enabled = true;
    await f.storage.put(hostPrefix + hostID, h);
    expect((await f.beat()).status).toBe(401);
    expect((await beat(f)).status).toBe(401);
    expect((await f.create()).status).toBe(409);
  });

  it.each(["revoke", "expire"])(
    "recovers only exact stopped custody after %s and settles only a clean receipt",
    async (reason) => {
      const { f } = await setup();
      await beat(f);
      await f.route([hostID], { enabled: true }, undefined, true, "PATCH");
      await f.create();
      await beat(
        f,
        token,
        key,
        f.report("ready", { sshHostKey: key, sshUser: "crabbox", workRoot: "/work/crabbox" }),
      );
      const original = (await f.lease())!;
      if (reason === "revoke") await f.route([hostID, "credential", "revoke"]);
      else {
        const h = (await f.storage.get<SSHHost>(hostPrefix + hostID))!;
        h.credential!.expiresAt = new Date(Date.now() - 1).toISOString();
        await f.storage.put(hostPrefix + hostID, h);
      }
      expect((await beat(f)).status).toBe(401);
      await f.route([hostID], { enabled: false, draining: true }, undefined, true, "PATCH");
      const nextToken = token.replace("_1_", "_2_");
      const cleanup = { leaseID: original.id, generation: 1, accessGeneration: 0 };
      const next = {
        ...(await credential(2, nextToken)),
        purpose: "cleanup",
        cleanup,
        expiresAt: new Date(Date.now() + 3600000).toISOString(),
      };
      const input = { expectedGeneration: 1, hostKey: key, credential: next };
      const route = [hostID, "credential", "recover"];
      expect((await f.route(route, input)).status).toBe(409); // Active lease cannot be recovered.
      await f.coordinator.release(original);
      const retained = (await f.lease())!;
      expect((await f.route(route, input, undefined, false)).status).toBe(403);
      expect((await f.route(route, { ...input, hostKey: key + " changed" })).status).toBe(400);
      for (const wrong of [
        { ...cleanup, leaseID: "cbx_000000000009" },
        { ...cleanup, generation: 2 },
        { ...cleanup, accessGeneration: 1 },
      ])
        expect(
          (await f.route(route, { ...input, credential: { ...next, cleanup: wrong } })).status,
        ).toBe(409);
      expect((await f.route(route, input)).status).toBe(200);
      expect((await f.route(route, input)).status).toBe(200);
      expect((await f.lease())?.sshHostAllocation).toEqual(retained.sshHostAllocation);
      expect(leaseProviderCleanupConfirmed((await f.lease())!)).toBe(false);
      const heartbeat = await beat(f, nextToken);
      const out = await heartbeat.json();
      expect(out.assignment.operation).toBe("clean");
      expect(out.relays).toEqual([]);
      expect((await f.route([hostID], { enabled: true }, undefined, true, "PATCH")).status).toBe(
        409,
      );
      expect((await f.create("cbx_000000000002")).status).toBe(409);
      expect((await beat(f, nextToken, key, f.report("ready"))).status).toBe(409);
      expect(
        (await beat(f, nextToken, key, { ...f.report("clean"), accessGeneration: 1 })).status,
      ).toBe(409);
      expect((await beat(f, nextToken, key, f.report("failed"))).status).toBe(200);
      expect((await f.storage.get<SSHHost>(hostPrefix + hostID))?.quarantine).toBeTruthy();
      expect(leaseProviderCleanupConfirmed((await f.lease())!)).toBe(false);
      const done = await beat(f, nextToken, key, f.report("clean"));
      expect(done.status).toBe(200);
      expect((await done.json()).assignment).toBeNull();
      expect(leaseProviderCleanupConfirmed((await f.lease())!)).toBe(true);
      expect((await f.storage.get<SSHHost>(hostPrefix + hostID))?.leaseID).toBeUndefined();
      // Retry the lost completion response with the same exact receipt.
      const replay = await beat(f, nextToken, key, f.report("clean"));
      expect(replay.status).toBe(200);
      expect((await replay.json()).assignment).toBeNull();
      expect((await beat(f, nextToken, key, f.report("ready"))).status).toBe(409);
      const completed = (await f.storage.get<SSHHost>(hostPrefix + hostID))!;
      completed.credential!.expiresAt = new Date(Date.now() - 1).toISOString();
      await f.storage.put(hostPrefix + hostID, completed);
      expect((await beat(f, nextToken)).status).toBe(401);
      const ackToken = token.replace("_1_", "_3_");
      expect(
        (
          await f.route(route, {
            expectedGeneration: 2,
            hostKey: key,
            credential: {
              ...(await credential(3, ackToken)),
              purpose: "cleanup",
              cleanup,
              expiresAt: new Date(Date.now() + 3600000).toISOString(),
            },
          })
        ).status,
      ).toBe(200);
      const ack = await beat(f, ackToken, key, f.report("clean"));
      expect(ack.status).toBe(200);
      expect(await ack.json()).toMatchObject({
        assignment: null,
        cleanupComplete: true,
        relays: [],
      });
      const finalToken = token.replace("_1_", "_4_");
      expect(
        (
          await f.route([hostID, "credential", "reissue"], {
            expectedGeneration: 3,
            hostKey: key,
            credential: await credential(4, finalToken),
          })
        ).status,
      ).toBe(200);
      expect((await beat(f, finalToken)).status).toBe(200);
      expect((await beat(f, ackToken)).status).toBe(401);
      expect((await beat(f, nextToken)).status).toBe(401);
    },
  );

  it.each(["revoke", "recover"])(
    "retains Cloudflare relay closure targets after %s returns 503",
    async (action) => {
      const { f } = await setup();
      await beat(f);
      await f.route([hostID], { enabled: true }, undefined, true, "PATCH");
      await f.create();
      const l = (await f.lease())!;
      const id = "3".repeat(32);
      l.serveRelaySessions = [id];
      await f.storage.put("lease:" + l.id, l);
      await f.storage.put("ssh-host-relay:" + id, { sessionID: id });
      let fail = true;
      let socketOpen = true;
      const calls: string[] = [];
      const namespace = {
        idFromName: (name: string) => name,
        get: (name: string) => ({
          fetch: async (r: Request) => {
            calls.push(name + new URL(r.url).pathname);
            if (fail) return new Response(null, { status: 503 });
            socketOpen = false;
            return new Response(null, { status: 200 });
          },
        }),
      } as unknown as DurableObjectNamespace;
      const coordinator = new SSHHostCoordinator(f.runtime, {
        ...env,
        serveRelayBackend: new CloudflareServeRelayBackend(namespace),
      });
      let input: unknown = {};
      if (action === "recover") {
        await f.coordinator.release(l);
        await f.route([hostID], { enabled: false, draining: true }, undefined, true, "PATCH");
        input = {
          expectedGeneration: 1,
          hostKey: key,
          credential: {
            ...(await credential(2, token.replace("_1_", "_2_"))),
            purpose: "cleanup",
            cleanup: { leaseID: l.id, generation: 1, accessGeneration: 0 },
            expiresAt: new Date(Date.now() + 3600000).toISOString(),
          },
        };
      }
      const operation = () =>
        coordinator.route(req(undefined, true, undefined, "POST", input), [
          "v1",
          "ssh-hosts",
          hostID,
          "credential",
          action,
        ]);
      expect((await operation()).status).toBe(503);
      expect(socketOpen).toBe(true);
      expect((await beat(f)).status).toBe(401);
      expect((await f.storage.get<SSHHost>(hostPrefix + hostID))?.enabled).toBe(false);
      expect((await f.lease())?.serveRelaySessions).toEqual([id]);
      expect(await f.storage.get("ssh-host-relay:" + id)).toBeTruthy();
      fail = false;
      expect((await operation()).status).toBe(200);
      expect(socketOpen).toBe(false);
      expect((await f.lease())?.serveRelaySessions).toBeUndefined();
      expect(await f.storage.get("ssh-host-relay:" + id)).toBeUndefined();
      expect(calls).toEqual([id + "/close", id + "/close"]);
    },
  );

  it("fences relay generations/expiry, closes active relays on revoke and retains custody", async () => {
    const { f, c } = await setup();
    expect((await beat(f)).status).toBe(200);
    await f.route([hostID], { enabled: true }, undefined, true, "PATCH");
    await f.create();
    const heartbeat = await beat(
      f,
      token,
      key,
      f.report("ready", { sshHostKey: key, sshUser: "crabbox", workRoot: "/work/crabbox" }),
    );
    expect((await heartbeat.json()).assignment.deadline).toBeLessThanOrEqual(
      Date.parse(c.expiresAt),
    );
    const sessions = new Map<string, string>();
    const deadlines = new Map<string, number>();
    const relayEnv = {
      ...env,
      serveRelayBackend: {
        prepare: async (d: { sessionID: string; deadline: number }, secret: string) => {
          sessions.set(d.sessionID, secret);
          deadlines.set(d.sessionID, d.deadline);
        },
        alive: async (id: string) => sessions.has(id),
        pendingToken: async (id: string) => sessions.get(id),
        close: async (id: string) => {
          sessions.delete(id);
        },
        attach: async () => new Response(null, { status: 501 }),
      },
    } as Env;
    const l = (await f.lease())!;
    const { sessionID } = await (
      await mintHostRelay(f.storage, relayEnv, req("borrower@example.com"), l)
    ).json();
    expect(deadlines.get(sessionID)).toBeLessThanOrEqual(Date.parse(c.expiresAt));
    const relay = request(`/v1/serve/relay/${sessionID}/agent`, "GET");
    relay.headers.set("upgrade", "websocket");
    relay.headers.set("x-crabbox-serve-machine", hostID);
    relay.headers.set("x-crabbox-relay-token", sessions.get(sessionID)!);
    expect(
      (await authorizeHostRelay(f.storage, relayEnv, relay, sessionID, "client"))?.status,
    ).toBe(403);
    const wrongMachine = new Request(relay);
    wrongMachine.headers.set("x-crabbox-serve-machine", "wrong");
    expect(
      (await authorizeHostRelay(f.storage, relayEnv, wrongMachine, sessionID, "agent"))?.status,
    ).toBe(403);
    const h = (await f.storage.get<SSHHost>(hostPrefix + hostID))!;
    h.credential!.generation++;
    await f.storage.put(hostPrefix + hostID, h);
    expect((await authorizeHostRelay(f.storage, relayEnv, relay, sessionID, "agent"))?.status).toBe(
      403,
    );
    h.credential!.generation--;
    await f.storage.put(hostPrefix + hostID, h);
    expect((await authorizeHostRelay(f.storage, relayEnv, relay, sessionID, "agent"))?.status).toBe(
      200,
    );
    expect((await authorizeHostRelay(f.storage, relayEnv, relay, sessionID, "agent"))?.status).toBe(
      403,
    );
    expect(
      (await new ServeRelayControl(relayEnv).authorize(f.storage, relay, "2".repeat(32), "agent"))
        .status,
    ).toBe(403);
    h.quarantine = "cleanup retained";
    await f.storage.put(hostPrefix + hostID, h);
    const coordinator = new SSHHostCoordinator(f.runtime, relayEnv);
    expect(
      (
        await coordinator.route(req(undefined, false), [
          "v1",
          "ssh-hosts",
          hostID,
          "credential",
          "revoke",
        ])
      ).status,
    ).toBe(403);
    expect(
      (
        await coordinator.route(req(undefined, true), [
          "v1",
          "ssh-hosts",
          hostID,
          "credential",
          "revoke",
        ])
      ).status,
    ).toBe(200);
    expect(sessions.size).toBe(0);
    expect((await beat(f)).status).toBe(401);
    const revoked = (await f.storage.get<SSHHost>(hostPrefix + hostID))!;
    expect(revoked).toMatchObject({
      enabled: false,
      lastHeartbeat: 0,
      leaseID: l.id,
      quarantine: "cleanup retained",
    });
    expect(revoked.credential?.revokedAt).toBeTruthy();
    expect((await f.lease())?.sshHostAllocation).toEqual(l.sshHostAllocation);
    expect(leaseProviderCleanupConfirmed((await f.lease())!)).toBe(false);
    expect((await f.beat()).status).toBe(401);
  });
});
