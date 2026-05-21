# Crew SSH-mesh

Read when:

- you want the operator's shell to dial crew peers by name without relying on
  Tailscale, Headscale, a SaaS relay, or any provider-specific network;
- the provider only gives you SSH access to each lease (AWS, Proxmox,
  exe.dev, RunPod, Sprites, Namespace, Semaphore, Daytona, plain SSH);
- a Tailscale plane is not available, not desired, or not allowed.

The SSH-mesh plane is operator-mediated: `crabbox crew connect <name>` opens
local `ssh -L` tunnels from the operator's machine to every crew member that
declared `--expose <port>` on its `crabbox run` / `warmup` call. The operator
(and any tool the operator launches in the same shell) can then reach peers
at `127.0.0.1:<port>`. No daemon ships to the lease, no relay, no SaaS —
just the same SSH credentials the SSH-lease providers already configure.

## How it composes with the Tailscale plane

The Tailscale plane (`docs/features/crew.md`) gives lease-to-lease peer
discovery and lets two boxes inside a tailnet reach each other as
`<slug>.cbx`. The SSH-mesh plane gives the operator-side dial path that
every SSH-accessible provider supports out of the box. The two planes are
complementary:

| Plane       | Reach (operator -> peer) | Reach (peer -> peer)             | Providers                           |
| ----------- | ------------------------ | -------------------------------- | ----------------------------------- |
| Tailscale   | via tailnet              | yes, `<slug>.cbx`                | Hetzner, Azure, GCP managed Linux   |
| SSH-mesh    | yes, `127.0.0.1:<port>`  | not in v1 (operator-side only)   | every SSH-lease provider            |

`crabbox doctor --crew <name>` reports both planes alongside each other.

## Declaring exposed ports

Add one `--expose <port>` per TCP port the lease wants reachable through
the SSH-mesh:

```sh
crabbox warmup --crew alpha --slug web    --expose 8080
crabbox warmup --crew alpha --slug client --expose 8080 --expose 9090
```

The flag is repeatable and accepts comma-separated lists
(`--expose 8080,9090`). Crabbox writes the normalized port list into a
reserved provider label (`crabbox_exposed_ports=8080-9090`) so
`crew connect` can discover the ports without growing a new store.

## Opening the mesh

```sh
crabbox crew connect alpha
```

Crabbox reads the active crew members, allocates one loopback port per
declared exposed port, and opens an `ssh -L 127.0.0.1:<local>:127.0.0.1:<remote>`
tunnel per (peer, port). The same connection options the rest of the CLI
uses (ControlMaster + ControlPersist for connection reuse) apply, so the
fan-out is cheap even with several peers.

Crabbox writes two files under `~/.crabbox/crew/<name>/`:

- `hosts` — operator-readable mapping from `<peer>.cbx (remote :<port>)`
  to its assigned `127.0.0.1:<local>` address.
- `env` — `export CRABBOX_CREW_<PEER>_<PORT>=127.0.0.1:<local>` for each
  forward, so the operator can `eval $(crabbox crew connect <name> --export)`
  and dial peers by variable.

```sh
eval $(crabbox crew connect alpha --export)
curl "$CRABBOX_CREW_WEB_8080/health"
```

The default invocation holds the tunnels open in the foreground; press
Ctrl-C to tear them down. Use `--json` to print the forward table without
holding the connections.

## What this is NOT

- **Lease-to-lease peer dial** (true P2P mesh). That requires an
  operator-side hub plus reverse tunnels and is a future PR.
- **UDP forwarding.** `ssh -L` is TCP-only.
- **Auto-plane selection.** v1 expects the operator to pick the SSH-mesh
  plane explicitly via `crew connect`; the Tailscale plane keeps its own
  auto-managed path.

## Doctor

`crabbox doctor --crew <name>` adds a `crew-mesh` sub-check that reports:

- how many active members the crew has,
- how many declared `--expose` ports,
- the total port count.

It is a `skip` when no member has declared a port (the plane has nothing
to forward), and `ok` once at least one port is in scope.

## Why the operator owns this

Putting the mesh in the operator's shell keeps the broker out of the
network path. The SSH credentials needed to open `-L` forwards are the
ones the operator already holds for `crabbox ssh`, so no new secret
flows through Crabbox. The plane stays usable on providers that have no
Tailscale support and on locked-down networks that block tailnet traffic.
