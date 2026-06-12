#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROVIDER="cloudflare-dynamic-workers"
SKIP_DEPLOY="${CRABBOX_CFDW_SKIP_DEPLOY:-0}"
SKIP_LOCAL_CHECKS="${CRABBOX_CFDW_SKIP_LOCAL_CHECKS:-0}"
repo="${CRABBOX_LIVE_REPO:-$ROOT}"

cd "$ROOT"

lease_id=""
smoke_tmp_files=()
last_stdout=""
last_stderr=""

cleanup() {
  if [[ -n "$lease_id" && -n "${CRABBOX_BIN:-}" && -x "${CRABBOX_BIN:-}" ]]; then
    "$CRABBOX_BIN" stop --provider "$PROVIDER" --id "$lease_id" >/dev/null 2>&1 || true
  fi
  if ((${#smoke_tmp_files[@]} > 0)); then
    rm -f "${smoke_tmp_files[@]}"
  fi
}
trap cleanup EXIT

print_classification() {
  local class="$1"
  local reason="$2"
  local mutation="${3:-false}"
  printf '%s provider=%s mutation=%s reason=%s\n' "$class" "$PROVIDER" "$mutation" "$reason"
}

provider_enabled() {
  [[ "${CRABBOX_LIVE:-}" == "1" ]] || return 1
  local selected=",${CRABBOX_LIVE_PROVIDERS:-},"
  selected="${selected//[[:space:]]/}"
  [[ "$selected" == *",all,"* || "$selected" == *",$PROVIDER,"* ]]
}

missing_env_names() {
  local missing=()
  for name in "$@"; do
    if [[ -z "${!name:-}" ]]; then
      missing+=("$name")
    fi
  done
  (IFS=,; printf '%s' "${missing[*]}")
}

redact_file() {
  local file="$1"
  local text
  text="$(cat "$file")"
  for secret in "${CLOUDFLARE_API_TOKEN:-}" "${CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN:-}"; do
    if [[ -n "$secret" ]]; then
      text="${text//${secret}/<redacted>}"
    fi
  done
  printf '%s' "$text"
}

print_command() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
}

run_logged() {
  local out
  local err
  local status
  out="$(mktemp)"
  err="$(mktemp)"
  smoke_tmp_files+=("$out" "$err")
  last_stdout="$out"
  last_stderr="$err"
  print_command "$@" >&2
  if "$@" >"$out" 2>"$err"; then
    status=0
  else
    status=$?
  fi
  redact_file "$out"
  redact_file "$err" >&2
  return "$status"
}

classify_files() {
  local text
  text="$(cat "$@" 2>/dev/null | tr '[:upper:]' '[:lower:]')"
  if [[ "$text" == *"unauthorized"* || "$text" == *"forbidden"* || "$text" == *"permission"* || "$text" == *"invalid token"* || "$text" == *"authentication"* ]]; then
    printf 'auth_blocked'
    return
  fi
  if [[ "$text" == *"quota"* || "$text" == *"limit exceeded"* || "$text" == *"billing"* || "$text" == *"paid plan"* || "$text" == *"capacity"* ]]; then
    printf 'quota_blocked'
    return
  fi
  printf 'environment_blocked'
}

parse_lease_id() {
  sed -nE '/^[[:space:]]*\{/s/.*"leaseId"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' "$@" | tail -1
}

setup_crabbox_bin() {
  if [[ -z "${CRABBOX_BIN:-}" ]]; then
    CRABBOX_BIN="$ROOT/bin/crabbox"
    mkdir -p "$(dirname "$CRABBOX_BIN")"
    run_logged go build -trimpath -o "$CRABBOX_BIN" ./cmd/crabbox || return $?
  elif [[ ! -x "$CRABBOX_BIN" ]]; then
    print_classification environment_blocked "crabbox_bin_not_executable" false
    return 1
  fi
}

local_checks() {
  if [[ "$SKIP_LOCAL_CHECKS" == "1" ]]; then
    return 0
  fi
  run_logged npm ci --prefix "$ROOT/worker" || return $?
  run_logged npm run format:check --prefix "$ROOT/worker" || return $?
  run_logged npm run lint --prefix "$ROOT/worker" || return $?
  run_logged npm run check --prefix "$ROOT/worker" || return $?
  run_logged npm test --prefix "$ROOT/worker" -- cloudflare-dynamic-worker-runner || return $?
  run_logged npm test --prefix "$ROOT/worker" -- cloudflare-container-runner || return $?
  run_logged npm run build:cloudflare-dynamic-workers --prefix "$ROOT/worker" || return $?
}

deploy_runner() {
  if [[ "$SKIP_DEPLOY" == "1" ]]; then
    return 0
  fi
  local oldpwd
  oldpwd="$(pwd)"
  cd "$ROOT/worker"
  run_logged npx wrangler secret put CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN \
    --config wrangler.cloudflare-dynamic-workers.jsonc \
    <<<"$CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN" || {
      local status=$?
      cd "$oldpwd"
      return "$status"
    }
  run_logged npm run deploy:cloudflare-dynamic-workers || {
    local status=$?
    cd "$oldpwd"
    return "$status"
  }
  cd "$oldpwd"
}

write_modules() {
  ok_module="$(mktemp "${TMPDIR:-/tmp}/cfdw-ok-XXXXXX.mjs")"
  egress_module="$(mktemp "${TMPDIR:-/tmp}/cfdw-egress-XXXXXX.mjs")"
  smoke_tmp_files+=("$ok_module" "$egress_module")
  cat >"$ok_module" <<'MODULE'
export default {
  async fetch() {
    return new Response("CRABBOX_CFDW_OK");
  },
};
MODULE
  cat >"$egress_module" <<'MODULE'
export default {
  async fetch() {
    try {
      await fetch("https://example.com/");
      return new Response("UNEXPECTED_EGRESS_OPEN", { status: 500 });
    } catch {
      return new Response("CRABBOX_CFDW_EGRESS_BLOCKED");
    }
  },
};
MODULE
}

smoke_dynamic_workers() {
  local ok_module
  local egress_module
  write_modules
  local oldpwd
  oldpwd="$(pwd)"
  if ! cd "$repo" 2>/dev/null; then
    print_classification environment_blocked "live_repo_unavailable" false
    return 0
  fi

  if ! run_logged "$CRABBOX_BIN" doctor --provider "$PROVIDER"; then
    cd "$oldpwd"
    print_classification "$(classify_files "$last_stdout" "$last_stderr")" "doctor_failed" false
    return 0
  fi
  if ! run_logged "$CRABBOX_BIN" run --provider "$PROVIDER" --keep --slug cfdw-live-smoke --script "$ok_module" --timing-json --label cfdw-live-smoke; then
    lease_id="$(parse_lease_id "$last_stdout" "$last_stderr")"
    cd "$oldpwd"
    print_classification "$(classify_files "$last_stdout" "$last_stderr")" "module_run_failed" true
    return 0
  fi
  lease_id="$(parse_lease_id "$last_stdout" "$last_stderr")"
  if [[ -z "$lease_id" ]]; then
    cd "$oldpwd"
    print_classification environment_blocked "missing_timing_lease_id" true
    return 0
  fi
  if ! grep -q 'CRABBOX_CFDW_OK' "$last_stdout"; then
    cd "$oldpwd"
    print_classification environment_blocked "unexpected_module_output" true
    return 0
  fi
  if ! run_logged "$CRABBOX_BIN" run --provider "$PROVIDER" --cloudflare-dynamic-workers-cache one-shot --cloudflare-dynamic-workers-egress blocked --script "$egress_module" --timing-json --label cfdw-egress-smoke; then
    cd "$oldpwd"
    print_classification "$(classify_files "$last_stdout" "$last_stderr")" "egress_run_failed" true
    return 0
  fi
  if ! grep -q 'CRABBOX_CFDW_EGRESS_BLOCKED' "$last_stdout"; then
    cd "$oldpwd"
    print_classification environment_blocked "egress_block_not_observed" true
    return 0
  fi
  if ! run_logged "$CRABBOX_BIN" status --provider "$PROVIDER" --id "$lease_id" --json; then
    cd "$oldpwd"
    print_classification "$(classify_files "$last_stdout" "$last_stderr")" "status_failed" true
    return 0
  fi
  if ! run_logged "$CRABBOX_BIN" list --provider "$PROVIDER" --refresh --json; then
    cd "$oldpwd"
    print_classification "$(classify_files "$last_stdout" "$last_stderr")" "list_failed" true
    return 0
  fi
  if ! run_logged "$CRABBOX_BIN" stop --provider "$PROVIDER" --id "$lease_id"; then
    cd "$oldpwd"
    print_classification "$(classify_files "$last_stdout" "$last_stderr")" "stop_failed" true
    return 0
  fi
  lease_id=""
  if ! run_logged "$CRABBOX_BIN" cleanup --provider "$PROVIDER" --dry-run; then
    cd "$oldpwd"
    print_classification "$(classify_files "$last_stdout" "$last_stderr")" "cleanup_failed" true
    return 0
  fi
    cd "$oldpwd"
    print_classification live_cloudflare_dynamic_workers_smoke_passed "deploy_doctor_run_status_list_stop_cleanup" true
}

main() {
  if ! provider_enabled; then
    print_classification environment_blocked live_gate_missing false
    return 0
  fi

  local missing
  missing="$(missing_env_names CLOUDFLARE_ACCOUNT_ID CLOUDFLARE_API_TOKEN CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN)"
  if [[ -n "$missing" ]]; then
    printf 'auth_blocked provider=%s mutation=false reason=missing_env missing=%s\n' "$PROVIDER" "$missing"
    return 0
  fi

  export CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL
  export CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN

  if ! setup_crabbox_bin; then
    return 0
  fi
  if ! local_checks; then
    print_classification "$(classify_files "$last_stdout" "$last_stderr")" "local_checks_failed" false
    return 0
  fi
  if ! deploy_runner; then
    print_classification "$(classify_files "$last_stdout" "$last_stderr")" "deploy_failed" true
    return 0
  fi
  smoke_dynamic_workers
}

main "$@"
