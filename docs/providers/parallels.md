# Parallels Provider

Read this when you:

- choose `provider: parallels`;
- run Crabbox on local or remote Parallels Desktop VMs;
- clone Linux, macOS, or Windows templates from known-good snapshots;
- change `internal/providers/parallels` or Parallels checkpoint behavior.

Parallels is a direct SSH-lease provider (it never goes through the broker). To
acquire a box, Crabbox asks `prlctl` to create a clone from a configured source
VM and snapshot, starts the clone, discovers the guest IP, injects the per-lease
SSH key through Parallels Tools, and then uses the normal Crabbox SSH
sync/run/checkpoint path.

The provider is local-first: by default it drives the `prlctl` on the same Mac
that runs Crabbox. Set `parallels.host` to drive a Parallels Desktop install on
another Mac over SSH.

**Targets:** Linux, macOS, and Windows (`--windows-mode normal` or
`--windows-mode wsl2`).

**Capabilities:** SSH, Crabbox sync, cleanup, desktop, browser, code, plus
native checkpoint/fork/restore/snapshot support backed by Parallels snapshots.

## Quick start

The recommended operator UX is a named template alias: a template points at a
source VM plus a known-good snapshot, so day-to-day commands run the normal
Crabbox flows without juggling source/snapshot flags.

```sh
crabbox warmup \
  --provider parallels \
  --target macos \
  --parallels-source "macOS Tahoe" \
  --parallels-source-snapshot fresh \
  --parallels-user alice \
  --ssh-port 22

crabbox run --provider parallels --id blue-lobster -- xcodebuild -version
crabbox checkpoint create --provider parallels --id blue-lobster --mode native --name xcode-ready
crabbox checkpoint fork chk_abc123 --provider parallels
crabbox stop --provider parallels blue-lobster
```

With a template alias:

```sh
crabbox checkpoint list --provider parallels --parallels-template tahoe-latest
crabbox checkpoint fork --provider parallels --parallels-template tahoe-latest --slug tahoe-test
crabbox run --provider parallels --parallels-template tahoe-latest -- xcodebuild -version
```

Use `--target linux`, `--target macos`, or `--target windows`. For Windows,
choose `--windows-mode normal` for PowerShell/OpenSSH or `--windows-mode wsl2`
when the template already exposes a working WSL2 environment through Windows
OpenSSH.

## Template requirements

Each source VM should already include:

- Parallels Tools (used to discover the guest IP and inject the per-lease key);
- a stable guest SSH user;
- an OpenSSH server listening on `ssh.port`;
- Crabbox sync tools for the target OS (`git`, `rsync` or archive sync tools,
  and a shell/PowerShell);
- a known-good power-off snapshot for fast linked clones.

Linked clones require an explicit power-off snapshot. Crabbox rejects linked
clone requests without `parallels.sourceSnapshot`/`sourceSnapshotId`, because
otherwise `prlctl` would create a source-side "Snapshot for linked clone" on the
template VM. Use `cloneMode: full` or `cloneMode: unlink` only when you
intentionally want to clone the current source VM state without a snapshot.
Parallels also refuses to clone from a busy source VM, so keep template VMs shut
down when they serve as local fleet bases.

For macOS templates, use a user with SSH login permission and a writable
`parallels.workRoot`, for example `/Users/<user>/crabbox`. For Windows native
templates, configure OpenSSH Server and PowerShell. For Windows WSL2 templates,
make sure `wsl.exe` works for the SSH user.

## Configuration

```yaml
provider: parallels
target: macos
ssh:
  port: "22"
parallels:
  source: macOS Tahoe
  sourceSnapshot: fresh
  cloneMode: linked
  user: alice
  workRoot: /Users/alice/crabbox
  startupTimeout: 15m
```

### Named templates

```yaml
provider: parallels
parallels:
  templates:
    tahoe-latest:
      target: macos
      source: macOS Tahoe
      sourceSnapshot: macOS 26.3.1 LATEST
      user: alice
      workRoot: /Users/alice/crabbox
    ubuntu-fast:
      target: linux
      source: Ubuntu 25.10
      sourceSnapshot: fresh-poweroff-2026-03-17
      user: alice
      workRoot: /work/crabbox
```

A template may set `source`, `sourceId`, `sourceSnapshot`, `sourceSnapshotId`,
`target`, `windowsMode`, `cloneMode`, `host`, `hostUser`, `hostKey`, `vmRoot`,
`user`, and `workRoot`. Explicit command-line flags override the selected
template.

### Remote Mac host

```yaml
provider: parallels
target: linux
parallels:
  host: mac-studio.tailnet
  hostUser: crabbox
  hostKey: ~/.ssh/id_ed25519
  source: Ubuntu 25.10
  sourceSnapshot: fresh-poweroff
  user: crabbox
  workRoot: /work/crabbox
```

When `parallels.host` is set, Crabbox runs `prlctl` over SSH on that Mac and
reaches the guest IP through an SSH `ProxyCommand` via the host. Normal SSH
commands, desktop input helpers, screenshots, and VNC tunnels all use the same
proxy path.

A repository-defined remote host, including one selected through `templates`
or `hosts`, cannot silently inherit a host key or ambient SSH authentication
from a more trusted source. Define the remote host and a relative,
symlink-resolved key file contained by the repository together, or approve the
destination explicitly with `--parallels-host` or `CRABBOX_PARALLELS_HOST`.
Absolute, missing, and repository-escaping key paths require explicit host
approval.

### Fleet hosts

```yaml
provider: parallels
parallels:
  hosts:
    - name: local
      targets: [linux, macos, windows]
      maxVMs: 4
    - name: mac-host
      host: mac-host.example.net
      user: alice
      targets: [linux, macos]
      maxVMs: 3
  templates:
    ubuntu-fast:
      target: linux
      source: Ubuntu 25.10
      sourceSnapshot: fresh-poweroff-2026-03-17
      user: alice
```

When `hosts` is configured, Crabbox checks each host whose `targets` match the
requested target, looks for the requested source VM, and picks the first host
below its `maxVMs` limit. Host selection applies to `warmup`, `run`,
`checkpoint fork`, `status`, `list`, `stop`, and `cleanup`.

### Environment variables

```text
CRABBOX_PARALLELS_SOURCE
CRABBOX_PARALLELS_SOURCE_ID
CRABBOX_PARALLELS_SOURCE_SNAPSHOT
CRABBOX_PARALLELS_SOURCE_SNAPSHOT_ID
CRABBOX_PARALLELS_TEMPLATE
CRABBOX_PARALLELS_CLONE_MODE
CRABBOX_PARALLELS_HOST
CRABBOX_PARALLELS_HOST_USER
CRABBOX_PARALLELS_HOST_KEY
CRABBOX_PARALLELS_VM_ROOT
CRABBOX_PARALLELS_USER
CRABBOX_PARALLELS_WORK_ROOT
CRABBOX_PARALLELS_STARTUP_TIMEOUT
```

Provider flags mirror the same fields (`--parallels-source`,
`--parallels-source-snapshot`, `--parallels-template`, `--parallels-host`, and
so on) and never carry passwords.

## Checkpoints

Native Parallels checkpoints are backed by Parallels snapshots:

```sh
crabbox checkpoint list --provider parallels --id "macOS Tahoe"
crabbox checkpoint list --provider parallels --id "macOS Tahoe" --forkable-only
crabbox checkpoint list --provider parallels --parallels-template tahoe-latest --current
crabbox checkpoint create --provider parallels --id blue-lobster --mode native --name after-xcode-setup
crabbox checkpoint fork chk_abc123 --provider parallels --slug test-a
crabbox checkpoint restore chk_abc123 --provider parallels --id blue-lobster
crabbox checkpoint delete chk_abc123
```

Existing Parallels snapshots do not need to be imported; reference them directly
by source VM and snapshot name:

```sh
crabbox checkpoint fork --provider parallels --target macos --id "macOS Tahoe" --snapshot "macOS 26.4" --slug tahoe-test
crabbox checkpoint fork --provider parallels --parallels-template ubuntu-fast --dry-run
crabbox checkpoint restore --provider parallels --id "macOS Tahoe" --snapshot "macOS 26.3.1 LATEST"
crabbox checkpoint restore --provider parallels --id blue-lobster --snapshot "known-good" --dry-run
crabbox checkpoint delete --provider parallels --id blue-lobster --snapshot "crabbox-test-snap"
```

- `fork` creates a linked clone from the recorded source VM and snapshot.
- `restore` switches an existing Parallels lease back to the recorded snapshot.
- `delete` removes only the recorded snapshot, not the source VM. Direct
  snapshot delete refuses names that do not start with `crabbox-` unless `--yes`
  is supplied, because known-good snapshots are usually hand-managed template
  state.

Linked clones depend on the source VM and snapshot. Keep known-good template VMs
and their base snapshots while any checkpoint or clone depends on them.

## Safety

Crabbox refuses to delete a Parallels VM unless an exact local claim binds the
lease to the VM ID and selected Parallels host. A `crabbox-` name alone is not
ownership proof. `stop` and `cleanup` skip unclaimed or mismatched clones;
intentionally recovered clones must first be adopted through an explicit
`--reclaim` reuse.

Use `--dry-run` on direct fork, restore, and delete when validating a template
or snapshot name. `checkpoint list` prints live Parallels state and marks
whether each snapshot is forkable: power-on snapshots can be restored in place,
while linked-clone forks require a power-off snapshot.

## Related docs

- [Provider overview](README.md)
- [Checkpoints](../features/checkpoints.md)
- [Static SSH](ssh.md)
