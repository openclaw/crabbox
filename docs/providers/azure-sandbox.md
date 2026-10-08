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
fresh lease labels and lifecycle settings. Full-state snapshot restore is not
used: that API replays captured settings and does not accept fresh lease labels.
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
Administratively disabled Sandboxes are never enabled automatically. Auto-delete
runs after the Sandbox stops. Those platform
policies are not an absolute wall-clock deletion guarantee. Transport failure or
cancellation cannot prove the remote command exited: the lease remains owned
until resource cleanup settles. Live acceptance must qualify deletion, idle
behavior, token renewal and Gateway recovery before production activation.
