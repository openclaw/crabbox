# Capacity And Fallback

Read when:

- adding or changing machine classes;
- debugging "why did Crabbox pick this instance type?";
- working on AWS spot/on-demand fallback or Hetzner location fallback;
- configuring multi-region or multi-AZ capacity for AWS.

Crabbox cares about capacity in three ways:

1. **Class fallback** - the ordered list of provider types that satisfy a
   class request.
2. **Market fallback** - AWS-specific Spot to On-Demand failover within a
   class.
3. **Region/AZ routing** - where the broker tries to provision when capacity
   is tight in a single zone.

Hetzner only deals with class fallback. AWS deals with all three. Static
SSH, Blacksmith, Daytona, and Islo do not have capacity fallback because
the operator or external service controls the underlying resources.

## Classes

Class names are provider-agnostic intent labels:

```text
standard  typical CI lane
fast      ~2x more cores than standard for parallel-friendly suites
large     memory-heavy or many-process workloads
beast     maximum capacity within the provider's burstable family
```

Each provider maps the four class names to an ordered list of concrete
instance types. The list is the fallback chain: try the first; if rejected,
try the second; and so on.

The full Hetzner and AWS class tables live in
[Providers](providers.md#hetzner-summary). The table also lists the AWS
Windows, Windows WSL2, and macOS class maps.

## When Class Fallback Triggers

Hetzner falls back when:

- the requested server type is unavailable in the configured location;
- the project quota rejects the request;
- the API returns a transient capacity error.

AWS falls back when:

- the instance type is rejected by capacity in the chosen Availability Zone;
- the account policy denies the type (e.g. quota = 0 vCPUs);
- the spot request is rejected by capacity.

Quota rejections are detected from the API error code rather than scraped
from the message string, so the fallback is deterministic. The next
candidate in the chain is tried until either one succeeds or the chain is
exhausted.

When the chain is exhausted, Crabbox returns exit code 4 (`no capacity`) and
the error includes `provisioningAttempts` that record which types were
tried, why each failed, and where (region/AZ for AWS). The same metadata is
attached to the failed lease record on the coordinator so operators can
inspect what went wrong without rerunning the workflow.

## Explicit Type Override

`--type c7a.16xlarge` and the matching `type:` config key skip the class
fallback chain and request that specific instance type. The contract is
"give me this exact type, not a fallback". If the provider rejects it,
Crabbox fails loudly with exit code 4 and does not silently choose a
different type.

Use `--type` when:

- you want deterministic capacity for benchmarks;
- you are pinning a specific generation for a known-bug workaround;
- you are debugging the capacity layer itself.

For everything else, prefer a class - the fallback chain handles transient
rejections without operator intervention.

## AWS Market Fallback

AWS supports two markets: `spot` and `on-demand`.

```yaml
capacity:
  market: spot
  fallback: on-demand-after-120s
```

`capacity.market: spot` requests Spot capacity first. `capacity.fallback:
on-demand-after-120s` falls back to On-Demand for the same instance type
when Spot fails to come up within 120 seconds. Set `fallback` to `none` (or
omit it) to never fall back to On-Demand.

Per-command overrides:

```sh
crabbox warmup --market spot
crabbox run --market on-demand -- pnpm test
```

The `--market` flag overrides `capacity.market` for one lease without
rewriting repo config. Use it when an account is temporarily out of Spot
quota or when Spot interruption rates spike.

## AWS Capacity Hints

The brokered AWS path uses Service Quotas and EC2 placement scoring to
preflight large requests:

```yaml
capacity:
  hints: true
  largeClasses:
    - large
    - beast
```

When `hints: true` and the class is in `largeClasses`:

- the broker calls Service Quotas to check applied Spot or On-Demand vCPU
  limits;
- candidates that exceed quota are recorded as quota attempts and skipped;
- remaining candidates are scored with `GetSpotPlacementScores` (Spot mode)
  to pick the most-available region/AZ.

The result is a single provisioning attempt that picks the best location
and skips known-rejected types instead of letting the chain stumble through
them sequentially.

Hints apply only on the brokered (Worker) path. Direct AWS mode still falls
back through the class chain but does not run quota or placement preflight.
`crabbox doctor --provider aws` can run the EC2 vCPU quota preflight before the
first warmup in both direct and brokered setups.

## Region And Availability Zone Routing

```yaml
capacity:
  regions:
    - eu-west-1
    - us-east-1
  availabilityZones:
    - eu-west-1a
    - eu-west-1b
```

`regions` is the ordered list of AWS regions the broker considers when
multiple regions are configured. Single-region setups use `aws.region` and
leave `capacity.regions` empty; multi-region setups list every region the
broker may launch into.

`availabilityZones` narrows the per-region zone selection. The broker uses
Spot placement scoring across the listed AZs and picks the highest-scoring
zone that has capacity.

Regions are tried in order; AZs within a region are scored. If every AZ in
a region rejects the request, Crabbox advances to the next region.

## Fallback Strategies

```yaml
capacity:
  strategy: most-available
```

| Value | Behavior |
|:------|:---------|
| `most-available` (default) | use placement scoring or class chain order |
| `cheapest` | prefer types with the lowest live hourly price (when known) |
| `provider-default` | follow the provider's own placement defaults |

`cheapest` is currently honored on the brokered AWS path that has live
pricing. Hetzner does not differentiate strategies because its server-type
prices are consistent across locations.

## Direct Mode Differences

Direct provider mode (no coordinator) supports class fallback but has no
quota preflight, no placement score, no `provisioningAttempts` metadata, and
no central history. Direct AWS still respects `--market` and the `fallback`
config key, so spot-to-on-demand failover works locally - just without the
diagnostic richness the broker provides.

If a direct AWS run exits with code 4, run the same command through the
broker once to get structured `provisioningAttempts` evidence; then go back
to direct mode for the rest of the iteration loop.

## Failure Surface

Capacity failures map to:

```text
exit 4   no capacity     every candidate in the chain was rejected
exit 5   provisioning failed   a candidate was accepted but never reached SSH
exit 8   lease expired   long warmup exceeded the configured TTL before SSH
```

The accompanying error message names the chain, the markets that were
tried, and (for brokered runs) `provisioningAttempts` you can inspect with:

```sh
crabbox history --lease cbx_...
```

Related docs:

- [Providers](providers.md)
- [AWS](../providers/aws.md)
- [Hetzner](../providers/hetzner.md)
- [Cost and usage](cost-usage.md)
- [Orchestrator](../orchestrator.md)
- [Operations](../operations.md)
