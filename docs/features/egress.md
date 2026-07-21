# Mediated Egress

Read when:

- browser or app QA needs a lease to reach the internet over the same network
  path as the operator workstation;
- using or extending the `crabbox egress` command family;
- choosing between mediated browser/app egress and alternatives such as
  Tailscale exit nodes, Cloudflare Tunnel, or full-VM routing;
- testing web apps that are sensitive to source IP, browser login, or regional
  routing (for example a chat or collaboration app whose login and abuse
  heuristics react to a fresh cloud IP).

## What it does

Some QA scenarios need the runner to look like it is browsing from the operator
machine, not from the provider's default cloud IP. Mediated egress makes a
lease-local browser or app exit to the internet through the machine running the
egress host agent:

```text
Chrome or an app inside a Crabbox lease
  speaks HTTP proxy to a loopback listener inside the lease
  and the real outbound TCP connections leave from the operator machine.
```

This is intentionally per-app (per-process) egress, opted in through a browser
proxy setting. It keeps browser QA reproducible without re-routing every process
on the box. Whole-machine routing is a separate concern; use a Tailscale exit
node for that.

The egress is mediated by the coordinator, but the coordinator is **not** the
egress point. It only pairs two WebSocket bridges by lease and session; the
operator machine opens the actual internet connections.

## Non-goals

Mediated egress is not:

- a public open proxy (it refuses to start without an allowlist);
- a replacement for provider firewalls or SSH access controls;
- a transparent VM-wide VPN;
- a way for the coordinator to become the internet egress point;
- a place to store browser login state, app credentials, or provider secrets.

## Architecture

Mediated egress has two long-running agents joined by one coordinator session:

```text
                    coordinator WebSocket bridge
                   +--------------------------------------+
                   | ticket auth, socket pairing, status, |
                   | allowlist metadata, cleanup          |
                   +------------------+-------------------+
                                       |
                    paired WebSocket streams over HTTPS
                                       |
        +------------------------------+------------------------------+
        |                                                             |
+-------v-----------------+                             +-------------v------+
| lease egress client     |                             | host egress agent  |
| runs inside the lease   |                             | runs on operator   |
| listens on 127.0.0.1    |                             | machine            |
+-----------+-------------+                             +-------------+------+
            |                                                         |
            | HTTP proxy / CONNECT                                    | TCP
            |                                                         |
      +-----v------+                                           +------v-----+
      | Chrome /   |                                           | internet   |
      | app        |                                           | from host  |
      +------------+                                           +------------+
```

- **Lease egress client** runs inside the box and listens on a loopback proxy,
  `127.0.0.1:3128` by default. Chrome or an app is launched with
  `--proxy-server=http://127.0.0.1:3128`. The client parses HTTP proxy requests
  (both `CONNECT host:port` and absolute-form HTTP) and asks the host agent to
  open each connection.
- **Host egress agent** runs on the operator machine. It enforces the allowlist
  and opens the real outbound TCP connections, so remote services see the
  operator's public IP. It resolves each allowed hostname once, rejects
  non-public results, and dials the validated IP address directly so DNS cannot
  retarget the connection after the allowlist check.
- **Coordinator session** consumes one-use tickets, pairs the host and client
  sockets by `leaseID`/`sessionID`, and reports status. Cloudflare bridge
  sockets survive Durable Object hibernation. Node sockets are process-local;
  after a coordinator restart, rerun `crabbox egress start` to mint tickets and
  restart the lease-side client. A newer session of the same role replaces an
  older one.

The bridge multiplexes many TCP connections over a single WebSocket per side
(browsers open several sockets at once), keyed by a per-connection ID.

## Quick start

Lease a desktop+browser box, start egress, then launch a browser through the
proxy and watch it in the WebVNC portal:

```sh
crabbox warmup --provider hetzner --desktop --browser
crabbox egress start --id swift-crab --profile discord --daemon
crabbox desktop launch --id swift-crab \
  --browser \
  --url https://example.com \
  --egress discord \
  --webvnc \
  --open
```

`egress start`:

1. resolves the lease through the coordinator;
2. copies and starts the lease-side egress client over SSH, listening on the
   loopback proxy port;
3. creates a client ticket and waits for the lease proxy to come up;
4. creates a host ticket and starts the local host agent (in the background
   with `--daemon`, otherwise in the foreground).

`desktop launch --egress <profile>` passes `--proxy-server=http://<proxy>` to
the launched browser (default proxy `127.0.0.1:3128`, override with
`--egress-proxy`). It requires `--browser`. Start `egress start` first so
something is listening on the lease proxy port.

`egress start` installs and runs a Linux helper over POSIX shell, so it only
supports Linux lease targets today. For non-Linux boxes, set up the client and
host pieces manually with the low-level commands.

## Commands

```sh
# Orchestrated: start the lease client over SSH plus the local host agent.
crabbox egress start --id swift-crab --profile discord [--daemon]

# Low-level pieces (run each side yourself).
crabbox egress host   --id swift-crab --profile discord
crabbox egress client --id swift-crab --listen 127.0.0.1:3128

# Inspect and tear down.
crabbox egress status --id swift-crab
crabbox egress stop   --id swift-crab
```

Common flags (most accept the lease `--id` or slug, or a positional id):

| Flag | Commands | Notes |
| --- | --- | --- |
| `--id` | all | Lease id or slug. |
| `--provider` | all | Defaults to the configured provider. |
| `--profile` | start, host | Named allowlist (`discord`, `slack`). |
| `--allow` | start, host | Comma-separated host patterns; merged with `--profile`. |
| `--listen` | start, client | Lease-local proxy address; loopback-only (default `127.0.0.1:3128`). |
| `--daemon` | start | Run the local host agent in the background under a supervisor. |
| `--coordinator` | start, host, client, status | Broker URL override (see Access note below). |
| `--ticket` | host, client | Pre-created egress ticket (for manual wiring). |
| `--session` | host, client | Egress session id to join. |

`egress host` and `egress start` refuse to run without an allowlist: pass
`--profile` or `--allow`, otherwise the command exits rather than start an open
proxy.

`egress stop` stops the local host daemon (if any) and kills the remote client
over SSH. Releasing or expiring the lease also tears down the coordinator-side egress
session.

### Access-protected coordinators

`egress start` installs and runs the egress client on the lease, so the lease
must be able to reach the coordinator. If your local coordinator config carries
Cloudflare Access credentials (client id/secret/token), `egress start` refuses
to push those onto the box. Either:

- pass `--coordinator https://broker.example.com` to use a public coordinator
  route the lease can reach without Access credentials; or
- run `egress client` and `egress host` manually with an explicit, safe
  credential plan.

## Profiles and allowlists

Profiles are built-in named allowlists, not config-file entries. Two ship
today:

- `discord` &rarr; `discord.com`, `*.discord.com`, `discordcdn.com`,
  `*.discordcdn.com`, `hcaptcha.com`, `*.hcaptcha.com`
- `slack` &rarr; `slack.com`, `*.slack.com`, `slack-edge.com`,
  `*.slack-edge.com`

For anything else, list patterns explicitly with `--allow`; `--profile` and
`--allow` merge. Patterns are case-insensitive. A `*.` prefix matches the bare
domain and any subdomain (`*.discord.com` matches `discord.com` and
`gateway.discord.com`); all other patterns are exact host matches. The host
agent dials only destinations that match; everything else is rejected with an
`error` frame.

## Coordinator API

The coordinator exposes ticketed egress routes alongside the WebVNC and code
bridges:

```text
POST /v1/leases/{leaseID}/egress/ticket
GET  /v1/leases/{leaseID}/egress/host     (ticketed WebSocket upgrade)
GET  /v1/leases/{leaseID}/egress/client   (ticketed WebSocket upgrade)
GET  /v1/leases/{leaseID}/egress/status
```

Ticket creation requires manage access on an active lease. The request body:

```json
{
  "role": "host",
  "sessionID": "egress_...",
  "profile": "discord",
  "allow": ["discord.com", "*.discord.com"]
}
```

The coordinator returns a one-use ticket (`{ ticket, leaseID, role, sessionID,
expiresAt }`, TTL 120s) and activates the egress session. Agent WebSocket
upgrades on `/egress/host` and `/egress/client` are accepted only after a valid
ticket of the matching role is consumed; a Cloudflare Access service token may
get the request through the edge, but the egress ticket still owns bridge
authorization.

`GET /egress/status` reports the tracked session:

```text
leaseID, active, sessionID, profile, allow, hostConnected, clientConnected,
createdAt, updatedAt
```

All visible callers receive the coarse `active` session state. The detailed
`hostConnected`/`clientConnected` fields reflect whether each side's WebSocket
is currently open and are returned only to owners and callers with `manage`
access.

## Bridge protocol

The host and client speak JSON control frames over their WebSockets, keyed by a
per-connection id:

```text
open     { type: "open",     id, host, port }   client -> host
open_ok  { type: "open_ok",  id }                host  -> client
data     { type: "data",     id, body }          both ways (body is base64)
close    { type: "close",    id }                both ways
error    { type: "error",    id, message }        host  -> client
```

The lease client parses incoming HTTP proxy requests. For `CONNECT host:port`,
it opens a stream and replies `200 Connection Established` to the browser; for
absolute-form HTTP, it forwards the rewritten request as a `data` frame and
defaults to port 80 (443 for `https` URLs). Data is base64-encoded; the read
limit is 2 MiB per message and reads are chunked at 32 KiB.

## Security model

Mediated egress defaults closed:

- the lease listener is validated as loopback-only (`127.0.0.1`/`::1`/
  `localhost`); a non-loopback `--listen` is rejected;
- no allowlist means no proxy &mdash; `host`/`start` refuse to run without
  `--profile` or `--allow`;
- tickets are one-use, short-lived (120s), and bound to lease, owner/org, role,
  and session;
- the host agent dials only allowlisted destinations whose resolved address is
  public; private, loopback, link-local, multicast, and reserved IP ranges are
  rejected, including IP-literal requests;
- a fatal bridge setup error (lease forbidden, gone, or conflicting session)
  stops the daemon instead of restarting it;
- releasing or expiring the lease tears down the session.

The host agent is powerful: it opens internet connections from the operator's
network. Its startup line names the lease, session, profile, and allowlist so
the operator can confirm scope before traffic flows. Mediated egress is
internet-only; it does not provide an opt-in path to private network targets.

## Portal integration

The portal lease detail page surfaces egress when a session exists: the profile
and allowlist summary, host/client connected state, and copyable
`crabbox egress status`/`crabbox egress stop` commands. It does not expose
proxy URLs or ticket material; egress shows as a bridge that exists only while
the local agents run.

## Alternatives

- **Tailscale exit node** routes the whole box through another machine. Use it
  when every process must share one egress path; it is heavier (OS forwarding,
  ACLs, route approval). Mediated egress is the lighter, per-app choice for
  browser/app QA. See [Tailscale](tailscale.md).
- **Cloudflare Tunnel TCP** can expose private TCP without a public listener,
  but still needs host and lease processes plus lifecycle management. Keeping
  egress inside the existing coordinator bridge reuses one auth,
  status, and cleanup model.
- **Coordinator as egress** is explicitly not the goal: the point is to use the
  operator machine's internet path, not the coordinator host. The coordinator
  only mediates.

## Verification

```sh
crabbox warmup --provider hetzner --desktop --browser
crabbox egress start --id swift-crab --profile discord --daemon
crabbox desktop launch --id swift-crab \
  --browser \
  --url https://example.com \
  --egress discord \
  --webvnc \
  --open
crabbox egress status --id swift-crab
```

Expected evidence:

- `egress status` reports `host=true client=true`;
- a browser IP check inside the box shows the host-side egress IP, not the
  cloud provider's;
- the page loads inside the WebVNC desktop;
- stopping or releasing the lease tears down the bridge and the lease proxy.

## Source map

- egress command implementation: `internal/cli/egress.go`
- coordinator ticket/status client: `internal/cli/coordinator.go`
- desktop/browser launch integration: `internal/cli/desktop.go`
- command tree: `internal/cli/cli_kong.go`
- shared WebSocket routing: `worker/src/coordinator-entry.ts`
- coordinator bridge state and routes: `worker/src/fleet.ts`
- portal lease detail status: `worker/src/portal.ts`

Related docs:

- [Server-bound egress session identity (design proposal)](../plan/egress-session-identity.md)
- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [Broker auth and routing](broker-auth-routing.md)
- [Browser portal](portal.md)
- [Tailscale](tailscale.md)
- [Configuration](configuration.md)
