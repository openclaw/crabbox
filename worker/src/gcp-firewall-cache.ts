const verificationTTL = 5 * 60_000;
const maxEntries = 128;

type Entry = { fingerprint: string; expiresAt: number };

// Memory only. A hit proves a recent exact GET, never resource ownership for cleanup.
export class GCPFirewallCache {
  private readonly entries = new Map<string, Entry>();

  async ensure(key: string, fingerprint: string, verify: () => Promise<boolean>): Promise<void> {
    const now = Date.now();
    const previous = this.entries.get(key);
    if (previous?.fingerprint === fingerprint && previous.expiresAt > now) return;
    for (const [cachedKey, entry] of this.entries) {
      if (entry.expiresAt <= now) this.entries.delete(cachedKey);
    }
    this.entries.delete(key);
    if (this.entries.size >= maxEntries) {
      const oldest = this.entries.keys().next().value;
      if (oldest !== undefined) this.entries.delete(oldest);
    }
    const entry = { fingerprint, expiresAt: 0 };
    this.entries.set(key, entry);
    try {
      const exact = await verify();
      // A completed write can invalidate a newer GET that observed the old policy.
      if (!exact) {
        this.entries.delete(key);
        return;
      }
      // Errors, changed policy, and newer reads fence late results from older requests.
      if (this.entries.get(key) !== entry) return;
      if (now + verificationTTL > Date.now()) entry.expiresAt = now + verificationTTL;
      else this.entries.delete(key);
    } catch (error) {
      this.invalidate();
      throw error;
    }
  }

  invalidate(): void {
    this.entries.clear();
  }
}
