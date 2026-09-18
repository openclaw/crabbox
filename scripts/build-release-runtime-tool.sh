#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$ROOT/scripts/release-config.sh"
WORK=${1:?isolated tool build directory required}
[[ ! -e "$WORK" ]]
mkdir -m 700 "$WORK" "$WORK/home" "$WORK/tmp"
go_bin=$(command -v go)
# Build and execute only protected tooling, never the candidate's tool source.
# A private HOME and empty environment exclude release credentials and Go flags.
(
  cd "$ROOT"
  env -i CGO_ENABLED=0 GOTOOLCHAIN="$CRABBOX_RELEASE_GO_VERSION" GOWORK=off \
    GOCACHE="$WORK/gocache" GOMODCACHE="$WORK/gomodcache" \
    GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org \
    HOME="$WORK/home" TMPDIR="$WORK/tmp" \
    PATH="${go_bin%/*}:/usr/bin:/bin:/usr/sbin:/sbin" \
    "$go_bin" build -trimpath -buildvcs=true \
      -o "$WORK/runtime-artifacts" ./scripts/runtime-artifacts
)
