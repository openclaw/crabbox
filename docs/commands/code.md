# code

`crabbox code` bridges a code-server workspace for a Linux lease into the
authenticated coordinator portal.

```sh
crabbox warmup --code
crabbox code --id blue-lobster
crabbox code --id blue-lobster --open
```

## How It Works

Create or reuse a lease with `code=true`:

```sh
crabbox warmup --code
```

The Linux bootstrap installs `code-server` only for leases that request the
capability. `crabbox code` then resolves the lease, starts `code-server` on
runner loopback, opens an SSH tunnel, mints a short-lived bridge ticket, and
registers a local bridge with the coordinator.

The editor opens the synced workspace by default. If you run `crabbox code`
from a subdirectory inside the local checkout, Crabbox maps that relative path
onto the remote workspace and opens the matching folder. Actions-hydrated
leases use the hydration workspace instead of the default `/work/crabbox/...`
path.

The browser URL is lease-scoped:

```text
/portal/leases/<lease-id>/code/
```

The data path is:

```text
browser
  <-> coordinator /portal/leases/<lease>/code/
  <-> local crabbox code process
  <-> SSH tunnel
  <-> runner 127.0.0.1:8080
```

Keep the local `crabbox code` process running while using the editor. The
coordinator authenticates the browser through portal auth and authenticates the
local bridge with a one-use, short-lived ticket. The CLI sends the ticket as
an `Authorization: Bearer ...` header so it stays out of websocket URLs and
proxy/access logs; the coordinator accepts a `?ticket=` query string as a
fallback for older CLIs.

If the browser opens before the local bridge connects, the Code portal renders a
waiting state with the exact `crabbox code --id <lease> --open` command, copy
and reload controls, and bridge status. Once the bridge is connected, the page
automatically opens the mapped workspace.

Managed code-server starts with `Default Dark Modern` as the default theme. The
bridge also chunks large HTTP responses and websocket frames so VS Code assets
and extension-host traffic stay below coordinator websocket frame limits.

## Flags

```text
--id <lease-id-or-slug>
--provider hetzner|aws|azure
--target linux
--network auto|tailscale|public
--local-port <port>
--open
--reclaim
```

## Limitations

- Coordinator-backed Linux leases are supported.
- Static SSH hosts, Windows, macOS, and Blacksmith Testbox are intentionally not
  supported by this portal bridge yet.
- `code-server` auth is disabled on the runner side because the trusted access
  boundary is the authenticated coordinator portal plus the local bridge.

## Troubleshooting

`lease ... was not created with code=true`

Warm a new lease with the code capability:

```sh
crabbox warmup --code
```

The portal shows a bridge command

The browser can reach the coordinator, but no local bridge is registered. Use
the command shown by the portal, or start `crabbox code --id <lease> --open`
locally and keep it running.

Check bridge health with:

```sh
curl https://crabbox.openclaw.ai/portal/leases/<lease>/code/health
```

When authenticated, the health response includes whether the code bridge agent
is currently connected.
