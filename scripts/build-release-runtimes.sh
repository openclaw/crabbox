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
for arch in amd64 arm64; do
  (
    cd "$SOURCE"
    env -i \
      CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOAMD64=v1 GOARM64=v8.0 \
      GOCACHE="$WORK/gocache" GOMODCACHE="$WORK/gomodcache" \
      GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org \
      GOTOOLCHAIN="$CRABBOX_RELEASE_GO_VERSION" GOWORK=off \
      HOME="$WORK/home" PATH="$clean_build_path" TMPDIR="$WORK/tmp" \
      "$go_bin" build -trimpath -buildvcs=true -ldflags="-s -w" \
      -o "$OUTPUT/linux-$arch" ./cmd/crabbox-runtime
  )
  node "$ROOT/scripts/verify-go-release-binary.mjs" \
    "$OUTPUT/linux-$arch" github.com/openclaw/crabbox/cmd/crabbox-runtime \
    "$COMMIT" linux "$arch" "$CRABBOX_RELEASE_GO_VERSION"
done
[[ -z "$(git -C "$SOURCE" status --porcelain --untracked-files=all)" ]]
