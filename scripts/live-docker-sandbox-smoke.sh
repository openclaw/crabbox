#!/usr/bin/env bash
set -euo pipefail

slug="docker-sandbox-smoke-$(date +%Y%m%d%H%M%S)-$$"
cleanup_armed=0

classify_blocker() {
  local command="$1"
  local status="$2"
  local output="$3"
  local classification="environment_blocked"
  local lower
  lower="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *capacity* ]]; then
    classification="quota_blocked"
  fi
  printf 'classification=%s command=%q exit=%s\n' "$classification" "$command" "$status" >&2
  printf '%s\n' "$output" >&2
}

classify_diagnostic() {
  local command="$1"
  local output="$2"
  printf 'classification=diagnostic_only command=%q exit=0\n' "$command" >&2
  printf '%s\n' "$output" >&2
}

classify_clone_guard() {
  local command="$1"
  local output="$2"
  printf 'classification=diagnostic_only clone_guard=manual command=%q exit=0\n' "$command" >&2
  printf '%s\n' "$output" >&2
}

classify_validation_failure() {
  local command="$1"
  local status="$2"
  local output="$3"
  printf 'classification=validation_failed command=%q exit=%s\n' "$command" "$status" >&2
  printf '%s\n' "$output" >&2
}

run_capture() {
  local command="$1"
  shift
  local output
  set +e
  output="$("$@" 2>&1)"
  local status=$?
  set -e
  if [ "$status" -ne 0 ]; then
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
  if command -v python3 >/dev/null 2>&1; then
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
  elif command -v node >/dev/null 2>&1; then
    validation_output="$(CRABBOX_SMOKE_SLUG="$slug" node -e '
const fs = require("node:fs");
const slug = process.env.CRABBOX_SMOKE_SLUG;
let payload;
try {
  payload = JSON.parse(fs.readFileSync(0, "utf8"));
} catch (error) {
  console.error(`invalid JSON: ${error.message}`);
  process.exit(1);
}
function hasSlug(value) {
  if (Array.isArray(value)) {
    return value.some(hasSlug);
  }
  if (value && typeof value === "object") {
    if (value.labels && typeof value.labels === "object" && value.labels.slug === slug) {
      return true;
    }
    if (value.slug === slug || value.name === slug || value.id === slug || value.leaseId === slug) {
      return true;
    }
    return Object.values(value).some(hasSlug);
  }
  return false;
}
if (!hasSlug(payload)) {
  console.error(`list JSON did not include slug ${slug}`);
  process.exit(1);
}
' <<<"$output" 2>&1)"
    status=$?
  elif command -v jq >/dev/null 2>&1; then
    validation_output="$(jq -e --arg slug "$slug" '.. | objects | select((.slug? == $slug) or (.name? == $slug) or (.id? == $slug) or (.leaseId? == $slug) or (((.labels? // {}) | .slug?) == $slug))' >/dev/null <<<"$output" 2>&1)"
    status=$?
    if [ "$status" -ne 0 ] && [ -z "$validation_output" ]; then
      validation_output="list JSON did not include slug $slug"
    fi
  else
    validation_output="no JSON parser available for list --json validation; install python3, node, or jq"
    status=1
  fi
  set -e
  if [ "$status" -ne 0 ]; then
    classify_validation_failure "$command" "$status" "$validation_output"
    exit "$status"
  fi
}

cleanup() {
  if [ "$cleanup_armed" -eq 1 ]; then
    bin/crabbox stop --provider docker-sandbox "$slug" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

mkdir -p bin
rm -f bin/crabbox
go build -trimpath -o bin/crabbox ./cmd/crabbox

doctor_output="$(run_capture "bin/crabbox doctor --provider docker-sandbox" bin/crabbox doctor --provider docker-sandbox)"
printf '%s\n' "$doctor_output"
if [[ "$doctor_output" != *sbx_version* ]]; then
  classify_diagnostic "bin/crabbox doctor --provider docker-sandbox" "$doctor_output"
  exit 1
fi
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  classify_clone_guard "git rev-parse --is-inside-work-tree" "clone-mode live proof skipped outside a Git repository workspace"
fi
cleanup_armed=1
run_capture "bin/crabbox warmup --provider docker-sandbox --slug $slug --keep" bin/crabbox warmup --provider docker-sandbox --slug "$slug" --keep >/dev/null
run_capture "bin/crabbox run --provider docker-sandbox --id $slug -- echo ok" bin/crabbox run --provider docker-sandbox --id "$slug" -- echo ok >/dev/null
run_capture "bin/crabbox run --provider docker-sandbox --id $slug -- pwd" bin/crabbox run --provider docker-sandbox --id "$slug" -- pwd >/dev/null
list_output="$(run_capture "bin/crabbox list --provider docker-sandbox --json" bin/crabbox list --provider docker-sandbox --json)"
printf '%s\n' "$list_output"
validate_list_json_contains_slug "bin/crabbox list --provider docker-sandbox --json" "$list_output"
run_capture "bin/crabbox stop --provider docker-sandbox $slug" bin/crabbox stop --provider docker-sandbox "$slug" >/dev/null
cleanup_armed=0
printf 'classification=live_sbx_smoke_passed slug=%s cleanup=complete\n' "$slug"
