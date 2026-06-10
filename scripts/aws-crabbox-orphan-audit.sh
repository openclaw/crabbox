#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/aws-crabbox-orphan-audit.sh --profile <aws-profile> [--profile <aws-profile> ...] [--region <region> ...] [--terminate]

Audits AWS accounts for Crabbox-tagged EC2 instances that look orphaned:
  - tag crabbox=true
  - non-terminated EC2 state
  - no active coordinator lease by lease tag or instance id after grace, or expires_at is past grace

This script is read-only and prints JSON lines. The --terminate flag is retained
only to fail closed; orphan cleanup needs a coordinator-side claim/lock, not a
local snapshot followed by an unconditional cloud delete.

Environment:
  CRABBOX_BIN                 crabbox binary to query active leases; default bin/crabbox or crabbox
  CRABBOX_LEASE_AUDIT_LIMIT   active lease query limit; default 1000
  CRABBOX_AWS_ORPHAN_AUDIT_GRACE_SECONDS
                             grace period for stale tags; default CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS or 900
USAGE
}

profiles=()
regions=()
terminate=0

split_csv() {
  local value="$1"
  local item
  IFS=',' read -ra parts <<<"$value"
  for item in "${parts[@]}"; do
    item="${item//[[:space:]]/}"
    [[ -n "$item" ]] && printf '%s\n' "$item"
  done
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile)
      profiles+=("${2:?missing value for --profile}")
      shift 2
      ;;
    --profiles)
      while IFS= read -r profile; do profiles+=("$profile"); done < <(split_csv "${2:?missing value for --profiles}")
      shift 2
      ;;
    --region)
      regions+=("${2:?missing value for --region}")
      shift 2
      ;;
    --regions)
      while IFS= read -r region; do regions+=("$region"); done < <(split_csv "${2:?missing value for --regions}")
      shift 2
      ;;
    --terminate)
      terminate=1
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

if [[ ${#profiles[@]} -eq 0 ]]; then
  if [[ -n "${CRABBOX_AWS_AUDIT_PROFILES:-}" ]]; then
    while IFS= read -r profile; do profiles+=("$profile"); done < <(split_csv "$CRABBOX_AWS_AUDIT_PROFILES")
  elif [[ -n "${AWS_PROFILE:-}" ]]; then
    profiles+=("$AWS_PROFILE")
  else
    echo "provide --profile or CRABBOX_AWS_AUDIT_PROFILES" >&2
    exit 2
  fi
fi

if ! command -v aws >/dev/null 2>&1; then
  echo "aws CLI is required" >&2
  exit 127
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required" >&2
  exit 127
fi

crabbox_bin="${CRABBOX_BIN:-}"
if [[ -z "$crabbox_bin" ]]; then
  if [[ -x bin/crabbox ]]; then
    crabbox_bin="bin/crabbox"
  else
    crabbox_bin="crabbox"
  fi
fi
lease_limit="${CRABBOX_LEASE_AUDIT_LIMIT:-1000}"
if [[ ! "$lease_limit" =~ ^[0-9]+$ || "$lease_limit" -eq 0 ]]; then
  echo "CRABBOX_LEASE_AUDIT_LIMIT must be a positive integer" >&2
  exit 2
fi
effective_lease_limit="$lease_limit"
if [[ "$effective_lease_limit" -gt 500 ]]; then
  effective_lease_limit=500
fi
grace_seconds="${CRABBOX_AWS_ORPHAN_AUDIT_GRACE_SECONDS:-${CRABBOX_AWS_ORPHAN_SWEEP_GRACE_SECONDS:-900}}"
if [[ ! "$grace_seconds" =~ ^[0-9]+$ ]]; then
  echo "CRABBOX_AWS_ORPHAN_AUDIT_GRACE_SECONDS must be a non-negative integer" >&2
  exit 2
fi
if [[ "$terminate" == 1 ]]; then
  echo "--terminate is disabled; coordinator leases cannot be locked atomically from this script" >&2
  exit 2
fi

tmpdir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmpdir"
}
trap cleanup EXIT

load_active_leases() {
  local prefix="$1"
  local active_json="${prefix}-leases.json"
  local active_err="${prefix}-leases.err"
  local active_count

  if "$crabbox_bin" admin leases --json -state active -limit "$lease_limit" >"$active_json" 2>"$active_err"; then
    if ! jq -e 'type == "array" and all(.[]; type == "object")' "$active_json" >/dev/null; then
      echo "error: invalid active coordinator lease response; expected a JSON array of objects" >&2
      return 3
    fi
    if ! jq -r '.[].id // empty' "$active_json" | jq -R -s 'split("\n") | map(select(length > 0))' >"${prefix}-ids.json"; then
      echo "error: could not extract active coordinator lease ids" >&2
      return 3
    fi
    if ! jq -r '.[].cloudID // empty' "$active_json" | jq -R -s 'split("\n") | map(select(length > 0))' >"${prefix}-cloud-ids.json"; then
      echo "error: could not extract active coordinator lease cloud ids" >&2
      return 3
    fi
    if ! jq '[.[] | select(.id != null) | {key: .id, value: (.cloudID // "")}] | from_entries' "$active_json" >"${prefix}-cloud-by-id.json"; then
      echo "error: could not index active coordinator leases by id" >&2
      return 3
    fi
    active_count="$(jq 'length' "$active_json")"
    if [[ "$active_count" -ge "$effective_lease_limit" ]]; then
      echo "warning: active coordinator lease list reached limit $effective_lease_limit; destructive audit disabled" >&2
      return 2
    fi
    return 0
  fi

  jq -n '[]' >"${prefix}-ids.json"
  jq -n '[]' >"${prefix}-cloud-ids.json"
  jq -n '{}' >"${prefix}-cloud-by-id.json"
  echo "warning: could not load active coordinator leases; falling back to expires_at-only detection" >&2
  sed 's/^/warning: /' "$active_err" >&2
  return 1
}

emit_orphan_matches() {
  local profile="$1"
  local account="$2"
  local region="$3"
  local active_loaded="$4"
  local active_prefix="$5"

  jq -c \
    --arg profile "$profile" \
    --arg account "$account" \
    --arg region "$region" \
    --argjson now "$now" \
    --argjson graceSeconds "$grace_seconds" \
    --argjson activeLoaded "$active_loaded" \
    --slurpfile active "${active_prefix}-ids.json" \
    --slurpfile activeCloud "${active_prefix}-cloud-ids.json" \
    --slurpfile activeCloudByID "${active_prefix}-cloud-by-id.json" '
      def tag($key): ((.Tags // []) | map(select(.Key == $key))[0].Value // "");
      def flag_enabled: ((. // "") | ascii_downcase | test("^(1|true|yes|on)$"));
      def epoch:
        (. // "") as $raw
        | if ($raw | test("^[0-9]+$")) then ($raw | tonumber)
          else (($raw | fromdateiso8601?) // null)
          end;
      .Reservations[].Instances[]? as $instance
      | ($instance | tag("lease")) as $lease
      | ($instance.InstanceId // "") as $instanceId
      | (($instance | tag("crabbox")) | flag_enabled) as $managed
      | ($instance.State.Name // "") as $state
      | (($instance | tag("keep")) | flag_enabled) as $keep
      | (($instance | tag("created_at")) | epoch) as $created
      | (($instance | tag("expires_at")) | epoch) as $expires
      | ($created != null and ($created + $graceSeconds) <= $now) as $oldEnough
      | ($expires != null and ($expires + $graceSeconds) <= $now) as $expired
      | ($lease != "" and (($active[0] | index($lease)) != null)) as $activeLeaseKnown
      | ($activeCloudByID[0][$lease] // "") as $activeLeaseCloudID
      | ($activeLeaseKnown and $activeLeaseCloudID == $instanceId) as $activeLeaseMatchesCloud
      | ($instanceId != "" and (($activeCloud[0] | index($instanceId)) != null)) as $activeCloudKnown
      | ($activeLoaded and $lease != "" and ($activeLeaseKnown | not) and $oldEnough) as $notActive
      | ($activeLoaded and $activeLeaseKnown and ($activeLeaseMatchesCloud | not) and $oldEnough) as $leaseCloudMismatch
      | ($activeLoaded and $lease == "" and $oldEnough) as $missingLease
      | select($managed and (["pending", "running", "stopping", "stopped"] | index($state) != null) and ($keep | not) and ($activeCloudKnown | not) and (($activeLeaseKnown | not) or $leaseCloudMismatch) and ($expired or $notActive or $missingLease or $leaseCloudMismatch))
      | {
          profile: $profile,
          account: $account,
          region: $region,
          instanceId: $instanceId,
          state: $state,
          instanceType: $instance.InstanceType,
          launchTime: $instance.LaunchTime,
          publicIp: ($instance.PublicIpAddress // null),
          name: ($instance | tag("Name")),
          lease: $lease,
          owner: ($instance | tag("owner")),
          createdAtEpoch: $created,
          expiresAtEpoch: $expires,
          expired: $expired,
          activeLeaseKnown: $activeLeaseKnown,
          activeCloudKnown: $activeCloudKnown,
          activeLeaseCloudID: $activeLeaseCloudID,
          reason: (if $leaseCloudMismatch then "lease-cloud-mismatch" elif $expired and ($notActive or $missingLease) then "expired-and-orphaned" elif $expired then "expired" elif $notActive then "not-active" else "missing-lease-label" end)
        }
    '
}

active_prefix="$tmpdir/active"
active_loaded=false
active_truncated=false
if load_active_leases "$active_prefix"; then
  active_loaded=true
else
  active_status=$?
  if [[ "$active_status" -eq 2 ]]; then
    active_loaded=true
    active_truncated=true
  elif [[ "$active_status" -eq 3 ]]; then
    exit 1
  fi
fi

if [[ "$terminate" == 1 && "$active_loaded" != true ]]; then
  echo "refusing --terminate because active coordinator leases could not be loaded" >&2
  exit 1
fi
if [[ "$terminate" == 1 && "$active_truncated" == true ]]; then
  echo "refusing --terminate because active coordinator leases may be truncated" >&2
  exit 1
fi

now="$(date -u +%s)"
matches="$tmpdir/matches.jsonl"
: >"$matches"

for profile in "${profiles[@]}"; do
  identity="$(aws sts get-caller-identity --profile "$profile" --region us-east-1 --output json)"
  account="$(jq -r '.Account' <<<"$identity")"

  if [[ ${#regions[@]} -eq 0 ]]; then
    scan_regions=()
    while IFS= read -r region_name; do
      [[ -n "$region_name" ]] && scan_regions+=("$region_name")
    done < <(
      aws ec2 describe-regions --profile "$profile" --region us-east-1 --all-regions --output json |
        jq -r '.Regions[] | select(.OptInStatus == null or .OptInStatus == "opt-in-not-required" or .OptInStatus == "opted-in") | .RegionName' |
        sort
    )
  else
    scan_regions=("${regions[@]}")
  fi

  for region in "${scan_regions[@]}"; do
    aws ec2 describe-instances \
      --profile "$profile" \
      --region "$region" \
      --filters Name=tag:crabbox,Values=true Name=instance-state-name,Values=pending,running,stopping,stopped \
      --output json |
      emit_orphan_matches "$profile" "$account" "$region" "$active_loaded" "$active_prefix" >>"$matches"
  done
done

cat "$matches"
