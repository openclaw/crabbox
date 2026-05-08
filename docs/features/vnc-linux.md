# Linux VNC

Read when:

- using `--desktop` on Hetzner, AWS, or Azure Linux;
- debugging Xvfb, XFCE/Openbox, x11vnc, or screenshots on a Linux lease;
- preparing a static Linux host for Crabbox VNC.

Linux is the simplest managed desktop path. Hetzner, AWS, and Azure Linux
leases use the same bootstrap shape: install a lightweight desktop, run it on
`DISPLAY=:99`, bind x11vnc to loopback, and let the CLI create an SSH tunnel.

## Managed Linux

```sh
crabbox warmup --desktop --browser
crabbox run --id blue-lobster --desktop --browser -- google-chrome --version
crabbox desktop doctor --id blue-lobster
crabbox webvnc --id blue-lobster --open
crabbox vnc --id blue-lobster --open
crabbox screenshot --id blue-lobster --output linux.png
```

Managed Linux desktop leases include:

- Xvfb on `:99`;
- a lightweight desktop/window-manager session;
- x11vnc bound to `127.0.0.1:5900`;
- screenshot and video capture tools (`scrot` and `ffmpeg`);
- input helpers (`xdotool`) and clipboard paste tools (`xclip`/`xsel`);
- a generated per-lease VNC password at `/var/lib/crabbox/vnc.password`;
- optional Chrome stable or Chromium fallback, first-run suppression, and native
  addon build helpers when `--browser` is requested;
- readiness checks that verify desktop services when `desktop=true`.

`crabbox run --desktop` injects `CRABBOX_DESKTOP=1` and `DISPLAY=:99`.
`crabbox run --browser` injects `CRABBOX_BROWSER=1`, `BROWSER`, and
`CHROME_BIN` after probing the target.

## Static Linux

Static Linux is host-managed. Crabbox does not install packages or start a
desktop service on an existing machine. The host must already provide a VNC
service reachable from SSH loopback:

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

For static Linux, keep x11vnc or another VNC server bound to
`127.0.0.1:5900`. Direct `host:5900` is accepted only when reachable and should
be limited to a trusted LAN or tailnet.

## Troubleshooting

`lease ... was not created with desktop=true`

Warm a new lease with `--desktop`; existing leases do not gain the capability
after creation.

`target=linux does not expose a loopback VNC desktop`

For managed leases, inspect cloud-init and service logs or warm a fresh box.
For static hosts, start Xvfb/desktop services and x11vnc on
`127.0.0.1:5900`.

Black screen

Check that the app was launched into `DISPLAY=:99`. For detached browser work,
use:

```sh
crabbox desktop launch --id blue-lobster --browser --url https://example.com
```

Run `crabbox desktop doctor --id blue-lobster` to separate session problems
from WebVNC/browser-portal problems. Missing `xfwm4`, `xfce4-panel`, x11vnc,
clipboard tools, browser, ffmpeg, screen size, or screenshot capture each get a
specific repair line.

Input symbols are wrong

Use Crabbox's desktop helpers instead of raw `xdotool type`:

```sh
crabbox desktop paste --id blue-lobster --text "peter+qa@example.com"
crabbox desktop type --id blue-lobster --text "peter+qa@example.com"
```

`desktop type` uses clipboard paste for symbol-heavy text, so `@`, `+`,
password-like values, and URLs do not depend on the target X keyboard layout.

Related docs:

- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [Hetzner](hetzner.md)
- [AWS](aws.md)
- [Azure](azure.md)
- [vnc command](../commands/vnc.md)
- [webvnc command](../commands/webvnc.md)
- [desktop command](../commands/desktop.md)
- [screenshot command](../commands/screenshot.md)
