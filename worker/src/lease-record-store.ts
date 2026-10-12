import type { CoordinatorStorageView } from "./coordinator-runtime";
import { leaseProviderCleanupConfirmed } from "./lease-cleanup";
import { isRegisteredLease, leaseIsLive } from "./lease-state";
import type { LeaseRecord } from "./types";

export const leaseMaintenancePrefix = "lease-maintenance:";
export const leaseMaintenanceKey = (leaseID: string): string =>
  `${leaseMaintenancePrefix}${leaseID}`;

export const leaseCurrentPrefix = "lease-current:";
export const leaseCurrentKey = (leaseID: string): string => `${leaseCurrentPrefix}${leaseID}`;

export function leaseIsListedCurrent(lease: LeaseRecord): boolean {
  return Boolean(
    leaseIsLive(lease) ||
    ((lease.keep || lease.releaseDeletesServer === false) && !leaseProviderCleanupConfirmed(lease)),
  );
}

export interface LeaseIndexDescriptor {
  name: "maintenance" | "current";
  prefix: string;
  stateKey: string;
  version: number;
  member: (lease: LeaseRecord) => boolean;
}

export const leaseMaintenanceIndex: LeaseIndexDescriptor = {
  name: "maintenance",
  prefix: leaseMaintenancePrefix,
  stateKey: "lease-maintenance-state",
  version: 1,
  member: leaseNeedsMaintenanceIndex,
};
export const leaseCurrentIndex: LeaseIndexDescriptor = {
  name: "current",
  prefix: leaseCurrentPrefix,
  stateKey: "lease-current-state",
  version: 1,
  member: leaseIsListedCurrent,
};
export const leaseIndexes = [leaseMaintenanceIndex, leaseCurrentIndex] as const;

// Time-independent superset: elapsed deadlines must never make excluded history
// eligible without a lease write. Keep uncertain custody, even when not due yet.
export function leaseNeedsMaintenanceIndex(lease: LeaseRecord): boolean {
  return Boolean(
    leaseIsLive(lease) ||
    lease.cleanupStartedAt ||
    lease.cleanupClaimExpiresAt ||
    lease.cleanupError ||
    lease.cleanupRetryAt ||
    lease.cleanupFailedAt ||
    lease.providerKeyCleanupPending ||
    lease.providerKeyCleanupID ||
    lease.provisioningResourceMayExist ||
    (lease.state === "released" &&
      lease.releaseDeletesServer === true &&
      lease.provisioningRequestStartedAt) ||
    lease.runtimeAdapterDeleteRequestedAt ||
    (lease.lifetimePolicy && lease.lifetimePolicy.status !== "complete") ||
    (lease.provider === "aws" &&
      !isRegisteredLease(lease) &&
      !lease.network?.awsPrivate &&
      lease.releaseDeletesServer === false),
  );
}

export async function putLeaseRecord(
  storage: CoordinatorStorageView,
  lease: LeaseRecord,
  options?: { noCache?: boolean },
): Promise<void> {
  const puts: (() => Promise<unknown>)[] = [];
  const deletes: (() => Promise<unknown>)[] = [];
  for (const index of leaseIndexes) {
    const key = `${index.prefix}${lease.id}`;
    if (index.member(lease)) puts.push(() => storage.put(key, 1, options));
    else deletes.push(() => storage.delete(key));
  }
  await writeInSupersetOrder(storage, [
    ...puts,
    () => storage.put(`lease:${lease.id}`, lease, options),
    ...deletes,
  ]);
}

export async function deleteLeaseRecord(
  storage: CoordinatorStorageView,
  leaseID: string,
): Promise<void> {
  await writeInSupersetOrder(storage, [
    () => storage.delete(`lease:${leaseID}`),
    ...leaseIndexes.map((index) => () => storage.delete(`${index.prefix}${leaseID}`)),
  ]);
}

// Durable Object storage commits operations issued without an intervening await
// as one atomic batch. Elsewhere (transaction views, PostgreSQL) run them in
// order and only after the previous one succeeded, so a failure or crash can
// leave a stale index entry but never a missing one.
async function writeInSupersetOrder(
  storage: CoordinatorStorageView,
  writes: (() => Promise<unknown>)[],
): Promise<void> {
  if (coalescesWrites(storage)) {
    await Promise.all(writes.map((write) => write()));
    return;
  }
  for (const write of writes) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- each write depends on the previous one succeeding.
    await write();
  }
}

function coalescesWrites(storage: CoordinatorStorageView): boolean {
  // SQLite-backed Durable Object storage; its transaction views are atomic anyway.
  return typeof (storage as { transactionSync?: unknown }).transactionSync === "function";
}
