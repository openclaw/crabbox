import { afterEach, describe, expect, it, vi } from "vitest";

import { fleetRequestQueue } from "../node/server-support";
import { AsyncMutex } from "../src/async-mutex";
import { issueUserToken } from "../src/auth";
import { routeCoordinatorRequest } from "../src/coordinator-entry";
import {
  CloudflareCoordinatorRuntime,
  coordinatorRequestQueue,
  legacyAlarmKey,
  provisioningDuePrefix,
} from "../src/coordinator-runtime";
import { FleetCoordinator, FleetDurableObject } from "../src/fleet";
import { maintainLeaseIndex } from "../src/lease-maintenance-index";
import { leaseMaintenanceIndex, putLeaseRecord } from "../src/lease-record-store";
import { orgKeyForLabel } from "../src/org-identity";
import type { Env, LeaseRecord } from "../src/types";
import {
  activeLeaseLimitsForOwner,
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
const generation = "capacity-test-generation";

type CapacityTransaction = Parameters<Parameters<ProvisioningTestStorage["transaction"]>[0]>[0];
type StorageMutation =
  | { kind: "put"; key: string }
  | { kind: "delete"; key: string }
  | { kind: "setAlarm"; time: number }
  | { kind: "deleteAlarm" };

class CapacityStorage extends ProvisioningTestStorage {
  readonly mutations: StorageMutation[] = [];
  readonly reads: Array<{ noCache?: boolean; allowConcurrency?: boolean } | undefined> = [];
  beforeList?: (options: {
    prefix?: string;
    startAfter?: string;
    allowConcurrency?: boolean;
  }) => Promise<void>;
  initialize = vi.fn<() => Promise<void>>(async () => {});
  close = vi.fn<() => Promise<void>>(async () => {});
  override get<T>(
    key: string,
    options?: { noCache?: boolean; allowConcurrency?: boolean },
  ): Promise<T | undefined> {
    this.reads.push(options);
    return super.get<T>(key);
  }
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
      const list = transaction.list.bind(transaction);
      transaction.list = async <Value>(options?: Parameters<CapacityTransaction["list"]>[0]) => {
        await this.beforeList?.(options ?? {});
        return list<Value>(options);
      };
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
    createdAt: "2026-09-02T11:30:00Z",
    updatedAt: now.toISOString(),
    expiresAt: "2026-09-02T12:30:00Z",
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
      {},
      generation,
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
    fleet = new FleetCoordinator(node, env, {}, {}, generation);
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
        fleet: { activeLeases: 1, limit: null },
        org: { key: "org-a", activeLeases: 1, limit: null },
        admissible: true,
        blockedBy: null,
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
        fleet: { activeLeases: 5, limit: null },
        org: { key: "org-a", activeLeases: 4, limit: null },
        admissible: true,
        blockedBy: null,
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

    it.each([
      { name: "fleet with owner headroom", fleet: 2, owner: 10, org: 10, blockedBy: "fleet" },
      { name: "org with owner headroom", fleet: 10, owner: 10, org: 2, blockedBy: "org" },
      { name: "owner", fleet: 10, owner: 1, org: 10, blockedBy: "owner" },
      { name: "fleet before owner and org", fleet: 3, owner: 1, org: 2, blockedBy: "fleet" },
      { name: "owner before org", fleet: 10, owner: 1, org: 2, blockedBy: "owner" },
      { name: "headroom", fleet: 4, owner: 2, org: 3, blockedBy: null },
      { name: "unlimited", fleet: 0, owner: 0, org: 0, blockedBy: null },
    ])("reports $name using the allocation limit order", async (test) => {
      const f = await fixture(kind, {
        CRABBOX_MAX_ACTIVE_LEASES: String(test.fleet),
        CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER: String(test.owner),
        CRABBOX_MAX_ACTIVE_LEASES_PER_ORG: String(test.org),
      });
      f.seed(lease("self"));
      f.seed(lease("same-org", { owner: "github:99999" }), "provider-access:");
      f.seed(lease("other-org", { owner: "github:88888", org: orgKeyForLabel("org-b") }));
      const response = await f.fetch();
      expect(response.status).toBe(200);
      expect(await response.json()).toEqual({
        owner,
        activeLeases: 1,
        effectiveLimit: test.owner,
        observedAt: now.toISOString(),
        fleet: { activeLeases: 3, limit: test.fleet || null },
        org: { key: "org-a", activeLeases: 2, limit: test.org || null },
        admissible: test.blockedBy === null,
        blockedBy: test.blockedBy,
      });
      f.readonly();
      const { costUsage } = await f.runtime.runExclusive(() =>
        admission(f.fleet).leaseAdmissionState({ owner, org }, now),
      );
      const messages = {
        fleet: `fleet active lease limit exceeded: 4/${test.fleet}`,
        owner: `active lease limit for owner exceeded: 2/${test.owner}`,
        org: `active lease limit for org exceeded: 3/${test.org}`,
      };
      expect(enforceCostLimitUsage(costUsage, lease("candidate"), costLimits(f.env))).toBe(
        test.blockedBy === null ? "" : messages[test.blockedBy as keyof typeof messages],
      );
    });

    it.each([
      {
        name: "eligible deployment admin",
        admin: owner,
        override: owner,
        adminLimit: "50",
        self: 11,
        shared: true,
        blockedBy: null,
      },
      {
        name: "eligible normalized legacy admin",
        admin: "",
        legacy: ` ${owner.toUpperCase()} `,
        override: ` ${owner.toUpperCase()} `,
        adminLimit: "50",
        self: 11,
        shared: true,
        blockedBy: null,
      },
      {
        name: "legacy admin only",
        admin: "",
        legacy: owner,
        override: "",
        adminLimit: "50",
        self: 11,
        shared: false,
        blockedBy: "fleet",
      },
      {
        name: "override only",
        admin: "",
        override: owner,
        adminLimit: "50",
        self: 11,
        shared: false,
        blockedBy: "fleet",
      },
      {
        name: "ordinary owner",
        admin: "github:99999",
        override: "github:99999",
        adminLimit: "50",
        self: 11,
        shared: false,
        blockedBy: "fleet",
      },
      {
        name: "disabled admin limit",
        admin: owner,
        override: owner,
        adminLimit: "0",
        self: 11,
        shared: false,
        blockedBy: "fleet",
      },
      {
        name: "owner allowance exhausted",
        admin: owner,
        override: owner,
        adminLimit: "50",
        self: 50,
        shared: true,
        blockedBy: "owner",
      },
    ])("reports shared override for $name without hiding aggregate usage", async (test) => {
      const f = await fixture(kind, {
        CRABBOX_DEPLOYMENT_CAPACITY_ADMIN_OWNERS: test.admin,
        CRABBOX_CAPACITY_ADMIN_OWNERS: test.legacy,
        CRABBOX_SHARED_CAPACITY_OVERRIDE_OWNERS: test.override,
        CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN: test.adminLimit,
        CRABBOX_MAX_ACTIVE_LEASES: "50",
        CRABBOX_MAX_ACTIVE_LEASES_PER_ORG: "50",
      });
      for (let i = 0; i < 50; i++)
        f.seed(lease(`shared-${i}`, { owner: i < test.self ? owner : "github:99999" }));
      const response = await f.fetch();
      expect(response.status).toBe(200);
      expect(await response.json()).toEqual({
        owner,
        activeLeases: test.self,
        effectiveLimit: test.adminLimit !== "0" && (test.admin === owner || test.legacy) ? 50 : 10,
        observedAt: now.toISOString(),
        fleet: { activeLeases: 50, limit: test.shared ? null : 50 },
        org: { key: "org-a", activeLeases: 50, limit: test.shared ? null : 50 },
        admissible: test.blockedBy === null,
        blockedBy: test.blockedBy,
      });
      f.readonly();
    });

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

    it("reads indexed leases and reservation pages and propagates scan failures", async () => {
      const f = await fixture(kind);
      for (let i = 0; i < 260; i++) {
        f.seed(lease(`canonical-${i.toString().padStart(3, "0")}`));
        f.seed(lease(`reservation-${i.toString().padStart(3, "0")}`), "provider-access:");
      }
      await maintainLeaseIndex(f.storage, leaseMaintenanceIndex, now.getTime(), generation);
      const start = f.storage.listOptions.length;
      expect(await (await f.fetch()).json()).toMatchObject({ activeLeases: 520 });
      for (const prefix of [leaseMaintenanceIndex.prefix, "provider-access:"]) {
        const calls = f.storage.listOptions
          .slice(start)
          .filter((options) => options?.prefix === prefix);
        expect(calls).toHaveLength(3);
        for (const options of calls) expect(options).toMatchObject({ limit: 128, noCache: true });
        expect(calls[1]?.startAfter).toBeDefined();
      }
      f.storage.beforeList = async (options) => {
        if (options.startAfter) throw new Error("synthetic scan failure");
      };
      const response = await f.fetch();
      expect(response.status).toBe(500);
      expect(await response.text()).toContain("synthetic scan failure");
    });

    it("does not queue a capacity snapshot behind the lifecycle mutex", async () => {
      const f = await fixture(kind);
      const request = new Request("https://coordinator.test/v1/capacity");
      expect(coordinatorRequestQueue(request)).toBe("direct");
      expect(fleetRequestQueue(request)).toBe("direct");
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
      try {
        expect(await (await f.fetch()).json()).toMatchObject({ activeLeases: 0 });
      } finally {
        release();
      }
      await pendingAdmission;
      expect(await (await f.fetch()).json()).toMatchObject({ activeLeases: 1 });
      f.readonly();
    });

    it("bounds cold historical reads and never returns a partial capacity count", async () => {
      const f = await fixture(kind);
      for (let i = 0; i < 5000; i++) f.seed(lease(`history-${i}`, { state: "released" }));
      f.seed(lease("z-active"));
      const start = f.storage.listOptions.length;
      const response = await f.fetch();
      expect(response.status).toBe(503);
      expect(response.headers.get("retry-after")).toBe("2");
      expect(await response.json()).toMatchObject({
        error: "capacity_unavailable",
        retryable: true,
      });
      expect(f.storage.listOptions.slice(start)).toEqual([
        { prefix: "lease:", limit: 129, noCache: true, allowConcurrency: true },
      ]);
      f.readonly();
    });

    it("keeps repeated indexed snapshots independent of retained history and tracks live writes", async () => {
      const f = await fixture(kind);
      for (let i = 0; i < 5000; i++) f.seed(lease(`history-${i}`, { state: "released" }));
      f.seed(lease("active"));
      // More than one maintenance pass is required; partial backfill is not admission proof.
      await maintainLeaseIndex(f.storage, leaseMaintenanceIndex, now.getTime(), generation);
      expect((await f.fetch()).status).toBe(503);
      await maintainLeaseIndex(f.storage, leaseMaintenanceIndex, now.getTime(), generation);
      const start = f.storage.listOptions.length;
      const reads: string[] = [];
      f.storage.beforeGet = async (key) => {
        reads.push(key);
      };
      for (let i = 0; i < 3; i++) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- prove successive snapshots, not concurrent requests.
        expect(await (await f.fetch()).json()).toMatchObject({ activeLeases: 1 });
      }
      expect(f.storage.listOptions.slice(start).map((options) => options?.prefix)).toEqual(
        Array.from({ length: 3 }, () => [leaseMaintenanceIndex.prefix, "provider-access:"]).flat(),
      );
      expect(reads.filter((key) => key.startsWith("lease:"))).toEqual(
        Array.from({ length: 3 }, () => "lease:active"),
      );
      await f.storage.transaction((storage) => putLeaseRecord(storage, lease("new")));
      expect(await (await f.fetch()).json()).toMatchObject({ activeLeases: 2 });
      await f.storage.transaction((storage) =>
        putLeaseRecord(storage, lease("active", { state: "released" })),
      );
      expect(await (await f.fetch()).json()).toMatchObject({ activeLeases: 1 });
      // A stale superset entry is harmless because the current lease is reread.
      f.storage.values.set(`${leaseMaintenanceIndex.prefix}history-0`, 1);
      expect(await (await f.fetch()).json()).toMatchObject({ activeLeases: 1 });
    });

    it("refuses an old-generation index until rollback history has been audited", async () => {
      const f = await fixture(kind);
      for (let i = 0; i < 130; i++) f.seed(lease(`history-${i}`, { state: "released" }));
      await maintainLeaseIndex(f.storage, leaseMaintenanceIndex, now.getTime(), "old-generation");
      // Simulate an older deployment that could not maintain this index.
      f.seed(lease("unindexed"));
      expect((await f.fetch()).status).toBe(503);
      await maintainLeaseIndex(f.storage, leaseMaintenanceIndex, now.getTime(), generation);
      expect(await (await f.fetch()).json()).toMatchObject({ activeLeases: 1 });
    });

    it("keeps released running compute in indexed capacity until its stop is confirmed", async () => {
      const f = await fixture(kind);
      const retained = lease("retained", {
        state: "released",
        releaseDeletesServer: false,
        lifetimePolicy: {
          mode: "enforce",
          workload: "batch",
          deadlineAt: "2026-09-03T00:00:00Z",
          retainSeconds: 3600,
          compute: { state: "running", accumulatedMs: 0, runningSince: now.toISOString() },
        },
      });
      f.seed(retained);
      await maintainLeaseIndex(f.storage, leaseMaintenanceIndex, now.getTime(), generation);
      expect(await (await f.fetch()).json()).toMatchObject({ activeLeases: 1 });
      await f.storage.transaction((storage) =>
        putLeaseRecord(storage, {
          ...retained,
          lifetimePolicy: {
            ...retained.lifetimePolicy!,
            compute: { state: "stopped", accumulatedMs: 0 },
          },
        }),
      );
      expect(await (await f.fetch()).json()).toMatchObject({ activeLeases: 0 });
    });

    it("leaves the lifecycle mutex available while a capacity read awaits storage", async () => {
      const f = await fixture(kind);
      const transactions = vi.spyOn(f.storage, "transaction");
      const readStart = f.storage.reads.length;
      let entered!: () => void;
      let release!: () => void;
      const started = new Promise<void>((resolve) => {
        entered = resolve;
      });
      const gate = new Promise<void>((resolve) => {
        release = resolve;
      });
      f.storage.beforeList = async (options) => {
        expect(options.allowConcurrency).toBe(true);
        entered();
        await gate;
      };
      const snapshot = f.fetch();
      await started;
      // A native transaction closes the input gate even when its reads opt out.
      expect(transactions).toHaveBeenCalledTimes(kind === "cloudflare" ? 0 : 1);
      expect(f.storage.reads.slice(readStart)).toEqual(
        kind === "cloudflare" ? [{ noCache: true, allowConcurrency: true }] : [],
      );
      let timer!: ReturnType<typeof setTimeout>;
      try {
        const outcome = await Promise.race([
          f.runtime.runExclusive(async () => "lifecycle"),
          new Promise<string>((resolve) => {
            timer = setTimeout(() => resolve("timeout"), 1000);
          }),
        ]);
        expect(outcome).toBe("lifecycle");
      } finally {
        clearTimeout(timer);
        release();
      }
      expect((await snapshot).status).toBe(200);
      f.readonly();
    });

    it("bounds retained reservation scans without deleting records or publishing an undercount", async () => {
      const f = await fixture(kind);
      for (let i = 0; i < 5000; i++)
        f.seed(lease(`stale-${i}`, { expiresAt: now.toISOString() }), "provider-access:");
      f.seed(lease("z-live"), "provider-access:");
      const start = f.storage.listOptions.length;
      expect((await f.fetch()).status).toBe(503);
      expect(
        f.storage.listOptions
          .slice(start)
          .filter((options) => options?.prefix === "provider-access:"),
      ).toHaveLength(33);
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
        fleet: { activeLeases: 2, limit: null },
        org: { key: "org-a", activeLeases: 2, limit: null },
        admissible: true,
        blockedBy: null,
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
        fleet: { activeLeases: 2, limit: null },
        org: { key: "org-a", activeLeases: 2, limit: null },
        admissible: true,
        blockedBy: null,
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
  it("reports deployment capacity membership through the authenticated Fleet route", async () => {
    const f = await fixture("cloudflare", {
      CRABBOX_DEPLOYMENT_CAPACITY_ADMIN_OWNERS: ` ${owner.toUpperCase()} `,
      CRABBOX_CAPACITY_ADMIN_OWNERS: "github:99999",
      CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN: "50",
      CRABBOX_MAX_ACTIVE_LEASES: "50",
      CRABBOX_MAX_ACTIVE_LEASES_PER_ORG: "50",
    });
    for (let index = 0; index < 10; index++) f.seed(lease(`existing-${index}`));
    const response = await f.fetch();
    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({
      owner,
      activeLeases: 10,
      effectiveLimit: 50,
      observedAt: now.toISOString(),
      fleet: { activeLeases: 10, limit: 50 },
      org: { key: "org-a", activeLeases: 10, limit: 50 },
      admissible: true,
      blockedBy: null,
    });
    f.readonly();
  });

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
      expect(activeLeaseLimitsForOwner(costLimits(f.env), "Alice@Example.com").owner).toBe(
        expected,
      );
      expect(await (await f.fetch()).json()).toMatchObject({
        owner: "Alice@Example.com",
        activeLeases: 1,
        effectiveLimit: expected,
      });
      f.readonly();
    },
  );
});
