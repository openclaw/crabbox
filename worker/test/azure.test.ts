import { describe, expect, it } from "vitest";

import {
  AzureClient,
  azureLabelsFromTags,
  azureLROPollIntervalMS,
  azureProvisioningErrorCategory,
  azureRegionCandidates,
  azureRegionalName,
  azureSupportsEphemeralOS,
  azureTagsFromLabels,
  conciseAzureProvisioningMessage,
  isRetryableDeleteError,
  isRetryableProvisioningError,
  preserveNonCrabboxRules,
  summarizeAzureErrorBody,
} from "../src/azure";
import type { LeaseConfig } from "../src/config";
import type { Env } from "../src/types";

const baseEnv: Env = {
  FLEET: {} as DurableObjectNamespace,
  HETZNER_TOKEN: "",
  AZURE_TENANT_ID: "tenant",
  AZURE_CLIENT_ID: "client",
  AZURE_CLIENT_SECRET: "secret",
  AZURE_SUBSCRIPTION_ID: "sub",
};

function isAzureLoginURL(value: string): boolean {
  return new URL(value).hostname === "login.microsoftonline.com";
}

function testLeaseConfig(overrides: Partial<LeaseConfig> = {}): LeaseConfig {
  return {
    provider: "azure",
    target: "linux",
    architecture: "amd64",
    windowsMode: "normal",
    desktop: false,
    desktopEnv: "xfce",
    browser: false,
    code: false,
    tailscale: false,
    tailscaleTags: ["tag:crabbox"],
    tailscaleHostname: "",
    tailscaleAuthKey: "",
    tailscaleExitNode: "",
    tailscaleExitNodeAllowLanAccess: false,
    profile: "default",
    class: "standard",
    serverType: "Standard_D2ads_v6",
    serverTypeExplicit: true,
    location: "fsn1",
    image: "ubuntu-24.04",
    awsRegion: "eu-west-1",
    awsAMI: "",
    awsSnapshot: "",
    awsSGID: "",
    awsSubnetID: "",
    awsProfile: "",
    awsRootGB: 400,
    awsSSHCIDRs: [],
    awsMacHostID: "",
    azureLocation: "eastus",
    azureImage: "",
    azureSnapshot: "",
    azureOSDisk: "managed",
    gcpProject: "",
    gcpZone: "",
    gcpImage: "",
    gcpMachineImage: "",
    gcpSnapshot: "",
    gcpNetwork: "",
    gcpSubnet: "",
    gcpTags: [],
    gcpSSHCIDRs: [],
    gcpRootGB: 0,
    gcpServiceAccount: "",
    capacityMarket: "spot",
    capacityStrategy: "most-available",
    capacityFallback: "on-demand-after-120s",
    capacityRegions: [],
    capacityAvailabilityZones: [],
    capacityHints: true,
    sshUser: "crabbox",
    sshPort: "2222",
    sshFallbackPorts: ["22"],
    providerKey: "crabbox-cbx",
    workRoot: "/workspace",
    ttlSeconds: 5400,
    idleTimeoutSeconds: 1800,
    keep: false,
    sshPublicKey: "ssh-rsa test",
    ...overrides,
  };
}

describe("azure provider", () => {
  it("classifies Azure capacity and quota errors as retryable", () => {
    expect(isRetryableProvisioningError("SkuNotAvailable: D8s_v5 not available")).toBe(true);
    expect(isRetryableProvisioningError("QuotaExceeded for cores")).toBe(true);
    expect(isRetryableProvisioningError("AllocationFailed")).toBe(true);
    expect(isRetryableProvisioningError("OverconstrainedAllocationRequest")).toBe(true);
    expect(isRetryableProvisioningError("NotAvailableForSubscription")).toBe(true);
    expect(isRetryableProvisioningError("ResourceNotFound")).toBe(false);
    expect(isRetryableProvisioningError("")).toBe(false);
  });

  it("orders Azure region candidates from defaults, env, and capacity regions", () => {
    expect(
      azureRegionCandidates(
        { azureLocation: "westeurope", capacityRegions: ["northeurope", "westeurope"] },
        { CRABBOX_AZURE_LOCATION: "centralus", CRABBOX_AZURE_REGIONS: "northeurope,westeurope" },
        "eastus",
      ),
    ).toEqual(["westeurope", "centralus", "eastus", "northeurope"]);
    expect(azureRegionalName("crabbox-vnet", "West Europe")).toBe("crabbox-vnet-west-europe");
    expect(azureRegionalName("crabbox-vnet-westeurope", "westeurope")).toBe(
      "crabbox-vnet-westeurope",
    );
  });

  it("classifies and condenses Azure provisioning failures for capacity hints", () => {
    const body =
      '{"error":{"code":"SkuNotAvailable","message":"The requested VM size is currently not available. Try another size."}}';
    expect(azureProvisioningErrorCategory(body)).toBe("capacity");
    expect(conciseAzureProvisioningMessage(body)).toBe(
      "The requested VM size is currently not available",
    );
    expect(azureProvisioningErrorCategory("QuotaExceeded for cores")).toBe("quota");
    expect(azureProvisioningErrorCategory("NotAvailableForSubscription")).toBe("policy");
  });

  it("summarizes Azure error bodies before truncating", () => {
    const longMessage = `The VM size is unavailable.\r\n${"extra ".repeat(140)}Try another size.`;
    const body = JSON.stringify({
      error: {
        code: "OperationNotAllowed",
        message: longMessage,
        details: [{ code: "SkuRestriction", message: "Standard_B2s is restricted here." }],
      },
    });
    const summary = summarizeAzureErrorBody(body);
    expect(summary).toContain("OperationNotAllowed");
    expect(summary).toContain("The VM size is unavailable.");
    expect(summary).toContain("SkuRestriction: Standard_B2s is restricted here.");
    expect(summary).not.toContain("\r");
    expect(summary.length).toBeLessThanOrEqual(1003);
  });

  it("classifies transient Azure delete dependency errors as retryable", () => {
    expect(isRetryableDeleteError("NicReservedForAnotherVm retry after 180 seconds")).toBe(true);
    expect(isRetryableDeleteError("PublicIPAddressCannotBeDeleted because it is in use")).toBe(
      true,
    );
    expect(isRetryableDeleteError("AnotherOperationInProgress")).toBe(true);
    expect(isRetryableDeleteError("plain validation error")).toBe(false);
  });

  it("maps Azure-reserved Windows tag prefixes without changing internal labels", () => {
    const tags = azureTagsFromLabels({ crabbox: "true", windows_mode: "normal" });
    expect(tags.windows_mode).toBeUndefined();
    expect(tags.crabbox_windows_mode).toBe("normal");
    expect(azureLabelsFromTags(tags).windows_mode).toBe("normal");
  });

  it("reads and deletes managed images by explicit kind", async () => {
    const client = new AzureClient(baseEnv);
    const calls: Array<{ method: string; pathname: string }> = [];
    client.fetcher = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = new URL(typeof input === "string" ? input : input.toString());
      if (url.hostname === "login.microsoftonline.com") {
        return Response.json({ access_token: "tkn", expires_in: 3600 });
      }
      calls.push({ method: init?.method ?? "GET", pathname: url.pathname });
      if (url.pathname.endsWith("/images/checkpoint-azure") && init?.method !== "DELETE") {
        return Response.json({
          id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/images/checkpoint-azure",
          name: "checkpoint-azure",
          location: "eastus",
          properties: { provisioningState: "Succeeded" },
        });
      }
      return new Response(null, { status: 204 });
    };

    const image = await client.getImage("checkpoint-azure", "azure-managed-image");
    await client.deleteImage("checkpoint-azure", "azure-managed-image");

    expect(image).toMatchObject({
      id: "checkpoint-azure",
      provider: "azure",
      kind: "azure-managed-image",
      state: "succeeded",
    });
    expect(calls.map((call) => `${call.method} ${call.pathname}`)).toEqual([
      "GET /subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/images/checkpoint-azure",
      "DELETE /subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/images/checkpoint-azure",
    ]);
  });

  it("routes kind-specific snapshot reads and deletes to Azure snapshots", async () => {
    const client = new AzureClient(baseEnv);
    const calls: Array<{ method: string; pathname: string }> = [];
    client.fetcher = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = new URL(typeof input === "string" ? input : input.toString());
      if (url.hostname === "login.microsoftonline.com") {
        return Response.json({ access_token: "tkn", expires_in: 3600 });
      }
      calls.push({ method: init?.method ?? "GET", pathname: url.pathname });
      if (url.pathname.endsWith("/snapshots/checkpoint-azure")) {
        return Response.json({
          id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/snapshots/checkpoint-azure",
          name: "checkpoint-azure",
          location: "eastus",
          properties: { provisioningState: "Succeeded" },
        });
      }
      return new Response("not found", { status: 404 });
    };

    const image = await client.getImage(
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/snapshots/checkpoint-azure",
      "azure-os-disk-snapshot",
    );
    await client.deleteImage(
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/snapshots/checkpoint-azure",
      "azure-os-disk-snapshot",
    );

    expect(image).toMatchObject({
      id: "checkpoint-azure",
      provider: "azure",
      kind: "azure-os-disk-snapshot",
    });
    expect(calls.map((call) => `${call.method} ${call.pathname}`)).toEqual([
      "GET /subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/snapshots/checkpoint-azure",
      "DELETE /subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/snapshots/checkpoint-azure",
    ]);
  });

  it("refuses createDiskSnapshot for VMs with an ephemeral OS disk", async () => {
    const client = new AzureClient(baseEnv);
    const calls: Array<{ method: string; pathname: string }> = [];
    client.fetcher = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = new URL(typeof input === "string" ? input : input.toString());
      if (url.hostname === "login.microsoftonline.com") {
        return Response.json({ access_token: "tkn", expires_in: 3600 });
      }
      calls.push({ method: init?.method ?? "GET", pathname: url.pathname });
      if (url.pathname.endsWith("/virtualMachines/crabbox-blue-lobster")) {
        return Response.json({
          id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-blue-lobster",
          name: "crabbox-blue-lobster",
          location: "eastus",
          properties: {
            provisioningState: "Succeeded",
            hardwareProfile: { vmSize: "Standard_D2ads_v6" },
            storageProfile: {
              osDisk: {
                name: "crabbox-blue-lobster-osdisk",
                osType: "Linux",
                caching: "ReadOnly",
                createOption: "FromImage",
                diffDiskSettings: { option: "Local", placement: "NvmeDisk" },
                managedDisk: {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk",
                  storageAccountType: "Standard_LRS",
                },
              },
            },
          },
        });
      }
      return new Response("unexpected request", { status: 500 });
    };

    await expect(
      client.createDiskSnapshot("crabbox-blue-lobster", "chk-ephemeral-repro"),
    ).rejects.toThrow(/azure ephemeral OS disk on vm crabbox-blue-lobster cannot be snapshotted/);
    expect(calls.map((call) => `${call.method} ${call.pathname}`)).toEqual([
      "GET /subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-blue-lobster",
    ]);
  });

  it("snapshots VMs with a non-Local diffDiskSettings option", async () => {
    const client = new AzureClient(baseEnv);
    const calls: Array<{ method: string; pathname: string }> = [];
    client.fetcher = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = new URL(typeof input === "string" ? input : input.toString());
      if (url.hostname === "login.microsoftonline.com") {
        return Response.json({ access_token: "tkn", expires_in: 3600 });
      }
      const method = init?.method ?? "GET";
      calls.push({ method, pathname: url.pathname });
      if (url.pathname.endsWith("/virtualMachines/crabbox-mauve-shrimp")) {
        return Response.json({
          id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-mauve-shrimp",
          name: "crabbox-mauve-shrimp",
          location: "eastus",
          properties: {
            provisioningState: "Succeeded",
            hardwareProfile: { vmSize: "Standard_D2as_v6" },
            storageProfile: {
              osDisk: {
                name: "crabbox-mauve-shrimp-osdisk",
                osType: "Linux",
                caching: "ReadWrite",
                createOption: "FromImage",
                diffDiskSettings: { option: "FutureSchemaValue" },
                managedDisk: {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-mauve-shrimp-osdisk",
                  storageAccountType: "StandardSSD_LRS",
                },
              },
            },
          },
        });
      }
      if (method === "PUT" && url.pathname.endsWith("/snapshots/chk-future-schema")) {
        return Response.json({
          id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/snapshots/chk-future-schema",
          name: "chk-future-schema",
          location: "eastus",
          properties: { provisioningState: "Succeeded" },
        });
      }
      return new Response("unexpected request", { status: 500 });
    };

    const image = await client.createDiskSnapshot("crabbox-mauve-shrimp", "chk-future-schema");
    expect(image).toMatchObject({ provider: "azure", kind: "azure-os-disk-snapshot" });
    expect(
      calls.some(
        (call) => call.method === "PUT" && call.pathname.endsWith("/snapshots/chk-future-schema"),
      ),
    ).toBe(true);
  });

  it("continues deleting per-lease resources after a delete failure", async () => {
    const client = new AzureClient(baseEnv);
    const deletes: string[] = [];
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input.toString();
      if (isAzureLoginURL(url)) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (init?.method === "DELETE") {
        deletes.push(url);
        if (url.includes("/virtualMachines/crabbox-blue-lobster?")) {
          return Promise.resolve(new Response("busy", { status: 409 }));
        }
        if (url.includes("/networkInterfaces/crabbox-blue-lobster-nic?")) {
          return Promise.resolve(new Response("missing", { status: 404 }));
        }
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    }) as typeof fetch;
    client.fetcher = fakeFetch;

    await expect(client.deleteServer("crabbox-blue-lobster")).rejects.toThrow(/delete vm/);
    expect(deletes.some((url) => url.includes("/virtualMachines/crabbox-blue-lobster?"))).toBe(
      true,
    );
    expect(
      deletes.some((url) => url.includes("/networkInterfaces/crabbox-blue-lobster-nic?")),
    ).toBe(true);
    expect(
      deletes.some((url) => url.includes("/publicIPAddresses/crabbox-blue-lobster-pip?")),
    ).toBe(true);
    expect(deletes.some((url) => url.includes("/disks/crabbox-blue-lobster-osdisk?"))).toBe(true);
  });

  it("treats successful async Azure deletes as complete without refetching deleted resources", async () => {
    const client = new AzureClient(baseEnv);
    const deletes: string[] = [];
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input.toString();
      if (isAzureLoginURL(url)) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (init?.method === "DELETE") {
        deletes.push(url);
        return Promise.resolve(new Response(null, { status: 202 }));
      }
      if (
        url.includes("/virtualMachines/") ||
        url.includes("/networkInterfaces/") ||
        url.includes("/publicIPAddresses/") ||
        url.includes("/disks/")
      ) {
        return Promise.resolve(new Response("deleted", { status: 404 }));
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    }) as typeof fetch;
    client.fetcher = fakeFetch;

    await expect(client.deleteServer("crabbox-blue-lobster")).resolves.toBeUndefined();
    expect(deletes).toHaveLength(4);
  });

  it("requires the four Azure SP secrets", () => {
    expect(() => new AzureClient({ ...baseEnv, AZURE_TENANT_ID: undefined })).toThrow(
      /AZURE_TENANT_ID/,
    );
    expect(() => new AzureClient({ ...baseEnv, AZURE_CLIENT_ID: undefined })).toThrow(
      /AZURE_CLIENT_ID/,
    );
    expect(() => new AzureClient({ ...baseEnv, AZURE_CLIENT_SECRET: undefined })).toThrow(
      /AZURE_CLIENT_SECRET/,
    );
    expect(() => new AzureClient({ ...baseEnv, AZURE_SUBSCRIPTION_ID: undefined })).toThrow(
      /AZURE_SUBSCRIPTION_ID/,
    );
  });

  it("applies CRABBOX_AZURE_* defaults", () => {
    const client = new AzureClient(baseEnv);
    expect(client.resourceGroup).toBe("crabbox-leases");
    expect(client.vnet).toBe("crabbox-vnet");
    expect(client.subnet).toBe("crabbox-subnet");
    expect(client.nsg).toBe("crabbox-nsg");
    expect(client.image).toContain("Canonical");
    expect(client.sshCIDRs).toEqual(["0.0.0.0/0"]);
    expect(client.defaultLocation).toBe("eastus");
  });

  it("creates Windows VMs with Windows OS profile and bootstrap extension", async () => {
    const client = new AzureClient(baseEnv);
    const bodies: unknown[] = [];
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input.toString();
      if (isAzureLoginURL(url)) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (init?.body) bodies.push(JSON.parse(String(init.body)));
      if (url.includes("/resourceGroups/crabbox-leases?")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (url.includes("/virtualNetworks/crabbox-vnet?")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (url.includes("/networkSecurityGroups/crabbox-nsg?") && init?.method === "GET") {
        return Promise.resolve(
          new Response(
            JSON.stringify({ tags: { managed_by: "crabbox" }, properties: { securityRules: [] } }),
            { status: 200 },
          ),
        );
      }
      if (url.includes("/providers/Microsoft.Compute/skus?")) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              value: [
                {
                  name: "Standard_D2ads_v6",
                  resourceType: "virtualMachines",
                  capabilities: [{ name: "EphemeralOSDiskSupported", value: "True" }],
                },
              ],
            }),
            { status: 200 },
          ),
        );
      }
      if (url.includes("/publicIPAddresses/") && init?.method === "GET") {
        return Promise.resolve(
          new Response(JSON.stringify({ properties: { ipAddress: "192.0.2.10" } }), {
            status: 200,
          }),
        );
      }
      if (url.includes("/virtualMachines/") && init?.method === "GET") {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              name: "crabbox-blue-lobster",
              tags: { crabbox: "true" },
              properties: {
                provisioningState: "Succeeded",
                hardwareProfile: { vmSize: "Standard_D2ads_v6" },
              },
            }),
            { status: 200 },
          ),
        );
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    }) as typeof fetch;
    client.fetcher = fakeFetch;
    const config: LeaseConfig = {
      provider: "azure",
      target: "windows",
      windowsMode: "normal",
      desktop: false,
      desktopEnv: "xfce",
      browser: false,
      code: false,
      tailscale: false,
      tailscaleTags: ["tag:crabbox"],
      tailscaleHostname: "",
      tailscaleAuthKey: "",
      tailscaleExitNode: "",
      tailscaleExitNodeAllowLanAccess: false,
      profile: "default",
      class: "standard",
      serverType: "Standard_D2ads_v6",
      serverTypeExplicit: true,
      location: "fsn1",
      image: "ubuntu-24.04",
      awsRegion: "eu-west-1",
      awsAMI: "",
      awsSnapshot: "",
      awsSGID: "",
      awsSubnetID: "",
      awsProfile: "",
      awsRootGB: 400,
      awsSSHCIDRs: [],
      awsMacHostID: "",
      azureLocation: "eastus",
      azureImage: "Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest",
      azureSnapshot: "",
      azureOSDisk: "managed",
      gcpProject: "",
      gcpZone: "",
      gcpImage: "",
      gcpMachineImage: "",
      gcpSnapshot: "",
      gcpNetwork: "",
      gcpSubnet: "",
      gcpTags: [],
      gcpSSHCIDRs: [],
      gcpRootGB: 0,
      gcpServiceAccount: "",
      capacityMarket: "spot",
      capacityStrategy: "most-available",
      capacityFallback: "on-demand-after-120s",
      capacityRegions: [],
      capacityAvailabilityZones: [],
      capacityHints: true,
      sshUser: "crabbox",
      sshPort: "2222",
      sshFallbackPorts: ["22"],
      providerKey: "crabbox-cbx",
      workRoot: "C:\\crabbox",
      ttlSeconds: 5400,
      idleTimeoutSeconds: 1800,
      keep: false,
      sshPublicKey: "ssh-rsa test",
    };
    await client.createServerWithFallback(config, "cbx_123456789abc", "blue-lobster", "owner");

    const vmBody = bodies.find(
      (body): body is { properties: { osProfile: Record<string, unknown> } } =>
        typeof body === "object" &&
        body !== null &&
        "properties" in body &&
        JSON.stringify(body).includes("windowsConfiguration"),
    );
    expect(vmBody?.properties.osProfile).toMatchObject({
      computerName: "cbxcbx123456789",
      adminUsername: "crabadmin",
      allowExtensionOperations: true,
      windowsConfiguration: { provisionVMAgent: true, enableAutomaticUpdates: false },
    });
    expect(vmBody?.properties).toMatchObject({
      priority: "Spot",
      evictionPolicy: "Delete",
      billingProfile: { maxPrice: -1 },
    });
    expect(String(vmBody?.properties.osProfile.customData ?? "")).toBeTruthy();
    expect(JSON.stringify(vmBody)).toContain("MicrosoftWindowsServer");
    const extensionBody = bodies.find((body) =>
      JSON.stringify(body).includes("CustomScriptExtension"),
    );
    expect(JSON.stringify(extensionBody)).toContain("AzureData\\\\CustomData.bin");
  });

  it("uses managed StandardSSD_LRS OS disks when azureOSDisk is managed", async () => {
    const client = new AzureClient(baseEnv);
    const bodies: unknown[] = [];
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input.toString();
      if (isAzureLoginURL(url)) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (init?.body) bodies.push(JSON.parse(String(init.body)));
      if (url.includes("/resourceGroups/crabbox-leases?")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (url.includes("/virtualNetworks/crabbox-vnet?")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (url.includes("/networkSecurityGroups/crabbox-nsg?") && init?.method === "GET") {
        return Promise.resolve(
          new Response(
            JSON.stringify({ tags: { managed_by: "crabbox" }, properties: { securityRules: [] } }),
            { status: 200 },
          ),
        );
      }
      if (url.includes("/providers/Microsoft.Compute/skus?")) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              value: [
                {
                  name: "Standard_D2ads_v6",
                  resourceType: "virtualMachines",
                  capabilities: [{ name: "EphemeralOSDiskSupported", value: "True" }],
                },
              ],
            }),
            { status: 200 },
          ),
        );
      }
      if (url.includes("/publicIPAddresses/") && init?.method === "GET") {
        return Promise.resolve(
          new Response(JSON.stringify({ properties: { ipAddress: "192.0.2.10" } }), {
            status: 200,
          }),
        );
      }
      if (url.includes("/virtualMachines/") && init?.method === "GET") {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              name: "crabbox-blue-lobster",
              tags: { crabbox: "true" },
              properties: {
                provisioningState: "Succeeded",
                hardwareProfile: { vmSize: "Standard_D2ads_v6" },
              },
            }),
            { status: 200 },
          ),
        );
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    }) as typeof fetch;
    client.fetcher = fakeFetch;

    await client.createServerWithFallback(
      testLeaseConfig({ azureOSDisk: "managed", serverType: "Standard_D2ads_v6" }),
      "cbx_123456789abc",
      "blue-lobster",
      "owner",
    );

    const vmBody = bodies.find(
      (body): body is { properties: { storageProfile: { osDisk: Record<string, unknown> } } } =>
        typeof body === "object" &&
        body !== null &&
        "properties" in body &&
        JSON.stringify(body).includes("storageProfile") &&
        JSON.stringify(body).includes("osDisk"),
    );
    expect(vmBody?.properties.storageProfile.osDisk).toMatchObject({
      caching: "ReadWrite",
      managedDisk: { storageAccountType: "StandardSSD_LRS" },
    });
    expect(vmBody?.properties.storageProfile.osDisk.diffDiskSettings).toBeUndefined();
  });

  it("rejects azureOSDisk=ephemeral when the selected SKU cannot support it", async () => {
    const client = new AzureClient(baseEnv);
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input.toString();
      if (isAzureLoginURL(url)) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (url.includes("/resourceGroups/crabbox-leases?")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (url.includes("/virtualNetworks/crabbox-vnet?")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (url.includes("/networkSecurityGroups/crabbox-nsg?") && init?.method === "GET") {
        return Promise.resolve(
          new Response(
            JSON.stringify({ tags: { managed_by: "crabbox" }, properties: { securityRules: [] } }),
            { status: 200 },
          ),
        );
      }
      if (url.includes("/providers/Microsoft.Compute/skus?")) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              value: [
                {
                  name: "Standard_D2as_v6",
                  resourceType: "virtualMachines",
                  capabilities: [{ name: "EphemeralOSDiskSupported", value: "False" }],
                },
              ],
            }),
            { status: 200 },
          ),
        );
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    }) as typeof fetch;
    client.fetcher = fakeFetch;

    await expect(
      client.createServerWithFallback(
        testLeaseConfig({ azureOSDisk: "ephemeral", serverType: "Standard_D2as_v6" }),
        "cbx_123456789abc",
        "blue-lobster",
        "owner",
      ),
    ).rejects.toThrow(/azureOSDisk=ephemeral requires/);
  });

  it("installs an SSH key extension when forking Linux VMs from OS disk snapshots", async () => {
    const client = new AzureClient(baseEnv);
    const bodies: unknown[] = [];
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input.toString();
      const pathname = new URL(url).pathname;
      if (isAzureLoginURL(url)) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (init?.body) bodies.push(JSON.parse(String(init.body)));
      if (pathname.endsWith("/resourceGroups/crabbox-leases")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (pathname.endsWith("/virtualNetworks/crabbox-vnet")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (pathname.endsWith("/networkSecurityGroups/crabbox-nsg") && init?.method === "GET") {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              tags: { managed_by: "crabbox" },
              properties: { securityRules: [] },
            }),
            { status: 200 },
          ),
        );
      }
      if (url.includes("/publicIPAddresses/") && init?.method === "GET") {
        return Promise.resolve(
          new Response(JSON.stringify({ properties: { ipAddress: "192.0.2.10" } }), {
            status: 200,
          }),
        );
      }
      if (url.includes("/virtualMachines/") && init?.method === "GET") {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              name: "crabbox-blue-lobster",
              tags: { crabbox: "true" },
              properties: {
                provisioningState: "Succeeded",
                hardwareProfile: { vmSize: "Standard_D2ads_v6" },
              },
            }),
            { status: 200 },
          ),
        );
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    }) as typeof fetch;
    client.fetcher = fakeFetch;

    await client.createServerWithFallback(
      testLeaseConfig({
        azureSnapshot:
          "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/snapshots/checkpoint-azure",
        capacityMarket: "on-demand",
        sshPublicKey: "ssh-ed25519 snapshot-key",
      }),
      "cbx_123456789abc",
      "blue-lobster",
      "owner",
    );

    const vmBody = bodies.find(
      (body): body is { properties: { osProfile?: unknown; storageProfile?: unknown } } =>
        typeof body === "object" &&
        body !== null &&
        "properties" in body &&
        JSON.stringify(body).includes("Attach"),
    );
    expect(vmBody?.properties.osProfile).toBeUndefined();
    const extensionBody = bodies.find((body) => JSON.stringify(body).includes("authorized_keys"));
    expect(extensionBody).toMatchObject({
      properties: {
        publisher: "Microsoft.Azure.Extensions",
        type: "CustomScript",
      },
    });
    expect(JSON.stringify(extensionBody)).toContain("ssh-ed25519 snapshot-key");
  });

  it("honors CRABBOX_AZURE_* overrides", () => {
    const client = new AzureClient({
      ...baseEnv,
      CRABBOX_AZURE_RESOURCE_GROUP: "custom-rg",
      CRABBOX_AZURE_LOCATION: "westus2",
      CRABBOX_AZURE_OS_DISK: "managed",
      CRABBOX_AZURE_SSH_CIDRS: "10.0.0.0/8, 192.168.0.0/16",
    });
    expect(client.resourceGroup).toBe("custom-rg");
    expect(client.defaultLocation).toBe("westus2");
    expect(client.sshCIDRs).toEqual(["10.0.0.0/8", "192.168.0.0/16"]);
  });

  it("deduplicates Azure NSG rules for repeated SSH ports", async () => {
    const client = new AzureClient(baseEnv);
    let nsgBody: { properties?: { securityRules?: Array<{ name?: string }> } } | undefined;
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input.toString();
      if (isAzureLoginURL(url)) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (url.includes("/resourceGroups/crabbox-leases?")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (url.includes("/virtualNetworks/crabbox-vnet?")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (url.includes("/networkSecurityGroups/crabbox-nsg?") && init?.method === "GET") {
        return Promise.resolve(
          new Response(
            JSON.stringify({ tags: { managed_by: "crabbox" }, properties: { securityRules: [] } }),
            { status: 200 },
          ),
        );
      }
      if (url.includes("/networkSecurityGroups/crabbox-nsg?") && init?.method === "PUT") {
        nsgBody = JSON.parse(String(init.body));
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    }) as typeof fetch;
    client.fetcher = fakeFetch;

    await client.ensureSharedInfra(
      "eastus",
      testLeaseConfig({ sshPort: "2222", sshFallbackPorts: ["22", "2222", "2022", "22"] }),
    );

    const names = nsgBody?.properties?.securityRules?.map((rule) => rule.name) ?? [];
    expect(names).toEqual(["crabbox-ssh-2222-0", "crabbox-ssh-22-0", "crabbox-ssh-2022-0"]);
    expect(new Set(names).size).toBe(names.length);
  });

  it("uses regional shared network names when defaults already exist elsewhere", async () => {
    const client = new AzureClient({ ...baseEnv, CRABBOX_AZURE_LOCATION: "westus3" });
    let puts: string[] = [];
    let nicBody:
      | {
          properties?: {
            ipConfigurations?: Array<{ properties?: { subnet?: { id?: string } } }>;
            networkSecurityGroup?: { id?: string };
          };
        }
      | undefined;
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input.toString();
      const pathname = new URL(url).pathname;
      if (isAzureLoginURL(url)) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (pathname.endsWith("/resourceGroups/crabbox-leases")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (
        init?.method === "GET" &&
        (pathname.endsWith("/virtualNetworks/crabbox-vnet") ||
          pathname.endsWith("/networkSecurityGroups/crabbox-nsg"))
      ) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              location: "eastus",
              tags: { managed_by: "crabbox" },
              properties: { securityRules: [] },
            }),
            { status: 200 },
          ),
        );
      }
      if (
        init?.method === "GET" &&
        (pathname.endsWith("/virtualNetworks/crabbox-vnet-westus3") ||
          pathname.endsWith("/networkSecurityGroups/crabbox-nsg-westus3"))
      ) {
        return Promise.resolve(new Response(JSON.stringify({ error: "missing" }), { status: 404 }));
      }
      if (init?.method === "PUT") puts.push(pathname);
      if (
        init?.method === "PUT" &&
        pathname.includes("/networkInterfaces/") &&
        pathname.endsWith("-nic")
      ) {
        nicBody = JSON.parse(String(init.body));
      }
      if (init?.method === "GET" && pathname.includes("/publicIPAddresses/")) {
        return Promise.resolve(
          new Response(JSON.stringify({ properties: { ipAddress: "192.0.2.10" } }), {
            status: 200,
          }),
        );
      }
      if (init?.method === "GET" && pathname.includes("/virtualMachines/")) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              name: pathname.split("/").pop() ?? "crabbox-blue-lobster",
              tags: { crabbox: "true" },
              properties: {
                provisioningState: "Succeeded",
                hardwareProfile: { vmSize: "Standard_D2ads_v6" },
              },
            }),
            { status: 200 },
          ),
        );
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    }) as typeof fetch;
    client.fetcher = fakeFetch;

    const infra = await client.ensureSharedInfra("westus3", testLeaseConfig());

    expect(infra).toEqual({ vnet: "crabbox-vnet-westus3", nsg: "crabbox-nsg-westus3" });
    expect(client.vnet).toBe("crabbox-vnet");
    expect(client.nsg).toBe("crabbox-nsg");
    expect(puts.some((path) => path.endsWith("/virtualNetworks/crabbox-vnet-westus3"))).toBe(true);
    expect(puts.some((path) => path.endsWith("/networkSecurityGroups/crabbox-nsg-westus3"))).toBe(
      true,
    );

    await client.createServerWithFallback(
      testLeaseConfig({ azureLocation: "westus3", capacityMarket: "on-demand" }),
      "cbx_123456789abc",
      "blue-lobster",
      "owner",
    );

    expect(nicBody?.properties?.ipConfigurations?.[0]?.properties?.subnet?.id).toContain(
      "/virtualNetworks/crabbox-vnet-westus3/subnets/",
    );
    expect(nicBody?.properties?.networkSecurityGroup?.id).toContain(
      "/networkSecurityGroups/crabbox-nsg-westus3",
    );

    puts = [];
    await client.ensureSharedInfra("eastus", testLeaseConfig());

    expect(puts.some((path) => path.endsWith("/networkSecurityGroups/crabbox-nsg"))).toBe(true);
    expect(puts.some((path) => path.includes("crabbox-nsg-westus3-eastus"))).toBe(false);
  });

  it("caches the client_credentials token across calls", async () => {
    const client = new AzureClient(baseEnv);
    let tokenMints = 0;
    const fakeFetch = ((input: RequestInfo | URL, _init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input.toString();
      if (isAzureLoginURL(url)) {
        tokenMints += 1;
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), {
            status: 200,
          }),
        );
      }
      return Promise.resolve(new Response(JSON.stringify({ value: [] }), { status: 200 }));
    }) as typeof fetch;
    client.fetcher = fakeFetch;
    await client.listCrabboxServers();
    await client.listCrabboxServers();
    expect(tokenMints).toBe(1);
  });

  it("uses a conservative Azure LRO polling floor to stay under Worker subrequest limits", () => {
    expect(azureLROPollIntervalMS(null)).toBe(15_000);
    expect(azureLROPollIntervalMS("3")).toBe(15_000);
    expect(azureLROPollIntervalMS("30")).toBe(30_000);
  });

  it("drops crabbox-ssh-* rules and preserves operator rules", () => {
    const kept = preserveNonCrabboxRules([
      { name: "crabbox-ssh-2222-0", properties: { destinationPortRange: "2222" } },
      { name: "operator-https", properties: { destinationPortRange: "443" } },
    ]);
    expect(kept).toEqual([{ name: "operator-https", properties: { destinationPortRange: "443" } }]);
  });

  it("uses a conservative ephemeral OS disk fallback", () => {
    expect(azureSupportsEphemeralOS("Standard_D2as_v5")).toBe(false);
    expect(azureSupportsEphemeralOS("Standard_D2s_v5")).toBe(false);
    expect(azureSupportsEphemeralOS("Standard_D2ads_v5")).toBe(true);
    expect(azureSupportsEphemeralOS("Standard_D2ads_v6")).toBe(true);
    expect(azureSupportsEphemeralOS("Standard_F2s_v2")).toBe(true);
    expect(azureSupportsEphemeralOS("Standard_D48ads_v6")).toBe(true);
    expect(azureSupportsEphemeralOS("Standard_F48s_v2")).toBe(true);
  });

  it("filters listCrabboxServers by crabbox=true tag", async () => {
    const client = new AzureClient(baseEnv);
    const fakeFetch = ((input: RequestInfo | URL, _init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input.toString();
      if (isAzureLoginURL(url)) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (url.includes("/virtualMachines?")) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              value: [
                {
                  name: "kept",
                  tags: { crabbox: "true" },
                  properties: { provisioningState: "Succeeded" },
                },
                {
                  name: "stranger",
                  tags: { other: "thing" },
                  properties: { provisioningState: "Succeeded" },
                },
              ],
            }),
            { status: 200 },
          ),
        );
      }
      if (url.includes("/publicIPAddresses/kept-pip?")) {
        return Promise.resolve(
          new Response(JSON.stringify({ properties: { ipAddress: "1.2.3.4" } }), { status: 200 }),
        );
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    }) as typeof fetch;
    client.fetcher = fakeFetch;
    const machines = await client.listCrabboxServers();
    expect(machines).toHaveLength(1);
    expect(machines[0]?.name).toBe("kept");
    expect(machines[0]?.host).toBe("1.2.3.4");
  });
});
