# checkpoint

Save remote state, fork it into fresh leases later.

**When to use**: Expensive setup (install deps once), paused bugs, generated
fixtures. Fork the scenario for repeated test runs without repeating setup.

## Two Checkpoint Types

**Native (provider snapshots)**
- AWS/Azure/GCP: Creates VM disk snapshot at provider level
- Preserves entire machine: packages, tools, caches, services
- Stored in provider account (incurs storage costs)
- AWS supports Linux and macOS; Azure/GCP support Linux only

**Archive (workspace tarball)**
- Creates local tar of workdir
- Portable across any SSH lease
- Preserves files only, not machine state

Default `--mode auto`: native for AWS Linux/macOS and Azure/GCP Linux,
Parallels VM leases, otherwise archive.

## Quick Start

```sh
# Create checkpoint from lease
crabbox warmup --provider aws --class beast
crabbox run --id blue-lobster --shell 'npm ci && npm test'
crabbox checkpoint create --id blue-lobster --name after-npm-ci
# Output: checkpoint created id=chk_abc123 kind=aws-ebs-snapshot

# Fork checkpoint into new lease
crabbox checkpoint fork chk_abc123 --class beast
# Output: checkpoint forked id=chk_abc123 lease=cbx_xyz slug=purple-whale

# Request a specific slug for the forked lease
crabbox checkpoint fork chk_abc123 --slug update-flow-smoke

# Use forked lease
crabbox run --id purple-whale -- npm test
```

**vs. `crabbox image promote`**: Checkpoints are explicit (fork by ID). Promoted
images change the default AWS runner for all future leases.

## Create

Create checkpoint from existing lease.

```sh
# Auto mode (native for AWS Linux/macOS and Azure/GCP Linux, archive otherwise)
crabbox checkpoint create --id blue-lobster --name after-install

# Force native (fails if unsupported)
crabbox checkpoint create --id blue-lobster --mode native --wait

# Force archive (portable tarball)
crabbox checkpoint create --id blue-lobster --mode archive

# Use image strategy instead of disk-snapshot (AWS Linux/macOS, GCP Linux)
crabbox checkpoint create --id blue-lobster --strategy image

# Direct AWS leases can force native AMI checkpoints without a coordinator
crabbox checkpoint create --provider aws --id blue-lobster --mode native

# Custom workdir
crabbox checkpoint create --id blue-lobster --workdir /work/cbx_123/my-app
```

**Flags**

```
--id <lease>              Required. Lease ID or slug to snapshot
--name <name>             Optional. Human-readable name
--mode auto|native|archive Default auto
--strategy auto|disk-snapshot|image Default auto (disk-snapshot for native)
--wait                    Wait for snapshot completion, default true
--wait-timeout <duration> Default 45m
--no-reboot               Avoid reboot (AWS AMI only), default true
--workdir <path>          Remote workdir, default is lease's repo workdir
--recipe-only             Metadata only, no artifact creation
--reclaim                 Claim lease for current repo
```

**Strategy details**

- `disk-snapshot`: EBS/Azure disk/GCP disk snapshot; AWS macOS maps to AMI-backed checkpoints with backing EBS snapshots
- `image`: AWS AMI/GCP machine image — slower, preserves full VM config
- Azure managed images require stopped VMs, not created from active leases

**What gets cleaned before native snapshot**

- Linux: `cloud-init clean --logs` — resets cloud-init for fresh SSH keys on fork
- `sync` — flushes filesystem writes

**⚠️ Security note**

Native checkpoints capture full root volume: packages, caches, logs, secrets.
Archive checkpoints capture workdir contents: build outputs, generated files.

Both may contain secrets. Delete when no longer needed.

## List And Inspect

```sh
crabbox checkpoint list
crabbox checkpoint list --verify
crabbox checkpoint inspect chk_abc123
crabbox checkpoint inspect chk_abc123 --verify
crabbox checkpoint inspect chk_abc123 --json

# List existing Parallels snapshots directly from a source VM
crabbox checkpoint list --provider parallels --id "macOS Tahoe"
crabbox checkpoint list --provider parallels --id "macOS Tahoe" --json
crabbox checkpoint list --provider parallels --parallels-template tahoe-latest --forkable-only
```

Checkpoints stored in `~/.local/state/crabbox/checkpoints/`.

**Local metadata includes**:
- Checkpoint ID, name, kind
- Source lease ID, provider, region
- Repo name, git head, workdir path
- Creation timestamp
- Native: provider resource ID (ami-xxx, snapshot-xxx)
- Archive: tarball path and size

**⚠️ Both parts required**: Native checkpoints need local metadata AND provider
resource. Losing either side breaks fork. Archive checkpoints need local metadata
AND tarball.

`--verify` audits the second half of the checkpoint:

- Archive checkpoints: confirms the local tarball still exists.
- Native checkpoints: asks the coordinator to look up the provider snapshot or image.
- JSON output includes `localState`, `providerState`, and `nextAction`.

For `provider=parallels`, `checkpoint list --id <vm>` reads live Parallels
snapshots from the VM instead of local `chk_...` metadata. The output marks
whether each snapshot is forkable. Linked clones require a `poweroff` snapshot.
Use `--tree=false` for flat output, `--forkable-only`, `--current`, and
`--name <text>` to filter live snapshots.

## Restore

**Archive checkpoints only**. Uploads tarball to running lease, extracts to workdir.

```sh
crabbox checkpoint restore chk_abc123 --id target-lease
crabbox checkpoint restore chk_abc123 --id target-lease --clear=false
```

**Flags**

```
--id <lease>     Required. Target lease ID or slug
--clear          Clear target workdir before restore, default true
--workdir <path> Custom restore path, default is lease's workdir
```

Recorded native image checkpoints cannot restore in place. Fork them instead
(creates new lease from snapshot/image). Parallels VM snapshots can restore a VM
in place:

```sh
crabbox checkpoint restore --provider parallels --id "macOS Tahoe" --snapshot "macOS 26.3.1 LATEST"
crabbox checkpoint restore --provider parallels --parallels-template tahoe-latest --snapshot "macOS 26.3.1 LATEST" --dry-run
```

## Fork

Create fresh lease from checkpoint. Works for both native and archive checkpoints.

```sh
crabbox checkpoint fork chk_abc123 --class beast
# Output: checkpoint forked id=chk_abc123 lease=cbx_xyz slug=purple-whale

# Fork directly from an existing Parallels snapshot without importing it first
crabbox checkpoint fork --provider parallels --target macos --id "macOS Tahoe" --snapshot "macOS 26.4" --slug tahoe-test
crabbox checkpoint fork --provider parallels --parallels-template ubuntu-fast --slug test-a --dry-run
```

**Flags**

```
--class <class>  Lease class (standard, beast, etc.)
--provider <p>   Provider (aws, azure, gcp, etc.)
--slug <slug>    Request a friendly slug for the forked lease
--keep           Keep lease running, default true
--id <vm>        Parallels source VM when using --snapshot
--snapshot <s>   Parallels snapshot name or id for direct fork
--dry-run        Validate direct Parallels fork without cloning
--parallels-template <name> Use a configured Parallels template alias
```

**What happens**

Native checkpoints:
1. Acquire lease from provider using checkpoint snapshot/image
2. Wait for boot
3. Relocate workdir: `/work/cbx_old/repo` → `/work/cbx_new/repo`
4. Print lease ID and slug
5. Keep lease running

Archive checkpoints:
1. Acquire standard new lease
2. Upload tarball via SSH
3. Extract to workdir
4. Print lease ID and slug
5. Keep lease running

**Fast iteration example**

```sh
crabbox warmup --provider aws --class beast
crabbox run --id blue-lobster --shell 'npm ci && npm test'
crabbox checkpoint create --id blue-lobster --name after-npm-ci

# Fork multiple times for parallel tests
crabbox checkpoint fork chk_abc123 --class beast
crabbox run --id purple-whale -- npm test

crabbox checkpoint fork chk_abc123 --class beast
crabbox run --id green-tiger -- npm run integration-test
```

## Delete

Delete checkpoint from provider and local storage.

```sh
crabbox checkpoint delete chk_abc123
crabbox checkpoint delete chk_abc123 --local-only

# Delete an existing Parallels snapshot directly
crabbox checkpoint delete --provider parallels --id blue-lobster --snapshot "crabbox-test-snap"
crabbox checkpoint delete --provider parallels --id blue-lobster --snapshot "manual-snap" --yes
```

**Default behavior (native checkpoints)**

1. Delete provider resource (EBS snapshot, AMI, disk snapshot)
2. For AMIs: deregister AMI, delete backing EBS snapshots
3. Remove local checkpoint metadata

**Archive checkpoints**

1. Delete local tarball
2. Remove local checkpoint metadata

**Flags**

```
--local-only  Skip provider deletion, remove local metadata only
--id <vm>     Parallels source VM when using --snapshot
--snapshot <s> Parallels snapshot name or id for direct deletion
--dry-run     Validate direct Parallels deletion without deleting
--yes         Allow direct deletion of non-crabbox snapshot names
```

Use `--local-only` only when provider resource was already deleted outside
Crabbox (manual cleanup, account migration, etc.).

## Prune

Delete old checkpoints by age, optionally scoped to native or archive
checkpoints.

```sh
crabbox checkpoint prune --older-than 30d --dry-run
crabbox checkpoint prune --older-than 30d --kind archive
crabbox checkpoint prune --older-than 30d --kind native
```

**Flags**

```
--older-than <duration> Required. Delete checkpoints older than this duration
--kind native|archive   Optional checkpoint kind filter
--dry-run               Print matching checkpoints without deleting them
--local-only            Skip provider deletion for native checkpoints
```

For native checkpoints, prune uses the same provider deletion path as
`checkpoint delete`. Keep `--dry-run` in operator automation until the match set
looks right.

**⚠️ Storage costs**: Provider snapshots/images incur storage costs while they
exist. Delete stale checkpoints periodically. Name checkpoints after scenarios
they preserve to identify candidates for cleanup.

## Provider Support

**Native checkpoints**

Default strategy `disk-snapshot`:
- AWS Linux: EBS snapshot
- AWS macOS: AMI-backed checkpoint with backing EBS snapshots
- Azure: Managed OS disk snapshot
- GCP: Persistent disk snapshot
- Parallels: VM snapshot

Azure disk-snapshot checkpoints require the source lease to use a managed OS
disk, which is the default for new Azure leases. Checkpoint creation refuses
leases started with `--azure-os-disk ephemeral`, because Azure reports a
successful snapshot but does not capture the live OS disk state.

Opt-in strategy `--strategy image`:
- AWS Linux/macOS: AMI (Amazon Machine Image)
- Azure: Not created from active VMs (requires stopped/generalized source)
- GCP Linux: Machine image

AWS macOS uses AMI-backed native checkpoints even when `disk-snapshot` is
requested, because relaunching EC2 Mac from a raw registered root EBS snapshot
does not preserve enough AWS launch metadata. Direct AWS Linux/macOS leases use
AMIs for native checkpoints. `--mode auto` still falls back to workspace
archives without a coordinator, while `--mode native` or `--strategy image`
creates an AMI in the configured AWS region.

AWS macOS checkpoint forks still require EC2 Mac Dedicated Host capacity. Brokered
mode can discover a host; host-pinned checkpoints reuse the recorded `hostId`.

**Archive checkpoints**

All SSH-accessible leases. Portable across providers.

**Future**: Proxmox VM snapshots, sandbox provider snapshots, storage-backed
snapshots (ZFS, Btrfs, LVM) when Crabbox owns integration.
