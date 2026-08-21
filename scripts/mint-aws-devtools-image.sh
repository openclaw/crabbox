#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CRABBOX_BIN="${CRABBOX_BIN:-$ROOT/bin/crabbox}"
target="${CRABBOX_IMAGE_TARGET:-linux}"
region="${CRABBOX_IMAGE_REGION:-${CRABBOX_AWS_REGION:-}}"
server_type="${CRABBOX_IMAGE_TYPE:-}"
server_class="${CRABBOX_IMAGE_CLASS:-standard}"
image_name="${CRABBOX_IMAGE_NAME:-}"
log_dir="${CRABBOX_IMAGE_LOG_DIR:-.crabbox}"
ttl="${CRABBOX_IMAGE_TTL:-2h}"
idle_timeout="${CRABBOX_IMAGE_IDLE_TIMEOUT:-30m}"
wait_timeout="${CRABBOX_IMAGE_WAIT_TIMEOUT:-60m}"
prep_wait_timeout="${CRABBOX_IMAGE_PREP_WAIT_TIMEOUT:-90m}"
reboot_wait_timeout="${CRABBOX_IMAGE_REBOOT_WAIT_TIMEOUT:-25m}"
reboot_settle_seconds="${CRABBOX_IMAGE_REBOOT_SETTLE_SECONDS:-30}"
reboot_ready_settle_seconds="${CRABBOX_IMAGE_REBOOT_READY_SETTLE_SECONDS:-180}"
windows_warmup_wait_timeout="${CRABBOX_IMAGE_WINDOWS_WARMUP_WAIT_TIMEOUT:-15m}"
windows_warmup_settle_seconds="${CRABBOX_IMAGE_WINDOWS_WARMUP_SETTLE_SECONDS:-90}"
fast_snapshot_restore="${CRABBOX_IMAGE_FAST_SNAPSHOT_RESTORE:-0}"
fast_snapshot_restore_azs="${CRABBOX_IMAGE_FAST_SNAPSHOT_RESTORE_AZS:-}"
run="${CRABBOX_IMAGE_RUN:-0}"
promote="${CRABBOX_IMAGE_PROMOTE:-0}"
keep_lease="${CRABBOX_IMAGE_KEEP_LEASE:-0}"
desktop="${CRABBOX_IMAGE_DESKTOP:-auto}"
browser="${CRABBOX_IMAGE_BROWSER:-auto}"
windows_mode="${CRABBOX_WINDOWS_MODE:-normal}"
prep_script="${CRABBOX_IMAGE_PREP_SCRIPT:-}"
recipe="${CRABBOX_IMAGE_RECIPE:-}"
candidate_output="${CRABBOX_IMAGE_CANDIDATE_OUTPUT:-}"
base_image="${CRABBOX_IMAGE_BASE_IMAGE:-}"
architecture="${CRABBOX_IMAGE_ARCHITECTURE:-x86_64}"
previous_default="${CRABBOX_IMAGE_PREVIOUS_DEFAULT:-}"
source_repository="${CRABBOX_IMAGE_SOURCE_REPOSITORY:-${GITHUB_REPOSITORY:-}}"
source_commit="${CRABBOX_IMAGE_SOURCE_COMMIT:-${GITHUB_SHA:-}}"
workflow_ref="${CRABBOX_IMAGE_WORKFLOW_REF:-${GITHUB_WORKFLOW_REF:-}}"
workflow_run_id="${CRABBOX_IMAGE_WORKFLOW_RUN_ID:-${GITHUB_RUN_ID:-local}}"
workflow_run_attempt="${CRABBOX_IMAGE_WORKFLOW_RUN_ATTEMPT:-${GITHUB_RUN_ATTEMPT:-1}}"
oci_repository="${CRABBOX_IMAGE_CANDIDATE_REPOSITORY:-}"
windows_reboot_marker='C:\ProgramData\crabbox\image-prep-reboot-required'
scrub_report=""
checkpoint_file=""
checks_file=""

usage() {
  cat <<'USAGE'
Usage: scripts/mint-aws-devtools-image.sh --target linux|windows [flags]

Mint AWS developer-tool AMI candidates for normal Crabbox leases.
By default this prints the plan and exits before paid work. Add --run to create
source/candidate leases, run scrub gates, and write an immutable evidence bundle.

Flags:
  --target TARGET       linux or windows
  --region REGION       AWS region
  --class CLASS         Crabbox machine class, default standard
  --type TYPE           AWS instance type
  --name NAME           image name
  --run                 allow paid lease/image work
  --no-promote          candidate-only flow (default)
  --promote             legacy explicit promotion flow; never used by publication
  --fast-snapshot-restore
                       enable AWS Fast Snapshot Restore when promoting
  --fsr-az AZ           availability zone for Fast Snapshot Restore; repeatable
  --keep-lease          keep proof leases alive
  --desktop             request desktop bootstrap
  --no-desktop          do not request desktop bootstrap
  --no-browser          do not request browser bootstrap on Linux
  --windows-mode MODE   normal or wsl2, default normal
  --prep-script PATH    override target prep script
  --recipe PATH         override the versioned AWS image recipe
  --candidate-output DIR
                       atomic candidate evidence bundle directory
  --base-image AMI      exact source AMI id, required with --run
  --architecture ARCH   x86_64 or arm64, default x86_64
  --previous-default AMI
                       previous promoted AMI recorded for rollback evidence
  --oci-repository REF lowercase GHCR repository recorded in the evidence bundle
  -h, --help            show this help

Useful env:
  CRABBOX_BIN
  CRABBOX_IMAGE_RUN
  CRABBOX_IMAGE_PROMOTE
  CRABBOX_IMAGE_CANDIDATE_OUTPUT
  CRABBOX_IMAGE_BASE_IMAGE
  CRABBOX_IMAGE_ARCHITECTURE
  CRABBOX_IMAGE_PREVIOUS_DEFAULT
  CRABBOX_IMAGE_CANDIDATE_REPOSITORY
  CRABBOX_IMAGE_KEEP_LEASE
  CRABBOX_IMAGE_LOG_DIR
  CRABBOX_IMAGE_WAIT_TIMEOUT
  CRABBOX_IMAGE_PREP_WAIT_TIMEOUT
  CRABBOX_IMAGE_REBOOT_WAIT_TIMEOUT
  CRABBOX_IMAGE_REBOOT_SETTLE_SECONDS
  CRABBOX_IMAGE_REBOOT_READY_SETTLE_SECONDS
  CRABBOX_IMAGE_WINDOWS_WARMUP_WAIT_TIMEOUT
  CRABBOX_IMAGE_WINDOWS_WARMUP_SETTLE_SECONDS
  CRABBOX_IMAGE_FAST_SNAPSHOT_RESTORE
  CRABBOX_IMAGE_FAST_SNAPSHOT_RESTORE_AZS
USAGE
}

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --target)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      target="$2"
      shift 2
      ;;
    --region)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      region="$2"
      shift 2
      ;;
    --type)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      server_type="$2"
      shift 2
      ;;
    --class)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      server_class="$2"
      shift 2
      ;;
    --name)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      image_name="$2"
      shift 2
      ;;
    --run)
      run=1
      shift
      ;;
    --no-promote)
      promote=0
      shift
      ;;
    --promote)
      promote=1
      shift
      ;;
    --fast-snapshot-restore)
      fast_snapshot_restore=1
      shift
      ;;
    --fsr-az)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      if [[ -n "$fast_snapshot_restore_azs" ]]; then
        fast_snapshot_restore_azs+=",$2"
      else
        fast_snapshot_restore_azs="$2"
      fi
      shift 2
      ;;
    --keep-lease)
      keep_lease=1
      shift
      ;;
    --desktop)
      desktop=1
      shift
      ;;
    --no-desktop)
      desktop=0
      shift
      ;;
    --no-browser)
      browser=0
      shift
      ;;
    --windows-mode)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      windows_mode="$2"
      shift 2
      ;;
    --prep-script)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      prep_script="$2"
      shift 2
      ;;
    --recipe)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      recipe="$2"
      shift 2
      ;;
    --candidate-output)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      candidate_output="$2"
      shift 2
      ;;
    --base-image)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      base_image="$2"
      shift 2
      ;;
    --architecture)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      architecture="$2"
      shift 2
      ;;
    --previous-default)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      previous_default="$2"
      shift 2
      ;;
    --oci-repository)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      oci_repository="$2"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      printf 'unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

case "$target" in
  linux | windows) ;;
  *)
    printf 'target must be linux or windows, got %s\n' "$target" >&2
    exit 2
    ;;
esac

invocation_id="$(date -u +%Y%m%d-%H%M%S)-$$-${RANDOM}"
log_id="$(printf '%s' "$invocation_id" | tr -c 'A-Za-z0-9_.-' '_')"
if [[ -z "$image_name" ]]; then
  image_name="crabbox-${target}-devtools-${log_id}"
fi
log_image_name="$(printf '%s' "$image_name" | tr -c 'A-Za-z0-9_.-' '_')"
if [[ -z "$source_repository" ]]; then
  origin_url="$(git -C "$ROOT" config --get remote.origin.url || true)"
  source_repository="$(node -e '
const value = process.argv[1] ?? "";
const match = /github[.]com[/:]([^/]+)\/([^/]+?)(?:[.]git)?$/.exec(value);
if (match) process.stdout.write(`${match[1]}/${match[2]}`);
' "$origin_url")"
fi
if [[ -z "$source_commit" ]]; then
  source_commit="$(git -C "$ROOT" rev-parse HEAD)"
fi
if [[ -z "$candidate_output" ]]; then
  candidate_output="$log_dir/aws-image-candidate-${target}-${log_id}"
fi
if [[ -z "$prep_script" ]]; then
  if [[ "$target" == "windows" ]]; then
    prep_script="$ROOT/scripts/install-windows-developer-tools.ps1"
  else
    prep_script="$ROOT/scripts/install-linux-developer-tools.sh"
  fi
fi
if [[ -z "$recipe" ]]; then
  recipe="$ROOT/recipes/aws/v1/${target}-devtools.json"
fi
if [[ "$browser" == "auto" ]]; then
  if [[ "$target" == "linux" ]]; then
    browser=1
  else
    browser=0
  fi
fi
if [[ "$desktop" == "auto" ]]; then
  if [[ "$target" == "windows" ]]; then
    desktop=0
  else
    desktop=1
  fi
fi

if [[ ! -x "$CRABBOX_BIN" ]]; then
  printf 'CRABBOX_BIN is not executable: %s\n' "$CRABBOX_BIN" >&2
  exit 2
fi
if [[ ! -f "$prep_script" ]]; then
  printf 'prep script not found: %s\n' "$prep_script" >&2
  exit 2
fi
if [[ ! -f "$recipe" ]]; then
  printf 'image recipe not found: %s\n' "$recipe" >&2
  exit 2
fi
if [[ ! "$architecture" =~ ^(x86_64|arm64)$ ]]; then
  printf 'architecture must be x86_64 or arm64\n' >&2
  exit 2
fi
if [[ "$fast_snapshot_restore" == "1" && "$promote" != "1" ]]; then
  printf 'Fast Snapshot Restore is a promotion-only action; candidate runs reject it\n' >&2
  exit 2
fi
if [[ "$keep_lease" == "1" && "$promote" != "1" ]]; then
  printf 'candidate evidence is written only after lease cleanup; --keep-lease requires --promote\n' >&2
  exit 2
fi

source_lease=""
candidate_lease=""
promoted_lease=""

cleanup() {
  rm -f "$checkpoint_file" "$checks_file"
  [[ "$keep_lease" == "1" ]] && return 0
  for lease in "$promoted_lease" "$candidate_lease" "$source_lease"; do
    [[ -n "$lease" ]] || continue
    "$CRABBOX_BIN" stop --provider aws --target "$target" "$lease" || true
  done
}
trap cleanup EXIT

run_cmd() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  "$@"
}

duration_seconds() {
  case "$1" in
    *h) printf '%s\n' "$((${1%h} * 3600))" ;;
    *m) printf '%s\n' "$((${1%m} * 60))" ;;
    *s) printf '%s\n' "${1%s}" ;;
    *) printf '%s\n' "$1" ;;
  esac
}

wait_windows_ssh_probe() {
  local lease="$1"
  local timeout_value="$2"
  local deadline
  deadline=$((SECONDS + $(duration_seconds "$timeout_value")))
  while true; do
    if run_cmd "$CRABBOX_BIN" run --provider aws --target windows --id "$lease" --no-sync --shell -- 'Write-Output "windows-ssh-ready"' >&2; then
      return 0
    fi
    if ((SECONDS >= deadline)); then
      printf 'Windows SSH probe did not succeed within %s\n' "$timeout_value" >&2
      return 1
    fi
    sleep 15
  done
}

wait_windows_reboot_ready() {
  local lease="$1"
  wait_windows_ssh_probe "$lease" "$reboot_wait_timeout"
  if ((reboot_ready_settle_seconds > 0)); then
    printf 'Windows SSH responded after reboot; settling for %ss before continuing\n' "$reboot_ready_settle_seconds" >&2
    sleep "$reboot_ready_settle_seconds"
    wait_windows_ssh_probe "$lease" "$reboot_wait_timeout"
  fi
}

run_windows_shell_retry() {
  local lease="$1"
  local label="$2"
  local command="$3"
  local attempt
  for attempt in 1 2 3; do
    if run_cmd "$CRABBOX_BIN" run --provider aws --target windows --id "$lease" --no-sync --shell -- "$command"; then
      return 0
    fi
    if ((attempt == 3)); then
      break
    fi
    printf 'Windows command failed during %s; waiting for SSH before retry %s/3\n' "$label" "$((attempt + 1))" >&2
    wait_windows_ssh_probe "$lease" "$reboot_wait_timeout"
    sleep 15
  done
  return 1
}

windows_prep_start_command() {
  printf '%s\n' "$(<"$ROOT/scripts/aws-image-windows-prep-start.ps1")"
}

windows_prep_status_command() {
  printf '%s\n' "$(<"$ROOT/scripts/aws-image-windows-prep-status.ps1")"
}

wait_windows_prep_task() {
  local lease="$1"
  local status_command output normalized status deadline
  status_command="$(windows_prep_status_command)"
  deadline=$((SECONDS + $(duration_seconds "$prep_wait_timeout")))
  while true; do
    status=0
    output="$("$CRABBOX_BIN" run --provider aws --target windows --id "$lease" --no-sync --shell -- "$status_command" 2>&1)" || status=$?
    printf '%s\n' "$output" >&2
    normalized="${output//$'\r'/}"
    if grep -qx 'crabbox-prep-done' <<<"$normalized"; then
      return 0
    fi
    if grep -qx 'crabbox-prep-failed' <<<"$normalized"; then
      return 1
    fi
    if ((SECONDS >= deadline)); then
      printf 'Windows prep task did not finish within %s\n' "$prep_wait_timeout" >&2
      return 1
    fi
    if [[ "$status" -ne 0 ]]; then
      printf 'Windows prep status unavailable; waiting for SSH before next poll\n' >&2
    fi
    sleep 30
  done
}

warmup_args() {
  printf '%s\0' warmup --provider aws --target "$target" --class "$server_class" --market on-demand --ttl "$ttl" --idle-timeout "$idle_timeout" --timing-json
  [[ -n "$server_type" ]] && printf '%s\0' --type "$server_type"
  [[ "$desktop" == "1" ]] && printf '%s\0' --desktop
  [[ "$browser" == "1" ]] && printf '%s\0' --browser
  [[ "$target" == "windows" ]] && printf '%s\0' --windows-mode "$windows_mode"
}

lease_from_log() {
  node -e '
const fs = require("fs");
const text = fs.readFileSync(process.argv[1], "utf8");
for (const line of text.trim().split(/\n/).reverse()) {
  try {
    const json = JSON.parse(line);
    if (json.leaseId) {
      console.log(json.leaseId);
      process.exit(0);
    }
  } catch {}
}
process.exit(1);
' "$1"
}

assert_selected_image() {
  local log="$1"
  local image_id="$2"
  local source="$3"
  if ! grep -Fq "image selected id=$image_id source=$source" "$log"; then
    printf 'warmup did not prove image selection id=%s source=%s; log=%s\n' \
      "$image_id" "$source" "$log" >&2
    return 1
  fi
  printf '%s image selection proved: %s\n' "$source" "$image_id" >&2
}

assert_image_id() {
  local log="$1"
  local image_id="$2"
  if ! grep -Eq "^image selected id=${image_id} source=" "$log"; then
    printf 'warmup did not prove selected image id=%s; log=%s\n' "$image_id" "$log" >&2
    return 1
  fi
}

warmup() {
  local label="$1"
  local log
  mkdir -p "$log_dir"
  log="$(mktemp "$log_dir/image-mint-${log_image_name}-${label}-${log_id}.log.XXXXXX")"
  local -a args
  while IFS= read -r -d '' arg; do args+=("$arg"); done < <(warmup_args)
  local -a env_args=()
  [[ -n "$region" ]] && env_args+=(CRABBOX_AWS_REGION="$region" AWS_REGION="$region")
  [[ "$label" == "source" && -n "$base_image" ]] && env_args+=(CRABBOX_AWS_AMI="$base_image")
  [[ "$label" == "candidate" ]] && env_args+=(CRABBOX_AWS_AMI="$2")
  printf 'warming %s lease log=%s\n' "$label" "$log" >&2
  local warmup_status=0
  if [[ "${#env_args[@]}" -gt 0 ]]; then
    run_cmd env "${env_args[@]}" "$CRABBOX_BIN" "${args[@]}" 2>&1 | tee "$log" >&2 || warmup_status=$?
  else
    run_cmd "$CRABBOX_BIN" "${args[@]}" 2>&1 | tee "$log" >&2 || warmup_status=$?
  fi
  local lease
  lease="$(lease_from_log "$log" || true)"
  if [[ "$warmup_status" -ne 0 ]]; then
    if [[ -n "$lease" && "$keep_lease" != "1" ]]; then
      run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$lease" >&2 || true
    fi
    return "$warmup_status"
  fi
  if [[ -z "$lease" ]]; then
    printf 'warmup did not return a lease id for %s\n' "$label" >&2
    return 1
  fi
  if [[ "$label" == "candidate" ]]; then
    if ! assert_selected_image "$log" "$2" explicit; then
      return 1
    fi
  elif [[ "$label" == "promoted" ]]; then
    if ! assert_selected_image "$log" "$ami_id" promoted; then
      return 1
    fi
  fi
  if [[ "$target" == "windows" ]]; then
    sleep "$windows_warmup_settle_seconds"
    if ! wait_windows_ssh_probe "$lease" "$windows_warmup_wait_timeout"; then
      [[ "$keep_lease" == "1" ]] || run_cmd "$CRABBOX_BIN" stop --provider aws --target windows "$lease" >&2 || true
      return 1
    fi
  fi
  printf '%s\n' "$lease"
}

smoke() {
  local lease="$1"
  local smoke_script
  smoke_script="$ROOT/scripts/smoke-aws-${target}-devtools-image.$([[ "$target" == windows ]] && printf ps1 || printf sh)"
  run_cmd "$CRABBOX_BIN" run --provider aws --target "$target" --id "$lease" --no-sync --script "$smoke_script"
}

run_prep() {
  local lease="$1"
  if [[ "$target" == "windows" ]]; then
    local encoded chunk_size offset chunk remote_dir remote_script command decode_and_run part_index part_name
    encoded="$(base64 <"$prep_script" | tr -d '\n')"
    chunk_size=1800
    remote_dir='C:\ProgramData\crabbox'
    remote_script='C:\ProgramData\crabbox\image-prep.ps1'
    decode_and_run="; \$__crabboxParts = Get-ChildItem -Path '$remote_dir' -Filter 'image-prep.part-*' | Sort-Object Name; \$__crabboxPrep = (\$__crabboxParts | ForEach-Object { Get-Content -Raw \$_.FullName }) -join ''; [IO.File]::WriteAllBytes('$remote_script', [Convert]::FromBase64String(\$__crabboxPrep)); Write-Output 'crabbox-prep-uploaded'"
    run_windows_shell_retry "$lease" "prep upload init" "New-Item -ItemType Directory -Force -Path '$remote_dir' | Out-Null; Remove-Item -Path '$remote_dir\\image-prep.part-*' -Force -ErrorAction SilentlyContinue"
    part_index=0
    for ((offset = 0; offset < ${#encoded}; offset += chunk_size)); do
      chunk="${encoded:offset:chunk_size}"
      printf -v part_name 'image-prep.part-%05d' "$part_index"
      command="Set-Content -Path '$remote_dir\\$part_name' -Value '$chunk' -NoNewline"
      if ((offset + chunk_size >= ${#encoded})); then
        command+="$decode_and_run"
      fi
      if ! run_windows_shell_retry "$lease" "prep upload $part_name" "$command"; then
        if ((offset + chunk_size >= ${#encoded})) && recover_windows_prep_disconnect "$lease"; then
          return 0
        fi
        return 1
      fi
      part_index=$((part_index + 1))
    done
    run_windows_shell_retry "$lease" "prep task start" "$(windows_prep_start_command)"
    wait_windows_prep_task "$lease"
    return
  fi
  run_cmd "$CRABBOX_BIN" run --provider aws --target "$target" --id "$lease" --no-sync --script "$prep_script"
}

extract_json_schema() {
  local schema="$1"
  node -e '
const fs = require("fs");
const schema = process.argv[1];
const lines = fs.readFileSync(0, "utf8").trim().split(/\r?\n/).reverse();
for (const line of lines) {
  try {
    const value = JSON.parse(line);
    if (value && value.schema === schema) {
      process.stdout.write(`${JSON.stringify(value)}\n`);
      process.exit(0);
    }
  } catch {}
}
process.exit(1);
' "$schema"
}

run_image_scrub() {
  local lease="$1"
  local raw normalized report_tmp
  mkdir -p "$log_dir"
  report_tmp="$(mktemp "$log_dir/image-scrub-${target}-${log_id}.json.XXXXXX")"
  if [[ "$target" == "linux" ]]; then
    raw="$("$CRABBOX_BIN" run --provider aws --target linux --id "$lease" --no-sync \
      --script "$ROOT/scripts/scrub-aws-image.mjs" -- \
      --target linux --report - --require-root)"
  else
    raw="$("$CRABBOX_BIN" run --provider aws --target windows --id "$lease" --no-sync \
      --script "$ROOT/scripts/scrub-aws-windows-image.ps1")"
  fi
  if ! normalized="$(printf '%s\n' "$raw" | extract_json_schema crabbox-aws-image-scrub/v1)"; then
    rm -f "$report_tmp"
    printf 'image scrub did not return a valid structured report\n' >&2
    return 1
  fi
  printf '%s' "$normalized" >"$report_tmp"
  if ! node -e '
const fs = require("fs");
const value = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
if (
  value.schema !== "crabbox-aws-image-scrub/v1" ||
  !Array.isArray(value.findings) ||
  value.findings.length !== 0 ||
  !/^sha256:[0-9a-f]{64}$/.test(value.evidenceDigest ?? "")
) {
  process.exit(1);
}
' "$report_tmp"; then
    rm -f "$report_tmp"
    printf 'image scrub reported residual findings or invalid evidence\n' >&2
    return 1
  fi
  scrub_report="$log_dir/image-scrub-${target}-${log_id}.json"
  mv -f "$report_tmp" "$scrub_report"
}

mark_linux_image_ready() {
  local lease="$1"
  [[ "$target" == "linux" ]] || return 0
  local builder_digest
  builder_digest="$(node --input-type=module -e '
import { readFileSync } from "node:fs";
import { digest } from "./scripts/generate-linux-readiness.mjs";
process.stdout.write(digest(JSON.parse(readFileSync("./recipes/linux/v1/linux-builder.json", "utf8"))));
' </dev/null)"
  run_cmd "$CRABBOX_BIN" run --provider aws --target linux --id "$lease" --no-sync --shell -- \
    "set -euo pipefail
command -v git >/dev/null
command -v curl >/dev/null
command -v rsync >/dev/null
command -v jq >/dev/null
command -v tmux >/dev/null
command -v flock >/dev/null
test -x /usr/sbin/sshd
sudo install -d -m 0755 /var/lib/crabbox
sudo install -d -m 0755 /var/lib/crabbox/readiness
printf 'crabbox-devtools-v1\\n' | sudo tee /var/lib/crabbox/image-ready >/dev/null
sudo chmod 0644 /var/lib/crabbox/image-ready
inventory_digest=\"sha256:\$(dpkg-query -W -f='\${binary:Package}=\${Version}\\n' | LC_ALL=C sort | sha256sum | awk '{print \$1}')\"
manifest_tmp=\"\$(sudo mktemp /var/lib/crabbox/readiness/.linux.json.XXXXXX)\"
printf '{\"inventoryDigest\":\"%s\",\"profile\":\"linux-builder\",\"recipeDigest\":\"$builder_digest\",\"schema\":\"crabbox-linux-readiness/v1\"}\\n' \"\$inventory_digest\" |
  sudo tee \"\$manifest_tmp\" >/dev/null
sudo chown root:root \"\$manifest_tmp\"
sudo chmod 0644 \"\$manifest_tmp\"
sudo mv -f \"\$manifest_tmp\" /var/lib/crabbox/readiness/linux.json"
}

windows_reboot_required() {
  local lease="$1"
  local output
  output="$("$CRABBOX_BIN" run --provider aws --target windows --id "$lease" --no-sync --shell -- "if (Test-Path '$windows_reboot_marker') { Write-Output 'crabbox-reboot-required' } else { Write-Output 'crabbox-reboot-not-required' }")"
  printf '%s\n' "$output"
  grep -q 'crabbox-reboot-required' <<<"$output"
}

recover_windows_prep_disconnect() {
  local lease="$1"
  printf 'Windows prep command failed or disconnected; checking whether a planned Docker reboot is pending\n' >&2
  for _ in 1 2 3; do
    if ! wait_windows_ssh_probe "$lease" "$reboot_wait_timeout"; then
      return 1
    fi
    if windows_reboot_required "$lease"; then
      return 0
    fi
    sleep 30
  done
  printf 'Windows prep command failed or disconnected and no reboot marker was found\n' >&2
  return 1
}

reboot_windows_source_if_needed() {
  local lease="$1"
  [[ "$target" == "windows" ]] || return 0
  if ! windows_reboot_required "$lease"; then
    return 0
  fi
  printf 'Windows source lease requires reboot before Docker image pull/proof\n' >&2
  run_cmd "$CRABBOX_BIN" run --provider aws --target windows --id "$lease" --no-sync --shell -- 'shutdown /r /t 5 /f; Write-Output "reboot scheduled"'
  sleep "$reboot_settle_seconds"
  wait_windows_reboot_ready "$lease"
  run_prep "$lease"
  if windows_reboot_required "$lease"; then
    printf 'Windows prep still requires reboot after one reboot cycle\n' >&2
    exit 1
  fi
}

printf '%s\n' \
  'AWS devtools image mint' \
  "  target: $target" \
  "  image:  $image_name" \
  "  region: ${region:-auto}" \
  "  class:  $server_class" \
  "  type:   ${server_type:-auto}" \
  "  prep:   $prep_script" \
  "  recipe: $recipe" \
  "  base:   ${base_image:-required-for-run}" \
  "  arch:   $architecture" \
  "  proof:  desktop=$desktop browser=$browser promote=$promote" \
  "  fsr:    enabled=$fast_snapshot_restore azs=${fast_snapshot_restore_azs:-auto}" \
  "  paid:   run=$run keep_lease=$keep_lease" >&2

if [[ "$run" != "1" ]]; then
  printf 'dry plan only; add --run to create source/candidate leases and AMIs.\n'
  exit 0
fi
[[ "$base_image" =~ ^ami-[0-9a-z]+$ ]] || {
  printf 'an exact --base-image ami-* value is required with --run\n' >&2
  exit 2
}
[[ "$region" =~ ^[a-z]{2}(-gov)?-[a-z]+-[0-9]+$ ]] || {
  printf 'an exact AWS --region is required with --run\n' >&2
  exit 2
}
[[ "$server_type" =~ ^[A-Za-z0-9.-]+$ ]] || {
  printf 'an exact AWS --type is required with --run\n' >&2
  exit 2
}
[[ -z "$previous_default" || "$previous_default" =~ ^ami-[0-9a-z]+$ ]] || {
  printf 'previous default must be an exact ami-* value\n' >&2
  exit 2
}
[[ -z "$oci_repository" || "$oci_repository" =~ ^ghcr\.io/[a-z0-9_.-]+(/[a-z0-9_.-]+)+$ ]] || {
  printf 'candidate repository must be a lowercase GHCR repository without a tag\n' >&2
  exit 2
}
[[ "$source_repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || {
  printf 'source repository must be owner/name\n' >&2
  exit 2
}
[[ "$source_commit" =~ ^[0-9a-f]{40}$ ]] || {
  printf 'source commit must be a full lowercase Git SHA\n' >&2
  exit 2
}
[[ "$workflow_ref" =~ ^"$source_repository"/\.github/workflows/[A-Za-z0-9._/-]+\.ya?ml@refs/heads/[A-Za-z0-9._/-]+$ ]] || {
  printf 'workflow ref must bind the source repository, workflow path, and protected branch\n' >&2
  exit 2
}
[[ "$workflow_run_id" =~ ^(local|[1-9][0-9]*)$ ]] || {
  printf 'workflow run id must be local or a positive integer\n' >&2
  exit 2
}
[[ "$workflow_run_attempt" =~ ^[1-9][0-9]*$ ]] || {
  printf 'workflow run attempt must be a positive integer\n' >&2
  exit 2
}
if [[ -e "$candidate_output" ]]; then
  printf 'candidate output already exists: %s\n' "$candidate_output" >&2
  exit 2
fi

source_lease="$(warmup source)"
source_log="$(find "$log_dir" -maxdepth 1 -type f -name "image-mint-${log_image_name}-source-${log_id}.log.*" -print -quit)"
[[ -n "$source_log" ]] || {
  printf 'source warmup log was not created\n' >&2
  exit 1
}
assert_image_id "$source_log" "$base_image"
run_prep "$source_lease"
reboot_windows_source_if_needed "$source_lease"
mark_linux_image_ready "$source_lease"
smoke "$source_lease"
run_image_scrub "$source_lease"

image_env=(env)
[[ -n "$region" ]] && image_env+=(CRABBOX_AWS_REGION="$region" AWS_REGION="$region")
image_output="$("${image_env[@]}" "$CRABBOX_BIN" checkpoint create \
  --provider aws --target "$target" --id "$source_lease" --name "$image_name" \
  --mode native --strategy image --no-reboot=false --wait --wait-timeout "$wait_timeout" --json)"
printf '%s\n' "$image_output"
checkpoint_record="$(printf '%s\n' "$image_output" | node -e '
const fs = require("fs");
const value = JSON.parse(fs.readFileSync(0, "utf8"));
if (
  value.kind !== "aws-ami" ||
  value.provider !== "aws" ||
  value.targetOS !== process.argv[1] ||
  value.native?.provider !== "aws" ||
  value.native?.kind !== "aws-ami" ||
  value.native?.region !== process.argv[2] ||
  (value.native?.architecture && value.native.architecture !== process.argv[3]) ||
  !/^ami-[0-9a-z]+$/.test(value.native?.imageId ?? "") ||
  !Array.isArray(value.native?.snapshotIds) ||
  value.native.snapshotIds.length === 0 ||
  value.native.snapshotIds.some((id) => !/^snap-[0-9a-z]+$/.test(id))
) {
  process.exit(1);
}
process.stdout.write(`${JSON.stringify(value)}\n`);
' "$target" "$region" "$architecture")"
ami_id="$(printf '%s' "$checkpoint_record" | node -e '
const fs = require("fs");
process.stdout.write(JSON.parse(fs.readFileSync(0, "utf8")).native.imageId);
')"
mkdir -p "$log_dir"
checkpoint_file="$(mktemp "$log_dir/image-checkpoint-${target}-${log_id}.json.XXXXXX")"
printf '%s' "$checkpoint_record" >"$checkpoint_file"

if [[ "$keep_lease" != "1" ]]; then
  run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$source_lease"
  source_lease=""
fi

candidate_lease="$(warmup candidate "$ami_id")"
smoke "$candidate_lease"
printf 'candidate AMI smoke passed: %s\n' "$ami_id"
if [[ "$keep_lease" != "1" ]]; then
  run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$candidate_lease"
  candidate_lease=""
fi

checks_file="$(mktemp "$log_dir/image-checks-${target}-${log_id}.json.XXXXXX")"
scrub_digest="$(node -e '
const fs = require("fs");
const value = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
if (!/^sha256:[0-9a-f]{64}$/.test(value.evidenceDigest ?? "")) process.exit(1);
process.stdout.write(value.evidenceDigest);
' "$scrub_report")"
node -e '
const fs = require("fs");
const [file, scrubDigest] = process.argv.slice(1);
fs.writeFileSync(file, `${JSON.stringify({
  schema: "crabbox-aws-image-checks/v1",
  checks: [
    { name: "source-smoke", status: "passed" },
    { name: "scrub", status: "passed", evidenceDigest: scrubDigest },
    { name: "candidate-boot", status: "passed" },
    { name: "candidate-smoke", status: "passed" },
  ],
})}\n`, { mode: 0o600 });
' "$checks_file" "$scrub_digest"

candidate_args=(
  create
  --checkpoint "$checkpoint_file"
  --scrub-report "$scrub_report"
  --checks "$checks_file"
  --recipe "$recipe"
  --out-dir "$candidate_output"
  --target "$target"
  --region "$region"
  --instance-type "${server_type:-auto}"
  --architecture "$architecture"
  --base-image "$base_image"
  --source-repository "$source_repository"
  --source-commit "$source_commit"
  --workflow-ref "$workflow_ref"
  --workflow-run-id "$workflow_run_id"
  --workflow-run-attempt "$workflow_run_attempt"
)
[[ -n "$previous_default" ]] && candidate_args+=(--previous-default "$previous_default")
[[ -n "$oci_repository" ]] && candidate_args+=(--oci-repository "$oci_repository")
candidate_result="$(node "$ROOT/scripts/aws-image-candidate.mjs" "${candidate_args[@]}")"
printf '%s\n' "$candidate_result"
rm -f "$checkpoint_file" "$checks_file"
checkpoint_file=""
checks_file=""

if [[ "$promote" != "1" ]]; then
  printf 'candidate evidence passed\n'
  exit 0
fi

promote_args=(image promote --target "$target" --json)
[[ -n "$region" ]] && promote_args+=(--region "$region")
if [[ "$fast_snapshot_restore" == "1" ]]; then
  promote_args+=(--fast-snapshot-restore)
  IFS=',' read -r -a fsr_az_values <<<"$fast_snapshot_restore_azs"
  for fsr_az in "${fsr_az_values[@]}"; do
    [[ -n "$fsr_az" ]] || continue
    promote_args+=(--fsr-az "$fsr_az")
  done
fi
promote_args+=("$ami_id")
run_cmd "$CRABBOX_BIN" "${promote_args[@]}"

promoted_lease="$(warmup promoted)"
smoke "$promoted_lease"
printf 'promoted image selection proved: %s\n' "$ami_id"
printf 'promoted %s developer image passed: %s\n' "$target" "$ami_id"
