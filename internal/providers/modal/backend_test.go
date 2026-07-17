package modal

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
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpec(t *testing.T) {
	p := Provider{}
	if p.Name() != "modal" {
		t.Fatalf("Name=%q want modal", p.Name())
	}
	if len(p.Aliases()) != 0 {
		t.Fatalf("aliases=%v want none", p.Aliases())
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v want delegated run", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want linux", spec.Targets)
	}
	if !hasFeature(spec.Features, core.FeatureArchiveSync) {
		t.Fatalf("features=%#v want archive sync", spec.Features)
	}
	if !hasFeature(spec.Features, core.FeatureRunSession) {
		t.Fatalf("features=%#v want run session", spec.Features)
	}
	if hasFeature(spec.Features, core.FeatureURLBridge) {
		t.Fatalf("features=%#v should not advertise unsupported URL bridge", spec.Features)
	}
}

func TestProviderForResolvesModal(t *testing.T) {
	got, err := core.ProviderFor("modal")
	if err != nil {
		t.Fatalf("ProviderFor(modal): %v", err)
	}
	if got.Name() != "modal" {
		t.Fatalf("ProviderFor(modal).Name=%q", got.Name())
	}
}

func TestCleanModalWorkdir(t *testing.T) {
	tests := []struct {
		name    string
		workdir string
		want    string
		wantErr string
	}{
		{name: "cleans absolute", workdir: " /workspace/crabbox/ ", want: "/workspace/crabbox"},
		{name: "rejects empty", workdir: " ", wantErr: "empty"},
		{name: "rejects relative", workdir: "repo", wantErr: "absolute"},
		{name: "rejects root", workdir: "/", wantErr: "too broad"},
		{name: "rejects workspace root", workdir: "/workspace", wantErr: "too broad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cleanModalWorkdir(tt.workdir)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("workdir=%q want %q", got, tt.want)
			}
		})
	}
}

func TestModalSandboxTagsFitModalLimit(t *testing.T) {
	tags := modalSandboxTags(newTestConfig(), "cbx_123", "blue-lobster", "repo", false, time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC))
	if len(tags) > 10 {
		t.Fatalf("modal tags=%d want at most 10: %#v", len(tags), tags)
	}
	for _, key := range []string{"crabbox", "provider", "lease", "slug", "state", "keep", "expires_at", "app", "image", "repo"} {
		if tags[key] == "" {
			t.Fatalf("missing tag %q in %#v", key, tags)
		}
	}
}

func hasFeature(features core.FeatureSet, want core.Feature) bool {
	for _, feature := range features {
		if feature == want {
			return true
		}
	}
	return false
}

func TestBuildModalCommandWrapsWorkdirAndShell(t *testing.T) {
	got, err := buildModalCommand([]string{"pnpm", "test"}, false, "/workspace/crabbox")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "bash" || got[1] != "-lc" {
		t.Fatalf("command=%#v want bash -lc", got)
	}
	if !strings.Contains(got[2], "cd '/workspace/crabbox'") || !strings.Contains(got[2], "exec 'pnpm' 'test'") {
		t.Fatalf("command script=%q", got[2])
	}

	got, err = buildModalCommand([]string{"pnpm install && pnpm test"}, true, "/workspace/crabbox")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got[2], "pnpm install && pnpm test") {
		t.Fatalf("shell command script=%q", got[2])
	}
}

func TestRunCreatesExecsAndTerminatesEphemeralSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeModalAPI{}
	withFakeModalAPI(t, fake)
	cfg := newTestConfig()
	cfg.Modal.Environment = "my-app-dev"
	cfg.Modal.Secrets = []string{"example", "sample"}
	backend := NewModalBackend(Provider{}.Spec(), cfg, testRuntime()).(*modalBackend)
	req := RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"echo", "hello"},
		NoSync:  true,
	}
	result, err := backend.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d want 0", result.ExitCode)
	}
	if fake.createReq.App != "crabbox" || fake.createReq.Image != "python:3.13-slim" {
		t.Fatalf("create req=%#v", fake.createReq)
	}
	if fake.createReq.Environment != "my-app-dev" || !reflect.DeepEqual(fake.createReq.Secrets, []string{"example", "sample"}) {
		t.Fatalf("modal environment/secrets=%#v/%#v", fake.createReq.Environment, fake.createReq.Secrets)
	}
	if fake.createReq.Tags["provider"] != "modal" || fake.createReq.Tags["crabbox"] != "true" || fake.createReq.Tags["repo"] != "repo" {
		t.Fatalf("tags=%#v", fake.createReq.Tags)
	}
	if !reflect.DeepEqual(fake.verbs, []string{"create", "exec", "exec", "terminate"}) {
		t.Fatalf("verbs=%v", fake.verbs)
	}
	userCommand := fake.execCommands[1]
	if !containsArg(userCommand, "bash") || !containsArg(userCommand, "-lc") || !containsArgSubstring(userCommand, "echo") {
		t.Fatalf("user command=%v", userCommand)
	}
}

func TestRunNoSyncDoesNotDeleteExistingWorkspace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeModalAPI{
		sandbox: modalSandbox{
			ID:     "sb-123",
			Status: "running",
			Tags: map[string]string{
				"provider": "modal",
				"crabbox":  "true",
				"lease":    "cbx_123",
			},
		},
	}
	withFakeModalAPI(t, fake)
	cfg := newTestConfig()
	cfg.Sync.Delete = true
	backend := NewModalBackend(Provider{}.Spec(), cfg, testRuntime()).(*modalBackend)
	req := RunRequest{
		ID:      "sb-123",
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"test", "-f", "kept.txt"},
		NoSync:  true,
	}
	if _, err := backend.Run(context.Background(), req); err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if len(fake.execCommands) != 2 {
		t.Fatalf("exec commands=%v want prepare and user command", fake.execCommands)
	}
	prepare := strings.Join(fake.execCommands[0], " ")
	if strings.Contains(prepare, "rm -rf") {
		t.Fatalf("--no-sync prepare deleted workspace: %v", fake.execCommands[0])
	}
	if !strings.Contains(prepare, "mkdir -p") {
		t.Fatalf("--no-sync prepare should ensure workspace: %v", fake.execCommands[0])
	}
}

func TestRunReturnsSessionHandleForKeptSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeModalAPI{}
	withFakeModalAPI(t, fake)
	backend := NewModalBackend(Provider{}.Spec(), newTestConfig(), testRuntime()).(*modalBackend)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "repo", Root: t.TempDir()},
		Command: []string{"true"},
		Keep:    true,
		NoSync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil {
		t.Fatal("missing session handle")
	}
	got := result.Session
	if got.Provider != providerName || got.LeaseID == "" || got.Slug == "" || got.Reused || !got.Kept {
		t.Fatalf("session=%#v", got)
	}
	if got.CleanupCommand != "crabbox stop --provider modal --id "+shellQuote(got.LeaseID) {
		t.Fatalf("cleanup command=%q", got.CleanupCommand)
	}
	if containsVerb(fake.verbs, "terminate") {
		t.Fatalf("terminate called despite kept sandbox: %v", fake.verbs)
	}
}

func TestRunByRemoteIdentifierEnforcesRepoClaim(t *testing.T) {
	for _, tt := range []struct {
		name string
		id   string
	}{
		{name: "sandbox id", id: "sb-123"},
		{name: "lease id", id: "cbx_123"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			oldRepo := t.TempDir()
			newRepo := t.TempDir()
			if err := core.ClaimLeaseForRepoProvider("cbx_123", "blue-lobster", "other-provider", oldRepo, time.Minute, false); err != nil {
				t.Fatal(err)
			}
			fake := &fakeModalAPI{sandbox: modalSandbox{
				ID:     "sb-123",
				Status: "running",
				Tags: map[string]string{
					"provider": "modal",
					"crabbox":  "true",
					"lease":    "cbx_123",
					"slug":     "blue-lobster",
				},
			}}
			withFakeModalAPI(t, fake)
			backend := NewModalBackend(Provider{}.Spec(), newTestConfig(), testRuntime()).(*modalBackend)
			_, err := backend.Run(context.Background(), RunRequest{
				ID:      tt.id,
				Repo:    Repo{Name: "new", Root: newRepo},
				Command: []string{"true"},
				NoSync:  true,
			})
			if err == nil || !strings.Contains(err.Error(), oldRepo) || !strings.Contains(err.Error(), "--reclaim") {
				t.Fatalf("Run err=%v, want repo claim rejection", err)
			}
			if containsVerb(fake.verbs, "exec") {
				t.Fatalf("run executed despite claim rejection: %v", fake.verbs)
			}

			if _, err := backend.Run(context.Background(), RunRequest{
				ID:      tt.id,
				Repo:    Repo{Name: "new", Root: newRepo},
				Command: []string{"true"},
				NoSync:  true,
				Reclaim: true,
			}); err != nil {
				t.Fatalf("Run with reclaim err=%v", err)
			}
			claim, ok, err := core.ResolveLeaseClaim("cbx_123")
			if err != nil || !ok {
				t.Fatalf("claim ok=%v err=%v", ok, err)
			}
			if claim.Provider != providerName || claim.RepoRoot != newRepo {
				t.Fatalf("claim after reclaim=%#v, want provider=%s repo=%s", claim, providerName, newRepo)
			}
		})
	}
}

func TestCreateSandboxReportsCleanupFailureAfterClaimFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claimErr := errors.New("claim write failed")
	oldClaim := claimLeaseForRepoProviderPond
	claimLeaseForRepoProviderPond = func(string, string, string, string, string, time.Duration, bool) error {
		return claimErr
	}
	t.Cleanup(func() { claimLeaseForRepoProviderPond = oldClaim })

	fake := &fakeModalAPI{terminateErr: errors.New("terminate failed")}
	var stderr bytes.Buffer
	rt := testRuntime()
	rt.Stderr = &stderr
	backend := NewModalBackend(Provider{}.Spec(), newTestConfig(), rt).(*modalBackend)

	_, _, _, err := backend.createSandbox(context.Background(), fake, Repo{Name: "repo", Root: t.TempDir()}, false, false, "")
	if err == nil {
		t.Fatal("createSandbox err=nil, want claim and cleanup failure")
	}
	msg := err.Error()
	for _, want := range []string{"claim write failed", "cleanup modal sandbox sb-123", "terminate failed", "crabbox stop --provider modal --id sb-123"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("err=%q missing %q", msg, want)
		}
	}
	if !reflect.DeepEqual(fake.verbs, []string{"create", "terminate"}) {
		t.Fatalf("verbs=%v want create then terminate", fake.verbs)
	}
	if !strings.Contains(stderr.String(), "warning: cleanup modal sandbox sb-123") {
		t.Fatalf("stderr=%q missing cleanup warning", stderr.String())
	}
}

func TestSyncWorkspaceCleansRemoteArchiveWhenExtractFails(t *testing.T) {
	fake := &fakeModalAPI{execCodes: []int{0, 7, 0}}
	backend := NewModalBackend(Provider{}.Spec(), newTestConfig(), testRuntime()).(*modalBackend)
	repoRoot := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(repoRoot, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := backend.syncWorkspace(context.Background(), fake, "sb-123", RunRequest{
		Repo: Repo{Name: "repo", Root: repoRoot},
	}, "/workspace/crabbox")
	if err == nil {
		t.Fatalf("expected extract failure")
	}
	verbs := fake.verbs
	want := []string{"exec", "upload", "exec", "exec"}
	if !reflect.DeepEqual(verbs, want) {
		t.Fatalf("verbs=%v want %v", verbs, want)
	}
	cleanup := strings.Join(fake.execCommands[2], " ")
	if !strings.Contains(cleanup, "rm -f '/tmp/crabbox-modal-sync-") {
		t.Fatalf("cleanup command missing remote archive removal: %v", fake.execCommands[2])
	}
}

func TestKeepOnFailureRetainsSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeModalAPI{execCodes: []int{0, 7}}
	withFakeModalAPI(t, fake)
	var stderr bytes.Buffer
	rt := testRuntime()
	rt.Stderr = &stderr
	backend := NewModalBackend(Provider{}.Spec(), newTestConfig(), rt).(*modalBackend)
	req := RunRequest{
		Repo:          Repo{Name: "repo", Root: t.TempDir()},
		Command:       []string{"false"},
		NoSync:        true,
		KeepOnFailure: true,
		TimingJSON:    true,
	}
	result, err := backend.Run(context.Background(), req)
	if result.ExitCode != 7 {
		t.Fatalf("exit=%d want 7", result.ExitCode)
	}
	var ee ExitError
	if !errors.As(err, &ee) || ee.Code != 7 {
		t.Fatalf("err=%v want ExitError code 7", err)
	}
	if containsVerb(fake.verbs, "terminate") {
		t.Fatalf("terminate called despite --keep-on-failure: %v", fake.verbs)
	}
	if !strings.Contains(stderr.String(), "keep-on-failure: kept lease=") {
		t.Fatalf("missing keep-on-failure hint: %s", stderr.String())
	}
	if result.Session == nil || !result.Session.Kept || result.Session.CleanupCommand == "" {
		t.Fatalf("session=%#v", result.Session)
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

func TestStatusMapsSandboxTags(t *testing.T) {
	fake := &fakeModalAPI{
		sandbox: modalSandbox{
			ID:     "sb-123",
			Status: "running",
			Tags: map[string]string{
				"provider": "modal",
				"crabbox":  "true",
				"lease":    "cbx_123",
				"slug":     "blue-lobster",
				"image":    "python:3.13-slim",
			},
		},
	}
	withFakeModalAPI(t, fake)
	view, err := NewModalBackend(Provider{}.Spec(), newTestConfig(), testRuntime()).(*modalBackend).Status(context.Background(), StatusRequest{ID: "cbx_123"})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "cbx_123" || view.Slug != "blue-lobster" || !view.Ready || view.ServerID != "sb-123" {
		t.Fatalf("view=%#v", view)
	}
}

func newTestConfig() Config {
	return Config{
		Provider:    providerName,
		TTL:         90 * time.Minute,
		IdleTimeout: 30 * time.Minute,
		Modal: ModalConfig{
			App:     "crabbox",
			Image:   "python:3.13-slim",
			Workdir: "/workspace/crabbox",
			Python:  "python3",
		},
	}
}

func testRuntime() Runtime {
	return Runtime{Stdout: io.Discard, Stderr: io.Discard}
}

func withFakeModalAPI(t *testing.T, fake *fakeModalAPI) {
	t.Helper()
	old := newModalAPI
	newModalAPI = func(Config, Runtime) (modalAPI, error) { return fake, nil }
	t.Cleanup(func() { newModalAPI = old })
}

type fakeModalAPI struct {
	verbs        []string
	createReq    modalCreateSandboxRequest
	sandbox      modalSandbox
	execCommands [][]string
	execCodes    []int
	terminateErr error
}

func (f *fakeModalAPI) CreateSandbox(_ context.Context, req modalCreateSandboxRequest) (modalSandbox, error) {
	f.verbs = append(f.verbs, "create")
	f.createReq = req
	if f.sandbox.ID != "" {
		return f.sandbox, nil
	}
	return modalSandbox{ID: "sb-123", Status: "running", Tags: req.Tags}, nil
}

func (f *fakeModalAPI) Exec(_ context.Context, req modalExecRequest) (int, error) {
	f.verbs = append(f.verbs, "exec")
	f.execCommands = append(f.execCommands, req.Command)
	if len(f.execCodes) == 0 {
		return 0, nil
	}
	code := f.execCodes[0]
	f.execCodes = f.execCodes[1:]
	return code, nil
}

func (f *fakeModalAPI) UploadFile(context.Context, string, string, string) error {
	f.verbs = append(f.verbs, "upload")
	return nil
}

func (f *fakeModalAPI) GetSandbox(context.Context, string) (modalSandbox, error) {
	f.verbs = append(f.verbs, "get")
	if f.sandbox.ID != "" {
		return f.sandbox, nil
	}
	return modalSandbox{ID: "sb-123", Status: "running", Tags: map[string]string{"provider": "modal", "crabbox": "true", "lease": "cbx_123"}}, nil
}

func (f *fakeModalAPI) ListSandboxes(context.Context, map[string]string) ([]modalSandbox, error) {
	f.verbs = append(f.verbs, "list")
	if f.sandbox.ID != "" {
		return []modalSandbox{f.sandbox}, nil
	}
	return []modalSandbox{{ID: "sb-123", Status: "running", Tags: map[string]string{"provider": "modal", "crabbox": "true", "lease": "cbx_123"}}}, nil
}

func (f *fakeModalAPI) Terminate(context.Context, string) error {
	f.verbs = append(f.verbs, "terminate")
	return f.terminateErr
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func containsArgSubstring(args []string, want string) bool {
	for _, arg := range args {
		if strings.Contains(arg, want) {
			return true
		}
	}
	return false
}

func containsVerb(verbs []string, want string) bool {
	for _, verb := range verbs {
		if verb == want {
			return true
		}
	}
	return false
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := osexec.Command("git", "init", "-q", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return root
}
