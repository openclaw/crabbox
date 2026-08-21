import { sha256Hex } from "./auth";
import type { CoordinatorStorage, CoordinatorStorageView } from "./coordinator-runtime";
import { orgMatchesForAccounting, sameOrgIdentityKey } from "./org-identity";
import type {
  CoordinatorCheckpointDeleteClaim,
  CoordinatorCheckpointDueIndex,
  CoordinatorCheckpointEvent,
  CoordinatorCheckpointImage,
  CoordinatorCheckpointPin,
  CoordinatorCheckpointProvider,
  CoordinatorCheckpointRecord,
  CoordinatorCheckpointResourceClaim,
  CoordinatorCheckpointResourceIntent,
  CoordinatorCheckpointRetention,
  CoordinatorCheckpointScope,
  CoordinatorCheckpointUseClaim,
  Env,
  LeaseRecord,
  ProviderImage,
} from "./types";

export const checkpointUseClaimTTLMS = 120_000;
export const checkpointCreateClaimTTLMS = 15 * 60_000;
export const checkpointDeleteClaimTTLMS = 5 * 60_000;
export const checkpointAuditRetentionMS = 90 * 24 * 60 * 60_000;
export const checkpointMaxAuditEvents = 256;
export const checkpointDuePrefix = "checkpoint-due:";
const maxRetentionSeconds = 10 * 366 * 24 * 60 * 60;
const maxAuditPruneBatch = 64;
export const checkpointMaxCreateRecoveryAttempts = 8;

export interface CheckpointLimits {
  checkpoints: number;
  checkpointsPerOwner: number;
  checkpointsPerOrg: number;
  useClaimsPerCheckpoint: number;
  useClaimsPerOwner: number;
  useClaimsTotal: number;
}

type CheckpointLimitEnvironment = Pick<
  Env,
  | "CRABBOX_MAX_CHECKPOINTS"
  | "CRABBOX_MAX_CHECKPOINTS_PER_OWNER"
  | "CRABBOX_MAX_CHECKPOINTS_PER_ORG"
  | "CRABBOX_MAX_CHECKPOINT_USE_CLAIMS"
  | "CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_PER_OWNER"
  | "CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_TOTAL"
>;

function positiveCheckpointLimit(value: string | undefined, fallback: number): number {
  if (!value || !/^\d+$/.test(value)) return fallback;
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : fallback;
}

export function checkpointLimits(env: CheckpointLimitEnvironment = {}): CheckpointLimits {
  return {
    checkpoints: positiveCheckpointLimit(env.CRABBOX_MAX_CHECKPOINTS, 64),
    checkpointsPerOwner: positiveCheckpointLimit(env.CRABBOX_MAX_CHECKPOINTS_PER_OWNER, 16),
    checkpointsPerOrg: positiveCheckpointLimit(env.CRABBOX_MAX_CHECKPOINTS_PER_ORG, 32),
    useClaimsPerCheckpoint: positiveCheckpointLimit(env.CRABBOX_MAX_CHECKPOINT_USE_CLAIMS, 16),
    useClaimsPerOwner: positiveCheckpointLimit(env.CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_PER_OWNER, 64),
    useClaimsTotal: positiveCheckpointLimit(env.CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_TOTAL, 256),
  };
}

export interface CheckpointPrincipal {
  owner: string;
  org: string;
  admin?: boolean;
}

export class CheckpointError extends Error {
  constructor(
    readonly code: string,
    message: string,
    readonly status = 409,
  ) {
    super(message);
    this.name = "CheckpointError";
  }
}

export function validCheckpointID(value: unknown): value is string {
  return typeof value === "string" && /^chk_[A-Za-z0-9_-]{1,96}$/.test(value);
}

export function validCheckpointRetention(value: unknown): value is CoordinatorCheckpointRetention {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const retention = value as { mode?: unknown; unusedForSeconds?: unknown };
  if (retention.mode === "manual") return retention.unusedForSeconds === undefined;
  return (
    retention.mode === "expire-unused" &&
    typeof retention.unusedForSeconds === "number" &&
    Number.isSafeInteger(retention.unusedForSeconds) &&
    retention.unusedForSeconds > 0 &&
    retention.unusedForSeconds <= maxRetentionSeconds
  );
}

export function checkpointKey(id: string): string {
  return `checkpoint:${id}`;
}

export function checkpointDueKey(id: string, timestamp: string): string {
  return `${checkpointDuePrefix}${String(Date.parse(timestamp)).padStart(16, "0")}:${id}`;
}

export function checkpointUseKey(id: string, tokenHash: string): string {
  return `checkpoint-use:${id}:${tokenHash}`;
}

export function checkpointPinKey(id: string, catalogKey: string): string {
  return `checkpoint-pin:${id}:${encodeURIComponent(catalogKey)}`;
}

export function checkpointEventKey(id: string, sequence: number): string {
  return `checkpoint-event:${id}:${String(sequence).padStart(12, "0")}`;
}

function checkpointScopeKey(
  provider: CoordinatorCheckpointProvider,
  scope: CoordinatorCheckpointScope,
): string {
  switch (provider) {
    case "aws":
      return `${scope.accountID}:${scope.region}`;
    case "azure":
      return `${scope.subscriptionID}:${scope.resourceGroup}:${scope.region}`.toLowerCase();
    case "gcp":
      return `${scope.project}:global`;
  }
}

export function checkpointResourceKey(
  provider: CoordinatorCheckpointProvider,
  scope: CoordinatorCheckpointScope,
  kind: string,
  resourceID: string,
): string {
  return `checkpoint-resource:${provider}:${encodeURIComponent(checkpointScopeKey(provider, scope))}:${encodeURIComponent(kind)}:${encodeURIComponent(resourceID)}`;
}

function checkpointResourceIntentKey(
  provider: CoordinatorCheckpointProvider,
  scope: CoordinatorCheckpointScope,
  kind: string,
  resourceName: string,
): string {
  return `checkpoint-intent:${provider}:${encodeURIComponent(checkpointScopeKey(provider, scope))}:${encodeURIComponent(kind)}:${encodeURIComponent(resourceName)}`;
}

function checkpointResourceIntent(
  record: CoordinatorCheckpointRecord,
): CoordinatorCheckpointResourceIntent {
  const resourceName = record.createClaim!.resourceName;
  const kind =
    record.provider === "aws"
      ? record.strategy === "image"
        ? "aws-ami"
        : "aws-ebs-snapshot"
      : record.provider === "azure"
        ? "azure-os-disk-snapshot"
        : record.strategy === "image"
          ? "gcp-machine-image"
          : "gcp-disk-snapshot";
  const resourceID =
    record.provider === "azure"
      ? `/subscriptions/${record.scope.subscriptionID}/resourceGroups/${record.scope.resourceGroup}/providers/Microsoft.Compute/snapshots/${resourceName}`
      : record.provider === "gcp"
        ? `projects/${record.scope.project}/global/${kind === "gcp-machine-image" ? "machineImages" : "snapshots"}/${resourceName}`
        : undefined;
  return {
    checkpointID: record.id,
    provider: record.provider,
    scope: record.scope,
    kind,
    resourceName,
    ...(resourceID ? { resourceID } : {}),
    generation: record.generation,
  };
}

export function validateCheckpointScope(
  provider: CoordinatorCheckpointProvider,
  scope: CoordinatorCheckpointScope,
): boolean {
  if (!scope.region?.trim()) return false;
  switch (provider) {
    case "aws":
      return (
        /^\d{12}$/.test(scope.accountID ?? "") && /^[a-z]{2}(?:-gov)?-[a-z]+-\d$/.test(scope.region)
      );
    case "azure":
      return Boolean(scope.subscriptionID?.trim() && scope.resourceGroup?.trim());
    case "gcp":
      return /^[a-z][a-z0-9-]{4,62}$/.test(scope.project ?? "");
  }
}

export function checkpointVisibleTo(
  checkpoint: CoordinatorCheckpointRecord,
  principal: CheckpointPrincipal,
): boolean {
  return Boolean(
    principal.admin ||
    (checkpoint.owner === principal.owner && sameOrgIdentityKey(checkpoint.org, principal.org)),
  );
}

function requireVisible(
  checkpoint: CoordinatorCheckpointRecord | undefined,
  principal?: CheckpointPrincipal,
): CoordinatorCheckpointRecord {
  if (!checkpoint || (principal && !checkpointVisibleTo(checkpoint, principal))) {
    throw new CheckpointError("not_found", "checkpoint not found", 404);
  }
  return checkpoint;
}

async function nextCheckpointDeadline(
  transaction: CoordinatorStorageView,
  record: CoordinatorCheckpointRecord,
): Promise<string | undefined> {
  const candidates: number[] = [];
  const retain = (timestamp: string | undefined) => {
    const value = Date.parse(timestamp ?? "");
    if (Number.isFinite(value)) candidates.push(value);
  };
  if (record.state === "deleted") {
    const deleted = Date.parse(record.deletedAt ?? "");
    if (Number.isFinite(deleted)) candidates.push(deleted + checkpointAuditRetentionMS);
  } else if (record.state === "creating") {
    retain(record.createClaim?.expiresAt);
    retain(record.retryAt);
  } else if (record.state === "delete-pending" || record.state === "deleting") {
    retain(record.retryAt);
    retain(record.deleteClaim?.expiresAt);
  } else if (record.state === "ready") {
    const claims = await transaction.list<CoordinatorCheckpointUseClaim>({
      prefix: `checkpoint-use:${record.id}:`,
    });
    for (const claim of claims.values()) retain(claim.expiresAt);
    if (
      record.retention.mode === "expire-unused" &&
      record.pinCount === 0 &&
      record.activeUseCount === 0
    ) {
      const lastUsed = Date.parse(record.lastUsedAt);
      if (Number.isFinite(lastUsed)) {
        candidates.push(lastUsed + record.retention.unusedForSeconds * 1000);
      }
    }
  }
  return candidates.length ? new Date(Math.min(...candidates)).toISOString() : undefined;
}

async function writeCheckpointTransition(
  transaction: CoordinatorStorageView,
  previous: CoordinatorCheckpointRecord | undefined,
  next: CoordinatorCheckpointRecord,
  eventType: string,
  actor: string,
  details: Pick<CoordinatorCheckpointEvent, "reason" | "error" | "retryAt"> = {},
): Promise<CoordinatorCheckpointRecord> {
  if (previous?.nextSweepAt) {
    await transaction.delete(checkpointDueKey(previous.id, previous.nextSweepAt));
  }
  const updated: CoordinatorCheckpointRecord = {
    ...next,
    revision: (previous?.revision ?? 0) + 1,
    eventSequence: (previous?.eventSequence ?? 0) + 1,
    updatedAt: new Date().toISOString(),
  };
  delete updated.nextSweepAt;
  const nextSweepAt = await nextCheckpointDeadline(transaction, updated);
  if (nextSweepAt) updated.nextSweepAt = nextSweepAt;
  const event: CoordinatorCheckpointEvent = {
    checkpointID: updated.id,
    sequence: updated.eventSequence,
    type: eventType,
    createdAt: updated.updatedAt,
    actor,
    provider: updated.provider,
    scope: updated.scope,
    generation: updated.generation,
    ...details,
  };
  await transaction.put(checkpointKey(updated.id), updated);
  await transaction.put(checkpointEventKey(updated.id, updated.eventSequence), event);
  if (updated.eventSequence > checkpointMaxAuditEvents) {
    const staleThrough = checkpointEventKey(
      updated.id,
      updated.eventSequence - checkpointMaxAuditEvents,
    );
    const oldest = await transaction.list<CoordinatorCheckpointEvent>({
      prefix: `checkpoint-event:${updated.id}:`,
      limit: maxAuditPruneBatch,
    });
    await Promise.all(
      [...oldest.keys()]
        .filter((key) => key <= staleThrough)
        .map(async (key) => await transaction.delete(key)),
    );
  }
  if (nextSweepAt) {
    const marker: CoordinatorCheckpointDueIndex = {
      checkpointID: updated.id,
      generation: updated.generation,
      revision: updated.revision,
      nextSweepAt,
    };
    await transaction.put(checkpointDueKey(updated.id, nextSweepAt), marker);
  }
  return updated;
}

export interface CheckpointCreateReservation {
  record: Omit<
    CoordinatorCheckpointRecord,
    | "version"
    | "state"
    | "generation"
    | "revision"
    | "updatedAt"
    | "attempts"
    | "pinCount"
    | "activeUseCount"
    | "eventSequence"
    | "createClaim"
  >;
  ownershipToken: string;
  resourceName: string;
  coordinatorGeneration: string;
}

export async function reserveCheckpointCreate(
  storage: CoordinatorStorage,
  input: CheckpointCreateReservation,
  limits: CheckpointLimits = checkpointLimits(),
): Promise<CoordinatorCheckpointRecord> {
  if (!validCheckpointID(input.record.id)) {
    throw new CheckpointError("invalid_checkpoint_id", "invalid checkpoint identifier", 400);
  }
  if (!validCheckpointRetention(input.record.retention)) {
    throw new CheckpointError("invalid_checkpoint_retention", "invalid checkpoint retention", 400);
  }
  if (!validateCheckpointScope(input.record.provider, input.record.scope)) {
    throw new CheckpointError(
      "checkpoint_source_mismatch",
      "source lease provider scope is incomplete",
      409,
    );
  }
  const tokenHash = await sha256Hex(input.ownershipToken);
  const expiresAt = new Date(Date.now() + checkpointCreateClaimTTLMS).toISOString();
  return storage.transaction(async (transaction) => {
    if (await transaction.get(checkpointKey(input.record.id))) {
      throw new CheckpointError("checkpoint_pending", "checkpoint identifier is already reserved");
    }
    const counts = { global: 0, owner: 0, org: 0 };
    await scanCheckpointRecords(transaction, limits.checkpoints, (existing) => {
      if (existing.state === "deleted") return false;
      counts.global++;
      if (existing.owner === input.record.owner) counts.owner++;
      if (orgMatchesForAccounting(existing.org, input.record.org)) counts.org++;
      return counts.global >= limits.checkpoints;
    });
    const exceeded =
      counts.global >= limits.checkpoints
        ? { scope: "global", observed: counts.global, limit: limits.checkpoints }
        : counts.owner >= limits.checkpointsPerOwner
          ? { scope: "owner", observed: counts.owner, limit: limits.checkpointsPerOwner }
          : counts.org >= limits.checkpointsPerOrg
            ? { scope: "org", observed: counts.org, limit: limits.checkpointsPerOrg }
            : undefined;
    if (exceeded) {
      throw new CheckpointError(
        "checkpoint_limit_exceeded",
        `checkpoint admission limit exceeded: scope=${exceeded.scope} observed=${exceeded.observed} limit=${exceeded.limit}`,
        429,
      );
    }
    const record: CoordinatorCheckpointRecord = {
      ...input.record,
      version: 1,
      state: "creating",
      generation: 1,
      revision: 0,
      updatedAt: input.record.createdAt,
      attempts: 0,
      pinCount: 0,
      activeUseCount: 0,
      eventSequence: 0,
      createClaim: {
        tokenHash,
        resourceName: input.resourceName,
        expiresAt,
        coordinatorGeneration: input.coordinatorGeneration,
      },
    };
    const intent = checkpointResourceIntent(record);
    const intentKey = checkpointResourceIntentKey(
      intent.provider,
      intent.scope,
      intent.kind,
      intent.resourceName,
    );
    const existingIntent = await transaction.get<CoordinatorCheckpointResourceIntent>(intentKey);
    if (existingIntent && existingIntent.checkpointID !== record.id) {
      throw new CheckpointError(
        "checkpoint_source_mismatch",
        "checkpoint provider resource name is already reserved",
      );
    }
    await transaction.put(intentKey, intent);
    return writeCheckpointTransition(
      transaction,
      undefined,
      record,
      "checkpoint.create.reserved",
      record.owner,
    );
  });
}

async function scanCheckpointRecords(
  transaction: CoordinatorStorageView,
  batchSize: number,
  visit: (record: CoordinatorCheckpointRecord) => boolean,
): Promise<void> {
  const scan = async (startAfter?: string): Promise<void> => {
    const records = await transaction.list<CoordinatorCheckpointRecord>({
      prefix: "checkpoint:",
      limit: batchSize,
      ...(startAfter ? { startAfter } : {}),
    });
    let lastKey: string | undefined;
    for (const [key, record] of records) {
      if (visit(record)) return;
      lastKey = key;
    }
    if (records.size === batchSize && lastKey) await scan(lastKey);
  };
  await scan();
}

function validatedCheckpointImage(
  record: CoordinatorCheckpointRecord,
  image: ProviderImage,
): CoordinatorCheckpointImage {
  const id = image.id?.trim();
  const resourceID = image.resourceID?.trim();
  const kind = image.kind?.trim();
  const immutableID = image.immutableID?.trim();
  if (!id || !resourceID || !kind || !immutableID || image.provider !== record.provider) {
    throw new CheckpointError(
      "checkpoint_source_mismatch",
      "provider did not establish exact checkpoint resource identity",
    );
  }
  if (
    image.checkpointOwnershipHash !== record.createClaim?.tokenHash ||
    image.checkpointSourceLeaseID !== record.leaseID
  ) {
    throw new CheckpointError(
      "checkpoint_source_mismatch",
      "provider checkpoint ownership evidence does not match the durable reservation",
    );
  }
  if (image.region !== record.scope.region) {
    throw new CheckpointError(
      "checkpoint_source_mismatch",
      "provider checkpoint location does not match the source lease",
    );
  }
  if (record.provider === "aws" && image.accountID !== record.scope.accountID) {
    throw new CheckpointError(
      "checkpoint_source_mismatch",
      "provider checkpoint AWS account does not match the source lease",
    );
  }
  if (record.provider === "aws" && kind === "aws-ami" && !(image.snapshots ?? []).length) {
    throw new CheckpointError(
      "checkpoint_source_mismatch",
      "AWS checkpoint AMI has no verified owned backing snapshots",
    );
  }
  if (record.provider === "azure") {
    const expected = `/subscriptions/${record.scope.subscriptionID}/resourceGroups/${record.scope.resourceGroup}/providers/Microsoft.Compute/snapshots/${id}`;
    if (resourceID.toLowerCase() !== expected.toLowerCase() || kind !== "azure-os-disk-snapshot") {
      throw new CheckpointError(
        "checkpoint_source_mismatch",
        "provider checkpoint Azure resource scope does not match the source lease",
      );
    }
  }
  if (record.provider === "gcp") {
    const collection =
      kind === "gcp-disk-snapshot"
        ? "snapshots"
        : kind === "gcp-machine-image"
          ? "machineImages"
          : "";
    if (
      !collection ||
      image.project !== record.scope.project ||
      (!resourceID.endsWith(`/projects/${record.scope.project}/global/${collection}/${id}`) &&
        resourceID !== `projects/${record.scope.project}/global/${collection}/${id}`)
    ) {
      throw new CheckpointError(
        "checkpoint_source_mismatch",
        "provider checkpoint GCP resource scope does not match the source lease",
      );
    }
  }
  return {
    id,
    resourceID,
    kind,
    immutableID,
    snapshotIDs: [...new Set((image.snapshots ?? []).map((value) => value.trim()).filter(Boolean))],
    state: image.state,
    ...(image.architecture ? { architecture: image.architecture } : {}),
  };
}

function checkpointResourceAliases(
  image: CoordinatorCheckpointImage,
): Array<{ kind: string; id: string }> {
  const aliases = new Map<string, { kind: string; id: string }>();
  for (const id of [image.id, image.resourceID]) {
    aliases.set(`${image.kind}:${id}`, { kind: image.kind, id });
  }
  if (image.kind === "aws-ami") {
    for (const id of image.snapshotIDs) {
      aliases.set(`aws-ebs-snapshot:${id}`, { kind: "aws-ebs-snapshot", id });
    }
  }
  return [...aliases.values()];
}

export async function publishCreatedCheckpoint(
  storage: CoordinatorStorage,
  checkpointID: string,
  ownershipToken: string,
  image: ProviderImage,
): Promise<CoordinatorCheckpointRecord> {
  const tokenHash = await sha256Hex(ownershipToken);
  return publishCheckpointWithHash(storage, checkpointID, tokenHash, image);
}

export async function publishRecoveredCheckpoint(
  storage: CoordinatorStorage,
  checkpointID: string,
  image: ProviderImage,
): Promise<CoordinatorCheckpointRecord> {
  const checkpoint = await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID));
  if (!checkpoint?.createClaim?.tokenHash) {
    throw new CheckpointError(
      "checkpoint_claim_invalid",
      "checkpoint creation claim is unavailable",
    );
  }
  return publishCheckpointWithHash(storage, checkpointID, checkpoint.createClaim.tokenHash, image);
}

async function publishCheckpointWithHash(
  storage: CoordinatorStorage,
  checkpointID: string,
  tokenHash: string,
  image: ProviderImage,
): Promise<CoordinatorCheckpointRecord> {
  return storage.transaction(async (transaction) => {
    const previous = requireVisible(
      await transaction.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)),
    );
    if (
      (previous.state !== "creating" && previous.state !== "failed") ||
      previous.createClaim?.tokenHash !== tokenHash
    ) {
      throw new CheckpointError("checkpoint_claim_invalid", "checkpoint creation claim is invalid");
    }
    const checkpointImage = validatedCheckpointImage(previous, image);
    const intent = checkpointResourceIntent(previous);
    const intentKey = checkpointResourceIntentKey(
      intent.provider,
      intent.scope,
      intent.kind,
      intent.resourceName,
    );
    const reserved = await transaction.get<CoordinatorCheckpointResourceIntent>(intentKey);
    if (!reserved || reserved.checkpointID !== previous.id) {
      throw new CheckpointError(
        "checkpoint_source_mismatch",
        "checkpoint exact provider creation intent is missing",
      );
    }
    await Promise.all(
      checkpointResourceAliases(checkpointImage).map(async (alias) => {
        const key = checkpointResourceKey(previous.provider, previous.scope, alias.kind, alias.id);
        const existing = await transaction.get<CoordinatorCheckpointResourceClaim>(key);
        if (existing && existing.checkpointID !== previous.id) {
          throw new CheckpointError(
            "checkpoint_source_mismatch",
            "checkpoint provider resource is already owned",
          );
        }
        const claim: CoordinatorCheckpointResourceClaim = {
          checkpointID: previous.id,
          provider: previous.provider,
          scope: previous.scope,
          kind: alias.kind,
          resourceID: alias.id,
          immutableID: checkpointImage.immutableID,
          generation: previous.generation,
        };
        await transaction.put(key, claim);
      }),
    );
    await Promise.all(
      [...new Set([checkpointImage.id, checkpointImage.resourceID])].map(
        async (alias) =>
          await transaction.put(
            `image:${previous.provider}:created:${encodeURIComponent(alias)}`,
            image,
          ),
      ),
    );
    await transaction.delete(intentKey);
    const next = { ...previous, state: "ready" as const, image: checkpointImage };
    delete next.createClaim;
    delete next.retryAt;
    delete next.lastError;
    let published = await writeCheckpointTransition(
      transaction,
      previous,
      next,
      "checkpoint.created",
      previous.owner,
    );
    const catalogPrefixes =
      previous.provider === "aws"
        ? ["image:aws:catalog:", "image:aws:variant:", "image:aws:promoted"]
        : previous.provider === "azure"
          ? ["image:azure:promoted:"]
          : [];
    const catalog = await Promise.all(
      catalogPrefixes.map(async (prefix) => await transaction.list<ProviderImage>({ prefix })),
    );
    await [...new Map(catalog.flatMap((entries) => [...entries])).entries()].reduce(
      async (pending, [key, promoted]) => {
        await pending;
        if (
          (promoted.id !== checkpointImage.id &&
            promoted.resourceID !== checkpointImage.resourceID) ||
          (promoted.region && promoted.region !== previous.scope.region) ||
          (promoted.immutableID && promoted.immutableID !== checkpointImage.immutableID) ||
          (promoted.accountID && promoted.accountID !== previous.scope.accountID) ||
          (previous.provider === "azure" &&
            promoted.resourceID?.toLowerCase() !== checkpointImage.resourceID.toLowerCase())
        ) {
          return;
        }
        await pinCheckpointPromotion(transaction, previous.provider, promoted, key);
        published = (await transaction.get<CoordinatorCheckpointRecord>(
          checkpointKey(previous.id),
        ))!;
      },
      Promise.resolve(),
    );
    return published;
  });
}

export async function recordCheckpointCreateFailure(
  storage: CoordinatorStorage,
  checkpointID: string,
  ownershipToken: string,
  message: string,
): Promise<CoordinatorCheckpointRecord> {
  const tokenHash = await sha256Hex(ownershipToken);
  return recordCheckpointCreateFailureWithHash(storage, checkpointID, tokenHash, message);
}

export async function recordCheckpointCreateRecoveryFailure(
  storage: CoordinatorStorage,
  checkpointID: string,
  message: string,
): Promise<CoordinatorCheckpointRecord> {
  const record = await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID));
  if (!record?.createClaim) {
    throw new CheckpointError(
      "checkpoint_claim_invalid",
      "checkpoint creation claim is unavailable",
    );
  }
  return recordCheckpointCreateFailureWithHash(
    storage,
    checkpointID,
    record.createClaim.tokenHash,
    message,
  );
}

async function recordCheckpointCreateFailureWithHash(
  storage: CoordinatorStorage,
  checkpointID: string,
  tokenHash: string,
  message: string,
): Promise<CoordinatorCheckpointRecord> {
  return storage.transaction(async (transaction) => {
    const previous = requireVisible(
      await transaction.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)),
    );
    if (previous.state !== "creating" || previous.createClaim?.tokenHash !== tokenHash)
      return previous;
    const attempts = previous.attempts + 1;
    const exhausted = attempts >= checkpointMaxCreateRecoveryAttempts;
    const retryAt = exhausted
      ? undefined
      : new Date(Date.now() + checkpointRetryDelayMS(attempts)).toISOString();
    const next: CoordinatorCheckpointRecord = {
      ...previous,
      state: exhausted ? "failed" : "creating",
      attempts,
      lastError: message.slice(0, 512),
      ...(retryAt
        ? { retryAt, createClaim: { ...previous.createClaim!, expiresAt: retryAt } }
        : {}),
    };
    if (!retryAt) delete next.retryAt;
    return writeCheckpointTransition(
      transaction,
      previous,
      next,
      exhausted ? "checkpoint.create.recovery_exhausted" : "checkpoint.create.failed",
      "system",
      { error: message.slice(0, 512), ...(retryAt ? { retryAt } : {}) },
    );
  });
}

export async function failCheckpointCreateDefinitively(
  storage: CoordinatorStorage,
  checkpointID: string,
  ownershipToken: string,
  message: string,
): Promise<CoordinatorCheckpointRecord> {
  const tokenHash = await sha256Hex(ownershipToken);
  return storage.transaction(async (transaction) => {
    const previous = requireVisible(
      await transaction.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)),
    );
    if (previous.state !== "creating" || previous.createClaim?.tokenHash !== tokenHash) {
      throw new CheckpointError("checkpoint_claim_invalid", "checkpoint creation claim is invalid");
    }
    const next: CoordinatorCheckpointRecord = {
      ...previous,
      state: "failed",
      attempts: previous.attempts + 1,
      createClaim: { ...previous.createClaim, definitiveRefusal: true },
      lastError: message.slice(0, 512),
    };
    delete next.retryAt;
    return writeCheckpointTransition(
      transaction,
      previous,
      next,
      "checkpoint.create.failed_definitively",
      "system",
      { error: message.slice(0, 512) },
    );
  });
}

export async function cancelFailedCheckpointCreate(
  storage: CoordinatorStorage,
  checkpointID: string,
  principal: CheckpointPrincipal,
): Promise<CoordinatorCheckpointRecord> {
  return storage.transaction(async (transaction) => {
    const previous = requireVisible(
      await transaction.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)),
      principal,
    );
    if (previous.state !== "failed" || !previous.createClaim || previous.image) {
      throw new CheckpointError(
        "checkpoint_pending",
        "checkpoint creation is not safely cancelable",
      );
    }
    const intent = checkpointResourceIntent(previous);
    await transaction.delete(
      checkpointResourceIntentKey(intent.provider, intent.scope, intent.kind, intent.resourceName),
    );
    const next: CoordinatorCheckpointRecord = {
      ...previous,
      state: "deleted",
      deletedAt: new Date().toISOString(),
    };
    delete next.createClaim;
    delete next.retryAt;
    return writeCheckpointTransition(
      transaction,
      previous,
      next,
      "checkpoint.create.canceled",
      principal.owner,
    );
  });
}

export async function updateCheckpointRetention(
  storage: CoordinatorStorage,
  checkpointID: string,
  principal: CheckpointPrincipal,
  retention: CoordinatorCheckpointRetention,
): Promise<CoordinatorCheckpointRecord> {
  if (!validCheckpointRetention(retention)) {
    throw new CheckpointError("invalid_checkpoint_retention", "invalid checkpoint retention", 400);
  }
  return storage.transaction(async (transaction) => {
    const previous = requireVisible(
      await transaction.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)),
      principal,
    );
    if (previous.state !== "ready") {
      throw new CheckpointError("checkpoint_pending", "checkpoint is not ready");
    }
    return writeCheckpointTransition(
      transaction,
      previous,
      { ...previous, retention },
      "checkpoint.retention.updated",
      principal.owner,
    );
  });
}

export async function acquireCheckpointUse(
  storage: CoordinatorStorage,
  checkpointID: string,
  principal: CheckpointPrincipal,
  limits: CheckpointLimits = checkpointLimits(),
): Promise<{ checkpoint: CoordinatorCheckpointRecord; token: string; expiresAt: string }> {
  const token = crypto.randomUUID() + crypto.randomUUID();
  const tokenHash = await sha256Hex(token);
  const now = new Date();
  const expiresAt = new Date(now.getTime() + checkpointUseClaimTTLMS).toISOString();
  const checkpoint = await storage.transaction(async (transaction) => {
    let previous = requireVisible(
      await transaction.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)),
      principal,
    );
    if (previous.state !== "ready" || previous.deleteClaim || !previous.image) {
      throw new CheckpointError(
        "checkpoint_delete_in_progress",
        "checkpoint is not available for use",
      );
    }
    previous = await expireAvailableCheckpointClaims(transaction, previous, now.getTime(), limits);
    if (
      previous.retention.mode === "expire-unused" &&
      previous.pinCount === 0 &&
      previous.activeUseCount === 0 &&
      Date.parse(previous.lastUsedAt) + previous.retention.unusedForSeconds * 1000 <= now.getTime()
    ) {
      throw new CheckpointError("checkpoint_pending", "checkpoint unused retention has expired");
    }
    const counts = { global: 0, owner: 0, checkpoint: previous.activeUseCount };
    await scanCheckpointRecords(transaction, limits.checkpoints, (record) => {
      if (record.state === "deleted") return false;
      counts.global += record.activeUseCount;
      if (record.owner === previous.owner) counts.owner += record.activeUseCount;
      return false;
    });
    const exceeded =
      counts.checkpoint >= limits.useClaimsPerCheckpoint
        ? {
            scope: "checkpoint",
            observed: counts.checkpoint,
            limit: limits.useClaimsPerCheckpoint,
          }
        : counts.owner >= limits.useClaimsPerOwner
          ? { scope: "owner", observed: counts.owner, limit: limits.useClaimsPerOwner }
          : counts.global >= limits.useClaimsTotal
            ? { scope: "global", observed: counts.global, limit: limits.useClaimsTotal }
            : undefined;
    if (exceeded) {
      throw new CheckpointError(
        "checkpoint_claim_limit_exceeded",
        `checkpoint use claim limit exceeded: scope=${exceeded.scope} observed=${exceeded.observed} limit=${exceeded.limit}`,
        429,
      );
    }
    const claim: CoordinatorCheckpointUseClaim = {
      checkpointID,
      tokenHash,
      owner: principal.owner,
      org: principal.org,
      generation: previous.generation,
      createdAt: now.toISOString(),
      expiresAt,
      state: "available",
    };
    await transaction.put(checkpointUseKey(checkpointID, tokenHash), claim);
    return writeCheckpointTransition(
      transaction,
      previous,
      { ...previous, activeUseCount: previous.activeUseCount + 1 },
      "checkpoint.use.claimed",
      principal.owner,
    );
  });
  return { checkpoint, token, expiresAt };
}

async function expireAvailableCheckpointClaims(
  transaction: CoordinatorStorageView,
  checkpoint: CoordinatorCheckpointRecord,
  now: number,
  limits: CheckpointLimits,
): Promise<CoordinatorCheckpointRecord> {
  const batchSize = Math.min(limits.useClaimsPerCheckpoint, limits.useClaimsTotal);
  const expirePage = async (
    current: CoordinatorCheckpointRecord,
    startAfter?: string,
  ): Promise<CoordinatorCheckpointRecord> => {
    const claims = await transaction.list<CoordinatorCheckpointUseClaim>({
      prefix: `checkpoint-use:${checkpoint.id}:`,
      limit: batchSize,
      ...(startAfter ? { startAfter } : {}),
    });
    const updated = await [...claims].reduce(async (pending, [key, claim]) => {
      const record = await pending;
      if (claim.state === "provisioning" || Date.parse(claim.expiresAt) > now) return record;
      await transaction.delete(key);
      return await writeCheckpointTransition(
        transaction,
        record,
        { ...record, activeUseCount: Math.max(0, record.activeUseCount - 1) },
        "checkpoint.use.expired",
        "system",
      );
    }, Promise.resolve(current));
    const lastKey = [...claims.keys()].at(-1);
    return claims.size === batchSize && lastKey ? await expirePage(updated, lastKey) : updated;
  };
  return await expirePage(checkpoint);
}

async function validatedUseClaim(
  transaction: CoordinatorStorageView,
  checkpointID: string,
  tokenHash: string,
  principal: CheckpointPrincipal,
): Promise<{ checkpoint: CoordinatorCheckpointRecord; claim: CoordinatorCheckpointUseClaim }> {
  const checkpoint = requireVisible(
    await transaction.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)),
    principal,
  );
  const claim = await transaction.get<CoordinatorCheckpointUseClaim>(
    checkpointUseKey(checkpointID, tokenHash),
  );
  if (
    !claim ||
    checkpoint.state !== "ready" ||
    claim.generation !== checkpoint.generation ||
    claim.owner !== principal.owner ||
    !sameOrgIdentityKey(claim.org, principal.org) ||
    (claim.state !== "provisioning" && Date.parse(claim.expiresAt) <= Date.now())
  ) {
    throw new CheckpointError(
      "checkpoint_claim_invalid",
      "checkpoint use claim is invalid or expired",
    );
  }
  return { checkpoint, claim };
}

export async function validateCheckpointUse(
  storage: CoordinatorStorage,
  checkpointID: string,
  token: string,
  principal: CheckpointPrincipal,
): Promise<CoordinatorCheckpointRecord> {
  const tokenHash = await sha256Hex(token);
  return storage.transaction(async (transaction) => {
    const { checkpoint } = await validatedUseClaim(transaction, checkpointID, tokenHash, principal);
    return checkpoint;
  });
}

export async function bindCheckpointUseProvisioning(
  storage: CoordinatorStorage,
  checkpointID: string,
  token: string,
  principal: CheckpointPrincipal,
  attemptID: string,
  leaseID: string,
): Promise<CoordinatorCheckpointRecord> {
  const tokenHash = await sha256Hex(token);
  return storage.transaction(async (transaction) => {
    const { checkpoint, claim } = await validatedUseClaim(
      transaction,
      checkpointID,
      tokenHash,
      principal,
    );
    if (claim.state === "provisioning") {
      throw new CheckpointError(
        "checkpoint_in_use",
        claim.attemptID === attemptID && claim.leaseID === leaseID
          ? "checkpoint lease attempt is already provisioning"
          : "checkpoint claim already owns a lease attempt",
      );
    }
    await transaction.put(checkpointUseKey(checkpointID, tokenHash), {
      ...claim,
      state: "provisioning",
      attemptID,
      leaseID,
    } satisfies CoordinatorCheckpointUseClaim);
    return writeCheckpointTransition(
      transaction,
      checkpoint,
      checkpoint,
      "checkpoint.use.provisioning",
      principal.owner,
    );
  });
}

export async function abortCheckpointProvisioningForLease(
  storage: CoordinatorStorage,
  leaseID: string,
  attemptID: string,
  principal: CheckpointPrincipal,
): Promise<CoordinatorCheckpointRecord | undefined> {
  return storage.transaction(async (transaction) => {
    const claims = await transaction.list<CoordinatorCheckpointUseClaim>({
      prefix: "checkpoint-use:",
    });
    const match = [...claims].find(
      ([, claim]) =>
        claim.state === "provisioning" &&
        claim.leaseID === leaseID &&
        claim.attemptID === attemptID &&
        (principal.admin ||
          (claim.owner === principal.owner && sameOrgIdentityKey(claim.org, principal.org))),
    );
    if (!match) return undefined;
    const [key, claim] = match;
    const checkpoint = await transaction.get<CoordinatorCheckpointRecord>(
      checkpointKey(claim.checkpointID),
    );
    if (!checkpoint || checkpoint.generation !== claim.generation) return undefined;
    await transaction.delete(key);
    return writeCheckpointTransition(
      transaction,
      checkpoint,
      { ...checkpoint, activeUseCount: Math.max(0, checkpoint.activeUseCount - 1) },
      "checkpoint.use.aborted",
      principal.owner,
      { reason: "lease-create-canceled" },
    );
  });
}

export async function renewCheckpointUse(
  storage: CoordinatorStorage,
  checkpointID: string,
  token: string,
  principal: CheckpointPrincipal,
): Promise<{ checkpoint: CoordinatorCheckpointRecord; expiresAt: string }> {
  const tokenHash = await sha256Hex(token);
  const expiresAt = new Date(Date.now() + checkpointUseClaimTTLMS).toISOString();
  const checkpoint = await storage.transaction(async (transaction) => {
    const { checkpoint: previous, claim } = await validatedUseClaim(
      transaction,
      checkpointID,
      tokenHash,
      principal,
    );
    await transaction.put(checkpointUseKey(checkpointID, tokenHash), { ...claim, expiresAt });
    return writeCheckpointTransition(
      transaction,
      previous,
      previous,
      "checkpoint.use.renewed",
      principal.owner,
    );
  });
  return { checkpoint, expiresAt };
}

export async function finishCheckpointUse(
  storage: CoordinatorStorage,
  checkpointID: string,
  token: string,
  principal: CheckpointPrincipal,
  complete: boolean,
  attemptID?: string,
): Promise<CoordinatorCheckpointRecord> {
  const tokenHash = await sha256Hex(token);
  return storage.transaction(async (transaction) => {
    const { checkpoint: previous, claim } = await validatedUseClaim(
      transaction,
      checkpointID,
      tokenHash,
      principal,
    );
    if (complete && claim.state !== "provisioning") {
      throw new CheckpointError(
        "checkpoint_claim_invalid",
        "checkpoint use can complete only after exact lease provisioning",
      );
    }
    if (complete && !attemptID) {
      const lease = await transaction.get<LeaseRecord>(`lease:${claim.leaseID}`);
      const attempt = await transaction.get<{
        token?: string;
        canonicalLeaseID?: string;
        owner?: string;
        org?: string;
        state?: string;
      }>(`create-attempt:${claim.leaseID}`);
      if (
        !lease ||
        lease.state !== "active" ||
        lease.checkpointID !== checkpointID ||
        lease.createAttemptID !== claim.attemptID ||
        !attempt ||
        attempt.token !== claim.attemptID ||
        attempt.state === "canceled" ||
        attempt.canonicalLeaseID !== lease.id ||
        attempt.owner !== claim.owner ||
        !sameOrgIdentityKey(attempt.org ?? "", claim.org)
      ) {
        throw new CheckpointError(
          "checkpoint_in_use",
          "checkpoint lease provisioning has not completed successfully",
        );
      }
      attemptID = claim.attemptID;
    }
    if (claim.state === "provisioning" && claim.attemptID !== attemptID) {
      throw new CheckpointError(
        "checkpoint_in_use",
        "checkpoint use claim is bound to active lease provisioning",
      );
    }
    if (attemptID && (claim.state !== "provisioning" || claim.attemptID !== attemptID)) {
      throw new CheckpointError(
        "checkpoint_claim_invalid",
        "checkpoint use claim does not own this lease attempt",
      );
    }
    await transaction.delete(checkpointUseKey(checkpointID, tokenHash));
    const next: CoordinatorCheckpointRecord = {
      ...previous,
      activeUseCount: Math.max(0, previous.activeUseCount - 1),
      ...(complete
        ? {
            lastUsedAt: new Date(
              Math.max(Date.parse(previous.lastUsedAt), Date.now()),
            ).toISOString(),
          }
        : {}),
    };
    return writeCheckpointTransition(
      transaction,
      previous,
      next,
      complete ? "checkpoint.use.completed" : "checkpoint.use.aborted",
      principal.owner,
    );
  });
}

export async function claimCheckpointDeletion(
  storage: CoordinatorStorage,
  checkpointID: string,
  principal: CheckpointPrincipal | undefined,
  reason: CoordinatorCheckpointDeleteClaim["reason"],
): Promise<{ checkpoint: CoordinatorCheckpointRecord; token: string }> {
  const token = crypto.randomUUID() + crypto.randomUUID();
  const tokenHash = await sha256Hex(token);
  const now = Date.now();
  const checkpoint = await storage.transaction(async (transaction) => {
    const previous = requireVisible(
      await transaction.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)),
      principal,
    );
    if (previous.state === "deleted") return previous;
    if (previous.state !== "ready" && previous.state !== "delete-pending") {
      throw new CheckpointError(
        "checkpoint_delete_in_progress",
        "checkpoint deletion is already in progress",
      );
    }
    if (!previous.image || !validateCheckpointScope(previous.provider, previous.scope)) {
      throw new CheckpointError(
        "checkpoint_source_mismatch",
        "checkpoint has no verified exact provider resource",
      );
    }
    if (previous.pinCount > 0) {
      throw new CheckpointError(
        "checkpoint_pinned",
        "checkpoint is pinned by an active image promotion",
      );
    }
    const claims = await transaction.list<CoordinatorCheckpointUseClaim>({
      prefix: `checkpoint-use:${checkpointID}:`,
    });
    if (
      [...claims.values()].some(
        (claim) => claim.state === "provisioning" || Date.parse(claim.expiresAt) > now,
      )
    ) {
      throw new CheckpointError("checkpoint_in_use", "checkpoint has an active use claim");
    }
    await Promise.all([...claims.keys()].map(async (key) => await transaction.delete(key)));
    if (
      reason === "unused-expiry" &&
      (previous.retention.mode !== "expire-unused" ||
        Date.parse(previous.lastUsedAt) + previous.retention.unusedForSeconds * 1000 > now)
    ) {
      throw new CheckpointError(
        "checkpoint_pending",
        "checkpoint has not reached its unused expiry",
      );
    }
    const immutableID = previous.image.immutableID;
    await Promise.all(
      checkpointResourceAliases(previous.image).map(async (alias) => {
        const key = checkpointResourceKey(previous.provider, previous.scope, alias.kind, alias.id);
        const claim = await transaction.get<CoordinatorCheckpointResourceClaim>(key);
        if (!claim || claim.checkpointID !== checkpointID || claim.immutableID !== immutableID) {
          throw new CheckpointError(
            "checkpoint_source_mismatch",
            "checkpoint exact provider ownership is missing",
          );
        }
      }),
    );
    const generation = previous.state === "ready" ? previous.generation + 1 : previous.generation;
    const next: CoordinatorCheckpointRecord = {
      ...previous,
      state: "deleting",
      generation,
      activeUseCount: 0,
      deleteRequestedAt: previous.deleteRequestedAt ?? new Date(now).toISOString(),
      deleteClaim: {
        tokenHash,
        generation,
        expiresAt: new Date(now + checkpointDeleteClaimTTLMS).toISOString(),
        reason,
        phase: "claimed",
      },
    };
    delete next.retryAt;
    return writeCheckpointTransition(
      transaction,
      previous,
      next,
      "checkpoint.delete.claimed",
      principal?.owner ?? "system",
      { reason },
    );
  });
  return { checkpoint, token };
}

async function validateDeleteClaim(
  transaction: CoordinatorStorageView,
  checkpointID: string,
  tokenHash: string,
): Promise<CoordinatorCheckpointRecord> {
  const checkpoint = requireVisible(
    await transaction.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)),
  );
  if (
    checkpoint.state !== "deleting" ||
    checkpoint.deleteClaim?.tokenHash !== tokenHash ||
    checkpoint.deleteClaim.generation !== checkpoint.generation
  ) {
    throw new CheckpointError("checkpoint_claim_invalid", "checkpoint deletion claim is invalid");
  }
  return checkpoint;
}

export function checkpointRetryDelayMS(attempts: number): number {
  return Math.min(60 * 60_000, 5_000 * 2 ** Math.min(Math.max(attempts - 1, 0), 10));
}

export async function recordCheckpointDeletionFailure(
  storage: CoordinatorStorage,
  checkpointID: string,
  token: string,
  message: string,
): Promise<CoordinatorCheckpointRecord> {
  const tokenHash = await sha256Hex(token);
  return storage.transaction(async (transaction) => {
    const previous = await validateDeleteClaim(transaction, checkpointID, tokenHash);
    const attempts = previous.attempts + 1;
    const retryAt = new Date(Date.now() + checkpointRetryDelayMS(attempts)).toISOString();
    const next: CoordinatorCheckpointRecord = {
      ...previous,
      state: "delete-pending",
      attempts,
      retryAt,
      lastError: message.slice(0, 512),
    };
    delete next.deleteClaim;
    return writeCheckpointTransition(
      transaction,
      previous,
      next,
      "checkpoint.delete.failed",
      "system",
      { error: message.slice(0, 512), retryAt },
    );
  });
}

export async function markCheckpointProviderDeleted(
  storage: CoordinatorStorage,
  checkpointID: string,
  token: string,
): Promise<CoordinatorCheckpointRecord> {
  const tokenHash = await sha256Hex(token);
  return storage.transaction(async (transaction) => {
    const previous = await validateDeleteClaim(transaction, checkpointID, tokenHash);
    const next: CoordinatorCheckpointRecord = {
      ...previous,
      deleteClaim: { ...previous.deleteClaim!, phase: "provider-deleted" },
    };
    return writeCheckpointTransition(
      transaction,
      previous,
      next,
      "checkpoint.delete.provider_completed",
      "system",
    );
  });
}

export async function finalizeCheckpointDeletion(
  storage: CoordinatorStorage,
  checkpointID: string,
  token: string,
): Promise<CoordinatorCheckpointRecord> {
  const tokenHash = await sha256Hex(token);
  return finalizeCheckpointDeletionWithHash(storage, checkpointID, tokenHash);
}

export async function recoverCheckpointDeletion(
  storage: CoordinatorStorage,
  checkpointID: string,
): Promise<CoordinatorCheckpointRecord> {
  const record = await storage.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID));
  if (!record?.deleteClaim) {
    throw new CheckpointError(
      "checkpoint_claim_invalid",
      "checkpoint deletion claim is unavailable",
    );
  }
  if (record.deleteClaim.phase === "provider-deleted") {
    return finalizeCheckpointDeletionWithHash(storage, checkpointID, record.deleteClaim.tokenHash);
  }
  return storage.transaction(async (transaction) => {
    const previous = await validateDeleteClaim(
      transaction,
      checkpointID,
      record.deleteClaim!.tokenHash,
    );
    if (Date.parse(previous.deleteClaim!.expiresAt) > Date.now()) return previous;
    const retryAt = new Date().toISOString();
    const next: CoordinatorCheckpointRecord = {
      ...previous,
      state: "delete-pending",
      attempts: previous.attempts + 1,
      retryAt,
      lastError: "checkpoint deletion claim expired",
    };
    delete next.deleteClaim;
    return writeCheckpointTransition(
      transaction,
      previous,
      next,
      "checkpoint.delete.failed",
      "system",
      { error: "checkpoint deletion claim expired", retryAt },
    );
  });
}

async function finalizeCheckpointDeletionWithHash(
  storage: CoordinatorStorage,
  checkpointID: string,
  tokenHash: string,
): Promise<CoordinatorCheckpointRecord> {
  return storage.transaction(async (transaction) => {
    const previous = await validateDeleteClaim(transaction, checkpointID, tokenHash);
    if (previous.deleteClaim?.phase !== "provider-deleted" || !previous.image) {
      throw new CheckpointError(
        "checkpoint_delete_failed",
        "checkpoint provider deletion was not confirmed",
      );
    }
    const checkpointImage = previous.image;
    await Promise.all(
      checkpointResourceAliases(checkpointImage).map(
        async (alias) =>
          await transaction.delete(
            checkpointResourceKey(previous.provider, previous.scope, alias.kind, alias.id),
          ),
      ),
    );
    await Promise.all(
      [...new Set([checkpointImage.id, checkpointImage.resourceID])].map(async (alias) => {
        const key = `image:${previous.provider}:created:${encodeURIComponent(alias)}`;
        const image = await transaction.get<ProviderImage>(key);
        if (
          image &&
          (image.id === checkpointImage.id || image.resourceID === checkpointImage.resourceID) &&
          image.immutableID === checkpointImage.immutableID
        ) {
          await transaction.delete(key);
        }
      }),
    );
    if (previous.provider === "aws") {
      await transaction.delete(`image:aws:deletion:${encodeURIComponent(previous.image.id)}`);
    }
    const next: CoordinatorCheckpointRecord = {
      ...previous,
      state: "deleted",
      deletedAt: new Date().toISOString(),
      activeUseCount: 0,
      pinCount: 0,
    };
    delete next.deleteClaim;
    delete next.retryAt;
    delete next.lastError;
    return writeCheckpointTransition(transaction, previous, next, "checkpoint.deleted", "system");
  });
}

type ManagedCheckpointImage = Pick<
  ProviderImage,
  "id" | "resourceID" | "kind" | "region" | "project" | "snapshots" | "immutableID"
> &
  Partial<
    Pick<
      ProviderImage,
      "name" | "accountID" | "checkpointOwnershipHash" | "checkpointSourceLeaseID"
    >
  >;

function checkpointResourceIdentity(
  provider: CoordinatorCheckpointProvider,
  scope: CoordinatorCheckpointScope,
  kind: string,
  value: string,
): string | undefined {
  const resource = value.trim();
  if (!resource) return undefined;
  if (provider === "aws") return resource;

  if (provider === "azure") {
    if (kind !== "azure-os-disk-snapshot") return undefined;
    if (!resource.includes("/")) return resource.toLowerCase();
    const match =
      /^\/subscriptions\/([^/]+)\/resourceGroups\/([^/]+)\/providers\/Microsoft\.Compute\/snapshots\/([^/]+)$/i.exec(
        resource,
      );
    const subscriptionID = match?.[1];
    const resourceGroup = match?.[2];
    const resourceName = match?.[3];
    if (
      !subscriptionID ||
      !resourceGroup ||
      !resourceName ||
      subscriptionID.toLowerCase() !== scope.subscriptionID?.toLowerCase() ||
      resourceGroup.toLowerCase() !== scope.resourceGroup?.toLowerCase()
    ) {
      return undefined;
    }
    return resourceName.toLowerCase();
  }

  const collection =
    kind === "gcp-disk-snapshot"
      ? "snapshots"
      : kind === "gcp-machine-image"
        ? "machineImages"
        : undefined;
  if (!collection) return undefined;
  if (!resource.includes("/")) return resource;
  let path = resource;
  if (resource.includes("://")) {
    let url: URL;
    try {
      url = new URL(resource);
    } catch {
      return undefined;
    }
    if (
      url.protocol !== "https:" ||
      !["compute.googleapis.com", "www.googleapis.com"].includes(url.hostname) ||
      url.port ||
      url.username ||
      url.password ||
      url.search ||
      url.hash
    ) {
      return undefined;
    }
    path = url.pathname;
  }
  const parts = path.replace(/^\//, "").split("/");
  if (parts[0] === "compute" && parts[1] === "v1") parts.splice(0, 2);
  if (
    parts.length !== 5 ||
    parts[0] !== "projects" ||
    parts[1] !== scope.project ||
    parts[2] !== "global" ||
    parts[3] !== collection ||
    !parts[4]
  ) {
    return undefined;
  }
  return parts[4];
}

function checkpointImageScopeMatches(
  provider: CoordinatorCheckpointProvider,
  scope: CoordinatorCheckpointScope,
  image: ManagedCheckpointImage,
): boolean {
  if (
    image.region &&
    (provider === "azure"
      ? image.region.toLowerCase() !== scope.region.toLowerCase()
      : image.region !== scope.region)
  ) {
    return false;
  }
  if (provider === "aws" && image.accountID && image.accountID !== scope.accountID) return false;
  if (provider === "gcp" && image.project && image.project !== scope.project) return false;
  return true;
}

function checkpointImageMatchesResource(
  provider: CoordinatorCheckpointProvider,
  scope: CoordinatorCheckpointScope,
  kind: string,
  resourceID: string,
  image: ManagedCheckpointImage,
): boolean {
  if (!checkpointImageScopeMatches(provider, scope, image)) return false;
  const expected = checkpointResourceIdentity(provider, scope, kind, resourceID);
  if (!expected) return false;
  const primary = [image.id, image.resourceID].filter((value): value is string => Boolean(value));
  if (provider !== "aws") {
    return (
      (!image.kind || image.kind === kind) &&
      primary.length > 0 &&
      primary.every(
        (value) => checkpointResourceIdentity(provider, scope, kind, value) === expected,
      )
    );
  }
  const primaryMatches =
    (!image.kind || image.kind === kind) &&
    primary.some((value) => checkpointResourceIdentity(provider, scope, kind, value) === expected);
  return (
    primaryMatches ||
    (kind === "aws-ebs-snapshot" &&
      (image.snapshots ?? []).some(
        (value) => checkpointResourceIdentity(provider, scope, kind, value) === expected,
      ))
  );
}

function pendingAWSCheckpointOwnershipMatches(
  intent: CoordinatorCheckpointResourceIntent,
  image: ManagedCheckpointImage,
): boolean {
  if (
    !image.checkpointOwnershipHash ||
    !image.checkpointSourceLeaseID ||
    image.accountID !== intent.scope.accountID ||
    image.region !== intent.scope.region
  ) {
    return false;
  }
  if (image.kind === intent.kind) return image.name === intent.resourceName;
  return (
    intent.kind === "aws-ami" &&
    image.kind === "aws-ebs-snapshot" &&
    /^snap-[a-z0-9-]+$/i.test(image.id)
  );
}

export async function findManagedCheckpointImage(
  storage: CoordinatorStorageView,
  provider: CoordinatorCheckpointProvider,
  image: ManagedCheckpointImage,
): Promise<CoordinatorCheckpointRecord | undefined> {
  if (![image.id, image.resourceID, ...(image.snapshots ?? [])].some(Boolean)) return undefined;
  const claims = await storage.list<CoordinatorCheckpointResourceClaim>({
    prefix: `checkpoint-resource:${provider}:`,
  });
  const matchingClaims = [...claims.values()].filter((claim) => {
    if (image.immutableID && claim.immutableID !== image.immutableID) return false;
    return checkpointImageMatchesResource(
      provider,
      claim.scope,
      claim.kind,
      claim.resourceID,
      image,
    );
  });
  const records = await Promise.all(
    matchingClaims.map(async (claim) => ({
      claim,
      record: await storage.get<CoordinatorCheckpointRecord>(checkpointKey(claim.checkpointID)),
    })),
  );
  const matchingRecords = records
    .filter(
      ({ claim, record }) =>
        record && record.state !== "deleted" && record.image?.immutableID === claim.immutableID,
    )
    .map(({ record }) => record!);
  const unique = [...new Map(matchingRecords.map((record) => [record.id, record])).values()];
  if (unique.length > 1) {
    throw new CheckpointError(
      "checkpoint_source_mismatch",
      "image identifier matches multiple exact checkpoint provider scopes",
    );
  }
  if (unique[0]) return unique[0];
  const intents = await storage.list<CoordinatorCheckpointResourceIntent>({
    prefix: `checkpoint-intent:${provider}:`,
  });
  const matchingIntents = [...intents.values()].filter((intent) => {
    if (!checkpointImageScopeMatches(provider, intent.scope, image)) return false;
    if (
      checkpointImageMatchesResource(
        provider,
        intent.scope,
        intent.kind,
        intent.resourceID ?? intent.resourceName,
        image,
      )
    ) {
      return true;
    }
    return provider === "aws" && pendingAWSCheckpointOwnershipMatches(intent, image);
  });
  const pending = await Promise.all(
    matchingIntents.map(async (intent) => ({
      intent,
      record: await storage.get<CoordinatorCheckpointRecord>(checkpointKey(intent.checkpointID)),
    })),
  );
  const exact = pending
    .filter(
      ({ intent, record }) =>
        record &&
        (record.state === "creating" || record.state === "failed") &&
        record.provider === provider &&
        record.generation === intent.generation &&
        checkpointScopeKey(provider, record.scope) === checkpointScopeKey(provider, intent.scope) &&
        (!image.checkpointOwnershipHash ||
          (record.createClaim?.tokenHash === image.checkpointOwnershipHash &&
            record.leaseID === image.checkpointSourceLeaseID)),
    )
    .map(({ record }) => record!);
  if (exact.length > 1) {
    throw new CheckpointError(
      "checkpoint_source_mismatch",
      "image identifier matches multiple pending checkpoint creation intents",
    );
  }
  return exact[0];
}

export async function pinCheckpointPromotion(
  transaction: CoordinatorStorageView,
  provider: CoordinatorCheckpointProvider,
  image: ProviderImage,
  catalogKey: string,
): Promise<void> {
  const checkpoint = await findManagedCheckpointImage(transaction, provider, image);
  if (!checkpoint) return;
  if (checkpoint.state !== "ready" || checkpoint.deleteClaim) {
    throw new CheckpointError(
      "checkpoint_delete_in_progress",
      "checkpoint image cannot be promoted during deletion",
    );
  }
  const key = checkpointPinKey(checkpoint.id, catalogKey);
  if (await transaction.get<CoordinatorCheckpointPin>(key)) return;
  const pin: CoordinatorCheckpointPin = {
    checkpointID: checkpoint.id,
    generation: checkpoint.generation,
    catalogKey,
    createdAt: new Date().toISOString(),
  };
  await transaction.put(key, pin);
  await writeCheckpointTransition(
    transaction,
    checkpoint,
    { ...checkpoint, pinCount: checkpoint.pinCount + 1 },
    "checkpoint.promotion.pinned",
    "system",
  );
}

export async function unpinCheckpointPromotion(
  transaction: CoordinatorStorageView,
  provider: CoordinatorCheckpointProvider,
  image: ProviderImage,
  catalogKey: string,
): Promise<void> {
  const checkpoint = await findManagedCheckpointImage(transaction, provider, image);
  if (!checkpoint) return;
  const key = checkpointPinKey(checkpoint.id, catalogKey);
  if (!(await transaction.get<CoordinatorCheckpointPin>(key))) return;
  await transaction.delete(key);
  await writeCheckpointTransition(
    transaction,
    checkpoint,
    { ...checkpoint, pinCount: Math.max(0, checkpoint.pinCount - 1) },
    "checkpoint.promotion.unpinned",
    "system",
  );
}

export async function expireCheckpointClaims(
  storage: CoordinatorStorage,
  checkpointID: string,
): Promise<CoordinatorCheckpointRecord | undefined> {
  return storage.transaction(async (transaction) => {
    let record = await transaction.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID));
    if (!record || record.state !== "ready") return record;
    const now = Date.now();
    const claims = await transaction.list<CoordinatorCheckpointUseClaim>({
      prefix: `checkpoint-use:${checkpointID}:`,
    });
    return await [...claims].reduce(async (pending, [key, claim]) => {
      const current = await pending;
      const expired = Date.parse(claim.expiresAt) <= now;
      if (claim.state === "provisioning") {
        const lease = claim.leaseID
          ? await transaction.get<LeaseRecord>(`lease:${claim.leaseID}`)
          : undefined;
        const attempt = claim.leaseID
          ? await transaction.get<{
              requestedLeaseID?: string;
              token?: string;
              state?: string;
              canonicalLeaseID?: string;
              owner?: string;
              org?: string;
              cloudID?: string;
            }>(`create-attempt:${claim.leaseID}`)
          : undefined;
        const exactLease = Boolean(
          lease &&
          lease.id === claim.leaseID &&
          lease.checkpointID === current.id &&
          lease.createAttemptID === claim.attemptID &&
          lease.owner === claim.owner &&
          sameOrgIdentityKey(lease.org, claim.org),
        );
        const exactAttempt = Boolean(
          attempt &&
          attempt.requestedLeaseID === claim.leaseID &&
          attempt.token === claim.attemptID &&
          (!attempt.canonicalLeaseID || attempt.canonicalLeaseID === claim.leaseID) &&
          attempt.owner === claim.owner &&
          sameOrgIdentityKey(attempt.org ?? "", claim.org),
        );
        const successful = Boolean(
          exactLease &&
          lease!.state === "active" &&
          exactAttempt &&
          attempt!.state !== "canceled" &&
          attempt!.canonicalLeaseID === lease!.id,
        );
        const uncertain = Boolean(
          lease?.provisioningResourceMayExist ||
          lease?.cleanupStartedAt ||
          lease?.cleanupRetryAt ||
          lease?.providerKeyCleanupPending,
        );
        const terminal = Boolean(
          (exactLease && !uncertain && ["failed", "released", "expired"].includes(lease!.state)) ||
          (!lease && exactAttempt && attempt!.state === "canceled" && !attempt!.cloudID),
        );
        if (successful || terminal) {
          await transaction.delete(key);
          return await writeCheckpointTransition(
            transaction,
            current,
            {
              ...current,
              activeUseCount: Math.max(0, current.activeUseCount - 1),
              ...(successful ? { lastUsedAt: new Date(now).toISOString() } : {}),
            },
            successful ? "checkpoint.use.completed" : "checkpoint.use.aborted",
            "system",
            { reason: successful ? "lease-create-recovered" : "lease-create-terminal" },
          );
        }
        if (!expired) return current;
        await transaction.put(key, {
          ...claim,
          expiresAt: new Date(now + checkpointUseClaimTTLMS).toISOString(),
        } satisfies CoordinatorCheckpointUseClaim);
        return await writeCheckpointTransition(
          transaction,
          current,
          current,
          "checkpoint.use.provisioning_extended",
          "system",
        );
      }
      if (!expired) return current;
      await transaction.delete(key);
      return await writeCheckpointTransition(
        transaction,
        current,
        { ...current, activeUseCount: Math.max(0, current.activeUseCount - 1) },
        "checkpoint.use.expired",
        "system",
      );
    }, Promise.resolve(record));
  });
}

export async function recordCheckpointExpiryDue(
  storage: CoordinatorStorage,
  checkpointID: string,
): Promise<CoordinatorCheckpointRecord> {
  return storage.transaction(async (transaction) => {
    const record = requireVisible(
      await transaction.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID)),
    );
    if (
      record.state !== "ready" ||
      record.retention.mode !== "expire-unused" ||
      record.pinCount > 0 ||
      record.activeUseCount > 0 ||
      Date.parse(record.lastUsedAt) + record.retention.unusedForSeconds * 1000 > Date.now()
    ) {
      throw new CheckpointError(
        "checkpoint_pending",
        "checkpoint is not eligible for unused expiry",
      );
    }
    return writeCheckpointTransition(
      transaction,
      record,
      record,
      "checkpoint.expiry.due",
      "system",
      { reason: "unused-expiry" },
    );
  });
}

export async function pruneCheckpointTombstone(
  storage: CoordinatorStorage,
  checkpointID: string,
): Promise<boolean> {
  return storage.transaction(async (transaction) => {
    const record = await transaction.get<CoordinatorCheckpointRecord>(checkpointKey(checkpointID));
    if (!record || record.state !== "deleted") return false;
    if (Date.parse(record.deletedAt ?? "") + checkpointAuditRetentionMS > Date.now()) return false;
    const latest = await transaction.get<CoordinatorCheckpointEvent>(
      checkpointEventKey(record.id, record.eventSequence),
    );
    const pruning =
      latest?.type === "checkpoint.audit.pruned"
        ? record
        : await writeCheckpointTransition(
            transaction,
            record,
            record,
            "checkpoint.audit.pruned",
            "system",
          );
    const events = await transaction.list<CoordinatorCheckpointEvent>({
      prefix: `checkpoint-event:${checkpointID}:`,
      limit: maxAuditPruneBatch,
    });
    await Promise.all([...events.keys()].map(async (key) => await transaction.delete(key)));
    if (events.size === maxAuditPruneBatch) return false;
    if (pruning.nextSweepAt)
      await transaction.delete(checkpointDueKey(pruning.id, pruning.nextSweepAt));
    await transaction.delete(checkpointKey(pruning.id));
    return true;
  });
}

export async function repairCheckpointDueMarker(
  storage: CoordinatorStorage,
  key: string,
  marker: CoordinatorCheckpointDueIndex,
): Promise<CoordinatorCheckpointRecord | undefined> {
  return storage.transaction(async (transaction) => {
    const record = await transaction.get<CoordinatorCheckpointRecord>(
      checkpointKey(marker.checkpointID),
    );
    if (
      !record ||
      record.revision !== marker.revision ||
      record.generation !== marker.generation ||
      record.nextSweepAt !== marker.nextSweepAt ||
      checkpointDueKey(marker.checkpointID, marker.nextSweepAt) !== key
    ) {
      await transaction.delete(key);
      return undefined;
    }
    return record;
  });
}

export function publicCheckpointRecord(
  record: CoordinatorCheckpointRecord,
): Record<string, unknown> {
  const { createClaim: _createClaim, deleteClaim: _deleteClaim, org: _org, ...visible } = record;
  void _createClaim;
  void _deleteClaim;
  void _org;
  return visible;
}
