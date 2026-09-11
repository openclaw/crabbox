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
pinned_go_version="1.27.1"
bun_bin_dir="/usr/local/bin"
bun_toolchain_root="/opt/crabbox/toolchains/bun"
pinned_bun_version="1.4.0"
pinned_rust_version="1.97.1"
rust_seed_root="/opt/crabbox/rust/1.97.1"
rust_user_record="/opt/crabbox/rust/runtime-user.json"
pinned_uv_version="0.12.11"
uv_toolchain_root="/opt/crabbox/toolchains/uv"
uv_bin_dir="/usr/local/bin"

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
    go1.27.1.linux-amd64.tar.gz)
      printf '%s\n' "sha256 63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445 https://go.dev/dl/$1" ;;
    node-v24.19.0-linux-x64.tar.xz)
      printf '%s\n' "sha256 14b342e71204f811bde6153be8e04b62aef63c236fef92b55f9c83154b409647 https://nodejs.org/dist/v24.19.0/$1" ;;
    pnpm-11.22.0.tgz)
      printf '%s\n' "sha512 1ff870c4c6133dfd88fb2afc46dd13d47f09c9794b438c6fdb47ca98caf3bc16381ee0be93a091b8e3824cf01f889f46d7d9e20910fb0be1ab0fb5baa80dd621 https://registry.npmjs.org/pnpm/-/$1" ;;
    pnpm-12.3.4.tgz)
      printf '%s\n' "sha512 961aa41fb077da3a04a441d9f8e15ebc0c96da8ef710b2eb67bf9ee7cb0610eabd48f1fd85f51cffe73846785fa0f87c56a3a872a1d893f8446741b5cce45457 https://registry.npmjs.org/pnpm/-/$1" ;;
    exe.linux-x64-12.3.4.tgz)
      printf '%s\n' "sha512 d99a8e9523e47f05f5879711f853e259ff3e17eda1653ff74ef8542b9b22807ab06900888aaf11ec21b186774ab3adc9b5c2e2d9ad50a68fb05ff128c9f8f225 https://registry.npmjs.org/@pnpm/exe.linux-x64/-/$1" ;;
    bun-v1.4.0-linux-x64.zip)
      printf '%s\n' "sha256 2d03fb5fb83ac8b567aca0a281b2ce1a1a19d488f56c2968d88c3f25e92fe452 https://github.com/oven-sh/bun/releases/download/bun-v1.4.0/bun-linux-x64.zip" ;;
    bun-v1.4.0-linux-x64-baseline.zip)
      printf '%s\n' "sha256 184fb4595f0d401a217cf7c78c1bc430ba83314dab7a8b94805babbf7fa7097f https://github.com/oven-sh/bun/releases/download/bun-v1.4.0/bun-linux-x64-baseline.zip" ;;
    bun-v1.4.0-linux-aarch64.zip)
      printf '%s\n' "sha256 4b1a332ee861983eb93bcfe6f770fff94e3e31b2c388bdaea3c8ed35e58eed0e https://github.com/oven-sh/bun/releases/download/bun-v1.4.0/bun-linux-aarch64.zip" ;;
    uv-0.12.11-x86_64-unknown-linux-gnu.tar.gz)
      printf '%s\n' "sha256 4ae93e0f148a18434cc094072547cec88912fc4a72b984183c7d0d0e9586cb5e https://github.com/astral-sh/uv/releases/download/0.12.11/uv-x86_64-unknown-linux-gnu.tar.gz" ;;
    channel-rust-1.97.1.toml)
      printf '%s\n' "sha256 03569b1886ceb5c05276b50c8431ab111de944cd6140fe1fa7d821dd8e0f29cf https://static.rust-lang.org/dist/$1" ;;
    rustup-init-1.29.0-x86_64-unknown-linux-gnu)
      printf '%s\n' "sha256 4acc9acc76d5079515b46346a485974457b5a79893cfb01112423c89aeb5aa10 https://static.rust-lang.org/rustup/archive/1.29.0/x86_64-unknown-linux-gnu/rustup-init" ;;
    rustc-1.97.1-x86_64-unknown-linux-gnu.tar.xz)
      printf '%s\n' "sha256 9819d0a32d56bd339585319c80260e332779f5541fd66838ab7e016d6c814819 https://static.rust-lang.org/dist/2026-07-16/$1" ;;
    cargo-1.97.1-x86_64-unknown-linux-gnu.tar.xz)
      printf '%s\n' "sha256 e1be5f5ff7f7f80ca506fb65770b759edbdc6d303781ed71c5de8ec8a8394779 https://static.rust-lang.org/dist/2026-07-16/$1" ;;
    rust-std-1.97.1-x86_64-unknown-linux-gnu.tar.xz)
      printf '%s\n' "sha256 1c1e704ae80126b7de34f72ea2825f7fd01736dec20732faed47374b95282fba https://static.rust-lang.org/dist/2026-07-16/$1" ;;
    rustfmt-1.97.1-x86_64-unknown-linux-gnu.tar.xz)
      printf '%s\n' "sha256 907fe97d6afbde1eca1b34c992c76e1406d422e2e6f137813d382acec7eb4d14 https://static.rust-lang.org/dist/2026-07-16/$1" ;;
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
  local algorithm expected url status
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
    --output "$staging/$name" "$url" || {
      status=$?
      log "toolchain archive download failed: $name (curl exit $status)"
      return "$status"
    }
  verify_toolchain_archive "$algorithm" "$expected" "$staging/$name"
}

prepare_public_toolchain_archive_dir() {
  local parent directory
  parent="$(dirname "$public_toolchain_archive_dir")"
  # Validate both owned public boundaries before changing either; never widen
  # unrelated ancestors or follow an operator-owned replacement.
  for directory in "$parent" "$public_toolchain_archive_dir"; do
    if [[ -L "$directory" || ( -e "$directory" && ( ! -d "$directory" || ! -O "$directory" ) ) ]]; then
      log "invalid public toolchain archive directory: $directory"
      return 1
    fi
  done
  install -d -m 0755 "$parent" "$public_toolchain_archive_dir"
}

cache_public_toolchain_archives() (
  set -euo pipefail
  umask 077
  local staging name pending
  # Conditional callers disable errexit; never publish after a failed step.
  prepare_public_toolchain_archive_dir || return $?
  staging="$(mktemp -d)" || return $?
  # Bind paths now: Bash can unwind function locals before an EXIT trap on failure.
  # shellcheck disable=SC2064
  trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
  if [[ "$#" -eq 0 ]]; then
    set -- node-v24.19.0-linux-x64.tar.xz pnpm-11.22.0.tgz pnpm-12.3.4.tgz exe.linux-x64-12.3.4.tgz
  fi
  for name in "$@"; do
    stage_toolchain_archive "$name" "$staging" 1 || return $?
    pending="$(mktemp "$public_toolchain_archive_dir/.archive.XXXXXX")" || return $?
    # shellcheck disable=SC2064
    trap "$(printf 'rm -rf -- %q %q' "$staging" "$pending")" EXIT
    install -m 0644 "$staging/$name" "$pending" || return $?
    python3 - "$pending" "$public_toolchain_archive_dir/$name" <<'PY' || return $?
import os
import sys
os.replace(sys.argv[1], sys.argv[2])
PY
  done
)

check_go_toolchain() (
  set -euo pipefail
  local distribution="$1" scratch="$2" version target result
  mkdir -p "$scratch/home" "$scratch/cache" "$scratch/mod" "$scratch/path" || return $?
  export HOME="$scratch/home" GOROOT="$distribution" GOCACHE="$scratch/cache"
  export GOMODCACHE="$scratch/mod" GOPATH="$scratch/path" GOENV=off GOTOOLCHAIN=local
  export GOPROXY=off GOSUMDB=off GOWORK=off GO111MODULE=off GOFLAGS="" CGO_ENABLED=1 CC=gcc CXX=g++
  unset GOOS GOARCH GOEXPERIMENT
  version="$("$distribution/bin/go" version)" || return $?
  [[ "$version" == "go version go$pinned_go_version linux/amd64" ]] || {
    log "unexpected Go version or architecture"
    return 1
  }
  target="$("$distribution/bin/go" env GOOS GOARCH)" || return $?
  [[ "$target" == $'linux\namd64' ]] || return 1
  cd "$scratch" || return $?
  # Group the heredoc so Bash 3.2 and 5.2 both serialize its failure check safely.
  {
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
  } || return $?
  "$distribution/bin/gofmt" main.go >formatted.go || return $?
  mv formatted.go main.go || return $?
  "$distribution/bin/go" test bytes crypto/sha256 || return $?
  result="$("$distribution/bin/go" run main.go)" || return $?
  [[ "$result" == "go-cgo-ok" ]]
)

go_public_tool_links() {
  # Only the previously shipped managed slot may migrate; foreign aliases
  # still fail closed. Mixed old/new links are safe to resume after interruption.
  public_tool_links "$1" "$go_link_dir" "$go_toolcache_root/go/$pinned_go_version/x64/bin" \
    --replace-from "$go_toolcache_root/go/1.27.0/x64/bin" go gofmt
}

install_pinned_go() (
  set -euo pipefail
  umask 022
  local staging destination
  destination="$go_toolcache_root/go/$pinned_go_version/x64"
  go_public_tool_links check || return $?
  staging="$(mktemp -d)" || return $?
  # shellcheck disable=SC2064
  trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
  stage_toolchain_archive "go$pinned_go_version.linux-amd64.tar.gz" "$staging" || return $?
  mkdir "$staging/go" || return $?
  tar --no-same-owner -xzf "$staging/go$pinned_go_version.linux-amd64.tar.gz" -C "$staging/go" --strip-components=1 || return $?
  rm -f "$destination.complete" || return $?
  check_go_toolchain "$staging/go" "$staging/check" || return $?
  go_public_tool_links check || return $?
  install -d -m 0755 "$(dirname "$destination")" "$go_link_dir" || return $?
  # This image-owned slot is always rebuilt from authenticated private bytes.
  rm -rf "$destination" || return $?
  mv "$staging/go" "$destination" || return $?
  go_public_tool_links publish || return $?
  touch "$destination.complete"
)

install_go_toolchain() {
  if linux_x64_supported; then
    go_public_tool_links check || return $?
    cache_public_toolchain_archives "go$pinned_go_version.linux-amd64.tar.gz" || return $?
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
  stage_toolchain_archive "go$pinned_go_version.linux-amd64.tar.gz" "$staging"
  mkdir "$staging/go"
  tar --no-same-owner -xzf "$staging/go$pinned_go_version.linux-amd64.tar.gz" -C "$staging/go" --strip-components=1
  check_go_toolchain "$staging/go" "$staging/check"
)

go_smoke_script() {
  printf 'public_toolchain_archive_dir=%q\n' "$public_toolchain_archive_dir"
  printf 'pinned_go_version=%q\n' "$pinned_go_version"
  declare -f log linux_x64_supported toolchain_archive_spec verify_toolchain_archive stage_toolchain_archive check_go_toolchain offline_go_probe
  # shellcheck disable=SC2016
  printf '%s\n' 'if linux_x64_supported; then' \
    '  [[ "$(GOTOOLCHAIN=local GOENV=off go version)" == "go version go$pinned_go_version linux/amd64" ]]' \
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
previous_bin = None
if tools[:1] == ["--replace-from"]:
    _, previous_bin, *tools = tools
    if not os.path.isabs(previous_bin):
        sys.exit("invalid previous public tool directory")

def check(tool):
    link = os.path.join(link_dir, tool)
    targets = [os.path.join(bin_dir, tool)]
    if previous_bin is not None:
        targets.append(os.path.join(previous_bin, tool))
    if os.path.lexists(link) and (not os.path.islink(link) or os.readlink(link) not in targets):
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

install_node_runtime() {
  local use_pinned_node=0
  if pinned_node_supported; then
    use_pinned_node=1
    public_tool_links check "$node_link_dir" "$node_toolcache_root/node/$pinned_node_version/x64/bin" node npm npx corepack pnpm pnpx || return $?
    cache_public_toolchain_archives "node-v$pinned_node_version-linux-x64.tar.xz" || return $?
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
  command -v npm >/dev/null || return $?
  command -v corepack >/dev/null || return $?
  if [[ "$use_pinned_node" == "0" ]]; then
    corepack enable
  fi
}

install_node_pnpm() {
  install_node_runtime || return $?
  if pinned_node_supported; then
    cache_public_toolchain_archives || return $?
  fi
  corepack prepare "pnpm@$pnpm_version" --activate || return $?
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

pinned_bun_supported() {
  [[ "$(uname -s)" == "Linux" && "$(dpkg --print-architecture)" == "amd64" &&
    "$(getconf GNU_LIBC_VERSION 2>/dev/null)" == glibc\ * ]]
}

bun_optimized_supported() {
  # Every visible guest CPU must support both flags; missing evidence means baseline.
  awk '
    function finish_cpu() {
      if (cpu) {
        seen = 1
        if (!flags || !avx || !avx2) missing = 1
      }
      cpu = flags = avx = avx2 = 0
    }
    /^processor[[:space:]]*:/ {
      finish_cpu()
      cpu = 1
    }
    /^[[:space:]]*$/ { finish_cpu() }
    /^flags[[:space:]]*:/ {
      if (!cpu || flags) missing = 1
      flags = 1
      avx = avx2 = 0
      for (i = 3; i <= NF; i++) {
        if ($i == "avx") avx = 1
        if ($i == "avx2") avx2 = 1
      }
      if (!avx || !avx2) missing = 1
    }
    END { finish_cpu(); exit !seen || missing }
  ' /proc/cpuinfo 2>/dev/null
}

stage_bun_archive() {
  local name="$1" staging="$2" allow_download="${3:-0}"
  case "$name" in
    bun-v1.4.0-linux-x64.zip|bun-v1.4.0-linux-x64-baseline.zip|bun-v1.4.0-linux-aarch64.zip) ;;
    *) log "unsupported pinned Bun archive: $name"; return 1 ;;
  esac
  if [[ -L "$public_toolchain_archive_dir" ||
    ( -e "$public_toolchain_archive_dir" && ! -d "$public_toolchain_archive_dir" ) ]]; then
    log "malformed Bun archive cache"
    return 1
  fi
  if [[ -e "$public_toolchain_archive_dir/$name" || -L "$public_toolchain_archive_dir/$name" ]]; then
    # A present cache is evidence to authenticate, never permission to redownload.
    if [[ ! -f "$public_toolchain_archive_dir/$name" || -L "$public_toolchain_archive_dir/$name" ]]; then
      log "malformed Bun archive: $name"
      return 1
    fi
    allow_download=0
  fi
  stage_toolchain_archive "$name" "$staging" "$allow_download"
}

extract_bun_archive() {
  local variant="$1" staging="$2"
  stage_bun_archive "bun-v$pinned_bun_version-$variant.zip" "$staging" || return 1
  python3 - "$staging/bun-v$pinned_bun_version-$variant.zip" "$variant" "$staging/$variant" <<'PY'
import os
import shutil
import stat
import sys
import zipfile

filename, variant, destination = sys.argv[1:]
member = "bun-" + variant + "/bun"
with zipfile.ZipFile(filename) as archive:
    entries = archive.infolist()
    files = [entry for entry in entries if not entry.is_dir()]
    if len(files) != 1 or files[0].filename != member or stat.S_ISLNK(files[0].external_attr >> 16):
        sys.exit("malformed Bun ZIP: expected one regular " + member)
    os.mkdir(destination, 0o700)
    with archive.open(files[0]) as source, open(destination + "/bun", "xb") as target:
        shutil.copyfileobj(source, target)
    os.chmod(destination + "/bun", 0o755)
PY
}

install_bun() {
  pinned_bun_supported || return 0
  local destination="$bun_toolchain_root/$pinned_bun_version/linux-x64-baseline" directory
  public_tool_links check "$bun_bin_dir" "$destination" bun bunx || return $?
  # Check only this managed path's ancestors before any write can follow a symlink.
  directory="$destination"
  while [[ "$directory" != "/" ]]; do
    [[ ! -L "$directory" ]] || { log "symlinked Bun managed directory: $directory"; return 1; }
    directory="$(dirname "$directory")" || return $?
  done
  prepare_public_toolchain_archive_dir || return $?
  (
    set -euo pipefail
    umask 077
    local staging name pending variant
    staging="$(mktemp -d)" || return $?
    # shellcheck disable=SC2064
    trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
    for variant in linux-x64-baseline linux-x64; do
      name="bun-v$pinned_bun_version-$variant.zip"
      stage_bun_archive "$name" "$staging" 1 || return $?
      pending="$(mktemp "$public_toolchain_archive_dir/.bun-archive.XXXXXX")" || return $?
      # shellcheck disable=SC2064
      trap "$(printf 'rm -rf -- %q %q' "$staging" "$pending")" EXIT
      install -m 0644 "$staging/$name" "$pending" || return $?
      python3 - "$pending" "$public_toolchain_archive_dir/$name" <<'PY' || return $?
import os
import sys
os.replace(sys.argv[1], sys.argv[2])
PY
    done
    extract_bun_archive linux-x64-baseline "$staging" || return $?
    [[ "$("$staging/linux-x64-baseline/bun" --version)" == "$pinned_bun_version" ]] ||
      { log "installed Bun version does not match $pinned_bun_version"; exit 1; }
    # The installed image can move to a less capable guest. Keep its default baseline.
    chmod 0755 "$staging/linux-x64-baseline" || return $?
    ln -s bun "$staging/linux-x64-baseline/bunx" || return $?
    public_tool_links check "$bun_bin_dir" "$destination" bun bunx || return $?
    (umask 022; install -d -m 0755 "$(dirname "$destination")" "$bun_bin_dir") || return $?
    # The exact image-owned slot may be incomplete; never reuse its executable bytes.
    rm -rf -- "$destination" || return $?
    mv "$staging/linux-x64-baseline" "$destination" || return $?
    public_tool_links publish "$bun_bin_dir" "$destination" bun bunx || return $?
  ) || return $?
  export PATH="$bun_bin_dir:$PATH"
  hash -r
}

offline_bun_probe() (
  set -euo pipefail
  umask 077
  [[ "$(id -u)" -ne 0 ]] || { log "offline Bun smoke must run as a nonroot user"; return 1; }
  local staging variant bun_path
  staging="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
  mkdir -p "$staging/home" "$staging/project/node_modules/.bin"
  printf 'const answer: number = 42; console.log(answer);\n' >"$staging/project/main.ts"
  printf 'import { test, expect } from "bun:test"; test("offline", () => expect(6 * 7).toBe(42));\n' >"$staging/project/main.test.ts"
  printf '#!/usr/bin/env bun\nconsole.log(42);\n' >"$staging/project/node_modules/.bin/offline-smoke"
  chmod 0755 "$staging/project/node_modules/.bin/offline-smoke"
  # No dependencies, auto-install, or ambient home/config/cache in this proof.
  local -a offline_env=(env -i "HOME=$staging/home" "TMPDIR=$staging" "PATH=$PATH"
    BUN_RUNTIME_TRANSPILER_CACHE_PATH=0 DO_NOT_TRACK=1 CI=1)
  cd "$staging/project"
  [[ "$("${offline_env[@]}" bun --version)" == "$pinned_bun_version" ]] ||
    { log "normal PATH Bun version does not match $pinned_bun_version"; return 1; }
  [[ "$("${offline_env[@]}" bunx --no-install --bun offline-smoke)" == "42" ]] ||
    { log "normal PATH bunx offline execution failed"; return 1; }
  [[ "$("${offline_env[@]}" bun --no-install main.ts)" == "42" ]] ||
    { log "normal PATH Bun TypeScript execution failed"; return 1; }
  for variant in linux-x64-baseline linux-x64; do
    extract_bun_archive "$variant" "$staging"
    if [[ "$variant" == "linux-x64" ]] && ! bun_optimized_supported; then
      continue
    fi
    bun_path="$staging/$variant/bun"
    [[ "$("${offline_env[@]}" "$bun_path" --version)" == "$pinned_bun_version" ]] ||
      { log "$variant Bun version does not match $pinned_bun_version"; return 1; }
    [[ "$("${offline_env[@]}" "$bun_path" --no-install main.ts)" == "42" ]] ||
      { log "$variant Bun TypeScript execution failed"; return 1; }
    "${offline_env[@]}" "$bun_path" test --no-install main.test.ts
    "${offline_env[@]}" "$bun_path" build --target=bun --outfile=bundle.js main.ts
    [[ "$("${offline_env[@]}" "$bun_path" --no-install bundle.js)" == "42" ]] ||
      { log "$variant Bun bundle execution failed"; return 1; }
  done
)

bun_smoke_script() {
  printf 'public_toolchain_archive_dir=%q\n' "$public_toolchain_archive_dir"
  printf 'pinned_bun_version=%q\n' "$pinned_bun_version"
  declare -f log toolchain_archive_spec verify_toolchain_archive stage_toolchain_archive pinned_bun_supported bun_optimized_supported stage_bun_archive extract_bun_archive offline_bun_probe
  printf '%s\n' 'if pinned_bun_supported; then' '  offline_bun_probe' 'fi'
}

rust_seed_inputs() {
  printf '%s\n' "channel-rust-$pinned_rust_version.toml" rustup-init-1.29.0-x86_64-unknown-linux-gnu
  printf '%s\n' {rustc,cargo,rust-std,rustfmt}-"$pinned_rust_version"-x86_64-unknown-linux-gnu.tar.xz
}

rust_seed_path() {
  case "$1" in
    channel-rust-*) printf '%s\n' "$rust_seed_root/dist/channel-rust-stable.toml" ;;
    rustup-init-*) printf '%s\n' "$rust_seed_root/rustup-init" ;;
    *) printf '%s\n' "$rust_seed_root/dist/2026-07-16/$1" ;;
  esac
}

check_root_owned_path() {
  python3 - "$1" <<'PY'
import os
import pathlib
import stat
import sys
path = pathlib.Path(sys.argv[1])
for entry in (path, *path.parents):
    if not os.path.lexists(entry):
        continue
    value = entry.lstat()
    if stat.S_ISLNK(value.st_mode) or value.st_uid != 0 or value.st_mode & 0o022:
        sys.exit("linux-tools: unsafe root-owned toolchain path: " + str(entry))
    required = 0o055 if stat.S_ISDIR(value.st_mode) else 0o044
    if value.st_mode & required != required:
        sys.exit("linux-tools: public toolchain path is not readable: " + str(entry))
PY
}

check_rust_seed() {
  local name filename algorithm expected url
  check_root_owned_path "$rust_seed_root" || return $?
  while IFS= read -r name; do
    filename="$(rust_seed_path "$name")" || return $?
    check_root_owned_path "$filename" || return $?
    read -r algorithm expected url <<<"$(toolchain_archive_spec "$name")"
    verify_toolchain_archive "$algorithm" "$expected" "$filename" || return $?
  done < <(rust_seed_inputs)
  filename="$rust_seed_root/dist/channel-rust-stable.toml.sha256"
  check_root_owned_path "$filename" || return $?
  [[ "$(cat "$filename")" == "03569b1886ceb5c05276b50c8431ab111de944cd6140fe1fa7d821dd8e0f29cf  channel-rust-stable.toml" ]]
}

prepare_rust_seed() (
  set -euo pipefail
  local staging name destination relative algorithm expected url
  check_root_owned_path "$rust_seed_root" || return $?
  if [[ -e "$rust_seed_root" ]]; then
    check_rust_seed
    return $?
  fi
  while IFS= read -r name; do
    cache_public_toolchain_archives "$name" || return $?
  done < <(rust_seed_inputs)
  install -d -m 0755 "$(dirname "$rust_seed_root")" || return $?
  staging="$(mktemp -d "$(dirname "$rust_seed_root")/.rust-seed.XXXXXXXX")" || return $?
  # shellcheck disable=SC2064
  trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
  install -d -m 0755 "$staging/dist/2026-07-16" "$staging/download" || return $?
  while IFS= read -r name; do
    stage_toolchain_archive "$name" "$staging/download" || return $?
    destination="$(rust_seed_path "$name")" || return $?
    relative="${destination#"$rust_seed_root"/}"
    install -m 0644 "$staging/download/$name" "$staging/$relative" || return $?
  done < <(rust_seed_inputs)
  printf '%s\n' '03569b1886ceb5c05276b50c8431ab111de944cd6140fe1fa7d821dd8e0f29cf  channel-rust-stable.toml' \
    >"$staging/dist/channel-rust-stable.toml.sha256" || return $?
  chmod 0644 "$staging/dist/channel-rust-stable.toml.sha256" || return $?
  chmod 0755 "$staging" "$staging/rustup-init" || return $?
  rm -rf "$staging/download" || return $?
  check_root_owned_path "$rust_seed_root" || return $?
  [[ ! -e "$rust_seed_root" && ! -L "$rust_seed_root" ]] || return 1
  mv "$staging" "$rust_seed_root" || return $?
  check_rust_seed
)

resolve_rust_runtime_user() {
  local entry password uid gid description shell selected="${SUDO_USER:-${CRABBOX_SSH_USER:-}}"
  [[ -n "$selected" && "$selected" != root && "$selected" != -* ]] || {
    log "run from the nonroot runtime account with sudo, or use the container's CRABBOX_SSH_USER"
    return 1
  }
  entry="$(getent passwd "$selected")" || return $?
  IFS=: read -r runtime_user password uid gid description runtime_home shell <<<"$entry"
  [[ "$runtime_user" == "$selected" && "$uid" =~ ^[0-9]+$ && "$uid" -ne 0 &&
    "$runtime_home" == /* && "$runtime_home" != / && "$shell" == /bin/bash ]] || return 1
}

rust_user_state() {
  # Root provenance is written only after complete provisioning. Unknown or
  # interrupted homes are retained, never treated as safe just because -y permits them.
  python3 - "$1" "$runtime_user" "$runtime_home" "$rust_user_record" "$pinned_rust_version" <<'PY'
import hashlib
import json
import os
import pathlib
import pwd
import stat
import sys
action, user, home, record, version = sys.argv[1:]
account = pwd.getpwnam(user)
home, record = pathlib.Path(home), pathlib.Path(record)
def fail():
    sys.exit("linux-tools: unknown existing Cargo/Rustup state; refusing to modify runtime user installation")
if account.pw_uid == 0 or str(home) != account.pw_dir:
    fail()
for ancestor in (home, *home.parents):
    if ancestor.is_symlink() or not ancestor.is_dir():
        fail()
if home.stat().st_uid != account.pw_uid:
    fail()
startup_files = (".profile", ".bash_profile", ".bash_login", ".bashrc", ".zshenv", ".tcshrc", ".cshrc",
                 ".config/fish/conf.d/rustup.fish", ".config/nushell/config.nu",
                 ".config/powershell/profile.ps1", ".config/xonsh/rc.xsh")
def check_startup_parents(path):
    for parent in path.parents:
        if parent == home:
            break
        if os.path.lexists(parent):
            value = parent.lstat()
            if not stat.S_ISDIR(value.st_mode) or value.st_uid != account.pw_uid or value.st_mode & 0o022:
                fail()
def metadata():
    result = {"user": user, "uid": account.pw_uid, "home": str(home), "version": version, "paths": {}}
    for name in (".cargo", ".rustup", *startup_files):
        path = home / name
        check_startup_parents(path)
        if not os.path.lexists(path):
            if name in (".cargo", ".rustup"):
                fail()
            continue
        value = path.lstat()
        if value.st_uid != account.pw_uid or path.is_symlink() or value.st_mode & 0o022:
            fail()
        if name in (".cargo", ".rustup"):
            if not path.is_dir() or value.st_dev != home.stat().st_dev:
                fail()
            result["paths"][name] = value.st_ino
        else:
            if not path.is_file():
                fail()
            result["paths"][name] = hashlib.sha256(path.read_bytes()).hexdigest()
    return result
for parent in (record.parent, *record.parent.parents):
    if os.path.lexists(parent) and (parent.is_symlink() or not parent.is_dir() or
                                  parent.stat().st_uid != 0 or parent.stat().st_mode & 0o022):
        fail()
if os.path.lexists(record):
    value = record.lstat()
    if not stat.S_ISREG(value.st_mode) or value.st_uid != 0 or value.st_mode & 0o022:
        fail()
    if json.loads(record.read_text()) != metadata():
        fail()
    print("reuse")
elif action == "publish":
    result = metadata()
    record.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    with record.open("x") as output:
        json.dump(result, output, sort_keys=True)
    record.chmod(0o644)
elif action == "check":
    if any(os.path.lexists(home / name) for name in (".cargo", ".rustup")):
        fail()
    for name in startup_files:
        path = home / name
        check_startup_parents(path)
        if os.path.lexists(path):
            value = path.lstat()
            if (not stat.S_ISREG(value.st_mode) or value.st_uid != account.pw_uid or
                    value.st_mode & 0o022):
                fail()
    for directory in ("/usr/local/bin", "/usr/bin", "/bin"):
        if any(os.path.lexists(pathlib.Path(directory) / tool) for tool in ("rustup", "rustc", "cargo", "rustfmt")):
            fail()
    print("fresh")
else:
    fail()
PY
}

run_rust_runtime_user() {
  (cd / && runuser -u "$runtime_user" -- env -i "HOME=$runtime_home" "USER=$runtime_user" "LOGNAME=$runtime_user" \
    SHELL=/bin/bash PATH=/usr/local/bin:/usr/bin:/bin CI=1 \
    "RUSTUP_DIST_SERVER=file://$rust_seed_root" "RUSTUP_UPDATE_ROOT=file://$rust_seed_root/disabled-self-update" "$@")
}

check_rust_zsh_directory() {
  # Rustup asks zsh for ZDOTDIR even during Bash setup. Resolve it with the
  # same unprivileged environment before permitting writes outside standard paths.
  run_rust_runtime_user python3 - <<'PY'
import os
import pathlib
import shutil
import stat
import subprocess
import sys
if shutil.which("zsh"):
    value = subprocess.check_output(["zsh", "-c", 'printf "%s" "${ZDOTDIR:-$HOME}"'],
                                    text=True, timeout=10)
    home = pathlib.Path.home()
    if not value or "\n" in value:
        sys.exit("linux-tools: invalid Rustup ZDOTDIR")
    directory = pathlib.Path(os.path.abspath(value))
    try:
        directory.relative_to(home)
    except ValueError:
        sys.exit("linux-tools: Rustup ZDOTDIR must remain inside the runtime home")
    for entry in (directory / ".zshenv", directory, *directory.parents):
        if entry == home:
            break
        if os.path.lexists(entry):
            mode = entry.lstat()
            expected = stat.S_ISREG if entry.name == ".zshenv" else stat.S_ISDIR
            if not expected(mode.st_mode) or mode.st_uid != os.getuid() or mode.st_mode & 0o022:
                sys.exit("linux-tools: unsafe Rustup ZDOTDIR startup path")
PY
}

rust_runtime_probe() (
  set -euo pipefail
  [[ "$(id -u)" -ne 0 ]] || { log "Rust smoke must run as a nonroot user"; return 1; }
  local staging tool selector version result
  for tool in rustup rustc cargo rustfmt; do
    [[ "$(command -v "$tool")" == "$HOME/.cargo/bin/$tool" ]] || {
      log "normal login PATH does not resolve the runtime user's $tool"; return 1;
    }
  done
  [[ -f "$HOME/.rustup/settings.toml" && ! -L "$HOME/.rustup" &&
    -d "$HOME/.rustup/toolchains/stable-x86_64-unknown-linux-gnu" &&
    ! -L "$HOME/.rustup/toolchains/stable-x86_64-unknown-linux-gnu" ]] || {
    log "baked stable Rust toolchain is missing"; return 1;
  }
  check_rust_seed || return $?
  staging="$(mktemp -d)" || return $?
  # shellcheck disable=SC2064
  trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
  mkdir -p "$staging/src" "$staging/cargo" || return $?
  printf '%s\n' '[package]' 'name = "crabbox-offline-rust"' 'version = "0.1.0"' 'edition = "2021"' >"$staging/Cargo.toml" || return $?
  printf '%s\n' 'fn main() { println!("rust-offline-ok"); }' \
    '#[test] fn answer() { assert_eq!(6 * 7, 42); }' >"$staging/src/main.rs" || return $?
  local -a offline_env=(env -i "HOME=$HOME" "PATH=$PATH" "TMPDIR=$staging" "CARGO_HOME=$staging/cargo"
    "CARGO_TARGET_DIR=$staging/target" CARGO_NET_OFFLINE=true RUSTUP_AUTO_INSTALL=0 CI=1
    "RUSTUP_DIST_SERVER=file://$rust_seed_root" "RUSTUP_UPDATE_ROOT=file://$rust_seed_root/disabled-self-update")
  cd "$staging" || return $?
  "${offline_env[@]}" rustfmt src/main.rs || return $?
  "${offline_env[@]}" cargo generate-lockfile --offline || return $?
  for selector in "" +stable; do
    version="$("${offline_env[@]}" rustc ${selector:+"$selector"} --version)" || return $?
    [[ "$version" == "rustc $pinned_rust_version "* ]] || return 1
    "${offline_env[@]}" cargo ${selector:+"$selector"} fmt --check || return $?
    "${offline_env[@]}" cargo ${selector:+"$selector"} check --offline --locked || return $?
    "${offline_env[@]}" cargo ${selector:+"$selector"} test --offline --locked || return $?
    result="$("${offline_env[@]}" cargo ${selector:+"$selector"} run --offline --locked --quiet)" || return $?
    [[ "$result" == rust-offline-ok ]] || return 1
  done
  "${offline_env[@]}" cargo install --offline --locked --path . || return $?
  [[ "$("$staging/cargo/bin/crabbox-offline-rust")" == rust-offline-ok ]]
)

install_rust() {
  linux_x64_supported || return 0
  local runtime_user runtime_home state probe
  resolve_rust_runtime_user || return $?
  state="$(rust_user_state check)" || return $?
  if [[ "$state" == fresh ]]; then
    check_rust_zsh_directory || return $?
  fi
  prepare_rust_seed || return $?
  if [[ "$state" == fresh ]]; then
    # Startup commands and seed preparation can outlive the initial preflight.
    # Never let rustup -y adopt state created while those steps were running.
    state="$(rust_user_state check)" || return $?
    [[ "$state" == fresh ]] || { log "Rust runtime state changed during preparation; refusing initialization"; return 1; }
    run_rust_runtime_user /bin/bash -c 'cd / && exec "$1" -y --default-host x86_64-unknown-linux-gnu --default-toolchain none --profile minimal' _ "$rust_seed_root/rustup-init" || return $?
    run_rust_runtime_user "$runtime_home/.cargo/bin/rustup" set auto-self-update disable || return $?
    run_rust_runtime_user "$runtime_home/.cargo/bin/rustup" toolchain install stable --profile minimal --component rustfmt --no-self-update || return $?
    run_rust_runtime_user "$runtime_home/.cargo/bin/rustup" default stable || return $?
  fi
  probe="$(rust_smoke_script)" || return $?
  run_rust_runtime_user /bin/bash -lc "cd /; $probe" || return $?
  rust_user_state publish >/dev/null
}

rust_smoke_script() {
  printf 'pinned_rust_version=%q\nrust_seed_root=%q\n' "$pinned_rust_version" "$rust_seed_root"
  declare -f log linux_x64_supported toolchain_archive_spec verify_toolchain_archive check_root_owned_path rust_seed_inputs rust_seed_path check_rust_seed rust_runtime_probe
  printf '%s\n' 'if linux_x64_supported; then' '  rust_runtime_probe' 'fi'
}

install_uv() (
  set -euo pipefail
  linux_x64_supported || return 0
  local staging destination="$uv_toolchain_root/$pinned_uv_version" name="uv-$pinned_uv_version-x86_64-unknown-linux-gnu.tar.gz" tool version
  public_tool_links check "$uv_bin_dir" "$destination" uv uvx || return $?
  check_root_owned_path "$destination" || return $?
  cache_public_toolchain_archives "$name" || return $?
  staging="$(mktemp -d)" || return $?
  # shellcheck disable=SC2064
  trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
  stage_toolchain_archive "$name" "$staging" || return $?
  tar --no-same-owner -xzf "$staging/$name" -C "$staging" || return $?
  for tool in uv uvx; do
    version="$("$staging/uv-x86_64-unknown-linux-gnu/$tool" --version)" || return $?
    [[ "$version" == "$tool $pinned_uv_version" || "$version" == "$tool $pinned_uv_version ("* ]] || return 1
  done
  public_tool_links check "$uv_bin_dir" "$destination" uv uvx || return $?
  check_root_owned_path "$destination" || return $?
  install -d -m 0755 "$uv_toolchain_root" "$uv_bin_dir" || return $?
  chmod 0755 "$staging/uv-x86_64-unknown-linux-gnu" || return $?
  rm -rf "$destination" || return $?
  mv "$staging/uv-x86_64-unknown-linux-gnu" "$destination" || return $?
  public_tool_links publish "$uv_bin_dir" "$destination" uv uvx
)

offline_uv_probe() (
  set -euo pipefail
  [[ "$(id -u)" -ne 0 ]] || { log "uv smoke must run as a nonroot user"; return 1; }
  local staging name="uv-$pinned_uv_version-x86_64-unknown-linux-gnu.tar.gz" uv_path tool version result
  staging="$(mktemp -d)" || return $?
  # shellcheck disable=SC2064
  trap "$(printf 'rm -rf -- %q' "$staging")" EXIT
  mkdir "$staging/home" "$staging/project" "$staging/wheels" || return $?
  printf '%s\n' '[build-system]' 'requires = ["setuptools", "wheel"]' 'build-backend = "setuptools.build_meta"' >"$staging/project/pyproject.toml" || return $?
  printf '%s\n' '[metadata]' 'name = crabbox-offline-python' 'version = 0.1.0' '[options]' 'py_modules = offline_console' \
    '[options.entry_points]' 'console_scripts =' '    crabbox-offline-python = offline_console:main' >"$staging/project/setup.cfg" || return $?
  printf '%s\n' 'def main():' '    print("uv-offline-ok")' >"$staging/project/offline_console.py" || return $?
  local -a offline_env=(env -i "HOME=$staging/home" "PATH=$PATH" "TMPDIR=$staging" "UV_CACHE_DIR=$staging/cache"
    "XDG_CACHE_HOME=$staging/home/cache" PYTHONDONTWRITEBYTECODE=1)
  for tool in uv uvx; do
    version="$("${offline_env[@]}" "$tool" --version)" || return $?
    [[ "$version" == "$tool $pinned_uv_version" || "$version" == "$tool $pinned_uv_version ("* ]] || return 1
  done
  stage_toolchain_archive "$name" "$staging" || return $?
  tar --no-same-owner -xzf "$staging/$name" -C "$staging" || return $?
  cd "$staging" || return $?
  "${offline_env[@]}" /usr/bin/python3 -I -B -m build --wheel --no-isolation --outdir "$staging/wheels" "$staging/project" || return $?
  for uv_path in "" "$staging/uv-x86_64-unknown-linux-gnu/"; do
    "${offline_env[@]}" "${uv_path}uv" --offline --no-config --no-python-downloads venv --python /usr/bin/python3 "$staging/venv" || return $?
    "${offline_env[@]}" "${uv_path}uv" --offline --no-config --no-python-downloads pip install --python "$staging/venv/bin/python" "$staging"/wheels/*.whl || return $?
    result="$("${offline_env[@]}" "$staging/venv/bin/crabbox-offline-python")" || return $?
    [[ "$result" == uv-offline-ok ]] || return 1
    result="$("${offline_env[@]}" "${uv_path}uvx" --offline --no-config --no-python-downloads --python /usr/bin/python3 --from "$staging"/wheels/*.whl crabbox-offline-python)" || return $?
    [[ "$result" == uv-offline-ok ]] || return 1
    rm -rf "$staging/venv" "$staging/cache" || return $?
  done
)

uv_smoke_script() {
  printf 'public_toolchain_archive_dir=%q\npinned_uv_version=%q\n' "$public_toolchain_archive_dir" "$pinned_uv_version"
  declare -f log linux_x64_supported toolchain_archive_spec verify_toolchain_archive stage_toolchain_archive offline_uv_probe
  printf '%s\n' 'if linux_x64_supported; then' '  offline_uv_probe' 'fi'
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
  "$readiness_producer" || return $?
  "$readiness_producer" --verify linux-builder || return $?
  systemctl disable --now apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true
  systemctl mask apt-daily.service apt-daily-upgrade.service 2>/dev/null || true
  clean_cloud_init_state || return $?
  sync
}

print_versions() {
  local runtime_user runtime_home
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
    uv --version
    uvx --version
    resolve_rust_runtime_user || return $?
    run_rust_runtime_user env RUSTUP_AUTO_INSTALL=0 /bin/bash -lc \
      'set -e; cd /; rustup --version; rustc --version; cargo --version; rustfmt --version' || return $?
  fi
  if pinned_bun_supported; then
    bun --version
    command -v bunx
  fi
  "$trufflehog_bin_dir/trufflehog" --no-update --version
  docker --version
  docker compose version
}

install_node_only() {
  local started=$SECONDS
  export DEBIAN_FRONTEND=noninteractive
  retry apt-get update
  apt_install ca-certificates curl gnupg python3-minimal xz-utils
  add_nodesource
  if ! pinned_node_supported; then
    retry apt-get update
  fi
  install_node_runtime
  node --version
  npm --version
  log "Node baseline installed in $((SECONDS - started))s"
}

main() {
  need_root "$@"
  if [[ "${1:-}" == "--node-only" ]]; then
    install_node_only
    return
  fi
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
  apt_install "${apt_get_base[@]}" "${readiness_packages[@]}" || return $?
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
    cmake \
    ninja-build \
    autoconf \
    automake \
    gawk \
    nasm \
    yasm \
    bat \
    direnv \
    zoxide \
    sqlite3 \
    python3-pip \
    python3-dev \
    python3-build \
    python3-setuptools \
    python3-wheel \
    netcat-openbsd \
    iproute2 \
    openssl \
    at-spi2-core \
    dbus-x11 \
    ffmpeg \
    file \
    gir1.2-atspi-2.0 \
    gstreamer1.0-libav \
    gstreamer1.0-plugins-bad \
    gstreamer1.0-plugins-good \
    gstreamer1.0-tools \
    libatk-adaptor \
    libayatana-appindicator3-dev \
    libegl1 \
    libgles2 \
    librsvg2-dev \
    libssl-dev \
    libwebkit2gtk-4.1-dev \
    libxdo-dev \
    mesa-utils \
    patchelf \
    pciutils \
    procps \
    psmisc \
    python3-gi \
    wget \
    wmctrl \
    xauth \
    xdg-utils \
    xvfb || return $?

  if [[ ! -e /usr/local/bin/fd && -x /usr/bin/fdfind ]]; then
    ln -sf /usr/bin/fdfind /usr/local/bin/fd
  fi

  if [[ "$install_desktop" == "1" ]]; then
    apt_install xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-terminal xfconf xfce4-settings x11vnc x11-xserver-utils xterm scrot xdotool xclip xsel fonts-dejavu-core fonts-liberation || return $?
  fi
  if [[ "$install_browser" == "1" ]]; then
    install_chrome_or_chromium
  fi
  install_node_pnpm || return $?
  install_go_toolchain || return $?
  install_bun
  install_uv || return $?
  install_rust || return $?
  install_trufflehog
  install_docker
  prepare_fast_boot
  print_versions
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
