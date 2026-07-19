# Cost And Usage

Read this when you are:

- changing budget guardrails;
- adjusting how provider prices are looked up;
- interpreting `crabbox usage` output.

The coordinator tracks lease counts, active leases,
elapsed runtime, estimated elapsed cost, and reserved worst-case cost. This is an
operational guardrail, not invoice reconciliation. Provider extras such as static IP
charges, egress, snapshots, taxes, credits, and discounts are not modeled.

Cost tracking only applies to brokered providers (`aws`, `azure`, `daytona`,
`gcp`, `hetzner`). Direct-from-CLI leases never reach the broker and are not
accounted for.

## Reading `crabbox usage`

`crabbox usage` requires a configured coordinator and prints the current month by default.

```bash
crabbox usage --scope all
crabbox usage --scope org --org example-org --month 2026-05
crabbox usage --scope user --user alice@example.com --json
```

Flags:

| Flag      | Default            | Description                                  |
| --------- | ------------------ | -------------------------------------------- |
| `--scope` | `user`             | One of `user`, `org`, `all`.                 |
| `--user`  | (caller identity)  | Owner email to filter by.                    |
| `--org`   | (caller identity)  | Org name to filter by.                       |
| `--month` | current `YYYY-MM`  | Reporting month, UTC.                         |
| `--json`  | `false`            | Emit the raw summary plus limits as JSON.     |

The text output prints a totals line, then breakdowns by owner, org, provider, and
server type (sorted by reserved cost), followed by the active limits:

```text
usage month=2026-05 scope=all
total leases=12 active=2 runtime=18h32m estimated=$24.18 reserved=$61.40
providers:
  aws                      leases=4   active=1   runtime=6h2m     estimated=$12.40   reserved=$36.00
  hetzner                  leases=8   active=1   runtime=12h30m    estimated=$11.78   reserved=$25.40
limits:
  active leases: fleet=10 user=off org=off
  monthly usd:   fleet=$500.00 user=off org=off
```

Limits print `off` when the corresponding budget is unset.

Only admin-token callers can widen the report: non-admin requests are forced to `scope=user`
scoped to the caller's own identity, and the `--scope`, `--user`, and `--org` filters are
ignored. Admin callers may use any scope and filter by an arbitrary owner or org.

## Cost model

Two cost figures are tracked per lease:

- **`estimatedUSD`** — elapsed cost: `runtime_hours x estimatedHourlyUSD`. For active
  leases, runtime is measured to "now"; otherwise to the lease end/release time.
- **`reservedUSD`** — worst-case cost reserved up front: `ttl_hours x hourlyUSD`, captured
  before provisioning. Budget enforcement uses reserved cost, not elapsed cost.

Pricing precedence (highest first):

```text
1. CRABBOX_COST_RATES_JSON explicit override, keyed "<provider>:<serverType>".
2. Provider live pricing:
   - AWS EC2 spot price history.
   - Hetzner Cloud server-type hourly prices.
3. Built-in static fallback rate for the "<provider>:<serverType>" pair.
4. Final fallback: $3.00/hour for AWS, $0.50/hour for any other provider.
```

`CRABBOX_COST_RATES_JSON` is a JSON object mapping `"<provider>:<serverType>"` to an
hourly USD number; non-positive or non-numeric entries are ignored. Hetzner live prices
are quoted in EUR and converted to USD by multiplying with `CRABBOX_EUR_TO_USD`
(default `1.08`).

## Budget guardrails

Budgets are enforced on lease creation. Exceeding any active-lease limit or monthly
reserved-USD budget rejects the request with HTTP 429 (`cost_limit_exceeded`). All
budgets default to `off` when their environment variable is unset or non-positive.
Active limits keep counting a live managed lease after its heartbeat deadline until
cleanup commits a terminal state, because its provider resource may still exist. The
usage summary's active count uses the same definition.

```text
CRABBOX_MAX_ACTIVE_LEASES            fleet-wide active lease cap
CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER  per-owner active lease cap
CRABBOX_MAX_ACTIVE_LEASES_PER_ORG    per-org active lease cap
CRABBOX_CAPACITY_ADMIN_OWNERS        comma-separated owners with an elevated active lease cap;
                                      use github:<numeric-id> for GitHub users
CRABBOX_MAX_ACTIVE_LEASES_PER_CAPACITY_ADMIN
                                      per-owner active lease cap for capacity admins
CRABBOX_MAX_MONTHLY_USD              fleet-wide monthly reserved-USD budget
CRABBOX_MAX_MONTHLY_USD_PER_OWNER    per-owner monthly reserved-USD budget
CRABBOX_MAX_MONTHLY_USD_PER_ORG      per-org monthly reserved-USD budget
CRABBOX_DEFAULT_ORG                  org assigned when no org header is present
```

Monthly budget checks add the candidate lease's `reservedUSD` to the month's existing
reserved total for the relevant scope. Live managed leases keep reserving budget after
a UTC month rollover until cleanup commits a terminal state. A lease that would push
the scope over budget is refused before it provisions.

## Identity for usage accounting

Leases are attributed to an owner and an org. The broker resolves them by token kind:

- **Signed GitHub login token** — carries an immutable `github:<numeric-id>` owner plus
  verified org and display login identity from the payload.
- **Admin token** (`CRABBOX_ADMIN_TOKEN`) — owner is a verified Access JWT email if present,
  else the `X-Crabbox-Owner` header, else `unknown`; org is the `X-Crabbox-Org` header,
  else `CRABBOX_DEFAULT_ORG`, else `unknown`. The CLI sets `X-Crabbox-Owner` from
  `CRABBOX_OWNER`, then `GIT_AUTHOR_EMAIL` / `GIT_COMMITTER_EMAIL`, then
  `git config user.email`, and `X-Crabbox-Org` from `CRABBOX_ORG`.
- **Shared token** (`CRABBOX_SHARED_TOKEN`) — owner is a verified Access JWT email if present,
  else `CRABBOX_SHARED_OWNER`, else `unknown`; org is `CRABBOX_DEFAULT_ORG`, else `unknown`.
  The `X-Crabbox-Owner` / `X-Crabbox-Org` headers are not honored for shared-token requests.
- A missing org displays as `unknown` but uses a distinct accounting identity from an org
  explicitly configured with the label `unknown`.
- Raw Cloudflare Access identity headers are ignored; only a verified Access JWT email
  (validated against `CRABBOX_ACCESS_TEAM_DOMAIN` / `CRABBOX_ACCESS_AUD`) can become the
  bearer-token owner.

## Related docs

- [usage command](../commands/usage.md)
- [Orchestrator](../orchestrator.md)
- [Provider Reference](../providers/README.md)
