import { afterEach, expect, it, vi } from "vitest";

import { leaseConfig } from "../src/config";
import { GCPProvider } from "../src/fleet";
import type { GCPClient } from "../src/gcp";
import { GCPFirewallCache } from "../src/gcp-firewall-cache";
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

function client(env: Env): GCPClient {
  return (new GCPProvider(env) as unknown as { client: GCPClient }).client;
}

const config = () => leaseConfig({ provider: "gcp", sshPublicKey: "ssh-ed25519 fixture" });
const policy = {
  description: "Crabbox-managed SSH ingress",
  network: "projects/example-project/global/networks/default",
  direction: "INGRESS",
  sourceRanges: ["0.0.0.0/0"],
  targetTags: ["crabbox-ssh"],
  allowed: [{ IPProtocol: "tcp", ports: ["22", "2222"] }],
};

function fixture() {
  const requests: string[] = [];
  const compute = vi.fn<typeof fetch>(async (_input, _init) => Response.json(policy));
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input).includes("metadata.google.internal"))
      return Response.json(
        { access_token: "fixture", expires_in: 3600 },
        { headers: { "Metadata-Flavor": "Google" } },
      );
    requests.push(`${init?.method} ${new URL(String(input)).pathname}`);
    return compute(input, init);
  });
  return { requests, compute };
}

it("reuses an exact firewall verification across coordinator providers and zones", async () => {
  const env = environment();
  const { requests } = fixture();
  await client(env).ensureFirewall(config());
  await client(env).forScope("us-central1-b").ensureFirewall(config());
  expect(requests).toHaveLength(1);
});

it("expires five minutes after verification starts without extending on cache hits", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const env = environment();
  const { compute, requests } = fixture();
  compute.mockImplementationOnce(async () => {
    vi.setSystemTime(Date.now() + 4000);
    return Response.json(policy);
  });
  await client(env).ensureFirewall(config());
  vi.setSystemTime(Date.now() + 295_999);
  await client(env).ensureFirewall(config());
  expect(requests).toHaveLength(1);
  vi.setSystemTime(Date.now() + 1);
  await client(env).ensureFirewall(config());
  expect(requests).toHaveLength(2);
});

it.each([
  { gcpNetwork: "other-network" },
  { gcpSSHCIDRs: ["198.51.100.1/32"] },
  { gcpTags: ["other-tag"] },
  { sshPort: 2223 },
])("rechecks changed desired policy %j", async (change) => {
  const env = environment();
  const { compute } = fixture();
  await client(env).ensureFirewall(config());
  compute.mockResolvedValueOnce(Response.json({ error: "denied" }, { status: 403 }));
  await expect(client(env).ensureFirewall({ ...config(), ...change })).rejects.toThrow("403");
  expect(compute).toHaveBeenCalledTimes(2);
});

it("isolates projects and coordinator environments", async () => {
  const env = environment();
  const { compute } = fixture();
  await client(env).ensureFirewall(config());
  await client(environment()).ensureFirewall(config());
  compute.mockResolvedValueOnce(
    Response.json({ ...policy, network: "projects/other/global/networks/default" }),
  );
  await client(env).forScope(undefined, "other").ensureFirewall(config());
  expect(compute).toHaveBeenCalledTimes(3);
});

it.each(["GCP_CLIENT_EMAIL", "GCP_PRIVATE_KEY", "CRABBOX_GCP_CREDENTIAL_SOURCE"] as const)(
  "invalidates verification when credential generation changes: %s",
  async (field) => {
    const env = {
      ...environment(),
      GCP_CLIENT_EMAIL: "fixture@example.invalid",
      GCP_PRIVATE_KEY: "fixture-key",
    };
    const { compute } = fixture();
    await client(env).ensureFirewall(config());
    if (field === "CRABBOX_GCP_CREDENTIAL_SOURCE") {
      env[field] = "service-account-key";
      const next = client(env);
      // Synthetic service-account material is intentionally not a signing key.
      vi.spyOn(
        next as unknown as { accessToken(): Promise<string> },
        "accessToken",
      ).mockResolvedValue("fixture");
      await next.ensureFirewall(config());
    } else {
      env[field] += "-rotated";
      await client(env).ensureFirewall(config());
    }
    expect(compute).toHaveBeenCalledTimes(2);
  },
);

it.each([404, 403, 503, "network", "json"])(
  "invalidates on GCP instance request errors: %s",
  async (failure) => {
    const env = environment();
    const { compute } = fixture();
    await client(env).ensureFirewall(config());
    if (failure === "network") compute.mockRejectedValueOnce(new Error("network"));
    else if (failure === "json") compute.mockResolvedValueOnce(new Response("{"));
    else compute.mockResolvedValueOnce(new Response("error", { status: failure }));
    await expect(client(env).getServer("fixture-instance")).rejects.toThrow(/network|JSON|http/);
    await client(env).ensureFirewall(config());
    expect(compute).toHaveBeenCalledTimes(3);
  },
);

it("invalidates on asynchronous GCP operation errors", async () => {
  const env = environment();
  const { compute } = fixture();
  await client(env).ensureFirewall(config());
  compute.mockResolvedValueOnce(Response.json({ name: "delete-operation" }));
  compute.mockResolvedValueOnce(
    Response.json({ status: "DONE", error: { errors: [{ code: "NETWORK_NOT_FOUND" }] } }),
  );
  await expect(client(env).deleteServer("fixture-instance")).rejects.toThrow("NETWORK_NOT_FOUND");
  await client(env).ensureFirewall(config());
  expect(compute).toHaveBeenCalledTimes(4);
});

it("does not let an in-flight verification repopulate an invalidated cache", async () => {
  const env = environment();
  const { compute } = fixture();
  let finish!: (response: Response) => void;
  compute.mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = resolve;
      }),
  );
  const verifying = client(env).ensureFirewall(config());
  await vi.waitFor(() => expect(finish).toBeDefined());
  compute.mockRejectedValueOnce(new Error("network"));
  await expect(client(env).getServer("fixture-instance")).rejects.toThrow("network");
  finish(Response.json(policy));
  await verifying;
  await client(env).ensureFirewall(config());
  expect(compute).toHaveBeenCalledTimes(3);
});

it("reconciles drift on expiry and caches only a subsequent exact GET", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const env = environment();
  const { compute, requests } = fixture();
  await client(env).ensureFirewall(config());
  vi.setSystemTime(Date.now() + 300_000);
  compute.mockResolvedValueOnce(Response.json({ ...policy, disabled: true }));
  compute.mockResolvedValueOnce(Response.json({ name: "firewall-operation" }));
  compute.mockResolvedValueOnce(Response.json({ status: "DONE" }));
  await client(env).ensureFirewall(config());
  await client(env).ensureFirewall(config());
  await client(env).ensureFirewall(config());
  expect(requests.map((request) => request.split(" ")[0])).toEqual([
    "GET",
    "GET",
    "PUT",
    "POST",
    "GET",
  ]);
});

it("never caches unmanaged or failed firewall observations", async () => {
  const env = environment();
  const { compute } = fixture();
  compute.mockResolvedValueOnce(Response.json({ ...policy, description: "external" }));
  await expect(client(env).ensureFirewall(config())).rejects.toThrow("not Crabbox-managed");
  compute.mockResolvedValueOnce(new Response("denied", { status: 403 }));
  await expect(client(env).ensureFirewall(config())).rejects.toThrow("403");
  await client(env).ensureFirewall(config());
  await client(env).ensureFirewall(config());
  expect(compute).toHaveBeenCalledTimes(3);
});

it("requires a new verification when the same firewall name has a different policy", async () => {
  const cache = new GCPFirewallCache();
  const verify = vi.fn<() => Promise<boolean>>().mockResolvedValue(true);
  await cache.ensure("project/name", "policy-a", verify);
  await cache.ensure("project/name", "policy-b", verify);
  await cache.ensure("project/name", "policy-b", verify);
  await cache.ensure("project/name", "policy-a", verify);
  expect(verify).toHaveBeenCalledTimes(3);
});

it("invalidates a newer read when an older policy reconciliation completes", async () => {
  const cache = new GCPFirewallCache();
  let finishWrite!: (exact: boolean) => void;
  const writing = cache.ensure(
    "project/name",
    "policy-a",
    () =>
      new Promise((resolve) => {
        finishWrite = resolve;
      }),
  );
  const verify = vi.fn<() => Promise<boolean>>().mockResolvedValue(true);
  await cache.ensure("project/name", "policy-b", verify);
  finishWrite(false);
  await writing;
  await cache.ensure("project/name", "policy-b", verify);
  expect(verify).toHaveBeenCalledTimes(2);
});

it("bounds retained verifications and rechecks evicted entries", async () => {
  const cache = new GCPFirewallCache();
  const verify = vi.fn<() => Promise<boolean>>().mockResolvedValue(true);
  for (let index = 0; index < 129; index++) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- exercise eviction after completed verifications.
    await cache.ensure(`project/${index}`, "policy", verify);
  }
  await cache.ensure("project/128", "policy", verify);
  expect(verify).toHaveBeenCalledTimes(129);
  await cache.ensure("project/0", "policy", verify);
  expect(verify).toHaveBeenCalledTimes(130);
});

it("does not retain a verification that finishes outside the drift window", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const cache = new GCPFirewallCache();
  const verify = vi.fn<() => Promise<boolean>>().mockImplementation(async () => {
    vi.setSystemTime(Date.now() + 300_000);
    return true;
  });
  await cache.ensure("project/name", "policy", verify);
  await cache.ensure("project/name", "policy", verify);
  expect(verify).toHaveBeenCalledTimes(2);
});
