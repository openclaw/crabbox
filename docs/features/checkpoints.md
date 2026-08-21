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

Coordinator-managed AWS, Azure, and GCP checkpoints reserve exact provider
ownership before cloud mutation and durably transition an explicit reserved
phase to started immediately before the provider call. Only an explicitly
reserved pre-mutation interruption, or a definitive provider refusal, can be
canceled immediately; older records without a phase are conservatively treated
as already started from their creation time. Once mutation has started,
ambiguous creation keeps its exact ownership reservation and remains visible in
inventory while maintenance continues recovery, including after the diagnostic
failure threshold. Bounded scheduled scans also re-index older exhausted failed
creations that predate durable retry markers. Cancellation becomes safe only
after at least 60 minutes and two exact, post-horizon provider-absence
confirmations separated by at least five minutes; verified absence still
requires an explicit delete. If the resource appears instead, Crabbox publishes
its verified ownership before managed deletion. Each fork atomically reserves
its exact lease creation attempt
with its checkpoint and hashed use-claim identity before that claim enters
provisioning, then retains the claim until provisioning completes or verified
cancellation proves it is safe to release. Pre-upgrade attempts without that
binding can be repaired only from their exact provisioning claim or completed
checkpoint-backed lease. An ordinary lease attempt cannot be adopted by a
checkpoint fork. Claim expiry cannot open a deletion race while provisioning is
still active. Coordinator maintenance reconciles interrupted attempts against
the exact durable lease and creation attempt,
completing recovered successful forks and releasing claims only after terminal
failure or provider cleanup is confirmed. A caller cannot fabricate last-use
activity by completing an unbound claim.

Coordinator-managed checkpoints are admitted transactionally before provider
mutation. `CRABBOX_MAX_CHECKPOINTS` defaults to 64,
`CRABBOX_MAX_CHECKPOINTS_PER_OWNER` to 16, and
`CRABBOX_MAX_CHECKPOINTS_PER_ORG` to 32; creating, ready, failed, and
deletion-in-progress checkpoints all count, while deleted audit tombstones do
not. Fork/shard claims have separate defaults of 16 per checkpoint
(`CRABBOX_MAX_CHECKPOINT_USE_CLAIMS`), 64 per owner
(`CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_PER_OWNER`), and 256 globally
(`CRABBOX_MAX_CHECKPOINT_USE_CLAIMS_TOTAL`). Expired available claims can be
replaced, but provisioning claims remain charged until their exact lease
lifecycle is reconciled. Exceeding either bound returns HTTP 429 with
`checkpoint_limit_exceeded` or `checkpoint_claim_limit_exceeded`.

Each managed checkpoint retains at most its latest 256 durable lifecycle events.
Event sequence numbers remain monotonic, but this is a bounded recent audit
suffix, not full-history retention: creation and older fork/use events can age
out on long-lived, high-churn checkpoints.

## Two kinds

Each checkpoint has a `kind`. Crabbox picks one automatically (`--mode auto`,
the default) and you can override it with `--mode` and `--strategy`.

### Native (provider snapshot)

A native checkpoint snapshots the VM at the provider level.

- Preserves full machine state: packages, tools, caches, services, on-disk files.
- Fast to fork (cloud-native snapshot/image).
- Lives in the provider account and incurs storage cost until deleted.
- Supported on brokered AWS Linux/macOS leases, brokered Azure/GCP Linux leases,
  direct AWS Linux/macOS and Hetzner Linux leases, and Parallels (local or
  remote Mac) clones.

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

**Ownership depends on how the checkpoint was created.** Newly created brokered
native AWS, Azure, and GCP checkpoints have authoritative, owner/org-scoped
coordinator records; local `checkpoint.json` files are recoverable compatibility
caches. Losing that cache does not prevent listing, inspection, policy changes,
forks, sharding, or deletion. Direct-provider, archive, recipe, and historical
checkpoints remain authoritative local records under
`$XDG_STATE_HOME/crabbox/checkpoints/` or
`<user-config>/crabbox/state/checkpoints/`. Archives also require their local
`workspace.tar.gz`; losing local/operator-managed metadata still loses access.

## Lifecycle and expiry

Checkpoint records store both `createdAt` and `lastUsedAt`. A new checkpoint
initializes `lastUsedAt` to exactly `createdAt`; each successful recorded
`checkpoint fork` or `checkpoint restore` updates it. Dry runs and failed
consumption attempts do not count as use. For records written by older Crabbox
versions without `lastUsedAt`, reads treat `createdAt` as the last-use time.
This fallback does not eagerly rewrite or migrate the record.

Managed brokered native checkpoints retain their provider resources indefinitely
by default. Opt into coordinator-owned unused expiry per checkpoint with
`checkpoint create --expire-unused-after 7d` or
`checkpoint policy <id> --expire-unused-after 7d`; restore indefinite retention
with `checkpoint policy <id> --manual`. Independent, renewable use claims fence
every fork/shard, and only a completed fork advances `lastUsedAt`. Durable
promotion pins protect AWS and Azure catalog/default images until their exact
catalog entries are retired or replaced. Provider failures retain ownership,
redacted retry diagnostics, and the exact resource identity for safe recovery.

Direct, archive, recipe, and all preexisting checkpoints never enter this
coordinator reaper. Use `checkpoint prune` manually or schedule it in the
environment that owns those local records. Native provider artifacts can keep
incurring storage charges while archive checkpoints consume local disk.

## Commands

```
crabbox checkpoint create  --id <lease> [--name <name>] [--mode auto|native|archive] [--strategy auto|disk-snapshot|image] [--expire-unused-after 7d]
crabbox checkpoint list    [--json] [--verify] [--local-only]
crabbox checkpoint inspect <checkpoint-id> [--json] [--verify]
crabbox checkpoint policy  <checkpoint-id> --manual|--expire-unused-after <duration>
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

This scheduled CLI recipe is for legacy/direct/operator-managed checkpoints.
Opted-in brokered native checkpoints are instead reaped by their coordinator;
they do not require an operator machine, local state directory, provider
credentials, or a separate cron job.

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
