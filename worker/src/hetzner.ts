import { cloudInit } from "./bootstrap";
import {
  serverTypeCandidatesForClass,
  workspaceProviderKeyPrefix,
  type LeaseConfig,
} from "./config";
import {
  leaseIDForProviderKey,
  providerKeyForLease,
  providerKeyOwnedByLease,
  providerKeyOwnershipLabels,
  sshPublicKeyIdentity,
} from "./provider-key";
import { leaseProviderLabels } from "./provider-labels";
import { leaseProviderName } from "./slug";
import type { Env, HetznerSSHKey, HetznerServer, ProviderMachine } from "./types";

interface HetznerListServersResponse {
  servers: HetznerServer[];
}

interface HetznerListSSHKeysResponse {
  ssh_keys: HetznerSSHKey[];
}

interface HetznerSSHKeyResponse {
  ssh_key: HetznerSSHKey;
}

interface HetznerServerResponse {
  server: HetznerServer;
}

interface HetznerListServerTypesResponse {
  server_types: HetznerServerType[];
}

interface HetznerServerType {
  name: string;
  prices: HetznerServerTypePrice[];
}

interface HetznerServerTypePrice {
  location: string;
  price_hourly: {
    net?: string;
    gross?: string;
  };
}

interface EnsuredHetznerSSHKey {
  key: HetznerSSHKey;
  created: boolean;
}

export class HetznerProvisioningError extends Error {
  constructor(
    message: string,
    readonly resourceMayExist: boolean,
    readonly retryable: boolean,
    readonly serverID?: number,
    readonly providerKeyCleanupID?: number,
  ) {
    super(message);
    this.name = "HetznerProvisioningError";
  }
}

export function hetznerProvisioningFailureMayHaveResource(error: unknown): boolean {
  if (error instanceof HetznerProvisioningError) {
    return error.resourceMayExist;
  }
  return errorText(error).includes("timed out waiting for server IP");
}

export function hetznerProvisioningFailureRetryable(error: unknown): boolean {
  if (error instanceof HetznerProvisioningError) {
    return error.retryable;
  }
  return errorText(error).includes("timed out waiting for server IP");
}

export function hetznerProvisioningResourceID(error: unknown): number | undefined {
  if (!(error instanceof HetznerProvisioningError)) {
    return undefined;
  }
  return Number.isSafeInteger(error.serverID) && (error.serverID ?? 0) > 0
    ? error.serverID
    : undefined;
}

export class HetznerClient {
  private readonly token: string;

  constructor(private readonly env: Env) {
    if (!env.HETZNER_TOKEN) {
      throw new Error("HETZNER_TOKEN secret is required");
    }
    this.token = env.HETZNER_TOKEN;
  }

  async listCrabboxServers(): Promise<HetznerServer[]> {
    const query = new URLSearchParams({
      label_selector: "crabbox=true",
      per_page: "100",
    });
    const response = await this.request<HetznerListServersResponse>("GET", `/servers?${query}`);
    return response.servers;
  }

  async findServerByLease(leaseID: string): Promise<HetznerServer | undefined> {
    const query = new URLSearchParams({
      label_selector: `crabbox=true,lease=${leaseID}`,
      per_page: "1",
    });
    const response = await this.request<HetznerListServersResponse>("GET", `/servers?${query}`);
    return response.servers[0];
  }

  async ensureSSHKey(name: string, publicKey: string, leaseID?: string): Promise<HetznerSSHKey> {
    return (await this.ensureSSHKeyForProvisioning(name, publicKey, leaseID)).key;
  }

  private async ensureSSHKeyForProvisioning(
    name: string,
    publicKey: string,
    leaseID?: string,
  ): Promise<EnsuredHetznerSSHKey> {
    const identity = sshPublicKeyIdentity(publicKey);
    const keyLeaseID = leaseIDForProviderKey(name);
    if (keyLeaseID && keyLeaseID !== leaseID) {
      throw new Error(`hetzner ssh key ${name} is reserved for lease ${keyLeaseID}`);
    }
    const leaseOwned = leaseID !== undefined && keyLeaseID === leaseID;
    const byName = await this.request<HetznerListSSHKeysResponse>(
      "GET",
      `/ssh_keys?${new URLSearchParams({ name })}`,
    );
    for (const key of byName.ssh_keys) {
      if (key.name === name) {
        if (sshPublicKeyIdentity(key.public_key) !== identity) {
          throw new Error(`hetzner ssh key ${name} exists with different public key`);
        }
        if (leaseOwned && !providerKeyOwnedByLease(key.labels ?? {}, leaseID)) {
          throw new Error(`hetzner ssh key ${name} is not owned by lease ${leaseID}`);
        }
        return { key, created: false };
      }
    }

    const existingIdentity = reusableHetznerSSHKey(
      await this.listSSHKeys(),
      name,
      identity,
      leaseOwned ? leaseID : undefined,
    );
    if (existingIdentity) {
      return { key: existingIdentity, created: false };
    }

    try {
      const created = await this.request<HetznerSSHKeyResponse>("POST", "/ssh_keys", {
        name,
        public_key: publicKey,
        labels: leaseOwned
          ? providerKeyOwnershipLabels(leaseID)
          : { crabbox: "true", created_by: "crabbox" },
      });
      return { key: created.ssh_key, created: true };
    } catch (error) {
      if (!String(error).includes("uniqueness_error")) {
        throw error;
      }
      const key = reusableHetznerSSHKey(
        await this.listSSHKeys(),
        name,
        identity,
        leaseOwned ? leaseID : undefined,
      );
      if (key) {
        return { key, created: false };
      }
      throw error;
    }
  }

  async deleteSSHKey(name: string, leaseID: string): Promise<void> {
    if (name !== providerKeyForLease(leaseID)) {
      return;
    }
    const byName = await this.request<HetznerListSSHKeysResponse>(
      "GET",
      `/ssh_keys?${new URLSearchParams({ name })}`,
    );
    const key = byName.ssh_keys.find((entry) => entry.name === name);
    if (key && providerKeyOwnedByLease(key.labels ?? {}, leaseID)) {
      await this.request<void>("DELETE", `/ssh_keys/${key.id}`);
    } else if (key) {
      console.warn(`Hetzner SSH key cleanup skipped unowned key lease=${leaseID} key=${name}`);
    }
  }

  async deleteSSHKeyByID(id: number): Promise<void> {
    try {
      await this.request<void>("DELETE", `/ssh_keys/${id}`);
    } catch (error) {
      if (!hetznerResourceNotFound(error)) {
        throw error;
      }
    }
  }

  async hourlyPriceUSD(name: string, location: string): Promise<number | undefined> {
    const response = await this.request<HetznerListServerTypesResponse>(
      "GET",
      `/server_types?${new URLSearchParams({ per_page: "100" })}`,
    );
    const serverType = response.server_types.find((candidate) => candidate.name === name);
    const price =
      serverType?.prices.find((candidate) => candidate.location === location) ??
      serverType?.prices[0];
    const hourlyEUR = positiveFloat(price?.price_hourly.gross ?? price?.price_hourly.net ?? "");
    if (hourlyEUR === undefined) {
      return undefined;
    }
    return roundUSD(hourlyEUR * eurToUSD(this.env));
  }

  async createServerWithFallback(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<{ server: HetznerServer; serverType: string; providerKey: string }> {
    const requireRunning = config.providerKey.startsWith(workspaceProviderKeyPrefix);
    let ensuredKey: EnsuredHetznerSSHKey;
    try {
      ensuredKey = await this.ensureSSHKeyForProvisioning(
        config.providerKey,
        config.sshPublicKey,
        leaseID,
      );
    } catch (error) {
      const message = errorText(error);
      throw new HetznerProvisioningError(message, false, transientHetznerError(message));
    }
    const providerKeyCleanupOwned =
      ensuredKey.created &&
      ensuredKey.key.name === providerKeyForLease(leaseID) &&
      providerKeyOwnedByLease(ensuredKey.key.labels ?? {}, leaseID);
    const resolvedConfig = { ...config, providerKey: ensuredKey.key.name };
    const candidates = prependUnique(
      resolvedConfig.serverType,
      serverTypeCandidatesForClass(resolvedConfig.class),
    );
    const failures: string[] = [];
    let resourceMayExist = false;
    let retryable = false;
    let serverID: number | undefined;
    let providerKeyCleanupID: number | undefined;
    for (const serverType of candidates) {
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- server-type fallback must stay sequential.
        const server = await this.createServer(
          { ...resolvedConfig, serverType },
          leaseID,
          slug,
          owner,
          requireRunning,
        );
        return { server, serverType, providerKey: ensuredKey.key.name };
      } catch (error) {
        const message = errorText(error);
        failures.push(`${serverType}: ${message}`);
        resourceMayExist = hetznerProvisioningFailureMayHaveResource(error);
        retryable = hetznerProvisioningFailureRetryable(error);
        serverID = hetznerProvisioningResourceID(error);
        if (resourceMayExist || !(retryable || isRetryableProvisioningError(message))) {
          break;
        }
      }
    }
    if (providerKeyCleanupOwned) {
      if (resourceMayExist) {
        providerKeyCleanupID = ensuredKey.key.id;
      } else {
        try {
          await this.deleteSSHKeyByID(ensuredKey.key.id);
        } catch (error) {
          failures.push(`cleanup ssh key ${ensuredKey.key.id}: ${errorText(error)}`);
          providerKeyCleanupID = ensuredKey.key.id;
        }
      }
    }
    throw new HetznerProvisioningError(
      failures.join("; "),
      resourceMayExist,
      retryable,
      serverID,
      providerKeyCleanupID,
    );
  }

  async getServer(id: number): Promise<HetznerServer> {
    return (await this.request<HetznerServerResponse>("GET", `/servers/${id}`)).server;
  }

  async waitForServerIP(id: number, requireRunning = false): Promise<HetznerServer> {
    const deadline = Date.now() + 60_000;
    while (Date.now() < deadline) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- polling must wait between Hetzner API reads.
      const server = await this.getServer(id);
      if (server.public_net.ipv4.ip && (!requireRunning || server.status === "running")) {
        return server;
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- this delay is the polling interval.
      await sleep(2_000);
    }
    throw new Error(`timed out waiting for server IP: ${id}`);
  }

  async deleteServer(id: number): Promise<void> {
    await this.request<void>("DELETE", `/servers/${id}`);
  }

  toMachine(server: HetznerServer): ProviderMachine {
    return {
      provider: "hetzner",
      id: server.id,
      cloudID: String(server.id),
      name: server.name,
      status: server.status,
      serverType: server.server_type.name,
      host: server.public_net.ipv4.ip,
      labels: server.labels,
    };
  }

  private async createServer(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
    requireRunning: boolean,
  ): Promise<HetznerServer> {
    const now = new Date();
    const name = leaseProviderName(leaseID, slug);
    const labels = leaseProviderLabels(config, leaseID, slug, owner, "hetzner", now);
    let response: HetznerServerResponse;
    try {
      response = await this.request<HetznerServerResponse>("POST", "/servers", {
        name,
        server_type: config.serverType,
        image: config.image,
        location: config.location,
        labels,
        ssh_keys: [config.providerKey],
        user_data: cloudInit(config),
        start_after_create: true,
        public_net: {
          enable_ipv4: true,
          enable_ipv6: false,
        },
      });
    } catch (error) {
      const message = errorText(error);
      const status = /hetzner POST \/servers: http (\d{3})/.exec(message)?.[1];
      throw new HetznerProvisioningError(
        message,
        status === undefined || Number.parseInt(status, 10) >= 500,
        transientHetznerError(message) || isRetryableProvisioningError(message),
      );
    }
    if (
      response.server.public_net.ipv4.ip &&
      (!requireRunning || response.server.status === "running")
    ) {
      return response.server;
    }
    try {
      return await this.waitForServerIP(response.server.id, requireRunning);
    } catch (error) {
      const message = errorText(error);
      try {
        await this.deleteServer(response.server.id);
      } catch (cleanupError) {
        if (!hetznerResourceNotFound(cleanupError)) {
          throw new HetznerProvisioningError(
            `${message}; cleanup server ${response.server.id}: ${errorText(cleanupError)}`,
            true,
            true,
            response.server.id,
          );
        }
      }
      throw new HetznerProvisioningError(message, false, true);
    }
  }

  private async listSSHKeys(): Promise<HetznerSSHKey[]> {
    const keys: HetznerSSHKey[] = [];
    const perPage = 50;
    for (let page = 1; page <= 100; page += 1) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- Hetzner SSH key pagination is sequential.
      const response = await this.request<HetznerListSSHKeysResponse>(
        "GET",
        `/ssh_keys?${new URLSearchParams({ page: String(page), per_page: String(perPage) })}`,
      );
      keys.push(...response.ssh_keys);
      if (response.ssh_keys.length < perPage) {
        break;
      }
    }
    return keys;
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const init: RequestInit = {
      method,
      headers: {
        authorization: `Bearer ${this.token}`,
        "content-type": "application/json",
      },
    };
    if (body !== undefined) {
      init.body = JSON.stringify(body);
    }
    const response = await fetch(`https://api.hetzner.cloud/v1${path}`, init);
    if (!response.ok) {
      throw new Error(
        `hetzner ${method} ${path}: http ${response.status}: ${await safeBody(response)}`,
      );
    }
    if (response.status === 204) {
      return undefined as T;
    }
    return (await response.json()) as T;
  }
}

export function isRetryableProvisioningError(message: string): boolean {
  return (
    message.includes("dedicated_core_limit") ||
    message.includes("resource_limit_exceeded") ||
    message.includes("server_type_not_available") ||
    message.includes("location_not_available")
  );
}

function reusableHetznerSSHKey(
  keys: HetznerSSHKey[],
  requestedName: string,
  identity: string,
  leaseID?: string,
): HetznerSSHKey | undefined {
  const exact = keys.find((entry) => entry.name === requestedName);
  if (exact) {
    if (sshPublicKeyIdentity(exact.public_key) !== identity) {
      throw new Error(`hetzner ssh key ${requestedName} exists with different public key`);
    }
    if (leaseID !== undefined && !providerKeyOwnedByLease(exact.labels ?? {}, leaseID)) {
      throw new Error(`hetzner ssh key ${requestedName} is not owned by lease ${leaseID}`);
    }
    return exact;
  }
  // Hetzner makes public-key material account-unique. A differently named match
  // is therefore shared and retained; lease finalization records its actual name.
  return keys.find((entry) => sshPublicKeyIdentity(entry.public_key) === identity);
}

function prependUnique(first: string, rest: string[]): string[] {
  return [first, ...rest.filter((value) => value !== first)];
}

function eurToUSD(env: Env): number {
  const parsed = Number.parseFloat(env.CRABBOX_EUR_TO_USD ?? "");
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 1.08;
}

function positiveFloat(value: string): number | undefined {
  const parsed = Number.parseFloat(value);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : undefined;
}

function roundUSD(value: number): number {
  return Math.round(value * 100) / 100;
}

async function safeBody(response: Response): Promise<string> {
  const text = await response.text();
  return text.length > 500 ? `${text.slice(0, 500)}...` : text;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function hetznerResourceNotFound(error: unknown): boolean {
  const message = errorText(error);
  return message.includes("http 404") || message.includes("not_found");
}

function transientHetznerError(message: string): boolean {
  if (message.includes("exists with different public key")) {
    return false;
  }
  const status = /hetzner \w+ [^:]+: http (\d{3})/.exec(message)?.[1];
  return (
    status === undefined ||
    Number.parseInt(status, 10) === 429 ||
    Number.parseInt(status, 10) >= 500
  );
}
