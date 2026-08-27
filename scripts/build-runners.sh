#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT="$ROOT/internal/runner/embedded"
BUILD_ID=$(git -C "$ROOT" rev-parse HEAD)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-runners.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT/raw"

# The caller pins the release toolchain/environment. Official helpers are built
# from the clean source checkout, not the development embedded-source compiler.
for target_os in darwin linux windows; do
  for target_arch in amd64 arm64; do
    name="crabbox-runner-${target_os}-${target_arch}"
    [[ "$target_os" != windows ]] || name="${name}.exe"
    (
      cd "$ROOT"
      CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
        go build -p 1 -trimpath -buildvcs=true \
        -ldflags="-s -w -X github.com/openclaw/crabbox/internal/runner.BuildID=$BUILD_ID" \
        -o "$WORK/$name" ./cmd/crabbox-runner
    )
    cp "$WORK/$name" "$OUT/raw/$name"
  done
done
node "$ROOT/scripts/pack-release-runners.mjs" "$WORK" "$BUILD_ID" "$WORK/runners.bin"
mv "$WORK/runners.bin" "$OUT/runners.bin"
