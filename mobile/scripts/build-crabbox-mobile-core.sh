#!/usr/bin/env bash
set -euo pipefail

out_dir="${1:-"${PWD}/.build/crabboxmobile"}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
sdk_name="${SDK_NAME:-iphoneos}"
deployment_target="${IPHONEOS_DEPLOYMENT_TARGET:-17.0}"

case "$sdk_name" in
  iphoneos*) sdk="iphoneos"; min_flag="-mios-version-min=${deployment_target}"; target_archs="arm64" ;;
  iphonesimulator*)
    sdk="iphonesimulator"
    min_flag="-mios-simulator-version-min=${deployment_target}"
    target_archs="${ARCHS:-${CURRENT_ARCH:-$(uname -m)}}"
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

build_archive() {
  local arch="$1"
  local goarch
  case "$arch" in
    arm64|aarch64) arch="arm64"; goarch="arm64" ;;
    x86_64) goarch="amd64" ;;
    *)
      echo "unsupported ${sdk} arch: $arch" >&2
      exit 2
      ;;
  esac

  local archive="$out_dir/libcrabboxmobile-${arch}.a"
  export CGO_ENABLED=1
  export GOOS=ios
  export GOARCH="$goarch"
  export CC="$cc"
  export CGO_CFLAGS="-arch ${arch} -isysroot ${sdk_path} ${min_flag}"
  export CGO_LDFLAGS="-arch ${arch} -isysroot ${sdk_path} ${min_flag}"

  (
    cd "$repo_root"
    go build -trimpath -buildmode=c-archive \
      -o "$archive" \
      ./mobile/go/crabboxmobile
  )

  cp "${archive%.a}.h" "$out_dir/libcrabboxmobile.h"
  printf '%s\n' "$archive"
}

archives=()
for arch in $target_archs; do
  if [ "$arch" = "undefined_arch" ]; then
    continue
  fi
  archive="$(build_archive "$arch")"
  archives+=("$archive")
done

if [ "${#archives[@]}" -eq 0 ]; then
  archive="$(build_archive "$(uname -m)")"
  archives+=("$archive")
fi

if [ "${#archives[@]}" -eq 1 ]; then
  cp "${archives[0]}" "$out_dir/libcrabboxmobile.a"
else
  xcrun lipo -create "${archives[@]}" -output "$out_dir/libcrabboxmobile.a"
fi

cat >"$out_dir/module.modulemap" <<'MODULEMAP'
module CrabboxMobile {
  header "libcrabboxmobile.h"
  link "crabboxmobile"
  export *
}
MODULEMAP

echo "CrabboxMobile built at $out_dir"
