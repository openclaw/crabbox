#!/usr/bin/env bash
set -euo pipefail

exclude_root_cli=0
if [[ $# -eq 1 && "$1" == --exclude-root-cli ]]; then
  exclude_root_cli=1
elif [[ $# -ne 0 ]]; then
  printf 'usage: %s [--exclude-root-cli]\n' "$0" >&2
  exit 2
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
found=0
modules_file="$(mktemp)"
cleanup() {
  rm -f "$modules_file"
}
trap cleanup EXIT

if ! find "$ROOT" \
  \( -path "$ROOT/.git" -o -path "*/node_modules" -o -path "*/dist" -o -path "*/dist-cloudflare" \) -prune \
  -o -type f -name go.mod -print0 >"$modules_file"; then
  printf 'failed to discover go.mod files under %s\n' "$ROOT" >&2
  exit 1
fi

# CI runs the root CLI in separate normal-test shards. Resolve that exact
# package before any tests; nested modules still keep their complete suites.
root_packages=()
if [[ "$exclude_root_cli" -eq 1 ]]; then
  cli_package="$(cd "$ROOT" && go list ./internal/cli)"
  if [[ -z "$cli_package" || "$cli_package" == *$'\n'* ]]; then
    printf 'expected one root CLI package\n' >&2
    exit 1
  fi
  packages="$(cd "$ROOT" && go list ./...)"
  cli_count=0
  while IFS= read -r package; do
    if [[ "$package" == "$cli_package" ]]; then
      cli_count=$((cli_count + 1))
    elif [[ -n "$package" ]]; then
      root_packages+=("$package")
    fi
  done <<<"$packages"
  if [[ "$cli_count" -ne 1 || ${#root_packages[@]} -eq 0 ]]; then
    printf 'expected root CLI and non-CLI packages\n' >&2
    exit 1
  fi
fi

while IFS= read -r -d '' modfile; do
  dir="$(dirname "$modfile")"
  rel="${dir#"$ROOT"/}"
  if [[ "$dir" == "$ROOT" ]]; then
    rel="."
  fi
  found=1
  test_args=(-timeout=20m ./...)
  if [[ "$exclude_root_cli" -eq 1 && "$dir" == "$ROOT" ]]; then
    test_args=(-count=1 -timeout=20m "${root_packages[@]}")
  fi
  printf '+ (cd %q && go test' "$rel"
  printf ' %q' "${test_args[@]}"
  printf ')\n'
  (cd "$dir" && go test "${test_args[@]}")
done <"$modules_file"

if [[ "$found" -eq 0 ]]; then
  printf 'no go.mod files found under %s\n' "$ROOT" >&2
  exit 1
fi
