# webvnc

`crabbox webvnc` bridges a desktop lease into the authenticated coordinator
portal.

Use it when you want the same VNC desktop that `crabbox vnc` opens, but inside
a browser tab instead of a native VNC client.

```sh
crabbox warmup --desktop
crabbox webvnc --id blue-lobster
crabbox webvnc --id blue-lobster --network tailscale
crabbox webvnc --id blue-lobster --open
crabbox webvnc daemon start --id blue-lobster --open
crabbox webvnc daemon status --id blue-lobster
crabbox webvnc daemon stop --id blue-lobster
crabbox webvnc status --id blue-lobster
crabbox webvnc status --id blue-lobster --network tailscale
crabbox webvnc reset --id blue-lobster --open
```

## How It Works

The command resolves the lease like `crabbox vnc`, verifies that the lease has
`desktop=true`, starts the normal SSH tunnel to the runner's loopback VNC
service, mints a short-lived bridge ticket over the authenticated coordinator
API, and opens a websocket bridge to the coordinator with that ticket. The
browser connects to `/portal/leases/<lease>/vnc` after GitHub portal auth, and
the Durable Object pairs that browser websocket with the local bridge process.

The data path is:

```text
browser noVNC
  <-> coordinator portal websocket
  <-> local crabbox webvnc process
  <-> SSH tunnel
  <-> runner 127.0.0.1:5900
```

That means the local `crabbox webvnc` process is not just a launcher. It is the
live bridge between the browser and the SSH-tunneled VNC socket. Keep it
running while the browser tab is open. If the browser tab reloads or drops, the
command re-registers a fresh bridge so the portal retry can reconnect.

## Security Boundary

This keeps the security boundary the same as `crabbox vnc`:

- VNC stays bound to runner loopback.
- The cloud provider does not open public VNC ingress.
- The coordinator authenticates the browser through portal auth and the bridge
  through a one-use short-lived ticket. The CLI sends the ticket as an
  `Authorization: Bearer ...` header so it stays out of websocket URLs and
  proxy/access logs; the coordinator falls back to a `?ticket=` query string
  for older CLIs.
- The noVNC client is served from the coordinator origin, not a third-party CDN.
- The local `crabbox webvnc` process must keep running while the browser uses
  the desktop.

Use `crabbox webvnc daemon start --id <lease> --open` to keep the bridge
running without a tmux or foreground shell. Crabbox writes the bridge log and
pid file under its local state directory, starts each daemon with a fresh log,
and prints `webvnc daemon: ready` once the bridge reports connected. Use
`crabbox webvnc daemon status --id <lease>` for the local pid/log check, and
`crabbox webvnc daemon stop --id <lease>` to kill the background bridge for
that lease. Shutdown terminates both the daemon supervisor and the active child
bridge process.

The bridge keeps a warm pool of backend VNC sessions open (default 4 slots,
which is what the `slots=` field in `webvnc status` reports). That lets
multiple portal viewers join the same lease: one viewer is the controller,
later viewers start in observer mode, and any viewer can press **take over**
to become the controller — including the prior controller, who stays connected
as an observer and can reclaim control the same way. Observer mode is a
collaboration UX for trusted shared leases; it relies on the portal noVNC
client staying read-only and is not a hostile-client isolation boundary.

The older `crabbox webvnc --id <lease> --daemon`, `--background`, `--status`,
and `--stop` forms remain accepted as compatibility aliases, but new docs and
automation should use the explicit `daemon` subcommands.

Use `crabbox webvnc status --id <lease>` for the full health view: local daemon
pid/log, SSH tunnel command, target VNC reachability, coordinator bridge/viewer
state, recent bridge events, portal URL/password, and the exact native VNC
fallback command. If status or reset is run with `--network public` or
`--network tailscale`, the printed native VNC fallback carries the same network
selection.

Typical status output is meant to be directly actionable:

```text
webvnc daemon: pid=12345 log=...
vnc target: reachable 127.0.0.1:5900 managed=true
ssh tunnel: ssh ... -L 5901:127.0.0.1:5900 ...
portal bridge: connected=true viewers=2 observers=1 slots=2
portal controller: peter
event: 2026-05-07T12:00:00Z bridge_connected
webvnc: https://crabbox.openclaw.ai/portal/leases/cbx_.../vnc#password=...
fallback: crabbox vnc --provider aws --target linux --network tailscale --id cbx_... --open
```

When a layer is unhealthy, the CLI prints `problem:`, optional `detail:`, and
one or more exact `rescue:` commands in the command output, not only in docs.
Common problems include `VNC bridge disconnected`, `WebVNC daemon not running`,
`waiting for an available WebVNC observer slot`, and `VNC target unreachable`.
If the browser portal path looks unhealthy but the target VNC service is
reachable, the output also prints the native `crabbox vnc ... --open` fallback
command with the same provider/target/network flags.

Use `crabbox webvnc reset --id <lease> --open` when the portal is stuck on a
stale bridge/viewer/session. Reset closes only that lease's coordinator
WebVNC sockets, stops only that lease's local daemon pid after verifying it is
a Crabbox WebVNC process, restarts the target desktop helper/VNC services, then
starts a fresh background bridge and prints the new portal URL.

`--network tailscale` changes only the SSH endpoint used for the local tunnel.
The runner VNC service stays bound to loopback.

## Portal And Passwords

`--open` opens the portal page after the bridge starts. If the VNC password is
available, the command also places it in the URL fragment for the local browser
tab. URL fragments are not sent to the coordinator, and Crabbox preserves
special characters such as `!` when building the fragment. If the portal login
flow redirects first, the page may still prompt for the VNC password; use the
password printed by the command. If an old browser tab is retrying with a stale
fragment, close it before opening the new bridge URL.

The portal page may show `WebVNC daemon not running` or `waiting for VNC
bridge` until the local command has connected. If you opened the portal first,
start:

```sh
crabbox webvnc --id <lease-id-or-slug>
```

in a terminal and leave it running.

For human demos, prefer WebVNC over native VNC because `crabbox webvnc --open`
preloads the per-lease password in the local browser URL fragment. Use native
VNC only as the fallback printed by `crabbox webvnc status` or
`crabbox webvnc reset`.

The WebVNC toolbar includes clipboard controls. The paste control reads the
local browser clipboard, sends it through noVNC, and then sends the target paste
shortcut: Command-V for macOS targets, Ctrl-V for Linux and Windows targets.
When the remote VNC server publishes clipboard text, the copy-remote control is
enabled; click it to write that remote text into the local browser clipboard.
Browsers require a user gesture for clipboard writes, so remote-to-local copy is
explicit instead of fully automatic.

## Flags

Flags:

```text
--id <lease-id-or-slug>
--provider hetzner|aws|azure
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--network auto|tailscale|public
--local-port <port>
--open
status
reset
daemon start
daemon status
daemon stop
--reclaim
```

## Limitations

Limitations:

- Coordinator-backed Hetzner, AWS, and Azure Linux desktop leases are supported.
- Static SSH hosts are intentionally not supported yet because the portal cannot
  prove that host-managed VNC credentials and prompts are safe to expose.
- Blacksmith Testbox still owns its own machine connectivity.

## Troubleshooting

`webvnc requires a configured coordinator login`

Run `crabbox login` for the coordinator you are using. WebVNC needs both the CLI
bridge and the browser portal to authenticate with the coordinator.

`webvnc currently supports coordinator-backed hetzner/aws/azure desktop leases`

WebVNC is not available for static SSH hosts or Blacksmith Testbox. Use
`crabbox vnc` for static hosts when you explicitly trust the host-managed VNC
service.

`target does not expose VNC on 127.0.0.1:5900`

The lease is reachable over SSH, but the desktop service is not ready or was not
provisioned. Create the lease with `--desktop`, or wait for bootstrap to finish
and retry.

The portal keeps saying `WebVNC daemon not running` or `waiting for VNC bridge`

The browser can reach the coordinator, but no local bridge is currently paired
with that lease. Start or restart `crabbox webvnc daemon start --id <lease>
--open`, or run `crabbox webvnc reset --id <lease> --open` when stale tabs or
session state are likely. If the command is still running, wait for the portal
retry or reload the browser tab.

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

Related docs:

- [Interactive desktop and VNC](../features/interactive-desktop-vnc.md)
- [Linux VNC](../features/vnc-linux.md)
- [Windows VNC](../features/vnc-windows.md)
- [macOS VNC](../features/vnc-macos.md)
