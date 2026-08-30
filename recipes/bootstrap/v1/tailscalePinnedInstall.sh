TS_VERSION={{version}}
    case "$(uname -m)" in
      x86_64) TS_ARCH=amd64; TS_SHA256={{amd64SHA}} ;;
      aarch64|arm64) TS_ARCH=arm64; TS_SHA256={{arm64SHA}} ;;
      *) echo "unsupported Tailscale architecture: $(uname -m)" >&2; exit 3 ;;
    esac
    if [ -z "$TS_SHA256" ]; then echo "missing Tailscale checksum for $TS_ARCH" >&2; exit 3; fi
    TS_INSTALL_DIR="$(mktemp -d)"
    trap 'rm -rf "$TS_INSTALL_DIR"' EXIT
    TS_ARCHIVE="$TS_INSTALL_DIR/tailscale.tgz"
    retry curl -fsSL -o "$TS_ARCHIVE" "https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_${TS_ARCH}.tgz"
    printf '%s  %s\n' "$TS_SHA256" "$TS_ARCHIVE" | sha256sum -c -
    tar -xzf "$TS_ARCHIVE" -C "$TS_INSTALL_DIR" --strip-components=1
    install -m 0755 "$TS_INSTALL_DIR/tailscale" /usr/local/bin/tailscale
    install -m 0755 "$TS_INSTALL_DIR/tailscaled" /usr/local/sbin/tailscaled
    install -d -m 0755 /var/lib/tailscale /run/tailscale
    {
      printf '%s\n' '[Unit]'
      printf '%s\n' 'Description=Tailscale node agent'
      printf '%s\n' 'After=network-online.target'
      printf '%s\n' 'Wants=network-online.target'
      printf '%s\n' ''
      printf '%s\n' '[Service]'
      printf '%s\n' 'ExecStart=/usr/local/sbin/tailscaled --state=/var/lib/tailscale/tailscaled.state --socket=/run/tailscale/tailscaled.sock'
      printf '%s\n' 'Restart=on-failure'
      printf '%s\n' 'RuntimeDirectory=tailscale'
      printf '%s\n' 'StateDirectory=tailscale'
      printf '%s\n' ''
      printf '%s\n' '[Install]'
      printf '%s\n' 'WantedBy=multi-user.target'
    } >/etc/systemd/system/tailscaled.service
    rm -rf "$TS_INSTALL_DIR"
    trap - EXIT
    systemctl daemon-reload || true