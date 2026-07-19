#!/usr/bin/env bash
set -euo pipefail

# Hermetic byo-ssh (provider=ssh) lifecycle smoke against 127.0.0.1.
#
# Generates a disposable ed25519 key, authorizes it for the current user,
# ensures a local sshd is running, then drives the static SSH provider
# through warmup -> status -> run -> cp upload/download -> tunnel -> list ->
# stop. No cloud resources, no credentials beyond the throwaway keypair; the
# authorized_keys entry is removed again on exit.
#
# Environment:
#   CRABBOX_BIN                  Crabbox binary (default: ./bin/crabbox)
#   CRABBOX_SSH_LOCALHOST_USER   SSH user (default: current user)
#   CRABBOX_SSH_LOCALHOST_PORT   SSH port (default: 22)

slug="ssh-localhost-smoke-$(date +%Y%m%d%H%M%S)-$$"
bin="${CRABBOX_BIN:-./bin/crabbox}"
ssh_user="${CRABBOX_SSH_LOCALHOST_USER:-$(id -un)}"
ssh_port="${CRABBOX_SSH_LOCALHOST_PORT:-22}"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-ssh-localhost-XXXXXX")"
key_file="$work_dir/id_ed25519"
work_root="$work_dir/workroot"
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
  local output
  set +e
  output="$("$@" 2>&1)"
  local status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    classify_blocker "$command" "$status" "$output"
    exit "$status"
  fi
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

mkdir -p "$work_root"
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
printf 'classification=live_ssh_localhost_smoke_passed slug=%s host=127.0.0.1 cp=roundtrip tunnel=%s cleanup=complete\n' "$slug" "$tunnel_result"
