# Operator-owned guest network policy

This optional workload-author boundary does **not** change Crabbox's trusted
operator model in [SECURITY.md](../../SECURITY.md). The CLI, coordinator,
credentials, policy and control-plane APIs remain controlled by trusted
operators. Guests author workloads only, not allocator configuration or raw
coordinator/CLI operations. This is not hostile multi-tenant operator isolation.

## Implemented routes and authority

- **Direct AWS** uses the Go provider adapter and EC2 control plane. Set
  CRABBOX_UNTRUSTED_WORKLOAD=true and
  CRABBOX_GUEST_NETWORK_POLICY_FILE=/absolute/operator/policy.json in the
  privileged operator process. Policy files must be regular, owner-controlled,
  outside the workload Git boundary, and not symlinks or group/other writable;
  ancestors must preserve custody. Windows file ACL custody is unsupported.
- **Managed AWS** uses the shared Worker/Node Fleet allocator, AWSProvider and
  EC2SpotClient in this repository. The server independently resolves operator
  CRABBOX_GUEST_NETWORK_POLICY_JSON; a client file can only constrain the
  expected identity, never install server rules. Versioned capability/create/
  inspect routes must be present on the server. An older server fails closed
  before allocation, never receives unknown fields on its legacy create route.
- Registered mode allocates directly and registers ordinary inventory afterward.
  Registration is not network attestation. A local controller identity gate is
  also not the managed coordinator and does not attest network policy.
- Other providers, unknown versions and unsupported image/transport combinations
  fail closed in this workload-author route. Trusted workflows remain unchanged.

Project configuration is skipped in guest mode. Shared caches/volumes, forwarded
environment, overlays, instance roles, legacy bootstrap hooks and secret-bearing
SSH host private-key userdata are not admitted. Provider credentials stay in the
privileged allocator. The public API never exposes the internal operator policy
snapshot or cleanup journal as caller authority.

## Policy and enforcement

Use the executable synthetic policy and descriptors in
[the caller contract fixture](../../internal/cli/testdata/guest-network-caller-contract.json).
Policy version 1 contains an id/revision, literal public IPv4 TCP destination CIDRs
and ports, SSH source CIDRs and explicitly acknowledged platform exceptions.
The canonical SHA-256 includes effective rules, bootstrap mode and exceptions,
not merely the display id/revision. Empty egress is expressed as an array.
It is not a domain-name allowlist, proxy, arbitrary protocol policy or broad
Internet access claim. Metadata policy is enforced separately from SG rules.

Each allocation owns a dedicated security group. The provider removes default
allow-all egress, applies exact rules, and reads them back before launch. It
checks the actual instance, single NIC, subnet/VPC/group, lease/policy tags,
metadata settings and absence of an instance profile before access. Supplied
subnet/group strings or a caller's JSON receipt are not evidence. A fabricated
subnet-review-trusted/sg-review-trusted cannot substitute for API facts.
Typed resource/class, capacity, architecture and region selection remain dynamic;
a security policy is not a pre-authored machine recipe. Managed AWS may resolve
an actual default subnet through read-only EC2 discovery; direct currently
requires an operator-selected subnet. No route/VPC/subnet creation is introduced.

### AWS platform limits

Security groups do not block all AWS platform traffic. Version 1 requires explicit
aws-platform-services-v1 acknowledgement and reports the AmazonProvidedDNS
exception. AWS DNS/resolver, DHCP, time sync, VPC router and other AWS-documented
platform exceptions are **not** certified as blocked. No claim of arbitrary DNS
isolation or protection against every AWS platform service is made. Evaluate
whether this bounded policy meets the operator's threat model before use.

## Secret-free ordinary Linux bootstrap

bootstrapMode: "ssh-public-key-v1" additionally requires explicit
aws-roleless-imdsv2-public-bootstrap-v1 acknowledgement. The implemented
ordinary-image path uses stock Ubuntu 22.04 or 24.04, SSH port 22, no instance
profile, no private/SSM transport, and no custom AMI/snapshot. These are tested
source compatibility limits, not a claim that no other image could be safe.
The managed path deliberately bypasses promoted/custom images.

Stock cloud-init obtains its datasource via IMDS, so permanently disabling IMDS
would break ordinary key installation. This mode instead enforces IMDSv2 tokens,
hop limit 1, disabled metadata tags/IPv6 metadata, and **no instance role**.
Userdata contains only the per-lease SSH **public** key, a configured user/work
directory, a minimal readiness marker and public host-key console emission.
No operator/model/provider secret, host private key, overlay token, package
install, repository hook or legacy setup is rendered. Userdata bytes are hashed
before launch and read back with DescribeInstanceAttribute before access.
The guest's SSH host private key is generated in the guest. The privileged
allocator obtains its public counterpart via EC2 GetConsoleOutput, pins it in
provider-owned instance tags and returns it for strict host-key verification.
Console output is **not** used as network isolation evidence.

Omitting bootstrapMode retains the direct path's stricter zero-userdata,
IMDS-disabled explicit-prebuilt-image mode. A nonempty AMI id alone does not prove
key installation or image hygiene. No ordinary-image startup is qualified in that
legacy mode; do not relax only the AMI check or enable legacy secret-bearing
userdata to make it boot. The managed route requires public-key bootstrap mode.

## Custody, replay and cleanup

Before provider mutation, the allocator persists an allocation-bound network
cleanup handle; after creation it persists the actual group id. A structured
CreateSecurityGroup UnauthorizedOperation is recorded as a definitive no-effect
rejection only for its matching persisted attempt. Creation transport retries are
disabled, so a later rejection cannot erase an earlier ambiguous request. Fenced
retirement of that network-only handle permits retry after permission repair; it
does not delete provider resources or unrelated guard references. Timeouts,
unknown errors, prior uncertain attempts and foreign resources retain custody. Failed launch or
receipt validation only terminates an instance whose provider lease/policy
ownership is verified. Cleanup deletes only the dedicated owned group after NIC
detachment and confirms absence. Uncertain outcomes retain custody for recovery;
foreign/shared groups are never modified or deleted to make a retry pass.

Acquisition and inspect receipts bind version, lease id, provider, actual resource
id, policy id/revision/digest/bootstrap mode and observed boundary id. Managed
inspect re-reads APIs, not a cached receipt; policy changes, missing facts,
mismatched resources and replay across trusted/guest modes fail closed before
workload access. Release identity remains available even if policy inspection
fails, so cleanup is not made dependent on accepting a bad receipt.

## Proof status and deployment gates

The implementation has synthetic/provider-mocked regression tests and retained
source/binary receipts. These are **not live isolation or successful SSH boot
proof**. No coordinator has been deployed, no policy/config/role has been changed,
and no cloud resource was allocated for this change. A live qualification needs
the exact candidate deployed by an authorized operator, a trusted policy, scoped
EC2 permissions (including userdata/console inspection and owned SG lifecycle),
a task-owned allocation, actual startup/readback receipts and confirmed cleanup.
An existing managed deployment is not unblocked merely by upgrading its caller.
