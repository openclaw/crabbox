#!/usr/bin/env bash
set -euo pipefail

node scripts/check-command-docs.mjs
node scripts/check-provider-matrix.mjs
node scripts/check-docs-links.mjs
node scripts/check-zed-extension.mjs
node scripts/test-zed-extension-e2e.mjs
node scripts/build-docs-site.mjs
node --test scripts/build-docs-site.test.js scripts/enhance-docs-site.test.mjs
node scripts/enhance-docs-site.mjs
node scripts/normalize-provider-counts.mjs
