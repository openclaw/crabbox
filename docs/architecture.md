# Architecture

Crabbox is a generic remote execution layer for software testing. A local CLI
leases a short-lived machine, syncs the current checkout, runs commands, and
returns logs, timing, results, and artifacts — without baking project-specific
setup into the base image. The boundary is deliberate: Crabbox owns leasing,
connectivity, sync, run recording, and cleanup; the repository under test owns
language runtimes, dependencies, services, and secrets through its own setup,
[Actions hydration](features/actions-hydration.md), devcontainer, Nix/mise/asdf
config, or shell scripts.

The architecture separates the *control plane* from *command execution*. The
broker serializes lease and provider state, while the CLI keeps SSH keys local
and streams command I/O directly between the developer machine and the leased
box. That keeps provider credentials out of the box, keeps user secrets out of
broker state, and leaves room for both plain-SSH providers and delegated runner
systems.

## System Overview

Crabbox has three parts:

- **CLI** — a local Go binary (`cmd/crabbox`, `internal/cli`) used by
  developers, CI operators, and agents.
- **Coordinator** — shared `FleetCoordinator` behavior with either a Cloudflare
  Worker/Durable Object runtime or a Node.js/PostgreSQL runtime (`worker/src`,
  `worker/node`).
- **Runners** — managed cloud machines, self-hosted VMs, BYO SSH hosts, or
  delegated sandboxes that actually run commands. See the
  [provider reference](features/providers.md).

The coordinator manages leases. The CLI executes work. Runners do not call back
to the coordinator for ordinary command execution; lease bridges (WebVNC,
code-server, egress) are the only on-demand paths that route runner traffic
through it.

```text
developer machine
  crabbox CLI ----------- SSH + rsync (data plane) ----------> leased box
    |                                                              ^
    | HTTPS JSON, Bearer auth (control plane)                      |
    v                                                  provider cloud API
coordinator ----------------------------------------------->  (provision)
  Cloudflare Worker + Durable Object
  or Node.js + PostgreSQL/pg-boss
  (lease / run / usage state, cleanup scheduling, live bridges)
```

## Execution Modes

The CLI picks one of four modes per provider in `loadBackend`
(`internal/cli/provider_backend.go`):

- **Brokered (coordinator) mode** — chosen when the provider declares
  `Coordinator: supported` *and* a broker URL is configured
  (`CRABBOX_COORDINATOR` or `config set-broker`). The provider's SSH backend is
  wrapped in a `coordinatorLeaseBackend`: lease lifecycle goes through the
  coordinator over HTTPS, but the CLI still drives SSH, rsync, and command execution
  **directly** to the runner. The brokered set is exactly the four managed cloud
  providers: `aws`, `azure`, `gcp`, `hetzner`.
- **Direct SSH mode** — the provider returns an SSH lease backend but no broker
  is configured. The CLI provisions and connects against the cloud or host API
  itself; no coordinator is involved. The four brokerable providers fall back to this
  when no broker URL is set, and every other SSH-lease provider (`ssh`,
  `parallels`, `proxmox`, `daytona`, `runpod`, and so on) always runs here.
- **Registered direct mode** — `broker.mode: registered` keeps the same direct
  SSH provider lifecycle but registers lease metadata and heartbeats with the
  coordinator. It can list and share portal bridges, but cannot call the
  provider, delete the resource, charge it to managed usage, or place it in a
  ready pool.
- **Delegated mode** — the provider implements a delegated-run backend (e.g.
  `e2b`, `modal`, `cloudflare`, `azure-dynamic-sessions`). The provider owns
  sync and execution end to end; the CLI calls `Warmup`/`Run` and never performs
  its own rsync. Delegated providers reject local-sync flags.

Provider kinds, coordinator modes, and feature sets are declared in each
adapter's `Spec()`; the type definitions live in
`internal/cli/provider_backend.go`.

## Lease Flow (brokered SSH provider)

1. The CLI loads config and authenticates with a signed GitHub login token or a
   shared/admin operator token.
2. The CLI generates a per-lease SSH key under
   `<user-config>/crabbox/testboxes/<lease-id>/id_ed25519` (RSA for AWS/Azure
   Windows).
3. The CLI sends `POST /v1/leases` with the lease ID (`cbx_<12 hex>`), slug,
   provider, target, machine class, TTL, idle timeout, the SSH public key, and
   provider-specific fields.
4. The coordinator validates identity and policy, checks provider readiness,
   and enforces cost/spend caps.
5. `FleetCoordinator` provisions the machine through the provider adapter (with
   region/market fallback) and persists the lease record through its runtime.
6. The broker returns the lease ID, slug, host, SSH user/port, work root, and
   expiry.
7. The CLI waits for the `crabbox-ready` bootstrap marker.
8. The CLI seeds the remote Git tree when possible, compares sync fingerprints,
   and rsyncs changed files (see [Sync](#sync-and-hydration)).
9. The CLI hydrates the worktree against the base ref, optionally via
   [Actions hydration](features/actions-hydration.md).
10. The CLI runs the command over SSH, streaming stdout/stderr (or capturing to
    a local file with `--capture-stdout`).
11. The CLI heartbeats while work runs: each `POST .../heartbeat` touches
    `lastTouchedAt`, recomputes idle expiry up to the TTL cap, and attaches a
    best-effort Linux telemetry snapshot when SSH is reachable.
12. The CLI releases the lease unless `--keep` is set.
13. A Durable Object alarm or pg-boss maintenance job reaps expired leases and
    orphaned cloud resources.

## Coordinator Entry And Auth

`worker/src/coordinator-entry.ts` contains shared routing and auth. Cloudflare's
`worker/src/index.ts` forwards fleet requests to one Durable Object instance
(`FLEET.idFromName("default")`); `worker/node/server.ts` forwards them to the
Node runtime:

- `GET /v1/health` returns liveness; `GET /` redirects to `/portal`.
- `/v1/auth/*`, `/portal/login`, `/portal/logout`, and WebSocket upgrades for
  the live bridges go to `FleetCoordinator`.
- `/v1/internal/*` is 404 externally; runtime schedulers invoke maintenance
  internally.
- Everything else passes through `authenticateRequest` and is forwarded with
  auth context injected via `requestWithAuthContext`.

Auth (`worker/src/auth.ts`) requires a Bearer token, matched in order:
`CRABBOX_ADMIN_TOKEN` (admin), `CRABBOX_SHARED_TOKEN` (non-admin shared), then a
signed user token (prefix `cbxu_`, HMAC-SHA256, 180-day default expiry) minted
after GitHub OAuth login verifies allowed org membership. An optional Cloudflare
Access JWT (`cf-access-jwt-assertion`) can supply the owner identity. The
coordinator injects `x-crabbox-auth`, `-admin`, `-owner`, `-org`, and `-github-login`
headers. The portal converts a `crabbox_session` cookie into a Bearer token.

## Fleet Coordinator And Runtime Adapters

One logical `FleetCoordinator` (`worker/src/fleet.ts`) owns:

- **Lease state** — `lease:*` records (`LeaseRecord` in `worker/src/types.ts`):
  provider, target, class/server type, cloud ID, host, SSH user/port, owner/org,
  sharing, TTL/idle timeout, cost estimates, state
  (`active|released|expired|failed`), telemetry history, cleanup metadata, and
  optional Tailscale/pond/exposed-port fields.
- **Cost and spend caps** (`worker/src/usage.ts`) — `enforceCostLimits` checks
  active-lease counts and monthly reserved-USD budgets (global / per-owner /
  per-org) from `CRABBOX_MAX_*` env. Over-limit requests get HTTP 429
  `cost_limit_exceeded`. Cost = hourly rate × TTL, where the rate comes from a
  `CRABBOX_COST_RATES_JSON` override, then a provider live price, then built-in
  defaults.
- **Usage accounting** — `usageSummary` aggregates leases per
  owner/org/provider/server type for the month; served at `GET /v1/usage`.
- **Cleanup and expiry** — runtime alarms/jobs and reconciliation run maintenance:
  `expireLeases` deletes the cloud server for active leases past `expiresAt`
  (retrying after a 5-minute backoff on failure), then an optional AWS orphan
  sweep, then `scheduleAlarm` arms the next alarm at the soonest pending expiry.
- **Runs, run events, run logs, and telemetry** — see
  [What Flows on a Run](#what-flows-on-a-run).
- **Live bridges** — WebSocket relays for WebVNC (agent ↔ viewer), the
  code-server proxy, and egress (host ↔ client), plus a `/v1/control` socket for
  run-event subscriptions and lease heartbeats. Cloudflare can hibernate
  sockets; Node keeps them in process and clients reconnect after restarts.
- **Provider operations** — per-provider adapters (`aws.ts`, `azure.ts`,
  `gcp.ts`, `hetzner.ts`) handle provision/release/images/identity/capacity. The
  core stays provider-neutral through hooks such as `prepareLeaseCreate`,
  `createServerWithFallback`, `finalizeLeaseCreate`, and `hourlyPriceUSD`.

Runtime-specific persistence and scheduling stay behind `CoordinatorRuntime`:

| Runtime | Durable state | Scheduling | WebSockets |
| --- | --- | --- | --- |
| Cloudflare | Durable Object storage | DO alarms plus scheduled Worker reconciliation | Hibernating WebSockets |
| Node.js | PostgreSQL `crabbox` schema | pg-boss `crabbox_jobs` schema | In-process `ws`; reconnect after restart |

The Node runtime currently requires one service replica because lifecycle
serialization and live bridge ownership are process-local. PostgreSQL and
pg-boss are durable, but horizontal replicas need distributed locking and
bridge routing first.

## Coordinator HTTP API

Lease lifecycle:

```text
GET  /v1/leases
GET  /v1/leases/{id-or-slug}
POST /v1/leases
POST /v1/leases/{id-or-slug}/heartbeat
POST /v1/leases/{id-or-slug}/release
POST /v1/leases/{id-or-slug}/tailscale
GET|PUT|DELETE /v1/leases/{id-or-slug}/share
```

Runs and observability:

```text
GET  /v1/runs
POST /v1/runs
GET  /v1/runs/{run-id}
GET  /v1/runs/{run-id}/logs
POST /v1/runs/{run-id}/events
POST /v1/runs/{run-id}/telemetry
POST /v1/runs/{run-id}/finish
```

Live bridges and tickets:

```text
.../webvnc/ticket | status | reset | agent
.../code/ticket | agent
.../egress/ticket | host | client | status
```

Service and admin:

```text
GET  /v1/health
GET  /v1/whoami
GET  /v1/usage
GET  /v1/pool
GET  /v1/providers/{provider}/readiness
GET  /v1/runners
POST /v1/runners/sync
POST /v1/images
POST /v1/images/{id}/promote | fast-snapshot-restore
POST /v1/artifacts/uploads
GET  /v1/admin/leases
POST /v1/admin/lease-audit
POST /v1/admin/leases/{id-or-slug}/release | delete
GET  /v1/admin/hosts
POST /v1/admin/aws-orphan-sweep
```

`GET /v1/pool` and `/v1/admin/*` require the admin token. User tokens scope
list, lookup, heartbeat, release, run history, logs, and usage to the token's
owner/org. The CLI client wraps these in `internal/cli/coordinator.go`; when a
user request 404s or 401s, an admin-token fallback re-resolves and retries as
admin.

## What Flows on a Run

`crabbox run` (`internal/cli/run.go`). In brokered mode a run recorder mirrors
progress to the broker so the portal and `history`/`logs`/`events`/`results`
commands can read it back:

- `POST /v1/runs` creates a `RunRecord` in state `running`.
- `POST /v1/runs/{id}/events` streams phase-tagged events: `run.started`,
  `leasing.started`, `bootstrap.waiting`, `sync.started`/`finished`,
  `actions.hydrate.*`, `command.started`, stdout/stderr chunks,
  `command.finished`, `lease.released`.
- `POST /v1/runs/{id}/telemetry` posts periodic host samples.
- `POST /v1/runs/{id}/finish` reports exit code, sync/command durations, the log
  (chunked at 64 KiB, capped at 8 MiB), and parsed [results](features/test-results.md).
  The coordinator computes `durationMs`, sets state `succeeded`/`failed`, and records
  classification (`blockedStage`, `retryLikely`).

The command itself, file sync, and I/O streaming all happen **directly
CLI → runner over SSH** and never traverse the broker.

## Sync and Hydration

Sync runs only for SSH backends; delegated providers reject local-sync flags.
The high-level flow in `run.go`:

1. **Manifest** — `syncManifest` builds a NUL-delimited list of changed and
   deleted files from the local Git repo, size-checked by `checkSyncPreflight`.
   `crabbox sync-plan` previews this manifest without touching a box.
2. **Fingerprint short-circuit** — when enabled, a local fingerprint is compared
   to the remote one; identical fingerprints skip the sync entirely.
3. **Optional reset** — `--full-resync` / `--fresh-sync` resets the remote
   workdir first.
4. **Git seed** — the remote clones/fetches the base tree so rsync only ships
   the diff.
5. **rsync** — files transfer with `--files-from` against the manifest (Windows
   uses a native path); deleted paths are pruned.
6. **Finalize** — the remote Git-hydrates the worktree against the base
   ref/SHA, applies a mass-deletion guard, and records the new fingerprint.

Alternative seeding paths: `--fresh-pr` does a remote fresh checkout of a GitHub
PR (optionally applying the local patch), and
[Actions hydration](features/actions-hydration.md) reconstructs a workspace from
a GitHub Actions run.

## Machine Bootstrap

Bootstrap produces a minimal, neutral box: a `crabbox` user, SSH key-only auth,
Git, rsync, curl, jq, and a writable work root (default `/work/crabbox` on
Linux, `C:\crabbox` on Windows, `/Users/<user>/crabbox` on macOS). Readiness is
signaled by the `crabbox-ready` marker.

Language runtimes, Docker, services, dependencies, and secrets are *project*
setup, not base bootstrap. Use [Actions hydration](features/actions-hydration.md),
devcontainers, Nix, mise/asdf, or repository scripts for that layer. Prefer
provider snapshots/images once bootstrap is proven; cloud-init is fine for a
first pass.

## Config Sources

Precedence, highest first:

```text
flags > env > repo-local crabbox.yaml/.crabbox.yaml > user config > defaults
```

User config (YAML) can define the broker URL and token, profiles, machine
classes, provider defaults, sync excludes and behavior (checksum mode, Git
seeding, fingerprint skipping), env allowlists, capacity market/region strategy,
Actions hints, and trusted projects. See the [configuration reference](features/configuration.md).

Config must **not** store live leases, SSH private keys, or provider secrets.
Per-lease SSH private keys live under the user-config directory, outside repo
config. Provider secrets live in the coordinator runtime's secret environment
for brokered providers; for direct providers they come from the local SDK
credential chain.

## Defaults

| Setting | Default |
| --- | --- |
| Lease ID format | `cbx_<12 hex>` |
| User token prefix | `cbxu_` |
| TTL | 5400 s (capped at 86400 s) |
| Idle timeout | 1800 s |
| SSH port | 2222, fallback 22 |
| Machine class | `beast` |
| Work root | `/work/crabbox` (Linux) |
| Run log | 64 KiB chunks, 8 MiB stored cap |
| Cleanup retry | 5 min |
| Bridge ticket TTL | 120 s |

## Failure Model

Assume the CLI can crash, SSH can disconnect, machines can fail to boot,
provider API calls can race or partially complete, and coordinator requests can
retry. Therefore:

- Lease creation is idempotent where practical.
- TTL/idle cleanup in coordinator state is authoritative.
- Provider resources carry labels so orphan sweeps can find them.
- Release is safe to call repeatedly.
- Machine delete tolerates already-deleted resources.

## Source of Truth

| Concern | Files |
| --- | --- |
| CLI command tree and flags | `internal/cli/cli_kong.go`, `internal/cli/app.go` |
| Backend selection / modes | `internal/cli/provider_backend.go` |
| Broker client | `internal/cli/coordinator.go`, `provider_coordinator.go` |
| Run / sync / lease | `internal/cli/run.go`, `lease.go` |
| Coordinator entry / auth | `worker/src/coordinator-entry.ts`, `worker/src/index.ts`, `worker/node/server.ts`, `worker/src/auth.ts` |
| Fleet state / endpoints | `worker/src/fleet.ts`, `types.ts`, `config.ts`, `usage.ts` |
| Runtime adapters | `worker/src/coordinator-runtime.ts`, `worker/node/node-runtime.ts`, `worker/node/postgres-storage.ts` |
