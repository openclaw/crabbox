# Fleet coordinator overload audit (Cloudflare error 1101)

This audit covers intermittent `http 500: error code: 1101` responses and
run-admission `context deadline exceeded` failures observed on 2026-10-11/12
(UTC), at source base `6b2521389`. It complements the
[memory audit](coordinator-memory.md): that audit bounds what one request
allocates; this one bounds how much retained history the coordinator reads
per unit of time.

## What clients see

Every authenticated request is forwarded to one `FleetDurableObject`. When the
object cannot accept or finish a request, `stub.fetch` rejects, and the Worker
entrypoint used to rethrow, so Cloudflare answered `500` with
`error code: 1101`. A filtered production tail (request line, status,
outcome, exception message only) recorded three distinct runtime messages:

| Message | Trigger |
| --- | --- |
| `Durable Object's isolate exceeded its memory limit and was reset.` | Portal external-runner read; fixed separately by bounding that read. |
| `Durable Object reset because its code was updated.` | Every deployment or Worker settings change; affects in-flight requests. |
| `Durable Object is overloaded. Requests queued for too long.` | Requests waiting at the object's input queue; recurring without any deploy. |

Over a 20-minute window after the memory fix had rolled out, Worker wall time
for ordinary calls (lease GET, heartbeat, run telemetry) was p50 0.8 s, p90
4.0 s, p99 7.7 s, max 34.6 s. Overload bursts followed a reset by under a
minute and also occurred with no reset nearby.

## Why the object saturates

Terminal lease records are never pruned. A bounded key-density sample of the
production `lease:` namespace (one 100-row keyset page; lease IDs are uniform
random hex) estimates about 34,000 records (±10%), 100% terminal in the sample,
averaging 4.6 KiB as served (p90 12 KiB, mostly `telemetryHistory`). That is
roughly 155 MB of structured data per full walk, growing by about 220 records a
day.

Two paths walk all of it repeatedly:

1. `scheduleAlarm()` visits every `lease:` record and, before this change,
   issued one `lease-provisioning:<id>` point read per record, regardless of
   whether the lease could contribute a wake. It holds the lifecycle mutex and
   uses default storage reads, so the Cloudflare input gate stays closed while it
   runs. It runs at the end of every maintenance pass and from about sixty
   request handlers, including release, create, workspace and ready-pool paths.
2. Each maintenance pass also performs a discovery walk of all `lease:` records
   (outside the gate since the discovery fix).

Maintenance passes are frequent. An AWS heartbeat whose SSH source policy is
unchanged records an ingress-reconcile target with `retryAt: now` and arms the
alarm one second later; while any AWS lease is provisioning, the reconcile
defers ten seconds and re-arms. Ingress reconciliation runs inside the full
maintenance pass, so ordinary fleet traffic can keep passes running back to
back. The no-progress alarm backoff is bypassed whenever a request re-arms the
alarm during a pass, which heartbeats do. The tail does not stream Durable
Object alarm invocations, so pass cadence is inferred from these code paths,
not measured.

Deterministic probe with the in-memory test storage (counts are meaningful;
its `list()` re-sorts on every call, so its timings are not):

| Retained leases | `scheduleAlarm()` lease pages | point reads | maintenance pass lease pages | point reads |
| ---: | ---: | ---: | ---: | ---: |
| 1,000 | 8 | 1,007 | 16 | 1,008 |
| 8,000 | 63 | 8,007 | 126 | 8,008 |
| 34,000 | 266 | 34,007 | 532 | 34,008 |

Run admission (`PUT /v1/runs/<id>`) uses the lifecycle queue and waits behind
whichever `scheduleAlarm()` holds the mutex. The CLI gave admission a 10 s
budget with two immediate attempts, so one 1101 followed by one queued retry
ended in `run admission ... unavailable before command: context deadline
exceeded`.

## Ruled out or minor

- **Lease lifetime maintenance** (`maintainLeaseLifetimes`) iterates only
  discovered candidates and performs provider I/O outside the lifecycle mutex.
  The 1101 failures predate it.
- **Lifetime readback** (`GET /v1/admin/leases/<id>/lifetime-readback`) was
  queued as lifecycle work and held the mutex across AWS reads while the
  rollout canary polled it. It is read-only and now uses the direct queue.
- **Tight `now + 1` alarm loops**: the no-progress backoff exists, and
  heartbeat-driven ingress wakes explain frequent passes without a stuck
  candidate. Alarm invocation logs are needed to confirm the cadence.
- **CPU-limit resets**: none observed in the tail windows.

## Changes in this audit

- `scheduleAlarm()` evaluates the pure lifetime, readiness and lease-alarm
  predicates before any per-lease storage read. Settled history skips the
  ownership and workspace reads; the armed alarm is unchanged (regression test
  compares 0 against 500 settled leases and requires identical alarms and
  ownership reads only for the live lease).
- Lifetime readback moved to the direct queue.

The Worker entrypoint separately maps stub failures to a structured `503`
with `Retry-After`, and the CLI retries reads and run admission with backoff.

## Maintenance-candidate index

The coordinator now maintains `lease-maintenance:<leaseID>` beside every lease
write and delete, in the same transaction or coalesced Durable Object write batch.
Stores without write coalescing still publish membership before the lease and
retract it only after, so an interrupted write leaves a stale entry rather than a
missing one. Its time-independent membership includes live leases,
unresolved cleanup or provisioning custody, pending runtime-adapter deletion,
non-complete lifetime policies, and retained AWS SSH access. Settled terminal
history without those markers is excluded. Each consumer still applies its
original predicate to freshly loaded records.

After backfill, five hot reads use this index: alarm reconciliation, maintenance
discovery, provider-access lease collection, live slug lookup, and registration
conflict collection. Index keys preserve lease-ID ordering and slug lookup still
stops after its second match. Reads use bounded uncached pages; discovery keeps
`allowConcurrency: true` so it does not close the input gate while scanning.

`lease-maintenance-state` records the version and cursors. Backfill adds members
in at most 32 pages of 128 lease records per maintenance pass (4,096 rows; roughly
nine passes for 34,000 leases). Until it completes, all five consumers retain
the full-history fallback. A version change restarts that fallback and backfill.
An add-only audit starts on the first maintenance pass after its daily deadline,
continues across bounded passes while consumers keep using the index, and
schedules the next audit 24 hours after completion. A new runtime generation
(each Cloudflare deployment, each Node process start) starts an audit
immediately, repairing membership written by a rolled-back runtime or interrupted
between a lease write and its index write. The existing 15-minute
scheduled maintenance trigger advances scans even when no lease alarm is armed;
no alarm-selection behavior changes.

Completion emits one JSON line with event `coordinator.lease_maintenance_index`
and `index` (`maintenance` or `current`):
`backfill_complete` includes `scanned` and `members`; `audit_complete` includes
`scanned` and `added`. A nonzero audit `added` count indicates missing membership
and warrants investigating a missed write path. Stale entries are removed only
in maintenance, after a transaction rereads the lease and confirms it is still
missing or no longer a member. Ordinary index readers never prune.

## Index-backed lease listings

`crabbox list` previously paged through all roughly 34,000 retained `lease:`
records (about 340 requests per full walk; production observed roughly 650 such
page requests per hour). Current listings now use a separate
`lease-current:<leaseID>` index after backfill. Active and provisioning queries
use the existing maintenance index, whose membership includes all live leases.
The same selection accelerates legacy newest-first lists and admin lease lists.
`crabbox list --all` now paginates its active admin lookup instead of relying on
a single capped history walk; older coordinators retain the legacy fallback with
a warning when its limit is reached.

The current index has its own `lease-current-state`, version 1, bounded backfill,
daily audit and runtime-generation repair. Introducing it does not reset the
already-complete `lease-maintenance-state` version 1. Both indexes use the same
write funnel and transaction-fenced stale pruning. Until the selected index is
ready, listings retain raw history scans. Sealed keyset cursors preserve their
source across pages; an unavailable indexed source requires restarting the list.

The current predicate is deliberately unchanged. Legacy `keep=true` leases
without confirmed cleanup still make roughly 29% of retained history current.
That set cannot safely use the smaller maintenance index. The bounded
[retained reconciliation pass](../features/lifecycle-cleanup.md#retained-record-reconciliation)
now retires eligible AWS members only after proving cleanup; the listing
predicate remains unchanged.

## Remaining work

Portal views, unfiltered or historical lease listings, administrative lease/host
audits, usage and monthly cost accounting still scan complete history because
their answers include
terminal records. AWS/Azure orphan sweeps, runtime-adapter identity reclamation,
and host reservations also retain complete-history reads: omitting ownership
or reservation evidence could authorize unsafe cloud-resource reuse/deletion.
These paths are deliberately outside the maintenance index.

Separate ingress wakes from full maintenance remains useful: heartbeat-driven
SSH reconciliation can still cause frequent passes, although their hot lease
reads now scale with outstanding work. Terminal lease archival or pruning still
needs a retention policy and usage rollups to preserve monthly accounting.

Retained terminal current-index members now have a bounded
[reconciliation pass](../features/lifecycle-cleanup.md#retained-record-reconciliation).
It confirms AWS cleanup only with original account/Region evidence and two complete
observations ten minutes apart, preserving unresolved companion and provider work.
Other providers are classification-only. Inspect `crabbox admin retained-reconcile
--json` for counters, cursor progress and backoff; deletion defaults off. This
shrinks current listings without changing the current predicate or deleting history.
