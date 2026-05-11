import { cloudInit } from "./bootstrap";
import { gcpMachineTypeCandidatesForClass, sshPorts, validCIDRs, type LeaseConfig } from "./config";
import { leaseProviderLabels } from "./provider-labels";
import { leaseProviderName } from "./slug";
import type { Env, ProviderMachine, ProvisioningAttempt } from "./types";

const computeBaseURL = "https://compute.googleapis.com/compute/v1";
const tokenURL = "https://oauth2.googleapis.com/token";
const defaultImage = "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64";
const firewallName = "crabbox-ssh";

interface TokenCache {
  token: string;
  expiresAt: number;
}

interface GCPInstance {
  id?: string;
  name?: string;
  status?: string;
  machineType?: string;
  zone?: string;
  labels?: Record<string, string>;
  networkInterfaces?: {
    accessConfigs?: { natIP?: string }[];
  }[];
}

interface GCPAggregatedInstanceList {
  items?: Record<string, { instances?: GCPInstance[] }>;
}

interface GCPOperation {
  name?: string;
  status?: string;
  error?: { errors?: { code?: string; message?: string }[] };
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
  private cache?: TokenCache;

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
    this.sshCIDRs = validCIDRs((env.CRABBOX_GCP_SSH_CIDRS ?? "").split(","));
    if (this.sshCIDRs.length === 0) this.sshCIDRs.push("0.0.0.0/0");
    this.rootGB = numberFromEnv(env.CRABBOX_GCP_ROOT_GB, 400);
    this.serviceAccount = env.CRABBOX_GCP_SERVICE_ACCOUNT?.trim() || "";
    if (!this.project) throw new Error("GCP_PROJECT_ID or CRABBOX_GCP_PROJECT secret is required");
    if (!env.GCP_CLIENT_EMAIL) throw new Error("GCP_CLIENT_EMAIL secret is required");
    if (!env.GCP_PRIVATE_KEY) throw new Error("GCP_PRIVATE_KEY secret is required");
  }

  async listCrabboxServers(): Promise<ProviderMachine[]> {
    const data = await this.gcp<GCPAggregatedInstanceList>(
      "GET",
      `/aggregated/instances?filter=${encodeURIComponent("labels.crabbox = true")}&returnPartialSuccess=true`,
    ).catch((error) => {
      if (isNotFound(error)) return { items: [] };
      throw error;
    });
    return Object.entries(data.items ?? {}).flatMap(([scope, list]) => {
      const zone = lastPathPart(scope);
      return (list.instances ?? []).map((instance) =>
        toMachine(instance, lastPathPart(instance.zone ?? zone)),
      );
    });
  }

  async createServerWithFallback(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<{
    server: ProviderMachine;
    serverType: string;
    market?: string;
    attempts?: ProvisioningAttempt[];
  }> {
    const candidates =
      config.serverTypeExplicit && config.serverType
        ? [config.serverType]
        : prependUnique(config.serverType, gcpMachineTypeCandidatesForClass(config.class));
    const zones = prependUnique(
      config.gcpZone || this.zone,
      config.capacityAvailabilityZones.length > 0 ? config.capacityAvailabilityZones : [this.zone],
    );
    const failures: string[] = [];
    const attempts: ProvisioningAttempt[] = [];
    const project = config.gcpProject || this.project;
    for (const zone of zones) {
      const client =
        zone === this.zone && project === this.project
          ? this
          : new GCPClient(this.env, zone, project);
      for (const machineType of candidates) {
        try {
          // oxlint-disable-next-line eslint/no-await-in-loop -- fallback must preserve capacity order.
          const server = await client.createServer(
            { ...config, gcpZone: zone, serverType: machineType },
            leaseID,
            slug,
            owner,
          );
          const result: {
            server: ProviderMachine;
            serverType: string;
            market?: string;
            attempts?: ProvisioningAttempt[];
          } = { server, serverType: machineType, market: config.capacityMarket };
          if (attempts.length > 0) result.attempts = attempts;
          return result;
        } catch (error) {
          const message = errorMessage(error);
          failures.push(`${zone}/${machineType}: ${message}`);
          attempts.push({
            region: zone,
            serverType: machineType,
            market: config.capacityMarket,
            category: isFallbackProvisioningError(message) ? "capacity" : "fatal",
            message,
          });
          if (!isFallbackProvisioningError(message)) {
            throw new Error(failures.join("; "), { cause: error });
          }
        }
      }
    }
    if (config.capacityMarket === "spot" && config.capacityFallback.startsWith("on-demand")) {
      for (const zone of zones) {
        const client =
          zone === this.zone && project === this.project
            ? this
            : new GCPClient(this.env, zone, project);
        for (const machineType of candidates) {
          try {
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
            );
            return {
              server,
              serverType: machineType,
              market: "on-demand",
              attempts,
            };
          } catch (error) {
            const message = errorMessage(error);
            failures.push(`on-demand ${zone}/${machineType}: ${message}`);
            if (!isFallbackProvisioningError(message)) {
              throw new Error(failures.join("; "), { cause: error });
            }
          }
        }
      }
    }
    throw new Error(failures.join("; "));
  }

  async createServer(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<ProviderMachine> {
    if (config.target !== "linux") {
      throw new Error("brokered gcp currently supports target=linux only");
    }
    await this.ensureFirewall(config);
    const name = leaseProviderName(leaseID, slug);
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
      disks: [
        {
          boot: true,
          autoDelete: true,
          type: "PERSISTENT",
          initializeParams: {
            sourceImage: config.gcpImage || this.image,
            diskSizeGb: config.gcpRootGB || this.rootGB,
            diskType: `zones/${this.zone}/diskTypes/pd-balanced`,
          },
        },
      ],
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
    try {
      const op = await this.gcp<GCPOperation>("POST", `/zones/${this.zone}/instances`, instance);
      await this.waitZoneOperation(op);
      return await this.getServer(name);
    } catch (error) {
      await this.deleteServer(name).catch(() => undefined);
      throw error;
    }
  }

  async getServer(name: string): Promise<ProviderMachine> {
    return toMachine(
      await this.gcp<GCPInstance>("GET", `/zones/${this.zone}/instances/${name}`),
      this.zone,
    );
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
      if (isNotFound(error)) return undefined;
      throw error;
    });
    if (op) await this.waitZoneOperation(op);
  }

  async deleteSSHKey(): Promise<void> {
    // GCP stores per-instance SSH metadata; nothing global to clean up.
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
    const op = await this.gcp<GCPOperation>("POST", "/global/firewalls", firewall);
    await this.waitGlobalOperation(op);
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
      throw new Error(`gcp ${method} ${path}: http ${response.status}: ${text}`);
    }
    return (text ? JSON.parse(text) : {}) as T;
  }

  private async accessToken(): Promise<string> {
    const now = Math.trunc(Date.now() / 1000);
    if (this.cache && this.cache.expiresAt - 60 > now) return this.cache.token;
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
    this.cache = { token: data.access_token, expiresAt: now + (data.expires_in ?? 3600) };
    return data.access_token;
  }

  private async waitZoneOperation(op: GCPOperation): Promise<void> {
    if (!op.name) return;
    for (;;) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- operation polling is sequential.
      const done = await this.gcp<GCPOperation>(
        "POST",
        `/zones/${this.zone}/operations/${op.name}/wait`,
      );
      operationError(done);
      if (operationDone(done)) return;
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

function toMachine(instance: GCPInstance, zone: string): ProviderMachine {
  const host =
    instance.networkInterfaces
      ?.flatMap((iface) => iface.accessConfigs ?? [])
      .find((cfg) => cfg.natIP)?.natIP ?? "";
  return {
    provider: "gcp",
    id: Number(instance.id ?? 0),
    cloudID: instance.name ?? "",
    region: zone,
    name: instance.name ?? "",
    status: instance.status ?? "",
    serverType: lastPathPart(instance.machineType ?? ""),
    host,
    labels: { ...instance.labels, zone },
  };
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

function gcpLabelValue(value: string): string {
  let out = value
    .trim()
    .toLowerCase()
    .replaceAll(/[^a-z0-9_-]/g, "_")
    .slice(0, 63)
    .replaceAll(/^[_-]+|[_-]+$/g, "");
  if (!out) out = "unknown";
  return out;
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

function operationError(op: GCPOperation): void {
  const errors = op.error?.errors ?? [];
  if (errors.length > 0) {
    throw new Error(
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

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
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
