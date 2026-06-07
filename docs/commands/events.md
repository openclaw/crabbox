# events

`crabbox events` prints the broker's event log for a recorded run.

```sh
crabbox events run_abcdef123456
crabbox events --id run_abcdef123456 --after 42 --limit 100
crabbox events run_abcdef123456 --type stdout
crabbox events run_abcdef123456 --phase sync
crabbox events run_abcdef123456 --json
```

The run id (`run_<hex>`) is also accepted as a positional argument; `--id`
and the positional form are equivalent. Reading events requires a configured
broker, since the event log lives in the coordinator, not on the box.

## What events are recorded

When `crabbox run` executes against a broker, a `runRecorder` creates a
durable `run_...` record before it leases or syncs, then appends ordered
events as the run advances. Each event has a monotonic sequence number, a
type, a phase, an optional stream (`stdout`/`stderr`), a timestamp, and a
short message or output text. Common event types include:

- `run.started`
- `leasing.started`, `lease.created`, `lease.released`
- `bootstrap.waiting`
- `actions.hydrate.started`, `actions.hydrate.finished`, `actions.hydrate.failed`
- `sync.started`, `sync.finished`
- `command.started`
- `stdout`, `stderr`, `output.truncated`
- `lease.replace.started`, `lease.replace.finished`, `lease.replace.failed`
- `run.failed`

The exact set depends on the run: an Actions-hydrated run emits the
`actions.hydrate.*` events, a run that swaps a dead box mid-flight emits the
`lease.replace.*` events, and so on.

## Output

Human-readable output prints the sequence number, type, phase, stream, and
timestamp followed by the message (falling back to the event's data text):

```text
0001 run.started        phase=starting   stream=- at=2026-05-07T07:42:18Z
0002 leasing.started    phase=leasing    stream=- at=2026-05-07T07:42:18Z
0003 lease.created      phase=leasing    stream=- at=2026-05-07T07:42:21Z leased=cbx_abcdef123456 slug=swift-crab
0004 bootstrap.waiting  phase=bootstrap  stream=- at=2026-05-07T07:42:21Z
0005 sync.started       phase=sync       stream=- at=2026-05-07T07:43:05Z
0006 sync.finished      phase=sync       stream=- at=2026-05-07T07:43:08Z files=184 bytes=12.4MiB
0007 command.started    phase=command    stream=- at=2026-05-07T07:43:08Z pnpm test
0008 stdout             phase=command    stream=stdout at=2026-05-07T07:43:09Z > vitest run
...
0043 lease.released     phase=release    stream=- at=2026-05-07T07:45:34Z
```

`--json` emits the raw event records (sequence, type, phase, stream,
timestamps, plus lease/provider/exit-code fields where present).

## Bounded output capture

Output events are a bounded preview, not the full command log. The CLI caps
captured `stdout`/`stderr` event bytes at **64 KiB** per run (queued in 16 KiB
chunks) and emits a single `output.truncated` event once the cap is reached.
For the larger retained command output, use [logs](logs.md): the broker stores
up to **8 MiB** of run log in 64 KiB chunks.

## Flags

```text
--id <run-id>     run id (also accepted as a positional argument)
--after <seq>     only show events after this sequence number (default 0)
--limit <n>       maximum number of events to return (default 500)
--type <kind>     only show events whose type matches exactly
--phase <name>    only show events whose phase matches exactly
--json            print JSON
```

The broker clamps each request to at most 500 events. When `--type` or
`--phase` is set the CLI pages through the log (500 at a time) and applies the
filter locally, returning up to `--limit` matching events.

`--after` is what [attach](attach.md) uses to resume from a known sequence
without replaying the whole event log.

## Use cases

- Post-mortem on a failed run when you need the exact sequence of phases.
- Correlating a failed step with the timestamps of surrounding sync,
  bootstrap, or lease-replace events.
- Scripting a status check that filters by event type or phase.
- Archiving event records for runs whose output exceeded the retained log cap.

## Related docs

- [history](history.md)
- [logs](logs.md)
- [attach](attach.md)
- [results](results.md)
- [History and logs](../features/history-logs.md)
