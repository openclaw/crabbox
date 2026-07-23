# Pricing and Costs

Crabbox is open-source software and does not meter or resell compute today.
There is no single Crabbox per-minute price: runtime costs remain with your
workstation, infrastructure account, or selected provider.

## Current Price Boundary

| Component | Current Cost | Who Bills It |
| --- | --- | --- |
| Crabbox CLI and coordinator software | **$0 license fee** under MIT | Nobody |
| Local containers, VMs, and sandboxes | Existing workstation and runtime cost | You or the local runtime vendor |
| Direct cloud VMs and GPU capacity | Provider's current rate | Your cloud or compute provider |
| Hosted sandboxes and dev environments | Provider's plan and usage rate | The selected sandbox provider |
| Cloudflare coordinator | Your Worker, Durable Object, storage, and related usage | Cloudflare |
| Node.js coordinator | Your service, PostgreSQL, queue, network, and storage | Your infrastructure provider |

Crabbox does not currently publish a hosted coordinator plan or a separate
Crabbox usage markup. If a provider requires a subscription, credits, or API
usage, that agreement remains between the operator and that provider.

`crabbox marketplace` is preview-only. Configured retail markups affect
advisory quotes, but no payment is captured, credits are not reserved, and a
quote does not provision a lease.

## What Crabbox Tracks

For coordinator-backed providers, Crabbox records lease counts, active leases,
runtime, estimated elapsed cost, and worst-case reserved cost. It can enforce
fleet, owner, and organization limits before a new lease is provisioned.

```sh
crabbox usage --scope user
crabbox usage --scope org --org example-org
```

The estimate is an operational guardrail, not invoice reconciliation. Static IP
charges, egress, snapshots, taxes, credits, negotiated discounts, and every
provider-specific extra are not all modeled.

Direct-from-CLI and delegated-provider spend does not pass through the
coordinator, so `crabbox usage` cannot present it as one consolidated invoice.

## How Rates Are Estimated

Coordinator-backed estimates use this order:

1. An operator-supplied `CRABBOX_COST_RATES_JSON` override.
2. Live provider pricing where the adapter implements it.
3. A checked-in provider and server-type fallback.
4. A generic final fallback.

Before provisioning, admission rejects a candidate when its reserved-cost
estimate for the requested type and TTL would cross a configured limit. This is
not a hard provider-invoice ceiling: the final provider or type price can be
updated after provisioning, and provider-specific extras are not all included.

See [Cost and Usage](features/cost-usage.md) for rate precedence, environment
variables, identity rules, and the exact budget calculation.

## Keep Spend Predictable

- Start with `crabbox providers recommend cost-control` to favor local runtimes,
  cleanup, cache reuse, reusable state, and coordinator governance.
- Use one-shot `crabbox run` to apply the provider's normal post-run release
  policy; check the provider page when release retains or pauses capacity.
- Give retained boxes an explicit idle timeout and TTL.
- Reuse a warm box or cache when setup time dominates the run.
- Set fleet, per-owner, and per-organization active-lease and monthly limits on
  the coordinator.
- Verify the selected provider's current rate, quota, architecture, and region
  before launching expensive or GPU-backed capacity.

## Related Docs

- [Cost and Usage](features/cost-usage.md)
- [`usage` Command](commands/usage.md)
- [Infrastructure](infrastructure.md)
- [Use Cases](use-cases.md)
