# code

`crabbox code` opens a Linux lease's `code-server` workspace in browser VS Code
without exposing the runner directly. Coordinator-backed providers use the
authenticated [portal](../features/portal.md). Coordinator-free managed SSH
providers use a local loopback URL forwarded through SSH.

```sh
crabbox warmup --code --slug swift-crab
crabbox run --id swift-crab --sync-only
crabbox code --id swift-crab
crabbox code --id swift-crab --open
```

## Prerequisites

- A lease created with the `code` capability (`crabbox warmup --code`). The
  Linux bootstrap installs `code-server` only for leases that request it, and
  reusing a lease checks for the matching `code=true` label.
- A managed Linux SSH lease on a provider that advertises `code`. Static SSH
  hosts remain host-managed and are rejected. Windows, macOS, and delegated-run
  providers are also rejected.
- Coordinator-backed providers require a configured login
  (`crabbox login --url broker.example.com`) and a valid
  `CRABBOX_CODE_ORIGIN_TEMPLATE` backed by wildcard TLS and WebSocket ingress.
  Coordinator-free managed providers, including Hyper-V Linux, do not require
  a coordinator login.

## How it works

`crabbox code` resolves the lease, ensures `code-server` is running on the
runner's loopback interface (`127.0.0.1:8080`), and opens an SSH tunnel to it.
Keep the process running while you use the editor.

`code` does not sync local files. Run `run --sync-only` first, or open an
already synced or Actions-hydrated workspace.

For coordinator-backed providers, the data path is:

```text
browser
  <-> coordinator /portal/leases/<lease-id>/code/
  <-> local crabbox code process (bridge)
  <-> SSH tunnel
  <-> runner 127.0.0.1:8080 (code-server)
```

For coordinator-free managed providers, the CLI prints a local URL such as
`http://127.0.0.1:43123` and `--open` opens that URL directly:

```text
browser 127.0.0.1:<local-port>
  <-> local crabbox code SSH tunnel
  <-> runner 127.0.0.1:8080 (code-server)
```

The coordinator authenticates the browser through portal auth and authenticates
the local bridge with a one-use, short-lived ticket. The CLI sends the ticket as
an `X-Crabbox-Bridge-Ticket` WebSocket upgrade header so it stays out of
WebSocket URLs while leaving ordinary coordinator authentication intact. A
bearer-header retry supports older coordinators. Current coordinators reject
bridge tickets in URL query strings by default, so older CLIs that still send
query-ticket bridges must be upgraded before they can connect. Operators who
need a temporary legacy rollout window can set
`CRABBOX_ALLOW_QUERY_BRIDGE_TICKETS=1`; remove that setting after affected
clients upgrade. Because the trusted boundary is the portal plus the bridge
ticket, `code-server` runs with auth disabled on the runner side.

Direct mode also runs `code-server` with `--auth none`, but both ends are
loopback-only: code-server binds only to guest `127.0.0.1:8080`, the SSH
forward binds only to local `127.0.0.1:<port>`, and no public listener is
created. The unauthenticated HTTP service is reachable only through the local
SSH session.

The portal URL is lease-scoped:

```text
/portal/leases/<lease-id>/code/
```

Displayed and opened portal URLs never include coordinator URL usernames,
passwords, original query parameters, or fragments. Coordinator HTTP and bridge
authentication retain their configured credentials; browsers authenticate
through their existing portal session or normal browser authentication.

If the browser opens before the local bridge connects, the Code portal renders a
waiting state with the exact `crabbox code` command, copy/reload controls, and
bridge status; it opens the workspace automatically once the bridge connects.

### Folder mapping

The editor opens the synced workspace by default. If you run `crabbox code` from
a subdirectory of the local checkout, Crabbox maps that relative path onto the
remote workspace and opens the matching folder. [Actions-hydrated](../features/actions-hydration.md)
leases open the hydration workspace instead of the default
`/work/crabbox/<lease>/<repo>` path.

### Resilience

Managed `code-server` starts with `Default Dark Modern` as its theme.
Coordinator mode chunks large HTTP responses and websocket frames so VS Code
assets and extension-host traffic stay under coordinator websocket frame
limits, and it reconnects automatically on transient bridge errors. Direct mode
keeps the SSH tunnel alive until the command is canceled.

Ctrl+C interrupts both the code-server readiness wait and the reconnect delay;
it does not wait for the next retry. The local bridge and SSH tunnel close,
while the lease remains available for reuse.

## Flags

```text
--id <lease-id-or-slug>     Lease to bridge (also accepted as a positional arg).
--provider <provider>        Managed SSH provider for the lease (default from config).
--target linux              Lease target OS (code requires linux).
--network auto|tailscale|public  Network mode used to reach the runner.
--local-port <port>         Local loopback tunnel port (automatically selected if unset).
--open                      Open the portal or direct local URL in a browser.
--reclaim                   Claim this lease for the current repo checkout.
```

Set `CRABBOX_CODE_DEBUG=1` to print coordinator bridge trace output to stderr.

## Troubleshooting

**`lease ... was not created with code=true`** — warm a new lease with the
capability:

```sh
crabbox warmup --code
```

**`code requires a configured coordinator login`** — log in to the broker:

```sh
crabbox login --url broker.example.com
```

This error applies only to coordinator-backed providers. A direct managed
provider with coordinator policy `never` uses the local SSH tunnel instead.

**`code requires a managed SSH lease`** — the provider is host-managed or does
not advertise managed cleanup. Static SSH intentionally does not provision
code-server; use a managed Linux provider and create the lease with `--code`.

**The portal shows a bridge command** — the browser reached the coordinator but
no local bridge is registered. Run the command the portal shows (or
`crabbox code --id <lease> --open`) and keep it running.

**Check bridge health:**

```sh
curl https://broker.example.com/portal/leases/<lease-id>/code/health
```

When authenticated, the health response reports whether the code bridge agent is
currently connected.

## See also

- [`webvnc`](webvnc.md) — bridge a desktop lease into the portal.
- [capabilities](../features/capabilities.md) — `--desktop`, `--browser`, `--code`.
- [portal](../features/portal.md) — the authenticated browser UI.
