import { afterEach, describe, expect, it, vi } from "vitest";

import {
  CheckpointError,
  checkpointDueKey,
  checkpointDeleteClaimTTLMS,
  checkpointKey,
  checkpointMaxCreateRecoveryAttempts,
  checkpointPinKey,
  checkpointResourceKey,
  checkpointUseClaimTTLMS,
  bindCheckpointUseProvisioning,
  claimCheckpointDeletion,
  findManagedCheckpointImage,
  markCheckpointProviderDeleted,
  pinCheckpointPromotion,
  recordCheckpointCreateRecoveryFailure,
  unpinCheckpointPromotion,
} from "../src/checkpoints";
import type {
  CoordinatorRuntime,
  CoordinatorSocketHandlers,
  CoordinatorStorage,
  CoordinatorStorageView,
  CoordinatorWebSocketUpgrade,
} from "../src/coordinator-runtime";
import { AWSProvider, AzureProvider, FleetCoordinator, GCPProvider } from "../src/fleet";
import { orgKeyForLabel } from "../src/org-identity";
import type {
  CoordinatorCheckpointProvider,
  CoordinatorCheckpointRecord,
  CoordinatorCheckpointScope,
  Env,
  LeaseRecord,
  ProviderCheckpointOwnership,
  ProviderImage,
} from "../src/types";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

class CheckpointMemoryStorage implements CoordinatorStorage {
  private values = new Map<string, unknown>();
  private transactionTail = Promise.resolve();
  readonly lists: Array<{ prefix?: string; limit?: number }> = [];
  failKey?: string;

  async get<T>(key: string): Promise<T | undefined> {
    const value = this.values.get(key);
    return value === undefined ? undefined : structuredClone(value as T);
  }

  async put<T>(key: string, value: T): Promise<void> {
    if (key === this.failKey) throw new Error("injected checkpoint storage failure");
    this.values.set(key, structuredClone(value));
  }

  async delete(key: string): Promise<void> {
    this.values.delete(key);
  }

  async list<T>(
    options: { prefix?: string; limit?: number; startAfter?: string } = {},
  ): Promise<Map<string, T>> {
    this.lists.push({
      ...(options.prefix ? { prefix: options.prefix } : {}),
      ...(options.limit ? { limit: options.limit } : {}),
    });
    const matches = [...this.values]
      .toSorted(([left], [right]) => left.localeCompare(right))
      .filter(
        ([key]) =>
          key.startsWith(options.prefix ?? "") && (!options.startAfter || key > options.startAfter),
      );
    return new Map(
      (options.limit === undefined ? matches : matches.slice(0, options.limit)).map(
        ([key, value]) => [key, structuredClone(value as T)],
      ),
    );
  }

  async transaction<T>(callback: (transaction: CoordinatorStorageView) => Promise<T>): Promise<T> {
    const predecessor = this.transactionTail;
    let release!: () => void;
    this.transactionTail = new Promise<void>((resolve) => {
      release = resolve;
    });
    await predecessor;
    const previous = this.values;
    this.values = structuredClone(previous);
    try {
      return await callback(this);
    } catch (error) {
      this.values = previous;
      throw error;
    } finally {
      release();
    }
  }

  snapshot(): string {
    return JSON.stringify([...this.values]);
  }
}

class CheckpointRuntime implements CoordinatorRuntime {
  readonly ephemeralWebSocketMaxPayloadBytes = 1024 * 1024;
  alarmTime?: number;
  private exclusiveTail = Promise.resolve();

  constructor(readonly storage: CheckpointMemoryStorage) {}

  async runExclusive<T>(callback: () => Promise<T>): Promise<T> {
    const predecessor = this.exclusiveTail;
    let release!: () => void;
    this.exclusiveTail = new Promise<void>((resolve) => {
      release = resolve;
    });
    await predecessor;
    try {
      return await callback();
    } finally {
      release();
    }
  }

  createWebSocketUpgrade(): CoordinatorWebSocketUpgrade {
    throw new Error("checkpoint tests do not use WebSockets");
  }

  getWebSockets(): Iterable<WebSocket> {
    return [];
  }

  socketAttachment<T>(): T | undefined {
    return undefined;
  }

  setSocketAttachment(): void {}

  acceptWebSocket(
    _socket: WebSocket,
    _attachment: unknown,
    _tags: string[],
    _handlers: CoordinatorSocketHandlers,
  ): void {}

  acceptEphemeralWebSocket(_socket: WebSocket, _handlers: CoordinatorSocketHandlers): void {}

  async take<T>(key: string): Promise<T | undefined> {
    const value = await this.storage.get<T>(key);
    await this.storage.delete(key);
    return value;
  }

  async getAlarm(): Promise<number | undefined> {
    return this.alarmTime;
  }

  async scheduleAlarm(time: number): Promise<void> {
    this.alarmTime = time;
  }

  async clearAlarm(): Promise<void> {
    delete this.alarmTime;
  }
}

const org = orgKeyForLabel("example-org");
const leaseID = "cbx_000000000001";

function checkpointLease(provider: CoordinatorCheckpointProvider): LeaseRecord {
  const now = new Date().toISOString();
  return {
    id: leaseID,
    slug: "source-box",
    provider,
    lifecycle: "managed",
    target: "linux",
    cloudID: provider === "aws" ? "i-000000000001" : "source-vm",
    region:
      provider === "azure" ? "westeurope" : provider === "gcp" ? "us-central1-a" : "eu-west-1",
    ...(provider === "azure"
      ? { providerScope: "/subscriptions/sub-123/resourceGroups/checkpoint-rg" }
      : {}),
    ...(provider === "gcp" ? { providerProject: "example-project" } : {}),
    owner: "alice@example.com",
    org,
    profile: "default",
    class: "standard",
    serverType: "standard-small",
    serverID: 1,
    serverName: "source-box",
    providerKey: "crabbox-source",
    host: "127.0.0.1",
    sshUser: "crabbox",
    sshPort: "22",
    workRoot: "/work",
    keep: true,
    ttlSeconds: 3600,
    estimatedHourlyUSD: 1,
    maxEstimatedUSD: 1,
    state: "active",
    createdAt: now,
    updatedAt: now,
    expiresAt: new Date(Date.now() + 3_600_000).toISOString(),
  };
}

function providerScope(provider: CoordinatorCheckpointProvider): CoordinatorCheckpointScope {
  if (provider === "aws") return { region: "eu-west-1", accountID: "123456789012" };
  if (provider === "azure") {
    return { region: "westeurope", subscriptionID: "sub-123", resourceGroup: "checkpoint-rg" };
  }
  return { region: "us-central1-a", project: "example-project" };
}

function providerImage(
  provider: CoordinatorCheckpointProvider,
  name: string,
  strategy: "image" | "disk-snapshot",
  ownership: ProviderCheckpointOwnership,
): ProviderImage {
  if (provider === "aws") {
    const id = strategy === "image" ? "ami-000000000001" : "snap-000000000001";
    return {
      id,
      name,
      state: "available",
      provider,
      kind: strategy === "image" ? "aws-ami" : "aws-ebs-snapshot",
      region: "eu-west-1",
      accountID: "123456789012",
      resourceID: id,
      immutableID: id,
      checkpointOwnershipHash: ownership.tokenHash,
      checkpointSourceLeaseID: ownership.sourceLeaseID,
      ...(strategy === "image" ? { snapshots: ["snap-backing-0001"] } : { snapshots: [id] }),
    };
  }
  if (provider === "azure") {
    return {
      id: name,
      name,
      state: "succeeded",
      provider,
      kind: "azure-os-disk-snapshot",
      region: "westeurope",
      resourceID: `/subscriptions/sub-123/resourceGroups/checkpoint-rg/providers/Microsoft.Compute/snapshots/${name}`,
      immutableID: "azure-immutable-0001",
      checkpointOwnershipHash: ownership.tokenHash,
      checkpointSourceLeaseID: ownership.sourceLeaseID,
    };
  }
  const collection = strategy === "image" ? "machineImages" : "snapshots";
  return {
    id: name,
    name,
    state: "ready",
    provider,
    kind: strategy === "image" ? "gcp-machine-image" : "gcp-disk-snapshot",
    region: "us-central1-a",
    project: "example-project",
    resourceID: `projects/example-project/global/${collection}/${name}`,
    immutableID: "987654321012345678",
    checkpointOwnershipHash: ownership.tokenHash,
    checkpointSourceLeaseID: ownership.sourceLeaseID,
  };
}

async function checkpointFixture(providerName: CoordinatorCheckpointProvider = "aws") {
  const storage = new CheckpointMemoryStorage();
  const runtime = new CheckpointRuntime(storage);
  const env = {
    FLEET: {} as DurableObjectNamespace,
    HETZNER_TOKEN: "",
    CRABBOX_DEFAULT_ORG: "example-org",
  } satisfies Env;
  const provider = new AWSProvider(env, "eu-west-1", storage);
  vi.spyOn(provider, "checkpointScope").mockResolvedValue(providerScope(providerName));
  vi.spyOn(provider, "validateCheckpointLeaseScope").mockResolvedValue(undefined);
  vi.spyOn(provider, "createCheckpointImage").mockImplementation(
    async (_lease, name, _noReboot, strategy, ownership) =>
      providerImage(providerName, name, strategy, ownership),
  );
  const deleteImage = vi.spyOn(provider, "deleteCheckpointImage").mockResolvedValue(undefined);
  const coordinator = new FleetCoordinator(runtime, env, { [providerName]: provider });
  await storage.put(`lease:${leaseID}`, checkpointLease(providerName));
  return { storage, runtime, coordinator, provider, deleteImage };
}

function checkpointRequest(
  method: string,
  path: string,
  body?: unknown,
  options: { owner?: string; org?: string; admin?: boolean } = {},
): Request {
  return new Request(`https://checkpoint.test${path}`, {
    method,
    headers: {
      "content-type": "application/json",
      "x-crabbox-owner": options.owner ?? "alice@example.com",
      "x-crabbox-org": options.org ?? "example-org",
      ...(options.admin ? { "x-crabbox-admin": "true" } : {}),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

async function createCheckpoint(
  coordinator: FleetCoordinator,
  checkpointID: string,
  retention: { mode: "manual" } | { mode: "expire-unused"; unusedForSeconds: number } = {
    mode: "manual",
  },
  strategy: "image" | "disk-snapshot" = "disk-snapshot",
): Promise<Response> {
  return coordinator.fetch(
    checkpointRequest("POST", "/v1/checkpoints", {
      id: checkpointID,
      leaseID,
      name: "prepared-workspace",
      strategy,
      retention,
      workdir: "/work/source/my-app",
      repo: { name: "my-app", head: "abc123" },
    }),
  );
}

describe("coordinator-managed checkpoints", () => {
  it.each(["aws", "azure", "gcp"] as const)(
    "reserves and publishes exact %s ownership with manual retention by default",
    async (providerName) => {
      const { coordinator, storage } = await checkpointFixture(providerName);
      const response = await createCheckpoint(coordinator, `chk_${providerName}_manual`);
      expect(response.status).toBe(201);
      const record = await storage.get<CoordinatorCheckpointRecord>(
        checkpointKey(`chk_${providerName}_manual`),
      );
      expect(record).toMatchObject({
        version: 1,
        owner: "alice@example.com",
        org,
        provider: providerName,
        state: "ready",
        retention: { mode: "manual" },
        generation: 1,
        activeUseCount: 0,
        pinCount: 0,
      });
      expect(record?.createClaim).toBeUndefined();
      expect(await storage.list({ prefix: "checkpoint-due:" })).toHaveLength(0);
      expect(
        await storage.get(
          checkpointResourceKey(
            providerName,
            record!.scope,
            record!.image!.kind,
            record!.image!.resourceID,
          ),
        ),
      ).toMatchObject({ checkpointID: record!.id, immutableID: record!.image!.immutableID });
    },
  );

  it.each([
    ["aws", { azureLocation: "westeurope" }],
    ["azure", { awsRegion: "eu-west-1" }],
    ["gcp", { gcpProject: "foreign-project" }],
    ["gcp", { gcpZone: "europe-west1-b" }],
  ] as const)(
    "rejects foreign provider scope in a %s checkpoint-backed lease",
    async (providerName, foreignScope) => {
      const { coordinator, storage, provider } = await checkpointFixture(providerName);
      const checkpointID = `chk_foreign_scope_${providerName}_${Object.keys(foreignScope)[0]}`;
      await createCheckpoint(coordinator, checkpointID);
      const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
      const acquired = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        )
      ).json()) as { claim: string };
      const selector =
        providerName === "aws"
          ? { awsSnapshot: record.image!.id }
          : providerName === "azure"
            ? { azureSnapshot: record.image!.id }
            : { gcpSnapshot: record.image!.id };
      vi.mocked(provider.validateCheckpointLeaseScope).mockClear();
      const response = await coordinator.fetch(
        checkpointRequest("POST", "/v1/leases/from-checkpoint", {
          provider: providerName,
          checkpointID,
          checkpointUseClaim: acquired.claim,
          leaseID: "cbx_000000000002",
          createAttemptID: `cat_${"f".repeat(32)}`,
          ...selector,
          ...foreignScope,
        }),
      );
      expect(response.status).toBe(409);
      await expect(response.json()).resolves.toMatchObject({ error: "checkpoint_source_mismatch" });
      expect(provider.validateCheckpointLeaseScope).not.toHaveBeenCalled();
    },
  );

  it.each(["image", "disk-snapshot"] as const)(
    "supports GCP %s checkpoint resources",
    async (strategy) => {
      const { coordinator, storage } = await checkpointFixture("gcp");
      const response = await createCheckpoint(
        coordinator,
        `chk_gcp_${strategy.replace("-", "_")}`,
        { mode: "manual" },
        strategy,
      );
      expect(response.status).toBe(201);
      const body = (await response.json()) as { checkpoint: CoordinatorCheckpointRecord };
      expect(body.checkpoint.image?.kind).toBe(
        strategy === "image" ? "gcp-machine-image" : "gcp-disk-snapshot",
      );
      expect(await storage.get(checkpointKey(body.checkpoint.id))).toBeDefined();
    },
  );

  it("rejects malformed IDs and retention before reserving or mutating providers", async () => {
    const { coordinator, provider, storage } = await checkpointFixture();
    const invalidIDs = await Promise.all(
      ["../chk_bad", "chk_", "bad"].map(
        async (value) => await createCheckpoint(coordinator, value),
      ),
    );
    expect(invalidIDs.map((response) => response.status)).toEqual([400, 400, 400]);
    const invalidRetention = await Promise.all(
      [
        { mode: "expire-unused", unusedForSeconds: 0 },
        { mode: "expire-unused", unusedForSeconds: -1 },
        { mode: "expire-unused", unusedForSeconds: 1.5 },
        { mode: "operator-default" },
      ].map(
        async (retention) =>
          await coordinator.fetch(
            checkpointRequest("POST", "/v1/checkpoints", {
              id: "chk_bad_retention",
              leaseID,
              name: "prepared-workspace",
              retention,
            }),
          ),
      ),
    );
    expect(invalidRetention.map((response) => response.status)).toEqual([400, 400, 400, 400]);
    expect(provider.createCheckpointImage).not.toHaveBeenCalled();
    expect(await storage.list({ prefix: "checkpoint:" })).toHaveLength(0);
  });

  it("persists its exact creation reservation before provider mutation and excludes remote URLs", async () => {
    const { coordinator, provider, storage } = await checkpointFixture();
    vi.mocked(provider.createCheckpointImage).mockImplementationOnce(
      async (_lease, name, _noReboot, strategy, ownership) => {
        const reservation = await storage.get<CoordinatorCheckpointRecord>(
          checkpointKey("chk_reserved_first"),
        );
        expect(reservation).toMatchObject({
          state: "creating",
          generation: 1,
          createClaim: { tokenHash: ownership.tokenHash },
        });
        expect(storage.snapshot()).not.toContain("signed-query-value");
        return providerImage("aws", name, strategy, ownership);
      },
    );
    const response = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints", {
        id: "chk_reserved_first",
        leaseID,
        name: "prepared-workspace",
        strategy: "disk-snapshot",
        repo: {
          name: "my-app",
          remoteURL: "https://example.invalid/my-app?signature=signed-query-value",
        },
      }),
    );
    expect(response.status).toBe(201);
    expect(storage.snapshot()).not.toContain("signed-query-value");
  });

  it("verifies and persists exact AWS AMI backing snapshots before publishing ownership", async () => {
    const storage = new CheckpointMemoryStorage();
    const provider = new AWSProvider(
      { FLEET: {} as DurableObjectNamespace, HETZNER_TOKEN: "" },
      "eu-west-1",
      storage,
    );
    const ownership: ProviderCheckpointOwnership = {
      checkpointID: "chk_aws_backing",
      tokenHash: "a".repeat(64),
      sourceLeaseID: leaseID,
    };
    const image = providerImage("aws", "prepared-workspace", "image", ownership);
    const createImage = vi.fn<() => Promise<ProviderImage>>(async () => ({
      ...image,
      snapshots: undefined,
    }));
    const getImage = vi.fn<(id: string) => Promise<ProviderImage>>(async (id) =>
      id === image.id
        ? image
        : {
            ...image,
            id,
            resourceID: id,
            immutableID: id,
            kind: "aws-ebs-snapshot",
            snapshots: [id],
          },
    );
    Reflect.set(provider, "clientValue", { createImage, getImage });
    const created = await provider.createCheckpointImage(
      checkpointLease("aws"),
      "prepared-workspace",
      true,
      "image",
      ownership,
      providerScope("aws"),
    );
    expect(getImage).toHaveBeenCalledWith(image.id);
    expect(created.snapshots).toEqual(["snap-backing-0001"]);
    expect(await storage.get(`image:aws:created:${image.id}`)).toBeUndefined();
    getImage.mockResolvedValueOnce({ ...image, snapshots: [] });
    await expect(
      provider.createCheckpointImage(
        checkpointLease("aws"),
        "prepared-workspace",
        true,
        "image",
        ownership,
        providerScope("aws"),
      ),
    ).rejects.toThrow(/backing snapshots could not be verified/);
    getImage
      .mockImplementationOnce(async () => image)
      .mockImplementationOnce(async (id) => ({
        ...image,
        id,
        resourceID: id,
        immutableID: id,
        checkpointOwnershipHash: "b".repeat(64),
      }));
    await expect(
      provider.createCheckpointImage(
        checkpointLease("aws"),
        "prepared-workspace",
        true,
        "image",
        ownership,
        providerScope("aws"),
      ),
    ).rejects.toThrow(/backing snapshot ownership/);
  });

  it("refuses to recover AWS AMIs without exactly owned backing snapshots", async () => {
    const storage = new CheckpointMemoryStorage();
    const provider = new AWSProvider(
      { FLEET: {} as DurableObjectNamespace, HETZNER_TOKEN: "" },
      "eu-west-1",
      storage,
    );
    const ownership: ProviderCheckpointOwnership = {
      checkpointID: "chk_aws_recovery_backing",
      tokenHash: "a".repeat(64),
      sourceLeaseID: leaseID,
    };
    const image = providerImage("aws", "recovery-image", "image", ownership);
    const findCheckpointImage = vi
      .fn<() => Promise<ProviderImage>>()
      .mockResolvedValue({ ...image, snapshots: [] });
    const getImage = vi.fn<() => Promise<ProviderImage>>().mockResolvedValue({
      ...image,
      accountID: "123456789012",
      checkpointOwnershipHash: "b".repeat(64),
    });
    Reflect.set(provider, "clientValue", {
      verifiedIdentity: async () => ({ account: "123456789012", region: "eu-west-1" }),
      findCheckpointImage,
      getImage,
    });
    const checkpoint = {
      id: ownership.checkpointID,
      provider: "aws",
      leaseID,
      strategy: "image",
      scope: providerScope("aws"),
      createClaim: { resourceName: "recovery-image", tokenHash: ownership.tokenHash },
    } as CoordinatorCheckpointRecord;
    await expect(provider.recoverCheckpointImage(checkpoint)).rejects.toThrow(/backing snapshots/);
    findCheckpointImage.mockResolvedValueOnce(image);
    await expect(provider.recoverCheckpointImage(checkpoint)).rejects.toThrow(
      /backing snapshot ownership/,
    );
  });

  it("hides checkpoints and source leases across owners and canonical organizations", async () => {
    const { coordinator } = await checkpointFixture();
    expect((await createCheckpoint(coordinator, "chk_private")).status).toBe(201);
    await Promise.all(
      [{ owner: "bob@example.com" }, { org: "another-org" }].map(async (options) => {
        const inspect = await coordinator.fetch(
          checkpointRequest("GET", "/v1/checkpoints/chk_private", undefined, options),
        );
        expect(inspect.status).toBe(404);
        const list = await coordinator.fetch(
          checkpointRequest("GET", "/v1/checkpoints", undefined, options),
        );
        await expect(list.json()).resolves.toEqual({ checkpoints: [] });
      }),
    );
    const admin = await coordinator.fetch(
      checkpointRequest("GET", "/v1/checkpoints/chk_private", undefined, {
        owner: "admin@example.com",
        org: "other-org",
        admin: true,
      }),
    );
    expect(admin.status).toBe(200);
  });

  it("indexes explicit unused expiry and recomputes it when policy changes", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage } = await checkpointFixture();
    expect(
      (
        await createCheckpoint(coordinator, "chk_expiring", {
          mode: "expire-unused",
          unusedForSeconds: 60,
        })
      ).status,
    ).toBe(201);
    const first = await storage.get<CoordinatorCheckpointRecord>(checkpointKey("chk_expiring"));
    expect(first?.nextSweepAt).toBe("2026-08-20T12:01:00.000Z");
    expect(await storage.get(checkpointDueKey("chk_expiring", first!.nextSweepAt!))).toMatchObject({
      checkpointID: "chk_expiring",
      revision: first!.revision,
    });
    const policy = await coordinator.fetch(
      checkpointRequest("PATCH", "/v1/checkpoints/chk_expiring/retention", {
        retention: { mode: "manual" },
      }),
    );
    expect(policy.status).toBe(200);
    expect(await storage.list({ prefix: "checkpoint-due:" })).toHaveLength(0);
    expect(
      storage.lists.some((entry) => entry.prefix === "checkpoint-due:" && entry.limit === 1),
    ).toBe(true);
  });

  it("serializes concurrent checkpoint alarm programming to retain the earliest deadline", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, runtime } = await checkpointFixture("azure");
    const responses = await Promise.all([
      createCheckpoint(coordinator, "chk_alarm_later", {
        mode: "expire-unused",
        unusedForSeconds: 600,
      }),
      createCheckpoint(coordinator, "chk_alarm_earlier", {
        mode: "expire-unused",
        unusedForSeconds: 60,
      }),
    ]);
    expect(responses.map((response) => response.status)).toEqual([201, 201]);
    expect(runtime.alarmTime).toBe(Date.parse("2026-08-20T12:01:00.000Z"));
  });

  it("hashes independent use claims, fences deletion, and advances use only on completion", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage, deleteImage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_claims", {
      mode: "expire-unused",
      unusedForSeconds: 300,
    });
    const firstResponse = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claims/use", { action: "begin" }),
    );
    const first = (await firstResponse.json()) as { claim: string };
    const secondResponse = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claims/use", { action: "begin" }),
    );
    const second = (await secondResponse.json()) as { claim: string };
    expect(first.claim).not.toBe(second.claim);
    expect(storage.snapshot()).not.toContain(first.claim);
    expect(storage.snapshot()).not.toContain(second.claim);
    expect(
      (await coordinator.fetch(checkpointRequest("DELETE", "/v1/checkpoints/chk_claims"))).status,
    ).toBe(409);
    expect(deleteImage).not.toHaveBeenCalled();
    vi.setSystemTime(new Date("2026-08-20T12:00:20.000Z"));
    const aborted = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claims/use", {
        action: "abort",
        claim: first.claim,
      }),
    );
    expect(aborted.status).toBe(200);
    expect(
      (await storage.get<CoordinatorCheckpointRecord>(checkpointKey("chk_claims")))?.lastUsedAt,
    ).toBe("2026-08-20T12:00:00.000Z");
    const renewed = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claims/use", {
        action: "renew",
        claim: second.claim,
      }),
    );
    expect(renewed.status).toBe(200);
    const attemptID = `cat_${"a".repeat(32)}`;
    await bindCheckpointUseProvisioning(
      storage,
      "chk_claims",
      second.claim,
      { owner: "alice@example.com", org },
      attemptID,
      "cbx_000000000002",
    );
    await storage.put("lease:cbx_000000000002", {
      ...checkpointLease("aws"),
      id: "cbx_000000000002",
      checkpointID: "chk_claims",
      createAttemptID: attemptID,
      state: "active",
    });
    await storage.put("create-attempt:cbx_000000000002", {
      token: attemptID,
      canonicalLeaseID: "cbx_000000000002",
      owner: "alice@example.com",
      org,
    });
    const completed = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claims/use", {
        action: "complete",
        claim: second.claim,
      }),
    );
    expect(completed.status).toBe(200);
    expect(await storage.get(checkpointKey("chk_claims"))).toMatchObject({
      activeUseCount: 0,
      lastUsedAt: "2026-08-20T12:00:20.000Z",
      nextSweepAt: "2026-08-20T12:05:20.000Z",
    });
  });

  it("rejects stale and cross-owner use claims without exposing or consuming them", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_stale");
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", "/v1/checkpoints/chk_stale/use", { action: "begin" }),
      )
    ).json()) as { claim: string };
    const stranger = await coordinator.fetch(
      checkpointRequest(
        "POST",
        "/v1/checkpoints/chk_stale/use",
        { action: "complete", claim: acquired.claim },
        { owner: "bob@example.com" },
      ),
    );
    expect(stranger.status).toBe(404);
    vi.setSystemTime(Date.now() + checkpointUseClaimTTLMS + 1);
    const expired = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_stale/use", {
        action: "complete",
        claim: acquired.claim,
      }),
    );
    expect(expired.status).toBe(409);
    expect(
      (await storage.get<CoordinatorCheckpointRecord>(checkpointKey("chk_stale")))?.lastUsedAt,
    ).toBe("2026-08-20T12:00:00.000Z");
  });

  it("rejects checkpoint-backed leases that select another resource without consuming the claim", async () => {
    const { coordinator, storage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_exact_selector");
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", "/v1/checkpoints/chk_exact_selector/use", {
          action: "begin",
        }),
      )
    ).json()) as { claim: string };
    const response = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        checkpointID: "chk_exact_selector",
        checkpointUseClaim: acquired.claim,
        awsSnapshot: "snap-unrelated-resource",
      }),
    );
    expect(response.status).toBe(409);
    await expect(response.json()).resolves.toMatchObject({
      error: "checkpoint_source_mismatch",
    });
    expect(await storage.get(checkpointKey("chk_exact_selector"))).toMatchObject({
      activeUseCount: 1,
    });
    expect(storage.snapshot()).not.toContain(acquired.claim);
  });

  it("pins promotions transactionally and suppresses expired wakeups until the matching pin retires", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage, deleteImage } = await checkpointFixture();
    await createCheckpoint(
      coordinator,
      "chk_pinned",
      { mode: "expire-unused", unusedForSeconds: 60 },
      "image",
    );
    const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey("chk_pinned")))!;
    const image = {
      id: record.image!.id,
      name: record.name,
      state: "available",
      provider: "aws" as const,
      region: record.scope.region,
      resourceID: record.image!.resourceID,
      snapshots: record.image!.snapshotIDs,
    };
    const catalogKey = "image:aws:catalog:linux:example:ami-000000000001";
    await storage.transaction(async (transaction) => {
      await transaction.put(catalogKey, image);
      await pinCheckpointPromotion(transaction, "aws", image, catalogKey);
    });
    expect(await storage.get(checkpointPinKey(record.id, catalogKey))).toBeDefined();
    expect(await storage.list({ prefix: "checkpoint-due:" })).toHaveLength(0);
    const blocked = await coordinator.fetch(
      checkpointRequest("DELETE", "/v1/checkpoints/chk_pinned"),
    );
    expect(blocked.status).toBe(409);
    await expect(blocked.json()).resolves.toMatchObject({ error: "checkpoint_pinned" });
    expect(deleteImage).not.toHaveBeenCalled();
    await storage.transaction(async (transaction) => {
      await transaction.delete(catalogKey);
      await unpinCheckpointPromotion(transaction, "aws", image, catalogKey);
    });
    expect(
      (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(record.id)))?.pinCount,
    ).toBe(0);
    expect(await storage.list({ prefix: "checkpoint-due:" })).toHaveLength(1);
  });

  it("pins every AWS default and catalog entry published through the image promotion route", async () => {
    const { coordinator, provider, storage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_promoted_route", { mode: "manual" }, "image");
    const record = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey("chk_promoted_route"),
    ))!;
    vi.spyOn(provider, "getImage").mockResolvedValue({
      ...providerImage("aws", record.name, "image", {
        checkpointID: record.id,
        tokenHash: "a".repeat(64),
        sourceLeaseID: record.leaseID,
      }),
      state: "available",
    });
    const promoted = await coordinator.fetch(
      checkpointRequest(
        "POST",
        `/v1/images/${record.image!.id}/promote?provider=aws&region=eu-west-1`,
        { target: "linux" },
        { admin: true },
      ),
    );
    expect(promoted.status).toBe(200);
    const pinned = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(record.id)))!;
    const pins = await storage.list({ prefix: `checkpoint-pin:${record.id}:` });
    expect(pinned.pinCount).toBe(pins.size);
    expect(pinned.pinCount).toBeGreaterThanOrEqual(2);
    const blocked = await coordinator.fetch(
      checkpointRequest("DELETE", `/v1/checkpoints/${record.id}`),
    );
    await expect(blocked.json()).resolves.toMatchObject({ error: "checkpoint_pinned" });
    const forbiddenRetirement = await coordinator.fetch(
      checkpointRequest(
        "DELETE",
        `/v1/images/${record.image!.id}/promote?provider=aws&region=eu-west-1`,
      ),
    );
    expect(forbiddenRetirement.status).toBe(403);
    await expect(forbiddenRetirement.json()).resolves.toMatchObject({ error: "forbidden" });
    expect(await storage.get(checkpointKey(record.id))).toMatchObject({
      pinCount: pinned.pinCount,
    });
    expect(await storage.list({ prefix: `checkpoint-pin:${record.id}:` })).toHaveLength(
      pinned.pinCount,
    );
    const retired = await coordinator.fetch(
      checkpointRequest(
        "DELETE",
        `/v1/images/${record.image!.id}/promote?provider=aws&region=eu-west-1`,
        undefined,
        { admin: true },
      ),
    );
    expect(retired.status).toBe(200);
    expect(await storage.get(checkpointKey(record.id))).toMatchObject({ pinCount: 0 });
    expect(await storage.list({ prefix: `checkpoint-pin:${record.id}:` })).toHaveLength(0);
    expect(
      (await coordinator.fetch(checkpointRequest("DELETE", `/v1/checkpoints/${record.id}`))).status,
    ).toBe(200);
  });

  it("pins Azure promoted snapshots and blocks checkpoint deletion without removing promotion metadata", async () => {
    const storage = new CheckpointMemoryStorage();
    const runtime = new CheckpointRuntime(storage);
    const env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_DEFAULT_ORG: "example-org",
      AZURE_TENANT_ID: "tenant",
      AZURE_CLIENT_ID: "client",
      AZURE_CLIENT_SECRET: "test-value",
      AZURE_SUBSCRIPTION_ID: "sub-123",
    } satisfies Env;
    const provider = new AzureProvider(env, undefined, storage, "westeurope");
    vi.spyOn(provider, "checkpointScope").mockResolvedValue(providerScope("azure"));
    vi.spyOn(provider, "createCheckpointImage").mockImplementation(
      async (_lease, name, _noReboot, strategy, ownership) => {
        const image = {
          ...providerImage("azure", name, strategy, ownership),
          serverType: "standard-small",
        };
        return image;
      },
    );
    const deleteImage = vi.spyOn(provider, "deleteCheckpointImage").mockResolvedValue(undefined);
    const coordinator = new FleetCoordinator(runtime, env, { azure: provider });
    await storage.put(`lease:${leaseID}`, checkpointLease("azure"));
    expect((await createCheckpoint(coordinator, "chk_azure_promoted")).status).toBe(201);
    const record = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey("chk_azure_promoted"),
    ))!;
    const promoted = await coordinator.fetch(
      checkpointRequest(
        "POST",
        `/v1/images/${encodeURIComponent(record.image!.id)}/promote?provider=azure&region=westeurope`,
        { target: "linux" },
        { admin: true },
      ),
    );
    expect(promoted.status).toBe(200);
    expect(await storage.get(checkpointKey(record.id))).toMatchObject({ pinCount: 1 });
    const catalog = await storage.list({ prefix: "image:azure:promoted:" });
    expect(catalog).toHaveLength(1);
    const blocked = await coordinator.fetch(
      checkpointRequest("DELETE", `/v1/checkpoints/${record.id}`),
    );
    expect(blocked.status).toBe(409);
    await expect(blocked.json()).resolves.toMatchObject({ error: "checkpoint_pinned" });
    expect(deleteImage).not.toHaveBeenCalled();
    expect(await storage.list({ prefix: "image:azure:promoted:" })).toHaveLength(1);
    const retired = await coordinator.fetch(
      checkpointRequest(
        "DELETE",
        `/v1/images/${encodeURIComponent(record.image!.resourceID)}/promote?provider=azure&region=westeurope`,
        undefined,
        { admin: true },
      ),
    );
    expect(retired.status).toBe(200);
    expect(await storage.get(checkpointKey(record.id))).toMatchObject({ pinCount: 0 });
    expect(await storage.list({ prefix: "image:azure:promoted:" })).toHaveLength(0);
    expect(
      (await coordinator.fetch(checkpointRequest("DELETE", `/v1/checkpoints/${record.id}`))).status,
    ).toBe(200);
  });

  it("blocks generic image deletion and backing-snapshot bypasses for managed resources", async () => {
    const { coordinator, storage, deleteImage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_fenced", { mode: "manual" }, "image");
    await Promise.all(
      ["ami-000000000001", "snap-backing-0001"].map(async (resource) => {
        const blocked = await coordinator.fetch(
          checkpointRequest(
            "DELETE",
            `/v1/images/${resource}?provider=aws&region=eu-west-1`,
            undefined,
            { admin: true },
          ),
        );
        expect(blocked.status).toBe(409);
        await expect(blocked.json()).resolves.toMatchObject({ error: "checkpoint_managed" });
      }),
    );
    expect(deleteImage).not.toHaveBeenCalled();
    expect(await storage.get(checkpointKey("chk_fenced"))).toBeDefined();
  });

  it.each(["azure", "gcp"] as const)(
    "blocks generic %s image deletion from bypassing managed checkpoint ownership",
    async (providerName) => {
      const { coordinator, storage, deleteImage } = await checkpointFixture(providerName);
      const checkpointID = `chk_${providerName}_fenced`;
      await createCheckpoint(coordinator, checkpointID);
      const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
      const query = new URLSearchParams({
        provider: providerName,
        region: record.scope.region,
        ...(record.scope.project ? { project: record.scope.project } : {}),
      });
      const response = await coordinator.fetch(
        checkpointRequest(
          "DELETE",
          `/v1/images/${encodeURIComponent(record.image!.id)}?${query}`,
          undefined,
          { admin: true },
        ),
      );
      expect(response.status).toBe(409);
      await expect(response.json()).resolves.toMatchObject({ error: "checkpoint_managed" });
      expect(deleteImage).not.toHaveBeenCalled();
    },
  );

  it("fences case-equivalent Azure names and ARM identifiers without matching foreign scopes", async () => {
    const { coordinator, provider, storage } = await checkpointFixture("azure");
    await createCheckpoint(coordinator, "chk_azure_case_fence");
    const record = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey("chk_azure_case_fence"),
    ))!;
    const deleteImage = vi.spyOn(provider, "deleteImage");
    const query = "provider=azure&region=westeurope";

    await Promise.all(
      [record.image!.id.toUpperCase(), record.image!.resourceID.toUpperCase()].map(
        async (resource) => {
          const blocked = await coordinator.fetch(
            checkpointRequest(
              "DELETE",
              `/v1/images/${encodeURIComponent(resource)}?${query}`,
              undefined,
              { admin: true },
            ),
          );
          expect(blocked.status).toBe(409);
          await expect(blocked.json()).resolves.toMatchObject({ error: "checkpoint_managed" });
        },
      ),
    );

    await Promise.all(
      [
        record.image!.resourceID.replace("sub-123", "foreign-subscription"),
        record.image!.resourceID.replace("checkpoint-rg", "foreign-group"),
        record.image!.resourceID.replace("/snapshots/", "/images/"),
      ].map(async (resource) => {
        const foreign = await coordinator.fetch(
          checkpointRequest(
            "DELETE",
            `/v1/images/${encodeURIComponent(resource)}?${query}`,
            undefined,
            { admin: true },
          ),
        );
        expect(foreign.status).toBe(409);
        await expect(foreign.json()).resolves.toMatchObject({ error: "image_not_owned" });
      }),
    );
    expect(deleteImage).not.toHaveBeenCalled();
  });

  it.each([
    ["disk-snapshot", "snapshots", "gcp-disk-snapshot"],
    ["image", "machineImages", "gcp-machine-image"],
  ] as const)(
    "fences every scoped GCP %s identifier alias while rejecting foreign projects and resource kinds",
    async (strategy, collection, kind) => {
      const { coordinator, provider, storage } = await checkpointFixture("gcp");
      const checkpointID = `chk_gcp_alias_${strategy.replace("-", "_")}`;
      await createCheckpoint(coordinator, checkpointID, { mode: "manual" }, strategy);
      const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
      const deleteImage = vi.spyOn(provider, "deleteImage");
      const relative = `projects/example-project/global/${collection}/${record.image!.id}`;
      const query = "provider=gcp&region=us-central1-a&project=example-project";

      await Promise.all(
        [
          record.image!.id,
          relative,
          `/${relative}`,
          `/compute/v1/${relative}`,
          `https://compute.googleapis.com/compute/v1/${relative}`,
          `https://www.googleapis.com/compute/v1/${relative}`,
        ].map(async (resource) => {
          const blocked = await coordinator.fetch(
            checkpointRequest(
              "DELETE",
              `/v1/images/${encodeURIComponent(resource)}?${query}`,
              undefined,
              { admin: true },
            ),
          );
          expect(blocked.status).toBe(409);
          await expect(blocked.json()).resolves.toMatchObject({ error: "checkpoint_managed" });
        }),
      );

      const wrongCollection = collection === "snapshots" ? "machineImages" : "snapshots";
      const wrongKind = kind === "gcp-disk-snapshot" ? "gcp-machine-image" : "gcp-disk-snapshot";
      const foreignRequests = [
        `${encodeURIComponent(relative.replace("example-project", "foreign-project"))}?${query}`,
        `${encodeURIComponent(relative.replace(`/${collection}/`, `/${wrongCollection}/`))}?${query}`,
        `${encodeURIComponent(`https://foreign.example/compute/v1/${relative}`)}?${query}`,
        `${encodeURIComponent(record.image!.id)}?${query}&kind=${wrongKind}`,
        `${encodeURIComponent(record.image!.id)}?provider=gcp&region=us-central1-a&project=foreign-project`,
      ];
      await Promise.all(
        foreignRequests.map(async (resource) => {
          const foreign = await coordinator.fetch(
            checkpointRequest("DELETE", `/v1/images/${resource}`, undefined, { admin: true }),
          );
          expect(foreign.status).toBe(409);
          await expect(foreign.json()).resolves.toMatchObject({ error: "image_not_owned" });
        }),
      );
      expect(deleteImage).not.toHaveBeenCalled();
    },
  );

  it("does not apply another GCP project's stored same-name image metadata to checkpoint matching", async () => {
    const { coordinator, provider, storage } = await checkpointFixture("gcp");
    await createCheckpoint(coordinator, "chk_gcp_foreign_metadata");
    const record = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey("chk_gcp_foreign_metadata"),
    ))!;
    vi.spyOn(provider, "storedImageMetadata").mockResolvedValue({
      id: record.image!.id,
      name: record.image!.id,
      state: "ready",
      provider: "gcp",
      kind: record.image!.kind,
      region: record.scope.region,
      project: record.scope.project,
      resourceID: record.image!.resourceID,
      immutableID: record.image!.immutableID,
    });
    const deleteImage = vi.spyOn(provider, "deleteImage");

    const foreign = await coordinator.fetch(
      checkpointRequest(
        "DELETE",
        `/v1/images/${encodeURIComponent(record.image!.id)}?provider=gcp&region=us-central1-a&project=foreign-project`,
        undefined,
        { admin: true },
      ),
    );
    expect(foreign.status).toBe(409);
    await expect(foreign.json()).resolves.toMatchObject({ error: "image_not_owned" });
    expect(deleteImage).not.toHaveBeenCalled();
  });

  it("fails closed when an unscoped GCP name matches checkpoints in multiple projects", async () => {
    const { coordinator, provider, storage } = await checkpointFixture("gcp");
    await createCheckpoint(coordinator, "chk_gcp_ambiguous_original");
    const original = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey("chk_gcp_ambiguous_original"),
    ))!;
    const foreign: CoordinatorCheckpointRecord = {
      ...original,
      id: "chk_gcp_ambiguous_foreign",
      scope: { ...original.scope, project: "foreign-project" },
      image: {
        ...original.image!,
        resourceID: `projects/foreign-project/global/snapshots/${original.image!.id}`,
        immutableID: "foreign-immutable-id",
      },
    };
    await storage.put(checkpointKey(foreign.id), foreign);
    await storage.put(
      checkpointResourceKey("gcp", foreign.scope, foreign.image!.kind, foreign.image!.resourceID),
      {
        checkpointID: foreign.id,
        provider: "gcp",
        scope: foreign.scope,
        kind: foreign.image!.kind,
        resourceID: foreign.image!.resourceID,
        immutableID: foreign.image!.immutableID,
      },
    );
    const deleteImage = vi.spyOn(provider, "deleteImage");

    await expect(
      findManagedCheckpointImage(storage, "gcp", { id: original.image!.id }),
    ).rejects.toMatchObject({ code: "checkpoint_source_mismatch" });
    const blocked = await coordinator.fetch(
      checkpointRequest(
        "DELETE",
        `/v1/images/${encodeURIComponent(original.image!.id)}?provider=gcp&region=us-central1-a`,
        undefined,
        { admin: true },
      ),
    );
    expect(blocked.status).toBe(500);
    expect(deleteImage).not.toHaveBeenCalled();
  });

  it.each(["azure", "gcp"] as const)(
    "rejects spoofed %s deletion scope without bypassing canonical ownership",
    async (providerName) => {
      const { coordinator, storage, deleteImage } = await checkpointFixture(providerName);
      const checkpointID = `chk_${providerName}_scope_spoof`;
      await createCheckpoint(coordinator, checkpointID);
      const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
      const queries = [
        new URLSearchParams({ provider: providerName, region: "wrong-provider-location" }),
        ...(providerName === "gcp"
          ? [
              new URLSearchParams({
                provider: providerName,
                region: record.scope.region,
                project: "foreign-project",
              }),
            ]
          : []),
      ];
      await Promise.all(
        queries.map(async (query) => {
          const response = await coordinator.fetch(
            checkpointRequest(
              "DELETE",
              `/v1/images/${encodeURIComponent(record.image!.resourceID)}?${query}`,
              undefined,
              { admin: true },
            ),
          );
          expect(response.status).toBe(409);
          await expect(response.json()).resolves.toMatchObject({
            error: "checkpoint_source_mismatch",
          });
        }),
      );
      expect(deleteImage).not.toHaveBeenCalled();
    },
  );

  it.each(["azure", "gcp"] as const)(
    "fences generic %s mutation before creation metadata is published",
    async (providerName) => {
      const { coordinator, provider, storage } = await checkpointFixture(providerName);
      let finish!: (image: ProviderImage) => void;
      let started!: () => void;
      const entered = new Promise<void>((resolve) => {
        started = resolve;
      });
      vi.mocked(provider.createCheckpointImage).mockImplementationOnce(
        async (_lease, name, _noReboot, strategy, ownership) => {
          started();
          return await new Promise<ProviderImage>((resolve) => {
            finish = () => resolve(providerImage(providerName, name, strategy, ownership));
          });
        },
      );
      const checkpointID = `chk_${providerName}_creating_fence`;
      const creating = createCheckpoint(coordinator, checkpointID);
      await entered;
      const pending = (await storage.get<CoordinatorCheckpointRecord>(
        checkpointKey(checkpointID),
      ))!;
      expect(await storage.list({ prefix: `checkpoint-intent:${providerName}:` })).toHaveLength(1);
      expect(await storage.list({ prefix: `image:${providerName}:created:` })).toHaveLength(0);
      const query = new URLSearchParams({
        provider: providerName,
        region: pending.scope.region,
        ...(pending.scope.project ? { project: pending.scope.project } : {}),
      });
      const name = pending.createClaim!.resourceName;
      const resources =
        providerName === "azure"
          ? [
              name,
              name.toUpperCase(),
              `/subscriptions/${pending.scope.subscriptionID}/resourceGroups/${pending.scope.resourceGroup}/providers/Microsoft.Compute/snapshots/${name}`.toUpperCase(),
            ]
          : [
              name,
              `projects/${pending.scope.project}/global/snapshots/${name}`,
              `https://compute.googleapis.com/compute/v1/projects/${pending.scope.project}/global/snapshots/${name}`,
            ];
      await Promise.all(
        resources.flatMap((resource) => {
          const path = `/v1/images/${encodeURIComponent(resource)}`;
          return [
            checkpointRequest("DELETE", `${path}?${query}`, undefined, { admin: true }),
            checkpointRequest(
              "POST",
              `${path}/promote?${query}`,
              { target: "linux" },
              { admin: true },
            ),
          ].map(async (request) => {
            const blocked = await coordinator.fetch(request);
            expect(blocked.status).toBe(409);
            await expect(blocked.json()).resolves.toMatchObject({ error: "checkpoint_pending" });
          });
        }),
      );
      finish({} as ProviderImage);
      expect((await creating).status).toBe(201);
      expect(await storage.list({ prefix: `image:${providerName}:created:` })).toHaveLength(2);
      expect(await storage.list({ prefix: `checkpoint-intent:${providerName}:` })).toHaveLength(0);
    },
  );

  it.each(["aws", "azure", "gcp"] as const)(
    "binds %s checkpoint leases to their exact provider-native scope",
    async (providerName) => {
      const { coordinator, storage } = await checkpointFixture(providerName);
      const checkpointID = `chk_${providerName}_bound_scope`;
      await createCheckpoint(coordinator, checkpointID);
      const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
      const acquired = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        )
      ).json()) as { claim: string };
      const createLease = vi
        .spyOn(coordinator as never, "createLease" as never)
        .mockImplementation(async (request: Request) => {
          const body = (await request.json()) as Record<string, unknown>;
          expect(body.awsRegion).toBe(providerName === "aws" ? record.scope.region : undefined);
          expect(body.azureLocation).toBe(
            providerName === "azure" ? record.scope.region : undefined,
          );
          expect(body.gcpZone).toBe(providerName === "gcp" ? record.scope.region : undefined);
          expect(body.gcpProject).toBe(providerName === "gcp" ? record.scope.project : undefined);
          return Response.json({ lease: { id: "cbx_000000000002" } }, { status: 201 });
        });
      const selector =
        providerName === "aws"
          ? { awsSnapshot: record.image!.id }
          : providerName === "azure"
            ? { azureSnapshot: record.image!.id }
            : { gcpSnapshot: record.image!.id };
      const response = await coordinator.fetch(
        checkpointRequest("POST", "/v1/leases/from-checkpoint", {
          provider: providerName,
          checkpointID,
          checkpointUseClaim: acquired.claim,
          leaseID: "cbx_000000000002",
          createAttemptID: `cat_${"a".repeat(32)}`,
          ...selector,
        }),
      );
      expect(response.status).toBe(201);
      expect(createLease).toHaveBeenCalledOnce();
      expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
    },
  );

  it("prevents duplicate provisioning and early external completion or abort", async () => {
    const { coordinator, storage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_provisioning_fence");
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", "/v1/checkpoints/chk_provisioning_fence/use", {
          action: "begin",
        }),
      )
    ).json()) as { claim: string };
    let settle!: () => void;
    let entered!: () => void;
    const started = new Promise<void>((resolve) => {
      entered = resolve;
    });
    const provision = vi
      .spyOn(coordinator as never, "createLease" as never)
      .mockImplementation(async () => {
        entered();
        return await new Promise<Response>((resolve) => {
          settle = () => resolve(Response.json({ lease: {} }, { status: 201 }));
        });
      });
    const body = {
      provider: "aws",
      awsSnapshot: "snap-000000000001",
      checkpointID: "chk_provisioning_fence",
      checkpointUseClaim: acquired.claim,
      leaseID: "cbx_000000000002",
      createAttemptID: `cat_${"b".repeat(32)}`,
    };
    const pending = coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", body),
    );
    await started;
    const blocked = await Promise.all([
      coordinator.fetch(checkpointRequest("POST", "/v1/leases/from-checkpoint", body)),
      coordinator.fetch(
        checkpointRequest("POST", "/v1/checkpoints/chk_provisioning_fence/use", {
          action: "complete",
          claim: acquired.claim,
        }),
      ),
      coordinator.fetch(
        checkpointRequest("POST", "/v1/checkpoints/chk_provisioning_fence/use", {
          action: "abort",
          claim: acquired.claim,
        }),
      ),
      coordinator.fetch(checkpointRequest("DELETE", "/v1/checkpoints/chk_provisioning_fence")),
    ]);
    expect(blocked.map((response) => response.status)).toEqual([409, 409, 409, 409]);
    expect(provision).toHaveBeenCalledOnce();
    expect(await storage.get(checkpointKey("chk_provisioning_fence"))).toMatchObject({
      activeUseCount: 1,
    });
    settle();
    expect((await pending).status).toBe(201);
    expect(await storage.get(checkpointKey("chk_provisioning_fence"))).toMatchObject({
      activeUseCount: 0,
    });
    expect(storage.snapshot()).not.toContain(acquired.claim);
  });

  it("replays a completed checkpoint-backed lease attempt without reusing its consumed claim", async () => {
    const { coordinator, storage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_completed_replay");
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", "/v1/checkpoints/chk_completed_replay/use", { action: "begin" }),
      )
    ).json()) as { claim: string };
    const createAttemptID = `cat_${"d".repeat(32)}`;
    const provision = vi
      .spyOn(coordinator as never, "createLease" as never)
      .mockImplementation(async () => {
        const lease: LeaseRecord = {
          ...checkpointLease("aws"),
          id: "cbx_000000000002",
          checkpointID: "chk_completed_replay",
          createAttemptID,
          createAttemptGeneration: "generation-one",
        };
        await storage.put(`lease:${lease.id}`, lease);
        await storage.put(`create-attempt:${lease.id}`, {
          version: 1,
          requestedLeaseID: lease.id,
          token: createAttemptID,
          owner: lease.owner,
          org: lease.org,
          state: "pending",
          canonicalLeaseID: lease.id,
          cloudID: lease.cloudID,
          generation: lease.createAttemptGeneration,
          createdAt: lease.createdAt,
          updatedAt: lease.updatedAt,
        });
        return Response.json({ lease }, { status: 201 });
      });
    const body = {
      provider: "aws",
      awsSnapshot: "snap-000000000001",
      checkpointID: "chk_completed_replay",
      checkpointUseClaim: acquired.claim,
      leaseID: "cbx_000000000002",
      createAttemptID,
    };
    expect(
      (await coordinator.fetch(checkpointRequest("POST", "/v1/leases/from-checkpoint", body)))
        .status,
    ).toBe(201);
    const replayed = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", body),
    );
    expect(replayed.ok).toBe(true);
    expect(provision).toHaveBeenCalledOnce();
    expect(storage.snapshot()).not.toContain(acquired.claim);
  });

  it("keeps an explicit administrator as the principal throughout checkpoint-backed provisioning", async () => {
    const { coordinator } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_admin_provision");
    const admin = { owner: "operator@example.com", admin: true };
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest(
          "POST",
          "/v1/checkpoints/chk_admin_provision/use",
          { action: "begin" },
          admin,
        ),
      )
    ).json()) as { claim: string };
    vi.spyOn(coordinator as never, "createLease" as never).mockImplementation(
      async (request: Request) => {
        expect(request.headers.get("x-crabbox-owner")).toBe("operator@example.com");
        expect(request.headers.get("x-crabbox-admin")).toBe("true");
        return Response.json({ lease: {} }, { status: 201 });
      },
    );
    const response = await coordinator.fetch(
      checkpointRequest(
        "POST",
        "/v1/leases/from-checkpoint",
        {
          provider: "aws",
          awsSnapshot: "snap-000000000001",
          checkpointID: "chk_admin_provision",
          checkpointUseClaim: acquired.claim,
          leaseID: "cbx_000000000002",
          createAttemptID: `cat_${"e".repeat(32)}`,
        },
        admin,
      ),
    );
    expect(response.status).toBe(201);
  });

  it("bounds failed creation recovery and permits exact no-resource cancellation", async () => {
    const { coordinator, provider, storage } = await checkpointFixture("azure");
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(new Error("uncertain create"));
    expect((await createCheckpoint(coordinator, "chk_failed_creation")).status).toBe(503);
    await Array.from({ length: checkpointMaxCreateRecoveryAttempts }).reduce(async (previous) => {
      await previous;
      await recordCheckpointCreateRecoveryFailure(storage, "chk_failed_creation", "still absent");
    }, Promise.resolve());
    expect(await storage.get(checkpointKey("chk_failed_creation"))).toMatchObject({
      state: "failed",
    });
    expect(await storage.list({ prefix: "checkpoint-due:" })).toHaveLength(0);
    vi.spyOn(provider, "recoverCheckpointImage").mockResolvedValue(undefined);
    const canceled = await coordinator.fetch(
      checkpointRequest("DELETE", "/v1/checkpoints/chk_failed_creation"),
    );
    expect(canceled.status).toBe(200);
    expect(await storage.get(checkpointKey("chk_failed_creation"))).toMatchObject({
      state: "deleted",
    });
    expect(await storage.list({ prefix: "checkpoint-intent:azure:" })).toHaveLength(0);
  });

  it.each(["azure", "gcp"] as const)(
    "preserves a unique checkpoint-derived suffix for long %s resource names",
    async (providerName) => {
      const { coordinator, provider } = await checkpointFixture(providerName);
      const names: string[] = [];
      vi.mocked(provider.createCheckpointImage).mockImplementation(
        async (_lease, name, _noReboot, strategy, ownership) => {
          names.push(name);
          return providerImage(providerName, name, strategy, ownership);
        },
      );
      await Promise.all(
        ["chk_name_collision_first", "chk_name_collision_second"].map(
          async (id) =>
            await coordinator.fetch(
              checkpointRequest("POST", "/v1/checkpoints", {
                id,
                leaseID,
                name: "very-long-provider-resource-name-".repeat(3),
                strategy: "disk-snapshot",
              }),
            ),
        ),
      );
      expect(names).toHaveLength(2);
      expect(new Set(names).size).toBe(2);
      expect(names.every((name) => name.length <= (providerName === "azure" ? 80 : 63))).toBe(true);
    },
  );

  it.each(["azure", "gcp"] as const)(
    "rejects fabricated %s ownership evidence before publishing aliases",
    async (providerName) => {
      const { coordinator, provider, storage } = await checkpointFixture(providerName);
      vi.mocked(provider.createCheckpointImage).mockImplementationOnce(
        async (_lease, name, _noReboot, strategy, ownership) => ({
          ...providerImage(providerName, name, strategy, ownership),
          checkpointOwnershipHash: "b".repeat(64),
        }),
      );
      const checkpointID = `chk_${providerName}_fabricated_ownership`;
      expect((await createCheckpoint(coordinator, checkpointID)).status).toBe(503);
      expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ state: "creating" });
      expect(await storage.list({ prefix: `image:${providerName}:created:` })).toHaveLength(0);
      expect(await storage.list({ prefix: `checkpoint-intent:${providerName}:` })).toHaveLength(1);
    },
  );

  it("resolves identically named Azure snapshots by exact subscription and resource group", async () => {
    const { coordinator, storage } = await checkpointFixture("azure");
    await createCheckpoint(coordinator, "chk_azure_original_scope");
    const original = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey("chk_azure_original_scope"),
    ))!;
    const foreign: CoordinatorCheckpointRecord = {
      ...original,
      id: "chk_azure_foreign_scope",
      scope: {
        ...original.scope,
        subscriptionID: "foreign-subscription",
        resourceGroup: "foreign-group",
      },
      image: {
        ...original.image!,
        resourceID: `/subscriptions/foreign-subscription/resourceGroups/foreign-group/providers/Microsoft.Compute/snapshots/${original.image!.id}`,
        immutableID: "foreign-immutable-id",
      },
    };
    await storage.put(checkpointKey(foreign.id), foreign);
    await storage.put(
      checkpointResourceKey("azure", foreign.scope, foreign.image!.kind, foreign.image!.resourceID),
      {
        checkpointID: foreign.id,
        provider: "azure",
        scope: foreign.scope,
        kind: foreign.image!.kind,
        resourceID: foreign.image!.resourceID,
        immutableID: foreign.image!.immutableID,
      },
    );
    const matched = await findManagedCheckpointImage(storage, "azure", {
      id: original.image!.id,
      name: original.name,
      state: "available",
      provider: "azure",
      kind: original.image!.kind,
      resourceID: original.image!.resourceID,
      immutableID: original.image!.immutableID,
    });
    expect(matched?.id).toBe(original.id);
  });

  it("marks definitive pre-mutation refusal terminal without retrying blindly", async () => {
    const { coordinator, provider, storage } = await checkpointFixture("azure");
    const refusal = Object.assign(new Error("snapshot already exists"), {
      checkpointResourceMayExist: false,
    });
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(refusal);
    expect((await createCheckpoint(coordinator, "chk_definitive_refusal")).status).toBe(503);
    expect(await storage.get(checkpointKey("chk_definitive_refusal"))).toMatchObject({
      state: "failed",
      createClaim: { definitiveRefusal: true },
    });
    expect(await storage.list({ prefix: "checkpoint-due:" })).toHaveLength(0);
    const recovery = vi.spyOn(provider, "recoverCheckpointImage");
    const canceled = await coordinator.fetch(
      checkpointRequest("DELETE", "/v1/checkpoints/chk_definitive_refusal"),
    );
    expect(canceled.status).toBe(200);
    expect(recovery).not.toHaveBeenCalled();
    expect(await storage.get(checkpointKey("chk_definitive_refusal"))).toMatchObject({
      state: "deleted",
    });
  });

  it("releases an expired provisioning claim when its exact lease never existed", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_cancel_provision");
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", "/v1/checkpoints/chk_cancel_provision/use", {
          action: "begin",
        }),
      )
    ).json()) as { claim: string };
    const createAttemptID = `cat_${"c".repeat(32)}`;
    const createLease = vi
      .spyOn(coordinator as never, "createLease" as never)
      .mockResolvedValue(Response.json({ error: "provider_pending" }, { status: 503 }));
    await storage.put("create-attempt:cbx_000000000002", {
      version: 1,
      requestedLeaseID: "cbx_000000000002",
      token: createAttemptID,
      owner: "alice@example.com",
      org,
      state: "pending",
      canonicalLeaseID: "cbx_000000000002",
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    });
    const response = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID: "chk_cancel_provision",
        checkpointUseClaim: acquired.claim,
        leaseID: "cbx_000000000002",
        createAttemptID,
      }),
    );
    expect(response.status).toBe(503);
    expect(createLease).toHaveBeenCalledOnce();
    vi.setSystemTime(Date.now() + checkpointUseClaimTTLMS + 1);
    await coordinator.alarm();
    expect(await storage.get(checkpointKey("chk_cancel_provision"))).toMatchObject({
      activeUseCount: 0,
    });
    expect(
      (await coordinator.fetch(checkpointRequest("DELETE", "/v1/checkpoints/chk_cancel_provision")))
        .status,
    ).toBe(200);
  });

  it("retains sanitized retry state after provider failure and finalizes only after successful retry", async () => {
    const { coordinator, storage, deleteImage } = await checkpointFixture("gcp");
    await createCheckpoint(coordinator, "chk_retry");
    deleteImage.mockRejectedValueOnce(new Error("Authorization: Bearer hidden-provider-token"));
    const failed = await coordinator.fetch(
      checkpointRequest("DELETE", "/v1/checkpoints/chk_retry"),
    );
    expect(failed.status).toBe(503);
    const retrying = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey("chk_retry")))!;
    expect(retrying.state).toBe("delete-pending");
    expect(retrying.attempts).toBe(1);
    expect(retrying.retryAt).toBeDefined();
    expect(retrying.lastError).not.toContain("hidden-provider-token");
    expect(await storage.list({ prefix: "checkpoint-resource:gcp:" })).not.toHaveLength(0);
    const retried = await coordinator.fetch(
      checkpointRequest("DELETE", "/v1/checkpoints/chk_retry"),
    );
    expect(retried.status).toBe(200);
    expect(await storage.get(checkpointKey("chk_retry"))).toMatchObject({ state: "deleted" });
    expect(await storage.list({ prefix: "checkpoint-resource:gcp:" })).toHaveLength(0);
    expect(await storage.list({ prefix: "image:gcp:created:" })).toHaveLength(0);
    const events = await coordinator.fetch(
      checkpointRequest("GET", "/v1/checkpoints/chk_retry/events"),
    );
    const history = (await events.json()) as { events: Array<{ type: string }> };
    expect(history.events.map((event) => event.type)).toEqual(
      expect.arrayContaining([
        "checkpoint.delete.claimed",
        "checkpoint.delete.failed",
        "checkpoint.delete.provider_completed",
        "checkpoint.deleted",
      ]),
    );
  });

  it("expires opted-in checkpoints through bounded indexed scheduler maintenance", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage, runtime, deleteImage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_scheduler_expiry", {
      mode: "expire-unused",
      unusedForSeconds: 60,
    });
    expect(runtime.alarmTime).toBe(Date.parse("2026-08-20T12:01:00.000Z"));
    vi.setSystemTime(new Date("2026-08-20T12:01:01.000Z"));
    await coordinator.alarm();
    expect(deleteImage).toHaveBeenCalledTimes(1);
    expect(await storage.get(checkpointKey("chk_scheduler_expiry"))).toMatchObject({
      state: "deleted",
    });
    const events = await coordinator.fetch(
      checkpointRequest("GET", "/v1/checkpoints/chk_scheduler_expiry/events"),
    );
    const history = (await events.json()) as { events: Array<{ type: string }> };
    expect(history.events.map((event) => event.type)).toContain("checkpoint.expiry.due");
    expect(
      storage.lists.some((entry) => entry.prefix === "checkpoint-due:" && entry.limit === 32),
    ).toBe(true);
  });

  it("recovers ambiguous provider creation using only its exact durable ownership claim", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, provider, storage } = await checkpointFixture("gcp");
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(
      new Error("Authorization: Bearer ambiguous-provider-value"),
    );
    const response = await createCheckpoint(coordinator, "chk_create_recovery");
    expect(response.status).toBe(503);
    const pending = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey("chk_create_recovery"),
    ))!;
    expect(pending.state).toBe("creating");
    expect(pending.lastError).not.toContain("ambiguous-provider-value");
    vi.spyOn(provider, "recoverCheckpointImage").mockImplementationOnce(async (checkpoint) =>
      providerImage("gcp", checkpoint.createClaim!.resourceName, "disk-snapshot", {
        checkpointID: checkpoint.id,
        tokenHash: checkpoint.createClaim!.tokenHash,
        sourceLeaseID: checkpoint.leaseID,
      }),
    );
    vi.setSystemTime(Date.parse(pending.retryAt!) + 1);
    await coordinator.alarm();
    expect(await storage.get(checkpointKey("chk_create_recovery"))).toMatchObject({
      state: "ready",
      provider: "gcp",
      image: { immutableID: "987654321012345678" },
    });
    expect(await storage.list({ prefix: "image:gcp:created:" })).toHaveLength(2);
  });

  it("restores both exact Azure aliases on recovery before allowing promotion", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const storage = new CheckpointMemoryStorage();
    const runtime = new CheckpointRuntime(storage);
    const env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_DEFAULT_ORG: "example-org",
      AZURE_TENANT_ID: "tenant",
      AZURE_CLIENT_ID: "client",
      AZURE_CLIENT_SECRET: "test-value",
      AZURE_SUBSCRIPTION_ID: "sub-123",
    } satisfies Env;
    const provider = new AzureProvider(env, undefined, storage, "westeurope");
    vi.spyOn(provider, "checkpointScope").mockResolvedValue(providerScope("azure"));
    vi.spyOn(provider, "createCheckpointImage").mockRejectedValueOnce(
      new Error("ambiguous Azure creation"),
    );
    vi.spyOn(provider, "recoverCheckpointImage").mockImplementation(async (checkpoint) => ({
      ...providerImage("azure", checkpoint.createClaim!.resourceName, "disk-snapshot", {
        checkpointID: checkpoint.id,
        tokenHash: checkpoint.createClaim!.tokenHash,
        sourceLeaseID: checkpoint.leaseID,
      }),
      serverType: "standard-small",
    }));
    const coordinator = new FleetCoordinator(runtime, env, { azure: provider });
    await storage.put(`lease:${leaseID}`, checkpointLease("azure"));
    expect((await createCheckpoint(coordinator, "chk_azure_recovered_promote")).status).toBe(503);
    const pending = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey("chk_azure_recovered_promote"),
    ))!;
    vi.setSystemTime(Date.parse(pending.retryAt!) + 1);
    await coordinator.alarm();
    const recovered = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(pending.id)))!;
    expect(await storage.list({ prefix: "image:azure:created:" })).toHaveLength(2);
    const promoted = await coordinator.fetch(
      checkpointRequest(
        "POST",
        `/v1/images/${encodeURIComponent(recovered.image!.id)}/promote?provider=azure&region=westeurope`,
        { target: "linux" },
        { admin: true },
      ),
    );
    expect(promoted.status).toBe(200);
    expect(await storage.get(checkpointKey(pending.id))).toMatchObject({ pinCount: 1 });
  });

  it("reclaims expired independent fork claims without advancing last use", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_scheduler_claim");
    await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_scheduler_claim/use", {
        action: "begin",
      }),
    );
    vi.setSystemTime(Date.now() + checkpointUseClaimTTLMS + 1);
    await coordinator.alarm();
    expect(await storage.get(checkpointKey("chk_scheduler_claim"))).toMatchObject({
      state: "ready",
      activeUseCount: 0,
      lastUsedAt: "2026-08-20T12:00:00.000Z",
    });
    expect(await storage.list({ prefix: "checkpoint-use:chk_scheduler_claim:" })).toHaveLength(0);
    expect(await storage.list({ prefix: "checkpoint-due:" })).toHaveLength(0);
  });

  it("recovers durable provider-deleted phases without repeating irreversible deletion", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage, deleteImage } = await checkpointFixture("azure");
    await createCheckpoint(coordinator, "chk_provider_deleted");
    const claimed = await claimCheckpointDeletion(
      storage,
      "chk_provider_deleted",
      { owner: "alice@example.com", org },
      "manual",
    );
    await markCheckpointProviderDeleted(storage, claimed.checkpoint.id, claimed.token);
    expect(storage.snapshot()).not.toContain(claimed.token);
    vi.setSystemTime(Date.now() + checkpointDeleteClaimTTLMS + 1);
    await coordinator.alarm();
    expect(deleteImage).not.toHaveBeenCalled();
    expect(await storage.get(checkpointKey("chk_provider_deleted"))).toMatchObject({
      state: "deleted",
    });
    expect(await storage.list({ prefix: "image:azure:created:" })).toHaveLength(0);
  });

  it("rolls back authoritative records, due markers, and audit events together", async () => {
    const { coordinator, provider, storage } = await checkpointFixture();
    storage.failKey = "checkpoint-event:chk_atomic:000000000001";
    const response = await createCheckpoint(coordinator, "chk_atomic");
    expect(response.status).toBe(500);
    expect(await storage.get(checkpointKey("chk_atomic"))).toBeUndefined();
    expect(await storage.list({ prefix: "checkpoint-due:" })).toHaveLength(0);
    expect(provider.createCheckpointImage).not.toHaveBeenCalled();
  });

  it("keeps concurrent deletion and use mutually fenced", async () => {
    const { coordinator, deleteImage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_race");
    const [used, deleted] = await Promise.all([
      coordinator.fetch(
        checkpointRequest("POST", "/v1/checkpoints/chk_race/use", { action: "begin" }),
      ),
      coordinator.fetch(checkpointRequest("DELETE", "/v1/checkpoints/chk_race")),
    ]);
    expect(
      (used.status === 201 && deleted.status === 409) ||
        (used.status === 409 && deleted.status === 200),
    ).toBe(true);
    expect(deleteImage).toHaveBeenCalledTimes(deleted.status === 200 ? 1 : 0);
  });

  it("rejects fabricated external completion of an unbound use claim", async () => {
    const { coordinator, storage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_unbound_completion");
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", "/v1/checkpoints/chk_unbound_completion/use", {
          action: "begin",
        }),
      )
    ).json()) as { claim: string };
    const completed = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_unbound_completion/use", {
        action: "complete",
        claim: acquired.claim,
      }),
    );
    expect(completed.status).toBe(409);
    await expect(completed.json()).resolves.toMatchObject({ error: "checkpoint_claim_invalid" });
    expect(await storage.get(checkpointKey("chk_unbound_completion"))).toMatchObject({
      activeUseCount: 1,
    });
  });

  it("reconciles a successful durable lease after restart and replays the exact attempt", async () => {
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_restart_success";
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    const createAttemptID = `cat_${"9".repeat(32)}`;
    const forkLeaseID = "cbx_000000000002";
    await bindCheckpointUseProvisioning(
      storage,
      checkpointID,
      acquired.claim,
      { owner: "alice@example.com", org },
      createAttemptID,
      forkLeaseID,
    );
    const lease = {
      ...checkpointLease("aws"),
      id: forkLeaseID,
      checkpointID,
      createAttemptID,
      createAttemptGeneration: "restart-generation",
    };
    await storage.put(`lease:${forkLeaseID}`, lease);
    await storage.put(`create-attempt:${forkLeaseID}`, {
      version: 1,
      requestedLeaseID: forkLeaseID,
      token: createAttemptID,
      owner: lease.owner,
      org: lease.org,
      state: "pending",
      canonicalLeaseID: forkLeaseID,
      cloudID: lease.cloudID,
      generation: lease.createAttemptGeneration,
      createdAt: lease.createdAt,
      updatedAt: lease.updatedAt,
    });
    const createLease = vi.spyOn(coordinator as never, "createLease" as never);
    const replay = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: forkLeaseID,
        createAttemptID,
      }),
    );
    expect(replay.ok).toBe(true);
    expect(createLease).not.toHaveBeenCalled();
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
  });

  it.each(["failed", "released", "expired"] as const)(
    "reconciles a terminal %s provisioning lease without renewing its claim indefinitely",
    async (state) => {
      vi.useFakeTimers();
      vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
      const { coordinator, storage } = await checkpointFixture();
      const checkpointID = `chk_terminal_${state}`;
      await createCheckpoint(coordinator, checkpointID);
      const acquired = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        )
      ).json()) as { claim: string };
      await bindCheckpointUseProvisioning(
        storage,
        checkpointID,
        acquired.claim,
        { owner: "alice@example.com", org },
        `cat_${"8".repeat(32)}`,
        "cbx_000000000002",
      );
      await storage.put("lease:cbx_000000000002", {
        ...checkpointLease("aws"),
        id: "cbx_000000000002",
        checkpointID,
        createAttemptID: `cat_${"8".repeat(32)}`,
        state,
      });
      vi.setSystemTime(Date.now() + checkpointUseClaimTTLMS + 1);
      await coordinator.alarm();
      expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
      expect(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).toHaveLength(0);
    },
  );

  it("recovers an exact owned resource from terminal creation failure and deletes it", async () => {
    const { coordinator, provider, storage, deleteImage } = await checkpointFixture("azure");
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(new Error("ambiguous"));
    const checkpointID = "chk_failed_owned_recovery";
    await createCheckpoint(coordinator, checkpointID);
    await Array.from({ length: checkpointMaxCreateRecoveryAttempts }).reduce(async (previous) => {
      await previous;
      await recordCheckpointCreateRecoveryFailure(storage, checkpointID, "ambiguous");
    }, Promise.resolve());
    vi.spyOn(provider, "recoverCheckpointImage").mockImplementationOnce(async (checkpoint) =>
      providerImage("azure", checkpoint.createClaim!.resourceName, "disk-snapshot", {
        checkpointID,
        tokenHash: checkpoint.createClaim!.tokenHash,
        sourceLeaseID: leaseID,
      }),
    );
    const deleted = await coordinator.fetch(
      checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`),
    );
    expect(deleted.status).toBe(200);
    expect(deleteImage).toHaveBeenCalledOnce();
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ state: "deleted" });
  });

  it("retains failed durable ownership when provider recovery detects a foreign resource", async () => {
    const { coordinator, provider, storage, deleteImage } = await checkpointFixture("gcp");
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(new Error("ambiguous"));
    const checkpointID = "chk_failed_foreign_resource";
    await createCheckpoint(coordinator, checkpointID);
    await Array.from({ length: checkpointMaxCreateRecoveryAttempts }).reduce(async (previous) => {
      await previous;
      await recordCheckpointCreateRecoveryFailure(storage, checkpointID, "ambiguous");
    }, Promise.resolve());
    vi.spyOn(provider, "recoverCheckpointImage").mockRejectedValueOnce(
      new CheckpointError(
        "checkpoint_source_mismatch",
        "provider resource has conflicting ownership",
      ),
    );
    const deleted = await coordinator.fetch(
      checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`),
    );
    expect(deleted.status).toBe(409);
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ state: "failed" });
    expect(await storage.list({ prefix: "checkpoint-intent:gcp:" })).toHaveLength(1);
    expect(deleteImage).not.toHaveBeenCalled();
  });

  it.each(["azure", "gcp"] as const)(
    "never treats an existing %s resource with mismatched ownership as absent",
    async (providerName) => {
      const env = { FLEET: {} as DurableObjectNamespace, HETZNER_TOKEN: "" } satisfies Env;
      const provider =
        providerName === "azure"
          ? new AzureProvider(env)
          : new GCPProvider(env, undefined, "us-central1-a", "example-project");
      const ownership: ProviderCheckpointOwnership = {
        checkpointID: `chk_${providerName}_foreign_tags`,
        tokenHash: "a".repeat(64),
        sourceLeaseID: leaseID,
      };
      const image = {
        ...providerImage(providerName, "foreign-snapshot", "disk-snapshot", ownership),
        checkpointOwnershipHash: "b".repeat(64),
      };
      Reflect.set(provider, "clientValue", { getImage: async () => image });
      const checkpoint = {
        id: ownership.checkpointID,
        leaseID,
        provider: providerName,
        strategy: "disk-snapshot",
        scope: providerScope(providerName),
        createClaim: { resourceName: "foreign-snapshot", tokenHash: ownership.tokenHash },
      } as CoordinatorCheckpointRecord;
      await expect(provider.recoverCheckpointImage(checkpoint)).rejects.toMatchObject({
        code: "checkpoint_source_mismatch",
      });
    },
  );

  it("fences pending AWS AMI promotion by freshly verified checkpoint ownership tags", async () => {
    const { coordinator, provider, storage } = await checkpointFixture("aws");
    let finish!: () => void;
    let started!: () => void;
    const entered = new Promise<void>((resolve) => {
      started = resolve;
    });
    vi.mocked(provider.createCheckpointImage).mockImplementationOnce(
      async (_lease, name, _noReboot, strategy, ownership) => {
        vi.spyOn(provider, "getImage").mockResolvedValue(
          providerImage("aws", name, strategy, ownership),
        );
        started();
        return await new Promise<ProviderImage>((resolve) => {
          finish = () => resolve(providerImage("aws", name, strategy, ownership));
        });
      },
    );
    const creating = createCheckpoint(
      coordinator,
      "chk_pending_aws_ami",
      { mode: "manual" },
      "image",
    );
    await entered;
    const blocked = await coordinator.fetch(
      checkpointRequest(
        "POST",
        "/v1/images/ami-000000000001/promote?provider=aws&region=eu-west-1",
        { target: "linux" },
        { admin: true },
      ),
    );
    expect(blocked.status).toBe(409);
    await expect(blocked.json()).resolves.toMatchObject({ error: "checkpoint_pending" });
    expect(await storage.list({ prefix: "image:aws:catalog:" })).toHaveLength(0);
    finish();
    expect((await creating).status).toBe(201);
  });

  it.each([
    ["disk-snapshot", "snap-000000000001"],
    ["image", "snap-backing-0001"],
  ] as const)(
    "fences generic AWS snapshot deletion during pending %s creation using fresh ownership tags",
    async (strategy, snapshotID) => {
      const { coordinator, provider, storage } = await checkpointFixture("aws");
      const deleteImage = vi.spyOn(provider, "deleteImage");
      let finish!: () => void;
      let started!: () => void;
      const entered = new Promise<void>((resolve) => {
        started = resolve;
      });
      vi.mocked(provider.createCheckpointImage).mockImplementationOnce(
        async (_lease, name, _noReboot, requestedStrategy, ownership) => {
          const image = providerImage("aws", name, requestedStrategy, ownership);
          vi.spyOn(provider, "getImage").mockResolvedValue(
            strategy === "image"
              ? {
                  id: snapshotID,
                  name: snapshotID,
                  state: "pending",
                  provider: "aws",
                  kind: "aws-ebs-snapshot",
                  region: "eu-west-1",
                  accountID: "123456789012",
                  resourceID: snapshotID,
                  immutableID: snapshotID,
                  checkpointOwnershipHash: ownership.tokenHash,
                  checkpointSourceLeaseID: ownership.sourceLeaseID,
                  snapshots: [snapshotID],
                }
              : image,
          );
          started();
          return await new Promise<ProviderImage>((resolve) => {
            finish = () => resolve(image);
          });
        },
      );
      const checkpointID = `chk_pending_aws_${strategy.replace("-", "_")}_snapshot`;
      const creating = createCheckpoint(coordinator, checkpointID, { mode: "manual" }, strategy);
      await entered;

      const blocked = await coordinator.fetch(
        checkpointRequest(
          "DELETE",
          `/v1/images/${snapshotID}?provider=aws&region=eu-west-1`,
          undefined,
          { admin: true },
        ),
      );
      expect(blocked.status).toBe(409);
      await expect(blocked.json()).resolves.toMatchObject({ error: "checkpoint_pending" });
      expect(provider.getImage).toHaveBeenCalledWith(snapshotID, undefined);
      expect(deleteImage).not.toHaveBeenCalled();
      expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ state: "creating" });

      finish();
      expect((await creating).status).toBe(201);
    },
  );

  it.each(["disk-snapshot", "image"] as const)(
    "does not fence unrelated AWS snapshots with missing or mismatched pending %s ownership evidence",
    async (strategy) => {
      const { coordinator, provider, storage } = await checkpointFixture("aws");
      const deleteImage = vi.spyOn(provider, "deleteImage").mockResolvedValue(undefined);
      let finish!: () => void;
      let started!: () => void;
      const entered = new Promise<void>((resolve) => {
        started = resolve;
      });
      let ownership!: ProviderCheckpointOwnership;
      let resourceName!: string;
      vi.mocked(provider.createCheckpointImage).mockImplementationOnce(
        async (_lease, name, _noReboot, requestedStrategy, checkpointOwnership) => {
          ownership = checkpointOwnership;
          resourceName = name;
          started();
          return await new Promise<ProviderImage>((resolve) => {
            finish = () =>
              resolve(providerImage("aws", name, requestedStrategy, checkpointOwnership));
          });
        },
      );
      const creating = createCheckpoint(
        coordinator,
        `chk_pending_aws_${strategy.replace("-", "_")}_foreign`,
        { mode: "manual" },
        strategy,
      );
      await entered;

      const foreignEvidence: Array<Partial<ProviderImage>> = [
        { checkpointOwnershipHash: "f".repeat(64) },
        { checkpointSourceLeaseID: "cbx_foreign_lease" },
        { accountID: "999999999999" },
        { region: "us-east-1" },
        { checkpointOwnershipHash: undefined },
        { checkpointSourceLeaseID: undefined },
        ...(strategy === "disk-snapshot" ? [{ name: "foreign-snapshot-name" }] : []),
      ];
      const observed = new Map<string, ProviderImage>();
      vi.spyOn(provider, "getImage").mockImplementation(async (imageID) => observed.get(imageID)!);
      await Promise.all(
        foreignEvidence.map(async (evidence, index) => {
          const snapshotID = `snap-foreign-${index}`;
          const metadata: ProviderImage = {
            id: snapshotID,
            name: "independent-image",
            state: "available",
            provider: "aws",
            kind: "aws-ebs-snapshot",
            region: "eu-west-1",
            resourceID: snapshotID,
            immutableID: snapshotID,
          };
          await storage.put(`image:aws:created:${encodeURIComponent(snapshotID)}`, metadata);
          observed.set(snapshotID, {
            ...metadata,
            name: strategy === "disk-snapshot" ? resourceName : snapshotID,
            accountID: "123456789012",
            checkpointOwnershipHash: ownership.tokenHash,
            checkpointSourceLeaseID: ownership.sourceLeaseID,
            snapshots: [snapshotID],
            ...evidence,
          });
          const unrelated = await coordinator.fetch(
            checkpointRequest(
              "DELETE",
              `/v1/images/${snapshotID}?provider=aws&region=eu-west-1`,
              undefined,
              { admin: true },
            ),
          );
          expect(unrelated.status).toBe(200);
          await expect(unrelated.json()).resolves.toMatchObject({
            imageID: snapshotID,
            deleted: true,
          });
        }),
      );
      expect(deleteImage).toHaveBeenCalledTimes(foreignEvidence.length);

      finish();
      expect((await creating).status).toBe(201);
    },
  );

  it("reconciles an existing exact AWS promotion into a durable pin before publication", async () => {
    const { coordinator, provider, storage } = await checkpointFixture("aws");
    vi.mocked(provider.createCheckpointImage).mockImplementationOnce(
      async (_lease, name, _noReboot, strategy, ownership) => {
        const image = providerImage("aws", name, strategy, ownership);
        await storage.put("image:aws:catalog:linux:existing:ami-000000000001", image);
        return image;
      },
    );
    expect(
      (await createCheckpoint(coordinator, "chk_reconciled_catalog", { mode: "manual" }, "image"))
        .status,
    ).toBe(201);
    expect(await storage.get(checkpointKey("chk_reconciled_catalog"))).toMatchObject({
      state: "ready",
      pinCount: 1,
    });
    const deleted = await coordinator.fetch(
      checkpointRequest("DELETE", "/v1/checkpoints/chk_reconciled_catalog"),
    );
    await expect(deleted.json()).resolves.toMatchObject({ error: "checkpoint_pinned" });
  });

  it("retains deferred cancellation claims until provider cleanup becomes definitive", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_deferred_cancel";
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    const attemptID = `cat_${"7".repeat(32)}`;
    await bindCheckpointUseProvisioning(
      storage,
      checkpointID,
      acquired.claim,
      { owner: "alice@example.com", org },
      attemptID,
      "cbx_000000000002",
    );
    const pendingLease = {
      ...checkpointLease("aws"),
      id: "cbx_000000000002",
      checkpointID,
      createAttemptID: attemptID,
      state: "released" as const,
      provisioningResourceMayExist: true,
      cleanupRetryAt: new Date(Date.now() + 30_000).toISOString(),
    };
    await storage.put("lease:cbx_000000000002", pendingLease);
    vi.setSystemTime(Date.now() + checkpointUseClaimTTLMS + 1);
    await coordinator.alarm();
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 1 });
    const cleaned = { ...pendingLease };
    delete cleaned.provisioningResourceMayExist;
    delete cleaned.cleanupRetryAt;
    await storage.put("lease:cbx_000000000002", cleaned);
    vi.setSystemTime(Date.now() + checkpointUseClaimTTLMS + 1);
    await coordinator.alarm();
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
  });
});
