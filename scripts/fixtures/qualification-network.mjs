// Synthetic management responses only; never cloud credentials or real targets.
import {
  networkDigest,
  qualificationAclShape,
  qualificationEgress,
  qualificationGroupShape,
} from "../image-qualification-network.ts";

export async function fixtureNetwork(
  scope = {
    accountId: "123456789012",
    subnetId: "subnet-00000000000000000",
    securityGroupId: "sg-00000000000000000",
  },
) {
  const network = {
    mode: "shared-subnet",
    vpcId: "vpc-11111111",
    vpcCidr: "172.31.0.0/16",
    subnetCidr: "172.31.48.0/20",
    routeTableId: "rtb-11111111",
    internetGatewayId: "igw-11111111",
    networkAclId: "acl-11111111",
  };
  const subnet = {
    subnetId: scope.subnetId,
    vpcId: network.vpcId,
    ownerId: scope.accountId,
    cidrBlock: network.subnetCidr,
    state: "available",
    mapPublicIpOnLaunch: true,
    assignIpv6AddressOnCreation: false,
    ipv6Native: false,
    ipv6CidrBlockAssociationSet: "",
  };
  const table = {
    routeTableId: network.routeTableId,
    vpcId: network.vpcId,
    ownerId: scope.accountId,
    associationSet: { item: [{ main: true, routeTableId: network.routeTableId }] },
    propagatingVgwSet: "",
    routeSet: {
      item: [
        {
          destinationCidrBlock: network.vpcCidr,
          gatewayId: "local",
          state: "active",
          origin: "CreateRouteTable",
        },
        {
          destinationCidrBlock: "0.0.0.0/0",
          gatewayId: network.internetGatewayId,
          state: "active",
          origin: "CreateRoute",
        },
      ],
    },
  };
  const acl = {
    networkAclId: network.networkAclId,
    vpcId: network.vpcId,
    ownerId: scope.accountId,
    associationSet: { item: [{ subnetId: scope.subnetId, networkAclId: network.networkAclId }] },
    entrySet: {
      item: [true, false].flatMap((egress) => [
        { ruleNumber: 100, protocol: -1, ruleAction: "allow", egress, cidrBlock: "0.0.0.0/0" },
        { ruleNumber: 32767, protocol: -1, ruleAction: "deny", egress, cidrBlock: "0.0.0.0/0" },
      ]),
    },
  };
  const group = {
    groupId: scope.securityGroupId,
    vpcId: network.vpcId,
    ownerId: scope.accountId,
    ipPermissions: {
      item: [
        {
          ipProtocol: "tcp",
          fromPort: 22,
          toPort: 22,
          ipRanges: { item: [{ cidrIp: "0.0.0.0/0" }] },
        },
      ],
    },
    ipPermissionsEgress: {
      item: [80, 443].map((port) => ({
        ipProtocol: "tcp",
        fromPort: port,
        toPort: port,
        ipRanges: { item: qualificationEgress(network.vpcCidr).map((cidrIp) => ({ cidrIp })) },
      })),
    },
  };
  network.aclSha256 = await networkDigest(qualificationAclShape(acl));
  network.securityGroupSha256 = await networkDigest(qualificationGroupShape(group));
  const roots = {
    DescribeSubnets: { subnetSet: { item: [subnet] } },
    DescribeRouteTables: { routeTableSet: { item: [table] } },
    DescribeNetworkAcls: { networkAclSet: { item: [acl] } },
    DescribeSecurityGroups: { securityGroupInfo: { item: [group] } },
  };
  const guest = {
    instanceId: "i-owned",
    vpcId: network.vpcId,
    subnetId: scope.subnetId,
    groupSet: { item: [{ groupId: scope.securityGroupId }] },
    networkInterfaceSet: {
      item: [
        {
          vpcId: network.vpcId,
          subnetId: scope.subnetId,
          groupSet: { item: [{ groupId: scope.securityGroupId }] },
        },
      ],
    },
    instanceState: { name: "running" },
    metadataOptions: {
      httpTokens: "required",
      httpEndpoint: "enabled",
      httpPutResponseHopLimit: 1,
      instanceMetadataTags: "disabled",
    },
  };
  return { network, scope, roots, subnet, table, acl, group, guest };
}
