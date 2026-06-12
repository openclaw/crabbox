#!/usr/bin/env bash
set -euo pipefail

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  echo "set CRABBOX_LIVE=1 to run the W&B live smoke" >&2
  exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cb="${CRABBOX_BIN:-$root/bin/crabbox}"
repo="${CRABBOX_LIVE_REPO:-$PWD}"
case "$cb" in
  */*)
    cb_dir="$(cd "$(dirname "$cb")" && pwd)"
    cb="$cb_dir/$(basename "$cb")"
    ;;
esac

if [[ -z "${CRABBOX_WANDB_API_KEY:-${WANDB_API_KEY:-}}" ]]; then
  echo "wandb smoke requires CRABBOX_WANDB_API_KEY or WANDB_API_KEY" >&2
  exit 2
fi

if [[ -z "${WANDB_ENTITY_NAME:-}" ]]; then
  echo "wandb smoke requires WANDB_ENTITY_NAME" >&2
  exit 2
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "wandb smoke requires jq on PATH" >&2
  exit 2
fi

run_in_repo() {
  (cd "$repo" && "$@")
}

run_in_repo "$cb" doctor --provider wandb
run_in_repo "$cb" run \
  --provider wandb \
  --no-sync \
  --wandb-max-lifetime 60 \
  -- echo crabbox-wandb-ok
run_in_repo "$cb" list --provider wandb --json | jq 'map({id:(.id // .CloudID),provider:(.provider // .Provider),state:(.status // .state)})'
printf 'wandb live smoke complete\n'
