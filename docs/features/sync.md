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

`--no-sync` skips local file transfer only on providers that support it.
Blacksmith Testbox rejects it before lease access or execution because native
Testbox runs own sync and offer no supported bypass, including when reusing an
ID. See [Blacksmith Testbox](../providers/blacksmith-testbox.md).

Skipping sync does not skip provider initialization. Generated prewarm probes
are admitted before backend configuration or warmup.

## Remote workspace path

For normal SSH-backed runs, sync starts from the effective work root and derives
the repository workspace as `<root>/<lease>/<repository>`. Top-level
`workRoot` and `CRABBOX_WORK_ROOT` change only `<root>`; they do not name the
exact sync target or command working directory. An explicitly configured
provider-specific work root or workdir takes precedence over the generic root
and remains subject to that adapter's validation and path translation.

Actions hydration has final authority over the exact workspace. When a lease
has a valid hydration marker, Crabbox uses the marker's canonical `WORKSPACE`
for both sync and command execution instead of the base-derived candidate.
Changing `CRABBOX_WORK_ROOT` does not relocate an already adopted Actions
workspace. Local automatic hydration uses the canonical lease workspace it
derived before writing the marker; `--full-resync` refuses a noncanonical
adopted workspace when it cannot safely rebuild that path. See
[Actions hydration](actions-hydration.md) for the marker lifecycle.

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

Before transfer, Crabbox checks tracked paths that remain in the effective
manifest scope. If sparse-checkout rules or `skip-worktree` state hide one of
those paths, sync stops instead of treating the omission as a deletion. Hidden
paths outside `sync.include` or removed by ordered excludes are ignored.
Gitlinks are not manifest files or remote file deletions, while symlinks remain
file-like.

Git 2.41 or newer distinguishes an intentional in-scope deletion from a sparse
omission after index metadata becomes ambiguous. Older Git fails closed only
for an ambiguous missing path that remains in the effective manifest scope.

Git-ignored output, dependency folders, `.git`, and common local caches stay out
of the transfer. This keeps a first sync close to what CI would see while still
letting you test uncommitted local edits.

Filesystem Git origins are resolved on the runner during Git seeding and must
be readable from that runner; otherwise Crabbox falls back to a full manifest sync.

### Jujutsu workspaces

Crabbox currently supports Jujutsu workspaces only when they are colocated with
Git metadata: the workspace root must contain both `.jj` and `.git`. Native
Jujutsu revision mapping is not supported yet. Because the sync manifest is
Git-owned, Crabbox rejects a native `.jj` workspace before leasing or borrowing
a runner rather than letting Git discover an outer checkout and sync the wrong
revision. This also applies when the native workspace is nested inside an outer
Git repository.

If you are starting from an existing Git checkout and want a colocated Jujutsu
workspace, `jj git init --git-repo=.` is one initialization example. It does not
convert an existing native Jujutsu repository in place. Use `--no-sync` with a
supporting provider when you intentionally want to run without transferring
local files.

The built-in excludes are intentionally conservative. They cover common churn
such as `node_modules`, `.git`, `dist`, `coverage`, `playwright-report`,
`test-results`, `.next`, `.vite`, `.turbo`, `target`, `.venv`, `__pycache__`,
`.gradle`, and Crabbox runtime state under `.crabbox/env`,
`.crabbox/scripts`, `.crabbox/logs`, `.crabbox/captures`, and
`.crabbox/runs`. Built-in rules for the ambiguous artifact names `dist`,
`dist-runtime`, `coverage`, `playwright-report`, `test-results`, `.build`, and
`target` still omit untracked output, but do not omit a Git-tracked regular file
solely because one of those names appears in its path. Crabbox reports a bounded
path-and-pattern summary when it protects such files. Unmistakable dependency
and cache rules such as `node_modules`, `.cache`, `.venv`, and `__pycache__`
remain component-wide, including for tracked files.

Except for the protected Crabbox runtime state described below, rules from
`sync.exclude` and `.crabboxignore` are authoritative, including bare
component-wide patterns. They can deliberately exclude tracked artifact files
or trees, and a later `!pattern` can re-include them. This keeps existing
repository policy intact across upgrades while making Crabbox-owned ambiguous
defaults safe. Crabbox also does not globally drop tracked source files just
because a path segment happens to be named `build` or `out`. Put project-specific
generated directories in `.crabboxignore` or `sync.exclude`.

`crabbox watch` observes only the ancestor chains needed by tracked protected
files or explicit re-includes, so unrelated untracked artifact trees do not
create watch churn. It also watches Git's resolved index and attaches the parent
chain when an index-only transition makes an artifact path tracked.

## Excludes

Patterns match against POSIX-style relative paths. A pattern with no `/` matches
any path segment by name or by glob (for example, `node_modules` or `*.log`);
patterns with a `/` match a path prefix or a glob over the full relative path.
Rules are evaluated in order and the last matching rule wins. Prefix a pattern
with `!` to re-include a path excluded by an earlier rule, including a built-in
default; prefix a literal leading `!` with a backslash (`\!cache`). For example:

```gitignore
# Keep generated target directories excluded, except this source package.
target
!apps/backend/app/connectors/target
```

Use `.crabboxignore` when you only need repo-local sync exclusions. The file is
read from the repository root. Blank lines and lines starting with `#` are
ignored; the remaining lines are appended to `sync.exclude` and use the same
matcher as config excludes. Crabbox supports only the exact `.crabboxignore`
name; there is no short alias.

Crabbox-owned runtime state under `.crabbox/env`, `.crabbox/scripts`,
`.crabbox/logs`, `.crabbox/captures`, and `.crabbox/runs` is always excluded
after repo rules are applied. Those paths can contain forwarded env profiles,
uploaded scripts, local run artifacts, or failure bundles, so `.crabboxignore`
cannot re-include them. Case aliases of these reserved paths are protected too,
including on case-insensitive filesystems.

If a project stores source files in one of these reserved directories, move
them elsewhere before upgrading; reserved runtime paths are no longer eligible
for sync even when they are tracked or explicitly re-included.

Repo-local config should hold project-specific excludes and env allowlists.
Secrets must never be passed as command-line arguments or via broad env globs.

## Sync flow

For an existing SSH lease, Crabbox first acquires a remote lease-scoped
workspace owner. It does this before reading hydration state, Git metadata, or
the sync fingerprint, and retains ownership through command execution, evidence
collection, failure capture, and ready-pool cleanup. Separate clients and watch
iterations contend on the same owner. Newly acquired one-shot leases bypass it
because the acquisition itself is exclusive.

The owner state lives under the remote user's Crabbox state directory, outside
the replaceable checkout. Its filename is derived from a non-reversible lease
digest, and its bounded contents contain only protocol version, expiry, random
fencing token, and an optional witnessed child PID/start identity. Token-bound
renewal and release fail closed. After a client crash, an expired owner is
recoverable only when the exact witnessed child is no longer alive. POSIX,
WSL2, and native Windows targets share these semantics.

Once ownership is established, sync runs these steps:

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

### Opt-in Git overlay

Set `sync.gitOverlay: true` or `CRABBOX_SYNC_GIT_OVERLAY=true` to let eligible
Linux SSH-backed runners fetch the exact advertised local commit and transfer
only the files that differ from that commit. A clean checkout sends no source
file payload. Staged, unstaged, and untracked changes, removals, renames,
executable bits, and symlink identities remain governed by the complete normal
sync and deletion manifests; excluded tracked files are pruned after the reset.
The selected remote branch may contain newer commits than the chosen checkout.
Overlay fetches complete filtered commit/tree ancestry for that branch and the
configured base ref, so `HEAD^`, `git merge-base`, and
`git diff origin/main...HEAD` continue to work without downloading unrelated
historical blobs. Origins that cannot support filtered history use ordinary
sync instead.

The optimization is off by default and requires `sync.gitSeed: true`,
`sync.delete: true`, an unrestricted, complete, conflict-free Git checkout
without submodules, and an anonymous HTTP(S) or remotely readable filesystem
origin. Actions-owned workspaces, full resyncs, fresh PR checkouts, delegated
providers, Windows/WSL2, macOS, `sync.include`, embedded credentials, SSH
origins, private origins, unsafe Git configuration, and unavailable runner
prerequisites fall back to the complete ordinary file manifest. Git commands
never receive forwarded credentials, credential helpers, hooks, global Git
configuration, external transports, or repository-defined filters.
Anonymous HTTP authentication failures safely fall back; genuine DNS, TLS,
firewall, and other eligible-origin transport failures remain fatal.

Only dependency caches ignored by verified `.gitignore` files from the exact
target tree may survive overlay preparation: `node_modules`, `.pnpm-store`,
`.yarn/cache`, and `.yarn/unplugged`. Local `.git/info/exclude` cannot grant
cache preservation. Existing workspace ownership and ready-pool preparation
remain unchanged. A real, contained `.crabbox` directory and its reserved
`env`, `scripts`, `logs`, `captures`, and `runs` runtime state survive overlay
cleanup; symlinked runtime or Git metadata roots are rejected.

When overlay mode is requested, timing JSON may additionally report `syncMode`,
`syncTransferFiles`, `syncTransferBytes`, and `syncFallbackReason`; ordinary
default-off timing output retains its existing shape.

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
the largest files and directories, using the same excludes as `run`. When an
ambiguous built-in artifact rule would otherwise hide a tracked regular file,
the plan also prints a bounded annotation naming the protected paths and
patterns. Use `--limit` to change how many top files and directories are listed
(default 20).

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
  gitOverlay: false
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
CRABBOX_SYNC_GIT_OVERLAY
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
