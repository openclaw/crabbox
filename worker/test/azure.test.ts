import { afterEach, describe, expect, it, vi } from "vitest";

import {
  AzureClient,
  azureLinuxCloudInit,
  azureLabelsFromTags,
  azureLROPollIntervalMS,
  azureProvisioningErrorCategory,
  azureRegionCandidates,
  azureRegionalName,
  azureSnapshotNotFound,
  azureSpotFallbackTimeoutMs,
  azureSupportsEphemeralFullCaching,
  azureSupportsEphemeralOS,
  azureTagsFromLabels,
  conciseAzureProvisioningMessage,
  isRetryableDeleteError,
  isRetryableProvisioningError,
  preserveNonCrabboxRules,
  summarizeAzureErrorBody,
  type AzureOwnedDeleteClaimStorage,
  type AzureDeferredCleanupRequest,
} from "../src/azure";
import type { LeaseConfig } from "../src/config";
import { providerProvisioningCleanupClaim } from "../src/provider-provisioning";
import type { Env, LeaseRecord } from "../src/types";

const baseEnv: Env = {
  FLEET: {} as DurableObjectNamespace,
  HETZNER_TOKEN: "",
  AZURE_TENANT_ID: "tenant",
  AZURE_CLIENT_ID: "client",
  AZURE_CLIENT_SECRET: "secret",
  AZURE_SUBSCRIPTION_ID: "sub",
};

afterEach(() => vi.useRealTimers());

it("preserves explicit provider scope when creating a regional client", () => {
  const client = new AzureClient(baseEnv, {
    subscription: "pinned-subscription",
    resourceGroup: "pinned-resource-group",
  });
  const regional = (
    Reflect.get(client, "clientForLocation") as (
      location: string,
      multiRegion: boolean,
    ) => AzureClient
  ).call(client, "westus", true);
  expect(regional.subscription).toBe("pinned-subscription");
  expect(regional.resourceGroup).toBe("pinned-resource-group");
});

it("installs pinned TruffleHog once in Azure Linux cloud-init", () => {
  const got = azureLinuxCloudInit(testLeaseConfig());

  expect(got).toContain(
    "trufflehog/releases/download/v${trufflehog_version}/${trufflehog_archive}",
  );
  expect(got).toContain("f6d1106b85107d79527ed7a5b98b592beadd8b770dc3c9e8c1ad99e1b2cf127e");
  expect(got).toContain("9d9c2ec4ea36a089a9c5aaafe1969d176013ddf9f44d68e8cd75291aed8c83ed");
  expect(got).toContain("trufflehog --no-update --version");
  expect(got.indexOf("sha256sum -c -")).toBeLessThan(got.indexOf("tar --no-same-owner"));
  expect(got.indexOf('trufflehog_candidate="$(mktemp')).toBeLessThan(
    got.indexOf('mv -f "$trufflehog_candidate" /usr/local/bin/trufflehog'),
  );
});

function isAzureLoginURL(value: string): boolean {
  return new URL(value).hostname === "login.microsoftonline.com";
}

function seedAzureAuthCache(
  client: AzureClient,
  token = baseEnv.AZURE_CLIENT_ID,
  expiresAt = Date.now() + 3_600_000,
): void {
  Reflect.set(Reflect.get(client, "tokenCache"), "cached", { token, expiresAt });
}

function ownedAzureLease(): Pick<
  LeaseRecord,
  "id" | "slug" | "provider" | "cloudID" | "owner" | "providerScope"
> {
  return {
    id: "cbx_abcdef123456",
    slug: "blue-lobster",
    provider: "azure",
    cloudID: "crabbox-blue-lobster",
    owner: "alice@example.com",
    providerScope: "/subscriptions/sub/resourceGroups/crabbox-leases",
  };
}

function ownedAzureTags(overrides: Record<string, string> = {}): Record<string, string> {
  return {
    crabbox: "true",
    created_by: "crabbox",
    lease: "cbx_abcdef123456",
    owner: "alice_example.com",
    provider: "azure",
    slug: "blue-lobster",
    ...overrides,
  };
}

function memoryAzureDeleteClaimStorage(): {
  records: Map<string, unknown>;
  putKeys: string[];
  storage: AzureOwnedDeleteClaimStorage;
} {
  const records = new Map<string, unknown>();
  const putKeys: string[] = [];
  let transactionTail = Promise.resolve();
  const storage: AzureOwnedDeleteClaimStorage = {
    get: async <T>(key: string) => records.get(key) as T | undefined,
    put: async <T>(key: string, value: T) => {
      putKeys.push(key);
      records.set(key, value);
    },
    delete: async (key: string) => {
      records.delete(key);
    },
    transaction: async (callback) => {
      const previous = transactionTail;
      let release!: () => void;
      transactionTail = new Promise<void>((resolve) => {
        release = resolve;
      });
      await previous;
      try {
        return await callback(storage);
      } finally {
        release();
      }
    },
  };
  return {
    records,
    putKeys,
    storage,
  };
}

function azureResourceNotFoundResponse(url: URL): Response {
  const resource = url.pathname.slice(url.pathname.indexOf("/providers/") + 11);
  return new Response(
    JSON.stringify({
      error: {
        code: "ResourceNotFound",
        message: `The Resource '${resource}' was not found.`,
      },
    }),
    { status: 404 },
  );
}

function azurePIPDiskCleanupClient(input: {
  storage: AzureOwnedDeleteClaimStorage;
  deleted: Set<string>;
  deletes: string[];
  failDelete: () => "pip" | "disk" | undefined;
  beforeDelete?: (path: string) => Promise<void>;
  beforeRead?: (path: string) => Promise<void>;
}): { client: AzureClient; nicID: string; pipID: string; diskID: string } {
  const nicID =
    "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic";
  const pipID =
    "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip";
  const diskID =
    "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk";
  const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: input.storage });
  seedAzureAuthCache(client);
  client.fetcher = async (request, init) => {
    const url = new URL(String(request));
    if (init?.method === "DELETE") {
      input.deletes.push(url.pathname);
      await input.beforeDelete?.(url.pathname);
      const kind = url.pathname === pipID ? "pip" : url.pathname === diskID ? "disk" : undefined;
      if (kind && input.failDelete() === kind) {
        return new Response(`injected ${kind} interruption`, { status: 400 });
      }
      input.deleted.add(url.pathname);
      return new Response(null, { status: 204 });
    }
    const deletedAtReadStart = input.deleted.has(url.pathname);
    await input.beforeRead?.(url.pathname);
    if (deletedAtReadStart) return azureResourceNotFoundResponse(url);
    if (url.pathname === nicID) {
      return Response.json({
        id: nicID,
        name: "crabbox-blue-lobster-nic",
        location: "eastus",
        tags: ownedAzureTags(),
        properties: {
          resourceGuid: "nic-immutable-id",
          ipConfigurations: [{ properties: { publicIPAddress: { id: pipID } } }],
        },
      });
    }
    if (url.pathname === pipID) {
      return Response.json({
        id: pipID,
        name: "crabbox-blue-lobster-pip",
        location: "eastus",
        tags: ownedAzureTags(),
        properties: { resourceGuid: "pip-immutable-id" },
      });
    }
    if (url.pathname === diskID) {
      return Response.json({
        id: diskID,
        name: "crabbox-blue-lobster-osdisk",
        location: "eastus",
        tags: ownedAzureTags(),
        properties: { uniqueId: "disk-immutable-id" },
      });
    }
    return azureResourceNotFoundResponse(url);
  };
  return { client, nicID, pipID, diskID };
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
  it("treats only an exact missing VM as absent", async () => {
    const client = new AzureClient(baseEnv);
    let body = JSON.stringify({
      error: {
        code: "ResourceNotFound",
        message:
          "The Resource 'Microsoft.Compute/virtualMachines/crabbox-blue-lobster' was not found.",
      },
    });
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      if (isAzureLoginURL(url.toString())) {
        return Response.json({ access_token: "tkn", expires_in: 3600 });
      }
      return new Response(body, { status: 404 });
    };

    await expect(client.findServer("crabbox-blue-lobster")).resolves.toBeUndefined();
    body = JSON.stringify({
      error: { code: "ResourceGroupNotFound", message: "Resource group was not found." },
    });
    await expect(client.findServer("crabbox-blue-lobster")).rejects.toThrow(
      "ResourceGroupNotFound",
    );
  });

  it("recovers an exact Azure NIC and public IP set before VM creation", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const cloudID = "crabbox-blue-lobster";
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      if (url.pathname.endsWith("/virtualMachines") || url.pathname.endsWith("/disks")) {
        return Response.json({ value: [] });
      }
      if (url.pathname.endsWith("/networkInterfaces")) {
        return Response.json({
          value: [
            {
              id: `${url.pathname}/${cloudID}-nic`,
              name: `${cloudID}-nic`,
              location: "eastus",
              tags: ownedAzureTags({ server_type: "Standard_D4ads_v6" }),
              properties: {
                ipConfigurations: [
                  {
                    properties: {
                      publicIPAddress: {
                        id: `/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/${cloudID}-pip`,
                      },
                    },
                  },
                ],
              },
            },
          ],
        });
      }
      if (url.pathname.endsWith("/publicIPAddresses")) {
        return Response.json({
          value: [
            {
              id: `${url.pathname}/${cloudID}-pip`,
              name: `${cloudID}-pip`,
              location: "eastus",
              tags: ownedAzureTags({ server_type: "Standard_D4ads_v6" }),
              properties: { ipAddress: "192.0.2.20" },
            },
          ],
        });
      }
      return new Response("unexpected request", { status: 500 });
    };

    await expect(
      client.recoverServerForLease({
        ...ownedAzureLease(),
        serverType: "Standard_D2ads_v6",
        region: "eastus",
      }),
    ).resolves.toMatchObject({
      provider: "azure",
      cloudID,
      name: cloudID,
      status: "provisioning",
      serverType: "Standard_D4ads_v6",
      host: "192.0.2.20",
      region: "eastus",
      labels: {
        lease: "cbx_abcdef123456",
        owner: "alice_example.com",
      },
    });
  });

  it("returns no Azure recovery identity when no resource is owned by the lease", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      return Response.json({
        value: [
          {
            id: `${url.pathname}/unrelated`,
            name: "unrelated",
            tags: ownedAzureTags({
              lease: "cbx_000000000000",
              slug: "unrelated",
            }),
          },
        ],
      });
    };

    await expect(
      client.recoverServerForLease({
        ...ownedAzureLease(),
        serverType: "Standard_D2ads_v6",
        region: "eastus",
      }),
    ).resolves.toBeUndefined();
  });

  it("fails closed when multiple Azure resource sets claim the lease", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      if (!url.pathname.endsWith("/publicIPAddresses")) {
        return Response.json({ value: [] });
      }
      const secondPage = url.searchParams.has("$skiptoken");
      return Response.json({
        value: [secondPage ? "crabbox-blue-lobster-retry" : "crabbox-blue-lobster"].map(
          (cloudID) => ({
            id: `${url.pathname}/${cloudID}-pip`,
            name: `${cloudID}-pip`,
            tags: ownedAzureTags(),
          }),
        ),
        ...(secondPage
          ? {}
          : {
              nextLink:
                "https://management.azure.com/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses?api-version=2024-05-01&$skiptoken=page-2",
            }),
      });
    };

    await expect(
      client.recoverServerForLease({
        ...ownedAzureLease(),
        serverType: "Standard_D2ads_v6",
        region: "eastus",
      }),
    ).rejects.toThrow("ambiguous Azure recovery");
  });

  it("fails closed when Azure recovery pagination leaves the management scope", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      if (!url.pathname.endsWith("/publicIPAddresses")) {
        return Response.json({ value: [] });
      }
      return Response.json({
        value: [
          {
            id: `${url.pathname}/crabbox-blue-lobster-pip`,
            name: "crabbox-blue-lobster-pip",
            tags: ownedAzureTags(),
          },
        ],
        nextLink: "https://example.test/subscriptions/sub/resourceGroups/crabbox-leases",
      });
    };

    await expect(
      client.recoverServerForLease({
        ...ownedAzureLease(),
        serverType: "Standard_D2ads_v6",
        region: "eastus",
      }),
    ).rejects.toThrow("invalid nextLink");
  });

  it("fails closed when an Azure resource claims the lease with mismatched ownership", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      if (!url.pathname.endsWith("/publicIPAddresses")) {
        return Response.json({ value: [] });
      }
      return Response.json({
        value: [
          {
            id: `${url.pathname}/crabbox-blue-lobster-pip`,
            name: "crabbox-blue-lobster-pip",
            tags: ownedAzureTags({ owner: "mallory_example.com" }),
          },
        ],
      });
    };

    await expect(
      client.recoverServerForLease({
        ...ownedAzureLease(),
        serverType: "Standard_D2ads_v6",
        region: "eastus",
      }),
    ).rejects.toThrow("ownership does not match");
  });

  it("refuses owned cleanup when the persisted Azure scope differs", async () => {
    const client = new AzureClient(baseEnv);
    await expect(
      client.deleteOwnedServer({
        ...ownedAzureLease(),
        providerScope: "/subscriptions/other/resourceGroups/crabbox-leases",
      }),
    ).rejects.toThrow("stored provider scope does not match");
  });

  it("can bind an Azure client to a persisted scope instead of mutable defaults", () => {
    const client = new AzureClient(
      { ...baseEnv, AZURE_SUBSCRIPTION_ID: "current", CRABBOX_AZURE_RESOURCE_GROUP: "current-rg" },
      { subscription: "stored", resourceGroup: "stored-rg" },
    );
    expect(client.providerScope()).toBe("/subscriptions/stored/resourceGroups/stored-rg");
  });

  it("classifies Azure capacity and quota errors as retryable", () => {
    expect(isRetryableProvisioningError("SkuNotAvailable: D8s_v5 not available")).toBe(true);
    expect(isRetryableProvisioningError("QuotaExceeded for cores")).toBe(true);
    expect(isRetryableProvisioningError("AllocationFailed")).toBe(true);
    expect(isRetryableProvisioningError("OverconstrainedAllocationRequest")).toBe(true);
    expect(isRetryableProvisioningError("NotAvailableForSubscription")).toBe(true);
    expect(isRetryableProvisioningError("azure long-running operation timed out after 120s")).toBe(
      true,
    );
    expect(isRetryableProvisioningError("ResourceNotFound")).toBe(false);
    expect(isRetryableProvisioningError("")).toBe(false);
  });

  it("bounds Azure Spot provisioning even when on-demand fallback is disabled", () => {
    expect(azureSpotFallbackTimeoutMs(testLeaseConfig({ capacityFallback: "spot-only" }))).toBe(
      120_000,
    );
    expect(azureSpotFallbackTimeoutMs(testLeaseConfig({ capacityFallback: "none" }))).toBe(120_000);
    expect(
      azureSpotFallbackTimeoutMs(
        testLeaseConfig({ capacityFallback: "spot-only", capacityMarket: "on-demand" }),
      ),
    ).toBeUndefined();
  });

  it("uses the Worker Azure image env fallback when the lease has no image override", () => {
    const client = new AzureClient({
      ...baseEnv,
      CRABBOX_AZURE_IMAGE: "Canonical:custom-linux:image:latest",
    });
    expect(client.resolvedImageForConfig(testLeaseConfig({ azureImage: "" }))).toBe(
      "Canonical:custom-linux:image:latest",
    );
    expect(
      client.resolvedImageForConfig(
        testLeaseConfig({ azureImage: "Canonical:custom-linux:image:latest" }),
      ),
    ).toBe("Canonical:custom-linux:image:latest");
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
    expect(isRetryableDeleteError("DiskInUse while detaching")).toBe(true);
    expect(isRetryableDeleteError("DiskIsAttachedToVM")).toBe(true);
    expect(isRetryableDeleteError("CannotDeleteDisk until detach completes")).toBe(true);
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

  it("recognizes only the exact managed snapshot in normal Azure resource-not-found responses", async () => {
    const client = new AzureClient(baseEnv);
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (url.hostname === "login.microsoftonline.com") {
        return Response.json({ access_token: "tkn", expires_in: 3600 });
      }
      void init;
      return azureResourceNotFoundResponse(url);
    };
    let failure: unknown;
    try {
      await client.getImage("checkpoint-azure", "azure-os-disk-snapshot");
    } catch (error) {
      failure = error;
    }
    const exact =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/snapshots/checkpoint-azure";
    expect(azureSnapshotNotFound(failure, exact)).toBe(true);
    expect(azureSnapshotNotFound(failure, exact.replace("checkpoint-azure", "other"))).toBe(false);
    expect(azureSnapshotNotFound(failure, exact.replace("/snapshots/", "/virtualMachines/"))).toBe(
      false,
    );
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

  it("refuses to overwrite an existing coordinator-owned snapshot name", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const requests: Array<{ method: string; ifNoneMatch: string | null }> = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      requests.push({ method, ifNoneMatch: new Headers(init?.headers).get("if-none-match") });
      if (method === "GET" && url.pathname.includes("/virtualMachines/")) {
        return Response.json({
          location: "eastus",
          properties: {
            storageProfile: {
              osDisk: {
                managedDisk: {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/source-disk",
                },
              },
            },
          },
        });
      }
      return Response.json({ error: { code: "PreconditionFailed" } }, { status: 412 });
    };
    let refused: unknown;
    try {
      await client.createDiskSnapshot("source-vm", "existing-unrelated-snapshot", {
        checkpointID: "chk_conditional_create",
        tokenHash: "a".repeat(64),
        sourceLeaseID: "cbx_abcdef123456",
      });
    } catch (error) {
      refused = error;
    }
    expect(refused).toMatchObject({ checkpointResourceMayExist: false });
    expect(requests).toEqual([
      { method: "GET", ifNoneMatch: null },
      { method: "PUT", ifNoneMatch: "*" },
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

  it("verifies every Azure companion before deleting an owned lease", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client, "test-token");
    const deletes: string[] = [];
    const deleted = new Set<string>();
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      if (method === "DELETE") {
        deletes.push(url.pathname);
        deleted.add(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (deleted.has(url.pathname)) return azureResourceNotFoundResponse(url);
      if (url.pathname.endsWith("/virtualMachines/crabbox-blue-lobster")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            vmId: "vm-immutable-id",
            networkProfile: {
              networkInterfaces: [
                {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic",
                },
              ],
            },
            storageProfile: {
              osDisk: {
                managedDisk: {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk",
                },
              },
            },
          },
        });
      }
      if (url.pathname.endsWith("/networkInterfaces/crabbox-blue-lobster-nic")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-nic",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            resourceGuid: "nic-immutable-id",
            ipConfigurations: [
              {
                id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic/ipConfigurations/ipconfig1",
                properties: {
                  publicIPAddress: {
                    id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip",
                  },
                },
              },
            ],
          },
        });
      }
      if (url.pathname.endsWith("/publicIPAddresses/crabbox-blue-lobster-pip")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-pip",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            resourceGuid: "pip-immutable-id",
            ipConfiguration: {
              id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic/ipConfigurations/ipconfig1",
            },
          },
        });
      }
      if (url.pathname.endsWith("/disks/crabbox-blue-lobster-osdisk")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-osdisk",
          location: "eastus",
          managedBy:
            "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-blue-lobster",
          tags: ownedAzureTags(),
          properties: { uniqueId: "disk-unique-id" },
        });
      }
      return new Response("unexpected request", { status: 500 });
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).resolves.toBeUndefined();
    expect(deletes).toHaveLength(4);
  });

  it("refuses to delete an Azure VM with an unexpected NIC reference", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname.endsWith("/virtualMachines/crabbox-blue-lobster")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster",
          tags: ownedAzureTags(),
          properties: {
            networkProfile: {
              networkInterfaces: [
                {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic",
                },
                {
                  id: "/subscriptions/sub/resourceGroups/shared/providers/Microsoft.Network/networkInterfaces/shared-nic",
                },
              ],
            },
          },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "VM references an unexpected NIC",
    );
    expect(deletes).toEqual([]);
  });

  it("refuses to delete an Azure VM when its exact canonical NIC is absent", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname.endsWith("/virtualMachines/crabbox-blue-lobster")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            vmId: "vm-immutable-id",
            networkProfile: {
              networkInterfaces: [
                {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic",
                },
              ],
            },
          },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "canonical NIC is missing",
    );
    expect(deletes).toEqual([]);
  });

  it("scopes durable Azure cleanup claims to the exact VM attempt", async () => {
    const { putKeys, storage } = memoryAzureDeleteClaimStorage();
    for (const cloudID of ["crabbox-blue-lobster-attempt-1", "crabbox-blue-lobster-attempt-2"]) {
      const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
      seedAzureAuthCache(client, "test-token");
      client.fetcher = async (input) => azureResourceNotFoundResponse(new URL(String(input)));
      // oxlint-disable-next-line eslint/no-await-in-loop -- preserve deterministic claim-write order for the assertion.
      await expect(
        client.deleteOwnedServer({ ...ownedAzureLease(), cloudID }),
      ).resolves.toBeUndefined();
    }

    expect(putKeys).toHaveLength(4);
    expect(new Set(putKeys).size).toBe(2);
  });

  it("cleans an exact Azure NIC, public IP, and managed disk set without a VM", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const deleted = new Set<string>();
    const deletes: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        deleted.add(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (deleted.has(url.pathname)) {
        return azureResourceNotFoundResponse(url);
      }
      if (url.pathname.endsWith("/networkInterfaces/crabbox-blue-lobster-nic")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-nic",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            resourceGuid: "nic-immutable-id",
            ipConfigurations: [
              {
                id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic/ipConfigurations/ipconfig1",
                properties: {
                  publicIPAddress: {
                    id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip",
                  },
                },
              },
            ],
          },
        });
      }
      if (url.pathname.endsWith("/publicIPAddresses/crabbox-blue-lobster-pip")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-pip",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            resourceGuid: "pip-immutable-id",
            ipConfiguration: {
              id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic/ipConfigurations/ipconfig1",
            },
          },
        });
      }
      if (url.pathname.endsWith("/disks/crabbox-blue-lobster-osdisk")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-osdisk",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: { uniqueId: "disk-unique-id" },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).resolves.toBeUndefined();
    expect(deletes).toEqual([
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic",
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip",
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk",
    ]);
  });

  it("claims, deletes, and clears an exact VM-less Azure NIC and public IP set", async () => {
    const { putKeys, records, storage } = memoryAzureDeleteClaimStorage();
    const writtenClaims: unknown[] = [];
    const put = storage.put;
    storage.put = async (key, value) => {
      writtenClaims.push(value);
      await put(key, value);
    };
    const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
    seedAzureAuthCache(client);
    const deleted = new Set<string>();
    const deletes: string[] = [];
    const nicID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic";
    const pipID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip";
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        deleted.add(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (deleted.has(url.pathname)) return azureResourceNotFoundResponse(url);
      if (url.pathname === nicID) {
        return Response.json({
          id: nicID,
          name: "crabbox-blue-lobster-nic",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            resourceGuid: "nic-immutable-id",
            ipConfigurations: [
              {
                id: `${nicID}/ipConfigurations/ipconfig1`,
                properties: { publicIPAddress: { id: pipID } },
              },
            ],
          },
        });
      }
      if (url.pathname === pipID) {
        return Response.json({
          id: pipID,
          name: "crabbox-blue-lobster-pip",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            resourceGuid: "pip-immutable-id",
            ipConfiguration: { id: `${nicID}/ipConfigurations/ipconfig1` },
          },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).resolves.toBeUndefined();
    expect(deletes).toEqual([nicID, pipID]);
    expect([...new Set(putKeys)].map((key) => key.split(":").map(decodeURIComponent))).toEqual([
      [
        "provider:azure:delete-claim",
        ownedAzureLease().providerScope,
        ownedAzureLease().cloudID,
        ownedAzureLease().id,
      ],
    ]);
    const preparedClaim = writtenClaims.find(
      (claim): claim is { stableResourceIdentity: string } =>
        typeof claim === "object" &&
        claim !== null &&
        "stableResourceIdentity" in claim &&
        typeof claim.stableResourceIdentity === "string",
    );
    expect(preparedClaim).toMatchObject({
      version: 2,
      provider: "azure",
      leaseID: ownedAzureLease().id,
      slug: ownedAzureLease().slug,
      owner: ownedAzureLease().owner,
      cloudID: ownedAzureLease().cloudID,
      providerScope: ownedAzureLease().providerScope,
    });
    expect(preparedClaim).not.toHaveProperty("disk");
    expect(JSON.parse(preparedClaim!.stableResourceIdentity)).toEqual([
      expect.objectContaining({
        kind: "networkInterfaces",
        id: nicID.toLowerCase(),
        immutableID: "nic-immutable-id",
      }),
      expect.objectContaining({
        kind: "publicIPAddresses",
        id: pipID.toLowerCase(),
        immutableID: "pip-immutable-id",
      }),
    ]);
    expect(records.size).toBe(0);
  });

  it("claims, deletes, and clears an exact Azure public IP before NIC creation", async () => {
    const { putKeys, records, storage } = memoryAzureDeleteClaimStorage();
    const writtenClaims: unknown[] = [];
    const put = storage.put;
    storage.put = async (key, value) => {
      writtenClaims.push(value);
      await put(key, value);
    };
    const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
    seedAzureAuthCache(client);
    const deleted = new Set<string>();
    const deletes: string[] = [];
    const pipID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip";
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        deleted.add(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (deleted.has(url.pathname)) return azureResourceNotFoundResponse(url);
      if (url.pathname === pipID) {
        return Response.json({
          id: pipID,
          name: "crabbox-blue-lobster-pip",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: { resourceGuid: "pip-immutable-id" },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).resolves.toBeUndefined();
    expect(deletes).toEqual([pipID]);
    expect([...new Set(putKeys)].map((key) => key.split(":").map(decodeURIComponent))).toEqual([
      [
        "provider:azure:delete-claim",
        ownedAzureLease().providerScope,
        ownedAzureLease().cloudID,
        ownedAzureLease().id,
      ],
    ]);
    const preparedClaim = writtenClaims.find(
      (claim): claim is { stableResourceIdentity: string } =>
        typeof claim === "object" &&
        claim !== null &&
        "stableResourceIdentity" in claim &&
        typeof claim.stableResourceIdentity === "string",
    );
    expect(preparedClaim).toMatchObject({
      version: 2,
      provider: "azure",
      leaseID: ownedAzureLease().id,
      cloudID: ownedAzureLease().cloudID,
      providerScope: ownedAzureLease().providerScope,
    });
    expect(JSON.parse(preparedClaim!.stableResourceIdentity)).toEqual([
      expect.objectContaining({
        kind: "publicIPAddresses",
        id: pipID.toLowerCase(),
        immutableID: "pip-immutable-id",
      }),
    ]);
    expect(records.size).toBe(0);
  });

  it("refuses a fresh Azure NIC without durable public IP deletion progress", async () => {
    const { records, storage } = memoryAzureDeleteClaimStorage();
    const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    const nicID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic";
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname === nicID) {
        return Response.json({
          id: nicID,
          name: "crabbox-blue-lobster-nic",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            resourceGuid: "nic-immutable-id",
            ipConfigurations: [],
          },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "canonical companion set is incomplete",
    );
    expect(deletes).toEqual([]);
    expect(records.size).toBe(1);
  });

  it.each([
    {
      scenario: "a public IP attached to a missing NIC",
      missing: "nic",
      expectedError: "public IP is attached to an unexpected configuration",
    },
    {
      scenario: "a missing public IP",
      missing: "pip",
      expectedError: "NIC references an unexpected public IP",
    },
    {
      scenario: "a public IP owned by another lease",
      foreignPublicIP: true,
      expectedError: "ownership does not match lease",
    },
    {
      scenario: "a NIC attached to another VM",
      foreignVMAttachment: true,
      expectedError: "NIC is attached to an unexpected VM",
    },
    {
      scenario: "a same-name public IP replacement after the claim is written",
      replacePublicIPAfterClaim: true,
      expectedError: "observed resource identity changed",
    },
  ])("refuses VM-less Azure cleanup with $scenario", async (testCase) => {
    const { records, storage } = memoryAzureDeleteClaimStorage();
    let publicIPIdentity = "pip-immutable-id";
    if (testCase.replacePublicIPAfterClaim) {
      const put = storage.put;
      storage.put = async (key, value) => {
        await put(key, value);
        if (typeof value === "object" && value !== null && "stableResourceIdentity" in value) {
          publicIPIdentity = "replacement-pip-immutable-id";
        }
      };
    }
    const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    const resourcePrefix = "/subscriptions/sub/resourceGroups/crabbox-leases/providers";
    const nicID = `${resourcePrefix}/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic`;
    const pipID = `${resourcePrefix}/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip`;
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname === nicID && testCase.missing !== "nic") {
        return Response.json({
          id: nicID,
          name: "crabbox-blue-lobster-nic",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            resourceGuid: "nic-immutable-id",
            ...(testCase.foreignVMAttachment
              ? {
                  virtualMachine: {
                    id: `${resourcePrefix}/Microsoft.Compute/virtualMachines/crabbox-other`,
                  },
                }
              : {}),
            ipConfigurations: [
              {
                id: `${nicID}/ipConfigurations/ipconfig`,
                properties: { publicIPAddress: { id: pipID } },
              },
            ],
          },
        });
      }
      if (url.pathname === pipID && testCase.missing !== "pip") {
        return Response.json({
          id: pipID,
          name: "crabbox-blue-lobster-pip",
          location: "eastus",
          tags: ownedAzureTags(testCase.foreignPublicIP ? { lease: "cbx_987654321abc" } : {}),
          properties: {
            resourceGuid: publicIPIdentity,
            ipConfiguration: { id: `${nicID}/ipConfigurations/ipconfig` },
          },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      testCase.expectedError,
    );
    expect(deletes).toEqual([]);
    expect(records.size).toBe(1);
  });

  it.each([
    ["public IP resourceGuid", "pip-guid"],
    ["public IP location", "pip-location"],
    ["network interface resourceGuid", "nic-guid"],
    ["network interface location", "nic-location"],
  ] as const)("refuses Azure network cleanup without stable %s", async (_label, missing) => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    const nicID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic";
    const pipID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip";
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname === nicID) {
        return Response.json({
          id: nicID,
          name: "crabbox-blue-lobster-nic",
          ...(missing === "nic-location" ? {} : { location: "eastus" }),
          tags: ownedAzureTags(),
          properties: {
            ...(missing === "nic-guid" ? {} : { resourceGuid: "nic-immutable-id" }),
            ipConfigurations: [
              {
                id: `${nicID}/ipConfigurations/ipconfig1`,
                properties: { publicIPAddress: { id: pipID } },
              },
            ],
          },
        });
      }
      if (url.pathname === pipID) {
        return Response.json({
          id: pipID,
          name: "crabbox-blue-lobster-pip",
          ...(missing === "pip-location" ? {} : { location: "eastus" }),
          tags: ownedAzureTags(),
          properties: {
            ...(missing === "pip-guid" ? {} : { resourceGuid: "pip-immutable-id" }),
            ipConfiguration: { id: `${nicID}/ipConfigurations/ipconfig1` },
          },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "stable resource identity is incomplete",
    );
    expect(deletes).toEqual([]);
  });

  it.each([
    ["VM immutable identity", "vm-id", "stable resource identity is incomplete"],
    ["VM location", "vm-location", "stable resource identity is incomplete"],
    ["managed disk immutable identity", "disk-id", "immutable identity is missing"],
    ["managed disk location", "disk-location", "stable resource identity is incomplete"],
  ] as const)(
    "refuses Azure VM cleanup without stable %s",
    async (_label, missing, expectedError) => {
      const { records, storage } = memoryAzureDeleteClaimStorage();
      const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
      seedAzureAuthCache(client);
      const deletes: string[] = [];
      const resourcePrefix = "/subscriptions/sub/resourceGroups/crabbox-leases/providers";
      const vmID = `${resourcePrefix}/Microsoft.Compute/virtualMachines/crabbox-blue-lobster`;
      const nicID = `${resourcePrefix}/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic`;
      const pipID = `${resourcePrefix}/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip`;
      const diskID = `${resourcePrefix}/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk`;
      client.fetcher = async (input, init) => {
        const url = new URL(String(input));
        if (init?.method === "DELETE") {
          deletes.push(url.pathname);
          return new Response(null, { status: 204 });
        }
        if (url.pathname === vmID) {
          return Response.json({
            id: vmID,
            name: "crabbox-blue-lobster",
            ...(missing === "vm-location" ? {} : { location: "eastus" }),
            tags: ownedAzureTags(),
            properties: {
              ...(missing === "vm-id" ? {} : { vmId: "vm-immutable-id" }),
              networkProfile: { networkInterfaces: [{ id: nicID }] },
              storageProfile: { osDisk: { managedDisk: { id: diskID } } },
            },
          });
        }
        if (url.pathname === nicID) {
          return Response.json({
            id: nicID,
            name: "crabbox-blue-lobster-nic",
            location: "eastus",
            tags: ownedAzureTags(),
            properties: {
              resourceGuid: "nic-immutable-id",
              virtualMachine: { id: vmID },
              ipConfigurations: [
                {
                  id: `${nicID}/ipConfigurations/ipconfig`,
                  properties: { publicIPAddress: { id: pipID } },
                },
              ],
            },
          });
        }
        if (url.pathname === pipID) {
          return Response.json({
            id: pipID,
            name: "crabbox-blue-lobster-pip",
            location: "eastus",
            tags: ownedAzureTags(),
            properties: {
              resourceGuid: "pip-immutable-id",
              ipConfiguration: { id: `${nicID}/ipConfigurations/ipconfig` },
            },
          });
        }
        if (url.pathname === diskID) {
          return Response.json({
            id: diskID,
            name: "crabbox-blue-lobster-osdisk",
            ...(missing === "disk-location" ? {} : { location: "eastus" }),
            managedBy: vmID,
            tags: ownedAzureTags(),
            properties: missing === "disk-id" ? {} : { uniqueId: "disk-immutable-id" },
          });
        }
        return azureResourceNotFoundResponse(url);
      };

      await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(expectedError);
      expect(deletes).toEqual([]);
      expect(records.size).toBe(1);
    },
  );

  it("refuses a same-name Azure public IP replacement without stable identity before delete", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    const pipID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip";
    let reads = 0;
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname === pipID) {
        reads += 1;
        return Response.json({
          id: pipID,
          name: "crabbox-blue-lobster-pip",
          tags: ownedAzureTags(),
          properties: {
            ipAddress: reads === 1 ? "203.0.113.10" : "203.0.113.11",
          },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "stable resource identity is incomplete",
    );
    expect(reads).toBe(1);
    expect(deletes).toEqual([]);
  });

  it("releases a VM with an ephemeral OS disk and no managed disk resource", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const deleted = new Set<string>();
    const deletes: string[] = [];
    const vmID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-blue-lobster";
    const nicID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic";
    const pipID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip";
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        deleted.add(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (deleted.has(url.pathname)) return azureResourceNotFoundResponse(url);
      if (url.pathname === vmID) {
        return Response.json({
          id: vmID,
          name: "crabbox-blue-lobster",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            vmId: "vm-immutable-id",
            networkProfile: { networkInterfaces: [{ id: nicID }] },
            storageProfile: { osDisk: { diffDiskSettings: { option: "Local" } } },
          },
        });
      }
      if (url.pathname === nicID) {
        return Response.json({
          id: nicID,
          name: "crabbox-blue-lobster-nic",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            resourceGuid: "nic-immutable-id",
            virtualMachine: { id: vmID },
            ipConfigurations: [
              {
                id: `${nicID}/ipConfigurations/ipconfig1`,
                properties: { publicIPAddress: { id: pipID } },
              },
            ],
          },
        });
      }
      if (url.pathname === pipID) {
        return Response.json({
          id: pipID,
          name: "crabbox-blue-lobster-pip",
          location: "eastus",
          tags: ownedAzureTags(),
          properties: {
            resourceGuid: "pip-immutable-id",
            ipConfiguration: { id: `${nicID}/ipConfigurations/ipconfig1` },
          },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).resolves.toBeUndefined();
    expect(deletes).toEqual([vmID, nicID, pipID]);
  });

  it("refuses to delete a VM-less Azure NIC attached to another VM", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname.endsWith("/networkInterfaces/crabbox-blue-lobster-nic")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-nic",
          tags: ownedAzureTags(),
          properties: {
            virtualMachine: {
              id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-other",
            },
            ipConfigurations: [
              {
                properties: {
                  publicIPAddress: {
                    id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip",
                  },
                },
              },
            ],
          },
        });
      }
      if (url.pathname.endsWith("/publicIPAddresses/crabbox-blue-lobster-pip")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-pip",
          tags: ownedAzureTags(),
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "NIC is attached to an unexpected VM",
    );
    expect(deletes).toEqual([]);
  });

  it("refuses to delete a VM-less Azure NIC referencing a foreign public IP", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname.endsWith("/networkInterfaces/crabbox-blue-lobster-nic")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-nic",
          tags: ownedAzureTags(),
          properties: {
            ipConfigurations: [
              {
                properties: {
                  publicIPAddress: {
                    id: "/subscriptions/sub/resourceGroups/shared/providers/Microsoft.Network/publicIPAddresses/shared-pip",
                  },
                },
              },
            ],
          },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "NIC references an unexpected public IP",
    );
    expect(deletes).toEqual([]);
  });

  it("refuses to delete a VM-less Azure NIC when its canonical public IP is missing", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    const pipID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip";
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname.endsWith("/networkInterfaces/crabbox-blue-lobster-nic")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-nic",
          tags: ownedAzureTags(),
          properties: {
            ipConfigurations: [
              {
                properties: {
                  publicIPAddress: { id: pipID },
                },
              },
            ],
          },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "NIC references an unexpected public IP",
    );
    expect(deletes).toEqual([]);
  });

  it("refuses to delete a VM-less Azure public IP attached to another resource", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname.endsWith("/publicIPAddresses/crabbox-blue-lobster-pip")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-pip",
          tags: ownedAzureTags(),
          properties: {
            ipConfiguration: {
              id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/loadBalancers/other/frontendIPConfigurations/frontend",
            },
          },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "public IP is attached to an unexpected configuration",
    );
    expect(deletes).toEqual([]);
  });

  it("refuses to delete a VM-less Azure public IP attached to a NAT gateway", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname.endsWith("/publicIPAddresses/crabbox-blue-lobster-pip")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-pip",
          tags: ownedAzureTags(),
          properties: {
            natGateway: {
              id: "/subscriptions/sub/resourceGroups/shared/providers/Microsoft.Network/natGateways/shared-nat",
            },
          },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "public IP is attached to an unexpected configuration",
    );
    expect(deletes).toEqual([]);
  });

  it.each(["linkedPublicIPAddress", "servicePublicIPAddress"] as const)(
    "refuses to delete a VM-less Azure public IP with a %s attachment",
    async (attachment) => {
      const client = new AzureClient(baseEnv);
      seedAzureAuthCache(client);
      const deletes: string[] = [];
      client.fetcher = async (input, init) => {
        const url = new URL(String(input));
        if (init?.method === "DELETE") {
          deletes.push(url.pathname);
          return new Response(null, { status: 204 });
        }
        if (url.pathname.endsWith("/publicIPAddresses/crabbox-blue-lobster-pip")) {
          return Response.json({
            id: url.pathname,
            name: "crabbox-blue-lobster-pip",
            tags: ownedAzureTags(),
            properties: {
              [attachment]: {
                id: "/subscriptions/sub/resourceGroups/shared/providers/Microsoft.Network/publicIPAddresses/shared-pip",
              },
            },
          });
        }
        return azureResourceNotFoundResponse(url);
      };

      await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
        "public IP is attached to an unexpected configuration",
      );
      expect(deletes).toEqual([]);
    },
  );

  it("keeps a lone fully tagged Azure disk report-only", async () => {
    const { storage } = memoryAzureDeleteClaimStorage();
    const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname.endsWith("/disks/crabbox-blue-lobster-osdisk")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-osdisk",
          tags: ownedAzureTags(),
          properties: { uniqueId: "disk-unique-id" },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "canonical companion set is incomplete",
    );
    expect(deletes).toEqual([]);
  });

  it.each(["public IP", "network interface"] as const)(
    "refuses a VM-less Azure managed disk with only its %s companion",
    async (companion) => {
      const { records, storage } = memoryAzureDeleteClaimStorage();
      const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
      seedAzureAuthCache(client);
      const deletes: string[] = [];
      const resourcePrefix = "/subscriptions/sub/resourceGroups/crabbox-leases/providers";
      const nicID = `${resourcePrefix}/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic`;
      const pipID = `${resourcePrefix}/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip`;
      const diskID = `${resourcePrefix}/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk`;
      client.fetcher = async (input, init) => {
        const url = new URL(String(input));
        if (init?.method === "DELETE") {
          deletes.push(url.pathname);
          return new Response(null, { status: 204 });
        }
        if (url.pathname === diskID) {
          return Response.json({
            id: diskID,
            name: "crabbox-blue-lobster-osdisk",
            location: "eastus",
            tags: ownedAzureTags(),
            properties: { uniqueId: "disk-immutable-id" },
          });
        }
        if (url.pathname === nicID && companion === "network interface") {
          return Response.json({
            id: nicID,
            name: "crabbox-blue-lobster-nic",
            location: "eastus",
            tags: ownedAzureTags(),
            properties: { resourceGuid: "nic-immutable-id", ipConfigurations: [] },
          });
        }
        if (url.pathname === pipID && companion === "public IP") {
          return Response.json({
            id: pipID,
            name: "crabbox-blue-lobster-pip",
            location: "eastus",
            tags: ownedAzureTags(),
            properties: { resourceGuid: "pip-immutable-id" },
          });
        }
        return azureResourceNotFoundResponse(url);
      };

      await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
        "canonical companion set is incomplete",
      );
      expect(deletes).toEqual([]);
      expect(records.size).toBe(1);
    },
  );

  it("refuses to delete a disk attached through managedByExtended", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client);
    const deletes: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname.endsWith("/disks/crabbox-blue-lobster-osdisk")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-osdisk",
          managedByExtended: [
            "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-other",
          ],
          tags: ownedAzureTags(),
          properties: { uniqueId: "disk-unique-id" },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "live association does not match lease",
    );
    expect(deletes).toEqual([]);
  });

  it("blocks replacement identities after a durable Azure cleanup claim", async () => {
    const { records, storage } = memoryAzureDeleteClaimStorage();
    const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
    seedAzureAuthCache(client);
    let vmImmutableID = "vm-original-id";
    let vmDataDisks = [
      {
        name: "shared-data",
        lun: 0,
        deleteOption: "Delete",
        managedDisk: {
          id: "/subscriptions/sub/resourceGroups/shared/providers/Microsoft.Compute/disks/shared-data",
        },
      },
    ];
    let vmOSDiskDeleteOption: string | undefined;
    let vmNICDeleteOption: string | undefined;
    let nicPIPDeleteOption: string | undefined;
    let resourceTags = ownedAzureTags();
    let failDelete = true;
    const deleted = new Set<string>();
    const deletes: string[] = [];
    const canonicalVMID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-blue-lobster";
    const canonicalNICID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic";
    const canonicalPIPID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip";
    const canonicalDiskID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk";
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        if (failDelete) {
          return new Response("interrupted", { status: 400 });
        }
        deleted.add(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (deleted.has(url.pathname)) {
        return azureResourceNotFoundResponse(url);
      }
      if (url.pathname.endsWith("/virtualMachines/crabbox-blue-lobster")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster",
          location: "eastus",
          tags: resourceTags,
          properties: {
            vmId: vmImmutableID,
            networkProfile: {
              networkInterfaces: [
                {
                  id: canonicalNICID,
                  ...(vmNICDeleteOption ? { deleteOption: vmNICDeleteOption } : {}),
                },
              ],
            },
            storageProfile: {
              osDisk: {
                managedDisk: { id: canonicalDiskID },
                ...(vmOSDiskDeleteOption ? { deleteOption: vmOSDiskDeleteOption } : {}),
              },
              dataDisks: vmDataDisks,
            },
          },
        });
      }
      if (url.pathname.endsWith("/networkInterfaces/crabbox-blue-lobster-nic")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-nic",
          location: "eastus",
          tags: resourceTags,
          properties: {
            resourceGuid: "nic-immutable-id",
            virtualMachine: { id: canonicalVMID },
            ipConfigurations: [
              {
                id: `${canonicalNICID}/ipConfigurations/ipconfig1`,
                properties: {
                  publicIPAddress: {
                    id: canonicalPIPID,
                    ...(nicPIPDeleteOption ? { deleteOption: nicPIPDeleteOption } : {}),
                  },
                },
              },
            ],
          },
        });
      }
      if (url.pathname.endsWith("/publicIPAddresses/crabbox-blue-lobster-pip")) {
        return Response.json({
          id: canonicalPIPID,
          name: "crabbox-blue-lobster-pip",
          location: "eastus",
          tags: resourceTags,
          properties: {
            resourceGuid: "pip-immutable-id",
            ipConfiguration: { id: `${canonicalNICID}/ipConfigurations/ipconfig1` },
          },
        });
      }
      if (url.pathname.endsWith("/disks/crabbox-blue-lobster-osdisk")) {
        return Response.json({
          id: canonicalDiskID,
          name: "crabbox-blue-lobster-osdisk",
          location: "eastus",
          managedBy: canonicalVMID,
          tags: resourceTags,
          properties: { uniqueId: "disk-immutable-id" },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "VM has unexpected data disks",
    );
    expect(records.size).toBe(1);
    expect(deletes).toEqual([]);

    vmDataDisks = [];
    vmOSDiskDeleteOption = "Delete";
    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "VM has cascading delete options",
    );
    vmOSDiskDeleteOption = undefined;
    vmNICDeleteOption = "Delete";
    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "VM has cascading delete options",
    );
    vmNICDeleteOption = undefined;
    nicPIPDeleteOption = "Delete";
    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "NIC has cascading delete options",
    );
    nicPIPDeleteOption = undefined;
    expect(deletes).toEqual([]);

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow("delete vm");
    expect(records.size).toBe(1);
    const [claimKey, storedClaim] = [...records.entries()][0] as [
      string,
      { stableResourceIdentity: string },
    ];
    const labelIdentity = JSON.stringify({
      crabbox: "true",
      created_by: "crabbox",
      lease: "cbx_abcdef123456",
      owner: "alice_example.com",
      provider: "azure",
      provider_key: "",
      slug: "blue-lobster",
      keep: "",
      created_at: "",
      expires_at: "",
    });
    const stableResourceIdentity = storedClaim.stableResourceIdentity;
    const observedResourceIdentity = JSON.stringify(
      (JSON.parse(storedClaim.stableResourceIdentity) as Array<Record<string, unknown>>).map(
        (entry) => ({ ...entry, labels: labelIdentity }),
      ),
    );
    expect(observedResourceIdentity).toEqual(expect.any(String));
    records.set(claimKey, { ...storedClaim, resourceIdentity: observedResourceIdentity });

    resourceTags = ownedAzureTags({ keep: "true" });
    await expect(
      client.deleteOwnedServer(ownedAzureLease(), { resourceIdentity: observedResourceIdentity }),
    ).rejects.toThrow("observed resource labels changed");
    expect(deletes).toHaveLength(1);

    resourceTags = ownedAzureTags();
    records.set(claimKey, storedClaim);
    vmImmutableID = "vm-replacement-id";
    await expect(
      client.deleteOwnedServer(ownedAzureLease(), { resourceIdentity: stableResourceIdentity }),
    ).rejects.toThrow("observed resource identity changed");
    expect(deletes).toHaveLength(1);

    failDelete = false;
    const replacementResourceIdentity = stableResourceIdentity.replace(
      "vm-original-id",
      "vm-replacement-id",
    );
    await expect(
      client.deleteOwnedServer(ownedAzureLease(), {
        resourceIdentity: replacementResourceIdentity,
      }),
    ).resolves.toBeUndefined();
    expect(records.size).toBe(0);
    expect(deletes).toHaveLength(5);
    expect(deleted.size).toBe(4);
  });

  it("preserves verified partial evidence when Azure cleanup identities change", async () => {
    const { records, storage } = memoryAzureDeleteClaimStorage();
    const deleted = new Set<string>();
    const deletes: string[] = [];
    let failNICDelete = true;
    const vmID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-blue-lobster";
    const nicID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic";
    const pipID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip";
    const diskID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk";
    const observedResourceIdentity = JSON.stringify([
      {
        kind: "networkInterfaces",
        id: nicID.toLowerCase(),
        immutableID: "nic-immutable-id",
        location: "eastus",
        topology: {
          virtualMachineID: vmID.toLowerCase(),
          publicIPIDs: [pipID.toLowerCase()],
          privateEndpointIDs: [],
          privateLinkServiceIDs: [],
          hostedWorkloadIDs: [],
          loadBalancerBackendAddressPoolIDs: [],
          applicationGatewayBackendAddressPoolIDs: [],
          loadBalancerInboundNatRuleIDs: [],
          privateLinkConnectionIDs: [],
          virtualNetworkTapIDs: [],
          gatewayLoadBalancerIDs: [],
        },
      },
      {
        kind: "publicIPAddresses",
        id: pipID.toLowerCase(),
        immutableID: "pip-immutable-id",
        location: "eastus",
        topology: {
          ipConfigurationID: `${nicID}/ipConfigurations/ipconfig1`.toLowerCase(),
          natGatewayID: "",
          linkedPublicIPAddressID: "",
          servicePublicIPAddressID: "",
        },
      },
      {
        kind: "disks",
        id: diskID.toLowerCase(),
        immutableID: "disk-immutable-id",
        location: "eastus",
        topology: { managedBy: "", managedByExtended: [] },
      },
    ]);
    const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
    seedAzureAuthCache(client);
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      if (method === "DELETE") {
        deletes.push(url.pathname);
        if (failNICDelete && url.pathname === nicID) {
          return new Response("injected NIC interruption", { status: 400 });
        }
        deleted.add(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (deleted.has(url.pathname)) return azureResourceNotFoundResponse(url);
      const isVM = url.pathname === vmID;
      const isNIC = url.pathname === nicID;
      const isPIP = url.pathname === pipID;
      const isDisk = url.pathname === diskID;
      if (!isVM && !isNIC && !isPIP && !isDisk) return azureResourceNotFoundResponse(url);
      const properties = isVM
        ? {
            vmId: "vm-immutable-id",
            networkProfile: { networkInterfaces: [{ id: nicID }] },
            storageProfile: { osDisk: { managedDisk: { id: diskID } } },
          }
        : isNIC
          ? {
              resourceGuid: "nic-immutable-id",
              virtualMachine: { id: vmID },
              ipConfigurations: [
                {
                  id: `${nicID}/ipConfigurations/ipconfig1`,
                  properties: { publicIPAddress: { id: pipID } },
                },
              ],
            }
          : isPIP
            ? {
                resourceGuid: "pip-immutable-id",
                ipConfiguration: { id: `${nicID}/ipConfigurations/ipconfig1` },
              }
            : { uniqueId: "disk-immutable-id" };
      return Response.json({
        id: url.pathname,
        name: url.pathname.slice(url.pathname.lastIndexOf("/") + 1),
        location: "eastus",
        managedBy: isDisk && !deleted.has(vmID) ? vmID : undefined,
        tags: ownedAzureTags(),
        properties,
      });
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "injected NIC interruption",
    );
    expect(deleted).toEqual(new Set([vmID]));

    await expect(
      client.deleteOwnedServer(ownedAzureLease(), { resourceIdentity: observedResourceIdentity }),
    ).rejects.toThrow("injected NIC interruption");
    const storedClaim = [...records.values()][0] as Record<string, unknown>;
    expect(storedClaim.partialStableResourceIdentity).toEqual(expect.any(String));
    expect(deletes).toEqual([vmID, nicID, nicID]);

    failNICDelete = false;
    await expect(
      client.deleteOwnedServer(ownedAzureLease(), { resourceIdentity: observedResourceIdentity }),
    ).resolves.toBeUndefined();
    expect(records.size).toBe(0);
    expect(deleted).toEqual(new Set([vmID, nicID, pipID, diskID]));
  });

  it("keeps durable cleanup claim storage on regional Azure fallback clients", () => {
    const { storage } = memoryAzureDeleteClaimStorage();
    const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
    const regional = (
      client as unknown as {
        clientForLocation(location: string, multiRegion: boolean): AzureClient;
      }
    ).clientForLocation("westus3", true);

    expect(
      (regional as unknown as { ownedDeleteClaimStorage?: AzureOwnedDeleteClaimStorage })
        .ownedDeleteClaimStorage,
    ).toBe(storage);
  });

  it("does not delete an Azure VM before its referenced managed disk identity is visible", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client, "test-token");
    const deletes: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (url.pathname.includes("/virtualMachines/")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster",
          tags: ownedAzureTags(),
          properties: {
            networkProfile: {
              networkInterfaces: [
                {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic",
                },
              ],
            },
            storageProfile: {
              osDisk: {
                managedDisk: {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk",
                },
              },
            },
          },
        });
      }
      if (url.pathname.includes("/networkInterfaces/")) {
        return Response.json({
          id: url.pathname,
          name: "crabbox-blue-lobster-nic",
          tags: ownedAzureTags(),
          properties: { ipConfigurations: [] },
        });
      }
      return azureResourceNotFoundResponse(url);
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "managed OS disk identity is not yet available",
    );
    expect(deletes).toEqual([]);
  });

  it("resumes verified Azure companion cleanup but rejects a legacy partial claim", async () => {
    const { records, putKeys, storage } = memoryAzureDeleteClaimStorage();
    const deleted = new Set<string>();
    const deletes: string[] = [];
    let failNICDelete = true;
    const client = (): AzureClient => {
      const value = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
      seedAzureAuthCache(value, "test-token");
      value.fetcher = async (input, init) => {
        const url = new URL(String(input));
        const method = init?.method ?? "GET";
        if (method === "DELETE") {
          deletes.push(url.pathname);
          if (failNICDelete && url.pathname.includes("/networkInterfaces/")) {
            return new Response("injected interruption", { status: 400 });
          }
          deleted.add(url.pathname);
          return new Response(null, { status: 204 });
        }
        if (deleted.has(url.pathname)) {
          const resource = url.pathname.slice(url.pathname.indexOf("/providers/") + 11);
          return new Response(
            JSON.stringify({
              error: {
                code: "ResourceNotFound",
                message: `The Resource '${resource}' was not found.`,
              },
            }),
            { status: 404 },
          );
        }
        const name = url.pathname.slice(url.pathname.lastIndexOf("/") + 1);
        const isVM = url.pathname.includes("/virtualMachines/");
        const isNIC = url.pathname.includes("/networkInterfaces/");
        const isPIP = url.pathname.includes("/publicIPAddresses/");
        const isDisk = url.pathname.includes("/disks/");
        const properties = isVM
          ? {
              vmId: "vm-immutable-id",
              networkProfile: {
                networkInterfaces: [
                  {
                    id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic",
                  },
                ],
              },
              storageProfile: {
                osDisk: {
                  managedDisk: {
                    id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk",
                  },
                },
              },
            }
          : isNIC
            ? {
                resourceGuid: "nic-immutable-id",
                virtualMachine: {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-blue-lobster",
                },
                ipConfigurations: [
                  {
                    properties: {
                      publicIPAddress: {
                        id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip",
                      },
                    },
                  },
                ],
              }
            : isPIP
              ? { resourceGuid: "pip-immutable-id" }
              : isDisk
                ? { uniqueId: "disk-unique-id" }
                : {};
        return Response.json({
          id: url.pathname,
          name,
          location: "eastus",
          managedBy:
            isDisk &&
            !deleted.has(
              url.pathname.replace("/disks/", "/virtualMachines/").replace("-osdisk", ""),
            )
              ? "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-blue-lobster"
              : undefined,
          tags: ownedAzureTags(),
          properties,
        });
      };
      return value;
    };

    await expect(client().deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "injected interruption",
    );
    expect(records.size).toBe(1);
    expect(deleted.size).toBe(1);
    expect([...deleted][0]).toContain("/virtualMachines/");

    const [claimKey, storedClaim] = [...records.entries()][0]!;
    records.set(claimKey, {
      ...(storedClaim as Record<string, unknown>),
      version: 1,
      preparing: undefined,
      resourceIdentity: undefined,
      stableResourceIdentity: undefined,
      deletedStableResourceIdentity: undefined,
    });
    const writesBeforeLegacyReplay = putKeys.length;
    await expect(client().deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "NIC is attached to an unexpected VM",
    );
    expect(putKeys).toHaveLength(writesBeforeLegacyReplay + 1);
    expect([...records.values()][0]).toMatchObject({
      stableResourceIdentity: undefined,
      deletedStableResourceIdentity: undefined,
    });

    records.set(claimKey, storedClaim);
    failNICDelete = false;
    await expect(client().deleteOwnedServer(ownedAzureLease())).resolves.toBeUndefined();
    expect(records.size).toBe(0);
    expect(deletes.filter((path) => path.includes("/virtualMachines/"))).toHaveLength(1);
    expect(deleted.size).toBe(4);
  });

  it("does not clear a newer claim transition after observing an empty legacy set", async () => {
    const { records, putKeys, storage } = memoryAzureDeleteClaimStorage();
    const seed = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
    seedAzureAuthCache(seed);
    seed.fetcher = async (input) => azureResourceNotFoundResponse(new URL(String(input)));
    await seed.deleteOwnedServer(ownedAzureLease());
    const claimKey = putKeys[0]!;
    records.set(claimKey, {
      version: 1,
      provider: "azure",
      leaseID: "cbx_abcdef123456",
      slug: "blue-lobster",
      owner: "alice@example.com",
      cloudID: "crabbox-blue-lobster",
      providerScope: "/subscriptions/sub/resourceGroups/crabbox-leases",
    });

    let releaseRead!: () => void;
    const readGate = new Promise<void>((resolve) => {
      releaseRead = resolve;
    });
    let signalRead!: () => void;
    const readStarted = new Promise<void>((resolve) => {
      signalRead = resolve;
    });
    let pendingRead = true;
    const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
    seedAzureAuthCache(client);
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      if (pendingRead && url.pathname.includes("/networkInterfaces/")) {
        pendingRead = false;
        signalRead();
        await readGate;
      }
      return azureResourceNotFoundResponse(url);
    };
    const cleanup = client.deleteOwnedServer(ownedAzureLease());
    await readStarted;
    const newerPreparation = {
      version: 2,
      provider: "azure",
      leaseID: "cbx_abcdef123456",
      slug: "blue-lobster",
      owner: "alice@example.com",
      cloudID: "crabbox-blue-lobster",
      providerScope: "/subscriptions/sub/resourceGroups/crabbox-leases",
      preparing: true,
    };
    records.set(claimKey, newerPreparation);
    releaseRead();

    await expect(cleanup).rejects.toThrow("durable cleanup claim changed");
    expect(records.get(claimKey)).toEqual(newerPreparation);
  });

  it("rejects an absent PIP after its durable deletion attempt failed", async () => {
    const { records, storage } = memoryAzureDeleteClaimStorage();
    const deleted = new Set<string>();
    const deletes: string[] = [];
    let failure: "pip" | "disk" | undefined = "pip";
    const first = azurePIPDiskCleanupClient({
      storage,
      deleted,
      deletes,
      failDelete: () => failure,
    });

    await expect(first.client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "injected pip interruption",
    );
    expect(deletes).toEqual([first.nicID, first.pipID]);

    deleted.add(first.pipID);
    failure = undefined;
    const retry = azurePIPDiskCleanupClient({
      storage,
      deleted,
      deletes,
      failDelete: () => failure,
    });
    await expect(retry.client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "observed resource identity changed",
    );
    expect(deletes).toEqual([first.nicID, first.pipID]);
    expect(deleted.has(first.diskID)).toBe(false);
    expect(records.size).toBe(1);
  });

  it("rejects an absent PIP when its successful deletion progress was not persisted", async () => {
    const memory = memoryAzureDeleteClaimStorage();
    const deleted = new Set<string>();
    const deletes: string[] = [];
    const put = async <T>(key: string, value: T) => {
      const progress = (value as { deletedStableResourceIdentity?: string })
        .deletedStableResourceIdentity;
      if (progress?.includes("pip-immutable-id")) {
        throw new Error("injected progress write interruption");
      }
      await memory.storage.put(key, value);
    };
    const storage: AzureOwnedDeleteClaimStorage = {
      get: memory.storage.get.bind(memory.storage),
      put,
      delete: memory.storage.delete.bind(memory.storage),
      transaction: async (callback) => await callback(storage),
    };
    const first = azurePIPDiskCleanupClient({
      storage,
      deleted,
      deletes,
      failDelete: () => undefined,
    });

    await expect(first.client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "injected progress write interruption",
    );
    expect(deleted).toEqual(new Set([first.nicID, first.pipID]));

    const retry = azurePIPDiskCleanupClient({
      storage: memory.storage,
      deleted,
      deletes,
      failDelete: () => undefined,
    });
    await expect(retry.client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "observed resource identity changed",
    );
    expect(deletes).toEqual([first.nicID, first.pipID]);
    expect(deleted.has(first.diskID)).toBe(false);
    expect(memory.records.size).toBe(1);
  });

  it("continues after the exact PIP deletion identity is durably verified", async () => {
    const { records, storage } = memoryAzureDeleteClaimStorage();
    const deleted = new Set<string>();
    const deletes: string[] = [];
    let failure: "pip" | "disk" | undefined = "disk";
    const first = azurePIPDiskCleanupClient({
      storage,
      deleted,
      deletes,
      failDelete: () => failure,
    });

    await expect(first.client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "injected disk interruption",
    );
    const claim = [...records.values()][0] as { deletedStableResourceIdentity?: string };
    expect(claim.deletedStableResourceIdentity).toContain("pip-immutable-id");
    expect(deleted).toEqual(new Set([first.nicID, first.pipID]));

    failure = undefined;
    const retry = azurePIPDiskCleanupClient({
      storage,
      deleted,
      deletes,
      failDelete: () => failure,
    });
    await expect(retry.client.deleteOwnedServer(ownedAzureLease())).resolves.toBeUndefined();
    expect(deletes).toEqual([first.nicID, first.pipID, first.diskID, first.diskID]);
    expect(deleted).toEqual(new Set([first.nicID, first.pipID, first.diskID]));
    expect(records.size).toBe(0);
  });

  it("merges concurrent deletion progress without regressing a longer prefix", async () => {
    const { records, storage } = memoryAzureDeleteClaimStorage();
    const deleted = new Set<string>();
    const deletes: string[] = [];
    let releaseSlowNIC!: () => void;
    const slowNICGate = new Promise<void>((resolve) => {
      releaseSlowNIC = resolve;
    });
    let signalSlowNIC!: () => void;
    const slowNICStarted = new Promise<void>((resolve) => {
      signalSlowNIC = resolve;
    });
    let slowNICPending = true;
    const slow = azurePIPDiskCleanupClient({
      storage,
      deleted,
      deletes,
      failDelete: () => "disk",
      beforeDelete: async (path) => {
        if (slowNICPending && path.includes("/networkInterfaces/")) {
          slowNICPending = false;
          signalSlowNIC();
          await slowNICGate;
        }
      },
    });
    const slowResult = slow.client.deleteOwnedServer(ownedAzureLease());
    await slowNICStarted;

    const fast = azurePIPDiskCleanupClient({
      storage,
      deleted,
      deletes,
      failDelete: () => "disk",
    });
    await expect(fast.client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "injected disk interruption",
    );

    releaseSlowNIC();
    await expect(slowResult).rejects.toThrow("injected disk interruption");
    const claim = [...records.values()][0] as { deletedStableResourceIdentity?: string };
    expect(claim.deletedStableResourceIdentity).toContain("nic-immutable-id");
    expect(claim.deletedStableResourceIdentity).toContain("pip-immutable-id");
    expect(deleted.has(slow.diskID)).toBe(false);
  });

  it("does not let a delayed claim preparation overwrite concurrent progress", async () => {
    const { records, storage } = memoryAzureDeleteClaimStorage();
    const deleted = new Set<string>();
    const deletes: string[] = [];
    let releasePreparation!: () => void;
    const preparationGate = new Promise<void>((resolve) => {
      releasePreparation = resolve;
    });
    let signalPreparation!: () => void;
    const preparationStarted = new Promise<void>((resolve) => {
      signalPreparation = resolve;
    });
    let preparationPending = true;
    const slow = azurePIPDiskCleanupClient({
      storage,
      deleted,
      deletes,
      failDelete: () => "disk",
      beforeRead: async (path) => {
        if (preparationPending && path.includes("/networkInterfaces/")) {
          preparationPending = false;
          signalPreparation();
          await preparationGate;
        }
      },
    });
    const slowResult = slow.client.deleteOwnedServer(ownedAzureLease());
    await preparationStarted;

    const fast = azurePIPDiskCleanupClient({
      storage,
      deleted,
      deletes,
      failDelete: () => "disk",
    });
    await expect(fast.client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "injected disk interruption",
    );

    releasePreparation();
    await expect(slowResult).rejects.toThrow("injected disk interruption");
    const claim = [...records.values()][0] as { deletedStableResourceIdentity?: string };
    expect(claim.deletedStableResourceIdentity).toContain("pip-immutable-id");
    expect(deleted.has(slow.diskID)).toBe(false);
  });

  it("refuses all Azure deletion when a companion belongs to another lease", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client, "test-token");
    const deletes: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        return new Response(null, { status: 204 });
      }
      const name = url.pathname.slice(url.pathname.lastIndexOf("/") + 1);
      const tags = url.pathname.includes("/publicIPAddresses/")
        ? ownedAzureTags({ lease: "cbx_000000000000" })
        : ownedAzureTags();
      const properties = url.pathname.includes("/virtualMachines/")
        ? {
            networkProfile: {
              networkInterfaces: [
                {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic",
                },
              ],
            },
            storageProfile: {
              osDisk: {
                managedDisk: {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk",
                },
              },
            },
          }
        : url.pathname.includes("/networkInterfaces/")
          ? {
              ipConfigurations: [
                {
                  properties: {
                    publicIPAddress: {
                      id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip",
                    },
                  },
                },
              ],
            }
          : url.pathname.includes("/disks/")
            ? { uniqueId: "disk-unique-id" }
            : {};
      return Response.json({ id: url.pathname, name, tags, properties });
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "public IP crabbox-blue-lobster-pip: ownership does not match",
    );
    expect(deletes).toEqual([]);
  });

  it("blocks Azure NIC service associations before any destructive request", async () => {
    const cases = [
      {
        privateEndpoint: undefined,
        ipConfigurationProperties: {
          loadBalancerBackendAddressPools: [
            {
              id: "/subscriptions/sub/resourceGroups/shared/providers/Microsoft.Network/loadBalancers/shared/backendAddressPools/pool",
            },
          ],
        },
      },
      {
        privateEndpoint: undefined,
        ipConfigurationProperties: {
          applicationGatewayBackendAddressPools: [
            {
              id: "/subscriptions/sub/resourceGroups/shared/providers/Microsoft.Network/applicationGateways/shared/backendAddressPools/pool",
            },
          ],
        },
      },
      {
        privateEndpoint: undefined,
        ipConfigurationProperties: {
          applicationSecurityGroups: [
            {
              id: "/subscriptions/sub/resourceGroups/shared/providers/Microsoft.Network/applicationSecurityGroups/shared",
            },
          ],
        },
      },
      {
        privateEndpoint: undefined,
        ipConfigurationProperties: {
          loadBalancerInboundNatRules: [
            {
              id: "/subscriptions/sub/resourceGroups/shared/providers/Microsoft.Network/loadBalancers/shared/inboundNatRules/rule",
            },
          ],
        },
      },
      {
        ipConfigurationProperties: undefined,
        privateEndpoint: {
          id: "/subscriptions/sub/resourceGroups/shared/providers/Microsoft.Network/privateEndpoints/shared",
        },
      },
    ];

    await Promise.all(
      cases.map(async ({ ipConfigurationProperties, privateEndpoint }) => {
        const client = new AzureClient(baseEnv);
        seedAzureAuthCache(client);
        const deletes: string[] = [];
        const nicID =
          "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic";
        const pipID =
          "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip";
        client.fetcher = async (input, init) => {
          const url = new URL(String(input));
          if (init?.method === "DELETE") {
            deletes.push(url.pathname);
            return new Response(null, { status: 204 });
          }
          if (url.pathname.includes("/networkInterfaces/")) {
            return Response.json({
              id: url.pathname,
              name: "crabbox-blue-lobster-nic",
              location: "eastus",
              tags: ownedAzureTags(),
              properties: {
                resourceGuid: "nic-immutable-id",
                ...(privateEndpoint ? { privateEndpoint } : {}),
                ipConfigurations: [
                  {
                    id: `${nicID}/ipConfigurations/ipconfig1`,
                    properties: {
                      publicIPAddress: { id: pipID },
                      ...ipConfigurationProperties,
                    },
                  },
                ],
              },
            });
          }
          return azureResourceNotFoundResponse(url);
        };

        await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
          "NIC has an unexpected service association",
        );
        expect(deletes).toEqual([]);
      }),
    );
  });

  it("binds an untagged Azure OS disk through the verified live VM before deletion", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client, "test-token");
    let diskTagged = false;
    const deleted = new Set<string>();
    const methods: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const method = init?.method ?? "GET";
      methods.push(`${method} ${url.pathname}`);
      if (method === "DELETE") {
        deleted.add(url.pathname);
        return new Response(null, { status: 204 });
      }
      if (deleted.has(url.pathname)) return azureResourceNotFoundResponse(url);
      if (method === "PATCH") {
        diskTagged = true;
        return Response.json({});
      }
      const name = url.pathname.slice(url.pathname.lastIndexOf("/") + 1);
      const tags = url.pathname.includes("/disks/") && !diskTagged ? {} : ownedAzureTags();
      const managedBy = url.pathname.includes("/disks/")
        ? "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-blue-lobster"
        : undefined;
      const properties = url.pathname.includes("/virtualMachines/")
        ? {
            vmId: "vm-immutable-id",
            networkProfile: {
              networkInterfaces: [
                {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic",
                },
              ],
            },
            storageProfile: {
              osDisk: {
                managedDisk: {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk",
                },
              },
            },
          }
        : url.pathname.includes("/networkInterfaces/")
          ? {
              resourceGuid: "nic-immutable-id",
              ipConfigurations: [
                {
                  properties: {
                    publicIPAddress: {
                      id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip",
                    },
                  },
                },
              ],
            }
          : url.pathname.includes("/publicIPAddresses/")
            ? { resourceGuid: "pip-immutable-id" }
            : url.pathname.includes("/disks/")
              ? { uniqueId: "disk-unique-id" }
              : {};
      return Response.json({
        id: url.pathname,
        name,
        location: "eastus",
        managedBy,
        tags,
        properties,
      });
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).resolves.toBeUndefined();
    expect(methods.some((call) => call.includes("PATCH") && call.includes("/disks/"))).toBe(true);
  });

  it.each([
    ["conflicting ownership", ownedAzureTags({ lease: "cbx_000000000000" }), undefined],
    ["missing managedBy", {}, undefined],
    [
      "wrong managedBy",
      {},
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-other",
    ],
    [
      "matching tags but wrong managedBy",
      ownedAzureTags(),
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-other",
      "live association does not match",
    ],
  ])(
    "refuses to adopt an Azure disk with %s",
    async (_case, diskTags, diskManagedBy, expectedError = "ownership does not match") => {
      const client = new AzureClient(baseEnv);
      seedAzureAuthCache(client, "test-token");
      const deletes: string[] = [];
      client.fetcher = async (input, init) => {
        const url = new URL(String(input));
        if (init?.method === "DELETE") {
          deletes.push(url.pathname);
          return new Response(null, { status: 204 });
        }
        const name = url.pathname.slice(url.pathname.lastIndexOf("/") + 1);
        const isDisk = url.pathname.includes("/disks/");
        const properties = url.pathname.includes("/virtualMachines/")
          ? {
              networkProfile: {
                networkInterfaces: [
                  {
                    id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic",
                  },
                ],
              },
              storageProfile: {
                osDisk: {
                  managedDisk: {
                    id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk",
                  },
                },
              },
            }
          : url.pathname.includes("/networkInterfaces/")
            ? {
                ipConfigurations: [
                  {
                    properties: {
                      publicIPAddress: {
                        id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip",
                      },
                    },
                  },
                ],
              }
            : isDisk
              ? { uniqueId: "disk-unique-id" }
              : {};
        return Response.json({
          id: url.pathname,
          name,
          managedBy: isDisk ? diskManagedBy : undefined,
          tags: isDisk ? diskTags : ownedAzureTags(),
          properties,
        });
      };

      await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(expectedError);
      expect(deletes).toEqual([]);
    },
  );

  it("revalidates Azure companions after deleting the VM", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client, "test-token");
    let vmDeleted = false;
    const deletes: string[] = [];
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        deletes.push(url.pathname);
        if (url.pathname.includes("/virtualMachines/")) vmDeleted = true;
        return new Response(null, { status: 204 });
      }
      if (vmDeleted && url.pathname.includes("/virtualMachines/")) {
        return new Response(
          JSON.stringify({
            error: {
              code: "ResourceNotFound",
              message:
                "The Resource 'Microsoft.Compute/virtualMachines/crabbox-blue-lobster' was not found.",
            },
          }),
          { status: 404 },
        );
      }
      const name = url.pathname.slice(url.pathname.lastIndexOf("/") + 1);
      const tags =
        vmDeleted && url.pathname.includes("/networkInterfaces/")
          ? ownedAzureTags({ lease: "cbx_000000000000" })
          : ownedAzureTags();
      const isDisk = url.pathname.includes("/disks/");
      const properties = url.pathname.includes("/virtualMachines/")
        ? {
            vmId: "vm-immutable-id",
            networkProfile: {
              networkInterfaces: [
                {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic",
                },
              ],
            },
            storageProfile: {
              osDisk: {
                managedDisk: {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk",
                },
              },
            },
          }
        : url.pathname.includes("/networkInterfaces/")
          ? {
              resourceGuid: "nic-immutable-id",
              ipConfigurations: [
                {
                  properties: {
                    publicIPAddress: {
                      id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip",
                    },
                  },
                },
              ],
            }
          : url.pathname.includes("/publicIPAddresses/")
            ? { resourceGuid: "pip-immutable-id" }
            : isDisk
              ? { uniqueId: "disk-unique-id" }
              : {};
      return Response.json({
        id: url.pathname,
        name,
        location: "eastus",
        managedBy: isDisk
          ? "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-blue-lobster"
          : undefined,
        tags,
        properties,
      });
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "NIC crabbox-blue-lobster-nic: ownership does not match",
    );
    expect(deletes).toHaveLength(1);
    expect(deletes[0]).toContain("/virtualMachines/");
  });

  it("fails closed when an owned Azure DELETE returns a scope-level 404", async () => {
    const client = new AzureClient(baseEnv);
    seedAzureAuthCache(client, "test-token");
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      if (init?.method === "DELETE") {
        return new Response(
          JSON.stringify({
            error: {
              code: "ResourceNotFound",
              message: "The Resource group 'crabbox-leases' was not found.",
            },
          }),
          { status: 404 },
        );
      }
      const name = url.pathname.slice(url.pathname.lastIndexOf("/") + 1);
      const isDisk = url.pathname.includes("/disks/");
      const properties = url.pathname.includes("/virtualMachines/")
        ? {
            vmId: "vm-immutable-id",
            networkProfile: {
              networkInterfaces: [
                {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/crabbox-blue-lobster-nic",
                },
              ],
            },
            storageProfile: {
              osDisk: {
                managedDisk: {
                  id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/disks/crabbox-blue-lobster-osdisk",
                },
              },
            },
          }
        : url.pathname.includes("/networkInterfaces/")
          ? {
              resourceGuid: "nic-immutable-id",
              ipConfigurations: [
                {
                  properties: {
                    publicIPAddress: {
                      id: "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/publicIPAddresses/crabbox-blue-lobster-pip",
                    },
                  },
                },
              ],
            }
          : url.pathname.includes("/publicIPAddresses/")
            ? { resourceGuid: "pip-immutable-id" }
            : isDisk
              ? { uniqueId: "disk-unique-id" }
              : {};
      return Response.json({
        id: url.pathname,
        name,
        location: "eastus",
        managedBy: isDisk
          ? "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-blue-lobster"
          : undefined,
        tags: ownedAzureTags(),
        properties,
      });
    };

    await expect(client.deleteOwnedServer(ownedAzureLease())).rejects.toThrow(
      "Resource group 'crabbox-leases' was not found",
    );
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

  it("rejects invalid configured Azure SSH CIDRs", () => {
    expect(
      () => new AzureClient({ ...baseEnv, CRABBOX_AZURE_SSH_CIDRS: "999.999.999.999/32" }),
    ).toThrow("CRABBOX_AZURE_SSH_CIDRS entries must be valid");
  });

  it("creates Windows VMs with Windows OS profile and bootstrap extension", async () => {
    vi.useFakeTimers();
    const client = new AzureClient(baseEnv);
    const bodies: unknown[] = [];
    let extensionStateReads = 0;
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input.toString();
      if (isAzureLoginURL(url)) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), {
            status: 200,
          }),
        );
      }
      if (init?.body) bodies.push(JSON.parse(String(init.body)));
      if (url === "https://management.azure.com/windows-extension-op") {
        return Promise.resolve(new Response(JSON.stringify({ status: "InProgress" })));
      }
      if (url.includes("/extensions/crabbox-bootstrap") && init?.method === "PUT") {
        return Promise.resolve(
          new Response(null, {
            status: 202,
            headers: {
              "azure-asyncoperation": "https://management.azure.com/windows-extension-op",
            },
          }),
        );
      }
      if (url.includes("/extensions/crabbox-bootstrap") && init?.method !== "PUT") {
        extensionStateReads += 1;
        return Promise.resolve(
          new Response(JSON.stringify({ properties: { provisioningState: "Succeeded" } }), {
            status: 200,
          }),
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
            JSON.stringify({
              tags: { managed_by: "crabbox" },
              properties: { securityRules: [] },
            }),
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
    const create = client.createServerWithFallback(
      config,
      "cbx_123456789abc",
      "blue-lobster",
      "owner",
    );
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(15_000);
    await create;

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
    expect(extensionStateReads).toBeGreaterThanOrEqual(1);
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

  it("uses full-caching ephemeral OS disks for azureOSDisk=ephemeral-preview", async () => {
    const client = new AzureClient(baseEnv);
    const bodies: unknown[] = [];
    const vmAPIVersions: string[] = [];
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = new URL(typeof input === "string" ? input : input.toString());
      if (isAzureLoginURL(url.toString())) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (init?.body) bodies.push(JSON.parse(String(init.body)));
      if (
        url.pathname.includes("/virtualMachines/crabbox-blue-lobster") &&
        init?.method === "PUT"
      ) {
        vmAPIVersions.push(url.searchParams.get("api-version") ?? "");
      }
      if (url.pathname.endsWith("/resourceGroups/crabbox-leases")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (url.pathname.endsWith("/virtualNetworks/crabbox-vnet")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (url.pathname.endsWith("/networkSecurityGroups/crabbox-nsg") && init?.method === "GET") {
        return Promise.resolve(
          new Response(
            JSON.stringify({ tags: { managed_by: "crabbox" }, properties: { securityRules: [] } }),
            { status: 200 },
          ),
        );
      }
      if (url.pathname.endsWith("/providers/Microsoft.Compute/skus")) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              value: [
                {
                  name: "Standard_D8ads_v6",
                  resourceType: "virtualMachines",
                  capabilities: [{ name: "EphemeralOSDiskSupported", value: "True" }],
                },
              ],
            }),
            { status: 200 },
          ),
        );
      }
      if (url.pathname.includes("/publicIPAddresses/") && init?.method === "GET") {
        return Promise.resolve(
          new Response(JSON.stringify({ properties: { ipAddress: "192.0.2.10" } }), {
            status: 200,
          }),
        );
      }
      if (url.pathname.includes("/virtualMachines/") && init?.method === "GET") {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              name: "crabbox-blue-lobster",
              tags: { crabbox: "true" },
              properties: {
                provisioningState: "Succeeded",
                hardwareProfile: { vmSize: "Standard_D8ads_v6" },
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
      testLeaseConfig({ azureOSDisk: "ephemeral-preview", serverType: "Standard_D8ads_v6" }),
      "cbx_123456789abc",
      "blue-lobster",
      "owner",
    );

    expect(vmAPIVersions).toContain("2025-04-01");
    const vmBody = bodies.find(
      (body): body is { properties: { storageProfile: { osDisk: Record<string, unknown> } } } =>
        typeof body === "object" &&
        body !== null &&
        "properties" in body &&
        JSON.stringify(body).includes("storageProfile") &&
        JSON.stringify(body).includes("osDisk"),
    );
    expect(vmBody?.properties.storageProfile.osDisk).toMatchObject({
      caching: "ReadOnly",
      managedDisk: { storageAccountType: "StandardSSD_LRS" },
      diffDiskSettings: { option: "Local", enableFullCaching: true },
    });
  });

  it("skips stale non-explicit defaults for azureOSDisk=ephemeral-preview fallback", async () => {
    const client = new AzureClient(baseEnv);
    const vmSizes: string[] = [];
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = new URL(typeof input === "string" ? input : input.toString());
      if (isAzureLoginURL(url.toString())) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (url.pathname.endsWith("/resourceGroups/crabbox-leases")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (url.pathname.endsWith("/virtualNetworks/crabbox-vnet")) {
        return Promise.resolve(
          new Response(JSON.stringify({ tags: { managed_by: "crabbox" } }), { status: 200 }),
        );
      }
      if (url.pathname.endsWith("/networkSecurityGroups/crabbox-nsg") && init?.method === "GET") {
        return Promise.resolve(
          new Response(
            JSON.stringify({ tags: { managed_by: "crabbox" }, properties: { securityRules: [] } }),
            { status: 200 },
          ),
        );
      }
      if (url.pathname.endsWith("/providers/Microsoft.Compute/skus")) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              value: [
                {
                  name: "Standard_D8ads_v6",
                  resourceType: "virtualMachines",
                  capabilities: [{ name: "EphemeralOSDiskSupported", value: "True" }],
                },
              ],
            }),
            { status: 200 },
          ),
        );
      }
      if (url.pathname.includes("/publicIPAddresses/") && init?.method === "GET") {
        return Promise.resolve(
          new Response(JSON.stringify({ properties: { ipAddress: "192.0.2.10" } }), {
            status: 200,
          }),
        );
      }
      if (
        url.pathname.includes("/virtualMachines/") &&
        !url.pathname.includes("/extensions/") &&
        init?.method === "PUT" &&
        init.body
      ) {
        const body = JSON.parse(String(init.body)) as {
          properties?: { hardwareProfile?: { vmSize?: string } };
        };
        vmSizes.push(body.properties?.hardwareProfile?.vmSize ?? "");
      }
      if (url.pathname.includes("/virtualMachines/") && init?.method === "GET") {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              name: "crabbox-blue-lobster",
              tags: { crabbox: "true" },
              properties: {
                provisioningState: "Succeeded",
                hardwareProfile: { vmSize: "Standard_D8ads_v6" },
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
        target: "windows",
        azureOSDisk: "ephemeral-preview",
        serverType: "Standard_D2ads_v6",
        serverTypeExplicit: false,
      }),
      "cbx_123456789abc",
      "blue-lobster",
      "owner",
    );

    expect(vmSizes).toEqual(["Standard_D8ads_v6"]);
  });

  it("rejects unsupported azureOSDisk=ephemeral-preview SKUs before allocating network resources", async () => {
    const client = new AzureClient(baseEnv);
    const calls: string[] = [];
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = new URL(typeof input === "string" ? input : input.toString());
      calls.push(`${init?.method ?? "GET"} ${url.pathname}`);
      if (isAzureLoginURL(url.toString())) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (url.pathname.endsWith("/providers/Microsoft.Compute/skus")) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              value: [
                {
                  name: "Standard_D4ads_v6",
                  resourceType: "virtualMachines",
                  capabilities: [{ name: "EphemeralOSDiskSupported", value: "True" }],
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
        testLeaseConfig({ azureOSDisk: "ephemeral-preview", serverType: "Standard_D4ads_v6" }),
        "cbx_123456789abc",
        "bad-preview",
        "owner",
      ),
    ).rejects.toThrow(/azureOSDisk=ephemeral-preview requires/);
    expect(calls.some((call) => call.includes("/providers/Microsoft.Compute/skus"))).toBe(true);
    expect(calls.some((call) => call.includes("/resourceGroups/crabbox-leases"))).toBe(false);
    expect(calls.some((call) => call.includes("/virtualNetworks/"))).toBe(false);
    expect(calls.some((call) => call.includes("/networkSecurityGroups/"))).toBe(false);
    expect(calls.some((call) => call.includes("/publicIPAddresses/"))).toBe(false);
    expect(calls.some((call) => call.includes("/networkInterfaces/"))).toBe(false);
    expect(calls.some((call) => call.includes("/virtualMachines/"))).toBe(false);
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
    const vmAPIVersions: string[] = [];
    const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input.toString();
      const parsedURL = new URL(url);
      const pathname = parsedURL.pathname;
      if (isAzureLoginURL(url)) {
        return Promise.resolve(
          new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), { status: 200 }),
        );
      }
      if (init?.body) bodies.push(JSON.parse(String(init.body)));
      if (
        pathname.includes("/virtualMachines/crabbox-blue-lobster") &&
        !pathname.includes("/extensions/") &&
        init?.method === "PUT"
      ) {
        vmAPIVersions.push(parsedURL.searchParams.get("api-version") ?? "");
      }
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
        azureOSDisk: "ephemeral-preview",
        capacityMarket: "on-demand",
        serverType: "Standard_D2ads_v6",
        serverTypeExplicit: false,
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
    expect(vmAPIVersions).toEqual(["2024-07-01"]);
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

  it("skips Azure NSG writes when SSH rules already match", async () => {
    const client = new AzureClient(baseEnv);
    const nsgWrites: string[] = [];
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
            JSON.stringify({
              tags: { managed_by: "crabbox" },
              properties: {
                securityRules: [
                  {
                    name: "crabbox-ssh-2222-0",
                    properties: {
                      priority: 100,
                      direction: "Inbound",
                      access: "Allow",
                      protocol: "Tcp",
                      sourceAddressPrefix: "0.0.0.0/0",
                      sourcePortRange: "*",
                      destinationAddressPrefix: "*",
                      destinationPortRange: "2222",
                    },
                  },
                  {
                    name: "crabbox-ssh-22-0",
                    properties: {
                      priority: 101,
                      direction: "Inbound",
                      access: "Allow",
                      protocol: "Tcp",
                      sourceAddressPrefix: "0.0.0.0/0",
                      sourcePortRange: "*",
                      destinationAddressPrefix: "*",
                      destinationPortRange: "22",
                    },
                  },
                ],
              },
            }),
            { status: 200 },
          ),
        );
      }
      if (url.includes("/networkSecurityGroups/crabbox-nsg?") && init?.method === "PUT") {
        nsgWrites.push(url);
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    }) as typeof fetch;
    client.fetcher = fakeFetch;

    await client.ensureSharedInfra("eastus", testLeaseConfig());

    expect(nsgWrites).toEqual([]);
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

  it("shares one token exchange across concurrent resource reads", async () => {
    const client = new AzureClient(baseEnv);
    let tokenMints = 0;
    const authorizations: string[] = [];
    client.fetcher = async (input, init) => {
      if (isAzureLoginURL(String(input))) {
        tokenMints += 1;
        return Response.json({ access_token: "parallel-token", expires_in: 3600 });
      }
      authorizations.push(new Headers(init?.headers).get("authorization") ?? "");
      return Response.json({ value: [] });
    };
    await client.listReconciliationResources();
    expect(tokenMints).toBe(1);
    expect(authorizations).toEqual(Array(4).fill("Bearer parallel-token"));
  });

  it("refreshes at the thirty-second margin without sharing credentials between clients", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-01-01T00:00:00Z"));
    let exchanges = 0;
    const client = new AzureClient(baseEnv);
    client.fetcher = async (input) => {
      if (isAzureLoginURL(String(input))) {
        exchanges += 1;
        return Response.json({ access_token: `token-${exchanges}`, expires_in: 60 });
      }
      return Response.json({ value: [] });
    };
    await client.listCrabboxServers();
    vi.advanceTimersByTime(29_999);
    await client.listCrabboxServers();
    expect(exchanges).toBe(1);
    vi.advanceTimersByTime(1);
    await client.listReconciliationResources();
    expect(exchanges).toBe(2);
    const other = new AzureClient({ ...baseEnv, AZURE_TENANT_ID: "other-tenant" });
    other.fetcher = client.fetcher;
    await other.listCrabboxServers();
    expect(exchanges).toBe(3);
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

  it("bounds Azure Spot VM LROs by the configured on-demand fallback window", () => {
    expect(
      azureSpotFallbackTimeoutMs({
        capacityMarket: "spot",
        capacityFallback: "on-demand-after-120s",
      }),
    ).toBe(120_000);
    expect(
      azureSpotFallbackTimeoutMs({
        capacityMarket: "spot",
        capacityFallback: "on-demand-after-2m",
      }),
    ).toBe(120_000);
    expect(
      azureSpotFallbackTimeoutMs({
        capacityMarket: "on-demand",
        capacityFallback: "on-demand-after-120s",
      }),
    ).toBeUndefined();
  });

  it("starts Azure on-demand fallback without waiting for timed-out Spot cleanup", async () => {
    vi.useFakeTimers();
    try {
      const cleanupRequests: AzureDeferredCleanupRequest[] = [];
      const client = new AzureClient(baseEnv, {
        deferredCleanup: (request) => {
          cleanupRequests.push(request);
          return Promise.resolve();
        },
      });
      const vmPutPaths: string[] = [];
      const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const url = typeof input === "string" ? input : input.toString();
        const pathname = new URL(url).pathname;
        if (isAzureLoginURL(url)) {
          return Promise.resolve(
            new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), {
              status: 200,
            }),
          );
        }
        if (url === "https://management.azure.com/spot-op") {
          return Promise.resolve(new Response(JSON.stringify({ status: "InProgress" })));
        }
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
        if (init?.method === "GET" && pathname.includes("/publicIPAddresses/")) {
          return Promise.resolve(
            new Response(JSON.stringify({ properties: { ipAddress: "192.0.2.10" } }), {
              status: 200,
            }),
          );
        }
        if (init?.method === "GET" && pathname.includes("/virtualMachines/")) {
          const name = pathname.split("/").pop() ?? "crabbox-blue-lobster";
          return Promise.resolve(
            new Response(
              JSON.stringify({
                name,
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
        if (init?.method === "PUT" && pathname.includes("/virtualMachines/")) {
          vmPutPaths.push(pathname);
          const name = pathname.split("/").pop() ?? "crabbox-blue-lobster";
          if (vmPutPaths.length === 1) {
            return Promise.resolve(
              new Response("", {
                status: 202,
                headers: { "azure-asyncoperation": "https://management.azure.com/spot-op" },
              }),
            );
          }
          return Promise.resolve(
            new Response(
              JSON.stringify({
                name,
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

      const create = client.createServerWithFallback(
        testLeaseConfig({ capacityFallback: "on-demand-after-1ms" }),
        "cbx_123456789abc",
        "blue-lobster",
        "owner",
      );
      await vi.advanceTimersByTimeAsync(1);
      const result = await create;

      expect(result.market).toBe("on-demand");
      expect(cleanupRequests).toMatchObject([
        {
          location: "eastus",
          subscription: "sub",
          resourceGroup: "crabbox-leases",
          leaseID: "cbx_123456789abc",
          slug: "blue-lobster",
          owner: "owner",
        },
      ]);
      expect(vmPutPaths).toHaveLength(2);
      expect(vmPutPaths[1]).not.toBe(vmPutPaths[0]);
    } finally {
      vi.useRealTimers();
    }
  });

  it("returns an exact Azure cleanup claim when provisioning rollback fails", async () => {
    const client = new AzureClient(baseEnv);
    const internal = client as unknown as {
      createVM(
        config: LeaseConfig,
        location: string,
        leaseID: string,
        slug: string,
        owner: string,
        infra: { vnet: string; nsg: string },
      ): Promise<unknown>;
      createVMUnchecked(): Promise<unknown>;
    };
    vi.spyOn(internal, "createVMUnchecked").mockRejectedValue(new Error("create failed"));
    vi.spyOn(client, "deleteOwnedServer").mockRejectedValue(new Error("cleanup unavailable"));

    const error = await internal
      .createVM(
        testLeaseConfig(),
        "eastus",
        "cbx_123456789abc",
        "blue-lobster",
        "alice@example.com",
        { vnet: "crabbox-vnet", nsg: "crabbox-nsg" },
      )
      .catch((caught: unknown) => caught);

    expect(providerProvisioningCleanupClaim(error)).toEqual({
      provider: "azure",
      cloudID: "crabbox-blue-lobster-ea753034",
      region: "eastus",
      providerScope: "/subscriptions/sub/resourceGroups/crabbox-leases",
    });
  });

  it("bounds stalled Azure on-demand VM creates so SKU fallback can continue", async () => {
    vi.useFakeTimers();
    try {
      const client = new AzureClient(baseEnv);
      const vmSizes: string[] = [];
      const fakeFetch = ((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const url = typeof input === "string" ? input : input.toString();
        const pathname = new URL(url).pathname;
        if (isAzureLoginURL(url)) {
          return Promise.resolve(
            new Response(JSON.stringify({ access_token: "tkn", expires_in: 3600 }), {
              status: 200,
            }),
          );
        }
        if (url === "https://management.azure.com/on-demand-op") {
          return Promise.resolve(new Response(JSON.stringify({ status: "InProgress" })));
        }
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
        if (init?.method === "GET" && pathname.includes("/publicIPAddresses/")) {
          return Promise.resolve(
            new Response(JSON.stringify({ properties: { ipAddress: "192.0.2.10" } }), {
              status: 200,
            }),
          );
        }
        if (init?.method === "GET" && pathname.includes("/virtualMachines/")) {
          const name = pathname.split("/").pop() ?? "crabbox-blue-lobster";
          return Promise.resolve(
            new Response(
              JSON.stringify({
                name,
                tags: { crabbox: "true" },
                properties: {
                  provisioningState: "Succeeded",
                  hardwareProfile: { vmSize: vmSizes.at(-1) ?? "Standard_D32ds_v6" },
                },
              }),
              { status: 200 },
            ),
          );
        }
        if (init?.method === "PUT" && pathname.includes("/virtualMachines/")) {
          const body = init.body ? JSON.parse(String(init.body)) : {};
          const vmSize = String(body.properties?.hardwareProfile?.vmSize ?? "");
          vmSizes.push(vmSize);
          const name = pathname.split("/").pop() ?? "crabbox-blue-lobster";
          if (vmSizes.length === 1) {
            return Promise.resolve(
              new Response("", {
                status: 202,
                headers: { "azure-asyncoperation": "https://management.azure.com/on-demand-op" },
              }),
            );
          }
          return Promise.resolve(
            new Response(
              JSON.stringify({
                name,
                tags: { crabbox: "true" },
                properties: {
                  provisioningState: "Succeeded",
                  hardwareProfile: { vmSize },
                },
              }),
              { status: 200 },
            ),
          );
        }
        return Promise.resolve(new Response("{}", { status: 200 }));
      }) as typeof fetch;
      client.fetcher = fakeFetch;

      const create = client.createServerWithFallback(
        testLeaseConfig({
          capacityMarket: "on-demand",
          serverType: "Standard_D32ads_v6",
          serverTypeExplicit: false,
        }),
        "cbx_123456789abc",
        "blue-lobster",
        "owner",
      );
      await vi.advanceTimersByTimeAsync(180_000);
      const result = await create;

      expect(result.market).toBe("on-demand");
      expect(result.serverType).toBe("Standard_D32ds_v6");
      expect(vmSizes.slice(0, 2)).toEqual(["Standard_D32ads_v6", "Standard_D32ds_v6"]);
      expect(result.attempts?.[0]).toMatchObject({
        market: "on-demand",
        serverType: "Standard_D32ads_v6",
        category: "capacity",
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it("cleans each exact Azure SKU attempt before creating the next public IP", async () => {
    const { records, storage } = memoryAzureDeleteClaimStorage();
    const client = new AzureClient(baseEnv, { ownedDeleteClaimStorage: storage });
    seedAzureAuthCache(client);
    const resources = new Map<string, Record<string, unknown>>();
    const deleted: string[] = [];
    const vmSizes: string[] = [];
    const vmNames: string[] = [];
    let firstAttemptClearedBeforeNextNetwork = false;
    const resourcePrefix = "/subscriptions/sub/resourceGroups/crabbox-leases/providers";
    const unrelatedNIC = `${resourcePrefix}/Microsoft.Network/networkInterfaces/crabbox-unrelated-nic`;
    const sharedNSG = `${resourcePrefix}/Microsoft.Network/networkSecurityGroups/crabbox-nsg`;
    resources.set(unrelatedNIC, { id: unrelatedNIC, name: "crabbox-unrelated-nic" });
    client.fetcher = async (input, init) => {
      const url = new URL(String(input));
      const path = url.pathname;
      if (path.endsWith("/resourceGroups/crabbox-leases")) {
        return Response.json({ tags: { managed_by: "crabbox" } });
      }
      if (path.endsWith("/virtualNetworks/crabbox-vnet")) {
        return Response.json({ tags: { managed_by: "crabbox" } });
      }
      if (path.endsWith("/networkSecurityGroups/crabbox-nsg") && init?.method === "GET") {
        return Response.json({
          tags: { managed_by: "crabbox" },
          properties: { securityRules: [] },
        });
      }
      if (init?.method === "DELETE") {
        deleted.push(path);
        resources.delete(path);
        if (path.includes("/virtualMachines/")) {
          const name = path.split("/").pop() ?? "";
          const diskID = `${resourcePrefix}/Microsoft.Compute/disks/${name}-osdisk`;
          const disk = resources.get(diskID);
          if (disk) resources.set(diskID, { ...disk, managedBy: undefined });
        }
        return new Response(null, { status: 204 });
      }
      if (init?.method === "PUT") {
        const body = JSON.parse(String(init.body)) as Record<string, unknown>;
        const name = path.split("/").pop() ?? "";
        if (path.includes("/publicIPAddresses/") && vmNames.length === 1) {
          const firstName = vmNames[0]!;
          const firstNIC = `${resourcePrefix}/Microsoft.Network/networkInterfaces/${firstName}-nic`;
          const firstPIP = `${resourcePrefix}/Microsoft.Network/publicIPAddresses/${firstName}-pip`;
          firstAttemptClearedBeforeNextNetwork =
            !resources.has(firstNIC) &&
            !resources.has(firstPIP) &&
            records.size === 0 &&
            deleted.length === 2;
        }
        let diskID: string | undefined;
        if (path.includes("/virtualMachines/")) {
          const properties = body["properties"] as {
            hardwareProfile?: { vmSize?: string };
          };
          vmSizes.push(properties.hardwareProfile?.vmSize ?? "");
          vmNames.push(name);
          if (vmSizes.length === 1) {
            return Response.json(
              { error: { code: "SkuNotAvailable", message: "requested size unavailable" } },
              { status: 409 },
            );
          }
          diskID = `${resourcePrefix}/Microsoft.Compute/disks/${name}-osdisk`;
          resources.set(diskID, {
            id: diskID,
            name: `${name}-osdisk`,
            location: body["location"],
            managedBy: path,
            tags: body["tags"],
            properties: { uniqueId: `${name}-disk-guid` },
          });
        }
        const bodyProperties = body["properties"] as Record<string, unknown>;
        const storageProfile = bodyProperties["storageProfile"] as
          | { osDisk?: Record<string, unknown> }
          | undefined;
        resources.set(path, {
          ...body,
          id: path,
          name,
          properties: {
            ...bodyProperties,
            ...(path.includes("/networkInterfaces/") ? { resourceGuid: `${name}-guid` } : {}),
            ...(path.includes("/publicIPAddresses/")
              ? { resourceGuid: `${name}-guid`, ipAddress: "192.0.2.10" }
              : {}),
            ...(path.includes("/virtualMachines/")
              ? {
                  vmId: `${name}-vm-guid`,
                  provisioningState: "Succeeded",
                  hardwareProfile: { vmSize: vmSizes.at(-1) },
                  storageProfile: {
                    ...storageProfile,
                    osDisk: {
                      ...storageProfile?.osDisk,
                      managedDisk: {
                        ...(storageProfile?.osDisk?.["managedDisk"] as object),
                        id: diskID,
                      },
                    },
                  },
                }
              : {}),
          },
        });
        return Response.json(resources.get(path));
      }
      if (init?.method === "PATCH") {
        const resource = resources.get(path);
        if (!resource) return azureResourceNotFoundResponse(url);
        const body = JSON.parse(String(init.body)) as { tags?: Record<string, string> };
        resources.set(path, { ...resource, tags: body.tags ?? resource["tags"] });
        return Response.json(resources.get(path));
      }
      const resource = resources.get(path);
      return resource ? Response.json(resource) : azureResourceNotFoundResponse(url);
    };

    const result = await client.createServerWithFallback(
      testLeaseConfig({
        capacityMarket: "on-demand",
        serverType: "Standard_D32ads_v6",
        serverTypeExplicit: false,
      }),
      "cbx_123456789abc",
      "blue-lobster",
      "owner",
    );

    expect(vmSizes.slice(0, 2)).toEqual(["Standard_D32ads_v6", "Standard_D32ds_v6"]);
    expect(result.serverType).toBe("Standard_D32ds_v6");
    expect(result.attempts?.[0]).toMatchObject({
      serverType: "Standard_D32ads_v6",
      category: "capacity",
    });
    expect(firstAttemptClearedBeforeNextNetwork).toBe(true);
    const [firstName, secondName] = vmNames as [string, string];
    const firstNIC = `${resourcePrefix}/Microsoft.Network/networkInterfaces/${firstName}-nic`;
    const firstPIP = `${resourcePrefix}/Microsoft.Network/publicIPAddresses/${firstName}-pip`;
    const secondNIC = `${resourcePrefix}/Microsoft.Network/networkInterfaces/${secondName}-nic`;
    const secondPIP = `${resourcePrefix}/Microsoft.Network/publicIPAddresses/${secondName}-pip`;
    const secondVM = `${resourcePrefix}/Microsoft.Compute/virtualMachines/${secondName}`;
    const secondDisk = `${resourcePrefix}/Microsoft.Compute/disks/${secondName}-osdisk`;
    expect(deleted).toEqual([firstNIC, firstPIP]);
    expect(resources.has(firstNIC)).toBe(false);
    expect(resources.has(firstPIP)).toBe(false);
    expect(resources.has(`${resourcePrefix}/Microsoft.Compute/virtualMachines/${firstName}`)).toBe(
      false,
    );
    expect(resources.has(`${resourcePrefix}/Microsoft.Compute/disks/${firstName}-osdisk`)).toBe(
      false,
    );
    expect(resources.has(secondNIC)).toBe(true);
    expect(resources.has(secondPIP)).toBe(true);
    expect(resources.has(secondVM)).toBe(true);
    expect(resources.has(secondDisk)).toBe(true);
    expect(resources.has(unrelatedNIC)).toBe(true);
    expect(result.server.cloudID).toBe(secondName);
    expect(records.size).toBe(0);

    await client.deleteOwnedServer({
      id: "cbx_123456789abc",
      slug: "blue-lobster",
      provider: "azure",
      cloudID: secondName,
      owner: "owner",
      providerScope: "/subscriptions/sub/resourceGroups/crabbox-leases",
    });

    expect(deleted).toEqual([firstNIC, firstPIP, secondVM, secondNIC, secondPIP, secondDisk]);
    expect([...resources.keys()]).toEqual([unrelatedNIC, sharedNSG]);
    expect(records.size).toBe(0);
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

  it("uses a conservative ephemeral full-caching fallback", () => {
    expect(azureSupportsEphemeralFullCaching("Standard_D2ads_v6")).toBe(false);
    expect(azureSupportsEphemeralFullCaching("Standard_D4ads_v6")).toBe(false);
    expect(azureSupportsEphemeralFullCaching("Standard_D8ads_v6")).toBe(true);
    expect(azureSupportsEphemeralFullCaching("Standard_D32ads_v6")).toBe(true);
    expect(azureSupportsEphemeralFullCaching("Standard_D32pds_v6")).toBe(true);
    expect(azureSupportsEphemeralFullCaching("Standard_D32ps_v6")).toBe(false);
    expect(azureSupportsEphemeralFullCaching("Standard_D32as_v6")).toBe(false);
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

  it("keeps a VM with a missing canonical NIC report-only in reconciliation", async () => {
    const client = new AzureClient(baseEnv);
    const cloudID = "crabbox-blue-lobster";
    const resourcePrefix = "/subscriptions/sub/resourceGroups/crabbox-leases/providers";
    const vmID = `${resourcePrefix}/Microsoft.Compute/virtualMachines/${cloudID}`;
    const nicID = `${resourcePrefix}/Microsoft.Network/networkInterfaces/${cloudID}-nic`;
    client.fetcher = async (input) => {
      const url = new URL(String(input));
      if (isAzureLoginURL(url.toString())) {
        return Response.json({ access_token: "tkn", expires_in: 3600 });
      }
      if (url.pathname.endsWith("/virtualMachines")) {
        return Response.json({
          value: [
            {
              id: vmID,
              name: cloudID,
              location: "eastus",
              tags: ownedAzureTags(),
              properties: {
                vmId: "vm-immutable-id",
                networkProfile: { networkInterfaces: [{ id: nicID }] },
              },
            },
          ],
        });
      }
      if (
        url.pathname.endsWith("/networkInterfaces") ||
        url.pathname.endsWith("/publicIPAddresses") ||
        url.pathname.endsWith("/disks")
      ) {
        return Response.json({ value: [] });
      }
      return new Response("unexpected request", { status: 500 });
    };

    const machines = await client.listReconciliationResources();
    expect(machines).toHaveLength(1);
    expect(machines[0]?.labels["lease"]).toBe("");
    expect(machines[0]?.resourceIdentity).toContain("vm-immutable-id");
  });

  it("lists VM-less Azure resources and rejects mixed ownership and topology", async () => {
    const client = new AzureClient(baseEnv);
    const cloudID = "crabbox-blue-lobster";
    const tags = ownedAzureTags({ server_type: "Standard_D4ads_v6" });
    let publicIPTags = tags;
    let nicVirtualMachineID: string | undefined;
    let nicPublicIPID: string | undefined;
    let nicLocation = "eastus";
    let nicResourceGuid = "nic-immutable-id";
    let publicIPConfigurationID: string | undefined;
    let publicIPNatGatewayID: string | undefined;
    let includeNIC = true;
    let includePublicIP = true;
    let includeDisk = true;
    let includeVMWithDataDisk = false;
    let diskManagedBy: string | undefined;
    let diskManagedByExtended: string[] | undefined;
    let nicServiceAssociation:
      | {
          privateEndpoint?: { id: string };
          ipConfigurationProperties?: Record<string, unknown>;
        }
      | undefined;
    const resourcePrefix = "/subscriptions/sub/resourceGroups/crabbox-leases/providers";
    const nicID = `${resourcePrefix}/Microsoft.Network/networkInterfaces/${cloudID}-nic`;
    const pipID = `${resourcePrefix}/Microsoft.Network/publicIPAddresses/${cloudID}-pip`;
    const diskID = `${resourcePrefix}/Microsoft.Compute/disks/${cloudID}-osdisk`;
    const canonicalVMID = `${resourcePrefix}/Microsoft.Compute/virtualMachines/${cloudID}`;
    const nicIPConfigurationID = `${nicID}/ipConfigurations/ipconfig1`;
    const inventoryPaths: string[] = [];
    nicPublicIPID = pipID;
    const fakeFetch = ((input: RequestInfo | URL): Promise<Response> => {
      const url = new URL(String(input));
      if (isAzureLoginURL(url.toString())) {
        return Promise.resolve(Response.json({ access_token: "tkn", expires_in: 3600 }));
      }
      inventoryPaths.push(url.pathname);
      if (url.pathname.endsWith("/virtualMachines")) {
        return Promise.resolve(
          Response.json({
            value: includeVMWithDataDisk
              ? [
                  {
                    id: canonicalVMID,
                    name: cloudID,
                    location: "eastus",
                    tags,
                    properties: {
                      vmId: "vm-immutable-id",
                      networkProfile: { networkInterfaces: [{ id: nicID }] },
                      storageProfile: {
                        osDisk: { managedDisk: { id: diskID } },
                        dataDisks: [
                          {
                            name: "shared-data",
                            lun: 0,
                            deleteOption: "Delete",
                            managedDisk: {
                              id: `${resourcePrefix}/Microsoft.Compute/disks/shared-data`,
                            },
                          },
                        ],
                      },
                    },
                  },
                ]
              : [],
          }),
        );
      }
      if (url.pathname.endsWith("/networkInterfaces")) {
        return Promise.resolve(
          Response.json({
            value: includeNIC
              ? [
                  {
                    id: nicID,
                    name: `${cloudID}-nic`,
                    location: nicLocation,
                    tags,
                    properties: {
                      ...(nicVirtualMachineID
                        ? { virtualMachine: { id: nicVirtualMachineID } }
                        : {}),
                      resourceGuid: nicResourceGuid,
                      ipConfigurations: [
                        {
                          id: nicIPConfigurationID,
                          properties: {
                            ...(nicPublicIPID ? { publicIPAddress: { id: nicPublicIPID } } : {}),
                            ...nicServiceAssociation?.ipConfigurationProperties,
                          },
                        },
                      ],
                      ...(nicServiceAssociation?.privateEndpoint
                        ? { privateEndpoint: nicServiceAssociation.privateEndpoint }
                        : {}),
                    },
                  },
                ]
              : [],
          }),
        );
      }
      if (url.pathname.endsWith("/publicIPAddresses")) {
        return Promise.resolve(
          Response.json({
            value: includePublicIP
              ? [
                  {
                    id: pipID,
                    name: `${cloudID}-pip`,
                    location: "eastus",
                    tags: publicIPTags,
                    properties: {
                      resourceGuid: "pip-immutable-id",
                      ipAddress: "192.0.2.20",
                      ...(publicIPConfigurationID
                        ? { ipConfiguration: { id: publicIPConfigurationID } }
                        : {}),
                      ...(publicIPNatGatewayID ? { natGateway: { id: publicIPNatGatewayID } } : {}),
                    },
                  },
                ]
              : [],
          }),
        );
      }
      if (url.pathname.endsWith("/disks")) {
        return Promise.resolve(
          Response.json({
            value: includeDisk
              ? [
                  {
                    id: diskID,
                    name: `${cloudID}-osdisk`,
                    location: "eastus",
                    managedBy: diskManagedBy,
                    ...(diskManagedByExtended ? { managedByExtended: diskManagedByExtended } : {}),
                    tags,
                    properties: { uniqueId: "disk-immutable-id" },
                  },
                ]
              : [],
          }),
        );
      }
      return Promise.resolve(new Response("unexpected request", { status: 500 }));
    }) as typeof fetch;
    client.fetcher = fakeFetch;

    const machines = await client.listReconciliationResources();

    expect(machines).toHaveLength(1);
    expect(machines[0]).toMatchObject({
      provider: "azure",
      id: 0,
      cloudID,
      name: cloudID,
      status: "provisioning",
      serverType: "Standard_D4ads_v6",
      region: "eastus",
      host: "192.0.2.20",
      labels: azureLabelsFromTags(tags),
    });
    expect(machines[0]?.resourceIdentity).toEqual(expect.any(String));
    expect(machines[0]?.resourceIdentity?.length).toBeGreaterThan(0);
    expect(inventoryPaths).toHaveLength(4);
    expect(
      inventoryPaths.every((path) =>
        /\/(virtualMachines|networkInterfaces|publicIPAddresses|disks)$/.test(path),
      ),
    ).toBe(true);

    includeVMWithDataDisk = true;
    nicVirtualMachineID = canonicalVMID;
    diskManagedBy = canonicalVMID;
    const withDataDisk = await client.listReconciliationResources();
    expect(withDataDisk[0]?.labels["lease"]).toBe("");
    expect(withDataDisk[0]?.resourceIdentity).toContain("shared-data");
    includeVMWithDataDisk = false;
    nicVirtualMachineID = undefined;
    diskManagedBy = undefined;

    diskManagedBy = canonicalVMID;
    const canonicalDisk = await client.listReconciliationResources();
    expect(canonicalDisk[0]?.labels["lease"]).toBe(tags.lease);
    expect(canonicalDisk[0]?.resourceIdentity).not.toBe(machines[0]?.resourceIdentity);

    diskManagedBy =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-other";
    const attachedDisk = await client.listReconciliationResources();
    expect(attachedDisk[0]?.labels["lease"]).toBe("");
    expect(attachedDisk[0]?.resourceIdentity).not.toBe(canonicalDisk[0]?.resourceIdentity);

    diskManagedBy = undefined;
    diskManagedByExtended = [
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-other",
    ];
    const attachedExtendedDisk = await client.listReconciliationResources();
    expect(attachedExtendedDisk[0]?.labels["lease"]).toBe("");
    expect(attachedExtendedDisk[0]?.resourceIdentity).not.toBe(machines[0]?.resourceIdentity);

    diskManagedByExtended = [canonicalVMID];
    const canonicalExtendedDisk = await client.listReconciliationResources();
    expect(canonicalExtendedDisk[0]?.labels["lease"]).toBe(tags.lease);
    expect(canonicalExtendedDisk[0]?.resourceIdentity).not.toBe(machines[0]?.resourceIdentity);

    diskManagedByExtended = undefined;
    publicIPConfigurationID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Network/networkInterfaces/other-nic/ipConfigurations/ipconfig1";
    const attachedPublicIP = await client.listReconciliationResources();
    expect(attachedPublicIP[0]?.labels["lease"]).toBe("");
    expect(attachedPublicIP[0]?.resourceIdentity).not.toBe(machines[0]?.resourceIdentity);

    publicIPConfigurationID = undefined;
    nicPublicIPID =
      "/subscriptions/sub/resourceGroups/shared/providers/Microsoft.Network/publicIPAddresses/shared-pip";
    const foreignForwardWithCanonicalPIP = await client.listReconciliationResources();
    expect(foreignForwardWithCanonicalPIP[0]?.labels["lease"]).toBe("");
    expect(foreignForwardWithCanonicalPIP[0]?.resourceIdentity).not.toBe(
      machines[0]?.resourceIdentity,
    );

    includePublicIP = false;
    const foreignForwardWithoutCanonicalPIP = await client.listReconciliationResources();
    expect(foreignForwardWithoutCanonicalPIP[0]?.labels["lease"]).toBe("");
    expect(foreignForwardWithoutCanonicalPIP[0]?.resourceIdentity).not.toBe(
      machines[0]?.resourceIdentity,
    );

    nicPublicIPID = pipID;
    const incompleteForward = await client.listReconciliationResources();
    expect(incompleteForward[0]?.labels["lease"]).toBe("");
    expect(incompleteForward[0]?.resourceIdentity).not.toBe(
      foreignForwardWithoutCanonicalPIP[0]?.resourceIdentity,
    );

    includePublicIP = true;
    publicIPNatGatewayID =
      "/subscriptions/sub/resourceGroups/shared/providers/Microsoft.Network/natGateways/shared-nat";
    const attachedNATGateway = await client.listReconciliationResources();
    expect(attachedNATGateway[0]?.labels["lease"]).toBe("");
    expect(attachedNATGateway[0]?.resourceIdentity).not.toBe(machines[0]?.resourceIdentity);

    publicIPNatGatewayID = undefined;
    nicVirtualMachineID =
      "/subscriptions/sub/resourceGroups/crabbox-leases/providers/Microsoft.Compute/virtualMachines/crabbox-other";
    const attachedNIC = await client.listReconciliationResources();
    expect(attachedNIC[0]?.labels["lease"]).toBe("");
    expect(attachedNIC[0]?.resourceIdentity).not.toBe(attachedPublicIP[0]?.resourceIdentity);

    nicVirtualMachineID = undefined;
    const serviceAssociations = [
      {
        ipConfigurationProperties: {
          loadBalancerBackendAddressPools: [
            { id: `${nicID}/loadBalancerBackendAddressPools/pool` },
          ],
        },
      },
      {
        ipConfigurationProperties: {
          applicationGatewayBackendAddressPools: [
            { id: `${nicID}/applicationGatewayBackendAddressPools/pool` },
          ],
        },
      },
      {
        ipConfigurationProperties: {
          applicationSecurityGroups: [
            {
              id: ` /SUBSCRIPTIONS/SUB/RESOURCEGROUPS/SHARED/PROVIDERS/MICROSOFT.NETWORK/APPLICATIONSECURITYGROUPS/SHARED `,
            },
          ],
        },
      },
      {
        ipConfigurationProperties: {
          loadBalancerInboundNatRules: [{ id: `${nicID}/loadBalancerInboundNatRules/rule` }],
        },
      },
      {
        privateEndpoint: {
          id: `${resourcePrefix}/Microsoft.Network/privateEndpoints/foreign-private-endpoint`,
        },
      },
    ];
    let applicationSecurityGroupResourceIdentity: string | undefined;
    for (const association of serviceAssociations) {
      nicServiceAssociation = association;
      // oxlint-disable-next-line eslint/no-await-in-loop -- each association must be observed after mutating the fixture.
      const attachedService = await client.listReconciliationResources();
      expect(attachedService[0]?.labels["lease"]).toBe("");
      expect(attachedService[0]?.resourceIdentity).not.toBe(machines[0]?.resourceIdentity);
      if ("applicationSecurityGroups" in (association.ipConfigurationProperties ?? {})) {
        applicationSecurityGroupResourceIdentity = attachedService[0]?.resourceIdentity;
      }
    }
    expect(applicationSecurityGroupResourceIdentity).toEqual(expect.any(String));
    const identityEntries = JSON.parse(applicationSecurityGroupResourceIdentity ?? "[]") as Array<{
      kind: string;
      topology?: Record<string, unknown>;
    }>;
    const nicEntry = identityEntries.find((entry) => entry.kind === "networkInterfaces");
    expect(nicEntry?.topology?.applicationSecurityGroupIDs).toEqual([
      "subscriptions/sub/resourcegroups/shared/providers/microsoft.network/applicationsecuritygroups/shared",
    ]);
    nicServiceAssociation = undefined;

    publicIPTags = { ...tags, owner: "other_owner" };
    const mixed = await client.listReconciliationResources();
    expect(mixed).toHaveLength(1);
    expect(mixed[0]?.labels["lease"]).toBe("");
    expect(mixed[0]?.resourceIdentity).not.toBe(machines[0]?.resourceIdentity);

    publicIPTags = tags;
    nicLocation = "";
    const incompleteLocation = await client.listReconciliationResources();
    expect(incompleteLocation[0]?.labels["lease"]).toBe("");

    nicLocation = "eastus";
    nicResourceGuid = "   ";
    const incompleteImmutableIdentity = await client.listReconciliationResources();
    expect(incompleteImmutableIdentity[0]?.labels["lease"]).toBe("");

    nicResourceGuid = "nic-immutable-id";
    includeNIC = false;
    includePublicIP = false;
    const loneDisk = await client.listReconciliationResources();
    expect(loneDisk[0]?.labels["lease"]).toBe("");

    includeNIC = true;
    const nicAndDisk = await client.listReconciliationResources();
    expect(nicAndDisk[0]?.labels["lease"]).toBe("");

    includePublicIP = true;
    includeDisk = false;
    const nicAndPublicIP = await client.listReconciliationResources();
    expect(nicAndPublicIP[0]?.labels["lease"]).toBe("");
  });
});
