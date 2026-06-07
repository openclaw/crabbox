# cleanup

`crabbox cleanup` sweeps direct-provider machines and local provider state that
Crabbox created but no longer tracks. It is a safety net for direct (non-brokered)
mode only; brokered fleets manage expiry through the coordinator instead.

```sh
crabbox cleanup --dry-run
crabbox cleanup
crabbox cleanup --provider namespace-devbox --dry-run
crabbox cleanup --provider namespace-devbox
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

- **Direct cloud/VM providers** (for example `hetzner`, `aws`, `azure`, `gcp`,
  `proxmox`, `parallels`, `cloudflare`, `local-container`, `multipass`)
  enumerate the machines they own and decide, per machine, whether to delete it.
- **`namespace-devbox`** removes only Crabbox-owned local Namespace SSH files;
  it does not delete remote Devboxes.
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
```

`skip` lines include a `reason=` (for example `keep=true`, `state=running`,
`missing expires_at`, `not expired`). Without `--dry-run`, each `delete` line is
followed by the actual provider delete call; a failed delete returns the provider
error and stops the sweep.

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

## Flags

```text
--provider hetzner|aws|azure|gcp|proxmox|namespace-devbox|cloudflare|multipass
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
