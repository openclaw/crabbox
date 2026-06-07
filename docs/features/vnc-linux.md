# Linux VNC

Read when:

- using `--desktop` on a managed Linux lease (Hetzner, AWS, or Azure);
- choosing a desktop environment with `--desktop-env` (xfce, wayland, gnome);
- debugging Xvfb, the window-manager session, x11vnc/WayVNC, or screenshots on a
  Linux lease;
- preparing a static (BYO) Linux host to serve a Crabbox VNC desktop.

Linux is the simplest managed desktop path. A managed Linux lease bootstraps a
lightweight desktop, runs it on `DISPLAY=:99`, binds a VNC server to loopback,
and lets the CLI tunnel into it over SSH.

## Managed Linux

```sh
crabbox warmup --desktop --browser
crabbox run --id swift-crab --desktop --browser -- google-chrome --version
crabbox desktop doctor --id swift-crab
crabbox webvnc --id swift-crab --open
crabbox vnc --id swift-crab --open
crabbox screenshot --id swift-crab --output linux.png
```

The default desktop environment is XFCE. Request a different one at warm time:

```sh
crabbox warmup --desktop --desktop-env wayland
crabbox warmup --desktop --desktop-env gnome
```

A managed Linux desktop lease provides:

- Xvfb on `:99` (XFCE) or a Wayland compositor (wayland/gnome);
- a window-manager / desktop session (`xfce4-session` with `xfwm4`,
  `xfce4-panel`, and `xfce4-terminal` for XFCE);
- x11vnc (XFCE) or WayVNC (wayland/gnome) bound to `127.0.0.1:5900`;
- screenshot and video capture tools (`scrot` and `ffmpeg`);
- input helpers (`xdotool`, `wmctrl`) and clipboard tools (`xclip`/`xsel`);
- a generated per-lease VNC password at `/var/lib/crabbox/vnc.password`;
- the resolved desktop environment recorded in `/var/lib/crabbox/desktop.env`
  (`CRABBOX_DESKTOP_ENV`, `DISPLAY`, and Wayland variables where applicable);
- optional Chrome stable (Chromium fallback) with first-run suppression and a
  managed policy when `--browser` is requested;
- readiness checks that verify the desktop services when the lease carries
  `desktop=true`.

Reusing a lease requires matching capability labels: a lease warmed without
`--desktop` (or with a different `--desktop-env`) cannot gain the desktop after
creation.

### Injected environment

`crabbox run --desktop` resolves the recorded desktop environment, then:

- always sets `CRABBOX_DESKTOP=1`;
- for XFCE, sets `DISPLAY=:99`;
- for wayland/gnome, sources `/var/lib/crabbox/desktop.env` and forwards
  `XDG_RUNTIME_DIR`, `WAYLAND_DISPLAY`, and related variables.

`crabbox run --browser` probes the target, then sets `CRABBOX_BROWSER=1` plus
`BROWSER` and `CHROME_BIN` (pointing at the resolved Chrome/Chromium wrapper).

## Static Linux

Static Linux (`provider: ssh`) is host-managed. Crabbox does not install
packages or start a desktop service on a preexisting machine. The host must
already serve a VNC server reachable from SSH loopback:

```yaml
provider: ssh
target: linux
static:
  host: linux-box.tailnet-name.ts.net
  user: crabbox
  port: "22"
  workRoot: /home/crabbox/work
```

```sh
crabbox vnc --provider ssh --target linux --static-host linux-box.tailnet-name.ts.net
```

Keep x11vnc (or another VNC server) bound to `127.0.0.1:5900`. A direct
`host:5900` connection is accepted only when reachable and should be limited to
a trusted LAN or tailnet.

## Troubleshooting

`lease ... was not created with desktop=true`

Warm a new lease with `--desktop`; existing leases do not gain the capability
after creation.

`lease ... was not created with desktopEnv=<env>`

The lease was warmed with a different desktop environment. Warm a fresh lease
with the matching `--desktop-env`.

`target=linux does not expose a loopback X11 VNC desktop`

For managed leases, inspect cloud-init and service logs or warm a fresh box.
For static hosts, start Xvfb/x11vnc on `127.0.0.1:5900`, or warm with
`--desktop-env wayland` or `--desktop-env gnome` for a configured Wayland
target.

`target=linux does not expose a Crabbox Wayland desktop`

Create `/var/lib/crabbox/desktop.env` with `CRABBOX_DESKTOP_ENV=wayland` (or
`gnome`), `XDG_RUNTIME_DIR`, and `WAYLAND_DISPLAY`, then start the compositor and
WayVNC on `127.0.0.1:5900`.

Black screen

Confirm the app launched into the desktop session. For detached browser work,
use:

```sh
crabbox desktop launch --id swift-crab --browser --url https://example.com
```

Run `crabbox desktop doctor --id swift-crab` to separate session problems from
WebVNC/browser-portal problems; it reports a specific repair line per missing
component (window manager, panel, VNC server, clipboard tools, browser, ffmpeg,
screen size, or screenshot capture).

Input symbols are wrong

Use Crabbox's desktop helpers instead of raw `xdotool type`:

```sh
crabbox desktop paste --id swift-crab --text "alice+qa@example.com"
crabbox desktop type --id swift-crab --text "alice+qa@example.com"
```

`desktop type` uses clipboard paste for symbol-heavy text, so `@`, `+`,
password-like values, and URLs do not depend on the target X keyboard layout.

Related docs:

- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [Capabilities](capabilities.md)
- [Hetzner](hetzner.md)
- [AWS](aws.md)
- [Azure](azure.md)
- [vnc command](../commands/vnc.md)
- [webvnc command](../commands/webvnc.md)
- [desktop command](../commands/desktop.md)
- [screenshot command](../commands/screenshot.md)
