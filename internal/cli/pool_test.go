package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestShouldCleanupServerSkipsRunningAndProvisioningStates(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, state := range []string{"running", "provisioning"} {
		server := Server{Labels: map[string]string{
			"keep":       "false",
			"state":      state,
			"expires_at": now.Add(-time.Hour).Format(time.RFC3339),
		}}
		if ok, reason := shouldCleanupServer(server, now); ok {
			t.Fatalf("shouldCleanupServer state=%s=%v, %s; want skip", state, ok, reason)
		}
	}
}

func TestShouldCleanupServerDeletesExpiredIdleStates(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, state := range []string{"leased", "ready", "active"} {
		server := Server{Labels: map[string]string{
			"keep":       "false",
			"state":      state,
			"expires_at": now.Add(-time.Minute).Format(time.RFC3339),
		}}
		if ok, reason := shouldCleanupServer(server, now); !ok {
			t.Fatalf("shouldCleanupServer state=%s=%v, %s; want delete", state, ok, reason)
		}
	}
}

func TestShouldCleanupServerDeletesStaleRunningStates(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	server := Server{Labels: map[string]string{
		"keep":       "false",
		"state":      "running",
		"expires_at": now.Add(-13 * time.Hour).Format(time.RFC3339),
	}}
	if ok, reason := shouldCleanupServer(server, now); !ok {
		t.Fatalf("shouldCleanupServer=%v, %s; want delete", ok, reason)
	}
}

func TestShouldCleanupServerDeletesExpiredInactive(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	server := Server{Labels: map[string]string{
		"keep":       "false",
		"expires_at": now.Add(-time.Minute).Format(time.RFC3339),
	}}
	if ok, reason := shouldCleanupServer(server, now); !ok {
		t.Fatalf("shouldCleanupServer=%v, %s; want delete", ok, reason)
	}
}

func TestShouldCleanupServerKeepsUnexpiredAndKept(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	tests := []Server{
		{Labels: map[string]string{"keep": "true", "expires_at": now.Add(-time.Hour).Format(time.RFC3339)}},
		{Labels: map[string]string{"keep": "false", "expires_at": now.Add(time.Hour).Format(time.RFC3339)}},
		{Labels: map[string]string{"keep": "false"}},
	}
	for _, server := range tests {
		if ok, reason := shouldCleanupServer(server, now); ok {
			t.Fatalf("shouldCleanupServer=%v, %s; want skip", ok, reason)
		}
	}
}

func TestDirectLeaseExpiresAtUsesTTLAsCap(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cfg := Config{TTL: 10 * time.Minute, IdleTimeout: 2 * time.Hour}
	if got := directLeaseExpiresAt(now, cfg); !got.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("expires_at=%s want TTL cap", got)
	}
	cfg = Config{TTL: 90 * time.Minute, IdleTimeout: 30 * time.Minute}
	if got := directLeaseExpiresAt(now, cfg); !got.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("expires_at=%s want idle timeout", got)
	}
}

func TestCoordinatorMachineOrphanField(t *testing.T) {
	active := activeCoordinatorLeaseIDs([]CoordinatorLease{{ID: "cbx_active"}})
	tests := map[string]struct {
		labels map[string]string
		want   string
	}{
		"active lease": {
			labels: map[string]string{"lease": "cbx_active"},
			want:   "",
		},
		"missing lease label": {
			labels: map[string]string{},
			want:   " orphan=missing-lease-label",
		},
		"missing active lease": {
			labels: map[string]string{"lease": "cbx_old"},
			want:   " orphan=no-active-lease",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := coordinatorMachineOrphanField(tt.labels, active); got != tt.want {
				t.Fatalf("orphan field=%q want %q", got, tt.want)
			}
		})
	}
}

func TestCoordinatorExternalRunnersFromBlacksmithListView(t *testing.T) {
	view := []map[string]string{
		{
			"id":       "tbx_01kqyahxh67z6qtwtsdkt5xcst",
			"status":   "ready",
			"repo":     "openclaw",
			"workflow": ".github/workflows/ci-check-testbox.yml",
			"job":      "check",
			"ref":      "main",
			"created":  "2026-05-06T09:45:16.000000Z",
		},
	}

	runners, err := coordinatorExternalRunnersFromListView(view)
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 1 {
		t.Fatalf("len=%d, want 1", len(runners))
	}
	got := runners[0]
	if got.Provider != "blacksmith-testbox" {
		t.Fatalf("provider=%q", got.Provider)
	}
	if got.ID != "tbx_01kqyahxh67z6qtwtsdkt5xcst" || got.CreatedAt != "2026-05-06T09:45:16.000000Z" {
		t.Fatalf("unexpected runner: %#v", got)
	}
}

type testExternalRunnerJSONBackend struct {
	view     any
	requests []ListRequest
}

func (b *testExternalRunnerJSONBackend) Spec() ProviderSpec {
	return ProviderSpec{Name: "blacksmith-testbox"}
}

func (b *testExternalRunnerJSONBackend) ListJSON(_ context.Context, req ListRequest) (any, error) {
	b.requests = append(b.requests, req)
	return b.view, nil
}

func TestSyncExternalRunnersBestEffortPostsBlacksmithJSONList(t *testing.T) {
	var posted struct {
		Provider string                      `json:"provider"`
		Runners  []CoordinatorExternalRunner `json:"runners"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runners/sync" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatalf("decode sync body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(CoordinatorExternalRunnerSyncResponse{})
	}))
	defer server.Close()

	backend := &testExternalRunnerJSONBackend{view: []map[string]string{
		{
			"id":      "tbx_01sync",
			"status":  "ready",
			"repo":    "example-org/example-repo",
			"created": "2026-05-06T09:45:16.000000Z",
		},
	}}

	app := App{Stderr: io.Discard}
	app.syncExternalRunnersBestEffort(context.Background(), Config{Provider: "blacksmith-testbox", Coordinator: server.URL}, backend)

	if len(backend.requests) != 1 || !backend.requests[0].All {
		t.Fatalf("list requests=%#v, want one all request", backend.requests)
	}
	if posted.Provider != "blacksmith-testbox" || len(posted.Runners) != 1 {
		t.Fatalf("posted=%#v", posted)
	}
	if got := posted.Runners[0]; got.ID != "tbx_01sync" || got.Provider != "blacksmith-testbox" {
		t.Fatalf("runner=%#v", got)
	}
}

func TestExternalRunnerGitHubRepoFallsBackToBlacksmithOrg(t *testing.T) {
	cfg := Config{}
	cfg.Blacksmith.Org = "openclaw"
	repo, ok := externalRunnerGitHubRepo(cfg, CoordinatorExternalRunner{Repo: "crabbox"})
	if !ok {
		t.Fatal("repo not inferred")
	}
	if repo.Slug() != "openclaw/crabbox" {
		t.Fatalf("repo=%q", repo.Slug())
	}
}

func TestExternalRunnerGitHubRepoFallsBackToRepoMirror(t *testing.T) {
	repo, ok := externalRunnerGitHubRepo(Config{}, CoordinatorExternalRunner{Repo: "openclaw"})
	if !ok {
		t.Fatal("repo not inferred")
	}
	if repo.Slug() != "openclaw/openclaw" {
		t.Fatalf("repo=%q", repo.Slug())
	}
}

func TestMatchExternalRunnerActionRunChoosesClosestCreatedAt(t *testing.T) {
	runner := CoordinatorExternalRunner{
		Ref:       "main",
		CreatedAt: "2026-05-06T10:00:00Z",
	}
	run, ok := matchExternalRunnerActionRun(runner, []externalRunnerActionsRun{
		{DatabaseID: 1, HeadBranch: "main", CreatedAt: "2026-05-06T08:00:00Z"},
		{DatabaseID: 2, HeadBranch: "main", CreatedAt: "2026-05-06T10:02:00Z"},
		{DatabaseID: 3, HeadBranch: "feature", CreatedAt: "2026-05-06T10:01:00Z"},
	})
	if !ok {
		t.Fatal("run not matched")
	}
	if run.DatabaseID != 2 {
		t.Fatalf("run=%d, want 2", run.DatabaseID)
	}
}

func TestExternalRunnerWorkflowURLUsesWorkflowBasename(t *testing.T) {
	got := externalRunnerWorkflowURL(
		GitHubRepo{Owner: "openclaw", Name: "openclaw"},
		".github/workflows/ci-check-testbox.yml",
	)
	want := "https://github.com/openclaw/openclaw/actions/workflows/ci-check-testbox.yml"
	if got != want {
		t.Fatalf("url=%q want %q", got, want)
	}
}

func TestStripANSIRemovesGitHubColorOutput(t *testing.T) {
	got := stripANSI("\x1b[1;37m[\x1b[m{\"databaseId\":1}]")
	if got != "[{\"databaseId\":1}]" {
		t.Fatalf("stripped=%q", got)
	}
}

func TestHeartbeatInterval(t *testing.T) {
	tests := map[time.Duration]time.Duration{
		0:                time.Minute,
		9 * time.Second:  5 * time.Second,
		30 * time.Second: 10 * time.Second,
		90 * time.Minute: time.Minute,
	}
	for ttl, want := range tests {
		if got := heartbeatInterval(ttl); got != want {
			t.Fatalf("heartbeatInterval(%s)=%s want %s", ttl, got, want)
		}
	}
}
