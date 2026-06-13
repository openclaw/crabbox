#!/usr/bin/env bash
set -euo pipefail

desktop_user="${CRABBOX_DESKTOP_USER:-crabbox}"
display="${CRABBOX_DESKTOP_DISPLAY:-:99}"
geometry="${CRABBOX_DESKTOP_GEOMETRY:-1920x1080x24}"

log() {
  printf 'crabbox-desktop: %s\n' "$*" >&2
}

validate_config() {
  if [[ ! "$desktop_user" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]]; then
    log "CRABBOX_DESKTOP_USER must be a conservative POSIX username"
    exit 2
  fi
  if [[ ! "$display" =~ ^:[0-9]{1,4}$ ]]; then
    log "CRABBOX_DESKTOP_DISPLAY must look like :99"
    exit 2
  fi
  if [[ ! "$geometry" =~ ^[0-9]{2,5}x[0-9]{2,5}x(8|16|24|32)$ ]]; then
    log "CRABBOX_DESKTOP_GEOMETRY must look like 1920x1080x24"
    exit 2
  fi
}

need_root() {
  if [[ "$(id -u)" -eq 0 ]]; then
    return
  fi
  if command -v sudo >/dev/null 2>&1; then
    exec sudo -E bash "$0" "$@"
  fi
  log "root or sudo is required"
  exit 2
}

retry() {
  local attempt=1
  until "$@"; do
    if [[ "$attempt" -ge 8 ]]; then
      return 1
    fi
    sleep "$((attempt * 5))"
    attempt="$((attempt + 1))"
  done
}

install_packages() {
  if ! command -v apt-get >/dev/null 2>&1; then
    log "this bootstrap currently supports Debian and Ubuntu guests"
    exit 2
  fi
  export DEBIAN_FRONTEND=noninteractive
  retry apt-get update
  retry apt-get install -y --no-install-recommends \
    arc-theme \
    dbus-x11 \
    fonts-dejavu-core \
    fonts-liberation \
    iproute2 \
    novnc \
    openssl \
    procps \
    scrot \
    sudo \
    util-linux \
    x11vnc \
    x11-xserver-utils \
    xauth \
    xclip \
    xdotool \
    xsel \
    xterm \
    xvfb \
    xfce4-panel \
    xfce4-session \
    xfce4-settings \
    xfce4-terminal \
    xfconf \
    xfdesktop4 \
    xfwm4 \
    websockify
}

ensure_user() {
  if id "$desktop_user" >/dev/null 2>&1; then
    return
  fi
  useradd --create-home --shell /bin/bash "$desktop_user"
}

require_safe_managed_directory() {
  local path="$1"
  if [[ -L "$path" ]] || [[ -e "$path" && ! -d "$path" ]]; then
    log "refusing unsafe managed directory: $path"
    exit 2
  fi
}

require_safe_managed_file() {
  local path="$1"
  if [[ -L "$path" ]] || [[ -e "$path" && ! -f "$path" ]]; then
    log "refusing unsafe managed file: $path"
    exit 2
  fi
  if [[ -e "$path" && "$(stat -c %h -- "$path")" != "1" ]]; then
    log "refusing multiply-linked managed file: $path"
    exit 2
  fi
}

write_managed_file() {
  local path="$1"
  local mode="$2"
  local owner="$3"
  local group="$4"
  local dir tmp
  dir="$(dirname "$path")"
  require_safe_managed_file "$path"
  tmp="$(mktemp "$dir/.crabbox-managed.XXXXXX")"
  if ! cat >"$tmp"; then
    rm -f -- "$tmp"
    return 1
  fi
  chown "$owner:$group" "$tmp"
  chmod "$mode" "$tmp"
  mv -fT -- "$tmp" "$path"
}

install_credentials() {
	local desktop_group
	desktop_group="$(id -gn "$desktop_user")"
	require_safe_managed_directory /var/lib/crabbox
	install -d -m 0750 -o root -g "$desktop_group" /var/lib/crabbox
	for path in /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass /var/lib/crabbox/desktop.env; do
		require_safe_managed_file "$path"
	done
	openssl rand -base64 18 | write_managed_file /var/lib/crabbox/vnc.password 0600 "$desktop_user" "$desktop_group"
	rm -f -- /var/lib/crabbox/vnc.pass
	printf 'CRABBOX_DESKTOP_ENV=xfce\nDISPLAY=%s\n' "$display" \
		| write_managed_file /var/lib/crabbox/desktop.env 0640 root "$desktop_group"
}

install_reset_helper() {
	require_safe_managed_directory /usr/local/bin
	install -d -m 0755 -o root -g root /usr/local/bin
	write_managed_file /usr/local/bin/crabbox-start-desktop 0755 root root <<'EOF'
#!/bin/bash
set -euo pipefail
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH

/usr/bin/systemctl restart crabbox-desktop.service crabbox-x11vnc.service
for attempt in {1..30}; do
  if /usr/bin/systemctl is-active --quiet crabbox-desktop.service \
    && /usr/bin/systemctl is-active --quiet crabbox-x11vnc.service; then
    exit 0
  fi
  /usr/bin/sleep 1
done
/usr/bin/systemctl --no-pager --full status crabbox-desktop.service crabbox-x11vnc.service >&2 || true
exit 5
EOF
	require_safe_managed_directory /etc/sudoers.d
	install -d -m 0755 -o root -g root /etc/sudoers.d
	printf '%s ALL=(root) NOPASSWD: /bin/bash /usr/local/bin/crabbox-start-desktop\n' "$desktop_user" \
		| write_managed_file /etc/sudoers.d/crabbox-desktop-reset 0440 root root
	visudo -cf /etc/sudoers.d/crabbox-desktop-reset >/dev/null
}

install_services() {
	write_managed_file /etc/systemd/system/crabbox-xvfb.service 0644 root root <<EOF
[Unit]
Description=Crabbox virtual X display
After=network.target

[Service]
User=$desktop_user
Environment=DISPLAY=$display
ExecStart=/usr/bin/Xvfb $display -screen 0 $geometry -nolisten tcp -ac
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

	write_managed_file /etc/systemd/system/crabbox-desktop.service 0644 root root <<EOF
[Unit]
Description=Crabbox XFCE desktop
After=crabbox-xvfb.service
Requires=crabbox-xvfb.service

[Service]
User=$desktop_user
Environment=DISPLAY=$display
ExecStart=/usr/bin/startxfce4
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

	write_managed_file /etc/systemd/system/crabbox-x11vnc.service 0644 root root <<EOF
[Unit]
Description=Crabbox loopback VNC server
After=crabbox-xvfb.service crabbox-desktop.service
Requires=crabbox-xvfb.service crabbox-desktop.service

[Service]
User=$desktop_user
ExecStart=/usr/bin/x11vnc -display $display -localhost -rfbport 5900 -forever -shared -passwdfile /var/lib/crabbox/vnc.password -wait 16 -defer 8 -nowait_bog
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable crabbox-xvfb.service crabbox-desktop.service crabbox-x11vnc.service
  systemctl restart crabbox-xvfb.service crabbox-desktop.service crabbox-x11vnc.service
}

verify_desktop() {
  local attempt
  for attempt in {1..30}; do
    if systemctl is-active --quiet crabbox-xvfb.service \
      && systemctl is-active --quiet crabbox-desktop.service \
      && systemctl is-active --quiet crabbox-x11vnc.service \
      && ss -ltn | awk '$4 ~ /127\.0\.0\.1:5900$/ || $4 ~ /\[::1\]:5900$/ { found=1 } END { exit !found }'; then
      log "ready user=$desktop_user display=$display vnc=127.0.0.1:5900"
      return
    fi
    sleep 1
  done
  log "desktop services did not become ready"
  systemctl --no-pager --full status crabbox-xvfb.service crabbox-desktop.service crabbox-x11vnc.service >&2 || true
  exit 5
}

main() {
  validate_config
  need_root "$@"
  install_packages
  ensure_user
  install_credentials
  install_reset_helper
  install_services
  verify_desktop
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
