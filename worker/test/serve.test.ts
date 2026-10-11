import { afterEach, describe, expect, it, vi } from "vitest";

import { routeCoordinatorRequest } from "../src/coordinator-entry";
import type { CoordinatorRuntime, ProvisioningRuntime } from "../src/coordinator-runtime";
import { FleetCoordinator, FleetDurableObject } from "../src/fleet";
import { completeLeaseProviderCleanup } from "../src/lease-cleanup";
import { ServeCoordinator, serveSize, serveBodyLimit } from "../src/serve";
import type { Env, LeaseRecord } from "../src/types";
import { ProvisioningTestStorage } from "./provisioning-fixtures";

const mid = "11111111-1111-4111-8111-111111111111";
const second = "22222222-2222-4222-8222-222222222222";
const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";
const env = {
  CRABBOX_SHARED_TOKEN: "synthetic-local-test-token",
  CRABBOX_DEFAULT_ORG: "example-org",
  CRABBOX_SHARED_OWNER: "alice@example.com",
  CRABBOX_SERVE_ALLOWED_OWNERS: "*",
} as Env;
function req(path: string, data?: unknown, owner = "alice@example.com") {
  return new Request(`http://localhost${path}`, {
    method: data === undefined ? "GET" : "POST",
    headers: {
      "content-type": "application/json",
      "x-crabbox-owner": owner,
      "x-crabbox-org": "example-org",
      authorization: "Bearer synthetic-local-test-token",
    },
    ...(data === undefined ? {} : { body: JSON.stringify(data) }),
  });
}
function machine(id = mid) {
  return {
    machineID: id,
    name: id === mid ? "builder" : "other",
    os: "linux",
    arch: "amd64",
    cpus: 16,
    memoryMB: 32768,
    maxLeases: 4,
    address: "100.64.1.2",
    runtime: "docker",
    runtimeVersion: "1.0",
    agentVersion: "dev",
    image: "example-image@sha256:example",
    labels: {},
    load: 0,
    freeMemoryMB: 32768,
    draining: false,
    leases: [] as { id: string; state: string }[],
  };
}
function fixture(durable: boolean, settings = env) {
  const storage = new ProvisioningTestStorage();
  const runtime = {
    storage,
    runExclusive: async <T>(f: () => Promise<T>) => f(),
    getWebSockets: () => [],
    getAlarm: async () => undefined,
    scheduleAlarm: async () => {},
    clearAlarm: async () => {},
  } as unknown as CoordinatorRuntime;
  if (durable)
    (runtime as { provisioning: ProvisioningRuntime }).provisioning = {
      commitAndWake: (f) => storage.transaction(f),
      registerProvisioningTick: () => {},
      ownMaintenance: () => {},
    };
  const serve = new ServeCoordinator(runtime, settings);
  const route = async (path: string, data?: unknown, owner?: string) =>
    serve.route(req(path, data, owner), path.split("/").filter(Boolean));
  const create = async (id: string, owner = "alice@example.com", extra = {}) =>
    serve.create(req("/v1/leases", {}, owner), {
      provider: "serve",
      leaseID: id,
      sshPublicKey: key,
      class: "standard",
      ...extra,
    });
  return { storage, runtime, serve, route, create };
}
afterEach(() => vi.useRealTimers());
describe.each([false, true])("serve shared lifecycle durable=%s", (durable) => {
  it("places only on owner machines; pins and reports available capacity", async () => {
    const f = fixture(durable);
    expect((await f.route("/v1/serve/heartbeat", machine(), "bob@example.com")).status).toBe(200);
    const none = await f.create("cbx_000000000001");
    expect(none.status).toBe(409);
    expect(await none.json()).toMatchObject({ error: "serve_capacity_unavailable", details: [] });
    await f.route("/v1/serve/heartbeat", machine(second));
    expect(
      (await f.create("cbx_000000000001", "alice@example.com", { serveMachine: "builder" })).status,
    ).toBe(409);
    expect(
      (await f.create("cbx_000000000001", "alice@example.com", { serveMachine: "other" })).status,
    ).toBe(202);
    expect((await f.storage.get<LeaseRecord>("lease:cbx_000000000001"))?.serve?.machineID).toBe(
      second,
    );
    const list = (await (await f.route("/v1/serve/machines")).json()) as {
      machines: { freeCPUs: number }[];
    };
    expect(list.machines).toHaveLength(1);
    expect(list.machines[0]?.freeCPUs).toBe(8);
    expect((await f.route("/v1/serve/heartbeat", machine(second), "bob@example.com")).status).toBe(
      403,
    );
  });
  it("reserves concurrently; frees only after stop acknowledgement; pins host key", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-10-10T12:00:00Z"));
    const f = fixture(durable);
    await f.route("/v1/serve/heartbeat", machine());
    const outcomes = await Promise.all([1, 2, 3].map((i) => f.create(`cbx_00000000000${i}`)));
    expect(outcomes.map((r) => r.status)).toEqual([202, 202, 409]);
    vi.advanceTimersByTime(2000);
    const hb = (await (await f.route("/v1/serve/heartbeat", machine())).json()) as {
      assignments: { id: string }[];
    };
    expect(hb.assignments).toHaveLength(2);
    const id = hb.assignments[0]!.id;
    expect(
      (
        await f.route(`/v1/serve/leases/${id}/ready`, {
          machineID: mid,
          host: "100.64.1.2",
          port: 30022,
          sshHostKey: key,
        })
      ).status,
    ).toBe(200);
    expect((await f.storage.get<LeaseRecord>(`lease:${id}`))?.state).toBe("active");
    expect(
      (
        await f.route(`/v1/serve/leases/${id}/ready`, {
          machineID: mid,
          host: "100.64.1.2",
          port: 30023,
          sshHostKey: key,
        })
      ).status,
    ).toBe(409);
    const l = (await f.storage.get<LeaseRecord>(`lease:${id}`))!;
    await f.serve.release(req("/", {}), l);
    expect((await f.create("cbx_000000000004")).status).toBe(409);
    vi.advanceTimersByTime(2000);
    const running = machine();
    running.leases = [{ id, state: "running" }];
    const stop = (await (await f.route("/v1/serve/heartbeat", running)).json()) as {
      stop: string[];
    };
    expect(stop.stop).toContain(id);
    vi.advanceTimersByTime(2000);
    await f.route("/v1/serve/heartbeat", machine());
    expect((await f.storage.get<LeaseRecord>(`lease:${id}`))?.cleanupCompletedAt).toBeTruthy();
    expect((await f.create("cbx_000000000004")).status).toBe(202);
  });
  it("marks offline leases lost; returning agent stops orphans", async () => {
    vi.useFakeTimers();
    const f = fixture(durable);
    await f.route("/v1/serve/heartbeat", machine());
    await f.create("cbx_000000000001");
    vi.advanceTimersByTime(90001);
    await f.serve.maintain();
    expect(await f.storage.get("lease:cbx_000000000001")).toMatchObject({
      state: "failed",
      failureError: "serve_machine_lost",
    });
    const m = machine();
    m.leases = [
      { id: "cbx_000000000001", state: "running" },
      { id: "cbx_000000000009", state: "running" },
    ];
    expect(await (await f.route("/v1/serve/heartbeat", m)).json()).toMatchObject({
      assignments: [],
      stop: ["cbx_000000000001", "cbx_000000000009"],
    });
  });
  it("records agent failure and refuses foreign ready reports", async () => {
    const f = fixture(durable);
    await f.route("/v1/serve/heartbeat", machine());
    await f.create("cbx_000000000001");
    const path = "/v1/serve/leases/cbx_000000000001/failed";
    expect(
      (await f.route(path, { machineID: mid, reason: "runtime bind refused" }, "bob@example.com"))
        .status,
    ).toBe(404);
    expect((await f.route(path, { machineID: mid, reason: "runtime bind refused" })).status).toBe(
      200,
    );
    expect(await f.storage.get("lease:cbx_000000000001")).toMatchObject({
      state: "failed",
      failureError: "runtime bind refused",
    });
  });
  it("bounds bodies and requires authentication through the shared entry", async () => {
    const f = fixture(durable);
    expect(
      (await f.route("/v1/serve/heartbeat", { padding: "x".repeat(serveBodyLimit) })).status,
    ).toBe(413);
    const unauth = new Request("http://localhost/v1/serve/machines");
    const result = await routeCoordinatorRequest(unauth, env, () => {
      throw new Error("must not dispatch");
    });
    expect(result.status).toBe(401);
    const fleet = new FleetCoordinator(f.runtime, env);
    const authenticated = await routeCoordinatorRequest(
      req("/v1/serve/heartbeat", machine()),
      env,
      (r) => fleet.fetch(r),
    );
    expect(authenticated.status).toBe(200);
  });
});
it("maps classes and explicit cpu sizes", () => {
  const m = { cpus: 24, memoryMB: 49152 };
  expect(serveSize(m, "standard")).toEqual({ cpus: 8, memoryMB: 16384 });
  expect(serveSize(m, "fast")).toEqual({ cpus: 16, memoryMB: 32768 });
  expect(serveSize(m, "large")).toEqual({ cpus: 32, memoryMB: 65536 });
  expect(serveSize(m, "beast")).toEqual(m);
  expect(serveSize(m, "beast", "cpu3")).toEqual({ cpus: 3, memoryMB: 6144 });
  expect(() => serveSize(m, "beast", "cpu0")).toThrow("invalid_serve_type");
});

it("uses the normal authenticated client read, heartbeat, release and cancellation paths", async () => {
  const f = fixture(true);
  const fleet = new FleetCoordinator(f.runtime, env);
  const send = (path: string, data?: unknown) =>
    routeCoordinatorRequest(req(path, data), env, (r) => fleet.fetch(r));
  expect((await send("/v1/serve/heartbeat", machine())).status).toBe(200);
  const id = "cbx_00000000000a";
  expect(
    (
      await send("/v1/leases", {
        provider: "serve",
        class: "standard",
        leaseID: id,
        createAttemptID: "cat_" + "a".repeat(32),
        sshPublicKey: key,
      })
    ).status,
  ).toBe(202);
  expect(
    (
      await send(`/v1/serve/leases/${id}/ready`, {
        machineID: mid,
        host: "100.64.1.2",
        port: 30022,
        sshHostKey: key,
      })
    ).status,
  ).toBe(200);
  expect(await (await send(`/v1/leases/${id}`)).json()).toMatchObject({
    lease: { state: "active", host: "100.64.1.2" },
  });
  expect((await send(`/v1/leases/${id}/heartbeat`, { expectedProvider: "serve" })).status).toBe(
    200,
  );
  expect(
    await (
      await send(`/v1/leases/${id}/release`, { expectedProvider: "serve", delete: true })
    ).json(),
  ).toMatchObject({ lease: { state: "released", cleanupStatus: "pending" } });
  const l = await f.storage.get<LeaseRecord>(`lease:${id}`);
  expect(l?.cleanupCompletedAt).toBeUndefined();
});

it("supports the Durable Object runtime adapter", async () => {
  const storage = new ProvisioningTestStorage();
  const state = {
    storage,
    getWebSockets: () => [],
    waitUntil: () => {},
    blockConcurrencyWhile: async <T>(f: () => Promise<T>) => f(),
  } as unknown as DurableObjectState;
  const fleet = new FleetDurableObject(state, env);
  const send = (path: string, data?: unknown) =>
    routeCoordinatorRequest(req(path, data), env, (r) => fleet.fetch(r));
  expect((await send("/v1/serve/heartbeat", machine())).status).toBe(200);
  expect(
    (
      await send("/v1/leases", {
        provider: "serve",
        class: "standard",
        leaseID: "cbx_00000000000b",
        sshPublicKey: key,
      })
    ).status,
  ).toBe(202);
  expect(await (await send("/v1/serve/machines")).json()).toMatchObject({
    machines: [{ freeCPUs: 8 }],
  });
});

it("keeps machine registration quotas independent between owners", async () => {
  const f = fixture(true);
  const registrations = await Promise.all(
    Array.from({ length: 64 }, (_, i) =>
      f.route(
        "/v1/serve/heartbeat",
        machine(`11111111-1111-4111-8111-${i.toString().padStart(12, "0")}`),
      ),
    ),
  );
  expect(registrations.every((r) => r.status === 200)).toBe(true);
  expect((await f.route("/v1/serve/heartbeat", machine(second))).status).toBe(409);
  expect((await f.route("/v1/serve/heartbeat", machine(second), "bob@example.com")).status).toBe(
    200,
  );
  const listing = (await (
    await f.route("/v1/serve/machines", undefined, "bob@example.com")
  ).json()) as { machines: unknown[] };
  expect(listing.machines).toHaveLength(1);
});

describe.each([false, true])("serve owner gate durable=%s", (durable) => {
  it.each([undefined, "", " , , ", "github:12345"])(
    "rejects unlisted callers before state access (%s)",
    async (allowed) => {
      const f = fixture(durable, { ...env, CRABBOX_SERVE_ALLOWED_OWNERS: allowed });
      f.storage.beforeGet = async () => {
        throw new Error("must not read state");
      };
      const list = vi.spyOn(f.storage, "list");
      const requests = [
        f.route("/v1/serve/heartbeat", machine()),
        f.route("/v1/serve/machines"),
        f.route("/v1/serve/leases/cbx_000000000001/ready", {}),
        f.route("/v1/serve/leases/cbx_000000000001/failed", {}),
        f.create("cbx_000000000001"),
      ];
      await Promise.all(
        requests.map(async (request) => {
          const response = await request;
          expect(response.status).toBe(403);
          expect(await response.json()).toEqual({ error: "serve_not_enabled" });
        }),
      );
      expect(list).not.toHaveBeenCalled();
      expect(f.storage.writes).toEqual([]);
    },
  );
  it.each(["alice@example.com", " , alice@example.com, ,", "*"])(
    "allows configured callers (%s)",
    async (allowed) => {
      const f = fixture(durable, { ...env, CRABBOX_SERVE_ALLOWED_OWNERS: allowed });
      expect((await f.route("/v1/serve/heartbeat", machine())).status).toBe(200);
      expect((await f.create("cbx_000000000001")).status).toBe(202);
    },
  );
  it("revokes existing machines and prevents further placements", async () => {
    const settings = { ...env };
    const f = fixture(durable, settings);
    await f.route("/v1/serve/heartbeat", machine());
    settings.CRABBOX_SERVE_ALLOWED_OWNERS = "github:12345";
    expect((await f.route("/v1/serve/heartbeat", machine())).status).toBe(403);
    expect((await f.create("cbx_000000000001")).status).toBe(403);
  });
});

it("skips disabled maintenance without reading storage", async () => {
  const f = fixture(false, { ...env, CRABBOX_SERVE_ALLOWED_OWNERS: " , " });
  const list = vi.spyOn(f.storage, "list");
  expect(await f.serve.maintain()).toBeUndefined();
  expect(list).not.toHaveBeenCalled();
});

it("does not rewrite unchanged lease rows on heartbeat or maintenance", async () => {
  vi.useFakeTimers();
  const f = fixture(false);
  await f.route("/v1/serve/heartbeat", machine());
  await f.create("cbx_000000000001");
  f.storage.writes.length = 0;
  vi.advanceTimersByTime(2000);
  await f.route("/v1/serve/heartbeat", machine());
  await f.serve.maintain();
  expect(f.storage.writes.filter((storageKey) => storageKey.startsWith("lease:"))).toEqual([]);
});

it("prunes both machine keys after seven days only with all leases cleaned up", async () => {
  vi.useFakeTimers();
  const f = fixture(false);
  await f.route("/v1/serve/heartbeat", machine());
  await f.route("/v1/serve/heartbeat", machine(second));
  await f.create("cbx_000000000001", undefined, { serveMachine: mid });
  vi.advanceTimersByTime(7 * 24 * 60 * 60 * 1000 + 1);
  await f.serve.maintain();
  expect(await f.storage.get(`serve-machine:${mid}`)).toBeDefined();
  expect(await f.storage.get(`serve-machine:${second}`)).toBeUndefined();
  expect(
    [...f.storage.values.keys()].filter((storageKey) => storageKey.startsWith("serve-owner:")),
  ).toHaveLength(1);
  const lease = (await f.storage.get<LeaseRecord>("lease:cbx_000000000001"))!;
  completeLeaseProviderCleanup(lease, new Date().toISOString());
  await f.storage.put(`lease:${lease.id}`, lease);
  await f.serve.maintain();
  expect(
    [...f.storage.values.keys()].filter((storageKey) => storageKey.startsWith("serve-")),
  ).toEqual([]);
});

it("continues the shared alarm after throwing serve maintenance", async () => {
  const f = fixture(false);
  const fleet = new FleetCoordinator(f.runtime, env);
  const failure = vi
    .spyOn(ServeCoordinator.prototype, "maintain")
    .mockRejectedValue(new Error("injected"));
  const log = vi.spyOn(console, "error").mockImplementation(() => {});
  const internals = fleet as unknown as {
    runAWSOrphanSweepIfDue: (source: string) => Promise<void>;
    runAzureOrphanSweepIfDue: (source: string) => Promise<void>;
  };
  const aws = vi.spyOn(internals, "runAWSOrphanSweepIfDue").mockResolvedValue();
  const azure = vi.spyOn(internals, "runAzureOrphanSweepIfDue").mockResolvedValue();
  try {
    await fleet.alarm();
    expect(failure).toHaveBeenCalledOnce();
    expect(aws).toHaveBeenCalledWith("alarm");
    expect(azure).toHaveBeenCalledWith("alarm");
    expect(log).toHaveBeenCalledWith(
      "serve maintenance failed; continuing coordinator maintenance",
    );
  } finally {
    failure.mockRestore();
    log.mockRestore();
    aws.mockRestore();
    azure.mockRestore();
  }
});

it("rejects disabled authenticated serve routes before dispatch in the shared runtime entry", async () => {
  const disabledEnv = { ...env, CRABBOX_SERVE_ALLOWED_OWNERS: "github:12345" };
  await Promise.all(
    [
      req("/v1/serve/machines"),
      req("/v1/serve/heartbeat", machine()),
      req("/v1/serve/leases/cbx_000000000001/ready", {}),
      req("/v1/serve/leases/cbx_000000000001/failed", {}),
    ].map(async (request) => {
      const response = await routeCoordinatorRequest(request, disabledEnv, () => {
        throw new Error("must not dispatch");
      });
      expect(response.status).toBe(403);
      expect(await response.json()).toEqual({ error: "serve_not_enabled" });
    }),
  );
  const f = fixture(false, disabledEnv);
  const fleet = new FleetCoordinator(f.runtime, disabledEnv);
  const response = await routeCoordinatorRequest(
    req("/v1/leases", { provider: "serve" }),
    disabledEnv,
    (r) => fleet.fetch(r),
  );
  expect(response.status).toBe(403);
  expect(await response.json()).toEqual({ error: "serve_not_enabled" });
  expect(f.storage.listOptions.filter((entry) => entry?.prefix?.startsWith("serve-"))).toEqual([]);
  expect(
    f.storage.writes.filter((entry) => entry.startsWith("serve-") || entry.startsWith("lease:")),
  ).toEqual([]);
});

it("accepts an immediate draining transition but throttles repeated heartbeats", async () => {
  vi.useFakeTimers();
  const f = fixture(false);
  await f.route("/v1/serve/heartbeat", machine());
  expect((await f.route("/v1/serve/heartbeat", machine())).status).toBe(429);
  await f.create("cbx_000000000001");
  const draining = { ...machine(), draining: true };
  expect((await f.route("/v1/serve/heartbeat", draining)).status).toBe(200);
  expect((await f.create("cbx_000000000002")).status).toBe(409);
  expect((await f.route("/v1/serve/heartbeat", draining)).status).toBe(429);
});
