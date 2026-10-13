// oxlint-disable eslint/no-await-in-loop -- Lifecycle and cursor tests depend on each prior committed operation.
import { afterEach, describe, expect, it, vi } from "vitest";

import { routeCoordinatorRequest } from "../src/coordinator-entry";
import type { CoordinatorRuntime, ProvisioningRuntime } from "../src/coordinator-runtime";
import { FleetCoordinator, FleetDurableObject } from "../src/fleet";
import { completeLeaseProviderCleanup } from "../src/lease-cleanup";
import { ServeCoordinator, serveSize, serveBodyLimit, type ServeMachine } from "../src/serve";
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
    frontDoor: { port: 2222, hostKey: key },
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
async function seedMaintenance(f: ReturnType<typeof fixture>, count: number, leases = 1) {
  await f.route("/v1/serve/heartbeat", machine());
  await f.create("cbx_000000000001");
  const template = (await f.storage.get<LeaseRecord>("lease:cbx_000000000001"))!;
  f.storage.values.clear();
  const ids: string[] = [];
  for (let i = 1; i <= count; i++) {
    const machineID = `00000000-0000-4000-8000-${i.toString(16).padStart(12, "0")}`;
    const m: ServeMachine = {
      ...machine(machineID),
      owner: `owner-${i}@example.com`,
      org: "example-org",
      maxLeases: leases,
      lastHeartbeat: Date.now() - 90_001,
      leases: [],
    };
    for (let j = 0; j < leases; j++) {
      const id = `cbx_${(i * 128 + j).toString(16).padStart(12, "0")}`;
      m.leases.push(id);
      f.storage.values.set(`lease:${id}`, {
        ...template,
        id,
        owner: m.owner,
        serve: { ...template.serve!, machineID },
      });
    }
    ids.push(machineID);
    f.storage.values.set(`serve-machine:${machineID}`, m);
  }
  f.storage.writes.length = 0;
  f.storage.listOptions.length = 0;
  return ids;
}
afterEach(() => vi.useRealTimers());
describe.each([false, true])("serve shared lifecycle durable=%s", (durable) => {
  it("validates the front door and stores its fingerprint", async () => {
    const f = fixture(durable);
    for (const frontDoor of [
      undefined,
      { port: 0, hostKey: key },
      { port: 65536, hostKey: key },
      { port: 2222, hostKey: "ssh-rsa AAAA" },
      { port: 2222, hostKey: "ssh-ed25519 AAAA" },
    ]) {
      expect((await f.route("/v1/serve/heartbeat", { ...machine(), frontDoor })).status).toBe(400);
    }
    expect((await f.route("/v1/serve/heartbeat", machine())).status).toBe(200);
    const saved = await f.storage.get<ServeMachine>(`serve-machine:${mid}`);
    expect(saved?.frontDoor).toMatchObject({
      port: 2222,
      hostKey: key,
      fingerprint: expect.stringMatching(/^SHA256:/),
    });
  });
  it("exposes both pinned SSH hops without a container endpoint", async () => {
    const f = fixture(durable);
    await f.route("/v1/serve/heartbeat", machine());
    const id = "cbx_000000000001";
    await f.create(id);
    expect(
      (
        await f.route(`/v1/serve/leases/${id}/ready`, {
          machineID: mid,
          host: "100.64.1.2",
          port: 30022,
          sshHostKey: key,
        })
      ).status,
    ).toBe(400);
    expect(
      (await f.route(`/v1/serve/leases/${id}/ready`, { machineID: mid, sshHostKey: key })).status,
    ).toBe(200);
    const saved = await f.storage.get<LeaseRecord>(`lease:${id}`);
    expect(saved).toMatchObject({
      host: id,
      sshPort: "22",
      sshHostKey: key,
      sshJump: {
        host: "100.64.1.2",
        port: 2222,
        hostKey: key,
        fingerprint: expect.stringMatching(/^SHA256:/),
      },
    });
  });
  it("accepts legacy heartbeats and adds compatible jump candidates", async () => {
    const f = fixture(durable);
    expect((await f.route("/v1/serve/heartbeat", machine())).status).toBe(200);
    const id = "cbx_000000000001";
    await f.create(id);
    await f.route(`/v1/serve/leases/${id}/ready`, { machineID: mid, sshHostKey: key });
    expect((await f.storage.get<LeaseRecord>(`lease:${id}`))?.sshJump).toMatchObject({
      host: "100.64.1.2",
      port: 2222,
      candidates: [{ host: "100.64.1.2", port: 2222 }],
    });
  });
  it("publishes all candidates and updates active leases on heartbeat", async () => {
    const f = fixture(durable);
    const candidates = [
      { address: "100.64.1.2", kind: "tailnet" },
      { address: "192.168.1.20", kind: "lan" },
      { address: "203.0.113.1", kind: "explicit" },
    ];
    const m = { ...machine(), frontDoor: { ...machine().frontDoor, candidates } };
    expect((await f.route("/v1/serve/heartbeat", m)).status).toBe(200);
    const id = "cbx_000000000001";
    await f.create(id);
    await f.route(`/v1/serve/leases/${id}/ready`, { machineID: mid, sshHostKey: key });
    expect((await f.storage.get<LeaseRecord>(`lease:${id}`))?.sshJump).toMatchObject({
      host: m.address,
      port: 2222,
      candidates: candidates.map((c) => ({ host: c.address, port: 2222 })),
    });
    vi.spyOn(Date, "now").mockReturnValue(Date.now() + 1000);
    m.address = "192.168.2.20";
    m.frontDoor.candidates = [{ address: m.address, kind: "lan" }];
    m.leases = [{ id, state: "running" }];
    expect((await f.route("/v1/serve/heartbeat", m)).status).toBe(200);
    expect((await f.storage.get<LeaseRecord>(`lease:${id}`))?.sshJump).toMatchObject({
      host: m.address,
      candidates: [{ host: m.address, port: 2222 }],
    });
    vi.restoreAllMocks();
  });
  it.each(
    [
      [],
      [{ address: "100.64.1.2", kind: "bad" }],
      [{ address: "192.168.1.2", kind: "tailnet" }],
      [{ address: "100.64.1.2", kind: "lan" }],
      [{ address: "127.0.0.1", kind: "explicit" }],
      [{ address: "0.0.0.0", kind: "explicit" }],
      [{ address: "224.0.0.1", kind: "explicit" }],
      [{ address: "::1", kind: "explicit" }],
      [{ address: "100.064.1.2", kind: "tailnet" }],
      [
        { address: "100.64.1.2", kind: "tailnet" },
        { address: "100.64.1.2", kind: "explicit" },
      ],
      [
        { address: "100.64.1.2", kind: "tailnet" },
        { address: "169.254.1.2", kind: "lan" },
      ],
      [
        { address: "100.64.1.2", kind: "tailnet" },
        { address: "172.32.1.2", kind: "lan" },
      ],
      [
        { address: "100.64.1.2", kind: "tailnet" },
        ...Array.from({ length: 4 }, (_, i) => ({ address: `10.1.1.${i + 1}`, kind: "lan" })),
      ],
    ].map((candidates) => [candidates]),
  )("rejects invalid candidates %j", async (candidates) => {
    const f = fixture(durable);
    const response = await f.route("/v1/serve/heartbeat", {
      ...machine(),
      frontDoor: { ...machine().frontDoor, candidates },
    });
    expect(response.status).toBe(400);
    expect(f.storage.writes).toEqual([]);
  });
  it.each(["0.1.2.3", "240.1.2.3", "169.254.1.2"])(
    "accepts explicitly configured IPv4 %s",
    async (address) => {
      const f = fixture(durable);
      expect(
        (
          await f.route("/v1/serve/heartbeat", {
            ...machine(),
            address,
            frontDoor: { ...machine().frontDoor, candidates: [{ address, kind: "explicit" }] },
          })
        ).status,
      ).toBe(200);
    },
  );
  it("rejects a legacy address inconsistent with the first candidate", async () => {
    const f = fixture(durable);
    expect(
      (
        await f.route("/v1/serve/heartbeat", {
          ...machine(),
          frontDoor: { ...machine().frontDoor, candidates: [{ address: "10.1.1.1", kind: "lan" }] },
        })
      ).status,
    ).toBe(400);
  });
  it("rejects maxLeases 33", async () => {
    const f = fixture(durable);
    const response = await f.route("/v1/serve/heartbeat", { ...machine(), maxLeases: 33 });
    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({ error: "invalid_serve_heartbeat" });
    expect(f.storage.writes).toEqual([]);
  });
  it.each([
    ["ssh-ed25519", 256],
    ["ssh-rsa", 4096],
  ])("bounds %s keys in bytes, including comments", async (kind, limit) => {
    const f = fixture(durable);
    await f.route("/v1/serve/heartbeat", machine());
    const prefix = `${kind} AAAA `;
    const accepted = prefix + "a".repeat(limit - prefix.length);
    expect((await f.create("cbx_000000000001", undefined, { sshPublicKey: accepted })).status).toBe(
      202,
    );
    for (const sshPublicKey of [accepted + "a", prefix + "é".repeat(limit - prefix.length)]) {
      const response = await f.create("cbx_000000000002", undefined, { sshPublicKey });
      expect(response.status).toBe(400);
      expect(await response.json()).toMatchObject({ error: "invalid_ssh_public_key" });
    }
  });
  it("rejects unsupported SSH key types", async () => {
    const f = fixture(durable);
    const response = await f.create("cbx_000000000001", undefined, {
      sshPublicKey: "ecdsa-sha2-nistp256 AAAA",
    });
    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({ error: "invalid_ssh_public_key" });
  });
  it("caps an owner's live leases at 64 across machines and frees capacity after release", async () => {
    const f = fixture(durable);
    for (const id of [mid, second, "33333333-3333-4333-8333-333333333333"])
      await f.route("/v1/serve/heartbeat", {
        ...machine(id),
        maxLeases: 32,
        cpus: 256,
        memoryMB: 524288,
      });
    for (let i = 1; i <= 64; i++)
      expect((await f.create(`cbx_${i.toString(16).padStart(12, "0")}`)).status).toBe(202);
    const response = await f.create("cbx_000000000041");
    expect(response.status).toBe(429);
    expect(await response.json()).toMatchObject({ error: "serve_lease_limit" });
    // Replaying an existing reservation is still idempotent at the cap.
    expect((await f.create("cbx_000000000001")).status).toBe(202);
    const lease = (await f.storage.get<LeaseRecord>("lease:cbx_000000000001"))!;
    await f.serve.release(req("/", {}), lease);
    expect((await f.create("cbx_000000000041")).status).toBe(202);
    await f.route(
      "/v1/serve/heartbeat",
      machine("44444444-4444-4444-8444-444444444444"),
      "bob@example.com",
    );
    expect((await f.create("cbx_000000000042", "bob@example.com")).status).toBe(202);
  });
  it("heartbeat reads only its own reserved leases", async () => {
    vi.useFakeTimers();
    const f = fixture(durable);
    await f.route("/v1/serve/heartbeat", machine());
    await f.route("/v1/serve/heartbeat", machine(second));
    await f.create("cbx_000000000001", undefined, { serveMachine: mid });
    await f.create("cbx_000000000002", undefined, { serveMachine: second });
    const reads: string[] = [];
    f.storage.beforeGet = async (storageKey) => {
      if (storageKey.startsWith("lease:")) reads.push(storageKey);
    };
    vi.advanceTimersByTime(2000);
    expect((await f.route("/v1/serve/heartbeat", machine())).status).toBe(200);
    expect(reads).toEqual(["lease:cbx_000000000001"]);
  });
  it("drops cleaned reservations before adding another lease", async () => {
    const f = fixture(durable);
    await f.route("/v1/serve/heartbeat", { ...machine(), maxLeases: 1 });
    for (let i = 1; i <= 34; i++) {
      const id = `cbx_${i.toString(16).padStart(12, "0")}`;
      expect((await f.create(id)).status).toBe(202);
      expect((await f.storage.get<ServeMachine>(`serve-machine:${mid}`))?.leases).toEqual([id]);
      const lease = (await f.storage.get<LeaseRecord>(`lease:${id}`))!;
      await f.serve.release(req("/", {}), lease);
      const released = (await f.storage.get<LeaseRecord>(`lease:${id}`))!;
      delete released.cleanupStartedAt;
      completeLeaseProviderCleanup(released, new Date().toISOString());
      await f.storage.put(`lease:${id}`, released);
    }
  });
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
    // Concurrent admissions serialize in arbitrary order; only the outcome set is guaranteed.
    expect(outcomes.map((r) => r.status).toSorted()).toEqual([202, 202, 409]);
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
          sshHostKey: key,
        })
      ).status,
    ).toBe(200);
    expect((await f.storage.get<LeaseRecord>(`lease:${id}`))?.state).toBe("active");
    expect(
      (
        await f.route(`/v1/serve/leases/${id}/ready`, {
          machineID: mid,
          sshHostKey:
            "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB",
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
describe.each([false, true])("bounded serve maintenance durable=%s", (durable) => {
  it.each([
    [35, 1, [16, 16, 3]],
    [10, 32, [8, 2]],
    [5, 128, [2, 2, 1]],
  ])(
    "resumes %i machines with %i leases across bounded transactions",
    async (count, leases, batches) => {
      vi.useFakeTimers();
      const f = fixture(durable);
      const ids = await seedMaintenance(f, count, leases);
      const exclusive = vi.spyOn(f.runtime, "runExclusive");
      const transactions = vi.spyOn(f.storage, "transaction");
      let visited = 0;
      for (const batch of batches) {
        const reads: string[] = [];
        f.storage.beforeGet = async (storageKey) => {
          if (storageKey.startsWith("lease:")) reads.push(storageKey);
        };
        // Recreate the coordinator to prove that resumption is persisted, not in memory.
        const wake = await new ServeCoordinator(f.runtime, env).maintain();
        visited += batch;
        expect(reads).toHaveLength(batch * leases);
        expect(reads.length).toBeLessThanOrEqual(256);
        expect(batch).toBeLessThanOrEqual(16);
        expect(wake === undefined || wake <= Date.now() + 15_000).toBe(true);
        const expectedCursor =
          visited < count ? { after: `serve-machine:${ids[visited - 1]}` } : undefined;
        expect(await f.storage.get("serve-maintenance-cursor")).toEqual(expectedCursor);
        expect(visited === count || wake !== undefined).toBe(true);
      }
      expect(exclusive).toHaveBeenCalledTimes(batches.length);
      expect(transactions).toHaveBeenCalledTimes(batches.length);
      expect(f.storage.listOptions).toEqual(
        batches.map((_, i) => ({
          prefix: "serve-machine:",
          limit: 16,
          ...(i
            ? {
                startAfter: `serve-machine:${ids[batches.slice(0, i).reduce((a, b) => a + b, 0) - 1]}`,
              }
            : {}),
        })),
      );
      const records = [...f.storage.values.values()] as LeaseRecord[];
      for (const lease of records.filter((l) => l.provider === "serve"))
        expect(lease.failureError).toBe("serve_machine_lost");
    },
  );
  it("preserves earlier page deadlines and resets the cursor after a full page tail", async () => {
    vi.useFakeTimers();
    const f = fixture(durable);
    const ids = await seedMaintenance(f, 16);
    for (const [storageKey, value] of f.storage.values) {
      if (storageKey.startsWith("serve-machine:"))
        (value as ServeMachine).lastHeartbeat = Date.now();
    }
    expect(await f.serve.maintain()).toBe(Date.now() + 15_000);
    expect(await f.serve.maintain()).toBe(Date.now() + 90_000);
    expect(await f.storage.get("serve-maintenance-cursor")).toBeUndefined();
    // A fresh sweep starts from the beginning again.
    await f.serve.maintain();
    expect(await f.storage.get("serve-maintenance-cursor")).toMatchObject({
      after: `serve-machine:${ids[15]}`,
    });
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
        sshHostKey: key,
      })
    ).status,
  ).toBe(200);
  expect(await (await send(`/v1/leases/${id}`)).json()).toMatchObject({
    lease: { state: "active", host: id },
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
  const read = vi.fn<() => Promise<void>>(async () => {});
  f.storage.beforeGet = read;
  expect(await f.serve.maintain()).toBeUndefined();
  expect(list).not.toHaveBeenCalled();
  expect(read).not.toHaveBeenCalled();
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

it("continues ordinary alarm maintenance after exactly one serve batch and rearms soon", async () => {
  vi.useFakeTimers();
  const f = fixture(false);
  await seedMaintenance(f, 20);
  const fleet = new FleetCoordinator(f.runtime, env);
  const internals = fleet as unknown as {
    expireLeases: (ids: Set<string>) => Promise<void>;
    runAWSOrphanSweepIfDue: (source: string) => Promise<void>;
    runAzureOrphanSweepIfDue: (source: string) => Promise<void>;
  };
  const maintain = vi.spyOn(ServeCoordinator.prototype, "maintain");
  const expire = vi.spyOn(internals, "expireLeases").mockImplementation(async () => {
    expect(maintain).toHaveBeenCalledOnce();
    const rows = [...f.storage.values.values()].filter(
      (v) => (v as LeaseRecord).provider === "serve",
    ) as LeaseRecord[];
    expect(rows.filter((l) => l.state === "failed")).toHaveLength(16);
    expect(rows.filter((l) => l.state === "provisioning")).toHaveLength(4);
  });
  const aws = vi.spyOn(internals, "runAWSOrphanSweepIfDue").mockResolvedValue();
  const azure = vi.spyOn(internals, "runAzureOrphanSweepIfDue").mockResolvedValue();
  const schedule = vi.spyOn(f.runtime, "scheduleAlarm");
  try {
    await fleet.alarm();
    expect(expire).toHaveBeenCalledOnce();
    expect(aws).toHaveBeenCalledWith("alarm");
    expect(azure).toHaveBeenCalledWith("alarm");
    expect(maintain).toHaveBeenCalledOnce();
    expect(schedule).toHaveBeenCalledWith(Date.now() + 15_000);
  } finally {
    maintain.mockRestore();
    expire.mockRestore();
    aws.mockRestore();
    azure.mockRestore();
    schedule.mockRestore();
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
