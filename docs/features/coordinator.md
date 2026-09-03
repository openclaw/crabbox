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
`daytona`, `gcp`, and `hetzner`) transfer lifecycle operations to the coordinator.
Every other adapter runs direct from the CLI. A brokerable provider also runs
direct unless a broker URL is configured (`CRABBOX_COORDINATOR`, or
`config set-broker --url`).

Coordinator-backed adapters bind every exact lease response to the provider
selected by the CLI. Legacy responses may omit provider metadata and inherit
that selection, but a response naming an unknown or different provider is
rejected before the CLI converts or acts on the lease.

Mutating CLI requests for release, heartbeat (including idle-timeout and
telemetry updates), and Tailscale metadata carry the canonical selected
provider as `expectedProvider`. The coordinator compares it with the stored
record inside the same state transaction and returns a stable `409` identity
error before any mutation when they differ. Omitting `expectedProvider` remains
supported for older clients and provider-neutral portal flows; when supplied,
it must be a nonempty normalized provider name.

`broker.mode: registered` is provider-neutral. Provisioning, SSH, touch, and
cleanup remain in the direct adapter, while the CLI idempotently registers an
owner-scoped lease record with the coordinator. This enables portal inventory,
sharing, and outbound WebVNC for external, KubeVirt, static SSH, local, and other
direct SSH providers without giving the coordinator provider credentials.
Registered inventory metadata never grants authority to select or invoke a
different provider adapter.
By default coordinator release and expiry remove only the registration. A
registered lease can instead bind an outbound runtime adapter and workspace ID;
the portal then confirms provider deletion through that adapter before removing
the registration. Registered records remain excluded from provider access
reconciliation, ready pools, image operations, orphan sweeps, and cost totals.

For normal CLI leases, the coordinator brokers the **control plane**, not the
data plane. Lease lifecycle, run recording, usage, and bridges flow through the
coordinator over HTTP. SSH readiness, rsync, command execution, and output
streaming happen directly from the CLI to the runner host and never traverse
the coordinator. The dedicated
[private AWS workspace service](aws-private-workspaces.md) is a separate
SSM-only workspace API path. See [Architecture](../architecture.md) for the full
topology.

## Durable Azure provisioning

`CRABBOX_DURABLE_PROVISIONING_ADMISSION=true` opts new eligible creates into the
versioned durable provisioning controller. It defaults to off. Eligibility is
evaluated after provider defaults and promoted images resolve: initially only
ordinary and fixed-ID Azure native Windows `normal`, amd64, managed OS disks
created from VM images qualify. Snapshot/copy-disk, ephemeral, WSL2, workspace,
registered and other-provider requests keep their existing provisioning paths.
An unsupported effective default is not advertised as resumable by readiness.

The existing server-only `CRABBOX_SESSION_SECRET` must be stable, distinct from
the shared token, and at least 32 characters. Missing or unsuitable material
configuration returns `424 continuation_unavailable` before durable admission;
the coordinator does not create a key or fall back to a provider/shared token.
Provider readiness includes `resumableProvisioning` with scope support,
admission availability, and missing material configuration.

Clients opt into an asynchronous create response with `Prefer: respond-async`.
An admitted supported job returns `202 {lease}` with `lease.state=provisioning`,
`Location`, `Retry-After`, and `Preference-Applied: respond-async`. The envelope
does not change. A create-attempt token alone is not async negotiation. Initial
requests without the preference retain a synchronous facade; ordinary token
replay and fixed PUT keep their existing status/intent contracts. Older brokers
may ignore the preference. Clients must confirm the canonical create intent,
then observe readiness within their original acquisition budget.
Durable fixed-ID admission also retains a private request fingerprint, allowing
the same request to rebind after deployment defaults change without serializing
the lease configuration or accepting a changed caller intent.

Admission commits the canonical attempt, lease, private operation, sealed
material and due entry atomically. Restarted controllers use the frozen
provider plan and original deadline. Disabling new admissions does not disable
existing journals. Publication is a separate durable phase; cancellation and
expiry remain authoritative over late provider results. Fixed-ID caller
disconnection does not cancel the retained operation; explicit release does.

Operation plans, attempt journals, claims and sealed bootstrap/admin-password
material live outside lease records and are never returned by lease GET/list or
portal serialization. Material is purged at terminal publication or when durable
cancellation, retention or expiry irrevocably disables forward work. Settlement
and exact owned cleanup use the nonsecret journals and provider authentication,
so retiring the VM password/bootstrap or losing the session key does not prevent
cleanup. Key loss or tampering still blocks forward replay. Unsupported or
quarantined records retain their material until their authority is understood;
resource and deletion evidence are preserved independently.

## CLI request budgets

The CLI bounds individual lease reads (including authoritative provider
metadata), health, identity, provider readiness, and HTTP heartbeat requests to
30 seconds. The same deadline covers authentication, response-body reads, and
any eligible read-only curl fallback; an earlier caller deadline still wins.
Best-effort foreground lease touches retain their shorter 20-second budget.
Provisioning and image operations retain the 30-minute HTTP budget.

Before releasing a lease, `stop` allows ten seconds for its preliminary lookup.
If that lookup fails, ordinary stop can use the existing provider-scoped release
request; provider identity errors still fail closed, and `stop --force` still
requires successful inspection. Caller cancellation stops the command rather
than starting this fallback. Release attempts and cleanup observation retain
their existing separate budgets. An uncertain cleanup result preserves the
local claim and SSH artifacts; a request timeout is not proof of remote failure
or successful deletion and does not add mutation retries.

## Responsibilities

The fleet coordinator owns:

- authentication of every broker request;
- lease lifecycle: create, look up, heartbeat, release, expire, share;
- provider credentials and provider operations (provision, release, images,
  identity, Mac hosts, capacity fallback, orphan sweep);
- owner/org-scoped brokered native checkpoint records, opt-in unused expiry,
  bounded checkpoint/fork-claim admission, generation-fenced fork claims,
  recent checkpoint audit events, promotion pins, and exact provider cleanup;
- cost and active-lease guardrails enforced at create time;
- usage aggregation by owner, org, provider, and instance type;
- run records, run events, run logs, and per-run telemetry;
- live bridges (WebVNC, code-server, mediated egress) and the run-event control
  socket;
- artifact-upload credentials and scoped upload URLs;
- expiry and cleanup, driven by Durable Object alarms or durable pg-boss jobs
  plus periodic reconciliation.

The PostgreSQL runtime retries serialization/deadlock contention with bounded
jittered backoff so parallel checkpoint shard claims do not lose authoritative
use counts or replay provider mutations inside retried storage transactions.

## Authentication

Every request below the public health route requires an authenticated identity,
resolved in this order (`worker/src/auth.ts`):

1. **Admin token** — matches `CRABBOX_ADMIN_TOKEN`. Grants admin scope.
2. **Shared operator token** — matches `CRABBOX_SHARED_TOKEN`. Non-admin scope,
   owner from `CRABBOX_SHARED_OWNER`.
3. **Signed user token** — prefix `cbxu_`, an HMAC-SHA256 signature over a
   base64url payload, keyed only by `CRABBOX_SESSION_SECRET`. The session secret
   must be configured and distinct from `CRABBOX_SHARED_TOKEN`. Issued by GitHub
   OAuth login; default 180-day expiry. Carries `owner`, `org`, GitHub `login`,
   and an encrypted OAuth credential used to revalidate allowed org/team
   membership on requests.
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
stores a distinct `cbwp_` browser-session credential in the unique
`__Host-crabbox_session` host-only cookie. The coordinator accepts that audience
only from the cookie-authenticated portal path and marks it as a trusted portal
session; normal API Bearer authentication rejects it. Legacy `cbxu_` portal
cookies remain usable for ordinary portal pages but cannot initiate pairing.
Duplicate session cookies fail closed.

GitHub user tokens are scoped to their owner/org for lease, run, log, and usage
routes. Admin scope (admin token, or shared token where allowed) is required for
`GET /v1/pool`, all `/v1/admin/*` routes, `POST /v1/images`, and the image
sub-routes.

The [read-only device pairing](device-pairing.md) surface uses a separate
`crabbox-device` principal. It can only list and inspect credential-free lease
status, never inherits owner or admin authority, and revalidates the paired
GitHub owner on every request.

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
POST   /v1/leases/from-checkpoint
PUT    /v1/leases/{canonical-id}       (fixed-ID idempotent create)
PUT    /v1/leases/{id}/registration
GET    /v1/leases
GET    /v1/leases/{id-or-slug}
POST   /v1/leases/{id-or-slug}/heartbeat
POST   /v1/leases/{id-or-slug}/release
POST   /v1/leases/{requested-id}/cancel-create
POST   /v1/leases/{id-or-slug}/tailscale
GET    /v1/leases/{id-or-slug}/share
PUT    /v1/leases/{id-or-slug}/share
DELETE /v1/leases/{id-or-slug}/share
POST   /v1/checkpoints
GET    /v1/checkpoints
GET    /v1/checkpoints/{id}
GET    /v1/checkpoints/{id}/events
PATCH  /v1/checkpoints/{id}/retention
POST   /v1/checkpoints/{id}/use
DELETE /v1/checkpoints/{id}
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

Current CLIs bind each ordinary `POST` create to a fresh opaque attempt token
and repeat only that same token after an ambiguous response. `cancel-create`
persists a permanent token-bound cancellation tombstone, including when it
arrives before the create request, and releases only the matching canonical
generation if one was already accepted. Concurrent same-token POSTs replay that
canonical provisioning or active lease instead of competing for its ID.
An unbound canceled tombstone rejects only that exact owner/org/token operation;
it does not reserve the provisional ID against a fresh token or a fixed,
registered, or workspace lifecycle. Pending and canonical-bound attempts remain
global ID reservations.
Tokenless POSTs from older CLIs remain supported with their previous behavior,
but they do not gain this cancellation guarantee. Roll out the coordinator
before distributing a CLI that sends create attempts; once token-bound creates
begin, do not roll the coordinator back to a version that ignores their
tombstones. A newer CLI against an older coordinator fails cancellation closed
rather than falling back to an unsafe ID-only release.

Checkpoint creation derives owner, canonical organization, provider, and exact
provider scope from the authoritative source lease. Global/owner/org admission
and the durable `creating` reservation share one serializable transaction
before provider mutation; only an exactly owned AWS, Azure, or GCP resource
can publish the checkpoint. Forks use transactionally bounded, renewable,
generation-bound claims and the narrowly validated
`/v1/leases/from-checkpoint` route instead of relaxing ordinary lease image
overrides. Manual retention is the default; explicit unused expiry, use claims,
deletion retries, and audit tombstones share the coordinator's sorted
checkpoint due index and scheduler. `/v1/checkpoints/{id}/events` exposes only
the retained recent audit suffix: at most 256 ordered events per checkpoint,
not complete lifetime history.
Receipt-bearing run finishes use the same fail-closed rollout rule. After every
successful finish response, the CLI retrieves the stored receipt and requires
an exact signed match before marking the run recorded. A coordinator that
predates receipt persistence may accept the unknown finish field, but its
missing receipt endpoint makes the new CLI fail visibly. Legacy CLIs may still
finish without receipts. Roll out the coordinator before distributing a
receipt-bearing CLI.

The fixed-ID `PUT` route is fail-closed and does not replace legacy `POST`.
It atomically reserves a versioned normalized immutable request hash before
provider work. An identical owner-scoped replay returns an active lease or the
same provisioning record; request drift and terminal-ID reuse return
`lease_id_conflict` without invoking the provider. CLIs using `--lease-id`
poll a provisioning replay until it becomes active or terminal. Coordinators
that predate this route return not found before any create side effect.
If the PUT response is ambiguous, the CLI repeats the full identical PUT until
the coordinator atomically confirms the same stored intent or returns a
conflict/definite error. Public GET is used only after that PUT confirmation,
never to adopt an unverified fixed-ID record.

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
GET    /v1/admin/azure-orphan-sweep
POST   /v1/admin/azure-orphan-sweep
POST   /v1/admin/leases/{id-or-slug}/release
POST   /v1/admin/leases/{id-or-slug}/delete
POST   /v1/images
POST   /v1/images/{id}/promote
GET    /v1/images/{id}/fast-snapshot-restore
```

## Browser portal surface

The portal is the authenticated browser UI served by the same coordinator
(`worker/src/portal.ts`). Login is unauthenticated; everything else uses the
host-only `__Host-crabbox_session` cookie. Logout confirmation is read-only,
while logout itself requires a same-origin `POST` and closes WebVNC, Code, and
mediated-egress bridges bound to that portal session.

```text
GET    /portal
GET    /portal/login
GET    /portal/logout
POST   /portal/logout
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

The CLI's best-effort external-runner sync has a single five-second budget
covering inventory, optional Actions enrichment, credential resolution, and the
HTTP request/response. Earlier caller deadlines still apply. Once canceled, the
CLI does not publish partial inventory or start further lookups. A warning does
not change a successful allocation or retained lease; Blacksmith warmup emits
final completion/timing after the sync attempt. An upload whose response is lost
may already have been accepted, so it is not retried.

`/portal/leases/{id-or-slug}` shows lease state, bridge status, the latest Linux
telemetry, copy-ready `ssh`/`run`/WebVNC/code commands, a recent-runs grid, and a
stop action. `/portal/runs/{run-id}` shows the command, owner, lease, exit
status, JUnit summary, an event table, and a log tail. The portal run pages mirror
the `/v1/runs/...` resources but authenticate via the session cookie, so logs and
events are inspectable without pasting a bearer token into the browser.

## Lease lifecycle through the broker

**Create.** The CLI generates the lease ID (`cbx_<12 hex>`), a fresh opaque
create-attempt token, a slug, and a per-lease SSH key, then `POST /v1/leases`
with the full request.
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

**Cancel create.** If the caller cancels an ordinary create, the CLI sends
`POST /v1/leases/{requested-id}/cancel-create` with the exact create-attempt
token on a cancellation-independent bounded context. The coordinator persists
the canceled tombstone even when cancellation arrives first. When a matching
canonical lease exists, its released state and durable cleanup claim are written
under the same state lock before provider deletion, so ordinary maintenance can
resume cleanup after a restart. It releases a reserved, provisioning, or
retained canonical lease only when its private token, owner/org, and retained
generation match; otherwise it fails closed.

Fixed-ID `PUT` creates record an owner/org/provider-bound intent before
asynchronous preparation or capacity checks. They remain replay-owned, without
a caller-supplied cancellation token. `POST /v1/leases/{id}/release` also
cancels an admitted fixed intent that has not allocated a machine, including
one rejected by a quota check. The cancellation survives coordinator restart
and prevents a delayed or repeated create from allocating. Admission alone
does not create a lease row or reserve usage. Unknown IDs still return 404;
missing records from older coordinators do not prove cancellation or cleanup.
Fixed admissions use version-2 attempt records: an older coordinator refuses
to reuse these IDs, but only the upgraded coordinator can confirm cancellation
before a lease row exists.

**Release.** `POST /v1/leases/{id}/release` (body `{delete?}`, defaulting to
`!keep`) deletes the cloud server when the lease is still active and sets state
`released`. For a registered lease bound to a runtime-adapter workspace,
explicit `{"delete":true}` instead initiates the same owner/org-scoped,
immutable-registration-generation-fenced delete used by the portal and returns
`202` while confirmed-absence cleanup is pending. Omitting `delete` preserves
metadata-only registered release. The CLI client retries as admin when a user
request 404s or 401s. A successful managed release normally returns queued
cleanup state; the CLI does not repeat the mutation. Explicit stop polls the
lease read-only with the same authenticated client until provider deletion is
final or a bounded wait, cleanup failure, or cancellation preserves the local
claim and credentials for retry. Acquisition rollback queues release without
waiting, as does automatic post-run release, so asynchronous provider cleanup
does not delay an otherwise completed run. Isolated local lease credentials are
removed only after final deletion is observed; retained releases preserve them.

**Expiry and cleanup.** A DO alarm and the cron both run maintenance:
`expireLeases` deletes cloud servers for active leases past `expiresAt`
(state `expired`), retrying after ~5 minutes on failure, and the AWS and Azure
orphan sweeps report untracked provider resources and delete or release only
resources with exact retained coordinator bindings. Each sweep is gated by its
provider-specific settings. See [Lifecycle and cleanup](lifecycle-cleanup.md)
for the detailed cleanup rules. The next alarm is scheduled for the soonest
upcoming expiry or sweep time.

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
