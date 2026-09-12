import { afterEach, expect, it, vi } from "vitest";

import { NodeCoordinatorRuntime } from "../node/node-runtime";
import { AsyncMutex } from "../node/server-support";
import type { AWSLegacyCleanupAudit } from "../src/aws-cleanup-recovery";
import type { CoordinatorStorageView } from "../src/coordinator-runtime";
import { AWSProvider, FleetCoordinator } from "../src/fleet";
import { orgKeyForLabel } from "../src/org-identity";
import { providerKeyForLease } from "../src/provider-key";
import type { Env, LeaseRecord } from "../src/types";

// Deliberately ignore DATABASE_URL: this proof may only connect to its fixed CI fixture.
const database = new URL("postgresql://127.0.0.1:55432/crabbox_recovery_test");
database.username = "crabbox_test";
database.password = "crabbox-test-only";
const scope = "aws:account:123456789012";
const region = "eu-west-1";
const headers = {
  "content-type": "application/json",
  "x-crabbox-owner": "alice@example.com",
  "x-crabbox-org": "example-org",
  "x-crabbox-admin": "true",
};

const env = {
  CRABBOX_DEFAULT_ORG: "example-org",
  CRABBOX_AWS_REGION: region,
  CRABBOX_AWS_ORPHAN_SWEEP_ENABLED: "0",
  AWS_ACCESS_KEY_ID: "synthetic-key",
  AWS_SECRET_ACCESS_KEY: "synthetic-secret",
} as Env;

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

it("persists atomic AWS scope recovery across real PostgreSQL reopen beside a scoped lease (synthetic AWS)", async () => {
  const legacy: LeaseRecord = {
    ...lease("cbx_000000000001", "i-00000000000000001", "legacy-runner"),
    state: "released",
    releaseDeletesServer: true,
    releasedAt: new Date(Date.now() - 30_000).toISOString(),
    expiresAt: new Date(Date.now() - 30_000).toISOString(),
    cleanupError: "original AWS account scope was not persisted",
    failureError: "original AWS account scope was not persisted",
    provisioningResourceMayExist: true,
    provisioningFailureRetryable: false,
  };
  const ordinary: LeaseRecord = {
    ...lease("cbx_000000000002", "i-00000000000000002", "scoped-runner"),
    providerScope: scope,
  };
  const aws = syntheticAWS(legacy, ordinary);
  // This test proves durable state, not AWS ingress or cloud authority. Those remain separate proof.
  vi.spyOn(AWSProvider.prototype, "reconcileLeaseAccess").mockResolvedValue();
  let active: ReturnType<typeof coordinator> | undefined = coordinator();
  try {
    const existing = await active.runtime.storage.pool.query<{ existing: string | null }>(
      "select to_regclass('crabbox.coordinator_kv')::text as existing",
    );
    expect(existing.rows[0]?.existing).toBeNull();
    // Hold the job runner offline initially: a failed wake hint must not lose committed due work.
    await active.runtime.storage.initialize();
    await active.runtime.storage.put(`lease:${legacy.id}`, legacy);
    await active.runtime.storage.put(`lease:${ordinary.id}`, ordinary);
    const before = await active.runtime.storage.get<LeaseRecord>(`lease:${legacy.id}`);
    const ordinaryBefore = await active.runtime.storage.get<LeaseRecord>(`lease:${ordinary.id}`);
    const beforeWake = await active.runtime.getAlarm();
    const auditKey = `aws-cleanup-recovery-audit:${legacy.id}`;
    const inspected = await active.fleet.fetch(request(legacy.id, "cleanup"));
    expect(inspected.status).toBe(200);
    const { inspection } = (await inspected.json()) as { inspection: { claimFingerprint: string } };

    // Wrap the existing real transaction. Observe both writes, then force its actual SQL ROLLBACK.
    const transact = active.runtime.storage.transaction.bind(active.runtime.storage);
    let stagedPair = false;
    const abort = vi
      .spyOn(active.runtime.storage, "transaction")
      .mockImplementation(
        async <T>(callback: (transaction: CoordinatorStorageView) => Promise<T>): Promise<T> =>
          transact(async (transaction) => {
            const result = await callback(transaction);
            const stagedLease = await transaction.get<LeaseRecord>(`lease:${legacy.id}`);
            const stagedAudit = await transaction.get<AWSLegacyCleanupAudit>(auditKey);
            if (stagedLease?.providerScope === scope && stagedAudit?.providerScope === scope) {
              stagedPair = true;
              throw new Error("synthetic failure after writes before PostgreSQL commit");
            }
            return result;
          }),
      );
    try {
      const failed = await active.fleet.fetch(acknowledge(legacy.id, inspection.claimFingerprint));
      expect(failed.status).toBe(500);
      expect(stagedPair).toBe(true);
    } finally {
      abort.mockRestore();
    }
    await active.runtime.stop();
    active = undefined;

    // A new runtime owns a new real pg Pool; no fake store or shared in-memory records survive.
    active = coordinator();
    expect(await active.runtime.storage.get(`lease:${legacy.id}`)).toEqual(before);
    expect(await active.runtime.storage.get(auditKey)).toBeUndefined();
    expect(await physicalScopes(active.runtime, [auditKey, `lease:${legacy.id}`])).toEqual([
      { key: `lease:${legacy.id}`, scope: null },
    ]);
    expect(await active.runtime.getAlarm()).toBe(beforeWake);
    expect(await active.runtime.storage.get(`lease:${ordinary.id}`)).toEqual(ordinaryBefore);
    const recovered = await active.fleet.fetch(acknowledge(legacy.id, inspection.claimFingerprint));
    expect(recovered.status).toBe(200);
    const { recovery } = (await recovered.json()) as { recovery: AWSLegacyCleanupAudit };
    const committedWake = await active.runtime.getAlarm();
    expect(committedWake).toBeTypeOf("number");
    expect(committedWake).toBeLessThanOrEqual(Date.now());
    await active.runtime.stop();
    active = undefined;

    active = coordinator();
    expect(await active.runtime.storage.get(auditKey)).toEqual(recovery);
    expect(await physicalScopes(active.runtime, [auditKey, `lease:${legacy.id}`])).toEqual([
      { key: auditKey, scope },
      { key: `lease:${legacy.id}`, scope },
    ]);
    expect(await active.runtime.getAlarm()).toBe(committedWake);
    const durable = await active.runtime.storage.get<LeaseRecord>(`lease:${legacy.id}`);
    expect(durable).toMatchObject({
      keep: true,
      releaseDeletesServer: true,
      providerScope: scope,
      provisioningResourceMayExist: false,
      cleanupError: legacy.cleanupError,
      host: legacy.host,
      sshHostKey: legacy.sshHostKey,
    });
    expect(durable?.cleanupCompletedAt).toBeUndefined();
    expect(await active.runtime.storage.get(`lease:${ordinary.id}`)).toEqual(ordinaryBefore);

    // Start the supported pg-boss runtime only after the persisted recovery has been reread.
    const resumed = active;
    await resumed.runtime.start(() => resumed.fleet.alarm());
    await vi.waitFor(
      async () => {
        const completed = await resumed.runtime.storage.get<LeaseRecord>(`lease:${legacy.id}`);
        expect(Number.isFinite(Date.parse(completed?.cleanupCompletedAt ?? ""))).toBe(true);
      },
      { timeout: 10_000, interval: 100 },
    );
    expect(await resumed.runtime.storage.get(auditKey)).toEqual(recovery);
    expect(await resumed.runtime.storage.get(`lease:${ordinary.id}`)).toEqual(ordinaryBefore);
    expect(aws.deletedKeys).toEqual(["key-00000000000000001"]);
    expect(aws.terminated).toEqual([]);
    const auditInspection = await resumed.fleet.fetch(request(legacy.id, "cleanup"));
    expect(auditInspection.status).toBe(200);
    await expect(auditInspection.json()).resolves.toMatchObject({
      inspection: { recoveryAudit: recovery },
    });
    const replay = await resumed.fleet.fetch(acknowledge(legacy.id, inspection.claimFingerprint));
    expect(replay.status).toBe(200);
    await expect(replay.json()).resolves.toMatchObject({ recovery });

    // An ordinary account-bound lease still uses normal release; it needs no legacy repair audit.
    const ownerRelease = request(ordinary.id, "release", { delete: true });
    ownerRelease.headers.delete("x-crabbox-admin");
    const released = await resumed.fleet.fetch(ownerRelease);
    expect(released.status).toBe(200);
    await vi.waitFor(
      async () => {
        const completed = await resumed.runtime.storage.get<LeaseRecord>(`lease:${ordinary.id}`);
        expect(Number.isFinite(Date.parse(completed?.cleanupCompletedAt ?? ""))).toBe(true);
      },
      { timeout: 10_000, interval: 100 },
    );
    expect(aws.terminated).toEqual([ordinary.cloudID]);
    expect(aws.deletedKeys).toEqual(["key-00000000000000001", "key-00000000000000002"]);
    expect(
      await resumed.runtime.storage.get(`aws-cleanup-recovery-audit:${ordinary.id}`),
    ).toBeUndefined();
    expect(await resumed.runtime.storage.get(auditKey)).toEqual(recovery);
  } finally {
    await active?.runtime.stop();
  }
}, 60_000);

async function physicalScopes(runtime: NodeCoordinatorRuntime, keys: string[]) {
  // Independently inspect the real compatibility table, not a coordinator cache or a fake Pool.
  const result = await runtime.storage.pool.query<{ key: string; scope: string | null }>(
    "select key, value ->> 'providerScope' as scope from crabbox.coordinator_kv where key = any($1::text[]) order by key",
    [keys],
  );
  return result.rows;
}

function coordinator() {
  const runtime = new NodeCoordinatorRuntime(database.toString());
  const mutex = new AsyncMutex();
  runtime.setOperationRunner((callback) => mutex.run(callback));
  return { runtime, fleet: new FleetCoordinator(runtime, env) };
}

function request(id: string, action: string, body?: unknown): Request {
  return new Request(`https://coordinator.test/v1/leases/${id}/${action}`, {
    method: body === undefined ? "GET" : "POST",
    headers,
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

function acknowledge(id: string, fingerprint: string): Request {
  return request(id, "cleanup", {
    action: "acknowledge-missing-resource",
    expectedClaimFingerprint: fingerprint,
  });
}

function lease(id: string, cloudID: string, slug: string): LeaseRecord {
  return {
    id,
    cloudID,
    slug,
    provider: "aws",
    region,
    owner: "alice@example.com",
    org: orgKeyForLabel("example-org"),
    profile: "default",
    class: "tiny",
    serverType: "t3.micro",
    serverID: 0,
    serverName: `crabbox-${slug}`,
    providerKey: providerKeyForLease(id),
    providerKeyCleanupOwned: true,
    host: "192.0.2.10",
    sshUser: "ubuntu",
    sshPort: "22",
    workRoot: "/work/crabbox",
    sshHostKey: "ssh-ed25519 synthetic-host-key",
    keep: true,
    ttlSeconds: 3600,
    estimatedHourlyUSD: 0.01,
    maxEstimatedUSD: 1,
    state: "active",
    createdAt: new Date(Date.now() - 60_000).toISOString(),
    updatedAt: new Date(Date.now() - 60_000).toISOString(),
    expiresAt: new Date(Date.now() + 60 * 60_000).toISOString(),
  };
}

function ownershipTags(value: LeaseRecord) {
  return [
    { key: "crabbox", value: "true" },
    { key: "created_by", value: "crabbox" },
    { key: "lease", value: value.id },
    { key: "owner", value: "alice_example.com" },
    { key: "provider", value: "aws" },
    { key: "slug", value: value.slug! },
    { key: "provider_key", value: value.providerKey },
  ];
}

function xml(body: string) {
  return new Response(body, { headers: { "content-type": "text/xml" } });
}

function syntheticAWS(legacy: LeaseRecord, ordinary: LeaseRecord) {
  const deletedKeys: string[] = [];
  const terminated: string[] = [];
  const resources = new Map([
    [legacy.providerKey, { lease: legacy, keyID: "key-00000000000000001" }],
    [ordinary.providerKey, { lease: ordinary, keyID: "key-00000000000000002" }],
  ]);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const outgoing = input instanceof Request ? input : new Request(input, init);
      if (outgoing.headers.get("x-amz-target")?.endsWith(".LookupEvents")) {
        return Response.json({
          Events: [
            {
              EventName: "RunInstances",
              EventSource: "ec2.amazonaws.com",
              EventId: "00000000-0000-4000-8000-000000000001",
              CloudTrailEvent: JSON.stringify({
                eventName: "RunInstances",
                eventSource: "ec2.amazonaws.com",
                eventType: "AwsApiCall",
                eventID: "00000000-0000-4000-8000-000000000001",
                eventTime: new Date(Date.parse(legacy.createdAt) + 15_000).toISOString(),
                recipientAccountId: "123456789012",
                awsRegion: region,
                requestParameters: {
                  keyName: legacy.providerKey,
                  tagSpecificationSet: {
                    items: [{ resourceType: "instance", tags: ownershipTags(legacy) }],
                  },
                },
                responseElements: { instancesSet: { items: [{ instanceId: legacy.cloudID }] } },
              }),
            },
          ],
        });
      }
      const params = new URLSearchParams(await outgoing.clone().text());
      switch (params.get("Action")) {
        case "GetCallerIdentity":
          return xml(
            "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/synthetic</Arn><UserId>synthetic</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>",
          );
        case "DescribeInstances": {
          if (
            params.get("InstanceId.1") !== ordinary.cloudID ||
            terminated.includes(ordinary.cloudID)
          ) {
            return xml(
              "<DescribeInstancesResponse><requestId>synthetic-read</requestId><reservationSet /></DescribeInstancesResponse>",
            );
          }
          const tags = ownershipTags(ordinary)
            .map((tag) => `<item><key>${tag.key}</key><value>${tag.value}</value></item>`)
            .join("");
          return xml(
            `<DescribeInstancesResponse><requestId>synthetic-read</requestId><reservationSet><item><instancesSet><item><instanceId>${ordinary.cloudID}</instanceId><instanceState><name>running</name></instanceState><tagSet>${tags}</tagSet></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`,
          );
        }
        case "TerminateInstances": {
          const id = params.get("InstanceId.1") ?? "";
          if (id !== ordinary.cloudID) throw new Error("unexpected synthetic termination");
          terminated.push(id);
          return xml(
            `<TerminateInstancesResponse><instancesSet><item><instanceId>${id}</instanceId></item></instancesSet></TerminateInstancesResponse>`,
          );
        }
        case "DescribeKeyPairs": {
          const resource = resources.get(params.get("KeyName.1") ?? "");
          if (!resource) throw new Error("unexpected synthetic key lookup");
          const tags = ownershipTags(resource.lease)
            .map((tag) => `<item><key>${tag.key}</key><value>${tag.value}</value></item>`)
            .join("");
          return xml(
            `<DescribeKeyPairsResponse><keySet><item><keyPairId>${resource.keyID}</keyPairId><tagSet>${tags}</tagSet></item></keySet></DescribeKeyPairsResponse>`,
          );
        }
        case "DeleteKeyPair":
          deletedKeys.push(params.get("KeyPairId") ?? "");
          return xml("<DeleteKeyPairResponse />");
        default:
          throw new Error("unexpected network operation in synthetic AWS fixture");
      }
    }),
  );
  return { deletedKeys, terminated };
}
