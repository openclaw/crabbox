import { describe, expect, it, vi } from "vitest";

import { orgKeyForLabel } from "../src/org-identity";
import { claimReadyPoolAllocation, ReadyPoolClaimError } from "../src/ready-pool-claims";
import type { LeaseRecord, ReadyPoolEntry, ReadyPoolIdentityV1 } from "../src/types";
import { ProvisioningTestStorage } from "./provisioning-fixtures";

const identity: ReadyPoolIdentityV1 = {
  schema: "crabbox-ready-pool-identity/v1",
  image: {
    provider: "koyeb",
    scope: "owned-app-region-shape-bootstrap",
    id: `registry.invalid/runner@sha256:${"a".repeat(64)}`,
  },
  architecture: "amd64",
  seedDigest: `sha256:${"b".repeat(64)}`,
  cacheCompatibility: "clean-v1",
};
const principal = { owner: "alice@example.com", org: orgKeyForLabel("example-org") };
const poolKey = "clean";

async function fixture() {
  const storage = new ProvisioningTestStorage();
  let now = Date.parse("2026-09-13T12:00:00Z");
  const lease = {
    id: "cbx_ready",
    ...principal,
    provider: "koyeb",
    state: "active",
    createAttemptGeneration: "generation-1",
    cloudID: "owned-service",
    expiresAt: new Date(now + 3600_000).toISOString(),
  } as LeaseRecord;
  const entry = {
    key: poolKey,
    leaseID: lease.id,
    ...principal,
    state: "ready",
    singleUse: true,
    identity,
    createdAt: new Date(now).toISOString(),
    updatedAt: new Date(now).toISOString(),
    expiresAt: lease.expiresAt,
  } as ReadyPoolEntry;
  await storage.put(`lease:${lease.id}`, lease);
  const entryKey = `typed-ready-pool-v1:${poolKey}:${lease.id}`;
  await storage.put(entryKey, entry);
  const claim = vi.fn<(lease: LeaseRecord, token: string) => Promise<void>>(async () => {});
  const request = (operationID: string, extra = {}) => ({
    storage,
    poolKey,
    principal,
    matches: () => true,
    claim,
    now: () => now,
    input: { operationID, coldLeaseID: `cbx_cold_${operationID}`, identity, ...extra },
  });
  return {
    storage,
    lease,
    entryKey,
    claim,
    request,
    advance: (ms: number) => {
      now += ms;
    },
  };
}

describe("single-use typed ready-pool claims", () => {
  it("commits one warm choice before the provider outcome and records a visible cold miss", async () => {
    const f = await fixture();
    const selections = await Promise.all(
      ["one", "two"].map((id) => claimReadyPoolAllocation(f.request(id, { selectOnly: true }))),
    );
    expect(selections.map((x) => x.allocation.kind).toSorted()).toEqual(["cold", "warm"]);
    expect(f.claim).not.toHaveBeenCalled();
    const warm = selections.find((x) => x.allocation.kind === "warm")!.allocation;
    expect(warm.phase).toBe("selected");
    const ready = await claimReadyPoolAllocation(f.request(warm.operationID));
    expect(ready.allocation).toMatchObject({
      kind: "warm",
      phase: "ready",
      selectedAt: warm.selectedAt,
      leaseGeneration: "generation-1",
    });
    expect(f.claim).toHaveBeenCalledTimes(1);
    expect(f.claim.mock.calls[0]![1]).toMatch(/^[a-f0-9]{64}$/);
    expect(JSON.stringify(ready.allocation)).not.toContain("claimToken");
    await claimReadyPoolAllocation(f.request(warm.operationID));
    expect(f.claim).toHaveBeenCalledTimes(1);
  });

  it("lookup-only cleanup never consumes inventory", async () => {
    const f = await fixture();
    const result = await claimReadyPoolAllocation(f.request("cleanup", { lookupOnly: true }));
    expect(result.allocation.reasonCode).toBe("pool-unselected");
    expect(await f.storage.get(f.entryKey)).toMatchObject({ state: "ready" });
    expect(f.claim).not.toHaveBeenCalled();
    expect(
      [...f.storage.values.keys()].filter((key) => key.startsWith("ready-pool-allocation:")),
    ).toEqual([]);
  });

  it("serializes duplicate requests while unrelated selections progress during a blocked provider call", async () => {
    const f = await fixture();
    let finish!: () => void;
    let started!: () => void;
    const startedPromise = new Promise<void>((resolve) => {
      started = resolve;
    });
    f.claim.mockImplementation(async () => {
      started();
      await new Promise<void>((resolve) => {
        finish = resolve;
      });
    });
    const first = claimReadyPoolAllocation(f.request("first"));
    await startedPromise;
    await expect(claimReadyPoolAllocation(f.request("first"))).rejects.toMatchObject({
      code: "pool_claim_pending",
    });
    const independent = await claimReadyPoolAllocation(f.request("second"));
    expect(independent.allocation).toMatchObject({ kind: "cold", reasonCode: "pool-miss" });
    finish();
    await first;
    expect(f.claim).toHaveBeenCalledTimes(1);
  });

  it("quarantines ambiguous warm failures and retains their classification after restart", async () => {
    const f = await fixture();
    f.claim.mockRejectedValue(new Error("synthetic private provider diagnostic"));
    await expect(claimReadyPoolAllocation(f.request("failure"))).rejects.toMatchObject({
      allocation: { kind: "warm", phase: "failed" },
    });
    expect(await f.storage.get(f.entryKey)).toMatchObject({ state: "quarantined" });
    const lookup = await claimReadyPoolAllocation(f.request("failure", { lookupOnly: true }));
    expect(lookup.allocation).toMatchObject({ kind: "warm", reasonCode: "pool-claim-failed" });
    expect(JSON.stringify(lookup)).not.toContain("private provider diagnostic");
    expect((await claimReadyPoolAllocation(f.request("fresh"))).allocation.kind).toBe("cold");
  });

  it("fences changed lease generations and expired selections", async () => {
    const f = await fixture();
    f.claim.mockImplementation(async () => {
      await f.storage.put(`lease:${f.lease.id}`, {
        ...f.lease,
        createAttemptGeneration: "replacement",
      });
    });
    await expect(claimReadyPoolAllocation(f.request("generation"))).rejects.toBeInstanceOf(
      ReadyPoolClaimError,
    );
    expect(await f.storage.get(f.entryKey)).toMatchObject({ state: "quarantined" });
    const expiry = await fixture();
    await claimReadyPoolAllocation(expiry.request("expiry", { selectOnly: true }));
    expiry.advance(120_001);
    await expect(claimReadyPoolAllocation(expiry.request("expiry"))).rejects.toMatchObject({
      allocation: { kind: "warm", phase: "failed", reasonCode: "pool-claim-fenced" },
    });
    expect(expiry.claim).not.toHaveBeenCalled();
  });

  it("drains incompatible images and cannot use inventory from another owner", async () => {
    const f = await fixture();
    expect(
      (await claimReadyPoolAllocation({ ...f.request("drift"), matches: () => false })).allocation
        .kind,
    ).toBe("cold");
    expect(await f.storage.get(f.entryKey)).toMatchObject({ state: "draining" });
    const other = await fixture();
    expect(
      (
        await claimReadyPoolAllocation({
          ...other.request("other"),
          principal: { ...principal, owner: "bob@example.com" },
        })
      ).allocation.kind,
    ).toBe("cold");
    expect(other.claim).not.toHaveBeenCalled();
  });

  it("rolls back partial reservation writes and never loses the consumed fence to entry GC", async () => {
    const f = await fixture();
    f.storage.failKey = f.entryKey;
    await expect(claimReadyPoolAllocation(f.request("storage"))).rejects.toThrow(
      "injected storage failure",
    );
    expect(await f.storage.get(`lease:${f.lease.id}`)).not.toHaveProperty("readyPoolConsumedAt");
    expect(f.claim).not.toHaveBeenCalled();
    f.storage.failKey = undefined;
    await claimReadyPoolAllocation(f.request("storage"));
    await f.storage.delete(f.entryKey);
    expect(await f.storage.get(`lease:${f.lease.id}`)).toHaveProperty("readyPoolConsumedAt");
    await expect(claimReadyPoolAllocation(f.request("storage"))).rejects.toMatchObject({
      code: "pool-claim-fenced",
    });
  });
});
