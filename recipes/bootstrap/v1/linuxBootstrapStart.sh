# bootcmd precedes write_files. Queue the unit without waiting for cloud-config,
# which supplies the script, users, SSH keys, and APT configuration it needs.
# Order after cloud-config without requiring its success, like cloud-final.
cloud-init-per instance crabbox-bootstrap-start sh -eu <<'CRABBOX_EARLY'
rm -f /var/lib/crabbox/bootstrapped
cat >/etc/systemd/system/crabbox-bootstrap.service <<'UNIT'
[Unit]
Description=Crabbox instance bootstrap
Wants=cloud-config.service network-online.target
After=cloud-config.service network-online.target
Before=cloud-final.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/bash -euxo pipefail /usr/local/lib/crabbox-bootstrap.sh
TimeoutStartSec=0
StandardOutput=journal+console
StandardError=journal+console
UNIT
systemctl daemon-reload
systemctl start --no-block crabbox-bootstrap.service
CRABBOX_EARLY
