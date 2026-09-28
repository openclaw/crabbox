# Enabling an already-enabled SysV-backed SSH service runs slow compatibility
# helpers. Batch the new unit's reload instead of reloading for each enable.
systemctl is-enabled --quiet ssh || systemctl enable --no-reload ssh || true
systemctl enable --no-reload crabbox-workspace-ready.service
systemctl daemon-reload
systemctl start --no-block crabbox-workspace-ready.service
