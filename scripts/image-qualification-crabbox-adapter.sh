#!/usr/bin/env bash
set -euo pipefail

: "${QUALIFICATION_REAL_CRABBOX:?missing real candidate CLI}"
: "${QUALIFICATION_ADAPTER_STATE:?missing adapter state directory}"
mkdir -p "$QUALIFICATION_ADAPTER_STATE"
chmod 700 "$QUALIFICATION_ADAPTER_STATE"

command_name="${1:-}"
if [[ "$command_name" == warmup ]]; then
  count_file="$QUALIFICATION_ADAPTER_STATE/launch-count"
  count=0
  [[ -f "$count_file" ]] && read -r count <"$count_file"
  count=$((count + 1))
  printf '%s\n' "$count" >"$count_file"
  chmod 600 "$count_file"
fi

status=0
"$QUALIFICATION_REAL_CRABBOX" "$@" || status=$?
[[ "$status" -eq 0 ]] || exit "$status"

count=0
[[ -f "$QUALIFICATION_ADAPTER_STATE/launch-count" ]] &&
  read -r count <"$QUALIFICATION_ADAPTER_STATE/launch-count"
if [[ "$command_name" == run && "$count" -eq 3 && ! -f "$QUALIFICATION_ADAPTER_STATE/injected" ]]; then
  printf 'after-promoted-smoke\n' >"$QUALIFICATION_ADAPTER_STATE/injected"
  chmod 600 "$QUALIFICATION_ADAPTER_STATE/injected"
  exit 86
fi
