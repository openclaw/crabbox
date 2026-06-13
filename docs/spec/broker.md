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

A pool key names one interchangeable lease class:

```text
<repo>/<ref>/<provider>/<target>/<type>
```

Examples:

```text
example/app/main/aws/linux/c6i.2xlarge
example/app/main/azure/linux/standard-d2ads-v6
```

The key is operator-chosen and normalized by the broker. Entries also record
repo, ref, commit, fingerprint, image, provider, target, server type, SSH
endpoint, work root, owner, org, state, and expiry.

## States

```text
ready      hydrated, active, borrowable
busy       leased to one run
draining   no longer borrowable; release or expiry cleanup owns it
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
POST /v1/ready-pools/:key/borrow
POST /v1/ready-pools/:key/return
```

The broker stores pool entries in coordinator storage. The CLI owns SSH
keys, source sync, and Actions hydration, so it registers a lease only after it
has proved the remote endpoint and setup. The broker is the arbiter for
exclusive borrow/return and uses the recorded SSH endpoint so provider-specific
port fallback does not repeat on every hot run.

## CLI flow

Prewarm and register:

```sh
crabbox prewarm --pool example/app/main/azure/linux/standard-d2ads-v6 \
  --provider azure \
  --type Standard_D2ads_v6 \
  --market on-demand \
  --probe-command 'node -v && pnpm -v'
```

Borrow for a run:

```sh
crabbox run --pool example/app/main/azure/linux/standard-d2ads-v6 -- pnpm test
```

Manual operations:

```sh
crabbox pool ready
crabbox pool register example/app/main/aws/linux/c6i.2xlarge --id cbx_...
crabbox pool borrow example/app/main/aws/linux/c6i.2xlarge
crabbox pool return example/app/main/aws/linux/c6i.2xlarge --id cbx_... --result ready --borrow-token <token>
crabbox pool ensure example/app/main/aws/linux/c6i.2xlarge --min-ready 1 --create -- \
  --provider aws --type c6i.2xlarge
```

## Capacity algorithm

Each pool has `minReady`, default `1`. A reconciler should run:

```text
targetReady = max(minReady, ceil(recentPeakConcurrentBorrows * 1.25))
targetReady = clamp(targetReady, minReady, maxReady)
```

Use a short lookback for bursts, such as 30 minutes, and a longer decay window,
such as 4 hours, before reducing `targetReady`. If ready entries are below
target, create leases from the promoted provider image, hydrate them with the
configured workflow for the current ref, probe them, and register them. If
entries exceed target and are idle past the pool idle window, mark them
draining and release the oldest first.
`crabbox pool ensure --create` forwards provider sizing flags to `prewarm`, but
repo/ref overrides must come from config so creation and readiness counting use
the same borrow criteria.

## Images and hydration

Images are the base layer: operating system, runner user, SSH, language
toolchains, package managers, Docker, and heavy system packages. Actions
hydration is the repo layer: checkout, dependency install, generated caches,
and project-specific setup from the same workflow that CI uses.

When the repository's main ref moves, new pool entries should hydrate the new
commit. Existing ready entries with older commits can serve only requests that
do not require the newer commit; otherwise they become stale and drain after
their current borrow or idle window.

## Return rules

`crabbox run --pool` defaults to `--pool-return auto`:

- command success: return `ready`
- command failure, SSH failure, failed sync, failed hydration marker read, or
  failed preflight: return `drain`
- explicit `--pool-return ready`: force reuse
- explicit `--pool-return drain`: release backing lease after the run
- explicit `--pool-return release`: release backing lease immediately

Successful returns keep the lease active and borrowable. Drained returns are no
longer borrowable and release the cloud machine to avoid poisoning the pool.
Pooled runs reject full resync because that can replace the hydrated workspace.
Pooled `--no-sync` runs require an exact commit match and do not borrow
ref-only entries.

## Provider contract

Providers do not need pool-specific runtime logic. They must provide normal
brokered SSH leases with stable lease records, expiry, SSH endpoint metadata,
and release semantics. Provider-specific image selection remains in the normal
lease request fields, so AWS, Azure, and GCP use the same register, borrow, and
return path.
