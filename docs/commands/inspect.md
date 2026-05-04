# inspect

`crabbox inspect` prints detailed lease and provider metadata.

```sh
crabbox inspect --id blue-lobster
crabbox inspect --id blue-lobster --network tailscale
crabbox inspect --id blue-lobster --json
crabbox inspect --provider ssh --target windows --windows-mode wsl2 --static-host win-dev.local
```

Use this for debugging coordinator state, provider labels, expiry, and SSH target details.

Flags:

```text
--id <lease-id-or-slug>
--provider hetzner|aws|ssh
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--network auto|tailscale|public
--json
```

JSON output includes non-secret Tailscale metadata when present. Human output
prints both the provider host and the resolved SSH command for the selected
network.
