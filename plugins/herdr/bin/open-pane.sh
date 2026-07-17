#!/bin/sh
set -eu

entrypoint=${1:-}
placement=${2:-}
herdr_bin=${HERDR_BIN_PATH:-herdr}
plugin_root=${HERDR_PLUGIN_ROOT:-$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)}
shim="$plugin_root/crabbox-shim.sh"

if [ -z "$entrypoint" ] || [ -z "$placement" ]; then
  echo "usage: open-pane.sh <entrypoint> <placement>" >&2
  exit 2
fi
if [ ! -x "$shim" ]; then
  echo "Crabbox plugin shim is missing. Run: sh $plugin_root/build.sh" >&2
  exit 127
fi

set -- plugin pane open \
  --plugin crabbox \
  --entrypoint "$entrypoint" \
  --placement "$placement" \
  --focus

case "$placement" in
  overlay)
    ;;
  split)
    if [ -n "${HERDR_PANE_ID:-}" ]; then
      set -- "$@" --target-pane "$HERDR_PANE_ID"
    fi
    set -- "$@" --direction right
    ;;
  tab)
    if [ -n "${HERDR_WORKSPACE_ID:-}" ]; then
      set -- "$@" --workspace "$HERDR_WORKSPACE_ID"
    fi
    ;;
  *)
    echo "unsupported plugin pane placement: $placement" >&2
    exit 2
    ;;
esac

exec "$herdr_bin" "$@"
