# Interactive Desktop And VNC: Design Plan And Status

Read when:

- understanding why desktop/VNC is modeled as a lease capability rather than a
  separate remote-access product;
- reviewing the security boundary for desktop takeover;
- deciding what interactive-desktop work is still open versus already shipped.

This is the original design plan for the interactive-desktop vertical slice.
Most of it has shipped; this document is kept as a retrospective and as the home
for the small set of forward-looking items that remain. For the current,
authoritative behavior, see the feature and command docs linked throughout. When
this plan and the shipped code disagree, the code and the feature docs win.

## Goal

Let a caller request a UI-capable box, run browser/desktop automation in a
visible session, and take over interactively through a tunnel — without turning
Crabbox into a bespoke remote-desktop product.

The design keeps a clean split:

- **Crabbox owns machine capability**: lease lifecycle, TTL, idle touch,
  cleanup, claims, provider-specific bootstrap and SSH connection details,
  desktop services, browser installation/probing, connection metadata, and
  tunnel-only or portal-mediated access.
- **The caller owns scenario logic**: app credentials, browser profiles,
  automation scripts, assertions, screenshots, and pass/fail reporting.

## Capability Model (shipped)

Interactive desktop is exposed as three opt-in lease capabilities, requested at
warm time and validated against the provider's declared feature set. See
[capabilities](../features/capabilities.md).

```sh
crabbox warmup --desktop
crabbox warmup --desktop --browser
crabbox warmup --desktop --code
crabbox run --desktop --browser -- <command...>
```

- `--desktop` exposes a visible session with a loopback-bound VNC server. The
  Linux desktop environment is selectable with `--desktop-env xfce|wayland|gnome`
  (default `xfce`).
- `--browser` installs a known browser binary (Chrome/Chromium) and exports it
  via `$BROWSER` and `$CHROME_BIN`.
- `--code` provisions code-server on a loopback port for the portal/code bridge
  (managed Linux only).

For `run`, `--browser` does not imply `--desktop`: it supports headless browser
automation on a box that merely has a known browser binary. Use
`--desktop --browser` only when the browser should run in the visible session.

Capabilities are stored on the lease and mirrored to provider labels/tags
(`desktop=true`, `browser=true`, `code=true`, `desktop_env=<env>`). Reusing a
lease with `--id` requires the matching labels to be present;
`enforceManagedLeaseCapabilities` refuses a capability the lease was not warmed
with and hints to warm a new lease. Validation lives in
`internal/cli/capabilities.go`.

## Access Surface (shipped)

The plan originally specified a single tunnel-only `crabbox vnc`. The shipped
surface is broader:

- `crabbox vnc --id <lease-or-slug>` — resolve the lease, claim/touch it, and
  print (or `--open`) an SSH-tunneled VNC connection. Source
  `internal/cli/vnc.go`; per-OS detail in
  [vnc-linux](../features/vnc-linux.md), [vnc-macos](../features/vnc-macos.md),
  and [vnc-windows](../features/vnc-windows.md).
- `crabbox webvnc --id <lease> [--open]` — bridge the desktop into the
  authenticated [portal](../features/portal.md) over a WebSocket relay, with
  `status`/`reset`/`daemon` subcommands and `--take-control`. Source
  `internal/cli/webvnc.go`.
- `crabbox code --id <lease> [--open]` — bridge a `--code` lease's code-server
  into the portal editor.
- `crabbox screenshot --id <lease> --output <file>` — capture a PNG of the
  visible session.
- `crabbox desktop …` — launch apps, drive input (`click`/`type`/`paste`/`key`),
  open a visible `terminal`, `record` video, collect `proof`, and run `doctor`
  on session readiness. Source `internal/cli/desktop*.go`.
- `crabbox media` and `crabbox artifacts` — turn recorded desktop video into
  trimmed GIFs/MP4s and publishable QA bundles.

Example `crabbox vnc` output (managed Linux):

```text
lease: cbx_0a1b2c3d4e5f slug=blue-lobster provider=aws target=linux
managed: true
display: :99
ssh tunnel:
  ssh -i ... -p 2222 -N -o GatewayPorts=no -L 127.0.0.1:5901:127.0.0.1:5900 crabbox@203.0.113.10
vnc:
  localhost:5901

Keep the tunnel process running while connected.
```

## Security Boundary (shipped, unchanged)

These are hard requirements and remain enforced:

- VNC/noVNC is never exposed to the public internet. The runner-side VNC server
  binds to `127.0.0.1`; the portal WebVNC bridge runs websockify and noVNC on
  an owner-specific, collision-checked loopback port allocated under a remote
  lock and targeting `127.0.0.1:5900`; the selected port is persisted in the
  owner's private identity and reached through the authenticated portal, not a
  public port.
- No provider firewall/security-group ingress is added for VNC. SSH remains the
  only public ingress for direct/brokered cloud leases.
- VNC passwords are not placed in command-line arguments, provider labels, run
  history, or logs. A per-lease password file is generated on the box
  (`/var/lib/crabbox/vnc.password` on Linux, platform equivalents on
  macOS/Windows) and retrieved over SSH only when needed.
- TTL, idle-timeout, and cleanup behavior are unchanged: cleanup is VM deletion
  for managed providers and a no-op for static hosts.

Linux uses loopback-bound x11vnc with `rfbauth`:

```text
x11vnc -display :99 -localhost -rfbport 5900 -forever -shared -rfbauth /var/lib/crabbox/vnc.pass
```

## Managed Linux Bootstrap (shipped)

The default bootstrap stays tiny; desktop/browser/code packages are installed
only when the matching capability is requested. The XFCE path installs a
lightweight visible-session stack (Xvfb, an XFCE session, x11vnc, fonts, dbus),
runs it under systemd units (`crabbox-xvfb`, `crabbox-desktop`,
`crabbox-x11vnc`), and gates readiness checks behind `desktop=true` so plain
leases never run them. `--browser` installs Chrome stable (Chromium fallback)
and records the discovered binary path; `--code` installs code-server. Wayland
and GNOME variants follow the same shape with their own units. Source
`internal/cli/bootstrap.go`; see [runner-bootstrap](../features/runner-bootstrap.md).

`run --desktop` injects `DISPLAY=:99` and `CRABBOX_DESKTOP=1`; with a known
browser it also exports `CRABBOX_BROWSER=1`, `CHROME_BIN`, and `BROWSER`. These
are static machine metadata and merge with the existing allowed-env and Actions
env-file behavior. See [env-forwarding](../features/env-forwarding.md).

## Provider Behavior (shipped)

- **Managed Linux (Hetzner, AWS, Azure)** support `--desktop`, `--browser`, and
  `--code` via the optional cloud-init bootstrap blocks. Brokered leases carry
  the capability booleans through the Worker; direct leases carry them on
  provider labels so an existing lease's capabilities can be detected.
- **AWS macOS / EC2 Mac** enables Screen Sharing for the runner user, sets a
  generated per-lease password, and serves the same `crabbox vnc`/`webvnc` flow
  over loopback `5900`. See [vnc-macos](../features/vnc-macos.md).
- **AWS Windows (normal mode)** supports managed VNC; `--windows-mode wsl2` does
  not. See [vnc-windows](../features/vnc-windows.md).
- **Static hosts** (`provider=ssh`) participate when the operator-managed
  services already exist; Crabbox probes rather than installs and prints clear
  guidance when prerequisites are missing. `crabbox vnc --open` on a static host
  requires `--host-managed` to acknowledge it is an existing host's OS session,
  not a Crabbox-created box.
- **Blacksmith Testbox** has no desktop/VNC capability; the command fails
  clearly because Blacksmith owns machine connectivity.

## What Remains Open

Nearly all of the original plan shipped, including items it had deferred —
noVNC/websockify (now the WebVNC portal bridge), macOS Screen Sharing
enablement, and scenario screenshots/video/artifacts. The remaining
forward-looking work is narrower:

- **Automatic Windows managed VNC/RDP** beyond the normal-mode path, and a
  first-class WSL2 desktop story.
- **Browser profile lifecycle management** (persisting/seeding profiles across
  leases) — still owned by the caller today.
- **Blacksmith and other delegated-run providers**: interactive desktop remains
  out of scope while those providers own their own connectivity.

Anything not listed here should be treated as shipped; verify against the linked
feature docs and the source files in `internal/cli/` before extending it.
