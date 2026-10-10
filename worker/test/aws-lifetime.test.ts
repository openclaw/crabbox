import { afterEach, describe, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import type { Env } from "../src/types";

const instanceID = "i-0123456789abcdef0";
const leaseID = "cbx_abcdef123456";
const volumeID = "vol-0123456789abcdef0";
const identity = `${instanceID}:${volumeID}:/dev/sda1`;

function fixture(
  options: {
    spot?: boolean;
    extraDisk?: boolean;
    localDisk?: boolean;
    wrongVolumeOwner?: boolean;
    wrongResponseID?: boolean;
  } = {},
) {
  const actions: string[] = [];
  const xml = (text: string, status = 200) =>
    new Response(text, { status, headers: { "content-type": "text/xml" } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      const params = new URLSearchParams(await request.text());
      const action = params.get("Action")!;
      actions.push(action);
      if (action === "GetCallerIdentity")
        return xml(
          "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>",
        );
      if (action === "DescribeInstances")
        return xml(
          `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>${instanceID}</instanceId><instanceType>m7i.large</instanceType><rootDeviceType>ebs</rootDeviceType><rootDeviceName>/dev/sda1</rootDeviceName>${options.spot ? "<instanceLifecycle>spot</instanceLifecycle>" : ""}<tagSet><item><key>lease</key><value>${leaseID}</value></item></tagSet><blockDeviceMapping><item><deviceName>/dev/sda1</deviceName><ebs><volumeId>${volumeID}</volumeId><deleteOnTermination>true</deleteOnTermination></ebs></item>${options.extraDisk ? "<item><deviceName>/dev/sdf</deviceName><ebs><volumeId>vol-abcdef</volumeId></ebs></item>" : ""}</blockDeviceMapping></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`,
        );
      if (action === "DescribeInstanceTypes")
        return xml(
          `<DescribeInstanceTypesResponse><instanceTypeSet><item><instanceType>m7i.large</instanceType><instanceStorageSupported>${options.localDisk ? "true" : "false"}</instanceStorageSupported></item></instanceTypeSet></DescribeInstanceTypesResponse>`,
        );
      if (action === "DescribeVolumes")
        return xml(
          `<DescribeVolumesResponse><volumeSet><item><volumeId>${volumeID}</volumeId><tagSet><item><key>lease</key><value>${options.wrongVolumeOwner ? "another-lease" : leaseID}</value></item></tagSet><attachmentSet><item><instanceId>${instanceID}</instanceId><device>/dev/sda1</device><status>attached</status></item></attachmentSet></item></volumeSet></DescribeVolumesResponse>`,
        );
      if (action === "StopInstances")
        return xml(
          `<StopInstancesResponse><instancesSet><item><instanceId>${options.wrongResponseID ? "i-abcdef" : instanceID}</instanceId></item></instancesSet></StopInstancesResponse>`,
        );
      throw new Error(`unexpected AWS operation ${action}`);
    }),
  );
  const client = new EC2SpotClient(
    { AWS_ACCESS_KEY_ID: "test-key", AWS_SECRET_ACCESS_KEY: "test-secret" } as Env,
    "eu-west-1",
  );
  return { client, actions };
}

afterEach(() => vi.unstubAllGlobals());

describe("AWS lifetime stop", () => {
  it("uses one account-verified operation and validates the exact disk before StopInstances", async () => {
    const f = fixture();
    const assertOwner = vi.fn<() => Promise<void>>(async () => {});
    await f.client.withLeaseOperation(async (session) => {
      expect(await session.retainedServerIdentity(instanceID, leaseID)).toBe(identity);
      await session.stopRetainedServer(instanceID, leaseID, identity, assertOwner);
    });
    expect(assertOwner).toHaveBeenCalledOnce();
    expect(f.actions.filter((action) => action === "GetCallerIdentity")).toHaveLength(1);
    expect(f.actions.at(-1)).toBe("StopInstances");
    expect(f.actions).not.toContain("TerminateInstances");
  });
  it.each([{ spot: true }, { extraDisk: true }, { localDisk: true }, { wrongVolumeOwner: true }])(
    "refuses unsupported or foreign storage: %j",
    async (options) => {
      const f = fixture(options);
      await expect(
        f.client.withLeaseOperation((session) =>
          session.stopRetainedServer(instanceID, leaseID, identity, async () => {}),
        ),
      ).rejects.toThrow(/lifetime retention|retained root disk/);
      expect(f.actions).not.toContain("StopInstances");
    },
  );
  it("rechecks disk identity and cleanup ownership at the mutation boundary", async () => {
    const f = fixture();
    await expect(
      f.client.withLeaseOperation((session) =>
        session.stopRetainedServer(instanceID, leaseID, "another-disk", async () => {}),
      ),
    ).rejects.toThrow("changed");
    await expect(
      f.client.withLeaseOperation((session) =>
        session.stopRetainedServer(instanceID, leaseID, identity, async () => {
          throw new Error("claim changed");
        }),
      ),
    ).rejects.toThrow("claim changed");
    expect(f.actions).not.toContain("StopInstances");
  });
  it("does not treat an unrelated stop response as acceptance", async () => {
    const f = fixture({ wrongResponseID: true });
    await expect(
      f.client.withLeaseOperation((session) =>
        session.stopRetainedServer(instanceID, leaseID, identity, async () => {}),
      ),
    ).rejects.toThrow("did not confirm");
  });
  it("does not call instance termination complete while the retained disk still exists", async () => {
    const f = fixture();
    await expect(
      f.client.withLeaseOperation((session) => session.confirmRetainedVolumeDeleted(identity)),
    ).rejects.toThrow("not been confirmed");
  });
});
