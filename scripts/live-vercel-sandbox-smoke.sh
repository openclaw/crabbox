#!/usr/bin/env bash
set -Eeuo pipefail

classification="diagnostic_only"
classification_emitted=0
invocation_dir="$PWD"
repo_root=""
bin=""
smoke_root=""
smoke_repo=""
slug="vs-live-$(date -u +%Y%m%d)-$(printf '%06x%06x' "$$" "$RANDOM")"
cleanup_needed=0
cleanup_retry_delay="${CRABBOX_VERCEL_SANDBOX_CLEANUP_RETRY_DELAY_SECONDS:-2}"

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
    live_vercel_sandbox_smoke_passed|environment_blocked|quota_blocked) exit 0 ;;
    *) exit 1 ;;
  esac
}

classify_failure() {
  local output="$1"
  local reason="$2"
  print_debug_detail "$output"
  if grep -Eiq 'quota|capacity|admission|rate limit|too many requests|429|insufficient|limit exceeded|plan limit|resource limit|concurrency' <<<"$output"; then
    classify_and_exit quota_blocked "$reason"
  fi
  if grep -Eiq 'oidc|auth|token|unauthorized|forbidden|login|required|permission denied|connection refused|no such host|network|timeout|timed out|TLS|x509|certificate|missing @vercel/sandbox|cannot find package|module not found|sandbox CLI unavailable|not linked|project' <<<"$output"; then
    classify_and_exit environment_blocked "$reason"
  fi
  classify_and_exit diagnostic_only "$reason"
}

print_debug_detail() {
  if [[ "${CRABBOX_VERCEL_SANDBOX_SMOKE_DEBUG:-0}" != "1" ]]; then
    return 0
  fi
  local detail="$1"
  local secret
  for secret in \
    "${CRABBOX_VERCEL_SANDBOX_AUTH_TOKEN:-}" \
    "${CRABBOX_VERCEL_AUTH_TOKEN:-}" \
    "${VERCEL_AUTH_TOKEN:-}" \
    "${CRABBOX_VERCEL_SANDBOX_TOKEN:-}" \
    "${CRABBOX_VERCEL_TOKEN:-}" \
    "${VERCEL_TOKEN:-}" \
    "${CRABBOX_VERCEL_SANDBOX_OIDC_TOKEN:-}" \
    "${VERCEL_OIDC_TOKEN:-}"; do
    if [[ -n "$secret" ]]; then
      detail="${detail//$secret/[redacted]}"
    fi
  done
  detail="$(printf '%s' "$detail" |
    perl -0pe 's/"(access_token|token|authToken|oidcToken)"\s*:\s*"[^"]*"/"$1":"[redacted]"/gi; s/[[:space:]]+/ /g; s/(vercel_[A-Za-z0-9_=-]{12,})/[redacted]/g; s/(vct_[A-Za-z0-9_=-]{12,})/[redacted]/g' |
    cut -c 1-800)"
  if [[ -n "$detail" ]]; then
    printf 'debug_detail=%s\n' "$detail" >&2
  fi
}

unexpected_failure() {
  classify_and_exit diagnostic_only "unexpected_failure_line_$1"
}

need_tool() {
  if ! command -v "$1" >/dev/null 2>&1; then
    classify_and_exit environment_blocked "missing_required_tool_$1"
  fi
}

inventory_has_slug() {
  local inventory
  if ! inventory="$("$bin" list --provider vercel-sandbox --json 2>/dev/null)"; then
    return 2
  fi
  if jq -e --arg slug "$slug" 'any(.[]; ((.slug // .Slug // .labels.slug // .Labels.slug // "") == $slug))' <<<"$inventory" >/dev/null 2>&1; then
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
    "$bin" stop --provider vercel-sandbox "$slug" >/dev/null 2>&1 || true
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
      printf 'cleanup=failed provider=vercel-sandbox slug=%s attempts=3\n' "$slug" >&2
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

if [[ "${CRABBOX_LIVE:-0}" != "1" ]]; then
  classify_and_exit environment_blocked "set_CRABBOX_LIVE=1"
fi

providers=",${CRABBOX_LIVE_PROVIDERS:-},"
if [[ "$providers" != *",vercel-sandbox,"* ]]; then
  classify_and_exit environment_blocked "set_CRABBOX_LIVE_PROVIDERS=vercel-sandbox"
fi

need_tool git
need_tool jq
need_tool sandbox

trap - ERR
if sandbox_help_output="$(sandbox --help 2>&1)"; then
  sandbox_help_status=0
else
  sandbox_help_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $sandbox_help_status -ne 0 ]]; then
  classify_failure "$sandbox_help_output" "sandbox_help_failed"
fi

trap - ERR
if sandbox_list_output="$(sandbox list --all --limit 1 2>&1)"; then
  sandbox_list_status=0
else
  sandbox_list_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $sandbox_list_status -ne 0 ]]; then
  classify_failure "$sandbox_list_output" "sandbox_auth_preflight_failed"
fi

bin="${CRABBOX_BIN:-$repo_root/bin/crabbox}"
if [[ "$bin" != /* ]]; then
  bin="$invocation_dir/$bin"
fi
if [[ -z "${CRABBOX_BIN:-}" ]]; then
  mkdir -p "$(dirname "$bin")"
  go build -trimpath -o "$bin" ./cmd/crabbox
fi

smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-vercel-sandbox-smoke.XXXXXX")"
smoke_repo="$smoke_root/repo"
export XDG_STATE_HOME="$smoke_root/state"
export CRABBOX_VERCEL_SANDBOX_SMOKE_VALUE="forwarded-ok"

mkdir -p "$smoke_repo"
cd "$smoke_repo"
git init -q
git config user.email smoke@example.com
git config user.name "Crabbox Vercel Sandbox Smoke"
cat >.crabbox.yaml <<EOF
provider: vercel-sandbox
sync:
  delete: true
vercelSandbox:
  runtime: ${CRABBOX_VERCEL_SANDBOX_SMOKE_RUNTIME:-node24}
  workdir: /vercel/sandbox/crabbox
EOF
if [[ -n "${CRABBOX_VERCEL_SANDBOX_PROJECT_ID:-}" ]]; then
  printf '  projectId: %s\n' "$CRABBOX_VERCEL_SANDBOX_PROJECT_ID" >>.crabbox.yaml
fi
if [[ -n "${CRABBOX_VERCEL_SANDBOX_TEAM_ID:-}" ]]; then
  printf '  teamId: %s\n' "$CRABBOX_VERCEL_SANDBOX_TEAM_ID" >>.crabbox.yaml
fi
if [[ -n "${CRABBOX_VERCEL_SANDBOX_SCOPE:-}" ]]; then
  printf '  scope: %s\n' "$CRABBOX_VERCEL_SANDBOX_SCOPE" >>.crabbox.yaml
fi
printf 'v1\n' >proof.txt
printf 'remove-me\n' >stale.txt
git add .crabbox.yaml proof.txt stale.txt
git commit -qm "test: seed Vercel Sandbox smoke fixture"

trap - ERR
if doctor_output="$("$bin" doctor --provider vercel-sandbox --json 2>&1)"; then
  doctor_status=0
else
  doctor_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $doctor_status -ne 0 ]]; then
  classify_failure "$doctor_output" "doctor_failed"
fi

cleanup_needed=1
trap - ERR
if run_output="$("$bin" run --provider vercel-sandbox --keep --slug "$slug" --timing-json \
  --allow-env CRABBOX_VERCEL_SANDBOX_SMOKE_VALUE -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v1 && test -f stale.txt && test "$CRABBOX_VERCEL_SANDBOX_SMOKE_VALUE" = forwarded-ok && printf VERCEL_SANDBOX_SMOKE_STDOUT && printf VERCEL_SANDBOX_SMOKE_STDERR >&2 && printf VERCEL_SANDBOX_SMOKE_V1_OK' 2>&1)"; then
  run_status=0
else
  run_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $run_status -ne 0 ]]; then
  classify_failure "$run_output" "initial_run_failed"
fi
if ! grep -q 'VERCEL_SANDBOX_SMOKE_V1_OK' <<<"$run_output" || ! grep -q 'VERCEL_SANDBOX_SMOKE_STDOUT' <<<"$run_output" || ! grep -q 'VERCEL_SANDBOX_SMOKE_STDERR' <<<"$run_output"; then
  classify_and_exit diagnostic_only "initial_run_proof_incomplete"
fi
if ! grep -q '"provider":"vercel-sandbox"' <<<"$run_output"; then
  classify_and_exit diagnostic_only "initial_timing_provider_missing"
fi

"$bin" status --provider vercel-sandbox --id "$slug" --wait --json >/dev/null
"$bin" list --provider vercel-sandbox --json |
  jq -e --arg slug "$slug" 'any(.[]; ((.slug // .Slug // .labels.slug // .Labels.slug // "") == $slug))' >/dev/null

printf 'v2\n' >proof.txt
printf 'second\n' >second.txt
git add proof.txt second.txt
git rm -q stale.txt
git commit -qm "test: update Vercel Sandbox smoke fixture"

trap - ERR
if reuse_output="$("$bin" run --provider vercel-sandbox --id "$slug" --timing-json -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v2 && test -f second.txt && test ! -e stale.txt && printf VERCEL_SANDBOX_SMOKE_V2_OK' 2>&1)"; then
  reuse_status=0
else
  reuse_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $reuse_status -ne 0 ]]; then
  classify_failure "$reuse_output" "reuse_run_failed"
fi
if ! grep -q 'VERCEL_SANDBOX_SMOKE_V2_OK' <<<"$reuse_output" || ! grep -q '"provider":"vercel-sandbox"' <<<"$reuse_output"; then
  classify_and_exit diagnostic_only "reuse_run_proof_incomplete"
fi

trap - ERR
if exit_output="$("$bin" run --provider vercel-sandbox --id "$slug" --no-sync -- \
  /bin/sh -lc 'printf VERCEL_SANDBOX_SMOKE_EXIT_23; exit 23' 2>&1)"; then
  exit_status=0
else
  exit_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $exit_status -ne 23 ]] || ! grep -q 'VERCEL_SANDBOX_SMOKE_EXIT_23' <<<"$exit_output"; then
  classify_and_exit diagnostic_only "exit_propagation_failed"
fi

if ! stop_and_confirm; then
  classify_and_exit diagnostic_only "lease_cleanup_unconfirmed"
fi
cleanup_needed=0
printf 'cleanup=confirmed provider=vercel-sandbox slug=%s\n' "$slug"

trap - EXIT
rm -rf -- "$smoke_root"
classify_and_exit live_vercel_sandbox_smoke_passed
