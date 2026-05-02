# history

`crabbox history` lists coordinator-recorded remote command runs.

```sh
crabbox history
crabbox history --lease cbx_...
crabbox history --owner steipete@gmail.com
crabbox history --org openclaw --json
```

Flags:

```text
--lease <lease-id>      filter by lease
--owner <email>         filter by owner
--org <name>            filter by org
--state <state>         running, succeeded, or failed
--limit <n>             default 50, maximum 500
--json                  print JSON
```

Human output includes run ID, lease ID, state, exit code, duration, start time, and command. Use the run ID with [logs](logs.md).
When event recording is available, human output also includes the last known phase, such as `sync`, `hydrate`, `command`, or `release`.

Related docs:

- [events](events.md)
- [attach](attach.md)
- [logs](logs.md)
- [History and logs](../features/history-logs.md)
