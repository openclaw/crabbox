package applemachine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	requests  []core.LocalCommandRequest
	responses map[string]core.LocalCommandResult
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.requests = append(r.requests, req)
	result := r.responses[strings.Join(req.Args, "\x00")]
	if req.Stdout != nil {
		_, _ = io.WriteString(req.Stdout, result.Stdout)
	}
	if req.Stderr != nil {
		_, _ = io.WriteString(req.Stderr, result.Stderr)
	}
	return result, nil
}

func testBackend(runner *recordingRunner) *backend {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleContainer.Image = "ubuntu:26.04"
	cfg.AppleContainer.CPUs = 4
	cfg.AppleContainer.Memory = "8G"
	return newBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*backend)
}

func TestCreateMachineArgs(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	if err := b.createMachine(t.Context(), "crabbox-test"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.requests[0].Args, " ")
	want := "machine create --name crabbox-test --home-mount rw --cpus 4 --memory 8G ubuntu:26.04"
	if got != want {
		t.Fatalf("args=%q want %q", got, want)
	}
}

func TestInspectMachineDecodesAppleJSON(t *testing.T) {
	args := []string{"machine", "inspect", "crabbox-test"}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		strings.Join(args, "\x00"): {Stdout: `[{"id":"crabbox-test","status":"running","ipAddress":"192.0.2.4","cpus":4,"memory":8589934592}]`},
	}}
	item, err := testBackend(runner).inspectMachine(t.Context(), "crabbox-test")
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != "crabbox-test" || item.Status != "running" || item.CPUs != 4 {
		t.Fatalf("machine=%+v", item)
	}
}

func TestRunUsesHomeMountedRepoAndEnv(t *testing.T) {
	originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "darwin", "arm64"
	t.Cleanup(func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH })
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "src", "example")
	leaseID := "cbx_123456789abc"
	slug := "blue-crab"
	if err := claimLease(leaseID, slug, repo, 0, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeLeaseClaim(leaseID) })
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	result, err := b.Run(t.Context(), RunRequest{
		Repo:    Repo{Root: repo},
		ID:      leaseID,
		Keep:    true,
		Command: []string{"go", "test", "./..."},
		Env:     map[string]string{"CI": "1", "PATH": "/opt/homebrew/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LeaseID != leaseID || !result.SyncDelegated {
		t.Fatalf("result=%+v", result)
	}
	got := strings.Join(runner.requests[0].Args, " ")
	if !strings.Contains(got, "machine run --name crabbox-123456789abc --cwd "+repo+" --env-file ") || !strings.HasSuffix(got, " go test ./...") {
		t.Fatalf("args=%q", got)
	}
	if strings.Contains(got, "CI=1") || strings.Contains(got, "/opt/homebrew/bin") {
		t.Fatalf("environment leaked into argv: %q", got)
	}
	if runner.requests[0].Dir != repo {
		t.Fatalf("dir=%q", runner.requests[0].Dir)
	}
}

func TestValidateRepoMountRejectsOutsideHome(t *testing.T) {
	if err := validateRepoMount("/var/tmp/example"); err == nil || !strings.Contains(err.Error(), "requires the repository under") {
		t.Fatalf("err=%v", err)
	}
}

func TestWriteEnvFileRejectsExplicitHostOwnedVariable(t *testing.T) {
	_, _, err := writeEnvFile(map[string]string{"PATH": "/custom/bin"}, []string{"PATH"})
	if err == nil || !strings.Contains(err.Error(), "cannot forward host-owned") {
		t.Fatalf("err=%v", err)
	}
}
