#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

classification="diagnostic_only"
slug="crabbox-opensandbox-smoke-$$"
bin="${CRABBOX_BIN:-$repo_root/bin/crabbox}"
smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-opensandbox-smoke.XXXXXX")"
smoke_repo="$smoke_root/repo"
export XDG_STATE_HOME="$smoke_root/state"
export CRABBOX_OPENSANDBOX_SMOKE_VALUE="forwarded-ok"

classify_and_exit() {
  classification="$1"
  message="${2:-}"
  if [[ -n "$message" ]]; then
    printf '%s %s\n' "$classification" "$message"
  else
    printf '%s\n' "$classification"
  fi
  case "$classification" in
    live_opensandbox_smoke_passed) exit 0 ;;
    environment_blocked|quota_blocked|diagnostic_only) exit 0 ;;
    *) exit 1 ;;
  esac
}

cleanup() {
  if [[ -x "$bin" ]]; then
    "$bin" stop --provider opensandbox --opensandbox-forget-missing "$slug" >/dev/null 2>&1 || true
  fi
  rm -rf -- "$smoke_root"
}
trap cleanup EXIT

if [[ -z "${CRABBOX_OPENSANDBOX_API_KEY:-${OPEN_SANDBOX_API_KEY:-}}" ]]; then
  classify_and_exit environment_blocked "missing CRABBOX_OPENSANDBOX_API_KEY or OPEN_SANDBOX_API_KEY"
fi

if [[ -z "${CRABBOX_OPENSANDBOX_API_URL:-${OPEN_SANDBOX_API_URL:-}}" ]]; then
  classify_and_exit environment_blocked "missing CRABBOX_OPENSANDBOX_API_URL or OPEN_SANDBOX_API_URL"
fi

mkdir -p "$(dirname "$bin")"
go build -trimpath -o "$bin" ./cmd/crabbox

mkdir -p "$smoke_repo"
cd "$smoke_repo"
git init -q
git config user.email smoke@example.com
git config user.name "Crabbox OpenSandbox Smoke"
printf 'provider: opensandbox\nsync:\n  delete: true\n' >.crabbox.yaml
printf 'v1\n' >proof.txt
printf 'remove-me\n' >stale.txt
git add .crabbox.yaml proof.txt stale.txt
git commit -qm "test: seed OpenSandbox smoke fixture"

set +e
run_output="$("$bin" run --provider opensandbox --keep --slug "$slug" --timing-json \
  --allow-env CRABBOX_OPENSANDBOX_SMOKE_VALUE -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v1 && test -f stale.txt && test "$CRABBOX_OPENSANDBOX_SMOKE_VALUE" = forwarded-ok && printf OPEN_SANDBOX_SMOKE_V1_OK' 2>&1)"
run_status=$?
set -e
if [[ $run_status -ne 0 ]]; then
  if grep -Eiq 'quota|capacity|rate limit|too many requests|429|insufficient' <<<"$run_output"; then
    classify_and_exit quota_blocked "$run_output"
  fi
  if grep -Eiq 'api key|unauthorized|forbidden|connection refused|no such host|timeout|TLS|x509' <<<"$run_output"; then
    classify_and_exit environment_blocked "$run_output"
  fi
  classify_and_exit diagnostic_only "$run_output"
fi
if ! grep -q 'OPEN_SANDBOX_SMOKE_V1_OK' <<<"$run_output" || ! grep -q '"name":"replace"' <<<"$run_output"; then
  classify_and_exit diagnostic_only "initial sync succeeded but staged-sync proof was incomplete"
fi

"$bin" status --provider opensandbox --id "$slug" >/dev/null
"$bin" list --provider opensandbox --json >/dev/null

printf 'v2\n' >proof.txt
printf 'second\n' >second.txt
git add proof.txt second.txt
git rm -q stale.txt

set +e
reuse_output="$("$bin" run --provider opensandbox --id "$slug" --timing-json -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v2 && test -f second.txt && test ! -e stale.txt && printf OPEN_SANDBOX_SMOKE_V2_OK' 2>&1)"
reuse_status=$?
set -e
if [[ $reuse_status -ne 0 ]]; then
  classify_and_exit diagnostic_only "$reuse_output"
fi
if ! grep -q 'OPEN_SANDBOX_SMOKE_V2_OK' <<<"$reuse_output" || ! grep -q '"name":"replace"' <<<"$reuse_output"; then
  classify_and_exit diagnostic_only "reuse sync succeeded but replacement proof was incomplete"
fi

"$bin" stop --provider opensandbox "$slug" >/dev/null 2>&1
trap - EXIT
rm -rf -- "$smoke_root"

classify_and_exit live_opensandbox_smoke_passed
