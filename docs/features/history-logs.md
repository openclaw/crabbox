# History And Logs

Read when:

- changing how `crabbox run` records progress;
- debugging a failed remote command after the fact;
- deciding what belongs in coordinator-stored run history.

History and logs are a **brokered-mode** feature. When `crabbox run` executes
against a brokered provider (`aws`, `azure`, `gcp`, `hetzner` with a coordinator
configured), the CLI mirrors the run into coordinator storage as a durable,
queryable record. Direct-provider runs and delegated runs do not
produce central history — there you have only the live terminal output and any
local captures you ask for.

## What a recorded run contains

The CLI creates a run handle (`run_<hex>`) before leasing starts, then appends
ordered events as it advances:

- `run.started`
- `leasing.started`, `lease.created`, `lease.replace.started|finished|failed`
- `bootstrap.waiting`
- `sync.started`, `sync.finished`
- `actions.hydrate.started|finished|failed` (when hydrating from a GitHub
  Actions workflow)
- `script.uploaded` (when running a `--script` file)
- `command.started`, streamed `stdout`/`stderr` events, `command.finished`
- `lease.released`
- `run.failed` (if the run errors before the command finishes)

Each event carries a sequence number, type, phase, and stream. Streamed output
events are capped at **64 KiB total per run**; once the cap is hit the CLI emits
a single `output.truncated` marker pointing you at `crabbox logs` for the full
retained output.

When the command exits, the CLI finishes the run with:

- exit code;
- sync duration, command duration, and total duration;
- owner and org;
- provider, target, class, and server type;
- the retained command log (capped — see below);
- a parsed test-result summary when JUnit results are available;
- a failure classification (`blockedStage`, `retryLikely`) on non-zero exits;
- optional Linux telemetry: a start sample, bounded mid-run samples (every 15s,
  up to 60 retained), and an end sample covering load, memory, disk, and uptime.

## Reading history

```sh
crabbox history                      # recent runs
crabbox history --lease cbx_abcdef01 # runs for one lease
crabbox history --state failed       # filter by state
crabbox logs run_a1b2c3d4            # full retained command log
crabbox logs run_a1b2c3d4 --tail 80  # last 80 lines only
crabbox events run_a1b2c3d4          # ordered run events
crabbox events run_a1b2c3d4 --type stderr  # filter events by type
crabbox attach run_a1b2c3d4          # follow an in-progress run live
```

`crabbox attach` follows a still-running run, preferring the broker's live
control WebSocket and falling back to polling. Use `attach` for active runs and
`logs` for the retained output of a finished run. All four commands accept
`--json`.

## Storage limits

History records, run events, and run logs all live in coordinator storage:
Durable Object storage on Cloudflare or PostgreSQL on Node. Log text is stored
separately from run metadata and is intentionally bounded so noisy commands
cannot exhaust storage:

- The CLI keeps the **last 8 MiB** of command output and reports
  `logTruncated` when more was produced.
- The broker stores the same **8 MiB** cap, chunked at **64 KiB** per storage
  value and reassembled by `crabbox logs`.

These caps are independent of the per-run **64 KiB** streamed-event budget
described above; events are for live tailing, `logs` is for the full retained
output.

## Local debug artifacts

For uncapped, local-only output, mirror the streams to files:

```sh
crabbox run --capture-stdout out.log --capture-stderr err.log -- ./test.sh
```

These captures are written on the operator's machine and bypass coordinator
run-log storage entirely. Use distinct paths for stdout, stderr, and any
`--download remote=local` artifacts — Crabbox rejects path collisions before the
command runs.

On a non-zero exit, SSH-backed and Blacksmith delegated runs also write a local
failure bundle under `.crabbox/captures/` by default (the bundled streams are
capped at 16 MiB each). `--capture-on-fail` is accepted as a no-op compatibility
alias; bundles save automatically on failure. Treat captured logs and bundles as
secret-bearing files unless you redact them before sharing.

## Phase timings

A command can annotate its own timeline by printing `CRABBOX_PHASE:<name>` on
stdout or stderr. Phase markers surface in `crabbox run --timing-json` under
`commandPhases`, and the failure digest reports the observed and final phases.
The marker line stays in the normal output stream, so scripts and humans see the
same text.

## Portal view

In the authenticated browser portal, `/portal/runs/<run-id>` renders the same
run as a human page: command metadata, result summary, searchable and paginated
recent events, compact resource deltas, short telemetry trend lines, and a
copyable retained log tail. `/portal/runs/<run-id>/logs` stays a plain-text log
endpoint and `/portal/runs/<run-id>/events` stays JSON, both for easy copying or
browser-side inspection.

## Related docs

- [history command](../commands/history.md)
- [logs command](../commands/logs.md)
- [events command](../commands/events.md)
- [attach command](../commands/attach.md)
- [results command](../commands/results.md)
- [Observability](../observability.md)
