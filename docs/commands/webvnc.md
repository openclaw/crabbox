# webvnc

`crabbox webvnc` opens a desktop lease in a browser tab. Coordinator-backed
leases bridge into the authenticated coordinator portal. Direct macOS providers
such as Tart and Parallels use that same portal whenever coordinator login is
configured; Crabbox registers the external lease until `crabbox stop` or normal
coordinator expiry. Without coordinator auth, direct macOS keeps a
localhost viewer as its offline fallback. The local container provider serves
noVNC locally over an SSH tunnel. An existing loopback VNC tunnel can use the
provider-neutral `webvnc local` bridge on a macOS or Linux host.

```sh
crabbox warmup --desktop
crabbox webvnc --id swift-crab
crabbox webvnc --id swift-crab --network tailscale
crabbox webvnc --id swift-crab --open
crabbox webvnc --id swift-crab --open --take-control
crabbox webvnc --id swift-crab --target macos --preflight
secret-command | crabbox webvnc local --vnc-host 127.0.0.1 --vnc-port 5900 --username admin --password-stdin --open
crabbox webvnc daemon start --id swift-crab --open
crabbox webvnc daemon status --id swift-crab
crabbox webvnc daemon list
crabbox webvnc daemon stop --id swift-crab
crabbox webvnc status --id swift-crab
crabbox webvnc reset --id swift-crab --open
```

The lease must have the `desktop` capability. Reusing a lease for WebVNC
requires that capability to be present (see
[Capabilities](../features/capabilities.md)).

## How it works

`webvnc` resolves the lease the same way `crabbox vnc` does, verifies the
`desktop` capability, and probes the runner's loopback VNC service
(`127.0.0.1:5900`) over SSH. Coordinator-backed leases then mint a short-lived
bridge ticket over the authenticated coordinator API and connect the local
bridge to the coordinator portal. The local container provider instead starts
`websockify` inside the local container and tunnels that local noVNC endpoint to
the browser. Direct-SSH startup records a private remote process identity and
uses an owner-specific loopback port allocated under a host-wide remote lock.
The owner ID supplies only the first candidate: occupied ports are skipped and
startup bind collisions retry another candidate. The exact selected port is
persisted in the owner's mode-`0600` identity and reused only with that owner's
exact listener and recorded websockify process, so concurrent workspaces on one
SSH host remain isolated even when their first candidates collide. Adapter
raw ownership material is domain-separated into the public owner ID before any
subprocess starts. The remote websockify process carries only a fresh per-launch
nonce, recorded in its identity, rather than the adapter owner token. After the
SSH tunnel opens, Crabbox proves that the exact expected SSH process owns the
local listener before retrieving a VNC credential. Password-authenticated VNC
sessions complete a noVNC WebSocket and VNC password challenge, recheck
ownership, and only then use the authenticated one-time browser handoff. In ARD
mode, whether the viewer uses the coordinator portal or a local bridge, the
Crabbox relay authenticates to Screen Sharing itself; it exposes no credential
endpoint and sends no account credential to the browser. A missing/zero
expected PID, unrelated listener, or unauthenticated endpoint never receives a
password probe or handoff.

When the CLI is authenticated with a coordinator bearer token, `--open` mints a
second one-use ticket for the Portal viewer itself. The CLI writes a private
temporary HTML file and opens its random `file:` URL; that file-origin page
submits the opaque ticket in a POST body.
The coordinator consumes it once and creates a short-lived, lease-scoped
viewer-only session. This path does not start GitHub OAuth and does not place
the bearer, viewer ticket, coordinator viewer URL, or VNC credential in the
browser URL or process arguments. GitHub-authenticated human sessions continue
to open the existing Portal path.

Deployments that expose the browser portal and bridge agent through different
origins can set `CRABBOX_WEBVNC_AGENT_BASE_URL` to the agent's exact HTTPS
origin. Ticket creation, status, and portal URLs continue using the configured
coordinator; only the outbound agent WebSocket uses the override. HTTP is
accepted only for an explicit loopback port.

The data path is:

```text
browser noVNC
  <-> coordinator portal websocket
  <-> local crabbox webvnc process
  <-> SSH tunnel
  <-> runner 127.0.0.1:5900
```

This path also carries Tart and Parallels macOS Screen Sharing sessions when a
coordinator login is configured. The portal chrome, sharing, controller,
clipboard, status, reset, and daemon controls are the same as for Linux and
Windows.

For the local container provider, the data path is local:

```text
browser noVNC at 127.0.0.1:<port>
  <-> SSH tunnel
  <-> runner websockify
  <-> runner 127.0.0.1:5900
```

The local `crabbox webvnc` process is not just a launcher; it is the live
bridge between the browser and the SSH-tunneled VNC socket. Keep it running
while the browser tab is open. If the browser tab reloads or drops, the bridge
re-registers so the portal retry can reconnect. If the SSH tunnel process
exits, the foreground bridge exits instead of leaving a stale viewer URL; a
background supervisor observes that exit and starts a freshly resolved bridge.

### Existing local VNC tunnel

On macOS and Linux, `crabbox webvnc local` adds the browser viewer to an
already-running VNC tunnel. It neither creates nor owns the underlying tunnel:

```sh
secret-command | crabbox webvnc local \
  --vnc-host 127.0.0.1 \
  --vnc-port 5900 \
  --username admin \
  --password-stdin \
  --security-type vnc \
  --open
```

The VNC source must be the literal IPv4 loopback address. Crabbox requires
exactly one current-user process owner, pins its PID and process-start identity,
verifies that identity around every connection, stops if it changes, and checks
the RFB banner before reading the password from stdin. The browser listener is
also bound to `127.0.0.1`; use `--local-port <port>` only when a fixed browser
port is required. The command stays in the foreground and must remain running
alongside the underlying tunnel.

Use `--security-type vnc` when the server advertises account-based security
types ahead of standard VNC password authentication but the supplied password
is the independent legacy VNC password. Crabbox then filters only the initial
RFB security offer so the browser must choose type 2; all subsequent VNC bytes
remain end-to-end between noVNC and the loopback source. The default `auto`
preserves the server's advertised order.

The self-contained noVNC handoff is a mode-`0600` temporary file. It contains
only a fresh per-process bridge token, never the VNC password. In `auto` and
`vnc` modes, the password remains in process memory and is returned to that
file-origin viewer only after a token-authenticated POST. In macOS ARD mode,
the local relay performs account authentication itself: it does not register a
credential endpoint, and the viewer neither receives nor fetches the account
username or password. ARD account credentials are never copied into arguments,
browser URLs, the handoff file, or a browser credential response.
An External provider may source its ARD password from an operator-approved
environment variable; the local relay reads that value and keeps it
server-side. The explicitly supplied legacy VNC username remains visible in the
local command arguments. The WebSocket relay requires the same token as a
per-session subprotocol. The handoff is removed when the bridge exits.

The bridge opens a small warm pool of backend sessions (4 slots for Linux and
Windows targets, 2 for macOS). That pool is what the `slots=` field in
`webvnc status` reports, and it lets multiple portal viewers join the same
lease: one viewer is the controller, later viewers join in observer mode, and
any viewer can press **take over** to become the controller. The prior
controller stays connected as an observer and can reclaim control the same way.
Observer mode is a collaboration UX for trusted shared leases; it relies on the
portal noVNC client staying read-only and is not a hostile-client isolation
boundary.

`--take-control` asks the viewer to request control once it connects. Bearer
bootstrap sessions carry that hint server-side; the existing human Portal path
uses the URL fragment. It is a viewer hint, not a new permission boundary:
Portal auth and lease sharing still decide who can open the session.

## Security boundary

WebVNC keeps the same security boundary as `crabbox vnc`:

- VNC stays bound to the runner's loopback interface.
- The cloud provider does not open public VNC ingress.
- The coordinator authenticates the browser through portal auth, and the bridge
  through a single-use short-lived ticket. The CLI sends the ticket as an
  `X-Crabbox-Bridge-Ticket` WebSocket upgrade header so it stays out of
  WebSocket URLs while leaving ordinary coordinator authentication intact. A
  bearer-header retry supports older coordinators; current coordinators reject
  bridge tickets in URL query strings by default. Operators who need a
  temporary legacy rollout window can set
  `CRABBOX_ALLOW_QUERY_BRIDGE_TICKETS=1`; remove that setting after affected
  clients upgrade.
- Shared/admin bearer `--open` uses a distinct 120-second, one-use Portal
  bootstrap ticket bound to the lease, owner, org, bearer identity, and current
  grant version. Consumption creates only a browser-session cookie scoped to
  that lease's `/vnc` path; the server expires it after at most 30 minutes and
  revalidates lease access and grant revocation on every request. It cannot
  access the Portal index, sharing, logout, bridge commands, or lease-management
  routes.
- A split agent origin is accepted only from the explicit
  `CRABBOX_WEBVNC_AGENT_BASE_URL` environment setting and must be one exact
  HTTPS origin (or loopback HTTP with an explicit port).
- The noVNC client is served from the coordinator origin, not a third-party CDN.
- The local `crabbox webvnc` process must keep running while the browser uses
  the desktop.
- Direct-SSH noVNC reuses only a Crabbox-started remote websockify process whose
  start identity, public owner ID, private process nonce, command, and exact
  loopback socket owner all match. Status withholds the local credential URL
  until a fresh authenticated WebSocket probe succeeds.

`--network tailscale` changes only the SSH endpoint used for the local tunnel.
The runner VNC service stays bound to loopback.

## Subcommands

### Foreground bridge

`crabbox webvnc --id <lease-id-or-slug>` runs the bridge in the foreground.
Leave it running while the browser tab is open. With `--open` it opens the
portal page once the bridge reports connected.

For macOS Screen Sharing leases, `--preflight` verifies the RFB/Apple Remote
Desktop authentication path through the same SSH tunnel and exits before opening
the portal bridge. The password still comes from the configured credential
source and is not printed.

### daemon start / status / list / stop

Use the `daemon` subcommands to run the bridge in the background without a tmux
or foreground shell:

```sh
crabbox webvnc daemon start --id <lease-id-or-slug> --open
crabbox webvnc daemon status --id <lease-id-or-slug>
crabbox webvnc daemon list
crabbox webvnc daemon stop --id <lease-id-or-slug>
```

`daemon start` writes a per-lease log and private identity file under the local Crabbox state
directory (`webvnc/<lease>.log` and `.pid`), truncates the log on each start,
and prints `webvnc daemon: ready` once the bridge reports connected (otherwise it
prints a hint to check `webvnc status`). A background supervisor restarts the
child bridge if it exits. `daemon status` reports the local pid/log and whether
the process is alive or stale. `daemon list` scans all recorded local WebVNC pid
files and prints alive/stale state for each bridge, which is useful after agent
runs leave helpers behind. `daemon stop` terminates both the supervisor and the
active child bridge, but only after the recorded workspace, process start
identity, and per-process nonce all match the live Crabbox WebVNC process. A
legacy, copied, or PID-recycled identity is never reused or signaled. Stop also
terminates and verifies the complete recorded process group before removing the
private identity. If the supervisor PID was recycled while descendants may
remain, Crabbox retains that identity and fails closed instead of losing its
only cleanup handle.
On Windows hosts, daemon status, reuse, and stop inspect process creation time
and command line through native process APIs; they do not require a Unix `ps`
binary. Manual Windows SSH and WebVNC tunnels remain supported unchanged.
Start, status checks, stop, identity publication, and launch-gate release are
serialized by a private per-workspace OS file lock, so concurrent Crabbox
processes cannot publish or revoke crossed daemon identities. Automatic local
port selection claims a host-wide per-port OS reservation before probing the
loopback socket. The supervisor inherits that reservation for its full lifetime,
including child restart gaps, independently of the caller's state directory.
The reservation is a bound loopback datagram socket, so it has no replaceable
filesystem pathname. macOS bridges claim a second reservation for their
internal SSH-tunnel port before that tunnel starts; foreground macOS bridges
also claim their browser-facing port before beginning SSH setup.
The supervisor cannot start the credential-bearing bridge until its private
identity file is flushed and installed; loss of the starting process before
that handshake closes the launch gate instead of leaving an untracked daemon.
Ordinary manual and automatically registered daemon children keep their normal
provider/coordinator heartbeat across supervisor restarts. Adapter-owned
persistent bridges are explicitly marked internal: only those children resolve
in provider-side-effect-free mode, never reclaim or touch a lease, and never
start a competing heartbeat. Their durable identity binds an opaque digest of
the adapter state, provider scope, and resource identity; reuse requires an
exact digest and live no-side-effects command. Raw ownership material remains
adapter-local; daemon argv carries only the domain-separated public ID, and
status reports only whether it matched while redacting the ID from command
diagnostics. Each start and status resolution
also receives and validates the adapter's full persisted lease, attempt,
slug, resource, and provider-scope identity. Direct-SSH status checks the exact
listener owner before credential retrieval and immediately before and after its
VNC authentication probe; without a positive expected owner PID it reports no
credential or viewer URL.
Adapter lifecycle
reconciliation remains their sole owner.

The older `crabbox webvnc --id <lease> --daemon`, `--background`, `--status`,
and `--stop` forms remain accepted as compatibility aliases, but new docs and
automation should use the explicit `daemon` subcommands.

### status

`crabbox webvnc status --id <lease-id-or-slug>` prints the full health view:
local daemon pid/log, the SSH tunnel command, target VNC reachability, the
coordinator bridge/viewer state, recent bridge log events, the portal URL and
credential state, and the exact native VNC fallback command. Credential-bearing
viewer URLs, usernames, and passwords are redacted by default.

```text
lease: cbx_0123456789ab slug=swift-crab provider=aws target=linux
webvnc daemon: pid=12345 log=...
vnc target: reachable 127.0.0.1:5900 managed=true
ssh tunnel: ssh ... -o GatewayPorts=no -L 127.0.0.1:5901:127.0.0.1:5900 ...
portal bridge: connected=true viewers=2 observers=1 slots=2
portal controller: alice
event: 2026-05-07T12:00:00Z bridge_connected
webvnc: [redacted]
fallback: crabbox vnc --provider aws --target linux --network tailscale --id cbx_... --open
```

When a layer is unhealthy, the CLI prints `problem:`, an optional `detail:`, and
one or more exact `rescue:` commands in the command output, not only in docs.
Common problems include `VNC bridge disconnected`, `WebVNC daemon not running`,
`waiting for an available WebVNC observer slot`, and `VNC target unreachable`.
If the portal path looks unhealthy but the target VNC service is reachable, the
output also prints the native `crabbox vnc ... --open` fallback with the same
provider/target/network flags. Running `status` with `--network public` or
`--network tailscale` carries that same network selection into the printed
fallback.

For password-authenticated VNC sessions, `--open` hands credentials to the
browser without printing them. An ARD relay keeps the account credential
server-side, including when it connects to a coordinator portal.
Credential-free viewer URLs and recovery commands remain visible. For a manual
coordinator credential handoff in a private terminal, explicitly pass
`--redact-credentials=false`; treat that output as a reusable secret while the
bridge remains active. Daemon and controller output always stays redacted.

### reset

`crabbox webvnc reset --id <lease-id-or-slug> --open` recovers a portal that is
stuck on a stale bridge, viewer, or session. Reset closes that lease's
coordinator WebVNC sockets, stops that lease's local daemon (after verifying it
is a Crabbox WebVNC process), restarts the target desktop helper/VNC services,
starts a fresh background bridge, and prints the new portal URL. Direct-SSH
reset terminates remote websockify only when its persisted PID, process-start
identity, public owner ID, private process nonce, command, and exact listener all
match. A stale recorded-port collision is preserved and replacement startup
allocates another free loopback port instead of signaling the unrelated
process. As with
`status`, the printed native fallback reflects `--network`. The public Linux
desktop image permits only a fixed `/bin/bash` invocation of the root-owned
reset helper; that helper uses fixed system binaries and a trusted `PATH`
instead of granting general passwordless sudo.

## Portal and passwords

For a password-authenticated coordinator bridge, `--open` opens the portal page
after the bridge starts. When the CLI uses a shared/admin coordinator bearer,
it sends any VNC credential handoff and the viewer bootstrap over authenticated
API requests, writes a private temporary HTML file, and opens only its random
`file:` URL. The file-origin page transfers the opaque viewer ticket in a
browser POST body. The resulting cookie is non-persistent, viewer-only, and
scoped to one lease's WebVNC path. The credential handoff stays server-side
until that session consumes it once.

GitHub-authenticated human Portal sessions retain the existing behavior: a
short-lived, one-use credential handoff ticket may travel in the URL fragment,
the Portal consumes it, loads the credential into browser memory, and removes
the fragment before connecting. Passwords and usernames never enter
CLI-generated Portal URLs. Viewer URLs, usernames, and passwords remain
redacted on stdout unless the operator explicitly sets
`--redact-credentials=false`; credential-free URLs and recovery commands remain
visible. Repeated `--open` calls focus and reload an existing viewer tab for
that lease when the browser permits instead of leaving a second active viewer.

A macOS ARD bridge has a stricter boundary in both local and coordinator-backed
modes: its relay performs ARD authentication, the browser receives no account
credential, and a local viewer has no credential endpoint registered.
`--redact-credentials=false` does not make the ARD account credential available
to the browser. Its portal share action therefore copies a clean URL without
minting a credential handoff; the coordinator still enforces the lease ACL.

The portal page may show `WebVNC daemon not running` or `waiting for VNC bridge`
until the local command has connected. If you opened the portal first, start the
bridge in a terminal and leave it running:

```sh
crabbox webvnc --id <lease-id-or-slug>
```

For human demos, prefer WebVNC over native VNC. Password-authenticated sessions
use a one-use browser handoff without putting the password in the URL; macOS ARD
sessions keep the account password inside the relay process. Use native VNC only
as the fallback printed by `webvnc status` or `webvnc reset`.

The WebVNC toolbar includes clipboard controls. The paste control reads the
local browser clipboard, sends it through noVNC, then sends the target paste
shortcut: Command-V for macOS targets, Ctrl-V for Linux and Windows targets.
When the remote VNC server publishes clipboard text, the copy-remote control is
enabled; click it to write that remote text into the local browser clipboard.
Browsers require a user gesture for clipboard writes, so remote-to-local copy is
explicit instead of fully automatic.

## Flags

```text
--id <lease-id-or-slug>     lease to bridge (also accepted as the first positional arg)
--provider <name>           desktop-capable provider for the lease
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--network auto|tailscale|public
--local-port <port>         local VNC tunnel port (auto-selected when unset)
--open                      open the portal VNC page once the bridge connects
--take-control              ask the portal viewer to request control after connecting
--preflight                 foreground bridge only: validate macOS authentication and exit
--redact-credentials=false  reveal viewer URLs, usernames, and passwords (unsafe)
--reclaim                   claim this lease for the current repo
```

Subcommands: `status`, `reset`, and `daemon start|status|stop`. The bridge,
`status`, and `reset` forms share the bridge flags above except `--preflight`,
which is accepted only by the foreground bridge. The `daemon status` and
`daemon stop` forms take only `--id`.

## Supported providers

- Coordinator-backed Hetzner, AWS, and Azure Linux desktop leases, plus
  coordinator-backed AWS macOS desktop leases.
- The local Docker container provider (`--provider local-container`) is also
  supported. It serves noVNC locally over an SSH tunnel rather than through the
  coordinator portal, so it needs no coordinator login.
- Direct SSH providers that advertise desktop support, including KubeVirt and
  external providers, use the same local noVNC-over-SSH path and need no
  coordinator login.
- Static SSH hosts are intentionally not supported, because the portal cannot
  prove that host-managed VNC credentials and prompts are safe to expose.
- Blacksmith Testbox still owns its own machine connectivity.

## Troubleshooting

`webvnc requires a configured coordinator login`

Configure either a coordinator bearer token for Agent/CLI use or run
`crabbox login --url <broker-url>` for a human GitHub session. A bearer-backed
`--open` bootstraps its scoped Portal viewer without GitHub OAuth. The local
container provider is the exception and needs no coordinator login.

`webvnc requires a configured coordinator login`

The selected provider is coordinator-backed. Direct desktop-capable SSH
providers do not require coordinator login and serve noVNC locally over the
provider SSH connection.

`missing websockify`, `missing flock`, or `missing noVNC web assets`

The direct target exposes VNC but does not have the noVNC package installed.
Install `novnc`, `websockify`, and `util-linux` in the provider image or guest
bootstrap, then retry. `crabbox vnc` remains available as the native-client
fallback.

`target does not expose VNC on 127.0.0.1:5900`

The lease is reachable over SSH, but the desktop service is not ready or was not
provisioned. Create the lease with `--desktop`, or wait for bootstrap to finish
and retry.

The portal keeps saying `WebVNC daemon not running` or `waiting for VNC bridge`

The browser can reach the coordinator, but no local bridge is currently paired
with the lease. Start or restart
`crabbox webvnc daemon start --id <lease> --open`, or run
`crabbox webvnc reset --id <lease> --open` when stale tabs or session state are
likely. If the command is already running, wait for the portal retry or reload
the browser tab.

`waiting for an available WebVNC observer slot`

The portal is reachable, but all bridge slots are already paired with viewers.
Restart the bridge with a current Crabbox CLI so it opens the default backend
pool. If the portal still cannot get a slot, run:

```sh
crabbox webvnc reset --id <lease-id-or-slug> --open
```

If WebVNC remains unreliable, use the exact native fallback command printed by
`crabbox webvnc status --id <lease-id-or-slug>`.

VNC authentication fails

For password-authenticated VNC sessions, retry with `--open` so Crabbox can mint
a fresh one-use browser handoff. If manual entry is required, run the
command in a private terminal with `--redact-credentials=false`; avoid copying
that output into logs, issues, or chat. For a macOS ARD relay, verify the
configured account and approved password environment variable, then run
`crabbox webvnc --id <lease> --target macos --preflight`; ARD credentials are
never handed to the browser.

## Related docs

- [Interactive desktop and VNC](../features/interactive-desktop-vnc.md)
- [Linux VNC](../features/vnc-linux.md)
- [Windows VNC](../features/vnc-windows.md)
- [macOS VNC](../features/vnc-macos.md)
