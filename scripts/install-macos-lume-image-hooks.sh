#!/bin/bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
firstboot_script="$root/macos-lume-firstboot.sh"
firstboot_plist="$root/macos-lume-firstboot-launchdaemon.plist"
cua_plist="$root/macos-cua-driver-launchagent.plist"

for file in "$firstboot_script" "$firstboot_plist"; do
  if [[ ! -f "$file" ]]; then
    echo "missing image hook asset: $file" >&2
    exit 2
  fi
done

sudo -v
sudo install -d -o root -g wheel -m 0755 /usr/local/libexec
sudo install -o root -g wheel -m 0755 \
  "$firstboot_script" \
  /usr/local/libexec/crabbox-lume-firstboot
sudo install -o root -g wheel -m 0644 \
  "$firstboot_plist" \
  /Library/LaunchDaemons/dev.crabbox.lume-firstboot.plist
marker="/var/db/crabbox-lume-machine-id"
sudo launchctl bootout system/dev.crabbox.lume-firstboot >/dev/null 2>&1 || true
sudo rm -f "$marker"
sudo launchctl bootstrap system \
  /Library/LaunchDaemons/dev.crabbox.lume-firstboot.plist

platform_uuid="$(/usr/sbin/ioreg -rd1 -c IOPlatformExpertDevice | /usr/bin/awk -F'"' '/IOPlatformUUID/ { print $(NF-1); exit }')"
firstboot_ready=false
for _ in {1..60}; do
  if [[ -n "$platform_uuid" ]] && [[ -r "$marker" ]] && [[ "$(<"$marker")" == "$platform_uuid" ]]; then
    firstboot_ready=true
    break
  fi
  sleep 1
done
if [[ "$firstboot_ready" != true ]]; then
  echo "Lume first-boot identity hook did not become ready" >&2
  exit 2
fi

if [[ -x /Applications/CuaDriver.app/Contents/MacOS/cua-driver ]]; then
  if [[ ! -f "$cua_plist" ]]; then
    echo "missing optional Cua Driver hook asset: $cua_plist" >&2
    exit 2
  fi
  mkdir -p "$HOME/Library/LaunchAgents"
  install -m 0644 "$cua_plist" \
    "$HOME/Library/LaunchAgents/com.trycua.cua-driver.plist"
  launchctl bootout "gui/$(id -u)/com.trycua.cua-driver" >/dev/null 2>&1 || true
  if launchctl bootstrap "gui/$(id -u)" \
    "$HOME/Library/LaunchAgents/com.trycua.cua-driver.plist"; then
    echo "installed and started optional Cua Driver LaunchAgent"
  else
    echo "installed optional Cua Driver LaunchAgent; it will start at the next GUI login" >&2
  fi
else
  echo "Cua Driver is not installed; skipped its optional LaunchAgent"
fi

echo "installed Lume first-boot SSH identity rotation"
