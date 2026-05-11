# stop

`crabbox stop` releases a coordinator lease or deletes a direct-provider machine.

```sh
crabbox stop blue-lobster
crabbox stop --provider namespace-devbox blue-lobster
crabbox stop --provider semaphore blue-lobster
crabbox stop --provider sprites blue-lobster
crabbox stop --provider daytona blue-lobster
crabbox stop --provider islo blue-lobster
crabbox stop --provider e2b blue-lobster
crabbox stop --provider ssh --static-host mac-studio.local mac-studio.local
```

`crabbox release` remains as a compatibility alias.
The argument accepts the stable `cbx_...` ID or an active friendly slug. In
`blacksmith-testbox` mode it accepts a `tbx_...` ID or local slug and forwards
to `blacksmith testbox stop`. In `namespace-devbox` mode it shuts down the
Namespace Devbox by default and removes the local claim; set
`namespace.deleteOnRelease` to delete the Devbox instead. In `semaphore` mode it
stops the Semaphore CI job and removes the local claim. In `sprites` mode it
deletes the Sprites sprite and removes the local claim. In `daytona` mode it
deletes the Daytona sandbox.
In `islo` mode it accepts an `isb_...` ID, Crabbox-created sandbox name, or
local slug and deletes the Islo sandbox. In `e2b` mode it accepts a Crabbox
lease ID, local slug, or Crabbox-owned E2B sandbox ID in raw or
`e2b_<sandboxID>` form and deletes the E2B sandbox. In `provider=ssh` mode it
removes the local claim for the configured static target; it never deletes the
host.

Flags:

```text
--provider hetzner|aws|azure|gcp|proxmox|ssh|blacksmith-testbox|namespace-devbox|semaphore|sprites|daytona|islo|e2b
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--namespace-delete-on-release
--sprites-api-url <url>
--e2b-api-url <url>
--e2b-domain <domain>
```
