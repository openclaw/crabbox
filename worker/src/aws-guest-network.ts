import { isAWSGuestCreateRejectedNoEffect } from "./aws-query-error";
import { sha256Hex } from "./encoding";
import {
  guestNetworkPolicyIdentity,
  guestNetworkCreateRejectedNoEffect,
  normalizeGuestSSHPublicKey,
  guestPublicKeyBootstrap,
  requireAWSGuestPolicy,
  type GuestNetworkBoundary,
  type GuestNetworkPolicy,
  type GuestNetworkReceipt,
} from "./guest-network";

export type GuestAWSQuery = (
  action: string,
  params: Record<string, string>,
  options?: { singleAttempt?: true },
) => Promise<Record<string, unknown>>;
const obj = (v: unknown): Record<string, unknown> =>
  v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
const list = (v: unknown): Record<string, unknown>[] =>
  v === undefined ? [] : (Array.isArray(v) ? v : [v]).map(obj);
const str = (v: unknown): string =>
  typeof v === "string" ? v : typeof v === "number" ? String(v) : "";
const bool = (v: unknown): boolean => v === true || v === "true";
const valid = (value: string, kind: string): boolean =>
  new RegExp("^" + kind + "-([0-9a-f]{8}|[0-9a-f]{17})$").test(value);
const tags = (b: GuestNetworkBoundary): Record<string, string> => ({
  "crabbox-guest-boundary": "v1",
  "crabbox-guest-lease": b.leaseId,
  "crabbox-guest-policy": b.policyDigest,
});
function tagMap(value: unknown): Record<string, string> {
  const result: Record<string, string> = {};
  for (const t of list(obj(value)["item"])) {
    const k = str(t["key"]);
    if (k in result) throw new Error("ambiguous guest boundary ownership");
    result[k] = str(t["value"]);
  }
  return result;
}
function ruleParams(rules: { cidr: string; port: number }[]): Record<string, string> {
  const result: Record<string, string> = {};
  rules.forEach((r, i) => {
    const p = "IpPermissions." + (i + 1);
    Object.assign(result, {
      [p + ".IpProtocol"]: "tcp",
      [p + ".FromPort"]: String(r.port),
      [p + ".ToPort"]: String(r.port),
      [p + ".IpRanges.1.CidrIp"]: r.cidr,
    });
  });
  return result;
}
function normalizedRules(value: unknown): string[] {
  const result: string[] = [];
  for (const p of list(obj(value)["item"])) {
    if (
      str(p["ipProtocol"]) !== "tcp" ||
      str(p["fromPort"]) !== str(p["toPort"]) ||
      list(obj(p["ipv6Ranges"])["item"]).length ||
      list(obj(p["groups"])["item"]).length ||
      list(obj(p["prefixListIds"])["item"]).length
    )
      throw new Error("unexpected guest security group permission");
    const ranges = list(obj(p["ipRanges"])["item"]);
    if (!ranges.length) throw new Error("missing guest security group range");
    for (const r of ranges) result.push(str(r["cidrIp"]) + ":" + str(p["fromPort"]));
  }
  return result.toSorted();
}
function expectedRules(
  policy: GuestNetworkPolicy,
  ingress: boolean,
): { cidr: string; port: number }[] {
  return ingress
    ? policy.sshSources.map((cidr) => ({ cidr, port: 22 }))
    : policy.egress.flatMap((r) => r.ports.map((port) => ({ cidr: r.cidr, port })));
}

// All facts are EC2 API observations. Console output is used only to bind the
// public SSH host key, never to attest network policy or grant credentials.
export class AWSGuestNetworkControl {
  constructor(
    private readonly query: GuestAWSQuery,
    private readonly region: string,
  ) {}
  async prepare(
    policy: GuestNetworkPolicy,
    leaseId: string,
    subnetId: string,
    persist: (b: GuestNetworkBoundary) => Promise<void>,
  ): Promise<GuestNetworkBoundary> {
    requireAWSGuestPolicy(policy, true);
    const identity = await guestNetworkPolicyIdentity(policy);
    if (!/^cbx_[0-9a-f]{12}$/.test(leaseId))
      throw new Error("guest allocation lease identity invalid");
    if (!subnetId) {
      const vpcs = await this.query("DescribeVpcs", {
        "Filter.1.Name": "is-default",
        "Filter.1.Value.1": "true",
      });
      const vs = list(obj(vpcs["vpcSet"])["item"]);
      if (vs.length !== 1) throw new Error("guest default VPC unavailable");
      const subnets = await this.query("DescribeSubnets", {
        "Filter.1.Name": "vpc-id",
        "Filter.1.Value.1": str(vs[0]!["vpcId"]),
        "Filter.2.Name": "default-for-az",
        "Filter.2.Value.1": "true",
      });
      const available = list(obj(subnets["subnetSet"])["item"])
        .filter((s) => str(s["state"]) === "available")
        .toSorted((a, b) => str(a["subnetId"]).localeCompare(str(b["subnetId"])));
      if (!available.length) throw new Error("guest default subnet unavailable");
      subnetId = str(available[0]!["subnetId"]);
    }
    if (!valid(subnetId, "subnet")) throw new Error("actual guest subnet ID required");
    const subnets = await this.query("DescribeSubnets", { "SubnetId.1": subnetId });
    const ss = list(obj(subnets["subnetSet"])["item"]);
    if (
      ss.length !== 1 ||
      str(ss[0]!["subnetId"]) !== subnetId ||
      str(ss[0]!["state"]) !== "available" ||
      !valid(str(ss[0]!["vpcId"]), "vpc")
    )
      throw new Error("guest subnet observation mismatch");
    const vpcId = str(ss[0]!["vpcId"]);
    const vpcs = await this.query("DescribeVpcs", { "VpcId.1": vpcId });
    const vs = list(obj(vpcs["vpcSet"])["item"]);
    if (vs.length !== 1 || str(vs[0]!["vpcId"]) !== vpcId || str(vs[0]!["state"]) !== "available")
      throw new Error("guest VPC observation mismatch");
    const name =
      "crabbox-guest-" +
      (
        await sha256Hex(
          leaseId +
            String.fromCharCode(0) +
            subnetId +
            String.fromCharCode(0) +
            identity.policyDigest,
        )
      ).slice(0, 24);
    const b: GuestNetworkBoundary = {
      region: this.region,
      leaseId,
      subnetId,
      vpcId,
      securityGroupId: "",
      createAttemptId: crypto.randomUUID(),
      securityGroupName: name,
      policyDigest: identity.policyDigest,
      ...(policy.bootstrapMode ? { bootstrapMode: policy.bootstrapMode } : {}),
    };
    await persist({ ...b }); // durable cleanup custody precedes an API without idempotency tokens
    const params: Record<string, string> = {
      VpcId: vpcId,
      GroupName: name,
      GroupDescription: "Dedicated operator guest network boundary",
      "TagSpecification.1.ResourceType": "security-group",
    };
    Object.entries(tags(b)).forEach(([key, value], i) => {
      params["TagSpecification.1.Tag." + (i + 1) + ".Key"] = key;
      params["TagSpecification.1.Tag." + (i + 1) + ".Value"] = value;
    });
    try {
      const created = await this.query("CreateSecurityGroup", params, { singleAttempt: true });
      b.securityGroupId = str(created["groupId"]);
      if (!valid(b.securityGroupId, "sg"))
        throw new Error("guest group creation outcome unresolved; retain cleanup custody");
      await persist({ ...b });
      const group = await this.group(b);
      if (list(obj(group["ipPermissions"])["item"]).length)
        throw new Error("new guest group has unexpected ingress");
      const old = list(obj(group["ipPermissionsEgress"])["item"]);
      if (old.length) {
        const revoke: Record<string, string> = { GroupId: b.securityGroupId };
        old.forEach((p, i) => {
          const n = "IpPermissions." + (i + 1);
          revoke[n + ".IpProtocol"] = str(p["ipProtocol"]);
          if (p["fromPort"] !== undefined) revoke[n + ".FromPort"] = str(p["fromPort"]);
          if (p["toPort"] !== undefined) revoke[n + ".ToPort"] = str(p["toPort"]);
          list(obj(p["ipRanges"])["item"]).forEach((r, j) => {
            revoke[n + ".IpRanges." + (j + 1) + ".CidrIp"] = str(r["cidrIp"]);
          });
          list(obj(p["ipv6Ranges"])["item"]).forEach((r, j) => {
            revoke[n + ".Ipv6Ranges." + (j + 1) + ".CidrIpv6"] = str(r["cidrIpv6"]);
          });
        });
        await this.query("RevokeSecurityGroupEgress", revoke);
      }
      const ingress = expectedRules(policy, true),
        egress = expectedRules(policy, false);
      if (ingress.length)
        await this.query("AuthorizeSecurityGroupIngress", {
          GroupId: b.securityGroupId,
          ...ruleParams(ingress),
        });
      if (egress.length)
        await this.query("AuthorizeSecurityGroupEgress", {
          GroupId: b.securityGroupId,
          ...ruleParams(egress),
        });
      await this.policy(b, policy);
      return b;
    } catch (error) {
      if (!b.securityGroupId && isAWSGuestCreateRejectedNoEffect(error)) {
        b.createRejectedNoEffect = true;
        b.cleanupConfirmed = true;
        await persist({ ...b });
        throw error;
      }

      if (b.securityGroupId) {
        try {
          await this.cleanup(b);
        } catch {
          throw new Error("guest network creation failed and owned cleanup remains pending");
        }
      }
      throw error;
    }
  }
  private async group(b: GuestNetworkBoundary): Promise<Record<string, unknown>> {
    if (
      !valid(b.securityGroupId, "sg") ||
      !valid(b.subnetId, "subnet") ||
      !valid(b.vpcId, "vpc") ||
      !b.leaseId ||
      b.policyDigest.length !== 64
    )
      throw new Error("guest boundary cleanup/inspection identity missing");
    const response = await this.query("DescribeSecurityGroups", { "GroupId.1": b.securityGroupId });
    const groups = list(obj(response["securityGroupInfo"])["item"]);
    if (response["nextToken"] || groups.length !== 1)
      throw new Error("guest group observation absent or ambiguous");
    const g = groups[0]!;
    if (
      str(g["groupId"]) !== b.securityGroupId ||
      str(g["vpcId"]) !== b.vpcId ||
      str(g["groupName"]) !== b.securityGroupName
    )
      throw new Error("guest group resource binding mismatch");
    const observed = tagMap(g["tagSet"]);
    for (const [k, v] of Object.entries(tags(b)))
      if (observed[k] !== v) throw new Error("guest group ownership mismatch");
    return g;
  }
  private async policy(b: GuestNetworkBoundary, policy: GuestNetworkPolicy): Promise<void> {
    const identity = await guestNetworkPolicyIdentity(policy);
    if (identity.policyDigest !== b.policyDigest)
      throw new Error("guest effective policy digest mismatch");
    const g = await this.group(b);
    for (const [field, ingress] of [
      ["ipPermissions", true],
      ["ipPermissionsEgress", false],
    ] as const) {
      const expected = expectedRules(policy, ingress)
        .map((r) => r.cidr + ":" + r.port)
        .toSorted();
      if (JSON.stringify(normalizedRules(g[field])) !== JSON.stringify(expected))
        throw new Error("guest effective security group policy mismatch");
    }
  }
  async inspect(
    b: GuestNetworkBoundary,
    policy: GuestNetworkPolicy,
    resourceId: string,
  ): Promise<GuestNetworkReceipt> {
    requireAWSGuestPolicy(policy, true);
    if (
      !valid(resourceId, "i") ||
      b.bootstrapMode !== guestPublicKeyBootstrap ||
      !b.bootstrapSha256
    )
      throw new Error("guest allocation/bootstrap binding missing");
    await this.policy(b, policy);
    const root = await this.query("DescribeInstances", { "InstanceId.1": resourceId });
    const reservations = list(obj(root["reservationSet"])["item"]);
    const instances = reservations.flatMap((r) => list(obj(r["instancesSet"])["item"]));
    if (root["nextToken"] || instances.length !== 1)
      throw new Error("guest instance observation absent or ambiguous");
    const i = instances[0]!;
    const groups = list(obj(i["groupSet"])["item"]),
      nics = list(obj(i["networkInterfaceSet"])["item"]),
      meta = obj(i["metadataOptions"]),
      state = obj(i["instanceState"]),
      instanceTags = tagMap(i["tagSet"]);
    if (
      str(i["instanceId"]) !== resourceId ||
      str(i["subnetId"]) !== b.subnetId ||
      str(i["vpcId"]) !== b.vpcId ||
      groups.length !== 1 ||
      str(groups[0]!["groupId"]) !== b.securityGroupId ||
      nics.length !== 1 ||
      !bool(i["sourceDestCheck"]) ||
      instanceTags["lease"] !== b.leaseId ||
      instanceTags["guest_network_policy_sha256"] !== b.policyDigest ||
      i["iamInstanceProfile"] !== undefined ||
      !["pending", "running"].includes(str(state["name"]))
    )
      throw new Error("guest actual instance/lease/role binding mismatch");
    if (
      str(meta["httpEndpoint"]) !== "enabled" ||
      str(meta["httpTokens"]) !== "required" ||
      str(meta["state"]) !== "applied" ||
      str(meta["httpPutResponseHopLimit"]) !== "1" ||
      str(meta["instanceMetadataTags"]) !== "disabled" ||
      str(meta["httpProtocolIpv6"]) !== "disabled"
    )
      throw new Error("guest roleless metadata policy mismatch");
    const nic = nics[0]!,
      ng = list(obj(nic["groupSet"])["item"]);
    if (
      !valid(str(nic["networkInterfaceId"]), "eni") ||
      str(nic["subnetId"]) !== b.subnetId ||
      str(nic["vpcId"]) !== b.vpcId ||
      ng.length !== 1 ||
      str(ng[0]!["groupId"]) !== b.securityGroupId ||
      list(obj(nic["ipv6AddressesSet"])["item"]).length ||
      !bool(nic["sourceDestCheck"])
    )
      throw new Error("guest interface binding mismatch");
    const attribute = await this.query("DescribeInstanceAttribute", {
      InstanceId: resourceId,
      Attribute: "userData",
    });
    if (str(attribute["instanceId"]) !== resourceId)
      throw new Error("guest bootstrap resource mismatch");
    let bytes: Uint8Array;
    try {
      bytes = Uint8Array.from(atob(str(obj(attribute["userData"])["value"])), (c) =>
        c.charCodeAt(0),
      );
    } catch {
      throw new Error("guest bootstrap observation invalid");
    }
    if ((await sha256Hex(bytes)) !== b.bootstrapSha256)
      throw new Error("guest public-key bootstrap content mismatch");
    await this.policy(b, policy);
    const identity = await guestNetworkPolicyIdentity(policy);
    return {
      ...identity,
      leaseId: b.leaseId,
      provider: "aws",
      resourceId,
      boundaryId: b.securityGroupId,
      facts: {
        subnetId: b.subnetId,
        vpcId: b.vpcId,
        networkInterfaceId: str(nic["networkInterfaceId"]),
        imds: "roleless-v2",
        instanceProfile: "none",
        ipv6: "absent",
        egress: "operator-public-ipv4-tcp-allowlist",
        amazonProvidedDNS: "aws-resolver-exception",
        platformExceptionsPolicy: "aws-platform-services-v1",
        bootstrapMode: guestPublicKeyBootstrap,
        bootstrapSha256: b.bootstrapSha256,
        ...(instanceTags["guest_network_ssh_host_key"]
          ? { sshHostKey: normalizeGuestSSHPublicKey(instanceTags["guest_network_ssh_host_key"]) }
          : {}),
      },
    };
  }
  async ownsInstance(b: GuestNetworkBoundary, resourceId: string): Promise<boolean> {
    if (!valid(resourceId, "i")) return false;
    const root = await this.query("DescribeInstances", { "InstanceId.1": resourceId });
    const instances = list(obj(root["reservationSet"])["item"]).flatMap((r) =>
      list(obj(r["instancesSet"])["item"]),
    );
    if (
      root["nextToken"] ||
      instances.length !== 1 ||
      str(instances[0]!["instanceId"]) !== resourceId
    )
      return false;
    const observed = tagMap(instances[0]!["tagSet"]);
    return (
      observed["lease"] === b.leaseId && observed["guest_network_policy_sha256"] === b.policyDigest
    );
  }
  async publicHostKey(resourceId: string): Promise<string> {
    if (!valid(resourceId, "i")) throw new Error("guest SSH host-key resource binding missing");
    const output = await this.query("GetConsoleOutput", { InstanceId: resourceId, Latest: "true" });
    if (str(output["instanceId"]) !== resourceId)
      throw new Error("guest console resource mismatch");
    let text: string;
    try {
      text = atob(str(output["output"]));
    } catch {
      throw new Error("guest console output invalid");
    }
    const keys = [
      ...text.matchAll(/CRABBOX_GUEST_HOST_KEY_V1 (ssh-ed25519 [A-Za-z0-9+/]+={0,3})/g),
    ].map((m) => m[1]!);
    const unique = [...new Set(keys)];
    if (unique.length !== 1) throw new Error("guest public SSH host key not yet uniquely observed");
    return normalizeGuestSSHPublicKey(unique[0]!);
  }
  async cleanup(b: GuestNetworkBoundary): Promise<void> {
    if (b.createRejectedNoEffect) {
      if (!guestNetworkCreateRejectedNoEffect(b))
        throw new Error("invalid definitive guest rejection custody");
      return;
    }
    if (!b.securityGroupId) {
      if (!b.securityGroupName) throw new Error("guest cleanup custody missing");
      const root = await this.query("DescribeSecurityGroups", {
        "Filter.1.Name": "group-name",
        "Filter.1.Value.1": b.securityGroupName,
        "Filter.2.Name": "vpc-id",
        "Filter.2.Value.1": b.vpcId,
      });
      const groups = list(obj(root["securityGroupInfo"])["item"]);
      if (root["nextToken"] || groups.length !== 1)
        throw new Error("guest group create outcome unresolved; retain custody");
      b = { ...b, securityGroupId: str(groups[0]!["groupId"]) };
    }
    try {
      await this.group(b);
    } catch (error) {
      if (obj(error)["code"] === "InvalidGroup.NotFound") return;
      throw error;
    }
    const nics = await this.query("DescribeNetworkInterfaces", {
      "Filter.1.Name": "group-id",
      "Filter.1.Value.1": b.securityGroupId,
    });
    if (nics["nextToken"] || list(obj(nics["networkInterfaceSet"])["item"]).length)
      throw new Error("guest boundary still attached or attachment observation incomplete");
    await this.query("DeleteSecurityGroup", { GroupId: b.securityGroupId });
    try {
      await this.group(b);
    } catch (error) {
      if (obj(error)["code"] === "InvalidGroup.NotFound") return;
      throw error;
    }
    throw new Error("guest boundary deletion not confirmed");
  }
}
