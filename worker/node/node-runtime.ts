import { AsyncLocalStorage } from "node:async_hooks";
import type { IncomingMessage } from "node:http";
import type { Duplex } from "node:stream";

import { PgBoss } from "pg-boss";
import { WebSocket as NodeWebSocket, WebSocketServer, type RawData } from "ws";

import type {
  CoordinatorRuntime,
  CoordinatorSocketHandlers,
  CoordinatorWebSocketUpgrade,
} from "../src/coordinator-runtime";
import { PostgresCoordinatorStorage } from "./postgres-storage";

const alarmQueue = "coordinator-alarm";
const reconcileQueue = "coordinator-reconcile";

export interface NodeUpgradeContext {
  request: IncomingMessage;
  socket: Duplex;
  head: Buffer;
  upgraded: boolean;
}

export class NodeCoordinatorRuntime implements CoordinatorRuntime {
  readonly storage: PostgresCoordinatorStorage;
  private readonly boss: PgBoss;
  private readonly webSocketServer = new WebSocketServer({
    noServer: true,
    perMessageDeflate: false,
    maxPayload: 12 * 1024 * 1024,
  });
  private readonly upgradeContext = new AsyncLocalStorage<NodeUpgradeContext>();
  private readonly attachments = new WeakMap<WebSocket, unknown>();
  private readonly sockets = new Set<NodeWebSocket>();
  private readonly socketAlive = new WeakMap<NodeWebSocket, boolean>();
  private alarmHandler?: () => Promise<void>;
  private operationRunner = async <T>(callback: () => Promise<T>): Promise<T> => callback();
  private alarmRun: Promise<void> = Promise.resolve();
  private readonly pingInterval: ReturnType<typeof setInterval>;

  constructor(connectionString: string) {
    this.storage = new PostgresCoordinatorStorage(connectionString);
    this.boss = new PgBoss({
      connectionString,
      schema: "crabbox_jobs",
      application_name: "crabbox-coordinator-jobs",
    });
    this.boss.on("error", (error) => {
      console.error("coordinator job queue error", error);
    });
    this.pingInterval = setInterval(() => this.pingSockets(), 30_000);
    this.pingInterval.unref();
  }

  async start(alarmHandler: () => Promise<void>): Promise<void> {
    this.alarmHandler = alarmHandler;
    await this.storage.initialize();
    await this.boss.start();
    await this.boss.createQueue(alarmQueue, {
      // "short" permits one queued successor while the current alarm is active.
      policy: "short",
      retryLimit: 5,
      retryDelay: 5,
      retryBackoff: true,
    });
    await this.boss.createQueue(reconcileQueue, {
      policy: "exclusive",
      retryLimit: 5,
      retryDelay: 5,
      retryBackoff: true,
    });
    await this.boss.work(alarmQueue, { pollingIntervalSeconds: 1 }, async () => {
      await this.runAlarm();
    });
    await this.boss.work(reconcileQueue, { pollingIntervalSeconds: 5 }, async () => {
      await this.runAlarm();
    });
    await this.boss.schedule(reconcileQueue, "*/15 * * * *", null, {
      tz: "UTC",
      singletonKey: "reconcile",
    });
    await this.boss.send(reconcileQueue, null, {
      singletonKey: "startup",
      singletonSeconds: 60,
    });
  }

  setOperationRunner(runner: <T>(callback: () => Promise<T>) => Promise<T>): void {
    this.operationRunner = runner;
  }

  async stop(): Promise<void> {
    clearInterval(this.pingInterval);
    for (const socket of this.sockets) {
      socket.close(1012, "coordinator shutting down");
    }
    await this.boss.stop({ graceful: true, timeout: 10_000 });
    await this.storage.close();
  }

  runWithUpgrade<T>(context: NodeUpgradeContext, callback: () => Promise<T>): Promise<T> {
    return this.upgradeContext.run(context, callback);
  }

  createWebSocketUpgrade(): CoordinatorWebSocketUpgrade {
    const context = this.upgradeContext.getStore();
    if (!context || context.upgraded) {
      throw new Error("websocket upgrade context is unavailable");
    }
    let accepted: NodeWebSocket | undefined;
    this.webSocketServer.handleUpgrade(context.request, context.socket, context.head, (socket) => {
      accepted = socket;
    });
    if (!accepted) {
      throw new Error("websocket upgrade did not produce a socket");
    }
    context.upgraded = true;
    return {
      socket: accepted as unknown as WebSocket,
      response: new Response(null, {
        status: 204,
        headers: { "x-crabbox-websocket-upgraded": "1" },
      }),
    };
  }

  getWebSockets(): Iterable<WebSocket> {
    return [...this.sockets] as unknown as WebSocket[];
  }

  socketAttachment<T>(socket: WebSocket): T | undefined {
    return this.attachments.get(socket) as T | undefined;
  }

  setSocketAttachment(socket: WebSocket, attachment: unknown): void {
    this.attachments.set(socket, attachment);
  }

  acceptWebSocket(
    socket: WebSocket,
    attachment: unknown,
    _tags: string[],
    handlers: CoordinatorSocketHandlers,
  ): void {
    const nodeSocket = socket as unknown as NodeWebSocket;
    this.attachments.set(socket, attachment);
    this.sockets.add(nodeSocket);
    this.socketAlive.set(nodeSocket, true);
    nodeSocket.on("pong", () => {
      this.socketAlive.set(nodeSocket, true);
    });
    nodeSocket.on("message", (data, isBinary) => {
      void Promise.resolve()
        .then(() =>
          this.operationRunner(async () => {
            await handlers.message(webSocketData(data, isBinary));
          }),
        )
        .catch((error) => {
          this.failSocket(nodeSocket, "message", error);
        });
    });
    nodeSocket.on("close", (code, reason) => {
      this.sockets.delete(nodeSocket);
      try {
        handlers.close(code, reason.toString());
      } catch (error) {
        console.error("coordinator websocket close handler failed", error);
      }
    });
    nodeSocket.on("error", () => {
      try {
        handlers.error();
      } catch (error) {
        console.error("coordinator websocket error handler failed", error);
      }
    });
  }

  async scheduleAlarm(time: number): Promise<void> {
    await this.boss.deleteQueuedJobs(alarmQueue);
    await this.boss.send(alarmQueue, null, {
      startAfter: new Date(Math.max(Date.now(), time)),
      singletonKey: "fleet",
      retryLimit: 5,
      retryDelay: 5,
      retryBackoff: true,
    });
  }

  async clearAlarm(): Promise<void> {
    await this.boss.deleteQueuedJobs(alarmQueue);
  }

  private runAlarm(): Promise<void> {
    const run = this.alarmRun.then(async () => {
      if (!this.alarmHandler) {
        throw new Error("coordinator alarm handler is unavailable");
      }
      return this.alarmHandler();
    });
    this.alarmRun = run.catch(() => undefined);
    return run;
  }

  private pingSockets(): void {
    for (const socket of this.sockets) {
      if (this.socketAlive.get(socket) === false) {
        socket.terminate();
        continue;
      }
      this.socketAlive.set(socket, false);
      socket.ping();
    }
  }

  private failSocket(socket: NodeWebSocket, phase: string, error: unknown): void {
    console.error(`coordinator websocket ${phase} handler failed`, error);
    try {
      socket.close(1011, "coordinator handler failed");
    } catch (closeError) {
      console.error("coordinator websocket close failed", closeError);
      try {
        socket.terminate();
      } catch {
        // The socket is already gone.
      }
    }
  }
}

function webSocketData(data: RawData, isBinary: boolean): string | ArrayBuffer {
  if (!isBinary) {
    return data.toString();
  }
  const buffer = Array.isArray(data)
    ? Buffer.concat(data)
    : data instanceof ArrayBuffer
      ? Buffer.from(data)
      : Buffer.from(data.buffer, data.byteOffset, data.byteLength);
  return Uint8Array.from(buffer).buffer;
}
