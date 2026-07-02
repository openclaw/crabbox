# code

`crabbox code` bridges a Linux lease's `code-server` workspace into the
authenticated coordinator [portal](../features/portal.md), so you can edit the
synced checkout in a browser VS Code without exposing the runner directly.

```sh
crabbox warmup --code
crabbox code --id swift-crab
crabbox code --id swift-crab --open
```

## Prerequisites

- A configured coordinator login. The command refuses to run without one:
  `crabbox login --url broker.example.com` first.
- A valid coordinator `CRABBOX_CODE_ORIGIN_TEMPLATE` backed by wildcard TLS and
  WebSocket ingress. Browser Code fails closed when the template is absent or
  invalid.
- A lease created with the `code` capability (`crabbox warmup --code`). The
  Linux bootstrap installs `code-server` only for leases that request it, and
  reusing a lease checks for the matching `code=true` label.
- A coordinator-backed Linux lease on a provider that advertises the `code`
  capability (`hetzner`, `aws`, `azure`). Static SSH hosts, Blacksmith Testbox,
  Windows, and macOS leases are rejected.

## How it works

`crabbox code` resolves the lease, ensures `code-server` is running on the
runner's loopback interface (`127.0.0.1:8080`), opens an SSH tunnel to it, mints
a short-lived bridge ticket from the coordinator, and registers a local bridge
process. Keep the process running while you use the editor.

The data path is:

```text
browser
  <-> coordinator /portal/leases/<lease-id>/code/
  <-> local crabbox code process (bridge)
  <-> SSH tunnel
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

The portal URL is lease-scoped:

```text
/portal/leases/<lease-id>/code/
```

If the browser opens before the local bridge connects, the Code portal renders a
waiting state with the exact `crabbox code` command, copy/reload controls, and
bridge status; it opens the workspace automatically once the bridge connects.

### Folder mapping

The editor opens the synced workspace by default. If you run `crabbox code` from
a subdirectory of the local checkout, Crabbox maps that relative path onto the
remote workspace and opens the matching folder. [Actions-hydrated](../features/actions-hydration.md)
leases open the hydration workspace instead of the default
`/work/crabbox/<repo>` path.

### Resilience

Managed `code-server` starts with `Default Dark Modern` as its theme. The bridge
chunks large HTTP responses and websocket frames so VS Code assets and
extension-host traffic stay under coordinator websocket frame limits, and it
reconnects automatically on transient bridge errors.

## Flags

```text
--id <lease-id-or-slug>     Lease to bridge (also accepted as a positional arg).
--provider hetzner|aws|azure  Provider for the lease (default from config).
--target linux              Lease target OS (code requires linux).
--network auto|tailscale|public  Network mode used to reach the runner.
--local-port <port>         Local code-server tunnel port (auto-selected 8081-8180 if unset).
--open                      Open the portal Code page in a browser.
--reclaim                   Claim this lease for the current repo checkout.
```

Set `CRABBOX_CODE_DEBUG=1` to print bridge trace output to stderr.

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
