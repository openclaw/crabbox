//go:build smoke

package islo

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveIsloStatusClassification is an end-to-end check against the real Islo
// API (api.islo.dev). It lists live sandboxes through the same SDK path crabbox
// uses (auth token exchange + GET /sandboxes/) and verifies that the aligned
// isloStatusReady / isloStatusTerminal classifiers agree with the statuses Islo
// actually returns today. It is skipped unless ISLO_API_KEY is set, so it never
// runs in CI without credentials.
//
//	go test -tags smoke -run TestLiveIsloStatusClassification -v ./internal/providers/islo
//
// This is the live guard for the M1/M2 alignment: the API reports
// running/paused/failed/etc., never the legacy "ready"/"started"/"active"
// values crabbox previously treated as ready.
func TestLiveIsloStatusClassification(t *testing.T) {
	if testing.Short() {
		t.Skip("live Islo e2e skipped in -short mode")
	}
	apiKey := strings.TrimSpace(os.Getenv("ISLO_API_KEY"))
	if apiKey == "" {
		t.Skip("ISLO_API_KEY not set; skipping live Islo e2e")
	}

	cfg := Config{}
	cfg.Islo.APIKey = apiKey
	cfg.Islo.BaseURL = "https://api.islo.dev"

	rt := Runtime{HTTP: &http.Client{Timeout: 30 * time.Second}}
	client, err := newIsloClient(cfg, rt)
	if err != nil {
		t.Fatalf("new islo client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	sandboxes, err := client.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("live list sandboxes: %v", err)
	}
	if len(sandboxes) == 0 {
		t.Skip("no live sandboxes available to classify")
	}

	hist := map[string]int{}
	for _, s := range sandboxes {
		if s == nil {
			continue
		}
		raw := s.GetStatus()
		st := strings.ToLower(strings.TrimSpace(raw))
		hist[st]++

		ready := isloStatusReady(raw)
		terminal := isloStatusTerminal(raw)

		switch st {
		case "running":
			if !ready || terminal {
				t.Errorf("status %q: ready=%v terminal=%v, want ready & non-terminal", raw, ready, terminal)
			}
		case "failed", "stopped", "stopping", "deleted":
			if ready || !terminal {
				t.Errorf("status %q: ready=%v terminal=%v, want terminal & not ready", raw, ready, terminal)
			}
		case "starting", "paused", "unknown":
			if ready || terminal {
				t.Errorf("status %q: ready=%v terminal=%v, want neither", raw, ready, terminal)
			}
		}

		// Legacy values the old code accepted must never appear on the live API.
		if st == "ready" || st == "started" || st == "active" {
			t.Errorf("live API returned legacy status %q; alignment assumed it is no longer emitted", raw)
		}
	}
	t.Logf("live Islo status histogram: %v", hist)
}
