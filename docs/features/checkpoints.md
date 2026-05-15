# Checkpoints

Crabbox checkpoints save prepared remote state for reuse.

**Goal**: Skip expensive setup. Install dependencies once, pause a bug at a useful
state, keep generated fixtures, then fork that scenario for repeated test runs.

**Key distinction**: Checkpoints are explicit scenario handles, not default images.
Use `crabbox checkpoint fork <id>` to start a fresh lease from a saved scenario.
Use `crabbox image promote` to change the default AWS runner image for all future
leases.

## Two Modes

**Native (Provider Snapshots)**
- Snapshots entire VM disk at provider level (AWS/Azure/GCP)
- Preserves full machine state: packages, tools, caches, services, files
- Fast to create and fork (leverages cloud-native snapshots)
- Lives in provider account, incurs storage costs
- Linux only (Windows not supported)

**Archive (Workspace Tarball)**
- Captures workdir files only, stores locally as tarball
- Portable across any SSH-accessible lease
- Does not preserve system packages or machine state
- Excludes `.crabbox/env` and `.crabbox/scripts` by default

Default `auto` mode: native for brokered AWS/Azure/GCP Linux leases, otherwise
archive.

## Native Checkpoint Strategies

Native checkpoints use two provider primitives:

**Disk-Snapshot Strategy (default)**
- AWS: EBS snapshot (`aws-ebs-snapshot`)
- Azure: Managed OS disk snapshot (`azure-os-disk-snapshot`)
- GCP: Persistent disk snapshot (`gcp-disk-snapshot`)
- Faster to create, boots with fresh SSH keys
- Best for iterative development

**Image Strategy (opt-in with `--strategy image`)**
- AWS: AMI (`aws-ami`)
- Azure: Managed image (`azure-managed-image`) — read-only, not created from active VMs
- GCP: Machine image (`gcp-machine-image`)
- Slower but preserves complete VM configuration
- Best for distribution or long-term storage

**Metadata-Only (`recipe`)**
- Records lease/repo/workdir info without creating artifacts
- Not yet supported by fork/restore commands

## Security First

**⚠️ Native checkpoints may contain secrets**

Native snapshots capture full root volume state: system files, logs, caches,
installed packages, and any secrets written to disk during setup. Treat them as
sensitive provider artifacts.

- Delete when no longer needed (storage costs accrue)
- Do not create from ad-hoc debugging sessions with temporary secrets
- Provider resources remain even if local metadata is lost

**Workspace archives may contain secrets too**

Archives capture workdir contents: build outputs, caches, logs, generated files.
Crabbox excludes `.crabbox/env` and `.crabbox/scripts` by default but does not
scan for credentials in arbitrary files.

**Local metadata is required**

Checkpoints store metadata locally (`~/.local/state/crabbox/checkpoints/`) with
provider resource IDs and regions. Losing local metadata means you cannot fork
by checkpoint ID. Deleting provider resources means local metadata is insufficient
to fork.

## Workflow

**Basic flow**

```sh
crabbox warmup --provider aws --class beast
crabbox run --id blue-lobster --shell 'npm ci && npm test'
crabbox checkpoint create --id blue-lobster --name after-npm-ci
# Creates chk_abc123

crabbox checkpoint fork chk_abc123 --class beast
# Prints forked lease slug: purple-whale
crabbox run --id purple-whale -- npm test
```

**What happens on create**

Native checkpoints:
1. Flush filesystem writes (`sync`)
2. Reset cloud-init state (allows fresh SSH keys on fork)
3. Call provider snapshot/image API (EBS snapshot, AMI, etc.)
4. Save local metadata with provider resource ID and region

Archive checkpoints:
1. SSH into lease
2. Tar workdir (excluding `.crabbox/env` and `.crabbox/scripts`)
3. Download tarball to `~/.local/state/crabbox/checkpoints/chk_*/workspace.tar.gz`
4. Save local metadata

**What happens on fork**

Native checkpoints:
1. Acquire new lease from provider using checkpoint snapshot/image
2. Wait for boot
3. Relocate snapshotted workdir to new lease's standard path
   (`/work/cbx_old/repo` → `/work/cbx_new/repo`)
4. Keep lease running for immediate use

Archive checkpoints:
1. Acquire standard new lease
2. Upload tarball via SSH
3. Extract to workdir
4. Keep lease running

**Azure disk-snapshot specifics**

Azure disk-snapshot forks boot from a specialized OS disk and may inherit source
machine identity. Treat them as exact-clone snapshots until Crabbox implements
stronger post-boot reset. AWS and GCP disk-snapshots inject fresh user-data for
per-lease SSH keys.

## When To Use Checkpoints

**Use native checkpoints when machine setup is slow**

Expensive system packages, heavy toolchain installs, large dependency downloads:

```sh
crabbox warmup --provider aws --class beast
crabbox run --id blue-lobster --shell 'sudo apt install -y cuda-toolkit && npm ci'
crabbox checkpoint create --id blue-lobster --name cuda-ready
crabbox checkpoint fork chk_123 --class beast
```

Works identically for AWS, Azure, and GCP Linux leases.

**Use workspace archives when repo state is valuable**

Paused bugs, generated fixtures, build artifacts, test failures:

```sh
crabbox checkpoint create --id blue-lobster --mode archive --name failing-fixtures
crabbox checkpoint fork chk_123 --class standard
```

Portable across all SSH-accessible leases.

**Use promoted images instead for default base images**

If the prepared machine should become the standard image for all future AWS
leases, use `crabbox image promote` instead of checkpoints. Checkpoints are
explicit scenario handles; promoted images change the global default.

## Use Cases

- Fast test iteration: `npm ci` once, fork for each test run
- Heavy toolchains: Install CUDA, Android SDK, Xcode once
- Paused debugging: Capture exact failure state, fork to investigate
- Generated fixtures: Keep expensive setup data, fork for clean runs
- CI optimization: Prebake common environments, fork in parallel jobs
- Team sharing: Distribute snapshot IDs for consistent environments (native only)

## Future Expansion

Additional native checkpoint backends require:

- Stable snapshot creation from active lease
- Fork snapshot into new lease
- Predictable restore/delete operations
- Clear cost, retention, security boundaries

Proxmox VM snapshots/clones are the next natural fit. Plain SSH providers should
not expose native checkpoint features unless the target host provides a real
snapshot API (ZFS, Btrfs, LVM) and Crabbox owns the integration.
