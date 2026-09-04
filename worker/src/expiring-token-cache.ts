export interface ExpiringToken {
  token: string;
  expiresAt: number;
}

// One cache belongs to one authenticated client. The adapter owns token loading,
// expiry units, and refresh margin; no expired-token fallback or 401 retry occurs.
export class ExpiringTokenCache {
  private cached?: ExpiringToken;
  private pending: Promise<ExpiringToken> | undefined;

  // Scope clones retain a completed token, but own their subsequent refreshes.
  clone(): ExpiringTokenCache {
    const clone = new ExpiringTokenCache();
    if (this.cached) clone.cached = { ...this.cached };
    return clone;
  }

  async get(validAfter: number, load: () => Promise<ExpiringToken>): Promise<string> {
    if (this.cached && this.cached.expiresAt > validAfter) return this.cached.token;
    if (!this.pending) {
      // Publish the promise before running the loader, including synchronous
      // failures, so every concurrent miss joins the same refresh.
      this.pending = Promise.resolve()
        .then(load)
        .then((value) => {
          this.cached = value;
          return value;
        })
        .finally(() => {
          this.pending = undefined;
        });
    }
    return (await this.pending).token;
  }
}
