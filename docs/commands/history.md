# history

`crabbox history` lists recorded remote command runs from the broker. Each run is
created by [`crabbox run`](run.md) (and run-backed flows such as
[`crabbox job run`](job.md) or `capsule replay`) and persists its state, phase,
exit code, duration, and telemetry on the coordinator. This command requires a
configured broker; without one it returns an error.

```sh
crabbox history
crabbox history --lease cbx_0a1b2c3d4e5f
crabbox history --owner alice@example.com
crabbox history --org example-org --json
crabbox history --state failed --limit 100
```

## Flags

```text
--lease <lease-id>   Filter to runs on a single lease.
--owner <email>      Filter to runs owned by an email.
--org <name>         Filter to runs scoped to an org.
--state <state>      Filter by state: running, succeeded, or failed.
--limit <n>          Maximum runs to return (default 50, broker-capped at 500).
--json               Print the raw run records as JSON.
```

## Output

The default output prints one line per run with the run ID, lease ID, lease slug,
state, phase, exit code, duration, start time, an optional resource summary, and
the executed command:

```text
run_a1b2c3d4 cbx_0a1b2c3d4e5f swift-crab succeeded phase=command.finished exit=0 duration=42.1s started=2026-05-29T10:15:04Z resources=load=0.42 mem=38% disk=12% command=npm test
```

Missing values render as `-` (for example a still-running run has no exit code).
The `resources=…` segment appears only when a coordinator-backed Linux run
captured [telemetry](../features/history-logs.md). `--json` emits the full run
records, including the start/end telemetry snapshots, JUnit result summaries, and
classification fields (`blockedStage`, `retryLikely`) when present.

Use a run ID from this list with [`logs`](logs.md), [`events`](events.md), or
[`attach`](attach.md) to inspect a specific run.

## Related docs

- [logs](logs.md) — print captured run logs.
- [events](events.md) — print phase and stream events.
- [attach](attach.md) — follow a live run's events.
- [results](results.md) — parsed test-result summaries.
- [History and logs](../features/history-logs.md) — concept overview.
