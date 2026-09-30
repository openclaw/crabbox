import { afterEach, expect, it, vi } from "vitest";

import { leaseConfig } from "../src/config";
import { appendCreationSteps, withCreationSteps } from "../src/creation-events";
import { GCPClient } from "../src/gcp";
import { HetznerClient } from "../src/hetzner";
import type { Env, LeaseRecord } from "../src/types";

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

it("measures GCP token, firewall, disk/instance, and operation boundaries without extra requests", async () => {
  vi.useFakeTimers();
  vi.setSystemTime(0);
  const env = {
    CRABBOX_GCP_PROJECT: "synthetic-project",
    CRABBOX_GCP_ZONE: "us-central1-a",
    CRABBOX_GCP_CREDENTIAL_SOURCE: "metadata",
  } as Env;
  const client = new GCPClient(env);
  const requests: string[] = [];
  client.fetcher = vi.fn<typeof fetch>(async (input, init) => {
    const url = new URL(String(input));
    const method = init?.method ?? "GET";
    requests.push(`${method} ${url.pathname}`);
    vi.advanceTimersByTime(10);
    if (url.hostname === "metadata.google.internal")
      return Response.json(
        { access_token: "synthetic-secret", expires_in: 3600 },
        { headers: { "Metadata-Flavor": "Google" } },
      );
    if (url.pathname.includes("/global/firewalls/") && method === "GET")
      return Response.json({ description: "Crabbox-managed" });
    if (url.pathname.includes("/global/firewalls/") && method === "PUT")
      return Response.json({ name: "firewall-op" });
    if (url.pathname.endsWith("/operations/firewall-op/wait"))
      return Response.json({ name: "firewall-op", status: "DONE" });
    if (url.pathname.endsWith("/instances") && method === "POST")
      return Response.json({ name: "instance-op", targetId: "123" });
    if (url.pathname.endsWith("/operations/instance-op/wait"))
      return Response.json({ name: "instance-op", status: "DONE", targetId: "123" });
    if (url.pathname.includes("/instances/") && method === "GET")
      return Response.json({
        id: "123",
        name: "synthetic-instance",
        status: "RUNNING",
        networkInterfaces: [{ accessConfigs: [{ natIP: "192.0.2.1" }] }],
      });
    throw new Error(`unexpected fixture request ${method} ${url.pathname}`);
  });
  await withCreationSteps(async () => {
    await client.createServerWithFallback(
      leaseConfig(
        { gcpRootGB: 400, provider: "gcp", class: "tiny", sshPublicKey: "ssh-ed25519 synthetic" },
        env,
      ),
      "cbx_abcdef123456",
      "test",
      "alice@example.com",
    );
    const lease: Pick<LeaseRecord, "creationEvents"> = {};
    appendCreationSteps(lease);
    for (const [step, durationMs] of Object.entries({
      "gcp.token_mint": 10,
      "gcp.firewall_get": 10,
      "gcp.firewall_put": 10,
      "gcp.firewall_operation_wait": 10,
      "gcp.disk_instance_insert": 10,
      "gcp.instance_operation_wait": 10,
      "gcp.zone_selection": 0,
      "gcp.image_selection": 0,
    })) {
      expect(lease.creationEvents).toContainEqual(
        expect.objectContaining({ step, durationMs, count: 1, errors: 0 }),
      );
    }
    expect(requests).toHaveLength(7);
    expect(JSON.stringify(lease)).not.toMatch(
      /synthetic-secret|synthetic-project|synthetic-instance|firewall-op|instance-op/,
    );
  });
});

it("measures Hetzner key registration, image selection and server create without extra requests", async () => {
  vi.useFakeTimers();
  vi.setSystemTime(0);
  const requests: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn<typeof fetch>(async (input, init) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      requests.push(`${method} ${url.pathname}`);
      vi.advanceTimersByTime(20);
      if (url.pathname === "/v1/ssh_keys" && method === "GET")
        return Response.json({ ssh_keys: [], meta: { pagination: { next_page: null } } });
      if (url.pathname === "/v1/ssh_keys" && method === "POST")
        return Response.json({ ssh_key: { ...JSON.parse(String(init?.body)), id: 7 } });
      if (url.pathname === "/v1/servers" && method === "POST")
        return Response.json({
          server: { id: 123, status: "running", public_net: { ipv4: { ip: "192.0.2.1" } } },
        });
      throw new Error(`unexpected fixture request ${method} ${url.pathname}`);
    }),
  );
  await withCreationSteps(async () => {
    await new HetznerClient({ HETZNER_TOKEN: "synthetic-secret" } as Env).createServerWithFallback(
      leaseConfig({ provider: "hetzner", class: "tiny", sshPublicKey: "ssh-ed25519 synthetic" }),
      "cbx_abcdef123456",
      "test",
      "alice@example.com",
    );
    const lease: Pick<LeaseRecord, "creationEvents"> = {};
    appendCreationSteps(lease);
    for (const [step, durationMs] of Object.entries({
      "hetzner.ssh_key_registration": 60,
      "hetzner.image_selection": 0,
      "hetzner.server_create": 20,
    })) {
      expect(lease.creationEvents).toContainEqual(
        expect.objectContaining({ step, durationMs, count: 1, errors: 0 }),
      );
    }
    expect(requests).toEqual([
      "GET /v1/ssh_keys",
      "GET /v1/ssh_keys",
      "POST /v1/ssh_keys",
      "POST /v1/servers",
    ]);
    expect(JSON.stringify(lease)).not.toContain("synthetic-secret");
  });
});
