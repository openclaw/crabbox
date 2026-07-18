# usage

`crabbox usage` reports lease cost and usage estimates from the broker, broken down by user, organization, or the whole fleet.

This page is the command reference for cost visibility. Keep command-specific behavior here; broker policy and provider internals live in [../orchestrator.md](../orchestrator.md) and [../features/cost-usage.md](../features/cost-usage.md).

```sh
crabbox usage
crabbox usage --scope org --org example-org
crabbox usage --scope user --user github:12345 --month 2026-05
crabbox usage --scope all --json
```

`crabbox usage` requires a configured broker (coordinator). Direct-provider mode keeps no central lease history to query, so the command exits with `usage requires a configured coordinator`.

## Flags

```text
--scope user|org|all   reporting scope (default user)
--user <owner>         owner identity to report (admin only for other owners)
--org <name>           organization to report
--month YYYY-MM        billing month to summarize (default current UTC month)
--json                 print the raw summary and limits as JSON
```

`--scope` must be `user`, `org`, or `all`; any other value exits with code 2. `--month` defaults to the current month in UTC.

## Scopes and authorization

```text
user    a single owner identity (default)
org     a single organization
all     the entire fleet
```

The broker decides what you may see based on how you authenticated:

- GitHub browser-login users always see their own owner/org usage. Requested `--scope`, `--user`, and `--org` values that widen visibility are ignored: the broker forces `scope=user` and reports the logged-in owner.
- Admin-token auth honors `--scope org` and `--scope all`, and may target another owner with `--user` or another org with `--org`.

Owner identity for shared bearer-token mode comes from `CRABBOX_OWNER`, then Git author/committer email env (`GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_EMAIL`), then local `git config user.email`. Set `CRABBOX_ORG` to group leases under an organization. Only a verified Cloudflare Access JWT email can supply the bearer-token owner; raw Access identity headers are ignored.

GitHub browser-login owners use the immutable `github:<numeric-id>` value shown
by `crabbox whoami`; emails and GitHub logins are display information, not owner
selectors. Shared bearer-token owners may still be email-shaped because that
deployment controls their stable configured identity.

## Output

Human output prints one total row, then any non-empty group breakdowns (owners, orgs, providers, server types), then the active limits:

```text
usage month=2026-05 scope=user user=github:12345 org=example-org
total leases=2 active=0 runtime=12m41s estimated=$0.13 reserved=$4.57
owners:
  github:12345             leases=2   active=0   runtime=12m41s    estimated=$0.13     reserved=$4.57
limits:
  active leases: fleet=off user=off org=off
  monthly usd:   fleet=off user=off org=off
```

`--json` emits the same summary and limit data for scripting:

```json
{
  "usage": {
    "month": "2026-05",
    "scope": "all",
    "leases": 6,
    "activeLeases": 0,
    "runtimeSeconds": 13551,
    "estimatedUSD": 0.13,
    "reservedUSD": 4.57,
    "byOwner": [],
    "byOrg": [],
    "byProvider": [],
    "byServerType": []
  },
  "limits": {
    "maxActiveLeases": 0,
    "maxActiveLeasesPerOwner": 0,
    "maxActiveLeasesPerOrg": 0,
    "capacityAdminOwners": [],
    "maxActiveLeasesPerCapacityAdmin": 0,
    "maxMonthlyUSD": 0,
    "maxMonthlyUSDPerOwner": 0,
    "maxMonthlyUSDPerOrg": 0
  }
}
```

## Estimated vs reserved cost

`estimatedUSD` is the elapsed-runtime cost for leases in the selected month.

`reservedUSD` is the worst-case cost based on each lease's TTL. The broker computes reserved cost before provisioning so monthly spend guardrails can reject a lease before any machine is created.

Cost values are estimates for compute leases, not provider invoice reconciliation. They do not fully model provider extras such as static public IP charges, egress, storage, snapshots, taxes, credits, or discounts.

## How rates are chosen

The broker resolves the hourly rate for each lease in this order:

```text
1. CRABBOX_COST_RATES_JSON explicit override (per provider and server type).
2. Provider live pricing:
   - AWS: EC2 Spot price history for the requested instance type.
   - Hetzner: Cloud server-type hourly price for the requested location.
3. Built-in static fallback rates.
```

Explicit overrides are useful for budget policy or conservative accounting:

```sh
export CRABBOX_COST_RATES_JSON='{
  "aws": {
    "c7a.48xlarge": 2.25
  },
  "hetzner": {
    "ccx63": 0.44
  }
}'
```

Hetzner prices are returned in EUR. The broker converts them to USD using `CRABBOX_EUR_TO_USD` (default `1.08`).

## Limits

The `limits` block mirrors the broker's active cost guardrails, read from these
coordinator environment variables:

```text
CRABBOX_MAX_ACTIVE_LEASES
CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER
CRABBOX_MAX_ACTIVE_LEASES_PER_ORG
CRABBOX_CAPACITY_ADMIN_OWNERS
CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN
CRABBOX_MAX_MONTHLY_USD
CRABBOX_MAX_MONTHLY_USD_PER_OWNER
CRABBOX_MAX_MONTHLY_USD_PER_ORG
```

A value of `0` (or unset) means the limit is off and prints as `off`.
