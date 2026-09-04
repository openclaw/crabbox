import {
  provisioningDuePrefix,
  type CoordinatorRuntime,
  type CoordinatorStorageView,
  type ProvisioningDueRecord,
} from "./coordinator-runtime";
import type {
  FrozenProvisioningPlan,
  ProviderResumableProvisioning,
  ProvisioningPublication,
  ProvisioningStep,
} from "./provider-provisioning";
import {
  openProvisioningMaterial,
  provisioningMaterialKey,
  type ProvisioningMaterialBinding,
  type SealedProvisioningMaterial,
} from "./provisioning-material";
import type { CreateAttemptRecord, Env, LeaseRecord, Provider } from "./types";

const claimDuration = 45_000;
const retryDelay = 15_000;
const maxRecordBytes = 100 * 1024;

export interface LeaseProvisioningOperation extends ProvisioningMaterialBinding {
  owner: string;
  org: string;
  provider: Provider;
  createdAt: number;
  deadline: number;
  revision: number;
  step: ProvisioningStep;
  claim?: { id: string; expiresAt: number };
  canceledAt?: number;
  retain?: boolean;
  fixedRequestFingerprint?: string;
}

export function provisioningOperationKey(leaseID: string): string {
  return `lease-provisioning:${leaseID}`;
}

export function provisioningPlanKey(operationID: string): string {
  return `provisioning-plan:${operationID}`;
}

export function provisioningAttemptKey(
  operation: Pick<LeaseProvisioningOperation, "operationID" | "step">,
): string {
  return `provisioning-attempt:${operation.operationID}:${operation.step.attempt}`;
}

interface ProvisioningAttemptJournal {
  schema: 1;
  generation: string;
  attempt: number;
  revision: number;
  state: unknown;
}

export function provisioningDueKey(operation: LeaseProvisioningOperation): string {
  return `${provisioningDuePrefix}${Math.trunc(operation.step.nextWake).toString().padStart(16, "0")}:${operation.leaseID}`;
}

export function validateProvisioningRecord(value: unknown): void {
  if (new TextEncoder().encode(JSON.stringify(value)).length > maxRecordBytes) {
    throw new Error("provisioning record exceeds storage limit");
  }
}

function validOperation(value: unknown): value is LeaseProvisioningOperation {
  if (!value || typeof value !== "object") return false;
  const operation = value as LeaseProvisioningOperation;
  return (
    operation.schema === 1 &&
    [
      operation.leaseID,
      operation.operationID,
      operation.generation,
      operation.owner,
      operation.org,
      operation.provider,
      operation.scope,
    ].every((field) => typeof field === "string" && field.length > 0) &&
    (operation.canceledAt === undefined || Number.isFinite(operation.canceledAt)) &&
    Number.isSafeInteger(operation.revision) &&
    operation.revision >= 0 &&
    Number.isFinite(operation.createdAt) &&
    Number.isFinite(operation.deadline) &&
    operation.deadline >= operation.createdAt &&
    Boolean(
      operation.step &&
      typeof operation.step === "object" &&
      [
        "prepared",
        "provisioning",
        "ready-to-publish",
        "settling",
        "cleanup",
        "blocked",
        "retained",
        "terminal",
      ].includes(operation.step.phase) &&
      Number.isSafeInteger(operation.step.attempt) &&
      operation.step.attempt >= 0 &&
      Number.isFinite(operation.step.nextWake),
    ) &&
    (!operation.claim ||
      (typeof operation.claim.id === "string" && Number.isFinite(operation.claim.expiresAt)))
  );
}

function validJournal(
  journal: ProvisioningAttemptJournal | undefined,
  operation: LeaseProvisioningOperation,
): journal is ProvisioningAttemptJournal {
  return Boolean(
    journal &&
    journal.schema === 1 &&
    journal.generation === operation.generation &&
    journal.attempt === operation.step.attempt &&
    journal.revision === operation.revision,
  );
}

async function quarantineDue(
  transaction: CoordinatorStorageView,
  key: string,
  leaseID: string,
): Promise<void> {
  // Leave the unsupported operation and identity evidence intact, off the runnable queue.
  await transaction.put(`provisioning-quarantine:${leaseID}`, {
    schema: 1,
    dueKey: key,
    observedAt: Date.now(),
    reason: "unsupported_or_inconsistent_record",
  });
  await transaction.delete(key);
}

export async function putProvisioningOperation(
  transaction: CoordinatorStorageView,
  operation: LeaseProvisioningOperation,
  previous?: LeaseProvisioningOperation,
): Promise<void> {
  if (!validOperation(operation)) throw new Error("provisioning operation schema invalid");
  validateProvisioningRecord(operation);
  if (previous) await transaction.delete(provisioningDueKey(previous));
  const reference = { journal: provisioningAttemptKey(operation) };
  if (JSON.stringify(operation.step.state) !== JSON.stringify(reference)) {
    const journal: ProvisioningAttemptJournal = {
      schema: 1,
      generation: operation.generation,
      attempt: operation.step.attempt,
      revision: operation.revision,
      state: operation.step.state,
    };
    validateProvisioningRecord(journal);
    await transaction.put(reference.journal, journal);
  } else {
    const journal = await transaction.get<ProvisioningAttemptJournal>(reference.journal);
    if (!journal || !previous || !validJournal(journal, previous))
      throw new Error("provisioning journal revision changed");
    await transaction.put(reference.journal, { ...journal, revision: operation.revision });
  }
  await transaction.put(provisioningOperationKey(operation.leaseID), {
    ...operation,
    step: { ...operation.step, state: reference },
  });
  if (operation.step.phase !== "terminal" && operation.step.phase !== "retained") {
    const due: ProvisioningDueRecord = {
      operationID: operation.leaseID,
      at: operation.step.nextWake,
    };
    await transaction.put(provisioningDueKey(operation), due);
  }
  if (
    operation.canceledAt !== undefined &&
    !(await transaction.get(`provisioning-quarantine:${operation.leaseID}`))
  ) {
    const plan = await transaction.get<FrozenProvisioningPlan>(
      provisioningPlanKey(operation.operationID),
    );
    if (
      plan?.version === 1 &&
      plan.provider === operation.provider &&
      plan.scope === operation.scope
    ) {
      // Durable cancellation permits observation/deletion only, never regeneration of replay inputs.
      await transaction.delete(provisioningMaterialKey(operation.operationID));
    }
  }
}

export async function provisioningOwnsLease(
  storage: CoordinatorStorageView,
  leaseID: string,
): Promise<boolean> {
  const operation = await storage.get<LeaseProvisioningOperation>(
    provisioningOperationKey(leaseID),
  );
  return Boolean(operation && (!validOperation(operation) || operation.step.phase !== "terminal"));
}

export async function cancelProvisioningOperation(
  transaction: CoordinatorStorageView,
  lease: LeaseRecord,
  at: number,
  options: { deleteServer: boolean; keep?: boolean } = { deleteServer: true },
): Promise<LeaseRecord | undefined> {
  const operation = await transaction.get<LeaseProvisioningOperation>(
    provisioningOperationKey(lease.id),
  );
  if (operation && !validOperation(operation))
    throw new Error("provisioning operation schema invalid");
  if (!operation || operation.step.phase === "terminal" || !matchesLease(operation, lease))
    return undefined;
  const previous = structuredClone(operation);
  // Once destructive cleanup is requested, a later retention request cannot undo it.
  operation.retain = options.deleteServer === false && lease.releaseDeletesServer !== true;
  operation.canceledAt ??= at;
  if (operation.step.phase === "retained" && !operation.retain) operation.step.phase = "settling";
  operation.step.nextWake = operation.claim?.expiresAt ?? at;
  await putProvisioningOperation(transaction, operation, previous);
  const released: LeaseRecord = {
    ...lease,
    state: "released",
    releasedAt: new Date(at).toISOString(),
    endedAt: new Date(at).toISOString(),
    updatedAt: new Date(at).toISOString(),
    releaseDeletesServer: !operation.retain,
    ...(options.keep === undefined ? {} : { keep: options.keep }),
    provisioningResourceMayExist: true,
  };
  updateCleanupMetadata(released, operation.step.phase, at);
  await transaction.put(`lease:${lease.id}`, released);
  return released;
}

export class LeaseProvisioningController {
  constructor(
    private readonly runtime: CoordinatorRuntime,
    private readonly env: Env,
    private readonly provider: (provider: Provider) => ProviderResumableProvisioning | undefined,
    private readonly publish: (
      transaction: CoordinatorStorageView,
      lease: LeaseRecord,
      result: ProvisioningPublication,
    ) => Promise<void>,
  ) {}

  async tick(): Promise<void> {
    const due = await this.runtime.storage.list<ProvisioningDueRecord>({
      prefix: provisioningDuePrefix,
      limit: 4,
    });
    const runtime = this.runtime.provisioning;
    if (!runtime) return;
    const runnable = await runtime.commitAndWake(async (transaction) => {
      const leases: string[] = [];
      for (const [key, entry] of due) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- bounded due reconciliation is one transaction.
        const current = await transaction.get<ProvisioningDueRecord>(key);
        if (!current || JSON.stringify(current) !== JSON.stringify(entry)) continue;
        if (!entry || typeof entry !== "object") {
          // oxlint-disable-next-line eslint/no-await-in-loop -- malformed index values never grant ownership.
          await transaction.delete(key);
          continue;
        }
        const leaseID =
          typeof entry.operationID === "string"
            ? entry.operationID
            : key.slice(key.lastIndexOf(":") + 1);
        // oxlint-disable-next-line eslint/no-await-in-loop -- every index entry is checked against its authoritative record.
        const operation = await transaction.get<LeaseProvisioningOperation>(
          provisioningOperationKey(leaseID),
        );
        if (operation && !validOperation(operation)) {
          // oxlint-disable-next-line eslint/no-await-in-loop -- unsupported records retain their evidence but cannot starve the queue.
          await quarantineDue(transaction, key, leaseID);
        } else if (
          !operation ||
          operation.step.phase === "terminal" ||
          operation.step.phase === "retained" ||
          provisioningDueKey(operation) !== key ||
          entry.at !== operation.step.nextWake
        ) {
          // oxlint-disable-next-line eslint/no-await-in-loop -- deleting a stale index entry does not alter its operation.
          await transaction.delete(key);
        } else if (entry.at <= Date.now()) leases.push(leaseID);
      }
      return leases;
    });
    await Promise.all(runnable.map((leaseID) => this.advance(leaseID)));
  }

  async advance(leaseID: string): Promise<void> {
    const runtime = this.runtime.provisioning;
    if (!runtime) throw new Error("durable provisioning runtime unavailable");
    const now = Date.now();
    const claimID = crypto.randomUUID();
    const claimed = await runtime.commitAndWake(async (transaction) => {
      const operation = await transaction.get<LeaseProvisioningOperation>(
        provisioningOperationKey(leaseID),
      );
      if (
        !validOperation(operation) ||
        operation.step.phase === "terminal" ||
        operation.step.phase === "retained" ||
        operation.step.nextWake > now ||
        (operation.claim && operation.claim.expiresAt > now)
      )
        return undefined;
      const journal = await transaction.get<ProvisioningAttemptJournal>(
        provisioningAttemptKey(operation),
      );
      if (
        !validJournal(journal, operation) ||
        (operation.step.state as { journal?: unknown })?.journal !==
          provisioningAttemptKey(operation)
      ) {
        await quarantineDue(transaction, provisioningDueKey(operation), leaseID);
        return undefined;
      }
      operation.step.state = journal.state;
      const lease = await transaction.get<LeaseRecord>(`lease:${leaseID}`);
      if (!lease || !matchesLease(operation, lease)) {
        const previous = structuredClone(operation);
        operation.step.phase = "blocked";
        operation.step.blockedReason = "lease_binding_changed";
        operation.step.nextWake = now + retryDelay;
        delete operation.claim;
        await putProvisioningOperation(transaction, operation, previous);
        return undefined;
      }
      const attempt = lease.createAttemptID
        ? await transaction.get<CreateAttemptRecord>(`create-attempt:${leaseID}`)
        : undefined;
      const canceled =
        operation.canceledAt !== undefined ||
        lease.state !== "provisioning" ||
        Date.parse(lease.expiresAt) <= now ||
        now >= operation.deadline ||
        (lease.createAttemptID !== undefined && !matchesAttempt(attempt, lease));
      const previous = structuredClone(operation);
      if (canceled) operation.canceledAt ??= now;
      if (canceled && lease.state === "provisioning") {
        lease.state = "released";
        lease.releaseDeletesServer = true;
        lease.provisioningResourceMayExist = true;
        lease.updatedAt = new Date(now).toISOString();
        updateCleanupMetadata(lease, operation.step.phase, now);
        await transaction.put(`lease:${leaseID}`, lease);
      }
      if (operation.step.coordinationKey) {
        const key = `provisioning-lock:${operation.step.coordinationKey}`;
        const owner = await transaction.get<string>(key);
        if (owner && owner !== operation.operationID) {
          operation.step.nextWake = now + retryDelay;
          await putProvisioningOperation(transaction, operation, previous);
          return undefined;
        }
        await transaction.put(key, operation.operationID);
      }
      if (operation.step.phase === "ready-to-publish" && !canceled && operation.step.publication) {
        await this.publish(transaction, lease, operation.step.publication);
        operation.step = { ...operation.step, phase: "terminal" };
        operation.revision += 1;
        await putProvisioningOperation(transaction, operation, previous);
        await transaction.delete(provisioningMaterialKey(operation.operationID));
        return undefined;
      }
      operation.revision += 1;
      operation.step.delivery = (operation.step.delivery ?? 0) + 1;
      operation.claim = { id: claimID, expiresAt: now + claimDuration };
      // A killed process leaves a durable due entry at claim expiry, before any provider dispatch.
      operation.step.nextWake = operation.claim.expiresAt;
      await putProvisioningOperation(transaction, operation, previous);
      const plan = await transaction.get<FrozenProvisioningPlan>(
        provisioningPlanKey(operation.operationID),
      );
      const sealed =
        operation.canceledAt === undefined
          ? await transaction.get<SealedProvisioningMaterial>(
              provisioningMaterialKey(operation.operationID),
            )
          : undefined;
      return { operation, lease, plan, sealed };
    });
    if (!claimed) return;
    const { operation, lease, plan, sealed } = claimed;
    let result: ProvisioningStep;
    try {
      const capability = this.provider(operation.provider);
      if (
        !capability ||
        !plan ||
        plan.version !== 1 ||
        plan.provider !== operation.provider ||
        plan.scope !== operation.scope
      )
        throw new Error("provisioning capability unavailable");
      const replay =
        operation.canceledAt === undefined
          ? {
              canceled: false as const,
              material: await openProvisioningMaterial(this.env, operation, sealed),
            }
          : { canceled: true as const };
      result = await capability.advance({
        plan,
        step: structuredClone(operation.step),
        ...replay,
        lease,
        deadline: operation.deadline,
        retain: operation.retain === true,
        recovering: operation.step.delivery! > 1,
      });
      validateProvisioningRecord(result);
    } catch {
      result = {
        ...operation.step,
        phase: "blocked",
        blockedReason: "continuation_unavailable",
        nextWake: Date.now() + retryDelay,
      };
    }
    await runtime.commitAndWake(async (transaction) => {
      const current = await transaction.get<LeaseProvisioningOperation>(
        provisioningOperationKey(leaseID),
      );
      if (
        !validOperation(current) ||
        current.generation !== operation.generation ||
        current.revision !== operation.revision ||
        current.claim?.id !== claimID ||
        current.step.attempt !== operation.step.attempt
      )
        return;
      const latest = await transaction.get<LeaseRecord>(`lease:${leaseID}`);
      if (!latest || !matchesLease(current, latest)) return;
      const attempt = latest.createAttemptID
        ? await transaction.get<CreateAttemptRecord>(`create-attempt:${leaseID}`)
        : undefined;
      const canceled =
        current.canceledAt !== undefined ||
        latest.state !== "provisioning" ||
        Date.parse(latest.expiresAt) <= Date.now() ||
        Date.now() >= current.deadline ||
        (latest.createAttemptID !== undefined && !matchesAttempt(attempt, latest));
      const previous = structuredClone(current);
      if (current.step.coordinationKey && current.step.coordinationKey !== result.coordinationKey) {
        const key = `provisioning-lock:${current.step.coordinationKey}`;
        if ((await transaction.get(key)) === current.operationID) await transaction.delete(key);
      }
      if (canceled) current.canceledAt ??= Date.now();
      // Preserve returned identity evidence after cancellation. Only a later claim may publish.
      current.step = result;
      if (canceled && result.phase === "ready-to-publish") current.step.phase = "settling";
      if (result.phase === "retained" && !current.retain) {
        current.step.phase = "settling";
        current.step.nextWake = Date.now();
      }
      current.revision += 1;
      delete current.claim;
      await putProvisioningOperation(transaction, current, previous);
      if (canceled) updateCleanupMetadata(latest, current.step.phase, Date.now());
      if (result.phase === "terminal") {
        await transaction.delete(provisioningMaterialKey(current.operationID));
        latest.state = canceled ? "released" : "failed";
        latest.endedAt = new Date().toISOString();
        latest.updatedAt = latest.endedAt;
        latest.provisioningResourceMayExist = false;
        latest.releaseDeletesServer = true;
        if (!canceled)
          latest.failureError = "provisioning candidates exhausted after verified cleanup";
      }
      if (canceled || result.phase === "terminal")
        await transaction.put(`lease:${leaseID}`, latest);
    });
  }
}

function matchesLease(operation: LeaseProvisioningOperation, lease: LeaseRecord): boolean {
  return (
    operation.leaseID === lease.id &&
    operation.owner === lease.owner &&
    operation.org === lease.org &&
    operation.provider === lease.provider &&
    operation.scope === lease.providerScope &&
    operation.generation === lease.createAttemptGeneration
  );
}

function matchesAttempt(attempt: CreateAttemptRecord | undefined, lease: LeaseRecord): boolean {
  return Boolean(
    attempt &&
    attempt.state === "pending" &&
    attempt.canonicalLeaseID === lease.id &&
    attempt.token === lease.createAttemptID &&
    attempt.generation === lease.createAttemptGeneration &&
    attempt.owner === lease.owner &&
    attempt.org === lease.org,
  );
}

function updateCleanupMetadata(
  lease: LeaseRecord,
  phase: ProvisioningStep["phase"],
  at: number,
): void {
  if (phase === "terminal" || lease.releaseDeletesServer === false) {
    delete lease.cleanupStartedAt;
    delete lease.cleanupClaimExpiresAt;
    delete lease.cleanupError;
    delete lease.cleanupFailedAt;
    delete lease.cleanupRetryAt;
    delete lease.cleanupAttempts;
  } else if (phase === "blocked") {
    delete lease.cleanupStartedAt;
    delete lease.cleanupClaimExpiresAt;
    lease.cleanupError = "durable provisioning cleanup requires resolution";
    lease.cleanupFailedAt = new Date(at).toISOString();
    lease.cleanupAttempts = (lease.cleanupAttempts ?? 0) + 1;
  } else {
    lease.cleanupStartedAt ??= new Date(at).toISOString();
    lease.cleanupClaimExpiresAt = new Date(at + claimDuration).toISOString();
    delete lease.cleanupError;
    delete lease.cleanupFailedAt;
    delete lease.cleanupRetryAt;
    delete lease.cleanupAttempts;
  }
}
