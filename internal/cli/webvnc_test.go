package cli

import "testing"

func TestWebVNCURLs(t *testing.T) {
	if got := webVNCAgentURL("https://crabbox.openclaw.ai", "cbx_abcdef123456", "wvnc_abc"); got != "wss://crabbox.openclaw.ai/v1/leases/cbx_abcdef123456/webvnc/agent?ticket=wvnc_abc" {
		t.Fatalf("agent URL=%q", got)
	}
	if got := webVNCPortalURL("https://crabbox.openclaw.ai/", "cbx_abcdef123456", "secret value"); got != "https://crabbox.openclaw.ai/portal/leases/cbx_abcdef123456/vnc#password=secret+value" {
		t.Fatalf("portal URL=%q", got)
	}
}
