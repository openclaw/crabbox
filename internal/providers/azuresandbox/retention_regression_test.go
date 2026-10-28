package azuresandbox

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestRetainedObservedResumeReplayDoesNotInterruptActivity(t *testing.T) {
	b, req, _, _, resumes := retainedFixture(t)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	p := retentionRequest(t, b, req.RequestedLeaseID, "setup-pause")
	if err := b.Pause(t.Context(), core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: p}); err != nil {
		t.Fatal(err)
	}
	r := retentionRequest(t, b, req.RequestedLeaseID, "completed-resume")
	if err := b.Resume(t.Context(), core.ResumeRequest{ID: req.RequestedLeaseID, Lifecycle: r}); err != nil {
		t.Fatal(err)
	}
	claim, err := b.claim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		defer close(finished)
		result <- withActivity(ctx, claim, func(ctx context.Context) error {
			close(started)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return nil
			}
		})
	}()
	defer func() { cancel(); <-finished }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("activity did not start")
	}
	replayCtx, replayCancel := context.WithTimeout(t.Context(), time.Second)
	defer replayCancel()
	if err := b.Resume(replayCtx, core.ResumeRequest{ID: req.RequestedLeaseID, Lifecycle: r}); err != nil {
		t.Fatalf("read-only replay waited behind active work: %v", err)
	}
	select {
	case err := <-result:
		t.Fatalf("resume replay interrupted active work: %v", err)
	default:
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("activity did not complete normally: %v", err)
	}
	after, err := b.claim(req.RequestedLeaseID)
	if err != nil || after.Revision != claim.Revision || *resumes != 1 {
		t.Fatal("observed replay changed the claim or repeated resume")
	}
}

func TestRetainedExpiredPreparationCannotCreateDisk(t *testing.T) {
	for _, replay := range []bool{false, true} {
		name := "expires-during-observation"
		if replay {
			name = "expired-prepared-replay"
		}
		t.Run(name, func(t *testing.T) {
			b, req, f, _, _ := retainedFixture(t)
			clock := &retentionClock{at: time.Now()}
			b.rt.Clock = clock
			transport := b.api.http.Transport
			b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/sandboxes") {
					if replay {
						return response(503, ""), nil
					}
					clock.at = clock.at.Add(5 * time.Hour)
				}
				return transport.RoundTrip(r)
			})
			if _, err := b.acquire(t.Context(), req); err == nil {
				t.Fatal("expired or unavailable preparation succeeded")
			}
			if replay {
				clock.at = clock.at.Add(5 * time.Hour)
				b.api.http.Transport = transport
				if _, err := b.acquire(t.Context(), req); err == nil {
					t.Fatal("expired prepared replay succeeded")
				}
			}
			if f.creates != 0 || f.box != nil {
				t.Fatalf("expired window created a retained disk: creates=%d", f.creates)
			}
		})
	}
}

func TestRetainedPauseStillInterruptsActivity(t *testing.T) {
	b, req, _, pauses, _ := retainedFixture(t)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	p := retentionRequest(t, b, req.RequestedLeaseID, "active-pause")
	claim, err := b.claim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	started, finished := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		defer close(finished)
		result <- withActivity(ctx, claim, func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() })
	}()
	defer func() { cancel(); <-finished }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("activity did not start")
	}
	pauseCtx, pauseCancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer pauseCancel()
	if err := b.Pause(pauseCtx, core.PauseRequest{ID: req.RequestedLeaseID, Lifecycle: p}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("pause failed to interrupt activity: %v", err)
	}
	if *pauses != 1 {
		t.Fatal("pause was not submitted exactly once")
	}
}

func TestExpiredAccessGuidanceKeepsRetentionAndEphemeralDistinct(t *testing.T) {
	for _, retained := range []bool{false, true} {
		name := "ephemeral"
		if retained {
			name = "retained"
		}
		t.Run(name, func(t *testing.T) {
			b, req, f, _, _ := retainedFixture(t)
			if !retained {
				b.cfg.AzureSandbox.Retention = ""
				b.cfg.AzureSandbox.OwnerBinding = ""
			}
			clock := &retentionClock{at: time.Now()}
			b.rt.Clock = clock
			transport := b.api.http.Transport
			b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(r)
				if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/sandboxes") {
					clock.at = clock.at.Add(5 * time.Hour)
				}
				return response, err
			})
			_, err := b.acquire(t.Context(), req)
			if err == nil {
				t.Fatal("expired access preparation succeeded")
			}
			if retained {
				if !strings.Contains(err.Error(), "explicit lifecycle recovery required") || strings.Contains(err.Error(), "release the lease") {
					t.Fatalf("retained recovery was replaced by destructive advice: %v", err)
				}
			} else if !strings.Contains(err.Error(), "release the lease") {
				t.Fatalf("ephemeral release advice changed: %v", err)
			}
			if f.creates != 1 || f.deletes != 0 || f.box == nil {
				t.Fatal("diagnostic changed resource custody")
			}
		})
	}
}
