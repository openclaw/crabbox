#!/usr/bin/env bash
set -euo pipefail

pnpm_version="${CRABBOX_LINUX_PNPM_VERSION:-11.1.0}"
node_major="${CRABBOX_LINUX_NODE_MAJOR:-24}"
trufflehog_version="3.95.9"
docker_images="${CRABBOX_LINUX_DOCKER_IMAGES:-hello-world ubuntu:24.04 node:24-bookworm}"
install_desktop="${CRABBOX_LINUX_DESKTOP_TOOLS:-1}"
install_browser="${CRABBOX_LINUX_BROWSER:-1}"
apt_keyrings_dir="${CRABBOX_LINUX_APT_KEYRINGS_DIR:-/etc/apt/keyrings}"
apt_sources_dir="${CRABBOX_LINUX_APT_SOURCES_DIR:-/etc/apt/sources.list.d}"
apt_conf_dir="${CRABBOX_LINUX_APT_CONF_DIR:-/etc/apt/apt.conf.d}"
os_release_file="${CRABBOX_LINUX_OS_RELEASE_FILE:-/etc/os-release}"
chrome_policy_dir="${CRABBOX_LINUX_CHROME_POLICY_DIR:-/etc/opt/chrome/policies/managed}"
chromium_policy_dir="${CRABBOX_LINUX_CHROMIUM_POLICY_DIR:-/etc/chromium/policies/managed}"
browser_bin_dir="${CRABBOX_LINUX_BROWSER_BIN_DIR:-/usr/local/bin}"
browser_state_dir="${CRABBOX_LINUX_BROWSER_STATE_DIR:-/var/lib/crabbox}"
chrome_defaults_file="${CRABBOX_LINUX_CHROME_DEFAULTS_FILE:-/etc/default/google-chrome}"
trufflehog_bin_dir="${CRABBOX_LINUX_TRUFFLEHOG_BIN_DIR:-/usr/local/bin}"
nodesource_signing_key_fingerprint="6F71F525282841EEDAF851B42F59B5F99B1BE0B4"
docker_signing_key_fingerprint="9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
google_linux_signing_key_fingerprint="EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796"

log() {
  printf 'linux-tools: %s\n' "$*" >&2
}

need_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    if command -v sudo >/dev/null 2>&1; then
      exec sudo -E bash "$0" "$@"
    fi
    log "sudo is required when not running as root"
    exit 2
  fi
}

retry() {
  local n=1
  until "$@"; do
    if [[ "$n" -ge 8 ]]; then
      return 1
    fi
    sleep "$((n * 5))"
    n="$((n + 1))"
  done
}

apt_install() {
  retry apt-get install -y --no-install-recommends "$@"
}

os_release_value() {
  local key="$1"
  awk -F= -v key="$key" '
    $1 == key {
      value = $0
      sub(/^[^=]*=/, "", value)
      first = substr(value, 1, 1)
      last = substr(value, length(value), 1)
      if ((first == "\"" && last == "\"") || (first == sprintf("%c", 39) && last == sprintf("%c", 39))) {
        value = substr(value, 2, length(value) - 2)
      }
      print value
      exit
    }
  ' "$os_release_file"
}

install_apt_keyring() {
  local url="$1"
  local target="$2"
  local expected_fingerprint="$3"
  local actual_fingerprint=""
  local downloaded_key
  local key_home
  local tmp_dir
  local tmp_key
  install -d -m 0755 "$(dirname "$target")"
  tmp_dir="$(mktemp -d "${target}.tmp.XXXXXX")"
  tmp_key="$tmp_dir/keyring.gpg"
  downloaded_key="$tmp_dir/downloaded.asc"
  key_home="$tmp_dir/gnupg"
  install -d -m 0700 "$key_home"
  if curl -fsSL "$url" >"$downloaded_key" &&
    GNUPGHOME="$key_home" gpg --batch --import "$downloaded_key" >/dev/null 2>&1; then
    actual_fingerprint="$(
      GNUPGHOME="$key_home" gpg --batch --with-colons --fingerprint "$expected_fingerprint" 2>/dev/null |
        awk -F: '$1 == "fpr" { print $10; exit }' || true
    )"
    if [[ "$actual_fingerprint" == "$expected_fingerprint" ]] &&
      GNUPGHOME="$key_home" gpg --batch --export "$expected_fingerprint" >"$tmp_key" &&
      [[ -s "$tmp_key" ]]; then
      chmod 0644 "$tmp_key"
      mv -f "$tmp_key" "$target"
      rm -rf "$tmp_dir"
      return 0
    fi
  fi
  rm -rf "$tmp_dir"
  return 1
}

docker_packages_installed() {
  local package
  for package in docker-ce docker-ce-cli containerd.io; do
    if ! dpkg-query -W -f='${Status}' "$package" 2>/dev/null | grep -qx 'install ok installed'; then
      return 1
    fi
  done
  return 0
}

node_toolchain_ready() {
  command -v node >/dev/null 2>&1 &&
    node --version | grep -q "^v${node_major}\\." &&
    command -v npm >/dev/null 2>&1 &&
    command -v corepack >/dev/null 2>&1
}

add_nodesource() {
  local source="$apt_sources_dir/nodesource.list"
  local source_tmp
  if node_toolchain_ready && [[ ! -e "$source" ]]; then
    return 0
  fi
  install -d -m 0755 "$apt_sources_dir"
  install_apt_keyring \
    "https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key" \
    "$apt_keyrings_dir/nodesource.gpg" \
    "$nodesource_signing_key_fingerprint"
  source_tmp="$(mktemp "${source}.tmp.XXXXXX")"
  if ! printf 'deb [signed-by=%s/nodesource.gpg] https://deb.nodesource.com/node_%s.x nodistro main\n' "$apt_keyrings_dir" "$node_major" \
    >"$source_tmp" || ! chmod 0644 "$source_tmp" || ! mv -f "$source_tmp" "$source"; then
    rm -f "$source_tmp"
    return 1
  fi
}

add_docker_repo() {
  local source="$apt_sources_dir/docker.list"
  local source_tmp
  if docker_packages_installed && [[ ! -e "$source" ]]; then
    return 0
  fi
  local distro_id codename arch
  distro_id="$(os_release_value ID)"
  codename="$(os_release_value VERSION_CODENAME)"
  arch="$(dpkg --print-architecture)"
  case "$distro_id" in
    debian|ubuntu) ;;
    "")
      log "could not determine Debian/Ubuntu distribution ID"
      exit 2
      ;;
    *)
      log "unsupported Docker repository distribution: $distro_id"
      exit 2
      ;;
  esac
  if [[ -z "$codename" ]]; then
    log "could not determine Debian/Ubuntu codename"
    exit 2
  fi
  install -d -m 0755 "$apt_sources_dir"
  install_apt_keyring \
    "https://download.docker.com/linux/${distro_id}/gpg" \
    "$apt_keyrings_dir/docker.gpg" \
    "$docker_signing_key_fingerprint"
  source_tmp="$(mktemp "${source}.tmp.XXXXXX")"
  if ! printf 'deb [arch=%s signed-by=%s/docker.gpg] https://download.docker.com/linux/%s %s stable\n' "$arch" "$apt_keyrings_dir" "$distro_id" "$codename" \
    >"$source_tmp" || ! chmod 0644 "$source_tmp" || ! mv -f "$source_tmp" "$source"; then
    rm -f "$source_tmp"
    return 1
  fi
}

install_chrome_or_chromium() {
  local browser_path=""
  local chrome_defaults_tmp=""
  if [[ "$(dpkg --print-architecture)" == "amd64" ]]; then
    install -d -m 0755 "$apt_sources_dir"
    if install_apt_keyring \
      https://dl.google.com/linux/linux_signing_key.pub \
      "$apt_keyrings_dir/google-linux.gpg" \
      "$google_linux_signing_key_fingerprint"; then
      install -d -m 0755 "$(dirname "$chrome_defaults_file")"
      chrome_defaults_tmp="$(mktemp "${chrome_defaults_file}.tmp.XXXXXX")"
      if [[ -f "$chrome_defaults_file" ]]; then
        awk '!/^[[:space:]]*repo_add_once=/ && !/^[[:space:]]*repo_reenable_on_distupgrade=/' "$chrome_defaults_file" >"$chrome_defaults_tmp"
      fi
      printf '%s\n' 'repo_add_once="false"' 'repo_reenable_on_distupgrade="false"' >>"$chrome_defaults_tmp"
      chmod 0644 "$chrome_defaults_tmp"
      mv -f "$chrome_defaults_tmp" "$chrome_defaults_file"
      rm -f "$apt_sources_dir/google-chrome.list" "$apt_sources_dir/google-chrome.sources"
      printf 'deb [arch=amd64 signed-by=%s/google-linux.gpg] https://dl.google.com/linux/chrome/deb/ stable main\n' "$apt_keyrings_dir" \
        >"$apt_sources_dir/crabbox-google-chrome.list"
      if retry apt-get update && apt_install google-chrome-stable; then
        rm -f "$apt_sources_dir/google-chrome.list" "$apt_sources_dir/google-chrome.sources"
        browser_path="$(command -v google-chrome || true)"
      else
        rm -f "$apt_sources_dir/crabbox-google-chrome.list" "$apt_sources_dir/google-chrome.list" "$apt_sources_dir/google-chrome.sources"
        retry apt-get update || true
      fi
    else
      log "Google Linux signing key verification failed; trying Chromium fallback"
    fi
  fi
  if [[ -z "$browser_path" ]]; then
    if apt-cache show chromium >/dev/null 2>&1 && apt_install chromium; then
      browser_path="$(command -v chromium || true)"
    elif apt-cache show chromium-browser >/dev/null 2>&1 && apt_install chromium-browser; then
      browser_path="$(command -v chromium-browser || true)"
    fi
  fi
  if [[ -n "$browser_path" ]]; then
    install -d -m 0755 "$chrome_policy_dir" "$chromium_policy_dir" "$browser_bin_dir" "$browser_state_dir"
    printf '%s\n' '{"DefaultBrowserSettingEnabled":false,"MetricsReportingEnabled":false,"PromotionalTabsEnabled":false}' \
      >"$chrome_policy_dir/crabbox.json"
    cp "$chrome_policy_dir/crabbox.json" "$chromium_policy_dir/crabbox.json"
    cat >"$browser_bin_dir/crabbox-browser" <<EOF
#!/bin/sh
exec "$browser_path" --no-first-run --no-default-browser-check --disable-default-apps --window-size=1500,900 --window-position=80,80 "\$@"
EOF
    chmod 0755 "$browser_bin_dir/crabbox-browser"
    printf 'CHROME_BIN=%s/crabbox-browser\nBROWSER=%s/crabbox-browser\n' "$browser_bin_dir" "$browser_bin_dir" \
      >"$browser_state_dir/browser.env"
    chmod 0644 "$browser_state_dir/browser.env"
  fi
}

install_node_pnpm() {
  apt_install nodejs
  command -v npm >/dev/null
  command -v corepack >/dev/null
  corepack enable
  corepack prepare "pnpm@$pnpm_version" --activate
  command -v pnpm >/dev/null
}

trufflehog_sha256_for_arch() {
  case "$1" in
    amd64) printf '%s\n' "f6d1106b85107d79527ed7a5b98b592beadd8b770dc3c9e8c1ad99e1b2cf127e" ;;
    arm64) printf '%s\n' "9d9c2ec4ea36a089a9c5aaafe1969d176013ddf9f44d68e8cd75291aed8c83ed" ;;
    *)
      log "unsupported TruffleHog architecture: $1"
      return 1
      ;;
  esac
}

trufflehog_binary_ready() {
  local binary="$1"
  [[ -x "$binary" ]] &&
    "$binary" --version 2>/dev/null |
      awk -v version="$trufflehog_version" '
        {
          for (field = 1; field <= NF; field++) {
            if ($field == version) {
              found = 1
            }
          }
        }
        END { exit found ? 0 : 1 }
      '
}

trufflehog_ready() {
  trufflehog_binary_ready "$trufflehog_bin_dir/trufflehog"
}

install_trufflehog() {
  local arch
  local archive
  local candidate
  local checksum
  local target
  local tmp_dir
  local url
  arch="$(dpkg --print-architecture)"
  checksum="$(trufflehog_sha256_for_arch "$arch")"
  archive="trufflehog_${trufflehog_version}_linux_${arch}.tar.gz"
  url="https://github.com/trufflesecurity/trufflehog/releases/download/v${trufflehog_version}/${archive}"
  tmp_dir="$(mktemp -d)"

  if ! retry curl -fsSL --output "$tmp_dir/$archive" "$url" ||
    ! (
      cd "$tmp_dir"
      printf '%s  %s\n' "$checksum" "$archive" | sha256sum -c -
    ) ||
    ! tar --no-same-owner -xzf "$tmp_dir/$archive" -C "$tmp_dir" trufflehog; then
    rm -rf "$tmp_dir"
    return 1
  fi

  install -d -m 0755 "$trufflehog_bin_dir"
  target="$trufflehog_bin_dir/trufflehog"
  candidate="$(mktemp "${target}.tmp.XXXXXX")"
  if ! install -m 0755 "$tmp_dir/trufflehog" "$candidate" ||
    ! trufflehog_binary_ready "$candidate"; then
    rm -f "$candidate"
    rm -rf "$tmp_dir"
    return 1
  fi
  rm -rf "$tmp_dir"
  mv -f "$candidate" "$target"
}

install_docker() {
  apt_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  systemctl enable --now docker || service docker start
  usermod -aG docker crabbox 2>/dev/null || true
  docker version
  docker compose version
  if [[ -n "$docker_images" ]]; then
    # shellcheck disable=SC2206
    local images=($docker_images)
    local image
    for image in "${images[@]}"; do
      retry docker pull "$image" || log "docker pull failed for $image; continuing"
    done
  fi
}

prepare_fast_boot() {
  install -d -m 1777 /var/cache/crabbox /var/cache/crabbox/pnpm /var/cache/crabbox/npm /var/cache/crabbox/corepack /var/cache/crabbox/docker
  systemctl disable --now apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true
  systemctl mask apt-daily.service apt-daily-upgrade.service 2>/dev/null || true
  cloud-init clean --logs --seed 2>/dev/null || true
  sync
}

print_versions() {
  # shellcheck disable=SC1091
  . "$os_release_file"
  printf 'os=%s %s\n' "${PRETTY_NAME:-unknown}" "$(uname -m)"
  git --version
  gh --version | head -n 1
  jq --version
  rg --version | head -n 1
  fd --version || fdfind --version
  python3 --version
  node --version
  npm --version
  corepack --version
  pnpm --version
  "$trufflehog_bin_dir/trufflehog" --version
  docker --version
  docker compose version
}

main() {
  need_root "$@"
  export DEBIAN_FRONTEND=noninteractive
  install -d -m 0755 "$apt_conf_dir" "$apt_sources_dir"
  cat >"$apt_conf_dir/80-crabbox-retries" <<'APT'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
APT

  apt_get_base=(apt-transport-https ca-certificates curl gnupg lsb-release software-properties-common)
  retry apt-get update
  apt_install "${apt_get_base[@]}"
  add_nodesource
  add_docker_repo
  retry apt-get update
  apt_install \
    build-essential \
    pkg-config \
    git \
    git-lfs \
    gh \
    jq \
    yq \
    ripgrep \
    fd-find \
    fzf \
    coreutils \
    tar \
    sed \
    findutils \
    rsync \
    unzip \
    zip \
    shellcheck \
    shfmt \
    python3 \
    python3-pip \
    python3-venv \
    python3-dev \
    netcat-openbsd \
    iproute2 \
    openssl

  if [[ ! -e /usr/local/bin/fd && -x /usr/bin/fdfind ]]; then
    ln -sf /usr/bin/fdfind /usr/local/bin/fd
  fi

  if [[ "$install_desktop" == "1" ]]; then
    apt_install xvfb xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-terminal xfconf xfce4-settings x11vnc xauth dbus-x11 x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl xclip xsel fonts-dejavu-core fonts-liberation
  fi
  if [[ "$install_browser" == "1" ]]; then
    install_chrome_or_chromium
  fi
  install_node_pnpm
  install_trufflehog
  install_docker
  prepare_fast_boot
  print_versions
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
