import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const { default: worker, CrabboxSandbox } = await import("../src/cloudflare-container-runner");

class MemoryStorage {
  readonly values = new Map<string, unknown>();
  alarm: number | null = null;

  async get<T>(key: string): Promise<T | undefined> {
    return this.values.get(key) as T | undefined;
  }

  async put<T>(key: string, value: T): Promise<void> {
    this.values.set(key, value);
  }

  async setAlarm(when: number | Date): Promise<void> {
    this.alarm = typeof when === "number" ? when : when.getTime();
  }

  async deleteAlarm(): Promise<void> {
    this.alarm = null;
  }
}

type CommandScript = {
  stdout?: string[];
  stderr?: string[];
  exitCode?: number | Promise<number>;
  // Keeps stdout open after exit, like a background descendant holding the pipe.
  holdStdoutOpen?: boolean;
  gate?: Promise<void>;
};

type ExecCall = { cmd: string[]; options: ContainerExecOptions | undefined };

function streamOf(chunks: string[], holdOpen = false): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      if (!holdOpen) controller.close();
    },
  });
}

function process(
  exitCode: number | Promise<number>,
  stdout: ReadableStream | null = null,
  stderr: ReadableStream | null = null,
  stdin: WritableStream | null = null,
  kill: (signal?: number) => void = () => undefined,
): ExecProcess {
  return {
    stdin,
    stdout,
    stderr,
    pid: 1,
    isPty: false,
    exitCode: Promise.resolve(exitCode),
    output: async () => ({ stdout: new ArrayBuffer(0), stderr: new ArrayBuffer(0), exitCode: 0 }),
    kill,
    resize: () => undefined,
  };
}

class MockContainer {
  running = false;
  readonly images: Record<string, string> = {
    default: "registry.cloudflare.com/acct/crabbox@sha256:abc",
  };
  started: ContainerStartupOptions[] = [];
  destroyed = 0;
  failStart = false;
  failCommand: Error | undefined;
  hangingProbes = 0;
  command: CommandScript = { stdout: ["hi\n"], exitCode: 0 };
  uploadExitCode = 0;
  uploadGate: Promise<void> | undefined;
  readonly files = new Map<string, string>();
  readonly calls: ExecCall[] = [];

  start(options?: ContainerStartupOptions): void {
    if (options) this.started.push(options);
    if (!this.failStart) this.running = true;
  }

  async monitor(): Promise<void> {
    if (this.failStart) throw new Error("internal error");
    await new Promise<never>(() => undefined);
  }

  async destroy(): Promise<void> {
    this.destroyed += 1;
    this.running = false;
  }

  readonly kills: number[] = [];
  inactivityTimeouts = 0;

  async setInactivityTimeout(): Promise<void> {
    this.inactivityTimeouts += 1;
  }

  async exec(cmd: string[], options?: ContainerExecOptions): Promise<ExecProcess> {
    this.calls.push({ cmd, options });
    if (!this.running)
      throw new Error("exec() cannot be called on a container that is not running.");
    if (cmd[0] === "true") {
      if (this.hangingProbes > 0) {
        this.hangingProbes -= 1;
        return new Promise<never>(() => undefined);
      }
      return process(0);
    }
    if (cmd[0] === "/bin/sh" && options?.stdin instanceof ReadableStream) {
      this.files.set(cmd[4] ?? "", await new Response(options.stdin).text());
      return process(0, null, streamOf([]));
    }
    if (cmd[0] === "/bin/sh" && options?.stdin === "pipe") {
      const path = cmd[4] ?? "";
      const chunks: string[] = [];
      const decoder = new TextDecoder();
      let closed!: () => void;
      const done = new Promise<void>((resolve) => {
        closed = resolve;
      });
      const stdin = new WritableStream<Uint8Array>({
        write: (chunk) => {
          chunks.push(decoder.decode(chunk));
        },
        close: () => {
          this.files.set(path, chunks.join(""));
          closed();
        },
      });
      const exitCode = (async () => {
        await done;
        await this.uploadGate;
        return this.uploadExitCode;
      })();
      const stderr = this.uploadExitCode === 0 ? [] : ["disk full\n"];
      return process(exitCode, null, streamOf(stderr), stdin);
    }
    if (cmd[0] === "timeout") {
      if (this.failCommand) throw this.failCommand;
      const script = this.command;
      await script.gate;
      return process(
        script.exitCode ?? 0,
        streamOf(script.stdout ?? [], script.holdStdoutOpen),
        streamOf(script.stderr ?? []),
        null,
        (signal) => this.kills.push(signal ?? 0),
      );
    }
    throw new Error(`unexpected exec ${cmd.join(" ")}`);
  }
}

type Harness = {
  sandbox: InstanceType<typeof CrabboxSandbox>;
  storage: MemoryStorage;
  container: MockContainer;
};

function harness(): Harness {
  const storage = new MemoryStorage();
  const container = new MockContainer();
  let queue: Promise<void> = Promise.resolve();
  const ctx = {
    storage,
    container,
    waitUntil: (promise: Promise<unknown>) => void promise,
    blockConcurrencyWhile: async <T>(callback: () => Promise<T>): Promise<T> => {
      const run = (async () => {
        await queue.catch(() => undefined);
        return callback();
      })();
      queue = run.then(
        () => undefined,
        () => undefined,
      );
      return run;
    },
  };
  const sandbox = new CrabboxSandbox(
    ctx as unknown as DurableObjectState,
    {} as ConstructorParameters<typeof CrabboxSandbox>[1],
  );
  return { sandbox, storage, container };
}

type CapturedInternalRequest = {
  names: string[];
  requests: Request[];
};

function envWithCapture(capture: CapturedInternalRequest): Parameters<typeof worker.fetch>[1] {
  return {
    CRABBOX_RUNNER_TOKEN: "runner-token",
    CrabboxSandbox: {
      idFromName(name: string) {
        capture.names.push(name);
        return name;
      },
      get() {
        return {
          async fetch(request: Request): Promise<Response> {
            capture.requests.push(request);
            return Response.json({ ok: true });
          },
        };
      },
    },
  } as unknown as Parameters<typeof worker.fetch>[1];
}

function crabboxRequest(path: string, body?: Record<string, unknown>): Request {
  return new Request(`http://crabbox.internal${path}`, {
    method: body === undefined ? "GET" : "POST",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

function createLease(
  sandbox: InstanceType<typeof CrabboxSandbox>,
  body: Record<string, unknown> = {},
): Promise<Response> {
  return sandbox.fetch(
    crabboxRequest("/__crabbox/create", { id: "cbx_test", workdir: "/workspace/repo", ...body }),
  );
}

function execLease(
  sandbox: InstanceType<typeof CrabboxSandbox>,
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

function uploadLease(sandbox: InstanceType<typeof CrabboxSandbox>, body = "payload") {
  return sandbox.fetch(
    new Request("http://crabbox.internal/__crabbox/files?path=/workspace/repo/archive.tgz", {
      method: "POST",
      body,
    }),
  );
}

async function events(response: Response): Promise<Array<Record<string, unknown>>> {
  const text = await response.text();
  return text
    .split("\n")
    .filter(Boolean)
    .map((line) => JSON.parse(line) as Record<string, unknown>);
}

// vi.waitFor advances faked Date between attempts, which skews lease timestamps.
async function eventually(check: () => Promise<void>): Promise<void> {
  const deadline = performance.now() + 2_000;
  for (;;) {
    try {
      // oxlint-disable-next-line eslint/no-await-in-loop -- polling until the check passes.
      await check();
      return;
    } catch (error) {
      if (performance.now() > deadline) throw error;
    }
    // oxlint-disable-next-line eslint/no-await-in-loop -- see above.
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((next) => {
    resolve = next;
  });
  return { promise, resolve };
}

async function activeExecutions(storage: MemoryStorage): Promise<number | undefined> {
  const meta = await storage.get<{ activeExecutions?: number }>("crabbox:lease");
  return meta?.activeExecutions;
}

describe("Cloudflare runner routing", () => {
  it("exposes authenticated runner readiness without touching containers", async () => {
    const capture: CapturedInternalRequest = { names: [], requests: [] };
    const response = await worker.fetch(
      new Request("https://runner.example/v1/readiness", {
        headers: { Authorization: "Bearer runner-token" },
      }),
      envWithCapture(capture),
    );

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toEqual({
      ok: true,
      runner: "cloudflare",
      instanceTypes: ["standard-1", "standard-2", "standard-3", "standard-4"],
    });
    expect(capture.requests).toHaveLength(0);
  });

  it("reports a missing container binding in readiness", async () => {
    const response = await worker.fetch(
      new Request("https://runner.example/v1/readiness", {
        headers: { Authorization: "Bearer runner-token" },
      }),
      { CRABBOX_RUNNER_TOKEN: "runner-token" } as Parameters<typeof worker.fetch>[1],
    );

    expect(response.status).toBe(503);
    await expect(response.json()).resolves.toMatchObject({
      ok: false,
      missing: ["CrabboxSandbox"],
    });
  });

  it("requires runner auth for readiness", async () => {
    const capture: CapturedInternalRequest = { names: [], requests: [] };
    const response = await worker.fetch(
      new Request("https://runner.example/v1/readiness"),
      envWithCapture(capture),
    );

    expect(response.status).toBe(401);
    await expect(response.json()).resolves.toEqual({ error: "unauthorized" });
  });

  it("sanitizes create payload before dispatching to the durable object", async () => {
    const capture: CapturedInternalRequest = { names: [], requests: [] };
    const response = await worker.fetch(
      new Request("https://runner.example/v1/sandboxes", {
        method: "POST",
        headers: { Authorization: "Bearer runner-token", "Content-Type": "application/json" },
        body: JSON.stringify({
          id: " cbx_test ",
          workdir: "/workspace/../workspace/repo",
          instanceType: "Standard-2",
          labels: { repo: "my-app" },
        }),
      }),
      envWithCapture(capture),
    );

    expect(response.status).toBe(200);
    expect(capture.names).toEqual(["cbx_test"]);
    const forwarded = capture.requests[0];
    expect(forwarded?.headers.get("Authorization")).toBeNull();
    await expect(forwarded?.json()).resolves.toMatchObject({
      id: "cbx_test",
      workdir: "/workspace/repo",
      instanceType: "standard-2",
      labels: { repo: "my-app" },
    });
  });

  it("rejects lite and maps basic to standard-1", async () => {
    const create = (instanceType: string, capture: CapturedInternalRequest) =>
      worker.fetch(
        new Request("https://runner.example/v1/sandboxes", {
          method: "POST",
          headers: { Authorization: "Bearer runner-token" },
          body: JSON.stringify({ id: "cbx_test", instanceType }),
        }),
        envWithCapture(capture),
      );

    const lite: CapturedInternalRequest = { names: [], requests: [] };
    const rejected = await create("lite", lite);
    expect(rejected.status).toBe(400);
    await expect(rejected.json()).resolves.toEqual({
      error: "instanceType lite cannot start the bundled runner image; use standard-1",
    });
    expect(lite.requests).toHaveLength(0);

    const basic: CapturedInternalRequest = { names: [], requests: [] };
    expect((await create("basic", basic)).status).toBe(200);
    await expect(basic.requests[0]?.json()).resolves.toMatchObject({ instanceType: "standard-1" });
  });

  it("does not forward edge auth headers to durable object proxy requests", async () => {
    const capture: CapturedInternalRequest = { names: [], requests: [] };
    const response = await worker.fetch(
      new Request(
        "https://runner.example/v1/sandboxes/cbx_test/exec-stream?instanceType=standard-4",
        {
          method: "POST",
          headers: { Authorization: "Bearer runner-token", "Content-Type": "application/json" },
          body: JSON.stringify({ command: "echo hi", cwd: "/workspace/repo" }),
        },
      ),
      envWithCapture(capture),
    );

    expect(response.status).toBe(200);
    expect(capture.names).toEqual(["cbx_test"]);
    for (const request of capture.requests) {
      expect(request.headers.get("Authorization")).toBeNull();
    }
    expect(new URL(capture.requests.at(-1)?.url ?? "").pathname).toBe("/__crabbox/exec-stream");
  });

  it("returns a controlled 400 response for invalid create JSON", async () => {
    const capture: CapturedInternalRequest = { names: [], requests: [] };
    const response = await worker.fetch(
      new Request("https://runner.example/v1/sandboxes", {
        method: "POST",
        headers: { Authorization: "Bearer runner-token" },
        body: "{",
      }),
      envWithCapture(capture),
    );

    expect(response.status).toBe(400);
    expect(capture.requests).toHaveLength(0);
    await expect(response.json()).resolves.toEqual({ error: "invalid json" });
  });
});

describe("Cloudflare runner lifecycle", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-05-13T18:00:00Z"));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("returns a controlled 400 response for invalid durable object JSON", async () => {
    const { sandbox } = harness();
    const response = await sandbox.fetch(
      new Request("http://crabbox.internal/__crabbox/exec-stream", { method: "POST", body: "{" }),
    );

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toEqual({ error: "invalid json" });
  });

  it("starts the configured image and instance and alarms at the idle deadline", async () => {
    const { sandbox, storage, container } = harness();

    const response = await createLease(sandbox, {
      instanceType: "standard-2",
      ttlSeconds: 3600,
      idleTimeoutSeconds: 600,
      labels: { repo: "my-app" },
    });

    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      id: "cbx_test",
      state: "running",
      image: "default",
      instanceType: "standard-2",
      expiresAt: "2026-05-13T18:10:00.000Z",
    });
    expect(container.started).toEqual([
      { image: container.images.default, instance: "standard-2", enableInternet: true },
    ]);
    expect(storage.alarm).toBe(Date.parse("2026-05-13T18:10:00Z"));
  });

  it("retries a readiness probe that never settles", async () => {
    vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout", "performance"] });
    const { sandbox, container } = harness();
    container.hangingProbes = 1;

    const response = createLease(sandbox);
    await vi.advanceTimersByTimeAsync(10_500);

    expect((await response).status).toBe(200);
    expect(container.calls.filter((call) => call.cmd[0] === "true")).toHaveLength(2);
  });

  it("rejects images that are not configured", async () => {
    const { sandbox, container } = harness();

    const response = await createLease(sandbox, { image: "python" });

    expect(response.status).toBe(400);
    await expect(response.json()).resolves.toEqual({
      error: "image python is not configured",
      images: ["default"],
    });
    expect(container.started).toHaveLength(0);
  });

  it("streams command output and exit code from native exec", async () => {
    const { sandbox, container } = harness();
    await createLease(sandbox);
    container.command = { stdout: ["hi\n"], stderr: ["warn\n"], exitCode: 3 };

    const response = await execLease(sandbox, {
      command: "echo hi",
      env: { FOO: "bar", "bad-key": "x" },
      timeoutMs: 1500,
    });

    expect(response.headers.get("Content-Type")).toBe("application/x-ndjson");
    const got = await events(response);
    expect(got[0]).toEqual({ type: "start" });
    expect(got).toContainEqual({ type: "stdout", data: "hi\n" });
    expect(got).toContainEqual({ type: "stderr", data: "warn\n" });
    expect(got.at(-1)).toEqual({ type: "complete", exitCode: 3 });

    const script = container.calls.find((call) => call.cmd[0] === "/bin/sh");
    const scriptPath = script?.cmd[4] ?? "";
    expect(script?.cmd[5]).toBe("/workspace/repo");
    expect(container.files.get(scriptPath)).toBe('rm -f -- "$0"\necho hi\n');
    const run = container.calls.find((call) => call.cmd[0] === "timeout");
    expect(run?.cmd).toEqual(["timeout", "--kill-after=5s", "2s", "/bin/bash", "-l", scriptPath]);
    expect(run?.options).toEqual({ cwd: "/workspace/repo", env: { FOO: "bar" } });
  });

  it("finishes the stream when a background descendant keeps stdout open", async () => {
    const { sandbox, container } = harness();
    await createLease(sandbox);
    container.command = { stdout: ["done\n"], exitCode: 0, holdStdoutOpen: true };

    const got = await events(await execLease(sandbox, { command: "sleep 30 & echo done" }));

    expect(got).toContainEqual({ type: "stdout", data: "done\n" });
    expect(got.at(-1)).toEqual({ type: "complete", exitCode: 0 });
  });

  it("reports exec failures as stream errors and clears active executions", async () => {
    const { sandbox, storage, container } = harness();
    await createLease(sandbox, { idleTimeoutSeconds: 600 });
    container.failCommand = new Error("exec exploded");

    const got = await events(await execLease(sandbox));

    expect(got.at(-1)).toEqual({ type: "error", error: "exec exploded" });
    await eventually(async () => expect(await activeExecutions(storage)).toBeUndefined());
  });

  it("touches the lease after streamed command completion", async () => {
    const { sandbox } = harness();
    await createLease(sandbox, { idleTimeoutSeconds: 600 });

    vi.setSystemTime(new Date("2026-05-13T18:05:00Z"));
    await (await execLease(sandbox)).text();

    await eventually(async () => {
      const status = await sandbox.fetch(crabboxRequest("/__crabbox/status"));
      await expect(status.json()).resolves.toMatchObject({
        state: "running",
        lastTouchedAt: "2026-05-13T18:05:00.000Z",
        expiresAt: "2026-05-13T18:15:00.000Z",
      });
    });
  });

  it("does not expire a lease while a command stream is active", async () => {
    const { sandbox, storage, container } = harness();
    await createLease(sandbox, { idleTimeoutSeconds: 10 });
    const gate = deferred<void>();
    container.command = { stdout: ["started\n"], exitCode: 0, gate: gate.promise };

    const response = await execLease(sandbox, { command: "sleep 30" });
    await eventually(async () => expect(await activeExecutions(storage)).toBe(1));

    vi.setSystemTime(new Date("2026-05-13T18:00:11Z"));
    await sandbox.alarm();
    expect(container.destroyed).toBe(0);

    gate.resolve();
    await response.text();
    await eventually(async () => expect(await activeExecutions(storage)).toBeUndefined());
    const status = await sandbox.fetch(crabboxRequest("/__crabbox/status"));
    await expect(status.json()).resolves.toMatchObject({
      state: "running",
      expiresAt: "2026-05-13T18:00:21.000Z",
    });
  });

  it("writes uploads through exec stdin and keeps the lease alive meanwhile", async () => {
    const { sandbox, storage, container } = harness();
    await createLease(sandbox, { idleTimeoutSeconds: 10 });
    const gate = deferred<void>();
    container.uploadGate = gate.promise;

    const upload = uploadLease(sandbox, "archive-bytes");
    await eventually(async () => expect(await activeExecutions(storage)).toBe(1));

    vi.setSystemTime(new Date("2026-05-13T18:00:11Z"));
    await sandbox.alarm();
    expect(container.destroyed).toBe(0);

    gate.resolve();
    const response = await upload;
    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toEqual({
      ok: true,
      path: "/workspace/repo/archive.tgz",
    });
    expect(container.files.get("/workspace/repo/archive.tgz")).toBe("archive-bytes");
    expect(await activeExecutions(storage)).toBeUndefined();
  });

  it("reports upload write failures", async () => {
    const { sandbox, container } = harness();
    await createLease(sandbox);
    container.uploadExitCode = 1;

    const response = await uploadLease(sandbox);

    expect(response.status).toBe(500);
    await expect(response.json()).resolves.toEqual({
      error: "write /workspace/repo/archive.tgz: disk full",
    });
  });

  it("serializes active execution accounting for concurrent requests", async () => {
    const { sandbox, storage, container } = harness();
    await createLease(sandbox, { idleTimeoutSeconds: 10 });
    const uploadGate = deferred<void>();
    const commandGate = deferred<void>();
    container.uploadGate = uploadGate.promise;
    container.command = { stdout: [], exitCode: 0, gate: commandGate.promise };

    const upload = uploadLease(sandbox);
    const exec = execLease(sandbox, { command: "sleep 30" });
    await eventually(async () => expect(await activeExecutions(storage)).toBe(2));

    uploadGate.resolve();
    commandGate.resolve();
    expect((await upload).status).toBe(200);
    await (await exec).text();
    await eventually(async () => expect(await activeExecutions(storage)).toBeUndefined());
  });

  it("ends the lease instead of restarting a stopped container with an empty workspace", async () => {
    const { sandbox, storage, container } = harness();
    await createLease(sandbox, { idleTimeoutSeconds: 600 });
    await container.destroy();

    const response = await execLease(sandbox);

    expect(response.status).toBe(410);
    await expect(response.json()).resolves.toMatchObject({
      error: "container stopped; its workspace is gone",
      state: "stopped",
    });
    expect(container.started).toHaveLength(1);
    expect(await activeExecutions(storage)).toBeUndefined();
    expect(storage.alarm).toBeNull();
    const status = await sandbox.fetch(crabboxRequest("/__crabbox/status"));
    await expect(status.json()).resolves.toMatchObject({ state: "stopped" });
  });

  it("renews the container inactivity timeout from the keep-alive alarm", async () => {
    const { sandbox, storage, container } = harness();
    await createLease(sandbox, { ttlSeconds: 24 * 3600 });
    expect(storage.alarm).toBe(Date.parse("2026-05-13T19:00:00Z"));
    const renewed = container.inactivityTimeouts;

    vi.setSystemTime(new Date("2026-05-13T19:00:00Z"));
    await sandbox.alarm();

    expect(container.inactivityTimeouts).toBe(renewed + 1);
    expect(container.destroyed).toBe(0);
    expect(storage.alarm).toBe(Date.parse("2026-05-13T20:00:00Z"));
  });

  it("stops a command whose request is canceled before exec returns", async () => {
    const { sandbox, container } = harness();
    await createLease(sandbox);
    const gate = deferred<void>();
    container.command = { stdout: ["never\n"], exitCode: 0, gate: gate.promise };
    const abort = new AbortController();

    const response = await sandbox.fetch(
      new Request("http://crabbox.internal/__crabbox/exec-stream", {
        method: "POST",
        body: JSON.stringify({ command: "sleep 30", cwd: "/workspace/repo" }),
        signal: abort.signal,
      }),
    );
    await eventually(async () =>
      expect(container.calls.some((call) => call.cmd[0] === "timeout")).toBe(true),
    );
    abort.abort();
    gate.resolve();

    await eventually(async () => expect(container.kills).toEqual([15]));
    await response.body?.cancel();
  });

  it("marks create startup failures stopped and destroys the container", async () => {
    const { sandbox, storage } = harness();
    const { container } = {
      container: (sandbox.ctx as unknown as { container: MockContainer }).container,
    };
    container.failStart = true;

    const response = await createLease(sandbox, { idleTimeoutSeconds: 600 });

    expect(response.status).toBe(503);
    await expect(response.json()).resolves.toMatchObject({
      error: "container failed to start: internal error",
      state: "stopped",
    });
    const meta = await storage.get<{ state?: string; stoppedAt?: string }>("crabbox:lease");
    expect(meta).toMatchObject({ state: "stopped" });
    expect(storage.alarm).toBeNull();
  });

  it("expires and destroys the container when the alarm fires after the deadline", async () => {
    const { sandbox, storage, container } = harness();
    await createLease(sandbox, { idleTimeoutSeconds: 10 });

    vi.setSystemTime(new Date("2026-05-13T18:00:11Z"));
    await sandbox.alarm();

    expect(container.destroyed).toBe(1);
    expect(storage.alarm).toBeNull();
    const response = await execLease(sandbox);
    expect(response.status).toBe(410);
    await expect(response.json()).resolves.toMatchObject({
      error: "sandbox expired",
      state: "expired",
    });
  });

  it("reports a lease whose container stopped as stopped", async () => {
    const { sandbox, container } = harness();
    await createLease(sandbox, { idleTimeoutSeconds: 600 });
    container.running = false;

    const status = await sandbox.fetch(crabboxRequest("/__crabbox/status"));

    await expect(status.json()).resolves.toMatchObject({ state: "stopped" });
  });
});
