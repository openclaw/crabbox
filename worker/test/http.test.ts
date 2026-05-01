import { describe, expect, it } from "vitest";

import { isAuthorized } from "../src";
import { authenticateRequest, issueUserToken, requestWithAuthContext } from "../src/auth";
import { requestOwner } from "../src/http";

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
    expect(requestOwner(next)).toBe("friend@example.com");
  });
});
