// FIFO in-process coordination; operation failures never poison the next turn.
export class AsyncMutex {
  private tail: Promise<void> = Promise.resolve();

  async run<T>(callback: () => Promise<T>): Promise<T> {
    const previous = this.tail;
    let release!: () => void;
    this.tail = new Promise<void>((resolvePromise) => {
      release = resolvePromise;
    });
    await previous;
    try {
      return await callback();
    } finally {
      release();
    }
  }

  async drain(): Promise<void> {
    await this.tail;
  }
}

export class KeyedAsyncMutex<K> {
  private readonly entries = new Map<K, { mutex: AsyncMutex; users: number }>();

  async run<T>(key: K, callback: () => Promise<T>): Promise<T> {
    let entry = this.entries.get(key);
    if (!entry) {
      entry = { mutex: new AsyncMutex(), users: 0 };
      this.entries.set(key, entry);
    }
    entry.users++;
    try {
      return await entry.mutex.run(callback);
    } finally {
      if (--entry.users === 0) this.entries.delete(key);
    }
  }
}
