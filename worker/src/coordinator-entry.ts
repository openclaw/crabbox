import {
  authenticateUserTokenForRevocation,
  authenticateRequest,
  requestWithAdminGrantVersion,
  requestWithAuthContext,
  requestWithoutTrustedHeaders,
  type AuthContext,
  type AuthRequestContext,
} from "./auth";
import { codeProxyRequestBodyBytes, isIsolatedCodeRequest } from "./code-origin";
import { cookieValue, portalSessionCookieName } from "./cookies";
import { bearerToken, json, pathParts } from "./http";
import { InvalidOrgLabelError, orgKeyForLabel } from "./org-identity";
import { runtimeAdapterProxyPath, runtimeAdapterRelayMethodAllowed } from "./runtime-adapter-relay";
import { timingSafeEqual } from "./timing-safe";
import type { Env } from "./types";

export type CoordinatorFetch = (request: Request) => Promise<Response>;
export type PreparedCoordinatorRequest =
  | { response: Response; authenticated: false }
  | { request: Request; authenticated: boolean; bodyLimit?: number };

export async function routeCoordinatorRequest(
  request: Request,
  env: Env,
  fleetFetch: CoordinatorFetch,
  authContext: AuthRequestContext = {},
): Promise<Response> {
  const prepared = await prepareCoordinatorRequest(request, env, authContext);
  if ("response" in prepared) {
    return prepared.response;
  }
  return fleetFetch(prepared.request);
}

export async function prepareCoordinatorRequest(
  request: Request,
  env: Env,
  authContext: AuthRequestContext = {},
): Promise<PreparedCoordinatorRequest> {
  const url = new URL(request.url);
  if (request.method === "GET" && url.pathname === "/v1/health") {
    return { response: json({ ok: true, service: "crabbox-coordinator" }), authenticated: false };
  }
  if (request.method === "GET" && url.pathname === "/") {
    return {
      response: new Response(null, { status: 302, headers: { location: "/portal" } }),
      authenticated: false,
    };
  }
  const isolatedCode = await isIsolatedCodeRequest(request, env);
  const canonicalPortal = await canonicalPortalRedirect(request, env, url, isolatedCode);
  if (canonicalPortal) {
    return { response: canonicalPortal, authenticated: false };
  }
  if (url.pathname.startsWith("/v1/auth/")) {
    return { request: requestWithoutTrustedHeaders(request), authenticated: false };
  }
  if (url.pathname === "/portal/login") {
    return { request: requestWithoutTrustedHeaders(request), authenticated: false };
  }
  if (
    request.method === "GET" &&
    url.pathname === "/v1/native-vnc/handoff" &&
    request.headers.get("upgrade")?.toLowerCase() === "websocket"
  ) {
    return { request: requestWithoutTrustedHeaders(request), authenticated: false };
  }
  const route = pathParts(request);
  if (route[0] === "v1" && route[1] === "internal") {
    return {
      response: json({ error: "not_found" }, { status: 404 }),
      authenticated: false,
    };
  }
  if (isolatedCode) {
    return {
      request: requestWithoutTrustedHeaders(request),
      authenticated: false,
      bodyLimit: codeProxyRequestBodyBytes,
    };
  }
  if (
    isWebVNCAgentUpgrade(request, url) ||
    isCodeAgentUpgrade(request, url) ||
    isEgressAgentUpgrade(request, url) ||
    isRuntimeAdapterAgentUpgrade(request, url)
  ) {
    return { request: requestWithoutTrustedHeaders(request), authenticated: false };
  }
  const runtimeAdapterAuth = runtimeAdapterServiceAuth(request, env, route);
  if (runtimeAdapterAuth) {
    return await authenticatedCoordinatorRequest(
      requestWithoutTrustedHeaders(request),
      runtimeAdapterAuth,
      env,
    );
  }
  const runtimeAdapterPath = runtimeAdapterProxyPath(route);
  if (
    env.CRABBOX_WORKSPACE_AWS_PRIVATE?.trim() === "1" &&
    runtimeAdapterPath &&
    runtimeAdapterRelayMethodAllowed(request.method, runtimeAdapterPath)
  ) {
    return {
      response: json({ error: "unauthorized" }, { status: 401 }),
      authenticated: false,
    };
  }
  const portal = url.pathname.startsWith("/portal");
  if (portal && !portalCookieRequestIntentAllowed(request, env, url)) {
    return {
      response: json({ error: "portal_request_origin_forbidden" }, { status: 403 }),
      authenticated: false,
    };
  }
  if (isWebVNCViewerBootstrap(request, url) || isWebVNCViewerSessionRequest(request, url)) {
    return {
      request: await requestWithAdminGrantVersion(
        requestWithoutCoordinatorAuthContext(request),
        env,
      ),
      authenticated: false,
    };
  }
  const authRequest = portal ? requestWithPortalCookie(request) : request;
  const portalLogoutToken =
    portal &&
    request.method === "POST" &&
    url.pathname === "/portal/logout" &&
    !request.headers.has("authorization")
      ? cookieValue(request.headers.get("cookie") ?? "", portalSessionCookieName)
      : undefined;
  const auth = portalLogoutToken
    ? await authenticateUserTokenForRevocation(portalLogoutToken, env)
    : await authenticateRequest(authRequest, env, authContext);
  if (!auth?.authorized) {
    if (portal && request.method === "GET" && request.headers.get("upgrade") !== "websocket") {
      const login = new URL("/portal/login", url.origin);
      login.searchParams.set("returnTo", `${url.pathname}${url.search}`);
      return {
        response: new Response(null, {
          status: 302,
          headers: { location: login.pathname + login.search },
        }),
        authenticated: false,
      };
    }
    return {
      response: json({ error: "unauthorized" }, { status: 401 }),
      authenticated: false,
    };
  }
  return await authenticatedCoordinatorRequest(authRequest, auth, env);
}

async function authenticatedCoordinatorRequest(
  request: Request,
  auth: AuthContext,
  env: Env,
): Promise<PreparedCoordinatorRequest> {
  try {
    if (auth.org) {
      orgKeyForLabel(auth.org);
    }
  } catch (error) {
    if (!(error instanceof InvalidOrgLabelError)) {
      throw error;
    }
    return {
      response: json({ error: "invalid_org_identity", message: error.message }, { status: 503 }),
      authenticated: false,
    };
  }
  return {
    request: await requestWithAdminGrantVersion(requestWithAuthContext(request, auth), env),
    authenticated: true,
  };
}

function portalCookieRequestIntentAllowed(request: Request, env: Env, url: URL): boolean {
  if (request.headers.get("authorization")) return true;
  const cookie = request.headers.get("cookie") ?? "";
  if (
    !cookieValue(cookie, portalSessionCookieName) &&
    !cookieValue(cookie, "crabbox_webvnc_session")
  ) {
    return true;
  }
  const method = request.method.toUpperCase();
  const websocket =
    method === "GET" && request.headers.get("upgrade")?.toLowerCase() === "websocket";
  if (!websocket && (method === "GET" || method === "HEAD" || method === "OPTIONS")) return true;
  const origin = request.headers.get("origin")?.trim();
  if (!origin) return false;
  try {
    const trustedOrigin = env.CRABBOX_PUBLIC_URL
      ? new URL(env.CRABBOX_PUBLIC_URL).origin
      : url.origin;
    return url.origin === trustedOrigin && new URL(origin).origin === trustedOrigin;
  } catch {
    return false;
  }
}

function isWebVNCViewerBootstrap(request: Request, url: URL): boolean {
  return (
    request.method.toUpperCase() === "POST" &&
    /^\/portal\/leases\/[^/]+\/vnc\/bootstrap$/.test(url.pathname)
  );
}

function isWebVNCViewerSessionRequest(request: Request, url: URL): boolean {
  if (request.headers.has("authorization")) {
    return false;
  }
  const session = cookieValue(request.headers.get("cookie") ?? "", "crabbox_webvnc_session");
  return (
    /^webvnc_session_[a-f0-9]{32}$/.test(session) &&
    /^\/portal\/leases\/[^/]+\/vnc(?:\/(?:status|control|theme|handoff|viewer))?$/.test(
      url.pathname,
    )
  );
}

function requestWithoutCoordinatorAuthContext(request: Request): Request {
  const clean = requestWithoutTrustedHeaders(request);
  const headers = new Headers(clean.headers);
  for (const name of [
    "x-crabbox-auth",
    "x-crabbox-admin",
    "x-crabbox-owner",
    "x-crabbox-org",
    "x-crabbox-github-login",
    "x-crabbox-token-expires-at",
  ]) {
    headers.delete(name);
  }
  return new Request(clean, { headers });
}

function isWebVNCAgentUpgrade(request: Request, url: URL): boolean {
  return (
    request.method === "GET" &&
    request.headers.get("upgrade")?.toLowerCase() === "websocket" &&
    /^\/v1\/leases\/[^/]+\/webvnc\/agent$/.test(url.pathname)
  );
}

function isCodeAgentUpgrade(request: Request, url: URL): boolean {
  return (
    request.method === "GET" &&
    request.headers.get("upgrade")?.toLowerCase() === "websocket" &&
    /^\/v1\/leases\/[^/]+\/code\/agent$/.test(url.pathname)
  );
}

function isEgressAgentUpgrade(request: Request, url: URL): boolean {
  return (
    request.method === "GET" &&
    request.headers.get("upgrade")?.toLowerCase() === "websocket" &&
    /^\/v1\/leases\/[^/]+\/egress\/(?:host|client)$/.test(url.pathname)
  );
}

function isRuntimeAdapterAgentUpgrade(request: Request, url: URL): boolean {
  return (
    request.method === "GET" &&
    request.headers.get("upgrade")?.toLowerCase() === "websocket" &&
    /^\/v1\/adapters\/[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\/agent$/.test(url.pathname)
  );
}

function runtimeAdapterServiceAuth(
  request: Request,
  env: Pick<
    Env,
    | "CRABBOX_RUNTIME_ADAPTER_TOKEN"
    | "CRABBOX_RUNTIME_ADAPTER_OWNER"
    | "CRABBOX_RUNTIME_ADAPTER_ORG"
    | "CRABBOX_DEFAULT_ORG"
  >,
  route: string[],
): AuthContext | undefined {
  const path = runtimeAdapterProxyPath(route);
  if (!path || !runtimeAdapterRelayMethodAllowed(request.method, path)) {
    return undefined;
  }
  const expected = env.CRABBOX_RUNTIME_ADAPTER_TOKEN?.trim();
  if (!expected || !timingSafeEqual(bearerToken(request) ?? "", expected)) {
    return undefined;
  }
  return {
    authorized: true,
    admin: false,
    auth: "bearer",
    owner: env.CRABBOX_RUNTIME_ADAPTER_OWNER || "service@openclaw.org",
    org: env.CRABBOX_RUNTIME_ADAPTER_ORG || env.CRABBOX_DEFAULT_ORG || "openclaw",
  };
}

async function canonicalPortalRedirect(
  request: Request,
  env: Env,
  url: URL,
  isolatedCode: boolean,
): Promise<Response | undefined> {
  const bootstrap = isWebVNCViewerBootstrap(request, url);
  if (
    (request.method !== "GET" && !bootstrap) ||
    request.headers.get("upgrade")?.toLowerCase() === "websocket" ||
    !url.pathname.startsWith("/portal") ||
    isolatedCode ||
    !env.CRABBOX_PUBLIC_URL
  ) {
    return undefined;
  }
  let publicURL: URL;
  try {
    publicURL = new URL(env.CRABBOX_PUBLIC_URL);
  } catch {
    return undefined;
  }
  if (url.origin === publicURL.origin) {
    return undefined;
  }
  const location = new URL(`${url.pathname}${url.search}`, publicURL.origin);
  if (bootstrap) {
    const contentType = request.headers.get("content-type")?.split(";", 1)[0]?.trim().toLowerCase();
    const ticket =
      contentType === "application/x-www-form-urlencoded"
        ? (new URLSearchParams(await request.clone().text()).get("ticket") ?? "")
        : "";
    if (/^webvnc_view_[a-f0-9]{32}$/.test(ticket)) {
      const nonce = crypto.randomUUID().replaceAll("-", "");
      return new Response(
        `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="referrer" content="no-referrer"><title>Opening WebVNC</title></head><body><form id="webvnc-bootstrap" method="post" action="${escapeHTMLAttribute(location.toString())}" autocomplete="off"><input type="hidden" name="ticket" value="${escapeHTMLAttribute(ticket)}"><p>Opening WebVNC...</p><button type="submit">Continue</button></form><script nonce="${nonce}">document.getElementById("webvnc-bootstrap").requestSubmit()</script></body></html>`,
        {
          status: 200,
          headers: {
            "cache-control": "no-store",
            "content-security-policy": `default-src 'none'; base-uri 'none'; form-action ${publicURL.origin}; frame-ancestors 'none'; script-src 'nonce-${nonce}'`,
            "content-type": "text/html; charset=utf-8",
            "referrer-policy": "no-referrer",
            "x-content-type-options": "nosniff",
          },
        },
      );
    }
  }
  return new Response(null, {
    status: bootstrap ? 307 : 302,
    headers: {
      location: location.toString(),
      ...(bootstrap ? { "cache-control": "no-store" } : {}),
    },
  });
}

function escapeHTMLAttribute(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll('"', "&quot;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;");
}

function requestWithPortalCookie(request: Request): Request {
  if (request.headers.get("authorization")) {
    return request;
  }
  const token = cookieValue(request.headers.get("cookie") ?? "", portalSessionCookieName);
  if (!token) {
    return request;
  }
  const headers = new Headers(request.headers);
  headers.set("authorization", `Bearer ${token}`);
  return new Request(request, { headers });
}
