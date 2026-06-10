#!/usr/bin/env bash
set -euo pipefail

threshold="${1:-90.0}"
if [[ ! "$threshold" =~ ^([0-9]+([.][0-9]+)?|[.][0-9]+)$ ]]; then
  echo "invalid coverage threshold: $threshold (must be a number from 0 to 100)" >&2
  exit 2
fi
if ! awk -v threshold="$threshold" 'BEGIN { exit !(threshold >= 0 && threshold <= 100) }'; then
  echo "invalid coverage threshold: $threshold (must be a number from 0 to 100)" >&2
  exit 2
fi
if ! work_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-go-coverage.XXXXXX")"; then
  echo "could not create temporary coverage directory" >&2
  exit 1
fi
cleanup() {
  rm -rf "$work_dir"
}
trap cleanup EXIT
profile="$work_dir/coverage.out"
core_profile="$work_dir/core-coverage.out"

go test ./... -covermode=atomic -coverprofile="$profile"

awk '
  NR == 1 {
    print
    next
  }
  $1 ~ /^github.com\/openclaw\/crabbox\/internal\/cli\/(bootstrap|claim|config|errors|flags|fmt|init|provider_labels|runlog|slug)\.go:/ {
    print
  }
' "$profile" >"$core_profile"

coverage="$(go tool cover -func="$core_profile" | awk '/^total:/ { sub(/%/, "", $3); print $3 }')"
awk -v coverage="$coverage" -v threshold="$threshold" 'BEGIN {
  if (coverage + 0 < threshold + 0) {
    printf "Go core coverage %.1f%% is below %.1f%%\n", coverage, threshold
    exit 1
  }
  printf "Go core coverage %.1f%% >= %.1f%%\n", coverage, threshold
}'
