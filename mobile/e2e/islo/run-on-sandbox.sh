#!/usr/bin/env bash
#
# run-on-sandbox.sh — run the Crabbox headless e2e (crabbox-sim) on a leased
# Linux sandbox, the same way the iOS app's "LLM on a sandbox" feature works.
#
# islo.dev is the direct sandbox lifecycle provider. crabbox.sh tokens are for
# portal/workspace flows until coordinator sandbox lifecycle exists. This script
# leases a box directly (`--provider islo`), installs the Swift toolchain, builds
# CrabboxKit, and runs the deterministic headless e2e (17 scenarios / 13
# invariants) — plus, optionally, the tiny-LLM agentic driver against a local
# Ollama on the box.
#
# Prereqs:
#   - the crabbox CLI on PATH
#   - ISLO_API_KEY (or CRABBOX_ISLO_API_KEY) in the environment (never a flag)
#
# Usage:
#   ./e2e/islo/run-on-sandbox.sh                 # deterministic e2e only
#   CRABBOX_E2E_AGENT=1 ./e2e/islo/run-on-sandbox.sh   # + tiny-LLM agent driver
#
set -euo pipefail

: "${ISLO_API_KEY:?set ISLO_API_KEY (or CRABBOX_ISLO_API_KEY) before running}"

IMAGE="${CRABBOX_ISLO_IMAGE:-docker.io/library/ubuntu:26.04}"
MODEL="${CRABBOX_AGENT_MODEL:-qwen2.5:0.5b}"
RUN_AGENT="${CRABBOX_E2E_AGENT:-0}"

crabbox run --provider islo \
  --islo-image "$IMAGE" \
  --islo-workdir crabbox-ios \
  --shell "$(cat <<EOF
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
SUDO=""; [ "\$(id -u)" -ne 0 ] && SUDO="sudo"

# --- Swift toolchain (swiftly) ---
\$SUDO apt-get update -y
\$SUDO apt-get install -y curl ca-certificates libcurl4-openssl-dev clang
if ! command -v swift >/dev/null 2>&1; then
  curl -fsSL https://download.swift.org/swiftly/linux/swiftly-\$(uname -m).tar.gz -o /tmp/swiftly.tar.gz
  tar -xzf /tmp/swiftly.tar.gz -C /tmp
  /tmp/swiftly init --quiet-shell-followup --assume-yes || true
  . "\$HOME/.local/share/swiftly/env.sh" 2>/dev/null || true
fi

cd /workspace/crabbox-ios 2>/dev/null || cd crabbox-ios

# --- deterministic headless e2e (always) ---
swift run crabbox-sim --json

# --- optional: tiny-LLM agentic driver against a local Ollama ---
if [ "${RUN_AGENT}" = "1" ]; then
  curl -fsSL https://ollama.com/install.sh | \$SUDO sh
  (OLLAMA_KEEP_ALIVE=-1 ollama serve >/tmp/ollama.log 2>&1 &) ; sleep 5
  ollama pull ${MODEL}
  CRABBOX_AGENT_MODEL=${MODEL} OLLAMA_HOST=http://127.0.0.1:11434 swift run crabbox-sim --agent --json
fi
EOF
)"
