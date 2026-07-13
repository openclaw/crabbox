# tunnel

`crabbox tunnel` forwards one loopback port from an SSH-reachable lease to the
local machine. The command prints the local address after the forward accepts
connections, then stays attached until you stop it or its SSH process exits.

```sh
crabbox tunnel --id swift-crab --port 3000
crabbox tunnel --id swift-crab --port 3000 --local-port 41000
crabbox tunnel --id swift-crab --port 3000 --json
```

## Network boundary

The forward binds to `127.0.0.1` on both sides. It does not publish the remote
service to the local network or expose a service that listens on a non-loopback
remote address.

Crabbox resolves the same SSH target as `crabbox connect`, including its key,
certificate, proxy command, network selection, and host-key policy. The command
rejects token-as-username targets because passing that secret through the local
SSH client is not portable.

## Readiness output

Without `--local-port`, Crabbox chooses an available local port. Text output
contains the local address:

```text
127.0.0.1:41000
```

Pass `--json` when another process needs to keep the tunnel open and consume
its coordinates:

```json
{"port":41000,"remotePort":3000}
```

The command writes this output only after the local forward accepts a
connection. It exits with an error when SSH stops before readiness or the
forward does not become ready within ten seconds.

## Flags

```text
--id <lease-id-or-slug>     Lease to resolve. You may also pass it as the first argument.
--provider <name>           Provider to resolve against.
--port <remote-port>        Required remote loopback port.
--local-port <local-port>   Local loopback port. The default chooses an available port.
--json                      Print machine-readable tunnel coordinates.
--network auto|tailscale|public
--reclaim                   Claim the lease for the current repository before touching it.
```

## Provider support

`crabbox tunnel` works with providers that expose an SSH-reachable lease. It
does not replace provider-native public URLs or port publishing; use
`crabbox ports` when a provider owns that bridge.

## See also

- [`connect`](connect.md) - open an interactive SSH session to a lease.
- [`ports`](ports.md) - publish or inspect provider-native ports.
- [`cp`](cp.md) - copy data between the host and a lease.
