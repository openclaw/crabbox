package githubcodespaces

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestAcquireCreatesClaimGeneratesSSHConfigAndWaitsReady(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.getSeq["cs-1"] = []codespace{
		fakeCodespace("cs-1", "Provisioning"),
		fakeCodespace("cs-1", "Available"),
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)

	lease, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:          Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedSlug: "green-box",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Server.CloudID != "cs-1" || lease.Server.Labels[labelCodespaceName] != "cs-1" {
		t.Fatalf("lease=%#v", lease)
	}
	if lease.Server.Labels[labelRepository] != "example-org/my-app" || lease.Server.Labels[labelLogin] != "alice" || lease.Server.Labels[labelRelease] != releaseDelete {
		t.Fatalf("labels=%#v", lease.Server.Labels)
	}
	if len(fc.creates) != 1 {
		t.Fatalf("creates=%#v", fc.creates)
	}
	create := fc.creates[0]
	if create.Repo != "example-org/my-app" || create.Ref != "main" || create.Machine != "standardLinux32gb" ||
		create.DevcontainerPath != ".devcontainer/devcontainer.json" || create.WorkingDirectory != "/workspaces/my-app" ||
		create.Geo != "UsWest" || !strings.HasPrefix(create.DisplayName, "crabbox-green-box-") {
		t.Fatalf("create=%#v", create)
	}
	if len(b.waits) != 1 {
		t.Fatalf("waits=%#v", b.waits)
	}
	wait := b.waits[0]
	if wait.User != "vscode" || wait.Host != "cs.cs-1.main" || wait.Key != "/tmp/codespaces/key" || !wait.SSHConfigProxy {
		t.Fatalf("wait target=%#v", wait)
	}
	if !strings.Contains(wait.ReadyCheck, "test -d '/workspaces/my-app'") {
		t.Fatalf("ready check=%q", wait.ReadyCheck)
	}
	claim, ok, err := resolveLeaseClaimForProvider(lease.LeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.CloudID != "cs-1" || claim.SSHHost != "cs.cs-1.main" || claim.Labels[labelEnvironmentID] != "env-cs-1" || claim.Labels["work_root"] != "/workspaces/my-app" {
		t.Fatalf("claim=%#v", claim)
	}
	if fg.configFor != "cs-1" {
		t.Fatalf("ssh config generated for %q", fg.configFor)
	}
}

func TestAcquireKeepDoesNotOverrideDeleteOnReleasePolicy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.getSeq["cs-1"] = []codespace{
		fakeCodespace("cs-1", "Provisioning"),
		fakeCodespace("cs-1", "Available"),
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)

	lease, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:          Repo{Root: t.TempDir(), Name: "my-app"},
		Keep:          true,
		RequestedSlug: "warm-box",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels["keep"] != "true" || lease.Server.Labels[labelRelease] != releaseDelete {
		t.Fatalf("labels=%#v", lease.Server.Labels)
	}
	claim, ok, err := resolveLeaseClaimForProvider(lease.LeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.Labels["keep"] != "true" || claim.Labels[labelRelease] != releaseDelete {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
}

func TestAcquireRetainsClaimWhenRollbackDeleteFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.getSeq["cs-1"] = []codespace{fakeCodespace("cs-1", "Failed")}
	fc.deleteErr = errors.New("delete temporarily unavailable")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: "cbx_123456789abc",
		RequestedSlug:    "rollback-box",
	})
	if err == nil {
		t.Fatal("acquire unexpectedly succeeded")
	}
	for _, want := range []string{"terminal state=Failed", "rollback github-codespaces", "delete temporarily unavailable", "cs-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%q missing %q", err, want)
		}
	}
	if _, ok, err := resolveLeaseClaimForProvider("cbx_123456789abc", providerName); err != nil || !ok {
		t.Fatalf("recovery claim missing ok=%t err=%v", ok, err)
	}
	if !fc.deleteDeadline {
		t.Fatal("rollback delete context had no deadline")
	}
}

func TestResolveStartsStoppedCodespaceAndRefreshesTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-stopped"] = fakeCodespace("cs-stopped", "Shutdown")
	fc.getSeq["cs-stopped"] = []codespace{
		fakeCodespace("cs-stopped", "Shutdown"),
		fakeCodespace("cs-stopped", "Starting"),
		fakeCodespace("cs-stopped", "Available"),
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789abc"
	server := b.serverFromCodespace(fc.items["cs-stopped"], b.labelsFor(leaseID, "sleepy-box", "example-org/my-app", "alice", true, releaseStop, fc.items["cs-stopped"], "stopped"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "sleepy-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "sleepy-box", ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.starts) != 1 || fc.starts[0] != "cs-stopped" {
		t.Fatalf("starts=%#v", fc.starts)
	}
	if lease.Server.Status != "Available" || lease.SSH.Host != "cs.cs-stopped.main" {
		t.Fatalf("lease=%#v", lease)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.Labels[labelState] != "ready" || claim.SSHHost != "cs.cs-stopped.main" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestResolveNoLocalStateMutationsDoesNotStoreSSHConfig(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	fc := newFakeCodespacesClient()
	fc.items["cs-readonly"] = fakeCodespace("cs-readonly", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac3"
	server := b.serverFromCodespace(fc.items["cs-readonly"], b.labelsFor(leaseID, "readonly-box", "example-org/my-app", "alice", true, releaseStop, fc.items["cs-readonly"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "readonly-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "readonly-box", NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "cs.cs-readonly.main" {
		t.Fatalf("lease=%#v", lease)
	}
	stored := filepath.Join(stateHome, "crabbox", "github-codespaces", leaseID+".ssh_config")
	if _, err := os.Stat(stored); !os.IsNotExist(err) {
		t.Fatalf("stored config err=%v path=%s", err, stored)
	}
}

func TestResolveStatusOnlyReadyProbeBuildsSSHTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-status"] = fakeCodespace("cs-status", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac4"
	server := b.serverFromCodespace(fc.items["cs-status"], b.labelsFor(leaseID, "status-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-status"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "status-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "status-box", StatusOnly: true, ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "cs.cs-status.main" || len(b.waits) != 1 {
		t.Fatalf("lease=%#v waits=%#v", lease, b.waits)
	}
}

func TestReleaseDeleteRemovesOnlyClaimBackedCodespaceAndConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-delete"] = fakeCodespace("cs-delete", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789abd"
	server := b.serverFromCodespace(fc.items["cs-delete"], b.labelsFor(leaseID, "delete-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-delete"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "delete-box", b.cfg, server, SSHTarget{Host: "cs-delete", Port: "22"}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if _, err := storeSSHConfig(leaseID, fg.config("cs-delete")); err != nil {
		t.Fatal(err)
	}

	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fc.deletes, ",") != "cs-delete" {
		t.Fatalf("deletes=%#v", fc.deletes)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID, providerName); err != nil || ok {
		t.Fatalf("claim remains ok=%t err=%v", ok, err)
	}
}

func TestReleaseDeleteRequiresLocalClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-orphan"] = fakeCodespace("cs-orphan", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	server := b.serverFromCodespace(fc.items["cs-orphan"], b.labelsFor("cbx_123456789ad0", "orphan-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-orphan"], "ready"))

	err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: "cbx_123456789ad0", Server: server}})
	if err == nil || !strings.Contains(err.Error(), "requires a local claim") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.deletes) != 0 || len(fc.stops) != 0 {
		t.Fatalf("provider action without claim deletes=%#v stops=%#v", fc.deletes, fc.stops)
	}
}

func TestReleaseDeleteFallsBackToStopForDirtyCodespace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-dirty", "Available")
	item.GitStatus.HasUncommittedChanges = true
	fc.items["cs-dirty"] = item
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac2"
	server := b.serverFromCodespace(item, b.labelsFor(leaseID, "dirty-box", "example-org/my-app", "alice", false, releaseDelete, item, "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "dirty-box", b.cfg, server, SSHTarget{Host: "cs-dirty", Port: "22"}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fc.stops, ",") != "cs-dirty" || len(fc.deletes) != 0 {
		t.Fatalf("stops=%#v deletes=%#v", fc.stops, fc.deletes)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.SSHHost != "" || claim.SSHPort != 0 || claim.Labels[labelRelease] != releaseStop || claim.Labels[labelState] != "stopped" {
		t.Fatalf("claim=%#v", claim)
	}
	if !b.RetainLeaseClaimAfterRelease(LeaseTarget{LeaseID: leaseID, Server: server}) {
		t.Fatal("dirty release fallback should retain local claim")
	}
	if got := b.ReleaseLeaseMessage(LeaseTarget{LeaseID: leaseID, Server: server}); !strings.Contains(got, "retained=true") {
		t.Fatalf("message=%q", got)
	}
}

func TestReleaseRetainedStopsAndClearsEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-stop"] = fakeCodespace("cs-stop", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	b.cfg.GitHubCodespaces.DeleteOnRelease = false
	leaseID := "cbx_123456789abe"
	server := b.serverFromCodespace(fc.items["cs-stop"], b.labelsFor(leaseID, "stop-box", "example-org/my-app", "alice", true, releaseStop, fc.items["cs-stop"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "stop-box", b.cfg, server, SSHTarget{Host: "cs-stop", Port: "22"}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fc.stops, ",") != "cs-stop" || len(fc.deletes) != 0 {
		t.Fatalf("stops=%#v deletes=%#v", fc.stops, fc.deletes)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.SSHHost != "" || claim.SSHPort != 0 || claim.Labels[labelRelease] != releaseStop || claim.Labels[labelState] != "stopped" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestCleanupDryRunKeepsProviderNonMutating(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-expired"] = fakeCodespace("cs-expired", "Available")
	fc.items["cs-unclaimed"] = fakeCodespace("cs-unclaimed", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789abf"
	server := b.serverFromCodespace(fc.items["cs-expired"], b.labelsFor(leaseID, "expired-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-expired"], "ready"))
	server.Labels["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := claimLeaseTargetForRepoConfig(leaseID, "expired-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("dry run deleted: %#v", fc.deletes)
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fc.deletes, ",") != "cs-expired" {
		t.Fatalf("deletes=%#v", fc.deletes)
	}
}

func TestCleanupRefusesIdentityMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-mismatch"] = fakeCodespace("cs-mismatch", "Available")
	fg := &fakeGH{login: "bob", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac1"
	server := b.serverFromCodespace(fc.items["cs-mismatch"], b.labelsFor(leaseID, "mismatch-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-mismatch"], "ready"))
	server.Labels["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := claimLeaseTargetForRepoConfig(leaseID, "mismatch-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	err := b.Cleanup(context.Background(), CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "login mismatch") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted on mismatch: %#v", fc.deletes)
	}
}

func TestDoctorIsNonMutating(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)

	result, err := b.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "mutation=false") || !strings.Contains(result.Message, "inventory=ready") {
		t.Fatalf("result=%#v", result)
	}
	if len(fc.creates) != 0 || len(fc.starts) != 0 || len(fc.stops) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("doctor mutated: creates=%#v starts=%#v stops=%#v deletes=%#v", fc.creates, fc.starts, fc.stops, fc.deletes)
	}
}

func TestControlPlanePrefersGitHubCLITokenPrecedence(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("GH_TOKEN", "gh-token")
	t.Setenv("GITHUB_TOKEN", "github-token")
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "fallback-token"}
	b := newTestBackend(t, fc, fg)
	var gotToken string
	b.clientFactory = func(token string) codespacesAPI {
		gotToken = token
		return fc
	}

	_, _, login, err := b.controlPlane(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if login != "alice" {
		t.Fatalf("login=%q", login)
	}
	if gotToken != "gh-token" {
		t.Fatalf("token=%q", gotToken)
	}
}

func TestWaitForAvailableUsesReadyTimeout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-slow"] = fakeCodespace("cs-slow", "Provisioning")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	b.readyTimeout = time.Nanosecond
	b.pollInterval = time.Hour

	_, err := b.waitForAvailable(context.Background(), fc, "cs-slow")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

func TestEffectiveWorkRootHonorsExplicitGenericWorkRoot(t *testing.T) {
	cfg := Config{
		Provider: providerName,
		WorkRoot: "/custom/workspace",
		GitHubCodespaces: GitHubCodespacesConfig{
			WorkRoot: defaultWorkRoot,
		},
	}
	core.MarkWorkRootExplicit(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, Runtime{})

	if got := b.effectiveWorkRoot("example-org/my-app"); got != "/custom/workspace" {
		t.Fatalf("work root=%q", got)
	}
}

func TestLabelsCarryEffectiveWorkRoot(t *testing.T) {
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)

	labels := b.labelsFor("cbx_123456789abc", "work-box", "example-org/my-app", "alice", false, releaseDelete, fakeCodespace("cs-1", "Available"), "ready")
	if labels["work_root"] != "/workspaces/my-app" {
		t.Fatalf("work_root=%q", labels["work_root"])
	}
}

func TestDisplayNameFitsGitHubCodespacesLimit(t *testing.T) {
	name := githubCodespacesDisplayName("cbx_abcdef123456", strings.Repeat("a", 41))
	if len(name) > 48 {
		t.Fatalf("display name length=%d name=%q", len(name), name)
	}
	if !strings.HasPrefix(name, "crabbox-") || !strings.HasSuffix(name, "-c80c2195") {
		t.Fatalf("display name=%q", name)
	}
}

type testBackend struct {
	*backend
	waits []SSHTarget
}

func newTestBackend(t *testing.T, fc *fakeCodespacesClient, fg *fakeGH) *testBackend {
	t.Helper()
	cfg := Config{
		Provider:    providerName,
		TargetOS:    targetLinux,
		SSHUser:     "vscode",
		SSHPort:     "22",
		IdleTimeout: time.Hour,
		GitHubCodespaces: GitHubCodespacesConfig{
			GHPath:           "gh",
			Repo:             "example-org/my-app",
			Ref:              "main",
			Machine:          "standardLinux32gb",
			DevcontainerPath: ".devcontainer/devcontainer.json",
			WorkingDirectory: "/workspaces/my-app",
			Geo:              "UsWest",
			IdleTimeout:      45 * time.Minute,
			RetentionPeriod:  48 * time.Hour,
			DeleteOnRelease:  true,
			WorkRoot:         defaultWorkRoot,
		},
	}
	rt := Runtime{}
	b := newBackend(Provider{}.Spec(), cfg, rt)
	b.pollInterval = time.Nanosecond
	tb := &testBackend{backend: b}
	b.clientFactory = func(string) codespacesAPI { return fc }
	b.ghFactory = func() githubCLI { return fg }
	b.waitSSH = func(_ context.Context, target *SSHTarget, _ string, _ time.Duration) error {
		tb.waits = append(tb.waits, *target)
		return nil
	}
	return tb
}

type fakeCodespacesClient struct {
	items          map[string]codespace
	getSeq         map[string][]codespace
	creates        []createCodespaceRequest
	starts         []string
	stops          []string
	deletes        []string
	deleteErr      error
	deleteDeadline bool
}

func newFakeCodespacesClient() *fakeCodespacesClient {
	return &fakeCodespacesClient{
		items:  map[string]codespace{},
		getSeq: map[string][]codespace{},
	}
}

func (f *fakeCodespacesClient) createCodespace(_ context.Context, req createCodespaceRequest) (codespace, error) {
	f.creates = append(f.creates, req)
	name := fmt.Sprintf("cs-%d", len(f.creates))
	item := fakeCodespace(name, "Provisioning")
	item.DisplayName = req.DisplayName
	item.Repository.FullName = req.Repo
	item.Machine.Name = req.Machine
	f.items[name] = item
	return item, nil
}

func (f *fakeCodespacesClient) listCodespaces(context.Context) ([]codespace, error) {
	out := make([]codespace, 0, len(f.items))
	for _, item := range f.items {
		out = append(out, item)
	}
	return out, nil
}

func (f *fakeCodespacesClient) getCodespace(_ context.Context, name string) (codespace, error) {
	if seq := f.getSeq[name]; len(seq) > 0 {
		item := seq[0]
		f.getSeq[name] = seq[1:]
		f.items[name] = item
		return item, nil
	}
	item, ok := f.items[name]
	if !ok {
		return codespace{}, githubAPIError(404, "", `{"message":"Not Found"}`)
	}
	return item, nil
}

func (f *fakeCodespacesClient) startCodespace(_ context.Context, name string) (codespace, error) {
	f.starts = append(f.starts, name)
	item := f.items[name]
	item.State = "Starting"
	f.items[name] = item
	return item, nil
}

func (f *fakeCodespacesClient) stopCodespace(_ context.Context, name string) error {
	f.stops = append(f.stops, name)
	item := f.items[name]
	item.State = "Shutdown"
	f.items[name] = item
	return nil
}

func (f *fakeCodespacesClient) deleteCodespace(ctx context.Context, name string) error {
	_, f.deleteDeadline = ctx.Deadline()
	f.deletes = append(f.deletes, name)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.items, name)
	return nil
}

func (f *fakeCodespacesClient) listMachines(context.Context, string, string) ([]codespaceMachine, error) {
	return []codespaceMachine{{Name: "standardLinux32gb"}}, nil
}

type fakeGH struct {
	login     string
	token     string
	configFor string
}

func (f *fakeGH) authStatus(context.Context) error { return nil }
func (f *fakeGH) authToken(context.Context) (string, error) {
	return f.token, nil
}
func (f *fakeGH) userLogin(context.Context) (string, error) {
	return f.login, nil
}
func (f *fakeGH) codespaceSSHConfig(_ context.Context, codespace string) (string, error) {
	f.configFor = codespace
	return f.config(codespace), nil
}
func (f *fakeGH) config(codespace string) string {
	return fmt.Sprintf(`Host cs.%s.main
  User vscode
  IdentityFile "/tmp/codespaces/key"
  UserKnownHostsFile /dev/null
  ProxyCommand gh codespace ssh -c %s --stdio
`, codespace, codespace)
}

func fakeCodespace(name, state string) codespace {
	return codespace{
		Name:          name,
		DisplayName:   "Crabbox",
		State:         state,
		EnvironmentID: "env-" + name,
		Repository:    repositoryRef{FullName: "example-org/my-app"},
		Machine:       machineRef{Name: "standardLinux32gb"},
	}
}
