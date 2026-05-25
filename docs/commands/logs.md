# logs

`crabbox logs` prints the retained command output for a recorded run.

```sh
crabbox logs run_abcdef123456
crabbox logs --id run_abcdef123456
crabbox logs run_abcdef123456 --tail 80
crabbox logs run_abcdef123456 --json
```

## What Gets Stored

When `crabbox run` runs against a coordinator, it streams remote stdout and
stderr to the local terminal *and* records a bounded copy on the
coordinator. The CLI keeps up to 8 MiB of capture per run; the coordinator
stores larger captures in chunks so a noisy parallel run does not exceed
Durable Object storage limits.

Output beyond the cap is truncated with an `output.truncated` marker on the
last event so the consumer knows the tail is missing.

Runs started with `--capture-stdout <path>` intentionally omit stdout from
retained logs and output events. Stderr is still streamed and retained. Use this
mode for binary stdout, Sixel frames, archives, or any output that should not be
stored as text in the coordinator. Files copied with `run --download
remote=local` are local artifacts and are not stored in coordinator logs.

## Output

The plain form writes the log text to stdout. Add `--tail N` to print only the
last N retained log lines. Logs are stored as combined command output; stdout
and stderr are not separately indexed in the retained log, so stream filtering
belongs on `events`.

`--json` returns run metadata plus the log:

```json
{
  "runId": "run_abcdef123456",
  "leaseId": "cbx_abcdef123456",
  "exitCode": 0,
  "truncated": false,
  "log": "..."
}
```

`--json` is stable enough for scripts that filter by exit code and want the
log text in one payload.

## Flags

```text
--id <run-id>       run id (also accepted as a positional argument)
--tail <n>          print only the last N log lines
--json              print JSON with metadata and log text
```

## When To Use Logs vs Events vs Attach

- `logs` returns the retained command output. Use when you want the full
  bounded transcript after the run finished.
- `events` returns ordered run events (lease, sync, command, output chunks,
  finish). Use when you need to know *what happened* and *when*.
- `attach` follows live events. Use when the run is still active and you
  want to watch it without re-attaching the original CLI.

Logs and events are independent surfaces - logs stay focused on command
output, events stay focused on lifecycle.

## Direct Mode

Direct-provider mode does not record runs centrally, so `crabbox logs` has
nothing to fetch. Use shell output or the local terminal log instead.

Related docs:

- [history](history.md)
- [events](events.md)
- [attach](attach.md)
- [results](results.md)
- [History and logs](../features/history-logs.md)
