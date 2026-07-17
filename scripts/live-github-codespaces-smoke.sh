#!/usr/bin/env bash
set -euo pipefail

provider_enabled() {
  local list="${CRABBOX_LIVE_PROVIDERS:-github-codespaces}"
  local item
  IFS=',' read -ra items <<<"$list"
  for item in "${items[@]}"; do
    item="${item//[[:space:]]/}"
    if [[ "$item" == "github-codespaces" || "$item" == "codespaces" || "$item" == "gh-codespaces" ]]; then
      return 0
    fi
  done
  return 1
}

redact_output() {
  local text="$1"
  local secret
  for secret in "${GH_TOKEN:-}" "${GITHUB_TOKEN:-}" "${GH_ENTERPRISE_TOKEN:-}" "${GITHUB_ENTERPRISE_TOKEN:-}"; do
    if [[ -n "$secret" ]]; then
      text="${text//$secret/<redacted>}"
    fi
  done
  printf '%s' "$text" | sed -E 's/(ghp_|github_pat_|gho_|ghu_|ghs_|ghr_)[A-Za-z0-9_]+/<redacted>/g'
}

classify_blocker() {
  local command="$1"
  local status="$2"
  local output="$3"
  local classification=""
  local lower
  lower="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *capacity* || "$lower" == *billing* || "$lower" == *"spending limit"* || "$lower" == *"too many requests"* ]]; then
    classification="quota_blocked"
  elif [[ "$lower" == *"bad credentials"* || "$lower" == *"requires authentication"* || "$lower" == *"authentication failed"* || "$lower" == *"authentication required"* || "$lower" == *"not authenticated"* || "$lower" == *"http 401"* || "$lower" == *"http 403"* || "$lower" == *"status 401"* || "$lower" == *"status 403"* || "$lower" == *"resource not accessible by personal access token"* || ( "$lower" == *codespace* && "$lower" == *scope* ) ]]; then
    classification="credential_bound"
  else
    return 1
  fi
  printf 'classification=%s command=%q exit=%s\n' "$classification" "$command" "$status" >&2
  redact_output "$output" >&2
  printf '\n' >&2
  return 0
}

classify_validation_failure() {
  local command="$1"
  local status="$2"
  local output="$3"
  printf 'classification=validation_failed command=%q exit=%s\n' "$command" "$status" >&2
  redact_output "$output" >&2
  printf '\n' >&2
}

run_capture_impl() {
  local allow_blocker="$1"
  local command="$2"
  shift 2
  local output=""
  local stderr_output=""
  local stderr_file=""
  stderr_file="$(mktemp "${TMPDIR:-/tmp}/crabbox-ghcs-stderr.XXXXXX")"
  set +e
  output="$("$@" 2>"$stderr_file")"
  local status=$?
  stderr_output="$(<"$stderr_file")"
  rm -f -- "$stderr_file"
  set -e
  if [[ "$status" -ne 0 ]]; then
    local failure_output="$output"
    if [[ -n "$stderr_output" ]]; then
      failure_output="${failure_output}${failure_output:+$'\n'}${stderr_output}"
    fi
    if [[ "$allow_blocker" -eq 1 ]] && classify_blocker "$command" "$status" "$failure_output"; then
      exit 0
    fi
    classify_validation_failure "$command" "$status" "$failure_output"
    exit "$status"
  fi
  if [[ -n "$stderr_output" ]]; then
    redact_output "$stderr_output" >&2
    printf '\n' >&2
  fi
  captured_output="$output"
}

run_capture() {
  run_capture_impl 0 "$@"
}

run_capture_or_blocked() {
  run_capture_impl 1 "$@"
}

validate_list_json_contains_slug() {
  local command="$1"
  local output="$2"
  local validation_output=""
  local status=0
  set +e
  validation_output="$(CRABBOX_SMOKE_SLUG="$slug" "$python_bin" -c '
import json
import os
import sys

slug = os.environ["CRABBOX_SMOKE_SLUG"]
try:
    payload = json.load(sys.stdin)
except Exception as exc:
    print(f"invalid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

def has_slug(value):
    if isinstance(value, dict):
        labels = value.get("labels")
        if isinstance(labels, dict) and labels.get("slug") == slug:
            return True
        if value.get("slug") == slug or value.get("name") == slug or value.get("id") == slug or value.get("leaseId") == slug:
            return True
        return any(has_slug(child) for child in value.values())
    if isinstance(value, list):
        return any(has_slug(child) for child in value)
    return False

if not has_slug(payload):
    print(f"list JSON did not include slug {slug}", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
}

validate_list_json_missing_slug() {
  local command="$1"
  local output="$2"
  local validation_output=""
  local status=0
  set +e
  validation_output="$(CRABBOX_SMOKE_SLUG="$slug" "$python_bin" -c '
import json
import os
import sys

slug = os.environ["CRABBOX_SMOKE_SLUG"]
try:
    payload = json.load(sys.stdin)
except Exception as exc:
    print(f"invalid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

def has_slug(value):
    if isinstance(value, dict):
        labels = value.get("labels")
        if isinstance(labels, dict) and labels.get("slug") == slug:
            return True
        if value.get("slug") == slug or value.get("name") == slug or value.get("id") == slug or value.get("leaseId") == slug:
            return True
        return any(has_slug(child) for child in value.values())
    if isinstance(value, list):
        return any(has_slug(child) for child in value)
    return False

if has_slug(payload):
    print(f"list JSON still included slug {slug}", file=sys.stderr)
    sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
}

validate_remote_list_json_missing_slug() {
  local command="$1"
  local output="$2"
  local validation_output=""
  local status=0
  set +e
  validation_output="$(CRABBOX_SMOKE_SLUG="$slug" "$python_bin" -c '
import json
import os
import sys

slug = os.environ["CRABBOX_SMOKE_SLUG"]
try:
    payload = json.load(sys.stdin)
except Exception as exc:
    print(f"invalid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

for item in payload:
    name = str(item.get("name", ""))
    display_name = str(item.get("displayName", ""))
    if slug in name or slug in display_name:
        print(f"remote Codespaces inventory still included slug {slug}", file=sys.stderr)
        sys.exit(1)
' <<<"$output" 2>&1)"
  status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
}

remote_codespace_names_for_slug() {
  CRABBOX_SMOKE_SLUG="$slug" "$python_bin" -c '
import json
import os
import sys

slug = os.environ["CRABBOX_SMOKE_SLUG"]
for item in json.load(sys.stdin):
    name = str(item.get("name", ""))
    display_name = str(item.get("displayName", ""))
    if name and (slug in name or slug in display_name):
        print(name)
'
}

local_inventory_has_slug() {
  CRABBOX_SMOKE_SLUG="$slug" "$python_bin" -c '
import json
import os
import sys

slug = os.environ["CRABBOX_SMOKE_SLUG"]

def has_slug(value):
    if isinstance(value, dict):
        labels = value.get("labels")
        if isinstance(labels, dict) and labels.get("slug") == slug:
            return True
        if value.get("slug") == slug or value.get("name") == slug or value.get("id") == slug or value.get("leaseId") == slug:
            return True
        return any(has_slug(child) for child in value.values())
    if isinstance(value, list):
        return any(has_slug(child) for child in value)
    return False

try:
    payload = json.load(sys.stdin)
except Exception as exc:
    print(f"invalid JSON: {exc}", file=sys.stderr)
    sys.exit(2)
sys.exit(0 if has_slug(payload) else 1)
'
}

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

cleanup_armed=0
printf -v slug 'g%06x%06x' "$RANDOM" "$RANDOM"
crabbox_bin="${CRABBOX_BIN:-bin/crabbox}"
crabbox_bin_overridden=0
if [[ -n "${CRABBOX_BIN:-}" ]]; then
  crabbox_bin_overridden=1
fi
python_bin="${CRABBOX_GITHUB_CODESPACES_PYTHON_PATH:-python3}"
delete_timeout_seconds="${CRABBOX_GITHUB_CODESPACES_SMOKE_DELETE_TIMEOUT_SECONDS:-300}"
captured_output=""

cleanup() {
  local status=$?
  trap - EXIT
  if [[ "$cleanup_armed" -eq 1 ]]; then
    local cleanup_output=""
    local cleanup_status=0
    local cleanup_failure=0
    set +e
    cleanup_output="$("$crabbox_bin" stop --provider github-codespaces --github-codespaces-delete-on-release=true "$slug" 2>&1)"
    cleanup_status=$?
    if [[ "$cleanup_status" -ne 0 ]]; then
      printf 'classification=cleanup_fallback command=%q exit=%s slug=%s\n' "$crabbox_bin stop --provider github-codespaces --github-codespaces-delete-on-release=true $slug" "$cleanup_status" "$slug" >&2
      redact_output "$cleanup_output" >&2
      printf '\n' >&2
    fi

    local remote_output=""
    local remote_status=0
    remote_output="$("$gh_bin" codespace list --repo "$repo" --limit 100 --json name,displayName 2>&1)"
    remote_status=$?
    if [[ "$remote_status" -eq 0 ]]; then
      local remote_name=""
      local remote_names=""
      remote_names="$(printf '%s' "$remote_output" | remote_codespace_names_for_slug 2>&1)"
      remote_status=$?
      if [[ "$remote_status" -ne 0 ]]; then
        printf 'classification=cleanup_failed reason=remote_inventory_parse exit=%s slug=%s\n' "$remote_status" "$slug" >&2
        redact_output "$remote_names" >&2
        printf '\n' >&2
        cleanup_failure="$remote_status"
      else
        while IFS= read -r remote_name; do
          [[ -n "$remote_name" ]] || continue
          cleanup_output="$("$gh_bin" codespace delete --codespace "$remote_name" --force 2>&1)"
          cleanup_status=$?
          if [[ "$cleanup_status" -ne 0 ]]; then
            printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "$gh_bin codespace delete --codespace $remote_name --force" "$cleanup_status" "$slug" >&2
            redact_output "$cleanup_output" >&2
            printf '\n' >&2
            cleanup_failure="$cleanup_status"
          fi
        done <<<"$remote_names"
      fi
    else
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "$gh_bin codespace list --repo $repo --limit 100 --json name,displayName" "$remote_status" "$slug" >&2
      redact_output "$remote_output" >&2
      printf '\n' >&2
      cleanup_failure="$remote_status"
    fi

    local remote_clean=0
    local attempt=0
    local delete_deadline=$((SECONDS + delete_timeout_seconds))
    while :; do
      attempt=$((attempt + 1))
      remote_output="$("$gh_bin" codespace list --repo "$repo" --limit 100 --json name,displayName 2>&1)"
      remote_status=$?
      if [[ "$remote_status" -eq 0 ]]; then
        local remaining_names=""
        remaining_names="$(printf '%s' "$remote_output" | remote_codespace_names_for_slug 2>&1)"
        remote_status=$?
        if [[ "$remote_status" -eq 0 && -z "$remaining_names" ]]; then
          remote_clean=1
          break
        fi
        if [[ "$remote_status" -eq 0 ]]; then
          while IFS= read -r remote_name; do
            [[ -n "$remote_name" ]] || continue
            cleanup_output="$("$gh_bin" codespace delete --codespace "$remote_name" --force 2>&1)"
            cleanup_status=$?
            if [[ "$cleanup_status" -ne 0 ]]; then
              printf 'classification=cleanup_retry command=%q exit=%s slug=%s attempt=%s\n' "$gh_bin codespace delete --codespace $remote_name --force" "$cleanup_status" "$slug" "$attempt" >&2
              redact_output "$cleanup_output" >&2
              printf '\n' >&2
            fi
          done <<<"$remaining_names"
        fi
      fi
      if (( SECONDS >= delete_deadline )); then
        break
      fi
      sleep 2
    done
    if [[ "$remote_clean" -ne 1 ]]; then
      printf 'classification=cleanup_failed reason=remote_codespace_still_present slug=%s\n' "$slug" >&2
      redact_output "$remote_output" >&2
      printf '\n' >&2
      cleanup_failure=1
    else
      # Reconcile any retained local claim after direct deletion bypassed the
      # provider's dirty-worktree guard.
      cleanup_output="$("$crabbox_bin" stop --provider github-codespaces --github-codespaces-delete-on-release=true "$slug" 2>&1)"
      cleanup_status=$?
      if [[ "$cleanup_status" -ne 0 ]]; then
        local local_output=""
        local list_status=0
        local inventory_status=0
        local_output="$("$crabbox_bin" list --provider github-codespaces --json 2>&1)"
        list_status=$?
        if [[ "$list_status" -eq 0 ]]; then
          printf '%s' "$local_output" | local_inventory_has_slug >/dev/null 2>&1
          inventory_status=$?
        fi
        if [[ "$list_status" -ne 0 || "$inventory_status" -ne 1 ]]; then
          printf 'classification=cleanup_failed reason=local_claim_reconciliation command=%q exit=%s slug=%s\n' "$crabbox_bin stop --provider github-codespaces --github-codespaces-delete-on-release=true $slug" "$cleanup_status" "$slug" >&2
          if [[ "$list_status" -ne 0 ]]; then
            printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "$crabbox_bin list --provider github-codespaces --json" "$list_status" "$slug" >&2
            redact_output "$local_output" >&2
            printf '\n' >&2
          fi
          redact_output "$cleanup_output" >&2
          printf '\n' >&2
          cleanup_failure="$cleanup_status"
        else
          cleanup_failure=0
        fi
      fi
      if [[ "$cleanup_status" -eq 0 ]]; then
        cleanup_failure=0
      fi
    fi
    if [[ "$cleanup_failure" -ne 0 ]]; then
      status="$cleanup_failure"
    fi
    cleanup_armed=0
  fi
  exit "$status"
}
trap cleanup EXIT

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  printf 'classification=environment_blocked reason=CRABBOX_LIVE_not_enabled\n'
  exit 0
fi

if ! provider_enabled; then
  printf 'classification=environment_blocked reason=github_codespaces_not_selected providers=%q\n' "${CRABBOX_LIVE_PROVIDERS:-}"
  exit 0
fi

if [[ ! "$delete_timeout_seconds" =~ ^[1-9][0-9]*$ ]]; then
  printf 'classification=environment_blocked reason=invalid_delete_timeout value=%q\n' "$delete_timeout_seconds"
  exit 0
fi

repo="${CRABBOX_GITHUB_CODESPACES_SMOKE_REPO:-${CRABBOX_GITHUB_CODESPACES_REPO:-}}"
if [[ -z "$repo" ]]; then
  printf 'classification=environment_blocked reason=CRABBOX_GITHUB_CODESPACES_SMOKE_REPO_missing\n'
  exit 0
fi

gh_bin="${CRABBOX_GITHUB_CODESPACES_GH_PATH:-gh}"
if ! command -v "$gh_bin" >/dev/null 2>&1; then
  printf 'classification=environment_blocked reason=gh_missing gh_path=%q\n' "$gh_bin"
  exit 0
fi

if ! command -v "$python_bin" >/dev/null 2>&1; then
  printf 'classification=environment_blocked reason=python_missing python_path=%q\n' "$python_bin"
  exit 0
fi

api_url="${CRABBOX_GITHUB_CODESPACES_API_URL:-https://api.github.com}"
host_details="$("$python_bin" - "$api_url" <<'PY'
import sys
from urllib.parse import urlparse

parsed = urlparse(sys.argv[1].strip())
hostname = (parsed.hostname or "").lower()
if not hostname:
    raise SystemExit("GitHub Codespaces API URL has no hostname")
port = parsed.port
if hostname == "api.github.com" and port in (None, 443):
    cli_host = "github.com"
elif hostname.startswith("api.") and hostname.endswith(".ghe.com"):
    cli_host = hostname[4:]
    if port is not None:
        cli_host += f":{port}"
else:
    cli_host = parsed.netloc
dotcom_route = hostname in {"github.com", "api.github.com", "ghe.com"} or hostname.endswith(".ghe.com")
print(cli_host)
print("1" if dotcom_route else "0")
PY
)"
gh_host="$(printf '%s\n' "$host_details" | sed -n '1p')"
dotcom_route="$(printf '%s\n' "$host_details" | sed -n '2p')"

selected_value=""
if [[ "$dotcom_route" == "1" ]]; then
  selected_value="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
else
  selected_value="${GH_ENTERPRISE_TOKEN:-${GITHUB_ENTERPRISE_TOKEN:-}}"
fi
if [[ -z "$selected_value" && "${CRABBOX_GITHUB_CODESPACES_USE_GH_AUTH:-}" != "1" ]]; then
  printf 'classification=credential_bound reason=github_token_missing_or_gh_auth_not_enabled host=%q\n' "$gh_host"
  exit 0
fi

unset GH_HOST GH_TOKEN GITHUB_TOKEN GH_ENTERPRISE_TOKEN GITHUB_ENTERPRISE_TOKEN
export GH_HOST="$gh_host"
if [[ -n "$selected_value" ]]; then
  if [[ "$dotcom_route" == "1" ]]; then
    export GH_TOKEN="$selected_value"
  else
    export GH_ENTERPRISE_TOKEN="$selected_value"
  fi
fi

auth_output=""
auth_status=0
set +e
auth_output="$("$gh_bin" auth status --active --hostname "$gh_host" 2>&1)"
auth_status=$?
set -e
if [[ "$auth_status" -ne 0 ]]; then
  printf 'classification=credential_bound command=%q exit=%s\n' "$gh_bin auth status --active --hostname $gh_host" "$auth_status" >&2
  redact_output "$auth_output" >&2
  printf '\n' >&2
  exit 0
fi

scope_output=""
scope_status=0
set +e
scope_output="$("$gh_bin" codespace list --limit 1 2>&1)"
scope_status=$?
set -e
if [[ "$scope_status" -ne 0 ]]; then
  scope_output_lower="$(printf '%s' "$scope_output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$scope_output_lower" == *quota* || "$scope_output_lower" == *"rate limit"* || "$scope_output_lower" == *capacity* || "$scope_output_lower" == *billing* || "$scope_output_lower" == *"spending limit"* || "$scope_output_lower" == *"too many requests"* ]]; then
    classify_blocker "$gh_bin codespace list --limit 1" "$scope_status" "$scope_output"
    exit 0
  elif [[ "$scope_output_lower" == *"bad credentials"* || "$scope_output_lower" == *unauthorized* || "$scope_output_lower" == *forbidden* || "$scope_output_lower" == *"requires authentication"* || "$scope_output_lower" == *"http 401"* || "$scope_output_lower" == *"http 403"* || ( "$scope_output_lower" == *codespace* && "$scope_output_lower" == *scope* ) ]]; then
    printf 'classification=credential_bound command=%q exit=%s reason=github_codespaces_scope_missing\n' "$gh_bin codespace list --limit 1" "$scope_status" >&2
    redact_output "$scope_output" >&2
    printf '\n' >&2
    exit 0
  fi
  classify_validation_failure "$gh_bin codespace list --limit 1" "$scope_status" "$scope_output"
  exit "$scope_status"
fi

if [[ "$crabbox_bin_overridden" -eq 0 ]]; then
  mkdir -p bin
  go build -trimpath -o bin/crabbox ./cmd/crabbox
  crabbox_bin="bin/crabbox"
elif [[ ! -x "$crabbox_bin" ]]; then
  printf 'classification=environment_blocked reason=crabbox_bin_not_executable path=%q\n' "$crabbox_bin"
  exit 0
fi

ref="${CRABBOX_GITHUB_CODESPACES_SMOKE_REF:-${CRABBOX_GITHUB_CODESPACES_REF:-}}"
if [[ -z "$ref" ]]; then
  run_capture_or_blocked "$gh_bin api repos/$repo --jq .default_branch" "$gh_bin" api "repos/$repo" --jq .default_branch
  ref="$(printf '%s' "$captured_output" | tr -d '[:space:]')"
  if [[ -z "$ref" || "$ref" == "null" ]]; then
    printf 'classification=environment_blocked reason=github_default_branch_missing repo=%q\n' "$repo"
    exit 0
  fi
fi
machine="${CRABBOX_GITHUB_CODESPACES_SMOKE_MACHINE:-${CRABBOX_GITHUB_CODESPACES_MACHINE:-basicLinux32gb}}"
devcontainer=""
if [[ "${CRABBOX_GITHUB_CODESPACES_SMOKE_DEVCONTAINER_PATH+x}" == "x" ]]; then
  devcontainer="$CRABBOX_GITHUB_CODESPACES_SMOKE_DEVCONTAINER_PATH"
  unset CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH
elif [[ "${CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH+x}" == "x" ]]; then
  devcontainer="$CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH"
fi
working_directory="${CRABBOX_GITHUB_CODESPACES_SMOKE_WORKING_DIRECTORY:-${CRABBOX_GITHUB_CODESPACES_WORKING_DIRECTORY:-}}"
geo="${CRABBOX_GITHUB_CODESPACES_SMOKE_GEO:-${CRABBOX_GITHUB_CODESPACES_GEO:-}}"

provider_args=(--provider github-codespaces --github-codespaces-repo "$repo" --github-codespaces-ref "$ref" --github-codespaces-machine "$machine" --github-codespaces-delete-on-release=true)
if [[ -n "$devcontainer" ]]; then
  provider_args+=(--github-codespaces-devcontainer-path "$devcontainer")
fi
if [[ -n "$working_directory" ]]; then
  provider_args+=(--github-codespaces-working-directory "$working_directory")
fi
if [[ -n "$geo" ]]; then
  provider_args+=(--github-codespaces-geo "$geo")
fi

run_capture_or_blocked "$crabbox_bin doctor ${provider_args[*]}" "$crabbox_bin" doctor "${provider_args[@]}"

cleanup_armed=1
run_capture_or_blocked "$crabbox_bin warmup ${provider_args[*]} --slug $slug --keep=true --ttl 20m --idle-timeout 5m" \
  "$crabbox_bin" warmup "${provider_args[@]}" --slug "$slug" --keep=true --ttl 20m --idle-timeout 5m

run_capture "$crabbox_bin status --provider github-codespaces --id $slug --wait --wait-timeout 600s" \
  "$crabbox_bin" status --provider github-codespaces --id "$slug" --wait --wait-timeout 600s

remote_command='cleanup() { git reset --hard HEAD >/dev/null 2>&1 || true; git clean -ffd >/dev/null 2>&1 || true; }; trap cleanup EXIT; test -f go.mod && echo github-codespaces-smoke-ok'
run_capture "$crabbox_bin run --provider github-codespaces --id $slug --full-resync -- sh -lc <smoke-command>" \
  "$crabbox_bin" run --provider github-codespaces --id "$slug" --full-resync -- sh -lc "$remote_command"
run_output="$captured_output"
if [[ "$run_output" != *"github-codespaces-smoke-ok"* ]]; then
  classify_validation_failure "$crabbox_bin run --provider github-codespaces --id $slug" 1 "remote smoke marker not found"
  exit 1
fi

run_capture "$crabbox_bin ssh --provider github-codespaces --id $slug" "$crabbox_bin" ssh --provider github-codespaces --id "$slug"
ssh_output="$captured_output"
if [[ "$ssh_output" != ssh* ]]; then
  classify_validation_failure "$crabbox_bin ssh --provider github-codespaces --id $slug" 1 "ssh command was not printed"
  exit 1
fi

run_capture "$crabbox_bin list --provider github-codespaces --json" "$crabbox_bin" list --provider github-codespaces --json
list_output="$captured_output"
validate_list_json_contains_slug "$crabbox_bin list --provider github-codespaces --json" "$list_output"

run_capture "$crabbox_bin stop --provider github-codespaces --github-codespaces-delete-on-release=true $slug" \
  "$crabbox_bin" stop --provider github-codespaces --github-codespaces-delete-on-release=true "$slug"

run_capture "$crabbox_bin cleanup --provider github-codespaces --dry-run" "$crabbox_bin" cleanup --provider github-codespaces --dry-run
run_capture "$crabbox_bin list --provider github-codespaces --json" "$crabbox_bin" list --provider github-codespaces --json
final_list="$captured_output"
validate_list_json_missing_slug "$crabbox_bin list --provider github-codespaces --json" "$final_list"
remote_list=""
remote_clean=0
delete_deadline=$((SECONDS + delete_timeout_seconds))
attempt=0
while :; do
  attempt=$((attempt + 1))
  run_capture "$gh_bin codespace list --repo $repo --limit 100 --json name,displayName" "$gh_bin" codespace list --repo "$repo" --limit 100 --json name,displayName
  remote_list="$captured_output"
  set +e
  remaining_names="$(printf '%s' "$remote_list" | remote_codespace_names_for_slug 2>&1)"
  remote_status=$?
  set -e
  if [[ "$remote_status" -ne 0 ]]; then
    classify_validation_failure "$gh_bin codespace list --repo $repo --limit 100 --json name,displayName" "$remote_status" "$remaining_names"
    exit "$remote_status"
  fi
  if [[ -z "$remaining_names" ]]; then
    remote_clean=1
    break
  fi
  if (( SECONDS >= delete_deadline )); then
    break
  fi
  sleep 2
done
if [[ "$remote_clean" -ne 1 ]]; then
  classify_validation_failure "$gh_bin codespace list --repo $repo --limit 100 --json name,displayName" 1 "remote Codespaces inventory still included slug $slug"
  exit 1
fi
validate_remote_list_json_missing_slug "$gh_bin codespace list --repo $repo --limit 100 --json name,displayName" "$remote_list"
cleanup_armed=0

printf 'classification=live_github_codespaces_smoke_passed slug=%s repo=%s machine=%s\n' "$slug" "$repo" "$machine"
