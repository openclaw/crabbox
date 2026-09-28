# Package postinst or a prepared image may already have the requested listeners.
# Socket-activated sshd inherits descriptors, so restart the socket on mismatch.
crabbox_ssh_listeners_match() {
  local configured listening port
  configured=$(/usr/sbin/sshd -T) || return 1
  configured=$(printf '%s\n' "$configured" | awk '$1 == "port" {print $2}')
  [ -n "$configured" ] || return 1
  listening=$(ss -H -ltnp) || return 1
  listening=$(printf '%s\n' "$listening" | awk '/"sshd"|"systemd"/ {n=split($4, address, ":"); print address[n]}')
  for port in $configured; do
    printf '%s\n' "$listening" | grep -Fxq "$port" || return 1
  done
}
if ! crabbox_ssh_listeners_match; then
  systemctl daemon-reload || true
  if systemctl is-active --quiet ssh.socket; then
    timeout 30s systemctl restart ssh.socket || true
  else
    timeout 30s systemctl restart ssh || true
  fi
fi
