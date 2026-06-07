# attach

`crabbox attach` follows the recorded events of an active coordinator run and
prints them as they arrive, exiting when the run finishes.

```sh
crabbox attach run_abcdef123456
crabbox attach --id run_abcdef123456 --after 42
crabbox attach run_abcdef123456 --poll 500ms
```

The run id may be passed as a positional argument or via `--id`; both forms are
equivalent. Run ids look like `run_<hex>`.

## How it works

`attach` first tries the authenticated coordinator control WebSocket, which
streams run events live. If the socket cannot be dialed, fails to subscribe, or
errors mid-stream before the run finishes, `attach` falls back to the HTTPS
events API and keeps reading from the last sequence number it printed. Either
way it advances a running cursor so no event is shown twice.

Events are routed by stream:

- `stdout` and `stderr` preview events are written back to stdout and stderr
  respectively, preserving the original stream split;
- all other events (lease, bootstrap, sync, command-start, command-finish,
  release, and similar lifecycle events) are printed to stderr as a single line
  carrying the sequence number, event type, phase, timestamp, and message.

When the WebSocket falls behind the run's current sequence, `attach` drains the
backlog in bounded pages, re-subscribing after each full page, before waiting on
live pushes. When the run is already finished, `attach` prints any remaining
events and exits. When the run is still active, it waits for streamed events or
polls until it observes the run leave the `running` state.

`attach` is a viewer, not detached execution. It follows the events the original
CLI emitted while running the command; it does not own or control that command.
If the original CLI process dies, the run remains inspectable through
[history](history.md), [events](events.md), and [logs](logs.md), but `attach`
cannot resume or restart it.

## Bounded output

Output preview events are a bounded view of the command output. The coordinator
stores run logs in 64 KiB chunks up to an 8 MiB cap and marks the run truncated
when that cap is exceeded. For the full retained output after a run completes,
use [logs](logs.md).

## Flags

```text
--id <run-id>       run id (also accepted as the first positional argument)
--after <seq>       resume after this event sequence number (default 0)
--poll <duration>   fallback poll interval and WebSocket idle check (default 1s)
```

`--after` must be `>= 0` and `--poll` must be positive.

## Use cases

- Watch a long warmup or run from a second terminal without disturbing the
  original CLI.
- Monitor an agent-launched run while doing other work locally.
- Reconnect after a network blip and resume from a known sequence with
  `--after`.

## Direct mode

Direct-provider runs (no configured coordinator) are not recorded centrally, so
there is no event stream to follow and `attach` has nothing to attach to. Watch
the shell output of the original CLI instead.

## Related docs

- [logs](logs.md)
- [events](events.md)
- [history](history.md)
- [results](results.md)
- [run](run.md)
- [History and logs](../features/history-logs.md)
