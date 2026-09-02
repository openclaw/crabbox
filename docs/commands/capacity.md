# capacity

`crabbox capacity [--json]` reports the authenticated owner's current admission
count and effective owner limit. It requires a configured coordinator and uses
the normal coordinator credentials, including the existing resolved shared or
`unknown` owner identity. It never falls back to a direct provider or local lease
list. Older coordinators without `GET /v1/capacity` return an unsupported error.

```sh
crabbox capacity
crabbox capacity --json
```

The only option is `--json` (plus `--help`). There are no positional arguments or
owner, user, org, month, or scope selectors. Even admin requests report only the
owner resolved by normal authentication. The API rejects every query parameter
with HTTP 400.

```text
self-owner admission count: owner=github:12345 activeLeases=10
effective owner limit: 10
observed at: 2026-09-02T12:00:00.000Z
Snapshot only; not a reservation or approval to allocate.
```

JSON contains exactly four fields:

```json
{
  "owner": "github:12345",
  "activeLeases": 10,
  "effectiveLimit": 10,
  "observedAt": "2026-09-02T12:00:00.000Z"
}
```

`effectiveLimit: 0` means the selected owner limit is off; text prints `off`.
The selected limit uses the existing capacity-owner policy, independently of
admin authentication. A positive elevated limit for a configured member selects
the larger of ordinary and elevated limits, including when the ordinary limit
is zero. The response does not expose membership or the underlying limit config.

`activeLeases` counts existing admission entries only, with no candidate added.
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
leases owned by someone else and direct-only leases absent from the coordinator
do not count.

This read-only snapshot uses the same serialized boundary as admission on
Cloudflare and Node. It neither allocates nor reconciles resources, deletes stale
records, schedules maintenance, nor calls providers. It reveals no lease IDs,
records, org/provider breakdowns, costs, or other owners' counts. The aggregate
is a narrow exception to owner/org visibility, not expanded access to lease
records or monthly reports.

The snapshot is not a reservation or approval to allocate. Fleet, org, budget,
and provider gates may still reject allocation, and the count can change after
`observedAt`. See [cost and usage](../features/cost-usage.md) and
[auth and routing](../features/broker-auth-routing.md).
