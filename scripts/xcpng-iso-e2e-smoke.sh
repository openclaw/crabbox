#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
  cat <<'USAGE'
Usage: scripts/xcpng-iso-e2e-smoke.sh [--read-only|--mutate] --os linux|windows --iso <path-or-vdi>
       [--answer-iso <path-or-vdi>] [--name-prefix <prefix>] [--timeout <duration>]

Runs the guarded XCP-ng fresh-installer ISO E2E harness.

Read-only mode:
  - validates merged Crabbox config and XCP-ng placement
  - resolves the installer ISO as a local file or XCP-ng VDI reference
  - writes redacted evidence under .crabbox/xcpng-iso-e2e/
  - creates, changes, and deletes no XCP-ng resources

Mutating mode:
  - requires --mutate and CRABBOX_XCP_NG_ISO_E2E_MUTATE=1
  - on --os linux, generates NoCloud answer media, imports local ISO media when needed,
    installs Ubuntu Server to a blank disk, proves first boot over SSH, and cleans up
  - on --os windows, expects x64/x86 installer media, generates unattended answer media
    when none is supplied, imports local ISO media when needed, and attempts first-boot
    SSH proof; if only guest metrics are available it reports source_uncovered

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
os_name=""
iso_value=""
answer_iso=""
name_prefix="crabbox-xcpng-iso-e2e"
timeout_value="90m"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --read-only)
      mode="read-only"
      ;;
    --mutate)
      mode="mutate"
      ;;
    --os)
      shift
      [[ $# -gt 0 ]] || { echo "missing value for --os" >&2; exit 2; }
      os_name="$1"
      ;;
    --iso)
      shift
      [[ $# -gt 0 ]] || { echo "missing value for --iso" >&2; exit 2; }
      iso_value="$1"
      ;;
    --answer-iso)
      shift
      [[ $# -gt 0 ]] || { echo "missing value for --answer-iso" >&2; exit 2; }
      answer_iso="$1"
      ;;
    --name-prefix)
      shift
      [[ $# -gt 0 ]] || { echo "missing value for --name-prefix" >&2; exit 2; }
      name_prefix="$1"
      ;;
    --timeout)
      shift
      [[ $# -gt 0 ]] || { echo "missing value for --timeout" >&2; exit 2; }
      timeout_value="$1"
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

if [[ "$os_name" != "linux" && "$os_name" != "windows" ]]; then
  echo "--os must be linux or windows" >&2
  exit 2
fi

if [[ -z "$iso_value" ]]; then
  echo "--iso is required" >&2
  exit 2
fi

env_file="${CRABBOX_XCP_NG_ENV_FILE:-.crabbox/xcpng.env}"
if [[ -f "$env_file" ]]; then
  load_xcpng_env_file "$env_file"
fi

resolve_configured_xcpng_api_url() {
  local helper_bin="${CRABBOX_BIN:-}"
  if [[ -n "$helper_bin" ]]; then
    "$helper_bin" config show --json 2>/dev/null | python3 -c '
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
    return 0
  fi
  go run ./cmd/crabbox config show --json 2>/dev/null | python3 -c '
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

evidence_dir="${CRABBOX_XCP_NG_ISO_E2E_DIR:-.crabbox/xcpng-iso-e2e}"
mkdir -p "$evidence_dir"
chmod 700 "$evidence_dir"

timestamp="$(date -u '+%Y%m%dT%H%M%SZ')"
run_marker="$(mktemp "$evidence_dir/${timestamp}-XXXXXX")"
run_id="$(basename "$run_marker")"
rm -f "$run_marker"
run_log="$evidence_dir/${run_id}-run.json"
summary_log="$evidence_dir/${run_id}-summary.json"
stdout_log="$evidence_dir/${run_id}-stdout.log"
stderr_log="$evidence_dir/${run_id}-stderr.log"

redact_file() {
  local src="$1"
  local dst="$2"
  python3 - "$src" "$dst" <<'PY'
import json
import os
import re
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

secret_key = r"(?:password|token|secret|session_id|username|api_url)"
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

tmp_stdout="$stdout_log.tmp"
tmp_stderr="$stderr_log.tmp"
helper_status=0
helper_cmd=(go run ./cmd/xcpng-iso-e2e-helper)
if [[ -n "${CRABBOX_XCP_NG_ISO_E2E_HELPER:-}" ]]; then
  helper_cmd=("$CRABBOX_XCP_NG_ISO_E2E_HELPER")
fi
set +e
helper_args=(
  --mode "$mode"
  --os "$os_name"
  --iso "$iso_value"
  --name-prefix "$name_prefix"
  --timeout "$timeout_value"
  --evidence-dir "$evidence_dir"
  --summary "$summary_log.tmp"
)
if [[ -n "$answer_iso" ]]; then
  helper_args+=(--answer-iso "$answer_iso")
fi
"${helper_cmd[@]}" "${helper_args[@]}" >"$tmp_stdout" 2>"$tmp_stderr"
helper_status=$?
set -e

if [[ -f "$tmp_stdout" ]]; then
  redact_file "$tmp_stdout" "$stdout_log"
fi
if [[ -f "$tmp_stderr" ]]; then
  redact_file "$tmp_stderr" "$stderr_log"
fi
rm -f "$tmp_stdout" "$tmp_stderr"

if [[ -f "$summary_log.tmp" ]]; then
  redact_file "$summary_log.tmp" "$summary_log"
  rm -f "$summary_log.tmp"
fi

if [[ -f "$stdout_log" ]]; then
  cp "$stdout_log" "$run_log"
fi

python3 - "$stdout_log" "$summary_log" <<'PY'
import json
import sys
from pathlib import Path

stdout_path = Path(sys.argv[1])
summary_path = Path(sys.argv[2])

stdout_text = stdout_path.read_text(encoding="utf-8", errors="replace").strip()
summary = {}
if stdout_text:
    try:
        summary = json.loads(stdout_text)
    except Exception:
        summary = {"classification": "test_failed", "reason": "invalid_helper_output", "stdout": stdout_text}
elif summary_path.exists():
    try:
        summary = json.loads(summary_path.read_text(encoding="utf-8", errors="replace"))
    except Exception:
        summary = {"classification": "test_failed", "reason": "invalid_summary_json"}
else:
    summary = {"classification": "test_failed", "reason": "missing_summary"}

for key in ["classification", "mutation", "os", "iso", "phase", "cleanup"]:
    if key not in summary:
        summary[key] = "" if key != "mutation" else False

evidence = summary.setdefault("evidence", {})
evidence["summary"] = str(summary_path)
evidence["stdout"] = str(stdout_path)
summary_path.write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
print(json.dumps(summary, indent=2))
PY

exit "$helper_status"
