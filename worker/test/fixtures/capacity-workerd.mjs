import { routeCoordinatorRequest } from "../../src/coordinator-entry";
import { CloudflareCoordinatorRuntime } from "../../src/coordinator-runtime";
import { FleetCoordinator } from "../../src/fleet";
import { maintainLeaseIndex } from "../../src/lease-maintenance-index";
import { leaseMaintenanceIndex } from "../../src/lease-record-store";
import { orgKeyForLabel } from "../../src/org-identity";

const generation = "synthetic-capacity-workerd";
const owner = "github:12345";
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

export class CapacityProof {
  constructor(ctx) {
    this.stats = { gets: 0, lists: [] };
    this.observe = false;
    this.pause = false;
    const storage = new Proxy(ctx.storage, {
      get: (target, key) => {
        const value = Reflect.get(target, key, target);
        if (key === "get") {
          return (...args) => {
            if (this.observe) {
              this.stats.gets++;
              if (!args[1]?.allowConcurrency) throw new Error("blocking diagnostic get");
            }
            return value.apply(target, args);
          };
        }
        if (key === "list") {
          return (...args) => {
            if (this.observe) {
              this.stats.lists.push(args[0]?.prefix);
              if (!args[0]?.allowConcurrency) throw new Error("blocking diagnostic list");
            }
            return value.apply(target, args);
          };
        }
        return typeof value === "function" ? value.bind(target) : value;
      },
    });
    this.runtime = new CloudflareCoordinatorRuntime({
      storage,
      blockConcurrencyWhile: ctx.blockConcurrencyWhile.bind(ctx),
      waitUntil: ctx.waitUntil.bind(ctx),
      getWebSockets: () => [],
    });
    this.env = {
      CRABBOX_SHARED_TOKEN: "synthetic-shared",
      CRABBOX_SHARED_OWNER: owner,
      CRABBOX_DEFAULT_ORG: "example-org",
      CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER: "10",
    };
    this.fleet = new FleetCoordinator(this.runtime, this.env, {}, {}, generation);
    const snapshot = this.runtime.readSnapshot.bind(this.runtime);
    this.runtime.readSnapshot = (callback) =>
      snapshot(async (view) => {
        const result = await callback(view);
        if (this.pause) {
          this.pause = false;
          await new Promise((resolve) => {
            const timer = setTimeout(resolve, 5000);
            this.release = () => {
              clearTimeout(timer);
              resolve();
            };
          });
        }
        return result;
      });
  }

  async fetch(request) {
    const route = new URL(request.url).pathname;
    if (route === "/seed") {
      const now = new Date().toISOString();
      for (let offset = 0; offset < 5000; offset += 100) {
        const batch = Object.fromEntries(
          Array.from({ length: 100 }, (_, i) => [
            `lease:history-${offset + i}`,
            { id: `history-${offset + i}`, owner, state: "released", provider: "aws" },
          ]),
        );
        await this.runtime.storage.put(batch);
      }
      await this.runtime.storage.put("lease:active", {
        id: "active",
        owner,
        org: orgKeyForLabel("example-org"),
        state: "active",
        provider: "aws",
        createdAt: now,
        updatedAt: now,
        expiresAt: new Date(Date.now() + 3600000).toISOString(),
      });
      await maintainLeaseIndex(this.runtime.storage, leaseMaintenanceIndex, Date.now(), generation);
      await maintainLeaseIndex(this.runtime.storage, leaseMaintenanceIndex, Date.now(), generation);
      this.observe = true;
      return Response.json({ seeded: 5001 });
    }
    if (route === "/blocking-control") {
      await this.runtime.storage.transaction(async () => sleep(400));
      return Response.json({ done: true });
    }
    if (route === "/heartbeat" || route === "/finish") {
      await this.runtime.runExclusive(() =>
        route === "/finish"
          ? this.runtime.storage.transaction((view) => view.put("synthetic-finish", "complete"))
          : this.runtime.storage.put("synthetic-heartbeat", Date.now()),
      );
      this.release?.();
      this.release = undefined;
      return Response.json({ lifecycle: route });
    }
    if (route === "/pause") {
      this.pause = true;
      return Response.json({ armed: true });
    }
    if (route === "/stats") return Response.json(this.stats);
    if (route === "/capacity") {
      return routeCoordinatorRequest(
        new Request("https://coordinator.test/v1/capacity", {
          headers: { authorization: "Bearer synthetic-shared" },
        }),
        this.env,
        (value) => this.fleet.fetch(value),
        { githubMembership: async () => {} },
      );
    }
    return new Response("not found", { status: 404 });
  }

  alarm() {}
}

export default { fetch: () => new Response("fixture only") };
