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
/usr/local/bin/crabbox-browser about:blank \
  >"${session_log_root}/browser.log" 2>&1 &

wait "$session_pid"
