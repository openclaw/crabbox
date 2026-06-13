import { readFileSync } from "node:fs";

import { beforeEach, describe, expect, it, vi } from "vitest";

const { default: worker } = await import("../src/cloudflare-dynamic-worker-runner");

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
  code: WorkerCode | undefined;
  entrypointOptions: EntrypointOptions | undefined;
  request: Request | undefined;
  response = new Response("ok", {
    status: 200,
    headers: { "X-Run": "ok" },
  });
  error: Error | undefined;

  constructor(codeFactory: () => Promise<WorkerCode>) {
    this.codeFactory = codeFactory;
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
        if (this.error) throw this.error;
        return this.response;
      },
    };
  }
}

class MockLoader {
  readonly loadCalls: WorkerCode[] = [];
  readonly getCalls: Array<{ id: string }> = [];
  worker: MockDynamicWorker | undefined;
  nextError: Error | undefined;
  nextResponse: Response | undefined;

  load(code: WorkerCode): MockDynamicWorker {
    this.loadCalls.push(code);
    return this.makeWorker(async () => code);
  }

  get(id: string, callback: () => Promise<WorkerCode>): MockDynamicWorker {
    this.getCalls.push({ id });
    return this.makeWorker(callback);
  }

  private makeWorker(callback: () => Promise<WorkerCode>): MockDynamicWorker {
    const dynamicWorker = new MockDynamicWorker(callback);
    if (this.nextResponse) dynamicWorker.response = this.nextResponse;
    if (this.nextError) dynamicWorker.error = this.nextError;
    this.worker = dynamicWorker;
    return dynamicWorker;
  }
}

type TestEnv = Parameters<typeof worker.fetch>[1] & {
  LOADER?: MockLoader;
  RUNS?: MockKVNamespace;
};

class MockKVNamespace {
  readonly values = new Map<string, string>();
  readonly getCalls: string[] = [];
  readonly putCalls: Array<{ key: string; value: string }> = [];
  readonly deleteCalls: string[] = [];
  readonly listCalls: Array<{ prefix?: string }> = [];

  async get(key: string): Promise<string | null> {
    this.getCalls.push(key);
    return this.values.get(key) ?? null;
  }

  async put(key: string, value: string): Promise<void> {
    this.putCalls.push({ key, value });
    this.values.set(key, value);
  }

  async delete(key: string): Promise<void> {
    this.deleteCalls.push(key);
    this.values.delete(key);
  }

  async list(options: { prefix?: string } = {}): Promise<{ keys: Array<{ name: string }> }> {
    this.listCalls.push(options);
    const keys = [...this.values.keys()]
      .filter((key) => options.prefix === undefined || key.startsWith(options.prefix))
      .map((name) => ({ name }));
    return { keys };
  }
}

function env(loader = new MockLoader(), runs?: MockKVNamespace): TestEnv {
  return {
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN: "runner-token",
    LOADER: loader,
    RUNS: runs,
  };
}

function ctx(gateway: unknown = { binding: "gateway" }, tailer: unknown = { binding: "tailer" }) {
  return {
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

  it("reports a missing loader binding from readiness", async () => {
    const testEnv = env();
    delete testEnv.LOADER;
    const response = await worker.fetch(authedRequest("/v1/readiness"), testEnv, ctx());

    expect(response.status).toBe(503);
    await expect(response.json()).resolves.toEqual({
      ok: false,
      runner: "cloudflare-dynamic-workers",
      error: "missing loader binding: LOADER",
      missing: ["LOADER"],
    });
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

  it("uses the Go provider id as the explicit Dynamic Worker cache identity", async () => {
    const loader = new MockLoader();
    const response = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify({
          id: "explicit-worker",
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
      id: "explicit-worker",
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
      props: { runId: "run_intercept", allowHostnames: ["api.example.com"] },
    });
    expect(testCtx.exports.LogTailer).toHaveBeenCalledWith({
      props: { runId: "run_intercept" },
    });
    expect(loader.worker?.code?.globalOutbound).toBe(gateway);
    expect(loader.worker?.code?.tails).toEqual([tailer]);
  });

  it("stores status, logs, list metadata, and stop metadata", async () => {
    const loader = new MockLoader();
    const runs = new MockKVNamespace();
    const testEnv = env(loader, runs);
    const testCtx = ctx();
    const create = await worker.fetch(
      authedRequest("/v1/runs", {
        method: "POST",
        body: JSON.stringify(runPayload({ id: "run_meta", metadata: { team: "platform" } })),
      }),
      testEnv,
      testCtx,
    );
    expect(create.status).toBe(200);
    expect(runs.putCalls.map((call) => call.key)).toContain("runs:run_meta");

    const status = await worker.fetch(authedRequest("/v1/runs/run_meta"), testEnv, testCtx);
    expect(runs.getCalls).toContain("runs:run_meta");
    await expect(status.json()).resolves.toMatchObject({
      id: "run_meta",
      status: "succeeded",
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
    expect(config).not.toContain('"containers"');
    expect(config).not.toContain('"durable_objects"');
  });
});
