import {
  MISSING_ORG_KEY,
  isCurrentOrgKey,
  isLegacyOrgKey,
  orgLabelForDisplay,
} from "./org-identity";
import type { ExternalRunnerRecord, LeaseRecord, ReadyPoolEntry, RunRecord } from "./types";

export type PortalOrgKind = "missing" | "legacy" | "unsupported";
type LeaseCleanupStatus = "pending" | "failed" | "complete" | "retained";
export type PublicLeaseRecord = LeaseRecord & { cleanupStatus?: LeaseCleanupStatus };
export type PortalLeaseRecord = PublicLeaseRecord & { portalOrgKind?: PortalOrgKind };
export type PortalExternalRunnerRecord = ExternalRunnerRecord & {
  portalOrgKind?: PortalOrgKind;
};

export function publicLeaseRecord(record: LeaseRecord): PublicLeaseRecord {
  const publicRecord: PublicLeaseRecord = {
    ...record,
    org: orgLabelForDisplay(record.org),
  };
  if (record.state === "released") {
    publicRecord.cleanupStatus = releaseCleanupStatus(record);
  }
  delete publicRecord.provisioningCoordinatorVersion;
  delete publicRecord.provisioningRequestSettledAt;
  delete publicRecord.provisioningRecoveryObservedAt;
  delete publicRecord.provisioningRecoveryMissingSince;
  delete publicRecord.fixedCreateIntentVersion;
  delete publicRecord.fixedCreateIntentHash;
  delete publicRecord.createAttemptID;
  delete publicRecord.createAttemptGeneration;
  return publicRecord;
}

function releaseCleanupStatus(record: LeaseRecord): LeaseCleanupStatus {
  if (record.releaseDeletesServer === false) return "retained";
  // A retry's current claim supersedes historical failure diagnostics; failures
  // clear that claim. This is an observation, never provider deletion authority.
  if (record.cleanupStartedAt) {
    return Number.isFinite(Date.parse(record.cleanupStartedAt)) ? "pending" : "failed";
  }
  if (
    record.releaseDeletesServer === true &&
    Number.isFinite(Date.parse(record.provisioningRequestStartedAt ?? "")) &&
    !record.provisioningRequestSettledAt &&
    !record.provisioningRecoveryObservedAt &&
    !record.provisioningRecoveryMissingSince &&
    !record.failureError &&
    !record.cleanupFailedAt &&
    !record.cleanupAttempts
  ) {
    // Release can precede the allocation response. Keep legacy diagnostics for
    // older clients while current observers wait for the create owner's outcome.
    return "pending";
  }
  return record.cleanupError ||
    record.cleanupRetryAt ||
    record.cleanupFailedAt ||
    record.cleanupAttempts ||
    record.provisioningRequestStartedAt ||
    record.provisioningRequestSettledAt ||
    record.provisioningRecoveryObservedAt ||
    record.provisioningRecoveryMissingSince ||
    record.provisioningResourceMayExist ||
    record.providerKeyCleanupPending
    ? "failed"
    : "complete";
}

export function publicRunRecord(record: RunRecord): RunRecord {
  const publicRecord = {
    ...record,
    org: orgLabelForDisplay(record.org),
  };
  delete publicRecord.terminalReceipt;
  delete publicRecord.terminalFinishSHA256;
  delete publicRecord.terminalLogPrefix;
  if (!record.leaseOwners) {
    return publicRecord;
  }
  return {
    ...publicRecord,
    leaseOwners: record.leaseOwners.map((owner) => ({
      ...owner,
      org: orgLabelForDisplay(owner.org),
    })),
  };
}

export function publicReadyPoolEntry(entry: ReadyPoolEntry): ReadyPoolEntry {
  return {
    ...entry,
    org: orgLabelForDisplay(entry.org),
  };
}

export function publicExternalRunnerRecord(record: ExternalRunnerRecord): ExternalRunnerRecord {
  return {
    ...record,
    org: orgLabelForDisplay(record.org),
  };
}

/** Portal rows carry a non-secret identity kind so display labels never drive authorization links. */
export function portalLeaseRecord(record: LeaseRecord): PortalLeaseRecord {
  const publicRecord = publicLeaseRecord(record);
  const portalOrgKind = portalOrgKindForKey(record.org);
  return portalOrgKind ? { ...publicRecord, portalOrgKind } : publicRecord;
}

export function portalExternalRunnerRecord(
  record: ExternalRunnerRecord,
): PortalExternalRunnerRecord {
  const publicRecord = {
    ...record,
    org: orgLabelForDisplay(record.org),
  };
  const portalOrgKind = portalOrgKindForKey(record.org);
  return portalOrgKind ? { ...publicRecord, portalOrgKind } : publicRecord;
}

function portalOrgKindForKey(key: string): PortalOrgKind | undefined {
  if (key === MISSING_ORG_KEY) return "missing";
  if (isCurrentOrgKey(key)) return undefined;
  return isLegacyOrgKey(key) ? "legacy" : "unsupported";
}
