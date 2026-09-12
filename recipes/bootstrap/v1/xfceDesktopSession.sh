#!/bin/bash
set -eu
export DISPLAY="${DISPLAY:-:99}"
. /usr/local/lib/crabbox/xfce-session.sh
CRABBOX_DESKTOP_USER="$(id -un)" /usr/local/bin/crabbox-configure-desktop-theme "${1:-}"
terminal_log="$HOME/.cache/crabbox/desktop-terminal.log"
mkdir -p "${terminal_log%/*}"
if command -v xfce4-terminal >/dev/null 2>&1; then
  if ! pgrep -u "$(id -u)" -f 'xfce4-terminal.*Crabbox Desktop' >/dev/null 2>&1; then
    xfce4-terminal --title='Crabbox Desktop' --geometry=110x32+48+48 </dev/null >"$terminal_log" 2>&1 &
  fi
elif command -v xterm >/dev/null 2>&1; then
  if ! pgrep -u "$(id -u)" -f 'xterm -title Crabbox Desktop' >/dev/null 2>&1; then
    terminal_config="$HOME/.config/xfce4/terminal/terminalrc"
    terminal_bg="$(sed -n 's/^ColorBackground=//p' "$terminal_config")"
    terminal_fg="$(sed -n 's/^ColorForeground=//p' "$terminal_config")"
    xterm -title 'Crabbox Desktop' -geometry 110x32+48+48 -bg "$terminal_bg" -fg "$terminal_fg" </dev/null >"$terminal_log" 2>&1 &
  fi
fi
