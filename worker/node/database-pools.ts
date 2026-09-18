import { randomUUID } from "node:crypto";
import { performance } from "node:perf_hooks";

import type { Pool } from "pg";

import { isAdminRequest } from "../src/auth";

export interface DatabasePoolSnapshot {
  total: number;
  idle: number;
  waiting: number;
  // Native pool occupancy includes clients connecting as well as checked-out clients.
  // It is not PostgreSQL's count of currently executing SQL statements.
  active: number;
  max: number;
  acquired: number;
  released: number;
  countersSaturated: boolean;
}

export interface DatabasePoolsSnapshot {
  schema: "crabbox.database-pools.v1";
  processInstanceId: string;
  processId: number;
  startedAt: string;
  observedAt: string;
  observedAtMonotonicMs: number;
  pools: { storage: DatabasePoolSnapshot; jobs: DatabasePoolSnapshot };
}

// Gauges are instantaneous. Cumulative acquire/release events retain short-lived
// demand between samples; neither source claims an unobserved interval peak.
class PoolObservation {
  private acquired = 0;
  private released = 0;

  constructor(private readonly pool: Pool) {
    pool.on("acquire", () => {
      this.acquired = Math.min(Number.MAX_SAFE_INTEGER, this.acquired + 1);
    });
    pool.on("release", () => {
      this.released = Math.min(Number.MAX_SAFE_INTEGER, this.released + 1);
    });
  }

  snapshot(): DatabasePoolSnapshot {
    const total = this.pool.totalCount;
    const idle = this.pool.idleCount;
    const waiting = this.pool.waitingCount;
    const max = this.pool.options.max;
    if (
      ![total, idle, waiting, max].every((value) => Number.isSafeInteger(value) && value >= 0) ||
      max < 1 ||
      idle > total
    ) {
      throw new Error("database pool observation unavailable");
    }
    return {
      total,
      idle,
      waiting,
      active: total - idle,
      max,
      acquired: this.acquired,
      released: this.released,
      countersSaturated:
        this.acquired === Number.MAX_SAFE_INTEGER || this.released === Number.MAX_SAFE_INTEGER,
    };
  }
}

export class DatabasePoolsObservation {
  private readonly processInstanceId = randomUUID();
  private readonly startedAt = new Date().toISOString();
  private readonly storage: PoolObservation;
  private readonly jobs: PoolObservation;

  constructor(storage: Pool, jobs: Pool) {
    this.storage = new PoolObservation(storage);
    this.jobs = new PoolObservation(jobs);
  }

  snapshot(): DatabasePoolsSnapshot {
    return {
      schema: "crabbox.database-pools.v1",
      processInstanceId: this.processInstanceId,
      processId: process.pid,
      startedAt: this.startedAt,
      observedAt: new Date().toISOString(),
      observedAtMonotonicMs: performance.now(),
      pools: { storage: this.storage.snapshot(), jobs: this.jobs.snapshot() },
    };
  }
}

// Call only after prepareCoordinatorRequest has verified and replaced the auth
// context headers. This read does not enter the fleet queue or access the DB.
export function databasePoolsResponse(
  request: Request,
  authenticated: boolean,
  observe: () => DatabasePoolsSnapshot,
): Response | undefined {
  const url = new URL(request.url);
  if (url.pathname !== "/v1/admin/database-pools") return undefined;
  const headers = { "cache-control": "no-store" };
  if (!authenticated) return Response.json({ error: "unauthorized" }, { status: 401, headers });
  if (!isAdminRequest(request)) {
    return Response.json(
      { error: "forbidden", message: "admin token required" },
      { status: 403, headers },
    );
  }
  if (request.method !== "GET") {
    return Response.json(
      { error: "method_not_allowed" },
      { status: 405, headers: { ...headers, allow: "GET" } },
    );
  }
  if (url.searchParams.size > 0) {
    return Response.json({ error: "query_parameters_not_allowed" }, { status: 400, headers });
  }
  try {
    return Response.json(observe(), { headers });
  } catch {
    return Response.json(
      { error: "database_pool_observation_unavailable" },
      { status: 503, headers },
    );
  }
}
