# Broker-mediated SSH handoff

Status: proposed design; no runtime implementation exists yet.

This document specifies portable SSH access for a ready-pool borrow. It is the
remaining security-sensitive half of
[issue 1074](https://github.com/openclaw/crabbox/issues/1074). The desired-capacity,
compatibility, heartbeat, quarantine, pruning, and metrics half landed in
[pull request 1197](https://github.com/openclaw/crabbox/pull/1197).

The authorization model follows the scoped, hash-only, fail-closed conventions
introduced by read-only device pairing in
[pull request 1199](https://github.com/openclaw/crabbox/pull/1199) and documented
in [Read-Only Device Pairing](../features/device-pairing.md). A borrow grant is
still a different principal with a different scope: it authorizes short-lived
SSH access to exactly one active borrow and nothing else.

## Goals

- Let an independently configured, authorized client use a borrowed ready-pool
  lease without receiving the fill keeper's private key.
- Bind every access credential to one authenticated borrower, owner/org scope,
  lease, borrow token, and expiry.
- Keep private keys on the client that generated them. When a provider uses a
  token instead, expose it once and never persist or log it in Crabbox.
- Revoke access before a lease becomes ready again. If revocation cannot be
  proved or established sessions cannot be fenced, quarantine or drain the
  lease instead of weakening the contract.
- Keep provider-specific certificates, key installation, proxy tokens, guest
  mutation, and cleanup behind a provider capability.
- Roll out without changing the meaning of legacy borrows or silently falling
  back to a standing key.

## Trust boundary

The coordinator already authenticates users, holds managed-provider
credentials, serializes pool state, and owns brokered lease cleanup. It is
therefore the only component that may issue a borrow grant and invoke a
managed-provider access capability. The runner remains a leaf and the CLI
continues to open the SSH data plane directly to it.

The recommended credential shape is a client-generated ephemeral Ed25519 key
pair. The borrower sends only its public key to the coordinator. A provider
adapter then returns one of these access forms:

- an OpenSSH user certificate for that public key;
- a time-bounded authorization of that public key on the runner; or
- a provider-issued, time-bounded SSH proxy token when the provider does not
  accept caller-generated keys.

The coordinator must not generate or escrow a reusable SSH private key. It may
relay a provider-issued proxy token in the successful response, but it must not
store that token. Public certificates and endpoint metadata are not secret, but
Crabbox should store only the minimum metadata needed to revoke or audit them.

## Grant model

### Identity and binding

An access-aware borrow creates one `cbxg_` borrow grant. Its persisted record
binds all of the following:

- a random grant ID and SHA-256 hash of the opaque grant token;
- the authenticated principal class, resolved owner identity, and exact
  organization (an immutable account ID for signed users or the configured
  identity of an approved automation token);
- the pool key and compatibility key used for selection;
- the ready-pool entry ID and canonical lease ID;
- a hash of the borrow token, never a second cleartext copy;
- the borrow generation or nonce, so a later borrow of the same lease cannot
  reuse an old grant;
- issue time, absolute expiry, current state, and any issued provider access IDs;
- the provider and advertised access-capability version.

Here, `owner` means the principal that performed the borrow, not necessarily the
fill keeper that registered the lease. The registering owner remains part of
the lease's cleanup and accounting record; it does not become the portable
credential holder.

Like device credentials, the raw grant is returned once, accepted only in a
request body or protected credential store, and represented at rest only by
its hash. It must not appear in URLs, command arguments, logs, analytics, crash
reports, screenshots, or repository configuration. Hash comparisons are
constant-time.

The grant's absolute expiry is the current borrow's hard expiry, capped by the
lease expiry. A rolling heartbeat can shorten its effective validity when a
deadline is missed, but it never extends the stored absolute expiry. The grant
exists to authorize credential renewal during that borrow; the SSH credential
itself has the much shorter lifetime defined below.

The existing ready-pool implementation stores cleartext borrow tokens. The
handoff implementation should migrate access-aware borrows to a hash verifier
for heartbeat, return, and access operations. A new worker may continue to read
legacy cleartext records during the rollout, but it must not issue portable
access from such a record until it has atomically upgraded the verifier and
borrow generation.

### Issuance and authorization

Use a new atomic route rather than adding an optional field to the existing
borrow route:

```text
POST /v1/ready-pools/:key/borrow-with-access
  Auth: normal user or approved automation principal
  Idempotency-Key: client-generated operation ID
  Body: { compatibilityKey, heartbeat: true }
  Result: provisional busy entry, borrow token, one cbxg_ grant, capability metadata
```

The distinct route matters for compatibility. An older worker returns `404` or
`405` instead of ignoring an access requirement and handing the new CLI a lease
it cannot use. The operation selects only an entry whose provider advertises a
supported handoff capability, marks it busy, creates its borrow verifier and
grant atomically, and returns the secrets once. A failure before commit leaves
the entry ready; a failure after an ambiguous provider mutation quarantines the
entry for reconciliation.

Secret delivery is a short two-phase operation. The initial result is
`provisional` and cannot issue provider access. The client acknowledges receipt
through this route:

```text
POST /v1/ready-pools/:key/borrow-access/ack
  Auth: same current owner/org principal
  Body: { leaseId, borrowOperationId, borrowToken, grant }
  Result: 204 No Content
```

The coordinator repeats the current owner/org, pool, lease, borrow generation,
operation ID, and hash-verifier checks. Only that atomic check makes the borrow
access-capable. The first valid acknowledgment and an exact replay both return
`204`. `409 provisional_replaced` means a retry rotated the secrets, so the
client repeats the original operation ID and acknowledges its newest result.
`410 provisional_expired` means the two-minute window elapsed and no lease was
borrowed. The access route returns `409 borrow_not_acknowledged` for every
provisional borrow.

A repeated original operation ID before acknowledgment rotates both verifiers
and returns new secrets, invalidating any late response. A failed
acknowledgment tells the client to repeat the original operation ID and use the
newest result. An unacknowledged operation expires after two minutes; because
no provider credential could have been issued, the coordinator can remove the
verifiers and return the entry to `ready`. After acknowledgment succeeds, a
late retry of the original borrow operation returns
`operation_already_acknowledged` without rotating or disclosing secrets.

Mint or renew provider access through a second route:

```text
POST /v1/ready-pools/:key/access
  Auth: same current owner/org principal
  Idempotency-Key: client-generated operation ID
  Body: { leaseId, borrowToken, grant, credentialKind,
          requestedLifetimeSeconds, publicKey?, previousAccessId? }
  Result: access ID, SSH endpoint, credential material, expiry, renewal time
```

Possession of a grant alone is insufficient. Each call also requires the same
authenticated owner/org and matching borrow token. The coordinator freshly
checks that:

- the caller is still authorized for the organization;
- the grant hash, owner/org, pool, lease, borrow verifier, and generation match;
- the entry is still `busy`, its negotiated heartbeat is current, and neither
  the borrow nor lease has expired;
- the lease remains owned by the recorded pool entry and is not draining,
  quarantined, releasing, or under cleanup; and
- the requested provider method and lifetime are within the advertised
  capability.

Authentication, membership, storage, or provider uncertainty fails closed.
There is no positive authorization cache for access issuance or renewal. This
mirrors device-pairing revalidation without copying its sealed browser grant:
the borrower already presents a normal authenticated principal on each call.

Every access operation ID is bound to a normalized request fingerprint covering
the grant ID, borrow generation, public-key fingerprint, credential kind,
requested lifetime, exact nullable `previousAccessId`, and trusted network
authorization context. The coordinator derives that context from the verified
request source and trusted proxy chain, canonicalizes it as a sorted unique list
of source CIDRs, and never accepts it from the request body. A retry observed
from a different network context is not an exact replay and returns
`409 idempotency_key_reused`; the coordinator must reconcile the original
operation before the client begins an authorized rotation with a new operation
ID.

The coordinator computes the effective absolute expiry once, caps it as
described below, and stores it on the operation. Reusing an operation ID with
any different input returns `409 idempotency_key_reused`. Exact replay lookup
happens before the current-access-ID precondition, so a renewal from A to B
whose response was lost can recover operation B even though A is no longer
current.

The coordinator also assigns and persists a random `accessHandle` before the
provider call. For a public-key authorization, or a certificate whose public
body can be stored, an exact retry returns the same access result. A proxy-token
adapter may replay the same result only when its provider guarantees idempotent
secret retrieval. Otherwise an ambiguous proxy-token response is unrecoverable
for that borrow: the coordinator cancels and revokes the attempt when possible,
waits for provider-confirmed expiry when necessary, fences sessions, and drains
the lease. It never issues a replacement proxy token or creates a second
provider attempt under the same client operation.

A grant permits one current credential. The first issue omits
`previousAccessId`. Every later issue is a renewal and must name the exact
current access ID. Atomically, at most one operation may be `issuing` or
`unknown`, and at most one prior credential may remain `active` while its
replacement is issuing. A successful replacement immediately moves the prior
ID to `revoking`; no further issue begins until that revoke is confirmed.
Therefore one grant has at most two live provider authorizations during a
bounded rotation and cannot create an unbounded operation queue.

Operation records contain no grant, borrow token, private key, or proxy token.
Nonterminal records have no borrow-based expiry: they survive return, deadline,
lease expiry, caller disconnect, and coordinator restart until provider absence
and session fencing are positively established. Only terminal records receive
a retention TTL, recommended as 24 hours for audit and retry diagnosis.

### Issuance and return serialization

Provider calls may outlive the coordinator transaction that authorized them.
Before calling `issueBorrowAccess`, the coordinator atomically creates an
`issuing` operation bound to the borrow generation, access handle, and
idempotency key. Every revocation trigger—return, timeout, drain, release,
explicit borrower/admin revocation, and detected owner/org authorization
loss—atomically tombstones the grant verifier and moves the generation plus all
issuing/active operations to `revoking`. No trigger moves the entry to `ready`
while any generation-scoped operation is `issuing`, `active`, `revoking`, or
`unknown`.

After the provider call completes, the coordinator freshly revalidates current
owner/org authorization, then atomically rechecks the borrow generation,
acknowledged `busy` state, active grant verifier, and issuing operation. Only if
every check still matches does it record the access ID as `active` before
returning credential material. If authorization fails, revocation started, or
any check changed meanwhile, it records the result under the old generation
and immediately revokes and fences it without returning the credential. An
ambiguous provider result remains `unknown` and must be reconciled by its
access handle or idempotency key.

The only transition to `ready` requires zero in-flight operations and positive
proof that every `active`, `revoking`, or `unknown` operation for the generation
has crossed a monotonic provider cancellation barrier, is absent, and is
fenced. An `absent` inspection alone is insufficient: a delayed provider issue
request could install access after the inspection. The barrier must guarantee
that no current or future issue with that access handle or idempotency key can
become active. Durable cleanup retries this reconciliation after caller
disconnects or coordinator restarts. A provider that cannot supply the barrier
for an ambiguous in-flight issue forces lease destruction; it cannot reconcile
that lease back to `ready`. The coordinator never holds its storage lock across
a provider network call.

### Caps and revocation

The recommended initial limits are one active grant per borrow and at most ten
active borrow grants per owner/org. The latter matches device pairing's bounded
owner indexes and is high enough for ordinary parallel test runs. Both limits
must be enforced atomically. Operators may configure a lower cap, but raising
it should remain an explicit deployment choice.

The coordinator revokes the grant verifier and every issued provider access
record on:

- successful return;
- missed heartbeat or borrow deadline;
- explicit drain or release;
- lease expiry or provider cleanup;
- owner/org authorization loss detected during a coordinator request; or
- explicit borrower or administrator revocation.

Verifier-first deletion prevents new credentials and renewals immediately. The
coordinator then asks the provider to revoke each active access ID. Return to
`ready` is permitted only after the adapter proves the authorization absent and
fences every connection and process associated with the borrow generation.
Removing an authorized key, expiring a certificate, or revoking a proxy token
prevents new authentication but does not terminate an established SSH session.
The provider must therefore terminate and positively reconcile those sessions,
or the lease must drain and be replaced. If the provider supports expiry but
not immediate revocation, the lease remains quarantined until the last
credential's provider-confirmed expiry and still requires session fencing. A
failed or ambiguous revoke or fence drains the lease. This rule is intentionally
stricter than best-effort cleanup: a lease must never become borrowable while a
prior borrower may still have an authenticated session.

## Credential lifecycle

The CLI creates a fresh key pair per borrow in its protected runtime directory,
with mode `0700` for the directory and `0600` for the private key. It does not
reuse the normal per-lease creator key. The private key is deleted after
revocation or final expiry. Providers that require RSA for a named target may
request that key type through capability metadata; the CLI must not guess based
on provider name.

The coordinator parses the submitted public key before authorization. It
accepts exactly one bounded key blob in an algorithm advertised by the provider
capability, rejects authorized-key options, line breaks, NULs, extra keys, and
trailing non-comment data, and reserializes the parsed key to one canonical
OpenSSH representation. Fingerprints and idempotency use the canonical key
blob. Adapters receive only that representation and must use structured APIs or
safe standard input rather than shell interpolation. Ed25519 is the default;
targets that require RSA must enforce the repository's configured minimum key
size and SHA-2 signatures.

The recommended maximum access lifetime is 15 minutes, capped to the earliest
of:

- the grant expiry;
- the negotiated borrow deadline;
- the lease expiry; and
- the provider's supported maximum.

The CLI renews at five minutes remaining, or at one third of the lifetime for
credentials shorter than 15 minutes. Renewal is a fresh authorization decision,
not an extension of stored secret material. The client may reuse its ephemeral
key pair within one borrow, but the provider must return a new access ID and
credential. After a successful renewal the coordinator revokes the superseded
access ID; an ambiguous rotation quarantines rather than accumulating unknown
credentials.

For an expiry-only provider, use a maximum five-minute credential and do not
renew until the previous credential is close to expiry. This limits overlap and
keeps quarantine after return bounded. Whether a five-minute wait is acceptable
for a ready pool is a maintainer policy decision; the alternative is to reject
expiry-only providers until they add explicit revocation.

The response can carry one of these versioned forms:

```text
ssh-certificate
  certificate, endpoint, host-key metadata, accessId, expiresAt

ssh-public-key
  endpoint, host-key metadata, accessId, expiresAt
  # the client's private key is the credential; no private material is returned

ssh-proxy-token
  token, endpoint, host-key metadata, accessId, expiresAt
```

Credential-bearing responses use `Cache-Control: no-store`. Tokens and private
material never enter coordinator storage, metrics, structured errors, or audit
payloads. Host-key verification remains mandatory; handoff does not authorize
`StrictHostKeyChecking=no`.

### SSH host-key provenance

An independently configured borrower has no prior host trust anchor. Therefore
the endpoint's host key must be established before the entry becomes portable
through an authenticated setup path, not learned by probing that endpoint. A
provider adapter may obtain it from a provider-authenticated guest/control
channel, inject a predeclared host key or host CA during trusted provisioning,
or consume equivalent signed provider attestation. Plain `ssh-keyscan`, first
connection trust, and a key reported over the same unauthenticated SSH path do
not qualify.

The coordinator stores the canonical host-key set and its provenance, bound to
the provider resource identity, lease ID, and lease generation. Registration
for portable access fails if that evidence is absent. Every access response
returns the stored `known_hosts` material over the authenticated coordinator
connection. A missing, changed, or ambiguously rotated host key makes portable
access unavailable and drains the entry. Rotation requires fresh authenticated
provider evidence while the entry is not borrowed and creates a new lease
generation before any later grant can bind to it.
Provider access issuance may return endpoint metadata, but core rejects a
conflicting host key and never replaces the stored trust anchor from that
response.

## Provider capability

Core code selects a capability and enforces the generic lifecycle. It must not
branch on `provider == aws`, `provider == azure`, or another provider name.
Provider adapters own access installation, certificate/token semantics,
network reconciliation, resource naming, and removal.

The conceptual Worker-side interface is:

```ts
interface BorrowSSHAccessBase {
  version: 1;
  credentialKinds: Array<"ssh-certificate" | "ssh-public-key" | "ssh-proxy-token">;
  revocation: "immediate" | "expiry-only";
  maxLifetimeSeconds: number;

  issueBorrowAccess(input: {
    lease: ProviderLeaseReference;
    borrowGeneration: string;
    accessHandle: string;
    credentialKind: "ssh-certificate" | "ssh-public-key" | "ssh-proxy-token";
    networkContext: { sourceCIDRs: string[] };
    publicKey?: string;
    requestedExpiresAt: string;
    idempotencyKey: string;
  }): Promise<BorrowSSHAccess>;

  revokeBorrowAccess(input: {
    lease: ProviderLeaseReference;
    accessId: string;
    idempotencyKey: string;
  }): Promise<"revoked" | "already-absent">;

  fenceBorrowSessions(input: {
    lease: ProviderLeaseReference;
    borrowGeneration: string;
    accessIds: string[];
    idempotencyKey: string;
  }): Promise<"fenced" | "already-absent">;
}

type BorrowAccessInspection =
  | { state: "active"; accessId: string }
  | { state: "absent" | "unknown" };

type BorrowSSHAccessCapability =
  | (BorrowSSHAccessBase & {
      ambiguousIssueRecovery: "linearizable-cancel";
      cancelBorrowAccess(input: {
        lease: ProviderLeaseReference;
        accessHandle: string;
        idempotencyKey: string;
      }): Promise<"cancelled" | "already-cancelled">;
      inspectBorrowAccess(input: {
        lease: ProviderLeaseReference;
        accessHandle: string;
        idempotencyKey: string;
      }): Promise<BorrowAccessInspection>;
    })
  | (BorrowSSHAccessBase & {
      ambiguousIssueRecovery: "drain";
      inspectBorrowAccess?(input: {
        lease: ProviderLeaseReference;
        accessHandle: string;
        idempotencyKey: string;
      }): Promise<BorrowAccessInspection>;
    });
```

`publicKey` is required for `ssh-certificate` and `ssh-public-key` and rejected
for `ssh-proxy-token`. The requested kind must be one of the adapter's declared
`credentialKinds`; core never asks the adapter to infer it from provider name
or key presence.

`issueBorrowAccess` must be idempotent for one idempotency key or persist the
coordinator-chosen access handle in provider state so
`inspectBorrowAccess` can recover the stable access ID after an ambiguous
response. `revokeBorrowAccess` must be safe to retry. For a
`linearizable-cancel` adapter, `cancelBorrowAccess` is a linearizable barrier:
after it returns, the provider guarantees that an issue for the same access
handle or idempotency key has either become observable or can never take
effect. Core can then revoke an observable access ID and prove absence. An
adapter without that guarantee may still support portable use with forced
drain, but it must advertise `ambiguousIssueRecovery: "drain"` and can never
return an ambiguously issued lease to `ready`.

`fenceBorrowSessions` is distinct from credential revocation. An adapter may
implement it with a provider gateway that terminates generation-scoped tunnels,
a dedicated per-borrow OS principal whose sessions and processes can be killed
and reconciled, or a reboot that preserves the reusable lease contract. Merely
removing a line from `authorized_keys`, changing a firewall rule, or restarting
the parent `sshd` is not proof that child sessions ended. If the adapter cannot
identify and fence the borrow generation, portable use must force `drain` and
the entry cannot return to `ready`.

Network access is part of the same provider operation. For example, an adapter
may add the borrower's observed source CIDR while issuing access and remove it
when the final access ID is revoked. Core passes generic request and lease
context; the adapter decides how its firewall or gateway represents that
access. A credential is not considered issued until both authentication and
network prerequisites are ready.

### Current provider survey

This survey describes the adapters currently in the repository, not every
feature a vendor might offer outside Crabbox.

| Current access model | Providers | Handoff status |
| --- | --- | --- |
| Provider-minted expiring SSH token | `daytona` | The only managed adapter with an existing issuance primitive. It can be the first end-to-end candidate, but the current API integration exposes expiry, not explicit per-token revoke or active-tunnel fencing. It therefore needs expiry-only quarantine plus proven gateway fencing, or it must drain after use. |
| Provider certificate plus authenticated proxy | `tenki` | Promising credential shape, but the adapter is direct-only and returns local Tenki-managed identity/certificate files plus a CLI proxy command. It cannot be brokered until a coordinator-side service credential and response-only handoff are defined. |
| Attach and detach a public key on a running machine | `vast` | The direct adapter has the closest current key-authorization primitive. It is not a managed broker provider, and its keys are not intrinsically short-lived; adding broker support, expiry metadata, and fail-closed reconciliation would be required. |
| Guest key injection or append during lifecycle operations | `applecontainer`, `hyperv`, `localcontainer`, `parallels`, `sprites`, `tart` | These contain useful mutation code, but no complete issue/remove/inspect capability. Most are local or direct-only. They cannot advertise handoff until exact removal and ambiguous-failure recovery are implemented. |
| Provider or CLI proxy using local account state | `coder`, `githubcodespaces`, `namespace`, `nvidiabrev` | Access depends on a locally authenticated CLI, SSH config, or key path. That local authority is not a portable broker credential and must not be copied or inferred by the coordinator. |
| Provider returns a standing private key | `morph`, `sealosdevbox`, `semaphore` | Incompatible with this design. Returning the same private key to another borrower recreates the rejected copy-the-key model; an upstream scoped-token, certificate, or key-install API is required. |
| Static configured target or external protocol | `asciibox`, `exedev`, `external`, `islo`, `ssh` | No coordinator-owned, per-borrow authorization primitive. These remain unsupported unless the external protocol grows the full capability and revocation contract. |
| Public key fixed at create/bootstrap time | `applevm`, `aws`, `azure`, `digitalocean`, `firecracker`, `gcp`, `hetzner`, `hostinger`, `incus`, `kubevirt`, `lambda`, `linode`, `lume`, `multipass`, `namespaceinstance`, `nebius`, `ovh`, `phala`, `proxmox`, `runpod`, `scaleway`, `tencentcloud`, `vultr`, `xcpng` | Unsupported today. Their current access path resolves the creator's stored key or a configured static key. Provider-native ephemeral access or a trusted guest mutation channel must be added before handoff. |

Of the five currently managed broker providers, the practical conclusion is
simple: `daytona` has a short-lived issuance mechanism; `aws`, `azure`, `gcp`,
and `hetzner` use the creation-time public key and cannot satisfy handoff today.
No current adapter implements the complete capability, including authenticated
host-key provenance and generation-scoped session fencing.

Possible future mechanisms still belong inside their adapters:

- AWS Linux could use EC2 Instance Connect where the image and network support
  it. Other AWS targets need a separate, explicitly advertised capability; SSM
  access is not silently equivalent to SSH.
- GCP Linux could use OS Login with time-bounded keys or certificates when the
  project and image are configured for it.
- Azure could use a VM extension or Run Command to add and remove an exact key,
  but it needs idempotent mutation and positive removal proof.
- Hetzner has no current post-create key-install path in the adapter, so it
  would need a trusted guest agent or another control channel.

These are candidates, not claims of current support. Target OS, image
preparation, firewall behavior, and vendor API guarantees can narrow support;
capability discovery must report that exact scope.

## Threat model

### What a grant permits

A valid grant, matching normal authentication, and a matching borrow token let
the borrower request SSH access only to the recorded lease while that exact
borrow remains active. Once connected, the borrower can execute arbitrary code
as the configured SSH user and can read or alter anything that user can access.
Ready pools are for trusted workloads; the checkout scrub prevents task-state
confusion but is not cross-tenant isolation.

An attacker with only the grant cannot call provider APIs, borrow another
lease, change lifecycle state, use portal/admin routes, mint access for another
owner/org, or renew after revocation. An attacker with an already issued private
key or proxy token can start new SSH sessions until provider revocation or
credential expiry. A session authenticated before that point may continue until
generation-scoped fencing or lease destruction succeeds. Neither the SSH
credential nor an established session is coordinator authentication. Stealing
both the grant and the borrower's normal API credential permits renewal only
within the same borrow, lease, owner/org, and hard expiry.

Compared with copying the fill keeper's private key, compromise is bounded to
one borrow and a maximum 15-minute credential, every issuance has a distinct
access ID, renewal is reauthorized, and return blocks on both revocation and
session fencing. The fill keeper's standing lease key never crosses clients
and does not need rotation after every borrower.

### Non-goals

- This design does not make a reused host safe for mutually untrusted tenants.
- It does not sandbox commands, protect secrets already present on the runner,
  or restrict privilege escalation available to the SSH user.
- It does not replace SSH host-key verification or network-layer authorization.
- It does not grant provider API, lease administration, pool management,
  portal, WebVNC, or device-pairing authority.
- It does not make direct-only or static SSH providers broker-capable.
- It does not promise immediate revocation from a provider whose credential is
  cryptographically valid until expiry; such leases stay quarantined until
  expiry or are excluded by policy.
- It does not assume credential expiry closes an already authenticated SSH
  connection; a provider must fence it or the lease drains.

Authorization loss discovered between coordinator calls cannot invalidate an
offline certificate faster than the provider supports. Short lifetimes,
renewal revalidation, provider revocation, and quarantine bound that window.

## Audit and observability

Emit structured audit events for grant issue, credential issue, renewal,
revocation request, confirmed revocation, expiry-only quarantine, revoke
failure, and final drain or ready transition. Each event includes timestamp,
request ID, grant ID, access ID, owner/org, pool key, lease ID, borrow
generation, provider, credential kind, expiry, result, and sanitized error
category. It never includes the grant, borrow token, private key, proxy token,
certificate body, full public key, provider credential, or command contents.
A public-key fingerprint is optional and should be recorded only if it is
needed for provider reconciliation.

Metrics should include issue and renewal latency, active grants, credentials by
kind, issue failures, revoke failures, expiry-only quarantine duration, drains
caused by uncertain access, and attempts rejected by each fail-closed check.
Per-owner detail belongs in authenticated audit lookup, not low-cardinality
metrics labels.

## Compatibility and rollout

Portable access is opt-in and versioned. Existing `borrow`, `heartbeat`, and
`return` routes keep their current meaning for older CLIs. A new worker must not
issue a portable-only entry through the legacy borrow route unless the caller
can prove it already owns the creator key; the simplest first rollout is to
keep portable pools separate and feature-gated.

A new CLI asks for `borrow-with-access`. Against an older worker, `404` or `405`
means portable handoff is unsupported. The CLI may use the existing documented
cold-overflow path to provision a new lease with its own key, but it must print
that it did not consume ready capacity. It must never retry a legacy pool borrow
and never request or copy the fill keeper's key.

An older CLI against a new worker continues to use legacy entries under the
old client-owned credential contract. It does not receive a grant and is not
subject to the new access lifecycle. Operators should not convert an existing
pool to portable-only until deployed clients understand capability metadata.

If an entry's provider lacks the capability, the access-aware route excludes
it. If no compatible capable entry exists, the response distinguishes
`portable_access_unavailable` from an ordinary empty pool. A provider error
after selection quarantines or drains the entry according to whether the
coordinator can prove that no credential was installed. No path falls back to
a static key, disables host-key checking, or lengthens the credential lifetime.

Persisted records need a schema version, borrow generation, hashed grant and
borrow verifiers, access IDs, and expiry/revocation state. Workers must tolerate
legacy records for normal return and pruning, but only versioned records may
enter the portable-access flow. Rollback must leave new records non-borrowable
to an older worker, or the feature must remain disabled until all coordinator
instances understand the schema.

## Phased implementation plan

1. Land the versioned grant, access-record, and provider-capability types behind
   a disabled deployment flag. Add adversarial tests for cross-owner use,
   replay, hash-only storage, response loss, concurrent cap enforcement,
   authorization loss, ambiguous issue/revoke, an SSH session held open across
   return, a delayed provider issue arriving after an absence check, expiry,
   heartbeat timeout, and rollback handling.
2. Add `borrow-with-access`, `borrow-access/ack`, and access issue/renew routes
   with a fake provider. Update the CLI to generate an ephemeral key, refuse
   legacy fallback, acknowledge provisional delivery, renew, and delete local
   material. Prove the full state machine without enabling a real provider.
3. Add one real provider. The smallest issuance implementation is Daytona
   because its managed adapter already mints expiring access. The recommendation
   is to ship it only if the provider can also prove active gateway tunnels are
   fenced; otherwise use it for protocol proof with forced drain, or choose a
   provider with explicit revoke and fencing before production enablement.
4. Add one immediate-revocation public-key provider and prove return-to-ready
   only after exact removal. This validates the common client-generated-key
   path rather than treating proxy tokens as the universal model.
5. Add AWS Linux and then Azure/GCP only after their target- and image-specific
   mechanisms meet the same interface. Capability metadata, not provider name,
   controls pool eligibility. Keep unsupported targets on cold provisioning.
6. Enable portable pools per owner/org, observe revoke/quarantine/drain metrics,
   then consider broader defaults and removal of the legacy creator-key pool
   contract in a separately approved migration.

## Maintainer decisions

1. **Expiry-only providers:** allow them with a five-minute maximum and
   quarantine until expiry, or require immediate revocation? Recommendation:
   allow expiry-only only in the initial Daytona experiment, then require an
   explicit operator policy for production pools.
2. **First real provider:** Daytona is the smallest end-to-end slice; AWS Linux
   is more representative but requires image and Instance Connect decisions.
   Recommendation: Daytona for protocol proof, followed by a revocable
   public-key adapter before calling the design generally supported.
3. **Grant authentication:** bearer grant alone would simplify handoff but
   increases theft impact. Recommendation: require grant, borrow token, and the
   same current authenticated owner/org on every issue or renewal.
4. **Lifetime:** recommendation is 15 minutes for immediately revocable access
   and five minutes for expiry-only access. Confirm whether long test commands
   may renew indefinitely up to the borrow and lease hard expiry.
5. **Owner cap:** recommendation is ten active grants per owner/org and one per
   borrow, matching device-pairing conventions. Confirm whether automation
   owners need a separately configured higher cap.
6. **Legacy pools:** recommendation is separate feature-gated portable pools
   during rollout. Confirm whether mixed legacy/portable entries are worth the
   additional creator-key ownership metadata and selection complexity.
7. **Provider-produced tokens:** recommendation is response-only relay with no
   coordinator persistence. Confirm whether deployments require sealed
   persistence for retry; if so, that expands the secret-storage boundary and
   should receive a separate security review.
8. **Session fencing:** recommendation is to require positive, generation-scoped
   fencing before `ready` and force drain for every adapter that cannot prove
   it. Confirm whether reboot is acceptable fencing for providers without a
   gateway or dedicated per-borrow OS principal.
