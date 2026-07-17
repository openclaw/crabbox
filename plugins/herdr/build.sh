#!/bin/sh
set -eu

plugin_root=${CRABBOX_HERDR_PLUGIN_ROOT:-$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)}
shim="$plugin_root/crabbox-shim.sh"
crabbox_bin=$(command -v crabbox || true)

if [ -n "$crabbox_bin" ]; then
  case "$crabbox_bin" in
    /*) ;;
    *)
      crabbox_dir=$(CDPATH= cd -- "$(dirname -- "$crabbox_bin")" && pwd)
      crabbox_bin="$crabbox_dir/${crabbox_bin##*/}"
      ;;
  esac
fi

if [ -n "$crabbox_bin" ] &&
  probe=$(HERDR_PLUGIN_CONTEXT_JSON='{"workspace_cwd":"/"}' "$crabbox_bin" __herdr-plugin context-cwd 2>/dev/null) &&
  [ "$probe" = / ]; then
  escaped=$(printf '%s' "$crabbox_bin" | sed "s/'/'\\\\''/g")
  printf '%s\n' '#!/bin/sh' "exec '$escaped' \"\$@\"" >"$shim"
  chmod 755 "$shim"
  printf 'Crabbox Herdr plugin: using %s\n' "$crabbox_bin"
  exit 0
fi

printf '%s\n' \
  '#!/bin/sh' \
  'echo "A compatible Crabbox CLI was not found. Install or upgrade Crabbox from https://crabbox.sh." >&2' \
  'exit 127' >"$shim"
chmod 755 "$shim"
printf '%s\n' >&2 \
  'Crabbox Herdr plugin installation requires a compatible crabbox executable on PATH.' \
  'Install or upgrade Crabbox from https://crabbox.sh, then retry the plugin installation.'
exit 1
