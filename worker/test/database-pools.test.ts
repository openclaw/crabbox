import { EventEmitter } from "node:events";
import { readFile } from "node:fs/promises";

import { Pool } from "pg";
import { describe, expect, it, vi } from "vitest";

import {
  DatabasePoolsObservation,
  databasePoolsResponse,
  type DatabasePoolsSnapshot,
} from "../node/database-pools";
import { prepareCoordinatorRequest } from "../src/coordinator-entry";
import type { Env } from "../src/types";

function observedPool(total: number, idle: number, waiting: number, max = 10): Pool {
  return Object.assign(new EventEmitter(), {
    totalCount: total,
    idleCount: idle,
    waitingCount: waiting,
    options: { max },
  }) as Pool;
}

describe("database pool observation", () => {
  it("reports each pool independently and retains demand between observations", () => {
    const storage = observedPool(6, 2, 3, 8);
    const jobs = observedPool(3, 1, 0);
    const observation = new DatabasePoolsObservation(storage, jobs);
    storage.emit("acquire");
    storage.emit("acquire");
    storage.emit("release");
    jobs.emit("acquire");

    const first = observation.snapshot();
    const second = observation.snapshot();

    expect(first).toMatchObject({
      schema: "crabbox.database-pools.v1",
      processId: process.pid,
      pools: {
        storage: { total: 6, idle: 2, waiting: 3, active: 4, max: 8, acquired: 2, released: 1 },
        jobs: { total: 3, idle: 1, waiting: 0, active: 2, max: 10, acquired: 1, released: 0 },
      },
    });
    expect(first.processInstanceId).toMatch(/^[a-f0-9-]{36}$/);
    expect(Date.parse(first.startedAt)).not.toBeNaN();
    expect(Date.parse(first.observedAt)).not.toBeNaN();
    expect(second.processInstanceId).toBe(first.processInstanceId);
    expect(second.startedAt).toBe(first.startedAt);
    expect(second.observedAtMonotonicMs).toBeGreaterThanOrEqual(first.observedAtMonotonicMs);
    const restarted = new DatabasePoolsObservation(observedPool(0, 0, 0), observedPool(0, 0, 0));
    expect(restarted.snapshot().processInstanceId).not.toBe(first.processInstanceId);
    expect(restarted.snapshot().pools.storage.acquired).toBe(0);
  });

  it("reads the supported gauges without acquiring a connection or issuing SQL", async () => {
    const storage = new Pool();
    const jobs = new Pool();
    const queries = [vi.spyOn(storage, "query"), vi.spyOn(jobs, "query")];
    const connections = [vi.spyOn(storage, "connect"), vi.spyOn(jobs, "connect")];
    const observation = new DatabasePoolsObservation(storage, jobs);

    expect(observation.snapshot().pools.storage).toMatchObject({
      total: 0,
      idle: 0,
      waiting: 0,
      active: 0,
      max: 10,
    });
    for (const spy of [...queries, ...connections]) expect(spy).not.toHaveBeenCalled();
    await Promise.all([storage.end(), jobs.end()]);
  });

  it.each([
    [-1, 0, 0, 10],
    [1, 2, 0, 10],
    [1, 0, Number.NaN, 10],
    [1, 0, 0, 0],
  ])("rejects invalid native gauges instead of reporting zero", (total, idle, waiting, max) => {
    const observation = new DatabasePoolsObservation(
      observedPool(total, idle, waiting, max),
      observedPool(0, 0, 0),
    );
    expect(() => observation.snapshot()).toThrow("database pool observation unavailable");
  });
});

describe("database pool admin endpoint", () => {
  const env = {
    CRABBOX_ADMIN_TOKEN: "synthetic-admin-token",
    CRABBOX_SHARED_TOKEN: "synthetic-shared-token",
    CRABBOX_SHARED_OWNER: "alice@example.com",
    CRABBOX_DEFAULT_ORG: "example-org",
  } as Env;
  const url = "http://localhost:8080/v1/admin/database-pools";

  it.each([
    [undefined, 401],
    ["wrong-token", 401],
    ["synthetic-shared-token", 403],
  ])("rejects token %s before observing, including forged admin headers", async (token, status) => {
    const headers = new Headers({ "x-crabbox-admin": "true" });
    if (token) headers.set("authorization", `Bearer ${token}`);
    const prepared = await prepareCoordinatorRequest(new Request(url, { headers }), env);
    const observe = vi.fn<() => DatabasePoolsSnapshot>();
    const response =
      "response" in prepared
        ? prepared.response
        : databasePoolsResponse(prepared.request, prepared.authenticated, observe);
    expect(response?.status).toBe(status);
    expect(observe).not.toHaveBeenCalled();
  });

  it("uses the existing admin authentication chain and returns a no-store snapshot", async () => {
    const prepared = await prepareCoordinatorRequest(
      new Request(url, { headers: { authorization: "Bearer synthetic-admin-token" } }),
      env,
    );
    if ("response" in prepared) throw new Error("admin authentication failed");
    const observation = new DatabasePoolsObservation(observedPool(2, 1, 0), observedPool(1, 1, 0));
    const snapshot = observation.snapshot();
    const observe = vi.fn<() => DatabasePoolsSnapshot>(() => snapshot);
    const response = databasePoolsResponse(prepared.request, prepared.authenticated, observe)!;
    expect(response.status).toBe(200);
    expect(response.headers.get("cache-control")).toBe("no-store");
    expect(await response.json()).toEqual(snapshot);
    expect(observe).toHaveBeenCalledOnce();
  });

  it.each([
    ["POST", "", 405],
    ["GET", "?reset=1", 400],
  ])("rejects %s %s before observing", (method, query, status) => {
    const observe = vi.fn<() => DatabasePoolsSnapshot>();
    const response = databasePoolsResponse(
      new Request(url + query, { method, headers: { "x-crabbox-admin": "true" } }),
      true,
      observe,
    );
    expect(response?.status).toBe(status);
    expect(observe).not.toHaveBeenCalled();
  });

  it("redacts an unavailable observation", async () => {
    const response = databasePoolsResponse(
      new Request(url, { headers: { "x-crabbox-admin": "true" } }),
      true,
      () => {
        throw new Error("sensitive detail");
      },
    )!;
    expect(response.status).toBe(503);
    expect(await response.text()).not.toContain("sensitive detail");
  });

  it("handles snapshots after authentication and before any fleet or readiness operation", async () => {
    const source = await readFile(new URL("../node/server.ts", import.meta.url), "utf8");
    const start = source.indexOf("const prepared = await prepareCoordinatorRequest");
    const endpoint = source.indexOf("databasePoolsResponse(prepared.request", start);
    expect(endpoint).toBeGreaterThan(start);
    expect(endpoint).toBeLessThan(
      source.indexOf("requiresAWSDeploymentReadiness(prepared.request)", start),
    );
    expect(endpoint).toBeLessThan(source.indexOf("await runFleetRequest(webRequest)", start));
  });
});
