import { describe, expect, it, vi } from "vitest";

import coordinator, { isAuthorized } from "../src";
import {
  authenticateRequest,
  base64URL,
  issueUserToken,
  requestWithAuthContext,
} from "../src/auth";
import { requestOwner } from "../src/http";
import type { Env } from "../src/types";

describe("coordinator auth", () => {
  it("denies requests when no shared token is configured", async () => {
    const request = new Request("https://example.test/v1/pool");
    await expect(isAuthorized(request, {})).resolves.toBe(false);
  });

  it("requires the configured bearer token", async () => {
    const denied = new Request("https://example.test/v1/pool");
    const allowed = new Request("https://example.test/v1/pool", {
      headers: { authorization: "Bearer secret" },
    });
    await expect(isAuthorized(denied, { CRABBOX_SHARED_TOKEN: "secret" })).resolves.toBe(false);
    await expect(isAuthorized(allowed, { CRABBOX_SHARED_TOKEN: "secret" })).resolves.toBe(true);
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

  it("accepts signed GitHub user tokens without admin rights", async () => {
    const env = { CRABBOX_SHARED_TOKEN: "shared", CRABBOX_DEFAULT_ORG: "openclaw" };
    const token = await issueUserToken(env, {
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
    });
    const request = new Request("https://example.test/v1/whoami", {
      headers: { authorization: `Bearer ${token}`, "x-crabbox-owner": "spoof@example.com" },
    });
    const auth = await authenticateRequest(request, env);
    expect(auth).toMatchObject({
      authorized: true,
      admin: false,
      auth: "github",
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
    });
  });

  it("rejects signed user tokens with admin claims", async () => {
    const env = { CRABBOX_SHARED_TOKEN: "shared", CRABBOX_DEFAULT_ORG: "openclaw" };
    const now = Math.floor(Date.now() / 1000);
    const token = await signedUserToken("shared", {
      typ: "crabbox-user",
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
    const token = await signedUserToken("shared", {
      typ: "crabbox-user",
      owner: "friend@example.com",
      org: "openclaw",
      login: "friend",
      admin: true,
      iat: now,
      exp: now + 300,
    });
    const env = {
      CRABBOX_SHARED_TOKEN: "shared",
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

  it("does not let caller-supplied Access identity override signed user token identity", () => {
    const request = new Request("https://example.test/v1/whoami", {
      headers: {
        "cf-access-authenticated-user-email": "spoof@example.com",
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
    expect(requestOwner(next)).toBe("friend@example.com");
  });

  it("redirects browser portal auth routes to the configured public origin", async () => {
    let fleetCalled = false;
    const env = {
      CRABBOX_PUBLIC_URL: "https://crabbox.openclaw.ai",
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
      "https://crabbox.openclaw.ai/portal/login?returnTo=%2Fportal%2Fleases%2Fcbx_1%2Fvnc",
    );

    const logout = await coordinator.fetch(
      new Request("https://crabbox-coordinator.steipete.workers.dev/portal/logout"),
      env,
    );
    expect(logout.status).toBe(302);
    expect(logout.headers.get("location")).toBe("https://crabbox.openclaw.ai/portal/logout");
    expect(fleetCalled).toBe(false);
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
