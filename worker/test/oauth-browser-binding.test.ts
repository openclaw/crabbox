import { afterEach, describe, expect, it, vi } from "vitest";

import type { CoordinatorStorage } from "../src/coordinator-runtime";
import { githubAuthRoute, githubPortalLogin } from "../src/oauth";
import type { Env } from "../src/types";

class MemoryStorage implements CoordinatorStorage {
  private readonly values = new Map<string, unknown>();

  async get<T>(key: string): Promise<T | undefined> {
    return this.values.get(key) as T | undefined;
  }

  async put<T>(key: string, value: T): Promise<void> {
    this.values.set(key, value);
  }

  async delete(key: string): Promise<void> {
    this.values.delete(key);
  }

  async list<T>({
    prefix = "",
    limit,
    startAfter,
  }: {
    prefix?: string;
    limit?: number;
    startAfter?: string;
    noCache?: boolean;
  } = {}): Promise<Map<string, T>> {
    const entries = [...this.values]
      .toSorted(([left], [right]) => left.localeCompare(right))
      .filter(([key]) => key.startsWith(prefix) && (!startAfter || key > startAfter));
    return new Map(
      (limit === undefined ? entries : entries.slice(0, limit)).map(([key, value]) => [
        key,
        value as T,
      ]),
    );
  }
}

function testRuntime(storage: MemoryStorage) {
  return {
    storage,
    async runExclusive<T>(callback: () => Promise<T>): Promise<T> {
      return callback();
    },
  };
}

const env = {
  CRABBOX_GITHUB_CLIENT_ID: "client-id",
  CRABBOX_GITHUB_CLIENT_SECRET: "client-secret",
  CRABBOX_GITHUB_ALLOWED_ORG: "openclaw",
  CRABBOX_DEFAULT_ORG: "openclaw",
  CRABBOX_PUBLIC_URL: "https://broker.test",
  CRABBOX_SESSION_SECRET: "session-secret",
} as Env;

function setCookies(response: Response): string[] {
  const headers = response.headers as Headers & { getSetCookie?: () => string[] };
  return headers.getSetCookie?.() ?? [headers.get("set-cookie") ?? ""];
}

function portalBindingCookie(response: Response): { name: string; pair: string } {
  const cookie = setCookies(response).find((value) =>
    value.startsWith("__Host-crabbox_oauth_state_"),
  );
  if (!cookie) throw new Error("missing portal OAuth binding cookie");
  const pair = cookie.split(";", 1)[0] ?? "";
  return { name: pair.split("=", 1)[0] ?? "", pair };
}

function oauthState(response: Response): string {
  const location = response.headers.get("location");
  if (!location) throw new Error("missing OAuth redirect");
  const state = new URL(location).searchParams.get("state");
  if (!state) throw new Error("missing OAuth state");
  return state;
}

async function startPortalLogin(storage: MemoryStorage): Promise<Response> {
  return githubPortalLogin(
    new Request("https://broker.test/portal/login"),
    testRuntime(storage),
    env,
  );
}

function stubSuccessfulGitHubOAuth(): ReturnType<typeof vi.fn> {
  const mock = vi.fn<(input: string | URL | Request) => Promise<Response>>(async (input) => {
    const url = input instanceof Request ? input.url : input.toString();
    if (url === "https://github.com/login/oauth/access_token") {
      return Response.json({ access_token: "github-access-token" });
    }
    if (url === "https://api.github.com/user") {
      return Response.json({ id: 12345, login: "alice", name: "Alice" });
    }
    if (url === "https://api.github.com/user/emails") {
      return Response.json([{ email: "alice@example.com", primary: true, verified: true }]);
    }
    if (url === "https://api.github.com/user/memberships/orgs/openclaw") {
      return Response.json({ state: "active", organization: { login: "openclaw" } });
    }
    throw new Error(`unexpected fetch ${url}`);
  });
  vi.stubGlobal("fetch", mock);
  return mock;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("portal OAuth browser binding", () => {
  it("stores only a hash and sets a short-lived host cookie", async () => {
    const storage = new MemoryStorage();
    const response = await startPortalLogin(storage);

    expect(response.status).toBe(302);
    const portalCookie = portalBindingCookie(response);
    expect(portalCookie.name).toMatch(/^__Host-crabbox_oauth_state_[a-f0-9]{32}$/);
    const binding = decodeURIComponent(portalCookie.pair.split("=", 2)[1] ?? "");
    expect(binding).toMatch(/^bind_[a-f0-9]{32}$/);
    const cookie = setCookies(response).find((value) => value.startsWith(`${portalCookie.name}=`));
    expect(cookie).toContain("HttpOnly");
    expect(cookie).toContain("Secure");
    expect(cookie).toContain("SameSite=Lax");
    expect(cookie).toContain("Max-Age=600");

    const pending = [
      ...(await storage.list<{ portalBindingHash?: string }>({ prefix: "oauth:" })).values(),
    ][0];
    expect(pending?.portalBindingHash).toMatch(/^[a-f0-9]{64}$/);
    expect(pending?.portalBindingHash).not.toBe(binding);
  });

  it.each([
    ["missing", undefined],
    ["wrong", "bind_00000000000000000000000000000000"],
  ])("rejects a callback with a %s browser binding", async (_label, binding) => {
    const storage = new MemoryStorage();
    const login = await startPortalLogin(storage);
    const state = oauthState(login);
    const portalCookie = portalBindingCookie(login);
    const cookie = binding ? `${portalCookie.name}=${binding}` : undefined;
    const fetchMock = vi.fn<() => void>(() => {
      throw new Error("GitHub must not be called before browser binding passes");
    });
    vi.stubGlobal("fetch", fetchMock);

    const response = await githubAuthRoute(
      new Request(
        `https://broker.test/v1/auth/github/callback?code=code&state=${encodeURIComponent(state)}`,
        { headers: cookie ? { cookie } : undefined },
      ),
      "callback",
      testRuntime(storage),
      env,
    );

    expect(response.status).toBe(403);
    expect(fetchMock).not.toHaveBeenCalled();
    expect(setCookies(response).join("\n")).toContain(
      `${portalCookie.name}=; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=0`,
    );
    expect((await storage.list({ prefix: "oauth:" })).size).toBe(0);
  });

  it("accepts the bound browser once and clears the binding cookie", async () => {
    const storage = new MemoryStorage();
    const login = await startPortalLogin(storage);
    const state = oauthState(login);
    const portalCookie = portalBindingCookie(login);
    const fetchMock = stubSuccessfulGitHubOAuth();
    const callbackURL = `https://broker.test/v1/auth/github/callback?code=code&state=${encodeURIComponent(state)}`;

    const response = await githubAuthRoute(
      new Request(callbackURL, { headers: { cookie: portalCookie.pair } }),
      "callback",
      testRuntime(storage),
      env,
    );

    expect(response.status).toBe(302);
    expect(response.headers.get("location")).toBe("/portal");
    expect(setCookies(response).join("\n")).toContain("__Host-crabbox_session=");
    expect(setCookies(response).join("\n")).toContain(
      `${portalCookie.name}=; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=0`,
    );
    expect((await storage.list({ prefix: "oauth:" })).size).toBe(0);

    const calls = fetchMock.mock.calls.length;
    const replay = await githubAuthRoute(
      new Request(callbackURL, { headers: { cookie: portalCookie.pair } }),
      "callback",
      testRuntime(storage),
      env,
    );
    expect(replay.status).toBe(400);
    expect(fetchMock).toHaveBeenCalledTimes(calls);
  });

  it("keeps concurrent portal logins independently bound", async () => {
    const storage = new MemoryStorage();
    const firstLogin = await startPortalLogin(storage);
    const secondLogin = await startPortalLogin(storage);
    const firstCookie = portalBindingCookie(firstLogin);
    const secondCookie = portalBindingCookie(secondLogin);
    expect(firstCookie.name).not.toBe(secondCookie.name);
    const browserCookies = `${firstCookie.pair}; ${secondCookie.pair}`;
    stubSuccessfulGitHubOAuth();

    const callbacks = await Promise.all(
      (
        [
          [firstLogin, firstCookie, secondCookie],
          [secondLogin, secondCookie, firstCookie],
        ] as const
      ).map(async ([login, cookie, otherCookie]) => ({
        cookie,
        otherCookie,
        response: await githubAuthRoute(
          new Request(
            `https://broker.test/v1/auth/github/callback?code=code&state=${encodeURIComponent(oauthState(login))}`,
            { headers: { cookie: browserCookies } },
          ),
          "callback",
          testRuntime(storage),
          env,
        ),
      })),
    );
    for (const { response, cookie, otherCookie } of callbacks) {
      expect(response.status).toBe(302);
      const cookies = setCookies(response).join("\n");
      expect(cookies).toContain(`${cookie.name}=;`);
      expect(cookies).not.toContain(`${otherCookie.name}=;`);
    }
    expect((await storage.list({ prefix: "oauth:" })).size).toBe(0);
  });

  it("does not clear an unrelated binding for an unknown callback state", async () => {
    const storage = new MemoryStorage();
    const login = await startPortalLogin(storage);
    const portalCookie = portalBindingCookie(login);

    const response = await githubAuthRoute(
      new Request("https://broker.test/v1/auth/github/callback?code=code&state=unknown", {
        headers: { cookie: portalCookie.pair },
      }),
      "callback",
      testRuntime(storage),
      env,
    );

    expect(response.status).toBe(400);
    expect(setCookies(response).join("\n")).not.toContain(`${portalCookie.name}=;`);
    expect((await storage.list({ prefix: "oauth:" })).size).toBeGreaterThan(0);
  });

  it("fails closed on legacy email revocation configuration", async () => {
    const storage = new MemoryStorage();
    const login = await startPortalLogin(storage);
    stubSuccessfulGitHubOAuth();

    const response = await githubAuthRoute(
      new Request(
        `https://broker.test/v1/auth/github/callback?code=code&state=${encodeURIComponent(oauthState(login))}`,
        { headers: { cookie: portalBindingCookie(login).pair } },
      ),
      "callback",
      testRuntime(storage),
      { ...env, CRABBOX_GITHUB_REVOKED_USERS: "owner:alice@example.com" },
    );

    expect(response.status).toBe(403);
    expect(await response.text()).toContain(
      "Replace email or login selectors with github:&lt;numeric-id&gt;",
    );
  });
});
