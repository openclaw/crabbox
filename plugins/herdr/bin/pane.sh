#!/bin/sh
set -eu

command=${1:-}
plugin_root=${HERDR_PLUGIN_ROOT:-$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)}
shim="$plugin_root/crabbox-shim.sh"

if [ -z "$command" ]; then
  echo "usage: pane.sh <boxes|connect|doctor|job|prewarm|warmup>" >&2
  exit 2
fi
if [ ! -x "$shim" ]; then
  echo "Crabbox plugin shim is missing. Run: sh $plugin_root/build.sh" >&2
  exit 127
fi

exec "$shim" __herdr-plugin "$command"
