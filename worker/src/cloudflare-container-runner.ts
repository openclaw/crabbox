import { DurableObject } from "cloudflare:workers";

import { authorize, isRecord, json, stringField } from "./runner-http";

const leaseMetaKey = "crabbox:lease";
const defaultInstanceType = "standard-4";
const defaultImage = "default";
const instanceTypes = ["standard-1", "standard-2", "standard-3", "standard-4"] as const;

// A cache miss on a new image took about 2.5 minutes to pull live.
const readyTimeoutMs = 300_000;
const readyPollMs = 250;
// A probe can stay pending while a container is still starting.
const readyProbeTimeoutMs = 10_000;
// Platform maximum; lease TTL and idle expiry are enforced by the Durable Object
// alarm. A Durable Object restart resets the timeout, so the alarm renews it at
// least every keepAliveIntervalMs while the lease runs.
const containerInactivityTimeoutMs = 6 * 60 * 60 * 1000;
const keepAliveIntervalMs = 60 * 60 * 1000;
const heartbeatIntervalMs = 15_000;
const timeoutKillAfter = "5s";

// A background descendant (`sleep 30 & echo done`) can keep stdout/stderr open
// after the command exits. Once the exit code is known, keep reading while
// output keeps arriving, stop after drainIdleMs without new bytes, and never
// wait longer than drainGraceMs in total.
const drainIdleMs = 300;
const drainPollMs = 100;
const drainGraceMs = 5_000;

type Env = {
  CrabboxSandbox: DurableObjectNamespace<CrabboxSandbox>;
  CRABBOX_RUNNER_TOKEN?: string;
};

type InstanceType = (typeof instanceTypes)[number];

type LeaseState = "running" | "expired" | "stopped";

type LeaseMetadata = {
  id: string;
  state: LeaseState;
  workdir: string;
  instanceType: string;
  image: string;
  labels: Record<string, string>;
  createdAt: string;
  lastTouchedAt: string;
  ttlSeconds?: number;
  idleTimeoutSeconds?: number;
  activeExecutions?: number;
  containerStarted?: boolean;
  expiredAt?: string;
  stoppedAt?: string;
  stopReason?: string;
};

type ExecRequest = {
  command: string;
  cwd: string;
  env: Record<string, string> | undefined;
  timeoutMs: number | undefined;
};

export class CrabboxSandbox extends DurableObject<Env> {
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
    return json({ error: "not found" }, 404);
  }

  override async alarm(): Promise<void> {
    await this.expireIfIdle();
  }

  async expireIfIdle(): Promise<void> {
    const meta = await this.leaseMeta();
    if (!meta || meta.state !== "running") return;

    const now = Date.now();
    const expiresAt = leaseExpiresAtMs(meta);
    if (expiresAt === undefined || expiresAt > now) {
      const container = this.container();
      if (container.running) {
        await container.setInactivityTimeout(containerInactivityTimeoutMs);
      } else if (meta.containerStarted) {
        await this.markWorkspaceLost(meta);
        return;
      }
      await this.scheduleCleanup(meta);
      return;
    }

    const expired: LeaseMetadata = {
      ...meta,
      state: "expired",
      expiredAt: new Date(now).toISOString(),
    };
    await this.ctx.storage.put(leaseMetaKey, expired);
    await this.ctx.storage.deleteAlarm();
    await this.destroyContainer();
  }

  private async createLease(request: Request): Promise<Response> {
    const body = await readObject(request);
    if (body instanceof Response) return body;
    const id = cleanSandboxID(stringField(body, "id") ?? stringField(body, "leaseId") ?? "");
    if (!id) return json({ error: "id is required" }, 400);

    const workdir = cleanAbsolutePath(stringField(body, "workdir") ?? "/workspace/crabbox");
    if (!workdir) return json({ error: "workdir must be an absolute path" }, 400);

    const instanceType = cleanInstanceType(
      stringField(body, "instanceType") ?? defaultInstanceType,
    );
    if (!instanceType) return instanceTypeError(stringField(body, "instanceType"));

    const image = cleanImageName(stringField(body, "image") ?? defaultImage);
    if (!image) return json({ error: "image must be a configured image name" }, 400);
    const container = this.container();
    if (!(image in container.images)) {
      return json(
        { error: `image ${image} is not configured`, images: imageNames(container) },
        400,
      );
    }

    const now = new Date();
    const existing = await this.leaseMeta();
    const ttlSeconds = positiveIntegerField(body, "ttlSeconds");
    const idleTimeoutSeconds = positiveIntegerField(body, "idleTimeoutSeconds");
    const meta: LeaseMetadata = {
      id,
      state: "running",
      workdir,
      instanceType,
      image,
      labels: sanitizeLabels(body["labels"]),
      createdAt: existing?.createdAt ?? now.toISOString(),
      lastTouchedAt: now.toISOString(),
    };
    if (ttlSeconds !== undefined) meta.ttlSeconds = ttlSeconds;
    if (idleTimeoutSeconds !== undefined) meta.idleTimeoutSeconds = idleTimeoutSeconds;
    await this.ctx.storage.put(leaseMetaKey, meta);
    await this.scheduleCleanup(meta);
    try {
      await this.ensureRunning(meta);
    } catch (error) {
      const stopped: LeaseMetadata = {
        ...meta,
        state: "stopped",
        stoppedAt: new Date().toISOString(),
      };
      await this.ctx.storage.put(leaseMetaKey, stopped);
      await this.ctx.storage.deleteAlarm();
      await this.destroyContainer();
      return json({ error: errorMessage(error), ...leaseResponse(stopped) }, 503);
    }
    const started: LeaseMetadata = { ...meta, containerStarted: true };
    await this.ctx.storage.put(leaseMetaKey, started);

    return json(leaseResponse(started, "running"));
  }

  private async leaseStatus(): Promise<Response> {
    const meta = await this.leaseMeta();
    if (!meta) return json({ error: "not found" }, 404);

    const expired = await this.expireIfNeeded(meta);
    if (expired.state !== "running") {
      return json(leaseResponse(expired));
    }
    const container = this.container();
    if (!container.running && expired.containerStarted) {
      return json(leaseResponse(await this.markWorkspaceLost(expired)));
    }
    return json(leaseResponse(expired, container.running ? "running" : "stopped"));
  }

  private async destroyLease(): Promise<Response> {
    const meta = await this.leaseMeta();
    const stopped: LeaseMetadata = {
      ...(meta ?? emptyLeaseMeta()),
      state: "stopped",
      stoppedAt: new Date().toISOString(),
    };
    await this.ctx.storage.put(leaseMetaKey, stopped);
    await this.ctx.storage.deleteAlarm();
    await this.destroyContainer();
    return json(leaseResponse(stopped));
  }

  private async uploadLeaseFile(request: Request, url: URL): Promise<Response> {
    const remotePath = cleanAbsolutePath(url.searchParams.get("path") ?? "");
    if (!remotePath) return json({ error: "path must be absolute" }, 400);
    if (!request.body) return json({ error: "request body is required" }, 400);

    const meta = await this.beginExecution();
    if (meta.state !== "running") return expiredResponse(meta);

    try {
      const container = await this.ensureRunning(meta);
      const process = await container.exec(
        ["/bin/sh", "-c", 'mkdir -p -- "$(dirname -- "$1")" && cat > "$1"', "sh", remotePath],
        { stdin: "pipe", stdout: "ignore", stderr: "pipe" },
      );
      const [written, exitCode, stderr] = await Promise.all([
        copyToStdin(request.body, process.stdin),
        process.exitCode,
        readText(process.stderr),
      ]);
      if (written instanceof Error) {
        return json({ error: `write ${remotePath}: ${written.message}` }, 500);
      }
      if (exitCode !== 0) {
        return json({ error: `write ${remotePath}: ${stderr.trim() || `exit ${exitCode}`}` }, 500);
      }
      return json({ ok: true, path: remotePath });
    } catch (error) {
      return this.startFailureResponse(meta, error);
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

    const timeoutMs = numberField(body, "timeoutMs");
    if (timeoutMs !== undefined && (!Number.isFinite(timeoutMs) || timeoutMs < 0)) {
      return json({ error: "timeoutMs must be a non-negative number" }, 400);
    }

    const meta = await this.beginExecution();
    if (meta.state !== "running") return expiredResponse(meta);

    let container: Container;
    try {
      container = await this.ensureRunning(meta);
    } catch (error) {
      await this.finishExecution();
      return this.startFailureResponse(meta, error);
    }
    const stream = execEventStream(
      container,
      { command, cwd, env: sanitizeEnv(body["env"]), timeoutMs },
      request.signal,
      () => this.ctx.waitUntil(this.finishExecution()),
    );
    return new Response(stream, {
      headers: { "Content-Type": "application/x-ndjson", "Cache-Control": "no-store" },
    });
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
    await this.ctx.storage.deleteAlarm();
    await this.destroyContainer();
    return expired;
  }

  private async leaseMeta(): Promise<LeaseMetadata | undefined> {
    const meta = await this.ctx.storage.get<LeaseMetadata>(leaseMetaKey);
    if (meta && meta.image === undefined) return { ...meta, image: defaultImage };
    return meta;
  }

  private async scheduleCleanup(meta: LeaseMetadata): Promise<void> {
    if (meta.state !== "running") {
      await this.ctx.storage.deleteAlarm();
      return;
    }
    const keepAlive = Date.now() + keepAliveIntervalMs;
    await this.ctx.storage.setAlarm(Math.min(leaseExpiresAtMs(meta) ?? keepAlive, keepAlive));
  }

  // A stopped container lost its filesystem; restarting it would hand the next
  // command an empty workspace, so the lease ends instead.
  private async markWorkspaceLost(meta: LeaseMetadata): Promise<LeaseMetadata> {
    const stopped: LeaseMetadata = {
      ...meta,
      state: "stopped",
      stoppedAt: new Date().toISOString(),
      stopReason: "container stopped; its workspace is gone",
    };
    delete stopped.activeExecutions;
    await this.ctx.storage.put(leaseMetaKey, stopped);
    await this.ctx.storage.deleteAlarm();
    return stopped;
  }

  private async startFailureResponse(meta: LeaseMetadata, error: unknown): Promise<Response> {
    if (error instanceof WorkspaceLostError) {
      const stopped = await this.markWorkspaceLost(meta);
      return json({ error: error.message, ...leaseResponse(stopped) }, 410);
    }
    return json({ error: errorMessage(error) }, 503);
  }

  private container(): Container {
    const container = this.ctx.container;
    if (!container) {
      throw new Error("Durable Object has no container; check the containers config");
    }
    return container;
  }

  private async ensureRunning(meta: LeaseMetadata): Promise<Container> {
    const container = this.container();
    if (!container.running && meta.containerStarted) {
      throw new WorkspaceLostError();
    }
    if (!container.running) {
      const image = container.images[meta.image];
      if (!image) throw new Error(`image ${meta.image} is not configured`);
      const instance = cleanInstanceType(meta.instanceType) || defaultInstanceType;
      container.start({ image, instance, enableInternet: true });
      const exit: { error?: Error } = {};
      void (async () => {
        try {
          await container.monitor();
          exit.error = new Error("container exited during startup");
        } catch (error) {
          exit.error = new Error(`container failed to start: ${errorMessage(error)}`);
        }
      })();
      await waitForExec(container, () => exit.error);
    } else {
      await waitForExec(container, () => undefined);
    }
    await container.setInactivityTimeout(containerInactivityTimeoutMs);
    return container;
  }

  private async destroyContainer(): Promise<void> {
    const container = this.container();
    if (container.running) await container.destroy();
  }
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);

    if (url.pathname === "/health") {
      return json({ ok: true, runner: "cloudflare" });
    }

    const auth = authorize(request, env.CRABBOX_RUNNER_TOKEN);
    if (auth) return auth;

    if (url.pathname === "/v1/readiness" && request.method === "GET") return runnerReadiness(env);

    if (url.pathname === "/v1/sandboxes" && request.method === "POST") {
      return createSandbox(request, env);
    }

    const match = url.pathname.match(/^\/v1\/sandboxes\/([^/]+)(?:\/([^/]+))?$/);
    if (!match) return json({ error: "not found" }, 404);

    const sandboxID = cleanSandboxID(decodeURIComponent(match[1] ?? ""));
    if (!sandboxID) return json({ error: "id is required" }, 400);
    const action = match[2] ?? "";

    if (request.method === "GET" && action === "") {
      return sandboxStub(env, sandboxID).fetch(internalRequest("/__crabbox/status"));
    }
    if (request.method === "DELETE" && action === "") {
      return withExistingLease(env, sandboxID, (stub) =>
        stub.fetch(internalRequest("/__crabbox/destroy", undefined, { method: "DELETE" })),
      );
    }
    if (request.method === "POST" && action === "files") {
      return withExistingLease(env, sandboxID, (stub) =>
        stub.fetch(internalRequest(`/__crabbox/files${url.search}`, request, { method: "POST" })),
      );
    }
    if (request.method === "POST" && action === "exec-stream") {
      return withExistingLease(env, sandboxID, (stub) =>
        stub.fetch(internalRequest("/__crabbox/exec-stream", request, { method: "POST" })),
      );
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
  if (!instanceType) return instanceTypeError(stringField(body, "instanceType"));

  const sanitizedBody = { ...body, id: sandboxID, workdir, instanceType };
  return sandboxStub(env, sandboxID).fetch(
    internalRequest("/__crabbox/create", undefined, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(sanitizedBody),
    }),
  );
}

async function withExistingLease(
  env: Env,
  sandboxID: string,
  next: (stub: DurableObjectStub<CrabboxSandbox>) => Promise<Response>,
): Promise<Response> {
  const stub = sandboxStub(env, sandboxID);
  const status = await stub.fetch(internalRequest("/__crabbox/status"));
  if (status.status === 404) return status;
  return next(stub);
}

function sandboxStub(env: Env, sandboxID: string): DurableObjectStub<CrabboxSandbox> {
  return env.CrabboxSandbox.get(env.CrabboxSandbox.idFromName(sandboxID));
}

function runnerReadiness(env: Env): Response {
  if ((env.CrabboxSandbox as unknown) === undefined || (env.CrabboxSandbox as unknown) === null) {
    return json(
      {
        ok: false,
        runner: "cloudflare",
        error: "missing container binding: CrabboxSandbox",
        missing: ["CrabboxSandbox"],
      },
      503,
    );
  }
  return json({ ok: true, runner: "cloudflare", instanceTypes });
}

async function waitForExec(
  container: Container,
  startupError: () => Error | undefined,
): Promise<void> {
  const deadline = performance.now() + readyTimeoutMs;
  let lastError: unknown;
  for (;;) {
    try {
      const budget = Math.max(Math.min(readyProbeTimeoutMs, deadline - performance.now()), 1);
      // oxlint-disable-next-line eslint/no-await-in-loop -- readiness is a sequential probe.
      const exitCode = await withTimeout(probeExec(container), budget, "readiness probe");
      if (exitCode === 0) return;
      lastError = new Error("readiness probe exited non-zero");
    } catch (error) {
      lastError = error;
    }
    const failed = startupError();
    if (failed) throw failed;
    if (performance.now() >= deadline) {
      throw new Error(
        `container did not accept exec within ${readyTimeoutMs}ms: ${errorMessage(lastError)}`,
        { cause: lastError },
      );
    }
    // oxlint-disable-next-line eslint/no-await-in-loop -- see above.
    await sleep(readyPollMs);
  }
}

// commandScript deletes itself on first line so aborted commands do not leak scripts.
function commandScript(command: string): string {
  return `rm -f -- "$0"\n${command}\n`;
}

function timeoutArgument(timeoutMs: number | undefined): string {
  if (timeoutMs === undefined || timeoutMs <= 0) return "0";
  return `${Math.max(Math.ceil(timeoutMs / 1000), 1)}s`;
}

function execEventStream(
  container: Container,
  request: ExecRequest,
  signal: AbortSignal,
  onFinish: () => void,
): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  let process: ExecProcess | undefined;
  let heartbeat: ReturnType<typeof setInterval> | undefined;
  let finished = false;
  let canceled = false;
  const finish = () => {
    if (finished) return;
    finished = true;
    if (heartbeat !== undefined) clearInterval(heartbeat);
    onFinish();
  };
  // Cloudflare does not stop an exec when its caller goes away. SIGTERM reaches
  // `timeout`, which signals the whole command process group.
  const cancel = () => {
    canceled = true;
    process?.kill(15);
    finish();
  };
  if (signal.aborted) canceled = true;
  signal.addEventListener("abort", cancel, { once: true });

  return new ReadableStream<Uint8Array>({
    start: (controller) => {
      const emit = (event: Record<string, unknown>) => {
        if (finished) return;
        controller.enqueue(encoder.encode(`${JSON.stringify(event)}\n`));
      };
      emit({ type: "start" });
      heartbeat = setInterval(() => emit({ type: "heartbeat" }), heartbeatIntervalMs);

      void (async () => {
        try {
          const scriptPath = `/tmp/crabbox-command-${crypto.randomUUID()}.sh`;
          const writer = await container.exec(
            [
              "/bin/sh",
              "-c",
              'mkdir -p -- "$2" && umask 077 && cat > "$1"',
              "sh",
              scriptPath,
              request.cwd,
            ],
            { stdin: textStream(commandScript(request.command)), stdout: "ignore", stderr: "pipe" },
          );
          const [writeExit, writeErr] = await Promise.all([
            writer.exitCode,
            readText(writer.stderr),
          ]);
          if (writeExit !== 0) {
            throw new Error(`prepare command: ${writeErr.trim() || `exit ${writeExit}`}`);
          }
          if (canceled) return;

          // GNU timeout puts the command in its own process group and signals the
          // whole group, so descendants die with it; it exits 124 on timeout.
          const argv = [
            "timeout",
            `--kill-after=${timeoutKillAfter}`,
            timeoutArgument(request.timeoutMs),
            "/bin/bash",
            "-l",
            scriptPath,
          ];
          const options: ContainerExecOptions = { cwd: request.cwd };
          if (request.env) options.env = request.env;
          process = await container.exec(argv, options);
          if (canceled) {
            process.kill(15);
            return;
          }
          const exitCode = await pumpOutput(process, emit);
          emit({ type: "complete", exitCode });
          finish();
          controller.close();
        } catch (error) {
          emit({ type: "error", error: errorMessage(error) });
          finish();
          controller.close();
        }
      })();
    },
    cancel,
  });
}

async function pumpOutput(
  process: ExecProcess,
  emit: (event: Record<string, unknown>) => void,
): Promise<number> {
  let lastProgress = performance.now();
  const readers: Array<ReadableStreamDefaultReader<Uint8Array>> = [];
  const pump = async (stream: ReadableStream | null, type: "stdout" | "stderr") => {
    if (!stream) return;
    const reader = (stream as ReadableStream<Uint8Array>).getReader();
    readers.push(reader);
    const decoder = new TextDecoder();
    for (;;) {
      let next: ReadableStreamReadResult<Uint8Array>;
      try {
        // oxlint-disable-next-line eslint/no-await-in-loop -- stream reads are sequential.
        next = await reader.read();
      } catch {
        break;
      }
      if (next.done) break;
      lastProgress = performance.now();
      const data = decoder.decode(next.value, { stream: true });
      if (data) emit({ type, data });
    }
    const tail = decoder.decode();
    if (tail) emit({ type, data: tail });
  };

  const pumps = Promise.all([pump(process.stdout, "stdout"), pump(process.stderr, "stderr")]);
  const exitCode = await process.exitCode;

  const drain = { done: false };
  void (async () => {
    await pumps;
    drain.done = true;
  })();
  const drainDeadline = performance.now() + drainGraceMs;
  lastProgress = Math.max(lastProgress, performance.now());
  while (
    !drain.done &&
    performance.now() < drainDeadline &&
    performance.now() - lastProgress < drainIdleMs
  ) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- polling the drain state.
    await sleep(drainPollMs);
  }
  if (!drain.done) {
    await Promise.all(readers.map((reader) => reader.cancel().catch(() => undefined)));
  }
  await pumps;
  return exitCode;
}

// Passing a forwarded request body as exec `stdin` never resolved the exit code live, and
// closing a piped stdin can reject with "Network connection lost" after the
// process has read everything; the exit code is the source of truth then.
async function copyToStdin(
  source: ReadableStream<Uint8Array>,
  stdin: WritableStream | null,
): Promise<Error | undefined> {
  if (!stdin) {
    await source.cancel();
    return new Error("exec stdin is not available");
  }
  const writer = stdin.getWriter();
  const reader = source.getReader();
  try {
    for (;;) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- stream copy is sequential.
      const next = await reader.read();
      if (next.done) break;
      // oxlint-disable-next-line eslint/no-await-in-loop -- see above.
      await writer.write(next.value);
    }
  } catch (error) {
    await reader.cancel().catch(() => undefined);
    await writer.abort(error).catch(() => undefined);
    return error instanceof Error ? error : new Error(String(error));
  }
  await writer.close().catch(() => undefined);
  return undefined;
}

function textStream(text: string): ReadableStream<Uint8Array> {
  const bytes = new TextEncoder().encode(text);
  return new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(bytes);
      controller.close();
    },
  });
}

async function readText(stream: ReadableStream | null): Promise<string> {
  if (!stream) return "";
  return new Response(stream).text();
}

async function probeExec(container: Container): Promise<number> {
  const probe = await container.exec(["true"], { stdout: "ignore", stderr: "ignore" });
  return probe.exitCode;
}

async function withTimeout<T>(promise: Promise<T>, ms: number, label: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${label} timed out after ${ms}ms`)), ms);
  });
  try {
    return await Promise.race([promise, timeout]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function imageNames(container: Container): string[] {
  return Object.keys(container.images).toSorted();
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

class WorkspaceLostError extends Error {
  constructor() {
    super("container stopped; its workspace is gone");
  }
}

function instanceTypeError(requested: string | undefined): Response {
  if (requested?.trim().toLowerCase() === "lite") {
    return json(
      { error: "instanceType lite cannot start the bundled runner image; use standard-1" },
      400,
    );
  }
  return json({ error: "instanceType is not supported" }, 400);
}

function cleanInstanceType(value: string): InstanceType | "" {
  // The durable_object policy has no basic type; standard-1 is the closest one.
  const trimmed =
    value.trim().toLowerCase() === "basic" ? "standard-1" : value.trim().toLowerCase();
  for (const instanceType of instanceTypes) {
    if (trimmed === instanceType) return instanceType;
  }
  return "";
}

function cleanImageName(value: string): string {
  const trimmed = value.trim();
  return /^[A-Za-z0-9_.-]{1,128}$/.test(trimmed) ? trimmed : "";
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

function numberField(value: Record<string, unknown>, key: string): number | undefined {
  const field = value[key];
  return typeof field === "number" ? field : undefined;
}

function positiveIntegerField(value: Record<string, unknown>, key: string): number | undefined {
  const field = numberField(value, key);
  return field !== undefined && Number.isInteger(field) && field > 0 ? field : undefined;
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
    image: meta.image,
    labels: meta.labels,
    createdAt: meta.createdAt,
    lastTouchedAt: meta.lastTouchedAt,
    ttlSeconds: meta.ttlSeconds,
    idleTimeoutSeconds: meta.idleTimeoutSeconds,
    expiresAt: isoTime(leaseExpiresAtMs(meta)),
    expiredAt: meta.expiredAt,
    stoppedAt: meta.stoppedAt,
    stopReason: meta.stopReason,
  };
}

function expiredResponse(meta: LeaseMetadata): Response {
  return json({ error: meta.stopReason ?? "sandbox expired", ...leaseResponse(meta) }, 410);
}

function emptyLeaseMeta(state: LeaseState = "stopped"): LeaseMetadata {
  const now = new Date().toISOString();
  return {
    id: "",
    state,
    workdir: "/workspace",
    instanceType: defaultInstanceType,
    image: defaultImage,
    labels: {},
    createdAt: now,
    lastTouchedAt: now,
  };
}

function isoTime(value: number | undefined): string | undefined {
  return value === undefined ? undefined : new Date(value).toISOString();
}
