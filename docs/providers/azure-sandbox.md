# Azure Container Apps Sandbox provider

`azure-sandbox` runs Linux shell commands through ACA Sandbox APIs in an
existing Sandbox group. It is separate from Azure VMs (`azure`) and Dynamic
Sessions (`azure-dynamic-sessions`). Infrastructure owns the group's identities,
egress policy and data-plane role assignments; Crabbox does not create or change
them.

Configure the trusted caller environment:

```sh
export CRABBOX_AZURE_SANDBOX_SUBSCRIPTION_ID='<subscription>'
export CRABBOX_AZURE_SANDBOX_RESOURCE_GROUP='<resource-group>'
export CRABBOX_AZURE_SANDBOX_GROUP='<sandbox-group>'
export CRABBOX_AZURE_SANDBOX_REGION='westus3'
export CRABBOX_AZURE_SANDBOX_CLIENT_ID='<allocator-managed-identity-client-id>'
```

With a client ID, the Azure SDK selects that managed identity. Without one, it
uses the local Azure CLI credential. Tokens use the `https://dynamicsessions.io`
audience; the credential owns caching and renewal. No bearer enters a lease or
command argument. The regional data-plane host is derived from the region.

```sh
crabbox warmup --provider azure-sandbox --lease-id cbx_012345abcdef \
  --class small --ttl 4h --idle-timeout 15m --keep
crabbox inspect --provider azure-sandbox --id cbx_012345abcdef --json
crabbox cp --provider azure-sandbox --id cbx_012345abcdef ./repository.pack SANDBOX:/workspace/repository.pack
printf 'printf "hello\n"\n' | crabbox run --provider azure-sandbox \
  --id cbx_012345abcdef --keep --no-sync --script-stdin
crabbox heartbeat --provider azure-sandbox --id cbx_012345abcdef --json
crabbox stop --provider azure-sandbox --id cbx_012345abcdef
```

The initial shape is 2 CPU / 4 GiB, using the public `ubuntu` disk by default.
The common class catalog maps every generic class to this one shape; larger
class names do not provision larger machines. `--azure-sandbox-disk` selects a
public disk name. `--azure-sandbox-disk-id` (or
`CRABBOX_AZURE_SANDBOX_DISK_ID`) instead selects a prepared private disk image
in the configured group. Its immutable ID is bound into the allocation intent;
changing the image cannot change an existing lease on replay. Image production
and retirement remain the image owner's responsibility. Each allocation gets
fresh lease labels and lifecycle settings. Optional prepared snapshot restores
are described below; disk images remain the default.
SSH, desktop, archive synchronization and snapshot forks are
not advertised. The caller owns workspace preparation, using kept leases and
`--no-sync`; shell scripts and explicitly allowlisted environment profiles reuse
the normal CLI input path. `cp` uploads regular local files through the native
file API under the same repository claim fence; directory copies and downloads
are not advertised by this initial adapter.

Fixed leases use the common durable journal. Sandbox IDs are server-generated;
immutable labels bind a resource to the recorded attempt. A lost create response
is reconciled by that binding, never by issuing another create after an empty
list. A known pre-submission attempt may continue. Ambiguous observations retain
the claim and require reconciliation; malformed inventory responses never count
as empty inventory. Stop can interrupt an active command,
upload or heartbeat: it cancels the local request wait, then checks and deletes
the exact owned resource. It waits for deletion readback before publishing the
common terminal receipt; cancellation alone is not proof of remote termination.

The original TTL bounds execution admission and heartbeats. Platform auto-suspend
uses the idle interval. Within that original TTL, warmup replay, commands, uploads
and heartbeats resume an owned idle Sandbox and recheck ownership before use.
Status and inspect expose that original deadline as `expiresAt`. Once it passes,
an exactly observed owned allocation reports `state=expired` and `ready=false`,
even if the physical Sandbox is still running. Its resource ID and ownership
labels remain available for `stop`; expiry does not prove resource deletion.
Missing or invalid allocation deadlines return a status error instead of readiness.
Administratively disabled Sandboxes are never enabled automatically. Auto-delete
runs after the Sandbox stops. Those platform
policies are not an absolute wall-clock deletion guarantee. Transport failure or
cancellation cannot prove the remote command exited: the lease remains owned
until resource cleanup settles. Live acceptance must qualify deletion, idle
behavior, token renewal and Gateway recovery before production activation.

## Prepared snapshot restore (opt-in)

Select an exact group-scoped snapshot with `--azure-sandbox-snapshot-id`, its
trusted binding file with `--azure-sandbox-snapshot-receipt`, and the SHA256 of
that file with `--azure-sandbox-snapshot-generation`. The corresponding
environment variables are `CRABBOX_AZURE_SANDBOX_SNAPSHOT_ID`,
`CRABBOX_AZURE_SANDBOX_SNAPSHOT_RECEIPT`, and
`CRABBOX_AZURE_SANDBOX_SNAPSHOT_GENERATION`. These settings cannot come from a
repository configuration. Keep `--azure-sandbox-disk-id` set to the exact source
disk. Omitting snapshot selection keeps normal disk provisioning; an invalid
selected snapshot fails closed rather than silently falling back.

The infrastructure owner creates and qualifies the snapshot **before** identity
enrollment, pairing, credential injection or repository preparation. A snapshot
captures filesystem state, so omitting environment and connections from a
restore request alone does not prove that captured state is clean. Crabbox
consumes a trusted verifier receipt; it does not perform a second image audit
or cryptographically attest the remote filesystem.

The binding JSON has schema `crabbox.azure-sandbox-snapshot.v1` and these fields:

| Field | Required meaning |
| --- | --- |
| `providerScope` | Exact provider claim scope: `azure-sandbox:<region>:<subscription>:<resource-group>:<group>` |
| `snapshotId`, `sourceSandboxId`, `sourceDiskId` | Exact platform capture and source identities |
| `capturedAt`, `validUntil` | Capture UTC timestamp matching platform metadata, and qualification expiry (RFC3339) |
| `verifierFile`, `verifierSha256` | Existing verifier JSON file and SHA256; relative paths resolve beside the binding |
| `preparedRuntime` | Exact runtime identity matching the verifier, described below |
| `lifecycle` | `autoSuspendPolicy` with `enabled=true`, requested idle `interval` in seconds, `mode=Disk`; `autoDeletePolicy` with `enabled=true` and requested TTL `deleteIntervalInSeconds` |

Both files are limited to 1 MiB. The verifier must contain `preEnrollment` with
schema `crabbox.pre-enrollment-state.v1` and all four true fields:
`credentialEnvironmentAbsent`, `credentialAndSessionPathsAbsent`,
`workerStateLimitedToPreparedCaches`, and `workspaceEmpty`. Its top-level
`checkedCredentialPathsAbsent` must also be true. The image owner's verifier
defines and audits the credential/session paths and permitted caches. Adapting
an existing verifier to this generic schema must retain its actual audit
results; renaming a failed receipt does not qualify it.

`preparedRuntime` contains an immutable `runtimeImage` ending in
`@sha256:<64 hex digits>`, nonempty `buildId` and `runtimeVersion`, an
`enabledPluginIds` array (empty is allowed), SHA256 fields `nodeBootstrapSha256`,
`workerBundleSha256` and `bundleHash`, and `cacheHitVerified=true`. The binding
and verifier must agree exactly, including plugin order. The configuration pin
binds this build/plugin generation to the fixed allocation intent.

Before submission, Crabbox reads the exact snapshot metadata and requires its
source Sandbox and capture time to match the binding, then checks the source
disk is Ready. Restore sends only the snapshot reference and fresh immutable
lease/attempt labels. Readback must show both the selected snapshot and source
disk. Since restored lifecycle defaults may differ, Crabbox sets and verifies
the requested policy on the exact owned clone. An uncertain lifecycle update
is reconciled by readback and never automatically posted again. Cleanup uses
persisted ownership and source references without requiring receipt files;
it deletes only the clone, never the source disk or snapshot.

The documented CLI/server accepts fresh labels even though Python SDK 0.1.0b4
rejects that combination locally. See the official
[snapshot](https://sandboxes.azure.com/docs/sandboxes/snapshots/),
[volume](https://sandboxes.azure.com/docs/sandboxes/volumes/), and
[lifecycle](https://sandboxes.azure.com/docs/sandboxes/sandbox/lifecycle/)
references. Snapshot custody and source-disk retention belong to the image owner.
This option does not add shared volumes or a reservation pool.

Compare disk versus snapshot using the same build and fresh attempts. Record
allocation submission-to-Running separately from runtime preparation,
enrollment, repository preparation and accepted-input-to-usable time. A Running
snapshot is not proof of a usable worker, and neither this option nor the
platform contract promises a ten-second startup. Clean end-to-end live
qualification remains required before selecting snapshots in production.

## Session disk retention (opt-in)

A native session lifecycle owner can select `--azure-sandbox-retention session-disk`
and `--azure-sandbox-owner-binding <64 lowercase hex digest>` at kept, fixed-ID
warmup. These are trusted user/environment/flag settings, not repository inputs.
The digest binds one session/environment without storing session contents.
`--ttl` remains finite and positive: it is the maximum authorized work window for
each activation. The exact owned disk remains until explicit release. Disk idle
suspend remains enabled and auto-delete is explicitly disabled, with independent
readback required before work. Existing ephemeral claims keep their original
finite allocation semantics; they cannot be converted into retained claims.

The existing placement owner must settle admitted work and persist its lifecycle
operation before calling `pause`. It must retain the exact lease, private runtime
home and conversation binding through restart. Crabbox preserves the disk; it
does not own session admission, native thread recovery or credential enrollment.
There is no RAM/process continuation promise or private snapshot fallback.

`inspect --json` and `status --json` add a `retention` receipt with schema
`crabbox.session-disk.v1`. It includes immutable `mode`, `ownerBinding`,
`computeWindowSeconds`; `identity` (`leaseId`, `attemptLeaseId`, `slug`,
`resourceId`); opaque `claimRevision`; `desiredState`; `computeWindow`
(`id`, `authorizedAt`, `expiresAt`, `state`); optional `operation`
(`id`, `action`, `phase`, `expectedClaimRevision`); `policyVerified`,
`observedState` and `stopPending`. Timestamps are UTC RFC3339. The top-level
`expiresAt` is the work window deadline, never a disk deletion deadline.
An unbound acquisition has no resource ID and grants no lifecycle authority.

Both `pause` and `resume` require the canonical `--id`, `--operation-id`,
`--owner-binding`, `--expected-claim-revision`, and the complete existing identity
flag set: `--expected-provider-lease-id`, `--expected-provider-attempt-lease-id`,
`--expected-provider-slug`, `--expected-provider-resource-id`. Values come from
the persisted receipt. `--json` returns an independent status receipt on success.
On failure, inspect the same lease; an unknown response does not authorize a
replacement. A fresh exact readback of a stable opposite state permits bounded
reassertion of that same desired-state operation on the same resource/window;
transitional, unknown or mismatched state does not. An expired pending resume
closes its window and settles Disk suspension instead of resuming.
Replaying the current operation uses its original expected revision;
a stale operation cannot regain authority after a successor.

Pause durably closes the current window before stopping compute. Resume requires
a confirmed suspended resource and a closed window, then records one new finite
window before the provider effect. Its ID is the operation ID; retrying never
moves its deadline. The original acquisition window ID is the canonical lease
ID. A natural provider idle stop must be closed through pause before opening the
next activation. Run, copy, heartbeat and observation cannot open a window or
implicitly resume a retained disk. Expiry refuses work and exposes pending stop
until the native lifecycle owner reconciles it; Gateway outages can delay actual
stopping. This is finite authorized work plus provider idle suspension, not a
Gateway-independent billed-compute limit.

Creation is also bounded by the original work window: an expired preparation
or retry cannot allocate a new retained disk. Replaying a completed resume is a
read-only exact observation and does not cancel or wait for admitted activity.
`status --wait` exits nonzero for suspended, compute-expired or deleted retained
leases rather than waiting for readiness that requires an explicit lifecycle action.

States are `running`, `suspended`, `transitioning`, `compute-expired`, `missing`,
`unknown`, and terminal `deleted`. Missing, uncertain policy or mismatched
ownership must not cause replacement or a fresh native conversation. Existing
provider quotas remain authoritative. List marks session disks separately with
`retention=session-disk retained_disk=true`; this is retained resource inventory,
not a storage-byte or price estimate, and these resources are not clean reserves.

`stop`/`release` remain destructive: they persist retirement, delete the exact
owned resource and require independent missing readback. Cleanup remains available
with expired windows or new retention disabled. Retained terminal receipts keep
exact identity for retry. The claim journal uses version 2 so previous readers
refuse the state before provider effects. Rollback requires a compatible cleanup
reader and the native placement version fence; disabling opt-in never deletes
existing retained data. The legacy Crabbox workspace controller refuses retained
warmup rather than routing window expiry into destructive cleanup.

Prepared clean snapshot initialization remains a separate image-owner contract.
Retention does not relax its clean pre-enrollment attestation or capture session
state into the clean catalog. A retained clone receives the no-auto-delete policy
before session adoption; shared source images and snapshots are never deleted.
