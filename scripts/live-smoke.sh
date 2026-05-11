#!/usr/bin/env bash
set -euo pipefail

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  echo "set CRABBOX_LIVE=1 to run live provider smoke tests" >&2
  exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cb="${CRABBOX_BIN:-$root/bin/crabbox}"
repo="${CRABBOX_LIVE_REPO:-$PWD}"
providers=",${CRABBOX_LIVE_PROVIDERS-aws,hetzner},"

run_in_repo() {
  (cd "$repo" && "$@")
}

capture_run() {
  local __name="$1"
  shift
  local __out
  if ! __out="$("$@" 2>&1)"; then
    printf '%s\n' "$__out"
    return 1
  fi
  printf -v "$__name" '%s' "$__out"
}

has_provider() {
  [[ "$providers" == *",$1,"* ]]
}

extract_lease() {
  rg -o '(cbx_[a-f0-9]{12}|sem_[A-Za-z0-9][A-Za-z0-9._-]*)' | head -1
}

extract_slug() {
  sed -n 's/.*slug=\([^ ]*\).*/\1/p' | rg -v '^-$' | head -1
}

stop_lease() {
  local id="$1"
  local slug="${2:-}"
  if [[ -n "$slug" ]]; then
    run_in_repo "$cb" stop "$slug" || run_in_repo "$cb" stop "$id" || true
  else
    run_in_repo "$cb" stop "$id" || true
  fi
}

stop_provider_lease() {
  local provider="$1"
  local id="$2"
  local slug="${3:-}"
  if [[ -n "$slug" ]]; then
    run_in_repo "$cb" stop --provider "$provider" "$slug" || run_in_repo "$cb" stop --provider "$provider" "$id" || true
  else
    run_in_repo "$cb" stop --provider "$provider" "$id" || true
  fi
}

provider_smoke() {
  local provider="$1"
  shift
  local lease=""
  local slug=""
  cleanup() {
    if [[ -n "$lease" ]]; then
      stop_lease "$lease" "$slug"
    fi
  }
  trap cleanup RETURN

  local out
  capture_run out run_in_repo "$cb" warmup --provider "$provider" "$@"
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  run_in_repo "$cb" status --id "$slug" --wait --wait-timeout 90s
  run_in_repo "$cb" inspect --id "$slug" --json | jq '{id,slug,provider,state,serverType,host,ready,lastTouchedAt,expiresAt}'
  run_in_repo "$cb" ssh --id "$slug"
  run_in_repo "$cb" cache stats --id "$slug" --json | jq 'if type=="array" then {items:length,kinds:[.[].kind]} else {keys:keys} end'

  local runout
  capture_run runout run_in_repo "$cb" run --id "$slug" --shell -- 'test -f package.json && printf crabbox-live-ok && printf " pwd=%s\n" "$PWD"'
  printf '%s\n' "$runout"
  local runid
  runid="$(printf '%s\n' "$runout" | rg -o 'run_[a-f0-9]{12}' | tail -1 || true)"
  run_in_repo "$cb" history --lease "$lease" --limit 5
  if [[ -n "$runid" ]]; then
    run_in_repo "$cb" logs "$runid" | tail -80
  fi
  stop_lease "$lease" "$slug"
  lease=""
}

blacksmith_smoke() {
  run_in_repo "$cb" list --provider blacksmith-testbox --json | jq '.[0] // empty'
  run_in_repo "$cb" run \
    --provider blacksmith-testbox \
    --blacksmith-org "${CRABBOX_BLACKSMITH_ORG:-openclaw}" \
    --blacksmith-workflow "${CRABBOX_BLACKSMITH_WORKFLOW:-.github/workflows/ci-check-testbox.yml}" \
    --blacksmith-job "${CRABBOX_BLACKSMITH_JOB:-check}" \
    --blacksmith-ref "${CRABBOX_BLACKSMITH_REF:-main}" \
    --idle-timeout "${CRABBOX_BLACKSMITH_IDLE_TIMEOUT:-10m}" \
    --shell -- 'echo blacksmith-crabbox-ok && pwd'
}

e2b_smoke() {
  local lease=""
  local slug=""
  cleanup() {
    if [[ -n "$lease" ]]; then
      stop_provider_lease e2b "$lease" "$slug"
    fi
  }
  trap cleanup RETURN

  local out
  capture_run out run_in_repo "$cb" warmup --provider e2b --e2b-template "${CRABBOX_E2B_TEMPLATE:-base}" --timing-json
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  run_in_repo "$cb" status --provider e2b --id "$slug" --wait
  run_in_repo "$cb" run --provider e2b --id "$slug" --no-sync -- echo crabbox-e2b-ok
  run_in_repo "$cb" list --provider e2b --json | jq 'map({id:(.id // .CloudID),slug:(.slug // .labels.slug),provider:(.provider // .Provider // .labels.provider),state:(.state // .labels.state // .status)})'
  stop_provider_lease e2b "$lease" "$slug"
  lease=""
}

daytona_smoke() {
  run_in_repo "$cb" run --provider daytona --no-sync -- echo crabbox-daytona-ok
  run_in_repo "$cb" list --provider daytona --json | jq 'map({id:(.id // .CloudID),slug:(.slug // .labels.slug),provider:(.provider // .Provider // .labels.provider),state:(.state // .labels.state // .status)})'
}

namespace_smoke() {
  if ! command -v devbox >/dev/null 2>&1; then
    echo "namespace-devbox smoke requires the Namespace devbox CLI on PATH" >&2
    return 2
  fi
  run_in_repo "$cb" run \
    --provider namespace-devbox \
    --namespace-size "${CRABBOX_NAMESPACE_SIZE:-S}" \
    --namespace-delete-on-release \
    --no-sync -- echo crabbox-namespace-ok
  run_in_repo "$cb" list --provider namespace-devbox --json | jq 'map({id:.id,slug:.slug,provider:.provider,state:.state})'
}

semaphore_smoke() {
  local lease=""
  local slug=""
  cleanup() {
    if [[ -n "$lease" ]]; then
      stop_provider_lease semaphore "$lease" "$slug"
    fi
  }
  trap cleanup RETURN

  local out
  capture_run out run_in_repo "$cb" warmup --provider semaphore --semaphore-idle-timeout "${CRABBOX_SEMAPHORE_IDLE_TIMEOUT:-10m}"
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  run_in_repo "$cb" status --provider semaphore --id "$slug" --wait --wait-timeout 120s
  run_in_repo "$cb" run --provider semaphore --id "$slug" --no-sync -- echo crabbox-semaphore-ok
  run_in_repo "$cb" list --provider semaphore --json | jq 'map({id:.id,slug:.slug,provider:.provider,state:.state})'
  stop_provider_lease semaphore "$lease" "$slug"
  lease=""
}

run_in_repo "$cb" whoami --json
run_in_repo "$cb" doctor
run_in_repo "$cb" sync-plan | sed -n '1,80p'

if has_provider aws; then
  provider_smoke aws --type "${CRABBOX_LIVE_AWS_TYPE:-t3.small}" --ttl 15m --idle-timeout 5m
fi

if has_provider hetzner; then
  provider_smoke hetzner --class "${CRABBOX_LIVE_HETZNER_CLASS:-standard}" --ttl 15m --idle-timeout 2m
fi

if has_provider blacksmith-testbox; then
  blacksmith_smoke
fi

if has_provider e2b; then
  e2b_smoke
fi

if has_provider daytona; then
  daytona_smoke
fi

if has_provider namespace-devbox || has_provider namespace; then
  namespace_smoke
fi

if has_provider semaphore; then
  semaphore_smoke
fi

admin_out="$(run_in_repo "$cb" admin leases --state active --json 2>&1)" || {
  printf 'warning: admin active-lease check skipped: %s\n' "$admin_out" >&2
  exit 0
}
printf '%s\n' "$admin_out" | jq 'length'
