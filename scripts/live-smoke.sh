#!/usr/bin/env bash
set -euo pipefail

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  echo "set CRABBOX_LIVE=1 to run live provider smoke tests" >&2
  exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cb="${CRABBOX_BIN:-$root/bin/crabbox}"
repo="${CRABBOX_LIVE_REPO:-$PWD}"
providers=",${CRABBOX_LIVE_PROVIDERS-aws,hetzner},"
default_live_command='if [ -f go.mod ]; then test -f go.mod; elif [ -f package.json ]; then test -f package.json; else test -d .; fi; printf crabbox-live-ok; printf " pwd=%s\n" "$PWD"'
live_command="${CRABBOX_LIVE_COMMAND:-$default_live_command}"
config_paths=()
trusted_config_path=""

run_in_repo() {
  (cd "$repo" && "$@")
}

add_config_path() {
  local path="$1"
  [[ -n "$path" ]] || return 0
  if [[ "$path" != /* ]]; then
    path="$repo/$path"
  fi
  config_paths+=("$path")
}

if [[ -n "${CRABBOX_CONFIG:-}" ]]; then
  add_config_path "$CRABBOX_CONFIG"
  if [[ "$CRABBOX_CONFIG" == /* ]]; then
    trusted_config_path="$CRABBOX_CONFIG"
  else
    trusted_config_path="$repo/$CRABBOX_CONFIG"
  fi
else
  user_config_path="$(run_in_repo "$cb" config path 2>/dev/null || true)"
  add_config_path "$user_config_path"
  if [[ -n "$user_config_path" ]]; then
    if [[ "$user_config_path" == /* ]]; then
      trusted_config_path="$user_config_path"
    else
      trusted_config_path="$repo/$user_config_path"
    fi
  fi
  add_config_path "$repo/crabbox.yaml"
  add_config_path "$repo/.crabbox.yaml"
fi

need_tool() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required tool: $1" >&2
    exit 2
  fi
}

config_value() {
  local key_path="$1"
  command -v ruby >/dev/null 2>&1 || return 1
  local value=""
  local found=0
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
      value="$candidate"
      found=1
    fi
  done
  if [[ "$found" == "1" ]]; then
    printf '%s' "$value"
    return 0
  fi
  return 1
}

config_value_path() {
  local key_path="$1"
  command -v ruby >/dev/null 2>&1 || return 1
  local found=""
  local path
  for path in "${config_paths[@]}"; do
    [[ -r "$path" ]] || continue
    if ruby -ryaml -e '
      value = ARGV[1].split(".").reduce(YAML.load_file(ARGV[0])) do |memo, key|
        memo.is_a?(Hash) ? memo[key] : nil
      end
      exit 3 if value.nil? || value.to_s.empty?
    ' "$path" "$key_path" 2>/dev/null; then
      found="$path"
    fi
  done
  [[ -n "$found" ]] || return 1
  printf '%s' "$found"
}

trusted_config_value() {
  local key_path="$1"
  command -v ruby >/dev/null 2>&1 || return 1
  local path="$trusted_config_path"
  [[ -r "$path" ]] || return 1
  ruby -ryaml -e '
    value = ARGV[1].split(".").reduce(YAML.load_file(ARGV[0])) do |memo, key|
      memo.is_a?(Hash) ? memo[key] : nil
    end
    exit 3 if value.nil? || value.to_s.empty?
    print value
  ' "$path" "$key_path" 2>/dev/null
}

capture_run() {
  local __name="$1"
  shift
  local __out
  if ! __out="$("$@" 2>&1)"; then
    printf '%s\n' "$__out"
    return 1
  fi
  printf -v "$__name" '%s' "$__out"
}

capture_stdout() {
  local __name="$1"
  shift
  local __stderr
  __stderr="$(mktemp)"
  local __out
  local __status=0
  if __out="$("$@" 2>"$__stderr")"; then
    __status=0
  else
    __status=$?
  fi
  cat "$__stderr" >&2
  rm -f "$__stderr"
  if [[ "$__status" -ne 0 ]]; then
    printf '%s\n' "$__out"
    return "$__status"
  fi
  printf -v "$__name" '%s' "$__out"
}

capture_run_live() {
  local __name="$1"
  shift
  local __log
  __log="$(mktemp)"
  local __status=0
  if "$@" 2>&1 | tee "$__log"; then
    __status=0
  else
    __status=$?
  fi
  local __out
  __out="$(cat "$__log")"
  rm -f "$__log"
  printf -v "$__name" '%s' "$__out"
  if [[ "$__status" -ne 0 ]]; then
    return "$__status"
  fi
}

log_step() {
  printf '[live-smoke %s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2
}

has_provider() {
  [[ "$providers" == *",$1,"* ]]
}

extract_lease() {
  grep -Eo '(cbx_[a-f0-9]{12}|sem_[A-Za-z0-9][A-Za-z0-9._-]*)' | head -1
}

extract_slug() {
  sed -n 's/.*slug=\([^ ]*\).*/\1/p' | grep -Ev '^-$' | tail -1
}

extract_tenki_session() {
  sed -n 's/.*tenki_session=\([^ ]*\).*/\1/p' | tail -1
}

extract_coder_workspace() {
  sed -n 's/.*workspace=\([^ ]*\).*/\1/p' | tail -1
}

stop_lease() {
  local id="$1"
  local slug="${2:-}"
  if [[ -n "$slug" ]]; then
    run_in_repo "$cb" stop "$slug" || run_in_repo "$cb" stop "$id" || true
  else
    run_in_repo "$cb" stop "$id" || true
  fi
}

stop_provider_lease() {
  local provider="$1"
  local id="$2"
  local slug="${3:-}"
  if [[ -n "$slug" ]]; then
    run_in_repo "$cb" stop --provider "$provider" "$slug" || run_in_repo "$cb" stop --provider "$provider" "$id" || true
  else
    run_in_repo "$cb" stop --provider "$provider" "$id" || true
  fi
}

blacksmith_workflow_path_like() {
  local workflow="$1"
  [[ "$workflow" == .github/* || "$workflow" == */* || "$workflow" == *.yml || "$workflow" == *.yaml ]]
}

validate_blacksmith_workflow() {
  local workflow="$1"
  if ! blacksmith_workflow_path_like "$workflow"; then
    return 0
  fi

  local path="$repo/$workflow"
  if [[ ! -f "$path" ]]; then
    echo "blacksmith-testbox smoke requires a Testbox workflow; missing $workflow" >&2
    echo "set CRABBOX_BLACKSMITH_WORKFLOW to a workflow containing useblacksmith/testbox, or use CRABBOX_LIVE_PROVIDERS=aws as a fallback" >&2
    return 2
  fi
  if ! rg -q 'useblacksmith/(testbox|begin-testbox|run-testbox)' "$path"; then
    echo "blacksmith-testbox smoke requires $workflow to contain a useblacksmith/testbox, useblacksmith/begin-testbox, or useblacksmith/run-testbox step" >&2
    echo "set CRABBOX_BLACKSMITH_WORKFLOW to a configured Testbox workflow, or use CRABBOX_LIVE_PROVIDERS=aws as a fallback" >&2
    return 2
  fi
}

provider_smoke() (
  need_tool jq
  need_tool rg

  local provider="$1"
  shift
  local CRABBOX_PROVIDER="$provider"
  export CRABBOX_PROVIDER
  local lease=""
  local slug=""
  # shellcheck disable=SC2329 # The subshell EXIT trap invokes this cleanup.
  cleanup() {
    trap - EXIT
    if [[ -n "$lease" ]]; then
      stop_provider_lease "$provider" "$lease" "$slug"
      lease=""
      slug=""
    fi
  }
  trap cleanup EXIT

  local out
  capture_run out run_in_repo "$cb" warmup --provider "$provider" "$@"
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  run_in_repo "$cb" status --provider "$provider" --id "$slug" --wait --wait-timeout 90s
  run_in_repo "$cb" inspect --provider "$provider" --id "$slug" --json | jq '{id,slug,provider,state,serverType,host,ready,lastTouchedAt,expiresAt}'
  run_in_repo "$cb" ssh --provider "$provider" --id "$slug"
  run_in_repo "$cb" cache stats --id "$slug" --json | jq 'if type=="array" then {items:length,kinds:[.[].kind]} else {keys:keys} end'

  local runout
  # shellcheck disable=SC2016 # expanded by the remote shell.
  capture_run runout run_in_repo "$cb" run --provider "$provider" --id "$slug" --shell -- "$live_command"
  printf '%s\n' "$runout"
  local runid
  runid="$(printf '%s\n' "$runout" | rg -o 'run_[a-f0-9]{12}' | tail -1 || true)"
  if needs_coordinator_preamble; then
    run_in_repo "$cb" history --lease "$lease" --limit 5
    if [[ -n "$runid" ]]; then
      run_in_repo "$cb" logs "$runid" | tail -80
    fi
  fi
  stop_provider_lease "$provider" "$lease" "$slug"
  lease=""
)

blacksmith_smoke() {
  need_tool jq
  need_tool rg

  local workflow="${CRABBOX_BLACKSMITH_WORKFLOW:-$(config_value blacksmith.workflow || config_value actions.workflow || true)}"
  workflow="${workflow:-.github/workflows/ci-check-testbox.yml}"
  local job="${CRABBOX_BLACKSMITH_JOB:-$(config_value blacksmith.job || config_value actions.job || true)}"
  job="${job:-check}"
  local ref="${CRABBOX_BLACKSMITH_REF:-$(config_value blacksmith.ref || config_value actions.ref || true)}"
  ref="${ref:-main}"
  local org="${CRABBOX_BLACKSMITH_ORG:-}"
  if [[ -z "$org" ]]; then
    need_tool ruby
    org="$(config_value blacksmith.org || true)"
  fi
  if [[ -z "$org" ]]; then
    local actions_repo
    actions_repo="$(config_value actions.repo || true)"
    if [[ "$actions_repo" == */* ]]; then
      org="${actions_repo%%/*}"
    fi
  fi
  if [[ -z "$org" ]]; then
    echo "blacksmith-testbox smoke requires CRABBOX_BLACKSMITH_ORG, blacksmith.org, or actions.repo in config" >&2
    return 2
  fi
  validate_blacksmith_workflow "$workflow"

  run_in_repo "$cb" list --provider blacksmith-testbox --json | jq '.[0] // empty'
  run_in_repo "$cb" run \
    --provider blacksmith-testbox \
    --blacksmith-org "$org" \
    --blacksmith-workflow "$workflow" \
    --blacksmith-job "$job" \
    --blacksmith-ref "$ref" \
    --idle-timeout "${CRABBOX_BLACKSMITH_IDLE_TIMEOUT:-10m}" \
    --shell -- 'echo blacksmith-crabbox-ok && pwd'
}

e2b_smoke() {
  need_tool jq
  need_tool rg

  local e2b_api_key="${CRABBOX_E2B_API_KEY:-${E2B_API_KEY:-}}"
  if [[ -z "$e2b_api_key" ]]; then
    echo "e2b smoke requires CRABBOX_E2B_API_KEY or E2B_API_KEY" >&2
    return 2
  fi

  local lease=""
  local slug=""
  cleanup() {
    trap - RETURN ERR
    if [[ -n "$lease" ]]; then
      stop_provider_lease e2b "$lease" "$slug"
      lease=""
      slug=""
    fi
  }
  trap cleanup RETURN ERR

  local out
  capture_run out run_in_repo "$cb" warmup --provider e2b --e2b-template "${CRABBOX_E2B_TEMPLATE:-base}" --timing-json
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  run_in_repo "$cb" status --provider e2b --id "$slug" --wait
  run_in_repo "$cb" run --provider e2b --id "$slug" --no-sync -- echo crabbox-e2b-ok
  run_in_repo "$cb" list --provider e2b --json | jq 'map({id:(.id // .CloudID),slug:(.slug // .labels.slug),provider:(.provider // .Provider // .labels.provider),state:(.state // .labels.state // .status)})'
  stop_provider_lease e2b "$lease" "$slug"
  lease=""
}

modal_smoke() {
  need_tool jq
  need_tool rg

  local python_bin="${CRABBOX_MODAL_PYTHON:-$(config_value modal.python || true)}"
  python_bin="${python_bin:-python3}"
  if ! run_in_repo "$python_bin" -c 'import modal' >/dev/null 2>&1; then
    echo "modal smoke requires the Modal Python client for $python_bin; install modal and authenticate with python3 -m modal setup or Modal token env vars" >&2
    return 2
  fi

  local lease=""
  local slug=""
  cleanup() {
    trap - RETURN ERR
    if [[ -n "$lease" ]]; then
      stop_provider_lease modal "$lease" "$slug"
      lease=""
      slug=""
    fi
  }
  trap cleanup RETURN ERR

  local out
  capture_run out run_in_repo "$cb" warmup \
    --provider modal \
    --modal-app "${CRABBOX_MODAL_APP:-crabbox}" \
    --modal-image "${CRABBOX_MODAL_IMAGE:-python:3.13-slim}" \
    --modal-python "$python_bin" \
    --timing-json
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  run_in_repo "$cb" status --provider modal --id "$slug" --wait
  run_in_repo "$cb" run --provider modal --id "$slug" --modal-python "$python_bin" --no-sync -- python -c 'print("crabbox-modal-ok")'
  run_in_repo "$cb" list --provider modal --json | jq 'map({id:(.id // .CloudID),slug:(.slug // .labels.slug),provider:(.provider // .Provider // .labels.provider),state:(.state // .labels.state // .status)})'
  stop_provider_lease modal "$lease" "$slug"
  lease=""
}

coder_stop_lease() {
  local action="$1"
  local id="$2"
  local slug="${3:-}"
  local args=(stop --provider coder)
  if [[ "$action" == "delete" ]]; then
    args+=(--coder-delete-on-release)
  fi
  if [[ -n "$slug" ]]; then
    run_in_repo "$cb" "${args[@]}" "$slug" || run_in_repo "$cb" "${args[@]}" "$id" || true
  else
    run_in_repo "$cb" "${args[@]}" "$id" || true
  fi
}

coder_smoke() {
  need_tool jq
  need_tool rg

  local template="${CRABBOX_LIVE_CODER_TEMPLATE:-${CRABBOX_CODER_TEMPLATE:-$(config_value coder.template || true)}}"
  if [[ -z "$template" ]]; then
    echo "coder smoke requires CRABBOX_LIVE_CODER_TEMPLATE, CRABBOX_CODER_TEMPLATE, or coder.template" >&2
    return 2
  fi

  local ttl="${CRABBOX_LIVE_CODER_TTL:-15m}"
  local idle_timeout="${CRABBOX_LIVE_CODER_IDLE_TIMEOUT:-5m}"
  local slug_prefix="${CRABBOX_LIVE_CODER_SLUG_PREFIX:-coder-smoke-$$}"
  local stop_slug="${slug_prefix}-stop"
  local delete_slug="${slug_prefix}-delete"
  local coder_args=(--provider coder --coder-template "$template" --ttl "$ttl" --idle-timeout "$idle_timeout")
  if [[ -n "${CRABBOX_LIVE_CODER_PRESET:-}" ]]; then
    coder_args+=(--coder-preset "$CRABBOX_LIVE_CODER_PRESET")
  fi

  local lease=""
  local slug=""
  local delete_lease=""
  local delete_workspace=""
  cleanup() {
    trap - RETURN ERR
    if [[ -n "$delete_lease" ]]; then
      coder_stop_lease delete "$delete_lease" "$delete_slug"
      delete_lease=""
      delete_workspace=""
    fi
    if [[ -n "$lease" ]]; then
      coder_stop_lease stop "$lease" "$slug"
      lease=""
      slug=""
    fi
  }
  trap cleanup RETURN ERR

  run_in_repo "$cb" doctor --provider coder
  run_in_repo "$cb" cleanup --provider coder --dry-run

  local out
  log_step "coder warmup stop_on_release slug=$stop_slug"
  capture_run_live out run_in_repo "$cb" warmup "${coder_args[@]}" --slug "$stop_slug" --timing-json
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  local workspace
  workspace="$(printf '%s\n' "$out" | extract_coder_workspace)"
  test -n "$lease"
  test -n "$slug"
  test -n "$workspace"

  run_in_repo "$cb" status --provider coder --id "$slug" --wait --wait-timeout 120s
  run_in_repo "$cb" inspect --provider coder --id "$slug" --json | jq '{id,slug,provider,state,serverType,host,ready,lastTouchedAt,expiresAt,labels}'
  run_in_repo "$cb" ssh --provider coder --id "$slug"
  local runout
  capture_run_live runout run_in_repo "$cb" run --provider coder --id "$slug" --shell -- "$live_command"
  local runid
  runid="$(printf '%s\n' "$runout" | rg -o 'run_[a-f0-9]{12}' | tail -1 || true)"
  run_in_repo "$cb" history --lease "$lease" --limit 5
  if [[ -n "$runid" ]]; then
    run_in_repo "$cb" logs "$runid" | tail -80
  fi
  log_step "coder stop stop_on_release slug=$slug workspace=$workspace"
  run_in_repo "$cb" stop --provider coder "$slug" || run_in_repo "$cb" stop --provider coder "$lease"
  lease=""
  slug=""
  run_in_repo "$cb" status --provider coder --id "$workspace" --json | jq '{id,slug,provider,state,ready,labels}'

  log_step "coder warmup delete_on_release slug=$delete_slug"
  capture_run_live out run_in_repo "$cb" warmup "${coder_args[@]}" --coder-delete-on-release --slug "$delete_slug" --timing-json
  delete_lease="$(printf '%s\n' "$out" | extract_lease)"
  delete_workspace="$(printf '%s\n' "$out" | extract_coder_workspace)"
  test -n "$delete_lease"
  test -n "$delete_workspace"
  run_in_repo "$cb" status --provider coder --id "$delete_slug" --wait --wait-timeout 120s
  run_in_repo "$cb" run --provider coder --id "$delete_slug" --shell -- "$live_command"
  log_step "coder stop delete_on_release slug=$delete_slug workspace=$delete_workspace"
  run_in_repo "$cb" stop --provider coder --coder-delete-on-release "$delete_slug" || run_in_repo "$cb" stop --provider coder --coder-delete-on-release "$delete_lease"
  delete_lease=""
  delete_workspace=""

  run_in_repo "$cb" list --provider coder --json | jq 'map({id:(.id // .CloudID),slug:(.slug // .labels.slug),provider:(.provider // .Provider // .labels.provider),state:(.state // .labels.state // .status),host:(.host // .Host),workspace:(.labels.coder_workspace // .labels.coder_workspace_ref // null)})'
  run_in_repo "$cb" cleanup --provider coder --dry-run
}

sealos_config_value() {
  local env_name="$1"
  local config_key="$2"
  local default_value="${3:-}"
  local value="${!env_name:-}"
  if [[ -z "$value" ]]; then
    value="$(config_value "sealosDevbox.$config_key" || true)"
  fi
  printf '%s' "${value:-$default_value}"
}

sealos_expand_home_path() {
  local value="$1"
  if [[ "$value" == "~" ]]; then
    printf '%s' "$HOME"
  elif [[ "$value" == "~/"* ]]; then
    printf '%s/%s' "$HOME" "${value:2}"
  else
    printf '%s' "$value"
  fi
}

sealos_has_kubeconfig() {
  local kubeconfig="$1"
  if [[ -n "$kubeconfig" ]]; then
    [[ -r "$(sealos_expand_home_path "$kubeconfig")" ]]
    return
  fi
  if [[ -n "${KUBECONFIG:-}" ]]; then
    return 0
  fi
  [[ -r "$HOME/.kube/config" ]]
}

sealos_blocked() {
  local reason="$1"
  shift || true
  printf 'classification=environment_blocked reason=%s provider=sealos-devbox' "$reason"
  if [[ "$#" -gt 0 ]]; then
    printf ' %s' "$*"
  fi
  printf '\n'
}

sealos_smoke() {
  need_tool jq
  need_tool rg

  local kubectl
  local kubeconfig
  local context
  local namespace
  local image
  local template_id
  local network
  local ssh_gateway_host
  local ssh_gateway_port
  local node_host
  local kubectl_path
  kubectl="$(sealos_config_value CRABBOX_SEALOS_DEVBOX_KUBECTL kubectl kubectl)"
  kubectl_path="$(sealos_expand_home_path "$kubectl")"
  kubeconfig="$(sealos_config_value CRABBOX_SEALOS_DEVBOX_KUBECONFIG kubeconfig)"
  context="$(sealos_config_value CRABBOX_SEALOS_DEVBOX_CONTEXT context)"
  namespace="$(sealos_config_value CRABBOX_SEALOS_DEVBOX_NAMESPACE namespace default)"
  image="$(sealos_config_value CRABBOX_SEALOS_DEVBOX_IMAGE image)"
  template_id="$(sealos_config_value CRABBOX_SEALOS_DEVBOX_TEMPLATE_ID templateID)"
  network="$(sealos_config_value CRABBOX_SEALOS_DEVBOX_NETWORK network SSHGate)"
  ssh_gateway_host="$(sealos_config_value CRABBOX_SEALOS_DEVBOX_SSH_GATEWAY_HOST sshGatewayHost)"
  ssh_gateway_port="$(sealos_config_value CRABBOX_SEALOS_DEVBOX_SSH_GATEWAY_PORT sshGatewayPort 2233)"
  node_host="$(sealos_config_value CRABBOX_SEALOS_DEVBOX_NODE_HOST nodeHost)"

  if ! command -v "$kubectl_path" >/dev/null 2>&1; then
    sealos_blocked missing_kubectl "kubectl=$kubectl_path"
    return 0
  fi
  if ! sealos_has_kubeconfig "$kubeconfig"; then
    sealos_blocked missing_kubeconfig
    return 0
  fi
  if [[ -z "$context" ]]; then
    sealos_blocked missing_context
    return 0
  fi
  if [[ -z "$namespace" ]]; then
    sealos_blocked missing_namespace
    return 0
  fi
  if [[ -z "$image" ]]; then
    sealos_blocked missing_image
    return 0
  fi
  local normalized_network
  normalized_network="$(printf '%s' "$network" | tr '[:upper:]' '[:lower:]')"
  case "$normalized_network" in
    sshgate|ssh-gate|ssh_gate)
      if [[ -z "$ssh_gateway_host" ]]; then
        sealos_blocked missing_ssh_gateway_host
        return 0
      fi
      ;;
    nodeport|node-port|node_port)
      if [[ -z "$node_host" ]]; then
        sealos_blocked missing_node_host
        return 0
      fi
      ;;
    *)
      sealos_blocked invalid_network "network=$network"
      return 0
      ;;
  esac

  local route_args=(--provider sealos-devbox)
  route_args+=(--sealos-devbox-kubectl "$kubectl_path")
  if [[ -n "$kubeconfig" ]]; then
    route_args+=(--sealos-devbox-kubeconfig "$kubeconfig")
  fi
  route_args+=(--sealos-devbox-context "$context")
  route_args+=(--sealos-devbox-namespace "$namespace")
  if [[ -n "$image" ]]; then
    route_args+=(--sealos-devbox-image "$image")
  fi
  if [[ -n "$template_id" ]]; then
    route_args+=(--sealos-devbox-template-id "$template_id")
  fi
  route_args+=(--sealos-devbox-network "$network")
  if [[ -n "$ssh_gateway_host" ]]; then
    route_args+=(--sealos-devbox-ssh-gateway-host "$ssh_gateway_host")
  fi
  if [[ -n "$ssh_gateway_port" ]]; then
    route_args+=(--sealos-devbox-ssh-gateway-port "$ssh_gateway_port")
  fi
  if [[ -n "$node_host" ]]; then
    route_args+=(--sealos-devbox-node-host "$node_host")
  fi
  route_args+=(--sealos-devbox-delete-on-release=true)

  local doctor_json
  if ! doctor_json="$(run_in_repo "$cb" doctor "${route_args[@]}" --json 2>&1)"; then
    sealos_blocked doctor_failed
    printf '%s\n' "$doctor_json"
    return 0
  fi
  printf '%s\n' "$doctor_json"
  if ! printf '%s\n' "$doctor_json" | jq -e '.status == "ready" or .ok == true' >/dev/null; then
    sealos_blocked doctor_not_ready
    return 0
  fi
  for check in create update delete; do
    if ! printf '%s\n' "$doctor_json" | jq -e --arg check "rbac.${check}.devboxes" '.checks[]? | select(.check == $check and .status == "ok")' >/dev/null; then
      sealos_blocked "missing_rbac_${check}_devboxes"
      return 0
    fi
  done

  run_in_repo "$cb" cleanup "${route_args[@]}" --dry-run

  local lease=""
  local slug=""
  local smoke_slug="${CRABBOX_LIVE_SEALOS_DEVBOX_SLUG:-sealos-devbox-smoke-$$}"
  cleanup() {
    trap - RETURN ERR
    if [[ -n "$lease" ]]; then
      if [[ -n "$slug" ]]; then
        run_in_repo "$cb" stop "${route_args[@]}" "$slug" || run_in_repo "$cb" stop "${route_args[@]}" "$lease" || true
      else
        run_in_repo "$cb" stop "${route_args[@]}" "$lease" || true
      fi
      lease=""
      slug=""
    fi
  }
  trap cleanup RETURN ERR

  local out
  log_step "sealos-devbox warmup slug=$smoke_slug"
  capture_run_live out run_in_repo "$cb" warmup "${route_args[@]}" --keep --slug "$smoke_slug" --ttl "${CRABBOX_LIVE_SEALOS_DEVBOX_TTL:-15m}" --idle-timeout "${CRABBOX_LIVE_SEALOS_DEVBOX_IDLE_TIMEOUT:-5m}" --timing-json
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  run_in_repo "$cb" status "${route_args[@]}" --id "$slug" --wait --wait-timeout "${CRABBOX_LIVE_SEALOS_DEVBOX_WAIT_TIMEOUT:-5m}" --json | jq '{id,slug,provider,state,serverType,host,ready,lastTouchedAt,expiresAt}'
  run_in_repo "$cb" ssh "${route_args[@]}" --id "$slug"
  local runout
  capture_run_live runout run_in_repo "$cb" run "${route_args[@]}" --id "$slug" --shell -- "$live_command"
  local runid
  runid="$(printf '%s\n' "$runout" | rg -o 'run_[a-f0-9]{12}' | tail -1 || true)"
  if [[ -n "$runid" ]]; then
    run_in_repo "$cb" logs "$runid" | tail -80
  fi
  run_in_repo "$cb" stop "${route_args[@]}" "$slug" || run_in_repo "$cb" stop "${route_args[@]}" "$lease"
  run_in_repo "$cb" list "${route_args[@]}" --json | jq -e --arg lease "$lease" --arg slug "$slug" \
    'all(.[]; (.id // .leaseID // .labels.lease // "") != $lease and (.slug // .labels.slug // "") != $slug)'
  lease=""
  slug=""
  run_in_repo "$cb" cleanup "${route_args[@]}" --dry-run
}

daytona_smoke() {
  need_tool jq

  local snapshot="${CRABBOX_DAYTONA_SNAPSHOT:-${DAYTONA_SNAPSHOT:-$(config_value daytona.snapshot || true)}}"
  if [[ -z "$snapshot" ]]; then
    echo "daytona smoke requires CRABBOX_DAYTONA_SNAPSHOT, DAYTONA_SNAPSHOT, or daytona.snapshot" >&2
    return 2
  fi
  run_in_repo "$cb" run --provider daytona --daytona-snapshot "$snapshot" --no-sync -- echo crabbox-daytona-ok
  run_in_repo "$cb" list --provider daytona --json | jq 'map({id:(.id // .CloudID),slug:(.slug // .labels.slug),provider:(.provider // .Provider // .labels.provider),state:(.state // .labels.state // .status)})'
}

namespace_smoke() {
  need_tool jq

  if ! command -v devbox >/dev/null 2>&1; then
    echo "namespace-devbox smoke requires the Namespace devbox CLI on PATH" >&2
    return 2
  fi
  run_in_repo "$cb" run \
    --provider namespace-devbox \
    --namespace-size "${CRABBOX_NAMESPACE_SIZE:-S}" \
    --namespace-delete-on-release \
    --no-sync -- echo crabbox-namespace-ok
  run_in_repo "$cb" list --provider namespace-devbox --json | jq 'map({id:.id,slug:.slug,provider:.provider,state:.state})'
}

semaphore_smoke() {
  need_tool jq
  need_tool rg

  local semaphore_host="${CRABBOX_SEMAPHORE_HOST:-${SEMAPHORE_HOST:-$(config_value semaphore.host || true)}}"
  local semaphore_project="${CRABBOX_SEMAPHORE_PROJECT:-${SEMAPHORE_PROJECT:-$(config_value semaphore.project || true)}}"
  local semaphore_token="${CRABBOX_SEMAPHORE_TOKEN:-${SEMAPHORE_API_TOKEN:-$(config_value semaphore.token || true)}}"
  if [[ -z "$semaphore_host" ]]; then
    echo "semaphore smoke requires CRABBOX_SEMAPHORE_HOST, SEMAPHORE_HOST, or semaphore.host" >&2
    return 2
  fi
  if [[ -z "$semaphore_project" ]]; then
    echo "semaphore smoke requires CRABBOX_SEMAPHORE_PROJECT, SEMAPHORE_PROJECT, or semaphore.project" >&2
    return 2
  fi
  if [[ -z "$semaphore_token" ]]; then
    echo "semaphore smoke requires CRABBOX_SEMAPHORE_TOKEN, SEMAPHORE_API_TOKEN, or semaphore.token" >&2
    return 2
  fi

  local lease=""
  local slug=""
  cleanup() {
    trap - RETURN ERR
    if [[ -n "$lease" ]]; then
      stop_provider_lease semaphore "$lease" "$slug"
      lease=""
      slug=""
    fi
  }
  trap cleanup RETURN ERR

  local out
  capture_run out run_in_repo "$cb" warmup --provider semaphore --semaphore-host "$semaphore_host" --semaphore-project "$semaphore_project" --semaphore-idle-timeout "${CRABBOX_SEMAPHORE_IDLE_TIMEOUT:-10m}"
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  run_in_repo "$cb" status --provider semaphore --id "$slug" --wait --wait-timeout 120s
  run_in_repo "$cb" run --provider semaphore --id "$slug" --no-sync -- echo crabbox-semaphore-ok
  run_in_repo "$cb" list --provider semaphore --json | jq 'map({id:.id,slug:.slug,provider:.provider,state:.state})'
  stop_provider_lease semaphore "$lease" "$slug"
  lease=""
}

wandb_smoke() {
  CRABBOX_BIN="$cb" CRABBOX_LIVE_REPO="$repo" "$root/scripts/wandb-smoke.sh"
}

incus_smoke() {
  need_tool jq
  need_tool rg

  local delete_args=(--provider incus --incus-delete-on-release=true)
  local delete_lease_args=("${delete_args[@]}" --ttl "${CRABBOX_LIVE_INCUS_TTL:-15m}" --idle-timeout "${CRABBOX_LIVE_INCUS_IDLE_TIMEOUT:-5m}")
  local retain_args=(--provider incus --incus-delete-on-release=false)
  local retain_lease_args=("${retain_args[@]}" --ttl "${CRABBOX_LIVE_INCUS_RETAIN_TTL:-15m}" --idle-timeout "${CRABBOX_LIVE_INCUS_RETAIN_IDLE_TIMEOUT:-5m}")
  local delete_wait_timeout="${CRABBOX_LIVE_INCUS_WAIT_TIMEOUT:-5m}"
  local retain_wait_timeout="${CRABBOX_LIVE_INCUS_RETAIN_WAIT_TIMEOUT:-5m}"
  local incus_run_debug="${CRABBOX_LIVE_INCUS_RUN_DEBUG:-${CRABBOX_LIVE_DEBUG:-0}}"
  local run_args=(--timing-json)
  if [[ "$incus_run_debug" == "1" ]]; then
    run_args+=(--debug)
  fi
  local retained_command="${CRABBOX_LIVE_INCUS_RETAIN_COMMAND:-$live_command}"

  local lease=""
  local slug=""
  local retained_lease=""
  local retained_slug=""
  cleanup() {
    trap - RETURN ERR
    if [[ -n "$retained_lease" ]]; then
      if [[ -n "$retained_slug" ]]; then
        run_in_repo "$cb" stop --provider incus --incus-delete-on-release=true "$retained_slug" || run_in_repo "$cb" stop --provider incus --incus-delete-on-release=true "$retained_lease" || true
      else
        run_in_repo "$cb" stop --provider incus --incus-delete-on-release=true "$retained_lease" || true
      fi
    fi
    if [[ -n "$lease" ]]; then
      if [[ -n "$slug" ]]; then
        run_in_repo "$cb" stop "${delete_args[@]}" "$slug" || run_in_repo "$cb" stop "${delete_args[@]}" "$lease" || true
      else
        run_in_repo "$cb" stop "${delete_args[@]}" "$lease" || true
      fi
    fi
    retained_lease=""
    retained_slug=""
    lease=""
    slug=""
  }
  trap cleanup RETURN ERR

  local doctor_out
  log_step "incus doctor"
  capture_run doctor_out run_in_repo "$cb" doctor --provider incus --json
  printf '%s\n' "$doctor_out"
  printf '%s\n' "$doctor_out" | jq '{ok,checks:[.checks[] | select(.check=="provider") | {status,details,message}]}'

  local out
  local delete_slug="${CRABBOX_LIVE_INCUS_SLUG:-incus-smoke-$$}"
  log_step "incus warmup delete_on_release=true slug=$delete_slug"
  capture_run_live out run_in_repo "$cb" warmup "${delete_lease_args[@]}" --slug "$delete_slug" --timing-json
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  log_step "incus status wait delete_on_release=true slug=$slug timeout=$delete_wait_timeout"
  if ! run_in_repo "$cb" status "${delete_args[@]}" --id "$slug" --wait --wait-timeout "$delete_wait_timeout"; then
    log_step "incus status wait failed, collecting postmortem slug=$slug"
    run_in_repo "$cb" inspect --provider incus --id "$slug" --json || true
    run_in_repo "$cb" list --provider incus --json | jq 'map({id:(.id // .CloudID),slug:(.slug // .labels.slug),provider:(.provider // .Provider // .labels.provider),state:(.state // .labels.state // .status),host:(.host // .Host)})' || true
    return 1
  fi
  run_in_repo "$cb" inspect --provider incus --id "$slug" --json | jq '{id,slug,provider,state,serverType,host,ready,lastTouchedAt,expiresAt}'
  local runout
  log_step "incus run delete_on_release=true slug=$slug"
  capture_run_live runout run_in_repo "$cb" run "${delete_args[@]}" --id "$slug" "${run_args[@]}" --shell -- "$live_command"
  run_in_repo "$cb" list --provider incus --json | jq 'map({id:(.id // .CloudID),slug:(.slug // .labels.slug),provider:(.provider // .Provider // .labels.provider),state:(.state // .labels.state // .status),host:(.host // .Host)})'
  log_step "incus stop delete_on_release=true slug=$slug"
  run_in_repo "$cb" stop "${delete_args[@]}" "$slug" || run_in_repo "$cb" stop "${delete_args[@]}" "$lease"
  lease=""
  slug=""

  local retain_slug="${CRABBOX_LIVE_INCUS_RETAINED_SLUG:-incus-retain-smoke-$$}"
  log_step "incus warmup delete_on_release=false slug=$retain_slug"
  capture_run_live out run_in_repo "$cb" warmup "${retain_lease_args[@]}" --slug "$retain_slug" --timing-json
  retained_lease="$(printf '%s\n' "$out" | extract_lease)"
  retained_slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$retained_lease"
  test -n "$retained_slug"

  log_step "incus status wait delete_on_release=false slug=$retained_slug timeout=$retain_wait_timeout"
  if ! run_in_repo "$cb" status "${retain_args[@]}" --id "$retained_slug" --wait --wait-timeout "$retain_wait_timeout"; then
    log_step "incus status wait failed, collecting postmortem slug=$retained_slug"
    run_in_repo "$cb" inspect --provider incus --id "$retained_slug" --json || true
    run_in_repo "$cb" list --provider incus --json | jq 'map({id:(.id // .CloudID),slug:(.slug // .labels.slug),provider:(.provider // .Provider // .labels.provider),state:(.state // .labels.state // .status),host:(.host // .Host)})' || true
    return 1
  fi
  log_step "incus run delete_on_release=false slug=$retained_slug"
  capture_run_live runout run_in_repo "$cb" run "${retain_args[@]}" --id "$retained_slug" "${run_args[@]}" --shell -- "$live_command"
  log_step "incus stop delete_on_release=false slug=$retained_slug"
  run_in_repo "$cb" stop "${retain_args[@]}" "$retained_slug" || run_in_repo "$cb" stop "${retain_args[@]}" "$retained_lease"
  run_in_repo "$cb" status "${retain_args[@]}" --id "$retained_slug" --json | jq '{id,slug,state,ready,host}'
  log_step "incus run retained reuse slug=$retained_slug"
  capture_run_live runout run_in_repo "$cb" run "${retain_args[@]}" --id "$retained_slug" "${run_args[@]}" --shell -- "$retained_command"
  log_step "incus stop delete_on_release=true slug=$retained_slug"
  run_in_repo "$cb" stop --provider incus --incus-delete-on-release=true "$retained_slug" || run_in_repo "$cb" stop --provider incus --incus-delete-on-release=true "$retained_lease"
  retained_lease=""
  retained_slug=""
}

sprites_smoke() {
  need_tool jq
  need_tool rg

  if ! command -v sprite >/dev/null 2>&1; then
    echo "sprites smoke requires the authenticated Sprites sprite CLI on PATH" >&2
    return 2
  fi
  local sprites_token="${CRABBOX_SPRITES_TOKEN:-${SPRITES_TOKEN:-${SPRITE_TOKEN:-${SETUP_SPRITE_TOKEN:-}}}}"
  if [[ -z "$sprites_token" ]]; then
    echo "sprites smoke requires CRABBOX_SPRITES_TOKEN, SPRITES_TOKEN, SPRITE_TOKEN, or SETUP_SPRITE_TOKEN" >&2
    return 2
  fi

  local lease=""
  local slug=""
  cleanup() {
    trap - RETURN ERR
    if [[ -n "$lease" ]]; then
      stop_provider_lease sprites "$lease" "$slug"
      lease=""
      slug=""
    fi
  }
  trap cleanup RETURN ERR

  local out
  capture_run out run_in_repo "$cb" warmup --provider sprites --timing-json
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  run_in_repo "$cb" status --provider sprites --id "$slug" --wait --wait-timeout 120s
  run_in_repo "$cb" ssh --provider sprites --id "$slug"
  run_in_repo "$cb" run --provider sprites --id "$slug" --shell -- 'echo crabbox-sprites-ok && pwd'
  run_in_repo "$cb" list --provider sprites --json | jq 'map({id:.id,slug:.slug,provider:.provider,state:.state})'
  stop_provider_lease sprites "$lease" "$slug"
  lease=""
}

tenki_smoke() {
  need_tool jq
  need_tool rg

  local tenki_cli="${CRABBOX_TENKI_CLI:-${TENKI_CLI:-tenki}}"
  need_tool "$tenki_cli"
  local tenki_endpoint="${CRABBOX_TENKI_ENDPOINT:-${TENKI_ENDPOINT:-$(config_value tenki.endpoint || true)}}"
  local tenki_sandbox_args=()
  if [[ -n "$tenki_endpoint" ]]; then
    tenki_sandbox_args+=(--endpoint "$tenki_endpoint")
  fi

  local auth
  capture_stdout auth "$tenki_cli" status --json
  if ! printf '%s\n' "$auth" | jq -e '.status | startswith("Logged in")' >/dev/null; then
    echo "tenki smoke requires an authenticated Tenki CLI; run tenki login" >&2
    return 2
  fi
  "$tenki_cli" --version
  printf '%s\n' "$auth" | jq '{status,api_endpoint,workspace_id,project_id}'

  local lease=""
  local slug=""
  local session=""
  cleanup() {
    trap - RETURN ERR
    if [[ -n "$lease" ]]; then
      stop_provider_lease tenki "$lease" "$slug"
      lease=""
      slug=""
    fi
  }
  trap cleanup RETURN ERR

  run_in_repo "$cb" doctor --provider tenki

  local out
  capture_run out run_in_repo "$cb" warmup \
    --provider tenki \
    --slug "${CRABBOX_LIVE_TENKI_SLUG:-tenki-smoke-$(date +%Y%m%d%H%M%S)-$$}" \
    --ttl "${CRABBOX_LIVE_TENKI_TTL:-15m}" \
    --idle-timeout "${CRABBOX_LIVE_TENKI_IDLE_TIMEOUT:-5m}" \
    --timing-json
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  session="$(printf '%s\n' "$out" | extract_tenki_session)"
  test -n "$lease"
  test -n "$slug"
  test -n "$session"

  run_in_repo "$cb" status --provider tenki --id "$slug" --wait --wait-timeout "${CRABBOX_LIVE_TENKI_WAIT_TIMEOUT:-120s}"
  run_in_repo "$cb" run --provider tenki --id "$slug" --no-sync -- echo crabbox-tenki-ok
  local list_json
  capture_stdout list_json run_in_repo "$cb" list --provider tenki --json
  printf '%s\n' "$list_json" | jq 'map({id,serverId,slug,provider,state})'
  if ! printf '%s\n' "$list_json" | jq -e --arg lease "$lease" --arg session "$session" \
    'any(.[]; .id == $lease and .serverId == $session and .provider == "tenki")' >/dev/null; then
    echo "tenki list JSON missing lease=$lease session=$session" >&2
    return 1
  fi

  "$tenki_cli" sandbox pause "${tenki_sandbox_args[@]}" --session "$session"
  local pause_timeout="${CRABBOX_LIVE_TENKI_PAUSE_TIMEOUT:-60}"
  local pause_deadline=$((SECONDS + pause_timeout))
  local state=""
  while (( SECONDS < pause_deadline )); do
    state="$("$tenki_cli" sandbox get "${tenki_sandbox_args[@]}" --output json "$session" | jq -r '.state // ""' | tr '[:upper:]' '[:lower:]')"
    [[ "$state" == "paused" ]] && break
    sleep 1
  done
  if [[ "$state" != "paused" ]]; then
    echo "tenki session did not pause within ${pause_timeout}s; last state=${state:-unknown}" >&2
    return 1
  fi

  local paused_out
  local paused_status=0
  if paused_out="$(run_in_repo "$cb" status --provider tenki --id "$slug" --wait --wait-timeout 2s 2>&1)"; then
    paused_status=0
  else
    paused_status=$?
  fi
  printf '%s\n' "$paused_out"
  if [[ "$paused_status" -ne 5 ]]; then
    echo "paused Tenki status wait exited $paused_status, want 5" >&2
    return 1
  fi
  state="$("$tenki_cli" sandbox get "${tenki_sandbox_args[@]}" --output json "$session" | jq -r '.state // ""' | tr '[:upper:]' '[:lower:]')"
  if [[ "$state" != "paused" ]]; then
    echo "paused Tenki status wait changed session state to ${state:-unknown}" >&2
    return 1
  fi
  echo "tenki paused-session readiness check preserved state=paused"

  stop_provider_lease tenki "$lease" "$slug"
  lease=""
}

kubevirt_smoke() {
  need_tool jq
  need_tool rg
  need_tool kubectl
  need_tool virtctl

  local template="${CRABBOX_LIVE_KUBEVIRT_TEMPLATE:-${CRABBOX_KUBEVIRT_TEMPLATE:-$(config_value kubevirt.template || true)}}"
  if [[ -z "$template" ]]; then
    echo "kubevirt smoke requires CRABBOX_LIVE_KUBEVIRT_TEMPLATE, CRABBOX_KUBEVIRT_TEMPLATE, or kubevirt.template" >&2
    return 2
  fi

  local kubevirt_env=(CRABBOX_KUBEVIRT_TEMPLATE="$template")
  local route_args=(--provider kubevirt)
  if [[ -n "${CRABBOX_LIVE_KUBEVIRT_CONTEXT:-}" ]]; then
    kubevirt_env+=(CRABBOX_KUBEVIRT_CONTEXT="$CRABBOX_LIVE_KUBEVIRT_CONTEXT")
  fi
  if [[ -n "${CRABBOX_LIVE_KUBEVIRT_NAMESPACE:-}" ]]; then
    kubevirt_env+=(CRABBOX_KUBEVIRT_NAMESPACE="$CRABBOX_LIVE_KUBEVIRT_NAMESPACE")
  fi
  local lease_args=("${route_args[@]}" --ttl "${CRABBOX_LIVE_KUBEVIRT_TTL:-15m}" --idle-timeout "${CRABBOX_LIVE_KUBEVIRT_IDLE_TIMEOUT:-5m}")
  kubevirt_run() {
    (cd "$repo" && env "${kubevirt_env[@]}" "$cb" "$@")
  }

  local lease=""
  local slug=""
  cleanup() {
    trap - RETURN ERR
    if [[ -n "$lease" ]]; then
      if [[ -n "$slug" ]]; then
        kubevirt_run stop "${route_args[@]}" "$slug" || kubevirt_run stop "${route_args[@]}" "$lease" || true
      else
        kubevirt_run stop "${route_args[@]}" "$lease" || true
      fi
      lease=""
      slug=""
    fi
  }
  trap cleanup RETURN ERR

  kubevirt_run doctor "${route_args[@]}"
  local out
  capture_run out kubevirt_run warmup "${lease_args[@]}" --slug "${CRABBOX_LIVE_KUBEVIRT_SLUG:-kv-smoke-$$}" --timing-json
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  kubevirt_run status "${route_args[@]}" --id "$slug" --wait --wait-timeout "${CRABBOX_LIVE_KUBEVIRT_WAIT_TIMEOUT:-5m}"
  kubevirt_run inspect "${route_args[@]}" --id "$slug" --json | jq '{id,slug,provider,state,serverType,host,ready,lastTouchedAt,expiresAt}'
  local runout
  capture_run runout kubevirt_run run "${route_args[@]}" --id "$slug" --shell -- "$live_command"
  printf '%s\n' "$runout"
  kubevirt_run list "${route_args[@]}" --json | jq 'map({id:(.id // .CloudID),slug:(.slug // .labels.slug),provider:(.provider // .Provider // .labels.provider),state:(.state // .labels.state // .status)})'
  kubevirt_run stop "${route_args[@]}" "$slug" || kubevirt_run stop "${route_args[@]}" "$lease"
  lease=""
}

external_smoke() {
  need_tool jq
  need_tool rg

  local command="${CRABBOX_LIVE_EXTERNAL_COMMAND:-${CRABBOX_EXTERNAL_COMMAND:-$(config_value external.command || true)}}"
  local lifecycle_acquire
  lifecycle_acquire="$(config_value external.lifecycle.acquire.argv || true)"
  if [[ -z "$command" && -z "$lifecycle_acquire" ]]; then
    echo "external smoke requires an external command or external.lifecycle.acquire configuration" >&2
    return 2
  fi

  local external_env=()
  if [[ -n "$command" ]]; then
    external_env+=(CRABBOX_EXTERNAL_COMMAND="$command")
  fi
  local route_args=(--provider external)
  if [[ -n "${CRABBOX_LIVE_EXTERNAL_ARG:-}" ]]; then
    external_env+=(CRABBOX_EXTERNAL_ARG="$CRABBOX_LIVE_EXTERNAL_ARG")
  fi
  if [[ -n "${CRABBOX_LIVE_EXTERNAL_WORK_ROOT:-}" ]]; then
    external_env+=(CRABBOX_EXTERNAL_WORK_ROOT="$CRABBOX_LIVE_EXTERNAL_WORK_ROOT")
  fi
  local lease_args=("${route_args[@]}" --ttl "${CRABBOX_LIVE_EXTERNAL_TTL:-15m}" --idle-timeout "${CRABBOX_LIVE_EXTERNAL_IDLE_TIMEOUT:-5m}")
  external_run() {
    if (( ${#external_env[@]} )); then
      (cd "$repo" && env "${external_env[@]}" "$cb" "$@")
    else
      (cd "$repo" && "$cb" "$@")
    fi
  }

  local lease=""
  local slug=""
  cleanup() {
    trap - RETURN ERR
    if [[ -n "$lease" ]]; then
      if [[ -n "$slug" ]]; then
        external_run stop "${route_args[@]}" "$slug" || external_run stop "${route_args[@]}" "$lease" || true
      else
        external_run stop "${route_args[@]}" "$lease" || true
      fi
      lease=""
      slug=""
    fi
  }
  trap cleanup RETURN ERR

  external_run doctor "${route_args[@]}"
  local out
  capture_run out external_run warmup "${lease_args[@]}" --slug "${CRABBOX_LIVE_EXTERNAL_SLUG:-external-smoke-$$}" --timing-json
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  external_run status "${route_args[@]}" --id "$slug" --wait --wait-timeout "${CRABBOX_LIVE_EXTERNAL_WAIT_TIMEOUT:-5m}"
  external_run inspect "${route_args[@]}" --id "$slug" --json | jq '{id,slug,provider,state,serverType,host,ready,lastTouchedAt,expiresAt}'
  local runout
  capture_run runout external_run run "${route_args[@]}" --id "$slug" --shell -- "$live_command"
  printf '%s\n' "$runout"
  external_run list "${route_args[@]}" --json | jq 'map({id:(.id // .CloudID),slug:(.slug // .labels.slug),provider:(.provider // .Provider // .labels.provider),state:(.state // .labels.state // .status)})'
  external_run stop "${route_args[@]}" "$slug" || external_run stop "${route_args[@]}" "$lease"
  lease=""
}

morph_smoke() {
  need_tool jq
  need_tool rg

  local api_key="${CRABBOX_MORPH_API_KEY:-${MORPH_API_KEY:-$(config_value morph.apiKey || true)}}"
  if [[ -z "$api_key" ]]; then
    echo "set CRABBOX_MORPH_API_KEY, MORPH_API_KEY, or morph.apiKey to run morph live smoke" >&2
    return 2
  fi
  local snapshot="${CRABBOX_LIVE_MORPH_SNAPSHOT:-}"
  if [[ -z "$snapshot" ]]; then
    echo "set CRABBOX_LIVE_MORPH_SNAPSHOT to run morph live smoke" >&2
    return 2
  fi
  local slug="${CRABBOX_LIVE_MORPH_SLUG:-morph-smoke-$$}"
  local ttl="${CRABBOX_LIVE_MORPH_TTL:-15m}"
  local idle="${CRABBOX_LIVE_MORPH_IDLE_TIMEOUT:-5m}"

  local morph_env=(CRABBOX_PROVIDER=morph "CRABBOX_MORPH_SNAPSHOT=$snapshot" CRABBOX_MORPH_DELETE_ON_RELEASE=1)
  morph_run() {
    run_in_repo env "${morph_env[@]}" "$cb" "$@"
  }

  local lease=""
  cleanup() {
    trap - RETURN ERR
    if [[ -n "$lease" ]]; then
      morph_run stop "$slug" || morph_run stop "$lease" || true
      lease=""
      slug=""
    fi
  }
  trap cleanup RETURN ERR

  morph_run doctor
  local out
  capture_run out morph_run warmup --keep=false --slug "$slug" --ttl "$ttl" --idle-timeout "$idle"
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  test -n "$lease"
  test -n "$slug"

  morph_run status --id "$slug" --wait --wait-timeout 120s
  morph_run inspect --id "$slug" --json | jq '{id,slug,provider,state,serverType,host,ready,lastTouchedAt,expiresAt}'
  morph_run run --id "$slug" --shell -- "$live_command"
  morph_run list --json | jq 'map({id:.id,slug:.slug,provider:.provider,state:.state})'
  morph_run stop "$slug" || morph_run stop "$lease"
  lease=""
}

orgo_smoke() {
  need_tool curl
  need_tool jq

  local configured_api_key="$(trusted_config_value orgo.apiKey || true)"
  local configured_api_key_path=""
  if [[ -n "$configured_api_key" ]]; then
    configured_api_key_path="$trusted_config_path"
  fi
  local api_key_source="inherited"
  local api_key="${CRABBOX_ORGO_API_KEY:-}"
  if [[ -z "$api_key" && -n "$configured_api_key" ]]; then
    api_key="$configured_api_key"
    api_key_source="config"
  fi
  api_key="${api_key:-${ORGO_API_KEY:-}}"
  if [[ -z "$api_key" ]]; then
    echo "orgo smoke requires CRABBOX_ORGO_API_KEY, ORGO_API_KEY, or orgo.apiKey" >&2
    return 2
  fi
  if [[ "$api_key" == *$'\r'* || "$api_key" == *$'\n'* ]]; then
    echo "orgo smoke API key must not contain line breaks" >&2
    return 2
  fi

  local configured_api_base="$(config_value orgo.apiBase || true)"
  local configured_api_base_path="$(config_value_path orgo.apiBase || true)"
  if [[ -n "$configured_api_base" && -z "${CRABBOX_ORGO_API_BASE:-}" && -z "${ORGO_API_BASE_URL:-}" ]]; then
    if [[ "$api_key_source" != "config" || "$configured_api_key_path" != "$configured_api_base_path" ]]; then
      echo "orgo smoke refuses configured orgo.apiBase with an inherited API key; set CRABBOX_ORGO_API_BASE or ORGO_API_BASE_URL to approve it" >&2
      return 2
    fi
  fi
  local api_base="${CRABBOX_ORGO_API_BASE:-${ORGO_API_BASE_URL:-$configured_api_base}}"
  api_base="${api_base:-https://www.orgo.ai/api}"
  api_base="${api_base%/}"
  case "$api_base" in
    https://*) ;;
    http://localhost | http://localhost:* | http://localhost/* | http://127.0.0.1 | http://127.0.0.1:* | http://127.0.0.1/* | 'http://[::1]' | 'http://[::1]':* | 'http://[::1]'/*) ;;
    *)
      echo "orgo smoke API base must use HTTPS unless it targets localhost" >&2
      return 2
      ;;
  esac
  local workspace_id="${CRABBOX_ORGO_WORKSPACE_ID:-${ORGO_WORKSPACE_ID:-$(config_value orgo.workspaceID || true)}}"
  local created_workspace=""
  local computer_id=""

  orgo_request() {
    local method="$1"
    local path="$2"
    local data="${3:-}"
    local args=(-fsS -X "$method" "$api_base$path")
    if [[ -n "$data" ]]; then
      args+=(-H "Content-Type: application/json" -d "$data")
    fi
    # Keep the bearer token out of process argv and ordinary command tracing.
    printf 'Authorization: Bearer %s\n' "$api_key" | curl -H @- "${args[@]}"
  }

  cleanup() {
    trap - RETURN ERR
    if [[ -n "$computer_id" ]]; then
      orgo_request DELETE "/computers/$computer_id" >/dev/null || true
      computer_id=""
    fi
    if [[ -n "$created_workspace" ]]; then
      orgo_request DELETE "/workspaces/$created_workspace" >/dev/null || true
      created_workspace=""
    fi
  }
  trap cleanup RETURN ERR

  local suffix="${CRABBOX_LIVE_ORGO_SUFFIX:-$(date -u +%Y%m%d%H%M%S)-$$}"
  if [[ -z "$workspace_id" ]]; then
    local workspace_json
    workspace_json="$(orgo_request POST /workspaces "$(jq -n --arg name "crabbox-smoke-$suffix" '{name:$name}')")"
    workspace_id="$(printf '%s\n' "$workspace_json" | jq -r '.id // empty')"
    if [[ -z "$workspace_id" ]]; then
      echo "orgo smoke could not create a workspace" >&2
      return 1
    fi
    created_workspace="$workspace_id"
  fi

  local computer_json
  computer_json="$(orgo_request POST /computers "$(jq -n \
    --arg workspace_id "$workspace_id" \
    --arg name "crabbox-smoke-$suffix" \
    '{workspace_id:$workspace_id,name:$name,os:"linux",ram:4,cpu:1,disk_size_gb:8}')")"
  computer_id="$(printf '%s\n' "$computer_json" | jq -r '.id // empty')"
  if [[ -z "$computer_id" ]]; then
    echo "orgo smoke could not create a computer" >&2
    return 1
  fi
  printf 'orgo computer=%s workspace=%s\n' "$computer_id" "$workspace_id"

  local computer_state
  computer_state="$(printf '%s\n' "$computer_json" | jq -r '(.status // "") | ascii_downcase')"
  local ready_deadline=$((SECONDS + 300))
  while [[ "$computer_state" != "running" ]]; do
    case "$computer_state" in
      error|failed|deleted)
        echo "orgo computer $computer_id entered $computer_state state while starting" >&2
        return 1
        ;;
    esac
    if ((SECONDS >= ready_deadline)); then
      echo "timed out waiting for orgo computer $computer_id to become running (last state=$computer_state)" >&2
      return 1
    fi
    computer_json="$(orgo_request GET "/computers/$computer_id")"
    computer_state="$(printf '%s\n' "$computer_json" | jq -r '(.status // "") | ascii_downcase')"
    [[ "$computer_state" == "running" ]] || sleep 2
  done

  local command="${CRABBOX_LIVE_ORGO_COMMAND:-printf crabbox-orgo-ok}"
  local bash_json
  bash_json="$(orgo_request POST "/computers/$computer_id/bash" "$(jq -n --arg command "$command" '{command:$command}')")"
  printf '%s\n' "$bash_json" | jq -r '
    .stdout // .output // .result // .text // .message // empty
  '
  if ! printf '%s\n' "$bash_json" | jq -e '
    .success == true and
    ((.exit_code // .exitCode // 0) == 0) and
    ((.stdout // .output // .result // .text // .message // "") | contains("crabbox-orgo-ok"))
  ' >/dev/null; then
    echo "orgo smoke command did not return crabbox-orgo-ok" >&2
    return 1
  fi

  orgo_request DELETE "/computers/$computer_id" >/dev/null
  computer_id=""
  if [[ -n "$created_workspace" ]]; then
    orgo_request DELETE "/workspaces/$created_workspace" >/dev/null
    created_workspace=""
  fi
}

run_coordinator_preamble() {
  run_in_repo "$cb" whoami --json
  run_in_repo "$cb" doctor
  run_in_repo "$cb" sync-plan | sed -n '1,80p'
}

needs_coordinator_preamble() {
  case "${CRABBOX_LIVE_COORDINATOR:-auto}" in
    0|false|no) return 1 ;;
    1|true|yes) return 0 ;;
  esac
  has_provider aws || has_provider hetzner || has_provider blacksmith-testbox
}

needs_admin_audit() {
  case "${CRABBOX_LIVE_ADMIN_AUDIT:-auto}" in
    0|false|no|skip) return 1 ;;
    1|true|yes|required) return 0 ;;
  esac
  needs_coordinator_preamble
}

if needs_coordinator_preamble; then
  run_coordinator_preamble
fi

if has_provider aws; then
  provider_smoke aws --type "${CRABBOX_LIVE_AWS_TYPE:-t3.small}" --ttl 15m --idle-timeout 5m
fi

if has_provider hetzner; then
  provider_smoke hetzner --class "${CRABBOX_LIVE_HETZNER_CLASS:-standard}" --ttl 15m --idle-timeout 2m
fi

if has_provider azure; then
  provider_smoke azure --type "${CRABBOX_LIVE_AZURE_TYPE:-Standard_D2s_v5}" --ttl 20m --idle-timeout 5m
fi

if has_provider scaleway; then
  "$root/scripts/live-scaleway-smoke.sh"
fi

if has_provider tencentcloud || has_provider tencent || has_provider tencent-cvm || has_provider cvm; then
  "$root/scripts/live-tencentcloud-smoke.sh"
fi

if has_provider linode; then
  "$root/scripts/live-linode-smoke.sh"
fi

if has_provider vultr; then
  "$root/scripts/live-vultr-smoke.sh"
fi

if has_provider runpod || has_provider run-pod || has_provider runpodio; then
  "$root/scripts/live-runpod-smoke.sh"
fi

if has_provider digitalocean || has_provider do; then
  "$root/scripts/live-digitalocean-smoke.sh"
fi

if has_provider nebius; then
  "$root/scripts/live-nebius-smoke.sh"
fi

if has_provider ovh; then
  "$root/scripts/live-ovh-smoke.sh"
fi

if has_provider nvidia-brev || has_provider brev || has_provider nvidia; then
  CRABBOX_NVIDIA_BREV_LIVE=1 "$root/scripts/live-nvidia-brev-smoke.sh"
fi

if has_provider anthropic-sandbox-runtime || has_provider srt; then
  "$root/scripts/live-anthropic-sandbox-runtime-smoke.sh"
fi

if has_provider opensandbox; then
  "$root/scripts/live-opensandbox-smoke.sh"
fi

if has_provider github-codespaces || has_provider codespaces || has_provider gh-codespaces; then
  "$root/scripts/live-github-codespaces-smoke.sh"
fi

if has_provider cua; then
  "$root/scripts/live-cua-smoke.sh"
fi

if has_provider proxmox; then
  "$root/scripts/proxmox-live-smoke.sh"
fi

if has_provider xcp-ng || has_provider xcpng; then
  "$root/scripts/xcpng-live-smoke.sh"
fi

if has_provider blacksmith-testbox; then
  blacksmith_smoke
fi

if has_provider e2b; then
  e2b_smoke
fi

if has_provider modal; then
  modal_smoke
fi

if has_provider coder; then
  coder_smoke
fi

if has_provider sealos-devbox || has_provider sealos; then
  sealos_smoke
fi

if has_provider daytona; then
  daytona_smoke
fi

if has_provider namespace-devbox || has_provider namespace; then
  namespace_smoke
fi

if has_provider namespace-instance || has_provider namespace-compute; then
  run_in_repo "$cb" doctor --provider namespace-instance
  namespace_instance_baseline="$(run_in_repo "$cb" list --provider namespace-instance --json | jq -c 'map(.CloudID) | sort')"
  provider_smoke namespace-instance \
    --class "${CRABBOX_LIVE_NAMESPACE_INSTANCE_CLASS:-standard}" \
    --ttl "${CRABBOX_LIVE_NAMESPACE_INSTANCE_TTL:-10m}" \
    --idle-timeout 5m
  namespace_instance_after="$(run_in_repo "$cb" list --provider namespace-instance --json | jq -c 'map(.CloudID) | sort')"
  if [[ "$namespace_instance_after" != "$namespace_instance_baseline" ]]; then
    echo "Namespace instance smoke changed the pre-existing Crabbox-owned inventory" >&2
    exit 1
  fi
fi

if has_provider phala || has_provider phala-cloud || has_provider dstack; then
  "$root/scripts/live-phala-smoke.sh"
fi

if has_provider unikraft-cloud || has_provider unikraftcloud || has_provider ukc; then
  "$root/scripts/live-unikraft-cloud-smoke.sh"
fi

if has_provider semaphore; then
  semaphore_smoke
fi

if has_provider sprites; then
  sprites_smoke
fi

if has_provider tenki; then
  tenki_smoke
fi

if has_provider wandb; then
  wandb_smoke
fi

if has_provider incus; then
  incus_smoke
fi

if has_provider apple-container || has_provider apple || has_provider applecontainer; then
  provider_smoke apple-container --ttl 15m --idle-timeout 5m
fi

if has_provider local-container || has_provider docker || has_provider container || has_provider local-docker; then
  provider_smoke local-container --ttl 15m --idle-timeout 5m
fi

if has_provider docker-sandbox; then
  "$root/scripts/live-docker-sandbox-smoke.sh"
fi

if has_provider cloud-run-sandbox || has_provider gcrun-sandbox || has_provider cloudrun-sandbox || has_provider google-cloud-run-sandbox; then
  "$root/scripts/live-cloud-run-sandbox-smoke.sh"
fi

if has_provider smolvm || has_provider smol || has_provider smolmachines || has_provider smolfleet; then
  CRABBOX_SMOLVM_LIVE_SMOKE=1 "$root/scripts/live-smolvm-smoke.sh"
fi

if has_provider nomad; then
  "$root/scripts/live-nomad-smoke.sh"
fi

if has_provider superserve; then
  "$root/scripts/live-superserve-smoke.sh"
fi

if has_provider vercel-sandbox; then
  "$root/scripts/live-vercel-sandbox-smoke.sh"
fi

if has_provider aws-lambda-microvm; then
  "$root/scripts/live-aws-lambda-microvm-smoke.sh"
fi

if has_provider multipass || has_provider mp || has_provider canonical-multipass; then
  provider_smoke multipass --ttl 15m --idle-timeout 5m
fi

if has_provider tart || has_provider local-tart || has_provider macos-vm; then
  provider_smoke tart --ttl 30m --idle-timeout 5m
fi

if has_provider lume || has_provider local-lume || has_provider lume-macos; then
  provider_smoke lume --ttl 30m --idle-timeout 5m
fi

if has_provider apple-vm || has_provider applevm; then
  apple_vm_args=(--ttl 15m --idle-timeout 5m)
  apple_vm_helper=""
  if [[ -n "${CRABBOX_LIVE_APPLE_VM_HELPER:-}" ]]; then
    if [[ ! -x "$CRABBOX_LIVE_APPLE_VM_HELPER" ]]; then
      echo "CRABBOX_LIVE_APPLE_VM_HELPER must point to an executable helper: $CRABBOX_LIVE_APPLE_VM_HELPER" >&2
      exit 2
    fi
    apple_vm_helper="$CRABBOX_LIVE_APPLE_VM_HELPER"
  elif [[ -x "$root/bin/crabbox-apple-vm-helper" ]]; then
    apple_vm_helper="$root/bin/crabbox-apple-vm-helper"
  fi
  if [[ -n "$apple_vm_helper" ]]; then
    CRABBOX_APPLE_VM_HELPER="$apple_vm_helper" provider_smoke apple-vm "${apple_vm_args[@]}"
  else
    provider_smoke apple-vm "${apple_vm_args[@]}"
  fi
fi

if has_provider kubevirt; then
  kubevirt_smoke
fi

if has_provider agent-sandbox; then
  "$root/scripts/live-agent-sandbox-smoke.sh"
fi

if has_provider external; then
  external_smoke
fi

if has_provider morph; then
  morph_smoke
fi

if has_provider orgo; then
  orgo_smoke
fi

if needs_admin_audit; then
  admin_status=0
  admin_out="$(run_in_repo "$cb" admin leases --state active --json 2>&1)" || admin_status=$?
  if [[ "$admin_status" -ne 0 ]]; then
    printf 'error: admin active-lease check failed: %s\n' "$admin_out" >&2
    exit "$admin_status"
  fi
else
  printf 'warning: admin active-lease check skipped; set CRABBOX_LIVE_ADMIN_AUDIT=1 to require it\n' >&2
  admin_out='[]'
fi
need_tool jq
printf '%s\n' "$admin_out" | jq 'length'
