#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() {
  cat <<'EOF'
Run an opt-in, zero-residue Unikraft Cloud lifecycle proof.

The script is non-mutating unless both guards are present:
  CRABBOX_LIVE=1
  CRABBOX_LIVE_PROVIDERS contains unikraft-cloud, unikraftcloud, ukc, or all

Environment:
  CRABBOX_BIN                                      Crabbox binary (default: build ./bin/crabbox)
  CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_SLUG          Slug prefix (default: unikraft-cloud-live-smoke)
  CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR           Owner-only proof and isolated state directory
  CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_IMAGE         OCI image (default: nginx:latest)
  CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_MEMORY_MB     Memory in MiB (default: 256)
  CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_WAIT_TIMEOUT  Ready wait (default: 300s)
  CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_UNCERTAINTY_SECONDS
                                                    Ambiguous-create observation window (default: 35)
  CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_TIMEOUT_SECONDS
                                                    Overall emergency cleanup bound (default: 90)

Use the normal Unikraft Cloud token and endpoint variables. Tokens stay in the
environment; neither Crabbox nor the raw emergency-cleanup helper receives a
token on argv. The proof preserves the account's exact baseline UUID set.
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

classification_emitted=0
cleanup_armed=0
cleanup_running=0
cleanup_deadline=0
cleanup_signal_code=0
capture_deadline=0
warmup_succeeded=0
remote_seen=0
repo_root=""
bin=""
binary_provenance="provided_unverified"
proof_base=""
proof_dir=""
raw_helper=""
capture_runner=""
capture_limit_bytes=1048576
baseline_inventory=""
current_inventory=""
created_lease=""
created_uuid=""
expected_name=""
slug=""
last_output=""
last_rc=0
process_output=""
process_rc=0
raw_output=""
owned_uuid_result=""
raw_call_counter=0
active_pid=""
active_capture=""
provider_args=(--provider unikraft-cloud)

classification_exit_code() {
  case "$1" in
    live_unikraft_cloud_smoke_passed|environment_blocked|quota_blocked) printf '0' ;;
    *) printf '1' ;;
  esac
}

classify_and_exit() {
  trap - ERR
  if [[ "$classification_emitted" -ne 0 ]]; then
    exit 1
  fi
  classification_emitted=1
  local classification="$1"
  local reason="${2:-}"
  if [[ -n "$reason" ]]; then
    printf 'classification=%s reason=%s\n' "$classification" "$reason"
  else
    printf 'classification=%s\n' "$classification"
  fi
  exit "$(classification_exit_code "$classification")"
}

# shellcheck disable=SC2329 # Invoked by ERR traps.
unexpected_failure() {
  local rc="$1"
  local line="$2"
  classify_and_exit validation_failed "unexpected_failure_exit_${rc}_line_${line}"
}

process_group_alive() {
  kill -0 -- "-$1" 2>/dev/null
}

terminate_process_group() {
  local pgid="$1"
  local attempt
  kill -TERM -- "-$pgid" 2>/dev/null || kill -TERM "$pgid" 2>/dev/null || true
  for ((attempt = 0; attempt < 20; attempt++)); do
    if ! process_group_alive "$pgid" && ! kill -0 "$pgid" 2>/dev/null; then
      return
    fi
    sleep 0.05 || true
  done
  kill -KILL -- "-$pgid" 2>/dev/null || kill -KILL "$pgid" 2>/dev/null || true
}

# shellcheck disable=SC2329 # Invoked by INT/TERM/HUP traps.
handle_signal() {
  local signal="$1"
  local code="$2"
  trap - INT TERM HUP
  if [[ -n "$active_pid" ]]; then
    terminate_process_group "$active_pid"
    wait "$active_pid" 2>/dev/null || true
    active_pid=""
  fi
  printf 'classification=validation_failed reason=interrupted_by_%s\n' "$signal" >&2
  exit "$code"
}

# shellcheck disable=SC2329 # Installed only while the EXIT cleanup is active.
handle_cleanup_signal() {
  local signal="$1"
  local code="$2"
  cleanup_signal_code="$code"
  trap '' INT TERM HUP
  if [[ -n "$active_pid" ]]; then
    terminate_process_group "$active_pid"
  fi
  printf 'classification=validation_failed reason=cleanup_interrupted_by_%s_continuing\n' "$signal" >&2
  return 0
}

provider_selected() {
  local raw="${CRABBOX_LIVE_PROVIDERS:-}"
  local item
  local items=()
  IFS=',' read -r -a items <<<"$raw"
  for item in "${items[@]}"; do
    item="$(printf '%s' "$item" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"
    case "$item" in
      unikraft-cloud|unikraftcloud|ukc|all) return 0 ;;
    esac
  done
  return 1
}

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
  if [[ -n "${CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR:-}" ]]; then
    proof_base="$CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_DIR"
    if [[ -L "$proof_base" ]]; then
      classify_and_exit environment_blocked proof_dir_is_symlink
    fi
    if [[ -e "$proof_base" ]]; then
      if [[ ! -d "$proof_base" || ! -O "$proof_base" ]] || ! directory_is_private "$proof_base"; then
        classify_and_exit environment_blocked proof_dir_must_be_owner_owned_mode_700
      fi
    else
      mkdir -p "$proof_base" || classify_and_exit environment_blocked proof_dir_create_failed
      chmod 700 "$proof_base" || classify_and_exit environment_blocked proof_dir_chmod_failed
    fi
    if [[ -L "$proof_base" || ! -d "$proof_base" || ! -O "$proof_base" ]] || ! directory_is_private "$proof_base"; then
      classify_and_exit environment_blocked proof_dir_changed_during_setup
    fi
    proof_dir="$(mktemp -d "$proof_base/run.XXXXXX")" || classify_and_exit environment_blocked proof_run_dir_create_failed
  else
    proof_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-unikraft-cloud-live-proof.XXXXXX")" ||
      classify_and_exit environment_blocked proof_dir_create_failed
  fi
  if [[ -L "$proof_dir" || ! -d "$proof_dir" || ! -O "$proof_dir" ]]; then
    classify_and_exit environment_blocked proof_run_dir_invalid
  fi
  chmod 700 "$proof_dir" || classify_and_exit environment_blocked proof_dir_chmod_failed
  directory_is_private "$proof_dir" || classify_and_exit environment_blocked proof_run_dir_not_private
  baseline_inventory="$proof_dir/baseline-inventory.json"
  current_inventory="$proof_dir/current-inventory.json"
  export CRABBOX_UNIKRAFT_CLOUD_SMOKE_REDACT_DIR="$proof_dir"
  export CRABBOX_UNIKRAFT_CLOUD_SMOKE_REDACT_BIN="$bin"
}

redact_stream() {
  perl -pe '
    BEGIN {
      @values = grep { length($_) } (
        $ENV{"CRABBOX_UNIKRAFT_CLOUD_SMOKE_TOKEN"} // "",
        $ENV{"CRABBOX_UNIKRAFT_CLOUD_SMOKE_API_URL"} // "",
        $ENV{"CRABBOX_UNIKRAFT_CLOUD_SMOKE_REDACT_DIR"} // "",
        $ENV{"CRABBOX_UNIKRAFT_CLOUD_SMOKE_REDACT_BIN"} // ""
      );
    }
    for my $value (@values) {
      s/\Q$value\E/<redacted>/g if length($value);
    }
    s/Authorization:[^\n\r]+/Authorization: <redacted>/gi;
    s#/(?:Users|home)/[^'"'"'"\n ]+#<local-home-path>#g;
    s#/tmp/crabbox-[^[:space:]'"'"'"]+#<local-temp-path>#g;
  '
}

secure_file() {
  local file="$1"
  if [[ -e "$file" || -L "$file" ]]; then
    return 1
  fi
  (set -o noclobber; : >"$file") 2>/dev/null || return 1
  chmod 600 "$file" || return 1
}

write_capture_runner() {
  capture_runner="$proof_dir/capture-runner.py"
  if ! (set -o noclobber; cat >"$capture_runner" <<'PY'
#!/usr/bin/env python3
import os
import signal
import subprocess
import sys
import time


def main():
    capture, overflow, raw_limit, *command = sys.argv[1:]
    limit = int(raw_limit)
    os.setsid()
    try:
        child = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, close_fds=True)
    except OSError:
        with open(capture, "ab", buffering=0) as output:
            output.write(b"command launch failed\n")
        return 127

    overflowed = False
    written = 0
    with open(capture, "ab", buffering=0) as output:
        while True:
            chunk = child.stdout.read(65536)
            if not chunk:
                break
            remaining = limit - written
            if remaining > 0:
                output.write(chunk[:remaining])
                written += min(len(chunk), remaining)
            if len(chunk) > remaining:
                overflowed = True
                descriptor = os.open(overflow, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                os.close(descriptor)
                break

    if overflowed:
        try:
            child.terminate()
            child.wait(timeout=0.25)
        except (ProcessLookupError, subprocess.TimeoutExpired):
            try:
                child.kill()
            except ProcessLookupError:
                pass
        return 125

    returncode = child.wait()
    if returncode < 0:
        return 128 + (-returncode)
    return returncode


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception:
        raise SystemExit(125)
PY
  ); then
    classify_and_exit environment_blocked capture_runner_create_failed
  fi
  chmod 700 "$capture_runner" || classify_and_exit environment_blocked capture_runner_chmod_failed
}

capture_process() {
  local capture="$1"
  shift
  process_output=""
  process_rc=0
  if ! secure_file "$capture"; then
    process_output="could not create secure capture"
    process_rc=125
    return 125
  fi
  active_capture="$capture"
  local overflow="$capture.overflow"
  if [[ -e "$overflow" || -L "$overflow" ]]; then
    process_output="capture overflow marker collision"
    process_rc=125
    rm -f -- "$capture" || true
    active_capture=""
    return 125
  fi
  trap - ERR
  python3 "$capture_runner" "$capture" "$overflow" "$capture_limit_bytes" "$@" &
  active_pid=$!
  trap 'unexpected_failure "$?" "$LINENO"' ERR
  local timed_out=0
  while kill -0 "$active_pid" 2>/dev/null; do
    if [[ "$capture_deadline" -gt 0 && "$SECONDS" -ge "$capture_deadline" ]]; then
      timed_out=1
      terminate_process_group "$active_pid"
      break
    fi
    sleep 0.1 || true
  done
  trap - ERR
  set +e
  local completed_pgid="$active_pid"
  wait "$active_pid"
  process_rc=$?
  set -e
  trap 'unexpected_failure "$?" "$LINENO"' ERR
  if process_group_alive "$completed_pgid"; then
    terminate_process_group "$completed_pgid"
  fi
  active_pid=""
  if [[ "$timed_out" -ne 0 ]]; then
    process_rc=124
  fi
  if [[ -e "$overflow" ]]; then
    process_output="command output exceeded capture limit"
    process_rc=125
  elif ! process_output="$(cat "$capture")"; then
    process_output="could not read secure capture"
    process_rc=125
  fi
  if ! rm -f -- "$capture"; then
    process_output="could not remove secure capture"
    process_rc=125
  fi
  if [[ -e "$overflow" ]] && ! rm -f -- "$overflow"; then
    process_output="could not remove capture overflow marker"
    process_rc=125
  fi
  active_capture=""
  return "$process_rc"
}

capture_command() {
  local name="$1"
  shift
  local capture="$proof_dir/.${name}.$$.capture"
  local rc=0
  capture_process "$capture" "$@" || rc=$?
  local log="$proof_dir/${name}.redacted.log"
  if ! secure_file "$log"; then
    last_output="proof_log_create_failed"
    last_rc=125
    return 125
  fi
  if ! printf '%s\n' "$process_output" | redact_stream >"$log"; then
    last_output="proof_log_redaction_failed"
    last_rc=125
    return 125
  fi
  if ! last_output="$(cat "$log")"; then
    last_output="proof_log_read_failed"
    last_rc=125
    return 125
  fi
  last_rc="$rc"
  return "$rc"
}

run_step() {
  local name="$1"
  shift
  local rc=0
  capture_command "$name" "$@" || rc=$?
  if [[ "$rc" -eq 0 ]]; then
    printf 'step=%s status=pass log=<proof-dir>/%s.redacted.log\n' "$name" "$name"
  else
    printf 'step=%s status=fail exit=%s log=<proof-dir>/%s.redacted.log\n' "$name" "$rc" "$name"
  fi
  return "$rc"
}

capture_step() {
  local name="$1"
  shift
  local rc=0
  capture_command "$name" "$@" || rc=$?
  return "$rc"
}

deleted_status_proves_absence() {
  local log="$proof_dir/live-deleted-status.redacted.log"
  [[ -f "$log" ]] || return 1
  grep -Eiq 'unikraft-cloud API error status=404:|instance (was )?not found|no instance with (uuid|name)' "$log"
}

failure_classification() {
  local lower
  lower="$(printf '%s' "$last_output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *quota* || "$lower" == *capacity* || "$lower" == *"rate limit"* ||
    "$lower" == *"too many requests"* || "$lower" == *"429"* || "$lower" == *"insufficient"* ||
    "$lower" == *"payment"* || "$lower" == *"balance"* ]]; then
    printf 'quota_blocked'
    return
  fi
  if [[ "$lower" == *unauthorized* || "$lower" == *forbidden* || "$lower" == *"api key"* ||
    "$lower" == *token* || "$lower" == *"no such host"* || "$lower" == *timeout* ||
    "$lower" == *tls* || "$lower" == *x509* || "$lower" == *"connection refused"* ]]; then
    printf 'environment_blocked'
    return
  fi
  printf 'validation_failed'
}

require_step() {
  local name="$1"
  shift
  if run_step "$name" "$@"; then
    return
  fi
  local classification
  classification="$(failure_classification)"
  classify_and_exit "$classification" "${name}_failed_exit_${last_rc}"
}

write_raw_helper() {
  if [[ -n "${CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER:-}" ]]; then
    raw_helper="$CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_RAW_HELPER"
    [[ -x "$raw_helper" ]] || classify_and_exit environment_blocked raw_helper_not_executable
    return
  fi
  raw_helper="$proof_dir/unikraft-cloud-raw-helper.py"
  if ! (set -o noclobber; cat >"$raw_helper" <<'PY'
#!/usr/bin/env python3
import json
import math
import os
import re
import socket
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request

UUID_RE = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
TOKEN = os.environ.get("CRABBOX_UNIKRAFT_CLOUD_SMOKE_TOKEN", "")
RAW_URL = os.environ.get("CRABBOX_UNIKRAFT_CLOUD_SMOKE_API_URL", "")
try:
    REQUEST_TIMEOUT = float(os.environ.get("CRABBOX_UNIKRAFT_CLOUD_SMOKE_HTTP_TIMEOUT", "5"))
except ValueError:
    REQUEST_TIMEOUT = 0


def fail(message):
    print(f"raw helper: {message}", file=sys.stderr)
    raise SystemExit(2)


parts = urllib.parse.urlsplit(RAW_URL)
if not parts.scheme or not parts.hostname or parts.username or parts.password or parts.query or parts.fragment:
    fail("invalid API endpoint")
if parts.path not in ("", "/"):
    fail("API endpoint must identify the root")
loopback = parts.hostname in ("localhost", "127.0.0.1", "::1")
if parts.scheme != "https" and not (parts.scheme == "http" and loopback):
    fail("API endpoint must use HTTPS")
if not TOKEN:
    fail("missing token")
if not math.isfinite(REQUEST_TIMEOUT) or REQUEST_TIMEOUT < 1 or REQUEST_TIMEOUT > 30:
    fail("invalid request timeout")
BASE_URL = urllib.parse.urlunsplit((parts.scheme, parts.netloc, "", "", ""))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


def request(method, api_path, body=None):
    data = None if body is None else json.dumps(body, separators=(",", ":")).encode()
    req = urllib.request.Request(
        BASE_URL + api_path,
        data=data,
        method=method,
        headers={
            "Authorization": "Bearer " + TOKEN,
            "Accept": "application/json",
            **({"Content-Type": "application/json"} if data is not None else {}),
        },
    )
    try:
        with OPENER.open(req, timeout=REQUEST_TIMEOUT) as response:
            raw = response.read(16 * 1024 * 1024 + 1)
            code = response.status
            content_type = response.headers.get_content_type()
    except urllib.error.HTTPError as error:
        raw = error.read(16 * 1024 * 1024 + 1)
        code = error.code
        content_type = error.headers.get_content_type()
    except (urllib.error.URLError, TimeoutError, socket.timeout):
        fail("request failed")
    if len(raw) > 16 * 1024 * 1024:
        fail("response too large")
    if content_type != "application/json":
        fail("response was not JSON")
    try:
        payload = json.loads(raw)
    except (json.JSONDecodeError, UnicodeDecodeError):
        fail("invalid JSON response")
    if not isinstance(payload, dict):
        fail("invalid response envelope")
    return code, payload


def instances(payload):
    data = payload.get("data")
    if not isinstance(data, dict) or not isinstance(data.get("instances"), list):
        fail("missing instances array")
    return data["instances"]


def validate_success(payload):
    if str(payload.get("status", "")).strip().lower() != "success":
        fail("operation did not return success")
    if payload.get("errors"):
        fail("operation returned errors")
    result = instances(payload)
    for item in result:
        if not isinstance(item, dict):
            fail("invalid instance item")
        item_status = str(item.get("status", "")).strip().lower()
        if item.get("error") not in (None, 0) or item_status == "error":
            fail("instance item failed")
        if item_status not in ("", "success"):
            fail("invalid instance item status")
    return result


def read_inventory():
    code, payload = request("GET", "/v1/instances")
    if code < 200 or code >= 300:
        fail("inventory request failed")
    result = []
    seen = set()
    for item in validate_success(payload):
        uuid = str(item.get("uuid", "")).strip()
        if not UUID_RE.fullmatch(uuid) or uuid.lower() in seen:
            fail("inventory returned invalid or duplicate UUID")
        seen.add(uuid.lower())
        result.append({"name": str(item.get("name", "")), "uuid": uuid.lower()})
    return sorted(result, key=lambda item: item["uuid"])


def load_inventory(path):
    try:
        with open(path, encoding="utf-8") as handle:
            value = json.load(handle)
    except (OSError, json.JSONDecodeError):
        fail("could not read inventory proof")
    if not isinstance(value, list):
        fail("invalid inventory proof")
    return value


def uuid_set(value):
    return {str(item.get("uuid", "")).lower() for item in value if isinstance(item, dict)}


def error8_absent(identifier):
    code, payload = request("GET", "/v1/instances/" + urllib.parse.quote(identifier, safe=""))
    envelope_status = str(payload.get("status", "")).strip().lower()
    if code == 404:
        return envelope_status == "error"
    if code < 200 or code >= 300:
        return False
    try:
        result = instances(payload)
    except SystemExit:
        return False
    if len(result) != 1 or not isinstance(result[0], dict):
        return False
    item = result[0]
    returned_uuid = str(item.get("uuid", "")).strip()
    returned_name = str(item.get("name", "")).strip()
    if UUID_RE.fullmatch(identifier):
        if returned_uuid and (
            not UUID_RE.fullmatch(returned_uuid)
            or returned_uuid.lower() != identifier.lower()
        ):
            return False
    else:
        if returned_name and returned_name != identifier:
            return False
        if returned_uuid and (
            not UUID_RE.fullmatch(returned_uuid)
            or not returned_name
        ):
            return False
    return (
        item.get("error") == 8
        and str(item.get("status", "")).strip().lower() == "error"
    )


mode = sys.argv[1] if len(sys.argv) > 1 else ""
if mode == "validate" and len(sys.argv) == 2:
    pass
elif mode == "inventory" and len(sys.argv) == 3:
    destination = sys.argv[2]
    value = read_inventory()
    descriptor, temporary = tempfile.mkstemp(prefix=".inventory.", dir=os.path.dirname(destination))
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(value, handle, separators=(",", ":"), sort_keys=True)
            handle.write("\n")
        os.replace(temporary, destination)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
elif mode == "compare" and len(sys.argv) == 4:
    raise SystemExit(0 if uuid_set(load_inventory(sys.argv[2])) == uuid_set(load_inventory(sys.argv[3])) else 1)
elif mode == "owned" and len(sys.argv) == 5:
    baseline = uuid_set(load_inventory(sys.argv[2]))
    expected_name = sys.argv[4]
    matches = [
        item for item in load_inventory(sys.argv[3])
        if isinstance(item, dict) and str(item.get("uuid", "")).lower() not in baseline and item.get("name") == expected_name
    ]
    if len(matches) == 0:
        raise SystemExit(1)
    if len(matches) != 1:
        fail("ambiguous owned inventory")
    print(str(matches[0]["uuid"]).lower())
elif mode == "claim" and len(sys.argv) == 7:
    payload = load_inventory(sys.argv[2])
    slug = sys.argv[3]
    matches = []
    for item in payload:
        if not isinstance(item, dict):
            continue
        labels = item.get("labels") if isinstance(item.get("labels"), dict) else {}
        if labels.get("slug") == slug:
            matches.append(item)
    if len(matches) != 1:
        raise SystemExit(1)
    item = matches[0]
    labels = item.get("labels") if isinstance(item.get("labels"), dict) else {}
    values = (
        str(labels.get("lease", "")),
        str(item.get("CloudID", item.get("cloudId", ""))),
        str(item.get("Name", item.get("name", ""))),
    )
    destinations = sys.argv[4:7]
    opened = []
    created = []
    try:
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        for destination in destinations:
            descriptor = os.open(destination, flags, 0o600)
            opened.append((descriptor, destination))
            created.append(destination)
        for (descriptor, _), value in zip(opened, values):
            data = value.encode()
            while data:
                data = data[os.write(descriptor, data):]
            os.close(descriptor)
        opened.clear()
    except OSError:
        for descriptor, _ in opened:
            try:
                os.close(descriptor)
            except OSError:
                pass
        for destination in created:
            try:
                os.unlink(destination)
            except FileNotFoundError:
                pass
        fail("could not write claim proof")
elif mode == "delete" and len(sys.argv) == 4:
    uuid = sys.argv[2].lower()
    expected_name = sys.argv[3]
    if not UUID_RE.fullmatch(uuid):
        fail("invalid delete UUID")
    if not re.fullmatch(r"crabbox-ukc-[0-9a-f]{12}", expected_name):
        fail("invalid delete name")
    matches = [item for item in read_inventory() if item["uuid"] == uuid and item["name"] == expected_name]
    if len(matches) != 1:
        fail("refusing cleanup without exact UUID and name ownership")
    code, payload = request("DELETE", "/v1/instances", [{"uuid": uuid}])
    if code < 200 or code >= 300:
        fail("delete request failed")
    result = validate_success(payload)
    if len(result) != 1 or str(result[0].get("uuid", "")).lower() != uuid or str(result[0].get("status", "")).lower() != "success":
        fail("delete did not explicitly accept the exact UUID")
    returned_name = str(result[0].get("name", result[0].get("Name", ""))).strip()
    if returned_name and returned_name != expected_name:
        fail("delete response changed the owned name")
elif mode == "absent" and len(sys.argv) == 3:
    uuid = sys.argv[2].lower()
    if not UUID_RE.fullmatch(uuid):
        fail("invalid absence UUID")
    if not error8_absent(uuid):
        raise SystemExit(1)
    if uuid in uuid_set(read_inventory()):
        raise SystemExit(1)
    if not error8_absent(uuid):
        raise SystemExit(1)
elif mode == "absent-name" and len(sys.argv) == 3:
    name = sys.argv[2]
    if not re.fullmatch(r"crabbox-ukc-[0-9a-fA-F]{12}", name):
        fail("invalid absence name")
    if not error8_absent(name):
        raise SystemExit(1)
    if any(item["name"] == name for item in read_inventory()):
        raise SystemExit(1)
    if not error8_absent(name):
        raise SystemExit(1)
else:
    fail("invalid invocation")
PY
  ); then
    classify_and_exit environment_blocked raw_helper_create_failed
  fi
  chmod 700 "$raw_helper" || classify_and_exit environment_blocked raw_helper_chmod_failed
}

remaining_cleanup_seconds() {
  local per_call="${CRABBOX_UNIKRAFT_CLOUD_SMOKE_HTTP_TIMEOUT:-5}"
  if [[ ! "$per_call" =~ ^[1-5]$ ]]; then
    per_call=5
  fi
  if [[ "$cleanup_deadline" -le 0 ]]; then
    printf '%s' "$per_call"
    return
  fi
  local remaining=$((cleanup_deadline - SECONDS))
  if [[ "$remaining" -le 0 ]]; then
    printf '0'
  elif [[ "$remaining" -gt "$per_call" ]]; then
    printf '%s' "$per_call"
  else
    printf '%s' "$remaining"
  fi
}

raw_call() {
  raw_call_counter=$((raw_call_counter + 1))
  local capture="$proof_dir/.raw-call-${raw_call_counter}.capture"
  local timeout
  timeout="$(remaining_cleanup_seconds)"
  if [[ "$timeout" -le 0 ]]; then
    raw_output="cleanup deadline expired"
    return 124
  fi
  local previous_timeout_set=0
  local previous_timeout=""
  if [[ -n "${CRABBOX_UNIKRAFT_CLOUD_SMOKE_HTTP_TIMEOUT+x}" ]]; then
    previous_timeout_set=1
    previous_timeout="$CRABBOX_UNIKRAFT_CLOUD_SMOKE_HTTP_TIMEOUT"
  fi
  export CRABBOX_UNIKRAFT_CLOUD_SMOKE_HTTP_TIMEOUT="$timeout"
  local previous_deadline="$capture_deadline"
  capture_deadline=$((SECONDS + timeout))
  if [[ "$cleanup_deadline" -gt 0 && "$capture_deadline" -gt "$cleanup_deadline" ]]; then
    capture_deadline="$cleanup_deadline"
  fi
  local rc=0
  capture_process "$capture" "$raw_helper" "$@" || rc=$?
  capture_deadline="$previous_deadline"
  if [[ "$previous_timeout_set" -ne 0 ]]; then
    export CRABBOX_UNIKRAFT_CLOUD_SMOKE_HTTP_TIMEOUT="$previous_timeout"
  else
    unset CRABBOX_UNIKRAFT_CLOUD_SMOKE_HTTP_TIMEOUT
  fi
  raw_output="$process_output"
  return "$rc"
}

raw_inventory() {
  local destination="$1"
  raw_call inventory "$destination"
}

inventory_restored() {
  raw_call compare "$baseline_inventory" "$current_inventory"
}

strong_absence() {
  [[ -n "$created_uuid" ]] || return 1
  raw_call absent "$created_uuid"
}

# shellcheck disable=SC2329 # Reached through EXIT-trap uncertainty cleanup.
strong_name_absence() {
  [[ -n "$expected_name" ]] || return 1
  raw_call absent-name "$expected_name"
}

find_owned_uuid() {
  [[ -n "$expected_name" ]] || return 1
  owned_uuid_result=""
  local rc=0
  raw_call owned "$baseline_inventory" "$current_inventory" "$expected_name" || rc=$?
  if [[ "$rc" -eq 0 ]]; then
    owned_uuid_result="$raw_output"
    return 0
  fi
  return "$rc"
}

delete_owned_uuid() {
  local uuid="$1"
  raw_call delete "$uuid" "$expected_name"
}

validate_lifecycle_view() {
  local file="$1"
  local kind="$2"
  python3 - "$file" "$kind" "$created_uuid" "$slug" "$created_lease" "$expected_name" <<'PY'
import json
import sys

file, kind, uuid, slug, lease, expected_name = sys.argv[1:]
try:
    with open(file, encoding="utf-8") as handle:
        payload = json.load(handle)
except (OSError, json.JSONDecodeError):
    raise SystemExit(1)

if kind == "status":
    valid = (
        isinstance(payload, dict)
        and payload.get("provider") == "unikraft-cloud"
        and payload.get("id") == lease
        and payload.get("serverId") == uuid
        and payload.get("slug") == slug
        and payload.get("ready") is True
    )
    raise SystemExit(0 if valid else 1)

if kind == "list" and isinstance(payload, list):
    matches = []
    for item in payload:
        if not isinstance(item, dict) or item.get("CloudID") != uuid:
            continue
        labels = item.get("labels") if isinstance(item.get("labels"), dict) else {}
        provider = item.get("Provider", item.get("provider"))
        if (
            provider == "unikraft-cloud"
            and item.get("name") == expected_name
            and labels.get("slug") == slug
            and labels.get("lease") == lease
        ):
            matches.append(item)
    raise SystemExit(0 if len(matches) == 1 else 1)

raise SystemExit(1)
PY
}

extract_created_identity() {
  local source="$1"
  if [[ -z "$created_lease" && "$source" =~ (ukc_[0-9a-fA-F]{12}) ]]; then
    created_lease="${BASH_REMATCH[1]}"
  fi
  if [[ -z "$created_uuid" && "$source" =~ instance=([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}) ]]; then
    created_uuid="$(printf '%s' "${BASH_REMATCH[1]}" | tr '[:upper:]' '[:lower:]')"
  fi
  if [[ -n "$created_lease" ]]; then
    expected_name="crabbox-${created_lease//_/-}"
  fi
}

# shellcheck disable=SC2329 # Invoked from the EXIT-trap cleanup.
discover_claim_identity() {
  local list_log="$proof_dir/cleanup-list.redacted.log"
  local lease_file="$proof_dir/discovered-lease"
  local uuid_file="$proof_dir/discovered-uuid"
  local name_file="$proof_dir/discovered-name"
  if ! run_step cleanup-list "$bin" list "${provider_args[@]}" --json >/dev/null; then
    return 1
  fi
  if ! raw_call claim "$list_log" "$slug" "$lease_file" "$uuid_file" "$name_file" >/dev/null 2>&1; then
    return 1
  fi
  if [[ -z "$created_lease" ]]; then
    if ! created_lease="$(cat "$lease_file")"; then
      return 1
    fi
  fi
  if [[ -z "$created_uuid" ]]; then
    if ! created_uuid="$(cat "$uuid_file")"; then
      return 1
    fi
  fi
  if [[ "$created_lease" =~ ^ukc_[0-9a-fA-F]{12}$ ]]; then
    expected_name="crabbox-${created_lease//_/-}"
  fi
}

cleanup_poll_delay() {
  local delay="${CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_POLL_SECONDS:-2}"
  if [[ ! "$delay" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
    delay=2
  fi
  printf '%s' "$delay"
}

poll_pause() {
  if [[ "$cleanup_deadline" -gt 0 && "$SECONDS" -ge "$cleanup_deadline" ]]; then
    return 1
  fi
  local delay
  delay="$(cleanup_poll_delay)"
  local remaining=5
  if [[ "$cleanup_deadline" -gt 0 ]]; then
    remaining=$((cleanup_deadline - SECONDS))
  fi
  if [[ "$remaining" -le 0 ]]; then
    return 1
  fi
  local bounded_delay
  bounded_delay="$(awk -v delay="$delay" -v remaining="$remaining" 'BEGIN {
    if (delay > 5) delay = 5
    if (delay > remaining) delay = remaining
    if (delay < 0) delay = 0
    printf "%.3f", delay
  }')" || return 1
  sleep "$bounded_delay" || true
}

known_outcome_cleanup() {
  while [[ "$cleanup_deadline" -eq 0 || "$SECONDS" -lt "$cleanup_deadline" ]]; do
    if raw_inventory "$current_inventory" >/dev/null 2>&1; then
      if inventory_restored >/dev/null 2>&1 && strong_absence >/dev/null 2>&1; then
        if [[ "$warmup_succeeded" -eq 0 || "$remote_seen" -ne 0 ]]; then
          return 0
        fi
      fi
      local owned_uuid=""
      local owned_rc=0
      find_owned_uuid 2>/dev/null || owned_rc=$?
      owned_uuid="$owned_uuid_result"
      if [[ "$owned_rc" -eq 0 && -n "$owned_uuid" ]]; then
        if [[ -n "$created_uuid" && "$created_uuid" != "$owned_uuid" ]]; then
          return 1
        fi
        created_uuid="$owned_uuid"
        remote_seen=1
        if delete_owned_uuid "$created_uuid" >/dev/null 2>&1; then
          continue
        fi
      fi
    fi
    poll_pause || break
  done
  return 1
}

# shellcheck disable=SC2329 # Reached through EXIT-trap cleanup.
uncertain_outcome_cleanup() {
  local uncertainty="${CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_UNCERTAINTY_SECONDS:-35}"
  if [[ ! "$uncertainty" =~ ^[1-9][0-9]*$ || "$uncertainty" -gt 60 ]]; then
    uncertainty=35
  fi
  local uncertainty_end=$((SECONDS + uncertainty))
  if [[ "$cleanup_deadline" -gt 0 && "$uncertainty_end" -gt "$cleanup_deadline" ]]; then
    uncertainty_end="$cleanup_deadline"
  fi
  local absence_proofs=0
  local last_absent=0
  while [[ "$SECONDS" -lt "$uncertainty_end" ]]; do
    last_absent=0
    if raw_inventory "$current_inventory" >/dev/null 2>&1; then
      local owned_uuid=""
      local owned_rc=0
      find_owned_uuid 2>/dev/null || owned_rc=$?
      owned_uuid="$owned_uuid_result"
      if [[ "$owned_rc" -eq 0 && -n "$owned_uuid" ]]; then
        if [[ -n "$created_uuid" && "$created_uuid" != "$owned_uuid" ]]; then
          return 1
        fi
        created_uuid="$owned_uuid"
        remote_seen=1
        if delete_owned_uuid "$created_uuid" >/dev/null 2>&1; then
          continue
        fi
      fi
      if [[ "$remote_seen" -ne 0 && -n "$created_uuid" ]]; then
        if inventory_restored >/dev/null 2>&1 && strong_absence >/dev/null 2>&1; then
          return 0
        fi
      elif inventory_restored >/dev/null 2>&1 && strong_name_absence >/dev/null 2>&1; then
        absence_proofs=$((absence_proofs + 1))
        last_absent=1
      fi
    fi
    poll_pause || break
  done
  if [[ "$remote_seen" -ne 0 && -n "$created_uuid" ]]; then
    known_outcome_cleanup
    return $?
  fi
  if [[ "$absence_proofs" -ge 2 && "$last_absent" -eq 1 ]] &&
    raw_inventory "$current_inventory" >/dev/null 2>&1 &&
    inventory_restored >/dev/null 2>&1 && strong_name_absence >/dev/null 2>&1; then
    return 0
  fi
  return 1
}

# shellcheck disable=SC2329 # Reached through EXIT-trap cleanup.
recover_interrupted_capture() {
  if [[ -z "$active_capture" || ! -f "$active_capture" ]]; then
    return 0
  fi
  local capture="$active_capture"
  local overflow="$active_capture.overflow"
  local recovered=""
  local log="$proof_dir/interrupted-command.redacted.log"
  local log_created=0
  local recovered_ok=1
  if ! recovered="$(cat "$capture")"; then
    recovered_ok=0
  elif ! secure_file "$log"; then
    recovered_ok=0
  else
    log_created=1
    if ! printf '%s\n' "$recovered" | redact_stream >"$log" ||
      ! last_output="$(cat "$log")"; then
      recovered_ok=0
    fi
  fi
  if [[ "$recovered_ok" -eq 0 && "$log_created" -ne 0 ]]; then
    rm -f -- "$log" || true
  fi
  if ! rm -f -- "$capture"; then
    recovered_ok=0
  fi
  if [[ -e "$overflow" ]] && ! rm -f -- "$overflow"; then
    recovered_ok=0
  fi
  active_capture=""
  [[ "$recovered_ok" -ne 0 ]]
}

# shellcheck disable=SC2329 # Reached through EXIT-trap cleanup.
set_cleanup_command_slice() {
  local seconds="$1"
  local candidate=$((SECONDS + seconds))
  if [[ "$candidate" -gt "$cleanup_deadline" ]]; then
    candidate="$cleanup_deadline"
  fi
  capture_deadline="$candidate"
}

# shellcheck disable=SC2329 # Invoked by the EXIT trap.
cleanup() {
  local original_rc=$?
  trap - EXIT ERR
  trap 'handle_cleanup_signal INT 130' INT
  trap 'handle_cleanup_signal TERM 143' TERM
  trap 'handle_cleanup_signal HUP 129' HUP
  if [[ "$cleanup_running" -ne 0 ]]; then
    exit "$original_rc"
  fi
  cleanup_running=1
  if ! recover_interrupted_capture; then
    original_rc=1
  fi
  if [[ "$cleanup_armed" -eq 1 ]]; then
    local timeout="${CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_CLEANUP_TIMEOUT_SECONDS:-90}"
    if [[ ! "$timeout" =~ ^[1-9][0-9]*$ || "$timeout" -lt 10 || "$timeout" -gt 300 ]]; then
      timeout=90
    fi
    cleanup_deadline=$((SECONDS + timeout))
    extract_created_identity "$last_output"
    set_cleanup_command_slice 10
    discover_claim_identity || true
    if raw_inventory "$current_inventory" >/dev/null 2>&1; then
      local observed_uuid=""
      local observed_rc=0
      find_owned_uuid 2>/dev/null || observed_rc=$?
      observed_uuid="$owned_uuid_result"
      if [[ "$observed_rc" -eq 0 && -n "$observed_uuid" ]]; then
        if [[ -z "$created_uuid" || "$created_uuid" == "$observed_uuid" ]]; then
          created_uuid="$observed_uuid"
          remote_seen=1
        fi
      fi
    fi
    local identifier="$created_lease"
    if [[ -z "$identifier" ]]; then
      identifier="$slug"
    fi
    if [[ -n "$bin" && -x "$bin" && -n "$identifier" ]]; then
      set_cleanup_command_slice 15
      run_step cleanup-stop "$bin" stop "${provider_args[@]}" "$identifier" >/dev/null || true
    fi
    capture_deadline=0
    local cleaned=1
    if [[ "$warmup_succeeded" -ne 0 || "$remote_seen" -ne 0 ]]; then
      known_outcome_cleanup || cleaned=0
    else
      uncertain_outcome_cleanup || cleaned=0
    fi
    if [[ "$cleaned" -ne 0 ]]; then
      cleanup_armed=0
    else
      printf 'classification=cleanup_failed reason=exact_baseline_not_restored proof_dir=<proof-dir>\n' >&2
      original_rc=1
    fi
  fi
  if [[ "$cleanup_signal_code" -ne 0 && "$original_rc" -eq 0 ]]; then
    original_rc="$cleanup_signal_code"
  fi
  exit "$original_rc"
}

trap cleanup EXIT
trap 'unexpected_failure "$?" "$LINENO"' ERR
trap 'handle_signal INT 130' INT
trap 'handle_signal TERM 143' TERM
trap 'handle_signal HUP 129' HUP

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  classify_and_exit environment_blocked CRABBOX_LIVE_not_enabled
fi
if ! provider_selected; then
  classify_and_exit environment_blocked provider_not_selected
fi

token="${CRABBOX_UNIKRAFT_CLOUD_API_KEY:-${UNIKRAFT_CLOUD_API_KEY:-${UKC_API_KEY:-${UKC_TOKEN:-}}}}"
if [[ -z "$token" ]]; then
  classify_and_exit environment_blocked unikraft_cloud_token_missing
fi
if ! command -v python3 >/dev/null 2>&1; then
  classify_and_exit environment_blocked python3_missing
fi
if ! command -v perl >/dev/null 2>&1; then
  classify_and_exit environment_blocked perl_missing
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
bin="${CRABBOX_BIN:-$repo_root/bin/crabbox}"
if [[ "$bin" != /* && ! "$bin" =~ ^[A-Za-z]:[\\/] ]]; then
  bin="$repo_root/$bin"
fi
if [[ -z "${CRABBOX_BIN:-}" ]]; then
  mkdir -p "$(dirname "$bin")"
  go build -trimpath -o "$bin" ./cmd/crabbox || classify_and_exit environment_blocked crabbox_build_failed
  binary_provenance="current_tree_build"
elif [[ ! -x "$bin" ]]; then
  classify_and_exit environment_blocked crabbox_binary_missing_or_not_executable
fi

metro="${CRABBOX_UNIKRAFT_CLOUD_METRO:-${UNIKRAFT_CLOUD_METRO:-${UKC_METRO:-fra}}}"
api_url="${CRABBOX_UNIKRAFT_CLOUD_API_URL:-${UNIKRAFT_CLOUD_API_URL:-}}"
if [[ -z "$api_url" ]]; then
  if [[ ! "$metro" =~ ^[a-z][a-z0-9]{1,15}$ ]]; then
    classify_and_exit environment_blocked invalid_unikraft_cloud_metro
  fi
  api_url="https://api.${metro}.unikraft.cloud"
fi
image="${CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_IMAGE:-${CRABBOX_UNIKRAFT_CLOUD_IMAGE:-${UNIKRAFT_CLOUD_IMAGE:-nginx:latest}}}"
memory_mb="${CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_MEMORY_MB:-256}"
wait_timeout="${CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_WAIT_TIMEOUT:-300s}"
if [[ -z "$image" ]]; then
  classify_and_exit environment_blocked unikraft_cloud_image_missing
fi
if [[ ! "$memory_mb" =~ ^[0-9]+$ ]]; then
  classify_and_exit environment_blocked invalid_unikraft_cloud_memory
fi

slug_prefix="${CRABBOX_UNIKRAFT_CLOUD_LIVE_SMOKE_SLUG:-unikraft-cloud-live-smoke}"
slug_prefix="$(printf '%s' "$slug_prefix" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//')"
if [[ -z "$slug_prefix" ]]; then
  slug_prefix="unikraft-cloud-live-smoke"
fi
nonce="$(od -An -N6 -tx1 /dev/urandom 2>/dev/null | tr -d '[:space:]')"
if [[ ! "$nonce" =~ ^[0-9a-f]{12}$ ]]; then
  classify_and_exit environment_blocked random_nonce_failed
fi
# Requested Crabbox slugs are capped at 41 characters. The timestamp, nonce,
# and separators consume 28, leaving 13 for the readable prefix.
slug_prefix="${slug_prefix:0:13}"
slug_prefix="${slug_prefix%-}"
slug="$slug_prefix-$(date -u +%Y%m%d%H%M%S)-$nonce"

export CRABBOX_UNIKRAFT_CLOUD_SMOKE_TOKEN="$token"
export CRABBOX_UNIKRAFT_CLOUD_SMOKE_API_URL="$api_url"
# Pin the normal provider variables for child Crabbox processes. This makes the
# dedicated smoke override authoritative even when the invoking shell already
# exported a different provider image or endpoint.
export CRABBOX_UNIKRAFT_CLOUD_API_URL="$api_url"
export CRABBOX_UNIKRAFT_CLOUD_METRO="$metro"
export CRABBOX_UNIKRAFT_CLOUD_IMAGE="$image"
prepare_proof_dir
write_capture_runner
write_raw_helper
if ! raw_call validate >/dev/null 2>&1; then
  classify_and_exit environment_blocked invalid_unikraft_cloud_api_url
fi

export XDG_STATE_HOME="$proof_dir/state"
export XDG_CONFIG_HOME="$proof_dir/config"
export CRABBOX_CONFIG="$proof_dir/crabbox.yaml"
export CRABBOX_COORDINATOR=
mkdir -p "$XDG_STATE_HOME" "$XDG_CONFIG_HOME"
chmod 700 "$XDG_STATE_HOME" "$XDG_CONFIG_HOME"
if ! CRABBOX_UNIKRAFT_CLOUD_SMOKE_CONFIG="$CRABBOX_CONFIG" \
CRABBOX_UNIKRAFT_CLOUD_SMOKE_METRO="$metro" \
CRABBOX_UNIKRAFT_CLOUD_SMOKE_IMAGE="$image" \
CRABBOX_UNIKRAFT_CLOUD_SMOKE_MEMORY="$memory_mb" \
python3 - <<'PY'
import json
import os

destination = os.environ["CRABBOX_UNIKRAFT_CLOUD_SMOKE_CONFIG"]
with open(destination, "x", encoding="utf-8") as handle:
    json.dump({
        "provider": "unikraft-cloud",
        "target": "linux",
        "unikraftCloud": {
            "apiUrl": os.environ["CRABBOX_UNIKRAFT_CLOUD_SMOKE_API_URL"],
            "metro": os.environ["CRABBOX_UNIKRAFT_CLOUD_SMOKE_METRO"],
            "image": os.environ["CRABBOX_UNIKRAFT_CLOUD_SMOKE_IMAGE"],
            "memoryMB": int(os.environ["CRABBOX_UNIKRAFT_CLOUD_SMOKE_MEMORY"]),
        },
    }, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
then
  classify_and_exit environment_blocked smoke_config_create_failed
fi
chmod 600 "$CRABBOX_CONFIG" || classify_and_exit environment_blocked smoke_config_chmod_failed

require_step preflight-doctor "$bin" doctor "${provider_args[@]}"
require_step preflight-list-all "$bin" list "${provider_args[@]}" --all --json
if ! raw_inventory "$baseline_inventory" >/dev/null 2>&1; then
  classify_and_exit environment_blocked baseline_raw_inventory_failed
fi

cleanup_armed=1
if run_step live-warmup "$bin" warmup "${provider_args[@]}" --slug "$slug" --keep; then
  warmup_succeeded=1
  extract_created_identity "$last_output"
else
  extract_created_identity "$last_output"
  classification="$(failure_classification)"
  classify_and_exit "$classification" "warmup_failed_exit_${last_rc}"
fi
if [[ -z "$created_lease" || -z "$created_uuid" || -z "$expected_name" ]]; then
  classify_and_exit validation_failed warmup_identity_missing
fi
if ! raw_inventory "$current_inventory" >/dev/null 2>&1; then
  classify_and_exit validation_failed post_create_raw_inventory_failed
fi
owned_rc=0
find_owned_uuid 2>/dev/null || owned_rc=$?
owned_uuid="$owned_uuid_result"
if [[ "$owned_rc" -ne 0 || "$owned_uuid" != "$created_uuid" ]]; then
  classify_and_exit validation_failed created_instance_raw_identity_mismatch
fi
remote_seen=1

require_step live-status "$bin" status "${provider_args[@]}" --id "$created_lease" --wait --wait-timeout "$wait_timeout" --json
if ! validate_lifecycle_view "$proof_dir/live-status.redacted.log" status; then
  classify_and_exit validation_failed status_identity_or_readiness_missing
fi
require_step live-list "$bin" list "${provider_args[@]}" --json
if ! validate_lifecycle_view "$proof_dir/live-list.redacted.log" list; then
  classify_and_exit validation_failed claimed_list_identity_missing
fi
require_step live-list-all "$bin" list "${provider_args[@]}" --all --json
if ! validate_lifecycle_view "$proof_dir/live-list-all.redacted.log" list; then
  classify_and_exit validation_failed all_list_identity_missing
fi

require_step live-stop "$bin" stop "${provider_args[@]}" "$created_lease"
if capture_step live-deleted-status "$bin" status "${provider_args[@]}" --id "$created_uuid" --json; then
  classify_and_exit validation_failed deleted_instance_still_resolves
fi
if ! deleted_status_proves_absence; then
  classify_and_exit validation_failed deleted_status_did_not_prove_not_found
fi
printf 'step=live-deleted-status status=pass expected_absence_exit=%s log=<proof-dir>/live-deleted-status.redacted.log\n' "$last_rc"
cleanup_deadline=$((SECONDS + 45))
if ! known_outcome_cleanup; then
  classify_and_exit cleanup_failed exact_baseline_not_restored
fi
cleanup_deadline=0
cleanup_armed=0

source_head="$(git rev-parse HEAD 2>/dev/null || printf unknown)"
source_tree="clean"
if [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
  source_tree="dirty"
fi
classify_and_exit live_unikraft_cloud_smoke_passed "head_${source_head}_tree_${source_tree}_binary_${binary_provenance}_slug_${slug}_cleanup_complete"
