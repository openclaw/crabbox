CS_VERSION={{sh:defaultCodeServerVersion}}
    case "$(uname -m)" in
      x86_64) CS_ARCH=amd64; CS_SHA256={{sh:defaultCodeServerAMD64SHA256}} ;;
      aarch64|arm64) CS_ARCH=arm64; CS_SHA256={{sh:defaultCodeServerARM64SHA256}} ;;
      *) echo "unsupported code-server architecture: $(uname -m)" >&2; exit 3 ;;
    esac
    if [ -z "$CS_SHA256" ]; then echo "missing code-server checksum for $CS_ARCH" >&2; exit 3; fi
    CS_INSTALL_DIR="$(mktemp -d)"
    trap 'rm -rf "$CS_INSTALL_DIR"' EXIT
    CS_ARCHIVE="$CS_INSTALL_DIR/code-server.tgz"
    retry curl -fsSL -o "$CS_ARCHIVE" "https://github.com/coder/code-server/releases/download/v${CS_VERSION}/code-server-${CS_VERSION}-linux-${CS_ARCH}.tar.gz"
    printf '%s  %s\n' "$CS_SHA256" "$CS_ARCHIVE" | sha256sum -c -
    tar -xzf "$CS_ARCHIVE" -C "$CS_INSTALL_DIR" --strip-components=1
    rm -f "$CS_ARCHIVE"
    rm -rf /usr/local/lib/code-server
    install -d -m 0755 /usr/local/lib/code-server
    cp -a "$CS_INSTALL_DIR/." /usr/local/lib/code-server/
    # cp -a preserves mktemp's private root mode; restore traversal for lease users.
    chmod 0755 /usr/local/lib/code-server
    ln -sfn /usr/local/lib/code-server/bin/code-server /usr/local/bin/code-server
    rm -rf "$CS_INSTALL_DIR"
    trap - EXIT