import { once } from "node:events";
import { createServer } from "node:http";

import { afterEach, describe, expect, it, vi } from "vitest";

import { authenticateRequest, issueUserToken } from "../src/auth";
import { prepareCoordinatorRequest, routeCoordinatorRequest } from "../src/coordinator-entry";
import {
  githubMembershipPolicy,
  requireCurrentGitHubMembership,
  requireFreshGitHubMembership,
} from "../src/github-membership";
import { GitHubTransientError } from "../src/github-request";
import type { Env } from "../src/types";

const accessToken = "github-access-token-for-tests";
const accountID = 12345;
const liveAccessToken = process.env.CRABBOX_GITHUB_LIVE_TOKEN;
const liveOrg = process.env.CRABBOX_GITHUB_LIVE_ORG;
const liveLogin = process.env.CRABBOX_GITHUB_LIVE_LOGIN;
const liveAccountID = Number(process.env.CRABBOX_GITHUB_LIVE_ID);
const liveMembershipConfigured = Boolean(
  liveAccessToken &&
  liveOrg &&
  liveLogin &&
  Number.isSafeInteger(liveAccountID) &&
  liveAccountID > 0,
);

function testEnv(overrides: Partial<Env> = {}): Env {
  return {
    CRABBOX_SESSION_SECRET: "session-secret",
    CRABBOX_DEFAULT_ORG: "example-org",
    CRABBOX_GITHUB_ALLOWED_ORG: "example-org",
    CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "0",
    ...overrides,
  } as Env;
}

async function testToken(env: Env): Promise<string> {
  return issueUserToken(env, {
    owner: `github:${accountID}`,
    ownerSource: "github-verified-email",
    org: "example-org",
    login: "alice",
    githubAccessToken: accessToken,
  });
}

function tokenRequest(token: string, path = "/v1/whoami", method = "GET"): Request {
  return new Request(`https://broker.example.test${path}`, {
    method,
    headers: { authorization: `Bearer ${token}` },
  });
}

function userResponse(id = accountID, login = "alice"): Response {
  return Response.json({ id, login });
}

function membershipResponse(state = "active", org = "example-org"): Response {
  return Response.json({ state, organization: { login: org } });
}

function teamIdentity(org = "example-org"): {
  accessToken: string;
  tokenID: string;
  owner: string;
  org: string;
  login: string;
} {
  return {
    accessToken,
    tokenID: `team-scope-token-${org}`,
    owner: `github:${accountID}`,
    org,
    login: "alice",
  };
}

function membershipFetch(): ReturnType<typeof vi.fn> {
  return vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
    async (input, init) => {
      expect(new Headers(init?.headers).get("authorization")).toBe(`Bearer ${accessToken}`);
      const url = String(input);
      if (url === "https://api.github.com/user") return userResponse();
      if (url === "https://api.github.com/user/memberships/orgs/example-org") {
        return membershipResponse();
      }
      throw new Error(`unexpected fetch ${url}`);
    },
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("GitHub user-token membership", () => {
  it("bounds a stalled GitHub request to the complete verification deadline", async () => {
    vi.useFakeTimers();
    let finishRequest!: (response: Response) => void;
    const stalled = new Promise<Response>((resolve) => {
      finishRequest = resolve;
    });
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockReturnValueOnce(stalled)
      .mockResolvedValue(membershipResponse());
    vi.stubGlobal("fetch", fetchMock);
    let outcome = "pending";
    const operation = requireFreshGitHubMembership(teamIdentity(), testEnv()).then(
      () => {
        outcome = "authorized";
        return undefined;
      },
      (error: unknown) => {
        outcome = error instanceof GitHubTransientError ? "timeout" : "other";
        return undefined;
      },
    );
    try {
      await vi.advanceTimersByTimeAsync(15_000);
      expect(outcome).toBe("timeout");
    } finally {
      finishRequest(userResponse());
      await operation;
    }
  });

  it.each([200, 403])(
    "bounds a stalled %i response body without reclassifying expiry",
    async (status) => {
      vi.useFakeTimers();
      let finishBody!: () => void;
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          finishBody = () => {
            controller.enqueue(new TextEncoder().encode(JSON.stringify({ id: accountID })));
            controller.close();
          };
        },
      });
      const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response(body, { status }));
      vi.stubGlobal("fetch", fetchMock);
      const operation = requireFreshGitHubMembership(teamIdentity(), testEnv());
      const rejected = operation.catch((error: unknown) => error);
      try {
        await vi.advanceTimersByTimeAsync(15_000);
        expect(await rejected).toBeInstanceOf(GitHubTransientError);
        expect(fetchMock).toHaveBeenCalledTimes(1);
        expect(vi.getTimerCount()).toBe(0);
      } finally {
        finishBody();
        await operation.catch(() => {});
      }
    },
  );

  it("aborts the real HTTP transport when GitHub sends headers but stalls its JSON body", async () => {
    const nativeFetch = fetch;
    let connectionClosed = false;
    const server = createServer((_request, response) => {
      response.on("close", () => {
        connectionClosed = true;
      });
      response.writeHead(200, { "content-type": "application/json" });
      response.write('{"id":');
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
        const url = new URL(String(input));
        expect(url.origin).toBe("https://api.github.com");
        const response = await nativeFetch(`http://127.0.0.1:${address.port}${url.pathname}`, init);
        headersReceived();
        return response;
      }),
    );
    const operation = requireFreshGitHubMembership(teamIdentity(), testEnv());
    const rejected = operation.catch((error: unknown) => error);
    try {
      await received;
      await vi.advanceTimersByTimeAsync(15_000);
      expect(await rejected).toBeInstanceOf(GitHubTransientError);
      vi.useRealTimers();
      await vi.waitFor(() => expect(connectionClosed).toBe(true));
    } finally {
      vi.useRealTimers();
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) =>
        server.close((error) => (error ? reject(error) : resolve())),
      );
    }
  });

  it("shares one deadline across team pages rather than restarting the budget", async () => {
    vi.useFakeTimers();
    const env = testEnv({ CRABBOX_GITHUB_ALLOWED_TEAMS: "example-org/operators" });
    const fetchMock = vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
      if (String(input).endsWith("/user")) return userResponse();
      if (String(input).includes("/memberships/")) return membershipResponse();
      return await new Promise((resolve) =>
        setTimeout(
          () =>
            resolve(
              Response.json(
                Array.from({ length: 100 }, () => ({
                  slug: "other",
                  organization: { login: "example-org" },
                })),
              ),
            ),
          4_000,
        ),
      );
    });
    vi.stubGlobal("fetch", fetchMock);
    const operation = requireFreshGitHubMembership(teamIdentity(), env);
    const rejected = operation.catch((error: unknown) => error);
    await vi.advanceTimersByTimeAsync(15_000);
    expect(await rejected).toBeInstanceOf(GitHubTransientError);
    expect(
      fetchMock.mock.calls.filter(([input]) => String(input).includes("/user/teams?")),
    ).toHaveLength(4);
    await vi.runOnlyPendingTimersAsync();
    expect(fetchMock).toHaveBeenCalledTimes(6);
  });

  it("releases shared checks after expiry and never caches their late success", async () => {
    vi.useFakeTimers();
    const env = testEnv({ CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "300" });
    const identity = { ...teamIdentity(), tokenID: "deadline-shared-membership" };
    let finishRequest!: (response: Response) => void;
    const stalled = new Promise<Response>((resolve) => {
      finishRequest = resolve;
    });
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockReturnValueOnce(stalled)
      .mockImplementation(async (input: RequestInfo | URL) =>
        String(input).endsWith("/user") ? userResponse() : membershipResponse(),
      );
    vi.stubGlobal("fetch", fetchMock);
    const outcomes = Promise.allSettled([
      requireCurrentGitHubMembership(identity, env),
      requireCurrentGitHubMembership(identity, env),
    ]);
    await vi.advanceTimersByTimeAsync(15_000);
    for (const result of await outcomes) {
      expect(result).toMatchObject({
        status: "rejected",
        reason: expect.any(GitHubTransientError),
      });
    }
    expect(fetchMock).toHaveBeenCalledTimes(1);
    finishRequest(userResponse());
    await vi.advanceTimersByTimeAsync(0);
    await requireCurrentGitHubMembership(identity, env);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    await requireCurrentGitHubMembership(identity, env);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(vi.getTimerCount()).toBe(0);
  });

  it("rejects create and cancellation requests without reaching the coordinator after auth expiry", async () => {
    const env = testEnv();
    const token = await testToken(env);
    vi.useFakeTimers();
    let finishRequest!: (response: Response) => void;
    let requestStarted!: () => void;
    let membershipChecks = 0;
    const started = new Promise<void>((resolve) => {
      requestStarted = resolve;
    });
    const stalled = new Promise<Response>((resolve) => {
      finishRequest = resolve;
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(() => stalled),
    );
    const downstream = vi.fn<() => Promise<Response>>(async () =>
      Response.json({ unexpected: true }),
    );
    const requests = Promise.all(
      ["/v1/leases", "/v1/leases/cbx_000000000001/cancel-create"].map((path) =>
        routeCoordinatorRequest(tokenRequest(token, path, "POST"), env, downstream, {
          githubMembership(identity, membershipEnv) {
            const check = requireCurrentGitHubMembership(identity, membershipEnv);
            if (++membershipChecks === 2) requestStarted();
            return check;
          },
        }),
      ),
    );
    try {
      await started;
      await vi.advanceTimersByTimeAsync(15_000);
      for (const response of await requests) expect(response.status).toBe(401);
      expect(downstream).not.toHaveBeenCalled();
    } finally {
      finishRequest(userResponse());
      await requests;
    }
  });

  it("encrypts the GitHub credential inside the signed user token", async () => {
    const token = await testToken(testEnv());
    const encoded = token.slice("cbxu_".length).split(".", 1)[0]!;
    const payload = JSON.parse(
      atob(
        encoded
          .replaceAll("-", "+")
          .replaceAll("_", "/")
          .padEnd(Math.ceil(encoded.length / 4) * 4, "="),
      ),
    ) as Record<string, unknown>;

    expect(JSON.stringify(payload)).not.toContain(accessToken);
    expect(payload).toMatchObject({
      version: 3,
      owner: `github:${accountID}`,
      org: "example-org",
      login: "alice",
    });
    expect(payload.githubCredential).toEqual(expect.any(String));
  });

  it("periodically revalidates the immutable account and organization membership", async () => {
    const env = testEnv({ CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "300" });
    const token = await testToken(env);
    const fetchMock = membershipFetch();
    vi.stubGlobal("fetch", fetchMock);

    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toMatchObject({
      authorized: true,
      owner: `github:${accountID}`,
      org: "example-org",
      login: "alice",
    });
    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toMatchObject({
      authorized: true,
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("supports uncached membership checks for device principals", async () => {
    const env = testEnv({ CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "300" });
    const fetchMock = membershipFetch();
    vi.stubGlobal("fetch", fetchMock);
    const identity = {
      accessToken,
      tokenID: crypto.randomUUID(),
      owner: `github:${accountID}`,
      org: "example-org",
      login: "alice",
    };

    await requireFreshGitHubMembership(identity, env);
    await requireFreshGitHubMembership(identity, env);

    expect(fetchMock).toHaveBeenCalledTimes(4);
  });

  it("rejects a credential whose GitHub account id no longer matches the session", async () => {
    const env = testEnv();
    const token = await testToken(env);
    const fetchMock = vi.fn<() => Promise<Response>>(async () => userResponse(67890, "alice"));
    vi.stubGlobal("fetch", fetchMock);

    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("rejects legacy email-owned sessions before calling GitHub", async () => {
    const env = testEnv();
    const token = await issueUserToken(env, {
      owner: "alice@example.com",
      ownerSource: "github-verified-email",
      org: "example-org",
      login: "alice",
      githubAccessToken: accessToken,
    });
    const fetchMock = vi.fn<() => void>();
    vi.stubGlobal("fetch", fetchMock);

    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toBeUndefined();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it.each([
    ["membership removal", async (): Promise<Response> => userResponse()],
    [
      "GitHub outage",
      async (): Promise<Response> => {
        throw new Error("GitHub unavailable");
      },
    ],
  ])("fails closed after %s", async (label, firstResult) => {
    const env = testEnv();
    const token = await testToken(env);
    const fetchMock = vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
      const url = String(input);
      if (label === "membership removal" && url === "https://api.github.com/user") {
        return firstResult();
      }
      if (label === "membership removal") return new Response(null, { status: 404 });
      return firstResult();
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toBeUndefined();
  });

  it("fails closed when a required team membership is removed", async () => {
    const env = testEnv({ CRABBOX_GITHUB_ALLOWED_TEAM: "example-org/operators" });
    const token = await testToken(env);
    vi.stubGlobal(
      "fetch",
      vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
        const url = String(input);
        if (url === "https://api.github.com/user") return userResponse();
        if (url.includes("/user/teams")) return Response.json([]);
        return membershipResponse();
      }),
    );

    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toBeUndefined();
  });

  it.each([
    ["a missing org", "/operators"],
    ["a missing slug", "example-org/"],
    ["an extra path component", "example-org/operators/extra"],
    ["only commas", ", ,"],
    ["a valid selector mixed with an empty entry", "operators,"],
    ["an invalid org token", "example_org/operators"],
    ["an invalid team token", "example-org/operator!"],
  ])("fails closed on allowed-team configuration with %s", async (_label, teams) => {
    const env = testEnv({
      CRABBOX_GITHUB_ALLOWED_TEAMS: teams,
      CRABBOX_GITHUB_ALLOWED_TEAM: "fallback-team",
    });
    const token = await testToken(env);
    const fetchMock = vi.fn<() => void>();
    vi.stubGlobal("fetch", fetchMock);

    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toBeUndefined();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it.each([
    ["an unset team policy", {}, []],
    ["an empty plural policy", { CRABBOX_GITHUB_ALLOWED_TEAMS: "" }, []],
    [
      "an empty plural policy with singular fallback",
      { CRABBOX_GITHUB_ALLOWED_TEAMS: "", CRABBOX_GITHUB_ALLOWED_TEAM: "operators" },
      ["example-org/operators"],
    ],
    [
      "a whitespace-only plural policy with singular fallback",
      { CRABBOX_GITHUB_ALLOWED_TEAMS: " \t ", CRABBOX_GITHUB_ALLOWED_TEAM: "operators" },
      ["example-org/operators"],
    ],
    [
      "a singular-only policy",
      { CRABBOX_GITHUB_ALLOWED_TEAM: "example-org/operators" },
      ["example-org/operators"],
    ],
    [
      "a configured plural policy taking precedence over singular",
      {
        CRABBOX_GITHUB_ALLOWED_TEAMS: "release-captains",
        CRABBOX_GITHUB_ALLOWED_TEAM: "operators",
      },
      ["example-org/release-captains"],
    ],
  ])("normalizes %s", (_label, overrides, expectedTeams) => {
    const policy = githubMembershipPolicy(teamIdentity(), testEnv(overrides));

    expect(policy.allowedTeams).toEqual(expectedTeams);
  });

  it.each([
    [
      "a team qualified for another configured org",
      "example-org/operators,partner-org/contractors",
    ],
    ["no team configured for the org being authorized", "partner-org/contractors"],
  ])("fails closed on %s", async (_label, teams) => {
    const env = testEnv({ CRABBOX_GITHUB_ALLOWED_TEAMS: teams });
    vi.stubGlobal(
      "fetch",
      vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
        const url = String(input);
        if (url === "https://api.github.com/user") return userResponse();
        if (url.includes("/user/teams")) {
          return Response.json([{ slug: "contractors", organization: { login: "partner-org" } }]);
        }
        return membershipResponse();
      }),
    );

    await expect(requireFreshGitHubMembership(teamIdentity(), env)).rejects.toThrow(/example-org/);
  });

  it.each([
    ["an org-qualified entry", "example-org/operators,partner-org/contractors"],
    ["an unqualified entry resolved against the authorized org", "operators"],
  ])("accepts a team membership matched by %s", async (_label, teams) => {
    const env = testEnv({ CRABBOX_GITHUB_ALLOWED_TEAMS: teams });
    vi.stubGlobal(
      "fetch",
      vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
        const url = String(input);
        if (url === "https://api.github.com/user") return userResponse();
        if (url.includes("/user/teams")) {
          return Response.json([{ slug: "operators", organization: { login: "example-org" } }]);
        }
        return membershipResponse();
      }),
    );

    await expect(requireFreshGitHubMembership(teamIdentity(), env)).resolves.toBeUndefined();
  });

  it.each(["example-org", "partner-org"])(
    "binds a bare team slug to the selected organization %s",
    (org) => {
      const env = testEnv({
        CRABBOX_GITHUB_ALLOWED_ORG: undefined,
        CRABBOX_GITHUB_ALLOWED_ORGS: "example-org,partner-org",
        CRABBOX_GITHUB_ALLOWED_TEAMS: "operators",
      });

      expect(githubMembershipPolicy(teamIdentity(org), env).allowedTeams).toEqual([
        `${org}/operators`,
      ]);
    },
  );

  it("allows an org-qualified team only for that exact selected organization", async () => {
    const env = testEnv({
      CRABBOX_GITHUB_ALLOWED_ORG: undefined,
      CRABBOX_GITHUB_ALLOWED_ORGS: "example-org,partner-org",
      CRABBOX_GITHUB_ALLOWED_TEAMS: "partner-org/contractors",
    });
    const fetchMock = vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
      const url = String(input);
      if (url === "https://api.github.com/user") return userResponse();
      if (url === "https://api.github.com/user/memberships/orgs/partner-org") {
        return membershipResponse("active", "partner-org");
      }
      if (url.includes("/user/teams")) {
        return Response.json([{ slug: "contractors", organization: { login: "partner-org" } }]);
      }
      throw new Error(`unexpected fetch ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(requireFreshGitHubMembership(teamIdentity("example-org"), env)).rejects.toThrow(
      /no allowed team configured/,
    );
    expect(fetchMock).not.toHaveBeenCalled();
    await expect(
      requireFreshGitHubMembership(teamIdentity("partner-org"), env),
    ).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("does not migrate a session to another allowed organization", async () => {
    const env = testEnv({
      CRABBOX_DEFAULT_ORG: "partner-org",
      CRABBOX_GITHUB_ALLOWED_ORG: "partner-org",
      CRABBOX_GITHUB_ALLOWED_TEAMS: "partner-org/contractors",
    });
    const fetchMock = vi.fn<() => void>();
    vi.stubGlobal("fetch", fetchMock);

    await expect(requireFreshGitHubMembership(teamIdentity("example-org"), env)).rejects.toThrow(
      /no longer allowed/,
    );
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("invalidates the ordinary membership cache when a valid team policy changes", async () => {
    const env = testEnv({
      CRABBOX_GITHUB_ALLOWED_TEAMS: "example-org/operators",
      CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "300",
    });
    const token = await testToken(env);
    const fetchMock = vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
      const url = String(input);
      if (url === "https://api.github.com/user") return userResponse();
      if (url.includes("/user/teams")) {
        return Response.json([{ slug: "operators", organization: { login: "example-org" } }]);
      }
      return membershipResponse();
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toMatchObject({
      authorized: true,
    });
    env.CRABBOX_GITHUB_ALLOWED_TEAMS = "example-org/release-captains";
    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledTimes(6);
  });

  it("rejects malformed team policy before consulting a warm ordinary cache", async () => {
    const env = testEnv({
      CRABBOX_GITHUB_ALLOWED_TEAMS: "example-org/operators",
      CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "300",
    });
    const token = await testToken(env);
    const fetchMock = vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) => {
      const url = String(input);
      if (url === "https://api.github.com/user") return userResponse();
      if (url.includes("/user/teams")) {
        return Response.json([{ slug: "operators", organization: { login: "example-org" } }]);
      }
      return membershipResponse();
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toMatchObject({
      authorized: true,
    });
    env.CRABBOX_GITHUB_ALLOWED_TEAMS = "operators,";
    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it.each([`github:${accountID}`, `owner:github:${accountID}`])(
    "applies narrow revocation %s before the GitHub cache",
    async (revoked) => {
      const env = testEnv({ CRABBOX_GITHUB_REVOKED_USERS: revoked });
      const token = await testToken(env);
      const fetchMock = vi.fn<() => void>();
      vi.stubGlobal("fetch", fetchMock);

      await expect(authenticateRequest(tokenRequest(token), env)).resolves.toBeUndefined();
      expect(fetchMock).not.toHaveBeenCalled();
    },
  );

  it.each(["alice@example.com", "owner:alice@example.com", "alice", "login:alice", "owner:alice"])(
    "fails closed on legacy or invalid revocation selector %s",
    async (revoked) => {
      const env = testEnv({ CRABBOX_GITHUB_REVOKED_USERS: revoked });
      const token = await testToken(env);
      const fetchMock = vi.fn<() => void>();
      vi.stubGlobal("fetch", fetchMock);

      await expect(authenticateRequest(tokenRequest(token), env)).resolves.toBeUndefined();
      expect(fetchMock).not.toHaveBeenCalled();
    },
  );

  it("rejects every coordinator capability before routing a revoked user", async () => {
    const env = testEnv({ CRABBOX_GITHUB_REVOKED_USERS: `github:${accountID}` });
    const token = await testToken(env);
    const preparedRequests = await Promise.all(
      [
        ["/v1/leases", "POST"],
        ["/v1/runs", "POST"],
        ["/v1/artifacts/uploads", "POST"],
        ["/v1/leases/cbx_000000000001/code", "GET"],
      ].map(async ([path, method]) =>
        prepareCoordinatorRequest(tokenRequest(token, path!, method!), env),
      ),
    );
    for (const prepared of preparedRequests) {
      expect(prepared).toMatchObject({ authenticated: false, response: { status: 401 } });
    }
  });

  it("rejects tokens after their exact organization leaves the allowed policy", async () => {
    const issuingEnv = testEnv();
    const token = await testToken(issuingEnv);
    const env = testEnv({
      CRABBOX_DEFAULT_ORG: "other-org",
      CRABBOX_GITHUB_ALLOWED_ORG: "other-org",
    });
    const fetchMock = vi.fn<() => void>();
    vi.stubGlobal("fetch", fetchMock);

    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toBeUndefined();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it.skipIf(!liveMembershipConfigured)(
    "live: accepts a current organization member through the encrypted user-token path",
    async () => {
      const env = testEnv({
        CRABBOX_DEFAULT_ORG: liveOrg,
        CRABBOX_GITHUB_ALLOWED_ORG: liveOrg,
      });
      const token = await issueUserToken(env, {
        owner: `github:${liveAccountID}`,
        ownerSource: "github-verified-email",
        org: liveOrg!,
        login: liveLogin!,
        githubAccessToken: liveAccessToken!,
      });

      await expect(authenticateRequest(tokenRequest(token), env)).resolves.toMatchObject({
        authorized: true,
        owner: `github:${liveAccountID}`,
        org: liveOrg,
        login: liveLogin,
      });
    },
  );
});
