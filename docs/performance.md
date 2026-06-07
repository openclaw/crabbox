# Performance

Read this when you want to:

- make remote runs faster;
- choose a machine class;
- shrink or tune what gets synced;
- get the most out of Actions hydration.

Crabbox runs commands on a remote box but executes the data plane — SSH, rsync,
and the command itself — directly from your machine to the runner. Performance
comes from four levers: avoid repeated setup, keep the sync small, pick capacity
that is actually available, and reuse project-defined hydration when it pays off.

## High-latency links

There is no special slow-network mode. SSH stays the universal command
transport, but the CLI enables SSH `ControlMaster` with a `ControlPersist`
window (10 minutes) so repeated readiness probes, sync helpers, and commands
reuse one connection instead of paying a fresh handshake each time. Connection
multiplexing is disabled only when a target requires an auth secret. Streaming
commands retry coordinator-provided SSH fallback ports, just like readiness and
helper probes.

When you use a broker (coordinator), `crabbox attach` and lease heartbeats use a
single authenticated coordinator WebSocket (`/v1/control`) instead of repeated
HTTP polls. If that socket cannot connect or drops, the CLI falls back to the
HTTPS run-events API and resumes from the last acknowledged event sequence, so
reconnects do not skip retained output.

## Warm leases

For repeated agent loops, lease a box once and reuse it:

```sh
bin/crabbox warmup --class beast
bin/crabbox run --id swift-crab -- pnpm test:changed:max
```

A warm lease skips the wait for a fresh VM and preserves package caches that live
outside the synced source tree. Warm leases release automatically after the idle
timeout (default `30m`) if left untouched. End the loop explicitly with:

```sh
bin/crabbox stop swift-crab
```

You can pass the lease slug (for example `swift-crab`) or its canonical id
(`cbx_…`) to `--id`.

## Sync size

`crabbox run` syncs a Git-derived manifest: tracked files plus non-ignored
untracked files. Ignored build output, dependency folders, `.git`, and common
local caches are excluded before rsync sees the tree. Default excludes also cover
frequent generated churn such as `.ignored`, `.vite`, `playwright-report`,
`test-results`, and local `.crabbox` log/capture directories.

Each run prints the full candidate file count, plus a dirty-delta count when the
checkout has local changes. The large-sync guardrails use the dirty delta when
present, so a dirty worktree with a small intended patch is not blocked just
because the complete source manifest is larger. When a sync is flagged as large,
the warning lists the top source directories by file count, which makes an
accidental dependency or build-output sync easy to spot before retrying.

Preview the manifest without touching a box:

```sh
bin/crabbox sync-plan
```

Good habits:

- keep generated artifacts and dependency folders out of the synced tree;
- tune repo-local excludes in `.crabbox.yaml`;
- keep `.gitignore` current so local build junk never enters the manifest;
- when a dirty local worktree carries unrelated dependency repair or generated
  churn, prefer `crabbox run --fresh-pr <owner/repo#123> --apply-local-patch`;
- raise `sync.failFiles` or `sync.failBytes` (defaults: 150,000 files / 20 GiB)
  only for projects that intentionally sync very large source trees.

## Sync fingerprints

After each sync the CLI records a local and remote fingerprint. If nothing
changed, hot reruns skip the expensive rsync pass entirely. The fingerprint
covers the commit, dirty metadata, sync config, and manifest, so adding a
non-ignored untracked file invalidates the skip while ignored cache churn does
not.

Good habits:

- avoid broad local deletes unless they are intentional;
- use `crabbox inspect` when diagnosing stale remote state.

## Git hydration

Crabbox seeds remote Git when possible, then overlays the dirty local checkout
with rsync, so only the diff travels over the wire. It also hydrates the
configured base-ref history so changed-file commands can compare against the
expected base:

```sh
pnpm test:changed:max
pnpm check:changed
git diff --name-only origin/main...
```

When a box is already Actions-hydrated and its remote checkout has the configured
base ref at the same SHA as the local `origin/<baseRef>`, Crabbox skips the extra
Git hydration fetch and records the skip reason in the sync summary. This keeps
dirty-overlay reruns focused on rsync plus the command, instead of repeatedly
refetching base history.

For PR iteration from a noisy local checkout, prefer a fresh remote PR checkout
with only your local diff applied as a small patch:

```sh
bin/crabbox run --fresh-pr owner/repo#123 --apply-local-patch -- pnpm test:changed
```

This avoids syncing unrelated local dependency or build-output churn.
`--fresh-pr` cannot be combined with `--no-sync`, `--sync-only`, or
`--full-resync`, and `--apply-local-patch` requires `--fresh-pr`.

## Package and tool caches

Runner bootstrap prepares shared cache directories but does not install project
runtimes. Package-manager and Docker caches are best-effort speedups once your
repository setup installs those tools; they must not be treated as a source of
truth.

Inspect and manage caches on a kept lease:

```sh
bin/crabbox cache stats --id swift-crab
bin/crabbox cache warm  --id swift-crab -- pnpm install --frozen-lockfile
bin/crabbox cache purge --id swift-crab --kind pnpm --force
```

`--kind` accepts `pnpm`, `npm`, `docker`, `git`, or `all` (the default).

For repeatable setup, use Actions hydration so the repo's own workflow installs
dependencies, configures caches, and provisions tooling:

```sh
bin/crabbox actions hydrate --id swift-crab
bin/crabbox run --id swift-crab -- pnpm test:changed:max
```

The workflow owns dependency installation; Crabbox attaches later commands to the
hydrated workspace. Use `crabbox actions hydrate --github-runner` when setup
needs repository secrets, OIDC, service containers, or other Actions features that
require a self-hosted runner.

For live or provider end-to-end loops, keep two lanes:

- **Source-backed PR iteration** — `--fresh-pr … --apply-local-patch` plus the
  smallest smoke command that proves the changed source path.
- **Package-backed release proof** — reuse a built package tarball or prebuilt
  functional image across repeated runs, rebuilding only when the image inputs
  change.

That split keeps fast debugging source-based while preserving one slower,
package-backed lane for release confidence.

## Machine classes

Pick the smallest class that keeps the target command CPU-bound without hitting
queue or quota failures. Typical choices:

- `standard` — cheap smoke checks and small repos.
- `fast` — general maintainer testing.
- `large` — broad test shards or heavy builds.
- `beast` — high-core changed-test runs.

Capacity caveats:

- Hetzner dedicated classes can hit account quota.
- AWS Spot classes can hit regional capacity or account-policy limits. For AWS, a
  class request tries the configured high-core candidates first and can fall back
  to a small burstable type when the account rejects them. Setting
  `CRABBOX_CAPACITY_REGIONS` (a comma-separated region list) lets brokered and
  direct AWS launches move to another region before giving up.

## Measure the loop

Time the whole command, not just the remote test process:

```sh
/usr/bin/time -p bin/crabbox run --id cbx_... -- pnpm test:changed:max
```

The useful number includes lease wait, SSH readiness, sync, Git hydration,
command execution, and release. Add `--timing-json` when comparing providers or
checking whether a run paid for `rsync`, `git_hydrate`, or only the remote
command:

```sh
bin/crabbox run --id swift-crab --timing-json -- pnpm test:changed:max
```

For warm leases, sync fingerprints and package caches should make repeated runs
much faster than cold runs.
