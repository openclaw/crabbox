import { afterEach, describe, expect, it, vi } from "vitest";

import type { Env } from "../src/types";

const env = {
  CRABBOX_SESSION_SECRET: "synthetic-membership-cache-secret",
  CRABBOX_GITHUB_ALLOWED_ORG: "example-org",
  CRABBOX_GITHUB_ALLOWED_TEAMS: "example-org/operators",
  CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "300",
} as Env;

function sharedCache() {
  // Keep expired responses too: the application must enforce absolute expiry.
  const entries = new Map<string, Response>();
  const cache = {
    match: vi.fn<(key: Request) => Promise<Response | undefined>>(async (key) =>
      entries.get(key.url)?.clone(),
    ),
    put: vi.fn<(key: Request, response: Response) => Promise<void>>(async (key, response) => {
      entries.set(key.url, response.clone());
    }),
  };
  vi.stubGlobal("caches", { default: cache });
  return { ...cache, entries };
}

function github(allowed = true) {
  const fetchMock = vi.fn<typeof fetch>(async (input) => {
    const url = String(input);
    if (url.endsWith("/user")) return Response.json({ id: 12345, login: "alice" });
    if (url.includes("/user/memberships/")) {
      return Response.json({ state: "active", organization: { login: "example-org" } });
    }
    if (url.includes("/user/teams?")) {
      return Response.json(
        allowed ? [{ slug: "operators", organization: { login: "example-org" } }] : [],
      );
    }
    throw new Error("Unexpected GitHub endpoint");
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

async function handler() {
  vi.resetModules();
  return (await import("../src/auth")).authenticateRequest;
}

async function token(ttlSeconds?: number) {
  const { issueUserToken } = await import("../src/auth");
  return issueUserToken(env, {
    owner: "github:12345",
    ownerSource: "github-verified-email",
    org: "example-org",
    login: "alice",
    githubAccessToken: "synthetic-github-credential",
    ttlSeconds,
  });
}

function request(value: string) {
  return new Request("https://coordinator.example.test/v1/whoami", {
    headers: { authorization: `Bearer ${value}` },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("regional GitHub membership cache", () => {
  it("shares a positive check across independently initialized authentication handlers", async () => {
    const cache = sharedCache();
    const fetchMock = github();
    const first = await handler();
    const value = await token();
    await expect(first(request(value), env)).resolves.toMatchObject({ authorized: true });
    const second = await handler();
    await expect(second(request(value), env)).resolves.toMatchObject({ authorized: true });
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(cache.put).toHaveBeenCalledTimes(1);
    expect(cache.match).toHaveBeenCalledTimes(2);
    // The warm first-level map does not even consult the shared cache.
    await expect(second(request(value), env)).resolves.toMatchObject({ authorized: true });
    expect(cache.match).toHaveBeenCalledTimes(2);
    const [key, response] = [...cache.entries][0]!;
    expect(key).toMatch(/^https:\/\/[^/]+\/github-membership\/v1\/[a-f0-9]{64}$/);
    for (const privateValue of [
      value,
      "synthetic-github-credential",
      "github:12345",
      "operators",
      env.CRABBOX_SESSION_SECRET!,
    ]) {
      expect(key).not.toContain(privateValue);
    }
    expect(response.headers.get("cache-control")).toBe("max-age=300");
    expect(await response.clone().text()).toMatch(/^\d+$/);
  });

  it("misses when the allowed teams change", async () => {
    const cache = sharedCache();
    const fetchMock = github();
    const first = await handler();
    const value = await token();
    await first(request(value), env);
    const second = await handler();
    await expect(
      second(request(value), {
        ...env,
        CRABBOX_GITHUB_ALLOWED_TEAMS: "example-org/release-captains",
      }),
    ).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledTimes(6);
    expect(cache.put).toHaveBeenCalledTimes(1);
  });

  it("never caches denials", async () => {
    const cache = sharedCache();
    const fetchMock = github(false);
    const value = await token();
    const first = await handler();
    await expect(first(request(value), env)).resolves.toBeUndefined();
    await expect(first(request(value), env)).resolves.toBeUndefined();
    const second = await handler();
    await expect(second(request(value), env)).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledTimes(9);
    expect(cache.put).not.toHaveBeenCalled();
  });

  it("partitions the shared cache by the server secret", async () => {
    const cache = sharedCache();
    const fetchMock = github();
    const identity = {
      accessToken: "synthetic-github-credential",
      tokenID: "synthetic-token-identity",
      owner: "github:12345",
      org: "example-org",
      login: "alice",
    };
    vi.resetModules();
    const first = await import("../src/github-membership");
    await first.requireCurrentGitHubMembership(identity, env);
    vi.resetModules();
    const second = await import("../src/github-membership");
    await second.requireCurrentGitHubMembership(identity, {
      ...env,
      CRABBOX_SESSION_SECRET: "rotated-synthetic-secret",
    });
    expect(fetchMock).toHaveBeenCalledTimes(6);
    expect(cache.entries.size).toBe(2);
  });

  it.each(["invalid", String(Number.MAX_SAFE_INTEGER)])(
    "ignores invalid or unbounded shared expiry %s",
    async (body) => {
      const cache = sharedCache();
      const fetchMock = github();
      const first = await handler();
      const value = await token();
      await first(request(value), env);
      const [key] = cache.entries.keys();
      cache.entries.set(key!, new Response(body));
      const second = await handler();
      await expect(second(request(value), env)).resolves.toMatchObject({ authorized: true });
      expect(fetchMock).toHaveBeenCalledTimes(6);
    },
  );

  it.each(["300", "99999"])(
    "preserves absolute expiry for TTL %s across both cache levels",
    async (configuredTTL) => {
      vi.useFakeTimers();
      vi.setSystemTime(new Date("2026-01-01T00:00:00Z"));
      const cache = sharedCache();
      const fetchMock = github();
      const config = { ...env, CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: configuredTTL };
      const ttl = Math.min(Number(configuredTTL), 3600);
      const first = await handler();
      const value = await token();
      await first(request(value), config);
      expect([...cache.entries.values()][0]!.headers.get("cache-control")).toBe(`max-age=${ttl}`);
      vi.setSystemTime(Date.now() + ttl * 1000 - 1);
      const second = await handler();
      await expect(second(request(value), config)).resolves.toMatchObject({ authorized: true });
      expect(fetchMock).toHaveBeenCalledTimes(3);
      vi.setSystemTime(Date.now() + 1);
      fetchMock.mockResolvedValue(new Response(null, { status: 503 }));
      await expect(second(request(value), config)).resolves.toBeUndefined();
      const third = await handler();
      await expect(third(request(value), config)).resolves.toBeUndefined();
      expect(fetchMock).toHaveBeenCalledTimes(5);
      expect(cache.put).toHaveBeenCalledTimes(1);
    },
  );

  it("bypasses shared entries when caching is disabled or the TTL is shortened", async () => {
    const cache = sharedCache();
    const fetchMock = github();
    const first = await handler();
    const value = await token();
    await first(request(value), env);
    await first(request(value), { ...env, CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "0" });
    const second = await handler();
    await second(request(value), { ...env, CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "60" });
    expect(fetchMock).toHaveBeenCalledTimes(9);
    expect(cache.put).toHaveBeenCalledTimes(2);
  });

  it("still rejects configured revocations and expired signed tokens with a warm shared entry", async () => {
    sharedCache();
    const fetchMock = github();
    vi.useFakeTimers();
    const first = await handler();
    const value = await token(3600);
    const config = { ...env, CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "3600" };
    await first(request(value), config);
    const second = await handler();
    await expect(
      second(request(value), { ...config, CRABBOX_GITHUB_REVOKED_USERS: "github:12345" }),
    ).resolves.toBeUndefined();
    vi.setSystemTime(Date.now() + 3600 * 1000);
    await expect(second(request(value), config)).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("keeps the Node fallback isolate-local when the Cache API is absent", async () => {
    vi.stubGlobal("caches", undefined);
    const fetchMock = github();
    const first = await handler();
    const value = await token();
    await first(request(value), env);
    await first(request(value), env);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    const second = await handler();
    await expect(second(request(value), env)).resolves.toMatchObject({ authorized: true });
    expect(fetchMock).toHaveBeenCalledTimes(6);
  });

  it("falls back to GitHub if cache reads or writes fail", async () => {
    const cache = sharedCache();
    cache.match.mockRejectedValue(new Error("cache unavailable"));
    cache.put.mockRejectedValue(new Error("cache unavailable"));
    const fetchMock = github();
    const first = await handler();
    const value = await token();
    await expect(first(request(value), env)).resolves.toMatchObject({ authorized: true });
    const second = await handler();
    await expect(second(request(value), env)).resolves.toMatchObject({ authorized: true });
    expect(fetchMock).toHaveBeenCalledTimes(6);
  });
});
