#!/usr/bin/env bash
set -euo pipefail

brew_update="${CRABBOX_MACOS_BREW_UPDATE:-1}"
pnpm_version="${CRABBOX_MACOS_PNPM_VERSION:-11.1.0}"
node_formula="${CRABBOX_MACOS_NODE_FORMULA:-node@24}"
python_formula="${CRABBOX_MACOS_PYTHON_FORMULA:-python@3.13}"
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

require_command_line_tools() {
  local developer_dir
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
  log "developer tools: $developer_dir"
}

install_homebrew() {
  local arch prefix
  arch="$(uname -m)"
  if [[ "$arch" == "arm64" ]]; then
    prefix="/opt/homebrew"
  else
    prefix="/usr/local"
  fi
  if [[ ! -x "$prefix/bin/brew" ]]; then
    log "installing Homebrew into $prefix"
    NONINTERACTIVE=1 CI=1 /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
  fi
  export PATH="$prefix/bin:$prefix/sbin:/usr/local/bin:$PATH"
  if ! command -v brew >/dev/null 2>&1; then
    log "Homebrew installed but brew is not on PATH"
    return 1
  fi
  log "homebrew: $(brew --prefix)"
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
  local formula
  if [[ -n "${CRABBOX_MACOS_BREW_FORMULAS:-}" ]]; then
    # shellcheck disable=SC2206
    formulas=(${CRABBOX_MACOS_BREW_FORMULAS})
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
  local src
  src="$(tool_path "$name" "$@")"
  sudo mkdir -p /usr/local/bin
  sudo ln -sf "$src" "/usr/local/bin/$name"
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
  python_bin="$brew_prefix/opt/$python_formula/bin"
  if [[ ! -x "$python_bin/python3" ]]; then
    python_bin="$brew_prefix/opt/python@3.12/bin"
  fi
  link_tool brew "$brew_prefix/bin"
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
}

prepare_cache_dirs() {
  sudo mkdir -p /var/cache/crabbox/pnpm /var/cache/crabbox/npm /var/cache/crabbox/corepack
  sudo chmod 1777 /var/cache/crabbox /var/cache/crabbox/pnpm /var/cache/crabbox/npm /var/cache/crabbox/corepack
}

print_versions() {
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

require_command_line_tools
install_homebrew
install_formulas
install_node_and_pnpm
link_common_tools
prepare_cache_dirs
print_versions
