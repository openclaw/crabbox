import type { Pool, PoolClient, QueryResult, QueryResultRow } from "pg";
import { describe, expect, it, vi } from "vitest";

import { PostgresCoordinatorStorage } from "../node/postgres-storage";
import { sha256Hex } from "../src/auth";
import {
  CheckpointError,
  acquireCheckpointUse,
  backfillFailedCheckpointCreateRecovery,
  bindCheckpointUseProvisioning,
  checkpointDueKey,
  checkpointKey,
  checkpointLimits,
  checkpointMaxCreateRecoveryAttempts,
  checkpointProviderAbsenceConfirmationIntervalMS,
  checkpointProviderAbsenceHorizonMS,
  markCheckpointProviderMutationStarted,
  recordCheckpointProviderAbsence,
  resolveRejectedCheckpointProvisioning,
  reserveCheckpointCreate,
} from "../src/checkpoints";
import { backfillCheckpointCreateAttempt } from "../src/fleet";
import { orgKeyForLabel } from "../src/org-identity";
import type {
  CoordinatorCheckpointRecord,
  CoordinatorCheckpointUseClaim,
  CreateAttemptRecord,
} from "../src/types";

describe("PostgresCoordinatorStorage", () => {
  it("initializes its schema and compatibility table", async () => {
    const pool = fakePool();
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);

    await storage.initialize();

    expect(pool.query).toHaveBeenCalledTimes(4);
    expect(pool.query.mock.calls.map(([sql]) => String(sql))).toEqual([
      expect.stringContaining("create schema if not exists crabbox"),
      expect.stringContaining("create table if not exists crabbox.coordinator_kv"),
      expect.stringContaining("add column if not exists value_text text"),
      expect.stringContaining("create index if not exists coordinator_kv_updated_at_idx"),
    ]);
  });

  it("stores JSON values with an upsert", async () => {
    const pool = fakePool();
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);

    await storage.put("lease:1", { state: "active" });

    expect(pool.query).toHaveBeenCalledWith(
      expect.stringContaining("on conflict (key) do update"),
      ["lease:1", '{"state":"active"}', '{"state":"active"}'],
    );
  });

  it("round-trips NUL-containing strings through the text representation", async () => {
    const pool = fakePool([{ encoded_value: '"before\\u0000after"' }]);
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);

    await storage.put("runlog:1", "before\0after");
    const value = await storage.get<string>("runlog:1");

    expect(pool.query).toHaveBeenCalledWith(expect.stringContaining("$2::jsonb"), [
      "runlog:1",
      '"before�after"',
      '"before\\u0000after"',
    ]);
    expect(value).toBe("before\0after");
  });

  it("sanitizes NUL-containing object keys in the JSONB compatibility value", async () => {
    const pool = fakePool();
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);

    await storage.put("runlog:1", { "before\0after": "value" });

    expect(pool.query).toHaveBeenCalledWith(expect.stringContaining("$2::jsonb"), [
      "runlog:1",
      '{"before�after":"value"}',
      '{"before\\u0000after":"value"}',
    ]);
  });

  it("escapes LIKE metacharacters in prefix scans", async () => {
    const pool = fakePool([{ key: "run:100%_x", encoded_value: '{"id":"100"}' }]);
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);

    const records = await storage.list<{ id: string }>({ prefix: "run:100%_x" });

    expect(pool.query).toHaveBeenCalledWith(expect.stringContaining("where key like $1"), [
      "run:100\\%\\_x%",
    ]);
    expect(records).toEqual(new Map([["run:100%_x", { id: "100" }]]));
  });

  it("pages prefix scans after the previous storage key", async () => {
    const pool = fakePool([{ key: "lease:2", encoded_value: '{"id":"2"}' }]);
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);

    const records = await storage.list<{ id: string }>({
      prefix: "lease:",
      startAfter: "lease:1",
      limit: 128,
      noCache: true,
    });

    expect(pool.query).toHaveBeenCalledWith(expect.stringContaining("and key > $2"), [
      "lease:%",
      "lease:1",
      128,
    ]);
    expect(String(pool.query.mock.calls[0]?.[0])).toContain("limit $3");
    expect(records).toEqual(new Map([["lease:2", { id: "2" }]]));
  });

  it("atomically takes one stored value", async () => {
    const pool = fakePool([{ encoded_value: '{"ticket":"one-time"}' }]);
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);

    const value = await storage.take<{ ticket: string }>("handoff:1");

    expect(pool.query).toHaveBeenCalledWith(
      expect.stringContaining("delete from crabbox.coordinator_kv"),
      ["handoff:1"],
    );
    expect(String(pool.query.mock.calls[0]?.[0])).toContain("returning case");
    expect(value).toEqual({ ticket: "one-time" });
  });

  it("commits transaction-scoped storage work on one checked-out client", async () => {
    const { pool, clientQuery, connect, release } = fakeTransactionalPool();
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);

    await storage.transaction(async (transaction) => {
      await transaction.put("image:one", { id: "one" });
      await transaction.delete("image:stale");
    });

    expect(connect).toHaveBeenCalledOnce();
    expect(pool.query).not.toHaveBeenCalled();
    expect(clientQuery.mock.calls.map(([sql]) => String(sql).trim().split(/\s+/, 1)[0])).toEqual([
      "begin",
      "insert",
      "delete",
      "commit",
    ]);
    expect(String(clientQuery.mock.calls[0]?.[0]).trim()).toBe(
      "begin isolation level serializable",
    );
    expect(release).toHaveBeenCalledOnce();
  });

  it("rolls back transaction-scoped storage work when the callback fails", async () => {
    const { pool, clientQuery, release } = fakeTransactionalPool();
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);

    await expect(
      storage.transaction(async (transaction) => {
        await transaction.put("image:one", { id: "one" });
        throw new Error("publish failed");
      }),
    ).rejects.toThrow("publish failed");

    expect(clientQuery.mock.calls.map(([sql]) => String(sql).trim().split(/\s+/, 1)[0])).toEqual([
      "begin",
      "insert",
      "rollback",
    ]);
    expect(release).toHaveBeenCalledOnce();
  });

  it.each([
    { code: "40001", label: "serialization failure" },
    { code: "40P01", label: "deadlock" },
  ])("retries one $label on a fresh client", async ({ code }) => {
    const { pool, clients, connect } = fakeRetryTransactionalPool();
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);
    const retryableError = postgresError(code, "retry transaction");
    let callbackInvocations = 0;

    const result = await storage.transaction(async () => {
      callbackInvocations++;
      if (callbackInvocations === 1) throw retryableError;
      return "committed";
    });

    expect(result).toBe("committed");
    expect(callbackInvocations).toBe(2);
    expect(connect).toHaveBeenCalledTimes(2);
    expect(clients).toHaveLength(2);
    expect(clients[0]?.query.mock.calls.map(([sql]) => String(sql).trim())).toEqual([
      "begin isolation level serializable",
      "rollback",
    ]);
    expect(clients[1]?.query.mock.calls.map(([sql]) => String(sql).trim())).toEqual([
      "begin isolation level serializable",
      "commit",
    ]);
    expect(clients[0]?.release).toHaveBeenCalledWith();
    expect(clients[1]?.release).toHaveBeenCalledWith();
  });

  it("stops after twelve bounded serialization failures and rethrows the database error", async () => {
    const { pool, clients, connect } = fakeRetryTransactionalPool();
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);
    const serializationError = postgresError("40001", "serialization failed");
    let callbackInvocations = 0;

    await expect(
      storage.transaction(async () => {
        callbackInvocations++;
        throw serializationError;
      }),
    ).rejects.toBe(serializationError);

    expect(callbackInvocations).toBe(12);
    expect(connect).toHaveBeenCalledTimes(12);
    expect(clients).toHaveLength(12);
    for (const client of clients) {
      expect(client.query.mock.calls.map(([sql]) => String(sql).trim())).toEqual([
        "begin isolation level serializable",
        "rollback",
      ]);
      expect(client.release).toHaveBeenCalledWith();
    }
  });

  it("retries parallel checkpoint-style record contention without losing any claim", async () => {
    const { pool } = fakeRetryTransactionalPool();
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);
    let activeUses = 0;
    const fanout = 12;
    await Promise.all(
      Array.from({ length: fanout }, async () =>
        storage.transaction(async () => {
          const observed = activeUses;
          await new Promise<void>((resolve) => setTimeout(resolve, 1));
          if (observed !== activeUses) {
            throw postgresError("40001", "checkpoint claim serialization conflict");
          }
          activeUses = observed + 1;
        }),
      ),
    );
    expect(activeUses).toBe(fanout);
  });

  it("preserves every concurrent checkpoint shard use claim across PostgreSQL serialization", async () => {
    const pool = fakeContendedCheckpointPool();
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);
    const id = "chk_postgres_fanout";
    const now = new Date().toISOString();
    const org = orgKeyForLabel("example-org");
    await storage.put(checkpointKey(id), {
      version: 1,
      id,
      owner: "alice@example.com",
      org,
      leaseID: "cbx_000000000001",
      provider: "aws",
      scope: { region: "eu-west-1", accountID: "123456789012" },
      name: "parallel-checkpoint",
      strategy: "disk-snapshot",
      noReboot: true,
      image: {
        id: "snap-owned",
        resourceID: "snap-owned",
        kind: "aws-ebs-snapshot",
        immutableID: "snap-owned",
        snapshotIDs: ["snap-owned"],
        state: "available",
      },
      state: "ready",
      retention: { mode: "manual" },
      generation: 1,
      revision: 1,
      createdAt: now,
      updatedAt: now,
      lastUsedAt: now,
      attempts: 0,
      pinCount: 0,
      activeUseCount: 0,
      eventSequence: 0,
      target: "linux",
    } satisfies CoordinatorCheckpointRecord);
    const claims = await Promise.all(
      Array.from({ length: 12 }, async () =>
        acquireCheckpointUse(storage, id, { owner: "alice@example.com", org }),
      ),
    );
    expect(new Set(claims.map((claim) => claim.token)).size).toBe(12);
    expect(await storage.get<CoordinatorCheckpointRecord>(checkpointKey(id))).toMatchObject({
      activeUseCount: 12,
      eventSequence: 12,
    });
    expect(await storage.list({ prefix: `checkpoint-use:${id}:` })).toHaveLength(12);
  });

  it("never over-admits concurrent checkpoint reservations across PostgreSQL serialization", async () => {
    const pool = fakeContendedCheckpointPool();
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);
    const org = orgKeyForLabel("example-org");
    const limits = checkpointLimits({ CRABBOX_MAX_CHECKPOINTS: "3" });
    const createdAt = new Date().toISOString();

    const results = await Promise.allSettled(
      Array.from({ length: 8 }, async (_, index) =>
        reserveCheckpointCreate(
          storage,
          {
            record: {
              id: `chk_postgres_reservation_${index}`,
              owner: "alice@example.com",
              org,
              leaseID: "cbx_000000000001",
              provider: "aws",
              scope: { region: "eu-west-1", accountID: "123456789012" },
              name: "parallel-checkpoint",
              strategy: "disk-snapshot",
              noReboot: true,
              retention: { mode: "manual" },
              createdAt,
              lastUsedAt: createdAt,
              target: "linux",
            },
            ownershipToken: `ownership-${index}`,
            resourceName: `parallel-resource-${index}`,
            coordinatorGeneration: "generation-1",
          },
          limits,
        ),
      ),
    );

    expect(results.filter((result) => result.status === "fulfilled")).toHaveLength(3);
    const rejected = results.filter((result) => result.status === "rejected");
    expect(rejected).toHaveLength(5);
    expect(
      rejected.every(
        (result) =>
          result.reason instanceof CheckpointError &&
          result.reason.code === "checkpoint_limit_exceeded" &&
          result.reason.status === 429,
      ),
    ).toBe(true);
    expect(await storage.list({ prefix: "checkpoint:" })).toHaveLength(3);
    expect(await storage.list({ prefix: "checkpoint-event:" })).toHaveLength(3);
  });

  it("persists checkpoint mutation and absence phases transactionally without schema changes", async () => {
    vi.useFakeTimers();
    const startedAt = Date.parse("2026-08-20T12:00:00.000Z");
    vi.setSystemTime(startedAt);
    try {
      const storage = new PostgresCoordinatorStorage(
        "postgres://unused",
        fakeContendedCheckpointPool(),
      );
      const id = "chk_postgres_absence_phases";
      const ownershipToken = "postgres-checkpoint-ownership";
      const createdAt = new Date().toISOString();
      await reserveCheckpointCreate(storage, {
        record: {
          id,
          owner: "alice@example.com",
          org: orgKeyForLabel("example-org"),
          leaseID: "cbx_000000000001",
          provider: "aws",
          scope: { region: "eu-west-1", accountID: "123456789012" },
          name: "durable-checkpoint",
          strategy: "disk-snapshot",
          noReboot: true,
          retention: { mode: "manual" },
          createdAt,
          lastUsedAt: createdAt,
          target: "linux",
        },
        ownershipToken,
        resourceName: "durable-checkpoint-resource",
        coordinatorGeneration: "generation-1",
      });
      expect(await storage.get<CoordinatorCheckpointRecord>(checkpointKey(id))).toMatchObject({
        createClaim: { providerMutationPhase: "reserved" },
      });
      await markCheckpointProviderMutationStarted(storage, id, ownershipToken);
      expect(await storage.get<CoordinatorCheckpointRecord>(checkpointKey(id))).toMatchObject({
        createClaim: {
          providerMutationPhase: "started",
          providerMutationStartedAt: "2026-08-20T12:00:00.000Z",
        },
      });

      vi.setSystemTime(startedAt + checkpointProviderAbsenceHorizonMS);
      const first = await recordCheckpointProviderAbsence(storage, id);
      expect(first).toMatchObject({
        createClaim: { providerAbsenceFirstObservedAt: "2026-08-20T13:00:00.000Z" },
        retryAt: "2026-08-20T13:05:00.000Z",
        nextSweepAt: "2026-08-20T13:05:00.000Z",
      });
      expect(await storage.list({ prefix: "checkpoint-due:" })).toHaveLength(1);

      vi.setSystemTime(
        startedAt +
          checkpointProviderAbsenceHorizonMS +
          checkpointProviderAbsenceConfirmationIntervalMS,
      );
      const verified = await recordCheckpointProviderAbsence(storage, id);

      expect(verified).toMatchObject({
        state: "failed",
        createClaim: { providerAbsenceVerifiedAt: "2026-08-20T13:05:00.000Z" },
      });
      expect(verified.retryAt).toBeUndefined();
      expect(await storage.list({ prefix: "checkpoint-due:" })).toHaveLength(0);
      expect(await storage.list({ prefix: "checkpoint-intent:" })).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("transactionally fences one create attempt to one PostgreSQL checkpoint use claim", async () => {
    const storage = new PostgresCoordinatorStorage(
      "postgres://unused",
      fakeContendedCheckpointPool(),
    );
    const checkpointIDs = ["chk_postgres_attempt_first", "chk_postgres_attempt_second"];
    const principal = { owner: "alice@example.com", org: orgKeyForLabel("example-org") };
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"a".repeat(32)}`;
    const now = new Date().toISOString();
    await Promise.all(
      checkpointIDs.map(async (id) => {
        await storage.put(checkpointKey(id), {
          version: 1,
          id,
          owner: principal.owner,
          org: principal.org,
          leaseID: "cbx_000000000001",
          provider: "aws",
          scope: { region: "eu-west-1", accountID: "123456789012" },
          name: id,
          strategy: "disk-snapshot",
          noReboot: true,
          image: {
            id,
            resourceID: id,
            kind: "aws-ebs-snapshot",
            immutableID: id,
            snapshotIDs: [id],
            state: "available",
          },
          state: "ready",
          retention: { mode: "manual" },
          generation: 1,
          revision: 1,
          createdAt: now,
          updatedAt: now,
          lastUsedAt: now,
          attempts: 0,
          pinCount: 0,
          activeUseCount: 0,
          eventSequence: 0,
          target: "linux",
        } satisfies CoordinatorCheckpointRecord);
      }),
    );
    const claims = await Promise.all(
      checkpointIDs.map(async (id) => await acquireCheckpointUse(storage, id, principal)),
    );
    const ordinaryAttempt = {
      version: 1,
      requestedLeaseID,
      token: attemptID,
      owner: principal.owner,
      org: principal.org,
      state: "pending",
      createdAt: now,
      updatedAt: now,
    } satisfies CreateAttemptRecord;
    await storage.put(`create-attempt:${requestedLeaseID}`, ordinaryAttempt);
    await expect(
      bindCheckpointUseProvisioning(
        storage,
        checkpointIDs[0]!,
        claims[0]!.token,
        principal,
        attemptID,
        requestedLeaseID,
      ),
    ).rejects.toMatchObject({ code: "create_attempt_binding_conflict" });
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toEqual(ordinaryAttempt);
    await storage.put(`create-attempt:${requestedLeaseID}`, {
      ...ordinaryAttempt,
      checkpointID: checkpointIDs[0],
      checkpointUseClaimHash: await sha256Hex(claims[0]!.token),
    } satisfies CreateAttemptRecord);

    const results = await Promise.allSettled(
      checkpointIDs.map(
        async (checkpointID, index) =>
          await bindCheckpointUseProvisioning(
            storage,
            checkpointID,
            claims[index]!.token,
            principal,
            attemptID,
            requestedLeaseID,
          ),
      ),
    );

    const winnerIndex = results.findIndex((result) => result.status === "fulfilled");
    const loserIndex = winnerIndex === 0 ? 1 : 0;
    expect(winnerIndex).toBe(0);
    expect(results[loserIndex]).toMatchObject({
      status: "rejected",
      reason: { code: "create_attempt_binding_conflict" },
    });
    const winnerID = checkpointIDs[winnerIndex]!;
    const loserID = checkpointIDs[loserIndex]!;
    const winningClaim = [...(await storage.list({ prefix: `checkpoint-use:${winnerID}:` }))][0]!;
    expect(
      await storage.get<CreateAttemptRecord>(`create-attempt:${requestedLeaseID}`),
    ).toMatchObject({
      checkpointID: winnerID,
      checkpointUseClaimHash: winningClaim[0].split(":").at(-1),
    });
    expect(winningClaim[1]).toMatchObject({ state: "provisioning" });
    expect([...(await storage.list({ prefix: `checkpoint-use:${loserID}:` })).values()]).toEqual([
      expect.objectContaining({ state: "available" }),
    ]);
  });

  it.each([
    { label: "pending attempt without a lease", attemptState: "pending" },
    { label: "canceled attempt without a lease", attemptState: "canceled" },
    { label: "clean failed lease", attemptState: "pending", leaseState: "failed" },
    { label: "clean released lease", attemptState: "pending", leaseState: "released" },
    { label: "clean expired lease", attemptState: "pending", leaseState: "expired" },
  ] as const)("atomically resolves a rejected PostgreSQL $label exactly once", async (scenario) => {
    const storage = new PostgresCoordinatorStorage(
      "postgres://unused",
      fakeContendedCheckpointPool(),
    );
    const checkpointID = "chk_postgres_rejected_attempt";
    const principal = { owner: "alice@example.com", org: orgKeyForLabel("example-org") };
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"b".repeat(32)}`;
    const now = new Date().toISOString();
    await storage.put(checkpointKey(checkpointID), {
      version: 1,
      id: checkpointID,
      owner: principal.owner,
      org: principal.org,
      leaseID: "cbx_000000000001",
      provider: "aws",
      scope: { region: "eu-west-1", accountID: "123456789012" },
      name: checkpointID,
      strategy: "disk-snapshot",
      noReboot: true,
      image: {
        id: "snap-owned",
        resourceID: "snap-owned",
        kind: "aws-ebs-snapshot",
        immutableID: "snap-owned",
        snapshotIDs: ["snap-owned"],
        state: "available",
      },
      state: "ready",
      retention: { mode: "manual" },
      generation: 1,
      revision: 1,
      createdAt: now,
      updatedAt: now,
      lastUsedAt: now,
      attempts: 0,
      pinCount: 0,
      activeUseCount: 0,
      eventSequence: 0,
      target: "linux",
    } satisfies CoordinatorCheckpointRecord);
    const claim = await acquireCheckpointUse(storage, checkpointID, principal);
    await storage.put(`create-attempt:${requestedLeaseID}`, {
      version: 1,
      requestedLeaseID,
      token: attemptID,
      owner: principal.owner,
      org: principal.org,
      state: "pending",
      checkpointID,
      checkpointUseClaimHash: await sha256Hex(claim.token),
      createdAt: now,
      updatedAt: now,
    } satisfies CreateAttemptRecord);
    await bindCheckpointUseProvisioning(
      storage,
      checkpointID,
      claim.token,
      principal,
      attemptID,
      requestedLeaseID,
    );
    if (scenario.attemptState === "canceled") {
      const attempt = (await storage.get<CreateAttemptRecord>(
        `create-attempt:${requestedLeaseID}`,
      ))!;
      await storage.put(`create-attempt:${requestedLeaseID}`, {
        ...attempt,
        state: "canceled",
      } satisfies CreateAttemptRecord);
    }
    if ("leaseState" in scenario) {
      await storage.put(`lease:${requestedLeaseID}`, {
        id: requestedLeaseID,
        checkpointID,
        createAttemptID: attemptID,
        owner: principal.owner,
        org: principal.org,
        state: scenario.leaseState,
        cloudID: "i-provider-resource",
      });
    }

    const resolved = await Promise.all(
      Array.from(
        { length: 2 },
        async () =>
          await resolveRejectedCheckpointProvisioning(
            storage,
            checkpointID,
            claim.token,
            principal,
            attemptID,
            requestedLeaseID,
          ),
      ),
    );

    expect(resolved.filter(Boolean)).toHaveLength(1);
    expect(await storage.get(checkpointKey(checkpointID))).toMatchObject({ activeUseCount: 0 });
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      token: attemptID,
      state: "leaseState" in scenario ? "pending" : "canceled",
      checkpointID,
      checkpointUseClaimHash: await sha256Hex(claim.token),
    });
    expect(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).toHaveLength(0);
    const events = [
      ...(
        await storage.list<{ type: string }>({
          prefix: `checkpoint-event:${checkpointID}:`,
        })
      ).values(),
    ];
    expect(events.filter((event) => event.type === "checkpoint.use.aborted")).toHaveLength(1);
  });

  it("backfills an exhausted PostgreSQL checkpoint exactly once beyond deleted record pages", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-20T12:00:00.000Z"));
    try {
      const storage = new PostgresCoordinatorStorage(
        "postgres://unused",
        fakeContendedCheckpointPool(),
      );
      const id = "chk_zz_postgres_exhausted";
      const createdAt = new Date().toISOString();
      const reserved = await reserveCheckpointCreate(storage, {
        record: {
          id,
          owner: "alice@example.com",
          org: orgKeyForLabel("example-org"),
          leaseID: "cbx_000000000001",
          provider: "aws",
          scope: { region: "eu-west-1", accountID: "123456789012" },
          name: "durable-checkpoint",
          strategy: "disk-snapshot",
          noReboot: true,
          retention: { mode: "manual" },
          createdAt,
          lastUsedAt: createdAt,
          target: "linux",
        },
        ownershipToken: "postgres-exhausted-ownership",
        resourceName: "postgres-exhausted-resource",
        coordinatorGeneration: "generation-1",
      });
      await storage.delete(checkpointDueKey(id, reserved.nextSweepAt!));
      const { nextSweepAt: _due, ...withoutDue } = reserved;
      void _due;
      const { providerMutationPhase: _phase, ...legacyClaim } = reserved.createClaim!;
      void _phase;
      const failed = {
        ...withoutDue,
        state: "failed",
        attempts: checkpointMaxCreateRecoveryAttempts,
        createClaim: legacyClaim,
      } satisfies CoordinatorCheckpointRecord;
      await storage.put(checkpointKey(id), failed);
      await Promise.all(
        Array.from({ length: 11 }, async (_, index) => {
          const tombstoneID = `chk_${String(index).padStart(2, "0")}_postgres_deleted`;
          await storage.put(checkpointKey(tombstoneID), {
            ...failed,
            id: tombstoneID,
            state: "deleted",
            deletedAt: createdAt,
          } satisfies CoordinatorCheckpointRecord);
        }),
      );
      const limits = checkpointLimits({ CRABBOX_MAX_CHECKPOINTS: "3" });

      expect(await backfillFailedCheckpointCreateRecovery(storage, limits)).toBe(1);
      expect(await storage.get(checkpointKey(id))).toMatchObject({
        state: "failed",
        retryAt: createdAt,
        nextSweepAt: createdAt,
        createClaim: {
          providerMutationPhase: "started",
          providerMutationStartedAt: createdAt,
        },
      });
      const events = await storage.list({ prefix: `checkpoint-event:${id}:` });
      const due = await storage.list({ prefix: "checkpoint-due:" });

      expect(await backfillFailedCheckpointCreateRecovery(storage, limits)).toBe(0);
      expect(await storage.list({ prefix: `checkpoint-event:${id}:` })).toEqual(events);
      expect(await storage.list({ prefix: "checkpoint-due:" })).toEqual(due);
      expect(due).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("serializes competing PostgreSQL legacy-attempt backfills to one exact checkpoint winner", async () => {
    const storage = new PostgresCoordinatorStorage(
      "postgres://unused",
      fakeContendedCheckpointPool(),
    );
    const checkpointIDs = ["chk_postgres_legacy_first", "chk_postgres_legacy_second"];
    const principal = { owner: "alice@example.com", org: orgKeyForLabel("example-org") };
    const requestedLeaseID = "cbx_000000000002";
    const attemptID = `cat_${"f".repeat(32)}`;
    const now = new Date().toISOString();
    await Promise.all(
      checkpointIDs.map(async (id) => {
        await storage.put(checkpointKey(id), {
          version: 1,
          id,
          owner: principal.owner,
          org: principal.org,
          leaseID: "cbx_000000000001",
          provider: "aws",
          scope: { region: "eu-west-1", accountID: "123456789012" },
          name: id,
          strategy: "disk-snapshot",
          noReboot: true,
          image: {
            id,
            resourceID: id,
            kind: "aws-ebs-snapshot",
            immutableID: id,
            snapshotIDs: [id],
            state: "available",
          },
          state: "ready",
          retention: { mode: "manual" },
          generation: 1,
          revision: 1,
          createdAt: now,
          updatedAt: now,
          lastUsedAt: now,
          attempts: 0,
          pinCount: 0,
          activeUseCount: 0,
          eventSequence: 0,
          target: "linux",
        } satisfies CoordinatorCheckpointRecord);
      }),
    );
    const claims = await Promise.all(
      checkpointIDs.map(async (id) => await acquireCheckpointUse(storage, id, principal)),
    );
    const claimHashes = await Promise.all(
      claims.map(async (claim) => await sha256Hex(claim.token)),
    );
    await Promise.all(
      checkpointIDs.map(async (checkpointID, index) => {
        const key = `checkpoint-use:${checkpointID}:${claimHashes[index]}`;
        const claim = (await storage.get<CoordinatorCheckpointUseClaim>(key))!;
        await storage.put(key, {
          ...claim,
          state: "provisioning",
          attemptID,
          leaseID: requestedLeaseID,
        } satisfies CoordinatorCheckpointUseClaim);
      }),
    );
    await storage.put(`create-attempt:${requestedLeaseID}`, {
      version: 1,
      requestedLeaseID,
      token: attemptID,
      owner: principal.owner,
      org: principal.org,
      state: "pending",
      createdAt: now,
      updatedAt: now,
    } satisfies CreateAttemptRecord);
    const eventsBefore = await storage.list({ prefix: "checkpoint-event:" });

    const results = await Promise.allSettled(
      checkpointIDs.map(
        async (checkpointID, index) =>
          await backfillCheckpointCreateAttempt(storage, {
            requestedLeaseID,
            token: attemptID,
            ...principal,
            checkpointID,
            checkpointUseClaimHash: claimHashes[index]!,
          }),
      ),
    );

    expect(results.filter((result) => result.status === "fulfilled")).toHaveLength(1);
    expect(results.find((result) => result.status === "rejected")).toMatchObject({
      reason: { code: "create_attempt_binding_conflict" },
    });
    const winnerIndex = results.findIndex((result) => result.status === "fulfilled");
    expect(await storage.get(`create-attempt:${requestedLeaseID}`)).toMatchObject({
      checkpointID: checkpointIDs[winnerIndex],
      checkpointUseClaimHash: claimHashes[winnerIndex],
    });
    await Promise.all(
      checkpointIDs.map(async (checkpointID) => {
        expect([
          ...(await storage.list({ prefix: `checkpoint-use:${checkpointID}:` })).values(),
        ]).toEqual([expect.objectContaining({ state: "provisioning", attemptID })]);
      }),
    );
    expect(await storage.list({ prefix: "checkpoint-event:" })).toEqual(eventsBefore);
  });

  it("rejects one-over concurrent checkpoint claims without duplicate PostgreSQL events", async () => {
    const pool = fakeContendedCheckpointPool();
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);
    const id = "chk_postgres_claim_cap";
    const now = new Date().toISOString();
    const org = orgKeyForLabel("example-org");
    await storage.put(checkpointKey(id), {
      version: 1,
      id,
      owner: "alice@example.com",
      org,
      leaseID: "cbx_000000000001",
      provider: "aws",
      scope: { region: "eu-west-1", accountID: "123456789012" },
      name: "parallel-checkpoint",
      strategy: "disk-snapshot",
      noReboot: true,
      image: {
        id: "snap-owned",
        resourceID: "snap-owned",
        kind: "aws-ebs-snapshot",
        immutableID: "snap-owned",
        snapshotIDs: ["snap-owned"],
        state: "available",
      },
      state: "ready",
      retention: { mode: "manual" },
      generation: 1,
      revision: 1,
      createdAt: now,
      updatedAt: now,
      lastUsedAt: now,
      attempts: 0,
      pinCount: 0,
      activeUseCount: 0,
      eventSequence: 0,
      target: "linux",
    } satisfies CoordinatorCheckpointRecord);
    const limits = checkpointLimits({ CRABBOX_MAX_CHECKPOINT_USE_CLAIMS: "5" });

    const results = await Promise.allSettled(
      Array.from({ length: 9 }, async () =>
        acquireCheckpointUse(storage, id, { owner: "alice@example.com", org }, limits),
      ),
    );

    expect(results.filter((result) => result.status === "fulfilled")).toHaveLength(5);
    expect(results.filter((result) => result.status === "rejected")).toHaveLength(4);
    expect(await storage.get<CoordinatorCheckpointRecord>(checkpointKey(id))).toMatchObject({
      activeUseCount: 5,
      eventSequence: 5,
    });
    expect(await storage.list({ prefix: `checkpoint-use:${id}:` })).toHaveLength(5);
    expect(await storage.list({ prefix: `checkpoint-event:${id}:` })).toHaveLength(5);
  });

  it("preserves transaction and rollback failures and evicts the uncertain client", async () => {
    const rollbackError = new Error("rollback failed");
    const originalError = postgresError("40001", "publish failed");
    const { pool, clientQuery, connect, release } = fakeTransactionalPool(rollbackError);
    const storage = new PostgresCoordinatorStorage("postgres://unused", pool);
    let callbackInvocations = 0;

    let observed: unknown;
    try {
      await storage.transaction(async () => {
        callbackInvocations++;
        throw originalError;
      });
    } catch (error) {
      observed = error;
    }

    expect(observed).toBeInstanceOf(AggregateError);
    expect((observed as AggregateError).errors).toEqual([originalError, rollbackError]);
    expect((observed as AggregateError).cause).toBe(originalError);
    expect(clientQuery.mock.calls.map(([sql]) => String(sql).trim())).toEqual([
      "begin isolation level serializable",
      "rollback",
    ]);
    expect(callbackInvocations).toBe(1);
    expect(connect).toHaveBeenCalledOnce();
    expect(release).toHaveBeenCalledWith(rollbackError);
  });
});

function fakePool(rows: QueryResultRow[] = []) {
  const query = vi.fn<(text: string, values?: unknown[]) => Promise<QueryResult<QueryResultRow>>>(
    async () => queryResult(rows),
  );
  const end = vi.fn<() => Promise<void>>(async () => undefined);
  return { query, end } as unknown as Pool & { query: typeof query };
}

function fakeTransactionalPool(rollbackError?: Error) {
  const clientQuery = vi.fn<
    (text: string, values?: unknown[]) => Promise<QueryResult<QueryResultRow>>
  >(async (text) => {
    if (text.trim() === "rollback" && rollbackError) throw rollbackError;
    return queryResult([]);
  });
  const release = vi.fn<(error?: Error | boolean) => void>();
  const client = { query: clientQuery, release } as unknown as PoolClient;
  const connect = vi.fn<() => Promise<PoolClient>>(async () => client);
  const query = vi.fn<(text: string, values?: unknown[]) => Promise<QueryResult<QueryResultRow>>>(
    async () => queryResult([]),
  );
  const end = vi.fn<() => Promise<void>>(async () => undefined);
  const pool = { query, connect, end } as unknown as Pool & { query: typeof query };
  return { pool, clientQuery, connect, release };
}

function fakeRetryTransactionalPool() {
  const clients: Array<{
    query: ReturnType<typeof transactionClientQuery>;
    release: ReturnType<typeof transactionClientRelease>;
  }> = [];
  const connect = vi.fn<() => Promise<PoolClient>>(async () => {
    const query = transactionClientQuery();
    const release = transactionClientRelease();
    clients.push({ query, release });
    return { query, release } as unknown as PoolClient;
  });
  const query = vi.fn<(text: string, values?: unknown[]) => Promise<QueryResult<QueryResultRow>>>(
    async () => queryResult([]),
  );
  const end = vi.fn<() => Promise<void>>(async () => undefined);
  const pool = { query, connect, end } as unknown as Pool & { query: typeof query };
  return { pool, clients, connect };
}

function fakeContendedCheckpointPool(): Pool {
  let committed = new Map<string, string>();
  let revision = 0;
  const execute = (
    values: Map<string, string>,
    text: string,
    parameters: unknown[] = [],
  ): QueryResult<QueryResultRow> => {
    const sql = text.trim().toLowerCase();
    if (sql.startsWith("insert")) {
      values.set(String(parameters[0]), String(parameters[2]));
      return queryResult([]);
    }
    if (sql.startsWith("delete")) {
      values.delete(String(parameters[0]));
      return queryResult([]);
    }
    if (sql.includes("where key like")) {
      const prefix = String(parameters[0])
        .slice(0, -1)
        .replaceAll(/\\([\\%_])/g, "$1");
      const startAfter = sql.includes("and key >") ? String(parameters[1]) : undefined;
      const limit = sql.includes("limit $") ? Number(parameters.at(-1)) : undefined;
      const matching = [...values]
        .filter(([key]) => key.startsWith(prefix) && (!startAfter || key > startAfter))
        .toSorted(([left], [right]) => left.localeCompare(right));
      return queryResult(
        (limit === undefined ? matching : matching.slice(0, limit)).map(([key, encoded_value]) => ({
          key,
          encoded_value,
        })),
      );
    }
    const encoded_value = values.get(String(parameters[0]));
    return queryResult(encoded_value === undefined ? [] : [{ encoded_value }]);
  };
  const query = vi.fn<
    (text: string, parameters?: unknown[]) => Promise<QueryResult<QueryResultRow>>
  >(async (text, parameters) => {
    const result = execute(committed, text, parameters);
    if (/^\s*(insert|delete)/i.test(text)) revision++;
    return result;
  });
  const connect = vi.fn<() => Promise<PoolClient>>(async () => {
    let snapshot = new Map<string, string>();
    let startedAt = 0;
    const clientQuery = vi.fn<
      (text: string, parameters?: unknown[]) => Promise<QueryResult<QueryResultRow>>
    >(async (text, parameters) => {
      const sql = text.trim().toLowerCase();
      if (sql.startsWith("begin")) {
        snapshot = new Map(committed);
        startedAt = revision;
        return queryResult([]);
      }
      if (sql === "rollback") return queryResult([]);
      if (sql === "commit") {
        if (revision !== startedAt) throw postgresError("40001", "checkpoint claim contention");
        committed = snapshot;
        revision++;
        return queryResult([]);
      }
      return execute(snapshot, text, parameters);
    });
    return { query: clientQuery, release: vi.fn<() => void>() } as unknown as PoolClient;
  });
  return { query, connect, end: vi.fn<() => Promise<void>>() } as unknown as Pool;
}

function transactionClientQuery() {
  return vi.fn<(text: string, values?: unknown[]) => Promise<QueryResult<QueryResultRow>>>(
    async () => queryResult([]),
  );
}

function transactionClientRelease() {
  return vi.fn<(error?: Error | boolean) => void>();
}

function postgresError(code: string, message: string): Error & { code: string } {
  return Object.assign(new Error(message), { code });
}

function queryResult<T extends QueryResultRow>(rows: T[]): QueryResult<T> {
  return {
    command: "",
    rowCount: rows.length,
    oid: 0,
    fields: [],
    rows,
  };
}
