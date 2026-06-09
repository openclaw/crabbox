package cli

import (
	"io/fs"
	"strings"
	"testing"
)

func TestMacOSWebVNCViewerURL(t *testing.T) {
	got := macOSWebVNCViewerURL("6080", "deadbeefcafef00d")
	if got != "http://127.0.0.1:6080/vnc.html#token=deadbeefcafef00d" {
		t.Errorf("unexpected viewer URL: %s", got)
	}
	// The account password must never appear in the URL (it's fetched from the
	// token-gated /credentials endpoint instead).
	for _, banned := range []string{"password", "admin", "?"} {
		if strings.Contains(got, banned) {
			t.Errorf("viewer URL must not contain %q: %s", banned, got)
		}
	}
}

func TestRandomTokenUnique(t *testing.T) {
	a, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := randomToken()
	if a == "" || a == b || len(a) != 32 {
		t.Fatalf("tokens should be unique 16-byte hex: %q %q", a, b)
	}
}

func TestVNCViewerPasswordDefaultsToAdmin(t *testing.T) {
	if p := vncViewerPassword(Config{}); p != "admin" {
		t.Errorf("default viewer password = %q, want admin (cirruslabs default)", p)
	}
	cfg := Config{}
	cfg.Tart.Password = "custom"
	if p := vncViewerPassword(cfg); p != "custom" {
		t.Errorf("override viewer password = %q, want custom", p)
	}
}

func TestWebVNCAssetsEmbedded(t *testing.T) {
	assets := webVNCAssets()
	for _, name := range []string{"vnc.html", "rfb.js"} {
		b, err := fs.ReadFile(assets, name)
		if err != nil {
			t.Fatalf("embedded asset %s missing: %v", name, err)
		}
		if len(b) == 0 {
			t.Fatalf("embedded asset %s is empty", name)
		}
	}
	// The viewer must import the vendored RFB module.
	html, _ := fs.ReadFile(assets, "vnc.html")
	if !strings.Contains(string(html), "rfb.js") || !strings.Contains(string(html), "/websockify") {
		t.Error("vnc.html should import rfb.js and connect to /websockify")
	}
}
