import { afterEach, describe, expect, it, vi } from "vitest";

import { authenticateRequest, issueUserToken } from "../src/auth";
import { prepareCoordinatorRequest } from "../src/coordinator-entry";
import type { Env } from "../src/types";

const accessToken = "github-access-token-for-tests";

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
    owner: "alice@example.com",
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

function membershipResponse(state = "active", org = "example-org"): Response {
  return Response.json({ state, organization: { login: org } });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("GitHub user-token membership", () => {
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
    expect(payload).toMatchObject({ version: 3, org: "example-org", login: "alice" });
    expect(payload.githubCredential).toEqual(expect.any(String));
  });

  it("periodically revalidates active organization membership", async () => {
    const env = testEnv({ CRABBOX_GITHUB_MEMBERSHIP_CACHE_SECONDS: "300" });
    const token = await testToken(env);
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      async (_input, init) => {
        expect(new Headers(init?.headers).get("authorization")).toBe(`Bearer ${accessToken}`);
        return membershipResponse();
      },
    );
    vi.stubGlobal("fetch", fetchMock);

    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toMatchObject({
      authorized: true,
      owner: "alice@example.com",
      org: "example-org",
      login: "alice",
    });
    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toMatchObject({
      authorized: true,
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["membership removal", async (): Promise<Response> => new Response(null, { status: 404 })],
    [
      "GitHub outage",
      async (): Promise<Response> => {
        throw new Error("GitHub unavailable");
      },
    ],
  ])("fails closed after %s", async (_label, result) => {
    const env = testEnv();
    const token = await testToken(env);
    vi.stubGlobal("fetch", vi.fn<() => Promise<Response>>(result));

    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toBeUndefined();
  });

  it("fails closed when a required team membership is removed", async () => {
    const env = testEnv({ CRABBOX_GITHUB_ALLOWED_TEAM: "example-org/operators" });
    const token = await testToken(env);
    vi.stubGlobal(
      "fetch",
      vi.fn<(input: RequestInfo | URL) => Promise<Response>>(async (input) =>
        String(input).includes("/user/teams") ? Response.json([]) : membershipResponse(),
      ),
    );

    await expect(authenticateRequest(tokenRequest(token), env)).resolves.toBeUndefined();
  });

  it.each(["alice", "owner:alice@example.com", "login:alice"])(
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

  it("rejects every coordinator capability before routing a revoked user", async () => {
    const env = testEnv({ CRABBOX_GITHUB_REVOKED_USERS: "alice" });
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

  it.skipIf(!process.env.CRABBOX_GITHUB_LIVE_TOKEN)(
    "live: accepts a current OpenClaw organization member through the encrypted user-token path",
    async () => {
      const liveAccessToken = process.env.CRABBOX_GITHUB_LIVE_TOKEN!;
      const env = testEnv({
        CRABBOX_DEFAULT_ORG: "openclaw",
        CRABBOX_GITHUB_ALLOWED_ORG: "openclaw",
      });
      const token = await issueUserToken(env, {
        owner: "steipete@gmail.com",
        ownerSource: "github-verified-email",
        org: "openclaw",
        login: "steipete",
        githubAccessToken: liveAccessToken,
      });

      await expect(authenticateRequest(tokenRequest(token), env)).resolves.toMatchObject({
        authorized: true,
        owner: "steipete@gmail.com",
        org: "openclaw",
        login: "steipete",
      });
    },
  );
});
