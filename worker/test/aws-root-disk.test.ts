import { afterEach, describe, expect, it, vi } from "vitest";

import { EC2SpotClient, type AWSLeaseOperation } from "../src/aws";
import {
  acquisitionRootDisk,
  sameAWSRootDisk,
  validAWSRootDiskIdentity,
} from "../src/aws-root-disk";
import { leaseConfig } from "../src/config";
import { AWSProvider } from "../src/fleet";
import {
  validateProviderProvisioningCleanupClaim,
  type ProviderProvisioningCleanupClaim,
} from "../src/provider-provisioning";
import type { Env, LeaseRecord, ProviderMachine } from "../src/types";

const disk = {
  leaseID: "cbx_abcdef123456",
  instanceID: "i-0123456789abcdef0",
  volumeID: "vol-0123456789abcdef0",
  deviceName: "/dev/sda1",
  region: "eu-west-1",
  providerScope: "aws:account:123456789012",
};
const machine: ProviderMachine = {
  provider: "aws",
  id: 0,
  cloudID: disk.instanceID,
  name: "test",
  status: "running",
  host: "192.0.2.1",
  serverType: "t3.small",
  labels: {
    lease: disk.leaseID,
    crabbox: "true",
    created_by: "crabbox",
    provider: "aws",
    slug: "test",
    owner: "alice",
  },
  awsRootVolumeID: disk.volumeID,
  awsRootDeviceName: disk.deviceName,
};
const lease = {
  id: disk.leaseID,
  slug: "test",
  provider: "aws",
  cloudID: disk.instanceID,
  region: disk.region,
  providerScope: disk.providerScope,
  owner: "alice",
  providerOwner: "alice",
  awsOriginalRootDisk: disk,
} as LeaseRecord;
const claim: ProviderProvisioningCleanupClaim = {
  provider: "aws",
  cloudID: disk.instanceID,
  region: disk.region,
  providerScope: disk.providerScope,
  awsOriginalRootDisk: disk,
};
const provider = () =>
  new AWSProvider(
    { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "test" } as Env,
    disk.region,
    {} as never,
  );
function session(overrides: Partial<AWSLeaseOperation> = {}) {
  const operation = {
    region: disk.region,
    verifiedIdentity: vi.fn<() => Promise<{ account: string }>>(async () => ({
      account: "123456789012",
    })),
    findServer: vi.fn<AWSLeaseOperation["findServer"]>(async () => undefined),
    retainedVolumePresent: vi.fn<AWSLeaseOperation["retainedVolumePresent"]>(async () => true),
    ...overrides,
  } as unknown as AWSLeaseOperation;
  vi.spyOn(EC2SpotClient.prototype, "withLeaseOperation").mockImplementation(async (fn) =>
    fn(operation),
  );
  return operation;
}
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("original AWS root identity", () => {
  it.each(["exact", "missing root name", "duplicate root"])(
    "does not invent acquisition identity from an ambiguous provider response: %s",
    async (shape) => {
      const mapping =
        "<item><deviceName>/dev/sda1</deviceName><ebs><volumeId>" +
        disk.volumeID +
        "</volumeId></ebs></item>";
      vi.stubGlobal(
        "fetch",
        vi.fn(
          async () =>
            new Response(
              "<DescribeInstancesResponse><requestId>test-root</requestId><reservationSet><item><instancesSet><item><instanceId>" +
                disk.instanceID +
                "</instanceId>" +
                (shape === "missing root name"
                  ? ""
                  : "<rootDeviceName>/dev/sda1</rootDeviceName>") +
                "<blockDeviceMapping>" +
                mapping +
                (shape === "duplicate root" ? mapping : "") +
                "</blockDeviceMapping><tagSet><item><key>lease</key><value>" +
                disk.leaseID +
                "</value></item></tagSet></item></instancesSet></item></reservationSet></DescribeInstancesResponse>",
              { headers: { "content-type": "text/xml" } },
            ),
        ),
      );
      const client = new EC2SpotClient(
        { AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "test" } as Env,
        disk.region,
      );
      const observed = await client.getServer(disk.instanceID);
      expect(acquisitionRootDisk(observed, disk.leaseID, disk.providerScope, disk.region)).toEqual(
        shape === "exact" ? disk : undefined,
      );
    },
  );
  it("captures provider-observed exact acquisition ownership without lifetime enrollment", () => {
    expect(acquisitionRootDisk(machine, disk.leaseID, disk.providerScope, disk.region)).toEqual(
      disk,
    );
    expect(validAWSRootDiskIdentity(disk)).toBe(true);
    expect(sameAWSRootDisk(disk, { ...disk })).toBe(true);
    expect(lease.lifetimePolicy).toBeUndefined();
  });
  it.each([
    { labels: { lease: "cbx_ffffffffffff" } },
    { awsRootDeviceName: undefined },
    { awsRootVolumeID: undefined },
    { awsRootDeviceName: "/dev/sda1:other" },
    { awsRootVolumeID: "vol-invalid" },
  ])("does not invent incomplete or foreign acquisition identity: %j", (change) => {
    expect(
      acquisitionRootDisk({ ...machine, ...change }, disk.leaseID, disk.providerScope, disk.region),
    ).toBeUndefined();
  });
  it.each([
    { instanceID: "i-fffffffffffffffff" },
    { region: "us-east-1" },
    { providerScope: "aws:account:999999999999" },
    { volumeID: "bad" },
  ])("rejects malformed or cross-context allocation claims: %j", (change) => {
    expect(
      validateProviderProvisioningCleanupClaim(
        { ...claim, awsOriginalRootDisk: { ...disk, ...change } },
        "aws",
      ),
    ).toBeUndefined();
  });
  it("reads the original exact disk after instance disappearance without modifying a lease", async () => {
    const op = session();
    const before = structuredClone(lease);
    await expect(provider().lifetimeReadback(lease)).resolves.toEqual({
      instanceId: disk.instanceID,
      instanceState: "terminated",
      rootVolumeId: disk.volumeID,
      rootVolumePresent: true,
    });
    expect(op.retainedVolumePresent).toHaveBeenCalledWith(
      [disk.instanceID, disk.volumeID, disk.deviceName].join(":"),
      disk.leaseID,
    );
    expect(lease).toEqual(before);
  });
  it.each([
    { leaseID: "cbx_ffffffffffff" },
    { instanceID: "i-fffffffffffffffff" },
    { region: "us-east-1" },
    { providerScope: "aws:account:999999999999" },
  ])("refuses a persisted binding moved to another context: %j", async (change) => {
    const op = session();
    await expect(
      provider().lifetimeReadback({ ...lease, awsOriginalRootDisk: { ...disk, ...change } }),
    ).rejects.toThrow("binding does not match lease");
    expect(op.retainedVolumePresent).not.toHaveBeenCalled();
  });
  it.each([{ region: "us-east-1" }, { providerScope: "aws:account:999999999999" }])(
    "authenticates the original lease scope before disk observation: %j",
    async (change) => {
      const op = session();
      await expect(provider().lifetimeReadback({ ...lease, ...change })).rejects.toThrow(
        "does not match",
      );
      expect(op.findServer).not.toHaveBeenCalled();
      expect(op.retainedVolumePresent).not.toHaveBeenCalled();
    },
  );
  it("never upgrades a live replacement mapping to original evidence", async () => {
    const op = session({
      findServer: vi.fn<AWSLeaseOperation["findServer"]>(async () => ({
        ...machine,
        awsRootVolumeID: "vol-fffffffffffffffff",
      })),
    });
    await expect(provider().lifetimeReadback(lease)).rejects.toThrow(
      "current AWS root disk does not match",
    );
    expect(op.retainedVolumePresent).not.toHaveBeenCalled();
  });
  it("requires retained evidence for historical leases even if a disk is currently visible", async () => {
    const originalRootDiskEvidence = vi.fn<() => Promise<never>>(async () => {
      throw new Error("requires the original root disk identity");
    });
    const op = session({
      findServer: vi.fn<AWSLeaseOperation["findServer"]>(async () => machine),
      client: { originalRootDiskEvidence } as unknown as EC2SpotClient,
    });
    await expect(
      provider().lifetimeReadback({ ...lease, awsOriginalRootDisk: undefined }),
    ).rejects.toThrow("requires the original root disk identity");
    expect(op.retainedVolumePresent).not.toHaveBeenCalled();
  });
  it("reconciles historical originals from authenticated allocation evidence without persisting guessed identity", async () => {
    const evidence = {
      disk,
      eventID: "00000000-0000-4000-8000-000000000001",
      eventTime: "2026-10-20T12:00:00Z",
    };
    const originalRootDiskEvidence = vi.fn<() => Promise<typeof evidence>>(async () => evidence);
    const op = session({ client: { originalRootDiskEvidence } as unknown as EC2SpotClient });
    const legacy = { ...lease, awsOriginalRootDisk: undefined };
    const before = structuredClone(legacy);
    await expect(provider().lifetimeReadback(legacy)).resolves.toMatchObject({
      rootVolumeId: disk.volumeID,
      rootVolumePresent: true,
      rootIdentityEvidence: { eventID: evidence.eventID, eventTime: evidence.eventTime },
    });
    expect(originalRootDiskEvidence).toHaveBeenCalledWith(legacy, "123456789012");
    expect(op.retainedVolumePresent).toHaveBeenCalledWith(
      [disk.instanceID, disk.volumeID, disk.deviceName].join(":"),
      disk.leaseID,
    );
    expect(legacy).toEqual(before);
  });
  it("refuses contradictory retention records instead of picking a convenient disk", async () => {
    const op = session();
    await expect(
      provider().lifetimeReadback({
        ...lease,
        lifetimePolicy: {
          retainedIdentity: disk.instanceID + ":vol-fffffffffffffffff:/dev/sda1",
        } as never,
      }),
    ).rejects.toThrow("records conflict");
    expect(op.retainedVolumePresent).not.toHaveBeenCalled();
  });
  it.each(["initial", "ready", "changed", "bootstrap failure"])(
    "publishes the original before readiness/SSM: %s",
    async (stage) => {
      const initial = stage === "ready" ? { ...machine, awsRootVolumeID: undefined } : machine;
      const ready =
        stage === "changed" ? { ...machine, awsRootVolumeID: "vol-fffffffffffffffff" } : machine;
      const claims: ProviderProvisioningCleanupClaim[] = [];
      const client = {
        createServerWithFallback: async () => ({
          server: initial,
          serverType: "t3.small",
          imageID: "ami-0123456789abcdef0",
        }),
        waitForServerIP: async () => {
          expect(claims[0]?.awsOriginalRootDisk).toEqual(stage === "ready" ? undefined : disk);
          return ready;
        },
        waitForSSMOnline: async () => {
          expect(claims.at(-1)?.awsOriginalRootDisk).toEqual(disk);
          throw new Error("synthetic bootstrap unavailable");
        },
      } as unknown as EC2SpotClient;
      session({ client });
      const run = provider().createServerWithFallback(
        {
          ...leaseConfig({
            provider: "aws",
            sshPublicKey: "ssh-ed25519 test",
            awsRegion: disk.region,
          }),
          awsRequireSSM: stage === "bootstrap failure",
        },
        disk.leaseID,
        "test",
        "alice",
        {
          providerScope: disk.providerScope,
          onResourceCreated: async (value) => {
            claims.push(structuredClone(value));
            return true;
          },
        },
      );
      const result = await run.then(
        (value) => ({ cloudID: value.server.cloudID }),
        (error: Error) => ({ error: error.message }),
      );
      const expected: Record<string, unknown> = {
        changed: { error: expect.stringContaining("original AWS root disk changed") },
        "bootstrap failure": { error: expect.stringContaining("synthetic bootstrap unavailable") },
        initial: { cloudID: disk.instanceID },
        ready: { cloudID: disk.instanceID },
      };
      expect(result).toEqual(expected[stage]);
      expect(claims.at(-1)?.awsOriginalRootDisk).toEqual(disk);
      expect(
        claims.every(
          (value) => !value.awsOriginalRootDisk || sameAWSRootDisk(value.awsOriginalRootDisk, disk),
        ),
      ).toBe(true);
    },
  );
});
