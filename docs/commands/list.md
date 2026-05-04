# list

`crabbox list` shows current Crabbox machines.

```sh
crabbox list
crabbox list --provider aws
crabbox list --provider ssh --target macos --static-host mac-studio.local
crabbox list --provider blacksmith-testbox
crabbox list --json
```

`crabbox pool list` remains as a compatibility alias.

In `provider=ssh` mode this prints the configured static target.

In `blacksmith-testbox` mode this forwards to `blacksmith testbox list`. Human output preserves the Blacksmith table; `--json` emits Crabbox-parsed rows with id, status, repo, workflow, job, ref, and created time when the upstream table exposes those columns.

Flags:

```text
--provider hetzner|aws|ssh|blacksmith-testbox
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--json
```
