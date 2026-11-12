import assert from "node:assert/strict";
import test from "node:test";
import { fixtureNetwork } from "./fixtures/qualification-network.mjs";
import {
  parseQualificationNetwork,
  qualificationEgress,
  verifyQualificationNetwork,
  verifyQualificationGuest,
  networkDigest,
  qualificationGroupShape,
} from "./image-qualification-network.ts";

test("IPv4 complement deletes every local destination without widening ports", () => {
  const ipv4 = (n) =>
    n
      .split(".")
      .map(Number)
      .reduce((a, b) => a * 256 + b, 0);
  for (const [block, prefix] of [
    ["172.31.0.0/16", 16],
    ["10.0.0.0/8", 8],
    ["192.0.2.0/24", 24],
    ["128.0.0.0/1", 1],
  ]) {
    const start = ipv4(block.split("/")[0]),
      end = start + 2 ** (32 - prefix);
    const ranges = qualificationEgress(block)
      .map((c) => {
        const [a, p] = c.split("/");
        const n = ipv4(a);
        return [n, n + 2 ** (32 - Number(p))];
      })
      .sort((a, b) => a[0] - b[0]);
    assert.equal(ranges.length, prefix);
    assert.equal(
      ranges.reduce((sum, [a, b]) => sum + b - a, 0),
      2 ** 32 - (end - start),
    );
    for (let n = 0; n < ranges.length; n++) {
      const [a, b] = ranges[n];
      assert.ok(b <= start || a >= end);
      if (n) assert.ok(a >= ranges[n - 1][1]);
    }
  }
  for (const block of ["0.0.0.0/0", "172.31.1.0/16", "256.0.0.0/8", "010.0.0.0/8", "192.0.2.0/31"])
    assert.throws(() => qualificationEgress(block));
});

test("legacy exclusive mode requires no network queries; shared mode performs exact read-only queries", async () => {
  await verifyQualificationNetwork({ mode: "exclusive" }, {}, () =>
    assert.fail("unexpected query"),
  );
  const f = await fixtureNetwork();
  const calls = [];
  await verifyQualificationNetwork(f.network, f.scope, async (action, params) => {
    calls.push([action, params]);
    return f.roots[action];
  });
  assert.deepEqual(calls, [
    ["DescribeSubnets", { "SubnetId.1": f.scope.subnetId }],
    ["DescribeRouteTables", { "Filter.1.Name": "vpc-id", "Filter.1.Value.1": f.network.vpcId }],
    [
      "DescribeNetworkAcls",
      { "Filter.1.Name": "association.subnet-id", "Filter.1.Value.1": f.scope.subnetId },
    ],
    ["DescribeSecurityGroups", { "GroupId.1": f.scope.securityGroupId }],
  ]);
  verifyQualificationGuest(f.network, f.scope, f.guest);
});

for (const [name, change] of [
  ["wrong account", (f) => (f.subnet.ownerId = "999999999999")],
  ["wrong VPC", (f) => (f.subnet.vpcId = "vpc-99999999")],
  ["wrong subnet", (f) => (f.subnet.subnetId = "subnet-99999999")],
  [
    "subnet IPv6",
    (f) => (f.subnet.ipv6CidrBlockAssociationSet = { item: [{ ipv6CidrBlock: "2001:db8::/64" }] }),
  ],
  ["IPv6 auto assignment", (f) => (f.subnet.assignIpv6AddressOnCreation = true)],
  [
    "secondary local CIDR",
    (f) =>
      f.table.routeSet.item.push({
        destinationCidrBlock: "10.0.0.0/16",
        gatewayId: "local",
        state: "active",
      }),
  ],
  [
    "private route",
    (f) =>
      (f.table.routeSet.item[1] = {
        destinationCidrBlock: "10.0.0.0/8",
        vpcPeeringConnectionId: "pcx-11111111",
        state: "active",
      }),
  ],
  ["appliance route", (f) => (f.table.routeSet.item[1].networkInterfaceId = "eni-11111111")],
  [
    "additional table",
    (f) => f.roots.DescribeRouteTables.routeTableSet.item.push(structuredClone(f.table)),
  ],
  [
    "explicit subnet association",
    (f) => f.table.associationSet.item.push({ subnetId: f.scope.subnetId }),
  ],
  ["ACL reassociation", (f) => (f.acl.networkAclId = "acl-99999999")],
  ["ACL entries drift", (f) => (f.acl.entrySet.item[0].ruleNumber = 90)],
  [
    "SG egress drift",
    (f) => f.group.ipPermissionsEgress.item[0].ipRanges.item.push({ cidrIp: "0.0.0.0/0" }),
  ],
  ["SG ingress drift", (f) => (f.group.ipPermissions.item[0].fromPort = 80)],
  ["truncated inventory", (f) => (f.roots.DescribeRouteTables.nextToken = "more")],
  [
    "duplicate group",
    (f) => f.roots.DescribeSecurityGroups.securityGroupInfo.item.push(structuredClone(f.group)),
  ],
])
  test(`network rejects ${name}`, async () => {
    const f = await fixtureNetwork();
    change(f);
    await assert.rejects(verifyQualificationNetwork(f.network, f.scope, async (a) => f.roots[a]));
  });

for (const [name, change] of [
  [
    "unrestricted egress",
    (f) => (f.group.ipPermissionsEgress.item[0].ipRanges.item = [{ cidrIp: "0.0.0.0/0" }]),
  ],
  ["extra ports", (f) => (f.group.ipPermissionsEgress.item[0].toPort = 443)],
  [
    "IPv6",
    (f) => (f.group.ipPermissionsEgress.item[0].ipv6Ranges = { item: [{ cidrIpv6: "::/0" }] }),
  ],
  [
    "group reference",
    (f) => (f.group.ipPermissionsEgress.item[0].groups = { item: [{ groupId: "sg-99999999" }] }),
  ],
  [
    "prefix list",
    (f) =>
      (f.group.ipPermissionsEgress.item[0].prefixListIds = {
        item: [{ prefixListId: "pl-11111111" }],
      }),
  ],
  ["other ingress port", (f) => (f.group.ipPermissions.item[0].toPort = 80)],
])
  test(`even an operator digest cannot authorize ${name}`, async () => {
    const f = await fixtureNetwork();
    change(f);
    f.network.securityGroupSha256 = await networkDigest(qualificationGroupShape(f.group));
    await assert.rejects(verifyQualificationNetwork(f.network, f.scope, async (a) => f.roots[a]));
  });

for (const [name, change] of [
  ["extra group", (f) => f.guest.groupSet.item.push({ groupId: "sg-99999999" })],
  [
    "wrong interface group",
    (f) => (f.guest.networkInterfaceSet.item[0].groupSet.item[0].groupId = "sg-99999999"),
  ],
  [
    "extra interface",
    (f) =>
      f.guest.networkInterfaceSet.item.push(structuredClone(f.guest.networkInterfaceSet.item[0])),
  ],
  ["wrong subnet", (f) => (f.guest.subnetId = "subnet-99999999")],
  [
    "instance profile",
    (f) =>
      (f.guest.iamInstanceProfile = {
        arn: "arn:aws:iam::123456789012:instance-profile/Unexpected",
      }),
  ],
  [
    "IPv6 address",
    (f) =>
      (f.guest.networkInterfaceSet.item[0].ipv6AddressesSet = {
        item: [{ ipv6Address: "2001:db8::1" }],
      }),
  ],
  ["optional IMDS token", (f) => (f.guest.metadataOptions.httpTokens = "optional")],
  ["missing response fields", (f) => delete f.guest.groupSet],
])
  test(`guest rejects ${name}`, async () => {
    const f = await fixtureNetwork();
    change(f);
    assert.throws(() => verifyQualificationGuest(f.network, f.scope, f.guest));
  });

test("policy cannot omit mode, add fields, use noncanonical/outside CIDRs or exceed rule bound", async () => {
  const f = await fixtureNetwork();
  assert.deepEqual(parseQualificationNetwork({ mode: "exclusive" }), { mode: "exclusive" });
  for (const p of [
    {},
    { ...f.network, extra: true },
    { ...f.network, subnetCidr: "10.0.0.0/24" },
    { ...f.network, vpcCidr: "172.31.0.0/31" },
    { ...f.network, aclSha256: "" },
  ])
    assert.throws(() => parseQualificationNetwork(p));
});
