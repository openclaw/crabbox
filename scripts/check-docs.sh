#!/usr/bin/env bash
set -euo pipefail

node scripts/check-command-docs.mjs
node scripts/check-provider-matrix.mjs
node scripts/check-docs-links.mjs
node scripts/build-docs-site.mjs
