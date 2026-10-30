# Hyper-V

Provider id: `hyperv`
Kind: SSH lease
Targets: Windows (native)
Family: `local-vm`

## Overview

The Hyper-V provider creates and manages Windows virtual machines on a
local Windows host using Microsoft Hyper-V. VMs are provisioned as Generation 2
VMs from a Windows VHDX template, connected to a configurable virtual
switch (default: "Default Switch"), and accessed over SSH. Windows normal mode
is the default target when `--target` is omitted.

| Capability | Windows normal |
| --- | --- |
| SSH, Crabbox sync, cleanup | Yes |
| Pause and resume | Yes |

Hyper-V must be enabled on the host (`Enable-WindowsOptionalFeature -Online
-FeatureName Microsoft-Hyper-V-All`). The provider is Windows-only and will
reject configuration on non-Windows hosts.

## Requirements

- Windows 10 Pro/Enterprise/Education or Windows Server with Hyper-V enabled
- PowerShell 5.1 or later (ships with Windows)
- A Generation 2 / UEFI VHDX template for the Windows guest
- A Windows VHDX with:
  - A local administrator account selected with `--hyperv-user`, with its
    password explicitly provided through `CRABBOX_HYPERV_GUEST_PASSWORD`
  - Network configured for DHCP on the Hyper-V virtual switch
  - Guest internet access to GitHub when OpenSSH or git must be installed on
    first use, and to nodejs.org when Node/npm must be installed
- The Hyper-V and Windows storage PowerShell modules

### Host permissions and doctor

Run Crabbox on the Hyper-V host from an account authorized to manage Hyper-V
VMs, switches, VHDXs, and storage. Membership in
the local `Hyper-V Administrators` group is sufficient and needs no elevation;
otherwise use an elevated shell.

Do not run the host-side Crabbox process as `LocalSystem`. Session 0 is only a
convenience for **unattended** hosts, where failed early PowerShell Direct
authentication can display credential UI that no one dismisses. It is unrelated
to the guest. A `LocalSystem` host process resolves a different profile, so SSH
known-hosts, Crabbox config, and `PATH` differ from the invoking user, per-user
tools such as `ffmpeg` are missing, leftover state becomes `LocalSystem`-owned
and needs elevation to remove, and elevation wrappers can corrupt captured
command output.

`crabbox doctor --provider hyperv` is non-mutating. It checks that the host is
Windows, queries the Hyper-V optional feature, inventories VMs, and reports the
configured image path. It does not validate that the image exists or boots,
that Windows credentials work, or that other guest prerequisites are satisfied. `--probe-ssh` reports that a running lease is
required rather than creating one. Its Hyper-V optional-feature query calls
`Get-WindowsOptionalFeature`, which requires elevation even when provisioning
does not, so an unelevated `Hyper-V Administrators` shell reports that one check
as failed while acquire, proof, and release still work.

OpenSSH, git, and Node/npm do **not** need to be pre-installed. On first acquire the
provider installs the pinned, SHA-256-verified Win32-OpenSSH MSI used by the
`Microsoft.OpenSSH.Preview` winget package and, if absent, portable MinGit
(also pinned and SHA-256-verified). The provider also runs the shared Windows
Node/npm baseline, downloading a pinned, SHA-256-verified Node archive if a
working Node/npm pair is absent. These installs are no-ops when already
present and working, so a prepared template skips the matching per-lease
download. OpenSSH bootstrap does not depend on
Windows Update, WSUS, or a matching Features on Demand source. This keeps the
template requirement to a plain Windows VHDX with a known admin password. ISO
images are not supported — provide a fully installed VHDX.

Windows acquisition allows up to 25 minutes overall, including a ten-minute
authenticated PowerShell Direct readiness budget after VM creation. An external
timeout shorter than that can terminate acquisition
before readiness finishes. Bootstrap progress on stderr distinguishes waiting for authentication
from completed SSH lockdown and network connection. A booted
VM or a started network adapter alone does not prove guest authentication.

### Preparing a Windows template

The only thing a base Windows VHDX needs is a reachable administrator account.
For example, from an elevated prompt inside the guest before capturing it:

```powershell
net user Administrator '<password>'   # or your admin account
net user Administrator /active:yes
```

Then point `--hyperv-image` at the VHDX and set `--hyperv-user Administrator`
and `CRABBOX_HYPERV_GUEST_PASSWORD=<password>`. The provider handles OpenSSH.

### Password-less templates (Windows dev-environment images)

Microsoft's downloadable Windows dev-environment VHDXs auto-log-on as `User`
with **no password**, and PowerShell Direct refuses empty credentials — so a
stock image fails the bootstrap as-is. Pass `--hyperv-init-password` to use one
unmodified:

```sh
set CRABBOX_HYPERV_GUEST_PASSWORD=<password>
crabbox warmup --provider hyperv --hyperv-image C:\Images\WinDev2407Eval.vhdx ^
  --hyperv-user User --hyperv-init-password
```

Before first boot the provider mounts the per-lease differencing disk, loads
its offline registry hive, and writes a `RunOnce` command that sets the guest
account's password to `CRABBOX_HYPERV_GUEST_PASSWORD` at the template's
auto-logon. Only the lease disk is modified; the template VHDX stays untouched.

Notes:

- Requires an explicit `CRABBOX_HYPERV_GUEST_PASSWORD` (the provider refuses
  to stamp its default password onto a guest).
- Neither the password nor the user name can contain `"` or `%` (both pass
  through `cmd.exe` at logon).
- This only works for templates that auto-log-on an administrator account
  (`RunOnce` fires at logon). Templates without auto-logon need a known
  password baked in, as above.
- The password is briefly visible inside the guest (the `RunOnce` registry
  value, then the `net.exe` command line at first logon). The guest belongs to
  the lease, and the differencing disk holding it is deleted on release.

## Configuration

`crabbox config show` displays loaded nonsecret Hyper-V settings in the `hyperv`
text line and JSON section, even when another provider is selected. It preserves
empty, zero and false values without resolving new defaults or invoking Hyper-V.
The guest password and credential-presence information are not displayed;
`initPassword` is only the configured boolean, not evidence of guest changes.

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--hyperv-image` | (none) | Path to the guest VHDX template (required) |
| `--hyperv-user` | `crabbox` | Guest account for SSH; letters, digits, `.`, `_`, and `-` only |
| `--hyperv-work-root` | `C:\crabbox` | Guest work root |
| `--hyperv-cpu` | `4` | Number of virtual CPUs |
| `--hyperv-memory` | `8192` | Memory in MB |
| `--hyperv-switch` | `Default Switch` | Hyper-V virtual switch name |
| `--hyperv-init-password` | `false` | Windows only: set the guest password at first boot via the lease disk |

Decoded negative CPU or memory values, including environment and explicit flag
values, are rejected when creating a VM, before native commands or local lease
state changes. Inherited sizing does not block stopping an existing lease or
cleaning up its resources; those operations retain their existing checks.
Zero retains the defaults
of 4 CPUs and 8192 MB. Positive memory values retain the existing binary
megabyte conversion, but values that overflow a signed 64-bit byte count are
rejected before native commands or local lease state changes. Other positive
sizing values are passed through unchanged. YAML CPU
and memory values still apply only when positive, so zero or negative YAML
values leave the previous setting unchanged.

### Config file

```yaml
hyperv:
  image: C:\Images\windows-crabbox.vhdx
  user: crabbox
  workRoot: C:\crabbox
  cpus: 4
  memory: 8192
  switch: Default Switch
  initPassword: false
```

For Windows, keep `CRABBOX_HYPERV_GUEST_PASSWORD` in the environment or trusted
user config, not repository config. There is no default guest password.

### Environment variables

| Variable | Description |
| --- | --- |
| `CRABBOX_HYPERV_IMAGE` | VHDX template path |
| `CRABBOX_HYPERV_USER` | SSH user inside the VM |
| `CRABBOX_HYPERV_WORK_ROOT` | Work root inside the VM |
| `CRABBOX_HYPERV_CPUS` | CPU count |
| `CRABBOX_HYPERV_MEMORY` | Memory in MB |
| `CRABBOX_HYPERV_SWITCH` | Virtual switch name |
| `CRABBOX_HYPERV_GUEST_PASSWORD` | Windows guest password for PowerShell Direct bootstrap |
| `CRABBOX_HYPERV_INIT_PASSWORD` | Windows only: set the guest password at first boot (`true`/`false`) |

## Bootstrap contract

All targets start with a per-lease differencing root VHDX over the configured
template and a Generation 2 VM.

### Windows

During Windows `Acquire`, the provider:

1. Creates the per-lease differencing disk backed by the template.
2. With `--hyperv-init-password`, mounts the lease disk offline and writes a
   first-boot `RunOnce` that sets the guest password (password-less templates)
3. Creates and starts the VM with its network adapter disconnected
4. Waits for a trivial authenticated PowerShell Direct call to succeed. The
   readiness loop has an overall boot budget and bounds each attempt so a guest
   that is still booting cannot hang acquisition.
5. Uses PowerShell Direct to stop/disable sshd, add an inbound TCP/22 block
   rule, replace authorized keys, discard template `Match` authentication
   blocks, and restrict SSH to the selected user
6. Connects the network adapter, installs the pinned Win32-OpenSSH MSI from
   GitHub if `sshd` is absent, and keeps sshd stopped behind the quarantine rule
7. Reapplies the final key-only config, validates `sshd_config`, regenerates
   per-lease SSH host keys, starts sshd, and removes the quarantine rule last
8. Installs pinned MinGit when git is absent, then ensures the shared Node/npm
   baseline
9. Waits for SSH readiness on the injected key, followed by repeated stable SSH probes

PowerShell Direct calls use the guest administrator password. Readiness and
later guest operations have bounded retries, preventing a stalled call from
hanging the lease.

Exhausting the PowerShell Direct boot-readiness budget reports a timeout while
preserving the last probe's diagnostic and public exit code. Caller cancellation
still takes precedence over that provider-owned budget.

## Pause and resume

`crabbox pause --provider hyperv <lease>` uses Hyper-V saved state so the VM
releases host CPU and memory while preserving guest state. A running VM is
saved with `Save-VM`; a Hyper-V `Paused` VM is also converted to saved state
because Hyper-V pause alone retains the VM's assigned memory. Pausing an
already saved lease is a no-op.

`crabbox resume --provider hyperv <lease>` starts a saved VM with `Start-VM`.
For a VM left in Hyper-V's native `Paused` state, Crabbox uses `Resume-VM`.
After either transition, Crabbox waits for DHCP and SSH readiness, then
atomically refreshes the local lease claim with the current IP address. Pause
and resume require an exact local claim for the VM; stopped or missing VMs are
reported as errors.

Resuming an already running VM skips the state transition but still checks
readiness and refreshes the endpoint. `pause --json` and `resume --json` return
the standard status receipt, with `state` set to `paused` or `ready` after a
successful transition and readback. `labels.hyperv_state` retains Hyper-V's
native state (`saved`, `paused`, or `running`).

Set `CRABBOX_HYPERV_GUEST_PASSWORD` or `hyperv.guestPassword` in trusted user
config to match the administrator password in your VHDX template. The provider
requires an explicit value and disables SSH password authentication after key
installation.

## Lifecycle

1. **Acquire**: Creates a per-lease differencing root disk and Generation 2 VM,
   then follows the Windows PowerShell Direct bootstrap described above.
2. **Resolve**: Finds a VM by lease ID, slug, or instance name and reports live
   Hyper-V state. Reuse requires a live VM and an exact local claim; a recovered
   legacy VM must be adopted with explicit `--reclaim`.
   Status and controller observations never adopt legacy claims or rewrite
   local claim/key state; inactive or incomplete leases remain metadata-only.
   Plain status also remains metadata-only for legacy claims; endpoint access and
   heartbeat require an exactly bound claim or explicit reclaim through reuse.
   `status --wait` can separately renew an acquired lease through guarded
   heartbeat; plain status does not renew it.
3. **List**: Includes Hyper-V VMs referenced by local provider claims plus
   unclaimed `crabbox-` prefixed candidates. A name prefix is inventory evidence,
   not ownership proof.
4. **Release**: Requires an exact local claim, performs guarded remote cleanup,
   stops/removes the VM, and deletes VM-owned root
   storage.
5. **Cleanup**: Deletes only exact-claimed VMs whose state or expiry makes them
   eligible, preserves `keep=true` VMs, and prunes missing
   local claims only after the provisioning grace period.

Heartbeats require an exact instance-scoped claim for a completed acquisition.
They atomically persist the touch timestamp and an explicit `--idle-timeout`
override; omitting the flag preserves the recorded timeout, even when current
configuration differs. Fresh status reads retain that policy and the original
lease TTL cap. A stale snapshot or incomplete acquisition is not renewed.
Ownership remains bound to the recorded VM name and instance scope.

The fixed operation budgets are:

| Operation | Budget |
| --- | --- |
| Acquisition, including bootstrap and SSH readiness | 25 minutes overall |
| Release or cleanup | 2 minutes overall |
| Ordinary host PowerShell command, including `Save-VM` and `Resume-VM` | 45 seconds |
| `Start-VM` | 5 minutes, then up to 45 seconds to reconcile a reported failure |
| Initial authenticated PowerShell Direct readiness | 10 minutes overall, 45 seconds per probe |
| Guest PowerShell Direct invocation | 10 minutes per attempt, at most 5 attempts within acquisition's overall budget |

After starting or resuming a guest, DHCP and SSH readiness use the shared
bootstrap wait budget: 20 minutes, or 45 minutes with desktop/browser enabled.
Each SSH probe is limited to 10 seconds or the remaining wait budget. Host
address queries retain their 45-second command limit; successful SSH readiness
is followed by a five-second address-stability check. Caller cancellation and
earlier enclosing deadlines still apply. These budgets do not change Hyper-V's
existing host-shutdown default (`AutomaticStopAction=Save`).

Cancellation before key creation does not manufacture a retained
lease. An ambiguous creation retains ownership and reports the blocked cleanup
instead of detaching a replacement VM's disk.

## Notes

- All VMs are named with a `crabbox-` prefix, but the prefix is not ownership
  proof. Cleanup and release require an exact local claim bound to the VM name;
  recovered VMs must first be adopted through an explicit `--reclaim` reuse.
- The selected SSH account must be a local account name containing only letters,
  digits, `.`, `_`, or `-`. Domain/UPN names and SSH pattern characters are
  rejected so `AllowUsers` cannot broaden access.
- VHD files are stored in `%USERPROFILE%\Hyper-V\Virtual Hard Disks\` by
  default. Only deterministically named provider-owned root
  disks are cleaned up; unrelated attached disks are preserved.
- The Windows ready check verifies `git`, `tar`, Node/npm, and the work root.
- There is no `tart exec` or `prlctl exec` equivalent for Hyper-V; all guest
  interaction after bootstrap happens over SSH.

## Examples

```powershell
crabbox warmup --provider hyperv --target windows `
  --hyperv-image C:\Images\win-server.vhdx
crabbox run --provider hyperv --target windows -- powershell -Command "Get-Process"
```

```sh
crabbox ssh --provider hyperv
crabbox stop --provider hyperv --id blue-lobster
crabbox cleanup --provider hyperv
```
