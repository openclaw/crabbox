#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CRABBOX_BIN="${CRABBOX_BIN:-$ROOT/bin/crabbox}"
target="${CRABBOX_IMAGE_TARGET:-linux}"
region="${CRABBOX_IMAGE_REGION:-${CRABBOX_AWS_REGION:-}}"
server_type="${CRABBOX_IMAGE_TYPE:-}"
image_name="${CRABBOX_IMAGE_NAME:-}"
ttl="${CRABBOX_IMAGE_TTL:-2h}"
idle_timeout="${CRABBOX_IMAGE_IDLE_TIMEOUT:-30m}"
wait_timeout="${CRABBOX_IMAGE_WAIT_TIMEOUT:-60m}"
reboot_wait_timeout="${CRABBOX_IMAGE_REBOOT_WAIT_TIMEOUT:-25m}"
reboot_settle_seconds="${CRABBOX_IMAGE_REBOOT_SETTLE_SECONDS:-30}"
run="${CRABBOX_IMAGE_RUN:-0}"
promote="${CRABBOX_IMAGE_PROMOTE:-1}"
keep_lease="${CRABBOX_IMAGE_KEEP_LEASE:-0}"
desktop="${CRABBOX_IMAGE_DESKTOP:-1}"
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
  --type TYPE           AWS instance type
  --name NAME           image name
  --run                 allow paid lease/image work
  --no-promote          smoke candidate only
  --keep-lease          keep proof leases alive
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
  CRABBOX_IMAGE_WAIT_TIMEOUT
  CRABBOX_IMAGE_REBOOT_WAIT_TIMEOUT
  CRABBOX_IMAGE_REBOOT_SETTLE_SECONDS
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
    --keep-lease)
      keep_lease=1
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

if [[ -z "$image_name" ]]; then
  image_name="crabbox-${target}-devtools-$(date -u +%Y%m%d-%H%M)"
fi
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

warmup_args() {
  printf '%s\0' warmup --provider aws --target "$target" --market on-demand --ttl "$ttl" --idle-timeout "$idle_timeout" --timing-json
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
  local log=".crabbox/image-mint-${image_name}-${label}.log"
  mkdir -p .crabbox
  local -a args
  while IFS= read -r -d '' arg; do args+=("$arg"); done < <(warmup_args)
  local -a env_args=()
  [[ -n "$region" ]] && env_args+=(CRABBOX_AWS_REGION="$region" AWS_REGION="$region")
  [[ "$label" == "candidate" ]] && env_args+=(CRABBOX_AWS_AMI="$2")
  printf 'warming %s lease\n' "$label" >&2
  if [[ "${#env_args[@]}" -gt 0 ]]; then
    run_cmd env "${env_args[@]}" "$CRABBOX_BIN" "${args[@]}" 2>&1 | tee "$log" >&2
  else
    run_cmd "$CRABBOX_BIN" "${args[@]}" 2>&1 | tee "$log" >&2
  fi
  lease_from_log "$log"
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
npm --version
corepack --version
pnpm --version
docker --version
docker version
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
docker version
docker compose version
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

windows_reboot_required() {
  local lease="$1"
  local output
  output="$("$CRABBOX_BIN" run --provider aws --target windows --id "$lease" --no-sync --shell -- "if (Test-Path '$windows_reboot_marker') { Write-Output 'crabbox-reboot-required' } else { Write-Output 'crabbox-reboot-not-required' }")"
  printf '%s\n' "$output"
  grep -q 'crabbox-reboot-required' <<<"$output"
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
  run_cmd "$CRABBOX_BIN" status --provider aws --target windows --id "$lease" --wait --wait-timeout "$reboot_wait_timeout"
  run_cmd "$CRABBOX_BIN" run --provider aws --target windows --id "$lease" --no-sync --script "$prep_script"
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
  type:   ${server_type:-auto}
  prep:   $prep_script
  proof:  desktop=$desktop browser=$browser promote=$promote
  paid:   run=$run keep_lease=$keep_lease
EOF

if [[ "$run" != "1" ]]; then
  printf 'dry plan only; add --run to create source/candidate leases and AMIs.\n'
  exit 0
fi

source_lease="$(warmup source)"
run_cmd "$CRABBOX_BIN" run --provider aws --target "$target" --id "$source_lease" --no-sync --script "$prep_script"
reboot_windows_source_if_needed "$source_lease"
smoke "$source_lease"

image_json="$("$CRABBOX_BIN" image create --id "$source_lease" --name "$image_name" --no-reboot=false --wait --wait-timeout "$wait_timeout" --json)"
printf '%s\n' "$image_json" | jq .
ami_id="$(printf '%s\n' "$image_json" | jq -r '.id // .image.id // empty')"
if [[ -z "$ami_id" ]]; then
  printf 'image create did not return an AMI id\n' >&2
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

promote_args=(image promote "$ami_id" --target "$target" --json)
[[ -n "$region" ]] && promote_args+=(--region "$region")
run_cmd "$CRABBOX_BIN" "${promote_args[@]}"

if [[ "$keep_lease" != "1" ]]; then
  run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$candidate_lease"
  candidate_lease=""
fi

promoted_lease="$(warmup promoted)"
smoke "$promoted_lease"
printf 'promoted %s developer image passed: %s\n' "$target" "$ami_id"
