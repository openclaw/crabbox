# Lifecycle and Cleanup

Read this when:

- changing how leases are released or expired;
- debugging leaked provider resources (instances, disks, Mac hosts);
- changing direct-provider cleanup behavior.

A lease holds a remote box until it is released or expires. Two independent
paths reclaim the underlying resources: the **brokered** path, owned by the
coordinator, and the **direct** path, owned by the local CLI (and, for GCP, a
guest-side guard). Which one applies depends on whether the provider runs
through a coordinator.

## Brokered lifecycle

When a provider is brokered (only `aws`, `azure`, `gcp`, and `hetzner`, and only
when a coordinator URL is configured), the coordinator owns the lease record
and its lifecycle. A brokered lease record moves through four states
(`worker/src/types.ts`):

```text
active -> released   (explicit release)
active -> expired    (TTL or idle expiry reclaimed the box)
active -> failed     (provisioning or cleanup failure)
```

A lease is created `active`. There is no separate `provisioning` state in the
brokered record; provisioning happens inside lease creation and the record only
persists once the box exists.

### Heartbeats and expiry

While a command runs, the CLI heartbeats the active lease (`POST
/v1/leases/{id}/heartbeat`). A heartbeat is a touch: it bumps `lastTouchedAt`,
recomputes `expiresAt`, clears stale cleanup metadata, and refreshes provider SSH
access where the provider supports it. Heartbeats at or after `expiresAt` are
rejected so they cannot revive a lease once expiry cleanup owns it.

Expiry is the minimum of two clocks (`leaseExpiresAt` in `worker/src/fleet.ts`):

- **idle expiry** — `lastTouchedAt + idleTimeout` (default idle timeout 1800s);
- **max lifetime** — `createdAt + ttl` (default TTL 5400s, capped at 86400s).

A heartbeat can only push idle expiry forward up to the max-lifetime cap, so a
busy lease still expires at its TTL regardless of activity.

### Release vs expiry

Both release and expiry call the same provider delete path:

- **Release** (`POST /v1/leases/{id}/release`, e.g. `crabbox stop`) deletes the
  cloud server when the lease is still active and sets state `released`. The
  body defaults `delete` to `!keep`.
- **Expiry** is driven by the runtime scheduler. `expireLeases` deletes the
  cloud server for every active lease past `expiresAt`, then sets state
  `expired`.

`keep=true` only suppresses the automatic release when a `run` command exits; it
does **not** exempt a lease from idle or TTL expiry.

### Cleanup retries

If deleting the cloud server during expiry fails, the lease stays `active` and
the coordinator records `cleanupAttempts`, `cleanupError`, `cleanupFailedAt`, and a
`cleanupRetryAt` set 5 minutes out (`leaseCleanupRetryDelayMs`). The next alarm
is scheduled for the soonest of all active-lease expiry/retry times, so a failed
delete is retried automatically. On success the cleanup metadata is cleared and
the state becomes `expired`. You can inspect stuck cleanups with `crabbox admin
lease-audit`.

### AWS orphan sweep

Independent of per-lease expiry, the Worker can report AWS resources that no
longer map to an active lease. Delete mode terminates instances or releases idle
Mac dedicated hosts only when retained coordinator state binds the exact
resource; tag-only and legacy candidates stay report-only. It runs from the same
alarm/cron, gated by `CRABBOX_AWS_ORPHAN_SWEEP_*` environment variables.

## Direct-provider lifecycle

Without a coordinator, the CLI talks to the provider API directly and owns
cleanup itself. Releasing a direct lease (`crabbox stop` / `crabbox release`)
deletes the backing machine immediately.

`crabbox cleanup` (alias `crabbox machine cleanup`) sweeps expired
direct-provider machines and stale local state. It refuses to run when a
coordinator is configured, because sweeping provider resources can race live
brokered leases:

```bash
crabbox cleanup --provider hetzner --dry-run
crabbox cleanup --provider hetzner
```

Use `--dry-run` to print what would be deleted without touching anything. The
sweep is conservative; for each candidate machine `shouldCleanupServer`
(`internal/cli/pool.go`) decides from the machine's Crabbox labels:

- skip machines with no labels, or labeled `keep=true`;
- `running` / `provisioning`: delete only when stale — past `expires_at` plus a
  12-hour safety window;
- `leased` / `ready` / `active`: delete once past `expires_at`;
- `failed` / `released` / `expired`: delete;
- otherwise: delete once past `expires_at`, skip if `expires_at` is missing or
  still in the future.

For this to work, every direct-provider machine must carry Crabbox labels/tags
(at least `crabbox`, `state`, and `expires_at`) so the sweep can identify owned
resources without touching unrelated infrastructure.

### GCP guest-side expiry guard

A direct GCP lease can outlive the local CLI that created it — if `cleanup`
never runs, the VM would leak. To guard against this, direct GCP leases install
a self-deleting guard (`cloudInitGCPExpiryGuardFiles` in
`internal/cli/bootstrap.go`): a systemd timer runs every 2 minutes, reads the
instance's own labels via the GCP metadata server, and deletes the instance when
it is clearly expired. It applies the same conservative logic as the CLI sweep:

- exits unless `crabbox=true` and `keep != true`;
- `failed` / `released` / `expired`: delete;
- `running` / `provisioning`: delete only past `expires_at` plus 12 hours;
- `leased` / `ready` / `active` (and unlabeled state): delete once past
  `expires_at`.

So an expired GCP box can reclaim itself even if the operator's machine is gone.

## Claims and `--reclaim`

Independent of provider cleanup, the CLI keeps a local **claim** file per lease
so repo-local wrappers do not need their own ledger. Commands that reuse a lease
validate that the current repo matches the claim; deleting a lease removes its
claim. Move a claim to a different repo deliberately with `--reclaim`. See
[Identifiers](identifiers.md) for the claim file format and location.

## Related docs

- [stop command](../commands/stop.md)
- [cleanup command](../commands/cleanup.md)
- [status command](../commands/status.md)
- [inspect command](../commands/inspect.md)
- [Identifiers](identifiers.md)
- [Security](../security.md)
