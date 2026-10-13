import { afterEach, describe, expect, it, vi } from "vitest";

import { AWSQueryError, EC2SpotClient, type AWSLeaseOperation } from "../src/aws";
import { awsCleanupCompletionBlocker } from "../src/aws-cleanup-completion";
import { completeLeaseProviderCleanup, leaseProviderCleanupConfirmed } from "../src/lease-cleanup";
import {
  leaseCurrentIndex,
  leaseCurrentKey,
  leaseMaintenanceIndex,
  putLeaseRecord,
} from "../src/lease-record-store";
import {
  maintainRetainedLeases,
  observeAWSRetained,
  retainedReconcileDue,
  retainedReconcileSettings,
  retainedReconcileStatus,
  retainedUnresolvedReason,
  RetainedLookupBudget,
  type RetainedObservation,
  type RetainedReconcileHooks,
} from "../src/retained-lease-reconcile";
import type { Env, LeaseRecord, ProviderMachine } from "../src/types";
import { ProvisioningTestStorage } from "./provisioning-fixtures";

const epoch = Date.parse("2026-10-13T12:00:00Z");
function lease(n = 1, patch: Partial<LeaseRecord> = {}): LeaseRecord {
  return {
    id: `cbx_${n.toString(16).padStart(12, "0")}`,
    provider: "aws",
    cloudID: `i-${n.toString(16).padStart(17, "0")}`,
    providerScope: "aws:account:123456789012",
    region: "eu-west-1",
    target: "linux",
    state: "released",
    keep: true,
    releaseDeletesServer: false,
    host: "192.0.2.1",
    serverType: "t3.small",
    providerKey: "shared",
    owner: "alice@example.com",
    org: "example-org",
    createdAt: "2026-10-12T10:00:00Z",
    endedAt: "2026-10-12T12:00:00Z",
    ...patch,
  } as LeaseRecord;
}
function machine(l: LeaseRecord): ProviderMachine {
  return {
    id: 0,
    cloudID: l.cloudID,
    name: l.serverName,
    provider: "aws",
    status: "running",
    serverType: l.serverType,
    host: l.host,
    labels: {
      crabbox: "true",
      created_by: "crabbox",
      lease: l.id,
      provider: "aws",
      slug: l.slug!,
      owner: l.owner.replace("@", "_"),
    },
  };
}
async function fixture(records = [lease()]) {
  const storage = new ProvisioningTestStorage();
  for (const index of [leaseCurrentIndex, leaseMaintenanceIndex])
    storage.values.set(index.stateKey, { version: 1, complete: true });
  for (const row of records) {
    // oxlint-disable-next-line eslint/no-await-in-loop -- initialize each indexed fixture before observing it.
    await putLeaseRecord(storage, row);
  }
  let time = epoch;
  let observation: RetainedObservation = {
    outcome: "absent",
    credentialFingerprint: "credential-one",
  };
  const hooks: RetainedReconcileHooks = {
    exclusive: async (work) => work(),
    ownsProvisioning: async () => false,
    deleteLease: vi.fn<RetainedReconcileHooks["deleteLease"]>(async (_lease, guard) => {
      await guard();
    }),
    now: () => time,
    observe: vi.fn<typeof observeAWSRetained>(
      async (_env, rows) => new Map(rows.map((row) => [row.id, observation])),
    ),
  };
  return {
    storage,
    hooks,
    setTime: (next: number) => {
      time = next;
    },
    setObservation: (next: RetainedObservation) => {
      observation = next;
    },
    read: async (id = records[0]!.id) => (await storage.get<LeaseRecord>(`lease:${id}`))!,
    run: (env: Partial<Env> = {}) => maintainRetainedLeases(storage, env as Env, hooks),
  };
}
afterEach(() => vi.restoreAllMocks());
describe("retained lease reconciliation", () => {
  it.each([
    [undefined, 120],
    ["60", 60],
    ["86400", 86400],
    ["59", 120],
    ["86401", 120],
    ["1.5", 120],
    ["120seconds", 120],
    ["", 120],
    ["9007199254740992", 120],
  ])("parses interval %s as %s seconds", (value, expected) => {
    expect(
      retainedReconcileSettings(
        (value === undefined ? {} : { CRABBOX_RETAINED_RECONCILE_INTERVAL_SECONDS: value }) as Env,
      ).intervalSeconds,
    ).toBe(expected);
  });
  it("skips index and AWS reads until the persisted pass is due, including after restart", async () => {
    const f = await fixture();
    const env = {} as Env;
    expect(await retainedReconcileDue(f.storage, env, epoch)).toBe(true);
    await f.run();
    expect((await retainedReconcileStatus(f.storage, env)).lastPassAt).toBe(
      new Date(epoch).toISOString(),
    );
    f.storage.listOptions.length = 0;
    vi.mocked(f.hooks.observe!).mockClear();
    const reads: string[] = [];
    f.storage.beforeGet = async (key) => {
      reads.push(key);
    };
    const restartStorage = new ProvisioningTestStorage();
    restartStorage.values = structuredClone(f.storage.values);
    expect(await retainedReconcileDue(restartStorage, env, epoch + 119_999)).toBe(false);
    if (await retainedReconcileDue(f.storage, env, epoch + 119_999)) await f.run();
    expect(f.storage.listOptions).toEqual([]);
    expect(reads).toEqual(["retained-reconcile-state"]);
    expect(f.hooks.observe).not.toHaveBeenCalled();
    f.setTime(epoch + 120_000);
    expect(await retainedReconcileDue(f.storage, env, epoch + 120_000)).toBe(true);
    await f.run();
    expect(f.hooks.observe).toHaveBeenCalledOnce();
    expect((await retainedReconcileStatus(f.storage, env)).lastPassAt).toBe(
      new Date(epoch + 120_000).toISOString(),
    );
  });
  it("uses the configured interval and ignores invalid persisted times", async () => {
    const f = await fixture();
    await f.run();
    const env = { CRABBOX_RETAINED_RECONCILE_INTERVAL_SECONDS: "60" } as Env;
    expect(await retainedReconcileDue(f.storage, env, epoch + 59_999)).toBe(false);
    expect(await retainedReconcileDue(f.storage, env, epoch + 60_000)).toBe(true);
    expect(
      await retainedReconcileDue(
        f.storage,
        { CRABBOX_RETAINED_RECONCILE: "off" } as Env,
        epoch + 120_000,
      ),
    ).toBe(false);
    const state = await retainedReconcileStatus(f.storage, env);
    await f.storage.put("retained-reconcile-state", { ...state, lastPassAt: "invalid" });
    expect(await retainedReconcileDue(f.storage, env, epoch)).toBe(true);
  });
  it("lets confirmation transaction failures escape without publishing partial cleanup", async () => {
    const f = await fixture();
    await f.run();
    const original = await f.read();
    f.setTime(epoch + 600_000);
    f.storage.failKey = leaseCurrentKey(original.id);
    await expect(f.run()).rejects.toThrow("injected storage failure");
    expect(await f.read()).toEqual(original);
    expect(await f.storage.get(leaseCurrentKey(original.id))).toBeDefined();
    expect((await retainedReconcileStatus(f.storage, {} as Env)).lastPassAt).toBe(
      new Date(epoch).toISOString(),
    );
  });
  it("reads only bounded current-index pages and resumes after restart", async () => {
    const f = await fixture(Array.from({ length: 5 }, (_, n) => lease(n + 1)));
    f.storage.values.set(
      "lease:historical",
      lease(99, { keep: false, releaseDeletesServer: true }),
    );
    await f.run({ CRABBOX_RETAINED_RECONCILE_BUDGET: "2" });
    expect((await retainedReconcileStatus(f.storage, {} as Env)).scanned).toBe(2);
    await f.run({ CRABBOX_RETAINED_RECONCILE_BUDGET: "2" });
    expect((await retainedReconcileStatus(f.storage, {} as Env)).scanned).toBe(4);
    await f.run({ CRABBOX_RETAINED_RECONCILE_BUDGET: "2" });
    expect((await retainedReconcileStatus(f.storage, {} as Env)).cycles).toBe(1);
    expect(
      f.storage.listOptions.every(
        (o) => o?.prefix === "lease-current:" && o.limit === 2 && o.allowConcurrency,
      ),
    ).toBe(true);
    expect(f.hooks.observe).toHaveBeenNthCalledWith(
      2,
      expect.anything(),
      [expect.objectContaining({ id: lease(3).id }), expect.objectContaining({ id: lease(4).id })],
      expect.anything(),
    );
  });
  it("requires two observations at least ten minutes apart and publishes the ordinary scrubbed completion fact", async () => {
    const f = await fixture();
    await f.run();
    expect(leaseProviderCleanupConfirmed(await f.read())).toBe(false);
    f.setTime(epoch + 599_999);
    await f.run();
    expect((await f.read()).cleanupCompletedAt).toBeUndefined();
    f.setTime(epoch + 600_000);
    await f.run();
    const completed = await f.read();
    expect(leaseProviderCleanupConfirmed(completed)).toBe(true);
    expect(completed.cleanupConfirmedBy).toBe("retained-reconcile");
    const ordinary = lease();
    delete ordinary.releaseDeletesServer;
    completeLeaseProviderCleanup(ordinary, completed.cleanupCompletedAt!);
    for (const key of [
      "host",
      "tailscale",
      "sshHostKey",
      "providerAccessExpiresAt",
      "cleanupCompletedAt",
      "provisioningResourceMayExist",
    ] as const)
      expect(completed[key]).toEqual(ordinary[key]);
    expect(await f.storage.get(leaseCurrentKey(completed.id))).toBeUndefined();
    expect(completed.retainedReconcileObservation).toMatchObject({
      firstAbsentAt: new Date(epoch).toISOString(),
      lastAbsentAt: new Date(epoch + 600_000).toISOString(),
    });
  });
  it.each(["report", "off"])("does not mutate leases in %s mode", async (mode) => {
    const f = await fixture();
    const original = await f.read();
    await f.run({ CRABBOX_RETAINED_RECONCILE: mode });
    expect(await f.read()).toEqual(original);
  });
  it("reports present machines by default and resets earlier absence evidence", async () => {
    const f = await fixture();
    await f.run();
    f.setObservation({
      outcome: "present",
      credentialFingerprint: "one",
      server: machine(lease()),
    });
    await f.run();
    expect(f.hooks.deleteLease).not.toHaveBeenCalled();
    expect((await f.read()).retainedReconcileObservation).toBeUndefined();
    expect((await retainedReconcileStatus(f.storage, {} as Env)).counters.aws?.present).toBe(1);
  });
  it.each(["scope_mismatch", "credential_changed", "lookup_error", "throttled"])(
    "does not mutate on %s and backs off",
    async (outcome) => {
      const f = await fixture();
      const original = await f.read();
      f.setObservation({ outcome, retryAfter: "1800" });
      await f.run();
      expect(await f.read()).toEqual(original);
      const state = await retainedReconcileStatus(f.storage, {} as Env);
      expect(state.backoff.aws!.until).toBeGreaterThanOrEqual(epoch + 1800_000);
      await f.run();
      expect(f.hooks.observe).toHaveBeenCalledTimes(1);
    },
  );
  it("rejects identity changes between observation and publication", async () => {
    const f = await fixture();
    f.hooks.observe = async () => {
      const changed = await f.read();
      changed.cloudID = lease(2).cloudID;
      await putLeaseRecord(f.storage, changed);
      return new Map([[lease().id, { outcome: "absent", credentialFingerprint: "one" }]]);
    };
    await f.run();
    expect((await f.read()).retainedReconcileObservation).toBeUndefined();
  });
  it("does not carry absence across a credential fingerprint change", async () => {
    const f = await fixture();
    await f.run();
    const original = await f.read();
    f.setTime(epoch + 600_000);
    f.setObservation({ outcome: "absent", credentialFingerprint: "changed" });
    await f.run();
    expect(await f.read()).toEqual(original);
  });
  it.each([
    { patch: { cloudID: "" }, reason: "missing_identity" },
    { patch: { providerScope: undefined }, reason: "missing_original_scope" },
    { patch: { provider: "azure" }, reason: "no_scoped_exact_read" },
    { patch: { provider: "hetzner", providerScope: undefined }, reason: "missing_original_scope" },
    { patch: { providerCleanup: {} } as Partial<LeaseRecord>, reason: "unresolved_action" },
    { patch: { target: "macos" } as Partial<LeaseRecord>, reason: "dedicated_host" },
  ])("leaves $reason records untouched", async ({ patch, reason }) => {
    const row = lease(1, patch);
    const f = await fixture([row]);
    await f.run();
    expect(await f.read()).toEqual(row);
    expect(f.hooks.observe).not.toHaveBeenCalled();
    expect(retainedUnresolvedReason(row)).toBe(reason);
  });
  it("waits for the current index rather than walking history", async () => {
    const f = await fixture();
    f.storage.values.delete(leaseCurrentIndex.stateKey);
    await f.run();
    expect(f.storage.listOptions).toEqual([]);
  });
  it("does not skip budget-exhausted candidates", async () => {
    const f = await fixture([lease(1), lease(2)]);
    f.hooks.observe = async () =>
      new Map([
        [lease(1).id, { outcome: "absent", credentialFingerprint: "one" }],
        [lease(2).id, { outcome: "lookup_budget" }],
      ]);
    await f.run();
    expect((await retainedReconcileStatus(f.storage, {} as Env)).cursor).toBe(
      leaseCurrentKey(lease(1).id),
    );
  });
  it.each(["flag", "grace", "ownership", "active"])(
    "blocks deletion without %s proof",
    async (barrier) => {
      const row = lease(1, { slug: "test", serverName: "test" });
      const f = await fixture([row]);
      const server = machine(row);
      if (barrier === "ownership") server.labels!.lease = "different";
      f.setObservation({ outcome: "present", credentialFingerprint: "one", server });
      if (barrier === "grace") f.setTime(Date.parse(row.endedAt!) + 10_000);
      if (barrier === "active")
        await putLeaseRecord(f.storage, lease(2, { state: "active", cloudID: row.cloudID }));
      await f.run({ CRABBOX_RETAINED_RECONCILE_DELETE: barrier === "flag" ? "0" : "1" });
      expect(f.hooks.deleteLease).not.toHaveBeenCalled();
    },
  );
  it("uses owned deletion then requires fresh double absence before completion", async () => {
    const row = lease(1, { slug: "test", serverName: "test" });
    const f = await fixture([row]);
    f.setObservation({ outcome: "present", credentialFingerprint: "one", server: machine(row) });
    await f.run({ CRABBOX_RETAINED_RECONCILE_DELETE: "1" });
    expect(f.hooks.deleteLease).toHaveBeenCalledTimes(1);
    expect((await f.read()).cleanupCompletedAt).toBeUndefined();
    f.setObservation({ outcome: "absent", credentialFingerprint: "one" });
    await f.run();
    f.setTime(epoch + 600_000);
    await f.run();
    expect(leaseProviderCleanupConfirmed(await f.read())).toBe(true);
  });
  it("recovers a crashed delete claim by observation", async () => {
    const f = await fixture([
      lease(1, {
        retainedReconcileDeleteClaim: "old",
        cleanupStartedAt: "old",
        cleanupClaimExpiresAt: new Date(epoch - 1).toISOString(),
      }),
    ]);
    await f.run();
    expect((await f.read()).retainedReconcileDeleteClaim).toBe("awaiting-confirmation");
    expect((await f.read()).retainedReconcileObservation).toBeDefined();
  });
  it("parses bounded settings and fails closed on unknown modes", () => {
    expect(retainedReconcileSettings({} as Env)).toMatchObject({
      mode: "on",
      delete: false,
      graceSeconds: 3600,
      budget: 200,
      lookupBudget: 64,
    });
    expect(
      retainedReconcileSettings({
        CRABBOX_RETAINED_RECONCILE: "invalid",
        CRABBOX_RETAINED_RECONCILE_BUDGET: "201",
        CRABBOX_RETAINED_RECONCILE_DELETE: "true",
      } as Env),
    ).toMatchObject({ mode: "off", budget: 200, delete: false });
  });
});

function mockSession(patch: Partial<AWSLeaseOperation> = {}): AWSLeaseOperation {
  return {
    region: "eu-west-1",
    verifiedIdentity: async () => ({ account: "123456789012" }),
    credentialFingerprint: async () => "one",
    findServers: vi.fn<AWSLeaseOperation["findServers"]>(async () => new Map()),
    findServer: vi.fn<AWSLeaseOperation["findServer"]>(async () => undefined),
    sshKeyAbsent: async () => true,
    confirmRetainedVolumeDeleted: async () => {},
    ...patch,
  } as unknown as AWSLeaseOperation;
}
function useSession(session: AWSLeaseOperation) {
  vi.spyOn(EC2SpotClient.prototype, "withLeaseOperation").mockImplementation(async (work) =>
    work(session),
  );
}
describe("AWS retained exact observation", () => {
  it("batches exact IDs and confirms terminated, never shutting-down", async () => {
    const row = lease(1, { slug: "test" });
    const server = { ...machine(row), status: "terminated" };
    const session = mockSession({
      findServers: vi.fn<AWSLeaseOperation["findServers"]>(
        async () => new Map([[row.cloudID, server]]),
      ),
    });
    useSession(session);
    expect(
      (
        await observeAWSRetained(
          { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
          [row],
          new RetainedLookupBudget(10),
        )
      ).get(row.id)?.outcome,
    ).toBe("absent");
    expect(session.findServers).toHaveBeenCalledWith([row.cloudID]);
    server.status = "shutting-down";
    expect(
      (
        await observeAWSRetained(
          { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
          [row],
          new RetainedLookupBudget(10),
        )
      ).get(row.id)?.outcome,
    ).toBe("present");
  });
  it("uses bounded individual exact reads only on the batch NotFound code", async () => {
    const session = mockSession({
      findServers: async () => {
        throw new AWSQueryError("DescribeInstances", 400, "InvalidInstanceID.NotFound", "missing");
      },
    });
    useSession(session);
    const rows = [lease(1), lease(2), lease(3)];
    const result = await observeAWSRetained(
      { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
      rows,
      new RetainedLookupBudget(4),
    );
    expect(session.findServer).toHaveBeenCalledTimes(2);
    expect(result.get(lease(3).id)?.outcome).toBe("lookup_budget");
  });
  it("requires original account and identical credential context", async () => {
    const session = mockSession();
    useSession(session);
    expect(
      (
        await observeAWSRetained(
          { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
          [lease(1, { providerScope: "aws:account:999999999999" })],
          new RetainedLookupBudget(10),
        )
      ).get(lease().id)?.outcome,
    ).toBe("scope_mismatch");
    expect(session.findServers).not.toHaveBeenCalled();
  });
  it("preserves the provider's companion obligations", async () => {
    const row = lease(1, { providerKeyCleanupOwned: true, providerKey: `crabbox-${lease().id}` });
    expect(
      awsCleanupCompletionBlocker(row, {
        instanceComplete: false,
        keyComplete: true,
        volumeComplete: true,
      }),
    ).toBe("instance_present");
    const session = mockSession({ sshKeyAbsent: async () => false });
    useSession(session);
    // Use the canonical name from the same helper as ordinary release.
    const { providerKeyForLease } = await import("../src/provider-key");
    row.providerKey = providerKeyForLease(row.id);
    expect(
      (
        await observeAWSRetained(
          { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
          [row],
          new RetainedLookupBudget(10),
        )
      ).get(row.id)?.outcome,
    ).toBe("owned_key_present");
  });
  it("propagates throttling and Retry-After without partial evidence", async () => {
    const error = new AWSQueryError("DescribeInstances", 429, "Throttling", "rate limit");
    error.retryAfter = "1800";
    useSession(
      mockSession({
        findServers: async () => {
          throw error;
        },
      }),
    );
    expect(
      (
        await observeAWSRetained(
          { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env,
          [lease()],
          new RetainedLookupBudget(10),
        )
      ).get(lease().id),
    ).toEqual({ outcome: "throttled", retryAfter: "1800" });
  });
});

describe("AWS exact-ID batch transport", () => {
  afterEach(() => vi.unstubAllGlobals());
  function transport(payload: string) {
    const calls: URLSearchParams[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const params = new URLSearchParams(await request.text());
        calls.push(params);
        if (params.get("Action") === "GetCallerIdentity")
          return new Response(
            "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>test</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>",
          );
        return new Response(payload);
      }),
    );
    return calls;
  }
  const env = { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" } as Env;
  it("sends only exact instance IDs in the recorded region and accepts an empty exact response", async () => {
    const calls = transport(
      "<DescribeInstancesResponse><requestId>test</requestId><reservationSet/></DescribeInstancesResponse>",
    );
    const rows = [lease(1), lease(2)];
    const result = await observeAWSRetained(env, rows, new RetainedLookupBudget(10));
    expect([...result.values()].map((row) => row.outcome)).toEqual(["absent", "absent"]);
    const exact = calls[1]!;
    expect(exact.get("InstanceId.1")).toBe(rows[0]!.cloudID);
    expect(exact.get("InstanceId.2")).toBe(rows[1]!.cloudID);
    expect([...exact.keys()].some((key) => key.startsWith("Filter"))).toBe(false);
  });
  it.each(["999999999999", "123456789012"])(
    "rejects unexpected or duplicate results (account %s)",
    async (account) => {
      transport(
        `<DescribeInstancesResponse><requestId>test</requestId><reservationSet><item><ownerId>${account}</ownerId><instancesSet><item><instanceId>i-fffffffffffffffff</instanceId><instanceState><name>terminated</name></instanceState></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`,
      );
      expect(
        (await observeAWSRetained(env, [lease()], new RetainedLookupBudget(10))).get(lease().id)
          ?.outcome,
      ).toBe("lookup_error");
    },
  );
});

describe("retained reconciliation recovery guards", () => {
  it("reserves enough request budget to observe companion keys and advance the cursor", async () => {
    const { providerKeyForLease } = await import("../src/provider-key");
    const rows = Array.from({ length: 100 }, (_, n) => {
      const row = lease(n + 1, { providerKeyCleanupOwned: true });
      row.providerKey = providerKeyForLease(row.id);
      return row;
    });
    const f = await fixture(rows);
    const session = mockSession({
      findServers: async () => {
        throw new AWSQueryError("DescribeInstances", 400, "InvalidInstanceID.NotFound", "missing");
      },
    });
    useSession(session);
    delete f.hooks.observe;
    await f.run({ AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "secret" });
    expect((await f.read(rows[0]!.id)).retainedReconcileObservation).toBeDefined();
    expect((await retainedReconcileStatus(f.storage, {} as Env)).cursor).toBe(
      leaseCurrentKey(rows[30]!.id),
    );
  });
  it("rechecks live bindings after claiming deletion and immediately before mutation", async () => {
    const row = lease(1, { slug: "test", serverName: "test" });
    const f = await fixture([row]);
    f.setObservation({ outcome: "present", credentialFingerprint: "one", server: machine(row) });
    let mutated = false;
    f.hooks.deleteLease = async (_claimed, guard) => {
      await putLeaseRecord(f.storage, lease(2, { state: "active", cloudID: row.cloudID }));
      await guard();
      mutated = true;
    };
    await f.run({ CRABBOX_RETAINED_RECONCILE_DELETE: "1" });
    expect(mutated).toBe(false);
    expect(
      (await retainedReconcileStatus(f.storage, {} as Env)).counters["aws"]?.["delete_failed"],
    ).toBe(1);
  });
  it("does not recover or mutate an expired delete claim in report mode", async () => {
    const row = lease(1, {
      retainedReconcileDeleteClaim: "old",
      cleanupStartedAt: "old",
      cleanupClaimExpiresAt: new Date(epoch - 1).toISOString(),
    });
    const f = await fixture([row]);
    await f.run({ CRABBOX_RETAINED_RECONCILE: "report", CRABBOX_RETAINED_RECONCILE_DELETE: "1" });
    expect(await f.read()).toEqual(row);
  });
});

describe("retained reconciliation review regressions", () => {
  it("allows at most one deletion per pass and passes a bounded provider signal", async () => {
    const rows = [lease(1, { slug: "one" }), lease(2, { slug: "two" })];
    const f = await fixture(rows);
    f.hooks.observe = async (_env, records) =>
      new Map(
        records.map((row) => [
          row.id,
          { outcome: "present", credentialFingerprint: "one", server: machine(row) },
        ]),
      );
    await f.run({ CRABBOX_RETAINED_RECONCILE_DELETE: "1" });
    expect(f.hooks.deleteLease).toHaveBeenCalledTimes(1);
    expect(vi.mocked(f.hooks.deleteLease).mock.calls[0]?.[2]).toBeInstanceOf(AbortSignal);
  });
  it("retains deletion Retry-After even after a successful present observation", async () => {
    const row = lease(1, { slug: "one" });
    const f = await fixture([row]);
    f.setObservation({ outcome: "present", credentialFingerprint: "one", server: machine(row) });
    f.hooks.deleteLease = async () => {
      const error = new AWSQueryError("TerminateInstances", 429, "Throttling", "limited");
      error.retryAfter = "1800";
      throw error;
    };
    await f.run({ CRABBOX_RETAINED_RECONCILE_DELETE: "1" });
    expect((await retainedReconcileStatus(f.storage, {} as Env)).backoff["aws"]?.until).toBe(
      epoch + 1800_000,
    );
  });
  it("rejects too-small lookup settings and advances a full-companion record at the minimum", async () => {
    expect(
      retainedReconcileSettings({ CRABBOX_RETAINED_RECONCILE_LOOKUP_BUDGET: "4" } as Env)
        .lookupBudget,
    ).toBe(64);
    const { providerKeyForLease } = await import("../src/provider-key");
    const row = lease(1, {
      providerKeyCleanupOwned: true,
      lifetimePolicy: {
        retainedIdentity: `${lease().cloudID}:vol-1234:/dev/sda1`,
      } as LeaseRecord["lifetimePolicy"],
    });
    row.providerKey = providerKeyForLease(row.id);
    const f = await fixture([row]);
    useSession(
      mockSession({
        findServers: async () => {
          throw new AWSQueryError(
            "DescribeInstances",
            400,
            "InvalidInstanceID.NotFound",
            "missing",
          );
        },
      }),
    );
    delete f.hooks.observe;
    await f.run({
      AWS_ACCESS_KEY_ID: "test",
      AWS_SECRET_ACCESS_KEY: "secret",
      CRABBOX_RETAINED_RECONCILE_LOOKUP_BUDGET: "5",
    });
    expect((await f.read()).retainedReconcileObservation).toBeDefined();
  });
  it("leaves an expired deletion claim unchanged when the lookup fails", async () => {
    const row = lease(1, {
      retainedReconcileDeleteClaim: "old",
      cleanupStartedAt: "old",
      cleanupClaimExpiresAt: new Date(epoch - 1).toISOString(),
    });
    const f = await fixture([row]);
    f.setObservation({ outcome: "lookup_error" });
    await f.run();
    expect(await f.read()).toEqual(row);
  });
});

describe("retained reconciliation conservative identity guards", () => {
  afterEach(() => vi.unstubAllGlobals());
  it("forwards an explicit deadline from a fixed-credential session", async () => {
    const controller = new AbortController();
    let sawAbort = false;
    const fetch = vi.fn<typeof globalThis.fetch>(async (input, init) => {
      const request = input instanceof Request ? input : new Request(input, init);
      controller.abort();
      sawAbort ||= request.signal.aborted;
      request.signal.throwIfAborted();
      return new Response(
        "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>",
      );
    });
    vi.stubGlobal("fetch", fetch);
    const client = new EC2SpotClient({} as Env, "eu-west-1", {
      accessKeyId: "test",
      secretAccessKey: "secret",
    });
    await expect(
      client.withLeaseOperation(
        (session) => session.findServers([lease().cloudID]),
        controller.signal,
      ),
    ).rejects.toThrow(/abort/i);
    expect(sawAbort).toBe(true);
  });
  it("blocks deletion when an active binding has lost original scope fields", async () => {
    const row = lease(1, { slug: "one" });
    const f = await fixture([row]);
    await putLeaseRecord(
      f.storage,
      lease(2, {
        state: "active",
        cloudID: row.cloudID,
        providerScope: undefined,
        region: undefined,
      }),
    );
    f.setObservation({ outcome: "present", credentialFingerprint: "one", server: machine(row) });
    await f.run({ CRABBOX_RETAINED_RECONCILE_DELETE: "1" });
    expect(f.hooks.deleteLease).not.toHaveBeenCalled();
  });
});

describe("retained observation timing", () => {
  it("does not subtract first-pass provider latency from the ten-minute interval", async () => {
    const f = await fixture();
    let first = true;
    f.hooks.observe = async (_env, rows) => {
      if (first) {
        f.setTime(epoch + 20_000);
        first = false;
      }
      return new Map(
        rows.map((row) => [row.id, { outcome: "absent", credentialFingerprint: "one" }]),
      );
    };
    await f.run();
    expect((await f.read()).retainedReconcileObservation?.firstAbsentAt).toBe(
      new Date(epoch + 20_000).toISOString(),
    );
    f.setTime(epoch + 600_000);
    await f.run();
    expect((await f.read()).cleanupCompletedAt).toBeUndefined();
    f.setTime(epoch + 620_000);
    await f.run();
    expect((await f.read()).cleanupCompletedAt).toBe(new Date(epoch + 620_000).toISOString());
  });
});

describe("retained backoff timing", () => {
  it("starts Retry-After at the failed observation rather than the pass start", async () => {
    const f = await fixture();
    f.hooks.observe = async (_env, rows) => {
      f.setTime(epoch + 20_000);
      return new Map(rows.map((row) => [row.id, { outcome: "throttled", retryAfter: "1800" }]));
    };
    await f.run();
    expect((await retainedReconcileStatus(f.storage, {} as Env)).backoff["aws"]?.until).toBe(
      epoch + 1_820_000,
    );
  });
});
