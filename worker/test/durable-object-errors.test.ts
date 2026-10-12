import { afterEach, describe, expect, it, vi } from "vitest";

import worker from "../src/index";
import type { Env } from "../src/types";

const leasePath = "/v1/leases/cbx_000000000001";
const runPath = "/v1/runs/run_" + "a".repeat(32);
const reset = "Durable Object's isolate exceeded its memory limit and was reset.";
const overloadedBody = {
  error: "coordinator_overloaded",
  message: "The coordinator is overloaded. Please retry shortly.",
  retryable: true,
};
const unavailableBody = {
  error: "coordinator_unavailable",
  message: "The coordinator is temporarily unavailable. Please retry shortly.",
  retryable: true,
};

function environment(fetch: (request: Request) => Promise<Response>): Env {
  return {
    CRABBOX_SHARED_TOKEN: "test-shared",
    CRABBOX_SHARED_OWNER: "alice@example.com",
    CRABBOX_DEFAULT_ORG: "example-org",
    FLEET: { idFromName: () => "default", get: () => ({ fetch }) },
  } as unknown as Env;
}

function request(method: string, path: string, body = {}): Request {
  return new Request("https://coordinator.test" + path, {
    method,
    headers: { authorization: "Bearer test-shared", "content-type": "application/json" },
    ...(method === "GET" ? {} : { body: JSON.stringify(body) }),
  });
}

afterEach(() => vi.restoreAllMocks());

describe("Durable Object Worker boundary", () => {
  const failures = [
    {
      label: "overloaded message",
      error: new Error("Durable Object is overloaded. Requests queued for too long."),
      status: 503,
      body: overloadedBody,
      after: "5",
    },
    {
      label: "overloaded flag",
      error: { overloaded: true },
      status: 503,
      body: overloadedBody,
      after: "5",
    },
    {
      label: "memory reset",
      error: new Error(reset),
      status: 503,
      body: unavailableBody,
      after: "2",
    },
    {
      label: "storage reset",
      error: new Error("Internal error in Durable Object storage caused object to be reset."),
      status: 503,
      body: unavailableBody,
      after: "2",
    },
    {
      label: "retryable flag",
      error: { retryable: true, message: "other text" },
      status: 503,
      body: unavailableBody,
      after: "2",
    },
    {
      label: "network lost",
      error: new Error("Network connection lost"),
      status: 503,
      body: unavailableBody,
      after: "2",
    },
    {
      label: "storage timeout",
      error: new Error("Durable Object storage operation exceeded timeout"),
      status: 503,
      body: unavailableBody,
      after: "2",
    },
    {
      label: "application error",
      error: new Error("boom"),
      status: 500,
      body: { error: "coordinator_internal_error" },
      after: null,
    },
    {
      label: "unknown error",
      error: null,
      status: 500,
      body: { error: "coordinator_internal_error" },
      after: null,
    },
  ];
  for (const [method, path] of [
    ["GET", leasePath],
    ["PUT", runPath],
  ]) {
    it.each(failures)(
      `${method} classifies $label without replay`,
      async ({ error, status, body, after }) => {
        const log = vi.spyOn(console, "error").mockImplementation(() => {});
        const fetch = vi.fn<() => Promise<Response>>(async () => {
          throw error;
        });
        const response = await worker.fetch(request(method, path), environment(fetch));
        expect(response.status).toBe(status);
        expect(await response.text()).toBe(JSON.stringify(body));
        expect(response.headers.get("retry-after")).toBe(after);
        expect(response.headers.get("cache-control")).toBe("no-store");
        expect(fetch).toHaveBeenCalledTimes(1);
        expect(log).toHaveBeenCalledTimes(1);
      },
    );
  }

  it("preserves the run-event error body without replay", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const fetch = vi.fn<() => Promise<Response>>(async () => {
      throw new Error(reset);
    });
    const response = await worker.fetch(request("POST", runPath + "/events"), environment(fetch));
    expect(response.status).toBe(503);
    expect(await response.text()).toBe('{"error":"run_event_append_unavailable"}');
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it.each([false, true])(
    "preserves lease-create replay (replay also fails: %s)",
    async (failsAgain) => {
      vi.spyOn(console, "error").mockImplementation(() => {});
      const fetch = vi.fn<() => Promise<Response>>(async () => new Response('{"lease":{}}'));
      fetch.mockRejectedValueOnce(new Error(reset));
      if (failsAgain) fetch.mockRejectedValueOnce(new Error(reset));
      const response = await worker.fetch(
        request("POST", "/v1/leases", {
          leaseID: "cbx_000000000001",
          createAttemptID: "cat_" + "b".repeat(32),
        }),
        environment(fetch),
      );
      expect(fetch).toHaveBeenCalledTimes(2);
      expect(response.status).toBe(failsAgain ? 503 : 200);
      expect(await response.json()).toEqual(failsAgain ? unavailableBody : { lease: {} });
      expect(response.headers.get("retry-after")).toBe(failsAgain ? "2" : null);
    },
  );

  it("handles scheduled failures without rejecting waitUntil", async () => {
    const log = vi.spyOn(console, "error").mockImplementation(() => {});
    const fetch = vi.fn<() => Promise<Response>>(async () => {
      throw new Error(reset);
    });
    const pending: Promise<unknown>[] = [];
    await worker.scheduled({} as ScheduledController, environment(fetch), {
      waitUntil: (promise: Promise<unknown>) => pending.push(promise),
    } as unknown as ExecutionContext);
    expect(pending).toHaveLength(1);
    await expect(pending[0]).resolves.toBeUndefined();
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(log).toHaveBeenCalledTimes(1);
    expect(JSON.parse(log.mock.calls[0][0])).toMatchObject({
      event: "coordinator.durable_object_failure",
      kind: "unavailable",
      method: "POST",
      path: "/v1/internal/scheduled",
    });
  });

  it("logs bounded error metadata once without request secrets", async () => {
    const log = vi.spyOn(console, "error").mockImplementation(() => {});
    const error = Object.assign(new Error("x".repeat(250)), {
      overloaded: true,
      retryable: false,
      remote: true,
    });
    await worker.fetch(
      request("GET", leasePath + "?token=private-query"),
      environment(async () => {
        throw error;
      }),
    );
    expect(log).toHaveBeenCalledTimes(1);
    const line = log.mock.calls[0][0];
    expect(JSON.parse(line)).toEqual({
      event: "coordinator.durable_object_failure",
      kind: "overloaded",
      method: "GET",
      path: leasePath,
      retryable: false,
      overloaded: true,
      remote: true,
      name: "Error",
      message: "x".repeat(200),
    });
    expect(line).not.toContain("private-query");
    expect(line).not.toContain("test-shared");
    expect(line).not.toContain("authorization");
  });
});
