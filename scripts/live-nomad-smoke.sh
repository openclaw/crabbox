#!/usr/bin/env bash
set -Eeuo pipefail

classification_emitted=0
repo_root=""
invocation_dir="$PWD"
bin=""
smoke_root=""
smoke_repo=""
created_slug=""
cleanup_retry_delay="${CRABBOX_NOMAD_CLEANUP_RETRY_DELAY_SECONDS:-2}"
provider_args=()

classify_and_exit() {
  trap - ERR
  if [[ $classification_emitted -ne 0 ]]; then
    exit 1
  fi
  classification_emitted=1
  local classification="$1"
  local reason="${2:-}"
  if [[ -n "$reason" ]]; then
    printf 'classification=%s reason=%s\n' "$classification" "$reason"
  else
    printf 'classification=%s\n' "$classification"
  fi
  case "$classification" in
    live_nomad_smoke_passed|environment_blocked|quota_blocked) exit 0 ;;
    *) exit 1 ;;
  esac
}

classify_failure() {
  local output="$1"
  local reason="$2"
  if grep -Eiq 'quota|capacity|rate limit|too many requests|429|insufficient|no eligible node|no nodes were eligible|placement failures' <<<"$output"; then
    classify_and_exit quota_blocked "$reason"
  fi
  if grep -Eiq 'unauthorized|forbidden|permission|permission denied|acl|token|no such host|connection refused|timeout|timed out|TLS|x509|certificate|namespace|region|driver|image|alloc.?exec' <<<"$output"; then
    classify_and_exit environment_blocked "$reason"
  fi
  classify_and_exit diagnostic_only "$reason"
}

unexpected_failure() {
  classify_and_exit diagnostic_only "unexpected_failure_line_$1"
}

extract_created_identifier() {
  local output="$1"
  if [[ "$output" =~ (cbx_[a-f0-9]{12}) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  if [[ "$output" =~ slug=([^[:space:]]+) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  return 1
}

inventory_has_created_slug() {
  local inventory
  if ! inventory="$("$bin" list "${provider_args[@]}" --json 2>/dev/null)"; then
    return 2
  fi
  if jq -e --arg slug "$created_slug" 'any(.[]; ((.slug // .Slug // .labels.slug // "") == $slug) or ((.id // .ID // .lease // .labels.lease // "") == $slug))' <<<"$inventory" >/dev/null 2>&1; then
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
    if inventory_has_created_slug; then
      inventory_status=0
    else
      inventory_status=$?
    fi
    if [[ $inventory_status -eq 1 ]]; then
      return 0
    fi
    "$bin" stop "${provider_args[@]}" "$created_slug" >/dev/null 2>&1 || true
    if [[ $attempt -lt 3 ]]; then
      sleep "$cleanup_retry_delay"
    fi
  done
  if inventory_has_created_slug; then
    inventory_status=0
  else
    inventory_status=$?
  fi
  [[ $inventory_status -eq 1 ]]
}

cleanup() {
  local exit_status=$?
  trap - EXIT
  if [[ -n "$created_slug" && -n "$bin" && -x "$bin" ]]; then
    if ! stop_and_confirm; then
      printf 'cleanup=failed provider=nomad id=%s attempts=3\n' "$created_slug" >&2
      exit_status=1
    fi
  fi
  if [[ -n "$smoke_root" ]]; then
    rm -rf -- "$smoke_root"
  fi
  exit "$exit_status"
}
trap cleanup EXIT
trap 'unexpected_failure "$LINENO"' ERR

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  classify_and_exit environment_blocked "CRABBOX_LIVE_not_enabled"
fi

providers=",${CRABBOX_LIVE_PROVIDERS:-},"
if [[ "$providers" != *",nomad,"* ]]; then
  classify_and_exit environment_blocked "provider_not_selected"
fi

config_paths=()
add_config_path() {
  local config_path="$1"
  [[ -n "$config_path" ]] || return 0
  if [[ "$config_path" != /* ]]; then
    config_path="$repo_root/$config_path"
  fi
  config_paths+=("$config_path")
}
if [[ -n "${CRABBOX_CONFIG:-}" ]]; then
  add_config_path "$CRABBOX_CONFIG"
else
  add_config_path "$repo_root/crabbox.yaml"
  add_config_path "$repo_root/.crabbox.yaml"
fi

config_value() {
  local key_path="$1"
  command -v ruby >/dev/null 2>&1 || return 1
  local config_path
  local candidate
  for config_path in "${config_paths[@]}"; do
    [[ -r "$config_path" ]] || continue
    if candidate="$(ruby -ryaml -e '
      value = ARGV[1].split(".").reduce(YAML.load_file(ARGV[0])) do |memo, key|
        memo.is_a?(Hash) ? memo[key] : nil
      end
      exit 3 if value.nil? || value.to_s.empty?
      print value
    ' "$config_path" "$key_path" 2>/dev/null)"; then
      printf '%s' "$candidate"
      return 0
    fi
  done
  return 1
}

value_from_env_or_config() {
  local env_name="$1"
  local yaml_path="$2"
  local value="${!env_name:-}"
  if [[ -n "$value" ]]; then
    printf '%s' "$value"
    return 0
  fi
  if value="$(config_value "$yaml_path")"; then
    printf '%s' "$value"
    return 0
  fi
  return 1
}

address="$(value_from_env_or_config NOMAD_ADDR nomad.address || true)"
if [[ -z "$address" ]]; then
  classify_and_exit environment_blocked "missing_NOMAD_ADDR_or_nomad.address"
fi

token_env="${CRABBOX_NOMAD_TOKEN_ENV:-}"
if [[ -z "$token_env" ]]; then
  token_env="$(config_value nomad.tokenEnv || true)"
fi
token_env="${token_env:-NOMAD_TOKEN}"
if [[ ! "$token_env" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
  classify_and_exit environment_blocked "invalid_nomad_token_env"
fi
if [[ -z "${!token_env:-}" ]]; then
  classify_and_exit environment_blocked "missing_${token_env}"
fi

bin="${CRABBOX_BIN:-$repo_root/bin/crabbox}"
if [[ "$bin" != /* ]]; then
  bin="$invocation_dir/$bin"
fi
if [[ -z "${CRABBOX_BIN:-}" || ! -x "$bin" ]]; then
  mkdir -p "$(dirname "$bin")"
  go build -trimpath -o "$bin" ./cmd/crabbox
fi

provider_args=(--provider nomad --nomad-address "$address" --nomad-token-env "$token_env")

add_optional_arg() {
  local env_name="$1"
  local yaml_path="$2"
  local flag_name="$3"
  local value
  value="$(value_from_env_or_config "$env_name" "$yaml_path" || true)"
  if [[ -n "$value" ]]; then
    provider_args+=("$flag_name" "$value")
  fi
}

add_optional_arg NOMAD_REGION nomad.region --nomad-region
add_optional_arg NOMAD_NAMESPACE nomad.namespace --nomad-namespace
add_optional_arg NOMAD_CACERT nomad.caCert --nomad-ca-cert
add_optional_arg NOMAD_CAPATH nomad.caPath --nomad-ca-path
add_optional_arg CRABBOX_NOMAD_CLIENT_CERT nomad.clientCert --nomad-client-cert
add_optional_arg CRABBOX_NOMAD_CLIENT_KEY nomad.clientKey --nomad-client-key
add_optional_arg CRABBOX_NOMAD_TLS_SERVER_NAME nomad.tlsServerName --nomad-tls-server-name

if [[ "${CRABBOX_NOMAD_SKIP_VERIFY:-}" == "1" ]]; then
  provider_args+=(--nomad-skip-verify)
fi

smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-nomad-smoke.XXXXXX")"
smoke_repo="$smoke_root/repo"
export XDG_STATE_HOME="$smoke_root/state"
export CRABBOX_NOMAD_SMOKE_VALUE="forwarded-ok"
export CRABBOX_SYNC_DELETE=true

mkdir -p "$smoke_repo"
cd "$smoke_repo"
git init -q
git config user.email smoke@example.com
git config user.name "Crabbox Nomad Smoke"
cat >.crabbox.yaml <<'YAML'
provider: nomad
sync:
  delete: true
nomad:
  workdir: /workspace/crabbox
YAML
printf 'v1\n' >proof.txt
printf 'remove-me\n' >stale.txt
git add .crabbox.yaml proof.txt stale.txt
git commit -qm "test: seed Nomad smoke fixture"

trap - ERR
if doctor_output="$("$bin" doctor "${provider_args[@]}" --json 2>&1)"; then
  doctor_status=0
else
  doctor_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $doctor_status -ne 0 ]]; then
  classify_failure "$doctor_output" "doctor_failed"
fi

slug="${CRABBOX_NOMAD_SMOKE_SLUG:-nomad-smoke-$$}"

trap - ERR
if warmup_output="$("$bin" warmup "${provider_args[@]}" --slug "$slug" --timing-json 2>&1)"; then
  warmup_status=0
else
  warmup_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if parsed_slug="$(extract_created_identifier "$warmup_output")"; then
  created_slug="$parsed_slug"
fi
if [[ $warmup_status -ne 0 ]]; then
  classify_failure "$warmup_output" "warmup_failed"
fi
if [[ -z "$created_slug" ]]; then
  classify_and_exit diagnostic_only "warmup_identifier_missing"
fi

"$bin" status "${provider_args[@]}" --id "$created_slug" --wait --json >/dev/null
"$bin" list "${provider_args[@]}" --json |
  jq -e --arg slug "$created_slug" 'any(.[]; ((.slug // .Slug // .labels.slug // "") == $slug) or ((.id // .ID // .lease // .labels.lease // "") == $slug))' >/dev/null

trap - ERR
if run_output="$("$bin" run "${provider_args[@]}" --id "$created_slug" --timing-json --allow-env CRABBOX_NOMAD_SMOKE_VALUE -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v1 && test -f stale.txt && test "$CRABBOX_NOMAD_SMOKE_VALUE" = forwarded-ok && printf NOMAD_SMOKE_V1_OK' 2>&1)"; then
  run_status=0
else
  run_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $run_status -ne 0 ]]; then
  classify_failure "$run_output" "initial_run_failed"
fi
if ! grep -q 'NOMAD_SMOKE_V1_OK' <<<"$run_output" || ! grep -q '"provider":"nomad"' <<<"$run_output"; then
  classify_and_exit diagnostic_only "initial_run_proof_incomplete"
fi

printf 'v2\n' >proof.txt
printf 'second\n' >second.txt
git add proof.txt second.txt
git rm -q stale.txt
git commit -qm "test: update Nomad smoke fixture"

trap - ERR
if reuse_output="$("$bin" run "${provider_args[@]}" --id "$created_slug" --timing-json -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v2 && test -f second.txt && test ! -e stale.txt && printf NOMAD_SMOKE_V2_OK' 2>&1)"; then
  reuse_status=0
else
  reuse_status=$?
fi
trap 'unexpected_failure "$LINENO"' ERR
if [[ $reuse_status -ne 0 ]]; then
  classify_failure "$reuse_output" "reuse_run_failed"
fi
if ! grep -q 'NOMAD_SMOKE_V2_OK' <<<"$reuse_output" || ! grep -q '"provider":"nomad"' <<<"$reuse_output"; then
  classify_and_exit diagnostic_only "reuse_run_proof_incomplete"
fi

"$bin" run "${provider_args[@]}" --id "$created_slug" --no-sync -- /bin/sh -lc 'printf NOMAD_SMOKE_NOSYNC_OK'

if ! stop_and_confirm; then
  printf 'cleanup=failed provider=nomad id=%s attempts=3\n' "$created_slug" >&2
  created_slug=""
  classify_and_exit diagnostic_only "lease_cleanup_unconfirmed"
fi
created_slug=""

trap - EXIT
rm -rf -- "$smoke_root"
classify_and_exit live_nomad_smoke_passed
