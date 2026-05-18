import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

class MemoryStorage {
  readonly values = new Map<string, unknown>();

  async get<T>(key: string): Promise<T | undefined> {
    return this.values.get(key) as T | undefined;
  }

  async put<T>(key: string, value: T): Promise<void> {
    this.values.set(key, value);
  }
}

class MockContainer {
  readonly ctx: {
    storage: MemoryStorage;
    waitUntil: (promise: Promise<unknown>) => void;
    blockConcurrencyWhile: <T>(callback: () => Promise<T>) => Promise<T>;
  };
  readonly schedules: Array<{ when: Date | number; callback: string }> = [];
  deletedSchedules: string[] = [];
  destroyed = false;
  stopped = false;
  started = false;
  failStart = false;
  renewedActivityTimeouts = 0;
  execResponse: Response | undefined;
  fileResponse: Response | Promise<Response> | undefined;
  private concurrencyQueue: Promise<void> = Promise.resolve();

  constructor(ctx: {
    storage: MemoryStorage;
    waitUntil?: (promise: Promise<unknown>) => void;
    blockConcurrencyWhile?: <T>(callback: () => Promise<T>) => Promise<T>;
  }) {
    this.ctx = {
      ...ctx,
      waitUntil: ctx.waitUntil ?? ((promise) => void promise),
      blockConcurrencyWhile:
        ctx.blockConcurrencyWhile ??
        (async (callback) => {
          const run = (async () => {
            try {
              await this.concurrencyQueue;
            } catch {
              // Keep the mock queue moving after failed callbacks, matching DO scheduling.
            }
            return callback();
          })();
          this.concurrencyQueue = run.then(
            () => undefined,
            () => undefined,
          );
          return run;
        }),
    };
  }

  async startAndWaitForPorts(): Promise<void> {
    if (this.failStart) {
      throw new Error("port wait failed");
    }
    this.started = true;
  }

  async containerFetch(input: string | URL): Promise<Response> {
    const url = new URL(String(input));
    if (url.pathname === "/v1/exec") {
      if (this.execResponse) return this.execResponse;
      return new Response('{"type":"complete","exitCode":0}\n', {
        headers: { "Content-Type": "application/x-ndjson" },
      });
    }
    if (url.pathname === "/v1/files") {
      if (this.fileResponse) return this.fileResponse;
      return Response.json({ ok: true, path: url.searchParams.get("path") });
    }
    return Response.json({ ok: true });
  }

  async getState(): Promise<{ status: string }> {
    return { status: "running" };
  }

  async destroy(): Promise<void> {
    this.destroyed = true;
  }

  async stop(): Promise<void> {
    this.stopped = true;
  }

  renewActivityTimeout(): void {
    this.renewedActivityTimeouts += 1;
  }

  deleteSchedules(name: string): void {
    this.deletedSchedules.push(name);
  }

  async schedule(when: Date | number, callback: string): Promise<unknown> {
    this.schedules.push({ when, callback });
    return {
      taskId: "task_1",
      callback,
      time: when instanceof Date ? when.getTime() / 1000 : when,
    };
  }

  async fetch(): Promise<Response> {
    return Response.json({ ok: true });
  }
}

vi.mock("@cloudflare/containers", () => ({
  Container: MockContainer,
  getContainer: (namespace: { get(id: string): unknown }, id: string) => namespace.get(id),
}));

const { default: worker, Sandbox } = await import("../src/cloudflare-container-runner");

type CapturedInternalRequest = {
  binding?: string;
  id?: string;
  request?: Request;
};

function envWithCapturedInternalRequest(
  capture: CapturedInternalRequest,
): Parameters<typeof worker.fetch>[1] {
  const binding = (name: string) => ({
    get(id: string) {
      capture.binding = name;
      capture.id = id;
      return {
        async fetch(request: Request): Promise<Response> {
          capture.request = request;
          return Response.json({ ok: true });
        },
      };
    },
  });
  return {
    CRABBOX_RUNNER_TOKEN: "runner-token",
    Sandbox: binding("Sandbox"),
    SandboxLite: binding("SandboxLite"),
    SandboxBasic: binding("SandboxBasic"),
    SandboxStandard1: binding("SandboxStandard1"),
    SandboxStandard2: binding("SandboxStandard2"),
    SandboxStandard3: binding("SandboxStandard3"),
  } as Parameters<typeof worker.fetch>[1];
}

function crabboxRequest(path: string, body?: Record<string, unknown>): Request {
  return new Request(`http://crabbox.internal${path}`, {
    method: body === undefined ? "GET" : "POST",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

function createLease(
  sandbox: InstanceType<typeof Sandbox>,
  body: Record<string, unknown> = {},
): Promise<Response> {
  return sandbox.fetch(
    crabboxRequest("/__crabbox/create", { id: "cbx_test", workdir: "/workspace/repo", ...body }),
  );
}

function execLease(
  sandbox: InstanceType<typeof Sandbox>,
  body: Record<string, unknown> = {},
): Promise<Response> {
  return sandbox.fetch(
    crabboxRequest("/__crabbox/exec-stream", {
      command: "echo hi",
      cwd: "/workspace/repo",
      ...body,
    }),
  );
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((next) => {
    resolve = next;
  });
  return { promise, resolve };
}

function uploadLease(sandbox: InstanceType<typeof Sandbox>): Promise<Response> {
  return sandbox.fetch(
    new Request("http://crabbox.internal/__crabbox/files?path=/workspace/repo/archive.tgz", {
      method: "POST",
      body: "payload",
    }),
  );
}

describe("Cloudflare runner lifecycle", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-05-13T18:00:00Z"));
  });

  afterEach(() => {
    vi.clearAllTimers();
    vi.useRealTimers();
  });

  it("exposes authenticated runner readiness without touching containers", async () => {
    const capture: CapturedInternalRequest = {};
    const response = await worker.fetch(
      new Request("https://runner.example/v1/readiness", {
        headers: {
          Authorization: "Bearer runner-token",
        },
      }),
      envWithCapturedInternalRequest(capture),
    );

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toEqual({
      ok: true,
      runner: "cloudflare",
      instanceTypes: ["lite", "basic", "standard-1", "standard-2", "standard-3", "standard-4"],
    });
    expect(capture.binding).toBeUndefined();
    expect(capture.request).toBeUndefined();
  });

  it("reports missing container bindings in readiness without touching containers", async () => {
    const capture: CapturedInternalRequest = {};
    const env = envWithCapturedInternalRequest(capture) as Record<string, unknown>;
    delete env.SandboxStandard2;
    const response = await worker.fetch(
      new Request("https://runner.example/v1/readiness", {
        headers: {
          Authorization: "Bearer runner-token",
        },
      }),
      env as Parameters<typeof worker.fetch>[1],
    );

    expect(response.status).toBe(503);
    await expect(response.json()).resolves.toEqual({
      ok: false,
      runner: "cloudflare",
      error: "missing container bindings: SandboxStandard2",
      missing: ["SandboxStandard2"],
    });
    expect(capture.binding).toBeUndefined();
    expect(capture.request).toBeUndefined();
  });

  it("requires runner auth for readiness", async () => {
    const capture: CapturedInternalRequest = {};
    const response = await worker.fetch(
      new Request("https://runner.example/v1/readiness"),
      envWithCapturedInternalRequest(capture),
    );

    expect(response.status).toBe(401);
    await expect(response.json()).resolves.toEqual({ error: "unauthorized" });
    expect(capture.binding).toBeUndefined();
  });

  it("sanitizes create payload before dispatching to the durable object", async () => {
    const capture: CapturedInternalRequest = {};
    const response = await worker.fetch(
      new Request("https://runner.example/v1/sandboxes", {
        method: "POST",
        headers: {
          Authorization: "Bearer runner-token",
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          id: " cbx_test ",
          workdir: "/workspace/../workspace/repo",
          labels: { repo: "my-app" },
        }),
      }),
      envWithCapturedInternalRequest(capture),
    );

    expect(response.status).toBe(200);
    expect(capture.binding).toBe("Sandbox");
    expect(capture.id).toBe("cbx_test");
    expect(capture.request?.headers.get("Authorization")).toBeNull();
    expect(capture.request?.headers.get("Content-Type")).toBe("application/json");
    if (capture.request === undefined) {
      throw new Error("missing captured request");
    }
    await expect(capture.request.json()).resolves.toMatchObject({
      id: "cbx_test",
      workdir: "/workspace/repo",
      instanceType: "standard-4",
      labels: { repo: "my-app" },
    });
  });

  it("routes requested instance types to the matching durable object binding", async () => {
    const capture: CapturedInternalRequest = {};
    const response = await worker.fetch(
      new Request("https://runner.example/v1/sandboxes", {
        method: "POST",
        headers: {
          Authorization: "Bearer runner-token",
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          id: "cbx_test",
          workdir: "/workspace/repo",
          instanceType: "standard-2",
        }),
      }),
      envWithCapturedInternalRequest(capture),
    );

    expect(response.status).toBe(200);
    expect(capture.binding).toBe("SandboxStandard2");
    if (capture.request === undefined) {
      throw new Error("missing captured request");
    }
    await expect(capture.request.json()).resolves.toMatchObject({
      instanceType: "standard-2",
    });
  });

  it("does not forward edge auth headers to durable object proxy requests", async () => {
    const capture: CapturedInternalRequest = {};
    const response = await worker.fetch(
      new Request(
        "https://runner.example/v1/sandboxes/cbx_test/exec-stream?instanceType=standard-4",
        {
          method: "POST",
          headers: {
            Authorization: "Bearer runner-token",
            "Content-Type": "application/json",
          },
          body: JSON.stringify({ command: "echo hi", cwd: "/workspace/repo" }),
        },
      ),
      envWithCapturedInternalRequest(capture),
    );

    expect(response.status).toBe(200);
    expect(capture.id).toBe("cbx_test");
    expect(capture.request?.headers.get("Authorization")).toBeNull();
    expect(capture.request?.headers.get("Content-Type")).toBeNull();
  });

  it("returns a controlled 400 response for invalid create JSON", async () => {
    const capture: CapturedInternalRequest = {};
    const response = await worker.fetch(
      new Request("https://runner.example/v1/sandboxes", {
        method: "POST",
        headers: {
          Authorization: "Bearer runner-token",
          "Content-Type": "application/json",
        },
        body: "{",
      }),
      envWithCapturedInternalRequest(capture),
    );

    expect(response.status).toBe(400);
    expect(capture.id).toBeUndefined();
    await expect(response.json()).resolves.toEqual({ error: "invalid json" });
  });

  it("returns a controlled 400 response for invalid durable object JSON", async () => {
    const sandbox = new Sandbox({ storage: new MemoryStorage() });

    const response = await sandbox.fetch(
      new Request("http://crabbox.internal/__crabbox/exec-stream", {
        method: "POST",
        body: "{",
      }),
    );

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toEqual({ error: "invalid json" });
  });

  it("stores lease metadata and schedules cleanup at the idle deadline", async () => {
    const sandbox = new Sandbox({ storage: new MemoryStorage() });

    const response = await createLease(sandbox, {
      ttlSeconds: 3600,
      idleTimeoutSeconds: 600,
      labels: { repo: "my-app" },
    });

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      id: "cbx_test",
      state: "running",
      expiresAt: "2026-05-13T18:10:00.000Z",
      idleTimeoutSeconds: 600,
      ttlSeconds: 3600,
    });
    expect(sandbox.started).toBe(true);
    expect(sandbox.schedules).toHaveLength(1);
    expect(sandbox.schedules[0]?.when).toEqual(new Date("2026-05-13T18:10:00Z"));
  });

  it("touches the lease after streamed command completion", async () => {
    const storage = new MemoryStorage();
    const sandbox = new Sandbox({ storage });
    await createLease(sandbox, { idleTimeoutSeconds: 600 });

    vi.setSystemTime(new Date("2026-05-13T18:05:00Z"));
    const response = await execLease(sandbox);
    await response.text();
    await vi.runAllTimersAsync();

    const status = await sandbox.fetch(crabboxRequest("/__crabbox/status"));
    await expect(status.json()).resolves.toMatchObject({
      state: "running",
      lastTouchedAt: "2026-05-13T18:05:00.000Z",
      expiresAt: "2026-05-13T18:15:00.000Z",
    });
  });

  it("does not expire a lease while a command stream is active", async () => {
    const storage = new MemoryStorage();
    const sandbox = new Sandbox({ storage });
    await createLease(sandbox, { idleTimeoutSeconds: 10 });

    let controller: ReadableStreamDefaultController<Uint8Array> | undefined;
    sandbox.execResponse = new Response(
      new ReadableStream<Uint8Array>({
        start(nextController) {
          controller = nextController;
          nextController.enqueue(
            new TextEncoder().encode('{"type":"stdout","data":"started\\n"}\n'),
          );
        },
      }),
      { headers: { "Content-Type": "application/x-ndjson" } },
    );

    const response = await execLease(sandbox, { command: "sleep 30" });

    vi.setSystemTime(new Date("2026-05-13T18:00:11Z"));
    await sandbox.expireIfIdle();
    expect(sandbox.destroyed).toBe(false);

    controller?.close();
    await response.text();
    await vi.runAllTimersAsync();

    const status = await sandbox.fetch(crabboxRequest("/__crabbox/status"));
    await expect(status.json()).resolves.toMatchObject({
      state: "running",
      lastTouchedAt: "2026-05-13T18:00:11.000Z",
      expiresAt: "2026-05-13T18:00:21.000Z",
    });
  });

  it("does not expire a lease while a file upload is active", async () => {
    const storage = new MemoryStorage();
    const sandbox = new Sandbox({ storage });
    await createLease(sandbox, { idleTimeoutSeconds: 10 });

    const uploadResponse = deferred<Response>();
    sandbox.fileResponse = uploadResponse.promise;

    const upload = uploadLease(sandbox);
    await vi.waitFor(async () => {
      const meta = await storage.get<{ activeExecutions?: number }>("crabbox:lease");
      expect(meta?.activeExecutions).toBe(1);
    });

    vi.setSystemTime(new Date("2026-05-13T18:00:11Z"));
    await sandbox.expireIfIdle();
    expect(sandbox.destroyed).toBe(false);

    uploadResponse.resolve(Response.json({ ok: true }));
    const response = await upload;
    expect(response.status).toBe(200);

    const status = await sandbox.fetch(crabboxRequest("/__crabbox/status"));
    await expect(status.json()).resolves.toMatchObject({
      state: "running",
      lastTouchedAt: "2026-05-13T18:00:11.000Z",
      expiresAt: "2026-05-13T18:00:21.000Z",
    });
  });

  it("serializes active execution accounting for concurrent requests", async () => {
    const storage = new MemoryStorage();
    const sandbox = new Sandbox({ storage });
    await createLease(sandbox, { idleTimeoutSeconds: 10 });

    const uploadResponse = deferred<Response>();
    sandbox.fileResponse = uploadResponse.promise;

    let controller: ReadableStreamDefaultController<Uint8Array> | undefined;
    sandbox.execResponse = new Response(
      new ReadableStream<Uint8Array>({
        start(nextController) {
          controller = nextController;
          nextController.enqueue(
            new TextEncoder().encode('{"type":"stdout","data":"started\\n"}\n'),
          );
        },
      }),
      { headers: { "Content-Type": "application/x-ndjson" } },
    );

    const upload = uploadLease(sandbox);
    const exec = execLease(sandbox, { command: "sleep 30" });
    await vi.waitFor(async () => {
      const meta = await storage.get<{ activeExecutions?: number }>("crabbox:lease");
      expect(meta?.activeExecutions).toBe(2);
    });

    vi.setSystemTime(new Date("2026-05-13T18:00:11Z"));
    await sandbox.expireIfIdle();
    expect(sandbox.destroyed).toBe(false);

    uploadResponse.resolve(Response.json({ ok: true }));
    expect((await upload).status).toBe(200);
    controller?.close();
    await (await exec).text();
    await vi.runAllTimersAsync();

    const meta = await storage.get<{ activeExecutions?: number }>("crabbox:lease");
    expect(meta?.activeExecutions).toBeUndefined();
  });

  it("clears active execution state when the container stream fails", async () => {
    const storage = new MemoryStorage();
    const sandbox = new Sandbox({ storage });
    await createLease(sandbox, { idleTimeoutSeconds: 600 });

    sandbox.execResponse = new Response(
      new ReadableStream<Uint8Array>({
        pull() {
          throw new Error("stream read failed");
        },
      }),
      { headers: { "Content-Type": "application/x-ndjson" } },
    );

    const response = await execLease(sandbox);
    await expect(response.text()).rejects.toThrow("stream read failed");
    await vi.runAllTimersAsync();

    const meta = await storage.get<{ activeExecutions?: number }>("crabbox:lease");
    expect(meta?.activeExecutions).toBeUndefined();
  });

  it("clears active execution state when startup fails", async () => {
    const storage = new MemoryStorage();
    const sandbox = new Sandbox({ storage });
    await createLease(sandbox, { idleTimeoutSeconds: 600 });
    sandbox.failStart = true;

    await expect(execLease(sandbox)).rejects.toThrow("port wait failed");
    await vi.runAllTimersAsync();

    const meta = await storage.get<{ activeExecutions?: number }>("crabbox:lease");
    expect(meta?.activeExecutions).toBeUndefined();
  });

  it("marks create startup failures stopped and destroys the container", async () => {
    const storage = new MemoryStorage();
    const sandbox = new Sandbox({ storage });
    sandbox.failStart = true;

    await expect(createLease(sandbox, { idleTimeoutSeconds: 600 })).rejects.toThrow(
      "port wait failed",
    );

    const meta = await storage.get<{ state?: string; stoppedAt?: string }>("crabbox:lease");
    expect(meta).toMatchObject({ state: "stopped" });
    expect(meta?.stoppedAt).toBeDefined();
    expect(sandbox.deletedSchedules).toContain("expireIfIdle");
    expect(sandbox.destroyed).toBe(true);
  });

  it("expires and destroys the container after the deadline", async () => {
    const sandbox = new Sandbox({ storage: new MemoryStorage() });
    await createLease(sandbox, { idleTimeoutSeconds: 10 });

    vi.setSystemTime(new Date("2026-05-13T18:00:11Z"));
    await sandbox.expireIfIdle();

    expect(sandbox.destroyed).toBe(true);
    const response = await execLease(sandbox);
    expect(response.status).toBe(410);
    await expect(response.json()).resolves.toMatchObject({
      error: "sandbox expired",
      state: "expired",
    });
  });

  it("keeps the container awake when platform activity expires before the lease", async () => {
    const sandbox = new Sandbox({ storage: new MemoryStorage() });
    await createLease(sandbox, { idleTimeoutSeconds: 3600 });

    vi.setSystemTime(new Date("2026-05-13T18:31:00Z"));
    await sandbox.onActivityExpired();

    expect(sandbox.destroyed).toBe(false);
    expect(sandbox.stopped).toBe(false);
    expect(sandbox.renewedActivityTimeouts).toBe(1);
    const status = await sandbox.fetch(crabboxRequest("/__crabbox/status"));
    await expect(status.json()).resolves.toMatchObject({
      state: "running",
      expiresAt: "2026-05-13T19:00:00.000Z",
    });
  });
});
