import { requestWithAdminGrantVersion } from "./auth";
import { routeCoordinatorRequest } from "./coordinator-entry";
import { durableObjectFailureResponse, logDurableObjectFailure } from "./durable-object-errors";
import { FleetDurableObject } from "./fleet";
import { fetchReplayableLeaseCreate } from "./lease-admission";
import { runEventAppendFailureResponse } from "./run-event-errors";
import { ServeRelayDurableObject, upgradeServeRelay } from "./serve-relay";
import type { Env } from "./types";

export { FleetDurableObject, ServeRelayDurableObject };

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    return routeCoordinatorRequest(request, env, async (fleetRequest) => {
      try {
        const id = env.FLEET.idFromName("default");
        return await upgradeServeRelay(
          fleetRequest,
          await fetchReplayableLeaseCreate(fleetRequest, (replay) =>
            env.FLEET.get(id).fetch(replay),
          ),
          env,
        );
      } catch (error) {
        const kind = logDurableObjectFailure(error, fleetRequest);
        const failure = runEventAppendFailureResponse(fleetRequest);
        if (failure) return failure;
        return durableObjectFailureResponse(kind);
      }
    });
  },

  async scheduled(
    _controller: ScheduledController,
    env: Env,
    ctx: ExecutionContext,
  ): Promise<void> {
    const request = new Request("https://crabbox.internal/v1/internal/scheduled", {
      method: "POST",
      headers: { "x-crabbox-internal": "scheduled" },
    });
    ctx.waitUntil(
      requestWithAdminGrantVersion(request, env)
        .then((authorizedRequest) => {
          const id = env.FLEET.idFromName("default");
          return env.FLEET.get(id).fetch(authorizedRequest);
        })
        .catch((error: unknown) => {
          logDurableObjectFailure(error, request);
        }),
    );
  },
};
