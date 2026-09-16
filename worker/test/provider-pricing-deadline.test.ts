import { createServer } from "node:http";

import { afterEach, describe, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import { createAWSProvisioningDiagnostics } from "../src/aws-provisioning-diagnostics";
import type {
  AWSQualificationRequest,
  AWSQualificationResponse,
} from "../src/aws-qualification-contract";
import { HetznerClient } from "../src/hetzner";
import { withPricingDeadline } from "../src/provider-pricing";
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
  vi.restoreAllMocks();
});

describe("optional provider pricing", () => {
  it("retains the supplied credential snapshot for a pricing quote", async () => {
    const credentials = vi.fn<() => Promise<{ accessKeyId: string; secretAccessKey: string }>>();
    const fetchImpl = vi.fn<typeof fetch>().mockResolvedValue(new Response(spotXML));
    vi.stubGlobal("fetch", fetchImpl);
    const client = new EC2SpotClient({ awsCredentialProvider: credentials } as Env, region, {
      accessKeyId: "snapshot-test",
      secretAccessKey: "snapshot-secret",
      sessionToken: "snapshot-session",
      expirationMs: Date.now() + 60_000,
    });
    await expect(client.hourlySpotPriceUSD("t3.small")).resolves.toBe(0.125);
    expect(credentials).not.toHaveBeenCalled();
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    const request = fetchImpl.mock.calls[0]![0] as Request;
    expect(request.headers.get("authorization")).toContain("Credential=snapshot-test/");
    expect(request.headers.get("x-amz-security-token")).toBe("snapshot-session");
  });

  it("rejects an expired pricing snapshot without resolving ambient credentials", async () => {
    const credentials = vi.fn<() => Promise<{ accessKeyId: string; secretAccessKey: string }>>();
    const fetchImpl = vi.fn<typeof fetch>();
    vi.stubGlobal("fetch", fetchImpl);
    const client = new EC2SpotClient({ awsCredentialProvider: credentials } as Env, region, {
      accessKeyId: "snapshot-test",
      secretAccessKey: "snapshot-secret",
      expirationMs: Date.now() - 1,
    });
    await expect(client.hourlySpotPriceUSD("t3.small")).rejects.toThrow(
      "AWS credential snapshot expired",
    );
    expect(credentials).not.toHaveBeenCalled();
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("aborts a fixed-snapshot quote without retrying a late response", async () => {
    vi.useFakeTimers();
    const credentials = vi.fn<() => Promise<{ accessKeyId: string; secretAccessKey: string }>>();
    const started = Promise.withResolvers<void>();
    const response = Promise.withResolvers<Response>();
    let signal: AbortSignal | undefined;
    const fetchImpl = vi.fn<typeof fetch>(async (input, init) => {
      signal = (input instanceof Request ? input : new Request(input, init)).signal;
      started.resolve();
      return await response.promise;
    });
    vi.stubGlobal("fetch", fetchImpl);
    const client = new EC2SpotClient({ awsCredentialProvider: credentials } as Env, region, {
      accessKeyId: "snapshot-test",
      secretAccessKey: "snapshot-secret",
      expirationMs: Date.now() + 60_000,
    });
    let settled = false;
    const price = client.hourlySpotPriceUSD("t3.small").catch((error) => {
      settled = true;
      return error;
    });
    try {
      await started.promise;
      await vi.advanceTimersByTimeAsync(4_999);
      expect(settled).toBe(false);
      await vi.advanceTimersByTimeAsync(1);
      expect(settled).toBe(true);
      expect(signal?.aborted).toBe(true);
      await expect(price).resolves.toMatchObject({ name: "TimeoutError" });
    } finally {
      response.resolve(new Response("unavailable", { status: 503 }));
      await price;
      await vi.advanceTimersByTimeAsync(0);
    }
    expect(credentials).not.toHaveBeenCalled();
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
  });

  it.each([
    { provider: "aws", phase: "fetch" },
    { provider: "aws", phase: "body" },
    { provider: "hetzner", phase: "fetch" },
    { provider: "hetzner", phase: "body" },
  ] as const)(
    "releases $provider pricing while $phase ignores abort",
    async ({ provider, phase }) => {
      vi.useFakeTimers();
      const started = Promise.withResolvers<void>();
      const release = Promise.withResolvers<void>();
      let signal: AbortSignal | undefined;
      const fetchImpl = vi.fn<typeof fetch>(
        async (input: RequestInfo | URL, init?: RequestInit) => {
          signal = (input instanceof Request ? input : new Request(input, init)).signal;
          if (phase === "fetch") {
            started.resolve();
            await release.promise;
          }
          const response = new Response(provider === "aws" ? spotXML : '{"server_types":[]}');
          if (phase === "body") {
            if (provider === "aws") {
              vi.spyOn(response, "text").mockImplementation(async () => {
                started.resolve();
                await release.promise;
                return spotXML;
              });
            } else {
              vi.spyOn(response, "json").mockImplementation(async () => {
                started.resolve();
                await release.promise;
                return { server_types: [] };
              });
            }
          }
          return response;
        },
      );
      vi.stubGlobal("fetch", fetchImpl);
      let settled = false;
      const price = quote(provider)
        .catch((error) => error)
        .finally(() => {
          settled = true;
        });
      try {
        await started.promise;
        await vi.advanceTimersByTimeAsync(4_999);
        expect(settled).toBe(false);
        await vi.advanceTimersByTimeAsync(1);
        expect(settled).toBe(true);
        expect(signal?.aborted).toBe(true);
        await expect(price).resolves.toMatchObject({ name: "TimeoutError" });
      } finally {
        release.resolve();
        await price;
        await vi.advanceTimersByTimeAsync(0);
      }
      expect(fetchImpl).toHaveBeenCalledTimes(1);
      expect(vi.getTimerCount()).toBe(0);
    },
  );

  it.each(["synchronous throw", "rejection"])("clears the deadline after %s", async (mode) => {
    vi.useFakeTimers();
    const failure = new Error("ordinary quote failure");
    await expect(
      withPricingDeadline(() => {
        if (mode === "synchronous throw") throw failure;
        return Promise.reject(failure);
      }),
    ).rejects.toBe(failure);
    expect(vi.getTimerCount()).toBe(0);
  });

  it.each([
    { status: 200, expectedValue: 0.125, expectedError: undefined, expectedMessage: undefined },
    {
      status: 503,
      expectedValue: undefined,
      expectedError: expect.any(Error),
      expectedMessage: expect.stringContaining("503"),
    },
  ])(
    "preserves observed pricing through the shared AWS client: HTTP $status",
    async ({ status, expectedValue, expectedError, expectedMessage }) => {
      const log = vi.spyOn(console, "info").mockImplementation(() => {});
      const fetchImpl = vi.fn<typeof fetch>(
        async () => new Response(status === 200 ? spotXML : "unavailable", { status }),
      );
      vi.stubGlobal("fetch", fetchImpl);
      const diagnostics = createAWSProvisioningDiagnostics("cbx_000000000001", region);
      const result = diagnostics.measure("key_pair", () => quote("aws"));
      const outcome = await result.then(
        (value) => ({ value, error: undefined }),
        (error: Error) => ({ value: undefined, error }),
      );
      expect(outcome.value).toBe(expectedValue);
      expect(outcome.error).toEqual(expectedError);
      expect(outcome.error?.message).toEqual(expectedMessage);
      diagnostics.finish(status === 200 ? "success" : "failure");
      const report = JSON.parse(String(log.mock.calls.at(-1)![0]));
      expect(
        report.steps.find((step: { name: string }) => step.name === "key_pair").transport,
      ).toMatchObject({
        requests: 1,
        credentialFailures: 0,
        signInvocations: 1,
        signCompletions: 1,
        signFailures: 0,
      });
      expect(fetchImpl).toHaveBeenCalledTimes(1);
    },
  );

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

  it.each(["resolve", "reject"])(
    "settles the complete quote deadline before Node credentials %s",
    async (completion) => {
      vi.useFakeTimers();
      const started = Promise.withResolvers<void>();
      const credentials = Promise.withResolvers<{ accessKeyId: string; secretAccessKey: string }>();
      const fetchImpl = vi.fn<typeof fetch>();
      vi.stubGlobal("fetch", fetchImpl);
      const unhandled = vi.fn<(reason: unknown, promise: Promise<unknown>) => void>();
      process.on("unhandledRejection", unhandled);
      const client = new EC2SpotClient(
        {
          awsCredentialProvider: async () => {
            started.resolve();
            return await credentials.promise;
          },
        } as Env,
        region,
      );
      let settled = false;
      const price = client
        .hourlySpotPriceUSD("t3.small")
        .catch((error) => error)
        .finally(() => {
          settled = true;
        });
      try {
        await started.promise;
        await vi.advanceTimersByTimeAsync(4_999);
        expect(settled).toBe(false);
        await vi.advanceTimersByTimeAsync(1);
        expect(settled).toBe(true);
        await expect(price).resolves.toMatchObject({ name: "TimeoutError" });
      } finally {
        try {
          if (completion === "resolve")
            credentials.resolve({ accessKeyId: "test", secretAccessKey: "test-secret" });
          else credentials.reject(new Error("late credential failure"));
          await price;
          await vi.advanceTimersByTimeAsync(0);
          expect(unhandled).not.toHaveBeenCalled();
        } finally {
          process.removeListener("unhandledRejection", unhandled);
        }
      }
      expect(fetchImpl).not.toHaveBeenCalled();
      expect(vi.getTimerCount()).toBe(0);
    },
  );

  it.each([
    { mode: "success", expected: 0.125, settledBeforeAwait: false },
    {
      mode: "timeout",
      expected: expect.objectContaining({ name: "TimeoutError" }),
      settledBeforeAwait: true,
    },
  ])(
    "preserves pricing qualification same-operation retry: $mode",
    async ({ mode, expected, settledBeforeAwait }) => {
      vi.useFakeTimers();
      const started = Promise.withResolvers<void>();
      const retry = Promise.withResolvers<AWSQualificationResponse>();
      const execute = vi
        .fn<(request: AWSQualificationRequest) => Promise<AWSQualificationResponse>>()
        .mockRejectedValueOnce(new Error("ordinary lost reply"))
        .mockImplementation(async () => {
          started.resolve();
          return await retry.promise;
        });
      const fetchImpl = vi.fn<typeof fetch>();
      vi.stubGlobal("fetch", fetchImpl);
      const client = new EC2SpotClient(
        { CRABBOX_AWS_QUALIFICATION_TRANSPORT: { execute } } as Env,
        region,
      );
      let settled = false;
      const price = client
        .hourlySpotPriceUSD("t3.small")
        .catch((error) => error)
        .finally(() => {
          settled = true;
        });
      try {
        await started.promise;
        expect(execute).toHaveBeenCalledTimes(2);
        expect(execute.mock.calls[1]![0]).toBe(execute.mock.calls[0]![0]);
        expect(execute.mock.calls[0]![0].action).toBe("DescribeSpotPriceHistory");
        if (mode === "success") {
          retry.resolve({ status: 200, body: spotXML });
        } else {
          await vi.advanceTimersByTimeAsync(5_000);
        }
        expect(settled).toBe(settledBeforeAwait);
        await expect(price).resolves.toEqual(expected);
        if (mode === "timeout") retry.reject(new Error("late retry failure"));
      } finally {
        retry.resolve({ status: 200, body: spotXML });
        await price;
        await vi.advanceTimersByTimeAsync(0);
      }
      expect(execute).toHaveBeenCalledTimes(2);
      expect(fetchImpl).not.toHaveBeenCalled();
      expect(vi.getTimerCount()).toBe(0);
    },
  );

  it.each(["pending quote", "late rejection", "late STS success"] as const)(
    "settles qualification pricing before its binding resolves: %s",
    async (mode) => {
      vi.useFakeTimers();
      const started = Promise.withResolvers<void>();
      const response = Promise.withResolvers<AWSQualificationResponse>();
      const execute = vi.fn<
        (request: AWSQualificationRequest) => Promise<AWSQualificationResponse>
      >(async (_request) => {
        started.resolve();
        return await response.promise;
      });
      const fetchImpl = vi.fn<typeof fetch>();
      vi.stubGlobal("fetch", fetchImpl);
      const client = new EC2SpotClient(
        {
          CRABBOX_AWS_QUALIFICATION_TRANSPORT: { execute },
          ...(mode === "late STS success"
            ? {
                CRABBOX_AWS_EXPECTED_ACCOUNT_ID: "123456789012",
                CRABBOX_AWS_EXPECTED_REGION: region,
              }
            : {}),
        } as Env,
        region,
      );
      let settled = false;
      const price = client
        .hourlySpotPriceUSD("t3.small")
        .catch((error) => error)
        .finally(() => {
          settled = true;
        });
      try {
        await started.promise;
        expect(execute.mock.calls[0]![0].action).toBe(
          mode === "late STS success" ? "GetCallerIdentity" : "DescribeSpotPriceHistory",
        );
        await vi.advanceTimersByTimeAsync(4_999);
        expect(settled).toBe(false);
        await vi.advanceTimersByTimeAsync(1);
        // Settlement must precede releasing authority-owned work, not depend on it.
        expect.soft(settled).toBe(true);
      } finally {
        if (mode === "late rejection") {
          response.reject(new Error("late binding failure"));
        } else {
          response.resolve({
            status: 200,
            body:
              mode === "late STS success"
                ? "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>test</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>"
                : spotXML,
          });
        }
        await price;
        await vi.advanceTimersByTimeAsync(0);
      }
      expect.soft(execute).toHaveBeenCalledTimes(1);
      await expect(price).resolves.toMatchObject({ name: "TimeoutError" });
      expect(fetchImpl).not.toHaveBeenCalled();
      expect(vi.getTimerCount()).toBe(0);
    },
  );

  it("preserves ordinary qualification same-operation retry", async () => {
    const execute = vi
      .fn<(request: AWSQualificationRequest) => Promise<AWSQualificationResponse>>()
      .mockRejectedValueOnce(new Error("lost binding response"))
      .mockResolvedValue({
        status: 200,
        body: "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>test</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>",
      });
    const client = new EC2SpotClient(
      { CRABBOX_AWS_QUALIFICATION_TRANSPORT: { execute } } as Env,
      region,
    );
    await expect(client.identity()).resolves.toMatchObject({ account: "123456789012" });
    expect(execute).toHaveBeenCalledTimes(2);
    expect(execute.mock.calls[1]![0]).toEqual(execute.mock.calls[0]![0]);
  });

  it.each([
    { provider: "aws", phase: "headers" },
    { provider: "aws", phase: "body" },
    { provider: "hetzner", phase: "headers" },
    { provider: "hetzner", phase: "body" },
  ] as const)(
    "closes real $provider HTTP stalled during $phase",
    async ({ provider, phase }) => {
      const nativeFetch = globalThis.fetch;
      const disconnected = Promise.withResolvers<void>();
      let closed = 0;
      const server = createServer((_request, response) => {
        response.on("close", () => {
          if (++closed === 1) disconnected.resolve();
        });
        if (phase === "body") {
          response.writeHead(200);
          response.write(
            provider === "aws" ? "<DescribeSpotPriceHistoryResponse>" : '{"server_types":[',
          );
        }
      });
      await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
      const address = server.address();
      if (!address || typeof address === "string") throw new Error("missing local HTTP address");
      vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
        const signal =
          init?.signal !== undefined
            ? init.signal
            : input instanceof Request
              ? input.signal
              : undefined;
        const request = input instanceof Request ? input : new Request(input, init);
        // Preserve the caller's signal when redirecting the request to loopback.
        return nativeFetch(new Request(`http://127.0.0.1:${address.port}/`, request), { signal });
      });
      try {
        await expect(quote(provider)).rejects.toMatchObject({ name: "TimeoutError" });
        let closeTimer: ReturnType<typeof setTimeout> | undefined;
        try {
          await Promise.race([
            disconnected.promise,
            new Promise<never>((_resolve, reject) => {
              closeTimer = setTimeout(
                () => reject(new Error("loopback connection remained open")),
                4_000,
              );
            }),
          ]);
        } finally {
          if (closeTimer !== undefined) clearTimeout(closeTimer);
        }
        expect(closed).toBe(1);
      } finally {
        server.closeAllConnections();
        await new Promise<void>((resolve) => server.close(() => resolve()));
      }
    },
    15_000,
  );
});
