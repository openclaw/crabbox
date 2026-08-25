import type { Pool, PoolClient, QueryResult, QueryResultRow } from "pg";
import { describe, expect, it, vi } from "vitest";

import { PostgresCoordinatorStorage } from "../node/postgres-storage";
import type { CoordinatorRuntime } from "../src/coordinator-runtime";
import {
  FleetCoordinator,
  readyPoolDesiredCapacityKeyV2,
  readyPoolSeedDigestV1,
} from "../src/fleet";
import { orgKeyForLabel } from "../src/org-identity";
import type { Env, LeaseRecord, ReadyPoolEntry, ReadyPoolIdentityV1 } from "../src/types";

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

  it("stops after three serialization failures and rethrows the database error", async () => {
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

    expect(callbackInvocations).toBe(3);
    expect(connect).toHaveBeenCalledTimes(3);
    expect(clients).toHaveLength(3);
    for (const client of clients) {
      expect(client.query.mock.calls.map(([sql]) => String(sql).trim())).toEqual([
        "begin isolation level serializable",
        "rollback",
      ]);
      expect(client.release).toHaveBeenCalledWith();
    }
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
    const desiredKey = await readyPoolDesiredCapacityKeyV2({
      org: orgKeyForLabel("example-org"),
      owner: "alice@example.com",
      key: "builders",
      compatibilityKey: undefined,
      identity,
    });
    expect([...(await storage.list({ prefix: "ready-pool-desired:" })).keys()]).toEqual([]);
    expect(await storage.get(desiredKey)).toBeTruthy();
    expect(await storage.get(`typed-ready-pool-v1:builders:${leaseID}`)).toBeTruthy();
    expect(await storage.get(`typed-ready-pool-v1-fill-claim:${claim.claim.token}`)).toBeTruthy();
    expect((await storage.list({ prefix: "typed-ready-pool-v1-desired:" })).size).toBe(0);

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
});

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
