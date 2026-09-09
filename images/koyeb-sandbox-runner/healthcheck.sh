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

# SSH callers already proved their selected private route. Full daemon and
# transport checks are restricted to the root-owned executor health path.
if [[ "$(id -u)" -ne 0 ]]; then
  exit 0
fi

test -r "$ready_file"
schema="$(jq -er '.schema' "$ready_file")"
case "$schema" in
  crabbox-koyeb-sandbox-runner/v1) network=tailscale ;;
  crabbox-koyeb-sandbox-runner/v2)
    network="$(jq -er '.network.transport' "$ready_file")"
    [[ "$network" == koyeb-mesh ]]
    jq -e '.network.privateHost == .ssh.host and (.ssh.host | test("^[a-z0-9][a-z0-9-]*\\.[a-z0-9][a-z0-9-]*\\.internal$"))' "$ready_file" >/dev/null
    ;;
  *) exit 1 ;;
esac

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

if [[ "$network" == tailscale ]]; then
  check_owned_pid tailscaled "--socket=${tailscale_socket}"
fi
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

assert_loopback_listener 5900
if [[ "$network" == tailscale ]]; then
  assert_loopback_listener 22
  tailscale_status="$(tailscale --socket="$tailscale_socket" status --json)"
  jq -e '.BackendState == "Running"' >/dev/null <<<"$tailscale_status"
  tailscale --socket="$tailscale_socket" ip -4 | grep -Eq '^100\.[0-9]+\.[0-9]+\.[0-9]+$'
  serve_status="$(tailscale --socket="$tailscale_socket" serve status --json)"
  grep -F '127.0.0.1:22' >/dev/null <<<"$serve_status"
  grep -Eq '"22"|":22"' <<<"$serve_status"
else
  ssh_listeners="$(ss -H -ltn 'sport = :22')"
  [[ -n "$ssh_listeners" ]]
  awk '{ print $4 }' <<<"$ssh_listeners" | grep -Eq '^0\.0\.0\.0:22$'
fi
