#!/usr/bin/env bash
set -Eeuo pipefail

classification_emitted=0
lease_created=0
slug="lambda-microvm-live-$(date -u +%Y%m%d)-$$"
root=""
bin=""
smoke_root=""

classify_and_exit() {
  trap - ERR
  classification_emitted=1
  printf 'classification=%s reason=%s\n' "$1" "$2"
  case "$1" in
    live_aws_lambda_microvm_smoke_passed|environment_blocked|quota_blocked) exit 0 ;;
    *) exit 1 ;;
  esac
}

classify_failure() {
  local output="$1"
  local reason="$2"
  if grep -Eiq 'quota|capacity|rate.?limit|too many requests|429|limit exceeded|concurrent' <<<"$output"; then
    classify_and_exit quota_blocked "$reason"
  fi
  if grep -Eiq 'credential|unauthorized|forbidden|access denied|expired|not found|unsupported|endpoint|no such host|network|timeout|timed out|TLS|x509|certificate' <<<"$output"; then
    classify_and_exit environment_blocked "$reason"
  fi
  classify_and_exit diagnostic_only "$reason"
}

cleanup() {
  if [[ $lease_created -eq 1 && -n "$bin" && -x "$bin" ]]; then
    "$bin" stop --provider aws-lambda-microvm --aws-lambda-microvm-forget-missing "$slug" >/dev/null 2>&1 || true
  fi
  if [[ -n "$smoke_root" ]]; then
    rm -rf -- "$smoke_root"
  fi
}

unexpected_failure() {
  local status="$1"
  local line="$2"
  if [[ $classification_emitted -eq 0 ]]; then
    classify_and_exit diagnostic_only "unexpected_failure_status_${status}_line_${line}"
  fi
}

trap cleanup EXIT
trap 'unexpected_failure "$?" "$LINENO"' ERR

if [[ "$#" -gt 1 ]]; then
  printf 'live AWS Lambda MicroVM smoke accepts at most one argument\n' >&2
  exit 2
fi
case "${1:-}" in
  "")
    ;;
  --dry-run)
    bin="${CRABBOX_BIN:-./bin/crabbox}"
    printf 'classification=dry_run provider=aws-lambda-microvm mutation=false\n'
    printf 'command=%s doctor --provider aws-lambda-microvm --json\n' "$bin"
    exit 0
    ;;
  *)
    printf 'unknown argument: %s\n' "$1" >&2
    exit 2
    ;;
esac

if [[ "${CRABBOX_LIVE:-0}" != "1" ]]; then
  classify_and_exit environment_blocked set_CRABBOX_LIVE=1
fi
case ",${CRABBOX_LIVE_PROVIDERS:-}," in
  *,aws-lambda-microvm,*) ;;
  *) classify_and_exit environment_blocked set_CRABBOX_LIVE_PROVIDERS=aws-lambda-microvm ;;
esac
if [[ -z "${CRABBOX_AWS_LAMBDA_MICROVM_IMAGE:-}" ]]; then
  classify_and_exit environment_blocked missing_CRABBOX_AWS_LAMBDA_MICROVM_IMAGE
fi
if ! command -v jq >/dev/null 2>&1; then
  classify_and_exit environment_blocked missing_required_tool_jq
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
if [[ -n "${CRABBOX_BIN:-}" ]]; then
  bin="$CRABBOX_BIN"
  if [[ ! -x "$bin" ]]; then
    classify_and_exit environment_blocked CRABBOX_BIN_not_executable
  fi
else
  bin="$root/bin/crabbox"
  mkdir -p "$(dirname "$bin")"
  go build -trimpath -o "$bin" ./cmd/crabbox
fi

smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-lambda-microvm-smoke.XXXXXX")"
export XDG_STATE_HOME="$smoke_root/state"
mkdir -p "$smoke_root/repo"
cd "$smoke_root/repo"
git init -q
git config user.email smoke@example.com
git config user.name "Crabbox Lambda MicroVM Smoke"
printf 'lambda-microvm-sync-proof\n' >proof.txt
git add proof.txt
git commit -qm "test: seed Lambda MicroVM smoke"

trap - ERR
if doctor_output="$("$bin" doctor --provider aws-lambda-microvm --json 2>&1)"; then
  doctor_status=0
else
  doctor_status=$?
fi
trap 'unexpected_failure "$?" "$LINENO"' ERR
if [[ $doctor_status -ne 0 ]]; then
  classify_failure "$doctor_output" doctor_failed
fi

trap - ERR
if run_output="$("$bin" run --provider aws-lambda-microvm --keep --slug "$slug" --shell 'test "$(cat proof.txt)" = lambda-microvm-sync-proof && printf LAMBDA_MICROVM_SYNC_OK' 2>&1)"; then
  run_status=0
else
  run_status=$?
fi
trap 'unexpected_failure "$?" "$LINENO"' ERR
if [[ $run_status -ne 0 ]]; then
  if "$bin" list --provider aws-lambda-microvm --json 2>/dev/null | jq -e --arg slug "$slug" 'any(.[]; ((.name // .Name // .slug // .Slug // .labels.slug // "") == $slug))' >/dev/null; then
    lease_created=1
  fi
  classify_failure "$run_output" initial_run_failed
fi
lease_created=1
if ! grep -q LAMBDA_MICROVM_SYNC_OK <<<"$run_output"; then
  classify_and_exit diagnostic_only archive_sync_proof_missing
fi

status_json="$("$bin" status --provider aws-lambda-microvm --id "$slug" --wait --json)"
if ! jq -e '((.state // .State // "") | ascii_downcase) == "running"' <<<"$status_json" >/dev/null; then
  classify_and_exit diagnostic_only running_status_proof_missing
fi
if ! "$bin" list --provider aws-lambda-microvm --json | jq -e --arg slug "$slug" 'any(.[]; ((.name // .Name // .slug // .Slug // .labels.slug // "") == $slug))' >/dev/null; then
  classify_and_exit diagnostic_only inventory_proof_missing
fi

reuse_output="$("$bin" run --provider aws-lambda-microvm --id "$slug" --no-sync -- printf LAMBDA_MICROVM_REUSE_OK)"
if ! grep -q LAMBDA_MICROVM_REUSE_OK <<<"$reuse_output"; then
  classify_and_exit diagnostic_only retained_reuse_proof_missing
fi

"$bin" pause --provider aws-lambda-microvm "$slug" >/dev/null
paused_json="$("$bin" status --provider aws-lambda-microvm --id "$slug" --json)"
if ! jq -e '((.state // .State // "") | ascii_downcase) == "suspended"' <<<"$paused_json" >/dev/null; then
  classify_and_exit diagnostic_only pause_proof_missing
fi
"$bin" resume --provider aws-lambda-microvm "$slug" >/dev/null
resumed_json="$("$bin" status --provider aws-lambda-microvm --id "$slug" --wait --json)"
if ! jq -e '((.state // .State // "") | ascii_downcase) == "running"' <<<"$resumed_json" >/dev/null; then
  classify_and_exit diagnostic_only resume_proof_missing
fi

"$bin" stop --provider aws-lambda-microvm "$slug" >/dev/null
lease_created=0
if "$bin" list --provider aws-lambda-microvm --json | jq -e --arg slug "$slug" 'any(.[]; ((.name // .Name // .slug // .Slug // .labels.slug // "") == $slug))' >/dev/null; then
  classify_and_exit diagnostic_only cleanup_proof_failed
fi

trap - EXIT
rm -rf -- "$smoke_root"
smoke_root=""
classify_and_exit live_aws_lambda_microvm_smoke_passed lifecycle_complete
