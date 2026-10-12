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

## Remaining work (needs a design decision)

The full `lease:` walk in `scheduleAlarm()` and discovery is still
proportional to all retained history. Options, in order of leverage:

1. **Durable maintenance-candidate index.** Maintain `lease-maintenance:<id>`
   for leases that are live, need cleanup, are lifetime candidates, may need
   interrupted-provisioning recovery, or have a runtime-adapter delete pending.
   Write it beside every lease write (`putLease` plus the transaction puts),
   backfill with a cursor across maintenance passes, and fall back to the full
   walk until the backfill marker is set. Alarm reconciliation and discovery
   then scale with outstanding work (tens of leases), not history.
2. **Separate ingress wakes from full maintenance.** Run AWS ingress
   reconciliation from its own wake so heartbeats cannot schedule a full pass
   every second.
3. **Terminal lease retention.** Archive or prune settled leases after a
   retention window, with usage rollups so monthly usage stays correct. This
   is a product decision.
