package windowssandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSandboxSessionOwnership(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		powershell, err = exec.LookPath("pwsh")
		if err != nil {
			t.Skip("PowerShell not available")
		}
	}
	dir := t.TempDir()
	host := filepath.Join(dir, "host.txt")
	fallback := filepath.Join(dir, "fallback.txt")
	for path, text := range map[string]string{host: hostRunnerScript(), fallback: canceledSandboxScript("fake-session.json")} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-File", "testdata/session-ownership.ps1", "-HostSource", host, "-FallbackSource", fallback).CombinedOutput()
	if err != nil {
		t.Fatalf("inert ownership regression: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}

type ownershipCancellationRunner struct {
	started       chan struct{}
	mu            sync.Mutex
	controlFails  bool
	unconfirmed   bool
	fallbackCalls int
	identityPath  string
}

func commandArg(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func (r *ownershipCancellationRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if commandArg(req.Args, "-File") != "" {
		r.identityPath = commandArg(req.Args, "-IdentityPath")
		if r.identityPath == "" {
			return core.LocalCommandResult{}, errors.New("missing identity path")
		}
		if r.controlFails {
			control := commandArg(req.Args, "-ControlDir")
			if err := os.Rename(control, control+".moved"); err != nil {
				return core.LocalCommandResult{}, err
			}
		}
		close(r.started)
		<-ctx.Done()
		return core.LocalCommandResult{}, ctx.Err()
	}
	if commandArg(req.Args, "-Command") != canceledSandboxScript(r.identityPath) {
		return core.LocalCommandResult{}, fmt.Errorf("unexpected inert cleanup request")
	}
	r.mu.Lock()
	r.fallbackCalls++
	r.mu.Unlock()
	if r.unconfirmed {
		return core.LocalCommandResult{}, os.WriteFile(r.identityPath+".unconfirmed", []byte("unconfirmed"), 0o600)
	}
	return core.LocalCommandResult{}, nil
}

func TestRunOwnedSessionCancellationFallbacks(t *testing.T) {
	oldOS := windowsSandboxHostOS
	windowsSandboxHostOS = "windows"
	t.Cleanup(func() { windowsSandboxHostOS = oldOS })
	for _, controlFails := range []bool{true, false} {
		for _, policy := range []string{"none", "keep", "keep-on-failure", "unconfirmed"} {
			t.Run(fmt.Sprintf("sentinel-failure=%t/%s", controlFails, policy), func(t *testing.T) {
				t.Parallel()
				runner := &ownershipCancellationRunner{started: make(chan struct{}), controlFails: controlFails, unconfirmed: policy == "unconfirmed"}
				cfg := core.BaseConfig()
				cfg.WindowsSandbox.TempRoot = t.TempDir()
				be := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan localCommandOutcome, 1)
				go func() {
					result, err := be.Run(ctx, core.RunRequest{NoSync: true, Command: []string{"inert"}, Keep: policy == "keep", KeepOnFailure: policy == "keep-on-failure"})
					done <- localCommandOutcome{result: core.LocalCommandResult{ExitCode: result.ExitCode}, err: err}
				}()
				select {
				case <-runner.started:
				case got := <-done:
					t.Fatalf("Run returned before fake launch: %v", got.err)
				case <-time.After(10 * time.Second):
					t.Fatal("fake runner did not start")
				}
				cancel()
				select {
				case got := <-done:
					if got.result.ExitCode != 130 || got.err == nil {
						t.Fatalf("exit=%d err=%v", got.result.ExitCode, got.err)
					}
				case <-time.After(40 * time.Second):
					t.Fatal("cancellation did not complete")
				}
				want := 0
				if policy == "none" || policy == "unconfirmed" {
					want = 1
				}
				runner.mu.Lock()
				calls := runner.fallbackCalls
				runner.mu.Unlock()
				if calls != want {
					t.Fatalf("fallback calls=%d want=%d", calls, want)
				}
				_, statErr := os.Stat(filepath.Dir(runner.identityPath))
				if (statErr == nil) != (policy != "none") {
					t.Fatalf("workspace preservation policy=%s: %v", policy, statErr)
				}
			})
		}
	}
}
