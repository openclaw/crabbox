import type { CoordinatorStorage, CoordinatorStorageView } from "./coordinator-runtime";
import type { RunRecord } from "./types";

const stateKey = "run-log-storage:state";
const terminalPrefix = "run-log-storage:terminal:";
const reservationPrefix = "run-log-storage:reservation:";
const pageSize = 128;
const evictionBatchSize = 128;
const defaultBudget = 512 * 1024 * 1024;

interface LogStorageState {
  bytes: number;
  ready: boolean;
  cursor?: string | undefined;
  warned?: boolean;
}
interface Reservation {
  prefix: string;
  bytes: number;
  createdAt: number;
}

export function runLogBudget(value?: string): number {
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : defaultBudget;
}

function terminalKey(run: RunRecord): string {
  return `${terminalPrefix}${run.endedAt ?? run.lastEventAt ?? run.startedAt}:${run.id}`;
}

/** Payload accounting uses the same transactional store in both coordinator runtimes. */
export class RunLogStorage {
  private lastWarningAt = 0;
  private readonly activeReservations = new Set<string>();

  constructor(
    private readonly storage: CoordinatorStorage,
    private readonly budget: number,
    private readonly reading: (id: string) => boolean,
  ) {}

  private async state(storage: CoordinatorStorageView): Promise<LogStorageState> {
    const current = await storage.get<LogStorageState>(stateKey);
    if (current) return current;
    // A fresh coordinator needs no migration. Existing payloads are accounted only by maintenance.
    const payloads = await storage.list({ prefix: "runlog:", limit: 1, noCache: true });
    return { bytes: 0, ready: payloads.size === 0 };
  }

  async capacity() {
    const state = await this.state(this.storage);
    return {
      storedBytes: state.bytes,
      budgetBytes: this.budget,
      accountingComplete: state.ready,
      pressure: !state.ready || state.bytes >= this.budget * 0.8,
    };
  }

  private async save(storage: CoordinatorStorageView, state: LogStorageState): Promise<void> {
    state.warned = state.bytes >= this.budget * 0.8;
    await storage.put(stateKey, state);
  }

  async warn(): Promise<void> {
    const state = await this.storage.get<LogStorageState>(stateKey);
    if (state?.warned && Date.now() - this.lastWarningAt >= 60_000) {
      this.lastWarningAt = Date.now();
      console.warn(
        JSON.stringify({
          event: "coordinator.run_log_storage_pressure",
          storedBytes: state.bytes,
          budgetBytes: this.budget,
          accountingComplete: state.ready,
        }),
      );
    }
  }

  async reserve(prefix: string, bytes: number): Promise<boolean> {
    if (bytes > this.budget) return false;
    this.activeReservations.add(prefix);
    try {
      await this.evictToFit(bytes);
      const reserved = await this.storage.transaction(async (storage) => {
        const state = await this.state(storage);
        if (!state.ready || state.bytes + bytes > this.budget) return false;
        state.bytes += bytes;
        await storage.put(`${reservationPrefix}${prefix}`, {
          prefix,
          bytes,
          createdAt: Date.now(),
        });
        await this.save(storage, state);
        return true;
      });
      if (!reserved) this.release(prefix);
      return reserved;
    } catch (error) {
      this.activeReservations.delete(prefix);
      throw error;
    }
  }

  async account(storage: CoordinatorStorageView, run: RunRecord): Promise<void> {
    if (run.storedLogBytes !== undefined) return;
    const state = await this.state(storage);
    run.storedLogBytes = run.logEvicted ? 0 : Math.max(0, run.logBytes || 0);
    state.bytes += run.storedLogBytes;
    await this.save(storage, state);
  }

  async index(storage: CoordinatorStorageView, run: RunRecord): Promise<void> {
    if (run.state !== "running" && (run.storedLogBytes ?? 0) > 0) {
      await storage.put(terminalKey(run), run.id);
    }
  }

  async commit(storage: CoordinatorStorageView, run: RunRecord, prefix: string, retained: boolean) {
    // The reservation already counts staged bytes. Publish its ownership atomically with the run.
    if (retained) {
      const reservation = await storage.get<Reservation>(`${reservationPrefix}${prefix}`);
      if (!reservation) throw new Error("run log reservation missing");
      run.storedLogBytes = (run.storedLogBytes ?? 0) + reservation.bytes;
      await storage.delete(`${reservationPrefix}${prefix}`);
    } else {
      run.logEvicted = "storage_pressure";
      run.storedLogBytes ??= 0;
    }
    if (run.storedLogBytes > 0) await storage.put(terminalKey(run), run.id);
  }

  release(prefix: string): void {
    this.activeReservations.delete(prefix);
  }

  async discard(prefix: string): Promise<void> {
    await this.storage.transaction(async (storage) => {
      const reservation = await storage.get<Reservation>(`${reservationPrefix}${prefix}`);
      if (!reservation) return;
      await this.deletePayload(storage, prefix);
      const state = await this.state(storage);
      state.bytes = Math.max(0, state.bytes - reservation.bytes);
      await storage.delete(`${reservationPrefix}${prefix}`);
      await this.save(storage, state);
    });
    this.release(prefix);
  }

  private async deletePayload(storage: CoordinatorStorageView, prefix: string): Promise<void> {
    // An 8 MiB log has at most 128 chunks. Include obsolete staged attempts in bounded pages.
    for (;;) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- delete a bounded page before advancing.
      const page = await storage.list({ prefix, limit: pageSize, noCache: true });
      // oxlint-disable-next-line eslint/no-await-in-loop -- atomic deletion and byte accounting.
      await Promise.all([...page.keys()].map((key) => storage.delete(key)));
      if (page.size < pageSize) return;
    }
  }

  async evict(
    id: string,
    reason: "storage_pressure" | "retention",
    cutoff?: number,
    indexKey?: string,
  ): Promise<boolean> {
    return this.storage.transaction(async (storage) => {
      const run = await storage.get<RunRecord>(`run:${id}`);
      if (!run || (indexKey && indexKey !== terminalKey(run))) {
        if (indexKey) await storage.delete(indexKey);
        return false;
      }
      if (run.state === "running" || this.reading(id)) return false;
      const ended = Date.parse(run.endedAt ?? run.lastEventAt ?? run.startedAt);
      if (cutoff !== undefined && (!Number.isFinite(ended) || ended > cutoff)) return false;
      if (run.logEvicted && !run.storedLogBytes) {
        if (indexKey) await storage.delete(indexKey);
        return false;
      }
      await this.deletePayload(storage, `runlog:${id}:`);
      await storage.delete(`runlog:${id}`);
      const state = await this.state(storage);
      state.bytes = Math.max(0, state.bytes - (run.storedLogBytes ?? 0));
      run.storedLogBytes = 0;
      run.logEvicted = reason;
      await storage.put(`run:${id}`, run);
      await storage.delete(terminalKey(run));
      await this.save(storage, state);
      return true;
    });
  }

  private async evictToFit(incoming: number): Promise<void> {
    const state = await this.state(this.storage);
    if (!state.ready || state.bytes + incoming <= this.budget) return;
    const initialBytes = state.bytes;
    const candidates = await this.storage.list<string>({
      prefix: terminalPrefix,
      limit: evictionBatchSize,
    });
    for (const [key, id] of candidates) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- oldest terminal payloads first, bounded by the index page.
      await this.evict(id, "storage_pressure", undefined, key);
      // oxlint-disable-next-line eslint/no-await-in-loop -- stop as soon as admission fits.
      const remaining = (await this.state(this.storage)).bytes;
      if (remaining + incoming <= this.budget || initialBytes - remaining >= 16 * 1024 * 1024)
        break;
    }
  }

  async maintain(): Promise<boolean> {
    await this.storage.transaction(async (storage) => {
      const state = await this.state(storage);
      if (state.ready) return;
      const page = await storage.list<RunRecord>({
        prefix: "run:",
        limit: pageSize,
        noCache: true,
        ...(state.cursor ? { startAfter: state.cursor } : {}),
      });
      for (const [key, run] of page) {
        if (key !== `run:${run.id}` || run.storedLogBytes !== undefined) continue;
        // Legacy logBytes is the UTF-8 payload length. Avoid reading bulk payloads during migration.
        run.storedLogBytes = run.logEvicted ? 0 : Math.max(0, run.logBytes || 0);
        state.bytes += run.storedLogBytes;
        // oxlint-disable-next-line eslint/no-await-in-loop -- bounded metadata-only migration transaction.
        await storage.put(key, run);
        if (run.state !== "running" && run.storedLogBytes > 0) {
          // oxlint-disable-next-line eslint/no-await-in-loop -- durable oldest-first index, published with accounting.
          await storage.put(terminalKey(run), run.id);
        }
      }
      state.cursor = [...page.keys()].at(-1) ?? state.cursor;
      state.ready = page.size < pageSize;
      await this.save(storage, state);
    });
    const reservations = await this.storage.list<Reservation>({
      prefix: reservationPrefix,
      limit: 16,
    });
    for (const reservation of reservations.values()) {
      if (
        reservation.createdAt < Date.now() - 60 * 60_000 &&
        !this.activeReservations.has(reservation.prefix)
      ) {
        // oxlint-disable-next-line eslint/no-await-in-loop -- reclaim interrupted staging attempts in bounded batches.
        await this.discard(reservation.prefix);
      }
    }
    await this.warn();
    await this.evictToFit(0);
    const state = await this.state(this.storage);
    return !state.ready || state.bytes > this.budget;
  }
}
