#!/bin/sh
# Builds the Swift VM daemon and stages it for embedding into the
# apple-vm helper (go build -tags vmdembed). Requires macOS with Xcode.
set -eu

cd "$(dirname "$0")/.."

swift build --package-path vmd -c release
mkdir -p internal/applevmhelper/embedded
cp vmd/.build/release/crabbox-apple-vm-vmd internal/applevmhelper/embedded/crabbox-apple-vm-vmd

minos="$(vtool -show-build internal/applevmhelper/embedded/crabbox-apple-vm-vmd | awk '$1 == "minos" { print $2; exit }')"
if [ "$minos" != "13.0" ]; then
  echo "crabbox-apple-vm-vmd minimum macOS is ${minos:-unknown}, expected 13.0" >&2
  exit 1
fi
echo "staged internal/applevmhelper/embedded/crabbox-apple-vm-vmd (minos $minos)"
