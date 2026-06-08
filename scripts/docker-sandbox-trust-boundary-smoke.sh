#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
proof_dir="${CRABBOX_DOCKER_SANDBOX_TRUST_PROOF_DIR:-}"
cleanup_proof=0
if [[ -z "$proof_dir" ]]; then
  proof_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-docker-sandbox-trust-XXXXXX")"
  cleanup_proof=1
fi
mkdir -p "$proof_dir"

cleanup() {
  if [[ "$cleanup_proof" -eq 1 && "${CRABBOX_KEEP_TRUST_BOUNDARY_PROOF:-0}" != "1" ]]; then
    rm -rf "$proof_dir"
  fi
}
trap cleanup EXIT

fail() {
  printf 'classification=trust_boundary_proof_failed reason=%s proof_dir=%s\n' "$1" "$proof_dir" >&2
  exit 1
}

require_file_contains() {
  local file="$1"
  local needle="$2"
  local label="$3"
  if ! grep -F -- "$needle" "$file" >/dev/null 2>&1; then
    fail "${label}_missing"
  fi
}

require_file_omits() {
  local file="$1"
  local needle="$2"
  local label="$3"
  if grep -F -- "$needle" "$file" >/dev/null 2>&1; then
    fail "${label}_leaked"
  fi
}

fake_sbx="$proof_dir/sbx"
cat >"$fake_sbx" <<'FAKE_SBX'
#!/usr/bin/env bash
set -euo pipefail

log_dir="${CRABBOX_FAKE_SBX_LOG_DIR:?CRABBOX_FAKE_SBX_LOG_DIR required}"
mkdir -p "$log_dir"

write_args() {
  local file="$1"
  shift
  : >"$file"
  for arg in "$@"; do
    printf '%s\n' "$arg" >>"$file"
  done
}

case "${1:-}" in
  version)
    printf 'Client Version:  v0.31.3 fake\n'
    printf 'Server Version:  v0.31.3 fake\n'
    ;;
  ls)
    printf '[{"id":"fake-id","name":"crabbox-trust-boundary-proof","status":"running","agent":"shell","workspace":"%s"}]\n' "${CRABBOX_FAKE_SBX_WORKSPACE:-/workspace}"
    ;;
  diagnose)
    printf '{"status":"ok","fake":true}\n'
    ;;
  create)
    shift
    write_args "$log_dir/create.args" "$@"
    ;;
  exec)
    shift
    write_args "$log_dir/exec.args" "$@"
    env_file=""
    while [[ "$#" -gt 0 ]]; do
      case "$1" in
        --env-file)
          env_file="${2:-}"
          shift 2
          ;;
        --workdir)
          shift 2
          ;;
        --)
          shift
          ;;
        *)
          shift
          ;;
      esac
    done
    if [[ -n "$env_file" ]]; then
      cp "$env_file" "$log_dir/env-file.snapshot"
    fi
    if [[ -f "$log_dir/env-file.snapshot" ]]; then
      value="$(awk -F= '$1 == "CRABBOX_TRUST_BOUNDARY_TOKEN" {print substr($0, index($0, "=") + 1)}' "$log_dir/env-file.snapshot")"
      printf '%s\n' "$value"
    fi
    ;;
  rm)
    shift
    write_args "$log_dir/rm.args" "$@"
    ;;
  *)
    printf 'unexpected fake sbx command: %s\n' "$*" >&2
    exit 97
    ;;
esac
FAKE_SBX
chmod +x "$fake_sbx"

work_repo="$proof_dir/workspace/project with spaces"
home_dir="$proof_dir/home"
state_dir="$proof_dir/state"
mkdir -p "$work_repo" "$home_dir" "$state_dir"
work_repo="$(cd "$work_repo" && pwd -P)"

token_value="crabbox-trust-boundary-proof-value"
slug="trust-boundary-proof"

crabbox_bin="$proof_dir/crabbox"
(cd "$repo_root" && go build -trimpath -o "$crabbox_bin" ./cmd/crabbox)

set +e
output="$(
  cd "$work_repo" && \
  HOME="$home_dir" \
  XDG_STATE_HOME="$state_dir" \
  CRABBOX_FAKE_SBX_LOG_DIR="$proof_dir" \
  CRABBOX_FAKE_SBX_WORKSPACE="$work_repo" \
  CRABBOX_DOCKER_SANDBOX_CLI="$fake_sbx" \
  CRABBOX_ENV_ALLOW="CRABBOX_TRUST_BOUNDARY_TOKEN" \
  CRABBOX_TRUST_BOUNDARY_TOKEN="$token_value" \
  "$crabbox_bin" run --provider docker-sandbox --slug "$slug" -- printenv CRABBOX_TRUST_BOUNDARY_TOKEN
)"
status=$?
set -e

if [[ "$status" -ne 0 ]]; then
  printf '%s\n' "$output" >&2
  fail "crabbox_run_failed"
fi

create_args="$proof_dir/create.args"
exec_args="$proof_dir/exec.args"
env_snapshot="$proof_dir/env-file.snapshot"

[[ -f "$create_args" ]] || fail "missing_create_args"
[[ -f "$exec_args" ]] || fail "missing_exec_args"
[[ -f "$env_snapshot" ]] || fail "missing_env_file_snapshot"

require_file_contains "$create_args" "$work_repo" "workspace_path_create"
require_file_contains "$exec_args" "--workdir" "workdir_flag"
require_file_contains "$exec_args" "$work_repo" "workspace_path_exec"
require_file_contains "$exec_args" "--env-file" "env_file_flag"
require_file_contains "$exec_args" "printenv" "user_command"
require_file_contains "$exec_args" "CRABBOX_TRUST_BOUNDARY_TOKEN" "user_command_env_name"
require_file_omits "$exec_args" "$token_value" "env_value_argv"
require_file_contains "$env_snapshot" "CRABBOX_TRUST_BOUNDARY_TOKEN=$token_value" "env_value_env_file"

if [[ "$output" != *"$token_value"* ]]; then
  fail "sandbox_command_did_not_receive_env"
fi

printf 'classification=trust_boundary_proof_passed proof_dir=%s workspace_sent=true command_sent=true env_file_sent=true env_value_in_argv=false env_value_received=true\n' "$proof_dir"
