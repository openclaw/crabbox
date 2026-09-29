import { afterEach, expect, it, vi } from "vitest";

import { leaseConfig } from "../src/config";
import { GCPProvider } from "../src/fleet";
import { GCPClient } from "../src/gcp";
import { coordinatorGCPTokenCache } from "../src/gcp-token-cache";
import type { Env } from "../src/types";

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

const policy = {
  description: "Crabbox-managed SSH ingress",
  network: "https://www.googleapis.com/compute/v1/projects/example-project/global/networks/default",
  direction: "INGRESS",
  sourceRanges: ["0.0.0.0/0"],
  targetTags: ["crabbox-ssh"],
  allowed: [{ IPProtocol: "tcp", ports: ["22", "2222"] }],
};

it.each([
  {},
  {
    priority: 1000,
    disabled: false,
    sourceTags: [],
    denied: [],
    allowed: [{ IPProtocol: "tcp", ports: ["2222", "22"] }],
  },
])("skips firewall PUT and operation wait for an exact managed policy: %j", async (overrides) => {
  const client = new GCPClient(environment());
  const calls: string[] = [];
  client.fetcher = async (input, init) => {
    if (String(input).includes("metadata.google.internal"))
      return Response.json(
        { access_token: "fixture", expires_in: 3600 },
        { headers: { "Metadata-Flavor": "Google" } },
      );
    calls.push(`${init?.method} ${new URL(String(input)).pathname}`);
    return Response.json({ ...policy, ...overrides });
  };
  await client.ensureFirewall(
    leaseConfig({ provider: "gcp", sshPublicKey: "ssh-ed25519 fixture" }),
  );
  expect(calls).toEqual(["GET /compute/v1/projects/example-project/global/firewalls/crabbox-ssh"]);
});

it.each([
  { network: "projects/other/global/networks/default" },
  { direction: "EGRESS" },
  { priority: 999 },
  { disabled: true },
  { sourceRanges: ["198.51.100.1/32"] },
  { sourceRanges: undefined },
  { targetTags: ["other"] },
  { sourceTags: ["other"] },
  { sourceServiceAccounts: ["source@example.invalid"] },
  { targetServiceAccounts: ["target@example.invalid"] },
  { destinationRanges: ["0.0.0.0/0"] },
  { denied: [{ IPProtocol: "tcp" }] },
  { allowed: [{ IPProtocol: "udp", ports: ["22", "2222"] }] },
  { allowed: [{ IPProtocol: "tcp", ports: ["22", "2222", "443"] }] },
  { allowed: [{ IPProtocol: "tcp", ports: ["22", "2222"] }, { IPProtocol: "icmp" }] },
])("reconciles and waits for firewall policy drift: %j", async (overrides) => {
  const client = new GCPClient(environment());
  const calls: string[] = [];
  client.fetcher = async (input, init) => {
    if (String(input).includes("metadata.google.internal"))
      return Response.json(
        { access_token: "fixture", expires_in: 3600 },
        { headers: { "Metadata-Flavor": "Google" } },
      );
    const method = init?.method ?? "GET";
    calls.push(method);
    return Response.json(
      method === "GET"
        ? { ...policy, ...overrides }
        : { name: "fixture-operation", status: method === "PUT" ? "PENDING" : "DONE" },
    );
  };
  await client.ensureFirewall(
    leaseConfig({ provider: "gcp", sshPublicKey: "ssh-ed25519 fixture" }),
  );
  expect(calls).toEqual(["GET", "PUT", "POST"]);
});

it("does not skip or mutate an unmanaged exact firewall", async () => {
  const client = new GCPClient(environment());
  const calls: string[] = [];
  client.fetcher = async (input, init) => {
    if (String(input).includes("metadata.google.internal"))
      return Response.json(
        { access_token: "fixture", expires_in: 3600 },
        { headers: { "Metadata-Flavor": "Google" } },
      );
    calls.push(init?.method ?? "GET");
    return Response.json({ ...policy, description: "external" });
  };
  await expect(
    client.ensureFirewall(leaseConfig({ provider: "gcp", sshPublicKey: "ssh-ed25519 fixture" })),
  ).rejects.toThrow("not Crabbox-managed");
  expect(calls).toEqual(["GET"]);
});

it("reuses metadata tokens across coordinator providers and refreshes at the expiry margin", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const env = environment();
  let exchanges = 0;
  vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
    if (String(input).includes("metadata.google.internal")) {
      exchanges++;
      return Response.json(
        { access_token: "fixture", expires_in: 3600 },
        { headers: { "Metadata-Flavor": "Google" } },
      );
    }
    return Response.json({ items: {} });
  });
  await Promise.all([
    new GCPProvider(env).listCrabboxServers(),
    new GCPProvider(env).listCrabboxServers(),
  ]);
  expect(exchanges).toBe(1);
  vi.setSystemTime(Date.now() + 3299_000);
  await new GCPProvider(env).listCrabboxServers();
  expect(exchanges).toBe(1);
  vi.setSystemTime(Date.now() + 1000);
  await new GCPProvider(env).listCrabboxServers();
  expect(exchanges).toBe(2);
  await new GCPProvider(environment()).listCrabboxServers();
  expect(exchanges).toBe(3);
});

it("does not retain failed token exchanges across coordinator providers", async () => {
  const env = environment();
  let exchanges = 0;
  vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
    if (String(input).includes("metadata.google.internal")) {
      exchanges++;
      if (exchanges === 1)
        return new Response("denied", { status: 403, headers: { "Metadata-Flavor": "Google" } });
      return Response.json(
        { access_token: "fixture", expires_in: 3600 },
        { headers: { "Metadata-Flavor": "Google" } },
      );
    }
    return Response.json({ items: {} });
  });
  await expect(new GCPProvider(env).listCrabboxServers()).rejects.toThrow("403");
  await new GCPProvider(env).listCrabboxServers();
  expect(exchanges).toBe(2);
});

it("shares service-account tokens until one minute before expiry and invalidates credential changes", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const keys = await crypto.subtle.generateKey(
    {
      name: "RSASSA-PKCS1-v1_5",
      modulusLength: 2048,
      publicExponent: new Uint8Array([1, 0, 1]),
      hash: "SHA-256",
    },
    true,
    ["sign", "verify"],
  );
  const encoded = btoa(
    String.fromCharCode(...new Uint8Array(await crypto.subtle.exportKey("pkcs8", keys.privateKey))),
  );
  const env = {
    ...environment(),
    CRABBOX_GCP_CREDENTIAL_SOURCE: "service-account-key",
    GCP_CLIENT_EMAIL: "fixture@example.iam.gserviceaccount.com",
    GCP_PRIVATE_KEY: `-----BEGIN PRIVATE KEY-----\n${encoded}\n-----END PRIVATE KEY-----`,
  };
  let exchanges = 0;
  const authorizations: string[] = [];
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    if (
      String(input) === "https://oauth2.googleapis.com/token" ||
      String(input).includes("metadata.google.internal")
    ) {
      exchanges++;
      return Response.json(
        { access_token: `fixture-${exchanges}`, expires_in: 3600 },
        { headers: { "Metadata-Flavor": "Google" } },
      );
    }
    authorizations.push(new Headers(init?.headers).get("authorization") ?? "");
    return Response.json({ items: {} });
  });
  await new GCPProvider(env).listCrabboxServers();
  await new GCPProvider(env).listCrabboxServers();
  expect(exchanges).toBe(1);
  expect(authorizations).toEqual(["Bearer fixture-1", "Bearer fixture-1"]);
  vi.setSystemTime(Date.now() + 3539_000);
  await new GCPProvider(env).listCrabboxServers();
  expect(exchanges).toBe(1);
  vi.setSystemTime(Date.now() + 1000);
  await new GCPProvider(env).listCrabboxServers();
  expect(exchanges).toBe(2);
  env.GCP_CLIENT_EMAIL = "rotated@example.iam.gserviceaccount.com";
  await new GCPProvider(env).listCrabboxServers();
  expect(exchanges).toBe(3);
  env.GCP_PRIVATE_KEY += "\n";
  await new GCPProvider(env).listCrabboxServers();
  expect(exchanges).toBe(4);
  env.CRABBOX_GCP_CREDENTIAL_SOURCE = "metadata";
  await new GCPProvider(env).listCrabboxServers();
  expect(exchanges).toBe(5);
});

it("retains coordinator token reuse when a scoped client performs the first exchange", async () => {
  const env = environment();
  let exchanges = 0;
  vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
    if (String(input).includes("metadata.google.internal")) {
      exchanges++;
      return Response.json(
        { access_token: "fixture", expires_in: 3600 },
        { headers: { "Metadata-Flavor": "Google" } },
      );
    }
    return Response.json({ items: {} });
  });
  const client = new GCPClient(env, undefined, undefined, coordinatorGCPTokenCache(env));
  await client.forScope("us-central1-b", "other-project").listCrabboxServers();
  await new GCPProvider(env).listCrabboxServers();
  expect(exchanges).toBe(1);
});
