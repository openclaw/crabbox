import { describe, expect, it } from "vitest";

import {
  cleanupFirstWalk,
  prepareFirstWalk,
  type FirstWalkOwner,
  type FirstWalkState,
} from "../src/aws-first-walk";
import type { AWSQualificationFirstWalkRequest } from "../src/aws-qualification-contract";

const request: AWSQualificationFirstWalkRequest = {
  checkpointID: "chk_capture",
  claimHash: "a".repeat(64),
  instanceID: "i-11111111",
  snapshotID: "snap-22222222",
  readinessReceiptSha256: "b".repeat(64),
};
function fixture() {
  let state: FirstWalkState | undefined;
  let volume = false,
    attached = false,
    dot = false;
  let occupied = false,
    foreign = false,
    cancelled = false;
  let loss: string | undefined, reject: string | undefined;
  let visible = true;
  const calls: Array<{ action: string; parameters: Record<string, string>; mutating: boolean }> =
    [];
  const owner: FirstWalkOwner = {
    identity: {
      runId: "test-1",
      owner: "fixture",
      candidateSha: "c".repeat(40),
      candidateWorker: "private-fixture",
      deploymentHash: "d".repeat(64),
      expiresAt: "2099-01-01T00:00:00Z",
      firstWalk: true,
    },
    accountID: "123456789012",
    read: async () => structuredClone(state),
    write: async (s) => {
      state = structuredClone(s);
    },
    query: async (action, parameters, mutating = false) => {
      calls.push({ action, parameters, mutating });
      if (mutating && cancelled) throw new Error("fenced");
      if (reject === action) throw new Error("denied");
      let body = "";
      if (action === "DescribeInstances")
        body = `<reservationSet><item><ownerId>123456789012</ownerId><instancesSet><item><instanceId>${request.instanceID}</instanceId><instanceState><name>running</name></instanceState><placement><availabilityZone>us-east-1a</availabilityZone></placement><tagSet><item><key>crabbox_qualification_run</key><value>test-1</value></item></tagSet><blockDeviceMapping><item><deviceName>/dev/sda1</deviceName><ebs><volumeId>vol-55555555</volumeId></ebs></item>${attached || occupied ? `<item><deviceName>/dev/sdf</deviceName><ebs><volumeId>vol-33333333</volumeId><deleteOnTermination>${dot}</deleteOnTermination></ebs></item>` : ""}</blockDeviceMapping></item></instancesSet></item></reservationSet>`;
      if (action === "DescribeSnapshots")
        body = `<snapshotSet><item><snapshotId>${request.snapshotID}</snapshotId><ownerId>123456789012</ownerId><status>completed</status><volumeSize>8</volumeSize></item></snapshotSet>`;
      if (action === "CreateVolume") {
        volume = true;
        body = "<volumeId>vol-33333333</volumeId>";
      }
      if (action === "AttachVolume") attached = true;
      if (action === "ModifyInstanceAttribute") dot = true;
      if (action === "DeleteVolume") volume = false;
      if (action === "DescribeVolumes") {
        if (!volume && parameters["VolumeId.1"]) throw new Error("InvalidVolume.NotFound");
        body = `<volumeSet>${
          volume && visible
            ? `<item><volumeId>vol-33333333</volumeId><snapshotId>${request.snapshotID}</snapshotId><availabilityZone>us-east-1a</availabilityZone><size>8</size><volumeType>gp3</volumeType><iops>3000</iops><throughput>125</throughput><encrypted>true</encrypted><status>${attached ? "in-use" : "available"}</status><tagSet>${Object.entries(
                {
                  crabbox_qualification_run: foreign ? "foreign" : "test-1",
                  crabbox_qualification_owner: "fixture",
                  crabbox_qualification_sha: "c".repeat(40),
                  crabbox_qualification_expiry: "2099-01-01T00:00:00Z",
                  crabbox_qualification_op: "first-walk-data",
                },
              )
                .map(([key, value]) => `<item><key>${key}</key><value>${value}</value></item>`)
                .join(
                  "",
                )}</tagSet><attachmentSet>${attached ? `<item><instanceId>${request.instanceID}</instanceId><device>/dev/sdf</device><status>attached</status></item>` : ""}</attachmentSet></item>`
            : ""
        }</volumeSet>`;
      }
      if (loss === action) {
        loss = undefined;
        throw new Error("response lost after effect");
      }
      return `<${action}Response>${body}</${action}Response>`;
    },
  };
  return {
    owner,
    calls,
    get state() {
      return state;
    },
    set loss(v: string | undefined) {
      loss = v;
    },
    set reject(v: string | undefined) {
      reject = v;
    },
    set occupied(v: boolean) {
      occupied = v;
    },
    set foreign(v: boolean) {
      foreign = v;
    },
    set cancelled(v: boolean) {
      cancelled = v;
    },
    set visible(v: boolean) {
      visible = v;
    },
    settleGuest() {
      attached = false;
    },
    get volume() {
      return volume;
    },
  };
}
async function complete(f: ReturnType<typeof fixture>) {
  for (let i = 0; i < 4; i++) {
    try {
      // oxlint-disable-next-line eslint/no-await-in-loop -- each retry depends on the previous persisted outcome.
      return await prepareFirstWalk(f.owner, request);
    } catch (error) {
      if (!/dispatched|readback/.test(String(error))) throw error;
    }
  }
  throw new Error("not completed");
}
describe("one-copy first-walk owner lifecycle", () => {
  it.each(["CreateVolume", "AttachVolume"])("never replaces a denied %s intent", async (action) => {
    const f = fixture();
    f.reject = action;
    await expect(prepareFirstWalk(f.owner, request)).rejects.toThrow("denied");
    f.reject = undefined;
    await expect(prepareFirstWalk(f.owner, request)).rejects.toThrow(/unresolved|retry reads/);
    expect(f.calls.filter((c) => c.action === action)).toHaveLength(1);
    await cleanupFirstWalk(f.owner);
    expect(f.volume).toBe(false);
  });
  it.each(["paginated", "multiple"])(
    "refuses %s volume discovery without attaching or deleting",
    async (kind) => {
      const f = fixture();
      f.loss = "CreateVolume";
      await expect(prepareFirstWalk(f.owner, request)).rejects.toThrow("lost");
      const query = f.owner.query;
      f.owner.query = async (...args) => {
        const body = await query(...args);
        if (args[0] !== "DescribeVolumes") return body;
        return kind === "paginated"
          ? body.replace(
              "</DescribeVolumesResponse>",
              "<nextToken>more</nextToken></DescribeVolumesResponse>",
            )
          : body.replace("</volumeSet>", body.match(/<item>.*<\/item>/)![0] + "</volumeSet>");
      };
      await expect(prepareFirstWalk(f.owner, request)).rejects.toThrow(/paginated|exceeds/);
      await expect(cleanupFirstWalk(f.owner)).rejects.toThrow(/paginated|exceeds/);
      expect(f.calls.some((c) => ["AttachVolume", "DeleteVolume"].includes(c.action))).toBe(false);
    },
  );
  it("creates once after admission, attaches exact IDs and proves deletion policy by readback", async () => {
    const f = fixture();
    expect((await complete(f)).phase).toBe("attached");
    await prepareFirstWalk(f.owner, request);
    const creates = f.calls.filter((c) => c.action === "CreateVolume");
    expect(creates).toHaveLength(1);
    expect(creates[0]?.parameters).toMatchObject({
      SnapshotId: request.snapshotID,
      AvailabilityZone: "us-east-1a",
      VolumeType: "gp3",
      Iops: "3000",
      Throughput: "125",
      Encrypted: "true",
    });
    expect(creates[0]?.parameters["ClientToken"]).toMatch(/^[a-f0-9]{64}$/);
    expect(f.calls.filter((c) => c.action === "AttachVolume")).toHaveLength(1);
    expect(f.calls.some((c) => JSON.stringify(c).includes("UUID"))).toBe(false);
    await expect(
      prepareFirstWalk(f.owner, { ...request, snapshotID: "snap-44444444" }),
    ).rejects.toThrow("immutable");
  });
  it.each(["CreateVolume", "AttachVolume", "ModifyInstanceAttribute"])(
    "recovers a committed %s response loss from durable state without replacement",
    async (action) => {
      const f = fixture();
      f.loss = action;
      for (let i = 0; i < 4; i++) {
        try {
          // oxlint-disable-next-line eslint/no-await-in-loop -- each retry depends on the previous persisted outcome.
          await prepareFirstWalk(f.owner, request);
        } catch {
          /* next RPC reads same persisted state */
        }
      }
      expect(f.state?.phase).toBe("attached");
      expect(f.calls.filter((c) => c.action === "CreateVolume")).toHaveLength(1);
      expect(f.calls.filter((c) => c.action === "AttachVolume")).toHaveLength(1);
    },
  );
  it("retains an uncertain empty create and never creates a replacement", async () => {
    const f = fixture();
    f.loss = "CreateVolume";
    f.visible = false;
    await expect(prepareFirstWalk(f.owner, request)).rejects.toThrow("lost");
    await expect(prepareFirstWalk(f.owner, request)).rejects.toThrow("unresolved");
    expect(f.calls.filter((c) => c.action === "CreateVolume")).toHaveLength(1);
    f.visible = true;
    await complete(f);
  });
  it("cleans an unattached partial create, preserves source and retries deletion after lost response", async () => {
    const f = fixture();
    f.reject = "AttachVolume";
    await expect(prepareFirstWalk(f.owner, request)).rejects.toThrow("denied");
    f.reject = undefined;
    f.loss = "DeleteVolume";
    await expect(cleanupFirstWalk(f.owner)).rejects.toThrow("lost");
    expect(await cleanupFirstWalk(f.owner)).toEqual(["vol-33333333"]);
    expect(f.volume).toBe(false);
    expect(
      f.calls.some((c) => ["DeleteSnapshot", "DeregisterImage", "DetachVolume"].includes(c.action)),
    ).toBe(false);
  });
  it("waits for guest settlement and retries explicit volume deletion failure", async () => {
    const f = fixture();
    await complete(f);
    await expect(cleanupFirstWalk(f.owner)).rejects.toThrow("busy");
    f.settleGuest();
    f.reject = "DeleteVolume";
    await expect(cleanupFirstWalk(f.owner)).rejects.toThrow("denied");
    expect(f.volume).toBe(true);
    f.reject = undefined;
    expect(await cleanupFirstWalk(f.owner)).toEqual(["vol-33333333"]);
  });
  it("rejects device collision and foreign exact-volume readback before mutation", async () => {
    const f = fixture();
    f.occupied = true;
    await expect(prepareFirstWalk(f.owner, request)).rejects.toThrow("one root EBS volume");
    expect(f.calls.some((c) => c.mutating)).toBe(false);
    f.occupied = false;
    f.loss = "CreateVolume";
    await expect(prepareFirstWalk(f.owner, request)).rejects.toThrow("lost");
    f.foreign = true;
    await expect(cleanupFirstWalk(f.owner)).rejects.toThrow("identity");
    expect(f.calls.some((c) => c.action === "DeleteVolume")).toBe(false);
  });
  it("a live cancellation fence stops dispatch while preserving the cleanup intent", async () => {
    const f = fixture();
    f.cancelled = true;
    await expect(prepareFirstWalk(f.owner, request)).rejects.toThrow("fenced");
    expect(f.state?.phase).toBe("creating");
    expect(f.volume).toBe(false);
    f.cancelled = false;
    expect(await cleanupFirstWalk(f.owner)).toEqual([]);
  });
});
