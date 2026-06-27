#!/usr/bin/env bash
#
# verify-islo-key.sh — prove your islo API key works end-to-end, the same way
# the app's Sandboxes tab does: provision a sandbox, (optionally) run an LLM on
# it, chat, and clean up. The key is read from a file (never argv, never echoed).
#
# Put your key in a private file first (do this in a terminal, NOT pasted in a
# chat, so it never lands in any transcript):
#
#   printf %s 'ak_your_islo_key_here' > ~/.crabbox_islo_key
#   chmod 600 ~/.crabbox_islo_key
#
# Then:
#   ./scripts/verify-islo-key.sh          # auth + create/list/delete a sandbox
#   ./scripts/verify-islo-key.sh --llm    # + install Ollama, pull a model, chat
#
set -euo pipefail
cd "$(dirname "$0")/.."

KEYFILE="${ISLO_KEY_FILE:-$HOME/.crabbox_islo_key}"
if [ ! -f "$KEYFILE" ]; then
  echo "No key file at $KEYFILE. Create it with:"
  echo "  printf %s 'ak_your_key' > $KEYFILE && chmod 600 $KEYFILE"
  exit 1
fi

export ISLO_API_KEY
ISLO_API_KEY="$(tr -d '[:space:]' < "$KEYFILE")"

if [ "${1:-}" = "--llm" ]; then
  export CRABBOX_ISLO_LLM=1
  export CRABBOX_AGENT_MODEL="${CRABBOX_AGENT_MODEL:-qwen2.5:0.5b}"
fi

echo "==> Running the live islo e2e (key read from $KEYFILE)"
swift run crabbox-sim --islo-demo
