import { authenticateRequest, requestWithAuthContext } from "./auth";
import { json } from "./http";
import type { Env } from "./types";

export type CoordinatorFetch = (request: Request) => Promise<Response>;

export async function routeCoordinatorRequest(
  request: Request,
  env: Env,
  fleetFetch: CoordinatorFetch,
): Promise<Response> {
  const url = new URL(request.url);
  if (request.method === "GET" && url.pathname === "/v1/health") {
    return json({ ok: true, service: "crabbox-coordinator" });
  }
  if (request.method === "GET" && url.pathname === "/") {
    return new Response(null, { status: 302, headers: { location: "/portal" } });
  }
  const canonicalPortal = canonicalPortalRedirect(request, env, url);
  if (canonicalPortal) {
    return canonicalPortal;
  }
  if (url.pathname.startsWith("/v1/auth/")) {
    return fleetFetch(request);
  }
  if (url.pathname === "/portal/login" || url.pathname === "/portal/logout") {
    return fleetFetch(request);
  }
  if (url.pathname.startsWith("/v1/internal/")) {
    return json({ error: "not_found" }, { status: 404 });
  }
  if (
    isWebVNCAgentUpgrade(request, url) ||
    isCodeAgentUpgrade(request, url) ||
    isEgressAgentUpgrade(request, url)
  ) {
    return fleetFetch(request);
  }
  const portal = url.pathname.startsWith("/portal");
  const authRequest = portal ? requestWithPortalCookie(request) : request;
  const auth = await authenticateRequest(authRequest, env);
  if (!auth?.authorized) {
    if (portal && request.method === "GET" && request.headers.get("upgrade") !== "websocket") {
      const login = new URL("/portal/login", url.origin);
      login.searchParams.set("returnTo", `${url.pathname}${url.search}`);
      return new Response(null, {
        status: 302,
        headers: { location: login.pathname + login.search },
      });
    }
    return json({ error: "unauthorized" }, { status: 401 });
  }
  return fleetFetch(requestWithAuthContext(authRequest, auth));
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
