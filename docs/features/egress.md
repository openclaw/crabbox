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
  and opens the real outbound TCP connections. By default, it resolves each
  allowed hostname once, rejects non-public results, and dials the validated IP
  address directly so DNS cannot retarget the connection after the allowlist
  check. Remote services then see the operator's public IP. An explicitly
  selected upstream proxy can own the final outbound connection instead.
- **Coordinator session** consumes one-use tickets, pairs the host and client
  sockets by `leaseID`/`sessionID`, and reports status. Cloudflare bridge
  sockets survive Durable Object hibernation. Node sockets are process-local;
  after a coordinator restart, rerun `crabbox egress start` to mint tickets and
  restart the lease-side client. A newer session of the same role replaces an
  older one.

The bridge multiplexes many TCP connections over a single WebSocket per side
(browsers open several sockets at once), keyed by a per-connection ID.

## Quick start

### Run one job

Use `egress run` on an existing Linux SSH lease you own exclusively. It starts
the bridge, runs the command, and cleans up its egress session afterward:

```sh
crabbox egress run --id swift-crab --allow api.ipify.org --no-sync --no-hydrate -- \
  curl --fail --proxy http://127.0.0.1:3128 https://api.ipify.org
```

The command explicitly points `curl` at the lease proxy. Other applications
need their own proxy setting; `egress run` does not inject proxy variables,
credentials, or CA trust. The example skips source sync and configured Actions
hydration because it uses an existing tool. Omit those flags to keep normal
[`crabbox run`](../commands/run.md) defaults for a repository workload.

To run a script without placing its contents on the command line, use
`--script-stdin` and pass any script arguments after `--`:

```sh
crabbox egress run --id swift-crab --allow api.example.com \
  --no-sync --no-hydrate --script-stdin -- smoke < ./scripts/test-live.sh
```

Crabbox reads the entire script before changing transport state. The script
must configure its application's lease-local proxy. The lease remains available
after the job; `egress run` owns the egress session, not lease allocation or
release. See [session lifecycle and cleanup](#session-lifecycle-and-cleanup).

### Keep a browser session open

Lease a desktop+browser box, start egress, then launch a browser through the
proxy and watch it in the WebVNC portal:

```sh
crabbox warmup --provider hetzner --desktop --browser
crabbox egress start --id swift-crab --profile discord --daemon
crabbox desktop launch --id swift-crab \
  --browser \
  --url https://discord.com/login \
  --egress discord \
  --webvnc \
  --open
```

`egress start`:

1. resolves the lease through the coordinator;
2. copies the lease-side egress client over SSH;
3. creates a client ticket, sends it through SSH stdin to start the client on
   the loopback proxy port, and waits for the lease proxy to come up;
4. creates a host ticket and starts the local host agent (in the background
   with `--daemon`, otherwise in the foreground).

`desktop launch --egress <profile>` passes `--proxy-server=http://<proxy>` to
the launched browser (default proxy `127.0.0.1:3128`, override with
`--egress-proxy`). It requires `--browser`. Start `egress start` first so
something is listening on the lease proxy port.

`egress run` and `egress start` install a Linux helper over SSH. For non-Linux
boxes, set up the client and host pieces manually with the low-level commands.

### Upstream proxies

Use `--upstream-proxy-env <ENV_NAME>` on `egress run`, `start`, or `host` to
forward destination connections through a trusted HTTP or HTTPS CONNECT
proxy. Have your proxy or credential manager populate a dedicated local
variable, then select it by name:

```sh
crabbox egress run --id swift-crab --allow api.example.com \
  --upstream-proxy-env CRABBOX_EGRESS_UPSTREAM_PROXY --no-sync --no-hydrate -- \
  curl --fail --proxy http://127.0.0.1:3128 https://api.example.com/health
```

The data path becomes:

```text
app in lease -> lease proxy -> coordinator bridge -> host agent
  -> explicitly selected upstream proxy -> destination
```

The variable must contain an `http://` or `https://` URL with a host and
optional port and user information. Paths other than `/`, queries, and
fragments are rejected. Credentials in the URL use Basic proxy
authentication. HTTPS proxies use TLS 1.2 or later, hostname verification, and
the host's system trust roots; Crabbox provides no TLS verification bypass.
HTTP proxy authentication travels over the connection without TLS, so use it
only over a trusted transport, such as host loopback.

Only the variable name enters arguments. The URL and upstream credentials stay
on the host, are omitted from Crabbox diagnostics, and are not copied to the
remote helper. `egress run` also excludes the selected variable from remote
environment exports, even when an allowlist names it or matches its prefix;
host and coordinator environments are unchanged. Daemons inherit the host
environment. A dedicated variable
keeps destination routing separate from `HTTP_PROXY` and `HTTPS_PROXY`, which
the coordinator clients also honor. Set those standard variables only if
coordinator traffic should use them too.

Omitting the flag selects direct destination connections, even when standard
proxy variables are set. Passing an empty name, an unset or empty variable, or
an invalid URL fails before setup. Once selected, an upstream connection, TLS,
or authentication failure never falls back to direct egress.

The host still enforces the allowlist and requires the destination to resolve
locally to at least one public address. Direct egress dials only those validated
addresses. With an upstream, the host sends the original hostname in `CONNECT`;
the trusted proxy owns final DNS resolution, public-address pinning, and any
credential substitution. The local preflight cannot pin the proxy's connection.

For a proxy that substitutes application credentials, the proxy owner must
supply the remote application's registered placeholder and any required CA
bundle. Configure the app to use the lease proxy and trust that CA through its
normal TLS settings. Crabbox transports the requests; it neither creates
credential grants nor provisions app credentials or trust. HTTPS proxy trust
on the host and application TLS trust inside the lease are separate settings.

If a credential grant lasts only as long as an owning subprocess, launch
`egress run` as that subprocess. It stays in the foreground through the job and
cleanup, keeping the transport within the grant's lifetime. The external proxy
still owns grant creation and revocation. With manual `start`, the controller
must maintain that lifetime and clean up both sides; `--daemon` does not extend
a grant.

See the [command reference](../commands/egress.md) for all flags and manual
`host`/`client` setup.

## Session lifecycle and cleanup

There is one active egress session and client per lease. `egress run` checks
status under the local lease lock and refuses an existing connected host or
client. Disconnected session metadata permits reuse; when connection details
are unavailable, `run` uses the coordinator's coarse active state.
`egress start` replaces the existing session. Both require exclusive lease
ownership: the status check cannot provide atomic admission across controllers
on different hosts.

For `egress run`, Crabbox waits for the remote listener and host bridge before
starting the workload through the existing `run` command. Transport setup goes
to stderr; workload output and exit status retain their usual behavior. A
connected bridge does not prove upstream authentication or application readiness.

Completion cancels and joins the host bridge, then stops the matching remote
helper. Cancellation or bridge failure also cancels the workload operation and
closes pending and active tunnels. Existing [`run` cancellation
semantics](../commands/run.md) still apply to the remote workload. Helper cleanup
gets a fresh 60-second deadline even when the job context has been cancelled.
A cleanup failure makes the command fail; an existing workload failure keeps
its exit status. Supervisors should allow at least 75 seconds after SIGTERM
before forcing termination so cleanup can finish.

`run` and `start` print this line before installing or starting the remote helper
(`run` writes it to stderr):

```text
egress session: lease=<lease-id> session=<session-id>
```

Keep the ID for recovery if automatic cleanup fails. With manual `start`,
record it as soon as it appears, including daemon runs; it identifies cleanup
even after failed setup. The line does not mean the bridge is ready. Check
`egress status` for `host=true client=true`, then verify an application request.

For manual cleanup, stop the owned host process and revoke its credential
grant, then stop that remote session:

```sh
crabbox egress stop --id swift-crab --session <session-id>
```

Scoped stop leaves local host daemons and foreground hosts untouched. It needs
SSH access, a Linux lease with pidfd support, and the current helper installed
by `egress run` or `start`. The helper matches the executable and lease/session
arguments, then signals the exact process through a pidfd, avoiding PID reuse.
It sends `SIGTERM`, waits up to five seconds, and sends `SIGKILL` if needed.
No matching client is a successful no-op; an unreachable target or missing
helper returns an error. Stopping a matching client fails if the kernel lacks
pidfd support.

Automatic bootstrap and scoped cleanup share a per-lease lock on the worker.
Before scanning processes, cleanup records a credential-free terminal marker
in the worker's Crabbox state. Bootstrap refuses that session even if its
startup arrives after cleanup. The marker lasts for the lease environment's
lifetime; rerun `egress run` or `start` to get a fresh session ID instead of
reusing a stopped one. This fence covers automatic startup; manually launched
low-level clients remain the operator's responsibility.

For an exclusively owned daemon session, `egress stop --id swift-crab` stops
the local daemon and makes a best-effort attempt to kill the lease client over
SSH. It has no session filter and is unsuitable for stale per-job cleanup.
Ordinary [`crabbox stop`](../commands/stop.md) also attempts egress cleanup before
releasing an SSH lease. Releasing or expiring the lease tears down its
coordinator session.

### Access-protected coordinators

Automatic setup installs the egress client on the lease, so the lease must be
able to reach the coordinator. If local configuration carries Cloudflare
Access credentials (client id/secret/token), `run` and `start` refuse to push
those onto the box. Either:

- configure a public coordinator route that the lease can reach without Access
  credentials; `run` uses that same configuration for bridge and workload;
- for manual sessions, pass `start --coordinator https://broker.example.com`; or
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
- no allowlist means no proxy &mdash; `run`, `start`, and `host` refuse to run without
  `--profile` or `--allow`;
- tickets are one-use, short-lived (120s), and bound to lease, owner/org, role,
  and session;
- automatic client startup carries the ticket through SSH stdin, never SSH,
  shell, or helper arguments, environment variables, or a remote credential
  file; a foreground helper validates and closes bounded SSH input before
  handing it through a private pipe to the detached client, failing closed
  without client startup, login, or ticket-mint fallback on invalid input;
- destinations must match the allowlist and resolve to at least one public
  address. Direct egress excludes private, loopback, link-local, multicast, and
  reserved addresses, including IP literals, and pins the validated address.
  An explicitly selected upstream proxy owns final resolution and pinning;
- a fatal bridge setup error (lease forbidden, gone, or conflicting session)
  stops the daemon instead of restarting it;
- releasing or expiring the lease tears down the session.

The host's startup line names the lease, session, profile, and allowlist so the
operator can confirm scope before traffic flows. Selecting an upstream does
not bypass the destination preflight.

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

## Troubleshooting

| Symptom | Action |
| --- | --- |
| `--upstream-proxy-env requires an environment variable name` | Supply a valid name, not a URL or an empty string. Omit the option only when direct egress is intended. |
| `upstream proxy environment variable … is empty` or a URL validation error | Have the proxy owner populate the selected variable before launching Crabbox. Use an HTTP(S) URL without a path, query, or fragment; do not print its value while diagnosing. |
| `connect upstream proxy failed` | Check that the proxy is running and reachable from the host. Requests remain blocked until it recovers; Crabbox will not bypass it. |
| `upstream proxy CONNECT returned HTTP 407` | Check the URL's Basic credentials and whether the proxy's grant is still active. Restarting or daemonizing Crabbox cannot renew a revoked grant. |
| `upstream proxy TLS handshake failed` | Check the HTTPS proxy's hostname and certificate chain against the host's system trust store. Install the proxy owner's CA through the host's trust configuration if needed. |
| The remote app reports a TLS certificate error | If the upstream intercepts application TLS, install its CA in the remote app's trust configuration. Trusting the HTTPS proxy on the host does not configure the app. |
| Coordinator login or WebSocket setup unexpectedly reaches the upstream | Use a dedicated variable such as `CRABBOX_EGRESS_UPSTREAM_PROXY`; check inherited `HTTP_PROXY` and `HTTPS_PROXY`, which also affect coordinator traffic. |
| `egress stop --session requires a reachable lease target` or an SSH failure | Restore coordinator/SSH access and retry the scoped stop. Stop the owned host and revoke its grant even when remote cleanup is unavailable. |
| Scoped stop reports a missing helper or an unsupported cleanup command | The lease must use the current helper installed by `egress run` or `start`. Check local/remote versions; start a fresh session with the updated CLI on an exclusively owned lease. Do not treat failed cleanup as success. |
| Cleanup reports `Linux pidfd support required` | Use a Linux kernel with pidfd support. Scoped stop deliberately has no PID-only signaling fallback. |
| `egress client session was stopped` | The terminal marker rejected late bootstrap. Start a fresh session; do not reuse the stopped ID or remove its marker. |

For remote startup failures, inspect `/tmp/crabbox-egress-client.log` over
[`crabbox ssh`](../commands/ssh.md). `egress status` confirms bridge connections,
but does not test upstream authentication or the destination application.

## Verification

On an exclusively owned Linux lease with `curl` installed:

```sh
crabbox egress run --id swift-crab --allow api.ipify.org --no-sync --no-hydrate -- \
  curl --fail --proxy http://127.0.0.1:3128 https://api.ipify.org
crabbox egress status --id swift-crab
```

Expected evidence:

- the request succeeds and reports the host-side egress IP;
- after `run` exits, `egress status` reports `host=false client=false` and the
  lease proxy no longer listens;
- for a manual browser session, status reports `host=true client=true` while
  the browser runs, and stopping egress removes the proxy.

For an upstream session, add `--upstream-proxy-env` with your populated variable
and allow a suitable test endpoint. Verify the upstream's observed exit path
or credential substitution through the remote app, then interrupt or revoke
the upstream and confirm requests fail. A connected bridge alone does not
prove the upstream path or fail-closed behavior.

## Source map

- egress command implementation: `internal/cli/egress.go`
- upstream CONNECT transport: `internal/cli/egress_upstream.go`
- scoped Linux client cleanup: `internal/cli/egress_cleanup_linux.go`
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
