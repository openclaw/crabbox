# Windows VNC

Read when:

- using managed AWS Windows desktop leases;
- choosing between native Windows and WSL2;
- preparing a static Windows host for Crabbox VNC.

Crabbox has two Windows execution contracts:

- native Windows: PowerShell over OpenSSH, archive sync, Windows desktop;
- WSL2: POSIX commands through WSL, Linux-style sync, no separate managed VNC
  contract beyond the underlying Windows host.

Managed Windows desktop support is AWS-only.

## Managed AWS Windows

```sh
crabbox warmup --provider aws --target windows --desktop
crabbox vnc --id crimson-crab --open
crabbox screenshot --id crimson-crab --output windows.png
```

Bootstrap flow:

- EC2Launch v2 enables the first OpenSSH foothold on port `22`.
- Crabbox installs Git for Windows and TightVNC.
- Crabbox creates a local `crabbox` administrator.
- Windows auto-logon starts a visible console session for that user.
- TightVNC runs in that logged-in user session, with its HKCU password values
  copied from the service configuration during startup.
- The generated password is stored at
  `C:\ProgramData\crabbox\vnc.password`.
- VNC remains reachable only through the SSH tunnel.

`crabbox vnc` prints both the VNC password and the generated Windows console
login:

```text
windows username: crabbox
windows password: ...
```

That login belongs to the Crabbox-created EC2 instance. It is not your local
Windows account and is not stored in coordinator history.

## WSL2

Managed AWS WSL2 leases are Windows instances with nested virtualization
enabled and an Ubuntu rootfs imported into WSL. Commands and sync use the POSIX
WSL contract:

```sh
crabbox warmup --provider aws --target windows --windows-mode wsl2
crabbox run --id blue-lobster -- pnpm test
```

Use native Windows mode when you need the Windows desktop. Use WSL2 when you
need Linux tooling on Windows-capable AWS instance families.

## Static Windows

Static Windows is host-managed:

```yaml
provider: ssh
target: windows
windows:
  mode: normal
static:
  host: win-dev.local
  user: Peter
  port: "22"
  workRoot: C:\crabbox
```

```sh
crabbox vnc --provider ssh --target windows --static-host win-dev.local --host-managed --open
```

The static host must already have OpenSSH Server, PowerShell, Git, `tar`, a
writable `static.workRoot`, and a VNC-compatible service. `--open` requires
`--host-managed` because the visible password prompt belongs to that durable
host, not to a Crabbox-created lease.

For static WSL2, set `windows.mode: wsl2` and use a WSL path such as
`/home/peter/crabbox` for `static.workRoot`.

## Troubleshooting

Tunnel command uses port `22`

Expected for AWS Windows. EC2Launch enables OpenSSH on port `22`, and Crabbox
records the working SSH port after probing fallbacks.

Screenshot is black from raw SSH

Use `crabbox screenshot`. It runs a scheduled task inside the logged-in console
session; an ad hoc non-interactive SSH PowerShell session cannot reliably
capture the visible desktop.

VNC opens an OS credential prompt

Check `managed:` in `crabbox vnc` output. If it is `false`, you opened a static
host. Use that host's credentials and pass `--host-managed` intentionally.

WebVNC keeps retrying in the browser

Close any older retrying tab and start a fresh `crabbox webvnc` bridge. A stale
tab can keep reconnecting with an old URL fragment. On managed AWS Windows,
Crabbox configures TightVNC in the logged-in user's registry profile; if direct
VNC auth also fails, recreate the lease with a current Crabbox build.

Related docs:

- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [AWS](aws.md)
- [vnc command](../commands/vnc.md)
- [screenshot command](../commands/screenshot.md)
