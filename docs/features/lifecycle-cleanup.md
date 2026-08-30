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
the lease record and its lifecycle. The coordinator persists a `provisioning`
reservation before calling the provider (`worker/src/types.ts`):

```text
provisioning -> active    (readiness completed)
provisioning -> failed    (creation or readiness failed)
active       -> released  (explicit release)
active       -> expired   (TTL or idle cleanup completed)
```

Release can also end a provisioning lease. It records user intent; it does not
cancel an already-dispatched provider request. Failed and released records can
still carry unresolved cleanup responsibility. Their local state alone is not
proof that the provider resource was deleted.

For an exact Azure lease whose provisioning stops before VM creation, ordinary
owned-resource release can clean the observed creation prefix: an unattached
canonical public IP alone, or the exact canonical public IP and NIC together.
A fresh NIC without its public IP is not a valid creation prefix and remains
report-only; cleanup can resume past a missing public IP only when its exact
durable claim already proves Crabbox deleted that public IP. A managed disk
without its complete NIC/public-IP set, a managed-disk VM without that disk,
foreign attachments, replacements, and missing immutable identities also fail
closed. This narrow exact-lease release path does not relax the separate Azure
orphan sweep's complete-set and quarantine requirements.

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

### Brokered native checkpoint retention

New brokered native AWS, Azure, and GCP checkpoints have their own durable
lifecycle, independent of the source lease. Retention is manual unless the
owner explicitly requests `checkpoint create --expire-unused-after <duration>`
or changes `checkpoint policy`. Coordinator alarms read a bounded, sorted due
index and expire only those explicitly opted-in records; generic provider
inventory, old image records, local files, and checkpoint-like names never
grant cleanup ownership.

Active renewable fork/shard use claims and AWS/Azure promotion pins prevent
deletion. Manual and automatic deletion share the same generation-fenced
provider cleanup: AWS confirms the exact AMI and every owned EBS snapshot are
gone, Azure confirms its exact canonical managed snapshot, and GCP confirms
its exact project-scoped machine image or disk snapshot. Ambiguous or failed
provider calls retain ownership and retry with capped backoff; a durable
provider-deleted phase makes final metadata cleanup restart-safe. Checkpoint
admission charges creating, ready, failed, and deletion-pending records until
deletion is confirmed; deleted audit tombstones no longer consume capacity.
Expired available fork claims can be replaced, but provisioning claims remain
charged until exact lifecycle reconciliation. Each checkpoint retains only its
256 most recent ordered audit events, and eventual tombstone pruning removes
that entire retained suffix. Direct, archive, recipe, and historical checkpoints
remain operator-managed.
### Managed Daytona cleanup

New brokered Daytona sandboxes receive native wall-clock TTL in the original
allocation request: the requested lease TTL rounded up to whole minutes, with a
one-minute minimum. This clock starts at provider creation, not coordinator
reservation, and applies even when the sandbox is kept or explicitly retained.
Heartbeats do not extend it. A TTL-capable Daytona API is required; Crabbox never
retries allocation without TTL. Before publishing access, a ready sandbox must
report a future native destruction deadline no later than one requested TTL from
the start of readiness observation. This bound does not move during polling or
assume that the provider's creation and TTL timestamps share an exact clock tick.
An API that silently ignores TTL is not accepted as ready. Organization, region,
and sandbox-class lifespan limits still apply. This is a provider lifetime
contract, not a teardown-latency SLA during provider failures.

Before dispatch, the lease stores a nonsecret fingerprint of the normalized API
URL, configured organization, and credential context. A returned sandbox UUID is
recorded before readiness waits. Cleanup uses that exact UUID and original
context, verifies current ownership before mutation, and observes a terminal
provider state or exact-resource absence before reporting completion. Accepted
DELETE requests alone are not completion.

If no authoritative UUID arrives, cleanup remains explicitly unresolved. The
coordinator preserves its original dispatch evidence, `failureError`, and
`cleanupError`; it neither adopts name/label matches nor declares deletion after
an empty inventory read or elapsed TTL. It does not repeatedly schedule a lookup
that cannot establish identity. Inspect these records with `crabbox admin
lease-audit` and resolve the resource in its original provider account. A release
acknowledgment does not clear this responsibility.

Missing historical scope or a changed API URL, organization, or API key blocks
automatic cleanup and access refresh rather than adopting the current context.
This deliberately includes same-organization key rotation: the fingerprint
proves identical credential context, not upstream account continuity. Finish
known-ID cleanup before rotating credentials, or restore the original context
and repeat explicit stop. Legacy records without scope require operator
resolution; never backfill them from current configuration.

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
