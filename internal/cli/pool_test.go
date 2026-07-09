package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

type testListRequestBackend struct {
	testSSHBackend
	requests []ListRequest
}

func (b *testListRequestBackend) List(_ context.Context, req ListRequest) ([]LeaseView, error) {
	b.requests = append(b.requests, req)
	return []LeaseView{{CloudID: "cbx_list_all", Name: "list-all", Status: "ready"}}, nil
}

func TestListForwardsAllAndRefreshToBackend(t *testing.T) {
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	backend := &testListRequestBackend{testSSHBackend: testSSHBackend{spec: testAWSProvider{}.Spec()}}
	testAWSBackendOverride = backend
	defer func() { testAWSBackendOverride = nil }()

	var stdout, stderr bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &stderr}).list(context.Background(), []string{"--provider", "aws", "--all", "--refresh", "--json"}); err != nil {
		t.Fatalf("list error=%v stderr=%q", err, stderr.String())
	}
	if len(backend.requests) != 1 {
		t.Fatalf("list requests=%#v, want one request", backend.requests)
	}
	if got := backend.requests[0]; !got.All || !got.Refresh {
		t.Fatalf("list request=%#v, want all and refresh", got)
	}
	if stdout.Len() == 0 {
		t.Fatal("list JSON output is empty")
	}
}

func TestListRoutedExternalAdapterScrubsCurrentRoutedAndOverrideDesktopEnvironments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake external adapter uses POSIX shell")
	}
	root := setExternalRoutingTestHome(t)
	adapterPath := filepath.Join(root, "external-adapter")
	adapter := `#!/bin/sh
if [ "${CURRENT_ARD_PASSWORD+x}" = x ]; then echo leaked-current >&2; exit 89; fi
if [ "${ROUTED_ARD_PASSWORD+x}" = x ]; then echo leaked-routed >&2; exit 90; fi
if [ "${OVERRIDE_ARD_PASSWORD+x}" = x ]; then echo leaked-override >&2; exit 91; fi
[ "${CRABBOX_TEST_KEEP:-}" = preserved ] || exit 92
printf '%s\n' '{"protocolVersion":1,"leases":[]}'
`
	if err := os.WriteFile(adapterPath, []byte(adapter), 0o700); err != nil {
		t.Fatal(err)
	}
	routing := ExternalConfig{
		Command:  adapterPath,
		WorkRoot: "/tmp/crabbox-routed-work",
		Connection: ExternalConnectionConfig{Desktop: ExternalDesktopConfig{
			PasswordEnv: "ROUTED_ARD_PASSWORD",
		}},
	}
	SetExternalRoutingTarget(&routing, targetLinux, windowsModeNormal)
	routingPath, err := PersistValidatedExternalRouting("cbx_monotonic123", routing)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	config := `provider: external
target: linux
external:
  command: /bin/false
  workRoot: /tmp/crabbox-current-work
  connection:
    desktop:
      passwordEnv: CURRENT_ARD_PASSWORD
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"CRABBOX_PROVIDER",
		"CRABBOX_TARGET",
		"CRABBOX_TARGET_OS",
		"CRABBOX_EXTERNAL_COMMAND",
		"CRABBOX_EXTERNAL_ROUTING_FILE",
		"CRABBOX_EXTERNAL_DESKTOP_USERNAME",
		"CRABBOX_COORDINATOR",
		"CRABBOX_COORDINATOR_TOKEN",
		"CRABBOX_COORDINATOR_ADMIN_TOKEN",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_EXTERNAL_DESKTOP_PASSWORD_ENV", "OVERRIDE_ARD_PASSWORD")
	t.Setenv("CURRENT_ARD_PASSWORD", "current-secret")
	t.Setenv("ROUTED_ARD_PASSWORD", "routed-secret")
	t.Setenv("OVERRIDE_ARD_PASSWORD", "override-secret")
	t.Setenv("CRABBOX_TEST_KEEP", "preserved")

	var stdout, stderr bytes.Buffer
	err = (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{
		"list",
		"--provider", "external",
		"--external-routing-file", routingPath,
		"--json",
	})
	if err != nil {
		t.Fatalf("list error=%v stderr=%q", err, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "null" {
		t.Fatalf("list output=%q", stdout.String())
	}
}

func TestLifecycleProviderOverrideRecomputesSingleTargetDefaults(t *testing.T) {
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_PROVIDER", "tart")
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")

	tests := []struct {
		name string
		run  func(App) error
	}{
		{
			name: "list",
			run: func(app App) error {
				return app.list(context.Background(), []string{"--provider", "cloudflare-dynamic-workers"})
			},
		},
		{
			name: "cleanup",
			run: func(app App) error {
				return app.cleanup(context.Background(), []string{"--provider", "cloudflare-dynamic-workers"})
			},
		},
		{
			name: "doctor",
			run: func(app App) error {
				return app.doctor(context.Background(), []string{"--provider", "cloudflare-dynamic-workers"})
			},
		},
		{
			name: "stop",
			run: func(app App) error {
				return app.stop(context.Background(), []string{"--provider", "cloudflare-dynamic-workers", "--id", "cfdw_test"})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := tt.run(App{Stdout: &stdout, Stderr: &stderr}); err != nil {
				t.Fatalf("%s error=%v stderr=%q", tt.name, err, stderr.String())
			}
		})
	}
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

func TestFilterJSONListViewByPondFailsClosedWithoutLabels(t *testing.T) {
	view := []any{
		map[string]any{"id": "cbx_one", "status": "active"},
		map[string]any{"id": "cbx_two", "status": "active"},
	}
	got, ok := filterJSONListViewByPond(view, "alpha").([]any)
	if !ok {
		t.Fatalf("filtered type=%T, want []any", got)
	}
	if len(got) != 0 {
		t.Fatalf("filtered=%#v, want empty without label evidence", got)
	}
}

func TestFilterTypedListViewByPondPreservesTypeAndFailsClosed(t *testing.T) {
	view := []CoordinatorMachine{
		{ID: "cbx_one", Labels: map[string]string{pondLabelKey: "alpha"}},
		{ID: "cbx_two", Labels: map[string]string{pondLabelKey: "bravo"}},
	}
	got, ok := filterJSONListViewByPond(view, "alpha").([]CoordinatorMachine)
	if !ok {
		t.Fatalf("filtered type=%T, want []CoordinatorMachine", got)
	}
	if len(got) != 1 || string(got[0].ID) != "cbx_one" {
		t.Fatalf("filtered=%#v", got)
	}

	noLabels := []CoordinatorMachine{{ID: "cbx_unlabeled"}}
	empty, ok := filterJSONListViewByPond(noLabels, "alpha").([]CoordinatorMachine)
	if !ok {
		t.Fatalf("filtered type=%T, want []CoordinatorMachine", empty)
	}
	if len(empty) != 0 {
		t.Fatalf("filtered=%#v, want empty without label evidence", empty)
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
