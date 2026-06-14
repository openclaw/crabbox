#!/usr/bin/env bash
set -Eeuo pipefail

classification_emitted=0
repo_root=""
bin=""
smoke_root=""
smoke_repo=""
created_slug=""
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
    live_agent_sandbox_smoke_passed|environment_blocked|quota_blocked|diagnostic_only) exit 0 ;;
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
  if [[ "$output" =~ lease=(asbx_[A-Za-z0-9_.-]+) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  if [[ "$output" =~ (^|[[:space:]])(asbx_[A-Za-z0-9_.-]+)($|[[:space:]]) ]]; then
    printf '%s\n' "${BASH_REMATCH[2]}"
    return 0
  fi
  if [[ "$output" =~ claim=([^[:space:]]+) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  if [[ "$output" =~ slug=([^[:space:]]+) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  return 1
}

cleanup() {
  if [[ -n "$created_slug" && -n "$bin" && -x "$bin" ]]; then
    "$bin" stop "${provider_args[@]}" --agent-sandbox-forget-missing "$created_slug" >/dev/null 2>&1 || true
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
if [[ "$bin" != /* ]]; then
  bin="$repo_root/$bin"
fi
providers=",${CRABBOX_LIVE_PROVIDERS:-},"

config_paths=()
add_config_path() {
  local path="$1"
  [[ -n "$path" ]] || return 0
  if [[ "$path" != /* ]]; then
    path="$repo_root/$path"
  fi
  config_paths+=("$path")
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
  local path
  local candidate
  for path in "${config_paths[@]}"; do
    [[ -r "$path" ]] || continue
    if candidate="$(ruby -ryaml -e '
      value = ARGV[1].split(".").reduce(YAML.load_file(ARGV[0])) do |memo, key|
        memo.is_a?(Hash) ? memo[key] : nil
      end
      exit 3 if value.nil? || value.to_s.empty?
      print value
    ' "$path" "$key_path" 2>/dev/null)"; then
      printf '%s' "$candidate"
      return 0
    fi
  done
  return 1
}

first_config_value() {
  local env_name="$1"
  local yaml_path="$2"
  local fallback="${3:-}"
  local value="${!env_name:-}"
  if [[ -n "$value" ]]; then
    printf '%s' "$value"
    return 0
  fi
  if value="$(config_value "$yaml_path")"; then
    printf '%s' "$value"
    return 0
  fi
  printf '%s' "$fallback"
}

trusted_config_value() {
  local env_name="$1"
  local yaml_path="$2"
  local value="${!env_name:-}"
  if [[ -n "$value" ]]; then
    printf '%s' "$value"
    return 0
  fi
  if [[ -n "${CRABBOX_CONFIG:-}" ]] && value="$(config_value "$yaml_path")"; then
    printf '%s' "$value"
    return 0
  fi
  return 1
}

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  classify_and_exit environment_blocked "reason=CRABBOX_LIVE_not_enabled missing=CRABBOX_LIVE=1"
fi

if [[ "$providers" != *",agent-sandbox,"* ]]; then
  classify_and_exit environment_blocked "reason=provider_not_selected missing=CRABBOX_LIVE_PROVIDERS=agent-sandbox"
fi

kubectl="$(trusted_config_value CRABBOX_AGENT_SANDBOX_KUBECTL agentSandbox.kubectl || true)"
kubeconfig="$(trusted_config_value CRABBOX_AGENT_SANDBOX_KUBECONFIG agentSandbox.kubeconfig || true)"
kubeconfig_inherited=0
context="$(trusted_config_value CRABBOX_AGENT_SANDBOX_CONTEXT agentSandbox.context || true)"
namespace="$(trusted_config_value CRABBOX_AGENT_SANDBOX_NAMESPACE agentSandbox.namespace || true)"
warm_pool="$(trusted_config_value CRABBOX_AGENT_SANDBOX_WARM_POOL agentSandbox.warmPool || true)"
container="$(trusted_config_value CRABBOX_AGENT_SANDBOX_CONTAINER agentSandbox.container || true)"
workdir="$(trusted_config_value CRABBOX_AGENT_SANDBOX_WORKDIR agentSandbox.workdir || true)"
namespace="${namespace:-default}"
workdir="${workdir:-/workspace/crabbox}"

if [[ -z "$kubeconfig" && -n "${KUBECONFIG:-}" ]]; then
  kubeconfig_inherited=1
fi
if [[ -z "$kubeconfig" && $kubeconfig_inherited -eq 0 && -r "$HOME/.kube/config" ]]; then
  kubeconfig="$HOME/.kube/config"
fi
if [[ -z "$kubeconfig" && $kubeconfig_inherited -eq 0 ]]; then
  classify_and_exit environment_blocked "reason=missing_kubeconfig missing=CRABBOX_AGENT_SANDBOX_KUBECONFIG_or_KUBECONFIG_or_readable_default_kubeconfig"
fi
if [[ -z "$context" ]]; then
  classify_and_exit environment_blocked "reason=missing_context missing=CRABBOX_AGENT_SANDBOX_CONTEXT_or_agentSandbox.context"
fi
if [[ -z "$warm_pool" ]]; then
  classify_and_exit environment_blocked "reason=missing_warm_pool missing=CRABBOX_AGENT_SANDBOX_WARM_POOL_or_agentSandbox.warmPool"
fi

mkdir -p "$(dirname "$bin")"
if [[ -z "${CRABBOX_BIN:-}" || ! -x "$bin" ]]; then
  go build -trimpath -o "$bin" ./cmd/crabbox
fi

smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-agent-sandbox-smoke.XXXXXX")"
smoke_repo="$smoke_root/repo"
export XDG_STATE_HOME="$smoke_root/state"
export CRABBOX_AGENT_SANDBOX_SMOKE_VALUE="forwarded-ok"
export CRABBOX_SYNC_DELETE=true

mkdir -p "$smoke_repo"
cd "$smoke_repo"
git init -q
git config user.email smoke@example.com
git config user.name "Crabbox Agent Sandbox Smoke"
printf 'provider: agent-sandbox\nsync:\n  delete: true\n' >.crabbox.yaml
printf 'v1\n' >proof.txt
git add .crabbox.yaml proof.txt
git commit -qm "test: seed Agent Sandbox smoke fixture"

provider_args=(--provider agent-sandbox)
if [[ -n "$kubectl" ]]; then
  provider_args+=(--agent-sandbox-kubectl "$kubectl")
fi
if [[ -n "$kubeconfig" ]]; then
  provider_args+=(--agent-sandbox-kubeconfig "$kubeconfig")
elif [[ $kubeconfig_inherited -eq 1 ]]; then
  # Clear trusted config while leaving the inherited list for kubectl.
  provider_args+=(--agent-sandbox-kubeconfig "")
fi
provider_args+=(--agent-sandbox-context "$context" --agent-sandbox-namespace "$namespace" --agent-sandbox-warm-pool "$warm_pool" --agent-sandbox-workdir "$workdir")
if [[ -n "$container" ]]; then
  provider_args+=(--agent-sandbox-container "$container")
fi

trap - ERR
if doctor_output="$("$bin" doctor "${provider_args[@]}" 2>&1)"; then
  doctor_status=0
else
  doctor_status=$?
fi
trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
if [[ $doctor_status -ne 0 ]]; then
  if grep -Eiq 'quota|capacity|rate limit|too many requests|429|insufficient' <<<"$doctor_output"; then
    classify_and_exit quota_blocked "$doctor_output"
  fi
  classify_and_exit environment_blocked "$doctor_output"
fi

slug="${CRABBOX_AGENT_SANDBOX_SMOKE_SLUG:-agent-sandbox-smoke-$$}"

trap - ERR
if run_output="$("$bin" run "${provider_args[@]}" --keep --slug "$slug" --timing-json --allow-env CRABBOX_AGENT_SANDBOX_SMOKE_VALUE -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v1 && test "$CRABBOX_AGENT_SANDBOX_SMOKE_VALUE" = forwarded-ok && printf AGENT_SANDBOX_SMOKE_OK' 2>&1)"; then
  run_status=0
else
  run_status=$?
fi
trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
if parsed_created_slug="$(extract_created_identifier "$run_output")"; then
  created_slug="$parsed_created_slug"
fi
if [[ $run_status -ne 0 ]]; then
  if grep -Eiq 'quota|capacity|rate limit|too many requests|429|insufficient|no warm pool|warm.?pool.*empty' <<<"$run_output"; then
    classify_and_exit quota_blocked "$run_output"
  fi
  if grep -Eiq 'unauthorized|forbidden|rbac|kubeconfig|context|no such host|timeout|TLS|x509|connection refused|not found|permission' <<<"$run_output"; then
    classify_and_exit environment_blocked "$run_output"
  fi
  classify_and_exit diagnostic_only "$run_output"
fi
if ! grep -q 'AGENT_SANDBOX_SMOKE_OK' <<<"$run_output"; then
  classify_and_exit diagnostic_only "run succeeded but archive-sync marker was missing"
fi
if [[ -z "$created_slug" ]]; then
  classify_and_exit diagnostic_only "run succeeded but created claim identifier was missing"
fi

"$bin" run "${provider_args[@]}" --id "$created_slug" --no-sync -- \
  /bin/sh -lc 'printf stale > stale-remote.txt' >/dev/null
printf 'v2\n' >proof.txt
git add proof.txt
git commit -qm "test: update Agent Sandbox smoke fixture"
if ! replace_output="$("$bin" run "${provider_args[@]}" --id "$created_slug" --allow-env CRABBOX_AGENT_SANDBOX_SMOKE_VALUE -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v2 && test "$CRABBOX_AGENT_SANDBOX_SMOKE_VALUE" = forwarded-ok && test ! -e stale-remote.txt && printf AGENT_SANDBOX_REPLACE_OK' 2>&1)"; then
  classify_and_exit diagnostic_only "$replace_output"
fi
if ! grep -q 'AGENT_SANDBOX_REPLACE_OK' <<<"$replace_output"; then
  classify_and_exit diagnostic_only "retained run succeeded but replacement-sync marker was missing"
fi

"$bin" status "${provider_args[@]}" --id "$created_slug" --wait --wait-timeout "${CRABBOX_AGENT_SANDBOX_SMOKE_WAIT_TIMEOUT:-90s}" >/dev/null
"$bin" list "${provider_args[@]}" --json >/dev/null
"$bin" stop "${provider_args[@]}" "$created_slug" >/dev/null 2>&1
created_slug=""

trap - EXIT
rm -rf -- "$smoke_root"
smoke_root=""

classify_and_exit live_agent_sandbox_smoke_passed
