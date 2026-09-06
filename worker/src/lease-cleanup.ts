import type { LeaseRecord } from "./types";

export function clearLeaseCleanupCompletion(lease: LeaseRecord): void {
  delete lease.cleanupCompletedAt;
}

export function completeLeaseProviderCleanup(lease: LeaseRecord, completedAt: string): void {
  lease.cleanupCompletedAt = completedAt;
  lease.host = "";
  delete lease.tailscale;
  delete lease.sshHostKey;
  delete lease.providerAccessExpiresAt;
}

export function leaseProviderCleanupCompleted(record: LeaseRecord): boolean {
  return (
    record.state === "released" &&
    record.releaseDeletesServer !== false &&
    Number.isFinite(Date.parse(record.cleanupCompletedAt ?? "")) &&
    !record.cleanupStartedAt &&
    !record.cleanupError &&
    !record.cleanupRetryAt &&
    !record.cleanupFailedAt &&
    !record.cleanupAttempts &&
    !record.provisioningRequestStartedAt &&
    !record.provisioningRequestSettledAt &&
    !record.provisioningRecoveryObservedAt &&
    !record.provisioningRecoveryMissingSince &&
    record.provisioningResourceMayExist !== true &&
    record.provisioningFailureRetryable !== true &&
    !record.providerKeyCleanupPending
  );
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
