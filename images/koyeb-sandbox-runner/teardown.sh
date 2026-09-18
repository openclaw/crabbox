#!/usr/bin/env bash
set -euo pipefail
set +x

export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
state_root="${CRABBOX_KOYEB_STATE_ROOT:-/var/lib/crabbox-koyeb}"
runtime_root="${CRABBOX_KOYEB_RUNTIME_ROOT:-/run/crabbox-koyeb}"
tailscale_socket="${runtime_root}/tailscaled.sock"
desktop_runtime="${runtime_root}/user"
unset CRABBOX_KOYEB_NETWORK CRABBOX_KOYEB_PRIVATE_HOST CRABBOX_KOYEB_TAILSCALE_AUTH_KEY
unset SANDBOX_SECRET KOYEB_API_TOKEN

[[ "$(id -u)" -eq 0 ]] || { echo "teardown must run as root" >&2; exit 2; }
for managed_dir in "$state_root" "$runtime_root"; do
  if [[ -L "$managed_dir" || ( -e "$managed_dir" && ! -d "$managed_dir" ) ]]; then
    echo "unsafe managed directory: $managed_dir" >&2
    exit 2
  fi
done
install -d -m 0700 -o root -g root "$state_root"
install -d -m 0711 -o root -g root "$runtime_root"

if [[ "${CRABBOX_KOYEB_LOCK_HELD:-0}" != 1 ]]; then
  exec 9>"${runtime_root}/operation.lock"
  flock -w 30 9 || { echo "timed out waiting for runner operation lock" >&2; exit 2; }
fi

if [[ -S "$tailscale_socket" ]]; then
  tailscale --socket="$tailscale_socket" serve --tcp=22 off >/dev/null 2>&1 || true
  tailscale --socket="$tailscale_socket" logout >/dev/null 2>&1 || true
fi

terminate_owned() {
  local name="$1"
  local expected="$2"
  local pid_file="${runtime_root}/${name}.pid"
  local pid cmdline pgid
  [[ -f "$pid_file" ]] || return 0
  pid="$(cat "$pid_file" 2>/dev/null || true)"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 0
  [[ -r "/proc/${pid}/cmdline" ]] || return 0
  cmdline="$(tr '\0' ' ' <"/proc/${pid}/cmdline" 2>/dev/null || true)"
  [[ "$cmdline" == *"$expected"* ]] || return 0
  pgid="$(ps -o pgid= -p "$pid" | tr -d '[:space:]')"
  if [[ "$pgid" == "$pid" ]]; then
    kill -- "-${pid}" 2>/dev/null || true
  else
    kill -- "$pid" 2>/dev/null || true
  fi
  for _ in $(seq 1 40); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.25
  done
  cmdline="$(tr '\0' ' ' <"/proc/${pid}/cmdline" 2>/dev/null || true)"
  if [[ "$cmdline" == *"$expected"* ]]; then
    if [[ "$pgid" == "$pid" ]]; then
      kill -KILL -- "-${pid}" 2>/dev/null || true
    else
      kill -KILL -- "$pid" 2>/dev/null || true
    fi
  fi
}

terminate_owned x11vnc '-rfbport 5900'
terminate_owned desktop 'desktop-session.sh'
terminate_owned xvfb ':99'
terminate_owned sshd "${state_root}/sshd/sshd_config"
terminate_owned tailscaled "--socket=${tailscale_socket}"

rm -f -- "${state_root}/crabbox-ready.json" \
  "${state_root}/crabbox-ready.json.tmp" \
  "${state_root}/lease-id" \
  "${state_root}/tailscaled.log" \
  "${state_root}/sshd.log" \
  "${state_root}/xvfb.log" \
  "${state_root}/desktop.log" \
  "${state_root}/x11vnc.log" \
  "${state_root}/xfce.log" \
  "${state_root}/terminal.log" \
  "${state_root}/browser.log" \
  "${runtime_root}/tailscaled.pid" \
  "${runtime_root}/sshd.pid" \
  "${runtime_root}/sshd.internal.pid" \
  "${runtime_root}/xvfb.pid" \
  "${runtime_root}/desktop.pid" \
  "${runtime_root}/x11vnc.pid" \
  "${runtime_root}/tailscaled.sock" \
  "${desktop_runtime}/xfce.log" \
  "${desktop_runtime}/terminal.log" \
  "${desktop_runtime}/browser.log" \
  /var/lib/crabbox/authorized_keys \
  /var/lib/crabbox/lease-id \
  /var/lib/crabbox/vnc.password \
  /var/lib/crabbox/vnc.pass \
  /var/lib/crabbox/desktop.env \
  /var/lib/crabbox/browser.env \
  /var/lib/crabbox/tailscale-ipv4 \
  /var/lib/crabbox/tailscale-hostname \
  /var/lib/crabbox/tailscale-fqdn
rm -f -- "${state_root}/sshd/ssh_host_ed25519_key" \
  "${state_root}/sshd/ssh_host_ed25519_key.pub" \
  "${state_root}/sshd/sshd_config"
rmdir -- "${state_root}/sshd" "$desktop_runtime" 2>/dev/null || true

if [[ "${1:-}" != --bootstrap-failure ]]; then
  printf 'crabbox-koyeb-teardown: complete\n'
fi
