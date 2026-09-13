//go:build !windows

package cli

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
	"time"
)

type execTestProvider struct {
	runEnvProfileTestProvider
	backend *execTestBackend
}

func (p execTestProvider) Name() string { return "exec-command-test" }
func (p execTestProvider) Spec() ProviderSpec {
	spec := p.runEnvProfileTestProvider.Spec()
	spec.Name = p.Name()
	spec.Features = append(spec.Features, FeatureClaimExec)
	return spec
}
func (p execTestProvider) Configure(_ Config, rt Runtime) (Backend, error) {
	p.backend.configureCalls++
	p.backend.output = rt.Stdout
	return p.backend, nil
}

type execTestBackend struct {
	runEnvProfileTestBackend
	lease                            LeaseTarget
	resolveCalls                     int
	configureCalls                   int
	activityStarted, activityStopped bool
	output                           io.Writer
	prepareMessage                   string
}

func (b *execTestBackend) ResolveExecLeaseUnderClaim(_ context.Context, _ ResolveRequest, _ LeaseClaim) (LeaseTarget, error) {
	b.resolveCalls++
	if b.prepareMessage != "" {
		_, _ = io.WriteString(b.output, b.prepareMessage)
	}
	return b.lease, nil
}
func (b *execTestBackend) BeginSSHRunActivity(context.Context, LeaseTarget) (func(), error) {
	b.activityStarted = true
	return func() { b.activityStopped = true }, nil
}

func setupExecCommand(t *testing.T) (*execTestBackend, string) {
	t.Helper()
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	isolateRunTestUserDirs(t, dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	// Configuration inspection uses the real parser; only the final synthetic
	// command runs locally. No network connection is made by this fixture.
	script := `#!/bin/sh
for arg do
  if [ "$arg" = -G ]; then exec /usr/bin/ssh "$@"; fi
  if [ "$arg" = -tt ]; then export CRABBOX_TEST_REMOTE_PTY=requested; fi
done
printf '%s\n' "$@" > "$CRABBOX_EXEC_ARGS"
for arg do command="$arg"; done
exec /bin/sh -c "$command"
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_EXEC_ARGS", filepath.Join(dir, "args"))
	b := &execTestBackend{}
	p := execTestProvider{backend: b}
	b.spec = p.Spec()
	RegisterProvider(p)
	t.Cleanup(func() { delete(providerRegistry, p.Name()) })
	b.lease = LeaseTarget{LeaseID: "cbx_123456789abc", Server: Server{Provider: p.Name(), CloudID: "synthetic-resource", Labels: map[string]string{"lease": "cbx_123456789abc", "provider": p.Name(), "state": "ready"}}, SSH: SSHTarget{User: "synthetic-token", Host: "fixture.invalid", Port: "22", TargetOS: targetLinux, AuthSecret: true}}
	cfg := baseConfig()
	cfg.Provider = p.Name()
	if err := ClaimLeaseTargetForRepoConfig(b.lease.LeaseID, "exec-fixture", cfg, b.lease.Server, b.lease.SSH, dir, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	return b, dir
}

func TestExecCommandPreservesStreamsArgumentsAndExit(t *testing.T) {
	b, dir := setupExecCommand(t)
	b.prepareMessage = "provider preparation\n"
	var out, diagnostic bytes.Buffer
	input := []byte{'a', 0, 'b', '\n', 255}
	err := (App{Stdin: bytes.NewReader(input), Stdout: &out, Stderr: &diagnostic}).Run(t.Context(), []string{"exec", "--id", b.lease.LeaseID, "--", "sh", "-c", "cat; printf '%s' \"$1\"; printf 'command-error' >&2; exit 23", "sh", "literal ; $(ignored) ' argument"})
	var commandExit ExitError
	if !AsExitError(err, &commandExit) || commandExit.Code != 23 || commandExit.Message != "" {
		t.Fatalf("exit=%#v", err)
	}
	want := append(append([]byte{}, input...), []byte("literal ; $(ignored) ' argument")...)
	if !bytes.Equal(out.Bytes(), want) || diagnostic.String() != "provider preparation\ncommand-error" {
		t.Fatalf("stdout=%q stderr=%q", out.Bytes(), diagnostic.String())
	}
	if !b.activityStarted || !b.activityStopped || b.resolveCalls != 1 {
		t.Fatal("execution did not resolve and finalize provider activity")
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), b.lease.SSH.User) {
		t.Fatal("token escaped onto SSH argv")
	}
	lines := strings.Split(string(args), "\n")
	if len(lines) < 2 || lines[0] != "-F" {
		t.Fatalf("private SSH config missing: %q", args)
	}
	if _, err := os.Stat(lines[1]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private config retained: %v", err)
	}
}

func TestExecCommandRejectsPreviousRepositoryBeforeResolution(t *testing.T) {
	b, dir := setupExecCommand(t)
	current := filepath.Join(dir, "current-owner")
	if err := os.Mkdir(current, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := mutateLeaseClaim(b.lease.LeaseID, func(claim *leaseClaim) error { claim.RepoRoot = current; return nil }); err != nil {
		t.Fatal(err)
	}
	app := App{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}
	args := []string{"exec", "--id", b.lease.LeaseID, "--", "true"}
	if err := app.Run(t.Context(), args); err == nil || !strings.Contains(err.Error(), "claimed by") {
		t.Fatalf("old owner error=%v", err)
	}
	if b.resolveCalls != 0 {
		t.Fatal("old repository reached native resolution")
	}
	t.Chdir(current)
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("current owner denied: %v", err)
	}
	if b.resolveCalls != 1 {
		t.Fatal("current owner did not execute")
	}
}

func TestExecCommandHoldsClaimThroughCancellationAndStreamCompletion(t *testing.T) {
	b, dir := setupExecCommand(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	app := App{Stdin: strings.NewReader(""), Stdout: writer, Stderr: io.Discard}
	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx, []string{"exec", "--id", b.lease.LeaseID, "--", "sh", "-c", "printf ready; sleep 60"})
		writer.Close()
	}()
	marker := make([]byte, 5)
	if _, err := io.ReadFull(reader, marker); err != nil || string(marker) != "ready" {
		cancel()
		<-done
		t.Fatalf("no live output: %q %v", marker, err)
	}
	claim, err := ReadLeaseClaim(b.lease.LeaseID)
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	waitCtx, stopWait := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err = withLeaseClaimUnchangedContext(waitCtx, b.lease.LeaseID, claim, false, func() error { return nil })
	stopWait()
	if !errors.Is(err, context.DeadlineExceeded) {
		cancel()
		<-done
		t.Fatalf("claim writer was admitted during command: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	if !b.activityStopped {
		t.Fatal("activity survived cancellation")
	}
	if err := WithLeaseClaimUnchanged(b.lease.LeaseID, claim, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	config := strings.Split(string(args), "\n")[1]
	if _, err := os.Stat(config); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private session survived cancellation")
	}
}

func TestExecCommandPTYAndRejectedAdmissions(t *testing.T) {
	for _, test := range []struct {
		name            string
		args            []string
		change          func(*execTestBackend)
		wantError       string
		beforeConfigure bool
	}{
		{name: "remote pty", args: []string{"--pty"}},
		{name: "resource changed", change: func(b *execTestBackend) { b.lease.Server.CloudID = "different-resource" }, wantError: "outside its original claim"},
		{name: "windows target", change: func(b *execTestBackend) { b.lease.SSH.TargetOS = targetWindows }, wantError: "requires a Linux or macOS"},
		{name: "reclaim unsupported", args: []string{"--reclaim"}, wantError: "flag provided but not defined"},
		{name: "unsupported requested target", args: []string{"--target", "macos"}, wantError: "does not support claim-fenced SSH execution", beforeConfigure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, dir := setupExecCommand(t)
			if test.change != nil {
				test.change(b)
			}
			args := append([]string{"exec", "--id", b.lease.LeaseID}, test.args...)
			args = append(args, "--", "sh", "-c", "printf '%s' \"$CRABBOX_TEST_REMOTE_PTY\"")
			var out, diagnostic bytes.Buffer
			err := (App{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &diagnostic}).Run(t.Context(), args)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error=%v", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "args")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("rejected admission reached SSH")
				}
				if test.beforeConfigure && b.configureCalls != 0 {
					t.Fatal("unsupported target initialized a provider backend")
				}
				return
			}
			if err != nil || out.String() != "requested" {
				t.Fatalf("pty result=%q error=%v", out.String(), err)
			}
		})
	}
}

func TestExecCommandHelpDoesNotResolveConfiguration(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "unavailable.yaml"))
	var out bytes.Buffer
	err := (App{Stdout: &out, Stderr: &out}).Run(t.Context(), []string{"exec", "--help"})
	if err != nil || !strings.Contains(out.String(), "canonical lease id") || !strings.Contains(out.String(), "pseudo-terminal") {
		t.Fatalf("help=%q err=%v", out.String(), err)
	}
}

func TestExecCheckAndUnsupportedScopedStopStayOffline(t *testing.T) {
	b, _ := setupExecCommand(t)
	for _, target := range []string{targetLinux, targetWindows} {
		var out bytes.Buffer
		app := App{Stdout: &out, Stderr: io.Discard}
		if err := app.Run(t.Context(), []string{"exec", "--check", "--provider", b.spec.Name, "--target", target}); err != nil {
			t.Fatal(err)
		}
		var capabilities execCapabilities
		if err := json.Unmarshal(out.Bytes(), &capabilities); err != nil {
			t.Fatal(err)
		}
		if capabilities.Provider != b.spec.Name || capabilities.Target != target || capabilities.Execution != (target == targetLinux) || capabilities.CurrentRepoStop {
			t.Fatalf("capabilities=%+v", capabilities)
		}
	}
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).Run(t.Context(), []string{"stop", "--current-repo", "--id", b.lease.LeaseID})
	if err == nil || !strings.Contains(err.Error(), "does not support stop --current-repo") {
		t.Fatalf("unsupported scoped stop=%v", err)
	}
	if b.configureCalls != 0 || b.resolveCalls != 0 {
		t.Fatal("offline capability admission initialized a backend")
	}
}
