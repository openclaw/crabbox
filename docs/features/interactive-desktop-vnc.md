# Interactive Desktop And VNC

Read when:

- choosing a desktop target for browser or UI testing;
- opening a lease with native VNC or the web portal (WebVNC);
- diagnosing stale WebVNC viewers, bridge disconnects, or broken desktop
  sessions;
- driving desktop input from automation without hand-written `xdotool`;
- deciding which layer owns desktop setup, browser state, screenshots, or
  credentials.

Crabbox treats desktop access as a lease capability, not a separate remote
access product. A desktop lease keeps the normal Crabbox boundaries: provider
lifecycle, per-lease SSH keys, SSH tunnels, idle expiry, cleanup, and run
history. VNC is one way to inspect or drive the visible session inside that
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

AWS Windows, EC2 Mac, and External Mac leases use the same `vnc` command once
the desktop lease exists:

```sh
crabbox warmup --provider aws --target windows --desktop
crabbox vnc --id crimson-crab --open

crabbox warmup --provider aws --target macos --desktop --market on-demand
crabbox vnc --id silver-squid --open
```

For External macOS, first configure the adapter's trusted target, desktop
account, and password environment reference, then validate the real
Screen Sharing credential path with `crabbox webvnc --provider external
--target macos --id <lease> --preflight`. See the
[External provider](../providers/external.md) and [macOS VNC](vnc-macos.md)
docs.

Static hosts are existing machines, so VNC against them is explicit and
host-managed:

```sh
crabbox vnc --provider ssh --target macos --static-host mac-studio.local --host-managed --open
crabbox vnc --provider ssh --target windows --static-host win-dev.local --host-managed --open
```

## What Crabbox Owns

Crabbox owns:

- lease lifecycle and cleanup orchestration through the selected provider or
  External adapter;
- per-lease SSH keys and `known_hosts` scoping;
- SSH local forwarding to the target's loopback VNC service;
- generated per-lease VNC or OS passwords for Crabbox-bootstrapped desktops,
  plus protected consumption of operator-managed External macOS credentials;
- `desktop=true`, `browser=true`, `code=true`, and `desktop_env` lease labels;
- screenshots, video, and launch/input commands that run inside the lease.

A scenario layer on top of Crabbox owns:

- product-specific login and app credentials;
- browser profile import and export;
- before/after screenshots that prove a bug and its fix;
- PR comments, issue triage, and artifact summaries.

## Support Matrix

| Target | Managed by Crabbox | Desktop access | Primary page |
| --- | --- | --- | --- |
| Linux on Hetzner | Yes | TigerVNC/XFCE, or Wayland + WayVNC, over SSH tunnel | [Linux VNC](vnc-linux.md) |
| Linux on AWS | Yes | TigerVNC/XFCE, or Wayland + WayVNC, over SSH tunnel | [Linux VNC](vnc-linux.md) |
| Linux on Azure | Yes | TigerVNC/XFCE, or Wayland + WayVNC, over SSH tunnel | [Linux VNC](vnc-linux.md) |
| AWS Windows | Yes | VNC service over SSH tunnel | [Windows VNC](vnc-windows.md) |
| Azure Windows | Yes | VNC service over SSH tunnel | [Windows VNC](vnc-windows.md) |
| AWS EC2 Mac | Yes | Screen Sharing/VNC over SSH tunnel | [macOS VNC](vnc-macos.md) |
| External macOS | External adapter | Screen Sharing/ARD over SSH tunnel with an operator-managed account | [macOS VNC](vnc-macos.md) |
| External Windows | External adapter | SSH/run by default; VNC only when the adapter supplies Crabbox-compatible desktop capability, service, and credential file | [External provider](../providers/external.md) |
| Local Docker container | Yes | Loopback VNC over SSH tunnel | [Linux VNC](vnc-linux.md) |
| Static Linux | Host-managed | Existing loopback VNC service | [Linux VNC](vnc-linux.md) |
| Static macOS | Host-managed | Existing Screen Sharing/VNC | [macOS VNC](vnc-macos.md) |
| Static Windows | Host-managed | Existing VNC service | [Windows VNC](vnc-windows.md) |
| Blacksmith Testbox | No | Not exposed through Crabbox VNC | [Blacksmith Testbox](blacksmith-testbox.md) |

Desktop capabilities require a provider that advertises the `desktop` feature
(`crabbox providers` lists support). Blacksmith and other connectivity-owning
providers reject the desktop, VNC, screenshot, and input commands.

## Choosing A Viewer

Two viewers share the same runner-side VNC service; they differ only in the
local-to-portal path.

- `crabbox webvnc` is the default for human demos and shared sessions. A local
  `crabbox webvnc` process keeps the SSH tunnel open, connects to the
  coordinator with a one-use bridge ticket, and the authenticated portal serves
  bundled noVNC. The portal never connects to the runner directly, so the local
  bridge must keep running. `--open` gives the browser a one-use credential
  handoff ticket; usernames and passwords never enter the URL.
- `crabbox vnc` is for a native VNC client: when WebVNC status or reset reports
  the portal path is unhealthy, or when you need a native client feature.

```sh
crabbox webvnc --id blue-lobster --open --take-control
crabbox webvnc status --id blue-lobster
crabbox webvnc reset --id blue-lobster --open --take-control

crabbox vnc --id blue-lobster
crabbox vnc --id blue-lobster --network tailscale
crabbox vnc --id blue-lobster --open
```

WebVNC is available on coordinator-managed desktop leases and on direct desktop
providers. Authenticated direct macOS providers such as Tart and Parallels are
registered automatically for the provider lease lifetime, so they use the same portal
chrome and controls as Linux and Windows. Other direct providers can opt into
the outbound coordinator bridge with `broker.mode: registered`; without that
mode they keep their localhost viewer. Blacksmith remains unsupported. Native
`crabbox vnc` works against every managed and host-managed desktop in the
support matrix.

External macOS uses the direct Screen Sharing bridge too. Crabbox resolves the
operator-managed account password from its approved environment reference,
authenticates the ARD session in the local relay, and never sends that account
credential to the browser. Because the coordinator authenticates portal access,
the toolbar copies a clean, ticket-free URL for these relay-authenticated
sessions; access remains subject to the lease ACL. Use `webvnc --preflight` to
test the exact SSH, RFB, and ARD authentication path without opening a viewer.

For kept registered desktop leases, `broker.autoWebVNC: true` starts the bridge
daemon automatically. The daemon heartbeats the registration while connected;
`crabbox stop` stops it and removes the registration after provider cleanup.
The portal defaults to the viewer's system appearance and sends the resolved
light or dark theme through that bridge on connect and whenever the operating
system appearance changes.

### Collaborative WebVNC

The local bridge keeps a warm pool of backend VNC sessions (default 4 slots).
The first browser viewer becomes the controller; later viewers join as
read-only observers. Any viewer — a new observer or the previous controller —
can press **take over** to become the controller; whoever loses control stays
connected as an observer and sees who took over. Pass `--take-control` so the
opened browser requests control immediately, which is useful for handoffs.
Observer mode is meant for trusted shared leases; it is not a hostile-client
security boundary.

The portal toolbar exchanges the clipboard explicitly. Paste reads the local
browser clipboard, forwards it to the remote VNC server, and sends the target
paste shortcut. Copy-remote becomes available once the remote server publishes
clipboard text, then writes it to the local browser clipboard on click;
browsers generally block fully automatic clipboard writes without a user
gesture.

## Capturing Without Taking Over

Use `crabbox screenshot` for a PNG without attaching to the session:

```sh
crabbox screenshot --id blue-lobster --output desktop.png
```

Screenshot capture is loopback-based: Linux uses `grim` (Wayland) or
`scrot`/`import` (X11), managed macOS captures the Screen Sharing framebuffer
over RFB, and native Windows uses an interactive screen capture task. External
macOS uses its approved operator credential for ARD authentication on that RFB
connection. Static hosts only support screenshots on Linux targets, since
macOS and Windows static hosts are existing machines.

For a durable proof bundle instead of a single image, use `crabbox artifacts`:

```sh
crabbox artifacts collect --id blue-lobster --all --output artifacts/blue-lobster
crabbox artifacts publish --dir artifacts/blue-lobster --pr 123 --storage s3 --bucket qa-artifacts
```

See the [artifacts feature doc](artifacts.md) and the
[artifacts command reference](../commands/artifacts.md) for bundle contents and
publish targets.

## Launching Apps And Terminals

`crabbox desktop launch` starts a browser or app inside the visible session
without keeping the SSH command attached:

```sh
crabbox desktop launch --id blue-lobster --browser --url https://example.com --webvnc --open --take-control
```

For human demos, launched browsers stay windowed so the desktop panel, title
bar, and surrounding session remain visible. Use `--fullscreen` only when you
want browser-only video or capture output. `--webvnc` (and its `--open` /
`--take-control` companions) bridges the launched desktop into the portal.
Authenticated Tart, Parallels, and External macOS leases use the same direct
Screen Sharing bridge; when registered, they use the same portal as
coordinator-managed leases. The local container provider uses local noVNC over
SSH. `--egress` routes the launched browser
through the lease-local egress proxy (default
`127.0.0.1:3128`) and currently requires `--browser`; see
[mediated egress](egress.md).

`crabbox desktop terminal` starts a visible terminal and can capture a
screenshot or record video after a short visibility delay:

```sh
crabbox desktop terminal --id blue-lobster --cols 120 --rows 40 -- npm test
crabbox desktop terminal --id blue-lobster --record run.mp4 --record-duration 8s
```

`crabbox desktop proof` launches a terminal and collects a full proof directory
(metadata, screenshot, diagnostics, video, contact sheet), with optional
publish flags. `crabbox desktop record` records desktop video (an alias for
`artifacts video`). Video capture currently requires an X11 Linux desktop or a
native Windows desktop; Wayland desktop envs are rejected for video.

## Desktop Doctor

Run `crabbox desktop doctor --id <lease>` before blaming WebVNC. On Linux it
checks the desktop session, VNC service, input tooling, browser binary, ffmpeg,
screen geometry, and screenshot capture, layer by layer, then separately reports
the portal's WebVNC bridge and viewer status with one-line repair suggestions.

Desktop and WebVNC commands are built for rescue-first debugging. When a command
cannot prove the expected state it prints the failing layer (for example
`problem: browser not launched`, `problem: input stack dead`, `problem: VNC
bridge disconnected`, `problem: WebVNC daemon not running`) followed by an exact
`rescue:` command. `webvnc status` and `webvnc reset` also print the exact
native `crabbox vnc ... --open` fallback when the native viewer is the better
next step, preserving any explicit `--network public` or `--network tailscale`
selection.

## Driving Input

Use the first-class input helpers instead of hand-rolled `xdotool`:

```sh
crabbox desktop click --id blue-lobster --x 640 --y 420
crabbox desktop paste --id blue-lobster --text "alice@example.com"
printf 'alice@example.com' | crabbox desktop paste --id blue-lobster
crabbox desktop type --id blue-lobster --text "hello"
crabbox desktop key --id blue-lobster ctrl+l
crabbox desktop key blue-lobster ctrl+l
```

- `desktop click` works on managed Linux, macOS, and native Windows targets.
- Prefer `desktop paste` or `desktop type` for emails, passwords, URLs, and any
  text with characters such as `@` or `+`; raw key-symbol typing can vary with
  the target keyboard layout. `desktop type` automatically falls back to a paste
  for special characters, newlines, or long text.
- `desktop key` is for shortcuts and special keys, and accepts both
  `--id <lease> <keys>` and the positional `<lease> <keys>` form.

Input helpers other than `click` currently target Linux desktops. On X11 they
use `xdotool` and `xclip`/`xsel`; on Wayland/GNOME desktop leases they use
`wtype` and `wl-clipboard`.

## Network Model

Managed VNC is tunnel-first:

- VNC binds to `127.0.0.1:5900` on the target.
- The cloud firewall or security group opens SSH only, not VNC.
- `crabbox vnc` forwards a local port (the first free port in `5901`–`5999`) to
  remote `127.0.0.1:5900`.
- `--network tailscale` changes only the SSH endpoint used by that tunnel.
- Remote-host providers such as Parallels and External carry their SSH proxy
  command into VNC, screenshots, and input commands using the same tunnel
  model.
- WebVNC keeps the same local SSH tunnel and adds an authenticated browser
  websocket through the coordinator. Browser websockets are paired with local
  bridge backend sessions inside the coordinator Durable Object: one viewer is
  the controller, the rest are observers until they take over. If a browser view
  disconnects, only its paired backend session resets, and the local command
  reconnects a fresh bridge slot for the next portal retry.
- `crabbox webvnc status` reports the local daemon pid and log, the SSH tunnel
  command, target VNC reachability, the coordinator bridge and viewer state,
  recent bridge events, the portal URL and password, and the exact native
  `crabbox vnc ... --open` fallback.
- `crabbox webvnc reset` closes only the selected lease's WebVNC sockets, stops
  only that lease's verified local WebVNC daemon, restarts the target desktop
  and VNC services, then prints a fresh portal URL.

Crabbox never binds managed VNC directly to a public IP or a Tailscale `100.x`
address. Static hosts can expose a direct `host:5900` endpoint only when the
operator has already made it reachable on a trusted network; `crabbox vnc` falls
back to that direct endpoint only when the SSH loopback service is unreachable.

### Running WebVNC As A Daemon

For long-lived sessions, run the bridge in the background:

```sh
crabbox webvnc daemon start --id blue-lobster --open
crabbox webvnc daemon status --id blue-lobster
crabbox webvnc daemon stop --id blue-lobster
```

The compatibility flags `--daemon`/`--background`, `--status`, and `--stop` map
to `daemon start`, `daemon status`, and `daemon stop` respectively.

## Browser State

`--browser` guarantees a browser binary and env such as `BROWSER` and
`CHROME_BIN`; it does not create, unlock, sync, or migrate a logged-in profile.
On managed Linux leases these env vars point to a Crabbox wrapper that disables
Chrome/Chromium first-run and default-browser prompts and pins a per-lease
profile for repeatable VNC use. Manual browser login through VNC lasts only for
that lease unless you intentionally export an artifact. On static hosts, any
existing browser profile belongs to that host.

For repeatable logged-in tests, prefer scenario-owned state such as a Playwright
storage-state file or a short-lived app token. Avoid syncing full browser
profile directories between operating systems; browser credentials are often
machine- and user-encrypted.

## Security Rules

- Never expose managed VNC directly to the public internet.
- Do not expose managed VNC directly on a Tailscale interface.
- Prefer SSH local forwarding such as `localhost:5901 -> 127.0.0.1:5900`.
- Generate per-lease passwords for Crabbox-bootstrapped desktop leases. For an
  External Mac, keep the existing account password in an operator secret store
  and configure only its approved environment-variable name.
- Redact passwords from logs, provider metadata, and run records.
- Keep TTL and idle-timeout cleanup in force.
- Require `--host-managed` before opening static-host VNC prompts.

## Where To Go Next

- [Linux VNC](vnc-linux.md): Hetzner/AWS/Azure Linux desktop services and static Linux.
- [Windows VNC](vnc-windows.md): AWS/Azure managed Windows, native Windows static hosts, and WSL2 boundaries.
- [macOS VNC](vnc-macos.md): AWS EC2 Mac, External adapters, and static Mac Screen Sharing.
- [External provider](../providers/external.md): adapter routing and operator-managed desktop credentials.
- [Capabilities](capabilities.md): how `--desktop`, `--browser`, and `--code` are requested and validated.
- [Portal](portal.md): the authenticated web UI that hosts WebVNC and Code panes.
- [Mediated egress](egress.md): per-app browser/app egress through the operator machine.
- [AWS](aws.md): AWS target matrix, capacity, AMIs, and EC2 Mac host requirements.
- [Hetzner](hetzner.md): Linux-only managed Hetzner behavior.
- [Blacksmith Testbox](blacksmith-testbox.md): delegated Testbox behavior and why VNC is not a Crabbox feature there.
- Command references: [vnc](../commands/vnc.md), [webvnc](../commands/webvnc.md), [screenshot](../commands/screenshot.md), [desktop](../commands/desktop.md), [artifacts](../commands/artifacts.md), [egress](../commands/egress.md).
