# cleanup

`crabbox cleanup` sweeps direct-provider leftovers based on Crabbox labels.

```sh
crabbox cleanup --dry-run
crabbox cleanup
```

`crabbox machine cleanup` is preserved as a compatibility alias.

## Behavior

Cleanup refuses to run when a coordinator is configured. Brokered cleanup
belongs to the Durable Object alarm; sweeping provider resources behind the
coordinator can race live brokered leases.

In direct-provider mode, cleanup is intentionally conservative:

- skip machines tagged `keep=true`;
- skip machines in `running` or `provisioning` state until the extra stale
  safety window passes (expiry plus 12 hours);
- delete machines that are clearly expired in `ready`, `leased`, or
  `active` states;
- delete machines that have been inactive past expiry.

Selection is label-driven. Cleanup uses `lease`, `slug`, `expires_at`,
`last_touched_at`, `state`, and `keep` labels written when the machine was
created. Resources without Crabbox labels are never touched.

Static SSH targets are existing operator-owned hosts, so `provider=ssh`
has nothing to sweep. Cleanup exits early for that provider.

## Output

`--dry-run` lists every decision without taking action:

```text
hetzner cx53 hz-12345 lease=cbx_abcdef123456 slug=blue-lobster keep=true skip=keep
hetzner cx53 hz-67890 lease=cbx_abcdef234567 slug=amber-crab    expires_at=2026-05-01T17:30:00Z delete
```

Without `--dry-run`, the same lines print but each `delete` is followed by
`deleted` after the provider call returns. Failures print the provider
error and continue with the next candidate.

## Flags

```text
--provider hetzner|aws|azure  provider to sweep (delegated providers do not need cleanup)
--target linux|macos|windows  for AWS, restrict by target
--windows-mode normal|wsl2    when target=windows
--static-host <host>          ignored (provider=ssh has nothing to sweep)
--static-user <user>          ignored
--static-port <port>          ignored
--static-work-root <path>     ignored
--dry-run                     log decisions without making provider calls
```

## When To Run

- after a CLI process crashed mid-warmup and left a server behind;
- when migrating from direct mode to brokered mode (sweep first, then
  switch);
- as a safety net after rotating provider credentials;
- never as part of a brokered workflow - the coordinator owns that path.

For brokered fleets, audit `crabbox admin leases --state active` and use
`crabbox admin release` instead.

Related docs:

- [stop](stop.md)
- [admin](admin.md)
- [Lifecycle cleanup](../features/lifecycle-cleanup.md)
- [Orchestrator](../orchestrator.md)
- [Operations](../operations.md)
