package applemachine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	errs      map[string]error
	fallback  core.LocalCommandResult
	err       error
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.requests = append(r.requests, req)
	key := strings.Join(req.Args, "\x00")
	result, ok := r.responses[key]
	if !ok {
		result = r.fallback
	}
	if req.Stdout != nil {
		_, _ = io.WriteString(req.Stdout, result.Stdout)
	}
	if req.Stderr != nil {
		_, _ = io.WriteString(req.Stderr, result.Stderr)
	}
	if err := r.errs[key]; err != nil {
		return result, err
	}
	return result, r.err
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
	var stderr bytes.Buffer
	b := newBackend(Provider{}.Spec(), testBackend(runner).cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: &stderr}).(*backend)
	result, err := b.Run(t.Context(), RunRequest{
		Repo:       Repo{Root: repo},
		ID:         leaseID,
		Keep:       true,
		TimingJSON: true,
		Command:    []string{"go", "test", "./..."},
		Env:        map[string]string{"CI": "1", "PATH": "/opt/homebrew/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LeaseID != leaseID || !result.SyncDelegated {
		t.Fatalf("result=%+v", result)
	}
	if result.Session == nil || result.Session.Provider != providerName || result.Session.LeaseID != leaseID || result.Session.Slug != slug || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if result.Session.CleanupCommand != "crabbox stop --provider apple-machine --id "+shellQuote(leaseID) {
		t.Fatalf("cleanup command=%q", result.Session.CleanupCommand)
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
	report := decodeLastTimingReport(t, stderr.String())
	if report.RunStatus != "succeeded" || report.ErrorKind != "" {
		t.Fatalf("timing outcome status=%q kind=%q", report.RunStatus, report.ErrorKind)
	}
}

func TestRunTimingJSONClassifiesCommandFailure(t *testing.T) {
	originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "darwin", "arm64"
	t.Cleanup(func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH })
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "src", "example")
	leaseID := "cbx_abcdef123456"
	slug := "failed-command"
	if err := claimLease(leaseID, slug, repo, 0, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeLeaseClaim(leaseID) })
	var stderr bytes.Buffer
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
		fallback:  core.LocalCommandResult{ExitCode: 9, Stderr: "boom"},
		err:       errors.New("exit status 9"),
	}
	b := newBackend(Provider{}.Spec(), testBackend(runner).cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: &stderr}).(*backend)
	result, err := b.Run(t.Context(), RunRequest{
		Repo:       Repo{Root: repo},
		ID:         leaseID,
		Keep:       true,
		TimingJSON: true,
		Command:    []string{"false"},
	})
	if err == nil || result.ExitCode != 9 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.Session == nil || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v, want retained failed reused lease", result.Session)
	}
	report := decodeLastTimingReport(t, stderr.String())
	if report.RunStatus != "failed" || report.ErrorKind != "command-exit" {
		t.Fatalf("timing outcome status=%q kind=%q", report.RunStatus, report.ErrorKind)
	}
}

func TestRunDeletesOneShotMachineSession(t *testing.T) {
	originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "darwin", "arm64"
	t.Cleanup(func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH })
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "src", "one-shot")
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := newBackend(Provider{}.Spec(), testBackend(runner).cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	result, err := b.Run(t.Context(), RunRequest{
		Repo:    Repo{Root: repo},
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || result.Session.Reused || result.Session.Kept || result.Session.CleanupCommand == "" {
		t.Fatalf("session=%#v, want cleaned one-shot handle", result.Session)
	}
	args := []string{}
	for _, req := range runner.requests {
		args = append(args, strings.Join(req.Args, " "))
	}
	if !strings.Contains(strings.Join(args, "\n"), "machine rm crabbox-") {
		t.Fatalf("remove command not recorded: %v", args)
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

func decodeLastTimingReport(t *testing.T, output string) timingReport {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		start := strings.Index(line, "{")
		if start < 0 {
			continue
		}
		var report timingReport
		if err := json.Unmarshal([]byte(line[start:]), &report); err != nil {
			t.Fatalf("timing json: %v\noutput=%s", err, output)
		}
		return report
	}
	t.Fatalf("output does not contain timing JSON: %s", output)
	return timingReport{}
}
