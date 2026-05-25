# events

`crabbox events` prints the coordinator event log for a recorded run.

```sh
crabbox events run_abcdef123456
crabbox events --id run_abcdef123456 --after 42 --limit 100
crabbox events run_abcdef123456 --type stderr
crabbox events run_abcdef123456 --phase command
crabbox events run_abcdef123456 --json
```

## What Events Are Recorded

Coordinator-backed `crabbox run` creates a durable `run_...` handle before
it leases or syncs. The CLI appends ordered events as the run advances:

- `lease.acquire.start`, `lease.acquire.success`, `lease.acquire.fail`;
- `bootstrap.wait`, `bootstrap.ready`;
- `sync.start`, `sync.skip`, `sync.success`, `sync.fail`;
- `command.start`, `command.finish`;
- `output.stdout`, `output.stderr`, `output.truncated`;
- `release.start`, `release.success`, `release.fail`.

Each event carries a sequence number, event type, phase, optional stream
(stdout/stderr), timestamp, and short message or output text.

## Output

Human output prints sequence number, event type, phase, stream, timestamp,
and message:

```text
   1  lease.acquire.start    plan       2026-05-07T07:42:18Z
   2  lease.acquire.success  plan       2026-05-07T07:42:21Z   leased=cbx_abcdef123456 slug=blue-lobster
   3  bootstrap.wait         provision  2026-05-07T07:42:21Z
   4  bootstrap.ready        provision  2026-05-07T07:43:05Z
   5  sync.start             sync       2026-05-07T07:43:05Z
   6  sync.success           sync       2026-05-07T07:43:08Z   files=184 bytes=12.4MiB
   7  command.start          run        2026-05-07T07:43:08Z   pnpm test
   8  output.stdout          run        2026-05-07T07:43:09Z   > vitest run
   9  output.stdout          run        2026-05-07T07:43:11Z   ✓ src/foo.test.ts (8)
  ...
  42  command.finish         run        2026-05-07T07:45:32Z   exit=0
  43  release.success        release    2026-05-07T07:45:34Z
```

`--json` returns the raw event records.

## Bounded Output Capture

Output events are a bounded preview. The coordinator caps stdout/stderr
capture at 64 KiB per run and records an `output.truncated` marker when
the cap is reached. The retained log keeps up to 8 MiB. For the larger
retained command output, use [logs](logs.md).

## Flags

```text
--id <run-id>     run id (also accepted as a positional argument)
--after <seq>     only show events after this sequence number
--limit <n>       maximum number of events, default 500, maximum 500
--type <kind>     only show events with this exact type
--phase <name>    only show events with this exact phase
--json            print JSON
```

`--after` is what `attach` uses internally - resume from a known sequence
without replaying the whole event log.

## Use Cases

- post-mortem on a failed run when you need the exact sequence of phases;
- correlating a failed step with the timestamps of surrounding sync or
  bootstrap events;
- scripting a status check that filters by event type;
- archiving event records for runs that exceeded the retained log cap.

Related docs:

- [history](history.md)
- [logs](logs.md)
- [attach](attach.md)
- [results](results.md)
- [History and logs](../features/history-logs.md)
