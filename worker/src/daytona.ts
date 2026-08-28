import { sha256Hex } from "./auth";
import type { LeaseConfig } from "./config";
import { redactDiagnosticSecrets } from "./http";
import { leaseProviderLabels, providerMachineOwnedByLease } from "./provider-labels";
import {
  ProviderResourceUnresolvedError,
  validateProviderProvisioningCleanupClaim,
} from "./provider-provisioning";
import { leaseProviderName } from "./slug";
import type { Env, LeaseRecord, ProviderMachine } from "./types";

const defaultAPIURL = "https://app.daytona.io/api";
const defaultSSHGatewayHost = "ssh.app.daytona.io";
const defaultSSHAccessMinutes = 120;
const maxSSHAccessMinutes = 24 * 60;

type DaytonaLeaseIdentity = Pick<
  LeaseRecord,
  | "id"
  | "slug"
  | "provider"
  | "owner"
  | "providerOwner"
  | "workspaceID"
  | "cloudID"
  | "providerScope"
>;

interface DaytonaSandbox {
  id: string;
  name: string;
  snapshot?: string;
  user?: string;
  labels?: Record<string, string>;
  target?: string;
  state?: string;
  autoDestroyAt?: string;
  cpu?: number;
  memory?: number;
  disk?: number;
}

interface DaytonaSnapshot {
  id: string;
  name: string;
  state?: string;
  errorReason?: string | null;
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

export interface DaytonaSnapshotBootstrap {
  sourceSnapshot: string;
  sourceCPU: number;
  sourceMemoryGiB: number;
  sourceDiskGiB: number;
  snapshot: string;
  cpu: number;
  memoryGiB: number;
  diskGiB: number;
  sandboxID: string;
  cleanup: "deleted";
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
  snapshotWaitMs = 20 * 60_000;
  snapshotAcceptanceWaitMs = 10_000;

  private readonly apiURL: string;
  private readonly token: string;
  private readonly organizationID: string;
  private scopeValue?: Promise<string>;

  constructor(env: Env) {
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

  providerScope(): Promise<string> {
    // Bind the immutable request context without retaining credentials. Key rotation
    // needs original-scope resolution, not silent adoption into the replacement account.
    this.scopeValue ??= sha256Hex(
      JSON.stringify(["crabbox-daytona-context-v1", this.apiURL, this.organizationID, this.token]),
    ).then((digest) => `daytona:context:v1:${digest}`);
    return this.scopeValue;
  }

  async getOwnedServer(lease: DaytonaLeaseIdentity): Promise<ProviderMachine> {
    const claim = lease.providerScope
      ? validateProviderProvisioningCleanupClaim(
          { provider: "daytona", cloudID: lease.cloudID, providerScope: lease.providerScope },
          "daytona",
        )
      : undefined;
    if (!claim || claim.providerScope !== (await this.providerScope())) {
      throw new ProviderResourceUnresolvedError(
        `Daytona cleanup unresolved for lease ${lease.id}: an authoritative sandbox UUID and its original API, organization, and credential context are required; no provider mutation attempted`,
      );
    }
    const server = await this.getServer(claim.cloudID);
    if (!providerMachineOwnedByLease(server, lease, "daytona")) {
      throw new ProviderResourceUnresolvedError(
        `Daytona cleanup unresolved for lease ${lease.id}: sandbox ${claim.cloudID} ownership does not match; no provider mutation attempted`,
      );
    }
    return server;
  }

  async deleteOwnedServer(lease: DaytonaLeaseIdentity): Promise<void> {
    await this.deleteSandboxAndWait(lease.cloudID, () => this.getOwnedServer(lease));
  }

  async getServer(id: string): Promise<ProviderMachine> {
    return daytonaMachine(await this.getSandbox(id));
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
      // Creation-relative wall-clock TTL also applies to kept sandboxes. Sending it
      // with allocation bounds lifetime even when no response UUID reaches the coordinator.
      ttlMinutes: Math.max(1, Math.ceil(config.ttlSeconds / 60)),
      autoStopInterval: 0,
      autoDeleteInterval: -1,
    };
    if (this.snapshot) body["snapshot"] = this.snapshot;
    if (this.target) body["target"] = this.target;
    const sandbox = await this.request<DaytonaSandbox>("POST", "/sandbox", body);
    return daytonaMachine(sandbox);
  }

  async waitForStarted(id: string, ttlSeconds: number): Promise<ProviderMachine> {
    const startedAt = Date.now();
    const deadline = startedAt + this.maxWaitMs;
    const latestAutoDestroyAt = startedAt + Math.max(1, Math.ceil(ttlSeconds / 60)) * 60_000;
    let lastState = "";
    while (Date.now() <= deadline) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- readiness polling is intentionally sequential.
      const sandbox = await this.getSandbox(id);
      lastState = sandbox.state ?? "unknown";
      if (daytonaReadyState(lastState)) {
        const autoDestroyAt = Date.parse(sandbox.autoDestroyAt ?? "");
        // Creation and TTL anchoring need not share a clock tick. Verify the
        // remaining native bound without extending it on each readiness poll;
        // older APIs that ignore TTL must still fail before access is published.
        if (
          !Number.isFinite(autoDestroyAt) ||
          autoDestroyAt <= Date.now() ||
          autoDestroyAt > latestAutoDestroyAt
        ) {
          throw new Error(`Daytona sandbox ${id} did not confirm a valid native TTL`);
        }
        return daytonaMachine(sandbox);
      }
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
    try {
      await this.request<unknown>("DELETE", `/sandbox/${encodeURIComponent(id)}`);
    } catch (error) {
      // A prior cleanup or Daytona-side expiry already reached the desired state.
      // Treating 404 as success keeps release and rollback retries idempotent.
      if (!isDaytonaNotFound(error)) throw error;
    }
  }

  async bootstrapSnapshot(
    name: string,
    cpu: number,
    memoryGiB: number,
    diskGiB: number,
    baseImage: string,
  ): Promise<DaytonaSnapshotBootstrap> {
    await this.assertSnapshotAbsent(name);

    const body: Record<string, unknown> = {
      name: `crabbox-snapshot-bootstrap-${crypto.randomUUID().slice(0, 8)}`,
      user: this.user,
      labels: {
        created_by: "crabbox",
        purpose: "snapshot-bootstrap",
        snapshot_name: name,
      },
      // The normal cleanup path deletes immediately. If that request is lost,
      // Daytona stops an idle builder after 30 minutes and deletes it after it
      // remains stopped for another 60 minutes.
      autoStopInterval: 30,
      autoDeleteInterval: 60,
      buildInfo: {
        dockerfileContent: `FROM ${baseImage}`,
      },
      cpu,
      memory: memoryGiB,
      disk: diskGiB,
    };
    if (this.target) body["target"] = this.target;

    let sandboxID = "";
    let snapshotRequested = false;
    let snapshotRequestConfirmed = false;
    let result: Omit<DaytonaSnapshotBootstrap, "cleanup"> | undefined;
    let operationError: unknown;
    try {
      const created = await this.request<DaytonaSandbox>("POST", "/sandbox", body);
      sandboxID = created.id?.trim() ?? "";
      if (!sandboxID) {
        throw new Error("daytona snapshot bootstrap returned no sandbox id");
      }
      await this.waitForState(sandboxID, ["started", "running", "ready", "active"]);
      const built = await this.getSandbox(sandboxID);
      if (built.cpu !== cpu) {
        throw new Error(
          `daytona sandbox ${sandboxID} has ${built.cpu ?? "unknown"} CPU after ${cpu} CPU image build`,
        );
      }
      if (built.memory !== memoryGiB) {
        throw new Error(
          `daytona sandbox ${sandboxID} has ${built.memory ?? "unknown"} GiB memory after ${memoryGiB} GiB image build`,
        );
      }
      if (built.disk !== diskGiB) {
        throw new Error(
          `daytona sandbox ${sandboxID} disk is ${built.disk ?? "unknown"} GiB after ${diskGiB} GiB image build`,
        );
      }
      await this.request<DaytonaSandbox>("POST", `/sandbox/${encodeURIComponent(sandboxID)}/stop`);
      await this.waitForState(sandboxID, ["stopped"]);
      snapshotRequested = true;
      await this.request<DaytonaSandbox>(
        "POST",
        `/sandbox/${encodeURIComponent(sandboxID)}/snapshot`,
        { name },
      );
      snapshotRequestConfirmed = true;
      // Daytona persists snapshot resources from this already verified source
      // sandbox, so the active snapshot state is the authoritative completion signal.
      await this.waitForSnapshotActive(name);
      result = {
        sourceSnapshot: baseImage,
        sourceCPU: built.cpu,
        sourceMemoryGiB: built.memory,
        sourceDiskGiB: built.disk,
        snapshot: name,
        cpu: built.cpu,
        memoryGiB: built.memory,
        diskGiB: built.disk,
        sandboxID,
      };
    } catch (error) {
      operationError = error;
    }

    try {
      if (sandboxID) {
        let transitionError: unknown;
        if (snapshotRequested) {
          try {
            await this.waitForSnapshotTransition(sandboxID, !snapshotRequestConfirmed);
          } catch (error) {
            transitionError = error;
          }
        }
        try {
          await this.deleteSandboxAndWait(sandboxID);
        } catch (deleteError) {
          if (!transitionError) throw deleteError;
          const transitionMessage =
            transitionError instanceof Error ? transitionError.message : String(transitionError);
          const deleteMessage =
            deleteError instanceof Error ? deleteError.message : String(deleteError);
          throw new Error(
            `daytona sandbox ${sandboxID} snapshot transition observation failed: ${transitionMessage}; deletion also failed: ${deleteMessage}`,
            { cause: deleteError },
          );
        }
      }
    } catch (cleanupError) {
      if (operationError) {
        const operationMessage =
          operationError instanceof Error ? operationError.message : String(operationError);
        const cleanupMessage =
          cleanupError instanceof Error ? cleanupError.message : String(cleanupError);
        throw new Error(
          `daytona snapshot bootstrap failed: ${operationMessage}; sandbox ${sandboxID} cleanup also failed: ${cleanupMessage}`,
          { cause: cleanupError },
        );
      }
      throw cleanupError;
    }
    if (operationError) throw operationError;
    if (!result) throw new Error("daytona snapshot bootstrap completed without a result");
    return { ...result, cleanup: "deleted" };
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

  private async getSandbox(id: string): Promise<DaytonaSandbox> {
    const sandbox = await this.request<DaytonaSandbox>(
      "GET",
      `/sandbox/${encodeURIComponent(id)}?verbose=true`,
    );
    if (sandbox.id !== id) {
      throw new ProviderResourceUnresolvedError(
        `Daytona sandbox identity mismatch for ${id}; refusing name-based substitution`,
      );
    }
    return sandbox;
  }

  private async getSnapshot(idOrName: string): Promise<DaytonaSnapshot> {
    return await this.request<DaytonaSnapshot>("GET", `/snapshots/${encodeURIComponent(idOrName)}`);
  }

  private async assertSnapshotAbsent(name: string): Promise<void> {
    try {
      const snapshot = await this.getSnapshot(name);
      throw new Error(
        `daytona snapshot ${name} already exists (state=${snapshot.state ?? "unknown"})`,
      );
    } catch (error) {
      if (!isDaytonaNotFound(error)) throw error;
    }
  }

  private async waitForSnapshotActive(name: string): Promise<DaytonaSnapshot> {
    const deadline = Date.now() + this.snapshotWaitMs;
    let lastState = "not_found";
    while (Date.now() <= deadline) {
      try {
        // Snapshot-from-sandbox is asynchronous; the row appears only after the
        // runner has persisted the image, so 404 remains a pending state here.
        // oxlint-disable-next-line eslint/no-await-in-loop -- each poll observes the latest provider state.
        const snapshot = await this.getSnapshot(name);
        lastState = snapshot.state ?? "";
        const normalized = normalizeDaytonaState(lastState);
        if (normalized === "active") return snapshot;
        if (daytonaSnapshotTerminalState(normalized)) {
          const reason = snapshot.errorReason?.trim();
          throw new Error(
            `daytona snapshot ${name} entered terminal state=${lastState || "unknown"}${reason ? `: ${reason}` : ""}`,
          );
        }
      } catch (error) {
        if (isDaytonaNotFound(error)) {
          lastState = "not_found";
        } else if (isDaytonaTransient(error)) {
          lastState =
            error instanceof DaytonaHTTPError ? `http_${error.status}` : "network_unavailable";
        } else {
          throw error;
        }
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- delay between sequential state probes.
      await new Promise((resolve) => setTimeout(resolve, this.pollDelayMs));
    }
    throw new Error(
      `timed out waiting for daytona snapshot ${name} to become active (state=${lastState || "unknown"})`,
    );
  }

  private async deleteSandboxAndWait(
    id: string,
    beforeDelete?: () => Promise<ProviderMachine>,
  ): Promise<void> {
    const deadline = Date.now() + this.maxWaitMs;
    let deleteRequested = false;
    let lastState = "";
    while (Date.now() <= deadline) {
      if (!deleteRequested) {
        try {
          // Recheck scoped ownership before every mutation, including after a conflict.
          // oxlint-disable-next-line eslint/no-await-in-loop -- each delete attempt needs current identity.
          const current = await beforeDelete?.();
          if (current && daytonaDeletedState(current.status)) return;
          const deletionPending =
            current && ["destroying", "deleting"].includes(normalizeDaytonaState(current.status));
          if (!deletionPending) {
            // DELETE may conflict with snapshotting. Revalidate before retrying,
            // but only observe a deletion already accepted by an earlier attempt.
            // oxlint-disable-next-line eslint/no-await-in-loop -- each retry observes provider state.
            await this.deleteServer(id);
          }
          deleteRequested = true;
        } catch (error) {
          if (isDaytonaNotFound(error)) return;
          if (!isDaytonaConflict(error)) throw error;
          lastState = "state_change_in_progress";
          // oxlint-disable-next-line eslint/no-await-in-loop -- delay between sequential cleanup retries.
          await new Promise((resolve) => setTimeout(resolve, this.pollDelayMs));
          continue;
        }
      }
      try {
        // DELETE only requests Daytona's asynchronous soft-delete transition.
        // Do not report cleanup until provider teardown reaches its terminal state.
        // oxlint-disable-next-line eslint/no-await-in-loop -- each poll observes the latest provider state.
        const sandbox = await this.getSandbox(id);
        lastState = sandbox.state ?? "";
        if (daytonaDeletedState(lastState)) return;
      } catch (error) {
        if (isDaytonaNotFound(error)) return;
        if (isDaytonaTransient(error)) {
          lastState =
            error instanceof DaytonaHTTPError ? `http_${error.status}` : "network_unavailable";
        } else {
          throw error;
        }
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- delay between sequential cleanup probes.
      await new Promise((resolve) => setTimeout(resolve, this.pollDelayMs));
    }
    throw new Error(
      `timed out waiting for daytona sandbox ${id} cleanup (state=${lastState || "unknown"})`,
    );
  }

  private async waitForSnapshotTransition(id: string, uncertainAcceptance: boolean): Promise<void> {
    const startedAt = Date.now();
    const deadline = startedAt + (uncertainAcceptance ? this.snapshotWaitMs : this.maxWaitMs);
    const acceptanceDeadline = startedAt + Math.min(this.snapshotAcceptanceWaitMs, this.maxWaitMs);
    let sawSnapshotting = false;
    let lastState = "";
    while (Date.now() <= deadline) {
      try {
        // Daytona persists the active snapshot before clearing the sandbox's
        // pending snapshot transition. DELETE is rejected until this state clears.
        // oxlint-disable-next-line eslint/no-await-in-loop -- each poll observes the latest provider state.
        const sandbox = await this.getSandbox(id);
        lastState = sandbox.state ?? "";
        if (normalizeDaytonaState(lastState) === "snapshotting") {
          sawSnapshotting = true;
        } else if (sawSnapshotting || !uncertainAcceptance || Date.now() >= acceptanceDeadline) {
          return;
        }
      } catch (error) {
        if (isDaytonaNotFound(error)) return;
        throw error;
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- delay between sequential state probes.
      await new Promise((resolve) => setTimeout(resolve, this.pollDelayMs));
    }
    throw new Error(
      `timed out waiting for daytona sandbox ${id} snapshot transition (state=${lastState || "unknown"})`,
    );
  }

  private async waitForState(id: string, expectedStates: string[]): Promise<DaytonaSandbox> {
    const expected = new Set(expectedStates.map(normalizeDaytonaState));
    const deadline = Date.now() + this.maxWaitMs;
    let lastState = "";
    while (Date.now() <= deadline) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- each poll observes the latest provider state.
      const sandbox = await this.getSandbox(id);
      lastState = sandbox.state ?? "";
      if (expected.has(normalizeDaytonaState(lastState))) return sandbox;
      if (daytonaTerminalState(lastState)) {
        throw new Error(`daytona sandbox ${id} entered terminal state=${lastState}`);
      }
      // oxlint-disable-next-line eslint/no-await-in-loop -- delay between sequential state probes.
      await new Promise((resolve) => setTimeout(resolve, this.pollDelayMs));
    }
    throw new Error(
      `timed out waiting for daytona sandbox ${id} state=${expectedStates.join("|")} (state=${lastState || "unknown"})`,
    );
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

function isDaytonaConflict(error: unknown): boolean {
  return error instanceof DaytonaHTTPError && error.status === 409;
}

function isDaytonaTransient(error: unknown): boolean {
  return (
    error instanceof TypeError ||
    (error instanceof DaytonaHTTPError && (error.status === 429 || error.status >= 500))
  );
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

function daytonaDeletedState(state: string): boolean {
  return ["destroyed", "deleted"].includes(normalizeDaytonaState(state));
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

function daytonaSnapshotTerminalState(state: string): boolean {
  return ["inactive", "error", "build_failed", "removing"].includes(normalizeDaytonaState(state));
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
