import { azureWindowsBootstrapPowerShell, cloudInit } from "./bootstrap";
import {
  azureSupportsEphemeralFullCaching,
  azureSupportsEphemeralOS,
  azureVMSizeCandidatesForTargetClass,
  concreteStoredServerType,
  isCanonicalProviderClass,
  uniqueProviderMachineCandidates,
  sshPorts,
  validatedCIDRs,
  type LeaseConfig,
} from "./config";
import { leaseProviderLabels, providerLabelsOwnedByLease } from "./provider-labels";
import {
  ProviderProvisioningCleanupError,
  providerProvisioningCleanupClaim,
} from "./provider-provisioning";
import { leaseProviderName } from "./slug";
import type {
  Env,
  LeaseRecord,
  ProviderImage,
  ProviderMachine,
  ProvisioningAttempt,
} from "./types";

export { azureSupportsEphemeralFullCaching, azureSupportsEphemeralOS } from "./config";

const ADDRESS_SPACE = "10.42.0.0/16";
const SUBNET_CIDR = "10.42.0.0/24";
const API_VERSIONS = {
  resources: "2021-04-01",
  network: "2024-05-01",
  compute: "2024-07-01",
  disks: "2024-03-02",
};
const COMPUTE_FULL_CACHING_PREVIEW_API_VERSION = "2025-04-01";
const DELETE_RETRY_ATTEMPTS = 13;
const DELETE_RETRY_DELAY_MS = 15_000;
const MIN_LRO_POLL_INTERVAL_MS = 15_000;
const DEFAULT_AZURE_NETWORK_LRO_TIMEOUT_MS = 60_000;
const DEFAULT_AZURE_VM_CREATE_TIMEOUT_MS = 180_000;
const DEFAULT_AZURE_SPOT_FALLBACK_MS = 120_000;
const DEFAULT_AZURE_LINUX_IMAGE = "Canonical:ubuntu-26_04-lts:server:latest";
const DEFAULT_AZURE_LINUX_ARM64_IMAGE = "Canonical:ubuntu-26_04-lts:server-arm64:latest";
const AZURE_NOBLE_LINUX_IMAGE = "Canonical:ubuntu-24_04-lts:server:latest";
const AZURE_NOBLE_LINUX_ARM64_IMAGE = "Canonical:ubuntu-24_04-lts:server-arm64:latest";
const LEGACY_AZURE_JAMMY_IMAGE = "Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest";
const LEGACY_AZURE_NOBLE_GEN2_IMAGE =
  "Canonical:0001-com-ubuntu-server-noble:24_04-lts-gen2:latest";
const DEFAULT_AZURE_WINDOWS_IMAGE =
  "MicrosoftWindowsServer:windowsserver2022:2022-datacenter-smalldisk-g2:latest";
const AZURE_TRUFFLEHOG_VERSION = "3.95.9";
const AZURE_TRUFFLEHOG_AMD64_SHA256 =
  "f6d1106b85107d79527ed7a5b98b592beadd8b770dc3c9e8c1ad99e1b2cf127e";
const AZURE_TRUFFLEHOG_ARM64_SHA256 =
  "9d9c2ec4ea36a089a9c5aaafe1969d176013ddf9f44d68e8cd75291aed8c83ed";

export function azureLinuxCloudInit(config: LeaseConfig): string {
  return cloudInit(config, azureLinuxTruffleHogBootstrap());
}

function azureLinuxTruffleHogBootstrap(): string {
  const versionPattern = AZURE_TRUFFLEHOG_VERSION.replaceAll(".", "[.]");
  return `    if ! command -v trufflehog >/dev/null 2>&1 || ! trufflehog --no-update --version | grep -Eq '(^|[[:space:]])${versionPattern}($|[[:space:]])'; then
      trufflehog_version=${AZURE_TRUFFLEHOG_VERSION}
      trufflehog_arch="$(dpkg --print-architecture)"
      case "$trufflehog_arch" in
        amd64) trufflehog_sha256=${AZURE_TRUFFLEHOG_AMD64_SHA256} ;;
        arm64) trufflehog_sha256=${AZURE_TRUFFLEHOG_ARM64_SHA256} ;;
        *) echo "unsupported TruffleHog architecture: $trufflehog_arch" >&2; exit 1 ;;
      esac
      trufflehog_archive="trufflehog_\${trufflehog_version}_linux_\${trufflehog_arch}.tar.gz"
      trufflehog_url="https://github.com/trufflesecurity/trufflehog/releases/download/v\${trufflehog_version}/\${trufflehog_archive}"
      trufflehog_tmp="$(mktemp -d)"
      retry curl -fsSL --output "$trufflehog_tmp/$trufflehog_archive" "$trufflehog_url"
      (
        cd "$trufflehog_tmp"
        printf '%s  %s\\n' "$trufflehog_sha256" "$trufflehog_archive" | sha256sum -c -
      )
      tar --no-same-owner -xzf "$trufflehog_tmp/$trufflehog_archive" -C "$trufflehog_tmp" trufflehog
      trufflehog_candidate="$(mktemp /usr/local/bin/trufflehog.tmp.XXXXXX)"
      install -m 0755 "$trufflehog_tmp/trufflehog" "$trufflehog_candidate"
      if ! "$trufflehog_candidate" --no-update --version | grep -Eq '(^|[[:space:]])${versionPattern}($|[[:space:]])'; then
        rm -f "$trufflehog_candidate"
        rm -rf "$trufflehog_tmp"
        exit 1
      fi
      mv -f "$trufflehog_candidate" /usr/local/bin/trufflehog
      rm -rf "$trufflehog_tmp"
    fi
    trufflehog --no-update --version`;
}

function azureOwnedDeleteClaimKey(providerScope: string, cloudID: string, leaseID: string): string {
  return ["provider:azure:delete-claim", providerScope, cloudID, leaseID]
    .map((part) => encodeURIComponent(part))
    .join(":");
}

interface TokenCache {
  token: string;
  expiresAt: number;
}

interface AzureVM {
  id?: string;
  name?: string;
  location?: string;
  tags?: Record<string, string>;
  properties?: {
    vmId?: string;
    provisioningState?: string;
    hardwareProfile?: { vmSize?: string };
    storageProfile?: {
      osDisk?: {
        managedDisk?: { id?: string };
        osType?: string;
        deleteOption?: string;
        diffDiskSettings?: { option?: string; placement?: string; enableFullCaching?: boolean };
      };
      dataDisks?: {
        name?: string;
        lun?: number;
        deleteOption?: string;
        managedDisk?: { id?: string };
      }[];
    };
    networkProfile?: {
      networkInterfaces?: {
        id?: string;
        deleteOption?: string;
        properties?: { deleteOption?: string };
      }[];
    };
  };
}

interface AzureManagedImage {
  id?: string;
  name?: string;
  location?: string;
  properties?: { provisioningState?: string };
}

interface AzureSnapshot {
  id?: string;
  name?: string;
  location?: string;
  properties?: { provisioningState?: string; completionPercent?: number };
}

interface AzurePublicIP {
  id?: string;
  name?: string;
  location?: string;
  tags?: Record<string, string>;
  properties?: {
    ipAddress?: string;
    resourceGuid?: string;
    ipConfiguration?: { id?: string } | null;
    natGateway?: { id?: string } | null;
    linkedPublicIPAddress?: { id?: string } | null;
    servicePublicIPAddress?: { id?: string } | null;
  };
}

interface AzureResourceReference {
  id?: string;
  deleteOption?: string;
}

interface AzureNICIPConfiguration {
  id?: string;
  properties?: {
    publicIPAddress?: AzureResourceReference;
    loadBalancerBackendAddressPools?: AzureResourceReference[];
    applicationGatewayBackendAddressPools?: AzureResourceReference[];
    applicationSecurityGroups?: AzureResourceReference[];
    loadBalancerInboundNatRules?: AzureResourceReference[];
    privateLinkConnectionProperties?: Record<string, unknown> | null;
    virtualNetworkTaps?: AzureResourceReference[];
    gatewayLoadBalancer?: AzureResourceReference | null;
  };
}

interface AzureNIC {
  id?: string;
  name?: string;
  location?: string;
  tags?: Record<string, string>;
  properties?: {
    resourceGuid?: string;
    virtualMachine?: AzureResourceReference;
    privateEndpoint?: AzureResourceReference | null;
    privateLinkService?: AzureResourceReference | null;
    hostedWorkloads?: string[];
    tapConfigurations?: AzureResourceReference[];
    ipConfigurations?: AzureNICIPConfiguration[];
  };
}

interface AzureDisk {
  id?: string;
  name?: string;
  location?: string;
  managedBy?: string;
  managedByExtended?: string[];
  tags?: Record<string, string>;
  properties?: { uniqueId?: string };
}

interface AzureLeaseResourceInventory {
  vms: AzureVM[];
  nics: AzureNIC[];
  pips: AzurePublicIP[];
  disks: AzureDisk[];
}

interface AzureReconciliationResourceSet {
  cloudID: string;
  canonicalVMID: string;
  canonicalNICID: string;
  canonicalPIPID: string;
  vm?: AzureVM;
  nic?: AzureNIC;
  pip?: AzurePublicIP;
  disk?: AzureDisk;
}

interface AzureReconciliationCanonicalIDs {
  vmID: string;
  nicID: string;
  pipID: string;
}

type AzureLeaseResource =
  | { kind: "virtualMachines"; resource: AzureVM }
  | { kind: "networkInterfaces"; resource: AzureNIC }
  | { kind: "publicIPAddresses"; resource: AzurePublicIP }
  | { kind: "disks"; resource: AzureDisk };

interface AzureOwnedDeleteResources {
  vm: boolean;
  nic: boolean;
  pip: boolean;
  disk: boolean;
}

interface AzureOwnedDeleteInspection extends AzureOwnedDeleteResources {
  diskResource?: AzureDisk;
  stableResourceIdentity: string;
  ephemeralOSDisk: boolean;
}

interface AzureOwnedDeleteClaim {
  version: 1 | 2;
  provider: "azure";
  leaseID: string;
  slug: string;
  owner: string;
  cloudID: string;
  providerScope: string;
  preparing?: true;
  canonicalVMID?: string;
  canonicalNICID?: string;
  resourceIdentity?: string;
  stableResourceIdentity?: string;
  partialStableResourceIdentity?: string;
  deletedStableResourceIdentity?: string;
  disk?: {
    resourceID: string;
    uniqueID: string;
  };
}

export interface AzureOwnedDeleteReleaseContext {
  resourceIdentity: string;
}

interface AzureResourceList<T> {
  value?: T[];
  nextLink?: string;
}

export interface AzureOwnedDeleteClaimStorage {
  get<T>(key: string): Promise<T | undefined>;
  put<T>(key: string, value: T): Promise<void>;
  delete(key: string): Promise<unknown>;
  transaction?<T>(
    callback: (transaction: AzureOwnedDeleteClaimTransaction) => Promise<T>,
  ): Promise<T>;
}

interface AzureOwnedDeleteClaimTransaction {
  get<T>(key: string): Promise<T | undefined>;
  put<T>(key: string, value: T): Promise<void>;
  delete(key: string): Promise<unknown>;
}

interface AzureSecurityRule {
  name?: string;
  properties?: Record<string, unknown>;
}

interface AzureSKU {
  name?: string;
  resourceType?: string;
  capabilities?: { name?: string; value?: string }[];
}

interface AzureSharedInfraNames {
  vnet: string;
  nsg: string;
}

interface AzureARMOptions {
  lroTimeoutMs?: number;
  terminalResourceState?: { path: string; apiVersion: string };
}

class AzureHTTPError extends Error {
  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    readonly body: string,
  ) {
    super(`azure ${method} ${path}: http ${status}: ${body}`);
  }
}

export interface AzureDeferredCleanupRequest {
  name: string;
  location: string;
  subscription: string;
  resourceGroup: string;
  leaseID: string;
  slug: string;
  owner: string;
  createdAt: string;
}

export class AzureClient {
  private readonly env: Env;
  private readonly tenant: string;
  private readonly clientID: string;
  private readonly secret: string;
  readonly subscription: string;
  readonly resourceGroup: string;
  readonly vnet: string;
  readonly subnet: string;
  readonly nsg: string;
  readonly image: string;
  readonly sshCIDRs: string[];
  readonly defaultLocation: string;
  private cache?: TokenCache;
  private ephemeralOSSupport?: Map<string, boolean>;
  private readonly deferredCleanup:
    | ((request: AzureDeferredCleanupRequest) => Promise<void>)
    | undefined;
  private readonly ownedDeleteClaimStorage: AzureOwnedDeleteClaimStorage | undefined;
  fetcher: typeof fetch = (input, init) => fetch(input, init);

  constructor(
    env: Env,
    options: {
      location?: string;
      vnet?: string;
      nsg?: string;
      subscription?: string;
      resourceGroup?: string;
      deferredCleanup?: (request: AzureDeferredCleanupRequest) => Promise<void>;
      ownedDeleteClaimStorage?: AzureOwnedDeleteClaimStorage;
    } = {},
  ) {
    this.env = env;
    if (!env.AZURE_TENANT_ID) throw new Error("AZURE_TENANT_ID secret is required");
    if (!env.AZURE_CLIENT_ID) throw new Error("AZURE_CLIENT_ID secret is required");
    if (!env.AZURE_CLIENT_SECRET) throw new Error("AZURE_CLIENT_SECRET secret is required");
    const subscription = options.subscription?.trim() || env.AZURE_SUBSCRIPTION_ID?.trim();
    if (!subscription) throw new Error("AZURE_SUBSCRIPTION_ID secret is required");
    this.tenant = env.AZURE_TENANT_ID;
    this.clientID = env.AZURE_CLIENT_ID;
    this.secret = env.AZURE_CLIENT_SECRET;
    this.subscription = subscription;
    this.resourceGroup =
      options.resourceGroup?.trim() || env.CRABBOX_AZURE_RESOURCE_GROUP?.trim() || "crabbox-leases";
    this.vnet = options.vnet || env.CRABBOX_AZURE_VNET?.trim() || "crabbox-vnet";
    this.subnet = env.CRABBOX_AZURE_SUBNET?.trim() || "crabbox-subnet";
    this.nsg = options.nsg || env.CRABBOX_AZURE_NSG?.trim() || "crabbox-nsg";
    this.image = env.CRABBOX_AZURE_IMAGE?.trim() || DEFAULT_AZURE_LINUX_IMAGE;
    this.sshCIDRs = validatedCIDRs(
      (env.CRABBOX_AZURE_SSH_CIDRS ?? "").split(","),
      "CRABBOX_AZURE_SSH_CIDRS",
    );
    if (this.sshCIDRs.length === 0) this.sshCIDRs.push("0.0.0.0/0");
    this.defaultLocation = options.location || env.CRABBOX_AZURE_LOCATION?.trim() || "eastus";
    this.deferredCleanup = options.deferredCleanup;
    this.ownedDeleteClaimStorage = options.ownedDeleteClaimStorage;
  }

  async listCrabboxServers(): Promise<ProviderMachine[]> {
    const response = await this.arm<{ value: AzureVM[] }>(
      "GET",
      `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Compute/virtualMachines`,
      API_VERSIONS.compute,
    ).catch((error) => {
      if (isNotFound(error)) return { value: [] as AzureVM[] };
      throw error;
    });
    const tagged = (response.value ?? []).filter((vm) => vm.tags?.["crabbox"] === "true");
    const ips = await Promise.all(
      tagged.map((vm) =>
        vm.name ? this.publicIP(`${vm.name}-pip`).catch(() => "") : Promise.resolve(""),
      ),
    );
    return tagged.map((vm, index) => toMachine(vm, ips[index] ?? ""));
  }

  async listReconciliationResources(): Promise<ProviderMachine[]> {
    return azureReconciliationMachines(await this.listLeaseResourceInventory(), (cloudID) => ({
      vmID: this.resourceID(vmPath(this.resourceGroup, cloudID)),
      nicID: this.resourceID(
        networkPath(this.resourceGroup, "networkInterfaces", `${cloudID}-nic`),
      ),
      pipID: this.resourceID(
        networkPath(this.resourceGroup, "publicIPAddresses", `${cloudID}-pip`),
      ),
    }));
  }

  private async listLeaseResourceInventory(): Promise<AzureLeaseResourceInventory> {
    const [vms, nics, pips, disks] = await Promise.all([
      this.listResources<AzureVM>(
        `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Compute/virtualMachines`,
        API_VERSIONS.compute,
      ),
      this.listResources<AzureNIC>(
        `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Network/networkInterfaces`,
        API_VERSIONS.network,
      ),
      this.listResources<AzurePublicIP>(
        `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Network/publicIPAddresses`,
        API_VERSIONS.network,
      ),
      this.listResources<AzureDisk>(
        `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Compute/disks`,
        API_VERSIONS.disks,
      ),
    ]);
    return { vms, nics, pips, disks };
  }

  async getServer(name: string): Promise<ProviderMachine> {
    return toMachine(
      await this.arm<AzureVM>("GET", vmPath(this.resourceGroup, name), API_VERSIONS.compute),
      "",
    );
  }

  async findServer(name: string): Promise<ProviderMachine | undefined> {
    try {
      return await this.getServer(name);
    } catch (error) {
      if (azureVMNotFound(error, name)) return undefined;
      throw error;
    }
  }

  async recoverServerForLease(
    lease: Pick<
      LeaseRecord,
      | "id"
      | "slug"
      | "provider"
      | "owner"
      | "providerOwner"
      | "workspaceID"
      | "serverType"
      | "region"
    >,
  ): Promise<ProviderMachine | undefined> {
    const { vms, nics, pips, disks } = await this.listLeaseResourceInventory();
    const resources = azureLeaseResources({ vms, nics, pips, disks });
    const ownedCloudIDs = new Set<string>();
    for (const candidate of resources) {
      const labels = azureLabelsFromTags(candidate.resource.tags ?? {});
      if (labels["lease"] !== lease.id) continue;
      if (!providerLabelsOwnedByLease(labels, lease, "azure")) {
        throw new Error(
          `refusing to recover Azure lease ${lease.id}: ${candidate.kind} ownership does not match`,
        );
      }
      ownedCloudIDs.add(azureRecoveryCloudID(candidate.kind, candidate.resource.name, lease.id));
    }
    if (ownedCloudIDs.size === 0) return undefined;
    if (ownedCloudIDs.size > 1) {
      throw new Error(
        `ambiguous Azure recovery for ${lease.id}: ${ownedCloudIDs.size} resource sets`,
      );
    }

    const cloudID = [...ownedCloudIDs][0]!;
    const vm = vms.find((resource) => resource.name === cloudID);
    const nic = nics.find((resource) => resource.name === `${cloudID}-nic`);
    const pip = pips.find((resource) => resource.name === `${cloudID}-pip`);
    const disk = disks.find((resource) => resource.name === `${cloudID}-osdisk`);
    const exactResources = [
      {
        kind: "VM",
        resource: vm,
        path: vmPath(this.resourceGroup, cloudID),
      },
      {
        kind: "NIC",
        resource: nic,
        path: networkPath(this.resourceGroup, "networkInterfaces", `${cloudID}-nic`),
      },
      {
        kind: "public IP",
        resource: pip,
        path: networkPath(this.resourceGroup, "publicIPAddresses", `${cloudID}-pip`),
      },
    ];
    for (const candidate of exactResources) {
      if (candidate.resource) {
        this.requireOwnedResource(candidate.kind, candidate.resource, candidate.path, lease);
      }
    }

    const diskPath = `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Compute/disks/${cloudID}-osdisk`;
    if (disk) {
      this.requireResourceIdentity("disk", disk, diskPath, lease.id);
      const diskLabels = azureLabelsFromTags(disk.tags ?? {});
      if (!providerLabelsOwnedByLease(diskLabels, lease, "azure")) {
        if (azureHasOwnershipClaims(diskLabels) || !vm) {
          throw new Error(
            `refusing to recover Azure lease ${lease.id}: disk ownership does not match`,
          );
        }
      }
    }

    const expectedNICID = this.resourceID(
      networkPath(this.resourceGroup, "networkInterfaces", `${cloudID}-nic`),
    );
    const expectedPIPID = this.resourceID(
      networkPath(this.resourceGroup, "publicIPAddresses", `${cloudID}-pip`),
    );
    const expectedVMID = this.resourceID(vmPath(this.resourceGroup, cloudID));
    if (
      vm &&
      !vm.properties?.networkProfile?.networkInterfaces?.some((item) =>
        azureResourceIDEqual(item.id, expectedNICID),
      )
    ) {
      throw new Error(`refusing to recover Azure lease ${lease.id}: VM ownership is incomplete`);
    }
    if (
      nic &&
      pip &&
      !nic.properties?.ipConfigurations?.some((config) =>
        azureResourceIDEqual(config.properties?.publicIPAddress?.id, expectedPIPID),
      )
    ) {
      throw new Error(`refusing to recover Azure lease ${lease.id}: NIC ownership is incomplete`);
    }
    if (disk && !azureDiskAttachmentsMatchVM(disk, expectedVMID, Boolean(vm))) {
      throw new Error(`refusing to recover Azure lease ${lease.id}: disk ownership is incomplete`);
    }

    if (vm) {
      return toMachine(vm, pip?.properties?.ipAddress ?? "");
    }
    const labeled = [nic, pip, disk].find((resource) =>
      providerLabelsOwnedByLease(azureLabelsFromTags(resource?.tags ?? {}), lease, "azure"),
    );
    const labels = azureLabelsFromTags(labeled?.tags ?? {});
    const locations = new Set(
      [nic?.location, pip?.location, disk?.location].filter((value): value is string =>
        Boolean(value?.trim()),
      ),
    );
    if (locations.size > 1) {
      throw new Error(`refusing to recover Azure lease ${lease.id}: resource locations differ`);
    }
    return {
      provider: "azure",
      id: 0,
      cloudID,
      name: cloudID,
      status: "provisioning",
      serverType: labels["server_type"] || lease.serverType,
      host: pip?.properties?.ipAddress ?? "",
      ...(locations.size === 1
        ? { region: [...locations][0] }
        : lease.region
          ? { region: lease.region }
          : {}),
      labels,
    };
  }

  async createServerWithFallback(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<{
    server: ProviderMachine;
    serverType: string;
    market: string;
    attempts?: ProvisioningAttempt[];
  }> {
    const locations = azureRegionCandidates(config, this.env, this.defaultLocation);
    const multiRegion = locations.length > 1;
    const failures: string[] = [];
    const attempts: ProvisioningAttempt[] = [];
    for (const location of locations) {
      const client = this.clientForLocation(location, multiRegion);
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- region fallback must preserve operator preference order.
        const result = await client.createServerWithFallbackInLocation(
          config,
          location,
          leaseID,
          slug,
          owner,
        );
        const allAttempts = [...attempts, ...(result.attempts ?? [])];
        const server = {
          ...result.server,
          region: location,
          labels: { ...result.server.labels, region: location },
        };
        return allAttempts.length > 0
          ? { ...result, server, attempts: allAttempts }
          : { ...result, server };
      } catch (error) {
        if (providerProvisioningCleanupClaim(error)) throw error;
        const message = error instanceof Error ? error.message : String(error);
        attempts.push({
          region: location,
          serverType: config.serverType,
          market: config.capacityMarket,
          category: azureProvisioningErrorCategory(message) || "region",
          message: conciseAzureProvisioningMessage(message),
        });
        failures.push(`${location}: ${message}`);
        if (!isRetryableProvisioningError(message)) break;
      }
    }
    throw new Error(failures.join("; "));
  }

  private clientForLocation(location: string, multiRegion: boolean): AzureClient {
    if (location === this.defaultLocation && !multiRegion) return this;
    const options: {
      location: string;
      vnet: string;
      nsg: string;
      deferredCleanup?: (request: AzureDeferredCleanupRequest) => Promise<void>;
      ownedDeleteClaimStorage?: AzureOwnedDeleteClaimStorage;
    } = {
      location,
      vnet: multiRegion ? azureRegionalName(this.vnet, location) : this.vnet,
      nsg: multiRegion ? azureRegionalName(this.nsg, location) : this.nsg,
    };
    if (this.deferredCleanup) {
      options.deferredCleanup = this.deferredCleanup;
    }
    if (this.ownedDeleteClaimStorage) {
      options.ownedDeleteClaimStorage = this.ownedDeleteClaimStorage;
    }
    return new AzureClient(this.env, options);
  }

  private async createServerWithFallbackInLocation(
    config: LeaseConfig,
    location: string,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<{
    server: ProviderMachine;
    serverType: string;
    market: string;
    attempts?: ProvisioningAttempt[];
  }> {
    const candidates = azureProvisioningCandidatesForConfig(config);
    const failures: string[] = [];
    const attempts: ProvisioningAttempt[] = [];
    let infra: AzureSharedInfraNames | undefined;
    for (let index = 0; index < candidates.length; index += 1) {
      const vmSize = candidates[index] ?? config.serverType;
      const nextConfig = { ...config, serverType: vmSize };
      if (!nextConfig.azureSnapshot) {
        // Validate preview-only OS disk requirements before allocating network resources.
        // oxlint-disable-next-line eslint/no-await-in-loop -- SKU fallback must stay sequential.
        await this.validateOSDiskMode(nextConfig, location);
      }
      try {
        if (!infra) {
          // oxlint-disable-next-line eslint/no-await-in-loop -- shared infra is created once, after config validation.
          infra = await this.ensureSharedInfra(location, config);
        }
        // oxlint-disable-next-line eslint/no-await-in-loop -- SKU fallback must stay sequential.
        const server = await this.createVM(
          nextConfig,
          location,
          leaseID,
          slug,
          owner,
          infra,
          azureAttemptNameSeed(leaseID, location, config.capacityMarket, index),
        );
        return attempts.length > 0
          ? { server, serverType: vmSize, market: config.capacityMarket, attempts }
          : { server, serverType: vmSize, market: config.capacityMarket };
      } catch (error) {
        if (providerProvisioningCleanupClaim(error)) throw error;
        const message = error instanceof Error ? error.message : String(error);
        attempts.push({
          region: location,
          serverType: vmSize,
          market: config.capacityMarket,
          category: azureProvisioningErrorCategory(message) || "fatal",
          message: conciseAzureProvisioningMessage(message),
        });
        failures.push(`${vmSize}: ${message}`);
        if (!isRetryableProvisioningError(message)) break;
      }
    }
    if (config.capacityMarket === "spot" && config.capacityFallback.startsWith("on-demand")) {
      for (let index = 0; index < candidates.length; index += 1) {
        const vmSize = candidates[index] ?? config.serverType;
        const nextConfig: LeaseConfig = {
          ...config,
          capacityMarket: "on-demand",
          serverType: vmSize,
        };
        if (!nextConfig.azureSnapshot) {
          // oxlint-disable-next-line eslint/no-await-in-loop -- market fallback must preserve ordered capacity preference.
          await this.validateOSDiskMode(nextConfig, location);
        }
        try {
          if (!infra) {
            // oxlint-disable-next-line eslint/no-await-in-loop -- shared infra is created once, after config validation.
            infra = await this.ensureSharedInfra(location, config);
          }
          // oxlint-disable-next-line eslint/no-await-in-loop -- market fallback must preserve ordered capacity preference.
          const server = await this.createVM(
            nextConfig,
            location,
            leaseID,
            slug,
            owner,
            infra,
            azureAttemptNameSeed(leaseID, location, "on-demand", index),
          );
          return attempts.length > 0
            ? { server, serverType: vmSize, market: "on-demand", attempts }
            : { server, serverType: vmSize, market: "on-demand" };
        } catch (error) {
          if (providerProvisioningCleanupClaim(error)) throw error;
          const message = error instanceof Error ? error.message : String(error);
          attempts.push({
            region: location,
            serverType: vmSize,
            market: "on-demand",
            category: azureProvisioningErrorCategory(message) || "fatal",
            message: conciseAzureProvisioningMessage(message),
          });
          failures.push(`on-demand ${vmSize}: ${message}`);
          if (!isRetryableProvisioningError(message)) break;
        }
      }
    }
    throw new Error(failures.join("; "));
  }

  async deleteServer(name: string): Promise<void> {
    for (let attempt = 0; ; attempt += 1) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- delete retries must wait for Azure dependency locks.
      const result = await this.deleteServerOnce(name);
      if (result.errors.length === 0) return;
      if (!result.retry || attempt >= DELETE_RETRY_ATTEMPTS - 1) {
        throw new Error(result.errors.join("; "));
      }
      console.warn(
        `azure delete retry name=${name} attempt=${attempt + 1}/${DELETE_RETRY_ATTEMPTS}: ${result.errors.join("; ")}`,
      );
      // oxlint-disable-next-line eslint/no-await-in-loop -- the next delete attempt depends on this delay.
      await sleep(DELETE_RETRY_DELAY_MS);
    }
  }

  async deleteOwnedServer(
    lease: Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner" | "providerScope">,
    context?: AzureOwnedDeleteReleaseContext,
  ): Promise<void> {
    if (this.ownedDeleteClaimStorage && !this.ownedDeleteClaimStorage.transaction) {
      throw new Error(
        `refusing to delete Azure lease ${lease.id}: transactional cleanup claim storage is required`,
      );
    }
    if (lease.providerScope !== this.providerScope()) {
      throw new Error(
        `refusing to delete Azure lease ${lease.id}: stored provider scope does not match the cleanup client`,
      );
    }
    const claimKey = azureOwnedDeleteClaimKey(this.providerScope(), lease.cloudID, lease.id);
    const observedStableResourceIdentity = context
      ? azureStableResourceIdentityFromObserved(context.resourceIdentity)
      : undefined;
    let claim = await this.ownedDeleteClaimStorage?.get<AzureOwnedDeleteClaim>(claimKey);
    if (!claim) {
      claim = await this.persistOwnedDeleteClaimCreation(
        claimKey,
        lease,
        this.ownedDeleteClaimPreparation(lease),
      );
    }
    if (claim.preparing) {
      const inspection = await this.ownedDeleteResources(lease, {
        prepareClaim: true,
        ...(context?.resourceIdentity
          ? { expectedResourceIdentity: context.resourceIdentity }
          : {}),
        ...(observedStableResourceIdentity
          ? { expectedStableResourceIdentity: observedStableResourceIdentity }
          : {}),
      });
      const preparedClaim = this.ownedDeleteClaim(
        lease,
        inspection.diskResource,
        context?.resourceIdentity,
        observedStableResourceIdentity ?? inspection.stableResourceIdentity,
      );
      claim = await this.persistOwnedDeleteClaimTransition(claimKey, lease, claim, preparedClaim);
    }
    this.requireOwnedDeleteClaim(lease, claim);
    const canonicalVMID = this.resourceID(vmPath(this.resourceGroup, lease.cloudID));
    const canonicalNICID = this.resourceID(
      networkPath(this.resourceGroup, "networkInterfaces", `${lease.cloudID}-nic`),
    );
    if (!claim.stableResourceIdentity && (!claim.canonicalVMID || !claim.canonicalNICID)) {
      const previousClaim = claim;
      const canonicalClaim = {
        ...claim,
        canonicalVMID: claim.canonicalVMID ?? canonicalVMID,
        canonicalNICID: claim.canonicalNICID ?? canonicalNICID,
      };
      claim = await this.persistOwnedDeleteClaimTransition(
        claimKey,
        lease,
        previousClaim,
        canonicalClaim,
      );
    }
    if (
      observedStableResourceIdentity &&
      claim.stableResourceIdentity &&
      !azureStableResourceIdentityExactlyMatches(
        claim.stableResourceIdentity,
        observedStableResourceIdentity,
      )
    ) {
      const previousPartialStableResourceIdentity =
        claim.partialStableResourceIdentity ?? claim.stableResourceIdentity;
      const currentInspection = await this.ownedDeleteResources(lease, {
        claim,
        prepareClaim: true,
        ...(claim.resourceIdentity ? { expectedResourceIdentity: claim.resourceIdentity } : {}),
      });
      const verifiedPartial = Boolean(
        previousPartialStableResourceIdentity &&
        azureStableResourceIdentityMatches(
          previousPartialStableResourceIdentity,
          currentInspection.stableResourceIdentity,
          claim.deletedStableResourceIdentity,
        ),
      );
      const inspection = await this.ownedDeleteResources(lease, {
        claim,
        prepareClaim: true,
        ...(context?.resourceIdentity
          ? { expectedResourceIdentity: context.resourceIdentity }
          : {}),
        expectedStableResourceIdentity: observedStableResourceIdentity,
      });
      const replacementClaim = this.ownedDeleteClaim(
        lease,
        inspection.diskResource,
        context?.resourceIdentity,
        observedStableResourceIdentity,
      );
      const previousClaim = claim;
      const nextClaim = {
        ...replacementClaim,
        ...(claim.canonicalVMID ? { canonicalVMID: claim.canonicalVMID } : {}),
        ...(claim.canonicalNICID ? { canonicalNICID: claim.canonicalNICID } : {}),
        ...(verifiedPartial
          ? { partialStableResourceIdentity: previousPartialStableResourceIdentity }
          : {}),
        ...(claim.deletedStableResourceIdentity
          ? { deletedStableResourceIdentity: claim.deletedStableResourceIdentity }
          : {}),
      };
      claim = await this.persistOwnedDeleteClaimTransition(
        claimKey,
        lease,
        previousClaim,
        nextClaim,
      );
    }
    let expectedStableResourceIdentity =
      claim.stableResourceIdentity ?? observedStableResourceIdentity;
    const expectedResourceIdentity = context?.resourceIdentity || claim.resourceIdentity;

    const order: (keyof AzureOwnedDeleteResources)[] = ["vm", "nic", "pip", "disk"];
    for (const kind of order) {
      for (let attempt = 0; ; attempt += 1) {
        // Re-read every surviving resource before each destructive operation so
        // a same-name replacement cannot inherit authorization from an older read.
        // oxlint-disable-next-line eslint/no-await-in-loop -- every delete requires fresh ownership proof.
        const resources = await this.ownedDeleteResources(lease, {
          claim,
          ...(expectedResourceIdentity ? { expectedResourceIdentity } : {}),
          ...(expectedStableResourceIdentity ? { expectedStableResourceIdentity } : {}),
        });
        if (!resources.vm && !resources.nic && !resources.pip && !resources.disk) {
          // oxlint-disable-next-line eslint/no-await-in-loop -- clear only after the fresh inventory and claim sequence both prove cleanup complete.
          await this.clearOwnedDeleteClaim(claimKey, lease, claim);
          return;
        }
        if (!claim.stableResourceIdentity) {
          if (
            !resources.vm ||
            !resources.nic ||
            !resources.pip ||
            (!resources.disk && !resources.ephemeralOSDisk)
          ) {
            throw new Error(
              `refusing to delete Azure resources for ${lease.cloudID}: legacy cleanup claim cannot prove the complete original resource set`,
            );
          }
          const previousClaim = claim;
          const upgradedClaim = {
            ...claim,
            version: 2 as const,
            ...(context?.resourceIdentity ? { resourceIdentity: context.resourceIdentity } : {}),
            stableResourceIdentity:
              expectedStableResourceIdentity ?? resources.stableResourceIdentity,
          };
          // oxlint-disable-next-line eslint/no-await-in-loop -- the versioned legacy transition fences deletion before the loop can advance.
          claim = await this.persistOwnedDeleteClaimTransition(
            claimKey,
            lease,
            previousClaim,
            upgradedClaim,
          );
          expectedStableResourceIdentity = claim.stableResourceIdentity;
        }
        if (!resources[kind]) break;
        const selected: AzureOwnedDeleteResources = {
          vm: false,
          nic: false,
          pip: false,
          disk: false,
        };
        selected[kind] = true;
        // oxlint-disable-next-line eslint/no-await-in-loop -- each delete uses the fresh ownership proof above.
        const result = await this.deleteServerOnce(lease.cloudID, selected, true);
        if (result.errors.length === 0) {
          const nextClaim = {
            ...claim,
            deletedStableResourceIdentity: azureStableResourceIdentityWithDeletedMember(
              claim.deletedStableResourceIdentity,
              resources.stableResourceIdentity,
              kind,
            ),
          };
          // Persist successful deletion progress before another member can be
          // considered absent on a retry. A failed write therefore stops cleanup.
          // oxlint-disable-next-line eslint/no-await-in-loop -- ordered cleanup is fenced by the durable progress write.
          claim = await this.persistOwnedDeleteProgress(claimKey, lease, nextClaim);
          break;
        }
        if (!result.retry || attempt >= DELETE_RETRY_ATTEMPTS - 1) {
          throw new Error(result.errors.join("; "));
        }
        console.warn(
          `azure owned delete retry name=${lease.cloudID} resource=${kind} attempt=${attempt + 1}/${DELETE_RETRY_ATTEMPTS}: ${result.errors.join("; ")}`,
        );
        // oxlint-disable-next-line eslint/no-await-in-loop -- the next ownership read follows this delay.
        await sleep(DELETE_RETRY_DELAY_MS);
      }
    }
    await this.clearOwnedDeleteClaim(claimKey, lease, claim);
  }

  private async clearOwnedDeleteClaim(
    claimKey: string,
    lease: Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner">,
    claim: AzureOwnedDeleteClaim,
  ): Promise<void> {
    this.requireOwnedDeleteClaim(lease, claim);
    const storage = this.ownedDeleteClaimStorage;
    if (!storage) return;
    if (!storage.transaction) {
      throw new Error(
        `refusing to clear Azure lease ${lease.id}: transactional cleanup claim storage is required`,
      );
    }
    await storage.transaction(async (transaction) => {
      const latest = await transaction.get<AzureOwnedDeleteClaim>(claimKey);
      if (!latest) return;
      this.requireOwnedDeleteClaim(lease, latest);
      if (!azureOwnedDeleteClaimsShareSequence(latest, claim)) {
        throw new Error(`refusing to clear Azure lease ${lease.id}: durable cleanup claim changed`);
      }
      await transaction.delete(claimKey);
    });
  }

  private async persistOwnedDeleteClaimCreation(
    claimKey: string,
    lease: Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner">,
    claim: AzureOwnedDeleteClaim,
  ): Promise<AzureOwnedDeleteClaim> {
    this.requireOwnedDeleteClaim(lease, claim);
    const storage = this.ownedDeleteClaimStorage;
    if (!storage) return claim;
    if (!storage.transaction) {
      throw new Error(
        `refusing to create Azure lease ${lease.id}: transactional cleanup claim storage is required`,
      );
    }
    return await storage.transaction(async (transaction) => {
      const latest = await transaction.get<AzureOwnedDeleteClaim>(claimKey);
      if (latest) {
        this.requireOwnedDeleteClaim(lease, latest);
        return latest;
      }
      await transaction.put(claimKey, claim);
      return claim;
    });
  }

  private async persistOwnedDeleteClaimTransition(
    claimKey: string,
    lease: Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner">,
    previous: AzureOwnedDeleteClaim,
    next: AzureOwnedDeleteClaim,
  ): Promise<AzureOwnedDeleteClaim> {
    this.requireOwnedDeleteClaim(lease, previous);
    this.requireOwnedDeleteClaim(lease, next);
    const storage = this.ownedDeleteClaimStorage;
    if (!storage) return next;
    if (!storage.transaction) {
      throw new Error(
        `refusing to update Azure lease ${lease.id}: transactional cleanup claim storage is required`,
      );
    }
    return await storage.transaction(async (transaction) => {
      const latest = await transaction.get<AzureOwnedDeleteClaim>(claimKey);
      if (!latest) {
        throw new Error(
          `refusing to update Azure lease ${lease.id}: durable cleanup claim disappeared`,
        );
      }
      this.requireOwnedDeleteClaim(lease, latest);
      const latestAlreadyTransitioned = azureOwnedDeleteClaimsShareSequence(latest, next);
      if (!latestAlreadyTransitioned && !azureOwnedDeleteClaimsShareSequence(latest, previous)) {
        throw new Error(
          `refusing to update Azure lease ${lease.id}: durable cleanup claim changed`,
        );
      }
      const mergedProgress = azureMergeDeletedStableResourceIdentity(
        latest.deletedStableResourceIdentity,
        next.deletedStableResourceIdentity,
      );
      const transitioned: AzureOwnedDeleteClaim = {
        ...(latestAlreadyTransitioned ? latest : next),
        ...(mergedProgress ? { deletedStableResourceIdentity: mergedProgress } : {}),
      };
      this.requireOwnedDeleteClaim(lease, transitioned);
      await transaction.put(claimKey, transitioned);
      return transitioned;
    });
  }

  private async persistOwnedDeleteProgress(
    claimKey: string,
    lease: Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner">,
    claim: AzureOwnedDeleteClaim,
  ): Promise<AzureOwnedDeleteClaim> {
    this.requireOwnedDeleteClaim(lease, claim);
    const storage = this.ownedDeleteClaimStorage;
    if (!storage) return claim;
    if (!storage.transaction) {
      throw new Error(
        `refusing to update Azure lease ${lease.id}: transactional cleanup claim storage is required`,
      );
    }
    return await storage.transaction(async (transaction) => {
      const latest = await transaction.get<AzureOwnedDeleteClaim>(claimKey);
      if (!latest) {
        throw new Error(
          `refusing to update Azure lease ${lease.id}: durable cleanup claim disappeared`,
        );
      }
      this.requireOwnedDeleteClaim(lease, latest);
      if (!azureOwnedDeleteClaimsShareSequence(latest, claim)) {
        throw new Error(
          `refusing to update Azure lease ${lease.id}: durable cleanup claim changed`,
        );
      }
      const mergedProgress = azureMergeDeletedStableResourceIdentity(
        latest.deletedStableResourceIdentity,
        claim.deletedStableResourceIdentity,
      );
      if (!mergedProgress) {
        throw new Error(`refusing to update Azure lease ${lease.id}: deletion progress is empty`);
      }
      const merged: AzureOwnedDeleteClaim = {
        ...latest,
        deletedStableResourceIdentity: mergedProgress,
      };
      this.requireOwnedDeleteClaim(lease, merged);
      await transaction.put(claimKey, merged);
      return merged;
    });
  }

  private async ownedDeleteResources(
    lease: Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner">,
    options: {
      claim?: AzureOwnedDeleteClaim;
      prepareClaim?: boolean;
      expectedResourceIdentity?: string;
      expectedStableResourceIdentity?: string;
    } = {},
  ): Promise<AzureOwnedDeleteInspection> {
    const name = lease.cloudID;
    const vmResourcePath = vmPath(this.resourceGroup, name);
    const nicResourcePath = networkPath(this.resourceGroup, "networkInterfaces", `${name}-nic`);
    const pipResourcePath = networkPath(this.resourceGroup, "publicIPAddresses", `${name}-pip`);
    const diskResourcePath = `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Compute/disks/${name}-osdisk`;
    const [vm, nic, pip, initialDisk] = await Promise.all([
      this.ownedResource<AzureVM>(vmResourcePath, API_VERSIONS.compute, "virtualMachines", name),
      this.ownedResource<AzureNIC>(
        nicResourcePath,
        API_VERSIONS.network,
        "networkInterfaces",
        `${name}-nic`,
      ),
      this.ownedResource<AzurePublicIP>(
        pipResourcePath,
        API_VERSIONS.network,
        "publicIPAddresses",
        `${name}-pip`,
      ),
      this.ownedResource<AzureDisk>(
        diskResourcePath,
        API_VERSIONS.disks,
        "disks",
        `${name}-osdisk`,
      ),
    ]);

    if (vm) this.requireOwnedResource("VM", vm, vmResourcePath, lease);
    if (nic) this.requireOwnedResource("NIC", nic, nicResourcePath, lease);
    if (pip) this.requireOwnedResource("public IP", pip, pipResourcePath, lease);
    const initialClaimSetIncomplete =
      options.prepareClaim &&
      !options.claim &&
      Boolean(vm || nic || pip || initialDisk) &&
      (!nic || !pip || (!initialDisk && !azureVMUsesEphemeralOSDisk(vm)));

    const expectedNICID = this.resourceID(nicResourcePath);
    const expectedPIPID = this.resourceID(pipResourcePath);
    const expectedDiskID = this.resourceID(diskResourcePath);
    const expectedVMID = this.resourceID(vmResourcePath);
    const currentResources: AzureLeaseResource[] = [
      ...(vm ? [{ kind: "virtualMachines" as const, resource: vm }] : []),
      ...(nic ? [{ kind: "networkInterfaces" as const, resource: nic }] : []),
      ...(pip ? [{ kind: "publicIPAddresses" as const, resource: pip }] : []),
      ...(initialDisk ? [{ kind: "disks" as const, resource: initialDisk }] : []),
    ];
    const currentStableResourceIdentity = azureStableResourceIdentity(currentResources);
    const currentResourceIdentity = azureReconciliationResourceIdentity(currentResources);
    if (
      options.expectedStableResourceIdentity &&
      !azureStableResourceIdentityMatches(
        options.expectedStableResourceIdentity,
        currentStableResourceIdentity,
        options.claim?.deletedStableResourceIdentity,
      )
    ) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: observed resource identity changed`,
      );
    }
    if (
      options.expectedResourceIdentity &&
      !azureResourceIdentityMatches(
        options.expectedResourceIdentity,
        currentResourceIdentity,
        options.claim?.deletedStableResourceIdentity,
      )
    ) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: observed resource labels changed`,
      );
    }
    const vmDiskID = vm?.properties?.storageProfile?.osDisk?.managedDisk?.id;
    if (vm && azureVMHasDataDisks(vm)) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: VM has unexpected data disks`,
      );
    }
    if (vm && azureVMHasCascadeDelete(vm)) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: VM has cascading delete options`,
      );
    }
    const nicVirtualMachine = nic?.properties?.virtualMachine;
    const staleCanonicalNICAttachmentAllowed =
      !vm &&
      azureStableResourceIdentityIncludesResourceID(
        options.claim?.deletedStableResourceIdentity,
        "virtualMachines",
        expectedVMID,
      );
    if (
      nicVirtualMachine &&
      (!azureResourceIDEqual(nicVirtualMachine.id, expectedVMID) ||
        (!vm && !staleCanonicalNICAttachmentAllowed))
    ) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: NIC is attached to an unexpected VM`,
      );
    }
    if (vm && !azureVMReferencesOnlyNIC(vm, expectedNICID)) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: VM references an unexpected NIC`,
      );
    }
    if (vm && !nic) {
      throw new Error(`refusing to delete Azure resources for ${name}: canonical NIC is missing`);
    }
    if (
      vm &&
      !vm.properties?.networkProfile?.networkInterfaces?.some((item) =>
        azureResourceIDEqual(item.id, expectedNICID),
      )
    ) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: VM does not own ${name}-nic`,
      );
    }
    if (nic && azureNICHasDisqualifyingServiceAssociation(nic)) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: NIC has an unexpected service association`,
      );
    }
    if (nic && azureNICHasCascadeDelete(nic)) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: NIC has cascading delete options`,
      );
    }
    const missingCanonicalPIPAllowed =
      !pip &&
      azureStableResourceIdentityIncludesResourceID(
        options.claim?.deletedStableResourceIdentity,
        "publicIPAddresses",
        expectedPIPID,
      );
    if (
      nic &&
      !azureNICReferencesOnlyPIP(nic, expectedPIPID, Boolean(pip) || missingCanonicalPIPAllowed)
    ) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: NIC references an unexpected public IP`,
      );
    }
    if (
      nic &&
      pip &&
      !nic.properties?.ipConfigurations?.some((config) =>
        azureResourceIDEqual(config.properties?.publicIPAddress?.id, expectedPIPID),
      )
    ) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: NIC does not own ${name}-pip`,
      );
    }
    if (
      pip &&
      !azurePublicIPAttachmentMatchesNIC(
        pip,
        nic,
        expectedPIPID,
        expectedNICID,
        !nic &&
          azureStableResourceIdentityIncludesResourceID(
            options.claim?.deletedStableResourceIdentity,
            "networkInterfaces",
            expectedNICID,
          ),
      )
    ) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: public IP is attached to an unexpected configuration`,
      );
    }
    if (options.prepareClaim && vmDiskID) {
      if (!azureResourceIDEqual(vmDiskID, expectedDiskID)) {
        throw new Error(
          `refusing to delete Azure resources for ${name}: VM does not own ${name}-osdisk`,
        );
      }
      if (!initialDisk) {
        throw new Error(
          `refusing to delete Azure resources for ${name}: managed OS disk identity is not yet available`,
        );
      }
    }

    let disk = initialDisk;
    let diskOwnedByLease = false;
    if (disk) {
      this.requireResourceIdentity("disk", disk, diskResourcePath, lease.id);
      diskOwnedByLease = providerLabelsOwnedByLease(
        azureLabelsFromTags(disk.tags ?? {}),
        lease,
        "azure",
      );
      if (vm && !azureResourceIDEqual(vmDiskID, expectedDiskID)) {
        throw new Error(
          `refusing to delete Azure resources for ${name}: VM does not own ${name}-osdisk`,
        );
      }
      if (!diskOwnedByLease) {
        const diskLabels = azureLabelsFromTags(disk.tags ?? {});
        if (azureHasOwnershipClaims(diskLabels)) {
          throw new Error(
            `refusing to delete Azure disk ${name}-osdisk: ownership does not match lease ${lease.id}`,
          );
        }
        if (
          !options.prepareClaim ||
          !vm ||
          !azureResourceIDEqual(vmDiskID, expectedDiskID) ||
          !azureDiskAttachmentsMatchVM(disk, this.resourceID(vmResourcePath), true)
        ) {
          throw new Error(
            `refusing to delete Azure disk ${name}-osdisk: ownership does not match lease ${lease.id}`,
          );
        }
        const uniqueID = this.requireDiskUniqueID(disk, lease.id);
        const ownershipTags = azureOwnershipTags(vm.tags ?? {}, lease);
        // Azure-created OS disks do not inherit VM tags. Bind the disk while
        // the verified VM association is live. The immutable identity check
        // prevents a concurrent same-name replacement from receiving the claim.
        await this.arm("PATCH", diskResourcePath, API_VERSIONS.disks, {
          tags: { ...disk.tags, ...ownershipTags },
        });
        const taggedDisk = await this.arm<AzureDisk>("GET", diskResourcePath, API_VERSIONS.disks);
        this.requireOwnedResource("disk", taggedDisk, diskResourcePath, lease);
        if (
          this.requireDiskUniqueID(taggedDisk, lease.id) !== uniqueID ||
          !azureDiskAttachmentsMatchVM(taggedDisk, this.resourceID(vmResourcePath), true)
        ) {
          throw new Error(
            `refusing to delete Azure disk ${name}-osdisk: live association changed while binding lease ${lease.id}`,
          );
        }
        disk = taggedDisk;
        diskOwnedByLease = true;
      } else {
        this.requireOwnedResource("disk", disk, diskResourcePath, lease);
      }

      const uniqueID = this.requireDiskUniqueID(disk, lease.id);
      if (options.prepareClaim) {
        if (vm) {
          if (
            !azureResourceIDEqual(vmDiskID, expectedDiskID) ||
            !azureDiskAttachmentsMatchVM(disk, this.resourceID(vmResourcePath), true)
          ) {
            throw new Error(
              `refusing to delete Azure disk ${name}-osdisk: live association does not match lease ${lease.id}`,
            );
          }
        } else if (
          !diskOwnedByLease ||
          !azureDiskAttachmentsMatchVM(disk, this.resourceID(vmResourcePath), false)
        ) {
          throw new Error(
            `refusing to delete Azure disk ${name}-osdisk: live association does not match lease ${lease.id}`,
          );
        }
      } else {
        const claimedDisk = options.claim?.disk;
        if (
          !claimedDisk ||
          !azureResourceIDEqual(claimedDisk.resourceID, expectedDiskID) ||
          claimedDisk.uniqueID !== uniqueID
        ) {
          throw new Error(
            `refusing to delete Azure disk ${name}-osdisk: immutable identity does not match lease ${lease.id}`,
          );
        }
        if (!azureDiskAttachmentsMatchVM(disk, this.resourceID(vmResourcePath), Boolean(vm))) {
          throw new Error(
            `refusing to delete Azure disk ${name}-osdisk: live association does not match lease ${lease.id}`,
          );
        }
      }
    }

    if (initialClaimSetIncomplete) {
      throw new Error(
        `refusing to delete Azure resources for ${name}: canonical companion set is incomplete`,
      );
    }

    return {
      vm: Boolean(vm),
      nic: Boolean(nic),
      pip: Boolean(pip),
      disk: Boolean(disk),
      stableResourceIdentity: currentStableResourceIdentity,
      ephemeralOSDisk: azureVMUsesEphemeralOSDisk(vm),
      ...(disk ? { diskResource: disk } : {}),
    };
  }

  private ownedDeleteClaimPreparation(
    lease: Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner">,
  ): AzureOwnedDeleteClaim {
    return {
      version: 2,
      provider: "azure",
      leaseID: lease.id,
      slug: lease.slug ?? "",
      owner: lease.owner,
      cloudID: lease.cloudID,
      providerScope: this.providerScope(),
      preparing: true,
    };
  }

  private ownedDeleteClaim(
    lease: Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner">,
    disk: AzureDisk | undefined,
    resourceIdentity: string | undefined,
    stableResourceIdentity: string,
  ): AzureOwnedDeleteClaim {
    const diskResourcePath = `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Compute/disks/${lease.cloudID}-osdisk`;
    return {
      version: 2,
      provider: "azure",
      leaseID: lease.id,
      slug: lease.slug ?? "",
      owner: lease.owner,
      cloudID: lease.cloudID,
      providerScope: this.providerScope(),
      ...(resourceIdentity ? { resourceIdentity } : {}),
      stableResourceIdentity,
      ...(disk
        ? {
            disk: {
              resourceID: this.resourceID(diskResourcePath),
              uniqueID: this.requireDiskUniqueID(disk, lease.id),
            },
          }
        : {}),
    };
  }

  private requireOwnedDeleteClaim(
    lease: Pick<LeaseRecord, "id" | "slug" | "provider" | "cloudID" | "owner">,
    claim: AzureOwnedDeleteClaim,
  ): void {
    const expectedDiskID = this.resourceID(
      `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Compute/disks/${lease.cloudID}-osdisk`,
    );
    const expectedVMID = this.resourceID(vmPath(this.resourceGroup, lease.cloudID));
    const expectedNICID = this.resourceID(
      networkPath(this.resourceGroup, "networkInterfaces", `${lease.cloudID}-nic`),
    );
    try {
      if (claim.resourceIdentity !== undefined) {
        if (
          claim.stableResourceIdentity !==
          azureStableResourceIdentityFromObserved(claim.resourceIdentity)
        ) {
          throw new Error("resource identity mismatch");
        }
      }
      if (claim.stableResourceIdentity !== undefined) {
        azureStableResourceIdentityEntries(claim.stableResourceIdentity);
      }
      if (claim.partialStableResourceIdentity !== undefined) {
        azureStableResourceIdentityEntries(claim.partialStableResourceIdentity);
      }
      if (
        !azureStableResourceIdentityDeletionProgressValid(
          claim.stableResourceIdentity,
          claim.partialStableResourceIdentity,
          claim.deletedStableResourceIdentity,
        )
      ) {
        throw new Error("invalid deletion progress");
      }
    } catch {
      throw new Error(`refusing to delete Azure lease ${lease.id}: durable cleanup claim mismatch`);
    }
    const preparingValid = claim.preparing
      ? claim.version === 2 &&
        claim.resourceIdentity === undefined &&
        claim.stableResourceIdentity === undefined &&
        claim.partialStableResourceIdentity === undefined &&
        claim.deletedStableResourceIdentity === undefined &&
        claim.disk === undefined
      : claim.preparing === undefined &&
        (claim.version === 1 || claim.stableResourceIdentity !== undefined);
    if (
      (claim.version !== 1 && claim.version !== 2) ||
      !preparingValid ||
      claim.provider !== "azure" ||
      claim.leaseID !== lease.id ||
      claim.slug !== (lease.slug ?? "") ||
      claim.owner !== lease.owner ||
      claim.cloudID !== lease.cloudID ||
      claim.providerScope !== this.providerScope() ||
      (claim.canonicalVMID !== undefined &&
        !azureResourceIDEqual(claim.canonicalVMID, expectedVMID)) ||
      (claim.canonicalNICID !== undefined &&
        !azureResourceIDEqual(claim.canonicalNICID, expectedNICID)) ||
      (claim.disk &&
        (!azureResourceIDEqual(claim.disk.resourceID, expectedDiskID) ||
          !claim.disk.uniqueID.trim()))
    ) {
      throw new Error(`refusing to delete Azure lease ${lease.id}: durable cleanup claim mismatch`);
    }
  }

  private requireDiskUniqueID(disk: AzureDisk, leaseID: string): string {
    const uniqueID = disk.properties?.uniqueId?.trim();
    if (!uniqueID) {
      throw new Error(
        `refusing to delete Azure disk ${disk.name ?? "unknown"}: immutable identity is missing for lease ${leaseID}`,
      );
    }
    return uniqueID;
  }

  private async ownedResource<T>(
    path: string,
    apiVersion: string,
    kind: string,
    name: string,
  ): Promise<T | undefined> {
    try {
      return await this.arm<T>("GET", path, apiVersion);
    } catch (error) {
      if (azureResourceNotFound(error, kind, name)) return undefined;
      throw error;
    }
  }

  private async listResources<T>(path: string, apiVersion: string): Promise<T[]> {
    let response = await this.arm<AzureResourceList<T>>("GET", path, apiVersion).catch(
      (error): AzureResourceList<T> => {
        if (isNotFound(error)) return { value: [] };
        throw error;
      },
    );
    const resources = [...(response.value ?? [])];
    const seen = new Set<string>();
    while (response.nextLink) {
      const nextLink = this.validatedNextLink(response.nextLink);
      if (seen.has(nextLink)) {
        throw new Error(`azure resource listing returned a pagination cycle for ${path}`);
      }
      seen.add(nextLink);
      // oxlint-disable-next-line eslint/no-await-in-loop -- every page is identified by the prior response.
      response = await this.fetchResourcePage<T>(nextLink);
      resources.push(...(response.value ?? []));
    }
    return resources;
  }

  private validatedNextLink(value: string): string {
    const url = new URL(value);
    const expectedPrefix = `/subscriptions/${this.subscription}/`.toLowerCase();
    if (
      url.origin !== "https://management.azure.com" ||
      url.username ||
      url.password ||
      !url.pathname.toLowerCase().startsWith(expectedPrefix)
    ) {
      throw new Error("azure resource listing returned an invalid nextLink");
    }
    return url.toString();
  }

  private async fetchResourcePage<T>(url: string): Promise<AzureResourceList<T>> {
    const response = await this.fetcher(url, {
      headers: { authorization: `Bearer ${await this.token()}` },
    });
    if (!response.ok) {
      throw new AzureHTTPError(
        "GET",
        new URL(url).pathname,
        response.status,
        await safeBody(response),
      );
    }
    return (await response.json()) as AzureResourceList<T>;
  }

  private requireOwnedResource(
    kind: string,
    resource: { id?: string; name?: string; tags?: Record<string, string> },
    path: string,
    lease: Pick<LeaseRecord, "id" | "slug" | "provider" | "owner">,
  ): void {
    this.requireResourceIdentity(kind, resource, path, lease.id);
    if (!providerLabelsOwnedByLease(azureLabelsFromTags(resource.tags ?? {}), lease, "azure")) {
      throw new Error(
        `refusing to delete Azure ${kind} ${azureResourceName(path)}: ownership does not match lease ${lease.id}`,
      );
    }
  }

  private requireResourceIdentity(
    kind: string,
    resource: { id?: string; name?: string },
    path: string,
    leaseID: string,
  ): void {
    if (
      resource.name !== azureResourceName(path) ||
      !azureResourceIDEqual(resource.id, this.resourceID(path))
    ) {
      throw new Error(
        `refusing to delete Azure ${kind} ${azureResourceName(path)}: identity does not match lease ${leaseID}`,
      );
    }
  }

  private resourceID(path: string): string {
    return `/subscriptions/${this.subscription}${path}`;
  }

  providerScope(): string {
    return `/subscriptions/${this.subscription}/resourceGroups/${this.resourceGroup}`;
  }

  private async deleteServerOnce(
    name: string,
    resources: AzureOwnedDeleteResources = { vm: true, nic: true, pip: true, disk: true },
    exactNotFound = false,
  ): Promise<{ errors: string[]; retry: boolean }> {
    const result = { errors: [] as string[], retry: false };
    if (resources.vm) {
      await this.deleteResource(
        "vm",
        vmPath(this.resourceGroup, name),
        API_VERSIONS.compute,
        result,
        exactNotFound ? { kind: "virtualMachines", name } : undefined,
      );
    }
    if (resources.nic) {
      await this.deleteResource(
        "nic",
        networkPath(this.resourceGroup, "networkInterfaces", `${name}-nic`),
        API_VERSIONS.network,
        result,
        exactNotFound ? { kind: "networkInterfaces", name: `${name}-nic` } : undefined,
      );
    }
    if (resources.pip) {
      await this.deleteResource(
        "pip",
        networkPath(this.resourceGroup, "publicIPAddresses", `${name}-pip`),
        API_VERSIONS.network,
        result,
        exactNotFound ? { kind: "publicIPAddresses", name: `${name}-pip` } : undefined,
      );
    }
    if (resources.disk) {
      await this.deleteResource(
        "disk",
        `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Compute/disks/${name}-osdisk`,
        API_VERSIONS.disks,
        result,
        exactNotFound ? { kind: "disks", name: `${name}-osdisk` } : undefined,
      );
    }
    return result;
  }

  private async deleteResource(
    kind: string,
    path: string,
    apiVersion: string,
    result: { errors: string[]; retry: boolean },
    exactNotFound?: { kind: string; name: string },
  ): Promise<void> {
    try {
      await this.arm("DELETE", path, apiVersion);
    } catch (error) {
      if (
        exactNotFound
          ? azureResourceNotFound(error, exactNotFound.kind, exactNotFound.name)
          : isNotFound(error)
      ) {
        return;
      }
      result.errors.push(`delete ${kind}: ${errorMessage(error)}`);
      result.retry ||= isRetryableDeleteError(error);
    }
  }

  async ensureSharedInfra(location: string, config: LeaseConfig): Promise<AzureSharedInfraNames> {
    const tags = { crabbox: "true", managed_by: "crabbox" };
    const rg = await this.arm<{ tags?: Record<string, string> }>(
      "GET",
      `/resourceGroups/${this.resourceGroup}`,
      API_VERSIONS.resources,
    ).catch((error) => {
      if (isNotFound(error)) return undefined;
      throw error;
    });
    if (rg) {
      if (rg.tags?.["managed_by"] !== "crabbox") {
        throw new Error(`azure resource group ${this.resourceGroup} is not Crabbox-managed`);
      }
    } else {
      await this.arm(
        "PUT",
        `/resourceGroups/${this.resourceGroup}`,
        API_VERSIONS.resources,
        {
          location,
          tags,
        },
        { lroTimeoutMs: DEFAULT_AZURE_NETWORK_LRO_TIMEOUT_MS },
      );
    }
    const infra = await this.sharedInfraNamesForLocation(location);
    const vnet = await this.arm<{ tags?: Record<string, string>; location?: string }>(
      "GET",
      networkPath(this.resourceGroup, "virtualNetworks", infra.vnet),
      API_VERSIONS.network,
    ).catch((error) => {
      if (isNotFound(error)) return undefined;
      throw error;
    });
    if (vnet) {
      if (vnet.tags?.["managed_by"] !== "crabbox") {
        throw new Error(`azure vnet ${infra.vnet} is not Crabbox-managed`);
      }
      if (!azureSameLocation(vnet.location, location)) {
        throw new Error(
          `azure vnet ${infra.vnet} exists in location ${vnet.location ?? ""}, not ${location}`,
        );
      }
    } else {
      await this.arm(
        "PUT",
        networkPath(this.resourceGroup, "virtualNetworks", infra.vnet),
        API_VERSIONS.network,
        {
          location,
          tags,
          properties: {
            addressSpace: { addressPrefixes: [ADDRESS_SPACE] },
            subnets: [{ name: this.subnet, properties: { addressPrefix: SUBNET_CIDR } }],
          },
        },
        { lroTimeoutMs: DEFAULT_AZURE_NETWORK_LRO_TIMEOUT_MS },
      );
    }
    const nsg = await this.arm<{
      tags?: Record<string, string>;
      location?: string;
      properties?: { securityRules?: AzureSecurityRule[] };
    }>(
      "GET",
      networkPath(this.resourceGroup, "networkSecurityGroups", infra.nsg),
      API_VERSIONS.network,
    ).catch((error) => {
      if (isNotFound(error)) return undefined;
      throw error;
    });
    if (nsg && nsg.tags?.["managed_by"] !== "crabbox") {
      throw new Error(`azure nsg ${infra.nsg} is not Crabbox-managed`);
    }
    if (nsg && !azureSameLocation(nsg.location, location)) {
      throw new Error(
        `azure nsg ${infra.nsg} exists in location ${nsg.location ?? ""}, not ${location}`,
      );
    }
    const preserved = preserveNonCrabboxRules(nsg?.properties?.securityRules ?? []);
    const usedPriorities = usedNSGPriorities(preserved);
    const sshRules = this.buildSSHRules(config, usedPriorities);
    const rules = [...preserved, ...sshRules];
    if (nsg && azureCrabboxSSHRulesMatch(nsg.properties?.securityRules ?? [], sshRules)) {
      return infra;
    }
    await this.arm(
      "PUT",
      networkPath(this.resourceGroup, "networkSecurityGroups", infra.nsg),
      API_VERSIONS.network,
      {
        location,
        tags,
        properties: { securityRules: rules },
      },
      { lroTimeoutMs: DEFAULT_AZURE_NETWORK_LRO_TIMEOUT_MS },
    );
    return infra;
  }

  private async sharedInfraNamesForLocation(location: string): Promise<AzureSharedInfraNames> {
    let mismatch = false;
    const vnet = await this.arm<{ tags?: Record<string, string>; location?: string }>(
      "GET",
      networkPath(this.resourceGroup, "virtualNetworks", this.vnet),
      API_VERSIONS.network,
    ).catch((error) => {
      if (isNotFound(error)) return undefined;
      throw error;
    });
    if (vnet) {
      if (vnet.tags?.["managed_by"] !== "crabbox") {
        throw new Error(`azure vnet ${this.vnet} is not Crabbox-managed`);
      }
      mismatch ||= !azureSameLocation(vnet.location, location);
    }
    const nsg = await this.arm<{ tags?: Record<string, string>; location?: string }>(
      "GET",
      networkPath(this.resourceGroup, "networkSecurityGroups", this.nsg),
      API_VERSIONS.network,
    ).catch((error) => {
      if (isNotFound(error)) return undefined;
      throw error;
    });
    if (nsg) {
      if (nsg.tags?.["managed_by"] !== "crabbox") {
        throw new Error(`azure nsg ${this.nsg} is not Crabbox-managed`);
      }
      mismatch ||= !azureSameLocation(nsg.location, location);
    }
    if (mismatch) {
      return {
        vnet: azureRegionalName(this.vnet, location),
        nsg: azureRegionalName(this.nsg, location),
      };
    }
    return { vnet: this.vnet, nsg: this.nsg };
  }

  private buildSSHRules(config: LeaseConfig, usedPriorities: Set<number>) {
    const ports = sshPorts(config);
    const rules = [];
    for (const port of ports) {
      for (let index = 0; index < this.sshCIDRs.length; index += 1) {
        const priority = nextNSGPriority(usedPriorities);
        rules.push({
          name: `crabbox-ssh-${port}-${index}`,
          properties: {
            priority,
            direction: "Inbound",
            access: "Allow",
            protocol: "Tcp",
            sourceAddressPrefix: this.sshCIDRs[index],
            sourcePortRange: "*",
            destinationAddressPrefix: "*",
            destinationPortRange: port,
          },
        });
      }
    }
    return rules;
  }

  private async createVM(
    config: LeaseConfig,
    location: string,
    leaseID: string,
    slug: string,
    owner: string,
    infra: AzureSharedInfraNames,
    nameSeed = leaseID,
  ): Promise<ProviderMachine> {
    const name = leaseProviderName(nameSeed, slug);
    try {
      return await this.createVMUnchecked(config, location, leaseID, slug, owner, name, infra);
    } catch (error) {
      if (isAzureVMCreateTimeout(error)) {
        await this.deferredCleanup?.({
          name,
          location,
          subscription: this.subscription,
          resourceGroup: this.resourceGroup,
          leaseID,
          slug,
          owner,
          createdAt: new Date().toISOString(),
        });
      } else {
        try {
          await this.deleteOwnedServer({
            id: leaseID,
            slug,
            provider: "azure",
            cloudID: name,
            owner,
            providerScope: this.providerScope(),
          });
        } catch (cleanupError) {
          throw new ProviderProvisioningCleanupError(
            `${errorMessage(error)}; Azure provisioning cleanup failed closed: ${errorMessage(cleanupError)}`,
            {
              provider: "azure",
              cloudID: name,
              region: location,
              providerScope: this.providerScope(),
            },
            cleanupError,
          );
        }
      }
      throw error;
    }
  }

  private async createVMUnchecked(
    config: LeaseConfig,
    location: string,
    leaseID: string,
    slug: string,
    owner: string,
    name: string,
    infra: AzureSharedInfraNames,
  ): Promise<ProviderMachine> {
    const tags = azureTagsFromLabels(
      leaseProviderLabels(config, leaseID, slug, owner, "azure", new Date(), {
        market: config.capacityMarket,
      }),
    );
    await this.arm(
      "PUT",
      networkPath(this.resourceGroup, "publicIPAddresses", `${name}-pip`),
      API_VERSIONS.network,
      {
        location,
        tags,
        sku: { name: "Standard" },
        properties: { publicIPAllocationMethod: "Static" },
      },
      { lroTimeoutMs: DEFAULT_AZURE_NETWORK_LRO_TIMEOUT_MS },
    );
    const subnetID = `/subscriptions/${this.subscription}/resourceGroups/${this.resourceGroup}/providers/Microsoft.Network/virtualNetworks/${infra.vnet}/subnets/${this.subnet}`;
    const nsgID = `/subscriptions/${this.subscription}/resourceGroups/${this.resourceGroup}/providers/Microsoft.Network/networkSecurityGroups/${infra.nsg}`;
    const pipID = `/subscriptions/${this.subscription}/resourceGroups/${this.resourceGroup}/providers/Microsoft.Network/publicIPAddresses/${name}-pip`;
    const nicID = `/subscriptions/${this.subscription}/resourceGroups/${this.resourceGroup}/providers/Microsoft.Network/networkInterfaces/${name}-nic`;
    await this.arm(
      "PUT",
      networkPath(this.resourceGroup, "networkInterfaces", `${name}-nic`),
      API_VERSIONS.network,
      {
        location,
        tags,
        properties: {
          ipConfigurations: [
            {
              name: "ipconfig",
              properties: {
                privateIPAllocationMethod: "Dynamic",
                subnet: { id: subnetID },
                publicIPAddress: { id: pipID },
              },
            },
          ],
          networkSecurityGroup: { id: nsgID },
        },
      },
      { lroTimeoutMs: DEFAULT_AZURE_NETWORK_LRO_TIMEOUT_MS },
    );
    const customData = btoa(
      config.target === "windows"
        ? azureWindowsBootstrapPowerShell(config)
        : azureLinuxCloudInit(config),
    );
    const storageProfile: Record<string, unknown> = {};
    const vmProperties: Record<string, unknown> = {
      hardwareProfile: { vmSize: config.serverType },
      storageProfile,
      networkProfile: { networkInterfaces: [{ id: nicID }] },
    };
    if (config.azureSnapshot) {
      const diskID = await this.createDiskFromSnapshot(
        config.azureSnapshot,
        `${name}-osdisk`,
        location,
        tags,
      );
      storageProfile["osDisk"] = {
        createOption: "Attach",
        managedDisk: { id: diskID },
        osType: config.target === "windows" ? "Windows" : "Linux",
        caching: "ReadWrite",
      };
    } else {
      const image = azureImageReference(this.resolvedImageForConfig(config));
      const osDisk: Record<string, unknown> = {
        name: `${name}-osdisk`,
        createOption: "FromImage",
      };
      if (await this.useEphemeralOSDisk(config, location)) {
        osDisk["caching"] = "ReadOnly";
        const diffDiskSettings: Record<string, unknown> = { option: "Local" };
        if (azureOSDiskUsesFullCaching(config.azureOSDisk)) {
          diffDiskSettings["enableFullCaching"] = true;
          osDisk["managedDisk"] = { storageAccountType: "StandardSSD_LRS" };
        }
        osDisk["diffDiskSettings"] = diffDiskSettings;
      } else {
        osDisk["caching"] = "ReadWrite";
        osDisk["managedDisk"] = { storageAccountType: "StandardSSD_LRS" };
      }
      storageProfile["imageReference"] = image;
      storageProfile["osDisk"] = osDisk;
      vmProperties["osProfile"] = this.osProfile(config, name, leaseID, customData);
    }
    if (config.capacityMarket === "spot") {
      vmProperties["priority"] = "Spot";
      vmProperties["evictionPolicy"] = "Delete";
      vmProperties["billingProfile"] = { maxPrice: -1 };
    }
    const vmLROTimeoutMs = azureVMCreateTimeoutMs(config);
    await this.arm(
      "PUT",
      vmPath(this.resourceGroup, name),
      azureComputeAPIVersionForOSDisk(config.azureSnapshot ? "managed" : config.azureOSDisk),
      {
        location,
        tags,
        properties: vmProperties,
      },
      vmLROTimeoutMs === undefined ? undefined : { lroTimeoutMs: vmLROTimeoutMs },
    );
    const configuredOSDisk = storageProfile["osDisk"] as
      | { diffDiskSettings?: { option?: string } }
      | undefined;
    if (!configuredOSDisk?.diffDiskSettings?.option) {
      await this.arm(
        "PATCH",
        `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Compute/disks/${name}-osdisk`,
        API_VERSIONS.disks,
        { tags },
      );
    }
    if (config.azureSnapshot && config.target !== "windows") {
      await this.installLinuxSSHKeyExtension(location, name, tags, config);
    }
    if (config.target === "windows") {
      await this.installWindowsBootstrapExtension(location, name, tags);
    }
    const ip = await this.publicIP(`${name}-pip`);
    const vm = await this.arm<AzureVM>(
      "GET",
      vmPath(this.resourceGroup, name),
      API_VERSIONS.compute,
    );
    return toMachine(vm, ip);
  }

  resolvedImageForConfig(config: LeaseConfig): string {
    const image = config.azureImage || this.image;
    if (
      config.target === "linux" &&
      config.architecture === "arm64" &&
      isAzureDefaultLinuxImage(image)
    ) {
      return config.os === "ubuntu:24.04"
        ? AZURE_NOBLE_LINUX_ARM64_IMAGE
        : DEFAULT_AZURE_LINUX_ARM64_IMAGE;
    }
    if (config.target === "windows" && isAzureDefaultLinuxImage(image)) {
      return DEFAULT_AZURE_WINDOWS_IMAGE;
    }
    return image;
  }

  private osProfile(
    config: LeaseConfig,
    name: string,
    leaseID: string,
    customData: string,
  ): Record<string, unknown> {
    if (config.target !== "windows") {
      return {
        computerName: name,
        adminUsername: config.sshUser,
        customData,
        linuxConfiguration: {
          disablePasswordAuthentication: true,
          ssh: {
            publicKeys: [
              {
                path: `/home/${config.sshUser}/.ssh/authorized_keys`,
                keyData: config.sshPublicKey,
              },
            ],
          },
        },
      };
    }
    return {
      computerName: azureComputerName(name, leaseID, config.target),
      adminUsername: "crabadmin",
      adminPassword: azureRandomAdminPassword(),
      allowExtensionOperations: true,
      customData,
      windowsConfiguration: {
        provisionVMAgent: true,
        enableAutomaticUpdates: false,
      },
    };
  }

  private async installWindowsBootstrapExtension(
    location: string,
    vmName: string,
    tags: Record<string, string>,
  ): Promise<void> {
    await this.arm(
      "PUT",
      `${vmPath(this.resourceGroup, vmName)}/extensions/crabbox-bootstrap`,
      API_VERSIONS.compute,
      {
        location,
        tags,
        properties: {
          publisher: "Microsoft.Compute",
          type: "CustomScriptExtension",
          typeHandlerVersion: "1.10",
          autoUpgradeMinorVersion: true,
          settings: { timestamp: Math.trunc(Date.now() / 1000) },
          protectedSettings: {
            commandToExecute: azureWindowsBootstrapCommand(),
          },
        },
      },
      {
        terminalResourceState: {
          path: `${vmPath(this.resourceGroup, vmName)}/extensions/crabbox-bootstrap`,
          apiVersion: API_VERSIONS.compute,
        },
      },
    );
  }

  private async installLinuxSSHKeyExtension(
    location: string,
    vmName: string,
    tags: Record<string, string>,
    config: LeaseConfig,
  ): Promise<void> {
    const user = shellQuote(config.sshUser || "crabbox");
    const key = shellQuote(config.sshPublicKey);
    const command = [
      "set -eu",
      `user=${user}`,
      `key=${key}`,
      `if ! id "$user" >/dev/null 2>&1; then useradd -m -s /bin/bash "$user"; fi`,
      `home=$(getent passwd "$user" | cut -d: -f6)`,
      `install -d -m 700 -o "$user" -g "$user" "$home/.ssh"`,
      `printf '%s\\n' "$key" > "$home/.ssh/authorized_keys"`,
      `chown "$user:$user" "$home/.ssh/authorized_keys"`,
      `chmod 600 "$home/.ssh/authorized_keys"`,
      `if command -v cloud-init >/dev/null 2>&1; then cloud-init clean --logs || true; fi`,
    ].join("; ");
    await this.arm(
      "PUT",
      `${vmPath(this.resourceGroup, vmName)}/extensions/crabbox-bootstrap`,
      API_VERSIONS.compute,
      {
        location,
        tags,
        properties: {
          publisher: "Microsoft.Azure.Extensions",
          type: "CustomScript",
          typeHandlerVersion: "2.1",
          autoUpgradeMinorVersion: true,
          settings: { timestamp: Math.trunc(Date.now() / 1000) },
          protectedSettings: {
            commandToExecute: `/bin/sh -c ${shellQuote(command)}`,
          },
        },
      },
    );
  }

  async createDiskSnapshot(vmName: string, name: string): Promise<ProviderImage> {
    const vm = await this.arm<AzureVM>(
      "GET",
      vmPath(this.resourceGroup, vmName),
      API_VERSIONS.compute,
    );
    const osDisk = vm.properties?.storageProfile?.osDisk;
    // Azure ARM accepts snapshot requests against the phantom managed-disk identity of
    // an ephemeral OS disk and reports Succeeded, but the resulting snapshot captures
    // the base image rather than live state - any fork silently loses the source workdir.
    // Azure documents "Local" as the only diffDiskSettings.option value; treat unknown
    // values as snapshottable so a schema addition does not break managed-disk leases.
    if (osDisk?.diffDiskSettings?.option === "Local") {
      throw new Error(
        `azure ephemeral OS disk on vm ${vmName} cannot be snapshotted; ` +
          `use --mode archive or relaunch the lease with a managed Azure OS disk`,
      );
    }
    const sourceDiskID = osDisk?.managedDisk?.id;
    if (!sourceDiskID) {
      throw new Error(`azure os disk not found for vm ${vmName}`);
    }
    const location = vm.location || this.defaultLocation;
    const snapshot = await this.arm<AzureSnapshot>(
      "PUT",
      azureSnapshotPath(this.resourceGroup, name),
      API_VERSIONS.disks,
      {
        location,
        tags: { crabbox: "true", managed_by: "crabbox" },
        properties: {
          creationData: {
            createOption: "Copy",
            sourceResourceId: sourceDiskID,
          },
        },
      },
    );
    return azureSnapshotProviderImage(snapshot, name, location);
  }

  async getImage(name: string, kind?: string): Promise<ProviderImage> {
    if (kind === "azure-os-disk-snapshot") {
      return await this.getDiskSnapshot(name);
    }
    const imageName = azureResourceName(name);
    if (kind === "azure-managed-image") {
      const image = await this.arm<AzureManagedImage>(
        "GET",
        azureImagePath(this.resourceGroup, imageName),
        API_VERSIONS.compute,
      );
      return azureProviderImage(image, imageName, image.location || this.defaultLocation);
    }
    const image = await this.arm<AzureManagedImage>(
      "GET",
      azureImagePath(this.resourceGroup, imageName),
      API_VERSIONS.compute,
    ).catch((error) => {
      if (isNotFound(error)) return undefined;
      throw error;
    });
    if (!image) return await this.getDiskSnapshot(name);
    return azureProviderImage(image, imageName, image.location || this.defaultLocation);
  }

  async deleteImage(name: string, kind?: string): Promise<void> {
    if (kind === "azure-os-disk-snapshot") {
      await this.deleteDiskSnapshot(name);
      return;
    }
    const imageName = azureResourceName(name);
    if (kind === "azure-managed-image") {
      await this.arm(
        "DELETE",
        azureImagePath(this.resourceGroup, imageName),
        API_VERSIONS.compute,
      ).catch((error) => {
        if (isNotFound(error)) return undefined;
        throw error;
      });
      return;
    }
    const image = await this.arm(
      "DELETE",
      azureImagePath(this.resourceGroup, imageName),
      API_VERSIONS.compute,
    ).catch((error) => {
      if (isNotFound(error)) return "not-found";
      throw error;
    });
    if (image !== "not-found") return;
    await this.deleteDiskSnapshot(name);
  }

  private async getDiskSnapshot(name: string): Promise<ProviderImage> {
    const snapshot = await this.arm<AzureSnapshot>(
      "GET",
      azureSnapshotPath(this.resourceGroup, azureResourceName(name)),
      API_VERSIONS.disks,
    );
    return azureSnapshotProviderImage(
      snapshot,
      azureResourceName(name),
      snapshot.location || this.defaultLocation,
    );
  }

  private async deleteDiskSnapshot(name: string): Promise<void> {
    await this.arm(
      "DELETE",
      azureSnapshotPath(this.resourceGroup, azureResourceName(name)),
      API_VERSIONS.disks,
    ).catch((error) => {
      if (isNotFound(error)) return undefined;
      throw error;
    });
  }

  private async createDiskFromSnapshot(
    snapshotID: string,
    diskName: string,
    location: string,
    tags: Record<string, string>,
  ): Promise<string> {
    const sourceResourceId = snapshotID.startsWith("/subscriptions/")
      ? snapshotID
      : `/subscriptions/${this.subscription}${azureSnapshotPath(this.resourceGroup, snapshotID)}`;
    const disk = await this.arm<{ id?: string }>(
      "PUT",
      `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Compute/disks/${diskName}`,
      API_VERSIONS.disks,
      {
        location,
        tags,
        properties: {
          creationData: {
            createOption: "Copy",
            sourceResourceId,
          },
        },
      },
    );
    return (
      disk.id ??
      `/subscriptions/${this.subscription}/resourceGroups/${this.resourceGroup}/providers/Microsoft.Compute/disks/${diskName}`
    );
  }

  private async publicIP(name: string): Promise<string> {
    const deadline = Date.now() + 60_000;
    while (Date.now() < deadline) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- public IP polling must wait between Azure reads.
      const pip = await this.arm<AzurePublicIP>(
        "GET",
        networkPath(this.resourceGroup, "publicIPAddresses", name),
        API_VERSIONS.network,
      );
      if (pip.properties?.ipAddress) return pip.properties.ipAddress;
      // oxlint-disable-next-line eslint/no-await-in-loop -- this delay is the polling interval.
      await sleep(2_000);
    }
    throw new Error(`timed out waiting for public ip: ${name}`);
  }

  private async arm<T>(
    method: string,
    path: string,
    apiVersion: string,
    body?: unknown,
    opts?: AzureARMOptions,
  ): Promise<T> {
    const token = await this.token();
    const url = `https://management.azure.com/subscriptions/${this.subscription}${path}?api-version=${apiVersion}`;
    const init: RequestInit = {
      method,
      headers: {
        authorization: `Bearer ${token}`,
        "content-type": "application/json",
      },
    };
    if (body !== undefined) init.body = JSON.stringify(body);
    const response = await this.fetcher(url, init);
    if (!response.ok && response.status !== 201 && response.status !== 202) {
      throw new AzureHTTPError(method, path, response.status, await safeBody(response));
    }
    const initialText = await response.text();
    if (response.status === 201 || response.status === 202) {
      await this.awaitLRO(response, token, opts);
      if (method === "DELETE") return undefined as T;
      // 201 typically returns the resource in the initial body; 202 returns nothing,
      // so re-GET the resource to read its post-provision state.
      if (initialText) return JSON.parse(initialText) as T;
      const refetch = await this.fetcher(url, {
        headers: { authorization: `Bearer ${token}` },
      });
      if (!refetch.ok) {
        throw new Error(
          `azure ${method} ${path}: refetch http ${refetch.status}: ${await safeBody(refetch)}`,
        );
      }
      const refetchText = await refetch.text();
      return refetchText ? (JSON.parse(refetchText) as T) : (undefined as T);
    }
    if (response.status === 204) return undefined as T;
    return initialText ? (JSON.parse(initialText) as T) : (undefined as T);
  }

  private async supportsEphemeralOS(vmSize: string, location: string): Promise<boolean> {
    if (!this.ephemeralOSSupport) {
      try {
        this.ephemeralOSSupport = await this.loadEphemeralOSSupport(location);
      } catch {
        return azureSupportsEphemeralOS(vmSize);
      }
    }
    return this.ephemeralOSSupport.get(vmSize) ?? azureSupportsEphemeralOS(vmSize);
  }

  private async useEphemeralOSDisk(config: LeaseConfig, location: string): Promise<boolean> {
    return await this.validateOSDiskMode(config, location);
  }

  private async validateOSDiskMode(config: LeaseConfig, location: string): Promise<boolean> {
    const mode = config.azureOSDisk;
    if (!azureOSDiskIsEphemeral(mode)) return false;
    const supported = await this.supportsEphemeralOS(config.serverType, location);
    if (!supported) {
      throw new Error(
        `azureOSDisk=${mode} requires an Azure VM size with ephemeral OS disk support; ${config.serverType} is not supported`,
      );
    }
    if (azureOSDiskUsesFullCaching(mode) && !azureSupportsEphemeralFullCaching(config.serverType)) {
      throw new Error(
        `azureOSDisk=ephemeral-preview requires a full-caching preview Azure VM size; ${config.serverType} is not supported because preview full caching requires more than 4 vCPUs and local storage larger than 2x the OS disk plus 1 GiB`,
      );
    }
    return supported;
  }

  private async loadEphemeralOSSupport(location: string): Promise<Map<string, boolean>> {
    const token = await this.token();
    const url = new URL(
      `https://management.azure.com/subscriptions/${this.subscription}/providers/Microsoft.Compute/skus`,
    );
    url.searchParams.set("api-version", API_VERSIONS.compute);
    url.searchParams.set("$filter", `location eq '${location}'`);
    const response = await this.fetcher(url.toString(), {
      headers: { authorization: `Bearer ${token}` },
    });
    if (!response.ok) {
      throw new Error(
        `azure GET resource skus: http ${response.status}: ${await safeBody(response)}`,
      );
    }
    const json = (await response.json()) as { value?: AzureSKU[] };
    const support = new Map<string, boolean>();
    for (const sku of json.value ?? []) {
      if (!sku.name || sku.resourceType !== "virtualMachines") continue;
      support.set(sku.name, azureSKUCapabilityTrue(sku.capabilities, "EphemeralOSDiskSupported"));
    }
    return support;
  }

  private async awaitLRO(response: Response, token: string, opts?: AzureARMOptions): Promise<void> {
    const asyncURL =
      response.headers.get("azure-asyncoperation") ?? response.headers.get("location");
    if (!asyncURL) return;
    const interval = azureLROPollIntervalMS(response.headers.get("retry-after"));
    const timeoutMs = opts?.lroTimeoutMs;
    const lroTimeoutMs = timeoutMs && timeoutMs > 0 ? timeoutMs : 20 * 60_000;
    const deadline = Date.now() + lroTimeoutMs;
    for (;;) {
      const remainingMs = deadline - Date.now();
      if (remainingMs <= 0) break;
      // oxlint-disable-next-line eslint/no-await-in-loop -- LRO must wait between status reads.
      await sleep(Math.min(interval, remainingMs));
      if (Date.now() >= deadline) break;
      // oxlint-disable-next-line eslint/no-await-in-loop -- LRO polling is sequential.
      const poll = await this.fetcher(asyncURL, {
        headers: { authorization: `Bearer ${token}` },
      });
      if (!poll.ok) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- only reached on error to format diagnostic.
        const detail = await safeBody(poll);
        throw new Error(`azure LRO poll: http ${poll.status}: ${detail}`);
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- reading the LRO status payload is part of polling.
      const text = await poll.text();
      const status = text ? (JSON.parse(text) as { status?: string }).status?.toLowerCase() : "";
      if (status === "succeeded") return;
      if (status === "failed" || status === "canceled") {
        throw new Error(`azure LRO ${status}: ${text}`);
      }
      if (opts?.terminalResourceState) {
        // Azure can leave the extension LRO pending after the resource itself is terminal.
        // Match the direct CLI by accepting the resource state as the completion signal.
        // oxlint-disable-next-line eslint/no-await-in-loop -- resource state follows each pending LRO poll.
        const resourceState = await this.resourceProvisioningState(
          token,
          opts.terminalResourceState,
        );
        if (resourceState === "succeeded") return;
        if (resourceState === "failed" || resourceState === "canceled") {
          throw new Error(`azure resource reached ${resourceState}`);
        }
      }
    }
    throw new Error(
      timeoutMs && timeoutMs > 0
        ? `azure long-running operation timed out after ${Math.round(timeoutMs / 1000)}s`
        : "azure long-running operation timed out",
    );
  }

  private async resourceProvisioningState(
    token: string,
    resource: { path: string; apiVersion: string },
  ): Promise<string> {
    const url = `https://management.azure.com/subscriptions/${this.subscription}${resource.path}?api-version=${resource.apiVersion}`;
    const response = await this.fetcher(url, {
      headers: { authorization: `Bearer ${token}` },
    });
    if (!response.ok) return "";
    const body = (await response.json()) as { properties?: { provisioningState?: string } };
    return body.properties?.provisioningState?.toLowerCase() ?? "";
  }

  private async token(): Promise<string> {
    if (this.cache && this.cache.expiresAt > Date.now() + 30_000) return this.cache.token;
    const body = new URLSearchParams({
      grant_type: "client_credentials",
      client_id: this.clientID,
      client_secret: this.secret,
      scope: "https://management.azure.com/.default",
    });
    const response = await this.fetcher(
      `https://login.microsoftonline.com/${this.tenant}/oauth2/v2.0/token`,
      {
        method: "POST",
        headers: { "content-type": "application/x-www-form-urlencoded" },
        body: body.toString(),
      },
    );
    if (!response.ok) {
      throw new Error(`azure token: http ${response.status}: ${await safeBody(response)}`);
    }
    const json = (await response.json()) as { access_token?: string; expires_in?: number };
    if (!json.access_token) throw new Error("azure token response missing access_token");
    this.cache = {
      token: json.access_token,
      expiresAt: Date.now() + (json.expires_in ?? 3600) * 1000,
    };
    return this.cache.token;
  }
}

function azureWindowsBootstrapCommand(): string {
  return `powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$p=Join-Path $env:SystemDrive 'AzureData\\CustomData.bin'; $d=Join-Path $env:SystemDrive 'AzureData\\crabbox-bootstrap.ps1'; Copy-Item -Force $p $d; & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $d"`;
}

function azureRandomAdminPassword(): string {
  const bytes = new Uint8Array(18);
  crypto.getRandomValues(bytes);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return `Cb1!${btoa(binary).slice(0, 18)}`;
}

function azureComputerName(vmName: string, leaseID: string, target: string): string {
  if (target !== "windows") return vmName;
  const suffix = (leaseID || vmName)
    .toLowerCase()
    .replace(/[^a-z0-9]/g, "")
    .slice(0, 12);
  return `cbx${suffix || "windows"}`;
}

function vmPath(rg: string, name: string): string {
  return `/resourceGroups/${rg}/providers/Microsoft.Compute/virtualMachines/${name}`;
}

function networkPath(rg: string, kind: string, name: string): string {
  return `/resourceGroups/${rg}/providers/Microsoft.Network/${kind}/${name}`;
}

function azureImageReference(value: string):
  | { id: string }
  | {
      publisher: string;
      offer: string;
      sku: string;
      version: string;
    } {
  if (value.startsWith("/subscriptions/")) {
    return { id: value };
  }
  return parseImageRef(value);
}

function parseImageRef(value: string): {
  publisher: string;
  offer: string;
  sku: string;
  version: string;
} {
  const parts = value.split(":");
  if (parts.length !== 4) {
    throw new Error(`azure image must be Publisher:Offer:SKU:Version, got ${value}`);
  }
  return { publisher: parts[0]!, offer: parts[1]!, sku: parts[2]!, version: parts[3]! };
}

function azureImagePath(rg: string, name: string): string {
  return `/resourceGroups/${rg}/providers/Microsoft.Compute/images/${name}`;
}

function azureSnapshotPath(rg: string, name: string): string {
  return `/resourceGroups/${rg}/providers/Microsoft.Compute/snapshots/${name}`;
}

function azureResourceName(value: string): string {
  return value.slice(value.lastIndexOf("/") + 1);
}

function azureResourceSetCloudID(
  kind: AzureLeaseResource["kind"],
  name: string | undefined,
): string | undefined {
  const suffix =
    kind === "networkInterfaces"
      ? "-nic"
      : kind === "publicIPAddresses"
        ? "-pip"
        : kind === "disks"
          ? "-osdisk"
          : "";
  const resourceName = name?.trim() ?? "";
  if (!resourceName) {
    return undefined;
  }
  if (!suffix) {
    return resourceName;
  }
  return resourceName.endsWith(suffix)
    ? resourceName.slice(0, -suffix.length) || undefined
    : undefined;
}

function azureRecoveryCloudID(
  kind: AzureLeaseResource["kind"],
  name: string | undefined,
  leaseID: string,
): string {
  const cloudID = azureResourceSetCloudID(kind, name);
  if (!cloudID) {
    throw new Error(
      `refusing to recover Azure lease ${leaseID}: invalid ${kind} resource identity`,
    );
  }
  return cloudID;
}

const azureReconciliationLabelKeys = [
  "crabbox",
  "created_by",
  "lease",
  "owner",
  "provider",
  "provider_key",
  "slug",
  "keep",
  "created_at",
  "expires_at",
] as const;

function azureLeaseResources(inventory: AzureLeaseResourceInventory): AzureLeaseResource[] {
  return [
    ...inventory.vms.map((resource) => ({ kind: "virtualMachines" as const, resource })),
    ...inventory.nics.map((resource) => ({ kind: "networkInterfaces" as const, resource })),
    ...inventory.pips.map((resource) => ({ kind: "publicIPAddresses" as const, resource })),
    ...inventory.disks.map((resource) => ({ kind: "disks" as const, resource })),
  ];
}

function azureReconciliationLabelIdentity(resource: AzureLeaseResource["resource"]): string {
  const labels = azureLabelsFromTags(resource.tags ?? {});
  return JSON.stringify(
    Object.fromEntries(azureReconciliationLabelKeys.map((key) => [key, labels[key] ?? ""])),
  );
}

function azureReconciliationImmutableIdentity(candidate: AzureLeaseResource): string {
  if (candidate.kind === "virtualMachines") {
    return candidate.resource.properties?.vmId ?? "";
  }
  if (candidate.kind === "networkInterfaces") {
    return candidate.resource.properties?.resourceGuid ?? "";
  }
  if (candidate.kind === "publicIPAddresses") {
    return candidate.resource.properties?.resourceGuid ?? "";
  }
  return candidate.resource.properties?.uniqueId ?? "";
}

function azureReconciliationMemberHasStableIdentity(candidate: AzureLeaseResource): boolean {
  const resourceID = candidate.resource.id?.trim();
  const resourceName = candidate.resource.name?.trim();
  const immutableIdentity = azureReconciliationImmutableIdentity(candidate).trim();
  const location = candidate.resource.location?.trim();
  return Boolean(
    resourceID &&
    resourceName &&
    azureResourceName(resourceID).toLowerCase() === resourceName.toLowerCase() &&
    immutableIdentity &&
    location,
  );
}

function azureResourceIDsEqual(left: string | undefined, right: string | undefined): boolean {
  const normalizedLeft = azureNormalizedResourceID(left);
  const normalizedRight = azureNormalizedResourceID(right);
  return Boolean(normalizedLeft && normalizedRight && normalizedLeft === normalizedRight);
}

function azureDiskAttachmentIDs(disk: AzureDisk): string[] {
  return [
    ...new Set(
      [...(disk.managedBy ? [disk.managedBy] : []), ...(disk.managedByExtended ?? [])].map(
        (attachment) => attachment.trim().toLowerCase(),
      ),
    ),
  ].toSorted();
}

function azureDiskAttachmentsMatchVM(
  disk: AzureDisk,
  expectedVMID: string,
  requireAttachment = false,
): boolean {
  const attachments = [
    ...(disk.managedBy ? [disk.managedBy] : []),
    ...(disk.managedByExtended ?? []),
  ];
  return (
    (!requireAttachment || attachments.length > 0) &&
    attachments.every((attachment) => azureResourceIDsEqual(attachment, expectedVMID))
  );
}

function azureVMReferencesOnlyNIC(vm: AzureVM, expectedNICID: string): boolean {
  return (vm.properties?.networkProfile?.networkInterfaces ?? []).every((networkInterface) =>
    azureResourceIDsEqual(networkInterface.id, expectedNICID),
  );
}

function azureVMHasDataDisks(vm: AzureVM): boolean {
  return (vm.properties?.storageProfile?.dataDisks ?? []).length > 0;
}

function azureVMUsesEphemeralOSDisk(vm: AzureVM | undefined): boolean {
  return (
    vm?.properties?.storageProfile?.osDisk?.diffDiskSettings?.option?.trim().toLowerCase() ===
    "local"
  );
}

function azureDeleteOptionIsSafe(value: string | undefined): boolean {
  const normalized = value?.trim().toLowerCase();
  return !normalized || normalized === "detach";
}

function azureVMHasCascadeDelete(vm: AzureVM): boolean {
  const osDisk = vm.properties?.storageProfile?.osDisk;
  if (!azureVMUsesEphemeralOSDisk(vm) && !azureDeleteOptionIsSafe(osDisk?.deleteOption)) {
    return true;
  }
  return (vm.properties?.networkProfile?.networkInterfaces ?? []).some(
    (networkInterface) =>
      !azureDeleteOptionIsSafe(
        networkInterface.deleteOption ?? networkInterface.properties?.deleteOption,
      ),
  );
}

function azureNICHasCascadeDelete(nic: AzureNIC): boolean {
  return (nic.properties?.ipConfigurations ?? []).some(
    (configuration) =>
      !azureDeleteOptionIsSafe(configuration.properties?.publicIPAddress?.deleteOption),
  );
}

function azureResourceReferenceIdentity(
  reference: AzureResourceReference | null | undefined,
  label: string,
): string {
  return azureNormalizedResourceID(reference?.id) || `${label}:attached`;
}

function azureResourceReferenceListIdentity(
  references: AzureResourceReference[] | undefined,
  label: string,
): string[] {
  return (references ?? [])
    .map((reference, index) => azureResourceReferenceIdentity(reference, `${label}:${index}`))
    .toSorted();
}

function azureNICServiceAssociationTopology(nic: AzureNIC): Record<string, string[]> {
  const properties = nic.properties;
  const ipConfigurations = properties?.ipConfigurations ?? [];
  return {
    privateEndpointIDs: properties?.privateEndpoint
      ? [azureResourceReferenceIdentity(properties.privateEndpoint, "privateEndpoint")]
      : [],
    privateLinkServiceIDs: properties?.privateLinkService
      ? [azureResourceReferenceIdentity(properties.privateLinkService, "privateLinkService")]
      : [],
    hostedWorkloadIDs: (properties?.hostedWorkloads ?? [])
      .map((workload, index) => workload.trim().toLowerCase() || `hostedWorkload:${index}`)
      .toSorted(),
    tapConfigurationIDs: azureResourceReferenceListIdentity(
      properties?.tapConfigurations,
      "tapConfiguration",
    ),
    loadBalancerBackendAddressPoolIDs: ipConfigurations
      .flatMap((configuration) =>
        azureResourceReferenceListIdentity(
          configuration.properties?.loadBalancerBackendAddressPools,
          "loadBalancerBackendAddressPool",
        ),
      )
      .toSorted(),
    applicationGatewayBackendAddressPoolIDs: ipConfigurations
      .flatMap((configuration) =>
        azureResourceReferenceListIdentity(
          configuration.properties?.applicationGatewayBackendAddressPools,
          "applicationGatewayBackendAddressPool",
        ),
      )
      .toSorted(),
    applicationSecurityGroupIDs: ipConfigurations
      .flatMap((configuration) =>
        azureResourceReferenceListIdentity(
          configuration.properties?.applicationSecurityGroups,
          "applicationSecurityGroup",
        ),
      )
      .toSorted(),
    loadBalancerInboundNatRuleIDs: ipConfigurations
      .flatMap((configuration) =>
        azureResourceReferenceListIdentity(
          configuration.properties?.loadBalancerInboundNatRules,
          "loadBalancerInboundNatRule",
        ),
      )
      .toSorted(),
    privateLinkConnectionIDs: ipConfigurations
      .flatMap((configuration, index) =>
        configuration.properties?.privateLinkConnectionProperties
          ? [`privateLinkConnection:${index}:attached`]
          : [],
      )
      .toSorted(),
    virtualNetworkTapIDs: ipConfigurations
      .flatMap((configuration) =>
        azureResourceReferenceListIdentity(
          configuration.properties?.virtualNetworkTaps,
          "virtualNetworkTap",
        ),
      )
      .toSorted(),
    gatewayLoadBalancerIDs: ipConfigurations
      .flatMap((configuration, index) =>
        configuration.properties?.gatewayLoadBalancer
          ? [
              azureResourceReferenceIdentity(
                configuration.properties.gatewayLoadBalancer,
                `gatewayLoadBalancer:${index}`,
              ),
            ]
          : [],
      )
      .toSorted(),
  };
}

function azureNICHasDisqualifyingServiceAssociation(nic: AzureNIC): boolean {
  return Object.values(azureNICServiceAssociationTopology(nic)).some(
    (associations) => associations.length > 0,
  );
}

function azureNICReferencesOnlyPIP(
  nic: AzureNIC,
  expectedPIPID: string,
  canonicalPIPPresent: boolean,
): boolean {
  if (azureNICHasDisqualifyingServiceAssociation(nic)) return false;
  const publicIPReferences = (nic.properties?.ipConfigurations ?? []).flatMap((configuration) => {
    const publicIPAddress = configuration.properties?.publicIPAddress;
    return publicIPAddress ? [publicIPAddress.id] : [];
  });
  return (
    (canonicalPIPPresent || publicIPReferences.length === 0) &&
    publicIPReferences.every((id) => azureResourceIDsEqual(id, expectedPIPID))
  );
}

function azurePublicIPAttachmentMatchesNIC(
  pip: AzurePublicIP,
  nic: AzureNIC | undefined,
  expectedPIPID: string | undefined,
  expectedNICID?: string,
  allowMissingNIC = false,
): boolean {
  if (
    pip.properties?.natGateway ||
    pip.properties?.linkedPublicIPAddress ||
    pip.properties?.servicePublicIPAddress
  ) {
    return false;
  }
  const ipConfiguration = pip.properties?.ipConfiguration;
  if (!ipConfiguration) {
    return true;
  }
  if (!nic && allowMissingNIC) {
    const normalizedIPConfigurationID = azureNormalizedResourceID(ipConfiguration.id);
    const normalizedNICID = azureNormalizedResourceID(expectedNICID);
    return Boolean(
      normalizedIPConfigurationID &&
      normalizedNICID &&
      normalizedIPConfigurationID.startsWith(`${normalizedNICID}/`),
    );
  }
  return Boolean(
    nic &&
    expectedPIPID &&
    (nic.properties?.ipConfigurations ?? []).some(
      (configuration) =>
        azureResourceIDsEqual(configuration.id, ipConfiguration.id) &&
        azureResourceIDsEqual(configuration.properties?.publicIPAddress?.id, expectedPIPID),
    ),
  );
}

function azureReconciliationTopologyConsistent(set: AzureReconciliationResourceSet): boolean {
  if (!set.nic || !set.pip || !set.disk) {
    return false;
  }
  const nicVirtualMachine = set.nic?.properties?.virtualMachine;
  if (
    nicVirtualMachine &&
    (!set.vm || !azureResourceIDsEqual(nicVirtualMachine.id, set.canonicalVMID))
  ) {
    return false;
  }
  if (set.vm && !azureResourceIDsEqual(set.vm.id, set.canonicalVMID)) {
    return false;
  }
  if (set.vm && azureVMHasDataDisks(set.vm)) {
    return false;
  }
  if (set.vm && azureVMHasCascadeDelete(set.vm)) {
    return false;
  }
  if (set.nic && azureNICHasCascadeDelete(set.nic)) {
    return false;
  }
  if (set.vm && !azureVMReferencesOnlyNIC(set.vm, set.canonicalNICID)) {
    return false;
  }
  if (set.vm && !set.nic) {
    return false;
  }
  if (
    set.vm &&
    set.nic &&
    !(set.vm.properties?.networkProfile?.networkInterfaces ?? []).some((networkInterface) =>
      azureResourceIDsEqual(networkInterface.id, set.canonicalNICID),
    )
  ) {
    return false;
  }
  if (set.nic && !azureNICReferencesOnlyPIP(set.nic, set.canonicalPIPID, Boolean(set.pip))) {
    return false;
  }
  if (
    set.nic &&
    set.pip &&
    !(set.nic.properties?.ipConfigurations ?? []).some((configuration) =>
      azureResourceIDsEqual(configuration.properties?.publicIPAddress?.id, set.canonicalPIPID),
    )
  ) {
    return false;
  }
  if (set.pip && !azurePublicIPAttachmentMatchesNIC(set.pip, set.nic, set.canonicalPIPID)) {
    return false;
  }
  if (set.disk && !azureDiskAttachmentsMatchVM(set.disk, set.canonicalVMID, Boolean(set.vm))) {
    return false;
  }
  if (
    set.vm &&
    set.disk &&
    (!azureResourceIDsEqual(
      set.vm.properties?.storageProfile?.osDisk?.managedDisk?.id,
      set.disk.id,
    ) ||
      !azureDiskAttachmentsMatchVM(set.disk, set.canonicalVMID, true))
  ) {
    return false;
  }
  return true;
}

function azureReconciliationTopologyIdentity(
  candidate: AzureLeaseResource,
): Record<string, string | string[]> {
  if (candidate.kind === "virtualMachines") {
    return {
      managedDiskID:
        candidate.resource.properties?.storageProfile?.osDisk?.managedDisk?.id?.toLowerCase() ?? "",
      osDiskDiffOption:
        candidate.resource.properties?.storageProfile?.osDisk?.diffDiskSettings?.option
          ?.trim()
          .toLowerCase() ?? "",
      osDiskDeleteOption:
        candidate.resource.properties?.storageProfile?.osDisk?.deleteOption?.trim().toLowerCase() ??
        "",
      dataDiskIDs: (candidate.resource.properties?.storageProfile?.dataDisks ?? [])
        .map(
          (disk, index) =>
            azureNormalizedResourceID(disk.managedDisk?.id) || `dataDisk:${index}:attached`,
        )
        .toSorted(),
      dataDiskDeleteOptions: (candidate.resource.properties?.storageProfile?.dataDisks ?? [])
        .map((disk, index) => `${index}:${disk.deleteOption?.trim().toLowerCase() || "default"}`)
        .toSorted(),
      networkInterfaceIDs: (candidate.resource.properties?.networkProfile?.networkInterfaces ?? [])
        .flatMap((networkInterface) =>
          networkInterface.id ? [networkInterface.id.toLowerCase()] : [],
        )
        .toSorted(),
      networkInterfaceDeleteOptions: (
        candidate.resource.properties?.networkProfile?.networkInterfaces ?? []
      )
        .flatMap((networkInterface, index) => {
          const value = (networkInterface.deleteOption ?? networkInterface.properties?.deleteOption)
            ?.trim()
            .toLowerCase();
          return value ? [`${index}:${value}`] : [];
        })
        .toSorted(),
    };
  }
  if (candidate.kind === "networkInterfaces") {
    return {
      virtualMachineID: candidate.resource.properties?.virtualMachine?.id?.toLowerCase() ?? "",
      publicIPIDs: (candidate.resource.properties?.ipConfigurations ?? [])
        .flatMap((configuration) => {
          const id = configuration.properties?.publicIPAddress?.id;
          return id ? [id.toLowerCase()] : [];
        })
        .toSorted(),
      publicIPDeleteOptions: (candidate.resource.properties?.ipConfigurations ?? [])
        .flatMap((configuration, index) => {
          const value = configuration.properties?.publicIPAddress?.deleteOption
            ?.trim()
            .toLowerCase();
          return value ? [`${index}:${value}`] : [];
        })
        .toSorted(),
      ...azureNICServiceAssociationTopology(candidate.resource),
    };
  }
  if (candidate.kind === "publicIPAddresses") {
    return {
      ipConfigurationID: candidate.resource.properties?.ipConfiguration?.id?.toLowerCase() ?? "",
      natGatewayID: candidate.resource.properties?.natGateway?.id?.toLowerCase() ?? "",
      linkedPublicIPAddressID:
        candidate.resource.properties?.linkedPublicIPAddress?.id?.toLowerCase() ?? "",
      servicePublicIPAddressID:
        candidate.resource.properties?.servicePublicIPAddress?.id?.toLowerCase() ?? "",
    };
  }
  if (candidate.kind === "disks") {
    return {
      managedBy: candidate.resource.managedBy?.toLowerCase() ?? "",
      managedByExtended: azureDiskAttachmentIDs(candidate.resource),
    };
  }
  return {};
}

interface AzureReconciliationIdentityEntry {
  kind: AzureLeaseResource["kind"];
  id: string;
  immutableID: string;
  location: string;
  topology: Record<string, string | string[]>;
  labels?: string;
}

function azureReconciliationIdentityEntries(
  resources: AzureLeaseResource[],
  includeLabels: boolean,
): AzureReconciliationIdentityEntry[] {
  return resources.map((candidate) => {
    const entry: AzureReconciliationIdentityEntry = {
      kind: candidate.kind,
      id: (candidate.resource.id ?? candidate.resource.name ?? "").toLowerCase(),
      immutableID: azureReconciliationImmutableIdentity(candidate),
      location: candidate.resource.location?.trim().toLowerCase() ?? "",
      topology: azureReconciliationTopologyIdentity(candidate),
    };
    if (includeLabels) {
      entry.labels = azureReconciliationLabelIdentity(candidate.resource);
    }
    return entry;
  });
}

function azureReconciliationResourceIdentity(resources: AzureLeaseResource[]): string {
  return JSON.stringify(azureReconciliationIdentityEntries(resources, true));
}

function azureStableResourceIdentity(resources: AzureLeaseResource[]): string {
  return JSON.stringify(azureReconciliationIdentityEntries(resources, false));
}

function azureStableResourceIdentityEntries(value: string): AzureReconciliationIdentityEntry[] {
  const parsed: unknown = JSON.parse(value);
  if (!Array.isArray(parsed)) {
    throw new Error("Azure reconciliation resource identity must be an array");
  }
  const kinds = new Set<AzureLeaseResource["kind"]>();
  return parsed.map((entry): AzureReconciliationIdentityEntry => {
    if (!entry || typeof entry !== "object" || Array.isArray(entry)) {
      throw new Error("Azure reconciliation resource identity member is invalid");
    }
    const member = entry as Record<string, unknown>;
    const kind = member["kind"];
    if (
      kind !== "virtualMachines" &&
      kind !== "networkInterfaces" &&
      kind !== "publicIPAddresses" &&
      kind !== "disks"
    ) {
      throw new Error("Azure reconciliation resource identity kind is invalid");
    }
    if (kinds.has(kind)) {
      throw new Error("Azure reconciliation resource identity has duplicate members");
    }
    const id = member["id"];
    const immutableID = member["immutableID"];
    const location = member["location"];
    const topology = member["topology"];
    const labels = member["labels"];
    if (
      typeof id !== "string" ||
      typeof immutableID !== "string" ||
      typeof location !== "string" ||
      (labels !== undefined && typeof labels !== "string") ||
      !topology ||
      typeof topology !== "object" ||
      Array.isArray(topology) ||
      !Object.values(topology).every(
        (topologyValue) =>
          typeof topologyValue === "string" ||
          (Array.isArray(topologyValue) && topologyValue.every((item) => typeof item === "string")),
      )
    ) {
      throw new Error("Azure reconciliation resource identity member is incomplete");
    }
    kinds.add(kind);
    return {
      kind,
      id,
      immutableID,
      location,
      topology: topology as Record<string, string | string[]>,
      ...(typeof labels === "string" ? { labels } : {}),
    };
  });
}

function azureStableResourceIdentityFromObserved(resourceIdentity: string): string {
  const entries = azureStableResourceIdentityEntries(resourceIdentity);
  if (
    entries.some((entry) => !entry.id.trim() || !entry.immutableID.trim() || !entry.location.trim())
  ) {
    throw new Error("Azure reconciliation resource identity is not stable");
  }
  return JSON.stringify(entries.map(({ labels: _labels, ...entry }) => entry));
}

function azureStableTopologyReferenceEntry(
  reference: string,
  entries: AzureReconciliationIdentityEntry[],
): AzureReconciliationIdentityEntry | undefined {
  const normalizedReference = azureNormalizedResourceID(reference);
  if (!normalizedReference) return undefined;
  return entries.find((entry) => {
    const normalizedID = azureNormalizedResourceID(entry.id);
    return Boolean(
      normalizedID &&
      (normalizedReference === normalizedID || normalizedReference.startsWith(`${normalizedID}/`)),
    );
  });
}

function azureStableTopologyMatches(
  expected: AzureReconciliationIdentityEntry,
  current: AzureReconciliationIdentityEntry,
  expectedEntries: AzureReconciliationIdentityEntry[],
  currentKinds: Set<AzureLeaseResource["kind"]>,
): boolean {
  const keys = new Set([...Object.keys(expected.topology), ...Object.keys(current.topology)]);
  for (const key of keys) {
    const expectedValue = expected.topology[key];
    const currentValue = current.topology[key];
    if (
      (expectedValue === undefined &&
        (currentValue === "" || (Array.isArray(currentValue) && currentValue.length === 0))) ||
      (currentValue === undefined &&
        (expectedValue === "" || (Array.isArray(expectedValue) && expectedValue.length === 0)))
    ) {
      continue;
    }
    if (Array.isArray(expectedValue) && Array.isArray(currentValue)) {
      const observedExpectedReferences = expectedValue.map(azureNormalizedResourceID);
      const expectedReferences = observedExpectedReferences
        .filter((reference) => {
          const entry = azureStableTopologyReferenceEntry(reference, expectedEntries);
          return !entry || currentKinds.has(entry.kind);
        })
        .toSorted();
      const expectedReferenceSet = new Set(observedExpectedReferences);
      const currentReferences = currentValue
        .map(azureNormalizedResourceID)
        .filter((reference) => {
          const entry = azureStableTopologyReferenceEntry(reference, expectedEntries);
          return !entry || currentKinds.has(entry.kind) || !expectedReferenceSet.has(reference);
        })
        .toSorted();
      if (JSON.stringify(expectedReferences) !== JSON.stringify(currentReferences)) {
        return false;
      }
      continue;
    }
    if (typeof expectedValue !== "string" || typeof currentValue !== "string") {
      return false;
    }
    const expectedReference = azureNormalizedResourceID(expectedValue);
    const currentReference = azureNormalizedResourceID(currentValue);
    if (expectedReference === currentReference) continue;
    const expectedEntry = azureStableTopologyReferenceEntry(expectedReference, expectedEntries);
    if (
      currentReference ||
      !expectedReference ||
      !expectedEntry ||
      currentKinds.has(expectedEntry.kind)
    ) {
      return false;
    }
  }
  return true;
}

function azureStableNormalizedTopology(
  topology: Record<string, string | string[]>,
): Record<string, string | string[]> {
  return Object.fromEntries(
    Object.entries(topology)
      .filter(([, value]) => value !== "" && (!Array.isArray(value) || value.length > 0))
      .toSorted(([leftKey], [rightKey]) => leftKey.localeCompare(rightKey)),
  );
}

function azureStableIdentityEntryExactlyMatches(
  left: AzureReconciliationIdentityEntry,
  right: AzureReconciliationIdentityEntry,
): boolean {
  return (
    left.kind === right.kind &&
    left.id === right.id &&
    left.immutableID === right.immutableID &&
    left.location === right.location &&
    JSON.stringify(azureStableNormalizedTopology(left.topology)) ===
      JSON.stringify(azureStableNormalizedTopology(right.topology))
  );
}

function azureStableIdentityEntryMatchesDeletedResource(
  expected: AzureReconciliationIdentityEntry,
  deleted: AzureReconciliationIdentityEntry,
  expectedEntries: AzureReconciliationIdentityEntry[],
  survivingKinds: Set<AzureLeaseResource["kind"]>,
): boolean {
  return (
    expected.kind === deleted.kind &&
    expected.id === deleted.id &&
    expected.immutableID === deleted.immutableID &&
    expected.location === deleted.location &&
    azureStableTopologyMatches(expected, deleted, expectedEntries, survivingKinds)
  );
}

const azureOwnedDeleteResourceKind: Record<
  keyof AzureOwnedDeleteResources,
  AzureLeaseResource["kind"]
> = {
  vm: "virtualMachines",
  nic: "networkInterfaces",
  pip: "publicIPAddresses",
  disk: "disks",
};

function azureOwnedDeleteClaimsShareSequence(
  left: AzureOwnedDeleteClaim,
  right: AzureOwnedDeleteClaim,
): boolean {
  return (
    left.version === right.version &&
    left.provider === right.provider &&
    left.leaseID === right.leaseID &&
    left.slug === right.slug &&
    left.owner === right.owner &&
    left.cloudID === right.cloudID &&
    left.providerScope === right.providerScope &&
    left.preparing === right.preparing &&
    left.canonicalVMID === right.canonicalVMID &&
    left.canonicalNICID === right.canonicalNICID &&
    left.resourceIdentity === right.resourceIdentity &&
    left.stableResourceIdentity === right.stableResourceIdentity &&
    left.partialStableResourceIdentity === right.partialStableResourceIdentity &&
    JSON.stringify(left.disk) === JSON.stringify(right.disk)
  );
}

function azureMergeDeletedStableResourceIdentity(
  left: string | undefined,
  right: string | undefined,
): string | undefined {
  if (!left) return right;
  if (!right) return left;
  const leftEntries = azureStableResourceIdentityEntries(left);
  const rightEntries = azureStableResourceIdentityEntries(right);
  const [shorter, longer] =
    leftEntries.length <= rightEntries.length
      ? [leftEntries, rightEntries]
      : [rightEntries, leftEntries];
  if (
    !shorter.every((entry, index) => azureStableIdentityEntryExactlyMatches(entry, longer[index]!))
  ) {
    throw new Error("Azure cleanup progress branches conflict");
  }
  return JSON.stringify(longer);
}

function azureStableResourceIdentityWithDeletedMember(
  deleted: string | undefined,
  current: string,
  kind: keyof AzureOwnedDeleteResources,
): string {
  const resourceKind = azureOwnedDeleteResourceKind[kind];
  const currentEntry = azureStableResourceIdentityEntries(current).find(
    (entry) => entry.kind === resourceKind,
  );
  if (!currentEntry) {
    throw new Error(`Azure cleanup progress is missing the deleted ${resourceKind} identity`);
  }
  const deletedEntries = deleted ? azureStableResourceIdentityEntries(deleted) : [];
  const previous = deletedEntries.find((entry) => entry.kind === resourceKind);
  if (previous) {
    if (!azureStableIdentityEntryExactlyMatches(previous, currentEntry)) {
      throw new Error(`Azure cleanup progress changed the deleted ${resourceKind} identity`);
    }
    return JSON.stringify(deletedEntries);
  }
  return JSON.stringify([...deletedEntries, currentEntry]);
}

function azureStableResourceIdentityDeletionProgressValid(
  stable: string | undefined,
  partial: string | undefined,
  deleted: string | undefined,
): boolean {
  if (!deleted) return true;
  if (!stable && !partial) return false;
  try {
    const stableEntries = stable ? azureStableResourceIdentityEntries(stable) : [];
    const partialEntries = partial ? azureStableResourceIdentityEntries(partial) : [];
    const deletedEntries = azureStableResourceIdentityEntries(deleted);
    const expectedEntries = [...stableEntries, ...partialEntries];
    const expectedKinds = Object.values(azureOwnedDeleteResourceKind).filter((kind) =>
      expectedEntries.some((entry) => entry.kind === kind),
    );
    if (deletedEntries.length > expectedKinds.length) return false;
    return deletedEntries.every((entry, index) => {
      if (entry.kind !== expectedKinds[index]) return false;
      const survivingKinds = new Set(expectedKinds.slice(index));
      return expectedEntries.some((expected) =>
        azureStableIdentityEntryMatchesDeletedResource(
          expected,
          entry,
          expectedEntries,
          survivingKinds,
        ),
      );
    });
  } catch {
    return false;
  }
}

function azureStableResourceIdentityMatches(
  expected: string,
  current: string,
  deleted?: string,
): boolean {
  try {
    const expectedEntries = azureStableResourceIdentityEntries(expected);
    const currentEntries = azureStableResourceIdentityEntries(current);
    const deletedEntries = deleted ? azureStableResourceIdentityEntries(deleted) : [];
    const expectedByKind = new Map(expectedEntries.map((entry) => [entry.kind, entry]));
    const currentByKind = new Map(currentEntries.map((entry) => [entry.kind, entry]));
    const deletedByKind = new Map(deletedEntries.map((entry) => [entry.kind, entry]));
    const currentKinds = new Set(currentEntries.map((entry) => entry.kind));
    return (
      currentEntries.every((entry) => {
        if (deletedByKind.has(entry.kind)) return false;
        const expectedEntry = expectedByKind.get(entry.kind);
        return Boolean(
          expectedEntry &&
          expectedEntry.id === entry.id &&
          expectedEntry.immutableID === entry.immutableID &&
          expectedEntry.location === entry.location &&
          azureStableTopologyMatches(expectedEntry, entry, expectedEntries, currentKinds),
        );
      }) &&
      expectedEntries.every((entry) => {
        const currentEntry = currentByKind.get(entry.kind);
        if (currentEntry) return true;
        const deletedEntry = deletedByKind.get(entry.kind);
        return Boolean(
          deletedEntry &&
          azureStableIdentityEntryMatchesDeletedResource(
            entry,
            deletedEntry,
            expectedEntries,
            currentKinds,
          ),
        );
      })
    );
  } catch {
    return false;
  }
}

function azureResourceIdentityMatches(
  expected: string,
  current: string,
  deleted?: string,
): boolean {
  try {
    if (!azureStableResourceIdentityMatches(expected, current, deleted)) return false;
    const expectedByKind = new Map(
      azureStableResourceIdentityEntries(expected).map((entry) => [entry.kind, entry]),
    );
    return azureStableResourceIdentityEntries(current).every((entry) => {
      const expectedEntry = expectedByKind.get(entry.kind);
      return Boolean(
        expectedEntry &&
        (expectedEntry.labels === undefined || expectedEntry.labels === entry.labels),
      );
    });
  } catch {
    return false;
  }
}

function azureStableResourceIdentityExactlyMatches(left: string, right: string): boolean {
  return (
    azureStableResourceIdentityMatches(left, right) &&
    azureStableResourceIdentityMatches(right, left)
  );
}

function azureStableResourceIdentityIncludesResourceID(
  value: string | undefined,
  kind: AzureLeaseResource["kind"],
  expectedID: string,
): boolean {
  if (!value) return false;
  try {
    return azureStableResourceIdentityEntries(value).some(
      (entry) => entry.kind === kind && azureResourceIDEqual(entry.id, expectedID),
    );
  } catch {
    return false;
  }
}

function azureReconciliationMachine(
  set: AzureReconciliationResourceSet,
): ProviderMachine | undefined {
  const resources: AzureLeaseResource[] = [
    ...(set.vm ? [{ kind: "virtualMachines" as const, resource: set.vm }] : []),
    ...(set.nic ? [{ kind: "networkInterfaces" as const, resource: set.nic }] : []),
    ...(set.pip ? [{ kind: "publicIPAddresses" as const, resource: set.pip }] : []),
    ...(set.disk ? [{ kind: "disks" as const, resource: set.disk }] : []),
  ];
  const tagged = resources.filter((candidate) => candidate.resource.tags?.["crabbox"] === "true");
  const representative = tagged[0];
  if (!representative) {
    return undefined;
  }
  const labels = azureLabelsFromTags(representative.resource.tags ?? {});
  const ownershipIdentities = new Set(
    tagged.map(({ resource }) => azureReconciliationLabelIdentity(resource)),
  );
  const locations = new Set(
    resources.map(({ resource }) => azureLocationKey(resource.location ?? "")),
  );
  if (
    tagged.length !== resources.length ||
    !resources.every(azureReconciliationMemberHasStableIdentity) ||
    !azureReconciliationTopologyConsistent(set) ||
    ownershipIdentities.size > 1 ||
    locations.size !== 1
  ) {
    labels["lease"] = "";
  }
  if (tagged.some(({ resource }) => azureLabelsFromTags(resource.tags ?? {})["keep"] === "true")) {
    labels["keep"] = "true";
  }
  return {
    provider: "azure",
    id: 0,
    cloudID: set.cloudID,
    ...(locations.size === 1 ? { region: [...locations][0] } : {}),
    name: set.cloudID,
    status: set.vm?.properties?.provisioningState ?? "provisioning",
    serverType: set.vm?.properties?.hardwareProfile?.vmSize ?? labels["server_type"] ?? "",
    host: set.pip?.properties?.ipAddress ?? "",
    labels,
    resourceIdentity: azureReconciliationResourceIdentity(resources),
  };
}

function azureReconciliationMachines(
  inventory: AzureLeaseResourceInventory,
  canonicalIDsForCloudID: (cloudID: string) => AzureReconciliationCanonicalIDs,
): ProviderMachine[] {
  const sets = new Map<string, AzureReconciliationResourceSet>();
  for (const candidate of azureLeaseResources(inventory)) {
    const cloudID = azureResourceSetCloudID(candidate.kind, candidate.resource.name);
    if (!cloudID) {
      continue;
    }
    const canonicalIDs = canonicalIDsForCloudID(cloudID);
    const set = sets.get(cloudID) ?? {
      cloudID,
      canonicalVMID: canonicalIDs.vmID,
      canonicalNICID: canonicalIDs.nicID,
      canonicalPIPID: canonicalIDs.pipID,
    };
    if (candidate.kind === "virtualMachines") {
      set.vm = candidate.resource;
    } else if (candidate.kind === "networkInterfaces") {
      set.nic = candidate.resource;
    } else if (candidate.kind === "publicIPAddresses") {
      set.pip = candidate.resource;
    } else {
      set.disk = candidate.resource;
    }
    sets.set(cloudID, set);
  }
  return [...sets.values()]
    .toSorted((left, right) => left.cloudID.localeCompare(right.cloudID))
    .map(azureReconciliationMachine)
    .filter((machine): machine is ProviderMachine => Boolean(machine));
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\"'\"'")}'`;
}

function azureProviderImage(
  image: AzureManagedImage,
  fallbackName: string,
  location: string,
): ProviderImage {
  const out: ProviderImage = {
    id: image.name ?? fallbackName,
    name: image.name ?? fallbackName,
    state: image.properties?.provisioningState?.toLowerCase() || "succeeded",
    provider: "azure",
    kind: "azure-managed-image",
    region: location,
  };
  if (image.id) out.resourceID = image.id;
  return out;
}

function azureSnapshotProviderImage(
  snapshot: AzureSnapshot,
  fallbackName: string,
  location: string,
): ProviderImage {
  const out: ProviderImage = {
    id: snapshot.name ?? fallbackName,
    name: snapshot.name ?? fallbackName,
    state: snapshot.properties?.provisioningState?.toLowerCase() || "succeeded",
    provider: "azure",
    kind: "azure-os-disk-snapshot",
    region: location,
  };
  if (snapshot.id) {
    out.resourceID = snapshot.id;
    out.snapshots = [snapshot.id];
  }
  return out;
}

function toMachine(vm: AzureVM, ip: string): ProviderMachine {
  return {
    provider: "azure",
    id: 0,
    cloudID: vm.name ?? "",
    ...(vm.location ? { region: vm.location } : {}),
    name: vm.name ?? "",
    status: vm.properties?.provisioningState ?? "",
    serverType: vm.properties?.hardwareProfile?.vmSize ?? "",
    host: ip,
    labels: azureLabelsFromTags(vm.tags ?? {}),
  };
}

export function azureTagsFromLabels(labels: Record<string, string>): Record<string, string> {
  return Object.fromEntries(
    Object.entries(labels).map(([key, value]) => [azureLabelToTagKey(key), value]),
  );
}

export function azureLabelsFromTags(tags: Record<string, string>): Record<string, string> {
  const labels = Object.fromEntries(
    Object.entries(tags).map(([key, value]) => [azureTagToLabelKey(key), value]),
  );
  if (!labels["windows_mode"] && labels["crabbox_windows_mode"]) {
    labels["windows_mode"] = labels["crabbox_windows_mode"];
  }
  return labels;
}

function azureLabelToTagKey(key: string): string {
  return key.toLowerCase().startsWith("windows") ? `crabbox_${key}` : key;
}

function azureTagToLabelKey(key: string): string {
  return key.startsWith("crabbox_windows") ? key.replace(/^crabbox_/, "") : key;
}

function isNotFound(error: unknown): boolean {
  const message = errorMessage(error);
  return message.includes("http 404") || message.includes("ResourceNotFound");
}

function azureVMNotFound(error: unknown, name: string): boolean {
  return azureResourceNotFound(error, "virtualMachines", name);
}

function azureResourceNotFound(error: unknown, kind: string, name: string): boolean {
  if (!(error instanceof AzureHTTPError) || error.status !== 404) return false;
  const namespace = kind === "virtualMachines" || kind === "disks" ? "compute" : "network";
  const body = error.body.toLowerCase();
  return (
    body.includes("resourcenotfound") &&
    body.includes(`microsoft.${namespace}/${kind.toLowerCase()}/${name.toLowerCase()}`)
  );
}

function azureNormalizedResourceID(value: string | undefined): string {
  return value?.trim().replace(/^\/+/, "").toLowerCase() ?? "";
}

function azureResourceIDEqual(actual: string | undefined, expected: string): boolean {
  return azureNormalizedResourceID(actual) === azureNormalizedResourceID(expected);
}

function azureHasOwnershipClaims(labels: Record<string, string>): boolean {
  return ["crabbox", "created_by", "lease", "owner", "provider", "slug"].some(
    (key) => (labels[key] ?? "").trim().length > 0,
  );
}

function azureOwnershipTags(
  tags: Record<string, string>,
  lease: Pick<LeaseRecord, "id" | "slug" | "provider" | "owner">,
): Record<string, string> {
  const labels = azureLabelsFromTags(tags);
  if (!providerLabelsOwnedByLease(labels, lease, "azure")) {
    throw new Error(`Azure VM ownership does not match lease ${lease.id}`);
  }
  return azureTagsFromLabels({
    crabbox: labels["crabbox"]!,
    created_by: labels["created_by"]!,
    lease: labels["lease"]!,
    owner: labels["owner"]!,
    provider: labels["provider"]!,
    slug: labels["slug"]!,
  });
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export function isRetryableDeleteError(error: unknown): boolean {
  const message = errorMessage(error);
  return (
    message.includes("NicReservedForAnotherVm") ||
    message.includes("PublicIPAddressCannotBeDeleted") ||
    message.includes("DiskInUse") ||
    message.includes("DiskIsAttachedToVM") ||
    message.includes("DiskAttached") ||
    message.includes("CannotDeleteDisk") ||
    message.includes("InUse") ||
    message.includes("AnotherOperationInProgress") ||
    (message.includes("OperationNotAllowed") && message.includes("retry after"))
  );
}

export function preserveNonCrabboxRules(rules: AzureSecurityRule[]): AzureSecurityRule[] {
  return rules.filter((rule) => !rule.name?.startsWith("crabbox-ssh-"));
}

function azureCrabboxSSHRulesMatch(existing: AzureSecurityRule[], desired: AzureSecurityRule[]) {
  const existingCrabbox = existing.filter((rule) => rule.name?.startsWith("crabbox-ssh-"));
  if (existingCrabbox.length !== desired.length) return false;
  const existingKeys = new Set(existingCrabbox.map(azureSecurityRuleKey));
  return desired.every((rule) => existingKeys.has(azureSecurityRuleKey(rule)));
}

function azureSecurityRuleKey(rule: AzureSecurityRule): string {
  const properties = rule.properties ?? {};
  return JSON.stringify({
    name: rule.name ?? "",
    priority: properties["priority"] ?? null,
    direction: properties["direction"] ?? "",
    access: properties["access"] ?? "",
    protocol: properties["protocol"] ?? "",
    sourceAddressPrefix: properties["sourceAddressPrefix"] ?? "",
    sourcePortRange: properties["sourcePortRange"] ?? "",
    destinationAddressPrefix: properties["destinationAddressPrefix"] ?? "",
    destinationPortRange: properties["destinationPortRange"] ?? "",
  });
}

function usedNSGPriorities(rules: AzureSecurityRule[]): Set<number> {
  const used = new Set<number>();
  for (const rule of rules) {
    const priority = rule.properties?.["priority"];
    if (typeof priority === "number") used.add(priority);
  }
  return used;
}

function nextNSGPriority(used: Set<number>): number {
  for (let priority = 100; priority <= 4096; priority += 1) {
    if (!used.has(priority)) {
      used.add(priority);
      return priority;
    }
  }
  throw new Error("azure nsg: no available security rule priorities");
}

export function azureLROPollIntervalMS(retryAfter: string | null): number {
  const seconds = Number.parseInt(retryAfter ?? "", 10);
  if (!Number.isFinite(seconds) || seconds <= 0) return MIN_LRO_POLL_INTERVAL_MS;
  return Math.max(seconds * 1000, MIN_LRO_POLL_INTERVAL_MS);
}

function azureOSDiskIsEphemeral(mode: string): boolean {
  return mode === "ephemeral" || mode === "ephemeral-preview";
}

function azureOSDiskUsesFullCaching(mode: string): boolean {
  return mode === "ephemeral-preview";
}

export function azureProvisioningCandidatesForConfig(
  config: Pick<
    LeaseConfig,
    | "serverType"
    | "serverTypeExplicit"
    | "class"
    | "target"
    | "windowsMode"
    | "architecture"
    | "azureSnapshot"
    | "azureOSDisk"
  >,
): string[] {
  if (config.serverTypeExplicit && config.serverType) {
    return [config.serverType];
  }
  const azureOSDisk = config.azureSnapshot ? "managed" : config.azureOSDisk;
  const candidates = azureVMSizeCandidatesForTargetClass(
    config.target,
    config.class,
    config.windowsMode,
    config.architecture,
    azureOSDisk,
  );
  if (candidates.length === 0 && isCanonicalProviderClass(config.class)) {
    const storedType = concreteStoredServerType(config.serverType, config.class);
    if (!storedType) return [];
    return azureOSDiskUsesFullCaching(azureOSDisk) && !azureSupportsEphemeralFullCaching(storedType)
      ? []
      : [storedType];
  }
  const storedType = concreteStoredServerType(config.serverType, config.class);
  if (!storedType || storedType === candidates[0]) {
    return candidates;
  }
  if (azureOSDiskUsesFullCaching(azureOSDisk)) {
    return azureSupportsEphemeralFullCaching(storedType)
      ? uniqueProviderMachineCandidates([storedType, ...candidates])
      : candidates;
  }
  return uniqueProviderMachineCandidates([storedType, ...candidates]);
}

function azureComputeAPIVersionForOSDisk(mode: string): string {
  return azureOSDiskUsesFullCaching(mode)
    ? COMPUTE_FULL_CACHING_PREVIEW_API_VERSION
    : API_VERSIONS.compute;
}

function azureSKUCapabilityTrue(
  capabilities: { name?: string; value?: string }[] | undefined,
  name: string,
): boolean {
  return (
    capabilities?.some(
      (capability) => capability.name === name && capability.value?.toLowerCase() === "true",
    ) ?? false
  );
}

export function isRetryableProvisioningError(message: string): boolean {
  return (
    message.includes("SkuNotAvailable") ||
    message.includes("long-running operation timed out") ||
    message.includes("QuotaExceeded") ||
    message.includes("AllocationFailed") ||
    message.includes("ZonalAllocationFailed") ||
    message.includes("OverconstrainedAllocationRequest") ||
    message.includes("OperationNotAllowed") ||
    message.includes("NotAvailableForSubscription")
  );
}

export function azureRegionCandidates(
  config: Pick<LeaseConfig, "azureLocation" | "capacityRegions">,
  env: Pick<Env, "CRABBOX_AZURE_LOCATION" | "CRABBOX_AZURE_REGIONS">,
  preferredLocation = "eastus",
): string[] {
  return uniqueStrings(
    [
      config.azureLocation,
      env.CRABBOX_AZURE_LOCATION ?? "",
      preferredLocation,
      ...splitCommaList(env.CRABBOX_AZURE_REGIONS ?? ""),
      ...config.capacityRegions,
    ].filter(Boolean),
  );
}

export function azureRegionalName(base: string, location: string): string {
  if (!base) return base;
  const suffix = azureLocationKey(location);
  if (!suffix || base.toLowerCase().endsWith(`-${suffix}`)) return base;
  return `${base}-${suffix}`;
}

function azureLocationKey(location: string): string {
  return location
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9-]/g, "-")
    .replace(/^-+|-+$/g, "");
}

function azureSameLocation(existing: string | undefined, desired: string): boolean {
  if (!existing || !desired.trim()) return true;
  return azureLocationKey(existing) === azureLocationKey(desired);
}

export function azureProvisioningErrorCategory(message: string): string | undefined {
  if (message.includes("long-running operation timed out")) return "capacity";
  if (message.includes("QuotaExceeded")) return "quota";
  if (
    message.includes("SkuNotAvailable") ||
    message.includes("AllocationFailed") ||
    message.includes("ZonalAllocationFailed") ||
    message.includes("OverconstrainedAllocationRequest")
  ) {
    return "capacity";
  }
  if (message.includes("NotAvailableForSubscription") || message.includes("OperationNotAllowed")) {
    return "policy";
  }
  return undefined;
}

export function azureSpotFallbackTimeoutMs(
  config: Pick<LeaseConfig, "capacityMarket" | "capacityFallback">,
): number | undefined {
  if (config.capacityMarket !== "spot") return undefined;
  const fallback = config.capacityFallback.trim().toLowerCase();
  if (fallback === "" || fallback === "none" || fallback === "spot-only") {
    return DEFAULT_AZURE_SPOT_FALLBACK_MS;
  }
  if (!fallback.startsWith("on-demand-after-")) return undefined;
  const duration = fallback.slice("on-demand-after-".length);
  const match = /^(\d+)(ms|s|m)?$/.exec(duration);
  if (!match?.[1]) return DEFAULT_AZURE_SPOT_FALLBACK_MS;
  const value = Number(match[1]);
  if (!Number.isFinite(value) || value <= 0) return DEFAULT_AZURE_SPOT_FALLBACK_MS;
  const unit = match[2] ?? "s";
  if (unit === "ms") return value;
  if (unit === "m") return value * 60_000;
  return value * 1000;
}

function azureVMCreateTimeoutMs(
  config: Pick<LeaseConfig, "capacityMarket" | "capacityFallback">,
): number {
  return azureSpotFallbackTimeoutMs(config) ?? DEFAULT_AZURE_VM_CREATE_TIMEOUT_MS;
}

function azureAttemptNameSeed(
  leaseID: string,
  location: string,
  market: "spot" | "on-demand",
  index: number,
): string {
  return `${leaseID}-${azureLocationKey(location)}-${market}-${index}`;
}

function isAzureVMCreateTimeout(error: unknown): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return message.includes("azure long-running operation timed out after");
}

export function conciseAzureProvisioningMessage(message: string): string {
  const parsed = parseAzureStatusMessage(message);
  const raw = parsed || message;
  return raw.split(/\n|\. /, 1)[0]?.trim() || raw.trim();
}

function splitCommaList(value: string): string[] {
  return value
    .split(",")
    .map((entry) => entry.trim())
    .filter(Boolean);
}

function uniqueStrings(values: string[]): string[] {
  return [...new Set(values.filter(Boolean))];
}

function parseAzureStatusMessage(message: string): string {
  const match = message.match(/"message"\s*:\s*"((?:\\.|[^"\\])*)"/);
  if (!match) return "";
  try {
    return JSON.parse(`"${match[1]}"`) as string;
  } catch {
    return match[1] ?? "";
  }
}

function isAzureDefaultLinuxImage(image: string): boolean {
  return (
    image.trim() === DEFAULT_AZURE_LINUX_IMAGE ||
    image.trim() === DEFAULT_AZURE_LINUX_ARM64_IMAGE ||
    image.trim() === AZURE_NOBLE_LINUX_IMAGE ||
    image.trim() === AZURE_NOBLE_LINUX_ARM64_IMAGE ||
    image.trim() === LEGACY_AZURE_JAMMY_IMAGE ||
    image.trim() === LEGACY_AZURE_NOBLE_GEN2_IMAGE
  );
}

async function safeBody(response: Response): Promise<string> {
  const text = await response.text();
  return summarizeAzureErrorBody(text);
}

export function summarizeAzureErrorBody(text: string): string {
  const raw = text.trim();
  if (!raw) return raw;
  try {
    const parsed = JSON.parse(raw) as {
      error?: { code?: string; message?: string; details?: { code?: string; message?: string }[] };
    };
    const error = parsed.error;
    if (error?.message) {
      const details =
        error.details
          ?.map((detail) => [detail.code, detail.message].filter(Boolean).join(": "))
          .filter(Boolean)
          .join("; ") ?? "";
      return truncateAzureBody(
        [
          error.code,
          normalizeAzureBodyWhitespace(error.message),
          normalizeAzureBodyWhitespace(details),
        ]
          .filter(Boolean)
          .join(": "),
      );
    }
  } catch {
    // Fall back to the raw response body below.
  }
  return truncateAzureBody(normalizeAzureBodyWhitespace(raw));
}

function normalizeAzureBodyWhitespace(text: string): string {
  return text.replace(/\s+/g, " ").trim();
}

function truncateAzureBody(text: string): string {
  return text.length > 1000 ? `${text.slice(0, 1000)}...` : text;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
