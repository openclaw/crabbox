# Embedded WebVNC viewer assets

These files are embedded into the `crabbox` binary (`internal/cli/webvnc_assets.go`)
and served by the host-side macOS WebVNC bridge (`crabbox webvnc --provider tart`).

## Provenance

- **`rfb.js`** — the noVNC RFB client, [@novnc/novnc](https://github.com/novnc/noVNC)
  **v1.7.0**, the browser-compatible ESM bundle. It is a verbatim copy of the
  asset the worker portal already vendors at
  `worker/public/portal/assets/novnc/rfb.js` (produced and version-pinned by
  `worker/scripts/vendor-novnc.mjs`). It is upstream-minified, not authored here.
- **`LICENSE.txt`** — noVNC's MPL-2.0 license, copied alongside the bundle.
The host-side bridge builds a mode-0600 temporary HTML viewer around `rfb.js`
for each session. The file contains only the ephemeral bridge token. Legacy VNC
viewers fetch their password from the token-protected loopback bridge; macOS ARD
authentication stays server-side and exposes no credential endpoint to the
browser.

## Updating

1. Bump `@novnc/novnc` and re-run `worker/scripts/vendor-novnc.mjs` (it pins the
   expected version and validates the bundle is browser-compatible ESM).
2. Copy the refreshed `rfb.js` and `LICENSE.txt` here:
   `cp worker/public/portal/assets/novnc/{rfb.js,LICENSE.txt} internal/cli/webvncassets/`
3. Run `go test ./internal/cli -run TestWebVNCAssets` to confirm the embed still loads.

Keeping this in lockstep with the worker's vendored copy ensures one audited,
version-pinned noVNC source rather than a second unmanaged bundle.
