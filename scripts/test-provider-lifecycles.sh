#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

packages=()
while IFS= read -r file; do
  if sed -n '1,8p' "$file" | grep -q '^//go:build smoke$'; then
    continue
  fi
  package="./$(dirname "$file")"
  found=0
  for existing in "${packages[@]-}"; do
    if [[ "$existing" == "$package" ]]; then
      found=1
      break
    fi
  done
  if [[ "$found" == "0" ]]; then
    packages+=("$package")
  fi
done < <(git ls-files 'internal/providers/*/*lifecycle*_test.go')

if [[ "${#packages[@]}" == "0" ]]; then
  echo "no convention-named provider lifecycle tests found" >&2
  exit 1
fi

printf 'testing %d provider lifecycle packages\n' "${#packages[@]}"
go test -race "${packages[@]}"
