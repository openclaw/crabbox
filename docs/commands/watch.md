# watch

`crabbox watch` runs a command on a warm lease, then watches the local
repository and re-runs the command whenever synced files change. Each iteration
goes through the normal [`crabbox run`](run.md) pipeline against the same held
lease, so sync fingerprints, run history, results, artifacts, and
[`attach`](attach.md) behave exactly as they do for `run`. It automates the
manual warm-lease loop described in the [performance guide](../performance.md).

```sh
crabbox watch -- pnpm test:changed
crabbox watch --id swift-crab -- go test ./...
crabbox watch --debounce 500ms --idle-exit 10m -- pnpm check
crabbox watch --provider hetzner --keep -- make test
```

Requires an SSH lease provider that advertises the `crabbox-sync` feature (see
[`crabbox providers`](providers.md)). Delegated and archive-sync providers are
rejected with an error; use `crabbox run` for those.

## Lease lifecycle

- With `--id`, watch reuses an existing claimed lease and never releases it;
  exiting the loop leaves the box running, like `crabbox run --id`.
- Without `--id`, watch acquires a fresh lease (all `warmup` lease-creation
  flags apply) and releases it on every exit path: idle exit, Ctrl-C, and
  failures. Pass `--keep` to retain the lease instead.

## Change detection

The initial run starts immediately. After that, filesystem events are debounced
(default `250ms`) and each batch is qualified against the exact sync universe:
Git tracked and non-ignored untracked files, minus the built-in excludes,
`sync.exclude` config, and `.crabboxignore` rules. Ignored churn such as
`node_modules` or build output never triggers a run and never counts as
activity. Edits to `.gitignore`, `.crabboxignore`, or repo-local config are
picked up live. Newly created directories are watched recursively; symlinked
directories outside the repository root are not followed.

Runs never overlap. A change that lands while a run is active queues exactly
one rerun, which starts from the newest tree once the active run finishes. A
non-zero remote exit is a normal iteration result and the loop keeps watching;
watcher, lease, and sync failures end the loop. One Ctrl-C exits and applies
the lease lifecycle rules above.

The loop exits on its own after `--idle-exit` without qualifying local changes.
It defaults to the effective lease idle timeout and must not exceed it; raise
`--idle-timeout` as well for longer sessions.

## Flags

```text
--id <id-or-slug>      Reuse an existing claimed lease instead of acquiring one.
--keep                 Keep an acquired lease when the watch loop exits.
--debounce <duration>  Quiet period before a change batch triggers a rerun (default 250ms).
--idle-exit <duration> Exit after this period without qualifying changes; defaults to
                       the lease idle timeout and must not exceed it.
```

Lease-creation flags (`--provider`, `--class`, `--ttl`, `--idle-timeout`,
`--slug`, ...) apply when acquiring a fresh lease. Most other `run` flags, such
as `--shell`, `--junit`, `--allow-env`, or `--preset`, pass through to every
iteration. Flags that conflict with a persistent session are rejected:
`--pool`, `--pool-return`, `--sync-only`, `--no-sync`, `--apply-local-patch`,
`--fresh-pr`, `--script`, `--script-stdin`, `--stop-after`, `--lease-output`,
`--keep-on-failure`, `--capture-stdout`, `--capture-stderr`, `--download`,
`--emit-proof`, and `--proof-template`. `--label` is reserved: watch labels
each iteration `watch #N` so runs stay readable in [`history`](history.md).

## Output

Lease and per-iteration run output match `crabbox run`. The loop itself prints
progress to stderr:

```text
watch root=/home/alice/my-app debounce=250ms idle_exit=30m0s
watch run=2 changes=3
watch idle_exit=30m0s runs=2
```

## Related docs

- [run](run.md) — the pipeline each iteration executes.
- [warmup](warmup.md) — lease a reusable box explicitly.
- [history](history.md) — recorded runs, one per iteration.
- [Performance guide](../performance.md) — warm leases and sync behavior.
