import { isAdminRequest } from "./auth";
import { clampLeaseDurations, resolveLeasePolicy } from "./config";
import type { CoordinatorRuntime, CoordinatorStorageView } from "./coordinator-runtime";
import { sha256Hex } from "./encoding";
import { json, requestOwner } from "./http";
import { completeLeaseProviderCleanup, leaseProviderCleanupConfirmed } from "./lease-cleanup";
import { putLeaseRecord } from "./lease-record-store";
import { requestOrg } from "./org-identity";
import { publicLeaseRecord } from "./org-records";
import { serveRelayBackend } from "./serve-relay";
import { ServeRelayControl } from "./serve-relay-control";
import { isHostRelayRequest } from "./ssh-host-relay";
import type { Env, LeaseRecord, LeaseRequest, CreateAttemptRecord } from "./types";
import { usageSummary, type UsageGroup } from "./usage";

export const serveBodyLimit = 32 * 1024;
const machinePrefix = "serve-machine:";
const onlineMS = 30_000;
const lostMS = 90_000;
const pruneMS = 7 * 24 * 60 * 60 * 1000;
const maxMachines = 64;
const maxLeases = 32;
const maxOwnerLiveLeases = 64;
// One shared alarm takes one short transaction, including pre-cap (128-lease) machines.
const maintenanceMachineBatch = 16;
const maintenanceLeaseReadBudget = 256;
const maintenanceResumeMS = 15_000;
const maintenanceCursorKey = "serve-maintenance-cursor";
export const serveUsageKey = (machineID: string, month: string): string =>
  `serve-usage:${machineID}:${month}`;
export interface ServeUsageRollup {
  owner: string;
  org: string;
  leases: number;
  runtimeSeconds: number;
  cpuSeconds: number;
  updatedAt: string;
}
interface MaintenanceCursor {
  after: string;
  next?: number;
}
const uuid = /^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$/;
const live = (l: LeaseRecord) => l.state === "active" || l.state === "provisioning";
export interface ServeAssignment {
  machineID: string;
  cpus: number;
  memoryMB: number;
  sshPublicKey: string;
  image: string;
}
export interface ServeAddressCandidate {
  address: string;
  kind: "tailnet" | "lan" | "explicit";
}
export interface ServeAvailability {
  state: "available" | "busy" | "unknown";
  reasons: string[];
  hostCPU: number | null;
  memoryPressure: "normal" | "warn" | "critical" | "unknown";
  userIdleSeconds: number | null;
  power: "ac" | "battery" | "unknown";
}
export interface ServeMachine {
  availability?: ServeAvailability;
  machineID: string;
  owner: string;
  org: string;
  name: string;
  os: string;
  arch: string;
  cpus: number;
  memoryMB: number;
  maxLeases: number;
  address: string;
  frontDoor: {
    port: number;
    hostKey: string;
    fingerprint?: string;
    candidates?: ServeAddressCandidate[];
    relay?: boolean;
  };
  runtime: string;
  runtimeVersion: string;
  agentVersion: string;
  image: string;
  labels: Record<string, string>;
  load: number;
  freeMemoryMB: number;
  draining: boolean;
  lastHeartbeat: number;
  leases: string[];
}
export interface ServeMachineView extends Omit<ServeMachine, "leases"> {
  usage?: {
    month: string;
    leases: number;
    activeLeases: number;
    runtimeSeconds: number;
    cpuSeconds: number;
  };
  leases?: undefined;
  online: boolean;
  freeCPUs: number;
  allocatedLeases: number;
  allocatedCPUs: number;
  allocatedMemoryMB: number;
  allocatedLeaseRefs: Pick<LeaseRecord, "id" | "slug" | "state">[];
}
interface Heartbeat extends Omit<ServeMachine, "owner" | "org" | "lastHeartbeat" | "leases"> {
  leases: { id: string; state: string }[];
}
class ServeError extends Error {
  constructor(
    readonly code: string,
    readonly status = 400,
    readonly details: unknown = undefined,
  ) {
    super(code);
  }
}
function validText(v: unknown, max = 128): v is string {
  return (
    typeof v === "string" &&
    v.length > 0 &&
    v.length <= max &&
    [...v].every((c) => c.charCodeAt(0) > 31 && c.charCodeAt(0) !== 127)
  );
}
function integer(v: unknown, max: number): v is number {
  return Number.isSafeInteger(v) && Number(v) > 0 && Number(v) <= max;
}
function validAvailability(value: unknown): boolean {
  if (value === undefined) return true; // Released agents retain their placement behavior.
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const v = value as ServeAvailability;
  return (
    ["available", "busy", "unknown"].includes(v.state) &&
    Array.isArray(v.reasons) &&
    v.reasons.length <= 8 &&
    v.reasons.every((reason) => validText(reason, 128)) &&
    (v.hostCPU === null || (Number.isFinite(v.hostCPU) && v.hostCPU >= 0 && v.hostCPU <= 1)) &&
    ["normal", "warn", "critical", "unknown"].includes(v.memoryPressure) &&
    (v.userIdleSeconds === null ||
      (Number.isFinite(v.userIdleSeconds) &&
        v.userIdleSeconds >= 0 &&
        v.userIdleSeconds <= Number.MAX_SAFE_INTEGER)) &&
    ["ac", "battery", "unknown"].includes(v.power)
  );
}
function validLeaseKey(v: unknown): v is string {
  if (!validText(v, 4096)) return false;
  const match = /^(ssh-ed25519|ssh-rsa) [A-Za-z0-9+/=]+(?: .*)?$/.exec(v);
  return Boolean(
    match && new TextEncoder().encode(v).length <= (match[1] === "ssh-ed25519" ? 256 : 4096),
  );
}
function validAddress(v: unknown): v is string {
  if (typeof v !== "string" || !/^(?:\d{1,3}\.){3}\d{1,3}$/.test(v)) return false;
  const octets = v.split(".").map(Number);
  return octets.every((n) => n <= 255) && octets[0] !== 0 && octets[0]! < 224;
}
function validCandidates(value: unknown, address: string): boolean {
  if (value === undefined) return true; // Released 0.75.0 heartbeat.
  if (!Array.isArray(value) || value.length < 1 || value.length > 4) return false;
  const seen = new Set<string>();
  return value.every((c, index) => {
    if (
      !c ||
      typeof c !== "object" ||
      typeof c.address !== "string" ||
      !/^(?:\d{1,3}\.){3}\d{1,3}$/.test(c.address) ||
      seen.has(c.address) ||
      (index === 0 && c.address !== address)
    )
      return false;
    // New candidates are canonical IPv4 literals; do not admit alternate spellings.
    const octets = c.address.split(".").map(Number);
    if (
      octets.some((n: number) => n > 255) ||
      octets.join(".") !== c.address ||
      octets.every((n: number) => n === 0) ||
      octets[0] === 127 ||
      (octets[0]! >= 224 && octets[0]! <= 239)
    )
      return false;
    seen.add(c.address);
    if (c.kind === "tailnet") return octets[0] === 100 && octets[1] >= 64 && octets[1] <= 127;
    if (c.kind === "lan")
      return (
        octets[0] === 10 ||
        (octets[0] === 172 && octets[1] >= 16 && octets[1] <= 31) ||
        (octets[0] === 192 && octets[1] === 168)
      );
    return c.kind === "explicit";
  });
}
function jumpForMachine(m: ServeMachine, env: Env): NonNullable<LeaseRecord["sshJump"]> {
  const candidates = m.frontDoor.candidates ?? [{ address: m.address, kind: "tailnet" }];
  return {
    host: candidates[0]!.address,
    port: m.frontDoor.port,
    hostKey: m.frontDoor.hostKey,
    fingerprint: m.frontDoor.fingerprint!,
    candidates: candidates.map((c) => ({ host: c.address, port: m.frontDoor.port })),
    ...(m.frontDoor.relay && serveRelayBackend(env) ? { relay: true } : {}),
  };
}
async function body<T>(r: Request): Promise<T> {
  const reader = r.body?.getReader();
  if (!reader) throw new ServeError("invalid_body");
  let size = 0;
  const chunks: Uint8Array[] = [];
  const read = async (): Promise<void> => {
    const { done, value } = await reader.read();
    if (done) return;
    size += value.length;
    if (size > serveBodyLimit) {
      await reader.cancel();
      throw new ServeError("body_too_large", 413);
    }
    chunks.push(value);
    return read();
  };
  try {
    await read();
  } finally {
    reader.releaseLock();
  }
  const all = new Uint8Array(size);
  let offset = 0;
  for (const c of chunks) {
    all.set(c, offset);
    offset += c.length;
  }
  try {
    return JSON.parse(new TextDecoder().decode(all)) as T;
  } catch {
    throw new ServeError("invalid_json");
  }
}
function heartbeatInput(v: Heartbeat): boolean {
  return Boolean(
    v &&
    uuid.test(v.machineID) &&
    validText(v.name) &&
    ["linux", "darwin"].includes(v.os) &&
    ["amd64", "arm64"].includes(v.arch) &&
    integer(v.cpus, 4096) &&
    integer(v.memoryMB, 16 * 1024 * 1024) &&
    integer(v.maxLeases, maxLeases) &&
    validAvailability(v.availability) &&
    (v.frontDoor?.candidates !== undefined || validAddress(v.address)) &&
    v.frontDoor &&
    integer(v.frontDoor.port, 65535) &&
    validCandidates(v.frontDoor.candidates, v.address) &&
    (v.frontDoor.relay === undefined || typeof v.frontDoor.relay === "boolean") &&
    validText(v.frontDoor.hostKey, 256) &&
    /^ssh-ed25519 [A-Za-z0-9+/=]+(?: |$)/.test(v.frontDoor.hostKey) &&
    ["docker", "podman"].includes(v.runtime) &&
    validText(v.runtimeVersion) &&
    validText(v.agentVersion) &&
    validText(v.image, 512) &&
    typeof v.draining === "boolean" &&
    Number.isFinite(v.load) &&
    v.load >= 0 &&
    Number.isFinite(v.freeMemoryMB) &&
    v.freeMemoryMB >= 0 &&
    v.labels &&
    typeof v.labels === "object" &&
    !Array.isArray(v.labels) &&
    Object.entries(v.labels).length <= 32 &&
    Object.entries(v.labels).every(([k, x]) => validText(k, 64) && validText(x, 128)) &&
    Array.isArray(v.leases) &&
    v.leases.length <= maxLeases &&
    new Set(v.leases.map((l) => l.id)).size === v.leases.length &&
    v.leases.every(
      (l) =>
        l &&
        typeof l === "object" &&
        /^cbx_[a-f0-9]{12}$/.test(l.id) &&
        ["running", "starting"].includes(l.state),
    ),
  );
}
export function serveSize(
  machine: Pick<ServeMachine, "cpus" | "memoryMB">,
  className: string,
  type?: string,
): { cpus: number; memoryMB: number } {
  if (type) {
    const m = /^cpu([1-9][0-9]{0,3})$/.exec(type);
    if (!m) throw new ServeError("invalid_serve_type");
    return { cpus: Number(m[1]), memoryMB: Number(m[1]) * 2048 };
  }
  if (className === "beast") return { cpus: machine.cpus, memoryMB: machine.memoryMB };
  const cpus = ({ standard: 8, fast: 16, large: 32 } as Record<string, number>)[className];
  if (!cpus) throw new ServeError("invalid_serve_class");
  return { cpus, memoryMB: cpus * 2048 };
}
export async function hostKeyFingerprint(key: string): Promise<string> {
  try {
    const encoded = key.split(/\s+/)[1]!;
    const bytes = Uint8Array.from(atob(encoded), (c) => c.charCodeAt(0));
    const view = new DataView(bytes.buffer);
    if (
      bytes.length !== 51 ||
      view.getUint32(0) !== 11 ||
      new TextDecoder().decode(bytes.slice(4, 15)) !== "ssh-ed25519" ||
      view.getUint32(15) !== 32
    )
      throw new Error("invalid");
    const hash = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
    return "SHA256:" + btoa(String.fromCharCode(...hash)).replace(/=+$/u, "");
  } catch {
    throw new ServeError("invalid_serve_host_key");
  }
}
export function serveEnabled(env: Env, owner?: string): boolean {
  const allowed = (env.CRABBOX_SERVE_ALLOWED_OWNERS ?? "")
    .split(",")
    .map((entry) => entry.trim())
    .filter(Boolean);
  return owner === undefined
    ? allowed.length > 0
    : allowed.includes("*") || allowed.includes(owner);
}
export function serveDisabledResponse(env: Env, owner: string): Response | undefined {
  if (!serveEnabled(env, owner)) return json({ error: "serve_not_enabled" }, { status: 403 });
  return undefined;
}
export class ServeCoordinator {
  constructor(
    private readonly runtime: CoordinatorRuntime,
    private readonly env: Env,
  ) {}
  private commit<T>(f: (s: CoordinatorStorageView) => Promise<T>): Promise<T> {
    return this.runtime.runExclusive(() =>
      this.runtime.provisioning
        ? this.runtime.provisioning.commitAndWake(f)
        : this.runtime.storage.transaction(f),
    );
  }
  private owned(m: ServeMachine, r: Request): boolean {
    return m.owner === requestOwner(r) && m.org === requestOrg(r, this.env);
  }
  private async ownerPrefix(owner: string, org: string): Promise<string> {
    return `serve-owner:${await sha256Hex(JSON.stringify([owner, org]))}:`;
  }
  private async machines(
    s: CoordinatorStorageView,
    r: Request,
  ): Promise<Map<string, ServeMachine>> {
    return s.list<ServeMachine>({
      prefix: await this.ownerPrefix(requestOwner(r), requestOrg(r, this.env)),
      limit: maxMachines,
    });
  }
  private async putMachine(s: CoordinatorStorageView, m: ServeMachine): Promise<void> {
    await s.put(machinePrefix + m.machineID, m);
    await s.put((await this.ownerPrefix(m.owner, m.org)) + m.machineID, m);
  }
  async listMachines(r: Request): Promise<ServeMachineView[]> {
    if (!serveEnabled(this.env, requestOwner(r))) return [];
    const now = new Date();
    const month = now.toISOString().slice(0, 7);
    // Read one snapshot so cleanup cannot move a lease into the rollup between reads.
    return this.runtime.storage.transaction(async (s) => {
      const machines = await this.machines(s, r);
      return Promise.all(
        [...machines.values()]
          .filter((m) => this.owned(m, r))
          .map(async (m) => {
            const [records, stored] = await Promise.all([
              this.records(s, m),
              s.get<ServeUsageRollup>(serveUsageKey(m.machineID, month)),
            ]);
            const settled = stored?.owner === m.owner && stored.org === m.org ? stored : undefined;
            const pending = usageSummary(
              records.filter((l) => !l.serveUsageRecordedAt),
              { scope: "user", owner: m.owner, org: m.org, month },
              now,
            ).byMachine[0];
            return {
              ...(await this.view(s, m, records)),
              usage: {
                month,
                leases: (settled?.leases ?? 0) + (pending?.leases ?? 0),
                activeLeases: pending?.activeLeases ?? 0,
                runtimeSeconds: (settled?.runtimeSeconds ?? 0) + (pending?.runtimeSeconds ?? 0),
                cpuSeconds: (settled?.cpuSeconds ?? 0) + (pending?.cpuSeconds ?? 0),
              },
            };
          }),
      );
    });
  }
  async labelUsageGroups(groups: UsageGroup[], r: Request): Promise<void> {
    for (const group of groups) {
      if (group.key === "unknown") continue;
      // oxlint-disable-next-line eslint/no-await-in-loop -- bound name hydration to one point read at a time, after usage filtering.
      const machine = await this.runtime.storage.get<ServeMachine>(machinePrefix + group.key);
      if (machine?.name && (isAdminRequest(r) || this.owned(machine, r))) {
        group.label = machine.name;
      }
    }
  }
  private async recordUsage(s: CoordinatorStorageView, l: LeaseRecord, now: number): Promise<void> {
    if (live(l) || l.serveUsageRecordedAt || !leaseProviderCleanupConfirmed(l)) return;
    const month = l.createdAt.slice(0, 7);
    const group = usageSummary([l], { scope: "all", month }, new Date(now)).byMachine[0];
    if (!group) return;
    const key = serveUsageKey(group.key, month);
    const stored = await s.get<ServeUsageRollup>(key);
    const previous = stored?.owner === l.owner && stored.org === l.org ? stored : undefined;
    const updatedAt = new Date(now).toISOString();
    await s.put(key, {
      owner: l.owner,
      org: l.org,
      leases: (previous?.leases ?? 0) + group.leases,
      runtimeSeconds: (previous?.runtimeSeconds ?? 0) + group.runtimeSeconds,
      cpuSeconds: (previous?.cpuSeconds ?? 0) + (group.cpuSeconds ?? 0),
      updatedAt,
    } satisfies ServeUsageRollup);
    // The caller persists this marker with the rollup in the heartbeat transaction.
    l.serveUsageRecordedAt = updatedAt;
  }
  async route(r: Request, parts: string[]): Promise<Response> {
    try {
      const disabled = serveDisabledResponse(this.env, requestOwner(r));
      if (disabled && !(await isHostRelayRequest(this.runtime.storage, parts)))
        return parts[2] === "relay" || parts[4] === "relay"
          ? json({ error: "serve_relay_not_allowed" }, { status: 403 })
          : disabled;
      if (parts.length === 3 && parts[2] === "machines" && r.method === "GET") {
        return json({ machines: await this.listMachines(r) });
      }
      if (parts.length === 3 && parts[2] === "heartbeat" && r.method === "POST")
        return await this.heartbeat(r, await body<Heartbeat>(r));
      if (
        parts.length === 5 &&
        parts[2] === "leases" &&
        parts[4] === "relay" &&
        r.method === "POST"
      ) {
        await body<Record<string, unknown>>(r);
        return this.commit((s) => new ServeRelayControl(this.env).mint(s, r, parts[3]!));
      }
      if (parts.length === 5 && parts[2] === "relay" && r.method === "GET")
        return this.commit((s) =>
          new ServeRelayControl(this.env).authorize(s, r, parts[3]!, parts[4]!),
        );
      if (parts.length === 5 && parts[2] === "leases" && r.method === "POST")
        return await this.report(r, parts[3]!, parts[4]!, await body<Record<string, unknown>>(r));
      return json({ error: "not_found" }, { status: 404 });
    } catch (e) {
      if (e instanceof ServeError)
        return json({ error: e.code, details: e.details }, { status: e.status });
      throw e;
    }
  }
  private async records(s: CoordinatorStorageView, m: ServeMachine): Promise<LeaseRecord[]> {
    return (await Promise.all(m.leases.map((id) => s.get<LeaseRecord>(`lease:${id}`)))).filter(
      (l): l is LeaseRecord =>
        Boolean(l && l.provider === "serve" && l.serve?.machineID === m.machineID),
    );
  }
  private async view(
    s: CoordinatorStorageView,
    m: ServeMachine,
    leases?: LeaseRecord[],
  ): Promise<ServeMachineView> {
    leases ??= await this.records(s, m);
    const held = leases.filter((l) => !leaseProviderCleanupConfirmed(l));
    const cpus = held.reduce((n, l) => n + l.serve!.cpus, 0);
    const memory = held.reduce((n, l) => n + l.serve!.memoryMB, 0);
    const available = m.availability === undefined || m.availability.state === "available";
    return {
      ...m,
      leases: undefined,
      online: Date.now() - m.lastHeartbeat < onlineMS,
      freeCPUs: available ? Math.max(0, m.cpus - cpus) : 0,
      freeMemoryMB: available ? Math.max(0, m.memoryMB - memory) : 0,
      allocatedLeases: held.length,
      allocatedCPUs: cpus,
      allocatedMemoryMB: memory,
      allocatedLeaseRefs: held
        .filter((l) => l.owner === m.owner && l.org === m.org)
        .map(({ id, slug, state }) => ({ id, state, ...(slug === undefined ? {} : { slug }) })),
    };
  }
  private async heartbeat(r: Request, input: Heartbeat): Promise<Response> {
    if (requestOwner(r) === "unknown" || !requestOwner(r))
      throw new ServeError("serve_owner_required", 403);
    if (!heartbeatInput(input)) throw new ServeError("invalid_serve_heartbeat");
    const fingerprint = await hostKeyFingerprint(input.frontDoor.hostKey);
    return this.commit(async (s) => {
      const key = machinePrefix + input["machineID"];
      const old = await s.get<ServeMachine>(key);
      const now = Date.now();
      if (old && !this.owned(old, r)) throw new ServeError("machine_owner_mismatch", 403);
      if (old && now - old.lastHeartbeat < 500 && !(input.draining && !old.draining))
        throw new ServeError("serve_rate_limited", 429);
      if (!old && (await this.machines(s, r)).size >= maxMachines)
        throw new ServeError("serve_machine_limit", 409);
      const m: ServeMachine = {
        ...input,
        frontDoor: {
          port: input.frontDoor.port,
          hostKey: input.frontDoor.hostKey,
          fingerprint,
          relay: input.frontDoor.relay === true,
          candidates: input.frontDoor.candidates?.map(({ address, kind }) => ({
            address,
            kind,
          })) ?? [{ address: input.address, kind: "tailnet" }],
        },
        owner: requestOwner(r),
        org: requestOrg(r, this.env),
        lastHeartbeat: now,
        leases: old?.leases ?? [],
      };
      const records = await this.records(s, m);
      const seen = new Set(input.leases.map((l) => l.id));
      const assignments: ReturnType<ServeCoordinator["assignment"]>[] = [];
      const stop = new Set<string>();
      // oxlint-disable eslint/no-await-in-loop -- serialize rollup increments for leases on the same machine and month.
      for (const l of records) {
        const before = JSON.stringify(l);
        this.expire(l, old ?? m, now);
        if (live(l) && l.state === "active") {
          l.sshJump = jumpForMachine(m, this.env);
        }
        if (!live(l) && !seen.has(l.id) && !leaseProviderCleanupConfirmed(l)) {
          delete l.cleanupStartedAt;
          completeLeaseProviderCleanup(l, new Date(now).toISOString());
        }
        if (live(l)) {
          if (l.state === "active" && !seen.has(l.id)) {
            l.state = "failed";
            l.failureError = "served container disappeared";
            delete l.cleanupStartedAt;
            completeLeaseProviderCleanup(l, new Date(now).toISOString());
          }
        } else if (seen.has(l.id)) stop.add(l.id);
        if (live(l)) assignments.push(this.assignment(l));
        if (!live(l)) await new ServeRelayControl(this.env).closeLease(l);
        await this.recordUsage(s, l, now);
        if (JSON.stringify(l) !== before) await putLeaseRecord(s, l);
      }
      // oxlint-enable eslint/no-await-in-loop
      for (const id of seen) if (!records.some((l) => l.id === id && live(l))) stop.add(id);
      // Terminal rows are kept in normal history; the machine only retains live allocations.
      m.leases = records.filter((l) => !leaseProviderCleanupConfirmed(l)).map((l) => l.id);
      await this.putMachine(s, m);
      return json({
        assignments: assignments.filter((a) => !stop.has(a.id)),
        stop: [...stop],
        serverTime: now,
        relays: await new ServeRelayControl(this.env).pending(s, m.owner, m.org, m.machineID),
      });
    });
  }
  private assignment(l: LeaseRecord) {
    return {
      id: l.id,
      ...l.serve,
      expiresAt: l.expiresAt,
      idleDeadline: idleDeadline(l),
      state: l.state,
    };
  }
  private expire(l: LeaseRecord, m: ServeMachine, now: number) {
    if (!live(l)) return;
    l.leasePolicy = { ...resolveLeasePolicy(this.env, l.owner, l.org), runtimeExtension: true };
    l.expiresAt = new Date(
      Math.min(
        Date.parse(l.expiresAt),
        Date.parse(l.createdAt) + Math.min(l.ttlSeconds, l.leasePolicy.maxTTLSeconds) * 1000,
      ),
    ).toISOString();
    if (now - m.lastHeartbeat >= lostMS) {
      l.state = "failed";
      l.failureError = "serve_machine_lost";
    } else if (Math.min(Date.parse(l.expiresAt), idleDeadline(l)) <= now) {
      l.state = "expired";
      l.failureError = "serve deadline elapsed";
    } else return;
    l.releaseDeletesServer = true;
    l.updatedAt = new Date(now).toISOString();
    l.endedAt = l.updatedAt;
  }
  async maintain(): Promise<number | undefined> {
    if (!serveEnabled(this.env)) return undefined;
    return this.commit(async (s) => {
      const cursor = await s.get<MaintenanceCursor>(maintenanceCursorKey);
      const machines = await s.list<ServeMachine>({
        prefix: machinePrefix,
        limit: maintenanceMachineBatch,
        ...(cursor ? { startAfter: cursor.after } : {}),
      });
      const now = Date.now();
      let next = cursor?.next;
      let after = cursor?.after ?? "";
      let remaining = maintenanceLeaseReadBudget;
      let processed = 0;
      // oxlint-disable eslint/no-await-in-loop -- Commit each machine in order without a global lease fan-out.
      for (const [key, m] of machines) {
        // Existing machines can still hold up to 128 reservations. Never start a
        // machine whose complete allocation set would exceed this tick's budget.
        if (m.leases.length > remaining) break;
        remaining -= m.leases.length;
        const records = await this.records(s, m);
        for (const l of records) {
          const before = JSON.stringify(l);
          this.expire(l, m, now);
          if (!live(l)) await new ServeRelayControl(this.env).closeLease(l);
          if (JSON.stringify(l) !== before) await putLeaseRecord(s, l);
          if (live(l)) {
            const deadline = Math.min(
              m.lastHeartbeat + lostMS,
              Date.parse(l.expiresAt),
              idleDeadline(l),
            );
            next = next === undefined ? deadline : Math.min(next, deadline);
          }
        }
        if (records.every(leaseProviderCleanupConfirmed)) {
          const pruneAt = m.lastHeartbeat + pruneMS;
          if (now > pruneAt) {
            await s.delete(key);
            await s.delete((await this.ownerPrefix(m.owner, m.org)) + m.machineID);
          } else {
            next = next === undefined ? pruneAt : Math.min(next, pruneAt);
          }
        }
        after = key;
        processed++;
      }
      // oxlint-enable eslint/no-await-in-loop
      if (processed < machines.size || machines.size === maintenanceMachineBatch) {
        // Carry earlier deadlines through the whole sweep; a later page must not
        // replace them with its own, later wake. A full page may need an empty tail tick.
        await s.put(maintenanceCursorKey, {
          after,
          ...(next === undefined ? {} : { next }),
        } satisfies MaintenanceCursor);
        return Math.min(next ?? Infinity, now + maintenanceResumeMS);
      }
      if (cursor) await s.delete(maintenanceCursorKey);
      return next;
    });
  }
  async create(
    r: Request,
    input: LeaseRequest,
    fixedID?: string,
    validate?: (l: LeaseRecord, s: CoordinatorStorageView) => Promise<Response | undefined>,
  ): Promise<Response> {
    try {
      const disabled = serveDisabledResponse(this.env, requestOwner(r));
      if (disabled) return disabled;
      if (requestOwner(r) === "unknown" || !requestOwner(r))
        throw new ServeError("serve_owner_required", 403);
      const intent = await sha256Hex(JSON.stringify(input));
      return await this.commit(async (s) => {
        const id =
          fixedID ?? input.leaseID ?? `cbx_${crypto.randomUUID().replaceAll("-", "").slice(0, 12)}`;
        if (!/^cbx_[a-f0-9]{12}$/.test(id)) throw new ServeError("invalid_lease_id");
        if (new TextEncoder().encode(JSON.stringify(input)).length > serveBodyLimit)
          throw new ServeError("body_too_large", 413);
        const priorAttempt = await s.get<CreateAttemptRecord>(`create-attempt:${id}`);
        if (
          priorAttempt &&
          (priorAttempt.state === "canceled" ||
            priorAttempt.token !== input.createAttemptID ||
            priorAttempt.owner !== requestOwner(r) ||
            priorAttempt.org !== requestOrg(r, this.env))
        )
          throw new ServeError("create_attempt_conflict", 409);
        if (
          input.createAttemptID !== undefined &&
          !/^cat_[a-f0-9]{32}$/.test(input.createAttemptID)
        )
          throw new ServeError("invalid_create_attempt_id");
        if (await s.get(`workspace-lease:${id}`)) throw new ServeError("lease_id_conflict", 409);
        const existing = await s.get<LeaseRecord>(`lease:${id}`);
        if (existing) {
          if (
            existing.provider !== "serve" ||
            existing.owner !== requestOwner(r) ||
            existing.org !== requestOrg(r, this.env) ||
            existing.createAttemptID !== input.createAttemptID ||
            existing.serveIntent !== intent
          )
            throw new ServeError("create_attempt_conflict", 409);
          return json({ lease: publicLeaseRecord(existing) }, { status: 202 });
        }
        if (
          (input.target && input.target !== "linux") ||
          input.desktop ||
          input.browser ||
          input.code ||
          input.tailscale
        )
          throw new ServeError("serve_unsupported_feature");
        if (!validLeaseKey(input.sshPublicKey)) throw new ServeError("invalid_ssh_public_key");
        if (
          input.os ||
          input.imageRequirements ||
          input.capacity?.minMemoryMiB ||
          input.capacity?.minVCPUs
        )
          throw new ServeError("serve_unsupported_resource_request");
        const machines = await this.machines(s, r);
        const candidates = await Promise.all(
          [...machines.values()]
            .filter((m) => this.owned(m, r))
            .map(async (m) => {
              const records = await this.records(s, m);
              return {
                m,
                liveLeases: records.filter(live).length,
                reservedLeases: records
                  .filter((l) => !leaseProviderCleanupConfirmed(l))
                  .map((l) => l.id),
                view: await this.view(s, m, records),
                size: serveSize(
                  m,
                  input.class ?? "beast",
                  input.serverTypeExplicit ? input.serverType : undefined,
                ),
              };
            }),
        );
        if (candidates.reduce((total, c) => total + c.liveLeases, 0) >= maxOwnerLiveLeases)
          throw new ServeError("serve_lease_limit", 429);
        const eligible = candidates
          .filter(
            ({ m, view, size }) =>
              view.online &&
              (m.availability === undefined || m.availability.state === "available") &&
              !m.draining &&
              view.allocatedLeases < Math.min(m.maxLeases, maxLeases) &&
              view.freeCPUs >= size.cpus &&
              view.freeMemoryMB >= size.memoryMB &&
              (!input.serveMachine ||
                input.serveMachine === m.name ||
                input.serveMachine === m.machineID) &&
              (!input.architecture || input.architecture === m.arch),
          )
          .toSorted(
            (a, b) =>
              b.view.freeCPUs - a.view.freeCPUs || a.m.machineID.localeCompare(b.m.machineID),
          );
        const chosen = eligible[0];
        if (!chosen)
          throw new ServeError(
            "serve_capacity_unavailable",
            409,
            candidates.map((c) => c.view),
          );
        const now = new Date();
        const policy = {
          ...resolveLeasePolicy(this.env, requestOwner(r), requestOrg(r, this.env)),
          runtimeExtension: true,
        };
        const {
          ttlSeconds: ttl,
          idleTimeoutSeconds: idle,
          policyNotice,
        } = clampLeaseDurations(input, policy);
        const l: LeaseRecord = {
          id,
          serveIntent: intent,
          provider: "serve",
          lifecycle: "managed",
          owner: requestOwner(r),
          org: requestOrg(r, this.env),
          target: "linux",
          architecture: chosen.m.arch,
          profile: input.profile ?? "default",
          class: input.class ?? "beast",
          serverType: `cpu${chosen.size.cpus}`,
          cloudID: `${chosen.m.machineID}/${id}`,
          serverID: 0,
          serverName: id,
          providerKey: "",
          host: "",
          sshUser: "crabbox",
          sshPort: "22",
          sshFallbackPorts: [],
          workRoot: "/work/crabbox",
          keep: input.keep ?? false,
          leasePolicy: policy,
          ...(policyNotice ? { policyNotice } : {}),
          ttlSeconds: ttl,
          idleTimeoutSeconds: idle,
          estimatedHourlyUSD: 0,
          maxEstimatedUSD: 0,
          state: "provisioning",
          createdAt: now.toISOString(),
          updatedAt: now.toISOString(),
          lastTouchedAt: now.toISOString(),
          expiresAt: new Date(now.getTime() + ttl * 1000).toISOString(),
          ...(input.createAttemptID
            ? {
                createAttemptID: input.createAttemptID,
                createAttemptGeneration: crypto.randomUUID(),
              }
            : {}),
          slug: input.slug ?? id,
          releaseDeletesServer: true,
          provisioningResourceMayExist: true,
          serve: {
            machineID: chosen.m.machineID,
            ...chosen.size,
            sshPublicKey: input.sshPublicKey,
            image: chosen.m.image,
          },
        };
        const blocked = await validate?.(l, s);
        if (blocked) return blocked;
        if (input.createAttemptID)
          await s.put(`create-attempt:${id}`, {
            version: 1,
            requestedLeaseID: id,
            token: input.createAttemptID,
            owner: l.owner,
            org: l.org,
            state: "pending",
            canonicalLeaseID: id,
            generation: l.createAttemptGeneration!,
            cloudID: l.cloudID,
            createdAt: l.createdAt,
            updatedAt: l.updatedAt,
          } satisfies CreateAttemptRecord);
        await putLeaseRecord(s, l);
        chosen.m.leases = [...chosen.reservedLeases, id];
        await this.putMachine(s, chosen.m);
        return json({ lease: publicLeaseRecord(l) }, { status: 202 });
      });
    } catch (e) {
      if (e instanceof ServeError)
        return json({ error: e.code, details: e.details }, { status: e.status });
      throw e;
    }
  }
  async release(r: Request, l: LeaseRecord): Promise<Response> {
    return this.commit(async (s) => {
      const current = await s.get<LeaseRecord>(`lease:${l.id}`);
      if (!current || current.owner !== requestOwner(r) || current.org !== requestOrg(r, this.env))
        return json({ error: "not_found" }, { status: 404 });
      current.state = "released";
      current.releaseDeletesServer = true;
      current.updatedAt = new Date().toISOString();
      current.endedAt ??= current.cleanupCompletedAt ?? current.updatedAt;
      if (!leaseProviderCleanupConfirmed(current)) current.cleanupStartedAt ??= current.updatedAt;
      await new ServeRelayControl(this.env).closeLease(current);
      await putLeaseRecord(s, current);
      return json({ lease: publicLeaseRecord(current) });
    });
  }
  private async report(
    r: Request,
    id: string,
    action: string,
    input: Record<string, unknown>,
  ): Promise<Response> {
    return this.commit(async (s) => {
      if (action !== "ready" && action !== "failed") throw new ServeError("not_found", 404);
      const l = await s.get<LeaseRecord>(`lease:${id}`);
      if (
        !l ||
        l.provider !== "serve" ||
        l.owner !== requestOwner(r) ||
        l.org !== requestOrg(r, this.env) ||
        l.serve?.machineID !== input["machineID"]
      )
        throw new ServeError("not_found", 404);
      const m = await s.get<ServeMachine>(machinePrefix + l.serve!.machineID);
      if (!m || !this.owned(m, r)) throw new ServeError("not_found", 404);
      this.expire(l, m, Date.now());
      if (!live(l)) {
        await new ServeRelayControl(this.env).closeLease(l);
        await putLeaseRecord(s, l);
        throw new ServeError("serve_lease_ended", 409);
      }
      if (action === "ready") {
        if (
          input["host"] !== undefined ||
          input["port"] !== undefined ||
          !validText(input["sshHostKey"], 1024) ||
          !/^ssh-ed25519 [A-Za-z0-9+/=]+(?: |$)/.test(input["sshHostKey"])
        )
          throw new ServeError("invalid_serve_endpoint");
        if (l.state === "active" && l.sshHostKey !== input["sshHostKey"])
          throw new ServeError("serve_endpoint_changed", 409);
        const fingerprint = await hostKeyFingerprint(input["sshHostKey"]);
        if (input["fingerprint"] !== undefined && input["fingerprint"] !== fingerprint)
          throw new ServeError("invalid_serve_host_key_fingerprint");
        l.sshHostKeyFingerprint = fingerprint;
        const evidence = input["imageEvidence"] as LeaseRecord["imageEvidence"];
        if (evidence) {
          if (
            typeof evidence !== "object" ||
            !validText(evidence.configuredReference, 512) ||
            !validText(evidence.runtimeImageId, 512) ||
            !Array.isArray(evidence.repositoryDigests) ||
            evidence.repositoryDigests.length > 16 ||
            !evidence.repositoryDigests.every((d) => validText(d, 512)) ||
            !["available", "unavailable", "unknown"].includes(evidence.repositoryDigestStatus)
          )
            throw new ServeError("invalid_image_evidence");
          l.imageEvidence = evidence;
        }
        l.state = "active";
        l.host = l.id;
        l.sshPort = "22";
        l.sshJump = jumpForMachine(m, this.env);
        l.sshHostKey = input["sshHostKey"];
      } else {
        l.state = "failed";
        l.failureError = validText(input["reason"], 1024) ? input["reason"] : "serve agent failed";
        l.endedAt = new Date().toISOString();
      }
      l.updatedAt = new Date().toISOString();
      if (!live(l)) await new ServeRelayControl(this.env).closeLease(l);
      await putLeaseRecord(s, l);
      return json({ ok: true });
    });
  }
}

function idleDeadline(l: LeaseRecord): number {
  return Date.parse(l.lastTouchedAt ?? l.createdAt) + (l.idleTimeoutSeconds ?? 1800) * 1000;
}
