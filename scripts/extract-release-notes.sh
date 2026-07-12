#!/usr/bin/env bash
set -euo pipefail

TAG=${1:-}
CHANGELOG=${2:--}
if [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "usage: $0 vX.Y.Z [changelog-file|-]" >&2
  exit 2
fi

version=${TAG#v}
if [[ "$CHANGELOG" == - ]]; then
  input=/dev/stdin
else
  input=$CHANGELOG
fi

awk -v version="$version" '
  $1 == "##" && $2 == version { in_section = 1 }
  in_section && $1 == "##" && $2 != version { exit }
  in_section { print }
' "$input" | awk '
  { lines[NR] = $0 }
  END {
    last = NR
    while (last > 0 && lines[last] == "") last--
    if (last == 0) exit 1
    for (i = 1; i <= last; i++) print lines[i]
  }
'
