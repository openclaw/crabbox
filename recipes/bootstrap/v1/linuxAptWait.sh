# Outside bootcmd: masking does not interrupt an already-running update. Wait
# before explicit installs; oneshot APT services remain "activating" while busy.
if command -v systemctl >/dev/null 2>&1; then
  while systemctl show --property=ActiveState --value apt-daily.service apt-daily-upgrade.service 2>/dev/null | grep -Eq '^(activating|active|reloading|deactivating)$'; do
    sleep 1
  done
fi
