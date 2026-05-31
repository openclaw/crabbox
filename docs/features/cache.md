# Cache Controls

Read this when you are tuning warm-box cache behavior, debugging a slow repeated
run, or deciding whether to purge cached state on a reused lease.

A kept lease can reuse build caches across repeated runs, so the second
`crabbox run` against the same box skips re-downloading dependencies. Crabbox
exposes these caches through the `cache` command group and a small `cache:`
config block.

## Where caches live

Runner bootstrap prepares dependency cache roots outside the synced source tree,
so the rsync of your dirty checkout never overwrites them:

```text
/var/cache/crabbox/pnpm
/var/cache/crabbox/npm
/var/cache/crabbox/git    (created on first use)
Docker local image/layer cache (managed by Docker)
```

The `pnpm` and `npm` roots are created and chowned during bootstrap. The `git`
root is reported and purged when present but is populated lazily by your
commands. The `docker` cache is the daemon's own image/layer store, inspected
via `docker system df`.

These caches are speed hints, not source of truth. The synced worktree stays
authoritative. A disposable lease loses all cache state when its VM is deleted;
only a kept lease (see [`lifecycle-cleanup.md`](lifecycle-cleanup.md)) carries
cache state forward.

## Cache volumes

Provider-backed cache volumes persist rebuildable cache state across fresh
leases. They are configured under `cache.volumes`, requested with
`--cache-volume [name=]key:path`, and inspected with `crabbox cache volumes`.
See [Cache volumes](cache-volumes.md) for the full feature contract, provider
support rules, and the boundary with images and checkpoints.

## Config

Set the cache policy in any [config file](configuration.md) under `cache:`:

```yaml
cache:
  pnpm: true
  npm: true
  docker: true
  git: true
  maxGB: 80
  purgeOnRelease: false
```

Defaults: all four kinds enabled, `maxGB: 80`, `purgeOnRelease: false`. Each key
also has an environment override:

| Config key       | Env var                          |
| ---------------- | -------------------------------- |
| `pnpm`           | `CRABBOX_CACHE_PNPM`             |
| `npm`            | `CRABBOX_CACHE_NPM`              |
| `docker`         | `CRABBOX_CACHE_DOCKER`           |
| `git`            | `CRABBOX_CACHE_GIT`              |
| `maxGB`          | `CRABBOX_CACHE_MAX_GB`           |
| `purgeOnRelease` | `CRABBOX_CACHE_PURGE_ON_RELEASE` |
| `volumes`        | `CRABBOX_CACHE_VOLUMES`          |

The per-kind toggles drive `cache stats` and `cache purge`: a disabled kind is
omitted from stats output and is skipped by `--kind all`. Asking to purge a
disabled kind directly (for example `--kind docker` with `docker: false`) fails
early. Bootstrap may still create the shared `pnpm`/`npm` directories regardless,
since empty cache roots are harmless scaffolding.

`maxGB` and `purgeOnRelease` are surfaced by `crabbox config show` but are
advisory: they are not currently enforced by the `cache` commands. Use
`cache purge` to reclaim space explicitly.

## Commands

All cache commands target one lease via `--id <lease-id-or-slug>` (also accepted
as a positional argument), claim the lease for the current repo with
`--reclaim`, and touch the lease so it does not idle out while you work.

### `cache stats` (alias `cache list`)

Print per-kind cache usage for a lease.

```sh
crabbox cache stats --id swift-crab
crabbox cache list --id swift-crab --json
```

Flags: `--id`, `--reclaim`, `--json`. Output lists each enabled kind with its
size and path; the `docker` row reports the `docker system df` breakdown.

### `cache warm`

Run a cache-populating command in the lease's repo workdir, streaming output.
Useful for seeding dependencies on a fresh warm box before real work.

```sh
crabbox cache warm --id swift-crab -- pnpm install --frozen-lockfile
```

Flags: `--id`, `--reclaim`. The command after `--` runs in the synced repo
directory (or the GitHub Actions workspace if the lease was hydrated via
[Actions hydration](actions-hydration.md)), with profile-allowed environment
applied.

### `cache volumes`

List configured cache volumes for the current repo:

```sh
crabbox cache volumes
crabbox cache volumes --json
```

This is a configuration view. Provider-specific attach/mount state is reported
by the provider run output or future provider diagnostics.

### `cache purge`

Remove cached content. Requires `--force` to confirm.

```sh
crabbox cache purge --id swift-crab --kind pnpm --force
crabbox cache purge --id swift-crab --force            # --kind defaults to all
```

Flags: `--id`, `--kind pnpm|npm|docker|git|all` (default `all`), `--force`,
`--reclaim`. The directory caches are cleared with `rm -rf`; `docker` runs
`docker system prune -af`.

## Windows-native leases

For `--target windows --windows-mode normal` leases, the Linux cache roots do
not apply: `cache stats` reports the caches as unsupported and `cache purge`
returns an error. WSL2 Windows leases behave like Linux. See
[`vnc-windows.md`](vnc-windows.md) and the network/target notes in
[`network.md`](network.md) for related Windows-target details.
