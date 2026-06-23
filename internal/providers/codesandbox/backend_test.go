package codesandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWarmupCreatesSandboxClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	backend, stdout, stderr := newFakeBackend(t, fake)

	if err := backend.Warmup(context.Background(), WarmupRequest{
		Repo:          Repo{Name: "my-app", Root: "/repo"},
		RequestedSlug: "codesandbox-blue",
		TimingJSON:    true,
	}); err != nil {
		t.Fatalf("Warmup err=%v", err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("create calls=%d want 1", len(fake.created))
	}
	if got := fake.created[0].Tags; !contains(got, codeSandboxClaimTag) || !hasPrefix(got, codeSandboxScopeTagPrefix) {
		t.Fatalf("create tags=%v missing ownership tags", got)
	}
	leaseID := leasePrefix + fake.sandboxID
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if claim.Provider != providerName || claim.Slug != "codesandbox-blue" || claim.RepoRoot != "/repo" || !strings.HasPrefix(claim.ProviderScope, codeSandboxClaimScopePrefix) {
		t.Fatalf("claim=%#v", claim)
	}
	if !strings.Contains(stdout.String(), "leased "+leaseID) || !strings.Contains(stderr.String(), `"provider":"codesandbox"`) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestResolveLeaseIDRequiresLocalClaimForRawIDs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, _, _, _, err := resolveLeaseID("sb-raw"); err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("resolve err=%v, want unclaimed rejection", err)
	}
}

func TestCodeSandboxScopeTagUsesProviderAcceptedCharacters(t *testing.T) {
	scope := codeSandboxClaimScopePrefix + strings.Repeat("a", 32)
	tag := codeSandboxScopeTag(scope)
	if tag != codeSandboxScopeTagPrefix+strings.Repeat("a", 32) {
		t.Fatalf("tag=%q", tag)
	}
	if strings.ContainsAny(tag, ":/") {
		t.Fatalf("tag=%q contains CodeSandbox-rejected punctuation", tag)
	}
	if got, ok := codeSandboxScopeFromTag(tag); !ok || got != scope {
		t.Fatalf("round trip scope=%q ok=%v", got, ok)
	}
}

func TestStopRejectsOwnershipMismatchBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	backend, _, _ := newFakeBackend(t, fake)
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, codeSandboxClaimScopePrefix+"local", "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Tags = []string{codeSandboxClaimTag, codeSandboxScopeTag(codeSandboxClaimScopePrefix + "remote")}

	err := backend.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "ownership tag") {
		t.Fatalf("Stop err=%v, want ownership rejection", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v, want no delete", fake.deleted)
	}
}

func TestStopRejectsMissingOwnershipTagBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	backend, _, _ := newFakeBackend(t, fake)
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, fake.scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Tags = []string{codeSandboxClaimTag}

	err := backend.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "missing its Crabbox ownership tag") {
		t.Fatalf("Stop err=%v, want missing ownership tag rejection", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v, want no delete", fake.deleted)
	}
}

func TestCleanupDeletesDueOwnedClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	backend, stdout, _ := newFakeBackend(t, fake)
	backend.rt.Clock = fixedClock{t: time.Now().UTC().Add(2 * time.Second)}
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "old", providerName, fake.scope, "", "/repo", time.Second, false); err != nil {
		t.Fatal(err)
	}

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup err=%v", err)
	}
	if !reflect.DeepEqual(fake.deleted, []string{fake.sandboxID}) {
		t.Fatalf("deleted=%v, want sandbox delete", fake.deleted)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim after cleanup=%#v err=%v, want removed", claim, err)
	}
	if got := stdout.String(); !strings.Contains(got, "delete sandbox="+fake.sandboxID+" lease="+leaseID+" reason=idle timeout") ||
		!strings.Contains(got, "codesandbox cleanup removed=1 checked=1") {
		t.Fatalf("stdout=%q", got)
	}
}

func TestCleanupDryRunKeepsDueOwnedClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	backend, stdout, _ := newFakeBackend(t, fake)
	backend.rt.Clock = fixedClock{t: time.Now().UTC().Add(2 * time.Second)}
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "old", providerName, fake.scope, "", "/repo", time.Second, false); err != nil {
		t.Fatal(err)
	}

	if err := backend.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatalf("Cleanup dry-run err=%v", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v, want dry-run to skip delete", fake.deleted)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != leaseID {
		t.Fatalf("claim after dry-run=%#v err=%v, want retained", claim, err)
	}
	if got := stdout.String(); !strings.Contains(got, "would delete sandbox="+fake.sandboxID+" lease="+leaseID+" reason=idle timeout") {
		t.Fatalf("stdout=%q", got)
	}
}

func TestCleanupSkipsClaimsBeforeIdleDeadline(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	backend, _, stderr := newFakeBackend(t, fake)
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "fresh", providerName, fake.scope, "", "/repo", time.Hour, false); err != nil {
		t.Fatal(err)
	}

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup err=%v", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v, want not-due claim retained", fake.deleted)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != leaseID {
		t.Fatalf("claim after cleanup=%#v err=%v, want retained", claim, err)
	}
	if got := stderr.String(); !strings.Contains(got, "skip sandbox="+fake.sandboxID+" lease="+leaseID+" reason=idle timeout not reached") {
		t.Fatalf("stderr=%q", got)
	}
}

func TestPauseResumeUseSDKLifecycleAndKeepClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	backend, _, stderr := newFakeBackend(t, fake)
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "nap", providerName, fake.scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	if err := backend.Pause(context.Background(), PauseRequest{ID: "nap"}); err != nil {
		t.Fatalf("Pause err=%v", err)
	}
	if err := backend.Resume(context.Background(), ResumeRequest{ID: leaseID}); err != nil {
		t.Fatalf("Resume err=%v", err)
	}
	if !reflect.DeepEqual(fake.hibernated, []string{fake.sandboxID}) || !reflect.DeepEqual(fake.resumed, []string{fake.sandboxID}) {
		t.Fatalf("hibernated=%v resumed=%v", fake.hibernated, fake.resumed)
	}
	if _, ok, err := resolveCodeSandboxLeaseClaim("nap"); err != nil || !ok {
		t.Fatalf("claim missing after pause/resume ok=%v err=%v", ok, err)
	}
	if got := stderr.String(); !strings.Contains(got, "paused lease="+leaseID) || !strings.Contains(got, "resumed lease="+leaseID) {
		t.Fatalf("stderr=%q", got)
	}
}

func TestPauseRejectsOwnershipMismatchBeforeHibernate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	backend, _, _ := newFakeBackend(t, fake)
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, codeSandboxClaimScopePrefix+"local", "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Tags = []string{codeSandboxClaimTag, codeSandboxScopeTag(codeSandboxClaimScopePrefix + "remote")}

	err := backend.Pause(context.Background(), PauseRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "ownership tag") {
		t.Fatalf("Pause err=%v, want ownership rejection", err)
	}
	if len(fake.hibernated) != 0 {
		t.Fatalf("hibernated=%v, want no call", fake.hibernated)
	}
}

func TestPortsListPublishAndRejectUnsupportedUnpublish(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	fake.ports = []PortInfo{{Port: 3000, Host: "https://sb-test01-3000.csb.app"}}
	backend, _, _ := newFakeBackend(t, fake)
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "web", providerName, fake.scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	out, err := backend.Ports(context.Background(), PortsRequest{ID: "web"})
	if err != nil {
		t.Fatalf("Ports list err=%v", err)
	}
	if out != "3000 https://sb-test01-3000.csb.app" {
		t.Fatalf("list output=%q", out)
	}
	out, err = backend.Ports(context.Background(), PortsRequest{ID: leaseID, JSON: true, Publish: []string{"5173"}})
	if err != nil {
		t.Fatalf("Ports publish err=%v", err)
	}
	var got []PortInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json output=%q err=%v", out, err)
	}
	if len(got) != 1 || got[0].Port != 5173 || got[0].Host != "https://sb-test01-5173.csb.app" {
		t.Fatalf("publish ports=%#v", got)
	}
	if !reflect.DeepEqual(fake.waitedPorts, []int{5173}) {
		t.Fatalf("waited ports=%v", fake.waitedPorts)
	}

	_, err = backend.Ports(context.Background(), PortsRequest{ID: "web", Publish: []string{"127.0.0.1:41000:3000"}})
	if err == nil || !strings.Contains(err.Error(), "only support a sandbox port number") {
		t.Fatalf("complex port spec err=%v", err)
	}
	_, err = backend.Ports(context.Background(), PortsRequest{ID: "web", Unpublish: []string{"3000"}})
	if err == nil || !strings.Contains(err.Error(), "does not support ports --unpublish") {
		t.Fatalf("unpublish err=%v", err)
	}
}

func TestListAndStatusUseOwnedClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	backend, _, _ := newFakeBackend(t, fake)
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "listed", providerName, fake.scope, "pond-a", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProviderScopePond("csbx_other", "other", "other-provider", fake.scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	leases, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List err=%v", err)
	}
	if len(leases) != 1 || leases[0].CloudID != fake.sandboxID || leases[0].Labels["slug"] != "listed" {
		t.Fatalf("leases=%#v", leases)
	}
	status, err := backend.Status(context.Background(), StatusRequest{ID: "listed"})
	if err != nil {
		t.Fatalf("Status err=%v", err)
	}
	if status.ID != leaseID || status.ServerID != fake.sandboxID || !status.Ready || status.Pond != "pond-a" {
		t.Fatalf("status=%#v", status)
	}
}

func TestListAndStatusDoNotTreatMissingStateAsReady(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	fake.sandbox.State = ""
	backend, _, _ := newFakeBackend(t, fake)
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "unknown-state", providerName, fake.scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	leases, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Status != "unknown" {
		t.Fatalf("leases=%#v", leases)
	}
	status, err := backend.Status(context.Background(), StatusRequest{ID: "unknown-state"})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "unknown" || status.Ready {
		t.Fatalf("status=%#v", status)
	}
}

func TestRunStreamsOutputAndCleansUpOneShot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	fake.commandResults = []CommandResult{
		{ExitCode: 0}, // mkdir for --no-sync
		{ExitCode: 0, Stdout: "hello\n", Stderr: "note\n"},
	}
	backend, stdout, stderr := newFakeBackend(t, fake)

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Name: "my-app", Root: "/repo"},
		NoSync:     true,
		Command:    []string{"echo", "hello"},
		Env:        map[string]string{"SECRET_TOKEN": "super-secret"},
		EnvSummary: true,
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.ExitCode != 0 || !result.SyncDelegated || result.Provider != providerName {
		t.Fatalf("result=%#v", result)
	}
	if result.Session == nil || result.Session.Provider != providerName || result.Session.Reused || result.Session.Kept || !strings.Contains(result.Session.CleanupCommand, "crabbox stop --provider codesandbox") {
		t.Fatalf("unexpected one-shot session handle: %#v", result.Session)
	}
	if !strings.Contains(stdout.String(), "hello\n") || !strings.Contains(stderr.String(), "note\n") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "super-secret") || !strings.Contains(stderr.String(), "SECRET_TOKEN=set len=12 secret=true") {
		t.Fatalf("env summary leaked or missing metadata: %q", stderr.String())
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandboxID {
		t.Fatalf("deleted=%v", fake.deleted)
	}
	if _, ok, err := resolveCodeSandboxLeaseClaim(leasePrefix + fake.sandboxID); err != nil || ok {
		t.Fatalf("claim exists after cleanup ok=%v err=%v", ok, err)
	}
}

func TestRunPropagatesExitAndKeepOnFailureRetainsSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	fake.commandResults = []CommandResult{
		{ExitCode: 0},
		{ExitCode: 7, Stderr: "boom\n"},
	}
	backend, _, stderr := newFakeBackend(t, fake)

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Name: "my-app", Root: "/repo"},
		NoSync:        true,
		Command:       []string{"false"},
		KeepOnFailure: true,
		TimingJSON:    true,
	})
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 || result.ExitCode != 7 {
		t.Fatalf("err=%v result=%#v", err, result)
	}
	if result.Session == nil || result.Session.Reused || !result.Session.Kept || result.Session.CleanupCommand == "" {
		t.Fatalf("keep-on-failure should return retained session handle: %#v", result.Session)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v, want retained", fake.deleted)
	}
	if !strings.Contains(stderr.String(), "stop: crabbox stop --provider codesandbox") {
		t.Fatalf("missing keep-on-failure stop hint: %q", stderr.String())
	}
	var report map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		var candidate map[string]any
		if err := json.Unmarshal([]byte(line), &candidate); err == nil {
			report = candidate
		}
	}
	if report == nil {
		t.Fatalf("stderr does not contain timing JSON: %q", stderr.String())
	}
	if report["runStatus"] != "failed" || report["errorKind"] != "command-exit" {
		t.Fatalf("timing outcome status=%v kind=%v", report["runStatus"], report["errorKind"])
	}
}

func TestRunByIDReturnsReusedSessionHandle(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	fake.commandResults = []CommandResult{
		{ExitCode: 0},
		{ExitCode: 0, Stdout: "reused\n"},
	}
	backend, stdout, _ := newFakeBackend(t, fake)
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "reused", providerName, fake.scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: "/repo"},
		ID:      "reused",
		NoSync:  true,
		Command: []string{"echo", "reused"},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.Session == nil || result.Session.LeaseID != leaseID || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("unexpected reused session handle: %#v", result.Session)
	}
	if !strings.Contains(stdout.String(), "reused\n") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v, want retained", fake.deleted)
	}
}

func TestRunCancellationPropagates(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	fake.blockRun = true
	backend, _, _ := newFakeBackend(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := backend.Run(ctx, RunRequest{
		Repo:    Repo{Name: "my-app", Root: "/repo"},
		NoSync:  true,
		Command: []string{"sleep", "10"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err=%v, want context.Canceled", err)
	}
}

func TestRunSyncOnlyUploadsArchiveAndExtracts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"), []byte("module example.test/app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repoRoot)
	fake := newFakeCodeSandboxAPI()
	fake.commandResults = []CommandResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0}}
	backend, stdout, _ := newFakeBackend(t, fake)
	backend.cfg.CodeSandbox.Workdir = "/project/workspace/my-app"

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:           Repo{Name: "my-app", Root: repoRoot},
		SyncOnly:       true,
		ForceSyncLarge: true,
	})
	if err != nil {
		t.Fatalf("Run sync-only err=%v", err)
	}
	if !result.SyncDelegated || !strings.Contains(stdout.String(), "synced /project/workspace/my-app") {
		t.Fatalf("result=%#v stdout=%q", result, stdout.String())
	}
	if len(fake.uploads) != 1 || !strings.HasPrefix(fake.uploads[0].Path, "/project/workspace/.crabbox-codesandbox-sync-") || len(fake.uploads[0].Data) == 0 {
		t.Fatalf("uploads=%#v", fake.uploads)
	}
	if !fake.hasCommandContaining("tar -xzf") || !fake.hasCommandContaining("/project/workspace/.crabbox-codesandbox-sync-") || !fake.hasCommandContaining("/project/workspace/my-app") {
		t.Fatalf("commands=%#v", fake.commands)
	}
}

func TestRunSyncOnlyReplacesDefaultWorkspaceMountContents(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "proof.txt"), []byte("proof\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repoRoot)
	fake := newFakeCodeSandboxAPI()
	backend, _, _ := newFakeBackend(t, fake)

	if _, err := backend.Run(context.Background(), RunRequest{
		Repo:           Repo{Name: "my-app", Root: repoRoot},
		SyncOnly:       true,
		ForceSyncLarge: true,
	}); err != nil {
		t.Fatalf("Run sync-only err=%v", err)
	}
	if !fake.hasCommandContaining("rollback()") ||
		!fake.hasCommandContaining("cp -a '/project/.workspace.crabbox-sync-") {
		t.Fatalf("mount-safe replacement command missing: %#v", fake.commands)
	}
	if fake.hasCommandContaining("mv '/project/workspace'") {
		t.Fatalf("attempted to rename CodeSandbox workspace mount: %#v", fake.commands)
	}
}

func newFakeBackend(t *testing.T, api *fakeCodeSandboxAPI) (*codeSandboxBackend, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cfg := newTestConfig()
	var stdout, stderr bytes.Buffer
	restore := replaceClientFactory(func(Config, Runtime) (codeSandboxAPI, error) {
		return api, nil
	})
	t.Cleanup(restore)
	return &codeSandboxBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt: Runtime{
			Stdout: &stdout,
			Stderr: &stderr,
		},
	}, &stdout, &stderr
}

type uploadCall struct {
	SandboxID string
	Path      string
	Data      []byte
}

type fakeCodeSandboxAPI struct {
	sandboxID      string
	scope          string
	sandbox        SandboxSummary
	created        []CreateSandboxRequest
	deleted        []string
	hibernated     []string
	resumed        []string
	commands       []CommandRequest
	uploads        []uploadCall
	ports          []PortInfo
	waitedPorts    []int
	commandResults []CommandResult
	blockRun       bool
}

type fixedClock struct {
	t time.Time
}

func (c fixedClock) Now() time.Time {
	return c.t
}

func newFakeCodeSandboxAPI() *fakeCodeSandboxAPI {
	scope := codeSandboxClaimScopePrefix + "local"
	return &fakeCodeSandboxAPI{
		sandboxID: "sb-test01",
		scope:     scope,
		sandbox: SandboxSummary{
			ID:    "sb-test01",
			Title: "crabbox-my-app",
			State: "running",
			URL:   "https://sb-test01.csb.app",
			Tags:  []string{codeSandboxClaimTag, codeSandboxScopeTag(scope)},
		},
	}
}

func (f *fakeCodeSandboxAPI) ListSandboxes(context.Context, ListSandboxesRequest) (ListSandboxesResult, error) {
	return ListSandboxesResult{Sandboxes: []SandboxSummary{f.sandbox}, TotalCount: 1}, nil
}

func (f *fakeCodeSandboxAPI) CreateSandbox(_ context.Context, req CreateSandboxRequest) (SandboxSummary, error) {
	f.created = append(f.created, req)
	f.sandbox.Tags = append([]string(nil), req.Tags...)
	for _, tag := range req.Tags {
		if scope, ok := codeSandboxScopeFromTag(tag); ok {
			f.scope = scope
		}
	}
	return f.sandbox, nil
}

func (f *fakeCodeSandboxAPI) GetSandbox(context.Context, string) (SandboxSummary, error) {
	return f.sandbox, nil
}

func (f *fakeCodeSandboxAPI) DeleteSandbox(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeCodeSandboxAPI) HibernateSandbox(_ context.Context, id string) error {
	f.hibernated = append(f.hibernated, id)
	f.sandbox.State = "hibernated"
	return nil
}

func (f *fakeCodeSandboxAPI) ResumeSandbox(_ context.Context, id string) (SandboxSummary, error) {
	f.resumed = append(f.resumed, id)
	f.sandbox.State = "running"
	return SandboxSummary{ID: id, State: "running"}, nil
}

func (f *fakeCodeSandboxAPI) RunCommand(ctx context.Context, _ string, req CommandRequest) (CommandResult, error) {
	f.commands = append(f.commands, req)
	if f.blockRun {
		<-ctx.Done()
		return CommandResult{}, ctx.Err()
	}
	if len(f.commandResults) == 0 {
		return CommandResult{ExitCode: 0}, nil
	}
	result := f.commandResults[0]
	if len(f.commandResults) > 1 {
		f.commandResults = f.commandResults[1:]
	}
	return result, nil
}

func (f *fakeCodeSandboxAPI) UploadFile(_ context.Context, sandboxID, remotePath string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.uploads = append(f.uploads, uploadCall{SandboxID: sandboxID, Path: remotePath, Data: data})
	return nil
}

func (f *fakeCodeSandboxAPI) ListPorts(context.Context, string) ([]PortInfo, error) {
	return append([]PortInfo(nil), f.ports...), nil
}

func (f *fakeCodeSandboxAPI) WaitForPortURL(_ context.Context, _ string, port int) (PortInfo, error) {
	f.waitedPorts = append(f.waitedPorts, port)
	return PortInfo{Port: port, Host: "https://sb-test01-" + strconv.Itoa(port) + ".csb.app"}, nil
}

func (f *fakeCodeSandboxAPI) hasCommandContaining(fragment string) bool {
	for _, command := range f.commands {
		if strings.Contains(strings.Join(command.Command, " "), fragment) {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"add", "."},
		{"-c", "user.name=Crabbox Test", "-c", "user.email=test@example.com", "commit", "-m", "init"},
	} {
		cmd := osexec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
