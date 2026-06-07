#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cb="${CRABBOX_BIN:-$root/bin/crabbox}"
repo="${CRABBOX_LIVE_REPO:-$root}"
skip_missing="${CRABBOX_LIVE_SKIP_MISSING:-0}"
sync_checkout="${CRABBOX_PROVIDER_E2E_SYNC:-0}"
idle_timeout="${CRABBOX_PROVIDER_E2E_IDLE_TIMEOUT:-5m}"
ttl="${CRABBOX_PROVIDER_E2E_TTL:-15m}"
wait_timeout="${CRABBOX_PROVIDER_E2E_WAIT_TIMEOUT:-180s}"

usage() {
  cat >&2 <<'USAGE'
Usage: CRABBOX_LIVE=1 scripts/live-provider-e2e.sh <provider>

Runs one live provider smoke. Set CRABBOX_LIVE_SKIP_MISSING=1 to skip a
provider when its required GitHub Actions secrets or runner tools are absent.
USAGE
}

provider="${1:-}"
if [[ -z "$provider" || "$provider" == "-h" || "$provider" == "--help" ]]; then
  usage
  exit 2
fi

normalize_provider() {
  case "$1" in
    blacksmith) printf 'blacksmith-testbox' ;;
    cf) printf 'cloudflare' ;;
    container | docker) printf 'local-container' ;;
    namespace) printf 'namespace-devbox' ;;
    static | static-ssh) printf 'ssh' ;;
    *) printf '%s' "$1" ;;
  esac
}

provider="$(normalize_provider "$provider")"

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  echo "set CRABBOX_LIVE=1 to run live provider E2E tests" >&2
  exit 2
fi
if [[ ! -x "$cb" ]]; then
  echo "missing crabbox binary: $cb" >&2
  echo "build first: go build -trimpath -o bin/crabbox ./cmd/crabbox" >&2
  exit 2
fi

run_in_repo() {
  (cd "$repo" && "$@")
}

skip_or_fail() {
  local reason="$1"
  if [[ "$skip_missing" == "1" ]]; then
    echo "skip provider=$provider reason=$reason"
    exit 0
  fi
  echo "missing provider=$provider requirement: $reason" >&2
  exit 2
}

unknown_provider() {
	echo "unknown provider=$provider" >&2
	exit 2
}

need_tool() {
  command -v "$1" >/dev/null 2>&1 || skip_or_fail "tool $1 on PATH"
}

need_env() {
  local name="$1"
  [[ -n "${!name:-}" ]] || skip_or_fail "env $name"
}

need_any_env() {
  local label="$1"
  shift
  local name
  for name in "$@"; do
    if [[ -n "${!name:-}" ]]; then
      return 0
    fi
  done
  skip_or_fail "$label (${*})"
}

need_env_pair() {
  local left="$1"
  local right="$2"
  [[ -n "${!left:-}" && -n "${!right:-}" ]] || skip_or_fail "env $left and $right"
}

coordinator_ready() {
  [[ -n "${CRABBOX_COORDINATOR:-}" && -n "${CRABBOX_COORDINATOR_TOKEN:-}" ]]
}

managed_provider() {
  case "$provider" in
    aws | azure | gcp | hetzner) return 0 ;;
    *) return 1 ;;
  esac
}

direct_or_coordinator_env() {
  if coordinator_ready; then
    return 0
  fi
  case "$provider" in
    aws)
      [[ -n "${AWS_ACCESS_KEY_ID:-}" || -n "${AWS_PROFILE:-}" ]]
      ;;
    azure)
      [[ -n "${AZURE_SUBSCRIPTION_ID:-}" && -n "${AZURE_TENANT_ID:-}" && -n "${AZURE_CLIENT_ID:-}" && -n "${AZURE_CLIENT_SECRET:-}" ]]
      ;;
    gcp)
      [[ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" || -n "${GCP_PRIVATE_KEY:-}" ]]
      ;;
    hetzner)
      [[ -n "${HCLOUD_TOKEN:-}" || -n "${HETZNER_TOKEN:-}" ]]
      ;;
    *)
      return 1
      ;;
  esac
}

coordinator_login() {
  if ! coordinator_ready; then
    return 0
  fi
  printf '%s' "$CRABBOX_COORDINATOR_TOKEN" | "$cb" login \
    --url "$CRABBOX_COORDINATOR" \
    --provider "$provider" \
    --token-stdin \
    --json >/dev/null
}

extract_lease() {
  sed -n '
    s/^leased \([^ ]*\).*/\1/p
    s/.* lease=\([^ ]*\).*/\1/p
  ' | head -1
}

extract_slug() {
  sed -n 's/.* slug=\([^ ]*\).*/\1/p' | head -1
}

provider_token() {
  printf '%s' "$provider" | tr -c '[:alnum:]' '-'
}

smoke_command() {
  local token
  token="$(provider_token)"
  if [[ "${CRABBOX_TARGET:-}" == "windows" && "${CRABBOX_WINDOWS_MODE:-}" != "wsl2" ]]; then
    printf "Write-Output 'crabbox-%s-e2e-ok'; Get-Location; [System.Environment]::OSVersion.Platform" "$token"
    return 0
  fi
  printf 'set -eu; echo crabbox-%s-e2e-ok; pwd; uname -s || true' "$token"
}

ssh_lease_smoke() {
  local out lease slug id
  out="$(run_in_repo "$cb" warmup --provider "$provider" --ttl "$ttl" --idle-timeout "$idle_timeout" "$@" 2>&1)"
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
  slug="$(printf '%s\n' "$out" | extract_slug)"
  id="${slug:-$lease}"
  [[ -n "$id" ]] || {
    echo "could not parse lease id or slug from warmup output" >&2
    return 3
  }

  cleanup() {
    run_in_repo "$cb" stop --provider "$provider" "$id" || true
  }
  trap cleanup RETURN

  run_in_repo "$cb" status --provider "$provider" --id "$id" --wait --wait-timeout "$wait_timeout"
  run_in_repo "$cb" inspect --provider "$provider" --id "$id" --json || true
  run_in_repo "$cb" ssh --provider "$provider" --id "$id"
  if [[ "$sync_checkout" == "1" ]]; then
    run_in_repo "$cb" run --provider "$provider" --id "$id" --timing-json --shell -- "$(smoke_command)"
  else
    run_in_repo "$cb" run --provider "$provider" --id "$id" --no-sync --timing-json --shell -- "$(smoke_command)"
  fi
  run_in_repo "$cb" list --provider "$provider" --json || true
  run_in_repo "$cb" stop --provider "$provider" "$id"
  trap - RETURN
}

delegated_run_smoke() {
  if [[ "$sync_checkout" == "1" ]]; then
    run_in_repo "$cb" run --provider "$provider" --timing-json --shell -- "$(smoke_command)"
  else
    run_in_repo "$cb" run --provider "$provider" --no-sync --timing-json --shell -- "$(smoke_command)"
  fi
  run_in_repo "$cb" list --provider "$provider" --json || true
}

blacksmith_smoke() {
  need_tool blacksmith
  need_env CRABBOX_BLACKSMITH_ORG
  need_env CRABBOX_BLACKSMITH_WORKFLOW
  run_in_repo "$cb" run \
    --provider blacksmith-testbox \
    --blacksmith-org "$CRABBOX_BLACKSMITH_ORG" \
    --blacksmith-workflow "$CRABBOX_BLACKSMITH_WORKFLOW" \
    --blacksmith-job "${CRABBOX_BLACKSMITH_JOB:-check}" \
    --blacksmith-ref "${CRABBOX_BLACKSMITH_REF:-main}" \
    --idle-timeout "$idle_timeout" \
    --timing-json \
    --shell -- "$(smoke_command)"
  run_in_repo "$cb" list --provider blacksmith-testbox --json || true
}

daytona_smoke() {
  need_any_env "Daytona API auth" DAYTONA_API_KEY DAYTONA_JWT_TOKEN
  need_any_env "Daytona snapshot" CRABBOX_DAYTONA_SNAPSHOT DAYTONA_SNAPSHOT
  delegated_run_smoke
}

railway_smoke() {
  need_any_env "Railway API token" CRABBOX_RAILWAY_API_TOKEN RAILWAY_API_TOKEN
  need_env CRABBOX_RAILWAY_SERVICE_ID
  need_env CRABBOX_RAILWAY_PROJECT_ID
  need_env CRABBOX_RAILWAY_ENVIRONMENT_ID
  run_in_repo "$cb" run \
    --provider railway \
    --no-sync \
    --id "$CRABBOX_RAILWAY_SERVICE_ID" \
    --railway-project "$CRABBOX_RAILWAY_PROJECT_ID" \
    --railway-environment "$CRABBOX_RAILWAY_ENVIRONMENT_ID" \
    --timing-json \
    -- "$(smoke_command)"
  run_in_repo "$cb" status \
    --provider railway \
    --id "$CRABBOX_RAILWAY_SERVICE_ID" \
    --railway-project "$CRABBOX_RAILWAY_PROJECT_ID" \
    --railway-environment "$CRABBOX_RAILWAY_ENVIRONMENT_ID" || true
}

wandb_smoke() {
  need_any_env "W&B API key" CRABBOX_WANDB_API_KEY WANDB_API_KEY
  need_env WANDB_ENTITY_NAME
  run_in_repo "$cb" doctor --provider wandb
  run_in_repo "$cb" run \
    --provider wandb \
    --no-sync \
    --wandb-max-lifetime 60 \
    --timing-json \
    -- "$(smoke_command)"
  run_in_repo "$cb" list --provider wandb --json || true
}

preflight_provider() {
  case "$provider" in
    aws | azure | gcp | hetzner)
      direct_or_coordinator_env || skip_or_fail "CRABBOX_COORDINATOR and CRABBOX_COORDINATOR_TOKEN, or direct $provider credentials"
      ;;
	azure-dynamic-sessions)
		need_env CRABBOX_AZURE_DYNAMIC_SESSIONS_ENDPOINT
		if [[ -z "${CRABBOX_AZURE_DYNAMIC_SESSIONS_TOKEN:-}" ]]; then
			need_tool az
		fi
		;;
    proxmox)
      need_env CRABBOX_PROXMOX_API_URL
      need_env CRABBOX_PROXMOX_TOKEN_ID
      need_env CRABBOX_PROXMOX_TOKEN_SECRET
      need_env CRABBOX_PROXMOX_NODE
      need_env CRABBOX_PROXMOX_TEMPLATE_ID
      ;;
    parallels)
      if [[ -z "${CRABBOX_PARALLELS_HOST:-}" ]]; then
        need_tool prlctl
      fi
      if [[ -z "${CRABBOX_PARALLELS_TEMPLATE:-}" ]]; then
        need_any_env "Parallels source" CRABBOX_PARALLELS_SOURCE CRABBOX_PARALLELS_SOURCE_ID
        need_any_env "Parallels source snapshot" CRABBOX_PARALLELS_SOURCE_SNAPSHOT CRABBOX_PARALLELS_SOURCE_SNAPSHOT_ID
      fi
      ;;
    local-container)
      need_tool "${CRABBOX_LOCAL_CONTAINER_RUNTIME:-docker}"
      ;;
	apple-container)
		need_tool "${CRABBOX_APPLE_CONTAINER_CLI:-container}"
		;;
	multipass)
		need_tool "${CRABBOX_MULTIPASS_CLI:-multipass}"
		;;
    ssh)
      need_env CRABBOX_STATIC_HOST
      ;;
    exe-dev)
      need_tool ssh
      ;;
    blacksmith-testbox)
      ;;
    namespace-devbox)
      need_tool devbox
      ;;
    semaphore)
      need_env CRABBOX_SEMAPHORE_HOST
      need_env CRABBOX_SEMAPHORE_PROJECT
      need_any_env "Semaphore API token" CRABBOX_SEMAPHORE_TOKEN SEMAPHORE_API_TOKEN
      ;;
    sprites)
      need_tool sprite
      need_any_env "Sprites token" CRABBOX_SPRITES_TOKEN SPRITES_TOKEN SPRITE_TOKEN SETUP_SPRITE_TOKEN
      ;;
    daytona)
      ;;
    islo)
      need_env ISLO_API_KEY
      ;;
    e2b)
      need_any_env "E2B API key" CRABBOX_E2B_API_KEY E2B_API_KEY
      ;;
    modal)
      need_tool modal
      need_env_pair MODAL_TOKEN_ID MODAL_TOKEN_SECRET
      ;;
    upstash-box)
      need_any_env "Upstash Box API key" CRABBOX_UPSTASH_BOX_API_KEY UPSTASH_BOX_API_KEY
      ;;
    tensorlake)
      need_tool tensorlake
      need_any_env "Tensorlake API key" CRABBOX_TENSORLAKE_API_KEY TENSORLAKE_API_KEY
      ;;
	ascii-box)
		need_tool "${CRABBOX_ASCII_BOX_CLI:-box}"
		need_any_env "ASCII Box API key" CRABBOX_ASCII_BOX_API_KEY ASCII_BOX_API_KEY
		;;
    cloudflare)
      need_env CRABBOX_CLOUDFLARE_RUNNER_URL
      need_env CRABBOX_CLOUDFLARE_RUNNER_TOKEN
      ;;
    railway)
      ;;
    runpod)
      need_any_env "RunPod API key" CRABBOX_RUNPOD_API_KEY RUNPOD_API_KEY
      ;;
    wandb)
      ;;
	smolvm)
		need_any_env "SmolVM API key" CRABBOX_SMOLVM_API_KEY SMOLMACHINES_API_KEY SMK_API_KEY
		;;
    *)
		unknown_provider
      ;;
  esac
}

preflight_provider
if managed_provider; then
  coordinator_login
fi

case "$provider" in
  aws)
    ssh_lease_smoke --type "${CRABBOX_LIVE_AWS_TYPE:-t3.small}"
    ;;
  azure)
    ssh_lease_smoke --class "${CRABBOX_LIVE_AZURE_CLASS:-standard}"
    ;;
  gcp)
    ssh_lease_smoke --class "${CRABBOX_LIVE_GCP_CLASS:-standard}"
    ;;
  hetzner)
    ssh_lease_smoke --class "${CRABBOX_LIVE_HETZNER_CLASS:-standard}"
    ;;
	proxmox | parallels | local-container | apple-container | multipass | ssh | exe-dev | namespace-devbox | semaphore | sprites | runpod)
    ssh_lease_smoke
    ;;
  blacksmith-testbox)
    blacksmith_smoke
    ;;
  daytona)
    daytona_smoke
    ;;
	azure-dynamic-sessions | islo | e2b | modal | upstash-box | tensorlake | ascii-box | cloudflare | smolvm)
    delegated_run_smoke
    ;;
  railway)
    railway_smoke
    ;;
  wandb)
    wandb_smoke
    ;;
	*)
	unknown_provider
	;;
esac
