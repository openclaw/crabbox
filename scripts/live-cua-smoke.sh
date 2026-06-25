#!/usr/bin/env bash
set -Eeuo pipefail

classification_emitted=0
repo_root=""
bin=""
smoke_root=""
smoke_repo=""
slug="crabbox-cua-smoke-$$"
sandbox_maybe_created=0

classify_and_exit() {
  trap - ERR
  if [[ $classification_emitted -ne 0 ]]; then
    exit 1
  fi
  local classification="$1"
  local message="${2:-}"
  local cleanup_output=""
  if [[ $sandbox_maybe_created -ne 0 ]]; then
    if cleanup_output="$(cleanup_created_sandbox 2>&1)"; then
      sandbox_maybe_created=0
    else
      classification="cleanup_failed"
      message="slug=$slug $(redact "$cleanup_output")"
    fi
  fi
  classification_emitted=1
  if [[ -n "$message" ]]; then
    printf '%s %s\n' "$classification" "$message"
  else
    printf '%s\n' "$classification"
  fi
  case "$classification" in
    live_cua_smoke_passed|skipped|environment_blocked|quota_blocked|diagnostic_only) exit 0 ;;
    validation_failed|cleanup_failed) exit 1 ;;
    *) exit 1 ;;
  esac
}

classify_unexpected_failure() {
  local status="$1"
  local line="$2"
  classify_and_exit diagnostic_only "unexpected_failure status=$status line=$line"
}

classify_failure_output() {
  local output="$1"
  if grep -Eiq 'quota|capacity|rate limit|too many requests|429|insufficient' <<<"$output"; then
    classify_and_exit quota_blocked "$(redact "$output")"
  fi
  if grep -Eiq 'api key|unauthorized|forbidden|permission|connection refused|no such host|timeout|TLS|x509|python|import|module' <<<"$output"; then
    classify_and_exit environment_blocked "$(redact "$output")"
  fi
  if grep -Eiq 'validation|invalid|unsupported|missing command|workdir|target' <<<"$output"; then
    classify_and_exit validation_failed "$(redact "$output")"
  fi
  classify_and_exit diagnostic_only "$(redact "$output")"
}

redact() {
  sed -E 's/(CRABBOX_CUA_API_KEY|CUA_API_KEY)=([^[:space:]]+)/\1=[redacted]/g; s/(cua_[A-Za-z0-9._-]{8,})/[redacted]/g; s/(sk-[A-Za-z0-9._-]{8,})/[redacted]/g' <<<"$1"
}

cleanup() {
  cleanup_created_sandbox >/dev/null 2>&1 || true
  if [[ -n "$smoke_root" ]]; then
    rm -rf -- "$smoke_root"
  fi
}

cleanup_created_sandbox() {
  if [[ $sandbox_maybe_created -eq 0 || -z "$bin" || ! -x "$bin" ]]; then
    return 0
  fi
  trap - ERR
  "$bin" stop --provider cua --cua-forget-missing "$slug"
  local cleanup_status=$?
  trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
  if [[ $cleanup_status -eq 0 ]]; then
    sandbox_maybe_created=0
  fi
  return "$cleanup_status"
}
trap cleanup EXIT
trap 'classify_unexpected_failure "$?" "$LINENO"' ERR

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [[ "${CRABBOX_CUA_LIVE:-0}" != "1" ]]; then
  classify_and_exit skipped "reason=missing_CRABBOX_CUA_LIVE"
fi

if [[ -z "${CRABBOX_CUA_API_KEY:-${CUA_API_KEY:-}}" ]]; then
  classify_and_exit environment_blocked "reason=missing_CRABBOX_CUA_API_KEY_or_CUA_API_KEY"
fi

bin="${CRABBOX_BIN:-$repo_root/bin/crabbox}"
smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-cua-smoke.XXXXXX")"
smoke_repo="$smoke_root/repo"
export XDG_STATE_HOME="$smoke_root/state"
export CRABBOX_CUA_SMOKE_VALUE="forwarded-ok"

mkdir -p "$(dirname "$bin")"
go build -trimpath -o "$bin" ./cmd/crabbox

mkdir -p "$smoke_repo"
cd "$smoke_repo"
git init -q
git config user.email smoke@example.com
git config user.name "Crabbox CUA Smoke"
printf 'provider: cua\nsync:\n  delete: true\n' >.crabbox.yaml
printf 'v1\n' >proof.txt
printf 'remove-me\n' >stale.txt
git add .crabbox.yaml proof.txt stale.txt
git commit -qm "test: seed CUA smoke fixture"

trap - ERR
if doctor_output="$("$bin" doctor --provider cua --json 2>&1)"; then
  doctor_status=0
else
  doctor_status=$?
fi
trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
if [[ $doctor_status -ne 0 ]]; then
  classify_failure_output "$doctor_output"
fi

trap - ERR
if warmup_output="$("$bin" warmup --provider cua --slug "$slug" --timing-json 2>&1)"; then
  warmup_status=0
else
  warmup_status=$?
fi
trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
if [[ $warmup_status -ne 0 ]]; then
  classify_failure_output "$warmup_output"
fi
sandbox_maybe_created=1

"$bin" status --provider cua --id "$slug" --wait --wait-timeout 300s >/dev/null
"$bin" list --provider cua --json >/dev/null

trap - ERR
if run_output="$("$bin" run --provider cua --id "$slug" --timing-json \
  --allow-env CRABBOX_CUA_SMOKE_VALUE -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v1 && test -f stale.txt && test "$CRABBOX_CUA_SMOKE_VALUE" = forwarded-ok && printf CUA_SMOKE_V1_OK' 2>&1)"; then
  run_status=0
else
  run_status=$?
fi
trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
if [[ $run_status -ne 0 ]]; then
  classify_failure_output "$run_output"
fi
if ! grep -q 'CUA_SMOKE_V1_OK' <<<"$run_output" || ! grep -q '"syncDelegated":true' <<<"$run_output"; then
  classify_and_exit diagnostic_only "initial run succeeded but sync proof was incomplete"
fi

printf 'v2\n' >proof.txt
printf 'second\n' >second.txt
git add proof.txt second.txt
git rm -q stale.txt

trap - ERR
if reuse_output="$("$bin" run --provider cua --id "$slug" --timing-json -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v2 && test -f second.txt && test ! -e stale.txt && printf CUA_SMOKE_V2_OK' 2>&1)"; then
  reuse_status=0
else
  reuse_status=$?
fi
trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
if [[ $reuse_status -ne 0 ]]; then
  classify_failure_output "$reuse_output"
fi
if ! grep -q 'CUA_SMOKE_V2_OK' <<<"$reuse_output" || ! grep -q '"syncDelegated":true' <<<"$reuse_output"; then
  classify_and_exit diagnostic_only "reuse run succeeded but replacement proof was incomplete"
fi

"$bin" stop --provider cua "$slug" >/dev/null
sandbox_maybe_created=0
trap - EXIT
rm -rf -- "$smoke_root"

classify_and_exit live_cua_smoke_passed
