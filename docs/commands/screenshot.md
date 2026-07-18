# screenshot

`crabbox screenshot` captures a single PNG from a desktop lease without opening a
VNC client. It is the fastest way to confirm what a remote box is rendering.

```sh
crabbox warmup --desktop
crabbox screenshot --id swift-crab
crabbox screenshot --id swift-crab --output desktop.png
crabbox screenshot --id swift-crab --network tailscale
```

## How it works

The command resolves and touches the lease the same way `crabbox ssh` does,
claims it for the current repo if asked, then waits for the loopback desktop/VNC
service to come up before grabbing a frame. The capture method depends on the
target OS:

- **Linux** captures over SSH. It sources `/var/lib/crabbox/desktop.env` when
  present, prefers `grim` on Wayland, and otherwise screenshots `DISPLAY=:99`
  with `scrot` (falling back to ImageMagick `import`). If no capture tool is
  found, it exits with a hint to warm a fresh `--desktop` lease.
- **macOS** captures the managed Screen Sharing/VNC framebuffer through the
  lease SSH tunnel. EC2 Mac SSH sessions cannot reliably run `screencapture`
  against the login-window display, so the framebuffer path is used instead.
  External macOS leases use the approved operator-managed account credential
  referenced by `external.connection.desktop.passwordEnv` and authenticate the
  framebuffer connection with ARD.
- **Windows** creates a one-shot scheduled task inside the logged-in `crabbox`
  console session, because non-interactive SSH sessions cannot capture the
  visible desktop. The screenshot reflects that active console session.

Managed AWS and Azure Windows desktop leases enable auto-logon for the generated
`crabbox` user, store that password under `C:\ProgramData\crabbox`, and use it
only on the instance to run the scheduled capture task.

## Output path

If `--output` is omitted, Crabbox writes to the current directory as:

```text
crabbox-<slug-or-id>-screenshot.png
```

`--output` accepts any path; missing parent directories are created. On success
the command prints `screenshot: <path>`.

## Capability and target requirements

The lease must have the `desktop` capability (`crabbox warmup --desktop`);
managed leases without it are rejected.

Static (`provider=ssh`) macOS and Windows targets are existing host machines,
not Crabbox-created desktops, so `screenshot` rejects them rather than capturing
your local or home-host desktop by accident. Static Linux hosts are still
captured. Managed AWS/Azure Windows and AWS macOS desktop leases are
Crabbox-created boxes and can be captured by lease id or slug. A
desktop-capable External macOS adapter can also be captured once its trusted
target/account contract and password environment reference are configured.
Use `crabbox webvnc --provider external --target macos --id <lease> --preflight`
to verify the same SSH-tunneled RFB/ARD authentication path before capture.

The Blacksmith provider owns machine connectivity and does not support desktop
screenshots.

## Flags

```text
--id <lease-id-or-slug>     Lease to capture (also accepts the first positional arg)
--provider <name>           SSH-capable provider (defaults to configured provider)
--output <path>             Local PNG output path
--reclaim                   Claim this lease for the current repo
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>        provider=ssh: existing SSH host
--static-user <user>        provider=ssh: SSH user
--static-port <port>        provider=ssh: SSH port
--static-work-root <path>   provider=ssh: work root on the host
--external-routing-file <path>
--external-desktop-username <name>
--external-desktop-password-env <name>
--network auto|tailscale|public
```

Run `crabbox screenshot --help` for the provider list valid in your build; it is
generated from the SSH-capable providers and varies by configuration.

## Related docs

- [Interactive desktop and VNC](../features/interactive-desktop-vnc.md)
- [Linux VNC](../features/vnc-linux.md)
- [Windows VNC](../features/vnc-windows.md)
- [macOS VNC](../features/vnc-macos.md)
- [External provider](../providers/external.md)
