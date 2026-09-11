# Sourced by Bash launchers as the desktop user; the running XFCE session owns its bus.
crabbox_bind_xfce_session() {
  local user_id="$(id -u)"
  local session_count session_dbus session_runtime session_pid
  local process_display process_dbus process_runtime entry
  local requested_display="$DISPLAY"
  # X11 omits the screen suffix for screen zero; keep the caller's DISPLAY intact.
  if [[ "$requested_display" =~ ^(.*:[0-9]+)\.0$ ]]; then
    requested_display="${BASH_REMATCH[1]}"
  fi
  session_count=0
  session_dbus=
  session_runtime=
  for session_pid in $(pgrep -u "$user_id" -x xfce4-session || true); do
    process_display=
    process_dbus=
    process_runtime=
    if ! {
      while IFS= read -r -d '' entry; do
        case "$entry" in
          DISPLAY=*) process_display="${entry#*=}" ;;
          DBUS_SESSION_BUS_ADDRESS=*) process_dbus="${entry#*=}" ;;
          XDG_RUNTIME_DIR=*) process_runtime="${entry#*=}" ;;
        esac
      done <"/proc/$session_pid/environ"
    } 2>/dev/null; then
      continue
    fi
    if [[ "$process_display" =~ ^(.*:[0-9]+)\.0$ ]]; then
      process_display="${BASH_REMATCH[1]}"
    fi
    [ "$process_display" = "$requested_display" ] || continue
    session_count=$((session_count + 1))
    session_dbus="$process_dbus"
    session_runtime="$process_runtime"
  done
  if [ "$session_count" -ne 1 ] || [ -z "$session_dbus" ]; then
    echo "Expected one XFCE session with a D-Bus connection for UID $user_id on $DISPLAY; restart the desktop session and retry" >&2
    return 1
  fi
  export DBUS_SESSION_BUS_ADDRESS="$session_dbus"
  unset XDG_RUNTIME_DIR
  if [ -n "$session_runtime" ]; then
    export XDG_RUNTIME_DIR="$session_runtime"
  fi
}
crabbox_bind_xfce_session
