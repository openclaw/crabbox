# Coordinator

Read this when you are:

- changing brokered lease behavior;
- debugging coordinator auth, health, pool, lease, run, or usage routes;
- deciding whether a behavior belongs in the CLI or in the Worker.

## What the coordinator is

The coordinator is an authenticated control plane that owns provider
credentials, lease state, run records, usage accounting, and live access
bridges. `FleetCoordinator` (`worker/src/fleet.ts`) contains the shared behavior
and runs through either:

- the Cloudflare Worker and one Fleet Durable Object (`worker/src/index.ts`);
- the Node.js service with PostgreSQL and pg-boss (`worker/node`).

The default `broker.mode: managed` lets brokerable providers (`aws`, `azure`,
`gcp`, and `hetzner`) transfer lifecycle operations to the coordinator. Every
other adapter runs direct from the CLI. A brokerable provider also runs direct
unless a broker URL is configured (`CRABBOX_COORDINATOR`, or
`config set-broker --url`).

`broker.mode: registered` is provider-neutral. Provisioning, SSH, touch, and
cleanup remain in the direct adapter, while the CLI idempotently registers an
owner-scoped lease record with the coordinator. This enables portal inventory,
sharing, and outbound WebVNC for external, KubeVirt, static SSH, local, and other
direct SSH providers without giving the coordinator provider credentials.
By default coordinator release and expiry remove only the registration. A
registered lease can instead bind an outbound runtime adapter and workspace ID;
the portal then confirms provider deletion through that adapter before removing
the registration. Registered records remain excluded from provider access
reconciliation, ready pools, image operations, orphan sweeps, and cost totals.

The coordinator brokers the **control plane**, not the data plane. Lease
lifecycle, run recording, usage, and bridges flow through the coordinator over
HTTP. SSH readiness, rsync, command execution, and output streaming always
happen directly from the CLI to the runner host and never traverse the
coordinator. See
[Architecture](../architecture.md) for the full topology.

## Responsibilities

The fleet coordinator owns:

- authentication of every broker request;
- lease lifecycle: create, look up, heartbeat, release, expire, share;
- provider credentials and provider operations (provision, release, images,
  identity, Mac hosts, capacity fallback, orphan sweep);
- cost and active-lease guardrails enforced at create time;
- usage aggregation by owner, org, provider, and instance type;
- run records, run events, run logs, and per-run telemetry;
- live bridges (WebVNC, code-server, mediated egress) and the run-event control
  socket;
- artifact-upload credentials and scoped upload URLs;
- expiry and cleanup, driven by Durable Object alarms or durable pg-boss jobs
  plus periodic reconciliation.

## Authentication

Every request below the public health route requires an authenticated identity,
resolved in this order (`worker/src/auth.ts`):

1. **Admin token** — matches `CRABBOX_ADMIN_TOKEN`. Grants admin scope.
2. **Shared operator token** — matches `CRABBOX_SHARED_TOKEN`. Non-admin scope,
   owner from `CRABBOX_SHARED_OWNER`.
3. **Signed user token** — prefix `cbxu_`, an HMAC-SHA256 signature over a
   base64url payload, keyed only by `CRABBOX_SESSION_SECRET`. The session secret
   must be configured and distinct from `CRABBOX_SHARED_TOKEN`. Issued by GitHub
   OAuth login; default 180-day expiry. Carries `owner`, `org`, and GitHub
   `login`.
4. **Trusted reverse-proxy identity** — opt-in through
   `CRABBOX_TRUSTED_USER_HEADER` on the Node runtime, accepted only from peers in
   `CRABBOX_TRUSTED_PROXY_CIDRS`; the authenticated ingress must also strip
   caller-supplied copies of that header.

An optional Cloudflare Access JWT (`cf-access-jwt-assertion`, verified against
`CRABBOX_ACCESS_TEAM_DOMAIN` and `CRABBOX_ACCESS_AUD`) supplies the verified
owner email for admin and shared-token requests.

After auth, the coordinator strips inbound Access headers and injects a trusted
context for the fleet implementation: `x-crabbox-auth`, `x-crabbox-admin`,
`x-crabbox-owner`, `x-crabbox-org`, and `x-crabbox-github-login`. The portal
converts a `crabbox_session` cookie into a Bearer token so browser sessions reuse
the same auth path.

GitHub user tokens are scoped to their owner/org for lease, run, log, and usage
routes. Admin scope (admin token, or shared token where allowed) is required for
`GET /v1/pool`, all `/v1/admin/*` routes, `POST /v1/images`, and the image
sub-routes.

## API surface

The shared entry router answers `GET /v1/health` and redirects `/` to `/portal`
directly, routes auth, portal-login, and bridge websocket upgrades to the fleet
unauthenticated, rejects `/v1/internal/*` externally, and otherwise
authenticates and forwards to the fleet. Cloudflare cron or pg-boss
reconciliation runs maintenance.

Public and user-scoped routes:

```text
GET    /v1/health
GET    /v1/whoami
GET    /v1/providers/{provider}/readiness
GET    /v1/control                       (websocket: run events + heartbeats)
POST   /v1/leases
PUT    /v1/leases/{id}/registration
GET    /v1/leases
GET    /v1/leases/{id-or-slug}
POST   /v1/leases/{id-or-slug}/heartbeat
POST   /v1/leases/{id-or-slug}/release
POST   /v1/leases/{id-or-slug}/tailscale
GET    /v1/leases/{id-or-slug}/share
PUT    /v1/leases/{id-or-slug}/share
DELETE /v1/leases/{id-or-slug}/share
POST   /v1/runs
GET    /v1/runs
GET    /v1/runs/{run-id}
GET    /v1/runs/{run-id}/logs
GET    /v1/runs/{run-id}/events
POST   /v1/runs/{run-id}/events
POST   /v1/runs/{run-id}/telemetry
POST   /v1/runs/{run-id}/finish
POST   /v1/artifacts/uploads
GET    /v1/runners
POST   /v1/runners/sync
GET    /v1/usage
POST   /v1/adapters/{adapter-id}/ticket
GET    /v1/adapters/{adapter-id}
GET    /v1/adapters/{adapter-id}/agent    (websocket; one-time ticket auth)
*      /v1/adapters/{adapter-id}/proxy/v1/workspaces/...
```

Registration accepts generic provider and SSH metadata. Repeating the same
owner/org/id/provider tuple refreshes it and reactivates an expired record.
Changing provider, claiming another owner's ID, or replacing a managed record
returns `409`. The CLI treats registration as best effort, but an authenticated
WebVNC/share operation still requires the record to exist.

Runtime adapters connect outbound to `/agent`, so their local lifecycle service
does not need an inbound public route. The coordinator issues a short-lived,
single-use ticket after normal owner authentication. The first ticket for a new
adapter ID creates a ten-minute provisional owner/org claim. Agent connection or
successful registered lease binding confirms it as durable. An expired
provisional claim can be recovered by another normally authenticated owner only
when no connected agent, pending relay request, unexpired ticket, live registered
lease, or pending workspace deletion still uses it. Existing and confirmed
claims remain durable. Lease registration rejects unclaimed IDs and claims owned
by another identity. The relay permits
only the versioned workspace create, inspect, delete, and desktop-connection
methods. Public proxy `DELETE` requests are rejected; destructive dispatch must
go through a registered lease release so the coordinator can fence its immutable
registration generation. Relay bodies are valid UTF-8 and bounded to 64 KiB. Lifecycle responses
have a nine-second local deadline plus five seconds for relay delivery; desktop
setup has a 150-second window. The absolute deadline travels with every frame,
so work delayed in WebSocket buffers is rejected before local dispatch. Only
workspace creation accepts a non-empty request body. Per-adapter, per-owner,
and global in-flight limits isolate relay capacity. A lease reaching its TTL
loses coordinator-mediated access immediately, including closure of existing
WebVNC, code, and egress bridge sockets, while any pending external delete
continues retrying as cleanup.
The adapter/workspace binding remains reserved until that cleanup finishes.
Generic and administrative lease release cannot clear a pending adapter delete;
only an owner-scoped confirmed-absence completion for the exact adapter,
workspace, and immutable registration generation can finalize it. The generation
is refreshed whenever a terminal registration is reactivated, so a delayed
completion from the previous workspace lifecycle cannot release the new one.
Client claims retain an acknowledged generation and any pending replacement
separately until registration succeeds, making both registration and later
confirmed-absence cleanup recoverable across request loss or process failure.
If that local generation state is missing, the CLI permits legacy release only
through an atomic owner-scoped completion that requires the exact adapter and
workspace binding to have no registration generation.
Once a delete reaches the adapter, authentication failures and other generic
upstream rejections remain retryable; only an explicit confirmed-absence result
for the matching generation ends cleanup. Adapter `404` and terminal-looking
relay responses alone never release the coordinator binding.
Already-dispatched work remains charged to relay quotas if its caller
disconnects. A durable dispatch fence also keeps that generation reserved until
each selected delete succeeds or its absolute relay deadline expires; connector
timeouts and transport failures cannot clear it early. Only the
idempotency key and response content type cross the relay. Provider credentials
and arbitrary callback URLs are never stored in lease records.

The agent upgrade carries its single-use ticket only as an
`Authorization: Bearer ...` header. Adapter-agent routes bypass end-user
authentication at the coordinator, and tickets are not accepted in URLs or
proxy-specific headers.

Bridge ticket and websocket routes (WebVNC, code-server, egress) live under
`/v1/leases/{id-or-slug}/{webvnc|code|egress}/...`; see
[Mediated egress](egress.md) and [Browser portal](portal.md).

Admin-scoped routes:

```text
GET    /v1/pool
GET    /v1/admin/leases
GET    /v1/admin/lease-audit
GET    /v1/admin/aws-identity
GET    /v1/admin/providers/identity
GET    /v1/admin/hosts/...
GET    /v1/admin/mac-hosts/...
GET    /v1/admin/aws-orphan-sweep
POST   /v1/admin/aws-orphan-sweep
POST   /v1/admin/leases/{id-or-slug}/release
POST   /v1/admin/leases/{id-or-slug}/delete
POST   /v1/images
POST   /v1/images/{id}/promote
GET    /v1/images/{id}/fast-snapshot-restore
```

## Browser portal surface

The portal is the authenticated browser UI served by the same coordinator
(`worker/src/portal.ts`). Login and logout are unauthenticated; everything else
uses the `crabbox_session` cookie.

```text
GET    /portal
GET    /portal/login
GET    /portal/logout
GET    /portal/leases/{id-or-slug}
GET    /portal/leases/{id-or-slug}/share
POST   /portal/leases/{id-or-slug}/share
POST   /portal/leases/{id-or-slug}/release
GET    /portal/leases/{id-or-slug}/vnc
GET    /portal/runs/{run-id}
GET    /portal/runners/{provider}/{runner-id}
```

`/portal` renders a searchable, sortable, paginated lease grid with
provider/target badges, access-capability icons, and active/ended/provider/
target filters. Owner/org sessions see their own leases; admin sessions also see
non-owned runner leases, with `mine` and `system` filters so coordinator-managed
runner leases stay visible without leaking to normal users. External runner rows
(synced via `POST /v1/runners/sync`) render as muted rows with inferred GitHub
Actions links and stale markers; clicking one opens its visibility-only detail
page at `/portal/runners/{provider}/{runner-id}`.

`/portal/leases/{id-or-slug}` shows lease state, bridge status, the latest Linux
telemetry, copy-ready `ssh`/`run`/WebVNC/code commands, a recent-runs grid, and a
stop action. `/portal/runs/{run-id}` shows the command, owner, lease, exit
status, JUnit summary, an event table, and a log tail. The portal run pages mirror
the `/v1/runs/...` resources but authenticate via the session cookie, so logs and
events are inspectable without pasting a bearer token into the browser.

## Lease lifecycle through the broker

**Create.** The CLI generates the lease ID (`cbx_<12 hex>`), allocates a slug,
mints a per-lease SSH key, then `POST /v1/leases` with the full request.
`createLease` (`worker/src/fleet.ts`) coerces the request into a `LeaseConfig`
(`worker/src/config.ts`) with defaults: provider `hetzner`, TTL `5400`s (capped
at `86400`), idle timeout `1800`s, SSH port `2222` (fallback `22`), class
`beast`. It checks provider readiness (HTTP 424 if the provider is not
configured), admin-gates native snapshot/image sources, enforces cost limits
(HTTP 429 `cost_limit_exceeded` when over an active-lease count or monthly
reserved-USD budget), provisions through the provider adapter with region/market
fallback, persists the record, and returns 201 `{lease}`. The CLI then starts a
heartbeat goroutine and a lease watch.

**Heartbeat.** `POST /v1/leases/{id}/heartbeat` bumps `lastTouchedAt`,
recomputes `expiresAt`, clears cleanup metadata, and reschedules the alarm. It
updates the idle timeout **only** when the request explicitly sends a positive
`idleTimeoutSeconds` (clamped to `86400`); telemetry samples may ride along in
the same body.

**Release.** `POST /v1/leases/{id}/release` (body `{delete?}`, defaulting to
`!keep`) deletes the cloud server when the lease is still active and sets state
`released`. For a registered lease bound to a runtime-adapter workspace,
explicit `{"delete":true}` instead initiates the same owner/org-scoped,
immutable-registration-generation-fenced delete used by the portal and returns
`202` while confirmed-absence cleanup is pending. Omitting `delete` preserves
metadata-only registered release. The CLI client retries as admin when a user
request 404s or 401s.

**Expiry and cleanup.** A DO alarm and the cron both run maintenance:
`expireLeases` deletes cloud servers for active leases past `expiresAt`
(state `expired`), retrying after ~5 minutes on failure, and an AWS orphan sweep
(report or delete, gated by `CRABBOX_AWS_ORPHAN_SWEEP_*`) reports untracked
instances and deletes or releases only resources with exact retained coordinator
bindings. The next alarm is scheduled for
the soonest upcoming expiry or sweep time.

Lease responses carry the canonical `cbx_...` ID, the friendly slug when present,
provider metadata, owner/org, `createdAt`, `lastTouchedAt`, `idleTimeoutSeconds`,
`ttlSeconds`, and the computed `expiresAt`.

## What flows on a run

In brokered mode, `crabbox run` mirrors progress to the coordinator while executing
directly against the runner over SSH:

- `POST /v1/runs` creates a `RunRecord` (state `running`).
- `POST /v1/runs/{id}/events` streams phase-tagged events (leasing, bootstrap,
  sync, command start/finish, stdout/stderr chunks, lease release).
- `POST /v1/runs/{id}/telemetry` posts periodic host samples.
- `POST /v1/runs/{id}/finish` posts the exit code, sync/command timings, chunked
  log (64 KiB chunks, 8 MiB stored cap), JUnit summary, and telemetry; the
  coordinator computes `durationMs` and sets state `succeeded` or `failed`.

Read back with `GET /v1/runs`, `/v1/runs/{id}`, `/logs`, and `/events`. The
`/v1/control` websocket lets clients subscribe to live run events and send lease
heartbeats. A run keeps its initiating actor in `owner`/`org` plus every backing
lease identity used by replacement flows. Each backing lease owner can read and
subscribe for audit purposes, while only the actor or an admin can append
events or telemetry and finish the run.

## CLI responsibilities

The CLI owns everything the broker does not: local config, per-lease SSH keys,
SSH readiness, the git-manifest sync, command execution, output streaming, and
local fallback handling. Provider operations that are coordinator-only (image
bake/promote, Mac-host management, identity) are invoked through admin routes.

## Related docs

- [Architecture](../architecture.md)
- [Orchestrator](../orchestrator.md)
- [Portable coordinator deployment](portable-coordinator.md)
- [Bring your own infrastructure](bring-your-own-infrastructure.md)
- [Portable coordinator design history](../plan/portable-coordinator.md)
- [CLI](../cli.md)
- [Browser portal](portal.md)
- [usage command](../commands/usage.md)
