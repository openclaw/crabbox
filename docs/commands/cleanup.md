# cleanup

`crabbox cleanup` sweeps direct-provider leftovers.

```sh
crabbox cleanup --dry-run
crabbox cleanup
```

Cleanup refuses to run when a coordinator is configured. Brokered cleanup belongs to the Durable Object alarm.

Direct cleanup skips kept machines, deletes expired ready/leased/active machines, and gives running/provisioning machines an extra stale safety window. It relies on provider labels such as `lease`, `slug`, `expires_at`, and `state`.

Static SSH targets are existing hosts, so `provider=ssh` has nothing to sweep.

Flags:

```text
--provider hetzner|aws
--target linux|macos|windows
--windows-mode normal|wsl2
--static-host <host>
--static-user <user>
--static-port <port>
--static-work-root <path>
--dry-run
```

`crabbox machine cleanup` remains as a compatibility alias.
