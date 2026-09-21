import type { CoordinatorRuntime, CoordinatorStorageView } from "./coordinator-runtime";
import { sha256Hex } from "./encoding";
import { json } from "./http";
import { leaseProviderCleanupConfirmed } from "./lease-cleanup";
import { publicLeaseRecord, publicReadyPoolEntry } from "./org-records";
import type { LeaseRecord, ReadyPoolEntry, ReadyPoolBorrowRequest } from "./types";

export const portablePoolPrefix = "portable-ready-pool-v1:";
import { provisioningDuePrefix, type ProvisioningDueRecord } from "./coordinator-runtime";
import { portablePoolWakePrefix, setPoolWake } from "./ready-pool-wake";
export const portablePoolReservationKey = (id: string) => `portable-ready-pool-v1-lease:${id}`;
export const portablePoolGrantKey = (id: string) => `portable-ready-pool-v1-grant:${id}`;
export const portablePoolMaxDurationMs = 30 * 60_000;
const acknowledgementMs = 60_000;
const retryMs = 15_000;
const installationMs = 45_000;
const ttlMarginMs = 60_000;

export interface PoolAccessBinding {
  leaseID: string;
  provider: string;
  resourceID: string;
  scope: string;
  user: string;
}

export interface PoolAccessGrant {
  id: string;
  key: string;
  leaseID: string;
  owner: string;
  org: string;
  binding: PoolAccessBinding;
  identity: NonNullable<ReadyPoolEntry["identity"]>;
  generation: number;
  borrowHash: string;
  receiptHash: string;
  publicKey: string;
  fingerprint: string;
  expiresAt: string;
  acknowledgementDeadline: string;
  state: "pending" | "installing" | "active" | "revoking" | "revoked";
  result?: "ready" | "drain" | "release";
  reason?: string;
  updatedAt: string;
}

export interface PoolAccessReservation {
  key: string;
  binding: PoolAccessBinding;
  generation: number;
}

// Provider operations are replay-safe by binding + generation. Revoke must fence
// delayed installs as well as existing sessions, even if install's reply was lost.
export interface ProviderPoolAccess {
  enroll(lease: LeaseRecord): Promise<PoolAccessBinding>;
  install(grant: PoolAccessGrant): Promise<void>;
  revoke(grant: PoolAccessGrant): Promise<{ destroyed: boolean }>;
}

export async function poolTokenHash(domain: "borrow" | "receipt" | "fill", token: string) {
  return sha256Hex(`crabbox-portable-pool-v1:${domain}\0${token}`);
}

export function poolPublicKey(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  // An Ed25519 SSH wire key is exactly 51 bytes; reject options and comments.
  const match = /^ssh-ed25519 ([A-Za-z0-9+/]{68})$/.exec(value);
  if (!match) return undefined;
  try {
    const bytes = Uint8Array.from(atob(match[1]!), (c) => c.charCodeAt(0));
    if (
      bytes.length !== 51 ||
      bytes.slice(0, 19).join(",") !== "0,0,0,11,115,115,104,45,101,100,50,53,53,49,57,0,0,0,32"
    )
      return undefined;
    return value;
  } catch {
    return undefined;
  }
}

export function poolGrantPublic(grant: PoolAccessGrant) {
  return {
    id: grant.id,
    state: grant.state,
    generation: grant.generation,
    leaseID: grant.leaseID,
    expiresAt: grant.expiresAt,
    acknowledgementDeadline: grant.acknowledgementDeadline,
    fingerprint: grant.fingerprint,
  };
}

interface AccessHooks {
  owner(request: Request): string;
  org(request: Request): string;
  authorized(lease: LeaseRecord, request: Request): boolean;
  matches(entry: ReadyPoolEntry, input: ReadyPoolBorrowRequest): boolean;
  identityMatches(entry: ReadyPoolEntry, lease: LeaseRecord): boolean;
  identityEqual(left: ReadyPoolEntry["identity"], right: ReadyPoolEntry["identity"]): boolean;
  heartbeatDeadline(entry: ReadyPoolEntry): number | undefined;
  withoutBorrow(entry: ReadyPoolEntry): ReadyPoolEntry;
  heartbeatMs: number;
  provider(binding: PoolAccessBinding): ProviderPoolAccess | undefined;
  counters(
    storage: CoordinatorStorageView,
    entry: Pick<ReadyPoolEntry, "owner" | "org" | "key">,
    delta: Record<string, number>,
  ): Promise<void>;
  release(lease: LeaseRecord): Promise<void>;
}

export class ReadyPoolAccess {
  constructor(
    private readonly runtime: CoordinatorRuntime,
    private readonly hooks: AccessHooks,
  ) {}

  async transaction<T>(fn: (storage: CoordinatorStorageView) => Promise<T>): Promise<T> {
    if (!this.runtime.provisioning)
      throw new Error("portable pools require transactional wake support");
    return this.runtime.provisioning.commitAndWake(fn);
  }

  private async save(
    storage: CoordinatorStorageView,
    grant: PoolAccessGrant,
    entry: ReadyPoolEntry,
  ) {
    await storage.put(portablePoolGrantKey(grant.leaseID), grant);
    await storage.put(`${portablePoolPrefix}${entry.key}:${entry.leaseID}`, entry);
    const wake =
      grant.state === "installing"
        ? Math.min(Date.parse(grant.expiresAt), Date.now() + installationMs)
        : grant.state === "revoking"
          ? Date.now() + retryMs
          : Math.min(
              Date.parse(grant.expiresAt),
              grant.state === "pending"
                ? Date.parse(grant.acknowledgementDeadline)
                : (this.hooks.heartbeatDeadline(entry) ?? Infinity),
            );
    if (grant.state === "revoked") {
      if (entry.state === "ready")
        await setPoolWake(storage, grant.leaseID, Date.parse(entry.expiresAt) - ttlMarginMs);
      else await setPoolWake(storage, grant.leaseID, Date.now() + retryMs);
    } else await setPoolWake(storage, grant.leaseID, wake);
  }

  private validLease(
    lease: LeaseRecord | undefined,
    reservation: PoolAccessReservation | undefined,
    entry: ReadyPoolEntry,
    now: number,
  ): lease is LeaseRecord {
    return Boolean(
      lease &&
      reservation &&
      lease.state === "active" &&
      lease.id === reservation.binding.leaseID &&
      lease.provider === reservation.binding.provider &&
      lease.cloudID === reservation.binding.resourceID &&
      lease.region === reservation.binding.scope &&
      lease.sshUser === reservation.binding.user &&
      Date.parse(lease.expiresAt) > now &&
      this.hooks.identityMatches(entry, lease),
    );
  }

  async borrow(
    request: Request,
    key: string,
    input: ReadyPoolBorrowRequest & { publicKey?: unknown; durationSeconds?: unknown },
  ): Promise<Response> {
    const receivedAt = Date.now();
    const publicKey = poolPublicKey(input.publicKey);
    const duration =
      input.durationSeconds === undefined
        ? portablePoolMaxDurationMs
        : Number(input.durationSeconds) * 1000;
    if (
      !publicKey ||
      !Number.isSafeInteger(duration) ||
      duration <= 0 ||
      duration > portablePoolMaxDurationMs
    )
      return json(
        {
          error: "invalid_access_request",
          message: "Ed25519 publicKey and a duration of at most 1800 seconds required",
        },
        { status: 400 },
      );
    const borrowToken = crypto.randomUUID();
    const receiptToken = crypto.randomUUID();
    const id = crypto.randomUUID();
    const [borrowHash, receiptHash, fingerprint] = await Promise.all([
      poolTokenHash("borrow", borrowToken),
      poolTokenHash("receipt", receiptToken),
      sha256Hex(publicKey),
    ]);
    const now = Date.now();
    const authExpiry = request.headers.get("x-crabbox-token-expires-at");
    const hardDeadline = Math.min(now + duration, authExpiry ? Date.parse(authExpiry) : Infinity);
    if (!Number.isFinite(hardDeadline) || hardDeadline <= now)
      return json({ error: "authorization_expired" }, { status: 403 });
    return this.transaction(async (storage) => {
      const scope = { key, owner: this.hooks.owner(request), org: this.hooks.org(request) };
      if (hardDeadline <= Date.now())
        return json({ error: "authorization_expired" }, { status: 403 });
      await this.hooks.counters(storage, scope, { borrowRequests: 1 });
      const entries = [
        ...(await storage.list<ReadyPoolEntry>({ prefix: portablePoolPrefix })).values(),
      ]
        .filter(
          (entry) =>
            entry.key === key && entry.state === "ready" && this.hooks.matches(entry, input),
        )
        .toSorted((a, b) =>
          (a.lastReadyAt ?? a.createdAt).localeCompare(b.lastReadyAt ?? b.createdAt),
        );
      /* oxlint-disable eslint/no-await-in-loop -- candidate admission and its compound writes must use one ordered transaction. */
      for (const entry of entries) {
        const lease = await storage.get<LeaseRecord>(`lease:${entry.leaseID}`);
        const reservation = await storage.get<PoolAccessReservation>(
          portablePoolReservationKey(entry.leaseID),
        );
        if (
          !this.validLease(lease, reservation, entry, now) ||
          !this.hooks.authorized(lease, request) ||
          Date.parse(lease.expiresAt) <= now + ttlMarginMs
        )
          continue;
        const previous = await storage.get<PoolAccessGrant>(portablePoolGrantKey(entry.leaseID));
        if (previous && previous.state !== "revoked") continue;
        const expiresAt = new Date(
          Math.floor(Math.min(hardDeadline, Date.parse(lease.expiresAt)) / 1000) * 1000,
        ).toISOString();
        const grant: PoolAccessGrant = {
          id,
          key,
          leaseID: lease.id,
          owner: this.hooks.owner(request),
          org: this.hooks.org(request),
          binding: reservation!.binding,
          identity: entry.identity!,
          generation: reservation!.generation + 1,
          borrowHash,
          receiptHash,
          publicKey,
          fingerprint,
          expiresAt,
          acknowledgementDeadline: new Date(
            Math.min(now + acknowledgementMs, Date.parse(expiresAt)),
          ).toISOString(),
          state: "pending",
          updatedAt: new Date(now).toISOString(),
        };
        const borrowed: ReadyPoolEntry = {
          ...entry,
          state: "busy",
          borrowedBy: grant.owner,
          borrowedAt: grant.updatedAt,
          borrowHardDeadline: expiresAt,
          expiresAt: lease.expiresAt,
          borrowHeartbeatRequired: true,
          borrowHeartbeatAt: grant.updatedAt,
          borrowExpiresAt: new Date(
            Math.min(now + this.hooks.heartbeatMs, Date.parse(expiresAt)),
          ).toISOString(),
          updatedAt: grant.updatedAt,
          lastUsedAt: grant.updatedAt,
        };
        await storage.put(portablePoolReservationKey(lease.id), {
          ...reservation,
          generation: grant.generation,
        });
        await this.save(storage, grant, borrowed);
        await this.hooks.counters(storage, entry, {
          warmHits: 1,
          grantsIssued: 1,
          borrowLatencyMsTotal: Math.max(0, Date.now() - receivedAt),
          readyAgeMsTotal: Math.max(0, now - Date.parse(entry.lastReadyAt ?? entry.createdAt)),
        });
        return json({
          entry: publicReadyPoolEntry(borrowed),
          lease: publicLeaseRecord(lease),
          grant: poolGrantPublic(grant),
          borrowToken,
          receiptToken,
        });
      }
      /* oxlint-enable eslint/no-await-in-loop */
      await this.hooks.counters(storage, scope, { warmMisses: 1 });
      return json(
        { error: "no_ready_lease", message: "no authorized compatible portable lease is ready" },
        { status: 409 },
      );
    });
  }

  async act(
    request: Request,
    key: string,
    action: "ack" | "heartbeat" | "return",
    input: {
      leaseID?: string;
      borrowToken?: string;
      receiptToken?: string;
      publicKey?: string;
      result?: string;
      identity?: ReadyPoolEntry["identity"];
      timing?: { firstCommandMs?: number; scrubMs?: number; scrubFailed?: boolean };
    },
  ): Promise<Response> {
    if (
      input.timing !== undefined &&
      (input.timing === null ||
        typeof input.timing !== "object" ||
        Array.isArray(input.timing) ||
        [input.timing.firstCommandMs, input.timing.scrubMs].some(
          (value) =>
            value !== undefined &&
            (!Number.isSafeInteger(value) || value < 0 || value > portablePoolMaxDurationMs),
        ) ||
        (input.timing.scrubFailed !== undefined && typeof input.timing.scrubFailed !== "boolean"))
    )
      return json({ error: "invalid_completion_timing" }, { status: 400 });
    const borrowHash = await poolTokenHash("borrow", input.borrowToken ?? "");
    const receiptHash = await poolTokenHash("receipt", input.receiptToken ?? "");
    const now = Date.now();
    const outcome = await this.transaction(async (storage): Promise<Response | PoolAccessGrant> => {
      const grant = await storage.get<PoolAccessGrant>(portablePoolGrantKey(input.leaseID ?? ""));
      const entry = await storage.get<ReadyPoolEntry>(
        `${portablePoolPrefix}${key}:${input.leaseID}`,
      );
      const lease = await storage.get<LeaseRecord>(`lease:${input.leaseID}`);
      const reservation = await storage.get<PoolAccessReservation>(
        portablePoolReservationKey(input.leaseID ?? ""),
      );
      if (
        !grant ||
        !entry ||
        grant.key !== key ||
        grant.owner !== this.hooks.owner(request) ||
        grant.org !== this.hooks.org(request) ||
        grant.borrowHash !== borrowHash ||
        grant.receiptHash !== receiptHash
      )
        return json({ error: "access_receipt_mismatch" }, { status: 403 });
      if (!lease || !this.hooks.authorized(lease, request)) {
        if (grant.state !== "revoked")
          await this.invalidate(storage, grant, entry, "authorization changed", "drain");
        return json({ error: "forbidden" }, { status: 403 });
      }
      const authExpiry = request.headers.get("x-crabbox-token-expires-at");
      if (
        action === "ack" &&
        authExpiry &&
        (!Number.isFinite(Date.parse(authExpiry)) ||
          Date.parse(authExpiry) < Date.parse(grant.expiresAt))
      ) {
        await this.invalidate(
          storage,
          grant,
          entry,
          "ack authorization ends before grant",
          "drain",
        );
        return json({ error: "authorization_expired" }, { status: 403 });
      }
      if (action === "return") {
        if (!["ready", "drain", "release"].includes(input.result ?? "ready"))
          return json({ error: "invalid_result" }, { status: 400 });
        if (grant.state === "revoked")
          return json({ entry: publicReadyPoolEntry(entry), grant: poolGrantPublic(grant) });
        if (grant.state === "active" && input.timing)
          await this.hooks.counters(storage, entry, {
            completionReports: 1,
            firstCommandMsTotal: input.timing.firstCommandMs ?? 0,
            scrubMsTotal: input.timing.scrubMs ?? 0,
            scrubFailures: input.timing.scrubFailed ? 1 : 0,
          });
        const reusable =
          grant.state === "active" &&
          this.validLease(lease, reservation, entry, now) &&
          Date.parse(grant.expiresAt) > now &&
          (this.hooks.heartbeatDeadline(entry) ?? 0) > now &&
          this.hooks.identityEqual(input.identity, grant.identity);
        await this.invalidate(
          storage,
          grant,
          entry,
          "returned",
          reusable ? ((input.result ?? "ready") as "ready" | "drain" | "release") : "drain",
        );
        return grant;
      }
      if (
        !this.validLease(lease, reservation, entry, now) ||
        reservation?.generation !== grant.generation ||
        Date.parse(grant.expiresAt) <= now ||
        (this.hooks.heartbeatDeadline(entry) ?? 0) <= now ||
        (grant.state === "pending" && Date.parse(grant.acknowledgementDeadline) <= now)
      ) {
        if (grant.state !== "revoked")
          await this.invalidate(
            storage,
            grant,
            entry,
            "borrow expired or binding changed",
            "drain",
          );
        return json({ error: "borrow_expired" }, { status: 409 });
      }
      if (action === "heartbeat") {
        if (grant.state !== "active") return json({ error: "grant_not_active" }, { status: 409 });
        const updated = {
          ...entry,
          borrowHeartbeatAt: new Date(now).toISOString(),
          updatedAt: new Date(now).toISOString(),
          borrowExpiresAt: new Date(
            Math.min(now + this.hooks.heartbeatMs, Date.parse(grant.expiresAt)),
          ).toISOString(),
        };
        await this.save(storage, grant, updated);
        await this.hooks.counters(storage, entry, { borrowHeartbeats: 1 });
        return json({ entry: publicReadyPoolEntry(updated), grant: poolGrantPublic(grant) });
      }
      if (input.publicKey !== grant.publicKey)
        return json({ error: "public_key_mismatch" }, { status: 409 });
      if (grant.state === "active")
        return json({
          entry: publicReadyPoolEntry(entry),
          lease: publicLeaseRecord(lease),
          grant: poolGrantPublic(grant),
        });
      if (grant.state !== "pending") return json({ error: "grant_not_pending" }, { status: 409 });
      grant.state = "installing";
      grant.updatedAt = new Date(now).toISOString();
      await this.save(storage, grant, entry);
      return grant;
    });
    if (outcome instanceof Response) return outcome;
    if (action === "return") {
      await this.cleanup(outcome.leaseID);
      return this.result(outcome.leaseID, key);
    }
    try {
      const provider = this.hooks.provider(outcome.binding);
      if (!provider) throw new Error("provider pool access unavailable");
      await provider.install(outcome);
    } catch {
      await this.invalidateLease(outcome.leaseID, "access installation uncertain");
      return json(
        { error: "access_installation_uncertain", grant: poolGrantPublic(outcome) },
        { status: 502 },
      );
    }
    const published = await this.transaction(async (storage) => {
      const current = await storage.get<PoolAccessGrant>(portablePoolGrantKey(outcome.leaseID));
      const entry = await storage.get<ReadyPoolEntry>(
        `${portablePoolPrefix}${key}:${outcome.leaseID}`,
      );
      const lease = await storage.get<LeaseRecord>(`lease:${outcome.leaseID}`);
      const reservation = await storage.get<PoolAccessReservation>(
        portablePoolReservationKey(outcome.leaseID),
      );
      if (!current || !entry || current.id !== outcome.id || current.state !== "installing")
        return false;
      const authExpiry = request.headers.get("x-crabbox-token-expires-at");
      if (
        !this.validLease(lease, reservation, entry, Date.now()) ||
        !this.hooks.authorized(lease, request) ||
        (authExpiry && Date.parse(authExpiry) <= Date.now()) ||
        Date.parse(current.expiresAt) <= Date.now() ||
        (this.hooks.heartbeatDeadline(entry) ?? 0) <= Date.now()
      ) {
        await this.invalidate(storage, current, entry, "installation revalidation failed", "drain");
        return false;
      }
      current.state = "active";
      current.updatedAt = new Date().toISOString();
      await this.save(storage, current, entry);
      await this.hooks.counters(storage, entry, { grantsAcknowledged: 1 });
      return true;
    });
    if (!published) return json({ error: "access_installation_invalidated" }, { status: 409 });
    return this.result(outcome.leaseID, key);
  }

  private async result(leaseID: string, key: string): Promise<Response> {
    const grant = await this.runtime.storage.get<PoolAccessGrant>(portablePoolGrantKey(leaseID));
    const entry = await this.runtime.storage.get<ReadyPoolEntry>(
      `${portablePoolPrefix}${key}:${leaseID}`,
    );
    const lease = await this.runtime.storage.get<LeaseRecord>(`lease:${leaseID}`);
    return json(
      {
        entry: entry && publicReadyPoolEntry(entry),
        lease: lease && publicLeaseRecord(lease),
        grant: grant && poolGrantPublic(grant),
      },
      { status: grant?.state === "revoking" ? 202 : 200 },
    );
  }

  private async invalidate(
    storage: CoordinatorStorageView,
    grant: PoolAccessGrant,
    entry: ReadyPoolEntry,
    reason: string,
    result: "ready" | "drain" | "release",
  ) {
    if (grant.state === "revoked" || grant.state === "revoking") return;
    grant.state = "revoking";
    grant.reason = reason;
    grant.result = result;
    grant.updatedAt = new Date().toISOString();
    await this.save(storage, grant, {
      ...entry,
      state: result === "ready" ? "busy" : "quarantined",
      lastResult: reason,
      updatedAt: grant.updatedAt,
    });
    await this.hooks.counters(storage, entry, {
      grantsRevoked: 1,
      ...(result === "ready" ? {} : { quarantined: 1 }),
    });
  }

  async invalidateLease(leaseID: string, reason: string): Promise<void> {
    if (!(await this.runtime.storage.get(portablePoolReservationKey(leaseID)))) return;
    await this.transaction(async (storage) => {
      const grant = await storage.get<PoolAccessGrant>(portablePoolGrantKey(leaseID));
      if (!grant) return;
      const entry = await storage.get<ReadyPoolEntry>(
        `${portablePoolPrefix}${grant.key}:${leaseID}`,
      );
      if (entry) await this.invalidate(storage, grant, entry, reason, "drain");
    });
  }

  async cleanup(leaseID: string): Promise<void> {
    const grant = await this.runtime.storage.get<PoolAccessGrant>(portablePoolGrantKey(leaseID));
    if (!grant || grant.state !== "revoking") return;
    let destroyed: boolean;
    try {
      const provider = this.hooks.provider(grant.binding);
      if (!provider) throw new Error("provider pool access unavailable");
      ({ destroyed } = await provider.revoke(grant));
      // Retire ancillary provider resources under the same durable retry intent.
      if (grant.result !== "ready" || destroyed) {
        const lease = await this.runtime.storage.get<LeaseRecord>(`lease:${leaseID}`);
        if (lease) await this.hooks.release(lease);
      }
    } catch {
      await this.transaction(async (storage) => {
        const current = await storage.get<PoolAccessGrant>(portablePoolGrantKey(leaseID));
        if (current?.id !== grant.id || current.state !== "revoking") return;
        await setPoolWake(storage, leaseID, Date.now() + retryMs);
        const entry = await storage.get<ReadyPoolEntry>(
          `${portablePoolPrefix}${grant.key}:${leaseID}`,
        );
        if (entry) await this.hooks.counters(storage, entry, { fencingFailures: 1 });
      });
      return;
    }
    const release = await this.transaction(async (storage) => {
      const current = await storage.get<PoolAccessGrant>(portablePoolGrantKey(leaseID));
      const entry = await storage.get<ReadyPoolEntry>(
        `${portablePoolPrefix}${grant.key}:${leaseID}`,
      );
      const lease = await storage.get<LeaseRecord>(`lease:${leaseID}`);
      const reservation = await storage.get<PoolAccessReservation>(
        portablePoolReservationKey(leaseID),
      );
      if (!current || !entry || current.id !== grant.id || current.state !== "revoking")
        return undefined;
      const ready =
        !destroyed &&
        current.result === "ready" &&
        this.validLease(lease, reservation, entry, Date.now() + ttlMarginMs);
      current.state = "revoked";
      current.updatedAt = new Date().toISOString();
      await this.save(storage, current, {
        ...this.hooks.withoutBorrow(entry),
        state: ready ? "ready" : "draining",
        expiresAt: lease?.expiresAt ?? entry.expiresAt,
        updatedAt: current.updatedAt,
        ...(ready ? { lastReadyAt: current.updatedAt } : {}),
      });
      await this.hooks.counters(storage, entry, { grantsFenced: 1 });
      return !ready && grant.result === "ready" && !destroyed ? lease : undefined;
    });
    if (release) {
      try {
        await this.hooks.release(release);
      } catch {
        await this.transaction((storage) => setPoolWake(storage, leaseID, Date.now() + retryMs));
      }
    }
  }

  async maintain(): Promise<void> {
    /* oxlint-disable eslint/no-await-in-loop -- reconcile each durable generation before advancing to the next binding. */
    const due = await this.runtime.storage.list<ProvisioningDueRecord>({
      prefix: provisioningDuePrefix,
      limit: 8,
    });
    const ids: string[] = [];
    for (const [indexKey, record] of due) {
      if (record.kind !== "pool-access" || record.at > Date.now()) continue;
      const accepted = await this.transaction(async (storage) => {
        const pointer = await storage.get<{ at: number }>(
          `${portablePoolWakePrefix}${record.operationID}`,
        );
        if (pointer?.at !== record.at) {
          await storage.delete(indexKey);
          return false;
        }
        if (record.operationID.startsWith("claim-")) {
          const claimKey = `portable-ready-pool-v1-fill-claim:${record.operationID.slice(6)}`;
          const claim = await storage.get<{ expiresAt: string }>(claimKey);
          if (!claim || Date.parse(claim.expiresAt) <= Date.now()) {
            await storage.delete(claimKey);
            await setPoolWake(storage, record.operationID);
          } else await setPoolWake(storage, record.operationID, Date.parse(claim.expiresAt));
          return false;
        }
        return true;
      });
      if (accepted) ids.push(record.operationID);
    }
    const reservations = new Map<string, PoolAccessReservation>();
    for (const id of ids) {
      const reservation = await this.runtime.storage.get<PoolAccessReservation>(
        portablePoolReservationKey(id),
      );
      if (reservation) reservations.set(id, reservation);
    }
    for (const reservation of reservations.values()) {
      const id = reservation.binding.leaseID;
      const retiring = await this.transaction(async (storage) => {
        const entry = await storage.get<ReadyPoolEntry>(
          `${portablePoolPrefix}${reservation.key}:${id}`,
        );
        const lease = await storage.get<LeaseRecord>(`lease:${id}`);
        const grant = await storage.get<PoolAccessGrant>(portablePoolGrantKey(id));
        if (!entry || (grant && grant.state !== "revoked")) return undefined;
        if (
          entry.state === "ready" &&
          (!lease || !this.validLease(lease, reservation, entry, Date.now() + ttlMarginMs))
        ) {
          await storage.put(`${portablePoolPrefix}${reservation.key}:${id}`, {
            ...entry,
            state: "draining",
            updatedAt: new Date().toISOString(),
            lastResult: "hard TTL rotation",
          });
          await setPoolWake(storage, id, Date.now() + retryMs);
          await this.hooks.counters(storage, entry, { ttlRotations: 1 });
          return lease;
        }
        if (entry.state === "ready" && lease && entry.expiresAt !== lease.expiresAt) {
          await storage.put(`${portablePoolPrefix}${reservation.key}:${id}`, {
            ...entry,
            expiresAt: lease.expiresAt,
          });
          await setPoolWake(storage, id, Date.parse(lease.expiresAt) - ttlMarginMs);
        }
        if (entry.state === "draining" && lease?.state === "active") return lease;
        if (entry.state !== "ready") {
          if (!lease || leaseProviderCleanupConfirmed(lease)) await setPoolWake(storage, id);
          else await setPoolWake(storage, id, Date.now() + retryMs);
        }
        return undefined;
      });
      if (retiring) {
        try {
          await this.hooks.release(retiring);
        } catch {
          /* Cleanup remains unavailable and consumes capacity until a later proof. */
        }
        await this.transaction(async (storage) => {
          const lease = await storage.get<LeaseRecord>(`lease:${id}`);
          if (!lease || leaseProviderCleanupConfirmed(lease)) await setPoolWake(storage, id);
          else await setPoolWake(storage, id, Date.now() + retryMs);
        });
      }
    }
    const records = new Map<string, PoolAccessGrant>();
    for (const id of ids) {
      const grant = await this.runtime.storage.get<PoolAccessGrant>(portablePoolGrantKey(id));
      if (grant) records.set(id, grant);
    }
    for (const grant of records.values()) {
      await this.transaction(async (storage) => {
        const current = await storage.get<PoolAccessGrant>(portablePoolGrantKey(grant.leaseID));
        if (!current || current.state === "revoked" || current.state === "revoking") return;
        const entry = await storage.get<ReadyPoolEntry>(
          `${portablePoolPrefix}${current.key}:${current.leaseID}`,
        );
        const lease = await storage.get<LeaseRecord>(`lease:${current.leaseID}`);
        const reservation = await storage.get<PoolAccessReservation>(
          portablePoolReservationKey(current.leaseID),
        );
        const now = Date.now();
        if (!entry) return; // Keep cleanup evidence; enrollment/terminal pruning cannot delete live entries.
        if (
          !this.validLease(lease, reservation, entry, now) ||
          Date.parse(current.expiresAt) <= now ||
          (this.hooks.heartbeatDeadline(entry) ?? 0) <= now ||
          (current.state === "pending" && Date.parse(current.acknowledgementDeadline) <= now) ||
          (current.state === "installing" && Date.parse(current.updatedAt) + installationMs <= now)
        ) {
          await this.invalidate(storage, current, entry, "borrow expired or abandoned", "drain");
          await this.hooks.counters(storage, entry, { grantsExpired: 1 });
        }
      });
      await this.cleanup(grant.leaseID);
    }
    /* oxlint-enable eslint/no-await-in-loop */
  }
}
