# Vision

Crabbox should feel like one remote execution tool, not a set of provider-shaped workflows. Core owns lease orchestration, identity, cost limits, sync, command execution, run history, and cleanup timing. Providers own how their infrastructure satisfies those requests.

## Provider Boundaries

Core can pass generic context into providers:

- request source CIDRs
- active lease records
- normalized lease config
- provider-neutral lease metadata
- provider capability hooks for defaults, access, provisioning, native images, release, cleanup, and diagnostics

Providers must own provider-specific policy:

- firewall and security-group reconciliation
- host, region, zone, subnet, and default image selection
- native image creation, lookup, promotion, deletion, and provider image metadata
- labels, tags, and orphan-detection details
- resource names and provider API identifiers
- rollout compatibility for provider-owned state
- provider API retry and partial-failure behavior

For example, core should not know that AWS SSH access is a shared security group that needs additive reconciliation during rollout. Core should pass the current request source and active leases; the AWS adapter decides which CIDRs to persist on the lease, which rules are safe to prune, and when unknown active lease state requires additive sync.

## Design Rule

When a fix needs a provider-specific branch in core, first add or reuse a provider hook. A small provider-neutral hook is usually cleaner than spreading `provider == ...` branches through lease, heartbeat, run, status, or cleanup paths. Provider-specific fields may remain in public config for compatibility, but persisted coordinator state should prefer provider-neutral metadata when the concept applies across providers.

The healthy shape is:

- core validates generic request shape, authorizes the caller, enforces cost limits, persists lease records, and schedules cleanup
- provider adapters resolve provider defaults before readiness checks need them
- provider adapters finalize provider-owned lease metadata after provisioning
- provider adapters own release cleanup, including auxiliary keys or API resources
- image and checkpoint routes dispatch by capability instead of encoding AWS, Azure, or GCP behavior inline

Provider names may still appear in central config schemas, API routing, admin feature gates, and provider registries. That is routing. Once code decides how a cloud resource is named, selected, reconciled, promoted, deleted, or retried, it belongs in the provider adapter.
