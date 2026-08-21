#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec node "$ROOT/scripts/scrub-aws-image.mjs" \
  --target linux \
  --root "${CRABBOX_SCRUB_ROOT:-/}" \
  --report "${CRABBOX_SCRUB_REPORT:-/var/lib/crabbox/image-scrub-report.json}" \
  --require-root
