#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$ROOT/scripts/release-config.sh"
SOURCE=${1:?source checkout required}
COMMIT=${2:?frozen source commit required}
OUTPUT=${3:?output directory required}
WORK=${4:?isolated build directory required}
[[ "$COMMIT" =~ ^[0-9a-f]{40}$ ]]
[[ "$(git -C "$SOURCE" rev-parse HEAD)" == "$COMMIT" ]]
[[ -z "$(git -C "$SOURCE" status --porcelain --untracked-files=all)" ]]
[[ ! -e "$OUTPUT" && ! -e "$WORK" ]]
mkdir -p "$OUTPUT"
mkdir -m 700 "$WORK" "$WORK/home" "$WORK/tmp"
go_bin=$(command -v go)
clean_build_path="${go_bin%/*}:/usr/bin:/bin:/usr/sbin:/sbin"
platforms=(linux)
runtime_verification_args=()
runtime_ldflags='-s -w'
if git -C "$SOURCE" cat-file -e "$COMMIT:internal/runner/development/main.go.txt" 2>/dev/null; then
  # Protected tooling reads frozen helper source as data. It does not execute
  # candidate code to obtain the identity used by the filesystem handshake.
  (
    cd "$ROOT"
    env -i GOCACHE="$WORK/gocache" GOMODCACHE="$WORK/gomodcache" \
      GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org \
      GOTOOLCHAIN="$CRABBOX_RELEASE_GO_VERSION" GOWORK=off \
      HOME="$WORK/home" PATH="$clean_build_path" TMPDIR="$WORK/tmp" \
      "$go_bin" build -trimpath -buildvcs=false -o "$WORK/runtime-artifacts" ./scripts/runtime-artifacts
  )
  "$WORK/runtime-artifacts" source-id --source-directory "$SOURCE" > "$WORK/filesystem-build-id"
  filesystem_build_id=$(cat "$WORK/filesystem-build-id")
  [[ "$filesystem_build_id" =~ ^[0-9a-f]{64}$ ]]
  runtime_ldflags="-s -w -X github.com/openclaw/crabbox/internal/runner.BuildID=$filesystem_build_id"
  platforms=(darwin linux windows)
  runtime_verification_args=(filesystem)
fi
for platform in "${platforms[@]}"; do
 for arch in amd64 arm64; do
  runtime_name="$platform-$arch"
  [[ "$platform" != windows ]] || runtime_name+=.exe
  (
    cd "$SOURCE"
    env -i \
      CGO_ENABLED=0 GOOS="$platform" GOARCH="$arch" GOAMD64=v1 GOARM64=v8.0 \
      GOCACHE="$WORK/gocache" GOMODCACHE="$WORK/gomodcache" \
      GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org \
      GOTOOLCHAIN="$CRABBOX_RELEASE_GO_VERSION" GOWORK=off \
      HOME="$WORK/home" PATH="$clean_build_path" TMPDIR="$WORK/tmp" \
      "$go_bin" build -trimpath -buildvcs=true -ldflags="$runtime_ldflags" \
      -o "$OUTPUT/$runtime_name" ./cmd/crabbox-runtime
  )
  node "$ROOT/scripts/verify-go-release-binary.mjs" \
    "$OUTPUT/$runtime_name" github.com/openclaw/crabbox/cmd/crabbox-runtime \
    "$COMMIT" "$platform" "$arch" "$CRABBOX_RELEASE_GO_VERSION" ${runtime_verification_args[@]+"${runtime_verification_args[@]}"}
 done
done
[[ -z "$(git -C "$SOURCE" status --porcelain --untracked-files=all)" ]]
