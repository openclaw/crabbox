import {
  MISSING_ORG_KEY,
  isCurrentOrgKey,
  isLegacyOrgKey,
  orgLabelForDisplay,
} from "./org-identity";
import type { ExternalRunnerRecord, LeaseRecord, ReadyPoolEntry, RunRecord } from "./types";

export type PortalOrgKind = "missing" | "legacy" | "unsupported";
export type PortalLeaseRecord = LeaseRecord & { portalOrgKind?: PortalOrgKind };
export type PortalExternalRunnerRecord = ExternalRunnerRecord & {
  portalOrgKind?: PortalOrgKind;
};

export function publicLeaseRecord(record: LeaseRecord): LeaseRecord {
  const publicRecord = {
    ...record,
    org: orgLabelForDisplay(record.org),
  };
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

export function publicRunRecord(record: RunRecord): RunRecord {
  const publicRecord = {
    ...record,
    org: orgLabelForDisplay(record.org),
  };
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
