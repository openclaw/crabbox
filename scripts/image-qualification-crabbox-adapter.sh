#!/usr/bin/env bash
set -euo pipefail

: "${QUALIFICATION_REAL_CRABBOX:?missing real candidate CLI}"
: "${QUALIFICATION_ADAPTER_STATE:?missing adapter state directory}"
mkdir -p "$QUALIFICATION_ADAPTER_STATE"
chmod 700 "$QUALIFICATION_ADAPTER_STATE"

validate_receipt() {
  node -e '
const fs = require("node:fs");
const image = (value) => /^ami-[0-9a-f]+$/.test(value?.id ?? "") &&
  typeof value.revision === "string" && value.revision.length > 0 && value.revision.length <= 128;
const aliasImage = (value) => /^ami-[0-9a-f]+$/.test(value?.id ?? "") &&
  (value.provider === undefined || value.provider === "aws") &&
  typeof value.name === "string" && typeof value.state === "string" &&
  typeof value.promotedAt === "string" && value.promotedAt.length > 0;
try {
  const stat = fs.lstatSync(process.argv[1]);
  if (!stat.isFile() || stat.size > 1048576) throw new Error();
  const receipt = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
  const previous = receipt.previous;
  if (previous?.state !== "absent" && !(previous?.state === "present" &&
      image({ id: previous.imageId, revision: previous.revision }))) throw new Error();
  if (process.argv[2] === "promotion") {
    if (!image(receipt.image) || !Array.isArray(previous.aliases) || previous.aliases.length === 0) throw new Error();
    const seen = new Set();
    for (const entry of previous.aliases) {
      if (!["linux-os", "legacy", "regional"].includes(entry.alias) || seen.has(entry.alias) ||
          (entry.state !== "absent" && !(entry.state === "present" && aliasImage(entry.image)))) throw new Error();
      seen.add(entry.alias);
    }
  } else if (receipt.image !== undefined && !image(receipt.image)) throw new Error();
} catch {
  console.error("qualification receipt is missing or invalid");
  process.exit(1);
}
' "$1" "$2"
}

command_name="${1:-}"
if [[ "$command_name" == warmup ]]; then
  if [[ "${QUALIFICATION_MODE:-mint}" == retained ]]; then
    validate_receipt "$QUALIFICATION_ADAPTER_STATE/promotion-receipt.json" promotion
  fi
  count_file="$QUALIFICATION_ADAPTER_STATE/launch-count"
  count=0
  [[ -f "$count_file" ]] && read -r count <"$count_file"
  count=$((count + 1))
  printf '%s\n' "$count" >"$count_file"
  chmod 600 "$count_file"
fi

status=0
receipt=""
receipt_kind=""
if [[ "$command_name" == image && "${2:-}" == promote ]]; then
  for argument in "$@"; do
    if [[ "$argument" == capture ]]; then
      receipt="$QUALIFICATION_ADAPTER_STATE/promotion-receipt.json"
      receipt_kind=promotion
    elif [[ "$argument" == --retire-expected-catalog || "$argument" == --restore-receipt ]]; then
      receipt="$QUALIFICATION_ADAPTER_STATE/rollback-receipt.json"
      receipt_kind=rollback
    fi
  done
fi
if [[ -n "$receipt" ]]; then
  # Publish only complete, usable receipts. A failed tee must never authorize a
  # lease or leave a partial file masquerading as rollback evidence.
  pending_receipt="$receipt.partial"
  set +e
  "$QUALIFICATION_REAL_CRABBOX" "$@" | tee "$pending_receipt"
  pipeline_status=("${PIPESTATUS[@]}")
  set -e
  status=${pipeline_status[0]}
  [[ "$status" -ne 0 ]] || status=${pipeline_status[1]}
  if [[ "$status" -ne 0 ]]; then
    printf 'qualification receipt capture failed\n' >&2
    exit "$status"
  fi
  chmod 600 "$pending_receipt"
  validate_receipt "$pending_receipt" "$receipt_kind"
  mv "$pending_receipt" "$receipt"
else
  "$QUALIFICATION_REAL_CRABBOX" "$@" || status=$?
fi
[[ "$status" -eq 0 ]] || exit "$status"

count=0
[[ -f "$QUALIFICATION_ADAPTER_STATE/launch-count" ]] &&
  read -r count <"$QUALIFICATION_ADAPTER_STATE/launch-count"
expected_count=3
[[ "${QUALIFICATION_MODE:-mint}" != retained ]] || expected_count=1
full_smoke=0
if [[ "$command_name" == run && "$#" -ge 4 ]]; then
  args=("$@")
  last=$(($# - 1))
  if [[ "${args[$((last - 2))]}" == --shell && "${args[$((last - 1))]}" == -- ]]; then
    payload="${args[$last]%$'\n'}"
    [[ "${payload##*$'\n'}" != 'echo devtools-smoke-ok' ]] || full_smoke=1
  fi
fi
if [[ "$full_smoke" -eq 1 && "$count" -eq "$expected_count" && ! -f "$QUALIFICATION_ADAPTER_STATE/injected" ]]; then
  printf 'after-promoted-smoke\n' >"$QUALIFICATION_ADAPTER_STATE/injected"
  chmod 600 "$QUALIFICATION_ADAPTER_STATE/injected"
  exit 86
fi
