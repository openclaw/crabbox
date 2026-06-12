//go:build smoke

package islo

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"regexp"
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

// TestLiveIsloPauseResumeLifecycle creates a real sandbox and proves the
// provider lifecycle end to end. It is separately opt-in because it mutates
// paid provider state.
//
//	CRABBOX_LIVE_ISLO_PAUSE_RESUME=1 ISLO_API_KEY=... \
//	  go test -tags smoke -run TestLiveIsloPauseResumeLifecycle -v ./internal/providers/islo
func TestLiveIsloPauseResumeLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("live Islo pause/resume skipped in -short mode")
	}
	if os.Getenv("CRABBOX_LIVE_ISLO_PAUSE_RESUME") != "1" {
		t.Skip("CRABBOX_LIVE_ISLO_PAUSE_RESUME=1 not set")
	}
	apiKey := strings.TrimSpace(os.Getenv("ISLO_API_KEY"))
	if apiKey == "" {
		t.Skip("ISLO_API_KEY not set; skipping live Islo pause/resume")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	cfg := Config{}
	cfg.Islo.APIKey = apiKey
	cfg.Islo.BaseURL = blank(strings.TrimSpace(os.Getenv("ISLO_BASE_URL")), "https://api.islo.dev")
	cfg.Islo.Image = blank(strings.TrimSpace(os.Getenv("CRABBOX_LIVE_ISLO_IMAGE")), "docker.io/library/ubuntu:26.04")
	cfg.Islo.Workdir = "crabbox-live"
	cfg.Islo.VCPUs = 1
	cfg.Islo.MemoryMB = 1024
	cfg.Islo.DiskGB = 10

	var stdout, stderr bytes.Buffer
	backend := NewIsloBackend(Provider{}.Spec(), cfg, Runtime{
		HTTP:   &http.Client{Timeout: 30 * time.Second},
		Stdout: &stdout,
		Stderr: &stderr,
	}).(*isloBackend)
	repo := Repo{Name: "pause-resume-live", Root: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := backend.Warmup(ctx, WarmupRequest{Repo: repo, Keep: true}); err != nil {
		t.Fatalf("warmup: %v\n%s", err, stderr.String())
	}
	match := regexp.MustCompile(`(?m)^leased ([^ ]+) `).FindStringSubmatch(stdout.String())
	if len(match) != 2 {
		t.Fatalf("warmup lease not found in output: %q", stdout.String())
	}
	leaseID := match[1]
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := backend.Stop(cleanupCtx, StopRequest{ID: leaseID}); err != nil {
			t.Logf("cleanup stop failed: %v", err)
		}
	}()

	if err := backend.Pause(ctx, PauseRequest{ID: leaseID}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	waitForLiveIsloState(t, ctx, backend, leaseID, "paused")
	if _, ok, err := resolveLeaseClaim(leaseID); err != nil || !ok {
		t.Fatalf("claim after pause ok=%v err=%v", ok, err)
	}

	if err := backend.Resume(ctx, ResumeRequest{ID: leaseID}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	status, err := backend.Status(ctx, StatusRequest{ID: leaseID, Wait: true, WaitTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatalf("status after resume: %v", err)
	}
	if !status.Ready || status.State != "running" {
		t.Fatalf("status after resume=%#v", status)
	}

	stdout.Reset()
	result, err := backend.Run(ctx, RunRequest{
		Repo:    repo,
		ID:      leaseID,
		Keep:    true,
		NoSync:  true,
		Command: []string{"printf", "crabbox-islo-resume-ok"},
	})
	if err != nil {
		t.Fatalf("run after resume: %v\n%s", err, stderr.String())
	}
	if result.ExitCode != 0 || !strings.Contains(stdout.String(), "crabbox-islo-resume-ok") {
		t.Fatalf("run result=%#v stdout=%q", result, stdout.String())
	}
}

func waitForLiveIsloState(t *testing.T, ctx context.Context, backend *isloBackend, leaseID, want string) {
	t.Helper()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		status, err := backend.Status(ctx, StatusRequest{ID: leaseID})
		if err != nil {
			t.Fatalf("status waiting for %s: %v", want, err)
		}
		if status.State == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for state=%s: %v", want, ctx.Err())
		case <-ticker.C:
		}
	}
}
