package cli

import (
	"io/fs"
	"net/http/httptest"
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

func TestMacOSWebVNCTokenAllowed(t *testing.T) {
	req := httptest.NewRequest("GET", "/websockify?token=deadbeef", nil)
	if !macOSWebVNCTokenAllowed(req, "deadbeef") {
		t.Fatal("matching token should be accepted")
	}
	for _, path := range []string{"/websockify", "/websockify?token=wrong"} {
		req := httptest.NewRequest("GET", path, nil)
		if macOSWebVNCTokenAllowed(req, "deadbeef") {
			t.Fatalf("path %q should be rejected", path)
		}
	}
}

func TestAvailableLocalVNCPortExcept(t *testing.T) {
	// The tunnel port must never equal the (possibly user-supplied) web port.
	webPort := availableLocalVNCPort()
	for i := 0; i < 20; i++ {
		if got := availableLocalVNCPortExcept(webPort); got == webPort {
			t.Fatalf("availableLocalVNCPortExcept(%q) returned the excluded port", webPort)
		}
	}
	// Excluding the fallback (5901) must still yield a different fallback.
	if got := availableLocalVNCPortExcept("5901"); got == "5901" {
		t.Errorf("availableLocalVNCPortExcept(5901) = 5901, want a different port")
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
	if !strings.Contains(string(html), "rfb.js") || !strings.Contains(string(html), "/websockify?token=") {
		t.Error("vnc.html should import rfb.js and connect to token-gated /websockify")
	}
}
