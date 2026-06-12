#!/usr/bin/env bash
set -u -o pipefail
umask 077

usage() {
  cat <<'EOF'
Run an opt-in Proxmox live proof through the public Crabbox CLI.

By default the script is read-only: it builds redacted proof logs for doctor and
list. Set CRABBOX_PROXMOX_LIVE_SMOKE=1 to run a controlled warmup/status/ssh
command/stop/cleanup-dry-run lifecycle.

Environment:
  CRABBOX_BIN                         Crabbox binary (default: ./bin/crabbox)
  CRABBOX_PROXMOX_LIVE_SMOKE          Set to 1 to permit lease mutation
  CRABBOX_PROXMOX_LIVE_SMOKE_SLUG     Per-run lease slug prefix (default: proxmox-live-smoke)
  CRABBOX_PROXMOX_LIVE_SMOKE_DIR      Proof directory (default: /tmp/crabbox-proxmox-live-proof.XXXXXX)
  CRABBOX_PROXMOX_SSH_INVENTORY_HOST  Optional Proxmox node SSH host for read-only inventory
  CRABBOX_PROXMOX_SSH_INVENTORY_USER  Optional Proxmox node SSH user (default: root)

The script never passes Proxmox token secrets as CLI flags. Keep API credentials
in the normal Crabbox environment or config file.
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

bin="${CRABBOX_BIN:-./bin/crabbox}"
live="${CRABBOX_PROXMOX_LIVE_SMOKE:-0}"
slug_prefix="${CRABBOX_PROXMOX_LIVE_SMOKE_SLUG:-proxmox-live-smoke}"
inventory_host="${CRABBOX_PROXMOX_SSH_INVENTORY_HOST:-}"
inventory_user="${CRABBOX_PROXMOX_SSH_INVENTORY_USER:-root}"

if [[ ! -x "$bin" ]]; then
  echo "missing crabbox binary: $bin" >&2
  echo "build first: go build -trimpath -o bin/crabbox ./cmd/crabbox" >&2
  exit 2
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "missing required tool: jq" >&2
  exit 2
fi
if ! command -v perl >/dev/null 2>&1; then
  echo "missing required tool: perl" >&2
  exit 2
fi

slug_prefix="$(
  printf '%s' "$slug_prefix" |
    tr '[:upper:]' '[:lower:]' |
    sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//'
)"
if [[ -z "$slug_prefix" ]]; then
  slug_prefix="proxmox-live-smoke"
fi
nonce="$(od -An -N6 -tx1 /dev/urandom 2>/dev/null | tr -d '[:space:]')"
if [[ ! "$nonce" =~ ^[0-9a-f]{12}$ ]]; then
  echo "could not generate live smoke ownership nonce" >&2
  exit 2
fi
epoch="$(date -u +%s)"
epoch="${epoch: -8}"
slug="${slug_prefix:0:19}-${epoch}-${nonce}"

resolve_configured_value() {
  local field="$1"
  "$bin" config show --json 2>/dev/null | jq -r --arg field "$field" '.proxmox[$field] // empty'
}

redaction_api_url="${CRABBOX_PROXMOX_API_URL:-}"
if [[ -z "$redaction_api_url" ]]; then
  redaction_api_url="$(resolve_configured_value apiUrl || true)"
fi
redaction_token_id="${CRABBOX_PROXMOX_TOKEN_ID:-}"
if [[ -z "$redaction_token_id" ]]; then
  redaction_token_id="$(resolve_configured_value tokenId || true)"
fi
redaction_api_host="$(
  printf '%s' "$redaction_api_url" |
    perl -ne 'if (m{^[a-z][a-z0-9+.-]*://(?:[^@/]+@)?(\[[^]]+\]|[^:/]+)}i) { print "$1\n" }'
)"
export CRABBOX_PROXMOX_REDACT_API_URL="$redaction_api_url"
export CRABBOX_PROXMOX_REDACT_API_HOST="$redaction_api_host"
export CRABBOX_PROXMOX_REDACT_TOKEN_ID="$redaction_token_id"

directory_is_private() {
  local mode=""
  mode="$(stat -f '%Lp' "$1" 2>/dev/null)" || mode=""
  if [[ "$mode" == "700" || "$mode" == "0700" ]]; then
    return 0
  fi
  mode="$(stat -c '%a' "$1" 2>/dev/null)" || mode=""
  [[ "$mode" == "700" || "$mode" == "0700" ]]
}

if [[ -n "${CRABBOX_PROXMOX_LIVE_SMOKE_DIR:-}" ]]; then
  proof_dir="${CRABBOX_PROXMOX_LIVE_SMOKE_DIR}"
  if [[ -L "$proof_dir" ]]; then
    echo "refusing symlink proof directory: $proof_dir" >&2
    exit 2
  fi
  if [[ -e "$proof_dir" ]]; then
    if [[ ! -d "$proof_dir" || ! -O "$proof_dir" ]] || ! directory_is_private "$proof_dir"; then
      echo "proof directory must be an owner-owned mode-700 directory: $proof_dir" >&2
      exit 2
    fi
  else
    mkdir -p "$proof_dir" || {
      echo "could not create proof directory: $proof_dir" >&2
      exit 2
    }
    if [[ -L "$proof_dir" || ! -d "$proof_dir" || ! -O "$proof_dir" ]] || ! directory_is_private "$proof_dir"; then
      echo "created proof directory is not an owner-owned mode-700 directory: $proof_dir" >&2
      exit 2
    fi
  fi
else
  proof_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-proxmox-live-proof.XXXXXX")" || {
    echo "could not create proof directory" >&2
    exit 2
  }
fi
export CRABBOX_PROXMOX_REDACT_PROOF_DIR="$proof_dir"

secure_log_file() {
  local file="$1"
  rm -f -- "$file" || {
    echo "could not replace proof log: $file" >&2
    exit 2
  }
  : >"$file" || {
    echo "could not create proof log: $file" >&2
    exit 2
  }
  chmod 600 "$file" || {
    echo "could not secure proof log: $file" >&2
    exit 2
  }
}

known_hosts="$proof_dir/proxmox-node-known-hosts"
summary="$proof_dir/summary.redacted.log"
secure_log_file "$summary"
rm -f -- "$known_hosts"

redact_stream() {
  local token_secret="${CRABBOX_PROXMOX_TOKEN_SECRET:-}"
  local token_id="${CRABBOX_PROXMOX_TOKEN_ID:-}"
  local inventory_host="${CRABBOX_PROXMOX_SSH_INVENTORY_HOST:-}"
  perl -pe '
    BEGIN {
      $token_secret = $ENV{"CRABBOX_PROXMOX_TOKEN_SECRET"} // "";
      $token_id = $ENV{"CRABBOX_PROXMOX_REDACT_TOKEN_ID"} // "";
      $api_url = $ENV{"CRABBOX_PROXMOX_REDACT_API_URL"} // "";
      $api_host = $ENV{"CRABBOX_PROXMOX_REDACT_API_HOST"} // "";
      $inventory_host = $ENV{"CRABBOX_PROXMOX_SSH_INVENTORY_HOST"} // "";
      $proof_dir = $ENV{"CRABBOX_PROXMOX_REDACT_PROOF_DIR"} // "";
      $bin = $ENV{"CRABBOX_BIN"} // "";
    }
    s/\Q$token_secret\E/<proxmox-token-secret>/g if length($token_secret);
    s/\Q$token_id\E/<proxmox-token-id>/g if length($token_id);
    s/\Q$api_url\E/<proxmox-api-url>/g if length($api_url);
    s/\Q$api_host\E/<proxmox-api-host>/g if length($api_host);
    s/\Q$inventory_host\E/<proxmox-ssh-host>/g if length($inventory_host);
    s/\Q$proof_dir\E/<proof-dir>/g if length($proof_dir);
    s/\Q$bin\E/<crabbox-bin>/g if length($bin) && $bin =~ m#/#;
    s/PVEAPIToken=[^[:space:]]+/PVEAPIToken=<redacted>/g;
    s#https?://[^[:space:]'"'"'"]+#<url>#g;
    s#/(?:Users|home)/[^'"'"'"\n]+#<local-home-path>#g;
    s#/tmp/crabbox-[^[:space:]'"'"'"]+#<local-temp-path>#g;
    s#(?:\b\d{1,3}\.){3}\d{1,3}\b#<ip>#g;
    s/\b(?:api|tokenid|proxmox)\.md\b/<credential-file>/g;
  '
}

summary_log_path() {
  printf '<proof-dir>/%s' "$(basename "$1")"
}

run_step() {
  local name="$1"
  shift
  local raw="$proof_dir/${name}.raw.log"
  local redacted="$proof_dir/${name}.redacted.log"
  local status=0
  local redaction_status=0

  secure_log_file "$raw"
  {
    printf '$'
    printf ' %q' "$@"
    printf '\n'
    "$@"
  } >"$raw" 2>&1 || status=$?

  secure_log_file "$redacted"
  redact_stream <"$raw" >"$redacted" || redaction_status=$?
  if [[ "$redaction_status" -ne 0 ]]; then
    : >"$redacted"
    printf 'step=%s status=fail exit=%s reason=redaction_failed log=private_raw_not_publishable\n' "$name" "$redaction_status" | tee -a "$summary"
    if [[ "$status" -ne 0 ]]; then
      return "$status"
    fi
    return "$redaction_status"
  fi
  if [[ "$status" -eq 0 ]]; then
    printf 'step=%s status=pass log=%s\n' "$name" "$(summary_log_path "$redacted")" | tee -a "$summary"
  else
    printf 'step=%s status=fail exit=%s log=%s\n' "$name" "$status" "$(summary_log_path "$redacted")" | tee -a "$summary"
  fi
  return "$status"
}

validate_doctor_json() {
  local file="$proof_dir/doctor.raw.log"
  awk 'NR > 1 && $0 ~ /^[[:space:]]*\{/ { print; exit }' "$file" |
    jq -e 'type == "object" and .provider == "proxmox" and (.ok | type == "boolean") and (.checks | type == "array")' >/dev/null
}

run_node_inventory() {
  [[ -n "$inventory_host" ]] || return 0
  local target="${inventory_user}@${inventory_host}"
  run_step node-ssh-inventory ssh \
    -o BatchMode=yes \
    -o StrictHostKeyChecking=accept-new \
    -o "UserKnownHostsFile=$known_hosts" \
    "$target" \
    "pvesh get /version && pvesh get /nodes && pvesh get /cluster/nextid && qm list"
}

lease_id=""
owned_lease_ids="$proof_dir/owned-lease-ids.txt"
extract_owned_lease_ids() {
  local raw="$proof_dir/warmup.raw.log"
  secure_log_file "$owned_lease_ids"
  sed -n 's/.*leased \([^[:space:]]*\).*/\1/p' "$raw" | sort -u >"$owned_lease_ids"
  lease_id="$(tail -1 "$owned_lease_ids")"
}

is_owned_smoke_lease() {
  local candidate_lease="$1"
  local candidate_slug="$2"
  if grep -Fqx -- "$candidate_lease" "$owned_lease_ids"; then
    return 0
  fi
  [[ "$candidate_slug" == "$slug" ]]
}

extract_list_inventory() {
  local raw="$1"
  awk 'NR > 1 { print }' "$raw" |
    jq -r '
      if type != "array" then error("expected list array") else . end
      | .[]
      | (.labels // .Labels // {}) as $labels
      | (.provider // .Provider // $labels.provider // "") as $provider
      | (.leaseId // $labels.lease // "") as $lease
      | (.slug // $labels.slug // "") as $slug
      | (.CloudID // .cloudId // .id // "") as $cloud_id
      | select($provider == "proxmox" and ($lease | type) == "string" and ($lease | length) > 0)
      | select(($cloud_id | tostring | length) > 0)
      | [$lease, ($slug | tostring), ($cloud_id | tostring)]
      | @tsv
    '
}

baseline_inventory="$proof_dir/list-before.inventory.tsv"

reconcile_new_smoke_lease() {
  local after_raw="$1"
  local after_inventory="$proof_dir/list-reconcile.inventory.tsv"
  local candidate_lease=""
  local candidate_count=0
  local candidate_slug=""
  local cloud_id=""

  secure_log_file "$after_inventory"
  if ! extract_list_inventory "$after_raw" | sort -u >"$after_inventory"; then
    echo "step=lease-reconcile status=fail reason=list_json_invalid" | tee -a "$summary"
    return 1
  fi
  while IFS=$'\t' read -r candidate candidate_slug cloud_id; do
    is_owned_smoke_lease "$candidate" "$candidate_slug" || continue
    if awk -F '\t' -v id="$cloud_id" '$3 == id { found = 1 } END { exit !found }' "$baseline_inventory"; then
      continue
    fi
    candidate_lease="$candidate"
    candidate_count=$((candidate_count + 1))
  done <"$after_inventory"

  if [[ "$candidate_count" -eq 0 ]]; then
    echo "step=lease-reconcile status=pass reason=no_new_matching_lease" | tee -a "$summary"
    return 0
  fi
  if [[ "$candidate_count" -ne 1 ]]; then
    echo "step=lease-reconcile status=fail reason=ambiguous_new_matching_leases count=$candidate_count" | tee -a "$summary"
    return 1
  fi
  echo "step=lease-reconcile status=attempt lease=$candidate_lease" | tee -a "$summary"
  run_step stop-reconciled "$bin" stop --provider proxmox --id "$candidate_lease"
}

verify_no_new_smoke_lease() {
  local final_raw="$1"
  local final_inventory="$proof_dir/list-final.inventory.tsv"
  local candidate_count=0
  local candidate_slug=""
  local cloud_id=""

  secure_log_file "$final_inventory"
  if ! extract_list_inventory "$final_raw" | sort -u >"$final_inventory"; then
    echo "step=lease-final-verify status=fail reason=list_json_invalid" | tee -a "$summary"
    return 1
  fi
  while IFS=$'\t' read -r candidate candidate_slug cloud_id; do
    is_owned_smoke_lease "$candidate" "$candidate_slug" || continue
    if awk -F '\t' -v id="$cloud_id" '$3 == id { found = 1 } END { exit !found }' "$baseline_inventory"; then
      continue
    fi
    candidate_count=$((candidate_count + 1))
  done <"$final_inventory"
  if [[ "$candidate_count" -ne 0 ]]; then
    echo "step=lease-final-verify status=fail reason=new_matching_leases_remain count=$candidate_count" | tee -a "$summary"
    return 1
  fi
  echo "step=lease-final-verify status=pass" | tee -a "$summary"
}

failure=0

run_step doctor "$bin" doctor --provider proxmox --json || failure=1
if [[ -f "$proof_dir/doctor.raw.log" ]]; then
  validate_doctor_json || {
    echo "step=doctor-json status=fail log=$(summary_log_path "$proof_dir/doctor.redacted.log")" | tee -a "$summary"
    failure=1
  }
fi

list_before_status=0
run_step list-before "$bin" list --provider proxmox --json || list_before_status=$?
secure_log_file "$baseline_inventory"
if [[ "$list_before_status" -ne 0 ]] || ! extract_list_inventory "$proof_dir/list-before.raw.log" | sort -u >"$baseline_inventory"; then
  echo "step=list-before-json status=fail" | tee -a "$summary"
  failure=1
fi
run_node_inventory || failure=1

if [[ "$live" != "1" ]]; then
  echo "classification=external_user_owned reason=CRABBOX_PROXMOX_LIVE_SMOKE not set to 1; no mutating lease proof attempted" | tee -a "$summary"
  echo "proof_dir=<proof-dir>"
  exit "$failure"
fi

if [[ "$failure" -ne 0 ]]; then
  echo "step=lifecycle status=skip reason=preflight_failed" | tee -a "$summary"
  echo "classification=environment_blocked proof_dir=<proof-dir>" | tee -a "$summary"
  echo "proof_dir=<proof-dir>"
  exit 1
fi

warmup_status=0
run_step warmup "$bin" warmup --provider proxmox --slug "$slug" --keep || warmup_status=$?
extract_owned_lease_ids
if [[ "$warmup_status" -ne 0 ]]; then
  echo "step=lifecycle status=fail reason=warmup_failed" | tee -a "$summary"
  failure=1
fi

if [[ "$warmup_status" -eq 0 && -n "$lease_id" ]]; then
  run_step status "$bin" status --provider proxmox --id "$lease_id" --json || failure=1
  run_step ssh-command "$bin" ssh --provider proxmox --id "$lease_id" || failure=1
  stop_status=0
  run_step stop "$bin" stop --provider proxmox --id "$lease_id" || stop_status=$?
  if [[ "$stop_status" -ne 0 ]]; then
    failure=1
  fi
  cleanup_dry_run_status=0
  run_step cleanup-dry-run "$bin" cleanup --provider proxmox --dry-run || cleanup_dry_run_status=$?
  if [[ "$cleanup_dry_run_status" -ne 0 ]]; then
    failure=1
  fi
elif [[ -n "$lease_id" ]]; then
  stop_status=0
  run_step stop-reconciled "$bin" stop --provider proxmox --id "$lease_id" || stop_status=$?
else
  if [[ "$warmup_status" -eq 0 ]]; then
    echo "step=lease-id status=fail reason=warmup output did not include an owned lease id" | tee -a "$summary"
    failure=1
  fi
fi
list_after_status=0
run_step list-after "$bin" list --provider proxmox --json || list_after_status=$?
if [[ "$list_after_status" -ne 0 ]]; then
  failure=1
else
  if reconcile_new_smoke_lease "$proof_dir/list-after.raw.log"; then
    list_final_status=0
    run_step list-final "$bin" list --provider proxmox --json || list_final_status=$?
    if [[ "$list_final_status" -ne 0 ]] || ! verify_no_new_smoke_lease "$proof_dir/list-final.raw.log"; then
      failure=1
    fi
  else
    failure=1
  fi
fi

if [[ "$failure" -eq 0 ]]; then
  echo "classification=live_proof_complete proof_dir=<proof-dir>" | tee -a "$summary"
else
  echo "classification=environment_blocked proof_dir=<proof-dir>" | tee -a "$summary"
fi

echo "proof_dir=<proof-dir>"
exit "$failure"
