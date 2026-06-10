package railway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestRailwayPublishPeerReturnsDeploymentURL(t *testing.T) {
	api := &fakeRailwayAPI{
		deployment: railwayDeployment{URL: "https://web-cbx-up.railway.app", Status: railwayStatusSuccess},
	}
	backend := newRailwayBackendForTest(api)
	target, err := backend.PublishPeer(context.Background(), "svc-1", 8080, time.Hour)
	if err != nil {
		t.Fatalf("PublishPeer: %v", err)
	}
	if target.URL != "https://web-cbx-up.railway.app" {
		t.Fatalf("expected deployment URL, got %q", target.URL)
	}
	if target.Port != 8080 {
		t.Fatalf("expected port 8080 echoed, got %d", target.Port)
	}
}

func TestRailwayPublishPeerRejectsBadInput(t *testing.T) {
	api := &fakeRailwayAPI{deployment: railwayDeployment{URL: "https://x.railway.app"}}
	backend := newRailwayBackendForTest(api)
	if _, err := backend.PublishPeer(context.Background(), "", 8080, time.Hour); err == nil {
		t.Fatalf("expected rejection of empty lease id")
	}
	if _, err := backend.PublishPeer(context.Background(), "svc-1", 0, time.Hour); err == nil {
		t.Fatalf("expected rejection of port 0")
	}
	if _, err := backend.PublishPeer(context.Background(), "svc-1", 70000, time.Hour); err == nil {
		t.Fatalf("expected rejection of port 70000")
	}
}

func TestRailwayPublishPeerRequiresProjectEnv(t *testing.T) {
	api := &fakeRailwayAPI{deployment: railwayDeployment{URL: "https://x.railway.app"}}
	backend := newRailwayBackendForTest(api)
	backend.cfg.Railway.ProjectID = ""
	if _, err := backend.PublishPeer(context.Background(), "svc-1", 8080, time.Hour); err == nil {
		t.Fatalf("expected error when project id is missing")
	}
}

func TestRailwayPublishPeerSurfacesAPIError(t *testing.T) {
	api := &fakeRailwayAPI{latestErr: errors.New("railway down")}
	backend := newRailwayBackendForTest(api)
	if _, err := backend.PublishPeer(context.Background(), "svc-1", 8080, time.Hour); err == nil {
		t.Fatalf("expected API error to surface")
	}
}

func TestRailwayPublishPeerRejectsNonReadyDeploymentURL(t *testing.T) {
	api := &fakeRailwayAPI{
		deployment: railwayDeployment{URL: "https://stale.railway.app", Status: railwayStatusFailed},
	}
	backend := newRailwayBackendForTest(api)
	_, err := backend.PublishPeer(context.Background(), "svc-1", 8080, time.Hour)
	if err == nil {
		t.Fatal("expected not-ready deployment error")
	}
	if !strings.Contains(err.Error(), "not ready") || !strings.Contains(err.Error(), "FAILED") {
		t.Fatalf("err=%v, want not-ready FAILED error", err)
	}
}

func TestRailwayListPeerTargetsLive(t *testing.T) {
	api := &fakeRailwayAPI{
		deployment: railwayDeployment{URL: "https://live.railway.app", Status: railwayStatusSuccess},
	}
	backend := newRailwayBackendForTest(api)
	targets, err := backend.ListPeerTargets(context.Background(), "svc-1")
	if err != nil {
		t.Fatalf("ListPeerTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].URL != "https://live.railway.app" {
		t.Fatalf("expected one live target, got %#v", targets)
	}
}

func TestRailwayListPeerTargetsIgnoresNonReadyDeploymentURLs(t *testing.T) {
	for _, status := range []railwayDeploymentStatus{
		railwayStatusFailed,
		railwayStatusCrashed,
		railwayStatusRemoved,
		railwayStatusSkipped,
		railwayStatusBuilding,
		railwayStatusDeploying,
	} {
		t.Run(string(status), func(t *testing.T) {
			api := &fakeRailwayAPI{
				deployment: railwayDeployment{URL: "https://stale.railway.app", Status: status},
			}
			backend := newRailwayBackendForTest(api)
			targets, err := backend.ListPeerTargets(context.Background(), "svc-1")
			if err != nil {
				t.Fatalf("ListPeerTargets: %v", err)
			}
			if len(targets) != 0 {
				t.Fatalf("expected no targets for %s deployment, got %#v", status, targets)
			}
		})
	}
}

func TestRailwayListPeerTargetsEmptyWhenNoDeployment(t *testing.T) {
	api := &fakeRailwayAPI{deployment: railwayDeployment{URL: ""}}
	backend := newRailwayBackendForTest(api)
	targets, err := backend.ListPeerTargets(context.Background(), "svc-1")
	if err != nil {
		t.Fatalf("ListPeerTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected no targets for sleeping service, got %#v", targets)
	}
}

// Static check: ensure railwayBackend satisfies core.BridgeProvider.
var _ core.BridgeProvider = (*railwayBackend)(nil)
