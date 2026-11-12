// Shared by protected Node tooling (Node 24 type stripping) and the private
// qualification authority. Keep outside worker/src: the build replaces that
// directory with candidate source data before running protected tooling again.
export type QualificationNetwork =
  | { mode: "exclusive" }
  | {
      mode: "shared-subnet";
      vpcId: string;
      vpcCidr: string;
      subnetCidr: string;
      routeTableId: string;
      internetGatewayId: string;
      networkAclId: string;
      securityGroupSha256: string;
      aclSha256: string;
    };

type Row = Record<string, unknown>;
type Scope = { accountId: string; subnetId: string; securityGroupId: string };
type Query = (action: string, parameters: Record<string, string>) => Promise<Row>;
function fail(): never {
  throw new Error("AWS qualification network admission failed");
}
const row = (v: unknown): Row =>
  v && typeof v === "object" && !Array.isArray(v) ? (v as Row) : {};
const list = (v: unknown): Row[] =>
  v === undefined || v === "" ? [] : (Array.isArray(v) ? v : [v]).map(row);
const text = (v: unknown) => (v === undefined ? "" : String(v));
const empty = (v: unknown) =>
  Object.keys(row(v)).length === 0 &&
  !Array.isArray(v) &&
  (v === undefined || v === "" || (v !== null && typeof v === "object"));

// Sort unordered AWS sets as well as object keys; descriptions, tags and request
// IDs are not security coordinates and are intentionally absent from the shapes.
export function networkCanonical(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(networkCanonical).toSorted().join(",")}]`;
  if (v && typeof v === "object")
    return `{${Object.keys(v)
      .toSorted()
      .map((k) => `${JSON.stringify(k)}:${networkCanonical(row(v)[k])}`)
      .join(",")}}`;
  return JSON.stringify(v) ?? "null";
}
export async function networkDigest(v: unknown): Promise<string> {
  return Array.from(
    new Uint8Array(
      await crypto.subtle.digest("SHA-256", new TextEncoder().encode(networkCanonical(v))),
    ),
    (b) => b.toString(16).padStart(2, "0"),
  ).join("");
}

function cidr(value: unknown): { start: number; size: number; prefix: number } {
  if (typeof value !== "string" || !/^(\d{1,3}\.){3}\d{1,3}\/(\d|[12]\d|3[0-2])$/.test(value))
    fail();
  const [address, bits] = value.split("/");
  const octets = address!.split(".").map(Number);
  const prefix = Number(bits);
  const start = octets.reduce((n, part) => n * 256 + part, 0);
  const size = 2 ** (32 - prefix);
  if (
    octets.some((n) => n > 255) ||
    start % size !== 0 ||
    `${octets.join(".")}/${prefix}` !== value
  )
    fail();
  return { start, size, prefix };
}

// The complement of one canonical VPC CIDR needs prefix-length allowances per
// port. Cap at 60 total IPv4 rules; do not widen the port range to save rules.
export function qualificationEgress(vpcCidr: string): string[] {
  const { start, prefix } = cidr(vpcCidr);
  if (prefix < 1 || prefix > 30) fail();
  const result: string[] = [];
  for (let bit = 1; bit <= prefix; bit++) {
    const size = 2 ** (32 - bit);
    const sibling =
      Math.floor(start / size) % 2 === 0
        ? Math.floor(start / size) * size + size
        : Math.floor(start / size) * size - size;
    result.push(
      `${[24, 16, 8, 0].map((shift) => Math.floor(sibling / 2 ** shift) % 256).join(".")}/${bit}`,
    );
  }
  return result.toSorted();
}

export function parseQualificationNetwork(value: unknown): QualificationNetwork {
  const v = row(value);
  const keys = Object.keys(v).toSorted().join(",");
  if (v["mode"] === "exclusive" && keys === "mode") return { mode: "exclusive" };
  if (
    v["mode"] !== "shared-subnet" ||
    keys !==
      "aclSha256,internetGatewayId,mode,networkAclId,routeTableId,securityGroupSha256,subnetCidr,vpcCidr,vpcId"
  )
    fail();
  for (const [key, prefix] of [
    ["vpcId", "vpc"],
    ["routeTableId", "rtb"],
    ["internetGatewayId", "igw"],
    ["networkAclId", "acl"],
  ]) {
    if (!new RegExp(`^${prefix}-[0-9a-f]+$`).test(text(v[key!]))) fail();
  }
  for (const key of ["aclSha256", "securityGroupSha256"])
    if (!/^[0-9a-f]{64}$/.test(text(v[key]))) fail();
  const parent = cidr(v["vpcCidr"]),
    child = cidr(v["subnetCidr"]);
  qualificationEgress(text(v["vpcCidr"]));
  if (child.start < parent.start || child.start + child.size > parent.start + parent.size) fail();
  return v as QualificationNetwork;
}

function permissionShape(value: unknown): Row[] {
  return list(row(value)["item"]).map((p) => ({
    protocol: text(p["ipProtocol"]),
    from: text(p["fromPort"]),
    to: text(p["toPort"]),
    ipv4: list(row(p["ipRanges"])["item"]).map((r) => text(r["cidrIp"])),
    ipv6: list(row(p["ipv6Ranges"])["item"]),
    groups: list(row(p["groups"])["item"]),
    prefixLists: list(row(p["prefixListIds"])["item"]),
  }));
}
export function qualificationGroupShape(group: Row): Row {
  return {
    id: text(group["groupId"]),
    vpc: text(group["vpcId"]),
    owner: text(group["ownerId"]),
    ingress: permissionShape(group["ipPermissions"]),
    egress: permissionShape(group["ipPermissionsEgress"]),
  };
}
export function qualificationAclShape(acl: Row): Row {
  return {
    id: text(acl["networkAclId"]),
    vpc: text(acl["vpcId"]),
    owner: text(acl["ownerId"]),
    entries: list(row(acl["entrySet"])["item"]),
  };
}
function single(root: Row, set: string): Row {
  const values = list(row(root[set])["item"]);
  if (text(root["nextToken"]) || values.length !== 1) fail();
  return values[0]!;
}

export async function verifyQualificationNetwork(
  network: QualificationNetwork,
  scope: Scope,
  query: Query,
): Promise<void> {
  if (network.mode === "exclusive") return;
  // Revalidate persisted policy too. No route inventory pagination or topology
  // expansion: this admission is deliberately one main table / one local CIDR.
  parseQualificationNetwork(network);
  const subnet = single(
    await query("DescribeSubnets", { "SubnetId.1": scope.subnetId }),
    "subnetSet",
  );
  if (
    text(subnet["subnetId"]) !== scope.subnetId ||
    text(subnet["vpcId"]) !== network.vpcId ||
    text(subnet["ownerId"]) !== scope.accountId ||
    subnet["cidrBlock"] !== network.subnetCidr ||
    subnet["state"] !== "available" ||
    text(subnet["mapPublicIpOnLaunch"]) !== "true" ||
    text(subnet["assignIpv6AddressOnCreation"]) !== "false" ||
    text(subnet["ipv6Native"]) !== "false" ||
    list(row(subnet["ipv6CidrBlockAssociationSet"])["item"]).length
  )
    fail();
  const table = single(
    await query("DescribeRouteTables", {
      "Filter.1.Name": "vpc-id",
      "Filter.1.Value.1": network.vpcId,
    }),
    "routeTableSet",
  );
  const associations = list(row(table["associationSet"])["item"]);
  const routes = list(row(table["routeSet"])["item"]);
  if (
    text(table["routeTableId"]) !== network.routeTableId ||
    text(table["vpcId"]) !== network.vpcId ||
    text(table["ownerId"]) !== scope.accountId ||
    associations.length !== 1 ||
    text(associations[0]!["main"]) !== "true" ||
    text(associations[0]!["routeTableId"]) !== network.routeTableId ||
    text(associations[0]!["subnetId"]) ||
    !empty(table["propagatingVgwSet"]) ||
    routes.length !== 2
  )
    fail();
  for (const route of routes) {
    const local =
      route["destinationCidrBlock"] === network.vpcCidr && route["gatewayId"] === "local";
    const internet =
      route["destinationCidrBlock"] === "0.0.0.0/0" &&
      route["gatewayId"] === network.internetGatewayId;
    if (
      (!local && !internet) ||
      route["state"] !== "active" ||
      Object.keys(route).some(
        (k) => !["destinationCidrBlock", "gatewayId", "state", "origin"].includes(k),
      )
    )
      fail();
  }
  if (
    !routes.some((r) => r["gatewayId"] === "local") ||
    !routes.some((r) => r["gatewayId"] === network.internetGatewayId)
  )
    fail();
  const acl = single(
    await query("DescribeNetworkAcls", {
      "Filter.1.Name": "association.subnet-id",
      "Filter.1.Value.1": scope.subnetId,
    }),
    "networkAclSet",
  );
  if (
    text(acl["networkAclId"]) !== network.networkAclId ||
    text(acl["vpcId"]) !== network.vpcId ||
    text(acl["ownerId"]) !== scope.accountId ||
    list(row(acl["associationSet"])["item"]).filter(
      (a) => a["subnetId"] === scope.subnetId && a["networkAclId"] === network.networkAclId,
    ).length !== 1 ||
    (await networkDigest(qualificationAclShape(acl))) !== network.aclSha256
  )
    fail();
  const group = single(
    await query("DescribeSecurityGroups", { "GroupId.1": scope.securityGroupId }),
    "securityGroupInfo",
  );
  const shape = qualificationGroupShape(group);
  if (
    shape["id"] !== scope.securityGroupId ||
    shape["vpc"] !== network.vpcId ||
    shape["owner"] !== scope.accountId ||
    (await networkDigest(shape)) !== network.securityGroupSha256
  )
    fail();
  const egress = shape["egress"] as Row[];
  const allowed = qualificationEgress(network.vpcCidr);
  // AWS may group many CIDRs in a permission or split them. Compare the exact
  // permission tuples, rejecting duplicates and additional destination kinds.
  const tuples: string[] = [];
  for (const permission of egress) {
    if (
      permission["protocol"] !== "tcp" ||
      permission["from"] !== permission["to"] ||
      !["80", "443"].includes(String(permission["from"])) ||
      (permission["ipv6"] as unknown[]).length ||
      (permission["groups"] as unknown[]).length ||
      (permission["prefixLists"] as unknown[]).length ||
      !(permission["ipv4"] as string[]).length
    )
      fail();
    for (const dest of permission["ipv4"] as string[]) tuples.push(`${permission["from"]}:${dest}`);
  }
  if (
    networkCanonical(tuples) !==
    networkCanonical(["80", "443"].flatMap((port) => allowed.map((dest) => `${port}:${dest}`)))
  )
    fail();
  for (const permission of shape["ingress"] as Row[]) {
    if (
      permission["protocol"] !== "tcp" ||
      permission["from"] !== "22" ||
      permission["to"] !== "22" ||
      (permission["ipv6"] as unknown[]).length ||
      (permission["groups"] as unknown[]).length ||
      (permission["prefixLists"] as unknown[]).length
    )
      fail();
    for (const source of permission["ipv4"] as string[]) cidr(source);
  }
}

export function verifyQualificationGuest(
  network: QualificationNetwork,
  scope: Scope,
  instance: Row,
): void {
  if (network.mode === "exclusive") return;
  const exactGroup = (v: unknown) => {
    const groups = list(row(v)["item"]);
    return groups.length === 1 && text(groups[0]!["groupId"]) === scope.securityGroupId;
  };
  const interfaces = list(row(instance["networkInterfaceSet"])["item"]);
  const metadata = row(instance["metadataOptions"]);
  if (
    instance["vpcId"] !== network.vpcId ||
    instance["subnetId"] !== scope.subnetId ||
    !exactGroup(instance["groupSet"]) ||
    !empty(instance["iamInstanceProfile"]) ||
    interfaces.length !== 1 ||
    !["pending", "running"].includes(text(row(instance["instanceState"])["name"])) ||
    text(metadata["httpTokens"]) !== "required" ||
    text(metadata["httpEndpoint"]) !== "enabled" ||
    text(metadata["httpPutResponseHopLimit"]) !== "1" ||
    text(metadata["instanceMetadataTags"]) !== "disabled" ||
    list(row(instance["ipv6AddressesSet"])["item"]).length
  )
    fail();
  const nic = interfaces[0]!;
  if (
    nic["vpcId"] !== network.vpcId ||
    nic["subnetId"] !== scope.subnetId ||
    !exactGroup(nic["groupSet"]) ||
    list(row(nic["ipv6AddressesSet"])["item"]).length
  )
    fail();
}
