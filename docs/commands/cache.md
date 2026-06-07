# cache

`crabbox cache` inspects, purges, or warms package and build caches on a
leased box. Caches are a speed optimization layered on top of the synced
worktree; the worktree stays authoritative, so cache state is always safe
to clear.

```sh
crabbox cache stats --id swift-crab
crabbox cache stats --id swift-crab --json
crabbox cache volumes
crabbox cache warm  --id swift-crab -- pnpm install --frozen-lockfile
crabbox cache purge --id swift-crab --kind pnpm --force
```

## Subcommands

```text
cache stats   show usage for each enabled cache kind on the lease
cache list    alias for cache stats
cache volumes list configured provider-backed cache volumes
cache warm    run a command in the synced workdir to populate caches
cache purge   delete one or all cache kinds (requires --force)
```

`stats`, `list`, `warm`, and `purge` resolve `--id` to a box and connect over SSH. `--id`
accepts the canonical `cbx_...` lease ID or an active friendly slug
(for example `swift-crab`). If you omit the flag, `stats` and `purge`
fall back to the first positional argument.

Resolving a lease also touches it (resetting the idle timer) and binds
the lease to the current repo via a local claim. If the lease is already
claimed by a different repo checkout, add `--reclaim` to move the claim.

These commands operate over SSH, so they apply to SSH-lease providers.
On Windows leases running in `windows.mode=normal`, remote cache stats
report as unsupported and `cache purge` exits with an error.

## Cache kinds

```text
pnpm     /var/cache/crabbox/pnpm
npm      /var/cache/crabbox/npm
git      /var/cache/crabbox/git   (shared origin objects)
docker   Docker layer/image cache (reported via docker system df)
```

Repo config toggles `cache.pnpm`, `cache.npm`, `cache.docker`, and
`cache.git` control which kinds are enabled. Disabled kinds are omitted
from `stats`, are not removed by `purge --kind all`, and purging a
disabled specific kind fails early with `cache kind "<kind>" is disabled
by config`.

`cache.volumes` define provider-backed persistent cache mounts. They are keyed
speed hints, not checkpoints; the provider may attach them during lease warmup
when it advertises `cache-volume` support. Use `--cache-volume [name=]key:path`
on `warmup` or a fresh `run` to require a volume for that lease; reused
`run --id` leases require local claim metadata proving the volume was attached
during warmup. See [Cache volumes](../features/cache-volumes.md) for the full
contract.

`cache.maxGB` (default `80`) caps total cache size on the runner; the box
trims the oldest entries automatically once caches exceed the cap, so
manual `purge` is rarely needed. See [Cache controls](../features/cache.md)
for the full config reference.

## stats

```sh
crabbox cache stats --id swift-crab
```

Prints one line per enabled cache kind. File-backed kinds (`pnpm`, `npm`,
`git`) show a size and path; `docker` reports the output of
`docker system df`:

```text
pnpm     8.4 GiB                          /var/cache/crabbox/pnpm
npm      1.2 GiB                          /var/cache/crabbox/npm
git      430.0 MiB                        /var/cache/crabbox/git
docker   Images=18.7GB,Containers=0B,Local Volumes=2.1GB
```

Add `--json` to emit the same data as a structured array (`kind`, `path`,
`bytes`, `note`).

## warm

```sh
crabbox cache warm --id swift-crab -- pnpm install --frozen-lockfile
crabbox cache warm --id swift-crab -- docker compose pull
```

Runs a command in the lease's synced repo workdir to prime caches.
Everything after `--id` is the command; a leading `--` separator is
optional. On boxes prepared by `crabbox actions hydrate`, `warm` uses the
hydrated `$GITHUB_WORKSPACE` and sources the workflow env handoff, the
same way [`crabbox run`](run.md) does, and prints which workspace it used.

Use `warm` for one-off cache priming when you do not want to record a
full run-history entry. The command exits with the remote command's exit
code.

## volumes

```sh
crabbox cache volumes
crabbox cache volumes --json
```

Prints configured `cache.volumes` for the current repo. This command does not
connect to a lease; it is a config view that helps confirm which provider cache
volumes will be requested by future `warmup` or one-shot `run` commands. See
[Cache volumes](../features/cache-volumes.md) for provider support and reuse
rules.

## purge

```sh
crabbox cache purge --id swift-crab --kind pnpm --force
crabbox cache purge --id swift-crab --kind all  --force
```

Removes the named cache kind from the box. `--kind` defaults to `all`.
`--force` is required to prevent accidental purges; without it the
command exits with `cache purge requires --force`. For `pnpm`, `npm`, and
`git` this clears the cache directory; for `docker` it runs
`docker system prune -af`. Disabled kinds are skipped even under
`--kind all`.

## Flags

```text
--id <lease-id-or-slug>           target lease (required)
--kind pnpm|npm|docker|git|all    cache kind for purge (default all)
--force                           required for purge
--reclaim                         move the local claim from another repo
--json                            stats output as JSON
```

`stats`/`list` accept `--id`, `--reclaim`, and `--json`. `purge` accepts
`--id`, `--kind`, `--force`, and `--reclaim`. `warm` accepts `--id` and
`--reclaim`, followed by the command to run. `volumes` accepts only `--json`.

## When to use cache

Caches are speed hints, not source of truth, so reaching for them is
always optional.

- Use `cache stats` to confirm a long-lived warm box is gaining benefit
  from cached packages.
- Use `cache warm` to prime a fresh lease before handing it to agents
  that run many short commands.
- Use `cache purge` when a corrupt cache is poisoning a build (rare;
  usually the underlying tool's own cache reset clears it first).
- Use `cache.volumes` when the provider can persist rebuildable cache
  directories beyond a single lease lifetime.

Disposable leases lose cache state when the box is deleted; kept leases
(see [warmup](warmup.md)) reuse cache state across repeated runs. For
shared baked images, see [Prebaked runner images](../features/prebaked-images.md).

## Related docs

- [Cache controls](../features/cache.md)
- [Performance](../performance.md)
- [run](run.md)
- [actions](actions.md)
