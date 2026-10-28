import { sanitizeAWSRegion } from "./aws-region";
import { validAWSRootDiskIdentity } from "./aws-root-disk";
import { sha256Hex } from "./encoding";
import { waitForProviderSignal } from "./provider-deadline";
import { providerKeyForLease } from "./provider-key";
import { providerLabelsOwnedByLease } from "./provider-labels";
import { ProviderResourceUnresolvedError } from "./provider-provisioning";
import type { AWSRootDiskIdentity, LeaseRecord } from "./types";

export interface AWSLegacyCleanupAudit {
  leaseID: string;
  region: string;
  providerScope: string;
  cloudID: string;
  eventID: string;
  eventTime: string;
  actor: string;
  recoveredAt: string;
  claimFingerprint: string;
}

export type AWSLegacyAllocationEvidence = Pick<
  AWSLegacyCleanupAudit,
  "region" | "providerScope" | "eventID" | "eventTime"
>;

const eventHistoryMs = 90 * 24 * 60 * 60_000;
const maxLookupPages = 10;
const maxLookupBytes = 2 * 1024 * 1024;
const lookupErrorCodes = new Set([
  "AccessDenied",
  "AccessDeniedException",
  "IncompleteSignature",
  "InvalidAction",
  "InvalidClientTokenId",
  "InvalidEventCategoryException",
  "InvalidLookupAttributesException",
  "InvalidMaxResultsException",
  "InvalidNextTokenException",
  "InvalidSignatureException",
  "InvalidTimeRangeException",
  "MissingAuthenticationToken",
  "OperationNotPermittedException",
  "RequestExpired",
  "Throttling",
  "ThrottlingException",
  "UnrecognizedClientException",
  "UnsupportedOperationException",
]);

export function awsCleanupRecoveryAuditKey(leaseID: string): string {
  return `aws-cleanup-recovery-audit:${leaseID}`;
}

// Exclude mutable cleanup/access progress so the audit remains bound after normal cleanup.
export function awsCleanupRecoveryFingerprint(lease: LeaseRecord): Promise<string> {
  return sha256Hex(
    JSON.stringify([
      lease.id,
      lease.provider,
      lease.lifecycle,
      lease.cloudID,
      lease.region,
      lease.owner,
      lease.org,
      lease.providerOwner,
      lease.slug,
      lease.createdAt,
      lease.providerKey,
      lease.providerKeyCleanupOwned,
      lease.hostId,
      lease.hostID,
      lease.createAttemptID,
      lease.createAttemptGeneration,
    ]),
  );
}

export function requireAWSLegacyCleanupLease(lease: LeaseRecord): void {
  if (
    lease.provider !== "aws" ||
    lease.lifecycle === "registered" ||
    !/^cbx_[a-f0-9]{12}$/.test(lease.id) ||
    !/^i-[a-f0-9]{8,17}$/.test(lease.cloudID) ||
    !lease.region ||
    sanitizeAWSRegion(lease.region) !== lease.region ||
    lease.providerScope != null ||
    lease.state !== "released" ||
    // Explicit release deletion supersedes the acquisition's default retention flag.
    lease.releaseDeletesServer !== true ||
    lease.cleanupStartedAt ||
    lease.provisioningRequestStartedAt ||
    lease.cleanupCompletedAt ||
    !lease.cleanupError ||
    !(Date.parse(lease.expiresAt) <= Date.now()) ||
    lease.providerKey !== providerKeyForLease(lease.id) ||
    lease.providerKeyCleanupOwned !== true
  ) {
    throw new ProviderResourceUnresolvedError(
      "AWS scope recovery requires an expired, released legacy lease with exact instance, Region, owned key, and no active cleanup or allocation",
    );
  }
}

export async function awsCleanupAuditMatchesLease(
  audit: AWSLegacyCleanupAudit,
  lease: LeaseRecord,
): Promise<boolean> {
  return (
    audit.leaseID === lease.id &&
    audit.cloudID === lease.cloudID &&
    audit.region === lease.region &&
    audit.providerScope === lease.providerScope &&
    /^aws:account:\d{12}$/.test(audit.providerScope) &&
    /^[a-f0-9-]{36}$/.test(audit.eventID) &&
    Number.isFinite(Date.parse(audit.eventTime)) &&
    Number.isFinite(Date.parse(audit.recoveredAt)) &&
    typeof audit.actor === "string" &&
    audit.actor.trim().length > 0 &&
    audit.actor.length <= 256 &&
    audit.claimFingerprint === (await awsCleanupRecoveryFingerprint(lease))
  );
}

export function awsCleanupAuditView(audit: AWSLegacyCleanupAudit): AWSLegacyCleanupAudit {
  return {
    leaseID: audit.leaseID,
    region: audit.region,
    providerScope: audit.providerScope,
    cloudID: audit.cloudID,
    eventID: audit.eventID,
    eventTime: audit.eventTime,
    actor: audit.actor,
    recoveredAt: audit.recoveredAt,
    claimFingerprint: audit.claimFingerprint,
  };
}

export async function lookupAWSLegacyAllocation(
  lease: LeaseRecord,
  account: string,
  fetch: (input: string, init: RequestInit) => Promise<Response>,
): Promise<AWSLegacyAllocationEvidence> {
  requireAWSLegacyCleanupLease(lease);
  return lookupAllocation(lease, account, fetch);
}

export interface AWSOriginalRootEvidence extends AWSLegacyAllocationEvidence {
  disk: AWSRootDiskIdentity;
}

// Read-only recovery: no caller-supplied disk, tag search, live replacement, or
// absence inference. Only the original, authenticated RunInstances response.
export async function lookupAWSOriginalRootDisk(
  lease: LeaseRecord,
  account: string,
  fetch: (input: string, init: RequestInit) => Promise<Response>,
): Promise<AWSOriginalRootEvidence> {
  if (
    lease.provider !== "aws" ||
    lease.lifecycle === "registered" ||
    !/^cbx_[a-f0-9]{12}$/.test(lease.id) ||
    !/^i-[a-f0-9]{8,17}$/.test(lease.cloudID) ||
    !lease.region ||
    sanitizeAWSRegion(lease.region) !== lease.region ||
    lease.providerScope !== "aws:account:" + account ||
    lease.providerKey !== providerKeyForLease(lease.id) ||
    lease.providerKeyCleanupOwned !== true
  ) {
    throw new ProviderResourceUnresolvedError(
      "original root recovery requires an exact owned lease and authenticated account scope",
    );
  }
  const evidence = await lookupAllocation(lease, account, fetch, true);
  if (!evidence.disk)
    throw new ProviderResourceUnresolvedError(
      "original root disk is absent from allocation evidence",
    );
  return { ...evidence, disk: evidence.disk };
}

async function lookupAllocation(
  lease: LeaseRecord,
  account: string,
  fetch: (input: string, init: RequestInit) => Promise<Response>,
  requireRootDisk = false,
): Promise<AWSLegacyAllocationEvidence & { disk?: AWSRootDiskIdentity }> {
  const start = Math.floor(Date.parse(lease.createdAt) / 1000) * 1000;
  const end = Date.parse(
    lease.releasedAt ?? lease.endedAt ?? (requireRootDisk ? new Date().toISOString() : ""),
  );
  if (
    !/^\d{12}$/.test(account) ||
    !Number.isFinite(start) ||
    !Number.isFinite(end) ||
    start < Date.now() - eventHistoryMs ||
    end < start ||
    end > Date.now()
  ) {
    throw new ProviderResourceUnresolvedError(
      "AWS scope recovery requires an original allocation interval within CloudTrail's 90-day event history",
    );
  }
  const body = {
    LookupAttributes: [{ AttributeKey: "ResourceName", AttributeValue: lease.cloudID }],
    StartTime: start / 1000,
    EndTime: end / 1000,
    MaxResults: 50,
  };
  const evidence = new Map<string, AWSLegacyAllocationEvidence & { disk?: AWSRootDiskIdentity }>();
  const tokens = new Set<string>();
  let nextToken: string | undefined;
  let remainingBytes = maxLookupBytes;
  for (let page = 0; page < maxLookupPages; page++) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- CloudTrail pagination must retain one exact lookup and credential snapshot.
    const response = await fetch(`https://cloudtrail.${lease.region}.amazonaws.com/`, {
      method: "POST",
      headers: {
        "content-type": "application/x-amz-json-1.1",
        "x-amz-target": "com.amazonaws.cloudtrail.v20131101.CloudTrail_20131101.LookupEvents",
      },
      body: JSON.stringify({ ...body, ...(nextToken ? { NextToken: nextToken } : {}) }),
      signal: AbortSignal.timeout(15_000),
    }).catch(() => {
      throw new ProviderResourceUnresolvedError(
        "AWS CloudTrail lookup could not be completed; no scope was recovered",
      );
    });
    if (!response.ok) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- Classify and close this failed page without another provider request.
      const code = await lookupErrorCode(response);
      throw new ProviderResourceUnresolvedError(
        `AWS CloudTrail LookupEvents failed (HTTP ${response.status}${code ? `, code ${code}` : ""}); verify existing read permission and original account evidence`,
      );
    }
    // oxlint-disable-next-line eslint/no-await-in-loop -- Each response is bounded before its next page is admitted.
    const payload = await readLookupResponse(response, remainingBytes);
    remainingBytes -= payload.bytes;
    const result = record(payload.value);
    if (!Array.isArray(result["Events"]) || result["Events"].length > 50) invalidEvidence();
    for (const value of result["Events"]) {
      const summary = record(value);
      if (summary["EventName"] !== "RunInstances") continue;
      const observed = allocationEvidence(summary, lease, account, start, end, requireRootDisk);
      const previous = evidence.get(observed.eventID);
      if (previous && JSON.stringify(previous) !== JSON.stringify(observed)) invalidEvidence();
      evidence.set(observed.eventID, observed);
    }
    if (result["NextToken"] === undefined || result["NextToken"] === "") {
      if (evidence.size !== 1) {
        throw new ProviderResourceUnresolvedError(
          "AWS CloudTrail did not establish one unambiguous original allocation; no scope was recovered",
        );
      }
      return evidence.values().next().value!;
    }
    if (
      typeof result["NextToken"] !== "string" ||
      result["NextToken"].length > 16_384 ||
      tokens.has(result["NextToken"])
    )
      invalidEvidence();
    nextToken = result["NextToken"];
    tokens.add(nextToken);
    // oxlint-disable-next-line eslint/no-await-in-loop -- CloudTrail permits two lookups per second per account and Region.
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new ProviderResourceUnresolvedError(
    "AWS CloudTrail lookup exceeded the bounded evidence window; no scope was recovered",
  );
}

function knownLookupErrorCode(value: unknown): string | undefined {
  if (typeof value !== "string" || value.length > 256) return undefined;
  const code = value.split(":")[0]?.split("#").at(-1);
  return code && lookupErrorCodes.has(code) ? code : undefined;
}

async function lookupErrorCode(response: Response): Promise<string | undefined> {
  const signal = AbortSignal.timeout(1_000);
  try {
    const header = response.headers.get("x-amzn-errortype");
    if (header) {
      const cancellation = response.body?.cancel().catch(() => undefined);
      await waitForProviderSignal(signal, async () => cancellation).catch(() => undefined);
      return knownLookupErrorCode(header);
    }
    const { value } = await readLookupResponse(response, 4_096, signal);
    const body = record(value);
    // Emit only fixed enums, never raw messages, request IDs, events, or unknown strings.
    return knownLookupErrorCode(body["code"] ?? body["__type"]);
  } catch {
    return undefined;
  }
}

function allocationEvidence(
  summary: Record<string, unknown>,
  lease: LeaseRecord,
  account: string,
  start: number,
  end: number,
  requireRootDisk: boolean,
): AWSLegacyAllocationEvidence & { disk?: AWSRootDiskIdentity } {
  if (typeof summary["CloudTrailEvent"] !== "string") invalidEvidence();
  let event: Record<string, unknown>;
  try {
    event = record(JSON.parse(summary["CloudTrailEvent"]));
  } catch {
    return invalidEvidence();
  }
  const eventTime = typeof event["eventTime"] === "string" ? Date.parse(event["eventTime"]) : NaN;
  const response = record(event["responseElements"]);
  const instances = record(response["instancesSet"])["items"];
  const request = record(event["requestParameters"]);
  const specifications = record(request["tagSpecificationSet"])["items"];
  if (
    event["eventName"] !== "RunInstances" ||
    event["eventSource"] !== "ec2.amazonaws.com" ||
    event["eventType"] !== "AwsApiCall" ||
    event["errorCode"] ||
    event["errorMessage"] ||
    event["recipientAccountId"] !== account ||
    event["awsRegion"] !== lease.region ||
    typeof event["eventID"] !== "string" ||
    !/^[a-f0-9-]{36}$/.test(event["eventID"]) ||
    summary["EventId"] !== event["eventID"] ||
    summary["EventSource"] !== event["eventSource"] ||
    !Number.isFinite(eventTime) ||
    eventTime < start ||
    eventTime > end ||
    !Array.isArray(instances) ||
    instances.length !== 1 ||
    record(instances[0])["instanceId"] !== lease.cloudID ||
    request["keyName"] !== lease.providerKey ||
    !Array.isArray(specifications)
  )
    invalidEvidence();
  const instanceSpecifications = specifications
    .map(record)
    .filter((item) => item["resourceType"] === "instance");
  if (instanceSpecifications.length !== 1) invalidEvidence();
  const tags = instanceSpecifications[0]!["tags"];
  if (!Array.isArray(tags) || tags.length > 100) invalidEvidence();
  const labels: Record<string, string> = Object.create(null) as Record<string, string>;
  for (const value of tags) {
    const tag = record(value);
    if (
      typeof tag["key"] !== "string" ||
      typeof tag["value"] !== "string" ||
      Object.hasOwn(labels, tag["key"])
    )
      invalidEvidence();
    labels[tag["key"]] = tag["value"];
  }
  if (
    !providerLabelsOwnedByLease(labels, lease, "aws") ||
    labels["provider_key"] !== lease.providerKey
  )
    invalidEvidence();
  let disk: AWSRootDiskIdentity | undefined;
  if (requireRootDisk) {
    const instance = record(instances[0]);
    const rootName = instance["rootDeviceName"];
    const mappings = record(instance["blockDeviceMapping"])["items"];
    const roots = Array.isArray(mappings)
      ? mappings.map(record).filter((mapping) => mapping["deviceName"] === rootName)
      : [];
    const candidate = {
      leaseID: lease.id,
      instanceID: lease.cloudID,
      region: lease.region,
      providerScope: "aws:account:" + account,
      deviceName: rootName,
      volumeID: record(roots[0]?.["ebs"])["volumeId"],
    };
    if (roots.length !== 1 || !validAWSRootDiskIdentity(candidate)) {
      throw new ProviderResourceUnresolvedError(
        "original root disk is absent or ambiguous in the authenticated RunInstances response; original provider custody evidence is required",
      );
    }
    disk = candidate;
  }
  return {
    ...(disk ? { disk } : {}),
    region: lease.region!,
    providerScope: `aws:account:${account}`,
    eventID: event["eventID"],
    eventTime: new Date(eventTime).toISOString(),
  };
}

function record(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function invalidEvidence(): never {
  throw new ProviderResourceUnresolvedError(
    "AWS CloudTrail allocation evidence is incomplete or conflicts with the retained lease; no scope was recovered",
  );
}

async function readLookupResponse(
  response: Response,
  limit: number,
  signal?: AbortSignal,
): Promise<{ value: unknown; bytes: number }> {
  if (!response.body || limit <= 0) invalidEvidence();
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let bytes = 0;
  try {
    while (true) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- Bound the streamed CloudTrail response before JSON parsing.
      const chunk = await (signal
        ? waitForProviderSignal(signal, () => reader.read())
        : reader.read());
      if (chunk.done) break;
      bytes += chunk.value.byteLength;
      if (bytes > limit) invalidEvidence();
      chunks.push(chunk.value);
    }
  } finally {
    if (signal) {
      const cancellation = reader.cancel().catch(() => undefined);
      await waitForProviderSignal(signal, () => cancellation).catch(() => undefined);
    } else {
      await reader.cancel();
    }
    reader.releaseLock();
  }
  const data = new Uint8Array(bytes);
  let offset = 0;
  for (const chunk of chunks) {
    data.set(chunk, offset);
    offset += chunk.byteLength;
  }
  try {
    return { value: JSON.parse(new TextDecoder().decode(data)), bytes };
  } catch {
    return invalidEvidence();
  }
}
