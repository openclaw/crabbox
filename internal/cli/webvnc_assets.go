package cli

import (
	"embed"
	"io/fs"
)

// webVNCAssetsFS holds the vendored noVNC viewer (rfb.js) plus a minimal
// vnc.html, served by the host-side macOS WebVNC bridge so a browser can view a
// tart lease's Screen Sharing without any noVNC/websockify tooling on the guest.
//
//go:embed webvncassets/rfb.js webvncassets/vnc.html webvncassets/LICENSE.txt
var webVNCAssetsFS embed.FS

// webVNCAssets returns the embedded viewer assets rooted so that "vnc.html" and
// "rfb.js" are served at the HTTP root.
func webVNCAssets() fs.FS {
	sub, err := fs.Sub(webVNCAssetsFS, "webvncassets")
	if err != nil {
		// The embed paths are compile-time constants; this cannot fail.
		panic(err)
	}
	return sub
}
