import { authenticateRequest, requestWithAuthContext, type AuthContext } from "./auth";
import { FleetDurableObject } from "./fleet";
import { json } from "./http";
import { paymentConfigured } from "./payments";
import type { Env } from "./types";

export { FleetDurableObject };

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/v1/health") {
      return json({ ok: true, service: "crabbox-coordinator" });
    }
    if (url.pathname.startsWith("/v1/auth/")) {
      const id = env.FLEET.idFromName("default");
      return env.FLEET.get(id).fetch(request);
    }
    const auth = await authenticateRequest(request, env);
    const fleetID = env.FLEET.idFromName("default");
    if (auth?.authorized) {
      return env.FLEET.get(fleetID).fetch(requestWithAuthContext(request, auth));
    }
    if (mppEligible(request, url, env)) {
      return env.FLEET.get(fleetID).fetch(requestWithAuthContext(request, mppAuth(request, env)));
    }
    return json({ error: "unauthorized" }, { status: 401 });
  },
};

function mppEligible(request: Request, url: URL, env: Env): boolean {
  if (request.method !== "POST" || url.pathname !== "/v1/leases") {
    return false;
  }
  return paymentConfigured(env);
}

function mppAuth(request: Request, env: Env): AuthContext {
  const payer = extractCredentialPayer(request);
  const owner = payer
    ? `mpp:${payer.toLowerCase()}`
    : `mpp:${env.CRABBOX_MPP_RECIPIENT?.toLowerCase() ?? "anonymous"}`;
  return {
    authorized: true,
    admin: false,
    auth: "mpp",
    owner,
    org: env.CRABBOX_DEFAULT_ORG ?? "mpp",
  };
}

export function extractCredentialPayer(request: Request): string | undefined {
  const auth = request.headers.get("authorization");
  if (!auth?.startsWith("Payment ")) {
    return undefined;
  }
  try {
    const padded = auth
      .slice(8)
      .replaceAll("-", "+")
      .replaceAll("_", "/")
      .padEnd(Math.ceil((auth.length - 8) / 4) * 4, "=");
    const decoded = JSON.parse(atob(padded)) as { source?: string };
    if (typeof decoded.source !== "string") {
      return undefined;
    }
    const match = /:0x([a-fA-F0-9]{40})$/.exec(decoded.source);
    return match ? `0x${match[1]}` : undefined;
  } catch {
    return undefined;
  }
}

export async function isAuthorized(
  request: Request,
  env: Pick<Env, "CRABBOX_SHARED_TOKEN" | "CRABBOX_SESSION_SECRET" | "CRABBOX_DEFAULT_ORG">,
): Promise<boolean> {
  return Boolean((await authenticateRequest(request, env))?.authorized);
}
