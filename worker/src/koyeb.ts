import type { LeaseConfig } from "./config";
import type { CoordinatorStorageView } from "./coordinator-runtime";
import { redactDiagnosticSecrets } from "./http";
import {
  leaseHasConfirmedNoProviderResource,
  leaseProviderCleanupConfirmed,
} from "./lease-cleanup";
import { ProjectCheckpointError, projectCheckpointFailureCodes } from "./project-checkpoints";
import { sshPublicKeyIdentity } from "./provider-key";
import { providerLabelValue } from "./provider-labels";
import {
  ProviderResourceUnresolvedError,
  type FrozenProvisioningPlan,
  type ProviderProvisioningCandidate,
  type ProviderResumableProvisioning,
  type ProvisioningStep,
} from "./provider-provisioning";
import { leaseProviderName } from "./slug";
import type {
  Env,
  KoyebCleanupEvidence,
  LeaseImageIdentity,
  LeaseRecord,
  ProviderMachine,
  ProviderReleasePending,
  ReadyPoolImageIdentity,
  TailscaleMetadata,
} from "./types";

const defaultAPIURL = "https://app.koyeb.com";
const defaultRegion = "was";
const defaultInstanceType = "large";
const managementRoute = "/koyeb-sandbox";
const publicKeyPath = "/run/crabbox-authorized-key.pub";
const bootstrapCommand = "/usr/local/bin/crabbox-koyeb-bootstrap";
const workRoot = "/workspace/crabbox";
const imageKind = "koyeb-sandbox-runner";
export const koyebPoolBootstrap = "clean-runner-v1";
const pollInterval = 2_000;
// An explicit coordinator observation budget, independent of any CLI/RPC deadline.
const deletionConfirmationBudgetMs = 5 * 60_000;
const koyebQuotaUsageCacheSeconds = 60;
const capacitySnapshotLocalReuseMs = koyebQuotaUsageCacheSeconds * 1_000;
const koyebRequestTimeoutMs = 30_000;
const serviceStatuses = new Set([
  "STARTING",
  "HEALTHY",
  "DEGRADED",
  "UNHEALTHY",
  "DELETING",
  "DELETED",
  "PAUSING",
  "PAUSED",
  "RESUMING",
]);
const provisioningServiceStatuses = new Set(["STARTING", "DELETING", "PAUSING", "RESUMING"]);
const provisioningDeploymentStatuses = new Set([
  "PENDING",
  "PROVISIONING",
  "SCHEDULED",
  "CANCELING",
  "ALLOCATING",
  "STARTING",
  "STOPPING",
  "ERRORING",
]);
const deploymentStatuses = new Set([
  ...provisioningDeploymentStatuses,
  "CANCELED",
  "HEALTHY",
  "DEGRADED",
  "UNHEALTHY",
  "STOPPED",
  "ERROR",
  "STASHED",
  "SLEEPING",
]);
const uuidPattern = /^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$/i;
const immutableImagePattern =
  /^(?=.{1,512}$)[a-z0-9][a-z0-9._:-]*(?:\/[a-z0-9][a-z0-9._-]*)+(?::[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?@sha256:[a-f0-9]{64}$/;

type JSONRecord = Record<string, unknown>;
type KoyebTransport = "tailscale" | "koyeb-mesh";

interface KoyebConfiguration {
  apiURL: string;
  token: string;
  organizationID: string;
  appID: string;
  region: string;
  instanceType: string;
  image: string;
  registrySecret?: string;
  appName?: string;
  managedTarget: boolean;
  readyPoolScope: string;
}

interface KoyebAppTarget {
  organizationID: string;
  appID: string;
  appName: string;
  region: string;
}

interface KoyebReleaseContext {
  resourceIdentity?: string;
  assertCleanupOwner?: () => Promise<void>;
  saveCleanupEvidence: (evidence: KoyebCleanupEvidence) => Promise<void>;
}

interface KoyebCleanupIdentity {
  schema: "crabbox-koyeb-cleanup/v1";
  scope: string;
  organizationID: string;
  appID: string;
  region: string;
  leaseID: string;
  generation: string;
  serviceID: string;
  deploymentID: string;
  ttlSeconds: number;
  idleTimeoutSeconds: number;
}

interface KoyebCapacitySnapshot {
  servicesByApp: number;
  organizationServices: number;
  serviceProvisioningConcurrency: number;
  memoryMB: number;
  snapshotCapturedAt: number;
  observedServiceIDs: string[];
  observedAppProvisioningServiceIDs: string[];
  observedOrganizationServiceIDs: string[];
  organizationServicesUsed: number;
  observedProvisioningServiceIDs: string[];
  organizationMemoryMBUsed: number;
  workerInstancesUsed: number;
  workerInstanceLimit?: number;
  workerMemoryMB: number;
}

interface KoyebOrganizationCapacitySnapshot extends Omit<
  KoyebCapacitySnapshot,
  "observedServiceIDs" | "observedAppProvisioningServiceIDs"
> {
  services: KoyebService[];
  deployments: KoyebDeployment[];
}

interface KoyebQuotaUsageSnapshot {
  receivedAt: number;
  servicesUsed: number;
  servicesLimit: number;
  memoryMBUsed: number;
  memoryMBLimit: number;
  workerInstancesUsed: number;
  workerInstanceLimit: number;
}

interface KoyebService {
  id: string;
  name: string;
  type: string;
  organizationID: string;
  appID: string;
  status: string;
  activeDeploymentID?: string;
  latestDeploymentID?: string;
  lifeCycle: JSONRecord;
}

interface KoyebDeployment {
  id: string;
  organizationID: string;
  appID: string;
  serviceID: string;
  status: string;
  definition: JSONRecord;
  metadata: JSONRecord;
}

interface KoyebProvisioningPlan {
  version: 1;
  serviceName: string;
  runnerLeaseID: string;
  organizationID: string;
  appID: string;
  appName?: string;
  region: string;
  instanceType: string;
  image: string;
  registrySecret?: string;
  leaseID: string;
  slug: string;
  owner: string;
  org: string;
  generation: string;
  ttlSeconds: number;
  idleTimeoutSeconds: number;
  sshPublicKey: string;
  transport: KoyebTransport;
  privateHost?: string;
  tailscaleHostname: string;
  tailscaleTags: string[];
  readyPoolScope?: string;
  capacity?: KoyebCapacitySnapshot;
}

type KoyebAction = "dispatch" | "discover" | "observe" | "bootstrap" | "delete" | "confirm-delete";

interface KoyebContinuationState {
  version: 1;
  action: KoyebAction;
  serviceID?: string;
  deploymentID?: string;
  outcomeUncertain?: boolean;
}

interface OwnedDeployment {
  service: KoyebService;
  deployment: KoyebDeployment;
  sandboxSecret: string;
  publicURL?: string;
  routingKey?: string;
}

interface RunnerReady {
  transport: KoyebTransport;
  host: string;
  ipv4?: string;
  fqdn?: string;
  user: string;
  port: string;
  hostKey: string;
}

type Observation =
  | { kind: "missing" }
  | { kind: "pending"; service: KoyebService }
  | { kind: "conflict" }
  | { kind: "owned"; value: OwnedDeployment };

export class KoyebHTTPError extends Error {
  override name = "KoyebHTTPError";

  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    readonly detail = "",
  ) {
    super(`koyeb ${method} ${path}: http ${status}${detail ? `: ${detail}` : ""}`);
  }
}

export class KoyebClient {
  readonly apiURL: string;
  readonly organizationID: string;
  readonly appID: string;
  readonly region: string;
  readonly instanceType: string;
  readonly image: string;
  readonly registrySecret?: string;
  readonly appName?: string;
  readonly managedTarget: boolean;
  readonly readyPoolScope: string;
  private readonly token: string;

  constructor(
    env: Env,
    private readonly fetcher: typeof fetch = fetch,
    appID?: string,
  ) {
    const missing = koyebConfigurationMissing(env);
    if (missing.length) {
      throw new Error(`Koyeb coordinator configuration invalid or missing: ${missing.join(", ")}`);
    }
    const configured = koyebConfiguration(env, appID);
    this.apiURL = configured.apiURL;
    this.token = configured.token;
    this.organizationID = configured.organizationID;
    this.appID = configured.appID;
    this.region = configured.region;
    this.instanceType = configured.instanceType;
    this.image = configured.image;
    this.managedTarget = configured.managedTarget;
    this.readyPoolScope = configured.readyPoolScope;
    if (configured.registrySecret) this.registrySecret = configured.registrySecret;
    if (configured.appName) this.appName = configured.appName;
  }

  async providerScope(): Promise<string> {
    const digest = await sha256Hex(
      JSON.stringify([
        "crabbox-koyeb-context-v1",
        this.apiURL,
        this.organizationID,
        this.appID,
        this.token,
      ]),
    );
    return `koyeb:context:v1:${digest}`;
  }

  async listServices(name?: string): Promise<KoyebService[]> {
    return this.listServiceInventory({
      appID: this.appID,
      ...(name ? { name } : {}),
      sandboxOnly: true,
    });
  }

  async listAllServices(): Promise<KoyebService[]> {
    return this.listServiceInventory({ appID: this.appID, sandboxOnly: false });
  }

  private async listOrganizationServices(): Promise<KoyebService[]> {
    return this.listServiceInventory({ sandboxOnly: false });
  }

  private async listServiceInventory(options: {
    appID?: string;
    name?: string;
    sandboxOnly: boolean;
  }): Promise<KoyebService[]> {
    const limit = 100;
    const query = new URLSearchParams({ limit: String(limit) });
    if (options.appID) query.set("app_id", options.appID);
    if (options.sandboxOnly) query.append("types", "SANDBOX");
    if (options.name) query.set("name", options.name);
    const inventory: KoyebService[] = [];
    const seen = new Set<string>();
    let offset = 0;
    let count: number | undefined;
    for (;;) {
      query.set("offset", String(offset));
      // oxlint-disable-next-line eslint/no-await-in-loop -- Each offset depends on the validated preceding page.
      const value = asObject(await this.apiRequest("GET", `/v1/services?${query.toString()}`));
      const services = value["services"];
      if (!Array.isArray(services)) throw new Error("koyeb service inventory is malformed");
      const pageLimit = inventoryInteger(value, "limit") ?? limit;
      const pageOffset = inventoryInteger(value, "offset");
      const pageCount = inventoryInteger(value, "count");
      const hasNext = value["has_next"];
      if (
        (hasNext !== undefined && typeof hasNext !== "boolean") ||
        pageLimit === 0 ||
        pageLimit > limit ||
        services.length > pageLimit ||
        (pageOffset !== undefined && pageOffset !== offset) ||
        (pageCount !== undefined && count !== undefined && pageCount !== count)
      ) {
        throw new Error("koyeb service inventory pagination is malformed");
      }
      count = pageCount ?? count;
      // Advance over raw provider rows, before ownership filtering by callers.
      const nextOffset = offset + services.length;
      if (
        !Number.isSafeInteger(nextOffset) ||
        (count !== undefined &&
          (count < nextOffset ||
            (hasNext === true && count === nextOffset) ||
            (hasNext === false && count > nextOffset)))
      ) {
        throw new Error("koyeb service inventory pagination is inconsistent");
      }
      // An omitted false has_next/count is common. Probe past a full page when
      // neither field establishes completion, rather than truncating silently.
      const more =
        hasNext ?? (count !== undefined ? nextOffset < count : services.length === pageLimit);
      if (more && nextOffset <= offset) {
        throw new Error("koyeb service inventory pagination made no progress");
      }
      for (const entry of services) {
        const service = koyebService(entry);
        if (
          !uuidPattern.test(service.id) ||
          !uuidPattern.test(service.organizationID) ||
          !uuidPattern.test(service.appID) ||
          !service.name ||
          !service.type
        ) {
          throw new Error("koyeb service inventory is malformed");
        }
        const id = service.id.toLowerCase();
        if (seen.has(id)) throw new Error("koyeb service inventory contains repeated service IDs");
        seen.add(id);
        inventory.push(service);
      }
      if (!more) return inventory;
      offset = nextOffset;
    }
  }

  async validateTarget(): Promise<void> {
    if (!this.managedTarget) return;
    const result = asObject(
      await this.apiRequest("GET", `/v1/apps/${encodeURIComponent(this.appID)}`),
    );
    const app = asObject(result["app"]);
    if (
      stringValue(app["id"]) !== this.appID ||
      stringValue(app["name"]) !== this.appName ||
      stringValue(app["organization_id"]) !== this.organizationID
    ) {
      throw new ProviderResourceUnresolvedError(
        "Koyeb app target identity does not match its registration",
      );
    }
  }

  private async quotas(): Promise<
    Pick<
      KoyebCapacitySnapshot,
      | "servicesByApp"
      | "organizationServices"
      | "serviceProvisioningConcurrency"
      | "memoryMB"
      | "workerInstanceLimit"
    >
  > {
    const response = asObject(
      await this.apiRequest(
        "GET",
        `/v1/organizations/${encodeURIComponent(this.organizationID)}/quotas`,
      ),
    );
    const quotas = asObject(response["quotas"]);
    const servicesByApp = quotaInteger(quotas, "services_by_app");
    const organizationServices = quotaInteger(quotas, "services");
    const serviceProvisioningConcurrency = quotaInteger(quotas, "service_provisioning_concurrency");
    const memoryMB = quotaInteger(quotas, "memory_mb");
    const instanceTypes = quotaStrings(quotas, "instance_types");
    const regions = quotaStrings(quotas, "regions");
    const maxInstancesByType = quotaIntegerMap(quotas, "max_instances_by_type");
    if (
      servicesByApp === undefined ||
      organizationServices === undefined ||
      serviceProvisioningConcurrency === undefined ||
      memoryMB === undefined ||
      servicesByApp <= 0 ||
      organizationServices <= 0 ||
      serviceProvisioningConcurrency <= 0 ||
      memoryMB <= 0
    ) {
      throw capacityEvidenceError("organization quota evidence is incomplete");
    }
    if (instanceTypes.length > 0 && !instanceTypes.includes(this.instanceType)) {
      throw capacityEvidenceError("configured instance type is excluded by organization quota");
    }
    if (regions.length > 0 && !regions.includes(this.region)) {
      throw capacityEvidenceError("configured region is excluded by organization quota");
    }
    const workerInstanceLimit = maxInstancesByType[this.instanceType];
    return {
      servicesByApp,
      organizationServices,
      serviceProvisioningConcurrency,
      memoryMB,
      ...(workerInstanceLimit === undefined ? {} : { workerInstanceLimit }),
    };
  }

  private async quotaUsage(): Promise<KoyebQuotaUsageSnapshot> {
    // Koyeb documents this response as cached for up to 60 seconds. The local
    // capture timestamp only bounds coordinator reuse after this response; it
    // does not establish provider-data age or atomicity with a later create.
    const response = asObject(
      await this.apiRequest(
        "GET",
        `/v1/quotas/organizations/${encodeURIComponent(this.organizationID)}/usage`,
      ),
    );
    const receivedAt = Date.now();
    const usage = asObject(response["usage"]);
    const servicesUsed = usageInteger(usage, "services_used");
    const servicesLimit = usageInteger(usage, "services_limit");
    const memoryMBUsed = usageInteger(usage, "memory_mb_used");
    const memoryMBLimit = usageInteger(usage, "memory_mb_limit");
    const instancesByType = usage["instances_by_type"];
    if (
      servicesUsed === undefined ||
      servicesLimit === undefined ||
      memoryMBUsed === undefined ||
      memoryMBLimit === undefined ||
      servicesLimit <= 0 ||
      memoryMBLimit <= 0 ||
      !Array.isArray(instancesByType)
    ) {
      throw capacityEvidenceError("organization quota usage is incomplete");
    }
    const seen = new Set<string>();
    let workerInstancesUsed: number | undefined;
    let workerInstanceLimit: number | undefined;
    for (const entry of instancesByType) {
      const item = asObject(entry);
      const instanceType = stringValue(item["instance_type"]);
      const used = usageInteger(item, "used");
      const limit = usageInteger(item, "limit");
      if (
        !validKoyebName(instanceType) ||
        used === undefined ||
        limit === undefined ||
        seen.has(instanceType)
      ) {
        throw capacityEvidenceError("organization instance type usage is malformed");
      }
      seen.add(instanceType);
      if (instanceType === this.instanceType) {
        workerInstancesUsed = used;
        workerInstanceLimit = limit;
      }
    }
    if (workerInstancesUsed === undefined || workerInstanceLimit === undefined) {
      throw capacityEvidenceError("configured instance type usage is missing");
    }
    return {
      receivedAt,
      servicesUsed,
      servicesLimit,
      memoryMBUsed,
      memoryMBLimit,
      workerInstancesUsed,
      workerInstanceLimit,
    };
  }

  private async listOrganizationDeployments(): Promise<KoyebDeployment[]> {
    const limit = 100;
    const inventory: KoyebDeployment[] = [];
    const seen = new Set<string>();
    let offset = 0;
    let count: number | undefined;
    for (;;) {
      const query = new URLSearchParams({ limit: String(limit), offset: String(offset) });
      // oxlint-disable-next-line eslint/no-await-in-loop -- Each offset depends on the validated preceding page.
      const value = asObject(await this.apiRequest("GET", `/v1/deployments?${query.toString()}`));
      const deployments = value["deployments"];
      if (!Array.isArray(deployments)) {
        throw capacityEvidenceError("deployment inventory is malformed");
      }
      const pageLimit = inventoryInteger(value, "limit", "deployment") ?? limit;
      const pageOffset = inventoryInteger(value, "offset", "deployment");
      const pageCount = inventoryInteger(value, "count", "deployment");
      const hasNext = value["has_next"];
      if (
        (hasNext !== undefined && typeof hasNext !== "boolean") ||
        pageLimit === 0 ||
        pageLimit > limit ||
        deployments.length > pageLimit ||
        (pageOffset !== undefined && pageOffset !== offset) ||
        (pageCount !== undefined && count !== undefined && pageCount !== count)
      ) {
        throw capacityEvidenceError("deployment inventory pagination is malformed");
      }
      count = pageCount ?? count;
      const nextOffset = offset + deployments.length;
      if (
        !Number.isSafeInteger(nextOffset) ||
        (count !== undefined &&
          (count < nextOffset ||
            (hasNext === true && count === nextOffset) ||
            (hasNext === false && count > nextOffset)))
      ) {
        throw capacityEvidenceError("deployment inventory pagination is inconsistent");
      }
      const more =
        hasNext ?? (count !== undefined ? nextOffset < count : deployments.length === pageLimit);
      if (more && nextOffset <= offset) {
        throw capacityEvidenceError("deployment inventory pagination made no progress");
      }
      for (const entry of deployments) {
        const deployment = koyebDeployment(entry);
        if (
          !uuidPattern.test(deployment.id) ||
          !uuidPattern.test(deployment.organizationID) ||
          !uuidPattern.test(deployment.appID) ||
          !uuidPattern.test(deployment.serviceID) ||
          deployment.organizationID !== this.organizationID ||
          !deploymentStatuses.has(deployment.status)
        ) {
          throw capacityEvidenceError("deployment inventory is malformed");
        }
        const id = deployment.id.toLowerCase();
        if (seen.has(id)) {
          throw capacityEvidenceError("deployment inventory contains repeated deployment IDs");
        }
        seen.add(id);
        inventory.push(deployment);
      }
      if (!more) return inventory;
      offset = nextOffset;
    }
  }

  private async catalogMemoryMB(instanceType: string): Promise<number> {
    const response = asObject(
      await this.apiRequest("GET", `/v1/catalog/instances/${encodeURIComponent(instanceType)}`),
    );
    const instance = asObject(response["instance"]);
    if (stringValue(instance["id"]) !== instanceType) {
      throw capacityEvidenceError("instance catalog identity is malformed");
    }
    const memoryMB = memoryStringMB(stringValue(instance["memory"]));
    if (!memoryMB) throw capacityEvidenceError("instance catalog memory is malformed");
    return memoryMB;
  }

  async organizationCapacitySnapshot(): Promise<KoyebOrganizationCapacitySnapshot> {
    if (!this.managedTarget) {
      throw capacityEvidenceError("managed target capacity is unavailable");
    }
    const [quotas, usage, services, deployments, workerMemoryMB] = await Promise.all([
      this.quotas(),
      this.quotaUsage(),
      this.listOrganizationServices(),
      this.listOrganizationDeployments(),
      this.catalogMemoryMB(this.instanceType),
    ]);
    const expectedWorkerInstanceLimit = quotas.workerInstanceLimit ?? 0;
    if (
      usage.servicesLimit !== quotas.organizationServices ||
      usage.memoryMBLimit !== quotas.memoryMB ||
      usage.workerInstanceLimit !== expectedWorkerInstanceLimit
    ) {
      throw capacityEvidenceError("organization quota limits are inconsistent");
    }
    if (
      services.some(
        (service) =>
          service.organizationID !== this.organizationID || !serviceStatuses.has(service.status),
      )
    ) {
      throw capacityEvidenceError("organization service inventory is malformed");
    }
    const liveServices = services.filter((service) => service.status !== "DELETED");
    const provisioningServiceIDs = observedProvisioningServiceIDs(liveServices, deployments);
    const observedOrganizationServiceIDs = liveServices.map((service) => service.id);
    return {
      ...quotas,
      snapshotCapturedAt: usage.receivedAt,
      observedOrganizationServiceIDs,
      organizationServicesUsed: usage.servicesUsed,
      observedProvisioningServiceIDs: [...provisioningServiceIDs],
      organizationMemoryMBUsed: usage.memoryMBUsed,
      workerInstancesUsed: usage.workerInstancesUsed,
      workerMemoryMB,
      services: liveServices,
      deployments,
    };
  }

  async capacitySnapshot(
    organization?: KoyebOrganizationCapacitySnapshot,
  ): Promise<KoyebCapacitySnapshot | undefined> {
    if (!this.managedTarget) return undefined;
    if (!organization) throw capacityEvidenceError("organization capacity evidence is missing");
    const [, services] = await Promise.all([this.validateTarget(), this.listAllServices()]);
    if (
      services.some(
        (service) =>
          service.organizationID !== this.organizationID ||
          service.appID !== this.appID ||
          !serviceStatuses.has(service.status),
      )
    ) {
      throw capacityEvidenceError("app service inventory is malformed");
    }
    const liveServices = services.filter((service) => service.status !== "DELETED");
    const organizationServicesByID = new Map(
      organization.services.map((service) => [service.id.toLowerCase(), service]),
    );
    if (
      liveServices.some((service) => {
        const organizationService = organizationServicesByID.get(service.id.toLowerCase());
        return (
          organizationService !== undefined &&
          (organizationService.organizationID !== service.organizationID ||
            organizationService.appID !== service.appID)
        );
      })
    ) {
      throw capacityEvidenceError("app service inventory conflicts with organization inventory");
    }
    const { deployments, services: _organizationServices, ...sharedCapacity } = organization;
    return {
      ...sharedCapacity,
      observedServiceIDs: liveServices.map((service) => service.id),
      observedAppProvisioningServiceIDs: observedProvisioningServiceIDs(liveServices, deployments),
    };
  }

  async listCrabboxServers(): Promise<ProviderMachine[]> {
    const services = await this.listServices();
    const machines = await Promise.all(
      services.map(async (service): Promise<ProviderMachine | undefined> => {
        if (
          !uuidPattern.test(service.id) ||
          service.type !== "SANDBOX" ||
          service.organizationID !== this.organizationID ||
          service.appID !== this.appID
        ) {
          return undefined;
        }
        const deploymentID = service.activeDeploymentID || service.latestDeploymentID;
        if (!deploymentID || !uuidPattern.test(deploymentID)) return undefined;
        const deployment = await this.getDeployment(deploymentID);
        return deployment ? inventoryMachine(this, service, deployment) : undefined;
      }),
    );
    return machines.filter((machine): machine is ProviderMachine => machine !== undefined);
  }

  async getService(id: string): Promise<KoyebService | undefined> {
    requireUUID(id, "service id");
    const result = await this.apiRequest("GET", `/v1/services/${encodeURIComponent(id)}`, {
      allowNotFound: true,
    });
    if (result === undefined) return undefined;
    const service = koyebService(asObject(result)["service"]);
    if (service.id !== id) {
      throw new ProviderResourceUnresolvedError(
        "Koyeb service response identity does not match its request",
      );
    }
    return service;
  }

  async getDeployment(id: string): Promise<KoyebDeployment | undefined> {
    requireUUID(id, "deployment id");
    const result = await this.apiRequest("GET", `/v1/deployments/${encodeURIComponent(id)}`, {
      allowNotFound: true,
    });
    if (result === undefined) return undefined;
    const deployment = koyebDeployment(asObject(result)["deployment"]);
    if (deployment.id !== id) {
      throw new ProviderResourceUnresolvedError(
        "Koyeb deployment response identity does not match its request",
      );
    }
    return deployment;
  }

  async createService(body: JSONRecord): Promise<KoyebService> {
    const result = asObject(await this.apiRequest("POST", "/v1/services", { body }));
    return koyebService(result["service"]);
  }

  async deleteService(id: string): Promise<boolean> {
    requireUUID(id, "service id");
    const result = await this.apiRequest("DELETE", `/v1/services/${encodeURIComponent(id)}`, {
      allowNotFound: true,
    });
    return result !== undefined;
  }

  privateHost(serviceName: string): string | undefined {
    return this.appName ? `${serviceName}.${this.appName}.internal` : undefined;
  }

  async managementHealth(
    baseURL: string,
    routingKey: string | undefined,
    secret: string,
  ): Promise<void> {
    await this.managementRequest(baseURL, routingKey, secret, "/health", "GET");
  }

  async managementWriteFile(
    baseURL: string,
    routingKey: string | undefined,
    secret: string,
    path: string,
    content: string,
  ): Promise<void> {
    await this.managementRequest(baseURL, routingKey, secret, "/write_file", "POST", {
      path,
      content,
    });
  }

  async managementRun(
    baseURL: string,
    routingKey: string | undefined,
    secret: string,
    body: { cmd: string; cwd?: string; env?: Record<string, string> },
    protectedValues: readonly string[] = [],
  ): Promise<{ stdout: string; stderr: string; code: number }> {
    const value = asObject(
      await this.managementRequest(baseURL, routingKey, secret, "/run", "POST", body, [
        ...protectedValues,
      ]),
    );
    const stdout = stringValue(value["stdout"]);
    const stderr = stringValue(value["stderr"]);
    const code = value["code"];
    if (!Number.isSafeInteger(code)) throw new Error("koyeb sandbox run response is malformed");
    return { stdout, stderr, code: Number(code) };
  }

  async managementBindPort(
    baseURL: string,
    routingKey: string | undefined,
    secret: string,
    port: string,
  ): Promise<void> {
    let value: JSONRecord;
    try {
      value = asObject(
        await this.managementRequest(baseURL, routingKey, secret, "/bind_port", "POST", { port }),
      );
    } catch (error) {
      if (error instanceof KoyebHTTPError && error.status === 409) {
        try {
          const conflict = asObject(JSON.parse(error.detail));
          if (conflict["success"] === false && conflict["current_port"] === port) return;
        } catch {
          // Preserve the original authenticated-management error below.
        }
      }
      throw error;
    }
    if (value["success"] !== true || value["port"] !== port) {
      throw new Error("koyeb sandbox bind-port response is malformed");
    }
  }

  async ownedServiceForLease(lease: LeaseRecord): Promise<OwnedDeployment | undefined> {
    const scope = await this.providerScope();
    if (
      lease.provider !== "koyeb" ||
      !uuidPattern.test(lease.cloudID) ||
      lease.providerScope !== scope ||
      (this.managedTarget &&
        lease.providerProject !== undefined &&
        lease.providerProject !== this.appID) ||
      (!this.managedTarget &&
        lease.providerProject !== undefined &&
        lease.providerProject !== this.appID) ||
      (lease.region !== undefined && lease.region !== this.region) ||
      !lease.createAttemptGeneration
    ) {
      throw new ProviderResourceUnresolvedError(
        "Koyeb lease cleanup identity is incomplete or belongs to another context",
      );
    }
    await this.validateTarget();
    const service = await this.getService(lease.cloudID);
    if (!service) return undefined;
    const plan = planForLeaseCleanup(this, lease);
    const observation = await observeService(this, plan, service);
    if (observation.kind === "missing") return undefined;
    if (observation.kind !== "owned") {
      throw new ProviderResourceUnresolvedError("Koyeb lease cleanup ownership is not proven");
    }
    return observation.value;
  }

  readyPoolImageIdentity(lease: LeaseRecord): ReadyPoolImageIdentity | undefined {
    const image = lease.image;
    if (
      lease.provider !== "koyeb" ||
      lease.target !== "linux" ||
      lease.architecture !== "amd64" ||
      lease.region !== this.region ||
      lease.serverType !== this.instanceType ||
      image?.provider !== "koyeb" ||
      image.kind !== imageKind ||
      image.source !== "explicit" ||
      image.id !== this.image ||
      image.region !== lease.region ||
      image.scope !== this.readyPoolScope ||
      image.sourceID !== this.registrySecret
    )
      return undefined;
    return { provider: "koyeb", scope: image.scope, id: image.id };
  }

  supportsReadyPoolImageIdentity(identity: ReadyPoolImageIdentity): boolean {
    return (
      identity.provider === "koyeb" &&
      identity.id === this.image &&
      identity.scope === this.readyPoolScope
    );
  }

  async observeReadyPoolImageIdentity(lease: LeaseRecord): Promise<LeaseImageIdentity | undefined> {
    if (!this.readyPoolImageIdentity(lease)) return undefined;
    const owned = await this.ownedServiceForLease(lease);
    if (!owned || owned.service.status !== "HEALTHY" || owned.deployment.status !== "HEALTHY") {
      throw new ProviderResourceUnresolvedError(
        "Koyeb ready pool requires a healthy owned deployment",
      );
    }
    return lease.image;
  }

  async prepareReadyPoolLease(lease: LeaseRecord): Promise<void> {
    const value = JSON.parse(await this.runProjectState(lease, { action: "pool-check" })) as Record<
      string,
      unknown
    >;
    if (value["schema"] !== "crabbox-clean-runner/v1" || value["state"] !== "clean")
      throw new ProviderResourceUnresolvedError("Koyeb clean runner proof is invalid");
  }

  async claimReadyPoolLease(lease: LeaseRecord, claim: string): Promise<void> {
    const value = JSON.parse(
      await this.runProjectState(lease, { action: "pool-claim", claim }),
    ) as Record<string, unknown>;
    if (value["schema"] !== "crabbox-clean-runner/v1" || value["state"] !== "claimed")
      throw new ProviderResourceUnresolvedError("Koyeb clean runner claim is invalid");
  }

  async runProjectState(lease: LeaseRecord, input: Record<string, unknown>): Promise<string> {
    const owned = await this.ownedServiceForLease(lease);
    if (!owned || owned.service.status !== "HEALTHY" || owned.deployment.status !== "HEALTHY") {
      throw new ProviderResourceUnresolvedError(
        "Koyeb project state requires a healthy owned deployment",
      );
    }
    const plan = planForLeaseCleanup(this, lease);
    const management = managementTarget(owned, plan);
    if (!management || plan.transport !== "koyeb-mesh") {
      throw new ProviderResourceUnresolvedError(
        "Koyeb project state requires the private mesh transport",
      );
    }
    // Keep large restores off argv/env limits. The root-owned staging file is consumed and
    // removed before the helper drops privileges to the project user.
    const requestPath = `/var/lib/crabbox-koyeb/project-request-${crypto.randomUUID()}.json`;
    await this.managementWriteFile(
      management.baseURL,
      undefined,
      owned.sandboxSecret,
      requestPath,
      JSON.stringify({ ...input, leaseID: plan.runnerLeaseID }),
    );
    const result = await this.managementRun(management.baseURL, undefined, owned.sandboxSecret, {
      cmd: "/usr/bin/python3 /usr/local/libexec/crabbox-koyeb-sandbox/project-state.py",
      cwd: workRoot,
      env: { CRABBOX_PROJECT_STATE_REQUEST_PATH: requestPath },
    });
    if (result.code !== 0) {
      // A project may contain secrets in paths or output. Keep provider diagnostics categorical.
      let code: unknown;
      try {
        code = (JSON.parse(result.stderr) as Record<string, unknown>)["error"];
      } catch {
        code = undefined;
      }
      if (typeof code === "string" && projectCheckpointFailureCodes.has(code))
        throw new ProjectCheckpointError(code);
      throw new ProviderResourceUnresolvedError("Koyeb project state operation failed");
    }
    return result.stdout;
  }

  async deleteOwnedService(
    lease: LeaseRecord,
    context: KoyebReleaseContext,
  ): Promise<void | ProviderReleasePending> {
    let stage = "identity";
    try {
      await assertKoyebCleanupOwner(context.assertCleanupOwner);
      const scope = await this.providerScope();
      if (
        lease.provider !== "koyeb" ||
        !uuidPattern.test(lease.cloudID) ||
        lease.providerScope !== scope ||
        (lease.providerProject !== undefined && lease.providerProject !== this.appID) ||
        (lease.region !== undefined && lease.region !== this.region) ||
        !lease.createAttemptGeneration
      ) {
        throw new ProviderResourceUnresolvedError(
          "Koyeb lease cleanup identity is incomplete or belongs to another context",
        );
      }
      await this.validateTarget();
      const retained = lease.providerCleanup;
      stage = retained ? "confirmation" : "ownership";
      const service = await this.getService(lease.cloudID);
      if (!service && (!retained || !context.resourceIdentity)) {
        await assertKoyebCleanupOwner(context.assertCleanupOwner);
        return;
      }
      let identity: KoyebCleanupIdentity | undefined;
      let plan: KoyebProvisioningPlan | undefined;
      let allocationSHA256: string | undefined;
      try {
        identity = cleanupIdentity(this, lease, context.resourceIdentity);
        plan = planForLeaseCleanup(this, lease, identity);
        allocationSHA256 = await sha256Hex(JSON.stringify([scope, lease.cloudID, plan]));
      } catch (error) {
        if (!(retained && error instanceof ProviderResourceUnresolvedError)) throw error;
        throw new ProviderResourceUnresolvedError(
          "Koyeb retained cleanup evidence does not match the lease",
        );
      }
      if (retained) {
        if (
          retained.provider !== "koyeb" ||
          !validKoyebCleanupEvidence(retained, lease, allocationSHA256) ||
          retained.deploymentID !== identity.deploymentID
        ) {
          throw new ProviderResourceUnresolvedError(
            "Koyeb retained cleanup evidence does not match the lease",
          );
        }
      }
      if (service && service.id !== identity.serviceID) {
        throw new ProviderResourceUnresolvedError("Koyeb cleanup resource identity changed");
      }
      let evidence = retained;
      const persist = async (next: KoyebCleanupEvidence) => {
        await assertKoyebCleanupOwner(context.assertCleanupOwner);
        const previousStage = stage;
        stage = "journal";
        await context.saveCleanupEvidence(next);
        evidence = next;
        stage = previousStage;
      };
      if (!service) {
        const at = new Date().toISOString();
        await persist({
          ...evidence!,
          lastObservation: { at, status: "ABSENT" },
          confirmation: { at, method: "service-absent" },
        });
        return;
      }
      if (!evidence) {
        const observation = await observeService(this, plan, service);
        if (
          observation.kind !== "owned" ||
          observation.value.deployment.id !== identity.deploymentID
        ) {
          throw new ProviderResourceUnresolvedError("Koyeb lease cleanup ownership is not proven");
        }
        const owned = observation.value;
        if (!serviceStatuses.has(owned.service.status)) {
          throw new ProviderResourceUnresolvedError("Koyeb service status is unknown");
        }
        const now = Date.now();
        await persist({
          version: 1,
          provider: "koyeb",
          leaseID: lease.id,
          serviceID: lease.cloudID,
          allocationSHA256,
          deploymentID: identity.deploymentID,
          dispatchStartedAt: new Date(now).toISOString(),
          confirmationDeadline: new Date(now + deletionConfirmationBudgetMs).toISOString(),
        });
        stage = "delete";
        // The journal write and provider reads can outlive the claim that authorized them.
        await assertKoyebCleanupOwner(context.assertCleanupOwner);
        const accepted = await this.deleteService(lease.cloudID);
        await persist({
          ...evidence!,
          deleteAcceptedAt: new Date().toISOString(),
          deleteResult: accepted ? "accepted" : "not-found",
        });
      }
      stage = "confirmation";
      await assertKoyebCleanupOwner(context.assertCleanupOwner);
      const observed = await this.getService(lease.cloudID);
      await assertKoyebCleanupOwner(context.assertCleanupOwner);
      if (
        observed &&
        (observed.id !== lease.cloudID ||
          !serviceMatchesPlan(observed, plan) ||
          (observed.activeDeploymentID && observed.activeDeploymentID !== evidence!.deploymentID) ||
          (observed.latestDeploymentID && observed.latestDeploymentID !== evidence!.deploymentID))
      ) {
        throw new ProviderResourceUnresolvedError("Koyeb cleanup service ownership changed");
      }
      if (observed && !serviceStatuses.has(observed.status)) {
        throw new ProviderResourceUnresolvedError(
          "Koyeb cleanup observation has an unknown service status",
        );
      }
      const now = Date.now();
      const at = new Date(now).toISOString();
      if (!observed || observed.status === "DELETED") {
        await persist({
          ...evidence!,
          lastObservation: { at, status: observed?.status ?? "ABSENT" },
          confirmation: { at, method: observed ? "service-deleted" : "service-absent" },
        });
        return;
      }
      if (!evidence!.deleteAcceptedAt) {
        throw new ProviderResourceUnresolvedError(
          "Koyeb deletion dispatch outcome is unresolved; deletion was not repeated",
        );
      }
      if (evidence!.deleteResult !== "accepted" || evidence!.confirmation) {
        throw new ProviderResourceUnresolvedError(
          "Koyeb cleanup absence evidence conflicts with a present service",
        );
      }
      if (now >= Date.parse(evidence!.confirmationDeadline)) {
        throw new ProviderResourceUnresolvedError(
          `Koyeb deletion confirmation deadline exhausted; service status=${observed.status}`,
        );
      }
      await persist({ ...evidence!, lastObservation: { at, status: observed.status } });
      return {
        status: "pending",
        nextCheckAt: new Date(
          Math.min(now + pollInterval, Date.parse(evidence!.confirmationDeadline)),
        ).toISOString(),
      };
    } catch (error) {
      if (error instanceof ProviderResourceUnresolvedError) throw error;
      // Unknown/auth/transport/malformed failures are terminal, never successful deletion or pending.
      throw new ProviderResourceUnresolvedError(
        `Koyeb cleanup failed stage=${stage}${error instanceof KoyebHTTPError ? ` http=${error.status}` : ""}`,
      );
    }
  }

  private async apiRequest(
    method: string,
    path: string,
    options: { body?: unknown; allowNotFound?: boolean } = {},
  ): Promise<unknown | undefined> {
    const headers = new Headers({ authorization: `Bearer ${this.token}` });
    if (options.body !== undefined) headers.set("content-type", "application/json");
    const request = new Request(`${this.apiURL}${path}`, {
      method,
      headers,
      redirect: "error",
      ...(options.body === undefined ? {} : { body: JSON.stringify(options.body) }),
    });
    return this.request(request, method, path, [this.token], options.allowNotFound === true);
  }

  private async managementRequest(
    baseURL: string,
    routingKey: string | undefined,
    secret: string,
    path: string,
    method: string,
    body?: unknown,
    protectedValues: readonly string[] = [],
  ): Promise<unknown> {
    const base = managementBaseURL(baseURL, routingKey !== undefined);
    if (routingKey !== undefined && !validRoutingKey(routingKey)) {
      throw new Error("koyeb sandbox routing key is malformed");
    }
    if (!/^[A-Za-z0-9_-]{32,128}$/.test(secret)) {
      throw new Error("koyeb sandbox credential is malformed");
    }
    const headers = new Headers({ authorization: `Bearer ${secret}` });
    if (routingKey !== undefined) headers.set("x-routing-key", routingKey);
    if (body !== undefined) headers.set("content-type", "application/json");
    const request = new Request(`${base}${path}`, {
      method,
      headers,
      redirect: "error",
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    return (await this.request(
      request,
      method,
      `${managementRoute}${path}`,
      [this.token, secret, ...(routingKey ? [routingKey] : []), ...protectedValues],
      false,
    ))!;
  }

  private async request(
    request: Request,
    method: string,
    path: string,
    secrets: readonly string[],
    allowNotFound: boolean,
  ): Promise<unknown | undefined> {
    const controller = new AbortController();
    const boundedRequest = new Request(request, { signal: controller.signal });
    let timer: ReturnType<typeof setTimeout> | undefined;
    let timedOut = false;
    const timeout = new Promise<never>((_resolve, reject) => {
      timer = setTimeout(() => {
        timedOut = true;
        controller.abort();
        reject(new Error("Koyeb request timed out"));
      }, koyebRequestTimeoutMs);
    });
    try {
      const response = await Promise.race([this.fetcher(boundedRequest), timeout]);
      const body = await Promise.race([response.text(), timeout]);
      if (allowNotFound && response.status === 404) return undefined;
      if (!response.ok) {
        throw new KoyebHTTPError(
          method,
          path,
          response.status,
          redactDiagnosticSecrets(body.slice(0, 2048), secrets),
        );
      }
      if (!body.trim()) return {};
      try {
        return JSON.parse(body) as unknown;
      } catch {
        throw new KoyebHTTPError(method, path, response.status, "invalid JSON response");
      }
    } catch (error) {
      if (error instanceof KoyebHTTPError) throw error;
      throw new KoyebHTTPError(
        method,
        path,
        timedOut ? 408 : 0,
        timedOut
          ? "request timed out"
          : redactDiagnosticSecrets(
              error instanceof Error ? error.message : String(error),
              secrets,
            ),
      );
    } finally {
      if (timer !== undefined) clearTimeout(timer);
    }
  }
}

function validKoyebCleanupEvidence(
  evidence: KoyebCleanupEvidence,
  lease: LeaseRecord,
  allocationSHA256: string,
): boolean {
  const startedAt = Date.parse(evidence.dispatchStartedAt);
  const deadline = Date.parse(evidence.confirmationDeadline);
  const acceptedAt = Date.parse(evidence.deleteAcceptedAt ?? "");
  return (
    evidence.version === 1 &&
    evidence.leaseID === lease.id &&
    evidence.serviceID === lease.cloudID &&
    evidence.allocationSHA256 === allocationSHA256 &&
    uuidPattern.test(evidence.deploymentID) &&
    Number.isFinite(startedAt) &&
    deadline === startedAt + deletionConfirmationBudgetMs &&
    (evidence.deleteAcceptedAt === undefined
      ? evidence.deleteResult === undefined
      : Number.isFinite(acceptedAt) &&
        acceptedAt >= startedAt &&
        ["accepted", "not-found"].includes(evidence.deleteResult ?? "")) &&
    (!evidence.lastObservation ||
      (Number.isFinite(Date.parse(evidence.lastObservation.at)) &&
        (evidence.lastObservation.status === "ABSENT" ||
          serviceStatuses.has(evidence.lastObservation.status)))) &&
    (!evidence.confirmation ||
      (Number.isFinite(Date.parse(evidence.confirmation.at)) &&
        ["service-absent", "service-deleted"].includes(evidence.confirmation.method)))
  );
}

function retryableManagementFailure(error: unknown): error is KoyebHTTPError {
  return (
    error instanceof KoyebHTTPError &&
    (error.status === 0 ||
      error.status === 408 ||
      error.status === 425 ||
      error.status === 429 ||
      error.status >= 500)
  );
}

function managementFailureCategory(error: KoyebHTTPError): string {
  if (error.status === 0) return "transport_unavailable";
  if (error.status === 408) return "request_timeout";
  if (error.status === 425) return "service_not_ready";
  if (error.status === 429) return "rate_limited";
  return "server_unavailable";
}

function managementBlockedReason(error: unknown): string {
  if (!(error instanceof KoyebHTTPError)) return "koyeb_management_response_invalid";
  if (error.status === 401 || error.status === 403) return "koyeb_management_unauthorized";
  if (error.status === 404) return "koyeb_management_route_missing";
  return "koyeb_management_rejected";
}

export class KoyebResumableProvisioning implements ProviderResumableProvisioning {
  readonly version = 1 as const;
  private readonly client: KoyebClient;
  private readonly clients: KoyebClient[];
  private readonly privateMeshAvailable: boolean;

  constructor(env: Env, fetcher: typeof fetch = fetch, targetAppID?: string) {
    const registered = koyebAppTargets(env);
    const appIDs = targetAppID
      ? [targetAppID]
      : (registered?.map((target) => target.appID) ?? [env.CRABBOX_KOYEB_APP_ID!.trim()]);
    this.clients = appIDs.map((appID) => new KoyebClient(env, fetcher, appID));
    this.client = this.clients[0]!;
    this.privateMeshAvailable = koyebPrivateMeshAvailable(env);
  }

  private clientForPlan(plan: FrozenProvisioningPlan): KoyebClient {
    const appID = stringValue(asObject(plan.data)["appID"]);
    const client = this.clients.find((candidate) => candidate.appID === appID);
    if (!client) {
      throw new ProviderResourceUnresolvedError(
        "Koyeb provisioning target registration is missing",
      );
    }
    return client;
  }

  supports(config: LeaseConfig): boolean {
    return (
      config.provider === "koyeb" &&
      config.target === "linux" &&
      config.architecture === "amd64" &&
      (config.tailscale ||
        (this.privateMeshAvailable &&
          this.clients.every((client) => Boolean(client.privateHost("crabbox"))))) &&
      !config.tailscaleExitNode &&
      config.serverType === this.client.instanceType &&
      config.workRoot === workRoot
    );
  }

  async prepare(config: LeaseConfig, lease: LeaseRecord) {
    if (!this.supports(config))
      throw new Error("Koyeb durable provisioning configuration unsupported");
    if (!lease.createAttemptGeneration) {
      throw new Error("Koyeb provisioning requires an immutable attempt generation");
    }
    if (config.tailscale && (!config.tailscaleAuthKey || config.tailscaleAuthKey.length > 4096)) {
      throw new Error("Koyeb provisioning requires a bounded one-off Tailscale auth key");
    }
    const serviceName = leaseProviderName(lease.id, lease.slug);
    const transport: KoyebTransport = config.tailscale ? "tailscale" : "koyeb-mesh";
    const organizationCapacity = this.client.managedTarget
      ? await this.client.organizationCapacitySnapshot()
      : undefined;
    const candidates = await Promise.all(
      this.clients.map(async (client): Promise<ProviderProvisioningCandidate> => {
        const [scope, capacity] = await Promise.all([
          client.providerScope(),
          client.capacitySnapshot(organizationCapacity),
        ]);
        const privateHost =
          transport === "koyeb-mesh" ? client.privateHost(serviceName) : undefined;
        if (transport === "koyeb-mesh" && !privateHost) {
          throw new Error("Koyeb private-mesh identity is unavailable");
        }
        const data: KoyebProvisioningPlan = {
          version: 1,
          serviceName,
          runnerLeaseID: serviceName,
          organizationID: client.organizationID,
          appID: client.appID,
          ...(client.managedTarget && client.appName ? { appName: client.appName } : {}),
          region: client.region,
          instanceType: client.instanceType,
          image: client.image,
          ...(client.registrySecret ? { registrySecret: client.registrySecret } : {}),
          leaseID: lease.id,
          slug: lease.slug ?? "",
          owner: lease.providerOwner || lease.owner,
          org: lease.org,
          generation: lease.createAttemptGeneration!,
          ttlSeconds: lease.ttlSeconds,
          idleTimeoutSeconds: lease.idleTimeoutSeconds ?? lease.ttlSeconds,
          sshPublicKey: config.sshPublicKey,
          transport,
          ...(privateHost ? { privateHost } : {}),
          tailscaleHostname: config.tailscaleHostname,
          tailscaleTags: [...config.tailscaleTags],
          ...(client.managedTarget ? { readyPoolScope: client.readyPoolScope } : {}),
          ...(capacity ? { capacity } : {}),
        };
        return {
          plan: {
            version: 1,
            provider: "koyeb",
            scope,
            resources: [{ cloudID: serviceName, region: client.region, scope }],
            data,
          },
          material: {
            adminPassword: randomSecret(),
            bootstrap: transport === "tailscale" ? config.tailscaleAuthKey : "",
            providerSecret: randomSecret(),
          },
          step: {
            phase: "prepared",
            attempt: 0,
            state: { version: 1, action: "dispatch" } satisfies KoyebContinuationState,
            nextWake: Date.now(),
          },
          lease: { providerScope: scope, providerProject: client.appID, region: client.region },
        };
      }),
    );
    const first = candidates[0]!;
    return { ...first, candidates };
  }

  async selectAdmission(
    storage: CoordinatorStorageView,
    candidates: readonly ProviderProvisioningCandidate[],
    _lease: LeaseRecord,
  ): Promise<number> {
    if (!candidates.length) throw new Error("Koyeb app capacity exhausted");
    if (candidates.length === 1 && !(candidates[0]!.plan.data as KoyebProvisioningPlan).capacity) {
      return 0;
    }
    const available = candidates.map((candidate, index) => {
      const data = candidate.plan.data as KoyebProvisioningPlan;
      const capacity = data.capacity;
      if (!capacity) return { index, data, scope: candidate.plan.scope, headroom: 1 };
      if (!validCapacitySnapshot(capacity)) {
        throw new ProviderResourceUnresolvedError("Koyeb app capacity inventory is invalid");
      }
      return {
        index,
        data,
        scope: candidate.plan.scope,
        headroom: capacity.servicesByApp - capacity.observedServiceIDs.length,
      };
    });
    const baseline = available[0]!.data.capacity;
    if (
      !baseline ||
      available.some(
        (candidate) =>
          !candidate.data.capacity || !sameOrganizationCapacity(baseline, candidate.data.capacity),
      )
    ) {
      throw new ProviderResourceUnresolvedError(
        "Koyeb organization capacity evidence is inconsistent",
      );
    }
    const snapshotAge = Date.now() - baseline.snapshotCapturedAt;
    if (snapshotAge < 0 || snapshotAge > capacitySnapshotLocalReuseMs) {
      throw new ProviderResourceUnresolvedError("Koyeb organization capacity snapshot is stale");
    }
    let organizationServices = baseline.organizationServicesUsed;
    let memoryMB = baseline.organizationMemoryMBUsed;
    let workerInstances = baseline.workerInstancesUsed;
    const observedOrganizationServices = new Set(
      baseline.observedOrganizationServiceIDs.map((id) => id.toLowerCase()),
    );
    const observedProvisioningServices = new Set(
      baseline.observedProvisioningServiceIDs.map((id) => id.toLowerCase()),
    );
    for (const candidate of available) {
      for (const serviceID of candidate.data.capacity!.observedAppProvisioningServiceIDs) {
        observedProvisioningServices.add(serviceID.toLowerCase());
      }
    }
    let provisioningServices = observedProvisioningServices.size;
    const reservations = [
      ...(await storage.list<LeaseRecord>({ prefix: "lease:" })).values(),
    ].filter(
      (reservation) => reservation.provider === "koyeb" && reservation.lifecycle !== "registered",
    );
    for (const reservation of reservations) {
      if (
        ["released", "expired", "failed"].includes(reservation.state) &&
        leaseHasConfirmedNoProviderResource(reservation)
      ) {
        continue;
      }
      if (leaseProviderCleanupConfirmed(reservation)) {
        const evidence = reservation.providerCleanup;
        if (!evidence) continue;
        // Canonical completion is committed only after provider cleanup verifies
        // the allocation digest. Completion clears access, so reconstructing the
        // original plan from its host would change native mesh into Tailscale.
        // Retain structural and lease/resource checks without re-deriving that
        // already-verified digest from mutable access fields.
        if (
          evidence.provider === "koyeb" &&
          evidence.confirmation &&
          /^[a-f0-9]{64}$/.test(evidence.allocationSHA256) &&
          validKoyebCleanupEvidence(evidence, reservation, evidence.allocationSHA256)
        ) {
          continue;
        }
        throw new ProviderResourceUnresolvedError(
          "Koyeb completed cleanup evidence does not match its durable lease identity",
        );
      }
      const target = available.find(
        (candidate) =>
          reservation.providerProject === candidate.data.appID ||
          (!reservation.providerProject && reservation.providerScope === candidate.scope),
      );
      const serviceID = reservation.cloudID.toLowerCase();
      if (serviceID && !uuidPattern.test(serviceID)) {
        throw new ProviderResourceUnresolvedError(
          "Koyeb durable lease service identity is invalid",
        );
      }
      const directlyObservedTargets = serviceID
        ? available.filter((candidate) =>
            candidate.data.capacity!.observedServiceIDs.some(
              (observedServiceID) => observedServiceID.toLowerCase() === serviceID,
            ),
          )
        : [];
      if (target) {
        if (
          reservation.providerScope !== target.scope ||
          reservation.region !== target.data.region ||
          reservation.serverType !== target.data.instanceType
        ) {
          throw new ProviderResourceUnresolvedError(
            "Koyeb durable lease target identity does not match its registration",
          );
        }
        const observedInTargetApp =
          directlyObservedTargets.length === 1 && directlyObservedTargets[0] === target;
        if (
          serviceID &&
          !observedInTargetApp &&
          (observedOrganizationServices.has(serviceID) || directlyObservedTargets.length > 0)
        ) {
          throw new ProviderResourceUnresolvedError(
            "Koyeb durable lease service identity does not match its target app",
          );
        }
        if (!serviceID || !observedInTargetApp) target.headroom -= 1;
      } else if (directlyObservedTargets.length > 0) {
        throw new ProviderResourceUnresolvedError(
          "Koyeb durable lease service identity does not match its target app",
        );
      }
      // The aggregate usage response is cached and carries no resource membership
      // or membership timestamp. Every unresolved durable lease is therefore
      // overlaid conservatively, even when a visible service has the same ID.
      organizationServices += 1;
      // Publication commits the active lease and terminal operation together.
      // Cancellation changes lease state before an uncertain create or cleanup
      // settles, so every other unresolved state retains its durable slot.
      // Only the same observed provisioning service can cover that slot;
      // a HEALTHY inventory row does not prove durable publication or cleanup.
      if (reservation.state !== "active" && !observedProvisioningServices.has(serviceID)) {
        provisioningServices += 1;
      }
      memoryMB += baseline.workerMemoryMB;
      workerInstances += 1;
    }
    if (
      organizationServices >= baseline.organizationServices ||
      provisioningServices >= baseline.serviceProvisioningConcurrency ||
      memoryMB + baseline.workerMemoryMB > baseline.memoryMB ||
      (baseline.workerInstanceLimit !== undefined &&
        workerInstances >= baseline.workerInstanceLimit)
    ) {
      throw new ProviderResourceUnresolvedError("Koyeb organization capacity is exhausted");
    }
    let selected: (typeof available)[number] | undefined;
    for (const candidate of available) {
      if (candidate.headroom <= 0) continue;
      if (!selected || candidate.headroom > selected.headroom) selected = candidate;
    }
    if (!selected) throw new Error("Koyeb app capacity exhausted");
    return selected.index;
  }

  async advance(
    input: Parameters<ProviderResumableProvisioning["advance"]>[0],
  ): Promise<ProvisioningStep> {
    const client = this.clientForPlan(input.plan);
    const plan = await validatedPlan(client, input.plan, input.lease);
    const state = continuationState(input.step.state);
    if (
      input.canceled ||
      input.step.phase === "cleanup" ||
      input.step.phase === "settling" ||
      input.step.phase === "retained"
    ) {
      return this.cleanup(client, input, plan, state);
    }
    if (
      !input.material.providerSecret ||
      (plan.transport === "tailscale" && !input.material.bootstrap)
    ) {
      throw new Error("Koyeb forward provisioning material is unavailable");
    }
    const output = (phase: ProvisioningStep["phase"], delay = pollInterval): ProvisioningStep => ({
      phase,
      attempt: 0,
      state,
      nextWake: Date.now() + delay,
    });

    if (state.action === "dispatch" || state.action === "discover") {
      const discovery = await discoverService(client, plan, input.material.providerSecret);
      if (discovery.kind === "conflict") {
        return { ...output("blocked"), blockedReason: "identity_resolution_required" };
      }
      if (discovery.kind === "owned" || discovery.kind === "pending") {
        state.action = "observe";
        state.serviceID = discovery.service.id;
        delete state.outcomeUncertain;
        return output("provisioning", 1);
      }
      if (state.action === "discover" || input.recovering) {
        state.action = "discover";
        state.outcomeUncertain = true;
        return { ...output("blocked"), blockedReason: "dispatch_outcome_unresolved" };
      }
      try {
        const created = await client.createService(
          createServiceRequest(plan, input.material.providerSecret),
        );
        if (!serviceMatchesPlan(created, plan) || !uuidPattern.test(created.id)) {
          state.action = "discover";
          state.outcomeUncertain = true;
          return { ...output("blocked"), blockedReason: "dispatch_identity_unresolved" };
        }
        state.action = "observe";
        state.serviceID = created.id;
        return output("provisioning", 1);
      } catch {
        state.action = "discover";
        state.outcomeUncertain = true;
        return output("provisioning");
      }
    }

    if (!state.serviceID || !uuidPattern.test(state.serviceID)) {
      return { ...output("blocked"), blockedReason: "identity_resolution_required" };
    }
    if (state.action === "observe") {
      const observation = await observeByID(
        client,
        plan,
        state.serviceID,
        input.material.providerSecret,
      );
      if (observation.kind === "missing") {
        return { ...output("blocked"), blockedReason: "observed_resource_missing" };
      }
      if (observation.kind === "conflict") {
        return { ...output("blocked"), blockedReason: "identity_resolution_required" };
      }
      if (observation.kind === "pending") return output("provisioning");
      if (serviceTerminal(observation.value.service.status)) {
        return { ...output("blocked"), blockedReason: "service_terminal" };
      }
      if (deploymentTerminal(observation.value.deployment.status)) {
        return { ...output("blocked"), blockedReason: "deployment_terminal" };
      }
      if (
        observation.value.service.status !== "HEALTHY" ||
        observation.value.deployment.status !== "HEALTHY" ||
        !managementTarget(observation.value, plan)
      ) {
        return output("provisioning");
      }
      state.action = "bootstrap";
      state.deploymentID = observation.value.deployment.id;
      return output("provisioning", 1);
    }

    if (state.action !== "bootstrap" || !state.deploymentID) {
      return { ...output("blocked"), blockedReason: "continuation_state_invalid" };
    }
    const observation = await observeByID(
      client,
      plan,
      state.serviceID,
      input.material.providerSecret,
    );
    if (observation.kind !== "owned") {
      return {
        ...output(observation.kind === "pending" ? "provisioning" : "blocked"),
        ...(observation.kind === "pending"
          ? {}
          : { blockedReason: "bootstrap_identity_unavailable" }),
      };
    }
    const owned = observation.value;
    const management = managementTarget(owned, plan);
    if (
      owned.deployment.id !== state.deploymentID ||
      owned.service.status !== "HEALTHY" ||
      owned.deployment.status !== "HEALTHY" ||
      !management
    ) {
      return { ...output("blocked"), blockedReason: "bootstrap_identity_unavailable" };
    }
    let run: { stdout: string; stderr: string; code: number };
    try {
      await client.managementHealth(
        management.baseURL,
        management.routingKey,
        input.material.providerSecret,
      );
      await client.managementWriteFile(
        management.baseURL,
        management.routingKey,
        input.material.providerSecret,
        publicKeyPath,
        plan.sshPublicKey,
      );
      const bootstrapEnvironment: Record<string, string> = {
        CRABBOX_KOYEB_LEASE_ID: plan.runnerLeaseID,
        CRABBOX_KOYEB_SSH_PUBLIC_KEY_FILE: publicKeyPath,
        ...(plan.transport === "tailscale"
          ? {
              CRABBOX_KOYEB_TAILSCALE_AUTH_KEY: input.material.bootstrap,
              CRABBOX_KOYEB_TAILSCALE_HOSTNAME: plan.tailscaleHostname,
              CRABBOX_KOYEB_TAILSCALE_TAGS: plan.tailscaleTags.join(","),
            }
          : {
              CRABBOX_KOYEB_NETWORK: "koyeb-mesh",
              CRABBOX_KOYEB_PRIVATE_HOST: plan.privateHost!,
            }),
      };
      run = await client.managementRun(
        management.baseURL,
        management.routingKey,
        input.material.providerSecret,
        {
          cmd: bootstrapCommand,
          cwd: workRoot,
          env: bootstrapEnvironment,
        },
        plan.transport === "tailscale" ? [input.material.bootstrap] : [],
      );
    } catch (error) {
      if (retryableManagementFailure(error)) {
        console.warn("koyeb sandbox bootstrap deferred", {
          phase: "bootstrap",
          category: managementFailureCategory(error),
        });
        return output("provisioning");
      }
      const blockedReason = managementBlockedReason(error);
      console.error("koyeb sandbox bootstrap blocked", { phase: "bootstrap", blockedReason });
      return { ...output("blocked"), blockedReason };
    }
    if (run.code !== 0) {
      return { ...output("blocked"), blockedReason: "runner_bootstrap_failed" };
    }
    const ready = runnerReady(run.stdout, plan);
    if (!ready) {
      return { ...output("blocked"), blockedReason: "runner_readiness_invalid" };
    }
    if (plan.transport === "koyeb-mesh") {
      try {
        await client.managementBindPort(
          management.baseURL,
          management.routingKey,
          input.material.providerSecret,
          "22",
        );
      } catch (error) {
        if (retryableManagementFailure(error)) {
          console.warn("koyeb sandbox bootstrap deferred", {
            phase: "bind-port",
            category: managementFailureCategory(error),
          });
          return output("provisioning");
        }
        const blockedReason = managementBlockedReason(error);
        console.error("koyeb sandbox bootstrap blocked", { phase: "bind-port", blockedReason });
        return { ...output("blocked"), blockedReason };
      }
    }
    const tailscale: TailscaleMetadata | undefined =
      plan.transport === "tailscale"
        ? {
            enabled: true,
            hostname: plan.tailscaleHostname,
            ipv4: ready.ipv4!,
            ...(ready.fqdn ? { fqdn: ready.fqdn } : {}),
            tags: [...plan.tailscaleTags],
            state: "ready",
          }
        : undefined;
    const labels =
      plan.transport === "tailscale"
        ? {
            ...ownershipLabels(plan),
            tailscale: "true",
            tailscale_state: "ready",
            tailscale_hostname: plan.tailscaleHostname,
            tailscale_ipv4: ready.ipv4!,
            ...(ready.fqdn ? { tailscale_fqdn: ready.fqdn } : {}),
            tailscale_tags: plan.tailscaleTags.join(","),
          }
        : { ...ownershipLabels(plan), koyeb_network: "mesh" };
    const result = output("ready-to-publish", 1);
    result.publication = {
      server: {
        ...machineForService(owned.service, plan, ready.host, labels),
        resourceIdentity: JSON.stringify({
          schema: "crabbox-koyeb-cleanup/v1",
          scope: input.plan.scope,
          organizationID: plan.organizationID,
          appID: plan.appID,
          region: plan.region,
          leaseID: plan.leaseID,
          generation: plan.generation,
          serviceID: owned.service.id,
          deploymentID: owned.deployment.id,
          ttlSeconds: plan.ttlSeconds,
          idleTimeoutSeconds: plan.idleTimeoutSeconds,
        } satisfies KoyebCleanupIdentity),
      },
      serverType: plan.instanceType,
      market: "on-demand",
      access: {
        sshUser: ready.user,
        sshPort: plan.transport === "koyeb-mesh" ? "3031" : ready.port,
        sshFallbackPorts: [],
        workRoot,
        sshHostKey: ready.hostKey,
        ...(tailscale ? { tailscale } : {}),
      },
      image: imageIdentity(plan),
    };
    return result;
  }

  private async cleanup(
    client: KoyebClient,
    input: Parameters<ProviderResumableProvisioning["advance"]>[0],
    plan: KoyebProvisioningPlan,
    state: KoyebContinuationState,
  ): Promise<ProvisioningStep> {
    const output = (phase: ProvisioningStep["phase"], delay = pollInterval): ProvisioningStep => ({
      phase,
      attempt: 0,
      state,
      nextWake: Date.now() + delay,
    });
    if (state.action === "dispatch" && !input.recovering) return output("terminal", 1);
    if (state.action === "dispatch" || state.action === "discover") {
      const discovery = await discoverService(client, plan);
      if (discovery.kind === "missing") {
        state.action = "discover";
        state.outcomeUncertain = true;
        return { ...output("blocked"), blockedReason: "dispatch_outcome_unresolved" };
      }
      if (discovery.kind === "conflict") {
        return { ...output("blocked"), blockedReason: "identity_resolution_required" };
      }
      state.serviceID = discovery.service.id;
      state.action = "observe";
      return output("cleanup", 1);
    }
    if (!state.serviceID || !uuidPattern.test(state.serviceID)) {
      return { ...output("blocked"), blockedReason: "identity_resolution_required" };
    }
    if (state.action === "confirm-delete") {
      const service = await client.getService(state.serviceID);
      if (!service || service.status === "DELETED") return output("terminal", 1);
      if (!serviceMatchesPlan(service, plan)) {
        return { ...output("blocked"), blockedReason: "identity_resolution_required" };
      }
      return output("cleanup");
    }
    if (state.action === "delete") {
      const observation = await observeByID(client, plan, state.serviceID);
      if (observation.kind === "missing") return output("terminal", 1);
      if (observation.kind !== "owned") {
        return {
          ...output(observation.kind === "pending" ? "cleanup" : "blocked"),
          ...(observation.kind === "pending"
            ? {}
            : { blockedReason: "identity_resolution_required" }),
        };
      }
      // A revoked owner must not advance the journal to confirm-delete.
      await assertKoyebCleanupOwner(input.assertCleanupOwner);
      try {
        await client.deleteService(state.serviceID);
      } finally {
        state.action = "confirm-delete";
      }
      return output("cleanup", 1);
    }
    const observation = await observeByID(client, plan, state.serviceID);
    if (observation.kind === "missing") return output("terminal", 1);
    if (observation.kind === "conflict") {
      return { ...output("blocked"), blockedReason: "identity_resolution_required" };
    }
    if (observation.kind === "pending") return output("cleanup");
    if (input.retain) return output("retained", 1);
    state.action = "delete";
    state.deploymentID = observation.value.deployment.id;
    return output("cleanup", 1);
  }
}

export function koyebConfigurationMissing(env: Env): string[] {
  const missing: string[] = [];
  if (!env.KOYEB_API_TOKEN?.trim()) missing.push("KOYEB_API_TOKEN");
  if (!uuidPattern.test(env.CRABBOX_KOYEB_ORGANIZATION_ID?.trim() ?? "")) {
    missing.push("CRABBOX_KOYEB_ORGANIZATION_ID");
  }
  if (!uuidPattern.test(env.CRABBOX_KOYEB_APP_ID?.trim() ?? "")) {
    missing.push("CRABBOX_KOYEB_APP_ID");
  }
  if (!immutableImagePattern.test(env.CRABBOX_KOYEB_IMAGE?.trim() ?? "")) {
    missing.push("CRABBOX_KOYEB_IMAGE");
  }
  const registrySecret = env.CRABBOX_KOYEB_REGISTRY_SECRET?.trim();
  if (registrySecret && !validKoyebSecretName(registrySecret)) {
    missing.push("CRABBOX_KOYEB_REGISTRY_SECRET");
  }
  try {
    canonicalAPIURL(env.CRABBOX_KOYEB_API_URL);
  } catch {
    missing.push("CRABBOX_KOYEB_API_URL");
  }
  if (!validKoyebName(env.CRABBOX_KOYEB_REGION?.trim() || defaultRegion)) {
    missing.push("CRABBOX_KOYEB_REGION");
  }
  if (!validKoyebName(env.CRABBOX_KOYEB_INSTANCE_TYPE?.trim() || defaultInstanceType)) {
    missing.push("CRABBOX_KOYEB_INSTANCE_TYPE");
  }
  if (env.CRABBOX_KOYEB_APP_TARGETS !== undefined) {
    try {
      koyebAppTargets(env);
    } catch {
      missing.push("CRABBOX_KOYEB_APP_TARGETS");
    }
  }
  return [...new Set(missing)];
}

export function koyebPrivateMeshAvailable(env: Env): boolean {
  const appName = env.KOYEB_APP_NAME?.trim() ?? "";
  const appID = env.KOYEB_APP_ID?.trim() ?? "";
  const organizationID = env.KOYEB_ORGANIZATION_ID?.trim() ?? "";
  const region = env.KOYEB_REGION?.trim() ?? "";
  return (
    validKoyebName(appName) &&
    appID === env.CRABBOX_KOYEB_APP_ID?.trim() &&
    organizationID === env.CRABBOX_KOYEB_ORGANIZATION_ID?.trim() &&
    region === (env.CRABBOX_KOYEB_REGION?.trim() || defaultRegion)
  );
}

function koyebConfiguration(env: Env, requestedAppID?: string): KoyebConfiguration {
  const registrySecret = env.CRABBOX_KOYEB_REGISTRY_SECRET?.trim();
  const targets = koyebAppTargets(env);
  const legacyAppID = env.CRABBOX_KOYEB_APP_ID!.trim();
  const organizationID = env.CRABBOX_KOYEB_ORGANIZATION_ID!.trim();
  const region = env.CRABBOX_KOYEB_REGION?.trim() || defaultRegion;
  const instanceType = env.CRABBOX_KOYEB_INSTANCE_TYPE?.trim() || defaultInstanceType;
  const selectedAppID = requestedAppID?.trim() || legacyAppID;
  const target = targets?.find((candidate) => candidate.appID === selectedAppID);
  if (targets && !target) {
    throw new ProviderResourceUnresolvedError("Koyeb app target registration is missing");
  }
  if (!targets && selectedAppID !== legacyAppID) {
    throw new ProviderResourceUnresolvedError("Koyeb app target registration is missing");
  }
  const appName =
    target?.appName ??
    (selectedAppID === legacyAppID && koyebPrivateMeshAvailable(env)
      ? env.KOYEB_APP_NAME!.trim()
      : undefined);
  return {
    apiURL: canonicalAPIURL(env.CRABBOX_KOYEB_API_URL),
    token: env.KOYEB_API_TOKEN!.trim(),
    organizationID,
    appID: selectedAppID,
    region,
    instanceType,
    image: env.CRABBOX_KOYEB_IMAGE!.trim(),
    ...(registrySecret ? { registrySecret } : {}),
    ...(appName ? { appName } : {}),
    managedTarget: Boolean(targets),
    readyPoolScope: koyebReadyPoolScope({
      organizationID,
      appID: legacyAppID,
      region,
      instanceType,
    }),
  };
}

function koyebAppTargets(env: Env): KoyebAppTarget[] | undefined {
  if (env.CRABBOX_KOYEB_APP_TARGETS === undefined) return undefined;
  const encoded = env.CRABBOX_KOYEB_APP_TARGETS;
  if (!encoded || encoded !== encoded.trim()) throw new Error("invalid Koyeb app target registry");
  let parsed: unknown;
  try {
    parsed = JSON.parse(encoded);
  } catch {
    throw new Error("invalid Koyeb app target registry");
  }
  if (!Array.isArray(parsed) || parsed.length === 0) {
    throw new Error("invalid Koyeb app target registry");
  }
  const organizationID = env.CRABBOX_KOYEB_ORGANIZATION_ID?.trim() ?? "";
  const legacyAppID = env.CRABBOX_KOYEB_APP_ID?.trim() ?? "";
  const region = env.CRABBOX_KOYEB_REGION?.trim() || defaultRegion;
  const targets: KoyebAppTarget[] = [];
  const appIDs = new Set<string>();
  const appNames = new Set<string>();
  for (const value of parsed) {
    const entry = asObject(value);
    if (
      !hasOnlyKeys(entry, ["organizationID", "appID", "appName", "region"]) ||
      Object.keys(entry).length !== 4
    ) {
      throw new Error("invalid Koyeb app target registry");
    }
    const target = {
      organizationID: stringValue(entry["organizationID"]),
      appID: stringValue(entry["appID"]),
      appName: stringValue(entry["appName"]),
      region: stringValue(entry["region"]),
    };
    if (
      !uuidPattern.test(target.organizationID) ||
      !uuidPattern.test(target.appID) ||
      !validKoyebName(target.appName) ||
      !validKoyebName(target.region) ||
      target.organizationID !== organizationID ||
      target.region !== region ||
      appIDs.has(target.appID.toLowerCase()) ||
      appNames.has(target.appName)
    ) {
      throw new Error("invalid Koyeb app target registry");
    }
    appIDs.add(target.appID.toLowerCase());
    appNames.add(target.appName);
    targets.push(target);
  }
  const legacy = targets.find((target) => target.appID === legacyAppID);
  if (!legacy) throw new Error("invalid Koyeb app target registry");
  if (koyebPrivateMeshAvailable(env) && legacy.appName !== env.KOYEB_APP_NAME!.trim()) {
    throw new Error("invalid Koyeb app target registry");
  }
  return targets;
}

export function koyebRegisteredAppIDs(env: Env): string[] {
  return koyebAppTargets(env)?.map((target) => target.appID) ?? [env.CRABBOX_KOYEB_APP_ID!.trim()];
}

function canonicalAPIURL(value: string | undefined): string {
  const url = new URL(value?.trim() || defaultAPIURL);
  if (
    url.protocol !== "https:" ||
    url.username ||
    url.password ||
    url.search ||
    url.hash ||
    (url.pathname !== "/" && url.pathname !== "")
  ) {
    throw new Error("invalid Koyeb API URL");
  }
  return url.origin;
}

function validKoyebName(value: string): boolean {
  return /^[a-z0-9][a-z0-9-]{0,62}$/.test(value);
}

function validKoyebSecretName(value: string): boolean {
  return value.length >= 2 && validKoyebName(value);
}

function validCapacitySnapshot(value: KoyebCapacitySnapshot): boolean {
  if (
    !Array.isArray(value.observedServiceIDs) ||
    !Array.isArray(value.observedAppProvisioningServiceIDs) ||
    !Array.isArray(value.observedOrganizationServiceIDs) ||
    !Array.isArray(value.observedProvisioningServiceIDs)
  ) {
    return false;
  }
  const serviceIDs = [
    ...value.observedServiceIDs,
    ...value.observedAppProvisioningServiceIDs,
    ...value.observedOrganizationServiceIDs,
    ...value.observedProvisioningServiceIDs,
  ];
  const organizationServiceIDs = new Set(
    value.observedOrganizationServiceIDs.map((id) => id.toLowerCase()),
  );
  const appServiceIDs = new Set(value.observedServiceIDs.map((id) => id.toLowerCase()));
  return (
    Number.isSafeInteger(value.servicesByApp) &&
    value.servicesByApp > 0 &&
    Number.isSafeInteger(value.organizationServices) &&
    value.organizationServices > 0 &&
    Number.isSafeInteger(value.serviceProvisioningConcurrency) &&
    value.serviceProvisioningConcurrency > 0 &&
    Number.isSafeInteger(value.memoryMB) &&
    value.memoryMB > 0 &&
    Number.isSafeInteger(value.snapshotCapturedAt) &&
    value.snapshotCapturedAt > 0 &&
    Number.isSafeInteger(value.organizationServicesUsed) &&
    value.organizationServicesUsed >= 0 &&
    Number.isSafeInteger(value.organizationMemoryMBUsed) &&
    value.organizationMemoryMBUsed >= 0 &&
    Number.isSafeInteger(value.workerInstancesUsed) &&
    value.workerInstancesUsed >= 0 &&
    (value.workerInstanceLimit === undefined ||
      (Number.isSafeInteger(value.workerInstanceLimit) && value.workerInstanceLimit >= 0)) &&
    Number.isSafeInteger(value.workerMemoryMB) &&
    value.workerMemoryMB > 0 &&
    serviceIDs.every((id) => uuidPattern.test(id)) &&
    value.observedProvisioningServiceIDs.every((id) =>
      organizationServiceIDs.has(id.toLowerCase()),
    ) &&
    value.observedAppProvisioningServiceIDs.every((id) => appServiceIDs.has(id.toLowerCase())) &&
    appServiceIDs.size === value.observedServiceIDs.length &&
    new Set(value.observedAppProvisioningServiceIDs.map((id) => id.toLowerCase())).size ===
      value.observedAppProvisioningServiceIDs.length &&
    organizationServiceIDs.size === value.observedOrganizationServiceIDs.length &&
    new Set(value.observedProvisioningServiceIDs.map((id) => id.toLowerCase())).size ===
      value.observedProvisioningServiceIDs.length
  );
}

function sameOrganizationCapacity(
  left: KoyebCapacitySnapshot,
  right: KoyebCapacitySnapshot,
): boolean {
  return (
    left.servicesByApp === right.servicesByApp &&
    left.organizationServices === right.organizationServices &&
    left.serviceProvisioningConcurrency === right.serviceProvisioningConcurrency &&
    left.memoryMB === right.memoryMB &&
    left.snapshotCapturedAt === right.snapshotCapturedAt &&
    left.organizationServicesUsed === right.organizationServicesUsed &&
    left.organizationMemoryMBUsed === right.organizationMemoryMBUsed &&
    left.workerInstancesUsed === right.workerInstancesUsed &&
    left.workerInstanceLimit === right.workerInstanceLimit &&
    left.workerMemoryMB === right.workerMemoryMB &&
    sameStrings(left.observedOrganizationServiceIDs, right.observedOrganizationServiceIDs) &&
    sameStrings(left.observedProvisioningServiceIDs, right.observedProvisioningServiceIDs)
  );
}

function sameStrings(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

async function validatedPlan(
  client: KoyebClient,
  frozen: FrozenProvisioningPlan,
  lease: LeaseRecord,
): Promise<KoyebProvisioningPlan> {
  const storedData = asObject(frozen.data) as Partial<KoyebProvisioningPlan>;
  const data: Partial<KoyebProvisioningPlan> = {
    ...storedData,
    // Plans written before native mesh support implicitly used Tailscale.
    transport: storedData.transport ?? "tailscale",
  };
  const scope = await client.providerScope();
  const resource = frozen.resources[0];
  const privateHost = client.privateHost(data.serviceName ?? "");
  const registeredPlan =
    data.appName !== undefined || data.readyPoolScope !== undefined || data.capacity !== undefined;
  if (
    frozen.version !== 1 ||
    frozen.provider !== "koyeb" ||
    frozen.scope !== scope ||
    frozen.resources.length !== 1 ||
    !resource ||
    resource.scope !== scope ||
    data.version !== 1 ||
    !validKoyebName(data.serviceName ?? "") ||
    data.runnerLeaseID !== data.serviceName ||
    resource.cloudID !== data.serviceName ||
    resource.region !== data.region ||
    data.organizationID !== client.organizationID ||
    data.appID !== client.appID ||
    data.region !== client.region ||
    (registeredPlan && data.appName !== client.appName) ||
    (registeredPlan && data.readyPoolScope !== client.readyPoolScope) ||
    (data.capacity !== undefined && !validCapacitySnapshot(data.capacity)) ||
    !validKoyebName(data.instanceType ?? "") ||
    !immutableImagePattern.test(data.image ?? "") ||
    (data.registrySecret !== undefined && !validKoyebSecretName(data.registrySecret)) ||
    lease.provider !== "koyeb" ||
    lease.providerScope !== scope ||
    (registeredPlan && lease.providerProject !== client.appID) ||
    (registeredPlan && lease.region !== client.region) ||
    data.leaseID !== lease.id ||
    data.slug !== (lease.slug ?? "") ||
    data.owner !== (lease.providerOwner || lease.owner) ||
    data.org !== lease.org ||
    data.generation !== lease.createAttemptGeneration ||
    !Number.isSafeInteger(data.ttlSeconds) ||
    (data.ttlSeconds ?? 0) <= 0 ||
    (data.ttlSeconds ?? 0) > 86_400 ||
    !Number.isSafeInteger(data.idleTimeoutSeconds) ||
    (data.idleTimeoutSeconds ?? 0) <= 0 ||
    (data.idleTimeoutSeconds ?? 0) > 86_400 ||
    typeof data.sshPublicKey !== "string" ||
    !data.sshPublicKey.trim() ||
    !["tailscale", "koyeb-mesh"].includes(data.transport ?? "") ||
    (data.transport === "koyeb-mesh" && (!privateHost || data.privateHost !== privateHost)) ||
    (data.transport === "tailscale" && data.privateHost !== undefined) ||
    (data.transport === "tailscale" && !validKoyebName(data.tailscaleHostname ?? "")) ||
    typeof data.tailscaleHostname !== "string" ||
    !Array.isArray(data.tailscaleTags) ||
    (data.transport === "tailscale" && data.tailscaleTags.length === 0) ||
    data.tailscaleTags.some((tag) => !/^tag:[a-z0-9][a-z0-9-]{0,62}$/.test(tag))
  ) {
    throw new Error("Koyeb provisioning plan is invalid or belongs to another context");
  }
  await client.validateTarget();
  return data as KoyebProvisioningPlan;
}

function continuationState(value: unknown): KoyebContinuationState {
  const state = asObject(value) as Partial<KoyebContinuationState>;
  if (
    state.version !== 1 ||
    !["dispatch", "discover", "observe", "bootstrap", "delete", "confirm-delete"].includes(
      state.action ?? "",
    ) ||
    (state.serviceID !== undefined && !uuidPattern.test(state.serviceID)) ||
    (state.deploymentID !== undefined && !uuidPattern.test(state.deploymentID))
  ) {
    throw new Error("Koyeb continuation state is invalid");
  }
  return structuredClone(state as KoyebContinuationState);
}

function createServiceRequest(plan: KoyebProvisioningPlan, sandboxSecret: string): JSONRecord {
  const docker: JSONRecord = {
    image: plan.image,
    ...(plan.registrySecret ? { image_registry_secret: plan.registrySecret } : {}),
  };
  return {
    app_id: plan.appID,
    life_cycle: {
      delete_after_create: plan.ttlSeconds,
      delete_after_sleep: plan.idleTimeoutSeconds,
    },
    definition: {
      name: plan.serviceName,
      type: "SANDBOX",
      docker,
      env: [
        { key: "SANDBOX_SECRET", value: sandboxSecret },
        ...Object.entries(ownershipEnvironment(plan)).map(([key, value]) => ({ key, value })),
      ],
      regions: [plan.region],
      instance_types: [{ type: plan.instanceType }],
      ...(plan.transport === "koyeb-mesh" ? { mesh: "DEPLOYMENT_MESH_ENABLED" } : {}),
      // Koyeb's Sandbox runtime owns 3030 for management and 3031 for its TCP
      // proxy. Exposing target port 22 directly makes Koyeb select it as PORT,
      // moving management off 3030 before Crabbox can bootstrap the runner.
      ports:
        plan.transport === "koyeb-mesh"
          ? [
              { port: 3030, protocol: "http" },
              { port: 3031, protocol: "tcp" },
            ]
          : [{ port: 3030, protocol: "http" }],
      routes:
        plan.transport === "koyeb-mesh"
          ? []
          : [
              {
                port: 3030,
                path: `${managementRoute}/`,
                security_policies: { api_keys: [sandboxSecret] },
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
  };
}

async function discoverService(
  client: KoyebClient,
  plan: KoyebProvisioningPlan,
  expectedSandboxSecret?: string,
): Promise<
  | { kind: "missing" }
  | { kind: "conflict" }
  | { kind: "pending"; service: KoyebService }
  | { kind: "owned"; service: KoyebService; value: OwnedDeployment }
> {
  const matches = (await client.listServices(plan.serviceName)).filter(
    (service) => service.name === plan.serviceName,
  );
  if (matches.length === 0) return { kind: "missing" };
  if (matches.length !== 1) return { kind: "conflict" };
  const service = matches[0]!;
  const observation = await observeService(client, plan, service, expectedSandboxSecret);
  return observation.kind === "owned"
    ? { kind: "owned", service, value: observation.value }
    : observation.kind === "pending"
      ? { kind: "pending", service }
      : { kind: observation.kind };
}

async function observeByID(
  client: KoyebClient,
  plan: KoyebProvisioningPlan,
  serviceID: string,
  expectedSandboxSecret?: string,
): Promise<Observation> {
  const service = await client.getService(serviceID);
  return service
    ? observeService(client, plan, service, expectedSandboxSecret)
    : { kind: "missing" };
}

async function observeService(
  client: KoyebClient,
  plan: KoyebProvisioningPlan,
  service: KoyebService,
  expectedSandboxSecret?: string,
): Promise<Observation> {
  if (!serviceMatchesPlan(service, plan)) return { kind: "conflict" };
  if (
    service.activeDeploymentID &&
    service.latestDeploymentID &&
    service.activeDeploymentID !== service.latestDeploymentID
  ) {
    // A service-level delete also removes the latest deployment. Do not trust
    // the active deployment as proof of ownership while Koyeb reports another
    // deployment generation that has not been validated against this lease.
    return { kind: "pending", service };
  }
  const deploymentID = service.activeDeploymentID || service.latestDeploymentID;
  if (!deploymentID) return { kind: "pending", service };
  const deployment = await client.getDeployment(deploymentID);
  if (!deployment) return { kind: "pending", service };
  const sandboxSecret = deploymentSandboxSecret(deployment);
  if (
    !deploymentMatchesPlan(deployment, service, plan, expectedSandboxSecret) ||
    !sandboxSecret ||
    (expectedSandboxSecret !== undefined && sandboxSecret !== expectedSandboxSecret)
  ) {
    return { kind: "conflict" };
  }
  const sandbox = asObject(asObject(deployment.metadata)["sandbox"]);
  const publicURL = optionalString(sandbox["public_url"]);
  const routingKey = optionalString(sandbox["routing_key"]);
  return {
    kind: "owned",
    value: {
      service,
      deployment,
      sandboxSecret,
      ...(publicURL ? { publicURL } : {}),
      ...(routingKey ? { routingKey } : {}),
    },
  };
}

function managementTarget(
  owned: OwnedDeployment,
  plan: KoyebProvisioningPlan,
): { baseURL: string; routingKey?: string } | undefined {
  if (plan.transport === "koyeb-mesh") {
    return plan.privateHost ? { baseURL: `http://${plan.privateHost}:3030` } : undefined;
  }
  return owned.publicURL && owned.routingKey
    ? { baseURL: owned.publicURL, routingKey: owned.routingKey }
    : undefined;
}

function serviceMatchesPlan(service: KoyebService, plan: KoyebProvisioningPlan): boolean {
  return (
    uuidPattern.test(service.id) &&
    service.name === plan.serviceName &&
    service.type === "SANDBOX" &&
    service.organizationID === plan.organizationID &&
    service.appID === plan.appID &&
    lifeCycleMatchesPlan(service.lifeCycle, plan)
  );
}

function lifeCycleMatchesPlan(lifeCycle: JSONRecord, plan: KoyebProvisioningPlan): boolean {
  return (
    hasOnlyKeys(lifeCycle, ["delete_after_create", "delete_after_sleep"]) &&
    lifeCycle["delete_after_create"] === plan.ttlSeconds &&
    lifeCycle["delete_after_sleep"] === plan.idleTimeoutSeconds
  );
}

function deploymentMatchesPlan(
  deployment: KoyebDeployment,
  service: KoyebService,
  plan: KoyebProvisioningPlan,
  expectedSandboxSecret?: string,
): boolean {
  const definition = deployment.definition;
  const docker = asObject(definition["docker"]);
  const regions = definition["regions"];
  const instanceTypes = definition["instance_types"];
  const scalings = definition["scalings"];
  const ports = definition["ports"];
  const routes = definition["routes"];
  const proxyPorts = definition["proxy_ports"];
  const sandboxSecret = deploymentSandboxSecret(deployment);
  return (
    uuidPattern.test(deployment.id) &&
    deployment.organizationID === plan.organizationID &&
    deployment.appID === plan.appID &&
    deployment.serviceID === service.id &&
    definition["name"] === plan.serviceName &&
    definition["type"] === "SANDBOX" &&
    meshMatchesPlan(definition["mesh"], plan) &&
    dockerMatchesPlan(docker, plan) &&
    exactStringArray(regions, [plan.region]) &&
    instanceTypesMatchPlan(instanceTypes, plan) &&
    scalingsMatchPlan(scalings, plan) &&
    portsMatchPlan(ports, plan) &&
    routesMatchPlan(routes, sandboxSecret, plan) &&
    optionalEmptyArray(proxyPorts) &&
    Boolean(sandboxSecret) &&
    (expectedSandboxSecret === undefined || sandboxSecret === expectedSandboxSecret) &&
    environmentMatchesPlan(definition["env"], plan, sandboxSecret)
  );
}

function dockerMatchesPlan(docker: JSONRecord, plan: KoyebProvisioningPlan): boolean {
  return (
    hasOnlyKeys(docker, [
      "image",
      "command",
      "args",
      "image_registry_secret",
      "entrypoint",
      "privileged",
    ]) &&
    docker["image"] === plan.image &&
    (plan.registrySecret
      ? docker["image_registry_secret"] === plan.registrySecret
      : optionalEmptyString(docker["image_registry_secret"])) &&
    optionalEmptyString(docker["command"]) &&
    optionalEmptyArray(docker["args"]) &&
    optionalEmptyArray(docker["entrypoint"]) &&
    optionalFalse(docker["privileged"])
  );
}

function instanceTypesMatchPlan(value: unknown, plan: KoyebProvisioningPlan): boolean {
  if (!Array.isArray(value) || value.length !== 1) return false;
  const instanceType = asObject(value[0]);
  return (
    hasOnlyKeys(instanceType, ["type", "scopes"]) &&
    instanceType["type"] === plan.instanceType &&
    optionalRegionScopes(instanceType["scopes"], plan.region)
  );
}

function scalingsMatchPlan(value: unknown, plan: KoyebProvisioningPlan): boolean {
  if (!Array.isArray(value) || value.length !== 1) return false;
  const scaling = asObject(value[0]);
  const targets = scaling["targets"];
  if (
    !hasOnlyKeys(scaling, ["scopes", "min", "max", "targets"]) ||
    !optionalRegionScopes(scaling["scopes"], plan.region) ||
    scaling["min"] !== 1 ||
    scaling["max"] !== 1 ||
    !Array.isArray(targets) ||
    targets.length !== 0
  ) {
    return false;
  }
  return true;
}

function meshMatchesPlan(value: unknown, plan: KoyebProvisioningPlan): boolean {
  return plan.transport === "koyeb-mesh"
    ? value === "DEPLOYMENT_MESH_ENABLED"
    : nullish(value) || value === "DEPLOYMENT_MESH_AUTO";
}

function portsMatchPlan(value: unknown, plan: KoyebProvisioningPlan): boolean {
  if (!Array.isArray(value)) return false;
  const actual = value
    .map((item) => asObject(item))
    .map((port) => {
      if (!hasOnlyKeys(port, ["port", "protocol"])) return "";
      return `${String(port["port"])}:${String(port["protocol"])}`;
    })
    .toSorted();
  const expected =
    plan.transport === "koyeb-mesh"
      ? [
          ["3030:http", "3031:tcp"],
          // Plans created before the Sandbox proxy contract exposed SSH
          // directly. Koyeb canonicalized that payload to this order. Keep
          // recognizing it so failed legacy leases remain safely reclaimable.
          ["22:tcp", "3030:http"],
        ]
      : [["3030:http"]];
  return expected.some(
    (ports) =>
      actual.length === ports.length && ports.every((port, index) => actual[index] === port),
  );
}

function routesMatchPlan(
  value: unknown,
  sandboxSecret: string,
  plan: KoyebProvisioningPlan,
): boolean {
  if (plan.transport === "koyeb-mesh") return Array.isArray(value) && value.length === 0;
  if (!Array.isArray(value) || value.length !== 1) return false;
  const route = asObject(value[0]);
  return (
    hasOnlyKeys(route, ["port", "path", "security_policies"]) &&
    route["port"] === 3030 &&
    route["path"] === `${managementRoute}/` &&
    sandboxRouteSecurityPolicies(route["security_policies"], sandboxSecret)
  );
}

function environmentMatchesPlan(
  value: unknown,
  plan: KoyebProvisioningPlan,
  sandboxSecret: string,
): boolean {
  if (!Array.isArray(value)) return false;
  const expected = { SANDBOX_SECRET: sandboxSecret, ...ownershipEnvironment(plan) };
  if (value.length !== Object.keys(expected).length) return false;
  const seen = new Set<string>();
  for (const item of value) {
    const entry = asObject(item);
    if (
      !hasOnlyKeys(entry, ["scopes", "key", "value", "secret"]) ||
      typeof entry["key"] !== "string" ||
      typeof entry["value"] !== "string" ||
      !optionalEmptyString(entry["secret"]) ||
      !optionalRegionScopes(entry["scopes"], plan.region) ||
      seen.has(entry["key"]) ||
      expected[entry["key"] as keyof typeof expected] !== entry["value"]
    ) {
      return false;
    }
    seen.add(entry["key"]);
  }
  return seen.size === Object.keys(expected).length;
}

function sandboxRouteSecurityPolicies(value: unknown, sandboxSecret: string): boolean {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const policies = value as JSONRecord;
  return (
    hasOnlyKeys(policies, ["basic_auths", "api_keys"]) &&
    optionalEmptyArray(policies["basic_auths"]) &&
    exactStringArray(policies["api_keys"], [sandboxSecret])
  );
}

function exactStringArray(value: unknown, expected: string[]): boolean {
  return (
    Array.isArray(value) &&
    value.length === expected.length &&
    value.every((item, index) => item === expected[index])
  );
}

function optionalRegionScopes(value: unknown, region: string): boolean {
  return (
    nullish(value) ||
    (Array.isArray(value) &&
      (value.length === 0 || (value.length === 1 && value[0] === `region:${region}`)))
  );
}

function optionalEmptyArray(value: unknown): boolean {
  return nullish(value) || (Array.isArray(value) && value.length === 0);
}

function optionalEmptyString(value: unknown): boolean {
  return nullish(value) || value === "";
}

function optionalFalse(value: unknown): boolean {
  return nullish(value) || value === false;
}

function nullish(value: unknown): boolean {
  return value === undefined || value === null;
}

function hasOnlyKeys(value: JSONRecord, allowed: string[]): boolean {
  const allowedKeys = new Set(allowed);
  return Object.keys(value).every((key) => allowedKeys.has(key));
}

function ownershipEnvironment(plan: KoyebProvisioningPlan): Record<string, string> {
  return {
    CRABBOX_MANAGED_BY: "crabbox",
    CRABBOX_PROVIDER: "koyeb",
    CRABBOX_LEASE_ID: plan.leaseID,
    CRABBOX_LEASE_SLUG: providerLabelValue(plan.slug),
    CRABBOX_LEASE_OWNER: providerLabelValue(plan.owner),
    CRABBOX_LEASE_ORG: providerLabelValue(plan.org),
    CRABBOX_PROVISIONING_GENERATION: plan.generation,
  };
}

function ownershipLabels(plan: KoyebProvisioningPlan): Record<string, string> {
  return {
    crabbox: "true",
    created_by: "crabbox",
    lease: plan.leaseID,
    slug: providerLabelValue(plan.slug),
    owner: providerLabelValue(plan.owner),
    provider: "koyeb",
    server_type: providerLabelValue(plan.instanceType),
  };
}

function imageIdentity(plan: KoyebProvisioningPlan): LeaseImageIdentity {
  return {
    id: plan.image,
    source: "explicit",
    provider: "koyeb",
    kind: imageKind,
    region: plan.region,
    scope: plan.readyPoolScope ?? koyebReadyPoolScope(plan),
    ...(plan.registrySecret ? { sourceID: plan.registrySecret } : {}),
  };
}

export function koyebReadyPoolScope(
  scope: Pick<KoyebConfiguration, "organizationID" | "appID" | "region" | "instanceType">,
): string {
  return `koyeb:${scope.organizationID}:${scope.appID}:${scope.region}:${scope.instanceType}:${koyebPoolBootstrap}`;
}

function deploymentEnvironment(deployment: KoyebDeployment): Record<string, string> {
  const values: Record<string, string> = {};
  const environment = deployment.definition["env"];
  if (!Array.isArray(environment)) return values;
  for (const item of environment) {
    const entry = asObject(item);
    if (typeof entry["key"] === "string" && typeof entry["value"] === "string") {
      values[entry["key"]] = entry["value"];
    }
  }
  return values;
}

function deploymentSandboxSecret(deployment: KoyebDeployment): string {
  const value = deploymentEnvironment(deployment)["SANDBOX_SECRET"] ?? "";
  return /^[A-Za-z0-9_-]{32,128}$/.test(value) ? value : "";
}

function runnerReady(stdout: string, plan: KoyebProvisioningPlan): RunnerReady | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(stdout.trim());
  } catch {
    return undefined;
  }
  const value = asObject(parsed);
  const ssh = asObject(value["ssh"]);
  const host = stringValue(ssh["host"]);
  const hostKey = sshPublicKeyIdentity(stringValue(ssh["hostKey"]));
  if (plan.transport === "koyeb-mesh") {
    const network = asObject(value["network"]);
    if (
      value["schema"] !== "crabbox-koyeb-sandbox-runner/v2" ||
      value["leaseId"] !== plan.runnerLeaseID ||
      ssh["user"] !== "crabbox" ||
      ssh["port"] !== 22 ||
      host !== plan.privateHost ||
      network["transport"] !== "koyeb-mesh" ||
      network["privateHost"] !== plan.privateHost ||
      !/^ssh-ed25519 [A-Za-z0-9+/]+={0,2}$/.test(hostKey)
    ) {
      return undefined;
    }
    return {
      transport: "koyeb-mesh",
      host,
      user: "crabbox",
      port: "22",
      hostKey,
    };
  }
  const tailscale = asObject(value["tailscale"]);
  const ipv4 = stringValue(tailscale["ipv4"]);
  const dnsName = optionalString(tailscale["dnsName"]);
  if (
    value["schema"] !== "crabbox-koyeb-sandbox-runner/v1" ||
    value["leaseId"] !== plan.runnerLeaseID ||
    ssh["user"] !== "crabbox" ||
    ssh["port"] !== 22 ||
    host !== ipv4 ||
    !validTailscaleIPv4(ipv4) ||
    (dnsName !== undefined && !validDNSName(dnsName)) ||
    !/^ssh-ed25519 [A-Za-z0-9+/]+={0,2}$/.test(hostKey)
  ) {
    return undefined;
  }
  return {
    transport: "tailscale",
    host: dnsName || ipv4,
    ipv4,
    ...(dnsName ? { fqdn: dnsName } : {}),
    user: "crabbox",
    port: "22",
    hostKey,
  };
}

function validTailscaleIPv4(value: string): boolean {
  const parts = value.split(".").map(Number);
  if (
    parts.length !== 4 ||
    parts.some((part) => !Number.isInteger(part) || part < 0 || part > 255)
  ) {
    return false;
  }
  return parts[0] === 100 && parts[1]! >= 64 && parts[1]! <= 127;
}

function validDNSName(value: string): boolean {
  return (
    value.length <= 253 &&
    value === value.toLowerCase() &&
    value.split(".").every((label) => validKoyebName(label))
  );
}

function machineForService(
  service: KoyebService,
  plan: KoyebProvisioningPlan,
  host: string,
  labels: Record<string, string> = ownershipLabels(plan),
): ProviderMachine {
  return {
    provider: "koyeb",
    id: 0,
    cloudID: service.id,
    region: plan.region,
    name: service.name,
    status: service.status.toLowerCase(),
    serverType: plan.instanceType,
    host,
    labels,
  };
}

async function assertKoyebCleanupOwner(assertOwner?: () => Promise<void>): Promise<void> {
  if (!assertOwner) {
    throw new ProviderResourceUnresolvedError(
      "Koyeb cleanup requires current coordinator authority",
    );
  }
  await assertOwner();
}

function cleanupIdentity(
  client: KoyebClient,
  lease: LeaseRecord,
  value?: string,
): KoyebCleanupIdentity {
  let identity: JSONRecord = {};
  if (typeof value === "string" && value.length <= 4096) {
    try {
      identity = asObject(JSON.parse(value));
    } catch {
      // Missing or malformed retained evidence cannot authorize an existing service.
    }
  }
  if (
    identity["schema"] !== "crabbox-koyeb-cleanup/v1" ||
    identity["scope"] !== lease.providerScope ||
    identity["organizationID"] !== client.organizationID ||
    identity["appID"] !== client.appID ||
    identity["region"] !== client.region ||
    (lease.region !== undefined && identity["region"] !== lease.region) ||
    identity["leaseID"] !== lease.id ||
    identity["generation"] !== lease.createAttemptGeneration ||
    identity["serviceID"] !== lease.cloudID ||
    !uuidPattern.test(stringValue(identity["deploymentID"])) ||
    ![identity["ttlSeconds"], identity["idleTimeoutSeconds"]].every(
      (seconds) =>
        typeof seconds === "number" &&
        Number.isSafeInteger(seconds) &&
        seconds > 0 &&
        seconds <= 86_400,
    )
  ) {
    throw new ProviderResourceUnresolvedError(
      "Koyeb lease cleanup allocation identity is missing or inconsistent",
    );
  }
  return identity as unknown as KoyebCleanupIdentity;
}

function planForLeaseCleanup(
  client: KoyebClient,
  lease: LeaseRecord,
  identity?: KoyebCleanupIdentity,
): KoyebProvisioningPlan {
  const image = lease.image;
  const region = identity?.region ?? lease.region;
  if (
    !image ||
    image.provider !== "koyeb" ||
    image.source !== "explicit" ||
    image.kind !== imageKind ||
    !immutableImagePattern.test(image.id) ||
    !region ||
    image.region !== region ||
    (image.sourceID !== undefined && !validKoyebSecretName(image.sourceID))
  ) {
    throw new ProviderResourceUnresolvedError("Koyeb lease cleanup image identity is incomplete");
  }
  const serviceName = leaseProviderName(lease.id, lease.slug);
  const expectedPrivateHost = client.privateHost(serviceName);
  const transport: KoyebTransport =
    !lease.tailscale?.enabled && lease.host === expectedPrivateHost ? "koyeb-mesh" : "tailscale";
  const privateHost = transport === "koyeb-mesh" ? client.privateHost(serviceName) : undefined;
  if (transport === "koyeb-mesh" && !privateHost) {
    throw new ProviderResourceUnresolvedError("Koyeb lease cleanup mesh identity is incomplete");
  }
  return {
    version: 1,
    serviceName,
    runnerLeaseID: serviceName,
    organizationID: identity?.organizationID ?? client.organizationID,
    appID: identity?.appID ?? client.appID,
    ...(lease.providerProject && client.appName ? { appName: client.appName } : {}),
    region,
    instanceType: lease.serverType,
    image: image.id,
    ...(image.sourceID ? { registrySecret: image.sourceID } : {}),
    leaseID: identity?.leaseID ?? lease.id,
    slug: lease.slug ?? "",
    owner: lease.providerOwner || lease.owner,
    org: lease.org,
    generation: identity?.generation ?? lease.createAttemptGeneration!,
    ttlSeconds: identity?.ttlSeconds ?? lease.ttlSeconds,
    idleTimeoutSeconds:
      identity?.idleTimeoutSeconds ?? lease.idleTimeoutSeconds ?? lease.ttlSeconds,
    sshPublicKey: "cleanup-only",
    transport,
    ...(privateHost ? { privateHost } : {}),
    tailscaleHostname: lease.tailscale?.hostname || leaseProviderName(lease.id, lease.slug),
    tailscaleTags: lease.tailscale?.tags?.length ? [...lease.tailscale.tags] : ["tag:crabbox"],
    ...(lease.providerProject ? { readyPoolScope: client.readyPoolScope } : {}),
  };
}

function inventoryMachine(
  client: KoyebClient,
  service: KoyebService,
  deployment: KoyebDeployment,
): ProviderMachine | undefined {
  const definition = deployment.definition;
  const environment = deploymentEnvironment(deployment);
  const leaseID = environment["CRABBOX_LEASE_ID"] ?? "";
  const slug = environment["CRABBOX_LEASE_SLUG"] ?? "";
  const owner = environment["CRABBOX_LEASE_OWNER"] ?? "";
  const generation = environment["CRABBOX_PROVISIONING_GENERATION"] ?? "";
  const org = environment["CRABBOX_LEASE_ORG"] ?? "";
  const docker = asObject(definition["docker"]);
  const regions = definition["regions"];
  const instanceTypes = definition["instance_types"];
  const region = Array.isArray(regions) ? stringValue(regions[0]) : "";
  const instanceType = Array.isArray(instanceTypes)
    ? stringValue(asObject(instanceTypes[0])["type"])
    : "";
  const image = stringValue(docker["image"]);
  const registrySecret = optionalString(docker["image_registry_secret"]);
  const transport: KoyebTransport =
    definition["mesh"] === "DEPLOYMENT_MESH_ENABLED" ? "koyeb-mesh" : "tailscale";
  const privateHost = transport === "koyeb-mesh" ? client.privateHost(service.name) : undefined;
  const deleteAfterCreate = service.lifeCycle["delete_after_create"];
  const deleteAfterSleep = service.lifeCycle["delete_after_sleep"];
  if (
    !/^cbx_[a-f0-9]{12}$/.test(leaseID) ||
    !validKoyebName(slug) ||
    !owner ||
    !org ||
    !generation ||
    !validKoyebName(region) ||
    !validKoyebName(instanceType) ||
    !immutableImagePattern.test(image) ||
    !Number.isSafeInteger(deleteAfterCreate) ||
    !Number.isSafeInteger(deleteAfterSleep) ||
    (registrySecret !== undefined && !validKoyebSecretName(registrySecret)) ||
    (transport === "koyeb-mesh" && !privateHost)
  ) {
    return undefined;
  }
  const plan: KoyebProvisioningPlan = {
    version: 1,
    serviceName: leaseProviderName(leaseID, slug),
    runnerLeaseID: leaseProviderName(leaseID, slug),
    organizationID: service.organizationID,
    appID: service.appID,
    region,
    instanceType,
    image,
    ...(registrySecret ? { registrySecret } : {}),
    leaseID,
    slug,
    owner,
    org,
    generation,
    ttlSeconds: deleteAfterCreate as number,
    idleTimeoutSeconds: deleteAfterSleep as number,
    sshPublicKey: "inventory-only",
    transport,
    ...(privateHost ? { privateHost } : {}),
    tailscaleHostname: service.name,
    tailscaleTags: ["tag:crabbox"],
  };
  if (!serviceMatchesPlan(service, plan) || !deploymentMatchesPlan(deployment, service, plan)) {
    return undefined;
  }
  return machineForService(service, plan, "");
}

function inventoryInteger(
  value: JSONRecord,
  field: "limit" | "offset" | "count",
  subject = "service",
): number | undefined {
  const raw = value[field];
  if (raw === undefined) return undefined;
  const parsed = typeof raw === "string" && /^\d+$/.test(raw) ? Number(raw) : raw;
  if (typeof parsed !== "number" || !Number.isSafeInteger(parsed) || parsed < 0) {
    throw new Error(`koyeb ${subject} inventory ${field} is malformed`);
  }
  return parsed;
}

function quotaInteger(value: JSONRecord, field: string): number | undefined {
  const raw = value[field];
  if (typeof raw !== "string" || !/^\d+$/.test(raw)) return undefined;
  const parsed = Number(raw);
  return Number.isSafeInteger(parsed) ? parsed : undefined;
}

function usageInteger(value: JSONRecord, field: string): number | undefined {
  const raw = value[field];
  return typeof raw === "number" && Number.isSafeInteger(raw) && raw >= 0 ? raw : undefined;
}

function quotaStrings(value: JSONRecord, field: string): string[] {
  const raw = value[field];
  if (raw === undefined) return [];
  if (
    !Array.isArray(raw) ||
    raw.some((entry) => typeof entry !== "string" || !validKoyebName(entry)) ||
    new Set(raw).size !== raw.length
  ) {
    throw capacityEvidenceError(`organization ${field} quota is malformed`);
  }
  return raw;
}

function quotaIntegerMap(value: JSONRecord, field: string): Record<string, number> {
  const raw = value[field];
  if (raw === undefined) return {};
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
    throw capacityEvidenceError(`organization ${field} quota is malformed`);
  }
  const parsed: Record<string, number> = {};
  for (const [key, entry] of Object.entries(raw)) {
    if (!validKoyebName(key) || typeof entry !== "string" || !/^\d+$/.test(entry)) {
      throw capacityEvidenceError(`organization ${field} quota is malformed`);
    }
    const count = Number(entry);
    if (!Number.isSafeInteger(count)) {
      throw capacityEvidenceError(`organization ${field} quota is malformed`);
    }
    parsed[key] = count;
  }
  return parsed;
}

function capacityEvidenceError(detail: string): ProviderResourceUnresolvedError {
  return new ProviderResourceUnresolvedError(`Koyeb ${detail}`);
}

function observedProvisioningServiceIDs(
  services: readonly KoyebService[],
  deployments: readonly KoyebDeployment[],
): string[] {
  const deploymentsByID = new Map(
    deployments.map((deployment) => [deployment.id.toLowerCase(), deployment]),
  );
  const observed = new Set(
    services
      .filter((service) => provisioningServiceStatuses.has(service.status))
      .map((service) => service.id.toLowerCase()),
  );
  for (const service of services) {
    const deploymentIDs = [service.activeDeploymentID, service.latestDeploymentID].filter(
      (value, index, values): value is string => Boolean(value) && values.indexOf(value) === index,
    );
    if (deploymentIDs.length === 0 || deploymentIDs.some((id) => !uuidPattern.test(id))) {
      throw capacityEvidenceError("service deployment identity is unresolved");
    }
    if (
      service.activeDeploymentID &&
      service.latestDeploymentID &&
      service.activeDeploymentID !== service.latestDeploymentID
    ) {
      observed.add(service.id.toLowerCase());
    }
    for (const deploymentID of deploymentIDs) {
      const deployment = deploymentsByID.get(deploymentID.toLowerCase());
      if (
        !deployment ||
        deployment.id !== deploymentID ||
        deployment.organizationID !== service.organizationID ||
        deployment.appID !== service.appID ||
        deployment.serviceID !== service.id
      ) {
        throw capacityEvidenceError("service deployment inventory is inconsistent");
      }
      if (provisioningDeploymentStatuses.has(deployment.status)) {
        observed.add(service.id.toLowerCase());
      }
    }
  }
  return [...observed];
}

function memoryStringMB(value: string): number | undefined {
  const match = /^(\d+)(B|KB|MB|GB|TB)$/i.exec(value.trim());
  if (!match) return undefined;
  const quantity = Number(match[1]);
  if (!Number.isSafeInteger(quantity) || quantity <= 0) return undefined;
  const multiplier = { B: 1 / (1024 * 1024), KB: 1 / 1024, MB: 1, GB: 1024, TB: 1024 * 1024 }[
    match[2]!.toUpperCase()
  ];
  const memoryMB = quantity * multiplier!;
  return Number.isSafeInteger(memoryMB) && memoryMB > 0 ? memoryMB : undefined;
}

function koyebService(value: unknown): KoyebService {
  const service = asObject(value);
  const parsed: KoyebService = {
    id: stringValue(service["id"]),
    name: stringValue(service["name"]),
    type: stringValue(service["type"]),
    organizationID: stringValue(service["organization_id"]),
    appID: stringValue(service["app_id"]),
    status: stringValue(service["status"]),
    lifeCycle: asObject(service["life_cycle"]),
  };
  const activeDeploymentID = optionalString(service["active_deployment_id"]);
  const latestDeploymentID = optionalString(service["latest_deployment_id"]);
  if (activeDeploymentID) parsed.activeDeploymentID = activeDeploymentID;
  if (latestDeploymentID) parsed.latestDeploymentID = latestDeploymentID;
  return parsed;
}

function koyebDeployment(value: unknown): KoyebDeployment {
  const deployment = asObject(value);
  return {
    id: stringValue(deployment["id"]),
    organizationID: stringValue(deployment["organization_id"]),
    appID: stringValue(deployment["app_id"]),
    serviceID: stringValue(deployment["service_id"]),
    status: stringValue(deployment["status"]),
    definition: asObject(deployment["definition"]),
    metadata: asObject(deployment["metadata"]),
  };
}

function asObject(value: unknown): JSONRecord {
  return value && typeof value === "object" && !Array.isArray(value) ? (value as JSONRecord) : {};
}

function stringValue(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function optionalString(value: unknown): string | undefined {
  const string = stringValue(value).trim();
  return string || undefined;
}

function requireUUID(value: string, name: string): void {
  if (!uuidPattern.test(value)) throw new Error(`invalid Koyeb ${name}`);
}

function managementBaseURL(value: string, publicEdge: boolean): string {
  const url = new URL(value.trim());
  const validPublic = publicEdge && url.protocol === "https:";
  const validPrivate =
    !publicEdge &&
    url.protocol === "http:" &&
    url.port === "3030" &&
    (url.pathname === "/" || url.pathname === "") &&
    url.hostname.endsWith(".internal") &&
    validDNSName(url.hostname);
  if ((!validPublic && !validPrivate) || url.username || url.password || url.search || url.hash) {
    throw new Error("koyeb sandbox management URL is malformed");
  }
  // The public Sandbox endpoint is mounted below /koyeb-sandbox. A private
  // mesh address reaches port 3030 directly, where the API is rooted at /
  // (/health, /run, and the other operation paths).
  if (publicEdge) url.pathname = `${url.pathname.replace(/\/+$/, "")}${managementRoute}`;
  return url.toString().replace(/\/+$/, "");
}

function validRoutingKey(value: string): boolean {
  return value.length > 0 && value.length <= 512 && !/[\r\n]/.test(value);
}

function serviceTerminal(status: string): boolean {
  return ["UNHEALTHY", "DELETING", "DELETED", "PAUSED"].includes(status);
}

function deploymentTerminal(status: string): boolean {
  return ["CANCELED", "UNHEALTHY", "STOPPED", "ERROR"].includes(status);
}

function randomSecret(): string {
  return btoa(
    Array.from(crypto.getRandomValues(new Uint8Array(32)), (byte) =>
      String.fromCharCode(byte),
    ).join(""),
  )
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replaceAll("=", "");
}

async function sha256Hex(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}
