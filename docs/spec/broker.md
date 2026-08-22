# Broker ready pools

This is the generic broker-side contract for prewarmed capacity. For the full
broker mental model, including CLI/broker/provider ownership, auth, lifecycle,
cleanup, and cost controls, read [How Crabbox Works](../how-it-works.md) and
[Orchestrator](../orchestrator.md) first.

Broker ready pools keep hydrated leases available before a test run asks for
one. The goal is that the first command pays only borrow, SSH, optional sync,
and command time; image boot and repository setup happen ahead of demand.

## Goals

- Keep at least one ready lease per configured pool.
- Use the same pool contract for AWS, Azure, GCP, and other brokered SSH
  providers.
- Hydrate from the repository's configured GitHub Actions workflow so the box
  matches CI setup.
- Prefer provider images for the base operating system and toolchain, then use
  Actions hydration for the latest repository state.
- Return successful leases to the pool and drain failed or unhealthy leases.

## Pool identity

A pool key names one logical lease class:

```text
<repo>/<ref>/<target>
```

Examples:

```text
example/app/main/linux
```

The key is operator-chosen and normalized by the broker. An optional
provider-neutral compatibility key identifies a capability and size class such
as `linux-16-vcpu`; compatible AWS and Azure entries can therefore satisfy the
same logical pool. Entries also record repo, ref, commit, fingerprint, image,
provider, target, server type, SSH endpoint, work root, owner, org, state, and
expiry.

For image-backed pools, use a versioned typed identity:

```json
{
  "schema": "crabbox-ready-pool-identity/v1",
  "profile": "linux-builder",
  "recipeDigest": "sha256:<digest>",
  "inventoryDigest": "sha256:<digest>",
  "imageID": "<immutable-provider-image-id>",
  "architecture": "amd64",
  "seedDigest": "sha256:<digest>",
  "cacheABIDigest": "sha256:<digest>"
}
```

Every field matches exactly; there are no wildcards. `seedDigest` binds the
repo, ref, commit, and setup fingerprint. `compatibilityKey` remains an
independent provider-neutral selector. Identity records contain only digests
and opaque image IDs, never raw inventory or tenant-sensitive values.
The broker recomputes the canonical seed digest from request metadata on typed
register, reconcile, and borrow rather than trusting the supplied digest.
The v1 seed preimage is byte-defined rather than JSON-defined: the UTF-8 domain
`crabbox-ready-pool-seed/v1\0`, followed in repo/ref/commit/fingerprint order by
a one-byte field tag, a four-byte big-endian UTF-8 byte length, and the exact
field bytes. Empty fields have length zero. Each field must be valid UTF-8 and
at most 1024 bytes; clients and the broker reject larger values before hashing.
Typed metadata is stored and compared exactly, without truncation.

Typed and legacy entries never cross-match, and legacy records are not
backfilled. An unknown stored schema remains visible and can be drained or
released, but is never borrowable. Requests with unknown required schemas fail
closed.

## States

```text
ready      hydrated, active, borrowable
busy       leased to one run
draining   no longer borrowable; release or expiry cleanup owns it
quarantined borrow heartbeat expired; cannot return directly to ready
stale      broker entry exists but the backing lease is gone or expired
```

Only `ready` entries can be borrowed. Borrow marks exactly one entry `busy`.
Return marks it `ready`, `draining`, or releases the backing lease.

## Control plane

Ready-pool APIs live beside normal lease APIs:

```text
GET  /v1/ready-pools
GET  /v1/ready-pools/:key
POST /v1/ready-pools/:key/register
POST /v1/ready-pools/:key/register-identity
POST /v1/ready-pools/:key/borrow
POST /v1/ready-pools/:key/borrow-identity
POST /v1/ready-pools/:key/heartbeat
POST /v1/ready-pools/:key/return
POST /v1/ready-pools/:key/return-identity
POST /v1/ready-pools/:key/reconcile
POST /v1/ready-pools/:key/reconcile-identity
POST /v1/ready-pools/:key/release-fill-claim
GET  /v1/ready-pools/:key/metrics
```

The typed routes are explicit protocol negotiation, so an older coordinator
cannot silently ignore identity fields. Typed registration reads a fresh
root-owned Linux readiness manifest and the broker cross-checks its profile and
digests with the recorded immutable image and architecture. Reusable typed
returns repeat that proof; changed or missing evidence drains the entry.
Provider adapters must persist the concrete selected image identity. For GCP,
boot images, disk snapshots, and machine images are resolved before
provisioning. The typed identity uses Google's server-defined numeric resource
ID, while the resolved self-link or canonical resource name remains the launch
selector. Bare resource resolution and instance provisioning use the
per-request project. A missing or nonnumeric resource ID fails closed.

The broker stores pool entries in coordinator storage. The CLI owns SSH
keys, source sync, and Actions hydration, so it registers a lease only after it
has proved the remote endpoint and setup. The broker is the arbiter for
exclusive borrow/return, desired-capacity fill claims, and borrow deadlines. It
uses the recorded SSH endpoint so provider-specific port fallback does not
repeat on every hot run. The coordinator does not issue SSH credentials; fill
keepers and borrowers retain the existing client-owned access contract.

## CLI flow

Prewarm and register:

```sh
crabbox prewarm --pool example/app/main/linux \
  --pool-compatibility-key linux-16-vcpu \
  --pool-identity-file ./ready-pool-identity.json \
  --provider azure \
  --type Standard_D2ads_v6 \
  --market on-demand \
  --probe-command 'node -v && pnpm -v'
```

Borrow for a run:

```sh
crabbox run --pool example/app/main/linux \
  --pool-compatibility-key linux-16-vcpu \
  --pool-identity-file ./ready-pool-identity.json -- pnpm test
```

Manual operations:

```sh
crabbox pool ready
crabbox pool register example/app/main/linux --id cbx_... --identity-file ./ready-pool-identity.json
crabbox pool borrow example/app/main/linux --identity-file ./ready-pool-identity.json
crabbox pool heartbeat example/app/main/linux --id cbx_... --borrow-token <token>
crabbox pool return example/app/main/linux --id cbx_... --result ready \
  --borrow-token <token> --identity-file ./ready-pool-identity.json
crabbox pool ensure example/app/main/linux --min-ready 2 --max-ready 4 \
  --compatibility-key linux-16-vcpu --identity-file ./ready-pool-identity.json --create -- \
  --provider aws --type c6i.4xlarge
```

## Capacity algorithm

Each pool has `minReady`, default `1`, and `maxReady`, which defaults to
`minReady`. `pool ensure` persists that desired state and asks the singleton
coordinator reconciler for at most one short-lived fill claim at a time. Ready,
busy, and in-flight claimed entries count toward `maxReady`; the claim is
consumed only when a compatible hydrated lease registers. This makes repeated
or concurrent keepers safe without moving provisioning or hydration into the
coordinator.
Once issued, an unexpired claim is not revoked by later desired-capacity or
compatibility changes. Those changes only gate issuance of new claims, so a
keeper that has already paid the provisioning and hydration cost can still
register its lease.

A future autoscaler can adjust the persisted target using observed demand:

```text
targetReady = max(minReady, ceil(recentPeakConcurrentBorrows * 1.25))
targetReady = clamp(targetReady, minReady, maxReady)
```

Use a short lookback for bursts, such as 30 minutes, and a longer decay window,
such as 4 hours, before reducing `targetReady`. A keeper with a fill claim
creates a lease from the promoted provider image, hydrates it with the
configured workflow for the current ref, probes it, and registers it. If
entries exceed target and are idle past the pool idle window, mark them
draining and release the oldest first.
`crabbox pool ensure --create` forwards provider sizing flags to `prewarm`, but
repo/ref overrides must come from config so creation and readiness counting use
the same borrow criteria.

Borrow heartbeat enforcement is negotiated per borrow. `crabbox run --pool`
opts in and refreshes its two-minute deadline every 30 seconds. Manual and
older-client borrows have no deadline until their first successful
`pool heartbeat`, which opts them in. A missed negotiated deadline quarantines
the entry so a late borrower cannot silently return it to ready. This keeps a
new worker compatible with already-deployed CLIs during rollout. Stale and
quarantined records are pruned after 24 hours. The metrics route reports current
state counts plus borrow, hit/miss, fill, heartbeat, quarantine, and prune
counters.

## Images and hydration

Images are the base layer: operating system, runner user, SSH, language
toolchains, package managers, Docker, and heavy system packages. Actions
hydration is the repo layer: checkout, dependency install, generated caches,
and project-specific setup from the same workflow that CI uses.
The hydration marker records the checked-out commit so return-time scrub can
drain a lease whose branch advanced beyond its prepared dependency state.
Legacy or custom markers without the commit field drain on return; regenerate
the workflow with `crabbox init --force` or add the checked-out `COMMIT` field
before replenishing the pool.

When the repository's main ref moves, new pool entries should hydrate the new
commit. Existing ready entries with older commits can serve only requests that
do not require the newer commit; otherwise they become stale and drain after
their current borrow or idle window.

## Return rules

`crabbox run --pool` defaults to `--pool-return auto`:

- command success: scrub the checkout, then return `ready`
- ordinary nonzero command exit: preserve the command failure and return
  `drain`
- transport, cancellation, stream/capture, artifact, or other lifecycle
  failure, failed scrub, SSH failure during scrub, or failed hydration marker
  verification: return `drain`
- explicit `--pool-return ready`: request reuse, still gated by the scrub
- explicit `--pool-return drain`: release backing lease after the run
- explicit `--pool-return release`: release backing lease immediately

Before borrow, Crabbox captures a canonical origin from the local checkout and
proves its HTTPS origin supports a credential-free fetch. Reusable pools reject
SSH, local/file, query/fragment-bearing, and private credential-backed origins; forced `drain` and
`release` policies do not need that contract. The scrub replaces task-mutable
remote Git metadata from the trusted origin, fetches the pool entry's recorded
branch, resets tracked and non-ignored untracked files, removes Crabbox
run-local state, and clears stale sync metadata before verifying the prepared
commit and clean worktree. Ignored task state is removed, while successful
commands may retain explicitly ignored dependency install trees. An
Actions-hydrated entry remains reusable only while its recorded hydration
commit matches the prepared branch commit. Submodule worktrees drain instead
of being reused. Ready-pool reuse is for trusted workloads; this
scrub prevents task-state confusion, not cross-tenant isolation or a hermetic
host reset. Every entry advances to the latest remote branch
commit. If that moves an exact-commit
entry beyond its recorded commit, Crabbox drains the entry after the scrub so a
later exact `--no-sync` borrow cannot use stale metadata.

Reusable returns keep the lease active and borrowable. Drained returns are no
longer borrowable and release the cloud machine to avoid poisoning the pool.
Pooled runs reject full resync because that can replace the hydrated workspace.
Reusable pooled runs require a branch ref. Pooled `--no-sync` runs also require
an exact commit match and do not borrow ref-only entries. Exact SHA and tag refs
require a forced `drain` or `release` policy.

## Provider contract

Providers do not need pool-specific runtime logic. They must provide normal
brokered SSH leases with stable lease records, expiry, SSH endpoint metadata,
and release semantics. Provider-specific image selection remains in the normal
lease request fields, so AWS, Azure, and GCP use the same register, borrow, and
return path.
