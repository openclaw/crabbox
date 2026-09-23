import { createServer } from "node:http";

import { afterEach, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import { HetznerClient } from "../src/hetzner";
import type { Env } from "../src/types";
import { leaseCost } from "../src/usage";

afterEach(() => vi.unstubAllGlobals());

it.each(["aws", "hetzner"] as const)(
  "%s pricing falls back after five seconds with stalled headers or body",
  async (provider) => {
    const nativeFetch = globalThis.fetch;
    const closed = [Promise.withResolvers<void>(), Promise.withResolvers<void>()];
    const started = [Promise.withResolvers<void>(), Promise.withResolvers<void>()];
    let requests = 0;
    const server = createServer((_request, response) => {
      const index = requests++;
      response.on("close", () => closed[index]!.resolve());
      if (index === 1) {
        response.writeHead(200);
        response.write(provider === "aws" ? "<DescribeSpotPriceHistoryResponse>" : "{");
      }
      started[index]!.resolve();
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    if (!address || typeof address === "string") throw new Error("missing server address");
    vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
      const original = new Request(input, init);
      return nativeFetch(`http://127.0.0.1:${address.port}`, {
        method: original.method,
        signal: init?.signal ?? original.signal,
      });
    });
    const env = {
      AWS_ACCESS_KEY_ID: "test",
      AWS_SECRET_ACCESS_KEY: "test-secret",
      HETZNER_TOKEN: "test",
    } as Env;
    const quote = () =>
      (provider === "aws"
        ? new EC2SpotClient(env, "eu-west-1").hourlySpotPriceUSD("t3.small")
        : new HetznerClient(env).hourlyPriceUSD("cx23", "hel1")
      ).catch(() => undefined);
    const expired = Promise.withResolvers<never>();
    const timer = setTimeout(() => expired.reject(new Error("pricing did not stop")), 6_000);
    try {
      const prices = [quote(), quote()];
      await Promise.all(started.map(({ promise }) => promise));
      const rates = await Promise.race([Promise.all(prices), expired.promise]);
      expect(rates).toEqual([undefined, undefined]);
      expect(leaseCost(env, provider, "unknown", 3600, rates[0]).hourlyUSD).toBe(
        provider === "aws" ? 3 : 0.5,
      );
      await Promise.race([Promise.all(closed.map(({ promise }) => promise)), expired.promise]);
      expect(requests).toBe(2);
    } finally {
      clearTimeout(timer);
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    }
  },
  7_000,
);
