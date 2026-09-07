#!/bin/bash
# User-data stdout/stderr are logged; never trace generated account credentials.
set +x
set -euo pipefail
crabbox_user={{user}}
crabbox_work_root={{workRoot}}
crabbox_public_key={{publicKey}}
crabbox_ssh_ports=({{ports}})
id "$crabbox_user" >/dev/null
install -d -m 0755 "$crabbox_work_root" /var/db/crabbox
chown -R "$crabbox_user":staff "$crabbox_work_root"
user_home="$(dscl . -read "/Users/$crabbox_user" NFSHomeDirectory 2>/dev/null | awk '{print $2; exit}')"
if [ -z "$user_home" ]; then
  user_home="/Users/$crabbox_user"
fi
install -d -m 0700 -o "$crabbox_user" -g staff "$user_home/.ssh"
authorized_keys="$user_home/.ssh/authorized_keys"
touch "$authorized_keys"
if [ -n "$crabbox_public_key" ] && ! grep -qxF "$crabbox_public_key" "$authorized_keys"; then
  printf '%s\n' "$crabbox_public_key" >>"$authorized_keys"
fi
chown "$crabbox_user":staff "$user_home/.ssh" "$authorized_keys"
chmod 0700 "$user_home/.ssh"
chmod 0600 "$authorized_keys"
sshd_config=/etc/ssh/sshd_config
touch "$sshd_config"
tmp_config="$(mktemp)"
awk '
  /^# crabbox ssh ports begin$/ { skip=1; next }
  /^# crabbox ssh ports end$/ { skip=0; next }
  !skip { print }
' "$sshd_config" >"$tmp_config"
cat "$tmp_config" >"$sshd_config"
rm -f "$tmp_config"
{
  printf '%s\n' '# crabbox ssh ports begin'
  for port in "${crabbox_ssh_ports[@]}"; do
    printf 'Port %s\n' "$port"
  done
  printf '%s\n' 'PubkeyAuthentication yes'
  printf '%s\n' 'PasswordAuthentication no'
  printf '%s\n' '# crabbox ssh ports end'
} >>"$sshd_config"
/usr/sbin/sshd -t -f "$sshd_config"
systemsetup -setremotelogin on || true
launchctl enable system/com.openssh.sshd || true
launchctl load -w /System/Library/LaunchDaemons/ssh.plist || true
launchctl kickstart -k system/com.openssh.sshd || true
if [ ! -s /var/db/crabbox/vnc.password ]; then
  set +o pipefail
  pw="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 16)"
  set -o pipefail
  if [ "${#pw}" -ne 16 ]; then
    echo "failed to generate vnc password" >&2
    exit 1
  fi
  (umask 077 && printf '%s\n' "$pw" >/var/db/crabbox/vnc.password)
  # EC2 user data has no TTY, so dscl's password prompt reads stdin.
  dscl . -passwd /Users/{{user}} </var/db/crabbox/vnc.password
fi
chmod 0600 /var/db/crabbox/vnc.password
launchctl enable system/com.apple.screensharing || true
launchctl load -w /System/Library/LaunchDaemons/com.apple.screensharing.plist || true
launchctl kickstart -k system/com.apple.screensharing || true
cat >/usr/local/bin/crabbox-ready <<'READY'
#!/bin/bash
set -euo pipefail
rsync --version >/dev/null
curl --version >/dev/null
test -w {{workRoot}}
ssh_ready=0
for port in {{ports}}; do
  if nc -z 127.0.0.1 "$port"; then
    ssh_ready=1
    break
  fi
done
test "$ssh_ready" -eq 1
nc -z 127.0.0.1 5900
READY
chmod 0755 /usr/local/bin/crabbox-ready
/usr/local/bin/crabbox-ready
