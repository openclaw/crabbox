if [ ! -r /etc/os-release ]; then
      echo "Tailscale package install requires /etc/os-release" >&2
      exit 3
    fi
    . /etc/os-release
    TS_DIST_ID="${ID:-}"
    TS_CODENAME="${VERSION_CODENAME:-}"
    case "$TS_DIST_ID" in
      ubuntu|debian) ;;
      *) echo "unsupported Tailscale package distribution: $TS_DIST_ID" >&2; exit 3 ;;
    esac
    case "$TS_CODENAME" in
      ''|*[!a-z0-9.-]*) echo "invalid Tailscale package codename: $TS_CODENAME" >&2; exit 3 ;;
    esac
    install -d -m 0755 /usr/share/keyrings
    TS_KEYRING_TMP="$(mktemp)"
    trap 'rm -f "$TS_KEYRING_TMP"' EXIT
    retry curl -fsSL -o "$TS_KEYRING_TMP" "https://pkgs.tailscale.com/stable/${TS_DIST_ID}/${TS_CODENAME}.noarmor.gpg"
    printf '%s  %s\n' {{sh:defaultTailscaleKeyringSHA256}} "$TS_KEYRING_TMP" | sha256sum -c -
    install -m 0644 "$TS_KEYRING_TMP" /usr/share/keyrings/tailscale-archive-keyring.gpg
    printf 'deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/%s %s main\n' "$TS_DIST_ID" "$TS_CODENAME" >/etc/apt/sources.list.d/tailscale.list
    rm -f "$TS_KEYRING_TMP"
    trap - EXIT
    retry apt-get update
    retry apt-get install -y --no-install-recommends tailscale