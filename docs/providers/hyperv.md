# Hyper-V

Provider id: `hyperv`
Kind: SSH lease
Targets: Linux, Windows (native)
Family: `local-vm`

## Overview

The Hyper-V provider creates and manages Linux or Windows virtual machines on a
local Windows host using Microsoft Hyper-V. VMs are provisioned as Generation 2
VMs from a target-appropriate VHDX template, connected to a configurable virtual
switch (default: "Default Switch"), and accessed over SSH. Windows normal mode
is the default target when `--target` is omitted.

| Capability | Linux | Windows normal |
| --- | --- | --- |
| SSH, Crabbox sync, cleanup | Yes | Yes |
| Pause and resume | Yes | Yes |
| Desktop and browser | Yes, through cloud-init | Yes, through PowerShell Direct |
| Workspace checkpoint, fork, restore, provider snapshot | Yes | Yes |
| Tailscale | Yes, through cloud-init | Yes, through PowerShell Direct |
| Cache volume | Yes | Yes |
| Code | Yes, through cloud-init and a local SSH tunnel | No |

Hyper-V must be enabled on the host (`Enable-WindowsOptionalFeature -Online
-FeatureName Microsoft-Hyper-V-All`). The provider is Windows-only and will
reject configuration on non-Windows hosts.

## Requirements

- Windows 10 Pro/Enterprise/Education or Windows Server with Hyper-V enabled
- PowerShell 5.1 or later (ships with Windows)
- A Generation 2 / UEFI VHDX template for the selected target
- For Linux, a generalized Debian or Ubuntu cloud VHDX with:
  - cloud-init NoCloud support
  - current Hyper-V integration services, including a running
    `hv_kvp_daemon` KVP service so Hyper-V can report guest IPs
    (Ubuntu requires `linux-cloud-tools-common` plus the versioned,
    kernel-flavour-specific `linux-cloud-tools` package matching `uname -r`)
  - the enabled Hyper-V VSS/file-system-freeze integration service backed by a
    running `hv_vss_daemon` so production checkpoints can quiesce filesystems
  - DHCP networking
  - guest internet access for first-boot apt packages and optional Tailscale
    installation
- For Windows, a VHDX with:
  - A local administrator account selected with `--hyperv-user`, with its
    password explicitly provided through `CRABBOX_HYPERV_GUEST_PASSWORD`
  - Network configured for DHCP on the Hyper-V virtual switch
  - Guest internet access to GitHub when OpenSSH or git must be installed on
    first use, and to `pkgs.tailscale.com` when Tailscale is requested
- The Hyper-V and Windows storage PowerShell modules

Linux support is initially limited to apt-based Debian and Ubuntu cloud images.
Crabbox does not convert QCOW2 or RAW images. The Linux template must already be
a generalized VHDX.

### Host permissions and doctor

Run Crabbox on the Hyper-V host from an account authorized to manage Hyper-V
VMs, firmware, switches, VHDXs, and storage. For Windows targets, membership in
the local `Hyper-V Administrators` group is sufficient and needs no elevation;
otherwise use an elevated shell.

Linux targets additionally require an elevated shell. Creating the NoCloud seed
disk mounts a VHDX, and `Mount-VHD` requires `SeManageVolumePrivilege`, which is
a volume-management right rather than a Hyper-V one. An unelevated
`Hyper-V Administrators` shell creates VMs, VHDXs, and switches successfully but
fails the seed step with `A required privilege is not held by the client`
(`0x80070522`), so group membership alone does not lift the requirement.

Do not run the host-side Crabbox process as `LocalSystem`. Session 0 is only a
convenience for **unattended** hosts, where failed early PowerShell Direct
authentication can display credential UI that no one dismisses. It is unrelated
to the guest interactive desktop that `--desktop` and `desktop proof` establish
inside the VM. A `LocalSystem` host process resolves a different profile, so SSH
known-hosts, Crabbox config, and `PATH` differ from the invoking user, per-user
tools such as `ffmpeg` are missing, leftover state becomes `LocalSystem`-owned
and needs elevation to remove, and elevation wrappers can corrupt captured
command output.

`crabbox doctor --provider hyperv` is non-mutating. It checks that the host is
Windows, queries the Hyper-V optional feature, inventories VMs, and reports the
configured image path. It does not validate that the image exists or boots,
that Windows credentials work, or that Linux KVP/VSS and optional browser
requirements are satisfied. `--probe-ssh` reports that a running lease is
required rather than creating one. Its Hyper-V optional-feature query calls
`Get-WindowsOptionalFeature`, which requires elevation even when provisioning
does not, so an unelevated `Hyper-V Administrators` shell reports that one check
as failed while acquire, proof, and release still work.

OpenSSH and git do **not** need to be pre-installed. On first acquire the
provider installs the pinned, SHA-256-verified Win32-OpenSSH MSI used by the
`Microsoft.OpenSSH.Preview` winget package and, if absent, portable MinGit
(also pinned and SHA-256-verified). Both installs are no-ops when already
present, so a template that pre-bakes inbox/FoD OpenSSH, the MSI version, or git
skips the matching per-lease download. OpenSSH bootstrap does not depend on
Windows Update, WSUS, or a matching Features on Demand source. This keeps the
template requirement to a plain Windows VHDX with a known admin password. ISO
images are not supported — provide a fully installed VHDX.

Windows acquisition allows up to 15 minutes overall, including a five-minute
authenticated PowerShell Direct readiness budget after VM creation. An external
five-minute timeout can terminate acquisition before that readiness budget
finishes. Bootstrap progress on stderr distinguishes waiting for authentication
from completed credential escrow, SSH lockdown, and network connection. A booted
VM or a started network adapter alone does not prove guest authentication.

## Linux code-server

Hyper-V advertises the `code` capability only for Linux targets. Create the VM
with `--code` so cloud-init installs the pinned, checksum-verified code-server
binary and includes it in the `crabbox-ready` check:

```powershell
crabbox warmup --provider hyperv --target linux `
  --hyperv-image 'C:\Images\debian-cloud.vhdx' `
  --code
crabbox code --provider hyperv --id <lease-id-or-slug> --open
```

Hyper-V has coordinator policy `never`, so `crabbox code` does not require a
broker login. It starts code-server on guest `127.0.0.1:8080`, creates an SSH
local forward bound to host `127.0.0.1:<port>`, prints or opens the resulting
local HTTP URL, and remains attached until canceled. `--auth none` is safe in
this path because neither code-server nor the local listener binds to a
non-loopback interface.

The persisted lease claim records `code=true`. Reusing a lease for
`crabbox code` requires that label, so a VM created without `--code` is rejected
instead of attempting an untracked in-place capability upgrade. Windows targets
do not advertise `code` and remain explicitly unsupported.

## Desktop and browser capabilities

`--desktop` installs the pinned, SHA-256-verified TightVNC server after the
OpenSSH and git bootstrap completes. TightVNC is configured without a firewall
exception and restricted to loopback; connect through Crabbox's SSH-backed VNC
or WebVNC commands. The guest account password is passed to PowerShell Direct
through the host process environment and a remoting argument, never on the
host command line. TightVNC receives a separate generated password stored at
`C:\ProgramData\crabbox\vnc.password`.

The desktop setup enables auto-logon and performs one expected Windows reboot.
Acquisition waits for bounded PowerShell Direct readiness, reruns the marked
idempotent setup after reboot, then waits for SSH, loopback VNC, and an active
interactive session for the configured `--hyperv-user` with a running Explorer
shell. TightVNC service readiness alone is not desktop readiness. If auto-logon
settings were applied to an already-marked template but no usable session
appears, acquisition performs at most two explicit bounded reboot retries,
reapplies the idempotent setup, waits again, and requires repeated stable SSH
probes.
`desktop=true` is recorded only after all checks succeed.

`--browser` is probe-only. After final SSH readiness, Crabbox accepts an
existing Microsoft Edge or Google Chrome installation. It does not install a
browser, and acquisition fails with a capability error when neither browser is
present. The `browser=true` label is recorded only after the probe succeeds.

For native Windows screenshot, video, and `desktop proof`, use a **fresh**
desktop lease acquired by the pinned Crabbox binary. Retained leases are not
backfilled with the credential/session contract. Git for Windows supplies
Mintty, and MP4 encoding requires local `ffmpeg` on the Hyper-V host:

```powershell
& $crabboxPath warmup --provider hyperv --target windows --desktop --keep `
  --hyperv-image 'E:\vhd\windows-template.vhdx' `
  --hyperv-user crabbox --hyperv-switch 'Default Switch'

ffmpeg -version
& $crabboxPath desktop proof --provider hyperv --id <lease> `
  --output 'C:\proof\<lease>' -- cmd.exe /d /c "echo desktop-proof"

& $crabboxPath stop --provider hyperv <lease>
```

Run this provider flow as the invoking interactive user, not as `LocalSystem`.
The interactive session the proof requires is the guest session belonging to the
SSH user, which the proof launcher targets; the host-side session is irrelevant.
Screenshot and video capture use short-lived interactive tasks in per-capture
directories protected for only the lease user, Builtin Administrators, and
LocalSystem. Normal completion and failure paths delete the tasks, scripts,
frames, archives, and protected directories; always stop the lease to remove the
VM and differencing disk.

Crabbox copies any PowerShell payload larger than 4 KB to the guest and runs it
with a short argument-based command, because Windows OpenSSH intermittently
wedges its per-connection event loop while forwarding a large stdin payload;
when that happens the guest never finishes reading stdin and ssh aborts with
`Timeout, server <host> not responding` even though the guest is healthy.

Actions runner, screenshot, and video task setup decode
`windows.password` as strict UTF-8, preserve Unicode plus leading/trailing
whitespace exactly, and reject only a zero-length file. The credential is never
trimmed, logged, or added to native process arguments.

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

### Preparing a Linux template

Use a generalized Debian or Ubuntu cloud VHDX that boots as a Generation 2 VM
with DHCP, cloud-init, and the Hyper-V KVP integration daemon enabled. On
Debian this is commonly supplied by `hyperv-daemons`; Ubuntu cloud images
commonly use the matching `linux-cloud-tools-virtual` and
`linux-cloud-tools-common` packages. Package names vary by image and kernel, so
verify that `hv_kvp_daemon` is running before generalizing the VHDX. Do not bake
a Crabbox password into the image. At acquire time Crabbox creates the selected
`--hyperv-user`, injects a per-lease SSH public key through cloud-init, and
disables SSH password authentication.

The VM starts with networking connected because cloud-init installs required
apt packages on first boot. The selected virtual switch must therefore provide
DHCP and internet access until `crabbox-ready` succeeds.

### Tailscale

Both targets support `--tailscale` and the standard hostname, tags, exit-node,
and LAN-access settings. Crabbox keeps using the DHCP endpoint until guest
metadata reports `state=ready`; `--network tailscale` never switches to a
requested-but-not-ready hostname.

When an exit node is requested with LAN access disabled, Hyper-V defers
activating the exit node until Crabbox has selected the ready tailnet SSH
endpoint. This preserves DHCP-based first-boot readiness without weakening the
requested steady-state setting.

Linux reuses the normal cloud-init Tailscale bootstrap. The auth key exists only
in the temporary NoCloud payload; a post-`cloud-final` cleanup removes cloud-init
guest caches containing that payload, and the seed is detached and deleted after
key-only SSH readiness succeeds. The cleanup disables cloud-init on the
per-lease disk after first-boot provisioning so later reboots do not need the
removed NoCloud datasource. Metadata remains under
`/var/lib/crabbox/tailscale-*`.

Linux checkpoint forks never place the current auth key in the specialization
seed or checkpoint metadata. Offline specialization clears copied Tailscale
identity with the shared target-aware reset contract. After the network is
connected and SSH is ready, Crabbox sends the fresh key over SSH stdin, rejoins
the fork, and only then attaches the new lease's requested cache volumes.

Windows installs the pinned official Tailscale MSI only when `tailscale.exe` is
absent and verifies its SHA-256 before invoking `msiexec`. The auth key is passed
from the host process environment into a PowerShell Direct remoting argument,
then written to a short-lived guest file for `tailscale up`; it is not placed in
host argv, lease claims, labels, logs, or checkpoint metadata. Windows metadata
is stored under `C:\ProgramData\crabbox\tailscale`.

Release attempts `tailscale logout` before deleting a running VM. Pause/resume
preserves the Tailscale state and node identity. Windows checkpoint forks remove
the copied Tailscale state and Crabbox metadata before reconnecting the fork.
Linux checkpoint forks clear copied identity during offline specialization and
rejoin with the current auth key over SSH after network readiness.

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
| `--hyperv-work-root` | target-specific | `C:\crabbox` for Windows or `/work/crabbox` for Linux |
| `--hyperv-cpu` | `4` | Number of virtual CPUs |
| `--hyperv-memory` | `8192` | Memory in MB |
| `--hyperv-switch` | `Default Switch` | Hyper-V virtual switch name |
| `--hyperv-secure-boot` | `auto` | `auto`, `windows`, `linux`, or `off` |
| `--hyperv-init-password` | `false` | Windows only: set the guest password at first boot via the lease disk |

`auto` selects the Microsoft Windows secure boot template for Windows targets
and the Microsoft UEFI Certificate Authority template for Linux targets.

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
  secureBoot: auto
  initPassword: false
```

For Windows, keep `CRABBOX_HYPERV_GUEST_PASSWORD` in the environment or trusted
user config, not repository config. There is no default guest password. Linux
does not use this password and rejects `hyperv.initPassword`.

### Environment variables

| Variable | Description |
| --- | --- |
| `CRABBOX_HYPERV_IMAGE` | VHDX template path |
| `CRABBOX_HYPERV_USER` | SSH user inside the VM |
| `CRABBOX_HYPERV_WORK_ROOT` | Work root inside the VM |
| `CRABBOX_HYPERV_CPUS` | CPU count |
| `CRABBOX_HYPERV_MEMORY` | Memory in MB |
| `CRABBOX_HYPERV_SWITCH` | Virtual switch name |
| `CRABBOX_HYPERV_SECURE_BOOT` | Secure boot mode: `auto`, `windows`, `linux`, or `off` |
| `CRABBOX_HYPERV_GUEST_PASSWORD` | Windows guest password for PowerShell Direct bootstrap |
| `CRABBOX_HYPERV_INIT_PASSWORD` | Windows only: set the guest password at first boot (`true`/`false`) |

## Bootstrap contract

All targets start with a per-lease differencing root VHDX over the configured
template and a Generation 2 VM.

### Linux

During Linux `Acquire`, the provider:

1. Generates cloud-init from the shared `CloudInitUserData` implementation,
   including the per-lease SSH key and requested desktop/browser bootstrap.
2. Creates a small dynamic VHDX, formats it as FAT with label `cidata`, and
   writes UTF-8 no-BOM `user-data` and `meta-data`.
3. Creates the VM, attaches the seed as a secondary SCSI disk, applies the
   selected secure boot firmware, connects networking, and starts the VM.
4. Discovers the DHCP address through `Get-VMNetworkAdapter`.
5. Waits for key-only SSH and `/usr/local/bin/crabbox-ready`.
6. Detaches and deletes the seed VHDX after readiness.
7. Attaches requested cache VHDXs and initializes/mounts them over SSH.

Linux guest commands are never run through PowerShell Direct.

### Windows

During Windows `Acquire`, the provider:

1. Creates the per-lease differencing disk backed by the template.
2. With `--hyperv-init-password`, mounts the lease disk offline and writes a
   first-boot `RunOnce` that sets the guest password (password-less templates)
3. Creates and starts the VM with its network adapter disconnected
4. Waits for a trivial authenticated PowerShell Direct call to succeed. The
   readiness loop has an overall boot budget and bounds each attempt so a guest
   that is still booting cannot hang acquisition.
5. Escrows the exact guest password at
   `C:\ProgramData\crabbox\windows.password` for durable GitHub Actions runner
   startup. The password reaches the host PowerShell process only through
   `_CRABBOX_GP` and reaches the guest only through the remoting argument list.
   Crabbox writes UTF-8 without a BOM or newline to an empty temporary file
   after applying a fresh protected ACL containing only the selected local user,
   Builtin Administrators, and LocalSystem, then atomically replaces the target.
   The password is never added to lease claims, labels, or logs.
6. Uses PowerShell Direct to stop/disable sshd, add an inbound TCP/22 block
   rule, replace authorized keys, discard template `Match` authentication
   blocks, and restrict SSH to the selected user
7. Connects the network adapter, installs the pinned Win32-OpenSSH MSI from
   GitHub if `sshd` is absent, and keeps sshd stopped behind the quarantine rule
8. Reapplies the final key-only config, validates `sshd_config`, regenerates
   per-lease SSH host keys, starts sshd, and removes the quarantine rule last
9. Installs full, pinned Git for Windows (including Mintty) for `--desktop`;
   otherwise installs pinned MinGit when git is absent
10. With `--desktop`, installs/configures loopback-only TightVNC, handles its
    expected setup reboot through bounded PowerShell Direct retries, and reruns
    the marked setup idempotently
11. Waits for SSH readiness on the injected key and, for `--desktop`, loopback
    VNC plus an active Explorer-backed session for the configured guest user,
    followed by repeated stable SSH probes.
    When auto-logon settings have not taken effect, it performs at most two
    explicit reboot retries and repeats the bounded readiness checks
12. Attaches requested cache VHDXs and initializes/mounts them through bounded
    PowerShell Direct
13. With `--browser`, probes for Edge or Chrome without installing either
14. Records successful desktop/browser/cache lease labels only after all requested
    readiness checks pass

PowerShell Direct calls use the guest administrator password. Readiness and
later guest operations have bounded retries, preventing a stalled call from
hanging the lease.

Exhausting the PowerShell Direct boot-readiness budget reports a timeout while
preserving the last probe's diagnostic and public exit code. Caller cancellation
still takes precedence over that provider-owned budget.

## Cache volumes

Hyper-V supports persistent cache volumes for Linux and Windows normal targets.
They are rebuildable speed-only state, not source, artifacts, secrets, synced
workspace data, or checkpoint state.

Each key maps to a provider-owned dynamic VHDX under:

```text
%USERPROFILE%\Hyper-V\Crabbox Cache Volumes\
  cache-<key-sha256-prefix>.vhdx
  cache-<key-sha256-prefix>.json
  cache-<key-sha256-prefix>.lock
```

The digest-derived name prevents keys from becoming host paths. The JSON
sidecar records the original key, target, filesystem, disk identity, size, and
the Linux filesystem UUID after first format. Reusing a key with an incompatible
target/filesystem is rejected.

Writable VHDXs are single-writer. Before attach, Crabbox briefly holds the
per-key host lock and checks all live Hyper-V disk attachments. The lock only
serializes create/format/attach and checkpoint detach/reattach races; it is not
treated as lease-lifetime ownership. A required busy volume fails acquire. An
optional busy volume warns and is omitted from the lease claim.

- Windows disks are initialized once as GPT/NTFS, found by persistent disk ID,
  and mounted at the configured drive-rooted directory without consuming a
  drive letter. The directory's drive must already exist in the guest; use
  `C:\crabbox-cache\<kind>` for ordinary single-disk templates.
- Linux disks are initialized once as ext4 after cloud-init/SSH readiness,
  mounted by UUID at the configured POSIX path, and added idempotently to
  `/etc/fstab`.

Mount paths must be outside the synced work root and cannot be a drive or
filesystem root. Only successfully mounted `key:path` values are recorded in
the local claim, so required reuse must prove the attachment.

Release removes the VM attachment but preserves the cache VHDX and metadata.
Normal provider cleanup never treats the cache root as lease-owned storage and
does not delete it. `crabbox cache purge` can clear cache contents on an attached
lease; there is no implicit destructive cache-volume purge.

## Workspace checkpoints

Workspace checkpoint, fork, restore, and provider snapshot capabilities are
available for Linux and Windows normal leases. Both targets use
the native checkpoint kind `hyperv-vhdx` and
`CheckpointType ProductionOnly`; Crabbox never permits Hyper-V to fall back to
a standard checkpoint. Before creating a Linux checkpoint, Crabbox verifies
that the VM's VSS integration service is enabled and reports `OK`. A missing,
disabled, unreachable, or protocol-mismatched service fails with guidance to
start `hv_vss_daemon`.

Checkpoint exports remain owned by the checkpoint artifact directory and
survive release of the source lease. Restore is source-only: it verifies the
exact original VM identity and lease claim, applies the production checkpoint,
re-resolves DHCP/SSH, and refreshes the claim without changing the source
hostname or SSH identity.

Before any checkpoint disk mutation, Crabbox durably journals the source VM
identity, unique snapshot name, and owned export directory. A journal write
failure aborts before mutation; an interrupted export retains its cleanup
identity rather than authorizing another checkpoint submission.

Windows forks retain the PowerShell Direct identity-rotation flow. Linux forks
use an offline NoCloud specialization sequence because an exported checkpoint
contains copied SSH login and host keys, hostname, cloud-init state, and
possibly Tailscale identity:

1. Import the exported VM with a generated Hyper-V VM ID and dynamic MAC while
   leaving its network adapter disconnected and the VM powered off.
2. Create and attach a separate `cidata` VHDX with a fresh NoCloud instance ID.
   Reapply the exact source secure-boot enabled state and template recorded at
   checkpoint creation rather than using the current host configuration.
3. Boot while disconnected. The one-shot specialization replaces every copied
   `authorized_keys` file with the new lease key, regenerates SSH host keys,
   assigns the new hostname and machine ID, stops and clears Tailscale identity
   state, removes copied source cloud-init instance directories while preserving
   the new NoCloud instance state, and writes
   `/var/lib/crabbox/checkpoint-fork-specialized` plus an instance-specific
   completion marker on the seed disk.
4. Cloud-init powers the guest off. Crabbox waits for Hyper-V to report the
   host-visible `Off` state, detaches the seed while networking is still
   disconnected, and mounts the FAT seed on the host to verify that the marker
   matches the new NoCloud instance ID. Guest network signals are not trusted
   for this phase.
5. Delete the verified specialization seed, connect the configured switch,
   start the VM, and wait for DHCP and SSH/`crabbox-ready`.
6. Rejoin Tailscale with the fresh current auth key over SSH stdin, attach only
   the new lease's successfully mounted cache volumes, and persist the new exact
   claim and endpoint.

The checkpoint metadata key `linux_fork_specialization` versions this offline
contract. `nocloud-v2` denotes the target-aware Tailscale reset plus post-network
rejoin and new-lease-only cache attachment sequence.

Before checkpoint create/export or restore, Hyper-V unmounts and detaches every
recorded cache disk while holding its short host lock, then reattaches and
remounts it on both success and failure. Windows uses PowerShell Direct for the
guest mount transition; Linux uses SSH and verifies the recorded filesystem
UUID. While a disk is detached, Crabbox replaces its guest mount directory with
a protected marker file so workloads cannot write into the underlying root
disk. Linux also removes the matching `/etc/fstab` entry; reattachment removes
the marker and replaces any stale entries for that mount path with the current
cache UUID. Cache disks are excluded from exported workspace state.

Fork import removes inherited provider cache disks before offline
specialization, then attaches only the new lease's requested caches after
network readiness. Only successful attachments are recorded in the new claim.
If a cache-backed source lease began in Hyper-V saved or paused state, Crabbox
temporarily resumes it for safe guest unmounts and restores the exact original
state after checkpoint create or restore completes, including failure cleanup.

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

Set `CRABBOX_HYPERV_GUEST_PASSWORD` or `hyperv.guestPassword` in trusted user
config to match the administrator password in your VHDX template. The provider
requires an explicit value and disables SSH password authentication after key
installation.

## Lifecycle

1. **Acquire**: Creates a per-lease differencing root disk and Generation 2 VM,
   applies target-aware secure boot, then follows the Linux cloud-init or
   Windows PowerShell Direct bootstrap described above. Credential escrow
   failure aborts acquisition and follows the normal `keep=true` preserve or
   default VM/differencing-disk cleanup policy.
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
   detaches recorded cache volumes, attempts Tailscale logout, stops/removes the
   VM, and deletes VM-owned root, internal checkpoint, and NoCloud seed storage.
   Exported checkpoint artifacts and persistent cache VHDXs survive release.
5. **Cleanup**: Deletes only exact-claimed VMs whose state or expiry makes them
   eligible, preserves `keep=true` and restore-reserved VMs, and prunes missing
   local claims only after the provisioning grace period.

Heartbeats require an exact instance-scoped claim for a completed acquisition.
They atomically persist the touch timestamp and an explicit `--idle-timeout`
override; omitting the flag preserves the recorded timeout, even when current
configuration differs. Fresh status reads retain that policy and the original
lease TTL cap. A stale snapshot or incomplete acquisition is not renewed.
Ownership remains bound to the recorded VM name and instance scope. Newly
acquired Linux leases additionally bind the immutable Hyper-V VM ID and a
private `linux-v1` storage directory with an exact lease-owner marker. Reuse and
release reject a changed VM ID; cleanup rejects conflicting owners or unexpected
storage contents rather than deleting them. Existing checkpoint forks retain
their checkpoint-specific identity and storage safeguards.

Acquisition is bounded to 15 minutes, cleanup to 2 minutes, and ordinary host
PowerShell commands to 45 seconds. NoCloud seed creation has its own 2-minute
budget; guest PowerShell Direct keeps its separate readiness and invocation
budgets. Cancellation before key creation does not manufacture a retained
lease. Once ownership is persisted, failed kept Linux acquisitions preserve
their claim for explicit recovery. They retain a non-Tailscale seed; when the
seed contains Tailscale credentials, cleanup detaches and deletes it only after
verifying the exact VM identity. An ambiguous creation retains ownership and
reports the blocked cleanup instead of detaching a replacement VM's disk.

## Notes

- All VMs are named with a `crabbox-` prefix, but the prefix is not ownership
  proof. Cleanup and release require an exact local claim bound to the VM name;
  recovered VMs must first be adopted through an explicit `--reclaim` reuse.
- The selected SSH account must be a local account name containing only letters,
  digits, `.`, `_`, or `-`. Domain/UPN names and SSH pattern characters are
  rejected so `AllowUsers` cannot broaden access.
- Durable native-Windows GitHub Actions runner startup depends on the protected
  credential escrow created during a fresh acquisition. Retained Windows leases
  acquired by older Crabbox builds are not backfilled during resolve or
  hydration; release and recreate them before using `--github-runner`.
- VHD files are stored in `%USERPROFILE%\Hyper-V\Virtual Hard Disks\` by
  default. Only deterministically named provider-owned root, checkpoint, and
  seed disks are cleaned up; unrelated attached disks are preserved.
- New Linux leases keep `disk.vhdx`, `seed.vhdx`, and an owner marker in a private
  directory named for the VM. NoCloud source files are private, temporary, and
  removed after seed creation. A persisted mount-intent marker authorizes
  recovery of an interrupted host-side seed mount; it does not authorize
  dismounting a kept guest's attached seed. Immediately before attaching a seed,
  Crabbox grants SYSTEM and Administrators access and checks the ACL command's
  exit code, because mounting a VHDX can replace its file ACL.
- The Windows ready check verifies `git`, `tar`, and the work root. The Linux
  ready check executes `/usr/local/bin/crabbox-ready`.
- There is no `tart exec` or `prlctl exec` equivalent for Hyper-V; all guest
  interaction after bootstrap happens over SSH.

## Examples

```powershell
crabbox warmup --provider hyperv --target windows `
  --hyperv-image C:\Images\win-server.vhdx
crabbox run --provider hyperv --target windows -- powershell -Command "Get-Process"
```

```powershell
crabbox warmup --provider hyperv --target linux `
  --hyperv-image C:\Images\ubuntu-cloud.vhdx `
  --hyperv-secure-boot auto
crabbox run --provider hyperv --target linux -- uname -a
```

```sh
crabbox ssh --provider hyperv
crabbox stop --provider hyperv --id blue-lobster
crabbox cleanup --provider hyperv
```
