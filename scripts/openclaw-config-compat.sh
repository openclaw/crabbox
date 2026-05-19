#!/usr/bin/env bash
# Verify that an OpenClaw config snapshot known-good on version X still loads
# on version Y, and surface whether `openclaw doctor` is required to migrate
# it.
#
# Phases (all run on the same leased Linux box):
#   1. Install OpenClaw X, stage the fixture into the state dir, run the
#      smoke command. Must succeed — this is the load-bearing assumption that
#      the fixture is valid for its declared baseline version. Failure exits
#      2; nothing has been learned about Y.
#   2. Upgrade to OpenClaw Y via `npm i -g openclaw@<y>`. Re-run the smoke
#      command WITHOUT invoking doctor. Snapshot the state-dir hash.
#   3. Run `openclaw doctor --non-interactive --yes`. Diff the state-dir hash
#      to detect mutations; capture doctor output.
#   4. Re-run the smoke command (post-doctor). Record pass/fail.
#
# Exit codes:
#   0  Clean upgrade — phase 2 passed. Doctor output is informational.
#   3  Migration required — phase 2 failed, phase 4 passed (doctor fixed it).
#   4  Regression — phase 2 AND phase 4 failed (doctor cannot recover).
#   2  Bad fixture, infra error, or unsatisfied precondition.
#
# Required:
#   CRABBOX_LIVE=1                 acknowledge that this leases a real box
#   OPENCLAW_VERSION_X=<semver>    baseline version the fixture was authored for
#   OPENCLAW_VERSION_Y=<semver>    target version under test (use "latest" for HEAD)
#
# Optional:
#   OPENCLAW_FIXTURE=<path>        config snapshot to copy to the box; default
#                                  scripts/fixtures/openclaw/$OPENCLAW_VERSION_X/openclaw.json
#   OPENCLAW_SMOKE_CMD=<cmd>       remote command that returns 0 iff the
#                                  config loads cleanly; default
#                                  `openclaw doctor --lint --json` (non-mutating)
#   OPENCLAW_STATE_DIR=<remote>    state dir on the box; default $HOME/.openclaw
#   OPENCLAW_COMPAT_ID=<lease>     reuse an existing lease instead of warmup
#   OPENCLAW_COMPAT_PROVIDER=<p>   crabbox provider; default aws
#   OPENCLAW_COMPAT_CLASS=<class>  crabbox machine class; default fast
#   OPENCLAW_COMPAT_IDLE=<dur>     lease idle timeout; default 60m
#   OPENCLAW_COMPAT_STOP=1|0       stop the lease on exit; default 1
#   CRABBOX_BIN=<path>             crabbox binary; default $repo/bin/crabbox

set -euo pipefail

if [[ "${CRABBOX_LIVE:-}" != "1" ]]; then
  echo "set CRABBOX_LIVE=1 to lease a real box" >&2
  exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cb="${CRABBOX_BIN:-$root/bin/crabbox}"

vx="${OPENCLAW_VERSION_X:?set OPENCLAW_VERSION_X=<baseline semver>}"
vy="${OPENCLAW_VERSION_Y:?set OPENCLAW_VERSION_Y=<target semver or latest>}"
fixture="${OPENCLAW_FIXTURE:-$root/scripts/fixtures/openclaw/${vx}/openclaw.json}"
smoke="${OPENCLAW_SMOKE_CMD:-openclaw doctor --lint --json}"
state_dir="${OPENCLAW_STATE_DIR:-\$HOME/.openclaw}"
lease="${OPENCLAW_COMPAT_ID:-}"
provider="${OPENCLAW_COMPAT_PROVIDER:-aws}"
class="${OPENCLAW_COMPAT_CLASS:-fast}"
idle="${OPENCLAW_COMPAT_IDLE:-60m}"
stop_after="${OPENCLAW_COMPAT_STOP:-1}"

if [[ ! -f "$fixture" ]]; then
  echo "missing fixture: $fixture" >&2
  echo "set OPENCLAW_FIXTURE=<path> or place a snapshot at the default location" >&2
  exit 2
fi

if [[ ! -x "$cb" ]]; then
  echo "crabbox binary not found at $cb; build it or set CRABBOX_BIN" >&2
  exit 2
fi

on_box() { "$cb" run --id "$lease" --shell -- "$1"; }
on_box_ok() { "$cb" run --id "$lease" --shell -- "$1" >/dev/null 2>&1; }
on_box_capture() { "$cb" run --id "$lease" --shell -- "$1" 2>&1; }

extract_lease() {
  sed -n 's/.*\(cbx_[a-f0-9]\{12\}\).*/\1/p' | head -1
}

if [[ -z "$lease" ]]; then
  echo "==> warming up $provider/$class lease"
  if ! out="$("$cb" warmup \
    --provider "$provider" \
    --class "$class" \
    --idle-timeout "$idle" \
    --timing-json 2>&1)"; then
    printf '%s\n' "$out" >&2
    echo "warmup failed" >&2
    exit 2
  fi
  printf '%s\n' "$out"
  lease="$(printf '%s\n' "$out" | extract_lease)"
fi

if [[ -z "$lease" ]]; then
  echo "could not resolve lease id" >&2
  exit 2
fi

cleanup() {
  if [[ "$stop_after" == "1" ]]; then
    "$cb" stop "$lease" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo "==> lease=$lease  X=$vx  Y=$vy"
echo "    fixture=$fixture"
echo "    smoke=$smoke"
echo "    state_dir=$state_dir"

stage_fixture() {
  local remote_path="$state_dir/openclaw.json"
  # Upload fixture as base64 over stdin to avoid quoting hell with secrets/JSON.
  local b64
  b64="$(base64 < "$fixture" | tr -d '\n')"
  on_box "
    set -e
    mkdir -p $state_dir
    printf '%s' '$b64' | base64 -d > $remote_path
    chmod 600 $remote_path
  "
}

state_hash() {
  on_box_capture "
    if [ ! -d $state_dir ]; then echo MISSING; exit 0; fi
    find $state_dir -type f -print0 \
      | sort -z \
      | xargs -0 sha256sum 2>/dev/null \
      | sha256sum \
      | awk '{print \$1}'
  " | tail -n1
}

# Phase 1: install vX, stage fixture, smoke must pass.
echo
echo "==> phase 1: install openclaw@${vx} + smoke (must pass)"
if ! on_box "npm i -g openclaw@${vx}"; then
  echo "FAIL[phase1]: could not install openclaw@${vx}" >&2
  exit 2
fi
stage_fixture
if ! on_box_ok "$smoke"; then
  echo "FAIL[phase1]: fixture rejected by its own baseline ${vx}." >&2
  echo "  The fixture is not actually valid for X — fix the fixture, not openclaw." >&2
  echo "  smoke output:" >&2
  on_box_capture "$smoke" >&2 || true
  exit 2
fi
echo "    phase 1: pass"

# Phase 2: upgrade to vY, no doctor, re-smoke.
echo
echo "==> phase 2: upgrade to openclaw@${vy}, smoke WITHOUT doctor"
if ! on_box "npm i -g openclaw@${vy}"; then
  echo "FAIL[phase2]: could not install openclaw@${vy}" >&2
  exit 2
fi
hash_before="$(state_hash)"
echo "    state-dir hash (pre-doctor): $hash_before"
if on_box_ok "$smoke"; then
  phase2=pass
  echo "    phase 2: pass (no migration needed)"
else
  phase2=fail
  echo "    phase 2: fail (config does not load on ${vy} without doctor)"
  echo "    --- smoke output ---"
  on_box_capture "$smoke" || true
  echo "    --------------------"
fi

# Phase 3: doctor.
echo
echo "==> phase 3: openclaw doctor --non-interactive --yes"
doctor_out="$(on_box_capture "openclaw doctor --non-interactive --yes" || true)"
hash_after="$(state_hash)"
if [[ "$hash_before" == "$hash_after" ]]; then
  doctor_changed=no
  echo "    state-dir hash unchanged ($hash_after) — doctor made no mutations"
else
  doctor_changed=yes
  echo "    state-dir MUTATED: $hash_before -> $hash_after"
fi
echo "    --- doctor output ---"
printf '%s\n' "$doctor_out"
echo "    ---------------------"

# Phase 4: smoke after doctor.
echo
echo "==> phase 4: smoke AFTER doctor"
if on_box_ok "$smoke"; then
  phase4=pass
  echo "    phase 4: pass"
else
  phase4=fail
  echo "    phase 4: fail"
  echo "    --- smoke output ---"
  on_box_capture "$smoke" || true
  echo "    --------------------"
fi

# Decision.
echo
echo "==> summary"
echo "    X=$vx -> Y=$vy"
echo "    phase 2 (Y, no doctor): $phase2"
echo "    doctor changed state  : $doctor_changed"
echo "    phase 4 (Y, +doctor)  : $phase4"

if [[ "$phase2" == "pass" ]]; then
  echo "RESULT: clean upgrade"
  exit 0
fi
if [[ "$phase4" == "pass" ]]; then
  echo "RESULT: migration required — doctor fixes ${vx} -> ${vy} on its own"
  exit 3
fi
echo "RESULT: REGRESSION — config valid on ${vx} fails on ${vy}, doctor cannot recover" >&2
exit 4
