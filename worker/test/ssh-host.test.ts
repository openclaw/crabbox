import { describe, expect, it, vi, afterEach } from "vitest";

import type { CoordinatorRuntime } from "../src/coordinator-runtime";
import { leaseProviderCleanupConfirmed } from "../src/lease-cleanup";
import { publicLeaseRecord } from "../src/org-records";
import { SSHHostCoordinator, hostPrefix, type SSHHost } from "../src/ssh-host";
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
  it("allocates exclusively across competing borrowers and retries", async () => {
    const f = fixture();
    await f.setup();
    const results = await Promise.all(
      Array.from({ length: 12 }, (_, i) =>
        f.create(`cbx_${(i + 1).toString(16).padStart(12, "0")}`, `borrower-${i}@example.com`),
      ),
    );
    expect(results.filter((r) => r.status === 202)).toHaveLength(1);
    expect(results.filter((r) => r.status === 409)).toHaveLength(11);
    expect((await f.create("cbx_000000000001", "borrower-0@example.com")).status).toBe(202);
    expect((await f.create("cbx_000000000001", "other@example.com")).status).toBe(409);
  });
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
