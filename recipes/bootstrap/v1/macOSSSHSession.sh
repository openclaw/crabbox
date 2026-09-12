#!/bin/bash
set +x
set -euo pipefail
export LC_ALL=C
sshd_config=/etc/ssh/sshd_config
session_config="$(mktemp /etc/ssh/.crabbox-session.XXXXXX)"
trap 'rm -f "$session_config"' EXIT
# PAM establishes the user's launchd namespace, which Apple's nohup requires.
# Keep password and keyboard-interactive authentication disabled.
{
  printf '%s\n' '# crabbox ssh session begin' 'UsePAM yes' 'PasswordAuthentication no' 'KbdInteractiveAuthentication no' '# crabbox ssh session end'
  awk '
    /^# crabbox ssh session begin$/ { skip=1; next }
    /^# crabbox ssh session end$/ { skip=0; next }
    !skip { print }
  ' "$sshd_config"
} >"$session_config"
/usr/sbin/sshd -t -f "$session_config"
chmod 0644 "$session_config"
mv "$session_config" "$sshd_config"
# macOS launchd starts sshd per connection; a new connection reads this file.
