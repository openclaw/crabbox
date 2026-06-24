# cleanup

`crabbox cleanup` sweeps direct-provider machines and local provider state that
Crabbox created but no longer tracks. It is a safety net for direct (non-brokered)
mode only; brokered fleets manage expiry through the coordinator instead.

```sh
crabbox cleanup --dry-run
crabbox cleanup
crabbox cleanup --provider namespace-devbox --dry-run
crabbox cleanup --provider namespace-devbox
crabbox cleanup --provider hostinger --dry-run
crabbox cleanup --provider coder --dry-run
```

`crabbox machine cleanup` is preserved as a compatibility alias and behaves
identically.

## Behavior

Cleanup refuses to run when a coordinator is configured:

```text
machine cleanup is disabled when a coordinator is configured; coordinator TTL alarms own brokered cleanup
```

Sweeping provider resources behind a coordinator can race live brokered leases,
so brokered expiry is owned entirely by the coordinator's TTL alarm. See
[Lifecycle cleanup](../features/lifecycle-cleanup.md).

What cleanup does depends on the selected provider:

- **Direct cloud/VM and delegated providers with cleanup** (for example
  `hetzner`, `aws`, `azure`, `gcp`, `proxmox`, `xcp-ng`, `hostinger`,
  `parallels`, `cloudflare`, `blaxel`, `local-container`, `multipass`)
  enumerate the machines they own and decide, per machine, whether to delete it.
- **`hostinger`** is stop-only: cleanup skips VPSs that are not positively
  identified as Crabbox-owned and stops matching VPSs; it does not delete VPSs
  or cancel Hostinger subscriptions.
- **`blaxel`** starts from local Blaxel claims scoped to the configured API URL
  and workspace, verifies remote ownership labels before deleting, and keeps
  missing claims unless `--blaxel-forget-missing` is explicitly set.
- **`namespace-devbox`** removes only Crabbox-owned local Namespace SSH files;
  it does not delete remote Devboxes.
- **`namespace-instance`** destroys only Namespace Compute instances carrying
  Crabbox ownership labels and removes claims for instances already gone.
- **`vercel-sandbox`** sweeps only local `vsbx_...` claims in the configured
  project/team/scope. It deletes idle-expired Crabbox-owned Vercel Sandboxes and
  keeps missing-or-inaccessible claims unless
  `--vercel-sandbox-forget-missing` is explicit.
- **`cloudflare-dynamic-workers`** checks local Dynamic Workers claims against
  the loader and removes only stale local claims whose loader metadata is
  missing or terminal. It does not enumerate or delete every Dynamic Worker in
  the Cloudflare account.
- **`cloudflare-sandbox`** sweeps only local `cfsbx_...` claims in the active
  provider scope. It deletes idle-expired Crabbox-owned sandboxes and keeps
  missing-or-inaccessible claims unless
  `--cloudflare-sandbox-forget-missing` is explicit.
- **`nomad`** sweeps only local `cbx_...` claims in the active Nomad namespace,
  region, and task scope. It deregisters idle-expired or TTL-expired
  Crabbox-owned jobs, removes stale local claims for missing jobs, and skips
  active claims.
- **`coder`** lists workspaces with Crabbox ownership evidence, such as the
  configured workspace prefix or Crabbox labels in Coder JSON, but mutates only
  workspaces that also have a local Crabbox claim with cleanup metadata. It
  uses the release action persisted in each local claim: new delete-on-release
  claims delete, stop-on-release claims stop, and older claims without that
  metadata default to stop.
- Providers that have nothing to sweep return an error rather than acting. For
  example `provider=ssh` (static / bring-your-own hosts) reports:

  ```text
  machine cleanup is not supported for provider=ssh
  ```

### Deletion decisions for direct machines

Selection is label-driven. Cleanup reads the `keep`, `state`, `expires_at`, and
`ttl` labels written when the machine was created. The decision is conservative:

- skip machines labeled `keep=true`;
- for `running` or `provisioning` machines, skip until well past expiry — delete
  only once the expiry time plus a 12-hour stale window has elapsed;
- for `leased`, `ready`, or `active` machines, delete once expired;
- always delete machines in `failed`, `released`, or `expired` states;
- for any other machine, delete only if `expires_at`/`ttl` parses and has passed;
  skip if the expiry label is missing or still in the future.

Resources without these labels are skipped (`reason=missing labels`), so
non-Crabbox machines are never touched.

### Namespace local cleanup

For `provider=namespace-devbox`, cleanup removes only Crabbox-owned Namespace SSH
snippets and keys under `~/.namespace/ssh/`:

```text
~/.namespace/ssh/crabbox-*.devbox.namespace.ssh
~/.namespace/ssh/crabbox-*.devbox.namespace.key
```

It does not remove non-Crabbox Namespace entries, and it does not touch the
`Include ~/.namespace/ssh/*.ssh` line in `~/.ssh/config`, because that include
may serve operator-owned Devboxes.

## Output

For direct machine providers, each candidate prints one decision line. `--dry-run`
prints the same lines but makes no provider calls:

```text
skip server id=12345 name=crabbox-blue-lobster reason=keep=true
delete server id=67890 name=crabbox-amber-crab
stop server id=11223 name=crabbox-green-heron
```

`skip` lines include a `reason=` (for example `keep=true`, `state=running`,
`missing expires_at`, `not expired`). Without `--dry-run`, each `delete` line is
followed by the actual provider delete call; a failed delete returns the provider
error and stops the sweep. Stop-only providers such as Hostinger print `stop`
instead and make the matching provider stop call.

Namespace local cleanup prints one line per file. `--dry-run` reports the
intended action instead of removing anything:

```text
namespace ssh cleanup would-delete /Users/alice/.namespace/ssh/crabbox-my-app.devbox.namespace.ssh
namespace ssh cleanup delete /Users/alice/.namespace/ssh/crabbox-my-app.devbox.namespace.key
```

When no matching files exist:

```text
namespace ssh cleanup no crabbox files found
```

Coder cleanup prints one line per cleanup-eligible claimed workspace:

```text
coder cleanup stop workspace=crabbox-blue dry_run=true
```

## Flags

```text
--provider hetzner|aws|azure|gcp|proxmox|xcp-ng|hostinger|namespace-devbox|namespace-instance|coder|cloudflare|cloudflare-dynamic-workers|cloudflare-sandbox|blaxel|multipass|vercel-sandbox
                                                                       provider to sweep (default from config)
--dry-run                                                              print decisions without making provider calls
```

Provider and target flags (for example `--target`, `--windows-mode`,
`--static-host`) are accepted for consistency with other commands but are not
used to scope the sweep.

## When to run

- after a CLI process crashed mid-warmup and left a server behind;
- when migrating from direct mode to brokered mode (sweep first, then switch);
- as a safety net after rotating provider credentials;
- never as part of a brokered workflow — the coordinator owns that path.

For brokered fleets, audit `crabbox admin leases --state active` and end leases
with `crabbox admin release` instead.

## Related docs

- [stop](stop.md)
- [admin](admin.md)
- [Lifecycle cleanup](../features/lifecycle-cleanup.md)
- [Orchestrator](../orchestrator.md)
- [Operations](../operations.md)
