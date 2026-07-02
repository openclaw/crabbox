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
  for secret in "${GH_TOKEN:-}" "${GITHUB_TOKEN:-}"; do
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
  local classification="environment_blocked"
  local lower
  lower="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *capacity* || "$lower" == *billing* || "$lower" == *"spending limit"* || "$lower" == *"too many requests"* ]]; then
    classification="quota_blocked"
  elif [[ "$lower" == *credential* || "$lower" == *authentication* || "$lower" == *authorization* || "$lower" == *unauthorized* || "$lower" == *forbidden* || "$lower" == *"bad credentials"* || "$lower" == *"requires authentication"* || "$lower" == *scope* || "$lower" == *token* ]]; then
    classification="credential_bound"
  fi
  printf 'classification=%s command=%q exit=%s\n' "$classification" "$command" "$status" >&2
  redact_output "$output" >&2
  printf '\n' >&2
}

classify_validation_failure() {
  local command="$1"
  local status="$2"
  local output="$3"
  printf 'classification=validation_failed command=%q exit=%s\n' "$command" "$status" >&2
  redact_output "$output" >&2
  printf '\n' >&2
}

run_capture() {
  local command="$1"
  shift
  local output
  set +e
  output="$("$@" 2>&1)"
  local status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    classify_blocker "$command" "$status" "$output"
    exit "$status"
  fi
  printf '%s\n' "$output"
}

validate_list_json_contains_slug() {
  local command="$1"
  local output="$2"
  local validation_output=""
  local status=0
  set +e
  validation_output="$(CRABBOX_SMOKE_SLUG="$slug" python3 -c '
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
  validation_output="$(CRABBOX_SMOKE_SLUG="$slug" python3 -c '
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
  validation_output="$(CRABBOX_SMOKE_SLUG="$slug" python3 -c '
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
  CRABBOX_SMOKE_SLUG="$slug" python3 -c '
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

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

cleanup_armed=0
slug="gcs-$(date +%Y%m%d%H%M%S)-$$"
crabbox_bin="${CRABBOX_BIN:-bin/crabbox}"

cleanup() {
  local status=$?
  if [[ "$cleanup_armed" -eq 1 ]]; then
    local cleanup_output=""
    local cleanup_status=0
    set +e
    cleanup_output="$("$crabbox_bin" stop --provider github-codespaces "$slug" 2>&1)"
    cleanup_status=$?
    set -e
    if [[ "$cleanup_status" -ne 0 ]]; then
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "$crabbox_bin stop --provider github-codespaces $slug" "$cleanup_status" "$slug" >&2
      redact_output "$cleanup_output" >&2
      printf '\n' >&2
      if [[ "$status" -eq 0 ]]; then
        status="$cleanup_status"
      fi
    fi

    local remote_output=""
    local remote_status=0
    set +e
    remote_output="$("$gh_bin" codespace list --repo "$repo" --limit 100 --json name,displayName 2>&1)"
    remote_status=$?
    set -e
    if [[ "$remote_status" -eq 0 ]]; then
      local remote_name=""
      while IFS= read -r remote_name; do
        [[ -n "$remote_name" ]] || continue
        set +e
        cleanup_output="$("$gh_bin" codespace delete --codespace "$remote_name" --force 2>&1)"
        cleanup_status=$?
        set -e
        if [[ "$cleanup_status" -ne 0 ]]; then
          printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "$gh_bin codespace delete --codespace $remote_name --force" "$cleanup_status" "$slug" >&2
          redact_output "$cleanup_output" >&2
          printf '\n' >&2
          if [[ "$status" -eq 0 ]]; then
            status="$cleanup_status"
          fi
        fi
      done < <(printf '%s' "$remote_output" | remote_codespace_names_for_slug)
    else
      printf 'classification=cleanup_failed command=%q exit=%s slug=%s\n' "$gh_bin codespace list --repo $repo --limit 100 --json name,displayName" "$remote_status" "$slug" >&2
      redact_output "$remote_output" >&2
      printf '\n' >&2
      if [[ "$status" -eq 0 ]]; then
        status="$remote_status"
      fi
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

repo="${CRABBOX_GITHUB_CODESPACES_SMOKE_REPO:-${CRABBOX_GITHUB_CODESPACES_REPO:-}}"
if [[ -z "$repo" ]]; then
  printf 'classification=environment_blocked reason=CRABBOX_GITHUB_CODESPACES_SMOKE_REPO_missing\n'
  exit 0
fi

if [[ -z "${GH_TOKEN:-}" && -z "${GITHUB_TOKEN:-}" && "${CRABBOX_GITHUB_CODESPACES_USE_GH_AUTH:-}" != "1" ]]; then
  printf 'classification=credential_bound reason=github_token_missing_or_gh_auth_not_enabled\n'
  exit 0
fi

gh_bin="${CRABBOX_GITHUB_CODESPACES_GH_PATH:-gh}"
if ! command -v "$gh_bin" >/dev/null 2>&1; then
  printf 'classification=environment_blocked reason=gh_missing gh_path=%q\n' "$gh_bin"
  exit 0
fi

auth_output=""
auth_status=0
set +e
auth_output="$("$gh_bin" auth status 2>&1)"
auth_status=$?
set -e
if [[ "$auth_status" -ne 0 ]]; then
  printf 'classification=credential_bound command=%q exit=%s\n' "$gh_bin auth status" "$auth_status" >&2
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
  printf 'classification=credential_bound command=%q exit=%s reason=github_codespaces_scope_missing\n' "$gh_bin codespace list --limit 1" "$scope_status" >&2
  redact_output "$scope_output" >&2
  printf '\n' >&2
  exit 0
fi

if [[ ! -x "$crabbox_bin" ]]; then
  mkdir -p bin
  go build -trimpath -o bin/crabbox ./cmd/crabbox
  crabbox_bin="bin/crabbox"
fi

ref="${CRABBOX_GITHUB_CODESPACES_SMOKE_REF:-${CRABBOX_GITHUB_CODESPACES_REF:-main}}"
machine="${CRABBOX_GITHUB_CODESPACES_SMOKE_MACHINE:-${CRABBOX_GITHUB_CODESPACES_MACHINE:-basicLinux32gb}}"
devcontainer="${CRABBOX_GITHUB_CODESPACES_SMOKE_DEVCONTAINER_PATH:-${CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH:-scripts/fixtures/github-codespaces/devcontainer.json}}"
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

run_capture "$crabbox_bin doctor ${provider_args[*]}" "$crabbox_bin" doctor "${provider_args[@]}" >/dev/null

cleanup_armed=1
run_capture "$crabbox_bin warmup ${provider_args[*]} --slug $slug --keep=false --ttl 20m --idle-timeout 5m" \
  "$crabbox_bin" warmup "${provider_args[@]}" --slug "$slug" --keep=false --ttl 20m --idle-timeout 5m >/dev/null

run_capture "$crabbox_bin status --provider github-codespaces --id $slug --wait --wait-timeout 600s" \
  "$crabbox_bin" status --provider github-codespaces --id "$slug" --wait --wait-timeout 600s >/dev/null

run_output="$(run_capture "$crabbox_bin run --provider github-codespaces --id $slug --full-resync -- sh -lc 'test -f go.mod && echo github-codespaces-smoke-ok'" \
  "$crabbox_bin" run --provider github-codespaces --id "$slug" --full-resync -- sh -lc 'test -f go.mod && echo github-codespaces-smoke-ok')"
if [[ "$run_output" != *"github-codespaces-smoke-ok"* ]]; then
  classify_validation_failure "$crabbox_bin run --provider github-codespaces --id $slug" 1 "remote smoke marker not found"
  exit 1
fi

ssh_output="$(run_capture "$crabbox_bin ssh --provider github-codespaces --id $slug" "$crabbox_bin" ssh --provider github-codespaces --id "$slug")"
if [[ "$ssh_output" != ssh* ]]; then
  classify_validation_failure "$crabbox_bin ssh --provider github-codespaces --id $slug" 1 "ssh command was not printed"
  exit 1
fi

list_output="$(run_capture "$crabbox_bin list --provider github-codespaces --json" "$crabbox_bin" list --provider github-codespaces --json)"
validate_list_json_contains_slug "$crabbox_bin list --provider github-codespaces --json" "$list_output"

run_capture "$crabbox_bin stop --provider github-codespaces $slug" "$crabbox_bin" stop --provider github-codespaces "$slug" >/dev/null

run_capture "$crabbox_bin cleanup --provider github-codespaces --dry-run" "$crabbox_bin" cleanup --provider github-codespaces --dry-run >/dev/null
final_list="$(run_capture "$crabbox_bin list --provider github-codespaces --json" "$crabbox_bin" list --provider github-codespaces --json)"
validate_list_json_missing_slug "$crabbox_bin list --provider github-codespaces --json" "$final_list"
remote_list="$(run_capture "$gh_bin codespace list --repo $repo --limit 100 --json name,displayName" "$gh_bin" codespace list --repo "$repo" --limit 100 --json name,displayName)"
validate_remote_list_json_missing_slug "$gh_bin codespace list --repo $repo --limit 100 --json name,displayName" "$remote_list"
cleanup_armed=0

printf 'classification=live_github_codespaces_smoke_passed slug=%s repo=%s machine=%s\n' "$slug" "$repo" "$machine"
