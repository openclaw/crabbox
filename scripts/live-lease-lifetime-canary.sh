#!/usr/bin/env bash
set -euo pipefail

# Uses only the operator's configured coordinator credentials, never AWS credentials.
if [[ "${CRABBOX_LIVE_CONFIRM:-}" != "lifetime-canary" ]]; then
  printf 'refusing live writes: set CRABBOX_LIVE_CONFIRM=lifetime-canary\n' >&2
  exit 2
fi
if [[ -z "${CRABBOX_COORDINATOR:-}" ]]; then
  printf 'CRABBOX_COORDINATOR must name the deployed coordinator\n' >&2
  exit 2
fi
for tool in python3 jq; do
  command -v "$tool" >/dev/null || { printf 'missing required tool: %s\n' "$tool" >&2; exit 2; }
done

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Warmup has no region flag; the AWS Region comes from the environment.
export CRABBOX_AWS_REGION=eu-west-1
cb="${CRABBOX_BIN:-$root/bin/crabbox}"
[[ -x "$cb" ]] || { printf 'build Crabbox or set CRABBOX_BIN to an executable\n' >&2; exit 2; }
started_at="$(date +%s)"
deadline=$((started_at + 2100))
canary_id="$(python3 -c 'import secrets; print("cbx_" + secrets.token_hex(6))')"
control_id="$(python3 -c 'import secrets; print("cbx_" + secrets.token_hex(6))')"
canary_started=0
control_started=0
proof_complete=0

progress() {
  printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"
}

# A stalled CLI/transport cannot outrun the 35-minute proof or eleven-minute cleanup budget.
run_cb() {
  local maximum="$1"
  shift
  python3 - "$deadline" "$maximum" "$cb" "$@" <<'PY'
import os, signal, subprocess, sys, time
remaining = min(int(sys.argv[2]), int(sys.argv[1]) - time.time())
if remaining <= 0:
    print("lifetime canary runtime bound exceeded", file=sys.stderr)
    sys.exit(124)
process = subprocess.Popen(sys.argv[3:], start_new_session=True)
def terminate():
    if process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    try:
        process.wait(timeout=2)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait()
def interrupted(signum, frame):
    terminate()
    sys.exit(128 + signum)
signal.signal(signal.SIGINT, interrupted)
signal.signal(signal.SIGTERM, interrupted)
try:
    sys.exit(process.wait(timeout=remaining))
except subprocess.TimeoutExpired:
    print("lifetime canary command timed out", file=sys.stderr)
    terminate()
    sys.exit(124)
PY
}

release_lease() {
  local label="$1" id="$2" output
  progress "cleanup $label=$id"
  if output="$(run_cb 300 stop --provider aws --id "$id" 2>&1)"; then
    [[ -z "$output" ]] || printf '%s\n' "$output"
    return 0
  fi
  printf '%s\n' "$output" >&2
  # A create rejected before admission leaves no coordinator lease to release.
  if [[ "$output" == *'"error":"not_found"'* ]]; then
    progress "cleanup $label=$id not found; nothing was allocated"
    return 0
  fi
  progress "CLEANUP FAILED $label=$id; retain this ID for operator recovery" >&2
  return 1
}

cleanup() {
  local outcome=$?
  trap - EXIT INT TERM
  # Cleanup gets its own budget; observing AWS termination can take several minutes.
  deadline=$(($(date +%s) + 660))
  if [[ "$canary_started" == 1 ]] && ! release_lease canary "$canary_id"; then
    outcome=1
  fi
  if [[ "$control_started" == 1 ]] && ! release_lease control "$control_id"; then
    outcome=1
  fi
  if [[ "$outcome" == 0 && "$proof_complete" == 1 ]]; then
    progress "classification=live_lease_lifetime_canary_passed canary=$canary_id instance=$instance_id volume=$volume_id cleanup=complete"
  fi
  progress "finished exit=$outcome canary=$canary_id control=$control_id"
  exit "$outcome"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() { progress "FAILED $*" >&2; exit 1; }

# Ride out brief coordinator Worker resets (5xx, Cloudflare 1101, request deadlines) instead of failing the whole proof.
retry_cb() {
  local maximum="$1" attempt output errors
  shift
  for attempt in 1 2 3 4 5 6 7 8; do
    errors="$(mktemp)"
    if output="$(run_cb "$maximum" "$@" 2>"$errors")"; then
      rm -f "$errors"
      [[ -z "$output" ]] || printf '%s\n' "$output"
      return 0
    fi
    if [[ "$attempt" == 8 ]] || ! grep -qE 'http 5[0-9][0-9]|error code: 1101|context deadline exceeded' "$errors"; then
      cat "$errors" >&2
      rm -f "$errors"
      return 1
    fi
    progress "transient coordinator error attempt=$attempt: $(tr '\n' ' ' <"$errors" | cut -c1-200)" >&2
    rm -f "$errors"
    pause
  done
}

snapshot() {
  retry_cb 60 status --provider aws --id "$1" --json
}

readback() {
  retry_cb 60 admin lifetime-readback --id "$1" --json
}

assert_json() {
  local document="$1" expression="$2" message="$3"
  jq -e "$expression" >/dev/null <<<"$document" || fail "$message"
}

pause() {
  [[ "$(date +%s)" -lt "$deadline" ]] || fail '35-minute proof deadline exceeded'
  sleep 15
}

next_touch=0
touch_control() {
  local now
  now="$(date +%s)"
  if [[ "$now" -ge "$next_touch" ]]; then
    # Keep the control under its ten-minute idle limit, without altering its policy.
    # A missed touch is housekeeping, not proof: the final control assertions still apply.
    if retry_cb 60 run --provider aws --id "$control_id" --no-sync --keep -- true >/dev/null; then
      next_touch=$((now + 60))
    else
      progress "control touch failed; retrying on the next poll control=$control_id" >&2
      next_touch=$((now + 15))
    fi
  fi
}

assert_extension() {
  local document="$1" expected="$2"
  python3 - "$expected" "$(jq -er '.lifetimePolicy.effectiveStopAt' <<<"$document")" \
    "$(jq -er '.lifetimePolicy.extension.expiresAt' <<<"$document")" <<'PY'
from datetime import datetime
import sys
dates = [datetime.fromisoformat(value.replace("Z", "+00:00")) for value in sys.argv[1:]]
sys.exit(0 if all(value == dates[0] for value in dates) else 1)
PY
}

progress "begin lifetime canary=$canary_id control=$control_id region=eu-west-1 type=t3.small"
# Fail before allocation if the configured account cannot use admin routes.
run_cb 60 admin leases --limit 1 --json >/dev/null
identity="$(run_cb 60 whoami --json)"
owner="$(jq -er '.owner | select(type == "string" and length > 0)' <<<"$identity")"

# Start the control first, so it remains running throughout canary enrollment.
# Both TTLs outlast the 35-minute proof so ordinary expiry never pre-empts it.
control_started=1
run_cb 600 warmup --provider aws --target linux --class tiny --type t3.small \
  --market on-demand --lease-id "$control_id" --keep --ttl 45m --idle-timeout 10m
control="$(snapshot "$control_id")"
assert_json "$control" '.lifetimePolicy == null' 'control was unexpectedly enrolled; default mode must be off'

canary_started=1
run_cb 600 warmup --provider aws --target linux --class tiny --type t3.small \
  --market on-demand --lease-id "$canary_id" --keep --ttl 45m --idle-timeout 10m \
  --lifetime-canary 6m --lifetime-retention 5m
while :; do
  touch_control
  canary="$(snapshot "$canary_id")"
  assert_json "$canary" '.lifetimePolicy.source == "admin-canary" and .lifetimePolicy.mode == "enforce"' 'canary policy missing or not enforced'
  assert_json "$canary" '(.lifetimePolicy.warnings | length) > 0' 'owner-visible pre-stop warning missing'
  assert_json "$canary" '.lifetimePolicy.status != "failed" and .lifetimePolicy.status != "unsupported" and .lifetimePolicy.error == null and .lifetimePolicy.readiness.status != "failed"' 'canary permission/readiness check failed'
  if jq -e '.lifetimePolicy.readiness.status == "ready"' >/dev/null <<<"$canary"; then
    break
  fi
  progress "waiting for coordinator StopInstances DryRun readiness canary=$canary_id"
  pause
done
before="$(jq -er '.lifetimePolicy.effectiveStopAt' <<<"$canary")"
extension="$(python3 - "$before" <<'PY'
from datetime import datetime, timedelta, timezone
import sys
print((datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00")).replace(microsecond=0) + timedelta(minutes=2)).astimezone(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))
PY
)"
retry_cb 60 admin extend-lifetime --id "$canary_id" --owner "$owner" \
  --reason 'bounded live lifetime canary' --expires-at "$extension" --json >/dev/null
canary="$(snapshot "$canary_id")"
assert_extension "$canary" "$extension" || fail 'accepted extension did not change the effective stop deadline'
progress "extension accepted canary=$canary_id before=$before stop=$extension"

instance_id=""
volume_id=""
while :; do
  touch_control
  canary="$(snapshot "$canary_id")"
  cloud="$(readback "$canary_id")"
  assert_json "$cloud" '.provider == "aws" and .region == "eu-west-1" and (.instanceId | length > 0)' 'AWS readback identity is incomplete'
  observed_instance="$(jq -er '.instanceId' <<<"$cloud")"
  observed_volume="$(jq -r '.rootVolumeId // empty' <<<"$cloud")"
  [[ -z "$instance_id" || "$instance_id" == "$observed_instance" ]] || fail 'instance identity changed'
  instance_id="$observed_instance"
  if [[ -n "$observed_volume" ]]; then
    [[ -z "$volume_id" || "$volume_id" == "$observed_volume" ]] || fail 'root volume identity changed'
    volume_id="$observed_volume"
  fi
  phase="$(jq -r '.lifetimePolicy.status // "waiting"' <<<"$canary")"
  progress "canary=$canary_id instance=$instance_id volume=${volume_id:-unknown} phase=$phase cloud=$(jq -r '.instanceState' <<<"$cloud") observed=$(jq -r '.observedAt' <<<"$cloud")"
  [[ "$phase" != failed && "$phase" != unsupported ]] || fail "lifetime phase=$phase"
  if jq -e '.lifetimePolicy.status == "retained" and .lifetimePolicy.stoppedAt != null' >/dev/null <<<"$canary"; then
    assert_json "$cloud" '.instanceState == "stopped" and .rootVolumePresent == true and (.rootVolumeId | length > 0)' 'retained lease lacks stopped-instance/root-volume proof'
    assert_json "$canary" 'any(.lifetimePolicy.warnings[]; contains("DELETION:"))' 'distinct owner-visible pre-deletion warning missing'
    progress "retained canary=$canary_id instance=$instance_id volume=$volume_id delete=$(jq -r '.lifetimePolicy.deleteAfterAt' <<<"$canary")"
    break
  fi
  pause
done

late_extension="$(python3 - "$extension" <<'PY'
from datetime import datetime, timedelta, timezone
import sys
print((datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00")).replace(microsecond=0) + timedelta(minutes=2)).astimezone(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))
PY
)"
set +e
denial="$(retry_cb 60 admin extend-lifetime --id "$canary_id" --owner "$owner" \
  --reason 'must reject after stop' --expires-at "$late_extension" --json 2>&1)"
denial_code=$?
set -e
[[ "$denial_code" -ne 0 ]] || fail 'extension after stopping was accepted'
if [[ "$denial" != *invalid_lifetime_extension* || "$denial" != *"cannot be extended"* ]]; then
  [[ "$denial" == *cleanup_in_progress* ]] || fail 'extension failed without a server-side lifecycle rejection'
fi
canary="$(snapshot "$canary_id")"
assert_extension "$canary" "$extension" || fail 'rejected extension changed the policy'
progress "post-stop extension rejected canary=$canary_id"

while :; do
  touch_control
  canary="$(snapshot "$canary_id")"
  cloud="$(readback "$canary_id")"
  jq -e --arg instance "$instance_id" --arg volume "$volume_id" \
    '.instanceId == $instance and .rootVolumeId == $volume' >/dev/null <<<"$cloud" || fail 'retained resource identities changed during deletion'
  progress "deletion canary=$canary_id instance=$instance_id volume=$volume_id phase=$(jq -r '.lifetimePolicy.status' <<<"$canary") cloud=$(jq -r '.instanceState' <<<"$cloud") observed=$(jq -r '.observedAt' <<<"$cloud")"
  if jq -e '.lifetimePolicy.status == "complete"' >/dev/null <<<"$canary"; then
    assert_json "$cloud" '(.instanceState == "terminated" or .instanceState == "absent") and .rootVolumePresent == false' 'completion lacks terminated-instance/root-volume-absence proof'
    break
  fi
  assert_json "$canary" '.lifetimePolicy.status != "failed"' 'retention cleanup failed'
  pause
done

control="$(snapshot "$control_id")"
assert_json "$control" '.lifetimePolicy == null' 'control was enrolled while the canary ran'
cloud="$(readback "$control_id")"
assert_json "$cloud" '.instanceState == "running" and .rootVolumePresent == true' 'unenrolled control did not remain running'
progress "control=$control_id instance=$(jq -r '.instanceId' <<<"$cloud") volume=$(jq -r '.rootVolumeId' <<<"$cloud") running=true unenrolled=true"
proof_complete=1
