# Parallels Provider

Read when:

- choosing `provider: parallels`;
- running Crabbox on local Parallels Desktop VMs;
- cloning Linux, macOS, or Windows templates from known-good snapshots;
- changing `internal/providers/parallels` or Parallels checkpoint behavior.

Parallels is a direct SSH lease provider. Crabbox asks `prlctl` to create a
linked clone from a configured source VM and optional snapshot, starts the clone,
discovers the guest IP, injects the per-lease SSH key with Parallels Tools, then
uses the normal Crabbox SSH sync/run/checkpoint path.

The provider is local-first. Set `parallels.host` when the Parallels Desktop
install lives on another Mac reachable over SSH.

The recommended operator UX is a named template alias. A template points at a
source VM plus a known-good snapshot. Commands then use the same normal Crabbox
flows without a separate Parallels command.

## Quick Start

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

Template alias:

```sh
crabbox checkpoint list --provider parallels --parallels-template tahoe-latest
crabbox checkpoint fork --provider parallels --parallels-template tahoe-latest --slug tahoe-test
crabbox run --provider parallels --parallels-template tahoe-latest -- xcodebuild -version
```

Use `--target linux`, `--target macos`, or `--target windows`. Windows can use
`--windows-mode normal` for PowerShell/OpenSSH or `--windows-mode wsl2` when the
template already exposes a working WSL2 environment through Windows OpenSSH.

## Template Requirements

Each template should already include:

- Parallels Tools;
- a stable guest SSH user;
- OpenSSH server listening on `ssh.port`;
- Crabbox sync tools for the target OS (`git`, `rsync` or archive sync tools,
  shell/PowerShell);
- a known-good Parallels snapshot for fast linked clones.

Linked clones require a power-off snapshot. Parallels also refuses to clone from
a busy source VM, so keep template VMs shut down when they are used as local
fleet bases.

For macOS templates, use a user with SSH login permission and a writable
`parallels.workRoot`, for example `/Users/<user>/crabbox`. For Windows native
templates, configure OpenSSH Server and PowerShell. For Windows WSL2 templates,
make sure `wsl.exe` works for the SSH user.

## Config

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

Named templates:

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

Template values can set `source`, `sourceId`, `sourceSnapshot`,
`sourceSnapshotId`, `target`, `windowsMode`, `cloneMode`, `host`, `hostUser`,
`hostKey`, `vmRoot`, `user`, and `workRoot`. Explicit command-line flags
override the template.

Remote Mac host:

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
uses SSH `ProxyCommand` through the host to reach the guest IP. Normal SSH
commands, desktop input helpers, screenshots, and VNC tunnels all use that same
proxy path.

Fleet hosts:

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

When `hosts` is configured, Crabbox checks each matching host for the requested
source VM and picks the first host below `maxVMs`. The host list works for
`warmup`, `run`, `checkpoint fork`, `status`, `list`, `stop`, and `cleanup`.

Environment:

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

Provider flags mirror the same fields and never carry passwords.

## Checkpoints

Native Parallels checkpoints use Parallels snapshots:

```sh
crabbox checkpoint list --provider parallels --id "macOS Tahoe"
crabbox checkpoint list --provider parallels --id "macOS Tahoe" --forkable-only
crabbox checkpoint list --provider parallels --parallels-template tahoe-latest --current
crabbox checkpoint create --provider parallels --id blue-lobster --mode native --name after-xcode-setup
crabbox checkpoint fork chk_abc123 --provider parallels --slug test-a
crabbox checkpoint restore chk_abc123 --provider parallels --id blue-lobster
crabbox checkpoint delete chk_abc123
```

Existing Parallels snapshots do not need to be imported. Use them directly:

```sh
crabbox checkpoint fork --provider parallels --target macos --id "macOS Tahoe" --snapshot "macOS 26.4" --slug tahoe-test
crabbox checkpoint fork --provider parallels --parallels-template ubuntu-fast --dry-run
crabbox checkpoint restore --provider parallels --id "macOS Tahoe" --snapshot "macOS 26.3.1 LATEST"
crabbox checkpoint restore --provider parallels --id blue-lobster --snapshot "known-good" --dry-run
crabbox checkpoint delete --provider parallels --id blue-lobster --snapshot "crabbox-test-snap"
```

`fork` creates a linked clone from the recorded source VM and snapshot.
`restore` switches an existing Parallels lease back to the recorded snapshot.
`delete` deletes only the recorded snapshot, not the source VM. Direct snapshot
delete refuses non-`crabbox-` snapshot names unless `--yes` is supplied. This is
intentional: known-good snapshots are usually hand-managed template state.

Linked clones depend on the source VM and snapshot. Keep known-good template VMs
and their base snapshots while any checkpoint or clone depends on them.

## Safety

Crabbox refuses to delete Parallels VMs unless their name starts with
`crabbox-`. `stop` and `cleanup` are scoped to clones Crabbox created.

Use `--dry-run` on direct fork, restore, and delete when validating a template
or snapshot name. `checkpoint list` prints live Parallels state and marks whether
each snapshot is forkable. Power-on snapshots can be restored in place; linked
clone forks require a power-off snapshot.

Related docs:

- [Provider overview](README.md)
- [Checkpoints](../features/checkpoints.md)
- [Static SSH](ssh.md)
