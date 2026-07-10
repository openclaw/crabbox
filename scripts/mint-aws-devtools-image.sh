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

source_lease=""
candidate_lease=""
promoted_lease=""

cleanup() {
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
  if [[ "$target" == "windows" ]]; then
    sleep "$windows_warmup_settle_seconds"
    if ! wait_windows_ssh_probe "$lease" "$windows_warmup_wait_timeout"; then
      [[ "$keep_lease" == "1" ]] || run_cmd "$CRABBOX_BIN" stop --provider aws --target windows "$lease" >&2 || true
      return 1
    fi
  fi
  printf '%s\n' "$lease"
}

smoke_script() {
  if [[ "$target" == "windows" ]]; then
    cat <<'POWERSHELL'
$ErrorActionPreference = "Stop"
Write-Output "devtools-smoke-ok"
Get-ComputerInfo | Select-Object OsName, OsVersion, OsBuildNumber | Format-List
git --version
gh --version | Select-Object -First 1
jq --version
rg --version | Select-Object -First 1
fd --version
python --version
node --version
$nodeMajor = [int](node -p "process.versions.node.split('.')[0]")
if ($nodeMajor -lt 24) { throw "Node.js 24 or newer is required, found major $nodeMajor" }
npm --version
corepack --version
pnpm --version
docker --version
docker version
docker image inspect mcr.microsoft.com/windows/servercore:ltsc2022 | Out-Null
POWERSHELL
  else
    cat <<'SHELL'
set -euo pipefail
echo devtools-smoke-ok
uname -a
command -v git
command -v gh
command -v jq
command -v rg
command -v fd
command -v python3
command -v node
command -v npm
command -v corepack
command -v pnpm
command -v docker
node --version
node -e 'if (Number(process.versions.node.split(".")[0]) < 24) throw new Error(`Node.js 24 or newer is required, found ${process.version}`)'
docker_group_member() {
  if id -nG 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
    return 0
  fi
  local current_user docker_entry docker_members member
  current_user="$(whoami)"
  docker_entry="$(getent group docker 2>/dev/null || true)"
  [[ -n "$docker_entry" ]] || return 1
  docker_members="${docker_entry#*:*:*:}"
  local IFS=','
  local -a docker_member_list
  read -ra docker_member_list <<<"$docker_members"
  for member in "${docker_member_list[@]}"; do
    [[ "$member" == "$current_user" ]] && return 0
  done
  return 1
}
docker_probe='docker version && docker compose version && docker image inspect hello-world ubuntu:24.04 node:24-bookworm >/dev/null'
if ! sh -c "$docker_probe"; then
  if command -v sg >/dev/null 2>&1 && docker_group_member; then
    sg docker -c "$docker_probe"
  else
    sudo sh -c "$docker_probe"
  fi
fi
test -d /var/cache/crabbox/pnpm
SHELL
  fi
}

smoke() {
  local lease="$1"
  local script
  script="$(smoke_script)"
  run_cmd "$CRABBOX_BIN" run --provider aws --target "$target" --id "$lease" --no-sync --shell -- "$script"
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

if [[ "$run" != "1" ]]; then
  printf 'dry plan only; add --run to create source/candidate leases and AMIs.\n'
  exit 0
fi

source_lease="$(warmup source)"
run_prep "$source_lease"
reboot_windows_source_if_needed "$source_lease"
smoke "$source_lease"

image_env=(env)
[[ -n "$region" ]] && image_env+=(CRABBOX_AWS_REGION="$region" AWS_REGION="$region")
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
  source_lease=""
fi

candidate_lease="$(warmup candidate "$ami_id")"
smoke "$candidate_lease"
printf 'candidate AMI smoke passed: %s\n' "$ami_id"

if [[ "$promote" != "1" ]]; then
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

if [[ "$keep_lease" != "1" ]]; then
  run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$candidate_lease"
  candidate_lease=""
fi

promoted_lease="$(warmup promoted)"
smoke "$promoted_lease"
printf 'promoted %s developer image passed: %s\n' "$target" "$ami_id"
