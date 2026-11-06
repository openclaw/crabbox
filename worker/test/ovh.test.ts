import { createHash } from "node:crypto";

import { afterEach, describe, expect, it, vi } from "vitest";

import { cloudInit } from "../src/bootstrap";
import { leaseConfig } from "../src/config";
import {
  OVHAllocationRejectedError,
  OVHClient,
  ovhCatalogHourlyUSD,
  ovhEndpoint,
  ovhInstanceOwnedByLease,
  ovhProvisioningFailureEvidence,
  ovhSubsidiaryForEndpoint,
} from "../src/ovh";
import {
  type ProviderProvisioningCleanupClaim,
  ProviderProvisioningOutcomeUncertainError,
  ProviderResourceUnresolvedError,
  validateProviderProvisioningCleanupClaim,
} from "../src/provider-provisioning";
import { leaseProviderName } from "../src/slug";
import type { Env, LeaseRecord } from "../src/types";

const endpoint = "https://api.us.ovhcloud.com/1.0";
const projectID = "0123456789abcdef0123456789abcdef";
const instanceID = "11111111-1111-4111-8111-111111111111";
const otherInstanceID = "22222222-2222-4222-8222-222222222222";
const leaseID = "cbx_abcdef123456";
const slug = "blue-lobster";
const keyName = "crabbox-cbx-abcdef123456";
const derivedName = leaseProviderName(leaseID, slug);
const instanceName = `${derivedName}-0123456789ab`;
const publicKey = "ssh-ed25519 AAAAsynthetic user@example.com";

const baseEnv = {
  FLEET: {} as DurableObjectNamespace,
  HETZNER_TOKEN: "",
  OVH_APPLICATION_KEY: "synthetic-app-key",
  OVH_APPLICATION_SECRET: "synthetic-app-secret",
  OVH_CONSUMER_KEY: "synthetic-consumer-key",
  CRABBOX_OVH_PROJECT_ID: projectID,
  CRABBOX_OVH_REGION: "BHS5",
} satisfies Env;

const lease = {
  id: leaseID,
  cloudID: instanceID,
  region: "BHS5",
  providerScope: `ovh:project:${projectID}`,
  providerResourceID: instanceName,
};

const flavor = { id: "flavor-id", name: "b3-8", osType: "linux", available: true };
const image = { id: "image-id", name: "Ubuntu 24.04", type: "linux", status: "active" };

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

interface FakeOVH {
  client: OVHClient;
  requests: Array<{
    method: string;
    path: string;
    search: string;
    url: string;
    headers: Headers;
    body: string;
  }>;
  operations: () => string[];
}

type Route = (body: string) => Response | Promise<Response>;

// Every test states the provider behavior it depends on; unrouted calls fail loudly.
function fakeOVH(routes: Record<string, Route>, env: Partial<Env> = {}): FakeOVH {
  const client = new OVHClient({ ...baseEnv, ...env });
  client.pollDelayMs = 1;
  client.maxWaitMs = 30;
  client.deleteWaitMs = 30;
  const requests: FakeOVH["requests"] = [];
  client.fetcher = vi.fn<typeof fetch>(async (input, init) => {
    const request = new Request(input, init);
    const url = new URL(request.url);
    const path = url.pathname.replace(/^\/1\.0/, "");
    const body = await request.text();
    requests.push({
      method: request.method,
      path,
      search: url.search,
      url: request.url,
      headers: request.headers,
      body,
    });
    if (path === "/auth/time") return json(1_800_000_000);
    const route = routes[`${request.method} ${path}`];
    if (!route) throw new Error(`unexpected OVH request: ${request.method} ${path}`);
    return await route(body);
  });
  return {
    client,
    requests,
    operations: () =>
      requests
        .filter((request) => request.path !== "/auth/time")
        .map(
          (request) =>
            `${request.method} ${request.path.replace(`/cloud/project/${projectID}`, "")}`,
        ),
  };
}

const p = (path: string) => `/cloud/project/${projectID}${path}`;
const catalogRoutes = {
  [`GET ${p("/flavor")}`]: () => json([flavor]),
  [`GET ${p("/image")}`]: () => json([image]),
};

function config(overrides: Record<string, unknown> = {}) {
  return {
    ...leaseConfig({ provider: "ovh", sshPublicKey: publicKey, serverType: "b3-8" }),
    ovhRegion: "BHS5",
    ovhInstanceName: instanceName,
    ...overrides,
  };
}

const activeInstance = {
  id: instanceID,
  name: instanceName,
  region: "BHS5",
  status: "ACTIVE",
  flavor: { name: "b3-8" },
  ipAddresses: [
    { ip: "10.0.0.5", version: 4, type: "private" },
    { ip: "2001:db8::1", version: 6, type: "public" },
    { ip: "203.0.113.20", version: 4, type: "public" },
  ],
};

afterEach(() => {
  vi.restoreAllMocks();
});

describe("OVH lease configuration", () => {
  const client = () => new OVHClient(baseEnv);
  const request = (input: Record<string, unknown> = {}) =>
    leaseConfig({ provider: "ovh", sshPublicKey: publicKey, ...input });

  it("defaults to the image the provider boots", () => {
    expect(request().os).toBe("ubuntu:24.04");
    expect(client().unsupportedOS(request())).toBeUndefined();
  });

  it("names an explicit OS the default image cannot satisfy", () => {
    expect(client().unsupportedOS(request({ os: "ubuntu:26.04" }))).toContain("ubuntu:26.04");
  });

  it("trusts the operator's image for an explicit OS", () => {
    const configured = new OVHClient({ ...baseEnv, CRABBOX_OVH_IMAGE: "Ubuntu 26.04" });
    expect(configured.unsupportedOS(request({ os: "ubuntu:26.04" }))).toBeUndefined();
  });

  it("applies coordinator defaults without overriding an explicit flavor", () => {
    const configured = new OVHClient({ ...baseEnv, CRABBOX_OVH_FLAVOR: "b3-16" });
    expect(configured.prepareLeaseConfig(request())).toMatchObject({
      ovhRegion: "BHS5",
      serverType: "b3-16",
    });
    expect(
      configured.prepareLeaseConfig(
        request({ serverType: "c3-4", serverTypeExplicit: true, ovhRegion: "GRA11" }),
      ),
    ).toMatchObject({ ovhRegion: "GRA11", serverType: "c3-4" });
  });
});

describe("OVH cleanup claims", () => {
  const claim = {
    provider: "ovh" as const,
    cloudID: instanceID,
    region: "BHS5",
    providerScope: `ovh:project:${projectID}`,
    providerResourceID: instanceName,
  };

  it("accepts a complete allocation identity", () => {
    expect(validateProviderProvisioningCleanupClaim(claim, "ovh")).toEqual(claim);
  });

  it.each([
    ["no recorded name", { ...claim, providerResourceID: undefined }],
    ["no region", { ...claim, region: undefined }],
    ["a non-UUID instance id", { ...claim, cloudID: "not-a-uuid" }],
    ["a scope from another provider", { ...claim, providerScope: "daytona:context:v1:abc" }],
  ])("refuses a claim with %s", (_name, value) => {
    expect(
      validateProviderProvisioningCleanupClaim(
        value as unknown as ProviderProvisioningCleanupClaim,
        "ovh",
      ),
    ).toBeUndefined();
  });
});

describe("OVH allocation identity", () => {
  it("records a unique name before dispatch, without changing the derived prefix", () => {
    const client = new OVHClient(baseEnv);
    const record = { ...lease, slug, cloudID: "" } as unknown as LeaseRecord;
    const first = client.prepareLeaseCreate(config({ ovhInstanceName: undefined }), record);
    const second = client.prepareLeaseCreate(config({ ovhInstanceName: undefined }), record);
    expect(first.lease.providerResourceID).toMatch(new RegExp(`^${derivedName}-[a-f0-9]{12}$`));
    expect(first.config.ovhInstanceName).toBe(first.lease.providerResourceID);
    expect(first.lease.providerScope).toBe(`ovh:project:${projectID}`);
    expect(first.lease.region).toBe("BHS5");
    expect(second.lease.providerResourceID).not.toBe(first.lease.providerResourceID);
  });
});

describe("OVH client configuration", () => {
  it("requires credentials and the project", () => {
    expect(() => new OVHClient({ ...baseEnv, OVH_APPLICATION_KEY: "" })).toThrow(
      "OVH_APPLICATION_KEY",
    );
    expect(() => new OVHClient({ ...baseEnv, OVH_APPLICATION_SECRET: "" })).toThrow(
      "OVH_APPLICATION_SECRET",
    );
    expect(() => new OVHClient({ ...baseEnv, OVH_CONSUMER_KEY: "" })).toThrow("OVH_CONSUMER_KEY");
    expect(() => new OVHClient({ ...baseEnv, CRABBOX_OVH_PROJECT_ID: "" })).toThrow(
      "CRABBOX_OVH_PROJECT_ID",
    );
  });

  it.each([
    ["", "https://api.us.ovhcloud.com/1.0"],
    ["ovh-us", "https://api.us.ovhcloud.com/1.0"],
    ["ovh-ca", "https://ca.api.ovh.com/1.0"],
    ["ovh-eu", "https://eu.api.ovh.com/1.0"],
    ["https://eu.api.ovh.com/1.0/", "https://eu.api.ovh.com/1.0"],
  ])("normalizes endpoint %j", (value, expected) => {
    expect(ovhEndpoint(value)).toBe(expected);
  });

  it.each([
    "http://eu.api.ovh.com/1.0",
    "https://ovh.example.com/1.0",
    "https://eu.api.ovh.com.example.com/1.0",
    "https://user:pass@eu.api.ovh.com/1.0",
    "https://eu.api.ovh.com/1.0?token=value",
  ])("refuses endpoint %s", (value) => {
    expect(() => ovhEndpoint(value)).toThrow("OVH_ENDPOINT");
  });

  it.each([
    ["https://api.us.ovhcloud.com/1.0", "US"],
    ["https://ca.api.ovh.com/1.0", "CA"],
    ["https://eu.api.ovh.com/1.0", "FR"],
  ])("derives the catalog subsidiary for %s", (value, subsidiary) => {
    expect(ovhSubsidiaryForEndpoint(value)).toBe(subsidiary);
  });
});

describe("OVH request signing", () => {
  it("matches the signature contract shared with the direct client", async () => {
    const { client, requests } = fakeOVH({
      [`POST ${p("/sshkey")}`]: () => json({ id: "key-id", name: keyName, publicKey }),
    });
    await client.createSSHKey(keyName, publicKey);
    const signed = requests.find((request) => request.method === "POST")!;
    const timestamp = signed.headers.get("x-ovh-timestamp")!;
    const expected = createHash("sha1")
      .update(
        [
          baseEnv.OVH_APPLICATION_SECRET,
          baseEnv.OVH_CONSUMER_KEY,
          "POST",
          `${endpoint}${p("/sshkey")}`,
          signed.body,
          timestamp,
        ].join("+"),
      )
      .digest("hex");
    expect(signed.headers.get("x-ovh-signature")).toBe(`$1$${expected}`);
    expect(signed.headers.get("x-ovh-application")).toBe(baseEnv.OVH_APPLICATION_KEY);
    expect(signed.headers.get("x-ovh-consumer")).toBe(baseEnv.OVH_CONSUMER_KEY);
  });

  it("signs the full URL, query included, to a fixed vector", async () => {
    vi.spyOn(Date, "now").mockReturnValue(1_800_000_000_000);
    const { client, requests } = fakeOVH({
      [`GET ${p("/flavor")}`]: () => json([]),
      [`GET ${p("/image")}`]: () => json([image]),
    });
    await client.createServer(config(), leaseID).catch(() => undefined);
    const signed = requests.find((request) => request.path === p("/flavor"))!;
    expect(signed.search).toBe("?region=BHS5");
    expect(signed.headers.get("x-ovh-signature")).toBe(
      "$1$c1ad1882fae6111f52bf3443e0c935d64921ca21",
    );
  });

  it("signs with server time and fetches it once", async () => {
    vi.spyOn(Date, "now").mockReturnValue(1_700_000_000_000);
    const { client, requests } = fakeOVH({
      [`GET ${p("/region")}`]: () => json(["BHS5"]),
    });
    await client.listRegions();
    await client.listRegions();
    expect(requests.filter((request) => request.path === "/auth/time")).toHaveLength(1);
    expect(requests.at(-1)?.headers.get("x-ovh-timestamp")).toBe("1800000000");
  });

  it("never sends the time probe signed", async () => {
    const { client, requests } = fakeOVH({ [`GET ${p("/region")}`]: () => json(["BHS5"]) });
    await client.listRegions();
    const probe = requests.find((request) => request.path === "/auth/time")!;
    expect(probe.headers.get("x-ovh-signature")).toBeNull();
    expect(probe.headers.get("x-ovh-consumer")).toBeNull();
  });

  it("does not follow redirects, so signed headers stay on the trusted origin", async () => {
    const { client } = fakeOVH({
      [`GET ${p("/region")}`]: () =>
        new Response(null, { status: 302, headers: { location: "https://elsewhere.example/" } }),
    });
    await expect(client.listRegions()).rejects.toThrow("http 302");
    const init = vi.mocked(client.fetcher).mock.calls.at(-1)?.[1];
    expect(init?.redirect).toBe("manual");
  });

  it("redacts credentials and signatures from provider errors", async () => {
    const { client } = fakeOVH({
      [`GET ${p("/region")}`]: () =>
        new Response(
          `denied ${baseEnv.OVH_APPLICATION_SECRET} ${baseEnv.OVH_CONSUMER_KEY} $1$${"a".repeat(40)}`,
          { status: 403 },
        ),
    });
    const error = await client.listRegions().catch((caught: unknown) => caught as Error);
    expect(String(error)).toContain("http 403");
    expect(String(error)).not.toContain(baseEnv.OVH_APPLICATION_SECRET);
    expect(String(error)).not.toContain(baseEnv.OVH_CONSUMER_KEY);
    expect(String(error)).not.toContain("a".repeat(40));
  });
});

describe("OVH catalog pricing", () => {
  const catalog = (currencyCode: string, price: number, type = "consumption") => ({
    locale: { currencyCode },
    addons: [
      { planCode: "other.consumption", pricings: [{ price: 999_000_000, type: "consumption" }] },
      {
        planCode: "b3-8.consumption",
        pricings: [
          { price: 7_000_000_000, type: "renew" },
          { price, type },
        ],
      },
    ],
  });

  it("reads the hourly consumption price in USD", () => {
    expect(ovhCatalogHourlyUSD(catalog("USD", 5_250_000), "b3-8.consumption", 1.08)).toBe(0.0525);
  });

  it("converts EUR with the configured rate", () => {
    expect(ovhCatalogHourlyUSD(catalog("EUR", 5_000_000), "b3-8.consumption", 1.2)).toBe(0.06);
  });

  it.each([
    ["an unknown currency", catalog("CAD", 5_000_000), "b3-8.consumption"],
    ["a missing plan", catalog("USD", 5_000_000), "absent.consumption"],
    ["a non-consumption pricing", catalog("USD", 5_000_000, "upfront"), "b3-8.consumption"],
    ["a zero price", catalog("USD", 0), "b3-8.consumption"],
    ["a malformed catalog", { addons: "nope" }, "b3-8.consumption"],
    ["a null catalog", null, "b3-8.consumption"],
  ])("returns no quote for %s", (_name, value, planCode) => {
    expect(ovhCatalogHourlyUSD(value, planCode, 1.08)).toBeUndefined();
  });

  it("prefers the flavor's plan code and reads the public catalog unsigned", async () => {
    const { client, requests } = fakeOVH({
      [`GET ${p("/flavor")}`]: () =>
        json([{ ...flavor, planCodes: { hourly: "b3-8.special.consumption" } }]),
      "GET /order/catalog/public/cloud": () =>
        json({
          locale: { currencyCode: "USD" },
          addons: [
            {
              planCode: "b3-8.special.consumption",
              pricings: [{ price: 12_340_000, type: "consumption" }],
            },
          ],
        }),
    });
    await expect(client.hourlyPriceUSD("b3-8", "BHS5")).resolves.toBe(0.1234);
    const quote = requests.find((request) => request.path === "/order/catalog/public/cloud")!;
    expect(quote.headers.get("x-ovh-signature")).toBeNull();
    expect(quote.headers.get("x-ovh-consumer")).toBeNull();
    const url = vi
      .mocked(client.fetcher)
      .mock.calls.map(([input]) => String(input))
      .find((value) => value.includes("/order/catalog/"));
    expect(url).toContain("ovhSubsidiary=US");
  });

  it("gives no quote for a catalog above the memory bound", async () => {
    const oversized = new ReadableStream<Uint8Array>({
      pull(controller) {
        controller.enqueue(new Uint8Array(1024 * 1024));
      },
    });
    const { client } = fakeOVH({
      [`GET ${p("/flavor")}`]: () => json([flavor]),
      "GET /order/catalog/public/cloud": () => new Response(oversized),
    });
    await expect(client.hourlyPriceUSD("b3-8", "BHS5")).rejects.toThrow("exceeds");
  });

  it("refuses a catalog that declares an oversized body before reading it", async () => {
    const { client } = fakeOVH({
      [`GET ${p("/flavor")}`]: () => json([flavor]),
      "GET /order/catalog/public/cloud": () =>
        new Response("{}", { headers: { "content-length": String(64 * 1024 * 1024) } }),
    });
    await expect(client.hourlyPriceUSD("b3-8", "BHS5")).rejects.toThrow("exceeds");
  });

  it("falls back to the conventional plan code when the flavor lookup fails", async () => {
    const { client } = fakeOVH({
      [`GET ${p("/flavor")}`]: () => new Response("nope", { status: 500 }),
      "GET /order/catalog/public/cloud": () =>
        json({
          locale: { currencyCode: "USD" },
          addons: [
            { planCode: "b3-8.consumption", pricings: [{ price: 5_000_000, type: "consumption" }] },
          ],
        }),
    });
    await expect(client.hourlyPriceUSD("b3-8", "BHS5")).resolves.toBe(0.05);
  });
});

describe("OVH ownership", () => {
  it.each([
    ["the exact lease instance", activeInstance, lease, true],
    ["an instance reporting no region", { ...activeInstance, region: "" }, lease, true],
    ["another instance id", { ...activeInstance, id: otherInstanceID }, lease, false],
    ["another name", { ...activeInstance, name: "crabbox-someone-else" }, lease, false],
    ["the derived name without the token", { ...activeInstance, name: derivedName }, lease, false],
    ["another region", { ...activeInstance, region: "GRA11" }, lease, false],
    [
      "a lease with no recorded name",
      activeInstance,
      { cloudID: instanceID, region: "BHS5" },
      false,
    ],
  ])("evaluates %s", (_name, instance, candidate, expected) => {
    expect(ovhInstanceOwnedByLease(instance, candidate)).toBe(expected);
  });
});

describe("OVH provisioning", () => {
  const keyRoutes = {
    [`POST ${p("/sshkey")}`]: () => json({ id: "key-id", name: keyName, publicKey }),
    [`DELETE ${p("/sshkey/key-id")}`]: () => new Response(null, { status: 200 }),
  };

  it("creates the instance and removes the transient key once the outcome is known", async () => {
    const { client, requests, operations } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: () => json({ ...activeInstance, ipAddresses: [] }),
      [`GET ${p(`/instance/${instanceID}`)}`]: () => json(activeInstance),
    });
    const onCreated = vi.fn<(claim: ProviderProvisioningCleanupClaim) => Promise<boolean>>(
      async () => true,
    );
    const server = await client.createServer(config(), leaseID, onCreated);
    expect(server).toMatchObject({
      provider: "ovh",
      cloudID: instanceID,
      name: instanceName,
      host: "203.0.113.20",
      region: "BHS5",
      serverType: "b3-8",
    });
    expect(onCreated).toHaveBeenCalledWith(
      expect.objectContaining({
        provider: "ovh",
        cloudID: instanceID,
        region: "BHS5",
        providerScope: `ovh:project:${projectID}`,
      }),
    );
    const create = JSON.parse(
      requests.find((request) => request.path === p("/instance") && request.method === "POST")!
        .body,
    ) as Record<string, unknown>;
    expect(create).toMatchObject({
      name: instanceName,
      region: "BHS5",
      flavorId: "flavor-id",
      imageId: "image-id",
      sshKeyId: "key-id",
    });
    expect(create).not.toHaveProperty("monthlyBilling");
    expect(String(create["userData"])).toContain(publicKey);
    const order = operations();
    expect(order.indexOf("POST /instance")).toBeGreaterThan(order.indexOf("POST /sshkey"));
    expect(order.indexOf("DELETE /sshkey/key-id")).toBeGreaterThan(order.indexOf("POST /instance"));
  });

  it.each([
    ["the default lease", {}],
    [
      "the largest supported lease",
      {
        tailscale: true,
        tailscaleAuthKey: "tskey-synthetic",
        tailscaleHostname: "crabbox-blue-lobster",
        tailscaleTags: ["tag:crabbox"],
        exposedPorts: ["8080", "8443"],
      },
    ],
  ])("keeps cloud-init for %s inside the OpenStack user data limit", (_name, input) => {
    const configured = leaseConfig({ provider: "ovh", sshPublicKey: publicKey, ...input });
    expect(btoa(cloudInit(configured)).length).toBeLessThan(65_535);
  });

  it("does not treat a failed transient key delete as a failed lease", async () => {
    const { client } = fakeOVH({
      ...catalogRoutes,
      [`POST ${p("/sshkey")}`]: () => json({ id: "key-id", name: keyName, publicKey }),
      [`DELETE ${p("/sshkey/key-id")}`]: () => new Response("busy", { status: 409 }),
      [`POST ${p("/instance")}`]: () => json(activeInstance),
    });
    await expect(client.createServer(config(), leaseID)).resolves.toMatchObject({
      cloudID: instanceID,
    });
  });

  it("adopts this lease's key after a replayed create", async () => {
    const { client, operations } = fakeOVH({
      ...catalogRoutes,
      [`POST ${p("/sshkey")}`]: () => new Response("exists", { status: 409 }),
      [`GET ${p("/sshkey")}`]: () => json([{ id: "key-id", name: keyName, publicKey }]),
      [`DELETE ${p("/sshkey/key-id")}`]: () => new Response(null, { status: 200 }),
      [`POST ${p("/instance")}`]: () => json(activeInstance),
    });
    await client.createServer(config(), leaseID);
    expect(operations()).toContain("POST /instance");
  });

  it("refuses a same-named key holding a different public key", async () => {
    const { client, operations } = fakeOVH({
      ...catalogRoutes,
      [`POST ${p("/sshkey")}`]: () => new Response("exists", { status: 409 }),
      [`GET ${p("/sshkey")}`]: () =>
        json([
          { id: "key-id", name: keyName, publicKey: "ssh-ed25519 AAAAother other@example.com" },
        ]),
    });
    await expect(client.createServer(config(), leaseID)).rejects.toBeInstanceOf(
      OVHAllocationRejectedError,
    );
    expect(operations()).not.toContain("POST /instance");
    expect(operations()).not.toContain("DELETE /sshkey/key-id");
  });

  it.each([
    ["an unavailable flavor", [{ ...flavor, available: false }], [image], "unavailable"],
    ["an exhausted quota", [{ ...flavor, quota: 0 }], [image], "quota"],
    ["a windows flavor", [{ ...flavor, osType: "windows" }], [image], "not linux"],
    ["a missing flavor", [], [image], "was not returned"],
    ["ambiguous flavors", [flavor, { ...flavor, id: "second" }], [image], "ambiguous"],
    ["a missing image", [flavor], [], "was not returned"],
    ["an inactive image", [flavor], [{ ...image, status: "queued" }], "status=queued"],
    ["a windows image", [flavor], [{ ...image, type: "windows" }], "not linux"],
    [
      "a private image selected by name",
      [flavor],
      [{ ...image, visibility: "private" }],
      "non-public",
    ],
  ])("rejects %s before creating anything", async (_name, flavors, images, message) => {
    const { client, operations } = fakeOVH({
      [`GET ${p("/flavor")}`]: () => json(flavors),
      [`GET ${p("/image")}`]: () => json(images),
    });
    const failure = client.createServer(config(), leaseID);
    await expect(failure).rejects.toBeInstanceOf(OVHAllocationRejectedError);
    await expect(failure).rejects.toThrow(message);
    expect(operations().filter((operation) => !operation.startsWith("GET"))).toEqual([]);
  });

  it("rejects a configured image outside the Debian family", async () => {
    const { client, operations } = fakeOVH(
      {
        [`GET ${p("/flavor")}`]: () => json([flavor]),
        [`GET ${p("/image")}`]: () => json([{ ...image, name: "Rocky 9" }]),
      },
      { CRABBOX_OVH_IMAGE: "Rocky 9" },
    );
    await expect(client.createServer(config(), leaseID)).rejects.toThrow(
      "not a supported Debian or Ubuntu",
    );
    expect(operations()).not.toContain("POST /sshkey");
  });

  it("selects flavors by name only, so an id cannot evade cost admission", async () => {
    const { client, operations } = fakeOVH(catalogRoutes);
    await expect(
      client.createServer(config({ serverType: "flavor-id" }), leaseID),
    ).rejects.toBeInstanceOf(OVHAllocationRejectedError);
    expect(operations()).not.toContain("POST /sshkey");
  });

  it.each([
    ["an empty body", () => new Response(null, { status: 200 })],
    ["null", () => json(null)],
    ["an object without an id", () => json({ name: instanceName })],
  ])("treats a create answered with %s as a lost response", async (_name, reply) => {
    const { client } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: reply,
      [`GET ${p("/instance")}`]: () => json([activeInstance]),
    });
    await expect(client.createServer(config(), leaseID)).resolves.toMatchObject({
      cloudID: instanceID,
    });
  });

  it("keeps a malformed create uncertain when nothing is visible yet", async () => {
    const { client } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: () => json(null),
      [`GET ${p("/instance")}`]: () => json([]),
    });
    await expect(client.createServer(config(), leaseID)).rejects.toBeInstanceOf(
      ProviderProvisioningOutcomeUncertainError,
    );
  });

  it("survives a malformed address list in the create response", async () => {
    const { client } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: () => json({ ...activeInstance, ipAddresses: 5 }),
      [`GET ${p(`/instance/${instanceID}`)}`]: () => json(activeInstance),
    });
    await expect(client.createServer(config(), leaseID)).resolves.toMatchObject({
      host: "203.0.113.20",
    });
  });

  it("never recovers a same-derived-name instance from another allocation", async () => {
    const { client } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: () => new Response("oops", { status: 500 }),
      [`GET ${p("/instance")}`]: () => json([{ ...activeInstance, name: derivedName }]),
    });
    await expect(client.createServer(config(), leaseID)).rejects.toBeInstanceOf(
      ProviderProvisioningOutcomeUncertainError,
    );
  });

  it("refuses to allocate without a prepared name", async () => {
    const { client, operations } = fakeOVH(catalogRoutes);
    await expect(
      client.createServer(config({ ovhInstanceName: "" }), leaseID),
    ).rejects.toBeInstanceOf(OVHAllocationRejectedError);
    expect(operations()).toEqual([]);
  });

  it("removes the key when the create is definitely rejected", async () => {
    const { client, operations } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: () => new Response("quota", { status: 403 }),
    });
    const error = await client.createServer(config(), leaseID).catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(OVHAllocationRejectedError);
    expect((error as OVHAllocationRejectedError).providerKeyCleanupID).toBeUndefined();
    expect(operations()).toContain("DELETE /sshkey/key-id");
  });

  it("retains the key id for cleanup when rollback cannot remove it", async () => {
    const { client } = fakeOVH({
      ...catalogRoutes,
      [`POST ${p("/sshkey")}`]: () => json({ id: "key-id", name: keyName, publicKey }),
      [`DELETE ${p("/sshkey/key-id")}`]: () => new Response("down", { status: 503 }),
      [`POST ${p("/instance")}`]: () => new Response("quota", { status: 403 }),
    });
    const error = await client.createServer(config(), leaseID).catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(OVHAllocationRejectedError);
    expect(ovhProvisioningFailureEvidence(error)).toEqual({
      allocationRejected: true,
      providerKeyCleanupID: "key-id",
    });
  });

  it.each([
    ["a server error", () => new Response("oops", { status: 500 })],
    ["rate limiting", () => new Response("slow", { status: 429 })],
    [
      "a lost response",
      () => {
        throw new TypeError("connection reset");
      },
    ],
  ])("recovers a created instance by exact name after %s", async (_name, reply) => {
    const { client } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: reply,
      [`GET ${p("/instance")}`]: () =>
        json([{ ...activeInstance, id: otherInstanceID, name: "other" }, activeInstance]),
    });
    await expect(client.createServer(config(), leaseID)).resolves.toMatchObject({
      cloudID: instanceID,
    });
  });

  it("reports an unresolved create as uncertain, never as absent", async () => {
    const { client, operations } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: () => new Response("oops", { status: 502 }),
      [`GET ${p("/instance")}`]: () => json([]),
    });
    const error = await client.createServer(config(), leaseID).catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(ProviderProvisioningOutcomeUncertainError);
    expect(ovhProvisioningFailureEvidence(error)).toEqual({});
    expect(operations()).toContain("DELETE /sshkey/key-id");
  });

  it("refuses ambiguous name recovery", async () => {
    const { client } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: () => new Response("oops", { status: 500 }),
      [`GET ${p("/instance")}`]: () =>
        json([activeInstance, { ...activeInstance, id: otherInstanceID }]),
    });
    await expect(client.createServer(config(), leaseID)).rejects.toBeInstanceOf(
      ProviderResourceUnresolvedError,
    );
  });

  it("stops before readiness when the coordinator cancels the attempt", async () => {
    const { client, operations } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: () => json({ ...activeInstance, ipAddresses: [] }),
    });
    const server = await client.createServer(config(), leaseID, async () => false);
    expect(server.cloudID).toBe(instanceID);
    expect(operations()).not.toContain(`GET /instance/${instanceID}`);
  });

  it("retains cleanup authority when no public address arrives", async () => {
    const { client } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: () => json({ ...activeInstance, ipAddresses: [] }),
      [`GET ${p(`/instance/${instanceID}`)}`]: () => json({ ...activeInstance, ipAddresses: [] }),
    });
    await expect(client.createServer(config(), leaseID)).rejects.toMatchObject({
      name: "ProviderProvisioningCleanupError",
      cleanupClaim: { provider: "ovh", cloudID: instanceID, region: "BHS5" },
    });
  });

  it("fails fast when the instance enters ERROR", async () => {
    const { client } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: () => json({ ...activeInstance, ipAddresses: [] }),
      [`GET ${p(`/instance/${instanceID}`)}`]: () =>
        json({ ...activeInstance, status: "ERROR", ipAddresses: [] }),
    });
    await expect(client.createServer(config(), leaseID)).rejects.toThrow(/status=ERROR/);
  });

  it("keeps waiting through transient reads", async () => {
    let reads = 0;
    const { client } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: () => json({ ...activeInstance, ipAddresses: [] }),
      [`GET ${p(`/instance/${instanceID}`)}`]: () => {
        reads += 1;
        if (reads === 1) return new Response("not yet", { status: 404 });
        if (reads === 2) return new Response("oops", { status: 503 });
        return json(activeInstance);
      },
    });
    await expect(client.createServer(config(), leaseID)).resolves.toMatchObject({
      host: "203.0.113.20",
    });
  });

  it("refuses a create response naming another instance", async () => {
    const { client } = fakeOVH({
      ...catalogRoutes,
      ...keyRoutes,
      [`POST ${p("/instance")}`]: () => json({ ...activeInstance, name: "crabbox-other" }),
    });
    await expect(client.createServer(config(), leaseID)).rejects.toBeInstanceOf(
      ProviderResourceUnresolvedError,
    );
  });
});

describe("OVH recovery", () => {
  it("recovers by exact instance id", async () => {
    const { client } = fakeOVH({
      [`GET ${p(`/instance/${instanceID}`)}`]: () => json(activeInstance),
    });
    await expect(client.recoverServer(lease)).resolves.toMatchObject({ cloudID: instanceID });
  });

  it("reports absence for a known id", async () => {
    const { client } = fakeOVH({
      [`GET ${p(`/instance/${instanceID}`)}`]: () => new Response("gone", { status: 404 }),
      [`GET ${p("/region")}`]: () => json(["BHS5"]),
    });
    await expect(client.recoverServer(lease)).resolves.toBeUndefined();
  });

  it("does not report absence when the project itself is not visible", async () => {
    const { client } = fakeOVH({
      [`GET ${p(`/instance/${instanceID}`)}`]: () => new Response("gone", { status: 404 }),
      [`GET ${p("/region")}`]: () => new Response("no such project", { status: 404 }),
    });
    await expect(client.recoverServer(lease)).rejects.toBeInstanceOf(
      ProviderResourceUnresolvedError,
    );
  });

  it("reports absence for a lease whose create was never dispatched", async () => {
    const { client, operations } = fakeOVH({});
    await expect(
      client.recoverServer({ ...lease, cloudID: "", providerResourceID: undefined }),
    ).resolves.toBeUndefined();
    expect(operations()).toEqual([]);
  });

  it("refuses an id now held by another name", async () => {
    const { client } = fakeOVH({
      [`GET ${p(`/instance/${instanceID}`)}`]: () => json({ ...activeInstance, name: "reused" }),
    });
    await expect(client.recoverServer(lease)).rejects.toBeInstanceOf(
      ProviderResourceUnresolvedError,
    );
  });

  it("finds an unbound create by exact name and region", async () => {
    const { client } = fakeOVH({
      [`GET ${p("/instance")}`]: () =>
        json([{ ...activeInstance, id: otherInstanceID, region: "GRA11" }, activeInstance]),
    });
    await expect(client.recoverServer({ ...lease, cloudID: "" })).resolves.toMatchObject({
      cloudID: instanceID,
    });
  });

  it("refuses recovery from another project", async () => {
    const { client } = fakeOVH({});
    await expect(
      client.recoverServer({ ...lease, providerScope: "ovh:project:other" }),
    ).rejects.toBeInstanceOf(ProviderResourceUnresolvedError);
  });
});

describe("OVH release", () => {
  const noKeys = { [`GET ${p("/sshkey")}`]: () => json([]) };

  it("deletes the exact instance, confirms absence, and sweeps the lease key", async () => {
    let present = true;
    const { client, operations } = fakeOVH({
      [`GET ${p(`/instance/${instanceID}`)}`]: () =>
        present ? json(activeInstance) : new Response("gone", { status: 404 }),
      [`DELETE ${p(`/instance/${instanceID}`)}`]: () => {
        present = false;
        return new Response(null, { status: 200 });
      },
      [`GET ${p("/sshkey")}`]: () =>
        json([
          { id: "key-id", name: keyName, publicKey },
          { id: "shared", name: "team-key", publicKey },
        ]),
      [`DELETE ${p("/sshkey/key-id")}`]: () => new Response(null, { status: 200 }),
    });
    const owner = vi.fn<() => Promise<void>>(async () => {});
    await client.releaseLease(lease, owner);
    expect(operations()).toContain(`DELETE /instance/${instanceID}`);
    expect(operations()).toContain("DELETE /sshkey/key-id");
    expect(operations()).not.toContain("DELETE /sshkey/shared");
    expect(owner.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it("settles when the instance is already gone", async () => {
    const { client, operations } = fakeOVH({
      ...noKeys,
      [`GET ${p("/region")}`]: () => json(["BHS5"]),
      [`GET ${p(`/instance/${instanceID}`)}`]: () => new Response("gone", { status: 404 }),
    });
    await client.releaseLease(lease, async () => {});
    expect(operations().filter((operation) => operation.startsWith("DELETE"))).toEqual([]);
  });

  it("does not settle on a 404 from a project the credentials cannot see", async () => {
    const { client } = fakeOVH({
      [`GET ${p("/region")}`]: () => new Response("no such project", { status: 404 }),
      [`GET ${p(`/instance/${instanceID}`)}`]: () => new Response("gone", { status: 404 }),
    });
    await expect(client.releaseLease(lease, async () => {})).rejects.toBeInstanceOf(
      ProviderResourceUnresolvedError,
    );
  });

  it.each([
    ["an empty body", () => new Response(null, { status: 200 })],
    ["null", () => json(null)],
  ])("does not read %s as a deleted instance", async (_name, reply) => {
    const { client } = fakeOVH({ [`GET ${p(`/instance/${instanceID}`)}`]: reply });
    await expect(client.releaseLease(lease, async () => {})).rejects.toThrow("no instance");
  });

  it.each([
    ["another name", { ...activeInstance, name: "crabbox-someone-else" }],
    ["another region", { ...activeInstance, region: "GRA11" }],
    ["another id", { ...activeInstance, id: otherInstanceID }],
  ])("refuses deletion for %s", async (_name, instance) => {
    const { client, operations } = fakeOVH({
      [`GET ${p(`/instance/${instanceID}`)}`]: () => json(instance),
    });
    await expect(client.releaseLease(lease, async () => {})).rejects.toBeInstanceOf(
      ProviderResourceUnresolvedError,
    );
    expect(operations().filter((operation) => operation.startsWith("DELETE"))).toEqual([]);
  });

  it("refuses to act outside the lease's recorded project", async () => {
    const { client, operations } = fakeOVH({});
    await expect(
      client.releaseLease({ ...lease, providerScope: "ovh:project:other" }, async () => {}),
    ).rejects.toBeInstanceOf(ProviderResourceUnresolvedError);
    expect(operations()).toEqual([]);
  });

  it("stops when the cleanup owner changes", async () => {
    const { client, operations } = fakeOVH({
      [`GET ${p(`/instance/${instanceID}`)}`]: () => json(activeInstance),
    });
    await expect(
      client.releaseLease(lease, async () => {
        throw new Error("lease cleanup owner or resource identity changed");
      }),
    ).rejects.toThrow("cleanup owner");
    expect(operations().filter((operation) => operation.startsWith("DELETE"))).toEqual([]);
  });

  it("fails retryably when deletion is not confirmed", async () => {
    const { client } = fakeOVH({
      [`GET ${p(`/instance/${instanceID}`)}`]: () => json(activeInstance),
      [`DELETE ${p(`/instance/${instanceID}`)}`]: () => new Response(null, { status: 200 }),
    });
    await expect(client.releaseLease(lease, async () => {})).rejects.toThrow(
      "deletion is not confirmed",
    );
  });

  it("does not repeat a delete already in progress", async () => {
    let reads = 0;
    const { client, operations } = fakeOVH({
      ...noKeys,
      [`GET ${p(`/instance/${instanceID}`)}`]: () => {
        reads += 1;
        return reads === 1
          ? json({ ...activeInstance, status: "DELETING" })
          : new Response("gone", { status: 404 });
      },
    });
    await client.releaseLease(lease, async () => {});
    expect(operations()).not.toContain(`DELETE /instance/${instanceID}`);
  });

  it("retries the sweep without repeating the instance delete", async () => {
    let present = true;
    let keyFails = true;
    let keys = [{ id: "key-id", name: keyName, publicKey }];
    const { client, operations } = fakeOVH({
      [`GET ${p("/region")}`]: () => json(["BHS5"]),
      [`GET ${p(`/instance/${instanceID}`)}`]: () =>
        present ? json(activeInstance) : new Response("gone", { status: 404 }),
      [`DELETE ${p(`/instance/${instanceID}`)}`]: () => {
        present = false;
        return new Response(null, { status: 200 });
      },
      [`GET ${p("/sshkey")}`]: () => json(keys),
      [`DELETE ${p("/sshkey/key-id")}`]: () => {
        if (keyFails) return new Response("down", { status: 503 });
        keys = [];
        return new Response(null, { status: 200 });
      },
    });
    await expect(client.releaseLease(lease, async () => {})).rejects.toThrow("http 503");
    keyFails = false;
    await client.releaseLease(lease, async () => {});
    const deletes = operations().filter((operation) => operation.startsWith("DELETE"));
    expect(deletes.filter((operation) => operation.includes("/instance/"))).toHaveLength(1);
    expect(deletes.filter((operation) => operation.includes("/sshkey/"))).toHaveLength(2);
    expect(keys).toEqual([]);
  });

  it("removes a retained key by exact id, verifying its lease name", async () => {
    const { client, operations } = fakeOVH({
      [`GET ${p("/sshkey/key-id")}`]: () => json({ id: "key-id", name: keyName, publicKey }),
      [`DELETE ${p("/sshkey/key-id")}`]: () => new Response(null, { status: 200 }),
    });
    await client.releaseLease(
      { ...lease, cloudID: "", providerKeyCleanupID: "key-id" },
      async () => {},
    );
    expect(operations()).toEqual(["GET /sshkey/key-id", "DELETE /sshkey/key-id"]);
  });

  it("refuses a retained key id that now names another key", async () => {
    const { client, operations } = fakeOVH({
      [`GET ${p("/sshkey/key-id")}`]: () => json({ id: "key-id", name: "team-key", publicKey }),
    });
    await expect(
      client.releaseLease(
        { ...lease, cloudID: "", providerKeyCleanupID: "key-id" },
        async () => {},
      ),
    ).rejects.toBeInstanceOf(ProviderResourceUnresolvedError);
    expect(operations()).toEqual(["GET /sshkey/key-id"]);
  });
});

describe("OVH inventory", () => {
  it("lists only crabbox-named instances and never claims a lease from a name", async () => {
    const { client } = fakeOVH({
      [`GET ${p("/instance")}`]: () =>
        json([activeInstance, { ...activeInstance, id: otherInstanceID, name: "production-db" }]),
    });
    const machines = await client.listCrabboxServers();
    expect(machines.map((machine) => machine.cloudID)).toEqual([instanceID]);
    expect(machines[0]?.labels).toEqual({ crabbox: "true", provider: "ovh" });
  });

  it("uses a read-only readiness check", async () => {
    const { client, operations } = fakeOVH({ [`GET ${p("/region")}`]: () => json(["BHS5"]) });
    await expect(client.readinessChecks()).resolves.toEqual([
      expect.objectContaining({ status: "ok", check: "ovh-coordinator" }),
    ]);
    expect(operations()).toEqual(["GET /region"]);
  });

  it("reports readiness failure without leaking secrets", async () => {
    const { client } = fakeOVH({
      [`GET ${p("/region")}`]: () =>
        new Response(`denied ${baseEnv.OVH_APPLICATION_SECRET}`, { status: 403 }),
    });
    const [check] = await client.readinessChecks();
    expect(check?.status).toBe("failed");
    expect(JSON.stringify(check)).not.toContain(baseEnv.OVH_APPLICATION_SECRET);
  });
});
