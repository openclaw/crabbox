# Lifecycle Cleanup

Read when:

- changing release or expiry behavior;
- debugging leaked provider resources;
- changing direct-provider cleanup.

Brokered lifecycle is coordinator-owned:

```text
provisioning -> active -> released
provisioning -> failed
active -> expired
active -> failed
```

The CLI heartbeats active coordinator leases while a command runs. Heartbeat is a touch: it updates `lastTouchedAt` and extends idle expiry up to the lease TTL cap. Release and expiry both call the provider delete path; `keep=true` only skips command-exit release, not coordinator idle expiry.

The CLI also keeps a local claim file per lease so repo-local wrappers do not need their own ledger. Commands that reuse a lease validate that the current repo matches the claim; `--reclaim` moves the claim intentionally.

Brokered cleanup belongs to the Durable Object alarm. `crabbox cleanup` refuses to sweep provider resources when a coordinator is configured because that can race live brokered leases.

Direct-provider cleanup is conservative:

- skip `keep=true`;
- skip running/provisioning states until expiry plus the extra safety window;
- delete clearly expired ready/leased/active direct machines;
- delete clearly expired inactive machines.

Direct GCP leases also set a provider TTL delete and install a guest-side expiry
guard so expired ready/active VMs can self-delete if the local CLI disappears
before release.

Provider resources should carry Crabbox labels/tags so orphan cleanup can identify them without touching unrelated infrastructure.

Related docs:

- [stop command](../commands/stop.md)
- [cleanup command](../commands/cleanup.md)
- [status command](../commands/status.md)
- [inspect command](../commands/inspect.md)
- [Security](../security.md)
