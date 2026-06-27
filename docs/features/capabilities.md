# Lease Capabilities

Read when:

- adding `--desktop`, `--browser`, or `--code` to a workflow;
- choosing a Linux desktop environment with `--desktop-env`;
- changing how Crabbox detects whether a lease can host a visible desktop;
- adding a new lease capability flag.

Lease capabilities are opt-in features that extend what a runner can do beyond
running headless commands. They are distinct from the provider *feature set*
declared in `ProviderSpec.Features`: a feature set says "this provider *can*
host a desktop"; a capability label says "this lease *was created* with a
desktop and exposes one right now".

## The three capabilities

```text
--desktop  visible desktop with a loopback VNC server (XFCE, Wayland, or GNOME)
--browser  Chrome/Chromium installed and exported via $BROWSER and $CHROME_BIN
--code     code-server bound to a loopback port for the portal code bridge
```

All three default to off. They must be requested when the lease is created
(`crabbox warmup --desktop`) and are then reused on later commands. A lease
created without a capability cannot grow it later; warm a new lease instead.

The desktop flavor is selected with `--desktop-env xfce|wayland|gnome`
(default `xfce`). Wayland and GNOME require `--target linux`.

## Selection and validation

Capability flags follow a two-step validation, both in
`internal/cli/capabilities.go`.

1. **Provider feature check** (`validateRequestedCapabilities`). When you set a
   capability flag, Crabbox looks up the selected provider's `Spec().Features`
   and rejects the request if the matching feature (`FeatureDesktop`,
   `FeatureBrowser`, `FeatureCode`) is missing. Hetzner Linux supports all
   three; the delegated-run providers support none. Additional guards:
   - `--code` is restricted to managed Linux leases; Windows, macOS, and static
     SSH are rejected.
   - `--target windows --windows-mode wsl2` rejects `--desktop` (use
     `--windows-mode normal` for a desktop, or omit `--desktop` for WSL2).
   - `provider=azure --target windows` supports SSH, sync, run, and
     desktop/VNC only; `--browser`, `--code`, and `--tailscale` need Linux or
     AWS Windows.
   - `--desktop-env wayland|gnome` requires `--target linux`.
2. **Lease label check** (`enforceManagedLeaseCapabilities`). When you reuse a
   lease with `--id`, Crabbox checks the matching label (`desktop=true`,
   `browser=true`, `code=true`) on the existing record. If the label is
   missing, it refuses with a hint to warm a new lease. A non-default
   `--desktop-env` must also match the lease's stored `desktop_env` label.

Label enforcement is skipped for static SSH targets, because Crabbox does not
own the host. There the capability is detected probe-by-probe instead (see
[Static targets](#static-targets)). A macOS lease is treated as
desktop-capable without a `desktop=true` label, because Screen Sharing is the
desktop.

## Desktop

When a managed Linux lease is created with `--desktop` (default `xfce`),
bootstrap (`internal/cli/bootstrap.go`) installs and enables systemd units for:

- resize-capable TigerVNC on display `:99`;
- an XFCE4 session (`xfce4-session`, panel, terminal, settings, theme);
- VNC bound to `127.0.0.1:5900` with `-localhost yes`;
- a randomized VNC password at `/var/lib/crabbox/vnc.password`;
- screenshot and capture tooling: `scrot`, `ffmpeg`, plus input helpers
  (`xdotool`, `wmctrl`, `xclip`, `xsel`).

With `--desktop-env wayland` or `gnome`, the lease runs a Wayland compositor
(labwc) with WayVNC on the same loopback `127.0.0.1:5900`.

`crabbox vnc --id ...` opens an SSH tunnel to that loopback port; your local
VNC viewer connects through the tunnel using the password the CLI reads from
the lease. There is no public VNC port — the loopback bind is the security
boundary.

The VNC password lives at an OS-specific path:

```text
/var/lib/crabbox/vnc.password         # Linux
/var/db/crabbox/vnc.password          # macOS
C:\ProgramData\crabbox\vnc.password   # Windows
```

When a run injects environment for a desktop lease, Crabbox probes the lease's
desktop env file and sets `CRABBOX_DESKTOP=1` plus the relevant display
variables. For X11 it sets:

```text
DISPLAY=:99
CRABBOX_DESKTOP=1
```

For a Wayland/GNOME desktop it instead forwards `WAYLAND_DISPLAY`,
`XDG_RUNTIME_DIR`, and related variables detected on the host. Tools that
respect these draw onto the desktop the lease created.

For per-OS detail and known limits, see:

- [Linux VNC](vnc-linux.md)
- [Windows VNC](vnc-windows.md)
- [macOS VNC](vnc-macos.md)
- [Interactive desktop and VNC](interactive-desktop-vnc.md)

## Browser

`--browser` adds a usable browser to the lease without provisioning a full
desktop.

On managed Linux, bootstrap installs:

- Google Chrome stable when the repo is reachable;
- Chromium (or `chromium-browser`) as a fallback;
- build helpers (`build-essential`, `python3`, etc.) so dependency installs
  that compile against Chromium succeed;
- a managed Chrome policy and a launcher wrapper, whose path is written to
  `/var/lib/crabbox/browser.env` as `BROWSER` and `CHROME_BIN`.

On static and macOS/Windows targets, Crabbox probes for an existing browser
(`probeBrowserEnv`) and aborts before the command runs if none is found:

- macOS: `/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`;
- Windows: `chrome.exe` or `msedge.exe` from PATH or the standard install
  directories;
- Linux: `$BROWSER`, `$CHROME_BIN`, then `google-chrome`, `chromium`, or
  `chromium-browser` from PATH.

The resolved path is exported into the run:

```text
BROWSER=/path/to/browser
CHROME_BIN=/path/to/browser
CRABBOX_BROWSER=1
```

Test runners that read `BROWSER` or `CHROME_BIN` (Vitest, Playwright, etc.)
work without extra plumbing.

For browser QA where the remote service is sensitive to source IP (login
flows, regional CDN behavior), pair `--browser` with
[mediated egress](egress.md): `crabbox egress start` opens a lease-local proxy
that exits through the operator machine, and `crabbox desktop launch --egress`
passes that proxy to Chrome.

## Code

`--code` provisions code-server on managed Linux leases. Bootstrap:

- installs the binary at `/usr/local/bin/code-server` (standalone install,
  `--prefix=/usr/local`);
- binds it to a loopback port (default `8080`);
- relies on coordinator state for the access token.

`crabbox code --id ...` and the portal open a code-server tab through the
authenticated portal bridge at `/portal/leases/{id-or-slug}/code/`. The bridge
proxies HTTP and WebSocket traffic to the loopback port and injects the auth
token, so you never handle it directly. There is no public code-server port.

Code is managed-Linux-only because the bridge depends on the lease shape and
the cloud-init that installs the binary. Windows, macOS, and static SSH are
intentionally unsupported today.

## Capability labels

Managed lease records carry capability labels (`internal/cli/provider_labels.go`)
so `list`, `status`, and the portal can render the capability matrix without
re-probing the host:

```text
desktop=true
desktop_env=xfce|wayland|gnome   # only when desktop=true
browser=true
code=true
```

`enforceManagedLeaseCapabilities` reads these labels to gate `--desktop`,
`--browser`, `--code`, and `--desktop-env` on `--id` reuse paths. The labels
are written at lease creation and never flipped on a live lease.

## Composing capabilities

Capabilities are independent — any combination is allowed where the provider
supports them:

```sh
crabbox warmup --desktop                       # desktop only (XFCE)
crabbox warmup --desktop --desktop-env gnome   # GNOME desktop
crabbox warmup --desktop --browser             # browser on the desktop
crabbox warmup --desktop --browser --code      # full interactive box
crabbox warmup --browser                       # headless browser, no VNC
crabbox warmup --code                          # editor-only Linux lease
```

Capability bootstrap adds installation time. A bare lease warms fastest; a
lease with all three takes longest. Use the lightest combination that
satisfies the workflow.

## Static targets

For static SSH hosts, capability validation degrades to probe-based detection,
because Crabbox does not install software on operator-owned machines:

- `--desktop`: probe loopback VNC at `127.0.0.1:5900` over SSH (X11 checks for
  Xtigervnc or legacy Xvfb/x11vnc; Wayland/GNOME checks for the compositor and
  WayVNC). Fail with a clear error if the desktop is not running.
- `--browser`: probe for a browser binary using the OS-specific search list;
  fail if none is found.
- `--code` is rejected (managed Linux only).

This is intentional: if a static box does not expose the capability, the run
should fail loudly rather than silently fall back. macOS hosts can enable
Screen Sharing; Windows hosts need a VNC server bound to `127.0.0.1:5900`.

## Related docs

- [warmup command](../commands/warmup.md)
- [run command](../commands/run.md)
- [vnc command](../commands/vnc.md)
- [webvnc command](../commands/webvnc.md)
- [code command](../commands/code.md)
- [egress command](../commands/egress.md)
- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [Mediated egress](egress.md)
- [Browser portal](portal.md)
