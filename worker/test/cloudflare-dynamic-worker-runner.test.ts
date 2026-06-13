import { readFileSync } from "node:fs";

import { beforeEach, describe, expect, it, vi } from "vitest";

const {
  default: worker,
  DynamicWorkerRunCoordinator,
  HttpGateway,
} = await import("../src/cloudflare-dynamic-worker-runner");

function isRunIndexObject(id: string): boolean {
  return id === "__crabbox/run-index__" || id.startsWith("__crabbox/run-index__/");
}

type WorkerCode = {
  compatibilityDate: string;
  compatibilityFlags?: string[];
  mainModule: string;
  modules: Record<string, unknown>;
  globalOutbound?: unknown;
  env?: Record<string, unknown>;
  tails?: unknown[];
  limits?: {
    cpuMs?: number;
    subRequests?: number;
  };
};

type EntrypointOptions = {
  limits?: {
    cpuMs?: number;
    subRequests?: number;
  };
};

class MockDynamicWorker {
  readonly codeFactory: () => Promise<WorkerCode>;
  readonly fetchStarted: Promise<void>;
  code: WorkerCode | undefined;
  entrypointOptions: EntrypointOptions | undefined;
  request: Request | undefined;
  response = new Response("ok", {
    status: 200,
    headers: { "X-Run": "ok" },
  });
  error: Error | undefined;
  fetchGate: Promise<void> | undefined;
  private resolveFetchStarted!: () => void;

  constructor(codeFactory: () => Promise<WorkerCode>) {
    this.codeFactory = codeFactory;
    this.fetchStarted = new Promise((resolve) => {
      this.resolveFetchStarted = resolve;
    });
  }

  getEntrypoint(
    _name?: string,
    options?: EntrypointOptions,
  ): {
    fetch: (request: Request) => Promise<Response>;
  } {
    this.entrypointOptions = options;
    return {
      fetch: async (request: Request) => {
        this.code = await this.codeFactory();
        this.request = request;
        this.resolveFetchStarted();
        if (this.fetchGate) await this.fetchGate;
        if (this.error) throw this.error;
        return this.response;
      },
    };
  }
}

class MockLoader {
  readonly loadCalls: WorkerCode[] = [];
  readonly getCalls: Array<{ id: string }> = [];
  readonly workers: MockDynamicWorker[] = [];
  readonly workerCreated: Promise<MockDynamicWorker>;
  worker: MockDynamicWorker | undefined;
  nextError: Error | undefined;
  nextResponse: Response | undefined;
  nextFetchGate: Promise<void> | undefined;
  private resolveWorkerCreated!: (worker: MockDynamicWorker) => void;
  private readonly workerWaiters: Array<{ count: number; resolve: () => void }> = [];

  constructor() {
    this.workerCreated = new Promise((resolve) => {
      this.resolveWorkerCreated = resolve;
    });
  }

  load(code: WorkerCode): MockDynamicWorker {
    this.loadCalls.push(code);
    return this.makeWorker(async () => code);
  }

  get(id: string, callback: () => Promise<WorkerCode>): MockDynamicWorker {
    this.getCalls.push({ id });
    return this.makeWorker(callback);
  }

  waitForWorkers(count: number): Promise<void> {
    if (this.workers.length >= count) return Promise.resolve();
    return new Promise((resolve) => {
      this.workerWaiters.push({ count, resolve });
    });
  }

  private makeWorker(callback: () => Promise<WorkerCode>): MockDynamicWorker {
    const dynamicWorker = new MockDynamicWorker(callback);
    if (this.nextResponse) dynamicWorker.response = this.nextResponse;
    if (this.nextError) dynamicWorker.error = this.nextError;
    dynamicWorker.fetchGate = this.nextFetchGate;
    this.worker = dynamicWorker;
    this.workers.push(dynamicWorker);
    for (const waiter of this.workerWaiters) {
      if (this.workers.length >= waiter.count) waiter.resolve();
    }
    this.resolveWorkerCreated(dynamicWorker);
    return dynamicWorker;
  }
}

type TestEnv = Parameters<typeof worker.fetch>[1] & {
  LOADER?: MockLoader;
  RUNS?: MockKVNamespace;
};

class MockKVNamespace {
  readonly values = new Map<string, string>();
  readonly getCalls: Array<string | string[]> = [];
  readonly putCalls: Array<{ key: string; value: string; options?: { expirationTtl?: number } }> =
    [];
  readonly deleteCalls: string[] = [];
  readonly listCalls: Array<{ prefix?: string; cursor?: string }> = [];
  listPageSize = 1000;
  putError?: Error;
  deleteError?: Error;
  bulkGetMaxBytes?: number;
  getGate?: Promise<void>;
  getStarted?: () => void;
  putRateLimitFailures = 0;
  deleteRateLimitFailures = 0;

  async get(key: string): Promise<string | null>;
  async get(keys: string[]): Promise<Map<string, string | null>>;
  async get(keyOrKeys: string | string[]): Promise<string | null | Map<string, string | null>> {
    this.getCalls.push(structuredClone(keyOrKeys));
    this.getStarted?.();
    if (this.getGate) await this.getGate;
    if (Array.isArray(keyOrKeys)) {
      const bytes = keyOrKeys.reduce(
        (total, key) => total + Buffer.byteLength(this.values.get(key) ?? ""),
        0,
      );
      if (this.bulkGetMaxBytes !== undefined && bytes > this.bulkGetMaxBytes) {
        throw new Error("KV GET failed: 413 Request Entity Too Large");
      }
      return new Map(keyOrKeys.map((key) => [key, this.values.get(key) ?? null]));
    }
    return this.values.get(keyOrKeys) ?? null;
  }

  async put(key: string, value: string, options?: { expirationTtl?: number }): Promise<void> {
    this.putCalls.push({ key, value, options });
    if (this.putError) throw this.putError;
    if (this.putRateLimitFailures > 0) {
      this.putRateLimitFailures -= 1;
      throw new Error("KV PUT failed: 429 Too Many Requests");
    }
    this.values.set(key, value);
  }

  async delete(key: string): Promise<void> {
    this.deleteCalls.push(key);
    if (this.deleteError) throw this.deleteError;
    if (this.deleteRateLimitFailures > 0) {
      this.deleteRateLimitFailures -= 1;
      throw new Error("KV DELETE failed: 429 Too Many Requests");
    }
    this.values.delete(key);
  }

  async list(options: { prefix?: string; cursor?: string } = {}): Promise<{
    keys: Array<{ name: string }>;
    list_complete: boolean;
    cursor?: string;
  }> {
    this.listCalls.push(options);
    const matching = [...this.values.keys()].filter(
      (key) => options.prefix === undefined || key.startsWith(options.prefix),
    );
    const offset = options.cursor === undefined ? 0 : Number.parseInt(options.cursor, 10);
    const page = matching.slice(offset, offset + this.listPageSize);
    const nextOffset = offset + page.length;
    const listComplete = nextOffset >= matching.length;
    return {
      keys: page.map((name) => ({ name })),
      list_complete: listComplete,
      ...(listComplete ? {} : { cursor: String(nextOffset) }),
    };
  }
}

class MockDurableObjectStorage {
  readonly values = new Map<string, unknown>();
  readonly getCalls: string[] = [];
  deleteAllCalls = 0;
  alarmTime: number | null = null;

  async get<T>(key: string): Promise<T | undefined> {
    this.getCalls.push(key);
    return this.values.get(key) as T | undefined;
  }

  async put(key: string, value: unknown): Promise<void> {
    this.values.set(key, structuredClone(value));
  }

  async delete(key: string): Promise<boolean> {
    return this.values.delete(key);
  }

  async deleteAll(): Promise<void> {
    this.deleteAllCalls += 1;
    this.values.clear();
  }

  async list<T>(options: { prefix?: string } = {}): Promise<Map<string, T>> {
    const entries = [...this.values.entries()].filter(([key]) =>
      options.prefix === undefined ? true : key.startsWith(options.prefix),
    );
    return new Map(entries) as Map<string, T>;
  }

  async setAlarm(scheduledTime: number | Date): Promise<void> {
    this.alarmTime = scheduledTime instanceof Date ? scheduledTime.getTime() : scheduledTime;
  }

  async getAlarm(): Promise<number | null> {
    return this.alarmTime;
  }

  async deleteAlarm(): Promise<void> {
    this.alarmTime = null;
  }
}

class MockDurableObjectState {
  private locked = false;
  private readonly waiters: Array<() => void> = [];

  constructor(readonly storage: MockDurableObjectStorage) {}

  async blockConcurrencyWhile<T>(callback: () => Promise<T>): Promise<T> {
    await this.acquire();
    try {
      return await callback();
    } finally {
      this.release();
    }
  }

  private acquire(): Promise<void> {
    if (!this.locked) {
      this.locked = true;
      return Promise.resolve();
    }
    return new Promise((resolve) => {
      this.waiters.push(resolve);
    });
  }

  private release(): void {
    const next = this.waiters.shift();
    if (next) {
      next();
    } else {
      this.locked = false;
    }
  }
}

function mockDurableObjectState(storage: MockDurableObjectStorage): DurableObjectState {
  return new MockDurableObjectState(storage) as unknown as DurableObjectState;
}

class MockRunCoordinatorNamespace {
  private readonly objects = new Map<string, DynamicWorkerRunCoordinator>();
  private readonly objectEnv = {} as Parameters<typeof worker.fetch>[1];
  readonly requests: Array<{ id: string; path: string; body?: unknown }> = [];
  completionFailures = 0;
  completionResponseLosses = 0;
  indexDeleteFailures = 0;

  constructor(
    private readonly failRunIndex = false,
    private readonly indexSetGate?: Promise<void>,
  ) {}

  idFromName(name: string): string {
    return name;
  }

  bindEnv(value: Parameters<typeof worker.fetch>[1]): void {
    Object.assign(this.objectEnv, value);
  }

  async alarm(id: string): Promise<void> {
    const object = this.objects.get(id);
    if (!object) throw new Error(`missing coordinator ${id}`);
    await object.alarm();
  }

  get(id: string): { fetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response> } {
    let object = this.objects.get(id);
    if (!object) {
      object = new DynamicWorkerRunCoordinator(
        mockDurableObjectState(new MockDurableObjectStorage()),
        this.objectEnv,
      );
      this.objects.set(id, object);
    }
    return {
      fetch: (input, init) => {
        const request = new Request(input, init);
        let body: unknown;
        if (typeof init?.body === "string") body = JSON.parse(init.body);
        this.requests.push({ id, path: new URL(request.url).pathname, body });
        if (this.failRunIndex && isRunIndexObject(id)) {
          return Promise.resolve(new Response("index unavailable", { status: 503 }));
        }
        if (
          isRunIndexObject(id) &&
          new URL(request.url).pathname === "/index/delete" &&
          this.indexDeleteFailures > 0
        ) {
          this.indexDeleteFailures -= 1;
          return Promise.resolve(new Response("index delete unavailable", { status: 503 }));
        }
        if (
          !isRunIndexObject(id) &&
          new URL(request.url).pathname === "/complete" &&
          this.completionResponseLosses > 0
        ) {
          this.completionResponseLosses -= 1;
          return (async () => {
            await object.fetch(request);
            return new Response("completion response lost", { status: 503 });
          })();
        }
        if (
          !isRunIndexObject(id) &&
          new URL(request.url).pathname === "/complete" &&
          this.completionFailures > 0
        ) {
          this.completionFailures -= 1;
          return Promise.resolve(new Response("completion unavailable", { status: 503 }));
        }
        if (
          this.indexSetGate &&
          isRunIndexObject(id) &&
          new URL(request.url).pathname === "/index/set"
        ) {
          return this.indexSetGate.then(() => object.fetch(request));
        }
        return object.fetch(request);
      },
    };
  }
}

function env(
  loader = new MockLoader(),
  runs?: MockKVNamespace,
  coordinator = new MockRunCoordinatorNamespace(),
): TestEnv {
  const value: TestEnv = {
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN: "runner-token",
    LOADER: loader,
    RUNS: runs,
    RUN_COORDINATOR: coordinator as unknown as DurableObjectNamespace<DynamicWorkerRunCoordinator>,
  };
  coordinator.bindEnv(value);
  return value;
}

function ctx(gateway: unknown = { binding: "gateway" }, tailer: unknown = { binding: "tailer" }) {
  return {
    waitUntil: vi.fn<(promise: Promise<unknown>) => void>((promise) => {
      void promise;
    }),
    exports: {
      HttpGateway: vi.fn<(options: { props: unknown }) => unknown>(() => gateway),
      LogTailer: vi.fn<(options: { props: unknown }) => unknown>(() => tailer),
    },
  };
}

function authedRequest(path: string, init: RequestInit = {}): Request {
  const headers = new Headers(init.headers);
  headers.set("Authorization", "Bearer runner-token");
  if (init.body !== undefined && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  return new Request(`https://runner.example${path}`, { ...init, headers });
}

function runPayload(extra: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "run_1",
    retainMetadata: true,
    mainModule: "index.js",
    modules: {
      "index.js": "export default { fetch() { return new Response('hello'); } };",
    },
    request: {
      method: "POST",
      url: "https://example.test/execute",
      headers: { "X-Test": "1" },
      body: "payload",
    },
    ...extra,
  };
}

describe("Cloudflare Dynamic Workers runner", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-06-12T21:00:00Z"));
  });

  it("exposes authenticated readiness without loading a dynamic worker", async () => {
    const loader = new MockLoader();
    const response = await worker.fetch(authedRequest("/v1/readiness"), env(loader), ctx());

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toEqual({
      ok: true,
      runner: "cloudflare-dynamic-workers",
      loader: true,
      loaderBinding: true,
      coordinatorBinding: true,
      durableRunMetadata: false,
      compatibilityDate: "2026-06-12",
      egress: "blocked",
      defaultEgress: "blocked",
      cacheModes: ["one-shot", "stable", "explicit"],
      tokenSource: "CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN",
    });
    expect(loader.loadCalls).toHaveLength(0);
    expect(loader.getCalls).toHaveLength(0);
  });

  it("rejects malformed percent-encoded run IDs", async () => {
    const response = await worker.fetch(authedRequest("/v1/runs/%"), env(), ctx());

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toEqual({ error: "run id is invalid" });
  });

  it("reports a missing loader binding from readiness", async () => {
    const testEnv = env();
    delete testEnv.LOADER;
    const response = await worker.fetch(authedRequest("/v1/readiness"), testEnv, ctx());

    expect(response.status).toBe(503);
    await expect(response.json()).resolves.toEqual({
      ok: false,
      runner: "cloudflare-dynamic-workers",
      error: "missing required binding: LOADER",
      missing: ["LOADER"],
    });
  });

  it("reports a missing run coordinator binding from readiness", async () => {
    const testEnv = env();
    delete testEnv.RUN_COORDINATOR;
    const response = await worker.fetch(authedRequest("/v1/readiness"), testEnv, ctx());

    expect(response.status).toBe(503);
    await expect(response.json()).resolves.toEqual({
      ok: false,
      runner: "cloudflare-dynamic-workers",
      error: "missing required binding: RUN_COORDINATOR",
      missing: ["RUN_COORDINATOR"],
    });
  });

  it.each([
    [
      "/start",
      {
        executionId: "execution_start",
        expiresAt: Date.parse("2026-06-12T21:15:00Z"),
        legacyReusableID: false,
        record: {
          id: "run_coordination_start",
          workerId: "worker_coordination_start",
          state: "running",
          cacheMode: "one-shot",
          egress: "blocked",
          createdAt: "2026-06-12T21:00:00.000Z",
          startedAt: "2026-06-12T21:00:00.000Z",
          logs: [],
        },
      },
    ],
    [
      "/complete",
      {
        executionId: "execution_complete",
        release: true,
        record: {
          id: "run_coordination_complete",
          workerId: "worker_coordination_complete",
          state: "succeeded",
          cacheMode: "one-shot",
          egress: "blocked",
          createdAt: "2026-06-12T21:00:00.000Z",
          startedAt: "2026-06-12T21:00:00.000Z",
          completedAt: "2026-06-12T21:00:01.000Z",
          durationMs: 1000,
          logs: [],
        },
      },
    ],
    ["/delete", { acknowledgedComplete: false, runId: "run_coordination_delete" }],
  ])("parses coordinator %s requests before reading state", async (path, body) => {
    const storage = new MockDurableObjectStorage();
    const coordinator = new DynamicWorkerRunCoordinator(
      mockDurableObjectState(storage),
      {} as Parameters<typeof worker.fetch>[1],
    );
    let releaseBody!: () => void;
    const bodyGate = new Promise<void>((resolve) => {
      releaseBody = resolve;
    });
    const request = {
      method: "POST",
      url: `https://run-coordinator${path}`,
      json: async () => {
        await bodyGate;
        return body;
      },
    } as unknown as Request;

    const responsePromise = coordinator.fetch(request);
    await Promise.resolve();
    expect(storage.getCalls).toEqual([]);

    releaseBody();
    const response = await responsePromise;
    expect(response.status).toBe(200);
    expect(storage.getCalls).toEqual(["state"]);
  });

  it("serves active and retained terminal records from the coordinator", async () => {
    const storage = new MockDurableObjectStorage();
    const coordinator = new DynamicWorkerRunCoordinator(
      mockDurableObjectState(storage),
      {} as Parameters<typeof worker.fetch>[1],
    );
    const activeRecord = {
      id: "run_coordinated_status",
      workerId: "worker_coordinated_status",
      state: "running",
      cacheMode: "one-shot",
      egress: "blocked",
      createdAt: "2026-06-12T21:00:00.000Z",
      startedAt: "2026-06-12T21:00:00.000Z",
      expiresAt: "2026-06-12T21:15:00.000Z",
      logs: [{ level: "info", message: "run started", time: "2026-06-12T21:00:00.000Z" }],
    };
    const started = await coordinator.fetch(
      new Request("https://run-coordinator/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId: "execution_coordinated_status",
          expiresAt: Date.parse(activeRecord.expiresAt),
          legacyReusableID: false,
          record: activeRecord,
        }),
      }),
    );
    expect(started.status).toBe(200);

    const active = await coordinator.fetch(new Request("https://run-coordinator/record"));
    await expect(active.json()).resolves.toMatchObject({
      known: true,
      record: { id: activeRecord.id, state: "running" },
    });

    const terminalRecord = {
      ...activeRecord,
      state: "succeeded",
      completedAt: "2026-06-12T21:00:01.000Z",
      durationMs: 1000,
    };
    delete terminalRecord.expiresAt;
    const completed = await coordinator.fetch(
      new Request("https://run-coordinator/complete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId: "execution_coordinated_status",
          release: false,
          record: terminalRecord,
        }),
      }),
    );
    expect(completed.status).toBe(200);

    const terminal = await coordinator.fetch(new Request("https://run-coordinator/record"));
    await expect(terminal.json()).resolves.toMatchObject({
      known: true,
      record: { id: activeRecord.id, state: "succeeded" },
    });
  });

  it("keeps a legacy shared run active until its final execution completes", async () => {
    const storage = new MockDurableObjectStorage();
    const coordinator = new DynamicWorkerRunCoordinator(
      mockDurableObjectState(storage),
      {} as Parameters<typeof worker.fetch>[1],
    );
    const activeRecord = {
      id: "legacy_shared_run",
      workerId: "legacy_shared_run",
      state: "running",
      cacheMode: "stable",
      egress: "blocked",
      createdAt: "2026-06-12T21:00:00.000Z",
      startedAt: "2026-06-12T21:00:00.000Z",
      logs: [],
    };
    const firstStarted = await coordinator.fetch(
      new Request("https://run-coordinator/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId: "execution_1",
          expiresAt: Date.now() + 60_000,
          legacyReusableID: true,
          record: activeRecord,
        }),
      }),
    );
    expect(firstStarted.status).toBe(200);
    const secondStarted = await coordinator.fetch(
      new Request("https://run-coordinator/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId: "execution_2",
          expiresAt: Date.now() + 60_000,
          legacyReusableID: true,
          record: activeRecord,
        }),
      }),
    );
    expect(secondStarted.status).toBe(200);

    const firstCompletion = await coordinator.fetch(
      new Request("https://run-coordinator/complete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId: "execution_1",
          release: false,
          record: { ...activeRecord, state: "succeeded" },
        }),
      }),
    );
    await expect(firstCompletion.json()).resolves.toMatchObject({ finalized: false });
    const active = await coordinator.fetch(new Request("https://run-coordinator/record"));
    await expect(active.json()).resolves.toMatchObject({
      record: { id: activeRecord.id, state: "running" },
    });

    const stop = await coordinator.fetch(
      new Request("https://run-coordinator/delete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ acknowledgedComplete: false, runId: activeRecord.id }),
      }),
    );
    expect(stop.status).toBe(409);

    const finalCompletion = await coordinator.fetch(
      new Request("https://run-coordinator/complete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId: "execution_2",
          release: false,
          record: { ...activeRecord, state: "succeeded" },
        }),
      }),
    );
    await expect(finalCompletion.json()).resolves.toMatchObject({ finalized: true });
    const terminal = await coordinator.fetch(new Request("https://run-coordinator/record"));
    await expect(terminal.json()).resolves.toMatchObject({
      record: { id: activeRecord.id, state: "succeeded" },
    });
  });

  it("caps concurrent legacy executions before coordinator state can exceed platform limits", async () => {
    const storage = new MockDurableObjectStorage();
    const coordinator = new DynamicWorkerRunCoordinator(
      mockDurableObjectState(storage),
      {} as Parameters<typeof worker.fetch>[1],
    );
    const metadata = Object.fromEntries(
      Array.from({ length: 16 }, (_, index) => [`metadata-${index}`, "x".repeat(512)]),
    );
    const logs = Array.from({ length: 32 }, () => ({
      level: "info",
      message: "x".repeat(1024),
      time: "2026-06-12T21:00:00.000Z",
    }));
    const start = (index: number) =>
      coordinator.fetch(
        new Request("https://run-coordinator/start", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            executionId: `execution_${index}`,
            expiresAt: Date.now() + 60_000,
            legacyConcurrentReuse: true,
            legacyReusableID: true,
            record: {
              id: "legacy_concurrency_cap",
              executionId: `execution_${index}`,
              workerId: "legacy_concurrency_cap",
              state: "running",
              cacheMode: "stable",
              egress: "blocked",
              createdAt: "2026-06-12T21:00:00.000Z",
              startedAt: "2026-06-12T21:00:00.000Z",
              metadata,
              logs,
            },
          }),
        }),
      );
    const startSequentially = async (index: number): Promise<void> => {
      if (index === 12) return;
      expect((await start(index)).status).toBe(200);
      await startSequentially(index + 1);
    };
    await startSequentially(0);

    const rejected = await start(12);
    expect(rejected.status).toBe(429);
    await expect(rejected.json()).resolves.toEqual({
      error: "run concurrency limit reached",
    });
    expect(JSON.stringify(storage.values.get("state")).length).toBeLessThan(2 * 1024 * 1024);
  });

  it("deletes the latest legacy index generation when an older execution completes last", async () => {
    const namespace = new MockRunCoordinatorNamespace();
    const coordinator = namespace.get("legacy_out_of_order");
    const index = namespace.get("__crabbox/run-index__");
    const record = {
      id: "legacy_out_of_order",
      workerId: "legacy_out_of_order",
      state: "running",
      cacheMode: "stable",
      egress: "blocked",
      createdAt: "2026-06-12T21:00:00.000Z",
      startedAt: "2026-06-12T21:00:00.000Z",
      logs: [],
    };
    const start = async (executionId: string, startedAt: string) => {
      const activeRecord = { ...record, executionId, createdAt: startedAt, startedAt };
      const response = await coordinator.fetch("https://run-coordinator/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId,
          expiresAt: Date.now() + 60_000,
          legacyReusableID: true,
          record: activeRecord,
        }),
      });
      const body = (await response.json()) as { generation: number };
      await index.fetch("https://run-coordinator/index/set", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ generation: body.generation, record: activeRecord }),
      });
      return { activeRecord, generation: body.generation };
    };

    const first = await start("execution_1", "2026-06-12T21:00:00.000Z");
    const second = await start("execution_2", "2026-06-12T21:00:01.000Z");
    const secondCompletion = await coordinator.fetch("https://run-coordinator/complete", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        executionId: "execution_2",
        release: false,
        record: { ...second.activeRecord, state: "succeeded" },
      }),
    });
    const handoff = (await secondCompletion.json()) as {
      activeRecord: Record<string, unknown>;
      finalized: boolean;
      indexGeneration: number;
    };
    expect(handoff).toMatchObject({
      finalized: false,
      activeRecord: { executionId: "execution_1", state: "running" },
    });
    expect(handoff.indexGeneration).toBeGreaterThan(second.generation);
    await index.fetch("https://run-coordinator/index/set", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        generation: handoff.indexGeneration,
        record: handoff.activeRecord,
      }),
    });

    const finalCompletion = await coordinator.fetch("https://run-coordinator/complete", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        executionId: "execution_1",
        release: false,
        record: { ...first.activeRecord, state: "succeeded" },
      }),
    });
    const finalized = (await finalCompletion.json()) as {
      finalized: boolean;
      indexGeneration: number;
    };
    expect(finalized).toMatchObject({
      finalized: true,
      indexGeneration: handoff.indexGeneration,
    });
    await index.fetch("https://run-coordinator/index/delete", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        runId: record.id,
        generation: finalized.indexGeneration,
        tombstone: false,
      }),
    });

    const listed = await index.fetch("https://run-coordinator/index/list");
    await expect(listed.json()).resolves.toEqual({ runs: [], deletedRunIds: [] });
  });

  it("indexes a replacement accepted after deletion even with an earlier start timestamp", async () => {
    const namespace = new MockRunCoordinatorNamespace();
    const coordinator = namespace.get("run_delete_replacement");
    const index = namespace.get("__crabbox/run-index__");
    const deleted = await coordinator.fetch("https://run-coordinator/delete", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        acknowledgedComplete: true,
        runId: "run_delete_replacement",
      }),
    });
    const deletion = (await deleted.json()) as { indexGeneration: number };
    await index.fetch("https://run-coordinator/index/delete", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        runId: "run_delete_replacement",
        generation: deletion.indexGeneration,
        tombstone: true,
      }),
    });

    const replacementRecord = {
      id: "run_delete_replacement",
      executionId: "replacement_execution",
      workerId: "worker_delete_replacement",
      state: "running",
      cacheMode: "one-shot",
      egress: "blocked",
      createdAt: "2026-06-12T20:59:59.000Z",
      startedAt: "2026-06-12T20:59:59.000Z",
      expiresAt: "2026-06-12T21:15:00.000Z",
      logs: [],
    };
    const started = await coordinator.fetch("https://run-coordinator/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        executionId: replacementRecord.executionId,
        expiresAt: Date.parse(replacementRecord.expiresAt),
        legacyReusableID: false,
        record: replacementRecord,
      }),
    });
    const replacement = (await started.json()) as { generation: number };
    expect(replacement.generation).toBeGreaterThan(deletion.indexGeneration);
    await index.fetch("https://run-coordinator/index/set", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        generation: replacement.generation,
        record: replacementRecord,
      }),
    });

    const listed = await index.fetch("https://run-coordinator/index/list");
    await expect(listed.json()).resolves.toEqual({
      runs: [replacementRecord],
      deletedRunIds: [],
    });
  });

  it("serializes expired lease recovery with replacement starts", async () => {
    const runs = new MockKVNamespace();
    const namespace = new MockRunCoordinatorNamespace();
    env(new MockLoader(), runs, namespace);
    const coordinator = namespace.get("run_expired_replacement");
    const expiredRecord = {
      id: "run_expired_replacement",
      executionId: "expired_execution",
      workerId: "worker_expired",
      state: "running",
      cacheMode: "one-shot",
      egress: "blocked",
      createdAt: "2026-06-12T20:30:00.000Z",
      startedAt: "2026-06-12T20:30:00.000Z",
      expiresAt: "2026-06-12T20:45:00.000Z",
      logs: [],
    };
    const started = await coordinator.fetch("https://run-coordinator/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        executionId: expiredRecord.executionId,
        expiresAt: Date.parse(expiredRecord.expiresAt),
        legacyReusableID: true,
        record: expiredRecord,
      }),
    });
    expect(started.status).toBe(200);
    const { expiresAt: _expiresAt, ...terminalRecord } = expiredRecord;
    runs.values.set(
      "runs:run_expired_replacement",
      JSON.stringify({
        ...terminalRecord,
        state: "succeeded",
        completedAt: "2026-06-12T20:31:00.000Z",
      }),
    );

    let releaseGet!: () => void;
    runs.getGate = new Promise((resolve) => {
      releaseGet = resolve;
    });
    let markGetStarted!: () => void;
    const getStarted = new Promise<void>((resolve) => {
      markGetStarted = resolve;
    });
    runs.getStarted = markGetStarted;
    const recovery = coordinator.fetch("https://run-coordinator/record");
    await getStarted;
    delete runs.getGate;

    const replacementRecord = {
      ...expiredRecord,
      executionId: "replacement_execution",
      workerId: "worker_replacement",
      createdAt: "2026-06-12T21:00:00.000Z",
      startedAt: "2026-06-12T21:00:00.000Z",
      expiresAt: "2026-06-12T21:15:00.000Z",
    };
    const replacement = coordinator.fetch("https://run-coordinator/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        executionId: replacementRecord.executionId,
        expiresAt: Date.parse(replacementRecord.expiresAt),
        legacyReusableID: true,
        record: replacementRecord,
      }),
    });
    releaseGet();

    expect((await recovery).status).toBe(200);
    expect((await replacement).status).toBe(200);
    const active = await coordinator.fetch("https://run-coordinator/record");
    await expect(active.json()).resolves.toMatchObject({
      record: {
        executionId: "replacement_execution",
        state: "running",
        workerId: "worker_replacement",
      },
    });
  });

  it("ignores stale completions after an expired execution is replaced", async () => {
    const storage = new MockDurableObjectStorage();
    const coordinator = new DynamicWorkerRunCoordinator(
      mockDurableObjectState(storage),
      {} as Parameters<typeof worker.fetch>[1],
    );
    const activeRecord = {
      id: "run_replaced_execution",
      workerId: "worker_replaced_execution",
      state: "running",
      cacheMode: "one-shot",
      egress: "blocked",
      createdAt: "2026-06-12T21:00:00.000Z",
      startedAt: "2026-06-12T21:00:00.000Z",
      logs: [],
    };
    await coordinator.fetch(
      new Request("https://run-coordinator/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId: "expired_execution",
          expiresAt: Date.now() - 1,
          legacyReusableID: true,
          record: { ...activeRecord, executionId: "expired_execution" },
        }),
      }),
    );
    await coordinator.fetch(
      new Request("https://run-coordinator/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId: "replacement_execution",
          expiresAt: Date.now() + 60_000,
          legacyReusableID: true,
          record: { ...activeRecord, executionId: "replacement_execution" },
        }),
      }),
    );

    const stale = await coordinator.fetch(
      new Request("https://run-coordinator/complete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId: "expired_execution",
          release: true,
          record: { ...activeRecord, state: "failed" },
        }),
      }),
    );
    await expect(stale.json()).resolves.toMatchObject({
      finalized: false,
      activeRecord: { executionId: "replacement_execution", state: "running" },
    });
    const replacement = await coordinator.fetch(new Request("https://run-coordinator/record"));
    await expect(replacement.json()).resolves.toMatchObject({
      record: { id: activeRecord.id, state: "running" },
    });

    const completed = await coordinator.fetch(
      new Request("https://run-coordinator/complete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId: "replacement_execution",
          release: false,
          record: { ...activeRecord, state: "succeeded" },
        }),
      }),
    );
    await expect(completed.json()).resolves.toMatchObject({ finalized: true });
    const terminal = await coordinator.fetch(new Request("https://run-coordinator/record"));
    await expect(terminal.json()).resolves.toMatchObject({
      record: { id: activeRecord.id, state: "succeeded" },
    });
  });

  it("schedules alarm cleanup for abandoned execution leases", async () => {
    const storage = new MockDurableObjectStorage();
    const coordinator = new DynamicWorkerRunCoordinator(
      mockDurableObjectState(storage),
      {} as Parameters<typeof worker.fetch>[1],
    );
    const expiresAt = Date.now() + 1_000;
    const started = await coordinator.fetch(
      new Request("https://run-coordinator/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId: "abandoned_execution",
          expiresAt,
          legacyReusableID: false,
          record: {
            id: "run_abandoned",
            executionId: "abandoned_execution",
            workerId: "worker_abandoned",
            state: "running",
            cacheMode: "one-shot",
            egress: "blocked",
            createdAt: "2026-06-12T21:00:00.000Z",
            startedAt: "2026-06-12T21:00:00.000Z",
            expiresAt: new Date(expiresAt).toISOString(),
            logs: [],
          },
        }),
      }),
    );
    expect(started.status).toBe(200);
    const initialGeneration = ((await started.json()) as { generation: number }).generation;
    expect(storage.alarmTime).toBe(expiresAt);

    vi.setSystemTime(new Date(expiresAt + 1));
    await coordinator.alarm();
    expect(storage.values.get("state")).toMatchObject({
      executions: {},
      generation: initialGeneration,
      deletedAt: expiresAt + 1,
    });
    expect(storage.alarmTime).toBe(expiresAt + 1 + 10 * 60 * 1000);

    vi.advanceTimersByTime(10 * 60 * 1000 + 1);
    await coordinator.alarm();
    expect(storage.values.has("state")).toBe(false);
    expect(storage.alarmTime).toBeNull();
    expect(storage.deleteAllCalls).toBe(1);

    const replacement = await coordinator.fetch(
      new Request("https://run-coordinator/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          executionId: "replacement_execution",
          expiresAt: Date.now() + 60_000,
          legacyReusableID: false,
          record: {
            id: "run_abandoned",
            executionId: "replacement_execution",
            workerId: "worker_replacement",
            state: "running",
            cacheMode: "one-shot",
            egress: "blocked",
            createdAt: new Date().toISOString(),
            startedAt: new Date().toISOString(),
            expiresAt: new Date(Date.now() + 60_000).toISOString(),
            logs: [],
          },
        }),
      }),
    );
    const replacementGeneration = ((await replacement.json()) as { generation: number }).generation;
    expect(replacementGeneration).toBeGreaterThan(initialGeneration);
  });

  it("indexes active and retained runs in a coordinator object", async () => {
    const storage = new MockDurableObjectStorage();
    const coordinator = new DynamicWorkerRunCoordinator(
      mockDurableObjectState(storage),
      {} as Parameters<typeof worker.fetch>[1],
    );
    const record = {
      id: "run_indexed",
      workerId: "worker_indexed",
      state: "running",
      cacheMode: "one-shot",
      egress: "blocked",
      createdAt: "2026-06-12T21:00:00.000Z",
      startedAt: "2026-06-12T21:00:00.000Z",
      logs: [],
    };
    const indexed = await coordinator.fetch(
      new Request("https://run-coordinator/index/set", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ generation: 1, record }),
      }),
    );
    expect(indexed.status).toBe(200);

    const listed = await coordinator.fetch(new Request("https://run-coordinator/index/list"));
    await expect(listed.json()).resolves.toEqual({ runs: [record], deletedRunIds: [] });

    const deleted = await coordinator.fetch(
      new Request("https://run-coordinator/index/delete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ runId: record.id, generation: 1 }),
      }),
    );
    expect(deleted.status).toBe(200);
    const empty = await coordinator.fetch(new Request("https://run-coordinator/index/list"));
    await expect(empty.json()).resolves.toEqual({ runs: [], deletedRunIds: [] });

    const expiredRecord = {
      ...record,
      id: "run_index_expired",
      expiresAt: "2026-06-12T20:15:00.000Z",
    };
    await coordinator.fetch(
      new Request("https://run-coordinator/index/set", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ generation: 1, record: expiredRecord }),
      }),
    );
    const pruned = await coordinator.fetch(new Request("https://run-coordinator/index/list"));
    await expect(pruned.json()).resolves.toEqual({ runs: [], deletedRunIds: [] });
    expect(storage.values.has("run:run_index_expired")).toBe(false);
  });

  it("indexes deletion tombstones and rejects delayed older run updates", async () => {
    const storage = new MockDurableObjectStorage();
    const coordinator = new DynamicWorkerRunCoordinator(
      mockDurableObjectState(storage),
      {} as Parameters<typeof worker.fetch>[1],
    );
    const record = {
      id: "run_index_deleted",
      executionId: "deleted_execution",
      workerId: "worker_index_deleted",
      state: "running",
      cacheMode: "one-shot",
      egress: "blocked",
      createdAt: "2026-06-12T21:00:00.000Z",
      startedAt: "2026-06-12T21:00:00.000Z",
      logs: [],
    };

    await coordinator.fetch(
      new Request("https://run-coordinator/index/delete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          runId: record.id,
          generation: 1,
          tombstone: true,
        }),
      }),
    );
    await coordinator.fetch(
      new Request("https://run-coordinator/index/set", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ generation: 1, record }),
      }),
    );

    const listed = await coordinator.fetch(new Request("https://run-coordinator/index/list"));
    await expect(listed.json()).resolves.toEqual({
      runs: [],
      deletedRunIds: [record.id],
    });
    expect(storage.alarmTime).toBe(Date.now() + 10 * 60 * 1000);

    vi.advanceTimersByTime(10 * 60 * 1000 + 1);
    await coordinator.alarm();
    expect(storage.values.has(`deleted:${record.id}`)).toBe(false);
    expect(storage.alarmTime).toBeNull();
  });

  it("does not let an older execution overwrite or delete a newer index entry", async () => {
    const storage = new MockDurableObjectStorage();
    const coordinator = new DynamicWorkerRunCoordinator(
      mockDurableObjectState(storage),
      {} as Parameters<typeof worker.fetch>[1],
    );
    const record = {
      id: "legacy_index_generation",
      executionId: "new_execution",
      workerId: "legacy_index_generation",
      state: "running",
      cacheMode: "stable",
      egress: "blocked",
      createdAt: "2026-06-12T21:00:01.000Z",
      startedAt: "2026-06-12T21:00:01.000Z",
      logs: [],
    };
    await coordinator.fetch(
      new Request("https://run-coordinator/index/set", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ generation: 2, record }),
      }),
    );
    await coordinator.fetch(
      new Request("https://run-coordinator/index/set", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          generation: 1,
          record: {
            ...record,
            executionId: "old_execution",
            createdAt: "2026-06-12T21:00:00.000Z",
            startedAt: "2026-06-12T21:00:00.000Z",
          },
        }),
      }),
    );
    await coordinator.fetch(
      new Request("https://run-coordinator/index/delete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          runId: record.id,
          generation: 1,
          tombstone: true,
        }),
      }),
    );

    const listed = await coordinator.fetch(new Request("https://run-coordinator/index/list"));
    await expect(listed.json()).resolves.toEqual({ runs: [record], deletedRunIds: [] });
  });

  it("requires bearer auth without leaking the configured token", async () => {
    const response = await worker.fetch(
      new Request("https://runner.example/v1/readiness"),
      env(),
      ctx(),
    );

    expect(response.status).toBe(401);
    const text = await response.text();
    expect(JSON.parse(text)).toEqual({ error: "unauthorized" });
    expect(text).not.toContain("runner-token");
  });

  it("rejects non-object JSON payloads", async () => {
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify([]),
      }),
      env(),
      ctx(),
    );

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toEqual({ error: "json body must be an object" });
  });

  it("rejects the reserved global run index ID", async () => {
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "__crabbox/run-index__" })),
      }),
      env(),
      ctx(),
    );

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toEqual({ error: "id is invalid" });
  });

  it("runs one-shot modules with blocked egress and propagated limits", async () => {
    const loader = new MockLoader();
    loader.nextResponse = new Response("done", {
      status: 201,
      statusText: "Created",
      headers: { "X-Result": "ok" },
    });

    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            limits: { cpuMs: 10, subRequests: 5 },
          }),
        ),
      }),
      env(loader),
      ctx(),
    );

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      id: "run_1",
      workerId: "run_1",
      status: "succeeded",
      exitCode: 0,
      cacheMode: "one-shot",
      egress: "blocked",
      body: "done",
      result: {
        status: 201,
        statusText: "Created",
        headers: { "x-result": "ok" },
        body: "done",
      },
    });
    expect(loader.loadCalls).toHaveLength(1);
    expect(loader.getCalls).toHaveLength(0);
    expect(loader.worker?.code).toMatchObject({
      compatibilityDate: "2026-06-12",
      mainModule: "index.js",
      limits: { cpuMs: 10, subRequests: 5 },
      globalOutbound: null,
    });
    expect(loader.worker?.entrypointOptions).toEqual({
      limits: { cpuMs: 10, subRequests: 5 },
    });
    expect(loader.worker?.request?.method).toBe("POST");
    expect(loader.worker?.request?.url).toBe("https://example.test/execute");
    await expect(loader.worker?.request?.text()).resolves.toBe("payload");
  });

  it("executes when the best-effort global run index is unavailable", async () => {
    const loader = new MockLoader();
    loader.nextResponse = new Response("executed");
    const testEnv = env(loader, undefined, new MockRunCoordinatorNamespace(true));
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_index_unavailable", retainMetadata: false })),
      }),
      testEnv,
      ctx(),
    );

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      id: "run_index_unavailable",
      status: "succeeded",
      body: "executed",
    });
    expect(loader.loadCalls).toHaveLength(1);
  });

  it("returns a successful result as lifecycle-uncertain after completion retries fail", async () => {
    const loader = new MockLoader();
    loader.nextResponse = new Response("executed");
    const coordinator = new MockRunCoordinatorNamespace();
    coordinator.completionFailures = 3;
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_completion_uncertain",
            retainMetadata: false,
          }),
        ),
      }),
      env(loader, undefined, coordinator),
      ctx(),
    );

    expect(response.status).toBe(200);
    expect(response.headers.get("X-Crabbox-Lifecycle-Uncertain")).toBe("true");
    await expect(response.json()).resolves.toMatchObject({
      id: "run_completion_uncertain",
      status: "succeeded",
      body: "executed",
      lifecycleUncertain: true,
    });
    expect(loader.loadCalls).toHaveLength(1);
    expect(
      coordinator.requests.filter(
        (request) => request.id === "run_completion_uncertain" && request.path === "/complete",
      ),
    ).toHaveLength(3);
  });

  it("replays a committed completion after its response is lost", async () => {
    const loader = new MockLoader();
    loader.nextResponse = new Response("executed");
    const coordinator = new MockRunCoordinatorNamespace();
    coordinator.completionResponseLosses = 1;
    const testEnv = env(loader, undefined, coordinator);
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_completion_response_lost",
            retainMetadata: false,
          }),
        ),
      }),
      testEnv,
      ctx(),
    );

    expect(response.status).toBe(200);
    expect(response.headers.has("X-Crabbox-Lifecycle-Uncertain")).toBe(false);
    expect(
      coordinator.requests.filter(
        (request) => request.id === "run_completion_response_lost" && request.path === "/complete",
      ),
    ).toHaveLength(2);
    expect(
      coordinator.requests.some(
        (request) => isRunIndexObject(request.id) && request.path === "/index/delete",
      ),
    ).toBe(true);
  });

  it("retries terminal run-index cleanup from the coordinator alarm", async () => {
    const coordinator = new MockRunCoordinatorNamespace();
    coordinator.indexDeleteFailures = 1;
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_index_cleanup_retry",
            retainMetadata: false,
          }),
        ),
      }),
      env(new MockLoader(), undefined, coordinator),
      ctx(),
    );

    expect(response.status).toBe(200);
    expect(
      coordinator.requests.filter(
        (request) => isRunIndexObject(request.id) && request.path === "/index/delete",
      ),
    ).toHaveLength(1);

    await coordinator.alarm("run_index_cleanup_retry");
    const deletions = coordinator.requests.filter(
      (request) => isRunIndexObject(request.id) && request.path === "/index/delete",
    );
    expect(deletions).toHaveLength(2);
    const listed = await coordinator
      .get(deletions[1]!.id)
      .fetch("https://run-coordinator/index/list");
    await expect(listed.json()).resolves.toMatchObject({
      runs: [],
      deletedRunIds: ["run_index_cleanup_retry"],
    });
  });

  it("durably reconciles a retained result after completion retries fail", async () => {
    const loader = new MockLoader();
    loader.nextResponse = new Response("executed");
    const runs = new MockKVNamespace();
    const coordinator = new MockRunCoordinatorNamespace();
    coordinator.completionFailures = 3;
    const testEnv = env(loader, runs, coordinator);
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_completion_recovered",
            retainMetadata: true,
          }),
        ),
      }),
      testEnv,
      ctx(),
    );

    expect(response.headers.get("X-Crabbox-Lifecycle-Uncertain")).toBe("true");
    vi.advanceTimersByTime(20 * 60 * 1000);
    await coordinator.alarm("run_completion_recovered");

    const status = await worker.fetch(
      authedRequest("/v1/runs/run_completion_recovered"),
      testEnv,
      ctx(),
    );
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({
      id: "run_completion_recovered",
      status: "succeeded",
      body: "executed",
    });
    expect(runs.values.has("runs:run_completion_recovered")).toBe(true);
  });

  it("fails closed when the sharded run index is unavailable", async () => {
    const runs = new MockKVNamespace();
    const record = {
      id: "run_retained_index_unavailable",
      workerId: "worker_retained_index_unavailable",
      state: "succeeded",
      cacheMode: "one-shot",
      egress: "blocked",
      createdAt: "2026-06-12T21:00:00.000Z",
      startedAt: "2026-06-12T21:00:00.000Z",
      completedAt: "2026-06-12T21:00:01.000Z",
      logs: [],
    };
    runs.values.set(`runs:${record.id}`, JSON.stringify(record));

    const response = await worker.fetch(
      authedRequest("/v1/runs"),
      env(new MockLoader(), runs, new MockRunCoordinatorNamespace(true)),
      ctx(),
    );

    expect(response.status).toBe(503);
    await expect(response.json()).resolves.toEqual({
      error: "run index is temporarily unavailable",
    });
  });

  it("lists high-cardinality retained history without per-run coordinator requests", async () => {
    const runs = new MockKVNamespace();
    const coordinator = new MockRunCoordinatorNamespace();
    for (let index = 0; index < 1200; index += 1) {
      const id = `run_retained_${index}`;
      runs.values.set(
        `runs:${id}`,
        JSON.stringify({
          id,
          workerId: `worker_retained_${index}`,
          state: "succeeded",
          cacheMode: "one-shot",
          egress: "blocked",
          createdAt: "2026-06-12T20:00:00.000Z",
          startedAt: "2026-06-12T20:00:00.000Z",
          completedAt: "2026-06-12T20:00:01.000Z",
          logs: [],
        }),
      );
    }

    const response = await worker.fetch(
      authedRequest("/v1/runs"),
      env(new MockLoader(), runs, coordinator),
      ctx(),
    );
    const body = (await response.json()) as { runs: Array<{ id: string }> };

    expect(response.status).toBe(200);
    expect(body.runs).toHaveLength(1200);
    expect(runs.listCalls).toEqual([{ prefix: "runs:" }, { prefix: "runs:", cursor: "1000" }]);
    expect(runs.getCalls).toHaveLength(12);
    expect(runs.getCalls.every((call) => Array.isArray(call) && call.length <= 100)).toBe(true);
    const indexLists = coordinator.requests.filter(
      (request) => isRunIndexObject(request.id) && request.path === "/index/list",
    );
    expect(indexLists).toHaveLength(16);
    expect(new Set(indexLists.map((request) => request.id)).size).toBe(16);
  });

  it("splits oversized KV bulk reads and falls back to single-key reads", async () => {
    const runs = new MockKVNamespace();
    runs.bulkGetMaxBytes = 300;
    for (let index = 0; index < 4; index += 1) {
      const id = `run_large_${index}`;
      runs.values.set(
        `runs:${id}`,
        JSON.stringify({
          id,
          workerId: `worker_large_${index}`,
          state: "succeeded",
          cacheMode: "one-shot",
          egress: "blocked",
          createdAt: "2026-06-12T20:00:00.000Z",
          startedAt: "2026-06-12T20:00:00.000Z",
          completedAt: "2026-06-12T20:00:01.000Z",
          result: { status: 200, statusText: "OK", headers: {}, body: "x".repeat(200) },
          logs: [],
        }),
      );
    }

    const response = await worker.fetch(
      authedRequest("/v1/runs"),
      env(new MockLoader(), runs),
      ctx(),
    );
    const body = (await response.json()) as { runs: Array<{ id: string }> };

    expect(response.status).toBe(200);
    expect(body.runs).toHaveLength(4);
    expect(runs.getCalls.some((call) => !Array.isArray(call))).toBe(true);
  });

  it("renews the bounded lease for a run without an enforced timeout", async () => {
    let releaseFetch!: () => void;
    const loader = new MockLoader();
    loader.nextFetchGate = new Promise((resolve) => {
      releaseFetch = resolve;
    });
    const coordinator = new MockRunCoordinatorNamespace();
    const responsePromise = worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_without_timeout" })),
      }),
      env(loader, undefined, coordinator),
      ctx(),
    );

    const dynamicWorker = await loader.workerCreated;
    await dynamicWorker.fetchStarted;
    const start = coordinator.requests.find(
      (request) => request.id === "run_without_timeout" && request.path === "/start",
    );
    const startBody = start?.body as {
      expiresAt: number;
      record: { id: string; expiresAt?: string };
    };
    expect(startBody.expiresAt).toBeGreaterThan(Date.now());
    expect(startBody.record).toMatchObject({ id: "run_without_timeout" });
    expect(startBody.record.expiresAt).toBe(new Date(startBody.expiresAt).toISOString());

    await vi.advanceTimersByTimeAsync(5 * 60 * 1000);
    const renewal = coordinator.requests.find(
      (request) => request.id === "run_without_timeout" && request.path === "/renew",
    );
    expect(renewal?.body).toMatchObject({
      executionId: expect.any(String),
      expiresAt: expect.any(Number),
    });

    releaseFetch();
    const response = await responsePromise;
    expect(response.status).toBe(200);
  });

  it("does not shorten a long timeout reservation during heartbeat renewal", async () => {
    let releaseFetch!: () => void;
    const loader = new MockLoader();
    loader.nextFetchGate = new Promise((resolve) => {
      releaseFetch = resolve;
    });
    const coordinator = new MockRunCoordinatorNamespace();
    const responsePromise = worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_long_timeout",
            timeoutMs: 30 * 60 * 1000,
          }),
        ),
      }),
      env(loader, undefined, coordinator),
      ctx(),
    );

    const dynamicWorker = await loader.workerCreated;
    await dynamicWorker.fetchStarted;
    const start = coordinator.requests.find(
      (request) => request.id === "run_long_timeout" && request.path === "/start",
    );
    const startBody = start?.body as { expiresAt: number };

    await vi.advanceTimersByTimeAsync(5 * 60 * 1000);
    const renewal = coordinator.requests.find(
      (request) => request.id === "run_long_timeout" && request.path === "/renew",
    );
    const renewalBody = renewal?.body as { expiresAt: number };
    expect(renewalBody.expiresAt).toBeGreaterThanOrEqual(startBody.expiresAt);

    releaseFetch();
    const response = await responsePromise;
    expect(response.status).toBe(200);
  });

  it("rejects a delayed active index set after completion", async () => {
    let releaseIndexSet!: () => void;
    const indexSetGate = new Promise<void>((resolve) => {
      releaseIndexSet = resolve;
    });
    const coordinator = new MockRunCoordinatorNamespace(false, indexSetGate);
    const testCtx = ctx();
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_ordered_index", retainMetadata: false })),
      }),
      env(new MockLoader(), undefined, coordinator),
      testCtx,
    );

    expect(response.status).toBe(200);
    expect(
      coordinator.requests
        .filter((request) => isRunIndexObject(request.id))
        .map((request) => request.path),
    ).toEqual(["/index/set", "/index/delete"]);
    const [setRequest, deleteRequest] = coordinator.requests.filter((request) =>
      isRunIndexObject(request.id),
    );
    expect(setRequest).toBeDefined();
    const setBody = setRequest!.body as { generation: number };
    expect(deleteRequest?.body).toMatchObject({
      generation: setBody.generation,
      tombstone: true,
    });
    releaseIndexSet();
    await Promise.all(testCtx.waitUntil.mock.calls.map(([promise]) => promise));
    const listed = await coordinator
      .get(setRequest!.id)
      .fetch("https://run-coordinator/index/list");
    await expect(listed.json()).resolves.toEqual({
      runs: [],
      deletedRunIds: ["run_ordered_index"],
    });
  });

  it("persists HTTP error responses as failed runs", async () => {
    const loader = new MockLoader();
    loader.nextResponse = new Response("not found", {
      status: 404,
      statusText: "Not Found",
    });
    const testEnv = env(loader);
    const testCtx = ctx();

    const create = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_http_error" })),
      }),
      testEnv,
      testCtx,
    );

    expect(create.status).toBe(200);
    await expect(create.json()).resolves.toMatchObject({
      id: "run_http_error",
      status: "failed",
      exitCode: 1,
      result: {
        status: 404,
        statusText: "Not Found",
        body: "not found",
      },
    });

    const status = await worker.fetch(authedRequest("/v1/runs/run_http_error"), testEnv, testCtx);
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({
      id: "run_http_error",
      status: "failed",
      exitCode: 1,
    });
  });

  it("uses loader get for stable cache mode and preserves worker IDs", async () => {
    const loader = new MockLoader();
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_stable",
            workerId: "worker:v1",
            cacheMode: "stable",
          }),
        ),
      }),
      env(loader),
      ctx(),
    );

    expect(response.status).toBe(200);
    expect(loader.loadCalls).toHaveLength(0);
    expect(loader.getCalls).toEqual([{ id: "worker:v1" }]);
    await expect(response.json()).resolves.toMatchObject({
      id: "run_stable",
      workerId: "worker:v1",
      exitCode: 0,
      cacheMode: "stable",
    });
  });

  it("accepts the Go provider module compatibility payload", async () => {
    const loader = new MockLoader();
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify({
          id: "run_go_payload",
          cacheMode: "stable",
          metadata: { team: "platform" },
          module: {
            name: "worker.mjs",
            source: "export default { fetch() { return new Response('go'); } };",
          },
          limits: { cpuMs: 25, subrequests: 4 },
        }),
      }),
      env(loader),
      ctx(),
    );

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      id: "run_go_payload",
      workerId: "run_go_payload",
      status: "succeeded",
      exitCode: 0,
      body: "ok",
      metadata: { team: "platform" },
    });
    expect(loader.getCalls).toEqual([{ id: "run_go_payload" }]);
    expect(loader.worker?.code).toMatchObject({
      mainModule: "worker.mjs",
      modules: {
        "worker.mjs": {
          js: "export default { fetch() { return new Response('go'); } };",
        },
      },
      limits: { cpuMs: 25, subRequests: 4 },
    });
  });

  it("enables the required compatibility flag for Python modules", async () => {
    const loader = new MockLoader();
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify({
          id: "run_python_payload",
          compatibilityFlags: ["nodejs_compat"],
          module: {
            name: "worker.py",
            source: "from workers import Response, WorkerEntrypoint",
          },
        }),
      }),
      env(loader),
      ctx(),
    );

    expect(response.status).toBe(200);
    expect(loader.worker?.code).toMatchObject({
      compatibilityFlags: ["nodejs_compat", "python_workers"],
      mainModule: "worker.py",
      modules: {
        "worker.py": {
          py: "from workers import Response, WorkerEntrypoint",
        },
      },
    });
  });

  it("enables the Python compatibility flag for plain-string modules", async () => {
    const loader = new MockLoader();
    const source = "from workers import Response, WorkerEntrypoint";
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify({
          id: "run_python_modules_payload",
          mainModule: "worker.py",
          modules: {
            "worker.py": source,
          },
        }),
      }),
      env(loader),
      ctx(),
    );

    expect(response.status).toBe(200);
    expect(loader.worker?.code).toMatchObject({
      compatibilityFlags: ["python_workers"],
      mainModule: "worker.py",
      modules: {
        "worker.py": source,
      },
    });
  });

  it("allows concurrent legacy clients to reuse a stable run ID as the worker cache ID", async () => {
    const loader = new MockLoader();
    let releaseFetch!: () => void;
    loader.nextFetchGate = new Promise((resolve) => {
      releaseFetch = resolve;
    });
    const testEnv = env(loader);
    const payload = {
      id: "legacy_stable_worker",
      cacheMode: "stable",
      module: {
        name: "worker.mjs",
        source: "export default { fetch() { return new Response('legacy'); } };",
      },
    };

    const firstPromise = worker.fetch(
      authedRequest("/v1/runs", { method: "POST", body: JSON.stringify(payload) }),
      testEnv,
      ctx(),
    );
    const secondPromise = worker.fetch(
      authedRequest("/v1/runs", { method: "POST", body: JSON.stringify(payload) }),
      testEnv,
      ctx(),
    );
    await loader.waitForWorkers(2);
    loader.workers[0]!.response = new Response("first");
    loader.workers[1]!.response = new Response("second");
    releaseFetch();
    const [first, second] = await Promise.all([firstPromise, secondPromise]);

    expect(first.status).toBe(200);
    expect(second.status).toBe(200);
    const firstBody = (await first.json()) as {
      body: string;
      logEvents: Array<{ message: string }>;
    };
    const secondBody = (await second.json()) as {
      body: string;
      logEvents: Array<{ message: string }>;
    };
    expect([firstBody.body, secondBody.body]).toEqual(["first", "second"]);
    expect(firstBody.logEvents.map((event) => event.message)).toEqual([
      "run started",
      "run completed with HTTP 200",
    ]);
    expect(secondBody.logEvents.map((event) => event.message)).toEqual([
      "run started",
      "run completed with HTTP 200",
    ]);
    expect(loader.getCalls).toEqual([
      { id: "legacy_stable_worker" },
      { id: "legacy_stable_worker" },
    ]);
  });

  it("deletes a retained legacy result before its run ID is reused", async () => {
    const loader = new MockLoader();
    const runs = new MockKVNamespace();
    const testEnv = env(loader, runs);
    const payload = {
      id: "legacy_reused_result",
      cacheMode: "stable",
      module: {
        name: "worker.mjs",
        source: "export default { fetch() { return new Response('legacy'); } };",
      },
    };
    const first = await worker.fetch(
      authedRequest("/v1/runs", { method: "POST", body: JSON.stringify(payload) }),
      testEnv,
      ctx(),
    );
    expect(first.status).toBe(200);
    expect(runs.values.has("runs:legacy_reused_result")).toBe(true);

    let releaseFetch!: () => void;
    loader.nextFetchGate = new Promise((resolve) => {
      releaseFetch = resolve;
    });
    const secondPromise = worker.fetch(
      authedRequest("/v1/runs", { method: "POST", body: JSON.stringify(payload) }),
      testEnv,
      ctx(),
    );
    await loader.waitForWorkers(2);
    await loader.workers[1]!.fetchStarted;
    expect(runs.values.has("runs:legacy_reused_result")).toBe(false);

    releaseFetch();
    expect((await secondPromise).status).toBe(200);
  });

  it("retains metadata for existing workerId clients without retention flags", async () => {
    const runs = new MockKVNamespace();
    const loader = new MockLoader();
    const testEnv = env(loader, runs);
    const payload = {
      id: "legacy_worker_id_run",
      workerId: "legacy_worker_cache",
      cacheMode: "stable",
      module: {
        name: "worker.mjs",
        source: "export default { fetch() { return new Response('legacy'); } };",
      },
    };
    const run = () =>
      worker.fetch(
        authedRequest("/v1/runs", { method: "POST", body: JSON.stringify(payload) }),
        testEnv,
        ctx(),
      );
    expect((await run()).status).toBe(200);
    expect((await run()).status).toBe(200);
    expect(runs.putCalls.map((call) => call.key)).toEqual([
      "runs:legacy_worker_id_run",
      "runs:legacy_worker_id_run",
    ]);
    const status = await worker.fetch(
      authedRequest("/v1/runs/legacy_worker_id_run"),
      testEnv,
      ctx(),
    );
    expect(status.status).toBe(200);
  });

  it("requires workerId before a reusable legacy ID can opt out of retention", async () => {
    const runs = new MockKVNamespace();
    const testEnv = env(new MockLoader(), runs);
    const payload = {
      id: "legacy_reused_ephemeral",
      cacheMode: "stable",
      module: {
        name: "worker.mjs",
        source: "export default { fetch() { return new Response('legacy'); } };",
      },
    };
    const retained = await worker.fetch(
      authedRequest("/v1/runs", { method: "POST", body: JSON.stringify(payload) }),
      testEnv,
      ctx(),
    );
    expect(retained.status).toBe(200);
    expect(runs.values.has("runs:legacy_reused_ephemeral")).toBe(true);

    const ephemeral = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify({ ...payload, retainMetadata: false }),
      }),
      testEnv,
      ctx(),
    );
    expect(ephemeral.status).toBe(400);
    await expect(ephemeral.json()).resolves.toEqual({
      error: "retainMetadata=false requires workerId for reusable cache modes",
    });
    expect(runs.values.has("runs:legacy_reused_ephemeral")).toBe(true);
    const status = await worker.fetch(
      authedRequest("/v1/runs/legacy_reused_ephemeral"),
      testEnv,
      ctx(),
    );
    expect(status.status).toBe(200);
  });

  it("retains legacy explicit-ID one-shot metadata", async () => {
    const testEnv = env();
    const payload = {
      id: "legacy_one_shot",
      cacheMode: "one-shot",
      module: {
        name: "worker.mjs",
        source: "export default { fetch() { return new Response('legacy'); } };",
      },
    };
    const run = () =>
      worker.fetch(
        authedRequest("/v1/runs", {
          method: "POST",
          body: JSON.stringify(payload),
        }),
        testEnv,
        ctx(),
      );
    expect((await run()).status).toBe(200);
    expect((await run()).status).toBe(200);

    const status = await worker.fetch(authedRequest("/v1/runs/legacy_one_shot"), testEnv, ctx());
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({ status: "succeeded" });
  });

  it("retains legacy runId one-shot metadata", async () => {
    const testEnv = env();
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify({
          runId: "legacy_run_id",
          cacheMode: "one-shot",
          module: {
            name: "worker.mjs",
            source: "export default { fetch() { return new Response('legacy'); } };",
          },
        }),
      }),
      testEnv,
      ctx(),
    );
    expect(response.status).toBe(200);

    const status = await worker.fetch(authedRequest("/v1/runs/legacy_run_id"), testEnv, ctx());
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({ status: "succeeded" });
  });

  it.each([
    ["worker.py", "py"],
    ["worker.cjs", "cjs"],
  ])("preserves the %s compatibility module type", async (name, moduleType) => {
    const loader = new MockLoader();
    const source = "compatibility module source";
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify({
          id: `run_${moduleType}`,
          module: { name, source },
        }),
      }),
      env(loader),
      ctx(),
    );

    expect(response.status).toBe(200);
    expect(loader.worker?.code).toMatchObject({
      mainModule: name,
      modules: {
        [name]: {
          [moduleType]: source,
        },
      },
    });
  });

  it("rejects TypeScript compatibility modules before loading a Dynamic Worker", async () => {
    const loader = new MockLoader();
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify({
          id: "run_typescript",
          module: {
            name: "worker.ts",
            source:
              "const value: string = 'typescript'; export default { fetch() { return new Response(value); } };",
          },
        }),
      }),
      env(loader),
      ctx(),
    );

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toEqual({
      error: "TypeScript module source must be transpiled to JavaScript",
    });
    expect(loader.loadCalls).toHaveLength(0);
    expect(loader.getCalls).toHaveLength(0);
  });

  it("accepts explicitly typed JavaScript modules with TypeScript filenames", async () => {
    const loader = new MockLoader();
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_typed_typescript_name",
            mainModule: "worker.ts",
            modules: {
              "worker.ts": {
                js: "export default { fetch() { return new Response('transpiled'); } };",
              },
            },
          }),
        ),
      }),
      env(loader),
      ctx(),
    );

    expect(response.status).toBe(200);
    expect(loader.worker?.code).toMatchObject({
      mainModule: "worker.ts",
      modules: {
        "worker.ts": {
          js: "export default { fetch() { return new Response('transpiled'); } };",
        },
      },
    });
  });

  it("uses the Go provider workerId as the explicit Dynamic Worker cache identity", async () => {
    const loader = new MockLoader();
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify({
          id: "run_explicit",
          workerId: "explicit-worker",
          cacheMode: "explicit",
          module: {
            name: "worker.mjs",
            source: "export default { fetch() { return new Response('explicit'); } };",
          },
        }),
      }),
      env(loader),
      ctx(),
    );

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      id: "run_explicit",
      workerId: "explicit-worker",
      cacheMode: "explicit",
      exitCode: 0,
    });
    expect(loader.getCalls).toEqual([{ id: "explicit-worker" }]);
  });

  it("wires intercept egress through loader-owned gateway and tail bindings", async () => {
    const loader = new MockLoader();
    const gateway = { binding: "gateway" };
    const tailer = { binding: "tailer" };
    const testCtx = ctx(gateway, tailer);
    const response = await worker.fetch(
      authedRequest("/v1/runs?egress=intercept", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_intercept",
            gateway: { allowHostnames: ["api.example.com"] },
          }),
        ),
      }),
      env(loader),
      testCtx,
    );

    expect(response.status).toBe(200);
    expect(testCtx.exports.HttpGateway).toHaveBeenCalledWith({
      props: { executionId: expect.any(String), allowHostnames: ["api.example.com"] },
    });
    expect(testCtx.exports.LogTailer).toHaveBeenCalledWith({
      props: { runId: "run_intercept", executionId: expect.any(String) },
    });
    expect(loader.worker?.code?.globalOutbound).toBe(gateway);
    expect(loader.worker?.code?.tails).toEqual([tailer]);
  });

  it("blocks redirects when intercept egress uses an allowlist", async () => {
    const fetchMock = vi.fn<(request: Request) => Promise<Response>>(async (request) => {
      expect(request.redirect).toBe("manual");
      return new Response(null, {
        status: 302,
        headers: { Location: "https://blocked.example.test/private" },
      });
    });
    vi.stubGlobal("fetch", fetchMock);
    try {
      const gateway = new HttpGateway({} as never, {
        executionId: "redirect_execution",
        allowHostnames: ["allowed.example.test"],
      });
      const response = await gateway.fetch(new Request("https://allowed.example.test/redirect"));
      expect(response.status).toBe(403);
      await expect(response.json()).resolves.toEqual({ error: "egress redirect blocked" });
      expect(fetchMock).toHaveBeenCalledTimes(1);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("truncates oversized dynamic worker response bodies", async () => {
    const loader = new MockLoader();
    loader.nextResponse = new Response("x".repeat(1024 * 1024 + 1024));
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_large_response",
            retainMetadata: false,
          }),
        ),
      }),
      env(loader),
      ctx(),
    );
    const body = (await response.json()) as { body: string };

    expect(response.status).toBe(200);
    expect(body.body.length).toBeLessThan(1024 * 1024 + 100);
    expect(body.body.endsWith("[crabbox response body truncated]")).toBe(true);
  });

  it("fails runs when response bodies exceed the configured deadline", async () => {
    const loader = new MockLoader();
    loader.nextResponse = new Response(
      new ReadableStream({
        start() {},
      }),
    );
    const responsePromise = worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_response_timeout",
            retainMetadata: true,
            timeoutMs: 10,
          }),
        ),
      }),
      env(loader, new MockKVNamespace()),
      ctx(),
    );
    await loader.workerCreated;
    await vi.advanceTimersByTimeAsync(20);
    const response = await responsePromise;

    expect(response.status).toBe(502);
    await expect(response.json()).resolves.toMatchObject({
      id: "run_response_timeout",
      status: "failed",
      exitCode: 1,
      error: { message: "dynamic worker response body read timed out" },
    });
  });

  it("rejects intercept egress with reusable cache modes", async () => {
    const loader = new MockLoader();
    const response = await worker.fetch(
      authedRequest("/v1/runs?egress=intercept", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_intercept_stable",
            workerId: "worker_intercept_stable",
            cacheMode: "stable",
          }),
        ),
      }),
      env(loader),
      ctx(),
    );

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toEqual({
      error: "intercept egress requires cacheMode one-shot",
    });
    expect(loader.loadCalls).toHaveLength(0);
    expect(loader.getCalls).toHaveLength(0);
  });

  it("stores status, logs, list metadata, and stop metadata", async () => {
    const loader = new MockLoader();
    const runs = new MockKVNamespace();
    const coordinator = new MockRunCoordinatorNamespace();
    const testEnv = env(loader, runs, coordinator);
    const testCtx = ctx();
    loader.nextResponse = new Response("retained response body");
    const create = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_meta", metadata: { team: "platform" } })),
      }),
      testEnv,
      testCtx,
    );
    expect(create.status).toBe(200);
    await expect(create.clone().json()).resolves.toMatchObject({ body: "retained response body" });
    expect(runs.putCalls.map((call) => call.key)).toEqual(["runs:run_meta"]);
    const completion = coordinator.requests.find(
      (request) => request.id === "run_meta" && request.path === "/complete",
    );
    const completionBody = completion?.body as {
      record: { executionId: string; result: { status: number; body?: string } };
    };
    expect(completionBody.record.result.status).toBe(200);
    expect(completionBody.record.result).not.toHaveProperty("body");
    const activeIndex = coordinator.requests.find(
      (request) => request.path === "/index/set" && isRunIndexObject(request.id),
    );
    const activeIndexBody = activeIndex?.body as {
      record: { id: string; result?: unknown; logs: unknown[] };
    };
    expect(activeIndexBody.record).toMatchObject({ id: "run_meta", state: "running", logs: [] });
    expect(activeIndexBody.record).not.toHaveProperty("result");
    expect(
      coordinator.requests.some(
        (request) => request.path === "/index/delete" && isRunIndexObject(request.id),
      ),
    ).toBe(true);

    const status = await worker.fetch(authedRequest("/v1/runs/run_meta"), testEnv, testCtx);
    await expect(status.json()).resolves.toMatchObject({
      id: "run_meta",
      status: "succeeded",
      body: "retained response body",
      metadata: { team: "platform" },
      logEvents: [
        { level: "info", message: "run started" },
        { level: "info", message: "run completed with HTTP 200" },
      ],
    });

    const logs = await worker.fetch(authedRequest("/v1/runs/run_meta/logs"), testEnv, testCtx);
    await expect(logs.json()).resolves.toMatchObject({
      id: "run_meta",
      logs: [{ message: "run started" }, { message: "run completed with HTTP 200" }],
    });

    const tailed = await coordinator.get("run_meta").fetch("https://run-coordinator/log", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        executionId: completionBody.record.executionId,
        log: {
          level: "info",
          message: "dynamic worker tail event received",
          time: "2026-06-12T21:00:02.000Z",
          eventCount: 2,
        },
      }),
    });
    expect(tailed.status).toBe(200);
    const tailedLogs = await worker.fetch(
      authedRequest("/v1/runs/run_meta/logs"),
      testEnv,
      testCtx,
    );
    await expect(tailedLogs.json()).resolves.toMatchObject({
      logs: [
        { message: "run started" },
        { message: "run completed with HTTP 200" },
        { message: "dynamic worker tail event received", eventCount: 2 },
      ],
    });

    const list = await worker.fetch(authedRequest("/v1/runs"), testEnv, testCtx);
    expect(runs.listCalls).toEqual([{ prefix: "runs:" }]);
    const listBody = (await list.json()) as { runs: Array<{ id: string; status: string }> };
    expect(listBody.runs).toEqual(
      expect.arrayContaining([
        expect.objectContaining({
          id: "run_meta",
          status: "succeeded",
          metadata: { team: "platform" },
        }),
      ]),
    );

    const stopped = await worker.fetch(
      authedRequest("/v1/runs/run_meta", { method: "DELETE" }),
      testEnv,
      testCtx,
    );
    await expect(stopped.json()).resolves.toMatchObject({
      id: "run_meta",
      status: "stopped",
    });
    expect(runs.deleteCalls).toEqual(["runs:run_meta"]);
    expect(runs.values.has("runs:run_meta")).toBe(false);

    const missing = await worker.fetch(authedRequest("/v1/runs/run_meta"), testEnv, testCtx);
    expect(missing.status).toBe(404);
  });

  it("stops retained KV-only metadata from before coordinator rollout", async () => {
    const runs = new MockKVNamespace();
    const coordinator = new MockRunCoordinatorNamespace();
    const testEnv = env(new MockLoader(), runs, coordinator);
    runs.values.set(
      "runs:legacy_kv_only",
      JSON.stringify({
        id: "legacy_kv_only",
        executionId: "legacy_execution",
        workerId: "legacy_worker",
        state: "succeeded",
        cacheMode: "one-shot",
        egress: "blocked",
        createdAt: "2026-06-12T20:00:00.000Z",
        startedAt: "2026-06-12T20:00:00.000Z",
        completedAt: "2026-06-12T20:01:00.000Z",
        logs: [],
      }),
    );

    const stopped = await worker.fetch(
      authedRequest("/v1/runs/legacy_kv_only", { method: "DELETE" }),
      testEnv,
      ctx(),
    );

    expect(stopped.status).toBe(200);
    await expect(stopped.json()).resolves.toMatchObject({
      id: "legacy_kv_only",
      status: "stopped",
    });
    expect(runs.values.has("runs:legacy_kv_only")).toBe(false);
  });

  it("bounds records stored by the run coordinator and global index", async () => {
    const coordinator = new MockRunCoordinatorNamespace();
    const metadata = Object.fromEntries(
      Array.from({ length: 100 }, (_, index) => [`metadata-${index}`, "x".repeat(5000)]),
    );
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_bounded_coordination", metadata })),
      }),
      env(new MockLoader(), undefined, coordinator),
      ctx(),
    );
    expect(response.status).toBe(200);

    const start = coordinator.requests.find(
      (request) => request.id === "run_bounded_coordination" && request.path === "/start",
    );
    expect(start).toBeDefined();
    const startRecord = (start!.body as { record: { metadata: Record<string, string> } }).record;
    expect(Object.keys(startRecord.metadata)).toHaveLength(16);
    expect(Math.max(...Object.values(startRecord.metadata).map((value) => value.length))).toBe(512);
    expect(JSON.stringify(startRecord).length).toBeLessThan(1024 * 1024);

    const indexSet = coordinator.requests.find(
      (request) => isRunIndexObject(request.id) && request.path === "/index/set",
    );
    expect(indexSet).toBeDefined();
    const indexRecord = (indexSet!.body as { record: { metadata: Record<string, string> } }).record;
    expect(Object.keys(indexRecord.metadata)).toHaveLength(16);
    expect(JSON.stringify(indexRecord).length).toBeLessThan(1024 * 1024);
  });

  it("publishes a stop tombstone before fallible KV cleanup", async () => {
    const runs = new MockKVNamespace();
    const coordinator = new MockRunCoordinatorNamespace();
    const testEnv = env(new MockLoader(), runs, coordinator);
    const created = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_stop_cleanup_failure" })),
      }),
      testEnv,
      ctx(),
    );
    expect(created.status).toBe(200);
    expect(runs.values.has("runs:run_stop_cleanup_failure")).toBe(true);

    runs.deleteError = new Error("KV DELETE failed");
    const stopCtx = ctx();
    const failedStop = await worker.fetch(
      authedRequest("/v1/runs/run_stop_cleanup_failure", { method: "DELETE" }),
      testEnv,
      stopCtx,
    );
    expect(failedStop.status).toBe(503);
    await expect(failedStop.json()).resolves.toEqual({
      error: "run deletion cleanup is pending",
    });
    await Promise.all(stopCtx.waitUntil.mock.calls.map(([promise]) => promise));

    const status = await worker.fetch(
      authedRequest("/v1/runs/run_stop_cleanup_failure"),
      testEnv,
      ctx(),
    );
    expect(status.status).toBe(404);
    const list = await worker.fetch(authedRequest("/v1/runs"), testEnv, ctx());
    const listed = (await list.json()) as { runs: Array<{ id: string }> };
    expect(listed.runs).not.toContainEqual(
      expect.objectContaining({ id: "run_stop_cleanup_failure" }),
    );

    vi.advanceTimersByTime(10 * 60 * 1000 + 1);
    const laterStatus = await worker.fetch(
      authedRequest("/v1/runs/run_stop_cleanup_failure"),
      testEnv,
      ctx(),
    );
    expect(laterStatus.status).toBe(404);
    const laterList = await worker.fetch(authedRequest("/v1/runs"), testEnv, ctx());
    const laterListed = (await laterList.json()) as { runs: Array<{ id: string }> };
    expect(laterListed.runs).not.toContainEqual(
      expect.objectContaining({ id: "run_stop_cleanup_failure" }),
    );

    const blockedReplacement = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_stop_cleanup_failure" })),
      }),
      testEnv,
      ctx(),
    );
    expect(blockedReplacement.status).toBe(409);
    await expect(blockedReplacement.json()).resolves.toEqual({
      error: "run deletion is pending",
    });

    runs.deleteError = undefined;
    await coordinator.alarm("run_stop_cleanup_failure");
    expect(runs.values.has("runs:run_stop_cleanup_failure")).toBe(false);

    const replacement = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_stop_cleanup_failure" })),
      }),
      testEnv,
      ctx(),
    );
    expect(replacement.status).toBe(200);
    expect(runs.values.has("runs:run_stop_cleanup_failure")).toBe(true);
  });

  it("removes metadata after an unretained completed run", async () => {
    const runs = new MockKVNamespace();
    const testEnv = env(new MockLoader(), runs);
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_ephemeral", retainMetadata: false })),
      }),
      testEnv,
      ctx(),
    );

    expect(response.status).toBe(200);
    const missing = await worker.fetch(authedRequest("/v1/runs/run_ephemeral"), testEnv, ctx());
    expect(missing.status).toBe(404);
    expect(runs.putCalls).toEqual([]);
    expect(runs.getCalls).toEqual(["runs:run_ephemeral"]);
    expect(runs.deleteCalls).toEqual([]);
    expect(runs.values.has("runs:run_ephemeral")).toBe(false);
  });

  it("removes stale metadata before releasing an unretained run", async () => {
    const runs = new MockKVNamespace();
    runs.values.set(
      "runs:run_ephemeral_stale",
      JSON.stringify({
        id: "run_ephemeral_stale",
        executionId: "old_execution",
        workerId: "old_worker",
        state: "succeeded",
        cacheMode: "one-shot",
        egress: "blocked",
        createdAt: "2026-06-12T20:00:00.000Z",
        startedAt: "2026-06-12T20:00:00.000Z",
        completedAt: "2026-06-12T20:01:00.000Z",
        logs: [],
      }),
    );
    const testEnv = env(new MockLoader(), runs);
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_ephemeral_stale",
            workerId: "new_worker",
            retainMetadata: false,
          }),
        ),
      }),
      testEnv,
      ctx(),
    );

    expect(response.status).toBe(200);
    expect(runs.deleteCalls).toEqual(["runs:run_ephemeral_stale"]);
    const missing = await worker.fetch(
      authedRequest("/v1/runs/run_ephemeral_stale"),
      testEnv,
      ctx(),
    );
    expect(missing.status).toBe(404);
  });

  it("preserves metadata from a later generation when an unretained run completes", async () => {
    const runs = new MockKVNamespace();
    runs.values.set(
      "runs:run_ephemeral_newer",
      JSON.stringify({
        id: "run_ephemeral_newer",
        executionId: "newer_execution",
        workerId: "newer_worker",
        state: "succeeded",
        cacheMode: "one-shot",
        egress: "blocked",
        createdAt: "2026-06-12T22:00:00.000Z",
        startedAt: "2026-06-12T22:00:00.000Z",
        completedAt: "2026-06-12T22:01:00.000Z",
        logs: [],
      }),
    );
    const testEnv = env(new MockLoader(), runs);
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_ephemeral_newer",
            workerId: "current_worker",
            retainMetadata: false,
          }),
        ),
      }),
      testEnv,
      ctx(),
    );

    expect(response.status).toBe(200);
    expect(runs.deleteCalls).toEqual([]);
    expect(JSON.parse(runs.values.get("runs:run_ephemeral_newer")!)).toMatchObject({
      executionId: "newer_execution",
    });
  });

  it("retries unretained coordinator cleanup when stale KV deletion fails", async () => {
    const runs = new MockKVNamespace();
    runs.values.set(
      "runs:run_ephemeral_cleanup_failure",
      JSON.stringify({
        id: "run_ephemeral_cleanup_failure",
        executionId: "old_execution",
        workerId: "old_worker",
        state: "succeeded",
        cacheMode: "one-shot",
        egress: "blocked",
        createdAt: "2026-06-12T20:00:00.000Z",
        startedAt: "2026-06-12T20:00:00.000Z",
        completedAt: "2026-06-12T20:01:00.000Z",
        logs: [],
      }),
    );
    runs.deleteError = new Error("KV DELETE failed");
    const loader = new MockLoader();
    const coordinator = new MockRunCoordinatorNamespace();
    const testEnv = env(loader, runs, coordinator);

    await expect(
      worker.fetch(
        authedRequest("/v1/runs", {
          method: "POST",
          body: JSON.stringify(
            runPayload({
              id: "run_ephemeral_cleanup_failure",
              workerId: "current_worker",
              retainMetadata: false,
            }),
          ),
        }),
        testEnv,
        ctx(),
      ),
    ).rejects.toThrow("KV DELETE failed");
    expect(loader.loadCalls).toHaveLength(1);

    const status = await worker.fetch(
      authedRequest("/v1/runs/run_ephemeral_cleanup_failure"),
      testEnv,
      ctx(),
    );
    expect(status.status).toBe(404);
    expect(
      coordinator.requests.find(
        (request) => request.id === "run_ephemeral_cleanup_failure" && request.path === "/complete",
      )?.body,
    ).toMatchObject({
      cleanupPending: true,
      release: false,
    });
    expect(runs.deleteCalls).toHaveLength(2);

    const blockedReplacement = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_ephemeral_cleanup_failure",
            workerId: "replacement_worker",
            retainMetadata: false,
          }),
        ),
      }),
      testEnv,
      ctx(),
    );
    expect(blockedReplacement.status).toBe(409);
    await expect(blockedReplacement.json()).resolves.toEqual({
      error: "run deletion is pending",
    });

    runs.deleteError = undefined;
    await coordinator.alarm("run_ephemeral_cleanup_failure");
    expect(runs.deleteCalls).toHaveLength(3);
    expect(runs.values.has("runs:run_ephemeral_cleanup_failure")).toBe(false);

    const replacement = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_ephemeral_cleanup_failure",
            workerId: "replacement_worker",
            retainMetadata: false,
          }),
        ),
      }),
      testEnv,
      ctx(),
    );
    expect(replacement.status).toBe(200);
  });

  it("preserves newer metadata during deferred unretained cleanup", async () => {
    const runs = new MockKVNamespace();
    runs.values.set(
      "runs:run_deferred_cleanup_newer",
      JSON.stringify({
        id: "run_deferred_cleanup_newer",
        executionId: "old_execution",
        workerId: "old_worker",
        state: "succeeded",
        cacheMode: "one-shot",
        egress: "blocked",
        createdAt: "2026-06-12T20:00:00.000Z",
        startedAt: "2026-06-12T20:00:00.000Z",
        completedAt: "2026-06-12T20:01:00.000Z",
        logs: [],
      }),
    );
    runs.deleteError = new Error("KV DELETE failed");
    const coordinator = new MockRunCoordinatorNamespace();
    const testEnv = env(new MockLoader(), runs, coordinator);

    await expect(
      worker.fetch(
        authedRequest("/v1/runs", {
          method: "POST",
          body: JSON.stringify(
            runPayload({
              id: "run_deferred_cleanup_newer",
              workerId: "current_worker",
              retainMetadata: false,
            }),
          ),
        }),
        testEnv,
        ctx(),
      ),
    ).rejects.toThrow("KV DELETE failed");

    runs.deleteError = undefined;
    runs.values.set(
      "runs:run_deferred_cleanup_newer",
      JSON.stringify({
        id: "run_deferred_cleanup_newer",
        executionId: "newer_execution",
        workerId: "newer_worker",
        state: "succeeded",
        cacheMode: "stable",
        egress: "blocked",
        createdAt: "2026-06-12T23:00:00.000Z",
        startedAt: "2026-06-12T23:00:00.000Z",
        completedAt: "2026-06-12T23:01:00.000Z",
        logs: [],
      }),
    );
    await coordinator.alarm("run_deferred_cleanup_newer");

    expect(JSON.parse(runs.values.get("runs:run_deferred_cleanup_newer")!)).toMatchObject({
      executionId: "newer_execution",
    });
    const status = await worker.fetch(
      authedRequest("/v1/runs/run_deferred_cleanup_newer"),
      testEnv,
      ctx(),
    );
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({
      id: "run_deferred_cleanup_newer",
      workerId: "newer_worker",
      status: "succeeded",
    });
  });

  it("does not resurrect stale KV metadata deleted by another isolate", async () => {
    const runs = new MockKVNamespace();
    const coordinator = new MockRunCoordinatorNamespace();
    const testEnv = env(new MockLoader(), runs, coordinator);
    const created = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_cross_isolate_delete" })),
      }),
      testEnv,
      ctx(),
    );
    expect(created.status).toBe(200);
    const staleKV = runs.values.get("runs:run_cross_isolate_delete");
    expect(staleKV).toBeDefined();

    const deleted = await coordinator
      .get("run_cross_isolate_delete")
      .fetch("https://run-coordinator/delete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          acknowledgedComplete: false,
          runId: "run_cross_isolate_delete",
        }),
      });
    expect(deleted.status).toBe(200);
    runs.values.set("runs:run_cross_isolate_delete", staleKV!);

    const status = await worker.fetch(
      authedRequest("/v1/runs/run_cross_isolate_delete"),
      testEnv,
      ctx(),
    );
    expect(status.status).toBe(404);
    const list = await worker.fetch(authedRequest("/v1/runs"), testEnv, ctx());
    const listed = (await list.json()) as { runs: Array<{ id: string }> };
    expect(listed.runs).not.toContainEqual(
      expect.objectContaining({ id: "run_cross_isolate_delete" }),
    );
  });

  it("prefers retained terminal KV metadata over a stale active index record", async () => {
    const runs = new MockKVNamespace();
    const coordinator = new MockRunCoordinatorNamespace();
    const terminalRecord = {
      id: "run_terminal_precedence",
      workerId: "worker_terminal_precedence",
      state: "succeeded",
      cacheMode: "stable",
      egress: "blocked",
      createdAt: "2026-06-12T21:00:00.000Z",
      startedAt: "2026-06-12T21:00:00.000Z",
      completedAt: "2026-06-12T21:00:01.000Z",
      durationMs: 1000,
      logs: [],
    };
    runs.values.set("runs:run_terminal_precedence", JSON.stringify(terminalRecord));
    await coordinator.get("__crabbox/run-index__").fetch("https://run-coordinator/index/set", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        generation: 1,
        record: {
          ...terminalRecord,
          state: "running",
          completedAt: undefined,
          durationMs: undefined,
          expiresAt: new Date(Date.now() + 60_000).toISOString(),
        },
      }),
    });

    const list = await worker.fetch(
      authedRequest("/v1/runs"),
      env(new MockLoader(), runs, coordinator),
      ctx(),
    );
    const body = (await list.json()) as { runs: Array<{ id: string; status: string }> };
    expect(body.runs).toContainEqual(
      expect.objectContaining({ id: "run_terminal_precedence", status: "succeeded" }),
    );
  });

  it("prefers an overlapping active legacy run over older terminal KV metadata", async () => {
    const runs = new MockKVNamespace();
    const coordinator = new MockRunCoordinatorNamespace();
    const terminalRecord = {
      id: "legacy_reused_precedence",
      executionId: "newer_completed_execution",
      workerId: "legacy_reused_precedence",
      state: "succeeded",
      cacheMode: "stable",
      egress: "blocked",
      createdAt: "2026-06-12T21:00:02.000Z",
      startedAt: "2026-06-12T21:00:02.000Z",
      completedAt: "2026-06-12T21:00:03.000Z",
      durationMs: 3000,
      logs: [],
    };
    runs.values.set("runs:legacy_reused_precedence", JSON.stringify(terminalRecord));
    await coordinator.get("__crabbox/run-index__").fetch("https://run-coordinator/index/set", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        generation: 1,
        record: {
          ...terminalRecord,
          executionId: "older_active_execution",
          state: "running",
          createdAt: "2026-06-12T21:00:00.000Z",
          startedAt: "2026-06-12T21:00:00.000Z",
          completedAt: undefined,
          durationMs: undefined,
          expiresAt: new Date(Date.now() + 60_000).toISOString(),
        },
      }),
    });

    const list = await worker.fetch(
      authedRequest("/v1/runs"),
      env(new MockLoader(), runs, coordinator),
      ctx(),
    );
    const body = (await list.json()) as { runs: Array<{ id: string; status: string }> };
    expect(body.runs).toContainEqual(
      expect.objectContaining({ id: "legacy_reused_precedence", status: "running" }),
    );
  });

  it("prefers authoritative terminal coordinator state over stale KV metadata", async () => {
    const runs = new MockKVNamespace();
    const coordinator = new MockRunCoordinatorNamespace();
    const runId = "run_stale_kv_status";
    runs.values.set(
      `runs:${runId}`,
      JSON.stringify({
        id: runId,
        executionId: "old_execution",
        workerId: "worker_stale_kv_status",
        state: "failed",
        cacheMode: "stable",
        egress: "blocked",
        createdAt: "2026-06-12T20:00:00.000Z",
        startedAt: "2026-06-12T20:00:00.000Z",
        completedAt: "2026-06-12T20:00:01.000Z",
        metadata: { generation: "old" },
        logs: [],
      }),
    );
    const currentRecord = {
      id: runId,
      executionId: "new_execution",
      workerId: "worker_stale_kv_status",
      state: "running",
      cacheMode: "stable",
      egress: "blocked",
      createdAt: "2026-06-12T21:00:00.000Z",
      startedAt: "2026-06-12T21:00:00.000Z",
      metadata: { generation: "new" },
      logs: [],
    };
    const stub = coordinator.get(runId);
    await stub.fetch("https://run-coordinator/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        executionId: currentRecord.executionId,
        expiresAt: null,
        legacyReusableID: false,
        record: currentRecord,
      }),
    });
    await stub.fetch("https://run-coordinator/complete", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        executionId: currentRecord.executionId,
        release: false,
        record: { ...currentRecord, state: "succeeded" },
      }),
    });

    const status = await worker.fetch(
      authedRequest(`/v1/runs/${runId}`),
      env(new MockLoader(), runs, coordinator),
      ctx(),
    );
    await expect(status.json()).resolves.toMatchObject({
      id: runId,
      status: "succeeded",
      metadata: { generation: "new" },
    });
  });

  it("writes keep-on-failure metadata only when the run fails", async () => {
    const successfulRuns = new MockKVNamespace();
    const successfulEnv = env(new MockLoader(), successfulRuns);
    const succeeded = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_keep_on_failure_success",
            retainMetadata: false,
            retainOnFailure: true,
          }),
        ),
      }),
      successfulEnv,
      ctx(),
    );
    expect(succeeded.status).toBe(200);
    expect(successfulRuns.putCalls).toEqual([]);

    const failedRuns = new MockKVNamespace();
    const failedLoader = new MockLoader();
    failedLoader.nextResponse = new Response("failed", { status: 500 });
    const failed = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "run_keep_on_failure_failed",
            retainMetadata: false,
            retainOnFailure: true,
          }),
        ),
      }),
      env(failedLoader, failedRuns),
      ctx(),
    );
    expect(failed.status).toBe(200);
    expect(failedRuns.putCalls.map((call) => call.key)).toEqual([
      "runs:run_keep_on_failure_failed",
    ]);
  });

  it("keeps coordinator status when retained KV persistence fails", async () => {
    const runs = new MockKVNamespace();
    runs.putError = new Error("KV PUT failed");
    const testEnv = env(new MockLoader(), runs);
    await expect(
      worker.fetch(
        authedRequest("/v1/runs", {
          method: "POST",
          body: JSON.stringify(runPayload({ id: "run_kv_failure" })),
        }),
        testEnv,
        ctx(),
      ),
    ).rejects.toThrow("KV PUT failed");

    const status = await worker.fetch(authedRequest("/v1/runs/run_kv_failure"), testEnv, ctx());
    expect(status.status).toBe(200);
    await expect(status.json()).resolves.toMatchObject({
      id: "run_kv_failure",
      status: "succeeded",
    });
  });

  it("expires interrupted running metadata and permits a replacement run", async () => {
    const runs = new MockKVNamespace();
    runs.values.set(
      "runs:stale_running",
      JSON.stringify({
        id: "stale_running",
        workerId: "stale_worker",
        state: "running",
        cacheMode: "stable",
        egress: "blocked",
        createdAt: "2026-06-12T20:00:00.000Z",
        startedAt: "2026-06-12T20:00:00.000Z",
        expiresAt: "2026-06-12T20:15:00.000Z",
        logs: [],
      }),
    );
    const loader = new MockLoader();
    const testEnv = env(loader, runs);

    const missing = await worker.fetch(authedRequest("/v1/runs/stale_running"), testEnv, ctx());
    expect(missing.status).toBe(404);
    expect(runs.values.has("runs:stale_running")).toBe(false);

    const replacement = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(
          runPayload({
            id: "stale_running",
            workerId: "stale_worker",
          }),
        ),
      }),
      testEnv,
      ctx(),
    );
    expect(replacement.status).toBe(200);
  });

  it("deletes an older retained result when a reused legacy run expires", async () => {
    const runs = new MockKVNamespace();
    const coordinator = new MockRunCoordinatorNamespace();
    const testEnv = env(new MockLoader(), runs, coordinator);
    const runId = "legacy_expired_reuse";
    runs.values.set(
      `runs:${runId}`,
      JSON.stringify({
        id: runId,
        executionId: "previous_execution",
        workerId: runId,
        state: "succeeded",
        cacheMode: "stable",
        egress: "blocked",
        createdAt: "2026-06-12T20:00:00.000Z",
        startedAt: "2026-06-12T20:00:00.000Z",
        completedAt: "2026-06-12T20:01:00.000Z",
        logs: [],
      }),
    );
    const expiresAt = Date.now() + 1000;
    await coordinator.get(runId).fetch("https://run-coordinator/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        executionId: "interrupted_execution",
        expiresAt,
        legacyConcurrentReuse: true,
        legacyReusableID: true,
        record: {
          id: runId,
          executionId: "interrupted_execution",
          workerId: runId,
          state: "running",
          cacheMode: "stable",
          egress: "blocked",
          createdAt: "2026-06-12T21:00:00.000Z",
          startedAt: "2026-06-12T21:00:00.000Z",
          expiresAt: new Date(expiresAt).toISOString(),
          logs: [],
        },
      }),
    });

    vi.setSystemTime(new Date(expiresAt + 1));
    await coordinator.alarm(runId);
    expect(runs.values.has(`runs:${runId}`)).toBe(false);

    vi.advanceTimersByTime(10 * 60 * 1000 + 1);
    await coordinator.alarm(runId);
    const status = await worker.fetch(authedRequest(`/v1/runs/${runId}`), testEnv, ctx());
    expect(status.status).toBe(404);
  });

  it("preserves active-run logs across status reads", async () => {
    let releaseFetch!: () => void;
    const loader = new MockLoader();
    loader.nextFetchGate = new Promise((resolve) => {
      releaseFetch = resolve;
    });
    const testEnv = env(loader, new MockKVNamespace());
    const testCtx = ctx();
    const createPromise = worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_active_logs" })),
      }),
      testEnv,
      testCtx,
    );

    const dynamicWorker = await loader.workerCreated;
    await dynamicWorker.fetchStarted;
    const active = await worker.fetch(authedRequest("/v1/runs/run_active_logs"), testEnv, testCtx);
    await expect(active.json()).resolves.toMatchObject({ status: "running" });

    releaseFetch();
    await createPromise;
    const completed = await worker.fetch(
      authedRequest("/v1/runs/run_active_logs"),
      testEnv,
      testCtx,
    );
    await expect(completed.json()).resolves.toMatchObject({
      status: "succeeded",
      logEvents: [{ message: "run started" }, { message: "run completed with HTTP 200" }],
    });
  });

  it("rejects stop while a run is active, then stops its completed metadata", async () => {
    let releaseFetch!: () => void;
    const loader = new MockLoader();
    loader.nextFetchGate = new Promise((resolve) => {
      releaseFetch = resolve;
    });
    const runs = new MockKVNamespace();
    const testEnv = env(loader, runs);
    const testCtx = ctx();
    const createPromise = worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_active" })),
      }),
      testEnv,
      testCtx,
    );

    const dynamicWorker = await loader.workerCreated;
    await dynamicWorker.fetchStarted;
    expect(runs.putCalls).toEqual([]);

    const active = await worker.fetch(authedRequest("/v1/runs/run_active"), testEnv, testCtx);
    const activeBody = (await active.json()) as Record<string, unknown>;
    expect(active.status).toBe(200);
    expect(activeBody).toMatchObject({ id: "run_active", status: "running" });
    expect(activeBody).not.toHaveProperty("exitCode");
    expect(activeBody).not.toHaveProperty("completedAt");
    expect(activeBody).not.toHaveProperty("durationMs");

    const stopped = await worker.fetch(
      authedRequest("/v1/runs/run_active", { method: "DELETE" }),
      testEnv,
      testCtx,
    );
    expect(stopped.status).toBe(409);
    await expect(stopped.json()).resolves.toEqual({
      error: "active runs cannot be stopped; wait for completion",
    });
    const acknowledgedStop = await worker.fetch(
      authedRequest("/v1/runs/run_active?acknowledgedComplete=true", {
        method: "DELETE",
      }),
      testEnv,
      testCtx,
    );
    expect(acknowledgedStop.status).toBe(409);
    await expect(acknowledgedStop.json()).resolves.toEqual({
      error: "active runs cannot be stopped; wait for completion",
    });

    const duplicate = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_active" })),
      }),
      testEnv,
      testCtx,
    );
    expect(duplicate.status).toBe(409);
    await expect(duplicate.json()).resolves.toEqual({ error: "run id already exists" });

    releaseFetch();
    const completed = await createPromise;
    expect(completed.status).toBe(200);
    await expect(completed.json()).resolves.toMatchObject({
      id: "run_active",
      status: "succeeded",
      exitCode: 0,
    });

    const terminalStop = await worker.fetch(
      authedRequest("/v1/runs/run_active", { method: "DELETE" }),
      testEnv,
      testCtx,
    );
    expect(terminalStop.status).toBe(200);
    await expect(terminalStop.json()).resolves.toMatchObject({
      id: "run_active",
      status: "stopped",
    });
    const missing = await worker.fetch(authedRequest("/v1/runs/run_active"), testEnv, testCtx);
    expect(missing.status).toBe(404);
    expect(runs.values.has("runs:run_active")).toBe(false);
  });

  it("deletes acknowledged completed metadata despite a stale running KV record", async () => {
    const runs = new MockKVNamespace();
    runs.values.set(
      "runs:acknowledged_complete",
      JSON.stringify({
        id: "acknowledged_complete",
        workerId: "acknowledged_worker",
        state: "running",
        cacheMode: "stable",
        egress: "blocked",
        createdAt: "2026-06-12T21:00:00.000Z",
        startedAt: "2026-06-12T21:00:00.000Z",
        expiresAt: "2026-06-12T21:15:00.000Z",
        logs: [],
      }),
    );
    const testEnv = env(new MockLoader(), runs);

    const response = await worker.fetch(
      authedRequest("/v1/runs/acknowledged_complete?acknowledgedComplete=true", {
        method: "DELETE",
      }),
      testEnv,
      ctx(),
    );

    expect(response.status).toBe(200);
    expect(runs.values.has("runs:acknowledged_complete")).toBe(false);
  });

  it("retries an immediate retained-run delete after KV rate limiting", async () => {
    const runs = new MockKVNamespace();
    const testEnv = env(new MockLoader(), runs);
    const created = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_retry_delete" })),
      }),
      testEnv,
      ctx(),
    );
    expect(created.status).toBe(200);
    expect(runs.putCalls.map((call) => call.key)).toEqual(["runs:run_retry_delete"]);
    runs.deleteCalls.length = 0;
    runs.deleteRateLimitFailures = 1;

    const stopPromise = worker.fetch(
      authedRequest("/v1/runs/run_retry_delete", { method: "DELETE" }),
      testEnv,
      ctx(),
    );
    await vi.advanceTimersByTimeAsync(1100);
    const stopped = await stopPromise;

    expect(stopped.status).toBe(200);
    expect(runs.deleteCalls).toEqual(["runs:run_retry_delete", "runs:run_retry_delete"]);
    expect(runs.values.has("runs:run_retry_delete")).toBe(false);
  });

  it("returns stable errors for invalid and failed runs without leaking secrets", async () => {
    const unsupported = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ egress: "open" })),
      }),
      env(),
      ctx(),
    );
    expect(unsupported.status).toBe(400);
    await expect(unsupported.json()).resolves.toEqual({
      error: "egress must be blocked or intercept",
    });

    const loader = new MockLoader();
    loader.nextError = new Error("runtime saw runner-token");
    const failed = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_failed" })),
      }),
      env(loader),
      ctx(),
    );

    expect(failed.status).toBe(502);
    await expect(failed.json()).resolves.toMatchObject({
      id: "run_failed",
      status: "failed",
      exitCode: 1,
      stderr: "runtime saw [redacted]",
      error: { message: "runtime saw [redacted]" },
    });
  });

  it("keeps Dynamic Workers Wrangler config separate from Cloudflare Containers", () => {
    const config = readFileSync(
      new URL("../wrangler.cloudflare-dynamic-workers.jsonc", import.meta.url),
      "utf8",
    );

    expect(config).toContain('"main": "src/cloudflare-dynamic-worker-runner.ts"');
    expect(config).toContain('"worker_loaders"');
    expect(config).toContain('"binding": "LOADER"');
    expect(config).toContain('"kv_namespaces"');
    expect(config).toContain('"binding": "RUNS"');
    expect(config).toContain('"durable_objects"');
    expect(config).toContain('"name": "RUN_COORDINATOR"');
    expect(config).toContain('"new_sqlite_classes": ["DynamicWorkerRunCoordinator"]');
    expect(config).not.toContain('"containers"');
  });
});
