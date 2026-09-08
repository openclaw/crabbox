#!/usr/bin/env bash
set -euo pipefail

export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
state_root="${CRABBOX_KOYEB_STATE_ROOT:-/var/lib/crabbox-koyeb}"
runtime_root="${CRABBOX_KOYEB_RUNTIME_ROOT:-/run/crabbox-koyeb}"
tailscale_socket="${runtime_root}/tailscaled.sock"
ready_file="${state_root}/crabbox-ready.json"

test -w /workspace/crabbox
nc -z 127.0.0.1 22 >/dev/null 2>&1
nc -z 127.0.0.1 5900 >/dev/null 2>&1

# SSH callers already proved the tailnet route. Full daemon and Serve checks are
# restricted to the root-owned Koyeb executor bootstrap/health path.
if [[ "$(id -u)" -ne 0 ]]; then
  exit 0
fi

test -r "$ready_file"
jq -e '.schema == "crabbox-koyeb-sandbox-runner/v1"' "$ready_file" >/dev/null

check_owned_pid() {
  local name="$1"
  local expected="$2"
  local pid_file="${runtime_root}/${name}.pid"
  local pid cmdline
  test -f "$pid_file"
  pid="$(cat "$pid_file")"
  [[ "$pid" =~ ^[0-9]+$ ]]
  kill -0 "$pid" 2>/dev/null
  cmdline="$(tr '\0' ' ' <"/proc/${pid}/cmdline")"
  [[ "$cmdline" == *"$expected"* ]]
}

check_owned_pid tailscaled "--socket=${tailscale_socket}"
check_owned_pid sshd "${state_root}/sshd/sshd_config"
check_owned_pid xvfb ':99'
check_owned_pid desktop 'desktop-session.sh'
check_owned_pid x11vnc '-rfbport 5900'

assert_loopback_listener() {
  local port="$1"
  local listeners
  listeners="$(ss -H -ltn "sport = :${port}")"
  [[ -n "$listeners" ]]
  if awk '{ print $4 }' <<<"$listeners" | grep -Ev "^(127\\.0\\.0\\.1|\\[::1\\]):${port}$" >/dev/null; then
    printf 'port %s has a non-loopback listener\n' "$port" >&2
    return 1
  fi
}

assert_loopback_listener 22
assert_loopback_listener 5900

tailscale_status="$(tailscale --socket="$tailscale_socket" status --json)"
jq -e '.BackendState == "Running"' >/dev/null <<<"$tailscale_status"
tailscale --socket="$tailscale_socket" ip -4 | grep -Eq '^100\.[0-9]+\.[0-9]+\.[0-9]+$'
serve_status="$(tailscale --socket="$tailscale_socket" serve status --json)"
grep -F '127.0.0.1:22' >/dev/null <<<"$serve_status"
grep -Eq '"22"|":22"' <<<"$serve_status"
