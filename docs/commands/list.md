# list

`crabbox list` shows current Crabbox machines.

```sh
crabbox list
crabbox list --provider aws
crabbox list --provider ssh --target macos --static-host mac-studio.local
crabbox list --provider blacksmith-testbox
crabbox list --provider namespace-devbox
crabbox list --provider semaphore
crabbox list --provider sprites
crabbox list --provider daytona
crabbox list --provider islo
crabbox list --provider e2b
crabbox list --json
```

`crabbox pool list` remains as a compatibility alias.

In `provider=ssh` mode this prints the configured static target.

In `blacksmith-testbox` mode this reads `blacksmith testbox list` and renders the
same Crabbox list shape as other providers. `--json` keeps the compatibility
shape parsed from the Blacksmith table: id, status, repo, workflow, job, ref,
and created time when the upstream table exposes those columns.
When coordinator auth is configured, the same list command also refreshes
owner-scoped external runner rows in the portal lease table from the current
all-status Blacksmith list. Crabbox also attempts to infer the matching GitHub
Actions run/workflow from the row's repo, workflow, ref, and created time.
The portal shows that Actions status, tags long-queued or long-running workflow
owners as `stuck`, exposes a copyable local stop command, and links each row to
a visibility-only runner detail page. Missing runners from later syncs are
marked stale rather than treated as Crabbox leases.

In `namespace-devbox`, `semaphore`, `sprites`, `daytona`, `islo`, and `e2b` modes,
rendering is core-owned: human output and `--json` use the normalized Crabbox
lease view.

Flags:

```text
--provider hetzner|aws|azure|gcp|proxmox|ssh|blacksmith-testbox|namespace-devbox|semaphore|sprites|daytona|islo|e2b
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--json
--sprites-api-url <url>
--e2b-api-url <url>
--e2b-domain <domain>
```
