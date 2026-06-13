import { authenticateRequest } from "./auth";
import { routeCoordinatorRequest } from "./coordinator-entry";
import { FleetDurableObject } from "./fleet";
import type { Env } from "./types";

export { FleetDurableObject };

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    return routeCoordinatorRequest(request, env, async (fleetRequest) => {
      const id = env.FLEET.idFromName("default");
      return env.FLEET.get(id).fetch(fleetRequest);
    });
  },

  async scheduled(
    _controller: ScheduledController,
    env: Env,
    ctx: ExecutionContext,
  ): Promise<void> {
    const id = env.FLEET.idFromName("default");
    ctx.waitUntil(
      env.FLEET.get(id).fetch("https://crabbox.internal/v1/internal/scheduled", {
        method: "POST",
        headers: { "x-crabbox-internal": "scheduled" },
      }),
    );
  },
};

export async function isAuthorized(
  request: Request,
  env: Pick<
    Env,
    | "CRABBOX_SHARED_TOKEN"
    | "CRABBOX_SHARED_OWNER"
    | "CRABBOX_ADMIN_TOKEN"
    | "CRABBOX_SESSION_SECRET"
    | "CRABBOX_DEFAULT_ORG"
    | "CRABBOX_ACCESS_TEAM_DOMAIN"
    | "CRABBOX_ACCESS_AUD"
  >,
): Promise<boolean> {
  return Boolean((await authenticateRequest(request, env))?.authorized);
}
