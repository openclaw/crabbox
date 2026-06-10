#!/usr/bin/env bash
set -u -o pipefail

bin="${CRABBOX_BIN:-./bin/crabbox}"
default_providers=(
  aws
  azure
  blacksmith-testbox
  cloudflare
  daytona
  e2b
  exe-dev
  gcp
  hetzner
  islo
  modal
  namespace-devbox
  proxmox
  semaphore
  sprites
  ssh
  tensorlake
)

if [[ -n "${CRABBOX_LIVE_DOCTOR_PROVIDERS:-}" ]]; then
  IFS=',' read -r -a providers <<<"${CRABBOX_LIVE_DOCTOR_PROVIDERS}"
else
  providers=("${default_providers[@]}")
fi

selected_providers=()
for provider in "${providers[@]}"; do
  provider="${provider//[[:space:]]/}"
  [[ -n "$provider" ]] || continue
  selected_providers+=("$provider")
done
providers=("${selected_providers[@]}")

if [[ ! -x "$bin" ]]; then
  echo "missing crabbox binary: $bin" >&2
  echo "build first: go build -trimpath -o bin/crabbox ./cmd/crabbox" >&2
  exit 2
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "missing required tool: jq" >&2
  exit 2
fi

if ! tmpdir="$(mktemp -d)"; then
  echo "live doctor smoke could not create temporary directory" >&2
  exit 2
fi
trap 'rm -rf "$tmpdir"' EXIT

pass=0
fail=0
unsupported=0

for provider in "${providers[@]}"; do
  out="$tmpdir/$provider.out"
  err="$tmpdir/$provider.err"
  doctor_status=0
  json_status=0
  "$bin" doctor --provider "$provider" --json >"$out" 2>"$err" || doctor_status=$?
  jq -s -e --arg provider "$provider" 'length == 1 and (.[0] | type == "object" and (.ok | type == "boolean") and .provider == $provider and (.checks | type == "array"))' "$out" >/dev/null || json_status=$?
  if [[ "$doctor_status" -eq 0 && "$json_status" -eq 0 ]]; then
    status="pass"
    pass=$((pass + 1))
  else
    status="fail"
    fail=$((fail + 1))
  fi
  if grep -q 'direct_doctor=unsupported' "$out" "$err"; then
    unsupported=$((unsupported + 1))
  fi
  summary="$({ cat "$out"; cat "$err"; } | tr '\n' ' ' | sed 's/[[:space:]][[:space:]]*/ /g' | cut -c 1-220)"
  printf '%-20s %s %s\n' "$provider" "$status" "$summary"
done

echo "summary pass=$pass fail=$fail unsupported=$unsupported"
if [[ "$unsupported" -ne 0 || "$fail" -ne 0 ]]; then
  if [[ "${CRABBOX_DOCTOR_SMOKE_ALLOW_FAILURES:-}" == "1" && "$unsupported" -eq 0 ]]; then
    exit 0
  fi
  exit 1
fi
