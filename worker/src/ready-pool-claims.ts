import type { CoordinatorStorage, CoordinatorStorageView } from "./coordinator-runtime";
import { sha256Hex } from "./encoding";
import { sameOrgIdentityKey } from "./org-identity";
import type { LeaseRecord, ReadyPoolEntry, ReadyPoolIdentityV1 } from "./types";

export interface ReadyPoolClaimInput {
  operationID: string;
  coldLeaseID?: string;
  identity: ReadyPoolIdentityV1;
  lookupOnly?: boolean;
  selectOnly?: boolean;
}

export interface ReadyPoolAllocation {
  schema: "crabbox-ready-pool-allocation/v1";
  operationID: string;
  poolKey: string;
  owner: string;
  org: string;
  identity: ReadyPoolIdentityV1;
  kind: "warm" | "cold";
  phase: "selected" | "claiming" | "ready" | "failed";
  leaseID: string;
  leaseGeneration?: string;
  selectedAt: string;
  settledAt?: string;
  reasonCode:
    | "pool-hit"
    | "pool-miss"
    | "pool-unselected"
    | "pool-claim-failed"
    | "pool-claim-fenced";
  claimExpiresAt?: string;
}

interface StoredAllocation extends ReadyPoolAllocation {
  claimToken?: string;
}

export class ReadyPoolClaimError extends Error {
  constructor(
    readonly code: string,
    readonly allocation?: ReadyPoolAllocation,
  ) {
    super(code);
    this.name = "ReadyPoolClaimError";
  }
}

export const readyPoolClaimTTLMS = 120_000;
const entryKey = (pool: string, lease: string) => `typed-ready-pool-v1:${pool}:${lease}`;

export function sameReadyIdentity(a: ReadyPoolIdentityV1, b: ReadyPoolIdentityV1): boolean {
  return (
    a.schema === b.schema &&
    a.image.provider === b.image.provider &&
    a.image.scope === b.image.scope &&
    a.image.id === b.image.id &&
    a.architecture === b.architecture &&
    a.seedDigest === b.seedDigest &&
    a.cacheCompatibility === b.cacheCompatibility
  );
}

function publicAllocation(value: StoredAllocation): ReadyPoolAllocation {
  const { claimToken: _claimToken, ...result } = value;
  return result;
}

async function quarantine(
  transaction: CoordinatorStorageView,
  allocation: StoredAllocation,
  now: string,
  reasonCode: "pool-claim-failed" | "pool-claim-fenced",
): Promise<StoredAllocation> {
  const entry = await transaction.get<ReadyPoolEntry>(
    entryKey(allocation.poolKey, allocation.leaseID),
  );
  if (entry && allocation.claimToken && entry.borrowToken === allocation.claimToken) {
    const { borrowToken: _token, ...retained } = entry;
    await transaction.put(entryKey(allocation.poolKey, allocation.leaseID), {
      ...retained,
      state: "quarantined",
      lastResult: reasonCode,
      updatedAt: now,
    });
  }
  return { ...allocation, phase: "failed", reasonCode, settledAt: now };
}

/** Reserve in storage, prove the clean claim outside the transaction, then publish.
 * No provider I/O holds the fleet's lifecycle queue or the ready-pool borrow lock.
 */
export async function claimReadyPoolAllocation(params: {
  storage: CoordinatorStorage;
  poolKey: string;
  input: ReadyPoolClaimInput;
  principal: { owner: string; org: string };
  matches: (lease: LeaseRecord) => boolean;
  claim: (lease: LeaseRecord, tokenHash: string) => Promise<void>;
  exclusive?: <T>(operation: () => Promise<T>) => Promise<T>;
  now?: () => number;
}): Promise<{ allocation: ReadyPoolAllocation; entry?: ReadyPoolEntry; lease?: LeaseRecord }> {
  const { storage, poolKey, input, principal, matches } = params;
  const now = params.now ?? Date.now;
  const atomic = <T>(operation: (transaction: CoordinatorStorageView) => Promise<T>) =>
    params.exclusive
      ? params.exclusive(() => storage.transaction(operation))
      : storage.transaction(operation);
  if (
    !/^[A-Za-z0-9_-]{1,128}$/.test(input.operationID) ||
    (input.coldLeaseID !== undefined && !/^cbx_[A-Za-z0-9_-]{1,128}$/.test(input.coldLeaseID))
  ) {
    throw new ReadyPoolClaimError("invalid_pool_allocation");
  }
  const key = `ready-pool-allocation:v1:${await sha256Hex(JSON.stringify([principal.owner, principal.org, poolKey, input.operationID]))}`;
  const newToken = crypto.randomUUID();
  const reserved = await atomic(async (transaction) => {
    const previous = await transaction.get<StoredAllocation>(key);
    if (previous) {
      if (
        !sameReadyIdentity(previous.identity, input.identity) ||
        (previous.kind === "cold" && previous.leaseID !== input.coldLeaseID)
      ) {
        throw new ReadyPoolClaimError("pool_allocation_conflict");
      }
      if (
        !input.lookupOnly &&
        previous.kind === "warm" &&
        (previous.phase === "claiming" || previous.phase === "selected") &&
        Date.parse(previous.claimExpiresAt ?? "") <= now()
      ) {
        const failed = await quarantine(
          transaction,
          previous,
          new Date(now()).toISOString(),
          "pool-claim-fenced",
        );
        await transaction.put(key, failed);
        return { allocation: failed };
      }
      if (
        !input.lookupOnly &&
        !input.selectOnly &&
        previous.kind === "warm" &&
        previous.phase === "selected"
      ) {
        const lease = await transaction.get<LeaseRecord>(`lease:${previous.leaseID}`);
        const entry = await transaction.get<ReadyPoolEntry>(entryKey(poolKey, previous.leaseID));
        if (
          !lease ||
          lease.state !== "active" ||
          lease.cleanupStartedAt ||
          lease.createAttemptGeneration !== previous.leaseGeneration ||
          !matches(lease) ||
          entry?.state !== "busy" ||
          entry.borrowToken !== previous.claimToken ||
          Date.parse(lease.expiresAt) <= now()
        ) {
          const failed = await quarantine(
            transaction,
            previous,
            new Date(now()).toISOString(),
            "pool-claim-fenced",
          );
          await transaction.put(key, failed);
          return { allocation: failed };
        }
        const allocation: StoredAllocation = { ...previous, phase: "claiming" };
        await transaction.put(key, allocation);
        return { allocation, lease, reserved: true };
      }
      if (!input.lookupOnly && previous.phase === "ready") {
        const lease = await transaction.get<LeaseRecord>(`lease:${previous.leaseID}`);
        const entry = await transaction.get<ReadyPoolEntry>(entryKey(poolKey, previous.leaseID));
        if (
          !lease ||
          lease.state !== "active" ||
          lease.cleanupStartedAt ||
          lease.createAttemptGeneration !== previous.leaseGeneration ||
          !matches(lease) ||
          entry?.state !== "busy" ||
          entry.borrowToken !== previous.claimToken ||
          Date.parse(entry.borrowExpiresAt ?? "") <= now() ||
          Date.parse(lease.expiresAt) <= now()
        ) {
          const failed = await quarantine(
            transaction,
            previous,
            new Date(now()).toISOString(),
            "pool-claim-fenced",
          );
          await transaction.put(key, failed);
          return { allocation: failed };
        }
      }
      return { allocation: previous };
    }
    const selectedAt = new Date(now()).toISOString();
    const common = {
      schema: "crabbox-ready-pool-allocation/v1" as const,
      operationID: input.operationID,
      poolKey,
      ...principal,
      identity: input.identity,
      selectedAt,
    };
    if (input.lookupOnly) {
      if (!input.coldLeaseID) throw new ReadyPoolClaimError("pool_allocation_not_found");
      return {
        allocation: {
          ...common,
          kind: "cold" as const,
          phase: "selected" as const,
          leaseID: input.coldLeaseID,
          reasonCode: "pool-unselected" as const,
        },
      };
    }
    const entries = [
      ...(await transaction.list<ReadyPoolEntry>({ prefix: "typed-ready-pool-v1:" })).values(),
    ]
      .filter(
        (entry) =>
          entry.key === poolKey &&
          entry.owner === principal.owner &&
          sameOrgIdentityKey(entry.org, principal.org) &&
          entry.state === "ready" &&
          entry.singleUse === true,
      )
      .toSorted(
        (a, b) => a.createdAt.localeCompare(b.createdAt) || a.leaseID.localeCompare(b.leaseID),
      );
    const duplicateBusy = new Set(
      [...(await transaction.list<ReadyPoolEntry>({ prefix: "ready-pool:" })).values()]
        .filter((entry) => entry.state === "busy" || entry.state === "quarantined")
        .map((entry) => entry.leaseID),
    );
    for (const entry of entries) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- Every candidate must be validated in the same serializable reservation.
      const lease = await transaction.get<LeaseRecord>(`lease:${entry.leaseID}`);
      if (
        !lease ||
        lease.state !== "active" ||
        lease.cleanupStartedAt ||
        lease.readyPoolConsumedAt ||
        Date.parse(lease.expiresAt) <= now() ||
        duplicateBusy.has(lease.id) ||
        lease.owner !== principal.owner ||
        !sameOrgIdentityKey(lease.org, principal.org) ||
        !lease.createAttemptGeneration
      )
        continue;
      if (!matches(lease)) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- Drain incompatible inventory before looking at another candidate.
        await transaction.put(entryKey(poolKey, entry.leaseID), {
          ...entry,
          state: "draining",
          lastResult: "pool-identity-mismatch",
          updatedAt: selectedAt,
        });
        continue;
      }
      if (!entry.identity || !sameReadyIdentity(entry.identity, input.identity)) continue;
      const allocation: StoredAllocation = {
        ...common,
        kind: "warm",
        phase: input.selectOnly ? "selected" : "claiming",
        leaseID: lease.id,
        leaseGeneration: lease.createAttemptGeneration,
        reasonCode: "pool-hit",
        claimToken: newToken,
        claimExpiresAt: new Date(now() + readyPoolClaimTTLMS).toISOString(),
      };
      const borrowed: ReadyPoolEntry = {
        ...entry,
        state: "busy",
        consumedAt: selectedAt,
        borrowedBy: principal.owner,
        borrowedAt: selectedAt,
        borrowToken: newToken,
        borrowHeartbeatRequired: true,
        borrowHeartbeatAt: selectedAt,
        borrowExpiresAt: allocation.claimExpiresAt!,
        updatedAt: selectedAt,
        lastUsedAt: selectedAt,
      };
      const consumed: LeaseRecord = {
        ...lease,
        readyPoolConsumedAt: selectedAt,
        readyPoolConsumedKey: poolKey,
      };
      // This marker survives entry garbage collection and prevents dirty re-registration.
      // oxlint-disable-next-line eslint/no-await-in-loop -- Lease, borrow and operation publish as one transaction.
      await transaction.put(`lease:${lease.id}`, consumed);
      // oxlint-disable-next-line eslint/no-await-in-loop -- Same atomic reservation.
      await transaction.put(entryKey(poolKey, lease.id), borrowed);
      // oxlint-disable-next-line eslint/no-await-in-loop -- Classify warm before any claim outcome is known.
      await transaction.put(key, allocation);
      return { allocation, lease: consumed, reserved: !input.selectOnly };
    }
    if (!input.coldLeaseID) throw new ReadyPoolClaimError("no_ready_lease");
    const allocation: StoredAllocation = {
      ...common,
      kind: "cold",
      phase: "selected",
      leaseID: input.coldLeaseID,
      reasonCode: "pool-miss",
    };
    await transaction.put(key, allocation);
    return { allocation };
  });
  if (reserved.reserved && reserved.lease) {
    const token = reserved.allocation.claimToken!;
    const tokenHash = await sha256Hex(token);
    let claimFailed = false;
    try {
      await params.claim(reserved.lease, tokenHash);
    } catch {
      claimFailed = true;
    }
    await atomic(async (transaction) => {
      const current = await transaction.get<StoredAllocation>(key);
      if (!current || current.claimToken !== token || current.phase !== "claiming") {
        throw new ReadyPoolClaimError("pool_claim_fenced");
      }
      const lease = await transaction.get<LeaseRecord>(`lease:${current.leaseID}`);
      const entry = await transaction.get<ReadyPoolEntry>(entryKey(poolKey, current.leaseID));
      const valid =
        lease?.state === "active" &&
        !lease.cleanupStartedAt &&
        Date.parse(lease.expiresAt) > now() &&
        lease.createAttemptGeneration === current.leaseGeneration &&
        matches(lease) &&
        entry?.state === "busy" &&
        entry.borrowToken === token &&
        Date.parse(current.claimExpiresAt ?? "") > now();
      const settledAt = new Date(now()).toISOString();
      await transaction.put(
        key,
        claimFailed || !valid
          ? await quarantine(
              transaction,
              current,
              settledAt,
              claimFailed ? "pool-claim-failed" : "pool-claim-fenced",
            )
          : { ...current, phase: "ready", settledAt },
      );
    });
  }
  const settled = (await storage.get<StoredAllocation>(key)) ?? reserved.allocation;
  const allocation = publicAllocation(settled);
  if (
    input.lookupOnly ||
    (input.selectOnly && allocation.phase === "selected") ||
    allocation.kind === "cold"
  )
    return { allocation };
  if (allocation.phase !== "ready")
    throw new ReadyPoolClaimError(
      allocation.reasonCode === "pool-hit" ? "pool_claim_pending" : allocation.reasonCode,
      allocation,
    );
  const lease = await storage.get<LeaseRecord>(`lease:${allocation.leaseID}`);
  const entry = await storage.get<ReadyPoolEntry>(entryKey(poolKey, allocation.leaseID));
  if (
    !lease ||
    lease.state !== "active" ||
    lease.cleanupStartedAt ||
    Date.parse(lease.expiresAt) <= now() ||
    lease.createAttemptGeneration !== allocation.leaseGeneration ||
    !matches(lease) ||
    entry?.state !== "busy" ||
    entry.borrowToken !== settled.claimToken ||
    Date.parse(entry.borrowExpiresAt ?? "") <= now()
  ) {
    throw new ReadyPoolClaimError("pool_claim_fenced", {
      ...allocation,
      phase: "failed",
      reasonCode: "pool-claim-fenced",
    });
  }
  return { allocation, lease, entry };
}
