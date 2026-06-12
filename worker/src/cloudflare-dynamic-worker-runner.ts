import { WorkerEntrypoint } from "cloudflare:workers";

const runnerName = "cloudflare-dynamic-workers";
const defaultCompatibilityDate = "2026-06-12";
const runMetadataPrefix = "runs:";
const supportedCacheModes = ["one-shot", "stable", "explicit"] as const;
const supportedEgressModes = ["blocked", "intercept"] as const;

const runStore = new Map<string, RunRecord>();

export class HttpGateway extends WorkerEntrypoint<Env, GatewayProps> {
  override async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const props = this.ctx.props;
    appendLog(props.runId, {
      level: "info",
      message: `egress fetch ${url.origin}`,
      time: new Date().toISOString(),
    });

    if (props.allowHostnames.length > 0 && !props.allowHostnames.includes(url.hostname)) {
      appendLog(props.runId, {
        level: "warn",
        message: `egress blocked ${url.hostname}`,
        time: new Date().toISOString(),
      });
      return Response.json({ error: "egress blocked" }, { status: 403 });
    }

    return fetch(request);
  }
}

export class LogTailer extends WorkerEntrypoint<Env, TailProps> {
  override tail(events: unknown[]): void {
    appendLog(this.ctx.props.runId, {
      level: "info",
      message: "dynamic worker tail event received",
      time: new Date().toISOString(),
      eventCount: events.length,
    });
  }
}

type Env = {
  LOADER?: DynamicWorkerLoader;
  RUNS?: KVNamespace;
  CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN?: string;
  CRABBOX_RUNNER_TOKEN?: string;
};

type DynamicWorkerContext = {
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
  runId: string;
  allowHostnames: string[];
};

type TailProps = {
  runId: string;
};

type CacheMode = (typeof supportedCacheModes)[number];
type EgressMode = (typeof supportedEgressModes)[number];
type RunState = "succeeded" | "failed" | "stopped";

type RunRecord = {
  id: string;
  workerId: string;
  state: RunState;
  cacheMode: CacheMode;
  egress: EgressMode;
  createdAt: string;
  startedAt: string;
  completedAt: string;
  durationMs: number;
  result?: RunResult;
  error?: RunError;
  logs: RunLog[];
};

type RunResult = {
  status: number;
  statusText: string;
  headers: Record<string, string>;
  body: string;
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

    const runId = decodeURIComponent(match[1] ?? "");
    const action = match[2] ?? "";
    if (!cleanRunId(runId)) return json({ error: "run id is invalid" }, 400);

    if (request.method === "GET" && action === "") return getRun(env, runId);
    if (request.method === "GET" && action === "logs") return getRunLogs(env, runId);
    if (request.method === "DELETE" && action === "") return stopRun(env, runId);

    return json({ error: "not found" }, 404);
  },
};

function readiness(env: Env): Response {
  if (!env.LOADER) {
    return json(
      {
        ok: false,
        runner: runnerName,
        error: "missing loader binding: LOADER",
        missing: ["LOADER"],
      },
      503,
    );
  }

  return json({
    ok: true,
    runner: runnerName,
    loader: true,
    loaderBinding: true,
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
  if (!env.LOADER) return readiness(env);
  if (!isJsonRequest(request)) return json({ error: "content-type must be application/json" }, 415);

  const body = await readObject(request);
  if (body instanceof Response) return body;

  const parsed = parseRunRequest(body, url);
  if (parsed instanceof Response) return parsed;

  const startedAt = Date.now();
  const workerCodeResult = workerCodeForRun(parsed, ctx);
  if (workerCodeResult instanceof Response) return workerCodeResult;

  const log: RunLog = {
    level: "info",
    message: "run started",
    time: new Date(startedAt).toISOString(),
  };
  const record: RunRecord = {
    id: parsed.id,
    workerId: parsed.workerId,
    state: "failed",
    cacheMode: parsed.cacheMode,
    egress: parsed.egress,
    createdAt: new Date(startedAt).toISOString(),
    startedAt: new Date(startedAt).toISOString(),
    completedAt: new Date(startedAt).toISOString(),
    durationMs: 0,
    logs: [log],
  };
  await persistRun(env, record);

  try {
    const worker =
      parsed.cacheMode === "one-shot"
        ? env.LOADER.load(workerCodeResult)
        : env.LOADER.get(parsed.workerId, async () => workerCodeResult);
    const entrypoint = worker.getEntrypoint(undefined, entrypointOptions(parsed.limits));
    const dynamicResponse = await entrypoint.fetch(requestForDynamicWorker(parsed));
    const result = await responseResult(dynamicResponse);
    const completedAt = Date.now();
    record.state = "succeeded";
    record.completedAt = new Date(completedAt).toISOString();
    record.durationMs = completedAt - startedAt;
    record.result = result;
    appendLog(parsed.id, {
      level: "info",
      message: `run completed with HTTP ${result.status}`,
      time: record.completedAt,
    });
    await persistRun(env, record);
    return json(runResponse(record));
  } catch (error) {
    const completedAt = Date.now();
    record.state = "failed";
    record.completedAt = new Date(completedAt).toISOString();
    record.durationMs = completedAt - startedAt;
    record.error = { message: redactSecrets(errorMessage(error), env) };
    appendLog(parsed.id, {
      level: "error",
      message: "run failed",
      time: record.completedAt,
    });
    await persistRun(env, record);
    return json(runResponse(record), 502);
  }
}

async function listRuns(env: Env): Promise<Response> {
  return json({
    runs: (await storedRuns(env)).map((record) => runSummary(record)),
  });
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

async function stopRun(env: Env, runId: string): Promise<Response> {
  const record = await storedRun(env, runId);
  if (!record) return json({ error: "not found" }, 404);

  const stoppedAt = new Date().toISOString();
  record.state = "stopped";
  record.completedAt = stoppedAt;
  appendLog(runId, {
    level: "info",
    message: "run metadata stopped",
    time: stoppedAt,
  });
  await deleteRun(env, runId);
  return json(runResponse(record));
}

function workerCodeForRun(
  parsed: ParsedRunRequest,
  ctx: DynamicWorkerContext,
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
        runId: parsed.id,
        allowHostnames: parsed.gateway.allowHostnames,
      },
    });
    code.tails = [
      tailer({
        props: {
          runId: parsed.id,
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

async function responseResult(response: Response): Promise<RunResult> {
  const headers: Record<string, string> = {};
  for (const [key, value] of response.headers) {
    headers[key] = value;
  }
  return {
    status: response.status,
    statusText: response.statusText,
    headers,
    body: await response.text(),
  };
}

type ParsedRunRequest = {
  id: string;
  workerId: string;
  cacheMode: CacheMode;
  egress: EgressMode;
  mainModule: string;
  modules: Record<string, WorkerModule>;
  compatibilityDate: string;
  compatibilityFlags: string[];
  env?: Record<string, unknown>;
  limits?: WorkerLimits;
  timeoutMs?: number;
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
  if (!id) return json({ error: "id is invalid" }, 400);

  const cacheMode = parseCacheMode(stringField(body, "cacheMode") ?? "one-shot");
  if (!cacheMode) return json({ error: "cacheMode must be one-shot, stable, or explicit" }, 400);

  const rawWorkerId = stringField(body, "workerId") ?? id;
  const workerId = cleanRunId(rawWorkerId);
  if (!workerId) return json({ error: "workerId is invalid" }, 400);

  const egress = parseEgressMode(
    url.searchParams.get("egress") ?? stringField(body, "egress") ?? "blocked",
  );
  if (!egress) return json({ error: "egress must be blocked or intercept" }, 400);

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

  const limits = parseLimits(body["limits"]);
  if (limits instanceof Response) return limits;

  const timeoutMs = parsePositiveInteger(body["timeoutMs"]);
  if (timeoutMs instanceof Response) return timeoutMs;

  const env = parseOptionalRecord(body["env"]);
  if (env instanceof Response) return env;

  const requestSpec = parseInvocationRequest(body["request"]);
  if (requestSpec instanceof Response) return requestSpec;

  const gateway = parseGateway(body["gateway"]);
  if (gateway instanceof Response) return gateway;

  const parsed: ParsedRunRequest = {
    id,
    workerId,
    cacheMode,
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
  return parsed;
}

function parseModules(value: unknown): Record<string, WorkerModule> | Response {
  if (!isRecord(value)) return json({ error: "modules must be an object" }, 400);

  const modules: Record<string, WorkerModule> = {};
  for (const [name, moduleValue] of Object.entries(value)) {
    if (!cleanModuleName(name)) return json({ error: "module name is invalid" }, 400);
    if (typeof moduleValue === "string") {
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

  return {
    mainModule: name,
    modules: {
      [name]: source,
    },
  };
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

function parsePositiveInteger(value: unknown): number | Response | undefined {
  if (value === undefined) return undefined;
  return typeof value === "number" && Number.isInteger(value) && value > 0
    ? value
    : json({ error: "value must be a positive integer" }, 400);
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
  return isRecord(value) ? value : {};
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
  const exitCode =
    record.state === "succeeded" && record.result !== undefined && record.result.status < 400
      ? 0
      : 1;
  return {
    id: record.id,
    workerId: record.workerId,
    status: record.state,
    exitCode,
    cacheMode: record.cacheMode,
    egress: record.egress,
    createdAt: record.createdAt,
    startedAt: record.startedAt,
    completedAt: record.completedAt,
    durationMs: record.durationMs,
    result: record.result,
    error: record.error,
    body: record.result?.body,
    stderr: record.error?.message,
    logs: record.logs.map((log) => log.message).join("\n"),
    logEvents: record.logs,
  };
}

function runSummary(record: RunRecord): Record<string, unknown> {
  return {
    id: record.id,
    workerId: record.workerId,
    status: record.state,
    cacheMode: record.cacheMode,
    egress: record.egress,
    createdAt: record.createdAt,
    completedAt: record.completedAt,
    durationMs: record.durationMs,
  };
}

async function persistRun(env: Env, record: RunRecord): Promise<void> {
  runStore.set(record.id, record);
  if (!env.RUNS) return;
  await env.RUNS.put(runMetadataKey(record.id), JSON.stringify(record));
}

async function storedRun(env: Env, runId: string): Promise<RunRecord | undefined> {
  if (env.RUNS) {
    const raw = await env.RUNS.get(runMetadataKey(runId));
    if (raw) {
      const parsed = JSON.parse(raw) as RunRecord;
      runStore.set(parsed.id, parsed);
      return parsed;
    }
  }
  return runStore.get(runId);
}

async function storedRuns(env: Env): Promise<RunRecord[]> {
  const records = new Map<string, RunRecord>();
  for (const record of runStore.values()) records.set(record.id, record);
  if (env.RUNS) {
    const listed = await env.RUNS.list({ prefix: runMetadataPrefix });
    await Promise.all(
      listed.keys.map(async (key) => {
        const raw = await env.RUNS?.get(key.name);
        if (!raw) return;
        const parsed = JSON.parse(raw) as RunRecord;
        records.set(parsed.id, parsed);
      }),
    );
  }
  return [...records.values()];
}

async function deleteRun(env: Env, runId: string): Promise<void> {
  runStore.delete(runId);
  if (!env.RUNS) return;
  await env.RUNS.delete(runMetadataKey(runId));
}

function runMetadataKey(runId: string): string {
  return `${runMetadataPrefix}${runId}`;
}

function appendLog(runId: string, log: RunLog): void {
  const record = runStore.get(runId);
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
