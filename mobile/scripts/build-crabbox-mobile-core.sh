#!/usr/bin/env bash
set -euo pipefail

out_dir="${1:-"${PWD}/.build/crabboxmobile"}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
sdk_name="${SDK_NAME:-iphoneos}"
target_arch="${CURRENT_ARCH:-${ARCHS%% *}}"
deployment_target="${IPHONEOS_DEPLOYMENT_TARGET:-17.0}"

case "$sdk_name" in
  iphoneos*) sdk="iphoneos"; goarch="arm64"; min_flag="-mios-version-min=${deployment_target}" ;;
  iphonesimulator*)
    sdk="iphonesimulator"
    min_flag="-mios-simulator-version-min=${deployment_target}"
    case "$target_arch" in
      x86_64) goarch="amd64" ;;
      arm64|"") goarch="arm64" ;;
      *) echo "unsupported simulator arch: $target_arch" >&2; exit 2 ;;
    esac
    ;;
  *)
    echo "unsupported SDK_NAME: $sdk_name (expected iphoneos or iphonesimulator)" >&2
    exit 2
    ;;
esac

if ! command -v go >/dev/null 2>&1; then
  echo "Go is required to build CrabboxMobile" >&2
  exit 2
fi
if ! command -v xcrun >/dev/null 2>&1; then
  echo "Xcode xcrun is required to build CrabboxMobile" >&2
  exit 2
fi

cc="$(xcrun --sdk "$sdk" --find clang)"
sdk_path="$(xcrun --sdk "$sdk" --show-sdk-path)"
mkdir -p "$out_dir"

export CGO_ENABLED=1
export GOOS=ios
export GOARCH="$goarch"
export CC="$cc"
export CGO_CFLAGS="-isysroot ${sdk_path} ${min_flag}"
export CGO_LDFLAGS="-isysroot ${sdk_path} ${min_flag}"

(
  cd "$repo_root"
  go build -trimpath -buildmode=c-archive \
    -o "$out_dir/libcrabboxmobile.a" \
    ./mobile/go/crabboxmobile
)

cat >"$out_dir/module.modulemap" <<'MODULEMAP'
module CrabboxMobile {
  header "libcrabboxmobile.h"
  link "crabboxmobile"
  export *
}
MODULEMAP

echo "CrabboxMobile built at $out_dir"
