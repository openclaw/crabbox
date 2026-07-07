# status

`crabbox status` prints the current state of a lease: its slug, provider,
target OS, host, network, readiness, idle time, and expiry. By default it is a
read-only snapshot. Add `--wait` to block until the box becomes ready (or
reaches a terminal state).

```sh
crabbox status --id swift-crab
crabbox status --id swift-crab --json
crabbox status --id swift-crab --wait --wait-timeout 10m
crabbox status --id swift-crab --network tailscale
crabbox status --provider ssh --target macos --static-host mac-studio.local
```

## Identifying the lease

`--id` accepts the canonical `cbx_...` lease ID or an active slug. For
`provider=ssh` (static hosts),
`--id` is optional: status resolves the configured static target or the local
claim for the current repo.

Several delegated and direct providers resolve their own native identifiers in
addition to the Crabbox lease ID and local slug:

- `blacksmith-testbox` — accepts a `tbx_...` ID or local slug; derives a
  normalized status view from `blacksmith testbox list --all`.
- `blaxel` — accepts a Crabbox lease ID (`blx_<sandbox-id>`) or local slug,
  verifies the local claim against Blaxel ownership labels, and reports the
  sandbox state, endpoint, pond, and readiness.
- `namespace-devbox` — accepts a lease ID, local slug, or existing Devbox name;
  prepares SSH through the Namespace CLI.
- `namespace-instance` — accepts a lease ID, local slug, or Compute instance ID;
  prepares SSH through `nsc proxy`.
- `exe-dev` — accepts a lease ID, local slug, or exe.dev VM name; resolves the
  VM through `ssh exe.dev ls`.
- `semaphore` — resolves local claims and Semaphore job state through the
  Semaphore API.
- `sprites` — resolves local claims, Sprites labels, and SSH readiness through
  `sprite proxy`.
- `daytona` — resolves Crabbox labels and sandbox state through the Daytona API.
- `coder` — accepts a Crabbox lease ID, local slug, Coder workspace name, or
  `owner/workspace`; plain status reads Coder inventory without starting stopped
  workspaces.
- `islo` — accepts an `isb_...` ID, a Crabbox-created sandbox name, or a local
  slug.
- `e2b` — accepts a lease ID, local slug, or a Crabbox-owned E2B sandbox ID in
  raw or `e2b_<sandboxID>` form.
- `vercel-sandbox` — accepts a Crabbox-created local slug or `vsbx_...` lease
  ID and verifies the Vercel Sandbox ownership metadata before reporting state.
- `cloudflare-dynamic-workers` — accepts a local Dynamic Workers claim,
  lifecycle run ID, or slug, then asks the loader for run metadata. Stable and
  explicit Worker cache IDs are not lifecycle IDs. The status target is
  `worker-runtime`, not a Linux host.

## Waiting for readiness

Plain status never modifies the lease. With `--wait`, status polls every five
seconds until the box is ready or reaches a terminal state
(`expired`, `failed`, `released`, `stopped`, `stopped_with_code`,
`terminated`), and exits non-zero (code 5) if it times out (`--wait-timeout`,
default `5m`) or the lease becomes terminal before it is ready. For direct
SSH-lease providers, each poll while waiting also touches the lease to keep it
from idling out only when an exact local claim matches the configured provider
scope and live resource identity. Claimless or mismatched resources remain
strictly read-only while status waits.

## Flags

```text
--id <lease-id-or-slug>
--provider hetzner|aws|azure|azure-dynamic-sessions|gcp|proxmox|ssh|exe-dev|blacksmith-testbox|blaxel|namespace-devbox|semaphore|sprites|tenki|coder|daytona|islo|e2b|vercel-sandbox
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--network auto|tailscale|public
--wait
--wait-timeout <duration>   (default 5m)
--json
```

Some providers add their own resolution flags, including
`--exe-dev-control-host`, `--sprites-api-url`, `--sprites-work-root`,
`--azure-dynamic-sessions-endpoint`, `--azure-dynamic-sessions-api-version`,
`--e2b-api-url`, and `--e2b-domain`.

## Output

Human-readable output is a single line covering the lease ID, slug, provider,
target, Windows mode, state, instance type, host, pond, network, readiness,
idle time, idle timeout, and expiry. When the lease carries Tailscale metadata,
status also prints the tailnet host and state. The selected
[network mode](../features/network.md) is always shown in both human and JSON
output.

For coordinator-backed Linux leases that have received a recent heartbeat,
status also prints the latest best-effort
[telemetry](../features/telemetry.md) snapshot: load, memory, disk, uptime, and
capture age.

`--json` emits the full status view. In addition to every field above, it
includes SSH connection details, lease labels, and `telemetryHistory` when the
coordinator has retained recent samples (bounded to the latest 60 per lease) for
portal trend charts. Under `--wait`, JSON is printed once the lease is ready or
terminal rather than on every poll.

## See also

- [`inspect`](inspect.md) — fuller lease and provider detail.
- [`list`](list.md) — all machines for a provider.
- [`warmup`](warmup.md) — lease a box and wait until ready.
