import { cloudInit } from "./bootstrap";
import { ovhDefaultOS, type LeaseConfig } from "./config";
import { stableMachineID } from "./daytona";
import { bytesToHex } from "./encoding";
import { eurToUSD } from "./hetzner";
import { redactDiagnosticSecrets } from "./http";
import { providerRequestSignal, providerRequestTimeoutMs } from "./provider-deadline";
import { providerKeyForLease, sshPublicKeyIdentity } from "./provider-key";
import { withPricingDeadline } from "./provider-pricing";
import {
  ProviderProvisioningCleanupError,
  type ProviderProvisioningCleanupClaim,
  ProviderProvisioningOutcomeUncertainError,
  ProviderResourceUnresolvedError,
  validateProviderProvisioningCleanupClaim,
} from "./provider-provisioning";
import { leaseProviderName } from "./slug";
import type { Env, LeaseRecord, ProviderMachine } from "./types";

const defaultEndpoint = "https://api.us.ovhcloud.com/1.0";
const defaultImage = "Ubuntu 24.04";
const catalogPriceDivisor = 100_000_000;
const createRecoveryReads = 3;
const maxResponseBytes = 8 * 1024 * 1024;
// Quotes run during admission, so an unexpectedly large catalog must not exhaust coordinator memory.
const maxCatalogBytes = 16 * 1024 * 1024;

interface OVHFlavor {
  id: string;
  name: string;
  osType?: string;
  available?: boolean;
  quota?: number;
  planCodes?: { hourly?: string | null };
}

interface OVHImage {
  id: string;
  name: string;
  type?: string;
  status?: string;
  visibility?: string;
}

interface OVHSSHKey {
  id: string;
  name: string;
  publicKey?: string;
}

interface OVHIPAddress {
  ip?: string;
  version?: number;
  type?: string;
}

export interface OVHInstance {
  id: string;
  name: string;
  status?: string;
  region?: string;
  flavor?: { name?: string };
  ipAddresses?: OVHIPAddress[];
}

// OVH instances carry no labels. Ownership is the exact id plus a random per-allocation name
// recorded before the create, so a same-named instance from elsewhere is never adopted.
export type OVHLeaseIdentity = Pick<LeaseRecord, "id" | "cloudID" | "region" | "providerScope"> &
  Partial<Pick<LeaseRecord, "providerKeyCleanupID" | "providerResourceID">>;

export class OVHHTTPError extends Error {
  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    body: string,
  ) {
    super(`ovh ${method} ${path}: http ${status}: ${body}`);
    this.name = "OVHHTTPError";
  }
}

// Proves no instance exists for this attempt. A retained key id is the only remaining resource.
export class OVHAllocationRejectedError extends Error {
  constructor(
    message: string,
    readonly providerKeyCleanupID?: string,
    options?: ErrorOptions,
  ) {
    super(message, options);
    this.name = "OVHAllocationRejectedError";
  }
}

export function ovhProvisioningFailureEvidence(error: unknown): {
  allocationRejected?: boolean;
  providerKeyCleanupID?: string;
} {
  if (!(error instanceof OVHAllocationRejectedError)) return {};
  return {
    allocationRejected: true,
    ...(error.providerKeyCleanupID ? { providerKeyCleanupID: error.providerKeyCleanupID } : {}),
  };
}

export class OVHClient {
  readonly projectID: string;
  readonly defaultRegion: string;
  fetcher: typeof fetch = (input, init) => fetch(input, init);
  pollDelayMs = 3_000;
  maxWaitMs = 5 * 60_000;
  deleteWaitMs = 60_000;

  private readonly endpoint: string;
  private readonly applicationKey: string;
  private readonly applicationSecret: string;
  private readonly consumerKey: string;
  private readonly image: string;
  private readonly flavor: string;
  private readonly imageConfigured: boolean;
  private readonly eurToUSD: number;
  private serverTimeDelta?: Promise<number> | undefined;

  constructor(env: Env) {
    this.applicationKey = requiredEnv(env.OVH_APPLICATION_KEY, "OVH_APPLICATION_KEY");
    this.applicationSecret = requiredEnv(env.OVH_APPLICATION_SECRET, "OVH_APPLICATION_SECRET");
    this.consumerKey = requiredEnv(env.OVH_CONSUMER_KEY, "OVH_CONSUMER_KEY");
    this.projectID = requiredEnv(env.CRABBOX_OVH_PROJECT_ID, "CRABBOX_OVH_PROJECT_ID");
    this.endpoint = ovhEndpoint(env.OVH_ENDPOINT);
    this.defaultRegion = env.CRABBOX_OVH_REGION?.trim() ?? "";
    this.imageConfigured = Boolean(env.CRABBOX_OVH_IMAGE?.trim());
    this.image = env.CRABBOX_OVH_IMAGE?.trim() || defaultImage;
    this.flavor = env.CRABBOX_OVH_FLAVOR?.trim() ?? "";
    this.eurToUSD = eurToUSD(env);
  }

  get providerScope(): string {
    return `ovh:project:${this.projectID}`;
  }

  prepareLeaseConfig(config: LeaseConfig): LeaseConfig {
    const ovhRegion = config.ovhRegion || this.defaultRegion;
    if (!ovhRegion) throw new Error("CRABBOX_OVH_REGION is required for OVH leases");
    return {
      ...config,
      ovhRegion,
      serverType: !config.serverTypeExplicit && this.flavor ? this.flavor : config.serverType,
    };
  }

  prepareLeaseCreate(
    config: LeaseConfig,
    lease: LeaseRecord,
  ): { config: LeaseConfig; lease: LeaseRecord } {
    const token = bytesToHex(crypto.getRandomValues(new Uint8Array(6)));
    const ovhInstanceName = `${leaseProviderName(lease.id, lease.slug)}-${token}`;
    return {
      config: { ...config, ovhInstanceName },
      lease: {
        ...lease,
        providerScope: this.providerScope,
        providerResourceID: ovhInstanceName,
        ...(config.ovhRegion ? { region: config.ovhRegion } : {}),
      },
    };
  }

  unsupportedOS(config: LeaseConfig): string | undefined {
    if (config.os === ovhDefaultOS || this.imageConfigured) return undefined;
    return `provider=ovh does not support os ${config.os}; use ${ovhDefaultOS} or configure CRABBOX_OVH_IMAGE`;
  }

  async listRegions(): Promise<string[]> {
    const regions = requireList<string | { name?: string }>(
      await this.request("GET", this.cloudPath("region")),
      "region list",
    );
    return regions.map((region) => (typeof region === "string" ? region : (region.name ?? "")));
  }

  async createSSHKey(name: string, publicKey: string): Promise<OVHSSHKey> {
    const key = await this.request<OVHSSHKey | null>("POST", this.cloudPath("sshkey"), {
      name,
      publicKey,
    });
    if (typeof key?.id !== "string" || !key.id) throw new Error("OVH key create returned no id");
    return key;
  }

  async listCrabboxServers(): Promise<ProviderMachine[]> {
    const instances = await this.listInstances();
    return instances
      .filter((instance) => instance.name.startsWith("crabbox-"))
      .map((instance) => this.toMachine(instance));
  }

  async getServer(id: string): Promise<ProviderMachine> {
    return this.toMachine(await this.getInstance(id));
  }

  async readinessChecks(): Promise<
    Array<{ status: string; check: string; message: string; details: Record<string, string> }>
  > {
    const details = {
      api: "regions",
      mutation: "false",
      client_auth: "crabbox",
      control_plane: "coordinator",
      data_plane: "ssh-rsync",
    };
    try {
      const regions = await this.listRegions();
      return [
        {
          status: "ok",
          check: "ovh-coordinator",
          message: `auth=ready control_plane=ready regions=${regions.length} mutation=false`,
          details: { ...details, regions: String(regions.length) },
        },
      ];
    } catch (error) {
      return [
        {
          status: "failed",
          check: "ovh-coordinator",
          message: `auth_or_control_plane=failed mutation=false ${this.redact(errorText(error))}`,
          details,
        },
      ];
    }
  }

  async hourlyPriceUSD(serverType: string, region: string): Promise<number | undefined> {
    return await withPricingDeadline(async (signal) => {
      const [planCode, catalog] = await Promise.all([
        this.hourlyPlanCode(serverType, region, signal),
        this.publicCatalog(signal),
      ]);
      return ovhCatalogHourlyUSD(catalog, planCode, this.eurToUSD);
    });
  }

  async createServer(
    config: LeaseConfig,
    leaseID: string,
    onResourceCreated?: (claim: ProviderProvisioningCleanupClaim) => Promise<boolean>,
  ): Promise<ProviderMachine> {
    const region = config.ovhRegion ?? "";
    const name = config.ovhInstanceName ?? "";
    if (!name || !region) {
      throw new OVHAllocationRejectedError(`OVH lease ${leaseID} has no prepared allocation`);
    }
    const { flavor, image, key } = await this.prepareAllocation(config, leaseID, region);
    const instance = await this.allocateInstance(name, region, flavor, image, key, config);
    const server = { ...this.toMachine(instance, region), serverType: flavor.name };
    const claim = validateProviderProvisioningCleanupClaim(
      {
        provider: "ovh",
        cloudID: instance.id,
        region,
        providerScope: this.providerScope,
        providerResourceID: name,
      },
      "ovh",
    );
    if (!claim) {
      await this.removeKeyQuietly(key.id);
      throw new ProviderResourceUnresolvedError(
        `OVH create for lease ${leaseID} returned no valid instance id`,
      );
    }
    try {
      const continueReadiness = await onResourceCreated?.(claim);
      // The key only seeds the create call; cloud-init authorizes the lease's own public key.
      await this.removeKeyQuietly(key.id);
      if (continueReadiness === false) return server;
      if (server.host) return server;
      return { ...(await this.waitForPublicIPv4(instance.id, region)), serverType: flavor.name };
    } catch (error) {
      if (error instanceof ProviderResourceUnresolvedError) throw error;
      // The coordinator owns rollback, so a recorded id cannot be orphaned by a restart.
      throw new ProviderProvisioningCleanupError(
        `${errorText(error)}; OVH instance ${instance.id} cleanup remains pending`,
        claim,
        error,
      );
    }
  }

  async recoverServer(lease: OVHLeaseIdentity): Promise<ProviderMachine | undefined> {
    this.assertProject(lease);
    if (lease.cloudID) {
      const instance = await this.findInstance(lease.cloudID);
      if (!instance) {
        await this.assertProjectVisible();
        return undefined;
      }
      if (!ovhInstanceOwnedByLease(instance, lease)) {
        throw new ProviderResourceUnresolvedError(
          `OVH instance ${lease.cloudID} no longer matches lease ${lease.id}`,
        );
      }
      return this.toMachine(instance, lease.region);
    }
    // Without a recorded name, the create was never dispatched.
    if (!lease.providerResourceID) return undefined;
    const instance = await this.findInstanceByName(lease.providerResourceID, lease.region ?? "");
    return instance ? this.toMachine(instance, lease.region) : undefined;
  }

  async releaseLease(lease: OVHLeaseIdentity, assertOwner: () => Promise<void>): Promise<void> {
    this.assertProject(lease);
    if (lease.cloudID) await this.deleteOwnedInstance(lease, assertOwner);
    if (lease.providerKeyCleanupID) {
      await this.deleteRetainedKey(lease, lease.providerKeyCleanupID, assertOwner);
      return;
    }
    await this.sweepLeaseKey(lease.id, assertOwner);
  }

  toMachine(instance: OVHInstance, fallbackRegion?: string): ProviderMachine {
    const region = instance.region || fallbackRegion;
    return {
      provider: "ovh",
      id: stableMachineID(instance.id),
      cloudID: instance.id,
      name: instance.name,
      providerResourceID: instance.name,
      status: instance.status ?? "unknown",
      serverType: instance.flavor?.name ?? "",
      host: publicIPv4(instance),
      // Names identify inventory for display; only a lease's exact identity grants deletion.
      labels: { crabbox: "true", provider: "ovh" },
      ...(region ? { region } : {}),
    };
  }

  private async prepareAllocation(
    config: LeaseConfig,
    leaseID: string,
    region: string,
  ): Promise<{ flavor: OVHFlavor; image: OVHImage; key: OVHSSHKey }> {
    try {
      const [flavors, images] = await Promise.all([
        this.request("GET", this.regionPath("flavor", region)),
        this.request("GET", this.regionPath("image", region)),
      ]);
      const flavor = selectFlavor(
        requireList<OVHFlavor>(flavors, "flavor list"),
        config.serverType,
      );
      const image = selectImage(requireList<OVHImage>(images, "image list"), this.image);
      const key = await this.ensureSSHKey(providerKeyForLease(leaseID), config.sshPublicKey);
      return { flavor, image, key };
    } catch (error) {
      // Nothing billable has been requested yet.
      throw new OVHAllocationRejectedError(errorText(error), undefined, { cause: error });
    }
  }

  private async allocateInstance(
    name: string,
    region: string,
    flavor: OVHFlavor,
    image: OVHImage,
    key: OVHSSHKey,
    config: LeaseConfig,
  ): Promise<OVHInstance> {
    let instance: OVHInstance | undefined;
    try {
      // A 2xx without an instance proves nothing, so it takes the lost-response path.
      instance = requireInstance(
        await this.request("POST", this.cloudPath("instance"), {
          name,
          region,
          flavorId: flavor.id,
          imageId: image.id,
          sshKeyId: key.id,
          userData: cloudInit(config),
        }),
        "create",
      );
    } catch (error) {
      if (ovhCreateRejected(error)) {
        const removed = await this.removeKeyQuietly(key.id);
        throw new OVHAllocationRejectedError(errorText(error), removed ? undefined : key.id, {
          cause: error,
        });
      }
      instance = await this.recoverAcceptedCreate(name, region, key, error);
    }
    if (instance.name !== name) {
      await this.removeKeyQuietly(key.id);
      throw new ProviderResourceUnresolvedError(
        `OVH create returned instance ${instance.id} named ${instance.name}, expected ${name}`,
      );
    }
    return instance;
  }

  // A lost response cannot prove absence: the create may have been accepted.
  private async recoverAcceptedCreate(
    name: string,
    region: string,
    key: OVHSSHKey,
    createError: unknown,
  ): Promise<OVHInstance> {
    try {
      for (let read = 0; read < createRecoveryReads; read += 1) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- this delay is the polling interval.
        if (read > 0) await this.sleep(this.pollDelayMs);
        // oxlint-disable-next-line eslint/no-await-in-loop -- each read follows the previous one's delay.
        const found = await this.findInstanceByName(name, region).catch((error: unknown) => {
          if (error instanceof ProviderResourceUnresolvedError) throw error;
          return undefined;
        });
        if (found) return found;
      }
    } catch (error) {
      await this.removeKeyQuietly(key.id);
      throw error;
    }
    await this.removeKeyQuietly(key.id);
    throw new ProviderProvisioningOutcomeUncertainError(
      `OVH create outcome is unknown for ${name}: ${errorText(createError)}`,
      { cause: createError },
    );
  }

  private async waitForPublicIPv4(id: string, region: string): Promise<ProviderMachine> {
    const deadline = Date.now() + this.maxWaitMs;
    let lastTransient = "";
    while (Date.now() < deadline) {
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- polling must wait between OVH API reads.
        const instance = await this.getInstance(id);
        const status = instance.status?.toUpperCase();
        if (status === "ERROR") throw new OVHInstanceFailedError(id, status);
        const machine = this.toMachine(instance, region);
        if (machine.host) return machine;
        lastTransient = "";
      } catch (error) {
        if (!ovhTransientError(error)) throw error;
        lastTransient = errorText(error);
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- this delay is the polling interval.
      await this.sleep(this.pollDelayMs);
    }
    throw new Error(
      `timed out waiting for OVH instance ${id} public IPv4${lastTransient ? ` after transient error: ${lastTransient}` : ""}`,
    );
  }

  private async deleteOwnedInstance(
    lease: OVHLeaseIdentity,
    assertOwner: () => Promise<void>,
  ): Promise<void> {
    const instance = await this.findInstance(lease.cloudID);
    if (!instance) {
      await this.assertProjectVisible();
      return;
    }
    if (ovhInstanceDeleted(instance)) return;
    if (!ovhInstanceOwnedByLease(instance, lease)) {
      throw new ProviderResourceUnresolvedError(
        `refusing to delete OVH instance ${lease.cloudID}: it no longer matches lease ${lease.id}`,
      );
    }
    if (instance.status?.toUpperCase() !== "DELETING") {
      await assertOwner();
      try {
        await this.request<void>("DELETE", this.cloudPath("instance", lease.cloudID));
      } catch (error) {
        if (!isOVHNotFound(error)) throw error;
        return;
      }
    }
    await this.confirmInstanceAbsent(lease.cloudID);
  }

  private async confirmInstanceAbsent(id: string): Promise<void> {
    const deadline = Date.now() + this.deleteWaitMs;
    while (Date.now() < deadline) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- polling must wait between OVH API reads.
      const instance = await this.findInstance(id).catch((error: unknown) => {
        if (!ovhTransientError(error)) throw error;
        return { id, name: "", status: "unknown" };
      });
      if (!instance || ovhInstanceDeleted(instance)) return;
      // oxlint-disable-next-line eslint/no-await-in-loop -- this delay is the polling interval.
      await this.sleep(this.pollDelayMs);
    }
    throw new Error(`OVH instance ${id} deletion is not confirmed`);
  }

  private async deleteRetainedKey(
    lease: OVHLeaseIdentity,
    keyID: string,
    assertOwner: () => Promise<void>,
  ): Promise<void> {
    let key: OVHSSHKey;
    try {
      key = await this.request<OVHSSHKey>("GET", this.cloudPath("sshkey", keyID));
    } catch (error) {
      if (isOVHNotFound(error)) return;
      throw error;
    }
    if (key.name !== providerKeyForLease(lease.id)) {
      throw new ProviderResourceUnresolvedError(
        `refusing to delete OVH SSH key ${keyID}: it is not owned by lease ${lease.id}`,
      );
    }
    await assertOwner();
    await this.deleteKey(keyID);
  }

  // Only a key the create path could not remove is left to find.
  private async sweepLeaseKey(leaseID: string, assertOwner: () => Promise<void>): Promise<void> {
    const keyName = providerKeyForLease(leaseID);
    const keys = await this.listSSHKeys();
    for (const key of keys.filter((candidate) => candidate.name === keyName)) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- each delete re-checks cleanup ownership first.
      await assertOwner();
      // oxlint-disable-next-line eslint/no-await-in-loop -- each delete re-checks cleanup ownership first.
      await this.deleteKey(key.id);
    }
  }

  private async listSSHKeys(): Promise<OVHSSHKey[]> {
    return requireList<OVHSSHKey>(
      await this.request("GET", this.cloudPath("sshkey")),
      "SSH key list",
    );
  }

  private async deleteKey(id: string): Promise<void> {
    try {
      await this.request<void>("DELETE", this.cloudPath("sshkey", id));
    } catch (error) {
      if (!isOVHNotFound(error)) throw error;
    }
  }

  private async removeKeyQuietly(id: string): Promise<boolean> {
    try {
      await this.deleteKey(id);
      return true;
    } catch {
      return false;
    }
  }

  private async ensureSSHKey(name: string, publicKey: string): Promise<OVHSSHKey> {
    try {
      return await this.createSSHKey(name, publicKey);
    } catch (error) {
      if (ovhCreateRejected(error)) throw error;
      const keys = await this.listSSHKeys();
      const identity = sshPublicKeyIdentity(publicKey);
      const existing = keys.find((key) => key.name === name);
      if (!existing || sshPublicKeyIdentity(existing.publicKey ?? "") !== identity) {
        throw new Error(`OVH SSH key ${name} could not be created for this lease`, {
          cause: error,
        });
      }
      return existing;
    }
  }

  private async hourlyPlanCode(
    serverType: string,
    region: string,
    signal: AbortSignal,
  ): Promise<string> {
    const flavors = await this.request<unknown>(
      "GET",
      this.regionPath("flavor", region),
      undefined,
      signal,
    ).catch(() => []);
    const flavor = requireList<OVHFlavor>(flavors, "flavor list").find(
      (candidate) => candidate.name === serverType,
    );
    return flavor?.planCodes?.hourly || `${serverType}.consumption`;
  }

  private async publicCatalog(signal: AbortSignal): Promise<unknown> {
    const subsidiary = ovhSubsidiaryForEndpoint(this.endpoint);
    const path = `/order/catalog/public/cloud?ovhSubsidiary=${subsidiary}`;
    return await this.send("GET", path, undefined, new Headers(), signal, maxCatalogBytes);
  }

  private async findInstance(id: string): Promise<OVHInstance | undefined> {
    try {
      return await this.getInstance(id);
    } catch (error) {
      if (isOVHNotFound(error)) return undefined;
      throw error;
    }
  }

  private async getInstance(id: string): Promise<OVHInstance> {
    return requireInstance(await this.request("GET", this.cloudPath("instance", id)), "read");
  }

  private async listInstances(): Promise<OVHInstance[]> {
    const instances = await this.request("GET", this.cloudPath("instance"));
    return requireList<unknown>(instances, "instance list").map((instance) =>
      requireInstance(instance, "list"),
    );
  }

  private async findInstanceByName(name: string, region: string): Promise<OVHInstance | undefined> {
    const matches = (await this.listInstances()).filter(
      (instance) => instance.name === name && (!instance.region || instance.region === region),
    );
    if (matches.length > 1) {
      throw new ProviderResourceUnresolvedError(
        `refusing ambiguous OVH recovery for ${name}: matched ${matches.length} instances`,
      );
    }
    return matches[0];
  }

  // A 404 from a project these credentials cannot see says nothing about the instance.
  private async assertProjectVisible(): Promise<void> {
    try {
      await this.listRegions();
    } catch (error) {
      if (!isOVHNotFound(error)) throw error;
      throw new ProviderResourceUnresolvedError(
        `OVH project is not visible to the configured credentials; instance absence cannot be trusted`,
        { cause: error },
      );
    }
  }

  private assertProject(lease: OVHLeaseIdentity): void {
    if (lease.providerScope !== this.providerScope) {
      throw new ProviderResourceUnresolvedError(
        `lease ${lease.id} belongs to another OVH project; the configured project cannot act on it`,
      );
    }
  }

  private cloudPath(...parts: string[]): string {
    return `/${["cloud", "project", this.projectID, ...parts].map(encodeURIComponent).join("/")}`;
  }

  private regionPath(resource: string, region: string): string {
    return `${this.cloudPath(resource)}?${new URLSearchParams({ region })}`;
  }

  private async request<T>(
    method: string,
    path: string,
    body?: unknown,
    signal?: AbortSignal,
  ): Promise<T> {
    const delta = await this.serverTimeOffset(signal);
    const timestamp = String(Math.floor(Date.now() / 1000) + delta);
    const payload = body === undefined ? "" : JSON.stringify(body);
    const signature = await sha1Hex(
      [
        this.applicationSecret,
        this.consumerKey,
        method,
        `${this.endpoint}${path}`,
        payload,
        timestamp,
      ].join("+"),
    );
    const headers = new Headers({
      "x-ovh-application": this.applicationKey,
      "x-ovh-consumer": this.consumerKey,
      "x-ovh-timestamp": timestamp,
      "x-ovh-signature": `$1$${signature}`,
    });
    return (await this.send(method, path, payload, headers, signal)) as T;
  }

  private async serverTimeOffset(signal?: AbortSignal): Promise<number> {
    this.serverTimeDelta ??= this.fetchServerTimeOffset(signal).catch((error: unknown) => {
      this.serverTimeDelta = undefined;
      throw error;
    });
    return await this.serverTimeDelta;
  }

  private async fetchServerTimeOffset(signal?: AbortSignal): Promise<number> {
    const serverTime = await this.send("GET", "/auth/time", undefined, new Headers(), signal);
    if (typeof serverTime !== "number" || !Number.isFinite(serverTime)) {
      throw new Error("OVH /auth/time returned no timestamp");
    }
    return serverTime - Math.floor(Date.now() / 1000);
  }

  private async send(
    method: string,
    path: string,
    payload: string | undefined,
    headers: Headers,
    callerSignal?: AbortSignal,
    maxBytes = maxResponseBytes,
  ): Promise<unknown> {
    headers.set("accept", "application/json");
    if (payload) headers.set("content-type", "application/json");
    const init: RequestInit = {
      method,
      headers,
      // Signed headers must never follow a redirect to another origin.
      redirect: "manual",
      signal: providerRequestSignal(
        providerRequestTimeoutMs,
        callerSignal,
        "ovh",
        `${method} ${path}`,
      ),
    };
    if (payload) init.body = payload;
    const response = await this.fetcher(`${this.endpoint}${path}`, init);
    const text = await boundedResponseText(response, maxBytes);
    if (!response.ok) {
      throw new OVHHTTPError(method, path, response.status, this.redact(text.slice(0, 400)));
    }
    return text.trim() ? (JSON.parse(text) as unknown) : undefined;
  }

  private redact(value: string): string {
    return redactDiagnosticSecrets(value, [
      this.applicationKey,
      this.applicationSecret,
      this.consumerKey,
    ]).replaceAll(/\$1\$[0-9a-fA-F]{40}/g, "[redacted-signature]");
  }

  private sleep(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }
}

class OVHInstanceFailedError extends Error {
  constructor(id: string, status: string) {
    super(`OVH instance ${id} entered status=${status}`);
    this.name = "OVHInstanceFailedError";
  }
}

export function ovhEndpoint(value: string | undefined): string {
  const configured = ovhEndpointAlias(value?.trim() ?? "");
  let url: URL;
  try {
    url = new URL(configured);
  } catch {
    throw new Error("OVH_ENDPOINT must be an absolute URL");
  }
  const host = url.hostname.toLowerCase();
  const trustedHost = ["ovhcloud.com", "ovh.com"].some(
    (domain) => host === domain || host.endsWith(`.${domain}`),
  );
  if (url.protocol !== "https:" || !trustedHost) {
    throw new Error("OVH_ENDPOINT must use https and an OVH or OVHcloud API host");
  }
  if (url.username || url.password || url.search || url.hash) {
    throw new Error("OVH_ENDPOINT must not contain credentials, query, or fragment");
  }
  return url.toString().replace(/\/+$/, "");
}

function ovhEndpointAlias(value: string): string {
  switch (value.toLowerCase()) {
    case "":
    case "ovh-us":
      return defaultEndpoint;
    case "ovh-ca":
      return "https://ca.api.ovh.com/1.0";
    case "ovh-eu":
      return "https://eu.api.ovh.com/1.0";
    default:
      return value;
  }
}

// Catalog prices are per subsidiary; these match each endpoint's default billing currency.
export function ovhSubsidiaryForEndpoint(endpoint: string): string {
  const host = new URL(endpoint).hostname.toLowerCase();
  if (host.includes(".us.") || host.startsWith("us.")) return "US";
  return host.startsWith("ca.") ? "CA" : "FR";
}

export function ovhCatalogHourlyUSD(
  catalog: unknown,
  planCode: string,
  eurRate: number,
): number | undefined {
  const value = catalog as {
    locale?: { currencyCode?: unknown };
    addons?: unknown;
  } | null;
  if (!value || !Array.isArray(value.addons)) return undefined;
  const addons = value.addons as Array<{
    planCode?: unknown;
    pricings?: Array<{ price?: unknown; type?: unknown }>;
  }>;
  const pricings = addons.find((addon) => addon.planCode === planCode)?.pricings;
  const price = Array.isArray(pricings)
    ? pricings.find((pricing) => pricing.type === "consumption")?.price
    : undefined;
  if (typeof price !== "number" || !Number.isFinite(price) || price <= 0) return undefined;
  const hourly = price / catalogPriceDivisor;
  const currency = value.locale?.currencyCode;
  if (currency === "USD") return roundHourlyUSD(hourly);
  return currency === "EUR" ? roundHourlyUSD(hourly * eurRate) : undefined;
}

// Public Cloud rates are fractions of a cent per hour for small flavors.
function roundHourlyUSD(value: number): number {
  return Math.round(value * 10_000) / 10_000;
}

export function ovhInstanceOwnedByLease(
  instance: Pick<OVHInstance, "id" | "name" | "region">,
  lease: Pick<OVHLeaseIdentity, "cloudID" | "region" | "providerResourceID">,
): boolean {
  return (
    Boolean(lease.cloudID && lease.providerResourceID) &&
    instance.id === lease.cloudID &&
    instance.name === lease.providerResourceID &&
    (!instance.region || instance.region === lease.region)
  );
}

function requireInstance(value: unknown, operation: string): OVHInstance {
  const instance = value as Partial<OVHInstance> | null | undefined;
  if (typeof instance?.id !== "string" || !instance.id || typeof instance.name !== "string") {
    throw new Error(`OVH instance ${operation} returned no instance`);
  }
  return instance as OVHInstance;
}

function requireList<T>(value: unknown, operation: string): T[] {
  if (!Array.isArray(value)) throw new Error(`OVH ${operation} returned no list`);
  return value as T[];
}

// Rate overrides and catalog plans key on the name, so an id could evade cost admission.
function selectFlavor(flavors: OVHFlavor[], value: string): OVHFlavor {
  const flavor = selectExact(flavors, value, "flavor", false);
  const label = flavor.id || flavor.name;
  if (flavor.available === false) throw new Error(`ovh flavor ${label} is unavailable`);
  if (flavor.quota !== undefined && flavor.quota <= 0) {
    throw new Error(`ovh flavor ${label} has no remaining project quota`);
  }
  const osType = flavor.osType?.trim().toLowerCase();
  if (osType && osType !== "linux") {
    throw new Error(`ovh flavor ${label} is for osType=${flavor.osType}, not linux`);
  }
  return flavor;
}

function selectImage(images: OVHImage[], value: string): OVHImage {
  const image = selectExact(images, value, "image");
  const label = image.id || image.name;
  const status = image.status?.trim().toLowerCase();
  if (status && status !== "active") {
    throw new Error(`ovh image ${label} has status=${image.status}`);
  }
  const type = image.type?.trim().toLowerCase();
  if (type && type !== "linux") throw new Error(`ovh image ${label} has type=${type}, not linux`);
  const name = image.name.toLowerCase();
  if (!name.includes("ubuntu") && !name.includes("debian")) {
    throw new Error(`ovh image ${label} is not a supported Debian or Ubuntu image`);
  }
  const visibility = image.visibility?.trim().toLowerCase();
  if (image.id !== value && visibility && visibility !== "public") {
    throw new Error(
      `ovh image name ${image.name} resolves to visibility=${visibility}; configure its exact id to use a non-public image`,
    );
  }
  return image;
}

function selectExact<T extends { id: string; name: string }>(
  records: T[],
  value: string,
  kind: string,
  matchID = true,
): T {
  const byID = matchID ? records.find((record) => record.id === value) : undefined;
  if (byID) return byID;
  const matches = records.filter((record) => record.name === value);
  const [match] = matches;
  if (!match) throw new Error(`ovh ${kind} ${value} was not returned`);
  if (matches.length > 1) {
    throw new Error(
      `ovh ${kind} name ${value} is ambiguous across ${matches.length} records; configure an exact id`,
    );
  }
  return match;
}

function publicIPv4(instance: OVHInstance): string {
  let fallback = "";
  const addresses = Array.isArray(instance.ipAddresses) ? instance.ipAddresses : [];
  for (const address of addresses) {
    const ip = address.ip?.trim() ?? "";
    if (!isPublicIPv4(ip)) continue;
    const type = address.type?.trim().toLowerCase() ?? "";
    if (type === "public" || type === "external" || type === "floating") return ip;
    if (type === "" && !fallback) fallback = ip;
  }
  return fallback;
}

function isPublicIPv4(value: string): boolean {
  const octets = value.split(".").map(Number);
  if (octets.length !== 4 || octets.some((part) => !Number.isInteger(part) || part > 255)) {
    return false;
  }
  const [a = 0, b = 0] = octets;
  return !(
    a === 0 ||
    a === 10 ||
    a === 127 ||
    a >= 224 ||
    (a === 169 && b === 254) ||
    (a === 172 && b >= 16 && b <= 31) ||
    (a === 192 && b === 168)
  );
}

function ovhInstanceDeleted(instance: OVHInstance): boolean {
  return instance.status?.toUpperCase() === "DELETED";
}

export function isOVHNotFound(error: unknown): boolean {
  return error instanceof OVHHTTPError && error.status === 404;
}

// Only a completed 4xx proves the create was refused. Throttling and conflicts may race an
// accepted request, and transport failures say nothing about what the provider did.
function ovhCreateRejected(error: unknown): boolean {
  return (
    error instanceof OVHHTTPError &&
    error.status >= 400 &&
    error.status < 500 &&
    ![408, 409, 425, 429].includes(error.status)
  );
}

function ovhTransientError(error: unknown): boolean {
  if (error instanceof OVHInstanceFailedError) return false;
  if (!(error instanceof OVHHTTPError)) return true;
  return [404, 408, 409, 425, 429].includes(error.status) || error.status >= 500;
}

async function boundedResponseText(response: Response, maxBytes: number): Promise<string> {
  const exceeded = () => new Error(`OVH response exceeds ${maxBytes} bytes`);
  if (Number(response.headers.get("content-length") ?? 0) > maxBytes) {
    await response.body?.cancel();
    throw exceeded();
  }
  const reader = response.body?.getReader();
  if (!reader) return "";
  const decoder = new TextDecoder();
  let text = "";
  let bytes = 0;
  for (;;) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- stream chunks arrive in order.
    const { done, value } = await reader.read();
    if (done) return text + decoder.decode();
    bytes += value.byteLength;
    if (bytes > maxBytes) {
      void reader.cancel("response body exceeded limit");
      throw exceeded();
    }
    text += decoder.decode(value, { stream: true });
  }
}

function requiredEnv(value: string | undefined, name: string): string {
  const trimmed = value?.trim();
  if (!trimmed) throw new Error(`${name} is required for OVH leases`);
  return trimmed;
}

async function sha1Hex(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-1", new TextEncoder().encode(value));
  return bytesToHex(new Uint8Array(digest));
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
