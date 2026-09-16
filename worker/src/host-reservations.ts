import type { CoordinatorStorageView } from "./coordinator-runtime";
import { leaseProviderCleanupConfirmed } from "./lease-cleanup";
import { leaseIsLive } from "./lease-state";
import { publicLeaseRecord } from "./org-records";
import { coordinatorStorageEntries } from "./storage-scan";
import type { LeaseRecord, Provider } from "./types";

export interface HostScope {
  provider: Provider;
  hostID: string;
  region: string | undefined;
}

export interface HostReservation {
  storageKey: string;
  record: LeaseRecord;
  lease?: LeaseRecord;
  staleReason:
    | undefined
    | "lease_missing"
    | "host_changed"
    | "cleanup_complete"
    | "expired_without_instance";
}

function matchesHost(lease: LeaseRecord, scope: HostScope): boolean {
  return (
    lease.provider === scope.provider &&
    (lease.hostId || lease.hostID) === scope.hostID &&
    (!lease.region || lease.region === scope.region)
  );
}

export function hostReservationStaleReason(
  lease: LeaseRecord | undefined,
  scope: HostScope,
): HostReservation["staleReason"] {
  if (!lease) return "lease_missing";
  if (!matchesHost(lease, scope)) return "host_changed";
  // A create owns its host before a provider instance ID has been committed.
  if (leaseIsLive(lease)) return undefined;
  if (leaseProviderCleanupConfirmed(lease)) return "cleanup_complete";
  if (
    lease.state === "expired" &&
    !lease.cloudID &&
    !lease.serverID &&
    !lease.provisioningRequestStartedAt &&
    !lease.provisioningResourceMayExist
  )
    return "expired_without_instance";
  return undefined;
}

// Host associations live on lease rows and provider-access snapshots, not in a
// separate host index. Resolve snapshots by exact lease ID before trusting them.
export async function readHostReservations(
  storage: CoordinatorStorageView,
  scope: HostScope,
): Promise<HostReservation[]> {
  const reservations: HostReservation[] = [];
  for (const prefix of ["lease:", "provider-access:"]) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- finish one association namespace before reading the next.
    for await (const [storageKey, record] of coordinatorStorageEntries<LeaseRecord>(storage, {
      prefix,
      limit: 256,
      noCache: true,
    })) {
      if (!matchesHost(record, scope)) continue;
      const lease = await storage.get<LeaseRecord>(`lease:${record.id}`, { noCache: true });
      reservations.push({
        storageKey,
        record,
        ...(lease ? { lease } : {}),
        staleReason: hostReservationStaleReason(lease, scope),
      });
    }
  }
  return reservations;
}

export async function clearHostReservations(
  storage: CoordinatorStorageView,
  reservations: HostReservation[],
): Promise<void> {
  for (const reservation of reservations) {
    const record = { ...reservation.record };
    delete record.hostId;
    delete record.hostID;
    // Keep lease history, cloud identity, and provider cleanup obligations intact.
    // oxlint-disable-next-line eslint/no-await-in-loop -- callers hold the admission lock or storage transaction.
    await storage.put(reservation.storageKey, record);
  }
}

export function logClearedHostReservations(
  scope: HostScope,
  reservations: HostReservation[],
  action = "stale_reservation_cleared",
): void {
  for (const reservation of reservations) {
    console.info(
      JSON.stringify({
        component: "crabbox_host_reservation",
        action,
        ...scope,
        leaseID: reservation.record.id,
        storageKey: reservation.storageKey,
        reason: reservation.staleReason ?? "admin_force",
      }),
    );
  }
}

function hostReservationSummary(record: LeaseRecord) {
  return {
    id: record.id,
    slug: record.slug,
    provider: record.provider,
    region: record.region,
    hostId: record.hostId,
    hostID: record.hostID,
    state: record.state,
    cloudID: record.cloudID,
    createdAt: record.createdAt,
    expiresAt: record.expiresAt,
    keep: record.keep,
    releaseDeletesServer: record.releaseDeletesServer,
    cleanupStatus: publicLeaseRecord(record).cleanupStatus,
    cleanupCompletedAt: record.cleanupCompletedAt,
    cleanupStartedAt: record.cleanupStartedAt,
    cleanupRetryAt: record.cleanupRetryAt,
    cleanupFailedAt: record.cleanupFailedAt,
    providerKeyCleanupPending: record.providerKeyCleanupPending,
    provisioningRequestStartedAt: record.provisioningRequestStartedAt,
    provisioningResourceMayExist: record.provisioningResourceMayExist,
  };
}

export function publicHostReservation(reservation: HostReservation) {
  return {
    storageKey: reservation.storageKey,
    reservation: hostReservationSummary(reservation.record),
    lease: reservation.lease ? hostReservationSummary(reservation.lease) : null,
    stale: Boolean(reservation.staleReason),
    reason: reservation.staleReason ?? "lease_occupies_host",
  };
}
