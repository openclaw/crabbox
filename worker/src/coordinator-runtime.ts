export interface CoordinatorStorage {
  get<T>(key: string): Promise<T | undefined>;
  put<T>(key: string, value: T): Promise<void>;
  delete(key: string): Promise<unknown>;
  list<T>(options?: { prefix?: string }): Promise<Map<string, T>>;
}

export type CoordinatorRequestQueue = "direct" | "lifecycle";

export function coordinatorRequestQueue(request: Request): CoordinatorRequestQueue {
  const url = new URL(request.url);
  const path = url.pathname.split("/").filter(Boolean);
  const method = request.method.toUpperCase();
  if (method === "GET" && path.join("/") === "v1/auth/github/callback") {
    return "direct";
  }
  if (method === "POST" && path.join("/") === "v1/leases") {
    return "direct";
  }
  if (method === "POST" && path.join("/") === "v1/internal/scheduled") {
    return "direct";
  }
  if (path[0] === "v1" && path[1] === "ready-pools") {
    return "direct";
  }
  if (path[0] === "v1" && path[1] === "images") {
    return "direct";
  }
  if (path.join("/") === "v1/pool") {
    return "direct";
  }
  if (path[0] === "v1" && path[1] === "providers" && path[3] === "readiness") {
    return "direct";
  }
  if (
    path[0] === "v1" &&
    path[1] === "leases" &&
    path[2] &&
    method === "POST" &&
    (path[3] === "heartbeat" || path[3] === "release")
  ) {
    return "direct";
  }
  if (
    path[0] === "v1" &&
    path[1] === "admin" &&
    (path[2] === "lease-audit" ||
      path[2] === "aws-identity" ||
      path[2] === "providers" ||
      path[2] === "hosts" ||
      path[2] === "mac-hosts" ||
      path[2] === "aws-orphan-sweep" ||
      path[2] === "azure-orphan-sweep" ||
      (path[2] === "leases" && method === "POST"))
  ) {
    return "direct";
  }
  if (
    method === "POST" &&
    path[0] === "portal" &&
    path[1] === "leases" &&
    path[2] &&
    path[3] === "release"
  ) {
    return "direct";
  }
  if (
    path[0] === "portal" &&
    ((method === "GET" && (path.length === 1 || path[1] === "admin" || path[1] === "hosts")) ||
      (method === "POST" && path[1] === "hosts" && path[4] === "vnc"))
  ) {
    return "direct";
  }
  if (path[0] === "portal" && path[1] === "leases" && path[2] && path[3] === "code") {
    return "direct";
  }
  return "lifecycle";
}

export async function bufferCoordinatorRequestBody(request: Request): Promise<Request> {
  if (request.method === "GET" || request.method === "HEAD" || request.body === null) {
    return request;
  }
  const body = await request.arrayBuffer();
  return new Request(request, { body, method: request.method });
}

export interface CoordinatorSocketHandlers {
  message(data: string | ArrayBuffer | Blob): Promise<void> | void;
  close(code: number, reason: string): void;
  error(): void;
}

export interface CoordinatorWebSocketUpgrade {
  socket: WebSocket;
  response: Response;
}

export interface CoordinatorRuntime {
  readonly storage: CoordinatorStorage;
  runExclusive<T>(callback: () => Promise<T>): Promise<T>;
  createWebSocketUpgrade(): CoordinatorWebSocketUpgrade;
  getWebSockets(): Iterable<WebSocket>;
  socketAttachment<T>(socket: WebSocket): T | undefined;
  setSocketAttachment(socket: WebSocket, attachment: unknown): void;
  acceptWebSocket(
    socket: WebSocket,
    attachment: unknown,
    tags: string[],
    handlers: CoordinatorSocketHandlers,
  ): void;
  scheduleAlarm(time: number): Promise<void>;
  clearAlarm(): Promise<void>;
}

export class CloudflareCoordinatorRuntime implements CoordinatorRuntime {
  readonly storage: CoordinatorStorage;
  private readonly attachments = new WeakMap<WebSocket, unknown>();
  private exclusiveTail = Promise.resolve();

  constructor(private readonly state: DurableObjectState) {
    this.storage = state.storage;
  }

  async runExclusive<T>(callback: () => Promise<T>): Promise<T> {
    const predecessor = this.exclusiveTail;
    let release!: () => void;
    this.exclusiveTail = new Promise<void>((resolve) => {
      release = resolve;
    });
    await predecessor;
    try {
      return await callback();
    } finally {
      release();
    }
  }

  createWebSocketUpgrade(): CoordinatorWebSocketUpgrade {
    const pair = new WebSocketPair();
    return {
      socket: pair[1],
      response: new Response(null, { status: 101, webSocket: pair[0] }),
    };
  }

  getWebSockets(): Iterable<WebSocket> {
    return this.state.getWebSockets?.() ?? [];
  }

  socketAttachment<T>(socket: WebSocket): T | undefined {
    return (this.attachments.get(socket) ?? socket.deserializeAttachment?.()) as T | undefined;
  }

  setSocketAttachment(socket: WebSocket, attachment: unknown): void {
    this.attachments.set(socket, attachment);
    socket.serializeAttachment?.(attachment);
  }

  acceptWebSocket(
    socket: WebSocket,
    attachment: unknown,
    tags: string[],
    handlers: CoordinatorSocketHandlers,
  ): void {
    this.attachments.set(socket, attachment);
    if (typeof this.state.acceptWebSocket === "function") {
      this.state.acceptWebSocket(socket, tags);
      socket.serializeAttachment(attachment);
      return;
    }
    socket.accept();
    socket.addEventListener("message", (event) => {
      void this.runSocketOperation(attachment, () => handlers.message(event.data));
    });
    socket.addEventListener("close", (event) => {
      handlers.close(event.code, event.reason);
    });
    socket.addEventListener("error", () => {
      handlers.error();
    });
  }

  scheduleAlarm(time: number): Promise<void> {
    return this.state.storage.setAlarm(time);
  }

  clearAlarm(): Promise<void> {
    return this.state.storage.deleteAlarm();
  }

  private runSocketOperation<T>(attachment: unknown, callback: () => Promise<T> | T): Promise<T> {
    const operation = async () => callback();
    if (socketAttachmentKind(attachment) === "control") {
      return this.runExclusive(operation);
    }
    return operation();
  }
}

function socketAttachmentKind(attachment: unknown): string | undefined {
  if (!attachment || typeof attachment !== "object" || !("kind" in attachment)) {
    return undefined;
  }
  return String(attachment.kind);
}
