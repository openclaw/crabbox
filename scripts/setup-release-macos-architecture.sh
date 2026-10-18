#!/usr/bin/env bash
set -euo pipefail

verify_arch=${1:-}
if [[ $# -lt 1 || $# -gt 2 || ( $# == 2 && "${2:-}" != homebrew ) ]] ||
  [[ "$verify_arch" != arm64 && "$verify_arch" != x86_64 ]]; then
  echo "usage: $0 arm64|x86_64 [homebrew]" >&2
  exit 2
fi
test "$(uname -m)" = arm64
[[ "$verify_arch" == x86_64 ]] || exit 0

if ! arch -x86_64 /usr/bin/true; then
  sudo softwareupdate --install-rosetta --agree-to-license
fi
arch -x86_64 /usr/bin/true

if [[ ${2:-} == homebrew ]]; then
  if [[ ! -x /usr/local/bin/brew ]]; then
    curl --fail --silent --show-error --location \
      https://raw.githubusercontent.com/Homebrew/install/be041513824fd2666a0e93cf5c3b0ca3bb0eb4b6/install.sh \
      --output "$RUNNER_TEMP/install-homebrew.sh"
    NONINTERACTIVE=1 arch -x86_64 /bin/bash "$RUNNER_TEMP/install-homebrew.sh"
  fi
  arch -x86_64 /usr/local/bin/brew --prefix | grep -Fx /usr/local
  echo /usr/local/bin >> "$GITHUB_PATH"
  echo /usr/local/sbin >> "$GITHUB_PATH"
fi
