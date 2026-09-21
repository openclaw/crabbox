import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import type { CoordinatorRuntime, CoordinatorStorageView } from "../src/coordinator-runtime";
import { FleetCoordinator } from "../src/fleet";
import {
  deviceOwnerIndexPrefix,
  deviceTokenKey,
  pairingGrantOwnerIndexPrefix,
  pairingGrantKey,
  type DeviceTokenRecord,
  type PairingGrantRecord,
} from "../src/pairing";
import type { Env, LeaseRecord, ProviderMachine } from "../src/types";

const now = Date.parse("2026-09-20T12:00:00Z");
const iso = (offset = 0) => new Date(now + offset).toISOString();
const owner = "alice@example.com";
const org = "example-org";
const deviceID = "00000000-0000-4000-8000-000000000001";
const grantHash = "a".repeat(64);
const config = {
  enabled: true,
  deleteEnabled: true,
  intervalSeconds: 300,
  graceSeconds: 1,
  regions: ["region-a"],
  macHostReleaseEnabled: false,
};
type Sweep = {
  candidates: Array<{ action: string; ownership: string; error?: string }>;
  errors: unknown[];
  terminated: number;
};
type Internals = {
  scheduleAlarm(): Promise<void>;
  runAWSOrphanSweepIfDue(
    trigger: "alarm" | "admin",
    input: typeof config,
  ): Promise<Sweep | undefined>;
  runAzureOrphanSweepIfDue(
    trigger: "alarm" | "admin",
    input: typeof config,
  ): Promise<Sweep | undefined>;
  activeOwnerDevices(owner: string, org: string): Promise<DeviceTokenRecord[]>;
  activeOwnerPairingGrants(owner: string, org: string, now: number): Promise<PairingGrantRecord[]>;
};

function fixture(provider: "aws" | "azure" = "aws", env: Partial<Env> = {}) {
  const values = new Map<string, unknown>();
  const storage: CoordinatorStorageView = {
    get: vi.fn<CoordinatorStorageView["get"]>(
      async <T>(key: string) => values.get(key) as T | undefined,
    ),
    put: vi.fn<CoordinatorStorageView["put"]>(async (key, value) => {
      values.set(key, value);
    }),
    delete: vi.fn<CoordinatorStorageView["delete"]>(async (key) => values.delete(key)),
    list: vi.fn<CoordinatorStorageView["list"]>(
      async <T>(options: { prefix?: string; limit?: number; startAfter?: string } = {}) =>
        new Map(
          [...values]
            .filter(
              ([key]) =>
                key.startsWith(options.prefix ?? "") &&
                (!options.startAfter || key > options.startAfter),
            )
            .toSorted(([a], [b]) => a.localeCompare(b))
            .slice(0, options.limit)
            .map(([key, value]) => [key, value as T]),
        ),
    ),
  };
  let exclusive = false;
  const runtime = {
    storage: {
      ...storage,
      transaction: vi.fn<
        (callback: (view: CoordinatorStorageView) => Promise<unknown>) => Promise<unknown>
      >(async (callback: (view: CoordinatorStorageView) => Promise<unknown>) => callback(storage)),
    },
    runExclusive: vi.fn<(callback: () => Promise<unknown>) => Promise<unknown>>(
      async (callback: () => Promise<unknown>) => {
        exclusive = true;
        try {
          return await callback();
        } finally {
          exclusive = false;
        }
      },
    ),
    getWebSockets: () => [],
    scheduleAlarm: vi.fn<(time: number) => Promise<void>>(async () => {}),
    clearAlarm: vi.fn<() => Promise<void>>(async () => {}),
  };
  const releaseLease = vi.fn<(lease: LeaseRecord, options?: unknown) => Promise<void>>(
    async (_lease: LeaseRecord, _options?: unknown) => {
      expect(exclusive).toBe(false);
    },
  );
  const inventory = vi.fn<() => Promise<ProviderMachine[]>>(
    async (): Promise<ProviderMachine[]> => [],
  );
  const fleet = new FleetCoordinator(
    runtime as unknown as CoordinatorRuntime,
    env as Env,
    {
      [provider]: {
        listCrabboxServers: inventory,
        listReconciliationResources: inventory,
        releaseLease,
      },
    } as ConstructorParameters<typeof FleetCoordinator>[2],
  ) as unknown as Internals;
  return {
    fleet,
    values,
    storage,
    runtime,
    inventory,
    releaseLease,
    sweep: (trigger: "alarm" | "admin" = "admin", input = config) =>
      provider === "aws"
        ? fleet.runAWSOrphanSweepIfDue(trigger, input)
        : fleet.runAzureOrphanSweepIfDue(trigger, input),
  };
}

function ownedResource(provider: "aws" | "azure") {
  const lease = {
    id: "cbx_000000000001",
    provider,
    cloudID: "orphan",
    region: "region-a",
    state: "expired",
    keep: false,
    slug: "test-box",
    owner,
    org,
    expiresAt: iso(-3600000),
  } as LeaseRecord;
  const machine: ProviderMachine = {
    id: 1,
    provider,
    cloudID: "orphan",
    name: "orphan",
    region: "region-a",
    host: "",
    status: "running",
    serverType: "test",
    resourceIdentity: "exact-resource-identity",
    labels: {
      crabbox: "true",
      created_by: "crabbox",
      provider,
      lease: lease.id,
      slug: lease.slug,
      owner: "alice_example.com",
      created_at: String((now - 3600000) / 1000),
      expires_at: String((now - 3600000) / 1000),
    },
  };
  return { lease, machine };
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(now);
  vi.spyOn(console, "warn").mockImplementation(() => {});
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe.each(["aws", "azure"] as const)("D5 %s sweeps", (provider) => {
  it("keeps tag-only and wrong-owner candidates report-only in delete mode", async () => {
    await Promise.all(
      [false, true].map(async (recorded) => {
        const f = fixture(provider);
        const { lease, machine } = ownedResource(provider);
        if (recorded) {
          f.values.set(`lease:${lease.id}`, { ...lease, owner: "bob@example.com" });
        }
        f.inventory.mockResolvedValue([machine]);
        const result = await f.sweep();
        expect(result?.candidates).toMatchObject([
          { action: "reported", ownership: "provider-tags-only" },
        ]);
        expect(f.releaseLease).not.toHaveBeenCalled();
      }),
    );
  });

  it("quarantines exact ownership, preserves failures, and persists successful release", async () => {
    const f = fixture(provider);
    const { lease, machine } = ownedResource(provider);
    f.values.set(`lease:${lease.id}`, lease);
    f.inventory.mockResolvedValue([machine]);
    expect((await f.sweep())?.candidates).toMatchObject([
      { action: "quarantined", ownership: "coordinator-lease" },
    ]);
    const key = `provider-reconciliation:${provider}:region-a:orphan`;
    expect(f.values.get(key)).toMatchObject({ observations: 1 });
    vi.setSystemTime(now + 2000);
    f.releaseLease.mockRejectedValueOnce(new Error("synthetic release failure"));
    expect((await f.sweep())?.candidates).toMatchObject([
      { action: "terminate_failed", error: "synthetic release failure" },
    ]);
    expect(f.values.get(key)).toMatchObject({ observations: 2 });
    expect((await f.sweep())?.terminated).toBe(1);
    expect(f.values.has(key)).toBe(false);
    expect(f.values.has(`${provider}-orphan-sweep:first-alarm`)).toBe(false);
    expect(f.releaseLease.mock.calls.at(-1)).toEqual(
      provider === "azure" ? [lease, { resourceIdentity: machine.resourceIdentity }] : [lease],
    );
    expect(f.runtime.storage.transaction.mock.calls.length).toBe(provider === "azure" ? 3 : 0);
  });

  it("retains quarantine when authoritative inventory fails", async () => {
    const f = fixture(provider);
    const { lease, machine } = ownedResource(provider);
    f.values.set(`lease:${lease.id}`, lease);
    f.inventory.mockResolvedValue([machine]);
    await f.sweep();
    const key = `provider-reconciliation:${provider}:region-a:orphan`;
    const quarantine = f.values.get(key);
    if (provider === "aws")
      vi.spyOn(EC2SpotClient.prototype, "listMacHosts").mockRejectedValue(
        new Error("mac inventory failed"),
      );
    else f.inventory.mockRejectedValue(new Error("azure inventory failed"));
    const result = await f.sweep("admin", { ...config, macHostReleaseEnabled: provider === "aws" });
    expect(result?.errors).toHaveLength(1);
    expect(f.values.get(key)).toEqual(quarantine);
    expect(f.releaseLease).not.toHaveBeenCalled();
  });

  it("skips early alarms, permits admin, and serializes overlapping sweeps", async () => {
    const f = fixture(provider);
    f.values.set(`${provider}-orphan-sweep:last`, { finishedAt: iso() });
    expect(await f.sweep("alarm")).toBeUndefined();
    expect(f.inventory).not.toHaveBeenCalled();
    let finish!: () => void;
    let signalStarted!: () => void;
    const started = new Promise<void>((resolve) => {
      signalStarted = resolve;
    });
    f.inventory.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = () => resolve([]);
          signalStarted();
        }),
    );
    const first = f.sweep("admin");
    await started;
    const second = f.sweep("alarm");
    finish();
    await first;
    expect(await second).toBeUndefined();
    expect(f.inventory).toHaveBeenCalledTimes(1);
    expect(f.runtime.runExclusive).toHaveBeenCalled();
    expect(await f.sweep("admin", { ...config, enabled: false })).toBeUndefined();
  });

  it("persists the first alarm without postponement and keeps the one-second floor", async () => {
    const env =
      provider === "aws"
        ? { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "synthetic" }
        : {
            AZURE_TENANT_ID: "test",
            AZURE_CLIENT_ID: "test",
            AZURE_CLIENT_SECRET: "synthetic",
            AZURE_SUBSCRIPTION_ID: "test",
          };
    const f = fixture(provider, env);
    await f.fleet.scheduleAlarm();
    const key = `${provider}-orphan-sweep:first-alarm`;
    expect(f.values.get(key)).toBe(now + 60000);
    vi.setSystemTime(now + 30000);
    await f.fleet.scheduleAlarm();
    expect(f.values.get(key)).toBe(now + 60000);
    expect(f.runtime.scheduleAlarm).toHaveBeenLastCalledWith(now + 60000);
    vi.setSystemTime(now + 60000);
    await f.fleet.scheduleAlarm();
    expect(f.runtime.scheduleAlarm).toHaveBeenLastCalledWith(now + 61000);
  });
});

describe.each(["device", "grant"] as const)("D5 %s owner index", (kind) => {
  function indexedFixture() {
    const f = fixture();
    const id = kind === "device" ? deviceID : grantHash;
    const prefix =
      kind === "device"
        ? deviceOwnerIndexPrefix(owner, org)
        : pairingGrantOwnerIndexPrefix(owner, org);
    const key = kind === "device" ? deviceTokenKey(id) : pairingGrantKey(id);
    const record = {
      version: 1,
      owner,
      org,
      ownerLogin: "alice",
      name: "device",
      createdAt: iso(-10000),
      expiresAt: iso(1000),
      ownerGrant: { tokenID: deviceID, sealedCredential: "synthetic", expiresAt: iso(1000) },
      ...(kind === "device"
        ? { id, audience: "crabbox-device", scope: ["leases:read"], tokenHash: grantHash }
        : { grantHash, audience: "crabbox-device-pairing", scope: "leases:read" }),
    };
    f.values.set(prefix + id, {
      version: 1,
      expiresAt: iso(1000),
      ...(kind === "device" ? { deviceID: id } : { grantHash: id }),
    });
    f.values.set(key, record);
    return {
      ...f,
      id,
      key,
      prefix,
      record,
      load: (time = now) =>
        kind === "device"
          ? f.fleet.activeOwnerDevices(owner, org)
          : f.fleet.activeOwnerPairingGrants(owner, org, time),
    };
  }

  it("uses a bounded no-cache lookup and preserves the caller's clock", async () => {
    const f = indexedFixture();
    expect(await f.load()).toEqual([f.record]);
    expect(f.storage.list).toHaveBeenCalledWith({ prefix: f.prefix, limit: 11, noCache: true });
    expect(f.storage.get).toHaveBeenCalledWith(f.key, { noCache: true });
    vi.setSystemTime(now + 2000);
    expect(await f.load(now)).toHaveLength(kind === "device" ? 0 : 1);
    expect(await f.load(now + 2000)).toEqual([]);
    expect(f.values.has(f.key)).toBe(false);
    expect(f.values.has(f.prefix + f.id)).toBe(false);
  });

  it.each(["owner", "org", "identity", "version", "index"])(
    "deletes only the index for a %s mismatch",
    async (field) => {
      const f = indexedFixture();
      if (field === "index") f.values.set(f.prefix + f.id, { version: 0 });
      else
        f.values.set(f.key, {
          ...f.record,
          [field === "identity" ? (kind === "device" ? "id" : "grantHash") : field]: "mismatch",
          expiresAt: iso(-1000),
        });
      expect(await f.load()).toEqual([]);
      expect(f.values.has(f.key)).toBe(true);
      expect(f.values.has(f.prefix + f.id)).toBe(false);
    },
  );
});
