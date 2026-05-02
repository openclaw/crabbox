# History And Logs

Read when:

- changing run recording;
- debugging failed remote commands;
- deciding what belongs in coordinator history.

Coordinator-backed `crabbox run` records a run before sync starts and appends lifecycle events as the CLI progresses. When the command exits, the CLI finishes that run with:

- exit code;
- sync duration;
- command duration;
- total duration;
- owner and org;
- provider, class, and server type;
- retained remote output tail.

The append-only event stream records:

- `run.created`
- `lease.created` or `lease.reused`
- `sync.started` and `sync.finished`
- `hydrate.started` and `hydrate.finished`
- `command.started`, `stdout`, `stderr`, and `command.finished`
- `lease.released`

Use:

```sh
crabbox history
crabbox history --lease cbx_...
crabbox events run_...
crabbox attach run_...
crabbox logs run_...
```

History and events live in the Fleet Durable Object. Log text is stored separately from run metadata and intentionally capped to the latest tail so noisy commands cannot exhaust storage.

Direct-provider mode does not have central history. Use shell output or local terminal logs there.

Related docs:

- [history command](../commands/history.md)
- [events command](../commands/events.md)
- [attach command](../commands/attach.md)
- [logs command](../commands/logs.md)
- [Observability](../observability.md)
