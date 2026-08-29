# checkpoint

Save the state of a lease, then restore it onto another box or fork it into a
fresh lease later. A checkpoint turns expensive one-time setup — installed
dependencies, warmed caches, generated fixtures, a paused bug — into something
you can reproduce on demand without repeating the work.

Each checkpoint has an ID of the form `chk_<hex>` and is recorded locally under
the Crabbox state directory (`$XDG_STATE_HOME/crabbox/checkpoints`, or
`<user-config-dir>/crabbox/state/checkpoints` when `XDG_STATE_HOME` is unset).

Subcommands: `create`, `list`, `inspect`, `restore`, `fork`, `delete`, `prune`.

## Two checkpoint kinds

**Native (provider snapshot or image)** — captures the whole machine: packages,
tools, caches, services. Stored in the provider account, so it incurs provider
storage costs. Recorded as one of `aws-ami`, `aws-ebs-snapshot`,
`azure-managed-image`, `azure-os-disk-snapshot`, `gcp-machine-image`,
`gcp-disk-snapshot`, `hetzner-snapshot`, `machine0-image`,
`parallels-snapshot`, or `daytona-snapshot`.

**Archive (workspace tarball)** — captures only the contents of the remote
workdir as `workspace.tar.gz`. Portable across any POSIX SSH lease, but it does
not preserve machine state.

`--mode auto` (the default) picks native for providers that support it on the
current lease and falls back to an archive otherwise. There is also a
metadata-only `recipe` kind produced by `--recipe-only`, which records the
checkpoint without creating any artifact.

> Both kinds may contain secrets. Native checkpoints capture the full root
> volume (caches, logs, credentials); archives capture build outputs and
> generated files. Delete checkpoints when you no longer need them.

## Quick start

```sh
# Warm a lease, do the expensive setup, then snapshot it
crabbox warmup --provider aws --class beast
crabbox run --id swift-crab --shell 'npm ci && npm test'
crabbox checkpoint create --id swift-crab --name after-npm-ci
# checkpoint created id=chk_abc123 kind=aws-ebs-snapshot resource=snap-... state=available region=eu-west-1 workdir=...

# Fork the checkpoint into a brand new lease
crabbox checkpoint fork chk_abc123 --class beast
# checkpoint forked id=chk_abc123 lease=cbx_... slug=purple-whale image=snap-... workdir=...

# Run against the forked lease
crabbox run --id purple-whale -- npm test
```

Checkpoints are explicit: you fork a specific ID. To change the *default* base
image used by all future leases instead, use
[`crabbox image promote`](image.md).

## create

Create a checkpoint from an existing lease.

```sh
# Auto mode (native where supported, archive otherwise)
crabbox checkpoint create --id swift-crab --name after-install

# Force a native checkpoint (fails if the lease does not support one)
crabbox checkpoint create --id swift-crab --mode native --wait

# Force a portable workspace archive
crabbox checkpoint create --id swift-crab --mode archive

# Prefer a full image over a disk snapshot
crabbox checkpoint create --id swift-crab --strategy image

# Direct AWS lease (no coordinator): force a native AMI
crabbox checkpoint create --provider aws --id swift-crab --mode native

# Direct Hetzner lease: create a project snapshot
crabbox checkpoint create --provider hetzner --id swift-crab --mode native

# Direct Daytona lease: stop, capture filesystem state, and restart the source
crabbox checkpoint create --provider daytona --id swift-crab --mode native --no-reboot=false

# Machine0 named image; draft readiness requires snapshotStatus=READY
crabbox checkpoint create --provider machine0 --id swift-crab \
  --mode native --strategy image --name ci-baseline

# Azure Windows: permit a bounded deallocate/snapshot/restart cycle
crabbox checkpoint create --provider azure --target windows --id swift-crab \
  --strategy disk-snapshot --no-reboot=false

# Archive a custom workdir
crabbox checkpoint create --id swift-crab --workdir /work/cbx_123/my-app

# Return the complete checkpoint record for orchestration
crabbox checkpoint create --id swift-crab --mode native --json
# {"id":"chk_abc123","kind":"aws-ebs-snapshot","leaseId":"cbx_abcdef123456","workdir":"/work/cbx_abcdef123456/my-app","native":{"imageId":"snap-0123456789abcdef0","state":"available"}}
```

**Flags**

```
--id <lease>                Required. Lease id or slug to checkpoint.
--provider <name>           Provider hint when the lease is not yet claimed.
--name <name>               Human-readable checkpoint name.
--mode auto|native|archive  Checkpoint mode (default auto).
--strategy auto|disk-snapshot|image
                            Native checkpoint strategy (default auto).
--wait                      Wait for the native snapshot to become available
                            (default true).
--wait-timeout <duration>   Maximum native snapshot wait (default 45m).
--no-reboot                 Avoid rebooting or stopping the source instance
                            during a native snapshot (default true). Azure
                            Windows disk snapshots require false.
--workdir <path>            Remote workdir to archive (default: the lease's repo
                            workdir).
--recipe-only               Record metadata only; create no artifact.
--json                      Print the complete checkpoint record as one JSON
                            object instead of the human-readable summary.
--reclaim                   Claim this lease for the current repo.
--checkpoint-id <id>        Stable caller-owned operation ID; requires retirement.
--retire-source             Replayable capture followed by verified source release.
--prepare-only              Read-only retirement eligibility; no capture reservation.
--discard-failed            Explicitly discard a verified failed capture and retire.
```

`--mode` also accepts the aliases `provider-native`/`vm` (native),
`ami`/`image` (image), `snapshot`/`disk`/`disk-snapshot` (disk snapshot),
`workspace`/`workspace-archive` (archive), and `recipe`. `--strategy auto`
resolves to a disk snapshot where the provider supports one.

**Strategy details**

- `disk-snapshot` — EBS / Azure managed-OS-disk / GCP persistent-disk / direct
  Hetzner project snapshot; Parallels VM snapshot. AWS macOS always uses an
  AMI-backed checkpoint (with a backing EBS snapshot) because relaunching an EC2
  Mac from a raw root snapshot loses required launch metadata.
- `image` — AWS AMI / GCP machine image / Machine0 named image. Slower, but
  preserves full VM config.
- Azure cannot create a managed image from an active VM, so the Azure native
  path uses a managed OS-disk snapshot. That snapshot requires a managed OS disk
  (the default); creation refuses leases started with
  `--azure-os-disk ephemeral` or `--azure-os-disk ephemeral-preview`, where
  Azure reports success but does not capture live disk state.
- Direct Azure Windows disk snapshots support `windows.mode=normal` leases and
  require `--no-reboot=false`. Crabbox
  deallocates the source for a consistent snapshot, restarts it after snapshot
  creation (including failure paths), and rotates SSH host, SSH login, Windows,
  and loopback-only VNC credentials when each fork boots.
- Azure snapshot names use letters, digits, underscores, and hyphens and are
  limited to 80 characters; generated names retain a unique timestamp suffix.
- Machine0 named images require a stopped source. Crabbox flushes a running
  source, stops it, initiates the image save, and keeps it stopped until the
  exact new version's underlying snapshot is `READY`. It then starts the source
  and atomically refreshes the claimed SSH endpoint. A source that was already
  stopped waits for snapshot readiness but remains stopped. This mandatory
  barrier also applies with `--wait=false`, using `--wait-timeout` when positive
  or the Machine0 create timeout otherwise. Machine0 stop preserves the
  instance and is still compute-billed; it is a consistency step, not a
  cost-saving lifecycle mode.

Before a native snapshot, Crabbox cleans the source: on Linux it runs
`cloud-init clean --logs` (so a forked box regenerates SSH host keys) and
`sync` to flush filesystem writes.

### Replayable source retirement

Before scrubbing a source, check `providers describe <provider> --json` for
`checkpoint-retirement-prepare` in `capabilities.lifecycle`. An older binary
without this capability cannot admit retirement; leave the source on its
ordinary release path. Then invoke the command below with `--prepare-only`.
Its JSON receipt binds `id`, `leaseId`, `provider`, `sourceId`, and
`sourceDisposition: "retire"` to `admission: "ready"` or `"unsupported"`
(with a `reason`). A known policy or strategy refusal writes no checkpoint
journal or claim binding and leaves ordinary release usable. An error, malformed
response, existing operation, or historical hold is not such a refusal.

Preparation is a read-only eligibility observation, not a reservation or
transferable authorization. Actual creation rechecks the current source and
claim before binding them. Persist the ID and scrub/capture phase before their
effects; never treat a later create error as proof that capture was not submitted.
Admission uses ordinary native-mode strategy selection: direct AWS Linux uses
an AMI for default/auto, while local-container uses Docker commit. That selection
retains its implicitness across replay; explicit strategies retain their provider
restrictions.

An orchestrator retiring a source can supply its own stable checkpoint ID:

```sh
crabbox checkpoint create --provider machine0 --id cbx_abcdef012345 \
  --checkpoint-id chk_0123456789abcdef --retire-source --mode native \
  --wait=false --json
```

Retirement uses the lease's canonical repository workspace. It requires
`--mode native` and `--wait=false` and rejects `--workdir`, `--name`, `--reclaim`,
and `--recipe-only`; those options belong to ordinary checkpoint creation.

Persist that ID before invoking Crabbox. Replay the same command after a
pending result, transport failure, or process interruption; never generate a
replacement ID because a response was lost. The returned record includes
`capture.sourceDisposition: "retire"` and a `capture.phase`. Only `retired`
proves source retirement. `prepared`, `stopping`, `submitting`, `pending`,
`ready`, and `retiring` require another bounded replay. `failed` holds the
source and image for explicit recovery. A stopped VM is not a retired VM.
After inspecting a terminal failure, add `--discard-failed` to that same
command to delete its exact failed image, verify image absence, and complete
the requested source retirement. The returned `capture.discardFailed: true`
means there is no reusable image, and that checkpoint cannot be forked even
after retirement completes. An ambiguous submission cannot be discarded through
this flag; it remains held until its provider identity is reconciled.

The operation binds the source resource, repository, and claim generation
before effects. Checkpoint and claim locks serialize cooperating owners;
unknown or replaced sources are never adopted. Machine0 reconciles an
ambiguous save against the original operation and exact image version. It records
the authenticated account before stopping the source and checks that account
before accepting source absence or retiring the claim. Changing accounts causes
a refusal; already-started captures without an account binding remain held for
recovery. Keep the native account and API endpoint stable throughout each
command; separate native invocations cannot make credential changes atomic. Other
supported native providers (AWS, local-container, and brokered Azure
disk/GCP checkpoints) retain an ambiguous submission without resaving
when no provider recovery identity was returned. No background worker is
started. Ordinary checkpoint creation retains its existing restore-source
behavior; source retirement is explicit and does not restart a source merely
to delete it. A configured Machine0 `suspend` release policy is incompatible
with `--retire-source`, not silently changed to destruction. Hetzner ordinary
snapshots remain supported, but source retirement is refused before admission:
its project-scoped API cannot attest the original project after a token change.
Existing unfinished Hetzner retirements remain held for operator recovery.

A provider may retain the released source's immutable identity in a terminal
cleanup receipt. Retirement replay preserves that receipt and revalidates it
through the provider's current account, region, identity, and inventory checks
before marking the operation retired.

Unresolved operations cannot be forked, pruned, or deleted locally. Historical
native records with missing image references are also held: a blank reference
does not prove that submission never happened. Inspect and reconcile the
original provider operation before removing any ownership evidence.

Older binaries do not understand these operation holds. Before downgrading,
stop new capture admission and finish all operations with this binary; do not
run older capture, release, or cleanup commands against unresolved records.
Retain the candidate and its state if an ambiguous operation cannot be
resolved. This is an operational rollback boundary, not transparent downgrade
support.

An older writer can erase a checkpoint record or release a held source because
it ignores the added fields. If it already ran, stop that writer, preserve all
remaining records, and restore the checkpoint journal before resuming with the
new binary. A surviving claim binding prevents recapture after a missing
journal; it cannot undo a source deletion performed by an older binary.

## list and inspect

```sh
crabbox checkpoint list
crabbox checkpoint list --json
crabbox checkpoint list --verify

crabbox checkpoint inspect chk_abc123
crabbox checkpoint inspect chk_abc123 --json
crabbox checkpoint inspect chk_abc123 --verify
crabbox checkpoint inspect chk_abc123 --verify --json
```

`list` and `inspect` read the local checkpoint records. Each record holds the
checkpoint id/name/kind, source lease/provider/region, repo name and git head,
workdir, creation and last-use times, and — for native checkpoints — the
provider resource id; for archives, the tarball path and size.

> A native checkpoint needs **both** halves to fork: the local metadata and the
> provider resource. An archive checkpoint needs the local metadata and the
> tarball. Lose either side and the checkpoint is unusable.

`--verify` audits the other half. For archives it confirms the local tarball
still exists; for native checkpoints it asks the provider (directly for AWS and
Hetzner, or via the coordinator) whether the snapshot or image is still present. JSON output
includes `localState`, `providerState`, and `nextAction`.

An existing native record with a missing provider resource reports
`localState: "metadata_available"`, `providerState: "missing"`, and
`nextAction: "delete_local"`. If the local checkpoint record itself is missing,
JSON inspection succeeds and instead returns a terminal verdict that callers
can use to remove their own reference:

```json
{"id":"chk_abc123","localState":"missing","providerState":"missing","nextAction":"forget"}
```

Human-readable inspection still reports a missing checkpoint as an error.

### Parallels: live VM snapshots

For `provider=parallels`, `list --id <vm>` reads the live snapshots on a source
VM instead of local `chk_...` records, marking which are forkable (linked clones
require a `poweroff` snapshot):

```sh
crabbox checkpoint list --provider parallels --id "macOS Tahoe"
crabbox checkpoint list --provider parallels --id "macOS Tahoe" --json
crabbox checkpoint list --provider parallels --parallels-template tahoe-latest --forkable-only
```

Filters: `--tree` (default true; use `--tree=false` for flat output),
`--forkable-only`, `--current`, and `--name <substring>`. Passing
`--parallels-template` implies `provider=parallels`.

## restore

Restore brings a checkpoint back onto a lease in place. This is for **archive
checkpoints** (and Parallels VM snapshots) — a native image checkpoint cannot be
restored in place; fork it instead.

```sh
# Archive checkpoint -> existing lease
crabbox checkpoint restore chk_abc123 --id target-lease
crabbox checkpoint restore chk_abc123 --id target-lease --clear=false

# Parallels VM snapshot, in place
crabbox checkpoint restore --provider parallels --id "macOS Tahoe" --snapshot "macOS 26.3.1 LATEST"
crabbox checkpoint restore --provider parallels --parallels-template tahoe-latest --snapshot "macOS 26.3.1 LATEST" --dry-run
```

**Flags**

```
--id <lease>      Required. Target lease id or slug (or Parallels VM with --snapshot).
--provider <name> Provider hint.
--snapshot <name> Parallels snapshot name or id to switch to in place.
--clear           Clear the target workdir before extracting (default true).
--workdir <path>  Custom restore workdir (default: the lease's workdir).
--dry-run         Print the restore target without changing anything.
--reclaim         Claim this lease for the current repo.
```

An archive restore uploads the tarball over SSH and extracts it into the
workdir. Restoring a non-archive, non-Parallels native checkpoint is an error
that points you at `checkpoint fork`.

## fork

Create a fresh lease from a checkpoint. Works for both native and archive
checkpoints, and accepts the shared lease-create flags (`--class`, `--provider`,
`--slug`, `--type`, `--os`, etc.).

```sh
crabbox checkpoint fork chk_abc123 --class beast
# checkpoint forked id=chk_abc123 lease=cbx_... slug=purple-whale ...

# Request a friendly slug for the forked lease
crabbox checkpoint fork chk_abc123 --slug update-flow-smoke

# Request a deterministic lease ID; replay adopts the same existing lease
crabbox checkpoint fork chk_abc123 --lease-id cbx_abcdef123456 --slug update-flow

# Return machine-readable lease details for one fork
crabbox checkpoint fork chk_abc123 --lease-id cbx_abcdef123456 --json
# {"checkpointId":"chk_abc123","leaseId":"cbx_abcdef123456","slug":"purple-whale","provider":"aws","workdir":"/work/cbx_abcdef123456/my-app"}

# Fan out one checkpoint into several forked leases for parallel attempts
crabbox checkpoint fork chk_abc123 --count 3 --slug update-flow
# checkpoint forked id=chk_abc123 lease=cbx_... slug=update-flow-1 ...
# checkpoint forked id=chk_abc123 lease=cbx_... slug=update-flow-2 ...
# checkpoint forked id=chk_abc123 lease=cbx_... slug=update-flow-3 ...

# Return machine-readable details for several forked leases as a JSON array
crabbox checkpoint fork chk_abc123 --count 2 --slug update-flow --json
# [{"checkpointId":"chk_abc123","leaseId":"cbx_abcdef123456","slug":"update-flow-1","provider":"aws","workdir":"/work/cbx_abcdef123456/my-app"},{"checkpointId":"chk_abc123","leaseId":"cbx_abcdef123457","slug":"update-flow-2","provider":"aws","workdir":"/work/cbx_abcdef123457/my-app"}]

# Fan out one checkpoint and run the same command on each fork
crabbox checkpoint fork chk_abc123 --count 3 --slug update-flow -- pnpm test -- --shard '{{index}}/{{total}}'
# checkpoint fork command lease=cbx_... slug=update-flow-1 index=1/3 command=...
# checkpoint fork command lease=cbx_... slug=update-flow-2 index=2/3 command=...
# checkpoint fork command lease=cbx_... slug=update-flow-3 index=3/3 command=...

# Fork directly from a Parallels snapshot without recording it first
crabbox checkpoint fork --provider parallels --target macos --id "macOS Tahoe" --snapshot "macOS 26.4" --slug tahoe-test
crabbox checkpoint fork --provider parallels --parallels-template ubuntu-fast --slug test-a --dry-run
```

**Flags** (in addition to the standard lease-create flags)

```
--keep            Keep the forked lease running (default true).
--count <n>       Create multiple forked leases (default 1).
--lease-id <id>   Fixed cbx_<12 lowercase hex characters> lease ID for
                  idempotent external-provider orchestration; native checkpoints
                  only; incompatible with --keep=false, fan-out, --workdir,
                  and commands after --.
--json            Print one fork as a JSON object or multiple forks as a JSON
                  array; incompatible with --dry-run.
--id <vm>         Parallels source VM when forking from --snapshot.
--snapshot <name> Parallels snapshot name or id for a direct fork.
--clear           Clear the workdir before restoring an archive (default true).
--workdir <path>  Remote workdir for the forked lease.
--dry-run         Print the fork target without acquiring a lease.
--reclaim         Claim the new lease for the current repo.
```

**What happens**

- *Native:* acquire a new lease from the checkpoint snapshot/image, wait for
  boot, relocate the workdir from the old lease path to the new one, then print
  the lease id and slug.
- *Azure Windows native:* preserve the snapshotted filesystem in place and
  print `workdir=-`; these desktop leases do not use the POSIX workdir
  relocation flow.
- *Archive:* acquire a standard new lease, upload and extract the tarball into
  the workdir, then print the lease id and slug.
- *Fan-out:* `--count <n>` repeats the same provider-neutral fork flow. When
  combined with `--slug`, Crabbox appends a stable numeric suffix such as
  `update-flow-1`, `update-flow-2`, and `update-flow-3`.
- *Fixed lease IDs:* `--lease-id` reuses the provider's existing fixed-ID
  acquisition and binds the exact native checkpoint ID to its durable create
  intent. Replaying the same checkpoint adopts the existing lease; a different
  checkpoint, changed create intent, ambiguous resources, or a released lease
  ID fails without allocating a replacement. A later fork failure preserves the
  known fixed-ID lease for recovery instead of deleting adopted work. Direct
  AWS, Machine0, and local-container backends support this checkpoint-bound
  contract. Archive checkpoints, direct Hetzner, direct Parallels snapshots,
  coordinator-backed leases, and external providers reject fixed checkpoint
  forks. Fixed IDs must remain retained and cannot fan out, override the
  deterministic workdir, or run commands following `--`.
- *JSON output:* one fork prints one JSON object; `--count` greater than one
  prints one JSON array. Every object contains `checkpointId`, `leaseId`,
  `slug`, `provider`, and `workdir`. Native Windows desktop forks preserve their
  filesystem in place and report an empty `workdir`. Direct Parallels snapshot
  forks have no local checkpoint record, so both their `checkpointId` and
  `workdir` are empty.
  If a later fork or command fails, JSON still reports every lease already
  acquired before the nonzero exit so orchestrators can recover or clean up.
  Provider progress and commands following `--` write to stderr in JSON mode.
- *Command fan-out:* arguments after `--` run through `crabbox run --id <lease>`
  on each fork, so normal sync, command wrapping, history, and proof behavior are
  preserved. Use `{{index}}`, `{{total}}`, `{{lease}}`, and `{{slug}}` in command
  arguments to specialize each fork. Forks and their commands run one after
  another; for concurrent shards with one merged test verdict, use
  [`crabbox shard`](shard.md).

Fork multiple times to run scenarios in parallel:

```sh
crabbox checkpoint fork chk_abc123 --class beast --count 2 --slug update-flow

crabbox run --id update-flow-1 -- npm test
crabbox run --id update-flow-2 -- npm run integration-test
```

For macOS native checkpoints, forks default to the `on-demand` market (unless
you set `--market`) and still require EC2 Mac Dedicated Host capacity; brokered
mode can discover a host, while host-pinned checkpoints reuse the recorded host.

## delete

Delete a checkpoint and its provider resource, then remove the local record.

```sh
crabbox checkpoint delete chk_abc123
crabbox checkpoint delete chk_abc123 --local-only
crabbox checkpoint delete chk_abc123 --dry-run

# Delete a Parallels snapshot directly
crabbox checkpoint delete --provider parallels --id swift-crab --snapshot "crabbox-test-snap"
crabbox checkpoint delete --provider parallels --id swift-crab --snapshot "manual-snap" --yes
```

For native checkpoints, delete removes the provider resource first (AMIs are
deregistered along with their backing EBS snapshots; disk snapshots are
deleted), then removes the local record. Archive checkpoints just lose their
tarball and record.

Deletion is idempotent: if the local checkpoint is already absent, the command
succeeds and prints `checkpoint absent id=chk_abc123`. If the coordinator
confirms that a recorded provider resource is already gone, Crabbox removes the
remaining local record and succeeds. Other provider or authorization failures
still preserve the local record.

Machine0 whole-image deletion is additionally fenced against later versions.
Even when the checkpoint originally created the image name, Crabbox refuses
`images rm` unless the exact metadata-bound version is still the image's only
version. Resolve extra versions manually so checkpoint deletion cannot erase
unrelated work.

If a Machine0 image exists but its recorded version is missing before deletion,
Crabbox refuses with exit 4 and keeps local metadata for manual reconciliation.
After an admitted version removal, the same invocation may confirm that exact
version is gone; whole-image removal must confirm the whole image is gone.
An empty version list alone does not prove whole-image deletion.

Delete and prune return exit 2 (`busy`) while another operation owns the
checkpoint lock. They leave resources and metadata unchanged; retry explicitly
after that operation, including any source rollback, finishes.

**Flags**

```
--local-only      Remove only the local record; skip provider deletion.
--provider <name> Provider hint for a direct Parallels snapshot delete.
--id <vm>         Parallels source VM when using --snapshot.
--snapshot <name> Parallels snapshot name or id to delete directly.
--dry-run         Print the deletion target without deleting.
--yes             Allow deleting a Parallels snapshot whose name is not
                  prefixed with `crabbox-`.
```

Use `--local-only` when provider access cannot confirm safe resource deletion,
such as after account migration or an ownership ambiguity.

## prune

Delete checkpoints selected by creation age, time since last use, or both,
optionally restricted to one kind.

```sh
crabbox checkpoint prune --older-than 30d --dry-run
crabbox checkpoint prune --unused-for 14d --dry-run
crabbox checkpoint prune --older-than 30d --unused-for 14d --kind native
```

**Flags**

```
--older-than <duration> Select checkpoints created before this age.
--unused-for <duration> Select checkpoints whose last successful fork or
                        restore was before this age.
--kind native|archive   Restrict to one kind.
--dry-run               Print matches, including creation and last-use times,
                        without deleting.
--local-only            Skip provider deletion for native checkpoints.
```

At least one of `--older-than` or `--unused-for` is required; when both are
present, a checkpoint must satisfy both. Durations accept Go syntax such as
`720h` or a whole number of days such as `30d`. Native checkpoints prune through
the same provider-first deletion path as `checkpoint delete`, so a provider
failure keeps the local record. Preview the exact match set with `--dry-run`.

Crabbox does not run an automatic checkpoint reaper. Any scheduler must invoke
this CLI as the user and in the environment that owns the local checkpoint
records. See [Checkpoints](../features/checkpoints.md#lifecycle-and-expiry) for
the detailed lifecycle behavior and scheduled-job recipe.

> Provider snapshots and images keep accruing storage cost while they exist.
> Prune stale checkpoints periodically, and name checkpoints after the scenario
> they preserve so cleanup candidates are easy to spot.

## Provider support

**Native checkpoints**

| Provider | Default (`disk-snapshot`) | `--strategy image` |
| --- | --- | --- |
| AWS Linux | EBS snapshot | AMI |
| AWS macOS | AMI-backed checkpoint (backing EBS snapshot) | AMI |
| Azure Linux | Managed OS-disk snapshot | not supported from an active VM |
| Azure Windows (`windows.mode=normal`) | Managed OS-disk snapshot (`--no-reboot=false`) | not supported |
| GCP Linux | Persistent-disk snapshot | Machine image |
| Hetzner Linux (direct only) | Project snapshot | not supported |
| Daytona Linux (direct only) | Filesystem snapshot (`--no-reboot=false` for a running source) | Same filesystem snapshot |
| Parallels | VM snapshot | — |

Brokered native checkpoints (through a configured coordinator) cover AWS
Linux/macOS and Azure/GCP Linux leases. Azure Windows leases use the direct
managed-OS-disk snapshot path. Direct AWS Linux/macOS leases create
AMIs locally without a coordinator: `--mode auto` falls back to a workspace
archive when no coordinator is configured, while `--mode native` or
`--strategy image` creates an AMI in the configured AWS region. Parallels native
snapshots run directly against the Parallels host.
Direct Hetzner Linux leases create project snapshots with `--mode native` or an
explicit disk-snapshot strategy; default auto mode retains the archive fallback.
Brokered Hetzner leases always use archive checkpoints.

Direct Daytona native snapshots require `--mode native` or an explicit strategy.
Capture requires a stopped source and waits for snapshot readiness even with
`--wait=false`; running sources require `--no-reboot=false` and are restarted
after capture. Already-stopped sources remain stopped. Fork starts a new sandbox
from the snapshot and relocates the workspace; native in-place restore and
memory capture are not supported. See [Daytona](../providers/daytona.md#native-snapshots-and-forks)
for ownership checks and recovery after an uncertain capture.

**Archive checkpoints**

Any POSIX SSH-accessible lease (Linux, macOS, or Windows under WSL2). Portable
across providers. Windows-native (non-WSL2) leases are not supported.
