# Vision

Crabbox should feel like one remote execution tool, not a set of provider-shaped workflows. Core owns lease orchestration, identity, cost limits, sync, command execution, run history, and cleanup timing. Providers own how their infrastructure satisfies those requests.

## Provider Boundaries

Core can pass generic context into providers:

- request source CIDRs
- active lease records
- normalized lease config
- provider-neutral lease metadata
- provisioning and cleanup lifecycle hooks

Providers must own provider-specific policy:

- firewall and security-group reconciliation
- host, region, zone, subnet, and image selection
- labels, tags, and orphan-detection details
- rollout compatibility for provider-owned state
- provider API retry and partial-failure behavior

For example, core should not know that AWS SSH access is a shared security group that needs additive reconciliation during rollout. Core should pass the current request source and active leases; the AWS adapter decides which CIDRs to persist on the lease, which rules are safe to prune, and when unknown active lease state requires additive sync.

## Design Rule

When a fix needs a provider-specific branch in core, first add or reuse a provider hook. A small provider-neutral hook is usually cleaner than spreading `provider == ...` branches through lease, heartbeat, run, status, or cleanup paths. Provider-specific fields may remain in public config for compatibility, but persisted coordinator state should prefer provider-neutral metadata when the concept applies across providers.
