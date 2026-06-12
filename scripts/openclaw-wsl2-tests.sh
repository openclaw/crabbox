#!/usr/bin/env bash
set -euo pipefail

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  echo "set CRABBOX_LIVE=1 to create an AWS Windows WSL2 lease" >&2
  exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cb="${CRABBOX_BIN:-$root/bin/crabbox}"
repo="${CRABBOX_OPENCLAW_REPO:-${CRABBOX_LIVE_REPO:-}}"
lease="${CRABBOX_OPENCLAW_WSL2_ID:-}"
user_requested_slug="${CRABBOX_OPENCLAW_WSL2_SLUG:-}"
requested_slug="${user_requested_slug:-wsl2-tests-$(date +%H%M%S)-$RANDOM-$$}"
class="${CRABBOX_OPENCLAW_WSL2_CLASS:-beast}"
market="${CRABBOX_OPENCLAW_WSL2_MARKET:-on-demand}"
idle_timeout="${CRABBOX_OPENCLAW_WSL2_IDLE_TIMEOUT:-240m}"
hydrate_wait="${CRABBOX_OPENCLAW_WSL2_HYDRATE_WAIT:-45m}"
keep_alive="${CRABBOX_OPENCLAW_WSL2_KEEP_ALIVE_MINUTES:-240}"
stop_after="${CRABBOX_OPENCLAW_WSL2_STOP:-0}"
test_command="${CRABBOX_OPENCLAW_TEST_COMMAND:-corepack enable && pnpm install --frozen-lockfile && CI=1 NODE_OPTIONS=--max-old-space-size=4096 OPENCLAW_TEST_PROJECTS_PARALLEL=6 OPENCLAW_VITEST_MAX_WORKERS=1 pnpm test}"
crabbox_target_args=(
  --provider aws
  --target windows
  --windows-mode wsl2
)

if [[ -z "$repo" ]]; then
  echo "OpenClaw repo path is required" >&2
  echo "set CRABBOX_OPENCLAW_REPO=/path/to/openclaw or CRABBOX_LIVE_REPO=/path/to/openclaw" >&2
  exit 2
fi

if [[ ! -d "$repo/.git" ]]; then
  echo "OpenClaw repo not found: $repo" >&2
  echo "set CRABBOX_OPENCLAW_REPO=/path/to/openclaw or CRABBOX_LIVE_REPO=/path/to/openclaw" >&2
  exit 2
fi

run_in_repo() {
  (cd "$repo" && "$@")
}

run_crabbox_wsl2() {
  (
    cd "$repo"
    CRABBOX_PROVIDER=aws CRABBOX_TARGET=windows CRABBOX_WINDOWS_MODE=wsl2 "$cb" "$@"
  )
}

extract_lease_id_from_timing() {
  node -e 'let data = ""; process.stdin.on("data", c => data += c); process.stdin.on("end", () => { for (const line of data.trim().split(/\n/).reverse()) { try { const json = JSON.parse(line); if (json.leaseId) { console.log(json.leaseId); process.exit(0); } } catch {} } process.exit(1); });' 2>/dev/null
}

tmpdir="$(mktemp -d)"
cleanup_ref="$lease"
cleanup() {
  if [[ "$stop_after" == "1" && -n "$cleanup_ref" ]]; then
    run_crabbox_wsl2 stop "$cleanup_ref" || true
  fi
  rm -rf "$tmpdir"
}
trap cleanup EXIT

capture_leases() {
  local output="$1"
  run_crabbox_wsl2 list --json >"$output" 2>/dev/null
}

resolve_new_lease_from_list() {
  local before="$1"
  local after="$tmpdir/leases-after.json"
  capture_leases "$after" || return 1
  jq -r --arg slug "$requested_slug" --slurpfile before "$before" '
    def lease_id: (.labels.lease? // .leaseId // .id // "");
    def lease_slug: (.slug // (.labels.slug? // "") // "");
    (($before[0] // []) | map(lease_id)) as $beforeIDs
    | [
        .[]
        | select((lease_slug == $slug or (lease_slug | startswith($slug + "-"))) and ((lease_id as $id | $beforeIDs | index($id)) | not))
      ]
    | if length == 1 then .[0] | lease_id else empty end
  ' "$after"
}

if [[ -z "$lease" ]]; then
  before_leases="$tmpdir/leases-before.json"
  if ! capture_leases "$before_leases"; then
    echo "could not capture pre-warmup lease list; refusing to create WSL2 lease without cleanup baseline" >&2
    exit 1
  fi
  warmup_out="$tmpdir/warmup.out"
  warmup_err="$tmpdir/warmup.err"
  if run_in_repo "$cb" warmup \
    "${crabbox_target_args[@]}" \
    --slug "$requested_slug" \
    --class "$class" \
    --market "$market" \
    --idle-timeout "$idle_timeout" \
    --timing-json >"$warmup_out" 2>"$warmup_err"; then
    cat "$warmup_out"
    cat "$warmup_err" >&2
  else
    rc=$?
    cat "$warmup_out"
    cat "$warmup_err" >&2
    exit "$rc"
  fi
  lease="$(extract_lease_id_from_timing <"$warmup_err" || true)"
  if [[ -z "$lease" ]]; then
    if resolved_lease="$(resolve_new_lease_from_list "$before_leases")" && [[ -n "$resolved_lease" ]]; then
      cleanup_ref="$resolved_lease"
      echo "warmup succeeded but no lease id or allocated slug could be parsed; cleanup resolved new lease $resolved_lease from list" >&2
      exit 1
    else
      echo "warmup succeeded but no lease id or allocated slug could be parsed; refusing to stop unconfirmed requested slug $requested_slug" >&2
      exit 1
    fi
  fi
  cleanup_ref="$lease"
fi

run_crabbox_wsl2 actions hydrate \
  --id "$lease" \
  --wait-timeout "$hydrate_wait" \
  --keep-alive-minutes "$keep_alive" \
  --timing-json

run_crabbox_wsl2 run --id "$lease" --shell -- "$test_command"
