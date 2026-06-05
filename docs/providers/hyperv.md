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
- A pre-configured Windows VHDX template with:
  - OpenSSH Server installed and running
  - A known user account (default: `crabbox`) with a known password
  - Network configured for DHCP on the Hyper-V virtual switch
- The Hyper-V PowerShell module (included with the Hyper-V feature)

ISO images are not supported. The VHDX template must be a fully installed
Windows image with SSH ready to accept connections.

## Configuration

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--hyperv-image` | (none) | Path to a Windows VHDX template (required) |
| `--hyperv-cpu` | `4` | Number of virtual CPUs |
| `--hyperv-memory` | `8192` | Memory in MB |
| `--hyperv-disk` | `50` | Disk size in GB |
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

1. Copies the VHDX template to a per-lease path
2. Creates and starts the VM
3. Injects the per-lease SSH public key via PowerShell Direct
   (`Invoke-Command -VMName`) using the configured guest password
4. Waits for SSH readiness on the injected key

The SSH key injection retries up to 5 times with backoff to allow the guest OS
to boot. If injection fails (wrong password, guest not ready), the provider
falls back to whatever SSH configuration the VHDX template already has.

Set `CRABBOX_HYPERV_GUEST_PASSWORD` or `hyperv.guestPassword` in config to
match the password baked into your VHDX template. Default: `crabbox`.

## Lifecycle

1. **Acquire**: Copies the VHDX template, creates a Generation 2 VM (`New-VM`),
   configures CPU count (`Set-VM`), connects the network adapter to the
   configured switch (`Connect-VMNetworkAdapter`), starts the VM (`Start-VM`),
   injects the SSH key via PowerShell Direct, polls for an IP address via
   `Get-VMNetworkAdapter`, then waits for SSH readiness.
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
