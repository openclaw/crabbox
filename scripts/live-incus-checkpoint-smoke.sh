#!/usr/bin/env bash
set -euo pipefail

# Dedicated, opt-in rig only. Shared connection settings come from Crabbox config/env.
if [[ "${CRABBOX_LIVE:-}" != 1 ]]; then
  echo 'set CRABBOX_LIVE=1 to run the Incus checkpoint smoke' >&2
  exit 2
fi
if [[ "$#" != 0 ]]; then
  echo 'configure the Incus connection and container type through CRABBOX_CONFIG or existing environment settings' >&2
  exit 2
fi
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cb="${CRABBOX_BIN:-$root/bin/crabbox}"
repo="${CRABBOX_LIVE_REPO:-$PWD}"
for tool in python3 jq ssh openssl rg; do command -v "$tool" >/dev/null; done
[[ "$cb" == /* ]] || cb="$PWD/$cb"
cd "$repo"
rig_args=(--provider incus)
proof_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-incus-proof.XXXXXX")"
chmod 700 "$proof_dir"
source_id="cbx_$(openssl rand -hex 6)"
fork_id="cbx_$(openssl rand -hex 6)"
checkpoint_name="incus-proof-$source_id"
checkpoint_id=""
source_active=0
fork_active=0

cb_run() {
  python3 - "$cb" "$@" <<'PY'
import subprocess, sys
try:
    result = subprocess.run(sys.argv[1:], timeout=1200)
    sys.exit(result.returncode)
except subprocess.TimeoutExpired:
    print('Crabbox smoke command exceeded 20 minutes; reconcile tracked resources', file=sys.stderr)
    sys.exit(124)
PY
}
cleanup() {
  local outcome=$? cleanup_failed=0
  trap - EXIT
  if [[ "$fork_active" == 1 ]]; then
    cb_run stop "${rig_args[@]}" --incus-delete-on-release=true "$fork_id" || cleanup_failed=1
  fi
  if [[ "$source_active" == 1 ]]; then
    cb_run stop "${rig_args[@]}" --incus-delete-on-release=true "$source_id" || cleanup_failed=1
  fi
  if [[ -z "$checkpoint_id" ]]; then
    checkpoint_id="$(cb_run checkpoint list --json | jq -r --arg name "$checkpoint_name" '.[]? | select(.name == $name) | .id')" || cleanup_failed=1
  fi
  if [[ -n "$checkpoint_id" ]]; then
    cb_run checkpoint delete "$checkpoint_id" || cleanup_failed=1
  fi
  rm -f "$proof_dir/source-key"
  if [[ "$cleanup_failed" != 0 ]]; then
    printf 'cleanup uncertain: source=%s fork=%s checkpoint=%s proof=%s\n' "$source_id" "$fork_id" "${checkpoint_id:-$checkpoint_name}" "$proof_dir" >&2
    exit 1
  fi
  exit "$outcome"
}
trap cleanup EXIT
cb_run doctor "${rig_args[@]}" --json
source_active=1
cb_run warmup "${rig_args[@]}" --lease-id "$source_id" --incus-delete-on-release=true
cb_run inspect "${rig_args[@]}" --id "$source_id" --json > "$proof_dir/source.json"
cb_run warmup "${rig_args[@]}" --lease-id "$source_id" --incus-delete-on-release=true
cb_run inspect "${rig_args[@]}" --id "$source_id" --json > "$proof_dir/replay.json"
jq -e -s '.[0].serverId == .[1].serverId and .[0].sshKey == .[1].sshKey' "$proof_dir/source.json" "$proof_dir/replay.json" >/dev/null
source_key="$(jq -er '.sshKey' "$proof_dir/source.json")"
cp "$source_key" "$proof_dir/source-key"
chmod 600 "$proof_dir/source-key"
cb_run run "${rig_args[@]}" --id "$source_id" --no-hydrate -- printf 'sync-before-checkpoint-ok\n'

cb_run run "${rig_args[@]}" --id "$source_id" --no-sync --shell \
  'sudo sh -c '\''printf "#!/bin/sh\necho dependency-retained\n" > /usr/local/bin/crabbox-checkpoint-fixture; chmod 755 /usr/local/bin/crabbox-checkpoint-fixture'\''; printf disk-retained > checkpoint-proof.txt'
cb_run run "${rig_args[@]}" --id "$source_id" --no-sync --capture-stdout "$proof_dir/source-identity.txt" --shell \
  'cat /etc/machine-id; cat /etc/ssh/ssh_host_ed25519_key.pub'
cb_run checkpoint create "${rig_args[@]}" --id "$source_id" --mode native --name "$checkpoint_name" --json > "$proof_dir/checkpoint.json"
checkpoint_id="$(jq -er '.id' "$proof_dir/checkpoint.json")"
cb_run run "${rig_args[@]}" --id "$source_id" --no-sync --capture-stdout "$proof_dir/source-after-capture.txt" --shell \
  'cat /etc/machine-id; cat /etc/ssh/ssh_host_ed25519_key.pub'
cmp "$proof_dir/source-identity.txt" "$proof_dir/source-after-capture.txt"

cb_run stop "${rig_args[@]}" --incus-delete-on-release=true "$source_id"
source_active=0
cb_run checkpoint inspect "$checkpoint_id" --verify --json | jq -e '.providerState == "available"' >/dev/null
fork_active=1
cb_run checkpoint fork "$checkpoint_id" "${rig_args[@]}" --lease-id "$fork_id"
cb_run inspect "${rig_args[@]}" --id "$fork_id" --json > "$proof_dir/fork.json"
cb_run checkpoint fork "$checkpoint_id" "${rig_args[@]}" --lease-id "$fork_id"
cb_run run "${rig_args[@]}" --id "$fork_id" --no-sync --shell \
  'test "$(cat checkpoint-proof.txt)" = disk-retained; test "$(crabbox-checkpoint-fixture)" = dependency-retained'
cb_run run "${rig_args[@]}" --id "$fork_id" --no-sync --capture-stdout "$proof_dir/fork-identity.txt" --shell \
  'cat /etc/machine-id; cat /etc/ssh/ssh_host_ed25519_key.pub'
python3 - "$proof_dir/source-identity.txt" "$proof_dir/fork-identity.txt" <<'PY'
import pathlib, sys
before, after = [pathlib.Path(p).read_text().splitlines() for p in sys.argv[1:]]
assert len(before) == len(after) == 2, 'identity evidence must contain machine ID and SSH host public key'
assert before[0] != after[0] and before[1] != after[1], 'fork reused source machine or host identity'
PY
fork_host="$(jq -er '.sshHost' "$proof_dir/fork.json")"
fork_port="$(jq -er '.sshPort' "$proof_dir/fork.json")"
fork_user="$(jq -er '.sshUser' "$proof_dir/fork.json")"
fork_key="$(jq -er '.sshKey' "$proof_dir/fork.json")"
# Readiness pins this lease's host in the key directory; neither probe may bypass it.
fork_known_hosts="$(dirname "$fork_key")/known_hosts"
[[ -s "$fork_known_hosts" ]]
# A positive probe first distinguishes key rejection from a broken SSH endpoint.
ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityAgent=none \
  -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=\"$fork_known_hosts\"" \
  -o GlobalKnownHostsFile=none -o KnownHostsCommand=none -o VerifyHostKeyDNS=no \
  -i "$fork_key" -p "$fork_port" "$fork_user@$fork_host" true
if ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityAgent=none \
  -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=\"$fork_known_hosts\"" \
  -o GlobalKnownHostsFile=none -o KnownHostsCommand=none -o VerifyHostKeyDNS=no \
  -i "$proof_dir/source-key" -p "$fork_port" "$fork_user@$fork_host" true 2> "$proof_dir/rejected-key.txt"; then
  echo 'source private key authenticated to fork' >&2
  exit 1
fi
rg -q 'Permission denied' "$proof_dir/rejected-key.txt"
cb_run checkpoint delete "$checkpoint_id"
checkpoint_id=""
cb_run run "${rig_args[@]}" --id "$fork_id" --no-sync -- crabbox-checkpoint-fixture
cb_run stop "${rig_args[@]}" --incus-delete-on-release=true "$fork_id"
fork_active=0
cb_run list "${rig_args[@]}" --json > "$proof_dir/remaining.json"
jq -e --arg source "$source_id" --arg fork "$fork_id" '((type == "array") or . == null) and all(.[]?; .labels.lease != $source and .labels.lease != $fork)' "$proof_dir/remaining.json" >/dev/null
printf 'Incus checkpoint lifecycle passed; evidence=%s\n' "$proof_dir"
