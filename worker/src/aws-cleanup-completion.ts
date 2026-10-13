import { providerKeyForLease } from "./provider-key";
import type { LeaseRecord } from "./types";

export function awsLeaseOwnsKey(lease: LeaseRecord): boolean {
  return (
    lease.providerKeyCleanupOwned === true &&
    /^cbx_[a-f0-9]{12}$/.test(lease.id) &&
    lease.providerKey === providerKeyForLease(lease.id)
  );
}

// Release receipts and read-only reconciliation must discharge the same obligations.
export function awsCleanupCompletionBlocker(
  lease: LeaseRecord,
  evidence: {
    instanceComplete: boolean;
    keyComplete: boolean;
    volumeComplete: boolean;
  },
): string | undefined {
  if (!evidence.instanceComplete) return "instance_present";
  if (lease.lifetimePolicy?.retainedIdentity && !evidence.volumeComplete)
    return "retained_volume_present";
  if ((awsLeaseOwnsKey(lease) || lease.providerKeyCleanupPending) && !evidence.keyComplete)
    return "owned_key_present";
  return undefined;
}
