import type { LeaseRecord } from "./types";

export function clearLeaseCleanupCompletion(lease: LeaseRecord): void {
  delete lease.cleanupCompletedAt;
}

export function completeLeaseProviderCleanup(lease: LeaseRecord, completedAt: string): void {
  lease.cleanupCompletedAt = completedAt;
  lease.provisioningResourceMayExist = false;
  delete lease.provisioningFailureRetryable;
  lease.host = "";
  delete lease.tailscale;
  delete lease.sshHostKey;
  delete lease.providerAccessExpiresAt;
}

// Provider cleanup may finish while a lease remains failed or expired. Keep this
// state-independent so a late create result cannot republish retired access.
export function leaseProviderCleanupConfirmed(record: LeaseRecord): boolean {
  return (
    record.releaseDeletesServer !== false &&
    Number.isFinite(Date.parse(record.cleanupCompletedAt ?? "")) &&
    !record.cleanupStartedAt &&
    !record.cleanupClaimExpiresAt &&
    !record.cleanupError &&
    !record.cleanupRetryAt &&
    !record.cleanupFailedAt &&
    !record.cleanupAttempts &&
    !record.provisioningRequestStartedAt &&
    !record.provisioningRequestSettledAt &&
    !record.provisioningCoordinatorVersion &&
    !record.provisioningRecoveryObservedAt &&
    !record.provisioningRecoveryMissingSince &&
    record.provisioningResourceMayExist !== true &&
    record.provisioningFailureRetryable !== true &&
    !record.providerKeyCleanupPending &&
    !record.providerKeyCleanupID &&
    record.host === "" &&
    record.tailscale === undefined &&
    record.sshHostKey === undefined &&
    record.providerAccessExpiresAt === undefined
  );
}

export function leaseProviderCleanupCompleted(record: LeaseRecord): boolean {
  return record.state === "released" && leaseProviderCleanupConfirmed(record);
}

export function leaseHasConfirmedNoProviderResource(record: LeaseRecord): boolean {
  return (
    !record.cloudID &&
    !record.providerKeyCleanupPending &&
    record.provisioningResourceMayExist === false &&
    !record.provisioningRequestStartedAt &&
    !record.provisioningRequestSettledAt &&
    !record.provisioningRecoveryObservedAt &&
    !record.provisioningRecoveryMissingSince
  );
}
