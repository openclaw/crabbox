# attach

`crabbox attach` follows structured events for an active recorded run.

```sh
crabbox attach run_...
crabbox attach --id run_... --after 42
```

Stdout and stderr events are written back to stdout and stderr. Lifecycle events are printed to stderr with their sequence number, timestamp, and event type. When the run has already finished, `attach` prints any remaining events and exits.

Flags:

```text
--id <run-id>       run id
--after <seq>       resume after this event sequence
--poll <duration>   polling interval, default 1s
```

`attach` follows broker events emitted by the original CLI. It is not detached command execution; if the original CLI process dies, the last recorded phase remains inspectable through [history](history.md) and [events](events.md).

