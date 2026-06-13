#!/usr/bin/env bash
set -Eeuo pipefail

classification="diagnostic_only"
classification_emitted=0
invocation_dir="$PWD"
repo_root=""
bin=""
smoke_root=""
smoke_repo=""
slug="smolvm-live-smoke-$(date -u +%Y%m%d)-$(printf '%06x%06x' "$$" "$RANDOM")"
cleanup_needed=0
cleanup_retry_delay="${CRABBOX_SMOLVM_CLEANUP_RETRY_DELAY_SECONDS:-2}"

classify_and_exit() {
  trap - ERR
  if [[ $classification_emitted -ne 0 ]]; then
    exit 1
  fi
  classification_emitted=1
  classification="$1"
  reason="${2:-}"
  if [[ -n "$reason" ]]; then
    printf 'classification=%s reason=%s\n' "$classification" "$reason"
  else
    printf 'classification=%s\n' "$classification"
  fi
  case "$classification" in
    live_smolvm_smoke_passed|environment_blocked|quota_blocked) exit 0 ;;
    *) exit 1 ;;
  esac
}

classify_failure() {
  local output="$1"
  local reason="$2"
  if grep -Eiq 'quota|capacity|rate limit|too many requests|429|insufficient' <<<"$output"; then
    classify_and_exit quota_blocked "$reason"
  fi
  if grep -Eiq 'api key|unauthorized|forbidden|connection refused|no such host|timeout|timed out|TLS|x509' <<<"$output"; then
    classify_and_exit environment_blocked "$reason"
  fi
  classify_and_exit diagnostic_only "$reason"
}

unexpected_failure() {
  classify_and_exit diagnostic_only "unexpected_failure_line_$1"
}

inventory_has_slug() {
  local inventory
  if ! inventory="$("$bin" list --provider smolvm --json 2>/dev/null)"; then
    return 2
  fi
  if jq -e --arg slug "$slug" 'any(.[]; ((.slug // .Slug // .labels.slug // "") == $slug))' <<<"$inventory" >/dev/null 2>&1; then
    return 0
  fi
  if jq -e 'type == "array"' <<<"$inventory" >/dev/null 2>&1; then
    return 1
  fi
  return 2
}

stop_and_confirm() {
  local attempt
  local inventory_status
  for attempt in 1 2 3; do
    if inventory_has_slug; then
      inventory_status=0
    else
      inventory_status=$?
    fi
    if [[ $inventory_status -eq 1 ]]; then
      return 0
    fi
    "$bin" stop --provider smolvm "$slug" >/dev/null 2>&1 || true
    if [[ $attempt -lt 3 ]]; then
      sleep "$cleanup_retry_delay"
    fi
  done
  if inventory_has_slug; then
    inventory_status=0
  else
    inventory_status=$?
  fi
  [[ $inventory_status -eq 1 ]]
}

cleanup() {
  local status=$?
  trap - EXIT
  if [[ $cleanup_needed -eq 1 && -n "$bin" && -x "$bin" ]]; then
    if ! stop_and_confirm; then
      printf 'cleanup=failed provider=smolvm slug=%s attempts=3\n' "$slug" >&2
      status=1
    fi
  fi
  if [[ -n "$smoke_root" ]]; then
    rm -rf -- "$smoke_root"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'unexpected_failure "$LINENO"' ERR

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [[ "${CRABBOX_SMOLVM_LIVE_SMOKE:-0}" != "1" ]]; then
  classify_and_exit environment_blocked "set_CRABBOX_SMOLVM_LIVE_SMOKE=1"
fi

if [[ -z "${CRABBOX_SMOLVM_API_KEY:-${SMOLMACHINES_API_KEY:-${SMK_API_KEY:-}}}" ]]; then
  classify_and_exit environment_blocked "missing_smolvm_api_key"
fi

bin="${CRABBOX_BIN:-$repo_root/bin/crabbox}"
if [[ "$bin" != /* ]]; then
  bin="$invocation_dir/$bin"
fi
if [[ -z "${CRABBOX_BIN:-}" ]]; then
  mkdir -p "$(dirname "$bin")"
  go build -trimpath -o "$bin" ./cmd/crabbox
fi

smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-smolvm-smoke.XXXXXX")"
smoke_repo="$smoke_root/repo"
export XDG_STATE_HOME="$smoke_root/state"
export CRABBOX_SMOLVM_SMOKE_VALUE="forwarded-ok"

mkdir -p "$smoke_repo"
cd "$smoke_repo"
git init -q
git config user.email smoke@example.com
git config user.name "Crabbox SmolVM Smoke"
cat >.crabbox.yaml <<'EOF'
provider: smolvm
sync:
  delete: true
smolvm:
  image: alpine
  workdir: /workspace/crabbox
  cpus: 1
  memoryMB: 512
EOF
printf 'v1\n' >proof.txt
printf 'remove-me\n' >stale.txt
git add .crabbox.yaml proof.txt stale.txt
git commit -qm "test: seed SmolVM smoke fixture"

cleanup_needed=1
trap - ERR
if run_output="$("$bin" run --provider smolvm --keep --slug "$slug" --timing-json \
  --allow-env CRABBOX_SMOLVM_SMOKE_VALUE -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v1 && test -f stale.txt && test "$CRABBOX_SMOLVM_SMOKE_VALUE" = forwarded-ok && printf SMOLVM_SMOKE_V1_OK' 2>&1)"; then
  run_status=0
else
  run_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $run_status -ne 0 ]]; then
  classify_failure "$run_output" "initial_run_failed"
fi
if ! grep -q 'SMOLVM_SMOKE_V1_OK' <<<"$run_output" || ! grep -q '"provider":"smolvm"' <<<"$run_output"; then
  classify_and_exit diagnostic_only "initial_run_proof_incomplete"
fi

"$bin" status --provider smolvm --id "$slug" --wait --json >/dev/null
"$bin" list --provider smolvm --json |
  jq -e --arg slug "$slug" 'any(.[]; ((.slug // .Slug // .labels.slug // "") == $slug))' >/dev/null
"$bin" doctor --provider smolvm --json >/dev/null

printf 'v2\n' >proof.txt
printf 'second\n' >second.txt
git add proof.txt second.txt
git rm -q stale.txt
git commit -qm "test: update SmolVM smoke fixture"

trap - ERR
if reuse_output="$("$bin" run --provider smolvm --id "$slug" --timing-json -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v2 && test -f second.txt && test ! -e stale.txt && printf SMOLVM_SMOKE_V2_OK' 2>&1)"; then
  reuse_status=0
else
  reuse_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $reuse_status -ne 0 ]]; then
  classify_failure "$reuse_output" "reuse_run_failed"
fi
if ! grep -q 'SMOLVM_SMOKE_V2_OK' <<<"$reuse_output" || ! grep -q '"provider":"smolvm"' <<<"$reuse_output"; then
  classify_and_exit diagnostic_only "reuse_run_proof_incomplete"
fi

trap - ERR
if exit_output="$("$bin" run --provider smolvm --id "$slug" --no-sync -- \
  /bin/sh -lc 'printf SMOLVM_SMOKE_EXIT_23; exit 23' 2>&1)"; then
  exit_status=0
else
  exit_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $exit_status -ne 23 ]] || ! grep -q 'SMOLVM_SMOKE_EXIT_23' <<<"$exit_output"; then
  classify_and_exit diagnostic_only "exit_propagation_failed"
fi

if ! stop_and_confirm; then
  classify_and_exit diagnostic_only "lease_cleanup_unconfirmed"
fi
cleanup_needed=0

trap - EXIT
rm -rf -- "$smoke_root"
classify_and_exit live_smolvm_smoke_passed
