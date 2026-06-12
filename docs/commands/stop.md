# stop

`crabbox stop` ends a single lease. For coordinator-backed and direct cloud
providers it releases or deletes the backing machine; for delegated runners it
tears down the underlying sandbox; for static `provider=ssh` hosts it only
removes the local claim and never touches the host.

```sh
crabbox stop swift-crab
crabbox stop --id cbx_0a1b2c3d4e5f
crabbox stop --provider namespace-devbox swift-crab
crabbox stop --provider daytona swift-crab
crabbox stop --provider e2b swift-crab
crabbox stop --provider ssh --static-host mac-studio.local mac-studio.local
```

`crabbox release` is a compatibility alias for `crabbox stop`.

## Identifying the lease

Pass the lease as a positional argument or with `--id`; both accept the
canonical `cbx_...` ID or an active friendly slug (see
[Identifiers](../features/identifiers.md)). Supplying both `--id` and a
positional argument, or more than one positional argument, is an error.

Several providers also accept their own native identifiers in addition to the
Crabbox lease ID and local slug:

- `blacksmith-testbox` — accepts a `tbx_...` ID or local slug and forwards to
  `blacksmith testbox stop`.
- `blaxel` — accepts a Crabbox lease ID (`blx_<sandbox-id>`) or local slug and
  deletes the Blaxel sandbox only when the local claim and remote ownership
  labels match. Missing sandboxes keep the local claim unless
  `--blaxel-forget-missing` is set.
- `namespace-devbox` — shuts down the Namespace Devbox by default and retains
  its local claim and SSH files for reuse. Set `namespace.deleteOnRelease` (or
  pass `--namespace-delete-on-release`) to delete the Devbox and local SSH
  files instead.
- `namespace-instance` — accepts a lease ID, local slug, or Namespace instance
  ID and destroys the Compute instance with `nsc destroy --force`.
- `morph` — pauses the instance by default and retains its local claim and SSH
  key for reuse. Set `morph.deleteOnRelease` (or pass
  `--morph-delete-on-release`) to delete the instance and key instead.
- `exe-dev` — accepts a Crabbox lease ID, local slug, or exe.dev VM name and
  deletes the VM through `ssh exe.dev rm`.
- `semaphore` — stops the Semaphore CI job and removes the local claim.
- `sprites` — deletes the Sprites sprite and removes the local claim.
- `daytona` — deletes the Daytona sandbox.
- `islo` — accepts an `isb_...` ID, a Crabbox-created sandbox name, or a local
  slug and deletes the Islo sandbox.
- `e2b` — accepts a Crabbox lease ID, a local slug, or a Crabbox-owned E2B
  sandbox ID in raw or `e2b_<sandboxID>` form and deletes the E2B sandbox.
- `vercel-sandbox` — accepts a Crabbox-created local slug or `vsbx_...` lease
  ID, verifies ownership metadata, deletes the Vercel Sandbox, and removes the
  local claim. Missing remote sandboxes preserve the claim unless
  `--vercel-sandbox-forget-missing` is explicit.
- `cloudflare-dynamic-workers` — accepts a local claim, lifecycle run ID, or
  slug, deletes loader metadata for that run, and removes the local claim.
  Stable and explicit Worker cache IDs are not lifecycle IDs. If the loader
  already reports `not found`, Crabbox removes the stale local claim.
- `cloudflare-sandbox` — accepts a Crabbox-created local slug or `cfsbx_...`
  lease ID, verifies ownership metadata, deletes the Cloudflare Sandbox through
  the configured bridge, and removes the local claim. Missing remote sandboxes
  preserve the claim unless `--cloudflare-sandbox-forget-missing` is explicit.
- `docker-sandbox` — accepts only a Crabbox lease ID or local slug backed by a
  `provider=docker-sandbox` local claim, then removes the sandbox with
  `sbx rm --force`. This is destructive cleanup, not Docker Sandbox pause, and
  it remains the manual cleanup path for clone-mode Docker Sandbox runs that
  Crabbox keeps after a successful one-shot command.
- `hostinger` — stops the VPS and retains its local claim and SSH key for later
  reuse. Hostinger still owns the subscription and may continue billing it.
- `ssh` (static hosts) — removes the local claim for the configured static
  target; it never deletes the host.
- `xcp-ng` — accepts a Crabbox lease ID or local slug for a Crabbox-managed VM,
  deletes the attached config drive when present, and refuses to touch VMs that
  are not labeled as Crabbox-managed XCP-ng leases.

## Behavior by provider mode

The action `stop` takes depends on how the lease was created:

- **Coordinator-backed** (`aws`, `azure`, `gcp`, `hetzner` brokered through a
  configured broker) — releases the lease through the broker and prints
  `released lease=<id> server=<id>`. If the lease cannot be inspected first,
  `stop` warns and still attempts the release by ID.
- **Direct cloud and local providers** — usually delete the backing server and
  print `deleted lease=<id> server=<id> name=<name>`, but retain-capable
  providers such as `namespace-devbox`, `morph`, `kubevirt`, and `incus` stop
  or pause instead
  when their `*.deleteOnRelease` setting is `false` (some providers print a
  provider-specific release message instead, for example
  `stopped lease=<id> instance=<name> retained=true` for retained Incus
  instances). Hostinger is stop-only and prints `billing=still-owned`; it does
  not delete or cancel the subscription.
- **Delegated runners** — call the provider's own teardown for the resolved
  sandbox.

For `provider=docker-sandbox`, `crabbox stop` intentionally keeps Crabbox's
cross-provider cleanup meaning. Use [`ports`](ports.md) and [`cp`](cp.md) for
non-destructive post-create workflows on a running sandbox. The separate
[`pause`](pause.md) and [`resume`](resume.md) commands are provider-dependent
and are not supported by Docker Sandbox.

Where applicable, `stop` makes a best-effort attempt to stop GitHub
[Actions hydration](../features/actions-hydration.md) on the host before
releasing it. For SSH leases that can host mediated egress, it also best-effort
stops egress state: the local host daemon pid state and the lease-side egress
client are cleaned up before the provider release runs.

## Flags

`stop` accepts the shared provider-selection and target flags. The most common:

```text
--provider <name>          provider to act against (see crabbox providers)
--id <lease-or-slug>        lease ID or slug (equivalent to the positional arg)
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>        static SSH host (provider=ssh)
--static-user <user>        static SSH user (provider=ssh)
--static-port <port>        static SSH port (provider=ssh)
--static-work-root <path>   static target work root (provider=ssh)
```

Each provider also registers its own flags; the ones relevant to `stop` include:

```text
--namespace-delete-on-release            delete the Namespace Devbox instead of shutting it down
--exe-dev-control-host <host>            exe.dev SSH API host
--sprites-api-url <url>                  Sprites API URL
--e2b-api-url <url>                      E2B API URL
--e2b-domain <domain>                    E2B sandbox domain
--hostinger-url <url>                    Hostinger API URL
--hostinger-release-action stop          Hostinger release action; only stop is supported
--azure-dynamic-sessions-endpoint <url>  Azure Container Apps Dynamic Sessions endpoint
--blaxel-forget-missing                  remove a Blaxel claim after confirming the sandbox is already gone
--cloudflare-dynamic-workers-url <url>   Cloudflare Dynamic Workers loader URL
--cloudflare-sandbox-url <url>           Cloudflare Sandbox bridge URL
--cloudflare-sandbox-forget-missing      forget a local claim when the bridge reports the sandbox missing
```

Run `crabbox stop --help` for the full, provider-aware flag list, and
`crabbox providers` for the providers available in your build.

## See also

- [`cleanup`](cleanup.md) — sweep expired direct-provider machines and stale
  local state.
- [`ports`](ports.md) / [`cp`](cp.md) — non-destructive Docker Sandbox follow-up
  operations.
- [`pond release`](pond.md) — stop every lease in a named pond at once.
- [`admin`](admin.md) — coordinator-side `release` and `delete` for operators.
- [Lifecycle & cleanup](../features/lifecycle-cleanup.md) — how leases expire
  and get reclaimed.
