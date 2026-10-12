import type { CoordinatorStorageView } from "./coordinator-runtime";
import { isRegisteredLease, leaseIsLive } from "./lease-state";
import type { LeaseRecord } from "./types";

export const leaseMaintenancePrefix = "lease-maintenance:";
export const leaseMaintenanceKey = (leaseID: string): string =>
  `${leaseMaintenancePrefix}${leaseID}`;

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
  const key = leaseMaintenanceKey(lease.id);
  const member = leaseNeedsMaintenanceIndex(lease);
  const leaseWrite = () => storage.put(`lease:${lease.id}`, lease, options);
  const indexWrite = () => (member ? storage.put(key, 1, options) : storage.delete(key));
  await writeInSupersetOrder(storage, member ? [indexWrite, leaseWrite] : [leaseWrite, indexWrite]);
}

export async function deleteLeaseRecord(
  storage: CoordinatorStorageView,
  leaseID: string,
): Promise<void> {
  await writeInSupersetOrder(storage, [
    () => storage.delete(`lease:${leaseID}`),
    () => storage.delete(leaseMaintenanceKey(leaseID)),
  ]);
}

// Durable Object storage commits operations issued without an intervening await
// as one atomic batch. Elsewhere (transaction views, PostgreSQL) run them in
// order and only after the previous one succeeded, so a failure or crash can
// leave a stale index entry but never a missing one.
async function writeInSupersetOrder(
  storage: CoordinatorStorageView,
  writes: [() => Promise<unknown>, () => Promise<unknown>],
): Promise<void> {
  if (coalescesWrites(storage)) {
    await Promise.all(writes.map((write) => write()));
    return;
  }
  for (const write of writes) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- the second write depends on the first succeeding.
    await write();
  }
}

function coalescesWrites(storage: CoordinatorStorageView): boolean {
  // SQLite-backed Durable Object storage; its transaction views are atomic anyway.
  return typeof (storage as { transactionSync?: unknown }).transactionSync === "function";
}
