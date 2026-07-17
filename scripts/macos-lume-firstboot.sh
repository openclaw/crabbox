#!/bin/bash
set -euo pipefail

marker="/var/db/crabbox-lume-machine-id"
platform_uuid="$(/usr/sbin/ioreg -rd1 -c IOPlatformExpertDevice | /usr/bin/awk -F'"' '/IOPlatformUUID/ { print $(NF-1); exit }')"

if [[ -z "$platform_uuid" ]]; then
  echo "could not determine IOPlatformUUID" >&2
  exit 1
fi

if [[ -r "$marker" ]] && [[ "$(<"$marker")" == "$platform_uuid" ]]; then
  exit 0
fi

echo "new Lume machine identity detected; rotating OpenSSH host keys"
/bin/rm -f /etc/ssh/ssh_host_*
/usr/bin/ssh-keygen -A
expected_host_key="$(/usr/bin/awk '$1 == "ssh-ed25519" { print $2; exit }' /etc/ssh/ssh_host_ed25519_key.pub)"
if [[ -z "$expected_host_key" ]]; then
  echo "could not read the new OpenSSH ED25519 host key" >&2
  exit 1
fi

/bin/launchctl kickstart -k system/com.openssh.sshd >/dev/null

# Publish the clone identity only after sshd actually serves the new key.
# Crabbox treats the marker as the boundary between its unpinned identity poll
# and the connection that pins this key in the lease's known_hosts file.
sshd_ready=false
for _ in {1..60}; do
  served_host_key="$(
    /usr/bin/ssh-keyscan -T 2 -t ed25519 127.0.0.1 2>/dev/null |
      /usr/bin/awk '$2 == "ssh-ed25519" { print $3; exit }' || true
  )"
  if [[ -n "$served_host_key" ]] && [[ "$served_host_key" == "$expected_host_key" ]]; then
    sshd_ready=true
    break
  fi
  /bin/sleep 1
done
if [[ "$sshd_ready" != true ]]; then
  echo "sshd did not serve the new ED25519 host key after rotation" >&2
  exit 1
fi

tmp="$(/usr/bin/mktemp "${marker}.XXXXXX")"
trap '/bin/rm -f "$tmp"' EXIT
/usr/bin/printf '%s\n' "$platform_uuid" >"$tmp"
/usr/sbin/chown root:wheel "$tmp"
/bin/chmod 0644 "$tmp"
/bin/mv -f "$tmp" "$marker"
trap - EXIT
