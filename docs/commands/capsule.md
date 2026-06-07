# capsule

`crabbox capsule` captures, replays, and tracks lightweight failure capsules.

A capsule is a portable, replayable failure bundle. Today the capture path is
Actions-first and local-first: `crabbox capsule from-actions` writes a
`capsule.yaml` manifest plus bounded local evidence next to it, and the other
subcommands replay, inspect, and annotate that manifest. Replay reuses
`crabbox run`, so a capsule runs on any lease or environment Crabbox can drive.

A capsule is not a VM snapshot. Use [`crabbox checkpoint`](checkpoint.md) or
`crabbox image` to save environment state; use `crabbox capsule` for the failure
source, replay command, oracle, and replay history. See the
[Capsules feature guide](../features/capsules.md) for the manifest schema and
broader concept.

Subcommands:

- [`from-actions`](#from-actions) — capture a capsule from a GitHub Actions run.
- [`replay`](#replay) — re-run the captured command and record the outcome.
- [`inspect`](#inspect) — print the source, replay command, oracle, and history.
- [`promote`](#promote) — mark the capsule as a regression replay.

A manifest argument may be either the `capsule.yaml` file or the directory that
contains it; the directory form resolves to `capsule.yaml` inside it.

## from-actions

Capture a failing GitHub Actions run into a capsule.

```sh
crabbox capsule from-actions <run-url> --replay '<command>'
crabbox capsule from-actions 123456 --repo example-org/my-app --replay 'go test ./...'
```

The argument is a GitHub Actions run URL
(`https://github.com/owner/repo/actions/runs/123`, optionally with
`/attempts/N`) or a numeric run id. A bare run id requires `--repo`. `--replay`
is required: the command is recorded verbatim and is not inferred from workflow
YAML.

Flags:

```text
--repo <owner/name>             repository; required when the argument is only a run id
--replay <command>              explicit replay command (required)
--output <dir>                  output directory (default capsules/<owner>-<repo>-actions-<run-id>)
--scenario <text>               human-readable scenario (default derived from the run)
--job <name>                    prefer a specific failed job when a run has several
--required-quality <quality>    required replay quality (default semantically_identical)
--max-log-bytes <n>             cap retained failed-log bytes (default 262144)
--no-logs                       skip fetching failed Actions logs
```

The command uses your authenticated `gh` CLI to read the run. It records run
metadata, the selected failed job/step, a bounded tail of the failed log (when
available), non-expired GitHub artifact references, and the explicit replay
command. The selected job must have a failure-class conclusion
(`failure`, `timed_out`, `cancelled`, or `action_required`); otherwise the
command exits with an error.

When a failed log is captured, a `failure_signature` is derived from it and
stored in the manifest's oracle. The signature turns replay into a stricter
check (see [Replay](#replay)). Use `--no-logs` to skip log capture and record
only the nonzero-exit contract.

The output directory defaults to `capsules/<owner>-<repo>-actions-<run-id>`,
suffixed with `-attempt-N` for re-runs and with the failing job/step when the
run has multiple failures or `--job` is set. It contains `capsule.yaml` and a
`logs/` subdirectory.

## replay

Re-run a capsule's command and record the outcome.

```sh
crabbox capsule replay <capsule.yaml> [--keep]
```

Flags:

```text
--id <lease-id-or-slug>   replay on an existing lease instead of a fresh one
--keep                    keep the lease after replay for SSH debugging
--junit <paths>           comma-separated remote JUnit XML paths to collect via crabbox run
--no-sync                 skip rsync of the local checkout
--reclaim                 claim the lease for the current repo
```

Replay runs `replay.command` through `crabbox run --shell`, passing the flags
above through unchanged. The exit handling encodes the oracle:

- **Zero exit** records `pass` and exits nonzero — the captured failure did not
  reproduce, which is treated as a failed replay so the regression is visible.
- **Nonzero exit, no `failure_signature`** records `fail_reproduced` and exits
  zero.
- **Nonzero exit with a `failure_signature`** records `fail_reproduced` only if
  the signature appears in the last 256 KiB of replay output; otherwise it
  records `fail_new` and exits nonzero so the mismatch surfaces.
- **A run/setup error** (not a remote command exit) records
  `inconclusive_env_error` and returns the underlying error.

Every replay appends a record (outcome, exit code, duration, kept-lease flag,
note) to the manifest's `replays` history.

Replay can target an environment prepared by other Crabbox features:

```sh
# Replay on a lease hydrated the same way CI runs.
crabbox actions hydrate --id blue-lobster
crabbox capsule replay capsules/example-org-my-app-actions-123/capsule.yaml --id blue-lobster --keep

# Replay on a box forked from a checkpoint.
crabbox checkpoint fork chk_123 --class beast
crabbox capsule replay capsules/example-org-my-app-actions-123/capsule.yaml --id purple-whale
```

## inspect

Print a summary of a capsule manifest.

```sh
crabbox capsule inspect <capsule.yaml>
crabbox capsule inspect <capsule.yaml> --json
```

`inspect` prints the capsule id, class, scenario, source run, replay command and
required quality, oracle type and failure signature, the most recent replay (if
any), and promotion state. `--json` emits the full manifest.

## promote

Mark a capsule as a regression replay.

```sh
crabbox capsule promote <capsule.yaml> --regression
```

Flags:

```text
--regression   promote this capsule as a regression replay (currently required)
--note <text>  optional promotion note
```

Promotion records `promotion.regression=true` with a timestamp (and the optional
note) in the local manifest. `--regression` is required today; there is no
remote registry in this first slice.

## Related docs

- [Capsules](../features/capsules.md)
- [Actions hydration](../features/actions-hydration.md)
- [checkpoint](checkpoint.md)
- [run](run.md)
