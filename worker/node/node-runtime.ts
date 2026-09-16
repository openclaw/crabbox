import { AsyncLocalStorage } from "node:async_hooks";
import type { IncomingMessage } from "node:http";
import type { Duplex } from "node:stream";

import { PgBoss } from "pg-boss";
import { WebSocket as NodeWebSocket, WebSocketServer, type RawData } from "ws";

import {
  controlMessageOwnsTransaction,
  earliestProvisioningWake,
  legacyAlarmKey,
  mergedCoordinatorWake,
  setLegacyWake,
  type CoordinatorRuntime,
  type CoordinatorStorageView,
  type ProvisioningRuntime,
  type CoordinatorSocketHandlers,
  type CoordinatorWebSocketUpgrade,
  type CoordinatorWebSocketUpgradeOptions,
} from "../src/coordinator-runtime";
import { PostgresCoordinatorStorage } from "./postgres-storage";

const alarmQueue = "coordinator-alarm";
const alarmTimeStorageKey = "node-runtime:alarm-time";
const reconcileQueue = "coordinator-reconcile";
const bridgeDataAttachmentKinds = new Set([
  "webvnc-agent",
  "webvnc-viewer",
  "code-agent",
  "code-viewer",
  "egress-host",
  "egress-client",
  "workspace-terminal",
  "runtime-adapter-agent",
]);

export interface NodeUpgradeContext {
  request: IncomingMessage;
  socket: Duplex;
  head: Buffer;
  upgraded: boolean;
}

export class NodeCoordinatorRuntime implements CoordinatorRuntime {
  readonly provisioning: ProvisioningRuntime = this;
  readonly storage: PostgresCoordinatorStorage;
  readonly ephemeralWebSocketMaxPayloadBytes = 1024 * 1024;
  private readonly boss: PgBoss;
  private readonly webSocketServers = new Map<number, WebSocketServer>();
  private readonly upgradeContext = new AsyncLocalStorage<NodeUpgradeContext>();
  private readonly attachments = new WeakMap<WebSocket, unknown>();
  private readonly sockets = new Set<NodeWebSocket>();
  private readonly socketAlive = new WeakMap<NodeWebSocket, boolean>();
  private readonly socketOperationTails = new WeakMap<NodeWebSocket, Promise<void>>();
  private readonly activeSocketOperations = new Set<Promise<unknown>>();
  private socketClosures?: Promise<void>[];
  private shuttingDown = false;
  private alarmHandler?: () => Promise<void>;
  private operationRunner = async <T>(callback: () => Promise<T>): Promise<T> => callback();
  private alarmRun: Promise<void> = Promise.resolve();
  private readonly pingInterval: ReturnType<typeof setInterval>;
  private provisioningTick?: () => Promise<void>;
  private provisioningRun: Promise<void> | undefined;
  private wakeHintRun: Promise<void> | undefined;
  private wakeHintPending = false;
  private provisioningScanner?: ReturnType<typeof setInterval>;
  private readonly maintenance = new Set<Promise<void>>();

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
    await this.scanProvisioning();
    this.provisioningScanner = setInterval(() => {
      void this.scanProvisioning();
    }, 1_000);
    this.provisioningScanner.unref();
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

  runExclusive<T>(callback: () => Promise<T>): Promise<T> {
    return this.operationRunner(callback);
  }

  beginShutdown(): void {
    this.shuttingDown = true;
    clearInterval(this.pingInterval);
    clearInterval(this.provisioningScanner);
    this.closeSocketsForShutdown();
  }

  private closeSocketsForShutdown(): void {
    if (this.socketClosures) return;
    this.socketClosures = [...this.sockets].map((socket) => closeSocketForShutdown(socket));
  }

  async stop(): Promise<void> {
    this.beginShutdown();
    await Promise.allSettled(this.socketClosures ?? []);
    await this.drainSocketOperations();
    await this.alarmRun;
    await this.provisioningRun;
    await this.drainMaintenance();
    if (this.wakeHintRun) await boundedWakeHint(this.wakeHintRun);
    await this.boss.stop({ graceful: true, timeout: 10_000 });
    await this.storage.close();
  }

  runWithUpgrade<T>(context: NodeUpgradeContext, callback: () => Promise<T>): Promise<T> {
    return this.upgradeContext.run(context, callback);
  }

  createWebSocketUpgrade(
    options?: CoordinatorWebSocketUpgradeOptions,
  ): CoordinatorWebSocketUpgrade {
    if (this.shuttingDown) {
      throw new Error("coordinator is shutting down");
    }
    const context = this.upgradeContext.getStore();
    if (!context || context.upgraded) {
      throw new Error("websocket upgrade context is unavailable");
    }
    const maxPayload = options?.maxPayload ?? 12 * 1024 * 1024;
    let webSocketServer = this.webSocketServers.get(maxPayload);
    if (!webSocketServer) {
      webSocketServer = new WebSocketServer({
        noServer: true,
        perMessageDeflate: false,
        maxPayload,
      });
      this.webSocketServers.set(maxPayload, webSocketServer);
    }
    let accepted: NodeWebSocket | undefined;
    webSocketServer.handleUpgrade(context.request, context.socket, context.head, (socket) => {
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
      const message = webSocketData(data, isBinary);
      void this.runSocketOperation(
        nodeSocket,
        attachment,
        () => handlers.message(message),
        message,
      ).catch((error) => {
        this.failSocket(nodeSocket, "message", error);
      });
    });
    nodeSocket.on("close", (code, reason) => {
      this.sockets.delete(nodeSocket);
      void this.runSocketOperation(nodeSocket, attachment, async () => {
        handlers.close(code, reason.toString());
      }).catch((error) => {
        console.error("coordinator websocket close handler failed", error);
      });
    });
    nodeSocket.on("error", () => {
      void this.runSocketOperation(nodeSocket, attachment, async () => {
        handlers.error();
      }).catch((error) => {
        console.error("coordinator websocket error handler failed", error);
      });
    });
  }

  acceptEphemeralWebSocket(socket: WebSocket, handlers: CoordinatorSocketHandlers): void {
    this.acceptWebSocket(socket, { kind: "workspace-terminal" }, [], handlers);
  }

  async getAlarm(): Promise<number | undefined> {
    const value = await this.storage.get<unknown>(alarmTimeStorageKey);
    return typeof value === "number" && Number.isFinite(value) ? value : undefined;
  }

  take<T>(key: string): Promise<T | undefined> {
    return this.storage.take<T>(key);
  }

  async commitAndWake<T>(
    callback: (transaction: CoordinatorStorageView) => Promise<T>,
  ): Promise<T> {
    const result = await this.storage.transaction(async (transaction) => {
      if ((await transaction.get(legacyAlarmKey)) === undefined) {
        await transaction.put(
          legacyAlarmKey,
          (await transaction.get<number>(alarmTimeStorageKey)) ?? null,
        );
      }
      const value = await callback(transaction);
      const time = await mergedCoordinatorWake(transaction);
      if (time === undefined) await transaction.delete(alarmTimeStorageKey);
      else await transaction.put(alarmTimeStorageKey, time);
      return value;
    });
    // A hung queue must not hold a committed provisioning claim or its scanner.
    this.wakeHintPending = true;
    if (!this.wakeHintRun) {
      this.wakeHintRun = this.drainWakeHints().finally(() => {
        this.wakeHintRun = undefined;
      });
      await boundedWakeHint(this.wakeHintRun);
    }
    return result;
  }

  private async drainWakeHints(): Promise<void> {
    while (this.wakeHintPending && !this.shuttingDown) {
      this.wakeHintPending = false;
      // oxlint-disable-next-line eslint/no-await-in-loop -- one queue writer re-reads the latest committed outbox.
      await this.sendWakeHint();
    }
  }

  private async sendWakeHint(): Promise<void> {
    try {
      const time = await this.getAlarm();
      await this.boss.deleteQueuedJobs(alarmQueue);
      if (time !== undefined) {
        await this.boss.send(alarmQueue, null, {
          startAfter: new Date(Math.max(Date.now(), time)),
          singletonKey: "fleet",
          retryLimit: 5,
          retryDelay: 5,
          retryBackoff: true,
        });
      }
    } catch {
      console.error("coordinator wake hint failed; committed due work remains scheduled");
    }
  }

  registerProvisioningTick(tick: () => Promise<void>): void {
    this.provisioningTick = tick;
  }

  ownMaintenance(operation: Promise<void>): void {
    this.maintenance.add(operation);
    void operation.then(
      () => this.maintenance.delete(operation),
      () => this.maintenance.delete(operation),
    );
  }

  private async scanProvisioning(): Promise<void> {
    if (this.shuttingDown || this.provisioningRun || !this.provisioningTick) return;
    const run = async () => {
      try {
        const wake = await earliestProvisioningWake(this.storage);
        if (wake !== undefined && wake <= Date.now()) await this.provisioningTick!();
      } catch {
        console.error("coordinator provisioning scan failed; retrying on next scan");
      }
    };
    this.provisioningRun = run();
    try {
      await this.provisioningRun;
    } finally {
      this.provisioningRun = undefined;
    }
  }

  async scheduleAlarm(time: number): Promise<void> {
    await this.commitAndWake(async (transaction) => setLegacyWake(transaction, time));
  }

  async clearAlarm(): Promise<void> {
    await this.commitAndWake(async (transaction) => setLegacyWake(transaction));
  }

  private runAlarm(): Promise<void> {
    const run = this.alarmRun.then(async () => {
      if (this.shuttingDown) return;
      if (!this.alarmHandler) {
        throw new Error("coordinator alarm handler is unavailable");
      }
      return this.alarmHandler();
    });
    this.alarmRun = run.catch(() => undefined);
    return run;
  }

  private async drainMaintenance(): Promise<void> {
    while (this.maintenance.size > 0) {
      // oxlint-disable-next-line eslint/no-await-in-loop -- completion can latch a follow-up maintenance pass.
      await Promise.allSettled(this.maintenance);
    }
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

  private runSocketOperation<T>(
    socket: NodeWebSocket,
    attachment: unknown,
    callback: () => Promise<T> | T,
    message?: string | ArrayBuffer,
  ): Promise<T> {
    const operation = async () => callback();
    const kind = socketAttachmentKind(attachment);
    if (kind === "control" && controlMessageOwnsTransaction(message)) {
      // Heartbeats own the lifecycle transaction in shared fleet code.
      return this.trackSocketOperation(operation());
    }
    // Data-plane frames and code-agent replies must be able to complete an HTTP
    // request that currently owns the lifecycle queue. Control frames mutate
    // lease state and stay serialized with HTTP requests and alarms.
    if (!kind || !bridgeDataAttachmentKinds.has(kind)) {
      return this.trackSocketOperation(this.operationRunner(operation));
    }
    const run = (this.socketOperationTails.get(socket) ?? Promise.resolve()).then(operation);
    this.socketOperationTails.set(
      socket,
      run.then(
        () => undefined,
        () => undefined,
      ),
    );
    return this.trackSocketOperation(run);
  }

  private trackSocketOperation<T>(operation: Promise<T>): Promise<T> {
    this.activeSocketOperations.add(operation);
    void operation.then(
      () => this.activeSocketOperations.delete(operation),
      () => this.activeSocketOperations.delete(operation),
    );
    return operation;
  }

  private async drainSocketOperations(): Promise<void> {
    const active = [...this.activeSocketOperations];
    if (active.length === 0) return;
    await Promise.allSettled(active);
    return this.drainSocketOperations();
  }
}

async function boundedWakeHint(hint: Promise<void>): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<void>((resolve) => {
    timer = setTimeout(resolve, 1000);
  });
  try {
    await Promise.race([hint, timeout]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

function closeSocketForShutdown(socket: NodeWebSocket): Promise<void> {
  if (socket.readyState === NodeWebSocket.CLOSED) {
    return Promise.resolve();
  }
  let timer: ReturnType<typeof setTimeout> | undefined;
  let onClose: (() => void) | undefined;
  const closed = new Promise<void>((resolve) => {
    onClose = resolve;
    socket.once("close", onClose);
  });
  const timedOut = new Promise<void>((resolve) => {
    timer = setTimeout(() => {
      try {
        socket.terminate();
      } finally {
        resolve();
      }
    }, 2_000);
    timer.unref();
  });
  try {
    socket.close(1012, "coordinator shutting down");
  } catch {
    if (timer) clearTimeout(timer);
    if (onClose) socket.off("close", onClose);
    return Promise.resolve();
  }
  return Promise.race([closed, timedOut]).finally(() => {
    if (timer) clearTimeout(timer);
    if (onClose) socket.off("close", onClose);
  });
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

function socketAttachmentKind(attachment: unknown): string | undefined {
  if (!attachment || typeof attachment !== "object" || !("kind" in attachment)) {
    return undefined;
  }
  return String(attachment.kind);
}
