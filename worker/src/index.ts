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
      return env.FLEET.get(fleetID).fetch(requestWithAuthContext(request, mppAuth(env)));
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

function mppAuth(env: Env): AuthContext {
  return {
    authorized: true,
    admin: false,
    auth: "mpp",
    owner: `mpp:${env.CRABBOX_MPP_RECIPIENT?.toLowerCase() ?? "anonymous"}`,
    org: env.CRABBOX_DEFAULT_ORG ?? "mpp",
  };
}

export async function isAuthorized(
  request: Request,
  env: Pick<Env, "CRABBOX_SHARED_TOKEN" | "CRABBOX_SESSION_SECRET" | "CRABBOX_DEFAULT_ORG">,
): Promise<boolean> {
  return Boolean((await authenticateRequest(request, env))?.authorized);
}
