import {
  AzureClient,
  azureOwnedDeleteClaimKey,
  azureCrabboxSSHRulesMatch,
  azureAttemptNameSeed,
  azureDiskAttachmentsMatchVM,
  azureImageReference,
  azureLabelsFromTags,
  azureNICHasCascadeDelete,
  azureNICHasDisqualifyingServiceAssociation,
  azureNICReferencesOnlyPIP,
  azureProvisioningCandidatesForConfig,
  azurePublicIPAttachmentMatchesNIC,
  azureRandomAdminPassword,
  azureReconciliationResourceIdentity,
  azureRegionCandidates,
  azureRegionalName,
  azureTagsFromLabels,
  azureVMHasCascadeDelete,
  azureVMHasDataDisks,
  azureVMReferencesOnlyNIC,
  azureWindowsBootstrapCommand,
  preserveNonCrabboxRules,
  type AzureOwnedDeleteClaimStorage,
  type AzureDisk,
  type AzureNIC,
  type AzurePublicIP,
  type AzureVM,
} from "./azure";
import { azureWindowsBootstrapPowerShell } from "./bootstrap";
import { sshPorts, type LeaseConfig } from "./config";
import type { CoordinatorStorageView } from "./coordinator-runtime";
import { leaseProviderLabels, providerLabelsOwnedByLease } from "./provider-labels";
import type {
  FrozenProvisioningPlan,
  ProviderResumableProvisioning,
  ProvisioningStep,
} from "./provider-provisioning";
import type { ProvisioningMaterial } from "./provisioning-material";
import { leaseProviderName } from "./slug";
import type { Env, LeaseRecord } from "./types";
import { leaseCost } from "./usage";

const armOrigin = "https://management.azure.com";
const computeAPI = "2024-07-01";
const networkAPI = "2024-05-01";
const diskAPI = "2024-03-02";
const requestBudget = 8_000;
const pollInterval = 15_000;
const resourceKinds = ["pip", "nic", "vm", "disk", "extension"] as const;
type ResourceKind = (typeof resourceKinds)[number];
type Stage = "rg" | "vnet" | "nsg" | ResourceKind | "endpoint";

interface ARMResource {
  id?: string;
  name?: string;
  location?: string;
  tags?: Record<string, string>;
  properties?: Record<string, unknown>;
}

interface ARMReply {
  status: number;
  resource: ARMResource;
  operationURL?: string;
  errorCode?: string;
  items?: ARMResource[];
}

interface AzureCandidate {
  region: string;
  serverType: string;
  market: "spot" | "on-demand";
  name: string;
  vnet: string;
  nsg: string;
  subnet: string;
  image: ReturnType<typeof azureImageReference>;
  tags: Record<string, string>;
  cost: { hourlyUSD: number; maxUSD: number };
}

export interface AzureProvisioningPlan {
  version: 1;
  subscription: string;
  resourceGroup: string;
  subnet: string;
  ports: string[];
  cidrs: string[];
  timestamp: number;
  extensionCommand: string;
  computerName: string;
  candidates: AzureCandidate[];
}

interface ResourceObservation {
  id: string;
  immutableID: string;
}

interface AzureAttemptJournal {
  stage: Stage;
  action: "inspect" | "dispatch" | "observe";
  identities: Partial<Record<ResourceKind, ResourceObservation>>;
  dispatched: Partial<Record<Stage, true>>;
  operationURL?: string;
  operationDeadline?: number;
  nsgRules?: unknown[];
  rejected?: true;
  allocationRejected?: Stage;
  cleanupResourceIdentity?: string;
  identityConflict?: true;
  operations?: Partial<Record<Stage, { deadline?: number; url?: string; settledAt: number }>>;
}

interface AzureContinuationState {
  version: 1;
  journal: AzureAttemptJournal;
}

class AzureContinuationError extends Error {
  constructor(readonly code: string) {
    super(code);
  }
}

// A single deadline includes auth, dispatch and every response-body read.
export class AzureProvisioningTransport {
  constructor(
    private readonly env: Env,
    private readonly fetcher: typeof fetch = fetch,
  ) {}

  async request(
    scope: string,
    region: string,
    method: string,
    path: string,
    api: string,
    body?: unknown,
    operation = false,
  ): Promise<ARMReply> {
    const requestURL = new URL(`${armOrigin}${path.startsWith("/") ? path : "/"}`);
    requestURL.searchParams.set("api-version", api);
    const url = operation ? validateAzureOperationURL(path, scope, region) : requestURL.toString();
    if (
      !operation &&
      !path.toLowerCase().startsWith(`${scope.toLowerCase()}/`) &&
      path.toLowerCase() !== scope.toLowerCase()
    ) {
      throw new AzureContinuationError("scope_mismatch");
    }
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const expired = new Promise<never>((_, reject) => {
      timer = setTimeout(() => {
        controller.abort();
        reject(new AzureContinuationError("request_unresolved"));
      }, requestBudget);
    });
    const request = async (): Promise<ARMReply> => {
      const authentication = await this.fetcher(
        `https://login.microsoftonline.com/${encodeURIComponent(this.env.AZURE_TENANT_ID ?? "")}/oauth2/v2.0/token`,
        {
          method: "POST",
          redirect: "error",
          signal: controller.signal,
          headers: { "content-type": "application/x-www-form-urlencoded" },
          body: new URLSearchParams({
            grant_type: "client_credentials",
            client_id: this.env.AZURE_CLIENT_ID ?? "",
            client_secret: this.env.AZURE_CLIENT_SECRET ?? "",
            scope: `${armOrigin}/.default`,
          }).toString(),
        },
      );
      const auth = asObject(await boundedJSON(authentication));
      if (!authentication.ok || typeof auth["access_token"] !== "string")
        throw new AzureContinuationError("authentication_unavailable");
      const headers: Record<string, string> = {
        authorization: `Bearer ${auth["access_token"]}`,
        "content-type": "application/json",
      };
      if (method === "PUT" && /\/virtualMachines\/[^/]+$/i.test(path))
        headers["if-none-match"] = "*";
      const response = await this.fetcher(url, {
        method,
        headers,
        redirect: "error",
        signal: controller.signal,
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
      const json = await boundedJSON(response);
      const resource = asObject(json);
      const operationURL =
        response.headers.get("azure-asyncoperation") ?? response.headers.get("location");
      const error = asObject(resource["error"]);
      const errorCode = typeof error["code"] === "string" ? error["code"] : undefined;
      return {
        status: response.status,
        resource,
        ...(Array.isArray(json) ? { items: json.map((item) => asObject(item)) } : {}),
        ...(operationURL
          ? { operationURL: validateAzureOperationURL(operationURL, scope, region) }
          : {}),
        ...(errorCode && /^[a-z0-9_]+$/i.test(errorCode) ? { errorCode } : {}),
      };
    };
    try {
      return await Promise.race([request(), expired]);
    } finally {
      if (timer !== undefined) clearTimeout(timer);
    }
  }
}

export function validateAzureOperationURL(value: string, scope: string, region: string): string {
  const url = new URL(value);
  const subscription = scope.split("/")[2];
  const path = url.pathname.toLowerCase();
  const scoped = path.startsWith(`${scope.toLowerCase()}/providers/`);
  const regional = ["microsoft.compute", "microsoft.network", "microsoft.resources"].some(
    (provider) =>
      path.startsWith(
        `/subscriptions/${subscription?.toLowerCase()}/providers/${provider}/locations/${region.toLowerCase()}/`,
      ),
  );
  if (
    url.origin !== armOrigin ||
    url.username ||
    url.password ||
    url.hash ||
    /%|\\/.test(url.pathname) ||
    (!scoped && !regional) ||
    [...url.searchParams.keys()].some((key) => key !== "api-version")
  ) {
    throw new AzureContinuationError("operation_scope_mismatch");
  }
  return url.toString();
}

async function boundedJSON(response: Response): Promise<unknown> {
  if (!response.body) return {};
  const reader = response.body.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: false });
  let text = "";
  let bytes = 0;
  try {
    for (;;) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- response chunks are sequential and bounded.
      const chunk = await reader.read();
      if (chunk.done) break;
      bytes += chunk.value.byteLength;
      if (bytes > 256 * 1024) throw new AzureContinuationError("response_too_large");
      text += decoder.decode(chunk.value, { stream: true });
    }
    text += decoder.decode();
    return text ? JSON.parse(text) : {};
  } finally {
    void reader.cancel().catch(() => undefined);
  }
}

function asObject(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

export class AzureResumableProvisioning implements ProviderResumableProvisioning {
  readonly version = 1;
  private readonly transport: AzureProvisioningTransport;

  constructor(
    private readonly env: Env,
    fetcher?: typeof fetch,
    private readonly cleanupStorage?: AzureOwnedDeleteClaimStorage,
  ) {
    this.transport = new AzureProvisioningTransport(env, fetcher);
  }

  supports(config: LeaseConfig): boolean {
    return (
      config.provider === "azure" &&
      config.target === "windows" &&
      config.windowsMode === "normal" &&
      config.architecture === "amd64" &&
      config.azureOSDisk === "managed" &&
      !config.azureSnapshot &&
      azureRegionCandidates(config, this.env, this.env.CRABBOX_AZURE_LOCATION || "eastus").length *
        azureProvisioningCandidatesForConfig(config).length *
        (config.capacityMarket === "spot" && config.capacityFallback.startsWith("on-demand")
          ? 2
          : 1) <=
        64
    );
  }

  async validateAdmission(
    storage: CoordinatorStorageView,
    plan: FrozenProvisioningPlan,
    lease: LeaseRecord,
  ): Promise<void> {
    const data = plan.data as AzureProvisioningPlan;
    for (const candidate of data.candidates) {
      const key = `azure-cleanup:${encodeURIComponent(data.subscription)}:${encodeURIComponent(data.resourceGroup)}:${encodeURIComponent(candidate.region)}:${encodeURIComponent(candidate.name)}`;
      // oxlint-disable-next-line eslint/no-await-in-loop -- bounded admission reads share one transaction.
      const [deferred, deletion] = await Promise.all([
        storage.get(key),
        storage.get(azureOwnedDeleteClaimKey(plan.scope, candidate.name, lease.id)),
      ]);
      if (deferred || deletion) throw new AzureContinuationError("existing_cleanup_debt");
    }
  }

  async prepare(
    config: LeaseConfig,
    lease: LeaseRecord,
  ): Promise<{
    plan: FrozenProvisioningPlan;
    material: ProvisioningMaterial;
    step: ProvisioningStep;
  }> {
    if (!this.supports(config)) throw new AzureContinuationError("unsupported_continuation");
    if (!lease.createAttemptGeneration)
      throw new AzureContinuationError("admission_generation_required");
    const client = new AzureClient(this.env);
    const scope = client.providerScope();
    const locations = azureRegionCandidates(config, this.env, client.defaultLocation);
    const sizes = azureProvisioningCandidatesForConfig(config);
    const markets: Array<"spot" | "on-demand"> =
      config.capacityMarket === "spot" && config.capacityFallback.startsWith("on-demand")
        ? ["spot", "on-demand"]
        : [config.capacityMarket];
    const candidates: AzureCandidate[] = [];
    const createdAt = new Date(lease.createdAt);
    for (const region of locations) {
      let vnet = locations.length > 1 ? azureRegionalName(client.vnet, region) : client.vnet;
      let nsg = locations.length > 1 ? azureRegionalName(client.nsg, region) : client.nsg;
      // oxlint-disable-next-line eslint/no-await-in-loop -- freeze candidates in operator preference order.
      const [existingVnet, existingNSG] = await Promise.all([
        this.transport.request(
          scope,
          region,
          "GET",
          `${scope}/providers/Microsoft.Network/virtualNetworks/${vnet}`,
          networkAPI,
        ),
        this.transport.request(
          scope,
          region,
          "GET",
          `${scope}/providers/Microsoft.Network/networkSecurityGroups/${nsg}`,
          networkAPI,
        ),
      ]);
      for (const reply of [existingVnet, existingNSG]) {
        if (
          reply.status !== 404 &&
          (reply.status !== 200 || reply.resource.tags?.["managed_by"] !== "crabbox")
        )
          throw new AzureContinuationError("shared_infrastructure_unowned");
      }
      if (
        [existingVnet, existingNSG].some(
          (reply) => reply.status === 200 && !same(reply.resource.location, region),
        )
      ) {
        vnet = azureRegionalName(client.vnet, region);
        nsg = azureRegionalName(client.nsg, region);
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- freeze each regional image before admission.
      const image = await this.freezeImage(scope, region, client.resolvedImageForConfig(config));
      for (const market of markets) {
        for (const [index, serverType] of sizes.entries()) {
          candidates.push({
            region,
            serverType,
            market,
            vnet,
            nsg,
            subnet: client.subnet,
            image,
            cost: leaseCost(this.env, "azure", serverType, config.ttlSeconds),
            name: leaseProviderName(
              azureAttemptNameSeed(lease.id, region, market, index),
              lease.slug ?? "",
            ),
            tags: {
              ...azureTagsFromLabels(
                leaseProviderLabels(
                  { ...config, serverType, capacityMarket: market },
                  lease.id,
                  lease.slug ?? "",
                  lease.owner,
                  "azure",
                  createdAt,
                  { market },
                ),
              ),
              provisioning_generation: lease.createAttemptGeneration,
            },
          });
        }
      }
    }
    if (!candidates.length || candidates.length > 64)
      throw new AzureContinuationError("candidate_limit");
    const data: AzureProvisioningPlan = {
      version: 1,
      subscription: client.subscription,
      resourceGroup: client.resourceGroup,
      subnet: client.subnet,
      ports: sshPorts(config),
      cidrs: [...client.sshCIDRs],
      timestamp: Math.trunc(createdAt.getTime() / 1000),
      extensionCommand: azureWindowsBootstrapCommand(),
      computerName: `cbx${lease.id.replace(/[^a-z0-9]/gi, "").slice(-12)}`,
      candidates,
    };
    return {
      plan: {
        version: 1,
        provider: "azure",
        scope,
        resources: candidates.map((candidate) => ({
          cloudID: candidate.name,
          region: candidate.region,
          scope,
        })),
        data,
      },
      material: {
        adminPassword: azureRandomAdminPassword(),
        bootstrap: azureWindowsBootstrapPowerShell(config),
      },
      step: {
        phase: "prepared",
        attempt: 0,
        state: { version: 1, journal: newAttempt() } satisfies AzureContinuationState,
        nextWake: Date.now(),
        coordinationKey: scope.toLowerCase(),
      },
    };
  }

  private async freezeImage(
    scope: string,
    region: string,
    value: string,
  ): Promise<ReturnType<typeof azureImageReference>> {
    const image = azureImageReference(value);
    if ("id" in image) {
      const gallery = image.id.match(
        /^(\/subscriptions\/[^/]+\/resourceGroups\/[^/]+)(\/providers\/Microsoft.Compute\/galleries\/[^/]+\/images\/[^/]+)(?:\/versions\/latest)?$/i,
      );
      if (gallery) {
        const parent = `${gallery[1]}${gallery[2]}`;
        const reply = await this.transport.request(
          gallery[1]!,
          region,
          "GET",
          `${parent}/versions`,
          "2024-03-03",
        );
        const versions = asObject(reply.resource)["value"];
        if (
          reply.status !== 200 ||
          !Array.isArray(versions) ||
          versions.length > 64 ||
          asObject(reply.resource)["nextLink"]
        )
          throw new AzureContinuationError("exact_image_version_unavailable");
        const eligible = versions
          .map(asObject)
          .filter((version) => {
            const properties = asObject(version["properties"]);
            return (
              properties["provisioningState"] === "Succeeded" &&
              asObject(properties["publishingProfile"])["excludeFromLatest"] !== true
            );
          })
          .map((version) =>
            String(
              version["name"] ??
                String(version["id"] ?? "")
                  .split("/")
                  .at(-1),
            ),
          )
          .filter((name) => /^\d+\.\d+\.\d+$/.test(name));
        const selected = eligible.toSorted((left, right) => {
          const a = left.split(".").map(BigInt);
          const b = right.split(".").map(BigInt);
          for (let index = 0; index < 3; index += 1) {
            if (a[index]! > b[index]!) return -1;
            if (a[index]! < b[index]!) return 1;
          }
          return 0;
        })[0];
        if (!selected) throw new AzureContinuationError("exact_image_version_unavailable");
        return { id: `${parent}/versions/${selected}` };
      }
      return image;
    }
    if (image.version.toLowerCase() !== "latest") return image;
    const subscriptionScope = scope.split("/resourceGroups/")[0]!;
    const path = `${subscriptionScope}/providers/Microsoft.Compute/locations/${region}/publishers/${image.publisher}/artifacttypes/vmimage/offers/${image.offer}/skus/${image.sku}/versions?$top=1&$orderby=name%20desc`;
    const reply = await this.transport.request(subscriptionScope, region, "GET", path, computeAPI);
    const version = reply.items?.[0]?.name;
    if (reply.status !== 200 || !version || !/^\d+(?:\.\d+)+$/.test(version))
      throw new AzureContinuationError("exact_image_version_unavailable");
    return { ...image, version };
  }

  async advance(
    input: Parameters<ProviderResumableProvisioning["advance"]>[0],
  ): Promise<ProvisioningStep> {
    const plan = input.plan.data as AzureProvisioningPlan;
    const state = structuredClone(input.step.state) as AzureContinuationState;
    if (
      plan.version !== 1 ||
      state.version !== 1 ||
      input.plan.scope !==
        `/subscriptions/${plan.subscription}/resourceGroups/${plan.resourceGroup}`
    )
      throw new AzureContinuationError("plan_invalid");
    const candidate = plan.candidates[input.step.attempt];
    const journal = state.journal;
    if (!candidate || !journal) throw new AzureContinuationError("attempt_invalid");
    const output = (phase: ProvisioningStep["phase"], delay = 1): ProvisioningStep => ({
      phase,
      attempt: input.step.attempt,
      state,
      nextWake: Date.now() + delay,
      ...(["rg", "vnet", "nsg"].includes(journal.stage)
        ? { coordinationKey: input.plan.scope.toLowerCase() }
        : {}),
    });
    if (journal.identityConflict)
      return { ...output("blocked", pollInterval), blockedReason: "identity_resolution_required" };
    if (
      input.canceled ||
      journal.rejected ||
      input.step.phase === "cleanup" ||
      input.step.phase === "settling"
    ) {
      return this.cleanup(input, plan, candidate, state, journal);
    }
    if (!input.material) throw new AzureContinuationError("forward_material_required");
    const scope = input.plan.scope;
    const path = resourcePath(scope, plan, candidate, journal.stage);
    if (journal.stage === "endpoint") {
      const inventory = await this.inventory(scope, plan, candidate);
      inspectAzureProvisioningResources(
        input.lease,
        scope,
        candidate,
        journal.identities,
        inventory,
      );
      const vm = inventory.vm;
      const pip = inventory.pip;
      if (!vm || !pip?.properties?.ipAddress) return output("provisioning", pollInterval);
      const result = output("ready-to-publish");
      result.publication = {
        server: {
          provider: "azure",
          id: 0,
          cloudID: candidate.name,
          name: candidate.name,
          status: "running",
          region: candidate.region,
          serverType: candidate.serverType,
          host: pip.properties.ipAddress,
          labels: azureLabelsFromTags(vm.tags ?? {}),
          resourceIdentity: inventoryIdentity(inventory),
        },
        serverType: candidate.serverType,
        market: candidate.market,
        cost: candidate.cost,
      };
      return result;
    }
    const api = apiFor(journal.stage);
    const reply = await this.transport.request(scope, candidate.region, "GET", path, api);
    if (reply.status !== 200 && reply.status !== 404) return output("provisioning", pollInterval);
    const existing = reply.status === 200 ? reply.resource : undefined;
    const kind = resourceKinds.find((value) => value === journal.stage);
    if (existing) {
      if (!kind) {
        if (
          existing.tags?.["managed_by"] !== "crabbox" ||
          (journal.stage !== "rg" && !same(existing.location, candidate.region))
        )
          throw new AzureContinuationError("shared_infrastructure_unowned");
      } else {
        verifyResource(existing, path, kind, candidate, input.lease, journal.identities[kind]);
        if (kind !== "extension") journal.identities[kind] = observation(existing, kind);
      }
    } else if (kind && journal.identities[kind]) {
      return { ...output("blocked", pollInterval), blockedReason: "observed_resource_missing" };
    }
    if (journal.action === "inspect") {
      if (existing && (!kind || kind === "disk")) {
        if (journal.stage === "nsg") {
          journal.nsgRules = mergedNSGRules(existing, plan);
          if (
            !azureCrabboxSSHRulesMatch(
              Array.isArray(existing.properties?.["securityRules"])
                ? existing.properties["securityRules"]
                : [],
              (
                journal.nsgRules as Array<{ name?: string; properties?: Record<string, unknown> }>
              ).filter((rule) => rule.name?.startsWith("crabbox-ssh-")),
            )
          ) {
            journal.action = "dispatch";
            journal.operationDeadline = Math.min(input.deadline, Date.now() + 60_000);
            return output("provisioning");
          }
        }
        if (
          kind === "disk" &&
          !providerLabelsOwnedByLease(
            azureLabelsFromTags(existing.tags ?? {}),
            input.lease,
            "azure",
          )
        ) {
          const inventory = await this.inventory(scope, plan, candidate);
          inspectAzureProvisioningResources(
            input.lease,
            scope,
            candidate,
            journal.identities,
            inventory,
          );
          journal.action = "dispatch";
          journal.operationDeadline = Math.min(input.deadline, Date.now() + 60_000);
          return output("provisioning");
        }
        advanceStage(journal);
        return output("provisioning");
      }
      if (existing) throw new AzureContinuationError("resource_exists_before_dispatch");
      if (kind === "disk") return output("provisioning", pollInterval);
      if (journal.stage === "nsg") journal.nsgRules = mergedNSGRules({}, plan);
      journal.action = "dispatch";
      journal.operationDeadline = Math.min(
        input.deadline,
        Date.now() + (kind === "vm" || kind === "extension" ? 20 * 60_000 : 60_000),
      );
      return output("provisioning");
    }
    if (
      journal.action === "dispatch" &&
      (!input.recovering || (kind === "vm" && !existing && !journal.identities.vm))
    ) {
      if (kind && kind !== "disk" && existing)
        throw new AzureContinuationError("resource_exists_before_dispatch");
      if (kind === "nic" || kind === "vm" || kind === "disk" || kind === "extension") {
        const inventory = await this.inventory(scope, plan, candidate);
        inspectAzureProvisioningResources(
          input.lease,
          scope,
          candidate,
          journal.identities,
          inventory,
        );
      }
      journal.dispatched[journal.stage] = true;
      journal.action = "observe";
      const body = resourceBody(plan, candidate, journal, input.material, scope);
      try {
        const result = await this.transport.request(
          scope,
          candidate.region,
          kind === "disk" ? "PATCH" : "PUT",
          path,
          api,
          body,
        );
        if (kind && kind !== "extension" && result.status < 300 && result.resource.id) {
          try {
            if (!same(result.resource.id, path))
              throw new AzureContinuationError("dispatch_identity_changed");
            const identityField =
              kind === "vm" ? "vmId" : kind === "disk" ? "uniqueId" : "resourceGuid";
            if (result.resource.properties?.[identityField]) {
              const observed = observation(result.resource, kind);
              const expected = journal.identities[kind];
              if (
                expected &&
                (!same(expected.id, observed.id) || expected.immutableID !== observed.immutableID)
              )
                throw new AzureContinuationError("dispatch_identity_changed");
              journal.identities[kind] = observed;
            }
          } catch {
            journal.identityConflict = true;
            return {
              ...output("blocked", pollInterval),
              blockedReason: "identity_resolution_required",
            };
          }
        }
        if (result.operationURL) journal.operationURL = result.operationURL;
        if (
          [400, 403, 409].includes(result.status) &&
          [
            "SkuNotAvailable",
            "AllocationFailed",
            "OverconstrainedAllocationRequest",
            "ZonalAllocationFailed",
          ].includes(result.errorCode ?? "")
        ) {
          journal.rejected = true;
          journal.allocationRejected = journal.stage;
        }
        if (result.status >= 400 && !journal.rejected && result.status !== 412)
          return {
            ...output("blocked", pollInterval),
            blockedReason: "dispatch_rejected_or_unresolved",
          };
      } catch {
        // The persisted dispatch intent remains uncertainty, even when GET temporarily returns 404.
      }
      return output(journal.rejected ? "settling" : "provisioning", pollInterval);
    }
    if (input.recovering && journal.action === "dispatch") {
      journal.dispatched[journal.stage] = true;
      journal.action = "observe";
    }
    if (!existing)
      return { ...output("blocked", pollInterval), blockedReason: "dispatch_outcome_unresolved" };
    const provisioningState = String(
      existing.properties?.["provisioningState"] ?? "",
    ).toLowerCase();
    if (provisioningState === "succeeded" || (journal.stage === "rg" && reply.status === 200)) {
      if (
        journal.stage === "disk" &&
        !providerLabelsOwnedByLease(azureLabelsFromTags(existing.tags ?? {}), input.lease, "azure")
      )
        return output("provisioning", pollInterval);
      if (
        journal.stage === "extension" &&
        asObject(existing.properties?.["settings"])["timestamp"] !== plan.timestamp
      )
        throw new AzureContinuationError("extension_settings_changed");
      advanceStage(journal);
      return output("provisioning");
    }
    if (provisioningState === "failed" || provisioningState === "canceled") {
      journal.rejected = true;
      return output("settling");
    }
    if (journal.operationURL) {
      const poll = await this.transport.request(
        scope,
        candidate.region,
        "GET",
        journal.operationURL,
        api,
        undefined,
        true,
      );
      const status = String(asObject(poll.resource)["status"] ?? "").toLowerCase();
      if (status === "failed" || status === "canceled") {
        journal.rejected = true;
        return output("settling");
      }
    }
    if (journal.operationDeadline && Date.now() >= journal.operationDeadline)
      return {
        ...output("blocked", pollInterval),
        blockedReason: "original_operation_deadline_exceeded",
      };
    return output("provisioning", pollInterval);
  }

  private async inventory(
    scope: string,
    plan: AzureProvisioningPlan,
    candidate: AzureCandidate,
  ): Promise<AzureInventory> {
    const values = await Promise.all(
      (["vm", "nic", "pip", "disk"] as const).map(async (kind) => {
        const reply = await this.transport.request(
          scope,
          candidate.region,
          "GET",
          resourcePath(scope, plan, candidate, kind),
          apiFor(kind),
        );
        if (reply.status !== 200 && reply.status !== 404)
          throw new AzureContinuationError("inventory_unavailable");
        return [kind, reply.status === 200 ? reply.resource : undefined] as const;
      }),
    );
    return Object.fromEntries(values) as AzureInventory;
  }

  private async cleanup(
    input: Parameters<ProviderResumableProvisioning["advance"]>[0],
    plan: AzureProvisioningPlan,
    candidate: AzureCandidate,
    state: AzureContinuationState,
    journal: AzureAttemptJournal,
  ): Promise<ProvisioningStep> {
    const output = (phase: ProvisioningStep["phase"], delay = pollInterval): ProvisioningStep => ({
      phase,
      attempt: input.step.attempt,
      state,
      nextWake: Date.now() + delay,
      ...(phase !== "terminal" &&
      phase !== "retained" &&
      ["rg", "vnet", "nsg"].includes(journal.stage)
        ? { coordinationKey: input.plan.scope.toLowerCase() }
        : {}),
    });
    // Do not turn a missing resource or an expired LRO into proof that allocation stopped.
    if (journal.action === "dispatch" || journal.action === "observe") {
      if (journal.action === "dispatch" && !input.recovering) {
        // Cancellation won before this durable dispatch intent was ever claimed.
        journal.action = "inspect";
      } else {
        const reply = await this.transport.request(
          input.plan.scope,
          candidate.region,
          "GET",
          resourcePath(input.plan.scope, plan, candidate, journal.stage),
          apiFor(journal.stage),
        );
        if (reply.status === 404 && journal.allocationRejected === journal.stage) {
          delete journal.dispatched[journal.stage];
          journal.action = "inspect";
        } else if (reply.status === 200) {
          const kind = resourceKinds.find((value) => value === journal.stage);
          if (kind) {
            verifyResource(
              reply.resource,
              resourcePath(input.plan.scope, plan, candidate, kind),
              kind,
              candidate,
              input.lease,
              journal.identities[kind],
            );
            if (kind !== "extension") journal.identities[kind] = observation(reply.resource, kind);
          }
          const status = String(
            reply.resource.properties?.["provisioningState"] ?? "",
          ).toLowerCase();
          if (!["succeeded", "failed", "canceled"].includes(status) && journal.stage !== "rg")
            return {
              ...output("settling"),
              blockedReason: "canceled_dispatch_requires_settlement",
            };
          journal.action = "inspect";
        } else
          return { ...output("blocked"), blockedReason: "canceled_dispatch_requires_settlement" };
      }
      return output("cleanup", 1);
    }
    if (input.retain) return output("retained", 1);
    if (!journal.cleanupResourceIdentity) {
      const inventory = await this.inventory(input.plan.scope, plan, candidate);
      inspectAzureProvisioningResources(
        input.lease,
        input.plan.scope,
        candidate,
        journal.identities,
        inventory,
      );
      for (const kind of ["vm", "nic", "pip", "disk"] as const) {
        if (journal.dispatched[kind] && !journal.identities[kind])
          return { ...output("blocked"), blockedReason: "unidentified_dispatch" };
        const resource = inventory[kind];
        if (resource) journal.identities[kind] = observation(resource as ARMResource, kind);
      }
      journal.cleanupResourceIdentity = inventoryIdentity(inventory);
      return output("cleanup", 1);
    }
    const client = new AzureClient(this.env, {
      subscription: plan.subscription,
      resourceGroup: plan.resourceGroup,
      location: candidate.region,
      ...(this.cleanupStorage ? { ownedDeleteClaimStorage: this.cleanupStorage } : {}),
    });
    const complete = await client.advanceOwnedServerDeletion(
      { ...input.lease, cloudID: candidate.name, providerScope: input.plan.scope },
      {
        request: (method, path, api, body, operation) =>
          this.transport.request(
            input.plan.scope,
            candidate.region,
            method,
            operation ? path : `/subscriptions/${plan.subscription}${path}`,
            api,
            body,
            operation,
          ),
      },
      { resourceIdentity: journal.cleanupResourceIdentity },
    );
    if (!complete) return output("cleanup");
    if (input.canceled || input.step.attempt + 1 >= plan.candidates.length)
      return output("terminal", 1);
    return {
      phase: "prepared",
      attempt: input.step.attempt + 1,
      state: { version: 1, journal: newAttempt() } satisfies AzureContinuationState,
      nextWake: Date.now() + 1,
      coordinationKey: input.plan.scope.toLowerCase(),
    };
  }
}

function newAttempt(): AzureAttemptJournal {
  return { stage: "rg", action: "inspect", identities: {}, dispatched: {} };
}

function advanceStage(journal: AzureAttemptJournal): void {
  const stages: Stage[] = [
    "rg",
    "vnet",
    "nsg",
    "pip",
    "nic",
    "vm",
    "disk",
    "extension",
    "endpoint",
  ];
  journal.operations ??= {};
  journal.operations[journal.stage] = {
    settledAt: Date.now(),
    ...(journal.operationDeadline === undefined ? {} : { deadline: journal.operationDeadline }),
    ...(journal.operationURL ? { url: journal.operationURL } : {}),
  };
  journal.stage = stages[stages.indexOf(journal.stage) + 1] ?? "endpoint";
  journal.action = "inspect";
  delete journal.operationURL;
  delete journal.operationDeadline;
}

function apiFor(stage: Stage): string {
  if (stage === "rg") return "2021-04-01";
  if (stage === "vm" || stage === "extension") return computeAPI;
  return stage === "disk" ? diskAPI : networkAPI;
}

function resourcePath(
  scope: string,
  plan: AzureProvisioningPlan,
  candidate: AzureCandidate,
  stage: Stage,
): string {
  switch (stage) {
    case "rg":
      return scope;
    case "vnet":
      return `${scope}/providers/Microsoft.Network/virtualNetworks/${candidate.vnet}`;
    case "nsg":
      return `${scope}/providers/Microsoft.Network/networkSecurityGroups/${candidate.nsg}`;
    case "pip":
    case "endpoint":
      return `${scope}/providers/Microsoft.Network/publicIPAddresses/${candidate.name}-pip`;
    case "nic":
      return `${scope}/providers/Microsoft.Network/networkInterfaces/${candidate.name}-nic`;
    case "vm":
      return `${scope}/providers/Microsoft.Compute/virtualMachines/${candidate.name}`;
    case "disk":
      return `${scope}/providers/Microsoft.Compute/disks/${candidate.name}-osdisk`;
    case "extension":
      return `${scope}/providers/Microsoft.Compute/virtualMachines/${candidate.name}/extensions/crabbox-bootstrap`;
  }
}

function resourceBody(
  plan: AzureProvisioningPlan,
  candidate: AzureCandidate,
  journal: AzureAttemptJournal,
  material: ProvisioningMaterial,
  scope: string,
): unknown {
  const { region: location, tags, name } = candidate;
  const sharedTags = { crabbox: "true", managed_by: "crabbox" };
  switch (journal.stage) {
    case "rg":
      return { location, tags: sharedTags };
    case "vnet":
      return {
        location,
        tags: sharedTags,
        properties: {
          addressSpace: { addressPrefixes: ["10.42.0.0/16"] },
          subnets: [{ name: plan.subnet, properties: { addressPrefix: "10.42.0.0/24" } }],
        },
      };
    case "nsg":
      return { location, tags: sharedTags, properties: { securityRules: journal.nsgRules } };
    case "pip":
      return {
        location,
        tags,
        sku: { name: "Standard" },
        properties: { publicIPAllocationMethod: "Static" },
      };
    case "nic":
      return {
        location,
        tags,
        properties: {
          ipConfigurations: [
            {
              name: "ipconfig",
              properties: {
                privateIPAllocationMethod: "Dynamic",
                subnet: {
                  id: `${resourcePath(scope, plan, candidate, "vnet")}/subnets/${plan.subnet}`,
                },
                publicIPAddress: { id: resourcePath(scope, plan, candidate, "pip") },
              },
            },
          ],
          networkSecurityGroup: { id: resourcePath(scope, plan, candidate, "nsg") },
        },
      };
    case "vm":
      return {
        location,
        tags,
        properties: {
          hardwareProfile: { vmSize: candidate.serverType },
          storageProfile: {
            imageReference: candidate.image,
            osDisk: {
              name: `${name}-osdisk`,
              createOption: "FromImage",
              caching: "ReadWrite",
              managedDisk: { storageAccountType: "StandardSSD_LRS" },
            },
          },
          networkProfile: {
            networkInterfaces: [{ id: resourcePath(scope, plan, candidate, "nic") }],
          },
          osProfile: {
            computerName: plan.computerName,
            adminUsername: "crabadmin",
            adminPassword: material.adminPassword,
            allowExtensionOperations: true,
            customData: btoa(material.bootstrap),
            windowsConfiguration: { provisionVMAgent: true, enableAutomaticUpdates: false },
          },
          ...(candidate.market === "spot"
            ? { priority: "Spot", evictionPolicy: "Delete", billingProfile: { maxPrice: -1 } }
            : {}),
        },
      };
    case "disk":
      return { tags };
    case "extension":
      return {
        location,
        tags,
        properties: {
          publisher: "Microsoft.Compute",
          type: "CustomScriptExtension",
          typeHandlerVersion: "1.10",
          autoUpgradeMinorVersion: true,
          settings: { timestamp: plan.timestamp },
          protectedSettings: { commandToExecute: plan.extensionCommand },
        },
      };
    case "endpoint":
      throw new AzureContinuationError("invalid_dispatch_stage");
  }
}

function mergedNSGRules(resource: ARMResource, plan: AzureProvisioningPlan): unknown[] {
  const existing = resource.properties?.["securityRules"];
  const preserved = preserveNonCrabboxRules(Array.isArray(existing) ? existing : []);
  const used = new Set(preserved.map((rule) => Number(rule.properties?.["priority"])));
  const rules: unknown[] = [...preserved];
  let priority = 1000;
  for (const port of plan.ports)
    for (const [index, cidr] of plan.cidrs.entries()) {
      while (used.has(priority)) priority += 1;
      if (priority > 4096) throw new AzureContinuationError("nsg_priorities_exhausted");
      used.add(priority);
      rules.push({
        name: `crabbox-ssh-${port}-${index}`,
        properties: {
          priority: priority++,
          direction: "Inbound",
          access: "Allow",
          protocol: "Tcp",
          sourceAddressPrefix: cidr,
          sourcePortRange: "*",
          destinationAddressPrefix: "*",
          destinationPortRange: port,
        },
      });
    }
  return rules;
}

function same(left: string | undefined, right: string | undefined): boolean {
  return Boolean(left && right && left.toLowerCase() === right.toLowerCase());
}

function observation(resource: ARMResource, kind: ResourceKind): ResourceObservation {
  const identity =
    resource.properties?.[kind === "vm" ? "vmId" : kind === "disk" ? "uniqueId" : "resourceGuid"];
  if (!resource.id || typeof identity !== "string" || !identity.trim())
    throw new AzureContinuationError("immutable_identity_unavailable");
  return { id: resource.id, immutableID: identity };
}

function verifyResource(
  resource: ARMResource,
  path: string,
  kind: ResourceKind,
  candidate: AzureCandidate,
  lease: LeaseRecord,
  expected?: ResourceObservation,
): void {
  if (!same(resource.id, path) || !same(resource.location, candidate.region))
    throw new AzureContinuationError("resource_scope_changed");
  if (
    kind !== "disk" &&
    !providerLabelsOwnedByLease(azureLabelsFromTags(resource.tags ?? {}), lease, "azure")
  )
    throw new AzureContinuationError("resource_ownership_changed");
  if (
    kind !== "disk" &&
    Object.entries(candidate.tags).some(([key, value]) => resource.tags?.[key] !== value)
  )
    throw new AzureContinuationError("resource_intent_changed");
  if (
    kind === "vm" &&
    asObject(resource.properties?.["hardwareProfile"])["vmSize"] !== candidate.serverType
  )
    throw new AzureContinuationError("vm_size_changed");
  if (kind === "vm") {
    const image = asObject(asObject(resource.properties?.["storageProfile"])["imageReference"]);
    if (
      Object.entries(candidate.image).some(([key, value]) => !same(String(image[key] ?? ""), value))
    )
      throw new AzureContinuationError("vm_image_changed");
  }
  if (kind === "nic") {
    const scope = path.split("/providers/")[0]!;
    const nsg = asObject(resource.properties?.["networkSecurityGroup"])["id"];
    const configurations = resource.properties?.["ipConfigurations"];
    if (
      !same(
        String(nsg ?? ""),
        `${scope}/providers/Microsoft.Network/networkSecurityGroups/${candidate.nsg}`,
      ) ||
      !Array.isArray(configurations) ||
      configurations.length !== 1
    )
      throw new AzureContinuationError("nic_configuration_changed");
    const subnet = asObject(asObject(asObject(configurations[0])["properties"])["subnet"])["id"];
    if (
      !same(
        String(subnet ?? ""),
        `${scope}/providers/Microsoft.Network/virtualNetworks/${candidate.vnet}/subnets/${candidate.subnet}`,
      )
    )
      throw new AzureContinuationError("nic_subnet_changed");
  }
  if (
    kind === "disk" &&
    Object.keys(resource.tags ?? {}).some((key) =>
      ["crabbox", "lease", "owner", "provider", "slug", "created_by"].includes(key),
    ) &&
    !providerLabelsOwnedByLease(azureLabelsFromTags(resource.tags ?? {}), lease, "azure")
  )
    throw new AzureContinuationError("disk_ownership_changed");
  if (kind !== "extension") {
    const identity = observation(resource, kind);
    if (
      expected &&
      (!same(expected.id, identity.id) || expected.immutableID !== identity.immutableID)
    )
      throw new AzureContinuationError("immutable_identity_changed");
  }
}

interface AzureInventory {
  vm?: AzureVM;
  nic?: AzureNIC;
  pip?: AzurePublicIP;
  disk?: AzureDisk;
}

// Pure validation: unlike ownedDeleteResources, this cannot bind/tag an OS disk.
export function inspectAzureProvisioningResources(
  lease: LeaseRecord,
  scope: string,
  candidate: AzureCandidate,
  expected: Partial<Record<ResourceKind, ResourceObservation>>,
  resources: AzureInventory,
): void {
  const { vm, nic, pip, disk } = resources;
  const vmID = `${scope}/providers/Microsoft.Compute/virtualMachines/${candidate.name}`;
  const nicID = `${scope}/providers/Microsoft.Network/networkInterfaces/${candidate.name}-nic`;
  const pipID = `${scope}/providers/Microsoft.Network/publicIPAddresses/${candidate.name}-pip`;
  const diskID = `${scope}/providers/Microsoft.Compute/disks/${candidate.name}-osdisk`;
  for (const [kind, resource, path] of [
    ["vm", vm, vmID],
    ["nic", nic, nicID],
    ["pip", pip, pipID],
    ["disk", disk, diskID],
  ] as const) {
    if (!resource && expected[kind]) throw new AzureContinuationError("known_resource_missing");
    if (resource)
      verifyResource(resource as ARMResource, path, kind, candidate, lease, expected[kind]);
  }
  if (
    vm &&
    (!nic ||
      !azureVMReferencesOnlyNIC(vm, nicID) ||
      !vm.properties?.networkProfile?.networkInterfaces?.some((reference) =>
        same(reference.id, nicID),
      ) ||
      azureVMHasDataDisks(vm) ||
      azureVMHasCascadeDelete(vm) ||
      !same(vm.properties?.storageProfile?.osDisk?.managedDisk?.id, diskID))
  )
    throw new AzureContinuationError("vm_topology_changed");
  if (
    nic &&
    (azureNICHasCascadeDelete(nic) ||
      azureNICHasDisqualifyingServiceAssociation(nic) ||
      !azureNICReferencesOnlyPIP(nic, pipID, Boolean(pip)) ||
      (pip &&
        !nic.properties?.ipConfigurations?.some((configuration) =>
          same(configuration.properties?.publicIPAddress?.id, pipID),
        )) ||
      (nic.properties?.virtualMachine && (!same(nic.properties.virtualMachine.id, vmID) || !vm)))
  )
    throw new AzureContinuationError("nic_topology_changed");
  if (pip && !azurePublicIPAttachmentMatchesNIC(pip, nic, pipID, nicID, false))
    throw new AzureContinuationError("pip_topology_changed");
  if (disk && !azureDiskAttachmentsMatchVM(disk, vmID, Boolean(vm)))
    throw new AzureContinuationError("disk_topology_changed");
}

function inventoryIdentity(inventory: AzureInventory): string {
  return azureReconciliationResourceIdentity([
    ...(inventory.vm ? [{ kind: "virtualMachines" as const, resource: inventory.vm }] : []),
    ...(inventory.nic ? [{ kind: "networkInterfaces" as const, resource: inventory.nic }] : []),
    ...(inventory.pip ? [{ kind: "publicIPAddresses" as const, resource: inventory.pip }] : []),
    ...(inventory.disk ? [{ kind: "disks" as const, resource: inventory.disk }] : []),
  ]);
}
