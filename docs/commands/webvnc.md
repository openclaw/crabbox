# webvnc

`crabbox webvnc` opens a desktop lease in a browser tab. For coordinator-backed
leases it bridges into the authenticated coordinator portal. The local container
provider also supports WebVNC by serving noVNC locally over an SSH tunnel.

```sh
crabbox warmup --desktop
crabbox webvnc --id swift-crab
crabbox webvnc --id swift-crab --network tailscale
crabbox webvnc --id swift-crab --open
crabbox webvnc --id swift-crab --open --take-control
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
the browser.

The data path is:

```text
browser noVNC
  <-> coordinator portal websocket
  <-> local crabbox webvnc process
  <-> SSH tunnel
  <-> runner 127.0.0.1:5900
```

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
re-registers so the portal retry can reconnect.

The bridge opens a small warm pool of backend sessions (4 slots for Linux and
Windows targets, 2 for macOS). That pool is what the `slots=` field in
`webvnc status` reports, and it lets multiple portal viewers join the same
lease: one viewer is the controller, later viewers join in observer mode, and
any viewer can press **take over** to become the controller. The prior
controller stays connected as an observer and can reclaim control the same way.
Observer mode is a collaboration UX for trusted shared leases; it relies on the
portal noVNC client staying read-only and is not a hostile-client isolation
boundary.

`--take-control` writes `control=take` into the portal URL fragment, asking the
viewer to request control once it connects. It is a viewer hint, not a new
permission boundary; portal auth and lease sharing still decide who can open the
session.

## Security boundary

WebVNC keeps the same security boundary as `crabbox vnc`:

- VNC stays bound to the runner's loopback interface.
- The cloud provider does not open public VNC ingress.
- The coordinator authenticates the browser through portal auth, and the bridge
  through a single-use short-lived ticket. The CLI sends the ticket as an
  `Authorization: Bearer ...` header so it stays out of WebSocket URLs and
  proxy/access logs; the coordinator falls back to a `?ticket=` query string for
  older CLIs.
- The noVNC client is served from the coordinator origin, not a third-party CDN.
- The local `crabbox webvnc` process must keep running while the browser uses
  the desktop.

`--network tailscale` changes only the SSH endpoint used for the local tunnel.
The runner VNC service stays bound to loopback.

## Subcommands

### Foreground bridge

`crabbox webvnc --id <lease-id-or-slug>` runs the bridge in the foreground.
Leave it running while the browser tab is open. With `--open` it opens the
portal page once the bridge reports connected.

### daemon start / status / list / stop

Use the `daemon` subcommands to run the bridge in the background without a tmux
or foreground shell:

```sh
crabbox webvnc daemon start --id <lease-id-or-slug> --open
crabbox webvnc daemon status --id <lease-id-or-slug>
crabbox webvnc daemon list
crabbox webvnc daemon stop --id <lease-id-or-slug>
```

`daemon start` writes a per-lease log and pid file under the local Crabbox state
directory (`webvnc/<lease>.log` and `.pid`), truncates the log on each start,
and prints `webvnc daemon: ready` once the bridge reports connected (otherwise it
prints a hint to check `webvnc status`). A background supervisor restarts the
child bridge if it exits. `daemon status` reports the local pid/log and whether
the process is alive or stale. `daemon list` scans all recorded local WebVNC pid
files and prints alive/stale state for each bridge, which is useful after agent
runs leave helpers behind. `daemon stop` terminates both the supervisor and the
active child bridge, but only after verifying the recorded pid is a Crabbox
WebVNC process.

The older `crabbox webvnc --id <lease> --daemon`, `--background`, `--status`,
and `--stop` forms remain accepted as compatibility aliases, but new docs and
automation should use the explicit `daemon` subcommands.

### status

`crabbox webvnc status --id <lease-id-or-slug>` prints the full health view:
local daemon pid/log, the SSH tunnel command, target VNC reachability, the
coordinator bridge/viewer state, recent bridge log events, the portal URL and
password, and the exact native VNC fallback command.

```text
lease: cbx_0123456789ab slug=swift-crab provider=aws target=linux
webvnc daemon: pid=12345 log=...
vnc target: reachable 127.0.0.1:5900 managed=true
ssh tunnel: ssh ... -o GatewayPorts=no -L 127.0.0.1:5901:127.0.0.1:5900 ...
portal bridge: connected=true viewers=2 observers=1 slots=2
portal controller: alice
event: 2026-05-07T12:00:00Z bridge_connected
webvnc: https://broker.example.com/portal/leases/cbx_.../vnc#password=...
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

### reset

`crabbox webvnc reset --id <lease-id-or-slug> --open` recovers a portal that is
stuck on a stale bridge, viewer, or session. Reset closes that lease's
coordinator WebVNC sockets, stops that lease's local daemon (after verifying it
is a Crabbox WebVNC process), restarts the target desktop helper/VNC services,
starts a fresh background bridge, and prints the new portal URL. As with
`status`, the printed native fallback reflects `--network`.

## Portal and passwords

`--open` opens the portal page after the bridge starts. When the VNC password is
available, the command also places it in the URL fragment for the local browser
tab and prints it on stdout. URL fragments are not sent to the coordinator, and
Crabbox preserves special characters such as `!` when building the fragment. For
macOS targets the lease username is also surfaced. If the portal login flow
redirects first, the page may still prompt for the VNC password; use the
password printed by the command. If an old tab is retrying with a stale
fragment, close it before opening the new bridge URL.

The portal page may show `WebVNC daemon not running` or `waiting for VNC bridge`
until the local command has connected. If you opened the portal first, start the
bridge in a terminal and leave it running:

```sh
crabbox webvnc --id <lease-id-or-slug>
```

For human demos, prefer WebVNC over native VNC because `crabbox webvnc --open`
preloads the per-lease password in the local browser URL fragment. Use native
VNC only as the fallback printed by `webvnc status` or `webvnc reset`.

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
--provider hetzner|aws|azure
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
--reclaim                   claim this lease for the current repo
```

Subcommands: `status`, `reset`, and `daemon start|status|stop`. The bridge,
`status`, and `reset` forms share the bridge flags above; the `daemon status`
and `daemon stop` forms take only `--id`.

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

Run `crabbox login --url <broker-url>` for the coordinator you are using. Portal
WebVNC needs both the CLI bridge and the browser portal to authenticate with the
coordinator. The local container provider is the exception and needs no login.

`webvnc requires a configured coordinator login`

The selected provider is coordinator-backed. Direct desktop-capable SSH
providers do not require coordinator login and serve noVNC locally over the
provider SSH connection.

`missing websockify` or `missing noVNC web assets`

The direct target exposes VNC but does not have the noVNC package installed.
Install `novnc` and `websockify` in the provider image or guest bootstrap, then
retry. `crabbox vnc` remains available as the native-client fallback.

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

Use the password printed by `crabbox webvnc`. With `--open`, the command tries
to pass the password in the browser URL fragment, but a portal login redirect
can lose that fragment before noVNC sees it.

## Related docs

- [Interactive desktop and VNC](../features/interactive-desktop-vnc.md)
- [Linux VNC](../features/vnc-linux.md)
- [Windows VNC](../features/vnc-windows.md)
- [macOS VNC](../features/vnc-macos.md)
