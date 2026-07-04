import type { Pool, QueryResult, QueryResultRow } from "pg";
import { describe, expect, it, vi } from "vitest";

import { PostgresCoordinatorStorage } from "../node/postgres-storage";

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
});

function fakePool(rows: QueryResultRow[] = []) {
  const query = vi.fn<(text: string, values?: unknown[]) => Promise<QueryResult<QueryResultRow>>>(
    async () => queryResult(rows),
  );
  const end = vi.fn<() => Promise<void>>(async () => undefined);
  return { query, end } as unknown as Pool & { query: typeof query };
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
