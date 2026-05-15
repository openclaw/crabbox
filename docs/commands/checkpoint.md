# checkpoint

Save remote state, fork it into fresh leases later.

**When to use**: Expensive setup (install deps once), paused bugs, generated
fixtures. Fork the scenario for repeated test runs without repeating setup.

## Two Checkpoint Types

**Native (provider snapshots)**
- AWS/Azure/GCP: Creates VM disk snapshot at provider level
- Preserves entire machine: packages, tools, caches, services
- Stored in provider account (incurs storage costs)
- Linux only

**Archive (workspace tarball)**
- Creates local tar of workdir
- Portable across any SSH lease
- Preserves files only, not machine state

Default `--mode auto`: native for AWS/Azure/GCP Linux, otherwise archive.

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

# Use forked lease
crabbox run --id purple-whale -- npm test
```

**vs. `crabbox image promote`**: Checkpoints are explicit (fork by ID). Promoted
images change the default AWS runner for all future leases.

## Create

Create checkpoint from existing lease.

```sh
# Auto mode (native for AWS/Azure/GCP, archive otherwise)
crabbox checkpoint create --id blue-lobster --name after-install

# Force native (fails if unsupported)
crabbox checkpoint create --id blue-lobster --mode native --wait

# Force archive (portable tarball)
crabbox checkpoint create --id blue-lobster --mode archive

# Use image strategy instead of disk-snapshot (AWS/GCP only)
crabbox checkpoint create --id blue-lobster --strategy image

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

- `disk-snapshot`: EBS/Azure disk/GCP disk snapshot — faster, best for iteration
- `image`: AMI/GCP machine image — slower, preserves full VM config
- Azure managed images require stopped VMs, not created from active leases

**What gets cleaned before native snapshot**

- `cloud-init clean --logs` — resets cloud-init for fresh SSH keys on fork
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

**Native checkpoints cannot restore**. Fork them instead (creates new lease from
snapshot).

## Fork

Create fresh lease from checkpoint. Works for both native and archive checkpoints.

```sh
crabbox checkpoint fork chk_abc123 --class beast
# Output: checkpoint forked id=chk_abc123 lease=cbx_xyz slug=purple-whale
```

**Flags**

```
--class <class>  Lease class (standard, beast, etc.)
--provider <p>   Provider (aws, azure, gcp, etc.)
--keep           Keep lease running, default true
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

**Native checkpoints (Linux only)**

Default strategy `disk-snapshot`:
- AWS: EBS snapshot
- Azure: Managed OS disk snapshot
- GCP: Persistent disk snapshot

Opt-in strategy `--strategy image`:
- AWS: AMI (Amazon Machine Image)
- Azure: Not created from active VMs (requires stopped/generalized source)
- GCP: Machine image

**Archive checkpoints**

All SSH-accessible leases. Portable across providers.

**Future**: Proxmox VM snapshots, sandbox provider snapshots, storage-backed
snapshots (ZFS, Btrfs, LVM) when Crabbox owns integration.
