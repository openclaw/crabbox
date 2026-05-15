import { azureWindowsBootstrapPowerShell, cloudInit } from "./bootstrap";
import {
  azureLocationFor,
  azureVMSizeCandidatesForTargetClass,
  sshPorts,
  type LeaseConfig,
} from "./config";
import { leaseProviderLabels } from "./provider-labels";
import { leaseProviderName } from "./slug";
import type { Env, ProviderImage, ProviderMachine } from "./types";

const ADDRESS_SPACE = "10.42.0.0/16";
const SUBNET_CIDR = "10.42.0.0/24";
const API_VERSIONS = {
  resources: "2021-04-01",
  network: "2024-05-01",
  compute: "2024-07-01",
  disks: "2024-03-02",
};
const DELETE_RETRY_ATTEMPTS = 13;
const DELETE_RETRY_DELAY_MS = 15_000;
const MIN_LRO_POLL_INTERVAL_MS = 15_000;
const DEFAULT_AZURE_LINUX_IMAGE = "Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest";
const DEFAULT_AZURE_WINDOWS_IMAGE =
  "MicrosoftWindowsServer:windowsserver2022:2022-datacenter-smalldisk-g2:latest";

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
    provisioningState?: string;
    hardwareProfile?: { vmSize?: string };
    storageProfile?: { osDisk?: { managedDisk?: { id?: string }; osType?: string } };
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
  properties?: { ipAddress?: string };
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

export class AzureClient {
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
  fetcher: typeof fetch = (input, init) => fetch(input, init);

  constructor(env: Env) {
    if (!env.AZURE_TENANT_ID) throw new Error("AZURE_TENANT_ID secret is required");
    if (!env.AZURE_CLIENT_ID) throw new Error("AZURE_CLIENT_ID secret is required");
    if (!env.AZURE_CLIENT_SECRET) throw new Error("AZURE_CLIENT_SECRET secret is required");
    if (!env.AZURE_SUBSCRIPTION_ID) throw new Error("AZURE_SUBSCRIPTION_ID secret is required");
    this.tenant = env.AZURE_TENANT_ID;
    this.clientID = env.AZURE_CLIENT_ID;
    this.secret = env.AZURE_CLIENT_SECRET;
    this.subscription = env.AZURE_SUBSCRIPTION_ID;
    this.resourceGroup = env.CRABBOX_AZURE_RESOURCE_GROUP?.trim() || "crabbox-leases";
    this.vnet = env.CRABBOX_AZURE_VNET?.trim() || "crabbox-vnet";
    this.subnet = env.CRABBOX_AZURE_SUBNET?.trim() || "crabbox-subnet";
    this.nsg = env.CRABBOX_AZURE_NSG?.trim() || "crabbox-nsg";
    this.image = env.CRABBOX_AZURE_IMAGE?.trim() || DEFAULT_AZURE_LINUX_IMAGE;
    this.sshCIDRs = (env.CRABBOX_AZURE_SSH_CIDRS ?? "")
      .split(",")
      .map((value) => value.trim())
      .filter(Boolean);
    if (this.sshCIDRs.length === 0) this.sshCIDRs.push("0.0.0.0/0");
    this.defaultLocation = env.CRABBOX_AZURE_LOCATION?.trim() || "eastus";
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

  async createServerWithFallback(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<{ server: ProviderMachine; serverType: string; market: string }> {
    const location = azureLocationFor(
      { CRABBOX_AZURE_LOCATION: this.defaultLocation },
      config.azureLocation,
    );
    await this.ensureSharedInfra(location, config);
    const candidates =
      config.serverTypeExplicit && config.serverType
        ? [config.serverType]
        : prependUnique(
            config.serverType,
            azureVMSizeCandidatesForTargetClass(config.target, config.class, config.windowsMode),
          );
    const failures: string[] = [];
    for (const vmSize of candidates) {
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- SKU fallback must stay sequential.
        const server = await this.createVM(
          { ...config, serverType: vmSize },
          location,
          leaseID,
          slug,
          owner,
        );
        return { server, serverType: vmSize, market: config.capacityMarket };
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        failures.push(`${vmSize}: ${message}`);
        if (!isRetryableProvisioningError(message)) break;
      }
    }
    if (config.capacityMarket === "spot" && config.capacityFallback.startsWith("on-demand")) {
      for (const vmSize of candidates) {
        try {
          // oxlint-disable-next-line eslint/no-await-in-loop -- market fallback must preserve ordered capacity preference.
          const server = await this.createVM(
            { ...config, capacityMarket: "on-demand", serverType: vmSize },
            location,
            leaseID,
            slug,
            owner,
          );
          return { server, serverType: vmSize, market: "on-demand" };
        } catch (error) {
          const message = error instanceof Error ? error.message : String(error);
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
      // oxlint-disable-next-line eslint/no-await-in-loop -- the next delete attempt depends on this delay.
      await sleep(DELETE_RETRY_DELAY_MS);
    }
  }

  private async deleteServerOnce(name: string): Promise<{ errors: string[]; retry: boolean }> {
    const result = { errors: [] as string[], retry: false };
    await this.deleteResource("vm", vmPath(this.resourceGroup, name), API_VERSIONS.compute, result);
    await this.deleteResource(
      "nic",
      networkPath(this.resourceGroup, "networkInterfaces", `${name}-nic`),
      API_VERSIONS.network,
      result,
    );
    await this.deleteResource(
      "pip",
      networkPath(this.resourceGroup, "publicIPAddresses", `${name}-pip`),
      API_VERSIONS.network,
      result,
    );
    await this.deleteResource(
      "disk",
      `/resourceGroups/${this.resourceGroup}/providers/Microsoft.Compute/disks/${name}-osdisk`,
      API_VERSIONS.disks,
      result,
    );
    return result;
  }

  private async deleteResource(
    kind: string,
    path: string,
    apiVersion: string,
    result: { errors: string[]; retry: boolean },
  ): Promise<void> {
    try {
      await this.arm("DELETE", path, apiVersion);
    } catch (error) {
      if (isNotFound(error)) return;
      result.errors.push(`delete ${kind}: ${errorMessage(error)}`);
      result.retry ||= isRetryableDeleteError(error);
    }
  }

  async ensureSharedInfra(location: string, config: LeaseConfig): Promise<void> {
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
      await this.arm("PUT", `/resourceGroups/${this.resourceGroup}`, API_VERSIONS.resources, {
        location,
        tags,
      });
    }
    const vnet = await this.arm<{ tags?: Record<string, string> }>(
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
    } else {
      await this.arm(
        "PUT",
        networkPath(this.resourceGroup, "virtualNetworks", this.vnet),
        API_VERSIONS.network,
        {
          location,
          tags,
          properties: {
            addressSpace: { addressPrefixes: [ADDRESS_SPACE] },
            subnets: [{ name: this.subnet, properties: { addressPrefix: SUBNET_CIDR } }],
          },
        },
      );
    }
    const nsg = await this.arm<{
      tags?: Record<string, string>;
      properties?: { securityRules?: AzureSecurityRule[] };
    }>(
      "GET",
      networkPath(this.resourceGroup, "networkSecurityGroups", this.nsg),
      API_VERSIONS.network,
    ).catch((error) => {
      if (isNotFound(error)) return undefined;
      throw error;
    });
    if (nsg && nsg.tags?.["managed_by"] !== "crabbox") {
      throw new Error(`azure nsg ${this.nsg} is not Crabbox-managed`);
    }
    const preserved = preserveNonCrabboxRules(nsg?.properties?.securityRules ?? []);
    const usedPriorities = usedNSGPriorities(preserved);
    const rules = [...preserved, ...this.buildSSHRules(config, usedPriorities)];
    await this.arm(
      "PUT",
      networkPath(this.resourceGroup, "networkSecurityGroups", this.nsg),
      API_VERSIONS.network,
      {
        location,
        tags,
        properties: { securityRules: rules },
      },
    );
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
  ): Promise<ProviderMachine> {
    const name = leaseProviderName(leaseID, slug);
    try {
      return await this.createVMUnchecked(config, location, leaseID, slug, owner, name);
    } catch (error) {
      await this.deleteServer(name).catch(() => undefined);
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
    );
    const subnetID = `/subscriptions/${this.subscription}/resourceGroups/${this.resourceGroup}/providers/Microsoft.Network/virtualNetworks/${this.vnet}/subnets/${this.subnet}`;
    const nsgID = `/subscriptions/${this.subscription}/resourceGroups/${this.resourceGroup}/providers/Microsoft.Network/networkSecurityGroups/${this.nsg}`;
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
    );
    const customData = btoa(
      config.target === "windows" ? azureWindowsBootstrapPowerShell(config) : cloudInit(config),
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
      const image = azureImageReference(this.imageForConfig(config));
      const osDisk: Record<string, unknown> = {
        name: `${name}-osdisk`,
        createOption: "FromImage",
      };
      if (await this.supportsEphemeralOS(config.serverType, location)) {
        osDisk["caching"] = "ReadOnly";
        osDisk["diffDiskSettings"] = { option: "Local" };
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
    }
    await this.arm("PUT", vmPath(this.resourceGroup, name), API_VERSIONS.compute, {
      location,
      tags,
      properties: vmProperties,
    });
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

  private imageForConfig(config: LeaseConfig): string {
    const image = config.azureImage || this.image;
    if (config.target === "windows" && image === DEFAULT_AZURE_LINUX_IMAGE) {
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
    const sourceDiskID = vm.properties?.storageProfile?.osDisk?.managedDisk?.id;
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
      throw new Error(
        `azure ${method} ${path}: http ${response.status}: ${await safeBody(response)}`,
      );
    }
    const initialText = await response.text();
    if (response.status === 201 || response.status === 202) {
      await this.awaitLRO(response, token);
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

  private async awaitLRO(response: Response, token: string): Promise<void> {
    const asyncURL =
      response.headers.get("azure-asyncoperation") ?? response.headers.get("location");
    if (!asyncURL) return;
    const interval = azureLROPollIntervalMS(response.headers.get("retry-after"));
    const deadline = Date.now() + 20 * 60_000;
    while (Date.now() < deadline) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- LRO must wait between status reads.
      await sleep(interval);
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
    }
    throw new Error("azure long-running operation timed out");
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

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export function isRetryableDeleteError(error: unknown): boolean {
  const message = errorMessage(error);
  return (
    message.includes("NicReservedForAnotherVm") ||
    message.includes("PublicIPAddressCannotBeDeleted") ||
    message.includes("InUse") ||
    message.includes("AnotherOperationInProgress") ||
    (message.includes("OperationNotAllowed") && message.includes("retry after"))
  );
}

export function preserveNonCrabboxRules(rules: AzureSecurityRule[]): AzureSecurityRule[] {
  return rules.filter((rule) => !rule.name?.startsWith("crabbox-ssh-"));
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

export function azureSupportsEphemeralOS(vmSize: string): boolean {
  const normalized = vmSize.toLowerCase();
  if (normalized.startsWith("standard_f") && normalized.endsWith("s_v2")) {
    return true;
  }
  if (
    (normalized.startsWith("standard_d") || normalized.startsWith("standard_e")) &&
    (normalized.includes("ds_v5") || normalized.includes("ds_v6"))
  ) {
    return true;
  }
  return false;
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
    message.includes("QuotaExceeded") ||
    message.includes("AllocationFailed") ||
    message.includes("ZonalAllocationFailed") ||
    message.includes("OverconstrainedAllocationRequest") ||
    message.includes("OperationNotAllowed")
  );
}

function prependUnique(first: string, rest: string[]): string[] {
  return [first, ...rest.filter((value) => value !== first)];
}

async function safeBody(response: Response): Promise<string> {
  const text = await response.text();
  return text.length > 500 ? `${text.slice(0, 500)}...` : text;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
