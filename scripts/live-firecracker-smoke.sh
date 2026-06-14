#!/usr/bin/env bash
set -u -o pipefail

usage() {
  cat <<'EOF'
Run the read-only Firecracker readiness smoke.

The current worktree only ships the Firecracker provider contract plus
`crabbox doctor --provider firecracker` checks, so this helper never mutates
provider state. It validates the doctor JSON contract and classifies missing
Linux/KVM/Firecracker/CNI prerequisites as environment_blocked.

Usage:
  scripts/live-firecracker-smoke.sh
  scripts/live-firecracker-smoke.sh --dry-run
  scripts/live-firecracker-smoke.sh --help

Environment:
  CRABBOX_BIN   Crabbox binary (default: ./bin/crabbox)
EOF
}

summarize_file() {
  local file="$1"
  if [[ ! -s "$file" ]]; then
    return 0
  fi
  tr '\n' ' ' <"$file" | sed 's/[[:space:]][[:space:]]*/ /g; s/^ //; s/ $//'
}

if [[ "$#" -gt 1 ]]; then
  echo "live firecracker smoke accepts at most one argument" >&2
  usage >&2
  exit 2
fi

mode="readiness"
case "${1:-}" in
  "")
    ;;
  -h|--help)
    usage
    exit 0
    ;;
  --dry-run)
    mode="dry-run"
    ;;
  *)
    echo "unknown argument: $1" >&2
    usage >&2
    exit 2
    ;;
esac

bin="${CRABBOX_BIN:-./bin/crabbox}"

if [[ "$mode" == "dry-run" ]]; then
  echo "classification=dry_run provider=firecracker mutation=false"
  echo "command=$bin doctor --provider firecracker --json"
  exit 0
fi

if [[ ! -x "$bin" ]]; then
  echo "missing crabbox binary: $bin" >&2
  echo "build first: go build -trimpath -o bin/crabbox ./cmd/crabbox" >&2
  exit 2
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "missing required tool: jq" >&2
  exit 2
fi

stdout_file="$(mktemp)" || {
  echo "could not create temporary stdout file" >&2
  exit 2
}
stderr_file="$(mktemp)" || {
  echo "could not create temporary stderr file" >&2
  rm -f -- "$stdout_file"
  exit 2
}
trap 'rm -f -- "$stdout_file" "$stderr_file"' EXIT

doctor_status=0
"$bin" doctor --provider firecracker --json >"$stdout_file" 2>"$stderr_file" || doctor_status=$?

if ! jq -e '
  type == "object" and
  .provider == "firecracker" and
  (.ok | type == "boolean") and
  (.checks | type == "array")
' "$stdout_file" >/dev/null 2>&1; then
  stderr_summary="$(summarize_file "$stderr_file")"
  echo "classification=validation_failed provider=firecracker mutation=false reason=malformed_doctor_json doctor_exit=$doctor_status"
  if [[ -n "$stderr_summary" ]]; then
    echo "stderr=$stderr_summary"
  fi
  exit 1
fi

firecracker_check_count="$(
  jq '[.checks[] | select((.provider // "") == "firecracker")] | length' "$stdout_file"
)"
if [[ "$firecracker_check_count" -eq 0 ]]; then
  stderr_summary="$(summarize_file "$stderr_file")"
  echo "classification=validation_failed provider=firecracker mutation=false reason=missing_firecracker_checks doctor_exit=$doctor_status"
  if [[ -n "$stderr_summary" ]]; then
    echo "stderr=$stderr_summary"
  fi
  exit 1
fi

check_summary="$(
  jq -r '[.checks[] | select((.provider // "") == "firecracker") | "\(.check)=\(.status)"] | join(",")' "$stdout_file"
)"
blocking_checks="$(
  jq -r '[.checks[] | select(.status == "failed" or .status == "missing") | "\(.check):\(.details.class // .details.reason // "failed")"] | join(",")' "$stdout_file"
)"
stderr_summary="$(summarize_file "$stderr_file")"

if [[ "$doctor_status" -eq 0 ]]; then
  echo "classification=readiness_passed provider=firecracker mutation=false checks=$check_summary"
  exit 0
fi

if [[ -n "$blocking_checks" ]]; then
  echo "classification=environment_blocked provider=firecracker mutation=false checks=$check_summary blocking_checks=$blocking_checks"
  if [[ -n "$stderr_summary" ]]; then
    echo "stderr=$stderr_summary"
  fi
  exit 1
fi

echo "classification=validation_failed provider=firecracker mutation=false reason=doctor_failed_without_blocking_checks doctor_exit=$doctor_status"
if [[ -n "$stderr_summary" ]]; then
  echo "stderr=$stderr_summary"
fi
exit 1
