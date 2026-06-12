#!/usr/bin/env bash
set -euo pipefail

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  echo "set CRABBOX_LIVE=1 to run live coordinator auth smoke tests" >&2
  exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cb="${CRABBOX_BIN:-$root/bin/crabbox}"
config_cwd="$PWD"
config_paths=()
tmp_files=()

cleanup_tmp_files() {
  if ((${#tmp_files[@]} > 0)); then
    rm -f "${tmp_files[@]}"
  fi
}
trap cleanup_tmp_files EXIT

add_config_path() {
  local path="$1"
  [[ -n "$path" ]] || return 0
  if [[ "$path" != /* ]]; then
    path="$config_cwd/$path"
  fi
  config_paths+=("$path")
}

if [[ -n "${CRABBOX_CONFIG:-}" ]]; then
  add_config_path "$CRABBOX_CONFIG"
else
  add_config_path "$("$cb" config path 2>/dev/null || true)"
  add_config_path "$config_cwd/crabbox.yaml"
  add_config_path "$config_cwd/.crabbox.yaml"
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

config_value() {
  local key_path="$1"
  local value=""
  local found=0
  local path
  local candidate
  for path in "${config_paths[@]}"; do
    [[ -r "$path" ]] || continue
    if candidate="$(ruby -ryaml -e '
      value = ARGV[1].split(".").reduce(YAML.load_file(ARGV[0])) do |memo, key|
        memo.is_a?(Hash) ? memo[key] : nil
      end
      exit 3 if value.nil? || value.to_s.empty?
      print value
    ' "$path" "$key_path" 2>/dev/null)"; then
      value="$candidate"
      found=1
    fi
  done
  if [[ "$found" == "1" ]]; then
    printf '%s' "$value"
    return 0
  fi
  return 1
}

required_config_value() {
  local key_path="$1"
  local value
  if ! value="$(config_value "$key_path" 2>/dev/null)"; then
    echo "missing required config: $key_path" >&2
    exit 2
  fi
  printf '%s' "$value"
}

curl_quote() {
  ruby -e 'print ARGV[0].inspect' "$1"
}

write_curl_config() {
  local token="$1"
  local url="$2"
  local output="$3"
  shift 3
  : >"$output"
  chmod 0600 "$output"
  {
    printf 'url = %s\n' "$(curl_quote "$url")"
    printf 'request = "GET"\n'
    printf 'connect-timeout = "10"\n'
    printf 'max-time = "300"\n'
    printf 'silent\n'
    printf 'show-error\n'
    printf 'location\n'
    printf 'output = "-"\n'
    printf 'write-out = "\\n%%{http_code}"\n'
    printf 'header = %s\n' "$(curl_quote "Authorization: Bearer $token")"
    if [[ -n "${access_client_id:-}" && -n "${access_client_secret:-}" ]]; then
      printf 'header = %s\n' "$(curl_quote "CF-Access-Client-Id: $access_client_id")"
      printf 'header = %s\n' "$(curl_quote "CF-Access-Client-Secret: $access_client_secret")"
    fi
    for header in "$@"; do
      printf 'header = %s\n' "$(curl_quote "$header")"
    done
  } >>"$output"
}

request_json() {
  local token="$1"
  local path="$2"
  local body_file="$3"
  shift 3
  local cfg
  cfg="$(mktemp)"
  if ! write_curl_config "$token" "${coord%/}$path" "$cfg" "$@"; then
    rm -f "$cfg"
    return 1
  fi
  local response
  local curl_status
  set +e
  response="$(curl --config "$cfg")"
  curl_status=$?
  set -e
  rm -f "$cfg"
  if [[ "$curl_status" != "0" ]]; then
    return "$curl_status"
  fi
  local status="${response##*$'\n'}"
  local body="${response%$'\n'*}"
  printf '%s' "$body" >"$body_file"
  printf '%s' "$status"
}

shared_token="$(required_config_value broker.token)"
admin_token="$(required_config_value broker.adminToken)"
coord="${CRABBOX_AUTH_SMOKE_COORDINATOR:-${CRABBOX_COORDINATOR:-$(config_value broker.url 2>/dev/null || true)}}"
if [[ -z "$coord" ]]; then
  echo "missing coordinator URL: set CRABBOX_AUTH_SMOKE_COORDINATOR, CRABBOX_COORDINATOR, or broker.url" >&2
  exit 2
fi
access_client_id="${CRABBOX_ACCESS_CLIENT_ID:-$(config_value broker.access.clientId 2>/dev/null || true)}"
access_client_secret="${CRABBOX_ACCESS_CLIENT_SECRET:-$(config_value broker.access.clientSecret 2>/dev/null || true)}"
owner="${CRABBOX_OWNER:-$(git config user.email 2>/dev/null || true)}"
owner="${owner:-crabbox-auth-smoke@example.invalid}"
org="${CRABBOX_ORG:-example-org}"

access_smoke="${CRABBOX_AUTH_SMOKE_ACCESS:-0}"
if [[ "$access_smoke" == "1" ]]; then
  no_access_code="$(curl -sS -o /dev/null -w '%{http_code}' "${coord%/}/v1/health")"
  if [[ "$no_access_code" != "403" ]]; then
    echo "failed no-access edge check: HTTP $no_access_code" >&2
    exit 1
  fi
  echo "ok no-access edge denied http=403"
  if [[ -z "$access_client_id" || -z "$access_client_secret" ]]; then
    echo "access auth smoke requires CRABBOX_ACCESS_CLIENT_ID/CRABBOX_ACCESS_CLIENT_SECRET or broker.access.clientId/clientSecret for $coord" >&2
    exit 2
  fi
elif [[ "$access_smoke" != "0" ]]; then
  echo "CRABBOX_AUTH_SMOKE_ACCESS must be 0 or 1" >&2
  exit 2
fi

whoami_err="$(mktemp)"
tmp_files+=("$whoami_err")
set +e
whoami="$(env -u CRABBOX_COORDINATOR_TOKEN CRABBOX_COORDINATOR="$coord" "$cb" whoami --json 2>"$whoami_err")"
whoami_status=$?
set -e
if [[ "$whoami_status" -ne 0 ]]; then
  echo "failed coordinator whoami: $(cat "$whoami_err")" >&2
  exit 1
fi
if ! printf '%s\n' "$whoami" | jq -e '(.auth == "bearer" or .auth == "github") and (.owner | length > 0) and (.org | length > 0)' >/dev/null; then
  echo "failed coordinator whoami shape: $whoami" >&2
  if [[ -s "$whoami_err" ]]; then
    echo "coordinator whoami stderr: $(cat "$whoami_err")" >&2
  fi
  exit 1
fi
if [[ -s "$whoami_err" ]]; then
  cat "$whoami_err" >&2
fi
whoami_auth="$(printf '%s\n' "$whoami" | jq -r '.auth')"
echo "ok coordinator whoami auth=$whoami_auth owner=$(printf '%s\n' "$whoami" | jq -r '.owner') org=$(printf '%s\n' "$whoami" | jq -r '.org')"

body="$(mktemp)"
tmp_files+=("$body")

status="$(request_json "$shared_token" "/v1/whoami" "$body" \
  "X-Crabbox-Owner: $owner" \
  "X-Crabbox-Org: $org" \
  "cf-access-authenticated-user-email: spoof@example.invalid")"
if [[ "$status" != "200" ]]; then
  echo "failed raw Access spoof check: HTTP $status body=$(cat "$body")" >&2
  exit 1
fi
spoof_owner="$(jq -r '.owner' "$body")"
if [[ "$spoof_owner" == "spoof@example.invalid" ]]; then
  echo "failed raw Access spoof check: spoofed owner accepted" >&2
  exit 1
fi
echo "ok raw Access identity spoof ignored owner=$spoof_owner"

status="$(request_json "$shared_token" "/v1/admin/leases?limit=1" "$body" \
  "X-Crabbox-Owner: $owner" \
  "X-Crabbox-Org: $org")"
if [[ "$status" != "403" ]]; then
  echo "failed shared-token admin denial: HTTP $status body=$(cat "$body")" >&2
  exit 1
fi
jq -e '.message == "admin token required"' "$body" >/dev/null
echo "ok shared token denied for admin http=403"

status="$(request_json "$admin_token" "/v1/admin/leases?limit=1" "$body" \
  "X-Crabbox-Owner: $owner" \
  "X-Crabbox-Org: $org")"
if [[ "$status" != "200" ]]; then
  echo "failed admin-token admin check: HTTP $status body=$(cat "$body")" >&2
  exit 1
fi
jq -e '.leases | type == "array"' "$body" >/dev/null
echo "ok admin token accepted for admin leases=$(jq '.leases | length' "$body")"
