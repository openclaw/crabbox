package cli

import (
	"fmt"
	"strings"
)

func cloudInit(cfg Config, publicKey string) string {
	portLines := ""
	for _, port := range sshPortCandidates(cfg.SSHPort, cfg.SSHFallbackPorts) {
		portLines += fmt.Sprintf("      Port %s\n", port)
	}
	readyChecks := cloudInitOptionalReadyChecks(cfg)
	writeFiles := cloudInitOptionalWriteFiles(cfg)
	bootstrap := cloudInitOptionalBootstrap(cfg)
	return fmt.Sprintf(`#cloud-config
package_update: false
package_upgrade: false
users:
  - name: %[1]s
    groups: sudo
    shell: /bin/bash
    sudo: ['ALL=(ALL) NOPASSWD:ALL']
    ssh_authorized_keys:
      - %[2]s
write_files:
  - path: /etc/ssh/sshd_config.d/99-crabbox-port.conf
    permissions: '0644'
    content: |
%[4]s
      PasswordAuthentication no
  - path: /usr/local/bin/crabbox-ready
    permissions: '0755'
    content: |
      #!/usr/bin/env bash
      set -euo pipefail
      git --version
      rsync --version >/dev/null
      curl --version >/dev/null
      jq --version >/dev/null
      test -f /var/lib/crabbox/bootstrapped
      test -w %[3]s
%[5]s
%[6]s
runcmd:
  - |
    bash -euxo pipefail <<'BOOT'
    export DEBIAN_FRONTEND=noninteractive
    cat >/etc/apt/apt.conf.d/80-crabbox-retries <<'APT'
    Acquire::Retries "8";
    Acquire::http::Timeout "30";
    Acquire::https::Timeout "30";
    APT
    retry() {
      n=1
      until "$@"; do
        if [ "$n" -ge 8 ]; then
          return 1
        fi
        sleep $((n * 5))
        n=$((n + 1))
      done
    }
    retry apt-get update
    retry apt-get install -y --no-install-recommends openssh-server ca-certificates curl git rsync jq
%[7]s
    mkdir -p %[3]s /var/cache/crabbox/pnpm /var/cache/crabbox/npm
    chown -R %[1]s:%[1]s %[3]s /var/cache/crabbox
    install -d /var/lib/crabbox
    touch /var/lib/crabbox/bootstrapped
    systemctl enable --now ssh
    systemctl restart ssh
    crabbox-ready
    BOOT
`, cfg.SSHUser, publicKey, cfg.WorkRoot, portLines, readyChecks, writeFiles, bootstrap)
}

func cloudInitOptionalReadyChecks(cfg Config) string {
	var b strings.Builder
	if cfg.Desktop {
		b.WriteString("      systemctl is-active --quiet crabbox-xvfb.service\n")
		b.WriteString("      systemctl is-active --quiet crabbox-desktop.service\n")
		b.WriteString("      systemctl is-active --quiet crabbox-desktop-session.service\n")
		b.WriteString("      systemctl is-active --quiet crabbox-x11vnc.service\n")
		b.WriteString("      ss -ltn | grep -q '127.0.0.1:5900'\n")
	}
	if cfg.Browser {
		b.WriteString("      test -s /var/lib/crabbox/browser.env\n")
		b.WriteString("      . /var/lib/crabbox/browser.env\n")
		b.WriteString("      test -x \"$BROWSER\"\n")
		b.WriteString("      \"$BROWSER\" --version >/dev/null\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func cloudInitOptionalWriteFiles(cfg Config) string {
	if !cfg.Desktop {
		return ""
	}
	return `  - path: /etc/systemd/system/crabbox-xvfb.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox Xvfb display
      After=network.target

      [Service]
      User=crabbox
      ExecStart=/usr/bin/Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp -ac
      Restart=always

      [Install]
      WantedBy=multi-user.target
  - path: /etc/systemd/system/crabbox-desktop.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox XFCE desktop session
      After=crabbox-xvfb.service
      Requires=crabbox-xvfb.service

      [Service]
      User=crabbox
      Environment=DISPLAY=:99
      ExecStart=/usr/bin/startxfce4
      Restart=always

      [Install]
      WantedBy=multi-user.target
  - path: /usr/local/bin/crabbox-desktop-session
    permissions: '0755'
    content: |
      #!/bin/sh
      set -eu
      export DISPLAY="${DISPLAY:-:99}"
      if command -v xsetroot >/dev/null 2>&1; then
        xsetroot -solid '#20242b' || true
      fi
      if command -v xterm >/dev/null 2>&1 && ! pgrep -u "$(id -u)" -f 'xterm -title Crabbox Desktop' >/dev/null 2>&1; then
        xterm -title 'Crabbox Desktop' -geometry 110x32+48+48 -bg '#111827' -fg '#e5e7eb' &
      fi
      tail -f /dev/null
  - path: /etc/systemd/system/crabbox-desktop-session.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox visible desktop helper
      After=crabbox-desktop.service
      Requires=crabbox-xvfb.service crabbox-desktop.service

      [Service]
      User=crabbox
      Environment=DISPLAY=:99
      ExecStart=/usr/local/bin/crabbox-desktop-session
      Restart=always

      [Install]
      WantedBy=multi-user.target
  - path: /etc/systemd/system/crabbox-x11vnc.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Crabbox loopback VNC server
      After=crabbox-xvfb.service
      Requires=crabbox-xvfb.service

      [Service]
      User=crabbox
      ExecStart=/usr/bin/x11vnc -display :99 -localhost -rfbport 5900 -forever -shared -rfbauth /var/lib/crabbox/vnc.pass
      Restart=always

      [Install]
      WantedBy=multi-user.target
`
}

func cloudInitOptionalBootstrap(cfg Config) string {
	var parts []string
	if cfg.Desktop {
		parts = append(parts, `    retry apt-get install -y --no-install-recommends xvfb xfce4 xfce4-terminal x11vnc xauth dbus-x11 x11-xserver-utils xterm fonts-dejavu-core fonts-liberation iproute2 openssl
    install -d -m 0750 -o crabbox -g crabbox /var/lib/crabbox
    if [ ! -s /var/lib/crabbox/vnc.password ]; then
      (umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)
    fi
    x11vnc -storepasswd "$(cat /var/lib/crabbox/vnc.password)" /var/lib/crabbox/vnc.pass >/dev/null
    chown crabbox:crabbox /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
    chmod 0600 /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
    systemctl daemon-reload
    systemctl enable --now crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service crabbox-x11vnc.service`)
	}
	if cfg.Browser {
		parts = append(parts, `    retry apt-get install -y --no-install-recommends gnupg
    browser_path=""
    if [ "$(dpkg --print-architecture)" = "amd64" ]; then
      install -d -m 0755 /etc/apt/trusted.gpg.d
      curl -fsSL https://dl.google.com/linux/linux_signing_key.pub > /etc/apt/trusted.gpg.d/google.asc
      chmod 0644 /etc/apt/trusted.gpg.d/google.asc
      echo "deb [arch=amd64] https://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list
      if apt-get update && retry apt-get install -y --no-install-recommends google-chrome-stable; then
        browser_path="$(command -v google-chrome || true)"
      else
        rm -f /etc/apt/sources.list.d/google-chrome.list
        retry apt-get update || true
      fi
    fi
    if [ -z "$browser_path" ]; then
      if apt-cache show chromium >/dev/null 2>&1 && retry apt-get install -y --no-install-recommends chromium; then
        browser_path="$(command -v chromium || true)"
      elif apt-cache show chromium-browser >/dev/null 2>&1 && retry apt-get install -y --no-install-recommends chromium-browser; then
        browser_path="$(command -v chromium-browser || true)"
      fi
    fi
    if [ -n "$browser_path" ]; then
      printf 'CHROME_BIN=%s\nBROWSER=%s\n' "$browser_path" "$browser_path" > /var/lib/crabbox/browser.env
      chown crabbox:crabbox /var/lib/crabbox/browser.env
      chmod 0644 /var/lib/crabbox/browser.env
    fi`)
	}
	return strings.Join(parts, "\n")
}
