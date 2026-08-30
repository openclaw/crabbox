import { afterEach, describe, expect, it, vi } from "vitest";

import { sha256Hex } from "../src/auth";
import {
  CheckpointError,
  expireCheckpointClaims,
  backfillFailedCheckpointCreateRecovery,
  checkpointAuditRetentionMS,
  checkpointDueKey,
  checkpointEventKey,
  checkpointDeleteClaimTTLMS,
  checkpointKey,
  checkpointLimits,
  checkpointMaxAuditEvents,
  checkpointMaxCreateRecoveryAttempts,
  checkpointPinKey,
  checkpointProviderAbsenceConfirmationIntervalMS,
  checkpointProviderAbsenceHorizonMS,
  checkpointResourceKey,
  checkpointUseClaimTTLMS,
  bindCheckpointUseProvisioning as bindCheckpointUseProvisioningTransaction,
  claimCheckpointDeletion,
  findManagedCheckpointImage,
  finalizeCheckpointDeletion,
  markCheckpointProviderDeleted,
  pinCheckpointPromotion,
  pruneCheckpointTombstone,
  recordCheckpointCreateRecoveryFailure,
  recordCheckpointDeletionFailure,
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
  CoordinatorCheckpointUseClaim,
  CreateAttemptRecord,
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
  readonly lists: Array<{ prefix?: string; limit?: number; startAfter?: string }> = [];
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
      ...(options.startAfter ? { startAfter: options.startAfter } : {}),
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

async function checkpointFixture(
  providerName: CoordinatorCheckpointProvider = "aws",
  overrides: Partial<Env> = {},
) {
  const storage = new CheckpointMemoryStorage();
  const runtime = new CheckpointRuntime(storage);
  const env = {
    FLEET: {} as DurableObjectNamespace,
    HETZNER_TOKEN: "",
    CRABBOX_DEFAULT_ORG: "example-org",
    ...overrides,
  } satisfies Env;
  const provider = new AWSProvider(env, "eu-west-1", storage);
  vi.spyOn(provider, "checkpointScope").mockResolvedValue(providerScope(providerName));
  vi.spyOn(provider, "validateCheckpointLeaseScope").mockResolvedValue(undefined);
  vi.spyOn(provider, "validateCheckpointImage").mockResolvedValue(undefined);
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

async function bindCheckpointUseProvisioning(
  storage: CheckpointMemoryStorage,
  checkpointID: string,
  claim: string,
  principal: { owner: string; org: string; admin?: boolean },
  attemptID: string,
  requestedLeaseID: string,
): Promise<CoordinatorCheckpointRecord> {
  const now = new Date().toISOString();
  await storage.put(`create-attempt:${requestedLeaseID}`, {
    version: 1,
    requestedLeaseID,
    token: attemptID,
    owner: principal.owner,
    org: principal.org,
    state: "pending",
    checkpointID,
    checkpointUseClaimHash: await sha256Hex(claim),
    createdAt: now,
    updatedAt: now,
  } satisfies CreateAttemptRecord);
  return await bindCheckpointUseProvisioningTransaction(
    storage,
    checkpointID,
    claim,
    principal,
    attemptID,
    requestedLeaseID,
  );
}

describe("coordinator-managed checkpoints", () => {
  it("parses finite positive checkpoint limits and falls back for invalid or unlimited values", () => {
    expect(checkpointLimits()).toEqual({
      checkpoints: 64,
      checkpointsPerOwner: 16,
      checkpointsPerOrg: 32,
      useClaimsPerCheckpoint: 16,
      useClaimsPerOwner: 64,
      useClaimsTotal: 256,
    });
    expect(
      checkpointLimits({
        CRABBOX_MAX_CHECKPOINTS: "0",
        CRABBOX_MAX_CHECKPOINTS_PER_OWNER: "-1",
        CRABBOX_MAX_CHECKPOINTS_PER_ORG: "1.5",
        CRABBOX_MAX_CHECKPOINT_USE_CLAIMS: "Infinity",
        CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_PER_OWNER: "9007199254740992",
        CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_TOTAL: "",
      }),
    ).toEqual(checkpointLimits());
    expect(
      checkpointLimits({
        CRABBOX_MAX_CHECKPOINTS: "3",
        CRABBOX_MAX_CHECKPOINTS_PER_OWNER: "2",
        CRABBOX_MAX_CHECKPOINTS_PER_ORG: "1",
        CRABBOX_MAX_CHECKPOINT_USE_CLAIMS: "4",
        CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_PER_OWNER: "5",
        CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_TOTAL: "6",
      }),
    ).toEqual({
      checkpoints: 3,
      checkpointsPerOwner: 2,
      checkpointsPerOrg: 1,
      useClaimsPerCheckpoint: 4,
      useClaimsPerOwner: 5,
      useClaimsTotal: 6,
    });
  });

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

  it.each([
    {
      scope: "global",
      limits: { CRABBOX_MAX_CHECKPOINTS: "1" },
      observed: "observed=1 limit=1",
    },
    {
      scope: "owner",
      limits: { CRABBOX_MAX_CHECKPOINTS_PER_OWNER: "1" },
      observed: "observed=1 limit=1",
    },
    {
      scope: "org",
      limits: { CRABBOX_MAX_CHECKPOINTS_PER_ORG: "1" },
      observed: "observed=1 limit=1",
    },
  ])(
    "rejects checkpoint creation at the exact $scope cap before provider mutation",
    async (test) => {
      const { coordinator, provider, storage } = await checkpointFixture("aws", test.limits);
      expect((await createCheckpoint(coordinator, "chk_admitted")).status).toBe(201);
      vi.mocked(provider.createCheckpointImage).mockClear();

      const rejected = await createCheckpoint(coordinator, "chk_one_over");

      expect(rejected.status).toBe(429);
      await expect(rejected.json()).resolves.toMatchObject({
        error: "checkpoint_limit_exceeded",
        message: expect.stringContaining(`scope=${test.scope} ${test.observed}`),
      });
      expect(provider.createCheckpointImage).not.toHaveBeenCalled();
      expect(await storage.get(checkpointKey("chk_one_over"))).toBeUndefined();
      expect(await storage.list({ prefix: "checkpoint-event:chk_one_over:" })).toHaveLength(0);
    },
  );

  it("preserves duplicate-checkpoint identifier precedence when admission is full", async () => {
    const { coordinator, provider } = await checkpointFixture("aws", {
      CRABBOX_MAX_CHECKPOINTS: "1",
    });
    expect((await createCheckpoint(coordinator, "chk_duplicate_full")).status).toBe(201);
    vi.mocked(provider.createCheckpointImage).mockClear();

    const duplicate = await createCheckpoint(coordinator, "chk_duplicate_full");

    expect(duplicate.status).toBe(409);
    await expect(duplicate.json()).resolves.toMatchObject({
      error: "checkpoint_pending",
      message: "checkpoint identifier is already reserved",
    });
    expect(provider.createCheckpointImage).not.toHaveBeenCalled();
  });

  it.each(["creating", "ready", "failed", "delete-pending", "deleting"] as const)(
    "counts %s checkpoint reservations toward admission",
    async (state) => {
      const { coordinator, provider, storage } = await checkpointFixture("aws", {
        CRABBOX_MAX_CHECKPOINTS: "1",
      });
      await createCheckpoint(coordinator, "chk_counted_state");
      const existing = (await storage.get<CoordinatorCheckpointRecord>(
        checkpointKey("chk_counted_state"),
      ))!;
      await storage.put(checkpointKey(existing.id), { ...existing, state });
      vi.mocked(provider.createCheckpointImage).mockClear();

      const rejected = await createCheckpoint(
        coordinator,
        `chk_counted_${state.replace("-", "_")}`,
      );

      expect(rejected.status).toBe(429);
      await expect(rejected.json()).resolves.toMatchObject({ error: "checkpoint_limit_exceeded" });
      expect(provider.createCheckpointImage).not.toHaveBeenCalled();
    },
  );

  it("skips deleted audit tombstones across bounded checkpoint scan pages", async () => {
    const { coordinator, provider, storage } = await checkpointFixture("aws", {
      CRABBOX_MAX_CHECKPOINTS: "1",
    });
    await createCheckpoint(coordinator, "chk_000_deleted");
    expect(
      (await coordinator.fetch(checkpointRequest("DELETE", "/v1/checkpoints/chk_000_deleted")))
        .status,
    ).toBe(200);
    const tombstone = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey("chk_000_deleted"),
    ))!;
    await storage.put(checkpointKey("chk_001_deleted"), { ...tombstone, id: "chk_001_deleted" });
    await storage.put(checkpointKey("chk_002_deleted"), { ...tombstone, id: "chk_002_deleted" });
    storage.lists.length = 0;
    vi.mocked(provider.createCheckpointImage).mockClear();

    const admitted = await createCheckpoint(coordinator, "chk_zzz_after_tombstones");

    expect(admitted.status).toBe(201);
    expect(provider.createCheckpointImage).toHaveBeenCalledOnce();
    const scanPages = storage.lists.filter((options) => options.prefix === "checkpoint:");
    expect(scanPages).toHaveLength(4);
    expect(scanPages.every((options) => options.limit === 1)).toBe(true);
  });

  it("accounts ambiguous legacy organization buckets against their canonical organization", async () => {
    const { coordinator, provider, storage } = await checkpointFixture("aws", {
      CRABBOX_MAX_CHECKPOINTS_PER_ORG: "1",
    });
    await createCheckpoint(coordinator, "chk_legacy_org");
    const existing = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey("chk_legacy_org"),
    ))!;
    await storage.put(checkpointKey(existing.id), {
      ...existing,
      owner: "bob@example.com",
      org: "example_org",
    });
    await storage.put(`lease:${leaseID}`, {
      ...checkpointLease("aws"),
      org: orgKeyForLabel("example/org"),
    });
    vi.mocked(provider.createCheckpointImage).mockClear();

    const rejected = await coordinator.fetch(
      checkpointRequest(
        "POST",
        "/v1/checkpoints",
        { id: "chk_canonical_org", leaseID, name: "prepared-workspace" },
        { org: "example/org" },
      ),
    );

    expect(rejected.status).toBe(429);
    await expect(rejected.json()).resolves.toMatchObject({
      error: "checkpoint_limit_exceeded",
      message: expect.stringContaining("scope=org observed=1 limit=1"),
    });
    expect(provider.createCheckpointImage).not.toHaveBeenCalled();
  });

  it("never over-admits concurrent checkpoint reservations in Durable Object-style storage", async () => {
    const { coordinator, provider, storage } = await checkpointFixture("aws", {
      CRABBOX_MAX_CHECKPOINTS: "3",
    });
    vi.mocked(provider.createCheckpointImage).mockImplementation(
      async (_lease, name, _noReboot, strategy, ownership) => {
        const id = `snap-${ownership.checkpointID}`;
        return {
          ...providerImage("aws", name, strategy, ownership),
          id,
          resourceID: id,
          immutableID: id,
          snapshots: [id],
        };
      },
    );

    const responses = await Promise.all(
      Array.from({ length: 8 }, async (_, index) =>
        createCheckpoint(coordinator, `chk_parallel_${index}`),
      ),
    );

    expect(responses.filter((response) => response.status === 201)).toHaveLength(3);
    expect(responses.filter((response) => response.status === 429)).toHaveLength(5);
    expect(provider.createCheckpointImage).toHaveBeenCalledTimes(3);
    expect(await storage.list({ prefix: "checkpoint:" })).toHaveLength(3);
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

  it.each([
    { scope: "/subscriptions/other/resourceGroups/checkpoint-rg", allowed: false },
    { scope: "/subscriptions/sub-123/resourceGroups/other", allowed: false },
    { scope: "/SUBSCRIPTIONS/SUB-123/RESOURCEGROUPS/CHECKPOINT-RG", allowed: true },
  ])(
    "checks durable Azure recovery scope before provider reads: $scope",
    async ({ scope, allowed }) => {
      const provider = new AzureProvider({
        FLEET: {} as DurableObjectNamespace,
        HETZNER_TOKEN: "",
      });
      const ownership = {
        checkpointID: "chk_azure_scope",
        tokenHash: "a".repeat(64),
        sourceLeaseID: leaseID,
      };
      const image = providerImage("azure", "recoverable", "disk-snapshot", ownership);
      const getImage = vi.fn<() => Promise<ProviderImage>>().mockResolvedValue(image);
      Reflect.set(provider, "clientValue", { providerScope: () => scope, getImage });
      const checkpoint = {
        id: ownership.checkpointID,
        provider: "azure",
        leaseID,
        scope: providerScope("azure"),
        createClaim: { resourceName: "recoverable", tokenHash: ownership.tokenHash },
      } as CoordinatorCheckpointRecord;
      const outcome = await provider.recoverCheckpointImage(checkpoint).then(
        (recovered) => ({ image: recovered }),
        (error: CheckpointError) => ({ code: error.code }),
      );
      expect(outcome).toEqual(allowed ? { image } : { code: "checkpoint_source_mismatch" });
      expect(getImage).toHaveBeenCalledTimes(Number(allowed));
    },
  );

  it.each([
    { strategy: "image", initiallyAbsent: true },
    { strategy: "image", initiallyAbsent: false },
    { strategy: "disk-snapshot", initiallyAbsent: true },
    { strategy: "disk-snapshot", initiallyAbsent: false },
  ] as const)(
    "finalizes AWS $strategy empty-describe absence (initial=$initiallyAbsent)",
    async ({ strategy, initiallyAbsent }) => {
      const storage = new CheckpointMemoryStorage();
      const provider = new AWSProvider(
        { FLEET: {} as DurableObjectNamespace, HETZNER_TOKEN: "" },
        "eu-west-1",
        storage,
      );
      const ownership = {
        checkpointID: "chk_aws_absent",
        tokenHash: "a".repeat(64),
        sourceLeaseID: leaseID,
      };
      const described = providerImage("aws", "absent-image", strategy, ownership);
      let deleted = initiallyAbsent;
      const getImage = vi.fn<(id: string) => Promise<ProviderImage>>(async (id) => {
        if (deleted)
          throw new Error(`aws ${id.startsWith("snap-") ? "snapshot" : "image"} not found: ${id}`);
        return described;
      });
      const deleteImage = vi.fn<() => Promise<void>>(async () => {
        deleted = true;
      });
      Reflect.set(provider, "clientValue", {
        verifiedIdentity: async () => ({ account: "123456789012", region: "eu-west-1" }),
        getImage,
        deleteImage,
      });
      const checkpoint = {
        id: ownership.checkpointID,
        name: "absent-image",
        provider: "aws",
        scope: providerScope("aws"),
        image: { ...described, snapshotIDs: described.snapshots ?? [] },
      } as CoordinatorCheckpointRecord;
      await expect(provider.deleteCheckpointImage(checkpoint)).resolves.toBeUndefined();
      expect(deleteImage).toHaveBeenCalledExactlyOnceWith(described.id, described.snapshots);
      for (const id of new Set([described.id, ...(described.snapshots ?? [])]))
        expect(getImage).toHaveBeenCalledWith(id);
    },
  );

  it.each([
    "aws image not found: ami-another-resource",
    "AccessDenied",
    "InvalidSnapshot.NotFound",
  ])("retains AWS deletion ownership on unrelated absence: %s", async (message) => {
    const storage = new CheckpointMemoryStorage();
    const provider = new AWSProvider(
      { FLEET: {} as DurableObjectNamespace, HETZNER_TOKEN: "" },
      "eu-west-1",
      storage,
    );
    const described = providerImage("aws", "owned-image", "image", {
      checkpointID: "chk_aws_wrong_absence",
      tokenHash: "a".repeat(64),
      sourceLeaseID: leaseID,
    });
    const deleteImage = vi.fn<() => Promise<void>>();
    Reflect.set(provider, "clientValue", {
      verifiedIdentity: async () => ({ account: "123456789012", region: "eu-west-1" }),
      getImage: async () => {
        throw new Error(message);
      },
      deleteImage,
    });
    const checkpoint = {
      id: "chk_aws_wrong_absence",
      provider: "aws",
      scope: providerScope("aws"),
      image: { ...described, snapshotIDs: described.snapshots ?? [] },
    } as CoordinatorCheckpointRecord;
    await expect(provider.deleteCheckpointImage(checkpoint)).rejects.toThrow(message);
    expect(deleteImage).not.toHaveBeenCalled();
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

  it.each([
    {
      scope: "checkpoint",
      limits: { CRABBOX_MAX_CHECKPOINT_USE_CLAIMS: "1" },
    },
    {
      scope: "owner",
      limits: { CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_PER_OWNER: "1" },
    },
    {
      scope: "global",
      limits: { CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_TOTAL: "1" },
    },
  ])("rejects the one-over $scope use claim without writing a claim or event", async (test) => {
    const { coordinator, storage } = await checkpointFixture("aws", test.limits);
    await createCheckpoint(coordinator, "chk_claim_limit");
    const first = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claim_limit/use", { action: "begin" }),
    );
    expect(first.status).toBe(201);
    const before = storage.snapshot();

    const rejected = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claim_limit/use", { action: "begin" }),
    );

    expect(rejected.status).toBe(429);
    await expect(rejected.json()).resolves.toMatchObject({
      error: "checkpoint_claim_limit_exceeded",
      message: expect.stringContaining(`scope=${test.scope} observed=1 limit=1`),
    });
    expect(storage.snapshot()).toBe(before);
    expect(await storage.list({ prefix: "checkpoint-use:chk_claim_limit:" })).toHaveLength(1);
  });

  it("counts claims across the same owner while excluding deleted checkpoints", async () => {
    const { coordinator, storage } = await checkpointFixture("aws", {
      CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_PER_OWNER: "2",
    });
    await createCheckpoint(coordinator, "chk_claim_owner_target");
    const target = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey("chk_claim_owner_target"),
    ))!;
    await storage.put(checkpointKey("chk_claim_owner_other"), {
      ...target,
      id: "chk_claim_owner_other",
      activeUseCount: 1,
    });
    await storage.put(checkpointKey("chk_claim_owner_deleted"), {
      ...target,
      id: "chk_claim_owner_deleted",
      state: "deleted",
      activeUseCount: 12,
    });

    const admitted = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claim_owner_target/use", {
        action: "begin",
      }),
    );
    const rejected = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claim_owner_target/use", {
        action: "begin",
      }),
    );

    expect(admitted.status).toBe(201);
    expect(rejected.status).toBe(429);
    await expect(rejected.json()).resolves.toMatchObject({
      message: expect.stringContaining("scope=owner observed=2 limit=2"),
    });
  });

  it("counts global claims across other checkpoint owners", async () => {
    const { coordinator, storage } = await checkpointFixture("aws", {
      CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_TOTAL: "2",
    });
    await createCheckpoint(coordinator, "chk_claim_global_target");
    const target = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey("chk_claim_global_target"),
    ))!;
    await storage.put(checkpointKey("chk_claim_global_other"), {
      ...target,
      id: "chk_claim_global_other",
      owner: "bob@example.com",
      activeUseCount: 1,
    });

    const admitted = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claim_global_target/use", {
        action: "begin",
      }),
    );
    const rejected = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claim_global_target/use", {
        action: "begin",
      }),
    );

    expect(admitted.status).toBe(201);
    expect(rejected.status).toBe(429);
    await expect(rejected.json()).resolves.toMatchObject({
      message: expect.stringContaining("scope=global observed=2 limit=2"),
    });
  });

  it("expires stale available claims before admitting their replacement", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage } = await checkpointFixture("aws", {
      CRABBOX_MAX_CHECKPOINT_USE_CLAIMS: "1",
      CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_PER_OWNER: "1",
      CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_TOTAL: "1",
    });
    await createCheckpoint(coordinator, "chk_claim_replacement");
    const first = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claim_replacement/use", { action: "begin" }),
    );
    expect(first.status).toBe(201);
    vi.setSystemTime(Date.now() + checkpointUseClaimTTLMS + 1);

    const replacement = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claim_replacement/use", { action: "begin" }),
    );

    expect(replacement.status).toBe(201);
    expect(await storage.get(checkpointKey("chk_claim_replacement"))).toMatchObject({
      activeUseCount: 1,
    });
    expect(await storage.list({ prefix: "checkpoint-use:chk_claim_replacement:" })).toHaveLength(1);
    const events = await storage.list<{ type: string }>({
      prefix: "checkpoint-event:chk_claim_replacement:",
    });
    expect([...events.values()].map((event) => event.type).slice(-2)).toEqual([
      "checkpoint.use.expired",
      "checkpoint.use.claimed",
    ]);
  });

  it("retains expired provisioning claims until exact lifecycle reconciliation", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage } = await checkpointFixture("aws", {
      CRABBOX_MAX_CHECKPOINT_USE_CLAIMS: "1",
    });
    await createCheckpoint(coordinator, "chk_claim_provisioning_limit");
    const first = (await (
      await coordinator.fetch(
        checkpointRequest("POST", "/v1/checkpoints/chk_claim_provisioning_limit/use", {
          action: "begin",
        }),
      )
    ).json()) as { claim: string };
    await bindCheckpointUseProvisioning(
      storage,
      "chk_claim_provisioning_limit",
      first.claim,
      { owner: "alice@example.com", org },
      `cat_${"7".repeat(32)}`,
      "cbx_000000000002",
    );
    vi.setSystemTime(Date.now() + checkpointUseClaimTTLMS + 1);
    const before = storage.snapshot();

    const rejected = await coordinator.fetch(
      checkpointRequest("POST", "/v1/checkpoints/chk_claim_provisioning_limit/use", {
        action: "begin",
      }),
    );

    expect(rejected.status).toBe(429);
    await expect(rejected.json()).resolves.toMatchObject({
      error: "checkpoint_claim_limit_exceeded",
      message: expect.stringContaining("scope=checkpoint observed=1 limit=1"),
    });
    expect(storage.snapshot()).toBe(before);
    expect(
      await storage.list({ prefix: "checkpoint-use:chk_claim_provisioning_limit:" }),
    ).toHaveLength(1);
  });

  it("supports concurrent checkpoint shard fanout exactly up to the configured limit", async () => {
    const { coordinator, storage } = await checkpointFixture("aws", {
      CRABBOX_MAX_CHECKPOINT_USE_CLAIMS: "6",
    });
    await createCheckpoint(coordinator, "chk_claim_parallel");

    const responses = await Promise.all(
      Array.from({ length: 9 }, async () =>
        coordinator.fetch(
          checkpointRequest("POST", "/v1/checkpoints/chk_claim_parallel/use", { action: "begin" }),
        ),
      ),
    );

    expect(responses.filter((response) => response.status === 201)).toHaveLength(6);
    expect(responses.filter((response) => response.status === 429)).toHaveLength(3);
    expect(await storage.get(checkpointKey("chk_claim_parallel"))).toMatchObject({
      activeUseCount: 6,
      eventSequence: 9,
    });
    expect(await storage.list({ prefix: "checkpoint-use:chk_claim_parallel:" })).toHaveLength(6);
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

  it.each(
    (["aws", "azure"] as const).flatMap((providerName) =>
      ["deleting", "delete-pending", "before-commit", "after-metadata", "generation-change"].map(
        (phase) => ({ providerName, phase }),
      ),
    ),
  )(
    "refuses $providerName promotion when checkpoint ownership changes $phase",
    async ({ providerName, phase }) => {
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
      const region = providerName === "aws" ? "eu-west-1" : "westeurope";
      const provider =
        providerName === "aws"
          ? new AWSProvider(env, region, storage)
          : new AzureProvider(env, undefined, storage, region);
      vi.spyOn(provider, "checkpointScope").mockResolvedValue(providerScope(providerName));
      vi.spyOn(provider, "createCheckpointImage").mockImplementation(
        async (_lease, name, _noReboot, strategy, ownership) => ({
          ...providerImage(providerName, name, strategy, ownership),
          serverType: "standard-small",
        }),
      );
      const coordinator = new FleetCoordinator(runtime, env, { [providerName]: provider });
      await storage.put(`lease:${leaseID}`, checkpointLease(providerName));
      const id = "chk_promotion_race";
      expect(
        (
          await createCheckpoint(
            coordinator,
            id,
            { mode: "manual" },
            providerName === "aws" ? "image" : "disk-snapshot",
          )
        ).status,
      ).toBe(201);
      const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(id)))!;
      const image = (await provider.storedImageMetadata(record.image!.id))!;
      vi.spyOn(provider, "getImage").mockResolvedValue(image);
      const promote = provider.promoteImage.bind(provider);
      const promotion = vi.spyOn(provider, "promoteImage");
      const finishDeletion = async () => {
        const claimed = await claimCheckpointDeletion(storage, id, undefined, "manual");
        await markCheckpointProviderDeleted(storage, id, claimed.token);
        await finalizeCheckpointDeletion(storage, id, claimed.token);
      };
      if (phase === "deleting" || phase === "delete-pending") {
        const claimed = await claimCheckpointDeletion(storage, id, undefined, "manual");
        if (phase === "delete-pending")
          await recordCheckpointDeletionFailure(storage, id, claimed.token, "retry");
      } else if (phase === "after-metadata") {
        const metadata = provider.storedImageMetadata.bind(provider);
        vi.spyOn(provider, "storedImageMetadata").mockImplementation(async (imageID) => {
          const known = await metadata(imageID);
          await finishDeletion();
          return known;
        });
      } else {
        promotion.mockImplementation(async (...args) => {
          if (phase === "before-commit") await finishDeletion();
          else
            await storage.put(checkpointKey(id), { ...record, generation: record.generation + 1 });
          return promote(...args);
        });
      }
      const catalogPrefixes =
        providerName === "aws"
          ? ["image:aws:promoted", "image:aws:catalog:", "image:aws:variant:"]
          : ["image:azure:promoted:"];
      const oldKey = `${catalogPrefixes[0]}:previous`;
      await storage.put(oldKey, {
        id: "previous-image",
        provider: providerName,
        state: "available",
      });
      const before = await Promise.all(
        catalogPrefixes.map(async (prefix) => await storage.list({ prefix })),
      );
      const response = await coordinator.fetch(
        checkpointRequest(
          "POST",
          `/v1/images/${encodeURIComponent(record.image!.id)}/promote?provider=${providerName}&region=${region}`,
          { target: "linux" },
          { admin: true },
        ),
      );
      expect(response.status).toBe(409);
      await expect(response.json()).resolves.toMatchObject({
        error: "checkpoint_delete_in_progress",
      });
      expect(promotion).toHaveBeenCalledTimes(
        ["before-commit", "generation-change"].includes(phase) ? 1 : 0,
      );
      expect(
        await Promise.all(catalogPrefixes.map(async (prefix) => await storage.list({ prefix }))),
      ).toEqual(before);
      expect(await storage.list({ prefix: `checkpoint-pin:${id}:` })).toHaveLength(0);
    },
  );

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

  it.each([
    ["azure", "disk-snapshot"],
    ["gcp", "disk-snapshot"],
    ["gcp", "image"],
    ["aws", "disk-snapshot"],
    ["aws", "image"],
  ] as const)(
    "freshly verifies the exact owned %s %s checkpoint before binding provisioning",
    async (providerName, strategy) => {
      const { coordinator, storage, provider } = await checkpointFixture(providerName);
      const checkpointID = `chk_verified_${providerName}_${strategy.replace("-", "_")}`;
      expect(
        (await createCheckpoint(coordinator, checkpointID, { mode: "manual" }, strategy)).status,
      ).toBe(201);
      const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
      const metadata = (await storage.get<ProviderImage>(
        `image:${providerName}:created:${encodeURIComponent(record.image!.id)}`,
      ))!;
      const backing =
        providerName === "aws" && strategy === "image"
          ? {
              ...metadata,
              id: record.image!.snapshotIDs[0]!,
              name: record.image!.snapshotIDs[0]!,
              kind: "aws-ebs-snapshot",
              resourceID: record.image!.snapshotIDs[0]!,
              immutableID: record.image!.snapshotIDs[0]!,
              snapshots: [record.image!.snapshotIDs[0]!],
            }
          : undefined;
      const current = {
        ...metadata,
        ...(providerName === "azure" ? { resourceID: metadata.resourceID!.toUpperCase() } : {}),
      };
      const getImage = vi.fn<(imageID: string, kind?: string) => Promise<ProviderImage>>(
        async (imageID) => (backing && imageID === backing.id ? backing : current),
      );
      const env = { FLEET: {} as DurableObjectNamespace, HETZNER_TOKEN: "" } satisfies Env;
      const validator =
        providerName === "azure"
          ? new AzureProvider(env, undefined, storage)
          : providerName === "gcp"
            ? new GCPProvider(env, storage)
            : new AWSProvider(env, record.scope.region, storage);
      Object.assign(validator, { clientValue: { getImage } });
      vi.mocked(provider.validateCheckpointImage).mockImplementation(
        async (checkpoint) => await validator.validateCheckpointImage(checkpoint),
      );
      const acquired = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        )
      ).json()) as { claim: string };
      const createLease = vi
        .spyOn(coordinator as never, "createLease" as never)
        .mockResolvedValue(Response.json({ lease: { id: "cbx_000000000002" } }, { status: 201 }));
      const selector =
        providerName === "azure"
          ? { azureSnapshot: record.image!.id }
          : providerName === "gcp"
            ? strategy === "image"
              ? { gcpMachineImage: record.image!.id }
              : { gcpSnapshot: record.image!.id }
            : strategy === "image"
              ? { awsAMI: record.image!.id }
              : { awsSnapshot: record.image!.id };
      const response = await coordinator.fetch(
        checkpointRequest("POST", "/v1/leases/from-checkpoint", {
          provider: providerName,
          checkpointID,
          checkpointUseClaim: acquired.claim,
          leaseID: "cbx_000000000002",
          createAttemptID: `cat_${"c".repeat(32)}`,
          ...selector,
        }),
      );
      expect(response.status).toBe(201);
      expect(createLease).toHaveBeenCalledOnce();
      expect(
        vi.mocked(provider.validateCheckpointLeaseScope).mock.invocationCallOrder[0],
      ).toBeLessThan(vi.mocked(provider.validateCheckpointImage).mock.invocationCallOrder[0]!);
      expect(getImage).toHaveBeenCalledWith(
        record.image!.id,
        ...(providerName === "aws" ? [] : [record.image!.kind]),
      );
      expect(getImage).toHaveBeenCalledTimes(backing ? 2 : 1);
    },
  );

  it.each([
    ["azure", "disk-snapshot", "immutable"],
    ["azure", "disk-snapshot", "ownership-hash"],
    ["azure", "disk-snapshot", "source-lease"],
    ["azure", "disk-snapshot", "missing-tag"],
    ["azure", "disk-snapshot", "location"],
    ["azure", "disk-snapshot", "resource"],
    ["azure", "disk-snapshot", "missing-metadata"],
    ["gcp", "disk-snapshot", "immutable"],
    ["gcp", "disk-snapshot", "ownership-hash"],
    ["gcp", "disk-snapshot", "source-lease"],
    ["gcp", "disk-snapshot", "missing-tag"],
    ["gcp", "disk-snapshot", "project"],
    ["gcp", "disk-snapshot", "kind"],
    ["gcp", "disk-snapshot", "missing-metadata"],
    ["gcp", "image", "immutable"],
    ["gcp", "image", "ownership-hash"],
    ["gcp", "image", "source-lease"],
    ["gcp", "image", "missing-tag"],
    ["gcp", "image", "project"],
    ["gcp", "image", "kind"],
    ["gcp", "image", "missing-metadata"],
    ["aws", "disk-snapshot", "ownership-hash"],
    ["aws", "disk-snapshot", "account"],
    ["aws", "disk-snapshot", "not-found"],
    ["aws", "image", "backing-ownership"],
    ["aws", "image", "backing-set"],
    ["aws", "image", "missing-metadata"],
  ] as const)(
    "rejects %s %s checkpoint %s drift before a lease or attempt exists",
    async (providerName, strategy, drift) => {
      const { coordinator, storage, provider } = await checkpointFixture(providerName);
      const checkpointID = `chk_drift_${providerName}_${strategy.replace("-", "_")}_${drift.replace("-", "_")}`;
      expect(
        (await createCheckpoint(coordinator, checkpointID, { mode: "manual" }, strategy)).status,
      ).toBe(201);
      const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
      const metadataKey = `image:${providerName}:created:${encodeURIComponent(record.image!.id)}`;
      const metadata = (await storage.get<ProviderImage>(metadataKey))!;
      const current: ProviderImage = { ...metadata };
      if (drift === "immutable") current.immutableID = "replacement-immutable-identity";
      if (drift === "ownership-hash") current.checkpointOwnershipHash = "f".repeat(64);
      if (drift === "source-lease") current.checkpointSourceLeaseID = "cbx_foreign_lease";
      if (drift === "missing-tag") delete current.checkpointOwnershipHash;
      if (drift === "location") current.region = "eastus";
      if (drift === "resource")
        current.resourceID = current.resourceID!.replace("sub-123", "sub-foreign");
      if (drift === "project") current.project = "foreign-project";
      if (drift === "kind")
        current.kind = strategy === "image" ? "gcp-disk-snapshot" : "gcp-machine-image";
      if (drift === "account") current.accountID = "999999999999";
      if (drift === "backing-set") current.snapshots = ["snap-replacement-0001"];
      if (drift === "missing-metadata") await storage.delete(metadataKey);
      const getImage = vi.fn<(imageID: string, kind?: string) => Promise<ProviderImage>>(
        async (imageID) => {
          if (drift === "not-found") throw new Error(`aws snapshot not found: ${imageID}`);
          if (providerName === "aws" && strategy === "image" && imageID !== record.image!.id) {
            return {
              ...metadata,
              id: imageID,
              name: imageID,
              kind: "aws-ebs-snapshot",
              resourceID: imageID,
              immutableID: imageID,
              checkpointOwnershipHash:
                drift === "backing-ownership" ? "f".repeat(64) : metadata.checkpointOwnershipHash,
              snapshots: [imageID],
            };
          }
          return current;
        },
      );
      const env = { FLEET: {} as DurableObjectNamespace, HETZNER_TOKEN: "" } satisfies Env;
      const validator =
        providerName === "azure"
          ? new AzureProvider(env, undefined, storage)
          : providerName === "gcp"
            ? new GCPProvider(env, storage)
            : new AWSProvider(env, record.scope.region, storage);
      Object.assign(validator, { clientValue: { getImage } });
      vi.mocked(provider.validateCheckpointImage).mockImplementation(
        async (checkpoint) => await validator.validateCheckpointImage(checkpoint),
      );
      const acquired = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        )
      ).json()) as { claim: string };
      const createLease = vi.spyOn(coordinator as never, "createLease" as never);
      const selector =
        providerName === "azure"
          ? { azureSnapshot: record.image!.id }
          : providerName === "gcp"
            ? strategy === "image"
              ? { gcpMachineImage: record.image!.id }
              : { gcpSnapshot: record.image!.id }
            : strategy === "image"
              ? { awsAMI: record.image!.id }
              : { awsSnapshot: record.image!.id };
      const response = await coordinator.fetch(
        checkpointRequest("POST", "/v1/leases/from-checkpoint", {
          provider: providerName,
          checkpointID,
          checkpointUseClaim: acquired.claim,
          leaseID: "cbx_000000000002",
          createAttemptID: `cat_${"c".repeat(32)}`,
          ...selector,
        }),
      );
      expect(response.status).toBe(409);
      await expect(response.json()).resolves.toMatchObject({ error: "checkpoint_source_mismatch" });
      expect(createLease).not.toHaveBeenCalled();
      expect(await storage.get("lease:cbx_000000000002")).toBeUndefined();
      expect(await storage.get("create-attempt:cbx_000000000002")).toBeUndefined();
      const claims = [
        ...(
          await storage.list<{ state: string; attemptID?: string; leaseID?: string }>({
            prefix: `checkpoint-use:${checkpointID}:`,
          })
        ).values(),
      ];
      expect(claims).toEqual([expect.objectContaining({ state: "available" })]);
      expect(claims[0]?.attemptID).toBeUndefined();
      expect(claims[0]?.leaseID).toBeUndefined();
      const aborted = await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, {
          action: "abort",
          claim: acquired.claim,
        }),
      );
      expect(aborted.status).toBe(200);
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

  it.each([
    { label: "admission", error: "cost_limit_exceeded", status: 429 },
    { label: "provider readiness", error: "provider_not_configured", status: 424 },
  ] as const)(
    "cancels the exact checkpoint attempt and releases its claim after $label rejection",
    async ({ label, error, status }) => {
      const { coordinator, storage, provider } = await checkpointFixture("aws", {
        CRABBOX_MAX_CHECKPOINT_USE_CLAIMS: "1",
        ...(label === "admission" ? { CRABBOX_MAX_ACTIVE_LEASES: "1" } : {}),
      });
      const checkpointID = `chk_preprovision_${label.replaceAll(" ", "_")}`;
      const requestedLeaseID = "cbx_000000000002";
      const attemptID = `cat_${"a".repeat(32)}`;
      await createCheckpoint(coordinator, checkpointID);
      const acquired = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        )
      ).json()) as { claim: string };
      vi.spyOn(provider, "hourlyPriceUSD").mockResolvedValue(1);
      const providerCalls = vi.spyOn(provider, "createServerWithFallback");
      if (label === "provider readiness") {
        const readinessCoordinator = coordinator as unknown as {
          providerConfigurationReadiness(provider: string): {
            provider: string;
            configured: boolean;
            missing: string[];
            message: string;
          };
        };
        vi.spyOn(readinessCoordinator, "providerConfigurationReadiness").mockReturnValue({
          provider: "aws",
          configured: false,
          missing: ["credentials"],
          message: "AWS provider is not configured",
        });
      }
      const body = {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: requestedLeaseID,
        createAttemptID: attemptID,
        sshPublicKey: "ssh-ed25519 checkpoint-test",
      };

      const rejected = await coordinator.fetch(
        checkpointRequest("POST", "/v1/leases/from-checkpoint", body),
      );

      expect(await rejected.json()).toMatchObject({ error });
      expect(rejected.status).toBe(status);
      expect(await storage.get(`lease:${requestedLeaseID}`)).toBeUndefined();
      expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
        requestedLeaseID,
        token: attemptID,
        owner: "alice@example.com",
        org,
        state: "canceled",
        checkpointID,
        checkpointUseClaimHash: await sha256Hex(acquired.claim),
      });
      expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
      expect(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).toHaveLength(0);
      expect(providerCalls).not.toHaveBeenCalled();

      const replay = await coordinator.fetch(
        checkpointRequest("POST", "/v1/leases/from-checkpoint", body),
      );
      expect(replay.status).toBe(409);
      await expect(replay.json()).resolves.toMatchObject({ error: "create_canceled" });
      expect(providerCalls).not.toHaveBeenCalled();
      expect(
        (
          await coordinator.fetch(
            checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
          )
        ).status,
      ).toBe(201);
    },
  );

  it.each([
    { label: "provisioning lease", lease: { state: "provisioning", cloudID: "" } },
    { label: "active lease", lease: { state: "active" } },
    {
      label: "uncertain failed lease",
      lease: { state: "failed", provisioningResourceMayExist: true },
    },
    {
      label: "uncertain released lease",
      lease: { state: "released", cleanupRetryAt: "2026-08-20T13:00:00.000Z" },
    },
    {
      label: "started provider cleanup",
      lease: { state: "failed", cleanupStartedAt: "2026-08-20T12:00:00.000Z" },
    },
    {
      label: "failed provider cleanup",
      lease: { state: "failed", cleanupError: "provider cleanup remains uncertain" },
    },
    {
      label: "recorded provider cleanup failure",
      lease: { state: "failed", cleanupFailedAt: "2026-08-20T12:00:00.000Z" },
    },
    {
      label: "pending provider cleanup claim",
      lease: { state: "released", cleanupClaimExpiresAt: "2026-08-20T13:00:00.000Z" },
    },
    {
      label: "incomplete provider cleanup attempts",
      lease: { state: "expired", cleanupAttempts: 1 },
    },
    {
      label: "retryable provisioning failure",
      lease: { state: "failed", provisioningFailureRetryable: true },
    },
    {
      label: "pending provider key cleanup",
      lease: { state: "released", providerKeyCleanupPending: true },
    },
    {
      label: "pending server deletion",
      lease: { state: "released", releaseDeletesServer: true },
    },
    {
      label: "pending runtime adapter deletion",
      lease: {
        state: "released",
        runtimeAdapterDeleteRequestedAt: "2026-08-20T12:00:00.000Z",
      },
    },
    {
      label: "retrying runtime adapter deletion",
      lease: { state: "failed", runtimeAdapterDeleteRetryAt: "2026-08-20T13:00:00.000Z" },
    },
    {
      label: "claimed runtime adapter deletion",
      lease: { state: "released", runtimeAdapterDeleteClaimID: "runtime-delete-claim" },
    },
    {
      label: "dispatched runtime adapter deletion",
      lease: {
        state: "released",
        runtimeAdapterDeleteDispatchUntil: "2026-08-20T13:00:00.000Z",
      },
    },
    {
      label: "incomplete runtime adapter deletion attempts",
      lease: { state: "expired", runtimeAdapterDeleteAttempts: 1 },
    },
    {
      label: "failed runtime adapter deletion",
      lease: { state: "expired", runtimeAdapterDeleteError: "adapter cleanup remains uncertain" },
    },
    { label: "workspace reservation", ownershipKey: "workspace-lease" },
    { label: "provider resource ownership", ownershipKey: "provider-access" },
    { label: "missing attempt", missingAttempt: true },
    { label: "mismatched checkpoint owner", checkpoint: { owner: "other@example.com" } },
    { label: "mismatched checkpoint organization", checkpoint: { org: "other-org" } },
    { label: "mismatched checkpoint generation", checkpoint: { generation: 99 } },
    { label: "mismatched claim owner", claim: { owner: "other@example.com" } },
    { label: "mismatched claim organization", claim: { org: "other-org" } },
    { label: "mismatched claim generation", claim: { generation: 99 } },
    { label: "mismatched claim token hash", claim: { tokenHash: "f".repeat(64) } },
    { label: "mismatched claim lease", claim: { leaseID: "cbx_000000000099" } },
    { label: "mismatched claim attempt", claim: { attemptID: `cat_${"9".repeat(32)}` } },
    { label: "canonical lease ownership", attempt: { canonicalLeaseID: "cbx_000000000002" } },
    { label: "cloud resource ownership", attempt: { cloudID: "i-potentially-surviving" } },
    { label: "provisioning generation", attempt: { generation: "provider-generation" } },
    { label: "mismatched attempt token", attempt: { token: `cat_${"f".repeat(32)}` } },
    { label: "mismatched attempt owner", attempt: { owner: "other@example.com" } },
    { label: "mismatched attempt organization", attempt: { org: "other-org" } },
    { label: "mismatched attempt checkpoint", attempt: { checkpointID: "chk_other_checkpoint" } },
    {
      label: "mismatched attempt claim",
      attempt: { checkpointUseClaimHash: "f".repeat(64) },
    },
    {
      label: "canceled attempt with cloud ownership",
      attempt: { state: "canceled", cloudID: "i-potentially-surviving" },
    },
    {
      label: "canceled attempt with canonical ownership",
      attempt: { state: "canceled", canonicalLeaseID: "cbx_000000000002" },
    },
    {
      label: "canceled attempt with provisioning generation",
      attempt: { state: "canceled", generation: "provider-generation" },
    },
    {
      label: "terminal lease with mismatched generation",
      lease: { state: "failed", createAttemptGeneration: "other-generation" },
    },
    {
      label: "terminal lease with mismatched canonical ownership",
      lease: { state: "released" },
      attempt: { canonicalLeaseID: "cbx_000000000099" },
    },
    {
      label: "terminal lease with mismatched cloud ownership",
      lease: { state: "released" },
      attempt: { cloudID: "i-other-provider-resource" },
    },
    {
      label: "terminal lease with workspace reservation",
      lease: { state: "failed" },
      ownershipKey: "workspace-lease",
    },
    {
      label: "terminal lease with provider resource ownership",
      lease: { state: "released" },
      ownershipKey: "provider-access",
    },
    {
      label: "foreign terminal lease",
      lease: { state: "released", checkpointID: "chk_other_checkpoint" },
    },
    {
      label: "terminal lease with mismatched owner",
      lease: { state: "released", owner: "other@example.com" },
    },
    {
      label: "terminal lease with mismatched organization",
      lease: { state: "released", org: "other-org" },
    },
    {
      label: "terminal lease with mismatched attempt",
      lease: { state: "released", createAttemptID: `cat_${"9".repeat(32)}` },
    },
  ] as const)("retains the checkpoint fence when rejection finds $label", async (test) => {
    const { coordinator, storage, provider } = await checkpointFixture();
    const checkpointID = `chk_retain_${test.label.replaceAll(" ", "_")}`;
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"b".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    const providerCalls = vi.spyOn(provider, "createServerWithFallback");
    vi.spyOn(coordinator as never, "createLease" as never).mockImplementation(async () => {
      if ("lease" in test) {
        await storage.put(`lease:${requestedLeaseID}`, {
          ...checkpointLease("aws"),
          id: requestedLeaseID,
          checkpointID,
          createAttemptID: attemptID,
          ...test.lease,
        });
      }
      if ("ownershipKey" in test) {
        await storage.put(`${test.ownershipKey}:${requestedLeaseID}`, true);
      }
      if ("attempt" in test) {
        const attempt = (await storage.get<CreateAttemptRecord>(
          `create-attempt:${requestedLeaseID}`,
        ))!;
        await storage.put(`create-attempt:${requestedLeaseID}`, { ...attempt, ...test.attempt });
      }
      if ("checkpoint" in test) {
        const checkpoint = (await storage.get<CoordinatorCheckpointRecord>(
          checkpointKey(checkpointID),
        ))!;
        await storage.put(checkpointKey(checkpointID), { ...checkpoint, ...test.checkpoint });
      }
      if ("claim" in test) {
        const claimKey = `checkpoint-use:${checkpointID}:${await sha256Hex(acquired.claim)}`;
        const claim = (await storage.get<CoordinatorCheckpointUseClaim>(claimKey))!;
        await storage.put(claimKey, { ...claim, ...test.claim });
      }
      if ("missingAttempt" in test) {
        await storage.delete(`create-attempt:${requestedLeaseID}`);
      }
      return Response.json({ error: "provider_pending" }, { status: 503 });
    });

    const rejected = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: requestedLeaseID,
        createAttemptID: attemptID,
      }),
    );

    expect(rejected.status).toBe(503);
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 1 });
    const retainedAttempt = await storage.get(`create-attempt:${requestedLeaseID}`);
    expect(retainedAttempt === undefined).toBe("missingAttempt" in test);
    expect(retainedAttempt ?? { state: "pending" }).toMatchObject({
      state: "pending",
      ...("attempt" in test ? test.attempt : {}),
    });
    expect([
      ...(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).values(),
    ]).toEqual([
      expect.objectContaining({
        state: "provisioning",
        leaseID: requestedLeaseID,
        attemptID,
        ...("claim" in test ? test.claim : {}),
      }),
    ]);
    expect(providerCalls).not.toHaveBeenCalled();
  });

  it("releases an exact already-canceled checkpoint attempt without a lease", async () => {
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_rejected_already_canceled";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"e".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    vi.spyOn(coordinator as never, "createLease" as never).mockImplementation(async () => {
      const attempt = (await storage.get<CreateAttemptRecord>(
        `create-attempt:${requestedLeaseID}`,
      ))!;
      await storage.put(`create-attempt:${requestedLeaseID}`, {
        ...attempt,
        state: "canceled",
      } satisfies CreateAttemptRecord);
      return Response.json({ error: "create_canceled" }, { status: 409 });
    });

    const rejected = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: requestedLeaseID,
        createAttemptID: attemptID,
      }),
    );

    expect(rejected.status).toBe(409);
    expect(await storage.get(`lease:${requestedLeaseID}`)).toBeUndefined();
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      token: attemptID,
      state: "canceled",
    });
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
    expect(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).toHaveLength(0);
    const events = [
      ...(
        await storage.list<{ type: string }>({ prefix: `checkpoint-event:${checkpointID}:` })
      ).values(),
    ];
    expect(events.filter((event) => event.type === "checkpoint.use.aborted")).toHaveLength(1);
  });

  it.each(["failed", "released", "expired"] as const)(
    "releases a checkpoint claim after an exact clean terminal %s lease rejection",
    async (state) => {
      const { coordinator, storage } = await checkpointFixture();
      const checkpointID = `chk_rejected_terminal_${state}`;
      const requestedLeaseID = "cbx_000000000002";
      const attemptID = `cat_${"d".repeat(32)}`;
      await createCheckpoint(coordinator, checkpointID);
      const acquired = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        )
      ).json()) as { claim: string };
      vi.spyOn(coordinator as never, "createLease" as never).mockImplementation(async () => {
        await storage.put(`lease:${requestedLeaseID}`, {
          ...checkpointLease("aws"),
          id: requestedLeaseID,
          checkpointID,
          createAttemptID: attemptID,
          state,
        });
        return Response.json({ error: "lease_create_failed" }, { status: 503 });
      });

      const rejected = await coordinator.fetch(
        checkpointRequest("POST", "/v1/leases/from-checkpoint", {
          provider: "aws",
          awsSnapshot: "snap-000000000001",
          checkpointID,
          checkpointUseClaim: acquired.claim,
          leaseID: requestedLeaseID,
          createAttemptID: attemptID,
        }),
      );

      expect(rejected.status).toBe(503);
      expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
      expect(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).toHaveLength(0);
      expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
        token: attemptID,
        state: "pending",
      });
      const events = [
        ...(
          await storage.list<{ type: string }>({ prefix: `checkpoint-event:${checkpointID}:` })
        ).values(),
      ];
      expect(events.filter((event) => event.type === "checkpoint.use.aborted")).toHaveLength(1);
    },
  );

  it("releases an exact terminal lease with matching canonical, cloud, and generation ownership", async () => {
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_rejected_fully_bound_terminal";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"d".repeat(32)}`;
    const generation = "exact-provider-generation";
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    vi.spyOn(coordinator as never, "createLease" as never).mockImplementation(async () => {
      const lease = {
        ...checkpointLease("aws"),
        id: requestedLeaseID,
        checkpointID,
        createAttemptID: attemptID,
        createAttemptGeneration: generation,
        state: "released",
      } satisfies LeaseRecord;
      const attempt = (await storage.get<CreateAttemptRecord>(
        `create-attempt:${requestedLeaseID}`,
      ))!;
      await storage.put(`lease:${requestedLeaseID}`, lease);
      await storage.put(`create-attempt:${requestedLeaseID}`, {
        ...attempt,
        canonicalLeaseID: requestedLeaseID,
        cloudID: lease.cloudID,
        generation,
      } satisfies CreateAttemptRecord);
      return Response.json({ error: "lease_create_failed" }, { status: 503 });
    });

    const rejected = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: requestedLeaseID,
        createAttemptID: attemptID,
      }),
    );

    expect(rejected.status).toBe(503);
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
    expect(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).toHaveLength(0);
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      token: attemptID,
      state: "pending",
      canonicalLeaseID: requestedLeaseID,
      generation,
    });
  });

  it.each([
    { label: "reactivated", lease: { state: "active" } },
    {
      label: "uncertain provider cleanup",
      lease: { state: "failed", provisioningResourceMayExist: true },
    },
    {
      label: "pending runtime adapter cleanup",
      lease: {
        state: "released",
        runtimeAdapterDeleteRequestedAt: "2026-08-20T12:00:00.000Z",
      },
    },
  ] as const)(
    "retains the checkpoint claim when a terminal lease becomes $label before resolution",
    async ({ label, lease }) => {
      const { coordinator, storage } = await checkpointFixture();
      const checkpointID = `chk_terminal_race_${label.replaceAll(" ", "_")}`;
      const requestedLeaseID = "cbx_000000000002";
      const attemptID = `cat_${"f".repeat(32)}`;
      await createCheckpoint(coordinator, checkpointID);
      const acquired = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        )
      ).json()) as { claim: string };
      let resolutionPending = false;
      const transaction = storage.transaction.bind(storage);
      vi.spyOn(storage, "transaction").mockImplementation(async (callback) => {
        if (resolutionPending) {
          resolutionPending = false;
          const previous = (await storage.get<LeaseRecord>(`lease:${requestedLeaseID}`))!;
          await storage.put(`lease:${requestedLeaseID}`, { ...previous, ...lease });
        }
        return await transaction(callback);
      });
      const schedule = vi.spyOn(coordinator as never, "scheduleCheckpointAlarm" as never);
      vi.spyOn(coordinator as never, "createLease" as never).mockImplementation(async () => {
        await storage.put(`lease:${requestedLeaseID}`, {
          ...checkpointLease("aws"),
          id: requestedLeaseID,
          checkpointID,
          createAttemptID: attemptID,
          state: "failed",
        });
        resolutionPending = true;
        return Response.json({ error: "lease_create_failed" }, { status: 503 });
      });

      const rejected = await coordinator.fetch(
        checkpointRequest("POST", "/v1/leases/from-checkpoint", {
          provider: "aws",
          awsSnapshot: "snap-000000000001",
          checkpointID,
          checkpointUseClaim: acquired.claim,
          leaseID: requestedLeaseID,
          createAttemptID: attemptID,
        }),
      );

      expect(rejected.status).toBe(503);
      expect(await storage.get(`lease:${requestedLeaseID}`)).toMatchObject(lease);
      expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 1 });
      expect([
        ...(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).values(),
      ]).toEqual([
        expect.objectContaining({ state: "provisioning", leaseID: requestedLeaseID, attemptID }),
      ]);
      const events = [
        ...(
          await storage.list<{ type: string }>({ prefix: `checkpoint-event:${checkpointID}:` })
        ).values(),
      ];
      expect(events.filter((event) => event.type === "checkpoint.use.aborted")).toHaveLength(0);
      expect(schedule).toHaveBeenCalledOnce();
    },
  );

  it("cancels one rejected checkpoint attempt despite concurrent exact retries", async () => {
    const { coordinator, storage, provider } = await checkpointFixture();
    const checkpointID = "chk_concurrent_rejected_attempt";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"c".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    let entered!: () => void;
    let reject!: () => void;
    const started = new Promise<void>((resolve) => {
      entered = resolve;
    });
    const providerCalls = vi.spyOn(provider, "createServerWithFallback");
    const createLease = vi
      .spyOn(coordinator as never, "createLease" as never)
      .mockImplementation(async () => {
        entered();
        return await new Promise<Response>((resolve) => {
          reject = () => resolve(Response.json({ error: "capacity_exhausted" }, { status: 429 }));
        });
      });
    const body = {
      provider: "aws",
      awsSnapshot: "snap-000000000001",
      checkpointID,
      checkpointUseClaim: acquired.claim,
      leaseID: requestedLeaseID,
      createAttemptID: attemptID,
    };
    const original = coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", body),
    );
    await started;

    const concurrent = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", body),
    );
    expect(concurrent.status).toBe(409);
    expect(createLease).toHaveBeenCalledOnce();
    reject();
    expect((await original).status).toBe(429);

    const replay = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", body),
    );
    expect(replay.status).toBe(409);
    await expect(replay.json()).resolves.toMatchObject({ error: "create_canceled" });
    expect(createLease).toHaveBeenCalledOnce();
    expect(providerCalls).not.toHaveBeenCalled();
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      token: attemptID,
      state: "canceled",
    });
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
  });

  it("durably reserves the exact lease attempt before a use claim enters provisioning", async () => {
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_attempt_before_claim";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"1".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    const writes = vi.spyOn(storage, "put");
    const provision = vi
      .spyOn(coordinator as never, "createLease" as never)
      .mockImplementation(async () => {
        expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
          requestedLeaseID,
          token: attemptID,
          owner: "alice@example.com",
          org,
          state: "pending",
          checkpointID,
          checkpointUseClaimHash: await sha256Hex(acquired.claim),
        });
        expect([
          ...(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).values(),
        ]).toEqual([
          expect.objectContaining({
            state: "provisioning",
            attemptID,
            leaseID: requestedLeaseID,
          }),
        ]);
        return Response.json({ lease: {} }, { status: 201 });
      });

    const response = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: requestedLeaseID,
        createAttemptID: attemptID,
      }),
    );

    expect(response.status).toBe(201);
    expect(provision).toHaveBeenCalledOnce();
    const attemptWrite = writes.mock.calls.findIndex(
      ([key]) => key === `create-attempt:${requestedLeaseID}`,
    );
    const bindingWrite = writes.mock.calls.findIndex(
      ([key, value]) =>
        key.startsWith(`checkpoint-use:${checkpointID}:`) &&
        (value as { state?: string }).state === "provisioning",
    );
    expect(attemptWrite).toBeGreaterThanOrEqual(0);
    expect(bindingWrite).toBeGreaterThan(attemptWrite);
    expect(writes.mock.calls[attemptWrite]?.[1]).toMatchObject({
      checkpointID,
      checkpointUseClaimHash: await sha256Hex(acquired.claim),
    });
    expect(storage.snapshot()).not.toContain(acquired.claim);
  });

  it("rejects checkpoint adoption of an ordinary pending attempt before claim or provider work", async () => {
    const { coordinator, storage, provider } = await checkpointFixture();
    const checkpointID = "chk_reject_ordinary_attempt";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"2".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    const reservingCoordinator = coordinator as unknown as {
      reserveCreateAttempt(
        requestedLeaseID: string,
        token: string,
        owner: string,
        org: string,
        checkpointID?: string,
        checkpointUseClaimHash?: string,
      ): Promise<{ attempt: CreateAttemptRecord; replayLease?: LeaseRecord } | Response>;
    };
    const ordinary = await reservingCoordinator.reserveCreateAttempt(
      requestedLeaseID,
      attemptID,
      "alice@example.com",
      org,
    );
    expect(ordinary).not.toBeInstanceOf(Response);
    vi.mocked(provider.validateCheckpointLeaseScope).mockClear();
    vi.mocked(provider.validateCheckpointImage).mockClear();
    const provision = vi.spyOn(coordinator as never, "createLease" as never);

    const response = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: requestedLeaseID,
        createAttemptID: attemptID,
      }),
    );

    expect(response.status).toBe(409);
    await expect(response.json()).resolves.toMatchObject({
      error: "create_attempt_binding_conflict",
    });
    expect(provider.validateCheckpointLeaseScope).not.toHaveBeenCalled();
    expect(provider.validateCheckpointImage).not.toHaveBeenCalled();
    expect(provision).not.toHaveBeenCalled();
    expect([
      ...(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).values(),
    ]).toEqual([expect.objectContaining({ state: "available" })]);
    const attempt = await storage.get<CreateAttemptRecord>(`create-attempt:${requestedLeaseID}`);
    expect(attempt?.checkpointID).toBeUndefined();
    expect(attempt?.checkpointUseClaimHash).toBeUndefined();
    await expect(
      reservingCoordinator.reserveCreateAttempt(
        requestedLeaseID,
        attemptID,
        "alice@example.com",
        org,
      ),
    ).resolves.toEqual(ordinary);
  });

  it("backfills an in-flight legacy attempt only from its exact provisioning claim", async () => {
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_legacy_inflight_attempt";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"6".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    const provision = vi
      .spyOn(coordinator as never, "createLease" as never)
      .mockImplementation(async () => {
        const attempt = (await storage.get<CreateAttemptRecord>(
          `create-attempt:${requestedLeaseID}`,
        ))!;
        await storage.put(`create-attempt:${requestedLeaseID}`, {
          ...attempt,
          cloudID: "i-potentially-surviving",
        });
        return Response.json({ error: "retry_later" }, { status: 503 });
      });
    const body = {
      provider: "aws",
      awsSnapshot: "snap-000000000001",
      checkpointID,
      checkpointUseClaim: acquired.claim,
      leaseID: requestedLeaseID,
      createAttemptID: attemptID,
    };
    expect(
      (await coordinator.fetch(checkpointRequest("POST", "/v1/leases/from-checkpoint", body)))
        .status,
    ).toBe(503);
    const attempt = (await storage.get<CreateAttemptRecord>(`create-attempt:${requestedLeaseID}`))!;
    const { checkpointID: _checkpoint, checkpointUseClaimHash: _hash, ...legacyAttempt } = attempt;
    void _checkpoint;
    void _hash;
    await storage.put(`create-attempt:${requestedLeaseID}`, legacyAttempt);
    const eventsBefore = await storage.list({ prefix: `checkpoint-event:${checkpointID}:` });

    const retry = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", body),
    );

    expect(retry.status).toBe(409);
    await expect(retry.json()).resolves.toMatchObject({ error: "checkpoint_in_use" });
    expect(provision).toHaveBeenCalledOnce();
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      checkpointID,
      checkpointUseClaimHash: await sha256Hex(acquired.claim),
    });
    expect([
      ...(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).values(),
    ]).toEqual([
      expect.objectContaining({ state: "provisioning", attemptID, leaseID: requestedLeaseID }),
    ]);
    expect(await storage.list({ prefix: `checkpoint-event:${checkpointID}:` })).toEqual(
      eventsBefore,
    );
  });

  it.each([
    { label: "missing claim", missing: true },
    { label: "available claim", claim: { state: "available" } },
    { label: "foreign claim checkpoint", claim: { checkpointID: "chk_other_claim_evidence" } },
    { label: "foreign claim hash", claim: { tokenHash: "a".repeat(64) } },
    { label: "foreign claim owner", claim: { owner: "other@example.com" } },
    { label: "foreign claim organization", claim: { org: "other-org" } },
    { label: "wrong claim generation", claim: { generation: 99 } },
    { label: "wrong claim attempt", claim: { attemptID: `cat_${"8".repeat(32)}` } },
    { label: "wrong claim lease", claim: { leaseID: "cbx_000000000003" } },
  ])("rejects legacy attempt adoption with a $label before provider work", async (test) => {
    const { coordinator, storage, provider } = await checkpointFixture();
    const checkpointID = "chk_legacy_claim_evidence";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"7".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    const claimHash = await sha256Hex(acquired.claim);
    const claimKey = `checkpoint-use:${checkpointID}:${claimHash}`;
    const claim = (await storage.get<CoordinatorCheckpointUseClaim>(claimKey))!;
    if (test.missing) {
      await storage.delete(claimKey);
    } else {
      await storage.put(claimKey, {
        ...claim,
        state: "provisioning",
        attemptID,
        leaseID: requestedLeaseID,
        ...test.claim,
      });
    }
    const now = new Date().toISOString();
    await storage.put(`create-attempt:${requestedLeaseID}`, {
      version: 1,
      requestedLeaseID,
      token: attemptID,
      owner: "alice@example.com",
      org,
      state: "pending",
      createdAt: now,
      updatedAt: now,
    } satisfies CreateAttemptRecord);
    vi.mocked(provider.validateCheckpointLeaseScope).mockClear();
    vi.mocked(provider.validateCheckpointImage).mockClear();
    const provision = vi.spyOn(coordinator as never, "createLease" as never);

    const response = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: requestedLeaseID,
        createAttemptID: attemptID,
      }),
    );

    expect(response.status).toBe(409);
    await expect(response.json()).resolves.toMatchObject({
      error: "create_attempt_binding_conflict",
    });
    expect(provider.validateCheckpointLeaseScope).not.toHaveBeenCalled();
    expect(provider.validateCheckpointImage).not.toHaveBeenCalled();
    expect(provision).not.toHaveBeenCalled();
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).not.toHaveProperty(
      "checkpointID",
    );
  });

  it.each([
    { label: "missing checkpoint identity", lease: { checkpointID: undefined } },
    { label: "foreign checkpoint", lease: { checkpointID: "chk_foreign_legacy_lease" } },
    { label: "wrong generation", lease: { createAttemptGeneration: "foreign-generation" } },
    { label: "wrong owner", lease: { owner: "other@example.com" } },
    { label: "wrong organization", lease: { org: orgKeyForLabel("other-org") } },
    { label: "wrong attempt", lease: { createAttemptID: `cat_${"a".repeat(32)}` } },
    { label: "wrong cloud resource", lease: { cloudID: "i-foreign-resource" } },
  ])("rejects a completed legacy replay with $label before provider work", async (test) => {
    const { coordinator, storage, provider } = await checkpointFixture();
    const checkpointID = "chk_legacy_lease_evidence";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"9".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    await storage.delete(`checkpoint-use:${checkpointID}:${await sha256Hex(acquired.claim)}`);
    const lease: LeaseRecord = {
      ...checkpointLease("aws"),
      id: requestedLeaseID,
      checkpointID,
      createAttemptID: attemptID,
      createAttemptGeneration: "legacy-generation",
      ...test.lease,
    };
    await storage.put(`lease:${requestedLeaseID}`, lease);
    await storage.put(`create-attempt:${requestedLeaseID}`, {
      version: 1,
      requestedLeaseID,
      token: attemptID,
      owner: "alice@example.com",
      org,
      state: "pending",
      canonicalLeaseID: requestedLeaseID,
      cloudID: "i-000000000001",
      generation: "legacy-generation",
      createdAt: lease.createdAt,
      updatedAt: lease.updatedAt,
    } satisfies CreateAttemptRecord);
    vi.mocked(provider.validateCheckpointLeaseScope).mockClear();
    vi.mocked(provider.validateCheckpointImage).mockClear();
    const provision = vi.spyOn(coordinator as never, "createLease" as never);

    const replay = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: requestedLeaseID,
        createAttemptID: attemptID,
      }),
    );

    expect(replay.status).toBe(409);
    await expect(replay.json()).resolves.toMatchObject({
      error: "create_attempt_binding_conflict",
    });
    expect(provider.validateCheckpointLeaseScope).not.toHaveBeenCalled();
    expect(provider.validateCheckpointImage).not.toHaveBeenCalled();
    expect(provision).not.toHaveBeenCalled();
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).not.toHaveProperty(
      "checkpointID",
    );
  });

  it.each([
    { label: "checkpoint without claim hash", checkpointID: "chk_partial_binding" },
    { label: "claim hash without checkpoint", checkpointUseClaimHash: "a".repeat(64) },
    {
      label: "invalid claim hash",
      checkpointID: "chk_partial_binding",
      checkpointUseClaimHash: "not-a-sha256-hash",
    },
    {
      label: "invalid checkpoint identifier",
      checkpointID: "not-a-checkpoint",
      checkpointUseClaimHash: "a".repeat(64),
    },
  ])("rejects a reservation with $label before writing an attempt", async (test) => {
    const { coordinator, storage } = await checkpointFixture();
    const requestedLeaseID = "cbx_000000000002";
    const reservingCoordinator = coordinator as unknown as {
      reserveCreateAttempt(
        requestedLeaseID: string,
        token: string,
        owner: string,
        org: string,
        checkpointID?: string,
        checkpointUseClaimHash?: string,
      ): Promise<{ attempt: CreateAttemptRecord; replayLease?: LeaseRecord } | Response>;
    };

    const result = await reservingCoordinator.reserveCreateAttempt(
      requestedLeaseID,
      `cat_${"3".repeat(32)}`,
      "alice@example.com",
      org,
      test.checkpointID,
      test.checkpointUseClaimHash,
    );

    expect(result).toBeInstanceOf(Response);
    const response = result as Response;
    expect(response.status).toBe(409);
    await expect(response.json()).resolves.toMatchObject({
      error: "create_attempt_binding_conflict",
    });
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toBeUndefined();
  });

  it("atomically binds a checkpoint reservation when replacing a canceled ordinary attempt", async () => {
    const { coordinator, storage } = await checkpointFixture();
    const requestedLeaseID = "cbx_000000000002";
    const canceledAttemptID = `cat_${"4".repeat(32)}`;
    const checkpointAttemptID = `cat_${"5".repeat(32)}`;
    const checkpointID = "chk_replaced_ordinary_attempt";
    const checkpointUseClaimHash = "a".repeat(64);
    const canceled = await coordinator.fetch(
      checkpointRequest("POST", `/v1/leases/${requestedLeaseID}/cancel-create`, {
        createAttemptID: canceledAttemptID,
      }),
    );
    expect(canceled.status).toBe(200);
    const writes = vi.spyOn(storage, "put");
    const reservingCoordinator = coordinator as unknown as {
      reserveCreateAttempt(
        requestedLeaseID: string,
        token: string,
        owner: string,
        org: string,
        checkpointID?: string,
        checkpointUseClaimHash?: string,
      ): Promise<{ attempt: CreateAttemptRecord; replayLease?: LeaseRecord } | Response>;
    };

    const reserved = await reservingCoordinator.reserveCreateAttempt(
      requestedLeaseID,
      checkpointAttemptID,
      "alice@example.com",
      org,
      checkpointID,
      checkpointUseClaimHash,
    );

    expect(reserved).toMatchObject({
      attempt: {
        token: checkpointAttemptID,
        checkpointID,
        checkpointUseClaimHash,
      },
    });
    expect(
      writes.mock.calls.find(([key]) => key === `create-attempt:${requestedLeaseID}`)?.[1],
    ).toMatchObject({ checkpointID, checkpointUseClaimHash });
    expect(
      await storage.get(`create-attempt-canceled:${requestedLeaseID}:${canceledAttemptID}`),
    ).toMatchObject({ token: canceledAttemptID, state: "canceled" });
  });

  it("atomically binds concurrent checkpoint routes to exactly one checkpoint and use claim", async () => {
    const { coordinator, storage } = await checkpointFixture("azure");
    const checkpointIDs = ["chk_atomic_binding_first", "chk_atomic_binding_second"] as const;
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"a".repeat(32)}`;
    const requests = await Promise.all(
      checkpointIDs.map(async (checkpointID) => {
        expect((await createCheckpoint(coordinator, checkpointID)).status).toBe(201);
        const checkpoint = (await storage.get<CoordinatorCheckpointRecord>(
          checkpointKey(checkpointID),
        ))!;
        const acquired = (await (
          await coordinator.fetch(
            checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
          )
        ).json()) as { claim: string };
        return {
          provider: "azure",
          azureSnapshot: checkpoint.image!.id,
          checkpointID,
          checkpointUseClaim: acquired.claim,
          leaseID: requestedLeaseID,
          createAttemptID: attemptID,
        };
      }),
    );
    const provision = vi
      .spyOn(coordinator as never, "createLease" as never)
      .mockImplementation(async () => {
        const attempt = (await storage.get<CreateAttemptRecord>(
          `create-attempt:${requestedLeaseID}`,
        ))!;
        const lease: LeaseRecord = {
          ...checkpointLease("azure"),
          id: requestedLeaseID,
          checkpointID: attempt.checkpointID,
          createAttemptID: attemptID,
          createAttemptGeneration: "atomic-checkpoint-generation",
        };
        await storage.put(`lease:${requestedLeaseID}`, lease);
        await storage.put(`create-attempt:${requestedLeaseID}`, {
          ...attempt,
          canonicalLeaseID: requestedLeaseID,
          cloudID: lease.cloudID,
          generation: lease.createAttemptGeneration,
        } satisfies CreateAttemptRecord);
        return Response.json({ lease }, { status: 201 });
      });

    const responses = await Promise.all(
      requests.map(
        async (body) =>
          await coordinator.fetch(checkpointRequest("POST", "/v1/leases/from-checkpoint", body)),
      ),
    );

    expect(responses.map((response) => response.status).toSorted()).toEqual([201, 409]);
    expect(provision).toHaveBeenCalledOnce();
    const winnerIndex = responses.findIndex((response) => response.status === 201);
    const loserIndex = winnerIndex === 0 ? 1 : 0;
    const winning = requests[winnerIndex]!;
    const losing = requests[loserIndex]!;
    await expect(responses[loserIndex]!.json()).resolves.toMatchObject({
      error: "create_attempt_binding_conflict",
    });
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      checkpointID: winning.checkpointID,
      checkpointUseClaimHash: await sha256Hex(winning.checkpointUseClaim),
    });
    expect(await storage.list({ prefix: `checkpoint-use:${winning.checkpointID}:` })).toHaveLength(
      0,
    );
    expect([
      ...(await storage.list({ prefix: `checkpoint-use:${losing.checkpointID}:` })).values(),
    ]).toEqual([expect.objectContaining({ state: "available" })]);

    const crossCheckpointReplay = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", losing),
    );
    expect(crossCheckpointReplay.status).toBe(409);
    await expect(crossCheckpointReplay.json()).resolves.toMatchObject({
      error: "create_attempt_binding_conflict",
    });
    const sameClaimReplay = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", winning),
    );
    expect(sameClaimReplay.status).toBe(200);
    expect(provision).toHaveBeenCalledOnce();
    provision.mockRestore();
    const ordinaryReplay = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases", {
        provider: "azure",
        leaseID: requestedLeaseID,
        createAttemptID: attemptID,
      }),
    );
    expect(ordinaryReplay.status).toBe(409);
    await expect(ordinaryReplay.json()).resolves.toMatchObject({
      error: "create_attempt_binding_conflict",
    });
    expect(
      (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${losing.checkpointID}/use`, {
            action: "abort",
            claim: losing.checkpointUseClaim,
          }),
        )
      ).status,
    ).toBe(200);
    expect(storage.snapshot()).not.toContain(winning.checkpointUseClaim);
    expect(storage.snapshot()).not.toContain(losing.checkpointUseClaim);
  });

  it("rejects another use-claim token for an attempt already bound to the same checkpoint", async () => {
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_exact_attempt_claim";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"b".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const claims = await Promise.all(
      Array.from({ length: 2 }, async () => {
        const response = await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        );
        return ((await response.json()) as { claim: string }).claim;
      }),
    );
    const provision = vi
      .spyOn(coordinator as never, "createLease" as never)
      .mockImplementation(async () => {
        const attempt = (await storage.get<CreateAttemptRecord>(
          `create-attempt:${requestedLeaseID}`,
        ))!;
        await storage.put(`create-attempt:${requestedLeaseID}`, {
          ...attempt,
          cloudID: "i-potentially-surviving",
        });
        return Response.json({ error: "retry_later" }, { status: 503 });
      });
    const body = {
      provider: "aws",
      awsSnapshot: "snap-000000000001",
      checkpointID,
      leaseID: requestedLeaseID,
      createAttemptID: attemptID,
    };

    expect(
      (
        await coordinator.fetch(
          checkpointRequest("POST", "/v1/leases/from-checkpoint", {
            ...body,
            checkpointUseClaim: claims[0],
          }),
        )
      ).status,
    ).toBe(503);
    const conflicting = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        ...body,
        checkpointUseClaim: claims[1],
      }),
    );

    expect(conflicting.status).toBe(409);
    await expect(conflicting.json()).resolves.toMatchObject({
      error: "create_attempt_binding_conflict",
    });
    expect(provision).toHaveBeenCalledOnce();
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      checkpointID,
      checkpointUseClaimHash: await sha256Hex(claims[0]!),
    });
    expect(
      [...(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).values()].map(
        (claim) => (claim as { state: string }).state,
      ),
    ).toEqual(expect.arrayContaining(["available", "provisioning"]));
    expect(storage.snapshot()).not.toContain(claims[0]!);
    expect(storage.snapshot()).not.toContain(claims[1]!);
  });

  it.each([
    { label: "missing", missing: true, error: "create_attempt_conflict" },
    {
      label: "wrong requested lease",
      requestedLeaseID: "cbx_000000000003",
      error: "create_attempt_conflict",
    },
    {
      label: "wrong attempt token",
      token: `cat_${"d".repeat(32)}`,
      error: "create_attempt_conflict",
    },
    { label: "wrong owner", owner: "other@example.com", error: "create_attempt_conflict" },
    {
      label: "wrong organization",
      org: orgKeyForLabel("other-org"),
      error: "create_attempt_conflict",
    },
    { label: "canceled", state: "canceled", error: "create_canceled" },
    { label: "legacy unbound", error: "create_attempt_binding_conflict" },
    {
      label: "partially checkpoint-bound",
      checkpointID: "chk_transaction_attempt_validation",
      error: "create_attempt_binding_conflict",
    },
    {
      label: "partially claim-bound",
      checkpointUseClaimHash: "a".repeat(64),
      error: "create_attempt_binding_conflict",
    },
    {
      label: "different checkpoint binding",
      checkpointID: "chk_other_transaction_binding",
      checkpointUseClaimHash: "a".repeat(64),
      error: "create_attempt_binding_conflict",
    },
    {
      label: "different claim binding",
      checkpointID: "chk_transaction_attempt_validation",
      checkpointUseClaimHash: "b".repeat(64),
      error: "create_attempt_binding_conflict",
    },
  ])("rejects a $label create attempt inside the checkpoint binding transaction", async (test) => {
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_transaction_attempt_validation";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"c".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    if (!test.missing) {
      const now = new Date().toISOString();
      await storage.put(`create-attempt:${requestedLeaseID}`, {
        version: 1,
        requestedLeaseID: test.requestedLeaseID ?? requestedLeaseID,
        token: test.token ?? attemptID,
        owner: test.owner ?? "alice@example.com",
        org: test.org ?? org,
        state: test.state ?? "pending",
        ...(test.checkpointID ? { checkpointID: test.checkpointID } : {}),
        ...(test.checkpointUseClaimHash
          ? { checkpointUseClaimHash: test.checkpointUseClaimHash }
          : {}),
        createdAt: now,
        updatedAt: now,
      });
    }
    const before = storage.snapshot();

    await expect(
      bindCheckpointUseProvisioningTransaction(
        storage,
        checkpointID,
        acquired.claim,
        { owner: "alice@example.com", org },
        attemptID,
        requestedLeaseID,
      ),
    ).rejects.toMatchObject({ code: test.error });

    expect(storage.snapshot()).toBe(before);
    expect([
      ...(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).values(),
    ]).toEqual([expect.objectContaining({ state: "available" })]);
  });

  it("preserves hashed checkpoint binding through create-attempt cancellation and archival", async () => {
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_canceled_attempt_binding";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"d".repeat(32)}`;
    const replacementAttemptID = `cat_${"e".repeat(32)}`;
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
      attemptID,
      requestedLeaseID,
    );
    const binding = {
      checkpointID,
      checkpointUseClaimHash: await sha256Hex(acquired.claim),
    };

    const canceled = await coordinator.fetch(
      checkpointRequest("POST", `/v1/leases/${requestedLeaseID}/cancel-create`, {
        createAttemptID: attemptID,
      }),
    );
    expect(canceled.status).toBe(200);
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      ...binding,
      state: "canceled",
    });
    expect(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).toHaveLength(0);

    const replacement = await coordinator.fetch(
      checkpointRequest("POST", `/v1/leases/${requestedLeaseID}/cancel-create`, {
        createAttemptID: replacementAttemptID,
      }),
    );

    expect(replacement.status).toBe(200);
    expect(
      await storage.get(`create-attempt-canceled:${requestedLeaseID}:${attemptID}`),
    ).toMatchObject({
      ...binding,
      token: attemptID,
      state: "canceled",
    });
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      token: replacementAttemptID,
      state: "canceled",
    });
    expect(storage.snapshot()).not.toContain(acquired.claim);
  });

  it("retries an exact durable attempt after crashing between reservation and claim binding", async () => {
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_attempt_reservation_crash";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"2".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    const claimKey = [
      ...(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).keys(),
    ][0]!;
    const provision = vi
      .spyOn(coordinator as never, "createLease" as never)
      .mockResolvedValue(Response.json({ lease: {} }, { status: 201 }));
    const body = {
      provider: "aws",
      awsSnapshot: "snap-000000000001",
      checkpointID,
      checkpointUseClaim: acquired.claim,
      leaseID: requestedLeaseID,
      createAttemptID: attemptID,
    };
    storage.failKey = claimKey;

    const interrupted = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", body),
    );

    expect(interrupted.status).toBe(500);
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      requestedLeaseID,
      token: attemptID,
      owner: "alice@example.com",
      org,
      state: "pending",
    });
    expect(await storage.get(claimKey)).toMatchObject({ state: "available" });
    expect(provision).not.toHaveBeenCalled();
    storage.failKey = undefined;

    expect(
      (await coordinator.fetch(checkpointRequest("POST", "/v1/leases/from-checkpoint", body)))
        .status,
    ).toBe(201);
    expect(provision).toHaveBeenCalledOnce();
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
  });

  it("expires an unbound use claim safely after an attempt-reservation crash", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_unbound_attempt_expiry";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"7".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    const claimKey = [
      ...(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).keys(),
    ][0]!;
    storage.failKey = claimKey;
    const provision = vi.spyOn(coordinator as never, "createLease" as never);

    const interrupted = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: requestedLeaseID,
        createAttemptID: attemptID,
      }),
    );

    expect(interrupted.status).toBe(500);
    storage.failKey = undefined;
    vi.setSystemTime(Date.now() + checkpointUseClaimTTLMS + 1);
    await coordinator.alarm();

    expect(provision).not.toHaveBeenCalled();
    expect(await storage.get(claimKey)).toBeUndefined();
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      token: attemptID,
      state: "pending",
    });
  });

  it("does not bind a use claim when its durable attempt is canceled before provisioning", async () => {
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_attempt_canceled_before_binding";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"6".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    const reservingCoordinator = coordinator as unknown as {
      reserveCreateAttempt(
        requestedLeaseID: string,
        token: string,
        owner: string,
        org: string,
        checkpointID?: string,
        checkpointUseClaimHash?: string,
      ): Promise<{ attempt: unknown; replayLease?: LeaseRecord } | Response>;
    };
    const originalReserve = reservingCoordinator.reserveCreateAttempt.bind(coordinator);
    vi.spyOn(reservingCoordinator, "reserveCreateAttempt").mockImplementationOnce(
      async (requested, token, owner, organization, checkpoint, claimHash) => {
        const reserved = await originalReserve(
          requested,
          token,
          owner,
          organization,
          checkpoint,
          claimHash,
        );
        const canceled = await coordinator.fetch(
          checkpointRequest("POST", `/v1/leases/${requested}/cancel-create`, {
            createAttemptID: token,
          }),
        );
        expect(canceled.status).toBe(200);
        return reserved;
      },
    );
    const provision = vi.spyOn(coordinator as never, "createLease" as never);

    const response = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: requestedLeaseID,
        createAttemptID: attemptID,
      }),
    );

    expect(response.status).toBe(409);
    await expect(response.json()).resolves.toMatchObject({ error: "create_canceled" });
    expect(provision).not.toHaveBeenCalled();
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      token: attemptID,
      state: "canceled",
    });
    expect([
      ...(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).values(),
    ]).toEqual([expect.objectContaining({ state: "available" })]);
  });

  it.each([
    { label: "different token", token: `cat_${"4".repeat(32)}`, error: "create_attempt_conflict" },
    { label: "different owner", owner: "other@example.com", error: "create_attempt_conflict" },
    {
      label: "different organization",
      org: orgKeyForLabel("other-org"),
      error: "create_attempt_conflict",
    },
    { label: "canceled attempt", state: "canceled", error: "create_canceled" },
  ])("does not bind a checkpoint claim when reservation finds a $label", async (test) => {
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_attempt_conflict";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"3".repeat(32)}`;
    await createCheckpoint(coordinator, checkpointID);
    const acquired = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    const timestamp = new Date().toISOString();
    await storage.put(`create-attempt:${requestedLeaseID}`, {
      version: 1,
      requestedLeaseID,
      token: test.token ?? attemptID,
      owner: test.owner ?? "alice@example.com",
      org: test.org ?? org,
      state: test.state ?? "pending",
      checkpointID,
      checkpointUseClaimHash: await sha256Hex(acquired.claim),
      createdAt: timestamp,
      updatedAt: timestamp,
    });
    const provision = vi.spyOn(coordinator as never, "createLease" as never);

    const response = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", {
        provider: "aws",
        awsSnapshot: "snap-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: requestedLeaseID,
        createAttemptID: attemptID,
      }),
    );

    expect(response.status).toBe(409);
    await expect(response.json()).resolves.toMatchObject({ error: test.error });
    expect(provision).not.toHaveBeenCalled();
    expect([
      ...(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).values(),
    ]).toEqual([expect.objectContaining({ state: "available" })]);
  });

  it.each([
    { label: "matching", leaseCheckpointID: "chk_available_replay", expectedStatus: 200 },
    { label: "foreign", leaseCheckpointID: "chk_foreign_replay", expectedStatus: 409 },
  ])(
    "safely handles a $label replay while the checkpoint claim is still available",
    async (test) => {
      const { coordinator, storage } = await checkpointFixture();
      const checkpointID = "chk_available_replay";
      const requestedLeaseID = "cbx_000000000002";
      const attemptID = `cat_${"5".repeat(32)}`;
      await createCheckpoint(coordinator, checkpointID);
      const acquired = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        )
      ).json()) as { claim: string };
      const lease: LeaseRecord = {
        ...checkpointLease("aws"),
        id: requestedLeaseID,
        checkpointID: test.leaseCheckpointID,
        createAttemptID: attemptID,
        createAttemptGeneration: "replay-generation",
      };
      await storage.put(`lease:${requestedLeaseID}`, lease);
      await storage.put(`create-attempt:${requestedLeaseID}`, {
        version: 1,
        requestedLeaseID,
        token: attemptID,
        owner: lease.owner,
        org: lease.org,
        state: "pending",
        canonicalLeaseID: requestedLeaseID,
        cloudID: lease.cloudID,
        generation: lease.createAttemptGeneration,
        checkpointID,
        checkpointUseClaimHash: await sha256Hex(acquired.claim),
        createdAt: lease.createdAt,
        updatedAt: lease.updatedAt,
      });
      const provision = vi.spyOn(coordinator as never, "createLease" as never);

      const replay = await coordinator.fetch(
        checkpointRequest("POST", "/v1/leases/from-checkpoint", {
          provider: "aws",
          awsSnapshot: "snap-000000000001",
          checkpointID,
          checkpointUseClaim: acquired.claim,
          leaseID: requestedLeaseID,
          createAttemptID: attemptID,
        }),
      );

      expect(replay.status).toBe(test.expectedStatus);
      expect(provision).not.toHaveBeenCalled();
      expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({
        activeUseCount: test.expectedStatus === 200 ? 0 : 1,
      });
      await expect(replay.json()).resolves.toMatchObject(
        test.expectedStatus === 409
          ? { error: "create_attempt_binding_conflict" }
          : { lease: { id: requestedLeaseID } },
      );
      const claimStates = [
        ...(
          await storage.list<{ state: string }>({ prefix: `checkpoint-use:${checkpointID}:` })
        ).values(),
      ].map((claim) => claim.state);
      expect(claimStates).toEqual(test.expectedStatus === 409 ? ["available"] : []);
    },
  );

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
        const reserved = (await storage.get<CreateAttemptRecord>(`create-attempt:${lease.id}`))!;
        await storage.put(`lease:${lease.id}`, lease);
        await storage.put(`create-attempt:${lease.id}`, {
          ...reserved,
          canonicalLeaseID: lease.id,
          cloudID: lease.cloudID,
          generation: lease.createAttemptGeneration,
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
    const completedAttempt = (await storage.get<CreateAttemptRecord>(
      `create-attempt:${body.leaseID}`,
    ))!;
    const {
      checkpointID: _checkpoint,
      checkpointUseClaimHash: _hash,
      ...legacyAttempt
    } = completedAttempt;
    void _checkpoint;
    void _hash;
    await storage.put(`create-attempt:${body.leaseID}`, legacyAttempt);
    const eventsBefore = await storage.list({ prefix: "checkpoint-event:chk_completed_replay:" });
    const replayed = await coordinator.fetch(
      checkpointRequest("POST", "/v1/leases/from-checkpoint", body),
    );
    expect(replayed.ok).toBe(true);
    expect(provision).toHaveBeenCalledOnce();
    expect(await storage.get(`create-attempt:${body.leaseID}`)).toMatchObject({
      checkpointID: body.checkpointID,
      checkpointUseClaimHash: await sha256Hex(acquired.claim),
    });
    expect(await storage.list({ prefix: "checkpoint-event:chk_completed_replay:" })).toEqual(
      eventsBefore,
    );
    expect(storage.snapshot()).not.toContain(acquired.claim);
  });

  it.each(
    [false, true].flatMap((macOS) =>
      ["none", "deleting", "delete-pending", "deleted"].map((deletion) => ({ macOS, deletion })),
    ),
  )(
    "completes a reconciled fork and replays its canonical ID (macOS=$macOS deletion=$deletion)",
    async ({ macOS, deletion }) => {
      const { coordinator, storage } = await checkpointFixture();
      const checkpointID = "chk_reconciled_success";
      await createCheckpoint(coordinator, checkpointID, { mode: "manual" }, "image");
      const checkpoint = (await storage.get<CoordinatorCheckpointRecord>(
        checkpointKey(checkpointID),
      ))!;
      if (macOS) await storage.put(checkpointKey(checkpointID), { ...checkpoint, target: "macos" });
      const acquired = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        )
      ).json()) as { claim: string };
      const requestedLeaseID = "cbx_000000000002";
      const canonicalLeaseID = macOS ? "cbx_000000000099" : requestedLeaseID;
      const createAttemptID = `cat_${"8".repeat(32)}`;
      const provision = vi
        .spyOn(coordinator as never, "createLease" as never)
        .mockImplementation(async () => {
          const lease: LeaseRecord = {
            ...checkpointLease("aws"),
            id: canonicalLeaseID,
            target: macOS ? "macos" : "linux",
            checkpointID,
            createAttemptID,
            createAttemptGeneration: "reconciled-generation",
          };
          const attempt = (await storage.get<CreateAttemptRecord>(
            `create-attempt:${requestedLeaseID}`,
          ))!;
          await storage.put(`lease:${canonicalLeaseID}`, lease);
          await storage.put(`create-attempt:${requestedLeaseID}`, {
            ...attempt,
            canonicalLeaseID,
            cloudID: lease.cloudID,
            generation: lease.createAttemptGeneration,
          });
          await expireCheckpointClaims(storage, checkpointID);
          expect(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).toHaveLength(0);
          if (deletion !== "none") {
            const claimed = await claimCheckpointDeletion(
              storage,
              checkpointID,
              undefined,
              "manual",
            );
            if (deletion === "deleted") {
              await markCheckpointProviderDeleted(storage, checkpointID, claimed.token);
              await finalizeCheckpointDeletion(storage, checkpointID, claimed.token);
            } else if (deletion === "delete-pending") {
              await recordCheckpointDeletionFailure(storage, checkpointID, claimed.token, "retry");
            }
          }
          return Response.json({ lease }, { status: 201 });
        });
      const body = {
        provider: "aws",
        awsAMI: "ami-000000000001",
        checkpointID,
        checkpointUseClaim: acquired.claim,
        leaseID: requestedLeaseID,
        createAttemptID,
      };
      const response = await coordinator.fetch(
        checkpointRequest("POST", "/v1/leases/from-checkpoint", body),
      );
      expect(response.status).toBe(201);
      const completed = await storage.get(checkpointKey(checkpointID));
      const events = await storage.list({ prefix: `checkpoint-event:${checkpointID}:` });
      const replay = await coordinator.fetch(
        checkpointRequest("POST", "/v1/leases/from-checkpoint", body),
      );
      expect(replay.ok).toBe(true);
      await expect(replay.json()).resolves.toMatchObject({ lease: { id: canonicalLeaseID } });
      expect(provision).toHaveBeenCalledOnce();
      expect(await storage.get(checkpointKey(checkpointID))).toEqual(completed);
      expect(await storage.list({ prefix: `checkpoint-event:${checkpointID}:` })).toEqual(events);
    },
  );

  it.each(["complete", "reconcile"])(
    "recovers an AWS macOS canonical provisioning lease via %s",
    async (action) => {
      const { coordinator, storage } = await checkpointFixture();
      const checkpointID = "chk_canonical_recovery";
      await createCheckpoint(coordinator, checkpointID, { mode: "manual" }, "image");
      const checkpoint = (await storage.get<CoordinatorCheckpointRecord>(
        checkpointKey(checkpointID),
      ))!;
      await storage.put(checkpointKey(checkpointID), { ...checkpoint, target: "macos" });
      const acquired = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        )
      ).json()) as { claim: string };
      const requestedLeaseID = "cbx_000000000002",
        canonicalLeaseID = "cbx_000000000099";
      const attemptID = `cat_${"7".repeat(32)}`;
      await bindCheckpointUseProvisioning(
        storage,
        checkpointID,
        acquired.claim,
        { owner: "alice@example.com", org },
        attemptID,
        requestedLeaseID,
      );
      const lease: LeaseRecord = {
        ...checkpointLease("aws"),
        id: canonicalLeaseID,
        target: "macos",
        checkpointID,
        createAttemptID: attemptID,
        createAttemptGeneration: "canonical-generation",
      };
      const attempt = (await storage.get<CreateAttemptRecord>(
        `create-attempt:${requestedLeaseID}`,
      ))!;
      await storage.put(`lease:${canonicalLeaseID}`, lease);
      await storage.put(`create-attempt:${requestedLeaseID}`, {
        ...attempt,
        canonicalLeaseID,
        cloudID: lease.cloudID,
        generation: lease.createAttemptGeneration,
      });
      const completed =
        action === "complete"
          ? (
              await coordinator.fetch(
                checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, {
                  action: "complete",
                  claim: acquired.claim,
                }),
              )
            ).ok
          : Boolean(await expireCheckpointClaims(storage, checkpointID));
      expect(completed).toBe(true);
      expect(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).toHaveLength(0);
      expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
    },
  );

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

  it.each([400, 503])(
    "releases an administrator's rejected checkpoint fork claim on HTTP %i",
    async (status) => {
      const { coordinator, storage } = await checkpointFixture();
      const id = "chk_admin_rejected";
      await createCheckpoint(coordinator, id);
      const before = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(id)))!;
      const admin = { owner: "operator@example.com", admin: true };
      const acquired = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${id}/use`, { action: "begin" }, admin),
        )
      ).json()) as { claim: string };
      vi.spyOn(coordinator as never, "createLease" as never).mockImplementation(async () =>
        Response.json({ error: "preflight_rejected" }, { status }),
      );
      const rejectedLeaseID = "cbx_000000000002";
      const attemptID = `cat_${"d".repeat(32)}`;
      const response = await coordinator.fetch(
        checkpointRequest(
          "POST",
          "/v1/leases/from-checkpoint",
          {
            provider: "aws",
            awsSnapshot: "snap-000000000001",
            checkpointID: id,
            checkpointUseClaim: acquired.claim,
            leaseID: rejectedLeaseID,
            createAttemptID: attemptID,
          },
          admin,
        ),
      );
      expect(response.status).toBe(status);
      expect(await storage.get(`create-attempt:${rejectedLeaseID}`)).toMatchObject({
        state: "canceled",
        owner: admin.owner,
        token: attemptID,
      });
      expect(await storage.list({ prefix: `checkpoint-use:${id}:` })).toHaveLength(0);
      expect(await storage.get(checkpointKey(id))).toMatchObject({
        activeUseCount: 0,
        lastUsedAt: before.lastUsedAt,
      });
      expect(
        (
          await coordinator.fetch(
            checkpointRequest("DELETE", `/v1/checkpoints/${id}`, undefined, admin),
          )
        ).status,
      ).toBe(200);
    },
  );

  it("retains failed creation ownership across repeated inconclusive deletion attempts", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
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
    expect(await storage.list({ prefix: "checkpoint-due:" })).toHaveLength(1);
    const recovery = vi.spyOn(provider, "recoverCheckpointImage").mockResolvedValue(undefined);

    await Array.from({ length: 2 }).reduce(async (previous) => {
      await previous;
      const blocked = await coordinator.fetch(
        checkpointRequest("DELETE", "/v1/checkpoints/chk_failed_creation"),
      );
      expect(blocked.status).toBe(409);
      await expect(blocked.json()).resolves.toMatchObject({ error: "checkpoint_pending" });
      expect(await storage.get(checkpointKey("chk_failed_creation"))).toMatchObject({
        state: "failed",
        createClaim: {
          providerMutationStartedAt: "2026-08-20T12:00:00.000Z",
          providerAbsenceFirstObservedAt: "2026-08-20T12:00:00.000Z",
          providerAbsenceLastObservedAt: "2026-08-20T12:00:00.000Z",
        },
        retryAt: expect.any(String),
      });
      expect(await storage.list({ prefix: "checkpoint-intent:azure:" })).toHaveLength(1);
    }, Promise.resolve());

    expect(recovery).toHaveBeenCalledTimes(2);
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
    expect(await storage.list({ prefix: "checkpoint-intent:azure:" })).toHaveLength(0);
  });

  it("commits the provider mutation-start phase before invoking checkpoint creation", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, provider, storage } = await checkpointFixture("gcp");
    const checkpointID = "chk_durable_mutation_start";
    vi.mocked(provider.createCheckpointImage).mockImplementationOnce(
      async (_lease, name, _noReboot, strategy, ownership) => {
        expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({
          state: "creating",
          createClaim: {
            providerMutationPhase: "started",
            providerMutationStartedAt: "2026-08-20T12:00:00.000Z",
          },
        });
        expect(await storage.get(checkpointEventKey(checkpointID, 2))).toMatchObject({
          type: "checkpoint.create.provider_mutation_started",
        });
        return providerImage("gcp", name, strategy, ownership);
      },
    );

    expect((await createCheckpoint(coordinator, checkpointID)).status).toBe(201);
    expect(provider.createCheckpointImage).toHaveBeenCalledOnce();
  });

  it("cancels a pre-mutation crash immediately without consulting the provider", async () => {
    const { coordinator, provider, storage } = await checkpointFixture("azure");
    const checkpointID = "chk_pre_mutation_crash";
    storage.failKey = checkpointEventKey(checkpointID, 2);
    const recovery = vi.spyOn(provider, "recoverCheckpointImage");

    expect((await createCheckpoint(coordinator, checkpointID)).status).toBe(500);
    expect(provider.createCheckpointImage).not.toHaveBeenCalled();
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({
      state: "creating",
      createClaim: { tokenHash: expect.any(String), providerMutationPhase: "reserved" },
    });
    expect(
      (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))?.createClaim
        ?.providerMutationStartedAt,
    ).toBeUndefined();
    expect(await storage.list({ prefix: "checkpoint-intent:azure:" })).toHaveLength(1);
    storage.failKey = undefined;

    const canceled = await coordinator.fetch(
      checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`),
    );

    expect(canceled.status).toBe(200);
    expect(recovery).not.toHaveBeenCalled();
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ state: "deleted" });
    expect(await storage.list({ prefix: "checkpoint-intent:azure:" })).toHaveLength(0);
  });

  it("backfills only exhausted legacy failures across bounded tombstone pages exactly once", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, provider, storage } = await checkpointFixture("azure");
    const checkpointID = "chk_zzz_exhausted_legacy";
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(new Error("ambiguous create"));
    expect((await createCheckpoint(coordinator, checkpointID)).status).toBe(503);
    const original = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
    await storage.delete(checkpointDueKey(checkpointID, original.nextSweepAt!));
    const { retryAt: _retryAt, nextSweepAt: _nextSweepAt, ...withoutRetry } = original;
    void _retryAt;
    void _nextSweepAt;
    const {
      providerMutationPhase: _phase,
      providerMutationStartedAt: _started,
      ...legacyClaim
    } = original.createClaim!;
    void _phase;
    void _started;
    const legacy = {
      ...withoutRetry,
      state: "failed",
      attempts: checkpointMaxCreateRecoveryAttempts,
      createClaim: legacyClaim,
    } satisfies CoordinatorCheckpointRecord;
    await storage.put(checkpointKey(checkpointID), legacy);
    await Promise.all(
      Array.from({ length: 145 }, async (_, index) => {
        const id = `chk_${String(index).padStart(3, "0")}_deleted`;
        await storage.put(checkpointKey(id), {
          ...legacy,
          id,
          state: "deleted",
          deletedAt: legacy.createdAt,
        } satisfies CoordinatorCheckpointRecord);
      }),
    );
    const exclusions: Array<{
      id: string;
      state?: CoordinatorCheckpointRecord["state"];
      createClaim?: Partial<NonNullable<CoordinatorCheckpointRecord["createClaim"]>>;
      image?: boolean;
      retryAt?: string;
    }> = [
      { id: "chk_za_reserved", createClaim: { providerMutationPhase: "reserved" } },
      { id: "chk_zb_refused", createClaim: { definitiveRefusal: true } },
      { id: "chk_zc_verified", createClaim: { providerAbsenceVerifiedAt: legacy.createdAt } },
      { id: "chk_zd_creating", state: "creating" },
      { id: "chk_ze_ready", state: "ready" },
      { id: "chk_zf_delete_pending", state: "delete-pending" },
      { id: "chk_zg_deleting", state: "deleting" },
      { id: "chk_zh_failed_image", image: true },
      { id: "chk_zi_scheduled", retryAt: "2026-08-20T12:05:00.000Z" },
    ];
    await Promise.all(
      exclusions.map(async (excluded) => {
        const image = excluded.image
          ? {
              id: excluded.id,
              resourceID: excluded.id,
              kind: "azure-os-disk-snapshot",
              immutableID: excluded.id,
              snapshotIDs: [],
              state: "available",
            }
          : undefined;
        await storage.put(checkpointKey(excluded.id), {
          ...legacy,
          id: excluded.id,
          state: excluded.state ?? "failed",
          createClaim: { ...legacyClaim, ...excluded.createClaim },
          ...(image ? { image } : {}),
          ...(excluded.retryAt ? { retryAt: excluded.retryAt } : {}),
        } satisfies CoordinatorCheckpointRecord);
      }),
    );
    const before = new Map(
      await Promise.all(
        exclusions.map(async ({ id }) => [id, await storage.get(checkpointKey(id))] as const),
      ),
    );
    storage.lists.length = 0;

    expect(
      await backfillFailedCheckpointCreateRecovery(
        storage,
        checkpointLimits({ CRABBOX_MAX_CHECKPOINTS: "16" }),
      ),
    ).toBe(1);
    const recovered = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey(checkpointID),
    ))!;
    expect(recovered).toMatchObject({
      state: "failed",
      attempts: checkpointMaxCreateRecoveryAttempts,
      retryAt: "2026-08-20T12:00:00.000Z",
      nextSweepAt: "2026-08-20T12:00:00.000Z",
      createClaim: {
        providerMutationPhase: "started",
        providerMutationStartedAt: legacy.createdAt,
      },
    });
    expect(await storage.get(checkpointDueKey(checkpointID, recovered.retryAt!))).toMatchObject({
      checkpointID,
      revision: recovered.revision,
    });
    const pages = storage.lists.filter((entry) => entry.prefix === "checkpoint:");
    expect(pages.length).toBeGreaterThan(9);
    expect(pages.every((page) => page.limit === 16)).toBe(true);
    expect(pages.slice(1).every((page) => page.startAfter)).toBe(true);
    await Promise.all(
      [...before].map(async ([id, record]) => {
        expect(await storage.get(checkpointKey(id))).toEqual(record);
      }),
    );
    const snapshot = storage.snapshot();

    expect(
      await backfillFailedCheckpointCreateRecovery(
        storage,
        checkpointLimits({ CRABBOX_MAX_CHECKPOINTS: "16" }),
      ),
    ).toBe(0);
    expect(storage.snapshot()).toBe(snapshot);
    const events = await storage.list<{ type: string }>({
      prefix: `checkpoint-event:${checkpointID}:`,
    });
    expect(
      [...events.values()].filter(
        (event) => event.type === "checkpoint.create.recovery.backfilled",
      ),
    ).toHaveLength(1);
  });

  it.each(["creating", "failed"] as const)(
    "recovers a legacy %s checkpoint without mistaking its missing mutation phase for a reservation",
    async (state) => {
      vi.useFakeTimers();
      const createdAt = Date.parse("2026-08-20T12:00:00.000Z");
      vi.setSystemTime(createdAt);
      const { coordinator, provider, storage } = await checkpointFixture("azure");
      const checkpointID = `chk_legacy_${state}_mutation`;
      vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(
        new Error("ambiguous create"),
      );
      expect((await createCheckpoint(coordinator, checkpointID)).status).toBe(503);
      const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
      const legacyClaim = { ...record.createClaim! };
      delete legacyClaim.providerMutationPhase;
      delete legacyClaim.providerMutationStartedAt;
      await storage.put(checkpointKey(checkpointID), {
        ...record,
        state,
        createClaim: legacyClaim,
      } satisfies CoordinatorCheckpointRecord);
      const recovery = vi.spyOn(provider, "recoverCheckpointImage").mockResolvedValue(undefined);

      vi.setSystemTime(createdAt + 30 * 60_000);
      const deletion = await coordinator.fetch(
        checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`),
      );

      expect(deletion.status).toBe(409);
      await expect(deletion.json()).resolves.toMatchObject({ error: "checkpoint_pending" });
      expect(recovery).toHaveBeenCalledOnce();
      expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({
        state,
        createClaim: {
          providerMutationPhase: "started",
          providerMutationStartedAt: "2026-08-20T12:00:00.000Z",
          providerAbsenceFirstObservedAt: "2026-08-20T12:30:00.000Z",
        },
      });
      expect(await storage.list({ prefix: "checkpoint-intent:azure:" })).toHaveLength(1);
    },
  );

  it("anchors legacy absence confirmations to checkpoint creation without resetting the horizon", async () => {
    vi.useFakeTimers();
    const createdAt = Date.parse("2026-08-20T12:00:00.000Z");
    vi.setSystemTime(createdAt);
    const { coordinator, provider, storage } = await checkpointFixture("azure");
    const checkpointID = "chk_legacy_created_at_horizon";
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(new Error("ambiguous create"));
    expect((await createCheckpoint(coordinator, checkpointID)).status).toBe(503);
    const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
    const legacyClaim = {
      ...record.createClaim!,
      providerMutationStartedAt: "2026-08-20T12:30:00.000Z",
    };
    delete legacyClaim.providerMutationPhase;
    await storage.put(checkpointKey(checkpointID), {
      ...record,
      state: "failed",
      createClaim: legacyClaim,
    } satisfies CoordinatorCheckpointRecord);
    const recovery = vi.spyOn(provider, "recoverCheckpointImage").mockResolvedValue(undefined);

    vi.setSystemTime(createdAt + checkpointProviderAbsenceHorizonMS);
    const first = await coordinator.fetch(
      checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`),
    );
    expect(first.status).toBe(409);
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({
      state: "failed",
      createClaim: {
        providerMutationPhase: "started",
        providerMutationStartedAt: "2026-08-20T12:00:00.000Z",
        providerAbsenceFirstObservedAt: "2026-08-20T13:00:00.000Z",
      },
      retryAt: "2026-08-20T13:05:00.000Z",
    });

    vi.setSystemTime(
      createdAt +
        checkpointProviderAbsenceHorizonMS +
        checkpointProviderAbsenceConfirmationIntervalMS,
    );
    const confirmed = await coordinator.fetch(
      checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`),
    );

    expect(confirmed.status).toBe(200);
    expect(recovery).toHaveBeenCalledTimes(2);
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ state: "deleted" });
  });

  it("maintains a failed legacy checkpoint until its exact provider resource eventually appears", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, provider, storage } = await checkpointFixture("gcp");
    const checkpointID = "chk_legacy_eventual_provider_resource";
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(new Error("ambiguous create"));
    expect((await createCheckpoint(coordinator, checkpointID)).status).toBe(503);
    const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
    const legacyClaim = { ...record.createClaim! };
    delete legacyClaim.providerMutationPhase;
    delete legacyClaim.providerMutationStartedAt;
    await storage.delete(checkpointDueKey(checkpointID, record.nextSweepAt!));
    const { retryAt: _retry, nextSweepAt: _due, ...exhausted } = record;
    void _retry;
    void _due;
    await storage.put(checkpointKey(checkpointID), {
      ...exhausted,
      state: "failed",
      attempts: checkpointMaxCreateRecoveryAttempts,
      createClaim: legacyClaim,
    } satisfies CoordinatorCheckpointRecord);
    const recovery = vi
      .spyOn(provider, "recoverCheckpointImage")
      .mockResolvedValueOnce(undefined)
      .mockImplementationOnce(async (checkpoint) =>
        providerImage("gcp", checkpoint.createClaim!.resourceName, checkpoint.strategy, {
          checkpointID: checkpoint.id,
          tokenHash: checkpoint.createClaim!.tokenHash,
          sourceLeaseID: checkpoint.leaseID,
        }),
      );

    vi.setSystemTime(Date.parse(record.retryAt!) + 1);
    await coordinator.alarm();
    const recovering = (await storage.get<CoordinatorCheckpointRecord>(
      checkpointKey(checkpointID),
    ))!;
    expect(recovering).toMatchObject({
      state: "failed",
      createClaim: {
        providerMutationPhase: "started",
        providerMutationStartedAt: record.createdAt,
      },
      retryAt: expect.any(String),
    });

    vi.setSystemTime(Date.parse(recovering.retryAt!) + 1);
    await coordinator.alarm();

    expect(recovery).toHaveBeenCalledTimes(2);
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({
      state: "ready",
      image: { id: legacyClaim.resourceName },
    });
  });

  it("reschedules an exhausted unindexed legacy failure through verified provider absence", async () => {
    vi.useFakeTimers();
    const createdAt = Date.parse("2026-08-20T12:00:00.000Z");
    vi.setSystemTime(createdAt);
    const { coordinator, provider, storage } = await checkpointFixture("gcp");
    const checkpointID = "chk_legacy_unindexed_absence";
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(new Error("ambiguous create"));
    expect((await createCheckpoint(coordinator, checkpointID)).status).toBe(503);
    const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
    await storage.delete(checkpointDueKey(checkpointID, record.nextSweepAt!));
    const { retryAt: _retry, nextSweepAt: _due, ...exhausted } = record;
    void _retry;
    void _due;
    const {
      providerMutationPhase: _phase,
      providerMutationStartedAt: _started,
      ...legacyClaim
    } = record.createClaim!;
    void _phase;
    void _started;
    await storage.put(checkpointKey(checkpointID), {
      ...exhausted,
      state: "failed",
      attempts: checkpointMaxCreateRecoveryAttempts,
      createClaim: legacyClaim,
    } satisfies CoordinatorCheckpointRecord);
    const recovery = vi.spyOn(provider, "recoverCheckpointImage").mockResolvedValue(undefined);

    vi.setSystemTime(createdAt + checkpointProviderAbsenceHorizonMS);
    await coordinator.alarm();
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({
      createClaim: {
        providerMutationPhase: "started",
        providerMutationStartedAt: record.createdAt,
        providerAbsenceFirstObservedAt: "2026-08-20T13:00:00.000Z",
      },
      retryAt: "2026-08-20T13:05:00.000Z",
    });

    vi.setSystemTime(
      createdAt +
        checkpointProviderAbsenceHorizonMS +
        checkpointProviderAbsenceConfirmationIntervalMS,
    );
    await coordinator.alarm();

    expect(recovery).toHaveBeenCalledTimes(2);
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({
      state: "failed",
      createClaim: { providerAbsenceVerifiedAt: "2026-08-20T13:05:00.000Z" },
    });
    expect(await storage.list({ prefix: "checkpoint-due:" })).toHaveLength(0);
  });

  it("requires two separated post-horizon absence confirmations before canceling a started mutation", async () => {
    vi.useFakeTimers();
    const startedAt = Date.parse("2026-08-20T12:00:00.000Z");
    vi.setSystemTime(startedAt);
    const { coordinator, provider, storage, runtime } = await checkpointFixture("azure");
    const checkpointID = "chk_confirmed_provider_absence";
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(new Error("ambiguous create"));
    expect((await createCheckpoint(coordinator, checkpointID)).status).toBe(503);
    const recovery = vi.spyOn(provider, "recoverCheckpointImage").mockResolvedValue(undefined);
    const deletion = async () =>
      await coordinator.fetch(checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`));

    vi.setSystemTime(startedAt + 10 * 60_000);
    expect((await deletion()).status).toBe(409);
    const early = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
    expect(early.createClaim).toMatchObject({
      providerMutationStartedAt: "2026-08-20T12:00:00.000Z",
      providerAbsenceFirstObservedAt: "2026-08-20T12:10:00.000Z",
      providerAbsenceLastObservedAt: "2026-08-20T12:10:00.000Z",
    });
    expect(early.createClaim?.providerAbsenceVerifiedAt).toBeUndefined();
    expect(early.retryAt).toBe("2026-08-20T12:15:00.000Z");
    expect(runtime.alarmTime).toBe(Date.parse(early.retryAt!));

    vi.setSystemTime(startedAt + checkpointProviderAbsenceHorizonMS);
    expect((await deletion()).status).toBe(409);
    const first = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
    expect(first.createClaim).toMatchObject({
      providerAbsenceFirstObservedAt: "2026-08-20T13:00:00.000Z",
      providerAbsenceLastObservedAt: "2026-08-20T13:00:00.000Z",
    });
    expect(first.createClaim?.providerAbsenceVerifiedAt).toBeUndefined();
    expect(first.retryAt).toBe("2026-08-20T13:05:00.000Z");
    expect(await storage.list({ prefix: "checkpoint-intent:azure:" })).toHaveLength(1);

    vi.setSystemTime(
      startedAt +
        checkpointProviderAbsenceHorizonMS +
        checkpointProviderAbsenceConfirmationIntervalMS -
        1,
    );
    expect((await deletion()).status).toBe(409);
    const tooClose = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
    expect(tooClose.createClaim).toMatchObject({
      providerAbsenceFirstObservedAt: "2026-08-20T13:00:00.000Z",
      providerAbsenceLastObservedAt: "2026-08-20T13:04:59.999Z",
    });
    expect(tooClose.createClaim?.providerAbsenceVerifiedAt).toBeUndefined();
    expect(tooClose.retryAt).toBe("2026-08-20T13:05:00.000Z");

    vi.setSystemTime(
      startedAt +
        checkpointProviderAbsenceHorizonMS +
        checkpointProviderAbsenceConfirmationIntervalMS,
    );
    const canceled = await deletion();

    expect(canceled.status).toBe(200);
    expect(recovery).toHaveBeenCalledTimes(4);
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ state: "deleted" });
    expect(await storage.list({ prefix: "checkpoint-intent:azure:" })).toHaveLength(0);
    const events = [
      ...(
        await storage.list<{ type: string }>({ prefix: `checkpoint-event:${checkpointID}:` })
      ).values(),
    ];
    expect(events.map((event) => event.type)).toEqual(
      expect.arrayContaining([
        "checkpoint.create.absence_observed",
        "checkpoint.create.absence_verified",
        "checkpoint.create.canceled",
      ]),
    );
  });

  it("keeps failed mutations scheduled beyond the diagnostic threshold until absence is verified", async () => {
    vi.useFakeTimers();
    const startedAt = Date.parse("2026-08-20T12:00:00.000Z");
    vi.setSystemTime(startedAt);
    const { coordinator, provider, storage, runtime } = await checkpointFixture("gcp");
    const checkpointID = "chk_failed_absence_maintenance";
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(new Error("ambiguous create"));
    expect((await createCheckpoint(coordinator, checkpointID)).status).toBe(503);
    const recovery = vi.spyOn(provider, "recoverCheckpointImage").mockResolvedValue(undefined);
    let checkpoint = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
    let observedFailedRetry = false;

    await Array.from({ length: 20 }).reduce(async (previous) => {
      await previous;
      if (checkpoint.createClaim?.providerAbsenceVerifiedAt) return;
      expect(checkpoint.retryAt).toBeDefined();
      expect(checkpoint.nextSweepAt).toBe(checkpoint.retryAt);
      expect(runtime.alarmTime).toBe(Date.parse(checkpoint.retryAt!));
      vi.setSystemTime(Date.parse(checkpoint.retryAt!) + 1);
      await coordinator.alarm();
      checkpoint = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
      observedFailedRetry ||= checkpoint.state === "failed" && Boolean(checkpoint.retryAt);
    }, Promise.resolve());

    expect(observedFailedRetry).toBe(true);
    expect(checkpoint).toMatchObject({
      state: "failed",
      createClaim: { providerAbsenceVerifiedAt: expect.any(String) },
    });
    expect(checkpoint.attempts).toBeGreaterThan(checkpointMaxCreateRecoveryAttempts);
    expect(checkpoint.retryAt).toBeUndefined();
    expect(checkpoint.nextSweepAt).toBeUndefined();
    expect(
      Date.parse(checkpoint.createClaim!.providerAbsenceFirstObservedAt!),
    ).toBeGreaterThanOrEqual(startedAt + checkpointProviderAbsenceHorizonMS);
    expect(
      Date.parse(checkpoint.createClaim!.providerAbsenceVerifiedAt!) -
        Date.parse(checkpoint.createClaim!.providerAbsenceFirstObservedAt!),
    ).toBeGreaterThanOrEqual(checkpointProviderAbsenceConfirmationIntervalMS);
    expect(await storage.list({ prefix: "checkpoint-intent:gcp:" })).toHaveLength(1);
    const recoveryCount = recovery.mock.calls.length;

    const canceled = await coordinator.fetch(
      checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`),
    );

    expect(canceled.status).toBe(200);
    expect(recovery).toHaveBeenCalledTimes(recoveryCount);
    expect(await storage.list({ prefix: "checkpoint-intent:gcp:" })).toHaveLength(0);
  });

  it("never counts ambiguous provider recovery errors as exact absence", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, provider, storage } = await checkpointFixture("azure");
    const checkpointID = "chk_ambiguous_recovery_error";
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(new Error("ambiguous create"));
    expect((await createCheckpoint(coordinator, checkpointID)).status).toBe(503);
    vi.spyOn(provider, "recoverCheckpointImage").mockRejectedValueOnce(
      new Error("provider recovery outcome remains ambiguous"),
    );

    const blocked = await coordinator.fetch(
      checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`),
    );

    expect(blocked.status).toBe(500);
    const retained = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
    expect(retained.createClaim?.providerMutationStartedAt).toBe("2026-08-20T12:00:00.000Z");
    expect(retained.createClaim?.providerAbsenceFirstObservedAt).toBeUndefined();
    expect(retained.createClaim?.providerAbsenceLastObservedAt).toBeUndefined();
    expect(retained.createClaim?.providerAbsenceVerifiedAt).toBeUndefined();
    expect(retained.retryAt).toBeDefined();
    expect(await storage.list({ prefix: "checkpoint-intent:azure:" })).toHaveLength(1);
  });

  it("renews expired pending provisioning claims and fences checkpoint deletion and expiry", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage, runtime, deleteImage } = await checkpointFixture();
    await createCheckpoint(coordinator, "chk_cancel_provision", {
      mode: "expire-unused",
      unusedForSeconds: 60,
    });
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
      cloudID: "i-potentially-surviving",
      checkpointID: "chk_cancel_provision",
      checkpointUseClaimHash: await sha256Hex(acquired.claim),
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
      state: "ready",
      activeUseCount: 1,
    });
    const claims = await storage.list<{ state: string; expiresAt: string }>({
      prefix: "checkpoint-use:chk_cancel_provision:",
    });
    expect(claims).toHaveLength(1);
    const renewed = [...claims.values()][0]!;
    expect(renewed.state).toBe("provisioning");
    expect(Date.parse(renewed.expiresAt)).toBeGreaterThan(Date.now());
    expect(runtime.alarmTime).toBe(Date.parse(renewed.expiresAt));

    const blocked = await coordinator.fetch(
      checkpointRequest("DELETE", "/v1/checkpoints/chk_cancel_provision"),
    );
    expect(blocked.status).toBe(409);
    await expect(blocked.json()).resolves.toMatchObject({ error: "checkpoint_in_use" });
    expect(deleteImage).not.toHaveBeenCalled();

    vi.setSystemTime(Date.parse(renewed.expiresAt) + 1);
    await coordinator.alarm();
    expect(await storage.get(checkpointKey("chk_cancel_provision"))).toMatchObject({
      state: "ready",
      activeUseCount: 1,
    });
    expect(deleteImage).not.toHaveBeenCalled();
  });

  it.each(["missing", "mismatched-canceled", "canceled-resource-uncertain"] as const)(
    "renews expired provisioning claims when the lease attempt is %s",
    async (attemptState) => {
      vi.useFakeTimers();
      vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
      const { coordinator, storage } = await checkpointFixture();
      const checkpointID = `chk_provision_${attemptState.replaceAll("-", "_")}`;
      const requestedLeaseID = "cbx_000000000002";
      const attemptID = `cat_${"c".repeat(32)}`;
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
        attemptID,
        requestedLeaseID,
      );
      if (attemptState === "missing") {
        await storage.delete(`create-attempt:${requestedLeaseID}`);
      } else {
        await storage.put(`create-attempt:${requestedLeaseID}`, {
          version: 1,
          requestedLeaseID,
          token: attemptState === "mismatched-canceled" ? `cat_${"d".repeat(32)}` : attemptID,
          owner: "alice@example.com",
          org,
          state: "canceled",
          canonicalLeaseID: requestedLeaseID,
          ...(attemptState === "canceled-resource-uncertain"
            ? { cloudID: "i-potentially-surviving" }
            : {}),
          createdAt: new Date().toISOString(),
          updatedAt: new Date().toISOString(),
        });
      }

      vi.setSystemTime(Date.now() + checkpointUseClaimTTLMS + 1);
      await coordinator.alarm();

      expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({
        state: "ready",
        activeUseCount: 1,
      });
      const claims = await storage.list<{ state: string; expiresAt: string }>({
        prefix: `checkpoint-use:${checkpointID}:`,
      });
      expect(claims).toHaveLength(1);
      expect([...claims.values()][0]).toMatchObject({ state: "provisioning" });
      expect(Date.parse([...claims.values()][0]!.expiresAt)).toBeGreaterThan(Date.now());
    },
  );

  it("releases an expired provisioning claim only after its exact attempt is safely canceled", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_canceled_provision";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"e".repeat(32)}`;
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
      attemptID,
      requestedLeaseID,
    );
    await storage.put(`create-attempt:${requestedLeaseID}`, {
      version: 1,
      requestedLeaseID,
      token: attemptID,
      owner: "alice@example.com",
      org,
      state: "canceled",
      canonicalLeaseID: requestedLeaseID,
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    });

    vi.setSystemTime(Date.now() + checkpointUseClaimTTLMS + 1);
    await coordinator.alarm();

    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
    expect(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).toHaveLength(0);
    expect(
      (await coordinator.fetch(checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`)))
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
      CRABBOX_AZURE_ORPHAN_SWEEP_ENABLED: "0",
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

  it("retains only the latest 256 ordered checkpoint audit events and still prunes tombstones", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_bounded_audit";
    await createCheckpoint(coordinator, checkpointID);

    await Array.from({ length: 101 }).reduce(async (pending) => {
      await pending;
      const begun = (await (
        await coordinator.fetch(
          checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
        )
      ).json()) as { claim: string };
      const renewed = await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, {
          action: "renew",
          claim: begun.claim,
        }),
      );
      const aborted = await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, {
          action: "abort",
          claim: begun.claim,
        }),
      );
      expect(renewed.status).toBe(200);
      expect(aborted.status).toBe(200);
    }, Promise.resolve());

    const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
    const retained = await storage.list<{ sequence: number }>({
      prefix: `checkpoint-event:${checkpointID}:`,
    });
    expect(record.eventSequence).toBe(306);
    expect(retained).toHaveLength(checkpointMaxAuditEvents);
    expect([...retained.values()].map((event) => event.sequence)).toEqual(
      Array.from(
        { length: checkpointMaxAuditEvents },
        (_, index) => record.eventSequence - checkpointMaxAuditEvents + 1 + index,
      ),
    );
    expect(await storage.get(checkpointEventKey(checkpointID, 1))).toBeUndefined();
    const auditResponse = await coordinator.fetch(
      checkpointRequest("GET", `/v1/checkpoints/${checkpointID}/events?limit=500`),
    );
    const publicAudit = (await auditResponse.json()) as { events: Array<{ sequence: number }> };
    expect(auditResponse.status).toBe(200);
    expect(Object.keys(publicAudit)).toEqual(["events"]);
    expect(publicAudit.events).toHaveLength(checkpointMaxAuditEvents);
    expect(publicAudit.events.at(0)?.sequence).toBe(51);
    expect(publicAudit.events.at(-1)?.sequence).toBe(record.eventSequence);

    const deleted = await coordinator.fetch(
      checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`),
    );
    expect(deleted.status).toBe(200);
    expect(await storage.list({ prefix: `checkpoint-event:${checkpointID}:` })).toHaveLength(
      checkpointMaxAuditEvents,
    );
    vi.setSystemTime(Date.now() + checkpointAuditRetentionMS + 1);
    const pruned = await Array.from({ length: 6 }).reduce(async (pending) => {
      const removed = await pending;
      return removed || (await pruneCheckpointTombstone(storage, checkpointID));
    }, Promise.resolve(false));

    expect(pruned).toBe(true);
    expect(await storage.get(checkpointKey(checkpointID))).toBeUndefined();
    expect(await storage.list({ prefix: `checkpoint-event:${checkpointID}:` })).toHaveLength(0);
  }, 20_000);

  it("converges historical excess checkpoint audit events in bounded prune batches", async () => {
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_audit_converges";
    await createCheckpoint(coordinator, checkpointID);
    const record = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
    await Promise.all(
      Array.from({ length: 338 }, async (_, index) => {
        const sequence = index + 3;
        await storage.put(checkpointEventKey(checkpointID, sequence), {
          checkpointID,
          sequence,
          type: "checkpoint.use.renewed",
        });
      }),
    );
    await storage.put(checkpointKey(checkpointID), { ...record, eventSequence: 340 });

    const begun = (await (
      await coordinator.fetch(
        checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, { action: "begin" }),
      )
    ).json()) as { claim: string };
    expect(await storage.list({ prefix: `checkpoint-event:${checkpointID}:` })).toHaveLength(277);
    const renewed = await coordinator.fetch(
      checkpointRequest("POST", `/v1/checkpoints/${checkpointID}/use`, {
        action: "renew",
        claim: begun.claim,
      }),
    );

    expect(renewed.status).toBe(200);
    const retained = await storage.list<{ sequence: number }>({
      prefix: `checkpoint-event:${checkpointID}:`,
    });
    expect(retained).toHaveLength(checkpointMaxAuditEvents);
    expect([...retained.values()].at(0)?.sequence).toBe(87);
    expect([...retained.values()].at(-1)?.sequence).toBe(342);
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
      checkpointID,
      checkpointUseClaimHash: await sha256Hex(acquired.claim),
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

  it("retains a provisioning claim when a terminal lease does not match its exact lifecycle", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    const { coordinator, storage } = await checkpointFixture();
    const checkpointID = "chk_foreign_terminal_lease";
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"f".repeat(32)}`;
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
      attemptID,
      requestedLeaseID,
    );
    await storage.put(`lease:${requestedLeaseID}`, {
      ...checkpointLease("aws"),
      id: requestedLeaseID,
      checkpointID: "chk_different_lifecycle",
      createAttemptID: attemptID,
      state: "released",
    });

    vi.setSystemTime(Date.now() + checkpointUseClaimTTLMS + 1);
    await coordinator.alarm();

    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 1 });
    expect(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).toHaveLength(1);
  });

  it("retains ownership through transient absence, then recovers and deletes the exact resource", async () => {
    const { coordinator, provider, storage, deleteImage } = await checkpointFixture("azure");
    vi.mocked(provider.createCheckpointImage).mockRejectedValueOnce(new Error("ambiguous"));
    const checkpointID = "chk_failed_owned_recovery";
    await createCheckpoint(coordinator, checkpointID);
    await Array.from({ length: checkpointMaxCreateRecoveryAttempts }).reduce(async (previous) => {
      await previous;
      await recordCheckpointCreateRecoveryFailure(storage, checkpointID, "ambiguous");
    }, Promise.resolve());
    const recovery = vi
      .spyOn(provider, "recoverCheckpointImage")
      .mockResolvedValueOnce(undefined)
      .mockImplementationOnce(async (checkpoint) =>
        providerImage("azure", checkpoint.createClaim!.resourceName, "disk-snapshot", {
          checkpointID,
          tokenHash: checkpoint.createClaim!.tokenHash,
          sourceLeaseID: leaseID,
        }),
      );

    const pending = await coordinator.fetch(
      checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`),
    );
    expect(pending.status).toBe(409);
    await expect(pending.json()).resolves.toMatchObject({ error: "checkpoint_pending" });
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({
      state: "failed",
      createClaim: { tokenHash: expect.any(String) },
    });
    expect(await storage.list({ prefix: "checkpoint-intent:azure:" })).toHaveLength(1);
    expect(deleteImage).not.toHaveBeenCalled();

    const deleted = await coordinator.fetch(
      checkpointRequest("DELETE", `/v1/checkpoints/${checkpointID}`),
    );
    expect(deleted.status).toBe(200);
    expect(recovery).toHaveBeenCalledTimes(2);
    expect(deleteImage).toHaveBeenCalledOnce();
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ state: "deleted" });
    expect(await storage.list({ prefix: "checkpoint-intent:azure:" })).toHaveLength(0);
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
    await expect(deleted.json()).resolves.toMatchObject({ error: "checkpoint_source_mismatch" });
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({
      state: "failed",
      createClaim: { tokenHash: expect.any(String) },
    });
    const retained = (await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)))!;
    expect(retained.createClaim?.providerAbsenceFirstObservedAt).toBeUndefined();
    expect(retained.createClaim?.providerAbsenceLastObservedAt).toBeUndefined();
    expect(retained.createClaim?.providerAbsenceVerifiedAt).toBeUndefined();
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
      Reflect.set(provider, "clientValue", {
        getImage: async () => image,
        providerScope: () =>
          `/subscriptions/${providerScope("azure").subscriptionID}/resourceGroups/${providerScope("azure").resourceGroup}`,
      });
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
