# hold

`crabbox hold --provider azure --id <canonical-cbx-id> --json` retains an exact
failed Azure fixed lease for later salvage. It never attaches, snapshots,
retags, or deletes Azure resources.

```sh
crabbox hold --provider azure --id cbx_012345abcdef --json
```

The ordinary command requires the existing exact fixed claim and its account scope.
GETs must prove that the VM is absent. Remaining NIC, public IP, OS disk, and
quarantine NSG resources must have the matching lease, fixed attempt, and
create-intent tags, identifiable immutable IDs, and no foreign attachments.
Image-created managed OS disks can lack tags; an untagged disk must match the
original immutable disk identity in the claim's durable cleanup binding.
Failed reads and conflicting ownership refuse the hold without cloud mutation.

If the local claim was lost after allocation, supply the original fixed slug
from the allocation record:

```sh
crabbox hold --provider azure --id <lease-id> --slug <original-slug> --json
```

This path requires the original Azure account
configuration, proves the exact VM name absent before and after a complete VM
inventory, and records current companion identities without deleting anything.
It requires at least one retained companion. An untagged managed disk is only
an observed resource in that exact name slot; its current unique ID is not a
reconstructed original cleanup binding. A missing original slug, active VM,
foreign attachment, conflicting tags, incomplete inventory, or failed read
refuses the hold.

The `crabbox.lease-hold.v1` JSON receipt contains `provider`, `leaseId`,
`status: "held"`, and an ordered `resources` inventory. Each resource records
its kind, Azure resource ID, observed `absent` or `retained` state, and immutable
ID when available. `unacceptedChanges: "unknown"` means disk-only edits have not
been inspected or recovered. A receipt is a retention policy, not a declaration
that the machine or its files were safely released. A claimless receipt cannot
make `stop` succeed while companions remain.

The hold is published under the existing durable claim lock. Repeating the
command in the same account returns that recorded receipt, including after
restart; it does not make a new allocation or refresh presence observations.
The held-provider marker makes older Azure writers refuse the claim rather
than ignoring the additive receipt. Keep the claim directory in the same
durable backup as the controller's recovery state.

Held claims cannot be acquired, reused, stopped, force-stopped, or swept by
automatic cleanup. There is no automatic expiry or deletion of uncertain data.
The controller must retain bounded, accounted references to these resources;
a hold does not grant spare capacity. Salvage and any later resource removal
require a separate operator decision. This command does not clear a hold or
authorize deletion after salvage.
