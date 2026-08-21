# Cache Volumes

Read this when:

- repeated fresh leases spend most of their time rebuilding dependency caches;
- a provider can attach persistent, rebuildable cache storage during warmup;
- you need to decide whether a cache belongs in a volume, a checkpoint, an image, or the synced worktree.

Cache volumes are provider-backed persistent mount points for speed-only state.
They are not checkpoints and not source storage. The synced worktree stays
authoritative, and volume contents must be safe to delete and rebuild.

## Contract

A cache volume has:

- `key`: the provider cache identity;
- `path`: the absolute mount path on the remote box;
- `name`: an optional local label for humans;
- `sizeGB`: an optional size hint for providers that support sizing;
- `required`: whether Crabbox must fail instead of silently ignoring the volume.

Put provider cache paths outside the synced source tree. Prefer
`/var/cache/crabbox/<kind>` for package-manager stores and other dependency
caches. Do not store secrets, checkout state, build artifacts that are the
result under test, proof bundles, screenshots, or logs in cache volumes.

Choose a key that changes whenever the cached bytes become incompatible. Include
the repository, target OS, architecture, runtime, package manager, lockfile hash,
and image or workflow generation when those values affect cache contents.

## Configuration

Configure volumes under `cache.volumes`:

```yaml
cache:
  volumes:
    - name: pnpm-store
      key: my-app-linux-amd64-node24-pnpm10-lockhash
      path: /var/cache/crabbox/pnpm
      sizeGB: 80
      required: false
```

An explicit empty list clears inherited volumes from lower-precedence config:

```yaml
cache:
  volumes: []
```

The environment equivalent is comma-separated:

```sh
CRABBOX_CACHE_VOLUMES=pnpm-store=my-app-linux-amd64-node24-pnpm10-lockhash:/var/cache/crabbox/pnpm
```

## CLI

For one-off lease creation, use repeatable `--cache-volume` flags:

```sh
crabbox warmup --provider blacksmith-testbox \
  --cache-volume pnpm-store=my-app-linux-amd64-node24-pnpm10-lockhash:/var/cache/crabbox/pnpm
```

Flag-provided volumes merge with configured volumes. If the same `key:path`
already exists in config, the flag marks that existing volume required for the
lease instead of duplicating it.

Use `crabbox cache volumes` to inspect the resolved config:

```sh
crabbox cache volumes
crabbox cache volumes --json
```

This command only reads local configuration. It does not connect to a lease.

## Provider Support

Providers advertise cache volume support with the `cache-volume` feature.
Blacksmith Testbox implements the feature by forwarding each resolved volume as
a `blacksmith testbox warmup --sticky-disk key:path` argument. Local Container
implements it with Docker named volumes mounted at the configured paths; the
Docker volume name is derived from the cache key. Apple Container implements it
with host cache directories under the local user cache directory mounted with
Apple's `--volume` flag.

AWS implements cache volumes for public Linux SSH leases in both direct and
brokered mode. Each logical cache gets one encrypted gp3 EBS member per
availability zone; concurrent leases reserve distinct members and sequential
leases reuse an available member. Required caches reject Windows, macOS,
private/SSM-only AWS workspaces, and coordinators that do not acknowledge cache
volume protocol v1. Optional caches may be ignored only with an explicit CLI
warning.

Providers that do not advertise `cache-volume` ignore non-required configured
volumes. Required volumes fail early when the selected provider cannot honor
them.

### AWS safety model

AWS cache identity binds the authenticated account/tenant, an opaque repository
scope, the cache key, and the cache ABI. EBS tags contain only random cache-set
and member IDs, generation, and an ABI digest; repository names, keys, paths,
users, and credentials are never tags.

Crabbox durably reserves the exact availability zone before `RunInstances`,
creates an ext4 volume with `DeleteOnTermination=false`, and attaches it only to
the selected instance. Bootstrap resolves the exact EBS NVMe serial, rejects
root devices and mount paths that hide the SSH home, work root, cloud-init, or
Crabbox runtime state. Descendants of the SSH `.ssh` directory,
`/var/lib/cloud`, and `/var/lib/crabbox` are also forbidden. It rechecks the
created path for indirect symlinks, mounts with `nodev,nosuid`, and gates
readiness on the ABI sentinel. Explicit cache subdirectories such as
`/var/cache/crabbox/pnpm`,
`/home/crabbox/.cache/build`, or a cache directory below the work root remain
valid. ABI changes quarantine the old member and allocate a new generation. An
exclusive same-ABI member may be repaired with `e2fsck` and reformatted when the
cache is corrupt.

Every adoption and reuse re-attests the live EBS member: encrypted, `gp3`, exact
recorded size, multi-attach disabled, exact availability zone, opaque ownership
tags, state, and attachments. Exact-tag clones with different storage
properties are quarantined. Failed creates and indeterminate
`DescribeVolumes`/discovery calls become non-capacity-counting durable cleanup
records with retry evidence, and stale records with no provider match are
removed without discovering or deleting tag-only resources.

Fixed create identity remains v2 when no cache volumes are requested. Cache
volumes use semantic identity v3 and bind the opaque repository scope; runtime
lifecycle adapters and resolved volume plans are deliberately excluded.

Release marks a member available only after `DescribeVolumes` proves zero
attachments. `cache.purgeOnRelease` deletes it after that proof. Direct state is
kept in a locked XDG registry; brokered state is durable coordinator state.
The generated AWS policy grants cache volume mutation only on volume ARNs with
the exact Crabbox cache resource tags; the companion instance ARN grant covers
only attach and detach and requires the existing Crabbox instance ownership
tags.
Aged available or quarantined members are garbage-collected after seven days
only when their durable account, region, availability zone, generation, ABI,
and opaque tags still match. Tag-only discoveries remain report-only.

## Existing Leases

Cache volumes are attached during warmup. Crabbox records the attached
`key:path` specs in the local lease claim. A later `crabbox run --id <lease>`
with required volumes is allowed only when the local claim proves those volumes
were attached during warmup. If the claim is missing or does not include a
required volume, warm a new lease.

## Boundary With Images And Checkpoints

Use cache volumes for rebuildable, mutable speed state such as package-manager
stores. Use provider images for stable machine setup, tools, runtimes, and OS
packages. Use workspace checkpoints for reusable source/workspace state that
should be restored or forked intentionally.
