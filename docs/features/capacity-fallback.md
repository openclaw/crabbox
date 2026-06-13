# Capacity And Fallback

Read this when:

- adding or changing machine classes;
- debugging "why did Crabbox pick this instance type?";
- working on AWS or Azure Spot/on-demand failover;
- configuring multi-region or multi-AZ capacity for AWS or Azure.

Crabbox treats capacity in three layers:

1. **Class fallback** — the ordered list of provider instance types that a
   class request expands into.
2. **Market fallback** — AWS/Azure Spot-to-On-Demand failover within a class.
3. **Region/AZ routing** — where the brokered AWS or Azure path may launch when
   one region or zone is configured against several.

Hetzner only exercises class fallback. AWS exercises all three; Azure exercises
class, market, and region fallback for VM leases. Static SSH and the delegated
runners (Blacksmith, Daytona, Islo, E2B, Modal, and the rest) have no capacity
fallback because the operator or the external service owns the underlying
resource.

## Classes

Class names are provider-neutral intent labels:

```text
standard  typical CI lane
fast      more cores for parallel-friendly suites
large     memory-heavy or many-process workloads
beast     maximum capacity within the provider's family
```

Each provider maps the four class names to an ordered list of concrete
instance types. That list *is* the fallback chain: try the first; if it is
rejected, try the next; and so on until one succeeds or the chain is exhausted.

The full per-provider class tables (including AWS Windows, Windows WSL2, and
macOS) live in [Providers](providers.md#machine-classes).

## When Class Fallback Triggers

Hetzner falls back when:

- the requested server type is unavailable in the configured location;
- the project quota rejects the request;
- the API returns a transient capacity error.

AWS and Azure fall back when a candidate is rejected by:

- insufficient instance capacity (`InsufficientInstanceCapacity`);
- a vCPU or Spot quota limit (`VcpuLimitExceeded`, `MaxSpotInstanceCountExceeded`);
- an unsupported type or account policy (e.g. Free-Tier restrictions).

Crabbox classifies each provider rejection (capacity, quota, unsupported,
policy, region) rather than guessing, then advances to the next candidate type.
Azure VM leases also bound slow long-running create operations so requests can
advance through the candidate chain instead of waiting on a stalled allocation.
Non-retryable failures stop the chain immediately so a genuine misconfiguration
fails fast instead of churning through every type.

When the chain is exhausted the command fails. On the brokered (Worker) path
the failure carries `provisioningAttempts` — one record per type tried, with
its region, market, failure category, and message — and the same metadata is
attached to the failed lease record on the coordinator, so an operator can see
what went wrong without rerunning the workflow:

```sh
crabbox history --lease cbx_...
```

## Explicit Type Override

`--type c7a.16xlarge` (and the matching `type:` config key) pins one exact
instance type and skips the class fallback chain. The contract is "give me this
type, not a substitute": if the provider rejects it, Crabbox fails loudly rather
than silently choosing a different type.

Use `--type` when:

- you want deterministic capacity for a benchmark;
- you are pinning a specific generation to work around a known issue;
- you are debugging the capacity layer itself.

For everything else, prefer a class so the fallback chain absorbs transient
rejections without operator intervention.

## AWS And Azure Market Fallback

AWS and Azure support two markets: `spot` and `on-demand`.

```yaml
capacity:
  market: spot
  fallback: on-demand-after-120s
```

`capacity.market: spot` (the default) requests Spot capacity first.
`capacity.fallback` controls whether a rejected Spot request retries the same
class on On-Demand. Any value that starts with `on-demand` (the shipped default
is `on-demand-after-120s`) enables the On-Demand retry pass; set it to `none`
(or leave it empty) to never fall back.

The On-Demand pass runs after every Spot candidate in the class chain has been
tried and rejected — it reruns the same chain on On-Demand. AWS fallback fires
on provider rejection. Azure also treats a slow Spot VM provisioning operation
as a capacity miss after the configured `on-demand-after-*` duration, or after
the default 120 seconds when on-demand fallback is disabled with `spot-only` or
`none`.
Slow Azure On-Demand creates are bounded too, so the coordinator can keep trying
the class chain before the CLI lease wait expires.

Per-command overrides:

```sh
crabbox warmup --market spot
crabbox run --market on-demand -- pnpm test
```

`--market` overrides `capacity.market` for one lease without editing repo
config. It must be `spot` or `on-demand`. Reach for it when an account is
temporarily out of Spot quota or when Spot interruption rates spike. The same
value is also available as the `CRABBOX_CAPACITY_MARKET` environment variable.

## AWS Capacity Hints

The brokered AWS path can preflight large requests against Service Quotas:

```yaml
capacity:
  hints: true
```

When `hints` is enabled (the shipped default), the broker checks the applied
Spot or On-Demand vCPU quota for each candidate type before launching it.
Candidates that exceed the quota are recorded as a quota attempt and skipped, so
the chain spends launch attempts only on types the account can actually run.
Brokered failures also emit advisory `CapacityHint` records (for example, when a
large class is under capacity pressure) alongside `provisioningAttempts`.

The set of classes treated as high-pressure for hint purposes is controlled on
the broker by the `CRABBOX_CAPACITY_LARGE_CLASSES` environment variable
(comma-separated, default `beast`); it is not a repo config key.

Hints apply only on the brokered path. Direct AWS mode still walks the class
chain but runs no quota preflight. To preflight quota before the first warmup in
either mode, run:

```sh
crabbox doctor --provider aws
```

It reports the applied EC2 vCPU quota for the relevant market(s) and warns when
the default class needs more vCPUs than the account is allowed.

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

`regions` is the ordered list of brokered AWS regions the coordinator considers.
Single-region setups use `aws.region` and leave `capacity.regions` empty;
multi-region setups list every region the broker may launch into. Regions are
tried in order: when every candidate type in a region is rejected for a
retryable reason, Crabbox advances to the next region.

Brokered Azure uses `CRABBOX_AZURE_REGIONS` on the coordinator for its Azure-specific
region list, so AWS and Azure do not accidentally share incompatible region
names. The CLI-level `capacity.regions` list is still included in Azure lease
requests for direct or explicitly shared setups.

`availabilityZones` narrows zone selection within a region (used today mainly
for EC2 Mac Dedicated Host allocation). Zones that do not belong to the active
region are ignored.

Both keys also accept comma-separated values from the
`CRABBOX_CAPACITY_REGIONS` and `CRABBOX_CAPACITY_AVAILABILITY_ZONES`
environment variables.

## Capacity Strategy

```yaml
capacity:
  strategy: most-available
```

`capacity.strategy` is accepted and persisted (it is one of `most-available`
[default], `price-capacity-optimized`, `capacity-optimized`, or `sequential`),
and it shows up in `crabbox config show`. It does not yet change provisioning
behavior: both direct and brokered AWS provisioning currently follow the class
chain in order. Treat the field as forward-looking configuration rather than a
behavior switch, and prefer ordering the class chain itself when you need a
specific preference today.

## Direct Mode Differences

Direct provider mode (no coordinator) supports class fallback and the
spot-to-on-demand failover above, but has no Service Quotas preflight, no
`provisioningAttempts` metadata, and no central run history. If a direct AWS run
fails to provision, rerun it once through the broker to capture structured
`provisioningAttempts` evidence, then return to direct mode for the rest of the
iteration loop.

## Failure Surface

`crabbox` returns `0` on success. Crabbox-internal failures — including capacity
exhaustion, provisioning that never reached SSH, and a lease that expired during
a long warmup — are reported before the remote command runs and exit non-zero;
when the remote command does run, its own exit code is passed through verbatim.
There is no fixed numeric enum for the internal categories (see the
[CLI exit codes](../cli.md#exit-codes) reference); branch on `0` versus non-zero
and read stderr or `--json` for the reason.

The error message names the class chain, the markets that were tried, and — for
brokered runs — the `provisioningAttempts` you can inspect with `crabbox history
--lease cbx_...`.

Related docs:

- [Providers](providers.md)
- [AWS](../providers/aws.md)
- [Hetzner](../providers/hetzner.md)
- [Cost and usage](cost-usage.md)
- [Orchestrator](../orchestrator.md)
- [Operations](../operations.md)
