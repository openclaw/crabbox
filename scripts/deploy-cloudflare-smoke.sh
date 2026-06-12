#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SKIP_DEPLOY="${CRABBOX_CLOUDFLARE_SKIP_DEPLOY:-0}"
SKIP_SMOKE="${CRABBOX_CLOUDFLARE_SKIP_SMOKE:-0}"
cd "$ROOT"

run() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  "$@"
}

need_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    printf '%s is required\n' "$name" >&2
    exit 2
  fi
}

setup_crabbox_bin() {
  if [[ -z "${CRABBOX_BIN:-}" ]]; then
    CRABBOX_BIN="$ROOT/bin/crabbox"
    run go build -trimpath -o "$CRABBOX_BIN" ./cmd/crabbox
  elif [[ ! -x "$CRABBOX_BIN" ]]; then
    printf 'CRABBOX_BIN is not executable: %s\n' "$CRABBOX_BIN" >&2
    exit 2
  fi
}

local_checks() {
  run npm ci --prefix "$ROOT/worker"
  run npm run format:check --prefix "$ROOT/worker"
  run npm run lint --prefix "$ROOT/worker"
  run npm run check --prefix "$ROOT/worker"
  run npm test --prefix "$ROOT/worker"
  run "$ROOT/scripts/test-go-modules.sh"
  run npm run build:cloudflare --prefix "$ROOT/worker"
}

deploy_runner() {
  need_env CLOUDFLARE_ACCOUNT_ID
  need_env CLOUDFLARE_API_TOKEN
  need_env CRABBOX_CLOUDFLARE_RUNNER_TOKEN

  (
    cd "$ROOT/worker"
    printf '%s' "$CRABBOX_CLOUDFLARE_RUNNER_TOKEN" \
      | CLOUDFLARE_ACCOUNT_ID="$CLOUDFLARE_ACCOUNT_ID" \
        CLOUDFLARE_API_TOKEN="$CLOUDFLARE_API_TOKEN" \
        npx wrangler secret put CRABBOX_RUNNER_TOKEN --config wrangler.cloudflare.jsonc
    CLOUDFLARE_ACCOUNT_ID="$CLOUDFLARE_ACCOUNT_ID" \
      CLOUDFLARE_API_TOKEN="$CLOUDFLARE_API_TOKEN" \
      npm run deploy:cloudflare
  )
}

require_smoke_env() {
  need_env CRABBOX_CLOUDFLARE_RUNNER_URL
  need_env CRABBOX_CLOUDFLARE_RUNNER_TOKEN
  export CRABBOX_CLOUDFLARE_RUNNER_URL
  export CRABBOX_CLOUDFLARE_RUNNER_TOKEN
}

repo="${CRABBOX_LIVE_REPO:-$ROOT}"
lease_id=""
smoke_tmp_files=()
cleanup() {
  if ((${#smoke_tmp_files[@]} > 0)); then
    rm -f "${smoke_tmp_files[@]}"
  fi
  if [[ -n "$lease_id" ]]; then
    "$CRABBOX_BIN" stop --provider cloudflare "$lease_id" || true
  fi
}
trap cleanup EXIT

smoke_no_sync() {
  (
    cd "$repo"
    run "$CRABBOX_BIN" cleanup --provider cloudflare
    run "$CRABBOX_BIN" list --provider cloudflare --refresh --json
    run "$CRABBOX_BIN" run --provider cloudflare --type lite --no-sync --timing-json --shell -- \
      'set -eu; echo CRABBOX_CF_NO_SYNC_OK; pwd; uname -s; command -v go; command -v node; command -v gh; command -v rg'
  )
}

smoke_keep_stop() {
  local keep_out
  local keep_err
  local lease_candidate
  keep_out="$(mktemp)"
  keep_err="$(mktemp)"
  smoke_tmp_files+=("$keep_out" "$keep_err")
  local keep_status=0
  if (
    cd "$repo" || exit 2
    "$CRABBOX_BIN" run --provider cloudflare --type lite --keep --no-sync --timing-json --shell -- \
      'set -eu; echo CRABBOX_CF_KEEP_OK; sleep 1'
  ) >"$keep_out" 2>"$keep_err"; then
    keep_status=0
  else
    keep_status=$?
  fi
  cat "$keep_out"
  cat "$keep_err" >&2
  lease_id="$(sed -nE '/^[[:space:]]*\{/s/.*"leaseId"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' "$keep_err" | tail -1)"
  if [[ -z "$lease_id" ]]; then
    lease_candidate="$(sed -nE 's/^leased ([^[:space:]]+) slug=[^[:space:]]+ provider=cloudflare sandbox=[^[:space:]]+$/\1/p' "$keep_err" | tail -1)"
    if [[ -z "$lease_candidate" ]]; then
      lease_candidate="$(sed -nE 's/^leased ([^[:space:]]+) slug=[^[:space:]]+ provider=cloudflare sandbox=[^[:space:]]+$/\1/p' "$keep_out" | tail -1)"
    fi
    if [[ -n "$lease_candidate" ]]; then
      lease_id="$lease_candidate"
    fi
  fi
  rm -f "$keep_out" "$keep_err"
  if [[ "$keep_status" -ne 0 ]]; then
    return "$keep_status"
  fi
  if [[ -z "$lease_id" ]]; then
    printf 'could not parse kept Cloudflare lease id\n' >&2
    exit 3
  fi

  (
    cd "$repo"
    run "$CRABBOX_BIN" status --provider cloudflare --id "$lease_id" --wait --json
    run "$CRABBOX_BIN" stop --provider cloudflare "$lease_id"
    run "$CRABBOX_BIN" status --provider cloudflare --id "$lease_id" --json
  )
  lease_id=""
}

smoke_sync() {
  (
    cd "$repo"
    run "$CRABBOX_BIN" run --provider cloudflare --type basic --timing-json --shell -- \
      'set -eu; test -f go.mod; test -f internal/providers/cloudflare/backend.go; rg -n "stopped_with_code" internal/providers/cloudflare/backend.go internal/providers/cloudflare/backend_test.go; go env GOVERSION; node --version; gh --version'
    run "$CRABBOX_BIN" cleanup --provider cloudflare
    run "$CRABBOX_BIN" list --provider cloudflare --refresh --json
  )
}

main() {
  setup_crabbox_bin
  local_checks
  if [[ "$SKIP_DEPLOY" != "1" ]]; then
    deploy_runner
  fi
  if [[ "$SKIP_SMOKE" == "1" ]]; then
    printf 'cloudflare deploy complete; smoke skipped by CRABBOX_CLOUDFLARE_SKIP_SMOKE=1\n'
    return 0
  fi
  require_smoke_env
  smoke_no_sync
  smoke_keep_stop
  smoke_sync
  printf 'cloudflare deploy/smoke complete\n'
}

main "$@"
