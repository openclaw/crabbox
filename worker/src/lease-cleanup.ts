import { lifetimeRuntimeMs } from "./lease-lifetime";
import type { LeaseRecord } from "./types";

// Outlives the longest in-flight create that could still change resources:
// 25 minutes creating, 5 minutes deploy settling, and 30 minutes confirming absence.
// A transient failure between refusals does not reset the streak; a coordinator
// outage can shorten the observed number of attempts in this time-based window.
export const leaseCleanupRefusalTerminalMs = 2 * 60 * 60 * 1000;

export function leaseCleanupBlockedMessage(name: string, refusal: string, since: string): string {
  return `${refusal}; cleanup refused since ${since}; automatic retries stopped; inspect resource ${name}, delete or restore it, then run crabbox stop again`;
}

export function clearLeaseCleanupCompletion(lease: LeaseRecord): void {
  delete lease.cleanupCompletedAt;
  delete lease.cleanupConfirmedBy;
  delete lease.retainedReconcileDeleteClaim;
  delete lease.retainedReconcileDeleteCredentialFingerprint;
  delete lease.retainedReconcileObservation;
}

export function completeLeaseProviderCleanup(lease: LeaseRecord, completedAt: string): void {
  if (lease.lifetimePolicy?.compute) {
    lease.lifetimePolicy.compute = {
      state: "stopped",
      accumulatedMs: lifetimeRuntimeMs(lease, Date.parse(completedAt))!,
    };
  }
  if (lease.lifetimePolicy && lease.lifetimePolicy.status !== "complete") {
    // Explicit deletion retires the policy; a stale stopping phase would keep warning the owner.
    lease.lifetimePolicy.status = "complete";
    delete lease.lifetimePolicy.nextCheckAt;
  }
  delete lease.cleanupRefusedSince;
  delete lease.cleanupBlockedAt;
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
    !record.cleanupRefusedSince &&
    !record.cleanupBlockedAt &&
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
