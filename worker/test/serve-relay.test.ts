// oxlint-disable eslint/no-await-in-loop -- Session caps depend on each preceding admission and close.
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  CloudflareCoordinatorRuntime,
  type CoordinatorRuntime,
  type CoordinatorSocketHandlers,
} from "../src/coordinator-runtime";
import { ServeCoordinator } from "../src/serve";
import {
  LocalServeRelayBackend,
  ServeRelaySession,
  serveRelaySendBufferBytes,
  upgradeServeRelay,
} from "../src/serve-relay";
import type { Env, LeaseRecord } from "../src/types";
import { ProvisioningTestStorage } from "./provisioning-fixtures";

const machineID = "11111111-1111-4111-8111-111111111111";
const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";
class Socket {
  bufferedAmount = 0;
  sent: (ArrayBuffer | Uint8Array | string)[] = [];
  closed = false;
  handlers?: CoordinatorSocketHandlers;
  send(data: ArrayBuffer | Uint8Array | string) {
    this.sent.push(data);
  }
  close() {
    if (this.closed) return;
    this.closed = true;
    this.handlers?.close(1000, "closed");
  }
  receive(data: number[]) {
    return this.handlers?.message(new Uint8Array(data).buffer);
  }
}
function runtimeFixture() {
  const storage = new ProvisioningTestStorage();
  const sockets: Socket[] = [];
  const runtime = {
    storage,
    runExclusive: async <T>(f: () => Promise<T>) => f(),
    getAlarm: async () => undefined,
    scheduleAlarm: async () => {},
    clearAlarm: async () => {},
    createWebSocketUpgrade: () => {
      const socket = new Socket();
      sockets.push(socket);
      return {
        socket: socket as unknown as WebSocket,
        response: new Response(null, { status: 204 }),
      };
    },
    acceptEphemeralWebSocket: (socket: WebSocket, handlers: CoordinatorSocketHandlers) => {
      (socket as unknown as Socket).handlers = handlers;
    },
  } as unknown as CoordinatorRuntime;
  return { runtime, storage, sockets };
}
function request(
  path: string,
  data?: unknown,
  owner = "alice",
  headers: Record<string, string> = {},
) {
  return new Request(`http://localhost${path}`, {
    method: data === undefined ? "GET" : "POST",
    headers: { "x-crabbox-owner": owner, "x-crabbox-org": "example-org", ...headers },
    ...(data === undefined ? {} : { body: JSON.stringify(data) }),
  });
}
async function controlFixture() {
  const f = runtimeFixture();
  const backend = new LocalServeRelayBackend(f.runtime);
  const env = {
    CRABBOX_SERVE_ALLOWED_OWNERS: "*",
    CRABBOX_DEFAULT_ORG: "example-org",
    serveRelayBackend: backend,
  } as unknown as Env;
  const serve = new ServeCoordinator(f.runtime, env);
  const route = (path: string, data?: unknown, owner?: string, headers?: Record<string, string>) =>
    serve.route(request(path, data, owner, headers), path.split("/").filter(Boolean));
  const machine = {
    machineID,
    name: "builder",
    os: "linux",
    arch: "amd64",
    cpus: 64,
    memoryMB: 131072,
    maxLeases: 8,
    address: "100.64.1.2",
    frontDoor: { port: 2222, hostKey: key, relay: true },
    runtime: "docker",
    runtimeVersion: "1",
    agentVersion: "test",
    image: "test-image",
    labels: {},
    load: 0,
    freeMemoryMB: 131072,
    draining: false,
    leases: [] as { id: string; state: string }[],
  };
  expect((await route("/v1/serve/heartbeat", machine)).status).toBe(200);
  async function create(id: string) {
    expect(
      (
        await serve.create(request("/v1/leases", {}), {
          provider: "serve",
          leaseID: id,
          sshPublicKey: key,
          class: "standard",
        })
      ).status,
    ).toBe(202);
    expect(
      (await route(`/v1/serve/leases/${id}/ready`, { machineID, sshHostKey: key })).status,
    ).toBe(200);
    machine.leases.push({ id, state: "running" });
  }
  await create("cbx_000000000001");
  return { ...f, serve, backend, env, route, create, machine };
}
afterEach(() => vi.useRealTimers());
describe("isolated serve relay session", () => {
  it("receives binary frames synchronously through Cloudflare's Blob-default sockets", () => {
    class CloudflareSocket extends EventTarget {
      binaryType: "blob" | "arraybuffer" = "blob";
      accept = vi.fn<() => void>();
      send = vi.fn<(data: ArrayBuffer | Uint8Array) => void>();
      close = vi.fn<() => void>();
      receive(frame: Uint8Array<ArrayBuffer>) {
        this.dispatchEvent(
          new MessageEvent("message", {
            data: this.binaryType === "blob" ? new Blob([frame]) : frame.buffer,
          }),
        );
      }
    }
    const runtime = new CloudflareCoordinatorRuntime({
      storage: {},
      acceptWebSocket: vi.fn<() => void>(),
    } as unknown as DurableObjectState);
    const sockets = [new CloudflareSocket(), new CloudflareSocket()];
    const [client, agent] = sockets as [CloudflareSocket, CloudflareSocket];
    let next = 0;
    vi.spyOn(runtime, "createWebSocketUpgrade").mockImplementation(() => ({
      socket: sockets[next++]! as unknown as WebSocket,
      response: new Response(null, { status: 204 }),
    }));
    const session = new ServeRelaySession(
      {
        sessionID: "a".repeat(32),
        connectBy: Date.now() + 60_000,
        deadline: Date.now() + 3600_000,
      },
      "b".repeat(32),
      runtime,
    );
    try {
      session.attach("client");
      session.attach("agent");
      expect(client.accept).toHaveBeenCalledOnce();
      expect(agent.accept).toHaveBeenCalledOnce();
      expect(client.send).toHaveBeenCalledWith(new Uint8Array([2]));
      expect(agent.send).toHaveBeenCalledWith(new Uint8Array([2]));
      for (const [sender, receiver] of [
        [client, agent],
        [agent, client],
      ] as const) {
        const frame = new Uint8Array(32 * 1024 + 1);
        frame.fill(42, 1);
        sender.receive(frame);
        expect(receiver.send).toHaveBeenLastCalledWith(frame.buffer);
        receiver.receive(new Uint8Array([1]));
        expect(sender.send).toHaveBeenLastCalledWith(new Uint8Array([1]).buffer);
      }
      expect(session.alive()).toBe(true);
      client.dispatchEvent(new Event("close"));
      expect(session.alive()).toBe(false);
      expect(agent.close).toHaveBeenCalledOnce();
    } finally {
      session.close();
    }
  });
  it("pairs once and pipes bounded binary frames with consumption acknowledgements and no Fleet calls", () => {
    vi.useFakeTimers();
    const f = runtimeFixture();
    const exclusive = vi.spyOn(f.runtime, "runExclusive");
    const storage = vi.spyOn(f.storage, "get");
    const session = new ServeRelaySession(
      {
        sessionID: "a".repeat(32),
        connectBy: Date.now() + 60_000,
        deadline: Date.now() + 3600_000,
      },
      "b".repeat(32),
      f.runtime,
    );
    expect(session.pendingToken()).toBe("b".repeat(32));
    expect(session.pendingToken()).toBe("b".repeat(32));
    session.attach("client");
    expect(session.attach("client").status).toBe(403);
    session.attach("agent");
    expect(session.pendingToken()).toBeUndefined();
    const [client, agent] = f.sockets;
    expect(client!.sent[0]).toEqual(new Uint8Array([2]));
    client!.receive([0, 1, 2]);
    expect(agent!.sent[1]).toEqual(new Uint8Array([0, 1, 2]).buffer);
    agent!.receive([1]);
    expect(client!.sent[1]).toEqual(new Uint8Array([1]).buffer);
    agent!.receive([0, 3]);
    expect(client!.sent[2]).toEqual(new Uint8Array([0, 3]).buffer);
    client!.receive([1]);
    expect(exclusive).not.toHaveBeenCalled();
    expect(storage).not.toHaveBeenCalled();
    client!.close();
    expect(agent!.closed).toBe(true);
    expect(session.alive()).toBe(false);
  });
  it("closes on peer timeout, maximum duration, and an unacknowledged extra frame", () => {
    vi.useFakeTimers();
    for (const mode of ["wait", "duration", "overrun"]) {
      const f = runtimeFixture();
      const session = new ServeRelaySession(
        {
          sessionID: "a".repeat(32),
          connectBy: Date.now() + 60_000,
          deadline: Date.now() + 40_000,
        },
        "b".repeat(32),
        f.runtime,
      );
      session.attach("client");
      if (mode !== "wait") session.attach("agent");
      if (mode === "overrun") {
        f.sockets[0]!.receive([0, 1]);
        f.sockets[0]!.receive([0, 2]);
      } else vi.advanceTimersByTime(mode === "wait" ? 30_001 : 40_001);
      expect(session.alive()).toBe(false);
      expect(f.sockets.every((socket) => socket.closed)).toBe(true);
    }
  });
  it("bounds the transport queue independently of forged consumption ACKs", () => {
    vi.useFakeTimers();
    const f = runtimeFixture();
    const session = new ServeRelaySession(
      {
        sessionID: "a".repeat(32),
        connectBy: Date.now() + 60_000,
        deadline: Date.now() + 3600_000,
      },
      "b".repeat(32),
      f.runtime,
    );
    session.attach("client");
    session.attach("agent");
    const [client, agent] = f.sockets;
    client!.receive([0, 1]);
    agent!.receive([1]);
    agent!.bufferedAmount = serveRelaySendBufferBytes;
    client!.receive([0, 2]);
    expect(session.alive()).toBe(false);
    expect(f.sockets.every((socket) => socket.closed)).toBe(true);
    expect(agent!.sent).toHaveLength(2);
  });
});
describe("serve relay authorization", () => {
  it("mints only for an enabled owner and active online lease, and stores only a token hash", async () => {
    vi.useFakeTimers();
    const f = await controlFixture();
    const path = "/v1/serve/leases/cbx_000000000001/relay";
    expect((await f.route(path, {}, "mallory")).status).toBe(403);
    expect((await f.route("/v1/serve/leases/cbx_000000000999/relay", {})).status).toBe(403);
    f.env.CRABBOX_SERVE_ALLOWED_OWNERS = "bob";
    expect((await f.route(path, {})).status).toBe(403);
    f.env.CRABBOX_SERVE_ALLOWED_OWNERS = "*";
    const response = await f.route(path, {});
    expect(response.status).toBe(200);
    const session = await response.json<{ sessionID: string }>();
    const token = await f.backend.pendingToken(session.sessionID);
    expect(token).toMatch(/^[a-f0-9]{32}$/);
    expect(JSON.stringify([...f.storage.values])).not.toContain(token!);
    expect((await f.storage.get<LeaseRecord>("lease:cbx_000000000001"))?.sshJump?.relay).toBe(true);
    vi.advanceTimersByTime(30_001);
    expect((await f.route(path, {})).status).toBe(403);
  });
  it("binds agent admission to its machine, consumes each side once, and expires", async () => {
    vi.useFakeTimers();
    const f = await controlFixture();
    const { sessionID } = await (
      await f.route("/v1/serve/leases/cbx_000000000001/relay", {})
    ).json<{ sessionID: string }>();
    vi.advanceTimersByTime(1000);
    const heartbeat = await (
      await f.route("/v1/serve/heartbeat", f.machine)
    ).json<{ relays: { sessionID: string; agentToken: string }[] }>();
    const token = heartbeat.relays[0]!.agentToken;
    const agent = `/v1/serve/relay/${sessionID}/agent`,
      client = `/v1/serve/relay/${sessionID}/client`;
    const headers = {
      upgrade: "websocket",
      "x-crabbox-serve-machine": machineID,
      "x-crabbox-relay-token": token,
    };
    expect(
      (await f.route(agent, undefined, "alice", { ...headers, "x-crabbox-serve-machine": "wrong" }))
        .status,
    ).toBe(403);
    expect((await f.route(agent, undefined, "mallory", headers)).status).toBe(403);
    expect(
      (await f.route(agent, undefined, "alice", { ...headers, "x-crabbox-relay-token": "bad" }))
        .status,
    ).toBe(403);
    const admitted = await f.route(agent, undefined, "alice", headers);
    expect(admitted.status).toBe(200);
    expect(
      (await upgradeServeRelay(request(agent, undefined, "alice", headers), admitted, f.env))
        .status,
    ).toBe(204);
    expect((await f.route(agent, undefined, "alice", headers)).status).toBe(403);
    const admittedClient = await f.route(client, undefined, "alice", { upgrade: "websocket" });
    expect(admittedClient.status).toBe(200);
    await upgradeServeRelay(
      request(client, undefined, "alice", { upgrade: "websocket" }),
      admittedClient,
      f.env,
    );
    expect((await f.route(client, undefined, "alice", { upgrade: "websocket" })).status).toBe(403);
    vi.advanceTimersByTime(60_000);
    expect((await f.route(agent, undefined, "alice", headers)).status).toBe(410);
  });
  it("enforces per-lease and per-owner caps and releases them when sessions close", async () => {
    vi.useFakeTimers();
    const f = await controlFixture();
    await f.create("cbx_000000000002");
    await f.create("cbx_000000000003");
    const ids: string[] = [];
    for (const lease of ["cbx_000000000001", "cbx_000000000002"]) {
      for (let i = 0; i < 4; i++) {
        const result = await f.route(`/v1/serve/leases/${lease}/relay`, {});
        expect(result.status).toBe(200);
        ids.push((await result.json<{ sessionID: string }>()).sessionID);
      }
      expect((await f.route(`/v1/serve/leases/${lease}/relay`, {})).status).toBe(429);
    }
    expect((await f.route("/v1/serve/leases/cbx_000000000003/relay", {})).status).toBe(429);
    await f.backend.close(ids[0]!);
    expect((await f.route("/v1/serve/leases/cbx_000000000003/relay", {})).status).toBe(200);
    const lease = (await f.storage.get<LeaseRecord>("lease:cbx_000000000002"))!;
    await f.serve.release(request("/v1/leases", {}), lease);
    for (const id of ids.slice(4)) expect(await f.backend.alive(id)).toBe(false);
  });
});
