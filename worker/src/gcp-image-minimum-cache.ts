const minimumTTL = 30 * 60_000;
const maxEntries = 128;

export type GCPImageMinimum = { source: string; diskSizeGb: number };
type Entry = { expiresAt: number; value: Promise<GCPImageMinimum> };

// Preparation hints only; ownership and cleanup always require fresh observations.
export class GCPImageMinimumCache {
  private readonly entries = new Map<string, Entry>();

  get(key: string, resolve: () => Promise<GCPImageMinimum>): Promise<GCPImageMinimum> {
    const now = Date.now();
    const previous = this.entries.get(key);
    if (previous && previous.expiresAt > now) return previous.value;
    for (const [cachedKey, entry] of this.entries) {
      if (entry.expiresAt <= now) this.entries.delete(cachedKey);
    }
    if (this.entries.size >= maxEntries) {
      const oldest = this.entries.keys().next().value;
      if (oldest !== undefined) this.entries.delete(oldest);
    }
    const entry: Entry = {
      expiresAt: now + minimumTTL,
      value: Promise.resolve().then(resolve),
    };
    entry.value = entry.value.catch((error: unknown) => {
      // Evicted or invalidated in-flight reads must never alter a newer entry.
      if (this.entries.get(key) === entry) this.entries.delete(key);
      throw error;
    });
    this.entries.set(key, entry);
    return entry.value;
  }

  invalidate(): void {
    this.entries.clear();
  }
}
