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
promote="${CRABBOX_IMAGE_PROMOTE:-1}"
keep_lease="${CRABBOX_IMAGE_KEEP_LEASE:-0}"
desktop="${CRABBOX_IMAGE_DESKTOP:-auto}"
browser="${CRABBOX_IMAGE_BROWSER:-auto}"
windows_mode="${CRABBOX_WINDOWS_MODE:-normal}"
prep_script="${CRABBOX_IMAGE_PREP_SCRIPT:-}"
linux_node_major="${CRABBOX_LINUX_NODE_MAJOR:-24}"
linux_pnpm_version="${CRABBOX_LINUX_PNPM_VERSION:-11.1.0}"
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
  CRABBOX_IMAGE_RUN
  CRABBOX_IMAGE_PROMOTE
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
    "$desktop" "$browser" "$promote" "$keep_lease" "$fast_snapshot_restore" "$ttl" "$idle_timeout")"
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
  local lease handle seen_leases="|"
  local -a cleanup_leases=()
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
Remove-Item -Force $log,$exitFile,$done,$failed -ErrorAction SilentlyContinue
@'
$dir = 'C:\ProgramData\crabbox'
$script = Join-Path $dir 'image-prep.ps1'
$log = Join-Path $dir 'image-prep.log'
$exitFile = Join-Path $dir 'image-prep.exit'
$done = Join-Path $dir 'image-prep.done'
$failed = Join-Path $dir 'image-prep.failed'
$ErrorActionPreference = 'Continue'
Remove-Item -Force $exitFile,$done,$failed -ErrorAction SilentlyContinue
& powershell -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $script *>&1 | Tee-Object -FilePath $log
$code = $LASTEXITCODE
if ($null -eq $code) { $code = 0 }
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
if (Test-Path $done) {
  Write-Output 'crabbox-prep-done'
  if (Test-Path $exitFile) { Get-Content $exitFile }
  if (Test-Path $log) { Get-Content $log -Tail 80 }
  exit 0
}
if (Test-Path $failed) {
  Write-Output 'crabbox-prep-failed'
  if (Test-Path $exitFile) { Get-Content $exitFile }
  if (Test-Path $log) { Get-Content $log -Tail 120 }
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
  # Do not log the owner/org filter or persist the complete administrative listing.
  "$CRABBOX_BIN" admin leases --owner "$CRABBOX_OWNER" --org "$CRABBOX_ORG" --limit 100 --json 2>"$out.error" |
    node "$ROOT/scripts/devtools-image-proof.mjs" select "$lease" >"$out" 2>>"$out.error" &&
    statuses=("${PIPESTATUS[@]}") || statuses=("${PIPESTATUS[@]}")
  [[ "${statuses[0]}" == "0" ]] || return "${statuses[0]}"
  return "${statuses[1]}"
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
  if [[ "$measured" == "1" ]]; then
    local phase="$label"
    local selection_status=0
    local selection="$measurement_dir/$label.selection.json"
    [[ "$phase" == "source" ]] && phase=baseline
    capture_selection "$lease" "$selection" || selection_status=$?
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
  local -a pipeline_status
  local -a env_args=(env -u CRABBOX_AWS_AMI CRABBOX_AWS_REGION="$region" AWS_REGION="$region")
  [[ "$phase" == "candidate" ]] && env_args+=(CRABBOX_AWS_AMI="$ami_id")
  for sample in 1 2 3; do
    log="$measurement_dir/$phase-$sample.log"
    handle="$measurement_dir/$phase-$sample.session.json"
    measurement_handle="$handle"
    selection="$measurement_dir/$phase-$sample.selection.json"
    # Fresh acquisition with a durable cleanup handle; this wrapper owns stop on every outcome.
    run_cmd "${env_args[@]}" "$CRABBOX_BIN" run --provider aws --target linux \
      --arch x86_64 --class "$server_class" --type "$server_type" --market on-demand \
      --ttl "$ttl" --idle-timeout "$idle_timeout" --desktop --browser \
      --full-resync --no-hydrate --keep --stop-after never --lease-output "$handle" \
      --timing-record "$store" -- true 2>&1 | tee "$log" &&
      pipeline_status=("${PIPESTATUS[@]}") || pipeline_status=("${PIPESTATUS[@]}")
    run_status="${pipeline_status[0]}"
    [[ "$run_status" != "0" ]] || run_status="${pipeline_status[1]}"
    recovery_status=0
    capture_status=0
    stop_status=0
    partial_status=0
    # Even a failed run can allocate a lease. Recover only this attempted sample, not an older row.
    measurement_lease="$(node "$ROOT/scripts/devtools-image-proof.mjs" sample "$store" "$sample" "$handle" | jq -er .leaseId)" || recovery_status=$?
    if [[ -n "$measurement_lease" ]]; then
      if [[ "$run_status" == "0" ]]; then
        capture_selection "$measurement_lease" "$selection" || capture_status=$?
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
  if [[ "$target" == "linux" ]]; then
    local expected_node_major="" archive_probe=":"
    if [[ "$linux_developer_builder" == "1" ]]; then
      [[ "$linux_node_major" == "24" ]] || expected_node_major="$linux_node_major"
      # Only the bundled builder declares archives. Freeze its selection, not guest environment.
      archive_probe="$(
        CRABBOX_LINUX_NODE_MAJOR="$linux_node_major" \
          bash -c 'source "$1"; node_pnpm_smoke_script' _ "$ROOT/scripts/install-linux-developer-tools.sh"
      )" || return $?
      local go_archive_probe
      go_archive_probe="$(
        bash -c 'source "$1"; go_smoke_script' _ "$ROOT/scripts/install-linux-developer-tools.sh"
      )" || return $?
      archive_probe+=$'\n'"$go_archive_probe"
    fi
    printf -v smoke_script_value 'set -euo pipefail\nexpected_node_major=%q\ndeveloper_archive_probe() {\n%s\n}\n%s' \
      "$expected_node_major" "$archive_probe" "$smoke_script_value"
  fi
}

smoke() {
  local lease="$1"
  smoke_script || return $?
  run_cmd "$CRABBOX_BIN" run --provider aws --target "$target" --id "$lease" --no-sync --shell -- "$smoke_script_value"
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
      --allow-env CRABBOX_LINUX_NODE_MAJOR,CRABBOX_LINUX_PNPM_VERSION --script "$prep_script"
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
    --shell -- /usr/local/libexec/crabbox/linux-readiness.generated.sh
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
stage_linux_readiness_producer "$source_lease"
run_prep "$source_lease"
reboot_windows_source_if_needed "$source_lease"
verify_linux_image_readiness "$source_lease"
smoke "$source_lease"

image_env=(env)
[[ -n "$region" ]] && image_env+=(CRABBOX_AWS_REGION="$region" AWS_REGION="$region")
outcome_stage="candidate_create"
image_output="$("${image_env[@]}" "$CRABBOX_BIN" checkpoint create \
  --provider aws --target "$target" --id "$source_lease" --name "$image_name" \
  --mode native --strategy image --no-reboot=false --wait --wait-timeout "$wait_timeout")"
printf '%s\n' "$image_output"
ami_id="$(printf '%s\n' "$image_output" | sed -nE 's/.* resource=(ami-[^[:space:]]+).*/\1/p' | tail -n 1)"
if [[ -z "$ami_id" ]]; then
  printf 'checkpoint create did not return an AMI id\n' >&2
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
