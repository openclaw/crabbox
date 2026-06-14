import { DurableObject, WorkerEntrypoint } from "cloudflare:workers";

const runnerName = "cloudflare-dynamic-workers";
const defaultCompatibilityDate = "2026-06-12";
const runMetadataPrefix = "runs:";
const runIndexObjectPrefix = "__crabbox/run-index__";
const runIndexShardCount = 16;
const runIndexMarkerStorageKey = "run-index";
const runIndexStoragePrefix = "run:";
const runIndexDeletionStoragePrefix = "deleted:";
const runMetadataBulkReadSize = 100;
const defaultRunReservationTTLSeconds = 15 * 60;
const runReservationRenewIntervalMs = 5 * 60 * 1000;
const runReservationGraceSeconds = 5 * 60;
const deletionTombstoneTTLms = 10 * 60 * 1000;
const deletionRetryDelayMs = 5 * 1000;
const kvWriteRetryDelayMs = 1100;
const kvWriteMaxAttempts = 3;
const maxConcurrentLegacyExecutions = 12;
const maxRememberedCompletions = 16;
const coordinationMaxMetadataEntries = 16;
const coordinationMaxMetadataKeyLength = 128;
const coordinationMaxMetadataValueLength = 512;
const coordinationMaxHeaderEntries = 32;
const coordinationMaxHeaderNameLength = 128;
const coordinationMaxHeaderValueLength = 1024;
const coordinationMaxLogEntries = 32;
const coordinationMaxLogMessageLength = 1024;
const coordinationMaxErrorMessageLength = 4096;
const retainedMaxLogEntries = 256;
const dynamicResponseBodyLimitBytes = 1024 * 1024;
const dynamicResponseBodyReadTimeoutMs = 30 * 1000;
const supportedCacheModes = ["one-shot", "stable", "explicit"] as const;
const supportedEgressModes = ["blocked", "intercept"] as const;

const executionStore = new Map<string, RunRecord>();

export class HttpGateway extends WorkerEntrypoint<Env, GatewayProps> {
  override async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const props = this.ctx.props;
    appendLog(props.executionId, {
      level: "info",
      message: `egress fetch ${url.origin}`,
      time: new Date().toISOString(),
    });

    if (props.allowHostnames.length > 0 && !props.allowHostnames.includes(url.hostname)) {
      appendLog(props.executionId, {
        level: "warn",
        message: `egress blocked ${url.hostname}`,
        time: new Date().toISOString(),
      });
      return Response.json({ error: "egress blocked" }, { status: 403 });
    }

    if (props.allowHostnames.length === 0) return fetch(request);
    const response = await fetch(new Request(request, { redirect: "manual" }));
    if ([301, 302, 303, 307, 308].includes(response.status)) {
      appendLog(props.executionId, {
        level: "warn",
        message: "egress redirect blocked",
        time: new Date().toISOString(),
      });
      void response.body?.cancel();
      return Response.json({ error: "egress redirect blocked" }, { status: 403 });
    }
    return response;
  }
}

export class LogTailer extends WorkerEntrypoint<Env, TailProps> {
  override tail(events: unknown[]): void {
    const props = this.ctx.props;
    this.ctx.waitUntil(
      persistTailLog(this.env, props.runId, props.executionId, {
        level: "info",
        message: "dynamic worker tail event received",
        time: new Date().toISOString(),
        eventCount: events.length,
      }),
    );
  }
}

async function persistTailLog(
  env: Env,
  runId: string,
  executionId: string,
  log: RunLog,
): Promise<void> {
  if (!env.RUN_COORDINATOR) return;
  const response = await runCoordinator(env, runId).fetch("https://run-coordinator/log", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ executionId, log }),
  });
  if (!response.ok) {
    throw new Error(`run coordinator log persistence failed: ${response.status}`);
  }
}

function appendRunLog(record: RunRecord, log: RunLog, limit = retainedMaxLogEntries): void {
  record.logs.push(log);
  if (record.logs.length > limit) {
    record.logs.splice(0, record.logs.length - limit);
  }
}

function tailLog(value: unknown): value is RunLog {
  return (
    isRecord(value) &&
    (value["level"] === "info" || value["level"] === "warn" || value["level"] === "error") &&
    typeof value["message"] === "string" &&
    typeof value["time"] === "string" &&
    (value["eventCount"] === undefined || typeof value["eventCount"] === "number")
  );
}

function mergeRunLogs(record: RunRecord, coordinated: RunRecord | undefined): void {
  if (coordinated === undefined) return;
  const seen = new Set(
    record.logs.map(
      (log) => `${log.time}\u0000${log.level}\u0000${log.message}\u0000${log.eventCount ?? ""}`,
    ),
  );
  for (const log of coordinated.logs) {
    const key = `${log.time}\u0000${log.level}\u0000${log.message}\u0000${log.eventCount ?? ""}`;
    if (!seen.has(key)) {
      appendRunLog(record, log);
      seen.add(key);
    }
  }
}

type TailProps = {
  runId: string;
  executionId: string;
};

type Env = {
  LOADER?: DynamicWorkerLoader;
  RUNS?: KVNamespace;
  RUN_COORDINATOR?: DurableObjectNamespace<DynamicWorkerRunCoordinator>;
  CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN?: string;
  CRABBOX_RUNNER_TOKEN?: string;
};

type DynamicWorkerContext = {
  waitUntil?: (promise: Promise<unknown>) => void;
  exports?: {
    HttpGateway?: (options: ExportOptions<GatewayProps>) => ServiceStub;
    LogTailer?: (options: ExportOptions<TailProps>) => ServiceStub;
  };
};

type DynamicWorkerLoader = {
  load(code: WorkerCode): WorkerStub;
  get(id: string, callback: () => Promise<WorkerCode>): WorkerStub;
};

type WorkerStub = {
  getEntrypoint(name?: string, options?: EntrypointOptions): WorkerEntrypointStub;
};

type WorkerEntrypointStub = {
  fetch(request: Request): Promise<Response>;
};

type WorkerCode = {
  compatibilityDate: string;
  compatibilityFlags?: string[];
  mainModule: string;
  modules: Record<string, WorkerModule>;
  globalOutbound?: ServiceStub | null;
  env?: Record<string, unknown>;
  tails?: ServiceStub[];
  limits?: WorkerLimits;
};

type ServiceStub = unknown;

type WorkerModule =
  | string
  | {
      js?: string;
      cjs?: string;
      py?: string;
      text?: string;
      json?: unknown;
    };

type WorkerLimits = {
  cpuMs?: number;
  subRequests?: number;
};

type EntrypointOptions = {
  limits?: WorkerLimits;
};

type ExportOptions<Props> = {
  props: Props;
};

type GatewayProps = {
  executionId: string;
  allowHostnames: string[];
};

type CacheMode = (typeof supportedCacheModes)[number];
type EgressMode = (typeof supportedEgressModes)[number];
type RunState = "running" | "succeeded" | "failed" | "stopped";

type RunRecord = {
  id: string;
  executionId?: string;
  workerId: string;
  state: RunState;
  cacheMode: CacheMode;
  egress: EgressMode;
  createdAt: string;
  startedAt: string;
  expiresAt?: string;
  completedAt?: string;
  durationMs?: number;
  metadata?: Record<string, string>;
  result?: RunResult;
  error?: RunError;
  logs: RunLog[];
};

type RunResult = {
  status: number;
  statusText: string;
  headers: Record<string, string>;
  body?: string;
};

type RunError = {
  message: string;
};

type RunLog = {
  level: "info" | "warn" | "error";
  message: string;
  time: string;
  eventCount?: number;
};

type RunCoordinationState = {
  executions: Record<string, RunExecutionLease>;
  completionResults?: Record<string, RunCompletionResult>;
  pendingIndexCleanup?: RunIndexCleanup;
  generation: number;
  terminal: boolean;
  releaseWhenIdle: boolean;
  cleanupWhenIdle?: boolean;
  record?: RunRecord;
  deletedAt?: number;
  deletionPending?: boolean;
  deletionConditional?: boolean;
  deletionRunId?: string;
};

type RunExecutionLease = {
  expiresAt: number | null;
  generation: number;
  record: RunRecord;
};

type RunCompletionResult = {
  indexGeneration: number;
  released: boolean;
};

type RunIndexCleanup = {
  generation: number;
  runId: string;
  tombstone: boolean;
};

type RunIndexEntry = {
  generation: number;
  record: RunRecord;
};

type RunIndexDeletion = {
  runId: string;
  generation: number;
  deletedAt: number;
  pending: boolean;
  suppressStored: boolean;
};

export class DynamicWorkerRunCoordinator extends DurableObject<Env> {
  override async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);

    if (request.method === "POST" && url.pathname === "/index/set") {
      await this.ensureRunIndexMarker();
      const body = (await request.json()) as { generation?: unknown; record?: unknown };
      if (
        typeof body.generation !== "number" ||
        !Number.isSafeInteger(body.generation) ||
        body.generation < 1 ||
        !coordinationRecord(body.record)
      ) {
        return json({ error: "invalid run index set request" }, 400);
      }
      const key = runIndexStorageKey(body.record.id);
      const deletionKey = runIndexDeletionStorageKey(body.record.id);
      const deletion = await this.ctx.storage.get<RunIndexDeletion>(deletionKey);
      if (deletion !== undefined && body.generation <= deletion.generation) {
        return json({ ok: true });
      }
      if (deletion !== undefined) await this.ctx.storage.delete(deletionKey);
      const current = await this.ctx.storage.get<RunIndexEntry>(key);
      if (current === undefined || body.generation >= current.generation) {
        await this.ctx.storage.put(key, {
          generation: body.generation,
          record: body.record,
        } satisfies RunIndexEntry);
      }
      return json({ ok: true });
    }

    if (request.method === "POST" && url.pathname === "/index/delete") {
      await this.ensureRunIndexMarker();
      const body = (await request.json()) as {
        runId?: unknown;
        generation?: unknown;
        pending?: unknown;
        tombstone?: unknown;
      };
      if (
        typeof body.runId !== "string" ||
        typeof body.generation !== "number" ||
        !Number.isSafeInteger(body.generation) ||
        body.generation < 1 ||
        (body.pending !== undefined && typeof body.pending !== "boolean") ||
        (body.tombstone !== undefined && typeof body.tombstone !== "boolean")
      ) {
        return json({ error: "invalid run index delete request" }, 400);
      }
      const key = runIndexStorageKey(body.runId);
      const current = await this.ctx.storage.get<RunIndexEntry>(key);
      if (current !== undefined && body.generation < current.generation) {
        return json({ ok: true });
      }
      if (current !== undefined) {
        await this.ctx.storage.delete(key);
      }
      const deletionKey = runIndexDeletionStorageKey(body.runId);
      const existingDeletion = await this.ctx.storage.get<RunIndexDeletion>(deletionKey);
      if (
        existingDeletion === undefined ||
        body.generation > existingDeletion.generation ||
        (body.generation === existingDeletion.generation &&
          ((body.tombstone === true && !existingDeletion.suppressStored) ||
            (body.pending === false && existingDeletion.pending)))
      ) {
        const deletion: RunIndexDeletion = {
          runId: body.runId,
          generation: body.generation,
          deletedAt: Date.now(),
          pending: body.pending === true,
          suppressStored: body.tombstone === true,
        };
        await this.ctx.storage.put(deletionKey, deletion);
        if (!deletion.pending) {
          const deletionAlarm = deletion.deletedAt + deletionTombstoneTTLms;
          const scheduledAlarm = await this.ctx.storage.getAlarm();
          if (scheduledAlarm === null || deletionAlarm < scheduledAlarm) {
            await this.ctx.storage.setAlarm(deletionAlarm);
          }
        }
      }
      return json({ ok: true });
    }

    if (request.method === "GET" && url.pathname === "/index/list") {
      await this.ensureRunIndexMarker();
      const [stored, storedDeletions] = await Promise.all([
        this.ctx.storage.list<RunIndexEntry>({ prefix: runIndexStoragePrefix }),
        this.ctx.storage.list<RunIndexDeletion>({ prefix: runIndexDeletionStoragePrefix }),
      ]);
      const records = [...stored.entries()];
      const deletions = [...storedDeletions.entries()];
      await Promise.all([
        ...records
          .filter(([, entry]) => entry.record.state !== "running" || runRecordExpired(entry.record))
          .map(([key]) => this.ctx.storage.delete(key)),
        ...deletions
          .filter(
            ([, deletion]) =>
              !deletion.pending && deletion.deletedAt + deletionTombstoneTTLms <= Date.now(),
          )
          .map(([key]) => this.ctx.storage.delete(key)),
      ]);
      return json({
        runs: records
          .map(([, entry]) => entry.record)
          .filter((record) => record.state === "running" && !runRecordExpired(record)),
        deletedRunIds: deletions
          .map(([, deletion]) => deletion)
          .filter(
            (deletion) =>
              deletion.suppressStored &&
              (deletion.pending || deletion.deletedAt + deletionTombstoneTTLms > Date.now()),
          )
          .map((deletion) => deletion.runId),
      });
    }

    if (request.method === "POST" && url.pathname === "/log") {
      const body = (await request.json()) as {
        executionId?: unknown;
        log?: unknown;
      };
      if (typeof body.executionId !== "string" || !tailLog(body.log)) {
        return json({ error: "invalid run coordination log request" }, 400);
      }
      await this.persistTailLog(body.executionId, body.log);
      return json({ ok: true });
    }

    if (request.method === "POST" && url.pathname === "/start") {
      const body = (await request.json()) as {
        executionId?: unknown;
        expiresAt?: unknown;
        legacyConcurrentReuse?: unknown;
        legacyReusableID?: unknown;
        record?: unknown;
      };
      if (
        typeof body.executionId !== "string" ||
        (body.expiresAt !== null && typeof body.expiresAt !== "number") ||
        (body.legacyConcurrentReuse !== undefined &&
          typeof body.legacyConcurrentReuse !== "boolean") ||
        typeof body.legacyReusableID !== "boolean" ||
        !coordinationRecord(body.record)
      ) {
        return json({ error: "invalid run coordination start request" }, 400);
      }
      const current = await this.coordinationState();
      if (current.deletionPending) {
        return json({ error: "run deletion is pending" }, 409);
      }
      const activeExecutions = Object.keys(current.executions).length;
      if (!body.legacyReusableID && (activeExecutions > 0 || current.terminal)) {
        return json({ error: "run id already exists" }, 409);
      }
      if (body.legacyReusableID && body.legacyConcurrentReuse === false && activeExecutions > 0) {
        return json({ error: "run id already exists" }, 409);
      }
      if (
        body.legacyReusableID &&
        body.legacyConcurrentReuse !== false &&
        activeExecutions >= maxConcurrentLegacyExecutions
      ) {
        return json({ error: "run concurrency limit reached" }, 429);
      }
      current.generation += 1;
      current.executions[body.executionId] = {
        expiresAt: body.expiresAt,
        generation: current.generation,
        record: body.record,
      };
      current.terminal = false;
      delete current.deletedAt;
      delete current.deletionPending;
      delete current.deletionRunId;
      current.record = body.record;
      await this.ctx.storage.put("state", current);
      await this.scheduleAlarm(current);
      return json({ ok: true, generation: current.generation });
    }

    if (request.method === "POST" && url.pathname === "/complete") {
      const body = (await request.json()) as {
        cleanupPending?: unknown;
        executionId?: unknown;
        release?: unknown;
        record?: unknown;
      };
      if (
        (body.cleanupPending !== undefined && typeof body.cleanupPending !== "boolean") ||
        typeof body.executionId !== "string" ||
        typeof body.release !== "boolean" ||
        !coordinationRecord(body.record)
      ) {
        return json({ error: "invalid run coordination completion request" }, 400);
      }
      const current = await this.coordinationState();
      if (!(body.executionId in current.executions)) {
        const completed = current.completionResults?.[body.executionId];
        if (completed !== undefined) {
          return json({
            ok: true,
            finalized: true,
            indexGeneration: completed.indexGeneration,
            released: completed.released,
          });
        }
        if (Object.keys(current.executions).length > 0 && current.record !== undefined) {
          return json({
            ok: true,
            finalized: false,
            activeRecord: current.record,
            indexGeneration: current.generation,
          });
        }
        return json({ ok: true, finalized: false, stale: true });
      }
      mergeRunLogs(body.record, current.executions[body.executionId]?.record);
      mergeRunLogs(body.record, current.record);
      delete current.executions[body.executionId];
      if (body.cleanupPending === true) {
        current.cleanupWhenIdle = true;
        current.releaseWhenIdle = true;
      } else if (body.release) {
        current.releaseWhenIdle = true;
      }
      if (Object.keys(current.executions).length > 0) {
        let activeRecord: RunRecord | undefined;
        if (current.record?.executionId === body.executionId) {
          activeRecord = promoteActiveExecution(current);
        }
        await this.ctx.storage.put("state", current);
        await this.scheduleAlarm(current);
        return json({
          ok: true,
          finalized: false,
          ...(activeRecord === undefined
            ? {}
            : { activeRecord, indexGeneration: current.generation }),
        });
      }
      current.record = body.record;
      if (current.cleanupWhenIdle === true) {
        current.terminal = false;
        current.releaseWhenIdle = false;
        current.cleanupWhenIdle = false;
        current.deletedAt = Date.now();
        current.deletionPending = true;
        current.deletionConditional = true;
        current.deletionRunId = body.record.id;
        rememberCompletion(current, body.executionId, {
          indexGeneration: current.generation,
          released: true,
        });
        await this.ctx.storage.put("state", current);
        await this.scheduleAlarm(current);
        await this.finishPendingDeletion(current);
        return json({
          ok: true,
          finalized: true,
          released: true,
          indexGeneration: current.generation,
        });
      }
      if (current.releaseWhenIdle) {
        current.terminal = false;
        current.releaseWhenIdle = false;
        delete current.record;
        current.deletedAt = Date.now();
        current.deletionPending = false;
        rememberCompletion(current, body.executionId, {
          indexGeneration: current.generation,
          released: true,
        });
        current.pendingIndexCleanup = {
          generation: current.generation,
          runId: body.record.id,
          tombstone: true,
        };
        await this.ctx.storage.put("state", current);
        await this.scheduleAlarm(current);
        await this.finishPendingIndexCleanup();
        return json({
          ok: true,
          finalized: true,
          released: true,
          indexGeneration: current.generation,
        });
      }
      current.terminal = true;
      rememberCompletion(current, body.executionId, {
        indexGeneration: current.generation,
        released: false,
      });
      current.pendingIndexCleanup = {
        generation: current.generation,
        runId: body.record.id,
        tombstone: false,
      };
      await this.ctx.storage.put("state", current);
      await this.scheduleAlarm(current);
      await this.finishPendingIndexCleanup();
      return json({ ok: true, finalized: true, indexGeneration: current.generation });
    }

    if (request.method === "POST" && url.pathname === "/renew") {
      const body = (await request.json()) as {
        executionId?: unknown;
        expiresAt?: unknown;
      };
      if (typeof body.executionId !== "string" || typeof body.expiresAt !== "number") {
        return json({ error: "invalid run coordination renewal request" }, 400);
      }
      const current = await this.coordinationState();
      const lease = current.executions[body.executionId];
      if (lease === undefined) {
        return json({ ok: true, stale: true });
      }
      lease.expiresAt = body.expiresAt;
      lease.record.expiresAt = new Date(body.expiresAt).toISOString();
      const indexed = current.record?.executionId === body.executionId;
      if (current.record?.executionId === body.executionId) {
        current.record.expiresAt = new Date(body.expiresAt).toISOString();
      }
      await this.ctx.storage.put("state", current);
      await this.scheduleAlarm(current);
      return json({
        ok: true,
        indexed,
        indexGeneration: lease.generation,
      });
    }

    if (request.method === "POST" && url.pathname === "/delete") {
      const body = (await request.json()) as {
        acknowledgedComplete?: unknown;
        legacyRecord?: unknown;
        runId?: unknown;
      };
      if (
        typeof body.acknowledgedComplete !== "boolean" ||
        typeof body.runId !== "string" ||
        !cleanRunId(body.runId) ||
        (body.legacyRecord !== undefined && !coordinationRecord(body.legacyRecord))
      ) {
        return json({ error: "invalid run coordination delete request" }, 400);
      }
      const current = await this.coordinationState();
      if (current.deletionPending) {
        return json({ error: "run deletion is pending" }, 409);
      }
      const active = Object.keys(current.executions).length;
      if (active > 0) {
        return json({ error: "active runs cannot be stopped; wait for completion" }, 409);
      }
      const known = active > 0 || current.terminal || body.legacyRecord !== undefined;
      const record = current.record ?? body.legacyRecord;
      current.generation += 1;
      current.executions = {};
      current.terminal = false;
      current.releaseWhenIdle = false;
      current.cleanupWhenIdle = false;
      if (record === undefined) {
        delete current.record;
      } else {
        current.record = record;
      }
      current.deletedAt = Date.now();
      current.deletionPending = true;
      current.deletionConditional = false;
      current.deletionRunId = body.runId;
      await this.ctx.storage.put("state", current);
      await this.scheduleAlarm(current);
      if (!(await this.finishPendingDeletion(current))) {
        return json({ error: "run deletion cleanup is pending" }, 503);
      }
      return json({ ok: true, known, record, indexGeneration: current.generation });
    }

    if (request.method === "GET" && url.pathname === "/record") {
      const current = await this.coordinationState();
      if (current.deletedAt !== undefined) {
        return json({ known: false, deleted: true }, 410);
      }
      if (
        current.record === undefined ||
        (Object.keys(current.executions).length === 0 && !current.terminal)
      ) {
        await this.ctx.storage.deleteAlarm();
        await this.ctx.storage.deleteAll();
        return json({ known: false }, 404);
      }
      return json({ known: true, record: current.record });
    }

    return json({ error: "not found" }, 404);
  }

  override async alarm(): Promise<void> {
    const runIndex = await this.ctx.storage.get<boolean>(runIndexMarkerStorageKey);
    if (runIndex === true) {
      const indexDeletions = await this.ctx.storage.list<RunIndexDeletion>({
        prefix: runIndexDeletionStoragePrefix,
      });
      const now = Date.now();
      const remainingExpirations: number[] = [];
      await Promise.all(
        [...indexDeletions.entries()].map(async ([key, deletion]) => {
          if (deletion.pending) return;
          const expiresAt = deletion.deletedAt + deletionTombstoneTTLms;
          if (expiresAt <= now) {
            await this.ctx.storage.delete(key);
          } else {
            remainingExpirations.push(expiresAt);
          }
        }),
      );
      if (remainingExpirations.length === 0) {
        await this.ctx.storage.deleteAlarm();
      } else {
        await this.ctx.storage.setAlarm(Math.min(...remainingExpirations));
      }
      return;
    }

    let current = await this.coordinationState();
    if (current.pendingIndexCleanup && !(await this.finishPendingIndexCleanup())) {
      return;
    }
    if (current.pendingIndexCleanup) current = await this.coordinationState();
    if (current.deletedAt !== undefined) {
      if (current.deletionPending) {
        await this.finishPendingDeletion(current);
        return;
      }
      if (current.deletedAt + deletionTombstoneTTLms > Date.now()) {
        await this.scheduleAlarm(current);
        return;
      }
      await this.ctx.storage.deleteAlarm();
      await this.ctx.storage.deleteAll();
      return;
    }
    if (Object.keys(current.executions).length > 0) {
      await this.ctx.storage.put("state", current);
      await this.scheduleAlarm(current);
      if (current.record && this.env.RUN_COORDINATOR) {
        await coordinateRunIndexSet(
          runIndexCoordinator(this.env, current.record.id),
          current.record,
          current.generation,
        ).catch(() => undefined);
      }
      return;
    }
    if (current.terminal) {
      await this.ctx.storage.deleteAlarm();
      return;
    }
    const record = current.record;
    if (record) await deletePrecedingRunMetadata(this.env, record);
    current.releaseWhenIdle = false;
    delete current.record;
    current.deletedAt = Date.now();
    current.deletionPending = false;
    await this.ctx.storage.put("state", current);
    await this.scheduleAlarm(current);
    if (record && this.env.RUN_COORDINATOR) {
      await coordinateRunIndexDelete(
        runIndexCoordinator(this.env, record.id),
        record.id,
        current.generation,
        true,
      ).catch(() => undefined);
    }
  }

  private async coordinationState(): Promise<RunCoordinationState> {
    const load = () => this.coordinationStateSerialized();
    const blockConcurrencyWhile = this.ctx.blockConcurrencyWhile?.bind(this.ctx);
    return blockConcurrencyWhile ? blockConcurrencyWhile(load) : load();
  }

  private async persistTailLog(executionId: string, log: RunLog): Promise<void> {
    const persist = async (): Promise<void> => {
      const current = await this.coordinationStateSerialized();
      const coordinatedRecords = new Set<RunRecord>();
      const lease = current.executions[executionId];
      if (lease !== undefined) coordinatedRecords.add(lease.record);
      if (current.record?.executionId === executionId) coordinatedRecords.add(current.record);
      for (const record of coordinatedRecords) {
        appendRunLog(record, log, coordinationMaxLogEntries);
      }
      if (coordinatedRecords.size > 0) {
        await this.ctx.storage.put("state", current);
      }
      const runId = current.record?.id ?? lease?.record.id;
      if (!this.env.RUNS || runId === undefined) return;
      const key = runMetadataKey(runId);
      const raw = await this.env.RUNS.get(key);
      if (!raw) return;
      const terminal = JSON.parse(raw) as RunRecord;
      if (terminal.executionId !== executionId) return;
      appendRunLog(terminal, log);
      await retryKVWrite(() => this.env.RUNS!.put(key, JSON.stringify(terminal)));
    };
    const blockConcurrencyWhile = this.ctx.blockConcurrencyWhile?.bind(this.ctx);
    await (blockConcurrencyWhile ? blockConcurrencyWhile(persist) : persist());
  }

  private async coordinationStateSerialized(): Promise<RunCoordinationState> {
    const stored = await this.ctx.storage.get<RunCoordinationState>("state");
    const current = stored ?? {
      executions: {},
      generation: Date.now() * 1000,
      terminal: false,
      releaseWhenIdle: false,
    };
    const now = Date.now();
    const expiredExecutionIds = new Set<string>();
    for (const [executionId, lease] of Object.entries(current.executions)) {
      if (lease.expiresAt !== null && lease.expiresAt <= now) {
        delete current.executions[executionId];
        expiredExecutionIds.add(executionId);
      }
    }
    let recoveredTerminal: RunRecord | undefined;
    if (expiredExecutionIds.size > 0 && current.record !== undefined) {
      const terminal = await storedTerminalRun(this.env, current.record.id);
      if (
        terminal?.executionId !== undefined &&
        expiredExecutionIds.has(terminal.executionId) &&
        terminal.state !== "running"
      ) {
        recoveredTerminal = runCoordinationRecord(terminal);
      }
    }
    if (
      recoveredTerminal !== undefined &&
      Object.keys(current.executions).length === 0 &&
      !current.releaseWhenIdle
    ) {
      current.record = recoveredTerminal;
      current.terminal = true;
    }
    let promoted: RunRecord | undefined;
    if (
      Object.keys(current.executions).length > 0 &&
      current.record?.executionId !== undefined &&
      !(current.record.executionId in current.executions)
    ) {
      promoted = promoteActiveExecution(current);
    }
    if (expiredExecutionIds.size > 0 || promoted) {
      await this.ctx.storage.put("state", current);
      await this.scheduleAlarm(current);
      if (this.env.RUN_COORDINATOR) {
        if (promoted) {
          await coordinateRunIndexSet(
            runIndexCoordinator(this.env, promoted.id),
            promoted,
            current.generation,
          ).catch(() => undefined);
        }
      }
    }
    return current;
  }

  private async finishPendingDeletion(current: RunCoordinationState): Promise<boolean> {
    const runId = current.deletionRunId ?? current.record?.id;
    if (runId === undefined) {
      await this.ctx.storage.setAlarm(Date.now() + deletionRetryDelayMs);
      return false;
    }
    const cleanup = async (): Promise<boolean> => {
      try {
        if (this.env.RUN_COORDINATOR) {
          await coordinateRunIndexDelete(
            runIndexCoordinator(this.env, runId),
            runId,
            current.generation,
            true,
            true,
          );
        }
        const tombstone =
          current.deletionConditional === true && current.record !== undefined
            ? await deletePrecedingRunMetadata(this.env, current.record)
            : await deleteRun(this.env, runId).then(() => true);
        if (this.env.RUN_COORDINATOR) {
          await coordinateRunIndexDelete(
            runIndexCoordinator(this.env, runId),
            runId,
            current.generation,
            tombstone,
            false,
          );
        }
        if (tombstone) {
          current.deletedAt = Date.now();
        } else {
          delete current.deletedAt;
        }
        current.deletionPending = false;
        delete current.deletionConditional;
        delete current.record;
        delete current.deletionRunId;
        if (tombstone) {
          await this.ctx.storage.put("state", current);
          await this.scheduleAlarm(current);
        } else {
          await this.ctx.storage.deleteAlarm();
          await this.ctx.storage.deleteAll();
        }
        return true;
      } catch {
        await this.ctx.storage.setAlarm(Date.now() + deletionRetryDelayMs);
        return false;
      }
    };
    const blockConcurrencyWhile = this.ctx.blockConcurrencyWhile?.bind(this.ctx);
    return blockConcurrencyWhile ? blockConcurrencyWhile(cleanup) : cleanup();
  }

  private async finishPendingIndexCleanup(): Promise<boolean> {
    const cleanup = async (): Promise<boolean> => {
      const current = await this.ctx.storage.get<RunCoordinationState>("state");
      const pending = current?.pendingIndexCleanup;
      if (current === undefined || pending === undefined) return true;
      try {
        if (this.env.RUN_COORDINATOR) {
          await coordinateRunIndexDelete(
            runIndexCoordinator(this.env, pending.runId),
            pending.runId,
            pending.generation,
            pending.tombstone,
          );
        }
        delete current.pendingIndexCleanup;
        await this.ctx.storage.put("state", current);
        await this.scheduleAlarm(current);
        return true;
      } catch {
        await this.ctx.storage.setAlarm(Date.now() + deletionRetryDelayMs);
        return false;
      }
    };
    const blockConcurrencyWhile = this.ctx.blockConcurrencyWhile?.bind(this.ctx);
    return blockConcurrencyWhile ? blockConcurrencyWhile(cleanup) : cleanup();
  }

  private async ensureRunIndexMarker(): Promise<void> {
    const marked = await this.ctx.storage.get<boolean>(runIndexMarkerStorageKey);
    if (marked !== true) await this.ctx.storage.put(runIndexMarkerStorageKey, true);
  }

  private async scheduleAlarm(current: RunCoordinationState): Promise<void> {
    if (current.pendingIndexCleanup) {
      await this.ctx.storage.setAlarm(Date.now() + deletionRetryDelayMs);
      return;
    }
    if (current.deletedAt !== undefined) {
      if (current.deletionPending) {
        await this.ctx.storage.setAlarm(Date.now() + deletionRetryDelayMs);
        return;
      }
      await this.ctx.storage.setAlarm(current.deletedAt + deletionTombstoneTTLms);
      return;
    }
    const expirations = Object.values(current.executions)
      .map((lease) => lease.expiresAt)
      .filter((expiresAt): expiresAt is number => expiresAt !== null);
    if (expirations.length === 0) {
      await this.ctx.storage.deleteAlarm();
      return;
    }
    await this.ctx.storage.setAlarm(Math.min(...expirations));
  }
}

function promoteActiveExecution(current: RunCoordinationState): RunRecord | undefined {
  const active = Object.values(current.executions).toSorted(
    (left, right) => right.generation - left.generation,
  )[0];
  if (!active) return undefined;
  current.generation += 1;
  active.generation = current.generation;
  current.record = active.record;
  return current.record;
}

function rememberCompletion(
  current: RunCoordinationState,
  executionId: string,
  result: RunCompletionResult,
): void {
  const entries = Object.entries(current.completionResults ?? {}).filter(
    ([storedExecutionId]) => storedExecutionId !== executionId,
  );
  entries.push([executionId, result]);
  current.completionResults = Object.fromEntries(entries.slice(-maxRememberedCompletions));
}

export default {
  async fetch(request: Request, env: Env, ctx: DynamicWorkerContext): Promise<Response> {
    const url = new URL(request.url);

    if (url.pathname === "/health") {
      return json({ ok: true, runner: runnerName });
    }

    const auth = authorize(request, env);
    if (auth) return auth;

    if (url.pathname === "/v1/readiness" && request.method === "GET") {
      return readiness(env);
    }

    if (url.pathname === "/v1/runs") {
      if (request.method === "GET") return listRuns(env);
      if (request.method === "POST") return createRun(request, env, ctx, url);
      return json({ error: "method not allowed" }, 405);
    }

    const match = url.pathname.match(/^\/v1\/runs\/([^/]+)(?:\/([^/]+))?$/);
    if (!match) return json({ error: "not found" }, 404);

    let runId = "";
    try {
      runId = decodeURIComponent(match[1] ?? "");
    } catch {
      return json({ error: "run id is invalid" }, 400);
    }
    const action = match[2] ?? "";
    if (!cleanRunId(runId)) return json({ error: "run id is invalid" }, 400);

    if (request.method === "GET" && action === "") return getRun(env, runId);
    if (request.method === "GET" && action === "logs") return getRunLogs(env, runId);
    if (request.method === "DELETE" && action === "") {
      return stopRun(env, runId, url.searchParams.get("acknowledgedComplete") === "true");
    }

    return json({ error: "not found" }, 404);
  },
};

function readiness(env: Env): Response {
  const missing = [
    ...(env.LOADER ? [] : ["LOADER"]),
    ...(env.RUN_COORDINATOR ? [] : ["RUN_COORDINATOR"]),
  ];
  if (missing.length > 0) {
    return json(
      {
        ok: false,
        runner: runnerName,
        error: `missing required binding${missing.length === 1 ? "" : "s"}: ${missing.join(", ")}`,
        missing,
      },
      503,
    );
  }

  return json({
    ok: true,
    runner: runnerName,
    loader: true,
    loaderBinding: true,
    coordinatorBinding: true,
    durableRunMetadata: Boolean(env.RUNS),
    compatibilityDate: defaultCompatibilityDate,
    egress: "blocked",
    defaultEgress: "blocked",
    cacheModes: supportedCacheModes,
    tokenSource: tokenSource(env),
  });
}

async function createRun(
  request: Request,
  env: Env,
  ctx: DynamicWorkerContext,
  url: URL,
): Promise<Response> {
  if (!env.LOADER || !env.RUN_COORDINATOR) return readiness(env);
  if (!isJsonRequest(request)) return json({ error: "content-type must be application/json" }, 415);

  const body = await readObject(request);
  if (body instanceof Response) return body;

  const parsed = parseRunRequest(body, url);
  if (parsed instanceof Response) return parsed;

  const startedAt = Date.now();
  const executionId = crypto.randomUUID();
  const workerCodeResult = workerCodeForRun(parsed, ctx, executionId);
  if (workerCodeResult instanceof Response) return workerCodeResult;
  const reservationExpiresAt = runReservationExpiresAt(parsed, startedAt);
  const log: RunLog = {
    level: "info",
    message: "run started",
    time: new Date(startedAt).toISOString(),
  };
  const record: RunRecord = {
    id: parsed.id,
    executionId,
    workerId: parsed.workerId,
    state: "running",
    cacheMode: parsed.cacheMode,
    egress: parsed.egress,
    createdAt: new Date(startedAt).toISOString(),
    startedAt: new Date(startedAt).toISOString(),
    logs: [log],
  };
  record.expiresAt = new Date(reservationExpiresAt).toISOString();
  if (parsed.metadata !== undefined) record.metadata = parsed.metadata;
  const reservation = await coordinateRunStart(
    env,
    parsed.id,
    executionId,
    reservationExpiresAt,
    parsed.legacyReusableID,
    parsed.legacyConcurrentReuse,
    record,
  );
  if (reservation instanceof Response) return reservation;
  const indexGeneration = reservation.generation;

  executionStore.set(executionId, record);
  let releaseCoordination = true;
  const indexCoordinator = runIndexCoordinator(env, parsed.id);
  const indexSet = bestEffortRunIndex(
    ctx,
    coordinateRunIndexSet(indexCoordinator, record, indexGeneration),
  );
  const stopHeartbeat = startRunHeartbeat(
    env,
    ctx,
    parsed.id,
    executionId,
    record,
    indexCoordinator,
    indexSet,
  );

  let executionResponse: Response | undefined;
  let lifecycleUncertain = false;
  let cleanupPending = false;
  try {
    if (parsed.legacyReusableID) {
      try {
        await deletePrecedingRunMetadata(env, record);
      } catch {
        appendLog(executionId, {
          level: "warn",
          message: "stale run metadata cleanup deferred until finalization",
          time: new Date().toISOString(),
        });
      }
    }
    let responseStatus = 200;
    try {
      const worker =
        parsed.cacheMode === "one-shot"
          ? env.LOADER.load(workerCodeResult)
          : env.LOADER.get(parsed.workerId, async () => workerCodeResult);
      const entrypoint = worker.getEntrypoint(undefined, entrypointOptions(parsed.limits));
      const dynamicResponse = await entrypoint.fetch(requestForDynamicWorker(parsed));
      const result = await responseResult(
        dynamicResponse,
        responseBodyTimeoutMs(parsed.timeoutMs, startedAt),
      );
      const completedAt = Date.now();
      record.state = result.status < 400 ? "succeeded" : "failed";
      record.completedAt = new Date(completedAt).toISOString();
      record.durationMs = completedAt - startedAt;
      record.result = result;
      appendLog(executionId, {
        level: "info",
        message: `run completed with HTTP ${result.status}`,
        time: record.completedAt,
      });
    } catch (error) {
      const completedAt = Date.now();
      record.state = "failed";
      record.completedAt = new Date(completedAt).toISOString();
      record.durationMs = completedAt - startedAt;
      record.error = { message: redactSecrets(errorMessage(error), env) };
      appendLog(executionId, {
        level: "error",
        message: "run failed",
        time: record.completedAt,
      });
      responseStatus = 502;
    }
    delete record.expiresAt;
    const retain = retainRunMetadata(parsed, record);
    releaseCoordination = !retain;
    let finalized: Awaited<ReturnType<typeof finalizeRun>>;
    try {
      finalized = await finalizeRun(env, record, responseStatus, retain);
    } catch (error) {
      if (!retain) {
        releaseCoordination = false;
        cleanupPending = true;
      }
      throw error;
    }
    releaseCoordination = !retain;
    executionResponse = finalized.response;
  } finally {
    stopHeartbeat();
    try {
      try {
        const completion = await coordinateRunCompleteWithRetry(
          env,
          parsed.id,
          executionId,
          record,
          releaseCoordination,
          cleanupPending,
        );
        if (completion.finalized) {
          if (completion.released && !releaseCoordination && !cleanupPending) {
            await deleteRun(env, parsed.id);
          }
        } else if (completion.activeRecord) {
          bestEffortRunIndex(
            ctx,
            coordinateRunIndexSet(
              indexCoordinator,
              completion.activeRecord,
              completion.indexGeneration,
            ),
          );
        }
      } catch {
        lifecycleUncertain = true;
      }
    } finally {
      executionStore.delete(executionId);
    }
  }
  if (!executionResponse) throw new Error("dynamic worker execution response is missing");
  return lifecycleUncertain
    ? await uncertainLifecycleResponse(executionResponse)
    : executionResponse;
}

async function uncertainLifecycleResponse(response: Response): Promise<Response> {
  const body = (await response.clone().json()) as Record<string, unknown>;
  const headers = new Headers(response.headers);
  headers.set("X-Crabbox-Lifecycle-Uncertain", "true");
  return Response.json(
    {
      ...body,
      lifecycleUncertain: true,
      lifecycleMessage: "execution completed but lifecycle reconciliation is pending",
    },
    { status: response.status, headers },
  );
}

async function finalizeRun(
  env: Env,
  record: RunRecord,
  responseStatus: number,
  retain: boolean,
): Promise<{ response: Response; retained: boolean }> {
  const response = runResponse(record);
  if (retain) {
    await persistRun(env, record);
  } else {
    await deletePrecedingRunMetadata(env, record);
  }
  return { response: json(response, responseStatus), retained: retain };
}

function retainRunMetadata(parsed: ParsedRunRequest, record: RunRecord): boolean {
  return parsed.retainMetadata || (parsed.retainOnFailure && record.state === "failed");
}

async function listRuns(env: Env): Promise<Response> {
  try {
    const records = await storedRuns(env);
    return json({
      runs: records.map((record) => runSummary(record)),
    });
  } catch {
    return json({ error: "run index is temporarily unavailable" }, 503);
  }
}

async function getRun(env: Env, runId: string): Promise<Response> {
  const record = await storedRun(env, runId);
  if (!record) return json({ error: "not found" }, 404);
  return json(runResponse(record));
}

async function getRunLogs(env: Env, runId: string): Promise<Response> {
  const record = await storedRun(env, runId);
  if (!record) return json({ error: "not found" }, 404);
  return json({ id: record.id, logs: record.logs });
}

async function stopRun(env: Env, runId: string, acknowledgedComplete = false): Promise<Response> {
  if (!env.RUN_COORDINATOR) return readiness(env);
  const legacyRecord = await storedTerminalRun(env, runId);
  const coordination = await coordinateRunDelete(env, runId, acknowledgedComplete, legacyRecord);
  if (coordination instanceof Response) return coordination;
  const record = coordination.record;
  if (!record) {
    if (!acknowledgedComplete && !coordination.known) {
      return json({ error: "not found" }, 404);
    }
    return json({ id: runId, status: "stopped", exitCode: 1 });
  }

  const stoppedAt = new Date().toISOString();
  record.state = "stopped";
  record.completedAt = stoppedAt;
  record.durationMs = Math.max(0, Date.parse(stoppedAt) - Date.parse(record.startedAt));
  record.logs.push({
    level: "info",
    message: "run metadata stopped",
    time: stoppedAt,
  });
  return json(runResponse(record));
}

function workerCodeForRun(
  parsed: ParsedRunRequest,
  ctx: DynamicWorkerContext,
  executionId: string,
): WorkerCode | Response {
  const code: WorkerCode = {
    compatibilityDate: parsed.compatibilityDate,
    mainModule: parsed.mainModule,
    modules: parsed.modules,
    globalOutbound: null,
  };

  if (parsed.compatibilityFlags.length > 0) code.compatibilityFlags = parsed.compatibilityFlags;
  if (parsed.env !== undefined) code.env = parsed.env;
  if (parsed.limits !== undefined) code.limits = parsed.limits;

  if (parsed.egress === "intercept") {
    const gateway = ctx.exports?.HttpGateway;
    const tailer = ctx.exports?.LogTailer;
    if (!gateway || !tailer) {
      return json({ error: "intercept egress requires HttpGateway and LogTailer exports" }, 503);
    }
    code.globalOutbound = gateway({
      props: {
        executionId,
        allowHostnames: parsed.gateway.allowHostnames,
      },
    });
    code.tails = [
      tailer({
        props: {
          runId: parsed.id,
          executionId,
        },
      }),
    ];
  }

  return code;
}

function requestForDynamicWorker(parsed: ParsedRunRequest): Request {
  const headers = new Headers(parsed.request.headers);
  const init: RequestInit = {
    method: parsed.request.method,
    headers,
  };
  if (parsed.request.body !== undefined && !bodylessMethod(parsed.request.method)) {
    init.body = parsed.request.body;
  }
  if (parsed.timeoutMs !== undefined) {
    init.signal = AbortSignal.timeout(parsed.timeoutMs);
  }
  return new Request(parsed.request.url, init);
}

function entrypointOptions(limits: WorkerLimits | undefined): EntrypointOptions | undefined {
  return limits === undefined ? undefined : { limits };
}

async function responseResult(response: Response, bodyTimeoutMs: number): Promise<RunResult> {
  const headers: Record<string, string> = {};
  for (const [key, value] of response.headers) {
    headers[key] = value;
  }
  return {
    status: response.status,
    statusText: response.statusText,
    headers,
    body: await readBoundedResponseBody(response, bodyTimeoutMs),
  };
}

function responseBodyTimeoutMs(timeoutMs: number | undefined, startedAt: number): number {
  if (timeoutMs === undefined) return dynamicResponseBodyReadTimeoutMs;
  return Math.max(1, timeoutMs - (Date.now() - startedAt));
}

async function readBoundedResponseBody(response: Response, timeoutMs: number): Promise<string> {
  if (!response.body) return "";
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let body = "";
  let bytes = 0;
  const deadline = Date.now() + timeoutMs;
  while (true) {
    const remainingMs = deadline - Date.now();
    if (remainingMs <= 0) {
      void reader.cancel("response body read timed out");
      throw new Error("dynamic worker response body read timed out");
    }
    // Stream reads are intentionally sequential so the byte cap is enforced before buffering.
    // oxlint-disable-next-line eslint/no-await-in-loop
    const result = await readResponseChunk(reader, remainingMs);
    if (result === undefined) {
      void reader.cancel("response body read timed out");
      throw new Error("dynamic worker response body read timed out");
    }
    if (result.done) return body + decoder.decode();
    const chunk = result.value;
    const remainingBytes = dynamicResponseBodyLimitBytes - bytes;
    if (chunk.byteLength > remainingBytes) {
      body += decoder.decode(chunk.subarray(0, remainingBytes), { stream: true });
      void reader.cancel("response body exceeded limit");
      return `${body}${decoder.decode()}\n[crabbox response body truncated]`;
    }
    bytes += chunk.byteLength;
    body += decoder.decode(chunk, { stream: true });
  }
}

async function readResponseChunk(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  timeoutMs: number,
): Promise<ReadableStreamReadResult<Uint8Array> | undefined> {
  let timeout: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      reader.read(),
      new Promise<undefined>((resolve) => {
        timeout = setTimeout(() => resolve(undefined), timeoutMs);
      }),
    ]);
  } finally {
    if (timeout !== undefined) clearTimeout(timeout);
  }
}

type ParsedRunRequest = {
  id: string;
  workerId: string;
  legacyConcurrentReuse: boolean;
  legacyReusableID: boolean;
  cacheMode: CacheMode;
  retainMetadata: boolean;
  retainOnFailure: boolean;
  egress: EgressMode;
  mainModule: string;
  modules: Record<string, WorkerModule>;
  compatibilityDate: string;
  compatibilityFlags: string[];
  env?: Record<string, unknown>;
  limits?: WorkerLimits;
  timeoutMs?: number;
  metadata?: Record<string, string>;
  gateway: {
    allowHostnames: string[];
  };
  request: {
    method: string;
    url: string;
    headers: Record<string, string>;
    body?: string;
  };
};

function parseRunRequest(body: Record<string, unknown>, url: URL): ParsedRunRequest | Response {
  const rawId = stringField(body, "id") ?? stringField(body, "runId") ?? makeRunId();
  const id = cleanRunId(rawId);
  if (!id || id === runIndexObjectPrefix) return json({ error: "id is invalid" }, 400);

  const cacheMode = parseCacheMode(stringField(body, "cacheMode") ?? "one-shot");
  if (!cacheMode) return json({ error: "cacheMode must be one-shot, stable, or explicit" }, 400);

  const rawWorkerId = stringField(body, "workerId") ?? id;
  const workerId = cleanRunId(rawWorkerId);
  if (!workerId) return json({ error: "workerId is invalid" }, 400);
  const retainMetadata = optionalBoolean(body, "retainMetadata");
  if (retainMetadata instanceof Response) return retainMetadata;
  const retainOnFailure = optionalBoolean(body, "retainOnFailure");
  if (retainOnFailure instanceof Response) return retainOnFailure;
  const legacyRetentionDefaults =
    body["retainMetadata"] === undefined && body["retainOnFailure"] === undefined;
  const legacyReusableID =
    body["workerId"] === undefined
      ? cacheMode !== "one-shot" || retainMetadata !== false
      : legacyRetentionDefaults;
  const legacyConcurrentReuse = legacyReusableID && cacheMode !== "one-shot";
  if (legacyReusableID && retainMetadata === false) {
    return json({ error: "retainMetadata=false requires workerId for reusable cache modes" }, 400);
  }

  const egress = parseEgressMode(
    url.searchParams.get("egress") ?? stringField(body, "egress") ?? "blocked",
  );
  if (!egress) return json({ error: "egress must be blocked or intercept" }, 400);
  if (egress === "intercept" && cacheMode !== "one-shot") {
    return json({ error: "intercept egress requires cacheMode one-shot" }, 400);
  }

  const moduleCompat = parseModuleCompat(body["module"]);
  if (moduleCompat instanceof Response) return moduleCompat;

  const modules = moduleCompat?.modules ?? parseModules(body["modules"]);
  if (modules instanceof Response) return modules;

  const mainModule = moduleCompat?.mainModule ?? stringField(body, "mainModule") ?? "index.js";
  if (!(mainModule in modules)) return json({ error: "mainModule must exist in modules" }, 400);

  const compatibilityDate = cleanCompatibilityDate(
    stringField(body, "compatibilityDate") ?? defaultCompatibilityDate,
  );
  if (!compatibilityDate) return json({ error: "compatibilityDate must be YYYY-MM-DD" }, 400);

  const compatibilityFlags = parseStringArray(body["compatibilityFlags"]);
  if (compatibilityFlags instanceof Response) return compatibilityFlags;
  if (
    Object.entries(modules).some(([name, module]) => pythonModule(name, module)) &&
    !compatibilityFlags.includes("python_workers")
  ) {
    compatibilityFlags.push("python_workers");
  }

  const limits = parseLimits(body["limits"]);
  if (limits instanceof Response) return limits;

  const timeoutMs = parsePositiveInteger(body["timeoutMs"]);
  if (timeoutMs instanceof Response) return timeoutMs;

  const env = parseOptionalRecord(body["env"]);
  if (env instanceof Response) return env;

  const metadata = parseMetadata(body["metadata"]);
  if (metadata instanceof Response) return metadata;

  const requestSpec = parseInvocationRequest(body["request"]);
  if (requestSpec instanceof Response) return requestSpec;

  const gateway = parseGateway(body["gateway"]);
  if (gateway instanceof Response) return gateway;

  const parsed: ParsedRunRequest = {
    id,
    workerId,
    legacyConcurrentReuse,
    legacyReusableID,
    cacheMode,
    retainMetadata: retainMetadata ?? true,
    retainOnFailure: retainOnFailure ?? false,
    egress,
    mainModule,
    modules,
    compatibilityDate,
    compatibilityFlags,
    gateway,
    request: requestSpec,
  };
  if (env !== undefined) parsed.env = env;
  if (limits !== undefined) parsed.limits = limits;
  if (timeoutMs !== undefined) parsed.timeoutMs = timeoutMs;
  if (metadata !== undefined) parsed.metadata = metadata;
  return parsed;
}

function parseModules(value: unknown): Record<string, WorkerModule> | Response {
  if (!isRecord(value)) return json({ error: "modules must be an object" }, 400);

  const modules: Record<string, WorkerModule> = {};
  for (const [name, moduleValue] of Object.entries(value)) {
    if (!cleanModuleName(name)) return json({ error: "module name is invalid" }, 400);
    if (typeof moduleValue === "string") {
      if (typescriptModuleName(name)) {
        return json({ error: "TypeScript module source must be transpiled to JavaScript" }, 400);
      }
      modules[name] = moduleValue;
      continue;
    }
    if (!isRecord(moduleValue)) return json({ error: "module value is invalid" }, 400);
    const parsed = parseTypedModule(moduleValue);
    if (!parsed) return json({ error: "module value is invalid" }, 400);
    modules[name] = parsed;
  }

  if (Object.keys(modules).length === 0) return json({ error: "modules must not be empty" }, 400);
  return modules;
}

function parseModuleCompat(
  value: unknown,
): { mainModule: string; modules: Record<string, WorkerModule> } | Response | undefined {
  if (value === undefined) return undefined;
  if (!isRecord(value)) return json({ error: "module must be an object" }, 400);

  const source = stringField(value, "source");
  if (source === undefined) return json({ error: "module.source must be a string" }, 400);
  const name = stringField(value, "name") ?? "index.js";
  if (!cleanModuleName(name)) return json({ error: "module.name is invalid" }, 400);

  const module = compatibilityModule(name, source);
  if (module instanceof Response) return module;
  return {
    mainModule: name,
    modules: {
      [name]: module,
    },
  };
}

function compatibilityModule(name: string, source: string): WorkerModule | Response {
  if (typescriptModuleName(name)) {
    return json({ error: "TypeScript module source must be transpiled to JavaScript" }, 400);
  }
  const lowerName = name.toLowerCase();
  if (lowerName.endsWith(".py")) return { py: source };
  if (lowerName.endsWith(".cjs")) return { cjs: source };
  return { js: source };
}

function parseTypedModule(value: Record<string, unknown>): WorkerModule | null {
  const keys = ["js", "cjs", "py", "text", "json"] as const;
  const present = keys.filter((key) => value[key] !== undefined);
  if (present.length !== 1) return null;
  const key = present[0];
  if (key === undefined) return null;
  if (key === "json") return { json: value[key] };
  const raw = value[key];
  return typeof raw === "string" ? { [key]: raw } : null;
}

function pythonModule(name: string, value: WorkerModule): boolean {
  return name.toLowerCase().endsWith(".py") || (isRecord(value) && typeof value["py"] === "string");
}

function parseLimits(value: unknown): WorkerLimits | Response | undefined {
  if (value === undefined) return undefined;
  if (!isRecord(value)) return json({ error: "limits must be an object" }, 400);

  const cpuMs = parsePositiveInteger(value["cpuMs"]);
  if (cpuMs instanceof Response)
    return json({ error: "limits.cpuMs must be a positive integer" }, 400);

  const subRequests = parsePositiveInteger(
    value["subRequests"] ?? value["subrequests"] ?? value["subrequestLimit"],
  );
  if (subRequests instanceof Response) {
    return json({ error: "limits.subRequests must be a positive integer" }, 400);
  }

  const limits: WorkerLimits = {};
  if (cpuMs !== undefined) limits.cpuMs = cpuMs;
  if (subRequests !== undefined) limits.subRequests = subRequests;
  return Object.keys(limits).length > 0 ? limits : undefined;
}

function parseInvocationRequest(value: unknown): ParsedRunRequest["request"] | Response {
  if (value === undefined) {
    return {
      method: "GET",
      url: "https://crabbox.invalid/",
      headers: {},
    };
  }
  if (!isRecord(value)) return json({ error: "request must be an object" }, 400);

  const method = (stringField(value, "method") ?? "GET").trim().toUpperCase();
  if (!/^[A-Z]{1,16}$/.test(method)) return json({ error: "request.method is invalid" }, 400);

  const requestUrl = stringField(value, "url") ?? "https://crabbox.invalid/";
  if (!validHttpUrl(requestUrl)) return json({ error: "request.url must be http or https" }, 400);

  const headers = parseHeaders(value["headers"]);
  if (headers instanceof Response) return headers;

  const parsed: ParsedRunRequest["request"] = {
    method,
    url: requestUrl,
    headers,
  };
  const rawBody = value["body"];
  if (rawBody !== undefined) {
    if (typeof rawBody !== "string") return json({ error: "request.body must be a string" }, 400);
    parsed.body = rawBody;
  }
  return parsed;
}

function parseHeaders(value: unknown): Record<string, string> | Response {
  if (value === undefined) return {};
  if (!isRecord(value)) return json({ error: "request.headers must be an object" }, 400);
  const headers: Record<string, string> = {};
  for (const [key, raw] of Object.entries(value)) {
    if (!cleanHeaderName(key) || typeof raw !== "string") {
      return json({ error: "request.headers must contain string values" }, 400);
    }
    headers[key] = raw;
  }
  return headers;
}

function parseGateway(value: unknown): ParsedRunRequest["gateway"] | Response {
  if (value === undefined) return { allowHostnames: [] };
  if (!isRecord(value)) return json({ error: "gateway must be an object" }, 400);
  const raw = value["allowHostnames"];
  if (raw === undefined) return { allowHostnames: [] };
  if (!Array.isArray(raw)) return json({ error: "gateway.allowHostnames must be an array" }, 400);

  const allowHostnames: string[] = [];
  for (const item of raw) {
    if (typeof item !== "string" || !cleanHostname(item)) {
      return json({ error: "gateway.allowHostnames contains an invalid hostname" }, 400);
    }
    allowHostnames.push(item.toLowerCase());
  }
  return { allowHostnames };
}

function parseOptionalRecord(value: unknown): Record<string, unknown> | Response | undefined {
  if (value === undefined) return undefined;
  if (!isRecord(value)) return json({ error: "env must be an object" }, 400);
  return value;
}

function parseMetadata(value: unknown): Record<string, string> | Response | undefined {
  if (value === undefined) return undefined;
  if (!isRecord(value)) return json({ error: "metadata must be an object" }, 400);
  const metadata: Record<string, string> = {};
  for (const [key, raw] of Object.entries(value)) {
    if (typeof raw !== "string") return json({ error: "metadata values must be strings" }, 400);
    const trimmed = key.trim();
    if (trimmed !== "") metadata[trimmed] = raw;
  }
  return metadata;
}

function parsePositiveInteger(value: unknown): number | Response | undefined {
  if (value === undefined) return undefined;
  return typeof value === "number" && Number.isInteger(value) && value > 0
    ? value
    : json({ error: "value must be a positive integer" }, 400);
}

function optionalBoolean(
  value: Record<string, unknown>,
  key: string,
): boolean | Response | undefined {
  const raw = value[key];
  if (raw === undefined) return undefined;
  return typeof raw === "boolean" ? raw : json({ error: `${key} must be a boolean` }, 400);
}

function parseStringArray(value: unknown): string[] | Response {
  if (value === undefined) return [];
  if (!Array.isArray(value)) return json({ error: "compatibilityFlags must be an array" }, 400);
  const flags: string[] = [];
  for (const item of value) {
    if (typeof item !== "string" || item.trim() === "") {
      return json({ error: "compatibilityFlags must contain strings" }, 400);
    }
    flags.push(item);
  }
  return flags;
}

async function readObject(request: Request): Promise<Record<string, unknown> | Response> {
  let value: unknown;
  try {
    value = await request.json();
  } catch {
    return json({ error: "invalid json" }, 400);
  }
  return isRecord(value) ? value : json({ error: "json body must be an object" }, 400);
}

function isJsonRequest(request: Request): boolean {
  return (request.headers.get("Content-Type") ?? "").toLowerCase().includes("application/json");
}

function authorize(request: Request, env: Env): Response | null {
  const expected = tokenValue(env);
  if (!expected) return json({ error: "runner token is not configured" }, 503);
  const header = request.headers.get("Authorization") ?? "";
  const actual = header.startsWith("Bearer ") ? header.slice("Bearer ".length) : "";
  if (!tokenEquals(actual, expected)) return json({ error: "unauthorized" }, 401);
  return null;
}

function tokenValue(env: Env): string | undefined {
  return env.CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN ?? env.CRABBOX_RUNNER_TOKEN;
}

function tokenSource(env: Env): string {
  return env.CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN
    ? "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN"
    : "CRABBOX_RUNNER_TOKEN";
}

function tokenEquals(actual: string, expected: string): boolean {
  const encoder = new TextEncoder();
  const actualBytes = encoder.encode(actual);
  const expectedBytes = encoder.encode(expected);
  let diff = actualBytes.length ^ expectedBytes.length;
  const length = Math.max(actualBytes.length, expectedBytes.length);
  for (let i = 0; i < length; i += 1) {
    diff |= (actualBytes[i] ?? 0) ^ (expectedBytes[i] ?? 0);
  }
  return diff === 0;
}

function redactSecrets(message: string, env: Env): string {
  const secret = tokenValue(env);
  if (!secret) return message;
  return message.split(secret).join("[redacted]");
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "dynamic worker execution failed";
}

function runResponse(record: RunRecord): Record<string, unknown> {
  const response: Record<string, unknown> = {
    id: record.id,
    workerId: record.workerId,
    status: record.state,
    cacheMode: record.cacheMode,
    egress: record.egress,
    createdAt: record.createdAt,
    startedAt: record.startedAt,
    completedAt: record.completedAt,
    durationMs: record.durationMs,
    result: record.result,
    error: record.error,
    metadata: record.metadata,
    body: record.result?.body,
    stderr: record.error?.message,
    logs: record.logs.map((log) => log.message).join("\n"),
    logEvents: record.logs,
  };
  if (record.state !== "running") {
    response["exitCode"] =
      record.state === "succeeded" && record.result !== undefined && record.result.status < 400
        ? 0
        : 1;
  }
  return response;
}

function runSummary(record: RunRecord): Record<string, unknown> {
  return {
    id: record.id,
    workerId: record.workerId,
    status: record.state,
    cacheMode: record.cacheMode,
    egress: record.egress,
    metadata: record.metadata,
    createdAt: record.createdAt,
    completedAt: record.completedAt,
    durationMs: record.durationMs,
  };
}

async function persistRun(env: Env, record: RunRecord): Promise<void> {
  if (!env.RUNS) return;
  await retryKVWrite(() => env.RUNS!.put(runMetadataKey(record.id), JSON.stringify(record)));
}

async function deletePrecedingRunMetadata(env: Env, record: RunRecord): Promise<boolean> {
  if (!env.RUNS) return true;
  const key = runMetadataKey(record.id);
  const raw = await env.RUNS.get(key);
  if (!raw) return true;

  let stored: RunRecord;
  try {
    stored = JSON.parse(raw) as RunRecord;
  } catch {
    await retryKVWrite(() => env.RUNS!.delete(key));
    return true;
  }
  const storedStartedAt = Date.parse(stored.startedAt);
  const currentStartedAt = Date.parse(record.startedAt);
  if (
    stored.executionId === record.executionId ||
    !Number.isFinite(storedStartedAt) ||
    storedStartedAt <= currentStartedAt
  ) {
    await retryKVWrite(() => env.RUNS!.delete(key));
    return true;
  }
  return false;
}

async function storedRun(env: Env, runId: string): Promise<RunRecord | undefined> {
  const coordination = await coordinateRunRecord(env, runId);
  if (coordination.deleted) return undefined;
  const coordinated = coordination.record;
  if (coordinated?.state === "running") {
    return coordinated;
  }
  const terminal = await storedTerminalRun(env, runId);
  if (coordinated) {
    if (terminal?.executionId !== undefined && terminal.executionId === coordinated.executionId) {
      return terminal;
    }
    return coordinated;
  }
  if (terminal) return terminal;
  return undefined;
}

async function storedTerminalRun(env: Env, runId: string): Promise<RunRecord | undefined> {
  if (env.RUNS) {
    const raw = await env.RUNS.get(runMetadataKey(runId));
    if (raw) {
      const parsed = JSON.parse(raw) as RunRecord;
      if (runRecordExpired(parsed)) {
        await env.RUNS.delete(runMetadataKey(runId));
        return undefined;
      }
      return parsed;
    }
  }
  return undefined;
}

async function storedRuns(env: Env): Promise<RunRecord[]> {
  const records = new Map<string, RunRecord>();
  if (env.RUNS) {
    await loadStoredRuns(env.RUNS, runMetadataPrefix, records);
  }
  const index = await coordinateRunIndexList(env);
  for (const runId of index.deletedRunIds) records.delete(runId);
  for (const record of index.runs) {
    const stored = records.get(record.id);
    const indexedStartedAt = Date.parse(record.startedAt);
    const storedStartedAt = stored === undefined ? Number.NaN : Date.parse(stored.startedAt);
    if (
      stored === undefined ||
      record.executionId !== stored.executionId ||
      indexedStartedAt > storedStartedAt ||
      !Number.isFinite(storedStartedAt)
    ) {
      records.set(record.id, record);
    }
  }
  return [...records.values()];
}

async function loadStoredRuns(
  runs: KVNamespace,
  prefix: string,
  records: Map<string, RunRecord>,
): Promise<void> {
  await loadStoredRunsPage(runs, prefix, records);
}

async function loadStoredRunsPage(
  runs: KVNamespace,
  prefix: string,
  records: Map<string, RunRecord>,
  cursor?: string,
): Promise<void> {
  const listed = await runs.list(cursor === undefined ? { prefix } : { prefix, cursor });
  const keys = listed.keys.map((key) => key.name);
  const batches = Array.from(
    { length: Math.ceil(keys.length / runMetadataBulkReadSize) },
    (_, index) =>
      keys.slice(index * runMetadataBulkReadSize, (index + 1) * runMetadataBulkReadSize),
  );
  const loaded = await Promise.all(batches.map((batch) => loadStoredRunBatch(runs, batch)));
  for (const values of loaded) {
    for (const [, raw] of values) {
      if (!raw) continue;
      const parsed = JSON.parse(raw) as RunRecord;
      if (runRecordExpired(parsed)) continue;
      records.set(parsed.id, parsed);
    }
  }
  if (listed.list_complete === false) {
    await loadStoredRunsPage(runs, prefix, records, listed.cursor);
  }
}

async function loadStoredRunBatch(
  runs: KVNamespace,
  keys: string[],
): Promise<Map<string, string | null>> {
  try {
    return await runs.get(keys);
  } catch (error) {
    if (!kvBulkReadTooLarge(error)) throw error;
    if (keys.length === 1) {
      const key = keys[0]!;
      return new Map([[key, await runs.get(key)]]);
    }
    const middle = Math.ceil(keys.length / 2);
    const [left, right] = await Promise.all([
      loadStoredRunBatch(runs, keys.slice(0, middle)),
      loadStoredRunBatch(runs, keys.slice(middle)),
    ]);
    return new Map([...left, ...right]);
  }
}

function kvBulkReadTooLarge(error: unknown): boolean {
  const message = errorMessage(error).toLowerCase();
  return (
    message.includes("413") ||
    message.includes("request entity too large") ||
    message.includes("response too large")
  );
}

async function deleteRun(env: Env, runId: string): Promise<void> {
  if (!env.RUNS) return;
  await retryKVWrite(() => env.RUNS!.delete(runMetadataKey(runId)));
}

async function coordinateRunStart(
  env: Env,
  runId: string,
  executionId: string,
  expiresAt: number,
  legacyReusableID: boolean,
  legacyConcurrentReuse: boolean,
  record: RunRecord,
): Promise<Response | { generation: number }> {
  const response = await runCoordinator(env, runId).fetch("https://run-coordinator/start", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      executionId,
      expiresAt,
      legacyConcurrentReuse,
      legacyReusableID,
      record: runCoordinationRecord(record),
    }),
  });
  if (!response.ok) return response;
  return (await response.json()) as { generation: number };
}

async function coordinateRunComplete(
  env: Env,
  runId: string,
  executionId: string,
  record: RunRecord,
  release: boolean,
  cleanupPending: boolean,
): Promise<{
  finalized: boolean;
  indexGeneration: number;
  released: boolean;
  activeRecord?: RunRecord;
}> {
  const response = await runCoordinator(env, runId).fetch("https://run-coordinator/complete", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      cleanupPending,
      executionId,
      record: runCoordinationRecord(record),
      release,
    }),
  });
  if (!response.ok) throw new Error(`run coordinator completion failed: ${response.status}`);
  const result = (await response.json()) as {
    finalized?: boolean;
    indexGeneration?: number;
    released?: boolean;
    activeRecord?: RunRecord;
  };
  const completion: {
    finalized: boolean;
    indexGeneration: number;
    released: boolean;
    activeRecord?: RunRecord;
  } = {
    finalized: result.finalized ?? true,
    indexGeneration: result.indexGeneration ?? 0,
    released: result.released === true,
  };
  if (result.activeRecord !== undefined) completion.activeRecord = result.activeRecord;
  return completion;
}

async function coordinateRunCompleteWithRetry(
  env: Env,
  runId: string,
  executionId: string,
  record: RunRecord,
  release: boolean,
  cleanupPending: boolean,
  attempt = 1,
): ReturnType<typeof coordinateRunComplete> {
  try {
    return await coordinateRunComplete(env, runId, executionId, record, release, cleanupPending);
  } catch (error) {
    if (attempt >= 3) throw error;
    return coordinateRunCompleteWithRetry(
      env,
      runId,
      executionId,
      record,
      release,
      cleanupPending,
      attempt + 1,
    );
  }
}

async function coordinateRunRenew(
  env: Env,
  runId: string,
  executionId: string,
  expiresAt: number,
): Promise<{ active: boolean; indexed: boolean; indexGeneration: number }> {
  const response = await runCoordinator(env, runId).fetch("https://run-coordinator/renew", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ executionId, expiresAt }),
  });
  if (!response.ok) throw new Error(`run coordinator renewal failed: ${response.status}`);
  const result = (await response.json()) as {
    stale?: boolean;
    indexed?: boolean;
    indexGeneration?: number;
  };
  return {
    active: result.stale !== true,
    indexed: result.indexed === true,
    indexGeneration: result.indexGeneration ?? 0,
  };
}

async function coordinateRunDelete(
  env: Env,
  runId: string,
  acknowledgedComplete: boolean,
  legacyRecord?: RunRecord,
): Promise<{ known: boolean; indexGeneration: number; record?: RunRecord } | Response> {
  const response = await runCoordinator(env, runId).fetch("https://run-coordinator/delete", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      acknowledgedComplete,
      runId,
      ...(legacyRecord === undefined ? {} : { legacyRecord: runCoordinationRecord(legacyRecord) }),
    }),
  });
  if (!response.ok) return response;
  return (await response.json()) as {
    known: boolean;
    indexGeneration: number;
    record?: RunRecord;
  };
}

async function coordinateRunRecord(
  env: Env,
  runId: string,
): Promise<{ record?: RunRecord; deleted: boolean }> {
  const response = await runCoordinator(env, runId).fetch("https://run-coordinator/record");
  if (response.status === 404) return { deleted: false };
  if (response.status === 410) return { deleted: true };
  if (!response.ok) throw new Error(`run coordinator record lookup failed: ${response.status}`);
  const result = (await response.json()) as { record?: RunRecord };
  return result.record === undefined
    ? { deleted: false }
    : { record: result.record, deleted: false };
}

async function coordinateRunIndexSet(
  coordinator: DurableObjectStub<DynamicWorkerRunCoordinator>,
  record: RunRecord,
  generation: number,
): Promise<void> {
  const response = await coordinator.fetch("https://run-coordinator/index/set", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ generation, record: runIndexRecord(record) }),
  });
  if (!response.ok) throw new Error(`run index update failed: ${response.status}`);
}

async function coordinateRunIndexDelete(
  coordinator: DurableObjectStub<DynamicWorkerRunCoordinator>,
  runId: string,
  generation: number,
  tombstone = false,
  pending = false,
): Promise<void> {
  const response = await coordinator.fetch("https://run-coordinator/index/delete", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ runId, generation, pending, tombstone }),
  });
  if (!response.ok) throw new Error(`run index delete failed: ${response.status}`);
}

async function coordinateRunIndexList(
  env: Env,
): Promise<{ runs: RunRecord[]; deletedRunIds: string[] }> {
  const shards = await Promise.all(
    runIndexCoordinators(env).map(async (coordinator) => {
      try {
        const response = await coordinator.fetch("https://run-coordinator/index/list");
        if (!response.ok) return undefined;
        return (await response.json()) as {
          runs?: RunRecord[];
          deletedRunIds?: string[];
        };
      } catch {
        return undefined;
      }
    }),
  );
  const available = shards.filter(
    (
      result,
    ): result is {
      runs?: RunRecord[];
      deletedRunIds?: string[];
    } => result !== undefined,
  );
  if (available.length !== runIndexShardCount) {
    throw new Error("run index lookup failed for one or more shards");
  }
  return {
    runs: available.flatMap((result) => result.runs ?? []),
    deletedRunIds: available.flatMap((result) => result.deletedRunIds ?? []),
  };
}

function runCoordinator(env: Env, runId: string): DurableObjectStub<DynamicWorkerRunCoordinator> {
  const namespace = env.RUN_COORDINATOR;
  if (!namespace) throw new Error("RUN_COORDINATOR binding is missing");
  return namespace.get(namespace.idFromName(runId));
}

function runIndexCoordinator(
  env: Env,
  runId: string,
): DurableObjectStub<DynamicWorkerRunCoordinator> {
  return runCoordinator(env, runIndexObjectName(runIndexShard(runId)));
}

function runIndexCoordinators(env: Env): DurableObjectStub<DynamicWorkerRunCoordinator>[] {
  return Array.from({ length: runIndexShardCount }, (_, shard) =>
    runCoordinator(env, runIndexObjectName(shard)),
  );
}

function runIndexShard(runId: string): number {
  let hash = 2166136261;
  for (let index = 0; index < runId.length; index += 1) {
    hash = Math.imul(hash ^ runId.charCodeAt(index), 16777619);
  }
  return (hash >>> 0) % runIndexShardCount;
}

function runIndexObjectName(shard: number): string {
  return shard === 0 ? runIndexObjectPrefix : `${runIndexObjectPrefix}/${shard}`;
}

function runMetadataKey(runId: string): string {
  return `${runMetadataPrefix}${runId}`;
}

function runIndexStorageKey(runId: string): string {
  return `${runIndexStoragePrefix}${runId}`;
}

function runIndexDeletionStorageKey(runId: string): string {
  return `${runIndexDeletionStoragePrefix}${runId}`;
}

function runCoordinationRecord(record: RunRecord): RunRecord {
  const summary: RunRecord = {
    id: record.id,
    workerId: record.workerId,
    state: record.state,
    cacheMode: record.cacheMode,
    egress: record.egress,
    createdAt: record.createdAt,
    startedAt: record.startedAt,
    logs: record.logs.slice(-coordinationMaxLogEntries).map((log) => {
      const bounded: RunLog = {
        level: log.level,
        message: boundedText(log.message, coordinationMaxLogMessageLength),
        time: boundedText(log.time, 64),
      };
      if (log.eventCount !== undefined) bounded.eventCount = log.eventCount;
      return bounded;
    }),
  };
  if (record.executionId !== undefined) summary.executionId = record.executionId;
  if (record.expiresAt !== undefined) summary.expiresAt = record.expiresAt;
  if (record.completedAt !== undefined) summary.completedAt = record.completedAt;
  if (record.durationMs !== undefined) summary.durationMs = record.durationMs;
  if (record.metadata !== undefined) {
    summary.metadata = boundedStringRecord(
      record.metadata,
      coordinationMaxMetadataEntries,
      coordinationMaxMetadataKeyLength,
      coordinationMaxMetadataValueLength,
    );
  }
  if (record.result !== undefined) {
    summary.result = {
      status: record.result.status,
      statusText: boundedText(record.result.statusText, 256),
      headers: boundedStringRecord(
        record.result.headers,
        coordinationMaxHeaderEntries,
        coordinationMaxHeaderNameLength,
        coordinationMaxHeaderValueLength,
      ),
    };
  }
  if (record.error !== undefined) {
    summary.error = {
      message: boundedText(record.error.message, coordinationMaxErrorMessageLength),
    };
  }
  return summary;
}

function runIndexRecord(record: RunRecord): RunRecord {
  const { result: _result, error: _error, ...summary } = runCoordinationRecord(record);
  return {
    ...summary,
    logs: [],
  };
}

function boundedStringRecord(
  values: Record<string, string>,
  maxEntries: number,
  maxKeyLength: number,
  maxValueLength: number,
): Record<string, string> {
  return Object.fromEntries(
    Object.entries(values)
      .slice(0, maxEntries)
      .map(([key, value]) => [boundedText(key, maxKeyLength), boundedText(value, maxValueLength)]),
  );
}

function boundedText(value: string, maxLength: number): string {
  return value.length <= maxLength ? value : value.slice(0, maxLength);
}

function bestEffortRunIndex(ctx: DynamicWorkerContext, operation: Promise<void>): Promise<void> {
  const task = operation.catch(() => undefined);
  ctx.waitUntil?.(task);
  return task;
}

function runRecordExpired(record: RunRecord): boolean {
  return (
    record.state === "running" &&
    record.expiresAt !== undefined &&
    Date.parse(record.expiresAt) <= Date.now()
  );
}

function runReservationExpiresAt(parsed: ParsedRunRequest, startedAt: number): number {
  const ttlSeconds =
    parsed.timeoutMs === undefined
      ? defaultRunReservationTTLSeconds
      : Math.max(
          defaultRunReservationTTLSeconds,
          Math.ceil(parsed.timeoutMs / 1000) + runReservationGraceSeconds,
        );
  return startedAt + ttlSeconds * 1000;
}

function startRunHeartbeat(
  env: Env,
  ctx: DynamicWorkerContext,
  runId: string,
  executionId: string,
  record: RunRecord,
  indexCoordinator: DurableObjectStub<DynamicWorkerRunCoordinator>,
  indexSet: Promise<void>,
): () => void {
  let stopped = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const renew = () => {
    timer = setTimeout(() => {
      if (stopped) return;
      const currentExpiresAt =
        record.expiresAt === undefined ? Number.NaN : Date.parse(record.expiresAt);
      const expiresAt = Math.max(
        Date.now() + defaultRunReservationTTLSeconds * 1000,
        Number.isFinite(currentExpiresAt) ? currentExpiresAt : 0,
      );
      const task = coordinateRunRenew(env, runId, executionId, expiresAt)
        .then(async (renewal) => {
          if (renewal.active && !stopped) {
            record.expiresAt = new Date(expiresAt).toISOString();
            if (renewal.indexed) {
              await indexSet;
              await coordinateRunIndexSet(indexCoordinator, record, renewal.indexGeneration);
            }
            renew();
          }
          return undefined;
        })
        .catch(() => {
          if (!stopped) renew();
        });
      ctx.waitUntil?.(task);
    }, runReservationRenewIntervalMs);
  };
  renew();
  return () => {
    stopped = true;
    if (timer !== undefined) clearTimeout(timer);
  };
}

async function retryKVWrite(operation: () => Promise<unknown>, attempt = 1): Promise<void> {
  try {
    await operation();
  } catch (error) {
    if (attempt === kvWriteMaxAttempts || !kvWriteRateLimited(error)) throw error;
    await new Promise((resolve) => setTimeout(resolve, kvWriteRetryDelayMs));
    await retryKVWrite(operation, attempt + 1);
  }
}

function kvWriteRateLimited(error: unknown): boolean {
  const message = errorMessage(error).toLowerCase();
  return message.includes("429") || message.includes("too many requests");
}

function coordinationRecord(value: unknown): value is RunRecord {
  return (
    isRecord(value) &&
    typeof value["id"] === "string" &&
    typeof value["workerId"] === "string" &&
    typeof value["state"] === "string" &&
    Array.isArray(value["logs"])
  );
}

function appendLog(executionId: string, log: RunLog): void {
  const record = executionStore.get(executionId);
  if (record) record.logs.push(log);
}

function makeRunId(): string {
  return `run_${crypto.randomUUID()}`;
}

function parseCacheMode(value: string): CacheMode | "" {
  return supportedCacheModes.includes(value as CacheMode) ? (value as CacheMode) : "";
}

function parseEgressMode(value: string): EgressMode | "" {
  return supportedEgressModes.includes(value as EgressMode) ? (value as EgressMode) : "";
}

function cleanRunId(value: string): string {
  const trimmed = value.trim();
  if (!/^[A-Za-z0-9_.:-]{1,128}$/.test(trimmed)) return "";
  return trimmed;
}

function cleanModuleName(value: string): boolean {
  return /^[A-Za-z0-9_./:-]{1,256}$/.test(value) && !value.includes("..");
}

function typescriptModuleName(value: string): boolean {
  const lowerName = value.toLowerCase();
  return [".ts", ".tsx", ".mts", ".cts"].some((extension) => lowerName.endsWith(extension));
}

function cleanCompatibilityDate(value: string): string {
  const trimmed = value.trim();
  return /^\d{4}-\d{2}-\d{2}$/.test(trimmed) ? trimmed : "";
}

function cleanHeaderName(value: string): boolean {
  return /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/.test(value);
}

function cleanHostname(value: string): boolean {
  return /^[A-Za-z0-9.-]{1,253}$/.test(value) && !value.includes("..");
}

function validHttpUrl(value: string): boolean {
  try {
    const url = new URL(value);
    return url.protocol === "http:" || url.protocol === "https:";
  } catch {
    return false;
  }
}

function bodylessMethod(method: string): boolean {
  return method === "GET" || method === "HEAD";
}

function json(value: unknown, status = 200): Response {
  return Response.json(value, { status });
}

function stringField(value: Record<string, unknown>, key: string): string | undefined {
  const field = value[key];
  return typeof field === "string" ? field : undefined;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
