#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() {
  cat <<'EOF'
Run an opt-in Blaxel live proof through the public Crabbox CLI.

By default the script is non-mutating. It runs read-only preflight when the
Crabbox binary and Blaxel credentials are available, then exits skipped unless
live mutation is explicitly enabled.

Live mutation requires both:
  CRABBOX_LIVE=1
  CRABBOX_LIVE_PROVIDERS containing blaxel or all

Environment:
  CRABBOX_BIN                       Crabbox binary (default: ./bin/crabbox)
  CRABBOX_LIVE                      Set to 1 to permit live provider smokes
  CRABBOX_LIVE_PROVIDERS            Comma list; must contain blaxel or all
  CRABBOX_BLAXEL_LIVE_SMOKE_SLUG    Per-run slug prefix (default: blaxel-live-smoke)
  CRABBOX_BLAXEL_LIVE_SMOKE_DIR     Private proof directory

Keep Blaxel credentials in environment/config only. This script never passes API
keys or workspace values as command-line arguments.
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin="${CRABBOX_BIN:-$repo_root/bin/crabbox}"
live="${CRABBOX_LIVE:-0}"
live_providers=",${CRABBOX_LIVE_PROVIDERS:-},"
slug_prefix="${CRABBOX_BLAXEL_LIVE_SMOKE_SLUG:-blaxel-live-smoke}"
proof_dir=""
smoke_root=""
smoke_repo=""
slug=""
cleanup_armed=0
classification_emitted=0

slug_prefix="$(
  printf '%s' "$slug_prefix" |
    tr '[:upper:]' '[:lower:]' |
    sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//'
)"
if [[ -z "$slug_prefix" ]]; then
  slug_prefix="blaxel-live-smoke"
fi
nonce="$(od -An -N4 -tx1 /dev/urandom 2>/dev/null | tr -d '[:space:]' || true)"
if [[ ! "$nonce" =~ ^[0-9a-f]{8}$ ]]; then
  nonce="$$"
fi
slug="${slug_prefix:0:32}-$(date -u +%Y%m%d%H%M%S)-$nonce"

directory_is_private() {
  local mode=""
  mode="$(stat -f '%Lp' "$1" 2>/dev/null)" || mode=""
  if [[ "$mode" == "700" || "$mode" == "0700" ]]; then
    return 0
  fi
  mode="$(stat -c '%a' "$1" 2>/dev/null)" || mode=""
  [[ "$mode" == "700" || "$mode" == "0700" ]]
}

prepare_proof_dir() {
  if [[ -n "$proof_dir" ]]; then
    return
  fi
  if [[ -n "${CRABBOX_BLAXEL_LIVE_SMOKE_DIR:-}" ]]; then
    proof_dir="${CRABBOX_BLAXEL_LIVE_SMOKE_DIR}"
    if [[ -L "$proof_dir" ]]; then
      classify_and_exit environment_blocked "refusing symlink proof directory: <proof-dir>"
    fi
    if [[ -e "$proof_dir" ]]; then
      if [[ ! -d "$proof_dir" || ! -O "$proof_dir" ]] || ! directory_is_private "$proof_dir"; then
        classify_and_exit environment_blocked "proof directory must be owner-owned mode 700: <proof-dir>"
      fi
    else
      mkdir -p "$proof_dir" || classify_and_exit environment_blocked "could not create proof directory: <proof-dir>"
      chmod 700 "$proof_dir" || classify_and_exit environment_blocked "could not secure proof directory: <proof-dir>"
    fi
  else
    proof_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-blaxel-live-proof.XXXXXX")" ||
      classify_and_exit environment_blocked "could not create proof directory"
    chmod 700 "$proof_dir" || classify_and_exit environment_blocked "could not secure proof directory: <proof-dir>"
  fi
  export CRABBOX_BLAXEL_REDACT_PROOF_DIR="$proof_dir"
  export CRABBOX_BLAXEL_REDACT_BIN="$bin"
  export CRABBOX_BLAXEL_REDACT_WORKSPACE="${CRABBOX_BLAXEL_WORKSPACE:-${BL_WORKSPACE:-}}"
  export CRABBOX_BLAXEL_REDACT_API_URL="${CRABBOX_BLAXEL_API_URL:-}"
}

redact_stream() {
  perl -pe '
    BEGIN {
      @secrets = grep { length($_) } (
        $ENV{"CRABBOX_BLAXEL_API_KEY"} // "",
        $ENV{"BL_API_KEY"} // "",
        $ENV{"CRABBOX_BLAXEL_REDACT_WORKSPACE"} // "",
        $ENV{"CRABBOX_BLAXEL_REDACT_API_URL"} // "",
        $ENV{"CRABBOX_BLAXEL_REDACT_PROOF_DIR"} // "",
        $ENV{"CRABBOX_BLAXEL_REDACT_BIN"} // ""
      );
    }
    for my $secret (@secrets) {
      s/\Q$secret\E/<redacted>/g if length($secret);
    }
    s/Authorization:[^\n\r]+/Authorization: <redacted>/gi;
    s/X-Blaxel-Workspace:[^\n\r]+/X-Blaxel-Workspace: <redacted>/gi;
    s#https?://[^[:space:]'"'"'"]+#<url>#g;
    s#/(?:Users|home)/[^'"'"'"\n ]+#<local-home-path>#g;
    s#/tmp/crabbox-[^[:space:]'"'"'"]+#<local-temp-path>#g;
    s#(?:\b\d{1,3}\.){3}\d{1,3}\b#<ip>#g;
  '
}

classify_exit_code() {
  case "$1" in
    live_blaxel_smoke_passed) printf '0' ;;
    skipped|environment_blocked|quota_blocked) printf '0' ;;
    *) printf '1' ;;
  esac
}

classify_and_exit() {
  trap - ERR
  if [[ $classification_emitted -ne 0 ]]; then
    exit 1
  fi
  classification_emitted=1
  local classification="$1"
  local message="${2:-}"
  local redacted_message=""
  if [[ -n "$message" ]]; then
    redacted_message="$(printf '%s' "$message" | redact_stream)"
    printf 'classification=%s %s\n' "$classification" "$redacted_message"
  else
    printf 'classification=%s\n' "$classification"
  fi
  exit "$(classify_exit_code "$classification")"
}

classify_unexpected_failure() {
  local status="$1"
  local line="$2"
  classify_and_exit validation_failed "unexpected failure status=$status line=$line"
}

classify_output() {
  local output="$1"
  local lower
  lower="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *quota* || "$lower" == *capacity* || "$lower" == *"rate limit"* || "$lower" == *"too many requests"* || "$lower" == *"429"* || "$lower" == *"insufficient"* ]]; then
    printf 'quota_blocked'
    return
  fi
  if [[ "$lower" == *"api key"* || "$lower" == *unauthorized* || "$lower" == *forbidden* || "$lower" == *"connection refused"* || "$lower" == *"no such host"* || "$lower" == *timeout* || "$lower" == *tls* || "$lower" == *x509* || "$lower" == *workspace* ]]; then
    printf 'environment_blocked'
    return
  fi
  printf 'validation_failed'
}

secure_log_file() {
  local file="$1"
  rm -f -- "$file"
  : >"$file"
  chmod 600 "$file"
}

run_step() {
  local name="$1"
  local command_label="$2"
  shift 2
  prepare_proof_dir
  local raw="$proof_dir/${name}.raw.log"
  local redacted="$proof_dir/${name}.redacted.log"
  secure_log_file "$raw"
  secure_log_file "$redacted"
  trap - ERR
  set +e
  {
    printf '$'
    printf ' %q' "$@"
    printf '\n'
    "$@"
  } >"$raw" 2>&1
  local status=$?
  set -e
  trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
  redact_stream <"$raw" >"$redacted"
  if [[ $status -eq 0 ]]; then
    printf 'step=%s status=pass log=<proof-dir>/%s\n' "$name" "$(basename "$redacted")"
  else
    printf 'step=%s status=fail exit=%s log=<proof-dir>/%s\n' "$name" "$status" "$(basename "$redacted")"
    local classification
    classification="$(classify_output "$(cat "$redacted")")"
    classify_and_exit "$classification" "command=$command_label exit=$status $(cat "$redacted")"
  fi
}

capture_step_allow_failure() {
  local name="$1"
  shift
  prepare_proof_dir
  local raw="$proof_dir/${name}.raw.log"
  local redacted="$proof_dir/${name}.redacted.log"
  secure_log_file "$raw"
  secure_log_file "$redacted"
  trap - ERR
  set +e
  {
    printf '$'
    printf ' %q' "$@"
    printf '\n'
    "$@"
  } >"$raw" 2>&1
  local status=$?
  set -e
  trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
  redact_stream <"$raw" >"$redacted"
  printf '%s' "$status"
}

read_redacted_log() {
  local name="$1"
  cat "$proof_dir/${name}.redacted.log"
}

validate_json_contains_slug() {
  local file="$1"
  local description="$2"
  local status=0
  local output=""
  trap - ERR
  set +e
  output="$(CRABBOX_SMOKE_SLUG="$slug" node -e '
const fs = require("node:fs");
const slug = process.env.CRABBOX_SMOKE_SLUG;
const file = process.argv[1];
const text = fs.readFileSync(file, "utf8");
const jsonLine = text.split(/\r?\n/).find((line) => /^\s*[\[{]/.test(line));
if (!jsonLine) {
  console.error("no JSON payload found");
  process.exit(1);
}
let payload;
try {
  payload = JSON.parse(jsonLine);
} catch (error) {
  console.error(`invalid JSON: ${error.message}`);
  process.exit(1);
}
function hasSlug(value) {
  if (Array.isArray(value)) return value.some(hasSlug);
  if (value && typeof value === "object") {
    if (value.slug === slug || value.name === slug || value.id === slug || value.leaseId === slug) return true;
    if (value.labels && typeof value.labels === "object" && value.labels.slug === slug) return true;
    return Object.values(value).some(hasSlug);
  }
  return false;
}
if (!hasSlug(payload)) {
  console.error(`JSON did not include slug ${slug}`);
  process.exit(1);
}
' "$file" 2>&1)"
  status=$?
  set -e
  trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
  if [[ $status -ne 0 ]]; then
    classify_and_exit validation_failed "$description validation failed: $output"
  fi
}

cleanup() {
  if [[ "$cleanup_armed" -eq 1 && -n "$bin" && -x "$bin" && -n "$slug" ]]; then
    "$bin" stop --provider blaxel "$slug" >/dev/null 2>&1 || true
  fi
  if [[ -n "$smoke_root" ]]; then
    rm -rf -- "$smoke_root"
  fi
}
trap cleanup EXIT
trap 'classify_unexpected_failure "$?" "$LINENO"' ERR

if [[ ! -x "$bin" ]]; then
  classify_and_exit environment_blocked "missing crabbox binary; build with go build -trimpath -o bin/crabbox ./cmd/crabbox"
fi
if ! command -v perl >/dev/null 2>&1; then
  classify_and_exit environment_blocked "missing required tool: perl"
fi
if ! command -v node >/dev/null 2>&1; then
  classify_and_exit environment_blocked "missing required tool: node"
fi
if ! command -v git >/dev/null 2>&1; then
  classify_and_exit environment_blocked "missing required tool: git"
fi

if [[ -z "${CRABBOX_BLAXEL_API_KEY:-${BL_API_KEY:-}}" ]]; then
  classify_and_exit environment_blocked "missing CRABBOX_BLAXEL_API_KEY or BL_API_KEY"
fi

run_step preflight-doctor "doctor" "$bin" doctor --provider blaxel --json
run_step preflight-list "list" "$bin" list --provider blaxel --json

if [[ "$live" != "1" || ( "$live_providers" != *",blaxel,"* && "$live_providers" != *",all,"* ) ]]; then
  classify_and_exit skipped "live mutation requires CRABBOX_LIVE=1 and CRABBOX_LIVE_PROVIDERS=blaxel"
fi

smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-blaxel-smoke.XXXXXX")"
smoke_repo="$smoke_root/repo"
mkdir -p "$smoke_repo"
export XDG_STATE_HOME="$smoke_root/state"
export CRABBOX_BLAXEL_SMOKE_VALUE="forwarded-ok"

cd "$smoke_repo"
git init -q
git config user.email smoke@example.com
git config user.name "Crabbox Blaxel Smoke"
cat >.crabbox.yaml <<'EOF'
provider: blaxel
sync:
  delete: true
EOF
printf 'v1\n' >proof.txt
printf 'remove-me\n' >stale.txt
git add .crabbox.yaml proof.txt stale.txt
git commit -qm "test: seed Blaxel smoke fixture"

cleanup_armed=1
run_step live-run-success "run-success" "$bin" run --provider blaxel --keep --slug "$slug" --timing-json \
  --allow-env CRABBOX_BLAXEL_SMOKE_VALUE -- \
  /bin/sh -lc 'test "$(cat proof.txt)" = v1 && test -f stale.txt && test "$CRABBOX_BLAXEL_SMOKE_VALUE" = forwarded-ok && printf BLAXEL_SMOKE_V1_OK'
if ! grep -q 'BLAXEL_SMOKE_V1_OK' "$proof_dir/live-run-success.redacted.log"; then
  classify_and_exit validation_failed "initial run succeeded but marker was missing"
fi
if ! grep -q '"name":"blaxel_sync"' "$proof_dir/live-run-success.redacted.log"; then
  classify_and_exit validation_failed "initial run succeeded but timing JSON did not include blaxel_sync"
fi

trap - ERR
set +e
nonzero_status="$(capture_step_allow_failure live-run-nonzero "$bin" run --provider blaxel --id "$slug" -- /bin/sh -lc 'exit 17')"
set -e
trap 'classify_unexpected_failure "$?" "$LINENO"' ERR
if [[ "$nonzero_status" != "17" ]]; then
  classify_and_exit validation_failed "nonzero exit propagation expected 17 got $nonzero_status $(read_redacted_log live-run-nonzero)"
fi
printf 'step=live-run-nonzero status=pass exit=17 log=<proof-dir>/live-run-nonzero.redacted.log\n'

run_step live-status "status" "$bin" status --provider blaxel --id "$slug" --wait --json
validate_json_contains_slug "$proof_dir/live-status.raw.log" "status"

run_step live-list "list" "$bin" list --provider blaxel --json
validate_json_contains_slug "$proof_dir/live-list.raw.log" "list"

run_step live-stop "stop" "$bin" stop --provider blaxel "$slug"
cleanup_armed=0

run_step live-cleanup-dry-run "cleanup-dry-run" "$bin" cleanup --provider blaxel --dry-run

trap - EXIT
rm -rf -- "$smoke_root"
classify_and_exit live_blaxel_smoke_passed "slug=$slug cleanup=complete proof_dir=<proof-dir>"
