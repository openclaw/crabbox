import { afterEach, expect, it, vi } from "vitest";

import { AzureClient, AzureProvisioningRejectedError } from "../src/azure";
import { AzureSKUAvailability } from "../src/azure-skus";
import { leaseConfig } from "../src/config";
import { appendCreationSteps, withCreationSteps } from "../src/creation-events";
import type { LeaseRecord } from "../src/types";
import type { Env } from "../src/types";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const env = {
  AZURE_TENANT_ID: "tenant",
  AZURE_CLIENT_ID: "client",
  AZURE_CLIENT_SECRET: "synthetic",
  AZURE_SUBSCRIPTION_ID: "subscription",
} as Env;
const first = "Standard_D2ads_v6";
const second = "Standard_D2ds_v6";
const config = () =>
  leaseConfig({
    provider: "azure",
    class: "tiny",
    capacity: { market: "on-demand" },
    sshPublicKey: "ssh-rsa synthetic",
  });
const sku = (name: string, restrictions: unknown[] = []) => ({
  name,
  resourceType: "virtualMachines",
  locations: ["eastus"],
  restrictions,
});

function fixture(catalogue: unknown[] = [], networkLRO = false) {
  vi.useFakeTimers();
  vi.setSystemTime(0);
  const client = new AzureClient(env);
  const events: { method: string; path: string; at: number }[] = [];
  const polls = vi.fn<() => Response>(() => Response.json({ status: "Succeeded" }));
  const vmSizes: string[] = [];
  const cleanup = vi.spyOn(AzureClient.prototype, "deleteOwnedServer").mockResolvedValue();
  const infra = vi.spyOn(AzureClient.prototype, "ensureSharedInfra").mockResolvedValue({
    vnet: "crabbox-vnet",
    nsg: "crabbox-nsg",
  });
  client.fetcher = vi.fn<typeof fetch>(async (input, init) => {
    const url = new URL(String(input));
    const method = init?.method ?? "GET";
    events.push({ method, path: url.pathname, at: Date.now() });
    if (url.hostname === "login.microsoftonline.com")
      return Response.json({ access_token: "synthetic", expires_in: 3600 });
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (url.pathname.endsWith("/skus")) return Response.json({ value: catalogue });
    if (url.pathname === "/operation") return polls();
    if (networkLRO && method === "PUT" && url.pathname.includes("/Microsoft.Network/"))
      return new Response(null, {
        status: 202,
        headers: { "azure-asyncoperation": "https://management.azure.com/operation" },
      });
    if (method === "PUT" && url.pathname.includes("/virtualMachines/")) {
      vmSizes.push(body.properties.hardwareProfile.vmSize);
      return new Response(null, {
        status: 202,
        headers: { "azure-asyncoperation": "https://management.azure.com/operation" },
      });
    }
    if (method === "GET" && url.pathname.includes("/virtualMachines/"))
      return Response.json({
        name: url.pathname.split("/").pop(),
        properties: { provisioningState: "Succeeded" },
      });
    if (method === "GET" && url.pathname.includes("/publicIPAddresses/"))
      return Response.json({ properties: { ipAddress: "192.0.2.1" } });
    return Response.json({});
  });
  const run = (input = config()) =>
    client.createServerWithFallback(input, "cbx_abcdef123456", "test", "alice@example.com");
  return { client, events, polls, vmSizes, cleanup, infra, run };
}

it("completes three sequential create LROs in 15 seconds while retaining ARM dependencies", async () => {
  const f = fixture([], true);
  const result = f.run();
  await vi.runAllTimersAsync();
  await result;
  expect(Date.now()).toBe(15_000);
  expect(f.events.filter((e) => e.method === "PUT").map((e) => e.at)).toEqual([0, 5_000, 10_000]);
  expect(f.polls).toHaveBeenCalledTimes(3);
});

it("selects a known available SKU before allocating network fragments", async () => {
  const f = fixture([sku(second)]);
  const result = f.run();
  await vi.advanceTimersByTimeAsync(5_000);
  expect((await result).serverType).toBe(second);
  expect(f.vmSizes).toEqual([second]);
  expect(f.events.findIndex((e) => e.path.endsWith("/skus"))).toBeLessThan(
    f.events.findIndex((e) => e.method === "PUT"),
  );
});

it("skips a location restriction without fragments or allocation-rejection facts", async () => {
  const f = fixture([
    sku(first, [{ type: "Location", restrictionInfo: { locations: ["eastus"] } }]),
  ]);
  const result = await f
    .run({ ...config(), serverTypeExplicit: true, serverType: first })
    .catch((error: unknown) => error);
  expect(result).toBeInstanceOf(Error);
  expect(result).not.toBeInstanceOf(AzureProvisioningRejectedError);
  expect(result).toMatchObject({ attempts: [{ category: "policy", serverType: first }] });
  expect(f.infra).not.toHaveBeenCalled();
  expect(f.events.filter((e) => e.method === "PUT")).toEqual([]);
  expect(f.cleanup).not.toHaveBeenCalled();
});

it.each([
  "SkuNotAvailable",
  "AllocationFailed",
  "ZonalAllocationFailed",
  "OverconstrainedAllocationRequest",
])("falls back after terminal %s at the first create poll, preserving cleanup", async (code) => {
  const f = fixture();
  f.polls.mockImplementationOnce(() => Response.json({ status: "Failed", error: { code } }));
  const result = f.run();
  await vi.advanceTimersByTimeAsync(5_000);
  expect(f.vmSizes).toEqual([first, second]);
  expect(f.cleanup).toHaveBeenCalledExactlyOnceWith(
    expect.objectContaining({ id: "cbx_abcdef123456", provider: "azure" }),
  );
  await vi.advanceTimersByTimeAsync(5_000);
  expect((await result).attempts).toMatchObject([{ category: "capacity", serverType: first }]);
});

it("honors updated Retry-After on every pending poll, including HTTP dates", async () => {
  const f = fixture();
  f.polls
    .mockImplementationOnce(() =>
      Response.json({ status: "InProgress" }, { headers: { "retry-after": "20" } }),
    )
    .mockImplementationOnce(() =>
      Response.json(
        { status: "InProgress" },
        { headers: { "retry-after": new Date(55_000).toUTCString() } },
      ),
    );
  const result = f.run();
  await vi.advanceTimersByTimeAsync(54_999);
  expect(f.polls).toHaveBeenCalledTimes(2);
  await vi.advanceTimersByTimeAsync(1);
  await result;
  expect(f.events.filter((e) => e.path === "/operation").map((e) => e.at)).toEqual([
    5_000, 25_000, 55_000,
  ]);
});

it("reads all SKU pages, ignores zone-only restrictions, and expires its regional cache", async () => {
  vi.useFakeTimers();
  const cache = new AzureSKUAvailability();
  const page =
    "https://management.azure.com/subscriptions/sub/providers/Microsoft.Compute/skus?next=2";
  const fetcher = vi.fn<typeof fetch>(async (input) =>
    String(input) === page
      ? Response.json({ value: [sku(second, [{ type: "Zone", values: ["eastus"] }])] })
      : Response.json({
          value: [sku(first, [{ type: "Location", values: ["eastus"] }])],
          nextLink: page,
        }),
  );
  const read = (region = "eastus") => cache.get("sub", region, async () => "token", fetcher);
  expect(await read()).toEqual(
    new Map([
      [first.toLowerCase(), false],
      [second.toLowerCase(), true],
    ]),
  );
  await read();
  expect(fetcher).toHaveBeenCalledTimes(2);
  await read("westus");
  expect(fetcher).toHaveBeenCalledTimes(4);
  await vi.advanceTimersByTimeAsync(300_000);
  await read();
  expect(fetcher).toHaveBeenCalledTimes(6);
});

it("treats partial, cross-origin and unauthorized SKU responses as unknown", async () => {
  const cache = new AzureSKUAvailability();
  const fetcher = vi
    .fn<typeof fetch>()
    .mockResolvedValueOnce(
      Response.json({ value: [sku(first)], nextLink: "https://example.com/skus" }),
    )
    .mockResolvedValueOnce(new Response(null, { status: 403 }))
    .mockResolvedValueOnce(Response.json({ value: [sku(first)] }));
  const read = () => cache.get("sub", "eastus", async () => "token", fetcher);
  expect((await read()).size).toBe(0);
  expect((await read()).size).toBe(0);
  expect((await read()).get(first.toLowerCase())).toBe(true);
  expect(fetcher).toHaveBeenCalledTimes(3);
});

it("bounds SKU preflight including authentication to five seconds", async () => {
  vi.useFakeTimers();
  const fetcher = vi.fn<typeof fetch>();
  const result = new AzureSKUAvailability().get(
    "sub",
    "eastus",
    () => new Promise(() => {}),
    fetcher,
  );
  await vi.advanceTimersByTimeAsync(5_000);
  expect((await result).size).toBe(0);
  expect(fetcher).not.toHaveBeenCalled();
});

it("bounds a long VM poll by the total create budget without treating timeout as rejection", async () => {
  const f = fixture();
  f.polls.mockImplementation(() =>
    Response.json({ status: "InProgress" }, { headers: { "retry-after": "2000" } }),
  );
  const result = f
    .run({
      ...config(),
      capacityMarket: "spot",
      capacityFallback: "on-demand-after-30m",
    })
    .catch((error: unknown) => error);
  await vi.advanceTimersByTimeAsync(25 * 60_000);
  const failure = await result;
  expect(String(failure)).toContain("deadline exceeded after 25m");
  expect(failure).not.toBeInstanceOf(AzureProvisioningRejectedError);
  expect(f.vmSizes).toEqual([first]);
  expect(f.cleanup).toHaveBeenCalledTimes(1);
  expect(Date.now()).toBe(25 * 60_000);
});

it("exposes the existing Azure attempt duration without changing LRO order", async () => {
  const f = fixture([], true);
  await withCreationSteps(async () => {
    const result = f.run();
    await vi.runAllTimersAsync();
    await result;
    const lease: Pick<LeaseRecord, "creationEvents"> = {};
    appendCreationSteps(lease);
    expect(lease.creationEvents).toContainEqual(
      expect.objectContaining({
        step: "azure.attempt",
        durationMs: 15_000,
        count: 1,
        errors: 0,
        source: "coordinator",
      }),
    );
    expect(f.events.filter((e) => e.method === "PUT").map((e) => e.at)).toEqual([0, 5000, 10000]);
  });
});
