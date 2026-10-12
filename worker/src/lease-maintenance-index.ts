import type { CoordinatorStorage } from "./coordinator-runtime";
import {
  leaseIndexes,
  leaseMaintenanceIndex,
  type LeaseIndexDescriptor,
} from "./lease-record-store";
import { storageRecordScanBatchSize } from "./storage-scan";
import type { LeaseRecord } from "./types";

export const leaseMaintenanceVersion = leaseMaintenanceIndex.version;
export const leaseMaintenanceStateKey = leaseMaintenanceIndex.stateKey;
export const leaseMaintenancePagesPerPass = 32;
export const leaseMaintenanceAuditIntervalMs = 24 * 60 * 60 * 1000;
// Stale entries only cost a filtered read, so sweep them periodically, not every pass.
export const leaseIndexPruneIntervalMs = 10 * 60 * 1000;

export interface LeaseMaintenanceState {
  version: number;
  complete: boolean;
  startAfter?: string;
  scanned?: number;
  members?: number;
  nextAuditAt?: number;
  // An empty cursor marks the start of an audit; undefined means no active audit.
  auditStartAfter?: string;
  auditScanned?: number;
  auditAdded?: number;
  pruneStartAfter?: string;
  nextPruneAt?: number;
  // Runtime generation that last maintained the index.
  generation?: string;
}

export function leaseMaintenanceIndexReady(
  state: LeaseMaintenanceState | undefined,
  index: LeaseIndexDescriptor = leaseMaintenanceIndex,
): boolean {
  return state?.version === index.version && state.complete;
}

// Only maintenance prunes. Rechecking inside a retryable transaction prevents a
// stale page from deleting membership published by a concurrent lease writer.
async function pruneStaleEntries(
  storage: CoordinatorStorage,
  state: LeaseMaintenanceState,
  index: LeaseIndexDescriptor,
  now: number,
) {
  // A started sweep continues across passes; a new sweep waits for its interval.
  if (state.pruneStartAfter === undefined && (state.nextPruneAt ?? 0) > now) return;
  /* oxlint-disable eslint/no-await-in-loop -- bounded cursor pages, each fenced reread precedes deletion. */
  for (let pageNumber = 0; pageNumber < leaseMaintenancePagesPerPass; pageNumber++) {
    const page = await storage.list<number>({
      prefix: index.prefix,
      limit: storageRecordScanBatchSize,
      noCache: true,
      allowConcurrency: true,
      ...(state.pruneStartAfter ? { startAfter: state.pruneStartAfter } : {}),
    });
    for (const key of page.keys()) {
      const id = key.slice(index.prefix.length);
      const lease = await storage.get<LeaseRecord>(`lease:${id}`, { noCache: true });
      if (!lease || !index.member(lease)) {
        await storage.transaction(async (transaction) => {
          const current = await transaction.get<LeaseRecord>(`lease:${id}`, { noCache: true });
          if (!current || !index.member(current)) await transaction.delete(key);
        });
      }
    }
    const cursor = [...page.keys()].at(-1);
    if (cursor) state.pruneStartAfter = cursor;
    if (page.size < storageRecordScanBatchSize) {
      delete state.pruneStartAfter;
      state.nextPruneAt = now + leaseIndexPruneIntervalMs;
      break;
    }
  }
  /* oxlint-enable eslint/no-await-in-loop */
}

export async function maintainLeaseIndexes(
  storage: CoordinatorStorage,
  now: number,
  generation?: string,
): Promise<void> {
  for (const index of leaseIndexes) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- each index advances its own bounded pass.
    await maintainLeaseIndex(storage, index, now, generation);
  }
}

export async function maintainLeaseMaintenanceIndex(
  storage: CoordinatorStorage,
  now: number,
  generation?: string,
): Promise<void> {
  await maintainLeaseIndex(storage, leaseMaintenanceIndex, now, generation);
}

export async function maintainLeaseIndex(
  storage: CoordinatorStorage,
  index: LeaseIndexDescriptor,
  now: number,
  generation?: string,
): Promise<void> {
  let state = await storage.get<LeaseMaintenanceState>(index.stateKey, { noCache: true });
  if (state?.version !== index.version) {
    state = { version: index.version, complete: false };
    await storage.put(index.stateKey, state);
  }
  // A new runtime may follow code that wrote leases without the index (a rollback),
  // or a non-coalescing store that crashed between a lease write and its index write.
  const newGeneration = generation !== undefined && state.generation !== generation;
  if (newGeneration) state.generation = generation;
  if (newGeneration && !state.complete) {
    // Rolled-back writes may also sit before an unfinished backfill's cursor.
    delete state.startAfter;
    state.scanned = 0;
    state.members = 0;
  }
  // A new generation restarts even an active audit: rolled-back writes may sit
  // before its cursor.
  if (
    state.complete &&
    (newGeneration || (state.auditStartAfter === undefined && (state.nextAuditAt ?? 0) <= now))
  ) {
    state.auditStartAfter = "";
    state.auditScanned = 0;
    state.auditAdded = 0;
  }
  if (!state.complete || state.auditStartAfter !== undefined) {
    const audit = state.complete;
    /* oxlint-disable eslint/no-await-in-loop -- persist each bounded page before advancing its cursor. */
    for (let pageNumber = 0; pageNumber < leaseMaintenancePagesPerPass; pageNumber++) {
      const startAfter = audit ? state.auditStartAfter : state.startAfter;
      const page = await storage.list<LeaseRecord>({
        prefix: "lease:",
        limit: storageRecordScanBatchSize,
        noCache: true,
        allowConcurrency: true,
        ...(startAfter ? { startAfter } : {}),
      });
      const members = [...page.values()].filter(index.member);
      const additions = await Promise.all(
        members.map(async (lease) => {
          const key = `${index.prefix}${lease.id}`;
          if (audit) {
            // Count only missing current members, not a stale page whose lease
            // a concurrent writer correctly settled and removed from the index.
            return storage.transaction(async (transaction) => {
              const current = await transaction.get<LeaseRecord>(`lease:${lease.id}`, {
                noCache: true,
              });
              if (!current || !index.member(current)) return 0;
              if ((await transaction.get(key, { noCache: true })) !== undefined) return 0;
              await transaction.put(key, 1, { noCache: true });
              return 1;
            });
          }
          // Backfill is add-only; pruning separately fences a current reread.
          await storage.put(key, 1, { noCache: true });
          return 0;
        }),
      );
      const cursor = [...page.keys()].at(-1);
      if (audit) {
        if (cursor) state.auditStartAfter = cursor;
        state.auditScanned = (state.auditScanned ?? 0) + page.size;
        state.auditAdded = (state.auditAdded ?? 0) + additions.reduce<number>((a, b) => a + b, 0);
      } else {
        if (cursor) state.startAfter = cursor;
        state.scanned = (state.scanned ?? 0) + page.size;
        state.members = (state.members ?? 0) + members.length;
      }
      const finished = page.size < storageRecordScanBatchSize;
      if (finished) {
        state.complete = true;
        state.nextAuditAt = now + leaseMaintenanceAuditIntervalMs;
        delete state.startAfter;
        delete state.auditStartAfter;
      }
      await storage.put(index.stateKey, state);
      if (finished) {
        console.log(
          JSON.stringify({
            event: "coordinator.lease_maintenance_index",
            index: index.name,
            phase: audit ? "audit_complete" : "backfill_complete",
            scanned: audit ? state.auditScanned : state.scanned,
            ...(audit ? { added: state.auditAdded } : { members: state.members }),
          }),
        );
        break;
      }
    }
    /* oxlint-enable eslint/no-await-in-loop */
  }
  await pruneStaleEntries(storage, state, index, now);
  await storage.put(index.stateKey, state);
}
