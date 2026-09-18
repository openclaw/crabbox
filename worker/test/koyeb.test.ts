/* oxlint-disable eslint/no-await-in-loop -- Fleet provisioning phases must advance sequentially. */
import { afterEach, describe, expect, it, vi } from "vitest";

import { leaseConfig } from "../src/config";
import { sha256Hex } from "../src/encoding";
import { FleetCoordinator, KoyebProvider, readyPoolSeedDigestV1 } from "../src/fleet";
import {
  KoyebClient,
  KoyebHTTPError,
  KoyebResumableProvisioning,
  koyebConfigurationMissing,
  koyebRegisteredAppIDs,
} from "../src/koyeb";
import {
  cancelProvisioningOperation,
  provisioningAttemptKey,
  provisioningOperationKey,
  putProvisioningOperation,
  type LeaseProvisioningOperation,
} from "../src/lease-provisioning";
import { orgKeyForLabel } from "../src/org-identity";
import { ProjectCheckpointError, type ProjectCheckpointIO } from "../src/project-checkpoints";
import { providerLabelValue } from "../src/provider-labels";
import {
  ProviderResourceUnresolvedError,
  type ProvisioningStep,
} from "../src/provider-provisioning";
import { leaseProviderName } from "../src/slug";
import type {
  Env,
  LeaseRecord,
  ProviderCleanupEvidence,
  ReadyPoolEntry,
  ReadyPoolIdentityV1,
} from "../src/types";
import { ProvisioningTestRuntime, ProvisioningTestStorage } from "./provisioning-fixtures";

const serviceID = "11111111-1111-4111-8111-111111111111";
const deploymentID = "22222222-2222-4222-8222-222222222222";
const latestDeploymentID = "33333333-3333-4333-8333-333333333333";
const serviceName = leaseProviderName("cbx_abcdef123456", "blue-lobster");
const appName = "my-app";
const secondAppID = "66666666-6666-4666-8666-666666666666";
const secondAppName = "my-app-workers-2";
const thirdAppID = "99999999-9999-4999-8999-999999999999";
const thirdAppName = "my-app-workers-3";
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

function managedAppPoolEnv(overrides: Partial<Env> = {}): Env {
  return {
    ...fleetEnv,
    CRABBOX_KOYEB_APP_TARGETS: JSON.stringify([
      {
        organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
        appID: baseEnv.CRABBOX_KOYEB_APP_ID,
        appName,
        region: "was",
      },
      {
        organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
        appID: secondAppID,
        appName: secondAppName,
        region: "was",
      },
    ]),
    ...overrides,
  } as Env;
}

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
  result.serverType = baseEnv.CRABBOX_KOYEB_INSTANCE_TYPE!;
  result.tailscale = true;
  result.sshPort = "22";
  result.sshFallbackPorts = [];
  result.workRoot = "/workspace/crabbox";
  result.tailscaleAuthKey = "tskey-auth-synthetic-one-off-key";
  result.tailscaleHostname = serviceName;
  result.tailscaleTags = ["tag:crabbox"];
  return result;
}

function meshConfig() {
  const result = leaseConfig({
    provider: "koyeb",
    sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey alice@example.com",
    browser: true,
    tailscale: false,
    ttlSeconds: 3_600,
    idleTimeoutSeconds: 600,
  });
  result.serverType = baseEnv.CRABBOX_KOYEB_INSTANCE_TYPE!;
  result.sshPort = "22";
  result.sshFallbackPorts = [];
  result.workRoot = "/workspace/crabbox";
  return result;
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

function capacityDeployment(
  id: string,
  targetServiceID: string,
  targetAppID: string,
  overrides: Record<string, unknown> = {},
) {
  return {
    id,
    organization_id: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
    app_id: targetAppID,
    service_id: targetServiceID,
    status: "HEALTHY",
    definition: {
      regions: ["was"],
      instance_types: [{ type: "large" }],
      scalings: [{ min: 1, max: 1, targets: [] }],
    },
    metadata: {},
    ...overrides,
  };
}

function capacityFixture(
  options: {
    quotas?: Record<string, unknown>;
    quotaUsage?: Record<string, unknown>;
    services?: Array<Record<string, unknown>>;
    appServices?: Record<string, Array<Record<string, unknown>>>;
    deployments?: Array<Record<string, unknown>>;
    appNames?: Record<string, string>;
    catalogMemory?: Record<string, string>;
    serviceInventoryGate?: Promise<void>;
  } = {},
) {
  const requests: Request[] = [];
  const appNames = options.appNames ?? {
    [baseEnv.CRABBOX_KOYEB_APP_ID!]: appName,
    [secondAppID]: secondAppName,
    [thirdAppID]: thirdAppName,
  };
  const organizationServiceLimit = Number(options.quotas?.["services"] ?? 1000);
  const organizationMemoryLimit = Number(options.quotas?.["memory_mb"] ?? 548576);
  const workerInstanceLimit = Number(
    (options.quotas?.["max_instances_by_type"] as Record<string, unknown> | undefined)?.["large"] ??
      0,
  );
  const fetcher = vi.fn<typeof fetch>(async (request) => {
    const incoming = request instanceof Request ? request.clone() : new Request(request);
    requests.push(incoming.clone());
    const url = new URL(incoming.url);
    if (url.pathname.startsWith("/v1/apps/")) {
      const appID = url.pathname.split("/").at(-1)!;
      return Response.json({
        app: {
          id: appID,
          name: appNames[appID] ?? "drifted-app",
          organization_id: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
        },
      });
    }
    if (url.pathname.endsWith("/quotas")) {
      return Response.json({
        quotas: {
          services_by_app: "16",
          services: "1000",
          service_provisioning_concurrency: "30",
          memory_mb: "548576",
          instance_types: [],
          regions: [],
          max_instances_by_type: {},
          ...options.quotas,
        },
      });
    }
    if (url.pathname.includes("/v1/quotas/organizations/") && url.pathname.endsWith("/usage")) {
      return Response.json({
        usage: {
          services_used: 0,
          services_limit: organizationServiceLimit,
          memory_mb_used: 0,
          memory_mb_limit: organizationMemoryLimit,
          instances_by_type: [{ instance_type: "large", used: 0, limit: workerInstanceLimit }],
          ...options.quotaUsage,
        },
      });
    }
    if (url.pathname === "/v1/services") {
      await options.serviceInventoryGate;
      const appID = url.searchParams.get("app_id");
      const services = appID
        ? (options.appServices?.[appID] ??
          (options.services ?? []).filter((entry) => entry["app_id"] === appID))
        : (options.services ?? []);
      const offset = Number(url.searchParams.get("offset") ?? 0);
      return Response.json({
        services: services.slice(offset, offset + 100),
        count: services.length,
        offset,
        limit: 100,
        has_next: offset + 100 < services.length,
      });
    }
    if (url.pathname === "/v1/deployments") {
      const deployments = options.deployments ?? [];
      const offset = Number(url.searchParams.get("offset") ?? 0);
      return Response.json({
        deployments: deployments.slice(offset, offset + 100),
        count: deployments.length,
        offset,
        limit: 100,
        has_next: offset + 100 < deployments.length,
      });
    }
    if (url.pathname.startsWith("/v1/catalog/instances/")) {
      const instanceType = url.pathname.split("/").at(-1)!;
      return Response.json({
        instance: {
          id: instanceType,
          memory: options.catalogMemory?.[instanceType] ?? "4GB",
        },
      });
    }
    throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
  });
  return { fetcher, requests };
}

async function managedCapacityCandidates(
  options: Parameters<typeof capacityFixture>[0] = {},
  env = managedAppPoolEnv(),
) {
  const fixture = capacityFixture(options);
  const capability = new KoyebResumableProvisioning(env, fixture.fetcher);
  const prepared = await capability.prepare(meshConfig(), lease());
  return { ...fixture, capability, candidates: prepared.candidates! };
}

function inventoryServices(count: number, offset = 0) {
  return Array.from({ length: count }, (_, index) =>
    service({
      id: `55555555-5555-4555-8555-${(offset + index + 1).toString(16).padStart(12, "0")}`,
      name: `inventory-${offset + index + 1}`,
    }),
  );
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
        { port: 3031, protocol: "tcp" },
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

function cleanupResourceIdentity(
  active: LeaseRecord,
  overrides: Record<string, unknown> = {},
): string {
  return JSON.stringify({
    schema: "crabbox-koyeb-cleanup/v1",
    scope: active.providerScope,
    organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
    appID: active.providerProject ?? baseEnv.CRABBOX_KOYEB_APP_ID,
    region: active.region ?? "was",
    leaseID: active.id,
    generation: active.createAttemptGeneration,
    serviceID: active.cloudID,
    deploymentID,
    ttlSeconds: 3_600,
    idleTimeoutSeconds: 600,
    ...overrides,
  });
}

async function retainCleanupPublication(
  storage: ProvisioningTestStorage,
  active: LeaseRecord,
  resourceIdentity: string,
): Promise<void> {
  const now = Date.now();
  const operation: LeaseProvisioningOperation = {
    schema: 1,
    leaseID: active.id,
    operationID: `published-${active.id}`,
    generation: active.createAttemptGeneration!,
    scope: active.providerScope!,
    owner: active.owner,
    org: active.org,
    provider: "koyeb",
    createdAt: now,
    deadline: now + 60_000,
    revision: 0,
    step: {
      phase: "terminal",
      attempt: 0,
      state: { version: 1, action: "confirm-delete", serviceID: active.cloudID, deploymentID },
      nextWake: now,
      publication: {
        server: {
          provider: "koyeb",
          id: 0,
          cloudID: active.cloudID,
          region: active.region,
          name: active.serverName || serviceName,
          status: "healthy",
          serverType: active.serverType,
          host: active.host,
          labels: {},
          resourceIdentity,
        },
        serverType: active.serverType,
        market: "on-demand",
      },
    },
  };
  await storage.put(provisioningOperationKey(active.id), operation);
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

async function activeCleanupFixture(transport: "tailscale" | "koyeb-mesh" = "tailscale") {
  const active = lease({
    state: "active",
    cloudID: serviceID,
    ...(transport === "koyeb-mesh" ? { host: privateHost } : {}),
    region: "was",
    providerScope: await new KoyebClient(baseEnv, vi.fn<typeof fetch>()).providerScope(),
    image: {
      id: runnerImage,
      source: "explicit",
      provider: "koyeb",
      kind: "koyeb-sandbox-runner",
      region: "was",
      sourceID: registrySecret,
    },
  });
  const state = {
    deletes: 0,
    present: true,
    requests: [] as string[],
    observations: [] as ProviderCleanupEvidence[],
    beforeDeployment: undefined as (() => Promise<void>) | undefined,
    observe: () => Response.json({ service: service({ status: "DELETING" }) }),
    delete: () => Response.json({ service: service({ status: "DELETING" }) }),
  };
  const fetcher = vi.fn<typeof fetch>(async (input) => {
    const incoming = input instanceof Request ? input : new Request(input);
    const path = new URL(incoming.url).pathname;
    state.requests.push(`${incoming.method} ${path}`);
    if (path === `/v1/services/${serviceID}` && incoming.method === "DELETE") {
      state.deletes += 1;
      return state.delete();
    }
    if (path === `/v1/services/${serviceID}`) {
      return state.present
        ? state.deletes
          ? state.observe()
          : Response.json({ service: service() })
        : new Response("not found", { status: 404 });
    }
    if (path === `/v1/deployments/${deploymentID}`) {
      await state.beforeDeployment?.();
      const owned =
        transport === "koyeb-mesh" ? meshDeployment("u".repeat(32)) : deployment("u".repeat(32));
      owned.definition.env = owned.definition.env.map((entry) =>
        entry.key === "CRABBOX_LEASE_ORG"
          ? { ...entry, value: providerLabelValue(active.org) }
          : entry,
      );
      return Response.json({ deployment: owned });
    }
    throw new Error(`unexpected request ${incoming.method} ${path}`);
  });
  const context = {
    assertCleanupOwner: vi.fn<() => Promise<void>>(async () => {}),
    resourceIdentity: cleanupResourceIdentity(active),
    saveCleanupEvidence: vi.fn<(evidence: ProviderCleanupEvidence) => Promise<void>>(
      async (evidence) => {
        active.providerCleanup = structuredClone(evidence);
        state.observations.push(structuredClone(evidence));
      },
    ),
  };
  return {
    active,
    state,
    context,
    fetcher,
    advance: () => new KoyebClient(baseEnv, fetcher).deleteOwnedService(active, context),
  };
}

async function activeFleetCleanupFixture(extraEnv: Partial<Env> = {}, io?: ProjectCheckpointIO) {
  const fixture = await activeCleanupFixture();
  fixture.active.org = orgKeyForLabel("example-org");
  const storage = new ProvisioningTestStorage();
  await storage.put(`lease:${fixture.active.id}`, fixture.active);
  await retainCleanupPublication(storage, fixture.active, fixture.context.resourceIdentity);
  const runtime = new ProvisioningTestRuntime(storage);
  const environment = { ...fleetEnv, ...extraEnv };
  const provider = new KoyebProvider(environment, fixture.fetcher);
  if (io) vi.spyOn(provider, "projectCheckpointIO").mockReturnValue(io);
  const coordinator = new FleetCoordinator(runtime, environment, {
    koyeb: provider,
  });
  const release = () =>
    coordinator.fetch(
      fleetRequest("POST", `/v1/leases/${fixture.active.id}/release`, { delete: true }),
    );
  const tick = async () => {
    await coordinator.alarm();
    await Promise.all(runtime.maintenance);
    return storage.get<LeaseRecord>(`lease:${fixture.active.id}`);
  };
  const publicLease = async () => {
    const response = await coordinator.fetch(
      fleetRequest("GET", `/v1/leases/${fixture.active.id}`),
    );
    return response.json();
  };
  return { ...fixture, storage, runtime, coordinator, provider, release, tick, publicLease };
}

const readyScope = `koyeb:${baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID}:${baseEnv.CRABBOX_KOYEB_APP_ID}:was:large:clean-runner-v1`;
const checkpointRevision = `sha256:${"a".repeat(64)}`;
const checkpointEnv = {
  CRABBOX_PROJECT_CHECKPOINTS_ENABLED: "true",
  CRABBOX_PROJECT_CHECKPOINT_TARGET: "personal",
  CRABBOX_PROJECT_CHECKPOINT_AUTHORITY: "coordinator-personal",
  CRABBOX_PROJECT_CHECKPOINT_KEY_ID: "version-1",
  CRABBOX_PROJECT_CHECKPOINT_ROOTS: '["/workspace"]',
};

async function projectSnapshot() {
  const work = "uncommitted synthetic project work";
  const content = JSON.stringify({
    schema: "crabbox-project-files/v1",
    bytes: Buffer.byteLength(work),
    gitMetadata: "excluded",
    processState: "not-captured",
    exclusions: [],
    entries: [
      {
        path: "app.js",
        type: "file",
        mode: 0o644,
        data: Buffer.from(work).toString("base64"),
        sha256: await sha256Hex(work),
      },
    ],
  });
  return {
    schema: "crabbox-project-files/v1",
    content: Buffer.from(content).toString("base64"),
    sha256: await sha256Hex(content),
    bytes: Buffer.byteLength(work),
    consistency: "stable-tree",
  };
}

async function projectFleetFixture() {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-09-08T12:05:00.000Z"));
  const io = {
    capture: vi.fn<typeof projectSnapshot>(projectSnapshot),
    restore: vi.fn<ProjectCheckpointIO["restore"]>(async () => ({})),
  };
  const f = await activeFleetCleanupFixture(checkpointEnv, io);
  const checkpoint = (action?: Record<string, unknown>) =>
    f.coordinator.fetch(
      fleetRequest(action ? "POST" : "GET", `/v1/leases/${f.active.id}/project-checkpoint`, action),
    );
  const bound = await checkpoint({
    action: "bind",
    target: "personal",
    authority: "coordinator-personal",
    projectID: "project-a",
    sessionID: "session-a",
    root: "/workspace/project-a",
    acceptedRevision: checkpointRevision,
  });
  expect(bound.status).toBe(200);
  return { ...f, io, checkpoint };
}

describe("Koyeb ready pools and checkpoint lifecycle", () => {
  it("binds pool evidence to immutable image, app, resource size, architecture and bootstrap", async () => {
    const f = await activeCleanupFixture();
    f.active.image!.scope = readyScope;
    const client = new KoyebClient(baseEnv, f.fetcher);
    const identity = { provider: "koyeb", scope: readyScope, id: runnerImage };
    expect(client.readyPoolImageIdentity(f.active)).toEqual(identity);
    expect(client.supportsReadyPoolImageIdentity(identity)).toBe(true);
    expect(await client.observeReadyPoolImageIdentity(f.active)).toEqual(f.active.image);
    for (const overrides of [
      { region: "fra" },
      { serverType: "small" },
      { architecture: "arm64" },
      { image: { ...f.active.image!, sourceID: "different-registry-credential" } },
      { image: { ...f.active.image!, id: digestOnlyRunnerImage } },
      {
        image: {
          ...f.active.image!,
          scope: readyScope.replace("clean-runner-v1", "clean-runner-v2"),
        },
      },
      {
        image: {
          ...f.active.image!,
          scope: readyScope.replace(baseEnv.CRABBOX_KOYEB_APP_ID!, deploymentID),
        },
      },
    ])
      expect(
        client.readyPoolImageIdentity({ ...f.active, ...overrides } as LeaseRecord),
      ).toBeUndefined();
  });

  it("registers and claims once; HTTP and control heartbeats keep the borrow alive", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-09-08T12:05:00.000Z"));
    const f = await activeFleetCleanupFixture();
    f.active.image!.scope = readyScope;
    await f.storage.put(`lease:${f.active.id}`, f.active);
    vi.spyOn(f.provider, "prepareReadyPoolLease").mockResolvedValue();
    const claim = vi.spyOn(f.provider, "claimReadyPoolLease").mockResolvedValue();
    const identity: ReadyPoolIdentityV1 = {
      schema: "crabbox-ready-pool-identity/v1",
      image: { provider: "koyeb", scope: readyScope, id: runnerImage },
      architecture: "amd64",
      seedDigest: await readyPoolSeedDigestV1({}),
      cacheCompatibility: "clean-node24",
    };
    const request = (action: string, body: unknown) =>
      f.coordinator.fetch(fleetRequest("POST", `/v1/ready-pools/coding/${action}`, body));
    expect((await request("register-identity", { leaseID: f.active.id, identity })).status).toBe(
      200,
    );
    const input = { operationID: "operation-one", coldLeaseID: "cbx_cold", identity };
    const selected = await request("claim-identity", { ...input, selectOnly: true });
    expect(selected.status).toBe(200);
    expect(await selected.json()).toMatchObject({
      allocation: { kind: "warm", phase: "selected" },
    });
    expect(claim).not.toHaveBeenCalled();
    expect((await request("claim-identity", input)).status).toBe(200);
    expect(claim).toHaveBeenCalledOnce();
    expect((await request("register-identity", { leaseID: f.active.id, identity })).status).toBe(
      409,
    );
    expect(
      await (
        await request("claim-identity", {
          ...input,
          operationID: "second",
          coldLeaseID: "cbx_second",
        })
      ).json(),
    ).toMatchObject({ allocation: { kind: "cold", reasonCode: "pool-miss" } });
    const replenish = { identity, minReady: 1, maxReady: 1, claim: true };
    const refill = await request("reconcile-identity", replenish);
    expect(refill.status).toBe(200);
    expect(await refill.json()).toMatchObject({
      counts: { ready: 0, busy: 1, inFlight: 1 },
      claim: { token: expect.any(String) },
    });
    const alreadyFilling = await request("reconcile-identity", replenish);
    expect(alreadyFilling.status).toBe(200);
    const capacity = await alreadyFilling.json();
    expect(capacity).toMatchObject({
      counts: { ready: 0, busy: 1, inFlight: 1 },
      capped: true,
    });
    expect(capacity).not.toHaveProperty("claim");
    const key = `typed-ready-pool-v1:coding:${f.active.id}`;
    const before = (await f.storage.get<ReadyPoolEntry>(key))!;
    vi.setSystemTime(Date.now() + 20_000);
    const heartbeat = () =>
      f.coordinator.fetch(fleetRequest("POST", `/v1/leases/${f.active.id}/heartbeat`, {}));
    expect((await heartbeat()).status).toBe(200);
    const after = (await f.storage.get<ReadyPoolEntry>(key))!;
    expect(Date.parse(after.borrowExpiresAt!)).toBeGreaterThan(Date.parse(before.borrowExpiresAt!));
    vi.setSystemTime(Date.now() + 20_000);
    const sent: string[] = [];
    const socket = { send: (message: string) => sent.push(message) } as unknown as WebSocket;
    const coordinator = f.coordinator as unknown as {
      controlHeartbeat: (
        socket: WebSocket,
        attachment: {
          kind: "control";
          clientID: string;
          owner: string;
          org: string;
          subscriptions: Record<string, number>;
        },
        input: { type: "heartbeat"; leaseID: string },
      ) => Promise<void>;
      maintainReadyPoolEntries: (
        entries: ReadyPoolEntry[],
        leases: Map<string, LeaseRecord>,
        nowMs: number,
        typed: boolean,
      ) => Promise<void>;
    };
    await coordinator.controlHeartbeat(
      socket,
      {
        kind: "control",
        clientID: "control-heartbeat",
        owner: f.active.owner,
        org: f.active.org,
        subscriptions: {},
      },
      { type: "heartbeat", leaseID: f.active.id },
    );
    expect(JSON.parse(sent.at(-1)!)).toMatchObject({ type: "heartbeat", ok: true });
    const afterControl = (await f.storage.get<ReadyPoolEntry>(key))!;
    expect(Date.parse(afterControl.borrowExpiresAt!)).toBeGreaterThan(
      Date.parse(after.borrowExpiresAt!),
    );
    vi.setSystemTime(Date.parse(after.borrowExpiresAt!) + 1);
    await coordinator.maintainReadyPoolEntries(
      [afterControl],
      new Map([[f.active.id, (await f.storage.get<LeaseRecord>(`lease:${f.active.id}`))!]]),
      Date.now(),
      true,
    );
    expect(await f.storage.get(key)).toMatchObject({ state: "busy" });
    expect(
      (
        await request("return-identity", {
          leaseID: f.active.id,
          borrowToken: after.borrowToken,
          result: "ready",
        })
      ).status,
    ).toBe(409);
    await f.storage.put(key, {
      ...afterControl,
      borrowExpiresAt: new Date(Date.now() - 1).toISOString(),
    });
    expect((await heartbeat()).status).toBe(200);
    expect(await f.storage.get(key)).toMatchObject({ state: "quarantined" });
  });

  it("holds HTTP ordinary release before lifecycle mutation and DELETE on checkpoint failure", async () => {
    const f = await projectFleetFixture();
    f.io.capture.mockRejectedValue(
      new ProjectCheckpointError("checkpoint_unpreserved_content_limit"),
    );
    const held = await f.release();
    expect(held.status).toBe(409);
    expect(await held.json()).toMatchObject({
      error: "checkpoint_unpreserved_content_limit_reclaim_held",
      state: "blocked",
    });
    expect(await f.storage.get(`lease:${f.active.id}`)).toMatchObject({ state: "active" });
    await f.tick();
    expect(f.state.deletes).toBe(0);
    expect(await (await f.checkpoint()).json()).toMatchObject({
      binding: { state: "blocked", failureCode: "checkpoint_unpreserved_content_limit" },
    });
    f.io.capture.mockImplementation(projectSnapshot);
    expect((await f.release()).status).toBe(200);
    f.state.observe = () => new Response("not found", { status: 404 });
    await f.tick();
    expect(f.state.deletes).toBe(1);
    expect(await (await f.checkpoint()).json()).toMatchObject({
      binding: { state: "reclaim-ready" },
      recovery: { processResume: false },
    });
  });

  it("does not conflate explicit discard, unproven crash and automatic elapsed TTL loss", async () => {
    const f = await projectFleetFixture();
    expect(
      (await f.checkpoint({ action: "record-loss", generation: 1, kind: "crash" })).status,
    ).toBe(409);
    expect((await f.checkpoint({ action: "discard", generation: 1 })).status).toBe(409);
    expect((await f.checkpoint({ action: "record-loss", generation: 1, kind: "ttl" })).status).toBe(
      409,
    );
    f.io.capture.mockRejectedValue(new Error("synthetic unpreserved state"));
    vi.setSystemTime(new Date(f.active.expiresAt).getTime() + 1);
    await f.tick();
    expect(await (await f.checkpoint()).json()).toMatchObject({
      binding: { state: "lost", loss: { kind: "ttl" } },
    });
    expect(f.io.capture).not.toHaveBeenCalled();
    const discarded = await projectFleetFixture();
    expect(
      (await discarded.checkpoint({ action: "discard", generation: 1, confirmDiscard: true }))
        .status,
    ).toBe(200);
    expect((await discarded.release()).status).toBe(200);
    expect(discarded.io.capture).not.toHaveBeenCalled();
    expect(await (await discarded.checkpoint()).json()).toMatchObject({
      binding: { loss: { kind: "discard" } },
    });
  });
});

describe("Koyeb active lease deletion confirmation", () => {
  it("uses the published allocation lifecycle after a heartbeat changes the lease idle policy", async () => {
    const { active, state, advance } = await activeCleanupFixture();
    active.idleTimeoutSeconds = 1_200;

    await expect(advance()).resolves.toMatchObject({ status: "pending" });
    expect(state.deletes).toBe(1);
  });

  it("rechecks cleanup authority after journaling and immediately before DELETE", async () => {
    const { state, context, advance } = await activeCleanupFixture();
    const save = context.saveCleanupEvidence.getMockImplementation()!;
    context.saveCleanupEvidence.mockImplementation(async (evidence) => {
      await save(evidence);
      context.assertCleanupOwner.mockRejectedValue(new Error("synthetic ownership change"));
    });

    await expect(advance()).rejects.toThrow("stage=delete");
    expect(state.deletes).toBe(0);
  });

  it("requires published allocation identity before deleting a present service", async () => {
    const { state, context, advance } = await activeCleanupFixture();
    delete context.resourceIdentity;

    await expect(advance()).rejects.toThrow("allocation identity is missing or inconsistent");
    expect(state.deletes).toBe(0);
  });

  it("keeps idempotent absence for a legacy publication without allocation identity", async () => {
    const { state, context, advance } = await activeCleanupFixture();
    delete context.resourceIdentity;
    state.present = false;

    await expect(advance()).resolves.toBeUndefined();
    expect(state.deletes).toBe(0);
  });

  it.each([
    ["organizationID", serviceID],
    ["appID", secondAppID],
    ["region", "fra"],
    ["serviceID", latestDeploymentID],
    ["deploymentID", latestDeploymentID],
    ["ttlSeconds", 3_599],
    ["idleTimeoutSeconds", 1_200],
  ] as const)(
    "refuses a present service when published %s identity changed",
    async (field, value) => {
      const { state, context, advance } = await activeCleanupFixture();
      context.resourceIdentity = JSON.stringify({
        ...(JSON.parse(context.resourceIdentity) as Record<string, unknown>),
        [field]: value,
      });

      await expect(advance()).rejects.toBeInstanceOf(ProviderResourceUnresolvedError);
      expect(state.deletes).toBe(0);
    },
  );

  it.each([
    "DELETING",
    "HEALTHY",
    "STARTING",
    "DEGRADED",
    "UNHEALTHY",
    "PAUSING",
    "PAUSED",
    "RESUMING",
  ])(
    "keeps an accepted deletion with owned %s observation pending and resumes without another DELETE",
    async (status) => {
      const { active, state, advance } = await activeCleanupFixture();
      state.observe = () => {
        expect(active.providerCleanup).toMatchObject({
          deleteResult: "accepted",
          deleteAcceptedAt: expect.any(String),
        });
        return Response.json({ service: service({ status }) });
      };
      await expect(advance()).resolves.toMatchObject({
        status: "pending",
        nextCheckAt: expect.any(String),
      });
      await expect(advance()).resolves.toMatchObject({ status: "pending" });
      state.observe = () => Response.json({ service: service({ status: "DELETED" }) });
      await expect(advance()).resolves.toBeUndefined();
      expect(state.deletes).toBe(1);
      expect(active.providerCleanup).toMatchObject({ confirmation: { method: "service-deleted" } });
    },
  );

  it.each([
    ["unknown status", () => Response.json({ service: service({ status: "FUTURE_UNKNOWN" }) })],
    ["malformed service", () => Response.json({})],
    ["malformed JSON", () => new Response("{")],
    ["wrong service", () => Response.json({ service: service({ id: latestDeploymentID }) })],
    ["wrong app", () => Response.json({ service: service({ app_id: latestDeploymentID }) })],
    [
      "changed deployment",
      () => Response.json({ service: service({ latest_deployment_id: latestDeploymentID }) }),
    ],
    ["authentication", () => new Response("unauthorized", { status: 401 })],
    ["authorization", () => new Response("forbidden", { status: 403 })],
    [
      "transport",
      () => {
        throw new Error("synthetic transport failure");
      },
    ],
  ] as const)(
    "does not turn a %s confirmation failure into pending or complete",
    async (_label, observation) => {
      const { active, state, advance } = await activeCleanupFixture();
      state.observe = observation;
      await expect(advance()).rejects.toBeInstanceOf(ProviderResourceUnresolvedError);
      expect(state.deletes).toBe(1);
      expect(active.providerCleanup?.confirmation).toBeUndefined();
      expect(active.providerCleanup).toMatchObject({ deleteResult: "accepted" });
    },
  );

  it.each([401, 403, 500, 0])(
    "keeps DELETE failure %s terminal and never redispatches its uncertain intent",
    async (status) => {
      const { active, state, advance } = await activeCleanupFixture();
      state.delete = () => {
        if (!status) throw new Error("synthetic lost delete response");
        return new Response("synthetic delete failure", { status });
      };
      await expect(advance()).rejects.toBeInstanceOf(ProviderResourceUnresolvedError);
      expect(active.providerCleanup).not.toHaveProperty("deleteAcceptedAt");
      await expect(advance()).rejects.toThrow("dispatch outcome is unresolved");
      expect(state.deletes).toBe(1);
      expect(active.providerCleanup?.confirmation).toBeUndefined();
    },
  );

  it("exhausts its explicit observation budget without completing or repeating DELETE", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const { active, state, advance } = await activeCleanupFixture();
    await expect(advance()).resolves.toMatchObject({ status: "pending" });
    expect(active.providerCleanup).toMatchObject({ provider: "koyeb" });
    const evidence = active.providerCleanup!;
    if (evidence.provider !== "koyeb") throw new Error("wrong cleanup evidence");
    vi.setSystemTime(new Date(evidence.confirmationDeadline));
    await expect(advance()).rejects.toThrow("confirmation deadline exhausted");
    expect(state.deletes).toBe(1);
    expect(active.providerCleanup?.confirmation).toBeUndefined();
  });

  it("confirms DELETE not-found only when GET independently observes absence", async () => {
    const { active, state, advance } = await activeCleanupFixture();
    state.delete = () => new Response("not found", { status: 404 });
    await expect(advance()).rejects.toThrow("absence evidence conflicts with a present service");
    expect(active.providerCleanup?.confirmation).toBeUndefined();
    state.observe = () => new Response("not found", { status: 404 });
    await expect(advance()).resolves.toBeUndefined();
    expect(active.providerCleanup).toMatchObject({
      deleteResult: "not-found",
      confirmation: { method: "service-absent" },
    });
    expect(state.deletes).toBe(1);
  });

  it("retains only bounded stage and HTTP status when provider errors contain secrets", async () => {
    const { state, advance } = await activeCleanupFixture();
    state.observe = () => new Response("unrelated-secret-value", { status: 403 });
    await expect(advance()).rejects.toThrow("Koyeb cleanup failed stage=confirmation http=403");
    state.observe = () => {
      throw new Error("other-sensitive-transport-value");
    };
    await expect(advance()).rejects.toThrow("Koyeb cleanup failed stage=confirmation");
  });

  it("fences changed lease identity on a resumed confirmation", async () => {
    const { active, state, advance } = await activeCleanupFixture();
    await advance();
    active.createAttemptGeneration = "replacement-generation";
    const requestCount = state.requests.length;
    await expect(advance()).rejects.toThrow("retained cleanup evidence does not match");
    expect(state.requests.slice(requestCount)).toEqual([`GET /v1/services/${serviceID}`]);
    expect(state.deletes).toBe(1);
  });

  it("does not mutate when dispatch evidence cannot be durably saved", async () => {
    const { state, context, advance } = await activeCleanupFixture();
    context.saveCleanupEvidence.mockRejectedValueOnce(new Error("synthetic persistence failure"));
    await expect(advance()).rejects.toBeInstanceOf(ProviderResourceUnresolvedError);
    expect(state.deletes).toBe(0);
  });

  it("does not repeat DELETE when acceptance persistence was interrupted", async () => {
    const { state, context, advance } = await activeCleanupFixture();
    const save = context.saveCleanupEvidence.getMockImplementation()!;
    context.saveCleanupEvidence.mockImplementation(async (evidence) => {
      if (evidence.provider === "koyeb" && evidence.deleteAcceptedAt)
        throw new Error("synthetic interrupted commit");
      await save(evidence);
    });
    await expect(advance()).rejects.toBeInstanceOf(ProviderResourceUnresolvedError);
    await expect(advance()).rejects.toThrow("dispatch outcome is unresolved");
    expect(state.deletes).toBe(1);
  });

  it("checks the cleanup owner again after the final provider observation", async () => {
    const { active, state, context, advance } = await activeCleanupFixture();
    await advance();
    state.observe = () => {
      context.assertCleanupOwner.mockRejectedValue(new Error("synthetic ownership change"));
      return new Response("not found", { status: 404 });
    };
    await expect(advance()).rejects.toThrow("stage=confirmation");
    expect(active.providerCleanup?.confirmation).toBeUndefined();
    expect(state.deletes).toBe(1);
  });
});

describe("Koyeb service inventory", () => {
  it("collects 101 services while preserving scope and name filters on every page", async () => {
    const inventory = inventoryServices(101);
    const requests: Request[] = [];
    const client = new KoyebClient(baseEnv, async (request) => {
      const incoming = request instanceof Request ? request : new Request(request);
      requests.push(incoming);
      const offset = Number(new URL(incoming.url).searchParams.get("offset"));
      return Response.json({
        services: inventory.slice(offset, offset + 100),
        limit: "100",
        offset: String(offset),
        count: "101",
        has_next: offset === 0,
      });
    });

    const services = await client.listServices(serviceName);

    expect(services.map((entry) => entry.id)).toEqual(inventory.map((entry) => entry.id));
    expect(
      requests.map((request) => Object.fromEntries(new URL(request.url).searchParams)),
    ).toEqual([
      {
        app_id: baseEnv.CRABBOX_KOYEB_APP_ID,
        types: "SANDBOX",
        name: serviceName,
        limit: "100",
        offset: "0",
      },
      {
        app_id: baseEnv.CRABBOX_KOYEB_APP_ID,
        types: "SANDBOX",
        name: serviceName,
        limit: "100",
        offset: "100",
      },
    ]);
    expect(requests.every((request) => request.method === "GET")).toBe(true);
  });

  it("collects every direct app service page without a type filter", async () => {
    const inventory = inventoryServices(101);
    const requests: Request[] = [];
    const client = new KoyebClient(baseEnv, async (request) => {
      const incoming = request instanceof Request ? request : new Request(request);
      requests.push(incoming);
      const offset = Number(new URL(incoming.url).searchParams.get("offset"));
      return Response.json({
        services: inventory.slice(offset, offset + 100),
        limit: 100,
        offset,
        count: inventory.length,
        has_next: offset === 0,
      });
    });

    await expect(client.listAllServices()).resolves.toHaveLength(101);
    expect(
      requests.map((request) => Object.fromEntries(new URL(request.url).searchParams)),
    ).toEqual([
      { app_id: baseEnv.CRABBOX_KOYEB_APP_ID, limit: "100", offset: "0" },
      { app_id: baseEnv.CRABBOX_KOYEB_APP_ID, limit: "100", offset: "100" },
    ]);
  });

  it("uses the total count and consumed items to advance through short pages", async () => {
    const inventory = inventoryServices(3);
    const offsets: string[] = [];
    const client = new KoyebClient(baseEnv, async (request) => {
      const incoming = request instanceof Request ? request : new Request(request);
      const offset = new URL(incoming.url).searchParams.get("offset")!;
      offsets.push(offset);
      return Response.json({
        services: inventory.slice(Number(offset), Number(offset) + 1),
        ...(offset === "0" ? { limit: 100, count: 3 } : {}),
      });
    });

    await expect(client.listServices()).resolves.toHaveLength(3);
    expect(offsets).toEqual(["0", "1", "2"]);
  });

  it.each([0, 1])(
    "probes beyond a full page with omitted metadata and %i remaining services",
    async (remaining) => {
      const inventory = inventoryServices(100 + remaining);
      const fetcher = vi.fn<typeof fetch>(async (request) => {
        const incoming = request instanceof Request ? request : new Request(request);
        const offset = Number(new URL(incoming.url).searchParams.get("offset"));
        return Response.json({ services: inventory.slice(offset, offset + 100) });
      });

      await expect(new KoyebClient(baseEnv, fetcher).listServices()).resolves.toHaveLength(
        100 + remaining,
      );
      expect(fetcher).toHaveBeenCalledTimes(2);
    },
  );

  it("accepts a short terminal page with omitted default metadata", async () => {
    const fetcher = vi.fn<typeof fetch>(async () => Response.json({ services: [service()] }));

    await expect(new KoyebClient(baseEnv, fetcher).listServices()).resolves.toHaveLength(1);
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it("rejects a later page failure without returning a partial inventory", async () => {
    const fetcher = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json({ services: [service()], has_next: true }))
      .mockResolvedValueOnce(new Response("provider unavailable", { status: 503 }));

    await expect(new KoyebClient(baseEnv, fetcher).listServices()).rejects.toMatchObject({
      name: "KoyebHTTPError",
      status: 503,
    });
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it.each([
    ["non-boolean continuation", { has_next: "false" }],
    ["null continuation", { has_next: null }],
    ["negative count", { count: -1 }],
    ["fractional count", { count: 1.5 }],
    ["unsafe count", { count: "9007199254740992" }],
    ["malformed count", { count: "1oops" }],
    ["null count", { count: null }],
    ["count below the page size", { count: 0 }],
    ["continuation after the count is exhausted", { count: 1, has_next: true }],
    ["premature terminal flag", { count: 2, has_next: false }],
    ["wrong offset", { offset: 1 }],
    ["malformed offset", { offset: "0oops" }],
    ["zero limit", { limit: 0 }],
    ["limit above the request", { limit: 101 }],
    ["malformed limit", { limit: false }],
    ["missing services", { services: undefined }],
    ["non-array services", { services: {} }],
    ["invalid service identity", { services: [service({ id: "invalid" })] }],
  ])("rejects malformed inventory metadata: %s", async (_name, metadata) => {
    const fetcher = vi.fn<typeof fetch>(async () =>
      Response.json({ services: [service()], ...metadata }),
    );

    await expect(new KoyebClient(baseEnv, fetcher).listServices()).rejects.toThrow(
      "koyeb service inventory",
    );
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["continuation", { has_next: true }],
    ["remaining count", { count: 1 }],
  ])("rejects an empty page claiming more results through %s", async (_name, metadata) => {
    const fetcher = vi.fn<typeof fetch>(async () => Response.json({ services: [], ...metadata }));

    await expect(new KoyebClient(baseEnv, fetcher).listServices()).rejects.toThrow(
      "koyeb service inventory",
    );
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["a repeated service", { services: [service()], has_next: false }],
    ["a repeated offset", { services: inventoryServices(1), offset: 0, has_next: false }],
    ["a changing count", { services: inventoryServices(1), count: 3, has_next: true }],
    ["an empty continuing page", { services: [], has_next: true }],
  ])("rejects %s on a later page", async (_name, page) => {
    const fetcher = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json({ services: [service()], count: 2, has_next: true }))
      .mockResolvedValueOnce(Response.json(page));

    await expect(new KoyebClient(baseEnv, fetcher).listServices()).rejects.toThrow(
      "koyeb service inventory",
    );
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it("rejects repeated service IDs within one page", async () => {
    const client = new KoyebClient(baseEnv, async () =>
      Response.json({ services: [service(), service()], has_next: false }),
    );

    await expect(client.listServices()).rejects.toThrow("koyeb service inventory");
  });
});

describe("Koyeb managed app targets", () => {
  it.each([
    ["malformed JSON", "{"],
    [
      "duplicate app IDs",
      JSON.stringify([
        {
          organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
          appID: baseEnv.CRABBOX_KOYEB_APP_ID,
          appName,
          region: "was",
        },
        {
          organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
          appID: baseEnv.CRABBOX_KOYEB_APP_ID,
          appName: secondAppName,
          region: "was",
        },
      ]),
    ],
    [
      "duplicate app names",
      JSON.stringify([
        {
          organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
          appID: baseEnv.CRABBOX_KOYEB_APP_ID,
          appName,
          region: "was",
        },
        {
          organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
          appID: secondAppID,
          appName,
          region: "was",
        },
      ]),
    ],
    [
      "missing legacy target",
      JSON.stringify([
        {
          organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
          appID: secondAppID,
          appName: secondAppName,
          region: "was",
        },
      ]),
    ],
    [
      "wrong organization",
      JSON.stringify([
        {
          organizationID: "77777777-7777-4777-8777-777777777777",
          appID: baseEnv.CRABBOX_KOYEB_APP_ID,
          appName,
          region: "was",
        },
      ]),
    ],
    [
      "wrong region",
      JSON.stringify([
        {
          organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
          appID: baseEnv.CRABBOX_KOYEB_APP_ID,
          appName,
          region: "fra",
        },
      ]),
    ],
  ])("rejects %s", (_name, targets) => {
    expect(
      koyebConfigurationMissing({ ...baseEnv, CRABBOX_KOYEB_APP_TARGETS: targets } as Env),
    ).toContain("CRABBOX_KOYEB_APP_TARGETS");
  });

  it("uses aggregate quota usage to expose thirty worker slots beside a database", async () => {
    const env = managedAppPoolEnv();
    const firstServiceID = "77777777-7777-4777-8777-777777777777";
    const secondServiceID = "88888888-8888-4888-8888-888888888888";
    const firstDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
    const secondDeploymentID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
    const baselineServices = [
      service({
        id: firstServiceID,
        name: "coordinator",
        type: "WEB",
        active_deployment_id: firstDeploymentID,
        latest_deployment_id: firstDeploymentID,
      }),
      service({
        id: secondServiceID,
        name: "postgres",
        type: "DATABASE",
        active_deployment_id: secondDeploymentID,
        latest_deployment_id: secondDeploymentID,
      }),
    ];
    const { fetcher, requests } = capacityFixture({
      quotaUsage: {
        services_used: 170,
        services_limit: 1000,
        memory_mb_used: 222856,
        memory_mb_limit: 548576,
        instances_by_type: [{ instance_type: "large", used: 12, limit: 0 }],
      },
      services: baselineServices,
      deployments: [
        capacityDeployment(firstDeploymentID, firstServiceID, baseEnv.CRABBOX_KOYEB_APP_ID!),
        capacityDeployment(secondDeploymentID, secondServiceID, baseEnv.CRABBOX_KOYEB_APP_ID!, {
          definition: {
            type: "DATABASE",
            database: {
              neon_postgres: {
                region: "aws-us-east-1",
                instance_type: "small",
              },
            },
          },
        }),
      ],
    });
    const capability = new KoyebResumableProvisioning(env, fetcher);
    const prepared = await capability.prepare(meshConfig(), lease());
    const candidates = prepared.candidates!;

    expect(candidates).toHaveLength(2);
    expect((candidates[0]!.plan.data as any).capacity).toMatchObject({
      organizationServicesUsed: 170,
      organizationMemoryMBUsed: 222856,
      workerInstancesUsed: 12,
      workerMemoryMB: 4096,
      observedProvisioningServiceIDs: [],
    });
    expect(
      requests
        .map((request) => new URL(request.url))
        .filter((request) => request.pathname === "/v1/services")
        .map((request) => request.searchParams.get("app_id")),
    ).toEqual([null, baseEnv.CRABBOX_KOYEB_APP_ID, secondAppID]);
    expect(
      candidates.map((candidate) => ({
        appID: candidate.lease.providerProject,
        region: candidate.lease.region,
        observed: (candidate.plan.data as any).capacity.observedServiceIDs.length,
        readyPoolScope: (candidate.plan.data as any).readyPoolScope,
        privateHost: (candidate.plan.data as any).privateHost,
      })),
    ).toEqual([
      {
        appID: baseEnv.CRABBOX_KOYEB_APP_ID,
        region: "was",
        observed: 2,
        readyPoolScope: readyScope,
        privateHost,
      },
      {
        appID: secondAppID,
        region: "was",
        observed: 0,
        readyPoolScope: readyScope,
        privateHost: `${serviceName}.${secondAppName}.internal`,
      },
    ]);

    const storage = new ProvisioningTestStorage();
    const selectedApps: string[] = [];
    for (let index = 0; index < 30; index += 1) {
      const reservation = lease({ id: `cbx_${index.toString(16).padStart(12, "0")}` });
      const selected = await capability.selectAdmission(storage, candidates, reservation);
      const candidate = candidates[selected]!;
      Object.assign(reservation, candidate.lease);
      await storage.put(`lease:${reservation.id}`, reservation);
      selectedApps.push(reservation.providerProject!);
    }
    expect(selectedApps.filter((appID) => appID === baseEnv.CRABBOX_KOYEB_APP_ID)).toHaveLength(14);
    expect(selectedApps.filter((appID) => appID === secondAppID)).toHaveLength(16);
    await expect(
      capability.selectAdmission(storage, candidates, lease({ id: "cbx_ffffffffffff" })),
    ).rejects.toThrow("Koyeb organization capacity is exhausted");
    expect(
      requests.filter((request) => new URL(request.url).pathname === "/v1/services"),
    ).toHaveLength(3);
  });

  it("does not replace aggregate service usage with a shorter organization inventory", async () => {
    const services = Array.from({ length: 134 }, (_, index) => {
      const suffix = (index + 1).toString(16).padStart(12, "0");
      return service({
        id: `70000000-0000-4000-8000-${suffix}`,
        app_id: thirdAppID,
        active_deployment_id: `80000000-0000-4000-8000-${suffix}`,
        latest_deployment_id: `80000000-0000-4000-8000-${suffix}`,
      });
    });
    const deployments = services.map((entry, index) =>
      capacityDeployment(
        `80000000-0000-4000-8000-${(index + 1).toString(16).padStart(12, "0")}`,
        String(entry.id),
        thirdAppID,
      ),
    );
    const { candidates } = await managedCapacityCandidates({
      quotaUsage: { services_used: 170 },
      services,
      deployments,
    });
    const capacity = (candidates[0]!.plan.data as any).capacity;

    expect(capacity.observedOrganizationServiceIDs).toHaveLength(134);
    expect(capacity.organizationServicesUsed).toBe(170);
  });

  it("expires only the local reuse window for an aggregate snapshot", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-16T02:17:48.000Z"));
    const { capability, candidates } = await managedCapacityCandidates();
    vi.setSystemTime(new Date("2026-09-16T02:18:48.001Z"));

    await expect(
      capability.selectAdmission(new ProvisioningTestStorage(), candidates, lease()),
    ).rejects.toThrow("Koyeb organization capacity snapshot is stale");
  });

  it("ages the snapshot from quota receipt while later inventory is deferred", async () => {
    let now = Date.parse("2026-09-16T02:17:48.000Z");
    vi.spyOn(Date, "now").mockImplementation(() => now);
    let releaseInventory!: () => void;
    const serviceInventoryGate = new Promise<void>((resolve) => {
      releaseInventory = resolve;
    });
    const fixture = capacityFixture({ serviceInventoryGate });
    const capability = new KoyebResumableProvisioning(managedAppPoolEnv(), fixture.fetcher);
    const preparedPromise = capability.prepare(meshConfig(), lease());
    await vi.waitFor(() => {
      expect(
        fixture.requests.some((request) => new URL(request.url).pathname.endsWith("/usage")),
      ).toBe(true);
    });
    await new Promise((resolve) => setTimeout(resolve, 0));
    now = Date.parse("2026-09-16T02:18:48.001Z");
    releaseInventory();
    const prepared = await preparedPromise;

    await expect(
      capability.selectAdmission(new ProvisioningTestStorage(), prepared.candidates!, lease()),
    ).rejects.toThrow("Koyeb organization capacity snapshot is stale");
  });

  it.each([
    ["missing services_by_app", { services_by_app: undefined }],
    ["malformed service concurrency", { service_provisioning_concurrency: "30oops" }],
    ["zero memory", { memory_mb: "0" }],
    ["malformed instance type restrictions", { instance_types: "large" }],
    ["malformed region restrictions", { regions: ["US East"] }],
    ["malformed per-type limits", { max_instances_by_type: { large: "many" } }],
  ])("fails closed on %s quota evidence", async (_label, quotas) => {
    await expect(
      managedCapacityCandidates({ quotas: quotas as Record<string, unknown> }),
    ).rejects.toThrow(/Koyeb organization .*quota|Koyeb organization quota evidence/);
  });

  it.each([
    ["excluded instance type", { instance_types: ["medium"] }],
    ["excluded region", { regions: ["fra"] }],
  ])("rejects a configured %s", async (_label, quotas) => {
    await expect(
      managedCapacityCandidates({ quotas: quotas as Record<string, unknown> }),
    ).rejects.toThrow("excluded by organization quota");
  });

  it("fails closed on incomplete service, deployment, and catalog evidence", async () => {
    const capacityServiceID = "77777777-7777-4777-8777-777777777777";
    const capacityDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
    const cases: Array<{
      options: Parameters<typeof capacityFixture>[0];
      message: string;
    }> = [
      {
        options: {
          services: [
            service({
              id: capacityServiceID,
              active_deployment_id: "",
              latest_deployment_id: "",
            }),
          ],
        },
        message: "service deployment identity is unresolved",
      },
      {
        options: {
          services: [
            service({
              id: capacityServiceID,
              active_deployment_id: capacityDeploymentID,
              latest_deployment_id: capacityDeploymentID,
            }),
          ],
        },
        message: "service deployment inventory is inconsistent",
      },
      {
        options: { catalogMemory: { large: "unknown" } },
        message: "instance catalog memory is malformed",
      },
    ];

    for (const testCase of cases) {
      await expect(managedCapacityCandidates(testCase.options)).rejects.toThrow(testCase.message);
    }
  });

  it.each([
    ["string service counter", { services_used: "170" }, "quota usage is incomplete"],
    ["negative memory counter", { memory_mb_used: -1 }, "quota usage is incomplete"],
    ["missing worker type", { instances_by_type: [] }, "configured instance type usage is missing"],
    [
      "duplicate worker type",
      {
        instances_by_type: [
          { instance_type: "large", used: 1, limit: 0 },
          { instance_type: "large", used: 1, limit: 0 },
        ],
      },
      "instance type usage is malformed",
    ],
  ])("fails closed on malformed aggregate %s", async (_label, quotaUsage, message) => {
    await expect(managedCapacityCandidates({ quotaUsage })).rejects.toThrow(message);
  });

  it.each([
    ["service", { services_limit: 999 }],
    ["memory", { memory_mb_limit: 548575 }],
    ["instance type", { instances_by_type: [{ instance_type: "large", used: 0, limit: 1 }] }],
  ])("fails closed when aggregate %s limits disagree", async (_label, quotaUsage) => {
    await expect(managedCapacityCandidates({ quotaUsage })).rejects.toThrow(
      "Koyeb organization quota limits are inconsistent",
    );
  });

  it("rejects an app-scoped service response with the wrong target identity", async () => {
    await expect(
      managedCapacityCandidates({
        appServices: {
          [baseEnv.CRABBOX_KOYEB_APP_ID!]: [service({ app_id: secondAppID })],
          [secondAppID]: [],
        },
      }),
    ).rejects.toThrow("Koyeb app service inventory is malformed");
  });

  it("rejects conflicting global and direct app bindings for one service ID", async () => {
    const sharedServiceID = "77777777-7777-4777-8777-777777777777";
    const globalDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
    const directDeploymentID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";

    await expect(
      managedCapacityCandidates({
        services: [
          service({
            id: sharedServiceID,
            app_id: secondAppID,
            active_deployment_id: globalDeploymentID,
            latest_deployment_id: globalDeploymentID,
          }),
        ],
        appServices: {
          [baseEnv.CRABBOX_KOYEB_APP_ID!]: [
            service({
              id: sharedServiceID,
              active_deployment_id: directDeploymentID,
              latest_deployment_id: directDeploymentID,
            }),
          ],
          [secondAppID]: [],
        },
        deployments: [
          capacityDeployment(globalDeploymentID, sharedServiceID, secondAppID),
          capacityDeployment(directDeploymentID, sharedServiceID, baseEnv.CRABBOX_KOYEB_APP_ID!),
        ],
      }),
    ).rejects.toThrow("Koyeb app service inventory conflicts with organization inventory");
  });

  it("keeps an explicit zero instance-type quota fail closed", async () => {
    const { capability, candidates } = await managedCapacityCandidates({
      quotas: { max_instances_by_type: { large: "0" } },
      quotaUsage: {
        instances_by_type: [{ instance_type: "large", used: 12, limit: 0 }],
      },
    });

    await expect(
      capability.selectAdmission(new ProvisioningTestStorage(), candidates, lease()),
    ).rejects.toThrow("Koyeb organization capacity is exhausted");
  });

  it.each([
    ["organization services", { services: "1" }],
    ["provisioning concurrency", { service_provisioning_concurrency: "1" }],
    ["memory", { memory_mb: "4096" }],
    ["instance type", { max_instances_by_type: { large: "1" } }],
  ])("blocks admission when %s capacity is exhausted", async (constraint, quotas) => {
    const capacityServiceID = "77777777-7777-4777-8777-777777777777";
    const capacityDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
    const provisioning = constraint === "provisioning concurrency";
    const quotaUsage = {
      ...(constraint === "organization services" ? { services_used: 1 } : {}),
      ...(constraint === "memory" ? { memory_mb_used: 4096 } : {}),
      ...(constraint === "instance type"
        ? { instances_by_type: [{ instance_type: "large", used: 1, limit: 1 }] }
        : {}),
    };
    const { capability, candidates } = await managedCapacityCandidates({
      quotas: quotas as Record<string, unknown>,
      quotaUsage,
      services: [
        service({
          id: capacityServiceID,
          status: provisioning ? "STARTING" : "HEALTHY",
          active_deployment_id: capacityDeploymentID,
          latest_deployment_id: capacityDeploymentID,
        }),
      ],
      deployments: [
        capacityDeployment(capacityDeploymentID, capacityServiceID, baseEnv.CRABBOX_KOYEB_APP_ID!, {
          status: provisioning ? "STARTING" : "HEALTHY",
        }),
      ],
    });

    await expect(
      capability.selectAdmission(new ProvisioningTestStorage(), candidates, lease()),
    ).rejects.toThrow("Koyeb organization capacity is exhausted");
  });

  it.each([
    ["service status", "STARTING", "HEALTHY"],
    ["deployment status", "HEALTHY", "STARTING"],
  ])(
    "counts direct-only provisioning from %s",
    async (_source, serviceStatus, deploymentStatus) => {
      const directServiceID = "77777777-7777-4777-8777-777777777777";
      const directDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
      const { capability, candidates } = await managedCapacityCandidates({
        quotas: { service_provisioning_concurrency: "1" },
        services: [],
        appServices: {
          [baseEnv.CRABBOX_KOYEB_APP_ID!]: [
            service({
              id: directServiceID,
              status: serviceStatus,
              active_deployment_id: directDeploymentID,
              latest_deployment_id: directDeploymentID,
            }),
          ],
          [secondAppID]: [],
        },
        deployments: [
          capacityDeployment(directDeploymentID, directServiceID, baseEnv.CRABBOX_KOYEB_APP_ID!, {
            status: deploymentStatus,
          }),
        ],
      });

      await expect(
        capability.selectAdmission(new ProvisioningTestStorage(), candidates, lease()),
      ).rejects.toThrow("Koyeb organization capacity is exhausted");
    },
  );

  it("fails closed when a direct-only service deployment is omitted", async () => {
    await expect(
      managedCapacityCandidates({
        services: [],
        appServices: {
          [baseEnv.CRABBOX_KOYEB_APP_ID!]: [service()],
          [secondAppID]: [],
        },
        deployments: [],
      }),
    ).rejects.toThrow("Koyeb service deployment inventory is inconsistent");
  });

  it.each([
    ["service", { services: "2" }, { services_used: 1 }],
    ["memory", { memory_mb: "8192" }, { memory_mb_used: 4096 }],
    [
      "instance type",
      { max_instances_by_type: { large: "2" } },
      { instances_by_type: [{ instance_type: "large", used: 1, limit: 2 }] },
    ],
  ])(
    "always overlays a visible durable lease on cached aggregate %s usage",
    async (_dimension, quotas, quotaUsage) => {
      const targetServiceID = "77777777-7777-4777-8777-777777777777";
      const targetDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
      const { capability, candidates } = await managedCapacityCandidates({
        quotas,
        quotaUsage,
        services: [
          service({
            id: targetServiceID,
            active_deployment_id: targetDeploymentID,
            latest_deployment_id: targetDeploymentID,
          }),
        ],
        deployments: [
          capacityDeployment(targetDeploymentID, targetServiceID, baseEnv.CRABBOX_KOYEB_APP_ID!),
        ],
      });
      const storage = new ProvisioningTestStorage();
      const reservation = lease({
        ...candidates[0]!.lease,
        state: "active",
        cloudID: targetServiceID,
      });
      await storage.put(`lease:${reservation.id}`, reservation);

      await expect(capability.selectAdmission(storage, candidates, lease())).rejects.toThrow(
        "Koyeb organization capacity is exhausted",
      );
    },
  );

  it("serializes simultaneous admissions against durable unobserved reservations", async () => {
    const { capability, candidates } = await managedCapacityCandidates({
      quotas: {
        services_by_app: "1",
        services: "1",
        service_provisioning_concurrency: "2",
        memory_mb: "8192",
      },
    });
    const storage = new ProvisioningTestStorage();
    const admit = (id: string) =>
      storage.transaction(async (transaction) => {
        const reservation = lease({ id });
        const selected = await capability.selectAdmission(transaction, candidates, reservation);
        Object.assign(reservation, candidates[selected]!.lease);
        await transaction.put(`lease:${id}`, reservation);
        return reservation.providerProject;
      });

    const results = await Promise.allSettled([
      admit("cbx_000000000001"),
      admit("cbx_000000000002"),
    ]);

    expect(results.filter((result) => result.status === "fulfilled")).toHaveLength(1);
    expect(results.filter((result) => result.status === "rejected")).toHaveLength(1);
    expect((await storage.list({ prefix: "lease:" })).size).toBe(1);
  });

  it("counts a Fleet-shaped admitted lease before provider dispatch begins", async () => {
    const { capability, candidates } = await managedCapacityCandidates({
      quotas: {
        services_by_app: "1",
        services: "1",
        service_provisioning_concurrency: "1",
        memory_mb: "4096",
        max_instances_by_type: { large: "1" },
      },
    });
    const storage = new ProvisioningTestStorage();
    await storage.put(
      "lease:cbx_admitted000001",
      lease({
        id: "cbx_admitted000001",
        state: "provisioning",
        cloudID: "",
        provisioningResourceMayExist: false,
        providerProject: candidates[0]!.lease.providerProject,
        providerScope: candidates[0]!.lease.providerScope,
        region: candidates[0]!.lease.region,
      }),
    );

    await expect(capability.selectAdmission(storage, candidates, lease())).rejects.toThrow(
      "Koyeb organization capacity is exhausted",
    );
  });

  it("does not treat an external registered lease as a managed app reservation", async () => {
    const { capability, candidates } = await managedCapacityCandidates({
      quotas: {
        services_by_app: "2",
        services: "2",
        service_provisioning_concurrency: "2",
        memory_mb: "8192",
        max_instances_by_type: { large: "2" },
      },
      quotaUsage: {
        services_used: 1,
        memory_mb_used: 4096,
        instances_by_type: [{ instance_type: "large", used: 1, limit: 2 }],
      },
    });
    const storage = new ProvisioningTestStorage();
    await storage.put(
      "lease:registered-worker",
      lease({
        id: "registered-worker",
        lifecycle: "registered",
        state: "active",
        createAttemptGeneration: undefined,
      }),
    );

    const admitted = lease({ id: "cbx_managed000001" });
    const selected = await capability.selectAdmission(storage, candidates, admitted);
    Object.assign(admitted, candidates[selected]!.lease);
    await storage.put(`lease:${admitted.id}`, admitted);
    await expect(
      capability.selectAdmission(storage, candidates, lease({ id: "cbx_managed000002" })),
    ).rejects.toThrow("Koyeb organization capacity is exhausted");
  });

  it.each(["provisioning", "released", "expired", "failed"] as const)(
    "keeps a %s durable reservation when provider inventory already looks healthy",
    async (state) => {
      const targetServiceID = "77777777-7777-4777-8777-777777777777";
      const targetDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
      const { capability, candidates } = await managedCapacityCandidates({
        quotas: { service_provisioning_concurrency: "1" },
        services: [
          service({
            id: targetServiceID,
            app_id: secondAppID,
            active_deployment_id: targetDeploymentID,
            latest_deployment_id: targetDeploymentID,
          }),
        ],
        deployments: [capacityDeployment(targetDeploymentID, targetServiceID, secondAppID)],
      });
      const storage = new ProvisioningTestStorage();
      const reservation = lease({
        ...candidates[1]!.lease,
        state,
        cloudID: targetServiceID,
        provisioningResourceMayExist: true,
      });
      await storage.put(`lease:${reservation.id}`, reservation);

      await expect(capability.selectAdmission(storage, candidates, lease())).rejects.toThrow(
        "Koyeb organization capacity is exhausted",
      );
    },
  );

  it("retains an uncertain canceled create across simultaneous admissions in both apps", async () => {
    const { capability, candidates } = await managedCapacityCandidates({
      quotas: { service_provisioning_concurrency: "2" },
    });
    const storage = new ProvisioningTestStorage();
    const runtime = new ProvisioningTestRuntime(storage);
    const reservation = lease({ ...candidates[0]!.lease });
    const uncertain = { version: 1, action: "discover", outcomeUncertain: true };
    const operation: LeaseProvisioningOperation = {
      schema: 1,
      leaseID: reservation.id,
      operationID: "uncertain-create",
      generation: reservation.createAttemptGeneration!,
      scope: reservation.providerScope!,
      owner: reservation.owner,
      org: reservation.org,
      provider: "koyeb",
      createdAt: Date.now(),
      deadline: Date.now() + 60_000,
      revision: 0,
      step: { phase: "provisioning", attempt: 0, state: uncertain, nextWake: Date.now() },
    };
    await runtime.commitAndWake(async (transaction) => {
      await transaction.put(`lease:${reservation.id}`, reservation);
      await putProvisioningOperation(transaction, operation);
      await cancelProvisioningOperation(transaction, reservation, Date.now());
    });
    const canceled = await storage.get<LeaseRecord>(`lease:${reservation.id}`);
    expect(canceled).toMatchObject({ state: "released", provisioningResourceMayExist: true });
    const journalBefore = await storage.get(provisioningAttemptKey(operation));
    expect(journalBefore).toMatchObject({ state: uncertain });
    const operationBefore = await storage.get(provisioningOperationKey(reservation.id));
    const admit = (id: string) =>
      runtime.commitAndWake(async (transaction) => {
        const requested = lease({ id });
        const selected = await capability.selectAdmission(transaction, candidates, requested);
        Object.assign(requested, candidates[selected]!.lease);
        await transaction.put(`lease:${id}`, requested);
        return requested.providerProject;
      });

    const results = await Promise.allSettled([
      admit("cbx_000000000001"),
      admit("cbx_000000000002"),
    ]);

    expect(results.filter((result) => result.status === "fulfilled")).toEqual([
      { status: "fulfilled", value: secondAppID },
    ]);
    expect(results.filter((result) => result.status === "rejected")).toHaveLength(1);
    expect((await storage.list({ prefix: "lease:" })).size).toBe(2);
    expect(await storage.get(`lease:${reservation.id}`)).toEqual(canceled);
    expect(await storage.get(provisioningAttemptKey(operation))).toEqual(journalBefore);
    expect(await storage.get(provisioningOperationKey(reservation.id))).toEqual(operationBefore);
  });

  it.each(["STARTING", "HEALTHY"])(
    "does not duplicate a visible %s service already bound in its durable lease",
    async (status) => {
      const targetServiceID = "77777777-7777-4777-8777-777777777777";
      const targetDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
      const { capability, candidates } = await managedCapacityCandidates({
        quotas: { service_provisioning_concurrency: status === "STARTING" ? "2" : "1" },
        services: [
          service({
            id: targetServiceID,
            status,
            active_deployment_id: targetDeploymentID,
            latest_deployment_id: targetDeploymentID,
          }),
        ],
        deployments: [
          capacityDeployment(targetDeploymentID, targetServiceID, baseEnv.CRABBOX_KOYEB_APP_ID!),
        ],
      });
      const storage = new ProvisioningTestStorage();
      const reservation = lease({
        ...candidates[0]!.lease,
        state: status === "STARTING" ? "provisioning" : "active",
        cloudID: targetServiceID,
      });
      await storage.put(`lease:${reservation.id}`, reservation);

      await expect(capability.selectAdmission(storage, candidates, lease())).resolves.toBe(1);
    },
  );

  it("rejects a durable lease whose observed service belongs to another registered app", async () => {
    const targetServiceID = "77777777-7777-4777-8777-777777777777";
    const targetDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
    const { capability, candidates } = await managedCapacityCandidates({
      services: [
        service({
          id: targetServiceID,
          app_id: secondAppID,
          active_deployment_id: targetDeploymentID,
          latest_deployment_id: targetDeploymentID,
        }),
      ],
      deployments: [capacityDeployment(targetDeploymentID, targetServiceID, secondAppID)],
    });
    const storage = new ProvisioningTestStorage();
    await storage.put(
      "lease:cbx_wrongapp00001",
      lease({
        id: "cbx_wrongapp00001",
        state: "active",
        cloudID: targetServiceID,
        providerProject: candidates[0]!.lease.providerProject,
        providerScope: candidates[0]!.lease.providerScope,
        region: candidates[0]!.lease.region,
      }),
    );

    await expect(capability.selectAdmission(storage, candidates, lease())).rejects.toThrow(
      "Koyeb durable lease service identity does not match its target app",
    );
  });

  it("uses direct app membership when organization inventory omits a bound service", async () => {
    const targetServiceID = "77777777-7777-4777-8777-777777777777";
    const firstServiceID = "11111111-1111-4111-8111-111111111112";
    const secondServiceID = "11111111-1111-4111-8111-111111111113";
    const firstDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1";
    const secondDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2";
    const targetDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3";
    const { capability, candidates } = await managedCapacityCandidates({
      quotas: { services_by_app: "2" },
      appServices: {
        [baseEnv.CRABBOX_KOYEB_APP_ID!]: [
          service({
            id: firstServiceID,
            active_deployment_id: firstDeploymentID,
            latest_deployment_id: firstDeploymentID,
          }),
          service({
            id: secondServiceID,
            active_deployment_id: secondDeploymentID,
            latest_deployment_id: secondDeploymentID,
          }),
        ],
        [secondAppID]: [
          service({
            id: targetServiceID,
            app_id: secondAppID,
            active_deployment_id: targetDeploymentID,
            latest_deployment_id: targetDeploymentID,
          }),
        ],
      },
      deployments: [
        capacityDeployment(firstDeploymentID, firstServiceID, baseEnv.CRABBOX_KOYEB_APP_ID!),
        capacityDeployment(secondDeploymentID, secondServiceID, baseEnv.CRABBOX_KOYEB_APP_ID!),
        capacityDeployment(targetDeploymentID, targetServiceID, secondAppID),
      ],
    });
    const storage = new ProvisioningTestStorage();
    const reservation = lease({
      ...candidates[1]!.lease,
      state: "active",
      cloudID: targetServiceID,
    });
    await storage.put(`lease:${reservation.id}`, reservation);

    await expect(capability.selectAdmission(storage, candidates, lease())).resolves.toBe(1);
  });

  it("rejects direct app membership in a different frozen target", async () => {
    const targetServiceID = "77777777-7777-4777-8777-777777777777";
    const targetDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
    const { capability, candidates } = await managedCapacityCandidates({
      appServices: {
        [baseEnv.CRABBOX_KOYEB_APP_ID!]: [],
        [secondAppID]: [
          service({
            id: targetServiceID,
            app_id: secondAppID,
            active_deployment_id: targetDeploymentID,
            latest_deployment_id: targetDeploymentID,
          }),
        ],
      },
      deployments: [capacityDeployment(targetDeploymentID, targetServiceID, secondAppID)],
    });
    const storage = new ProvisioningTestStorage();
    const reservation = lease({
      ...candidates[0]!.lease,
      state: "active",
      cloudID: targetServiceID,
    });
    await storage.put(`lease:${reservation.id}`, reservation);

    await expect(capability.selectAdmission(storage, candidates, lease())).rejects.toThrow(
      "Koyeb durable lease service identity does not match its target app",
    );
  });

  it("keeps ready-pool image compatibility stable while lease ownership stays app-specific", async () => {
    const env = managedAppPoolEnv();
    const secondScope = await new KoyebClient(
      env,
      vi.fn<typeof fetch>(),
      secondAppID,
    ).providerScope();
    const selected = lease({
      state: "active",
      cloudID: serviceID,
      providerProject: secondAppID,
      providerScope: secondScope,
      region: "was",
      image: {
        id: runnerImage,
        source: "explicit",
        provider: "koyeb",
        kind: "koyeb-sandbox-runner",
        region: "was",
        scope: readyScope,
        sourceID: registrySecret,
      },
    });
    const target = new KoyebClient(env, vi.fn<typeof fetch>(), secondAppID);
    const legacy = new KoyebClient(env, vi.fn<typeof fetch>());

    expect(target.readyPoolImageIdentity(selected)).toEqual({
      provider: "koyeb",
      scope: readyScope,
      id: runnerImage,
    });
    expect(target.supportsReadyPoolImageIdentity(target.readyPoolImageIdentity(selected)!)).toBe(
      true,
    );
    await expect(legacy.ownedServiceForLease(selected)).rejects.toThrow(
      "belongs to another context",
    );
  });

  it("routes a selected second-app lease through provisioning, observation, checkpoint, ready-pool, and cleanup", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-09-08T12:05:00.000Z"));
    const env = managedAppPoolEnv();
    const targetServiceID = "77777777-7777-4777-8777-777777777777";
    const targetDeploymentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
    const targetHost = `${serviceName}.${secondAppName}.internal`;
    let created = false;
    let deleted = false;
    let sandboxSecret = "";
    let projectAction = "";
    let createdBody: Record<string, unknown> | undefined;
    const routedRequests: Request[] = [];
    const capacity = capacityFixture();
    const targetService = () =>
      service({
        id: targetServiceID,
        app_id: secondAppID,
        status: deleted ? "DELETING" : "HEALTHY",
        active_deployment_id: targetDeploymentID,
        latest_deployment_id: targetDeploymentID,
      });
    const targetDeployment = () =>
      meshDeployment(sandboxSecret, {
        id: targetDeploymentID,
        app_id: secondAppID,
        service_id: targetServiceID,
      });
    const fetcher = vi.fn<typeof fetch>(async (input) => {
      const incoming = input instanceof Request ? input.clone() : new Request(input);
      routedRequests.push(incoming.clone());
      const url = new URL(incoming.url);
      if (url.hostname === targetHost) {
        if (url.pathname.endsWith("/write_file")) {
          const body = (await incoming.json()) as { content?: string };
          try {
            projectAction = String(
              (JSON.parse(body.content ?? "") as Record<string, unknown>)["action"] ?? "",
            );
          } catch {
            projectAction = "";
          }
          return Response.json({ ok: true });
        }
        if (url.pathname.endsWith("/health")) return Response.json({ ok: true });
        if (url.pathname.endsWith("/bind_port")) {
          return Response.json({ success: true, port: "22" });
        }
        if (url.pathname.endsWith("/run")) {
          const body = (await incoming.json()) as { cmd?: string };
          if (body.cmd === "/usr/local/bin/crabbox-koyeb-bootstrap") {
            return Response.json({
              stdout: JSON.stringify({
                schema: "crabbox-koyeb-sandbox-runner/v2",
                leaseId: serviceName,
                ssh: { user: "crabbox", host: targetHost, port: 22, hostKey: sshHostKey },
                network: { transport: "koyeb-mesh", privateHost: targetHost },
              }),
              stderr: "",
              code: 0,
            });
          }
          return Response.json({
            stdout: JSON.stringify(
              projectAction === "capture"
                ? await projectSnapshot()
                : { schema: "crabbox-clean-runner/v1", state: "clean" },
            ),
            stderr: "",
            code: 0,
          });
        }
      }
      if (url.pathname === "/v1/services" && incoming.method === "POST") {
        createdBody = (await incoming.json()) as Record<string, unknown>;
        sandboxSecret = String(
          (
            (createdBody["definition"] as Record<string, unknown>)["env"] as Array<
              Record<string, unknown>
            >
          ).find((entry) => entry["key"] === "SANDBOX_SECRET")?.["value"] ?? "",
        );
        created = true;
        return Response.json({ service: targetService() });
      }
      if (
        url.pathname === "/v1/services" &&
        url.searchParams.get("app_id") === secondAppID &&
        url.searchParams.has("name")
      ) {
        return Response.json({ services: created ? [targetService()] : [], has_next: false });
      }
      if (url.pathname === `/v1/services/${targetServiceID}` && incoming.method === "DELETE") {
        deleted = true;
        return Response.json({ service: targetService() });
      }
      if (url.pathname === `/v1/services/${targetServiceID}`) {
        return Response.json({ service: targetService() });
      }
      if (url.pathname === `/v1/deployments/${targetDeploymentID}`) {
        return Response.json({ deployment: targetDeployment() });
      }
      return capacity.fetcher(incoming);
    });
    const capability = new KoyebResumableProvisioning(env, fetcher);
    const prepared = await capability.prepare(meshConfig(), lease());
    const candidates = prepared.candidates!;
    const storage = new ProvisioningTestStorage();
    await storage.put(
      "lease:cbx_existing000001",
      lease({
        id: "cbx_existing000001",
        providerProject: candidates[0]!.lease.providerProject,
        providerScope: candidates[0]!.lease.providerScope,
        region: candidates[0]!.lease.region,
      }),
    );
    const selected = await capability.selectAdmission(storage, candidates, lease());
    expect(candidates[selected]!.lease.providerProject).toBe(secondAppID);
    const candidate = candidates[selected]!;
    const selectedLease = lease({ ...candidate.lease });
    let step = candidate.step;
    for (let attempt = 0; attempt < 3; attempt += 1) {
      step = await capability.advance({
        plan: candidate.plan,
        step,
        lease: selectedLease,
        deadline: Date.now() + 60_000,
        recovering: false,
        canceled: false,
        material: candidate.material,
      });
    }
    expect(step).toMatchObject({
      phase: "ready-to-publish",
      publication: {
        server: { cloudID: targetServiceID, region: "was" },
        access: { sshPort: "3031", workRoot: "/workspace/crabbox" },
      },
    });
    expect(JSON.parse(step.publication!.server.resourceIdentity!)).toEqual({
      schema: "crabbox-koyeb-cleanup/v1",
      scope: candidate.plan.scope,
      organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
      appID: secondAppID,
      region: "was",
      leaseID: selectedLease.id,
      generation: selectedLease.createAttemptGeneration,
      serviceID: targetServiceID,
      deploymentID: targetDeploymentID,
      ttlSeconds: 3_600,
      idleTimeoutSeconds: 600,
    });
    expect(createdBody).toMatchObject({ app_id: secondAppID });

    const active = lease({
      ...candidate.lease,
      state: "active",
      cloudID: targetServiceID,
      host: targetHost,
      sshPort: "3031",
      workRoot: "/workspace/crabbox",
      image: step.publication!.image,
    });
    const provider = new KoyebProvider(env, fetcher);
    expect(provider.readyPoolImageIdentity(active)).toEqual({
      provider: "koyeb",
      scope: readyScope,
      id: runnerImage,
    });
    await expect(provider.observeReadyPoolImageIdentity(active)).resolves.toEqual(active.image);
    await expect(
      provider.projectCheckpointIO().capture(active, "/workspace/project-a", "/workspace", "none"),
    ).resolves.toMatchObject({ schema: "crabbox-project-files/v1" });
    await expect(provider.prepareReadyPoolLease(active)).resolves.toBeUndefined();
    const cleanupEvidence: ProviderCleanupEvidence[] = [];
    await expect(
      provider.releaseLease(active, {
        resourceIdentity: step.publication!.server.resourceIdentity,
        assertCleanupOwner: async () => {},
        saveCleanupEvidence: async (evidence) => {
          active.providerCleanup = structuredClone(evidence);
          cleanupEvidence.push(structuredClone(evidence));
        },
      }),
    ).resolves.toMatchObject({ status: "pending" });
    expect(cleanupEvidence.at(-1)).toMatchObject({ provider: "koyeb", serviceID: targetServiceID });
    expect(
      routedRequests.some(
        (request) =>
          request.method === "DELETE" &&
          new URL(request.url).pathname === `/v1/services/${targetServiceID}`,
      ),
    ).toBe(true);
    expect(
      routedRequests
        .filter((request) => new URL(request.url).pathname.startsWith("/koyeb-sandbox/"))
        .every((request) => new URL(request.url).hostname === targetHost),
    ).toBe(true);
  });

  it("keeps a frozen target stable across registry reordering and additive reload", async () => {
    const original = managedAppPoolEnv();
    const fixture = capacityFixture();
    const prepared = await new KoyebResumableProvisioning(original, fixture.fetcher).prepare(
      meshConfig(),
      lease(),
    );
    const frozen = prepared.candidates![1]!;
    const originalScope = frozen.lease.providerScope;
    const expanded = managedAppPoolEnv({
      CRABBOX_KOYEB_APP_TARGETS: JSON.stringify([
        {
          organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
          appID: thirdAppID,
          appName: thirdAppName,
          region: "was",
        },
        {
          organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
          appID: secondAppID,
          appName: secondAppName,
          region: "was",
        },
        {
          organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
          appID: baseEnv.CRABBOX_KOYEB_APP_ID,
          appName,
          region: "was",
        },
      ]),
    });
    expect(koyebRegisteredAppIDs(expanded)).toEqual([
      thirdAppID,
      secondAppID,
      baseEnv.CRABBOX_KOYEB_APP_ID,
    ]);
    const reloaded = new KoyebResumableProvisioning(expanded, fixture.fetcher);
    await expect(
      reloaded.advance({
        plan: frozen.plan,
        step: frozen.step,
        lease: lease({ ...frozen.lease }),
        deadline: Date.now() + 60_000,
        recovering: false,
        canceled: true,
      }),
    ).resolves.toMatchObject({ phase: "terminal" });
    const reloadedClient = new KoyebClient(expanded, fixture.fetcher, secondAppID);
    await expect(reloadedClient.providerScope()).resolves.toBe(originalScope);
    expect(reloadedClient.region).toBe("was");
    expect(reloadedClient.privateHost(serviceName)).toBe(
      `${serviceName}.${secondAppName}.internal`,
    );
  });

  it("rejects a frozen lease when its registered target is removed or renamed", async () => {
    const fixture = capacityFixture();
    const prepared = await new KoyebResumableProvisioning(
      managedAppPoolEnv(),
      fixture.fetcher,
    ).prepare(meshConfig(), lease());
    const frozen = prepared.candidates![1]!;
    const input = {
      plan: frozen.plan,
      step: frozen.step,
      lease: lease({ ...frozen.lease }),
      deadline: Date.now() + 60_000,
      recovering: false,
      canceled: true as const,
    };
    const removed = managedAppPoolEnv({
      CRABBOX_KOYEB_APP_TARGETS: JSON.stringify([
        {
          organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
          appID: baseEnv.CRABBOX_KOYEB_APP_ID,
          appName,
          region: "was",
        },
      ]),
    });
    await expect(
      new KoyebResumableProvisioning(removed, fixture.fetcher).advance(input),
    ).rejects.toThrow("Koyeb provisioning target registration is missing");

    const renamed = managedAppPoolEnv({
      CRABBOX_KOYEB_APP_TARGETS: JSON.stringify([
        {
          organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
          appID: baseEnv.CRABBOX_KOYEB_APP_ID,
          appName,
          region: "was",
        },
        {
          organizationID: baseEnv.CRABBOX_KOYEB_ORGANIZATION_ID,
          appID: secondAppID,
          appName: "renamed-workers",
          region: "was",
        },
      ]),
    });
    await expect(
      new KoyebResumableProvisioning(renamed, fixture.fetcher).advance(input),
    ).rejects.toThrow("Koyeb provisioning plan is invalid or belongs to another context");
  });

  it("rejects a registered target whose live app identity drifted", async () => {
    await expect(
      managedCapacityCandidates({
        appNames: {
          [baseEnv.CRABBOX_KOYEB_APP_ID!]: appName,
          [secondAppID]: "renamed-workers",
        },
      }),
    ).rejects.toThrow("Koyeb app target identity does not match its registration");
  });

  it("rejects a lease routed through a different explicit provider target", async () => {
    const env = managedAppPoolEnv();
    const secondScope = await new KoyebClient(
      env,
      vi.fn<typeof fetch>(),
      secondAppID,
    ).providerScope();
    const selected = lease({
      providerProject: secondAppID,
      providerScope: secondScope,
      region: "was",
    });
    const provider = new KoyebProvider(env, vi.fn<typeof fetch>(), baseEnv.CRABBOX_KOYEB_APP_ID);

    expect(() =>
      provider.releaseLease(selected, {
        assertCleanupOwner: async () => {},
        saveCleanupEvidence: async () => {},
      }),
    ).toThrow("Koyeb lease target does not match the selected provider context");
  });

  it("releases canonical capacity after native mesh cleanup clears access", async () => {
    const cleanup = await activeCleanupFixture("koyeb-mesh");
    cleanup.state.observe = () => new Response("not found", { status: 404 });
    await expect(cleanup.advance()).resolves.toBeUndefined();
    expect(cleanup.active.providerCleanup).toMatchObject({
      provider: "koyeb",
      confirmation: { method: "service-absent" },
    });
    const canonical = lease({
      ...cleanup.active,
      state: "released",
      cleanupCompletedAt: new Date().toISOString(),
      provisioningResourceMayExist: false,
      host: "",
    });
    const storage = new ProvisioningTestStorage();
    await storage.put(`lease:${canonical.id}`, canonical);
    const { capability, candidates } = await managedCapacityCandidates({
      quotas: {
        services_by_app: "1",
        services: "1",
        service_provisioning_concurrency: "1",
        memory_mb: "4096",
        max_instances_by_type: { large: "1" },
      },
    });
    await expect(capability.selectAdmission(storage, candidates, lease())).resolves.toBe(0);
  });

  it("preserves legacy cleanup proof after registry expansion and releases only canonical capacity", async () => {
    const cleanup = await activeCleanupFixture();
    cleanup.state.observe = () => new Response("not found", { status: 404 });
    await expect(cleanup.advance()).resolves.toBeUndefined();
    const completedAt = "2026-09-08T12:06:00.000Z";
    const canonical = lease({
      ...cleanup.active,
      state: "released",
      cleanupCompletedAt: completedAt,
      provisioningResourceMayExist: false,
      host: "",
    });
    const noResource = lease({
      id: "cbx_noresource001",
      state: "failed",
      cloudID: "",
      providerProject: baseEnv.CRABBOX_KOYEB_APP_ID,
      providerScope: await new KoyebClient(
        managedAppPoolEnv(),
        vi.fn<typeof fetch>(),
      ).providerScope(),
      region: "was",
      provisioningResourceMayExist: false,
    });
    const storage = new ProvisioningTestStorage();
    await storage.put(`lease:${canonical.id}`, canonical);
    await storage.put(`lease:${noResource.id}`, noResource);
    const { capability, candidates } = await managedCapacityCandidates({
      quotas: {
        services_by_app: "1",
        services: "1",
        service_provisioning_concurrency: "1",
        memory_mb: "4096",
        max_instances_by_type: { large: "1" },
      },
    });
    await expect(capability.selectAdmission(storage, candidates, lease())).resolves.toBe(0);

    const stale = lease({
      ...noResource,
      id: "cbx_staleevidence1",
      provisioningRequestStartedAt: "2026-09-08T12:05:00.000Z",
    });
    await storage.put(`lease:${stale.id}`, stale);
    await expect(capability.selectAdmission(storage, candidates, lease())).rejects.toThrow(
      "Koyeb organization capacity is exhausted",
    );
  });

  it("isolates retired app reservations while retaining their aggregate capacity charge", async () => {
    const storage = new ProvisioningTestStorage();
    const retired = {
      providerProject: thirdAppID,
      providerScope: `koyeb:context:v1:${"c".repeat(64)}`,
      region: "was",
      serverType: "large",
    };
    const cleaned = lease({
      ...retired,
      id: "cbx_retiredclean01",
      state: "released",
      cloudID: "77777777-7777-4777-8777-777777777777",
      cleanupCompletedAt: "2026-09-08T12:06:00.000Z",
      provisioningResourceMayExist: false,
      host: "",
    });
    const unresolved = lease({
      ...retired,
      id: "cbx_retiredlive001",
      state: "active",
      cloudID: "88888888-8888-4888-8888-888888888888",
    });
    await storage.put(`lease:${cleaned.id}`, cleaned);
    await storage.put(`lease:${unresolved.id}`, unresolved);
    const { capability, candidates } = await managedCapacityCandidates({
      quotas: {
        services_by_app: "1",
        services: "2",
        service_provisioning_concurrency: "2",
        memory_mb: "8192",
        max_instances_by_type: { large: "2" },
      },
    });

    await expect(capability.selectAdmission(storage, candidates, lease())).resolves.toBe(0);
  });
});

describe("Koyeb Sandbox coordinator adapter", () => {
  it("owns Koyeb request restrictions and configured runner defaults", async () => {
    const provider = new KoyebProvider(
      { ...baseEnv, CRABBOX_KOYEB_INSTANCE_TYPE: "medium" },
      vi.fn<typeof fetch>(),
    );
    const request = {
      provider: "koyeb" as const,
      sshPublicKey: "ssh-ed25519 test",
      desktop: true,
      browser: true,
      code: true,
    };
    await expect(provider.prepareLeaseConfig(leaseConfig(request), request)).resolves.toMatchObject(
      {
        serverType: "medium",
        sshUser: "crabbox",
        sshPort: "22",
        sshFallbackPorts: [],
        workRoot: "/workspace/crabbox",
        tailscale: true,
      },
    );
    const mesh = { ...request, tailscale: false };
    await expect(provider.prepareLeaseConfig(leaseConfig(mesh), mesh)).resolves.toMatchObject({
      tailscale: false,
    });

    const explicitType = { ...request, serverType: "small", serverTypeExplicit: true };
    await expect(
      provider.prepareLeaseConfig(leaseConfig(explicitType), explicitType),
    ).rejects.toThrow("takes CPU and memory from the coordinator Koyeb instance configuration");
    const exitNode = { ...request, tailscaleExitNode: "100.64.0.1" };
    await expect(provider.prepareLeaseConfig(leaseConfig(exitNode), exitNode)).rejects.toThrow(
      "does not support a Tailscale exit node",
    );
  });

  it.each([
    ["app", { KOYEB_APP_ID: "55555555-5555-4555-8555-555555555555" }],
    ["organization", { KOYEB_ORGANIZATION_ID: "55555555-5555-4555-8555-555555555555" }],
    ["region", { KOYEB_REGION: "fra" }],
  ])("rejects private mesh when the runtime Koyeb %s identity differs", async (_label, drift) => {
    const capability = new KoyebResumableProvisioning(
      { ...baseEnv, ...drift },
      vi.fn<typeof fetch>(),
    );

    await expect(capability.prepare(meshConfig(), lease())).rejects.toThrow(
      "Koyeb durable provisioning configuration unsupported",
    );
  });

  it("rejects private mesh when the runtime app name differs from its registered target", () => {
    expect(
      () =>
        new KoyebResumableProvisioning(
          managedAppPoolEnv({ KOYEB_APP_NAME: "different-app" }),
          vi.fn<typeof fetch>(),
        ),
    ).toThrow("invalid Koyeb app target registry");
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
        { port: 3031, protocol: "tcp" },
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

    await expect(
      client.deleteOwnedService(active, {
        resourceIdentity: cleanupResourceIdentity(active),
        assertCleanupOwner: async () => {},
        saveCleanupEvidence: async (evidence) => {
          active.providerCleanup = evidence;
        },
      }),
    ).resolves.toBeUndefined();
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

  it("blocks named discovery when another service on a later page has the same lease name", async () => {
    const requests: Request[] = [];
    let providerSecret = "";
    const capability = new KoyebResumableProvisioning(baseEnv, async (request) => {
      const incoming = request instanceof Request ? request : new Request(request);
      requests.push(incoming);
      const url = new URL(incoming.url);
      if (incoming.method === "GET" && url.pathname === "/v1/services") {
        const offset = Number(url.searchParams.get("offset"));
        return Response.json({
          services: [service(offset === 0 ? {} : { id: latestDeploymentID })],
          count: 2,
          offset,
        });
      }
      if (incoming.method === "GET" && url.pathname === `/v1/deployments/${deploymentID}`) {
        return Response.json({ deployment: deployment(providerSecret) });
      }
      throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
    });
    const prepared = await capability.prepare(config(), lease());
    providerSecret = prepared.material.providerSecret;

    await expect(
      capability.advance(advanceInput(prepared, prepared.step, false)),
    ).resolves.toMatchObject({
      phase: "blocked",
      blockedReason: "identity_resolution_required",
    });
    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ["GET", "/v1/services"],
      ["GET", "/v1/services"],
    ]);
    expect(requests.map((request) => new URL(request.url).searchParams.get("name"))).toEqual([
      serviceName,
      serviceName,
    ]);
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

  it("recognizes Koyeb-canonicalized legacy mesh ports so failed leases remain reclaimable", async () => {
    const capability = new KoyebResumableProvisioning(baseEnv, async (request) => {
      const incoming = request instanceof Request ? request : new Request(request);
      const url = new URL(incoming.url);
      if (url.pathname === `/v1/services/${serviceID}`) {
        return Response.json({ service: service() });
      }
      if (url.pathname === `/v1/deployments/${deploymentID}`) {
        const legacy = meshDeployment("u".repeat(32));
        legacy.definition.ports = [
          { port: 22, protocol: "tcp" },
          { port: 3030, protocol: "http" },
        ];
        return Response.json({ deployment: legacy });
      }
      throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
    });
    const prepared = await capability.prepare(meshConfig(), lease());
    prepared.material.providerSecret = "u".repeat(32);
    const observed = await capability.advance(
      advanceInput(
        prepared,
        {
          ...prepared.step,
          phase: "cleanup",
          state: { version: 1, action: "observe", serviceID },
        },
        true,
      ),
    );

    expect(observed).toMatchObject({
      phase: "cleanup",
      state: { action: "delete", serviceID, deploymentID },
    });
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

  it("bootstraps privately over the Koyeb mesh and publishes proxied key-only SSH", async () => {
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
      if (incoming.method === "GET" && url.pathname === "/health") {
        return Response.json({ ok: true });
      }
      if (incoming.method === "POST" && url.pathname === "/write_file") {
        return Response.json({ ok: true });
      }
      if (incoming.method === "POST" && url.pathname === "/run") {
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
      if (incoming.method === "POST" && url.pathname === "/bind_port") {
        return Response.json({ success: true, message: "Port binding configured", port: "22" });
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
          sshPort: "3031",
          sshFallbackPorts: [],
          workRoot: "/workspace/crabbox",
          sshHostKey: sshHostKey.split(" ", 2).join(" "),
        },
      },
    });
    expect(published.publication?.access).not.toHaveProperty("tailscale");
    expect(
      managementRequests.map((request) => [
        new URL(request.url).origin,
        new URL(request.url).pathname,
      ]),
    ).toEqual([
      [`http://${privateHost}:3030`, "/health"],
      [`http://${privateHost}:3030`, "/write_file"],
      [`http://${privateHost}:3030`, "/run"],
      [`http://${privateHost}:3030`, "/bind_port"],
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
    await expect(managementRequests[3]!.json()).resolves.toEqual({ port: "22" });
    expect(JSON.stringify(published)).not.toContain(material.providerSecret);
  });

  it("retries bootstrap while the private Sandbox endpoint is becoming reachable", async () => {
    const providerSecret = "v".repeat(32);
    const managementPaths: string[] = [];
    let failFirstHealth = true;
    const warning = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const capability = new KoyebResumableProvisioning(baseEnv, async (request) => {
      const incoming = request instanceof Request ? request.clone() : new Request(request);
      const url = new URL(incoming.url);
      if (url.origin === "https://koyeb.example") {
        if (url.pathname === `/v1/services/${serviceID}`) {
          return Response.json({ service: service() });
        }
        if (url.pathname === `/v1/deployments/${deploymentID}`) {
          return Response.json({ deployment: meshDeployment(providerSecret) });
        }
      }
      managementPaths.push(url.pathname);
      if (incoming.method === "GET" && url.pathname === "/health") {
        if (failFirstHealth) {
          failFirstHealth = false;
          throw new Error(`getaddrinfo ENOTFOUND ${url.hostname} ${providerSecret}`);
        }
        return Response.json({ ok: true });
      }
      if (incoming.method === "POST" && url.pathname === "/write_file") {
        return Response.json({ ok: true });
      }
      if (incoming.method === "POST" && url.pathname === "/run") {
        return Response.json({
          stdout: JSON.stringify({
            schema: "crabbox-koyeb-sandbox-runner/v2",
            leaseId: serviceName,
            ssh: { user: "crabbox", host: privateHost, port: 22, hostKey: sshHostKey },
            network: { transport: "koyeb-mesh", privateHost },
          }),
          stderr: "",
          code: 0,
        });
      }
      if (incoming.method === "POST" && url.pathname === "/bind_port") {
        return Response.json({ success: true, message: "Port binding configured", port: "22" });
      }
      throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
    });
    const prepared = await capability.prepare(meshConfig(), lease());
    prepared.material.providerSecret = providerSecret;
    const bootstrapStep: ProvisioningStep = {
      ...prepared.step,
      phase: "provisioning",
      state: { version: 1, action: "bootstrap", serviceID, deploymentID },
    };

    const deferred = await capability.advance(advanceInput(prepared, bootstrapStep, true));
    expect(deferred).toMatchObject({
      phase: "provisioning",
      state: { action: "bootstrap", serviceID, deploymentID },
    });
    expect(warning).toHaveBeenCalledWith("koyeb sandbox bootstrap deferred", {
      phase: "bootstrap",
      category: "transport_unavailable",
    });
    expect(JSON.stringify(warning.mock.calls)).not.toContain(providerSecret);

    const published = await capability.advance(advanceInput(prepared, deferred, true));
    expect(published).toMatchObject({ phase: "ready-to-publish" });
    expect(managementPaths).toEqual(["/health", "/health", "/write_file", "/run", "/bind_port"]);
    warning.mockRestore();
  });

  it("treats only an exact existing Sandbox TCP proxy binding as idempotent", async () => {
    const sandboxSecret = "synthetic-sandbox-secret-value-xx";
    const exact = new KoyebClient(baseEnv, async () =>
      Response.json(
        { success: false, error: "Port already bound", current_port: "22" },
        { status: 409 },
      ),
    );
    await expect(
      exact.managementBindPort(`http://${privateHost}:3030`, undefined, sandboxSecret, "22"),
    ).resolves.toBeUndefined();

    const drifted = new KoyebClient(baseEnv, async () =>
      Response.json(
        { success: false, error: "Port already bound", current_port: "5900" },
        { status: 409 },
      ),
    );
    await expect(
      drifted.managementBindPort(`http://${privateHost}:3030`, undefined, sandboxSecret, "22"),
    ).rejects.toBeInstanceOf(KoyebHTTPError);
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
      assertCleanupOwner: vi.fn<() => Promise<void>>(async () => {}),
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

  it.each(["valid", "expired", "replaced", "missing"])(
    "checks %s authority after ownership reads without advancing a refused delete",
    async (authority) => {
      const requests: Request[] = [];
      let checkedRead = false;
      let revoked = false;
      const capability = new KoyebResumableProvisioning(baseEnv, async (input) => {
        const request = input instanceof Request ? input : new Request(input);
        requests.push(request);
        const url = new URL(request.url);
        if (request.method === "DELETE") return Response.json({});
        if (url.pathname === `/v1/services/${serviceID}`)
          return Response.json({ service: service() });
        if (url.pathname === `/v1/deployments/${deploymentID}`) {
          await Promise.resolve();
          checkedRead = true;
          revoked = authority !== "valid";
          return Response.json({ deployment: deployment("u".repeat(32)) });
        }
        throw new Error("unexpected request");
      });
      const prepared = await capability.prepare(config(), lease());
      const step: ProvisioningStep = {
        ...prepared.step,
        phase: "cleanup",
        state: { version: 1, action: "delete", serviceID, deploymentID },
      };
      const assertCleanupOwner = vi.fn<() => Promise<void>>(async () => {
        expect(checkedRead).toBe(true);
        if (revoked) throw new Error(`${authority} cleanup claim`);
      });
      const result = capability.advance({
        plan: prepared.plan,
        step,
        lease: lease({ providerScope: prepared.plan.scope }),
        deadline: Date.now() - 1,
        recovering: true,
        canceled: true,
        assertCleanupOwner: authority === "missing" ? undefined : assertCleanupOwner,
      });
      const outcome = await result.then(
        (value) => ({ state: value.state }),
        (error: Error) => ({ error: error.message }),
      );
      expect(outcome).toMatchObject(
        authority === "valid"
          ? { state: { action: "confirm-delete" } }
          : {
              error:
                authority === "missing"
                  ? "Koyeb cleanup requires current coordinator authority"
                  : `${authority} cleanup claim`,
            },
      );
      expect(checkedRead).toBe(true);
      expect(requests.filter((request) => request.method === "DELETE")).toHaveLength(
        authority === "valid" ? 1 : 0,
      );
      expect(step.state).toMatchObject({ action: "delete" });
    },
  );

  it.each([
    ["service", latestDeploymentID, deploymentID],
    ["deployment", serviceID, latestDeploymentID],
  ] as const)(
    "refuses cleanup when an exact %s lookup returns another resource",
    async (identity, returnedServiceID, returnedDeploymentID) => {
      const methods: string[] = [];
      const capability = new KoyebResumableProvisioning(baseEnv, async (request) => {
        const incoming = request instanceof Request ? request : new Request(request);
        const url = new URL(incoming.url);
        methods.push(`${incoming.method} ${url.pathname}`);
        if (url.pathname === `/v1/services/${serviceID}`) {
          return Response.json({ service: service({ id: returnedServiceID }) });
        }
        if (url.pathname === `/v1/deployments/${deploymentID}`) {
          return Response.json({
            deployment: deployment("u".repeat(32), {
              id: returnedDeploymentID,
              service_id: returnedServiceID,
            }),
          });
        }
        throw new Error(`unexpected request ${incoming.method} ${incoming.url}`);
      });
      const prepared = await capability.prepare(config(), lease());

      const error = await capability
        .advance({
          plan: prepared.plan,
          step: {
            ...prepared.step,
            phase: "cleanup",
            state: { version: 1, action: "delete", serviceID },
          },
          lease: lease({ providerScope: prepared.plan.scope, cloudID: serviceID }),
          deadline: Date.now() + 60_000,
          recovering: true,
          canceled: true,
        })
        .then(
          () => undefined,
          (caught: unknown) => caught,
        );
      expect(methods.some((entry) => entry.startsWith("DELETE"))).toBe(false);
      expect(error).toBeInstanceOf(ProviderResourceUnresolvedError);
      expect(String(error)).toContain(
        `Koyeb ${identity} response identity does not match its request`,
      );
    },
  );

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

  it.each(["api fetch", "management body"] as const)(
    "bounds a stalled %s request and aborts its request signal",
    async (stage) => {
      vi.useFakeTimers();
      let observed: Request | undefined;
      const fetcher = vi.fn<typeof fetch>(async (input) => {
        observed = input instanceof Request ? input : new Request(input);
        if (stage === "management body") {
          return {
            ok: true,
            status: 200,
            text: () => new Promise<string>(() => {}),
          } as Response;
        }
        return await new Promise<Response>((_resolve, reject) => {
          observed!.signal.addEventListener("abort", () => reject(observed!.signal.reason), {
            once: true,
          });
        });
      });
      const client = new KoyebClient(baseEnv, fetcher);
      const pending = (
        stage === "api fetch"
          ? client.getService(serviceID)
          : client.managementHealth(
              "https://sandbox.example",
              "routing-key-one",
              "synthetic-sandbox-secret-value-xx",
            )
      ).catch((error: unknown) => error);

      await vi.runAllTimersAsync();

      await expect(pending).resolves.toMatchObject({ name: "KoyebHTTPError", status: 408 });
      expect(observed?.signal.aborted).toBe(true);
    },
  );
});

describe("Koyeb Fleet integration", () => {
  it("rechecks the retained publication after provider ownership reads before DELETE", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-09-08T12:05:00.000Z"));
    const fixture = await activeFleetCleanupFixture();
    fixture.state.beforeDeployment = async () => {
      const key = provisioningOperationKey(fixture.active.id);
      const operation = (await fixture.storage.get<LeaseProvisioningOperation>(key))!;
      operation.step.publication!.server.resourceIdentity = cleanupResourceIdentity(
        fixture.active,
        { idleTimeoutSeconds: 1_200 },
      );
      await fixture.storage.put(key, operation);
    };

    expect((await fixture.release()).status).toBe(200);
    const failed = await fixture.tick();

    expect(fixture.state.deletes).toBe(0);
    expect(failed?.providerCleanup).toBeUndefined();
    expect(failed?.cleanupError).toContain("Koyeb cleanup failed stage=ownership");
  });

  it.each([
    ["authentication", () => new Response("sensitive-untrusted-body", { status: 401 })],
    ["malformed observation", () => Response.json({})],
    ["unknown status", () => Response.json({ service: service({ status: "UNKNOWN" }) })],
    [
      "changed ownership",
      () => Response.json({ service: service({ app_id: latestDeploymentID }) }),
    ],
  ] as const)(
    "terminalizes %s during scheduled confirmation without another DELETE or retry",
    async (_label, observation) => {
      vi.useFakeTimers({ toFake: ["Date"] });
      vi.setSystemTime(new Date("2026-09-08T12:05:00.000Z"));
      const fixture = await activeFleetCleanupFixture();
      expect((await fixture.release()).status).toBe(200);
      const pending = await fixture.tick();
      await expect(fixture.publicLease()).resolves.toMatchObject({
        lease: { cleanupStatus: "pending" },
      });
      fixture.state.observe = observation;
      vi.setSystemTime(new Date(pending!.cleanupClaimExpiresAt!));
      const failed = await fixture.tick();
      expect(failed?.cleanupError).toBeDefined();
      expect(failed?.cleanupError).not.toContain("sensitive-untrusted-body");
      expect(failed?.cleanupFailedAt).toBeDefined();
      expect(failed?.cleanupRetryAt).toBeUndefined();
      expect(failed?.cleanupStartedAt).toBeUndefined();
      expect(failed?.cleanupCompletedAt).toBeUndefined();
      await expect(fixture.publicLease()).resolves.toMatchObject({
        lease: { cleanupStatus: "failed" },
      });
      const requests = fixture.state.requests.length;
      vi.setSystemTime(Date.now() + 600_000);
      await fixture.tick();
      expect(fixture.state.requests).toHaveLength(requests);
      expect(fixture.state.deletes).toBe(1);
    },
  );

  it("terminalizes exhausted scheduled confirmation without claiming completion or extending its deadline", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-09-08T12:05:00.000Z"));
    const fixture = await activeFleetCleanupFixture();
    expect((await fixture.release()).status).toBe(200);
    const pending = await fixture.tick();
    const evidence = pending!.providerCleanup!;
    if (evidence.provider !== "koyeb") throw new Error("wrong cleanup evidence");
    vi.setSystemTime(new Date(evidence.confirmationDeadline));
    const failed = await fixture.tick();
    expect(failed?.cleanupError).toContain("confirmation deadline exhausted");
    expect(failed?.cleanupCompletedAt).toBeUndefined();
    expect(failed?.cleanupRetryAt).toBeUndefined();
    expect(failed?.providerCleanup).toMatchObject({
      confirmationDeadline: evidence.confirmationDeadline,
    });
    await expect(fixture.publicLease()).resolves.toMatchObject({
      lease: { cleanupStatus: "failed" },
    });
    expect(fixture.state.deletes).toBe(1);
  });

  it.each([0, 4_000])(
    "keeps accepted active-lease deletion pending across scheduler restarts with %s ms persistence delay",
    async (persistenceDelay) => {
      vi.useFakeTimers({ toFake: ["Date"] });
      vi.setSystemTime(new Date("2026-09-08T12:05:00.000Z"));
      const storage = new ProvisioningTestStorage();
      const providerScope = await new KoyebClient(baseEnv, vi.fn<typeof fetch>()).providerScope();
      const active = lease({
        state: "active",
        cloudID: serviceID,
        region: "was",
        providerScope,
        org: orgKeyForLabel("example-org"),
        image: {
          id: runnerImage,
          source: "explicit",
          provider: "koyeb",
          kind: "koyeb-sandbox-runner",
          region: "was",
          sourceID: registrySecret,
        },
      });
      await storage.put(`lease:${active.id}`, active);
      await retainCleanupPublication(storage, active, cleanupResourceIdentity(active));
      const put = storage.put.bind(storage);
      vi.spyOn(storage, "put").mockImplementation(async (key, value) => {
        const record = value as LeaseRecord;
        if (
          record.providerCleanup?.provider === "koyeb" &&
          record.providerCleanup.lastObservation &&
          !record.providerCleanup.confirmation
        ) {
          vi.setSystemTime(Date.now() + persistenceDelay);
        }
        await put(key, value);
      });
      let deletes = 0;
      let absent = false;
      const fetcher = vi.fn<typeof fetch>(async (input) => {
        const incoming = input instanceof Request ? input : new Request(input);
        const path = new URL(incoming.url).pathname;
        if (path === `/v1/services/${serviceID}` && incoming.method === "DELETE") {
          deletes += 1;
          return Response.json({ service: service({ status: "DELETING" }) });
        }
        if (path === `/v1/services/${serviceID}`) {
          return absent
            ? new Response("not found", { status: 404 })
            : Response.json({ service: service({ status: deletes ? "DELETING" : "HEALTHY" }) });
        }
        if (path === `/v1/deployments/${deploymentID}`) {
          const owned = deployment("u".repeat(32));
          owned.definition.env = owned.definition.env.map((entry) =>
            entry.key === "CRABBOX_LEASE_ORG"
              ? { ...entry, value: providerLabelValue(active.org) }
              : entry,
          );
          return Response.json({ deployment: owned });
        }
        throw new Error(`unexpected request ${incoming.method} ${path}`);
      });
      const runtime = new ProvisioningTestRuntime(storage);
      const coordinator = new FleetCoordinator(runtime, fleetEnv, {
        koyeb: new KoyebProvider(fleetEnv, fetcher),
      });
      const response = await coordinator.fetch(
        fleetRequest("POST", `/v1/leases/${active.id}/release`, { delete: true }),
      );
      expect(response.status).toBe(200);
      await coordinator.alarm();
      await Promise.all(runtime.maintenance);
      const pending = await storage.get<LeaseRecord>(`lease:${active.id}`);
      expect(pending?.cleanupError).toBeUndefined();
      expect(pending?.cleanupFailedAt).toBeUndefined();
      expect(pending?.cleanupCompletedAt).toBeUndefined();
      const observed = await coordinator.fetch(fleetRequest("GET", `/v1/leases/${active.id}`));
      await expect(observed.json()).resolves.toMatchObject({ lease: { cleanupStatus: "pending" } });
      expect(deletes).toBe(1);

      const resumedRuntime = new ProvisioningTestRuntime(storage);
      const reconstructed = new FleetCoordinator(resumedRuntime, fleetEnv, {
        koyeb: new KoyebProvider(fleetEnv, fetcher),
      });
      vi.setSystemTime(new Date(pending!.cleanupClaimExpiresAt!));
      await reconstructed.alarm();
      await Promise.all(resumedRuntime.maintenance);
      expect(deletes).toBe(1);
      expect((await storage.get<LeaseRecord>(`lease:${active.id}`))?.cleanupError).toBeUndefined();
      absent = true;
      const next = await storage.get<LeaseRecord>(`lease:${active.id}`);
      vi.setSystemTime(new Date(next!.cleanupClaimExpiresAt!));
      await reconstructed.alarm();
      await Promise.all(resumedRuntime.maintenance);
      expect(deletes).toBe(1);
      const finalResponse = await reconstructed.fetch(
        fleetRequest("GET", `/v1/leases/${active.id}`),
      );
      await expect(finalResponse.json()).resolves.toMatchObject({
        lease: { cleanupStatus: "complete" },
      });
    },
  );

  it("runs scheduled maintenance with Koyeb as the workspace provider", async () => {
    const coordinator = new FleetCoordinator(
      new ProvisioningTestRuntime(new ProvisioningTestStorage()),
      {
        ...fleetEnv,
        CRABBOX_WORKSPACE_PROVIDER: "koyeb",
      },
    ) as unknown as {
      runScheduledMaintenance: () => Promise<void>;
    };

    await expect(coordinator.runScheduledMaintenance()).resolves.toBeUndefined();
  });

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

  it("includes owned Koyeb sandboxes beyond the first inventory page in the provider pool", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (requestInput: RequestInfo | URL, init?: RequestInit) => {
        const incoming =
          requestInput instanceof Request ? requestInput : new Request(requestInput, init);
        const url = new URL(incoming.url);
        if (url.pathname === "/v1/services") {
          const offset = Number(url.searchParams.get("offset"));
          return Response.json({
            services:
              offset === 0
                ? inventoryServices(100).map((entry) => ({
                    ...entry,
                    active_deployment_id: "",
                    latest_deployment_id: "",
                  }))
                : [service()],
            count: 101,
            offset,
            limit: 100,
            has_next: offset === 0,
          });
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
