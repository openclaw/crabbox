# CALLER-CONTRACT v1: guest network receipts

The [executable fixture](../../internal/cli/testdata/guest-network-caller-contract.json)
contains minimum parsed fields, **synthetic examples only**, tested by Go and
TypeScript. Its managedOperatorPolicy, managedServerCapability,
managedCreateRequest, managedAcquisitionResponse and managedInspectResponse
sections define the managed contract. Existing direct sections remain independent.
See [policy and bootstrap limits](guest-network-policy.md).

## Allocation routes

| Allocation path | Local controller identity gate | Implementation |
| --- | --- | --- |
| Direct AWS CLI | absent or present | Go provider network enforcement and receipt validation before access. |
| Registered AWS | absent or present | Same direct allocator; legacy coordinator registration is inventory, not an attestor. |
| Managed AWS on this candidate server | absent or present | Fleet/AWSProvider/EC2SpotClient enforce independently configured server policy; versioned protocol below. |
| Managed AWS on an older/unconfigured server | any | Capability handshake fails before create; do not fall back to legacy allocation. |
| Other provider/unknown version | any | Unsupported for workload-author admission; ordinary trusted flows unchanged. |

Discovery capabilities.guestNetwork contains version 1, allocationRoutes
[direct, managed-coordinator], receiptOutputs [warmup-timing, inspect], and
controllerIdentityProtocol lease-identity-v1. Discovery describes candidate code,
not a deployed server or live cloud attestation. Offline config for managed mode
reports state server-qualification-required. The explicit authenticated command
config show --guest-network-server --json performs the server handshake and
reports its policy identity; it does not allocate or certify a live resource.

The local controller ack remains **exactly** its existing four allocation identity
fields. It does not serialize guestNetwork or attest a remote coordinator. Direct
and registered policy receipts come from the direct allocator even when this gate
is present. Do not confuse this gate with the managed broker.

## Managed capability and create

1. Authenticated GET /v1/guest-network/capability?provider=aws returns:

    {"version":1,"provider":"aws","allocationRoute":"managed-coordinator",
     "state":"implemented","policy":{"version":1,"policyId":"example-egress",
     "policyRevision":"fixture-v1","policyDigest":"5dcc85a2264ab4290534a16c7d065a1ac48b703650a55b15daef2ee4b4e998f3",
     "bootstrapMode":"ssh-public-key-v1"}}

2. PUT /v1/guest-network/leases/<canonical-fixed-lease-id> uses existing typed
   allocation fields (provider, resources, OS, public key, lifetime), sshPort:"22", plus
   untrustedWorkload:true and expectedGuestNetworkPolicy equal to that descriptor.
   The server independently resolves CRABBOX_GUEST_NETWORK_POLICY_JSON and compares
   the descriptor. Rules, client policy files, supplied receipts, network boundary
   handles and private SSH host keys are not accepted as authority. This request
   must never be posted to the old ordinary lease endpoint as an ignored flag.

3. A successful allocation returns the ordinary lease envelope plus its fresh
   guestNetwork receipt. It must match requested lease id, provider, actual cloudID,
   expected policy id/revision/digest/bootstrap mode, and nonempty boundaryId.
   Verify untrustedWorkload:true, strict sshHostKey equal to the observed receipt
   pin, instanceProfile:none, imds:roleless-v2, bootstrapMode:ssh-public-key-v1 and
   bootstrapSha256 before any workspace transfer, command, hook or guest access.
   The fixed identity alone is not readiness. This caller uses synchronous create;
   a pending response without the authoritative receipt fails closed, not ready.

4. GET /v1/guest-network/leases/<id> returns the same envelope after **fresh**
   provider inspection. Revalidate all bindings at inspect/replay. Never accept a
   receipt from the regular cached inventory endpoint. Changed server policy,
   unsupported version, absent receipt, mismatched lease/resource/mode or missing
   bootstrap/pin facts fail closed. No fallback to cached/legacy attestation.

The CLI can omit a local policy file in managed mode: the authenticated server
owns policy custody. If supplied, a local file is only an operator-owned expected
policy constraint and must independently pass provenance checks. Project files
are not consulted. The server never imports caller-selected rules or files.

## Direct outputs

The warmup timing document uses leaseId; inspect uses id and providerResourceId.
Both contain guestNetwork. Validate the outer actual identity and every receipt
binding, not just version or a boolean. The retained direct policy omits bootstrap
mode and illustrates the stricter explicit-image/IMDS-disabled path, not proof of
compatible image startup. The managed fixture illustrates public-key-only stock
Ubuntu bootstrap and roleless IMDSv2; the two policies have different digests.

## Source ownership and evidence

Managed implementation is in worker/src/fleet.ts (Fleet/AWSProvider), aws.ts
(EC2SpotClient), aws-guest-network.ts (control-plane enforcement/readback), and
coordinator-runtime.ts (both runtimes' request queue). Go callers use
coordinator_guest_network.go; direct AWS uses the provider adapter and
aws_guest_network*.go. A server source implementation now exists in this repo;
there is no claim that an existing deployment already runs it.

Provider-mocked positive acquisition/replay/inspect and failure tests are not a
live VM boot, IAM qualification, network probe or deployment receipt. Ordinary
stock-image startup and strict host-key transport need exact-source live proof
with authorized task resources and confirmed cleanup before production admission.
