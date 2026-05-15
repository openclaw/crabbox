# Checkpoints

Crabbox checkpoints make prepared remote state reusable.

The product goal is fast access to known-good or known-interesting scenarios:
dependencies installed, repositories synced, services warm, failure artifacts
present, or a bug paused at a useful workspace state.

Checkpoints are scenario handles. They do not change the default runner image
for normal `warmup` or `run` commands. Use `crabbox checkpoint fork <id>` when
you want a fresh lease from a saved scenario. Use `crabbox image promote` when
you want future AWS leases to boot from a new default runner image.

## Tiers

Crabbox treats checkpointing as tiered, because providers do not expose the same
snapshot primitives.

- `recipe`: metadata only. Stores repo identity, lease/provider info and
  workdir. Current restore/fork commands do not rebuild recipe checkpoints yet.
- `aws-ami`: VM-level AWS Linux checkpoint. Creates an AMI from the backing EC2
  instance, then forks fresh leases from that image. An AMI is AWS's bootable
  machine image format; the disk contents are stored as EBS snapshots.
- `workspace-archive`: portable SSH fallback. Stores the remote workdir as a
  local tarball and restores it to any compatible POSIX SSH lease. It preserves
  workspace files, not the full machine.
- `provider-native`: future provider-owned snapshots for other backends such as
  Proxmox VM snapshots/clones and sandbox-provider snapshots.

The default `auto` mode uses `aws-ami` when the source is a brokered AWS Linux
lease. Otherwise it falls back to `workspace-archive`.

## Current Flow

```sh
crabbox warmup --class beast
crabbox run --id blue-lobster --shell 'npm ci && npm test'
crabbox checkpoint create --id blue-lobster --name after-npm-ci
crabbox checkpoint fork chk_123 --class beast
crabbox run --id <forked-lease> -- npm test
```

`checkpoint create` records either a provider image or a local workspace
archive. `fork` creates a fresh lease from that checkpoint and keeps it
available for more runs or SSH debugging.

For AWS AMI checkpoints, Crabbox resets cloud-init state before image creation.
That lets forked VMs run new user-data and install their own per-lease SSH key
instead of inheriting the source lease key. After the fork boots, Crabbox moves
the snapshotted source workdir to the new lease's normal workdir so existing
`crabbox run --id <fork>` workflows see the prepared scenario.

## What Gets Preserved

AWS AMI checkpoints preserve machine-level state from the EC2 root volume:
system packages, installed tools, language runtimes, caches on disk, services,
repository workdirs, and generated files. They can also preserve secrets if
secrets were written to disk. Treat them like sensitive provider artifacts.

Workspace archives preserve files under the selected workdir. They do not
preserve installed OS packages, system services, remote caches outside the
workdir, users, SSH host configuration, or other machine state.

Both modes record local metadata such as checkpoint id, source lease, provider,
repo name, git head, workdir, kind, and creation time under Crabbox's local
state directory.

## When To Use It

Use an AWS AMI checkpoint when machine setup is the slow part:

```sh
crabbox warmup --provider aws --class beast
crabbox run --id blue-lobster --shell 'sudo apt-get update && sudo apt-get install -y heavy-tool && npm ci'
crabbox checkpoint create --id blue-lobster --name heavy-tool-ready
crabbox checkpoint fork chk_123 --class beast
```

Use a workspace archive when the repo state is the valuable part:

```sh
crabbox checkpoint create --id blue-lobster --mode archive --name failing-fixtures
crabbox checkpoint fork chk_123 --class standard
```

Use a promoted image instead of a checkpoint when the prepared machine should
become the normal base image for future AWS leases.

## Security Boundary

Workspace archives may contain build outputs, caches, logs, repository data, and
anything else in the workdir. Crabbox excludes `.crabbox/env` and
`.crabbox/scripts` by default to avoid persisting profile-backed env helpers,
but users should still treat checkpoint archives as sensitive local artifacts.

AWS AMI checkpoints live in the provider account and are backed by EBS
snapshots. They may contain the full root volume state. `crabbox checkpoint
delete` deregisters the AMI and deletes its snapshots; keep long-lived
checkpoints intentionally. EBS snapshots can incur storage cost while they
exist.

The local checkpoint record is also part of the checkpoint. For AWS, it stores
the AMI id and region. If the local record is lost, the AMI still exists in AWS,
but Crabbox cannot fork it by checkpoint id until equivalent metadata is
restored. If the AMI or EBS snapshots are deleted, the local record is no longer
enough to fork.

## Native Snapshot Direction

Additional native checkpointing should be added only when a backend can
guarantee real semantics:

- create a stable snapshot from a lease or workspace;
- fork that snapshot into a new lease;
- restore or delete it predictably;
- report cost, retention, and security boundaries.

Proxmox is the next natural backend because it has VM-level snapshot and clone
semantics close to local hypervisor snapshots. Plain SSH providers should not
advertise native checkpoint features unless the target host exposes a real
snapshot API such as ZFS, Btrfs, or LVM and Crabbox owns that integration.
