#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CRABBOX_BIN="${CRABBOX_BIN:-$ROOT/bin/crabbox}"

profile_export() {
  local name="$1" file line value
  for file in "$HOME/.profile" "$HOME/.zprofile"; do
    [[ -r "$file" ]] || continue
    line="$(grep -E "^export ${name}=" "$file" | tail -n 1 || true)"
    [[ -n "$line" ]] || continue
    value="${line#export ${name}=}"
    value="${value%\"}"
    value="${value#\"}"
    value="${value%\'}"
    value="${value#\'}"
    printf '%s' "$value"
    return 0
  done
  return 1
}

if [[ -z "${CRABBOX_CLOUDFLARE_API_TOKEN:-}" ]]; then
  CRABBOX_CLOUDFLARE_API_TOKEN="$(profile_export CRABBOX_CLOUDFLARE_API_TOKEN || true)"
fi
if [[ -z "${CRABBOX_CLOUDFLARE_ACCOUNT_ID:-}" ]]; then
  CRABBOX_CLOUDFLARE_ACCOUNT_ID="$(profile_export CRABBOX_CLOUDFLARE_ACCOUNT_ID || true)"
fi
if [[ -n "${CRABBOX_CLOUDFLARE_API_TOKEN:-}" ]]; then
  export CLOUDFLARE_API_TOKEN="$CRABBOX_CLOUDFLARE_API_TOKEN"
fi
if [[ -n "${CRABBOX_CLOUDFLARE_ACCOUNT_ID:-}" ]]; then
  export CLOUDFLARE_ACCOUNT_ID="$CRABBOX_CLOUDFLARE_ACCOUNT_ID"
fi

run() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  "$@"
}

validate_smoke_url() {
  local url="$1" rest authority path host host_lower port labels label octets octet
  if [[ "$url" == *[[:space:]]* || "$url" == *'<'* || "$url" == *'>'* ]]; then
    printf 'CRABBOX_DEPLOY_SMOKE_URLS entries must be concrete URLs without spaces or angle brackets: %s\n' "$url" >&2
    exit 2
  fi
  case "$url" in
    http://* | https://*) ;;
    *)
      printf 'CRABBOX_DEPLOY_SMOKE_URLS entries must start with http:// or https://: %s\n' "$url" >&2
      exit 2
      ;;
  esac
  rest="${url#http://}"
  rest="${rest#https://}"
  authority="${rest%%/*}"
  if [[ "$authority" == *"@"* ]]; then
    printf 'CRABBOX_DEPLOY_SMOKE_URLS entry has a malformed host: %s\n' "$authority" >&2
    exit 2
  fi
  path="${rest#"$authority"}"
  case "$path" in
    /v1/health | /v1/health\?*) ;;
    *)
      printf 'CRABBOX_DEPLOY_SMOKE_URLS entries must point at /v1/health: %s\n' "$url" >&2
      exit 2
      ;;
  esac
  if [[ "$authority" == *":"* ]]; then
    port="${authority##*:}"
    host="${authority%:*}"
    if [[ "$host" == *":"* || ! "$port" =~ ^[0-9]+$ || "$port" -lt 1 || "$port" -gt 65535 ]]; then
      printf 'CRABBOX_DEPLOY_SMOKE_URLS entry has a malformed host or port: %s\n' "$authority" >&2
      exit 2
    fi
  else
    host="$authority"
  fi
  if [[ -z "$host" ]]; then
    printf 'CRABBOX_DEPLOY_SMOKE_URLS entry is missing a host: %s\n' "$url" >&2
    exit 2
  fi
  if [[ "$host" == *..* || "$host" == .* || "$host" == *. ]]; then
    printf 'CRABBOX_DEPLOY_SMOKE_URLS entry has a malformed host: %s\n' "$host" >&2
    exit 2
  fi
  if [[ "$host" != "localhost" ]]; then
    if [[ "$host" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
      IFS='.' read -r -a octets <<< "$host"
      for octet in "${octets[@]}"; do
        if [[ "$octet" -gt 255 ]]; then
          printf 'CRABBOX_DEPLOY_SMOKE_URLS entry has a malformed host: %s\n' "$host" >&2
          exit 2
        fi
      done
    else
      if [[ "$host" != *.* ]]; then
        printf 'CRABBOX_DEPLOY_SMOKE_URLS entry must use a deployed FQDN, IP, or localhost: %s\n' "$host" >&2
        exit 2
      fi
      IFS='.' read -r -a labels <<< "$host"
      for label in "${labels[@]}"; do
        if [[ ! "$label" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$ ]]; then
          printf 'CRABBOX_DEPLOY_SMOKE_URLS entry has a malformed host: %s\n' "$host" >&2
          exit 2
        fi
      done
    fi
  fi
  host_lower="$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]')"
  case "$host_lower" in
    example.com | *.example.com | example.org | *.example.org | example.net | *.example.net)
      printf 'CRABBOX_DEPLOY_SMOKE_URLS must use a real deployed host, not placeholder host: %s\n' "$host" >&2
      exit 2
      ;;
  esac
}

smoke_origin() {
  local url="$1" scheme rest authority
  case "$url" in
    http://*) scheme="http" ;;
    https://*) scheme="https" ;;
  esac
  rest="${url#http://}"
  rest="${rest#https://}"
  authority="${rest%%/*}"
  printf '%s://%s' "$scheme" "$authority"
}

IFS=',' read -r -a raw_smoke_urls <<< "${CRABBOX_DEPLOY_SMOKE_URLS:-}"
smoke_urls=()
smoke_origins=()
for url in "${raw_smoke_urls[@]}"; do
  url="${url#"${url%%[![:space:]]*}"}"
  url="${url%"${url##*[![:space:]]}"}"
  [[ -n "$url" ]] || continue
  validate_smoke_url "$url"
  smoke_urls+=("$url")
  smoke_origins+=("$(smoke_origin "$url")")
done
if [[ "${#smoke_urls[@]}" -eq 0 ]]; then
  printf 'CRABBOX_DEPLOY_SMOKE_URLS is required before deploy, e.g. https://broker.company.com/v1/health\n' >&2
  exit 2
fi

if [[ "${CRABBOX_DEPLOY_SMOKE_AWS:-}" == "1" ]]; then
  if [[ "${#smoke_origins[@]}" -ne 1 ]]; then
    printf 'CRABBOX_DEPLOY_SMOKE_AWS=1 requires exactly one health URL so the coordinator is unambiguous\n' >&2
    exit 2
  fi
  if [[ -n "${CRABBOX_COORDINATOR:-}" && "${CRABBOX_COORDINATOR%/}" != "${smoke_origins[0]%/}" ]]; then
    printf 'CRABBOX_COORDINATOR must match CRABBOX_DEPLOY_SMOKE_URLS origin for AWS deploy smoke: %s != %s\n' "${CRABBOX_COORDINATOR%/}" "${smoke_origins[0]%/}" >&2
    exit 2
  fi
  export CRABBOX_COORDINATOR="${smoke_origins[0]%/}"
fi

run npm --prefix "$ROOT/worker" run format:check
run npm --prefix "$ROOT/worker" run lint
run npm --prefix "$ROOT/worker" run check
run npm --prefix "$ROOT/worker" test
run npm --prefix "$ROOT/worker" run build
run npm --prefix "$ROOT/worker" run deploy

for url in "${smoke_urls[@]}"; do
  printf '+ curl -fsS'
  printf ' %q' "$url"
  printf '\n'
  response="$(curl -fsS "$url")"
  printf '%s\n' "$response"
  if ! node -e 'let data="";process.stdin.on("data",c=>data+=c);process.stdin.on("end",()=>{try{const v=JSON.parse(data);if(v.ok===true&&v.service==="crabbox-coordinator")return;}catch{} process.exit(1);});' <<<"$response"; then
    printf 'deploy smoke URL did not return Crabbox coordinator health JSON: %s\n' "$url" >&2
    exit 1
  fi
done

if [[ "${CRABBOX_DEPLOY_SMOKE_AWS:-}" != "1" ]]; then
  printf 'deploy smoke complete; set CRABBOX_DEPLOY_SMOKE_AWS=1 for an opt-in AWS lease smoke\n'
  exit 0
fi

if [[ -z "${CRABBOX_LIVE_REPO:-}" ]]; then
  printf 'CRABBOX_LIVE_REPO is required for CRABBOX_DEPLOY_SMOKE_AWS=1\n' >&2
  exit 2
fi

if [[ ! -x "$CRABBOX_BIN" ]]; then
  printf 'CRABBOX_BIN is not executable: %s\n' "$CRABBOX_BIN" >&2
  exit 2
fi

warmup_dir="$(mktemp -d)"
log="$warmup_dir/warmup.err"
warmup_pipe="$warmup_dir/warmup.err.pipe"
lease_id=""
cleanup() {
  if [[ -n "$lease_id" ]]; then
    (cd "$CRABBOX_LIVE_REPO" && "$CRABBOX_BIN" stop "$lease_id") || true
  fi
  rm -rf "$warmup_dir"
}
trap cleanup EXIT

warmup_status=0
mkfifo "$warmup_pipe"
tee "$log" <"$warmup_pipe" >&2 &
tee_pid="$!"
(
  cd "$CRABBOX_LIVE_REPO"
  "$CRABBOX_BIN" warmup --provider aws --ttl 20m --idle-timeout 6m --reclaim --timing-json
) 2>"$warmup_pipe" || warmup_status=$?
rm -f "$warmup_pipe"
wait "$tee_pid"
if [[ "$warmup_status" != "0" ]]; then
  exit "$warmup_status"
fi

if ! lease_id="$(
  node -e 'const fs=require("fs"); for (const line of fs.readFileSync(process.argv[1],"utf8").trim().split(/\n/).reverse()) { try { const json=JSON.parse(line); if (json.leaseId) { console.log(json.leaseId); process.exit(0); } } catch {} } process.exit(1);' "$log"
)"; then
  printf 'warmup succeeded but no leaseId could be parsed from timing output; a 20m AWS smoke lease may need manual cleanup\n' >&2
  exit 1
fi
printf 'aws deploy smoke lease=%s\n' "$lease_id"

(
  cd "$CRABBOX_LIVE_REPO"
  "$CRABBOX_BIN" run --id "$lease_id" --no-sync --timing-json -- uname -a
)

(
  cd "$CRABBOX_LIVE_REPO"
  "$CRABBOX_BIN" stop "$lease_id"
)
lease_id=""
