package cloudflare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

type fakeSnapshotRunner struct {
	mu        sync.Mutex
	server    *httptest.Server
	creates   []createSandboxRequest
	snapshots []string
}

func newFakeSnapshotRunner(t *testing.T) *fakeSnapshotRunner {
	t.Helper()
	f := &fakeSnapshotRunner{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var req createSandboxRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode create: %v", err)
			}
			f.creates = append(f.creates, req)
			_, _ = fmt.Fprintf(w, `{"id":%q,"state":"running","workdir":%q,"instanceType":%q,"snapshotId":%q}`, req.ID, req.Workdir, req.InstanceType, req.SnapshotID)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/")
			_, _ = fmt.Fprintf(w, `{"id":%q,"state":"running","workdir":"/workspace/app","instanceType":"standard-1"}`, id)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/snapshots"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), "/snapshots")
			var body struct{ Name string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode snapshot: %v", err)
			}
			f.snapshots = append(f.snapshots, body.Name)
			_, _ = fmt.Fprintf(w, `{"id":"snap-1","size":4096,"name":%q,"leaseId":%q,"image":"default","instanceType":"standard-1","workdir":"/workspace/app"}`, body.Name, id)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func checkpointTestRepo(t *testing.T) string {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://example.com/example-org/my-app.git"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return repo
}

func TestCloudflareCheckpointCreateAndForkThroughCLI(t *testing.T) {
	testutil.IsolateUserDirs(t)
	runner := newFakeSnapshotRunner(t)
	repo := checkpointTestRepo(t)
	t.Chdir(repo)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	config := fmt.Sprintf("provider: cloudflare\ncloudflare:\n  apiUrl: %s\n", runner.server.URL)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_CLOUDFLARE_RUNNER_TOKEN", "runner-token")
	source, err := core.ClaimLeaseForRepoProviderScopePondWithLabels("cbx_0123456789ab", "blue-lobster", providerName, "", "", repo, time.Hour, map[string]string{"instance_type": "standard-1"})
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := core.App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(t.Context(), []string{"checkpoint", "create", "--provider", "cloudflare", "--id", source.LeaseID, "--reclaim"}); err == nil || !strings.Contains(err.Error(), "--reclaim is not supported") {
		t.Fatalf("create --reclaim error = %v", err)
	}
	for _, args := range [][]string{{"--strategy", "image"}, {"--mode", "image"}} {
		err := app.Run(t.Context(), append([]string{"checkpoint", "create", "--provider", "cloudflare", "--id", source.LeaseID}, args...))
		if err == nil || !strings.Contains(err.Error(), "unsupported") || len(runner.snapshots) != 0 {
			t.Fatalf("create %v error = %v snapshots=%v", args, err, runner.snapshots)
		}
	}
	stdout.Reset()
	if err := app.Run(t.Context(), []string{"checkpoint", "create", "--provider", "cloudflare", "--id", source.LeaseID, "--json"}); err != nil {
		t.Fatalf("create: %v %s", err, stderr.String())
	}
	var record struct {
		ID      string
		Kind    string
		Workdir string
		Native  struct {
			ImageID  string
			Metadata map[string]string
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Kind != core.CheckpointKindCloudflare || record.Native.ImageID != "snap-1" || record.Workdir != "/workspace/app" {
		t.Fatalf("record = %+v", record)
	}
	if record.Native.Metadata["api_url"] != runner.server.URL || record.Native.Metadata["source"] != source.LeaseID {
		t.Fatalf("metadata = %#v", record.Native.Metadata)
	}
	if len(runner.snapshots) != 1 || runner.snapshots[0] != "crabbox-"+record.ID {
		t.Fatalf("snapshot names = %v", runner.snapshots)
	}

	stdout.Reset()
	if err := app.Run(t.Context(), []string{"checkpoint", "fork", record.ID, "--type", "standard-2", "--slug", "coral-crab"}); err != nil {
		t.Fatalf("fork: %v %s", err, stderr.String())
	}
	if len(runner.creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(runner.creates))
	}
	fork := runner.creates[0]
	if fork.SnapshotID != "snap-1" || fork.Workdir != "/workspace/app" || fork.InstanceType != "standard-2" {
		t.Fatalf("fork create = %+v", fork)
	}
	if !strings.Contains(stdout.String(), "checkpoint forked id="+record.ID+" lease="+fork.ID+" slug=coral-crab image=snap-1 workdir=/workspace/app") {
		t.Fatalf("fork output = %q", stdout.String())
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(fork.ID, providerName)
	if err != nil || !ok || claim.RepoRoot != repo {
		t.Fatalf("fork claim = %+v ok=%t err=%v", claim, ok, err)
	}

	if err := app.Run(t.Context(), []string{"checkpoint", "delete", record.ID}); err == nil || !strings.Contains(err.Error(), "--local-only") {
		t.Fatalf("delete error = %v", err)
	}
	if err := app.Run(t.Context(), []string{"checkpoint", "delete", record.ID, "--local-only"}); err != nil {
		t.Fatal(err)
	}
}

func TestCloudflareCheckpointForkRejectsAnotherRunner(t *testing.T) {
	cfg := core.Config{Provider: providerName}
	cfg.Cloudflare.APIURL = "https://runner-b.example.com"
	cfg.Cloudflare.Token = "token"
	err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{
		Config: &cfg,
		Record: core.NativeCheckpointForkRecord{
			Kind: core.CheckpointKindCloudflare, ImageID: "snap-1", Direct: true,
			Metadata: map[string]string{"api_url": "https://runner-a.example.com"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "captured on runner https://runner-a.example.com") {
		t.Fatalf("fork config error = %v", err)
	}
}

func TestCloudflareCheckpointRejectsClaimReassignedAfterResolve(t *testing.T) {
	testutil.IsolateUserDirs(t)
	runner := newFakeSnapshotRunner(t)
	repo := checkpointTestRepo(t)
	source, err := core.ClaimLeaseForRepoProviderScopePondWithLabels("cbx_0123456789ab", "blue-lobster", providerName, "", "", repo, time.Hour, map[string]string{"instance_type": "standard-1"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.Config{Provider: providerName, TargetOS: core.TargetLinux}
	cfg.Cloudflare.APIURL = runner.server.URL
	cfg.Cloudflare.Token = "runner-token"
	backend := NewCloudflareBackend(Provider{}.Spec(), cfg, core.Runtime{HTTP: runner.server.Client()}).(*cloudflareBackend)
	lease, err := backend.ResolveCheckpointSource(t.Context(), core.ResolveRequest{Repo: core.Repo{Root: repo}, ID: source.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseForRepoProvider(source.LeaseID, source.Slug, providerName, t.TempDir(), time.Hour, true); err != nil {
		t.Fatal(err)
	}

	_, err = (Provider{}).CreateNativeCheckpoint(t.Context(), core.NativeCheckpointCreateRequest{
		Config: cfg, Server: lease.Server, LeaseID: source.LeaseID, CheckpointID: "chk_0123456789abcdef",
	})

	if err == nil || !strings.Contains(err.Error(), "claim changed after it was resolved") {
		t.Fatalf("create error = %v", err)
	}
	if len(runner.snapshots) != 0 {
		t.Fatalf("snapshot requests = %v, want none", runner.snapshots)
	}
}
