#!/usr/bin/env bash
set -euo pipefail

# Hermetic byo-ssh (provider=ssh) lifecycle smoke against 127.0.0.1.
#
# Generates a disposable ed25519 key, authorizes it for the current user,
# ensures a local sshd is running, then drives the static SSH provider
# through warmup -> status -> run -> retained script failure bundle ->
# cp upload/download -> tunnel -> list ->
# stop. No cloud resources, no credentials beyond the throwaway keypair; the
# authorized_keys entry is removed again on exit.
#
# Environment:
#   CRABBOX_BIN                  Crabbox binary (default: ./bin/crabbox)
#   CRABBOX_SSH_LOCALHOST_USER   SSH user (default: current user)
#   CRABBOX_SSH_LOCALHOST_PORT   SSH port (default: 22)

slug="ssh-localhost-smoke-$(date +%Y%m%d%H%M%S)-$$"
bin="${CRABBOX_BIN:-./bin/crabbox}"
# Keep relative binary paths valid after entering the owned fixture project.
if [[ "$bin" != /* ]]; then
  bin="$PWD/$bin"
fi
ssh_user="${CRABBOX_SSH_LOCALHOST_USER:-$(id -un)}"
ssh_port="${CRABBOX_SSH_LOCALHOST_PORT:-22}"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-ssh-localhost-XXXXXX")"
key_file="$work_dir/id_ed25519"
work_root="$work_dir/workroot"
failure_project="$work_dir/failure-project"
authorized_keys="$HOME/.ssh/authorized_keys"
authorized_entry=""
cleanup_armed=0
http_server_pid=""
tunnel_pid=""
tunnel_result="skipped"

classify_blocker() {
  local command="$1"
  local status="$2"
  local output="$3"
  local classification="environment_blocked"
  local lower
  lower="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *quota* || "$lower" == *"rate limit"* || "$lower" == *capacity* ]]; then
    classification="quota_blocked"
  fi
  printf 'classification=%s command=%q exit=%s\n' "$classification" "$command" "$status" >&2
  printf '%s\n' "$output" >&2
}

classify_validation_failure() {
  local command="$1"
  local status="$2"
  local output="$3"
  printf 'classification=validation_failed command=%q exit=%s\n' "$command" "$status" >&2
  printf '%s\n' "$output" >&2
}

run_capture() {
  local command="$1"
  shift
  local output stderr_file status=0
  stderr_file="$(mktemp "$work_dir/command-stderr-XXXXXX")"
  # Keep diagnostics out of captured stdout, especially list --json.
  output="$("$@" 2>"$stderr_file")" || status=$?
  if [ "$status" -ne 0 ]; then
    output+=$'\n'"$(cat "$stderr_file")"
    rm -f "$stderr_file"
    classify_blocker "$command" "$status" "$output"
    exit "$status"
  fi
  cat "$stderr_file" >&2
  rm -f "$stderr_file"
  printf '%s\n' "$output"
}

file_mode() {
  if stat -f '%Lp' "$1" >/dev/null 2>&1; then
    stat -f '%Lp' "$1"
  else
    stat -c '%a' "$1"
  fi
}

remove_authorized_entry() {
  if [ -n "$authorized_entry" ] && [ -f "$authorized_keys" ]; then
    grep -vF -- "$authorized_entry" "$authorized_keys" >"$authorized_keys.crabbox-smoke" || true
    mv "$authorized_keys.crabbox-smoke" "$authorized_keys"
    chmod 600 "$authorized_keys"
    authorized_entry=""
  fi
}

cleanup() {
  if [ -n "$tunnel_pid" ]; then
    kill -INT "$tunnel_pid" >/dev/null 2>&1 || true
    wait "$tunnel_pid" 2>/dev/null || true
  fi
  if [ -n "$http_server_pid" ]; then
    kill "$http_server_pid" >/dev/null 2>&1 || true
    wait "$http_server_pid" 2>/dev/null || true
  fi
  if [ "$cleanup_armed" -eq 1 ]; then
    "$bin" stop --provider ssh "$slug" >/dev/null 2>&1 || true
  fi
  remove_authorized_entry
  rm -rf "$work_dir"
}
trap cleanup EXIT

if [ ! -x "$bin" ]; then
  echo "missing crabbox binary: $bin" >&2
  echo "build first: go build -trimpath -o bin/crabbox ./cmd/crabbox" >&2
  exit 2
fi

mkdir -p "$work_root" "$failure_project"
# Every CLI call must share the RepoRoot that warmup binds to the lease.
cd "$failure_project"
ssh-keygen -t ed25519 -N "" -q -f "$key_file" -C "crabbox-ssh-localhost-smoke"
mkdir -p "$HOME/.ssh"
chmod 700 "$HOME/.ssh"
authorized_entry="$(cat "$key_file.pub")"
printf '%s\n' "$authorized_entry" >>"$authorized_keys"
chmod 600 "$authorized_keys"

sshd_reachable() {
  ssh-keyscan -T 5 -p "$ssh_port" 127.0.0.1 >/dev/null 2>&1
}

if ! sshd_reachable; then
  if command -v sudo >/dev/null 2>&1; then
    sudo systemctl start ssh 2>/dev/null ||
      sudo service ssh start 2>/dev/null ||
      sudo systemctl start sshd 2>/dev/null || true
  fi
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if sshd_reachable; then
      break
    fi
    sleep 1
  done
fi
if ! sshd_reachable; then
  classify_blocker "ssh-keyscan -p $ssh_port 127.0.0.1" 1 "no sshd reachable on 127.0.0.1:$ssh_port; start the local SSH service first"
  exit 1
fi

export CRABBOX_STATIC_HOST=127.0.0.1
export CRABBOX_STATIC_USER="$ssh_user"
export CRABBOX_STATIC_PORT="$ssh_port"
export CRABBOX_STATIC_WORK_ROOT="$work_root"
export CRABBOX_SSH_KEY="$key_file"

doctor_output="$(run_capture "$bin doctor --provider ssh" "$bin" doctor --provider ssh)"
printf '%s\n' "$doctor_output"

cleanup_armed=1
run_capture "$bin warmup --provider ssh --slug $slug --keep" "$bin" warmup --provider ssh --slug "$slug" --keep >/dev/null
run_capture "$bin status --provider ssh --id $slug --wait --wait-timeout 60s" "$bin" status --provider ssh --id "$slug" --wait --wait-timeout 60s >/dev/null

run_output="$(run_capture "$bin run --provider ssh --id $slug --no-sync -- echo crabbox-ssh-localhost-ok" "$bin" run --provider ssh --id "$slug" --no-sync -- echo crabbox-ssh-localhost-ok)"
printf '%s\n' "$run_output"
if [[ "$run_output" != *crabbox-ssh-localhost-ok* ]]; then
  classify_validation_failure "$bin run --provider ssh --id $slug" 1 "run output did not include crabbox-ssh-localhost-ok"
  exit 1
fi

# Keep all local captures and remote payloads in this smoke's owned workspace.
# Expected workload exits are validation inputs, not environment blockers.
cat >"$failure_project/first success ' script.sh" <<'SH'
#!/bin/sh
set -eu
case "$0" in
  /*) printf '%s\n' "$0" ;;
  *) printf '%s/%s\n' "$PWD" "$0" ;;
esac
SH
cat >"$failure_project/current failure ' script.sh" <<'SH'
#!/bin/sh
set -eu
store=$(dirname "$0")
dd if=/dev/urandom of="$store/arbitrary.bin" bs=1048576 count=8 2>/dev/null
printf 'selected prior log\n' > "$store/prior.log"
printf '<testsuite/>\n' > "$store/junit.xml"
printf '<testsuite/>\n' > "$store/TEST-prior.xml"
mkdir -p test-results
printf 'current report\n' > test-results/failure.log
printf 'current stdout\n'
printf 'current stderr\n' >&2
exit 23
SH
(
  first_upload="$(run_capture "$bin run first retained script" "$bin" run --provider ssh --id "$slug" --no-sync --keep \
    --script "$failure_project/first success ' script.sh")"
  workload_status=0
  "$bin" run --provider ssh --id "$slug" --no-sync --keep-on-failure --timing-json \
    --download-on-failure ".crabbox/scripts/prior.log=$failure_project/selected.log" \
    --download ".crabbox/scripts/prior.log=$failure_project/success-only.log" \
    --script "$failure_project/current failure ' script.sh" \
    >"$work_dir/failure-stdout" 2>"$work_dir/failure-stderr" || workload_status=$?
  cat "$work_dir/failure-stdout"
  cat "$work_dir/failure-stderr" >&2
  if [ "$workload_status" -ne 23 ]; then
    classify_validation_failure "$bin run current failure script" "$workload_status" "expected workload exit 23, got $workload_status"
    exit 1
  fi
  validation_status=0
  validation_output="$(python3 - "$failure_project" "$first_upload" "$work_root" "$work_dir/failure-stderr" <<'PY'
import hashlib
import json
from pathlib import Path
import sys
import tarfile

project, first, root, stderr = map(Path, sys.argv[1:])
assert first.is_relative_to(root) and first.is_file(), f"missing retained earlier upload: {first}"
assert first.read_bytes() == (project / "first success ' script.sh").read_bytes(), "earlier upload changed"
source = project / "current failure ' script.sh"
data = source.read_bytes()
current = ".crabbox/scripts/" + hashlib.sha256(data).hexdigest()[:12] + "-currentfailurescript.sh"
bundles = list((project / ".crabbox/captures").glob("*.tar.gz"))
assert len(bundles) == 1, bundles
prefix = "crabbox-artifacts/remote/"
with tarfile.open(bundles[0]) as archive:
    names = archive.getnames()
    scripts = [name for name in names if name.startswith(prefix + ".crabbox/scripts/")]
    assert scripts == [prefix + current], scripts
    assert archive.extractfile(prefix + current).read() == data, "current script bytes differ"
    assert names.count(prefix + "test-results/failure.log") == 1, "report missing or duplicated"
    metadata = json.load(archive.extractfile("crabbox-artifacts/crabbox-run.json"))
    assert metadata["exitCode"] == 23, metadata
    assert archive.extractfile("crabbox-artifacts/stdout.log").read() == b"current stdout\n"
    assert archive.extractfile("crabbox-artifacts/stderr.log").read() == b"current stderr\n"
assert (project / "selected.log").read_bytes() == b"selected prior log\n", "explicit failure download lost"
assert not (project / "success-only.log").exists(), "success download ran after failure"
assert (first.parent / "arbitrary.bin").stat().st_size == 8 * 1024 * 1024
assert not list(first.parent.parent.glob("*.tar.gz")), "remote capture archive leaked"
timings = [json.loads(line) for line in stderr.read_text().splitlines() if line.startswith('{"')]
assert len(timings) == 1 and timings[0]["exitCode"] == 23, timings
# Reused retained leases skip release entirely; no-attempt cleanup fields are omitted.
# Retention is also checked via the earlier upload above and the status call below.
assert "leaseStopped" not in timings[0] and "leaseStopError" not in timings[0], timings
print(f"failure_bundle=passed exit=23 current_bytes={len(data)} stale=absent neighbors=absent noise_bytes=8388608 explicit_download=passed retained=true remote_archives=0 bundle_bytes={bundles[0].stat().st_size}")
PY
  )" || validation_status=$?
  if [ "$validation_status" -ne 0 ]; then
    classify_validation_failure "failure bundle contents and lifecycle" "$validation_status" "$validation_output"
    exit "$validation_status"
  fi
  printf '%s\n' "$validation_output"
)
run_capture "$bin status after expected script failure" "$bin" status --provider ssh --id "$slug" --wait --wait-timeout 60s >/dev/null

printf 'crabbox-ssh-cp-roundtrip\n' >"$work_dir/cp upload.txt"
RSYNC_OLD_ARGS=1 RSYNC_PROTECT_ARGS=1 run_capture "$bin cp upload over resolved SSH" "$bin" cp --provider ssh --id "$slug" \
  "$work_dir/cp upload.txt" "SANDBOX:$work_root/cp remote[1].txt" >/dev/null
chmod 0711 "$work_root/cp remote[1].txt"
(
  umask 022
  RSYNC_OLD_ARGS=1 RSYNC_PROTECT_ARGS=1 run_capture "$bin cp download over resolved SSH" "$bin" cp --provider ssh --id "$slug" \
    "SANDBOX:$work_root/cp remote[1].txt" "$work_dir/cp download.txt"
) >/dev/null
if ! cmp -s "$work_dir/cp upload.txt" "$work_dir/cp download.txt"; then
  classify_validation_failure "$bin cp --provider ssh --id $slug" 1 "SSH cp roundtrip content mismatch"
  exit 1
fi
if [ "$(file_mode "$work_dir/cp download.txt")" != "644" ]; then
  classify_validation_failure "$bin cp --provider ssh --id $slug" 1 "SSH cp download did not normalize an unusual remote file mode to 0644"
  exit 1
fi

if command -v python3 >/dev/null 2>&1 && command -v curl >/dev/null 2>&1; then
  remote_port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
  mkdir -p "$work_dir/http"
  printf 'crabbox-ssh-tunnel-ok\n' >"$work_dir/http/index.html"
  python3 -m http.server "$remote_port" --bind 127.0.0.1 --directory "$work_dir/http" \
    >"$work_dir/http-server.log" 2>&1 &
  http_server_pid=$!
  server_wait_attempt=0
  while [ "$server_wait_attempt" -lt 40 ]; do
    if curl -fsS "http://127.0.0.1:$remote_port" >/dev/null 2>&1; then
      break
    fi
    if ! kill -0 "$http_server_pid" 2>/dev/null; then
      classify_validation_failure "python3 -m http.server $remote_port" 1 "HTTP fixture exited before readiness: $(cat "$work_dir/http-server.log")"
      exit 1
    fi
    sleep 0.1
    server_wait_attempt=$((server_wait_attempt + 1))
  done
  if [ "$server_wait_attempt" -ge 40 ]; then
    classify_validation_failure "python3 -m http.server $remote_port" 1 "HTTP fixture did not accept connections"
    exit 1
  fi
  "$bin" tunnel --provider ssh --id "$slug" "$remote_port" \
    >"$work_dir/tunnel-url" 2>"$work_dir/tunnel-error" &
  tunnel_pid=$!
  tunnel_wait_attempt=0
  while [ "$tunnel_wait_attempt" -lt 80 ]; do
    if [ -s "$work_dir/tunnel-url" ]; then
      break
    fi
    if ! kill -0 "$tunnel_pid" 2>/dev/null; then
      classify_validation_failure "$bin tunnel --provider ssh --id $slug $remote_port" 1 "$(cat "$work_dir/tunnel-error")"
      exit 1
    fi
    sleep 0.25
    tunnel_wait_attempt=$((tunnel_wait_attempt + 1))
  done
  if [ ! -s "$work_dir/tunnel-url" ]; then
    classify_validation_failure "$bin tunnel --provider ssh --id $slug $remote_port" 1 "tunnel did not print a readiness URL within 20 seconds: $(cat "$work_dir/tunnel-error")"
    exit 1
  fi
  tunnel_url="$(tr -d '\r\n' <"$work_dir/tunnel-url")"
  tunnel_output="$(curl -fsS "$tunnel_url")"
  if [ "$tunnel_output" != "crabbox-ssh-tunnel-ok" ]; then
    classify_validation_failure "$bin tunnel --provider ssh --id $slug $remote_port" 1 "unexpected tunnel response: $tunnel_output"
    exit 1
  fi
  tunnel_result="ready"
  kill -INT "$tunnel_pid"
  wait "$tunnel_pid"
  tunnel_pid=""
  kill "$http_server_pid"
  wait "$http_server_pid" 2>/dev/null || true
  http_server_pid=""
else
  printf 'classification=environment_skipped feature=tunnel reason=python3-or-curl-missing\n' >&2
fi

list_output="$(run_capture "$bin list --provider ssh --json" "$bin" list --provider ssh --json)"
printf '%s\n' "$list_output"
validation_status=0
validation_output=""
set +e
if command -v python3 >/dev/null 2>&1; then
  validation_output="$(CRABBOX_SMOKE_SLUG="$slug" python3 -c '
import json
import os
import sys

slug = os.environ["CRABBOX_SMOKE_SLUG"]
try:
    payload = json.load(sys.stdin)
except Exception as exc:
    print(f"invalid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

def has_slug(value):
    if isinstance(value, dict):
        labels = value.get("labels")
        if isinstance(labels, dict) and labels.get("slug") == slug:
            return True
        if value.get("slug") == slug or value.get("name") == slug:
            return True
        return any(has_slug(child) for child in value.values())
    if isinstance(value, list):
        return any(has_slug(child) for child in value)
    return False

if not has_slug(payload):
    print(f"list JSON did not include slug {slug}", file=sys.stderr)
    sys.exit(1)
' <<<"$list_output" 2>&1)"
  validation_status=$?
elif command -v jq >/dev/null 2>&1; then
  validation_output="$(jq -e --arg slug "$slug" \
    '.. | objects | select((.slug? == $slug) or (.name? == $slug) or (((.labels? // {}) | .slug?) == $slug))' \
    >/dev/null <<<"$list_output" 2>&1)"
  validation_status=$?
  if [ "$validation_status" -ne 0 ] && [ -z "$validation_output" ]; then
    validation_output="list JSON did not include slug $slug"
  fi
else
  validation_output="no JSON parser available for list --json validation; install python3 or jq"
  validation_status=1
fi
set -e
if [ "$validation_status" -ne 0 ]; then
  classify_validation_failure "$bin list --provider ssh --json" "$validation_status" "$validation_output"
  exit "$validation_status"
fi

run_capture "$bin stop --provider ssh $slug" "$bin" stop --provider ssh "$slug" >/dev/null
cleanup_armed=0

# Static start/stop commands: repository config is refused before any command
# runs, approved commands bracket one lease, and a repeated stop is inert.
power_log="$work_dir/power.log"
power_command="$work_dir/host-power"
power_id="static_power_smoke"
power_slug="power-smoke"
cat >"$power_command" <<'SH'
#!/bin/sh
printf '%s %s %s\n' "$1" "$CRABBOX_LEASE_ID" "$CRABBOX_STATIC_HOST" >>"$CRABBOX_POWER_LOG"
SH
chmod 0755 "$power_command"
export CRABBOX_POWER_LOG="$power_log"
power_argv() {
  printf '["%s","%s"]' "$power_command" "$1"
}
printf 'static:\n  startCommand: %s\n' "$(power_argv up)" >"$failure_project/power-repo.yaml"
rejection_status=0
CRABBOX_CONFIG="$failure_project/power-repo.yaml" CRABBOX_STATIC_ID="$power_id" \
  "$bin" warmup --provider ssh --slug "$power_slug" --keep \
  >"$work_dir/power-reject-stdout" 2>"$work_dir/power-reject-stderr" || rejection_status=$?
rm -f "$failure_project/power-repo.yaml"
if [ "$rejection_status" -eq 0 ] || ! grep -q 'repository-configured static.startCommand' "$work_dir/power-reject-stderr" || [ -e "$power_log" ]; then
  classify_validation_failure "$bin warmup with repository static.startCommand" 1 "expected refusal before any command ran: status=$rejection_status stderr=$(cat "$work_dir/power-reject-stderr")"
  exit 1
fi
cp "$power_command" "$failure_project/host-power"
relative_status=0
CRABBOX_STATIC_ID="$power_id" CRABBOX_STATIC_START_COMMAND='["./host-power","up"]' \
  "$bin" warmup --provider ssh --slug "$power_slug" --keep \
  >"$work_dir/power-relative-stdout" 2>"$work_dir/power-relative-stderr" || relative_status=$?
rm -f "$failure_project/host-power"
if [ "$relative_status" -eq 0 ] || ! grep -q 'must be an absolute path' "$work_dir/power-relative-stderr" || [ -e "$power_log" ]; then
  classify_validation_failure "$bin warmup with relative static.startCommand" 1 "expected refusal before any command ran: status=$relative_status stderr=$(cat "$work_dir/power-relative-stderr")"
  exit 1
fi
(
  export CRABBOX_STATIC_ID="$power_id"
  export CRABBOX_STATIC_START_COMMAND="$(power_argv up)"
  export CRABBOX_STATIC_STOP_COMMAND="$(power_argv down)"
  run_capture "$bin warmup with static power commands" "$bin" warmup --provider ssh --slug "$power_slug" --keep >/dev/null
  run_capture "$bin stop with static power commands" "$bin" stop --provider ssh "$power_id" >/dev/null
  "$bin" stop --provider ssh "$power_id" >/dev/null 2>&1 || true
)
expected_power="up $power_id 127.0.0.1
down $power_id 127.0.0.1"
if [ "$(cat "$power_log" 2>/dev/null)" != "$expected_power" ]; then
  classify_validation_failure "static power command lifecycle" 1 "power log mismatch: $(cat "$power_log" 2>/dev/null)"
  exit 1
fi
# A run whose claim another process reacquires mid-run must not stop the host.
: >"$power_log"
held_marker="$work_dir/power-held"
release_marker="$work_dir/power-release"
(
  export CRABBOX_STATIC_ID="$power_id"
  export CRABBOX_STATIC_START_COMMAND="$(power_argv up)"
  export CRABBOX_STATIC_STOP_COMMAND="$(power_argv down)"
  "$bin" run --provider ssh --no-sync -- /bin/sh -c "touch '$held_marker'; i=0; while [ ! -e '$release_marker' ] && [ \$i -lt 300 ]; do sleep 0.2; i=\$((i + 1)); done" \
    >"$work_dir/power-run-stdout" 2>"$work_dir/power-run-stderr" &
  run_pid=$!
  wait_attempt=0
  while [ ! -e "$held_marker" ] && [ "$wait_attempt" -lt 300 ] && kill -0 "$run_pid" 2>/dev/null; do
    sleep 0.2
    wait_attempt=$((wait_attempt + 1))
  done
  if [ ! -e "$held_marker" ]; then
    touch "$release_marker"
    wait "$run_pid" || true
    classify_validation_failure "$bin run holding a static power lease" 1 "run never held the lease: $(cat "$work_dir/power-run-stderr")"
    exit 1
  fi
  reacquire_status=0
  "$bin" warmup --provider ssh --slug "$power_slug" --keep >/dev/null 2>"$work_dir/power-reacquire-stderr" || reacquire_status=$?
  touch "$release_marker"
  run_status=0
  wait "$run_pid" || run_status=$?
  if [ "$reacquire_status" -ne 0 ] || [ "$run_status" -ne 0 ]; then
    classify_validation_failure "static power stale-claim scenario" 1 "reacquire=$reacquire_status run=$run_status: $(cat "$work_dir/power-reacquire-stderr" "$work_dir/power-run-stderr")"
    exit 1
  fi
  expected_stale="up $power_id 127.0.0.1
up $power_id 127.0.0.1"
  if [ "$(cat "$power_log")" != "$expected_stale" ] || ! grep -q 'claim is absent or changed' "$work_dir/power-run-stderr"; then
    classify_validation_failure "static power stale-claim scenario" 1 "stale release must skip stop: $(cat "$power_log") $(cat "$work_dir/power-run-stderr")"
    exit 1
  fi
  run_capture "$bin stop of the surviving reacquired lease" "$bin" stop --provider ssh "$power_id" >/dev/null
)
expected_final="up $power_id 127.0.0.1
up $power_id 127.0.0.1
down $power_id 127.0.0.1"
if [ "$(cat "$power_log")" != "$expected_final" ]; then
  classify_validation_failure "static power stale-claim scenario" 1 "surviving claim must stop once: $(cat "$power_log")"
  exit 1
fi
printf 'static_power=passed repository_command=refused relative_executable=refused start=1 stop=1 repeated_stop=inert stale_claim_stop=skipped surviving_claim_stop=1\n'
cleanup
trap - EXIT
test ! -e "$work_dir"
printf 'classification=live_ssh_localhost_smoke_passed slug=%s host=127.0.0.1 cp=roundtrip tunnel=%s static_power=passed cleanup=complete\n' "$slug" "$tunnel_result"
