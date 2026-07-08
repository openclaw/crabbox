# sync-plan

`crabbox sync-plan` prints the local sync manifest and its size hotspots
without leasing a box. Use it to preview what `crabbox run` would upload
before paying for a cold sync, or to confirm that artifacts dropped out of
the manifest after editing `.crabboxignore`.

```sh
crabbox sync-plan
crabbox sync-plan --limit 10
crabbox sync-plan --json
```

The command reads only your local Git checkout. It does not require a
lease, does not call the broker, and does not call any provider API.

## What it reads

`sync-plan` builds the same manifest `crabbox run` uses, so the file set
matches what an actual sync would ship:

- files reported by `git ls-files --cached --others --exclude-standard`
  (tracked files plus non-ignored untracked files);
- root `.crabboxignore` patterns;
- `sync.exclude` patterns from config;
- Crabbox's built-in cache/build excludes.

Ordered exclude rules are applied before size accounting; a later `!pattern`
can re-include a path matched by an earlier rule.

## Output

The first line reports the candidate file count and total size. If the
checkout has tracked files that were deleted locally (and would be pruned
on the remote), a `deleted tracked paths` line follows. Then `sync-plan`
prints the largest files and the largest top-level or second-level
directories.

```text
sync candidate: 1843 files, 312.5 MiB
deleted tracked paths: 2
top files:
  84.5 MiB   assets/demo.mp4
  12.4 MiB   fixtures/sample-data.json
  ...
top dirs:
  140.2 MiB  assets
  80.1 MiB   fixtures
  ...
```

Directories are grouped at one level deep for top-level paths and two
levels deep for nested paths (for example `internal/cli`), so deeply
nested hotspots still roll up to a meaningful prefix.

With `--json`, the command emits the same information in a stable
machine-readable shape for CI checks and agent preflights:

```json
{
  "candidate": { "files": 1843, "bytes": 327680000, "humanBytes": "312.5 MiB" },
  "dirtyDelta": { "files": 12, "bytes": 524288, "humanBytes": "512.0 KiB" },
  "deletedTrackedPaths": 2,
  "guardrail": {
    "scope": "dirty_delta",
    "files": 12,
    "bytes": 524288,
    "humanBytes": "512.0 KiB",
    "limits": { "warnFiles": 0, "warnBytes": 0, "failFiles": 0, "failBytes": 0 },
    "allowLarge": false,
    "status": "ok"
  },
  "topFiles": [{ "path": "assets/demo.mp4", "bytes": 88604672, "humanBytes": "84.5 MiB" }],
  "topDirs": [{ "path": "assets", "bytes": 147010355, "humanBytes": "140.2 MiB" }]
}
```

`candidate` is the full manifest that would be present on the remote after
sync. `dirtyDelta` is the locally changed/untracked/deleted path set that
`crabbox run` uses for large-sync guardrails when it is non-empty.
`guardrail.scope` is therefore either `dirty_delta` or `candidate`, matching
the sync preflight path. `guardrail.status` is `ok`, `warning`, or `failed`;
warnings and failures are listed in `guardrail.reasons` when configured
`sync.warn*` or `sync.fail*` thresholds are reached.

## Flags

```text
--limit <n>   number of top files and directories to print (default 20)
--json        print machine-readable JSON
```

`--limit` must be positive; `--limit 0` (or any non-positive value) is
rejected with an error.

## Use cases

- preview a first sync before warming a lease;
- find directories that quietly grew (`.cache/`, `dist/`, generated
  assets);
- audit `.crabboxignore` and `sync.exclude` after adding new patterns.
- gate CI or an agent workflow on sync size before provisioning a remote box.

The numbers `sync-plan` prints are upper bounds. The actual rsync transfer
depends on what already exists on the remote runner: a repeat sync after a
warmup is much smaller because the manifest matches the remote fingerprint
and rsync ships only changed bytes.

## Related docs

- [run](run.md)
- [Sync](../features/sync.md)
- [Configuration](../features/configuration.md)
