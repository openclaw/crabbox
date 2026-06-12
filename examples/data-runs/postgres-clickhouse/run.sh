#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
DEMO_DIR="$ROOT/examples/data-runs/postgres-clickhouse"
REPORT_DIR="$ROOT/reports/data/postgres-clickhouse"
PROJECT="crabbox-data-run-demo"

mkdir -p "$REPORT_DIR"

cleanup() {
  docker compose -p "$PROJECT" -f "$DEMO_DIR/compose.yaml" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker compose -p "$PROJECT" -f "$DEMO_DIR/compose.yaml" up -d --wait >/dev/null

for _ in $(seq 1 30); do
  if docker compose -p "$PROJECT" -f "$DEMO_DIR/compose.yaml" exec -T clickhouse clickhouse-client \
    --user crabbox_demo --password crabbox_demo --database crabbox_demo --query 'SELECT 1' >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

docker compose -p "$PROJECT" -f "$DEMO_DIR/compose.yaml" exec -T postgres psql -v ON_ERROR_STOP=1 -U crabbox_demo -d crabbox_demo >/dev/null <<'SQL'
SET client_min_messages = warning;
DROP TABLE IF EXISTS orders;
CREATE TABLE orders (
  order_id integer PRIMARY KEY,
  order_date date NOT NULL,
  region text NOT NULL,
  amount_cents integer NOT NULL
);
INSERT INTO orders(order_id, order_date, region, amount_cents)
SELECT n,
       DATE '2026-06-01' + ((n - 1) % 3),
       CASE WHEN n % 2 = 0 THEN 'eu' ELSE 'us' END,
       1000 + (n * 37)
FROM generate_series(1, 48) AS n;
SQL

docker compose -p "$PROJECT" -f "$DEMO_DIR/compose.yaml" exec -T clickhouse clickhouse-client \
  --user crabbox_demo --password crabbox_demo --database crabbox_demo --multiquery >/dev/null <<'SQL'
DROP TABLE IF EXISTS daily_sales;
CREATE TABLE daily_sales (
  order_date Date,
  region LowCardinality(String),
  orders UInt64,
  amount_cents UInt64
) ENGINE = Memory;
SQL

docker compose -p "$PROJECT" -f "$DEMO_DIR/compose.yaml" exec -T postgres psql -v ON_ERROR_STOP=1 -U crabbox_demo -d crabbox_demo -At -F, <<'SQL' \
  | docker compose -p "$PROJECT" -f "$DEMO_DIR/compose.yaml" exec -T clickhouse clickhouse-client \
      --user crabbox_demo --password crabbox_demo --database crabbox_demo \
      --query "INSERT INTO daily_sales FORMAT CSV"
SELECT order_date, region, count(*) AS orders, sum(amount_cents) AS amount_cents
FROM orders
GROUP BY order_date, region
ORDER BY order_date, region;
SQL

SOURCE_ROWS="$(docker compose -p "$PROJECT" -f "$DEMO_DIR/compose.yaml" exec -T postgres psql -v ON_ERROR_STOP=1 -U crabbox_demo -d crabbox_demo -At -c 'SELECT count(*) FROM orders')"
SINK_ROWS="$(docker compose -p "$PROJECT" -f "$DEMO_DIR/compose.yaml" exec -T clickhouse clickhouse-client --user crabbox_demo --password crabbox_demo --database crabbox_demo --query 'SELECT sum(orders) FROM daily_sales' | tr -d '[:space:]')"
AGG_ROWS="$(docker compose -p "$PROJECT" -f "$DEMO_DIR/compose.yaml" exec -T clickhouse clickhouse-client --user crabbox_demo --password crabbox_demo --database crabbox_demo --query 'SELECT count() FROM daily_sales' | tr -d '[:space:]')"
TOTAL_CENTS="$(docker compose -p "$PROJECT" -f "$DEMO_DIR/compose.yaml" exec -T clickhouse clickhouse-client --user crabbox_demo --password crabbox_demo --database crabbox_demo --query 'SELECT sum(amount_cents) FROM daily_sales' | tr -d '[:space:]')"

if [[ "$SOURCE_ROWS" != "$SINK_ROWS" ]]; then
  echo "row-count mismatch: source=$SOURCE_ROWS sink=$SINK_ROWS" >&2
  exit 1
fi

cat > "$REPORT_DIR/quality.json" <<'JSON'
{
  "checks": [
    {
      "name": "source_rows_match_sink_rows",
      "status": "pass"
    },
    {
      "name": "aggregate_rows_present",
      "status": "pass"
    }
  ],
  "schemaVersion": "crabbox.data-quality.v1"
}
JSON
QUALITY_BYTES="$(wc -c < "$REPORT_DIR/quality.json" | tr -d '[:space:]')"

cat > "$REPORT_DIR/manifest.json" <<JSON
{
  "artifacts": [
    {
      "bytes": $QUALITY_BYTES,
      "kind": "quality",
      "path": "reports/data/postgres-clickhouse/quality.json"
    }
  ],
  "inputs": [
    {
      "identity": "demo:postgres.orders",
      "name": "postgres.orders",
      "rows": $SOURCE_ROWS
    }
  ],
  "name": "postgres-clickhouse-demo",
  "outputs": [
    {
      "identity": "demo:clickhouse.daily_sales",
      "name": "clickhouse.daily_sales",
      "rows": $SINK_ROWS
    }
  ],
  "policy": {
    "egress": "local-compose-only",
    "enforcement": "declared-only",
    "sinkIdentity": "demo:clickhouse.daily_sales",
    "sourceIdentity": "demo:postgres.orders"
  },
  "promotion": {
    "enforcement": "declared-only",
    "mode": "manual",
    "target": "demo-only"
  },
  "schemaVersion": "crabbox.data-run.v1",
  "status": "success",
  "summary": {
    "aggregateRows": $AGG_ROWS,
    "totalAmountCents": $TOTAL_CENTS
  }
}
JSON

echo "postgres-clickhouse data-run proof written to $REPORT_DIR/manifest.json"
