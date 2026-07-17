import { describe, expect, it, vi } from "vitest";

import coordinator, { isAuthorized } from "../src";
import {
  adminGrantVersion,
  authenticateRequest,
  base64URL,
  githubUserGrantIsCurrent,
  issueUserToken,
  requestWithAuthContext,
} from "../src/auth";
import { codeOriginForLease } from "../src/code-origin";
import { prepareCoordinatorRequest } from "../src/coordinator-entry";
import { errorMessage, json, redactDiagnosticSecrets, requestOwner } from "../src/http";
import { MISSING_ORG_KEY, requestOrg, requestOrgLabel } from "../src/org-identity";
import type { Env } from "../src/types";

function proxyIdentityRequest(secret?: string): Request {
  return new Request("https://example.test/v1/whoami", {
    headers: {
      "x-authenticated-user": "alice@example.com",
      ...(secret ? { "x-crabbox-proxy-secret": secret } : {}),
    },
  });
}

const allowGitHubMembership = {
  githubMembership: async (): Promise<void> => {},
};

describe("coordinator auth", () => {
  it("keeps colliding org labels distinct after coordinator authentication", async () => {
    const sharedEnv = {
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SHARED_OWNER: "automation@example.com",
      CRABBOX_DEFAULT_ORG: "science team",
    } as Env;
    const proxyEnv = {
      CRABBOX_TRUSTED_USER_HEADER: "X-Authenticated-User",
      CRABBOX_TRUSTED_USER_ORG: "science_team",
    } as Env;
    const shared = await prepareCoordinatorRequest(
      new Request("https://example.test/v1/whoami", {
        headers: { authorization: "Bearer shared" },
      }),
      sharedEnv,
    );
    const proxy = await prepareCoordinatorRequest(
      new Request("https://example.test/v1/whoami", {
        headers: { "x-authenticated-user": "alice@example.com" },
      }),
      proxyEnv,
      { trustedProxy: true },
    );
    if ("response" in shared || "response" in proxy) {
      throw new Error("expected authenticated coordinator requests");
    }

    expect(requestOrgLabel(shared.request, sharedEnv)).toBe("science team");
    expect(requestOrgLabel(proxy.request, proxyEnv)).toBe("science_team");
    expect(requestOrg(shared.request, sharedEnv)).not.toBe(requestOrg(proxy.request, proxyEnv));
  });

  it("keeps a missing authenticated org distinct from the literal unknown org", async () => {
    const missingEnv = {
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SHARED_OWNER: "automation@example.com",
    } as Env;
    const unknownEnv = { ...missingEnv, CRABBOX_DEFAULT_ORG: "unknown" } as Env;
    const githubEnv = { CRABBOX_SESSION_SECRET: "session-secret" } as Env;
    const githubToken = await issueUserToken(githubEnv, {
      owner: "alice@example.com",
      ownerSource: "github-verified-email",
      org: "unknown",
      login: "alice",
      githubAccessToken: "github-access-token",
    });
    const [missingShared, missingProxy, configuredUnknown, signedUnknown] = await Promise.all([
      prepareCoordinatorRequest(
        new Request("https://example.test/v1/whoami", {
          headers: { authorization: "Bearer shared" },
        }),
        missingEnv,
      ),
      prepareCoordinatorRequest(
        proxyIdentityRequest(),
        { CRABBOX_TRUSTED_USER_HEADER: "X-Authenticated-User" } as Env,
        { trustedProxy: true },
      ),
      prepareCoordinatorRequest(
        new Request("https://example.test/v1/whoami", {
          headers: { authorization: "Bearer shared" },
        }),
        unknownEnv,
      ),
      prepareCoordinatorRequest(
        new Request("https://example.test/v1/whoami", {
          headers: { authorization: `Bearer ${githubToken}` },
        }),
        githubEnv,
        allowGitHubMembership,
      ),
    ]);
    if (
      "response" in missingShared ||
      "response" in missingProxy ||
      "response" in configuredUnknown ||
      "response" in signedUnknown
    ) {
      throw new Error("expected authenticated coordinator requests");
    }

    expect(missingShared.request.headers.get("x-crabbox-org")).toBe("");
    expect(missingProxy.request.headers.get("x-crabbox-org")).toBe("");
    expect(configuredUnknown.request.headers.get("x-crabbox-org")).toBe("unknown");
    expect(signedUnknown.request.headers.get("x-crabbox-org")).toBe("unknown");
    expect(requestOrg(missingShared.request, unknownEnv)).toBe(MISSING_ORG_KEY);
    expect(requestOrg(missingProxy.request, unknownEnv)).toBe(MISSING_ORG_KEY);
    const configuredUnknownKey = requestOrg(configuredUnknown.request, unknownEnv);
    expect(configuredUnknownKey).not.toBe(MISSING_ORG_KEY);
    expect(requestOrg(signedUnknown.request, unknownEnv)).toBe(configuredUnknownKey);
    expect(
      [missingShared, missingProxy, configuredUnknown, signedUnknown].map((prepared) =>
        requestOrgLabel(prepared.request, unknownEnv),
      ),
    ).toEqual(["unknown", "unknown", "unknown", "unknown"]);
  });

  it("rejects invalid authenticated org configuration before coordinator forwarding", async () => {
    const invalidOrg = " science_team";
    const [shared, runtimeAdapter, trustedProxy] = await Promise.all([
      prepareCoordinatorRequest(
        new Request("https://example.test/v1/whoami", {
          headers: { authorization: "Bearer shared" },
        }),
        {
          CRABBOX_SHARED_TOKEN: "shared",
          CRABBOX_DEFAULT_ORG: invalidOrg,
        } as Env,
      ),
      prepareCoordinatorRequest(
        new Request("https://example.test/v1/workspaces", {
          method: "POST",
          headers: { authorization: "Bearer runtime-adapter" },
        }),
        {
          CRABBOX_RUNTIME_ADAPTER_TOKEN: "runtime-adapter",
          CRABBOX_DEFAULT_ORG: invalidOrg,
        } as Env,
      ),
      prepareCoordinatorRequest(
        proxyIdentityRequest(),
        {
          CRABBOX_TRUSTED_USER_HEADER: "X-Authenticated-User",
          CRABBOX_TRUSTED_USER_ORG: invalidOrg,
        } as Env,
        { trustedProxy: true },
      ),
    ]);

    const responses = [shared, runtimeAdapter, trustedProxy].map((prepared) => {
      expect(prepared).toMatchObject({ authenticated: false, response: { status: 503 } });
      if (!("response" in prepared)) throw new Error("invalid org reached the coordinator");
      return prepared.response;
    });
    const expectedError = {
      error: "invalid_org_identity",
      message:
        "organization must be 1-63 printable ASCII characters without leading or trailing spaces",
    };
    await expect(Promise.all(responses.map((response) => response.json()))).resolves.toEqual([
      expectedError,
      expectedError,
      expectedError,
    ]);
  });

  it("requires same-origin intent for portal-cookie mutations and viewer sockets", async () => {
    const env = {
      CRABBOX_SESSION_SECRET: "session-secret",
      CRABBOX_PUBLIC_URL: "https://broker.example.test",
      CRABBOX_DEFAULT_ORG: "example-org",
    } as Env;
    const token = await issueUserToken(env, {
      owner: "alice@example.com",
      ownerSource: "github-verified-email",
      org: "example-org",
      login: "alice",
      githubAccessToken: "github-access-token",
    });
    const cookie = `__Host-crabbox_session=${encodeURIComponent(token)}`;
    const mutationURL = "https://broker.example.test/portal/leases/blue-lobster/share";
    const viewerURL = "https://broker.example.test/portal/leases/blue-lobster/vnc/viewer";

    const denied = await Promise.all(
      [
        new Request(mutationURL, { method: "POST", headers: { cookie } }),
        new Request(mutationURL, {
          method: "POST",
          headers: { cookie, origin: "https://lease.code.example.test" },
        }),
        new Request("https://broker.example.test/portal/logout", {
          method: "POST",
          headers: { cookie },
        }),
        new Request("https://broker.example.test/portal/logout", {
          method: "POST",
          headers: { cookie, origin: "https://attacker.example" },
        }),
        new Request(viewerURL, { headers: { cookie, upgrade: "websocket" } }),
        new Request(viewerURL, {
          headers: { cookie, origin: "https://attacker.example", upgrade: "websocket" },
        }),
      ].map((request) => prepareCoordinatorRequest(request, env, allowGitHubMembership)),
    );
    await Promise.all(
      denied.map(async (prepared) => {
        expect(prepared).toMatchObject({ authenticated: false, response: { status: 403 } });
        if (!("response" in prepared)) throw new Error("cross-origin portal request was accepted");
        await expect(prepared.response.json()).resolves.toEqual({
          error: "portal_request_origin_forbidden",
        });
      }),
    );

    const accepted = await Promise.all(
      [
        new Request(mutationURL, {
          method: "POST",
          headers: { cookie, origin: "https://broker.example.test" },
        }),
        new Request("https://broker.example.test/portal/logout", {
          method: "POST",
          headers: { cookie, origin: "https://broker.example.test" },
        }),
        new Request("https://broker.example.test/portal/logout", {
          headers: { cookie, origin: "https://attacker.example" },
        }),
        new Request(viewerURL, {
          headers: {
            cookie,
            origin: "https://broker.example.test",
            upgrade: "websocket",
          },
        }),
        new Request("https://broker.example.test/portal/leases/blue-lobster", {
          headers: { cookie },
        }),
        new Request(mutationURL, {
          method: "POST",
          headers: {
            authorization: `Bearer ${token}`,
            origin: "https://automation.example.test",
          },
        }),
      ].map((request) => prepareCoordinatorRequest(request, env, allowGitHubMembership)),
    );
    for (const prepared of accepted) {
      expect(prepared).toMatchObject({ authenticated: true });
    }
  });

  it("admits only the WebVNC viewer bootstrap and scoped session routes without portal OAuth", async () => {
    const env = {
      CRABBOX_PUBLIC_URL: "https://broker.example.test",
    } as Env;
    const sessionCookie = "crabbox_webvnc_session=webvnc_session_0123456789abcdef0123456789abcdef";

    const bootstrap = await prepareCoordinatorRequest(
      new Request("https://broker.example.test/portal/leases/cbx_000000000001/vnc/bootstrap", {
        method: "POST",
        headers: {
          "content-type": "application/x-www-form-urlencoded",
          "x-crabbox-admin-grant-version": "f".repeat(64),
          "x-crabbox-owner": "forged@example.test",
        },
        body: "ticket=webvnc_view_0123456789abcdef0123456789abcdef",
      }),
      env,
    );
    expect(bootstrap).toMatchObject({ authenticated: false });
    if ("response" in bootstrap) {
      throw new Error("WebVNC viewer bootstrap did not reach the coordinator");
    }
    expect(bootstrap.request.headers.get("x-crabbox-admin-grant-version")).toBe(
      await adminGrantVersion(env),
    );
    expect(bootstrap.request.headers.has("x-crabbox-owner")).toBe(false);
    expect(bootstrap.request.headers.has("x-crabbox-auth")).toBe(false);

    const page = await prepareCoordinatorRequest(
      new Request("https://broker.example.test/portal/leases/cbx_000000000001/vnc", {
        headers: { cookie: `crabbox_session=existing-github-session; ${sessionCookie}` },
      }),
      env,
    );
    expect(page).toMatchObject({ authenticated: false });
    expect("response" in page).toBe(false);

    const missingOrigin = await prepareCoordinatorRequest(
      new Request("https://broker.example.test/portal/leases/cbx_000000000001/vnc/control", {
        method: "POST",
        headers: { cookie: sessionCookie },
      }),
      env,
    );
    expect(missingOrigin).toMatchObject({ authenticated: false, response: { status: 403 } });

    const sameOrigin = await prepareCoordinatorRequest(
      new Request("https://broker.example.test/portal/leases/cbx_000000000001/vnc/control", {
        method: "POST",
        headers: {
          cookie: sessionCookie,
          origin: "https://broker.example.test",
        },
      }),
      env,
    );
    expect(sameOrigin).toMatchObject({ authenticated: false });
    expect("response" in sameOrigin).toBe(false);

    const outsideViewerScope = await prepareCoordinatorRequest(
      new Request("https://broker.example.test/portal/leases/cbx_000000000001/share", {
        headers: { cookie: sessionCookie },
      }),
      env,
    );
    expect(outsideViewerScope).toMatchObject({ authenticated: false, response: { status: 302 } });
  });

  it("allows a verified same-origin portal logout without GitHub membership", async () => {
    const env = {
      CRABBOX_SESSION_SECRET: "session-secret",
      CRABBOX_PUBLIC_URL: "https://broker.example.test",
      CRABBOX_DEFAULT_ORG: "example-org",
    } as Env;
    const token = await issueUserToken(env, {
      owner: "alice@example.com",
      ownerSource: "github-verified-email",
      org: "example-org",
      login: "alice",
      githubAccessToken: "github-access-token",
    });
    const headers = {
      cookie: `__Host-crabbox_session=${encodeURIComponent(token)}`,
      origin: "https://broker.example.test",
    };
    const membershipUnavailable = {
      githubMembership: async (): Promise<void> => {
        throw new Error("GitHub unavailable");
      },
    };

    const logout = await prepareCoordinatorRequest(
      new Request("https://broker.example.test/portal/logout", { method: "POST", headers }),
      env,
      membershipUnavailable,
    );
    expect(logout).toMatchObject({ authenticated: true });

    const normalMutation = await prepareCoordinatorRequest(
      new Request("https://broker.example.test/portal/leases/blue-lobster/share", {
        method: "POST",
        headers,
      }),
      env,
      membershipUnavailable,
    );
    expect(normalMutation).toMatchObject({ authenticated: false, response: { status: 401 } });
  });

  it("fails closed when a live GitHub grant can no longer be decrypted", async () => {
    const env = {
      CRABBOX_SESSION_SECRET: "session-secret",
      CRABBOX_DEFAULT_ORG: "example-org",
    } as Env;
    const token = await issueUserToken(env, {
      owner: "alice@example.com",
      ownerSource: "github-verified-email",
      org: "example-org",
      login: "alice",
      githubAccessToken: "github-access-token",
    });
    const auth = await authenticateRequest(
      new Request("https://broker.example.test/v1/whoami", {
        headers: { authorization: `Bearer ${token}` },
      }),
      env,
      allowGitHubMembership,
    );
    expect(auth?.githubGrant).toBeDefined();

    await expect(
      githubUserGrantIsCurrent(
        auth!.githubGrant!,
        { owner: auth!.owner, org: auth!.org, login: auth!.login! },
        { CRABBOX_SESSION_SECRET: "rotated-session-secret" },
        allowGitHubMembership,
      ),
    ).resolves.toBe(false);
  });

  it("treats malformed portal cookies as absent across request types", async () => {
    const env = {
      CRABBOX_SESSION_SECRET: "session-secret",
      CRABBOX_PUBLIC_URL: "https://broker.example.test",
    } as Env;
    const cookie = "__Host-crabbox_session=%ZZ";

    const portalGet = await prepareCoordinatorRequest(
      new Request("https://broker.example.test/portal?provider=aws", { headers: { cookie } }),
      env,
    );
    expect(portalGet).toMatchObject({ authenticated: false, response: { status: 302 } });
    if (!("response" in portalGet)) throw new Error("malformed portal cookie authenticated");
    expect(portalGet.response.headers.get("location")).toBe(
      "/portal/login?returnTo=%2Fportal%3Fprovider%3Daws",
    );

    const unauthorized = await Promise.all(
      [
        new Request("https://broker.example.test/portal/leases/blue-lobster/share", {
          method: "POST",
          headers: { cookie, origin: "https://attacker.example" },
        }),
        new Request("https://broker.example.test/v1/pool", { headers: { cookie } }),
      ].map((request) => prepareCoordinatorRequest(request, env)),
    );
    await Promise.all(
      unauthorized.map(async (prepared) => {
        expect(prepared).toMatchObject({ authenticated: false, response: { status: 401 } });
        if (!("response" in prepared)) throw new Error("malformed portal cookie authenticated");
        await expect(prepared.response.json()).resolves.toEqual({ error: "unauthorized" });
      }),
    );
  });

  it("ignores legacy portal cookies and rejects duplicate host-prefixed sessions", async () => {
    const env = {
      CRABBOX_SESSION_SECRET: "session-secret",
      CRABBOX_PUBLIC_URL: "https://broker.example.test",
      CRABBOX_DEFAULT_ORG: "example-org",
    } as Env;
    const token = await issueUserToken(env, {
      owner: "alice@example.com",
      ownerSource: "github-verified-email",
      org: "example-org",
      login: "alice",
      githubAccessToken: "github-access-token",
    });
    const url = "https://broker.example.test/portal/leases/blue-lobster";
    const prepare = async (cookie: string) =>
      await prepareCoordinatorRequest(
        new Request(url, { headers: { cookie } }),
        env,
        allowGitHubMembership,
      );

    const hostOnly = await prepare(
      `crabbox_session=attacker; __Host-crabbox_session=${encodeURIComponent(token)}`,
    );
    expect(hostOnly).toMatchObject({ authenticated: true });

    const rejected = await Promise.all(
      [
        `crabbox_session=${encodeURIComponent(token)}`,
        `__Host-crabbox_session=${encodeURIComponent(token)}; __Host-crabbox_session=${encodeURIComponent(token)}`,
      ].map(prepare),
    );
    for (const prepared of rejected) {
      expect(prepared).toMatchObject({ authenticated: false, response: { status: 302 } });
    }
  });

  it("routes only the exact per-lease Code origin without portal-cookie authority", async () => {
    const env = {
      CRABBOX_CODE_ORIGIN_TEMPLATE: "https://{lease}.code.example.test",
      CRABBOX_PUBLIC_URL: "https://broker.example.test",
    } as Env;
    const leaseID = "cbx_000000000001";
    const origin = await codeOriginForLease(env, leaseID);
    const isolated = await prepareCoordinatorRequest(
      new Request(`${origin}/portal/leases/${leaseID}/code/static/app.js`, {
        headers: { cookie: "__Host-crabbox_session=must-not-authorize-code-origin" },
      }),
      env,
    );

    const isolatedRequest = "request" in isolated ? isolated.request : undefined;
    expect(isolatedRequest).toBeDefined();
    expect(isolated.authenticated).toBe(false);
    expect("bodyLimit" in isolated ? isolated.bodyLimit : undefined).toBe(10 * 1024 * 1024);
    expect(isolatedRequest?.headers.get("authorization")).toBeNull();

    const wrongLease = await prepareCoordinatorRequest(
      new Request(`${origin}/portal/leases/cbx_000000000002/code/`),
      env,
    );
    const wrongLeaseResponse = "response" in wrongLease ? wrongLease.response : undefined;
    expect(wrongLeaseResponse?.status).toBe(302);
    expect(wrongLeaseResponse?.headers.get("location")).toBe(
      "https://broker.example.test/portal/leases/cbx_000000000002/code/",
    );
  });

  it("denies requests when no shared token is configured", async () => {
    const request = new Request("https://example.test/v1/pool");
    await expect(isAuthorized(request, {})).resolves.toBe(false);
  });

  it("requires the configured bearer token", async () => {
    const denied = new Request("https://example.test/v1/pool");
    const wrongSameLength = new Request("https://example.test/v1/pool", {
      headers: { authorization: "Bearer secres" },
    });
    const wrongLength = new Request("https://example.test/v1/pool", {
      headers: { authorization: "Bearer secret-extra" },
    });
    const allowed = new Request("https://example.test/v1/pool", {
      headers: { authorization: "Bearer secret" },
    });
    await expect(isAuthorized(denied, { CRABBOX_SHARED_TOKEN: "secret" })).resolves.toBe(false);
    await expect(isAuthorized(wrongSameLength, { CRABBOX_SHARED_TOKEN: "secret" })).resolves.toBe(
      false,
    );
    await expect(isAuthorized(wrongLength, { CRABBOX_SHARED_TOKEN: "secret" })).resolves.toBe(
      false,
    );
    await expect(isAuthorized(allowed, { CRABBOX_SHARED_TOKEN: "secret" })).resolves.toBe(true);
  });

  it("accepts a reverse-proxy identity only from a trusted proxy source", async () => {
    const request = new Request("https://example.test/v1/whoami", {
      headers: { "x-authenticated-user": "alice@example.com" },
    });

    await expect(
      authenticateRequest(
        request,
        {
          CRABBOX_TRUSTED_USER_HEADER: "X-Authenticated-User",
          CRABBOX_TRUSTED_USER_ORG: "example-org",
        },
        { trustedProxy: true },
      ),
    ).resolves.toEqual({
      authorized: true,
      admin: false,
      auth: "proxy",
      owner: "alice@example.com",
      org: "example-org",
    });
    await expect(
      authenticateRequest(request, {
        CRABBOX_TRUSTED_USER_HEADER: "X-Authenticated-User",
        CRABBOX_TRUSTED_USER_ORG: "example-org",
      }),
    ).resolves.toBeUndefined();
    await expect(authenticateRequest(request, {})).resolves.toBeUndefined();
  });

  it("requires the configured reverse-proxy secret before trusting identity", async () => {
    const env = {
      CRABBOX_TRUSTED_USER_HEADER: "X-Authenticated-User",
      CRABBOX_TRUSTED_USER_ORG: "example-org",
      CRABBOX_TRUSTED_PROXY_SECRET: "proxy-secret",
    };

    await expect(
      authenticateRequest(proxyIdentityRequest("proxy-secret"), env, { trustedProxy: true }),
    ).resolves.toMatchObject({ auth: "proxy", owner: "alice@example.com" });
    await expect(
      authenticateRequest(proxyIdentityRequest("wrong-secret"), env, { trustedProxy: true }),
    ).resolves.toBeUndefined();
    await expect(
      authenticateRequest(proxyIdentityRequest(), env, { trustedProxy: true }),
    ).resolves.toBeUndefined();
    await expect(
      authenticateRequest(proxyIdentityRequest("proxy-secret"), env),
    ).resolves.toBeUndefined();
    await expect(
      authenticateRequest(
        proxyIdentityRequest(),
        { ...env, CRABBOX_TRUSTED_PROXY_SECRET: "" },
        { trustedProxy: true },
      ),
    ).resolves.toBeUndefined();
    await expect(
      authenticateRequest(
        new Request("https://example.test/v1/whoami", {
          headers: { "x-crabbox-proxy-secret": "proxy-secret" },
        }),
        { ...env, CRABBOX_TRUSTED_USER_HEADER: "X-Crabbox-Proxy-Secret" },
        { trustedProxy: true },
      ),
    ).resolves.toBeUndefined();
  });

  it("strips trusted headers from unauthenticated pass-through routes", async () => {
    const requests = [
      new Request("https://example.test/v1/auth/github/callback", {
        headers: { "x-crabbox-proxy-secret": "proxy-secret" },
      }),
      new Request("https://example.test/portal/login", {
        headers: { "x-crabbox-proxy-secret": "proxy-secret" },
      }),
      new Request("https://example.test/v1/leases/lease-1/webvnc/agent", {
        headers: { upgrade: "websocket", "x-crabbox-proxy-secret": "proxy-secret" },
      }),
      new Request("https://example.test/v1/adapters/example-adapter/agent", {
        headers: {
          upgrade: "websocket",
          authorization: "Bearer adapter_ticket",
          "x-crabbox-proxy-secret": "proxy-secret",
        },
      }),
    ];
    requests.forEach((request) => {
      request.headers.set("x-crabbox-internal", "scheduled");
      request.headers.set("x-crabbox-admin-grant-version", "a".repeat(64));
      request.headers.set("x-crabbox-github-token-id", "forged-token-id");
      request.headers.set("x-crabbox-github-sealed-credential", "forged-credential");
    });

    const routedRequests = await Promise.all(
      requests.map(async (request) => {
        const prepared = await prepareCoordinatorRequest(request, {} as Env);
        if ("response" in prepared) {
          throw new Error("expected pass-through request");
        }
        return prepared.request;
      }),
    );

    expect(routedRequests.map((request) => request.headers.get("x-crabbox-proxy-secret"))).toEqual([
      null,
      null,
      null,
      null,
    ]);
    expect(routedRequests.map((request) => request.headers.get("x-crabbox-internal"))).toEqual([
      null,
      null,
      null,
      null,
    ]);
    expect(
      routedRequests.map((request) => request.headers.get("x-crabbox-admin-grant-version")),
    ).toEqual([null, null, null, null]);
    expect(
      routedRequests.map((request) => request.headers.get("x-crabbox-github-token-id")),
    ).toEqual([null, null, null, null]);
    expect(
      routedRequests.map((request) => request.headers.get("x-crabbox-github-sealed-credential")),
    ).toEqual([null, null, null, null]);
  });

  it("replaces caller-supplied admin grant versions after authentication", async () => {
    const env = {
      CRABBOX_ADMIN_TOKEN: "admin-secret",
      CRABBOX_DEFAULT_ORG: "example-org",
    } as Env;
    const forgedVersion = "a".repeat(64);
    const prepared = await prepareCoordinatorRequest(
      new Request("https://example.test/v1/admin/leases", {
        headers: {
          authorization: "Bearer admin-secret",
          "x-crabbox-admin-grant-version": forgedVersion,
        },
      }),
      env,
    );

    expect(prepared).toMatchObject({ authenticated: true });
    if ("response" in prepared) throw new Error("admin request was rejected");
    expect(prepared.request.headers.get("x-crabbox-admin-grant-version")).toBe(
      await adminGrantVersion(env),
    );
    expect(prepared.request.headers.get("x-crabbox-admin-grant-version")).not.toBe(forgedVersion);
  });

  it("requires normal coordinator authentication for workspace terminals", async () => {
    const unauthorized = await prepareCoordinatorRequest(
      new Request("https://example.test/v1/workspaces/fleet-is-101/terminal", {
        headers: { upgrade: "websocket" },
      }),
      {},
    );
    const authorized = await prepareCoordinatorRequest(
      new Request("https://example.test/v1/workspaces/fleet-is-101/terminal", {
        headers: { upgrade: "websocket", authorization: "Bearer shared" },
      }),
      {
        CRABBOX_SHARED_TOKEN: "shared",
        CRABBOX_SHARED_OWNER: "alice@example.com",
        CRABBOX_DEFAULT_ORG: "example-org",
      },
    );
    const authorizedRequest = "request" in authorized ? authorized.request : undefined;

    expect(unauthorized).toMatchObject({
      authenticated: false,
      response: { status: 401 },
    });
    expect(authorized).toMatchObject({ authenticated: true });
    expect(authorizedRequest?.headers.get("x-crabbox-owner")).toBe("alice@example.com");
    expect(authorizedRequest?.headers.get("x-crabbox-org")).toBe("example-org");
  });

  it("rejects the runtime adapter token from workspace terminals", async () => {
    const prepared = await prepareCoordinatorRequest(
      new Request("https://example.test/v1/workspaces/fleet-is-101/terminal", {
        headers: {
          upgrade: "websocket",
          authorization: "Bearer runtime-adapter",
        },
      }),
      { CRABBOX_RUNTIME_ADAPTER_TOKEN: "runtime-adapter" },
    );

    expect(prepared).toMatchObject({ authenticated: false, response: { status: 401 } });
  });

  it("lets one-time native VNC websocket tickets bypass user authentication", async () => {
    const prepared = await prepareCoordinatorRequest(
      new Request("https://example.test/v1/native-vnc/handoff", {
        headers: { upgrade: "websocket", authorization: "Bearer native_vnc_ticket" },
      }),
      {} as Env,
    );
    expect(prepared).toMatchObject({ authenticated: false });
    if ("response" in prepared) throw new Error("native VNC websocket was rejected");
    expect(prepared.request.headers.get("authorization")).toBe("Bearer native_vnc_ticket");

    const plain = await prepareCoordinatorRequest(
      new Request("https://example.test/v1/native-vnc/handoff", {
        headers: { authorization: "Bearer native_vnc_ticket" },
      }),
      {} as Env,
    );
    expect(plain).toMatchObject({ authenticated: false, response: { status: 401 } });
  });

  it("accepts the runtime adapter token only for workspace routes", async () => {
    const env = {
      CRABBOX_DEFAULT_ORG: "openclaw",
      CRABBOX_RUNTIME_ADAPTER_TOKEN: "runtime-adapter",
    } as Env;
    const accepted = await Promise.all(
      [
        new Request("https://example.test/v1/workspaces", { method: "POST" }),
        new Request("https://example.test/v1/workspaces/fleet-is-101"),
        new Request("https://example.test/v1/workspaces/fleet-is-101/connections/desktop", {
          method: "POST",
        }),
        new Request("https://example.test/v1/workspaces/fleet-is-101/connections/native-vnc", {
          method: "POST",
        }),
      ].map((request) => {
        request.headers.set("authorization", "Bearer runtime-adapter");
        return prepareCoordinatorRequest(request, env);
      }),
    );
    expect(
      accepted.map((prepared) =>
        "response" in prepared
          ? null
          : {
              authenticated: prepared.authenticated,
              owner: prepared.request.headers.get("x-crabbox-owner"),
              org: prepared.request.headers.get("x-crabbox-org"),
            },
      ),
    ).toEqual([
      { authenticated: true, owner: "service@openclaw.org", org: "openclaw" },
      { authenticated: true, owner: "service@openclaw.org", org: "openclaw" },
      { authenticated: true, owner: "service@openclaw.org", org: "openclaw" },
      { authenticated: true, owner: "service@openclaw.org", org: "openclaw" },
    ]);

    const denied = await Promise.all(
      [
        new Request("https://example.test/v1/workspaces", {
          headers: { authorization: "Bearer runtime-adapter" },
        }),
        new Request("https://example.test/v1/workspaces/fleet-is-101/terminal", {
          headers: {
            upgrade: "websocket",
            authorization: "Bearer runtime-adapter",
          },
        }),
        new Request("https://example.test/v1/leases", {
          headers: { authorization: "Bearer runtime-adapter" },
        }),
        new Request("https://example.test/v1/workspaces", {
          method: "POST",
          headers: { authorization: "Bearer wrong" },
        }),
      ].map((request) => prepareCoordinatorRequest(request, env)),
    );
    expect(
      denied.map((prepared) =>
        "response" in prepared
          ? { authenticated: prepared.authenticated, status: prepared.response.status }
          : null,
      ),
    ).toEqual([
      { authenticated: false, status: 401 },
      { authenticated: false, status: 401 },
      { authenticated: false, status: 401 },
      { authenticated: false, status: 401 },
    ]);
  });

  it("requires the dedicated runtime token for private AWS workspace routes", async () => {
    const env = {
      CRABBOX_WORKSPACE_AWS_PRIVATE: "1",
      CRABBOX_RUNTIME_ADAPTER_TOKEN: "dedicated",
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_ADMIN_TOKEN: "admin",
      CRABBOX_DEFAULT_ORG: "example-org",
    } as Env;
    const rejected = await Promise.all(
      [undefined, "Bearer wrong", "Bearer shared", "Bearer admin"].map(async (authorization) => {
        const headers = new Headers();
        if (authorization) headers.set("authorization", authorization);
        return await prepareCoordinatorRequest(
          new Request("https://example.test/v1/workspaces/private-gateway", { headers }),
          env,
        );
      }),
    );
    expect(
      rejected.map((prepared) => ("response" in prepared ? prepared.response.status : 0)),
    ).toEqual([401, 401, 401, 401]);

    const accepted = await prepareCoordinatorRequest(
      new Request("https://example.test/v1/workspaces/private-gateway", {
        headers: { authorization: "Bearer dedicated" },
      }),
      env,
    );
    expect(accepted).toMatchObject({ authenticated: true });
  });

  it("keeps shared bearer token non-admin and ignores caller-supplied identity headers", async () => {
    const env = {
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SHARED_OWNER: "automation@example.com",
      CRABBOX_ADMIN_TOKEN: "admin",
      CRABBOX_DEFAULT_ORG: "openclaw",
    };
    const shared = await authenticateRequest(
      new Request("https://example.test/v1/pool", {
        headers: {
          authorization: "Bearer shared",
          "x-crabbox-owner": "operator@example.com",
          "x-crabbox-org": "attacker-org",
          "cf-access-authenticated-user-email": "spoof@example.com",
        },
      }),
      env,
    );
    const admin = await authenticateRequest(
      new Request("https://example.test/v1/pool", {
        headers: { authorization: "Bearer admin", "x-crabbox-owner": "operator@example.com" },
      }),
      env,
    );

    expect(shared).toMatchObject({
      authorized: true,
      admin: false,
      owner: "automation@example.com",
      org: "openclaw",
    });
    expect(admin).toMatchObject({
      authorized: true,
      admin: true,
      owner: "operator@example.com",
    });
  });

  it("uses Cloudflare Access identity only after verifying the Access JWT", async () => {
    const { jwt, publicJwk } = await accessJwt({
      kid: "access-test-kid",
      aud: "access-aud",
      iss: "https://team.example.cloudflareaccess.com",
      email: "verified@example.com",
    });
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ keys: [publicJwk] }), {
        headers: { "content-type": "application/json" },
      }),
    );
    try {
      const auth = await authenticateRequest(
        new Request("https://example.test/v1/whoami", {
          headers: {
            authorization: "Bearer shared",
            "cf-access-authenticated-user-email": "spoof@example.com",
            "cf-access-jwt-assertion": jwt,
            "x-crabbox-owner": "operator@example.com",
          },
        }),
        {
          CRABBOX_SHARED_TOKEN: "shared",
          CRABBOX_DEFAULT_ORG: "openclaw",
          CRABBOX_ACCESS_TEAM_DOMAIN: "team.example.cloudflareaccess.com",
          CRABBOX_ACCESS_AUD: "access-aud",
        },
      );

      expect(auth).toMatchObject({
        authorized: true,
        admin: false,
        owner: "verified@example.com",
      });
      expect(fetchMock).toHaveBeenCalledWith(
        "https://team.example.cloudflareaccess.com/cdn-cgi/access/certs",
      );
    } finally {
      fetchMock.mockRestore();
    }
  });

  it("does not fetch Cloudflare Access keys before Crabbox bearer authentication", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ keys: [] }), {
        headers: { "content-type": "application/json" },
      }),
    );
    try {
      const env = {
        CRABBOX_SHARED_TOKEN: "shared",
        CRABBOX_ACCESS_TEAM_DOMAIN: "preauth.example.cloudflareaccess.com",
        CRABBOX_ACCESS_AUD: "access-aud",
      };
      await Promise.all(
        [undefined, "Bearer wrong"].map(async (authorization) => {
          const headers = new Headers({
            "cf-access-jwt-assertion": accessJwtShape("unknown-kid"),
          });
          if (authorization) {
            headers.set("authorization", authorization);
          }
          await expect(
            authenticateRequest(new Request("https://example.test/v1/leases", { headers }), env),
          ).resolves.toBeUndefined();
        }),
      );
      expect(fetchMock).not.toHaveBeenCalled();
    } finally {
      fetchMock.mockRestore();
    }
  });

  it("bounds Cloudflare Access key fetches for unknown kids", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ keys: [] }), {
        headers: { "content-type": "application/json" },
      }),
    );
    try {
      const env = {
        CRABBOX_SHARED_TOKEN: "shared",
        CRABBOX_SHARED_OWNER: "automation@example.com",
        CRABBOX_ACCESS_TEAM_DOMAIN: "unknown-kid.example.cloudflareaccess.com",
        CRABBOX_ACCESS_AUD: "access-aud",
      };
      const authenticateKid = (kid: string) =>
        authenticateRequest(
          new Request("https://example.test/v1/leases", {
            headers: {
              authorization: "Bearer shared",
              "cf-access-jwt-assertion": accessJwtShape(kid),
            },
          }),
          env,
        );
      const [first, repeated, different] = await Promise.all([
        authenticateKid("missing-one"),
        authenticateKid("missing-one"),
        authenticateKid("missing-two"),
      ]);
      expect([first?.owner, repeated?.owner, different?.owner]).toEqual([
        "automation@example.com",
        "automation@example.com",
        "automation@example.com",
      ]);
      expect(fetchMock).toHaveBeenCalledTimes(1);
    } finally {
      fetchMock.mockRestore();
    }
  });

  it("refreshes a cached Cloudflare Access key set once for key rotation", async () => {
    const domain = "rotation.example.cloudflareaccess.com";
    const firstKey = await accessJwt({
      kid: "rotation-one",
      aud: "access-aud",
      iss: `https://${domain}`,
      email: "first@example.com",
    });
    const rotatedKey = await accessJwt({
      kid: "rotation-two",
      aud: "access-aud",
      iss: `https://${domain}`,
      email: "second@example.com",
    });
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ keys: [firstKey.publicJwk] })))
      .mockResolvedValueOnce(new Response(JSON.stringify({ keys: [rotatedKey.publicJwk] })));
    try {
      const env = {
        CRABBOX_SHARED_TOKEN: "shared",
        CRABBOX_ACCESS_TEAM_DOMAIN: domain,
        CRABBOX_ACCESS_AUD: "access-aud",
      };
      const authenticate = (jwt: string) =>
        authenticateRequest(
          new Request("https://example.test/v1/leases", {
            headers: {
              authorization: "Bearer shared",
              "cf-access-jwt-assertion": jwt,
            },
          }),
          env,
        );
      await expect(authenticate(firstKey.jwt)).resolves.toMatchObject({
        owner: "first@example.com",
      });
      const rotated = await Promise.all([
        authenticate(rotatedKey.jwt),
        authenticate(rotatedKey.jwt),
        authenticate(rotatedKey.jwt),
      ]);
      expect(rotated.map((auth) => auth?.owner)).toEqual([
        "second@example.com",
        "second@example.com",
        "second@example.com",
      ]);
      expect(fetchMock).toHaveBeenCalledTimes(2);
    } finally {
      fetchMock.mockRestore();
    }
  });

  it("rejects oversized Cloudflare Access key ids without fetching", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ keys: [] }), {
        headers: { "content-type": "application/json" },
      }),
    );
    try {
      const auth = await authenticateRequest(
        new Request("https://example.test/v1/leases", {
          headers: {
            authorization: "Bearer shared",
            "cf-access-jwt-assertion": accessJwtShape("x".repeat(257)),
          },
        }),
        {
          CRABBOX_SHARED_TOKEN: "shared",
          CRABBOX_SHARED_OWNER: "automation@example.com",
          CRABBOX_ACCESS_TEAM_DOMAIN: "oversized.example.cloudflareaccess.com",
          CRABBOX_ACCESS_AUD: "access-aud",
        },
      );
      expect(auth?.owner).toBe("automation@example.com");
      expect(fetchMock).not.toHaveBeenCalled();
    } finally {
      fetchMock.mockRestore();
    }
  });

  it("normalizes Cloudflare Access team domains before fetching certs", async () => {
    const { jwt, publicJwk } = await accessJwt({
      kid: "access-test-kid-url",
      aud: "access-aud",
      iss: "https://team-url.example.cloudflareaccess.com",
      email: "verified@example.com",
    });
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ keys: [publicJwk] }), {
        headers: { "content-type": "application/json" },
      }),
    );
    try {
      const auth = await authenticateRequest(
        new Request("https://example.test/v1/whoami", {
          headers: {
            authorization: "Bearer shared",
            "cf-access-authenticated-user-email": "spoof@example.com",
            "cf-access-jwt-assertion": jwt,
          },
        }),
        {
          CRABBOX_SHARED_TOKEN: "shared",
          CRABBOX_DEFAULT_ORG: "example-org",
          CRABBOX_ACCESS_TEAM_DOMAIN: "https://team-url.example.cloudflareaccess.com/path",
          CRABBOX_ACCESS_AUD: "access-aud",
        },
      );

      expect(auth.owner).toBe("verified@example.com");
      expect(fetchMock).toHaveBeenCalledWith(
        "https://team-url.example.cloudflareaccess.com/cdn-cgi/access/certs",
      );
    } finally {
      fetchMock.mockRestore();
    }
  });

  it("accepts signed GitHub user tokens without admin rights", async () => {
    const env = {
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SESSION_SECRET: "session-secret",
      CRABBOX_DEFAULT_ORG: "openclaw",
    };
    const token = await issueUserToken(env, {
      owner: "friend@example.com",
      ownerSource: "github-verified-email",
      org: "openclaw",
      login: "friend",
      githubAccessToken: "github-access-token",
    });
    const request = new Request("https://example.test/v1/whoami", {
      headers: { authorization: `Bearer ${token}`, "x-crabbox-owner": "spoof@example.com" },
    });
    const auth = await authenticateRequest(request, env, allowGitHubMembership);
    expect(auth).toMatchObject({
      authorized: true,
      admin: false,
      auth: "github",
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
    });
    expect(auth?.tokenExpiresAt).toMatch(/^\d{4}-\d{2}-\d{2}T/);
  });

  it("rejects non-canonical signed user token spellings", async () => {
    const env = {
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SESSION_SECRET: "session-secret",
      CRABBOX_DEFAULT_ORG: "example-org",
    };
    const token = await issueUserToken(env, {
      owner: "friend@example.com",
      ownerSource: "github-verified-email",
      org: "example-org",
      login: "friend",
      githubAccessToken: "github-access-token",
    });

    const authResults = await Promise.all(
      [`${token}.ignored`, `${token}.`, `${token}..ignored`].map((malformed) =>
        authenticateRequest(
          new Request("https://example.test/v1/whoami", {
            headers: { authorization: `Bearer ${malformed}` },
          }),
          env,
        ),
      ),
    );
    expect(authResults).toEqual([undefined, undefined, undefined]);
  });

  it("issues a distinct identity for each GitHub user session", async () => {
    const env = { CRABBOX_SESSION_SECRET: "session-secret" };
    const input = {
      owner: "friend@example.com",
      ownerSource: "github-verified-email" as const,
      org: "openclaw",
      login: "friend",
      githubAccessToken: "github-access-token",
    };

    const first = await issueUserToken(env, input);
    const second = await issueUserToken(env, input);

    expect(second).not.toBe(first);
  });

  it.each([
    ["legacy schema", {}],
    ["unverified owner provenance", { version: 2, ownerSource: "github-public-email" }],
  ])("rejects signed user tokens with %s", async (_label, schema) => {
    const env = {
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SESSION_SECRET: "session-secret",
      CRABBOX_DEFAULT_ORG: "openclaw",
    };
    const now = Math.floor(Date.now() / 1000);
    const token = await signedUserToken("session-secret", {
      typ: "crabbox-user",
      ...schema,
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
      iat: now,
      exp: now + 300,
    });

    const auth = await authenticateRequest(
      new Request("https://example.test/v1/whoami", {
        headers: { authorization: `Bearer ${token}` },
      }),
      env,
      allowGitHubMembership,
    );

    expect(auth).toBeUndefined();
  });

  it("promotes configured GitHub user tokens to admin", async () => {
    const env = {
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SESSION_SECRET: "session-secret",
      CRABBOX_DEFAULT_ORG: "openclaw",
      CRABBOX_GITHUB_ADMIN_OWNERS: "vincentkoc@ieee.org",
      CRABBOX_GITHUB_ADMIN_LOGINS: "steipete",
    };
    const ownerToken = await issueUserToken(env, {
      owner: "vincentkoc@ieee.org",
      ownerSource: "github-verified-email",
      org: "openclaw",
      login: "vincentkoc",
      githubAccessToken: "github-access-token",
    });
    const loginToken = await issueUserToken(env, {
      owner: "peter@example.com",
      ownerSource: "github-verified-email",
      org: "openclaw",
      login: "steipete",
      githubAccessToken: "github-access-token",
    });

    const ownerAuth = await authenticateRequest(
      new Request("https://example.test/v1/whoami", {
        headers: { authorization: `Bearer ${ownerToken}` },
      }),
      env,
      allowGitHubMembership,
    );
    const loginAuth = await authenticateRequest(
      new Request("https://example.test/v1/whoami", {
        headers: { authorization: `Bearer ${loginToken}` },
      }),
      env,
      allowGitHubMembership,
    );

    expect(ownerAuth).toMatchObject({ admin: true, owner: "vincentkoc@ieee.org" });
    expect(loginAuth).toMatchObject({ admin: true, login: "steipete" });
    expect(
      requestWithAuthContext(
        new Request("https://example.test/v1/admin/leases"),
        ownerAuth!,
      ).headers.get("x-crabbox-admin"),
    ).toBe("true");
  });

  it("rejects signed user tokens with admin claims", async () => {
    const env = {
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SESSION_SECRET: "session-secret",
      CRABBOX_DEFAULT_ORG: "openclaw",
    };
    const now = Math.floor(Date.now() / 1000);
    const token = await signedUserToken("session-secret", {
      typ: "crabbox-user",
      version: 2,
      ownerSource: "github-verified-email",
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
      admin: true,
      iat: now,
      exp: now + 300,
    });

    const auth = await authenticateRequest(
      new Request("https://example.test/v1/admin/leases", {
        headers: { authorization: `Bearer ${token}` },
      }),
      env,
    );

    expect(auth).toBeUndefined();
  });

  it("does not route admin-claim user tokens to the coordinator", async () => {
    const now = Math.floor(Date.now() / 1000);
    const token = await signedUserToken("session-secret", {
      typ: "crabbox-user",
      version: 2,
      ownerSource: "github-verified-email",
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
      admin: true,
      iat: now,
      exp: now + 300,
    });
    const env = {
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_SESSION_SECRET: "session-secret",
      CRABBOX_DEFAULT_ORG: "openclaw",
      FLEET: {
        idFromName: () => "default",
        get: () => {
          throw new Error("admin-claim user token reached coordinator");
        },
      },
    } as unknown as Env;

    const response = await coordinator.fetch(
      new Request("https://example.test/v1/admin/leases", {
        headers: { authorization: `Bearer ${token}` },
      }),
      env,
    );

    expect(response.status).toBe(401);
  });

  it("rejects user tokens signed with the shared automation token", async () => {
    const now = Math.floor(Date.now() / 1000);
    const token = await signedUserToken("shared", {
      typ: "crabbox-user",
      version: 2,
      ownerSource: "github-verified-email",
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
      iat: now,
      exp: now + 300,
    });
    const env = {
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_DEFAULT_ORG: "openclaw",
    };

    const auth = await authenticateRequest(
      new Request("https://example.test/v1/whoami", {
        headers: { authorization: `Bearer ${token}` },
      }),
      env,
    );

    expect(auth).toBeUndefined();
  });

  it("requires independent signing material when issuing user tokens", async () => {
    const input = {
      owner: "friend@example.com",
      ownerSource: "github-verified-email" as const,
      org: "openclaw",
      login: "friend",
      githubAccessToken: "github-access-token",
    };

    await expect(issueUserToken({ CRABBOX_SHARED_TOKEN: "shared" }, input)).rejects.toThrow(
      "CRABBOX_SESSION_SECRET is required",
    );
    await expect(
      issueUserToken({ CRABBOX_SHARED_TOKEN: "shared", CRABBOX_SESSION_SECRET: "shared" }, input),
    ).rejects.toThrow("CRABBOX_SESSION_SECRET must differ");
  });

  it.each([
    "/v1/internal/scheduled",
    "/v1//internal/scheduled",
    "/v1///internal/scheduled",
    "//v1/internal/scheduled",
  ])("does not expose internal scheduled maintenance through public fetch: %s", async (path) => {
    const env = {
      CRABBOX_SHARED_TOKEN: "shared",
      CRABBOX_DEFAULT_ORG: "example-org",
      FLEET: {
        idFromName: () => "default",
        get: () => {
          throw new Error("internal maintenance reached coordinator");
        },
      },
    } as unknown as Env;

    const response = await coordinator.fetch(
      new Request(`https://example.test${path}`, {
        method: "POST",
        headers: {
          authorization: "Bearer shared",
          "x-crabbox-internal": "scheduled",
        },
      }),
      env,
    );

    expect(response.status).toBe(404);
  });

  it("does not let caller-supplied Access identity override signed user token identity", async () => {
    const request = new Request("https://example.test/v1/whoami", {
      method: "POST",
      body: "request-body",
      headers: {
        "cf-access-authenticated-user-email": "spoof@example.com",
        "x-crabbox-proxy-secret": "proxy-secret",
        "x-crabbox-internal": "scheduled",
        "x-crabbox-owner": "spoof@example.com",
      },
    });
    const next = requestWithAuthContext(request, {
      authorized: true,
      admin: false,
      auth: "github",
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
    });

    expect(next.headers.get("cf-access-authenticated-user-email")).toBeNull();
    expect(next.headers.get("cf-access-jwt-assertion")).toBeNull();
    expect(next.headers.get("x-crabbox-proxy-secret")).toBeNull();
    expect(next.headers.get("x-crabbox-internal")).toBeNull();
    expect(requestOwner(next)).toBe("friend@example.com");
    await expect(next.text()).resolves.toBe("request-body");
  });

  it("redirects browser portal auth routes to the configured public origin", async () => {
    let fleetCalled = false;
    const env = {
      CRABBOX_PUBLIC_URL: "https://broker.example.com",
      FLEET: {
        idFromName: () => "default",
        get: () => {
          fleetCalled = true;
          return { fetch: () => new Response("unexpected", { status: 599 }) };
        },
      },
    } as unknown as Env;

    const login = await coordinator.fetch(
      new Request(
        "https://crabbox-coordinator.steipete.workers.dev/portal/login?returnTo=%2Fportal%2Fleases%2Fcbx_1%2Fvnc",
      ),
      env,
    );
    expect(login.status).toBe(302);
    expect(login.headers.get("location")).toBe(
      "https://broker.example.com/portal/login?returnTo=%2Fportal%2Fleases%2Fcbx_1%2Fvnc",
    );

    const logout = await coordinator.fetch(
      new Request("https://crabbox-coordinator.steipete.workers.dev/portal/logout"),
      env,
    );
    expect(logout.status).toBe(302);
    expect(logout.headers.get("location")).toBe("https://broker.example.com/portal/logout");

    const bootstrap = await coordinator.fetch(
      new Request(
        "https://crabbox-coordinator.steipete.workers.dev/portal/leases/cbx_1/vnc/bootstrap",
        {
          method: "POST",
          headers: { "content-type": "application/x-www-form-urlencoded" },
          body: "ticket=webvnc_view_0123456789abcdef0123456789abcdef",
        },
      ),
      env,
    );
    expect(bootstrap.status).toBe(200);
    expect(bootstrap.headers.get("location")).toBeNull();
    expect(bootstrap.headers.get("content-type")).toBe("text/html; charset=utf-8");
    expect(bootstrap.headers.get("referrer-policy")).toBe("no-referrer");
    expect(bootstrap.headers.get("content-security-policy")).toContain(
      "form-action https://broker.example.com",
    );
    expect(bootstrap.headers.get("cache-control")).toBe("no-store");
    const bootstrapBody = await bootstrap.text();
    expect(bootstrapBody).toContain(
      'action="https://broker.example.com/portal/leases/cbx_1/vnc/bootstrap"',
    );
    expect(bootstrapBody).toContain('value="webvnc_view_0123456789abcdef0123456789abcdef"');
    expect(bootstrapBody).toContain('document.getElementById("webvnc-bootstrap").requestSubmit()');
    expect(fleetCalled).toBe(false);
  });
});

describe("http responses", () => {
  it("does not serialize stack traces from response payloads", async () => {
    const response = json({
      error: new Error("boom\n    at hidden"),
      nested: { stack: "hidden stack" },
    });

    expect(await response.json()).toEqual({
      error: { name: "Error", message: "boom" },
      nested: {},
    });
  });

  it("handles circular arrays while redacting response payloads", async () => {
    const circular: unknown[] = [];
    circular.push(circular);

    const response = json({ circular });

    expect(await response.json()).toEqual({ circular: ["[Circular]"] });
  });

  it("handles self-returning toJSON methods while redacting response payloads", async () => {
    const value = { ok: true, stack: "hidden", toJSON: () => value };

    const response = json({ value });

    expect(await response.json()).toEqual({ value: { ok: true } });
  });

  it("keeps public error messages to the first line", () => {
    expect(errorMessage(new Error("boom\n    at hidden"))).toBe("boom");
  });

  it("redacts configured and structured credentials from diagnostics", () => {
    const secrets = ["configured-provider-secret", "longer-configured-provider-secret", undefined];
    const diagnostic = [
      "provider failed",
      "longer-configured-provider-secret",
      "configured-provider-secret",
      "Authorization: Bearer bearer-secret",
      "Authorization: Bearer: colon-bearer-secret",
      "Authorization: Bearer\n folded-bearer-secret",
      "Authorization: Bearer:\r\n folded-colon-bearer-secret",
      "Authorization: Bearer :\r\n spaced-folded-colon-bearer-secret",
      "standalone Bearer   spaced-standalone-secret",
      "standalone Bearer: colon-standalone-secret",
      "standalone Bearer\n folded-standalone-secret",
      "standalone Bearer:\r\n folded-colon-standalone-secret",
      "standalone Bearer :\r\n spaced-folded-colon-standalone-secret",
      "standalone Bearer [redacted]",
      "Proxy-Authorization=Basic basic-secret",
      "refresh_token=refresh-assignment-value",
      "api-secret=api-assignment-value",
      "refreshToken=refresh-camel-assignment-value",
      "idToken=id-camel-assignment-value",
      "secretAccessKey=aws-camel-assignment-value",
      "apiSecret=api-camel-assignment-value",
      "Authorization: Token token-scheme-secret",
      "Proxy-Authorization: Digest digest-scheme-secret",
      'Authorization: Digest username="digest-user", response="digest-response"',
      "Authorization: 1custom digit-scheme-secret",
      "X-API-Key=header-secret",
      "client_secret=plain-secret",
      '{"token":"json-secret\\\"escaped-tail-leak","clientSecret":"client-secret","x-api-key":"json-api-secret","accessToken":"access-json-value","refresh_token":"refresh-snake-value\\\"refresh-escaped-tail","refreshToken":"refresh-camel-value","refresh-token":"refresh-kebab-value","id_token":"id-snake-value","idToken":"id-camel-value","id-token":"id-kebab-value","secretAccessKey":"aws-camel-value","secret_access_key":"aws-snake-value","secret-access-key":"aws-kebab-value","apiSecret":"api-camel-value","api_secret":"api-snake-value","api-secret":"api-kebab-value","nextToken":"page-2","token_type":"Bearer"}',
      "https://alice:password@example.test/path?api_key=query-secret&refresh_token=query-refresh-token&id_token=query-id-token&secret_access_key=query-aws-secret&api_secret=query-api-secret&x-api-key=query-api-key&authorization=query-authorization&proxy-authorization=query-proxy-authorization&session_token=query-session-token&X-Amz-Signature=signed-secret&X-Goog-Credential=gcp-credential&X-Goog-Signature=gcp-signature&X-Goog-Security-Token=gcp-security-token&region=eu",
      "https://single-userinfo-token@other.example.test/path",
      "https://first-userinfo-value@second-userinfo-value@multi.example.test/path",
      ["-----BEGIN", "PRIVATE KEY-----\nprivate-key-body\n-----END PRIVATE KEY-----"].join(" "),
      "safe suffix",
    ].join("\n");

    const redacted = redactDiagnosticSecrets(diagnostic, secrets);

    for (const secret of [
      "configured-provider-secret",
      "longer-configured-provider-secret",
      "bearer-secret",
      "colon-bearer-secret",
      "folded-bearer-secret",
      "folded-colon-bearer-secret",
      "spaced-folded-colon-bearer-secret",
      "spaced-standalone-secret",
      "colon-standalone-secret",
      "folded-standalone-secret",
      "folded-colon-standalone-secret",
      "spaced-folded-colon-standalone-secret",
      "basic-secret",
      "refresh-assignment-value",
      "api-assignment-value",
      "refresh-camel-assignment-value",
      "id-camel-assignment-value",
      "aws-camel-assignment-value",
      "api-camel-assignment-value",
      "token-scheme-secret",
      "digest-scheme-secret",
      "digest-user",
      "digest-response",
      "digit-scheme-secret",
      "header-secret",
      "plain-secret",
      "json-secret",
      "escaped-tail-leak",
      "client-secret",
      "json-api-secret",
      "access-json-value",
      "refresh-snake-value",
      "refresh-escaped-tail",
      "refresh-camel-value",
      "refresh-kebab-value",
      "id-snake-value",
      "id-camel-value",
      "id-kebab-value",
      "aws-camel-value",
      "aws-snake-value",
      "aws-kebab-value",
      "api-camel-value",
      "api-snake-value",
      "api-kebab-value",
      "alice",
      "password",
      "single-userinfo-token",
      "first-userinfo-value",
      "second-userinfo-value",
      "query-secret",
      "query-refresh-token",
      "query-id-token",
      "query-aws-secret",
      "query-api-secret",
      "query-api-key",
      "query-authorization",
      "query-proxy-authorization",
      "query-session-token",
      "signed-secret",
      "gcp-credential",
      "gcp-signature",
      "gcp-security-token",
      "PRIVATE KEY",
      "private-key-body",
    ]) {
      expect(redacted).not.toContain(secret);
    }
    expect(redacted).toContain("[redacted]");
    expect(redacted).toContain("standalone Bearer [redacted]");
    expect(redacted).toContain("region=eu");
    expect(redacted).toContain('"nextToken":"page-2"');
    expect(redacted).toContain('"token_type":"Bearer"');
    expect(redacted).toContain("multi.example.test/path");
    expect(redacted).toContain("safe suffix");
    expect(redactDiagnosticSecrets(redacted, secrets)).toBe(redacted);
  });

  it("preserves already-redacted and non-userinfo URL at-signs", () => {
    const value =
      "http://<redacted>@broker.example.test https://[redacted]@api.example.test https://host.example.test?email=alice@example.test https://host.example.test#realm@tenant";
    expect(redactDiagnosticSecrets(value, ["redacted"])).toBe(value);
  });

  it("removes whole exact secrets that contain structured credentials", () => {
    for (const [value, secret, expected] of [
      ["prefix Bearer opaque-suffix", "prefix Bearer opaque-suffix", "[redacted]"],
      ["abc?token=xyz", "abc?token=xyz", "[redacted]"],
      ["?token=xyz&supersecretSuffix", "token=xyz&supersecretSuffix", "?[redacted]"],
    ]) {
      expect(redactDiagnosticSecrets(value, [secret])).toBe(expected);
    }
  });

  it("stabilizes structural boundaries created by exact redaction", () => {
    const secrets = ["refresh_token"];
    const redacted = redactDiagnosticSecrets('refresh_tokenBearer b?token=""', secrets);
    expect(redacted).not.toContain("Bearer b");
    expect(redactDiagnosticSecrets(redacted, secrets)).toBe(redacted);
  });

  it("redacts structured values before overlapping exact secrets", () => {
    const redacted = redactDiagnosticSecrets(
      '{"refresh_token":"upstream-minted-value","region":"iad"}',
      ["refresh_token"],
    );
    expect(redacted).not.toContain("upstream-minted-value");
    expect(redacted).toContain("region");
  });

  it("redacts exact secrets that overlap an existing marker", () => {
    for (const [value, secrets] of [
      ["[redacted]suffix route=iad", ["[redacted]suffix"]],
      ["prefix[redacted]suffix route=iad", ["redacted]suffix"]],
      ["[redacted]suffix route=iad", ["redacted", "acted]suffix"]],
      ["triggersuffix route=iad", ["trigger", "redacted]suffix"]],
    ] as const) {
      const redacted = redactDiagnosticSecrets(value, secrets);
      expect(redacted).not.toContain("suffix");
      expect(redacted).toContain("route=iad");
      expect(redacted).not.toContain("[[redacted]");
      expect(redacted).not.toContain("[red[redacted]");
      expect(redactDiagnosticSecrets(redacted, secrets)).toBe(redacted);
    }
  });

  it("fails closed when redaction does not converge within its pass limit", () => {
    const value = "trigger" + "x".repeat(72);
    expect(redactDiagnosticSecrets(value, ["trigger", "redacted]x"])).toBe("[redacted]");
  });

  it("redacts unterminated secret JSON values", () => {
    for (const diagnostic of [
      '{"refresh_token":"unterminated-refresh-value',
      ['{"refresh_token":"dangling-backslash-refresh-value', "\\"].join(""),
    ]) {
      expect(redactDiagnosticSecrets(diagnostic)).not.toContain("refresh-value");
    }
  });

  it("redacts punctuation-bearing credential suffixes while preserving context", () => {
    for (const separator of [",", ";", "'", "}", "&", "#", "?", '\\"']) {
      const credential = `prefix${separator}secret-suffix`;
      expect(redactDiagnosticSecrets(`Authorization: Bearer ${credential} route=iad`)).toBe(
        "Authorization: [redacted] route=iad",
      );
      expect(redactDiagnosticSecrets(`upstream Bearer ${credential} request=retry`)).toBe(
        "upstream Bearer [redacted] request=retry",
      );
    }
    expect(redactDiagnosticSecrets("provider:Authorization: Bearer prefix}secret route=iad")).toBe(
      "provider:Authorization: [redacted] route=iad",
    );
  });
});

async function signedUserToken(secret: string, payload: Record<string, unknown>): Promise<string> {
  const encoder = new TextEncoder();
  const encodedPayload = base64URL(encoder.encode(JSON.stringify(payload)));
  const key = await crypto.subtle.importKey(
    "raw",
    encoder.encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const signature = await crypto.subtle.sign("HMAC", key, encoder.encode(encodedPayload));
  return `cbxu_${encodedPayload}.${base64URL(new Uint8Array(signature))}`;
}

async function accessJwt(input: {
  kid: string;
  aud: string;
  iss: string;
  email: string;
}): Promise<{ jwt: string; publicJwk: JsonWebKey & { kid: string } }> {
  const keyPair = (await crypto.subtle.generateKey(
    {
      name: "RSASSA-PKCS1-v1_5",
      modulusLength: 2048,
      publicExponent: new Uint8Array([1, 0, 1]),
      hash: "SHA-256",
    },
    true,
    ["sign", "verify"],
  )) as CryptoKeyPair;
  const publicJwk = (await crypto.subtle.exportKey("jwk", keyPair.publicKey)) as JsonWebKey & {
    kid: string;
  };
  publicJwk.kid = input.kid;
  publicJwk.alg = "RS256";
  publicJwk.use = "sig";
  const now = Math.floor(Date.now() / 1000);
  const header = base64URL(
    new TextEncoder().encode(JSON.stringify({ alg: "RS256", kid: input.kid, typ: "JWT" })),
  );
  const payload = base64URL(
    new TextEncoder().encode(
      JSON.stringify({
        aud: input.aud,
        email: input.email,
        exp: now + 300,
        iat: now,
        iss: input.iss,
        sub: "access-subject",
      }),
    ),
  );
  const signature = await crypto.subtle.sign(
    "RSASSA-PKCS1-v1_5",
    keyPair.privateKey,
    new TextEncoder().encode(`${header}.${payload}`),
  );
  return { jwt: `${header}.${payload}.${base64URL(new Uint8Array(signature))}`, publicJwk };
}

function accessJwtShape(kid: string): string {
  return `${encodeAccessJwtPart({ alg: "RS256", kid, typ: "JWT" })}.${encodeAccessJwtPart({})}.signature`;
}

function encodeAccessJwtPart(value: object): string {
  return base64URL(new TextEncoder().encode(JSON.stringify(value)));
}
