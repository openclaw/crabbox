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

A failed attempt that never bound a VM can also be held using its existing
claim. Inspection uses the original attempt name and tags, checks any recorded
pre-VM network identities, and inventories VMs before publishing the receipt.
Retained disks are observations, not newly reconstructed cleanup bindings; the
original attempt remains intact and no VM identity is invented.

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
require a separate operator decision. Creating a hold does not authorize
deletion after salvage.

## Finalizing after salvage and external disposal

First salvage any needed files using a separately authorized recovery workflow.
Then, only after an explicit operator decision, dispose of the retained resources
externally in Azure. To close the recorded hold after disposal:

```sh
crabbox hold --provider azure --id cbx_012345abcdef --finalize --json
```

`--finalize` requires an existing exact held claim and the original Azure
subscription/resource-group configuration. It uses the stored original slug;
`--slug` cannot be combined with `--finalize`. Bound, unbound, and claimless-origin
holds use the same finalization path. An unknown ID or missing/invalid claim
cannot be finalized, and unsupported providers refuse the operation. Do not
manually edit or remove the claim to bypass a hold.

Under the durable claim lock, read-only verification must prove that **all five
resource slots** are absent: VM, NIC, public IP, managed OS disk, and quarantine
NSG. Any surviving resource (including an untagged disk), wrong account scope,
failed or ambiguous read, invalid receipt, cancellation before publication, or
failed persistence refuses finalization and preserves the protective hold.
Finalization never attaches, snapshots, retags, or deletes cloud resources.

Success durably records a `crabbox.lease-hold.v1` receipt with
`status: "finalized"`, the ordered resources all `absent`, and
`unacceptedChanges: "unknown"`. Absence is **not** proof that files were recovered
or accepted. Keep separate salvage evidence. The claim and held-provider marker
remain as a single-use tombstone: finalization does not free this ID for reuse,
make `stop` delete anything, or allow older writers to reinterpret the claim.

Repeating `--finalize`, including from a fresh process, returns the identical
terminal receipt in the original account without recreating the hold or
rechecking Azure. Ordinary `hold` also returns that terminal receipt. These are
point-in-time absence observations, not a cloud-side lock on external writers.
If a resource is recreated later, inspect and dispose of it separately; replay
does not certify its current absence. No hold expires automatically.
