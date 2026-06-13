# Orchestrator

Crabbox has one logical orchestrator, `FleetCoordinator`, with two supported
deployment runtimes:

- Cloudflare Worker fronting one Fleet Durable Object;
- Node.js service backed by PostgreSQL and pg-boss.

It is the control plane for brokered leases: lease identity, provider
credentials, server lifecycle, expiry, cost guardrails, and usage accounting.
The CLI still performs all data-plane work (SSH, rsync, command execution)
directly against the runner host, even in brokered mode.

For the broader request flow and the Worker's internals, see
[Architecture](architecture.md) and the
[Coordinator](features/coordinator.md) feature doc.

## Brokered vs direct

How a command reaches a provider is decided by `loadBackend`
(`internal/cli/provider_backend.go`):

- **Brokered.** The provider's spec advertises coordinator support (`aws`,
  `azure`, `gcp`, `hetzner`) *and* a broker URL is configured
  (`CRABBOX_COORDINATOR` or `config set-broker`). Lease lifecycle calls go
  through the coordinator over HTTP; the CLI still opens SSH and runs commands
  directly against the box.
- **Direct.** A coordinator-capable provider with no broker configured, or any
  other SSH-lease provider. The CLI talks to the cloud API itself; there is no
  central control plane.
- **Delegated.** Sandbox/run providers that own sync and execution end to end.
  The CLI never opens its own SSH or rsync session.

The four brokerable providers run direct unless a broker is configured, so
"brokered" is a deployment choice, not a property of the provider alone.

## Responsibilities

The orchestrator owns:

- lease IDs and lease state;
- provider credentials;
- server creation and deletion;
- idle expiry and heartbeat renewal;
- pool listing;
- cost controls and usage estimates;
- status and audit lookups.

The CLI owns:

- local config and claims;
- per-lease SSH key creation;
- SSH readiness waits;
- rsync of the dirty working tree;
- remote command execution;
- output streaming.

## Lease states

The coordinator stores brokered leases in one of four states:

```text
active
released
expired
failed
```

Provider runner labels carry finer machine-level states such as `leased`,
`ready`, and `running`; these are used by direct-mode cleanup and the portal
grid but are not the coordinator's authoritative lease state.

## Heartbeats and idle timeout

A lease expires at the earlier of its wall-clock TTL and its idle window:

```text
expiresAt = min(createdAt + ttl, lastTouchedAt + idleTimeout)
```

`--ttl` is the maximum lifetime; `--idle-timeout` is the inactivity window.
Defaults are a 5400 s TTL (capped at 86400 s) and a 1800 s idle timeout. Set
them per command, for example:

```bash
crabbox warmup --ttl 2h --idle-timeout 30m
crabbox run --idle-timeout 30m -- ./run-tests.sh
```

While a lease is in use the CLI sends `POST /v1/leases/{id}/heartbeat`. Each
heartbeat bumps `lastTouchedAt`, recomputes `expiresAt`, clears any pending
cleanup metadata, refreshes provider SSH access when source CIDRs are known,
and reschedules the runtime's durable maintenance deadline.

For Linux leases the heartbeat also carries best-effort telemetry whenever SSH
is reachable (source `ssh-linux`): load, memory, disk, and uptime. The Durable
Object keeps the latest sanitized sample on the lease plus a bounded
`telemetryHistory` ring of the 60 most recent samples for compact portal
trends. Active runs can append their own samples through the run telemetry
endpoint, so longer commands show short trend lines on the run detail page
instead of only start/end deltas. See [Telemetry](features/telemetry.md).

Direct-provider mode has no central heartbeat or alarm. It labels machines with
`created_at`, `last_touched_at`, `idle_timeout_secs`, `expires_at`, `state`,
`lease`, and `slug`; `crabbox cleanup` reads those labels conservatively.

Delegated external runners (for example Blacksmith Testboxes) are
visibility-only records. `crabbox list --provider blacksmith-testbox` syncs the
current runner table into muted portal rows with inferred GitHub Actions
run/workflow links and status badges, and a later sync marks missing runners
stale. These rows do not heartbeat and do not participate in lease expiry,
cleanup, or cost accounting.

## Cleanup

Brokered cleanup is owned by the coordinator scheduler: a Durable Object alarm
plus scheduled Worker reconciliation, or pg-boss jobs plus recurring
reconciliation. `crabbox cleanup` refuses to run when a coordinator is
configured — sweeping provider resources from the CLI could delete live
brokered leases. When provider deletion fails during TTL cleanup, the
coordinator keeps the lease `active`, records `cleanupAttempts`,
`cleanupError`, and `cleanupRetryAt`, and reschedules the alarm (retry after
5 minutes) rather than marking the lease `expired` while the machine may still
exist. On success the lease moves to `expired`.

Maintenance runs at the earliest active-lease expiry or AWS orphan-sweep time.
The orphan sweep (report or delete mode, gated by
`CRABBOX_AWS_ORPHAN_SWEEP_*`) terminates untracked instances and releases idle
Mac dedicated hosts.

Direct cleanup only deletes machines that are clearly safe:

- `keep=true` is skipped;
- `running` and `provisioning` are skipped until past expiry plus 12 hours;
- expired `ready`/`leased`/`active` machines are deleted once past expiry;
- `failed`/`released`/`expired` machines are deleted;
- a machine with no labels or no `expires_at` is left alone.

Direct AWS security-group maintenance prunes Crabbox-owned SSH ingress rules
before adding the current source CIDRs, including old fallback ports that are
no longer configured, and leaves non-Crabbox rules untouched.

See [Lifecycle and cleanup](features/lifecycle-cleanup.md) for the full
runner-state model.

## Cost control

Before creating a machine the orchestrator estimates its worst-case cost,
multiplies the hourly rate by the lease TTL, and reserves that amount for the
current month. This is a guardrail, not a billing export.

The hourly rate is resolved in this order:

1. an explicit override from `CRABBOX_COST_RATES_JSON` (key `provider:type`);
2. live provider pricing when available;
3. a built-in static default (final fallback ~`$3`/h for AWS, `$0.50`/h
   otherwise).

Live pricing today:

- **AWS:** `DescribeSpotPriceHistory` for the requested instance type and region.
- **Hetzner:** Cloud API server-type prices for the requested location; hourly
  EUR prices are converted with `CRABBOX_EUR_TO_USD` (default `1.08`).

```text
CRABBOX_COST_RATES_JSON='{"aws:c7a.48xlarge":9,"hetzner:ccx63":1.08}'
CRABBOX_EUR_TO_USD=1.08
```

Limits are read from the coordinator environment:

```text
CRABBOX_MAX_ACTIVE_LEASES
CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER
CRABBOX_MAX_ACTIVE_LEASES_PER_ORG
CRABBOX_MAX_MONTHLY_USD
CRABBOX_MAX_MONTHLY_USD_PER_OWNER
CRABBOX_MAX_MONTHLY_USD_PER_ORG
CRABBOX_DEFAULT_ORG
```

Owner and org identity drives the per-owner and per-org limits. For signed
GitHub login tokens, owner/org is embedded in the bearer token and forwarded to
`FleetCoordinator`. In shared-token automation the CLI sends `X-Crabbox-Owner`
from `CRABBOX_OWNER`, then `GIT_AUTHOR_EMAIL`/`GIT_COMMITTER_EMAIL`, then local
`git config user.email`; it sends `X-Crabbox-Org` from `CRABBOX_ORG` when set.
Raw Cloudflare Access identity headers are ignored — only a verified Access JWT
email can become the bearer-token owner.

If a new lease would exceed any configured active-lease count or monthly
reserved-cost budget, the create call returns HTTP 429 `cost_limit_exceeded`
and no machine is provisioned. See [Cost and usage](features/cost-usage.md).

## Usage statistics

The coordinator exposes `GET /v1/usage`. `crabbox usage` reports a single user,
an org, or the whole fleet for a month:

```bash
crabbox usage --scope user --user alice@example.com --month 2026-05
crabbox usage --scope org --org example-org
crabbox usage --scope all --json
```

A report includes lease count, active-lease count, elapsed runtime, estimated
elapsed cost, reserved worst-case cost, and breakdowns by owner, org, provider,
and server type.

## Actions parity boundary

Actions-backed lanes can use local SSH hydration for ordinary repository setup.
They need a real GitHub Actions job only when they require Actions secrets,
OIDC, service containers, or unsupported `uses:` steps.

The bridge is `crabbox init`, which generates a repo-local workflow and agent
instructions so `warmup`/`run` can hydrate the same dependencies CI uses. The
GitHub fallback registers ephemeral self-hosted runners or dispatches a
configured workflow for full secrets/OIDC parity. See
[Actions hydration](features/actions-hydration.md).
