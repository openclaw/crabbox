import { Pool } from "pg";

import type { CoordinatorStorage } from "../src/coordinator-runtime";

const schema = "crabbox";
const table = `${schema}.coordinator_kv`;

export class PostgresCoordinatorStorage implements CoordinatorStorage {
  readonly pool: Pool;

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

  async get<T>(key: string): Promise<T | undefined> {
    const result = await this.pool.query<{ encoded_value: unknown }>(
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

  async put<T>(key: string, value: T): Promise<void> {
    const encoded = JSON.stringify(value);
    if (encoded === undefined) {
      throw new TypeError("coordinator storage cannot persist undefined");
    }
    const jsonbEncoded = jsonbCompatibleEncoding(encoded);
    await this.pool.query(
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
    await this.pool.query(`delete from ${table} where key = $1`, [key]);
  }

  async list<T>({ prefix = "" }: { prefix?: string } = {}): Promise<Map<string, T>> {
    const result = await this.pool.query<{ key: string; encoded_value: unknown }>(
      `
        select key,
               case
                 when value_text_updated_at = updated_at then value_text
                 else value::text
               end as encoded_value
        from ${table}
        where key like $1 escape '\\'
        order by key
      `,
      [`${escapeLike(prefix)}%`],
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
