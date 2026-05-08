# Interactive Desktop And VNC

Read when:

- choosing a desktop target for browser/UI QA;
- opening a lease with VNC or WebVNC;
- diagnosing stale WebVNC viewers, bridge disconnects, or broken desktop
  sessions;
- driving desktop input from agents without hand-written `xdotool`;
- deciding which layer owns desktop setup, browser state, screenshots, or
  credentials.

Crabbox treats desktop access as a lease capability, not a separate remote
access product. A desktop lease still uses the normal Crabbox boundaries:
provider lifecycle, per-lease SSH keys, SSH tunnels, idle expiry, cleanup, and
run history. VNC is a way to inspect or drive the visible session inside that
boundary.

## Quick Start

```sh
crabbox warmup --desktop --browser
crabbox webvnc --id blue-lobster --open
crabbox webvnc status --id blue-lobster
crabbox desktop doctor --id blue-lobster
crabbox vnc --id blue-lobster --open
crabbox screenshot --id blue-lobster --output desktop.png
```

AWS Windows and EC2 Mac use the same VNC command once the desktop lease exists:

```sh
crabbox warmup --provider aws --target windows --desktop
crabbox vnc --id crimson-crab --open

CRABBOX_AWS_MAC_HOST_ID=h-... \
  crabbox warmup --provider aws --target macos --desktop --market on-demand
crabbox vnc --id silver-squid --open
```

Static hosts are explicit and host-managed:

```sh
crabbox vnc --provider ssh --target macos --static-host mac-studio.local --host-managed --open
crabbox vnc --provider ssh --target windows --static-host win-dev.local --host-managed --open
```

## What Crabbox Owns

Crabbox owns:

- the lease lifecycle and cleanup;
- per-lease SSH keys and known_hosts scoping;
- SSH local forwarding to the target's loopback VNC service;
- generated per-lease VNC or OS passwords for managed desktop leases;
- `desktop=true` and `browser=true` lease metadata;
- screenshots and desktop launch commands that operate inside the lease.

Scenario systems such as Mantis own:

- product-specific login and app credentials;
- browser profile import/export;
- screenshots that prove a bug before and after a fix;
- PR comments, issue triage, and artifact summaries.

## Support Matrix

| Target | Managed by Crabbox | Desktop access | Primary page |
| --- | --- | --- | --- |
| Linux on Hetzner | Yes | Xvfb/XFCE/x11vnc over SSH tunnel | [Linux VNC](vnc-linux.md) |
| Linux on AWS | Yes | Xvfb/XFCE/x11vnc over SSH tunnel | [Linux VNC](vnc-linux.md) |
| Linux on Azure | Yes | Xvfb/XFCE/x11vnc over SSH tunnel | [Linux VNC](vnc-linux.md) |
| AWS Windows | Yes | TightVNC over SSH tunnel | [Windows VNC](vnc-windows.md) |
| AWS EC2 Mac | Yes | Screen Sharing/VNC over SSH tunnel | [macOS VNC](vnc-macos.md) |
| Azure Windows | No | SSH/sync/run only | [Azure](azure.md) |
| Static Linux | Host-managed | Existing loopback VNC service | [Linux VNC](vnc-linux.md) |
| Static macOS | Host-managed | Existing Screen Sharing/VNC | [macOS VNC](vnc-macos.md) |
| Static Windows | Host-managed | Existing VNC service | [Windows VNC](vnc-windows.md) |
| Blacksmith Testbox | No | Not exposed through Crabbox VNC today | [Blacksmith Testbox](blacksmith-testbox.md) |

## Commands

Use `crabbox webvnc` for the authenticated coordinator portal. This is the
preferred path for human demos because `--open` preloads the VNC password in
the local browser fragment:

```sh
crabbox webvnc --id blue-lobster --open
crabbox webvnc status --id blue-lobster
crabbox webvnc reset --id blue-lobster --open
```

Use `crabbox vnc` for a native VNC client when WebVNC status/reset says the
portal/browser path is unhealthy or when you need a native client feature:

```sh
crabbox vnc --id blue-lobster
crabbox vnc --id blue-lobster --network tailscale
crabbox vnc --id blue-lobster --open
```

WebVNC uses the same runner-side VNC service as `crabbox vnc`. The difference
is the viewer path: a local `crabbox webvnc` process keeps an SSH tunnel open,
connects to the coordinator with a one-use bridge ticket, and the browser uses
bundled noVNC from the authenticated portal. The portal does not connect to the
runner by itself; the local bridge must keep running.

WebVNC supports collaborative viewing. The local bridge keeps a warm pool of
backend VNC sessions (default 4 slots), the first browser viewer controls the
lease, and additional viewers join as read-only observers. Any viewer — a new
observer or the prior controller — can press **take over** to become the
controller; whoever loses control stays connected as an observer and sees who
took over. Observer mode is intended for trusted shared leases; it is not a
hostile-client security boundary.

The portal toolbar supports explicit clipboard exchange. Paste reads the local
browser clipboard, forwards it to the remote VNC server, and sends the target
paste shortcut. Copy-remote is enabled after the remote server publishes
clipboard text and then writes that text to the local browser clipboard on
click; browsers generally block fully automatic clipboard writes without a user
gesture.

Use `crabbox screenshot` when you need a PNG without taking over the session:

```sh
crabbox screenshot --id blue-lobster --output desktop.png
```

Use `crabbox artifacts` when QA needs a durable proof bundle instead of a
single screenshot:

```sh
crabbox artifacts collect --id blue-lobster --all --output artifacts/blue-lobster
crabbox artifacts publish --dir artifacts/blue-lobster --pr 123 --storage s3 --bucket qa-artifacts
```

Use `crabbox desktop launch` to start a browser or app inside the visible
session without keeping the SSH command attached:

```sh
crabbox desktop launch --id blue-lobster --browser --url https://example.com --webvnc --open
```

For human demos, Crabbox keeps launched browsers windowed so the remote desktop
panel, title bar, and surrounding session remain visible. Use
`desktop launch --fullscreen` only when you intentionally want browser-only
video or capture output.

Use `crabbox desktop doctor --id <lease>` before blaming WebVNC. It checks the
lease's desktop session, VNC service, input tooling, browser binary, ffmpeg,
screen geometry, and screenshot capture, then separately reports WebVNC
bridge/viewer status with one-line repair suggestions.

Failure output is designed for rescue-first debugging. When a desktop command
cannot prove the expected state, Crabbox prints the failed layer as
`problem: browser not launched`, `problem: input stack dead`, `problem: VNC
bridge disconnected`, `problem: WebVNC daemon not running`, or similar, followed
by an exact `rescue:` command. WebVNC status/reset also prints the exact native
`crabbox vnc ... --open` fallback when the native viewer is the better next
step.

Use first-class input helpers instead of hand-rolled `xdotool`:

```sh
crabbox desktop click --id blue-lobster --x 640 --y 420
crabbox desktop paste --id blue-lobster --text "peter@example.com"
printf 'peter@example.com' | crabbox desktop paste --id blue-lobster
crabbox desktop type --id blue-lobster --text "hello"
crabbox desktop key --id blue-lobster ctrl+l
crabbox desktop key blue-lobster ctrl+l
```

Prefer `desktop paste` or symbol-aware `desktop type` for emails, passwords,
URLs, and text containing characters such as `@` or `+`; raw key-symbol typing
can vary with the target X keyboard layout. `desktop key` is for shortcuts and
special keys, and supports both `--id <lease> <keys>` and positional
`<lease> <keys>` forms.

## Network Model

Managed VNC is tunnel-first:

- VNC binds to `127.0.0.1:5900` on the target.
- The cloud firewall/security group opens SSH only, not VNC.
- `crabbox vnc` forwards a local port such as `localhost:5901` to remote
  `127.0.0.1:5900`.
- `--network tailscale` changes only the SSH endpoint used by that tunnel.
- WebVNC keeps the same local SSH tunnel and adds an authenticated browser
  websocket through the coordinator.
- WebVNC browser websockets are paired with local bridge backend sessions
  inside the coordinator Durable Object. One viewer is the controller; other
  viewers are observers until they press **take over**. If a browser view
  disconnects, only its paired backend session is reset and the local command
  reconnects a fresh bridge slot for the next portal retry.
- `crabbox webvnc status` reports the local daemon pid/log, SSH tunnel command,
  target VNC reachability, coordinator bridge/viewer state, recent bridge
  events, portal URL/password, and the exact native `crabbox vnc ... --open`
  fallback. The fallback preserves explicit `--network public` or
  `--network tailscale` selections.
- `crabbox webvnc reset` closes only the selected lease's WebVNC sockets,
  stops only that lease's verified local WebVNC daemon, restarts the target
  desktop/VNC services, then prints the fresh portal URL.
- WebVNC and desktop commands print rescue commands inline when the bridge,
  viewer, browser launch, VNC target, or input stack fails, so operators do not
  need to dig through troubleshooting docs during a demo.

Crabbox does not bind managed VNC directly to a public IP or Tailscale 100.x
address. Static hosts can expose direct `host:5900` only when the operator has
already made that endpoint reachable on a trusted network.

## Browser State

`--browser` guarantees a browser binary and env such as `BROWSER` and
`CHROME_BIN`; it does not create, unlock, sync, or migrate a logged-in profile.
On managed Linux leases, these env vars point to a Crabbox wrapper that disables
Chrome/Chromium first-run and default-browser prompts for repeatable VNC use.
On managed targets, manual browser login through VNC lasts only for that lease
unless the caller intentionally exports an artifact. On static hosts, any
existing browser profile belongs to that host.

For repeatable logged-in tests, use scenario-owned state such as a Playwright
storage-state file or an app-specific short-lived token. Avoid syncing full
browser profile directories between operating systems; browser credentials are
often machine- and user-encrypted.

## Security Rules

- Never expose managed VNC directly to the public internet.
- Do not expose managed VNC directly on a Tailscale interface.
- Prefer SSH local forwarding such as
  `localhost:5901 -> 127.0.0.1:5900`.
- Generate per-lease passwords for managed desktop leases.
- Redact passwords from logs, provider metadata, and run records.
- Keep TTL and idle-timeout cleanup in force.
- Require `--host-managed` before opening static-host VNC prompts.

## Where To Go Next

- [Linux VNC](vnc-linux.md): Hetzner/AWS/Azure Linux desktop services and static Linux.
- [Windows VNC](vnc-windows.md): AWS Windows, native Windows static hosts, and WSL2 boundaries.
- [macOS VNC](vnc-macos.md): AWS EC2 Mac and static Mac Screen Sharing.
- [AWS](aws.md): AWS target matrix, capacity, AMIs, and EC2 Mac host requirements.
- [Hetzner](hetzner.md): Linux-only managed Hetzner behavior.
- [Blacksmith Testbox](blacksmith-testbox.md): delegated Testbox behavior and why VNC is not a Crabbox feature there yet.
- [vnc command](../commands/vnc.md), [webvnc command](../commands/webvnc.md), [screenshot command](../commands/screenshot.md), [desktop command](../commands/desktop.md), [artifacts command](../commands/artifacts.md), [egress command](../commands/egress.md).
- [Mediated egress](egress.md): per-app browser/app egress through the operator
  machine for Discord, Slack, and similar source-IP-sensitive QA.
