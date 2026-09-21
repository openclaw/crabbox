# Portable ready-pool access: v1 design

Status: accepted v1 bounded-borrow design; experimental Phase 2 implementation.
This document specifies the portable access contract; availability requires the
explicit client opt-in and coordinator capability described below.

Source: [portable ready-pool access and desired capacity](https://github.com/openclaw/crabbox/issues/1074),
including the [latest maintainer boundary](https://github.com/openclaw/crabbox/issues/1074#issuecomment-5322467977).
The earlier broad design in [the closed design PR](https://github.com/openclaw/crabbox/pull/1204)
is not an approved implementation contract. The current shipping protocol is
documented in [Broker ready pools](../spec/broker.md) and
[pool commands](../commands/pool.md).

## Existing behavior

The coordinator already implements desired capacity, compatible fill claims,
borrow heartbeats, abandonment quarantine, stale pruning, and counters. The CLI
already exposes `--min-ready`, `--max-ready`, and `--compatibility-key` and uses
fill claims before provisioning. This functionality should be extended where
needed, not implemented as a competing capacity algorithm.

Legacy pools match a provider-neutral capability/size key. Opt-in typed pools
add exact repository/cache metadata and provider-scoped immutable image
identity. Neither protocol hands out SSH credentials: the keeper owns the
creation key, and another client cannot use a borrowed lease merely by holding
the borrow token. Typed identity is compatibility evidence, not an access grant.

Current pool mutations use `readyPoolBorrowLock` and `runExclusive` in
`worker/src/fleet.ts`. These serialize operations but do not make their multiple
storage writes one crash-atomic transaction. The new lifecycle must use
`CoordinatorStorage.transaction` for each compound state transition. Existing
in-memory locking alone is insufficient for grant and cleanup journals.

The existing [trusted-operator security model](../../SECURITY.md) remains in
force. Reusable pools support trusted workloads; checkout scrub and reboot do
not provide a hermetic reset against hostile code with guest root access.
Portable access must not advertise mutually untrusted tenant isolation.

## Decision: bounded borrows; renewal deferred

V1 borrows are bounded. A grant's immutable expiry is exactly the borrow hard
deadline disclosed before execution. The default and maximum borrow duration is
30 minutes, capped further by the backing lease hard TTL and the caller's
current authorization expiry. Clients may request a shorter duration. A command
still running at that deadline is interrupted and the CLI reports expiration;
heartbeats cannot extend access or let that command continue.

There is no in-place renewal. A caller needing more time must return and borrow
again, receiving a fresh grant after fresh authorization, possibly on a different
machine. Each borrow uses a new ephemeral key. A heartbeat proves only liveness
for abandonment and quarantine decisions; it never changes the grant expiry or
the borrow hard deadline.

AWS v1 uses reboot fencing as its revocation primitive: remove the exact grant
key and observe a boot change before reuse. Removing a key alone prevents new
SSH authentication but does not terminate existing sessions; accepting a reboot
request alone does not prove that sessions ended. Guest expiry enforcement must
also terminate access at the hard deadline during a coordinator outage.

Uninterrupted renewal is deferred, not rejected. Whole-instance reboot fences
all sessions, including sessions using a replacement grant. A later renewal
design therefore needs provider support for fencing an old grant's sessions
without interrupting its replacement, with fresh authorization and immutable
expiry on every new grant. V1 makes no promise that running commands survive
expiry, return, or fencing.

## Access-grant model

A portable pool is an explicitly enabled protocol separate from both current
pool protocols. Enabling typed identity alone must not enable portable access.
The coordinator gate and client opt-in must both be present; unsupported
coordinators or providers fail closed without legacy fallback.

The client generates an ephemeral Ed25519 key locally for the borrow and sends
only its public key. Its private key stays in an owner-only local file and is
removed after terminal cleanup. The broker never creates, stores, or returns a
shared private key. Client key cleanup is not evidence of remote revocation.

Grant authority binds the authenticated borrower, organization, pool identity,
lease ID, immutable provider resource identity, borrow-token hash, generation,
public-key fingerprint, and immutable expiry. It is also capped by the backing
lease's hard TTL and the caller's authorization expiry. Raw borrow, grant, and
fill-claim tokens are returned only to the authorized caller; persisted lookup
keys and records contain domain-separated hashes. Tokens are not permissions
on their own: each issue, acknowledgement, and any future renewal requires
fresh authentication and current lease-management authorization.

Two-phase acknowledgement separates reservation from usable access:

1. Atomically reserve an eligible lease, advance its generation, and persist a
   pending grant with hashes and a bounded acknowledgement deadline. Return the
   receipt tokens to the authenticated client. No usable key is installed yet.
2. The client acknowledges with the same public key and receipt tokens. Check
   current authorization, hashes, generation, lease identity, and all deadlines
   again. Persist installation intent before calling the provider. Publish an
   active grant only after the adapter confirms installation for that exact
   generation and storage revalidation succeeds.

Do not put provider mutations inside a retried storage transaction. Installation
and revocation use durable operation identities and generation preconditions;
a delayed completion must never activate a newer borrow or restore an old key.
A lost reply must not cause an unconditional second installation. Ambiguous
installation retains cleanup intent and makes the entry unavailable until the
adapter proves fencing or destruction. A pending receipt that expires never
silently returns the machine to ready.

Heartbeat deadlines detect abandoned clients; they do not extend hard access
expiry or grant authorization. Return, timeout, drain, release, identity drift,
or failed revalidation durably invalidates the grant before remote cleanup.
Removing the exact installed key and fencing its active sessions are separate
proof obligations. A returned lease becomes ready only after both succeed and
the checkout scrub remains valid. Failure or uncertainty drains or quarantines
it; a successful API write alone is not revocation proof.

## Provider boundary

Core owns authorization, grant/borrow state, deadlines, capacity, and durable
cleanup intent. An optional provider capability owns enrollment validation,
exact key installation, immutable-resource observation, key revocation,
session fencing, and destruction evidence. No AWS/Azure branching belongs in
the pool state machine. Unsupported capabilities reject enrollment.

The first adapter proof targets AWS Linux, SSM, and reboot fencing, as directed
by the issue. It must verify the exact instance and lease binding before every
mutation, fence delayed SSM work by generation, and observe an actual boot
change or confirmed destruction. An accepted reboot request is not proof that
previous sessions have ended. Ordinary SSH credential or ingress refresh
helpers are not a substitute for this capability.

Portable enrollment must establish the guest support required to enforce the
chosen expiry policy. A coordinator outage or delayed SSM command must not
turn an expired grant back into usable authority. Tests must distinguish guest
expiry enforcement, coordinator quarantine, and observed provider fencing;
none can stand in for the others. Deployment-owned provider authority must
remain available for cleanup after the borrower disappears, without persisting
request-scoped provider tokens in a grant or operation record.

AWS support does not imply Azure or GCP support. Each adapter must prove the
same contract before advertising the capability. Logical compatibility stays
provider-neutral, while an image-pinned typed cohort remains provider-scoped;
portable access must not silently merge distinct typed image identities.

## Reconciler state machine

The lifecycle is:

```text
fill claim -> provisioning/hydration -> enrollment -> ready
ready -> pending acknowledgement -> installing -> busy
busy -> revoking/fencing -> verified scrub -> ready
pending/installing/busy -> quarantine or drain -> confirmed destruction
ready near hard TTL -> drain -> confirmed destruction
confirmed terminal state -> retention window -> prune
```

Every storage transition must atomically update the affected lease reservation,
pool entry, grant, fill claim, counters, and durable wake intent as applicable.
Generate randomness and calculate immutable inputs before entering a retriable
transaction. Re-read authoritative records in the transaction and write only
the observed generation. Do not write a pre-provider-call snapshot over a
newer heartbeat, release, or registration.

Reconciliation retains the existing `minReady`/`maxReady` and compatibility
semantics. It atomically counts compatible ready/busy capacity and outstanding
claims before issuing a fill claim. Registration consumes the exact claim in
the same transaction that registers its compatible lease. An unexpired claim
already funding provisioning survives later desired-policy changes. Actual
ready capacity, not another keeper's claim, determines `pool ensure` success.

Pending access, installation, fencing, and unconfirmed cleanup retain their
capacity reservation so failed grants cannot trigger unbounded replacement
provisioning. Admission and maintenance exclude leases approaching hard TTL;
rotation must reserve replacement capacity within the maximum. No grant may
outlive its bound lease or resurrect an expired generation.

Expired acknowledgements and missed heartbeats invalidate access and enter
quarantine/drain. They never become ready merely because time passed or a late
heartbeat arrived. Cleanup is retryable from durable intent after a restart.
Terminal display records can age out, but pruning must not discard the only
resource binding or revocation/fencing work for a still-existing machine.

Extend existing state and hit/miss counters with grant issue, acknowledgement,
expiry, revocation, fencing failure, and TTL-rotation outcomes. Record borrow
latency and ready-queue age in the broker, and first-command latency and scrub
duration/failure through validated client completion reports. Client timings
are operational telemetry, never authorization or reuse proof. Avoid token,
key, or unconstrained repository labels in metrics.

## Compatibility and rollback

Existing legacy and typed records keep their current routes, namespaces,
client-owned SSH keys, and negotiated heartbeat behavior. Do not reinterpret
their stored raw borrow tokens as hashed portable tokens. There is no automatic
migration, shared-key distribution, or fallback from a portable request to
either older protocol.

Portable entries, grants, fill claims, desired capacity, counters, and cleanup
journals need distinct versioned namespaces that old coordinator prefix scans
cannot enumerate. Enrollment must reject a live legacy/typed reservation for
the same lease; old registration/borrow paths must likewise reject a portable
reservation. Rollback safety also requires that a portable lease was never
left borrowable in an older pool namespace. A namespace split alone does not
provide a remote revocation or maintenance service after rollback: drain and
verify portable cleanup before disabling its controller.

The CLI adds `--access` to manual typed pool commands and `--pool-access` to
`run`/`prewarm`, prints pending versus active access distinctly, and reports the
hard deadline and fencing/cleanup status. Manual operations need a protected receipt
file instead of tokens on argv. `run --pool` must preserve command failures
while reporting cleanup failure and deleting local ephemeral keys only after
retaining enough receipt state to retry remote cleanup. V1 reports an immutable hard deadline and does not expose a renewal command.

## Implementation proof

Focused Worker tests must cover token-hash persistence, grant binding and
authorization changes, issue/acknowledgement replay, expiry at equality,
delayed installation after invalidation, exact revocation and fencing failure,
transaction rollback, concurrent keepers, abandoned-borrow quarantine, TTL
rotation, and cleanup retention. Include rollback isolation and conflicting
legacy/typed registrations; do not test only the new namespace in isolation.

Focused CLI tests and binary invocations must cover explicit opt-in, unsupported
servers/providers, ephemeral key handling, receipt/output redaction, hard
deadline handling, command/cleanup error precedence, and unchanged legacy and
typed behavior. Long-job tests must prove that heartbeats do not move the disclosed deadline
and that expiry interrupts the command.

Before merge, two independently configured clients must exercise an AWS Linux
pool without sharing a private key. Provider proof must include usable access,
return/reborrow, an already-open session fenced on revocation, killed-borrower
cleanup, ambiguous SSM completion, and confirmed zero remaining paid resources.
Unit tests and a design document cannot establish that provider behavior.

## Implementation locations

`worker/src/ready-pool-access.ts` owns transactional grant state and cleanup
journals. Pool deadlines share the runtime’s ordered durable due index; normal
heartbeats retain a bounded lookup, and the pool controller visits only due
bindings. The existing pool reconciler in `fleet.ts` supplies capacity claims,
compatibility filtering, heartbeat deadlines, counters, and terminal pruning;
portable records use separate namespaces. `aws-pool-access.ts` owns the SSM guest
contract. `internal/cli/ready_pool_access.go` owns ephemeral keys and receipts.
The live provider proof above remains a prerequisite for production rollout; mocked
provider tests do not establish that live AWS session fencing works.
