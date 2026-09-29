import { createServer } from "node:http";

import { afterEach, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import { leaseConfig } from "../src/config";
import type { CoordinatorStorage } from "../src/coordinator-runtime";
import { AWSProvider, HetznerProvider } from "../src/fleet";
import { HetznerClient } from "../src/hetzner";
import { cachedProviderPrice } from "../src/provider-pricing";
import type { Env } from "../src/types";
import { leaseCost } from "../src/usage";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

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

it.each(["aws", "hetzner"] as const)(
  "caches %s quotes across coordinator provider instances for five minutes",
  async (provider) => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const env = {
      AWS_ACCESS_KEY_ID: "fixture",
      AWS_SECRET_ACCESS_KEY: "fixture",
      HETZNER_TOKEN: "fixture",
    } as Env;
    let calls = 0;
    vi.stubGlobal("fetch", async () => {
      calls++;
      return provider === "aws"
        ? new Response(
            "<DescribeSpotPriceHistoryResponse><spotPriceHistorySet><item><spotPrice>0.25</spotPrice></item></spotPriceHistorySet></DescribeSpotPriceHistoryResponse>",
          )
        : Response.json({
            server_types: [
              { name: "cx23", prices: [{ location: "hel1", price_hourly: { gross: "0.25" } }] },
            ],
          });
    });
    const config = leaseConfig({
      provider,
      serverType: provider === "aws" ? "t3.small" : "cx23",
      sshPublicKey: "ssh-ed25519 fixture",
      location: "hel1",
    });
    const quote = (scope = env) =>
      provider === "aws"
        ? new AWSProvider(scope, "eu-west-1", {} as CoordinatorStorage).hourlyPriceUSD(
            "t3.small",
            config,
          )
        : new HetznerProvider(scope).hourlyPriceUSD("cx23", config);
    try {
      expect(await quote()).toBe(provider === "aws" ? 0.25 : 0.27);
      expect(await quote()).toBe(provider === "aws" ? 0.25 : 0.27);
      expect(calls).toBe(1);
      vi.setSystemTime(Date.now() + 299_999);
      await quote();
      expect(calls).toBe(1);
      vi.setSystemTime(Date.now() + 1);
      await quote();
      expect(calls).toBe(2);
      await quote({ ...env });
      expect(calls).toBe(3);
    } finally {
      vi.useRealTimers();
    }
  },
);

it("separates provider, type, region, market and currency pricing dimensions", async () => {
  const env = {
    AWS_ACCESS_KEY_ID: "fixture",
    AWS_SECRET_ACCESS_KEY: "fixture",
    HETZNER_TOKEN: "fixture",
  } as Env;
  const config = leaseConfig({
    provider: "hetzner",
    location: "hel1",
    sshPublicKey: "ssh-ed25519 fixture",
  });
  const hetzner = vi.spyOn(HetznerClient.prototype, "hourlyPriceUSD").mockResolvedValue(0.2);
  const aws = vi.spyOn(EC2SpotClient.prototype, "hourlySpotPriceUSD").mockResolvedValue(0.3);
  const h = new HetznerProvider(env);
  await h.hourlyPriceUSD("cx23", config);
  await h.hourlyPriceUSD("cx33", config);
  await h.hourlyPriceUSD("cx23", { ...config, location: "fsn1" });
  env.CRABBOX_EUR_TO_USD = "1.2";
  await h.hourlyPriceUSD("cx23", config);
  expect(hetzner).toHaveBeenCalledTimes(4);
  const a = new AWSProvider(env, "eu-west-1", {} as CoordinatorStorage);
  const awsConfig = { ...config, awsRegion: "eu-west-1", capacityMarket: "spot" as const };
  await a.hourlyPriceUSD("cx23", awsConfig);
  await a.hourlyPriceUSD("cx23", { ...awsConfig, awsRegion: "eu-west-2" });
  expect(
    await a.hourlyPriceUSD("cx23", { ...awsConfig, capacityMarket: "on-demand" }),
  ).toBeUndefined();
  expect(aws).toHaveBeenCalledTimes(2);
});

it.each([undefined, NaN, Infinity, -1, 0])(
  "does not cache missing or invalid price %s",
  async (value) => {
    const scope = {};
    const quote = vi
      .fn<() => Promise<number | undefined>>()
      .mockResolvedValueOnce(value)
      .mockResolvedValue(0.2);
    expect(await cachedProviderPrice(scope, ["fixture"], quote)).toBeUndefined();
    expect(await cachedProviderPrice(scope, ["fixture"], quote)).toBe(0.2);
    expect(await cachedProviderPrice(scope, ["fixture"], quote)).toBe(0.2);
    expect(quote).toHaveBeenCalledTimes(2);
  },
);

it("falls back on failed quotes, retries them, and never serves an expired quote", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const env = { HETZNER_TOKEN: "fixture" } as Env;
  const config = leaseConfig({ provider: "hetzner", sshPublicKey: "ssh-ed25519 fixture" });
  const quote = vi
    .spyOn(HetznerClient.prototype, "hourlyPriceUSD")
    .mockRejectedValueOnce(new Error("unavailable"))
    .mockResolvedValueOnce(0.2)
    .mockRejectedValueOnce(new Error("unavailable"));
  const price = () => new HetznerProvider(env).hourlyPriceUSD("cx23", config);
  expect(await price()).toBeUndefined();
  expect(await price()).toBe(0.2);
  expect(await price()).toBe(0.2);
  vi.setSystemTime(Date.now() + 300_000);
  expect(await price()).toBeUndefined();
  expect(quote).toHaveBeenCalledTimes(3);
});

it("bounds retained pricing entries and treats synchronous quote failures as unavailable", async () => {
  const scope = {};
  const quote = vi.fn<() => Promise<number>>().mockResolvedValue(0.2);
  await cachedProviderPrice(scope, ["oldest"], quote);
  await Promise.all(
    Array.from({ length: 256 }, (_, index) => cachedProviderPrice(scope, [String(index)], quote)),
  );
  await cachedProviderPrice(scope, ["oldest"], quote);
  expect(quote).toHaveBeenCalledTimes(258);
  expect(
    await cachedProviderPrice(scope, ["failure"], () => {
      throw new Error("unavailable");
    }),
  ).toBeUndefined();
});
