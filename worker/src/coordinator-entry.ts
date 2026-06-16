import {
  authenticateRequest,
  requestWithAuthContext,
  requestWithoutProxySecret,
  type AuthContext,
  type AuthRequestContext,
} from "./auth";
import { bearerToken, json } from "./http";
import type { Env } from "./types";

export type CoordinatorFetch = (request: Request) => Promise<Response>;
export type PreparedCoordinatorRequest =
  | { response: Response; authenticated: false }
  | { request: Request; authenticated: boolean };

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
  const canonicalPortal = canonicalPortalRedirect(request, env, url);
  if (canonicalPortal) {
    return { response: canonicalPortal, authenticated: false };
  }
  if (url.pathname.startsWith("/v1/auth/")) {
    return { request: requestWithoutProxySecret(request), authenticated: false };
  }
  if (url.pathname === "/portal/login" || url.pathname === "/portal/logout") {
    return { request: requestWithoutProxySecret(request), authenticated: false };
  }
  if (url.pathname.startsWith("/v1/internal/")) {
    return {
      response: json({ error: "not_found" }, { status: 404 }),
      authenticated: false,
    };
  }
  if (
    isWebVNCAgentUpgrade(request, url) ||
    isCodeAgentUpgrade(request, url) ||
    isEgressAgentUpgrade(request, url) ||
    isRuntimeAdapterAgentUpgrade(request, url)
  ) {
    return { request: requestWithoutProxySecret(request), authenticated: false };
  }
  const runtimeAdapterAuth = runtimeAdapterServiceAuth(request, env, url);
  if (runtimeAdapterAuth) {
    return {
      request: requestWithAuthContext(requestWithoutProxySecret(request), runtimeAdapterAuth),
      authenticated: true,
    };
  }
  const portal = url.pathname.startsWith("/portal");
  const authRequest = portal ? requestWithPortalCookie(request) : request;
  const auth = await authenticateRequest(authRequest, env, authContext);
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
  return { request: requestWithAuthContext(authRequest, auth), authenticated: true };
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
  env: Pick<Env, "CRABBOX_RUNTIME_ADAPTER_TOKEN" | "CRABBOX_DEFAULT_ORG">,
  url: URL,
): AuthContext | undefined {
  if (url.pathname !== "/v1/workspaces" && !url.pathname.startsWith("/v1/workspaces/")) {
    return undefined;
  }
  const expected = env.CRABBOX_RUNTIME_ADAPTER_TOKEN?.trim();
  if (!expected || bearerToken(request) !== expected) {
    return undefined;
  }
  return {
    authorized: true,
    admin: false,
    auth: "bearer",
    owner: "service@openclaw.org",
    org: env.CRABBOX_DEFAULT_ORG ?? "openclaw",
  };
}

function canonicalPortalRedirect(request: Request, env: Env, url: URL): Response | undefined {
  if (
    request.method !== "GET" ||
    request.headers.get("upgrade")?.toLowerCase() === "websocket" ||
    !url.pathname.startsWith("/portal") ||
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
  return new Response(null, { status: 302, headers: { location: location.toString() } });
}

function requestWithPortalCookie(request: Request): Request {
  if (request.headers.get("authorization")) {
    return request;
  }
  const token = cookieValue(request.headers.get("cookie") ?? "", "crabbox_session");
  if (!token) {
    return request;
  }
  const headers = new Headers(request.headers);
  headers.set("authorization", `Bearer ${token}`);
  return new Request(request, { headers });
}

function cookieValue(header: string, name: string): string {
  for (const part of header.split(";")) {
    const [rawKey, ...rawValue] = part.trim().split("=");
    if (rawKey === name) {
      return decodeURIComponent(rawValue.join("="));
    }
  }
  return "";
}
