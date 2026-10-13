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

if [[ $# -eq 1 || $# -eq 0 ]]; then
  go test -count=1 -timeout=20m ./... -covermode=atomic -coverprofile="$profile"
elif [[ $# -eq 3 && "$2" == --profiles ]]; then
  printf 'mode: atomic\n' >"$profile"
  for name in core cli-0 cli-1 cli-2; do
    input="$3/$name.out"
    if [[ ! -f "$input" ]] || [[ "$(head -n 1 "$input")" != 'mode: atomic' ]] ||
      ! awk 'NR > 1 && NF { found = 1 } END { exit !found }' "$input"; then
      echo "missing, empty, or non-atomic coverage profile: $name" >&2
      exit 1
    fi
    tail -n +2 "$input" >>"$profile"
  done
else
  echo "usage: $0 [threshold] [--profiles directory]" >&2
  exit 2
fi

# Go's parser merges duplicate atomic blocks. Validate the whole input before
# filtering, so corrupt records outside the gated files cannot disappear.
go tool cover -func="$profile" >/dev/null

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
