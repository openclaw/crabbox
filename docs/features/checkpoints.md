# Checkpoints

Checkpoints save prepared remote state so you can reuse it instead of rebuilding
it. Install dependencies once, pause a bug at a useful state, keep generated
fixtures, then fork that scenario for repeated runs.

Read when:

- you want to skip slow per-run setup (toolchains, deps, caches);
- you want to capture and re-open an exact failure state;
- you are choosing between a checkpoint, a base image, a [capsule](capsules.md),
  or a [cache](cache.md).

**Checkpoints are explicit scenario handles, not the default image.** Use
`crabbox checkpoint fork <id>` to start a fresh lease from a saved scenario. To
change the *default* base image for all future leases on a provider, use
`crabbox image promote` instead (see [prebaked images](prebaked-images.md)).

## Two kinds

Each checkpoint has a `kind`. Crabbox picks one automatically (`--mode auto`,
the default) and you can override it with `--mode` and `--strategy`.

### Native (provider snapshot)

A native checkpoint captures a VM or container's disk state at the provider level.

- Preserves packages, tools, caches, service configuration, and on-disk files.
- Fast to fork (cloud-native snapshot/image).
- Lives in the provider account and incurs storage cost until deleted.
- Supported on brokered AWS Linux/macOS leases, brokered Azure/GCP Linux leases,
  direct AWS Linux/macOS and Hetzner Linux leases, Parallels (local or remote
  Mac) clones, and direct Docker and Incus Linux containers.

### Archive (workspace tarball)

An archive checkpoint captures the workdir only and stores it locally.

- Portable across any POSIX SSH-accessible lease (Linux, macOS, Windows WSL2).
- Does **not** preserve system packages or machine state.
- Excludes `.crabbox/env` and `.crabbox/scripts` from the tarball by default.

A third kind, `recipe` (`--recipe-only`), records lease/repo/workdir metadata
without creating any artifact. It is a bookkeeping marker and cannot be forked
or restored.

### How `auto` decides

`--mode auto` produces a native checkpoint when the resolved lease supports one
for the chosen strategy, otherwise it falls back to an archive. With the default
`--strategy auto`, direct (non-brokered) providers other than Parallels only get
a native checkpoint when you explicitly ask for `--mode native`; `auto` keeps the
archive fallback for them.

## Native strategies

Native checkpoints use one of two provider primitives, selected with
`--strategy`:

**`disk-snapshot`** — the default the auto strategy normalizes to.

| Provider | Kind |
| --- | --- |
| AWS Linux | `aws-ebs-snapshot` |
| AWS macOS | `aws-ami` (AMI-backed; raw EC2 Mac root snapshots lack enough launch metadata to fork reliably) |
| Azure Linux or native Windows (`windows.mode=normal`) | `azure-os-disk-snapshot` |
| GCP | `gcp-disk-snapshot` |
| Hetzner Linux (direct only) | `hetzner-snapshot` |
| Parallels | `parallels-snapshot` |
| Incus Linux containers (direct, root disk only) | `incus-image` (private image published from a stateless disk snapshot) |

Disk snapshots are faster to create and (on AWS and GCP) boot with fresh
per-lease SSH keys via injected user-data.

Native Azure Windows snapshot creation is intentionally disruptive and does not
support `windows.mode=wsl2`: pass
`--no-reboot=false` to allow Crabbox to deallocate the source, create a
consistent managed-OS-disk snapshot, and restart the source. Forks specialize
the copied disk with fresh SSH host/login keys, Windows credentials, and
loopback-only TightVNC credentials.

**`image`** — opt in with `--strategy image`.

| Provider | Kind |
| --- | --- |
| AWS | `aws-ami` |
| Azure | `azure-managed-image` |
| GCP | `gcp-machine-image` |

Images are slower to create but preserve complete launch configuration. Direct
AWS Linux/macOS leases use the AMI path for native checkpoints because AMIs fork
directly without a coordinator.

**Hetzner notes.** Direct Hetzner Linux leases with a numeric server ID support
`--mode native` and create a project snapshot. `--strategy image` is not
supported. Crabbox re-reads the source server, requires its canonical ownership
labels and exact local lease claim, resets cloud-init, and binds the snapshot to
the checkpoint, lease, source server, location, and architecture. Brokered
Hetzner leases continue to use workspace archives; the coordinator does not
create Hetzner snapshots.

**`docker-commit`** — the `local-container` provider's native primitive (opt in
with `--mode native`; `auto` keeps the workspace-archive default). `crabbox
checkpoint create` captures the container filesystem as a Docker image tagged
`crabbox-checkpoint-<name>-<digest>` (using the immutable image digest as
identity); `crabbox checkpoint inspect <id> --verify` (or `checkpoint list
--verify`) confirms the image is still present on its daemon; and `crabbox
checkpoint delete <id>` removes its verified Crabbox-owned image tag while
preserving any user-created tags or dependent containers. Crabbox strips lease
ownership labels from the committed image so derived containers are not
inventoried as the source lease and replaces the mount-dependent bootstrap
command with a persistent default command. The Docker context, context-store
path, resolved daemon endpoint, and Docker system ID used at create time are
recorded and validated so verify and delete fail closed if that context or
daemon is later replaced. Native checkpoints are currently Docker-only; Podman
and nerdctl leases keep using workspace archives. Crabbox rejects native
checkpoint creation when the workspace is stored in a mounted volume because
`docker commit` does not capture mounted data.

`crabbox checkpoint fork <id>` starts a fresh local-container lease from the
checkpoint image, then relocates the saved workspace into the new lease path.
Fork validates the recorded image tag and Docker system ID before launch and
replays the checkpoint's Docker runtime, context store, context, and host so a
changed ambient Docker configuration cannot select another daemon. The forked
lease persists that scope for later `run`, `ssh`, and `stop` commands and
reuses the source container user and work root during workspace relocation.

**Incus notes.** Native capture is opt-in with `--mode native`; `auto` retains
the generic archive default. A private Incus image outlives source-instance
deletion, unlike an ordinary Incus snapshot. Forks replace SSH login/host keys,
machine ID, and hostname on the stopped clone before first boot, disable
inherited cloud-init/templates, and preserve root-disk workspace and dependency
data. Fixed-ID allocation and checkpoint forks are supported. Attached disks,
mounted workspaces, and VM native captures are rejected. Memory and running
processes are not included. See [Incus](../providers/incus.md#native-disk-checkpoints)
for ownership, cleanup, and live-proof requirements.

**Azure notes.** Disk-snapshot checkpoints require managed OS disks, the default
for new Azure leases. Crabbox refuses native checkpoint creation from Azure
ephemeral-OS-disk leases (Azure reports success but does not capture live disk
state). Azure disk-snapshot forks boot from a specialized OS disk and may inherit
the source machine identity — treat them as exact clones. Use
`--azure-os-disk ephemeral` or `--azure-os-disk ephemeral-preview` only for
stateless leases that do not need native checkpoints.

**Parallels notes.** A forkable Parallels snapshot must be taken from a
powered-off VM (linked clones require it). `checkpoint create` stops a running VM
when `--no-reboot=false`; with the default `--no-reboot=true` it refuses and asks
you to stop the VM first.

**AWS macOS notes.** macOS forks still require EC2 Mac Dedicated Host capacity.
Brokered mode can discover a host; host-pinned checkpoints reuse the recorded
host. `checkpoint fork` defaults the market to `on-demand` for macOS native
checkpoints unless you set `--market`.

## Security

**Native snapshots may contain secrets.** They capture the full root volume:
system files, logs, caches, installed packages, and anything written to disk
during setup. Treat them as sensitive provider artifacts. Delete them when no
longer needed, and do not create them from ad-hoc debugging sessions that hold
temporary credentials.

**Archives may contain secrets too.** They capture workdir contents including
build outputs, caches, and generated files. Crabbox excludes `.crabbox/env` and
`.crabbox/scripts` but does not scan arbitrary files for credentials.

**Local metadata is authoritative.** Every checkpoint stores metadata locally
under the Crabbox state directory: `$XDG_STATE_HOME/crabbox/checkpoints/` when
`XDG_STATE_HOME` is set, otherwise `<user-config>/crabbox/state/checkpoints/`
(for example `~/Library/Application Support/crabbox/state/checkpoints/` on
macOS). Each entry holds a `checkpoint.json` record plus, for archives, a
`workspace.tar.gz`. Losing the local record means you can no longer fork or
delete by checkpoint ID; deleting the provider resource leaves the local record
unable to fork.

## Lifecycle and expiry

Capture and deletion of the same checkpoint cannot overlap. A conflicting delete
or prune reports that the checkpoint is busy; retry after the active operation
finishes. Usage updates read the current record, so a late fork or restore cannot
recreate a checkpoint that was deleted in the meantime.

Checkpoint records store both `createdAt` and `lastUsedAt`. A new checkpoint
initializes `lastUsedAt` to exactly `createdAt`; each successful recorded
`checkpoint fork` or `checkpoint restore` updates it. Dry runs and failed
consumption attempts do not count as use. For records written by older Crabbox
versions without `lastUsedAt`, reads treat `createdAt` as the last-use time.
This fallback does not eagerly rewrite or migrate the record.

Crabbox does not run an automatic checkpoint reaper yet. Use `checkpoint prune`
manually or schedule it in the environment that owns the local checkpoint
records. Native provider snapshots and images can keep incurring provider
storage charges even when no lease uses them, while archive checkpoints consume
disk in the local state directory. Choose an unused interval that reflects the
cost of rebuilding the checkpoint and preview it with `--dry-run` before making
the scheduled job destructive.

## Commands

```
crabbox checkpoint create  --id <lease> [--name <name>] [--mode auto|native|archive] [--strategy auto|disk-snapshot|image]
crabbox checkpoint list    [--json] [--verify]
crabbox checkpoint inspect <checkpoint-id> [--json] [--verify]
crabbox checkpoint restore <checkpoint-id> --id <lease> [--clear=false]
crabbox checkpoint fork    <checkpoint-id> [--class <class>] [--keep] [--count <n>]
crabbox checkpoint delete  <checkpoint-id> [--local-only]
crabbox checkpoint prune   [--older-than <duration>] [--unused-for <duration>] [--kind native|archive] [--dry-run]
```

Checkpoint IDs look like `chk_<hex>` (see [identifiers](identifiers.md)).
`--id` accepts a lease ID or slug. `create`, `restore`, and `fork` accept
`--reclaim` to claim the lease for the current repo.

### create

```sh
crabbox checkpoint create --id blue-lobster --name after-npm-ci
# checkpoint created id=chk_abc123 ...
```

Useful flags:

- `--mode auto|native|archive` (default `auto`).
- `--strategy auto|disk-snapshot|image` (default `auto`).
- `--name <name>` — friendly label stored in the record and provider resource.
- `--workdir <path>` — archive a workdir other than the repo checkout.
- `--recipe-only` — record metadata only; create no artifact.
- `--wait` / `--wait-timeout` (default on, 45m) — wait for the native snapshot to
  become available.
- `--no-reboot` (default on) — avoid rebooting the source instance during a
  native snapshot.

Native checkpoints call the provider snapshot/image API and save the local
record with its resource identity and location. Source quiescing and fork
credential handling are provider-specific; see the provider notes above.
Archive checkpoints tar the workdir over SSH (excluding `.crabbox/env` and
`.crabbox/scripts`), download it, and save the record.

### list and inspect

`list` prints local checkpoint records; `inspect <id>` prints one record's
detail. Add `--json` for machine-readable output. Add `--verify` to audit each
record against its local artifact and the live provider resource — the audit
reports a local state, provider state, and a suggested next action. Listing
includes only atomically published metadata, not unpublished reservation
directories. Corrupt published records remain explicit errors.

`list` can also enumerate provider-native snapshots directly for Parallels:

```sh
crabbox checkpoint list --provider parallels --id <vm-name-or-id> [--tree] [--forkable-only] [--current] [--name <substr>]
```

### restore

Restore re-applies a checkpoint onto an **existing** lease.

```sh
crabbox checkpoint restore chk_abc123 --id purple-whale
```

- Archive checkpoints extract back into the workdir; `--clear` (default true)
  wipes the target workdir first, and `--workdir` overrides the destination.
- Parallels native checkpoints switch the VM to the snapshot.
- AWS/Azure/GCP/Hetzner native checkpoints are VM images, not in-place restores
  — use `fork` to create a lease from them.
- `--dry-run` prints the target without changing anything.

Parallels also supports restoring a snapshot by name directly:

```sh
crabbox checkpoint restore --provider parallels --id <vm> --snapshot <name-or-id>
```

### fork

Fork leases a **new** box from a checkpoint and keeps it running.

```sh
crabbox checkpoint fork chk_abc123 --class beast
# checkpoint forked id=chk_abc123 lease=cbx_... slug=purple-whale ...
crabbox run --id purple-whale -- npm test

crabbox checkpoint fork chk_abc123 --count 3 --slug update-flow
# checkpoint forked id=chk_abc123 lease=cbx_... slug=update-flow-1 ...
# checkpoint forked id=chk_abc123 lease=cbx_... slug=update-flow-2 ...
# checkpoint forked id=chk_abc123 lease=cbx_... slug=update-flow-3 ...
```

- Native forks acquire a lease from the provider using the snapshot/image, wait
  for boot, then relocate the snapshotted workdir to the new lease's standard
  path.
- Azure Windows native forks are the exception: they preserve the snapshotted
  filesystem in place and report `workdir=-` because Windows desktop leases do
  not use Crabbox's POSIX repository workdir relocation flow.
- Archive forks acquire a standard lease, upload the tarball, and extract it.
- Accepts the standard lease-create flags (`--class`, `--type`, `--market`,
  `--slug`, etc.), `--keep` (default true), `--count`, `--workdir`, and
  `--clear`.
- `--count <n>` fans out the same checkpoint into multiple fresh leases. When
  `--slug` is set, each fork gets a stable numeric suffix.
- `--dry-run` prints the planned fork target.

Parallels can fork from a snapshot by name:

```sh
crabbox checkpoint fork --provider parallels --id <vm> --snapshot <name-or-id> [--slug <slug>]
```

### delete and prune

```sh
crabbox checkpoint delete chk_abc123          # remove provider resource + local record
crabbox checkpoint delete chk_abc123 --local-only   # keep provider resource
crabbox checkpoint prune --older-than 7d --kind native --dry-run
crabbox checkpoint prune --unused-for 30d --kind native --dry-run
```

`delete` removes the provider snapshot/image (AWS AMI + backing snapshots, Azure
or GCP image, Hetzner project snapshot, Parallels snapshot) and then the local
record. Hetzner first verifies the remote snapshot type and every recorded
ownership/source-binding label. `--local-only` deletes the record only. Provider
deletion remains first: if it fails, Crabbox keeps the local record. `prune`
requires at least one age filter. `--older-than` selects by creation time and
`--unused-for` selects by last-use time (`30m`, `12h`, `7d`, …). When both are
supplied, a checkpoint must satisfy both; the optional `--kind native|archive`
filter is also composed with them. Pair the final filter set with `--dry-run`
first. Deleting a non-Crabbox Parallels snapshot requires `--yes`.

For example, this user crontab previews the policy interactively first, then
removes native checkpoints that have not been forked or restored for 30 days:

```sh
crabbox checkpoint prune --unused-for 30d --kind native --dry-run

# crontab -e (run as the same user that created the checkpoint records)
SHELL=/bin/sh
PATH=/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin
XDG_STATE_HOME=/home/alice/.local/state
CRABBOX_CONFIG=/home/alice/.config/crabbox/config.yaml
15 3 * * * crabbox checkpoint prune --unused-for 30d --kind native >> /home/alice/.local/state/crabbox/checkpoint-prune.log 2>&1
```

The scheduler invokes the CLI; Crabbox itself does not run an automatic
checkpoint reaper yet. The scheduled environment must point at the same state
directory and config as the checkpoint owner and provide whatever provider
credentials or local runtime access deletion requires.

## When to use which

**Native checkpoints — machine setup is slow.** Heavy toolchains, large package
installs, GPU drivers:

```sh
crabbox warmup --provider aws --class beast
crabbox run --id blue-lobster --shell 'sudo apt-get install -y cuda-toolkit && npm ci'
crabbox checkpoint create --id blue-lobster --name cuda-ready
crabbox checkpoint fork chk_123 --class beast
```

**Archives — repo state is the valuable part.** Paused bugs, generated fixtures,
build artifacts:

```sh
crabbox checkpoint create --id blue-lobster --mode archive --name failing-fixtures
crabbox checkpoint fork chk_123 --class standard
```

**Promoted images — you want a new default base.** If the prepared machine should
become the standard image for all future leases, use `crabbox image promote`
instead. Checkpoints are explicit scenario handles; promoted images change the
global default. See [prebaked images](prebaked-images.md) and the
[image bake runbook](image-bake-runbook.md).

## Related

- [Capsules](capsules.md) — failure replay manifests (versus prepared machines).
- [Cache](cache.md) — package/build cache state on a lease.
- [Prebaked images](prebaked-images.md) — trusted base runner images.
- [Identifiers](identifiers.md) — `chk_`, `cbx_`, and slug formats.
