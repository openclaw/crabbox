#!/usr/bin/env bash
set -euo pipefail

brew_update="${CRABBOX_MACOS_BREW_UPDATE:-1}"
pnpm_version="${CRABBOX_MACOS_PNPM_VERSION:-11.1.0}"
node_formula="${CRABBOX_MACOS_NODE_FORMULA:-node@24}"
python_formula="${CRABBOX_MACOS_PYTHON_FORMULA:-python@3.13}"
require_xcode="${CRABBOX_MACOS_REQUIRE_XCODE:-0}"
xcode_developer_dir="${CRABBOX_MACOS_XCODE_DEVELOPER_DIR:-}"
force_links="${CRABBOX_MACOS_FORCE_LINKS:-0}"
homebrew_install_commit="280cbc9adffcbdef15dd1c9d991ef2d1dd7cfc9c"
homebrew_install_sha256="f3e91784ffeda32bc397de7acc1154724cc47522a459c9ac656cca176eeba457"
homebrew_install_url="https://raw.githubusercontent.com/Homebrew/install/${homebrew_install_commit}/install.sh"
default_formulas=(
  git
  git-lfs
  gh
  jq
  yq
  ripgrep
  fd
  fzf
  coreutils
  gnu-tar
  gnu-sed
  findutils
  rsync
  shellcheck
  shfmt
  "$python_formula"
  "$node_formula"
)

log() {
  printf 'macos-tools: %s\n' "$*" >&2
}

die() {
  log "$*"
  exit 1
}

shell_single_quote() {
  printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

select_full_xcode() {
  local candidate
  if [[ -n "$xcode_developer_dir" ]]; then
    sudo xcode-select -s "$xcode_developer_dir"
    return
  fi
  for candidate in /Applications/Xcode*.app/Contents/Developer; do
    [[ -d "$candidate" ]] || continue
    if [[ -x "$candidate/usr/bin/xcodebuild" && -d "$candidate/Platforms/MacOSX.platform/Developer/SDKs" ]]; then
      sudo xcode-select -s "$candidate"
      return
    fi
  done
}

require_developer_tools() {
  local developer_dir
  if [[ "$require_xcode" == "1" ]]; then
    select_full_xcode || true
  fi
  developer_dir="$(xcode-select -p 2>/dev/null || true)"
  if [[ -z "$developer_dir" ]]; then
    log "xcode-select has no active developer directory"
    return 1
  fi
  if [[ ! -x "$developer_dir/usr/bin/clang" && ! -x "$developer_dir/Toolchains/XcodeDefault.xctoolchain/usr/bin/clang" ]]; then
    log "active developer directory does not expose clang: $developer_dir"
    return 1
  fi
  xcrun --sdk macosx --show-sdk-path >/dev/null
  xcrun --find clang >/dev/null
  xcrun --find swift >/dev/null
  if [[ "$require_xcode" == "1" ]]; then
    case "$developer_dir" in
      *CommandLineTools*)
        log "full Xcode developer directory required, got Command Line Tools: $developer_dir"
        log "install Xcode.app first or set CRABBOX_MACOS_XCODE_DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer"
        return 1
        ;;
    esac
    if [[ ! -x "$developer_dir/usr/bin/xcodebuild" || ! -d "$developer_dir/Platforms/MacOSX.platform/Developer/SDKs" ]]; then
      log "active developer directory is not a full Xcode.app: $developer_dir"
      return 1
    fi
    sudo xcodebuild -license accept >/dev/null 2>&1 || true
    sudo xcodebuild -runFirstLaunch >/dev/null 2>&1 || true
    xcodebuild -version
    xcodebuild -showsdks | grep -E 'macOS|macosx' >/dev/null
  fi
  log "developer tools: $developer_dir"
}

install_homebrew() {
  local arch installer prefix
  arch="$(uname -m)"
  if [[ "$arch" == "arm64" ]]; then
    prefix="/opt/homebrew"
  else
    prefix="/usr/local"
  fi
  if [[ ! -x "$prefix/bin/brew" ]]; then
    log "installing Homebrew into $prefix"
    if ! installer="$(download_verified_homebrew_installer)"; then
      return 1
    fi
    if ! NONINTERACTIVE=1 CI=1 /bin/bash "$installer"; then
      rm -f "$installer"
      return 1
    fi
    rm -f "$installer"
  fi
  export PATH="$prefix/bin:$prefix/sbin:/usr/local/bin:$PATH"
  if ! command -v brew >/dev/null 2>&1; then
    log "Homebrew installed but brew is not on PATH"
    return 1
  fi
  log "homebrew: $(brew --prefix)"
}

download_verified_homebrew_installer() {
  local dest
  command -v curl >/dev/null 2>&1 || die "missing required command: curl"
  command -v shasum >/dev/null 2>&1 || die "missing required command: shasum"
  dest="$(mktemp)"
  if ! curl -fsSL --retry 3 --output "$dest" "$homebrew_install_url"; then
    rm -f "$dest"
    return 1
  fi
  if ! printf '%s  %s\n' "$homebrew_install_sha256" "$dest" | shasum -a 256 -c - >/dev/null; then
    rm -f "$dest"
    return 1
  fi
  printf '%s\n' "$dest"
}

install_formula() {
  local formula="$1"
  if brew list --formula "$formula" >/dev/null 2>&1; then
    return 0
  fi
  HOMEBREW_NO_INSTALL_CLEANUP=1 brew install "$formula"
}

install_formulas() {
  local formulas=("${default_formulas[@]}")
  local formula parts
  if [[ -n "${CRABBOX_MACOS_BREW_FORMULAS:-}" ]]; then
    if [[ "$CRABBOX_MACOS_BREW_FORMULAS" == *$'\n'* ]]; then
      formulas=()
      while IFS= read -r formula; do
        read -r -a parts <<<"$formula"
        formulas+=("${parts[@]}")
      done <<<"$CRABBOX_MACOS_BREW_FORMULAS"
    else
      read -r -a formulas <<<"$CRABBOX_MACOS_BREW_FORMULAS"
    fi
  fi
  if [[ "$brew_update" == "1" ]]; then
    HOMEBREW_NO_INSTALL_CLEANUP=1 brew update
  fi
  for formula in "${formulas[@]}"; do
    if [[ -z "$formula" ]]; then
      continue
    fi
    if ! install_formula "$formula"; then
      case "$formula" in
        node@*)
          log "$formula unavailable; falling back to node"
          install_formula node
          ;;
        python@*)
          log "$formula unavailable; falling back to python@3.12"
          install_formula python@3.12
          ;;
        *)
          return 1
          ;;
      esac
    fi
  done
  brew cleanup --prune=7 || true
}

tool_path() {
  local name="$1"
  shift
  local candidate
  for candidate in "$@"; do
    if [[ -x "$candidate/$name" ]]; then
      printf '%s/%s\n' "$candidate" "$name"
      return 0
    fi
  done
  command -v "$name" 2>/dev/null || return 1
}

link_tool() {
  local name="$1"
  shift
  local dest src tmp
  src="$(tool_path "$name" "$@")"
  dest="/usr/local/bin/$name"
  if [[ "$src" == "$dest" ]]; then
    return 0
  fi
  sudo mkdir -p /usr/local/bin
  if [[ -L "$dest" && "$(readlink "$dest")" == "$src" ]]; then
    return 0
  fi
  if [[ ( -e "$dest" || -L "$dest" ) && "$force_links" != "1" ]]; then
    die "refusing to replace existing $dest; set CRABBOX_MACOS_FORCE_LINKS=1 to override"
  fi
  tmp="$(mktemp -d)"
  ln -s "$src" "$tmp/$name"
  sudo mv -f "$tmp/$name" "$dest"
  rmdir "$tmp"
}

install_brew_wrapper() {
  local brew_path="$1"
  local dest=/usr/local/bin/brew
  local quoted_brew tmp
  if [[ "$(dirname "$brew_path")" == "/usr/local/bin" ]]; then
    return 0
  fi
  sudo mkdir -p /usr/local/bin
  if [[ -L "$dest" && "$(readlink "$dest")" == "$brew_path" ]]; then
    return 0
  fi
  if [[ -f "$dest" ]] && grep -qx '# crabbox-managed brew wrapper' "$dest"; then
    :
  elif is_legacy_brew_wrapper "$dest" "$brew_path"; then
    :
  elif [[ ( -e "$dest" || -L "$dest" ) && "$force_links" != "1" ]]; then
    die "refusing to replace existing $dest; set CRABBOX_MACOS_FORCE_LINKS=1 to override"
  fi
  quoted_brew="$(shell_single_quote "$brew_path")"
  tmp="$(mktemp)"
  printf '#!/bin/sh\n# crabbox-managed brew wrapper\nexec %s "$@"\n' "$quoted_brew" >"$tmp"
  chmod 0755 "$tmp"
  sudo install -o root -g wheel -m 0755 "$tmp" "$dest"
  rm -f "$tmp"
}

is_legacy_brew_wrapper() {
  local path="$1"
  local brew_path="$2"
  local expected line
  [[ -f "$path" ]] || return 1
  expected="exec ${brew_path} \"\$@\""
  line="$(sed -n '2p' "$path")"
  [[ "$line" == "$expected" ]]
}

install_node_and_pnpm() {
  local brew_prefix node_bin npm_bin corepack_bin
  brew_prefix="$(brew --prefix)"
  node_bin="$brew_prefix/opt/$node_formula/bin"
  if [[ ! -x "$node_bin/node" ]]; then
    node_bin="$brew_prefix/opt/node/bin"
  fi
  export PATH="$node_bin:$PATH"
  node --version
  npm --version
  corepack enable
  corepack prepare "pnpm@$pnpm_version" --activate

  npm_bin="$(dirname "$(command -v npm)")"
  corepack_bin="$(dirname "$(command -v corepack)")"
  link_tool node "$node_bin" "$npm_bin"
  link_tool npm "$node_bin" "$npm_bin"
  link_tool npx "$node_bin" "$npm_bin"
  link_tool corepack "$node_bin" "$corepack_bin"
  link_tool pnpm "$node_bin" "$corepack_bin" "$npm_bin"
}

link_common_tools() {
  local brew_prefix python_bin
  brew_prefix="$(brew --prefix)"
  python_bin="$brew_prefix/opt/$python_formula/libexec/bin"
  if [[ ! -x "$python_bin/python3" ]]; then
    python_bin="$brew_prefix/opt/python@3.12/libexec/bin"
  fi
  install_brew_wrapper "$brew_prefix/bin/brew"
  link_tool git "$brew_prefix/bin"
  link_tool git-lfs "$brew_prefix/bin"
  link_tool gh "$brew_prefix/bin"
  link_tool jq "$brew_prefix/bin"
  link_tool yq "$brew_prefix/bin"
  link_tool rg "$brew_prefix/bin"
  link_tool fd "$brew_prefix/bin"
  link_tool fzf "$brew_prefix/bin"
  link_tool shellcheck "$brew_prefix/bin"
  link_tool shfmt "$brew_prefix/bin"
  link_tool python3 "$python_bin" "$brew_prefix/bin"
  export PATH="/usr/local/bin:$PATH"
  hash -r 2>/dev/null || true
}

prepare_cache_dirs() {
  sudo mkdir -p /var/cache/crabbox/pnpm /var/cache/crabbox/npm /var/cache/crabbox/corepack
  sudo chmod 1777 /var/cache/crabbox /var/cache/crabbox/pnpm /var/cache/crabbox/npm /var/cache/crabbox/corepack
}

print_versions() {
  hash -r 2>/dev/null || true
  sw_vers
  xcode-select -p
  xcrun --sdk macosx --show-sdk-path
  clang --version | head -n 1
  swift --version
  brew --version | head -n 1
  git --version
  gh --version | head -n 1
  jq --version
  rg --version | head -n 1
  fd --version
  python3 --version
  node --version
  npm --version
  corepack --version
  pnpm --version
}

if [[ "${CRABBOX_MACOS_SOURCE_ONLY:-0}" == "1" ]]; then
  # shellcheck disable=SC2317
  return 0 2>/dev/null || exit 0
fi

require_developer_tools
install_homebrew
install_formulas
install_node_and_pnpm
link_common_tools
prepare_cache_dirs
print_versions
