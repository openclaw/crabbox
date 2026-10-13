import { CloudflareCoordinatorRuntime, type CoordinatorRuntime } from "./coordinator-runtime";
import { json } from "./http";
import type { Env } from "./types";

export const serveRelayFrameBytes = 32 * 1024 + 1;
export const serveRelaySendBufferBytes = 2 * serveRelayFrameBytes;
export const serveRelayID = /^[a-f0-9]{32}$/;
export interface ServeRelayDescriptor {
  sessionID: string;
  deadline: number;
  connectBy: number;
}
export interface ServeRelayBackend {
  prepare(session: ServeRelayDescriptor, token: string): Promise<void>;
  pendingToken(id: string): Promise<string | undefined>;
  alive(id: string): Promise<boolean>;
  close(id: string): Promise<void>;
  attach(id: string, role: "client" | "agent", request: Request): Promise<Response>;
}

// One outstanding 32 KiB data frame per direction. ACKs are emitted only when
// the receiver has consumed a frame, so no runtime needs an unbounded send queue.
export class ServeRelaySession {
  private sockets: Partial<Record<"client" | "agent", WebSocket>> = {};
  private inFlight = { client: false, agent: false };
  private timer: ReturnType<typeof setTimeout>;
  private token: string | undefined;
  private closed = false;
  constructor(
    readonly descriptor: ServeRelayDescriptor,
    token: string,
    private readonly runtime: CoordinatorRuntime,
    private readonly onClose: () => void = () => {},
  ) {
    this.token = token;
    this.timer = setTimeout(() => this.close(), Math.max(0, descriptor.connectBy - Date.now()));
  }
  alive(): boolean {
    return !this.closed;
  }
  pendingToken(): string | undefined {
    const token = this.token;
    return this.closed ? undefined : token;
  }
  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.token = undefined;
    clearTimeout(this.timer);
    for (const socket of Object.values(this.sockets)) {
      try {
        socket.close(1000, "serve relay ended");
      } catch {
        /* Already closed. */
      }
    }
    this.onClose();
  }
  attach(role: "client" | "agent"): Response {
    if (
      this.closed ||
      Date.now() >= this.descriptor.deadline ||
      (!this.sockets.client && !this.sockets.agent && Date.now() >= this.descriptor.connectBy)
    ) {
      this.close();
      return json({ error: "serve_relay_expired" }, { status: 410 });
    }
    if (this.sockets[role]) return json({ error: "serve_relay_not_allowed" }, { status: 403 });
    const { socket, response } = this.runtime.createWebSocketUpgrade({
      maxPayload: serveRelayFrameBytes,
    });
    // Workerd defaults to Blob delivery; the synchronous frame parser requires bytes.
    socket.binaryType = "arraybuffer";
    this.sockets[role] = socket;
    if (role === "agent") this.token = undefined;
    this.runtime.acceptEphemeralWebSocket(
      socket,
      {
        message: (data) => this.message(role, data),
        close: () => this.close(),
        error: () => this.close(),
      },
      "serve-relay",
    );
    clearTimeout(this.timer);
    const paired = this.sockets.client && this.sockets.agent;
    this.timer = setTimeout(
      () => this.close(),
      Math.max(
        0,
        (paired
          ? this.descriptor.deadline
          : Math.min(Date.now() + 30_000, this.descriptor.connectBy)) - Date.now(),
      ),
    );
    if (paired) {
      this.send("client", new Uint8Array([2]));
      this.send("agent", new Uint8Array([2]));
    }
    return response;
  }
  private message(role: "client" | "agent", value: string | ArrayBuffer | Blob): void {
    const peer = role === "client" ? "agent" : "client";
    if (
      this.closed ||
      !this.sockets[peer] ||
      !(value instanceof ArrayBuffer) ||
      value.byteLength < 1 ||
      value.byteLength > serveRelayFrameBytes
    ) {
      this.close();
      return;
    }
    const frame = new Uint8Array(value);
    if (frame[0] === 0 && frame.length > 1 && !this.inFlight[role]) {
      this.inFlight[role] = true;
    } else if (frame[0] === 1 && frame.length === 1 && this.inFlight[peer]) {
      this.inFlight[peer] = false;
    } else {
      this.close();
      return;
    }
    this.send(peer, value);
  }
  private send(role: "client" | "agent", value: ArrayBuffer | Uint8Array): void {
    if (this.closed) return;
    const socket = this.sockets[role]!;
    // Node's transport queue must stay bounded even if both peers forge ACKs.
    const queued = (socket as WebSocket & { bufferedAmount?: number }).bufferedAmount ?? 0;
    if (queued + value.byteLength > serveRelaySendBufferBytes) {
      this.close();
      return;
    }
    try {
      socket.send(value);
    } catch {
      this.close();
    }
  }
}

// Node uses the same per-session engine and its existing upgrade context.
// The serve-relay attachment runs outside the Fleet lifecycle/storage queue.
export class LocalServeRelayBackend implements ServeRelayBackend {
  private sessions = new Map<string, ServeRelaySession>();
  constructor(private readonly runtime: CoordinatorRuntime) {}
  async prepare(descriptor: ServeRelayDescriptor, token: string): Promise<void> {
    if (this.sessions.has(descriptor.sessionID) || this.sessions.size >= 1024)
      throw new Error("serve_relay_capacity");
    this.sessions.set(
      descriptor.sessionID,
      new ServeRelaySession(descriptor, token, this.runtime, () =>
        this.sessions.delete(descriptor.sessionID),
      ),
    );
  }
  async pendingToken(id: string): Promise<string | undefined> {
    return this.sessions.get(id)?.pendingToken();
  }
  async alive(id: string): Promise<boolean> {
    return this.sessions.get(id)?.alive() ?? false;
  }
  async close(id: string): Promise<void> {
    this.sessions.get(id)?.close();
  }
  async attach(id: string, role: "client" | "agent", _request: Request): Promise<Response> {
    return (
      this.sessions.get(id)?.attach(role) ?? json({ error: "serve_relay_expired" }, { status: 410 })
    );
  }
}

export class CloudflareServeRelayBackend implements ServeRelayBackend {
  constructor(private readonly namespace: DurableObjectNamespace) {}
  private call(id: string, path: string, init?: RequestInit): Promise<Response> {
    return this.namespace
      .get(this.namespace.idFromName(id))
      .fetch(new Request(`https://serve-relay.internal/${path}`, init));
  }
  async prepare(descriptor: ServeRelayDescriptor, token: string): Promise<void> {
    const response = await this.call(descriptor.sessionID, "prepare", {
      method: "POST",
      body: JSON.stringify({ descriptor, token }),
    });
    if (!response.ok) throw new Error("serve_relay_capacity");
  }
  async pendingToken(id: string): Promise<string | undefined> {
    const response = await this.call(id, "token", { method: "POST" });
    return response.ok ? (await response.json<{ token?: string }>()).token : undefined;
  }
  async alive(id: string): Promise<boolean> {
    return (await this.call(id, "alive")).ok;
  }
  async close(id: string): Promise<void> {
    await this.call(id, "close", { method: "POST" });
  }
  async attach(id: string, role: "client" | "agent", _request: Request): Promise<Response> {
    return this.call(id, role, { headers: { upgrade: "websocket" } });
  }
}

export function serveRelayBackend(env: Env): ServeRelayBackend | undefined {
  return (
    env.serveRelayBackend ??
    (env.SERVE_RELAY ? new CloudflareServeRelayBackend(env.SERVE_RELAY) : undefined)
  );
}

// This object is reachable only through its Worker binding. No public request
// reaches it until Fleet has authenticated and consumed that side's admission.
// Tokens live only in this active session's memory; Fleet stores their hashes.
export class ServeRelayDurableObject {
  private session?: ServeRelaySession;
  private initialized = false;
  private readonly runtime: CloudflareCoordinatorRuntime;
  constructor(state: DurableObjectState) {
    this.runtime = new CloudflareCoordinatorRuntime(state);
  }
  async fetch(request: Request): Promise<Response> {
    const path = new URL(request.url).pathname;
    if (path === "/prepare" && request.method === "POST") {
      if (this.initialized) return json({ error: "serve_relay_not_allowed" }, { status: 403 });
      const body = await request.text();
      if (body.length > 1024) return new Response(null, { status: 413 });
      const { descriptor, token } = JSON.parse(body) as {
        descriptor: ServeRelayDescriptor;
        token: string;
      };
      if (
        !serveRelayID.test(descriptor.sessionID) ||
        !serveRelayID.test(token) ||
        !Number.isSafeInteger(descriptor.deadline) ||
        !Number.isSafeInteger(descriptor.connectBy) ||
        descriptor.connectBy > Date.now() + 60_000 ||
        descriptor.connectBy > descriptor.deadline
      )
        return new Response(null, { status: 400 });
      this.initialized = true;
      this.session = new ServeRelaySession(descriptor, token, this.runtime);
      return json({ ok: true });
    }
    if (!this.session?.alive()) return json({ error: "serve_relay_expired" }, { status: 410 });
    if (path === "/alive") return json({ ok: true });
    if (path === "/token" && request.method === "POST")
      return json({ token: this.session.pendingToken() });
    if (path === "/close" && request.method === "POST") {
      this.session.close();
      return json({ ok: true });
    }
    if (
      (path === "/client" || path === "/agent") &&
      request.headers.get("upgrade")?.toLowerCase() === "websocket"
    )
      return this.session.attach(path === "/client" ? "client" : "agent");
    return new Response(null, { status: 404 });
  }
}

// Called outside Fleet. Fleet's response is a one-time admission, never a
// tunnel; all subsequent frames are handled by the dedicated session engine.
export async function upgradeServeRelay(
  request: Request,
  response: Response,
  env: Env,
): Promise<Response> {
  const match = /^\/v1\/serve\/relay\/([a-f0-9]{32})\/(client|agent)$/.exec(
    new URL(request.url).pathname,
  );
  if (!match || response.status !== 200) return response;
  const backend = serveRelayBackend(env);
  if (!backend) return json({ error: "serve_relay_unavailable" }, { status: 501 });
  return backend.attach(match[1]!, match[2] as "client" | "agent", request);
}
