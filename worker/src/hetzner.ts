import { cloudInit } from "./bootstrap";
import {
  serverTypeCandidatesForClass,
  workspaceProviderKeyPrefix,
  type LeaseConfig,
} from "./config";
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

export class HetznerProvisioningError extends Error {
  constructor(
    message: string,
    readonly resourceMayExist: boolean,
    readonly retryable: boolean,
  ) {
    super(message);
    this.name = "HetznerProvisioningError";
  }
}

export function hetznerProvisioningFailureMayHaveResource(error: unknown): boolean {
  return (
    (error instanceof HetznerProvisioningError && error.resourceMayExist) ||
    errorText(error).includes("timed out waiting for server IP")
  );
}

export function hetznerProvisioningFailureRetryable(error: unknown): boolean {
  return (
    (error instanceof HetznerProvisioningError && error.retryable) ||
    errorText(error).includes("timed out waiting for server IP")
  );
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

  async ensureSSHKey(name: string, publicKey: string): Promise<HetznerSSHKey> {
    const identity = sshPublicKeyIdentity(publicKey);
    const byName = await this.request<HetznerListSSHKeysResponse>(
      "GET",
      `/ssh_keys?${new URLSearchParams({ name })}`,
    );
    for (const key of byName.ssh_keys) {
      if (key.name === name) {
        if (sshPublicKeyIdentity(key.public_key) !== identity) {
          throw new Error(`hetzner ssh key ${name} exists with different public key`);
        }
        return key;
      }
    }

    for (const key of await this.listSSHKeys()) {
      if (sshPublicKeyIdentity(key.public_key) === identity) {
        return key;
      }
    }

    try {
      const created = await this.request<HetznerSSHKeyResponse>("POST", "/ssh_keys", {
        name,
        public_key: publicKey,
        labels: {
          crabbox: "true",
          created_by: "crabbox",
        },
      });
      return created.ssh_key;
    } catch (error) {
      if (!String(error).includes("uniqueness_error")) {
        throw error;
      }
      const key = (await this.listSSHKeys()).find(
        (entry) => sshPublicKeyIdentity(entry.public_key) === identity,
      );
      if (key) {
        return key;
      }
      throw error;
    }
  }

  async deleteSSHKey(name: string): Promise<void> {
    const byName = await this.request<HetznerListSSHKeysResponse>(
      "GET",
      `/ssh_keys?${new URLSearchParams({ name })}`,
    );
    const key = byName.ssh_keys.find((entry) => entry.name === name);
    if (key) {
      await this.request<void>("DELETE", `/ssh_keys/${key.id}`);
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
  ): Promise<{ server: HetznerServer; serverType: string }> {
    const requireRunning = config.providerKey.startsWith(workspaceProviderKeyPrefix);
    let key: HetznerSSHKey;
    try {
      key = await this.ensureSSHKey(config.providerKey, config.sshPublicKey);
    } catch (error) {
      const message = errorText(error);
      throw new HetznerProvisioningError(message, false, transientHetznerError(message));
    }
    const resolvedConfig = { ...config, providerKey: key.name };
    const candidates = prependUnique(
      resolvedConfig.serverType,
      serverTypeCandidatesForClass(resolvedConfig.class),
    );
    const failures: string[] = [];
    let resourceMayExist = false;
    let retryable = false;
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
        return { server, serverType };
      } catch (error) {
        const message = errorText(error);
        failures.push(`${serverType}: ${message}`);
        resourceMayExist = hetznerProvisioningFailureMayHaveResource(error);
        retryable = hetznerProvisioningFailureRetryable(error);
        if (resourceMayExist || !isRetryableProvisioningError(message)) {
          break;
        }
      }
    }
    throw new HetznerProvisioningError(failures.join("; "), resourceMayExist, retryable);
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
      throw new HetznerProvisioningError(errorText(error), true, true);
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

export function sshPublicKeyIdentity(publicKey: string): string {
  const [type, encoded] = publicKey.trim().split(/\s+/, 3);
  return type && encoded ? `${type} ${encoded}` : publicKey.trim();
}

export function isRetryableProvisioningError(message: string): boolean {
  return (
    message.includes("dedicated_core_limit") ||
    message.includes("resource_limit_exceeded") ||
    message.includes("server_type_not_available") ||
    message.includes("location_not_available")
  );
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
