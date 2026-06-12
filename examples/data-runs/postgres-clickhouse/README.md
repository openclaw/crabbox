# Postgres to ClickHouse Data Run

This example shows `crabbox data run` as a thin wrapper over caller-owned ETL
code. The script starts local Postgres and ClickHouse containers inside the
Crabbox box, seeds synthetic orders in Postgres, writes a daily aggregate into
ClickHouse, and emits bounded proof under `reports/data/postgres-clickhouse/`.

Crabbox does not act as a connector, scheduler, warehouse, or raw-data transport
layer here. It only runs the caller-owned command and validates/downloads the
bounded manifest.

## Run

Inspect the exact underlying command:

```sh
crabbox data run --dry-run postgres-clickhouse-demo
```

Run against an existing islo sandbox:

```sh
crabbox data run --id <sandbox-slug-or-id> postgres-clickhouse-demo
```

The manifest contains dataset identities, row counts, scalar metrics, and
artifact metadata. It intentionally does not contain raw rows, credentials,
tokens, signed URLs, or connector state.
