# Telemetry

Read when:

- changing how Crabbox samples runner load, memory, disk, or uptime;
- adding metrics to lease records or run history;
- debugging missing portal sparklines or stale telemetry pills;
- deciding where telemetry stops and full observability begins.

Crabbox captures lightweight runner telemetry so a lease detail page or run
record can answer "is this box healthy right now?" and "did this command spike
memory?" without standing up Prometheus or shipping a logging agent. Telemetry
is best-effort, capped, and only collected for Linux leases.

## What gets captured

For a Linux lease, the CLI runs a small read-only script over the lease's SSH
connection whenever it already has a reason to talk to the box during a run (a
run heartbeat or a mid-run sample). The script reads:

- `load1`, `load5`, `load15` from `/proc/loadavg`;
- `memoryTotalBytes`, `memoryUsedBytes`, `memoryPercent` derived from
  `MemTotal` and `MemAvailable` in `/proc/meminfo`;
- `diskTotalBytes`, `diskUsedBytes`, `diskPercent` from `df -PB1 /`;
- `uptimeSeconds` from `/proc/uptime`.

Each sample is parsed into a `LeaseTelemetry` record:

```json
{
  "capturedAt": "2026-05-07T07:42:18Z",
  "source": "ssh-linux",
  "load1": 0.42,
  "load5": 0.3,
  "load15": 0.18,
  "memoryUsedBytes": 5368709120,
  "memoryTotalBytes": 16777216000,
  "memoryPercent": 32.0,
  "diskUsedBytes": 21474836480,
  "diskTotalBytes": 107374182400,
  "diskPercent": 20.0,
  "uptimeSeconds": 38400
}
```

The collector returns nothing for non-Linux targets and for hosts with no
SSH address, so managed Windows, EC2 Mac, and static SSH macOS/Windows leases
produce no telemetry. Delegated-run providers (sandbox/proof runners with no
SSH lease) also produce none. Their lease records have no `telemetry` field,
and parsing is strict: a sample with no usable numeric metric is dropped
rather than stored as an empty record.

## Where it lives

Telemetry is stored in two places on the coordinator:

- **Lease record.** `FleetCoordinator` keeps the most recent sanitized
  snapshot on the lease (`telemetry`) plus a bounded ring of the latest
  samples (`telemetryHistory`, capped at 60). Older samples drop off as new
  ones arrive.
- **Run record.** While a run is active, the CLI POSTs samples to
  `POST /v1/runs/{run-id}/telemetry`. The run keeps a `RunTelemetrySummary`
  with a `start` snapshot, an `end` snapshot, and a `samples[]` series (also
  capped at 60) so longer commands show a load/memory/disk trend rather than
  just two endpoints.

The coordinator sanitizer only accepts the numeric fields above plus
`source` (truncated to 32 characters). Raw `/proc` content, hostnames, kernel
versions, mount points, and process tables never reach storage.

## How samples get sent

The CLI samples through `collectLeaseTelemetryBestEffort`, which wraps each
collection in a 5-second timeout. A failed collection is never an error — it
just means the box was busy or briefly unreachable, and the caller proceeds
without a sample. Sampling happens in two contexts, both during a run:

1. **Heartbeat.** While a run is active, the heartbeat goroutine collects a
   fresh sample, attaches it to the heartbeat body, and lets the coordinator
   update the lease record's `telemetry` snapshot and append to the
   `telemetryHistory` ring.
2. **Run telemetry.** During the same run the recorder captures a `start`
   snapshot, then samples on a 15-second ticker and posts each to
   `POST /v1/runs/{run-id}/telemetry`. The summary is finalized on
   `POST /v1/runs/{run-id}/finish`, which also records the `end` snapshot.

`crabbox status` does not collect a fresh sample itself — it displays the
`telemetry` snapshot already stored on the lease record (most recently written
by a run's heartbeat), so the trailing `telemetry=<age>` reflects how long ago
that sample was captured.

## What shows up where

- **`crabbox status --id ...`** appends, when a sample is available,
  `load=0.42 mem=5.0GiB/16.0GiB disk=20.0GiB/100.0GiB uptime=10h40m
  telemetry=2s`. The trailing `telemetry=<age>` shows how fresh the sample is.
- **`crabbox history`** appends `resources=load=0.42 mem=32% disk=20%
  mem_delta=+512.0MiB` for runs that carry telemetry. `mem_delta` is the
  difference between the `start` and `end` memory snapshots.
- **`/portal/leases/{id-or-slug}`** renders the latest sample as health pills
  and draws load, memory, and disk sparklines once at least two samples exist
  (until then each line reads "waiting for samples"). A sample older than
  10 minutes shows a `stale <age>` pill; otherwise a `live` pill. Memory or
  disk at or above 85% adds a `memory N%` / `disk N%` pill; `load1` at or
  above 16 adds a `load N.N` pill. With no captured sample the strip shows a
  single `no signal` pill.
- **`/portal/runs/{run-id}`** renders a compact load/memory/disk panel from the
  current snapshot, memory and disk deltas, and the sample count.

## Limits and thresholds

- Sampler timeout: 5 seconds per collection.
- Run telemetry interval: 15 seconds while a command runs.
- Lease telemetry history ring: 60 samples.
- Run telemetry samples ring: 60 samples.
- Portal stale threshold: 10 minutes since `capturedAt`.
- Portal pill thresholds: memory or disk percent ≥ 85, `load1` ≥ 16.

These thresholds are display hints, not alerts. Crabbox does not page or take
automatic action on telemetry; use real observability tooling for that.

## When to use full observability instead

Telemetry is intentionally narrow: a "is the box healthy?" pulse, not a
metrics pipeline. For per-process traces, per-command flame graphs, or
historical correlation across many runs, scrape the runner with a real agent
or ship logs to a real backend. Crabbox does not try to replace that layer;
see [Observability](../observability.md) for what it surfaces upstream.

## Extending the captured fields

Telemetry has no user-facing toggle; there is no env flag to silence sampling.
To add a captured field, change all four layers:

- the parser in `internal/cli/telemetry.go`;
- the schema in `worker/src/types.ts`;
- the sanitizer and storage in `worker/src/fleet.ts`;
- the lease and run renderers in `worker/src/portal.ts`.

Keep new fields numeric, sanitized, and bounded. Free-form strings, hostnames,
and process names do not belong on the telemetry record.

Related docs:

- [Coordinator](coordinator.md)
- [Orchestrator](../orchestrator.md)
- [History and logs](history-logs.md)
- [Observability](../observability.md)
- [Source map](../source-map.md)
