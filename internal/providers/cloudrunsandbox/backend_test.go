package cloudrunsandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCloudRunSandboxCreateTimeoutRetainsRecoveryClaim(t *testing.T) {
	isolateLeaseHome(t)
	var sandboxID string
	createErr := context.DeadlineExceeded
	transport := &fakeTransport{
		mode: "remote",
		onCreate: func(id string) error {
			sandboxID = id
			return createErr
		},
	}
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)

	_, _, _, err := b.createSandbox(context.Background(), transport, Repo{Root: t.TempDir()}, false, "timeout-recovery")
	if !errors.Is(err, createErr) || !strings.Contains(err.Error(), "recovery claim retained") {
		t.Fatalf("create error=%v, want indeterminate recovery", err)
	}
	if sandboxID == "" {
		t.Fatal("create did not receive a sandbox id")
	}
	claim, readErr := readLeaseClaim(leasePrefix + sandboxID)
	if readErr != nil {
		t.Fatalf("read recovery claim: %v", readErr)
	}
	if claim.LeaseID != leasePrefix+sandboxID || claim.Provider != providerName || claim.Slug != "timeout-recovery" {
		t.Fatalf("recovery claim=%#v", claim)
	}
}

func TestCloudRunSandboxCleanupSkipsClaimReclaimedAfterSnapshot(t *testing.T) {
	isolateLeaseHome(t)
	const sandboxID = "crabbox-reclaimed-123456"
	leaseID := leasePrefix + sandboxID
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, "reclaimed", providerName, scope, "", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	newRepo := t.TempDir()
	if err := claimLeaseForRepoProviderScopePond(leaseID, "reclaimed", providerName, scope, "", newRepo, time.Minute, true); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	destroyed := false
	transport := &fakeTransport{mode: "remote", onDestroy: func(string) error {
		destroyed = true
		return nil
	}}

	removed, err := b.destroyClaimedSandboxIfUnchanged(context.Background(), transport, sandboxID, snapshot)
	if err != nil {
		t.Fatalf("cleanup stale candidate: %v", err)
	}
	if removed {
		t.Fatal("cleanup reported a stale candidate removed")
	}
	if destroyed {
		t.Fatal("cleanup destroyed a sandbox reclaimed after its snapshot")
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil || claim.RepoRoot != newRepo {
		t.Fatalf("reclaimed claim not preserved: claim=%#v err=%v", claim, err)
	}
}

func TestCloudRunSandboxCleanupDestroyFailureRetainsClaim(t *testing.T) {
	isolateLeaseHome(t)
	const sandboxID = "crabbox-destroy-failure-123456"
	leaseID := leasePrefix + sandboxID
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Minute,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, "destroy-failure", providerName, scope, "", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	destroyErr := errors.New("gateway unavailable")
	transport := &fakeTransport{mode: "remote", onDestroy: func(string) error { return destroyErr }}

	removed, err := b.destroyClaimedSandboxIfUnchanged(context.Background(), transport, sandboxID, claim)
	if err != nil {
		t.Fatalf("cleanup destroy failure: %v", err)
	}
	if removed {
		t.Fatal("cleanup reported a failed destroy removed")
	}
	retained, err := readLeaseClaim(leaseID)
	if err != nil || retained.LeaseID != leaseID {
		t.Fatalf("failed destroy lost claim: claim=%#v err=%v", retained, err)
	}
}

func TestCloudRunSandboxReclaimCannotRepublishAfterCleanupWins(t *testing.T) {
	isolateLeaseHome(t)
	const sandboxID = "crabbox-cleanup-wins-123456"
	leaseID := leasePrefix + sandboxID
	destroyStarted := make(chan struct{})
	allowDestroy := make(chan struct{})
	transport := &fakeTransport{mode: "remote", onDestroy: func(string) error {
		close(destroyStarted)
		<-allowDestroy
		return nil
	}}
	previousTransport := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return transport, nil }
	t.Cleanup(func() { newTransport = previousTransport })

	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir},
		IdleTimeout:     time.Second,
	}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	scope, err := b.claimScope()
	if err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, "cleanup-wins", providerName, scope, "", t.TempDir(), time.Second, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	expired := claim
	expired.LastUsedAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if err := core.ReplaceLeaseClaimIfUnchanged(leaseID, claim, expired); err != nil {
		t.Fatalf("expire claim: %v", err)
	}

	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- b.Cleanup(context.Background(), CleanupRequest{}) }()
	<-destroyStarted

	reclaimDone := make(chan error, 1)
	newRepo := t.TempDir()
	go func() {
		_, _, _, resolveErr := b.resolveLeaseID(leaseID, newRepo, true)
		reclaimDone <- resolveErr
	}()
	select {
	case reclaimErr := <-reclaimDone:
		t.Fatalf("reclaim bypassed cleanup claim lock: %v", reclaimErr)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowDestroy)
	if err := <-cleanupDone; err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if err := <-reclaimDone; err == nil || (!strings.Contains(err.Error(), "claim changed") && !strings.Contains(err.Error(), "not claimed by Crabbox")) {
		t.Fatalf("reclaim error=%v, want guarded missing-claim failure", err)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("reclaim republished deleted sandbox claim: exists=%v err=%v", exists, err)
	}
}

func TestProviderSpec(t *testing.T) {
	t.Parallel()
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("name=%q", spec.Name)
	}
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v", spec.Kind)
	}
	for _, feature := range []core.Feature{core.FeatureArchiveSync, core.FeatureRunSession, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("missing feature %s in %v", feature, spec.Features)
		}
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v", spec.Coordinator)
	}
}

func TestRunWithFakeTransport(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	fake := &fakeTransport{
		mode: "remote",
		onCreate: func(id string) error {
			mu.Lock()
			calls = append(calls, "create:"+id)
			mu.Unlock()
			return nil
		},
		onExec: func(_ string, command string) (int, string, string, error) {
			mu.Lock()
			calls = append(calls, "exec:"+command)
			mu.Unlock()
			if strings.Contains(command, "mkdir") {
				return 0, "", "", nil
			}
			return 0, "hello\n", "", nil
		},
		onDestroy: func(id string) error {
			mu.Lock()
			calls = append(calls, "destroy:"+id)
			mu.Unlock()
			return nil
		},
	}
	prev := newTransport
	newTransport = func(Config, Runtime) (sandboxTransport, error) { return fake, nil }
	t.Cleanup(func() { newTransport = prev })

	// Isolate lease claims to a temp dir.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	b := NewBackend(Provider{}.Spec(), Config{
		CloudRunSandbox: CloudRunSandboxConfig{
			CLIPath: defaultCLIPath,
			Workdir: defaultWorkdir,
			Write:   true,
		},
		IdleTimeout: 30 * time.Minute,
	}, Runtime{
		Stdout: &stdout,
		Stderr: &stderr,
	}).(*backend)

	result, err := b.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: root},
		Command: []string{"echo", "hello"},
		NoSync:  true,
		Keep:    false,
	})
	if err != nil {
		t.Fatalf("Run: %v\nstderr=%s", err, stderr.String())
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d", result.ExitCode)
	}
	if !strings.Contains(stdout.String(), "hello") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(calls, ",")
	if !strings.Contains(joined, "create:") || !strings.Contains(joined, "exec:") || !strings.Contains(joined, "destroy:") {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()
	cfg := Config{CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath, Workdir: defaultWorkdir}}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cfg.CloudRunSandbox.Workdir = "relative"
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected relative workdir rejection")
	}
	cfg.CloudRunSandbox.Workdir = defaultWorkdir
	cfg.CloudRunSandbox.Mode = "weird"
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected invalid mode rejection")
	}
	cfg.CloudRunSandbox.Mode = "local"
	cfg.CloudRunSandbox.CLIPath = ""
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected empty cliPath rejection")
	}
}

type fakeTransport struct {
	mode      string
	onCreate  func(string) error
	onExec    func(string, string) (int, string, string, error)
	onDestroy func(string) error
	onWrite   func(string, string) error
}

func (f *fakeTransport) Mode() string                 { return f.mode }
func (f *fakeTransport) Health(context.Context) error { return nil }
func (f *fakeTransport) Create(_ context.Context, sandboxID string, _ runOptions) error {
	if f.onCreate != nil {
		return f.onCreate(sandboxID)
	}
	return nil
}
func (f *fakeTransport) Exec(_ context.Context, sandboxID, command string, _ execOptions, stdout, stderr io.Writer) (int, error) {
	if f.onExec != nil {
		code, out, errOut, err := f.onExec(sandboxID, command)
		if stdout != nil && out != "" {
			_, _ = io.WriteString(stdout, out)
		}
		if stderr != nil && errOut != "" {
			_, _ = io.WriteString(stderr, errOut)
		}
		return code, err
	}
	return 0, nil
}
func (f *fakeTransport) Destroy(_ context.Context, sandboxID string) error {
	if f.onDestroy != nil {
		return f.onDestroy(sandboxID)
	}
	return nil
}
func (f *fakeTransport) WriteFile(_ context.Context, sandboxID, path, _ string) error {
	if f.onWrite != nil {
		return f.onWrite(sandboxID, path)
	}
	return nil
}
