import { afterEach, describe, expect, it, vi } from "vitest";

import { leaseConfig } from "../src/config";
import {
  GCPClient,
  gcpEffectiveTags,
  gcpFirewallNameForPolicy,
  gcpFirewallNameForNetwork,
  isFallbackProvisioningError,
  operationDone,
} from "../src/gcp";
import { providerProvisioningCleanupClaim } from "../src/provider-provisioning";
import { leaseProviderName } from "../src/slug";
import type { Env, ProviderMachine } from "../src/types";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function metadataResponse(body: BodyInit | null, init: ResponseInit = {}): Response {
  const headers = new Headers(init.headers);
  headers.set("Metadata-Flavor", "Google");
  return new Response(body, { ...init, headers });
}

function metadataJSON(body: unknown, init: ResponseInit = {}): Response {
  const headers = new Headers(init.headers);
  headers.set("Content-Type", "application/json");
  return metadataResponse(JSON.stringify(body), { ...init, headers });
}

describe("gcp provider", () => {
  const env: Env = {
    FLEET: {} as DurableObjectNamespace,
    HETZNER_TOKEN: "",
    GCP_CLIENT_EMAIL: "test@example.iam.gserviceaccount.com",
    GCP_PRIVATE_KEY: "test-key",
    CRABBOX_GCP_PROJECT: "default-project",
    CRABBOX_GCP_ZONE: "us-central1-a",
  };

  it("waits until operations report DONE", () => {
    expect(operationDone({ name: "operation-1", status: "RUNNING" })).toBe(false);
    expect(operationDone({ name: "operation-1", status: "PENDING" })).toBe(false);
    expect(operationDone({ name: "operation-1" })).toBe(false);
    expect(operationDone({ name: "operation-1", status: "DONE" })).toBe(true);
  });

  it("prefers per-request project over Worker defaults", () => {
    expect(new GCPClient(env).project).toBe("default-project");
    expect(new GCPClient(env, undefined, "request-project").project).toBe("request-project");
  });

  it("uses the metadata server when service account key credentials are omitted", async () => {
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_ZONE: "us-central1-a",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    const calls: Array<{ url: string; headers: Headers; redirect?: RequestRedirect }> = [];
    client.fetcher = async (input, init) => {
      const url = String(input);
      calls.push({ url, headers: new Headers(init?.headers), redirect: init?.redirect });
      if (url.includes("metadata.google.internal")) {
        return metadataJSON({ access_token: "metadata-token", expires_in: 1200 });
      }
      if (url.includes("/aggregated/instances")) {
        return Response.json({ items: {} });
      }
      throw new Error(`unexpected GCP request ${url}`);
    };

    await expect(client.listCrabboxServers()).resolves.toEqual([]);
    expect(calls[0]?.url).toContain("metadata.google.internal");
    expect(calls[0]?.headers.get("Metadata-Flavor")).toBe("Google");
    expect(calls[0]?.redirect).toBe("error");
    expect(calls[1]?.headers.get("Authorization")).toBe("Bearer metadata-token");
  });

  it("retries transient metadata token failures", async () => {
    vi.useFakeTimers();
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    let metadataCalls = 0;
    client.fetcher = async (input) => {
      const url = String(input);
      if (url.includes("metadata.google.internal")) {
        metadataCalls += 1;
        if (metadataCalls === 1) throw new TypeError("connection refused");
        if (metadataCalls === 2) return metadataResponse("client closed", { status: 499 });
        if (metadataCalls === 3)
          return metadataResponse("control plane unavailable", { status: 500 });
        if (metadataCalls === 4) return metadataResponse("bad gateway", { status: 502 });
        if (metadataCalls === 5) return metadataResponse("busy", { status: 503 });
        if (metadataCalls === 6) return metadataResponse("rate limited", { status: 429 });
        return metadataJSON({ access_token: "metadata-token", expires_in: 1200 });
      }
      if (url.includes("/aggregated/instances")) return Response.json({ items: {} });
      throw new Error(`unexpected GCP request ${url}`);
    };

    const result = client.listCrabboxServers();
    await vi.runAllTimersAsync();
    await expect(result).resolves.toEqual([]);
    expect(metadataCalls).toBe(7);
  });

  it("keeps the HTTP status when metadata errors are not JSON", async () => {
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    client.fetcher = async () =>
      metadataResponse("service account disabled", { status: 401, statusText: "Unauthorized" });

    await expect(client.listCrabboxServers()).rejects.toThrow(
      "gcp metadata token: http 401: Unauthorized",
    );
  });

  it("bounds metadata retries", async () => {
    vi.useFakeTimers();
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    let metadataCalls = 0;
    client.fetcher = async () => {
      metadataCalls += 1;
      return metadataResponse("busy", { status: 503, statusText: "Service Unavailable" });
    };

    const result = client.listCrabboxServers().then(
      () => undefined,
      (error: unknown) => error,
    );
    await vi.runAllTimersAsync();
    const error = await result;
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toBe("gcp metadata token: http 503: Service Unavailable");
    expect(metadataCalls).toBe(7);
  });

  it("bounds metadata connection retries", async () => {
    vi.useFakeTimers();
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    let metadataCalls = 0;
    client.fetcher = async () => {
      metadataCalls += 1;
      throw new TypeError("connection refused");
    };

    const result = client.listCrabboxServers().then(
      () => undefined,
      (error: unknown) => error,
    );
    await vi.runAllTimersAsync();
    const error = await result;
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toBe("gcp metadata token: request failed: connection refused");
    expect(metadataCalls).toBe(7);
  });

  it("rejects token responses without the metadata server response marker", async () => {
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    client.fetcher = async () =>
      Response.json({ access_token: "untrusted-token", expires_in: 1200 });

    await expect(client.listCrabboxServers()).rejects.toThrow(
      "gcp metadata token: response missing Metadata-Flavor: Google",
    );
  });

  it("bounds stalled metadata requests with an overall deadline", async () => {
    vi.useFakeTimers();
    const startedAt = Date.now();
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    let metadataCalls = 0;
    client.fetcher = async (_input, init) => {
      metadataCalls += 1;
      return await new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener(
          "abort",
          () => reject(new DOMException("aborted", "AbortError")),
          { once: true },
        );
      });
    };

    const result = client.listCrabboxServers().then(
      () => undefined,
      (error: unknown) => error,
    );
    await vi.runAllTimersAsync();
    const error = await result;
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toBe("gcp metadata token: request failed: request timed out");
    expect(metadataCalls).toBeGreaterThan(1);
    expect(Date.now() - startedAt).toBeGreaterThanOrEqual(60_000);
  });

  it("keeps the metadata timeout active while reading the response body", async () => {
    vi.useFakeTimers();
    const startedAt = Date.now();
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    let metadataCalls = 0;
    client.fetcher = async (_input, init) => {
      metadataCalls += 1;
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          init?.signal?.addEventListener(
            "abort",
            () => controller.error(new DOMException("aborted", "AbortError")),
            { once: true },
          );
        },
      });
      return metadataResponse(body);
    };

    const result = client.listCrabboxServers().then(
      () => undefined,
      (error: unknown) => error,
    );
    await vi.runAllTimersAsync();
    const error = await result;
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toBe("gcp metadata token: request failed: request timed out");
    expect(metadataCalls).toBeGreaterThan(1);
    expect(Date.now() - startedAt).toBeGreaterThanOrEqual(60_000);
  });

  it("refreshes metadata tokens at the five-minute cache boundary", async () => {
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
    };
    const client = new GCPClient(metadataEnv);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "expiring-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 300,
    };
    const authorizations: string[] = [];
    let metadataCalls = 0;
    client.fetcher = async (input, init) => {
      if (String(input).includes("metadata.google.internal")) {
        metadataCalls += 1;
        return metadataJSON({ access_token: "fresh-token", expires_in: 3600 });
      }
      authorizations.push(new Headers(init?.headers).get("Authorization") ?? "");
      return Response.json({ items: {} });
    };

    await expect(client.listCrabboxServers()).resolves.toEqual([]);
    expect(metadataCalls).toBe(1);
    expect(authorizations).toEqual(["Bearer fresh-token"]);
  });

  it("keeps service account key tokens until the one-minute cache boundary", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "cached-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 120,
    };
    const calls: Array<{ url: string; authorization: string }> = [];
    client.fetcher = async (input, init) => {
      calls.push({
        url: String(input),
        authorization: new Headers(init?.headers).get("Authorization") ?? "",
      });
      return Response.json({ items: {} });
    };

    await expect(client.listCrabboxServers()).resolves.toEqual([]);
    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toContain("/aggregated/instances");
    expect(calls[0]?.authorization).toBe("Bearer cached-token");
  });

  it("rejects partial service account key credentials", () => {
    expect(() => new GCPClient({ ...env, GCP_PRIVATE_KEY: "" })).toThrow(
      "GCP_CLIENT_EMAIL and GCP_PRIVATE_KEY must be configured together",
    );
    expect(() => new GCPClient({ ...env, GCP_CLIENT_EMAIL: "" })).toThrow(
      "GCP_CLIENT_EMAIL and GCP_PRIVATE_KEY must be configured together",
    );
  });

  it("requires explicit metadata credential source when service account key credentials are omitted", () => {
    const metadataEnv: Env = {
      FLEET: {} as DurableObjectNamespace,
      HETZNER_TOKEN: "",
      CRABBOX_GCP_PROJECT: "default-project",
      CRABBOX_GCP_ZONE: "us-central1-a",
    };
    expect(() => new GCPClient(metadataEnv)).toThrow(
      "GCP_CLIENT_EMAIL and GCP_PRIVATE_KEY are required unless CRABBOX_GCP_CREDENTIAL_SOURCE=metadata",
    );
  });

  it("rejects invalid configured GCP credential sources", () => {
    expect(
      () => new GCPClient({ ...env, CRABBOX_GCP_CREDENTIAL_SOURCE: "workload-identity" }),
    ).toThrow("CRABBOX_GCP_CREDENTIAL_SOURCE must be metadata or service-account-key");
  });

  it("rejects invalid configured GCP SSH CIDRs", () => {
    expect(() => new GCPClient({ ...env, CRABBOX_GCP_SSH_CIDRS: "::::/128" })).toThrow(
      "CRABBOX_GCP_SSH_CIDRS entries must be valid",
    );
  });

  it("treats only the exact zonal instance as absent", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    let message =
      "The resource 'projects/default-project/zones/us-central1-a/instances/crabbox-blue-lobster' was not found";
    client.fetcher = async () =>
      new Response(JSON.stringify({ error: { code: 404, message } }), { status: 404 });

    await expect(client.findServer("crabbox-blue-lobster")).resolves.toBeUndefined();
    message = "The resource 'projects/default-project' was not found";
    await expect(client.findServer("crabbox-blue-lobster")).rejects.toThrow(
      "projects/default-project",
    );
  });

  it("treats only an exact missing instance DELETE as complete", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    let message =
      "The resource 'projects/default-project/zones/us-central1-a/instances/crabbox-blue-lobster' was not found";
    client.fetcher = async () =>
      new Response(JSON.stringify({ error: { code: 404, message } }), { status: 404 });

    await expect(client.deleteServer("crabbox-blue-lobster")).resolves.toBeUndefined();
    message = "The resource 'projects/default-project' was not found";
    await expect(client.deleteServer("crabbox-blue-lobster")).rejects.toThrow(
      "projects/default-project",
    );
  });

  it("does not delete a foreign same-name instance after a failed create", async () => {
    const client = new GCPClient(env);
    const calls: string[] = [];
    const internal = client as unknown as {
      ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void>;
      gcp<T>(method: string, path: string, body?: unknown): Promise<T>;
    };
    vi.spyOn(internal, "ensureFirewall").mockResolvedValue();
    vi.spyOn(internal, "gcp").mockImplementation(async (method, path) => {
      calls.push(`${method} ${path}`);
      if (method === "POST") throw new Error("already exists");
      return {
        id: "1",
        name: leaseProviderName("cbx_abcdef123456", "blue-lobster"),
        zone: "projects/default-project/zones/us-central1-a",
        machineType: "zones/us-central1-a/machineTypes/e2-micro",
        labels: {
          crabbox: "true",
          created_by: "crabbox",
          provider: "gcp",
          lease: "cbx_000000000000",
          owner: "foreign_example_com",
          slug: "blue-lobster",
        },
      } as T;
    });

    const error = await client
      .createServer(
        leaseConfig({
          provider: "gcp",
          serverType: "e2-micro",
          gcpProject: "default-project",
          gcpZone: "us-central1-a",
          sshPublicKey: "ssh-ed25519 test",
        }),
        "cbx_abcdef123456",
        "blue-lobster",
        "alice@example.com",
      )
      .catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(Error);
    expect(String(error)).toContain("provisioning cleanup failed closed");
    expect(providerProvisioningCleanupClaim(error)).toEqual({
      provider: "gcp",
      cloudID: leaseProviderName("cbx_abcdef123456", "blue-lobster"),
      region: "us-central1-a",
      providerProject: "default-project",
    });
    expect(calls.some((call) => call.startsWith("DELETE "))).toBe(false);
  });

  it("recovers when another create wins the shared firewall race", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    const calls: string[] = [];
    let firewallReads = 0;
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      calls.push(`${method} ${url.pathname}`);
      if (url.pathname.includes("/global/firewalls/") && method === "GET") {
        firewallReads += 1;
        return firewallReads === 1
          ? new Response("not found", { status: 404 })
          : Response.json({ description: "Crabbox-managed SSH ingress" });
      }
      if (url.pathname.endsWith("/global/firewalls") && method === "POST") {
        return new Response("already exists", { status: 409 });
      }
      return Response.json({});
    };

    await (
      client as unknown as { ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void> }
    ).ensureFirewall(
      leaseConfig({
        provider: "gcp",
        gcpSSHCIDRs: ["198.51.100.77/32"],
        sshPublicKey: "ssh-ed25519 test",
      }),
    );

    expect(calls.filter((call) => call.startsWith("GET "))).toHaveLength(2);
    expect(calls.filter((call) => call.startsWith("POST "))).toHaveLength(1);
    expect(calls.filter((call) => call.startsWith("PUT "))).toHaveLength(1);
  });

  it("waits for a raced firewall insert before reconciling policy", async () => {
    vi.useFakeTimers();
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    let firewallReads = 0;
    let firewallUpdates = 0;
    let operationWaits = 0;
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      if (url.pathname.includes("/global/firewalls/") && method === "GET") {
        firewallReads += 1;
        return firewallReads < 3
          ? new Response("not found", { status: 404 })
          : Response.json({ description: "Crabbox-managed SSH ingress" });
      }
      if (url.pathname.endsWith("/global/firewalls") && method === "POST") {
        return new Response("already exists", { status: 409 });
      }
      if (url.pathname.includes("/global/firewalls/") && method === "PUT") {
        firewallUpdates += 1;
        return firewallUpdates === 1
          ? new Response("operation in progress", { status: 409 })
          : Response.json({ name: "op-raced", status: "PENDING" });
      }
      if (url.pathname.endsWith("/global/operations/op-raced/wait") && method === "POST") {
        operationWaits += 1;
        return Response.json({ name: "op-raced", status: "DONE" });
      }
      return Response.json({});
    };

    const ensure = (
      client as unknown as { ensureFirewall(config: ReturnType<typeof leaseConfig>): Promise<void> }
    ).ensureFirewall(
      leaseConfig({
        provider: "gcp",
        gcpSSHCIDRs: ["198.51.100.77/32"],
        sshPublicKey: "ssh-ed25519 test",
      }),
    );
    await vi.runAllTimersAsync();
    await ensure;

    expect(firewallReads).toBe(4);
    expect(firewallUpdates).toBe(2);
    expect(operationWaits).toBe(1);
  });

  it("lists Crabbox machines across aggregated GCP zones", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      expect(url.pathname).toBe("/compute/v1/projects/default-project/aggregated/instances");
      expect(url.searchParams.get("filter")).toBe("labels.crabbox = true");
      expect(url.searchParams.get("returnPartialSuccess")).toBe("true");
      return Response.json({
        items: {
          "zones/us-central1-a": {
            instances: [
              {
                id: "1",
                name: leaseProviderName("cbx_000000000001", "alpha"),
                machineType: "zones/us-central1-a/machineTypes/e2-micro",
                labels: {
                  crabbox: "true",
                  created_by: "crabbox",
                  provider: "gcp",
                  lease: "cbx_000000000001",
                  slug: "alpha",
                },
              },
              {
                id: "forged",
                name: "crabbox-wrong-deterministic-name",
                machineType: "zones/us-central1-a/machineTypes/e2-micro",
                labels: {
                  crabbox: "true",
                  created_by: "crabbox",
                  provider: "gcp",
                  lease: "cbx_000000000099",
                  slug: "forged",
                },
              },
            ],
          },
          "zones/europe-west2-b": {
            instances: [
              {
                id: "2",
                name: leaseProviderName("cbx_000000000002", "bravo"),
                zone: "projects/default-project/zones/europe-west2-b",
                machineType: "zones/europe-west2-b/machineTypes/c4-standard-32",
                labels: {
                  crabbox: "true",
                  created_by: "crabbox",
                  provider: "gcp",
                  lease: "cbx_000000000002",
                  slug: "bravo",
                },
              },
            ],
          },
        },
      });
    };

    const servers = await client.listCrabboxServers();
    expect(servers.map((server) => [server.name, server.region])).toEqual([
      [leaseProviderName("cbx_000000000001", "alpha"), "us-central1-a"],
      [leaseProviderName("cbx_000000000002", "bravo"), "europe-west2-b"],
    ]);
  });

  it("recovers a lease only through its deterministic canonical instance", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    let leaseLabel = "cbx_abcdef123456";
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      expect(url.pathname).toBe(
        "/compute/v1/projects/default-project/zones/us-central1-a/instances/crabbox-blue-lobster-c80c2195",
      );
      return Response.json({
        id: "1",
        name: "crabbox-blue-lobster-c80c2195",
        machineType: "zones/us-central1-a/machineTypes/e2-micro",
        labels: {
          crabbox: "true",
          created_by: "crabbox",
          provider: "gcp",
          lease: leaseLabel,
          slug: "blue-lobster",
        },
      });
    };

    await expect(
      client.recoverServerForLease("cbx_abcdef123456", "blue-lobster"),
    ).resolves.toMatchObject({
      name: "crabbox-blue-lobster-c80c2195",
      labels: { lease: "cbx_abcdef123456" },
    });
    leaseLabel = "cbx_000000000001";
    await expect(
      client.recoverServerForLease("cbx_abcdef123456", "blue-lobster"),
    ).resolves.toBeUndefined();
  });

  it("recovers pre-upgrade fallback-zone instances by exact canonical name", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    const expectedName = leaseProviderName("cbx_abcdef123456", "blue-lobster");
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      if (url.pathname.endsWith(`/instances/${expectedName}`)) {
        return new Response("not found", { status: 404 });
      }
      expect(url.pathname).toBe("/compute/v1/projects/default-project/aggregated/instances");
      return Response.json({
        items: {
          "zones/us-central1-b": {
            instances: [
              {
                id: "1",
                name: expectedName,
                zone: "projects/default-project/zones/us-central1-b",
                machineType: "zones/us-central1-b/machineTypes/e2-micro",
                labels: {
                  crabbox: "true",
                  created_by: "crabbox",
                  provider: "gcp",
                  lease: "cbx_abcdef123456",
                  slug: "blue-lobster",
                },
              },
            ],
          },
        },
      });
    };

    await expect(
      client.recoverServerForLease("cbx_abcdef123456", "blue-lobster"),
    ).resolves.toMatchObject({
      name: expectedName,
      region: "us-central1-b",
    });
  });

  it("rejects ambiguous exact-name fallback-zone recovery", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    const leaseID = "cbx_abcdef123456";
    const slug = "blue-lobster";
    const expectedName = leaseProviderName(leaseID, slug);
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      if (url.pathname.endsWith(`/instances/${expectedName}`)) {
        return new Response("not found", { status: 404 });
      }
      const instance = (zone: string) => ({
        id: zone,
        name: expectedName,
        zone: `projects/default-project/zones/${zone}`,
        machineType: `zones/${zone}/machineTypes/e2-micro`,
        labels: {
          crabbox: "true",
          created_by: "crabbox",
          provider: "gcp",
          lease: leaseID,
          slug,
        },
      });
      return Response.json({
        items: {
          "zones/us-central1-b": { instances: [instance("us-central1-b")] },
          "zones/us-central1-c": { instances: [instance("us-central1-c")] },
        },
      });
    };

    await expect(client.recoverServerForLease(leaseID, slug)).rejects.toThrow(
      "ambiguous GCP recovery",
    );
  });

  it("publishes each fallback zone before creating an instance", async () => {
    const client = new GCPClient(env);
    const attemptedZones: string[] = [];
    const createCalls: string[] = [];
    vi.spyOn(GCPClient.prototype, "createServer").mockImplementation(async (config) => {
      createCalls.push(config.gcpZone);
      if (config.gcpZone === "us-central1-a") {
        throw new Error("ZONE_RESOURCE_POOL_EXHAUSTED");
      }
      return {
        provider: "gcp",
        id: 1,
        cloudID: "crabbox-blue-lobster-c80c2195",
        name: "crabbox-blue-lobster-c80c2195",
        status: "running",
        serverType: config.serverType,
        region: config.gcpZone,
        host: "192.0.2.10",
        labels: {},
      };
    });

    await client.createServerWithFallback(
      leaseConfig({
        provider: "gcp",
        gcpZone: "us-central1-a",
        capacity: { availabilityZones: ["us-central1-b"] },
        serverType: "e2-micro",
        serverTypeExplicit: true,
        sshPublicKey: "ssh-ed25519 test",
      }),
      "cbx_abcdef123456",
      "blue-lobster",
      "alice@example.com",
      {
        async onTargetAttempt(target) {
          attemptedZones.push(target.region ?? "");
        },
      },
    );

    expect(attemptedZones).toEqual(["us-central1-a", "us-central1-b"]);
    expect(createCalls).toEqual(["us-central1-a", "us-central1-b"]);
  });

  it("creates and deletes machine images through Compute Engine", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    const calls: Array<{ method: string; path: string; body: unknown }> = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const body = init?.body ? JSON.parse(String(init.body)) : undefined;
      calls.push({ method: init?.method ?? "GET", path: url.pathname + url.search, body });
      if (url.pathname.endsWith("/global/operations/op-1/wait")) {
        return Response.json({ name: "op-1", status: "DONE" });
      }
      if (url.pathname.endsWith("/global/machineImages/checkpoint-gcp") && init?.method === "GET") {
        return Response.json({
          name: "checkpoint-gcp",
          selfLink: "projects/default-project/global/machineImages/checkpoint-gcp",
          status: "READY",
        });
      }
      return Response.json({ name: "op-1", status: "PENDING" });
    };

    const image = await client.createImage("crabbox-source", "checkpoint-gcp");
    await client.deleteImage("checkpoint-gcp");

    expect(image).toMatchObject({
      id: "checkpoint-gcp",
      provider: "gcp",
      kind: "gcp-machine-image",
      state: "ready",
    });
    expect(calls.map((call) => `${call.method} ${call.path}`)).toEqual([
      "POST /compute/v1/projects/default-project/global/machineImages",
      "POST /compute/v1/projects/default-project/global/operations/op-1/wait",
      "GET /compute/v1/projects/default-project/global/machineImages/checkpoint-gcp",
      "DELETE /compute/v1/projects/default-project/global/machineImages/checkpoint-gcp",
      "POST /compute/v1/projects/default-project/global/operations/op-1/wait",
    ]);
    expect(calls[0]?.body).toMatchObject({
      name: "checkpoint-gcp",
      sourceInstance: "zones/us-central1-a/instances/crabbox-source",
    });
  });

  it("routes kind-specific snapshot reads and deletes to GCP snapshots", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    const calls: Array<{ method: string; path: string }> = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      calls.push({ method: init?.method ?? "GET", path: url.pathname + url.search });
      if (url.pathname.endsWith("/global/operations/op-1/wait")) {
        return Response.json({ name: "op-1", status: "DONE" });
      }
      if (url.pathname.endsWith("/global/snapshots/checkpoint-gcp") && init?.method !== "DELETE") {
        return Response.json({
          name: "checkpoint-gcp",
          selfLink: "projects/default-project/global/snapshots/checkpoint-gcp",
          status: "READY",
        });
      }
      return Response.json({ name: "op-1", status: "PENDING" });
    };

    const image = await client.getImage(
      "projects/default-project/global/snapshots/checkpoint-gcp",
      "gcp-disk-snapshot",
    );
    await client.deleteImage(
      "projects/default-project/global/snapshots/checkpoint-gcp",
      "gcp-disk-snapshot",
    );

    expect(image).toMatchObject({
      id: "checkpoint-gcp",
      provider: "gcp",
      kind: "gcp-disk-snapshot",
    });
    expect(calls.map((call) => `${call.method} ${call.path}`)).toEqual([
      "GET /compute/v1/projects/default-project/global/snapshots/checkpoint-gcp",
      "DELETE /compute/v1/projects/default-project/global/snapshots/checkpoint-gcp",
      "POST /compute/v1/projects/default-project/global/operations/op-1/wait",
    ]);
  });

  it("creates instances from machine images without boot disk initialization", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    const calls: Array<{
      method: string;
      path: string;
      body: Record<string, unknown> | undefined;
    }> = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      const body = init?.body
        ? (JSON.parse(String(init.body)) as Record<string, unknown>)
        : undefined;
      calls.push({ method, path: url.pathname + url.search, body });
      if (url.pathname.endsWith("/global/firewalls/crabbox-ssh") && method === "GET") {
        return new Response("not found", { status: 404 });
      }
      if (url.pathname.endsWith("/global/operations/op-firewall/wait")) {
        return Response.json({ name: "op-firewall", status: "DONE" });
      }
      if (url.pathname.endsWith("/zones/us-central1-a/operations/op-instance/wait")) {
        return Response.json({ name: "op-instance", status: "DONE" });
      }
      if (url.pathname.endsWith("/global/firewalls") && method === "POST") {
        return Response.json({ name: "op-firewall", status: "PENDING" });
      }
      if (url.pathname.endsWith("/zones/us-central1-a/instances") && method === "POST") {
        return Response.json({ name: "op-instance", status: "PENDING" });
      }
      if (url.pathname.includes("/zones/us-central1-a/instances/crabbox-blue-lobster-")) {
        return Response.json({
          id: "123",
          name: url.pathname.split("/").pop(),
          status: "RUNNING",
          machineType: "zones/us-central1-a/machineTypes/e2-micro",
          networkInterfaces: [{ accessConfigs: [{ natIP: "192.0.2.5" }] }],
        });
      }
      return Response.json({});
    };

    const config = leaseConfig({
      provider: "gcp",
      serverType: "e2-micro",
      gcpMachineImage: "checkpoint-gcp",
      sshPublicKey: "ssh-ed25519 test",
    });
    const server = await client.createServer(
      config,
      "cbx_123456789abc",
      "blue-lobster",
      "alice@example.com",
    );

    const createCall = calls.find(
      (call) => call.method === "POST" && call.path.includes("/zones/us-central1-a/instances?"),
    );
    expect(server.host).toBe("192.0.2.5");
    expect(createCall?.path).toContain(
      "sourceMachineImage=projects%2Fdefault-project%2Fglobal%2FmachineImages%2Fcheckpoint-gcp",
    );
    expect(createCall?.body).not.toHaveProperty("disks");
    expect(String(createCall?.body?.name)).toMatch(/^crabbox-blue-lobster-/);
  });

  it("creates instances from disk snapshots without forcing default disk size", async () => {
    const client = new GCPClient(env);
    (client as unknown as { cache: { token: string; expiresAt: number } }).cache = {
      token: "test-token",
      expiresAt: Math.trunc(Date.now() / 1000) + 3600,
    };
    const calls: Array<{
      method: string;
      path: string;
      body: Record<string, unknown> | undefined;
    }> = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      const body = init?.body
        ? (JSON.parse(String(init.body)) as Record<string, unknown>)
        : undefined;
      calls.push({ method, path: url.pathname + url.search, body });
      if (url.pathname.endsWith("/global/firewalls/crabbox-ssh") && method === "GET") {
        return new Response("not found", { status: 404 });
      }
      if (url.pathname.endsWith("/global/operations/op-firewall/wait")) {
        return Response.json({ name: "op-firewall", status: "DONE" });
      }
      if (url.pathname.endsWith("/zones/us-central1-a/operations/op-instance/wait")) {
        return Response.json({ name: "op-instance", status: "DONE" });
      }
      if (url.pathname.endsWith("/global/firewalls") && method === "POST") {
        return Response.json({ name: "op-firewall", status: "PENDING" });
      }
      if (url.pathname.endsWith("/zones/us-central1-a/instances") && method === "POST") {
        return Response.json({ name: "op-instance", status: "PENDING" });
      }
      if (url.pathname.includes("/zones/us-central1-a/instances/crabbox-blue-lobster-")) {
        return Response.json({
          id: "123",
          name: url.pathname.split("/").pop(),
          status: "RUNNING",
          machineType: "zones/us-central1-a/machineTypes/e2-micro",
          networkInterfaces: [{ accessConfigs: [{ natIP: "192.0.2.5" }] }],
        });
      }
      return Response.json({});
    };

    await client.createServer(
      leaseConfig({
        provider: "gcp",
        serverType: "e2-micro",
        gcpSnapshot: "checkpoint-gcp",
        sshPublicKey: "ssh-ed25519 test",
      }),
      "cbx_123456789abc",
      "blue-lobster",
      "alice@example.com",
    );

    const createCall = calls.find(
      (call) => call.method === "POST" && call.path.endsWith("/zones/us-central1-a/instances"),
    );
    const disks = createCall?.body?.disks as Array<{ initializeParams?: Record<string, unknown> }>;
    expect(disks[0]?.initializeParams).toMatchObject({
      sourceSnapshot: "projects/default-project/global/snapshots/checkpoint-gcp",
      diskType: "zones/us-central1-a/diskTypes/pd-balanced",
    });
    expect(disks[0]?.initializeParams).not.toHaveProperty("diskSizeGb");
  });

  it("keeps exact GCP types eligible for zone fallback", async () => {
    const attempts: string[] = [];
    const original = GCPClient.prototype.createServer;
    GCPClient.prototype.createServer = async function (config): Promise<ProviderMachine> {
      attempts.push(`${config.gcpZone}/${config.serverType}`);
      if (config.gcpZone === "europe-west2-b") {
        return {
          provider: "gcp",
          id: 2,
          cloudID: "crabbox-b",
          name: "crabbox-b",
          status: "RUNNING",
          serverType: config.serverType,
          host: "192.0.2.10",
          region: config.gcpZone,
          labels: {},
        };
      }
      throw new Error("quota exceeded");
    };
    try {
      const client = new GCPClient(env, "us-central1-a");
      const config = leaseConfig({
        provider: "gcp",
        serverType: "c4-standard-32",
        serverTypeExplicit: true,
        gcpZone: "us-central1-a",
        capacity: { market: "spot", availabilityZones: ["europe-west2-b"] },
        sshPublicKey: "ssh-ed25519 test",
      });
      const result = await client.createServerWithFallback(
        config,
        "cbx_123456789abc",
        "blue-lobster",
        "peter@example.com",
      );
      expect(result.server.region).toBe("europe-west2-b");
      expect(attempts).toEqual(["us-central1-a/c4-standard-32", "europe-west2-b/c4-standard-32"]);
    } finally {
      GCPClient.prototype.createServer = original;
    }
  });

  it("uses network-specific firewall names", () => {
    expect(gcpFirewallNameForNetwork("default")).toBe("crabbox-ssh");
    expect(gcpFirewallNameForNetwork("projects/p/global/networks/default")).toBe("crabbox-ssh");
    expect(gcpFirewallNameForNetwork("crabbox-ci")).toBe("crabbox-ssh-crabbox-ci");
    expect(gcpFirewallNameForNetwork("projects/p/global/networks/123_custom")).toBe(
      "crabbox-ssh-net-123-custom",
    );
  });

  it("adds an ingress-policy suffix to non-default firewall names", () => {
    expect(
      gcpFirewallNameForPolicy("default", ["0.0.0.0/0"], ["crabbox-ssh"], ["2222", "22"]),
    ).toBe("crabbox-ssh");
    expect(
      gcpFirewallNameForPolicy("default", ["198.51.100.7/32"], ["crabbox-ssh"], ["2222", "22"]),
    ).not.toBe("crabbox-ssh");
    expect(
      gcpFirewallNameForPolicy("crabbox-ci", ["198.51.100.7/32"], ["crabbox-ssh"], ["2222", "22"]),
    ).toMatch(/^crabbox-ssh-crabbox-ci-[0-9a-f]{8}$/);
    expect(
      gcpFirewallNameForPolicy(
        "this-is-a-very-long-custom-network-name-that-would-fill-the-firewall-name",
        ["198.51.100.7/32"],
        ["crabbox-ssh"],
        ["2222", "22"],
      ).length,
    ).toBeLessThanOrEqual(63);
  });

  it("replaces default GCP tags when request tags are explicit", () => {
    expect(gcpEffectiveTags(["crabbox-ssh"], [])).toEqual(["crabbox-ssh"]);
    expect(gcpEffectiveTags(["crabbox-ssh"], ["crabbox-ci", "crabbox-ci"])).toEqual(["crabbox-ci"]);
    expect(gcpEffectiveTags(["  "], [])).toEqual(["crabbox-ssh"]);
    expect(gcpEffectiveTags(["crabbox-ssh"], ["  "])).toEqual(["crabbox-ssh"]);
  });

  it("treats unavailable machine types as fallback-eligible", () => {
    expect(
      isFallbackProvisioningError(
        "gcp POST /zones/us-central1-a/instances: http 400: Invalid value for field 'resource.machineType': 'zones/us-central1-a/machineTypes/c4-standard-192'. The referenced resource does not exist.",
      ),
    ).toBe(true);
    expect(
      isFallbackProvisioningError(
        "gcp POST /zones/us-central1-a/instances: http 404: The resource 'projects/p/zones/us-central1-a/machineTypes/c4-standard-192' was not found",
      ),
    ).toBe(true);
    expect(
      isFallbackProvisioningError(
        "gcp POST /zones/us-central1-a/instances: http 400: invalid labels",
      ),
    ).toBe(false);
  });
});
