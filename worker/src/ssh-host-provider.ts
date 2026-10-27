import type { CoordinatorRuntime } from "./coordinator-runtime";
// oxlint-disable eslint/no-await-in-loop -- receipt polling must observe ordered state transitions.
import type { CloudProvider } from "./fleet";
import { leaseProviderCleanupConfirmed } from "./lease-cleanup";
import { putLeaseRecord } from "./lease-record-store";
import type { PoolAccessGrant, ProviderPoolAccess } from "./ready-pool-access";
import { serveCloudProvider } from "./serve-provider";
import { SSHHostCoordinator, hostOnlineMS, hostPrefix, type SSHHost } from "./ssh-host";
import type { Env, LeaseRecord, ReadyPoolImageIdentity } from "./types";

class HostPoolAccess implements ProviderPoolAccess {
  constructor(private readonly runtime: CoordinatorRuntime) {}
  private async command(
    leaseID: string,
    generation: number,
    operation: "prepare" | "clean",
    grant?: PoolAccessGrant,
  ) {
    await this.runtime.storage.transaction(async (s) => {
      const l = await s.get<LeaseRecord>(`lease:${leaseID}`);
      const a = l?.sshHostAllocation;
      if (!l || !a || l.state !== "active") throw new Error("host lease unavailable");
      const h = await s.get<SSHHost>(hostPrefix + a.hostID);
      if (!h || h.leaseID !== l.id || h.generation !== a.generation || h.org !== l.org)
        throw new Error("host allocation changed");
      if (
        generation < a.accessGeneration ||
        (generation === a.accessGeneration && a.operation !== operation && operation === "prepare")
      )
        throw new Error("host access generation fenced");
      if (
        operation === "prepare" &&
        (h.quarantine ||
          !h.enabled ||
          h.draining ||
          Date.now() - h.lastHeartbeat >= hostOnlineMS ||
          !grant ||
          grant.binding.resourceID !== a.hostID ||
          grant.binding.scope !== l.region ||
          grant.binding.user !== l.sshUser ||
          Date.parse(grant.expiresAt) <= Date.now() ||
          a.cleanGeneration !== a.accessGeneration)
      )
        throw new Error("host access not eligible");
      a.accessGeneration = generation;
      a.operation = operation;
      if (grant) {
        a.publicKey = grant.publicKey;
        a.accessDeadline = grant.expiresAt;
      }
      await putLeaseRecord(s, l);
    });
  }
  private async wait(leaseID: string, generation: number, operation: "ready" | "clean") {
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      const l = await this.runtime.storage.get<LeaseRecord>(`lease:${leaseID}`);
      const a = l?.sshHostAllocation;
      if (!a || a.accessGeneration !== generation || l?.state !== "active")
        throw new Error("host access changed");
      if (
        operation === "ready"
          ? a.readyGeneration === generation && a.operation === "prepare"
          : a.cleanGeneration === generation && a.operation === "idle"
      )
        return l;
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    throw new Error("host acknowledgement pending");
  }
  async enroll(l: LeaseRecord) {
    const a = l.sshHostAllocation;
    if (!a) throw new Error("host allocation missing");
    await this.command(l.id, a.accessGeneration, "clean");
    await this.wait(l.id, a.accessGeneration, "clean");
    return {
      leaseID: l.id,
      provider: "ssh-host",
      resourceID: a.hostID,
      scope: l.region!,
      user: l.sshUser,
    };
  }
  async install(grant: PoolAccessGrant) {
    await this.command(grant.leaseID, grant.generation, "prepare", grant);
    const ready = await this.wait(grant.leaseID, grant.generation, "ready");
    return { workRoot: ready.workRoot! };
  }
  async revoke(grant: PoolAccessGrant) {
    const l = await this.runtime.storage.get<LeaseRecord>(`lease:${grant.leaseID}`);
    if (l && leaseProviderCleanupConfirmed(l)) return { destroyed: true };
    await this.command(grant.leaseID, grant.generation, "clean");
    await this.wait(grant.leaseID, grant.generation, "clean");
    return { destroyed: false };
  }
}
export function sshHostCloudProvider(runtime: CoordinatorRuntime, env: Env): CloudProvider {
  const host = new SSHHostCoordinator(runtime, env);
  const identity = (l: LeaseRecord): ReadyPoolImageIdentity | undefined =>
    l.sshHostAllocation
      ? { provider: "ssh-host", scope: l.region!, id: l.sshHostAllocation.template }
      : undefined;
  return {
    ...serveCloudProvider(),
    poolAccess: () => new HostPoolAccess(runtime),
    requiresPortablePoolAccess: true,
    supportsReadyPoolTarget: (l) => l.target === "windows" && Boolean(l.sshHostAllocation),
    readyPoolImageIdentity: identity,
    supportsReadyPoolImageIdentity: (i) => i.provider === "ssh-host" && Boolean(i.scope && i.id),
    releaseLease: async (l) => {
      const current = await runtime.storage.get<LeaseRecord>(`lease:${l.id}`);
      if (current && leaseProviderCleanupConfirmed(current)) return;
      await host.release(l);
      throw new Error("managed host cleanup awaits agent confirmation");
    },
  };
}
