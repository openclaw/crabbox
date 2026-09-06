import { cloudInit } from "./bootstrap";
import {
  concreteStoredServerType,
  gcpMachineTypeCandidatesForClass,
  isCanonicalProviderClass,
  sshPorts,
  validatedCIDRs,
  uniqueProviderMachineCandidates,
  type LeaseConfig,
} from "./config";
import { ExpiringTokenCache, type ExpiringToken } from "./expiring-token-cache";
import {
  leaseProviderLabels,
  providerLabelValue,
  providerMachineOwnedByLease,
} from "./provider-labels";
import {
  ProviderProvisioningCleanupError,
  ProviderProvisioningOutcomeUncertainError,
  ProviderResourceUnresolvedError,
  providerProvisioningCleanupClaim,
  type ProviderProvisioningCleanupClaim,
} from "./provider-provisioning";
import { ProvisioningAttemptHistory } from "./provisioning-attempts";
import { leaseProviderName } from "./slug";
import type {
  Env,
  LeaseImageIdentity,
  LeaseRecord,
  ProviderCheckpointOwnership,
  ProviderImage,
  ProviderMachine,
  ProvisioningAttempt,
} from "./types";

const computeBaseURL = "https://compute.googleapis.com/compute/v1";
const gcpReadyPoolResourcePattern =
  /^(?:https:\/\/(?:compute|www)\.googleapis\.com\/compute\/v1\/)?projects\/([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)\/global\/(images|snapshots)\/([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)$/;
const gcpReadyPoolScopePattern =
  /^projects\/[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\/global\/(?:images|snapshots)$/;
const gcpObservedDiskPattern =
  /^(?:https:\/\/(?:compute|www)\.googleapis\.com\/compute\/v1\/)?projects\/([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)\/zones\/([a-z0-9-]+)\/disks\/([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)$/;
const gcpObservedInstancePattern =
  /^(?:https:\/\/(?:compute|www)\.googleapis\.com\/compute\/v1\/)?projects\/([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)\/zones\/([a-z0-9-]+)\/instances\/([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)$/;
const tokenURL = "https://oauth2.googleapis.com/token";
const metadataTokenURL =
  "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token";
const defaultImage = "projects/ubuntu-os-cloud/global/images/family/ubuntu-2604-lts-amd64";
const firewallName = "crabbox-ssh";
const firewallVisibilityBackoffMs = [100, 200, 400, 800, 1_600, 3_200];
const metadataTokenBackoffMs = [1_000, 2_000, 4_000, 8_000, 16_000, 28_000];
const metadataTokenDeadlineMs = 60_000;
const metadataTokenRequestTimeoutMs = 5_000;
const metadataTokenRefreshSkewSeconds = 300;
const serviceAccountTokenRefreshSkewSeconds = 60;

class GCPHTTPError extends Error {
  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    readonly body: string,
  ) {
    super(`gcp ${method} ${path}: http ${status}: ${body}`);
  }
}

class GCPOperationError extends Error {}

export function gcpReadyPoolImageScope(
  sourceID: string | undefined,
  kind: string | undefined,
): string | undefined {
  const match = gcpReadyPoolSourceMatch(sourceID, kind);
  return match ? `projects/${match[1]}/global/${match[2]}` : undefined;
}

export function gcpReadyPoolImageScopeSupported(scope: string): boolean {
  return gcpReadyPoolScopePattern.test(scope);
}

class GCPMetadataTokenRequestError extends Error {}

class GCPMetadataTokenTrustError extends Error {}

interface GCPInstance {
  id?: string;
  name?: string;
  sourceMachineImage?: string;
  status?: string;
  machineType?: string;
  zone?: string;
  labels?: Record<string, string>;
  networkInterfaces?: {
    accessConfigs?: { natIP?: string }[];
  }[];
  disks?: {
    boot?: boolean;
    source?: string;
    type?: string;
  }[];
}

interface GCPAggregatedInstanceList {
  items?: Record<string, { instances?: GCPInstance[] }>;
}

interface GCPOperation {
  name?: string;
  status?: string;
  targetId?: string;
  error?: { errors?: { code?: string; message?: string }[] };
}

interface GCPMachineImage {
  id?: string;
  name?: string;
  selfLink?: string;
  status?: string;
  labels?: Record<string, string>;
}

interface GCPSnapshot {
  id?: string;
  name?: string;
  selfLink?: string;
  status?: string;
  labels?: Record<string, string>;
}

interface GCPDisk {
  id?: string;
  selfLink?: string;
  users?: string[];
  sourceImage?: string;
  sourceImageId?: string;
  sourceSnapshot?: string;
  sourceSnapshotId?: string;
}

export class GCPClient {
  readonly project: string;
  readonly zone: string;
  readonly image: string;
  readonly network: string;
  readonly subnet: string;
  readonly tags: string[];
  readonly sshCIDRs: string[];
  readonly rootGB: number;
  readonly serviceAccount: string;
  fetcher: typeof fetch = (input, init) => fetch(input, init);
  private tokenCache = new ExpiringTokenCache();

  constructor(
    private readonly env: Env,
    zone?: string,
    project?: string,
  ) {
    this.project =
      project?.trim() || env.CRABBOX_GCP_PROJECT?.trim() || env.GCP_PROJECT_ID?.trim() || "";
    this.zone = zone || env.CRABBOX_GCP_ZONE?.trim() || "europe-west2-a";
    this.image = env.CRABBOX_GCP_IMAGE?.trim() || defaultImage;
    this.network = env.CRABBOX_GCP_NETWORK?.trim() || "default";
    this.subnet = env.CRABBOX_GCP_SUBNET?.trim() || "";
    this.tags = uniqueStrings((env.CRABBOX_GCP_TAGS ?? "crabbox-ssh").split(","));
    this.sshCIDRs = validatedCIDRs(
      (env.CRABBOX_GCP_SSH_CIDRS ?? "").split(","),
      "CRABBOX_GCP_SSH_CIDRS",
    );
    if (this.sshCIDRs.length === 0) this.sshCIDRs.push("0.0.0.0/0");
    this.rootGB = numberFromEnv(env.CRABBOX_GCP_ROOT_GB, 400);
    this.serviceAccount = env.CRABBOX_GCP_SERVICE_ACCOUNT?.trim() || "";
    if (!this.project) throw new Error("GCP_PROJECT_ID or CRABBOX_GCP_PROJECT secret is required");
    if (hasPartialServiceAccountCredential(env)) {
      throw new Error("GCP_CLIENT_EMAIL and GCP_PRIVATE_KEY must be configured together");
    }
    const credentialSource = gcpCredentialSource(env);
    if (credentialSource === "service-account-key" && !hasServiceAccountCredential(env)) {
      throw new Error(
        "GCP_CLIENT_EMAIL and GCP_PRIVATE_KEY are required unless CRABBOX_GCP_CREDENTIAL_SOURCE=metadata",
      );
    }
  }

  async listCrabboxServers(): Promise<ProviderMachine[]> {
    const data = await this.gcp<GCPAggregatedInstanceList>(
      "GET",
      `/aggregated/instances?filter=${encodeURIComponent("labels.crabbox = true")}&returnPartialSuccess=true`,
    ).catch((error) => {
      if (isNotFound(error)) return { items: [] };
      throw error;
    });
    return Object.entries(data.items ?? {})
      .flatMap(([scope, list]) => {
        const zone = lastPathPart(scope);
        return (list.instances ?? []).map((instance) =>
          toMachine(instance, lastPathPart(instance.zone ?? zone)),
        );
      })
      .filter(canonicalGCPMachine);
  }

  forScope(zone?: string, project?: string): GCPClient {
    const scopedZone = zone?.trim() || this.zone;
    const scopedProject = project?.trim() || this.project;
    if (scopedZone === this.zone && scopedProject === this.project) {
      return this;
    }
    const client = new GCPClient(this.env, scopedZone, scopedProject);
    client.fetcher = this.fetcher;
    client.tokenCache = this.tokenCache.clone();
    return client;
  }

  async observeReadyPoolImageIdentity(lease: LeaseRecord): Promise<LeaseImageIdentity | undefined> {
    const project = lease.providerProject?.trim() ?? "";
    const zone = lease.region?.trim() ?? "";
    const name = lease.cloudID.trim();
    const providerResourceID = canonicalNumericResourceID(lease.providerResourceID);
    // A missing raw ID has no safe adoption path here; legacy migration remains operator-controlled.
    if (
      lease.provider !== "gcp" ||
      !project ||
      project !== lease.providerProject ||
      !zone ||
      zone !== lease.region ||
      !name ||
      name !== lease.cloudID ||
      name !== lease.serverName ||
      !providerResourceID
    ) {
      return undefined;
    }
    const client = this.forScope(zone, project);
    const instancePath = `projects/${project}/zones/${zone}/instances/${name}`;
    const instance = await client.gcp<GCPInstance>("GET", `/zones/${zone}/instances/${name}`);
    const diskPath = observedOwnedGCPBootDiskPath(
      instance,
      lease,
      project,
      zone,
      providerResourceID,
    );
    if (!diskPath) return undefined;
    const diskName = lastPathPart(diskPath);
    const disk = await client.gcp<GCPDisk>("GET", `/zones/${zone}/disks/${diskName}`);
    const observedDisk = observedGCPBootDisk(disk, diskPath, instancePath, zone);
    if (!observedDisk) return undefined;

    const verifiedInstance = await client.gcp<GCPInstance>(
      "GET",
      `/zones/${zone}/instances/${name}`,
    );
    const verifiedDiskPath = observedOwnedGCPBootDiskPath(
      verifiedInstance,
      lease,
      project,
      zone,
      providerResourceID,
    );
    if (verifiedInstance.id !== instance.id || verifiedDiskPath !== diskPath) {
      return undefined;
    }
    const verifiedDisk = await client.gcp<GCPDisk>("GET", `/zones/${zone}/disks/${diskName}`);
    const verifiedObservation = observedGCPBootDisk(verifiedDisk, diskPath, instancePath, zone);
    return verifiedObservation &&
      verifiedObservation.resourceID === observedDisk.resourceID &&
      sameGCPObservedImageIdentity(verifiedObservation.image, observedDisk.image)
      ? observedDisk.image
      : undefined;
  }

  async recoverServerForLease(lease: LeaseRecord): Promise<ProviderMachine | undefined> {
    const project = lease.providerProject?.trim() ?? "";
    const zone = lease.region?.trim() ?? "";
    const name = lease.cloudID.trim();
    const providerResourceID = canonicalNumericResourceID(lease.providerResourceID);
    if (
      lease.provider !== "gcp" ||
      !project ||
      project !== lease.providerProject ||
      !zone ||
      zone !== lease.region ||
      !name ||
      name !== lease.cloudID ||
      name !== lease.serverName ||
      !providerResourceID
    ) {
      throw new ProviderResourceUnresolvedError(
        `GCP lease ${lease.id} has no exact persisted project, zone, name, and numeric resource id`,
      );
    }
    const client = this.forScope(zone, project);
    let instance: GCPInstance;
    try {
      instance = await client.gcp<GCPInstance>("GET", `/zones/${zone}/instances/${name}`);
    } catch (error) {
      if (gcpInstanceNotFound(error, project, zone, name)) return undefined;
      throw error;
    }
    const server = toMachine(instance, zone);
    if (
      server.providerResourceID !== providerResourceID ||
      !canonicalGCPMachine(server) ||
      !providerMachineOwnedByLease(server, lease, "gcp", gcpProviderLabelValue)
    ) {
      throw new ProviderResourceUnresolvedError(
        `GCP instance ${name} identity or ownership does not match lease ${lease.id}`,
      );
    }
    return server;
  }

  async observeLegacyCleanupIdentity(
    lease: LeaseRecord,
    context?: { resourceIdentity?: string },
  ): Promise<{ providerResourceID: string } | undefined> {
    const project = lease.providerProject?.trim() ?? "";
    const zone = lease.region?.trim() ?? "";
    const name = lease.cloudID.trim();
    const resourceIdentity =
      context?.resourceIdentity === undefined
        ? undefined
        : canonicalNumericResourceID(context.resourceIdentity);
    const historicalServerID =
      Number.isFinite(lease.serverID) && Number.isInteger(lease.serverID) && lease.serverID > 0
        ? lease.serverID
        : undefined;
    if (
      lease.provider !== "gcp" ||
      !canonicalGCPProject(project) ||
      project !== lease.providerProject ||
      !canonicalGCPZone(zone) ||
      zone !== lease.region ||
      !name ||
      name !== lease.cloudID ||
      name !== lease.serverName ||
      name !== leaseProviderName(lease.id, lease.slug) ||
      lease.providerResourceID !== undefined ||
      (context?.resourceIdentity !== undefined && !resourceIdentity) ||
      (resourceIdentity === undefined && historicalServerID === undefined)
    ) {
      throw new ProviderResourceUnresolvedError(
        `GCP lease ${lease.id} has no canonical legacy cleanup identity`,
      );
    }
    const client = this.forScope(zone, project);
    let instance: GCPInstance;
    try {
      instance = await client.gcp<GCPInstance>("GET", `/zones/${zone}/instances/${name}`);
    } catch (error) {
      if (gcpInstanceNotFound(error, project, zone, name)) return undefined;
      throw error;
    }
    const server = toMachine(instance, zone);
    const providerResourceID = canonicalNumericResourceID(server.providerResourceID);
    const identityMatches =
      resourceIdentity === undefined
        ? Number(providerResourceID) === historicalServerID
        : providerResourceID === resourceIdentity;
    if (
      !providerResourceID ||
      !identityMatches ||
      !canonicalGCPMachine(server) ||
      !providerMachineOwnedByLease(server, lease, "gcp", gcpProviderLabelValue)
    ) {
      throw new ProviderResourceUnresolvedError(
        `GCP instance ${name} cannot be bound to legacy cleanup for lease ${lease.id}`,
      );
    }
    return { providerResourceID };
  }

  async recoverUnboundProvisioningResource(
    lease: LeaseRecord,
  ): Promise<ProviderMachine | undefined> {
    const project = lease.providerProject?.trim() ?? "";
    const zone = lease.region?.trim() ?? "";
    const name = leaseProviderName(lease.id, lease.slug);
    const providerResourceID =
      lease.providerResourceID === undefined
        ? undefined
        : canonicalNumericResourceID(lease.providerResourceID);
    if (
      lease.provider !== "gcp" ||
      !canonicalGCPProject(project) ||
      project !== lease.providerProject ||
      !canonicalGCPZone(zone) ||
      zone !== lease.region ||
      lease.cloudID !== "" ||
      lease.serverID !== 0 ||
      (lease.providerResourceID !== undefined && !providerResourceID) ||
      lease.serverName !== ""
    ) {
      throw new ProviderResourceUnresolvedError(
        `GCP lease ${lease.id} has no canonical unbound provisioning scope`,
      );
    }
    const client = this.forScope(zone, project);
    let instance: GCPInstance;
    try {
      instance = await client.gcp<GCPInstance>("GET", `/zones/${zone}/instances/${name}`);
    } catch (error) {
      if (gcpInstanceNotFound(error, project, zone, name)) return undefined;
      throw error;
    }
    const server = toMachine(instance, zone);
    const claimedLease = { ...lease, cloudID: name };
    if (
      !server.providerResourceID ||
      (providerResourceID !== undefined && server.providerResourceID !== providerResourceID) ||
      !canonicalGCPMachine(server) ||
      !providerMachineOwnedByLease(server, claimedLease, "gcp", gcpProviderLabelValue)
    ) {
      throw new ProviderResourceUnresolvedError(
        `GCP instance ${name} cannot be bound to unbound lease ${lease.id}`,
      );
    }
    return server;
  }

  async createServerWithFallback(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
    provisioning?: {
      onTargetAttempt?: (target: { region?: string }) => Promise<void>;
      onResourceCreated?: (claim: ProviderProvisioningCleanupClaim) => Promise<boolean>;
    },
  ): Promise<{
    server: ProviderMachine;
    serverType: string;
    market?: string;
    attempts?: ProvisioningAttempt[];
  }> {
    const candidates = gcpProvisioningCandidatesForConfig(config);
    const zones = prependUnique(
      config.gcpZone || this.zone,
      config.capacityAvailabilityZones.length > 0 ? config.capacityAvailabilityZones : [this.zone],
    );
    const history = new ProvisioningAttemptHistory();
    const project = config.gcpProject || this.project;
    for (const zone of zones) {
      const client = this.forScope(zone, project);
      for (const machineType of candidates) {
        try {
          // Persist the zone before an instance create can outlive the coordinator request.
          // oxlint-disable-next-line eslint/no-await-in-loop -- fallback must preserve capacity order.
          await provisioning?.onTargetAttempt?.({ region: zone });
          // oxlint-disable-next-line eslint/no-await-in-loop -- fallback must preserve capacity order.
          const server = await client.createServer(
            { ...config, gcpZone: zone, serverType: machineType },
            leaseID,
            slug,
            owner,
            provisioning,
          );
          const result: {
            server: ProviderMachine;
            serverType: string;
            market?: string;
            attempts?: ProvisioningAttempt[];
          } = { server, serverType: machineType, market: config.capacityMarket };
          return { ...result, ...history.result() };
        } catch (error) {
          if (
            error instanceof ProviderProvisioningOutcomeUncertainError ||
            error instanceof ProviderResourceUnresolvedError ||
            providerProvisioningCleanupClaim(error)
          ) {
            throw error;
          }
          const message = errorMessage(error);
          history.record(
            {
              region: zone,
              serverType: machineType,
              market: config.capacityMarket,
              category: isFallbackProvisioningError(message) ? "capacity" : "fatal",
              message,
            },
            `${zone}/${machineType}: ${message}`,
          );
          if (!isFallbackProvisioningError(message)) {
            throw history.error("", { cause: error });
          }
        }
      }
    }
    if (config.capacityMarket === "spot" && config.capacityFallback.startsWith("on-demand")) {
      for (const zone of zones) {
        const client = this.forScope(zone, project);
        for (const machineType of candidates) {
          try {
            // Persist the zone before an instance create can outlive the coordinator request.
            // oxlint-disable-next-line eslint/no-await-in-loop -- fallback must preserve capacity order.
            await provisioning?.onTargetAttempt?.({ region: zone });
            // oxlint-disable-next-line eslint/no-await-in-loop -- fallback must preserve capacity order.
            const server = await client.createServer(
              {
                ...config,
                gcpZone: zone,
                serverType: machineType,
                capacityMarket: "on-demand",
              },
              leaseID,
              slug,
              owner,
              provisioning,
            );
            return {
              server,
              serverType: machineType,
              market: "on-demand",
              ...history.result(),
            };
          } catch (error) {
            if (
              error instanceof ProviderProvisioningOutcomeUncertainError ||
              error instanceof ProviderResourceUnresolvedError ||
              providerProvisioningCleanupClaim(error)
            ) {
              throw error;
            }
            const message = errorMessage(error);
            history.record(
              {
                region: zone,
                serverType: machineType,
                market: "on-demand",
                category: isFallbackProvisioningError(message) ? "capacity" : "fatal",
                message,
              },
              `on-demand ${zone}/${machineType}: ${message}`,
            );
            if (!isFallbackProvisioningError(message)) {
              throw history.error("", { cause: error });
            }
          }
        }
      }
    }
    throw history.error();
  }

  async createServer(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
    provisioning?: {
      onResourceCreated?: (claim: ProviderProvisioningCleanupClaim) => Promise<boolean>;
    },
  ): Promise<ProviderMachine> {
    if (config.target !== "linux") {
      throw new Error("brokered gcp currently supports target=linux only");
    }
    await this.ensureFirewall(config);
    const name = leaseProviderName(leaseID, slug);
    const project = config.gcpProject || this.project;
    const labels = gcpLabels(
      leaseProviderLabels(config, leaseID, slug, owner, "gcp", new Date(), {
        market: config.capacityMarket,
      }),
    );
    const instance: Record<string, unknown> = {
      name,
      labels,
      machineType: `zones/${this.zone}/machineTypes/${config.serverType}`,
      tags: { items: gcpEffectiveTags(this.tags, config.gcpTags) },
      metadata: {
        items: [
          { key: "enable-oslogin", value: "FALSE" },
          { key: "ssh-keys", value: `${config.sshUser}:${config.sshPublicKey}` },
          { key: "user-data", value: cloudInit(config) },
        ],
      },
      networkInterfaces: [
        {
          network: this.networkSelfLink(config),
          ...(this.subnetSelfLink(config) ? { subnetwork: this.subnetSelfLink(config) } : {}),
          accessConfigs: [{ name: "External NAT", type: "ONE_TO_ONE_NAT" }],
        },
      ],
    };
    if (!config.gcpMachineImage) {
      const initializeParams: Record<string, unknown> = config.gcpSnapshot
        ? { sourceSnapshot: gcpSnapshotRef(config.gcpSnapshot, project) }
        : {
            sourceImage: config.gcpImage || this.image,
            diskSizeGb: config.gcpRootGB || this.rootGB,
          };
      if (config.gcpSnapshot && config.gcpRootGB > 0) {
        initializeParams["diskSizeGb"] = config.gcpRootGB;
      }
      instance["disks"] = [
        {
          boot: true,
          autoDelete: true,
          type: "PERSISTENT",
          initializeParams: {
            ...initializeParams,
            diskType: `zones/${this.zone}/diskTypes/pd-balanced`,
          },
        },
      ];
    }
    if (config.gcpServiceAccount || this.serviceAccount) {
      instance["serviceAccounts"] = [
        {
          email: config.gcpServiceAccount || this.serviceAccount,
          scopes: ["https://www.googleapis.com/auth/cloud-platform"],
        },
      ];
    }
    if (config.capacityMarket === "spot") {
      instance["scheduling"] = {
        provisioningModel: "SPOT",
        instanceTerminationAction: "DELETE",
        automaticRestart: false,
        onHostMaintenance: "TERMINATE",
      };
    }
    const path = config.gcpMachineImage
      ? `/zones/${this.zone}/instances?sourceMachineImage=${encodeURIComponent(gcpMachineImageRef(config.gcpMachineImage, project))}`
      : `/zones/${this.zone}/instances`;
    const claim: ProviderProvisioningCleanupClaim = {
      provider: "gcp",
      cloudID: name,
      region: this.zone,
      providerProject: project,
    };
    let recoveredCollision: ProviderMachine | undefined;
    try {
      await this.insertInstanceAndWait(path, instance, claim);
    } catch (error) {
      if (provisioning?.onResourceCreated) {
        if (error instanceof GCPOperationError) {
          delete claim.providerResourceID;
        }
        recoveredCollision = await this.ownedCreateCollision(error, name, leaseID, slug, owner);
        if (!recoveredCollision) throw error;
        const collisionResourceID = recoveredCollision.providerResourceID;
        if (!collisionResourceID) {
          throw new ProviderResourceUnresolvedError(
            `GCP instance ${name} collision has no numeric resource id`,
          );
        }
        claim.providerResourceID = collisionResourceID;
      } else {
        await this.rollbackDirectCreate(name, leaseID, slug, owner, claim, error);
        throw error;
      }
    }
    if (provisioning?.onResourceCreated) {
      if (!claim.providerResourceID) {
        try {
          const created = await this.gcp<GCPInstance>(
            "GET",
            `/zones/${this.zone}/instances/${name}`,
          );
          const machine = toMachine(created, this.zone);
          if (
            !machine.providerResourceID ||
            !canonicalGCPMachine(machine) ||
            !providerMachineOwnedByLease(
              machine,
              { id: leaseID, slug, owner, provider: "gcp", cloudID: name },
              "gcp",
              gcpProviderLabelValue,
            )
          ) {
            throw new Error("instance identity or ownership is incomplete");
          }
          claim.providerResourceID = machine.providerResourceID;
        } catch (error) {
          throw new ProviderProvisioningOutcomeUncertainError(
            `GCP instance ${name} was created but its numeric resource id is unconfirmed: ${errorMessage(error)}`,
            { cause: error },
          );
        }
      }
      let continueReadiness: boolean;
      try {
        continueReadiness = await provisioning.onResourceCreated(claim);
      } catch (error) {
        if (error instanceof ProviderResourceUnresolvedError) throw error;
        throw new ProviderProvisioningCleanupError(
          `${errorMessage(error)}; GCP instance ${name} cleanup remains pending`,
          claim,
          error,
        );
      }
      if (!continueReadiness) {
        return pendingMachine(name, config.serverType, this.zone, labels);
      }
      if (recoveredCollision) {
        throw new ProviderResourceUnresolvedError(
          `GCP instance ${name} collision is owned but not bound to this create attempt`,
        );
      }
      try {
        const created = await this.gcp<GCPInstance>("GET", `/zones/${this.zone}/instances/${name}`);
        const machine = toMachine(created, this.zone);
        if (claim.providerResourceID && machine.providerResourceID !== claim.providerResourceID) {
          throw new ProviderResourceUnresolvedError(
            `GCP instance ${name} numeric resource id changed after creation`,
          );
        }
        return machine;
      } catch (error) {
        if (error instanceof ProviderResourceUnresolvedError) throw error;
        throw new ProviderProvisioningCleanupError(
          `${errorMessage(error)}; GCP instance ${name} cleanup remains pending`,
          claim,
          error,
        );
      }
    }
    try {
      const created = await this.gcp<GCPInstance>("GET", `/zones/${this.zone}/instances/${name}`);
      return toMachine(created, this.zone);
    } catch (error) {
      await this.rollbackDirectCreate(name, leaseID, slug, owner, claim, error);
      throw error;
    }
  }

  private async ownedCreateCollision(
    createError: unknown,
    name: string,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<ProviderMachine | undefined> {
    if (!isGCPCreateCollision(createError)) return undefined;
    let instance: GCPInstance;
    try {
      instance = await this.gcp<GCPInstance>("GET", `/zones/${this.zone}/instances/${name}`);
    } catch (error) {
      if (isNotFound(error)) return undefined;
      throw new ProviderResourceUnresolvedError(
        `GCP instance ${name} collision ownership could not be resolved`,
        { cause: error },
      );
    }
    const machine = toMachine(instance, this.zone);
    return machine.providerResourceID &&
      canonicalGCPMachine(machine) &&
      providerMachineOwnedByLease(
        machine,
        { id: leaseID, slug, owner, provider: "gcp", cloudID: name },
        "gcp",
        gcpProviderLabelValue,
      )
      ? machine
      : undefined;
  }

  private async insertInstanceAndWait(
    path: string,
    instance: Record<string, unknown>,
    claim: ProviderProvisioningCleanupClaim,
  ): Promise<void> {
    let operation: GCPOperation;
    try {
      operation = await this.gcp<GCPOperation>("POST", path, instance);
      if (!operation.name) {
        throw new Error("GCP instance insert returned no operation name");
      }
    } catch (error) {
      if (error instanceof GCPHTTPError && error.status < 500 && error.status !== 408) throw error;
      throw new ProviderProvisioningOutcomeUncertainError(
        `GCP instance insert outcome is uncertain: ${errorMessage(error)}`,
        { cause: error },
      );
    }
    if (operation.targetId !== undefined) {
      const providerResourceID = canonicalNumericResourceID(operation.targetId);
      if (!providerResourceID) {
        throw new ProviderProvisioningOutcomeUncertainError(
          "GCP accepted an instance insert with a malformed numeric target id",
        );
      }
      claim.providerResourceID = providerResourceID;
    }
    try {
      const completed = await this.waitZoneOperation(operation);
      if (completed.targetId !== undefined) {
        const providerResourceID = canonicalNumericResourceID(completed.targetId);
        if (
          !providerResourceID ||
          (claim.providerResourceID && claim.providerResourceID !== providerResourceID)
        ) {
          delete claim.providerResourceID;
          throw new ProviderProvisioningOutcomeUncertainError(
            "GCP accepted an instance insert with malformed or conflicting numeric target ids",
          );
        }
        claim.providerResourceID = providerResourceID;
      }
    } catch (error) {
      if (error instanceof GCPOperationError || error instanceof ProviderResourceUnresolvedError) {
        throw error;
      }
      if (error instanceof ProviderProvisioningOutcomeUncertainError) throw error;
      const uncertain = new ProviderProvisioningOutcomeUncertainError(
        `GCP accepted instance operation ${operation.name} but its outcome is uncertain: ${errorMessage(error)}`,
        { cause: error },
      );
      if (claim.providerResourceID) {
        throw new ProviderProvisioningCleanupError(uncertain.message, { ...claim }, uncertain);
      }
      throw uncertain;
    }
  }

  private async rollbackDirectCreate(
    name: string,
    leaseID: string,
    slug: string,
    owner: string,
    claim: ProviderProvisioningCleanupClaim,
    createError: unknown,
  ): Promise<void> {
    try {
      await this.deleteServerOwnedByClaim(name, leaseID, slug, owner);
    } catch (cleanupError) {
      throw new ProviderProvisioningCleanupError(
        `${errorMessage(createError)}; GCP provisioning cleanup failed closed: ${errorMessage(cleanupError)}`,
        claim,
        cleanupError,
      );
    }
  }

  private async deleteServerOwnedByClaim(
    name: string,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<void> {
    const machine = await this.findServer(name);
    if (!machine) return;
    if (
      !providerMachineOwnedByLease(
        machine,
        { id: leaseID, slug, owner, provider: "gcp", cloudID: name },
        "gcp",
        gcpProviderLabelValue,
      )
    ) {
      throw new Error(`GCP instance ${name} ownership does not match lease ${leaseID}`);
    }
    await this.deleteServer(name);
  }

  async getServer(name: string): Promise<ProviderMachine> {
    return toMachine(
      await this.gcp<GCPInstance>("GET", `/zones/${this.zone}/instances/${name}`),
      this.zone,
    );
  }

  async findServer(name: string): Promise<ProviderMachine | undefined> {
    try {
      return await this.getServer(name);
    } catch (error) {
      if (gcpInstanceNotFound(error, this.project, this.zone, name)) return undefined;
      throw error;
    }
  }

  async waitForServerIP(name: string): Promise<ProviderMachine> {
    const deadline = Date.now() + 120_000;
    for (;;) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- polling waits for eventual public IP.
      const server = await this.getServer(name);
      if (server.host) return server;
      if (Date.now() > deadline) throw new Error(`timeout waiting for gcp public ip on ${name}`);
      // oxlint-disable-next-line eslint/no-await-in-loop -- polling interval.
      await sleep(5000);
    }
  }

  async deleteServer(name: string): Promise<void> {
    const op = await this.gcp<GCPOperation>(
      "DELETE",
      `/zones/${this.zone}/instances/${name}`,
    ).catch((error) => {
      if (gcpInstanceNotFound(error, this.project, this.zone, name)) return undefined;
      throw error;
    });
    if (op) await this.waitZoneOperation(op);
  }

  async deleteSSHKey(): Promise<void> {
    // GCP stores per-instance SSH metadata; nothing global to clean up.
  }

  async createImage(
    instanceName: string,
    name: string,
    ownership?: ProviderCheckpointOwnership,
  ): Promise<ProviderImage> {
    const op = await this.gcp<GCPOperation>("POST", "/global/machineImages", {
      name,
      sourceInstance: `zones/${this.zone}/instances/${instanceName}`,
      description: `Crabbox checkpoint from ${instanceName}`,
      ...(ownership ? { labels: gcpCheckpointOwnershipLabels(ownership) } : {}),
    });
    await this.waitGlobalOperation(op);
    return await this.getImage(name);
  }

  async createDiskSnapshot(
    instanceName: string,
    name: string,
    ownership?: ProviderCheckpointOwnership,
  ): Promise<ProviderImage> {
    const instance = await this.gcp<GCPInstance>(
      "GET",
      `/zones/${this.zone}/instances/${instanceName}`,
    );
    const sourceDisk = instance.disks?.find((disk) => disk.boot)?.source;
    if (!sourceDisk) {
      throw new Error(`gcp boot disk not found for instance ${instanceName}`);
    }
    const diskName = lastPathPart(sourceDisk);
    const op = await this.gcp<GCPOperation>(
      "POST",
      `/zones/${this.zone}/disks/${diskName}/createSnapshot`,
      {
        name,
        description: `Crabbox checkpoint from ${instanceName}`,
        labels: {
          crabbox: "true",
          managed_by: "crabbox",
          ...(ownership ? gcpCheckpointOwnershipLabels(ownership) : {}),
        },
      },
    );
    await this.waitZoneOperation(op);
    return await this.getImage(name, "gcp-disk-snapshot");
  }

  async getImage(name: string, kind?: string): Promise<ProviderImage> {
    const imageName = lastPathPart(name);
    if (kind === "gcp-disk-snapshot") {
      return await this.getDiskSnapshot(name);
    }
    if (kind === "gcp-machine-image") {
      const image = await this.gcp<GCPMachineImage>("GET", `/global/machineImages/${imageName}`);
      return gcpMachineProviderImage(image, imageName, this.zone, this.project);
    }
    const image = await this.gcp<GCPMachineImage>(
      "GET",
      `/global/machineImages/${imageName}`,
    ).catch((error) => {
      if (isNotFound(error)) return undefined;
      throw error;
    });
    if (!image) return await this.getDiskSnapshot(name);
    return gcpMachineProviderImage(image, imageName, this.zone, this.project);
  }

  async deleteImage(name: string, kind?: string): Promise<void> {
    const imageName = lastPathPart(name);
    if (kind === "gcp-disk-snapshot") {
      await this.deleteDiskSnapshot(name);
      return;
    }
    const op = await this.gcp<GCPOperation>("DELETE", `/global/machineImages/${imageName}`).catch(
      (error) => {
        if (gcpMachineImageNotFound(error, this.project, imageName)) return undefined;
        throw error;
      },
    );
    if (op) {
      await this.waitGlobalOperation(op);
      return;
    }
    if (kind === "gcp-machine-image") return;
    await this.deleteDiskSnapshot(name);
  }

  private async deleteDiskSnapshot(name: string): Promise<void> {
    const snapshotOp = await this.gcp<GCPOperation>(
      "DELETE",
      `/global/snapshots/${lastPathPart(name)}`,
    ).catch((error) => {
      if (gcpSnapshotNotFound(error, this.project, lastPathPart(name))) return undefined;
      throw error;
    });
    if (snapshotOp) await this.waitGlobalOperation(snapshotOp);
  }

  private async getDiskSnapshot(name: string): Promise<ProviderImage> {
    const snapshotName = lastPathPart(name);
    const snapshot = await this.gcp<GCPSnapshot>("GET", `/global/snapshots/${snapshotName}`);
    return {
      id: snapshot.name ?? snapshotName,
      name: snapshot.name ?? snapshotName,
      state: (snapshot.status ?? "READY").toLowerCase(),
      provider: "gcp",
      kind: "gcp-disk-snapshot",
      region: this.zone,
      project: this.project,
      resourceID: snapshot.selfLink ?? gcpSnapshotRef(snapshotName, this.project),
      ...(snapshot.id ? { immutableID: String(snapshot.id) } : {}),
      ...(gcpCheckpointTokenHash(snapshot.labels)
        ? { checkpointOwnershipHash: gcpCheckpointTokenHash(snapshot.labels)! }
        : {}),
      ...(snapshot.labels?.["crabbox_checkpoint_lease"]
        ? { checkpointSourceLeaseID: snapshot.labels["crabbox_checkpoint_lease"] }
        : {}),
      snapshots: [snapshot.selfLink ?? gcpSnapshotRef(snapshotName, this.project)],
    };
  }

  hourlyPriceUSD(): Promise<number | undefined> {
    return Promise.resolve(undefined);
  }

  async ensureFirewall(config: LeaseConfig): Promise<void> {
    const sourceRanges = config.gcpSSHCIDRs.length > 0 ? config.gcpSSHCIDRs : this.sshCIDRs;
    const targetTags = gcpEffectiveTags(this.tags, config.gcpTags);
    const ports = sshPorts(config);
    const name = gcpFirewallNameForPolicy(
      config.gcpNetwork || this.network,
      sourceRanges,
      targetTags,
      ports,
    );
    const firewall = {
      name,
      description: "Crabbox-managed SSH ingress",
      network: this.networkSelfLink(config),
      direction: "INGRESS",
      sourceRanges,
      targetTags,
      allowed: [{ IPProtocol: "tcp", ports }],
    };
    const existing = await this.gcp<{ description?: string }>(
      "GET",
      `/global/firewalls/${name}`,
    ).catch((error) => {
      if (isNotFound(error)) return undefined;
      throw error;
    });
    if (existing) {
      if (!existing.description?.includes("Crabbox-managed")) {
        throw new Error(`gcp firewall ${name} exists but is not Crabbox-managed`);
      }
      const op = await this.gcp<GCPOperation>("PUT", `/global/firewalls/${name}`, firewall);
      await this.waitGlobalOperation(op);
      return;
    }
    try {
      const op = await this.gcp<GCPOperation>("POST", "/global/firewalls", firewall);
      await this.waitGlobalOperation(op);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (!message.toLowerCase().includes("http 409")) {
        throw error;
      }
      await this.reconcileRacedFirewall(name, firewall, error);
    }
  }

  private async reconcileRacedFirewall(
    name: string,
    firewall: Record<string, unknown>,
    conflictError: unknown,
  ): Promise<void> {
    for (const delay of [0, ...firewallVisibilityBackoffMs]) {
      if (delay > 0) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- firewall insertion is eventually consistent.
        await sleep(delay);
      }
      let raced: { description?: string } | undefined;
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- each lookup follows bounded propagation backoff.
        raced = await this.gcp<{ description?: string }>("GET", `/global/firewalls/${name}`);
      } catch (error) {
        if (isNotFound(error)) {
          continue;
        }
        throw error;
      }
      if (!raced.description?.includes("Crabbox-managed")) {
        throw new Error(`gcp firewall ${name} exists but is not Crabbox-managed`, {
          cause: conflictError,
        });
      }
      try {
        // A completed update proves the raced insert is visible and the desired policy is effective.
        // oxlint-disable-next-line eslint/no-await-in-loop -- a conflicting insert may still be finishing.
        const op = await this.gcp<GCPOperation>("PUT", `/global/firewalls/${name}`, firewall);
        // oxlint-disable-next-line eslint/no-await-in-loop -- the raced policy must finish before this caller proceeds.
        await this.waitGlobalOperation(op);
        return;
      } catch (error) {
        const message = error instanceof Error ? error.message.toLowerCase() : String(error);
        if (!message.includes("http 409") && !message.includes("http 404")) {
          throw error;
        }
      }
    }
    throw conflictError;
  }

  private async gcp<T>(method: string, path: string, body?: unknown): Promise<T> {
    const token = await this.accessToken();
    const init: RequestInit = {
      method,
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json",
      },
    };
    if (body !== undefined) init.body = JSON.stringify(body);
    const response = await this.fetcher(`${computeBaseURL}/projects/${this.project}${path}`, init);
    const text = await response.text();
    if (!response.ok) {
      throw new GCPHTTPError(method, path, response.status, text);
    }
    return (text ? JSON.parse(text) : {}) as T;
  }

  private async accessToken(): Promise<string> {
    const now = Math.trunc(Date.now() / 1000);
    const credentialSource = gcpCredentialSource(this.env);
    const refreshSkewSeconds =
      credentialSource === "metadata"
        ? metadataTokenRefreshSkewSeconds
        : serviceAccountTokenRefreshSkewSeconds;
    return this.tokenCache.get(now + refreshSkewSeconds, () =>
      credentialSource === "metadata"
        ? this.metadataAccessToken()
        : this.serviceAccountAccessToken(now),
    );
  }

  private async serviceAccountAccessToken(now: number): Promise<ExpiringToken> {
    const assertion = await serviceAccountAssertion(this.env, now);
    const response = await this.fetcher(tokenURL, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({
        grant_type: "urn:ietf:params:oauth:grant-type:jwt-bearer",
        assertion,
      }),
    });
    const data = (await response.json()) as {
      access_token?: string;
      expires_in?: number;
      error?: string;
    };
    if (!response.ok || !data.access_token) {
      throw new Error(`gcp token: ${data.error ?? response.statusText}`);
    }
    return { token: data.access_token, expiresAt: now + (data.expires_in ?? 3600) };
  }

  private async metadataAccessToken(): Promise<ExpiringToken> {
    const deadline = Date.now() + metadataTokenDeadlineMs;
    let lastFailure: Error | undefined;
    for (let attempt = 0; ; attempt += 1) {
      if (deadline - Date.now() <= 0) {
        throw lastFailure ?? new Error("gcp metadata token: deadline exceeded");
      }
      let response: Response;
      let text: string;
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- metadata retries are bounded and sequential.
        ({ response, text } = await this.metadataTokenResponse(deadline));
      } catch (error) {
        if (!(error instanceof GCPMetadataTokenRequestError)) throw error;
        lastFailure = error;
        // oxlint-disable-next-line eslint/no-await-in-loop -- metadata retries are bounded and sequential.
        if (await sleepBeforeMetadataRetry(attempt, deadline)) {
          // GKE metadata can refuse connections while the server starts; retry within the same bound.
          continue;
        }
        throw lastFailure;
      }
      // Error responses from the metadata server are not guaranteed to be JSON.
      const data = parseMetadataTokenResponse(text);
      const token = typeof data?.access_token === "string" ? data.access_token.trim() : "";
      if (response.ok && token) {
        const expiresIn =
          typeof data?.expires_in === "number" &&
          Number.isFinite(data.expires_in) &&
          data.expires_in > 0
            ? data.expires_in
            : 3600;
        return {
          token,
          expiresAt: Math.trunc(Date.now() / 1000) + expiresIn,
        };
      }
      const detail =
        typeof data?.error === "string" && data.error.trim()
          ? data.error.trim()
          : response.statusText.trim();
      lastFailure = !response.ok
        ? new Error(`gcp metadata token: http ${response.status}${detail ? `: ${detail}` : ""}`)
        : new Error("gcp metadata token: response missing access_token");
      if (metadataTokenRetryStatus(response.status)) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- metadata retries are bounded and sequential.
        if (await sleepBeforeMetadataRetry(attempt, deadline)) {
          continue;
        }
        throw lastFailure;
      }
      if (!response.ok) {
        throw lastFailure;
      }
      throw lastFailure;
    }
  }

  private async metadataTokenResponse(
    deadline: number,
  ): Promise<{ response: Response; text: string }> {
    const remaining = deadline - Date.now();
    if (remaining <= 0) {
      throw new GCPMetadataTokenRequestError("gcp metadata token: deadline exceeded");
    }
    const controller = new AbortController();
    const timeout = setTimeout(
      () => controller.abort(),
      Math.max(1, Math.min(metadataTokenRequestTimeoutMs, remaining)),
    );
    try {
      const response = await this.fetcher(metadataTokenURL, {
        headers: { "Metadata-Flavor": "Google" },
        redirect: "manual",
        signal: controller.signal,
      });
      // workerd supports manual redirects, not Fetch's error mode. Never follow metadata elsewhere.
      if (response.status >= 300 && response.status < 400) {
        throw new GCPMetadataTokenTrustError(
          `gcp metadata token: redirect rejected (http ${response.status})`,
        );
      }
      if (response.headers.get("Metadata-Flavor") !== "Google") {
        throw new GCPMetadataTokenTrustError(
          "gcp metadata token: response missing Metadata-Flavor: Google",
        );
      }
      const text = await response.text();
      return { response, text };
    } catch (error) {
      if (error instanceof GCPMetadataTokenTrustError) throw error;
      const detail = controller.signal.aborted
        ? "request timed out"
        : error instanceof Error
          ? error.message.trim()
          : String(error ?? "").trim();
      throw new GCPMetadataTokenRequestError(
        `gcp metadata token: request failed${detail ? `: ${detail}` : ""}`,
        { cause: error },
      );
    } finally {
      clearTimeout(timeout);
    }
  }

  private async waitZoneOperation(op: GCPOperation): Promise<GCPOperation> {
    if (!op.name) return op;
    for (;;) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- operation polling is sequential.
      const done = await this.gcp<GCPOperation>(
        "POST",
        `/zones/${this.zone}/operations/${op.name}/wait`,
      );
      operationError(done);
      if (operationDone(done)) return done;
      // oxlint-disable-next-line eslint/no-await-in-loop -- polling interval.
      await sleep(2000);
    }
  }

  private async waitGlobalOperation(op: GCPOperation): Promise<void> {
    if (!op.name) return;
    for (;;) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- operation polling is sequential.
      const done = await this.gcp<GCPOperation>("POST", `/global/operations/${op.name}/wait`);
      operationError(done);
      if (operationDone(done)) return;
      // oxlint-disable-next-line eslint/no-await-in-loop -- polling interval.
      await sleep(2000);
    }
  }

  private networkSelfLink(config: LeaseConfig): string {
    const network = config.gcpNetwork || this.network;
    return network.includes("/") ? network : `projects/${this.project}/global/networks/${network}`;
  }

  private subnetSelfLink(config: LeaseConfig): string {
    const subnet = config.gcpSubnet || this.subnet;
    if (!subnet) return "";
    return subnet.includes("/")
      ? subnet
      : `projects/${this.project}/regions/${regionFromZone(this.zone)}/subnetworks/${subnet}`;
  }
}

export function gcpProvisioningCandidatesForConfig(
  config: Pick<
    LeaseConfig,
    "serverType" | "serverTypeExplicit" | "class" | "target" | "architecture"
  >,
): string[] {
  if (config.serverTypeExplicit && config.serverType) {
    return [config.serverType];
  }
  let profileCandidates =
    config.target === "linux" && config.architecture === "amd64"
      ? gcpMachineTypeCandidatesForClass(config.class)
      : [];
  if (profileCandidates.length === 0 && isCanonicalProviderClass(config.class)) {
    const storedType = concreteStoredServerType(config.serverType, config.class);
    return storedType ? [storedType] : [];
  }
  if (profileCandidates.length === 0) {
    profileCandidates = [config.class];
  }
  const storedType = concreteStoredServerType(config.serverType, config.class);
  return storedType
    ? uniqueProviderMachineCandidates([storedType, ...profileCandidates])
    : profileCandidates;
}

async function serviceAccountAssertion(env: Env, now: number): Promise<string> {
  const email = env.GCP_CLIENT_EMAIL?.trim() ?? "";
  const privateKey = (env.GCP_PRIVATE_KEY ?? "").replaceAll("\\n", "\n");
  const header = base64url(JSON.stringify({ alg: "RS256", typ: "JWT" }));
  const payload = base64url(
    JSON.stringify({
      iss: email,
      scope: "https://www.googleapis.com/auth/cloud-platform",
      aud: tokenURL,
      exp: now + 3600,
      iat: now,
    }),
  );
  const unsigned = `${header}.${payload}`;
  const key = await crypto.subtle.importKey(
    "pkcs8",
    pemToArrayBuffer(privateKey),
    { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const signature = await crypto.subtle.sign("RSASSA-PKCS1-v1_5", key, utf8(unsigned));
  return `${unsigned}.${base64url(signature)}`;
}

function pemToArrayBuffer(pem: string): ArrayBuffer {
  const base64 = pem.replaceAll(/-----BEGIN PRIVATE KEY-----|-----END PRIVATE KEY-----|\s/g, "");
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index);
  }
  return bytes.buffer;
}

function hasServiceAccountCredential(env: Env): boolean {
  return Boolean(env.GCP_CLIENT_EMAIL?.trim()) && Boolean(env.GCP_PRIVATE_KEY?.trim());
}

function hasPartialServiceAccountCredential(env: Env): boolean {
  return Boolean(env.GCP_CLIENT_EMAIL?.trim()) !== Boolean(env.GCP_PRIVATE_KEY?.trim());
}

function gcpCredentialSource(env: Env): "metadata" | "service-account-key" {
  const source = env.CRABBOX_GCP_CREDENTIAL_SOURCE?.trim() ?? "";
  if (!source) return "service-account-key";
  if (source === "metadata" || source === "service-account-key") return source;
  throw new Error("CRABBOX_GCP_CREDENTIAL_SOURCE must be metadata or service-account-key");
}

function parseMetadataTokenResponse(
  text: string,
): { access_token?: unknown; expires_in?: unknown; error?: unknown } | undefined {
  if (!text) return undefined;
  try {
    const value = JSON.parse(text) as unknown;
    return value && typeof value === "object"
      ? (value as { access_token?: unknown; expires_in?: unknown; error?: unknown })
      : undefined;
  } catch {
    return undefined;
  }
}

function metadataTokenRetryStatus(status: number): boolean {
  return status === 429 || status === 499 || (status >= 500 && status <= 599);
}

async function sleepBeforeMetadataRetry(attempt: number, deadline: number): Promise<boolean> {
  const delay = metadataTokenBackoffMs[attempt];
  const remaining = deadline - Date.now();
  if (delay === undefined || remaining <= 1) return false;
  await sleep(Math.min(delay, remaining - 1));
  return Date.now() < deadline;
}

function toMachine(instance: GCPInstance, zone: string): ProviderMachine {
  const host =
    instance.networkInterfaces
      ?.flatMap((iface) => iface.accessConfigs ?? [])
      .find((cfg) => cfg.natIP)?.natIP ?? "";
  return {
    provider: "gcp",
    id: Number(instance.id ?? 0),
    ...(instance.id ? { providerResourceID: instance.id } : {}),
    cloudID: instance.name ?? "",
    region: zone,
    name: instance.name ?? "",
    status: instance.status ?? "",
    serverType: lastPathPart(instance.machineType ?? ""),
    host,
    labels: { ...instance.labels, zone },
  };
}

function canonicalGCPMachine(machine: ProviderMachine): boolean {
  const leaseID = machine.labels["lease"] ?? "";
  const slug = machine.labels["slug"] ?? "";
  return (
    /^cbx_[a-f0-9]{12}$/.test(leaseID) &&
    slug.length > 0 &&
    machine.name === leaseProviderName(leaseID, slug) &&
    machine.labels["crabbox"] === "true" &&
    machine.labels["created_by"] === "crabbox" &&
    machine.labels["provider"] === "gcp"
  );
}

function gcpLabels(labels: Record<string, string>): Record<string, string> {
  return Object.fromEntries(
    Object.entries(labels).map(([key, value]) => [gcpLabelKey(key), gcpLabelValue(value)]),
  );
}

function gcpLabelKey(value: string): string {
  const out = gcpLabelValue(value);
  return /^[a-z]/.test(out) ? out : `x${out}`.slice(0, 63);
}

export function gcpLabelValue(value: string): string {
  let out = value
    .trim()
    .toLowerCase()
    .replaceAll(/[^a-z0-9_-]/g, "_")
    .slice(0, 63)
    .replaceAll(/^[_-]+|[_-]+$/g, "");
  if (!out) out = "unknown";
  return out;
}

export function gcpProviderLabelValue(value: string): string {
  return gcpLabelValue(providerLabelValue(value));
}

export function isFallbackProvisioningError(message: string): boolean {
  const value = message.toLowerCase();
  return (
    value.includes("quota") ||
    value.includes("capacity") ||
    value.includes("resource_pool_exhausted") ||
    value.includes("does not have enough resources") ||
    isUnavailableMachineTypeError(value) ||
    value.includes("rate limit") ||
    value.includes("try again") ||
    value.includes("http 409") ||
    value.includes("http 429") ||
    value.includes("http 5")
  );
}

function isUnavailableMachineTypeError(value: string): boolean {
  return (
    value.includes("/machinetypes/") ||
    value.includes("resource.machinetype") ||
    (value.includes("machine type") &&
      (value.includes("does not exist") ||
        value.includes("not found") ||
        value.includes("invalid value")))
  );
}

function isGCPCreateCollision(error: unknown): boolean {
  return (
    (error instanceof GCPHTTPError && error.status === 409) ||
    (error instanceof GCPOperationError &&
      /(?:already[_ ]exists|resource[_ ]already[_ ]exists)/i.test(error.message))
  );
}

function operationError(op: GCPOperation): void {
  const errors = op.error?.errors ?? [];
  if (errors.length > 0) {
    throw new GCPOperationError(
      errors.map((item) => `${item.code ?? "error"}: ${item.message ?? ""}`).join("; "),
    );
  }
}

export function operationDone(op: GCPOperation): boolean {
  return !op.name || op.status === "DONE";
}

function isNotFound(error: unknown): boolean {
  return errorMessage(error).includes("http 404");
}

function gcpInstanceNotFound(error: unknown, project: string, zone: string, name: string): boolean {
  if (!(error instanceof GCPHTTPError) || error.status !== 404) return false;
  const resource = `projects/${project}/zones/${zone}/instances/${name}`.toLowerCase();
  return error.body.toLowerCase().includes(resource);
}

export function gcpSnapshotNotFound(error: unknown, project: string, name: string): boolean {
  return gcpGlobalResourceNotFound(error, project, "snapshots", name);
}

export function gcpMachineImageNotFound(error: unknown, project: string, name: string): boolean {
  return gcpGlobalResourceNotFound(error, project, "machineImages", name);
}

function gcpGlobalResourceNotFound(
  error: unknown,
  project: string,
  collection: "snapshots" | "machineImages",
  name: string,
): boolean {
  if (!(error instanceof GCPHTTPError) || error.status !== 404) return false;
  const expected = `projects/${project}/global/${collection}/${name}`.toLowerCase();
  return error.body.toLowerCase().includes(expected);
}

function gcpCheckpointOwnershipLabels(
  ownership: ProviderCheckpointOwnership,
): Record<string, string> {
  return {
    crabbox_checkpoint_id:
      ownership.checkpointID.length <= 63
        ? ownership.checkpointID.toLowerCase()
        : ownership.tokenHash.slice(0, 63),
    crabbox_checkpoint_token_a: ownership.tokenHash.slice(0, 32),
    crabbox_checkpoint_token_b: ownership.tokenHash.slice(32),
    crabbox_checkpoint_lease: ownership.sourceLeaseID.toLowerCase(),
  };
}

function gcpCheckpointTokenHash(labels: Record<string, string> | undefined): string | undefined {
  const first = labels?.["crabbox_checkpoint_token_a"];
  const second = labels?.["crabbox_checkpoint_token_b"];
  return first && second ? first + second : undefined;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function pendingMachine(
  name: string,
  serverType: string,
  zone: string,
  labels: Record<string, string>,
): ProviderMachine {
  return {
    provider: "gcp",
    id: 0,
    cloudID: name,
    name,
    status: "provisioning",
    serverType,
    host: "",
    region: zone,
    labels,
  };
}

function uniqueStrings(values: string[]): string[] {
  return [...new Set(values.map((value) => value.trim()).filter(Boolean))];
}

function prependUnique(first: string, rest: string[]): string[] {
  return uniqueStrings([first, ...rest]);
}

function lastPathPart(value: string): string {
  return value.slice(value.lastIndexOf("/") + 1);
}

function gcpMachineProviderImage(
  image: GCPMachineImage,
  fallbackName: string,
  zone: string,
  project: string,
): ProviderImage {
  return {
    id: image.name ?? fallbackName,
    name: image.name ?? fallbackName,
    state: (image.status ?? "READY").toLowerCase(),
    provider: "gcp",
    kind: "gcp-machine-image",
    region: zone,
    project,
    resourceID: image.selfLink ?? gcpMachineImageRef(fallbackName, project),
    ...(image.id ? { immutableID: String(image.id) } : {}),
    ...(gcpCheckpointTokenHash(image.labels)
      ? { checkpointOwnershipHash: gcpCheckpointTokenHash(image.labels)! }
      : {}),
    ...(image.labels?.["crabbox_checkpoint_lease"]
      ? { checkpointSourceLeaseID: image.labels["crabbox_checkpoint_lease"] }
      : {}),
  };
}

export function gcpFirewallNameForNetwork(network: string): string {
  const name = lastPathPart(network.trim());
  if (!name || name === "default") return firewallName;
  let suffix = name
    .toLowerCase()
    .replaceAll(/[^a-z0-9-]/g, "-")
    .replaceAll(/^-+|-+$/g, "")
    .replaceAll(/-+/g, "-");
  if (!/^[a-z]/.test(suffix)) suffix = `net-${suffix}`;
  suffix = suffix.slice(0, 63 - `${firewallName}-`.length).replaceAll(/-+$/g, "");
  return `${firewallName}-${suffix || "custom"}`;
}

export function gcpFirewallNameForPolicy(
  network: string,
  sourceRanges: string[],
  targetTags: string[],
  ports: string[],
): string {
  const base = gcpFirewallNameForNetwork(network);
  if (
    canonicalPolicyPart(sourceRanges) === "0.0.0.0/0" &&
    canonicalPolicyPart(targetTags) === "crabbox-ssh" &&
    canonicalPolicyPart(ports) === "22,2222"
  ) {
    return base;
  }
  return gcpFirewallNameWithSuffix(
    base,
    fnv32Hex(
      [sourceRanges, targetTags, ports].map((values) => canonicalPolicyPart(values)).join("|"),
    ),
  );
}

export function gcpEffectiveTags(defaultTags: string[], requestTags: string[]): string[] {
  const tags = uniqueStrings(requestTags.length > 0 ? requestTags : defaultTags);
  return tags.length > 0 ? tags : [firewallName];
}

function gcpFirewallNameWithSuffix(base: string, suffix: string): string {
  const maxBaseLength = 63 - suffix.length - 1;
  const trimmed = base.slice(0, maxBaseLength).replaceAll(/-+$/g, "");
  return `${trimmed || firewallName}-${suffix}`;
}

function canonicalPolicyPart(values: string[]): string {
  return values.toSorted().join(",");
}

function fnv32Hex(value: string): string {
  let hash = 0x811c9dc5;
  for (let index = 0; index < value.length; index += 1) {
    hash ^= value.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash.toString(16).padStart(8, "0");
}

function regionFromZone(zone: string): string {
  return zone.slice(0, zone.lastIndexOf("-")) || zone;
}

function gcpMachineImageRef(value: string, project: string): string {
  if (value.includes("/")) {
    return value;
  }
  return `projects/${project}/global/machineImages/${value}`;
}

function gcpSnapshotRef(value: string, project: string): string {
  if (value.includes("/")) {
    return value;
  }
  return `projects/${project}/global/snapshots/${value}`;
}

function canonicalGCPReadyPoolSource(
  source: string | undefined,
  kind: string | undefined,
): string | undefined {
  const match = gcpReadyPoolSourceMatch(source, kind);
  return match ? `projects/${match[1]}/global/${match[2]}/${match[3]}` : undefined;
}

function gcpReadyPoolSourceMatch(
  source: string | undefined,
  kind: string | undefined,
): RegExpExecArray | undefined {
  if (
    !source ||
    source !== source.trim() ||
    (kind !== "gcp-image" && kind !== "gcp-disk-snapshot")
  ) {
    return undefined;
  }
  const match = gcpReadyPoolResourcePattern.exec(source);
  if (
    !match?.[1] ||
    !match[2] ||
    !match[3] ||
    (kind === "gcp-image" ? match[2] !== "images" : match[2] !== "snapshots")
  ) {
    return undefined;
  }
  return match;
}

function canonicalGCPObservedResource(
  source: string | undefined,
  pattern: RegExp,
  project: string,
  zone: string,
  collection: "disks" | "instances",
): string | undefined {
  if (!source) return undefined;
  const match = pattern.exec(source.trim());
  return match?.[1] === project && match[2] === zone
    ? `projects/${project}/zones/${zone}/${collection}/${match[3]}`
    : undefined;
}

function canonicalNumericResourceID(value: unknown): string | undefined {
  return typeof value === "string" && value === value.trim() && /^[0-9]+$/.test(value)
    ? value
    : undefined;
}

function canonicalGCPProject(value: string): boolean {
  return /^[a-z][a-z0-9-]{4,28}[a-z0-9]$/.test(value);
}

function canonicalGCPZone(value: string): boolean {
  return /^[a-z][a-z0-9-]{1,61}[a-z0-9]$/.test(value);
}

function observedOwnedGCPBootDiskPath(
  instance: GCPInstance,
  lease: LeaseRecord,
  project: string,
  zone: string,
  providerResourceID: string,
): string | undefined {
  const machine = toMachine(instance, zone);
  if (
    instance.id !== providerResourceID ||
    instance.name !== lease.cloudID ||
    !providerMachineOwnedByLease(machine, lease, "gcp", gcpProviderLabelValue) ||
    instance.sourceMachineImage?.trim()
  ) {
    return undefined;
  }
  const bootDisks = (instance.disks ?? []).filter((disk) => disk.boot === true);
  if (bootDisks.length !== 1 || bootDisks[0]?.type !== "PERSISTENT") return undefined;
  return canonicalGCPObservedResource(
    bootDisks[0].source,
    gcpObservedDiskPattern,
    project,
    zone,
    "disks",
  );
}

function observedGCPBootDisk(
  disk: GCPDisk,
  expectedDiskPath: string,
  expectedInstancePath: string,
  zone: string,
): { resourceID: string; image: LeaseImageIdentity } | undefined {
  const diskMatch = gcpObservedDiskPattern.exec(expectedDiskPath);
  const instanceMatch = gcpObservedInstancePattern.exec(expectedInstancePath);
  if (!diskMatch?.[1] || !diskMatch[2] || !instanceMatch?.[1] || !instanceMatch[2]) {
    return undefined;
  }
  const diskPath = canonicalGCPObservedResource(
    disk.selfLink,
    gcpObservedDiskPattern,
    diskMatch[1],
    diskMatch[2],
    "disks",
  );
  const users = (disk.users ?? []).map((user) =>
    canonicalGCPObservedResource(
      user,
      gcpObservedInstancePattern,
      instanceMatch[1]!,
      instanceMatch[2]!,
      "instances",
    ),
  );
  const resourceID = canonicalNumericResourceID(disk.id);
  if (
    diskPath !== expectedDiskPath ||
    !resourceID ||
    users.length !== 1 ||
    users[0] !== expectedInstancePath
  ) {
    return undefined;
  }

  const imagePresent = Boolean(disk.sourceImage?.trim() || disk.sourceImageId?.trim());
  const snapshotPresent = Boolean(disk.sourceSnapshot?.trim() || disk.sourceSnapshotId?.trim());
  if (imagePresent === snapshotPresent) return undefined;
  const kind = imagePresent ? "gcp-image" : "gcp-disk-snapshot";
  const source = imagePresent ? disk.sourceImage : disk.sourceSnapshot;
  const id = canonicalNumericResourceID(imagePresent ? disk.sourceImageId : disk.sourceSnapshotId);
  const canonicalSource = canonicalGCPReadyPoolSource(source, kind);
  if (!canonicalSource || !id) return undefined;
  return {
    resourceID,
    image: {
      id,
      source: imagePresent ? "explicit" : "snapshot",
      provider: "gcp",
      kind,
      region: zone,
      sourceID: canonicalSource,
    },
  };
}

function sameGCPObservedImageIdentity(
  left: LeaseImageIdentity,
  right: LeaseImageIdentity,
): boolean {
  return (
    left.id === right.id &&
    left.source === right.source &&
    left.provider === right.provider &&
    left.kind === right.kind &&
    left.region === right.region &&
    left.sourceID === right.sourceID
  );
}

function numberFromEnv(value: string | undefined, fallback: number): number {
  const parsed = Number(value);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

function utf8(value: string): Uint8Array {
  return new TextEncoder().encode(value);
}

function base64url(value: string | ArrayBuffer): string {
  const bytes = typeof value === "string" ? utf8(value) : new Uint8Array(value);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
