import { cloudInit } from "./bootstrap";
import { serverTypeCandidatesForClass, type LeaseConfig } from "./config";
import { leaseProviderLabels } from "./provider-labels";
import { leaseProviderName } from "./slug";
import type {
  Env,
  HetznerImage,
  HetznerSSHKey,
  HetznerServer,
  ProviderImage,
  ProviderMachine,
} from "./types";

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

interface HetznerImageResponse {
  image: HetznerImage;
}

interface HetznerCreateImageResponse {
  image: HetznerImage;
  action: { id: number; status: string };
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

  async ensureSSHKey(name: string, publicKey: string): Promise<HetznerSSHKey> {
    const byName = await this.request<HetznerListSSHKeysResponse>(
      "GET",
      `/ssh_keys?${new URLSearchParams({ name })}`,
    );
    for (const key of byName.ssh_keys) {
      if (key.name === name) {
        if (key.public_key.trim() !== publicKey.trim()) {
          throw new Error(`hetzner ssh key ${name} exists with different public key`);
        }
        return key;
      }
    }

    const all = await this.request<HetznerListSSHKeysResponse>(
      "GET",
      `/ssh_keys?${new URLSearchParams({ per_page: "100" })}`,
    );
    for (const key of all.ssh_keys) {
      if (key.public_key.trim() === publicKey.trim()) {
        return key;
      }
    }

    const created = await this.request<HetznerSSHKeyResponse>("POST", "/ssh_keys", {
      name,
      public_key: publicKey,
      labels: {
        crabbox: "true",
        created_by: "crabbox",
      },
    });
    return created.ssh_key;
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
    const key = await this.ensureSSHKey(config.providerKey, config.sshPublicKey);
    const resolvedConfig = { ...config, providerKey: key.name };
    const candidates = resolvedConfig.strictServerType
      ? [resolvedConfig.serverType]
      : prependUnique(
          resolvedConfig.serverType,
          serverTypeCandidatesForClass(resolvedConfig.class),
        );
    const failures: string[] = [];
    for (const serverType of candidates) {
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- server-type fallback must stay sequential.
        const server = await this.createServer(
          { ...resolvedConfig, serverType },
          leaseID,
          slug,
          owner,
        );
        return { server, serverType };
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        failures.push(`${serverType}: ${message}`);
        if (!isRetryableProvisioningError(message)) {
          break;
        }
      }
    }
    throw new Error(failures.join("; "));
  }

  async getServer(id: number): Promise<HetznerServer> {
    return (await this.request<HetznerServerResponse>("GET", `/servers/${id}`)).server;
  }

  async waitForServerIP(id: number): Promise<HetznerServer> {
    const deadline = Date.now() + 60_000;
    while (Date.now() < deadline) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- polling must wait between Hetzner API reads.
      const server = await this.getServer(id);
      if (server.public_net.ipv4.ip) {
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

  async createImage(serverID: number, name: string): Promise<ProviderImage> {
    const response = await this.request<HetznerCreateImageResponse>(
      "POST",
      `/servers/${serverID}/actions/create_image`,
      {
        type: "snapshot",
        description: name,
        labels: {
          crabbox: "true",
          created_by: "crabbox",
          Name: name,
        },
      },
    );
    return toProviderImage(response.image);
  }

  async getImage(imageID: string): Promise<ProviderImage> {
    const response = await this.request<HetznerImageResponse>("GET", `/images/${imageID}`);
    return toProviderImage(response.image);
  }

  async deleteImage(imageID: string): Promise<void> {
    await this.request<void>("DELETE", `/images/${imageID}`);
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
  ): Promise<HetznerServer> {
    const now = new Date();
    const name = leaseProviderName(leaseID, slug);
    const labels = leaseProviderLabels(config, leaseID, slug, owner, "hetzner", now);
    const response = await this.request<HetznerServerResponse>("POST", "/servers", {
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
    return response.server.public_net.ipv4.ip
      ? response.server
      : await this.waitForServerIP(response.server.id);
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

function toProviderImage(image: HetznerImage): ProviderImage {
  return {
    id: String(image.id),
    name: image.description ?? "",
    state: hetznerImageState(image.status),
  };
}

function hetznerImageState(status: HetznerImage["status"]): string {
  switch (status) {
    case "available":
      return "available";
    case "creating":
      return "pending";
    case "unavailable":
      return "failed";
    default:
      return status;
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
