import { once } from "node:events";
import { createServer } from "node:http";

import { afterEach, describe, expect, it, vi } from "vitest";

import type { CoordinatorStorage, CoordinatorStorageView } from "../src/coordinator-runtime";
import { sha256Hex } from "../src/encoding";
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

  transaction<T>(callback: (transaction: CoordinatorStorageView) => Promise<T>): Promise<T> {
    return callback(this);
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

const cliPollSecret = "local-poll-secret";
const cliLoopbackRedirectURI = `http://127.0.0.1:54321/crabbox/oauth/${"a".repeat(64)}`;

async function startCLILogin(storage: MemoryStorage): Promise<{
  callbackURL: string;
  loginID: string;
}> {
  const start = await githubAuthRoute(
    new Request("https://broker.test/v1/auth/github/start", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        pollSecretHash: await sha256Hex(cliPollSecret),
        loopbackRedirectURI: cliLoopbackRedirectURI,
      }),
    }),
    "start",
    testRuntime(storage),
    env,
  );
  expect(start.status).toBe(200);
  const started = (await start.json()) as { loginID: string; url: string };
  const state = new URL(started.url).searchParams.get("state");
  expect(state).toBeTruthy();
  return {
    callbackURL: `https://broker.test/v1/auth/github/callback?code=code&state=${encodeURIComponent(state ?? "")}`,
    loginID: started.loginID,
  };
}

async function pollCLILogin(
  storage: MemoryStorage,
  loginID: string,
  browserConfirmation?: string | null,
): Promise<Response> {
  return githubAuthRoute(
    new Request("https://broker.test/v1/auth/github/poll", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        loginID,
        pollSecret: cliPollSecret,
        browserConfirmation,
      }),
    }),
    "poll",
    testRuntime(storage),
    env,
  );
}

function fetchCallCount(fetchMock: ReturnType<typeof vi.fn>, expectedURL: string): number {
  return fetchMock.mock.calls.filter(([input]) => {
    const url = input instanceof Request ? input.url : String(input);
    return url === expectedURL;
  }).length;
}

function runRetryDelayImmediately(): ReturnType<typeof vi.fn> {
  const original = globalThis.setTimeout;
  const retryDelay = vi.fn<(callback: () => void, delay: number) => number>((callback) => {
    callback();
    return 0;
  });
  vi.spyOn(globalThis, "setTimeout").mockImplementation(((
    callback: (...args: unknown[]) => void,
    delay?: number,
    ...args: unknown[]
  ) => {
    // Only collapse the OAuth retry delay; leave verification deadlines intact.
    if (delay === 200) return retryDelay(() => callback(...args), delay);
    return original(callback, delay, ...args);
  }) as typeof setTimeout);
  return retryDelay;
}

function stalledGitHubResponse(
  mode: "headers" | "body",
  value: unknown,
  status = 200,
): { response: Promise<Response>; finish: () => void } {
  let finish!: () => void;
  let finished = false;
  const response =
    mode === "headers"
      ? new Promise<Response>((resolve) => {
          finish = () => resolve(Response.json(value, { status }));
        })
      : Promise.resolve(
          new Response(
            new ReadableStream<Uint8Array>({
              start(controller) {
                const bytes = new TextEncoder().encode(JSON.stringify(value));
                controller.enqueue(bytes.slice(0, 1));
                finish = () => {
                  controller.enqueue(bytes.slice(1));
                  controller.close();
                };
              },
            }),
            { status },
          ),
        );
  return {
    response,
    finish() {
      if (!finished) {
        finished = true;
        finish();
      }
    },
  };
}

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
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

describe("GitHub OAuth transient failures", () => {
  it.each([
    { mode: "headers", status: 200 },
    { mode: "body", status: 200 },
    { mode: "body", status: 403 },
  ] as const)(
    "bounds stalled OAuth exchange $mode/$status without retrying or late credential writes",
    async ({ mode, status }) => {
      const storage = new MemoryStorage();
      const { callbackURL, loginID } = await startCLILogin(storage);
      const fetchMock = stubSuccessfulGitHubOAuth();
      vi.useFakeTimers();
      let started!: () => void;
      const entered = new Promise<void>((resolve) => {
        started = resolve;
      });
      const stalled = stalledGitHubResponse(
        mode,
        status === 200 ? { access_token: "github-access-token" } : { error: "upstream-error" },
        status,
      );
      fetchMock.mockImplementationOnce(() => {
        started();
        return stalled.response;
      });
      let callbackStatus: number | undefined;
      const callback = githubAuthRoute(
        new Request(callbackURL),
        "callback",
        testRuntime(storage),
        env,
      ).then((response) => {
        callbackStatus = response.status;
        return response;
      });
      try {
        await entered;
        await vi.advanceTimersByTimeAsync(15_000);
        expect(callbackStatus).toBe(503);
        expect(fetchMock).toHaveBeenCalledOnce();
        const pending = structuredClone(
          await storage.get<Record<string, unknown>>(`oauth:${loginID}`),
        );
        expect(pending).toMatchObject({ id: loginID });
        expect(pending?.githubCredential).toBeUndefined();
        expect(pending?.token).toBeUndefined();
        expect(pending?.callbackClaim).toBeUndefined();
        stalled.finish();
        await vi.advanceTimersByTimeAsync(0);
        expect(await storage.get(`oauth:${loginID}`)).toEqual(pending);
        expect(fetchMock).toHaveBeenCalledOnce();
      } finally {
        stalled.finish();
        await callback;
      }
    },
  );

  it("shares each post-exchange deadline across identity, email, and membership checks", async () => {
    const storage = new MemoryStorage();
    const { callbackURL, loginID } = await startCLILogin(storage);
    const fetchMock = stubSuccessfulGitHubOAuth();
    const successful = fetchMock.getMockImplementation();
    if (!successful) throw new Error("GitHub fixture required");
    vi.useFakeTimers();
    let started!: () => void;
    const entered = new Promise<void>((resolve) => {
      started = resolve;
    });
    fetchMock.mockImplementation((input: string | URL | Request) => {
      const url = input instanceof Request ? input.url : String(input);
      if (url === "https://github.com/login/oauth/access_token") return successful(input);
      started();
      return new Promise<Response>((resolve) =>
        setTimeout(() => resolve(successful(input)), 6_000),
      );
    });
    let callbackStatus: number | undefined;
    const callback = githubAuthRoute(
      new Request(callbackURL),
      "callback",
      testRuntime(storage),
      env,
    ).then((response) => {
      callbackStatus = response.status;
      return response;
    });
    try {
      await entered;
      await vi.advanceTimersByTimeAsync(30_200);
      expect(callbackStatus).toBe(503);
      expect(fetchCallCount(fetchMock, "https://github.com/login/oauth/access_token")).toBe(1);
      for (const path of [
        "/user",
        "/user/emails",
        `/user/memberships/orgs/${env.CRABBOX_DEFAULT_ORG}`,
      ]) {
        expect(fetchCallCount(fetchMock, `https://api.github.com${path}`)).toBe(2);
      }
      const pending = structuredClone(
        await storage.get<Record<string, unknown>>(`oauth:${loginID}`),
      );
      expect(pending).toMatchObject({ id: loginID, githubCredential: expect.any(String) });
      expect(JSON.stringify(pending)).not.toContain("github-access-token");
      expect(pending?.token).toBeUndefined();
      expect(pending?.callbackClaim).toBeUndefined();
      await vi.runOnlyPendingTimersAsync();
      expect(await storage.get(`oauth:${loginID}`)).toEqual(pending);
      fetchMock.mockImplementation(successful);
      const resumed = await githubAuthRoute(
        new Request(callbackURL),
        "callback",
        testRuntime(storage),
        env,
      );
      expect(resumed.status).toBe(303);
      expect(fetchCallCount(fetchMock, "https://github.com/login/oauth/access_token")).toBe(1);
    } finally {
      await vi.runAllTimersAsync();
      await callback;
    }
  });

  it("aborts a real HTTP OAuth exchange without retrying or persisting partial credentials", async () => {
    const storage = new MemoryStorage();
    const { callbackURL, loginID } = await startCLILogin(storage);
    const nativeFetch = fetch;
    let requests = 0;
    let connectionClosed = false;
    const server = createServer((request, response) => {
      requests += 1;
      request.resume();
      response.on("close", () => {
        connectionClosed = true;
      });
      response.writeHead(200, { "content-type": "application/json" });
      response.write('{"access_token":');
    });
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    const address = server.address();
    if (!address || typeof address === "string") throw new Error("loopback server required");
    let headersReceived!: () => void;
    const received = new Promise<void>((resolve) => {
      headersReceived = resolve;
    });
    vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        expect(String(input)).toBe("https://github.com/login/oauth/access_token");
        expect(init?.method).toBe("POST");
        const response = await nativeFetch(
          `http://127.0.0.1:${address.port}/login/oauth/access_token`,
          init,
        );
        headersReceived();
        return response;
      }),
    );
    const callback = githubAuthRoute(
      new Request(callbackURL),
      "callback",
      testRuntime(storage),
      env,
    );
    try {
      await received;
      await vi.advanceTimersByTimeAsync(15_000);
      expect((await callback).status).toBe(503);
      vi.useRealTimers();
      await vi.waitFor(() => expect(connectionClosed).toBe(true));
      expect(requests).toBe(1);
      const pending = await storage.get<Record<string, unknown>>(`oauth:${loginID}`);
      expect(pending).toMatchObject({ id: loginID });
      expect(pending?.githubCredential).toBeUndefined();
      expect(pending?.token).toBeUndefined();
      expect(pending?.callbackClaim).toBeUndefined();
    } finally {
      vi.useRealTimers();
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) =>
        server.close((error) => (error ? reject(error) : resolve())),
      );
    }
  });

  it("does not automatically retry a transient OAuth code exchange", async () => {
    const storage = new MemoryStorage();
    const { callbackURL, loginID } = await startCLILogin(storage);
    const fetchMock = vi.fn<() => Promise<Response>>(async () =>
      Response.json({ message: "Service Unavailable" }, { status: 503 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const unavailable = await githubAuthRoute(
      new Request(callbackURL),
      "callback",
      testRuntime(storage),
      env,
    );
    expect(unavailable.status).toBe(503);
    expect(fetchMock).toHaveBeenCalledOnce();
    const pending = [
      ...(
        await storage.list<{ githubCredential?: string }>({
          prefix: "oauth:",
        })
      ).values(),
    ][0];
    expect(pending?.githubCredential).toBeUndefined();

    const waiting = await pollCLILogin(storage, loginID);
    expect(waiting.status).toBe(200);
    await expect(waiting.json()).resolves.toMatchObject({ status: "pending" });
  });

  it("retries a one-time user lookup 503 inside the original callback", async () => {
    const retryDelay = runRetryDelayImmediately();
    const storage = new MemoryStorage();
    const { callbackURL, loginID } = await startCLILogin(storage);

    let userLookups = 0;
    const fetchMock = vi.fn<(input: string | URL | Request) => Promise<Response>>(async (input) => {
      const url = input instanceof Request ? input.url : input.toString();
      if (url === "https://github.com/login/oauth/access_token") {
        return Response.json({ access_token: "github-access-token" });
      }
      if (url === "https://api.github.com/user") {
        userLookups += 1;
        return userLookups === 1
          ? Response.json({ message: "Service Unavailable" }, { status: 503 })
          : Response.json({ id: 12345, login: "alice", name: "Alice" });
      }
      if (url === "https://api.github.com/user/emails") {
        return Response.json([{ email: "alice@example.com", primary: true, verified: true }]);
      }
      if (url === "https://api.github.com/user/memberships/orgs/openclaw") {
        return Response.json({ state: "active", organization: { login: "openclaw" } });
      }
      throw new Error(`unexpected fetch ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const callback = await githubAuthRoute(
      new Request(callbackURL),
      "callback",
      testRuntime(storage),
      env,
    );
    expect(callback.status).toBe(303);
    const confirmation = new URL(callback.headers.get("location") ?? "").searchParams.get(
      "confirmation",
    );
    expect(confirmation).toMatch(/^confirm_[a-f0-9]{32}$/);
    expect(userLookups).toBe(2);
    expect(retryDelay).toHaveBeenCalledOnce();
    expect(retryDelay.mock.calls[0]?.[1]).toBe(200);
    expect(fetchCallCount(fetchMock, "https://github.com/login/oauth/access_token")).toBe(1);

    const complete = await pollCLILogin(storage, loginID, confirmation);
    expect(complete.status).toBe(200);
    await expect(complete.json()).resolves.toMatchObject({
      status: "complete",
      owner: "github:12345",
      org: "openclaw",
      login: "alice",
    });
  });

  it("keeps a persistent outage retryable with only the sealed exchanged credential", async () => {
    const retryDelay = runRetryDelayImmediately();
    const storage = new MemoryStorage();
    const { callbackURL, loginID } = await startCLILogin(storage);
    let recovered = false;
    let membershipLookups = 0;
    const fetchMock = vi.fn<(input: string | URL | Request) => Promise<Response>>(async (input) => {
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
        membershipLookups += 1;
        return recovered
          ? Response.json({ state: "active", organization: { login: "openclaw" } })
          : Response.json({ message: "Service Unavailable" }, { status: 503 });
      }
      throw new Error(`unexpected fetch ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const unavailable = await githubAuthRoute(
      new Request(callbackURL),
      "callback",
      testRuntime(storage),
      env,
    );
    expect(unavailable.status).toBe(503);
    expect(unavailable.headers.get("cache-control")).toBe("no-store");
    expect(unavailable.headers.get("referrer-policy")).toBe("no-referrer");
    expect(unavailable.headers.get("retry-after")).toBe("2");
    const retryPage = await unavailable.text();
    expect(retryPage).toContain("The Crabbox CLI is still waiting");
    expect(retryPage).toContain("Retry GitHub login");
    expect(membershipLookups).toBe(2);
    expect(retryDelay).toHaveBeenCalledOnce();
    expect(retryDelay.mock.calls[0]?.[1]).toBe(200);

    const pending = [
      ...(
        await storage.list<{ callbackClaim?: string; githubCredential?: string }>({
          prefix: "oauth:",
        })
      ).values(),
    ][0];
    expect(pending?.callbackClaim).toBeUndefined();
    expect(pending?.githubCredential).toBeTruthy();
    expect(JSON.stringify(pending)).not.toContain("github-access-token");

    const waiting = await pollCLILogin(storage, loginID);
    expect(waiting.status).toBe(200);
    await expect(waiting.json()).resolves.toMatchObject({ status: "pending" });

    recovered = true;
    const retry = await githubAuthRoute(
      new Request(callbackURL),
      "callback",
      testRuntime(storage),
      env,
    );
    expect(retry.status).toBe(303);
    const confirmation = new URL(retry.headers.get("location") ?? "").searchParams.get(
      "confirmation",
    );
    expect(confirmation).toMatch(/^confirm_[a-f0-9]{32}$/);

    const complete = await pollCLILogin(storage, loginID, confirmation);
    expect(complete.status).toBe(200);
    await expect(complete.json()).resolves.toMatchObject({
      status: "complete",
      owner: "github:12345",
      org: "openclaw",
      login: "alice",
    });
    expect(membershipLookups).toBe(3);
    expect(fetchCallCount(fetchMock, "https://github.com/login/oauth/access_token")).toBe(1);
  });

  it("preserves portal browser binding across the retry page", async () => {
    const retryDelay = runRetryDelayImmediately();
    const storage = new MemoryStorage();
    const login = await startPortalLogin(storage);
    const portalCookie = portalBindingCookie(login);
    const callbackURL = `https://broker.test/v1/auth/github/callback?code=code&state=${encodeURIComponent(oauthState(login))}`;
    let recovered = false;
    const fetchMock = vi.fn<(input: string | URL | Request) => Promise<Response>>(async (input) => {
      const url = input instanceof Request ? input.url : input.toString();
      if (url === "https://github.com/login/oauth/access_token") {
        return Response.json({ access_token: "github-access-token" });
      }
      if (url === "https://api.github.com/user") {
        return recovered
          ? Response.json({ id: 12345, login: "alice", name: "Alice" })
          : Response.json({ message: "Service Unavailable" }, { status: 503 });
      }
      if (url === "https://api.github.com/user/emails") {
        return Response.json([{ email: "alice@example.com", primary: true, verified: true }]);
      }
      if (url === "https://api.github.com/user/memberships/orgs/openclaw") {
        return Response.json({ state: "active", organization: { login: "openclaw" } });
      }
      throw new Error(`unexpected fetch ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const unavailable = await githubAuthRoute(
      new Request(callbackURL, { headers: { cookie: portalCookie.pair } }),
      "callback",
      testRuntime(storage),
      env,
    );
    expect(unavailable.status).toBe(503);
    expect(await unavailable.text()).toContain("Your Crabbox portal login is still waiting");
    expect(setCookies(unavailable).join("\n")).not.toContain(`${portalCookie.name}=;`);
    expect(retryDelay).toHaveBeenCalledOnce();

    recovered = true;
    const complete = await githubAuthRoute(
      new Request(callbackURL, { headers: { cookie: portalCookie.pair } }),
      "callback",
      testRuntime(storage),
      env,
    );
    expect(complete.status).toBe(302);
    expect(complete.headers.get("location")).toBe("/portal");
    expect(setCookies(complete).join("\n")).toContain("__Host-crabbox_session=");
    expect(fetchCallCount(fetchMock, "https://github.com/login/oauth/access_token")).toBe(1);
  });
});
