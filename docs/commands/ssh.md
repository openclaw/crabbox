# ssh

`crabbox ssh` resolves a lease and prints a ready-to-run `ssh` command for it.
It does not open a connection itself — it prints the command so you can copy it,
pipe it, or wrap it in your own tooling.

```sh
crabbox ssh --id swift-crab
crabbox ssh --id swift-crab --network tailscale
crabbox ssh --provider ssh --target macos --static-host mac-studio.local
```

The first positional argument is treated as the lease id or slug, so
`crabbox ssh swift-crab` is equivalent to `crabbox ssh --id swift-crab`.

## What it prints

The printed command includes the per-lease private key (`-i <key>`) when Crabbox
generated one, the resolved host, user, and port, and the connection options
Crabbox uses internally (`BatchMode`, `StrictHostKeyChecking=accept-new`, a
per-lease `known_hosts` file, and connection keepalives). For most providers
that is a plain `ssh -i … user@host -p <port>` line you can run as-is.

`crabbox ssh` touches the lease as a side effect — printing the command signals
intended manual use, so the lease's idle timer is refreshed and the local repo
claim is validated. Pass `--reclaim` when you are intentionally taking over a
lease that is claimed by another repo checkout.

## Network selection

The `--network` flag controls which path to the box the command targets:

- `auto` (default) prefers the tailnet host when the lease carries Tailscale
  metadata and this client can reach it, otherwise falls back to the public
  provider host.
- `tailscale` requires the tailnet path and fails if it is unavailable.
- `public` always uses the provider's public host.

## Provider-specific behavior

`crabbox ssh` works for any provider that exposes an SSH-reachable box. A few
providers resolve access lazily or wrap the connection:

- **`provider=ssh`** (aliases `static`, `static-ssh`) resolves the configured
  static target from `--static-host`/`--static-user`/`--static-port` (or the
  matching config keys / `CRABBOX_STATIC_HOST`) instead of a leased machine.
- **Token-based providers** (for example `daytona`) authenticate with a
  short-lived SSH token embedded in the command. Crabbox redacts that secret by
  default and prints a warning; pass `--show-secret` only when you need a
  pasteable command in a trusted terminal.
- **Proxy-based providers** (for example `sprites`) print an `ssh` command with
  a provider `ProxyCommand` rather than a direct host connection.
- **`provider=xcp-ng`** resolves the VM IPv4 address from XCP-ng guest metrics
  during provisioning, then prints the normal per-lease SSH command for the
  cloud-init user.
- **Provider-routed direct providers** accept the same provider-specific routing
  flags here as `status` and `stop`; for example `--kubevirt-context` or
  `--external-routing-file` can select the exact lease backend when config
  defaults are not enough.

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
--show-secret               Print secret auth material for token-based SSH providers.
```

`--provider` accepts any provider that supports SSH access; run
[`crabbox providers`](providers.md) to see the available set and their
capabilities.

## See also

- [`crabbox warmup`](warmup.md) — lease a box and wait until it is ready.
- [`crabbox status`](status.md) / [`crabbox inspect`](inspect.md) — check lease
  state and connection details.
- [`crabbox vnc`](vnc.md) / [`crabbox code`](code.md) — other access bridges for
  desktop and editor leases.
- [`crabbox stop`](stop.md) — end the lease when you are done.
