# status

`crabbox status` prints the current state for a lease.

```sh
crabbox status --id blue-lobster
crabbox status --id blue-lobster --network tailscale
crabbox status --id blue-lobster --wait --wait-timeout 10m
crabbox status --id blue-lobster --json
crabbox status --provider daytona --id blue-lobster
crabbox status --provider islo --id blue-lobster
crabbox status --provider ssh --target macos --static-host mac-studio.local
```

`--id` accepts the canonical `cbx_...` ID or active slug. In
`blacksmith-testbox` mode it accepts a `tbx_...` ID or local slug and derives a
normalized Crabbox status view from `blacksmith testbox list --all`. In
`daytona` mode it resolves Crabbox labels and sandbox state through Daytona
APIs. In `islo` mode it accepts an `isb_...` ID, Crabbox-created sandbox name,
or local slug and renders SDK status through the core status view. In
`provider=ssh` mode `--id` is optional and resolves the configured static target
or local claim. Plain status is read-only; `--wait` touches the lease while
waiting for Crabbox brokered leases.

Flags:

```text
--id <lease-id-or-slug>
--provider hetzner|aws|azure|ssh|blacksmith-testbox|daytona|islo
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--network auto|tailscale|public
--wait
--wait-timeout <duration>
--json
```

Human and JSON output include the selected network. With Tailscale metadata,
status also prints the tailnet host/state. For coordinator-backed Linux leases
that have received a recent heartbeat, status also includes the latest
best-effort telemetry snapshot: load, memory, disk, uptime, and capture age.
JSON status includes `telemetryHistory` when the coordinator has retained recent
samples for portal trend charts. The retained history is bounded to the latest
60 samples per lease.
