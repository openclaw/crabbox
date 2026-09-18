#!/usr/bin/env bash
set -euo pipefail

umask 077
export DISPLAY="${DISPLAY:-:99}"
export HOME="${HOME:-/home/crabbox}"
export USER="${USER:-crabbox}"
export LOGNAME="${LOGNAME:-crabbox}"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/crabbox-koyeb/user}"
session_log_root="${CRABBOX_KOYEB_SESSION_LOG_ROOT:-${XDG_RUNTIME_DIR}}"

[[ -d "$XDG_RUNTIME_DIR" && ! -L "$XDG_RUNTIME_DIR" && -w "$XDG_RUNTIME_DIR" ]] || exit 1
[[ "$session_log_root" == "$XDG_RUNTIME_DIR" ]] || exit 1

# Desktop caches, icon links and GPG-agent sockets are runtime state. Keep them
# out of the clean project user's home, which is checked before a pool claim.
export XDG_CONFIG_HOME="${XDG_RUNTIME_DIR}/desktop-config"
export XDG_CACHE_HOME="${XDG_RUNTIME_DIR}/desktop-cache"
export GNUPGHOME="${XDG_RUNTIME_DIR}/gnupg"
install -d -m 0700 -- "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$GNUPGHOME"

session_pid=""
session_ready=false
cleanup() {
  if [[ -n "$session_pid" ]] && kill -0 "$session_pid" 2>/dev/null; then
    kill "$session_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT HUP INT TERM

/usr/bin/startxfce4 >"${session_log_root}/xfce.log" 2>&1 &
session_pid=$!

for _ in $(seq 1 60); do
  if /usr/bin/xfconf-query -c xfwm4 -p /general/theme >/dev/null 2>&1; then
    session_ready=true
    break
  fi
  kill -0 "$session_pid" 2>/dev/null || exit 1
  sleep 0.5
done
[[ "$session_ready" == true ]] || exit 1

/usr/bin/xfce4-terminal --title="Crabbox Desktop" --geometry=110x32+48+48 \
  >"${session_log_root}/terminal.log" 2>&1 &
# Browser profiles are created only on an explicit, post-allocation launch.
# An idle generic runner must contain no browser session or profile state.

wait "$session_pid"
