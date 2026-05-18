import { Container, getContainer } from "@cloudflare/containers";

const leaseMetaKey = "crabbox:lease";
const cleanupCallback = "expireIfIdle";
const defaultInstanceType = "standard-4";
const instanceTypes = [
  "lite",
  "basic",
  "standard-1",
  "standard-2",
  "standard-3",
  "standard-4",
] as const;

class SandboxBase extends Container {
  override defaultPort = 8787;
  override sleepAfter = "30m";
  override enableInternet = true;

  override async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);
    if (url.pathname === "/__crabbox/create" && request.method === "POST") {
      return this.createLease(request);
    }
    if (url.pathname === "/__crabbox/status" && request.method === "GET") {
      return this.leaseStatus();
    }
    if (url.pathname === "/__crabbox/destroy" && request.method === "DELETE") {
      return this.destroyLease();
    }
    if (url.pathname === "/__crabbox/files" && request.method === "POST") {
      return this.uploadLeaseFile(request, url);
    }
    if (url.pathname === "/__crabbox/exec-stream" && request.method === "POST") {
      return this.execLeaseStream(request);
    }
    return super.fetch(request);
  }

  override async onActivityExpired(): Promise<void> {
    const meta = await this.leaseMeta();
    if (!meta || meta.state !== "running") {
      await this.stop();
      return;
    }

    const expired = await this.expireIfNeeded(meta);
    if (expired.state !== "running") return;

    await this.scheduleCleanup(expired);
    this.renewActivityTimeout();
  }

  async expireIfIdle(): Promise<void> {
    const meta = await this.leaseMeta();
    if (!meta || meta.state !== "running") return;

    const now = Date.now();
    const expiresAt = leaseExpiresAtMs(meta);
    if (expiresAt === undefined || expiresAt > now) {
      await this.scheduleCleanup(meta);
      return;
    }

    const expired: LeaseMetadata = {
      ...meta,
      state: "expired",
      expiredAt: new Date(now).toISOString(),
    };
    await this.ctx.storage.put(leaseMetaKey, expired);
    this.deleteSchedules(cleanupCallback);
    await this.destroy();
  }

  private async createLease(request: Request): Promise<Response> {
    const body = await readObject(request);
    if (body instanceof Response) return body;
    const id = cleanSandboxID(stringField(body, "id") ?? stringField(body, "leaseId") ?? "");
    if (!id) return json({ error: "id is required" }, 400);

    const workdir = cleanAbsolutePath(stringField(body, "workdir") ?? "/workspace/crabbox");
    if (!workdir) return json({ error: "workdir must be an absolute path" }, 400);

    const now = new Date();
    const existing = await this.leaseMeta();
    const ttlSeconds = positiveIntegerField(body, "ttlSeconds");
    const idleTimeoutSeconds = positiveIntegerField(body, "idleTimeoutSeconds");
    const instanceType = cleanInstanceType(
      stringField(body, "instanceType") ?? defaultInstanceType,
    );
    if (!instanceType) return json({ error: "instanceType is not supported" }, 400);
    const meta: LeaseMetadata = {
      id,
      state: "running",
      workdir,
      instanceType,
      labels: sanitizeLabels(body["labels"]),
      createdAt: existing?.createdAt ?? now.toISOString(),
      lastTouchedAt: now.toISOString(),
    };
    if (ttlSeconds !== undefined) meta.ttlSeconds = ttlSeconds;
    if (idleTimeoutSeconds !== undefined) meta.idleTimeoutSeconds = idleTimeoutSeconds;
    await this.ctx.storage.put(leaseMetaKey, meta);
    await this.scheduleCleanup(meta);
    try {
      await this.ensureReady();
    } catch (error) {
      const stopped: LeaseMetadata = {
        ...meta,
        state: "stopped",
        stoppedAt: new Date().toISOString(),
      };
      await this.ctx.storage.put(leaseMetaKey, stopped);
      this.deleteSchedules(cleanupCallback);
      await this.destroy();
      throw error;
    }

    return json(leaseResponse(meta));
  }

  private async leaseStatus(): Promise<Response> {
    const meta = await this.leaseMeta();
    if (!meta) return json({ error: "not found" }, 404);

    const expired = await this.expireIfNeeded(meta);
    if (expired.state === "expired") {
      return json(leaseResponse(expired));
    }

    const state = await this.getState();
    return json(leaseResponse(expired, state.status));
  }

  private async destroyLease(): Promise<Response> {
    const meta = await this.leaseMeta();
    const stopped: LeaseMetadata = {
      ...(meta ?? emptyLeaseMeta()),
      state: "stopped",
      stoppedAt: new Date().toISOString(),
    };
    await this.ctx.storage.put(leaseMetaKey, stopped);
    this.deleteSchedules(cleanupCallback);
    await this.destroy();
    return json(leaseResponse(stopped));
  }

  private async uploadLeaseFile(request: Request, url: URL): Promise<Response> {
    const remotePath = cleanAbsolutePath(url.searchParams.get("path") ?? "");
    if (!remotePath) return json({ error: "path must be absolute" }, 400);
    if (!request.body) return json({ error: "request body is required" }, 400);

    const meta = await this.beginExecution();
    if (meta.state !== "running") return expiredResponse(meta);

    try {
      await this.ensureReady();
      const uploadURL = new URL("http://container/v1/files");
      uploadURL.searchParams.set("path", remotePath);
      return await this.containerFetch(uploadURL, {
        method: "POST",
        body: request.body,
        headers: {
          "Content-Type": "application/octet-stream",
        },
      });
    } finally {
      await this.finishExecution();
    }
  }

  private async execLeaseStream(request: Request): Promise<Response> {
    const body = await readObject(request);
    if (body instanceof Response) return body;
    const command = stringField(body, "command")?.trim() ?? "";
    if (!command) return json({ error: "command is required" }, 400);

    const cwd = cleanAbsolutePath(stringField(body, "cwd") ?? "/workspace/crabbox");
    if (!cwd) return json({ error: "cwd must be an absolute path" }, 400);

    const meta = await this.beginExecution();
    if (meta.state !== "running") return expiredResponse(meta);

    try {
      await this.ensureReady();
      const response = await this.execContainer({
        command,
        cwd,
        env: sanitizeEnv(body["env"]),
        timeoutMs: numberField(body, "timeoutMs"),
      });
      return this.finishExecutionWhenStreamCloses(response);
    } catch (error) {
      await this.finishExecution();
      throw error;
    }
  }

  private async beginExecution(): Promise<LeaseMetadata> {
    return this.ctx.blockConcurrencyWhile(async () => {
      const meta = await this.leaseMeta();
      if (!meta) {
        return emptyLeaseMeta("expired");
      }
      const expired = await this.expireIfNeeded(meta);
      if (expired.state !== "running") return expired;

      const active: LeaseMetadata = {
        ...expired,
        activeExecutions: (expired.activeExecutions ?? 0) + 1,
        lastTouchedAt: new Date().toISOString(),
      };
      await this.ctx.storage.put(leaseMetaKey, active);
      await this.scheduleCleanup(active);
      return active;
    });
  }

  private async finishExecution(): Promise<void> {
    await this.ctx.blockConcurrencyWhile(async () => {
      const meta = await this.leaseMeta();
      if (!meta || meta.state !== "running") return;

      const activeExecutions = Math.max((meta.activeExecutions ?? 0) - 1, 0);
      const touched: LeaseMetadata = {
        ...meta,
        lastTouchedAt: new Date().toISOString(),
      };
      if (activeExecutions > 0) {
        touched.activeExecutions = activeExecutions;
      } else {
        delete touched.activeExecutions;
      }
      await this.ctx.storage.put(leaseMetaKey, touched);
      await this.scheduleCleanup(touched);
    });
  }

  private async touchLease(): Promise<LeaseMetadata> {
    const meta = await this.leaseMeta();
    if (!meta) {
      return emptyLeaseMeta("expired");
    }
    const expired = await this.expireIfNeeded(meta);
    if (expired.state !== "running") return expired;

    const touched: LeaseMetadata = {
      ...expired,
      lastTouchedAt: new Date().toISOString(),
    };
    await this.ctx.storage.put(leaseMetaKey, touched);
    await this.scheduleCleanup(touched);
    return touched;
  }

  private async expireIfNeeded(meta: LeaseMetadata): Promise<LeaseMetadata> {
    if (meta.state !== "running") return meta;
    const expiresAt = leaseExpiresAtMs(meta);
    if (expiresAt === undefined || expiresAt > Date.now()) return meta;

    const expired: LeaseMetadata = {
      ...meta,
      state: "expired",
      expiredAt: new Date().toISOString(),
    };
    await this.ctx.storage.put(leaseMetaKey, expired);
    this.deleteSchedules(cleanupCallback);
    await this.destroy();
    return expired;
  }

  private async leaseMeta(): Promise<LeaseMetadata | undefined> {
    return this.ctx.storage.get<LeaseMetadata>(leaseMetaKey);
  }

  private async scheduleCleanup(meta: LeaseMetadata): Promise<void> {
    this.deleteSchedules(cleanupCallback);
    if (meta.state !== "running") return;
    const expiresAt = leaseExpiresAtMs(meta);
    if (expiresAt === undefined) return;
    await this.schedule(new Date(expiresAt), cleanupCallback);
  }

  private async ensureReady(): Promise<void> {
    await this.startAndWaitForPorts({
      ports: 8787,
      cancellationOptions: {
        instanceGetTimeoutMS: 120_000,
        portReadyTimeoutMS: 120_000,
        waitInterval: 1_000,
      },
    });
  }

  private async execContainer(payload: Record<string, unknown>): Promise<Response> {
    return this.containerFetch("http://container/v1/exec", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify(payload),
    });
  }

  private finishExecutionWhenStreamCloses(response: Response): Response {
    if (!response.body) {
      this.finishExecutionAfterResponse();
      return response;
    }
    const reader = response.body.getReader();
    const clientBody = new ReadableStream<Uint8Array>({
      pull: async (controller) => {
        let next: ReadableStreamReadResult<Uint8Array>;
        try {
          next = await reader.read();
        } catch (error) {
          this.finishExecutionAfterResponse();
          controller.error(error);
          return;
        }
        if (next.done) {
          controller.close();
          this.finishExecutionAfterResponse();
          return;
        }
        controller.enqueue(next.value);
      },
      cancel: async (reason) => {
        try {
          await reader.cancel(reason);
        } finally {
          this.finishExecutionAfterResponse();
        }
      },
    });
    return new Response(clientBody, {
      status: response.status,
      statusText: response.statusText,
      headers: response.headers,
    });
  }

  private finishExecutionAfterResponse(): void {
    this.ctx.waitUntil(this.finishExecution());
  }
}

export class Sandbox extends SandboxBase {}
export class SandboxLite extends SandboxBase {}
export class SandboxBasic extends SandboxBase {}
export class SandboxStandard1 extends SandboxBase {}
export class SandboxStandard2 extends SandboxBase {}
export class SandboxStandard3 extends SandboxBase {}

type Env = {
  Sandbox: DurableObjectNamespace<Sandbox>;
  SandboxLite: DurableObjectNamespace<SandboxLite>;
  SandboxBasic: DurableObjectNamespace<SandboxBasic>;
  SandboxStandard1: DurableObjectNamespace<SandboxStandard1>;
  SandboxStandard2: DurableObjectNamespace<SandboxStandard2>;
  SandboxStandard3: DurableObjectNamespace<SandboxStandard3>;
  CRABBOX_RUNNER_TOKEN?: string;
};

type InstanceType = (typeof instanceTypes)[number];

type SandboxNamespace =
  | DurableObjectNamespace<Sandbox>
  | DurableObjectNamespace<SandboxLite>
  | DurableObjectNamespace<SandboxBasic>
  | DurableObjectNamespace<SandboxStandard1>
  | DurableObjectNamespace<SandboxStandard2>
  | DurableObjectNamespace<SandboxStandard3>;

type SandboxBindingName =
  | "Sandbox"
  | "SandboxLite"
  | "SandboxBasic"
  | "SandboxStandard1"
  | "SandboxStandard2"
  | "SandboxStandard3";

type ResolvedSandbox = {
  container: DurableObjectStub<SandboxBase>;
  status: Response;
  instanceType: InstanceType;
};

type LeaseState = "running" | "expired" | "stopped";

type LeaseMetadata = {
  id: string;
  state: LeaseState;
  workdir: string;
  instanceType: string;
  labels: Record<string, string>;
  createdAt: string;
  lastTouchedAt: string;
  ttlSeconds?: number;
  idleTimeoutSeconds?: number;
  activeExecutions?: number;
  expiredAt?: string;
  stoppedAt?: string;
};

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);

    if (url.pathname === "/health") {
      return json({ ok: true, runner: "cloudflare" });
    }

    const auth = authorize(request, env);
    if (auth) return auth;

    if (url.pathname === "/v1/readiness" && request.method === "GET") return runnerReadiness(env);

    if (url.pathname === "/v1/sandboxes" && request.method === "POST") {
      return createSandbox(request, env);
    }

    const match = url.pathname.match(/^\/v1\/sandboxes\/([^/]+)(?:\/([^/]+))?$/);
    if (!match) return json({ error: "not found" }, 404);

    const sandboxID = decodeURIComponent(match[1] ?? "");
    const action = match[2] ?? "";

    if (request.method === "GET" && action === "") {
      return getSandboxStatus(env, sandboxID, url);
    }
    if (request.method === "DELETE" && action === "") {
      return destroySandbox(env, sandboxID, url);
    }
    if (request.method === "POST" && action === "files") {
      return uploadFile(request, env, sandboxID, url);
    }
    if (request.method === "POST" && action === "exec-stream") {
      return execStream(request, env, sandboxID, url);
    }

    return json({ error: "not found" }, 404);
  },
};

async function createSandbox(request: Request, env: Env): Promise<Response> {
  const body = await readObject(request);
  if (body instanceof Response) return body;
  const sandboxID = cleanSandboxID(stringField(body, "id") ?? stringField(body, "leaseId") ?? "");
  if (!sandboxID) return json({ error: "id is required" }, 400);

  const workdir = cleanAbsolutePath(stringField(body, "workdir") ?? "/workspace/crabbox");
  if (!workdir) return json({ error: "workdir must be an absolute path" }, 400);

  const instanceType = cleanInstanceType(stringField(body, "instanceType") ?? defaultInstanceType);
  if (!instanceType) return json({ error: "instanceType is not supported" }, 400);

  const container = getContainer(namespaceForInstanceType(env, instanceType), sandboxID);
  const sanitizedBody = { ...body, id: sandboxID, workdir, instanceType };
  return container.fetch(
    internalRequest("/__crabbox/create", undefined, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(sanitizedBody),
    }),
  );
}

async function getSandboxStatus(env: Env, sandboxID: string, url: URL): Promise<Response> {
  const id = cleanSandboxID(sandboxID);
  if (!id) return json({ error: "id is required" }, 400);
  const requested = requestedInstanceType(url);
  if (requested instanceof Response) return requested;
  const resolved = await resolveSandbox(env, id, requested);
  if (resolved instanceof Response) return resolved;
  return resolved.status;
}

async function destroySandbox(env: Env, sandboxID: string, url: URL): Promise<Response> {
  const id = cleanSandboxID(sandboxID);
  if (!id) return json({ error: "id is required" }, 400);
  const requested = requestedInstanceType(url);
  if (requested instanceof Response) return requested;
  const resolved = await resolveSandbox(env, id, requested);
  if (resolved instanceof Response) return resolved;
  return resolved.container.fetch(
    internalRequest("/__crabbox/destroy", undefined, { method: "DELETE" }),
  );
}

async function uploadFile(
  request: Request,
  env: Env,
  sandboxID: string,
  url: URL,
): Promise<Response> {
  const id = cleanSandboxID(sandboxID);
  if (!id) return json({ error: "id is required" }, 400);

  const requested = requestedInstanceType(url);
  if (requested instanceof Response) return requested;
  const resolved = await resolveSandbox(env, id, requested);
  if (resolved instanceof Response) return resolved;
  return resolved.container.fetch(
    internalRequest(`/__crabbox/files${url.search}`, request, {
      method: "POST",
    }),
  );
}

async function execStream(
  request: Request,
  env: Env,
  sandboxID: string,
  url?: URL,
): Promise<Response> {
  const id = cleanSandboxID(sandboxID);
  if (!id) return json({ error: "id is required" }, 400);

  const requested = url ? requestedInstanceType(url) : undefined;
  if (requested instanceof Response) return requested;
  const resolved = await resolveSandbox(env, id, requested);
  if (resolved instanceof Response) return resolved;
  return resolved.container.fetch(
    internalRequest("/__crabbox/exec-stream", request, {
      method: "POST",
    }),
  );
}

async function resolveSandbox(
  env: Env,
  sandboxID: string,
  requested?: InstanceType,
): Promise<ResolvedSandbox | Response> {
  const candidates = requested
    ? [requested, ...instanceTypes.filter((instanceType) => instanceType !== requested)]
    : instanceTypes;
  for (const instanceType of candidates) {
    const container = getContainer(namespaceForInstanceType(env, instanceType), sandboxID);
    // oxlint-disable-next-line eslint/no-await-in-loop -- stop at the first binding that owns this lease.
    const status = await container.fetch(internalRequest("/__crabbox/status"));
    if (status.status !== 404) return { container, status, instanceType };
  }
  return json({ error: "not found" }, 404);
}

function runnerReadiness(env: Env): Response {
  const missing = missingContainerBindings(env);
  if (missing.length > 0) {
    return json(
      {
        ok: false,
        runner: "cloudflare",
        error: `missing container bindings: ${missing.join(", ")}`,
        missing,
      },
      503,
    );
  }
  return json({ ok: true, runner: "cloudflare", instanceTypes });
}

function missingContainerBindings(env: Env): string[] {
  const missing: string[] = [];
  for (const instanceType of instanceTypes) {
    const name = namespaceBindingNameForInstanceType(instanceType);
    if ((env[name] as unknown) === undefined || (env[name] as unknown) === null) {
      missing.push(name);
    }
  }
  return missing;
}

function namespaceBindingNameForInstanceType(instanceType: InstanceType): SandboxBindingName {
  switch (instanceType) {
    case "lite":
      return "SandboxLite";
    case "basic":
      return "SandboxBasic";
    case "standard-1":
      return "SandboxStandard1";
    case "standard-2":
      return "SandboxStandard2";
    case "standard-3":
      return "SandboxStandard3";
    case "standard-4":
      return "Sandbox";
  }
}

function namespaceForInstanceType(env: Env, instanceType: InstanceType): SandboxNamespace {
  return env[namespaceBindingNameForInstanceType(instanceType)];
}

function requestedInstanceType(url: URL): InstanceType | Response | undefined {
  const raw = url.searchParams.get("instanceType") ?? url.searchParams.get("serverType");
  if (raw === null) return undefined;
  const instanceType = cleanInstanceType(raw);
  return instanceType || json({ error: "instanceType is not supported" }, 400);
}

function authorize(request: Request, env: Env): Response | null {
  const expected = env.CRABBOX_RUNNER_TOKEN;
  if (!expected) return json({ error: "runner token is not configured" }, 503);
  const header = request.headers.get("Authorization") ?? "";
  const actual = header.startsWith("Bearer ") ? header.slice("Bearer ".length) : "";
  if (!tokenEquals(actual, expected)) return json({ error: "unauthorized" }, 401);
  return null;
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

function json(value: unknown, status = 200): Response {
  return Response.json(value, { status });
}

function internalRequest(path: string, source?: Request, init: RequestInit = {}): Request {
  const next: RequestInit & { duplex?: "half" } = {
    method: init.method ?? source?.method ?? "GET",
  };
  if (init.headers !== undefined) next.headers = init.headers;
  const body = init.body ?? source?.body;
  if (body !== undefined && body !== null) {
    next.body = body;
    if (body instanceof ReadableStream) next.duplex = "half";
  }
  return new Request(`http://crabbox.internal${path}`, next);
}

function cleanSandboxID(value: string): string {
  const trimmed = value.trim();
  if (!/^[A-Za-z0-9_.:-]{1,128}$/.test(trimmed)) return "";
  return trimmed;
}

function cleanInstanceType(value: string): InstanceType | "" {
  const trimmed = value.trim().toLowerCase();
  for (const instanceType of instanceTypes) {
    if (trimmed === instanceType) return instanceType;
  }
  return "";
}

function cleanAbsolutePath(value: string): string {
  const trimmed = value.trim();
  if (!trimmed.startsWith("/") || trimmed.includes("\0")) return "";
  const parts: string[] = [];
  for (const part of trimmed.split("/")) {
    if (part === "" || part === ".") continue;
    if (part === "..") {
      if (parts.length === 0) return "";
      parts.pop();
      continue;
    }
    parts.push(part);
  }
  return `/${parts.join("/")}`;
}

function sanitizeLabels(value: unknown): Record<string, string> {
  if (!isRecord(value)) return {};
  const out: Record<string, string> = {};
  for (const [key, raw] of Object.entries(value)) {
    if (typeof raw === "string" && /^[A-Za-z0-9_.:-]{1,64}$/.test(key)) {
      out[key] = raw.slice(0, 256);
    }
  }
  return out;
}

function sanitizeEnv(value: unknown): Record<string, string> | undefined {
  if (!isRecord(value)) return undefined;
  const out: Record<string, string> = {};
  for (const [key, raw] of Object.entries(value)) {
    if (typeof raw === "string" && /^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
      out[key] = raw;
    }
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

function stringField(value: Record<string, unknown>, key: string): string | undefined {
  const field = value[key];
  return typeof field === "string" ? field : undefined;
}

function numberField(value: Record<string, unknown>, key: string): number | undefined {
  const field = value[key];
  return typeof field === "number" ? field : undefined;
}

function positiveIntegerField(value: Record<string, unknown>, key: string): number | undefined {
  const field = numberField(value, key);
  return field !== undefined && Number.isInteger(field) && field > 0 ? field : undefined;
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

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function leaseExpiresAtMs(meta: LeaseMetadata): number | undefined {
  const createdAt = Date.parse(meta.createdAt);
  const lastTouchedAt = Date.parse(meta.lastTouchedAt);
  const candidates: number[] = [];
  if (Number.isFinite(createdAt) && meta.ttlSeconds !== undefined) {
    candidates.push(createdAt + meta.ttlSeconds * 1000);
  }
  if (
    (meta.activeExecutions ?? 0) === 0 &&
    Number.isFinite(lastTouchedAt) &&
    meta.idleTimeoutSeconds !== undefined
  ) {
    candidates.push(lastTouchedAt + meta.idleTimeoutSeconds * 1000);
  }
  return candidates.length > 0 ? Math.min(...candidates) : undefined;
}

function leaseResponse(meta: LeaseMetadata, containerState?: string): Record<string, unknown> {
  return {
    id: meta.id,
    state: meta.state === "running" ? (containerState ?? "running") : meta.state,
    workdir: meta.workdir,
    instanceType: meta.instanceType,
    labels: meta.labels,
    createdAt: meta.createdAt,
    lastTouchedAt: meta.lastTouchedAt,
    ttlSeconds: meta.ttlSeconds,
    idleTimeoutSeconds: meta.idleTimeoutSeconds,
    expiresAt: isoTime(leaseExpiresAtMs(meta)),
    expiredAt: meta.expiredAt,
    stoppedAt: meta.stoppedAt,
  };
}

function expiredResponse(meta: LeaseMetadata): Response {
  return json({ error: "sandbox expired", ...leaseResponse(meta) }, 410);
}

function emptyLeaseMeta(state: LeaseState = "stopped"): LeaseMetadata {
  const now = new Date().toISOString();
  return {
    id: "",
    state,
    workdir: "/workspace",
    instanceType: defaultInstanceType,
    labels: {},
    createdAt: now,
    lastTouchedAt: now,
  };
}

function isoTime(value: number | undefined): string | undefined {
  return value === undefined ? undefined : new Date(value).toISOString();
}
