/* oxlint-disable eslint/no-await-in-loop -- These tests advance committed phases sequentially and reconstruct between deliveries. */
import { afterEach, describe, expect, it, vi } from "vitest";

import { azureOwnedDeleteClaimKey } from "../src/azure";
import { AzureResumableProvisioning, AzureProvisioningTransport } from "../src/azure-provisioning";
import { leaseConfig } from "../src/config";
import { AzureProvider, FleetCoordinator } from "../src/fleet";
import {
  provisioningOperationKey,
  provisioningDueKey,
  provisioningPlanKey,
  type LeaseProvisioningOperation,
} from "../src/lease-provisioning";
import { orgKeyForLabel } from "../src/org-identity";
import {
  openProvisioningMaterial,
  provisioningMaterialKey,
  sealProvisioningMaterial,
} from "../src/provisioning-material";
import type { Env, LeaseRecord } from "../src/types";
import { ProvisioningTestRuntime, ProvisioningTestStorage } from "./provisioning-fixtures";

const env = {
  FLEET: {},
  AZURE_TENANT_ID: "tenant",
  AZURE_CLIENT_ID: "client",
  AZURE_CLIENT_SECRET: "synthetic-provider-secret",
  AZURE_SUBSCRIPTION_ID: "sub",
  CRABBOX_SESSION_SECRET: "synthetic-session-secret-with-32-characters",
  CRABBOX_DURABLE_PROVISIONING_ADMISSION: "true",
  CRABBOX_AZURE_OS_DISK: "managed",
} as Env;
const id = "cbx_0123456789ab";
const org = orgKeyForLabel("example-org");
const scope = "/subscriptions/sub/resourceGroups/crabbox-leases";

class AzureFixture {
  readonly resources = new Map<string, Record<string, unknown>>();
  readonly reads: string[] = [];
  readonly mutations: Array<{
    method: string;
    path: string;
    body: Record<string, unknown>;
    headers: Headers;
  }> = [];
  loseResponseFor?: string;
  afterMutation?: (path: string, method: string) => void | Promise<void>;
  beforeMutation?: (path: string, method: string) => void | Promise<void>;
  deleteFailures: string[] = [];
  asyncDelete = false;
  readonly operations = new Map<string, string>();
  rejectFirstVM = false;
  galleryVersions: unknown[] = [];
  private sequence = 0;
  remove(path: string): void {
    this.resources.delete(path);
    if (/\/virtualMachines\/[^/]+$/.test(path)) {
      for (const key of this.resources.keys())
        if (key.startsWith(`${path}/extensions/`)) this.resources.delete(key);
      const baseName = path.split("/").at(-1)!;
      const nic = this.resources.get(
        `${scope}/providers/Microsoft.Network/networkInterfaces/${baseName}-nic`,
      );
      if (nic) delete (nic["properties"] as Record<string, unknown>)["virtualMachine"];
      const disk = this.resources.get(
        `${scope}/providers/Microsoft.Compute/disks/${baseName}-osdisk`,
      );
      if (disk) delete disk["managedBy"];
    }
    if (path.includes("/networkInterfaces/")) {
      const pip = this.resources.get(
        path.replace("networkInterfaces/", "publicIPAddresses/").replace(/-nic$/, "-pip"),
      );
      if (pip) delete (pip["properties"] as Record<string, unknown>)["ipConfiguration"];
    }
  }
  readonly fetch: typeof fetch = async (fetchInput, init) => {
    const url = new URL(String(fetchInput));
    if (url.hostname === "login.microsoftonline.com")
      return Response.json({ access_token: "synthetic-access-token" });
    const path = url.pathname;
    const method = init?.method ?? "GET";
    if (method === "GET") this.reads.push(path);
    if (path.includes("/operations/"))
      return Response.json({ status: this.operations.get(path) ?? "InProgress" });
    if (path.endsWith("/versions"))
      return path.includes("/galleries/")
        ? Response.json({ value: this.galleryVersions })
        : Response.json([{ name: "20348.2608.240906" }]);
    if (method === "GET")
      return this.resources.has(path)
        ? Response.json(this.resources.get(path))
        : Response.json(
            { error: { code: "ResourceNotFound", message: `Resource ${path} was not found` } },
            { status: 404 },
          );
    const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : {};
    await this.beforeMutation?.(path, method);
    this.mutations.push({
      method,
      path,
      body: structuredClone(body),
      headers: new Headers(init?.headers),
    });
    const properties = (body["properties"] ?? {}) as Record<string, unknown>;
    if (method === "DELETE") {
      const failure = this.deleteFailures.shift();
      if (failure) return Response.json({ error: { code: failure } }, { status: 409 });
      this.remove(path);
      await this.afterMutation?.(path, method);
      if (this.asyncDelete) {
        const operationPath = `/subscriptions/sub/providers/Microsoft.Compute/locations/eastus/operations/delete-${this.operations.size}`;
        this.operations.set(operationPath, "InProgress");
        return new Response(null, {
          status: 202,
          headers: { "azure-asyncoperation": `https://management.azure.com${operationPath}` },
        });
      }
      return new Response(null, { status: 204 });
    }
    if (method === "PATCH") {
      const existing = this.resources.get(path)!;
      this.resources.set(path, { ...existing, ...body });
    } else {
      const vm = /\/virtualMachines\/[^/]+$/.test(path);
      if (vm && this.rejectFirstVM) {
        this.rejectFirstVM = false;
        return Response.json({ error: { code: "SkuNotAvailable" } }, { status: 400 });
      }
      if (vm && this.resources.has(path)) return Response.json({}, { status: 412 });
      const resource = {
        id: path,
        name: path.split("/").at(-1),
        ...body,
        properties: {
          ...properties,
          provisioningState: "Succeeded",
          ...(vm
            ? { vmId: `vm-${++this.sequence}` }
            : { resourceGuid: `network-${++this.sequence}` }),
        },
      };
      if (path.includes("/publicIPAddresses/"))
        Object.assign(resource.properties, { ipAddress: "192.0.2.10" });
      this.resources.set(path, resource);
      if (path.includes("/networkInterfaces/")) {
        for (const config of resource.properties["ipConfigurations"] as Record<string, unknown>[])
          config["id"] = `${path}/ipConfigurations/${config["name"]}`;
        const pipPath = path
          .replace("networkInterfaces/", "publicIPAddresses/")
          .replace(/-nic$/, "-pip");
        Object.assign(this.resources.get(pipPath)!["properties"] as object, {
          ipConfiguration: { id: `${path}/ipConfigurations/ipconfig` },
        });
      }
      if (vm) {
        const name = path.split("/").at(-1)!;
        const diskPath = `${scope}/providers/Microsoft.Compute/disks/${name}-osdisk`;
        const storage = resource.properties["storageProfile"] as Record<
          string,
          Record<string, unknown>
        >;
        (storage["osDisk"]!["managedDisk"] as Record<string, unknown>)["id"] = diskPath;
        this.resources.set(diskPath, {
          id: diskPath,
          name: `${name}-osdisk`,
          location: body["location"],
          managedBy: path,
          properties: { uniqueId: `disk-${++this.sequence}`, provisioningState: "Succeeded" },
        });
        const nicPath = `${scope}/providers/Microsoft.Network/networkInterfaces/${name}-nic`;
        Object.assign(this.resources.get(nicPath)!["properties"] as object, {
          virtualMachine: { id: path },
        });
      }
    }
    const responseResource = structuredClone(this.resources.get(path));
    await this.afterMutation?.(path, method);
    if (this.loseResponseFor && path.includes(this.loseResponseFor)) {
      this.loseResponseFor = undefined;
      throw new Error("synthetic lost acknowledgement");
    }
    return Response.json(responseResource, {
      status: 201,
      ...(path.includes("/extensions/")
        ? {
            headers: {
              "azure-asyncoperation":
                "https://management.azure.com/subscriptions/sub/providers/Microsoft.Compute/locations/eastus/operations/extension?api-version=2024-07-01",
            },
          }
        : {}),
    });
  };
}

function request(method = "POST", path = "/v1/leases", body?: unknown): Request {
  return new Request(`https://coordinator.test${path}`, {
    method,
    headers: {
      "content-type": "application/json",
      prefer: "respond-async",
      "x-crabbox-owner": "alice@example.com",
      "x-crabbox-org": org,
      "x-crabbox-admin": "true",
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

function input() {
  return {
    leaseID: id,
    createAttemptID: "cat_0123456789abcdef0123456789abcdef",
    provider: "azure",
    target: "windows",
    windowsMode: "normal",
    architecture: "amd64",
    azureOSDisk: "managed",
    serverType: "Standard_D4s_v5",
    serverTypeExplicit: true,
    sshPublicKey:
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJgQC1DxrVMv3zLzppzo7SYTts53cylRcEuPEikSyaK1 fixture",
    ttlSeconds: 7200,
    idleTimeoutSeconds: 3600,
  };
}

function fleet(
  storage: ProvisioningTestStorage,
  fixture: AzureFixture,
  overrides: Partial<Env> = {},
) {
  const currentEnv = { ...env, ...overrides };
  const provider = new AzureProvider(currentEnv);
  provider.resumableProvisioning = () =>
    new AzureResumableProvisioning(currentEnv, fixture.fetch, storage);
  provider.createServerWithFallback = async () => {
    throw new Error("legacy allocation is outside this fixture");
  };
  const runtime = new ProvisioningTestRuntime(storage);
  return {
    coordinator: new FleetCoordinator(runtime, currentEnv, { azure: provider }),
    runtime,
    provider,
  };
}

async function step(
  storage: ProvisioningTestStorage,
  fixture: AzureFixture,
  overrides: Partial<Env> = {},
) {
  const operation = await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id));
  if (!operation || operation.step.phase === "terminal") return;
  vi.setSystemTime(Math.max(Date.now(), operation.step.nextWake + 1));
  const fresh = fleet(storage, fixture, overrides);
  await fresh.runtime.tick!();
}

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

async function ready(storage: ProvisioningTestStorage, azure: AzureFixture) {
  await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input()));
  for (let n = 0; n < 40; n++) {
    const operation = await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id));
    if (operation?.step.phase === "ready-to-publish") return operation;
    await step(storage, azure);
  }
  throw new Error("fixture did not reach ready-to-publish");
}

async function publicLease(storage: ProvisioningTestStorage, azure: AzureFixture) {
  const response = await fleet(storage, azure).coordinator.fetch(
    request("GET", `/v1/leases/${id}`),
  );
  expect(response.status).toBe(200);
  return ((await response.json()) as { lease: LeaseRecord & { cleanupStatus: string } }).lease;
}

describe("durable Azure admission and reconstruction", () => {
  it("persists the requested market with the initial provisioning lease", async () => {
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();

    const response = await fleet(storage, azure).coordinator.fetch(
      request("POST", "/v1/leases", {
        ...input(),
        capacity: { market: "on-demand", fallback: "none" },
      }),
    );

    expect(response.status).toBe(202);
    expect(await storage.get<LeaseRecord>(`lease:${id}`)).toMatchObject({
      state: "provisioning",
      market: "on-demand",
    });
    expect(azure.mutations).toHaveLength(0);
  });

  it("removes stale due markers without starving a valid operation", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input()));
    for (let n = 0; n < 4; n++)
      await storage.put(`provisioning-due:000000000000000${n}:missing-${n}`, {
        operationID: `missing-${n}`,
        at: n,
      });
    await step(storage, azure);
    expect((await storage.list({ prefix: "provisioning-due:" })).size).toBe(1);
    await step(storage, azure);
    expect(
      (await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id)))?.revision,
    ).toBeGreaterThan(0);
  });

  it.each(["operation-schema", "journal-schema", "journal-revision"])(
    "quarantines %s without rewriting evidence or issuing provider work",
    async (corruption) => {
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      const current = fleet(storage, azure);
      await current.coordinator.fetch(request("POST", "/v1/leases", input()));
      const operation = (await storage.get<LeaseProvisioningOperation>(
        provisioningOperationKey(id),
      ))!;
      const journalKey = (operation.step.state as { journal: string }).journal;
      const key = corruption === "operation-schema" ? provisioningOperationKey(id) : journalKey;
      const record = (await storage.get<Record<string, unknown>>(key))!;
      if (corruption === "journal-revision") record["revision"] = 999;
      else record["schema"] = 999;
      await storage.put(key, record);
      await current.runtime.tick!();
      expect(await storage.get(key)).toEqual(record);
      expect(await storage.get(`provisioning-quarantine:${id}`)).toBeDefined();
      expect(await storage.get(provisioningDueKey(operation))).toBeUndefined();
      expect(azure.mutations).toHaveLength(0);
      expect(await storage.get(provisioningMaterialKey(operation.operationID))).toBeDefined();
    },
  );

  it.each([false, true])(
    "releases the shared coordination lock after a settled early release (delete=%s)",
    async (deleteServer) => {
      vi.useFakeTimers({ toFake: ["Date"] });
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input()));
      await step(storage, azure);
      expect(await storage.get(`provisioning-lock:${scope.toLowerCase()}`)).toBeDefined();
      await fleet(storage, azure).coordinator.fetch(
        request("POST", `/v1/leases/${id}/release`, { delete: deleteServer }),
      );
      for (let n = 0; n < 10; n++) await step(storage, azure);
      expect(await storage.get(`provisioning-lock:${scope.toLowerCase()}`)).toBeUndefined();
      expect((await publicLease(storage, azure)).cleanupStatus).toBe(
        deleteServer ? "complete" : "retained",
      );
      expect(azure.mutations).toHaveLength(0);
    },
  );

  it.each(["tiny", "small", "standard", "fast", "large", "beast"])(
    "supports the normal Windows %s defaults and bounds configured region/market expansion",
    async (machineClass) => {
      const { serverType: _type, serverTypeExplicit: _explicit, ...body } = input();
      const config = leaseConfig({
        ...body,
        class: machineClass,
        capacity: {
          market: "spot",
          fallback: "on-demand",
          regions: ["westus", "centralus", "eastus2", "westus2", "northcentralus"],
        },
      });
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      const response = await fleet(storage, azure).coordinator.fetch(
        request("POST", "/v1/leases", {
          ...body,
          class: machineClass,
          capacity: { market: "spot", fallback: "on-demand", regions: config.capacityRegions },
        }),
      );
      expect(response.status).toBe(202);
      const operation = (await storage.get<LeaseProvisioningOperation>(
        provisioningOperationKey(id),
      ))!;
      const plan = await storage.get<{ resources: unknown[] }>(
        provisioningPlanKey(operation.operationID),
      );
      expect(plan?.resources).toHaveLength(60);
      expect(azure.mutations).toHaveLength(0);
      const capability = new AzureResumableProvisioning(env);
      expect(capability.supports(config)).toBe(true);
      expect(
        capability.supports({
          ...config,
          capacityRegions: [...config.capacityRegions, "southcentralus"],
        }),
      ).toBe(false);
    },
  );

  it("does not advertise oversized effective defaults as resumable", async () => {
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    const response = await fleet(storage, azure, {
      CRABBOX_AZURE_REGIONS: "westus,centralus,eastus2,westus2,northcentralus,southcentralus",
    }).coordinator.fetch(request("GET", "/v1/providers/azure/readiness?target=windows"));
    expect(response.status).toBe(200);
    expect(await response.json()).toMatchObject({
      resumableProvisioning: { supported: false, available: false, admissionEnabled: true },
    });
  });

  it("retains an explicitly released provisioning VM and later deletes the exact retained resources", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    await ready(storage, azure);
    const response = await fleet(storage, azure).coordinator.fetch(
      request("POST", `/v1/leases/${id}/release`, { delete: false }),
    );
    expect(response.status).toBe(200);
    expect(await response.json()).toMatchObject({
      lease: { state: "released", releaseDeletesServer: false, cleanupStatus: "retained" },
    });
    for (let n = 0; n < 8; n++) await step(storage, azure);
    expect(await storage.get(`lease:${id}`)).toMatchObject({
      state: "released",
      releaseDeletesServer: false,
    });
    expect(azure.mutations.filter((entry) => entry.method === "DELETE")).toHaveLength(0);
    expect((await publicLease(storage, azure)).cleanupStatus).toBe("retained");
    const stop = await fleet(storage, azure).coordinator.fetch(
      request("POST", `/v1/leases/${id}/release`, { delete: true }),
    );
    expect(stop.status).toBe(200);
    for (let n = 0; n < 25; n++) await step(storage, azure);
    expect((await publicLease(storage, azure)).cleanupStatus).toBe("complete");
    expect(azure.mutations.filter((entry) => entry.method === "DELETE")).toHaveLength(4);
  });

  it("preserves a newer stop request when an older retention step completes late", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    await ready(storage, azure);
    const current = fleet(storage, azure);
    await current.coordinator.fetch(request("POST", `/v1/leases/${id}/release`, { delete: false }));
    const capability = current.provider.resumableProvisioning();
    let retained!: () => void;
    const observed = new Promise<void>((resolve) => {
      retained = resolve;
    });
    let release!: () => void;
    const response = new Promise<void>((resolve) => {
      release = resolve;
    });
    current.provider.resumableProvisioning = () => ({
      version: 1,
      supports: capability.supports.bind(capability),
      prepare: capability.prepare.bind(capability),
      advance: async (stepInput) => {
        const result = await capability.advance(stepInput);
        expect(result.phase).toBe("retained");
        retained();
        await response;
        return result;
      },
    });
    const pending = current.runtime.tick!();
    await observed;
    await fleet(storage, azure).coordinator.fetch(
      request("POST", `/v1/leases/${id}/release`, { delete: true }),
    );
    release();
    await pending;
    const operation = (await storage.get<LeaseProvisioningOperation>(
      provisioningOperationKey(id),
    ))!;
    expect(operation.step.phase).toBe("settling");
    expect(await storage.get(provisioningDueKey(operation))).toBeDefined();
    for (let n = 0; n < 20; n++) await step(storage, azure);
    expect((await publicLease(storage, azure)).cleanupStatus).toBe("complete");
    expect(azure.mutations.filter((entry) => entry.method === "DELETE")).toHaveLength(4);
  });

  it("reports healthy release cleanup pending until verified completion and clears terminal metadata", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    await ready(storage, azure);
    const response = await fleet(storage, azure).coordinator.fetch(
      request("POST", `/v1/leases/${id}/release`, { delete: true }),
    );
    expect(await response.json()).toMatchObject({ lease: { cleanupStatus: "pending" } });
    expect((await publicLease(storage, azure)).cleanupStatus).toBe("pending");
    for (let n = 0; n < 25; n++) {
      await step(storage, azure);
      const operation = await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id));
      expect((await publicLease(storage, azure)).cleanupStatus).toBe(
        operation?.step.phase === "terminal" ? "complete" : "pending",
      );
    }
    const lease = await storage.get<LeaseRecord>(`lease:${id}`);
    expect(lease?.cleanupStartedAt).toBeUndefined();
    expect(lease?.cleanupClaimExpiresAt).toBeUndefined();
    expect(lease?.cleanupError).toBeUndefined();
    expect(lease?.cleanupFailedAt).toBeUndefined();
    expect(lease?.cleanupRetryAt).toBeUndefined();
  });

  it("does not infer deletion from an intent-only crash followed by external disappearance", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    await ready(storage, azure);
    await fleet(storage, azure).coordinator.fetch(
      request("POST", `/v1/leases/${id}/release`, { delete: true }),
    );
    let interrupted = "";
    azure.beforeMutation = (path, method) => {
      if (method === "DELETE") {
        interrupted = path;
        throw new Error("crash before dispatch");
      }
    };
    for (let n = 0; n < 10; n++) {
      if (interrupted) break;
      await step(storage, azure);
    }
    expect(interrupted).toContain("/virtualMachines/");
    const claimKey = azureOwnedDeleteClaimKey(scope, interrupted.split("/").at(-1)!, id);
    expect(await storage.get(claimKey)).toBeDefined();
    expect(
      (await storage.get<{ deletedStableResourceIdentity?: string }>(claimKey))
        ?.deletedStableResourceIdentity,
    ).toBeUndefined();
    azure.remove(interrupted);
    azure.beforeMutation = undefined;
    for (let n = 0; n < 12; n++) await step(storage, azure);
    expect(azure.mutations.filter((entry) => entry.method === "DELETE")).toHaveLength(0);
    expect((await publicLease(storage, azure)).cleanupStatus).toBe("failed");
    expect([...azure.resources.keys()].some((path) => path.includes("/networkInterfaces/"))).toBe(
      true,
    );
  });

  it.each(["replacement", "attachment"])(
    "refuses remaining deletion after a %s changes the frozen survivor",
    async (change) => {
      vi.useFakeTimers({ toFake: ["Date"] });
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      await ready(storage, azure);
      await fleet(storage, azure).coordinator.fetch(
        request("POST", `/v1/leases/${id}/release`, { delete: true }),
      );
      for (let n = 0; n < 10 && !azure.mutations.some((entry) => entry.method === "DELETE"); n++)
        await step(storage, azure);
      const nic = [...azure.resources.entries()].find(([path]) =>
        path.includes("/networkInterfaces/"),
      )![1];
      const properties = nic["properties"] as Record<string, unknown>;
      if (change === "replacement") properties["resourceGuid"] = "different-immutable-id";
      else
        properties["virtualMachine"] = {
          id: `${scope}/providers/Microsoft.Compute/virtualMachines/unrelated`,
        };
      for (let n = 0; n < 10; n++) await step(storage, azure);
      expect(azure.mutations.filter((entry) => entry.method === "DELETE")).toHaveLength(1);
      expect((await publicLease(storage, azure)).cleanupStatus).toBe("failed");
    },
  );

  it("persists successful LRO deletion progress before deleting another member", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    await ready(storage, azure);
    azure.asyncDelete = true;
    await fleet(storage, azure).coordinator.fetch(
      request("POST", `/v1/leases/${id}/release`, { delete: true }),
    );
    for (let n = 0; n < 15; n++) await step(storage, azure);
    expect(azure.mutations.filter((entry) => entry.method === "DELETE")).toHaveLength(1);
    expect((await publicLease(storage, azure)).cleanupStatus).toBe("pending");
    for (let n = 0; n < 20; n++) {
      for (const path of azure.operations.keys()) azure.operations.set(path, "Succeeded");
      await step(storage, azure);
    }
    expect(azure.mutations.filter((entry) => entry.method === "DELETE")).toHaveLength(4);
    expect((await publicLease(storage, azure)).cleanupStatus).toBe("complete");
  });

  it("rechecks ownership and retries in-use DELETE failures on subsequent bounded ticks", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    await ready(storage, azure);
    azure.deleteFailures = ["AnotherOperationInProgress", "DiskInUse"];
    await fleet(storage, azure).coordinator.fetch(
      request("POST", `/v1/leases/${id}/release`, { delete: true }),
    );
    for (let n = 0; n < 25; n++) await step(storage, azure);
    const deletions = azure.mutations.filter((entry) => entry.method === "DELETE");
    expect(deletions).toHaveLength(6);
    expect(new Set(deletions.slice(0, 3).map((entry) => entry.path)).size).toBe(1);
    expect((await publicLease(storage, azure)).cleanupStatus).toBe("complete");
  });

  it("retains debt when DELETE succeeds but exact deletion progress cannot commit", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    const operation = await ready(storage, azure);
    await fleet(storage, azure).coordinator.fetch(
      request("POST", `/v1/leases/${id}/release`, { delete: true }),
    );
    let deleted = false;
    azure.afterMutation = (path, method) => {
      if (method === "DELETE") {
        deleted = true;
        storage.failKey = azureOwnedDeleteClaimKey(scope, path.split("/").at(-1)!, id);
      }
    };
    for (let n = 0; n < 10; n++) {
      if (deleted) break;
      await step(storage, azure);
    }
    expect(deleted).toBe(true);
    storage.failKey = undefined;
    for (let n = 0; n < 15; n++) await step(storage, azure);
    expect((await publicLease(storage, azure)).cleanupStatus).toBe("failed");
    expect(azure.mutations.filter((entry) => entry.method === "DELETE")).toHaveLength(1);
    expect(
      (await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id)))?.step.phase,
    ).toBe("blocked");
    expect(await storage.get(provisioningMaterialKey(operation.operationID))).toBeUndefined();
    expect(await storage.get(provisioningPlanKey(operation.operationID))).toBeDefined();
    expect((await storage.list({ prefix: "provider%3Aazure%3Adelete-claim:" })).size).toBe(1);
  });

  it("resumes cleanup when successful owned-delete progress commits before the provisioning result is lost", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input()));
    for (let n = 0; n < 40; n++) {
      if (
        (await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id)))?.step
          .phase === "ready-to-publish"
      )
        break;
      await step(storage, azure);
    }
    expect(
      (
        await fleet(storage, azure).coordinator.fetch(
          request("POST", `/v1/leases/${id}/release`, { delete: true }),
        )
      ).status,
    ).toBe(200);
    let failed = false;
    azure.afterMutation = (_path, method) => {
      if (method === "DELETE" && !failed) {
        failed = true;
        storage.failKey = provisioningOperationKey(id);
      }
    };
    let crash: unknown;
    for (let n = 0; n < 10; n++) {
      if (failed) break;
      try {
        await step(storage, azure);
      } catch (error) {
        crash = error;
      }
    }
    expect(String(crash)).toContain("injected storage failure");
    storage.failKey = undefined;
    for (let n = 0; n < 30; n++) await step(storage, azure);
    expect(
      (await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id)))?.step.phase,
    ).toBe("terminal");
    expect(azure.mutations.filter((entry) => entry.method === "DELETE")).toHaveLength(4);
    expect(
      [...azure.resources.keys()].some(
        (key) =>
          key.includes("/virtualMachines/") ||
          key.includes("/disks/") ||
          key.includes("/networkInterfaces/") ||
          key.includes("/publicIPAddresses/"),
      ),
    ).toBe(false);
  });
  it("revalidates quota in concurrent admission transactions", async () => {
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    const coordinator = fleet(storage, azure, { CRABBOX_MAX_ACTIVE_LEASES: "1" }).coordinator;
    const responses = await Promise.all([
      coordinator.fetch(request("POST", "/v1/leases", input())),
      coordinator.fetch(
        request("POST", "/v1/leases", {
          ...input(),
          leaseID: "cbx_abcdef012345",
          createAttemptID: "cat_abcdef0123456789abcdef0123456789",
        }),
      ),
    ]);
    expect(responses.map((response) => response.status).toSorted()).toEqual([202, 429]);
    expect((await storage.list({ prefix: "lease-provisioning:" })).size).toBe(1);
    expect(azure.mutations).toHaveLength(0);
  });

  it("claims duplicate concurrent deliveries once and accepts extension success ahead of its LRO", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input()));
    for (let n = 0; n < 40; n++) {
      const operation = await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id));
      vi.setSystemTime(Math.max(Date.now(), operation!.step.nextWake + 1));
      await Promise.all([
        fleet(storage, azure).runtime.tick!(),
        fleet(storage, azure).runtime.tick!(),
      ]);
    }
    expect((await storage.get<LeaseRecord>(`lease:${id}`))?.state).toBe("active");
    expect(
      azure.mutations.filter(
        (entry) => entry.method === "PUT" && /\/virtualMachines\/[^/]+$/.test(entry.path),
      ),
    ).toHaveLength(1);
    expect(azure.mutations.filter((entry) => entry.path.includes("/extensions/"))).toHaveLength(1);
    expect(azure.reads.filter((path) => path.includes("/operations/"))).toHaveLength(0);
  });
  it("refuses an immutable replacement before dispatching the next resource", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    azure.afterMutation = (path) => {
      if (path.includes("/publicIPAddresses/")) {
        const resource = azure.resources.get(path)!;
        (resource["properties"] as Record<string, unknown>)["resourceGuid"] = "replacement-guid";
      }
    };
    await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input()));
    for (let n = 0; n < 20; n++) await step(storage, azure);
    expect(
      (await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id)))?.step.phase,
    ).toBe("blocked");
    expect(
      azure.mutations.filter(
        (entry) => entry.path.includes("/networkInterfaces/") || entry.method === "DELETE",
      ),
    ).toHaveLength(0);
  });
  it("freezes an unversioned gallery image to its highest eligible numeric version", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    azure.galleryVersions = ["2.9.0", "2.10.0", "9.0.0"].map((name) => ({
      name,
      properties: {
        provisioningState: "Succeeded",
        publishingProfile: { excludeFromLatest: name === "9.0.0" },
      },
    }));
    const imageID = `${scope}/providers/Microsoft.Compute/galleries/images/images/windows`;
    expect(
      (
        await fleet(storage, azure).coordinator.fetch(
          request("POST", "/v1/leases", { ...input(), azureImage: imageID }),
        )
      ).status,
    ).toBe(202);
    azure.galleryVersions = [];
    for (let n = 0; n < 40; n++) await step(storage, azure);
    const vm = azure.mutations.find(
      (entry) => entry.method === "PUT" && /\/virtualMachines\/[^/]+$/.test(entry.path),
    );
    expect(vm?.body).toMatchObject({
      properties: { storageProfile: { imageReference: { id: `${imageID}/versions/2.10.0` } } },
    });
  });
  it.each(["scope", "key"])(
    "blocks %s drift while retaining the journal and sealed material",
    async (drift) => {
      vi.useFakeTimers({ toFake: ["Date"] });
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input()));
      if (drift === "scope") {
        const lease = await storage.get<LeaseRecord>(`lease:${id}`);
        await storage.put(`lease:${id}`, {
          ...lease,
          providerScope: "/subscriptions/other/resourceGroups/other",
        });
      }
      await step(storage, azure, drift === "key" ? { CRABBOX_SESSION_SECRET: "" } : {});
      const operation = await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id));
      expect(operation?.step.phase).toBe("blocked");
      expect(await storage.get(provisioningMaterialKey(operation!.operationID))).toBeDefined();
      expect(azure.mutations).toHaveLength(0);
    },
  );

  it("keeps a tokenless, non-opt-in initial caller on the synchronous facade", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    const { createAttemptID: _token, ...body } = input();
    const initial = request("POST", "/v1/leases", body);
    initial.headers.delete("prefer");
    const response = fleet(storage, azure).coordinator.fetch(initial);
    await vi.waitFor(async () => {
      expect(await storage.get(provisioningOperationKey(id))).toBeDefined();
    });
    for (let n = 0; n < 40; n++) await step(storage, azure);
    expect((await response).status).toBe(201);
  });
  it("settles and cleans the rejected candidate before allocating its market fallback", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    azure.rejectFirstVM = true;
    expect(
      (
        await fleet(storage, azure).coordinator.fetch(
          request("POST", "/v1/leases", {
            ...input(),
            capacity: { market: "spot", fallback: "on-demand" },
          }),
        )
      ).status,
    ).toBe(202);
    for (let n = 0; n < 70; n++) await step(storage, azure);
    expect(await storage.get<LeaseRecord>(`lease:${id}`)).toMatchObject({
      state: "active",
      market: "on-demand",
    });
    const vmIndices = azure.mutations.flatMap((entry, index) =>
      entry.method === "PUT" && /\/virtualMachines\/[^/]+$/.test(entry.path) ? [index] : [],
    );
    expect(vmIndices).toHaveLength(2);
    const beforeFallback = azure.mutations.slice(vmIndices[0], vmIndices[1]);
    expect(beforeFallback.filter((entry) => entry.method === "DELETE")).toHaveLength(2);
    expect((await storage.list({ prefix: "provisioning-attempt:" })).size).toBe(2);
  });

  it.each(["cancel-create", "retain"])(
    "does not publish a late VM result after concurrent %s",
    async (action) => {
      vi.useFakeTimers({ toFake: ["Date"] });
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      let accepted!: () => void;
      const acceptance = new Promise<void>((resolve) => {
        accepted = resolve;
      });
      let reply!: () => void;
      const response = new Promise<void>((resolve) => {
        reply = resolve;
      });
      azure.afterMutation = async (path) => {
        if (/\/virtualMachines\/[^/]+$/.test(path)) {
          accepted();
          await response;
        }
      };
      await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input()));
      let pending: Promise<void> | undefined;
      for (let n = 0; n < 30; n++) {
        const operation = await storage.get<LeaseProvisioningOperation>(
          provisioningOperationKey(id),
        );
        const journal = await storage.get<{
          state: { journal: { stage: string; action: string } };
        }>((operation!.step.state as { journal: string }).journal);
        if (journal?.state.journal.stage === "vm" && journal.state.journal.action === "dispatch") {
          pending = step(storage, azure);
          break;
        }
        await step(storage, azure);
      }
      expect(pending).toBeDefined();
      await acceptance;
      const canceled = await fleet(storage, azure).coordinator.fetch(
        request(
          "POST",
          `/v1/leases/${id}/${action === "retain" ? "release" : "cancel-create"}`,
          action === "retain"
            ? { delete: false }
            : {
                createAttemptID: input().createAttemptID,
              },
        ),
      );
      expect(canceled.status).toBe(200);
      expect((await storage.list({ prefix: "provisioning-material:" })).size).toBe(0);
      const overrides = { CRABBOX_SESSION_SECRET: "" };
      reply();
      await pending;
      expect((await storage.get<LeaseRecord>(`lease:${id}`))?.state).toBe("released");
      for (let n = 0; n < 25; n++) {
        await step(storage, azure, overrides);
        expect((await storage.get<LeaseRecord>(`lease:${id}`))?.state).toBe("released");
      }
      expect((await publicLease(storage, azure)).cleanupStatus).toBe(
        action === "retain" ? "retained" : "complete",
      );
      expect(azure.mutations.filter((entry) => entry.method === "DELETE")).toHaveLength(
        action === "retain" ? 0 : 4,
      );
      if (action === "retain") {
        await fleet(storage, azure, overrides).coordinator.fetch(
          request("POST", `/v1/leases/${id}/release`, { delete: true }),
        );
        for (let n = 0; n < 25; n++) await step(storage, azure, overrides);
      }
      expect(
        (await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id)))?.step.phase,
      ).toBe("terminal");
      expect(
        azure.mutations.filter(
          (entry) => entry.method === "PUT" && /\/virtualMachines\/[^/]+$/.test(entry.path),
        ),
      ).toHaveLength(1);
    },
  );
  it.each(["vm", "extension"])(
    "recovers a committed dispatch claim after %s acceptance but before result commit",
    async (kind) => {
      vi.useFakeTimers({ toFake: ["Date"] });
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      let failed = false;
      azure.afterMutation = (path) => {
        if (
          !failed &&
          (kind === "vm" ? /\/virtualMachines\/[^/]+$/.test(path) : path.includes("/extensions/"))
        ) {
          failed = true;
          storage.failKey = provisioningOperationKey(id);
        }
      };
      expect(
        (await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input())))
          .status,
      ).toBe(202);
      let crash: unknown;
      for (let n = 0; n < 40; n++) {
        if (failed) break;
        try {
          await step(storage, azure);
        } catch (error) {
          crash = error;
        }
      }
      expect(String(crash)).toContain("injected storage failure");
      expect(failed).toBe(true);
      expect(
        (await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id)))?.claim,
      ).toBeDefined();
      storage.failKey = undefined;
      for (let n = 0; n < 40; n++) await step(storage, azure);
      expect((await storage.get<LeaseRecord>(`lease:${id}`))?.state).toBe("active");
      expect(
        azure.mutations.filter(
          (entry) => entry.method === "PUT" && /\/virtualMachines\/[^/]+$/.test(entry.path),
        ),
      ).toHaveLength(1);
      expect(
        azure.mutations.filter(
          (entry) => entry.method === "PUT" && entry.path.includes("/extensions/"),
        ),
      ).toHaveLength(1);
    },
  );

  it.each(["cancel-create", "release"])(
    "honors %s after allocation, retaining identity until verified cleanup",
    async (action) => {
      vi.useFakeTimers({ toFake: ["Date"] });
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      expect(
        (
          await fleet(storage, azure).coordinator.fetch(
            request("POST", "/v1/leases", { ...input(), keep: true }),
          )
        ).status,
      ).toBe(202);
      for (
        let n = 0;
        n < 30 && !azure.mutations.some((entry) => /\/virtualMachines\/[^/]+$/.test(entry.path));
        n++
      )
        await step(storage, azure);
      const response = await fleet(storage, azure).coordinator.fetch(
        request(
          "POST",
          `/v1/leases/${id}/${action}`,
          action === "cancel-create"
            ? { createAttemptID: input().createAttemptID }
            : { delete: true },
        ),
      );
      expect(response.status).toBe(200);
      for (let n = 0; n < 25; n++) await step(storage, azure);
      expect((await storage.get<LeaseRecord>(`lease:${id}`))?.state).toBe("released");
      expect(
        (await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id)))?.step.phase,
      ).toBe("terminal");
      expect(azure.mutations.filter((entry) => entry.method === "DELETE")).toHaveLength(4);
      expect(azure.mutations.filter((entry) => entry.path.includes("/extensions/"))).toHaveLength(
        0,
      );
    },
  );

  it("finishes publication after a storage rollback without repeating provider work", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input()));
    for (
      let n = 0;
      n < 35 &&
      (await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id)))?.step.phase !==
        "ready-to-publish";
      n++
    )
      await step(storage, azure);
    const writes = azure.mutations.length;
    storage.failKey = `lease:${id}`;
    await expect(step(storage, azure)).rejects.toThrow("injected storage failure");
    storage.failKey = undefined;
    await step(storage, azure);
    expect((await storage.get<LeaseRecord>(`lease:${id}`))?.state).toBe("active");
    expect(azure.mutations).toHaveLength(writes);
  });

  it("rejects missing material configuration before admission and reports readiness", async () => {
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    const coordinator = fleet(storage, azure, { CRABBOX_SESSION_SECRET: "" }).coordinator;
    expect((await coordinator.fetch(request("POST", "/v1/leases", input()))).status).toBe(424);
    expect((await storage.list({ prefix: "lease-provisioning:" })).size).toBe(0);
    expect(azure.mutations).toHaveLength(0);
    const readiness = await coordinator.fetch(
      request("GET", "/v1/providers/azure/readiness?target=windows"),
    );
    expect(await readiness.json()).toMatchObject({
      resumableProvisioning: { available: false, missing: ["CRABBOX_SESSION_SECRET"] },
    });
  });

  it("keeps fixed PUT replay and intent-conflict contracts", async () => {
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    const { createAttemptID: _token, ...body } = input();
    const coordinator = fleet(storage, azure).coordinator;
    expect((await coordinator.fetch(request("PUT", `/v1/leases/${id}`, body))).status).toBe(202);
    expect((await coordinator.fetch(request("PUT", `/v1/leases/${id}`, body))).status).toBe(202);
    const restarted = fleet(storage, azure, {
      CRABBOX_DURABLE_PROVISIONING_ADMISSION: "false",
      CRABBOX_AZURE_IMAGE: "different:default:image:1.0.0",
    }).coordinator;
    const replay = await restarted.fetch(request("PUT", `/v1/leases/${id}`, body));
    expect(replay.status).toBe(202);
    expect(replay.headers.get("preference-applied")).toBe("respond-async");
    expect(
      (
        await coordinator.fetch(
          request("PUT", `/v1/leases/${id}`, { ...body, serverType: "Standard_D8s_v5" }),
        )
      ).status,
    ).toBe(409);
    expect((await storage.list({ prefix: "lease-provisioning:" })).size).toBe(1);
  });
  it("rejects a concurrent fixed replay canceled after durable admission commits", async () => {
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    const { coordinator, provider, runtime } = fleet(storage, azure);
    const continuation = new AzureResumableProvisioning(env, azure.fetch, storage);
    provider.resumableProvisioning = () => continuation;
    const prepare = continuation.prepare.bind(continuation);
    const prepared = [Promise.withResolvers<void>(), Promise.withResolvers<void>()];
    const resume = [Promise.withResolvers<void>(), Promise.withResolvers<void>()];
    let preparations = 0;
    vi.spyOn(continuation, "prepare").mockImplementation(async (...args) => {
      const index = preparations++;
      prepared[index]!.resolve();
      await resume[index]!.promise;
      return prepare(...args);
    });
    const { createAttemptID: _token, ...body } = input();
    const first = coordinator.fetch(request("PUT", `/v1/leases/${id}`, body));
    let second: Promise<Response> | undefined;
    const admitted = Promise.withResolvers<void>();
    const acknowledge = Promise.withResolvers<void>();
    try {
      await prepared[0]!.promise;
      second = coordinator.fetch(request("PUT", `/v1/leases/${id}`, body));
      await prepared[1]!.promise;
      resume[0]!.resolve();
      expect((await first).status).toBe(202);
      const original = runtime.commitAndWake.bind(runtime);
      let hold = true;
      vi.spyOn(runtime, "commitAndWake").mockImplementation(async (callback) => {
        const result = await original(callback);
        if (hold) {
          hold = false;
          admitted.resolve();
          await acknowledge.promise;
        }
        return result;
      });
      resume[1]!.resolve();
      await admitted.promise;
      expect(
        (await coordinator.fetch(request("POST", `/v1/leases/${id}/release`, { delete: true })))
          .status,
      ).toBe(200);
      acknowledge.resolve();
      expect((await second).status).toBe(409);
      expect(await storage.get<LeaseRecord>(`lease:${id}`)).toMatchObject({ state: "released" });
      expect(
        await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id)),
      ).toHaveProperty("canceledAt");
      expect(azure.mutations).toHaveLength(0);
      expect((await storage.list({ prefix: "lease-provisioning:" })).size).toBe(1);
    } finally {
      for (const gate of resume) gate.resolve();
      acknowledge.resolve();
      await Promise.all([first, second]);
    }
  });

  it("reconstructs Fleet, runtime and Azure at every phase without reallocating or rerunning bootstrap", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    azure.loseResponseFor = "/virtualMachines/";
    const admitted = await fleet(storage, azure).coordinator.fetch(
      request("POST", "/v1/leases", input()),
    );
    expect(admitted.status).toBe(202);
    expect(admitted.headers.get("preference-applied")).toBe("respond-async");
    expect(admitted.headers.get("location")).toBe(`/v1/leases/${id}`);
    for (let n = 0; n < 45; n++)
      await step(storage, azure, {
        CRABBOX_DURABLE_PROVISIONING_ADMISSION: "false",
        CRABBOX_AZURE_RESOURCE_GROUP: "changed-after-admission",
        CRABBOX_AZURE_LOCATION: "westus",
      });
    const lease = await storage.get<LeaseRecord>(`lease:${id}`);
    expect(lease?.state).toBe("active");
    const vms = azure.mutations.filter(
      (mutation) => mutation.method === "PUT" && /\/virtualMachines\/[^/]+$/.test(mutation.path),
    );
    expect(vms).toHaveLength(1);
    expect(vms[0]!.headers.get("if-none-match")).toBe("*");
    expect(
      azure.mutations.filter(
        (mutation) => mutation.path.includes("/extensions/") && mutation.method === "PUT",
      ),
    ).toHaveLength(1);
    const operation = await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id));
    expect(await storage.get(provisioningMaterialKey(operation!.operationID))).toBeUndefined();
    const response = await fleet(storage, azure).coordinator.fetch(
      request("GET", `/v1/leases/${id}`),
    );
    const publicJSON = await response.text();
    expect(publicJSON).not.toContain("adminPassword");
    expect(publicJSON).not.toContain("bootstrap");
    expect(publicJSON).not.toContain("synthetic-session-secret");
  });

  it("rolls back the canonical binding, lease, material and due marker on admission failure", async () => {
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    storage.failKey = provisioningOperationKey(id);
    const response = await fleet(storage, azure).coordinator.fetch(
      request("POST", "/v1/leases", input()),
    );
    expect(response.status).toBe(500);
    expect(await storage.get(`lease:${id}`)).toBeUndefined();
    expect(
      (await storage.get<{ canonicalLeaseID?: string }>(`create-attempt:${id}`))?.canonicalLeaseID,
    ).toBeUndefined();
    expect((await storage.list({ prefix: "provisioning-material:" })).size).toBe(0);
    expect((await storage.list({ prefix: "provisioning-due:" })).size).toBe(0);
    expect(azure.mutations).toHaveLength(0);
  });

  it("deduplicates concurrent same-token admission", async () => {
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    const coordinator = fleet(storage, azure).coordinator;
    const responses = await Promise.all([
      coordinator.fetch(request("POST", "/v1/leases", input())),
      coordinator.fetch(request("POST", "/v1/leases", input())),
    ]);
    expect(responses.map((response) => response.status)).toEqual([202, 202]);
    expect((await storage.list({ prefix: "lease-provisioning:" })).size).toBe(1);
    expect((await storage.list({ prefix: "provisioning-material:" })).size).toBe(1);
  });

  it("retains unknown dispatch absence as debt without fallback", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    azure.afterMutation = (path) => {
      if (path.includes("/publicIPAddresses/")) azure.resources.delete(path);
    };
    azure.loseResponseFor = "/publicIPAddresses/";
    expect(
      (await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input())))
        .status,
    ).toBe(202);
    for (let n = 0; n < 25; n++) await step(storage, azure);
    const operation = await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id));
    expect(operation?.step.phase).toBe("blocked");
    expect(operation?.step.attempt).toBe(0);
    expect(
      azure.mutations.filter((mutation) => mutation.path.includes("/publicIPAddresses/")),
    ).toHaveLength(1);
    expect(await storage.get(provisioningMaterialKey(operation!.operationID))).toBeDefined();
    const overrides = { CRABBOX_SESSION_SECRET: "" };
    const canceled = await fleet(storage, azure, overrides).coordinator.fetch(
      request("POST", `/v1/leases/${id}/cancel-create`, {
        createAttemptID: input().createAttemptID,
      }),
    );
    expect(canceled.status).toBe(200);
    for (let n = 0; n < 8; n++) await step(storage, azure, overrides);
    expect((await storage.list({ prefix: "provisioning-material:" })).size).toBe(0);
    expect((await publicLease(storage, azure)).cleanupStatus).toBe("failed");
    expect((await storage.get<LeaseRecord>(`lease:${id}`))?.provisioningResourceMayExist).toBe(
      true,
    );
    expect(
      azure.mutations.filter((entry) => entry.path.includes("/publicIPAddresses/")),
    ).toHaveLength(1);
    expect(azure.mutations.filter((entry) => entry.method === "DELETE")).toHaveLength(0);
  });
});

describe("protected material", () => {
  it("atomically rolls back cancellation when material retirement cannot commit", async () => {
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input()));
    const operation = (await storage.get<LeaseProvisioningOperation>(
      provisioningOperationKey(id),
    ))!;
    storage.failKey = provisioningMaterialKey(operation.operationID);
    const response = await fleet(storage, azure).coordinator.fetch(
      request("POST", `/v1/leases/${id}/release`, { delete: false }),
    );
    expect(response.status).toBe(500);
    expect(await storage.get(provisioningOperationKey(id))).toEqual(operation);
    expect((await storage.get<LeaseRecord>(`lease:${id}`))?.state).toBe("provisioning");
    expect((await storage.list({ prefix: "provisioning-material:" })).size).toBe(1);
    expect(azure.mutations).toHaveLength(0);
  });

  it.each(["lost-key", "missing-material", "tampered-material"])(
    "blocks forward work with %s before any allocation",
    async (failure) => {
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input()));
      const operation = (await storage.get<LeaseProvisioningOperation>(
        provisioningOperationKey(id),
      ))!;
      const key = provisioningMaterialKey(operation.operationID);
      const sealed = (await storage.get<Parameters<typeof openProvisioningMaterial>[2]>(key))!;
      if (failure === "missing-material") await storage.delete(key);
      if (failure === "tampered-material")
        await storage.put(key, { ...sealed, ciphertext: "AAAA" });
      const current = fleet(
        storage,
        azure,
        failure === "lost-key" ? { CRABBOX_SESSION_SECRET: "" } : {},
      );
      await current.runtime.tick!();
      expect(
        (await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id)))?.step.phase,
      ).toBe("blocked");
      expect(azure.mutations).toHaveLength(0);
      expect((await storage.get<LeaseRecord>(`lease:${id}`))?.state).toBe("provisioning");
    },
  );

  it.each(["plan", "quarantine"])(
    "does not blindly retire material for an unsupported %s record",
    async (unsupported) => {
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      await fleet(storage, azure).coordinator.fetch(request("POST", "/v1/leases", input()));
      const operation = (await storage.get<LeaseProvisioningOperation>(
        provisioningOperationKey(id),
      ))!;
      if (unsupported === "plan") {
        const key = provisioningPlanKey(operation.operationID);
        await storage.put(key, {
          ...(await storage.get<Record<string, unknown>>(key)),
          version: 999,
        });
      } else
        await storage.put(`provisioning-quarantine:${id}`, {
          schema: 1,
          reason: "unsupported_or_inconsistent_record",
        });
      await fleet(storage, azure).coordinator.fetch(
        request("POST", `/v1/leases/${id}/release`, { delete: false }),
      );
      expect((await storage.list({ prefix: "provisioning-material:" })).size).toBe(1);
      expect(azure.mutations).toHaveLength(0);
    },
  );

  it.each(["", "rotated-synthetic-session-secret-with-32-characters"])(
    "retires retained replay material and later cleans exact resources with session key %j",
    async (sessionSecret) => {
      vi.useFakeTimers({ toFake: ["Date"] });
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      const operation = await ready(storage, azure);
      const writes = azure.mutations.length;
      await fleet(storage, azure).coordinator.fetch(
        request("POST", `/v1/leases/${id}/release`, { delete: false }),
      );
      expect(await storage.get(provisioningMaterialKey(operation.operationID))).toBeUndefined();
      for (let n = 0; n < 4; n++) await step(storage, azure);
      expect((await publicLease(storage, azure)).cleanupStatus).toBe("retained");
      const overrides = { CRABBOX_SESSION_SECRET: sessionSecret };
      const response = await fleet(storage, azure, overrides).coordinator.fetch(
        request("POST", `/v1/leases/${id}/release`, { delete: true }),
      );
      expect(response.status).toBe(200);
      for (let n = 0; n < 20; n++) await step(storage, azure, overrides);
      expect((await publicLease(storage, azure)).cleanupStatus).toBe("complete");
      expect(azure.mutations.slice(writes).map((entry) => entry.method)).toEqual([
        "DELETE",
        "DELETE",
        "DELETE",
        "DELETE",
      ]);
      expect(await storage.get(provisioningPlanKey(operation.operationID))).toBeDefined();
    },
  );

  it.each(["expiry", "attempt-cancellation"])(
    "retires replay material on controller-driven %s without decrypting it",
    async (reason) => {
      vi.useFakeTimers({ toFake: ["Date"] });
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      const operation = await ready(storage, azure);
      const writes = azure.mutations.length;
      if (reason === "expiry") {
        vi.setSystemTime(operation.deadline + 1);
      } else {
        const attempt = await storage.get<Record<string, unknown>>(`create-attempt:${id}`);
        await storage.put(`create-attempt:${id}`, { ...attempt, state: "canceled" });
      }
      const overrides = { CRABBOX_SESSION_SECRET: "" };
      await step(storage, azure, overrides);
      expect(await storage.get(provisioningMaterialKey(operation.operationID))).toBeUndefined();
      for (let n = 0; n < 20; n++) await step(storage, azure, overrides);
      expect((await publicLease(storage, azure)).cleanupStatus).toBe("complete");
      expect(azure.mutations.slice(writes).map((entry) => entry.method)).toEqual([
        "DELETE",
        "DELETE",
        "DELETE",
        "DELETE",
      ]);
    },
  );

  it.each([false, true])(
    "cancels under a lost session key while preserving exact cleanup authority (replacement=%s)",
    async (replacement) => {
      vi.useFakeTimers({ toFake: ["Date"] });
      const storage = new ProvisioningTestStorage();
      const azure = new AzureFixture();
      const operation = await ready(storage, azure);
      const writes = azure.mutations.length;
      if (replacement) {
        const vm = [...azure.resources.entries()].find(([path]) =>
          /\/virtualMachines\/[^/]+$/.test(path),
        )![1];
        (vm["properties"] as Record<string, unknown>)["vmId"] = "replacement-vm";
      }
      const overrides = { CRABBOX_SESSION_SECRET: "" };
      const response = await fleet(storage, azure, overrides).coordinator.fetch(
        request("POST", `/v1/leases/${id}/cancel-create`, {
          createAttemptID: input().createAttemptID,
        }),
      );
      expect(response.status).toBe(200);
      expect(await storage.get(provisioningMaterialKey(operation.operationID))).toBeUndefined();
      for (let n = 0; n < 20; n++) await step(storage, azure, overrides);
      expect((await publicLease(storage, azure)).cleanupStatus).toBe(
        replacement ? "failed" : "complete",
      );
      expect(azure.mutations.slice(writes).map((entry) => entry.method)).toEqual(
        replacement ? [] : ["DELETE", "DELETE", "DELETE", "DELETE"],
      );
      expect((await storage.get<LeaseRecord>(`lease:${id}`))?.provisioningResourceMayExist).toBe(
        replacement,
      );
      expect(await storage.get(provisioningPlanKey(operation.operationID))).toBeDefined();
      expect(
        (await storage.list({ prefix: `provisioning-attempt:${operation.operationID}:` })).size,
      ).toBe(1);
    },
  );

  it("keeps generated material out of GET, list, portal, journal errors and logs", async () => {
    const storage = new ProvisioningTestStorage();
    const azure = new AzureFixture();
    const current = fleet(storage, azure);
    await current.coordinator.fetch(request("POST", "/v1/leases", input()));
    const operation = await storage.get<LeaseProvisioningOperation>(provisioningOperationKey(id));
    const sealed = await storage.get<Parameters<typeof openProvisioningMaterial>[2]>(
      provisioningMaterialKey(operation!.operationID),
    );
    const material = await openProvisioningMaterial(env, operation!, sealed);
    const output: string[] = [];
    for (const level of ["log", "warn", "error"] as const)
      vi.spyOn(console, level).mockImplementation((...args: unknown[]) => {
        output.push(args.map(String).join(" "));
      });
    const capability = current.provider.resumableProvisioning();
    current.provider.resumableProvisioning = () => ({
      version: 1,
      supports: capability.supports.bind(capability),
      prepare: capability.prepare.bind(capability),
      advance: async () => {
        throw new Error(material.adminPassword);
      },
    });
    await current.runtime.tick!();
    for (const path of [`/v1/leases/${id}`, "/v1/leases", "/portal"]) {
      const response = await current.coordinator.fetch(request("GET", path));
      expect(response.status).toBe(200);
      output.push(await response.text());
    }
    output.push(JSON.stringify(await storage.get(provisioningOperationKey(id))));
    expect(
      output.some(
        (value) =>
          value.includes(material.adminPassword) ||
          value.includes(material.bootstrap) ||
          value.includes(sealed!.ciphertext),
      ),
    ).toBe(false);
  });
  it("bounds authentication even when the transport ignores abort", async () => {
    vi.useFakeTimers();
    const fetcher = vi.fn<typeof fetch>(() => new Promise<Response>(() => {}));
    const transport = new AzureProvisioningTransport(env, fetcher);
    const result = transport.request(scope, "eastus", "GET", scope, "2021-04-01");
    const settled = result.then(
      () => "unexpected success",
      (error: unknown) => String(error),
    );
    await vi.advanceTimersByTimeAsync(8000);
    expect(await settled).toContain("request_unresolved");
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
  it("never authenticates to an operation URL outside the frozen ARM scope", async () => {
    const fetcher = vi.fn<typeof fetch>();
    const transport = new AzureProvisioningTransport(env, fetcher);
    await expect(
      transport.request(
        scope,
        "eastus",
        "GET",
        "https://untrusted.invalid/operation",
        "2024-07-01",
        undefined,
        true,
      ),
    ).rejects.toThrow("scope");
    await expect(
      transport.request(
        scope,
        "eastus",
        "GET",
        "https://management.azure.com/subscriptions/other/resourceGroups/foreign/providers/Microsoft.Compute/operations/one",
        "2024-07-01",
        undefined,
        true,
      ),
    ).rejects.toThrow("scope");
    expect(fetcher).not.toHaveBeenCalled();
  });
  it("binds the ciphertext to lease, operation, generation and scope; fails closed on key loss", async () => {
    const binding = {
      schema: 1 as const,
      leaseID: id,
      operationID: "operation",
      generation: "generation",
      scope,
    };
    const material = { adminPassword: "generated-password-123!", bootstrap: "original bootstrap" };
    const sealed = await sealProvisioningMaterial(env, binding, material);
    expect(JSON.stringify(sealed)).not.toContain(material.adminPassword);
    expect(await openProvisioningMaterial(env, binding, sealed)).toEqual(material);
    for (const field of ["leaseID", "operationID", "generation", "scope"] as const)
      await expect(
        openProvisioningMaterial(env, { ...binding, [field]: "changed" }, sealed),
      ).rejects.toThrow("unavailable");
    await expect(
      openProvisioningMaterial({ ...env, CRABBOX_SESSION_SECRET: "" }, binding, sealed),
    ).rejects.toThrow("unavailable");
    await expect(
      openProvisioningMaterial(env, binding, {
        ...sealed,
        ciphertext: `AAAA${sealed.ciphertext.slice(4)}`,
      }),
    ).rejects.toThrow("unavailable");
  });
});
