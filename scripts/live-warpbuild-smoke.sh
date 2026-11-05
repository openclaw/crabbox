#!/usr/bin/env bash
# Shell expressions in remote commands must expand inside the sandbox.
# shellcheck disable=SC2016
set -euo pipefail

# One small sandbox; delete only the lease created by this invocation.
[[ "${CRABBOX_LIVE:-}" == 1 ]] || { echo 'set CRABBOX_LIVE=1' >&2; exit 2; }
[[ -n "${CRABBOX_WARPBUILD_TOKEN:-${WB_TOKEN:-}}" ]] || { echo 'set WB_TOKEN or CRABBOX_WARPBUILD_TOKEN' >&2; exit 2; }
command -v jq >/dev/null || { echo 'jq is required' >&2; exit 2; }
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cb="${CRABBOX_BIN:-$root/bin/crabbox}"
[[ "$cb" == /* && -x "$cb" ]] || { echo 'CRABBOX_BIN must name an absolute executable path' >&2; exit 2; }
scratch="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-warpbuild-smoke.XXXXXX")"
export XDG_STATE_HOME="$scratch/state"
slug="warpbuild-smoke-$(date +%s)-$$"
cleanup_needed=0
cleanup() {
  local status=$?
  trap - EXIT
  if [[ "$cleanup_needed" == 1 ]]; then
    if ! "$cb" stop --provider warpbuild "$slug"; then
      printf 'cleanup unconfirmed; retained state at %s; retry with XDG_STATE_HOME=%q %q stop --provider warpbuild %q\n' "$scratch" "$XDG_STATE_HOME" "$cb" "$slug" >&2
      exit 1
    fi
  fi
  rm -rf -- "$scratch"
  exit "$status"
}
trap cleanup EXIT
mkdir "$scratch/repo"
cd "$scratch/repo"
git init -q
git config maintenance.auto false
printf 'v1\n' >proof.txt
printf 'remove-me\n' >stale.txt
git add proof.txt stale.txt
git -c user.name="Crabbox WarpBuild Smoke" -c user.email=smoke@example.com commit -qm "test: seed workspace"
export CRABBOX_WARPBUILD_SMOKE_VALUE=forwarded-ok
export CRABBOX_SYNC_DELETE=true
"$cb" doctor --provider warpbuild --json >/dev/null
# Mark cleanup before allocation so a failed run still attempts claim recovery.
cleanup_needed=1
"$cb" run --provider warpbuild --warpbuild-runner-label warp-macos-26-arm64-6x \
  --keep --slug "$slug" --allow-env CRABBOX_WARPBUILD_SMOKE_VALUE -- \
  /bin/bash -c 'test "$(cat proof.txt)" = v1 && test -f stale.txt && test "$CRABBOX_WARPBUILD_SMOKE_VALUE" = forwarded-ok && printf "warpbuild stdout\n" && printf "warpbuild stderr\n" >&2'
"$cb" status --provider warpbuild --id "$slug" --wait --json | jq -e '.ready == true' >/dev/null
"$cb" list --provider warpbuild --json | jq -e --arg slug "$slug" 'any(.[]; .labels.slug == $slug)' >/dev/null
printf 'v2\n' >proof.txt
rm stale.txt
git add -u
git -c user.name="Crabbox WarpBuild Smoke" -c user.email=smoke@example.com commit -qm "test: update workspace"
"$cb" run --provider warpbuild --id "$slug" -- /bin/bash -c 'test "$(cat proof.txt)" = v2 && test ! -e stale.txt'
"$cb" pause --provider warpbuild "$slug"
"$cb" resume --provider warpbuild "$slug"
"$cb" run --provider warpbuild --id "$slug" --no-sync -- /bin/bash -c 'test "$(cat proof.txt)" = v2'
exit_status=0
"$cb" run --provider warpbuild --id "$slug" --no-sync -- /bin/bash -c 'exit 23' || exit_status=$?
[[ "$exit_status" == 23 ]] || { echo "expected exit 23, got $exit_status" >&2; exit 1; }
"$cb" stop --provider warpbuild "$slug"
cleanup_needed=0
"$cb" list --provider warpbuild --json | jq -e --arg slug "$slug" 'all(.[]; .labels.slug != $slug)' >/dev/null
echo 'WarpBuild smoke passed: sync, environment, status, reuse, pause/resume, exit status, deletion'
