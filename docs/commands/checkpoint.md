# checkpoint

Save the state of a lease, then restore it onto another box or fork it into a
fresh lease later. A checkpoint turns expensive one-time setup — installed
dependencies, warmed caches, generated fixtures, a paused bug — into something
you can reproduce on demand without repeating the work.

Each checkpoint has an ID of the form `chk_<hex>` and is recorded locally under
the Crabbox state directory (`$XDG_STATE_HOME/crabbox/checkpoints`, or
`<user-config-dir>/crabbox/state/checkpoints` when `XDG_STATE_HOME` is unset).

Subcommands: `create`, `list`, `inspect`, `restore`, `fork`, `delete`, `prune`.

## Two checkpoint kinds

**Native (provider snapshot or image)** — captures the whole machine: packages,
tools, caches, services. Stored in the provider account, so it incurs provider
storage costs. Recorded as one of `aws-ami`, `aws-ebs-snapshot`,
`azure-managed-image`, `azure-os-disk-snapshot`, `gcp-machine-image`,
`gcp-disk-snapshot`, or `parallels-snapshot`.

**Archive (workspace tarball)** — captures only the contents of the remote
workdir as `workspace.tar.gz`. Portable across any POSIX SSH lease, but it does
not preserve machine state.

`--mode auto` (the default) picks native for providers that support it on the
current lease and falls back to an archive otherwise. There is also a
metadata-only `recipe` kind produced by `--recipe-only`, which records the
checkpoint without creating any artifact.

> Both kinds may contain secrets. Native checkpoints capture the full root
> volume (caches, logs, credentials); archives capture build outputs and
> generated files. Delete checkpoints when you no longer need them.

## Quick start

```sh
# Warm a lease, do the expensive setup, then snapshot it
crabbox warmup --provider aws --class beast
crabbox run --id swift-crab --shell 'npm ci && npm test'
crabbox checkpoint create --id swift-crab --name after-npm-ci
# checkpoint created id=chk_abc123 kind=aws-ebs-snapshot resource=snap-... state=available region=eu-west-1 workdir=...

# Fork the checkpoint into a brand new lease
crabbox checkpoint fork chk_abc123 --class beast
# checkpoint forked id=chk_abc123 lease=cbx_... slug=purple-whale image=snap-... workdir=...

# Run against the forked lease
crabbox run --id purple-whale -- npm test
```

Checkpoints are explicit: you fork a specific ID. To change the *default* base
image used by all future leases instead, use
[`crabbox image promote`](image.md).

## create

Create a checkpoint from an existing lease.

```sh
# Auto mode (native where supported, archive otherwise)
crabbox checkpoint create --id swift-crab --name after-install

# Force a native checkpoint (fails if the lease does not support one)
crabbox checkpoint create --id swift-crab --mode native --wait

# Force a portable workspace archive
crabbox checkpoint create --id swift-crab --mode archive

# Prefer a full image over a disk snapshot
crabbox checkpoint create --id swift-crab --strategy image

# Direct AWS lease (no coordinator): force a native AMI
crabbox checkpoint create --provider aws --id swift-crab --mode native

# Azure Windows: permit a bounded deallocate/snapshot/restart cycle
crabbox checkpoint create --provider azure --target windows --id swift-crab \
  --strategy disk-snapshot --no-reboot=false

# Archive a custom workdir
crabbox checkpoint create --id swift-crab --workdir /work/cbx_123/my-app
```

**Flags**

```
--id <lease>                Required. Lease id or slug to checkpoint.
--provider <name>           Provider hint when the lease is not yet claimed.
--name <name>               Human-readable checkpoint name.
--mode auto|native|archive  Checkpoint mode (default auto).
--strategy auto|disk-snapshot|image
                            Native checkpoint strategy (default auto).
--wait                      Wait for the native snapshot to become available
                            (default true).
--wait-timeout <duration>   Maximum native snapshot wait (default 45m).
--no-reboot                 Avoid rebooting or stopping the source instance
                            during a native snapshot (default true). Azure
                            Windows disk snapshots require false.
--workdir <path>            Remote workdir to archive (default: the lease's repo
                            workdir).
--recipe-only               Record metadata only; create no artifact.
--reclaim                   Claim this lease for the current repo.
```

`--mode` also accepts the aliases `provider-native`/`vm` (native),
`ami`/`image` (image), `snapshot`/`disk`/`disk-snapshot` (disk snapshot),
`workspace`/`workspace-archive` (archive), and `recipe`. `--strategy auto`
resolves to a disk snapshot where the provider supports one.

**Strategy details**

- `disk-snapshot` — EBS / Azure managed-OS-disk / GCP persistent-disk snapshot;
  Parallels VM snapshot. AWS macOS always uses an AMI-backed checkpoint (with a
  backing EBS snapshot) because relaunching an EC2 Mac from a raw root snapshot
  loses required launch metadata.
- `image` — AWS AMI / GCP machine image. Slower, but preserves full VM config.
- Azure cannot create a managed image from an active VM, so the Azure native
  path uses a managed OS-disk snapshot. That snapshot requires a managed OS disk
  (the default); creation refuses leases started with
  `--azure-os-disk ephemeral` or `--azure-os-disk ephemeral-preview`, where
  Azure reports success but does not capture live disk state.
- Direct Azure Windows disk snapshots support `windows.mode=normal` leases and
  require `--no-reboot=false`. Crabbox
  deallocates the source for a consistent snapshot, restarts it after snapshot
  creation (including failure paths), and rotates SSH host, SSH login, Windows,
  and loopback-only VNC credentials when each fork boots.
- Azure snapshot names use letters, digits, underscores, and hyphens and are
  limited to 80 characters; generated names retain a unique timestamp suffix.

Before a native snapshot, Crabbox cleans the source: on Linux it runs
`cloud-init clean --logs` (so a forked box regenerates SSH host keys) and
`sync` to flush filesystem writes.

## list and inspect

```sh
crabbox checkpoint list
crabbox checkpoint list --json
crabbox checkpoint list --verify

crabbox checkpoint inspect chk_abc123
crabbox checkpoint inspect chk_abc123 --json
crabbox checkpoint inspect chk_abc123 --verify
```

`list` and `inspect` read the local checkpoint records. Each record holds the
checkpoint id/name/kind, source lease/provider/region, repo name and git head,
workdir, creation time, and — for native checkpoints — the provider resource id;
for archives, the tarball path and size.

> A native checkpoint needs **both** halves to fork: the local metadata and the
> provider resource. An archive checkpoint needs the local metadata and the
> tarball. Lose either side and the checkpoint is unusable.

`--verify` audits the other half. For archives it confirms the local tarball
still exists; for native checkpoints it asks the provider (directly for AWS, or
via the coordinator) whether the snapshot or image is still present. JSON output
includes `localState`, `providerState`, and `nextAction`.

### Parallels: live VM snapshots

For `provider=parallels`, `list --id <vm>` reads the live snapshots on a source
VM instead of local `chk_...` records, marking which are forkable (linked clones
require a `poweroff` snapshot):

```sh
crabbox checkpoint list --provider parallels --id "macOS Tahoe"
crabbox checkpoint list --provider parallels --id "macOS Tahoe" --json
crabbox checkpoint list --provider parallels --parallels-template tahoe-latest --forkable-only
```

Filters: `--tree` (default true; use `--tree=false` for flat output),
`--forkable-only`, `--current`, and `--name <substring>`. Passing
`--parallels-template` implies `provider=parallels`.

## restore

Restore brings a checkpoint back onto a lease in place. This is for **archive
checkpoints** (and Parallels VM snapshots) — a native image checkpoint cannot be
restored in place; fork it instead.

```sh
# Archive checkpoint -> existing lease
crabbox checkpoint restore chk_abc123 --id target-lease
crabbox checkpoint restore chk_abc123 --id target-lease --clear=false

# Parallels VM snapshot, in place
crabbox checkpoint restore --provider parallels --id "macOS Tahoe" --snapshot "macOS 26.3.1 LATEST"
crabbox checkpoint restore --provider parallels --parallels-template tahoe-latest --snapshot "macOS 26.3.1 LATEST" --dry-run
```

**Flags**

```
--id <lease>      Required. Target lease id or slug (or Parallels VM with --snapshot).
--provider <name> Provider hint.
--snapshot <name> Parallels snapshot name or id to switch to in place.
--clear           Clear the target workdir before extracting (default true).
--workdir <path>  Custom restore workdir (default: the lease's workdir).
--dry-run         Print the restore target without changing anything.
--reclaim         Claim this lease for the current repo.
```

An archive restore uploads the tarball over SSH and extracts it into the
workdir. Restoring a non-archive, non-Parallels native checkpoint is an error
that points you at `checkpoint fork`.

## fork

Create a fresh lease from a checkpoint. Works for both native and archive
checkpoints, and accepts the shared lease-create flags (`--class`, `--provider`,
`--slug`, `--type`, `--os`, etc.).

```sh
crabbox checkpoint fork chk_abc123 --class beast
# checkpoint forked id=chk_abc123 lease=cbx_... slug=purple-whale ...

# Request a friendly slug for the forked lease
crabbox checkpoint fork chk_abc123 --slug update-flow-smoke

# Fan out one checkpoint into several forked leases for parallel attempts
crabbox checkpoint fork chk_abc123 --count 3 --slug update-flow
# checkpoint forked id=chk_abc123 lease=cbx_... slug=update-flow-1 ...
# checkpoint forked id=chk_abc123 lease=cbx_... slug=update-flow-2 ...
# checkpoint forked id=chk_abc123 lease=cbx_... slug=update-flow-3 ...

# Fan out one checkpoint and run the same command on each fork
crabbox checkpoint fork chk_abc123 --count 3 --slug update-flow -- pnpm test -- --shard '{{index}}/{{total}}'
# checkpoint fork command lease=cbx_... slug=update-flow-1 index=1/3 command=...
# checkpoint fork command lease=cbx_... slug=update-flow-2 index=2/3 command=...
# checkpoint fork command lease=cbx_... slug=update-flow-3 index=3/3 command=...

# Fork directly from a Parallels snapshot without recording it first
crabbox checkpoint fork --provider parallels --target macos --id "macOS Tahoe" --snapshot "macOS 26.4" --slug tahoe-test
crabbox checkpoint fork --provider parallels --parallels-template ubuntu-fast --slug test-a --dry-run
```

**Flags** (in addition to the standard lease-create flags)

```
--keep            Keep the forked lease running (default true).
--count <n>       Create multiple forked leases (default 1).
--id <vm>         Parallels source VM when forking from --snapshot.
--snapshot <name> Parallels snapshot name or id for a direct fork.
--clear           Clear the workdir before restoring an archive (default true).
--workdir <path>  Remote workdir for the forked lease.
--dry-run         Print the fork target without acquiring a lease.
--reclaim         Claim the new lease for the current repo.
```

**What happens**

- *Native:* acquire a new lease from the checkpoint snapshot/image, wait for
  boot, relocate the workdir from the old lease path to the new one, then print
  the lease id and slug.
- *Azure Windows native:* preserve the snapshotted filesystem in place and
  print `workdir=-`; these desktop leases do not use the POSIX workdir
  relocation flow.
- *Archive:* acquire a standard new lease, upload and extract the tarball into
  the workdir, then print the lease id and slug.
- *Fan-out:* `--count <n>` repeats the same provider-neutral fork flow. When
  combined with `--slug`, Crabbox appends a stable numeric suffix such as
  `update-flow-1`, `update-flow-2`, and `update-flow-3`.
- *Command fan-out:* arguments after `--` run through `crabbox run --id <lease>`
  on each fork, so normal sync, command wrapping, history, and proof behavior are
  preserved. Use `{{index}}`, `{{total}}`, `{{lease}}`, and `{{slug}}` in command
  arguments to specialize each fork. Forks and their commands run one after
  another; for concurrent shards with one merged test verdict, use
  [`crabbox shard`](shard.md).

Fork multiple times to run scenarios in parallel:

```sh
crabbox checkpoint fork chk_abc123 --class beast --count 2 --slug update-flow

crabbox run --id update-flow-1 -- npm test
crabbox run --id update-flow-2 -- npm run integration-test
```

For macOS native checkpoints, forks default to the `on-demand` market (unless
you set `--market`) and still require EC2 Mac Dedicated Host capacity; brokered
mode can discover a host, while host-pinned checkpoints reuse the recorded host.

## delete

Delete a checkpoint and its provider resource, then remove the local record.

```sh
crabbox checkpoint delete chk_abc123
crabbox checkpoint delete chk_abc123 --local-only
crabbox checkpoint delete chk_abc123 --dry-run

# Delete a Parallels snapshot directly
crabbox checkpoint delete --provider parallels --id swift-crab --snapshot "crabbox-test-snap"
crabbox checkpoint delete --provider parallels --id swift-crab --snapshot "manual-snap" --yes
```

For native checkpoints, delete removes the provider resource first (AMIs are
deregistered along with their backing EBS snapshots; disk snapshots are
deleted), then removes the local record. Archive checkpoints just lose their
tarball and record.

**Flags**

```
--local-only      Remove only the local record; skip provider deletion.
--provider <name> Provider hint for a direct Parallels snapshot delete.
--id <vm>         Parallels source VM when using --snapshot.
--snapshot <name> Parallels snapshot name or id to delete directly.
--dry-run         Print the deletion target without deleting.
--yes             Allow deleting a Parallels snapshot whose name is not
                  prefixed with `crabbox-`.
```

Use `--local-only` only when the provider resource was already removed outside
Crabbox (manual cleanup, account migration, and so on).

## prune

Delete checkpoints older than a cutoff, optionally restricted to one kind.

```sh
crabbox checkpoint prune --older-than 30d --dry-run
crabbox checkpoint prune --older-than 30d --kind archive
crabbox checkpoint prune --older-than 30d --kind native
```

**Flags**

```
--older-than <duration> Required. Delete checkpoints older than this. Accepts a
                        Go duration (e.g. 720h) or a whole number of days (30d).
--kind native|archive   Restrict to one kind.
--dry-run               Print matches without deleting.
--local-only            Skip provider deletion for native checkpoints.
```

Native checkpoints prune through the same provider-deletion path as
`checkpoint delete`. Keep `--dry-run` in operator automation until the match set
looks right.

> Provider snapshots and images keep accruing storage cost while they exist.
> Prune stale checkpoints periodically, and name checkpoints after the scenario
> they preserve so cleanup candidates are easy to spot.

## Provider support

**Native checkpoints**

| Provider | Default (`disk-snapshot`) | `--strategy image` |
| --- | --- | --- |
| AWS Linux | EBS snapshot | AMI |
| AWS macOS | AMI-backed checkpoint (backing EBS snapshot) | AMI |
| Azure Linux | Managed OS-disk snapshot | not supported from an active VM |
| Azure Windows (`windows.mode=normal`) | Managed OS-disk snapshot (`--no-reboot=false`) | not supported |
| GCP Linux | Persistent-disk snapshot | Machine image |
| Parallels | VM snapshot | — |

Brokered native checkpoints (through a configured coordinator) cover AWS
Linux/macOS and Azure/GCP Linux leases. Azure Windows leases use the direct
managed-OS-disk snapshot path. Direct AWS Linux/macOS leases create
AMIs locally without a coordinator: `--mode auto` falls back to a workspace
archive when no coordinator is configured, while `--mode native` or
`--strategy image` creates an AMI in the configured AWS region. Parallels native
snapshots run directly against the Parallels host.

**Archive checkpoints**

Any POSIX SSH-accessible lease (Linux, macOS, or Windows under WSL2). Portable
across providers. Windows-native (non-WSL2) leases are not supported.
