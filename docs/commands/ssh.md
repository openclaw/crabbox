# ssh

`crabbox ssh` prints the SSH command for a lease.

```sh
crabbox ssh --id blue-lobster
crabbox ssh --id blue-lobster --network tailscale
crabbox ssh --provider ssh --target macos --static-host mac-studio.local
```

The output includes the per-lease private key path when Crabbox created one. Printing an SSH command touches coordinator leases because it signals intended manual use. In `provider=ssh` mode it resolves the configured static target.

Flags:

```text
--id <lease-id-or-slug>
--provider hetzner|aws|ssh
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--network auto|tailscale|public
--reclaim
```

`ssh` touches the lease and validates the local repo claim. Use `--reclaim` when intentionally taking over a lease from another repo.

`--network auto` prefers the tailnet host when the lease has Tailscale metadata
and this client can reach it. `--network tailscale` requires that path.
`--network public` forces the provider host.
