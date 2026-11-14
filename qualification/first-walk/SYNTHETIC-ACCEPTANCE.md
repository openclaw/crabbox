# Serial first-walk qualification acceptance

This candidate requests qualification of the existing diagnostic package, without
changing its implementation or the production baseline. Keep it open while an
explicitly authorized attempt uses its exact commit. This document is a checklist,
not execution authority. The protected workflow must still verify the open
same-repository candidate, current protected main, exact package and production
baseline. Source drift requires reconciliation before dispatch.

## Before execution

- Build fresh version-2 artifacts with the default-branch qualification workflow
  and the pinned baseline and overlay in `v1/manifest.json`. Local builds and old
  handoffs cannot replace that run's sealed artifacts.
- Verify the serving private authority's exact module, protocol, bindings,
  namespaces and version against the admitted artifacts and configuration. Any
  required update needs separate authorization, idle-state proof and recovery.
  Preserve registry state and credentials required for recovery.
- Admit one fresh immutable handoff, scoped run permissions, credential custody,
  protected manual approvals and independent recovery. Reserve the complete cycle
  before dispatch, including source preparation, snapshot completion, a possible
  35-minute ownership walk, post-walk verification and settlement. Do not reset or
  extend an earlier attempt's clocks.

## One untouched data copy

Use the existing `fixed-node-monorepo-v1` preparer and its pinned public inputs.
Its source receipt must establish the saved workspace and populated
`/var/cache/crabbox` on one captured filesystem within the existing byte, entry
and preparation-time limits. Preparation may inspect the source before capture.

The existing owner must preserve the source-use pin through attachment, execution
and complete cleanup. Attach one disposable snapshot-backed data copy after stock
guest readiness. Resolve the exact snapshot, volume, NVMe device and partition;
never select by filesystem UUID or label. Reject an already mounted or prewarmed
target. Mount in the private namespace, record journal replay, and run the existing
serial diagnostic ownership routine once before copying, scanning or verifying
the restored tree. A normal restore followed by a rescan does not qualify.

Retain the existing resource and runtime bounds, numeric ownership checks,
descriptor anchoring, no-follow traversal, conditional writes and failure
propagation. This candidate introduces no optimization or repair bypass.

## Evidence and settlement

Capture elapsed time, visited/mismatched/changed counts, ownership-process CPU and
I/O, device activity and exits. Keep the final process-I/O sampling limitation
explicit. Verify ownership and layout afterward, reusing that pass for directory
fanout and depth. Missing measurements remain unknown rather than zero.

Qualify the existing executor-death and independent finalizer/reaper path. Preserve
the recorded identity and uncertain-response recovery rules: stop writers, settle
processes and guests, completely inventory owned root/data volumes, delete owned
copies and checkpoints, then release source protection only after finalized
proof. Preserve borrowed inputs. Retain access until cleanup is verified; a failed
command or incomplete inventory does not establish absence.

This observation establishes behavior of the fixed public workload and the
qualification lifecycle. An attached data copy omits original root-boot
contention. It cannot demonstrate improvement on a historical checkpoint or
justify either rejected scheduler. Use the evidence to select the responsible
work: traversal/open/readdir waits, ownership mutations, storage initialization,
or another demonstrated cause. If it does not discriminate, record that limit
before proposing an implementation.
