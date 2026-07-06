# Sync

Read this when you are:

- changing rsync behavior or the remote sync flow;
- debugging missing, stale, or unexpectedly deleted files on a runner;
- tuning Git seeding, fingerprints, excludes, or large-sync guardrails.

Before running a command, `crabbox run` syncs your current checkout to the
leased runner. Sync only applies to SSH-lease providers; delegated-run providers
own their own file transfer and reject the local sync options. Native Windows
targets use the same file list but ship it as a tar archive over OpenSSH instead
of rsync.

## What gets synced

Sync transfers the Git-managed working set, not the whole directory tree. The
file list comes from `git ls-files --cached --others --exclude-standard -z`,
which is:

- tracked files in the index;
- nonignored untracked files (new files Git would not ignore).

That list is then filtered by the active excludes:

- Crabbox's built-in cache and generated-output excludes;
- repo-local `sync.exclude` (config) patterns;
- root `.crabboxignore` patterns.

Git-ignored output, dependency folders, `.git`, and common local caches stay out
of the transfer. This keeps a first sync close to what CI would see while still
letting you test uncommitted local edits.

The built-in excludes are intentionally conservative. They cover common churn
such as `node_modules`, `.git`, `dist`, `coverage`, `playwright-report`,
`test-results`, `.next`, `.vite`, `.turbo`, `target`, `.venv`, `__pycache__`,
`.gradle`, and the local `.crabbox/logs`, `.crabbox/captures`, and
`.crabbox/runs` directories. Crabbox does not globally drop tracked source files
just because a path segment happens to be named `build` or `out`. Put
project-specific generated directories in `.crabboxignore` or `sync.exclude`.

## Excludes

Patterns match against POSIX-style relative paths. A pattern with no `/` matches
any path segment by name or by glob (for example, `node_modules` or `*.log`);
patterns with a `/` match a path prefix or a glob over the full relative path.

Use `.crabboxignore` when you only need repo-local sync exclusions. The file is
read from the repository root. Blank lines and lines starting with `#` are
ignored; the remaining lines are appended to `sync.exclude` and use the same
matcher as config excludes. Crabbox supports only the exact `.crabboxignore`
name; there is no short alias.

Repo-local config should hold project-specific excludes and env allowlists.
Secrets must never be passed as command-line arguments or via broad env globs.

## Sync flow

For an SSH-lease run, sync runs these steps:

1. Resolve the local repository root.
2. Build the sync manifest (the NUL-delimited file list) and a parallel list of
   tracked paths that were deleted locally.
3. Print a candidate estimate and, when the checkout is dirty, a dirty-delta
   estimate; then enforce the large-sync guardrails (see below).
4. When fingerprinting is enabled, compute a local fingerprint and compare it to
   the remote one. If they match, print
   `No changes detected, skipping sync` and skip the rest.
5. On `--full-resync` / `--fresh-sync`, reset the remote workdir first.
6. Seed the remote Git tree from `origin` at the local `HEAD` when that commit
   is reachable from a remote ref, so rsync only ships the diff.
7. Write the manifest (and the deletion list) to the remote workdir.
8. When delete-sync is enabled, prune previously synced remote files that are no
   longer in the manifest.
9. rsync the working set with `--files-from=- --from0` (the manifest drives the
   transfer).
10. Finalize: git-hydrate the worktree against the configured base ref, run the
    mass-deletion sanity check, and record the new fingerprint.

The remote prune in step 8 only removes paths Crabbox previously synced. It does
not touch workflow-created state, package caches, `.git`, or any other runner
file outside the managed list. The mass-deletion guard in step 10 aborts a sync
that would delete an unexpectedly large fraction of tracked files; set
`CRABBOX_ALLOW_MASS_DELETIONS=1` to override it (this is also implied during
Actions hydration).

On the remote box, sync metadata (including the fingerprint) is stored under
`.git/crabbox` when `.git` is a directory, and under `.crabbox` otherwise. The
`.crabbox/` directory in your repository remains available for repository-owned
files and config; Crabbox does not delete files there.

## Fingerprints and Git seeding

When `sync.fingerprint` is enabled (the default), Crabbox derives a fingerprint
from `HEAD`, the delete/checksum settings, the manifest, the deletion list, the
excludes, and the content of every changed file. If the remote workdir already
carries that fingerprint, the sync is skipped entirely. `--full-resync` ignores
the remote fingerprint and forces a clean transfer.

Git seeding (`sync.gitSeed`, default on) clones or fetches the base tree on the
runner before rsync, so only your diff travels over the wire. It activates only
when the local `HEAD` commit is reachable from a remote ref.
Crabbox disables Git seeding when the origin is an HTTP(S) URL with embedded
userinfo, warns without printing the URL, and uses the normal file sync instead.
This prevents credentials stored in local Git remotes from reaching lease
command arguments or the seeded worktree's Git configuration.

## Large-sync guardrails

`crabbox run` prints a one-line size estimate before transferring. When the
checkout is clean, the candidate counts the full file set. When the checkout is
dirty, the guardrails count the dirty delta (changed plus new files) instead,
but the line still shows the full candidate size so first-sync cost stays
visible:

```text
sync candidate: 299 files, 14.2 MiB dirty_delta=7 files, 92.4 KiB
```

The guardrail scope (candidate or dirty delta) is compared against the warn and
fail thresholds. Crossing a warn threshold prints a warning plus the top source
directories by file count, so accidental dependency repair or generated churn is
easy to spot. Crossing a fail threshold aborts the run.

`crabbox run --force-sync-large` bypasses the fail thresholds for one run.
`--debug` adds rsync progress and stat output; quiet syncs still print a
heartbeat when rsync goes silent for a while.

## Alternatives to syncing the whole checkout

For noisy worktrees, `crabbox run --fresh-pr example-org/my-app#123` is often
faster and clearer than syncing the local checkout. The runner starts from the
PR head; add `--apply-local-patch` to layer your local git diff on top. The
`--fresh-pr` path replaces rsync and cannot be combined with `--no-sync`,
`--sync-only`, or `--full-resync`.

Use `crabbox sync-plan` to inspect the manifest before leasing a box. It prints
the candidate file count, total bytes, the count of deleted tracked paths, and
the largest files and directories, using the same excludes as `run`. Use
`--limit` to change how many top files and directories are listed (default 20).

```text
$ crabbox sync-plan
sync candidate: 299 files, 14.2 MiB
top files:
  3.1 MiB    docs/assets/demo.gif
  ...
top dirs:
  6.4 MiB    docs/assets
  ...
```

## Configuration

Sync defaults (override per repo in config or via env):

```yaml
sync:
  delete: true
  checksum: false
  gitSeed: true
  fingerprint: true
  baseRef: "" # defaults to the repo's origin HEAD / current branch
  timeout: 15m
  warnFiles: 50000
  warnBytes: 5368709120 # 5 GiB
  failFiles: 150000
  failBytes: 21474836480 # 20 GiB
  allowLarge: false
  exclude: []
```

Environment overrides:

```text
CRABBOX_SYNC_CHECKSUM
CRABBOX_SYNC_DELETE
CRABBOX_SYNC_GIT_SEED
CRABBOX_SYNC_FINGERPRINT
CRABBOX_SYNC_BASE_REF
CRABBOX_SYNC_TIMEOUT
CRABBOX_SYNC_WARN_FILES
CRABBOX_SYNC_WARN_BYTES
CRABBOX_SYNC_FAIL_FILES
CRABBOX_SYNC_FAIL_BYTES
CRABBOX_SYNC_ALLOW_LARGE
CRABBOX_ALLOW_MASS_DELETIONS
CRABBOX_ENV_ALLOW
```

## Related docs

- [CLI](../cli.md)
- [run command](../commands/run.md)
- [sync-plan command](../commands/sync-plan.md)
- [Environment forwarding](env-forwarding.md)
- [Repository onboarding](repository-onboarding.md)
