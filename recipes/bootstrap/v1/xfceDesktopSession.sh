#!/bin/bash
set -eu
export DISPLAY="${DISPLAY:-:99}"
. /usr/local/lib/crabbox/xfce-session.sh
CRABBOX_DESKTOP_USER="$(id -un)" /usr/local/bin/crabbox-configure-desktop-theme
terminal_log="$HOME/.cache/crabbox/desktop-terminal.log"
mkdir -p "${terminal_log%/*}"
if command -v xfce4-terminal >/dev/null 2>&1; then
  if ! pgrep -u "$(id -u)" -f 'xfce4-terminal.*Crabbox Desktop' >/dev/null 2>&1; then
    xfce4-terminal --title='Crabbox Desktop' --geometry=110x32+48+48 </dev/null >"$terminal_log" 2>&1 &
  fi
elif command -v xterm >/dev/null 2>&1; then
  if ! pgrep -u "$(id -u)" -f 'xterm -title Crabbox Desktop' >/dev/null 2>&1; then
    xterm -title 'Crabbox Desktop' -geometry 110x32+48+48 -bg '#111827' -fg '#e5e7eb' </dev/null >"$terminal_log" 2>&1 &
  fi
fi
