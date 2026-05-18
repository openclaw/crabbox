# Performance

Read when:

- making remote runs faster;
- choosing machine classes;
- changing sync behavior;
- tuning Actions hydration.

Crabbox performance comes from avoiding repeated setup, keeping the sync small, choosing available capacity, and reusing project-defined hydration when it matters.

## High-Latency Links

Crabbox should not require a special slow-network mode. The CLI keeps SSH as
the universal command transport, but uses SSH ControlMaster with a longer
persist window so repeated probes, sync helpers, and commands avoid paying a
new handshake every time. Streaming commands retry coordinator-provided
fallback ports just like readiness and helper probes.

When the broker supports it, `crabbox attach` and lease heartbeats use one
authenticated coordinator WebSocket instead of repeated HTTP polls. If the
socket cannot connect or drops, the CLI resumes through the existing HTTPS API
from the last acknowledged run-event sequence. WebSocket attach still catches
up from older sequences in bounded pages before switching to live pushed
events, so reconnects do not skip retained output.

## Warm Leases

Use `warmup` for repeated agent loops:

```sh
bin/crabbox warmup --class beast
bin/crabbox run --id blue-lobster -- pnpm test:changed:max
```

Warm leases avoid waiting for a fresh VM and preserve package caches outside the synced source tree. They release after the idle timeout, default `30m`, if untouched. Use `crabbox stop blue-lobster` when the loop is done.

## Sync Size

The CLI syncs a Git-derived manifest: tracked files plus nonignored untracked files. Ignored build output, dependency folders, `.git`, and common local caches are excluded before rsync sees the tree. Default excludes also cover common generated churn such as `.ignored`, `.vite`, `playwright-report`, `test-results`, and local `.crabbox` log/capture directories. Each run prints the full candidate file count plus the dirty-delta count when the checkout has local changes. Large-sync guardrails use that dirty delta when present, so a dirty worktree with a small intended patch is not blocked just because the complete source manifest is larger.

Large sync warnings include the top source directories by file count, which makes accidental dependency or build-output syncs easier to spot before retrying.

Good habits:

- keep generated artifacts and dependency folders out of the synced tree;
- tune repo-local excludes in `.crabbox.yaml`;
- keep `.gitignore` current so local build junk never enters the manifest;
- use `crabbox run --fresh-pr <owner/repo#number> --apply-local-patch` when a dirty local worktree has unrelated dependency repair or generated churn;
- raise `sync.failFiles` or `sync.failBytes` only for projects that intentionally sync very large source trees.

## Sync Fingerprints

The CLI records a local/remote fingerprint after sync. If nothing changed, hot runs skip the expensive rsync pass. The fingerprint includes the commit, dirty metadata, sync config, and manifest, so adding a nonignored untracked file invalidates the skip while ignored cache churn does not.

Good habits:

- avoid broad local deletes unless they are intentional;
- use `inspect` when diagnosing stale remote state.

## Git Hydration

Crabbox seeds remote Git when possible, then overlays the dirty local checkout with rsync. It also hydrates configured base-ref history so changed-file commands can compare against the expected base.

This matters for commands such as:

```sh
pnpm test:changed:max
pnpm check:changed
git diff --name-only origin/main...
```

When a box is already Actions-hydrated and the remote checkout already has the configured base ref at the same SHA as the local `origin/<baseRef>`, Crabbox skips the extra Git hydration fetch and records the skip reason in the sync summary. This keeps dirty-overlay reruns focused on rsync plus the command instead of repeatedly fetching base history.

For PR iteration from a noisy local checkout, prefer:

```sh
bin/crabbox run --fresh-pr owner/repo#123 --apply-local-patch -- pnpm test:changed
```

That creates a clean remote PR checkout, applies only the local diff as a small patch, and avoids syncing unrelated local dependency or build-output churn.

## Package And Tool Caches

Runner bootstrap prepares shared cache directories, but does not install project runtimes. Package-manager and Docker caches are best-effort speedups once the repository setup installs those tools; they must not be treated as source of truth.

Use explicit cache commands on kept leases:

```sh
bin/crabbox cache stats --id blue-lobster
bin/crabbox cache warm --id blue-lobster -- pnpm install --frozen-lockfile
bin/crabbox cache purge --id blue-lobster --kind pnpm --force
```

For repeatable setup, use Actions hydration:

```sh
bin/crabbox actions hydrate --id blue-lobster
bin/crabbox run --id blue-lobster -- pnpm test:changed:max
```

The workflow owns dependency installation, cache setup, and project tooling. Crabbox attaches later commands to the hydrated workspace. Use `crabbox actions hydrate --github-runner` when the setup needs repository secrets, OIDC, service containers, or unsupported Actions features.

For live/provider E2E loops, keep two lanes:

- Source-backed PR iteration: use `--fresh-pr ... --apply-local-patch` plus the
  smallest smoke command that proves the changed source path.
- Package-backed release proof: reuse a built package tarball or prebuilt
  functional image across repeated Testbox runs, and rebuild it only when the
  live-image inputs changed.

That split keeps fast debugging source-based while preserving one slower
package-backed lane for release confidence.

## Machine Classes

Use the smallest class that keeps the target command CPU-bound without creating queue or quota failures.

Typical choices:

- `standard`: cheap smoke checks and small repos.
- `fast`: general maintainer testing.
- `large`: broad test shards or heavy builds.
- `beast`: high-core changed-test runs.

Hetzner dedicated classes can hit account quota. AWS Spot classes can hit regional capacity or account policy limits. For AWS, class requests try the configured high-core candidates first and can fall back to a small burstable type when the account rejects those candidates. Multiple `CRABBOX_CAPACITY_REGIONS` let brokered and direct AWS launches move to another region before giving up.

## Measure The Loop

Use wall-clock timing around the whole command, not just the remote test process:

```sh
/usr/bin/time -p bin/crabbox run --id cbx_... -- pnpm test:changed:max
```

The useful number includes lease wait, SSH readiness, sync, Git hydration, command execution, and release. Add `--timing-json` when comparing providers or checking whether a run paid for `rsync`, `git_hydrate`, or only the remote command. For warm leases, sync fingerprints and package caches should make repeated runs much faster than cold runs.
