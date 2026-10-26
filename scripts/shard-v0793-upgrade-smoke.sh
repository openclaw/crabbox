#!/usr/bin/env bash
# Prove v0.79.3 on-disk shard settings and lease claims stay intact under the
# current crabbox binary. Optionally builds the v0.79.3 tag binary first so the
# claim/config files are written by that release, then re-reads them with HEAD.
#
# Usage (from repo root):
#   bash scripts/shard-v0793-upgrade-smoke.sh
#
# Optional env:
#   CRABBOX_V0793_UPGRADE_DIR   work directory (default: mktemp)
#   CRABBOX_SKIP_OLD_BUILD=1    skip building v0.79.3; seed files in-process style
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="${CRABBOX_V0793_UPGRADE_DIR:-}"
cleanup_work=0
if [[ -z "$work_dir" ]]; then
  work_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-shard-v0793-XXXXXX")"
  cleanup_work=1
fi
mkdir -p "$work_dir"
trap 'if [[ "$cleanup_work" -eq 1 ]]; then rm -rf "$work_dir"; fi' EXIT

state_home="$work_dir/xdg-state"
config_home="$work_dir/xdg-config"
mkdir -p "$state_home/crabbox/claims" "$config_home"
config_path="$work_dir/crabbox.yaml"
claim_path="$state_home/crabbox/claims/cbx_v0793claim01.json"

# v0.79.3 repo yaml: shard.maxCount only (no maxConcurrency).
cat >"$config_path" <<'EOF'
shard:
  maxCount: 12
EOF

# Minimal claim JSON matching v0.79.3's persisted fields.
cat >"$claim_path" <<EOF
{
  "leaseID": "cbx_v0793claim01",
  "revision": "gen-v0793",
  "slug": "upgrade-claim",
  "provider": "aws",
  "cloudID": "i-v0793",
  "providerScope": "us-east-1",
  "repoRoot": "$work_dir",
  "claimedAt": "2026-01-02T03:04:05Z",
  "lastUsedAt": "2026-01-02T03:04:05Z"
}
EOF

new_bin="$work_dir/crabbox-new"
(
  cd "$repo_root"
  go build -trimpath -o "$new_bin" ./cmd/crabbox
)

if [[ "${CRABBOX_SKIP_OLD_BUILD:-0}" != "1" ]]; then
  old_src="$work_dir/v0.79.3-src"
  if [[ ! -d "$old_src" ]]; then
    git -C "$repo_root" worktree add --detach "$old_src" v0.79.3
  fi
  old_bin="$work_dir/crabbox-v0793"
  (
    cd "$old_src"
    go build -trimpath -o "$old_bin" ./cmd/crabbox
  )
  # Touch the seeded files with the old binary's help path so argv/env match a
  # real v0.79.3 process that opened the same config/state roots.
  env CRABBOX_CONFIG="$config_path" XDG_STATE_HOME="$state_home" XDG_CONFIG_HOME="$config_home" \
    "$old_bin" --help >/dev/null
fi

before_config="$(cksum "$config_path" | awk '{print $1" "$2}')"
before_claim="$(cksum "$claim_path" | awk '{print $1" "$2}')"

env CRABBOX_CONFIG="$config_path" XDG_STATE_HOME="$state_home" XDG_CONFIG_HOME="$config_home" \
  "$new_bin" shard --help >/dev/null

after_config="$(cksum "$config_path" | awk '{print $1" "$2}')"
after_claim="$(cksum "$claim_path" | awk '{print $1" "$2}')"

if [[ "$before_config" != "$after_config" ]]; then
  printf 'v0.79.3 config changed under new shard path: before=%s after=%s\n' "$before_config" "$after_config" >&2
  exit 1
fi
if [[ "$before_claim" != "$after_claim" ]]; then
  printf 'v0.79.3 claim changed under new shard path: before=%s after=%s\n' "$before_claim" "$after_claim" >&2
  exit 1
fi

python3 - "$config_path" "$claim_path" <<'PY'
import json, sys, pathlib
config = pathlib.Path(sys.argv[1]).read_text()
claim = json.loads(pathlib.Path(sys.argv[2]).read_text())
assert "maxCount: 12" in config and "maxConcurrency" not in config, config
assert claim["leaseID"] == "cbx_v0793claim01" and claim["revision"] == "gen-v0793", claim
print("shard-v0793-upgrade-smoke: settings and claim intact")
PY
