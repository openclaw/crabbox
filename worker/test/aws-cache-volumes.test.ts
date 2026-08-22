import { describe, expect, it } from "vitest";

import {
  AWSCacheVolumeLifecycle,
  type AWSCacheVolumeCloud,
  type AWSCacheVolumeDescription,
} from "../src/aws-cache-volumes";
import { leaseConfig, type LeaseConfig } from "../src/config";
import type { CoordinatorStorage, CoordinatorStorageView } from "../src/coordinator-runtime";
import {
  ProviderProvisioningCleanupError,
  validatedProviderProvisioningCleanupClaim,
} from "../src/provider-provisioning";

class MemoryStorage implements CoordinatorStorage {
  private readonly values = new Map<string, unknown>();
  private transactions = Promise.resolve();

  async get<T>(key: string): Promise<T | undefined> {
    return this.values.get(key) as T | undefined;
  }

  async put<T>(key: string, value: T): Promise<void> {
    this.values.set(key, structuredClone(value));
  }

  async delete(key: string): Promise<void> {
    this.values.delete(key);
  }

  async list<T>({ prefix = "" }: { prefix?: string } = {}): Promise<Map<string, T>> {
    return new Map(
      [...this.values]
        .filter(([key]) => key.startsWith(prefix))
        .map(([key, value]) => [key, structuredClone(value) as T]),
    );
  }

  async transaction<T>(callback: (transaction: CoordinatorStorageView) => Promise<T>): Promise<T> {
    let release!: () => void;
    const previous = this.transactions;
    this.transactions = new Promise<void>((resolve) => {
      release = resolve;
    });
    await previous;
    try {
      return await callback(this);
    } finally {
      release();
    }
  }

  entries(): [string, unknown][] {
    return [...this.values].map(([key, value]) => [key, structuredClone(value)]);
  }

  seed(key: string, value: unknown): void {
    this.values.set(key, structuredClone(value));
  }
}

class FakeCloud implements AWSCacheVolumeCloud {
  readonly volumes = new Map<string, AWSCacheVolumeDescription>();
  readonly createdTags: Record<string, string>[] = [];
  readonly deleted: string[] = [];
  availabilityZone = "us-west-2a";
  failAt = 0;
  ambiguousAt = 0;
  attachPending = 0;
  validationError?: Error;
  failCreates = false;
  findError?: Error;
  describeError?: Error;
  readonly sizes = new Map<string, number>();
  beforeDescribe?: (volumeID: string) => Promise<void>;
  private nextID = 1;

  async callerAccountID(): Promise<string> {
    return "123456789012";
  }

  async validateCacheVolumeInstanceType(_config: LeaseConfig): Promise<void> {
    if (this.validationError) throw this.validationError;
  }

  async cacheVolumeAvailabilityZone(_config: LeaseConfig): Promise<string> {
    return this.availabilityZone;
  }

  async createCacheVolume(
    availabilityZone: string,
    sizeGB: number,
    tags: Record<string, string>,
    _clientToken: string,
  ): Promise<string> {
    const id = `vol-${String(this.nextID++).padStart(8, "0")}`;
    if (this.failCreates || (this.failAt > 0 && this.nextID - 1 === this.failAt)) {
      throw new Error("injected create failure");
    }
    this.createdTags.push(structuredClone(tags));
    this.volumes.set(id, {
      id,
      state: "available",
      availabilityZone,
      encrypted: true,
      volumeType: "gp3",
      sizeGB,
      multiAttachEnabled: false,
      attachments: [],
      tags: structuredClone(tags),
    });
    this.sizes.set(id, sizeGB);
    if (this.ambiguousAt > 0 && this.nextID - 1 === this.ambiguousAt) {
      throw new Error("ambiguous create timeout");
    }
    return id;
  }

  async findCacheVolumes(
    availabilityZone: string,
    tags: Record<string, string>,
  ): Promise<AWSCacheVolumeDescription[]> {
    if (this.findError) throw this.findError;
    return [...this.volumes.values()]
      .filter(
        (volume) =>
          volume.availabilityZone === availabilityZone &&
          Object.entries(tags).every(([key, value]) => volume.tags[key] === value),
      )
      .map((volume) => structuredClone(volume));
  }

  async describeCacheVolume(volumeID: string): Promise<AWSCacheVolumeDescription> {
    await this.beforeDescribe?.(volumeID);
    if (this.describeError) throw this.describeError;
    const volume = this.volumes.get(volumeID);
    if (!volume) throw new Error(`AWS cache volume not found: ${volumeID}`);
    return structuredClone(volume);
  }

  async attachCacheVolume(volumeID: string, instanceID: string): Promise<void> {
    if (this.attachPending > 0) {
      this.attachPending -= 1;
      throw new Error("IncorrectInstanceState: instance is pending");
    }
    const volume = await this.describeCacheVolume(volumeID);
    volume.state = "in-use";
    volume.attachments = [instanceID];
    this.volumes.set(volumeID, volume);
  }

  async detachCacheVolume(volumeID: string, instanceID: string): Promise<void> {
    const volume = await this.describeCacheVolume(volumeID);
    if (volume.attachments.length !== 1 || volume.attachments[0] !== instanceID) {
      throw new Error(`unexpected attachment for ${volumeID}`);
    }
    volume.state = "available";
    volume.attachments = [];
    this.volumes.set(volumeID, volume);
  }

  async deleteCacheVolume(volumeID: string): Promise<void> {
    this.volumes.delete(volumeID);
    this.deleted.push(volumeID);
  }
}

describe("AWS cache volume lifecycle", () => {
  it("reuses sequentially while allocating distinct concurrent and AZ members", async () => {
    const storage = new MemoryStorage();
    const cloud = new FakeCloud();
    const lifecycle = new AWSCacheVolumeLifecycle(storage);
    const config = cacheConfig();

    const first = (await lifecycle.prepare(cloud, config, "lease-1", "tenant"))!;
    await lifecycle.attach(cloud, first, "i-1");
    await lifecycle.release(cloud, "lease-1");
    const [reused, concurrent] = await Promise.all([
      lifecycle.prepare(cloud, config, "lease-2", "tenant"),
      lifecycle.prepare(cloud, config, "lease-3", "tenant"),
    ]);
    expect([reused!.bindings[0].volumeID, concurrent!.bindings[0].volumeID]).toContain(
      first.bindings[0].volumeID,
    );
    expect(concurrent!.bindings[0].volumeID).not.toBe(reused.bindings[0].volumeID);

    cloud.availabilityZone = "us-west-2b";
    const otherAZ = (await lifecycle.prepare(cloud, config, "lease-4", "tenant"))!;
    expect(otherAZ.bindings[0].volumeID).not.toBe(reused!.bindings[0].volumeID);
  });

  it("uses opaque tags, exact serial bootstrap, detach proof, purge, and external refusal", async () => {
    const storage = new MemoryStorage();
    const cloud = new FakeCloud();
    const lifecycle = new AWSCacheVolumeLifecycle(storage);
    const config = cacheConfig();
    const plan = (await lifecycle.prepare(cloud, config, "lease-owned", "tenant-secret"))!;
    const id = plan.bindings[0].volumeID;

    expect(JSON.stringify(cloud.createdTags)).not.toContain("repo-secret");
    expect(JSON.stringify(cloud.createdTags)).not.toContain("cache-secret");
    expect(JSON.stringify(cloud.createdTags)).not.toContain("/var/cache/build");
    expect(plan.bootstrap).toContain(id.replaceAll("-", ""));
    expect(plan.bootstrap).toContain("for cache_wait in $(seq 1 120)");
    expect(plan.bootstrap).toContain("mount -o nodev,nosuid");
    expect(plan.bootstrap).toContain('readlink -f -- "$cache_path"');
    expect(plan.bootstrap).toContain("chown 'crabbox:crabbox'");
    expect(plan.readyChecks).toContain("mountpoint -q");

    cloud.attachPending = 1;
    await lifecycle.attach(cloud, plan, "i-owned");
    await lifecycle.release(cloud, "lease-owned");
    expect(cloud.volumes.get(id)?.attachments).toEqual([]);

    const purge = (await lifecycle.prepare(cloud, config, "lease-purge", "tenant-secret"))!;
    await lifecycle.release(cloud, "lease-purge", true);
    expect(cloud.volumes.has(purge.bindings[0].volumeID)).toBe(false);

    const external = (await lifecycle.prepare(cloud, config, "lease-external", "tenant-secret"))!;
    await lifecycle.attach(cloud, external, "i-owned");
    const externalVolume = cloud.volumes.get(external.bindings[0].volumeID)!;
    externalVolume.attachments = ["i-other"];
    await expect(lifecycle.release(cloud, "lease-external", true)).rejects.toThrow(
      "external attachment",
    );
    expect(cloud.volumes.has(external.bindings[0].volumeID)).toBe(true);

    const gc = (await lifecycle.prepare(cloud, config, "lease-gc", "tenant-gc"))!;
    await lifecycle.release(cloud, "lease-gc");
    const tagOnly = await cloud.createCacheVolume(
      "us-west-2a",
      20,
      { crabbox: "true" },
      "tag-only",
    );
    await expect(
      lifecycle.garbageCollect(cloud, "us-west-2", new Date(Date.now() + 1000)),
    ).resolves.toContain(gc.bindings[0].volumeID);
    expect(cloud.volumes.has(tagOnly)).toBe(true);
  });

  it("rejects required private and Windows modes and retains pre-instance cleanup claims", () => {
    expect(() => cacheConfig({ target: "windows", windowsMode: "normal" })).toThrow(
      "public AWS Linux SSH lease",
    );
    expect(() => cacheConfig({ awsPrivate: true, awsRequireSSM: true })).toThrow(
      "public AWS Linux SSH lease",
    );
    expect(() =>
      cacheConfig({
        cacheVolumes: [{ name: "bad", key: "bad", path: "/var/cache/..", required: true }],
      }),
    ).toThrow("safe absolute path");
    for (const path of [
      "/root",
      "/home",
      "/home/crabbox",
      "/home/crabbox/.ssh",
      "/home/crabbox/.ssh/authorized_keys",
      "/var",
      "/var/lib",
      "/var/lib/cloud",
      "/var/lib/cloud/instance",
      "/var/lib/crabbox",
      "/var/lib/crabbox/cache-volumes",
      "/workspaces",
    ]) {
      expect(() =>
        cacheConfig({
          workRoot: "/workspaces",
          cacheVolumes: [{ name: "bad", key: "bad", path, required: true }],
        }),
      ).toThrow(/would hide runtime root|overlaps sensitive runtime tree/u);
    }
    for (const path of [
      "/home/crabbox/.cache/build",
      "/workspaces/.cache/build",
      "/var/cache/build",
    ]) {
      expect(
        cacheConfig({
          workRoot: "/workspaces",
          cacheVolumes: [{ name: "safe", key: "safe", path, required: true }],
        }).cacheVolumes[0]?.path,
      ).toBe(path);
    }

    const error = new ProviderProvisioningCleanupError(
      "cache rollback failed",
      { provider: "aws", region: "us-west-2", cacheVolumeProtocol: 1 },
      new Error("detach failed"),
    );
    expect(validatedProviderProvisioningCleanupClaim(error, "aws")).toEqual({
      provider: "aws",
      region: "us-west-2",
      cacheVolumeProtocol: 1,
    });
  });

  it("rolls back partial preparation and recovers the post-create durability window", async () => {
    const storage = new MemoryStorage();
    const cloud = new FakeCloud();
    const lifecycle = new AWSCacheVolumeLifecycle(storage);
    cloud.failAt = 2;
    const config = cacheConfig({
      cacheVolumes: [
        { name: "one", key: "one", path: "/var/cache/one", sizeGB: 20 },
        { name: "two", key: "two", path: "/var/cache/two", sizeGB: 20 },
      ],
    });
    await expect(lifecycle.prepare(cloud, config, "lease-rollback", "tenant")).rejects.toThrow(
      "injected create failure",
    );
    const first = storage
      .entries()
      .map(([, value]) => value as { state?: string; leaseID?: string })
      .find((record) => record.state === "available");
    expect(first).toMatchObject({ state: "available", leaseID: undefined });

    cloud.failAt = 0;
    const plan = (await lifecycle.prepare(cloud, cacheConfig(), "lease-recover", "tenant"))!;
    const volumeID = plan.bindings[0].volumeID;
    const replayed = (await lifecycle.prepare(cloud, cacheConfig(), "lease-recover", "tenant"))!;
    expect(replayed.bindings[0].volumeID).toBe(volumeID);
    const committed = storage.entries().find(([key]) => key.endsWith(volumeID))!;
    const pending = {
      ...(committed[1] as Record<string, unknown>),
      volumeID: undefined,
    };
    await storage.delete(committed[0]);
    storage.seed(`aws-cache-volume:v1:pending:${pending["memberID"]}`, pending);
    const recovered = (await lifecycle.prepare(cloud, cacheConfig(), "lease-recover", "tenant"))!;
    expect(recovered.bindings[0].volumeID).toBe(volumeID);
    await lifecycle.release(cloud, "lease-recover");
    const renamed = (await lifecycle.prepare(
      cloud,
      cacheConfig({
        cacheVolumes: [
          {
            name: "renamed",
            key: "cache-secret",
            path: "/var/cache/renamed",
            sizeGB: 20,
            required: true,
          },
        ],
      }),
      "lease-renamed",
      "tenant",
    ))!;
    expect(renamed.bindings[0]).toMatchObject({
      volumeID,
      name: "renamed",
      path: "/var/cache/renamed",
    });
  });

  it("claims GC candidates before a concurrent reservation can reuse them", async () => {
    const storage = new MemoryStorage();
    const cloud = new FakeCloud();
    const lifecycle = new AWSCacheVolumeLifecycle(storage);
    const config = cacheConfig();
    const first = (await lifecycle.prepare(cloud, config, "lease-old", "tenant"))!;
    await lifecycle.release(cloud, "lease-old");

    let releaseDescribe!: () => void;
    const describeBlocked = new Promise<void>((resolve) => {
      releaseDescribe = resolve;
    });
    let enteredResolve!: () => void;
    const entered = new Promise<void>((resolve) => {
      enteredResolve = resolve;
    });
    cloud.beforeDescribe = async (volumeID) => {
      if (volumeID === first.bindings[0].volumeID) {
        enteredResolve();
        await describeBlocked;
      }
    };
    const gc = lifecycle.garbageCollect(cloud, "us-west-2", new Date(Date.now() + 1000));
    await entered;
    const concurrent = await lifecycle.prepare(cloud, config, "lease-new", "tenant");
    expect(concurrent!.bindings[0].volumeID).not.toBe(first.bindings[0].volumeID);
    releaseDescribe();
    await gc;
  });

  it("rejects blank release and garbage collects quarantined reservations", async () => {
    const storage = new MemoryStorage();
    const cloud = new FakeCloud();
    const lifecycle = new AWSCacheVolumeLifecycle(storage);
    const plan = (await lifecycle.prepare(cloud, cacheConfig(), "lease-quarantined", "tenant"))!;
    await expect(lifecycle.release(cloud, "", true)).rejects.toThrow("exact lease ID");
    expect(cloud.volumes.has(plan.bindings[0].volumeID)).toBe(true);

    const [key, value] = storage
      .entries()
      .find(([entryKey]) => entryKey.endsWith(plan.bindings[0].volumeID))!;
    storage.seed(key, {
      ...(value as Record<string, unknown>),
      state: "quarantined",
      updatedAt: new Date(Date.now() - 8 * 24 * 60 * 60 * 1000).toISOString(),
    });
    await expect(
      lifecycle.garbageCollect(cloud, "us-west-2", new Date(Date.now() - 7 * 24 * 60 * 60 * 1000)),
    ).resolves.toEqual([plan.bindings[0].volumeID]);
  });

  it("replaces undersized members and garbage collects ambiguous creates", async () => {
    const storage = new MemoryStorage();
    const cloud = new FakeCloud();
    const lifecycle = new AWSCacheVolumeLifecycle(storage);
    const first = (await lifecycle.prepare(cloud, cacheConfig(), "lease-small", "tenant"))!;
    await lifecycle.release(cloud, "lease-small");

    const large = (await lifecycle.prepare(
      cloud,
      cacheConfig({
        cacheVolumes: [
          {
            name: "build",
            key: "cache-secret",
            path: "/var/cache/build",
            sizeGB: 100,
            required: true,
          },
        ],
      }),
      "lease-large",
      "tenant",
    ))!;
    expect(large.bindings[0].volumeID).not.toBe(first.bindings[0].volumeID);
    expect(cloud.sizes.get(large.bindings[0].volumeID)).toBe(100);

    const ambiguousStorage = new MemoryStorage();
    const ambiguousCloud = new FakeCloud();
    const ambiguousLifecycle = new AWSCacheVolumeLifecycle(ambiguousStorage);
    ambiguousCloud.ambiguousAt = 1;
    await expect(
      ambiguousLifecycle.prepare(
        ambiguousCloud,
        cacheConfig({
          repoScope: "other-repo",
          cacheVolumes: [
            {
              name: "ambiguous",
              key: "ambiguous",
              path: "/var/cache/ambiguous",
              sizeGB: 20,
              required: true,
            },
          ],
        }),
        "lease-ambiguous",
        "tenant",
      ),
    ).rejects.toThrow("ambiguous create timeout");
    await expect(
      ambiguousLifecycle.garbageCollect(ambiguousCloud, "us-west-2", new Date(Date.now() + 1000)),
    ).resolves.toHaveLength(1);
  });

  it("rejects duplicate bindings and non-Nitro types before creation", async () => {
    expect(() =>
      cacheConfig({
        cacheVolumes: [
          { name: "one", key: "one", path: "/var/cache/shared", sizeGB: 20 },
          { name: "two", key: "two", path: "/var/cache/shared", sizeGB: 20 },
        ],
      }),
    ).toThrow("unique names and mount paths");

    const cloud = new FakeCloud();
    cloud.validationError = new Error("AWS cache volumes require a Nitro/NVMe instance type");
    await expect(
      new AWSCacheVolumeLifecycle(new MemoryStorage()).prepare(
        cloud,
        cacheConfig(),
        "lease-xen",
        "tenant",
      ),
    ).rejects.toThrow("Nitro/NVMe");
    expect(cloud.volumes.size).toBe(0);

    expect(() =>
      cacheConfig({
        cacheVolumes: Array.from({ length: 12 }, (_, index) => ({
          name: `cache-${index}`,
          key: `key-${index}`,
          path: `/var/cache/cache-${index}`,
          sizeGB: 20,
        })),
      }),
    ).toThrow("at most 11 bindings");
  });

  it("reconciles deletion tombstones after ambiguous success", async () => {
    const storage = new MemoryStorage();
    const cloud = new FakeCloud();
    const lifecycle = new AWSCacheVolumeLifecycle(storage);
    const plan = (await lifecycle.prepare(cloud, cacheConfig(), "lease-delete", "tenant"))!;
    const [key, value] = storage
      .entries()
      .find(([entryKey]) => entryKey.endsWith(plan.bindings[0].volumeID))!;
    storage.seed(key, { ...(value as Record<string, unknown>), state: "deleting" });
    cloud.volumes.delete(plan.bindings[0].volumeID);
    await lifecycle.garbageCollect(cloud, "us-west-2", new Date());
    expect(storage.entries().some(([entryKey]) => entryKey === key)).toBe(false);
  });

  it("does not let repeated create failures consume member capacity", async () => {
    const storage = new MemoryStorage();
    const cloud = new FakeCloud();
    const lifecycle = new AWSCacheVolumeLifecycle(storage);
    cloud.failCreates = true;
    for (let index = 0; index < 9; index += 1) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- capacity accounting is proven across serial failed reservations.
      await expect(
        lifecycle.prepare(cloud, cacheConfig(), `lease-failure-${index}`, "tenant"),
      ).rejects.toThrow("injected create failure");
    }
    for (const [index, [key, value]] of storage.entries().entries()) {
      const record = value as Record<string, unknown>;
      expect(record["state"]).toBe("quarantined");
      expect(Object.hasOwn(record, "volumeID")).toBe(false);
      storage.seed(key, {
        ...record,
        state: index === 0 ? "reserving" : record["state"],
        updatedAt: new Date(Date.now() - 8 * 24 * 60 * 60 * 1000).toISOString(),
      });
    }
    await lifecycle.garbageCollect(
      cloud,
      "us-west-2",
      new Date(Date.now() - 7 * 24 * 60 * 60 * 1000),
    );
    expect(storage.entries()).toEqual([]);
    cloud.failCreates = false;
    await expect(
      lifecycle.prepare(cloud, cacheConfig(), "lease-recovered-capacity", "tenant"),
    ).resolves.toBeDefined();
  });

  it("does not let repeated discovery failures consume member capacity", async () => {
    const storage = new MemoryStorage();
    const cloud = new FakeCloud();
    const lifecycle = new AWSCacheVolumeLifecycle(storage);
    cloud.findError = new Error("injected discovery failure");
    for (let index = 0; index < 9; index += 1) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- capacity accounting is proven across serial discovery failures.
      await expect(
        lifecycle.prepare(cloud, cacheConfig(), `lease-discovery-${index}`, "tenant"),
      ).rejects.toThrow("injected discovery failure");
    }
    for (const [, value] of storage.entries()) {
      expect(value).toMatchObject({
        state: "quarantined",
        lastError: "injected discovery failure",
      });
      expect((value as Record<string, unknown>)["lastErrorAt"]).toEqual(expect.any(String));
      expect((value as Record<string, unknown>)["retryCount"]).toEqual(expect.any(Number));
    }
    cloud.findError = undefined;
    await expect(
      lifecycle.prepare(cloud, cacheConfig(), "lease-after-discovery", "tenant"),
    ).resolves.toBeDefined();
  });

  it("quarantines describe failures without poisoning member capacity", async () => {
    const storage = new MemoryStorage();
    const cloud = new FakeCloud();
    const lifecycle = new AWSCacheVolumeLifecycle(storage);
    const first = (await lifecycle.prepare(
      cloud,
      cacheConfig(),
      "lease-before-describe",
      "tenant",
    ))!;
    await lifecycle.release(cloud, "lease-before-describe");
    cloud.describeError = new Error("injected describe failure");
    await expect(
      lifecycle.prepare(cloud, cacheConfig(), "lease-describe", "tenant"),
    ).rejects.toThrow("injected describe failure");
    const failed = storage
      .entries()
      .map(([, value]) => value as Record<string, unknown>)
      .find((record) => record["volumeID"] === first.bindings[0].volumeID);
    expect(failed).toMatchObject({
      state: "quarantined",
      lastError: "injected describe failure",
    });
    cloud.describeError = undefined;
    const replacement = (await lifecycle.prepare(
      cloud,
      cacheConfig(),
      "lease-after-describe",
      "tenant",
    ))!;
    expect(replacement.bindings[0].volumeID).not.toBe(first.bindings[0].volumeID);
  });

  it.each([
    ["unencrypted", (volume: AWSCacheVolumeDescription) => (volume.encrypted = false)],
    ["wrong type", (volume: AWSCacheVolumeDescription) => (volume.volumeType = "io2")],
    ["wrong size", (volume: AWSCacheVolumeDescription) => (volume.sizeGB += 1)],
    ["multi attach", (volume: AWSCacheVolumeDescription) => (volume.multiAttachEnabled = true)],
  ])("rejects exact-tag volumes with mutated %s properties", async (_name, mutate) => {
    const storage = new MemoryStorage();
    const cloud = new FakeCloud();
    const lifecycle = new AWSCacheVolumeLifecycle(storage);
    const first = (await lifecycle.prepare(cloud, cacheConfig(), "lease-original", "tenant"))!;
    await lifecycle.release(cloud, "lease-original");
    const id = first.bindings[0].volumeID;
    const volume = cloud.volumes.get(id)!;
    mutate(volume);
    cloud.volumes.set(id, volume);
    const replacement = (await lifecycle.prepare(
      cloud,
      cacheConfig(),
      "lease-replacement",
      "tenant",
    ))!;
    expect(replacement.bindings[0].volumeID).not.toBe(id);
  });

  it("refuses to adopt an incompatible exact-tag clone", async () => {
    const storage = new MemoryStorage();
    const cloud = new FakeCloud();
    const lifecycle = new AWSCacheVolumeLifecycle(storage);
    const plan = (await lifecycle.prepare(cloud, cacheConfig(), "lease-original", "tenant"))!;
    const id = plan.bindings[0].volumeID;
    const [key, value] = storage.entries().find(([entryKey]) => entryKey.endsWith(id))!;
    const pending = {
      ...(value as Record<string, unknown>),
      state: "reserving",
      leaseID: "lease-adopt",
    };
    delete pending["volumeID"];
    await storage.delete(key);
    storage.seed(`aws-cache-volume:v1:pending:${pending["memberID"]}`, pending);
    const volume = cloud.volumes.get(id)!;
    volume.volumeType = "io2";
    cloud.volumes.set(id, volume);

    await expect(lifecycle.prepare(cloud, cacheConfig(), "lease-adopt", "tenant")).rejects.toThrow(
      "incompatible volume",
    );
    expect(cloud.volumes.has(id)).toBe(true);
    const stored = storage.entries().map(([, entry]) => entry as Record<string, unknown>);
    expect(stored).toHaveLength(1);
    expect(stored[0]).toMatchObject({ state: "quarantined" });
    expect(Object.hasOwn(stored[0]!, "volumeID")).toBe(false);
  });
});

function cacheConfig(overrides: Partial<Parameters<typeof leaseConfig>[0]> = {}): LeaseConfig {
  return leaseConfig({
    provider: "aws",
    target: "linux",
    awsRegion: "us-west-2",
    sshPublicKey: "ssh-ed25519 test",
    cacheVolumeProtocol: 1,
    cacheVolumes: [
      {
        name: "build",
        key: "cache-secret",
        path: "/var/cache/build",
        sizeGB: 20,
        required: true,
      },
    ],
    repoScope: "repo-secret",
    ...overrides,
  });
}
