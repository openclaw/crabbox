/* oxlint-disable eslint/no-await-in-loop -- Fleet provisioning phases must advance sequentially. */
import { afterEach, describe, expect, it, vi } from "vitest";

import { leaseConfig } from "../src/config";
import { FleetCoordinator } from "../src/fleet";
import { KoyebClient, KoyebHTTPError, KoyebResumableProvisioning } from "../src/koyeb";
import {
  provisioningOperationKey,
  type LeaseProvisioningOperation,
} from "../src/lease-provisioning";
import type { ProvisioningStep } from "../src/provider-provisioning";
import { leaseProviderName } from "../src/slug";
import type { Env, LeaseRecord } from "../src/types";
import { ProvisioningTestRuntime, ProvisioningTestStorage } from "./provisioning-fixtures";

const serviceID = "11111111-1111-4111-8111-111111111111";
const deploymentID = "22222222-2222-4222-8222-222222222222";
const latestDeploymentID = "33333333-3333-4333-8333-333333333333";
const serviceName = leaseProviderName("cbx_abcdef123456", "blue-lobster");
const appName = "my-app";
const privateHost = `${serviceName}.${appName}.internal`;
const runnerImage = `ghcr.io/example/crabbox-koyeb-runner:source-${"b".repeat(40)}@sha256:${"a".repeat(64)}`;
const digestOnlyRunnerImage = `ghcr.io/example/crabbox-koyeb-runner@sha256:${"c".repeat(64)}`;
const registrySecret = "crabbox-koyeb-runner";
const tailscaleIPv4 = "100.64.12.34";
const tailscaleFQDN = "crabbox-blue-lobster.tail.example.ts.net";
const sshHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestHostKey crabbox-koyeb-sandbox";
const baseEnv: Env = {
  FLEET: {} as DurableObjectNamespace,
  HETZNER_TOKEN: "",
  KOYEB_API_TOKEN: "koyeb-api-token-value",
  CRABBOX_KOYEB_API_URL: "https://koyeb.example",
  CRABBOX_KOYEB_ORGANIZATION_ID: "33333333-3333-4333-8333-333333333333",
  CRABBOX_KOYEB_APP_ID: "44444444-4444-4444-8444-444444444444",
  CRABBOX_KOYEB_REGION: "was",
  CRABBOX_KOYEB_INSTANCE_TYPE: "large",
  CRABBOX_KOYEB_IMAGE: runnerImage,
  CRABBOX_KOYEB_REGISTRY_SECRET: registrySecret,
  KOYEB_APP_ID: "44444444-4444-4444-8444-444444444444",
  KOYEB_APP_NAME: appName,
  KOYEB_ORGANIZATION_ID: "33333333-3333-4333-8333-333333333333",
  KOYEB_REGION: "was",
} as Env;

const fleetEnv: Env = {
  ...baseEnv,
  CRABBOX_SESSION_SECRET: "synthetic-session-secret-with-32-characters",
  CRABBOX_DURABLE_PROVISIONING_ADMISSION: "true",
  CRABBOX_TAILSCALE_ENABLED: "1",
  CRABBOX_TAILSCALE_CLIENT_ID: "synthetic-tailscale-client",
  CRABBOX_TAILSCALE_CLIENT_SECRET: "synthetic-tailscale-secret",
};

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function lease(overrides: Partial<LeaseRecord> = {}): LeaseRecord {
  return {
    id: "cbx_abcdef123456",
    slug: "blue-lobster",
    provider: "koyeb",
    owner: "alice@example.com",
    providerOwner: "alice@example.com",
    org: "example-org",
    target: "linux",
    windowsMode: "normal",
    architecture: "amd64",
    os: "ubuntu:22.04",
    desktop: false,
    desktopEnv: "xfce",
    browser: true,
    code: false,
    profile: "default",
    class: "tiny",
    serverType: "large",
    requestedServerType: "large",
    cloudID: "",
    serverID: 0,
    serverName: "",
    host: "",
    providerKey: "",
    sshUser: "crabbox",
    sshPort: "22",
    sshFallbackPorts: [],
    workRoot: "/home/crabbox/crabbox",
    keep: false,
    ttlSeconds: 3_600,
    idleTimeoutSeconds: 600,
    estimatedHourlyUSD: 0,
    maxEstimatedUSD: 0,
    state: "provisioning",
    createdAt: "2026-09-08T12:00:00.000Z",
    updatedAt: "2026-09-08T12:00:00.000Z",
    lastTouchedAt: "2026-09-08T12:00:00.000Z",
    expiresAt: "2026-09-08T13:00:00.000Z",
    createAttemptGeneration: "generation-one",
    ...overrides,
  };
}

function config() {
  const result = leaseConfig({
    provider: "koyeb",
    sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey alice@example.com",
    browser: true,
    ttlSeconds: 3_600,
    idleTimeoutSeconds: 600,
  });
  result.tailscaleAuthKey = "tskey-auth-synthetic-one-off-key";
  result.tailscaleHostname = serviceName;
  result.tailscaleTags = ["tag:crabbox"];
  return result;
}

function meshConfig() {
  return leaseConfig({
    provider: "koyeb",
    sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey alice@example.com",
    browser: true,
    tailscale: false,
    ttlSeconds: 3_600,
    idleTimeoutSeconds: 600,
  });
}

function service(overrides: Record<string, unknown> = {}) {
  return {
    id: serviceID,
    name: serviceName,
    type: "SANDBOX",
    organization_id: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
    app_id: baseEnv.CRABBOX_KOYEB_APP_ID,
    status: "HEALTHY",
    active_deployment_id: deploymentID,
    latest_deployment_id: deploymentID,
    life_cycle: { delete_after_create: 3_600, delete_after_sleep: 600 },
    ...overrides,
  };
}

function deployment(providerSecret: string, overrides: Record<string, unknown> = {}) {
  return {
    id: deploymentID,
    organization_id: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
    app_id: baseEnv.CRABBOX_KOYEB_APP_ID,
    service_id: serviceID,
    status: "HEALTHY",
    definition: {
      name: serviceName,
      type: "SANDBOX",
      env: [
        { key: "SANDBOX_SECRET", value: providerSecret },
        { key: "CRABBOX_MANAGED_BY", value: "crabbox" },
        { key: "CRABBOX_PROVIDER", value: "koyeb" },
        { key: "CRABBOX_LEASE_ID", value: "cbx_abcdef123456" },
        { key: "CRABBOX_LEASE_SLUG", value: "blue-lobster" },
        { key: "CRABBOX_LEASE_OWNER", value: "alice_example.com" },
        { key: "CRABBOX_LEASE_ORG", value: "example-org" },
        { key: "CRABBOX_PROVISIONING_GENERATION", value: "generation-one" },
      ],
      scalings: [
        {
          min: 1,
          max: 1,
          targets: [],
        },
      ],
      regions: ["was"],
      instance_types: [{ type: "large" }],
      docker: { image: runnerImage, image_registry_secret: registrySecret },
      ports: [{ port: 3030, protocol: "http" }],
      routes: [
        {
          port: 3030,
          path: "/koyeb-sandbox/",
          security_policies: { api_keys: [providerSecret] },
        },
      ],
    },
    metadata: {
      sandbox: {
        public_url: "https://sandbox.example/",
        routing_key: "routing-key-one",
      },
    },
    ...overrides,
  };
}

function meshDeployment(providerSecret: string, overrides: Record<string, unknown> = {}) {
  const current = deployment(providerSecret);
  return {
    ...current,
    definition: {
      ...current.definition,
      mesh: "DEPLOYMENT_MESH_ENABLED",
      ports: [
        { port: 3030, protocol: "http" },
        { port: 22, protocol: "tcp" },
      ],
      routes: [],
    },
    metadata: { sandbox: {} },
    ...overrides,
  };
}

function advanceInput(
  prepared: Awaited<ReturnType<KoyebResumableProvisioning["prepare"]>>,
  step: ProvisioningStep,
  recovering: boolean,
) {
  return {
    plan: prepared.plan,
    step,
    lease: lease({ providerScope: prepared.plan.scope }),
    deadline: Date.now() + 60_000,
    recovering,
    canceled: false as const,
    material: prepared.material,
  };
}

function fleetRequest(method: string, path: string, body?: unknown): Request {
  return new Request(`https://coordinator.test${path}`, {
    method,
    headers: {
      "content-type": "application/json",
      prefer: "respond-async",
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": "example-org",
      "x-crabbox-admin": "true",
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

function fleetLeaseRequest() {
  return {
    leaseID: "cbx_abcdef123456",
    slug: "blue-lobster",
    provider: "koyeb",
    target: "linux",
    architecture: "amd64",
    sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey alice@example.com",
    browser: true,
    ttlSeconds: 3_600,
    idleTimeoutSeconds: 600,
  };
}

async function advanceFleetProvisioning(
  storage: ProvisioningTestStorage,
  runtime: ProvisioningTestRuntime,
): Promise<LeaseRecord> {
  for (let attempt = 0; attempt < 12; attempt += 1) {
    const current = await storage.get<LeaseRecord>("lease:cbx_abcdef123456");
    if (current?.state === "active") return current;
    const operation = await storage.get<LeaseProvisioningOperation>(
      provisioningOperationKey("cbx_abcdef123456"),
    );
    if (!operation) throw new Error("Koyeb provisioning operation was not admitted");
    vi.setSystemTime(Math.max(Date.now(), operation.step.nextWake + 1));
    await runtime.tick?.();
  }
  throw new Error("Koyeb provisioning did not publish an active lease");
}

describe("Koyeb Sandbox coordinator adapter", () => {
  it("rejects private mesh when the coordinator is not in the configured Koyeb app", async () => {
    const capability = new KoyebResumableProvisioning(
      { ...baseEnv, KOYEB_APP_ID: "55555555-5555-4555-8555-555555555555" },
      vi.fn<typeof fetch>(),
    );

    await expect(capability.prepare(meshConfig(), lease())).rejects.toThrow(
      "Koyeb durable provisioning configuration unsupported",
    );
  });

  it("freezes an unrouted Koyeb private-mesh service without Tailscale material", async () => {
    const requests: Request[] = [];
    const capability = new KoyebResumableProvisioning(baseEnv, async (request) => {
      const captured = request instanceof Request ? request.clone() : new Request(request);
      requests.push(captured);
      const url = new URL(captured.url);
      if (captured.method === "GET" && url.pathname === "/v1/services") {
        return Response.json({ services: [], has_next: false });
      }
      if (captured.method === "POST" && url.pathname === "/v1/services") {
        return Response.json({ service: service() });
      }
      throw new Error(`unexpected request ${captured.method} ${captured.url}`);
    });
    const prepared = await capability.prepare(meshConfig(), lease());

    expect(prepared.plan).toMatchObject({
      data: {
        transport: "koyeb-mesh",
        privateHost,
      },
    });
    expect(prepared.material.bootstrap).toBe("");

    await capability.advance(advanceInput(prepared, prepared.step, false));
    const create = (await requests[1]!.json()) as Record<string, any>;
    expect(create.definition).toMatchObject({
      mesh: "DEPLOYMENT_MESH_ENABLED",
      ports: [
        { port: 3030, protocol: "http" },
        { port: 22, protocol: "tcp" },
      ],
      routes: [],
    });
    expect(JSON.stringify(create)).not.toContain("TAILSCALE");
  });

  it("freezes a one-replica Sandbox payload with native TTLs and sealed-only credentials", async () => {
    const requests: Request[] = [];
    const capability = new KoyebResumableProvisioning(baseEnv, async (request) => {
      const captured = request instanceof Request ? request.clone() : new Request(request);
      requests.push(captured);
      const url = new URL(captured.url);
      if (captured.method === "GET" && url.pathname === "/v1/services") {
        return Response.json({ services: [], has_next: false });
      }
      if (captured.method === "POST" && url.pathname === "/v1/services") {
        return Response.json({ service: service() });
      }
      throw new Error(`unexpected request ${captured.method} ${captured.url}`);
    });
    const prepared = await capability.prepare(config(), lease());

    expect(prepared.plan).toMatchObject({
      provider: "koyeb",
      scope: expect.stringMatching(/^koyeb:context:v1:[a-f0-9]{64}$/),
      data: { ttlSeconds: 3_600, idleTimeoutSeconds: 600 },
      resources: [
        {
          cloudID: serviceName,
          region: "was",
          scope: expect.stringMatching(/^koyeb:context:v1:[a-f0-9]{64}$/),
        },
      ],
    });
    expect(prepared.material.providerSecret).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(prepared.material.adminPassword).not.toBe(prepared.material.providerSecret);
    expect(JSON.stringify(prepared.plan)).not.toContain(prepared.material.providerSecret);
    expect(JSON.stringify(prepared.step)).not.toContain(prepared.material.providerSecret);

    const changedLeaseInput = advanceInput(prepared, prepared.step, false);
    changedLeaseInput.lease.ttlSeconds = 7_200;
    changedLeaseInput.lease.idleTimeoutSeconds = 1_200;
    const observed = await capability.advance(changedLeaseInput);
    expect(observed).toMatchObject({
      phase: "provisioning",
      state: { version: 1, action: "observe", serviceID },
    });
    expect(requests.map((request) => `${request.method} ${new URL(request.url).pathname}`)).toEqual(
      ["GET /v1/services", "POST /v1/services"],
    );
    const create = (await requests[1]!.json()) as Record<string, any>;
    expect(create).toMatchObject({
      app_id: baseEnv.CRABBOX_KOYEB_APP_ID,
      life_cycle: { delete_after_create: 3_600, delete_after_sleep: 600 },
      definition: {
        name: serviceName,
        type: "SANDBOX",
        docker: { image: runnerImage, image_registry_secret: registrySecret },
        regions: ["was"],
        instance_types: [{ type: "large" }],
        ports: [{ port: 3030, protocol: "http" }],
        routes: [
          {
            port: 3030,
            path: "/koyeb-sandbox/",
            security_policies: { api_keys: [prepared.material.providerSecret] },
          },
        ],
        scalings: [
          {
            min: 1,
            max: 1,
            targets: [],
          },
        ],
      },
    });
    expect(create.definition.env).toContainEqual({
      key: "SANDBOX_SECRET",
      value: prepared.material.providerSecret,
    });
    expect(create.definition.env).toContainEqual({
      key: "CRABBOX_PROVISIONING_GENERATION",
      value: "generation-one",
    });
    expect(JSON.stringify(create)).not.toContain(config().tailscaleAuthKey);
    expect(JSON.stringify(create)).not.toContain("3031");
    expect(create.definition).not.toHaveProperty("proxy_ports");
    expect(requests[1]!.headers.get("authorization")).toBe("Bearer koyeb-api-token-value");
  });

  it("resumes Tailscale plans created before the transport discriminator existed", async () => {
    const requests: Request[] = [];
    const capability = new KoyebResumableProvisioning(baseEnv, async (request) => {
      const captured = request instanceof Request ? request.clone() : new Request(request);
      requests.push(captured);
      const url = new URL(captured.url);
      if (captured.method === "GET" && url.pathname === "/v1/services") {
        return Response.json({ services: [], has_next: false });
      }
      if (captured.method === "POST" && url.pathname === "/v1/services") {
        return Response.json({ service: service() });
      }
      throw new Error(`unexpected request ${captured.method} ${captured.url}`);
    });
    const prepared = await capability.prepare(config(), lease());
    const legacyPlan = structuredClone(prepared.plan);
    delete (legacyPlan.data as Record<string, unknown>).transport;

    await expect(
      capability.advance(advanceInput({ ...prepared, plan: legacyPlan }, prepared.step, false)),
    ).resolves.toMatchObject({
      phase: "provisioning",
      state: { action: "observe", serviceID },
    });
    const create = (await requests[1]!.json()) as Record<string, any>;
    expect(create.definition).not.toHaveProperty("mesh");
    expect(create.definition.routes).toHaveLength(1);
  });

  it("uses the frozen lease image identity for active release after coordinator config changes", async () => {
    let deleted = false;
    const rotatedImage = `ghcr.io/example/crabbox-koyeb-runner@sha256:${"b".repeat(64)}`;
    const rotatedRegistrySecret = "rotated-koyeb-runner";
    const client = new KoyebClient(
      {
        ...baseEnv,
        CRABBOX_KOYEB_IMAGE: rotatedImage,
        CRABBOX_KOYEB_REGISTRY_SECRET: rotatedRegistrySecret,
      },
      async (request) => {
        const incoming = request instanceof Request ? request : new Request(request);
        const url = new URL(incoming.url);
        if (incoming.method === "DELETE" && url.pathname === `/v1/services/${serviceID}`) {
          deleted = true;
          return Response.json({ service: service({ status: "DELETING" }) });
        }
        if (url.pathname === `/v1/services/${serviceID}`) {
          return deleted
            ? new Response("not found", { status: 404 })
            : Response.json({ service: service() });
        }
        if (url.pathname === `/v1/deployments/${deploymentID}`) {
          return Response.json({ deployment: deployment("u".repeat(32)) });
        }
        throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
      },
    );
    const providerScope = await new KoyebClient(baseEnv, vi.fn<typeof fetch>()).providerScope();
    const active = lease({
      state: "active",
      cloudID: serviceID,
      region: "was",
      providerScope,
      image: {
        id: runnerImage,
        source: "explicit",
        provider: "koyeb",
        kind: "koyeb-sandbox-runner",
        region: "was",
        sourceID: registrySecret,
      },
    });

    await expect(client.deleteOwnedService(active)).resolves.toBeUndefined();
    expect(deleted).toBe(true);
  });

  it("discovers an exact owned service after a lost create acknowledgement without reposting", async () => {
    let created = false;
    let posts = 0;
    let providerSecret = "";
    const fetcher = vi.fn<typeof fetch>(async (request) => {
      const incoming = request instanceof Request ? request : new Request(request);
      const url = new URL(incoming.url);
      if (incoming.method === "GET" && url.pathname === "/v1/services") {
        return Response.json({ services: created ? [service()] : [], has_next: false });
      }
      if (incoming.method === "POST" && url.pathname === "/v1/services") {
        posts += 1;
        providerSecret = ((await incoming.json()) as any).definition.env.find(
          (entry: any) => entry.key === "SANDBOX_SECRET",
        ).value;
        created = true;
        throw new TypeError("response lost after provider acceptance");
      }
      if (incoming.method === "GET" && url.pathname === `/v1/services/${serviceID}`) {
        return Response.json({ service: service() });
      }
      if (incoming.method === "GET" && url.pathname === `/v1/deployments/${deploymentID}`) {
        return Response.json({ deployment: deployment(providerSecret) });
      }
      throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
    });
    let capability = new KoyebResumableProvisioning(baseEnv, fetcher);
    const prepared = await capability.prepare(config(), lease());
    const uncertain = await capability.advance(advanceInput(prepared, prepared.step, false));
    expect(uncertain).toMatchObject({
      phase: "provisioning",
      state: { action: "discover", outcomeUncertain: true },
    });

    capability = new KoyebResumableProvisioning(baseEnv, fetcher);
    const recovered = await capability.advance(advanceInput(prepared, uncertain, true));
    expect(recovered).toMatchObject({
      phase: "provisioning",
      state: { action: "observe", serviceID },
    });
    expect(posts).toBe(1);
  });

  it("accepts semantically empty Koyeb response normalization while recovering", async () => {
    let providerSecret = "";
    const fetcher = vi.fn<typeof fetch>(async (request) => {
      const incoming = request instanceof Request ? request : new Request(request);
      const url = new URL(incoming.url);
      if (incoming.method === "GET" && url.pathname === "/v1/services") {
        return Response.json({ services: [service()], has_next: false });
      }
      if (incoming.method === "GET" && url.pathname === `/v1/deployments/${deploymentID}`) {
        const normalized = deployment(providerSecret);
        const definition = normalized.definition as Record<string, any>;
        definition.docker = {
          ...definition.docker,
          command: "",
          args: [],
          entrypoint: [],
          privileged: false,
        };
        definition.env = definition.env.map((entry: Record<string, unknown>) => ({
          ...entry,
          scopes: ["region:was"],
          secret: "",
        }));
        definition.instance_types[0].scopes = ["region:was"];
        definition.scalings[0].scopes = ["region:was"];
        definition.routes[0].security_policies = {
          basic_auths: [],
          api_keys: [providerSecret],
        };
        definition.proxy_ports = [];
        return Response.json({ deployment: normalized });
      }
      throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
    });
    const capability = new KoyebResumableProvisioning(baseEnv, fetcher);
    const prepared = await capability.prepare(config(), lease());
    providerSecret = prepared.material.providerSecret;
    const uncertain: ProvisioningStep = {
      ...prepared.step,
      phase: "provisioning",
      state: { version: 1, action: "discover", outcomeUncertain: true },
    };

    const recovered = await capability.advance(advanceInput(prepared, uncertain, true));

    expect(recovered).toMatchObject({
      phase: "provisioning",
      state: { action: "observe", serviceID },
    });
  });

  it.each([
    {
      drift: "region",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.regions = ["fra"];
      },
    },
    {
      drift: "instance type",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.instance_types = [{ type: "medium" }];
      },
    },
    {
      drift: "port protocol",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.ports = [{ port: 3030, protocol: "tcp" }];
      },
    },
    {
      drift: "minimum scale",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.scalings[0].min = 0;
      },
    },
    {
      drift: "maximum scale",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.scalings[0].max = 2;
      },
    },
    {
      drift: "deep-sleep target",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.scalings[0].targets = [{ sleep_idle_delay: { deep_sleep_value: 600 } }];
      },
    },
    {
      drift: "create lifecycle",
      mutate: (foundService: Record<string, any>) => {
        foundService.life_cycle.delete_after_create = 7_200;
      },
    },
    {
      drift: "sleep lifecycle",
      mutate: (foundService: Record<string, any>) => {
        foundService.life_cycle.delete_after_sleep = 1_200;
      },
    },
    {
      drift: "extra lifecycle field",
      mutate: (foundService: Record<string, any>) => {
        foundService.life_cycle.unexpected = true;
      },
    },
    {
      drift: "Docker command",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.docker.command = "sleep infinity";
      },
    },
    {
      drift: "Docker arguments",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.docker.args = ["--unsafe"];
      },
    },
    {
      drift: "Docker entrypoint",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.docker.entrypoint = ["/bin/sh"];
      },
    },
    {
      drift: "Docker privileged mode",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.docker.privileged = true;
      },
    },
    {
      drift: "route security policy",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.routes[0].security_policies = { api_keys: ["unexpected"] };
      },
    },
    {
      drift: "proxy port",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.proxy_ports = [{ port: 22, protocol: "tcp" }];
      },
    },
    {
      drift: "extra environment variable",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.env.push({
          key: "CRABBOX_KOYEB_TAILSCALE_LOGIN_SERVER",
          value: "https://unexpected.example",
        });
      },
    },
    {
      drift: "duplicate environment variable",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.env[definition.env.length - 1] = {
          key: "CRABBOX_LEASE_ID",
          value: "cbx_abcdef123456",
        };
      },
    },
    {
      drift: "sandbox secret",
      mutate: (_service: Record<string, any>, definition: Record<string, any>) => {
        definition.env.find(
          (entry: Record<string, string>) => entry.key === "SANDBOX_SECRET",
        ).value = "x".repeat(32);
      },
    },
  ])("rejects $drift drift while recovering an uncertain create", async ({ mutate }) => {
    let providerSecret = "";
    const fetcher = vi.fn<typeof fetch>(async (request) => {
      const incoming = request instanceof Request ? request : new Request(request);
      const url = new URL(incoming.url);
      const foundService = service();
      const foundDeployment = deployment(providerSecret);
      mutate(foundService, foundDeployment.definition);
      if (incoming.method === "GET" && url.pathname === "/v1/services") {
        return Response.json({ services: [foundService], has_next: false });
      }
      if (incoming.method === "GET" && url.pathname === `/v1/deployments/${deploymentID}`) {
        return Response.json({ deployment: foundDeployment });
      }
      throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
    });
    const capability = new KoyebResumableProvisioning(baseEnv, fetcher);
    const prepared = await capability.prepare(config(), lease());
    providerSecret = prepared.material.providerSecret;
    const uncertain: ProvisioningStep = {
      ...prepared.step,
      phase: "provisioning",
      state: { version: 1, action: "discover", outcomeUncertain: true },
    };

    const recovered = await capability.advance(advanceInput(prepared, uncertain, true));

    expect(recovered).toMatchObject({
      phase: "blocked",
      blockedReason: "identity_resolution_required",
    });
    expect(fetcher).toHaveBeenCalled();
    expect(
      fetcher.mock.calls.every(([request]) =>
        (request instanceof Request ? request : new Request(request)).method.startsWith("GET"),
      ),
    ).toBe(true);
  });

  it("never blindly posts when a persisted dispatch intent is recovered without a service", async () => {
    const fetcher = vi.fn<typeof fetch>(async () =>
      Response.json({ services: [], has_next: false }),
    );
    const capability = new KoyebResumableProvisioning(baseEnv, fetcher);
    const prepared = await capability.prepare(config(), lease());

    const result = await capability.advance(advanceInput(prepared, prepared.step, true));

    expect(result).toMatchObject({
      phase: "blocked",
      blockedReason: "dispatch_outcome_unresolved",
    });
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it("bootstraps through the exact authenticated management URL and publishes only Tailscale SSH", async () => {
    const managementRequests: Request[] = [];
    const capability = new KoyebResumableProvisioning(baseEnv, async (request) => {
      const incoming = request instanceof Request ? request.clone() : new Request(request);
      const url = new URL(incoming.url);
      if (url.origin === "https://koyeb.example") {
        if (url.pathname === `/v1/services/${serviceID}`) {
          return Response.json({ service: service() });
        }
        if (url.pathname === `/v1/deployments/${deploymentID}`) {
          return Response.json({ deployment: deployment(material.providerSecret!) });
        }
      }
      managementRequests.push(incoming);
      if (incoming.method === "GET" && url.pathname === "/koyeb-sandbox/health") {
        return Response.json({ ok: true });
      }
      if (incoming.method === "POST" && url.pathname === "/koyeb-sandbox/write_file") {
        return Response.json({ ok: true });
      }
      if (incoming.method === "POST" && url.pathname === "/koyeb-sandbox/run") {
        return Response.json({
          stdout: JSON.stringify({
            schema: "crabbox-koyeb-sandbox-runner/v1",
            leaseId: serviceName,
            ssh: { user: "crabbox", host: tailscaleIPv4, port: 22, hostKey: sshHostKey },
            tailscale: { ipv4: tailscaleIPv4, dnsName: tailscaleFQDN },
            desktop: {
              display: ":99",
              vncHost: "127.0.0.1",
              vncPort: 5900,
              browser: "/usr/local/bin/crabbox-browser",
              terminal: "xfce4-terminal",
            },
          }),
          stderr: "",
          code: 0,
        });
      }
      throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
    });
    const prepared = await capability.prepare(config(), lease());
    const material = prepared.material;
    const observeStep: ProvisioningStep = {
      ...prepared.step,
      phase: "provisioning",
      state: { version: 1, action: "observe", serviceID },
    };
    const bootstrap = await capability.advance(advanceInput(prepared, observeStep, true));
    expect(bootstrap).toMatchObject({ state: { action: "bootstrap", serviceID, deploymentID } });

    const published = await capability.advance(advanceInput(prepared, bootstrap, true));
    expect(published).toMatchObject({
      phase: "ready-to-publish",
      publication: {
        server: {
          provider: "koyeb",
          cloudID: serviceID,
          host: tailscaleFQDN,
        },
        access: {
          sshUser: "crabbox",
          sshPort: "22",
          sshFallbackPorts: [],
          workRoot: "/workspace/crabbox",
          sshHostKey: sshHostKey.split(" ", 2).join(" "),
          tailscale: {
            enabled: true,
            hostname: serviceName,
            fqdn: tailscaleFQDN,
            ipv4: tailscaleIPv4,
            tags: ["tag:crabbox"],
            state: "ready",
          },
        },
      },
    });
    expect(managementRequests.map((request) => new URL(request.url).pathname)).toEqual([
      "/koyeb-sandbox/health",
      "/koyeb-sandbox/write_file",
      "/koyeb-sandbox/run",
    ]);
    for (const request of managementRequests) {
      expect(request.headers.get("authorization")).toBe(`Bearer ${material.providerSecret}`);
      expect(request.headers.get("x-routing-key")).toBe("routing-key-one");
    }
    const write = (await managementRequests[1]!.json()) as Record<string, string>;
    expect(write.path).toBe("/run/crabbox-authorized-key.pub");
    expect(write.content).toBe(config().sshPublicKey);
    expect(write.content).not.toContain(material.providerSecret);
    const run = (await managementRequests[2]!.json()) as Record<string, any>;
    expect(run).toMatchObject({
      cmd: "/usr/local/bin/crabbox-koyeb-bootstrap",
      cwd: "/workspace/crabbox",
      env: {
        CRABBOX_KOYEB_LEASE_ID: serviceName,
        CRABBOX_KOYEB_SSH_PUBLIC_KEY_FILE: "/run/crabbox-authorized-key.pub",
        CRABBOX_KOYEB_TAILSCALE_AUTH_KEY: config().tailscaleAuthKey,
        CRABBOX_KOYEB_TAILSCALE_HOSTNAME: serviceName,
        CRABBOX_KOYEB_TAILSCALE_TAGS: "tag:crabbox",
      },
    });
    expect(JSON.stringify(published)).not.toContain(material.providerSecret);
    expect(JSON.stringify(published)).not.toContain("routing-key-one");
    expect(JSON.stringify(published)).not.toContain("sandbox.example");
    expect(JSON.stringify(published)).not.toContain(config().tailscaleAuthKey);
  });

  it("bootstraps privately over the Koyeb mesh and publishes direct key-only SSH", async () => {
    const managementRequests: Request[] = [];
    const capability = new KoyebResumableProvisioning(baseEnv, async (request) => {
      const incoming = request instanceof Request ? request.clone() : new Request(request);
      const url = new URL(incoming.url);
      if (url.origin === "https://koyeb.example") {
        if (url.pathname === `/v1/services/${serviceID}`) {
          return Response.json({ service: service() });
        }
        if (url.pathname === `/v1/deployments/${deploymentID}`) {
          return Response.json({ deployment: meshDeployment(material.providerSecret!) });
        }
      }
      managementRequests.push(incoming);
      if (incoming.method === "GET" && url.pathname === "/koyeb-sandbox/health") {
        return Response.json({ ok: true });
      }
      if (incoming.method === "POST" && url.pathname === "/koyeb-sandbox/write_file") {
        return Response.json({ ok: true });
      }
      if (incoming.method === "POST" && url.pathname === "/koyeb-sandbox/run") {
        return Response.json({
          stdout: JSON.stringify({
            schema: "crabbox-koyeb-sandbox-runner/v2",
            leaseId: serviceName,
            ssh: { user: "crabbox", host: privateHost, port: 22, hostKey: sshHostKey },
            network: { transport: "koyeb-mesh", privateHost },
            desktop: {
              display: ":99",
              vncHost: "127.0.0.1",
              vncPort: 5900,
              browser: "/usr/local/bin/crabbox-browser",
              terminal: "xfce4-terminal",
            },
          }),
          stderr: "",
          code: 0,
        });
      }
      throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
    });
    const prepared = await capability.prepare(meshConfig(), lease());
    const material = prepared.material;
    const observeStep: ProvisioningStep = {
      ...prepared.step,
      phase: "provisioning",
      state: { version: 1, action: "observe", serviceID },
    };
    const bootstrap = await capability.advance(advanceInput(prepared, observeStep, true));
    expect(bootstrap).toMatchObject({ state: { action: "bootstrap", serviceID, deploymentID } });

    const published = await capability.advance(advanceInput(prepared, bootstrap, true));
    expect(published).toMatchObject({
      phase: "ready-to-publish",
      publication: {
        server: {
          provider: "koyeb",
          cloudID: serviceID,
          host: privateHost,
          labels: { koyeb_network: "mesh" },
        },
        access: {
          sshUser: "crabbox",
          sshPort: "22",
          sshFallbackPorts: [],
          workRoot: "/workspace/crabbox",
          sshHostKey: sshHostKey.split(" ", 2).join(" "),
        },
      },
    });
    expect(published.publication?.access).not.toHaveProperty("tailscale");
    expect(managementRequests.map((request) => new URL(request.url).origin)).toEqual([
      `http://${privateHost}:3030`,
      `http://${privateHost}:3030`,
      `http://${privateHost}:3030`,
    ]);
    for (const request of managementRequests) {
      expect(request.headers.get("authorization")).toBe(`Bearer ${material.providerSecret}`);
      expect(request.headers.has("x-routing-key")).toBe(false);
    }
    const run = (await managementRequests[2]!.json()) as Record<string, any>;
    expect(run).toMatchObject({
      cmd: "/usr/local/bin/crabbox-koyeb-bootstrap",
      cwd: "/workspace/crabbox",
      env: {
        CRABBOX_KOYEB_LEASE_ID: serviceName,
        CRABBOX_KOYEB_NETWORK: "koyeb-mesh",
        CRABBOX_KOYEB_PRIVATE_HOST: privateHost,
        CRABBOX_KOYEB_SSH_PUBLIC_KEY_FILE: "/run/crabbox-authorized-key.pub",
      },
    });
    expect(JSON.stringify(run)).not.toContain("TAILSCALE");
    expect(JSON.stringify(published)).not.toContain(material.providerSecret);
  });

  it("requires exact deployment ownership before deleting and confirms provider absence", async () => {
    const methods: string[] = [];
    let deleted = false;
    const capability = new KoyebResumableProvisioning(baseEnv, async (request) => {
      const incoming = request instanceof Request ? request : new Request(request);
      const url = new URL(incoming.url);
      methods.push(`${incoming.method} ${url.pathname}`);
      if (incoming.method === "DELETE" && url.pathname === `/v1/services/${serviceID}`) {
        deleted = true;
        return Response.json({ service: service({ status: "DELETING" }) });
      }
      if (url.pathname === `/v1/services/${serviceID}`) {
        return deleted
          ? new Response("not found", { status: 404 })
          : Response.json({ service: service() });
      }
      if (url.pathname === `/v1/deployments/${deploymentID}`) {
        return Response.json({ deployment: deployment("u".repeat(32)) });
      }
      throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
    });
    const prepared = await capability.prepare(config(), lease());
    const cleanupInput = (step: ProvisioningStep) => ({
      plan: prepared.plan,
      step,
      lease: lease({ providerScope: prepared.plan.scope, cloudID: serviceID }),
      deadline: Date.now() + 60_000,
      recovering: true,
      canceled: true as const,
    });
    let step: ProvisioningStep = {
      ...prepared.step,
      phase: "cleanup",
      state: { version: 1, action: "observe", serviceID },
    };
    step = await capability.advance(cleanupInput(step));
    expect(step).toMatchObject({ phase: "cleanup", state: { action: "delete", serviceID } });
    expect(methods).not.toContain(`DELETE /v1/services/${serviceID}`);
    step = await capability.advance(cleanupInput(step));
    expect(step).toMatchObject({
      phase: "cleanup",
      state: { action: "confirm-delete", serviceID },
    });
    step = await capability.advance(cleanupInput(step));
    expect(step.phase).toBe("terminal");
    expect(methods.filter((entry) => entry.startsWith("DELETE"))).toEqual([
      `DELETE /v1/services/${serviceID}`,
    ]);
  });

  it("waits without deleting when active and latest deployment identities differ", async () => {
    const methods: string[] = [];
    const capability = new KoyebResumableProvisioning(baseEnv, async (request) => {
      const incoming = request instanceof Request ? request : new Request(request);
      const url = new URL(incoming.url);
      methods.push(`${incoming.method} ${url.pathname}`);
      if (url.pathname === `/v1/services/${serviceID}`) {
        return Response.json({
          service: service({ latest_deployment_id: latestDeploymentID }),
        });
      }
      throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
    });
    const prepared = await capability.prepare(config(), lease());

    const result = await capability.advance({
      plan: prepared.plan,
      step: {
        ...prepared.step,
        phase: "cleanup",
        state: { version: 1, action: "observe", serviceID },
      },
      lease: lease({ providerScope: prepared.plan.scope, cloudID: serviceID }),
      deadline: Date.now() + 60_000,
      recovering: true,
      canceled: true,
    });

    expect(result).toMatchObject({
      phase: "cleanup",
      state: { action: "observe", serviceID },
    });
    expect(methods).toEqual([`GET /v1/services/${serviceID}`]);
    expect(methods.some((entry) => entry.startsWith("DELETE"))).toBe(false);
  });

  it("fails closed on ownership mismatch without issuing a delete", async () => {
    const methods: string[] = [];
    const capability = new KoyebResumableProvisioning(baseEnv, async (request) => {
      const incoming = request instanceof Request ? request : new Request(request);
      const url = new URL(incoming.url);
      methods.push(incoming.method);
      if (url.pathname === `/v1/services/${serviceID}`) {
        return Response.json({ service: service() });
      }
      if (url.pathname === `/v1/deployments/${deploymentID}`) {
        return Response.json({
          deployment: deployment("unused", {
            definition: {
              ...deployment("unused").definition,
              env: [{ key: "CRABBOX_LEASE_ID", value: "cbx_ffffffffffff" }],
            },
          }),
        });
      }
      throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
    });
    const prepared = await capability.prepare(config(), lease());
    const result = await capability.advance({
      plan: prepared.plan,
      step: {
        ...prepared.step,
        phase: "cleanup",
        state: { version: 1, action: "observe", serviceID },
      },
      lease: lease({ providerScope: prepared.plan.scope, cloudID: serviceID }),
      deadline: Date.now() + 60_000,
      recovering: true,
      canceled: true,
    });

    expect(result).toMatchObject({
      phase: "blocked",
      blockedReason: "identity_resolution_required",
    });
    expect(methods).not.toContain("DELETE");
  });

  it("redacts API and sandbox secrets from failed control-plane and management responses", async () => {
    const sandboxSecret = "synthetic-sandbox-secret-value-xx";
    const tailscaleAuthKey = "tskey-auth-synthetic-one-off-key";
    const client = new KoyebClient(baseEnv, async (request) => {
      const incoming = request instanceof Request ? request : new Request(request);
      return new Response(
        `api=${baseEnv.KOYEB_API_TOKEN} sandbox=${sandboxSecret} routing=routing-key-one tailscale=${tailscaleAuthKey} path=${incoming.url}`,
        { status: 500 },
      );
    });

    const apiError = await client.getService(serviceID).catch((error: unknown) => error);
    expect(apiError).toBeInstanceOf(KoyebHTTPError);
    expect(String(apiError)).not.toContain(baseEnv.KOYEB_API_TOKEN);

    const managementError = await client
      .managementHealth("https://sandbox.example", "routing-key-one", sandboxSecret)
      .catch((error: unknown) => error);
    expect(managementError).toBeInstanceOf(KoyebHTTPError);
    expect(String(managementError)).not.toContain(sandboxSecret);
    expect(String(managementError)).not.toContain(baseEnv.KOYEB_API_TOKEN);
    expect(String(managementError)).not.toContain("routing-key-one");

    const runError = await client
      .managementRun("https://sandbox.example", "routing-key-one", sandboxSecret, { cmd: "true" }, [
        tailscaleAuthKey,
      ])
      .catch((error: unknown) => error);
    expect(runError).toBeInstanceOf(KoyebHTTPError);
    expect(String(runError)).not.toContain(tailscaleAuthKey);
  });
});

describe("Koyeb Fleet integration", () => {
  it("uses the configured instance type for readiness, admission, and publication", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const runtime = new ProvisioningTestRuntime(storage);
    let createdDefinition: Record<string, unknown> | undefined;
    const requests: Request[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (requestInput: RequestInfo | URL, init?: RequestInit) => {
        const incoming =
          requestInput instanceof Request ? requestInput.clone() : new Request(requestInput, init);
        requests.push(incoming.clone());
        const url = new URL(incoming.url);
        if (url.pathname === "/api/v2/oauth/token") {
          return Response.json({ access_token: "synthetic-tailscale-access-token" });
        }
        if (url.pathname.endsWith("/keys")) {
          return Response.json({ key: "tskey-auth-synthetic-one-off-key" });
        }
        if (incoming.method === "GET" && url.pathname === "/v1/services") {
          return Response.json({ services: createdDefinition ? [service()] : [], has_next: false });
        }
        if (incoming.method === "POST" && url.pathname === "/v1/services") {
          const body = (await incoming.json()) as { definition: Record<string, unknown> };
          createdDefinition = body.definition;
          return Response.json({ service: service() });
        }
        if (url.pathname === `/v1/services/${serviceID}`) {
          return Response.json({ service: service() });
        }
        if (url.pathname === `/v1/deployments/${deploymentID}`) {
          return Response.json({
            deployment: {
              ...deployment("unused"),
              definition: createdDefinition,
            },
          });
        }
        if (incoming.method === "GET" && url.pathname === "/koyeb-sandbox/health") {
          return Response.json({ ok: true });
        }
        if (incoming.method === "POST" && url.pathname === "/koyeb-sandbox/write_file") {
          return Response.json({ ok: true });
        }
        if (incoming.method === "POST" && url.pathname === "/koyeb-sandbox/run") {
          return Response.json({
            stdout: JSON.stringify({
              schema: "crabbox-koyeb-sandbox-runner/v1",
              leaseId: serviceName,
              ssh: { user: "crabbox", host: tailscaleIPv4, port: 22, hostKey: sshHostKey },
              tailscale: { ipv4: tailscaleIPv4, dnsName: tailscaleFQDN },
            }),
            stderr: "",
            code: 0,
          });
        }
        throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
      }),
    );
    const coordinator = new FleetCoordinator(runtime, {
      ...fleetEnv,
      CRABBOX_KOYEB_INSTANCE_TYPE: "medium",
    });

    const readiness = await coordinator.fetch(fleetRequest("GET", "/v1/providers/koyeb/readiness"));
    expect(readiness.status).toBe(200);
    await expect(readiness.json()).resolves.toMatchObject({
      provider: "koyeb",
      configured: true,
      resumableProvisioning: {
        supported: true,
        available: true,
      },
    });

    const admitted = await coordinator.fetch(
      fleetRequest("POST", "/v1/leases", fleetLeaseRequest()),
    );
    expect(admitted.status).toBe(202);
    const active = await advanceFleetProvisioning(storage, runtime);

    expect(active).toMatchObject({
      provider: "koyeb",
      state: "active",
      serverType: "medium",
      requestedServerType: "medium",
      cloudID: serviceID,
      host: tailscaleFQDN,
      sshUser: "crabbox",
      sshPort: "22",
      sshFallbackPorts: [],
      workRoot: "/workspace/crabbox",
      sshHostKey: sshHostKey.split(" ", 2).join(" "),
      tailscale: {
        enabled: true,
        hostname: "crabbox-blue-lobster",
        fqdn: tailscaleFQDN,
        ipv4: tailscaleIPv4,
        tags: ["tag:crabbox"],
        state: "ready",
      },
      image: {
        id: runnerImage,
        source: "explicit",
        provider: "koyeb",
        kind: "koyeb-sandbox-runner",
        region: "was",
        sourceID: registrySecret,
      },
    });
    expect(createdDefinition).toMatchObject({ instance_types: [{ type: "medium" }] });
    expect(
      requests.filter(
        (request) => request.method === "POST" && new URL(request.url).pathname === "/v1/services",
      ),
    ).toHaveLength(1);
  });

  it("refuses the legacy allocation path when durable admission is disabled", async () => {
    const storage = new ProvisioningTestStorage();
    const runtime = new ProvisioningTestRuntime(storage);
    const fetcher = vi.fn<typeof fetch>();
    vi.stubGlobal("fetch", fetcher);
    const coordinator = new FleetCoordinator(runtime, {
      ...fleetEnv,
      CRABBOX_DURABLE_PROVISIONING_ADMISSION: "false",
    });

    const response = await coordinator.fetch(
      fleetRequest("POST", "/v1/leases", fleetLeaseRequest()),
    );

    expect(response.status).toBe(424);
    await expect(response.json()).resolves.toMatchObject({
      error: "durable_provisioning_required",
      provider: "koyeb",
    });
    expect(fetcher).not.toHaveBeenCalled();
    expect(await storage.get("lease:cbx_abcdef123456")).toBeUndefined();
  });

  it("reports missing Koyeb image and durable material while accepting native mesh", async () => {
    const storage = new ProvisioningTestStorage();
    const runtime = new ProvisioningTestRuntime(storage);
    const { CRABBOX_KOYEB_IMAGE: _image, ...withoutImage } = baseEnv;
    const missingImage = await new FleetCoordinator(runtime, withoutImage).fetch(
      fleetRequest("GET", "/v1/providers/koyeb/readiness"),
    );
    await expect(missingImage.json()).resolves.toMatchObject({
      provider: "koyeb",
      configured: false,
      missing: ["CRABBOX_KOYEB_IMAGE"],
    });

    const invalidRegistrySecret = await new FleetCoordinator(runtime, {
      ...baseEnv,
      CRABBOX_KOYEB_REGISTRY_SECRET: "Not a valid Koyeb secret name",
    }).fetch(fleetRequest("GET", "/v1/providers/koyeb/readiness"));
    await expect(invalidRegistrySecret.json()).resolves.toMatchObject({
      provider: "koyeb",
      configured: false,
      missing: ["CRABBOX_KOYEB_REGISTRY_SECRET"],
    });

    const incompleteRuntime = await new FleetCoordinator(runtime, {
      ...baseEnv,
      CRABBOX_TAILSCALE_ENABLED: "1",
      CRABBOX_DURABLE_PROVISIONING_ADMISSION: "false",
    }).fetch(fleetRequest("GET", "/v1/providers/koyeb/readiness"));
    const payload = (await incompleteRuntime.json()) as Record<string, any>;
    expect(payload).toMatchObject({
      provider: "koyeb",
      configured: true,
      missing: [],
      resumableProvisioning: {
        version: 1,
        supported: true,
        admissionEnabled: false,
        available: false,
        missing: expect.arrayContaining([
          "CRABBOX_DURABLE_PROVISIONING_ADMISSION",
          "CRABBOX_SESSION_SECRET",
        ]),
      },
    });
    expect(payload.resumableProvisioning.missing).not.toContain("CRABBOX_TAILSCALE_CLIENT_ID");
    expect(payload.resumableProvisioning.missing).not.toContain("CRABBOX_TAILSCALE_CLIENT_SECRET");
  });

  it("accepts digest-only Koyeb runner images for backwards compatibility", async () => {
    const readiness = await new FleetCoordinator(
      new ProvisioningTestRuntime(new ProvisioningTestStorage()),
      {
        ...baseEnv,
        CRABBOX_KOYEB_IMAGE: digestOnlyRunnerImage,
      },
    ).fetch(fleetRequest("GET", "/v1/providers/koyeb/readiness"));

    expect(readiness.status).toBe(200);
    await expect(readiness.json()).resolves.toMatchObject({
      provider: "koyeb",
      configured: true,
      missing: [],
    });
  });

  it("includes owned Koyeb sandboxes in the provider pool", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (requestInput: RequestInfo | URL, init?: RequestInit) => {
        const incoming =
          requestInput instanceof Request ? requestInput : new Request(requestInput, init);
        const url = new URL(incoming.url);
        if (url.pathname === "/v1/services") {
          return Response.json({ services: [service()], has_next: false });
        }
        if (url.pathname === `/v1/deployments/${deploymentID}`) {
          return Response.json({ deployment: deployment("u".repeat(32)) });
        }
        throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
      }),
    );
    const coordinator = new FleetCoordinator(
      new ProvisioningTestRuntime(new ProvisioningTestStorage()),
      fleetEnv,
    );

    const response = await coordinator.fetch(fleetRequest("GET", "/v1/pool?provider=koyeb"));

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      machines: [
        {
          provider: "koyeb",
          cloudID: serviceID,
          name: serviceName,
          status: "healthy",
          serverType: "large",
          region: "was",
        },
      ],
    });
  });

  it("preserves Koyeb provider attribution in durable run events", async () => {
    const coordinator = new FleetCoordinator(
      new ProvisioningTestRuntime(new ProvisioningTestStorage()),
      {} as Env,
    );
    const runID = `run_${"a".repeat(32)}`;
    const created = await coordinator.fetch(
      fleetRequest("PUT", `/v1/runs/${runID}`, { provider: "koyeb", command: ["true"] }),
    );
    expect(created.status).toBe(201);

    const event = await coordinator.fetch(
      fleetRequest("POST", `/v1/runs/${runID}/events`, {
        type: "lease.created",
        provider: "koyeb",
      }),
    );

    expect(event.status).toBe(201);
    await expect(event.json()).resolves.toMatchObject({ event: { provider: "koyeb" } });
  });
});
