#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/aws-account-guard.sh"

usage() {
  cat <<'USAGE'
Usage: scripts/request-macos-host-quota.sh --quota <mac-host-quota.json> --region <aws-region> [--identity <provider-identity.json>] [--profile <aws-profile|auto>] [--desired-value <number>] [--apply]

Safely prepares or submits an AWS Service Quotas request for the selected EC2
Mac Dedicated Host family. The script is dry-run by default and refuses to
write unless --apply is passed with a provider identity whose AWS account
matches the local AWS caller.

Options:
  --quota <file>          JSON from crabbox admin hosts quota --json
  --region <region>       AWS region where the quota should be raised
  --identity <file>       provider-identity.json from lifecycle evidence
  --profile <name|auto>   AWS profile to use for sts/service-quotas commands
  --desired-value <n>     requested quota value; default 1
  --apply                 submit aws service-quotas request-service-quota-increase
  -h, --help              show this help
USAGE
}

quota_file=""
identity_file=""
region=""
profile=""
desired_value="1"
apply=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --quota)
      quota_file="${2:?missing value for --quota}"
      shift 2
      ;;
    --identity)
      identity_file="${2:?missing value for --identity}"
      shift 2
      ;;
    --region)
      region="${2:?missing value for --region}"
      shift 2
      ;;
    --profile)
      profile="${2:?missing value for --profile}"
      shift 2
      ;;
    --desired-value)
      desired_value="${2:?missing value for --desired-value}"
      shift 2
      ;;
    --apply)
      apply=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$quota_file" || -z "$region" ]]; then
  usage >&2
  exit 2
fi
if [[ ! -s "$quota_file" ]]; then
  echo "quota file is missing or empty: $quota_file" >&2
  exit 2
fi
if [[ "$apply" == 1 && -z "$identity_file" ]]; then
  echo "refusing to submit quota request without --identity account guard" >&2
  exit 2
fi
if [[ -n "$identity_file" && ! -s "$identity_file" ]]; then
  echo "identity file is missing or empty: $identity_file" >&2
  exit 2
fi
if ! command -v aws >/dev/null 2>&1; then
  echo "aws CLI is required" >&2
  exit 127
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required" >&2
  exit 127
fi
if ! jq -e -n --arg value "$desired_value" '$value | test("^[0-9]+([.][0-9]+)?$")' >/dev/null; then
  echo "desired value must be numeric: $desired_value" >&2
  exit 2
fi

quota_matches="$(
  jq -c '
    map(select(
      (.serviceCode // "") == "ec2"
      and (.quotaCode // "") != ""
      and ((.quotaName // "") | test("^Running Dedicated mac[[:alnum:]-]* Hosts$"; "i"))
    ))
  ' "$quota_file"
)"
quota_count="$(printf '%s\n' "$quota_matches" | jq 'length')"
if [[ "$quota_count" -eq 0 ]]; then
  echo "quota file does not contain a visible EC2 Mac host quota" >&2
  exit 1
fi
if [[ "$quota_count" -ne 1 ]]; then
  echo "quota file contains multiple EC2 Mac host quotas; provide the quota output for one Mac host family" >&2
  exit 1
fi
quota_json="$(printf '%s\n' "$quota_matches" | jq -c '.[0]')"

service_code="$(printf '%s\n' "$quota_json" | jq -r '.serviceCode')"
quota_code="$(printf '%s\n' "$quota_json" | jq -r '.quotaCode')"
quota_name="$(printf '%s\n' "$quota_json" | jq -r '.quotaName // "unknown quota"')"
current_value="$(printf '%s\n' "$quota_json" | jq -r '(.value // 0) | tostring')"
adjustable="$(printf '%s\n' "$quota_json" | jq -r '(.adjustable // false) | tostring')"

if jq -e -n --argjson current "$current_value" --argjson desired "$desired_value" '$current >= $desired' >/dev/null; then
  printf 'quota already sufficient: %s current=%s desired=%s region=%s\n' "$quota_name" "$current_value" "$desired_value" "$region"
  exit 0
fi

if [[ "$adjustable" != "true" ]]; then
  printf 'quota is not adjustable: %s (%s)\n' "$quota_name" "$quota_code" >&2
  exit 1
fi

coordinator_account=""
if [[ -n "$identity_file" ]]; then
  coordinator_account="$(jq -r '.account // empty' "$identity_file")"
  if [[ -z "$coordinator_account" ]]; then
    echo "identity file does not include .account" >&2
    exit 2
  fi
fi

if [[ "$profile" == "auto" ]]; then
  if [[ -z "$coordinator_account" ]]; then
    echo "profile auto requires --identity" >&2
    exit 2
  fi
  profile="$(aws_guard_select_profile_for_account "$coordinator_account" "request quota")"
fi

aws_base=(aws)
if [[ -n "$profile" ]]; then
  aws_base+=(--profile "$profile")
fi

local_account="$(aws_guard_account_for_selected_profile "$profile" "$coordinator_account" "request quota")"

cmd=(
  "${aws_base[@]}"
  service-quotas request-service-quota-increase
  --service-code "$service_code"
  --quota-code "$quota_code"
  --desired-value "$desired_value"
  --region "$region"
)

printf 'quota=%s\n' "$quota_name"
printf 'quota_code=%s\n' "$quota_code"
printf 'region=%s\n' "$region"
printf 'current_value=%s\n' "$current_value"
printf 'desired_value=%s\n' "$desired_value"
printf 'local_account=%s\n' "$local_account"
if [[ -n "$coordinator_account" ]]; then
  printf 'coordinator_account=%s\n' "$coordinator_account"
fi
if [[ -n "$profile" ]]; then
  printf 'aws_profile=%s\n' "$profile"
fi

if [[ "$apply" != 1 ]]; then
  printf 'dry-run:'
  printf ' %q' "${cmd[@]}"
  printf '\n'
  printf 'rerun with --apply to submit the quota increase request.\n'
  exit 0
fi

printf '+'
printf ' %q' "${cmd[@]}"
printf '\n'
"${cmd[@]}"
