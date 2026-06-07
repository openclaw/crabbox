# logs

`crabbox logs` prints the retained command output for a recorded run.

```sh
crabbox logs run_abcdef123456
crabbox logs --id run_abcdef123456
crabbox logs run_abcdef123456 --tail 80
crabbox logs run_abcdef123456 --json
```

Run IDs have the form `run_<hex>`. Get them from [`history`](history.md), or
from the `run.started` event in [`events`](events.md).

## What gets stored

When `crabbox run` executes against a coordinator, it streams the remote
process stdout and stderr to your terminal *and* records a bounded copy on the
coordinator. The retained log is the combined command output, capped at 8 MiB
per run; the coordinator stores it in 64 KiB chunks so a noisy parallel run
does not blow past Durable Object storage limits.

Output beyond the cap is dropped and the run is flagged as truncated, so a
consumer can tell the tail is missing.

Runs started with `run --capture-stdout <path>` intentionally omit stdout from
the retained log and from output events; stderr is still streamed and retained.
Use that mode for binary stdout, Sixel frames, archives, or anything that
should not be stored as text on the coordinator. Files pulled with `run
--download remote=local` are local artifacts and never appear in coordinator
logs.

## Output

The default form writes the log text to stdout, unmodified. Add `--tail N` to
print only the last N lines of the retained log.

Logs are stored as combined output; stdout and stderr are not separately
indexed in the retained log, so per-stream filtering belongs on
[`events`](events.md) instead.

`--json` prints the full run record alongside the log:

```json
{
  "run": {
    "id": "run_abcdef123456",
    "leaseID": "cbx_abcdef123456",
    "state": "succeeded",
    "exitCode": 0,
    "logBytes": 4096,
    "logTruncated": false
  },
  "log": "..."
}
```

The `run` object carries the same fields you get from [`history`](history.md)
(provider, target, timings, `exitCode`, `logBytes`, `logTruncated`, parsed
`results`, and more); the `log` field holds the retained text after any
`--tail` trim. Scripts that want exit code plus log text in one payload should
read `run.exitCode` and `log`.

## Flags

```text
--id <run-id>   run id (also accepted as a positional argument)
--tail <n>      print only the last N log lines (must be >= 0)
--json          print JSON: the run record plus the log text
```

## When to use logs vs events vs attach

- [`logs`](logs.md) returns the retained command output. Use it when you want
  the full bounded transcript after a run finished.
- [`events`](events.md) returns ordered run events (lease, sync, command,
  output chunks, finish). Use it when you need to know *what happened* and
  *when*.
- [`attach`](attach.md) follows live events. Use it while a run is still active
  and you want to watch it without re-running the original CLI.

Logs and events are independent surfaces: logs stay focused on command output,
events stay focused on lifecycle.

## Direct mode

Direct-provider runs (no coordinator configured) are not recorded centrally, so
`crabbox logs` has nothing to fetch and reports that no coordinator is
configured. Use the local terminal output instead.

Related docs:

- [history](history.md)
- [events](events.md)
- [attach](attach.md)
- [results](results.md)
- [History and logs](../features/history-logs.md)
