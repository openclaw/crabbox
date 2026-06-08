export interface CoordinatorStorage {
  get<T>(key: string): Promise<T | undefined>;
  put<T>(key: string, value: T): Promise<void>;
  delete(key: string): Promise<unknown>;
  list<T>(options?: { prefix?: string }): Promise<Map<string, T>>;
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

  constructor(private readonly state: DurableObjectState) {
    this.storage = state.storage;
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
      void handlers.message(event.data);
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
}
