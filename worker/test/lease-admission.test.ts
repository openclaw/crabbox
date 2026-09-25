import { describe, expect, it, vi } from "vitest";

import { commitLeaseAdmission, fetchReplayableLeaseCreate } from "../src/lease-admission";
import { ProvisioningTestRuntime, ProvisioningTestStorage } from "./provisioning-fixtures";

const body = {
  leaseID: "cbx_ca1100000091",
  createAttemptID: "cat_91000000000000000000000000000091",
};
const reset = () => new Error("Durable Object's storage operation failed and was reset.");
const request = (method: string, path: string, input: unknown = body) =>
  new Request(`https://example.com${path}`, {
    method,
    headers: { "content-type": "application/json", "x-crabbox-owner": "alice@example.com" },
    body: method === "GET" ? undefined : JSON.stringify(input),
  });

describe("legacy admission retry boundaries", () => {
  it.each([reset(), new Error("permanent storage failure"), new Error("provider HTTP 500")])(
    "does not retry unclassified or reset storage failures: %s",
    async (error) => {
      const runtime = new ProvisioningTestRuntime(new ProvisioningTestStorage());
      const commit = vi.fn<() => Promise<never>>(async () => {
        throw error;
      });
      const reread = vi.fn<() => Promise<undefined>>(async () => undefined);
      await expect(commitLeaseAdmission(runtime, commit, reread)).rejects.toBe(error);
      expect(commit).toHaveBeenCalledTimes(1);
      expect(reread).toHaveBeenCalledTimes(1);
    },
  );

  it("does not extend the retry budget after a slow transaction", async () => {
    vi.useFakeTimers();
    try {
      const runtime = new ProvisioningTestRuntime(new ProvisioningTestStorage());
      const commit = vi.fn<() => Promise<never>>(async () => {
        vi.setSystemTime(Date.now() + 500);
        throw Object.assign(new Error("transient"), { retryable: true });
      });
      await expect(commitLeaseAdmission(runtime, commit, async () => undefined)).rejects.toThrow(
        "transient",
      );
      expect(commit).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not start a retry when the backoff timer resumes after its budget", async () => {
    const now = vi
      .spyOn(Date, "now")
      .mockReturnValueOnce(0)
      .mockReturnValueOnce(0)
      .mockReturnValue(600);
    try {
      const runtime = new ProvisioningTestRuntime(new ProvisioningTestStorage());
      const commit = vi.fn<() => Promise<never>>(async () => {
        throw Object.assign(new Error("transient"), { retryable: true });
      });
      await expect(commitLeaseAdmission(runtime, commit, async () => undefined)).rejects.toThrow(
        "transient",
      );
      expect(commit).toHaveBeenCalledTimes(1);
    } finally {
      now.mockRestore();
    }
  });

  it("replays only the original bound POST once after a runtime reset", async () => {
    const observed: unknown[] = [];
    const fetch = vi.fn<(request: Request) => Promise<Response>>(async (next: Request) => {
      observed.push([next.headers.get("x-crabbox-owner"), await next.json()]);
      if (observed.length === 1) throw reset();
      return Response.json({ lease: { id: body.leaseID, state: "provisioning" } });
    });
    const response = await fetchReplayableLeaseCreate(request("POST", "/v1/leases"), fetch);
    expect(response.status).toBe(200);
    expect(observed).toEqual([
      ["alice@example.com", body],
      ["alice@example.com", body],
    ]);
  });

  it.each([
    ["POST", "/v1/leases", { leaseID: body.leaseID }],
    ["POST", "/v1/leases", { ...body, createAttemptID: "bad" }],
    ["POST", "/v1/runs/run_example/finish", body],
    ["POST", "/v1/leases/from-checkpoint", body],
    ["PUT", `/v1/leases/${body.leaseID}`, body],
    ["GET", `/v1/leases/${body.leaseID}`, body],
  ])("does not expand replay contracts for %s %s", async (method, path, input) => {
    const fetch = vi.fn<() => Promise<never>>(async () => {
      throw reset();
    });
    await expect(
      fetchReplayableLeaseCreate(request(method as string, path as string, input), fetch),
    ).rejects.toThrow("reset");
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it("leaves returned 5xx and non-reset exceptions to the caller", async () => {
    const fetch = vi.fn<() => Promise<Response>>(
      async () => new Response("provider unavailable", { status: 500 }),
    );
    expect((await fetchReplayableLeaseCreate(request("POST", "/v1/leases"), fetch)).status).toBe(
      500,
    );
    expect(fetch).toHaveBeenCalledTimes(1);
    const throwing = vi.fn<() => Promise<never>>(async () => {
      throw new Error("connection closed");
    });
    await expect(
      fetchReplayableLeaseCreate(request("POST", "/v1/leases"), throwing),
    ).rejects.toThrow("connection closed");
    expect(throwing).toHaveBeenCalledTimes(1);
  });

  it("stops after one boundary replay even when the new runtime resets", async () => {
    const fetch = vi.fn<() => Promise<never>>(async () => {
      throw reset();
    });
    await expect(fetchReplayableLeaseCreate(request("POST", "/v1/leases"), fetch)).rejects.toThrow(
      "reset",
    );
    expect(fetch).toHaveBeenCalledTimes(2);
  });
});
