#!/usr/bin/env bash
set -Eeuo pipefail

smoke_root=""
sandbox_id=""
claim_path=""
claim_backup=""

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

cleanup() {
  local status=$?
  if [[ -z "$sandbox_id" && -n "$smoke_root" && -d "$XDG_STATE_HOME/crabbox/claims" ]]; then
    local claims=("$XDG_STATE_HOME"/crabbox/claims/*.json)
    if [[ "${#claims[@]}" -eq 1 && -f "${claims[0]}" ]]; then
      sandbox_id="${claims[0]##*/}"
      sandbox_id="${sandbox_id%.json}"
    fi
  fi
  if [[ -n "$claim_backup" && -e "$claim_backup" && -n "$claim_path" && ! -e "$claim_path" ]]; then
    mv -- "$claim_backup" "$claim_path"
  fi
  if [[ -n "$sandbox_id" ]]; then
    run_in_repo "$cb" stop --provider wandb --id "$sandbox_id" >/dev/null 2>&1 || true
  fi
  if [[ -n "$smoke_root" ]]; then
    rm -rf -- "$smoke_root"
  fi
  exit "$status"
}
trap cleanup EXIT

smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-wandb-smoke.XXXXXX")"
export XDG_STATE_HOME="$smoke_root/state"
shopt -s nullglob
lease_json="$smoke_root/lease.json"

run_in_repo "$cb" doctor --provider wandb
run_in_repo "$cb" run \
  --provider wandb \
  --no-sync \
  --keep \
  --lease-output "$lease_json" \
  --wandb-max-lifetime 60 \
  -- echo crabbox-wandb-ok
sandbox_id="$(jq -r '.leaseId // .LeaseID // .slug // empty' "$lease_json")"
if [[ -z "$sandbox_id" ]]; then
  echo "wandb smoke lease output did not contain a sandbox id" >&2
  exit 1
fi
claim_path="$XDG_STATE_HOME/crabbox/claims/$sandbox_id.json"
if [[ ! -f "$claim_path" ]]; then
  echo "wandb smoke did not persist the expected local claim" >&2
  exit 1
fi

run_in_repo "$cb" status --provider wandb --id "$sandbox_id" >/dev/null

# Prove the provider-side tag alone is insufficient. Restore the exact claim
# before cleanup regardless of how the denial assertion exits.
claim_backup="$smoke_root/claim.json"
mv -- "$claim_path" "$claim_backup"
set +e
denied_output="$(run_in_repo "$cb" stop --provider wandb --id "$sandbox_id" 2>&1)"
denied_status=$?
set -e
if [[ "$denied_status" -eq 0 || "$denied_output" != *"no matching local ownership claim"* ]]; then
  echo "wandb smoke expected tagged-but-unclaimed stop denial" >&2
  exit 1
fi
mv -- "$claim_backup" "$claim_path"
claim_backup=""
run_in_repo "$cb" status --provider wandb --id "$sandbox_id" >/dev/null

run_in_repo "$cb" run \
  --provider wandb \
  --no-sync \
  --id "$sandbox_id" \
  -- echo crabbox-wandb-reuse-ok
claim_backup="$smoke_root/claim.json"
cp -- "$claim_path" "$claim_backup"
run_in_repo "$cb" stop --provider wandb --id "$sandbox_id"
if [[ -e "$claim_path" ]]; then
  echo "wandb smoke stop left local claim residue" >&2
  exit 1
fi
if ! run_in_repo "$cb" list --provider wandb --json | jq -e --arg id "$sandbox_id" 'map(.CloudID // .id) | index($id) == null' >/dev/null; then
  echo "wandb smoke stop left active remote inventory residue" >&2
  exit 1
fi
rm -- "$claim_backup"
claim_backup=""
sandbox_id=""
printf 'wandb live smoke complete\n'
