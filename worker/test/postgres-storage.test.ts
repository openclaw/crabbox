import type { Pool, PoolClient, QueryResult, QueryResultRow } from "pg";
import { describe, expect, it, vi } from "vitest";

import { PostgresCoordinatorStorage } from "../node/postgres-storage";
import { sha256Hex } from "../src/auth";
import {
  CheckpointError,
  acquireCheckpointUse,
  finishCheckpointUse,
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
import type { CoordinatorRuntime } from "../src/coordinator-runtime";
import {
  FleetCoordinator,
  readyPoolSeedDigestV1,
  backfillCheckpointCreateAttempt,
} from "../src/fleet";
import { orgKeyForLabel } from "../src/org-identity";
import type {
  CoordinatorCheckpointRecord,
  CoordinatorCheckpointUseClaim,
  CreateAttemptRecord,
  Env,
  LeaseRecord,
  ReadyPoolEntry,
  ReadyPoolIdentityV1,
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

  it.each(["40001", "40P01"])(
    "revalidates checkpoint completion after a %s commit retry",
    async (code) => {
      const pool = fakeContendedCheckpointPool();
      const storage = new PostgresCoordinatorStorage("postgres://unused", pool);
      const id = "chk_postgres_finish_retry";
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

      const principal = { owner: "alice@example.com", org };
      const claim = await acquireCheckpointUse(storage, id, principal);
      const leaseID = "cbx_000000000002";
      const attemptID = `cat_${"f".repeat(32)}`;
      const attempt = {
        version: 1,
        requestedLeaseID: leaseID,
        token: attemptID,
        owner: principal.owner,
        org,
        state: "pending",
        canonicalLeaseID: leaseID,
        checkpointID: id,
        checkpointUseClaimHash: await sha256Hex(claim.token),
        createdAt: now,
        updatedAt: now,
      } satisfies CreateAttemptRecord;
      await storage.put(`create-attempt:${leaseID}`, attempt);
      await bindCheckpointUseProvisioning(storage, id, claim.token, principal, attemptID, leaseID);
      await storage.put(`lease:${leaseID}`, {
        id: leaseID,
        state: "active",
        checkpointID: id,
        createAttemptID: attemptID,
      });
      const before = await storage.get(checkpointKey(id));
      const originalConnect = pool.connect.bind(pool);
      let injected = false;
      let connections = 0;
      pool.connect = (async () => {
        connections++;
        const client = await originalConnect();
        const originalQuery = client.query.bind(client);
        client.query = (async (sql: string, parameters?: unknown[]) => {
          if (sql.trim().toLowerCase() === "commit" && !injected) {
            injected = true;
            await storage.put(`create-attempt:${leaseID}`, { ...attempt, state: "canceled" });
            throw postgresError(code, "concurrent cancellation");
          }
          return originalQuery(sql, parameters);
        }) as typeof client.query;
        return client;
      }) as typeof pool.connect;
      await expect(
        finishCheckpointUse(storage, id, claim.token, principal, true),
      ).rejects.toMatchObject({ code: "checkpoint_in_use" });
      expect(injected).toBe(true);
      expect(connections).toBe(2);
      expect(await storage.get(checkpointKey(id))).toEqual(before);
      expect(await storage.list({ prefix: `checkpoint-use:${id}:` })).toHaveLength(1);
      expect(await storage.get(`create-attempt:${leaseID}`)).toMatchObject({ state: "canceled" });
    },
  );

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

  it("keeps new typed PostgreSQL pool records invisible to shipped legacy scans and borrow after rollback", async () => {
    const { storage, env, headers, leaseID, metadata, identity, poolRequest, newWorker } =
      await postgresReadyPoolFixture();
    const reconcile = await newWorker.fetch(
      poolRequest("reconcile-identity", {
        ...metadata,
        identity,
        minReady: 2,
        maxReady: 2,
        claim: true,
      }),
    );
    expect(reconcile.status).toBe(200);
    const claim = (await reconcile.json()) as { claim: { token: string } };
    const register = await newWorker.fetch(
      poolRequest("register-identity", { leaseID, ...metadata, identity }),
    );
    expect(register.status).toBe(200);

    expect([...(await storage.list({ prefix: "ready-pool:" })).values()]).toEqual([]);
    expect([...(await storage.list({ prefix: "ready-pool-fill-claim:" })).values()]).toEqual([]);
    expect([...(await storage.list({ prefix: "ready-pool-desired:" })).values()]).toEqual([]);
    expect(await storage.get(`typed-ready-pool-v1:builders:${leaseID}`)).toBeTruthy();
    expect(await storage.get(`typed-ready-pool-v1-fill-claim:${claim.claim.token}`)).toBeTruthy();
    expect((await storage.list({ prefix: "typed-ready-pool-v1-desired:" })).size).toBe(1);

    const rolledBackWorker = new FleetCoordinator(postgresTestRuntime(storage), env);
    const legacyStatus = await rolledBackWorker.fetch(
      new Request("https://coordinator.test/v1/ready-pools/builders", { headers }),
    );
    expect(await legacyStatus.json()).toEqual({ pool: [] });
    const legacyBorrow = await rolledBackWorker.fetch(poolRequest("borrow", metadata));
    expect(legacyBorrow.status).toBe(409);
    expect(
      (await storage.get<ReadyPoolEntry>(`typed-ready-pool-v1:builders:${leaseID}`))?.state,
    ).toBe("ready");
  });

  it.each([false, true])(
    "preserves PostgreSQL pool ownership and quarantine across restart (typed source: %s)",
    async (typed) => {
      const { pool, storage, env, leaseID, metadata, identity, poolRequest, newWorker } =
        await postgresReadyPoolFixture();
      const source = typed ? "-identity" : "";
      const destination = typed ? "" : "-identity";
      const sourceBody = { ...metadata, ...(typed ? { identity } : {}) };
      const destinationBody = { ...metadata, ...(!typed ? { identity } : {}) };
      const registered = await newWorker.fetch(
        poolRequest(`register${source}`, { leaseID, ...sourceBody }),
      );
      expect(registered.status).toBe(200);
      const { entry: ready } = (await registered.json()) as { entry: ReadyPoolEntry };
      const borrowed = await newWorker.fetch(
        poolRequest(`borrow${source}`, { ...sourceBody, heartbeat: true }),
      );
      expect(borrowed.status).toBe(200);
      const { entry: busy } = (await borrowed.json()) as { entry: ReadyPoolEntry };
      const duplicateRegister = await newWorker.fetch(
        poolRequest(`register${destination}`, { leaseID, ...destinationBody }),
      );
      expect(duplicateRegister.status).toBe(409);

      await storage.put(`${typed ? "ready-pool" : "typed-ready-pool-v1"}:builders:${leaseID}`, {
        ...ready,
        identity: undefined,
        ...destinationBody,
      });
      const reopenedStorage = new PostgresCoordinatorStorage("postgres://unused", pool);
      const reopened = new FleetCoordinator(postgresTestRuntime(reopenedStorage), env);
      expect(
        (await reopened.fetch(poolRequest(`borrow${destination}`, destinationBody))).status,
      ).toBe(409);
      const clock = vi.spyOn(Date, "now").mockReturnValue(Date.parse(busy.borrowExpiresAt!));
      try {
        const expiredReturn = await reopened.fetch(
          poolRequest(`return${source}`, {
            leaseID,
            ...sourceBody,
            borrowToken: busy.borrowToken,
          }),
        );
        expect(expiredReturn.status).toBe(409);
        expect(await expiredReturn.json()).toMatchObject({ error: "borrow_expired" });
        const persisted = await reopenedStorage.get<ReadyPoolEntry>(
          `${typed ? "typed-ready-pool-v1" : "ready-pool"}:builders:${leaseID}`,
        );
        expect(persisted).toMatchObject({ state: "quarantined", failureCount: 1 });
        expect(persisted).not.toHaveProperty("borrowToken");
        const afterQuarantine = new FleetCoordinator(postgresTestRuntime(reopenedStorage), env);
        const reRegister = await afterQuarantine.fetch(
          poolRequest(`register${destination}`, { leaseID, ...destinationBody }),
        );
        expect(reRegister.status).toBe(409);
        expect(await reRegister.json()).toMatchObject({ error: "pool_entry_quarantined" });
        expect(
          (await afterQuarantine.fetch(poolRequest(`borrow${destination}`, destinationBody)))
            .status,
        ).toBe(409);
      } finally {
        clock.mockRestore();
      }
    },
  );
});

async function postgresReadyPoolFixture() {
  const pool = statefulFakePool();
  const storage = new PostgresCoordinatorStorage("postgres://unused", pool);
  const runtime = postgresTestRuntime(storage);
  const env = { CRABBOX_DEFAULT_ORG: "example-org" } as Env;
  const headers = {
    "x-crabbox-owner": "alice@example.com",
    "x-crabbox-org": "example-org",
    "content-type": "application/json",
  };
  const leaseID = "cbx_000000000099";
  const lease: LeaseRecord = {
    id: leaseID,
    provider: "aws",
    target: "linux",
    architecture: "amd64",
    cloudID: "i-0123456789abcdef0",
    region: "us-east-1",
    owner: "alice@example.com",
    org: orgKeyForLabel("example-org"),
    profile: "default",
    class: "standard",
    serverType: "c6i.large",
    image: {
      id: "ami-0123456789abcdef0",
      source: "promoted",
      provider: "aws",
      kind: "aws-ami",
      region: "us-east-1",
    },
    serverID: 1,
    serverName: "typed-runner",
    providerKey: "typed-key",
    host: "192.0.2.10",
    sshUser: "crabbox",
    sshPort: "22",
    workRoot: "/work/crabbox",
    keep: true,
    ttlSeconds: 3600,
    estimatedHourlyUSD: 1,
    maxEstimatedUSD: 1,
    state: "active",
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    expiresAt: new Date(Date.now() + 60 * 60_000).toISOString(),
  };
  await storage.put(`lease:${leaseID}`, lease);
  const metadata = { repo: "example-org/my-app", ref: "main", commit: "abc123" };
  const identity: ReadyPoolIdentityV1 = {
    schema: "crabbox-ready-pool-identity/v1",
    image: { provider: "aws", scope: "us-east-1", id: "ami-0123456789abcdef0" },
    architecture: "amd64",
    seedDigest: await readyPoolSeedDigestV1(metadata),
    cacheCompatibility: "node-22",
  };
  const poolRequest = (action: string, body: Record<string, unknown>) =>
    new Request(`https://coordinator.test/v1/ready-pools/builders/${action}`, {
      method: "POST",
      headers,
      body: JSON.stringify(body),
    });
  const newWorker = new FleetCoordinator(runtime, env);
  return { pool, storage, env, headers, leaseID, metadata, identity, poolRequest, newWorker };
}

function postgresTestRuntime(storage: PostgresCoordinatorStorage): CoordinatorRuntime {
  let alarm: number | undefined;
  return {
    storage,
    ephemeralWebSocketMaxPayloadBytes: 1024 * 1024,
    runExclusive: async (callback) => await callback(),
    createWebSocketUpgrade() {
      throw new Error("websockets are not used by the PostgreSQL pool fixture");
    },
    getWebSockets: () => [],
    socketAttachment: () => undefined,
    setSocketAttachment: () => undefined,
    acceptWebSocket: () => undefined,
    acceptEphemeralWebSocket: () => undefined,
    take: async (key) => await storage.take(key),
    getAlarm: async () => alarm,
    scheduleAlarm: async (time) => {
      alarm = time;
    },
    clearAlarm: async () => {
      alarm = undefined;
    },
  };
}

function statefulFakePool(): Pool {
  const values = new Map<string, string>();
  const query = vi.fn<(text: string, params?: unknown[]) => Promise<QueryResult<QueryResultRow>>>(
    async (text, params = []) => {
      const sql = text.trim().toLowerCase();
      if (sql.startsWith("insert")) {
        values.set(String(params[0]), String(params[2]));
        return queryResult([]);
      }
      if (sql.startsWith("delete")) {
        const previous = values.get(String(params[0]));
        values.delete(String(params[0]));
        return queryResult(previous === undefined ? [] : [{ encoded_value: previous }]);
      }
      if (sql.includes("where key like $1")) {
        const pattern = String(params[0]);
        const prefix = pattern.slice(0, -1).replaceAll(/\\([\\%_])/g, "$1");
        const after = sql.includes("and key > $2") ? String(params[1]) : undefined;
        let records = [...values.entries()]
          .toSorted(([left], [right]) => left.localeCompare(right))
          .filter(([key]) => key.startsWith(prefix) && (!after || key > after));
        if (sql.includes("limit $")) records = records.slice(0, Number(params.at(-1)));
        return queryResult(records.map(([key, encoded_value]) => ({ key, encoded_value })));
      }
      if (sql.includes("where key = $1")) {
        const encoded = values.get(String(params[0]));
        return queryResult(encoded === undefined ? [] : [{ encoded_value: encoded }]);
      }
      return queryResult([]);
    },
  );
  const connect = vi.fn<() => Promise<PoolClient>>(
    async () => ({ query, release: vi.fn<() => void>() }) as unknown as PoolClient,
  );
  return { query, connect, end: vi.fn<() => Promise<void>>() } as unknown as Pool;
}

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
