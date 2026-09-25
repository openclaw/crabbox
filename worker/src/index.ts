import { requestWithAdminGrantVersion } from "./auth";
import { routeCoordinatorRequest } from "./coordinator-entry";
import { FleetDurableObject } from "./fleet";
import { fetchReplayableLeaseCreate } from "./lease-admission";
import type { Env } from "./types";

export { FleetDurableObject };

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    return routeCoordinatorRequest(request, env, async (fleetRequest) => {
      const id = env.FLEET.idFromName("default");
      return fetchReplayableLeaseCreate(fleetRequest, (replay) => env.FLEET.get(id).fetch(replay));
    });
  },

  async scheduled(
    _controller: ScheduledController,
    env: Env,
    ctx: ExecutionContext,
  ): Promise<void> {
    const id = env.FLEET.idFromName("default");
    ctx.waitUntil(
      requestWithAdminGrantVersion(
        new Request("https://crabbox.internal/v1/internal/scheduled", {
          method: "POST",
          headers: { "x-crabbox-internal": "scheduled" },
        }),
        env,
      ).then((request) => env.FLEET.get(id).fetch(request)),
    );
  },
};
