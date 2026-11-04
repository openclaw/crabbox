import { sha256Hex } from "./encoding";

export const guestNetworkVersion = 1;
export const guestPublicKeyBootstrap = "ssh-public-key-v1";
export const awsPlatformException = "aws-platform-services-v1";
export const awsBootstrapException = "aws-roleless-imdsv2-public-bootstrap-v1";
export interface GuestNetworkRule {
  cidr: string;
  ports: number[];
}
export interface GuestNetworkPolicy {
  bootstrapMode?: string;
  version: number;
  id: string;
  revision: string;
  egress: GuestNetworkRule[];
  sshSources: string[];
  platformExceptions?: string[];
}
export interface GuestNetworkPolicyIdentity {
  version: number;
  policyId: string;
  policyRevision: string;
  policyDigest: string;
  bootstrapMode?: string;
}
export interface GuestNetworkReceipt extends GuestNetworkPolicyIdentity {
  leaseId: string;
  provider: string;
  resourceId: string;
  boundaryId: string;
  facts: Record<string, string>;
}
export interface GuestNetworkBoundary {
  createAttemptId?: string;
  createRejectedNoEffect?: true;
  cleanupConfirmed?: true;
  region?: string;
  leaseId: string;
  subnetId: string;
  vpcId: string;
  securityGroupId: string;
  securityGroupName: string;
  policyDigest: string;
  bootstrapMode?: string;
  bootstrapSha256?: string;
}
export interface GuestNetworkServerEnv {
  CRABBOX_GUEST_NETWORK_POLICY_JSON?: string;
}

function prefix(value: string): [number, number] {
  const m = /^(\d+)\.(\d+)\.(\d+)\.(\d+)\/(\d+)$/.exec(value);
  if (!m) throw new Error("guest policy requires canonical IPv4 CIDRs");
  const parts = m.slice(1, 5).map(Number),
    bits = Number(m[5]);
  if (parts.some((n, i) => n > 255 || String(n) !== m[i + 1]) || bits > 32 || String(bits) !== m[5])
    throw new Error("invalid guest policy CIDR");
  const address = parts.reduce((n, p) => n * 256 + p, 0),
    size = 2 ** (32 - bits);
  if (address % size !== 0) throw new Error("noncanonical guest policy CIDR");
  return [address, address + size - 1];
}
const privateRanges = [
  "0.0.0.0/8",
  "10.0.0.0/8",
  "100.64.0.0/10",
  "127.0.0.0/8",
  "169.254.0.0/16",
  "172.16.0.0/12",
  "192.0.0.0/24",
  "192.0.2.0/24",
  "192.168.0.0/16",
  "198.18.0.0/15",
  "198.51.100.0/24",
  "203.0.113.0/24",
  "224.0.0.0/4",
  "240.0.0.0/4",
].map(prefix);
export function normalizeGuestNetworkPolicy(input: unknown): GuestNetworkPolicy {
  if (!input || typeof input !== "object" || Array.isArray(input))
    throw new Error("operator guest policy missing");
  const p = input as Record<string, unknown>;
  if (
    Object.keys(p).some(
      (k) =>
        ![
          "bootstrapMode",
          "version",
          "id",
          "revision",
          "egress",
          "sshSources",
          "platformExceptions",
        ].includes(k),
    )
  )
    throw new Error("unknown operator guest policy field");
  if (
    p["version"] !== 1 ||
    typeof p["id"] !== "string" ||
    !p["id"].trim() ||
    p["id"].length > 128 ||
    typeof p["revision"] !== "string" ||
    !p["revision"].trim() ||
    p["revision"].length > 128
  )
    throw new Error("invalid operator guest policy identity/version");
  if (
    p["bootstrapMode"] !== undefined &&
    p["bootstrapMode"] !== "" &&
    p["bootstrapMode"] !== guestPublicKeyBootstrap
  )
    throw new Error("unknown guest bootstrap mode");
  if (
    !Array.isArray(p["egress"]) ||
    p["egress"].length > 64 ||
    !Array.isArray(p["sshSources"]) ||
    !p["sshSources"].length ||
    p["sshSources"].length > 64
  )
    throw new Error("invalid guest network rule count");
  const egress = p["egress"]
    .map((entry: unknown) => {
      if (!entry || typeof entry !== "object" || Array.isArray(entry))
        throw new Error("invalid guest egress rule");
      const rule = entry as Record<string, unknown>;
      if (
        Object.keys(rule).some((k) => k !== "cidr" && k !== "ports") ||
        typeof rule["cidr"] !== "string" ||
        !Array.isArray(rule["ports"]) ||
        !rule["ports"].length ||
        rule["ports"].length > 64
      )
        throw new Error("invalid guest egress rule");
      const [lo, hi] = prefix(rule["cidr"]);
      if (privateRanges.some(([a, b]) => lo <= b && a <= hi))
        throw new Error("guest egress must be public IPv4");
      const ports = rule["ports"]
        .map((port: unknown) => {
          if (typeof port !== "number" || !Number.isInteger(port) || port < 1 || port > 65535)
            throw new Error("invalid guest TCP port");
          return port;
        })
        .toSorted((a, b) => a - b);
      if (new Set(ports).size !== ports.length) throw new Error("duplicate guest TCP port");
      return { cidr: rule["cidr"], ports };
    })
    .toSorted((a, b) =>
      a.cidr < b.cidr
        ? -1
        : a.cidr > b.cidr
          ? 1
          : JSON.stringify(a.ports) < JSON.stringify(b.ports)
            ? -1
            : JSON.stringify(a.ports) > JSON.stringify(b.ports)
              ? 1
              : 0,
    );
  const sshSources = p["sshSources"]
    .map((value: unknown) => {
      if (typeof value !== "string") throw new Error("invalid guest SSH source");
      prefix(value);
      if (Number(value.split("/")[1]) < 8) throw new Error("unbounded guest SSH source");
      return value;
    })
    .toSorted();
  const exceptions = p["platformExceptions"] === undefined ? [] : p["platformExceptions"];
  if (
    !Array.isArray(exceptions) ||
    exceptions.length > 8 ||
    exceptions.some((v) => typeof v !== "string" || !v || v.length > 128 || v.trim() !== v) ||
    new Set(exceptions).size !== exceptions.length
  )
    throw new Error("invalid platform exception policy");
  return {
    ...(p["bootstrapMode"] ? { bootstrapMode: p["bootstrapMode"] as string } : {}),
    version: 1,
    id: p["id"],
    revision: p["revision"],
    egress,
    sshSources,
    ...(exceptions.length ? { platformExceptions: [...exceptions].toSorted() as string[] } : {}),
  };
}
export async function guestNetworkPolicyIdentity(
  input: GuestNetworkPolicy,
): Promise<GuestNetworkPolicyIdentity> {
  const p = normalizeGuestNetworkPolicy(input);
  return {
    version: 1,
    policyId: p.id,
    policyRevision: p.revision,
    policyDigest: await sha256Hex(
      JSON.stringify(p).replace(
        /[<>&\u2028\u2029]/g,
        (c) => "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0"),
      ),
    ),
    ...(p.bootstrapMode ? { bootstrapMode: p.bootstrapMode } : {}),
  };
}
export function requireAWSGuestPolicy(policy: GuestNetworkPolicy, ordinaryImages = false): void {
  const expected = [
    awsPlatformException,
    ...(policy.bootstrapMode === guestPublicKeyBootstrap ? [awsBootstrapException] : []),
  ].toSorted();
  if (
    JSON.stringify([...(policy.platformExceptions ?? [])].toSorted()) !==
      JSON.stringify(expected) ||
    (ordinaryImages && policy.bootstrapMode !== guestPublicKeyBootstrap)
  )
    throw new Error(
      "operator must explicitly authorize the AWS platform and roleless public-key bootstrap contract",
    );
}
// Only the server environment is authority. Client rules, files and receipts are never inputs.
export async function serverGuestNetworkPolicy(
  env: GuestNetworkServerEnv,
): Promise<{ policy: GuestNetworkPolicy; identity: GuestNetworkPolicyIdentity }> {
  const text = env.CRABBOX_GUEST_NETWORK_POLICY_JSON;
  if (!text || text.length > 65536)
    throw new Error("managed guest network policy is not configured by the operator");
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    throw new Error("invalid operator guest network policy JSON");
  }
  const policy = normalizeGuestNetworkPolicy(raw);
  return { policy, identity: await guestNetworkPolicyIdentity(policy) };
}
export function rejectGuestNetworkClientAuthority(input: unknown): void {
  if (!input || typeof input !== "object") throw new Error("invalid lease request");
  const p = input as Record<string, unknown>;
  for (const key of [
    "guestNetworkPolicy",
    "guestNetworkRules",
    "guestNetworkReceipt",
    "guestNetwork",
    "guestNetworkBoundary",
    "guestNetworkPolicyFile",
  ])
    if (key in p) throw new Error("client guest network authority is forbidden");
  if (
    p["untrustedWorkload"] !== undefined &&
    p["untrustedWorkload"] !== false &&
    p["untrustedWorkload"] !== true
  )
    throw new Error("invalid untrusted workload request");
  if (p["untrustedWorkload"] === true && p["sshHostPrivateKey"] !== undefined)
    throw new Error("client private host-key bootstrap is forbidden");
  if (p["expectedGuestNetworkPolicy"] !== undefined && p["untrustedWorkload"] !== true)
    throw new Error("guest policy precondition requires untrusted workload mode");
  const expected = p["expectedGuestNetworkPolicy"];
  if (
    expected !== undefined &&
    (!expected ||
      typeof expected !== "object" ||
      Array.isArray(expected) ||
      Object.keys(expected).some(
        (k) =>
          !["version", "policyId", "policyRevision", "policyDigest", "bootstrapMode"].includes(k),
      ))
  )
    throw new Error("invalid guest policy precondition fields");
}
export function sameGuestNetworkPolicy(
  expected: GuestNetworkPolicyIdentity | undefined,
  actual: GuestNetworkPolicyIdentity,
): boolean {
  return (
    !!expected &&
    expected.version === actual.version &&
    expected.policyId === actual.policyId &&
    expected.policyRevision === actual.policyRevision &&
    expected.policyDigest === actual.policyDigest &&
    (expected.bootstrapMode ?? "") === (actual.bootstrapMode ?? "")
  );
}
export function validateGuestNetworkReceipt(
  receipt: GuestNetworkReceipt | undefined,
  identity: GuestNetworkPolicyIdentity,
  leaseId: string,
  provider: string,
  resourceId: string,
): void {
  if (
    !receipt ||
    !sameGuestNetworkPolicy(receipt, identity) ||
    receipt.leaseId !== leaseId ||
    receipt.provider !== provider ||
    receipt.resourceId !== resourceId ||
    !receipt.boundaryId ||
    !leaseId ||
    !resourceId
  )
    throw new Error("guest network receipt missing or mismatched");
}
export function normalizeGuestSSHPublicKey(publicKey: string): string {
  const fields = publicKey.trim().split(/\s+/);
  if (
    fields.length < 2 ||
    !["ssh-ed25519", "ssh-rsa"].includes(fields[0] ?? "") ||
    !/^[A-Za-z0-9+/]+={0,3}$/.test(fields[1] ?? "") ||
    /[\r\n]/.test(publicKey)
  )
    throw new Error("one valid public SSH key required");
  let blob: string;
  try {
    blob = atob(fields[1]!);
  } catch {
    throw new Error("invalid public SSH key encoding");
  }
  if (blob.length < 4) throw new Error("invalid public SSH key blob");
  const length =
    blob.charCodeAt(0) * 2 ** 24 +
    blob.charCodeAt(1) * 2 ** 16 +
    blob.charCodeAt(2) * 256 +
    blob.charCodeAt(3);
  if (blob.slice(4, 4 + length) !== fields[0] || blob.length <= 4 + length)
    throw new Error("invalid public SSH key type");

  let offset = 4 + length;
  const field = (): string => {
    if (offset + 4 > blob.length) throw new Error("invalid public SSH key wire data");
    const n =
      blob.charCodeAt(offset) * 2 ** 24 +
      blob.charCodeAt(offset + 1) * 2 ** 16 +
      blob.charCodeAt(offset + 2) * 256 +
      blob.charCodeAt(offset + 3);
    offset += 4;
    if (n < 1 || offset + n > blob.length) throw new Error("invalid public SSH key wire length");
    const value = blob.slice(offset, offset + n);
    offset += n;
    return value;
  };
  if (fields[0] === "ssh-ed25519") {
    if (field().length !== 32) throw new Error("invalid Ed25519 key length");
  } else {
    const exponent = field();
    const modulus = field();
    if (exponent.length > 8 || modulus.length < 256 || (modulus.charCodeAt(0) & 128) !== 0)
      throw new Error("invalid RSA public key");
  }
  if (offset !== blob.length) throw new Error("trailing public SSH key wire data");
  return fields[0] + " " + fields[1];
}

export function guestPublicKeyUserData(publicKey: string, user: string, workRoot: string): string {
  const normalized = normalizeGuestSSHPublicKey(publicKey);
  const fields = normalized.split(" ");
  if (
    !/^[a-z_][a-z0-9_-]{0,31}$/.test(user) ||
    !workRoot.startsWith("/") ||
    workRoot.includes("/../") ||
    workRoot.includes(String.fromCharCode(0)) ||
    /[\r\n]/.test(workRoot)
  )
    throw new Error("invalid public-key bootstrap user/work root");
  const content = {
    ssh_pwauth: false,
    disable_root: true,
    users: [
      {
        name: user,
        groups: ["sudo"],
        shell: "/bin/bash",
        sudo: ["ALL=(ALL) NOPASSWD:ALL"],
        lock_passwd: true,
        ssh_authorized_keys: [fields.slice(0, 2).join(" ")],
      },
    ],
    write_files: [
      {
        path: "/usr/local/bin/crabbox-ready",
        permissions: "0755",
        content: "#!/bin/sh\nset -eu\ntest -f /var/lib/crabbox/guest-public-key-ready\n",
      },
    ],
    runcmd: [
      ["install", "-d", "-m", "0755", "-o", user, "-g", user, workRoot],
      ["install", "-d", "-m", "0755", "/var/lib/crabbox"],
      [
        "/bin/sh",
        "-c",
        "{ printf 'CRABBOX_GUEST_HOST_KEY_V1 '; cut -d ' ' -f 1,2 /etc/ssh/ssh_host_ed25519_key.pub; } > /dev/console",
      ],
      ["touch", "/var/lib/crabbox/guest-public-key-ready"],
    ],
  };
  const bytes = new TextEncoder().encode("#cloud-config\n" + JSON.stringify(content));
  if (bytes.length > 16384)
    throw new Error("guest public-key bootstrap exceeds EC2 userdata limit");
  return btoa(String.fromCharCode(...bytes));
}

export function validateGuestNetworkCustodyUpdate(
  previous: GuestNetworkBoundary | undefined,
  boundary: GuestNetworkBoundary,
): void {
  if (
    boundary.createRejectedNoEffect &&
    (!previous ||
      !boundary.createAttemptId ||
      previous.createAttemptId !== boundary.createAttemptId ||
      previous.securityGroupId ||
      boundary.securityGroupId ||
      previous.leaseId !== boundary.leaseId ||
      previous.policyDigest !== boundary.policyDigest ||
      previous.subnetId !== boundary.subnetId ||
      previous.vpcId !== boundary.vpcId ||
      previous.securityGroupName !== boundary.securityGroupName ||
      previous.region !== boundary.region)
  )
    throw new Error("definitive guest rejection lacks matching attempt custody");
  if (
    previous &&
    !previous.cleanupConfirmed &&
    previous.createAttemptId !== boundary.createAttemptId
  )
    throw new Error("guest network prior attempt custody cannot be overwritten");
  if (
    previous?.createRejectedNoEffect &&
    previous.createAttemptId === boundary.createAttemptId &&
    !boundary.createRejectedNoEffect
  )
    throw new Error("guest rejection custody cannot regress");
}
export function guestNetworkCreateRejectedNoEffect(boundary: GuestNetworkBoundary): boolean {
  return (
    boundary.createRejectedNoEffect === true &&
    Boolean(boundary.createAttemptId) &&
    !boundary.securityGroupId &&
    Boolean(
      boundary.leaseId &&
      boundary.policyDigest &&
      boundary.securityGroupName &&
      boundary.subnetId &&
      boundary.vpcId,
    )
  );
}
