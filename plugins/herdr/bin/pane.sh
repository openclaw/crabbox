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

case "$command" in
  boxes)
    exec "$shim" __herdr-plugin "$command"
    ;;
  connect)
    if "$shim" __herdr-plugin "$command"; then
      exit 0
    else
      command_status=$?
    fi
    ;;
  doctor | job | prewarm | warmup)
    if "$shim" __herdr-plugin "$command"; then
      command_status=0
    else
      command_status=$?
    fi
    ;;
  *)
    echo "unsupported Crabbox plugin pane command: $command" >&2
    exit 2
    ;;
esac

printf '\nCommand exited with status %d. Press Enter to close.\n' "$command_status"
IFS= read -r _ || true
exit "$command_status"
