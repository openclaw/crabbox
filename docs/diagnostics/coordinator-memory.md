# Fleet coordinator memory and reset recovery audit

This audit covers `FleetCoordinator` and its in-isolate helpers at source base
`1ed9f60b4` (2026-10-08). It identifies allocations from code and reproduces
unbounded reads with storage instrumentation; it does **not** attribute a
production isolate reset to a measured heap consumer. Twenty live leases do not
bound retained history, concurrent requests, or bridge traffic.

## Findings ranked for a fleet with thousands of historical records

| Rank | Path and allocation | Change or remaining exposure |
| --- | --- | --- |
| 1 | `leaseRecords()` read every full lease into a storage map and array. Usage, portal, pool status/borrow/capacity/metrics, registered-lease admission, and adapter reclamation called it. Diagnostics, telemetry, and provisioning history magnified each row; concurrent portal requests multiplied the graphs. | Removed the helper. Usage accumulates counters over 128-record uncached pages; audit retains only the requested newest rows; pools fetch referenced leases; registration retains live/deletion candidates; reclamation short-circuits. Portal lists retain the requested newest rows plus active Mac host leases, while admin views project each row before retention. Admin responses still contain all small summaries, and usage group counts scale with distinct owners/orgs/types. |
| 2 | `readRunLog()` loaded all chunks, sorted and copied arrays, joined the full log, and created another response encoding. Stored logs are capped at 8 MiB, but several concurrent downloads or portal previews could multiply that footprint. | Downloads pull one uncached chunk (normally <=64 KiB) at a time, with no read-ahead; cancellation closes the iterator. Portal detail retains a 12 KiB character tail while scanning. Legacy single-value logs remain one storage value. |
| 3 | `maintainReadyPools()` retained every pool entry and referenced full lease. Azure deferred cleanup loaded every journal and launched one promise per record, including terminal history. Ticket/OAuth cleanup similarly loaded all records and deletion promises. | Pool maintenance consumes one entry/lease at a time. Deferred Azure cleanup scans 128-record uncached pages and performs provider work sequentially; alarm selection retains one earliest journal. Native-VNC, runtime-adapter, bridge, credential-handoff, and OAuth cleanup scan bounded pages with sequential deletion. |
| 4 | `webVNCEvents` retained twelve events per historical lease indefinitely; `hydratedEgressSessionState` retained every examined/closed lease ID. | Both now retain at most 1,024 lease IDs. Only diagnostic events and hydration markers are evicted. Durable egress state, replacement tombstones, and live sockets are unchanged; evicted hydration markers cause a fresh storage read. |
| 5 | Pool request helpers (`readyPoolEntries`, `readyPoolFillClaims`, portable access borrow) and `externalRunnerRecords` still materialize collections for complete responses, cross-namespace exclusion, or ordered candidate selection. | Remaining exposure scales with pool/runner cardinality, not unrelated lease/run history. Pool terminal retention limits age, not count. Follow-up: indexed scoped candidate queries and paginated inventory responses; do not silently truncate borrow eligibility or ownership checks. |
| 6 | Provider image helpers (`promotedImagesByID`, `variantImagesByID`, `promotedImageMetadataByID`, `deletePromotedAWSImageRecords`, AWS created-image/selection scans, Azure promoted-image scans) and `findManagedCheckpointImage` list entire metadata/claim/intent namespaces. | Remaining exposure scales with image/checkpoint catalog size. Image deletion also builds a promise chain retaining matching entries. Follow-up: scoped indexes or bounded scans retaining only exact matches, preserving ambiguity checks and promotion pins. These are not ordinary stock-image admission history reads. |

### Other allocation surfaces and bounds

- **Incoming bodies and serialization:** `bufferCoordinatorRequestBody` uses a
  whole `arrayBuffer`, and `readJson` parses the whole JSON document. The Worker
  body mutex admits one lifecycle upload through execution, but its waiting
  closures retain request streams; direct requests bypass it. There is no
  application-wide queue-count/byte cap. `fetchReplayableLeaseCreate` clones and
  parses the create body before forwarding. Run finish normalizes/joins log
  input, encodes it, computes a receipt hash, and writes up to 128 retained
  chunks concurrently. Its stored 8 MiB limit does not bound the original
  request. These are high-priority **conditional** risks if large uploads were
  concurrent with the incident; changing accepted upload limits or receipt
  semantics needs a deliberate protocol change. Node has separate request
  limits (16 MiB normally, 64 MiB for run finish).
- **Code bridge:** `codeProxyHTTP` reads the whole request before checking its
  size. Pending HTTP responses have a 30-second timeout but no aggregate byte
  cap; chunk arrays are joined at completion. Pending WebSocket frame chunks
  have no aggregate count/byte cap or frame timeout. These can dominate memory
  under bridge load, independent of fleet history. Follow-up requires bounded
  admission and streaming/backpressure with explicit overflow behavior.
- **Other live bridges:** socket maps follow connections; WebVNC buffers,
  workspace-terminal buffers, and runtime-adapter requests have local limits.
  Active egress records and per-lease replacement tombstones remain ownership
  state (256 replacements per lease), not an evictable cache. Pending hydration
  promises and keyed mutex entries are removed on completion. Ordinary mutex
  queues have no admission cap. The maintenance runner retains one in-flight
  pass and at most one follow-up, not one promise per tick.
- **Run/event/idempotency state:** recent-run and lease scans retain bounded
  newest results; event pages use `limit` and `noCache`. Event append and create
  attempt tables use exact-key reads, not whole-table maps. Legacy run
  attribution scans events in pages and retains distinct lease IDs/owners.
  Run retention prunes a bounded page; prefix deletion batches 128 records.
- **Provisioning/pool journals:** durable provisioning ticks read four due
  records and cap operation records at 100 KiB. Portable pool maintenance reads
  a bounded due batch and only the referenced grants/reservations. Legacy
  recovery retains a bounded provider-recovery batch. Admission cost accounting
  already scanned 128-record uncached pages; live access/cleanup candidates are
  retained intentionally and can exceed the active cap when cleanup is owed.
- **Alarm/census aggregates:** maintenance discovery retains candidate IDs;
  alarm selection retains compact deadline records, workspace shape sets, and
  viewer deadlines. Provider orphan sweeps retain provider inventory and exact
  matching ownership/census maps. These scale with outstanding work/inventory,
  not full historical lease payloads. Inventory failures must continue to fail
  closed; truncation must never authorize deletion.
- **Auth/pricing:** device membership cache already caps at 1,024 entries,
  Access JWKS cache at eight domains, and pricing at 256 entries per scope.
  Token refresh joins pending work and clears failures. Config JSON parsing is
  deployment-sized, not a load of historical records. No whole-table token or
  create-attempt cache was found.

## Interrupted create behavior

The previous restart detector only considered leases with no cloud ID and a
persisted provider-request start. Two durable states fell outside it:

1. Admission had committed, but provider preparation/dispatch had not finished.
   The lease had no runtime-generation marker yet.
2. The provider had reported its resource ID, but readiness had not completed.
   The detector explicitly excluded any lease with a cloud ID.

Admission now persists the runtime generation. On the next maintenance tick,
legacy provisioning owned by another runtime becomes an inspectable `failed`
lease. A pre-dispatch record keeps its create-attempt binding; no allocation is
replayed. A captured resource goes through the normal provider-owned cleanup
path, including ownership validation and retained debt on rejection. A request
with no returned resource ID keeps the existing settlement and repeated-absence
confirmation windows. Workspace recovery and durable continuation journals
retain their existing lifecycle owners.

The normal recovery alarm remains within one minute. A replay returns the
terminal error (422 when absence/cleanup is confirmed, otherwise 409 with the
failed record), instead of another provisioning response. Provider scope,
resource identity, and attempt binding remain durable. An interrupted provider
request is never treated as proof that EC2 did not launch an instance.

This explains two reproducible stuck-create paths, but does not prove the exact
production sequence. At this source revision, `awaiting-instance` means a
provisioning record had a cloud ID. A later 404 alone cannot establish whether
that record was deleted, lookup used another ID/slug/principal, or another
runtime version responded. Reservation cancellation can remove a record only
when no provider request or resource is recorded; resource-bearing records must
remain in custody. The client timeout/cancellation is not evidence of absence.

## Regression evidence

- `bounds historical lease reads for %s`: seed 1,000 historical leases with
  bulky diagnostics and reject unbounded lease scans. Usage, empty pool status,
  and audit all failed on the base; now pass. Pool status performs zero
  unrelated lease-history reads; usage still counts all 1,000 leases.
- `streams ordered run logs on demand and stops reading after cancellation`:
  seed 128 chunks, consume two, cancel, and assert exactly two uncached
  single-chunk reads. The base returned the whole joined log on the first read.
- `bounds per-lease bridge history caches and reloads evicted hydration markers`:
  simulate 1,100 leases; the base retained 1,100 entries, now both cap at 1,024
  and an evicted marker triggers a storage reread.
- `retains inspectable interrupted create custody after a reset at %s`:
  pause a real synthetic create at preparation, provider request, or resource-ID
  publication; copy only durable storage into a fresh coordinator; verify
  failure, terminal replay, no second allocation, and cleanup of the captured
  ID. Preparation and captured-ID cases failed on the base; hostless dispatch
  was already covered by earlier recovery fixes.

These are deterministic cardinality/backpressure tests, not heap measurements.
Existing late-resource recovery, provider ownership, cancellation, pool ordering,
portal, usage, and receipt tests remain part of the full Worker gate.

## Production evidence needed

The deploying operator should correlate the reset timestamp and nearby requests
without collecting secrets or workload contents:

- Workers/DO analytics: reset/OOM outcomes, deployment/version, route, concurrent
  requests, duration/CPU, request and response sizes, storage read counts, and
  alarms in flight around the reset. Cloudflare does not expose a reliable
  per-request JavaScript heap high-water mark through ordinary application APIs.
- Tail logs: admission phase timings, last completed storage commit, runtime
  generation change, alarm start/completion/failure/backoff, and recovery or
  provider-cleanup errors. Log opaque attempt hashes, never bearer/create tokens.
- Exact retained lease and create-attempt binding: canonical ID, state,
  generation, dispatch/settlement timestamps, cloud ID, region/account scope,
  cleanup markers, and alarm/due-index timestamps, including the observation
  immediately before cancellation.
- Aggregate namespace counts and encoded size distribution: leases, runs,
  event/log chunks, image/checkpoint records, pool entries, bridge tickets, and
  cleanup journals; largest-record sizes and number/bytes of simultaneous run
  finishes, log downloads, and code-bridge requests.
- Provider-owned reconciliation proof for the affected attempt: exact scoped
  launch identity and any late-discovered EC2 resource, checked against the
  retained claim. No live provider inventory was accessed for this audit.

A local heap profile under representative synthetic traffic can rank the
remaining conditional risks. A production smoke must verify both successful
creation and terminal recovery with zero cleanup residue after deployment.
