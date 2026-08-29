import { createServer } from "node:http";

import { afterEach, describe, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import { HetznerClient } from "../src/hetzner";
import type { Env } from "../src/types";

const awsEnv = { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "test-secret" } as Env;
const region = "eu-west-1";
const spotXML =
  "<DescribeSpotPriceHistoryResponse><spotPriceHistorySet><item><spotPrice>0.125</spotPrice></item></spotPriceHistorySet></DescribeSpotPriceHistoryResponse>";

function quote(provider: "aws" | "hetzner") {
  return provider === "aws"
    ? new EC2SpotClient(awsEnv, region).hourlySpotPriceUSD("t3.small")
    : new HetznerClient({ HETZNER_TOKEN: "test" } as Env).hourlyPriceUSD("cx23", "hel1");
}

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("optional provider pricing", () => {
  it.each(["aws", "hetzner"] as const)(
    "cancels a %s quote with stalled response headers",
    async (provider) => {
      vi.useFakeTimers();
      const started = Promise.withResolvers<void>();
      const release = Promise.withResolvers<void>();
      let requestAborted = false;
      vi.stubGlobal(
        "fetch",
        vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
          const request = input instanceof Request ? input : new Request(input, init);
          const interrupted = new Promise<never>((_resolve, reject) => {
            request.signal.addEventListener(
              "abort",
              () => {
                requestAborted = true;
                reject(request.signal.reason);
              },
              { once: true },
            );
          });
          started.resolve();
          await Promise.race([release.promise, interrupted]);
          return new Response(provider === "aws" ? spotXML : '{"server_types":[]}');
        }),
      );
      let settled = false;
      const price = quote(provider)
        .catch(() => undefined)
        .finally(() => {
          settled = true;
        });
      try {
        await started.promise;
        await vi.advanceTimersByTimeAsync(5_000);
        expect(requestAborted).toBe(true);
        expect(settled).toBe(true);
      } finally {
        release.resolve();
        await price;
      }
    },
  );

  it("preserves successful quotes and clears the deadline", async () => {
    vi.useFakeTimers();
    const fetchImpl = vi.fn<() => Promise<Response>>(async () => new Response(spotXML));
    vi.stubGlobal("fetch", fetchImpl);
    await expect(quote("aws")).resolves.toBe(0.125);
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
    fetchImpl.mockImplementation(async () =>
      Response.json({
        server_types: [
          { name: "cx23", prices: [{ location: "hel1", price_hourly: { gross: "0.1" } }] },
        ],
      }),
    );
    await expect(quote("hetzner")).resolves.toBe(0.11);
    expect(vi.getTimerCount()).toBe(0);
  });

  it.each([429, 503])("does not retry optional AWS pricing after HTTP %s", async (status) => {
    vi.useFakeTimers();
    const fetchImpl = vi.fn<() => Promise<Response>>(
      async () => new Response("unavailable", { status }),
    );
    vi.stubGlobal("fetch", fetchImpl);
    await expect(quote("aws")).rejects.toThrow(String(status));
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
  });

  it("does not abort another operation's cached AWS identity check", async () => {
    vi.useFakeTimers();
    const env = {
      ...awsEnv,
      CRABBOX_AWS_EXPECTED_ACCOUNT_ID: "123456789012",
      CRABBOX_AWS_EXPECTED_REGION: region,
    } as Env;
    const started = Promise.withResolvers<void>();
    const releaseIdentity = Promise.withResolvers<void>();
    const signals: AbortSignal[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        signals.push(request.signal);
        if (signals.length === 2) started.resolve();
        await Promise.race([
          releaseIdentity.promise,
          new Promise<never>((_resolve, reject) =>
            request.signal.addEventListener("abort", () => reject(request.signal.reason), {
              once: true,
            }),
          ),
        ]);
        return new Response(
          "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>test</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>",
        );
      }),
    );
    const client = new EC2SpotClient(env, region);
    const identity = client.verifiedIdentity();
    const price = client.hourlySpotPriceUSD("t3.small").catch(() => undefined);
    try {
      await started.promise;
      await vi.advanceTimersByTimeAsync(5_000);
      await price;
      expect(signals.map((signal) => signal.aborted).toSorted()).toEqual([false, true]);
      releaseIdentity.resolve();
      await expect(identity).resolves.toMatchObject({ account: "123456789012" });
      await expect(client.verifiedIdentity()).resolves.toMatchObject({ account: "123456789012" });
      expect(signals).toHaveLength(2);
    } finally {
      releaseIdentity.resolve();
      await Promise.allSettled([identity, price]);
    }
  });

  it("checks the quote deadline after resolving Node credentials before any HTTP request", async () => {
    vi.useFakeTimers();
    const started = Promise.withResolvers<void>();
    const credentials = Promise.withResolvers<{ accessKeyId: string; secretAccessKey: string }>();
    const fetchImpl = vi.fn<typeof fetch>();
    vi.stubGlobal("fetch", fetchImpl);
    const client = new EC2SpotClient(
      {
        awsCredentialProvider: async () => {
          started.resolve();
          return await credentials.promise;
        },
      } as Env,
      region,
    );
    const price = client.hourlySpotPriceUSD("t3.small").catch((error) => error);
    await started.promise;
    await vi.advanceTimersByTimeAsync(5_000);
    credentials.resolve({ accessKeyId: "test", secretAccessKey: "test-secret" });
    await expect(price).resolves.toMatchObject({ name: "TimeoutError" });
    expect(fetchImpl).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });

  it("closes real HTTP connections stalled before headers and during the body", async () => {
    const nativeFetch = globalThis.fetch;
    const disconnected = Promise.withResolvers<void>();
    let closed = 0;
    const server = createServer((request, response) => {
      response.on("close", () => {
        if (++closed === 2) disconnected.resolve();
      });
      if (request.method === "POST") {
        response.writeHead(200);
        response.write("<DescribeSpotPriceHistoryResponse>");
      }
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    if (!address || typeof address === "string") throw new Error("missing local HTTP address");
    vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      return nativeFetch(new Request(`http://127.0.0.1:${address.port}/`, request));
    });
    try {
      const results = await Promise.allSettled([quote("aws"), quote("hetzner")]);
      expect(results.map((result) => result.status)).toEqual(["rejected", "rejected"]);
      await disconnected.promise;
      expect(closed).toBe(2);
    } finally {
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    }
  }, 15_000);
});
