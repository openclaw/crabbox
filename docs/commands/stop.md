# stop

`crabbox stop` releases a coordinator lease or deletes a direct-provider machine.

```sh
crabbox stop blue-lobster
crabbox stop --provider ssh --static-host mac-studio.local mac-studio.local
```

`crabbox release` remains as a compatibility alias.
The argument accepts the stable `cbx_...` ID or an active friendly slug. In `blacksmith-testbox` mode it accepts a `tbx_...` ID or local slug and forwards to `blacksmith testbox stop`. In `provider=ssh` mode it removes the local claim for the configured static target; it never deletes the host.

Flags:

```text
--provider hetzner|aws|ssh|blacksmith-testbox
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
```
