# Linux VNC

Read when:

- using `--desktop` on Hetzner or AWS Linux;
- debugging Xvfb, XFCE/Openbox, x11vnc, or screenshots on a Linux lease;
- preparing a static Linux host for Crabbox VNC.

Linux is the simplest managed desktop path. Hetzner and AWS Linux leases use
the same bootstrap shape: install a lightweight desktop, run it on `DISPLAY=:99`,
bind x11vnc to loopback, and let the CLI create an SSH tunnel.

## Managed Linux

```sh
crabbox warmup --desktop --browser
crabbox run --id blue-lobster --desktop --browser -- google-chrome --version
crabbox vnc --id blue-lobster --open
crabbox screenshot --id blue-lobster --output linux.png
```

Managed Linux desktop leases include:

- Xvfb on `:99`;
- a lightweight desktop/window-manager session;
- x11vnc bound to `127.0.0.1:5900`;
- a generated per-lease VNC password at `/var/lib/crabbox/vnc.password`;
- optional Chrome stable or Chromium fallback when `--browser` is requested;
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

Related docs:

- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [Hetzner](hetzner.md)
- [AWS](aws.md)
- [vnc command](../commands/vnc.md)
- [screenshot command](../commands/screenshot.md)
