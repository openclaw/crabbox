import { afterEach, describe, expect, it, vi } from "vitest";

import { AsyncMutex, fleetRequestQueue } from "../node/server-support";
import { issueUserToken } from "../src/auth";
import { routeCoordinatorRequest } from "../src/coordinator-entry";
import {
  CloudflareCoordinatorRuntime,
  coordinatorRequestQueue,
  legacyAlarmKey,
  provisioningDuePrefix,
} from "../src/coordinator-runtime";
import { FleetCoordinator, FleetDurableObject } from "../src/fleet";
import { orgKeyForLabel } from "../src/org-identity";
import type { Env, LeaseRecord } from "../src/types";
import {
  activeLeaseLimitForOwner,
  costLimits,
  enforceCostLimitUsage,
  type CostLimitUsage,
} from "../src/usage";
import { ProvisioningTestStorage } from "./provisioning-fixtures";

const nodeMocks = vi.hoisted(() => ({
  storage: undefined as unknown,
  boss: undefined as unknown,
}));
vi.mock("../node/postgres-storage", () => ({
  PostgresCoordinatorStorage: function () {
    return nodeMocks.storage;
  },
}));
vi.mock("pg-boss", () => ({
  PgBoss: function () {
    return nodeMocks.boss;
  },
}));
import { NodeCoordinatorRuntime } from "../node/node-runtime";

const now = new Date("2026-09-02T12:00:00.000Z");
const owner = "github:12345";
const org = orgKeyForLabel("org-a");

type CapacityTransaction = Parameters<Parameters<ProvisioningTestStorage["transaction"]>[0]>[0];
type StorageMutation =
  | { kind: "put"; key: string }
  | { kind: "delete"; key: string }
  | { kind: "setAlarm"; time: number }
  | { kind: "deleteAlarm" };

class CapacityStorage extends ProvisioningTestStorage {
  readonly mutations: StorageMutation[] = [];
  beforeList?: (options: { prefix?: string; startAfter?: string }) => Promise<void>;
  initialize = vi.fn<() => Promise<void>>(async () => {});
  close = vi.fn<() => Promise<void>>(async () => {});
  override list = vi.fn<
    <T>(options?: {
      prefix?: string;
      limit?: number;
      startAfter?: string;
      noCache?: boolean;
    }) => Promise<Map<string, T>>
  >(
    async <T>(
      options: { prefix?: string; limit?: number; startAfter?: string; noCache?: boolean } = {},
    ): Promise<Map<string, T>> => {
      await this.beforeList?.(options);
      return super.list<T>(options);
    },
  );
  override transaction<T>(callback: (storage: CapacityTransaction) => Promise<T>): Promise<T> {
    return super.transaction(async (transaction) => {
      // Observe transaction-local effects too; the shared fixture still owns commit/rollback.
      const { put, delete: remove, setAlarm, deleteAlarm } = transaction;
      transaction.put = async (key, value) => {
        this.mutations.push({ kind: "put", key });
        await put.call(transaction, key, value);
      };
      transaction.delete = async (key) => {
        this.mutations.push({ kind: "delete", key });
        await remove.call(transaction, key);
      };
      transaction.setAlarm = async (time) => {
        this.mutations.push({ kind: "setAlarm", time });
        await setAlarm.call(transaction, time);
      };
      transaction.deleteAlarm = async () => {
        this.mutations.push({ kind: "deleteAlarm" });
        await deleteAlarm.call(transaction);
      };
      return callback(transaction);
    });
  }
}

function lease(id: string, overrides: Partial<LeaseRecord> = {}): LeaseRecord {
  return {
    id,
    owner,
    org,
    provider: "aws",
    cloudID: "synthetic-resource",
    region: "eu-west-1",
    profile: "test",
    class: "standard",
    serverType: "test",
    serverID: 0,
    serverName: "synthetic",
    providerKey: "synthetic",
    host: "192.0.2.1",
    sshUser: "crabbox",
    sshPort: "2222",
    sshFallbackPorts: [],
    workRoot: "/work",
    keep: false,
    ttlSeconds: 3600,
    estimatedHourlyUSD: 1,
    maxEstimatedUSD: 1,
    state: "active",
    createdAt: "2026-09-01T00:00:00Z",
    updatedAt: now.toISOString(),
    expiresAt: "2026-09-03T00:00:00Z",
    ...overrides,
  };
}

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => {
  await Promise.all(cleanups.splice(0).map((cleanup) => cleanup()));
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

async function fixture(
  kind: "cloudflare" | "node",
  envOverrides: Partial<Env> = {},
  beforeStartup?: (storage: CapacityStorage) => void,
) {
  vi.useFakeTimers({ toFake: ["Date", "setInterval", "clearInterval"] });
  vi.setSystemTime(now);
  const storage = new CapacityStorage();
  beforeStartup?.(storage);
  const env = {
    CRABBOX_SHARED_TOKEN: "synthetic-shared",
    CRABBOX_SHARED_OWNER: owner,
    CRABBOX_DEFAULT_ORG: "org-a",
    CRABBOX_ADMIN_TOKEN: "synthetic-admin",
    CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER: "10",
    ...envOverrides,
  } as Env;
  let fleet: FleetCoordinator;
  let runtime: CloudflareCoordinatorRuntime | NodeCoordinatorRuntime;
  let dispatch: (request: Request) => Promise<Response>;
  const initializers: Promise<unknown>[] = [];
  const maintenance: Promise<unknown>[] = [];
  const jobs = {
    on: vi.fn<(...args: unknown[]) => void>(),
    start: vi.fn<() => Promise<void>>(async () => {}),
    stop: vi.fn<() => Promise<void>>(async () => {}),
    createQueue: vi.fn<(...args: unknown[]) => Promise<void>>(async () => {}),
    work: vi.fn<(...args: unknown[]) => Promise<string>>(async () => "synthetic-worker"),
    schedule: vi.fn<(...args: unknown[]) => Promise<void>>(async () => {}),
    send: vi.fn<(...args: unknown[]) => Promise<string>>(async () => "synthetic-job"),
    deleteQueuedJobs: vi.fn<(...args: unknown[]) => Promise<void>>(async () => {}),
  };
  // Fail closed for provider/network work during startup as well as requests.
  const network = vi.fn<() => never>(() => {
    throw new Error("unexpected network call");
  });
  vi.stubGlobal("fetch", network);
  if (kind === "cloudflare") {
    const durable = new FleetDurableObject(
      {
        storage,
        getWebSockets: () => [],
        blockConcurrencyWhile<T>(callback: () => Promise<T>): Promise<T> {
          const initialized = callback();
          initializers.push(initialized);
          return initialized;
        },
        waitUntil(operation: Promise<unknown>): void {
          maintenance.push(operation);
        },
      } as unknown as DurableObjectState,
      env,
    );
    fleet = durable;
    runtime = (durable as unknown as { runtime: CloudflareCoordinatorRuntime }).runtime;
    const initialized = Promise.all(initializers);
    dispatch = async (request) => {
      await initialized;
      return durable.fetch(request);
    };
    await initialized;
    cleanups.push(async () => {
      await Promise.all(maintenance);
    });
  } else {
    nodeMocks.storage = storage;
    nodeMocks.boss = jobs;
    const node = new NodeCoordinatorRuntime("postgresql://synthetic.invalid/capacity");
    const mutex = new AsyncMutex();
    node.setOperationRunner((callback) => mutex.run(callback));
    runtime = node;
    fleet = new FleetCoordinator(node, env);
    dispatch = (request) =>
      fleetRequestQueue(request) === "direct"
        ? fleet.fetch(request)
        : mutex.run(() => fleet.fetch(request));
    cleanups.push(() => node.stop());
    await node.start(() => fleet.alarm());
  }
  expect(initializers).toHaveLength(kind === "cloudflare" ? 1 : 0);
  expect(network).not.toHaveBeenCalled();
  const effects = () => ({
    mutations: storage.mutations.length,
    network: network.mock.calls.length,
    maintenance: maintenance.length,
    jobs: Object.fromEntries(
      Object.entries(jobs).map(([key, spy]) => [key, spy.mock.calls.length]),
    ),
  });
  // Startup recovery is complete before observation; never clear evidence of its work.
  const startup = { effects: effects(), lists: storage.list.mock.calls.length };
  const fetch = async (
    query = "",
    headers: Record<string, string> = { authorization: "Bearer synthetic-shared" },
  ) => {
    const before = effects();
    const response = await routeCoordinatorRequest(
      new Request(`https://coordinator.test/v1/capacity${query}`, { headers }),
      env,
      dispatch,
      { githubMembership: async () => {} },
    );
    expect(effects()).toEqual(before);
    return response;
  };
  const seed = (record: LeaseRecord, prefix = "lease:") =>
    storage.values.set(prefix + record.id, record);
  const readonly = () => {
    expect(effects()).toEqual(startup.effects);
  };
  return { storage, env, fleet, runtime, fetch, seed, readonly, startup, jobs };
}

type Admission = {
  leaseAdmissionState(
    candidate: Pick<LeaseRecord, "owner" | "org">,
    now: Date,
  ): Promise<{ costUsage: CostLimitUsage }>;
};
function admission(fleet: FleetCoordinator) {
  return fleet as unknown as Admission;
}

for (const kind of ["cloudflare", "node"] as const) {
  describe(`${kind} owner capacity`, () => {
    it("keeps the diagnostic read-only after cold-start recovery with retained state", async () => {
      const overdue = lease("cold-overdue", { expiresAt: "2026-09-01T00:00:00Z" });
      const stale = lease("cold-stale", { expiresAt: now.toISOString() });
      const dueAt = now.getTime() - 1;
      const legacyAt = now.getTime() + 60_000;
      const dueKey = `${provisioningDuePrefix}${String(dueAt).padStart(16, "0")}:missing-operation`;
      const records = new Map([
        [`lease:${overdue.id}`, overdue],
        [`provider-access:${stale.id}`, stale],
      ]);
      const f = await fixture(kind, {}, (storage) => {
        for (const [key, record] of records) storage.values.set(key, structuredClone(record));
        storage.values.set(
          kind === "cloudflare" ? "test:native-alarm" : "node-runtime:alarm-time",
          legacyAt,
        );
        storage.values.set(dueKey, { operationID: "missing-operation", at: dueAt });
      });

      expect(f.startup.effects.mutations).toBeGreaterThan(0);
      expect(await f.storage.get(legacyAlarmKey)).toBe(legacyAt);
      // Cloudflare only rearms at construction; Node runs its controller scan before serving.
      expect(await f.runtime.getAlarm()).toBe(kind === "cloudflare" ? dueAt : legacyAt);
      expect(f.storage.values.has(dueKey)).toBe(kind === "cloudflare");
      expect(f.storage.initialize).toHaveBeenCalledTimes(kind === "node" ? 1 : 0);
      const nodeStartupJobs = [
        ["coordinator-alarm", null, expect.objectContaining({ startAfter: new Date(legacyAt) })],
        ["coordinator-reconcile", null, { singletonKey: "startup", singletonSeconds: 60 }],
      ];
      expect(f.jobs.send.mock.calls).toEqual(kind === "node" ? nodeStartupJobs : []);
      for (const [key, record] of records) expect(f.storage.values.get(key)).toEqual(record);
      const before = structuredClone(f.storage.values);
      expect(await (await f.fetch()).json()).toEqual({
        owner,
        activeLeases: 1,
        effectiveLimit: 10,
        observedAt: now.toISOString(),
      });
      expect(f.storage.values).toEqual(before);
      f.readonly();
    });

    it("counts self-owner admission entries across months and orgs without leaking leases", async () => {
      const f = await fixture(kind);
      f.seed(lease("current"));
      f.seed(
        lease("previous", {
          createdAt: "2026-08-01T00:00:00Z",
          org: orgKeyForLabel("org-b"),
          expiresAt: "2026-08-02T00:00:00Z",
        }),
      );
      f.seed(
        lease("provisioning", {
          state: "provisioning",
          expiresAt: "2026-09-01T00:00:00Z",
          provider: "hetzner",
        }),
      );
      for (const state of ["released", "expired", "failed"] as const)
        f.seed(lease(state, { state }));
      f.seed(lease("registered", { lifecycle: "registered" }));
      f.seed(lease("other", { owner: "github:99999" }));
      f.seed(lease("shared", { owner: "github:99999", share: { users: { [owner]: "use" } } }));
      const response = await f.fetch();
      expect(response.status).toBe(200);
      expect(await response.json()).toEqual({
        owner,
        activeLeases: 3,
        effectiveLimit: 10,
        observedAt: now.toISOString(),
      });
      f.readonly();
    });

    it.each([9, 10, 11])(
      "reports %i existing entries without the admission candidate",
      async (count) => {
        const f = await fixture(kind);
        for (let i = 0; i < count; i++) f.seed(lease(`count-${i}`));
        const response = await f.fetch();
        expect(response.status).toBe(200);
        expect(await response.json()).toMatchObject({ activeLeases: count, effectiveLimit: 10 });
        f.readonly();
        const { costUsage } = await f.runtime.runExclusive(() =>
          admission(f.fleet).leaseAdmissionState({ owner, org }, now),
        );
        expect(enforceCostLimitUsage(costUsage, lease("candidate"), costLimits(f.env))).toBe(
          count < 10 ? "" : `active lease limit for owner exceeded: ${count + 1}/10`,
        );
      },
    );

    it("uses live reservations once with precedence and ignores stale entries without deleting", async () => {
      const f = await fixture(kind);
      f.seed(lease("overlap", { state: "released", owner: "other" }));
      f.seed(lease("overlap"), "provider-access:");
      f.seed(lease("reservation-only"), "provider-access:");
      f.seed(lease("other-overlay"));
      f.seed(lease("other-overlay", { owner: "other" }), "provider-access:");
      for (const [id, overrides] of [
        ["expired", { expiresAt: "2026-09-02T11:59:59Z" }],
        ["equal", { expiresAt: now.toISOString() }],
        ["terminal", { state: "released" }],
      ] as const) {
        f.seed(lease(id, overrides), "provider-access:");
        f.seed(lease(id));
      }
      f.seed(lease("stale-only", { expiresAt: now.toISOString() }), "provider-access:");
      const before = structuredClone(f.storage.values);
      expect(await (await f.fetch()).json()).toMatchObject({ activeLeases: 5 });
      expect(f.storage.values).toEqual(before);
      f.readonly();
      const { costUsage } = await f.runtime.runExclusive(() =>
        admission(f.fleet).leaseAdmissionState({ owner, org }, now),
      );
      expect(costUsage.ownerActiveLeases).toBe(5);
      expect(
        f.storage.mutations
          .slice(f.startup.effects.mutations)
          .filter((mutation) => mutation.kind === "delete")
          .map((mutation) => mutation.key)
          .toSorted(),
      ).toEqual([
        "provider-access:equal",
        "provider-access:expired",
        "provider-access:stale-only",
        "provider-access:terminal",
      ]);
    });

    it("scans every bounded page of leases and reservations and propagates scan failures", async () => {
      const f = await fixture(kind);
      for (let i = 0; i < 260; i++) {
        f.seed(lease(`canonical-${i.toString().padStart(3, "0")}`));
        f.seed(lease(`reservation-${i.toString().padStart(3, "0")}`), "provider-access:");
      }
      expect(await (await f.fetch()).json()).toMatchObject({ activeLeases: 520 });
      for (const prefix of ["lease:", "provider-access:"]) {
        const calls = f.storage.list.mock.calls.filter(([options]) => options?.prefix === prefix);
        expect(calls).toHaveLength(3);
        for (const [options] of calls) expect(options).toMatchObject({ limit: 128, noCache: true });
        expect(calls[1]?.[0]?.startAfter).toBeDefined();
      }
      f.storage.beforeList = async (options) => {
        if (options.startAfter) throw new Error("synthetic scan failure");
      };
      const response = await f.fetch();
      expect(response.status).toBe(500);
      expect(await response.text()).toContain("synthetic scan failure");
      f.readonly();
    });

    it("serializes the snapshot behind admission on the runtime's lifecycle boundary", async () => {
      const f = await fixture(kind);
      const request = new Request("https://coordinator.test/v1/capacity");
      expect(coordinatorRequestQueue(request)).toBe("lifecycle");
      expect(fleetRequestQueue(request)).toBe("lifecycle");
      let release!: () => void;
      let started!: () => void;
      const entered = new Promise<void>((resolve) => {
        started = resolve;
      });
      const gate = new Promise<void>((resolve) => {
        release = resolve;
      });
      const pendingAdmission = f.runtime.runExclusive(async () => {
        await admission(f.fleet).leaseAdmissionState({ owner, org }, now);
        started();
        await gate;
        f.seed(lease("admitted"), "provider-access:");
      });
      await entered;
      const calls = f.storage.list.mock.calls.length;
      let finished = false;
      const pendingSnapshot = f.fetch().then((response) => {
        finished = true;
        return response;
      });
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
      expect(finished).toBe(false);
      expect(f.storage.list.mock.calls).toHaveLength(calls);
      release();
      await pendingAdmission;
      expect(await (await pendingSnapshot).json()).toMatchObject({ activeLeases: 1 });
      f.readonly();
    });

    it("does not run bridge reconciliation for a diagnostic", async () => {
      const f = await fixture(kind);
      const methods = f.fleet as unknown as {
        reconcileAdminGrantVersion(): Promise<void>;
        restoredBridgesReady(): Promise<boolean>;
      };
      const admin = vi.spyOn(methods, "reconcileAdminGrantVersion");
      const restored = vi.spyOn(methods, "restoredBridgesReady");
      expect(
        (await f.fetch("", { authorization: "Bearer synthetic-admin", "x-crabbox-owner": owner }))
          .status,
      ).toBe(200);
      expect(admin).not.toHaveBeenCalled();
      expect(restored).not.toHaveBeenCalled();
      f.readonly();
    });

    it("uses immutable GitHub identity despite spoofed owner/org/admin headers", async () => {
      const f = await fixture(kind, { CRABBOX_SESSION_SECRET: "synthetic-session-secret" });
      const token = await issueUserToken(f.env, {
        owner,
        ownerSource: "github-verified-email",
        org: "org-a",
        login: "alice",
        githubAccessToken: "synthetic-oauth",
      });
      f.seed(lease("self"));
      f.seed(lease("spoofed", { owner: "github:99999" }));
      const response = await f.fetch("", {
        authorization: `Bearer ${token}`,
        "x-crabbox-owner": "github:99999",
        "x-crabbox-org": "org-b",
        "x-crabbox-admin": "true",
        "x-crabbox-auth": "bearer",
      });
      expect(response.status).toBe(200);
      expect(await response.json()).toEqual({
        owner,
        activeLeases: 1,
        effectiveLimit: 10,
        observedAt: now.toISOString(),
      });
      f.readonly();
    });

    it("keeps shared identity and admin requests self-scoped without granting elevated capacity", async () => {
      const f = await fixture(kind, {
        CRABBOX_CAPACITY_ADMIN_OWNERS: "github:99999",
        CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN: "20",
      });
      f.seed(lease("self"));
      f.seed(lease("other", { owner: "github:99999" }));
      const shared = await f.fetch("", {
        authorization: "Bearer synthetic-shared",
        "x-crabbox-owner": "github:99999",
        "x-crabbox-org": "org-b",
      });
      expect(await shared.json()).toMatchObject({ owner, activeLeases: 1, effectiveLimit: 10 });
      const admin = await f.fetch("", {
        authorization: "Bearer synthetic-admin",
        "x-crabbox-owner": owner,
      });
      expect(await admin.json()).toEqual({
        owner,
        activeLeases: 1,
        effectiveLimit: 10,
        observedAt: now.toISOString(),
      });
      f.readonly();
    });

    it("preserves the unknown owner bucket", async () => {
      const f = await fixture(kind, { CRABBOX_SHARED_OWNER: undefined });
      f.seed(lease("unknown", { owner: "unknown" }));
      f.seed(lease("other"));
      expect(await (await f.fetch()).json()).toMatchObject({ owner: "unknown", activeLeases: 1 });
      f.readonly();
    });

    it("rejects all selectors and denies absent or invalid authentication", async () => {
      const f = await fixture(kind);
      const responses = await Promise.all(
        [
          "?owner=other",
          "?user=other",
          "?org=other",
          "?month=2026-08",
          "?scope=all",
          "?json=1",
          "?unknown=",
        ].flatMap((query) => [
          f.fetch(query),
          f.fetch(query, { authorization: "Bearer synthetic-admin" }),
        ]),
      );
      for (const response of responses) expect(response.status).toBe(400);
      expect((await f.fetch("", {})).status).toBe(401);
      expect((await f.fetch("", { authorization: "Bearer invalid" })).status).toBe(401);
      expect(f.storage.list).toHaveBeenCalledTimes(f.startup.lists);
      f.readonly();
    });
  });
}

describe("owner capacity limit policy", () => {
  it.each([
    [10, 20, true, 20],
    [10, 5, true, 10],
    [10, 0, true, 10],
    [10, 20, false, 10],
    [0, 20, true, 20],
    [0, 20, false, 0],
    [0, 0, true, 0],
  ])(
    "ordinary %i elevated %i membership %s selects %i",
    async (ordinary, elevated, member, expected) => {
      const f = await fixture("cloudflare", {
        CRABBOX_SHARED_OWNER: "Alice@Example.com",
        CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER: String(ordinary),
        CRABBOX_CAPACITY_ADMIN_OWNERS: member ? "ALICE@example.COM" : "bob@example.com",
        CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN: String(elevated),
      });
      f.seed(lease("exact", { owner: "Alice@Example.com" }));
      f.seed(lease("different-case", { owner: "alice@example.com" }));
      expect(activeLeaseLimitForOwner(costLimits(f.env), "Alice@Example.com")).toBe(expected);
      expect(await (await f.fetch()).json()).toMatchObject({
        owner: "Alice@Example.com",
        activeLeases: 1,
        effectiveLimit: expected,
      });
      f.readonly();
    },
  );
});
