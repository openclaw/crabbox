# capacity

`crabbox capacity [--json]` reports current fleet, org, and owner admission
counts and limits for the authenticated caller. It requires a configured coordinator and uses
the normal coordinator credentials, including the existing resolved shared or
`unknown` owner identity. It never falls back to a direct provider or local lease
list. Older coordinators without `GET /v1/capacity` return an unsupported error.

```sh
crabbox capacity
crabbox capacity --json
```

The only option is `--json` (plus `--help`). There are no positional arguments or
owner, user, org, month, or scope selectors. Even admin requests use the
owner and org resolved by normal authentication. The API rejects every query parameter
with HTTP 400.

```text
fleet 21/20 (blocked)
org example-org 17/20
owner github:12345 17/20
observed at: 2026-09-02T12:00:00.000Z
Snapshot only; not a reservation or approval to allocate.
admissible: no — fleet cap reached
```

JSON preserves the original owner fields and adds fleet and org dimensions plus
an active-lease admission decision:

```json
{
  "owner": "github:12345",
  "activeLeases": 17,
  "effectiveLimit": 20,
  "observedAt": "2026-09-02T12:00:00.000Z",
  "fleet": { "activeLeases": 21, "limit": 20 },
  "org": { "key": "example-org", "activeLeases": 17, "limit": 20 },
  "admissible": false,
  "blockedBy": "fleet"
}
```

`fleet.limit` and `org.limit` are `null` when their configured limit is zero
(unlimited). The original `effectiveLimit: 0` retains its meaning of an unlimited
owner limit for compatibility; text prints `unlimited` for every unlimited limit.
`org.key` is the authenticated org's display label. Older coordinators that omit
the new fields still render an owner line and snapshot metadata, with no admission
decision; their JSON remains unchanged.

The selected limit uses the existing capacity-owner policy, independently of
admin authentication. A positive elevated limit for a configured member selects
the larger of ordinary and elevated limits, including when the ordinary limit
is zero. The response does not expose membership or the underlying limit config.

`activeLeases` counts existing admission entries only, with no candidate added.
The fleet dimension counts all owners; the org dimension counts all owners in
the caller's org. `admissible` answers whether one additional lease fits the
active-lease caps. `blockedBy` is `"fleet"`, `"owner"`, `"org"`, or `null`, using
the same fleet → owner → org ordering as allocation. A fixed-lease orchestrator
can use this to diagnose a cap reached even while its own owner has headroom.
At ten existing leases and a limit of ten, the diagnostic succeeds with `10`;
an attempted new allocation would instead be checked as `11/10`. Successful
diagnostics exit 0 even at or above the cap. Invalid syntax/configuration and
request errors follow normal CLI error handling; errors go to stderr and data
go to stdout. Malformed successful responses fail instead of displaying zeros.

Unlike [monthly usage](usage.md), the count spans every creation month and org
for the exact, case-sensitive owner identity. Active and provisioning managed
leases count even after their canonical expiry, until a terminal state is
committed. Released, expired, failed, and registered inventory records do not
count. A live, unexpired admission reservation overrides its canonical lease
by ID and counts once; reservation-only IDs count too. Stale reservations are
ignored without cleanup, with canonical records used when present. Shared
leases owned by someone else do not count toward the owner dimension, but do
count toward the fleet and, where applicable, org dimensions. Direct-only leases
absent from the coordinator do not count.

This read-only snapshot uses the same serialized boundary as admission on
Cloudflare and Node. Normal service startup and background recovery retain their
existing behavior; the capacity handler itself neither allocates nor reconciles
resources, deletes stale records, schedules maintenance, nor calls providers.
Fleet or org cap exhaustion caused by other owners is reported as an aggregate
count only. The response reveals no other owners' identities, lease IDs, records,
per-owner breakdowns, or costs. These aggregates are a narrow exception to
owner/org visibility, not expanded access to lease records or monthly reports.

The snapshot is not a reservation or approval to allocate. Budget and provider
gates may still reject allocation, and any count can change after
`observedAt`. See [cost and usage](../features/cost-usage.md) and
[auth and routing](../features/broker-auth-routing.md).
