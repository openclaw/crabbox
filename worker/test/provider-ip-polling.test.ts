import { afterEach, describe, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import { GCPClient } from "../src/gcp";
import type { Env, ProviderMachine } from "../src/types";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const machine = (provider: "aws" | "gcp", host = ""): ProviderMachine => ({
  provider,
  id: 1,
  cloudID: "instance",
  name: "instance",
  status: "running",
  serverType: "small",
  host,
  labels: {},
});

for (const provider of ["aws", "gcp"] as const) {
  const timeout = provider === "aws" ? 600_000 : 120_000;
  function fixture(read: () => Promise<ProviderMachine>) {
    if (provider === "aws") {
      const client = new EC2SpotClient(
        { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "test" } as Env,
        "us-east-1",
      );
      vi.spyOn(client, "findServer").mockImplementation(read);
      return () => client.waitForServerIP("instance");
    }
    const client = new GCPClient({
      GCP_PROJECT_ID: "example-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    } as Env);
    vi.spyOn(client, "getServer").mockImplementation(read);
    return () => client.waitForServerIP("instance");
  }
  describe(`${provider} address polling`, () => {
    it("finds an early address using bounded exponential intervals", async () => {
      vi.useFakeTimers();
      vi.setSystemTime(0);
      const reads: number[] = [];
      const run = fixture(async () => {
        reads.push(Date.now());
        return machine(provider, reads.length === 7 ? "192.0.2.10" : "");
      });
      const pending = run();
      await vi.runAllTimersAsync();
      expect((await pending).host).toBe("192.0.2.10");
      expect(reads).toEqual([0, 250, 750, 1750, 3750, 7750, 12750]);
    });
    it("clamps the last wait and admits no read at the existing deadline", async () => {
      vi.useFakeTimers();
      vi.setSystemTime(0);
      const reads: number[] = [];
      const run = fixture(async () => {
        reads.push(Date.now());
        return machine(provider);
      });
      const pending = run().catch((error: unknown) => error);
      await vi.runAllTimersAsync();
      expect(await pending).toBeInstanceOf(Error);
      expect(Date.now()).toBe(timeout);
      expect(reads.every((at) => at < timeout)).toBe(true);
    });
    it("rejects an address whose read completes after the deadline", async () => {
      vi.useFakeTimers();
      vi.setSystemTime(0);
      const run = fixture(async () => {
        vi.setSystemTime(timeout + 1);
        return machine(provider, "192.0.2.10");
      });
      await expect(run()).rejects.toThrow(/timed? ?out|timeout/);
      expect(vi.getTimerCount()).toBe(0);
    });
    it("does not turn a provider throttling error into faster polling", async () => {
      vi.useFakeTimers();
      const throttled = new Error("provider HTTP 429: throttled; retry after 30 seconds");
      const read = vi.fn<() => Promise<ProviderMachine>>(async () => {
        throw throttled;
      });
      await expect(fixture(read)()).rejects.toBe(throttled);
      expect(read).toHaveBeenCalledTimes(1);
      expect(vi.getTimerCount()).toBe(0);
    });
  });
}
