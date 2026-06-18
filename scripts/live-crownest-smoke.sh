#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin="${CRABBOX_BIN:-$repo_root/bin/crabbox}"
smoke_root=""
slug=""

classify() {
  local name="$1"
  local reason="${2:-}"
  if [[ -n "$reason" ]]; then
    printf 'classification=%s reason=%s\n' "$name" "$reason"
  else
    printf 'classification=%s\n' "$name"
  fi
  case "$name" in
    live_crownest_smoke_passed|environment_blocked|quota_blocked) exit 0 ;;
    *) exit 1 ;;
  esac
}

cleanup() {
  local status=$?
  if [[ -n "$slug" && -x "$bin" ]]; then
    "$bin" stop --provider crownest --crownest-forget-missing "$slug" >/dev/null 2>&1 || true
  fi
  if [[ -n "$smoke_root" ]]; then
    rm -rf -- "$smoke_root"
  fi
  exit "$status"
}
trap cleanup EXIT

need_tool() {
  command -v "$1" >/dev/null 2>&1 || classify environment_blocked "missing_$1"
}

run_or_classify() {
  local reason="$1"
  shift
  local output status
  set +e
  output="$("$@" 2>&1)"
  status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    if grep -Eiq 'quota|capacity|rate limit|too many requests|429|insufficient' <<<"$output"; then
      classify quota_blocked "$reason"
    fi
    if grep -Eiq 'api key|unauthorized|forbidden|connection refused|no such host|timeout|timed out|TLS|x509|certificate|authentication|permission denied' <<<"$output"; then
      classify environment_blocked "$reason"
    fi
    printf '%s\n' "$output" >&2
    classify diagnostic_only "$reason"
  fi
  printf '%s\n' "$output"
}

[[ -n "${CRABBOX_CROWNEST_API_KEY:-${CROWNEST_API_KEY:-}}" ]] ||
  classify environment_blocked "missing_crownest_api_key"
need_tool git
need_tool jq

cd "$repo_root"
if [[ -z "${CRABBOX_BIN:-}" ]]; then
  mkdir -p "$(dirname "$bin")"
  go build -trimpath -o "$bin" ./cmd/crabbox
fi

smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-crownest-smoke.XXXXXX")"
export CRABBOX_CONFIG="$smoke_root/config.yaml"
export XDG_STATE_HOME="$smoke_root/state"
mkdir -p "$smoke_root/repo/scripts"
cd "$smoke_root/repo"
git init -q
git config user.email smoke@example.com
git config user.name "Crabbox Crownest Smoke"
printf 'hello-from-crabbox-crownest\n' >marker.txt
cat >package.json <<'JSON'
{"private":true,"scripts":{"test":"node scripts/check.js"}}
JSON
cat >scripts/check.js <<'JS'
const fs = require("node:fs");
const value = fs.readFileSync("marker.txt", "utf8");
if (!value.includes("crabbox-crownest")) process.exit(2);
console.log(value.trim());
JS
git add marker.txt package.json scripts/check.js
git commit -qm "test: seed Crownest smoke fixture"

doctor_output="$(run_or_classify doctor_failed "$bin" doctor --provider crownest)"
grep -q 'provider=crownest' <<<"$doctor_output" || classify diagnostic_only "doctor_provider_missing"

run_output="$(run_or_classify run_failed "$bin" run --provider crownest --crownest-template python-node --crownest-timeout-secs 120 -- pnpm test)"
grep -q 'hello-from-crabbox-crownest' <<<"$run_output" || classify diagnostic_only "pnpm_test_output_missing"

keep_json="$smoke_root/lease.json"
run_or_classify keep_run_failed "$bin" run --provider crownest --crownest-template python-node --crownest-timeout-secs 120 --keep --lease-output "$keep_json" -- sh -lc 'printf keep-ok' >/dev/null
slug="$(jq -r '.slug' "$keep_json")"
[[ -n "$slug" && "$slug" != "null" ]] || classify diagnostic_only "lease_slug_missing"
"$bin" status --provider crownest --id "$slug" >/dev/null
reuse_output="$(run_or_classify reuse_run_failed "$bin" run --provider crownest --id "$slug" --crownest-timeout-secs 120 -- sh -lc 'printf "reuse-ok\n"')"
grep -q 'reuse-ok' <<<"$reuse_output" || classify diagnostic_only "reuse_output_missing"
"$bin" stop --provider crownest "$slug" >/dev/null
slug=""

classify live_crownest_smoke_passed
