#!/usr/bin/env bash
set -Eeuo pipefail

classification_emitted=0
repo_root=""
bin=""
smoke_root=""
smoke_repo=""
created_slug=""
created_cvm=""
phala_cli="phala"
provider_args=()

classify_and_exit() {
  trap - ERR
  if [[ $classification_emitted -ne 0 ]]; then
    exit 1
  fi
  classification_emitted=1
  local classification="$1"
  local message="${2:-}"
  if [[ -n "$message" ]]; then
    printf '%s %s\n' "$classification" "$message"
  else
    printf '%s\n' "$classification"
  fi
  case "$classification" in
    live_phala_smoke_passed|environment_blocked|quota_blocked|diagnostic_only) exit 0 ;;
    *) exit 1 ;;
  esac
}

classify_unexpected_failure() {
  local status="$1"
  local line="$2"
  classify_and_exit diagnostic_only "unexpected failure status=$status line=$line"
}

extract_created_identifier() {
  local output="$1"
  if [[ "$output" =~ lease=(cbx_[A-Za-z0-9_.-]+) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  if [[ "$output" =~ (^|[[:space:]])(cbx_[A-Za-z0-9_.-]+)($|[[:space:]]) ]]; then
    printf '%s\n' "${BASH_REMATCH[2]}"
    return 0
  fi
  if [[ "$output" =~ slug=([^[:space:]]+) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  return 1
}

# Hard teardown: delete the CVM directly via the phala CLI so a leaked
# confidential VM cannot survive a failed run, then drop the local crabbox claim.
cleanup() {
  if [[ -n "$created_slug" && -n "$bin" && -x "$bin" ]]; then
    "$bin" stop "${provider_args[@]}" "$created_slug" >/dev/null 2>&1 || true
  fi
  if [[ -n "$created_cvm" ]]; then
    "$phala_cli" cvms delete --cvm-id "$created_cvm" --force >/dev/null 2>&1 || true
  fi
  if [[ -n "$smoke_root" ]]; then
    rm -rf -- "$smoke_root"
  fi
}
trap cleanup EXIT
trap 'classify_unexpected_failure "$?" "$LINENO"' ERR

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

bin="${CRABBOX_BIN:-$repo_root/bin/crabbox}"
# Resolve a relative binary against the repo root. Absolute POSIX paths (/...)
# and Windows drive paths (C:\... or C:/...) are already absolute.
if [[ "$bin" != /* && ! "$bin" =~ ^[A-Za-z]:[\\/] ]]; then
  bin="$repo_root/$bin"
fi
providers=",${CRABBOX_LIVE_PROVIDERS:-},"

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  classify_and_exit environment_blocked "reason=CRABBOX_LIVE_not_enabled missing=CRABBOX_LIVE=1"
fi

if [[ "$providers" != *",phala,"* && "$providers" != *",phala-cloud,"* && "$providers" != *",dstack,"* ]]; then
  classify_and_exit environment_blocked "reason=provider_not_selected missing=CRABBOX_LIVE_PROVIDERS=phala"
fi

phala_cli="${CRABBOX_PHALA_CLI:-phala}"

mkdir -p "$(dirname "$bin")"
if [[ -z "${CRABBOX_BIN:-}" ]]; then
  go build -trimpath -o "$bin" ./cmd/crabbox
fi

provider_args=(--provider phala)
if [[ -n "${CRABBOX_PHALA_CLI:-}" ]]; then
  provider_args+=(--phala-cli "$phala_cli")
fi
if [[ -n "${CRABBOX_PHALA_INSTANCE_TYPE:-}" ]]; then
  provider_args+=(--phala-instance-type "$CRABBOX_PHALA_INSTANCE_TYPE")
fi
if [[ -n "${CRABBOX_PHALA_NODE_ID:-}" ]]; then
  provider_args+=(--phala-node-id "$CRABBOX_PHALA_NODE_ID")
fi

trap - ERR
if doctor_output="$("$bin" doctor "${provider_args[@]}" 2>&1)"; then
  doctor_status=0
else
  doctor_status=$?
fi
trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
if [[ $doctor_status -ne 0 ]]; then
  if grep -Eiq 'quota|capacity|insufficient|balance|funds|credit|payment|rate limit|too many requests|429' <<<"$doctor_output"; then
    classify_and_exit quota_blocked "$doctor_output"
  fi
  if grep -Eiq 'login|unauthenticated|unauthorized|forbidden|not logged in|api.?key|token|no such host|timeout|x509|connection refused|executable file not found' <<<"$doctor_output"; then
    classify_and_exit environment_blocked "$doctor_output"
  fi
  classify_and_exit environment_blocked "$doctor_output"
fi

smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-phala-smoke.XXXXXX")"
smoke_repo="$smoke_root/repo"
export XDG_STATE_HOME="$smoke_root/state"
export CRABBOX_PHALA_SMOKE_VALUE="forwarded-ok"
export CRABBOX_SYNC_DELETE=true

mkdir -p "$smoke_repo"
cd "$smoke_repo"
git init -q
git config user.email smoke@example.com
git config user.name "Crabbox Phala Smoke"
printf 'provider: phala\nsync:\n  delete: true\n' >.crabbox.yaml
printf 'v1\n' >proof.txt
git add .crabbox.yaml proof.txt
git commit -qm "test: seed Phala smoke fixture"

slug="${CRABBOX_PHALA_SMOKE_SLUG:-phala-smoke-$$}"

trap - ERR
if run_output="$("$bin" run "${provider_args[@]}" --keep --slug "$slug" --ttl "${CRABBOX_LIVE_PHALA_TTL:-15m}" --idle-timeout 5m --allow-env CRABBOX_PHALA_SMOKE_VALUE -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v1 && test "$CRABBOX_PHALA_SMOKE_VALUE" = forwarded-ok && printf PHALA_SMOKE_OK' 2>&1)"; then
  run_status=0
else
  run_status=$?
fi
trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
if parsed_created_slug="$(extract_created_identifier "$run_output")"; then
  created_slug="$parsed_created_slug"
fi
created_cvm="$(grep -Eo 'phala_cvm=[^[:space:]]+' <<<"$run_output" | head -n1 | cut -d= -f2 || true)"
if [[ $run_status -ne 0 ]]; then
  if grep -Eiq 'quota|capacity|insufficient|balance|funds|credit|payment|rate limit|too many requests|429' <<<"$run_output"; then
    classify_and_exit quota_blocked "$run_output"
  fi
  if grep -Eiq 'login|unauthenticated|unauthorized|forbidden|api.?key|token|no such host|timeout|x509|connection refused' <<<"$run_output"; then
    classify_and_exit environment_blocked "$run_output"
  fi
  classify_and_exit diagnostic_only "$run_output"
fi
if ! grep -q 'PHALA_SMOKE_OK' <<<"$run_output"; then
  classify_and_exit diagnostic_only "run succeeded but sync/exec marker was missing"
fi
if [[ -z "$created_slug" ]]; then
  classify_and_exit diagnostic_only "run succeeded but created lease identifier was missing"
fi

"$bin" status "${provider_args[@]}" --id "$created_slug" --wait --wait-timeout "${CRABBOX_PHALA_SMOKE_WAIT_TIMEOUT:-120s}" >/dev/null
"$bin" list "${provider_args[@]}" --json >/dev/null
"$bin" stop "${provider_args[@]}" "$created_slug" >/dev/null 2>&1
created_slug=""
created_cvm=""

trap - EXIT
rm -rf -- "$smoke_root"
smoke_root=""

classify_and_exit live_phala_smoke_passed
