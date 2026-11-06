# Playwright recipe proof

Related: https://github.com/openclaw/crabbox/issues/2879

Base: `5c23be026faffd867e11101d37587bfeb28c958b`.
Scope: documentation and a credential-free synthetic browser example. No
provider code, default image, credential configuration, or cloud resources changed.

## Local checks, 2026-10-18

From `examples/cloudflare-playwright`:

- `node prepare.mjs`: passed; lockfile install and browser preparation measured
  separately in generated `artifacts/preparation.json`.
- `node browser.mjs artifacts/local`: passed both contexts.
- `node browser.mjs artifacts/local-repeat`: passed both contexts again.
- Actual runtime: macOS/arm64, Node 24.21.0, Playwright 1.61.1, Chromium
  149.0.7827.55. Browser totals were 626.333 ms and 411.596 ms; these are local
  observations, not Cloudflare measurements or speedup evidence.
- Every context asserted empty cookies/local storage before writing its own
  case marker. Both screenshots were visually inspected: distinct fixture IDs,
  heading, and Saved button. All four PNG sizes/hashes matched result JSON.
- Repeating the same output path failed without changing the existing result.
- A disposable copy with an intentionally wrong heading assertion failed with
  an AssertionError and produced no successful result JSON.

From the repository root:

- `node scripts/build-docs-site.mjs`: passed.
- `node --test scripts/build-docs-site.test.js scripts/check-docs-links.test.js`:
  28 passed, zero failed/skipped.
- `node scripts/check-docs-links.mjs`: 263 Markdown files checked, links passed.
- `git diff --check`: passed.

Generated screenshots, dependencies and run output are untracked. No new
Cloudflare execution was performed for this recipe. The new assertions still
need Linux/Cloudflare execution before claiming this exact recipe is deployed-proven.

## Existing Linux evidence and remaining dependency

https://github.com/openclaw/crabbox/pull/2860#issuecomment-6244850537
records deployed native-artifact proof at
`12f3871a7d867138fad7994192e4163b49c1853a`: Linux/x64 Chromium 149.0.7827.55,
Playwright 1.61.1, fresh exit 0 and reused intentional exit 23, with checked
PNG/trace/JSON/native archive hashes. Fresh command 83.985 seconds; reused
command 3.787 seconds. These are different commands, not a controlled performance comparison.

The original driver's post-stop verifier failed on a retained stopped row.
Independent operator observation and exact-resource cleanup completed that
experiment; it was not fully automated cleanup. Its assertions were different
from this recipe's two-context test.

Immutable sanitized evidence:
https://github.com/saari-co/public-oss-proof-assets/releases/tag/crabbox-pr2860-12f3871a-cloudflare-artifacts-20261018

The recipe's native-download instructions require both CLI and Worker support
from https://github.com/openclaw/crabbox/pull/2860 and explicitly reject treating
those flags as supported on introduction-time main. That independent PR's
native input-safety hold remains a separate unresolved gate. This PR copies no
provider implementation and should remain draft pending the dependency and
appropriate exact-recipe Linux proof. Merge and cloud execution require their
own authorization.
