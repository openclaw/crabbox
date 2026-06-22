# Windows VNC and WSL2

Read this when you:

- use managed AWS or Azure Windows desktop leases;
- choose between native Windows and WSL2;
- prepare a static (bring-your-own) Windows host for Crabbox VNC.

Crabbox supports two Windows execution contracts, selected with `--windows-mode`
(`--windows-mode normal`, the default, or `--windows-mode wsl2`; also settable via
`CRABBOX_WINDOWS_MODE` or `windows.mode` in config):

- **Native Windows** (`normal`): commands run as PowerShell over OpenSSH, sync uses
  the Windows-native path, and `--desktop` brings up a visible Windows console
  session with VNC.
- **WSL2** (`wsl2`): commands run as POSIX shell inside a WSL distribution, sync
  uses the Linux contract, and there is no separate managed VNC contract beyond the
  underlying Windows host.

Managed native Windows desktop support is available on AWS and Azure. For the
provider-neutral desktop and VNC overview, see
[Interactive desktop and VNC](interactive-desktop-vnc.md).

## Managed AWS Windows

```sh
crabbox warmup --provider aws --target windows --desktop
crabbox vnc --id swift-crab --open
crabbox screenshot --id swift-crab --output windows.png
```

Bootstrap flow:

- EC2Launch v2 runs the `enableOpenSsh` user-data task, opening the first OpenSSH
  foothold on port `22`.
- Over that SSH connection, Crabbox runs the shared Windows desktop bootstrap as a
  local `crabbox` administrator: it installs Git for Windows and TightVNC and
  configures the visible console session.
- Windows auto-logon (`AutoAdminLogon`) starts a visible console session for the
  `crabbox` user.
- TightVNC runs inside that logged-in user session through the `CrabboxUserVNC`
  logon scheduled task; the per-user (`HKCU`) password values are copied from the
  service (`HKLM`) configuration at startup.
- The TightVNC service is disabled after seeding the per-user configuration, so
  screenshots and WebVNC target the visible console session, not the service
  session.
- The Windows first-network-discovery flyout is suppressed (`NewNetworkWindowOff`)
  during bootstrap so it does not cover screenshots.
- VNC listens on port `5900` and is reachable only through the SSH tunnel.

## Managed Azure Windows

```sh
crabbox warmup --provider azure --target windows --desktop
crabbox vnc --id blue-lobster --open
crabbox screenshot --id blue-lobster --output windows.png
```

Bootstrap flow:

- Azure creates the VM, public IP, NIC, and OS disk.
- The full Windows bootstrap script is delivered as base64 custom data; an Azure
  Custom Script Extension copies `C:\AzureData\CustomData.bin` to
  `C:\AzureData\crabbox-bootstrap.ps1` and runs it.
- The script installs OpenSSH, Git for Windows, and TightVNC, creates the `crabbox`
  administrator, and configures auto-logon. Every downloaded installer/archive
  is pinned to a release URL and verified with SHA-256 before extraction or
  execution; a mismatch deletes the download and stops bootstrap.
- In desktop mode the bootstrap reboots once to land the auto-logon console
  session, then Crabbox waits for SSH and VNC readiness.
- VNC listens on port `5900` and is reachable only through the SSH tunnel.

### Console login credentials

On managed Windows desktop leases, `crabbox vnc` prints both the VNC password and
the generated Windows console login:

```text
managed: true
password: ...
windows username: crabbox
windows password: ...
```

That login belongs to the Crabbox-created instance. It is not your local Windows
account and is not stored in coordinator history. On the box, Crabbox records:

- `C:\ProgramData\crabbox\vnc.password` — VNC authentication password (what `crabbox
  vnc` reads and prints as `password:`);
- `C:\ProgramData\crabbox\windows.username` and
  `C:\ProgramData\crabbox\windows.password` — the console account name and its
  password (the same value as the VNC password in desktop mode).

## WSL2

Managed AWS and Azure WSL2 leases are Windows instances with nested virtualization
enabled and an Ubuntu rootfs imported into WSL. Commands and sync use the POSIX WSL
contract:

```sh
crabbox warmup --provider aws --target windows --windows-mode wsl2
crabbox warmup --provider azure --target windows --windows-mode wsl2
crabbox actions hydrate --id blue-lobster
crabbox run --id blue-lobster -- pnpm test
```

Use native Windows mode when you need the Windows desktop. Use WSL2 when you need
Linux tooling on Windows-capable nested-virtualization VM families.

The bootstrap enables the `Microsoft-Windows-Subsystem-Linux`,
`VirtualMachinePlatform`, and `HypervisorPlatform` features, updates the WSL kernel,
and imports a `Crabbox` distribution from a versioned Ubuntu rootfs whose
SHA-256 is verified before import. Enabling the features
and updating the kernel each trigger a reboot, so the first WSL2 warmup takes longer
than a native Windows warmup.

## Static Windows

Static Windows is host-managed: Crabbox connects to a durable host you already own
and never provisions or tears it down.

```yaml
provider: ssh
target: windows
windows:
  mode: normal
static:
  host: win-dev.example.com
  user: alice
  port: "22"
  workRoot: C:\crabbox
```

```sh
crabbox vnc --provider ssh --target windows --static-host win-dev.example.com --host-managed --open
```

The static host must already provide OpenSSH Server, PowerShell, Git, `tar`, a
writable `static.workRoot`, and a VNC-compatible service. `--open` requires
`--host-managed`, because the visible credential prompt belongs to that durable
host rather than to a Crabbox-created lease.

For static WSL2, set `windows.mode: wsl2` and point `static.workRoot` at a WSL path
such as `/home/alice/crabbox`.

## Troubleshooting

**Tunnel command uses port `22`.** Expected for AWS Windows. EC2Launch enables
OpenSSH on port `22`, and Crabbox records the working SSH port after probing the
configured fallbacks.

**Screenshot is black from raw SSH.** Use `crabbox screenshot`. It runs a scheduled
task inside the logged-in console session; an ad hoc non-interactive SSH PowerShell
session cannot reliably capture the visible desktop.

**VNC opens an OS credential prompt.** Check `managed:` in `crabbox vnc` output. If
it is `false`, you opened a static host. Use that host's own credentials, and pass
`--host-managed` only when you intend to.

**WebVNC keeps retrying in the browser.** Close any older retrying tab and start a
fresh `crabbox webvnc` bridge; a stale tab can keep reconnecting with an outdated
URL fragment. On managed AWS Windows, Crabbox configures TightVNC in the logged-in
user's registry profile; if direct VNC auth also fails, recreate the lease with a
current Crabbox build.

## Related docs

- [Interactive desktop and VNC](interactive-desktop-vnc.md)
- [AWS](aws.md)
- [Azure](azure.md)
- [`vnc` command](../commands/vnc.md)
- [`screenshot` command](../commands/screenshot.md)
