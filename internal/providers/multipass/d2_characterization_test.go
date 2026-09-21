package multipass

import (
	"context"
	"io"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestD2CleanupIdleBoundaries(t *testing.T) {
	last := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, last string
		idle       int
		offset     time.Duration
		expired    bool
	}{
		{"before", last.Format(time.RFC3339), 60, -time.Nanosecond, false},
		{"equal", last.Format(time.RFC3339), 60, 0, false},
		{"after", last.Format(time.RFC3339), 60, time.Nanosecond, true},
		{"whitespace", " " + last.Format(time.RFC3339) + "\n", 60, time.Second, true},
		{"invalid", "invalid", 60, time.Second, false},
		{"zero timestamp", "0001-01-01T00:00:00Z", 60, time.Second, false},
		{"disabled", last.Format(time.RFC3339), 0, time.Hour, false},
		{"negative", last.Format(time.RFC3339), -1, time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := last.Add(12*time.Hour + time.Minute + tc.offset)
			claim := core.LeaseClaim{Provider: providerName, LeaseID: "cbx_d2", CloudID: "crabbox-d2", LastUsedAt: tc.last, IdleTimeoutSeconds: tc.idle}
			server := core.Server{CloudID: claim.CloudID, Status: "running", Labels: map[string]string{"lease": claim.LeaseID}}
			got, reason := shouldCleanup(server, claim, true, now)
			wantReason := "claim active"
			if tc.expired {
				wantReason = "claim expired"
			}
			if got != tc.expired || reason != wantReason {
				t.Fatalf("cleanup=(%v,%q), want (%v,%q)", got, reason, tc.expired, wantReason)
			}
		})
	}
}

func TestD2ObservationPrecedence(t *testing.T) {
	b := &backend{}
	cfg := core.BaseConfig()
	for _, running := range []bool{true, false} {
		for _, stored := range []string{"", "ready", "error", "running", " "} {
			native := "running"
			if !running {
				native = "stopped"
			}
			claim := core.LeaseClaim{Provider: providerName, LeaseID: "cbx_d2", CloudImmutableID: "immutable", SSHHost: "192.0.2.1", LastUsedAt: "2026-09-01T00:00:00Z", IdleTimeoutSeconds: 60, Labels: map[string]string{"state": stored, "provider": "stored-provider", "custom": "value"}}
			view := b.serverFromInstance(multipassInstance{Name: "crabbox-d2", State: native, IPv4: []string{"192.0.2.1"}}, claim, cfg)
			wantStatus := "stopped"
			if running {
				wantStatus = "running"
				if stored == "ready" {
					wantStatus = "ready"
				}
			}
			wantLabel := stored
			if stored == "" || !running {
				wantLabel = wantStatus
			}
			if view.Status != wantStatus || view.Labels["state"] != wantLabel {
				t.Fatalf("running=%v stored=%q: status=%q state=%q, want %q/%q", running, stored, view.Status, view.Labels["state"], wantStatus, wantLabel)
			}
			if view.CloudID != "crabbox-d2" || view.Name != view.CloudID || view.Provider != providerName || view.Labels["custom"] != "value" {
				t.Fatalf("view=%#v", view)
			}
			if view.Labels["last_touched_at"] != "1788220800" || view.Labels["idle_timeout"] != "60" || view.Labels["idle_timeout_secs"] != "60" {
				t.Fatalf("claim lifecycle policy was not projected: %v", view.Labels)
			}
			if view.Labels["provider"] != "stored-provider" {
				t.Fatalf("provider label=%q", view.Labels["provider"])
			}
			view.Labels["custom"] = "changed"
			if claim.Labels["custom"] != "value" || claim.Labels["state"] != stored {
				t.Fatal("projection mutated input")
			}
			if claim.Labels["last_touched_at"] != "" || claim.Labels["idle_timeout"] != "" || claim.Labels["idle_timeout_secs"] != "" {
				t.Fatal("projection wrote lifecycle labels into the claim")
			}
		}
	}
}

func TestD2ListDisplayFilterAndOrder(t *testing.T) {
	testutil.IsolateUserDirs(t)
	payload := `{"list":[{"name":"foreign","state":"Running"},{"name":"crabbox-second","state":"Stopped"},{"name":"crabbox-first","state":"Running"}]}`
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{"list": {Stdout: payload}}}
	b := newBackend(Provider{}.Spec(), core.BaseConfig(), core.Runtime{Exec: runner, Stderr: io.Discard, Stdout: io.Discard}).(*backend)
	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].Name != "crabbox-second" || views[1].Name != "crabbox-first" {
		t.Fatalf("views=%#v", views)
	}
	if views[0].Labels["lease"] != "" {
		t.Fatal("display filter fabricated a claim")
	}
}
