import type {
  CoordinatorRuntime,
  CoordinatorStorage,
  CoordinatorStorageView,
  ProvisioningRuntime,
} from "../src/coordinator-runtime";
import { mergedCoordinatorWake, setLegacyWake } from "../src/coordinator-runtime";

class TransactionView implements CoordinatorStorageView {
  constructor(
    readonly values: Map<string, unknown>,
    private readonly fail: (key: string) => void,
    private readonly beforeGet?: (key: string) => Promise<void>,
  ) {}
  async get<T>(key: string): Promise<T | undefined> {
    await this.beforeGet?.(key);
    return structuredClone(this.values.get(key)) as T | undefined;
  }
  async put<T>(key: string, value: T): Promise<void> {
    this.fail(key);
    this.values.set(key, structuredClone(value));
  }
  async delete(key: string): Promise<void> {
    this.fail(key);
    this.values.delete(key);
  }
  async list<T>(
    options: { prefix?: string; limit?: number; startAfter?: string } = {},
  ): Promise<Map<string, T>> {
    const entries = [...this.values]
      .toSorted(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
      .filter(
        ([key]) =>
          key.startsWith(options.prefix ?? "") && (!options.startAfter || key > options.startAfter),
      );
    return new Map(
      entries
        .slice(0, options.limit ?? entries.length)
        .map(([key, value]) => [key, structuredClone(value) as T]),
    );
  }
  async getAlarm(): Promise<number | null> {
    return (await this.get<number>("test:native-alarm")) ?? null;
  }
  async setAlarm(time: number): Promise<void> {
    await this.put("test:native-alarm", time);
  }
  async deleteAlarm(): Promise<void> {
    await this.delete("test:native-alarm");
  }
}

// Transactions mutate an isolated snapshot, including the native alarm, and publish only on commit.
export class ProvisioningTestStorage implements CoordinatorStorage {
  values = new Map<string, unknown>();
  beforeGet: (key: string) => Promise<void> = async () => {};
  private tail = Promise.resolve();
  failKey?: string;
  private view(): TransactionView {
    return new TransactionView(
      this.values,
      (key) => {
        if (key === this.failKey) throw new Error("injected storage failure");
      },
      this.beforeGet,
    );
  }
  get<T>(key: string): Promise<T | undefined> {
    return this.view().get<T>(key);
  }
  put<T>(key: string, value: T): Promise<void> {
    return this.transaction((transaction) => transaction.put(key, value));
  }
  delete(key: string): Promise<void> {
    return this.transaction((transaction) => transaction.delete(key));
  }
  list<T>(options?: Parameters<CoordinatorStorageView["list"]>[0]): Promise<Map<string, T>> {
    return this.view().list<T>(options);
  }
  getAlarm(): Promise<number | null> {
    return this.view().getAlarm();
  }
  setAlarm(time: number): Promise<void> {
    return this.transaction((transaction) => transaction.setAlarm(time));
  }
  deleteAlarm(): Promise<void> {
    return this.transaction((transaction) => transaction.deleteAlarm());
  }
  async transaction<T>(callback: (transaction: TransactionView) => Promise<T>): Promise<T> {
    const previous = this.tail;
    let release!: () => void;
    this.tail = new Promise<void>((resolve) => {
      release = resolve;
    });
    await previous;
    const view = new TransactionView(
      structuredClone(this.values),
      (key) => {
        if (key === this.failKey) throw new Error("injected storage failure");
      },
      this.beforeGet,
    );
    try {
      const value = await callback(view);
      this.values = view.values;
      return value;
    } finally {
      release();
    }
  }
}

export class ProvisioningTestRuntime implements CoordinatorRuntime, ProvisioningRuntime {
  readonly provisioning = this;
  readonly ephemeralWebSocketMaxPayloadBytes = 1024 * 1024;
  readonly maintenance = new Set<Promise<void>>();
  tick?: () => Promise<void>;
  constructor(readonly storage: ProvisioningTestStorage) {}
  runExclusive<T>(callback: () => Promise<T>): Promise<T> {
    return callback();
  }
  async commitAndWake<T>(
    callback: (transaction: CoordinatorStorageView) => Promise<T>,
  ): Promise<T> {
    return this.storage.transaction(async (transaction) => {
      const result = await callback(transaction);
      const wake = await mergedCoordinatorWake(transaction);
      if (wake === undefined) await transaction.deleteAlarm();
      else await transaction.setAlarm(wake);
      return result;
    });
  }
  registerProvisioningTick(tick: () => Promise<void>): void {
    this.tick = tick;
  }
  ownMaintenance(operation: Promise<void>): void {
    this.maintenance.add(operation);
    void operation.finally(() => this.maintenance.delete(operation));
  }
  scheduleAlarm(time: number): Promise<void> {
    return this.commitAndWake((transaction) => setLegacyWake(transaction, time));
  }
  clearAlarm(): Promise<void> {
    return this.commitAndWake((transaction) => setLegacyWake(transaction));
  }
  async getAlarm(): Promise<number | undefined> {
    return (await this.storage.getAlarm()) ?? undefined;
  }
  take<T>(key: string): Promise<T | undefined> {
    return this.storage.transaction(async (transaction) => {
      const value = await transaction.get<T>(key);
      await transaction.delete(key);
      return value;
    });
  }
  getWebSockets(): WebSocket[] {
    return [];
  }
  socketAttachment<T>(): T | undefined {
    return undefined;
  }
  setSocketAttachment(): void {}
  acceptWebSocket(): void {}
  acceptEphemeralWebSocket(): void {}
  createWebSocketUpgrade(): never {
    throw new Error("unused");
  }
}
