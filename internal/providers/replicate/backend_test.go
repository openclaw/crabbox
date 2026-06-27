package replicate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeReplicateAPI struct {
	mu        sync.Mutex
	creates   []replicateCreatePredictionRequest
	gets      []string
	cancels   []string
	listed    int
	create    replicatePrediction
	createErr error
	getSeq    []replicatePrediction
	getErr    error
	cancel    replicatePrediction
	cancelErr error
}

func (f *fakeReplicateAPI) CreatePrediction(_ context.Context, req replicateCreatePredictionRequest) (replicatePrediction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, req)
	if f.createErr != nil {
		return replicatePrediction{}, f.createErr
	}
	return f.create, nil
}

func (f *fakeReplicateAPI) GetPrediction(_ context.Context, id string) (replicatePrediction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, id)
	if f.getErr != nil {
		return replicatePrediction{}, f.getErr
	}
	if len(f.getSeq) == 0 {
		return replicatePrediction{ID: id, Status: "succeeded", Output: rawJSON(`{"exit_code":0}`)}, nil
	}
	out := f.getSeq[0]
	if len(f.getSeq) > 1 {
		f.getSeq = f.getSeq[1:]
	}
	return out, nil
}

func (f *fakeReplicateAPI) ListPredictions(context.Context) (replicatePredictionList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed++
	return replicatePredictionList{Results: []replicatePrediction{{ID: "unrelated", Status: "processing"}}}, nil
}

func (f *fakeReplicateAPI) CancelPrediction(_ context.Context, id string) (replicatePrediction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, id)
	if f.cancelErr != nil {
		return replicatePrediction{}, f.cancelErr
	}
	if f.cancel.ID == "" {
		return replicatePrediction{ID: id, Status: "canceled"}, nil
	}
	return f.cancel, nil
}

func TestWarmupDoctorValidateConfigWithoutPredictionCreate(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "test-token")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeReplicateAPI{}
	var stdout bytes.Buffer
	backend := newTestBackend(fake, &stdout, io.Discard)

	if err := backend.Warmup(context.Background(), WarmupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Doctor(context.Background(), DoctorRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.creates) != 0 || fake.listed != 0 || len(fake.gets) != 0 || len(fake.cancels) != 0 {
		t.Fatalf("warmup/doctor made API calls: creates=%d list=%d gets=%d cancels=%d", len(fake.creates), fake.listed, len(fake.gets), len(fake.cancels))
	}
	if !strings.Contains(stdout.String(), "mutation=false") {
		t.Fatalf("warmup output=%q", stdout.String())
	}
}

func TestWarmupRequiresTokenAndRunnerTarget(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "")
	t.Setenv(envReplicateToken, "")
	backend := newTestBackend(&fakeReplicateAPI{}, io.Discard, io.Discard)
	if err := backend.Warmup(context.Background(), WarmupRequest{}); err == nil || !strings.Contains(err.Error(), "needs an API token") {
		t.Fatalf("Warmup err=%v, want missing token", err)
	}

	t.Setenv(envCrabboxReplicateToken, "test-token")
	backend.cfg.Replicate.Deployment = ""
	backend.cfg.Replicate.Version = ""
	if err := backend.Warmup(context.Background(), WarmupRequest{}); err == nil || !strings.Contains(err.Error(), "requires exactly one") {
		t.Fatalf("Warmup err=%v, want target validation", err)
	}

	backend.cfg.Replicate.Deployment = "missing-slash"
	if err := backend.Warmup(context.Background(), WarmupRequest{}); err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("Warmup err=%v, want deployment shape validation", err)
	}
}

func TestReplicateWorkdirRejectsRelativeAndBroad(t *testing.T) {
	for _, workdir := range []string{"relative", "/", "/workspace", "/tmp"} {
		cfg := testReplicateConfig()
		cfg.Replicate.Workdir = workdir
		if _, err := replicateWorkdir(cfg); err == nil {
			t.Fatalf("replicateWorkdir(%q) unexpectedly passed", workdir)
		}
	}
	cfg := testReplicateConfig()
	cfg.Replicate.Workdir = "/workspace/crabbox/../app"
	if got, err := replicateWorkdir(cfg); err != nil || got != "/workspace/app" {
		t.Fatalf("replicateWorkdir=%q err=%v", got, err)
	}
}

func TestRunCreatesPredictionWithArchiveInputAndMapsExitZero(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "test-token")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := newReplicateTestRepo(t, map[string]string{"main.go": "package main\n"})
	fake := &fakeReplicateAPI{
		create: replicatePrediction{
			ID:     "pred_success",
			Status: "succeeded",
			Logs:   "runner boot\n",
			Output: rawJSON(`{"exit_code":0,"stdout":"ok\n","stderr":"warn\n"}`),
		},
	}
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(fake, &stdout, &stderr)

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    core.Repo{Name: "my-app", Root: repoRoot},
		Command: []string{"go", "test", "./..."},
		Env: map[string]string{
			"CI":                     "1",
			"PROJECT_TOKEN":          "project-secret",
			envCrabboxReplicateToken: "crabbox-token",
			envReplicateToken:        "vendor-token",
		},
		Label:      "unit",
		TimingJSON: true,
		NoSync:     false,
		Reclaim:    true,
		ShellMode:  false,
		EnvSummary: true,
		Options:    core.LeaseOptions{EnvAllow: []string{"CI"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.LeaseID != "rbx_pred_success" || result.Session == nil || result.Session.RunID != "pred_success" {
		t.Fatalf("result=%#v", result)
	}
	if len(fake.creates) != 1 {
		t.Fatalf("creates=%d", len(fake.creates))
	}
	req := fake.creates[0]
	if req.Deployment != "owner/runner" || req.Version != "" || req.WaitSecs != 3 || req.CancelAfterSecs != 9 {
		t.Fatalf("create request=%#v", req)
	}
	command, _ := req.Input["command"].([]any)
	if !reflect.DeepEqual(command, []any{"go", "test", "./..."}) {
		t.Fatalf("command input=%#v", req.Input["command"])
	}
	if req.Input["workdir"] != "/workspace/crabbox" || req.Input["timeout_secs"] != float64(17) || req.Input["cancel_after_secs"] != float64(9) {
		t.Fatalf("runner input=%#v", req.Input)
	}
	env, ok := req.Input["env"].(map[string]any)
	if !ok || env["CI"] != "1" || env["PROJECT_TOKEN"] != "project-secret" {
		t.Fatalf("env input=%#v", req.Input["env"])
	}
	for _, name := range []string{envCrabboxReplicateToken, envReplicateToken} {
		if _, ok := env[name]; ok {
			t.Fatalf("env input forwarded provider auth %s: %#v", name, env)
		}
	}
	archive, _ := req.Input["archive_url"].(string)
	if !strings.HasPrefix(archive, replicateArchiveDataURLPrefix) {
		t.Fatalf("archive_url=%q", archive)
	}
	if stdout.String() != "ok\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "runner boot\nwarn\n") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "did not forward provider authentication variables: CRABBOX_REPLICATE_API_TOKEN,REPLICATE_API_TOKEN") {
		t.Fatalf("stderr missing auth stripping warning: %q", stderr.String())
	}
	for _, leaked := range []string{"crabbox-token", "vendor-token"} {
		if strings.Contains(stderr.String(), leaked) {
			t.Fatalf("stderr leaked auth token %q: %q", leaked, stderr.String())
		}
	}
	if _, ok, err := core.ResolveLeaseClaim("rbx_pred_success"); err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
}

func TestRunCancelsPredictionWhenRequestedSlugAllocationFails(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "test-token")
	stateFile := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(stateFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateFile)
	fake := &fakeReplicateAPI{create: replicatePrediction{ID: "pred_slug", Status: "processing"}}
	backend := newTestBackend(fake, io.Discard, io.Discard)

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          testRepo(t),
		Command:       []string{"true"},
		NoSync:        true,
		RequestedSlug: "needs-claim-store",
	})
	if err == nil || !strings.Contains(err.Error(), "read claims directory") {
		t.Fatalf("err=%v, want claim-store read failure", err)
	}
	if result.LeaseID != "rbx_pred_slug" {
		t.Fatalf("result=%#v", result)
	}
	if !reflect.DeepEqual(fake.cancels, []string{"pred_slug"}) {
		t.Fatalf("cancels=%v, want cleanup cancel", fake.cancels)
	}
}

func TestRunMapsNonzeroRunnerExit(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "test-token")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeReplicateAPI{create: replicatePrediction{ID: "pred_exit", Status: "succeeded", Output: rawJSON(`{"exit_code":7,"stderr":"bad\n"}`)}}
	backend := newTestBackend(fake, io.Discard, io.Discard)
	result, err := backend.Run(context.Background(), RunRequest{Repo: testRepo(t), Command: []string{"false"}, Reclaim: true})
	if err == nil || result.ExitCode != 7 {
		t.Fatalf("result=%#v err=%v, want exit 7", result, err)
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%T %[1]v, want ExitError code 7", err)
	}
}

func TestRunRejectsMalformedRunnerOutput(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "test-token")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeReplicateAPI{create: replicatePrediction{ID: "pred_bad", Status: "succeeded", Output: rawJSON(`{"stdout":"missing exit"}`)}}
	backend := newTestBackend(fake, io.Discard, io.Discard)
	result, err := backend.Run(context.Background(), RunRequest{Repo: testRepo(t), Command: []string{"true"}, Reclaim: true})
	if err == nil || result.ExitCode != 1 || !strings.Contains(err.Error(), "missing required exit_code") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestRunCancelOnContextCancellation(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "test-token")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeReplicateAPI{create: replicatePrediction{ID: "pred_cancel", Status: "processing", Logs: "start\n"}}
	backend := newTestBackend(fake, io.Discard, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := backend.Run(ctx, RunRequest{Repo: testRepo(t), Command: []string{"sleep", "10"}, Reclaim: true, NoSync: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("result=%#v err=%v, want context canceled", result, err)
	}
	if !reflect.DeepEqual(fake.cancels, []string{"pred_cancel"}) {
		t.Fatalf("cancels=%v", fake.cancels)
	}
}

func TestRunDedupePredictionLogsDuringPolling(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "test-token")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeReplicateAPI{
		create: replicatePrediction{ID: "pred_logs", Status: "processing", Logs: "line1\n"},
		getSeq: []replicatePrediction{{
			ID:     "pred_logs",
			Status: "succeeded",
			Logs:   "line1\nline2\n",
			Output: rawJSON(`{"exit_code":0}`),
		}},
	}
	var stderr bytes.Buffer
	backend := newTestBackend(fake, io.Discard, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{Repo: testRepo(t), Command: []string{"true"}, Reclaim: true})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if strings.Count(stderr.String(), "line1\n") != 1 || !strings.Contains(stderr.String(), "line2\n") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunRejectsUnsupportedSyncOptionsAndExistingID(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "test-token")
	backend := newTestBackend(&fakeReplicateAPI{}, io.Discard, io.Discard)
	for _, req := range []RunRequest{
		{Command: []string{"true"}, ChecksumSync: true},
		{Command: []string{"true"}, SyncOnly: true},
		{Command: []string{"true"}, ID: "pred_existing"},
	} {
		if _, err := backend.Run(context.Background(), req); err == nil {
			t.Fatalf("Run(%#v) unexpectedly passed", req)
		}
	}
}

func TestStatusSupportsRawPredictionIDAndClaimSlug(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "test-token")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeReplicateAPI{getSeq: []replicatePrediction{{ID: "pred_status", Status: "processing"}}}
	backend := newTestBackend(fake, io.Discard, io.Discard)
	view, err := backend.Status(context.Background(), StatusRequest{ID: "pred_status"})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "rbx_pred_status" || view.ServerID != "pred_status" || view.State != "processing" {
		t.Fatalf("raw status=%#v", view)
	}

	if err := claimLeaseForRepoProviderScopePond("rbx_pred_claimed", "blue", providerName, backend.claimScope(), "pond-a", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.getSeq = []replicatePrediction{{ID: "pred_claimed", Status: "succeeded"}}
	view, err = backend.Status(context.Background(), StatusRequest{ID: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "rbx_pred_claimed" || view.Slug != "blue" || view.Pond != "pond-a" || !view.Ready {
		t.Fatalf("claimed status=%#v", view)
	}
}

func TestListUsesOnlyLocalClaimsAndDoesNotListAccountPredictions(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "test-token")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeReplicateAPI{}
	backend := newTestBackend(fake, io.Discard, io.Discard)
	if err := claimLeaseForRepoProviderScopePond("rbx_owned", "owned", providerName, backend.claimScope(), "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProviderScopePond("rbx_other", "other", providerName, "replicate-endpoint-sha256:other", "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if fake.listed != 0 {
		t.Fatalf("ListPredictions called %d times", fake.listed)
	}
	if len(views) != 1 || views[0].CloudID != "owned" || views[0].Labels["slug"] != "owned" {
		t.Fatalf("views=%#v", views)
	}
}

func TestExistingPredictionOperationsDoNotRequireRunnerTarget(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "test-token")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeReplicateAPI{
		getSeq: []replicatePrediction{{ID: "pred_status", Status: "processing"}},
		cancel: replicatePrediction{
			ID:     "pred_stop",
			Status: "canceled",
		},
	}
	backend := newTestBackend(fake, io.Discard, io.Discard)
	backend.cfg.Replicate.Deployment = ""
	backend.cfg.Replicate.Version = ""

	view, err := backend.Status(context.Background(), StatusRequest{ID: "pred_status"})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "rbx_pred_status" || view.ServerID != "pred_status" {
		t.Fatalf("status=%#v", view)
	}
	if err := claimLeaseForRepoProviderScopePond("rbx_pred_list", "list-me", providerName, backend.claimScope(), "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].CloudID != "pred_list" {
		t.Fatalf("views=%#v", views)
	}
	if err := backend.Stop(context.Background(), StopRequest{ID: "pred_stop"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.cancels, []string{"pred_stop"}) {
		t.Fatalf("cancels=%v", fake.cancels)
	}
}

func TestStopCancelsPredictionAndRemovesClaim(t *testing.T) {
	t.Setenv(envCrabboxReplicateToken, "test-token")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeReplicateAPI{cancel: replicatePrediction{ID: "pred_stop", Status: "canceled"}}
	backend := newTestBackend(fake, io.Discard, io.Discard)
	if err := claimLeaseForRepoProviderScopePond("rbx_pred_stop", "stop-me", providerName, backend.claimScope(), "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := backend.Stop(context.Background(), StopRequest{ID: "stop-me"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.cancels, []string{"pred_stop"}) {
		t.Fatalf("cancels=%v", fake.cancels)
	}
	if _, ok, err := core.ResolveLeaseClaim("rbx_pred_stop"); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed", ok, err)
	}
}

func newTestBackend(api *fakeReplicateAPI, stdout, stderr io.Writer) replicateBackend {
	cfg := testReplicateConfig()
	return replicateBackend{
		spec:   Provider{}.Spec(),
		cfg:    cfg,
		rt:     Runtime{Stdout: stdout, Stderr: stderr},
		client: api,
	}
}

func testReplicateConfig() Config {
	cfg := Config{Provider: providerName}
	cfg.Replicate = ReplicateConfig{
		APIURL:           "https://api.replicate.com/v1",
		Deployment:       "owner/runner",
		Workdir:          "/workspace/crabbox",
		WaitSecs:         3,
		PollIntervalSecs: 1,
		ExecTimeoutSecs:  17,
		CancelAfterSecs:  9,
		MaxArchiveBytes:  1024 * 1024,
	}
	return cfg
}

func testRepo(t *testing.T) Repo {
	t.Helper()
	return core.Repo{Name: "my-app", Root: newReplicateTestRepo(t, map[string]string{"README.md": "ok\n"})}
}

func rawJSON(value string) json.RawMessage {
	return json.RawMessage(value)
}
