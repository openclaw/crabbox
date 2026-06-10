#!/usr/bin/env bash
set -euo pipefail

pnpm_version="${CRABBOX_LINUX_PNPM_VERSION:-11.1.0}"
node_major="${CRABBOX_LINUX_NODE_MAJOR:-24}"
docker_images="${CRABBOX_LINUX_DOCKER_IMAGES:-hello-world ubuntu:24.04 node:24-bookworm}"
install_desktop="${CRABBOX_LINUX_DESKTOP_TOOLS:-1}"
install_browser="${CRABBOX_LINUX_BROWSER:-1}"

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

docker_packages_installed() {
  local package
  for package in docker-ce docker-ce-cli containerd.io; do
    if ! dpkg-query -W -f='${Status}' "$package" 2>/dev/null | grep -qx 'install ok installed'; then
      return 1
    fi
  done
  return 0
}

add_nodesource() {
  if command -v node >/dev/null 2>&1 && node --version | grep -q "^v${node_major}\\."; then
    return 0
  fi
  install -d -m 0755 /etc/apt/keyrings
  curl -fsSL "https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key" |
    gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg
  chmod 0644 /etc/apt/keyrings/nodesource.gpg
  printf 'deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_%s.x nodistro main\n' "$node_major" \
    >/etc/apt/sources.list.d/nodesource.list
}

add_docker_repo() {
  if docker_packages_installed; then
    return 0
  fi
  local distro_id codename arch
  # shellcheck disable=SC1091
  distro_id="$(. /etc/os-release && printf '%s' "${ID:-}")"
  # shellcheck disable=SC1091
  codename="$(. /etc/os-release && printf '%s' "${VERSION_CODENAME:-}")"
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
  install -d -m 0755 /etc/apt/keyrings
  curl -fsSL "https://download.docker.com/linux/${distro_id}/gpg" |
    gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  chmod 0644 /etc/apt/keyrings/docker.gpg
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/%s %s stable\n' "$arch" "$distro_id" "$codename" \
    >/etc/apt/sources.list.d/docker.list
}

install_chrome_or_chromium() {
  local browser_path=""
  if [[ "$(dpkg --print-architecture)" == "amd64" ]]; then
    install -d -m 0755 /etc/apt/keyrings
    curl -fsSL https://dl.google.com/linux/linux_signing_key.pub |
      gpg --dearmor -o /etc/apt/keyrings/google-linux.gpg
    chmod 0644 /etc/apt/keyrings/google-linux.gpg
    printf 'deb [arch=amd64 signed-by=/etc/apt/keyrings/google-linux.gpg] https://dl.google.com/linux/chrome/deb/ stable main\n' \
      >/etc/apt/sources.list.d/google-chrome.list
    if retry apt-get update && apt_install google-chrome-stable; then
      browser_path="$(command -v google-chrome || true)"
    else
      rm -f /etc/apt/sources.list.d/google-chrome.list
      retry apt-get update || true
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
    install -d -m 0755 /etc/opt/chrome/policies/managed /etc/chromium/policies/managed
    printf '%s\n' '{"DefaultBrowserSettingEnabled":false,"MetricsReportingEnabled":false,"PromotionalTabsEnabled":false}' \
      >/etc/opt/chrome/policies/managed/crabbox.json
    cp /etc/opt/chrome/policies/managed/crabbox.json /etc/chromium/policies/managed/crabbox.json
    cat >/usr/local/bin/crabbox-browser <<EOF
#!/bin/sh
exec "$browser_path" --no-first-run --no-default-browser-check --disable-default-apps --window-size=1500,900 --window-position=80,80 "\$@"
EOF
    chmod 0755 /usr/local/bin/crabbox-browser
    install -d -m 0755 /var/lib/crabbox
    printf 'CHROME_BIN=/usr/local/bin/crabbox-browser\nBROWSER=/usr/local/bin/crabbox-browser\n' \
      >/var/lib/crabbox/browser.env
    chmod 0644 /var/lib/crabbox/browser.env
  fi
}

install_node_pnpm() {
  apt_install nodejs
  corepack enable
  corepack prepare "pnpm@$pnpm_version" --activate
  command -v pnpm >/dev/null
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
  . /etc/os-release
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
  docker --version
  docker compose version
}

need_root "$@"
export DEBIAN_FRONTEND=noninteractive
cat >/etc/apt/apt.conf.d/80-crabbox-retries <<'APT'
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
install_docker
prepare_fast_boot
print_versions
