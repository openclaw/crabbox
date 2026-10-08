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
capacity_wait="${CRABBOX_IMAGE_CAPACITY_WAIT:-45m}"
prep_wait_timeout="${CRABBOX_IMAGE_PREP_WAIT_TIMEOUT:-90m}"
reboot_wait_timeout="${CRABBOX_IMAGE_REBOOT_WAIT_TIMEOUT:-25m}"
reboot_settle_seconds="${CRABBOX_IMAGE_REBOOT_SETTLE_SECONDS:-30}"
reboot_ready_settle_seconds="${CRABBOX_IMAGE_REBOOT_READY_SETTLE_SECONDS:-180}"
windows_warmup_wait_timeout="${CRABBOX_IMAGE_WINDOWS_WARMUP_WAIT_TIMEOUT:-15m}"
windows_warmup_settle_seconds="${CRABBOX_IMAGE_WINDOWS_WARMUP_SETTLE_SECONDS:-90}"
fast_snapshot_restore="${CRABBOX_IMAGE_FAST_SNAPSHOT_RESTORE:-0}"
fast_snapshot_restore_azs="${CRABBOX_IMAGE_FAST_SNAPSHOT_RESTORE_AZS:-}"
run="${CRABBOX_IMAGE_RUN:-0}"
promote="${CRABBOX_IMAGE_PROMOTE:-1}"
keep_lease="${CRABBOX_IMAGE_KEEP_LEASE:-0}"
desktop="${CRABBOX_IMAGE_DESKTOP:-auto}"
browser="${CRABBOX_IMAGE_BROWSER:-auto}"
windows_mode="${CRABBOX_WINDOWS_MODE:-normal}"
prep_script="${CRABBOX_IMAGE_PREP_SCRIPT:-}"
linux_node_major="${CRABBOX_LINUX_NODE_MAJOR:-24}"
linux_pnpm_version="${CRABBOX_LINUX_PNPM_VERSION:-11.1.0}"
linux_pnpm_default=""
stock_source=0
windows_source_os=""
windows_source_build=""
source_root_gb=""
measured=0
max_p95_runner_total_ms=""
measurement_dir=""
measurement_policy=""
public_outcome="${CRABBOX_IMAGE_PUBLIC_OUTCOME:-}"
outcome_stage="preflight"
rollback_status="not_required"
cleanup_status="not_started"
windows_reboot_marker='C:\ProgramData\crabbox\image-prep-reboot-required'

usage() {
  cat <<'USAGE'
Usage: scripts/mint-aws-devtools-image.sh --target linux|windows [flags]

Mint and optionally promote AWS developer-tool AMIs for normal Crabbox leases.
By default this prints the plan and exits before paid work. Add --run to create
source/candidate leases and image artifacts.

Flags:
  --target TARGET       linux or windows
  --region REGION       AWS region
  --class CLASS         Crabbox machine class, default standard
  --type TYPE           AWS instance type
  --stock-source        build from stock Ubuntu or Windows (Windows requires CRABBOX_OS)
  --root-gb N           source root size, integer 16..400; Linux requires --stock-source
  --name NAME           image name
  --run                 allow paid lease/image work
  --measured            Linux only: nine fresh measurements plus three lifecycle leases
  --max-p95-runner-total-ms N
                        required positive integer threshold for measured publication
  --no-promote          smoke candidate only
  --fast-snapshot-restore
                       enable AWS Fast Snapshot Restore when promoting
  --fsr-az AZ           availability zone for Fast Snapshot Restore; repeatable
  --keep-lease          keep proof leases alive
  --desktop             request desktop bootstrap
  --no-desktop          do not request desktop bootstrap
  --no-browser          do not request browser bootstrap on Linux
  --windows-mode MODE   normal or wsl2, default normal
  --prep-script PATH    override target prep script
  -h, --help            show this help

Useful env:
  CRABBOX_BIN
  CRABBOX_OS            Linux selector for all phases; Windows stock-source selector only
  CRABBOX_IMAGE_RUN
  CRABBOX_IMAGE_PROMOTE
  CRABBOX_IMAGE_KEEP_LEASE
  CRABBOX_IMAGE_LOG_DIR
  CRABBOX_IMAGE_WAIT_TIMEOUT
  CRABBOX_IMAGE_CAPACITY_WAIT  active-lease admission retry budget per acquisition, default 45m (0 disables)
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
    --stock-source)
      stock_source=1
      shift
      ;;
    --root-gb)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      [[ -n "$2" ]] || { printf '%s\n' '--root-gb must be an integer from 16 to 400' >&2; exit 2; }
      source_root_gb="$2"
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
    --measured)
      measured=1
      shift
      ;;
    --max-p95-runner-total-ms)
      [[ "$#" -ge 2 ]] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      max_p95_runner_total_ms="$2"
      shift 2
      ;;
    --no-promote)
      promote=0
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

[[ "$capacity_wait" =~ ^(0|[1-9][0-9]{0,5})([smh])?$ ]] || {
  printf 'CRABBOX_IMAGE_CAPACITY_WAIT must be whole seconds, minutes or hours (e.g. 45m, 0 disables)\n' >&2
  exit 2
}

if [[ "$target" == windows && "$stock_source" == 1 ]]; then
  case "${CRABBOX_OS:-}" in
    windows-server:2022) windows_source_build=20348 ;;
    windows-server:2025) windows_source_build=26100 ;;
    *) printf 'Windows stock source requires CRABBOX_OS=windows-server:2022 or windows-server:2025\n' >&2; exit 2 ;;
  esac
  windows_source_os="$CRABBOX_OS"
  # Explicit Windows selectors bypass promotions. Only the source gets one;
  # candidate and promoted proofs must exercise their normal image paths.
  unset CRABBOX_OS
fi
if [[ -n "$source_root_gb" ]]; then
  [[ "$source_root_gb" =~ ^[1-9][0-9]{1,2}$ ]] && (( source_root_gb >= 16 && source_root_gb <= 400 )) || {
    printf '%s\n' '--root-gb must be an integer from 16 to 400' >&2; exit 2;
  }
  [[ "$target" != linux || "$stock_source" == 1 ]] || { printf '%s\n' '--root-gb requires --stock-source; a promoted source cannot shrink' >&2; exit 2; }
fi

# Clear ambient/file overrides for proof leases; the source opts in below.
export CRABBOX_AWS_STOCK_IMAGE=0 CRABBOX_AWS_ROOT_GB=0
if [[ -n "$region" ]]; then
  export CRABBOX_CAPACITY_REGIONS="$region"
  # A nonempty empty list clears file defaults; an empty env value is ignored.
  export CRABBOX_CAPACITY_AVAILABILITY_ZONES="${CRABBOX_CAPACITY_AVAILABILITY_ZONES:-,}"
fi

invocation_id="$(date -u +%Y%m%d-%H%M%S)-$$-${RANDOM}"
log_id="$(printf '%s' "$invocation_id" | tr -c 'A-Za-z0-9_.-' '_')"
if [[ -z "$image_name" ]]; then
  image_name="crabbox-${target}-devtools-${log_id}"
fi
log_image_name="$(printf '%s' "$image_name" | tr -c 'A-Za-z0-9_.-' '_')"
if [[ -z "$prep_script" ]]; then
  if [[ "$target" == "windows" ]]; then
    prep_script="$ROOT/scripts/install-windows-developer-tools.ps1"
  else
    prep_script="$ROOT/scripts/install-linux-developer-tools.sh"
  fi
fi
linux_developer_builder=0
if [[ "$target" == "linux" && "$prep_script" -ef "$ROOT/scripts/install-linux-developer-tools.sh" ]]; then
  linux_developer_builder=1
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

if [[ "$measured" == "1" ]]; then
  [[ "$target" == "linux" ]] || { printf 'measured publication is Linux-only\n' >&2; exit 2; }
  [[ "$max_p95_runner_total_ms" =~ ^[1-9][0-9]*$ ]] || {
    printf 'measured publication requires --max-p95-runner-total-ms with a positive integer\n' >&2
    exit 2
  }
  measurement_policy="$(node "$ROOT/scripts/devtools-image-proof.mjs" preflight \
    "$prep_script" "$region" "$server_type" "$server_class" "$max_p95_runner_total_ms" \
    "$desktop" "$browser" "$promote" "$keep_lease" "$fast_snapshot_restore" "$ttl" "$idle_timeout" "$stock_source" "${source_root_gb:-0}")"
  # Candidate selection is explicit below; baseline and promoted proof use normal selection.
  unset CRABBOX_AWS_AMI
  umask 077
  mkdir -p "$log_dir"
  measurement_dir="$(mktemp -d "$log_dir/measurement-${log_id}.XXXXXX")"
  printf '%s\n' "$measurement_policy" >"$measurement_dir/policy.json"
  if [[ -z "$public_outcome" ]]; then
    public_outcome="$measurement_dir/manifest.json"
  fi
  node "$ROOT/scripts/devtools-image-proof.mjs" initialize-policy \
    "$public_outcome" "$measurement_dir/policy.json"
elif [[ -n "$max_p95_runner_total_ms" ]]; then
  printf '%s\n' '--max-p95-runner-total-ms requires --measured' >&2
  exit 2
fi

source_lease=""
candidate_lease=""
promoted_lease=""
measurement_lease=""
measurement_handle=""
promotion_log=""
rollback_pending=0
candidate_checkpoint=""
candidate_region=""
promotion_confirmed=0
warmup_handle_dir="$log_dir/.image-mint-${log_image_name}-leases-${log_id}"

cleanup() {
  local exit_status=$?
  trap - EXIT
  if [[ "${BASH_SUBSHELL:-0}" != "0" ]]; then
    return "$exit_status"
  fi
  local finalizer_status=0
  local proof_status=0
  local receipt_path="-"
  local outcome_candidate=""
  local lease handle stop_output stop_code seen_leases="|"
  local -a cleanup_leases=()
  # A successful promotion can precede a failed receipt tee or a signal.
  if [[ -n "$promotion_log" && -n "$candidate_checkpoint" ]] &&
    jq -e --arg image "$ami_id" '.image.id == $image and (.image.revision | type == "string" and length > 0)' "$promotion_log" >/dev/null 2>&1; then
    promotion_confirmed=1
  fi
  # A signal can arrive after handle publication but before run returns or writes timing.
  if [[ -z "$measurement_lease" && -n "$measurement_handle" && -f "$measurement_handle" ]]; then
    measurement_lease="$(node "$ROOT/scripts/devtools-image-proof.mjs" handle "$measurement_handle" | jq -er .leaseId)" || {
      printf 'could not recover the active retained measurement handle\n' >&2
      finalizer_status=1
    }
  fi
  if [[ "$rollback_pending" == "1" && "$exit_status" != "0" ]]; then
    rollback_pending=0
    if rollback_promoted_image "$promotion_log"; then
      rollback_status="succeeded"
    else
      rollback_status="failed"
      finalizer_status=1
    fi
  fi
  if [[ "$keep_lease" != "1" ]]; then
    cleanup_status="succeeded"
    cleanup_leases=("$measurement_lease" "$promoted_lease" "$candidate_lease" "$source_lease")
    if [[ -d "$warmup_handle_dir" ]]; then
      for handle in "$warmup_handle_dir"/*.lease; do
        [[ -f "$handle" ]] || continue
        lease="$(cat "$handle")"
        [[ -n "$lease" ]] && cleanup_leases+=("$lease")
      done
    fi
    for lease in "${cleanup_leases[@]}"; do
      [[ -n "$lease" ]] || continue
      case "$seen_leases" in
        *"|$lease|"*) continue ;;
      esac
      seen_leases+="$lease|"
      # Only Windows failure cleanup may defer observation; preserve strict publisher cleanup below.
      if [[ "$target" == "windows" && "$exit_status" != "0" ]]; then
        stop_code=0
        stop_output="$("$CRABBOX_BIN" stop --provider aws --target "$target" "$lease" 2>&1)" || stop_code=$?
        printf '%s\n' "$stop_output" >&2
        if [[ "$stop_code" == "0" ]]; then
          continue
        elif [[ "$stop_output" == *"coordinator accepted release for $lease, but remote cleanup observation was canceled;"* ||
                "$stop_output" == *"coordinator accepted release for $lease, but remote cleanup is still pending"* ]]; then
          [[ "$cleanup_status" == "failed" ]] || cleanup_status="pending"
          printf 'Windows failure cleanup: release accepted for %s; cleanup remains unconfirmed; preserving original mint failure (exit %s) and lease handles.\n' "$lease" "$exit_status" >&2
          printf 'Check crabbox status --provider aws --id %s --json, then retry crabbox stop --provider aws --target windows %s to verify cleanup and remove retained local artifacts.\n' "$lease" "$lease" >&2
        else
          cleanup_status="failed"
          finalizer_status=1
        fi
        continue
      fi
      if ! "$CRABBOX_BIN" stop --provider aws --target "$target" "$lease"; then
        cleanup_status="failed"
        finalizer_status=1
      fi
    done
  fi
  if [[ "$exit_status" == "0" && "$finalizer_status" != "0" ]]; then
    exit_status="$finalizer_status"
  fi
  if [[ "$rollback_pending" == "1" && "$exit_status" != "0" ]]; then
    rollback_pending=0
    if rollback_promoted_image "$promotion_log"; then
      rollback_status="succeeded"
    else
      rollback_status="failed"
      finalizer_status=1
    fi
  fi
  if [[ "$exit_status" != "0" && -n "$candidate_checkpoint" && "$promotion_confirmed" != "1" ]]; then
    local delete_status=0
    local cleanup_receipt="$log_dir/image-mint-${log_image_name}-${log_id}-candidate-cleanup.json"
    # Only the checkpoint captured by this invocation owns the AMI and snapshots.
    # The checkpoint API also refuses deletion if an unacknowledged promotion pinned it.
    run_json_tee "${cleanup_receipt%.json}.log" env \
      CRABBOX_AWS_REGION="$candidate_region" AWS_REGION="$candidate_region" \
      "$CRABBOX_BIN" checkpoint delete "$candidate_checkpoint" --admin || delete_status=$?
    local candidate_cleanup_status="succeeded"
    if [[ "$delete_status" != "0" ]]; then
      candidate_cleanup_status="failed"
      cleanup_status="failed"
      printf 'FAILED to delete candidate image=%s region=%s checkpoint=%s (exit %s); retry checkpoint delete --admin\n' \
        "$ami_id" "$candidate_region" "$candidate_checkpoint" "$delete_status" >&2
    elif [[ "$cleanup_status" != "failed" && "$cleanup_status" != "pending" ]]; then
      cleanup_status="succeeded"
    fi
    if ! jq -n --arg checkpoint "$candidate_checkpoint" --arg image "$ami_id" \
      --arg region "$candidate_region" --arg status "$candidate_cleanup_status" --argjson code "$delete_status" \
      '{checkpointId: $checkpoint, imageId: $image, region: $region, status: $status, exitCode: $code}' >"$cleanup_receipt"; then
      cleanup_status="failed"
      printf 'FAILED to record candidate image cleanup receipt: %s\n' "$cleanup_receipt" >&2
    fi
  fi
  if [[ "$measured" == "1" && -n "$measurement_dir" && -n "$public_outcome" ]]; then
    if [[ -n "$promotion_log" ]] &&
      jq -e '.image.id and .image.revision' "$promotion_log" >/dev/null 2>&1; then
      receipt_path="$promotion_log"
    fi
    outcome_candidate="$(mktemp "${public_outcome}.candidate.XXXXXX")" || proof_status=$?
    if [[ "$proof_status" == "0" ]]; then
    node "$ROOT/scripts/devtools-image-proof.mjs" finalize \
      "$outcome_candidate" "$measurement_dir/policy.json" "$measurement_dir" \
      "$outcome_stage" "$exit_status" "$rollback_status" "$cleanup_status" "$receipt_path" ||
      proof_status=$?
    fi
    if [[ "$proof_status" == "0" ]]; then
      mv -f "$outcome_candidate" "$public_outcome" || proof_status=$?
    fi
    if [[ "$proof_status" != "0" ]]; then
      [[ -z "$outcome_candidate" ]] || rm -f "$outcome_candidate"
      printf 'could not finalize public measurement outcome\n' >&2
      if [[ "$exit_status" == "0" ]]; then
        exit_status="$proof_status"
      fi
      if [[ "$rollback_pending" == "1" ]]; then
        rollback_pending=0
        if rollback_promoted_image "$promotion_log"; then
          rollback_status="succeeded"
        else
          rollback_status="failed"
          finalizer_status=1
        fi
      fi
    elif [[ "$exit_status" == "0" ]]; then
      rollback_pending=0
      printf 'public measurement proof: %s\n' "$public_outcome"
    fi
  fi
  exit "$exit_status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

run_cmd() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  "$@"
}

run_json_tee() {
  local out="$1"
  shift
  local -a statuses
  printf '+' >&2
  printf ' %q' "$@" >&2
  printf '\n' >&2
  "$@" | tee "$out" &&
    statuses=("${PIPESTATUS[@]}") || statuses=("${PIPESTATUS[@]}")
  [[ "${statuses[0]}" == "0" ]] || return "${statuses[0]}"
  return "${statuses[1]}"
}

rollback_promoted_image() {
  local receipt="$1"
  local current_id previous_state rollback_image rollback_log
  if ! current_id="$(jq -er '.image.id' "$receipt" 2>/dev/null)" ||
    ! previous_state="$(jq -er '.previous.state' "$receipt" 2>/dev/null)"; then
    printf 'post-promotion failure; transactional promotion receipt is unavailable for rollback\n' >&2
    return 1
  fi
  if [[ "$previous_state" == "present" ]]; then
    rollback_image="$(jq -er '.previous.imageId' "$receipt")"
  elif [[ "$previous_state" == "absent" ]]; then
    rollback_image="none"
  else
    printf 'post-promotion failure; promotion receipt has invalid previous state\n' >&2
    return 1
  fi
  local -a args=(image promote --json --target "$target")
  [[ -n "$region" ]] && args+=(--region "$region")
  [[ -n "$server_type" ]] && args+=(--type "$server_type")
  [[ "$target" == "linux" && -n "${CRABBOX_OS:-}" ]] && args+=(--os "$CRABBOX_OS")
  args+=(--restore-receipt "$receipt" "$current_id")
  rollback_log="$(mktemp "$log_dir/image-mint-${log_image_name}-rollback-${log_id}.json.XXXXXX")"
  if ! run_json_tee "$rollback_log" "$CRABBOX_BIN" "${args[@]}"; then
    printf 'post-promotion failure; CAS rollback failed or was rejected; a newer default was not overwritten\n' >&2
    return 1
  fi
  printf 'post-promotion failure; restored previous default image=%s\n' "$rollback_image" >&2
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
  cat <<'POWERSHELL'
$dir = 'C:\ProgramData\crabbox'
$runner = Join-Path $dir 'image-prep-runner.ps1'
$script = Join-Path $dir 'image-prep.ps1'
$log = Join-Path $dir 'image-prep.log'
$exitFile = Join-Path $dir 'image-prep.exit'
$done = Join-Path $dir 'image-prep.done'
$failed = Join-Path $dir 'image-prep.failed'
$step = Join-Path $dir 'image-prep.step'
$errorFile = Join-Path $dir 'image-prep.error'
Remove-Item -Force $log,$exitFile,$done,$failed,$step,$errorFile -ErrorAction SilentlyContinue
@'
$dir = 'C:\ProgramData\crabbox'
$script = Join-Path $dir 'image-prep.ps1'
$log = Join-Path $dir 'image-prep.log'
$exitFile = Join-Path $dir 'image-prep.exit'
$done = Join-Path $dir 'image-prep.done'
$failed = Join-Path $dir 'image-prep.failed'
$step = Join-Path $dir 'image-prep.step'
$errorFile = Join-Path $dir 'image-prep.error'
$ErrorActionPreference = 'Continue'
$PSNativeCommandUseErrorActionPreference = $false
Remove-Item -Force $exitFile,$done,$failed,$errorFile -ErrorAction SilentlyContinue
Set-Content -Path $step -Value 'starting prep PowerShell process'
$LASTEXITCODE = $null
try {
  & powershell -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $script *>&1 | Tee-Object -FilePath $log -ErrorAction Stop
  $code = $LASTEXITCODE
  if ($null -eq $code) { throw 'prep PowerShell process did not return an exit code' }
} catch {
  $code = 1
  $_ | Out-String | Set-Content -Path $errorFile
}
Set-Content -Path $exitFile -Value $code
if ($code -eq 0) {
  Set-Content -Path $done -Value 'ok'
} else {
  Set-Content -Path $failed -Value $code
}
exit $code
'@ | Set-Content -Path $runner -Encoding UTF8
Unregister-ScheduledTask -TaskName 'CrabboxImagePrep' -Confirm:$false -ErrorAction SilentlyContinue
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument ('-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "{0}"' -f $runner)
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Hours 2)
Register-ScheduledTask -TaskName 'CrabboxImagePrep' -Action $action -Principal $principal -Settings $settings -Force | Out-Null
Start-ScheduledTask -TaskName 'CrabboxImagePrep'
Write-Output 'crabbox-prep-started'
POWERSHELL
}

windows_prep_status_command() {
  cat <<'POWERSHELL'
$dir = 'C:\ProgramData\crabbox'
$log = Join-Path $dir 'image-prep.log'
$exitFile = Join-Path $dir 'image-prep.exit'
$done = Join-Path $dir 'image-prep.done'
$failed = Join-Path $dir 'image-prep.failed'
$step = Join-Path $dir 'image-prep.step'
$errorFile = Join-Path $dir 'image-prep.error'
if (Test-Path $done) {
  Write-Output 'crabbox-prep-done'
  if (Test-Path $exitFile) { Get-Content $exitFile }
  if (Test-Path $log) { Get-Content $log -Tail 80 }
  exit 0
}
if (Test-Path $failed) {
  Write-Output 'crabbox-prep-failed'
  if (Test-Path $step) { Write-Output ("failed during: {0}" -f (Get-Content $step -Raw).Trim()) }
  if (Test-Path $exitFile) {
    $code = (Get-Content $exitFile -Raw).Trim()
    if ($code -eq '-1' -or $code -eq '4294967295') {
      Write-Output "exit code: $code (0xFFFFFFFF); the prep PowerShell process returned a failure status, which alone does not identify the failing statement; see the step, exception and log below"
    } else {
      Write-Output "exit code: $code (prep PowerShell process)"
    }
  }
  if (Test-Path $errorFile) { Get-Content $errorFile }
  if (Test-Path $log) { Get-Content $log -Tail 200 }
  exit 0
}
$task = Get-ScheduledTask -TaskName 'CrabboxImagePrep' -ErrorAction SilentlyContinue
if ($task) {
  $info = Get-ScheduledTaskInfo -TaskName 'CrabboxImagePrep' -ErrorAction SilentlyContinue
  if ($info) {
    Write-Output ("crabbox-prep-state={0} result={1}" -f $task.State,$info.LastTaskResult)
  } else {
    Write-Output ("crabbox-prep-state={0}" -f $task.State)
  }
}
if (Test-Path $log) { Get-Content $log -Tail 30 }
Write-Output 'crabbox-prep-running'
exit 0
POWERSHELL
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
  [[ "$measured" == "1" ]] && printf '%s\0' --arch x86_64
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

warmup_handle_path() {
  printf '%s/%s.lease\n' "$warmup_handle_dir" "$1"
}

clear_warmup_handle() {
  rm -f "$(warmup_handle_path "$1")"
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

capture_selection() {
  local lease="$1" out="$2"
  local -a statuses
  local -a args=(admin leases --limit 100 --json)
  [[ -z "${CRABBOX_OWNER:-}" ]] || args+=(--owner "$CRABBOX_OWNER")
  [[ -z "${CRABBOX_ORG:-}" ]] || args+=(--org "$CRABBOX_ORG")
  # Do not log the owner/org filter or persist the complete administrative listing.
  "$CRABBOX_BIN" "${args[@]}" 2>"$out.error" |
    node "$ROOT/scripts/devtools-image-proof.mjs" select "$lease" >"$out" 2>>"$out.error" &&
    statuses=("${PIPESTATUS[@]}") || statuses=("${PIPESTATUS[@]}")
  [[ "${statuses[0]}" == "0" ]] || return "${statuses[0]}"
  return "${statuses[1]}"
}

assert_region() {
  local log="$1" selection="$2"
  [[ -n "$region" ]] || return 0
  local selected_region
  selected_region="$(sed -nE 's/^image selected id=[^[:space:]]+ source=[^[:space:]]+ kind=aws-ami region=([^[:space:]]+).*/\1/p' "$log")"
  # Capacity regions are additive on the coordinator, not a client-side allowlist.
  if [[ "$selected_region" != "$region" ]] ||
    ! jq -e --arg region "$region" '.provider == "aws" and .region == $region and .image.region == $region' "$selection" >/dev/null 2>&1; then
    printf 'mint requires image and lease region=%s (selected=%s); quota/capacity in %s; retry later or choose another region\n' \
      "$region" "${selected_region:-unknown}" "$region" >&2
    return 1
  fi
}

capacity_rejected() {
  node - "$@" <<'NODE'
const fs = require("node:fs");
const [log, handle, store, offset] = process.argv.slice(2);
if (handle && fs.existsSync(handle)) process.exit(1);
const text = fs.readFileSync(log, "utf8");
const hasLease = value => value && typeof value === "object" &&
  (value.leaseId || Object.values(value).some(hasLease));
let rejected = false;
for (const line of text.split("\n")) {
  try { if (hasLease(JSON.parse(line))) process.exit(1); } catch {}
  const match = line.match(/coordinator POST \/v1\/leases: http (\d+): (.*)$/);
  if (!match) continue;
  try {
    const body = JSON.parse(match[2]);
    // These three admission messages precede lease publication and provisioning.
    // Monthly spending limits share the error code and must fail immediately.
    if (match[1] !== "429" || body.error !== "cost_limit_exceeded" ||
        !/^(fleet active lease limit exceeded|active lease limit for (owner|org) exceeded): [0-9]+\/[0-9]+$/.test(body.message)) process.exit(1);
    rejected = true;
  } catch { process.exit(1); }
}
if (!rejected) process.exit(1);
if (store && fs.existsSync(store)) {
  const records = fs.readFileSync(store).subarray(Number(offset)).toString("utf8");
  for (const line of records.split("\n").filter(Boolean)) {
    const record = JSON.parse(line);
    if (hasLease(record) || record.timing?.exitCode === 0) process.exit(1);
  }
}
NODE
}

run_with_capacity_wait() {
  local label="$1" log="$2" handle="$3" store="$4"
  shift 4
  local deadline="" attempt=1 remaining delay store_size command_status
  local -a statuses
  while true; do
    store_size=0
    [[ -z "$store" || ! -f "$store" ]] || store_size="$(wc -c <"$store")"
    run_cmd "$@" 2>&1 | tee "$log" &&
      statuses=("${PIPESTATUS[@]}") || statuses=("${PIPESTATUS[@]}")
    command_status="${statuses[0]}"
    [[ "$command_status" != 0 ]] || return "${statuses[1]}"
    # Never retry an output failure or any evidence of an allocated lease.
    [[ "${statuses[1]}" == 0 ]] || return "$command_status"
    capacity_rejected "$log" "$handle" "$store" "$store_size" || return "$command_status"
    [[ -n "$deadline" ]] || deadline=$((SECONDS + $(duration_seconds "$capacity_wait")))
    remaining=$((deadline - SECONDS))
    if ((remaining <= 0)); then
      printf 'capacity wait budget exhausted for %s after %s; last rejection log=%s\n' "$label" "$capacity_wait" "$log" >&2
      return "$command_status"
    fi
    # Retain rejected evidence separately; only actual acquisitions are samples.
    cp "$log" "$log.capacity-$attempt" || return "$command_status"
    if [[ -n "$store" && -f "$store" ]]; then
      node -e '
const fs = require("node:fs");
const [store, offset, out] = process.argv.slice(1);
fs.writeFileSync(out, fs.readFileSync(store).subarray(Number(offset)));
fs.truncateSync(store, Number(offset));
' "$store" "$store_size" "$log.capacity-$attempt.timing.jsonl" || return "$command_status"
    fi
    delay=120
    ((remaining >= delay)) || delay="$remaining"
    printf 'active-lease capacity unavailable for %s; retrying in %ss (%ss remaining; attempt %s; log=%s.capacity-%s)\n' \
      "$label" "$delay" "$remaining" "$attempt" "$log" "$attempt" >&2
    sleep "$delay" || return "$command_status"
    attempt=$((attempt + 1))
  done
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
  if [[ "$label" == "source" ]]; then
    env_args+=(CRABBOX_AWS_STOCK_IMAGE="$stock_source" CRABBOX_AWS_ROOT_GB="${source_root_gb:-0}")
    [[ -z "$windows_source_os" ]] || env_args+=(CRABBOX_OS="$windows_source_os")
  fi
  [[ "$label" == "candidate" ]] && env_args+=(CRABBOX_AWS_AMI="$2")
  printf 'warming %s lease log=%s\n' "$label" "$log" >&2
  local warmup_status=0
  if [[ "${#env_args[@]}" -gt 0 ]]; then
    run_with_capacity_wait "$label" "$log" "$(warmup_handle_path "$label")" "" env "${env_args[@]}" "$CRABBOX_BIN" "${args[@]}" >&2 || warmup_status=$?
  else
    run_with_capacity_wait "$label" "$log" "$(warmup_handle_path "$label")" "" "$CRABBOX_BIN" "${args[@]}" >&2 || warmup_status=$?
  fi
  local lease
  lease="$(lease_from_log "$log" || true)"
  if [[ -n "$lease" ]]; then
    mkdir -p "$warmup_handle_dir"
    printf '%s\n' "$lease" >"$(warmup_handle_path "$label")"
  fi
  if [[ "$warmup_status" -ne 0 ]]; then
    if [[ -n "$lease" && "$keep_lease" != "1" ]]; then
      if run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$lease" >&2; then
        clear_warmup_handle "$label"
      fi
    fi
    return "$warmup_status"
  fi
  if [[ -z "$lease" ]]; then
    printf 'warmup did not return a lease id for %s\n' "$label" >&2
    return 1
  fi
  local selection_status=0
  local selection="$log.selection.json"
  [[ "$measured" != "1" ]] || selection="$measurement_dir/$label.selection.json"
  if [[ -n "$region" || "$measured" == "1" ]]; then
    capture_selection "$lease" "$selection" || selection_status=$?
    assert_region "$log" "$selection" || selection_status=1
    if [[ "$selection_status" != "0" ]]; then
      printf 'warmup region evidence failed for %s\n' "$label" >&2
      [[ ! -s "$selection.error" ]] || cat "$selection.error" >&2
      # A routed or unverified lease must not survive --keep-lease.
      if run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$lease" >&2; then
        clear_warmup_handle "$label"
      fi
      return "$selection_status"
    fi
  fi
  if [[ "$measured" == "1" ]]; then
    local phase="$label"
    [[ "$phase" == "source" && "$stock_source" != 1 ]] && phase=baseline
    if [[ "$selection_status" == "0" ]]; then
      node "$ROOT/scripts/devtools-image-proof.mjs" selection \
        "$measurement_dir/policy.json" "$selection" "$phase" "${ami_id:-}" \
        "$measurement_dir/baseline-1.selection.json" >&2 || selection_status=$?
    fi
    if [[ "$selection_status" != "0" ]]; then
      printf 'warmup selection evidence failed for %s\n' "$label" >&2
      [[ ! -s "$selection.error" ]] || cat "$selection.error" >&2
      if run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$lease" >&2; then
        clear_warmup_handle "$label"
      fi
      return "$selection_status"
    fi
  elif [[ "$label" == "source" && "$stock_source" == 1 ]]; then
    if ! grep -Eq 'image selected id=ami-[^[:space:]]+ source=stock' "$log"; then
      printf 'source warmup did not prove stock image selection; log=%s\n' "$log" >&2
      return 1
    fi
  elif [[ "$label" == "candidate" ]]; then
    assert_selected_image "$log" "$2" explicit || return 1
  elif [[ "$label" == "promoted" ]]; then
    assert_selected_image "$log" "$ami_id" promoted || return 1
  fi
  if [[ "$target" == "windows" ]]; then
    sleep "$windows_warmup_settle_seconds"
    if ! wait_windows_ssh_probe "$lease" "$windows_warmup_wait_timeout"; then
      if [[ "$keep_lease" != "1" ]] &&
        run_cmd "$CRABBOX_BIN" stop --provider aws --target windows "$lease" >&2; then
        clear_warmup_handle "$label"
      fi
      return 1
    fi
  fi
  printf '%s\n' "$lease"
}

measure_cohort() {
  local phase="$1"
  local sample log handle selection run_status recovery_status capture_status stop_status partial_status
  local store="$measurement_dir/$phase.jsonl"
  local -a env_args=(env -u CRABBOX_AWS_AMI CRABBOX_AWS_REGION="$region" AWS_REGION="$region")
  [[ "$phase" == "candidate" ]] && env_args+=(CRABBOX_AWS_AMI="$ami_id")
  for sample in 1 2 3; do
    log="$measurement_dir/$phase-$sample.log"
    handle="$measurement_dir/$phase-$sample.session.json"
    measurement_handle="$handle"
    selection="$measurement_dir/$phase-$sample.selection.json"
    # Fresh acquisition with a durable cleanup handle; this wrapper owns stop on every outcome.
    run_status=0
    run_with_capacity_wait "$phase-$sample" "$log" "$handle" "$store" \
      "${env_args[@]}" "$CRABBOX_BIN" run --provider aws --target linux \
      --arch x86_64 --class "$server_class" --type "$server_type" --market on-demand \
      --ttl "$ttl" --idle-timeout "$idle_timeout" --desktop --browser \
      --full-resync --no-hydrate --keep --stop-after never --lease-output "$handle" \
      --timing-record "$store" -- true || run_status=$?
    recovery_status=0
    capture_status=0
    stop_status=0
    partial_status=0
    # Even a failed run can allocate a lease. Recover only this attempted sample, not an older row.
    measurement_lease="$(node "$ROOT/scripts/devtools-image-proof.mjs" sample "$store" "$sample" "$handle" | jq -er .leaseId)" || recovery_status=$?
    if [[ -n "$measurement_lease" ]]; then
      if [[ "$run_status" == "0" ]]; then
        capture_selection "$measurement_lease" "$selection" || capture_status=$?
        if [[ "$capture_status" == "0" ]]; then
          assert_region "$log" "$selection" || capture_status=$?
        fi
        if [[ "$capture_status" != "0" ]]; then
          printf 'could not capture exact-lease measurement evidence\n' >&2
          [[ ! -s "$selection.error" ]] || cat "$selection.error" >&2
        fi
      fi
      # Stop observes provider cleanup completion, outside the original runner timing.
      run_cmd "$CRABBOX_BIN" stop --provider aws --target linux "$measurement_lease" || stop_status=$?
      if [[ "$stop_status" == "0" ]]; then
        measurement_handle=""
        if [[ "$run_status" == "0" ]]; then
          printf '%s\n' "$measurement_lease" >>"$measurement_dir/$phase-cleanup.ids"
        fi
        measurement_lease=""
      else
        printf 'measurement cleanup remains unconfirmed: %s (exit %s)\n' "$measurement_lease" "$stop_status" >&2
      fi
    else
      printf 'could not recover the attempted measurement lease; inspect %s\n' "$log" >&2
    fi
    if [[ -f "$store" ]]; then
      node "$ROOT/scripts/devtools-image-proof.mjs" partial \
        "$measurement_dir/policy.json" "$store" "$phase" \
        "$measurement_dir/$phase-cohort.json" || partial_status=$?
    fi
    [[ "$run_status" == "0" ]] || return "$run_status"
    [[ "$recovery_status" == "0" ]] || return "$recovery_status"
    [[ "$capture_status" == "0" ]] || return "$capture_status"
    [[ "$stop_status" == "0" ]] || return "$stop_status"
    [[ "$partial_status" == "0" ]] || return "$partial_status"
    node "$ROOT/scripts/devtools-image-proof.mjs" selection \
      "$measurement_dir/policy.json" "$selection" "$phase" "${ami_id:-}" \
      "$measurement_dir/baseline-1.selection.json"
  done
  run_json_tee "$measurement_dir/$phase-report.json" "$CRABBOX_BIN" bench report \
    --store "$store" --min-samples 3 --json
  if [[ "$phase" != "baseline" ]]; then
    run_json_tee "$measurement_dir/$phase-check.json" "$CRABBOX_BIN" bench check \
      --store "$store" --min-samples 3 --max-failures 0 \
      --max-p95-runner-total "${max_p95_runner_total_ms}ms" --json
  fi
  node "$ROOT/scripts/devtools-image-proof.mjs" cohort \
    "$measurement_dir/policy.json" "$measurement_dir" "$phase" "${ami_id:-}" \
    "$measurement_dir/$phase-cohort.json"
}

smoke_script() {
  if [[ "$target" == "windows" ]]; then
    smoke_script_path="$ROOT/scripts/devtools-image-smoke-windows.ps1"
  else
    smoke_script_path="$ROOT/scripts/devtools-image-smoke-linux.sh"
  fi
  smoke_script_value=""
  IFS= read -r -d '' smoke_script_value <"$smoke_script_path" || [[ -n "$smoke_script_value" ]] || return 1
  if [[ -n "$windows_source_build" ]]; then
    printf -v smoke_script_value '$ExpectedWindowsBuild = '\''%s'\''\n%s' "$windows_source_build" "$smoke_script_value"
  fi
  if [[ "$target" == "linux" ]]; then
    local expected_node_major="" archive_probe=":"
    if [[ "$linux_developer_builder" == "1" ]]; then
      [[ "$linux_node_major" == "24" ]] || expected_node_major="$linux_node_major"
      # Only the bundled builder declares archives. Freeze its selection, not guest environment.
      archive_probe="$(
        CRABBOX_LINUX_NODE_MAJOR="$linux_node_major" \
          bash -c 'source "$1"; node_pnpm_smoke_script' _ "$ROOT/scripts/install-linux-developer-tools.sh"
      )" || return $?
      local go_archive_probe bun_archive_probe rust_archive_probe uv_archive_probe
      go_archive_probe="$(
        bash -c 'source "$1"; go_smoke_script' _ "$ROOT/scripts/install-linux-developer-tools.sh"
      )" || return $?
      bun_archive_probe="$(
        bash -c 'source "$1"; bun_smoke_script' _ "$ROOT/scripts/install-linux-developer-tools.sh"
      )" || return $?
      rust_archive_probe="$(
        bash -c 'source "$1"; rust_smoke_script' _ "$ROOT/scripts/install-linux-developer-tools.sh"
      )" || return $?
      uv_archive_probe="$(
        bash -c 'source "$1"; uv_smoke_script' _ "$ROOT/scripts/install-linux-developer-tools.sh"
      )" || return $?
      archive_probe+=$'\n'"$go_archive_probe"$'\n'"$bun_archive_probe"$'\n'"$rust_archive_probe"$'\n'"$uv_archive_probe"
    fi
    printf -v smoke_script_value 'set -euo pipefail\nexpected_node_major=%q\nexpected_pnpm_version=%q\ndeveloper_archive_probe() {\n%s\n}\n%s' \
      "$expected_node_major" "$linux_pnpm_default" "$archive_probe" "$smoke_script_value"
  fi
}

smoke() {
  local lease="$1"
  verify_linux_image_readiness "$lease" || return $?
  smoke_script || return $?
  if [[ "$target" == linux ]]; then
    # Keep the smoke's EXIT cleanup outside the login shell: logout hooks can
    # replace its exit status, including turning a successful proof into failure.
    printf '%s' "$smoke_script_value" |
      run_cmd "$CRABBOX_BIN" run --provider aws --target "$target" --id "$lease" --no-sync --script-stdin
  else
    run_cmd "$CRABBOX_BIN" run --provider aws --target "$target" --id "$lease" --no-sync --shell -- "$smoke_script_value"
  fi
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
  if [[ "$linux_developer_builder" == "1" ]]; then
    # Prep and every smoke must use the same declared overrides, not ambient guest state.
    run_cmd env CRABBOX_LINUX_NODE_MAJOR="$linux_node_major" CRABBOX_LINUX_PNPM_VERSION="$linux_pnpm_version" \
      "$CRABBOX_BIN" run --provider aws --target "$target" --id "$lease" --no-sync \
      --allow-env CRABBOX_LINUX_NODE_MAJOR,CRABBOX_LINUX_PNPM_VERSION --script "$prep_script" || return $?
    # sudo preparation seeds root's Corepack state, not the lease user's default.
    local pnpm_command pnpm_capture
    printf -v pnpm_command 'set -euo pipefail\n[[ "$(id -u)" -ne 0 ]] || { echo "pnpm preparation requires a nonroot user" >&2; exit 1; }\ncd /\ncorepack prepare %q --activate >&2\n' "pnpm@$linux_pnpm_version"
    pnpm_command+='version="$(COREPACK_ENABLE_NETWORK=0 pnpm --version)"
corepack_version="$(COREPACK_ENABLE_NETWORK=0 corepack pnpm --version)"
[[ "$version" == "$corepack_version" ]] || { echo "ordinary pnpm does not match Corepack" >&2; exit 1; }
printf "%s\n" "$version"'
    pnpm_capture="$(mktemp "$log_dir/.image-mint-pnpm-${log_id}.XXXXXX")" || return $?
    run_cmd "$CRABBOX_BIN" run --provider aws --target linux --id "$lease" --no-sync \
      --capture-stdout "$pnpm_capture" --shell -- "$pnpm_command" || return $?
    # Bound the read and reject extra output before rendering it into later smokes.
    linux_pnpm_default="$(node - "$pnpm_capture" <<'NODE'
const fs = require("node:fs");
const fd = fs.openSync(process.argv[2], "r");
const bytes = Buffer.alloc(130);
const length = fs.readSync(fd, bytes, 0, bytes.length, 0);
fs.closeSync(fd);
const version = bytes.subarray(0, length).toString("latin1");
if (!/^[!-~]{1,128}\n$/.test(version)) {
  console.error("invalid runtime-user pnpm version: expected one bounded version line");
  process.exit(1);
}
process.stdout.write(version.slice(0, -1));
NODE
    )" || return $?
  else
    run_cmd "$CRABBOX_BIN" run --provider aws --target "$target" --id "$lease" --no-sync --script "$prep_script"
  fi
}

stage_linux_readiness_producer() {
  local lease="$1"
  [[ "$target" == "linux" ]] || return 0
  run_cmd "$CRABBOX_BIN" run --provider aws --target linux --id "$lease" --no-sync \
    --script "$ROOT/scripts/linux-readiness.generated.sh" -- --install
}

verify_linux_image_readiness() {
  local lease="$1"
  [[ "$target" == "linux" ]] || return 0
  run_cmd "$CRABBOX_BIN" run --provider aws --target linux --id "$lease" --no-sync \
    --script "$ROOT/scripts/linux-readiness.generated.sh" -- --verify linux-builder
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

cat >&2 <<EOF
AWS devtools image mint
  target: $target
  image:  $image_name
  region: ${region:-auto}
  class:  $server_class
  type:   ${server_type:-auto}
  source: stock=$stock_source root_gb=${source_root_gb:-auto} windows_os=${windows_source_os:-normal}
  prep:   $prep_script
  proof:  desktop=$desktop browser=$browser promote=$promote
  fsr:    enabled=$fast_snapshot_restore azs=${fast_snapshot_restore_azs:-auto}
  paid:   run=$run keep_lease=$keep_lease
EOF

if [[ "$measured" == "1" ]]; then
  printf 'measured campaign: 12 planned leases (3 baseline + 3 candidate + 3 promoted measurements + 3 lifecycle leases)\n' >&2
  printf 'provider retries may add launch attempts; no hard attempt or dollar cap is enforced by this wrapper\n' >&2
  printf 'absolute p95 runner threshold: %sms for candidate and promoted cohorts; per-lease TTL: %s\n' \
    "$max_p95_runner_total_ms" "$ttl" >&2
fi

if [[ "$run" != "1" ]]; then
  printf 'dry plan only; add --run to create source/candidate leases and AMIs.\n'
  trap - EXIT
  exit 0
fi

if [[ "$measured" == "1" ]]; then
  effective_config="$(env CRABBOX_AWS_REGION="$region" AWS_REGION="$region" \
    "$CRABBOX_BIN" config show --provider aws --json)"
  if ! jq -e --arg region "$region" \
    '.provider == "aws" and .aws.region == $region and .aws.ami == "" and
     (.coordinator | type == "string" and length > 0) and .brokerMode == "managed" and
     (.brokerAuth == "configured" or .brokerAuth == "command") and .brokerAdminAuth == "configured"' <<<"$effective_config" >/dev/null; then
    printf 'measured publication requires a managed admin coordinator, the requested region, and no effective aws.ami override\n' >&2
    exit 2
  fi
  outcome_stage="baseline"
  measure_cohort baseline
fi

outcome_stage="source_prepare"
source_lease="$(warmup source)"
jq -n --argjson stock "$stock_source" --argjson root "${source_root_gb:-0}" --arg lease "$source_lease" \
  '{stockSource: ($stock == 1), rootGB: $root, leaseId: $lease}' \
  >"$log_dir/image-mint-${log_image_name}-${log_id}-source.json"
stage_linux_readiness_producer "$source_lease"
run_prep "$source_lease"
reboot_windows_source_if_needed "$source_lease"
smoke "$source_lease"

image_env=(env)
[[ -n "$region" ]] && image_env+=(CRABBOX_AWS_REGION="$region" AWS_REGION="$region")
outcome_stage="candidate_create"
image_status=0
image_output="$("${image_env[@]}" "$CRABBOX_BIN" checkpoint create \
  --provider aws --target "$target" --id "$source_lease" --name "$image_name" \
  --mode native --strategy image --no-reboot=false --wait --wait-timeout "$wait_timeout")" || image_status=$?
printf '%s\n' "$image_output"
candidate_identity="$(printf '%s\n' "$image_output" | sed -nE 's/^checkpoint created id=(chk_[a-zA-Z0-9]+) kind=aws-ami resource=(ami-[a-zA-Z0-9]+) state=[^[:space:]]+ region=([a-z0-9-]+) .*/\1 \2 \3/p')"
if [[ -z "$candidate_identity" || "$candidate_identity" == *$'\n'* ]]; then
  printf 'checkpoint create did not return one exact AMI/checkpoint/region identity; inspect capture output before cleanup\n' >&2
  [[ "$image_status" != "0" ]] || image_status=1
  exit "$image_status"
fi
read -r candidate_checkpoint ami_id candidate_region <<<"$candidate_identity"
[[ "$image_status" == "0" ]] || exit "$image_status"
if [[ -n "$region" && "$candidate_region" != "$region" ]]; then
  printf 'captured image region=%s differs from requested region=%s; refusing candidate launch\n' "$candidate_region" "$region" >&2
  exit 1
fi

if [[ "$keep_lease" != "1" ]]; then
  run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$source_lease"
  clear_warmup_handle source
  source_lease=""
fi

outcome_stage="candidate_smoke"
candidate_lease="$(warmup candidate "$ami_id")"
smoke "$candidate_lease"
printf 'candidate AMI smoke passed: %s\n' "$ami_id"

if [[ "$promote" != "1" ]]; then
  exit 0
fi

if [[ "$keep_lease" != "1" ]]; then
  run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$candidate_lease"
  clear_warmup_handle candidate
  candidate_lease=""
fi

if [[ "$measured" == "1" ]]; then
  outcome_stage="candidate_measure"
  measure_cohort candidate
fi

outcome_stage="promotion"
promote_args=(image promote --target "$target" --json --expected-current-image capture)
[[ -n "$region" ]] && promote_args+=(--region "$region")
[[ "$target" == "linux" && -n "${CRABBOX_OS:-}" ]] && promote_args+=(--os "$CRABBOX_OS")
if [[ "$fast_snapshot_restore" == "1" ]]; then
  promote_args+=(--fast-snapshot-restore)
  IFS=',' read -r -a fsr_az_values <<<"$fast_snapshot_restore_azs"
  for fsr_az in "${fsr_az_values[@]}"; do
    [[ -n "$fsr_az" ]] || continue
    promote_args+=(--fsr-az "$fsr_az")
  done
fi
promote_args+=("$ami_id")
promotion_log="$(mktemp "$log_dir/image-mint-${log_image_name}-promotion-${log_id}.json.XXXXXX")"
rollback_pending=1
run_json_tee "$promotion_log" "$CRABBOX_BIN" "${promote_args[@]}"
promotion_confirmed=1
jq -e '.image.id and .image.revision and (.previous.state == "present" or .previous.state == "absent") and (.previous.aliases | length > 0)' "$promotion_log" >/dev/null
if [[ "$measured" == "1" ]]; then
  node "$ROOT/scripts/devtools-image-proof.mjs" receipt \
    "$measurement_dir/policy.json" "$measurement_dir/baseline-1.selection.json" "$promotion_log" "$ami_id"
fi

outcome_stage="promoted_smoke"
promoted_lease="$(warmup promoted)"
smoke "$promoted_lease"
if [[ "$measured" == "1" ]]; then
  run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$promoted_lease"
  clear_warmup_handle promoted
  promoted_lease=""
  outcome_stage="promoted_measure"
  measure_cohort promoted
  outcome_stage="proof"
  node "$ROOT/scripts/devtools-image-proof.mjs" receipt \
    "$measurement_dir/policy.json" "$measurement_dir/baseline-1.selection.json" "$promotion_log" "$ami_id"
fi
outcome_stage="complete"
printf 'promoted image selection proved: %s\n' "$ami_id"
printf 'promoted %s developer image passed: %s\n' "$target" "$ami_id"
