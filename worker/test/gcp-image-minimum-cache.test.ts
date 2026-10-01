// oxlint-disable eslint/no-await-in-loop -- Sequential creates exercise cache history.
import { afterEach, expect, it, vi } from "vitest";

import { leaseConfig, type LeaseConfig } from "../src/config";
import { appendCreationSteps, creationEvent, withCreationSteps } from "../src/creation-events";
import { GCPProvider } from "../src/fleet";
import type { GCPClient } from "../src/gcp";
import { GCPImageMinimumCache, type GCPImageMinimum } from "../src/gcp-image-minimum-cache";
import { coordinatorGCPImageMinimumCache } from "../src/gcp-token-cache";
import type { Env, LeaseRecord } from "../src/types";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

function environment(): Env {
  return {
    CRABBOX_GCP_PROJECT: "example-project",
    CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
  } as Env;
}

function client(env: Env): GCPClient {
  return (new GCPProvider(env) as unknown as { client: GCPClient }).client;
}

function fixture() {
  const lookups: string[] = [];
  const disks: Record<string, unknown>[] = [];
  const image = vi.fn<typeof fetch>(async () =>
    Response.json({ name: "resolved-source", diskSizeGb: "60" }),
  );
  const insert = vi.fn<typeof fetch>(async () =>
    Response.json({ name: "create-op", targetId: "123" }),
  );
  const operation = vi.fn<typeof fetch>(async () =>
    Response.json({ name: "create-op", status: "DONE", targetId: "123" }),
  );
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input));
    if (url.hostname === "metadata.google.internal")
      return Response.json(
        { access_token: "fixture", expires_in: 3600 },
        { headers: { "Metadata-Flavor": "Google" } },
      );
    if (/\/global\/(images|snapshots)\//.test(url.pathname)) {
      lookups.push(url.pathname);
      return image(input, init);
    }
    if (url.pathname.includes("/global/firewalls/"))
      return Response.json({
        description: "Crabbox-managed SSH ingress",
        network: url.pathname
          .replace("/compute/v1/", "")
          .replace(/firewalls\/.*/, "networks/default"),
        direction: "INGRESS",
        sourceRanges: ["0.0.0.0/0"],
        targetTags: ["crabbox-ssh"],
        allowed: [{ IPProtocol: "tcp", ports: ["22", "2222"] }],
      });
    if (url.pathname.endsWith("/instances") && init?.method === "POST") {
      const body = JSON.parse(String(init.body));
      disks.push(body.disks?.[0]?.initializeParams ?? {});
      return insert(input, init);
    }
    if (url.pathname.endsWith("/operations/create-op/wait")) return operation(input, init);
    if (url.pathname.includes("/instances/"))
      return Response.json({ id: "123", name: "fixture", status: "RUNNING" });
    throw new Error(`unexpected fixture request ${init?.method} ${url.pathname}`);
  });
  return { lookups, disks, image, insert, operation };
}

async function create(env: Env, overrides: Partial<LeaseConfig> = {}) {
  return withCreationSteps(async () => {
    const record: Pick<LeaseRecord, "creationEvents"> = {
      creationEvents: [creationEvent("admission_complete")],
    };
    const result = await client(env).createServerWithFallback(
      {
        ...leaseConfig({ provider: "gcp", class: "tiny", sshPublicKey: "ssh-ed25519 fixture" }),
        serverType: "e2-small",
        serverTypeExplicit: true,
        capacityMarket: "on-demand",
        ...overrides,
      },
      "cbx_abcdef123456",
      "fixture",
      "alice@example.com",
      { onResourceCreated: async () => true },
    );
    record.creationEvents!.push(...(result.server.creationEvents ?? []));
    appendCreationSteps(record);
    return record.creationEvents!;
  });
}

it("accounts for the 3.7 second pre-create lookup and removes it on warm creates", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const env = environment();
  const { image, lookups, disks } = fixture();
  await client(env).ensureFirewall(
    leaseConfig({ provider: "gcp", sshPublicKey: "ssh-ed25519 fixture" }),
  );
  image.mockImplementation(async () => {
    vi.setSystemTime(Date.now() + 3700);
    return Response.json({ name: "resolved-source", diskSizeGb: "60" });
  });
  const cold = await create(env);
  const warm = await create(env, { class: "standard", gcpZone: "us-central1-b" });
  const gap = (events: typeof cold) =>
    Date.parse(events.find((event) => event.phase === "provider_create_request")!.at) -
    Date.parse(events.find((event) => event.phase === "admission_complete")!.at);
  expect(gap(cold)).toBe(3700);
  expect(cold).toContainEqual(
    expect.objectContaining({ step: "gcp.image_minimum", durationMs: 3700, count: 1, errors: 0 }),
  );
  expect(gap(warm)).toBe(0);
  expect(warm).not.toContainEqual(expect.objectContaining({ step: "gcp.image_minimum" }));
  expect(lookups).toHaveLength(1);
  expect(disks.map(({ diskSizeGb }) => diskSizeGb)).toEqual([60, 150]);
  expect(disks.map(({ sourceImage }) => sourceImage)).toEqual([
    "projects/ubuntu-os-cloud/global/images/resolved-source",
    "projects/ubuntu-os-cloud/global/images/resolved-source",
  ]);
  expect(JSON.stringify(cold.filter((event) => event.phase === "coordinator_step"))).not.toMatch(
    /example-project|resolved-source|fixture/,
  );
});

it.each([
  { gcpImage: "global/images/custom" },
  { gcpImage: "global/images/family/custom" },
  { gcpSnapshot: "custom" },
])("caches image, family and snapshot references: %j", async (source) => {
  const env = environment();
  const { lookups } = fixture();
  await create(env, source);
  await create(env, source);
  expect(lookups).toHaveLength(1);
});

it("expires from lookup start after 30 minutes without extending cache hits", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const env = environment();
  const { image, disks } = fixture();
  const startedAt = Date.now();
  image.mockImplementationOnce(async () => {
    vi.setSystemTime(Date.now() + 3700);
    return Response.json({ name: "old-image", diskSizeGb: "60" });
  });
  await create(env);
  vi.setSystemTime(startedAt + 30 * 60_000 - 1);
  await create(env);
  expect(image).toHaveBeenCalledTimes(1);
  vi.setSystemTime(Date.now() + 1);
  image.mockResolvedValueOnce(Response.json({ name: "new-image", diskSizeGb: "90" }));
  await create(env);
  expect(image).toHaveBeenCalledTimes(2);
  expect(disks[2]).toMatchObject({
    sourceImage: "projects/ubuntu-os-cloud/global/images/new-image",
    diskSizeGb: 90,
  });
});

it("isolates execution projects, source projects, source kinds, and coordinator environments", async () => {
  const env = environment();
  const { lookups } = fixture();
  await create(env, { gcpImage: "projects/source-a/global/images/custom" });
  await create(env, { gcpImage: "projects/source-b/global/images/custom" });
  await create(env, { gcpImage: "projects/source-a/global/images/family/custom" });
  await create(env, { gcpSnapshot: "projects/source-a/global/snapshots/custom" });
  await create(env, { gcpImage: "projects/source-a/global/images/custom", gcpProject: "other" });
  await create(environment(), { gcpImage: "projects/source-a/global/images/custom" });
  await create(env, { gcpImage: "projects/source-a/global/images/custom" });
  expect(lookups).toHaveLength(6);
});

it.each(["GCP_CLIENT_EMAIL", "GCP_PRIVATE_KEY"] as const)(
  "invalidates cached image minimums on credential rotation: %s",
  async (field) => {
    const env = { ...environment(), GCP_CLIENT_EMAIL: "fixture", GCP_PRIVATE_KEY: "fixture" };
    const { lookups } = fixture();
    await create(env);
    env[field] += "-rotated";
    await create(env);
    await create(env);
    expect(lookups).toHaveLength(2);
  },
);

it.each([{ gcpRootGB: 8 }, { gcpRootGB: 8, gcpSnapshot: "custom" }, { gcpMachineImage: "custom" }])(
  "never looks up a source minimum for explicit sizes or machine images: %j",
  async (options) => {
    const env = environment();
    const { lookups } = fixture();
    const events = await create(env, options);
    expect(lookups).toHaveLength(0);
    expect(events).not.toContainEqual(expect.objectContaining({ step: "gcp.image_minimum" }));
  },
);

it("skips lookup for a coordinator root size override", async () => {
  const { lookups, disks } = fixture();
  await create({ ...environment(), CRABBOX_GCP_ROOT_GB: "8" });
  expect(lookups).toHaveLength(0);
  expect(disks[0]?.diskSizeGb).toBe(8);
});

it.each([
  [404, "resource missing"],
  [400, "Invalid sourceImage"],
  [400, "diskSizeGb is smaller than image size"],
  [400, "sourceSnapshot is unavailable"],
  [400, "RESOURCE_NOT_FOUND"],
] as const)("invalidates after a synchronous create error: %s %s", async (status, message) => {
  const env = environment();
  const { insert, image } = fixture();
  await create(env);
  insert.mockResolvedValueOnce(new Response(message, { status }));
  await expect(create(env)).rejects.toThrow(message);
  await create(env);
  await create(env);
  expect(image).toHaveBeenCalledTimes(2);
});

it("invalidates after an asynchronous disk-size operation error", async () => {
  const env = environment();
  const { operation, image } = fixture();
  await create(env);
  operation.mockResolvedValueOnce(
    Response.json({
      status: "DONE",
      error: { errors: [{ code: "INVALID_FIELD_VALUE", message: "disk size too small" }] },
    }),
  );
  await expect(create(env)).rejects.toThrow("disk size too small");
  await create(env);
  expect(image).toHaveBeenCalledTimes(2);
});

it("does not discard image minimums for unrelated capacity errors", async () => {
  const env = environment();
  const { insert, image } = fixture();
  await create(env);
  insert.mockResolvedValueOnce(new Response("QUOTA_EXCEEDED", { status: 429 }));
  await expect(create(env)).rejects.toThrow("QUOTA_EXCEEDED");
  await create(env);
  expect(image).toHaveBeenCalledTimes(1);
});

it.each(["failed", "malformed"])(
  "does not cache %s lookups and accounts for failures",
  async (failure) => {
    const env = environment();
    const { image, insert } = fixture();
    image.mockResolvedValueOnce(
      failure === "failed"
        ? new Response("denied", { status: 403 })
        : Response.json({ name: "bad", diskSizeGb: "0" }),
    );
    await withCreationSteps(async () => {
      await expect(
        client(env).createServer(
          leaseConfig({ provider: "gcp", class: "tiny", sshPublicKey: "ssh-ed25519 fixture" }),
          "cbx_abcdef123456",
          "fixture",
          "alice@example.com",
        ),
      ).rejects.toThrow(failure === "failed" ? "denied" : "no name or disk size");
      const record: Pick<LeaseRecord, "creationEvents"> = {};
      appendCreationSteps(record);
      expect(record.creationEvents).toContainEqual(
        expect.objectContaining({ step: "gcp.image_minimum", count: 1, errors: 1 }),
      );
    });
    expect(insert).not.toHaveBeenCalled();
    await create(env);
    await create(env);
    expect(image).toHaveBeenCalledTimes(2);
  },
);

it("bounds the cache to 128 references and evicts the oldest", async () => {
  const cache = new GCPImageMinimumCache();
  const resolve = vi.fn<() => Promise<GCPImageMinimum>>(async () => ({
    source: "resolved",
    diskSizeGb: 60,
  }));
  for (let index = 0; index < 129; index++) await cache.get(String(index), resolve);
  await cache.get("128", resolve);
  expect(resolve).toHaveBeenCalledTimes(129);
  await cache.get("0", resolve);
  expect(resolve).toHaveBeenCalledTimes(130);
});

it.each([false, true])(
  "fences late lookup completion after invalidation (reject=%s)",
  async (reject) => {
    const cache = new GCPImageMinimumCache();
    let finish!: (value: GCPImageMinimum) => void;
    let fail!: (error: Error) => void;
    const resolve = vi.fn<() => Promise<GCPImageMinimum>>(
      () =>
        new Promise<GCPImageMinimum>((done, failed) => {
          finish = done;
          fail = failed;
        }),
    );
    const first = cache.get("source", resolve);
    const joined = cache.get("source", resolve);
    await Promise.resolve();
    expect(resolve).toHaveBeenCalledOnce();
    cache.invalidate();
    const fresh = vi.fn<() => Promise<GCPImageMinimum>>(async () => ({
      source: "new",
      diskSizeGb: 90,
    }));
    await cache.get("source", fresh);
    const settled = Promise.allSettled([first, joined]);
    if (reject) {
      fail(new Error("stale lookup failed"));
    } else {
      finish({ source: "old", diskSizeGb: 60 });
    }
    expect((await settled).map((result) => result.status)).toEqual(
      reject ? ["rejected", "rejected"] : ["fulfilled", "fulfilled"],
    );
    await expect(cache.get("source", fresh)).resolves.toEqual({ source: "new", diskSizeGb: 90 });
    expect(fresh).toHaveBeenCalledOnce();
  },
);

it("discards image minimums when the credential source changes", async () => {
  const env = environment();
  const old = coordinatorGCPImageMinimumCache(env);
  const resolve = vi.fn<() => Promise<GCPImageMinimum>>(async () => ({
    source: "resolved",
    diskSizeGb: 60,
  }));
  await old.get("source", resolve);
  env.CRABBOX_GCP_CREDENTIAL_SOURCE = "service-account-key";
  const next = coordinatorGCPImageMinimumCache(env);
  expect(next).not.toBe(old);
  await next.get("source", resolve);
  await old.get("source", resolve);
  expect(resolve).toHaveBeenCalledTimes(3);
});
