import { beforeEach, describe, expect, it, vi } from "vitest";

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
  readonly ctx: { storage: MemoryStorage };
  readonly schedules: Array<{ when: Date | number; callback: string }> = [];
  deletedSchedules: string[] = [];
  destroyed = false;
  stopped = false;
  started = false;
  renewedActivityTimeouts = 0;
  execResponse: Response | undefined;

  constructor(ctx: { storage: MemoryStorage }) {
    this.ctx = ctx;
  }

  async startAndWaitForPorts(): Promise<void> {
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
  id?: string;
  request?: Request;
};

function envWithCapturedInternalRequest(
  capture: CapturedInternalRequest,
): Parameters<typeof worker.fetch>[1] {
  return {
    CRABBOX_RUNNER_TOKEN: "runner-token",
    Sandbox: {
      get(id: string) {
        capture.id = id;
        return {
          async fetch(request: Request): Promise<Response> {
            capture.request = request;
            return Response.json({ ok: true });
          },
        };
      },
    },
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

describe("Cloudflare runner lifecycle", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-05-13T18:00:00Z"));
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
    expect(capture.id).toBe("cbx_test");
    expect(capture.request?.headers.get("Authorization")).toBeNull();
    expect(capture.request?.headers.get("Content-Type")).toBe("application/json");
    if (capture.request === undefined) {
      throw new Error("missing captured request");
    }
    await expect(capture.request.json()).resolves.toMatchObject({
      id: "cbx_test",
      workdir: "/workspace/repo",
      labels: { repo: "my-app" },
    });
  });

  it("does not forward edge auth headers to durable object proxy requests", async () => {
    const capture: CapturedInternalRequest = {};
    const response = await worker.fetch(
      new Request("https://runner.example/v1/sandboxes/cbx_test/exec-stream", {
        method: "POST",
        headers: {
          Authorization: "Bearer runner-token",
          "Content-Type": "application/json",
        },
        body: JSON.stringify({ command: "echo hi", cwd: "/workspace/repo" }),
      }),
      envWithCapturedInternalRequest(capture),
    );

    expect(response.status).toBe(200);
    expect(capture.id).toBe("cbx_test");
    expect(capture.request?.headers.get("Authorization")).toBeNull();
    expect(capture.request?.headers.get("Content-Type")).toBeNull();
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
