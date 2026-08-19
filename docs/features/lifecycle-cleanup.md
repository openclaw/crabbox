# Lifecycle and Cleanup

Read this when:

- changing how leases are released or expired;
- debugging leaked provider resources (instances, NICs, public IPs, disks, Mac
  hosts);
- changing direct-provider cleanup behavior.

A lease holds a remote box until it is released or expires. Two independent
paths reclaim the underlying resources: the **brokered** path, owned by the
coordinator, and the **direct** path, owned by the local CLI (and, for GCP, a
guest-side guard). Which one applies depends on whether the provider runs
through a coordinator.

## Brokered lifecycle

When a provider is brokered (only `aws`, `azure`, `daytona`, `gcp`, and
`hetzner`, and only when a coordinator URL is configured), the coordinator owns
the lease record and its lifecycle. A brokered lease record moves through four states
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

- **Release** (`POST /v1/leases/{id}/release`, e.g. `crabbox stop`) records
  state `released` and queues provider deletion when the lease is still active.
  The body defaults `delete` to `!keep`; normal release does not synchronously
  await provider cleanup.
- **Expiry** is driven by the runtime scheduler. `expireLeases` deletes the
  cloud server for every active lease past `expiresAt`, then sets state
  `expired`.

`keep=true` only suppresses the automatic release when a `run` command exits; it
does **not** exempt a lease from idle or TTL expiry.

After one release mutation is accepted, an explicit CLI stop observes the lease
with read-only requests until provider deletion is final or a bounded wait ends.
The CLI removes its local per-lease SSH connection directory only after final
cleanup state is observed. Pending or retrying cleanup, observation timeout or
cancellation, provider errors, ownership mismatches, and retained resources keep
the local claim and credentials available for a safe retry. Acquisition rollback
and automatic post-run release only queue cleanup and do not wait for provider
deletion; they preserve local state while cleanup is pending. Local cleanup is
scoped to `<user-config>/crabbox/testboxes/<lease-id>` and never follows configured
or shared SSH key paths.

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

### Azure orphan sweep

The coordinator's Azure sweep uses a reconciliation-specific inventory of the
configured resource group across canonical per-lease VMs, NICs, public IPs, and
managed OS disks. The ordinary VM list/pool path remains VM-only. The sweep can
group a complete exact-owned NIC, public IP, and managed-disk set even when an
interrupted provisioning attempt left no VM.

An exact group must retain consistent Crabbox ownership tags, location,
canonical topology, and stable Azure resource identities. Mixed or incomplete
groups remain report-only. VMs with any data disk are also report-only because
VM deletion can cascade through data-disk delete options. VM managed-disk,
VM-to-NIC, and NIC-to-public-IP cascade-delete options are likewise rejected and
fingerprinted. A change to the member set, ownership labels, location, topology,
or stable identity changes the reconciliation fingerprint and resets the grace
quarantine. Ordinary release still supports a verified VM/NIC/public-IP set
whose OS disk is explicitly ephemeral (`diffDiskSettings.option=Local`); the
managed-disk orphan sweep keeps that non-managed shape report-only.

Delete mode releases only a group bound to an exact retained coordinator lease
after the same complete resource identity has survived the grace period and two
successful authoritative inventories. The normal owned-resource delete path
performs a fresh preflight before each deletion. Shared VNets, subnets, NSGs,
and resource groups are not part of the inventory or deletion path. With all
four Azure broker credentials configured, the sweep is enabled unless
`CRABBOX_AZURE_ORPHAN_SWEEP_ENABLED=0`; set
`CRABBOX_AZURE_ORPHAN_SWEEP_DELETE=1` to allow deletion. Its interval and grace
default to 3600 and 900 seconds and are controlled by
`CRABBOX_AZURE_ORPHAN_SWEEP_INTERVAL_SECONDS` and
`CRABBOX_AZURE_ORPHAN_SWEEP_GRACE_SECONDS`.

Durable owned-delete claims persist each successful member deletion with that
member's stable resource identity before cleanup advances. A missing member is
accepted on retry only when the same claim contains that exact ordered progress;
an external disappearance or failed progress write stops the remaining cleanup.
Progress updates transactionally merge the longest verified prefix so concurrent
cleanup attempts cannot overwrite newer evidence. Fresh preflight also compares
the quarantined ownership labels, including `keep` and expiry, on every survivor.
New cleanup starts with a transactional version-2 preparing reservation, then
binds the complete inspected identity before the first deletion. Every later
claim transition and completed-claim clear is transactional, so a delayed
preparation, replacement, or empty-set observer cannot overwrite or erase newer
progress. Older workers reject version-2 claims rather than replaying them with
version-1 semantics. Legacy version-1 claims still
replay through the same scope and topology checks, but a
legacy partial claim with an unexplained missing member fails closed because it
cannot prove which cleanup deleted that member. A legacy claim can establish a
new stable baseline only while the VM, NIC, public IP, and managed disk are all
still present; an already-empty legacy claim can be cleared without mutation.

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

Providers may durably publish an exact-resource claim with the generic
`state=provisioning` label before post-create readiness completes. Such a claim
is recovery authority, not proof that the lease is ready: inventory and status
must keep it non-ready until a fenced claim update records `state=ready`.
Provider adapters own the immutable resource identity and routing scope needed
to inspect or delete that pending resource. Cleanup must compare the unchanged
claim under its lifecycle fence before mutation so an old readiness or cleanup
attempt cannot overwrite or delete a newer claim.

## Related docs

- [stop command](../commands/stop.md)
- [cleanup command](../commands/cleanup.md)
- [status command](../commands/status.md)
- [inspect command](../commands/inspect.md)
- [Identifiers](identifiers.md)
- [Security](../security.md)
