# Postgres to ClickHouse Data Run

This example shows `crabbox data run` as an ETL contract smoke test for
caller-owned code. The script starts local Postgres and ClickHouse containers
inside the Crabbox box, seeds synthetic orders in Postgres, transforms them into
a `daily_sales_by_region` aggregate, writes the result into ClickHouse, and
emits bounded proof under `reports/data/postgres-clickhouse/`.

Crabbox does not act as a connector, scheduler, warehouse, or raw-data transport
layer here. It only runs the caller-owned command and validates/downloads the
bounded manifest.

The flow is:

```text
Postgres orders -> daily_sales_by_region transform -> ClickHouse daily_sales
```

This is the intended Data Runs use case: debug or heal a data pipeline by
running a small, repeatable contract smoke in an ephemeral box and emitting
bounded evidence. The same shape works for a migration, backfill, dbt/SQLMesh
model, Airbyte-style wrapper, or a failing production pipeline reduced to a
minimal fixture.

Data Runs should support several topologies without making Crabbox own them:

- `single-box-fixture`: source, transform, and destination all run inside one
  box, as this repeatable demo does.
- `box-to-services`: the command runs inside one box and connects to
  caller-owned services such as Postgres, ClickHouse Cloud, Snowflake, BigQuery,
  or S3 using caller-provided credentials.
- `service-to-service`: the command coordinates existing services and emits
  proof from the box, while raw data moves only between caller-owned systems.

The demo keeps the source and destination local so it is deterministic and needs
no secrets. Production callers can swap in their own private source and
destination access from inside the box while keeping the same manifest boundary.

## Run

Inspect the exact underlying command:

```sh
crabbox data run --dry-run postgres-clickhouse-demo
```

Run against an existing islo sandbox:

```sh
crabbox data run --id <sandbox-slug-or-id> postgres-clickhouse-demo
```

The manifest contains dataset identities, row counts, the named transform,
topology, use case, scalar metrics, and artifact metadata. It intentionally does
not contain raw rows, credentials, tokens, signed URLs, or connector state.
