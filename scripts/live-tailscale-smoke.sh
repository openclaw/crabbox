#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/live-tailscale-smoke.sh [--json]

Runs the coordinator Tailscale preflight endpoint.

Environment:
  CRABBOX_LIVE=1                         required for live coordinator calls
  CRABBOX_COORDINATOR                    coordinator URL
  CRABBOX_TAILSCALE_SMOKE_COORDINATOR    coordinator URL override
  CRABBOX_ADMIN_TOKEN                    admin token
  CRABBOX_TAILSCALE_SMOKE_ADMIN_TOKEN    admin token override
  CRABBOX_TAILSCALE_ENABLED=0            emit local disabled result without network
USAGE
}

json=0
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --json)
      json=1
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

if [[ "${CRABBOX_TAILSCALE_ENABLED:-}" == "0" ]]; then
  if [[ "$json" == "1" ]]; then
    printf '{"tailscale":{"status":"disabled","enabled":false,"message":"Tailscale is disabled for this coordinator"}}\n'
  else
    echo "tailscale status=disabled enabled=false"
  fi
  exit 0
fi

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  echo "set CRABBOX_LIVE=1 to run live Tailscale coordinator smoke tests" >&2
  exit 2
fi

need_tool() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required tool: $1" >&2
    exit 2
  fi
}

need_tool curl
need_tool jq
need_tool ruby

coord="${CRABBOX_TAILSCALE_SMOKE_COORDINATOR:-${CRABBOX_COORDINATOR:-}}"
token="${CRABBOX_TAILSCALE_SMOKE_ADMIN_TOKEN:-${CRABBOX_ADMIN_TOKEN:-}}"
if [[ -z "$coord" ]]; then
  echo "missing coordinator URL: set CRABBOX_TAILSCALE_SMOKE_COORDINATOR or CRABBOX_COORDINATOR" >&2
  exit 2
fi
if [[ -z "$token" ]]; then
  echo "missing admin token: set CRABBOX_TAILSCALE_SMOKE_ADMIN_TOKEN or CRABBOX_ADMIN_TOKEN" >&2
  exit 2
fi

tmp_cfg="$(mktemp)"
tmp_body="$(mktemp)"
cleanup() {
  rm -f "$tmp_cfg" "$tmp_body"
}
trap cleanup EXIT
chmod 0600 "$tmp_cfg"

curl_quote() {
  ruby -e 'print ARGV[0].inspect' "$1"
}

{
  printf 'url = %s\n' "$(curl_quote "${coord%/}/v1/admin/tailscale-preflight")"
  printf 'request = "POST"\n'
  printf 'connect-timeout = "10"\n'
  printf 'max-time = "300"\n'
  printf 'silent\n'
  printf 'show-error\n'
  printf 'location\n'
  printf 'output = %s\n' "$(curl_quote "$tmp_body")"
  printf 'write-out = "%%{http_code}"\n'
  printf 'header = %s\n' "$(curl_quote "Authorization: Bearer $token")"
} >"$tmp_cfg"

http_code="$(curl --config "$tmp_cfg")"
if [[ "$http_code" != "200" ]]; then
  echo "tailscale preflight failed http=$http_code body=$(cat "$tmp_body")" >&2
  exit 1
fi

if [[ "$json" == "1" ]]; then
  jq -c . "$tmp_body"
else
  status="$(jq -r '.tailscale.status // "unknown"' "$tmp_body")"
  enabled="$(jq -r '.tailscale.enabled // false' "$tmp_body")"
  tags="$(jq -r '(.tailscale.tags // []) | join(",")' "$tmp_body")"
  install_mode="$(jq -r '.tailscale.install.mode // "unknown"' "$tmp_body")"
  echo "tailscale status=$status enabled=$enabled tags=${tags:-none} install=$install_mode"
fi
