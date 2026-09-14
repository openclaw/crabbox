import { afterEach, describe, expect, it, vi } from "vitest";

import {
  LeaseProvisioningController,
  provisioningAttemptKey,
  provisioningOperationKey,
  provisioningPlanKey,
  putProvisioningOperation,
  type LeaseProvisioningOperation,
} from "../src/lease-provisioning";
import type {
  FrozenProvisioningPlan,
  ProviderResumableProvisioning,
} from "../src/provider-provisioning";
import type { Env, LeaseRecord } from "../src/types";
import { ProvisioningTestRuntime, ProvisioningTestStorage } from "./provisioning-fixtures";

const leaseID = "cbx_0123456789ab";
const operationKey = provisioningOperationKey(leaseID);
const coordinationKey = "provisioning-lock:example-resource-group";

async function cleanupFixture() {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-09-14T12:00:00Z"));
  const now = Date.now();
  const storage = new ProvisioningTestStorage();
  const lease: LeaseRecord = {
    id: leaseID,
    createAttemptGeneration: "generation-one",
    provider: "azure",
    providerScope: "example-scope",
    target: "linux",
    cloudID: "example-service",
    owner: "alice@example.com",
    org: "example-org",
    profile: "default",
    class: "small",
    serverType: "small",
    serverID: 0,
    serverName: "example-service",
    providerKey: "",
    host: "",
    sshUser: "crabbox",
    sshPort: "22",
    workRoot: "/workspace/crabbox",
    keep: false,
    ttlSeconds: 600,
    estimatedHourlyUSD: 0,
    maxEstimatedUSD: 0,
    state: "released",
    releaseDeletesServer: true,
    createdAt: new Date(now - 600_000).toISOString(),
    updatedAt: new Date(now).toISOString(),
    expiresAt: new Date(now - 1).toISOString(),
  };
  const operation: LeaseProvisioningOperation = {
    schema: 1,
    leaseID,
    operationID: "operation-one",
    generation: "generation-one",
    scope: "example-scope",
    owner: lease.owner,
    org: lease.org,
    provider: "azure",
    createdAt: now - 600_000,
    // Cleanup must still work after the provisioning deadline has elapsed.
    deadline: now - 1,
    canceledAt: now - 1,
    revision: 0,
    step: {
      phase: "cleanup",
      attempt: 0,
      state: { action: "delete", resourceID: "example-service" },
      coordinationKey: "example-resource-group",
      nextWake: now,
    },
  };
  const plan: FrozenProvisioningPlan = {
    version: 1,
    provider: "azure",
    scope: operation.scope,
    resources: [{ cloudID: lease.cloudID, region: "example-region", scope: operation.scope }],
    data: { immutableAllocation: "allocation-one" },
  };
  await storage.put(`lease:${leaseID}`, lease);
  await storage.put(provisioningPlanKey(operation.operationID), plan);
  await storage.transaction((transaction) => putProvisioningOperation(transaction, operation));
  let finishRead!: () => void;
  const delayedRead = new Promise<void>((resolve) => {
    finishRead = resolve;
  });
  let startedRead!: () => void;
  const readStarted = new Promise<void>((resolve) => {
    startedRead = resolve;
  });
  const destructiveRequests: string[] = [];
  let providerReads = 0;
  let finalAuthority: (() => Promise<void>) | undefined;
  const provider: ProviderResumableProvisioning = {
    version: 1,
    supports: () => true,
    prepare: async () => {
      throw new Error("unused preparation");
    },
    advance: async (input) => {
      providerReads += 1;
      finalAuthority = input.assertCleanupOwner;
      startedRead();
      await delayedRead;
      // Model the provider's final asynchronous identity read before dispatch.
      await finalAuthority?.();
      destructiveRequests.push(`DELETE ${(input.step.state as { resourceID: string }).resourceID}`);
      return { ...input.step, phase: "terminal", nextWake: Date.now() };
    },
  };
  const controller = new LeaseProvisioningController(
    new ProvisioningTestRuntime(storage),
    {} as Env,
    () => provider,
    async () => {
      throw new Error("cleanup must not publish");
    },
  );
  const running = controller.advance(leaseID);
  await readStarted;
  return {
    storage,
    operation,
    destructiveRequests,
    providerReads: () => providerReads,
    finalAuthority: () => finalAuthority,
    tick: () => controller.tick(),
    advance: () => controller.advance(leaseID),
    finish: async () => {
      finishRead();
      await running;
    },
  };
}

afterEach(() => vi.useRealTimers());

describe("resumable final cleanup authority", () => {
  it("authorizes the current cleanup claim after the provisioning deadline", async () => {
    const fixture = await cleanupFixture();
    await fixture.finish();
    expect(fixture.finalAuthority()).toBeTypeOf("function");
    expect(fixture.destructiveRequests).toEqual(["DELETE example-service"]);
    expect((await fixture.storage.get<LeaseProvisioningOperation>(operationKey))?.step.phase).toBe(
      "terminal",
    );
  });

  it.each([
    "expired claim",
    "expired claim during authority reads",
    "expired claim during authority commit",
    "missing claim",
    "replaced claim",
    "replaced operation",
    "changed revision",
    "changed generation",
    "changed attempt",
    "changed lease binding",
    "retention requested",
    "changed coordination owner",
    "missing journal",
    "changed journal revision",
    "changed journal resource",
    "missing plan",
    "changed plan resource",
  ])("issues no DELETE after %s during a provider read", async (revocation) => {
    const fixture = await cleanupFixture();
    const { storage } = fixture;
    const current = (await storage.get<LeaseProvisioningOperation>(operationKey))!;
    if (revocation === "expired claim") {
      vi.setSystemTime(current.claim!.expiresAt);
    } else if (revocation === "expired claim during authority reads") {
      storage.beforeGet = async (key) => {
        if (key === provisioningPlanKey(current.operationID)) {
          vi.setSystemTime(current.claim!.expiresAt);
        }
      };
    } else if (revocation === "expired claim during authority commit") {
      storage.beforeGet = async (key) => {
        if (key === "runtime:legacy-alarm") vi.setSystemTime(current.claim!.expiresAt);
      };
    } else if (revocation === "changed lease binding") {
      const lease = (await storage.get<LeaseRecord>(`lease:${leaseID}`))!;
      await storage.put(`lease:${leaseID}`, { ...lease, createAttemptGeneration: "replacement" });
    } else if (revocation === "changed coordination owner") {
      await storage.put(coordinationKey, "replacement-operation");
    } else if (revocation === "missing journal") {
      await storage.delete(provisioningAttemptKey(current));
    } else if (revocation.startsWith("changed journal")) {
      const key = provisioningAttemptKey(current);
      const journal = (await storage.get<Record<string, unknown>>(key))!;
      if (revocation === "changed journal revision") journal["revision"] = 999;
      else journal["state"] = { action: "delete", resourceID: "replacement-service" };
      await storage.put(key, journal);
    } else if (revocation === "missing plan") {
      await storage.delete(provisioningPlanKey(current.operationID));
    } else if (revocation === "changed plan resource") {
      const key = provisioningPlanKey(current.operationID);
      const plan = (await storage.get<FrozenProvisioningPlan>(key))!;
      plan.resources[0]!.cloudID = "replacement-service";
      await storage.put(key, plan);
    } else {
      if (revocation === "missing claim") delete current.claim;
      if (revocation === "replaced claim") current.claim!.id = "replacement-claim";
      if (revocation === "replaced operation") current.operationID = "replacement-operation";
      if (revocation === "changed revision") current.revision += 1;
      if (revocation === "changed generation") current.generation = "replacement-generation";
      if (revocation === "changed attempt") current.step.attempt += 1;
      // Cancellation may change retain without changing the claim or revision.
      if (revocation === "retention requested") current.retain = true;
      await storage.put(operationKey, current);
    }
    await fixture.finish();
    expect(fixture.destructiveRequests).toEqual([]);
  });

  it.each([
    "missing journal",
    "changed journal revision",
    "changed journal resource",
    "missing plan",
    "changed plan resource",
  ])("preserves %s revocation and blocks the next cleanup delivery", async (revocation) => {
    const fixture = await cleanupFixture();
    const { storage } = fixture;
    const current = (await storage.get<LeaseProvisioningOperation>(operationKey))!;
    const isPlan = revocation.includes("plan");
    const key = isPlan ? provisioningPlanKey(current.operationID) : provisioningAttemptKey(current);
    if (revocation.startsWith("missing")) {
      await storage.delete(key);
    } else {
      const evidence = (await storage.get<Record<string, unknown>>(key))!;
      if (revocation === "changed journal revision") evidence["revision"] = 999;
      else if (isPlan)
        evidence["resources"] = [
          { cloudID: "replacement-service", region: "example-region", scope: current.scope },
        ];
      else evidence["state"] = { action: "delete", resourceID: "replacement-service" };
      await storage.put(key, evidence);
    }
    const revokedEvidence = await storage.get(key);
    await fixture.finish();
    vi.setSystemTime(current.claim!.expiresAt + 1);
    await fixture.tick();
    await fixture.advance();

    expect(fixture.destructiveRequests).toEqual([]);
    expect(fixture.providerReads()).toBe(1);
    expect(await storage.get(key)).toEqual(revokedEvidence);
    expect(await storage.get(`provisioning-quarantine:${leaseID}`)).toBeDefined();
  });

  it("does not restore a revoked journal when its quarantine write fails", async () => {
    const fixture = await cleanupFixture();
    const { storage } = fixture;
    const current = (await storage.get<LeaseProvisioningOperation>(operationKey))!;
    const journalKey = provisioningAttemptKey(current);
    await storage.delete(journalKey);
    storage.failKey = `provisioning-quarantine:${leaseID}`;
    await fixture.finish();
    storage.failKey = undefined;
    vi.setSystemTime(current.claim!.expiresAt + 1);
    await fixture.tick();
    await fixture.advance();

    expect(fixture.destructiveRequests).toEqual([]);
    expect(fixture.providerReads()).toBe(1);
    expect(await storage.get(journalKey)).toBeUndefined();
    expect(await storage.get(`provisioning-quarantine:${leaseID}`)).toBeDefined();
  });
});
