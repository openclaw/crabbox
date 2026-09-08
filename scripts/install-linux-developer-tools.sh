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
sudo_preserve_env="CRABBOX_LINUX_PNPM_VERSION,CRABBOX_LINUX_NODE_MAJOR,CRABBOX_LINUX_DOCKER_IMAGES,CRABBOX_LINUX_DESKTOP_TOOLS,CRABBOX_LINUX_BROWSER,CRABBOX_LINUX_APT_KEYRINGS_DIR,CRABBOX_LINUX_APT_SOURCES_DIR,CRABBOX_LINUX_APT_CONF_DIR,CRABBOX_LINUX_OS_RELEASE_FILE,CRABBOX_LINUX_CHROME_POLICY_DIR,CRABBOX_LINUX_CHROMIUM_POLICY_DIR,CRABBOX_LINUX_BROWSER_BIN_DIR,CRABBOX_LINUX_BROWSER_STATE_DIR,CRABBOX_LINUX_CHROME_DEFAULTS_FILE,CRABBOX_LINUX_TRUFFLEHOG_BIN_DIR,HTTP_PROXY,HTTPS_PROXY,NO_PROXY,http_proxy,https_proxy,no_proxy,ALL_PROXY,all_proxy"
nodesource_signing_key_fingerprint="6F71F525282841EEDAF851B42F59B5F99B1BE0B4"
docker_signing_key_fingerprint="9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
google_linux_signing_key_fingerprint="EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796"
public_toolchain_archive_dir="/opt/crabbox/toolchain-archives"
node_toolcache_root="/opt/hostedtoolcache"
node_link_dir="/usr/local/bin"
pinned_node_version="24.19.0"
go_toolcache_root="/opt/hostedtoolcache"
go_link_dir="/usr/local/bin"
pinned_go_version="1.27.0"

log() {
  printf 'linux-tools: %s\n' "$*" >&2
}

need_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    if command -v sudo >/dev/null 2>&1; then
      exec sudo -H "--preserve-env=$sudo_preserve_env" bash "$0" "$@"
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

pinned_node_supported() {
  [[ "$node_major" == "24" && "$(dpkg --print-architecture)" == "amd64" ]]
}

linux_x64_supported() {
  [[ "$(uname -s)" == "Linux" && "$(dpkg --print-architecture)" == "amd64" ]]
}

add_nodesource() {
  local source="$apt_sources_dir/nodesource.list"
  local source_tmp
  if pinned_node_supported; then
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

toolchain_archive_spec() {
  # Digests bind upstream bytes, not a mutable installation or Corepack metadata.
  case "$1" in
    go1.27.0.linux-amd64.tar.gz)
      printf '%s\n' "sha256 675c26c449cbb18fc24b74650de1eabbae6e16f64326fd85a283fb3b58280685 https://go.dev/dl/$1" ;;
    node-v24.19.0-linux-x64.tar.xz)
      printf '%s\n' "sha256 14b342e71204f811bde6153be8e04b62aef63c236fef92b55f9c83154b409647 https://nodejs.org/dist/v24.19.0/$1" ;;
    pnpm-11.22.0.tgz)
      printf '%s\n' "sha512 1ff870c4c6133dfd88fb2afc46dd13d47f09c9794b438c6fdb47ca98caf3bc16381ee0be93a091b8e3824cf01f889f46d7d9e20910fb0be1ab0fb5baa80dd621 https://registry.npmjs.org/pnpm/-/$1" ;;
    pnpm-12.3.4.tgz)
      printf '%s\n' "sha512 961aa41fb077da3a04a441d9f8e15ebc0c96da8ef710b2eb67bf9ee7cb0610eabd48f1fd85f51cffe73846785fa0f87c56a3a872a1d893f8446741b5cce45457 https://registry.npmjs.org/pnpm/-/$1" ;;
    exe.linux-x64-12.3.4.tgz)
      printf '%s\n' "sha512 d99a8e9523e47f05f5879711f853e259ff3e17eda1653ff74ef8542b9b22807ab06900888aaf11ec21b186774ab3adc9b5c2e2d9ad50a68fb05ff128c9f8f225 https://registry.npmjs.org/@pnpm/exe.linux-x64/-/$1" ;;
    *) log "no reviewed public toolchain archive: $1"; return 1 ;;
  esac
}

verify_toolchain_archive() {
  python3 - "$@" <<'PY'
import hashlib
import sys

algorithm, expected, filename = sys.argv[1:]
digest = hashlib.new(algorithm)
with open(filename, "rb") as archive:
    for block in iter(lambda: archive.read(1024 * 1024), b""):
        digest.update(block)
if digest.hexdigest() != expected:
    sys.exit("public toolchain archive checksum mismatch: " + filename)
PY
}

stage_toolchain_archive() {
  local name="$1" staging="$2" allow_download="${3:-0}"
  local algorithm expected url
  read -r algorithm expected url <<<"$(toolchain_archive_spec "$name")"
  [[ -n "$expected" ]] || return 1
  # Hash the private copy that will be extracted; never execute a cached tree.
  if [[ -f "$public_toolchain_archive_dir/$name" && ! -L "$public_toolchain_archive_dir/$name" ]] &&
    cp "$public_toolchain_archive_dir/$name" "$staging/$name" &&
    verify_toolchain_archive "$algorithm" "$expected" "$staging/$name"; then
    return 0
  fi
  rm -f "$staging/$name"
  if [[ "$allow_download" != "1" ]]; then
    log "verified public toolchain archive unavailable offline: $name"
    return 1
  fi
  curl -q --proto '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 300 \
    --output "$staging/$name" "$url" &&
    verify_toolchain_archive "$algorithm" "$expected" "$staging/$name"
}

cache_public_toolchain_archives() (
  set -euo pipefail
  umask 077
  local staging name pending
  staging="$(mktemp -d)"
  # Bind paths now: Bash can unwind function locals before an EXIT trap on failure.
  # shellcheck disable=SC2064
  trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
  [[ ! -L "$public_toolchain_archive_dir" ]] || return 1
  install -d -m 0755 "$public_toolchain_archive_dir"
  if [[ "$#" -eq 0 ]]; then
    set -- node-v24.19.0-linux-x64.tar.xz pnpm-11.22.0.tgz pnpm-12.3.4.tgz exe.linux-x64-12.3.4.tgz
  fi
  for name in "$@"; do
    stage_toolchain_archive "$name" "$staging" 1
    pending="$(mktemp "$public_toolchain_archive_dir/.archive.XXXXXX")"
    # shellcheck disable=SC2064
    trap "$(printf 'rm -rf -- %q %q' "$staging" "$pending")" EXIT
    install -m 0644 "$staging/$name" "$pending"
    python3 - "$pending" "$public_toolchain_archive_dir/$name" <<'PY'
import os
import sys
os.replace(sys.argv[1], sys.argv[2])
PY
  done
)

check_go_toolchain() (
  set -euo pipefail
  local distribution="$1" scratch="$2"
  mkdir -p "$scratch/home" "$scratch/cache" "$scratch/mod" "$scratch/path"
  export HOME="$scratch/home" GOROOT="$distribution" GOCACHE="$scratch/cache"
  export GOMODCACHE="$scratch/mod" GOPATH="$scratch/path" GOENV=off GOTOOLCHAIN=local
  export GOPROXY=off GOSUMDB=off GOWORK=off GO111MODULE=off GOFLAGS="" CGO_ENABLED=1 CC=gcc CXX=g++
  unset GOOS GOARCH GOEXPERIMENT
  [[ "$("$distribution/bin/go" version)" == "go version go$pinned_go_version linux/amd64" ]] || {
    log "unexpected Go version or architecture"
    return 1
  }
  [[ "$("$distribution/bin/go" env GOOS GOARCH)" == $'linux\namd64' ]]
  cd "$scratch"
  cat >main.go <<'GO'
package main

// static int answer(void) { return 42; }
import "C"
import "fmt"

func main() {
	if int(C.answer()) != 42 {
		panic("CGO result mismatch")
	}
	fmt.Println("go-cgo-ok")
}
GO
  "$distribution/bin/gofmt" main.go >formatted.go
  mv formatted.go main.go
  "$distribution/bin/go" test bytes crypto/sha256
  [[ "$("$distribution/bin/go" run main.go)" == "go-cgo-ok" ]]
)

install_pinned_go() (
  set -euo pipefail
  umask 022
  local staging destination
  destination="$go_toolcache_root/go/$pinned_go_version/x64"
  public_tool_links check "$go_link_dir" "$destination/bin" go gofmt || return $?
  staging="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
  install -d -m 0755 "$(dirname "$destination")"
  rm -f "$destination.complete"
  stage_toolchain_archive go1.27.0.linux-amd64.tar.gz "$staging"
  mkdir "$staging/go"
  tar --no-same-owner -xzf "$staging/go1.27.0.linux-amd64.tar.gz" -C "$staging/go" --strip-components=1
  check_go_toolchain "$staging/go" "$staging/check"
  # This image-owned slot is always rebuilt from authenticated private bytes.
  rm -rf "$destination"
  mv "$staging/go" "$destination"
  install -d -m 0755 "$go_link_dir"
  public_tool_links publish "$go_link_dir" "$destination/bin" go gofmt
  touch "$destination.complete"
)

install_go_toolchain() {
  if linux_x64_supported; then
    public_tool_links check "$go_link_dir" "$go_toolcache_root/go/$pinned_go_version/x64/bin" go gofmt || return $?
    cache_public_toolchain_archives go1.27.0.linux-amd64.tar.gz
    install_pinned_go
  fi
}

offline_go_probe() (
  set -euo pipefail
  umask 077
  [[ "$(id -u)" -ne 0 ]] || { log "offline Go smoke must run as a nonroot user"; return 1; }
  local staging
  staging="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
  stage_toolchain_archive go1.27.0.linux-amd64.tar.gz "$staging"
  mkdir "$staging/go"
  tar --no-same-owner -xzf "$staging/go1.27.0.linux-amd64.tar.gz" -C "$staging/go" --strip-components=1
  check_go_toolchain "$staging/go" "$staging/check"
)

go_smoke_script() {
  printf 'public_toolchain_archive_dir=%q\n' "$public_toolchain_archive_dir"
  printf 'pinned_go_version=%q\n' "$pinned_go_version"
  declare -f log linux_x64_supported toolchain_archive_spec verify_toolchain_archive stage_toolchain_archive check_go_toolchain offline_go_probe
  # shellcheck disable=SC2016
  printf '%s\n' 'if linux_x64_supported; then' \
    '  [[ "$(GOTOOLCHAIN=local GOENV=off go version)" == "go version go1.27.0 linux/amd64" ]]' \
    '  command -v gofmt' '  offline_go_probe' 'fi'
}

seed_offline_pnpm() {
  local version="$1" staging="$2" corepack_home="$3"
  local destination="$corepack_home/v1/pnpm/$version" algorithm digest url
  read -r algorithm digest url <<<"$(toolchain_archive_spec "pnpm-$version.tgz")"
  [[ -n "$digest" ]] || return 1
  [[ ! -e "$destination" && ! -L "$destination" ]] || return 1
  stage_toolchain_archive "pnpm-$version.tgz" "$staging" || return 1
  mkdir -p "$destination" || return 1
  tar --no-same-owner -xzf "$staging/pnpm-$version.tgz" -C "$destination" --strip-components=1 || return 1
  if [[ "$version" == "12.3.4" ]]; then
    stage_toolchain_archive exe.linux-x64-12.3.4.tgz "$staging" || return 1
    mkdir "$staging/native" || return 1
    tar --no-same-owner -xzf "$staging/exe.linux-x64-12.3.4.tgz" -C "$staging/native" || return 1
    install -m 0755 "$staging/native/package/pnpm" "$destination/pnpm-native" || return 1
  fi
  # Corepack 0.35.0 does not authenticate cache hits or `install -g <pack.tgz>`.
  # Create its metadata only in a fresh tree extracted from independently pinned bytes.
  python3 - "$destination/.corepack" "$version" "$digest" <<'PY'
import json
import sys
filename, version, digest = sys.argv[1:]
with open(filename, "x") as metadata:
    json.dump({
        "locator": {"name": "pnpm", "reference": version + "+sha512." + digest},
        "bin": {"pnpm": "./bin/pnpm.mjs", "pnpx": "./bin/pnpx.mjs"},
        "hash": "sha512." + digest,
    }, metadata)
PY
}

public_tool_links() {
  python3 - "$@" <<'PY'
import os
import sys
import tempfile

action, link_dir, bin_dir, *tools = sys.argv[1:]
if action not in ("check", "publish") or not os.path.isabs(bin_dir):
    sys.exit("invalid public tool link operation")

def check(tool):
    link = os.path.join(link_dir, tool)
    target = os.path.join(bin_dir, tool)
    if os.path.lexists(link) and (not os.path.islink(link) or os.readlink(link) != target):
        sys.exit("linux-tools: public tool conflict at " + link + "; resolve before rebake")

for tool in tools:
    check(tool)
if action == "publish":
    # Each rename replaces one entry, never a directory's contents; not a group transaction.
    with tempfile.TemporaryDirectory(prefix=".crabbox-tool-links-", dir=link_dir) as staging:
        for tool in tools:
            check(tool)
            pending = os.path.join(staging, tool)
            os.symlink(os.path.join(bin_dir, tool), pending)
            os.replace(pending, os.path.join(link_dir, tool))
PY
}

install_pinned_node() (
  set -euo pipefail
  umask 022
  local staging destination node_path
  destination="$node_toolcache_root/node/$pinned_node_version/x64"
  public_tool_links check "$node_link_dir" "$destination/bin" node npm npx corepack pnpm pnpx || return $?
  staging="$(mktemp -d)" || return $?
  # shellcheck disable=SC2064
  trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
  stage_toolchain_archive node-v24.19.0-linux-x64.tar.xz "$staging" || return $?
  install -d -m 0755 "$(dirname "$destination")" || return $?
  rm -f "$destination.complete" || return $?
  mkdir "$staging/node" || return $?
  tar --no-same-owner -xJf "$staging/node-v24.19.0-linux-x64.tar.xz" -C "$staging/node" --strip-components=1 || return $?
  node_path="$staging/node/bin:$PATH"
  [[ "$("$staging/node/bin/node" --version)" == "v$pinned_node_version" ]] || return 1
  env PATH="$node_path" "$staging/node/bin/npm" --version || return $?
  env PATH="$node_path" "$staging/node/bin/corepack" --version || return $?
  env PATH="$node_path" "$staging/node/bin/corepack" enable --install-directory "$staging/node/bin" || return $?
  # The image recipe owns this exact version slot. Markers never justify reusing its bytes.
  rm -rf "$destination" || return $?
  mv "$staging/node" "$destination" || return $?
  install -d -m 0755 "$node_link_dir" || return $?
  public_tool_links publish "$node_link_dir" "$destination/bin" node npm npx corepack pnpm pnpx || return $?
  touch "$destination.complete"
)

nodesource_node_version() {
  local arch="$1" versions package version origin extra url suite source_arch kind selected=""
  [[ "$node_major" =~ ^[0-9]+$ ]] || return 1
  versions="$(LC_ALL=C apt-cache madison "nodejs:$arch")" || return $?
  while IFS='|' read -r package version origin extra; do
    read -r package <<<"$package"
    read -r version <<<"$version"
    read -r url suite source_arch kind extra <<<"$origin"
    [[ "$package" == "nodejs" || "$package" == "nodejs:$arch" ]] || continue
    [[ "$url" == "https://deb.nodesource.com/node_$node_major.x" &&
      "$suite" == "nodistro/main" && "$source_arch" == "$arch" &&
      "$kind" == "Packages" && -z "$extra" ]] || continue
    [[ "${version#*:}" == "$node_major."* ]] || continue
    if [[ -z "$selected" ]] || dpkg --compare-versions "$version" gt "$selected"; then
      selected="$version"
    fi
  done <<<"$versions"
  [[ -n "$selected" ]] || return 1
  printf '%s\n' "$selected"
}

install_requested_node() {
  local arch version installed status installed_version installed_arch tool link_target
  local package_files file node_binary="" actual_version expected_version
  local -a retired_tools=() install_args=(install -y --no-install-recommends)
  arch="$(dpkg --print-architecture)" || return $?
  version="$(nodesource_node_version "$arch")" || {
    log "no NodeSource Node $node_major package for native architecture $arch; owned links retained"
    return 1
  }
  installed="$(dpkg-query -W -f='${db:Status-Status}\t${Version}\t${Architecture}\n' "nodejs:$arch" 2>/dev/null || true)"
  read -r status installed_version installed_arch <<<"$installed"
  if [[ "$status" == "installed" ]] && dpkg --compare-versions "$version" lt "$installed_version"; then
    if [[ "$node_major" != "22" || "${installed_version#*:}" != "24."* || "$installed_arch" != "$arch" ]]; then
      log "refusing Node package downgrade $installed_version to $version; only the explicit Node 24 to 22 transition is supported"
      return 1
    fi
    install_args+=(--allow-downgrades)
  fi
  # Snapshot this installer's exact retirement set before APT can change any links.
  for tool in node npm npx corepack pnpm pnpx; do
    if [[ -L "$node_link_dir/$tool" ]]; then
      link_target="$(readlink -n "$node_link_dir/$tool" && printf '.')" || return 1
      [[ "$link_target" != "$node_toolcache_root/node/24.19.0/x64/bin/$tool." ]] || retired_tools+=("$tool")
    fi
  done
  if retry apt-get "${install_args[@]}" "nodejs:$arch=$version"; then
    :
  else
    status=$?
    log "NodeSource package replacement failed for nodejs:$arch=$version; owned links retained"
    return "$status"
  fi
  installed="$(dpkg-query -W -f='${db:Status-Status}\t${Version}\t${Architecture}\n' "nodejs:$arch")" || {
    log "Node package replacement verification failed; owned links retained"
    return 1
  }
  read -r status installed_version installed_arch <<<"$installed"
  if [[ "$status" != "installed" || "$installed_version" != "$version" || "$installed_arch" != "$arch" ]]; then
    log "Node package replacement verification failed for nodejs:$arch=$version; owned links retained"
    return 1
  fi
  package_files="$(dpkg-query -L "nodejs:$arch")" || return $?
  while IFS= read -r file; do
    if [[ "$file" == /*/bin/node ]]; then
      [[ -z "$node_binary" ]] || { log "ambiguous package-owned Node binary; owned links retained"; return 1; }
      node_binary="$file"
    fi
  done <<<"$package_files"
  expected_version="${version#*:}"
  expected_version="v${expected_version%%-*}"
  if [[ ! -f "$node_binary" || -L "$node_binary" || ! -x "$node_binary" ]] ||
    ! actual_version="$("$node_binary" --version)" || [[ "$actual_version" != "$expected_version" ]]; then
    log "package-owned Node binary verification failed for nodejs:$arch=$version; owned links retained"
    return 1
  fi
  for tool in ${retired_tools[@]+"${retired_tools[@]}"}; do
    if [[ -L "$node_link_dir/$tool" ]]; then
      # Preserve operator replacements, including targets with trailing newlines.
      link_target="$(readlink -n "$node_link_dir/$tool" && printf '.')" || return 1
      if [[ "$link_target" == "$node_toolcache_root/node/24.19.0/x64/bin/$tool." ]]; then
        rm -- "$node_link_dir/$tool" || return 1
      fi
    fi
  done
  hash -r
}

install_node_pnpm() {
  local use_pinned_node=0
  if pinned_node_supported; then
    use_pinned_node=1
    public_tool_links check "$node_link_dir" "$node_toolcache_root/node/$pinned_node_version/x64/bin" node npm npx corepack pnpm pnpx || return $?
    cache_public_toolchain_archives
    install_pinned_node || return $?
    export PATH="$node_link_dir:$PATH"
  else
    if [[ "$node_major" != "24" ]]; then
      local actual_node_version
      install_requested_node || return $?
      actual_node_version="$(node --version)" || {
        log "requested Node major $node_major is unavailable; resolve PATH before preparing Corepack"
        return 1
      }
      if [[ "$actual_node_version" != "v$node_major."* ]]; then
        log "requested Node major $node_major, but node reports $actual_node_version; resolve PATH shadowing before preparing Corepack"
        return 1
      fi
    else
      # The non-x86_64 default route does not opt into any downgrade.
      apt_install nodejs || return $?
    fi
  fi
  command -v npm >/dev/null
  command -v corepack >/dev/null
  if [[ "$use_pinned_node" == "0" ]]; then
    corepack enable
  fi
  corepack prepare "pnpm@$pnpm_version" --activate
  command -v pnpm >/dev/null
}

offline_node_pnpm_probe() (
  set -euo pipefail
  umask 077
  [[ "$(id -u)" -ne 0 ]] || { log "offline developer-tool smoke must run as a nonroot user"; return 1; }
  local staging version corepack_home package_manager algorithm digest url
  staging="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
  stage_toolchain_archive node-v24.19.0-linux-x64.tar.xz "$staging"
  mkdir "$staging/node" "$staging/home"
  tar --no-same-owner -xJf "$staging/node-v24.19.0-linux-x64.tar.xz" -C "$staging/node" --strip-components=1
  [[ "$("$staging/node/bin/node" --version)" == "v24.19.0" ]]
  for version in 11.22.0 12.3.4; do
    corepack_home="$staging/corepack-$version"
    seed_offline_pnpm "$version" "$staging" "$corepack_home"
    read -r algorithm digest url <<<"$(toolchain_archive_spec "pnpm-$version.tgz")"
    package_manager="pnpm@$version+sha512.$digest"
    mkdir -p "$staging/project-$version/dependency"
    printf '{"name":"offline-smoke","version":"1.0.0","dependencies":{"smoke-dependency":"file:./dependency"}}\n' >"$staging/project-$version/package.json"
    printf '{"name":"smoke-dependency","version":"1.0.0","main":"index.js"}\n' >"$staging/project-$version/dependency/package.json"
    printf 'module.exports = 42;\n' >"$staging/project-$version/dependency/index.js"
    (
      cd "$staging/project-$version"
      local -a offline_env=(env -i "HOME=$staging/home" "PATH=$staging/node/bin:/usr/bin:/bin"
        "COREPACK_HOME=$corepack_home" COREPACK_ENABLE_NETWORK=0 COREPACK_DEFAULT_TO_LATEST=0
        COREPACK_ENABLE_PROJECT_SPEC=0 COREPACK_ENV_FILE=0 CI=1)
      [[ "$("${offline_env[@]}" corepack "$package_manager" --version)" == "$version" ]]
      "${offline_env[@]}" corepack "$package_manager" install --offline --ignore-scripts --no-frozen-lockfile
      "${offline_env[@]}" node -e 'if (require("smoke-dependency") !== 42) process.exit(1)'
    )
  done
)

node_pnpm_smoke_script() {
  printf 'public_toolchain_archive_dir=%q\n' "$public_toolchain_archive_dir"
  printf 'node_major=%q\n' "$node_major"
  declare -f log pinned_node_supported toolchain_archive_spec verify_toolchain_archive stage_toolchain_archive seed_offline_pnpm offline_node_pnpm_probe
  # Evaluate the builder's predicate in the guest, never against the mint host.
  printf '%s\n' 'if pinned_node_supported; then' '  offline_node_pnpm_probe' 'fi'
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
    "$binary" --no-update --version 2>/dev/null |
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

readiness_producer_path() {
  local producer="/usr/local/libexec/crabbox/linux-readiness.generated.sh"
  if [[ ! -x "$producer" ]]; then
    producer="$(dirname "${BASH_SOURCE[0]}")/linux-readiness.generated.sh"
  fi
  if [[ ! -x "$producer" ]]; then
    log "standalone Linux readiness producer is missing"
    return 1
  fi
  printf '%s\n' "$producer"
}

clean_cloud_init_state() {
  if ! command -v cloud-init >/dev/null 2>&1; then
    return 0
  fi
  # Match native checkpoint preparation: preserve real completion facts only in
  # tmpfs, so cleaning the image does not invalidate this boot or certify a clone.
  /usr/bin/python3 -I - <<'PY'
import json, os, pathlib, shutil, subprocess, sys, tempfile
from cloudinit.cmd.devel import read_cfg_paths

cloud_init = [sys.executable, "-I", "-m", "cloudinit.cmd.main"]
def require_done():
    result = subprocess.run(cloud_init + ["status", "--format=json"], check=True, capture_output=True, text=True, timeout=30)
    if json.loads(result.stdout)["status"] != "done":
        raise RuntimeError("developer image preparation requires completed cloud-init initialization")

require_done()
paths = read_cfg_paths()
runtime = pathlib.Path(paths.run_dir).absolute()
cache = pathlib.Path(paths.cloud_dir).resolve()
for ancestor in (runtime, *runtime.parents):
    resolved = ancestor.resolve()
    if resolved == cache or cache in resolved.parents:
        raise RuntimeError("cloud-init runtime directory must be outside its cleaned disk cache")
runtime = runtime.resolve()
filesystem = subprocess.check_output(["stat", "-f", "-c", "%T", str(runtime)], text=True).strip()
if filesystem != "tmpfs":
    raise RuntimeError("cloud-init runtime directory must use tmpfs to exclude completion state from the image")
files = [runtime / "status.json", runtime / "result.json"]
for path in files:
    if not path.is_file():
        raise RuntimeError("cloud-init completion file is missing: " + str(path))
for path in files:
    fd, temporary = tempfile.mkstemp(prefix="." + path.name + "-", dir=runtime)
    os.close(fd)
    try:
        original = path.stat()
        shutil.copy2(path, temporary)
        os.chown(temporary, original.st_uid, original.st_gid)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
subprocess.run(cloud_init + ["clean", "--logs", "--seed"], check=True)
require_done()
PY
}

prepare_fast_boot() {
  local readiness_producer
  readiness_producer="$(readiness_producer_path)" || return 1
  install -d -m 1777 /var/cache/crabbox /var/cache/crabbox/pnpm /var/cache/crabbox/npm /var/cache/crabbox/corepack /var/cache/crabbox/docker
  "$readiness_producer"
  systemctl disable --now apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true
  systemctl mask apt-daily.service apt-daily-upgrade.service 2>/dev/null || true
  clean_cloud_init_state || return $?
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
  if linux_x64_supported; then
    "$go_link_dir/go" version
  fi
  "$trufflehog_bin_dir/trufflehog" --no-update --version
  docker --version
  docker compose version
}

main() {
  need_root "$@"
  local readiness_producer readiness_package_output package
  local -a readiness_packages apt_get_base
  readiness_producer="$(readiness_producer_path)"
  readiness_package_output="$("$readiness_producer" --print-packages linux-builder)"
  if [[ -z "$readiness_package_output" || "$readiness_package_output" == *$'\n'* ]]; then
    log "invalid Linux readiness package contract"
    return 1
  fi
  read -r -a readiness_packages <<<"$readiness_package_output"
  for package in "${readiness_packages[@]}"; do
    if [[ ! "$package" =~ ^[a-z0-9][a-z0-9+.-]*$ ]]; then
      log "invalid Linux readiness package name"
      return 1
    fi
  done
  export DEBIAN_FRONTEND=noninteractive
  install -d -m 0755 "$apt_conf_dir" "$apt_sources_dir"
  cat >"$apt_conf_dir/80-crabbox-retries" <<'APT'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
APT

  apt_get_base=(apt-transport-https gnupg lsb-release software-properties-common)
  retry apt-get update
  apt_install "${apt_get_base[@]}" "${readiness_packages[@]}"
  add_nodesource
  add_docker_repo
  retry apt-get update
  apt_install \
    gh \
    yq \
    ripgrep \
    fd-find \
    fzf \
    coreutils \
    tar \
    sed \
    findutils \
    unzip \
    zip \
    shellcheck \
    shfmt \
    python3-pip \
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
  install_go_toolchain
  install_trufflehog
  install_docker
  prepare_fast_boot
  print_versions
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
