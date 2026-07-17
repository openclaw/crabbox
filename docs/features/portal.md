# Browser Portal

Read this when:

- using the web UI to inspect leases, runs, or external runners;
- changing portal pages, page-level routes, or bridge proxies;
- deciding whether a feature belongs in the CLI, the `/v1` API, or the portal.

The browser portal is a server-rendered web UI hosted by the same coordinator
that backs the Crabbox API. It is not a separate frontend or single-page app:
every page is HTML rendered by the coordinator, with light
client-side JavaScript only for filtering, sorting, clipboard copy, theme
switching, and the live VNC viewer. Because the portal and the API use the same
`FleetCoordinator` state, the two surfaces cannot drift apart.

## URL map

The portal lives under `/portal`. Authenticated pages return HTML; a few
endpoints are bridges or raw data feeds rather than pages.

```text
GET  /portal                                    lease / runner / host index
GET  /portal/leases/{id-or-slug}                lease detail
GET  /portal/leases/{id-or-slug}/share          share page
POST /portal/leases/{id-or-slug}/share          add/remove user, set org, clear
POST /portal/leases/{id-or-slug}/release        stop, delete via adapter, or remove registration
POST /portal/leases/{id-or-slug}/vnc/bootstrap  consume an Agent viewer ticket
GET  /portal/leases/{id-or-slug}/vnc            WebVNC viewer page
GET  /portal/leases/{id-or-slug}/code/...       code-server bridge (HTTP/WS proxy)
GET  /portal/runs/{run-id}                       run detail
GET  /portal/runs/{run-id}/logs                  retained log (text/plain)
GET  /portal/runs/{run-id}/events                events (JSON)
GET  /portal/runners/{provider}/{runner-id}      external runner detail
GET  /portal/hosts/{provider}/{host-id}          dedicated host detail
POST /portal/hosts/{provider}/{host-id}/vnc      enable VNC on the host's lease
GET  /portal/login                               GitHub OAuth login redirect
GET  /portal/logout                              confirm logout without changing session state
POST /portal/logout                              end the portal and its live bridge sessions
```

The WebVNC viewer page also drives a small set of bridge sub-routes that the
browser calls directly: `/vnc/viewer` (the noVNC WebSocket), `/vnc/status`,
`/vnc/control` (take control), `/vnc/theme` (sync the desktop theme), and
`/vnc/handoff` (consume a server-bound credential handoff).
Static assets, including the noVNC client at `/portal/assets/novnc/rfb.js`,
are served from the coordinator's bundled assets.

The portal defaults to the browser's system color scheme and tracks later
system appearance changes. Explicit light, dark, or system choices use the
`crabbox-theme-source` browser storage key; the distinct key intentionally
resets preferences written by the older two-state switch. WebVNC sends the
resolved light or dark appearance to connected desktops, including registered
direct Linux leases.

A `GET /` redirects to `/portal` (while `GET /v1/health` returns a JSON
health payload). When
`CRABBOX_PUBLIC_URL` is set and a portal request arrives on a non-canonical
origin (for example a `*.workers.dev` preview URL), the Worker redirects to
the canonical host first.

Deployments that expose browser Code must isolate lease-controlled code-server
content from the coordinator and from other leases by setting
`CRABBOX_CODE_ORIGIN_TEMPLATE=https://{lease}.code.example.com` and routing
`*.code.example.com` to the same coordinator. The existing
`/portal/leases/{id-or-slug}/code/` URL remains the entrypoint: after normal
portal authorization it redirects through a one-use, short-lived viewer ticket
to an opaque per-lease hostname. That hostname receives only an HttpOnly,
browser-enforced host-only `__Host-crabbox_code_session`; it never receives
`__Host-crabbox_session` and cannot use the Code session on other portal routes
or lease origins. The ingress must provide wildcard TLS and WebSocket routing
for the template.
Proxied responses may set only code-server's `vscode-tkn` cookie; Crabbox removes
domain scope and confines it to that lease's Code path.
Crabbox also replaces upstream Content Security Policy headers and limits
framing to the same isolated Code origin, so a sibling same-site origin cannot
embed an authenticated Code session while code-server's own webviews still work.
Portal logout revokes WebVNC, isolated Code, and mediated-egress bridge sessions
server-side, so their sockets or separate host-scoped cookies cannot retain
access after the portal cookie is cleared. Logout uses a same-origin `POST`; a
cross-site top-level `GET` can show the confirmation page but cannot clear the
cookie or revoke bridge sessions.

Crabbox cannot provision an operator's wildcard DNS, certificate, or ingress
route. When the setting is absent or invalid, Code health remains available but
browser HTTP and WebSocket traffic fails closed with `409 Code origin required`.

## Authentication and scope

Portal pages use a browser-enforced host-only session cookie
(`__Host-crabbox_session`) minted after a successful GitHub login through the
same OAuth flow as `crabbox login`. The Worker converts the cookie to a `Bearer`
token internally; an unauthenticated GET request to a portal page is redirected
to `/portal/login` with a `returnTo` parameter. The session carries owner/org
claims, and the Worker rejects altered or suffixed spellings of its signed
session tokens. Duplicate session cookies fail closed. The session scopes every
page to that identity.

Upgrading retires the legacy `crabbox_session` and `crabbox_code_session`
cookies. Existing portal sessions must sign in once; active browser Code
sessions rebootstrap through their normal portal entrypoint.

Agent/CLI WebVNC `--open` has a narrower non-OAuth entrypoint. An authenticated
shared/admin bearer request can mint a 120-second, one-use opaque viewer ticket
for one visible desktop lease. The CLI writes a private temporary HTML file and
opens its random `file:` URL; that file-origin page submits the ticket to
`/vnc/bootstrap` in a POST body. Consumption binds a non-persistent
`crabbox_webvnc_session` cookie to that lease's `/vnc` path and a server-side
lifetime of at most 30 minutes. The stored principal uses the same owner/org,
shared-token or admin-grant version, grant revocation, and lease ACL checks as
existing Portal and bridge sessions.

The scoped WebVNC cookie is not a Portal login. It can reach only the viewer
page, viewer WebSocket, status, control, theme, and its one-use server-bound
credential handoff for the ticket's lease. It cannot reach the Portal index,
share or release routes, logout, bridge commands, or any other lease. The
viewer page suppresses those controls, and server authorization independently
enforces the same restriction. Expiry, ticket replay, lease mismatch, principal
mismatch, shared/admin token rotation, and grant revocation all fail closed.
Existing GitHub Portal sessions remain unchanged.

```text
session  authenticated GitHub user (owner / org embedded in the token)
admin    sessions whose token carries the admin role
```

- Lease index, lease detail, run detail: a user sees their own leases and
  runs, plus leases shared directly with them or with their org.
- Admin sessions additionally see non-owned (system) leases and external
  runners.
- VNC and code bridges open only when the lease is active, carries the
  matching capability (`desktop` for VNC, `code` for the editor), and the
  session can access the lease.

Tokens for `/v1/...` API calls are separate from the portal cookie. The
Portal never turns a bearer token into a cookie or echoes it to the browser.
The Agent bootstrap sends only its short-lived opaque ticket in a POST body;
the browser address, fragment, and long-lived storage contain neither bearer
nor bootstrap ticket.
Raw Cloudflare Access headers are not trusted on their own: only a verified
Access JWT email can become the portal owner.

## Index `/portal`

The index renders a searchable, paginated, sortable grid that mixes three row
kinds:

- **Leases** — managed or registered boxes with compact provider/target badges,
  state pills, the lease class, icon-only access capabilities (SSH, VNC,
  code, browser), relative time cells, and a confirmed lifecycle action for
  sessions with manage access. Confirmations use a themed in-page HTML dialog
  instead of a browser-native prompt. The action stops and deletes the backing
  machine for coordinator-managed leases. Runtime-adapter-managed registrations
  show **delete workspace** and permanently delete through that adapter; other
  registered leases show **remove registration** and warn that the external
  machine keeps running.
- **External runners** — visibility-only rows for Blacksmith Testboxes synced
  from `crabbox list` output. They render as muted, disabled rows with status
  badges, inferred GitHub Actions run/workflow links, `stuck` markers for
  long-queued or long-running Actions, a copyable local stop command, and
  `stale` markers when a previously visible runner no longer appears in the
  next sync.
- **Dedicated hosts** — when AWS credentials and a region are configured, admins
  see EC2 Mac Dedicated Host inventory as capacity rows. Normal users see only
  hosts attached to active leases already visible to them. Each visible row
  links to a host detail page and its active macOS lease when attached.

Default view rules:

- Defaults to the active filter when any leases, runners, or hosts are active.
- Falls back to showing everything when nothing is active.
- Admin sessions get extra `mine` / `system` filters so personal leases stay
  distinct from external runners and other owners' leases.

Clicking a lease opens its detail page; clicking an external runner or visible
dedicated host opens the matching visibility-only detail page. Unattached host
details and host-pinned launch commands require admin authentication.

## Lease detail `/portal/leases/{id-or-slug}`

The lease detail page shows:

- compact provider/target badges and the lease state pill;
- a status card with host, SSH endpoint, work root, expiry, and the latest
  Linux telemetry sample with CPU capacity and gauges (with sparklines and
  high-load /
  high-memory / high-disk / stale-telemetry pills when thresholds are
  exceeded);
- a bridge panel reporting connection state for the WebVNC, code-server, and
  mediated egress bridges, including host/client state for an active egress
  session;
- an access panel with copy-to-clipboard commands for `crabbox ssh`,
  `crabbox run`, the WebVNC bridge, the code bridge, and (when egress is
  active) `crabbox egress status` / `crabbox egress stop`;
- a viewport-fitted "recent runs" grid with state filters;
- a stop button for coordinator-managed leases, a destructive delete-workspace
  button for runtime-adapter-managed registrations, or a metadata-only remove
  registration button for other client-managed leases, when the session can
  manage the lease.

Owners and users with `manage` access see a share control in the lease
header. The share page (`/share`) adds or removes individual users, sets
org-wide access (`use`, `manage`, or off), or clears sharing entirely; it can
also render embedded (`?embed=1`) inside the VNC viewer's share dialog. A
`use` share can open visible lease pages and portal bridges; a `manage` share
can also view or change the sharing roster and use the matching stop or
deregistration action. Lease API responses shown to `use` recipients omit the
sharing roster.
For adapter-managed registrations, that matching action deletes the external
workspace rather than only removing coordinator metadata.

`/portal/leases/{id-or-slug}/vnc` and `/portal/leases/{id-or-slug}/code/` are
not ordinary pages. VNC opens a noVNC viewer that talks to the lease's desktop
over a WebSocket; the code path proxies code-server HTTP and WebSocket traffic
straight through. Both remove the need for a local SSH tunnel to reach the
desktop or editor. If browser clipboard permission is unavailable, WebVNC uses
an in-page text dialog for manual paste input. Mediated egress has no portal
page — it is operator-driven and never opens an HTML view, so it lives under the ticketed
`/v1/leases/{id-or-slug}/egress/...` routes instead. See
[Interactive desktop and VNC](interactive-desktop-vnc.md),
[code command](../commands/code.md), and [Mediated egress](egress.md).

Current clients send bridge tickets in an `X-Crabbox-Bridge-Ticket` WebSocket
upgrade header, with a bearer-header retry for older coordinators. Coordinators
reject bridge tickets in URL query strings by default, so bridge credentials
stay out of request targets and logs. Operators who need a temporary legacy
rollout window can set `CRABBOX_ALLOW_QUERY_BRIDGE_TICKETS=1`; remove that
setting after affected clients upgrade.

## Run detail `/portal/runs/{run-id}`

Run detail mirrors the `/v1/runs/...` resources but reads through the browser
session cookie, so a run can be inspected without pasting a bearer token into
the browser. The page renders:

- the command, owner, lease, provider/target metadata, exit status, phase,
  duration, and log size;
- the blocked stage and retry hint when the run was classified;
- a JUnit summary and a failure list when the run attached results;
- a copyable retained log tail;
- a searchable, paginated event table with event-type filters;
- bounded load, memory, and disk trend lines for longer Linux runs that
  attached mid-run telemetry samples.

`/portal/runs/{run-id}/logs` returns the retained log as `text/plain`, and
`/portal/runs/{run-id}/events` returns events as JSON (with `after` and
`limit` query parameters). Both stay raw on purpose so they are easy to copy
or pipe.

## External runner detail `/portal/runners/{provider}/{runner-id}`

External runner detail is visibility-only. It shows owner/org, inferred GitHub
Actions ownership (repo, workflow, run id, status), lifecycle timestamps, a
copyable local stop command, and a boundary note explaining that Crabbox does
not own the machine. These runners do not heartbeat through Crabbox and do not
participate in Crabbox lease expiry, cleanup, telemetry, or cost accounting.
The page exists so operators have a single URL to share when an external
runner looks stuck.

## Dedicated host detail `/portal/hosts/{provider}/{host-id}`

When the broker can list EC2 Mac Dedicated Hosts, each host has a detail page
showing its state, region, zone, instance type, and placement. If an active
macOS lease is attached, the page surfaces that lease's SSH endpoint and
access bridges and links to its VNC/code views; the VNC POST route can enable
VNC on the attached lease. With no active lease, the page offers a copyable
host-pinned `crabbox run` command so the host can still be used as macOS
capacity.

## Why server-rendered

The portal is intentionally a thin server-rendered surface, not a SPA:

- the Worker already owns lease and run data, so rendering at the edge avoids
  a separate API/UI deployment;
- pages stay copy-pasteable, and URLs deep-link to a specific lease, run,
  runner, or host;
- there is no build step, no JavaScript framework, and no offline session
  management to maintain;
- the portal cannot drift from the API because both serve the same Durable
  Object state.

Adding a portal feature usually means a new render in `worker/src/portal.ts`,
a new route in `worker/src/fleet.ts` (often a matching `/v1` endpoint), and a
doc update here.

Related docs:

- [Coordinator](coordinator.md)
- [Broker auth and routing](broker-auth-routing.md)
- [History and logs](history-logs.md)
- [Telemetry](telemetry.md)
- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [Source map](../source-map.md)
