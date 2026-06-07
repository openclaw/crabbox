import { describe, expect, it } from "vitest";

import { leaseConfig } from "../src/config";
import {
  GCPClient,
  gcpEffectiveTags,
  gcpFirewallNameForPolicy,
  gcpFirewallNameForNetwork,
  isFallbackProvisioningError,
  operationDone,
} from "../src/gcp";
import type { Env, ProviderMachine } from "../src/types";

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

  it("rejects invalid configured GCP SSH CIDRs", () => {
    expect(() => new GCPClient({ ...env, CRABBOX_GCP_SSH_CIDRS: "::::/128" })).toThrow(
      "CRABBOX_GCP_SSH_CIDRS entries must be valid",
    );
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
                name: "crabbox-a",
                machineType: "zones/us-central1-a/machineTypes/e2-micro",
                labels: { crabbox: "true" },
              },
            ],
          },
          "zones/europe-west2-b": {
            instances: [
              {
                id: "2",
                name: "crabbox-b",
                zone: "projects/default-project/zones/europe-west2-b",
                machineType: "zones/europe-west2-b/machineTypes/c4-standard-32",
                labels: { crabbox: "true" },
              },
            ],
          },
        },
      });
    };

    const servers = await client.listCrabboxServers();
    expect(servers.map((server) => [server.name, server.region])).toEqual([
      ["crabbox-a", "us-central1-a"],
      ["crabbox-b", "europe-west2-b"],
    ]);
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
