import { Pool, type PoolClient, type QueryResult, type QueryResultRow } from "pg";

import type { CoordinatorStorage, CoordinatorStorageView } from "../src/coordinator-runtime";

const schema = "crabbox";
const table = `${schema}.coordinator_kv`;
const transactionAttempts = 12;
const retryableTransactionErrorCodes = new Set(["40001", "40P01"]);

export class PostgresCoordinatorStorage implements CoordinatorStorage {
  readonly pool: Pool;
  private readonly view: PostgresCoordinatorStorageView;

  constructor(connectionString: string, pool?: Pool) {
    this.pool =
      pool ??
      new Pool({
        connectionString,
        application_name: "crabbox-coordinator",
        max: positiveInt(process.env["CRABBOX_DATABASE_POOL_SIZE"], 10),
        connectionTimeoutMillis: positiveInt(
          process.env["CRABBOX_DATABASE_CONNECT_TIMEOUT_MS"],
          10_000,
        ),
      });
    this.view = new PostgresCoordinatorStorageView(storageQuery(this.pool));
  }

  async initialize(): Promise<void> {
    await this.pool.query(`create schema if not exists ${schema}`);
    await this.pool.query(`
      create table if not exists ${table} (
        key text primary key,
        value jsonb not null,
        updated_at timestamptz not null default now()
      )
    `);
    await this.pool.query(`
      alter table ${table}
      add column if not exists value_text text,
      add column if not exists value_text_updated_at timestamptz
    `);
    await this.pool.query(`
      create index if not exists coordinator_kv_updated_at_idx
      on ${table} (updated_at)
    `);
  }

  async ready(): Promise<void> {
    await this.pool.query("select 1");
  }

  async close(): Promise<void> {
    await this.pool.end();
  }

  async get<T>(key: string, _options?: { noCache?: boolean }): Promise<T | undefined> {
    return this.view.get<T>(key);
  }

  async put<T>(key: string, value: T, _options?: { noCache?: boolean }): Promise<void> {
    await this.view.put(key, value);
  }

  async delete(key: string): Promise<void> {
    await this.view.delete(key);
  }

  async take<T>(key: string): Promise<T | undefined> {
    const result = await this.pool.query<{ encoded_value: unknown }>(
      `
        delete from ${table}
        where key = $1
        returning case
          when value_text_updated_at = updated_at then value_text
          else value::text
        end as encoded_value
      `,
      [key],
    );
    const row = result.rows[0];
    return row ? decodeStoredValue<T>(row.encoded_value) : undefined;
  }

  async list<T>({
    prefix = "",
    limit,
    startAfter,
  }: {
    prefix?: string;
    limit?: number;
    startAfter?: string;
    noCache?: boolean;
  } = {}): Promise<Map<string, T>> {
    return this.view.list<T>({
      prefix,
      ...(limit === undefined ? {} : { limit }),
      ...(startAfter === undefined ? {} : { startAfter }),
    });
  }

  async transaction<T>(callback: (transaction: CoordinatorStorageView) => Promise<T>): Promise<T> {
    const attempt = async (remaining: number): Promise<T> => {
      try {
        return await this.transactionAttempt(callback);
      } catch (error) {
        if (remaining > 1 && retryablePostgresTransactionError(error)) {
          const retries = transactionAttempts - remaining;
          const delay = Math.min(50, 2 ** Math.min(retries, 5)) + Math.floor(Math.random() * 8);
          await new Promise<void>((resolve) => setTimeout(resolve, delay));
          return attempt(remaining - 1);
        }
        throw error;
      }
    };
    return attempt(transactionAttempts);
  }

  private async transactionAttempt<T>(
    callback: (transaction: CoordinatorStorageView) => Promise<T>,
  ): Promise<T> {
    const client = await this.pool.connect();
    let releaseError: Error | undefined;
    try {
      await client.query("begin isolation level serializable");
      const result = await callback(new PostgresCoordinatorStorageView(storageQuery(client)));
      await client.query("commit");
      return result;
    } catch (error) {
      try {
        await client.query("rollback");
      } catch (rollbackError) {
        releaseError =
          rollbackError instanceof Error
            ? rollbackError
            : new Error("PostgreSQL rollback failed", { cause: rollbackError });
        const aggregate = new AggregateError(
          [error, rollbackError],
          "PostgreSQL transaction failed and rollback also failed",
          { cause: error },
        );
        throw aggregate;
      }
      throw error;
    } finally {
      if (releaseError) {
        client.release(releaseError);
      } else {
        client.release();
      }
    }
  }
}

function retryablePostgresTransactionError(error: unknown): boolean {
  if (error instanceof AggregateError || !error || typeof error !== "object") return false;
  const code = "code" in error ? error.code : undefined;
  return typeof code === "string" && retryableTransactionErrorCodes.has(code);
}

type StorageQuery = <T extends QueryResultRow>(
  text: string,
  values?: unknown[],
) => Promise<QueryResult<T>>;

function storageQuery(queryable: Pick<Pool | PoolClient, "query">): StorageQuery {
  return async <T extends QueryResultRow>(text: string, values?: unknown[]) =>
    await queryable.query<T>(text, values);
}

class PostgresCoordinatorStorageView implements CoordinatorStorageView {
  constructor(private readonly query: StorageQuery) {}

  async get<T>(key: string, _options?: { noCache?: boolean }): Promise<T | undefined> {
    const result = await this.query<{ encoded_value: unknown }>(
      `
        select case
          when value_text_updated_at = updated_at then value_text
          else value::text
        end as encoded_value
        from ${table}
        where key = $1
      `,
      [key],
    );
    const row = result.rows[0];
    return row ? decodeStoredValue<T>(row.encoded_value) : undefined;
  }

  async put<T>(key: string, value: T, _options?: { noCache?: boolean }): Promise<void> {
    const encoded = JSON.stringify(value);
    if (encoded === undefined) {
      throw new TypeError("coordinator storage cannot persist undefined");
    }
    const jsonbEncoded = jsonbCompatibleEncoding(encoded);
    await this.query(
      `
        insert into ${table} (key, value, value_text, value_text_updated_at)
        values ($1, $2::jsonb, $3, now())
        on conflict (key) do update
        set value = excluded.value,
            value_text = excluded.value_text,
            value_text_updated_at = now(),
            updated_at = now()
      `,
      [key, jsonbEncoded, encoded],
    );
  }

  async delete(key: string): Promise<void> {
    await this.query(`delete from ${table} where key = $1`, [key]);
  }

  async list<T>({
    prefix = "",
    limit,
    startAfter,
  }: {
    prefix?: string;
    limit?: number;
    startAfter?: string;
    noCache?: boolean;
  } = {}): Promise<Map<string, T>> {
    const values: unknown[] = [`${escapeLike(prefix)}%`];
    const afterClause = startAfter ? `and key > $${values.push(startAfter)}` : "";
    const limitClause = limit === undefined ? "" : `limit $${values.push(limit)}`;
    const result = await this.query<{ key: string; encoded_value: unknown }>(
      `
        select key,
               case
                 when value_text_updated_at = updated_at then value_text
                 else value::text
               end as encoded_value
        from ${table}
        where key like $1 escape '\\'
        ${afterClause}
        order by key
        ${limitClause}
      `,
      values,
    );
    return new Map(result.rows.map((row) => [row.key, decodeStoredValue<T>(row.encoded_value)]));
  }
}

function decodeStoredValue<T>(value: unknown): T {
  return (typeof value === "string" ? JSON.parse(value) : value) as T;
}

function jsonbCompatibleEncoding(encoded: string): string {
  if (!encoded.includes("\\u0000")) return encoded;
  return JSON.stringify(replaceNulCharacters(JSON.parse(encoded)));
}

function replaceNulCharacters(value: unknown): unknown {
  if (typeof value === "string") return value.replaceAll("\0", "\uFFFD");
  if (Array.isArray(value)) return value.map(replaceNulCharacters);
  if (!value || typeof value !== "object") return value;
  return Object.fromEntries(
    Object.entries(value).map(([key, item]) => [
      key.replaceAll("\0", "\uFFFD"),
      replaceNulCharacters(item),
    ]),
  );
}

function escapeLike(value: string): string {
  return value.replace(/[\\%_]/g, "\\$&");
}

function positiveInt(value: string | undefined, fallback: number): number {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : fallback;
}
