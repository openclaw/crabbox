# Interactive Desktop And VNC

Read when:

- choosing a desktop target for browser/UI QA;
- opening a lease with VNC or WebVNC;
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
crabbox vnc --id blue-lobster --open
crabbox webvnc --id blue-lobster --open
crabbox screenshot --id blue-lobster --output desktop.png
crabbox record --id blue-lobster --duration 10s --output desktop.mp4
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
| AWS Windows | Yes | TightVNC over SSH tunnel | [Windows VNC](vnc-windows.md) |
| AWS EC2 Mac | Yes | Screen Sharing/VNC over SSH tunnel | [macOS VNC](vnc-macos.md) |
| Static Linux | Host-managed | Existing loopback VNC service | [Linux VNC](vnc-linux.md) |
| Static macOS | Host-managed | Existing Screen Sharing/VNC | [macOS VNC](vnc-macos.md) |
| Static Windows | Host-managed | Existing VNC service | [Windows VNC](vnc-windows.md) |
| Blacksmith Testbox | No | Not exposed through Crabbox VNC today | [Blacksmith Testbox](blacksmith-testbox.md) |

## Commands

Use `crabbox vnc` for a native VNC client:

```sh
crabbox vnc --id blue-lobster
crabbox vnc --id blue-lobster --network tailscale
crabbox vnc --id blue-lobster --open
```

Use `crabbox webvnc` for the authenticated coordinator portal:

```sh
crabbox webvnc --id blue-lobster --open
```

WebVNC uses the same runner-side VNC service as `crabbox vnc`. The difference
is the viewer path: a local `crabbox webvnc` process keeps an SSH tunnel open,
connects to the coordinator with a one-use bridge ticket, and the browser uses
bundled noVNC from the authenticated portal. The portal does not connect to the
runner by itself; the local bridge must keep running.

Use `crabbox screenshot` when you need a PNG without taking over the session:

```sh
crabbox screenshot --id blue-lobster --output desktop.png
```

Use `crabbox record` when a temporal UI bug needs video evidence:

```sh
crabbox record --id blue-lobster --duration 10s --output desktop.mp4
crabbox record --id blue-lobster --duration 2m --output task.mp4 --while -- ./drive-ui.sh
```

The `--while` form records while a local driver command controls the desktop.
Drivers can be deterministic scripts, Playwright/CDP flows, VNC/xdotool
automation, or an agent wrapper. Crabbox injects `CRABBOX_RECORD_LEASE_ID` and
`CRABBOX_RECORD_PROVIDER` into the driver environment. `--duration` is the hard
cap for the local driver; the recording can include a small margin around it.

Use `crabbox desktop launch` to start a browser or app inside the visible
session without keeping the SSH command attached:

```sh
crabbox desktop launch --id blue-lobster --browser --url https://example.com
```

## Network Model

Managed VNC is tunnel-first:

- VNC binds to `127.0.0.1:5900` on the target.
- The cloud firewall/security group opens SSH only, not VNC.
- `crabbox vnc` forwards a local port such as `localhost:5901` to remote
  `127.0.0.1:5900`.
- `--network tailscale` changes only the SSH endpoint used by that tunnel.
- WebVNC keeps the same local SSH tunnel and adds an authenticated browser
  websocket through the coordinator.
- The WebVNC browser websocket is paired with the local bridge process inside
  the coordinator Durable Object; if the browser view disconnects, the local
  command reconnects a fresh bridge for the portal retry. If the local process
  exits, the browser view disconnects until you start it again.

Crabbox does not bind managed VNC directly to a public IP or Tailscale 100.x
address. Static hosts can expose direct `host:5900` only when the operator has
already made that endpoint reachable on a trusted network.

## Browser State

`--browser` guarantees a browser binary and env such as `BROWSER` and
`CHROME_BIN`; it does not create, unlock, sync, or migrate a logged-in profile.
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

- [Linux VNC](vnc-linux.md): Hetzner/AWS Linux desktop services and static Linux.
- [Windows VNC](vnc-windows.md): AWS Windows, native Windows static hosts, and WSL2 boundaries.
- [macOS VNC](vnc-macos.md): AWS EC2 Mac and static Mac Screen Sharing.
- [AWS](aws.md): AWS target matrix, capacity, AMIs, and EC2 Mac host requirements.
- [Hetzner](hetzner.md): Linux-only managed Hetzner behavior.
- [Blacksmith Testbox](blacksmith-testbox.md): delegated Testbox behavior and why VNC is not a Crabbox feature there yet.
- [vnc command](../commands/vnc.md), [webvnc command](../commands/webvnc.md), [screenshot command](../commands/screenshot.md), [record command](../commands/record.md), [desktop command](../commands/desktop.md).
