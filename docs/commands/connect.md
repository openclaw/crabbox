# connect

`crabbox connect` resolves a lease and opens an interactive SSH session to it.
It accepts the same provider, target, and network resolution flags as
[`crabbox ssh`](ssh.md), but runs the local `ssh` client directly instead of
printing a command string.

```sh
crabbox connect swift-crab
crabbox connect --id swift-crab --network tailscale
crabbox connect --provider ssh --target macos --static-host mac-studio.local
```

The first positional argument is treated as the lease id or slug, so
`crabbox connect swift-crab` is equivalent to
`crabbox connect --id swift-crab`.

## What it opens

The command uses the resolved SSH target directly: host, user, port, per-lease
private key, optional certificate, optional provider `ProxyCommand`, host-key
policy, and connection keepalives. It does not pass through a shell and does not
print token-based SSH auth material.

`crabbox connect` touches the lease as a side effect. Opening an interactive
session signals intended manual use, so the lease's idle timer is refreshed and
the local repo claim is validated. While the SSH process is running, Crabbox
keeps coordinator-backed leases heartbeating and marks direct-provider leases
running/ready where supported. Pass `--reclaim` when you are intentionally
taking over a lease that is claimed by another repo checkout.

## Network selection

The `--network` flag controls which path to the box the SSH session targets:

- `auto` (default) prefers the tailnet host when the lease carries Tailscale
  metadata and this client can reach it, otherwise falls back to the public
  provider host.
- `tailscale` requires the tailnet path and fails if it is unavailable.
- `public` always uses the provider's public host.

## Provider-specific behavior

`crabbox connect` works for any provider that exposes an SSH-reachable box. A
few providers resolve access lazily or wrap the connection:

- **`provider=ssh`** (aliases `static`, `static-ssh`) resolves the configured
  static target from `--static-host`/`--static-user`/`--static-port` (or the
  matching config keys / `CRABBOX_STATIC_HOST`) instead of a leased machine.
- **Token-as-username providers** are rejected by `connect` because safely
  handing their secret username to every supported OpenSSH client is not
  portable. Use [`crabbox ssh --show-secret`](ssh.md) in a trusted terminal.
- **Proxy-based providers** pass the provider `ProxyCommand` directly to `ssh`.
- **Provider-routed direct providers** accept the same provider-specific routing
  flags here as `status`, `stop`, and `ssh`; for example
  `--kubevirt-context` or `--external-routing-file` can select the exact lease
  backend when config defaults are not enough.

## Flags

```text
--id <lease-id-or-slug>     Lease to resolve (also accepted as the first argument).
--provider <name>           Provider to resolve against (default from config).
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>        Static target host (provider=ssh).
--static-user <user>        Static target user (provider=ssh).
--static-port <port>        Static target SSH port (provider=ssh).
--static-work-root <path>   Static target work root (provider=ssh).
--local-container-runtime <path-or-name>
                              Local container CLI override (provider=local-container).
--network auto|tailscale|public
--reclaim                   Claim this lease for the current repo before touching it.
```

`--provider` accepts any provider that supports SSH access; run
[`crabbox providers`](providers.md) to see the available set and their
capabilities.

## See also

- [`crabbox ssh`](ssh.md) - print the exact SSH command instead of opening it.
- [`crabbox warmup`](warmup.md) - lease a box and wait until it is ready.
- [`crabbox status`](status.md) / [`crabbox inspect`](inspect.md) - check lease
  state and connection details.
- [`crabbox stop`](stop.md) - end the lease when you are done.
