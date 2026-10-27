import { AsyncMutex } from "./async-mutex";
import { recordDurableObjectStorageFailure } from "./durable-object-errors";
export interface CoordinatorStorageView {
  get<T>(
    key: string,
    options?: { noCache?: boolean; allowConcurrency?: boolean },
  ): Promise<T | undefined>;
  put<T>(key: string, value: T, options?: { noCache?: boolean }): Promise<void>;
  delete(key: string): Promise<unknown>;
  list<T>(options?: {
    prefix?: string;
    limit?: number;
    startAfter?: string;
    end?: string;
    reverse?: boolean;
    noCache?: boolean;
    allowConcurrency?: boolean;
  }): Promise<Map<string, T>>;
}

export interface CoordinatorStorage extends CoordinatorStorageView {
  // Implementations may retry callbacks after serialization conflicts; callbacks must contain
  // only storage reads/writes and must not perform external or otherwise non-idempotent effects.
  transaction<T>(callback: (transaction: CoordinatorStorageView) => Promise<T>): Promise<T>;
}

// Runtime flags are not unique to storage errors. Record provenance without wrapping
// the error so admission retries and provider errors keep their original identity.
async function storageOperation<T>(operation: () => Promise<T>): Promise<T> {
  try {
    return await operation();
  } catch (error) {
    recordDurableObjectStorageFailure(error);
    throw error;
  }
}

function trackedStorageView<T extends CoordinatorStorageView>(storage: T): T {
  const operations = new Set<PropertyKey>([
    "get",
    "put",
    "delete",
    "deleteAll",
    "list",
    "getAlarm",
    "setAlarm",
    "deleteAlarm",
  ]);
  return new Proxy(storage, {
    get: (target, key) => {
      const value: unknown = Reflect.get(target, key, target);
      if (typeof value !== "function") return value;
      if (operations.has(key)) {
        return (...args: unknown[]) => storageOperation(() => Reflect.apply(value, target, args));
      }
      return value.bind(target);
    },
  });
}

export const provisioningDuePrefix = "provisioning-due:";
export const legacyAlarmKey = "runtime:legacy-alarm";

export interface ProvisioningDueRecord {
  kind?: "pool-access";
  operationID: string;
  at: number;
}

export async function earliestProvisioningWake(
  storage: CoordinatorStorageView,
): Promise<number | undefined> {
  const entries = await storage.list<ProvisioningDueRecord>({
    prefix: provisioningDuePrefix,
    limit: 1,
  });
  if (entries.size === 0) return undefined;
  const at = entries.values().next().value?.at;
  // A malformed index value still needs the bounded controller tick to quarantine it.
  return typeof at === "number" && Number.isFinite(at) ? at : Date.now();
}

export async function mergedCoordinatorWake(
  storage: CoordinatorStorageView,
): Promise<number | undefined> {
  const legacy = await storage.get<number | null>(legacyAlarmKey);
  const provisioning = await earliestProvisioningWake(storage);
  if (legacy == null) return provisioning;
  return provisioning === undefined ? legacy : Math.min(legacy, provisioning);
}

export async function setLegacyWake(storage: CoordinatorStorageView, time?: number): Promise<void> {
  await storage.put(legacyAlarmKey, time ?? null);
}

export interface ProvisioningRuntime {
  commitAndWake<T>(callback: (transaction: CoordinatorStorageView) => Promise<T>): Promise<T>;
  registerProvisioningTick(tick: () => Promise<void>): void;
  ownMaintenance(operation: Promise<void>): void;
}

export type CoordinatorRequestQueue = "direct" | "lifecycle";

export function coordinatorRequestPublishesRunEvents(request: Request): boolean {
  const path = new URL(request.url).pathname.split("/").filter(Boolean);
  const method = request.method.toUpperCase();
  return (
    path[0] === "v1" &&
    path[1] === "runs" &&
    ((method === "POST" && path.length === 2) ||
      (method === "PUT" && path.length === 3) ||
      (["POST", "PUT"].includes(method) &&
        path.length === 4 &&
        ["events", "finish", "admission-failure"].includes(path[3] ?? "")))
  );
}

export function coordinatorRequestQueue(request: Request): CoordinatorRequestQueue {
  const url = new URL(request.url);
  const path = url.pathname.split("/").filter(Boolean);
  const method = request.method.toUpperCase();
  // These routes commit with storage transactions; subscriber authorization may use GitHub.
  if (coordinatorRequestPublishesRunEvents(request)) return "direct";
  if (path[0] === "v1" && (path[1] === "serve" || path[1] === "ssh-hosts")) return "direct";
  if (method === "POST" && path.join("/") === "v1/admin/tailscale-preflight") {
    return "direct";
  }
  if (
    method === "GET" &&
    [
      "v1/whoami",
      "v1/capacity",
      "v1/leases",
      "v1/admin/leases",
      "v1/admin/retained-reconcile",
    ].includes(path.join("/"))
  ) {
    return "direct";
  }
  if (
    (method === "POST" && path.join("/") === "v1/auth/github/start") ||
    (method === "GET" && path.join("/") === "portal/login")
  ) {
    return "direct";
  }
  if (method === "GET" && path.join("/") === "v1/auth/github/callback") {
    return "direct";
  }
  if (
    path[0] === "v1" &&
    ((path[1] === "pairing" && (path[2] === "grants" || path[2] === "exchange")) ||
      path[1] === "devices")
  ) {
    return "direct";
  }
  if (
    method === "POST" &&
    (path.join("/") === "v1/leases" ||
      path.join("/") === "v1/leases/capability-aware" ||
      path.join("/") === "v1/leases/resource-constrained" ||
      path.join("/") === "v1/leases/from-checkpoint")
  ) {
    return "direct";
  }
  if (path[0] === "v1" && path[1] === "checkpoints") {
    return "direct";
  }
  if (
    method === "PUT" &&
    path[0] === "v1" &&
    path[1] === "leases" &&
    path[2] &&
    (path.length === 3 ||
      (path.length === 4 && ["from-checkpoint", "resource-constrained"].includes(path[3] ?? "")))
  ) {
    return "direct";
  }
  if (path[0] === "v1" && path[1] === "workspaces") {
    return "direct";
  }
  if (method === "GET" && path.join("/") === "v1/control") {
    // Admission only binds this socket; control messages retain their lifecycle fences.
    return "direct";
  }
  if (method === "GET" && path.join("/") === "v1/native-vnc/handoff") {
    return "direct";
  }
  if (method === "POST" && path.join("/") === "v1/internal/scheduled") {
    return "direct";
  }
  if (path[0] === "v1" && path[1] === "ready-pools") {
    return "direct";
  }
  if (
    path[0] === "v1" &&
    path[1] === "images" &&
    path[2] &&
    ((method === "POST" &&
      path.length === 4 &&
      (path[3] === "promote" || path[3] === "promote-cas" || path[3] === "promote-catalog")) ||
      (method === "DELETE" &&
        (path.length === 3 ||
          (path.length === 4 && (path[3] === "promote-catalog" || path[3] === "promote")))))
  ) {
    // Existing-image mutations intentionally hold the lifecycle queue across provider I/O:
    // these rare admin calls must validate, re-read, clean up, and publish as one serialized commit.
    return "lifecycle";
  }
  if (path[0] === "v1" && path[1] === "images") {
    return "direct";
  }
  if (
    path[0] === "v1" &&
    path[1] === "adapters" &&
    ((method === "GET" && path.length === 3) || path[3] === "proxy")
  ) {
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
    path.length === 4 &&
    path[3] === "cleanup" &&
    (method === "GET" || method === "POST")
  ) {
    // Provider reads stay outside the queue; recovery owns its short final commit fence.
    return "direct";
  }
  if (
    path[0] === "v1" &&
    path[1] === "leases" &&
    path[2] &&
    method === "GET" &&
    path.length === 3
  ) {
    return "direct";
  }
  if (
    path[0] === "v1" &&
    path[1] === "leases" &&
    path[2] &&
    method === "POST" &&
    (path[3] === "heartbeat" ||
      path[3] === "extend" ||
      path[3] === "tailscale" ||
      path[3] === "release" ||
      path[3] === "cancel-create")
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
    method === "GET" &&
    path.length === 5 &&
    path[0] === "v1" &&
    path[1] === "admin" &&
    path[2] === "leases" &&
    path[4] === "lifetime-readback"
  ) {
    // Read-only provider readback; holding the lifecycle queue would stall admissions on AWS I/O.
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

export interface CoordinatorWebSocketUpgradeOptions {
  maxPayload?: number;
}

export interface CoordinatorRuntime {
  readonly storage: CoordinatorStorage;
  readonly ephemeralWebSocketMaxPayloadBytes: number;
  runExclusive<T>(callback: () => Promise<T>): Promise<T>;
  readSnapshot?<T>(
    callback: (storage: CoordinatorStorageView) => Promise<T>,
  ): Promise<T | undefined>;
  createWebSocketUpgrade(options?: CoordinatorWebSocketUpgradeOptions): CoordinatorWebSocketUpgrade;
  getWebSockets(): Iterable<WebSocket>;
  socketAttachment<T>(socket: WebSocket): T | undefined;
  setSocketAttachment(socket: WebSocket, attachment: unknown): void;
  acceptWebSocket(
    socket: WebSocket,
    attachment: unknown,
    tags: string[],
    handlers: CoordinatorSocketHandlers,
  ): void;
  acceptEphemeralWebSocket(
    socket: WebSocket,
    handlers: CoordinatorSocketHandlers,
    kind?: string,
  ): void;
  take<T>(key: string): Promise<T | undefined>;
  getAlarm(): Promise<number | undefined>;
  scheduleAlarm(time: number): Promise<void>;
  clearAlarm(): Promise<void>;
  readonly provisioning?: ProvisioningRuntime;
}

export class CloudflareCoordinatorRuntime implements CoordinatorRuntime {
  readonly provisioning: ProvisioningRuntime = this;
  readonly storage: DurableObjectStorage;
  readonly ephemeralWebSocketMaxPayloadBytes = 32 * 1024 * 1024;
  private readonly attachments = new WeakMap<WebSocket, unknown>();
  private readonly exclusive = new AsyncMutex();
  private storageRevision = 0;
  private pendingStorageWrites = 0;

  constructor(private readonly state: DurableObjectState) {
    const mutations = new Set<PropertyKey>([
      "put",
      "delete",
      "deleteAll",
      "setAlarm",
      "deleteAlarm",
    ]);
    this.storage = new Proxy(trackedStorageView(state.storage), {
      get: (target, key) => {
        const value: unknown = Reflect.get(target, key, target);
        if (typeof value !== "function") return value;
        if (key === "transaction") {
          return (callback: (transaction: DurableObjectTransaction) => Promise<unknown>) =>
            this.observeStorageWrite(() => this.storageTransaction(callback));
        }
        if (key === "transactionSync") {
          return (...args: unknown[]) => {
            this.storageRevision++;
            try {
              return Reflect.apply(value, target, args);
            } finally {
              this.storageRevision++;
            }
          };
        }
        if (mutations.has(key)) {
          return (...args: unknown[]) =>
            this.observeStorageWrite(() => Reflect.apply(value, target, args));
        }
        return value.bind(target);
      },
    });
  }

  private async storageTransaction<T>(
    callback: (transaction: DurableObjectTransaction) => Promise<T>,
  ): Promise<T> {
    const callbackFailures = new Set<unknown>();
    try {
      return await this.state.storage.transaction(async (transaction) => {
        try {
          return await callback(trackedStorageView(transaction));
        } catch (error) {
          callbackFailures.add(error);
          throw error;
        }
      });
    } catch (error) {
      // Callback failures are application errors unless a native call already marked them.
      if (!callbackFailures.has(error)) recordDurableObjectStorageFailure(error);
      throw error;
    }
  }

  private async observeStorageWrite<T>(operation: () => Promise<T>): Promise<T> {
    this.storageRevision++;
    this.pendingStorageWrites++;
    try {
      return await operation();
    } finally {
      this.pendingStorageWrites--;
      this.storageRevision++;
    }
  }

  async readSnapshot<T>(
    callback: (storage: CoordinatorStorageView) => Promise<T>,
  ): Promise<T | undefined> {
    // Native async transactions close the DO input gate for the whole callback.
    // Concurrent diagnostic reads instead fail closed if any write overlaps.
    if (this.pendingStorageWrites > 0) return undefined;
    const revision = this.storageRevision;
    const result = await callback(this.storage);
    return this.pendingStorageWrites === 0 && revision === this.storageRevision
      ? result
      : undefined;
  }

  runExclusive<T>(callback: () => Promise<T>): Promise<T> {
    return this.exclusive.run(callback);
  }

  createWebSocketUpgrade(
    _options?: CoordinatorWebSocketUpgradeOptions,
  ): CoordinatorWebSocketUpgrade {
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

  acceptEphemeralWebSocket(
    socket: WebSocket,
    handlers: CoordinatorSocketHandlers,
    _kind?: string,
  ): void {
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

  async getAlarm(): Promise<number | undefined> {
    return (await this.storage.getAlarm()) ?? undefined;
  }

  take<T>(key: string): Promise<T | undefined> {
    return this.storage.transaction(async (transaction) => {
      const value = await transaction.get<T>(key);
      if (value !== undefined) {
        await transaction.delete(key);
      }
      return value;
    });
  }

  async commitAndWake<T>(
    callback: (transaction: CoordinatorStorageView) => Promise<T>,
  ): Promise<T> {
    return this.storage.transaction(async (transaction) => {
      const currentAlarm = await transaction.getAlarm();
      if ((await transaction.get(legacyAlarmKey)) === undefined) {
        if (currentAlarm !== null) await transaction.put(legacyAlarmKey, currentAlarm);
      }
      const result = await callback(transaction);
      const wake = await mergedCoordinatorWake(transaction);
      if (wake === undefined) {
        if (currentAlarm !== null) await transaction.deleteAlarm();
      } else if (wake !== currentAlarm || wake <= Date.now()) {
        // A retained due timestamp may outlive its consumed job; only future wakes are reusable.
        await transaction.setAlarm(wake);
      }
      return result;
    });
  }

  registerProvisioningTick(_tick: () => Promise<void>): void {
    // Constructor recovery is storage-only; provider I/O belongs to a later alarm.
    this.state.blockConcurrencyWhile(async () => {
      await this.commitAndWake(async () => undefined);
    });
  }

  ownMaintenance(operation: Promise<void>): void {
    this.state.waitUntil(operation);
  }

  scheduleAlarm(time: number): Promise<void> {
    return this.commitAndWake(async (transaction) => setLegacyWake(transaction, time));
  }

  clearAlarm(): Promise<void> {
    return this.commitAndWake(async (transaction) => setLegacyWake(transaction));
  }
}
