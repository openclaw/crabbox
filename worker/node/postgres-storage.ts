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
    const result = await this.pool.query<{ value: T }>(
      `select value from ${table} where key = $1`,
      [key],
    );
    return result.rows[0]?.value;
  }

  async put<T>(key: string, value: T): Promise<void> {
    await this.pool.query(
      `
        insert into ${table} (key, value)
        values ($1, $2::jsonb)
        on conflict (key) do update
        set value = excluded.value, updated_at = now()
      `,
      [key, JSON.stringify(value)],
    );
  }

  async delete(key: string): Promise<void> {
    await this.pool.query(`delete from ${table} where key = $1`, [key]);
  }

  async list<T>({ prefix = "" }: { prefix?: string } = {}): Promise<Map<string, T>> {
    const result = await this.pool.query<{ key: string; value: T }>(
      `
        select key, value
        from ${table}
        where key like $1 escape '\\'
        order by key
      `,
      [`${escapeLike(prefix)}%`],
    );
    return new Map(result.rows.map((row) => [row.key, row.value]));
  }
}

function escapeLike(value: string): string {
  return value.replace(/[\\%_]/g, "\\$&");
}

function positiveInt(value: string | undefined, fallback: number): number {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : fallback;
}
