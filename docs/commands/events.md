# events

`crabbox events` lists structured lifecycle and output events for a recorded run.

```sh
crabbox events run_...
crabbox events --id run_... --after 42
crabbox events run_... --json
```

Events are recorded by the coordinator while the CLI is alive. They include run creation, lease use, sync and hydration phases, command start/finish, stdout/stderr chunks, and release attempts.

Flags:

```text
--id <run-id>       run id
--after <seq>       only show events after this sequence
--limit <n>         default 500, maximum 500
--json              print JSON
```

Use [attach](attach.md) to follow a still-running run. Use [logs](logs.md) for the retained output tail.

