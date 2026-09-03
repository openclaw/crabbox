# Socket-activated sshd inherits listening descriptors. Reload the port generator
# before restarting the socket, including when readiness skips package postinst.
systemctl daemon-reload || true
if systemctl is-active --quiet ssh.socket; then
  timeout 30s systemctl restart ssh.socket || true
else
  timeout 30s systemctl restart ssh || true
fi
