#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROVIDER="cloudflare-dynamic-workers"
SKIP_DEPLOY="${CRABBOX_CFDW_SKIP_DEPLOY:-0}"
SKIP_LOCAL_CHECKS="${CRABBOX_CFDW_SKIP_LOCAL_CHECKS:-0}"
repo="${CRABBOX_LIVE_REPO:-$ROOT}"

cd "$ROOT"

lease_id=""
deploy_config=""
deployed_worker=""
worker_deploy_attempted=0
kv_namespace_id=""
smoke_tmp_files=()
last_stdout=""
last_stderr=""
cleanup_done=0
cleanup_status=0
cleanup_reported=0

delete_worker_for_cleanup() {
  local out
  local err
  out="$(mktemp)"
  err="$(mktemp)"
  smoke_tmp_files+=("$out" "$err")
  if (
    cd "$ROOT/worker"
    npx wrangler delete "$deployed_worker" --config "$deploy_config" --force >"$out" 2>"$err"
  ); then
    return 0
  fi
  grep -Eiq '(^|[^0-9])404([^0-9]|$)|worker.*not found' "$out" "$err"
}

retire_durable_object_for_cleanup() {
  node - "$deploy_config" <<'NODE'
const fs = require("node:fs");
const configPath = process.argv[2];
const config = JSON.parse(fs.readFileSync(configPath, "utf8"));
delete config.durable_objects;
config.migrations = [
  ...(Array.isArray(config.migrations) ? config.migrations : []),
  {
    tag: "cloudflare-dynamic-workers-v2-cleanup",
    deleted_classes: ["DynamicWorkerRunCoordinator"],
  },
];
fs.writeFileSync(configPath, JSON.stringify(config));
NODE
  local oldpwd
  oldpwd="$(pwd)"
  cd "$ROOT/worker"
  if run_logged npx wrangler deploy --config "$deploy_config"; then
    cd "$oldpwd"
    return 0
  else
    local status=$?
    cd "$oldpwd"
    return "$status"
  fi
}

cleanup() {
  if [[ "$cleanup_done" == "1" ]]; then
    return "$cleanup_status"
  fi
  cleanup_done=1
  local status=0
  if [[ -n "$lease_id" && -n "${CRABBOX_BIN:-}" && -x "${CRABBOX_BIN:-}" ]]; then
    "$CRABBOX_BIN" stop --provider "$PROVIDER" --id "$lease_id" >/dev/null 2>&1 || status=1
  fi
  if [[ -n "$deploy_config" ]]; then
    local worker_deleted=1
    if [[ "$worker_deploy_attempted" == "1" && -n "$deployed_worker" ]]; then
      retire_durable_object_for_cleanup || {
        status=1
      }
      delete_worker_for_cleanup || {
        status=1
        worker_deleted=0
      }
    fi
    if [[ "$worker_deleted" == "1" && -n "$kv_namespace_id" ]]; then
      (
        cd "$ROOT/worker"
        npx wrangler kv namespace delete --namespace-id "$kv_namespace_id" --config "$deploy_config" --skip-confirmation >/dev/null 2>&1
      ) || status=1
    fi
  fi
  if ((${#smoke_tmp_files[@]} > 0)); then
    rm -f "${smoke_tmp_files[@]}" || status=1
  fi
  cleanup_status="$status"
  return "$status"
}

cleanup_on_exit() {
  if ! cleanup && [[ "$cleanup_reported" != "1" ]]; then
    print_classification environment_blocked "ephemeral_cleanup_failed" true
  fi
}
trap cleanup_on_exit EXIT

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
  deployed_worker="crabbox-cfdw-smoke-$(date -u +%Y%m%d%H%M%S)-$RANDOM"
  local deploy_config_base
  deploy_config_base="$(mktemp "$ROOT/worker/.wrangler-cfdw-smoke-XXXXXX")"
  deploy_config="${deploy_config_base}.json"
  mv "$deploy_config_base" "$deploy_config"
  local secrets_file
  local secrets_file_base
  secrets_file_base="$(mktemp "${TMPDIR:-/tmp}/cfdw-secrets-XXXXXX")"
  secrets_file="${secrets_file_base}.json"
  mv "$secrets_file_base" "$secrets_file"
  smoke_tmp_files+=("$deploy_config" "$secrets_file")
  cat >"$deploy_config" <<EOF
{
  "\$schema": "./node_modules/wrangler/config-schema.json",
  "name": "$deployed_worker",
  "main": "src/cloudflare-dynamic-worker-runner.ts",
  "compatibility_date": "2026-06-12",
  "workers_dev": true,
  "preview_urls": false,
  "worker_loaders": [{ "binding": "LOADER" }],
  "durable_objects": {
    "bindings": [
      { "name": "RUN_COORDINATOR", "class_name": "DynamicWorkerRunCoordinator" }
    ]
  },
  "migrations": [
    {
      "tag": "cloudflare-dynamic-workers-v1",
      "new_sqlite_classes": ["DynamicWorkerRunCoordinator"]
    }
  ],
  "observability": { "enabled": true }
}
EOF
  node -e '
    const token = process.env.CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN;
    if (token === undefined) process.exit(1);
    process.stdout.write(JSON.stringify({ CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN: token }));
  ' >"$secrets_file"
  chmod 0600 "$secrets_file"

  local oldpwd
  oldpwd="$(pwd)"
  cd "$ROOT/worker"
  run_logged npx wrangler kv namespace create "${deployed_worker}-runs" --binding RUNS --update-config --config "$deploy_config" || {
    local status=$?
    cd "$oldpwd"
    return "$status"
  }
  kv_namespace_id="$(
    node -e '
      const fs = require("node:fs");
      const config = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
      const binding = config.kv_namespaces?.find((entry) => entry.binding === "RUNS");
      process.stdout.write(binding?.id ?? "");
    ' "$deploy_config"
  )"
  if [[ -z "$kv_namespace_id" ]]; then
    cd "$oldpwd"
    return 1
  fi
  worker_deploy_attempted=1
  run_logged npx wrangler deploy --config "$deploy_config" --secrets-file "$secrets_file" || {
    local status=$?
    cd "$oldpwd"
    return "$status"
  }
  local runner_url
  runner_url="$(grep -Eo 'https://[^[:space:]]+\.workers\.dev' "$last_stdout" | tail -1)"
  if [[ -z "$runner_url" ]]; then
    cd "$oldpwd"
    return 1
  fi
  CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL="$runner_url"
  export CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL
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
  if ! cleanup; then
    cleanup_reported=1
    print_classification environment_blocked "ephemeral_cleanup_failed" true
    return 0
  fi
  print_classification live_cloudflare_dynamic_workers_smoke_passed "deploy_doctor_run_status_list_stop_cleanup" true
}

main() {
  if ! provider_enabled; then
    print_classification environment_blocked live_gate_missing false
    return 0
  fi

  local missing
  if [[ "$SKIP_DEPLOY" == "1" ]]; then
    missing="$(missing_env_names CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN)"
  else
    missing="$(missing_env_names CLOUDFLARE_ACCOUNT_ID)"
  fi
  if [[ -n "$missing" ]]; then
    printf 'auth_blocked provider=%s mutation=false reason=missing_env missing=%s\n' "$PROVIDER" "$missing"
    return 0
  fi

  if [[ -z "${CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN:-}" ]]; then
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN="$(openssl rand -hex 32)"
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
