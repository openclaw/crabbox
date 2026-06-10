package cli

import (
	"embed"
	"io/fs"
)

// webVNCAssetsFS holds the vendored noVNC RFB module used to build the
// mode-0600 temporary macOS viewer handoff.
//
//go:embed webvncassets/rfb.js webvncassets/LICENSE.txt
var webVNCAssetsFS embed.FS

// webVNCAssets returns the embedded viewer assets rooted at webvncassets.
func webVNCAssets() fs.FS {
	sub, err := fs.Sub(webVNCAssetsFS, "webvncassets")
	if err != nil {
		// The embed paths are compile-time constants; this cannot fail.
		panic(err)
	}
	return sub
}
