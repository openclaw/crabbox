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

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

pass=0
fail=0
unsupported=0

for provider in "${providers[@]}"; do
  out="$tmpdir/$provider.out"
  if "$bin" doctor --provider "$provider" --json >"$out" 2>&1; then
    status="pass"
    pass=$((pass + 1))
  else
    status="fail"
    fail=$((fail + 1))
  fi
  if grep -q 'direct_doctor=unsupported' "$out"; then
    unsupported=$((unsupported + 1))
  fi
  summary="$(tr '\n' ' ' <"$out" | sed 's/[[:space:]][[:space:]]*/ /g' | cut -c 1-220)"
  printf '%-20s %s %s\n' "$provider" "$status" "$summary"
done

echo "summary pass=$pass fail=$fail unsupported=$unsupported"
if [[ "$unsupported" -ne 0 || "$fail" -ne 0 ]]; then
  if [[ "${CRABBOX_DOCTOR_SMOKE_ALLOW_FAILURES:-}" == "1" && "$unsupported" -eq 0 ]]; then
    exit 0
  fi
  exit 1
fi
