import { readFileSync } from "node:fs";

import { describe, expect, it, vi } from "vitest";

import { EC2SpotClient } from "../src/aws";
import { AWSGuestNetworkControl } from "../src/aws-guest-network";
import { AWSQueryError } from "../src/aws-query-error";
import { awsRunInstancesUserData } from "../src/bootstrap";
import { leaseConfig } from "../src/config";
import { sha256Hex } from "../src/encoding";
import {
  guestNetworkPolicyIdentity,
  validateGuestNetworkCustodyUpdate,
  guestNetworkCreateRejectedNoEffect,
  guestPublicKeyUserData,
  serverGuestNetworkPolicy,
  validateGuestNetworkReceipt,
  type GuestNetworkBoundary,
  type GuestNetworkPolicy,
} from "../src/guest-network";

function publicKey(fill = 1): string {
  const b = new Uint8Array(51);
  new DataView(b.buffer).setUint32(0, 11);
  b.set(new TextEncoder().encode("ssh-ed25519"), 4);
  new DataView(b.buffer).setUint32(15, 32);
  b.fill(fill, 19);
  return "ssh-ed25519 " + btoa(String.fromCharCode(...b));
}
const policy: GuestNetworkPolicy = {
  bootstrapMode: "ssh-public-key-v1",
  version: 1,
  id: "example-guest-network",
  revision: "1",
  egress: [{ cidr: "93.184.216.34/32", ports: [443] }],
  sshSources: ["198.51.100.7/32"],
  platformExceptions: ["aws-platform-services-v1", "aws-roleless-imdsv2-public-bootstrap-v1"],
};
const leaseId = "cbx_0123456789ab",
  resourceId = "i-11111111";
class ControlPlane {
  calls: string[] = [];
  group: Record<string, unknown> | undefined;
  attached = false;
  userdata = guestPublicKeyUserData(publicKey(), "runner", "/workspace");
  instance: Record<string, unknown> = {};
  permissions(p: Record<string, string>) {
    const result = [];
    for (let n = 1; p["IpPermissions." + n + ".IpProtocol"]; n++) {
      const k = "IpPermissions." + n;
      result.push({
        ipProtocol: p[k + ".IpProtocol"],
        fromPort: p[k + ".FromPort"],
        toPort: p[k + ".ToPort"],
        ipRanges: { item: { cidrIp: p[k + ".IpRanges.1.CidrIp"] } },
      });
    }
    return { item: result };
  }
  query = async (action: string, p: Record<string, string>): Promise<Record<string, unknown>> => {
    this.calls.push(action);
    switch (action) {
      case "DescribeSubnets":
        return {
          subnetSet: {
            item: { subnetId: "subnet-11111111", vpcId: "vpc-11111111", state: "available" },
          },
        };
      case "DescribeVpcs":
        return { vpcSet: { item: { vpcId: "vpc-11111111", state: "available" } } };
      case "CreateSecurityGroup": {
        const tags = [];
        for (let n = 1; p["TagSpecification.1.Tag." + n + ".Key"]; n++)
          tags.push({
            key: p["TagSpecification.1.Tag." + n + ".Key"],
            value: p["TagSpecification.1.Tag." + n + ".Value"],
          });
        this.group = {
          groupId: "sg-11111111",
          groupName: p["GroupName"],
          vpcId: "vpc-11111111",
          tagSet: { item: tags },
          ipPermissions: { item: [] },
          ipPermissionsEgress: {
            item: { ipProtocol: "-1", ipRanges: { item: { cidrIp: "0.0.0.0/0" } } },
          },
        };
        return { groupId: "sg-11111111" };
      }
      case "DescribeSecurityGroups":
        if (!this.group)
          throw Object.assign(new Error("absent"), { code: "InvalidGroup.NotFound" });
        return { securityGroupInfo: { item: this.group } };
      case "RevokeSecurityGroupEgress":
        this.group!["ipPermissionsEgress"] = { item: [] };
        return {};
      case "AuthorizeSecurityGroupIngress":
        this.group!["ipPermissions"] = this.permissions(p);
        return {};
      case "AuthorizeSecurityGroupEgress":
        this.group!["ipPermissionsEgress"] = this.permissions(p);
        return {};
      case "DescribeInstances":
        return { reservationSet: { item: { instancesSet: { item: this.instance } } } };
      case "DescribeInstanceAttribute":
        return { instanceId: resourceId, userData: { value: this.userdata } };
      case "GetConsoleOutput":
        return {
          instanceId: resourceId,
          output: btoa("CRABBOX_GUEST_HOST_KEY_V1 " + publicKey(2)),
        };
      case "DescribeNetworkInterfaces":
        return {
          networkInterfaceSet: {
            item: this.attached ? [{ networkInterfaceId: "eni-11111111" }] : [],
          },
        };
      case "DeleteSecurityGroup":
        this.group = undefined;
        return {};
      default:
        throw new Error("unexpected mock action " + action);
    }
  };
  bind(b: GuestNetworkBoundary) {
    this.instance = {
      instanceId: resourceId,
      subnetId: b.subnetId,
      vpcId: b.vpcId,
      sourceDestCheck: true,
      instanceState: { name: "running" },
      groupSet: { item: { groupId: b.securityGroupId } },
      networkInterfaceSet: {
        item: {
          networkInterfaceId: "eni-11111111",
          subnetId: b.subnetId,
          vpcId: b.vpcId,
          sourceDestCheck: true,
          groupSet: { item: { groupId: b.securityGroupId } },
        },
      },
      metadataOptions: {
        httpEndpoint: "enabled",
        httpTokens: "required",
        httpPutResponseHopLimit: 1,
        instanceMetadataTags: "disabled",
        httpProtocolIpv6: "disabled",
        state: "applied",
      },
      tagSet: {
        item: [
          { key: "lease", value: leaseId },
          { key: "guest_network_policy_sha256", value: b.policyDigest },
        ],
      },
    };
  }
}
async function prepared() {
  const api = new ControlPlane();
  const control = new AWSGuestNetworkControl(api.query, "example-region-1");
  const custody: GuestNetworkBoundary[] = [];
  const boundary = await control.prepare(policy, leaseId, "subnet-11111111", async (b) => {
    custody.push({ ...b });
  });
  boundary.bootstrapSha256 = await sha256Hex(
    Uint8Array.from(atob(api.userdata), (c) => c.charCodeAt(0)),
  );
  api.bind(boundary);
  return { api, control, boundary, custody };
}

describe("managed AWS guest network enforcement", () => {
  it("persists custody before mutation, removes default egress, and returns observed allocation-bound facts", async () => {
    const api = new ControlPlane();
    const control = new AWSGuestNetworkControl(api.query, "example-region-1");
    const saved: GuestNetworkBoundary[] = [];
    const boundary = await control.prepare(policy, leaseId, "subnet-11111111", async (b) => {
      expect(saved.length > 0 || !api.calls.includes("CreateSecurityGroup")).toBe(true);
      saved.push({ ...b });
    });
    expect(saved).toHaveLength(2);
    expect(saved[0]!.securityGroupId).toBe("");
    expect(saved[1]!.securityGroupId).toBe("sg-11111111");
    expect(api.calls.indexOf("RevokeSecurityGroupEgress")).toBeLessThan(
      api.calls.indexOf("AuthorizeSecurityGroupEgress"),
    );
    boundary.bootstrapSha256 = await sha256Hex(
      Uint8Array.from(atob(api.userdata), (c) => c.charCodeAt(0)),
    );
    api.bind(boundary);
    const receipt = await control.inspect(boundary, policy, resourceId);
    expect(receipt.facts).toMatchObject({
      imds: "roleless-v2",
      instanceProfile: "none",
      bootstrapMode: "ssh-public-key-v1",
    });
    validateGuestNetworkReceipt(
      receipt,
      await guestNetworkPolicyIdentity(policy),
      leaseId,
      "aws",
      resourceId,
    );
    await expect(control.publicHostKey(resourceId)).resolves.toBe(publicKey(2));
  });
  it.each([
    "instance role",
    "wrong group",
    "wrong lease",
    "bootstrap content",
    "metadata tokens",
    "IPv6",
    "policy widened",
  ])("rejects %s before access", async (kind) => {
    const { api, control, boundary } = await prepared();
    if (kind === "instance role") api.instance["iamInstanceProfile"] = { arn: "fixture-role" };
    if (kind === "wrong group") api.instance["groupSet"] = { item: { groupId: "sg-22222222" } };
    if (kind === "wrong lease")
      api.instance["tagSet"] = { item: { key: "lease", value: "cbx_abcdef012345" } };
    if (kind === "bootstrap content") api.userdata = btoa("fixture-secret-must-not-be-logged");
    if (kind === "metadata tokens")
      (api.instance["metadataOptions"] as Record<string, unknown>)["httpTokens"] = "optional";
    if (kind === "IPv6")
      (api.instance["networkInterfaceSet"] as { item: Record<string, unknown> }).item[
        "ipv6AddressesSet"
      ] = { item: { ipv6Address: "::1" } };
    if (kind === "policy widened")
      api.group!["ipPermissionsEgress"] = {
        item: { ipProtocol: "-1", ipRanges: { item: { cidrIp: "0.0.0.0/0" } } },
      };
    await expect(control.inspect(boundary, policy, resourceId)).rejects.toThrow(/guest/);
  });
  it("does not create any resource without durable custody", async () => {
    const api = new ControlPlane();
    await expect(
      new AWSGuestNetworkControl(api.query, "example-region-1").prepare(
        policy,
        leaseId,
        "subnet-11111111",
        async () => {
          throw new Error("journal refused");
        },
      ),
    ).rejects.toThrow("journal refused");
    expect(api.calls).not.toContain("CreateSecurityGroup");
  });
  it("cleans only owned detached groups and confirms deletion", async () => {
    const { api, control, boundary } = await prepared();
    api.attached = true;
    await expect(control.cleanup(boundary)).rejects.toThrow("attached");
    expect(api.calls).not.toContain("DeleteSecurityGroup");
    api.attached = false;
    await control.cleanup(boundary);
    expect(api.group).toBeUndefined();
  });
  it("rejects a synthetic network name rather than treating it as evidence", async () => {
    const api = new ControlPlane();
    await expect(
      new AWSGuestNetworkControl(api.query, "example-region-1").prepare(
        policy,
        leaseId,
        "subnet-review-trusted",
        async () => {},
      ),
    ).rejects.toThrow(/guest/);
    expect(api.calls).not.toContain("CreateSecurityGroup");
  });
});

describe("public-key-only ordinary-image bootstrap", () => {
  it("does not serialize secret-bearing legacy config or key comments", async () => {
    const config = leaseConfig({
      provider: "aws",
      target: "linux",
      os: "ubuntu:24.04",
      sshPublicKey: publicKey() + " fixture-comment-must-not-copy",
    });
    config.guestNetworkPolicy = policy;
    config.tailscale = true;
    config.tailscaleAuthKey = "fixture-organization-secret";
    config.sshHostPrivateKey = "fixture-private-host-key";
    const encoded = await awsRunInstancesUserData(config);
    const text = new TextDecoder().decode(Uint8Array.from(atob(encoded), (c) => c.charCodeAt(0)));
    expect(text).toMatch(/^#cloud-config\n/);
    expect(text).not.toContain("fixture-organization-secret");
    expect(text).not.toContain("fixture-private-host-key");
    expect(text).not.toContain("fixture-comment-must-not-copy");
    const data = JSON.parse(text.slice("#cloud-config\n".length));
    expect(data.users[0].ssh_authorized_keys).toEqual([publicKey()]);
    expect(data.runcmd).toContainEqual(["touch", "/var/lib/crabbox/guest-public-key-ready"]);
    expect(text).toContain("CRABBOX_GUEST_HOST_KEY_V1");
  });
  it("resolves policy from server environment, not caller rules", async () => {
    const sealed = await serverGuestNetworkPolicy({
      CRABBOX_GUEST_NETWORK_POLICY_JSON: JSON.stringify(policy),
    });
    expect(sealed.identity.policyDigest).toHaveLength(64);
    await expect(serverGuestNetworkPolicy({})).rejects.toThrow("operator");
    await expect(
      serverGuestNetworkPolicy({
        CRABBOX_GUEST_NETWORK_POLICY_JSON: JSON.stringify({ ...policy, version: 99 }),
      }),
    ).rejects.toThrow("version");
  });
});

it("shares the retained managed caller contract and canonical digest with Go", async () => {
  const fixture = JSON.parse(
    readFileSync(
      new URL("../../internal/cli/testdata/guest-network-caller-contract.json", import.meta.url),
      "utf8",
    ),
  );
  const identity = await guestNetworkPolicyIdentity(fixture.managedOperatorPolicy);
  expect(identity).toEqual(fixture.managedServerCapability.policy);
  for (const response of [fixture.managedAcquisitionResponse, fixture.managedInspectResponse]) {
    validateGuestNetworkReceipt(
      response.lease.guestNetwork,
      identity,
      response.lease.id,
      response.lease.provider,
      response.lease.cloudID,
    );
  }
});

it("enforces the network before actual EC2 launch and never renders legacy secrets", async () => {
  const api = new ControlPlane();
  const calls: { action: string; params: Record<string, string> }[] = [];
  const client = new EC2SpotClient(
    { AWS_ACCESS_KEY_ID: "synthetic", AWS_SECRET_ACCESS_KEY: "synthetic" },
    "eu-west-1",
  );
  let custody: GuestNetworkBoundary | undefined;
  (
    client as unknown as {
      ec2: (action: string, p: Record<string, string>) => Promise<Record<string, unknown>>;
    }
  ).ec2 = async (action, p) => {
    calls.push({ action, params: p });
    if (action === "DescribeImages")
      return {
        imagesSet: {
          item: {
            imageId: "ami-11111111",
            state: "available",
            architecture: "x86_64",
            name: "ubuntu-stock",
            rootDeviceName: "/dev/sda1",
            blockDeviceMapping: { item: { deviceName: "/dev/sda1", ebs: { volumeSize: 8 } } },
            creationDate: "2026-01-01T00:00:00Z",
          },
        },
      };
    if (action === "DescribeKeyPairs") throw new Error("InvalidKeyPair.NotFound");
    if (action === "ImportKeyPair") return { keyName: "fixture-key", keyPairId: "key-11111111" };
    if (action === "RunInstances") {
      if (!custody?.bootstrapSha256) throw new Error("launch before custody");
      api.userdata = p["UserData"]!;
      api.bind(custody);
      api.instance["instanceType"] = "c7i.2xlarge";
      api.instance["ipAddress"] = "198.51.100.9";
      return { instancesSet: { item: api.instance } };
    }
    if (action === "CreateTags") return {};
    if (action === "DescribeInstanceTypes" || action === "DescribeInstanceTypeOfferings") return {};
    if (action === "DescribeInstances")
      return { requestId: "fixture-request", ...(await api.query(action, p)) };
    return api.query(action, p);
  };
  const cfg = {
    ...leaseConfig({
      provider: "aws",
      target: "linux",
      os: "ubuntu:24.04",
      sshPort: "22",
      sshKeyName: "crabbox-guest-fixture",
      sshPublicKey: publicKey(),
      awsSubnetID: "subnet-11111111",
      tailscale: false,
    }),
    guestNetworkPolicy: policy,
    awsUseStockImage: true,
    sshHostPrivateKey: "LEGACY_PRIVATE_SECRET",
    tailscaleAuthKey: "LEGACY_OVERLAY_SECRET",
  };
  const result = await client.createServerWithFallback(
    cfg,
    leaseId,
    "example",
    "alice@example.com",
    {
      onGuestNetworkBoundary: async (b) => {
        custody = { ...b };
      },
    },
  );
  const launch = calls.find((c) => c.action === "RunInstances")!.params;
  expect(launch["MetadataOptions.HttpTokens"]).toBe("required");
  expect(launch["MetadataOptions.HttpEndpoint"]).toBe("enabled");
  expect(launch["IamInstanceProfile.Name"]).toBeUndefined();
  expect(launch["IamInstanceProfile.Arn"]).toBeUndefined();
  expect(atob(launch["UserData"]!)).not.toMatch(
    /LEGACY_PRIVATE_SECRET|LEGACY_OVERLAY_SECRET|PRIVATE KEY/,
  );
  expect(result.server.guestNetwork?.resourceId).toBe(resourceId);
  expect(result.server.guestNetwork?.facts["sshHostKey"]).toBe(publicKey(2));
  expect(calls.findIndex((c) => c.action === "RevokeSecurityGroupEgress")).toBeLessThan(
    calls.findIndex((c) => c.action === "RunInstances"),
  );
});

it.each(["role", "image", "snapshot", "private", "metadata-mode", "invalid-key"])(
  "rejects %s before any managed provider mutation",
  async (failure) => {
    const client = new EC2SpotClient(
      { AWS_ACCESS_KEY_ID: "synthetic", AWS_SECRET_ACCESS_KEY: "synthetic" },
      "eu-west-1",
    );
    const actions: string[] = [];
    (
      client as unknown as {
        ec2: (action: string, p: Record<string, string>) => Promise<Record<string, unknown>>;
      }
    ).ec2 = async (action) => {
      actions.push(action);
      throw new Error("unexpected provider request");
    };
    const cfg = {
      ...leaseConfig({
        provider: "aws",
        target: "linux",
        os: "ubuntu:24.04",
        sshPort: "22",
        sshPublicKey: publicKey(),
      }),
      guestNetworkPolicy: policy,
    };
    if (failure === "role") cfg.awsProfile = "forbidden-role";
    if (failure === "image") cfg.awsAMI = "ami-custom";
    if (failure === "snapshot") cfg.awsSnapshot = "snapshot-custom";
    if (failure === "private") cfg.awsPrivate = true;
    if (failure === "metadata-mode")
      cfg.guestNetworkPolicy = { ...policy, bootstrapMode: "unknown" };
    if (failure === "invalid-key") cfg.sshPublicKey = "not-a-public-key";
    await expect(
      client.createServerWithFallback(cfg, leaseId, "example", "alice@example.com", {
        onGuestNetworkBoundary: async () => {},
      }),
    ).rejects.toThrow(/guest|public SSH key|roleless public-key bootstrap/);
    expect(actions).toEqual([]);
  },
);

it("persists typed UnauthorizedOperation as no effect, settles without SG calls and permits repaired IAM retry", async () => {
  const api = new ControlPlane();
  let denied = true;
  let previous: GuestNetworkBoundary | undefined;
  let creates = 0;
  const optionsSeen: unknown[] = [];
  const control = new AWSGuestNetworkControl(async (action, p, options) => {
    if (action === "CreateSecurityGroup") {
      creates++;
      optionsSeen.push(options);
      if (denied)
        throw new AWSQueryError(action, 403, "UnauthorizedOperation", "synthetic rejection");
    }
    return api.query(action, p);
  }, "eu-west-1");
  const persist = async (b: GuestNetworkBoundary) => {
    validateGuestNetworkCustodyUpdate(previous, b);
    previous = { ...b };
  };
  await expect(control.prepare(policy, leaseId, "subnet-11111111", persist)).rejects.toThrow(
    "synthetic rejection",
  );
  expect(previous?.createRejectedNoEffect).toBe(true);
  expect(previous?.cleanupConfirmed).toBe(true);
  expect(guestNetworkCreateRejectedNoEffect(previous!)).toBe(true);
  expect(optionsSeen).toEqual([{ singleAttempt: true }]);
  const calls = api.calls.length;
  await control.cleanup(previous!);
  expect(api.calls).toHaveLength(calls);
  const attempt = previous!.createAttemptId;
  denied = false;
  const boundary = await control.prepare(policy, leaseId, "subnet-11111111", persist);
  expect(boundary.securityGroupId).toBe("sg-11111111");
  expect(boundary.createAttemptId).not.toBe(attempt);
  expect(creates).toBe(2);
});
it.each(["timeout", "message", "untyped", "other-action", "other-code"])(
  "keeps %s outcome uncertain and refuses to erase it with a new rejected attempt",
  async (kind) => {
    const api = new ControlPlane();
    let previous: GuestNetworkBoundary | undefined;
    let calls = 0;
    const failure: unknown =
      kind === "timeout"
        ? new Error("request timeout")
        : kind === "message"
          ? new Error("UnauthorizedOperation")
          : kind === "untyped"
            ? { action: "CreateSecurityGroup", status: 403, code: "UnauthorizedOperation" }
            : new AWSQueryError(
                kind === "other-action" ? "AuthorizeSecurityGroupIngress" : "CreateSecurityGroup",
                403,
                kind === "other-code" ? "AccessDenied" : "UnauthorizedOperation",
                "synthetic",
              );
    const control = new AWSGuestNetworkControl(async (action, p) => {
      if (action === "CreateSecurityGroup") {
        calls++;
        throw calls === 1
          ? failure
          : new AWSQueryError(action, 403, "UnauthorizedOperation", "later rejection");
      }
      return api.query(action, p);
    }, "eu-west-1");
    const persist = async (b: GuestNetworkBoundary) => {
      validateGuestNetworkCustodyUpdate(previous, b);
      previous = { ...b };
    };
    await expect(control.prepare(policy, leaseId, "subnet-11111111", persist)).rejects.toBe(
      failure,
    );
    expect(previous?.createRejectedNoEffect).toBeUndefined();
    const retained = { ...previous! };
    await expect(control.prepare(policy, leaseId, "subnet-11111111", persist)).rejects.toThrow(
      "prior attempt",
    );
    expect(calls).toBe(1);
    expect(previous).toEqual(retained);
  },
);
it("does not retire resource-bearing or mismatched custody through a rejection marker", () => {
  const previous: GuestNetworkBoundary = {
    region: "eu-west-1",
    createAttemptId: "first",
    leaseId,
    subnetId: "subnet-11111111",
    vpcId: "vpc-11111111",
    securityGroupId: "sg-11111111",
    securityGroupName: "owned",
    policyDigest: "a".repeat(64),
  };
  expect(() =>
    validateGuestNetworkCustodyUpdate(previous, {
      ...previous,
      securityGroupId: "",
      createRejectedNoEffect: true,
      cleanupConfirmed: true,
    }),
  ).toThrow("matching attempt");
  expect(() =>
    validateGuestNetworkCustodyUpdate(
      { ...previous, securityGroupId: "" },
      {
        ...previous,
        securityGroupId: "",
        createAttemptId: "later",
        createRejectedNoEffect: true,
        cleanupConfirmed: true,
      },
    ),
  ).toThrow("matching attempt");
});

it.each([403, 500])(
  "classifies the actual EC2 HTTP response without retrying CreateSecurityGroup (status=%s)",
  async (status) => {
    let creates = 0;
    const saved: GuestNetworkBoundary[] = [];
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const request = new Request(input, init);
      const fields = new URLSearchParams(await request.text());
      const action = fields.get("Action");
      if (action === "DescribeSubnets")
        return new Response(
          "<DescribeSubnetsResponse><subnetSet><item><subnetId>subnet-11111111</subnetId><vpcId>vpc-11111111</vpcId><state>available</state></item></subnetSet></DescribeSubnetsResponse>",
        );
      if (action === "DescribeVpcs")
        return new Response(
          "<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-11111111</vpcId><state>available</state></item></vpcSet></DescribeVpcsResponse>",
        );
      if (action === "CreateSecurityGroup") {
        creates++;
        return new Response(
          "<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>synthetic rejection</Message></Error></Errors><RequestID>synthetic-request</RequestID></Response>",
          { status },
        );
      }
      throw new Error("unexpected mocked action " + action);
    });
    try {
      const client = new EC2SpotClient(
        { AWS_ACCESS_KEY_ID: "synthetic", AWS_SECRET_ACCESS_KEY: "synthetic" },
        "eu-west-1",
      );
      const control = (
        client as unknown as { guestNetworkControl(): AWSGuestNetworkControl }
      ).guestNetworkControl();
      await expect(
        control.prepare(policy, leaseId, "subnet-11111111", async (b) => {
          saved.push({ ...b });
        }),
      ).rejects.toBeInstanceOf(AWSQueryError);
      expect(creates).toBe(1);
      expect(saved).toHaveLength(status === 403 ? 2 : 1);
      expect(saved.at(-1)?.createRejectedNoEffect === true).toBe(status === 403);
    } finally {
      fetchSpy.mockRestore();
    }
  },
);
