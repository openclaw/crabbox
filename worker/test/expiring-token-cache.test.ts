import { describe, expect, it, vi } from "vitest";

import { ExpiringTokenCache, type ExpiringToken } from "../src/expiring-token-cache";

describe("per-client expiring token cache", () => {
  it("shares a pending refresh and refreshes at the exact margin", async () => {
    const cache = new ExpiringTokenCache();
    const first = Promise.withResolvers<ExpiringToken>();
    const load = vi.fn<() => Promise<ExpiringToken>>(() => first.promise);
    const requests = [cache.get(100, load), cache.get(100, load)];
    first.resolve({ token: "first", expiresAt: 200 });
    await expect(Promise.all(requests)).resolves.toEqual(["first", "first"]);
    expect(load).toHaveBeenCalledTimes(1);
    await expect(cache.get(199, load)).resolves.toBe("first");
    expect(load).toHaveBeenCalledTimes(1);

    const refresh = vi.fn<() => Promise<ExpiringToken>>(async () => ({
      token: "second",
      expiresAt: 300,
    }));
    await expect(Promise.all([cache.get(200, refresh), cache.get(200, refresh)])).resolves.toEqual([
      "second",
      "second",
    ]);
    expect(refresh).toHaveBeenCalledTimes(1);
  });

  it("shares failures without caching them or falling back to an expired token", async () => {
    const cache = new ExpiringTokenCache();
    await cache.get(100, async () => ({ token: "expired", expiresAt: 200 }));
    const failure = new Error("refresh denied");
    const load = vi.fn<() => Promise<ExpiringToken>>(async () => {
      throw failure;
    });
    const results = await Promise.allSettled([cache.get(200, load), cache.get(200, load)]);
    expect(results).toEqual([
      { status: "rejected", reason: failure },
      { status: "rejected", reason: failure },
    ]);
    expect(load).toHaveBeenCalledTimes(1);
    await expect(
      cache.get(200, async () => ({ token: "recovered", expiresAt: 300 })),
    ).resolves.toBe("recovered");
  });

  it("clears a synchronous loader failure before a later retry", async () => {
    const cache = new ExpiringTokenCache();
    const load = vi.fn<() => Promise<ExpiringToken>>((): Promise<ExpiringToken> => {
      throw new Error("local credential unavailable");
    });
    await expect(cache.get(100, load)).rejects.toThrow("local credential unavailable");
    await expect(
      cache.get(100, async () => ({ token: "recovered", expiresAt: 200 })),
    ).resolves.toBe("recovered");
  });

  it("keeps client caches and in-flight refreshes isolated", async () => {
    const first = new ExpiringTokenCache();
    const second = new ExpiringTokenCache();
    const firstLoad = Promise.withResolvers<ExpiringToken>();
    const pending = first.get(100, () => firstLoad.promise);
    await expect(
      second.get(100, async () => ({ token: "second-client", expiresAt: 200 })),
    ).resolves.toBe("second-client");
    firstLoad.resolve({ token: "first-client", expiresAt: 200 });
    await expect(pending).resolves.toBe("first-client");
    await expect(first.get(199, () => Promise.reject(new Error("not needed")))).resolves.toBe(
      "first-client",
    );
  });

  it("does not reuse a short-lived response on the next request", async () => {
    const cache = new ExpiringTokenCache();
    const load = vi.fn<() => Promise<ExpiringToken>>(async () => ({
      token: "short-lived",
      expiresAt: 90,
    }));
    await cache.get(100, load);
    await cache.get(100, load);
    expect(load).toHaveBeenCalledTimes(2);
  });
});
