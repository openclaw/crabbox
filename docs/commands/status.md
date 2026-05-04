# status

`crabbox status` prints the current state for a lease.

```sh
crabbox status --id blue-lobster
crabbox status --id blue-lobster --network tailscale
crabbox status --id blue-lobster --wait --wait-timeout 10m
crabbox status --id blue-lobster --json
crabbox status --provider ssh --target macos --static-host mac-studio.local
```

`--id` accepts the canonical `cbx_...` ID or active slug. In `blacksmith-testbox` mode it accepts a `tbx_...` ID or local slug and forwards to `blacksmith testbox status`. In `provider=ssh` mode `--id` is optional and resolves the configured static target or local claim. Plain status is read-only; `--wait` touches the lease while waiting for Crabbox brokered leases.

Flags:

```text
--id <lease-id-or-slug>
--provider hetzner|aws|ssh|blacksmith-testbox
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--network auto|tailscale|public
--wait
--wait-timeout <duration>
--json
```

Human and JSON output include the selected network. With Tailscale metadata,
status also prints the tailnet host/state.
