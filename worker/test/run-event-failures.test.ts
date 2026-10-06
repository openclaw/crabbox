import { describe, expect, it, vi } from "vitest";

import worker from "../src/index";
import type { Env } from "../src/types";

describe("run-event Worker boundary", () => {
  it.each([
    "Durable Object's isolate exceeded its memory limit and was reset.",
    "synthetic body stream disconnected",
  ])("returns a structured failure without replay after %s", async (message) => {
    const fetch = vi.fn<() => Promise<Response>>(async () => {
      throw new Error(message);
    });
    const env = {
      CRABBOX_SHARED_TOKEN: "test-shared",
      CRABBOX_SHARED_OWNER: "alice@example.com",
      CRABBOX_DEFAULT_ORG: "example-org",
      FLEET: { idFromName: () => "default", get: () => ({ fetch }) },
    } as unknown as Env;
    const response = await worker.fetch(
      new Request("https://coordinator.test/v1/runs/run_example/events", {
        method: "POST",
        headers: { authorization: "Bearer test-shared", "content-type": "application/json" },
        body: JSON.stringify({ type: "stdout", data: "test output" }),
      }),
      env,
    );
    expect(response.status).toBe(503);
    expect(await response.json()).toEqual({ error: "run_event_append_unavailable" });
    expect(fetch).toHaveBeenCalledTimes(1);
  });
});
