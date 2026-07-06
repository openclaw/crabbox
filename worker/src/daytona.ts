import type { LeaseConfig } from "./config";
import { redactDiagnosticSecrets } from "./http";
import { leaseProviderLabels } from "./provider-labels";
import { leaseProviderName } from "./slug";
import type { Env, LeaseRecord, ProviderMachine } from "./types";

const defaultAPIURL = "https://app.daytona.io/api";
const defaultSSHGatewayHost = "ssh.app.daytona.io";
const defaultSSHAccessMinutes = 120;
const maxSSHAccessMinutes = 24 * 60;

interface DaytonaSandbox {
  id: string;
  name: string;
  snapshot?: string;
  user?: string;
  labels?: Record<string, string>;
  target?: string;
  state?: string;
}

interface DaytonaSandboxListResponse {
  items?: DaytonaSandbox[];
  nextCursor?: string | null;
}

interface DaytonaSSHAccess {
  token: string;
  expiresAt: string;
  sshCommand: string;
}

export interface DaytonaSSHEndpoint {
  user: string;
  host: string;
  port: string;
  expiresAt: string;
}

export class DaytonaHTTPError extends Error {
  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    readonly body: string,
  ) {
    super(`daytona ${method} ${path}: http ${status}: ${body}`);
    this.name = "DaytonaHTTPError";
  }
}

export class DaytonaClient {
  readonly snapshot: string;
  readonly target: string;
  readonly user: string;
  readonly workRoot: string;
  readonly sshGatewayHost: string;
  readonly sshAccessMinutes: number;
  fetcher: typeof fetch = (input, init) => fetch(input, init);
  pollDelayMs = 2_000;
  maxWaitMs = 5 * 60_000;

  private readonly apiURL: string;
  private readonly token: string;
  private readonly organizationID: string;

  constructor(private readonly env: Env) {
    this.token = env.DAYTONA_CRABBOX_KEY?.trim() ?? "";
    if (!this.token) {
      throw new Error("DAYTONA_CRABBOX_KEY secret is required");
    }
    this.apiURL = normalizeDaytonaAPIURL(env.CRABBOX_DAYTONA_API_URL);
    this.organizationID = env.CRABBOX_DAYTONA_ORGANIZATION_ID?.trim() ?? "";
    this.snapshot = env.CRABBOX_DAYTONA_SNAPSHOT?.trim() ?? "";
    this.target = env.CRABBOX_DAYTONA_TARGET?.trim() ?? "";
    this.user = env.CRABBOX_DAYTONA_USER?.trim() || "daytona";
    this.workRoot = env.CRABBOX_DAYTONA_WORK_ROOT?.trim() || "/home/daytona/crabbox";
    this.sshGatewayHost = env.CRABBOX_DAYTONA_SSH_GATEWAY_HOST?.trim() || defaultSSHGatewayHost;
    this.sshAccessMinutes = integerFromEnv(
      env.CRABBOX_DAYTONA_SSH_ACCESS_MINUTES,
      defaultSSHAccessMinutes,
      1,
      maxSSHAccessMinutes,
    );
  }

  async listCrabboxServers(): Promise<ProviderMachine[]> {
    const labels = JSON.stringify({ crabbox: "true" });
    const servers: ProviderMachine[] = [];
    let cursor = "";
    do {
      const query = new URLSearchParams({ labels, limit: "100" });
      if (cursor) query.set("cursor", cursor);
      // oxlint-disable-next-line eslint/no-await-in-loop -- each page supplies the next cursor.
      const page = await this.request<DaytonaSandboxListResponse>(
        "GET",
        `/sandbox?${query.toString()}`,
      );
      servers.push(...(page.items ?? []).map(daytonaMachine));
      cursor = page.nextCursor?.trim() ?? "";
    } while (cursor);
    return servers;
  }

  async findServerByLease(leaseID: string): Promise<ProviderMachine | undefined> {
    const matches = (await this.listCrabboxServers()).filter(
      (server) => server.labels["lease"] === leaseID,
    );
    if (matches.length > 1) {
      throw new Error(`ambiguous Daytona recovery for ${leaseID}: ${matches.length} sandboxes`);
    }
    return matches[0];
  }

  async getServer(id: string): Promise<ProviderMachine> {
    return daytonaMachine(
      await this.request<DaytonaSandbox>("GET", `/sandbox/${encodeURIComponent(id)}?verbose=true`),
    );
  }

  async createServer(
    config: LeaseConfig,
    leaseID: string,
    slug: string,
    owner: string,
  ): Promise<ProviderMachine> {
    const now = new Date();
    const labels = leaseProviderLabels(config, leaseID, slug, owner, "daytona", now, {
      lease_name: leaseProviderName(leaseID, slug),
      work_root: this.workRoot,
    });
    const body: Record<string, unknown> = {
      name: leaseProviderName(leaseID, slug),
      user: this.user,
      labels,
      autoStopInterval: 0,
      autoDeleteInterval: -1,
    };
    if (this.snapshot) body["snapshot"] = this.snapshot;
    if (this.target) body["target"] = this.target;
    const sandbox = await this.request<DaytonaSandbox>("POST", "/sandbox", body);
    return daytonaMachine(sandbox);
  }

  async waitForStarted(id: string): Promise<ProviderMachine> {
    const deadline = Date.now() + this.maxWaitMs;
    let lastState = "";
    while (Date.now() <= deadline) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- readiness polling is intentionally sequential.
      const server = await this.getServer(id);
      lastState = server.status;
      if (daytonaReadyState(lastState)) return server;
      if (daytonaTerminalState(lastState)) {
        throw new Error(`daytona sandbox ${id} entered terminal state=${lastState}`);
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- delay between sequential readiness probes.
      await new Promise((resolve) => setTimeout(resolve, this.pollDelayMs));
    }
    throw new Error(
      `timed out waiting for daytona sandbox ${id} (state=${lastState || "unknown"})`,
    );
  }

  async deleteServer(id: string): Promise<void> {
    await this.request<unknown>("DELETE", `/sandbox/${encodeURIComponent(id)}`);
  }

  async createSSHAccess(
    id: string,
    lease?: Pick<LeaseRecord, "expiresAt">,
  ): Promise<DaytonaSSHEndpoint> {
    const minutes = lease
      ? daytonaSSHAccessMinutesForLease(lease, this.sshAccessMinutes)
      : this.sshAccessMinutes;
    const query = new URLSearchParams({ expiresInMinutes: String(minutes) });
    const access = await this.request<DaytonaSSHAccess>(
      "POST",
      `/sandbox/${encodeURIComponent(id)}/ssh-access?${query.toString()}`,
    );
    return daytonaSSHEndpoint(access, this.sshGatewayHost);
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers = new Headers({
      accept: "application/json",
      authorization: `Bearer ${this.token}`,
    });
    if (this.organizationID) {
      headers.set("x-daytona-organization-id", this.organizationID);
    }
    if (body !== undefined) {
      headers.set("content-type", "application/json");
    }
    const init: RequestInit = {
      method,
      headers,
      redirect: "manual",
    };
    if (body !== undefined) init.body = JSON.stringify(body);
    const response = await this.fetcher(`${this.apiURL}${path}`, init);
    if (!response.ok) {
      const responseBody = redactDiagnosticSecrets((await response.text()).slice(0, 4_096), [
        this.token,
      ]);
      throw new DaytonaHTTPError(method, path, response.status, responseBody);
    }
    const responseBody = await response.text();
    return responseBody.trim() ? (JSON.parse(responseBody) as T) : (undefined as T);
  }
}

export function daytonaSSHEndpoint(
  access: Pick<DaytonaSSHAccess, "token" | "expiresAt" | "sshCommand">,
  fallbackHost = defaultSSHGatewayHost,
): DaytonaSSHEndpoint {
  let user = access.token.trim();
  let host = fallbackHost;
  let port = "22";
  const fields = access.sshCommand.trim().split(/\s+/).filter(Boolean);
  if (fields[0] === "ssh") fields.shift();
  for (let index = 0; index < fields.length; index += 1) {
    const field = fields[index]!;
    if (field === "-p") {
      port = fields[index + 1]?.trim() || port;
      index += 1;
      continue;
    }
    if (field.startsWith("-")) {
      if (field === "-o" || field === "-i" || field === "-F" || field === "-J") index += 1;
      continue;
    }
    const separator = field.lastIndexOf("@");
    if (separator > 0 && separator < field.length - 1) {
      user = field.slice(0, separator);
      host = field.slice(separator + 1);
    }
  }
  if (!user) throw new Error("daytona ssh access response missing token");
  if (!host) throw new Error("daytona ssh access response missing host");
  return { user, host, port, expiresAt: access.expiresAt };
}

export function daytonaAccessNeedsRefresh(
  lease: Pick<LeaseRecord, "providerAccessExpiresAt">,
  now = Date.now(),
): boolean {
  const expiresAt = Date.parse(lease.providerAccessExpiresAt ?? "");
  return !Number.isFinite(expiresAt) || expiresAt <= now + 10 * 60_000;
}

export function isDaytonaNotFound(error: unknown): boolean {
  return error instanceof DaytonaHTTPError && error.status === 404;
}

function daytonaMachine(sandbox: DaytonaSandbox): ProviderMachine {
  return {
    provider: "daytona",
    id: stableMachineID(sandbox.id),
    cloudID: sandbox.id,
    name: sandbox.name || sandbox.id,
    status: sandbox.state ?? "unknown",
    serverType: sandbox.snapshot?.trim() || "default",
    host: "",
    labels: sandbox.labels ?? {},
    ...(sandbox.target?.trim() ? { region: sandbox.target.trim() } : {}),
  };
}

function stableMachineID(value: string): number {
  let hash = 2_166_136_261;
  for (const byte of new TextEncoder().encode(value)) {
    hash ^= byte;
    hash = Math.imul(hash, 16_777_619);
  }
  return hash >>> 0;
}

function daytonaTerminalState(state: string): boolean {
  return [
    "error",
    "errored",
    "failed",
    "build_failed",
    "destroyed",
    "destroying",
    "deleted",
  ].includes(normalizeDaytonaState(state));
}

function daytonaReadyState(state: string): boolean {
  return ["started", "running", "ready", "active"].includes(normalizeDaytonaState(state));
}

function normalizeDaytonaState(state: string): string {
  return state.trim().toLowerCase();
}

function daytonaSSHAccessMinutesForLease(
  lease: Pick<LeaseRecord, "expiresAt">,
  configuredMinutes: number,
): number {
  const remainingMinutes = Math.ceil((Date.parse(lease.expiresAt) - Date.now()) / 60_000) + 5;
  return Math.min(
    maxSSHAccessMinutes,
    Math.max(configuredMinutes, Number.isFinite(remainingMinutes) ? remainingMinutes : 0),
  );
}

function normalizeDaytonaAPIURL(value: string | undefined): string {
  const configured = value?.trim() || defaultAPIURL;
  const url = new URL(configured);
  const local = url.hostname === "localhost" || url.hostname === "127.0.0.1";
  if (url.protocol !== "https:" && !(local && url.protocol === "http:")) {
    throw new Error("CRABBOX_DAYTONA_API_URL must use https");
  }
  if (url.username || url.password || url.search || url.hash) {
    throw new Error("CRABBOX_DAYTONA_API_URL must not contain credentials, query, or fragment");
  }
  return url.toString().replace(/\/+$/, "");
}

function integerFromEnv(
  value: string | undefined,
  fallback: number,
  minimum: number,
  maximum: number,
): number {
  const parsed = Number.parseInt(value?.trim() ?? "", 10);
  return Number.isFinite(parsed) ? Math.min(maximum, Math.max(minimum, parsed)) : fallback;
}
