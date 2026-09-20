export class AsyncOperationTracker {
  private readonly active = new Set<Promise<unknown>>();

  async run<T>(callback: () => Promise<T>): Promise<T> {
    const operation = callback();
    this.active.add(operation);
    try {
      return await operation;
    } finally {
      this.active.delete(operation);
    }
  }

  track<T>(operation: Promise<T>): Promise<T> {
    this.active.add(operation);
    void operation.then(
      () => this.active.delete(operation),
      () => this.active.delete(operation),
    );
    return operation;
  }

  async drain(): Promise<void> {
    const active = [...this.active];
    if (active.length === 0) return;
    await Promise.allSettled(active);
    return this.drain();
  }
}
