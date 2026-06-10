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

A native checkpoint snapshots the VM at the provider level.

- Preserves full machine state: packages, tools, caches, services, on-disk files.
- Fast to fork (cloud-native snapshot/image).
- Lives in the provider account and incurs storage cost until deleted.
- Supported on brokered AWS Linux/macOS leases, brokered Azure/GCP Linux leases,
  direct AWS Linux/macOS leases, and Parallels (local or remote Mac) clones.

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
| Azure | `azure-os-disk-snapshot` |
| GCP | `gcp-disk-snapshot` |
| Parallels | `parallels-snapshot` |

Disk snapshots are faster to create and (on AWS and GCP) boot with fresh
per-lease SSH keys via injected user-data.

**`image`** — opt in with `--strategy image`.

| Provider | Kind |
| --- | --- |
| AWS | `aws-ami` |
| Azure | `azure-managed-image` |
| GCP | `gcp-machine-image` |

Images are slower to create but preserve complete launch configuration. Direct
AWS Linux/macOS leases use the AMI path for native checkpoints because AMIs fork
directly without a coordinator.

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

## Commands

```
crabbox checkpoint create  --id <lease> [--name <name>] [--mode auto|native|archive] [--strategy auto|disk-snapshot|image]
crabbox checkpoint list    [--json] [--verify]
crabbox checkpoint inspect <checkpoint-id> [--json] [--verify]
crabbox checkpoint restore <checkpoint-id> --id <lease> [--clear=false]
crabbox checkpoint fork    <checkpoint-id> [--class <class>] [--keep]
crabbox checkpoint delete  <checkpoint-id> [--local-only]
crabbox checkpoint prune   --older-than <duration> [--kind native|archive] [--dry-run]
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

On create, native checkpoints flush filesystem writes, reset Linux cloud-init
state when present (so forks boot with fresh SSH keys), call the provider
snapshot/image API, and save the local record with the resource ID and region.
Archive checkpoints tar the workdir over SSH (excluding `.crabbox/env` and
`.crabbox/scripts`), download it, and save the record.

### list and inspect

`list` prints local checkpoint records; `inspect <id>` prints one record's
detail. Add `--json` for machine-readable output. Add `--verify` to audit each
record against its local artifact and the live provider resource — the audit
reports a local state, provider state, and a suggested next action.

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
- AWS/Azure/GCP native checkpoints are VM images, not in-place restores — use
  `fork` to create a lease from them.
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
```

- Native forks acquire a lease from the provider using the snapshot/image, wait
  for boot, then relocate the snapshotted workdir to the new lease's standard
  path.
- Archive forks acquire a standard lease, upload the tarball, and extract it.
- Accepts the standard lease-create flags (`--class`, `--type`, `--market`,
  `--slug`, etc.), `--keep` (default true), `--workdir`, and `--clear`.
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
```

`delete` removes the provider snapshot/image (AWS AMI + backing snapshots, Azure
or GCP image, Parallels snapshot) and then the local record. `--local-only`
deletes the record only. `prune` deletes records older than `--older-than`
(`30m`, `12h`, `7d`, …), optionally filtered by `--kind native|archive`; pair it
with `--dry-run` first. Deleting a non-Crabbox Parallels snapshot requires
`--yes`.

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
