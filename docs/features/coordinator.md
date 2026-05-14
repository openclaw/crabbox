# Coordinator

Read when:

- changing brokered lease behavior;
- debugging coordinator auth, health, pool, status, or usage;
- deciding whether behavior belongs in the CLI or Worker.

The coordinator is the Cloudflare Worker plus Fleet Durable Object. Normal Crabbox operation goes through this broker; direct provider mode is for debugging and escape hatches.

Responsibilities:

- authenticate broker requests with signed GitHub user tokens, the shared operator token, or the separate admin token, with optional verified Cloudflare Access context on protected fallback routes;
- serialize fleet state in one Durable Object;
- create, heartbeat, release, expire, and look up leases;
- own provider credentials;
- own artifact storage credentials and mint scoped artifact upload URLs;
- create and delete provider resources;
- list the pool;
- enforce cost and active-lease guardrails;
- expose usage statistics.

API surface:

```text
GET  /v1/health
GET  /v1/pool
GET  /v1/whoami
POST /v1/leases
GET  /v1/leases
GET  /v1/leases/{id-or-slug}
POST /v1/leases/{id-or-slug}/heartbeat
POST /v1/leases/{id-or-slug}/release
GET  /v1/runs
POST /v1/runs
GET  /v1/runs/{run-id}
GET  /v1/runs/{run-id}/logs
POST /v1/runs/{run-id}/finish
POST /v1/artifacts/uploads
GET  /v1/runners
POST /v1/runners/sync
GET  /v1/usage
GET  /v1/admin/leases
GET  /v1/admin/lease-audit
POST /v1/admin/leases/{id-or-slug}/release
POST /v1/admin/leases/{id-or-slug}/delete
```

Browser portal surface:

```text
GET  /portal
GET  /portal/leases/{id-or-slug}
POST /portal/leases/{id-or-slug}/release
GET  /portal/leases/{id-or-slug}/vnc
GET  /portal/leases/{id-or-slug}/code/
GET  /portal/runs/{run-id}
GET  /portal/runs/{run-id}/logs
GET  /portal/runs/{run-id}/events
GET  /portal/runners/{provider}/{runner-id}
```

`/portal` renders a searchable/paginated/sortable lease data grid with compact
provider/target badges, icon-only access capabilities, relative time cells,
dense rows, sticky column headers, and active, ended, provider, target, and all
filters. Normal browser sessions are owner/org scoped. Admin/operator sessions
can also see non-owned runner leases, with `mine` and `system` filters so
Blacksmith/Testbox-style coordinator leases are visible without leaking them to
normal users. It defaults to active leases when any are active, and falls back to
all visible leases when the active list is empty. External runner rows, currently
Blacksmith Testboxes synced from the CLI's current all-status list, render in the
same grid as muted, disabled rows with search, pagination, status/provider
filters, inferred GitHub Actions run/workflow links and status badges when
available, `stuck` markers for long-queued or long-running Actions owners, a
copyable local stop command, and stale markers when the next sync no longer sees
a previously visible runner. Clicking an external runner opens
`/portal/runners/{provider}/{runner-id}`, a visibility-only detail page with
owner/org, Actions ownership, lifecycle timestamps, boundary notes, and the local
stop command.

`/portal/leases/{id-or-slug}` is the authenticated lease detail page. It shows
the lease state, bridge status, compact provider/target badges, latest Linux
telemetry, access-panel copy commands for `ssh`, `run`, WebVNC, and code, a
viewport-fitted recent runs grid with state filters, and a stop action for the
visible lease. When multiple telemetry samples are present, the detail page
adds load, memory, and disk sparklines plus stale/high-resource status pills.
Portal run links mirror the `/v1/runs/...` resources but use the browser
session cookie, so users can inspect logs and events without copying a bearer
token into the browser. The run detail page at `/portal/runs/{run-id}` renders
the command, owner, lease, provider metadata, exit status, JUnit summary when
present, a searchable/paginated event table with event-type filters, and a
copyable retained log tail. Longer Linux runs include bounded load, memory, and
disk trend lines collected from run telemetry samples; `/logs` and `/events`
remain raw/plain resources for copying and automation.

GitHub browser-login tokens are owner/org scoped for lease, run, log, and usage routes. Shared-token admin auth is required for `GET /v1/pool`, admin lease routes, and fleet-wide usage/listing.

Lease responses include the canonical `cbx_...` ID, friendly slug when present, provider metadata, owner/org, `createdAt`, `lastTouchedAt`, `idleTimeoutSeconds`, `ttlSeconds`, and computed `expiresAt`. Heartbeat is a touch and can update idle timeout only when the request explicitly sends `idleTimeoutSeconds`.

The CLI owns local config, per-lease SSH keys, SSH readiness, sync, command execution, output streaming, and local fallback handling.

Related docs:

- [Orchestrator](../orchestrator.md)
- [Architecture](../architecture.md)
- [CLI](../cli.md)
- [usage command](../commands/usage.md)
