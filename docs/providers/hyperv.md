# Hyper-V

Provider id: `hyperv`
Aliases: `local-hyperv`, `hyper-v`, `windows-vm`
Kind: SSH lease
Targets: Windows (native)
Family: `local-vm`

## Overview

The Hyper-V provider creates and manages Windows virtual machines on a local
Windows host using Microsoft Hyper-V. VMs are provisioned as Generation 2 VMs
with a new VHDX, connected to a configurable virtual switch (default: "Default
Switch"), and accessed over SSH via the built-in Windows OpenSSH server.

Hyper-V must be enabled on the host (`Enable-WindowsOptionalFeature -Online
-FeatureName Microsoft-Hyper-V-All`). The provider is Windows-only and will
reject configuration on non-Windows hosts.

## Requirements

- Windows 10 Pro/Enterprise/Education or Windows Server with Hyper-V enabled
- PowerShell 5.1 or later (ships with Windows)
- A Windows VHDX image with OpenSSH Server installed and enabled
- The Hyper-V PowerShell module (included with the Hyper-V feature)

## Configuration

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--hyperv-image` | (none) | Path to a Windows VHDX or ISO for VM creation (required) |
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
```

### Environment variables

| Variable | Description |
| --- | --- |
| `CRABBOX_HYPERV_IMAGE` | VHDX or ISO path |
| `CRABBOX_HYPERV_USER` | SSH user inside the VM |
| `CRABBOX_HYPERV_WORK_ROOT` | Work root inside the VM |
| `CRABBOX_HYPERV_CPUS` | CPU count |
| `CRABBOX_HYPERV_MEMORY` | Memory in MB |
| `CRABBOX_HYPERV_DISK` | Disk in GB |
| `CRABBOX_HYPERV_SWITCH` | Virtual switch name |

## Lifecycle

1. **Acquire**: Creates a Generation 2 VM (`New-VM`), configures CPU count
   (`Set-VM`), connects the network adapter to the configured switch
   (`Connect-VMNetworkAdapter`), starts the VM (`Start-VM`), polls for an IP
   address via `Get-VMNetworkAdapter`, then waits for SSH readiness.
2. **Resolve**: Finds a running crabbox VM by lease ID, slug, or instance name.
3. **List**: Lists all VMs with the `crabbox-` name prefix.
4. **Release**: Stops the VM (`Stop-VM -Force`) and removes it
   (`Remove-VM -Force`), then cleans up the VHDX file.
5. **Cleanup**: Scans for stale `crabbox-` prefixed VMs with expired or missing
   lease claims and removes them.

## Notes

- All VMs are named with a `crabbox-` prefix. Cleanup and release operations
  refuse to touch VMs without this prefix.
- VHD files are stored in `%USERPROFILE%\Hyper-V\Virtual Hard Disks\` by
  default.
- The SSH ready check uses `powershell -Command "$PSVersionTable.PSVersion |
  Out-Null"` to verify the Windows guest is responsive.
- There is no `tart exec` or `prlctl exec` equivalent for Hyper-V; all guest
  interaction happens over SSH.

## Examples

```sh
crabbox warmup --provider hyperv --hyperv-image C:\Images\win-server.vhdx
crabbox run --provider hyperv -- powershell -Command "Get-Process"
crabbox ssh --provider hyperv
crabbox stop --provider hyperv --id blue-lobster
crabbox cleanup --provider hyperv
```
