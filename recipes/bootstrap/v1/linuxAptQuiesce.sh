# Quiesce scheduled APT on every boot, including resumed instances. Stop only
# timers, without waiting on early-boot ordering; let active apt/dpkg finish.
# Keep unattended-upgrades.service's shutdown coordination intact.
if command -v systemctl >/dev/null 2>&1; then
  systemctl disable --now --no-block apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true
  systemctl mask apt-daily.service apt-daily-upgrade.service 2>/dev/null || true
fi
