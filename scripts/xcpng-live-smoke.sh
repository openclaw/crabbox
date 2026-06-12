#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
  cat <<'USAGE'
Usage: scripts/xcpng-live-smoke.sh [--read-only|--mutate]

Runs a guarded XCP-ng provider smoke.

Default/read-only mode:
  - runs crabbox doctor --provider xcp-ng --json
  - writes redacted evidence under .crabbox/xcpng-live-smoke/
  - creates, changes, and deletes no XCP-ng resources

Mutating mode:
  - requires --mutate and CRABBOX_XCP_NG_LIVE_MUTATE=1
  - requires an XCP-ng template name or UUID in config or environment
  - warms one kept lease, runs a minimal no-sync command, then stops the lease

Secrets must come from private config or environment variables. This script
never accepts or forwards an XCP-ng password as an argument.
USAGE
}

load_xcpng_env_file() {
  local file="$1"
  local line key value
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [[ -z "$line" || "${line:0:1}" == "#" ]] && continue
    if [[ "$line" == export[[:space:]]* ]]; then
      line="${line#export }"
      line="${line#"${line%%[![:space:]]*}"}"
    fi
    [[ "$line" == *"="* ]] || continue
    key="${line%%=*}"
    value="${line#*=}"
    key="${key%"${key##*[![:space:]]}"}"
    value="${value#"${value%%[![:space:]]*}"}"
    case "$key" in
      CRABBOX_XCP_NG_API_URL|CRABBOX_XCP_NG_USERNAME|CRABBOX_XCP_NG_PASSWORD|\
      CRABBOX_XCP_NG_TEMPLATE|CRABBOX_XCP_NG_TEMPLATE_UUID|\
      CRABBOX_XCP_NG_SR|CRABBOX_XCP_NG_SR_UUID|\
      CRABBOX_XCP_NG_NETWORK|CRABBOX_XCP_NG_NETWORK_UUID|CRABBOX_XCP_NG_GUEST_CIDR|\
      CRABBOX_XCP_NG_HOST|CRABBOX_XCP_NG_USER|CRABBOX_XCP_NG_WORK_ROOT|\
      CRABBOX_XCP_NG_INSECURE_TLS)
        ;;
      *)
        continue
        ;;
    esac
    if [[ "$value" == \"*\" && "$value" == *\" && "${#value}" -ge 2 ]]; then
      value="${value:1:${#value}-2}"
    elif [[ "$value" == \'*\' && "${#value}" -ge 2 ]]; then
      value="${value:1:${#value}-2}"
    fi
    if [[ -z "${!key-}" ]]; then
      printf -v "$key" '%s' "$value"
      export "$key"
    fi
  done <"$file"
}

mode="read-only"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --read-only)
      mode="read-only"
      ;;
    --mutate)
      mode="mutate"
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *password*|*PASSWORD*)
      echo "refusing password-like argument; use private config or CRABBOX_XCP_NG_PASSWORD" >&2
      exit 2
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

env_file="${CRABBOX_XCP_NG_ENV_FILE:-.crabbox/xcpng.env}"
if [[ -f "$env_file" ]]; then
  load_xcpng_env_file "$env_file"
fi

crabbox_bin="${CRABBOX_BIN:-}"
if [[ -z "$crabbox_bin" ]]; then
  if [[ -x ./bin/crabbox ]]; then
    crabbox_bin="./bin/crabbox"
  else
    crabbox_bin="crabbox"
  fi
fi

resolve_configured_xcpng_api_url() {
  "$crabbox_bin" config show --json 2>/dev/null | python3 -c '
import json
import sys

try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(0)

value = data.get("xcpNg", {}).get("apiUrl", "")
if isinstance(value, str) and value.strip():
    print(value.strip())
'
}

redaction_api_url="${CRABBOX_XCP_NG_API_URL:-}"
if [[ -z "$redaction_api_url" ]]; then
  redaction_api_url="$(resolve_configured_xcpng_api_url || true)"
fi
export CRABBOX_XCP_NG_REDACT_API_URL="$redaction_api_url"

evidence_dir="${CRABBOX_XCP_NG_SMOKE_DIR:-.crabbox/xcpng-live-smoke}"
mkdir -p "$evidence_dir"
chmod 700 "$evidence_dir"

timestamp="$(date -u '+%Y%m%dT%H%M%SZ')"
doctor_log="$evidence_dir/${timestamp}-doctor.json"
warmup_log="$evidence_dir/${timestamp}-warmup.log"
run_log="$evidence_dir/${timestamp}-run.log"
stop_log="$evidence_dir/${timestamp}-stop.log"
summary_log="$evidence_dir/${timestamp}-summary.txt"
lease_id=""

redact_file() {
  local src="$1"
  local dst="$2"
  python3 - "$src" "$dst" <<'PY'
import re
import os
import sys
from pathlib import Path
from urllib.parse import urlparse

src = Path(sys.argv[1])
dst = Path(sys.argv[2])
text = src.read_text(encoding="utf-8", errors="replace")

api_url = os.environ.get("CRABBOX_XCP_NG_REDACT_API_URL", "").strip()
if api_url:
    parsed = urlparse(api_url if "://" in api_url else f"//{api_url}")
    if parsed.hostname:
        host = re.escape(parsed.hostname)
        pool_url_pattern = re.compile(
            rf"\bhttps?://(?:[^/?#\"'\s@]+@)?{host}(?=[:/?#\"'\s]|$)(?::[0-9]+)?[^\"'\s]*",
            re.I,
        )
        text = pool_url_pattern.sub("https://xcp-pool.example.test", text)

secret_key = r"(?:password|token|secret|session_id)"
patterns = [
    (re.compile(rf"((?:\"?{secret_key}\"?)\s*:\s*\")([^\"\r\n]*)(\")", re.I), r"\1<redacted>\3"),
    (re.compile(rf"((?:'?{secret_key}'?)\s*:\s*')([^'\r\n]*)(')", re.I), r"\1<redacted>\3"),
    (re.compile(rf"(\b{secret_key}\b\s*[:=]\s*)([^,\s}}]+)", re.I), r"\1<redacted>"),
]
for pattern, replacement in patterns:
    text = pattern.sub(replacement, text)

dst.write_text(text, encoding="utf-8")
PY
  chmod 600 "$dst"
}

redact_tmp_if_present() {
  local tmp="$1"
  local final="$2"
  if [[ -f "$tmp" ]]; then
    redact_file "$tmp" "$final"
    rm -f "$tmp"
  fi
}

cleanup() {
  local status=$?
  redact_tmp_if_present "$doctor_log.tmp" "$doctor_log"
  redact_tmp_if_present "$warmup_log.tmp" "$warmup_log"
  redact_tmp_if_present "$run_log.tmp" "$run_log"
  redact_tmp_if_present "$stop_log.tmp" "$stop_log"
  if [[ "$mode" == "mutate" && -n "$lease_id" ]]; then
    "$crabbox_bin" stop --provider xcp-ng "$lease_id" >"$stop_log.tmp" 2>&1 || true
    redact_tmp_if_present "$stop_log.tmp" "$stop_log"
  fi
  exit "$status"
}
trap cleanup EXIT

if ! command -v "$crabbox_bin" >/dev/null 2>&1 && [[ ! -x "$crabbox_bin" ]]; then
  echo "crabbox binary not found: $crabbox_bin" >&2
  exit 127
fi

if "$crabbox_bin" doctor --provider xcp-ng --json >"$doctor_log.tmp" 2>&1; then
  doctor_status="ok"
else
  doctor_status="environment_blocked"
fi
redact_file "$doctor_log.tmp" "$doctor_log"
rm -f "$doctor_log.tmp"

if [[ "$doctor_status" != "ok" ]]; then
  {
    echo "classification=environment_blocked"
    echo "reason=doctor_failed"
    echo "doctor_log=$doctor_log"
  } > "$summary_log"
  cat "$summary_log"
  exit 3
fi

if [[ "$mode" == "read-only" ]]; then
  {
    echo "classification=read_only_doctor_passed"
    echo "mutation=false"
    echo "doctor_log=$doctor_log"
  } > "$summary_log"
  cat "$summary_log"
  exit 0
fi

if [[ "${CRABBOX_XCP_NG_LIVE_MUTATE:-}" != "1" ]]; then
  {
    echo "classification=environment_blocked"
    echo "reason=mutation_gate_missing"
    echo "required=CRABBOX_XCP_NG_LIVE_MUTATE=1"
    echo "doctor_log=$doctor_log"
  } > "$summary_log"
  cat "$summary_log"
  exit 3
fi

if "$crabbox_bin" warmup --provider xcp-ng --keep --slug xcp-ng-live-smoke --timing-json >"$warmup_log.tmp" 2>&1; then
  :
else
  redact_file "$warmup_log.tmp" "$warmup_log"
  rm -f "$warmup_log.tmp"
  {
    echo "classification=environment_blocked"
    echo "reason=warmup_failed"
    echo "doctor_log=$doctor_log"
    echo "warmup_log=$warmup_log"
  } > "$summary_log"
  cat "$summary_log"
  exit 3
fi

lease_id="$(sed -nE 's/^leased (cbx_[[:xdigit:]]+).*/\1/p' "$warmup_log.tmp" | head -n 1)"
redact_file "$warmup_log.tmp" "$warmup_log"
rm -f "$warmup_log.tmp"

if [[ -z "$lease_id" ]]; then
  {
    echo "classification=environment_blocked"
    echo "reason=lease_id_not_found"
    echo "doctor_log=$doctor_log"
    echo "warmup_log=$warmup_log"
  } > "$summary_log"
  cat "$summary_log"
  exit 3
fi

if "$crabbox_bin" run --provider xcp-ng --id "$lease_id" --no-sync -- echo xcp-ng-ok >"$run_log.tmp" 2>&1; then
  redact_tmp_if_present "$run_log.tmp" "$run_log"
else
  redact_tmp_if_present "$run_log.tmp" "$run_log"
  {
    echo "classification=environment_blocked"
    echo "reason=run_failed"
    echo "doctor_log=$doctor_log"
    echo "warmup_log=$warmup_log"
    echo "run_log=$run_log"
  } > "$summary_log"
  cat "$summary_log"
  exit 3
fi

if "$crabbox_bin" stop --provider xcp-ng "$lease_id" >"$stop_log.tmp" 2>&1; then
  :
else
  redact_tmp_if_present "$stop_log.tmp" "$stop_log"
  {
    echo "classification=environment_blocked"
    echo "reason=stop_failed"
    echo "doctor_log=$doctor_log"
    echo "warmup_log=$warmup_log"
    echo "run_log=$run_log"
    echo "stop_log=$stop_log"
  } > "$summary_log"
  cat "$summary_log"
  exit 3
fi
redact_tmp_if_present "$stop_log.tmp" "$stop_log"
lease_id=""

{
  echo "classification=live_smoke_passed"
  echo "doctor_log=$doctor_log"
  echo "warmup_log=$warmup_log"
  echo "run_log=$run_log"
  echo "stop_log=$stop_log"
} > "$summary_log"
cat "$summary_log"
