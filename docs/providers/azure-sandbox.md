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
