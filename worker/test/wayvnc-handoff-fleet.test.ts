import { expect, it } from "vitest";

import { FleetCoordinator } from "../src/fleet";

type Viewer = { id: string; agentID: string; label: string; socket: WebSocket };
type Harness = {
  webVNCControllers: Map<string, string>;
  webVNCViewers: Map<string, Map<string, Viewer>>;
  webVNCHandoffs: Map<string, { viewerID: string; status: string }>;
  webVNCTakeControl(request: Request, identifier: string): Promise<Response>;
};

it("fences remote retirement across overlapping controller generations", async () => {
  const fleet = Object.create(FleetCoordinator.prototype) as Harness;
  const sockets = [0, 1, 2].map(() => ({ readyState: 1 }) as WebSocket);
  const viewers = sockets.map((socket, index) => ({
    id: `viewer_00000${index}`,
    agentID: `agent_00000${index}`,
    label: `viewer ${index}`,
    socket,
  }));
  const leaseID = "cbx_handoff";
  const pending: Array<(retired: boolean) => void> = [];
  Object.assign(fleet, {
    webVNCControllers: new Map([[leaseID, viewers[0]!.id]]),
    webVNCViewers: new Map([[leaseID, new Map(viewers.map((viewer) => [viewer.id, viewer]))]]),
    webVNCAgents: new Map([
      [leaseID, new Map(viewers.map((viewer) => [viewer.agentID, viewer.socket]))],
    ]),
    webVNCHandoffs: new Map(),
    webVNCEvents: new Map(),
    resolvePortalLease: async () => ({
      id: leaseID,
      state: "active",
      desktop: true,
      host: "127.0.0.1",
    }),
    wayVNCRetirement: { retire: () => new Promise<boolean>((resolve) => pending.push(resolve)) },
    webVNCStatus: async () => new Response("{}"),
  });
  const takeover = (index: number) =>
    fleet.webVNCTakeControl(
      new Request("https://example.test/portal/vnc/control", {
        method: "POST",
        body: JSON.stringify({ viewerID: viewers[index]!.id }),
      }),
      leaseID,
    );
  const first = takeover(1);
  await new Promise((resolve) => setImmediate(resolve));
  expect(fleet.webVNCHandoffs.get(leaseID)?.status).toBe("pending");
  const second = takeover(2);
  await new Promise((resolve) => setImmediate(resolve));
  await second;
  expect(fleet.webVNCHandoffs.get(leaseID)).toEqual({
    viewerID: viewers[2]!.id,
    status: "manual",
  });
  pending[0]!(true);
  await first;
  expect(fleet.webVNCHandoffs.get(leaseID)?.status).toBe("manual");
  expect(pending).toHaveLength(1);

  // A failed chain cannot silently promote an earlier observer's layout owner.
  await takeover(1);
  expect(fleet.webVNCHandoffs.get(leaseID)?.status).toBe("manual");
  expect(pending).toHaveLength(1);

  // Closing/resetting the viewer group starts a fresh ownership chain.
  fleet.webVNCHandoffs.clear();
  const third = takeover(2);
  await new Promise((resolve) => setImmediate(resolve));
  fleet.webVNCViewers.get(leaseID)!.set(viewers[2]!.id, {
    ...viewers[2]!,
    socket: sockets[0]!,
  });
  pending[1]!(true);
  await third;
  expect(fleet.webVNCHandoffs.get(leaseID)?.status).toBe("manual");
});
