# Hyper-V

Provider id: `hyperv`
Kind: SSH lease
Targets: Windows (native)
Family: `local-vm`

## Overview

The Hyper-V provider creates and manages Windows virtual machines on a local
Windows host using Microsoft Hyper-V. VMs are provisioned as Generation 2 VMs
from a pre-configured VHDX template, connected to a configurable virtual switch
(default: "Default Switch"), and accessed over SSH via the built-in Windows
OpenSSH server.

Hyper-V must be enabled on the host (`Enable-WindowsOptionalFeature -Online
-FeatureName Microsoft-Hyper-V-All`). The provider is Windows-only and will
reject configuration on non-Windows hosts.

## Requirements

- Windows 10 Pro/Enterprise/Education or Windows Server with Hyper-V enabled
- PowerShell 5.1 or later (ships with Windows)
- A Windows VHDX template (Generation 2 / UEFI) with:
  - A local administrator account whose password is known to Crabbox
    (`--hyperv-user` / `CRABBOX_HYPERV_GUEST_PASSWORD`)
  - Network configured for DHCP on the Hyper-V virtual switch
  - Guest internet access (or a Features-on-Demand source) so OpenSSH can be
    installed on first use
- The Hyper-V PowerShell module (included with the Hyper-V feature)

OpenSSH and git do **not** need to be pre-installed: on first acquire the
provider installs the Windows OpenSSH server (over PowerShell Direct) and, if
absent, git (portable MinGit) — both are no-ops when already present, so a
template that pre-bakes them just skips the per-lease download. This keeps the
template requirement to a plain Windows VHDX with a known admin password. ISO
images are not supported — provide a fully installed VHDX.

### Preparing a template

The only thing a base Windows VHDX needs is a reachable administrator account.
For example, from an elevated prompt inside the guest before capturing it:

```powershell
net user Administrator '<password>'   # or your admin account
net user Administrator /active:yes
```

Then point `--hyperv-image` at the VHDX and set `--hyperv-user Administrator`
and `CRABBOX_HYPERV_GUEST_PASSWORD=<password>`. The provider handles OpenSSH.

## Configuration

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--hyperv-image` | (none) | Path to a Windows VHDX template (required) |
| `--hyperv-user` | `crabbox` | Guest administrator account for SSH (password via `CRABBOX_HYPERV_GUEST_PASSWORD`) |
| `--hyperv-work-root` | `C:\crabbox` | Crabbox work root inside the guest |
| `--hyperv-cpu` | `4` | Number of virtual CPUs |
| `--hyperv-memory` | `8192` | Memory in MB |
| `--hyperv-disk` | `50` | Reserved; leases inherit the template's virtual size (differencing disk) |
| `--hyperv-switch` | `Default Switch` | Hyper-V virtual switch name |

### Config file

```yaml
hyperv:
  image: C:\Images\windows-crabbox.vhdx
  user: crabbox
  workRoot: C:\crabbox
  cpus: 4
  memory: 8192
  disk: 50
  switch: Default Switch
  guestPassword: crabbox
```

### Environment variables

| Variable | Description |
| --- | --- |
| `CRABBOX_HYPERV_IMAGE` | VHDX template path |
| `CRABBOX_HYPERV_USER` | SSH user inside the VM |
| `CRABBOX_HYPERV_WORK_ROOT` | Work root inside the VM |
| `CRABBOX_HYPERV_CPUS` | CPU count |
| `CRABBOX_HYPERV_MEMORY` | Memory in MB |
| `CRABBOX_HYPERV_DISK` | Disk in GB |
| `CRABBOX_HYPERV_SWITCH` | Virtual switch name |
| `CRABBOX_HYPERV_GUEST_PASSWORD` | Guest user password for SSH key injection |

## Bootstrap contract

During `Acquire`, the provider:

1. Creates a per-lease **differencing disk** backed by the template (near-instant
   and space-thin; the template stays read-only and shared — no multi-GB copy)
2. Creates and starts the VM
3. Installs and starts the Windows OpenSSH server in the guest via PowerShell
   Direct if not already present (`Add-WindowsCapability`, `Start-Service sshd`,
   firewall rule), and installs git (MinGit) if absent — both required for SSH
   readiness and crabbox sync
4. Injects the per-lease SSH public key via PowerShell Direct
   (`Invoke-Command -VMName`) using the configured guest password
5. Waits for SSH readiness on the injected key

Both the OpenSSH-install and key-injection steps authenticate over PowerShell
Direct using the guest administrator password and retry up to 5 times with
backoff to allow the guest OS to boot.

Set `CRABBOX_HYPERV_GUEST_PASSWORD` or `hyperv.guestPassword` in config to
match the administrator password in your VHDX template. Default: `crabbox`.

## Lifecycle

1. **Acquire**: Creates a per-lease differencing disk over the template
   (`New-VHD -Differencing -ParentPath`), creates a Generation 2 VM (`New-VM`),
   configures CPU count and disables automatic checkpoints (`Set-VM`), connects
   the network adapter to the configured switch (`Connect-VMNetworkAdapter`),
   starts the VM (`Start-VM`), polls for an IP address via
   `Get-VMNetworkAdapter`, installs OpenSSH in the guest if needed and injects
   the SSH key via PowerShell Direct, then waits for SSH readiness.
2. **Resolve**: Finds a running crabbox VM by lease ID, slug, or instance name.
   Queries live VM state and IP from Hyper-V.
3. **List**: Lists all VMs with the `crabbox-` name prefix.
4. **Release**: Stops the VM (`Stop-VM -Force`) and removes it
   (`Remove-VM -Force`), then cleans up the provider-created VHDX file.
5. **Cleanup**: Scans for stale `crabbox-` prefixed VMs with expired or missing
   lease claims and removes them.

## Notes

- All VMs are named with a `crabbox-` prefix. Cleanup and release operations
  refuse to touch VMs without this prefix.
- VHD files are stored in `%USERPROFILE%\Hyper-V\Virtual Hard Disks\` by
  default. Only the provider-created boot disk is cleaned up on release; other
  attached disks are preserved.
- The SSH ready check uses the shared native-Windows readiness probe, which
  verifies that `git`, `tar`, and the work root are available in the guest.
- There is no `tart exec` or `prlctl exec` equivalent for Hyper-V; all guest
  interaction after bootstrap happens over SSH.

## Examples

```sh
crabbox warmup --provider hyperv --hyperv-image C:\Images\win-server.vhdx
crabbox run --provider hyperv -- powershell -Command "Get-Process"
crabbox ssh --provider hyperv
crabbox stop --provider hyperv --id blue-lobster
crabbox cleanup --provider hyperv
```
