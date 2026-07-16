# Vision

Crabbox should feel like one remote execution tool, not a collection of provider-shaped workflows. You ask for a box, sync your checkout, run a command, and get results back — the same way regardless of which cloud or sandbox actually serves the request.

To keep that experience consistent across ~27 provider adapters, the codebase enforces one rule: **core owns the generic control plane; provider adapters own everything specific to their infrastructure.**

## The split

Core (`internal/cli`, plus the runtime-neutral coordinator in `worker/src`) owns the parts that are the same for every provider:

- identity and authorization
- normalized lease config and lease records
- cost limits and usage accounting
- sync, command execution, and run history
- cleanup timing and expiry scheduling

Provider adapters (`internal/providers/*`, plus the Worker provider modules) own everything that depends on a particular cloud, hypervisor, or sandbox:

- firewall and security-group reconciliation
- host, region, zone, subnet, and default-image selection
- native image creation, lookup, promotion, deletion, and image metadata
- labels, tags, and orphan-detection details
- resource names and provider API identifiers
- rollout compatibility for provider-owned state
- provider API retry and partial-failure behavior

## How they talk

Core never reaches into provider internals. It passes generic context and lets the adapter decide what to do with it. The context core can hand a provider includes:

- the request source CIDRs
- the set of active lease records
- the normalized lease config
- provider-neutral lease metadata
- capability hooks for defaults, access, provisioning, native images, release, cleanup, and diagnostics

The capability hooks are the seam. On the coordinator side, the lease-create path calls `prepareLeaseConfig`, `prepareLeaseCreate`, `createServerWithFallback`, `finalizeLeaseCreate`, `refreshLeaseAccess`, and `hourlyPriceUSD` on the provider module rather than branching on the provider name (`worker/src/fleet.ts`). On the CLI side, providers declare a [`ProviderSpec`](providers/README.md) with a feature set (`ssh`, `crabbox-sync`, `desktop`, `workspace-checkpoint`, ...), and core dispatches by feature, not by identity (`internal/cli/provider_backend.go`).

### Example: AWS SSH access

Core should not know that AWS SSH access is a shared security group that needs additive reconciliation during a rollout. It passes the current request source and the active leases; the AWS adapter decides which CIDRs to persist on the lease, which rules are safe to prune, and when unknown active-lease state forces an additive sync. The same shape applies to GCP firewall rules, Azure network security groups, and any other provider's access model.

## Design rule

When a fix seems to need a provider-specific branch in core, add or reuse a provider hook first. A small provider-neutral hook is almost always cleaner than threading `provider == ...` branches through the lease, heartbeat, run, status, or cleanup paths.

Provider-specific fields may stay in public config for compatibility, but persisted coordinator state should prefer provider-neutral metadata whenever the concept applies across providers.

The healthy shape looks like this:

- **Core** validates the generic request shape, authorizes the caller, enforces cost limits, persists lease records, and schedules cleanup (expiry alarms, retry-on-failure, orphan sweeps in `worker/src/fleet.ts`).
- **Provider adapters** resolve provider defaults before readiness checks need them.
- **Provider adapters** finalize provider-owned lease metadata after provisioning.
- **Provider adapters** own release cleanup, including auxiliary keys and API resources.
- **Image and checkpoint routes** dispatch by capability (for example, the Worker image route calls the provider's `promoteImage` hook) instead of encoding per-cloud behavior inline.

## Where provider names are still allowed

Provider names legitimately appear in central config schemas, API routing, admin feature gates, and provider registries. That is routing, and routing is core's job.

The line is drawn at *behavior*. Once code decides how a cloud resource is named, selected, reconciled, promoted, deleted, or retried, that decision belongs in the provider adapter — never in core.

## Related reading

- [Architecture](architecture.md) — the CLI ⇄ broker ⇄ runner topology.
- [Provider backends](provider-backends.md) and [Provider Reference](providers/README.md) — the adapter contract and capability map.
- [features/provider-authoring.md](features/provider-authoring.md) — how to add a new adapter without leaking specifics into core.
