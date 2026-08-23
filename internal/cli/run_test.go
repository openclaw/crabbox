package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func init() {
	RegisterProvider(windowsEnvHelperTestProvider{})
	RegisterProvider(runEnvProfileTestProvider{})
	RegisterProvider(runReadyPoolPreflightTestProvider{})
	RegisterProvider(runPrepareTestProvider{})
	RegisterProvider(runModuleRuntimeTestProvider{})
	RegisterProvider(runArchiveSyncPreflightTestProvider{})
}

type warmupFailureReleaseBackend struct {
	releases int
	resolves int
}

type ownershipChangedReleaseBackend struct {
	*warmupFailureReleaseBackend
}

func (b *ownershipChangedReleaseBackend) RefreshReleaseLeaseTarget(context.Context, LeaseTarget) (LeaseTarget, error) {
	return LeaseTarget{}, ErrReleaseLeaseOwnershipChanged
}

func (b *ownershipChangedReleaseBackend) ReleaseLeaseConnectionCleanupSafe() bool { return false }

func (b *warmupFailureReleaseBackend) Spec() ProviderSpec {
	return ProviderSpec{Name: "warmup-release-test"}
}
func (b *warmupFailureReleaseBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{}, nil
}
func (b *warmupFailureReleaseBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	b.resolves++
	return LeaseTarget{}, nil
}
func (b *warmupFailureReleaseBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b *warmupFailureReleaseBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	b.releases++
	return nil
}
func (b *warmupFailureReleaseBackend) Touch(context.Context, TouchRequest) (Server, error) {
	return Server{}, nil
}

func TestWarmupFailureLeavesAcknowledgedLeaseForControllerReleaseGate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // standalone cleanup stops egress daemons; keep lock files out of the real state dir
	backend := &warmupFailureReleaseBackend{}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	lease := LeaseTarget{LeaseID: "cbx_abcdef123456"}
	app.releaseWarmupLeaseAfterFailure(context.Background(), backend, defaultConfig(), lease, true)
	if backend.releases != 0 {
		t.Fatalf("controller-owned cleanup released provider directly: %d", backend.releases)
	}
	app.releaseWarmupLeaseAfterFailure(context.Background(), backend, defaultConfig(), lease, false)
	if backend.releases != 1 {
		t.Fatalf("standalone warmup cleanup releases=%d", backend.releases)
	}
	if backend.resolves != 0 {
		t.Fatalf("standalone warmup cleanup resolves=%d, want no implicit preparation", backend.resolves)
	}
}

func TestReleaseBackendLeaseStopsBeforeCleanupAfterOwnershipChange(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // same release path; only the ownership-change early return keeps it off the state dir
	base := &warmupFailureReleaseBackend{}
	backend := &ownershipChangedReleaseBackend{warmupFailureReleaseBackend: base}
	lease := LeaseTarget{LeaseID: "cbx_abcdef123456", SSH: SSHTarget{Host: "192.0.2.70"}}
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).releaseBackendLeaseBestEffort(context.Background(), backend, defaultConfig(), lease)
	if !errors.Is(err, ErrReleaseLeaseOwnershipChanged) {
		t.Fatalf("err=%v, want ownership-change sentinel", err)
	}
	if base.releases != 0 {
		t.Fatalf("releases=%d, want no cleanup or release after ownership change", base.releases)
	}
}

func TestReleaseBackendLeaseBestEffortCleansMediatedEgressBeforeRelease(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	logPath := installRecordingSSH(t, dir)
	runEnvProfileTestReleaseHook = func() error {
		assertSSHLogContains(t, logPath, remoteStopEgressClientCommand())
		return nil
	}
	runEnvProfileTestReleaseRequestHook = func(req ReleaseLeaseRequest) error {
		if !req.DeferProviderCleanupObservation {
			return errors.New("automatic release did not defer provider cleanup observation")
		}
		return nil
	}
	t.Cleanup(func() {
		runEnvProfileTestReleaseHook = nil
		runEnvProfileTestReleaseRequestHook = nil
	})

	backend := runEnvProfileTestBackend{spec: runEnvProfileTestProvider{}.Spec()}
	lease := LeaseTarget{
		Server:  Server{Provider: "run-env-profile-test"},
		SSH:     SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetLinux},
		LeaseID: "cbx_env_profile_test",
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).releaseBackendLeaseBestEffort(context.Background(), backend, defaultConfig(), lease)
	if err != nil {
		t.Fatalf("releaseBackendLeaseBestEffort error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	assertSSHLogContains(t, logPath, remoteStopEgressClientCommand())
}

func TestReleaseCoordinatorLeaseHonorsCancellationDuringBackoff(t *testing.T) {
	// Prove cancel is observed during the inter-attempt backoff, not only at
	// the next release attempt. The coordinator always fails so release enters the sleep.
	var (
		mu    sync.Mutex
		calls int
		first = make(chan struct{})
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/release") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad", http.StatusNotFound)
			return
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			close(first)
		}
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- releaseCoordinatorLease(ctx, client, "cbx_123", "aws")
	}()

	select {
	case <-first:
		// First failed release completed and the retry sleep is next.
	case err := <-errCh:
		t.Fatalf("releaseCoordinatorLease returned before backoff: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("releaseCoordinatorLease did not reach the backoff")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("releaseCoordinatorLease returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("releaseCoordinatorLease did not return within 3s after cancel; still blocked on bare sleep")
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("release attempts=%d, want 1 (no retries after cancel)", calls)
	}
}

func TestStopCleansMediatedEgressBeforeRelease(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	logPath := installRecordingSSH(t, dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	for _, id := range []string{"friendly-slug", "cbx_env_profile_test"} {
		_, pidPath, err := egressDaemonPaths(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pidPath, []byte("99999999\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runEnvProfileTestReleaseHook = func() error {
		assertSSHLogContains(t, logPath, remoteStopEgressClientCommand())
		return nil
	}
	runEnvProfileTestReleaseRequestHook = func(req ReleaseLeaseRequest) error {
		if req.DeferProviderCleanupObservation {
			return errors.New("explicit stop deferred provider cleanup observation")
		}
		return nil
	}
	t.Cleanup(func() {
		runEnvProfileTestReleaseHook = nil
		runEnvProfileTestReleaseRequestHook = nil
	})

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).stop(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--id", "friendly-slug",
	})
	if err != nil {
		t.Fatalf("stop error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	assertSSHLogContains(t, logPath, remoteStopEgressClientCommand())
	for _, id := range []string{"friendly-slug", "cbx_env_profile_test"} {
		_, pidPath, err := egressDaemonPaths(id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
			t.Fatalf("egress pid path for %s still exists: %v", id, err)
		}
	}
}

func TestStopDefersUnsafeLocalConnectionCleanupUntilRelease(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	logPath := installRecordingSSH(t, dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	leaseID := "cbx_env_profile_test"
	requestedID := "friendly-slug"
	pidPaths := map[string]string{}
	for _, id := range []string{requestedID, leaseID} {
		_, pidPath, err := egressDaemonPaths(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pidPath, []byte("99999999\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		pidPaths[id] = pidPath
	}
	runEnvProfileTestConnectionCleanupSafe = false
	t.Cleanup(func() { runEnvProfileTestConnectionCleanupSafe = true })
	runEnvProfileTestReleaseHook = func() error {
		assertSSHLogContains(t, logPath, remoteStopEgressClientCommand())
		for id, pidPath := range pidPaths {
			if _, err := os.Stat(pidPath); err != nil {
				return fmt.Errorf("local connection cleanup for %s ran before guarded release", id)
			}
		}
		return nil
	}
	t.Cleanup(func() { runEnvProfileTestReleaseHook = nil })

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).stop(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--id", requestedID,
	})
	if err != nil {
		t.Fatalf("stop error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	for id, pidPath := range pidPaths {
		if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
			t.Fatalf("egress pid path for %s still exists after release: %v", id, err)
		}
	}
}

func installRecordingSSH(t *testing.T, dir string) string {
	t.Helper()
	logPath := filepath.Join(dir, "ssh.log")
	sshPath := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
decoded=""
case "$cmd" in
  *'payload_b64="'*'"; decoded=; if command -v base64'*)
    payload_b64=${cmd#*'payload_b64="'}
    payload_b64=${payload_b64%%'"; decoded=; if command -v base64'*}
    decoded=$(printf %s "$payload_b64" | /usr/bin/base64 --decode 2>/dev/null) ||
      decoded=$(printf %s "$payload_b64" | /usr/bin/base64 -d 2>/dev/null) ||
      decoded=$(printf %s "$payload_b64" | /usr/bin/base64 -D 2>/dev/null) || decoded=""
    ;;
esac
printf '%s\n%s\n---\n' "$cmd" "$decoded" >> "$CRABBOX_FAKE_SSH_LOG"
match=$cmd
if [ -n "$decoded" ]; then match=$decoded; fi
case "$match" in
  *"protocol_action='acquire'"*) printf ACQUIRED; exit 0 ;;
  *"protocol_action='renew'"*) printf RENEWED; exit 0 ;;
  *"protocol_action='inspect'"*) printf OWNED; exit 0 ;;
  *"protocol_action='release'"*) printf RELEASED; exit 0 ;;
esac
if [ -n "${CRABBOX_FAKE_SSH_STDIN_LOG:-}" ]; then
  /bin/cat >> "$CRABBOX_FAKE_SSH_STDIN_LOG" || true
else
  /bin/cat >/dev/null || true
fi
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	return logPath
}

func installWorkspaceOwnerAwareSSH(t *testing.T, sshPath, commandScript string) {
	t.Helper()
	commandPath := filepath.Join(filepath.Dir(sshPath), "ssh-command")
	if err := os.WriteFile(commandPath, []byte(commandScript), 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper := `#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
current=$cmd
decode_depth=0
while [ "$decode_depth" -lt 8 ]; do
  case "$current" in
    *'payload_b64="'*'"; decoded=; if command -v base64'*)
      payload_b64=${current#*'payload_b64="'}
      payload_b64=${payload_b64%%'"; decoded=; if command -v base64'*}
      current=$(printf %s "$payload_b64" | /usr/bin/base64 --decode 2>/dev/null) ||
        current=$(printf %s "$payload_b64" | /usr/bin/base64 -d 2>/dev/null) ||
        current=$(printf %s "$payload_b64" | /usr/bin/base64 -D 2>/dev/null) || break
      decode_depth=$((decode_depth + 1))
      ;;
    *) break ;;
  esac
done
case "$current" in
  *"protocol_action='acquire'"*) printf ACQUIRED; exit 0 ;;
  *"protocol_action='renew'"*) printf RENEWED; exit 0 ;;
  *"protocol_action='inspect'"*)
    if [ -e "$(dirname "$0")/owner-child" ]; then printf CHILD; else printf OWNED; fi
    exit 0
    ;;
  *"protocol_action='release'"*) printf RELEASED; exit 0 ;;
  *"rsync-stop."*"phase_live="*) : > "$(dirname "$0")/owner-child"; printf '123\n'; exit 0 ;;
  *'touch "$HOME/.crabbox/workspace-owners/'*"rsync-stop."*) rm -f "$(dirname "$0")/owner-child"; exit 0 ;;
  *"kill -0"*"exit=unknown"*) printf 'exit=unknown\nno marker written\n'; exit 0 ;;
esac
exec "$(dirname "$0")/ssh-command" "$current"
`
	if err := os.WriteFile(sshPath, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertSSHLogContains(t *testing.T, logPath, want string) {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("ssh log missing %q:\n%s", want, data)
	}
}

func TestRunCommandInjectsReservedMetadataAcrossSSHCommandModes(t *testing.T) {
	tests := []struct {
		name string
		args func(string) []string
	}{
		{name: "argv", args: func(string) []string { return []string{"--", "env"} }},
		{name: "shell", args: func(string) []string { return []string{"--shell", "--", "env | sort"} }},
		{name: "script", args: func(script string) []string { return []string{"--script", script} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			logPath := installRecordingSSH(t, dir)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
			t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
			t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
			t.Setenv(runEnvLeaseID, "ambient-lease")
			t.Setenv(runEnvRunID, "ambient-run")
			t.Setenv(runEnvSlug, "ambient-slug")

			profile := filepath.Join(dir, "env.profile")
			if err := os.WriteFile(profile, []byte("CRABBOX_LEASE_ID=profile-lease\nCRABBOX_RUN_ID=profile-run\nCRABBOX_SLUG=profile-slug\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(dir, "proof.sh")
			if err := os.WriteFile(script, []byte("#!/bin/sh\nenv | sort\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{
				"--provider", "run-env-profile-test",
				"--no-sync",
				"--allow-env", strings.Join(reservedRunEnvNames, ","),
				"--env-from-profile", profile,
			}
			args = append(args, tt.args(script)...)
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), args); err != nil {
				t.Fatalf("run error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			logText := string(data)
			if !strings.Contains(logText, "CRABBOX_LEASE_ID='cbx_env_profile_test'") {
				t.Fatalf("lease metadata missing from SSH command:\n%s", logText)
			}
			if !strings.Contains(logText, "CRABBOX_SLUG=''") {
				t.Fatalf("empty slug metadata missing from SSH command:\n%s", logText)
			}
			runIDMatch := regexp.MustCompile(`CRABBOX_RUN_ID='(run_[a-f0-9]{12})'`).FindStringSubmatch(logText)
			if len(runIDMatch) != 2 {
				t.Fatalf("CLI-generated run metadata missing from SSH command:\n%s", logText)
			}
			if !strings.Contains(stderr.String(), "run="+runIDMatch[1]) {
				t.Fatalf("reported run ID differs from command metadata: run=%s stderr=%s", runIDMatch[1], stderr.String())
			}
			for _, forbidden := range []string{"ambient-lease", "ambient-run", "ambient-slug", "profile-lease", "profile-run", "profile-slug"} {
				if strings.Contains(logText, forbidden) {
					t.Fatalf("reserved metadata override %q reached SSH command:\n%s", forbidden, logText)
				}
			}
		})
	}
}

func TestRunCommandRetriesIdempotentRemoteWorkdirCreation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell ssh fixture")
	}
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	mkdirCallsPath := filepath.Join(dir, "mkdir-calls")
	firstMkdirPath := filepath.Join(dir, "first-mkdir")
	mkdirCommandPath := filepath.Join(dir, "mkdir-command")
	script := `#!/bin/sh
remote=""
for arg do remote="$arg"; done
case "$remote" in
  "mkdir -p "*)
    if [ ! -e "$CRABBOX_FAKE_SSH_MKDIR_COMMAND" ]; then
      printf '%s\n' "$remote" > "$CRABBOX_FAKE_SSH_MKDIR_COMMAND"
    fi
    IFS= read -r mkdir_command < "$CRABBOX_FAKE_SSH_MKDIR_COMMAND"
    if [ "$remote" = "$mkdir_command" ]; then
      printf 'call\n' >> "$CRABBOX_FAKE_SSH_MKDIR_CALLS"
      if [ ! -e "$CRABBOX_FAKE_SSH_FIRST_MKDIR" ]; then
        : > "$CRABBOX_FAKE_SSH_FIRST_MKDIR"
        exit 255
      fi
    fi
    ;;
esac
/bin/cat >/dev/null || true
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	t.Setenv("CRABBOX_FAKE_SSH_MKDIR_CALLS", mkdirCallsPath)
	t.Setenv("CRABBOX_FAKE_SSH_FIRST_MKDIR", firstMkdirPath)
	t.Setenv("CRABBOX_FAKE_SSH_MKDIR_COMMAND", mkdirCommandPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")

	var stdout, stderr bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--no-hydrate",
		"--",
		"true",
	}); err != nil {
		t.Fatalf("run error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	calls, err := os.ReadFile(mkdirCallsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(calls), "call\n"); got != 2 {
		t.Fatalf("remote mkdir calls=%d want 2", got)
	}
}

type windowsEnvHelperTestProvider struct{}

func (windowsEnvHelperTestProvider) Name() string { return "windows-env-helper-test" }
func (windowsEnvHelperTestProvider) Aliases() []string {
	return nil
}
func (windowsEnvHelperTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name: "windows-env-helper-test",
		Kind: ProviderKindSSHLease,
		Targets: []TargetSpec{
			{OS: targetWindows, WindowsMode: windowsModeNormal},
		},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorNever,
	}
}
func (windowsEnvHelperTestProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (windowsEnvHelperTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p windowsEnvHelperTestProvider) Configure(Config, Runtime) (Backend, error) {
	return windowsEnvHelperTestBackend{spec: p.Spec()}, nil
}

type windowsEnvHelperTestBackend struct {
	spec ProviderSpec
}

var windowsEnvHelperTestTouchCount int

func (b windowsEnvHelperTestBackend) Spec() ProviderSpec { return b.spec }
func (b windowsEnvHelperTestBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{
		Server: Server{Provider: b.spec.Name},
		SSH: SSHTarget{
			User:        "crabbox",
			Host:        "203.0.113.10",
			Port:        "22",
			TargetOS:    targetWindows,
			WindowsMode: windowsModeNormal,
		},
		LeaseID: "cbx_win",
	}, nil
}
func (b windowsEnvHelperTestBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return b.Acquire(context.Background(), AcquireRequest{})
}
func (b windowsEnvHelperTestBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b windowsEnvHelperTestBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return nil
}
func (b windowsEnvHelperTestBackend) Touch(context.Context, TouchRequest) (Server, error) {
	windowsEnvHelperTestTouchCount++
	return Server{Provider: b.spec.Name}, nil
}

type runEnvProfileTestProvider struct{}

func (runEnvProfileTestProvider) Name() string { return "run-env-profile-test" }
func (runEnvProfileTestProvider) Aliases() []string {
	return nil
}
func (runEnvProfileTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "run-env-profile-test",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorNever,
	}
}
func (runEnvProfileTestProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (runEnvProfileTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p runEnvProfileTestProvider) Configure(Config, Runtime) (Backend, error) {
	return runEnvProfileTestBackend{spec: p.Spec()}, nil
}

type runReadyPoolPreflightTestProvider struct{}

func (runReadyPoolPreflightTestProvider) Name() string { return "run-ready-pool-preflight-test" }
func (runReadyPoolPreflightTestProvider) Aliases() []string {
	return nil
}
func (runReadyPoolPreflightTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "run-ready-pool-preflight-test",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorSupported,
	}
}
func (runReadyPoolPreflightTestProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (runReadyPoolPreflightTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p runReadyPoolPreflightTestProvider) Configure(Config, Runtime) (Backend, error) {
	return runEnvProfileTestBackend{spec: p.Spec()}, nil
}

type runEnvProfileTestBackend struct {
	spec ProviderSpec
}

func (b runEnvProfileTestBackend) AcquireIsExclusiveOneShot() bool { return true }

var runEnvProfileTestReleaseErr error
var runEnvProfileTestReleaseHook func() error
var runEnvProfileTestReleaseRequestHook func(ReleaseLeaseRequest) error
var runEnvProfileTestConnectionCleanupSafe = true
var runEnvProfileTestAcquireHook func(AcquireRequest)
var runEnvProfileTestAcquireLease func(AcquireRequest) (LeaseTarget, error)
var runEnvProfileTestTouchHook func(TouchRequest) error

func (b runEnvProfileTestBackend) Spec() ProviderSpec { return b.spec }
func (b runEnvProfileTestBackend) Acquire(_ context.Context, req AcquireRequest) (LeaseTarget, error) {
	if runEnvProfileTestAcquireHook != nil {
		runEnvProfileTestAcquireHook(req)
	}
	if runEnvProfileTestAcquireLease != nil {
		return runEnvProfileTestAcquireLease(req)
	}
	return LeaseTarget{
		Server: Server{Provider: b.spec.Name},
		SSH: SSHTarget{
			User:           "crabbox",
			Host:           "127.0.0.1",
			Port:           os.Getenv("CRABBOX_FAKE_SSH_PORT"),
			TargetOS:       targetLinux,
			SSHConfigProxy: os.Getenv("CRABBOX_FAKE_SSH_PROXY") == "1",
		},
		LeaseID: "cbx_env_profile_test",
	}, nil
}
func (b runEnvProfileTestBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return b.Acquire(context.Background(), AcquireRequest{})
}
func (b runEnvProfileTestBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b runEnvProfileTestBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	if req.GuardedRemoteCleanup != nil {
		req.GuardedRemoteCleanup(ctx, req.Lease)
	}
	if runEnvProfileTestReleaseHook != nil {
		if err := runEnvProfileTestReleaseHook(); err != nil {
			return err
		}
	}
	if runEnvProfileTestReleaseRequestHook != nil {
		if err := runEnvProfileTestReleaseRequestHook(req); err != nil {
			return err
		}
	}
	return runEnvProfileTestReleaseErr
}
func (b runEnvProfileTestBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	if runEnvProfileTestTouchHook != nil {
		if err := runEnvProfileTestTouchHook(req); err != nil {
			return Server{}, err
		}
	}
	server := req.Lease.Server
	server.Provider = b.spec.Name
	return server, nil
}

func setupRunClaimSnapshotTest(t *testing.T) (LeaseTarget, leaseClaim) {
	t.Helper()
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	installRecordingSSH(t, dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")

	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Provider = runEnvProfileTestProvider{}.Name()
	lease := LeaseTarget{
		LeaseID: "cbx_env_profile_test",
		Server: Server{
			CloudID:  "task-owned-container",
			Provider: cfg.Provider,
			Labels: map[string]string{
				"lease":    "cbx_env_profile_test",
				"provider": cfg.Provider,
				"slug":     "claim-snapshot",
				"state":    "ready",
			},
		},
		SSH: SSHTarget{
			User:           "crabbox",
			Host:           "127.0.0.1",
			Port:           "22",
			TargetOS:       targetLinux,
			SSHConfigProxy: true,
		},
	}
	if err := claimLeaseTargetForRepoConfig(lease.LeaseID, "claim-snapshot", cfg, lease.Server, lease.SSH, repo.Root, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	initial, err := readLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	SetServerLeaseClaimSnapshot(&lease.Server, initial, true)
	runEnvProfileTestAcquireLease = func(AcquireRequest) (LeaseTarget, error) { return lease, nil }
	t.Cleanup(func() {
		runEnvProfileTestAcquireLease = nil
		runEnvProfileTestReleaseRequestHook = nil
		removeLeaseClaim(lease.LeaseID)
	})
	return lease, initial
}

func TestRunCommandOneShotCleanupUsesUpdatedClaimSnapshot(t *testing.T) {
	lease, initial := setupRunClaimSnapshotTest(t)
	resourceDeleted := false
	runEnvProfileTestReleaseRequestHook = func(req ReleaseLeaseRequest) error {
		claim, exists, set := ServerLeaseClaimSnapshot(req.Lease.Server)
		if !set || !exists {
			return errors.New("release did not receive a claim snapshot")
		}
		if claim.Revision == initial.Revision {
			return errors.New("release received the pre-registration claim snapshot")
		}
		return removeLeaseClaimIfUnchangedAfter(req.Lease.LeaseID, claim, func() error {
			resourceDeleted = true
			return nil
		})
	}

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", runEnvProfileTestProvider{}.Name(),
		"--no-sync",
		"--",
		"true",
	})
	if err != nil {
		t.Fatalf("run error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if !resourceDeleted {
		t.Fatal("task-owned resource was not deleted")
	}
	if _, exists, err := readLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v after successful cleanup", exists, err)
	}
}

func TestWarmupFailureAfterRegistrationReleasesNewestClaimSnapshot(t *testing.T) {
	lease, initial := setupRunClaimSnapshotTest(t)
	releases := 0
	runEnvProfileTestReleaseRequestHook = func(req ReleaseLeaseRequest) error {
		releases++
		snapshot, exists, set := ServerLeaseClaimSnapshot(req.Lease.Server)
		if !set || !exists || snapshot.Revision == initial.Revision {
			return fmt.Errorf("release received stale claim snapshot: %#v", snapshot)
		}
		return removeLeaseClaimIfUnchangedAfter(req.Lease.LeaseID, snapshot, nil)
	}
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).warmup(context.Background(), []string{
		"--provider", runEnvProfileTestProvider{}.Name(), "--network", "tailscale",
	})
	if err == nil || !strings.Contains(err.Error(), "no tailnet address") || releases != 1 {
		t.Fatalf("warmup error=%v releases=%d", err, releases)
	}
	if _, exists, err := readLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
}

func TestResolvedRegistrationTouchReceivesNewestClaimSnapshot(t *testing.T) {
	lease, initial := setupRunClaimSnapshotTest(t)
	cfg := baseConfig()
	setProviderSelection(&cfg, runEnvProfileTestProvider{}.Name(), providerSelectionFlag)
	touches := 0
	runEnvProfileTestTouchHook = func(req TouchRequest) error {
		touches++
		snapshot, exists, set := ServerLeaseClaimSnapshot(req.Lease.Server)
		current, err := readLeaseClaim(req.Lease.LeaseID)
		if err != nil || !set || !exists || snapshot.Revision == initial.Revision || !reflect.DeepEqual(snapshot, current) {
			return fmt.Errorf("touch received stale snapshot: snapshot=%#v current=%#v exists=%t set=%t err=%v", snapshot, current, exists, set, err)
		}
		return nil
	}
	t.Cleanup(func() { runEnvProfileTestTouchHook = nil })
	var stderr bytes.Buffer
	if err := (App{Stderr: &stderr}).claimAndTouchLeaseTarget(context.Background(), cfg, &lease.Server, lease.SSH, lease.LeaseID, false); err != nil {
		t.Fatal(err)
	}
	if touches != 1 {
		t.Fatalf("touches=%d stderr=%q", touches, stderr.String())
	}
}

func TestRunCommandCleanupRejectsClaimReplacedAfterRegistration(t *testing.T) {
	lease, _ := setupRunClaimSnapshotTest(t)
	resourceDeleted := false
	runEnvProfileTestReleaseRequestHook = func(req ReleaseLeaseRequest) error {
		claim, exists, set := ServerLeaseClaimSnapshot(req.Lease.Server)
		if !set || !exists {
			return errors.New("release did not receive a claim snapshot")
		}
		labels := cloneStringMap(claim.Labels)
		labels["owner"] = "replacement-process"
		if _, err := updateLeaseClaimLabelsIfUnchanged(req.Lease.LeaseID, claim, labels); err != nil {
			return err
		}
		return removeLeaseClaimIfUnchangedAfter(req.Lease.LeaseID, claim, func() error {
			resourceDeleted = true
			return nil
		})
	}

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", runEnvProfileTestProvider{}.Name(),
		"--no-sync",
		"--",
		"true",
	})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("run error=%v, want ownership-change cleanup failure\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if resourceDeleted {
		t.Fatal("replacement-owned resource was deleted")
	}
	replacement, readErr := readLeaseClaim(lease.LeaseID)
	if readErr != nil || replacement.Labels["owner"] != "replacement-process" {
		t.Fatalf("replacement claim=%#v err=%v", replacement, readErr)
	}
}
func (b runEnvProfileTestBackend) ReleaseLeaseConnectionCleanupSafe() bool {
	return runEnvProfileTestConnectionCleanupSafe
}

type runWorkdirCase struct {
	name       string
	native     bool
	diagnostic string
	guidance   []string
}

type runArchiveSyncPreflightTestProvider struct{}

func (runArchiveSyncPreflightTestProvider) Name() string { return "run-archive-sync-preflight-test" }
func (runArchiveSyncPreflightTestProvider) Aliases() []string {
	return nil
}
func (runArchiveSyncPreflightTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "run-archive-sync-preflight-test",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureArchiveSync},
		Coordinator: CoordinatorNever,
	}
}
func (runArchiveSyncPreflightTestProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (runArchiveSyncPreflightTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p runArchiveSyncPreflightTestProvider) Configure(Config, Runtime) (Backend, error) {
	runArchiveSyncPreflightProviderCalls.Add(1)
	return runArchiveSyncPreflightTestBackend{spec: p.Spec()}, nil
}

type runArchiveSyncPreflightTestBackend struct {
	spec ProviderSpec
}

var runArchiveSyncPreflightProviderCalls atomic.Int64

func (b runArchiveSyncPreflightTestBackend) Spec() ProviderSpec { return b.spec }
func (runArchiveSyncPreflightTestBackend) Warmup(context.Context, WarmupRequest) error {
	runArchiveSyncPreflightProviderCalls.Add(1)
	return nil
}
func (b runArchiveSyncPreflightTestBackend) Run(context.Context, RunRequest) (RunResult, error) {
	runArchiveSyncPreflightProviderCalls.Add(1)
	return RunResult{Provider: b.spec.Name}, nil
}
func (runArchiveSyncPreflightTestBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	runArchiveSyncPreflightProviderCalls.Add(1)
	return nil, nil
}
func (runArchiveSyncPreflightTestBackend) Status(context.Context, StatusRequest) (StatusView, error) {
	runArchiveSyncPreflightProviderCalls.Add(1)
	return StatusView{}, nil
}
func (runArchiveSyncPreflightTestBackend) Stop(context.Context, StopRequest) error {
	runArchiveSyncPreflightProviderCalls.Add(1)
	return nil
}

func setupDelegatedArchiveSyncPreflightWorkspace(t *testing.T, colocatedGit, invalidGitMarker bool) string {
	t.Helper()
	clearConfigEnv(t)
	outer := t.TempDir()
	runGit(t, outer, "init")
	root := filepath.Join(outer, "native-workspace")
	makeNativeJujutsuWorkspace(t, root)
	if colocatedGit {
		runGit(t, root, "init")
	} else if invalidGitMarker {
		if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "tracked.txt"), "tracked\n")
	if colocatedGit {
		runGit(t, root, "add", "tracked.txt")
	}
	isolateRunTestUserDirs(t, root)
	t.Chdir(root)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(root, "missing.yaml"))
	return root
}

func TestRunDelegatedArchiveSyncValidatesSourceBeforeProviderCall(t *testing.T) {
	provider := runArchiveSyncPreflightTestProvider{}.Name()

	t.Run("native Jujutsu", func(t *testing.T) {
		root := setupDelegatedArchiveSyncPreflightWorkspace(t, false, false)
		runArchiveSyncPreflightProviderCalls.Store(0)

		var stdout, stderr bytes.Buffer
		err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
			"--provider", provider,
			"--", "true",
		})
		var exitErr ExitError
		if !AsExitError(err, &exitErr) || exitErr.Code != 6 {
			t.Fatalf("error=%v, want exit 6\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
		}
		for _, want := range []string{root, "native Jujutsu workspace", "Git-manifest-based", "wrong revision", "colocated Git workspace", "jj git init --git-repo=.", "--no-sync"} {
			if !strings.Contains(exitErr.Message, want) {
				t.Fatalf("message missing %q: %q", want, exitErr.Message)
			}
		}
		if calls := runArchiveSyncPreflightProviderCalls.Load(); calls != 0 {
			t.Fatalf("delegated provider calls=%d, want zero before source validation", calls)
		}
	})

	t.Run("invalid colocated Git marker", func(t *testing.T) {
		setupDelegatedArchiveSyncPreflightWorkspace(t, false, true)
		runArchiveSyncPreflightProviderCalls.Store(0)

		err := (App{Stdout: io.Discard, Stderr: io.Discard}).runCommand(context.Background(), []string{
			"--provider", provider,
			"--", "true",
		})
		var exitErr ExitError
		if !AsExitError(err, &exitErr) || exitErr.Code != 6 || !strings.Contains(exitErr.Message, "native Jujutsu workspace") {
			t.Fatalf("error=%v, want native Jujutsu exit 6", err)
		}
		if calls := runArchiveSyncPreflightProviderCalls.Load(); calls != 0 {
			t.Fatalf("delegated provider calls=%d, want zero for invalid Git metadata", calls)
		}
	})

	t.Run("no sync", func(t *testing.T) {
		setupDelegatedArchiveSyncPreflightWorkspace(t, false, false)
		runArchiveSyncPreflightProviderCalls.Store(0)

		err := (App{Stdout: io.Discard, Stderr: io.Discard}).runCommand(context.Background(), []string{
			"--provider", provider,
			"--no-sync",
			"--", "true",
		})
		if err != nil {
			t.Fatalf("run error=%v", err)
		}
		if calls := runArchiveSyncPreflightProviderCalls.Load(); calls != 2 {
			t.Fatalf("delegated provider calls=%d, want configure and --no-sync run", calls)
		}
	})

	t.Run("colocated Git", func(t *testing.T) {
		setupDelegatedArchiveSyncPreflightWorkspace(t, true, false)
		runArchiveSyncPreflightProviderCalls.Store(0)

		err := (App{Stdout: io.Discard, Stderr: io.Discard}).runCommand(context.Background(), []string{
			"--provider", provider,
			"--", "true",
		})
		if err != nil {
			t.Fatalf("run error=%v", err)
		}
		if calls := runArchiveSyncPreflightProviderCalls.Load(); calls != 2 {
			t.Fatalf("delegated provider calls=%d, want configure and colocated Git run", calls)
		}
	})

	t.Run("fresh PR remains provider option validation", func(t *testing.T) {
		setupDelegatedArchiveSyncPreflightWorkspace(t, false, false)
		runArchiveSyncPreflightProviderCalls.Store(0)

		err := (App{Stdout: io.Discard, Stderr: io.Discard}).runCommand(context.Background(), []string{
			"--provider", provider,
			"--fresh-pr", "example-org/my-app#1",
			"--", "true",
		})
		var exitErr ExitError
		if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "--fresh-pr is not supported") {
			t.Fatalf("error=%v, want delegated fresh-PR rejection", err)
		}
		if strings.Contains(exitErr.Message, "native Jujutsu workspace") {
			t.Fatalf("fresh-PR option rejection triggered local source validation: %q", exitErr.Message)
		}
		if calls := runArchiveSyncPreflightProviderCalls.Load(); calls != 0 {
			t.Fatalf("delegated provider calls=%d, want zero for rejected fresh PR", calls)
		}
	})
}

func runWorkdirCases() []runWorkdirCase {
	return []runWorkdirCase{
		{name: "ordinary non-Git", diagnostic: "not a Git repository", guidance: []string{"git init", "--no-sync"}},
		{name: "native Jujutsu", native: true, diagnostic: "native Jujutsu workspace", guidance: []string{"Git-manifest-based", "wrong revision", "colocated Git workspace", "jj git init --git-repo=.", "--no-sync"}},
	}
}

func setupRunWorkdirCase(t *testing.T, tc runWorkdirCase) string {
	t.Helper()
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	if tc.native {
		makeNativeJujutsuWorkspace(t, dir)
	}
	t.Chdir(dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	return dir
}

func TestRunWorkdirFailsBeforeAcquire(t *testing.T) {
	for _, tc := range runWorkdirCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupRunWorkdirCase(t, tc)
			acquireCalls := 0
			runEnvProfileTestAcquireHook = func(AcquireRequest) { acquireCalls++ }
			t.Cleanup(func() { runEnvProfileTestAcquireHook = nil })

			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
				"--provider", "run-env-profile-test",
				"--", "true",
			})
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 6 {
				t.Fatalf("error=%v, want exit 6\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
			}
			if acquireCalls != 0 {
				t.Fatalf("Acquire called %d time(s) before local manifest failure", acquireCalls)
			}
			for _, want := range append([]string{dir, tc.diagnostic}, tc.guidance...) {
				if !strings.Contains(exitErr.Message, want) {
					t.Fatalf("message missing %q: %q", want, exitErr.Message)
				}
			}
		})
	}
}

func TestRunWorkdirFailsBeforeReadyPoolBorrow(t *testing.T) {
	for _, tc := range runWorkdirCases() {
		t.Run(tc.name, func(t *testing.T) {
			setupRunWorkdirCase(t, tc)
			requests := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.Method + " " + r.URL.Path
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			}))
			t.Cleanup(server.Close)
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "test-token")

			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
				"--provider", "run-ready-pool-preflight-test",
				"--pool", "shared-linux",
				"--pool-return", "drain",
				"--", "true",
			})
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 6 || !strings.Contains(exitErr.Message, tc.diagnostic) {
				t.Fatalf("error=%v, want %s exit 6\nstdout=%s\nstderr=%s", err, tc.name, stdout.String(), stderr.String())
			}
			select {
			case request := <-requests:
				t.Fatalf("ready-pool request occurred before local manifest failure: %s", request)
			default:
			}
		})
	}
}

func TestRunBuildsSyncManifestAfterAcquire(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	t.Chdir(dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "before-acquire.txt"), "before\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sshPath := filepath.Join(binDir, "ssh")
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\n/bin/cat >/dev/null || true\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rsyncLog := filepath.Join(dir, "rsync-manifest.log")
	rsyncPath := filepath.Join(binDir, "rsync")
	if err := os.WriteFile(rsyncPath, []byte("#!/bin/sh\n/bin/cat > \"$CRABBOX_FAKE_RSYNC_STDIN_LOG\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_RSYNC_STDIN_LOG", rsyncLog)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
	acquireCalls := 0
	runEnvProfileTestAcquireHook = func(req AcquireRequest) {
		acquireCalls++
		writeFile(t, filepath.Join(req.Repo.Root, "after-acquire.txt"), "after\n")
	}
	t.Cleanup(func() { runEnvProfileTestAcquireHook = nil })

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--", "true",
	})
	if err != nil {
		t.Fatalf("run error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if acquireCalls != 1 {
		t.Fatalf("Acquire calls=%d, want 1", acquireCalls)
	}
	manifest, err := os.ReadFile(rsyncLog)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(manifest, []byte("before-acquire.txt\x00")) {
		t.Fatalf("ordinary sync omitted initial file: %q", manifest)
	}
	if !bytes.Contains(manifest, []byte("after-acquire.txt\x00")) {
		t.Fatalf("ordinary sync reused the pre-acquisition file list: %q", manifest)
	}
}

type runPrepareTestProvider struct{}

func (runPrepareTestProvider) Name() string { return "run-prepare-test" }
func (runPrepareTestProvider) Aliases() []string {
	return nil
}
func (runPrepareTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "run-prepare-test",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorNever,
	}
}
func (runPrepareTestProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (runPrepareTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p runPrepareTestProvider) Configure(Config, Runtime) (Backend, error) {
	return runPrepareTestBackend{spec: p.Spec()}, nil
}

type runPrepareTestBackend struct {
	spec ProviderSpec
}

var runPrepareTestResolveRequests []ResolveRequest

func (b runPrepareTestBackend) Spec() ProviderSpec { return b.spec }
func (b runPrepareTestBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{}, exit(9, "unexpected acquire")
}
func (b runPrepareTestBackend) Resolve(_ context.Context, req ResolveRequest) (LeaseTarget, error) {
	runPrepareTestResolveRequests = append(runPrepareTestResolveRequests, req)
	return LeaseTarget{}, exit(9, "resolve captured")
}
func (b runPrepareTestBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b runPrepareTestBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return nil
}
func (b runPrepareTestBackend) Touch(context.Context, TouchRequest) (Server, error) {
	return Server{Provider: b.spec.Name}, nil
}

type runModuleRuntimeTestProvider struct{}

func (runModuleRuntimeTestProvider) Name() string { return "module-runtime-test" }
func (runModuleRuntimeTestProvider) Aliases() []string {
	return nil
}
func (runModuleRuntimeTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "module-runtime-test",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetWorkerRuntime}},
		Features:    FeatureSet{FeatureModuleRun},
		Coordinator: CoordinatorNever,
	}
}
func (runModuleRuntimeTestProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (runModuleRuntimeTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p runModuleRuntimeTestProvider) Configure(Config, Runtime) (Backend, error) {
	return runModuleRuntimeTestBackend{spec: p.Spec()}, nil
}

type runModuleRuntimeTestBackend struct {
	spec ProviderSpec
}

var runModuleRuntimeTestRequests []RunRequest

type countingReader struct {
	data  string
	reads int
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads++
	if r.data == "" {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (b runModuleRuntimeTestBackend) Spec() ProviderSpec { return b.spec }
func (b runModuleRuntimeTestBackend) Warmup(context.Context, WarmupRequest) error {
	return nil
}
func (b runModuleRuntimeTestBackend) Run(_ context.Context, req RunRequest) (RunResult, error) {
	runModuleRuntimeTestRequests = append(runModuleRuntimeTestRequests, req)
	return RunResult{Provider: b.spec.Name, LeaseID: "mod_test", Slug: "module-runtime-test"}, nil
}
func (b runModuleRuntimeTestBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b runModuleRuntimeTestBackend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, nil
}
func (b runModuleRuntimeTestBackend) Stop(context.Context, StopRequest) error {
	return nil
}

func TestRunNoSyncRequestsExistingLeasePreparation(t *testing.T) {
	for _, tc := range runWorkdirCases() {
		t.Run(tc.name, func(t *testing.T) {
			setupRunWorkdirCase(t, tc)
			runPrepareTestResolveRequests = nil

			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
				"--provider", "run-prepare-test",
				"--id", "cbx_existing",
				"--no-sync",
				"--", "true",
			})
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 9 || !strings.Contains(exitErr.Message, "resolve captured") {
				t.Fatalf("run error=%v, want backend resolve proof\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
			}
			if strings.Contains(exitErr.Message, "native Jujutsu workspace") {
				t.Fatalf("--no-sync triggered native Jujutsu guard: %q", exitErr.Message)
			}
			if len(runPrepareTestResolveRequests) != 1 {
				t.Fatalf("resolve requests=%#v, want one", runPrepareTestResolveRequests)
			}
			if got := runPrepareTestResolveRequests[0]; got.ID != "cbx_existing" || !got.Prepare {
				t.Fatalf("resolve request=%#v, want existing id with Prepare", got)
			}
		})
	}
}

func TestRunWorkdirFailsBeforeExistingLeaseResolveAndPrepare(t *testing.T) {
	for _, tc := range runWorkdirCases() {
		t.Run(tc.name, func(t *testing.T) {
			setupRunWorkdirCase(t, tc)
			runPrepareTestResolveRequests = nil

			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
				"--provider", "run-prepare-test",
				"--id", "cbx_existing",
				"--", "true",
			})
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 6 || !strings.Contains(exitErr.Message, tc.diagnostic) {
				t.Fatalf("error=%v, want %s exit 6\nstdout=%s\nstderr=%s", err, tc.name, stdout.String(), stderr.String())
			}
			if len(runPrepareTestResolveRequests) != 0 {
				t.Fatalf("Resolve/Prepare called before local manifest failure: %#v", runPrepareTestResolveRequests)
			}
		})
	}
}

func TestRunNonGitFreshPRStillResolvesExistingLease(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	t.Chdir(dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	runPrepareTestResolveRequests = nil

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-prepare-test",
		"--id", "cbx_existing",
		"--fresh-pr", "example-org/my-app#123",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 9 || !strings.Contains(exitErr.Message, "resolve captured") {
		t.Fatalf("run error=%v, want resolve-captured exit\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if len(runPrepareTestResolveRequests) != 1 || !runPrepareTestResolveRequests[0].Prepare {
		t.Fatalf("fresh-PR run did not reach Resolve/Prepare: %#v", runPrepareTestResolveRequests)
	}
}

func TestRunWithExistingLeaseRoutesProviderFromClaim(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_PROVIDER", "hetzner")
	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	const leaseID = "cbx_1257abcdefff"
	if err := claimLeaseForRepoProvider(leaseID, "claim-routed", "run-prepare-test", repo.Root, time.Minute, false); err != nil {
		t.Fatal(err)
	}

	runPrepareTestResolveRequests = nil
	var stdout, stderr bytes.Buffer
	err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--id", leaseID,
		"--no-sync",
		"--",
		"true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 9 || !strings.Contains(exitErr.Message, "resolve captured") {
		t.Fatalf("run error=%v, want claim-routed resolve-captured exit; stderr=%s", err, stderr.String())
	}
	if len(runPrepareTestResolveRequests) != 1 {
		t.Fatalf("resolve requests=%#v, want one", runPrepareTestResolveRequests)
	}
	if got := runPrepareTestResolveRequests[0]; got.ID != leaseID || !got.Prepare {
		t.Fatalf("resolve request=%#v, want claim-routed existing id with Prepare", got)
	}
}

func TestFormatRunSummary(t *testing.T) {
	endToEndStartedAt := time.Unix(0, 0)
	got := formatRunSummary(runTimings{
		started:           endToEndStartedAt.Add(1500 * time.Millisecond),
		endToEndStartedAt: endToEndStartedAt,
		lease:             time.Second,
		sync:              1200 * time.Millisecond,
		command:           3400 * time.Millisecond,
		syncSteps: syncStepTimings{
			manifest: 20 * time.Millisecond,
			rsync:    900 * time.Millisecond,
		},
		syncSkipped: true,
	}, 5*time.Second, 7)
	for _, want := range []string{
		"run summary",
		"sync=1.2s",
		"command=3.4s",
		"total=5s",
		"end_to_end=6.5s",
		"sync_skipped=true",
		"exit=7",
		"sync_steps=manifest:20ms,rsync:900ms",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q in %q", want, got)
		}
	}
}

func TestFormatRunSummaryIncludesGitHydrateSkipReason(t *testing.T) {
	got := formatRunSummary(runTimings{
		sync: 2 * time.Second,
		syncSteps: syncStepTimings{
			gitHydrateSkipped:    true,
			gitHydrateSkipReason: "remote base current",
		},
	}, 3*time.Second, 0)
	if !strings.Contains(got, "git_hydrate:skipped_remote_base_current") {
		t.Fatalf("summary missing git hydrate skip reason: %q", got)
	}
}

func TestFormatRunSummaryNoSync(t *testing.T) {
	got := formatRunSummary(runTimings{
		syncSkipped: true,
	}, 500*time.Millisecond, 0)
	for _, want := range []string{
		"sync=0s",
		"sync_skipped=true",
		"exit=0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q in %q", want, got)
		}
	}
}

func TestShouldReplaceLeaseAfterBeforeCommandSSHFailure(t *testing.T) {
	waitErr := exit(5, "timed out waiting for SSH on 203.0.113.10 during before command")
	otherErr := exit(6, "rsync failed")
	tests := []struct {
		name            string
		err             error
		acquired        bool
		useCoordinator  bool
		explicitLeaseID bool
		keep            bool
		keepOnFailure   bool
		noSync          bool
		syncOnly        bool
		stopAfter       string
		requestedSlug   string
		want            bool
	}{
		{name: "fresh coordinator one shot", err: waitErr, acquired: true, useCoordinator: true, want: true},
		{name: "wrong error", err: otherErr, acquired: true, useCoordinator: true},
		{name: "direct backend", err: waitErr, acquired: true},
		{name: "existing lease", err: waitErr, acquired: true, useCoordinator: true, explicitLeaseID: true},
		{name: "kept lease", err: waitErr, acquired: true, useCoordinator: true, keep: true},
		{name: "keep on failure", err: waitErr, acquired: true, useCoordinator: true, keepOnFailure: true},
		{name: "no sync", err: waitErr, acquired: true, useCoordinator: true, noSync: true},
		{name: "sync only", err: waitErr, acquired: true, useCoordinator: true, syncOnly: true},
		{name: "custom slug", err: waitErr, acquired: true, useCoordinator: true, requestedSlug: "qa-smoke"},
		{name: "stop after failure", err: waitErr, acquired: true, useCoordinator: true, stopAfter: "failure", want: true},
		{name: "stop after success", err: waitErr, acquired: true, useCoordinator: true, stopAfter: "success"},
		{name: "stop after never", err: waitErr, acquired: true, useCoordinator: true, stopAfter: "never"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldReplaceLeaseAfterBeforeCommandSSHFailure(tt.err, tt.acquired, tt.useCoordinator, tt.explicitLeaseID, tt.keep, tt.keepOnFailure, tt.noSync, tt.syncOnly, tt.stopAfter, tt.requestedSlug)
			if got != tt.want {
				t.Fatalf("shouldReplaceLeaseAfterBeforeCommandSSHFailure()=%t, want %t", got, tt.want)
			}
		})
	}
}

func TestTimingJSONShape(t *testing.T) {
	var buf bytes.Buffer
	endToEndStartedAt := time.Unix(0, 0)
	err := writeTimingJSON(&buf, timingReportFromRun("aws", "cbx_123", "blue-crab", runTimings{
		started:           endToEndStartedAt.Add(2600 * time.Millisecond),
		endToEndStartedAt: endToEndStartedAt,
		lease:             2300 * time.Millisecond,
		bootstrap:         700 * time.Millisecond,
		sync:              1200 * time.Millisecond,
		command:           3400 * time.Millisecond,
		syncSteps: syncStepTimings{
			rsync:                900 * time.Millisecond,
			gitHydrateSkipped:    true,
			gitHydrateSkipReason: "marker base current",
		},
		syncSkipped: true,
	}, 5*time.Second, 7))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Provider    string `json:"provider"`
		LeaseID     string `json:"leaseId"`
		LeaseMs     int64  `json:"leaseMs"`
		BootstrapMs int64  `json:"bootstrapMs"`
		SyncMs      int64  `json:"syncMs"`
		CommandMs   int64  `json:"commandMs"`
		TotalMs     int64  `json:"totalMs"`
		EndToEndMs  int64  `json:"endToEndMs"`
		ExitCode    int    `json:"exitCode"`
		RunStatus   string `json:"runStatus"`
		ErrorKind   string `json:"errorKind"`
		SyncSkipped bool   `json:"syncSkipped"`
		SyncPhases  []struct {
			Name    string `json:"name"`
			Ms      int64  `json:"ms"`
			Skipped bool   `json:"skipped"`
			Reason  string `json:"reason"`
		} `json:"syncPhases"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider != "aws" || got.LeaseID != "cbx_123" || got.LeaseMs != 2300 || got.BootstrapMs != 700 || got.SyncMs != 1200 || got.CommandMs != 3400 || got.TotalMs != 5000 || got.EndToEndMs != 7600 || got.ExitCode != 7 || !got.SyncSkipped {
		t.Fatalf("unexpected report: %#v", got)
	}
	if got.RunStatus != "failed" || got.ErrorKind != "command-exit" {
		t.Fatalf("runStatus/errorKind=%q/%q", got.RunStatus, got.ErrorKind)
	}
	if len(got.SyncPhases) != 2 || got.SyncPhases[1].Name != "git_hydrate" || !got.SyncPhases[1].Skipped || got.SyncPhases[1].Reason != "marker base current" {
		t.Fatalf("unexpected phases: %#v", got.SyncPhases)
	}
}

func TestTimingJSONDefaultsSuccessfulRunStatus(t *testing.T) {
	var buf bytes.Buffer
	if err := writeTimingJSON(&buf, timingReportFromRun("aws", "cbx_123", "blue-crab", runTimings{}, time.Second, 0)); err != nil {
		t.Fatal(err)
	}
	var got TimingReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RunStatus != RunStatusSucceeded || got.ErrorKind != RunErrorNone {
		t.Fatalf("runStatus/errorKind=%q/%q", got.RunStatus, got.ErrorKind)
	}
	if strings.Contains(buf.String(), `"errorKind"`) {
		t.Fatalf("successful timing JSON should omit errorKind: %s", buf.String())
	}
}

func TestTimingJSONPreservesProviderOutcome(t *testing.T) {
	var buf bytes.Buffer
	report := timingReportFromRun("sandbox", "cbx_123", "blue-crab", runTimings{}, time.Second, 1)
	report.RunStatus = RunStatusTimedOut
	report.ErrorKind = RunErrorTimeout
	if err := writeTimingJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	var got TimingReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RunStatus != RunStatusTimedOut || got.ErrorKind != RunErrorTimeout {
		t.Fatalf("runStatus/errorKind=%q/%q", got.RunStatus, got.ErrorKind)
	}
}

func TestTimingJSONIncludesActionsRunURLWhenAvailable(t *testing.T) {
	var buf bytes.Buffer
	err := writeTimingJSON(&buf, timingReportFromRunWithActionsURL("aws", "cbx_123", "blue-crab", runTimings{
		sync:    1200 * time.Millisecond,
		command: 3400 * time.Millisecond,
	}, 5*time.Second, 0, "https://github.com/openclaw/openclaw/actions/runs/123"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ActionsRunURL string `json:"actionsRunUrl"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ActionsRunURL != "https://github.com/openclaw/openclaw/actions/runs/123" {
		t.Fatalf("actionsRunUrl=%q", got.ActionsRunURL)
	}
}

func TestTimingJSONIncludesLabelWhenAvailable(t *testing.T) {
	var buf bytes.Buffer
	report := timingReportFromRun("aws", "cbx_123", "blue-crab", runTimings{}, time.Second, 0)
	report.Label = "update flow smoke"
	if err := writeTimingJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Label != "update flow smoke" {
		t.Fatalf("label=%q", got.Label)
	}
}

func TestRunCommandRejectsUnsupportedDelegatedCaptureOptions(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")

	tests := []struct {
		name     string
		provider string
		args     []string
		want     string
	}{
		{name: "daytona capture stdout", provider: "daytona", args: []string{"--capture-stdout", "stdout.bin"}, want: "daytona delegates run execution; --capture-stdout is not supported"},
		{name: "islo capture stdout", provider: "islo", args: []string{"--capture-stdout", "stdout.bin"}, want: "islo delegates run execution; --capture-stdout is not supported"},
		{name: "e2b capture stdout", provider: "e2b", args: []string{"--capture-stdout", "stdout.bin"}, want: "e2b delegates run execution; --capture-stdout is not supported"},
		{name: "daytona capture stderr", provider: "daytona", args: []string{"--capture-stderr", "stderr.bin"}, want: "daytona delegates run execution; --capture-stderr is not supported"},
		{name: "islo capture on fail", provider: "islo", args: []string{"--capture-on-fail"}, want: "islo delegates run execution; --capture-on-fail is not supported"},
		{name: "daytona download", provider: "daytona", args: []string{"--download", "/tmp/proof=proof.bin"}, want: "daytona delegates run execution; --download is not supported"},
		{name: "islo unsafe download", provider: "islo", args: []string{"--download", "/tmp/proof=proof.bin"}, want: "--download for delegated providers requires a safe relative file path"},
		{name: "e2b download", provider: "e2b", args: []string{"--download", "/tmp/proof=proof.bin"}, want: "e2b delegates run execution; --download is not supported"},
		{name: "daytona require artifact", provider: "daytona", args: []string{"--require-artifact", "reports/data/manifest.json"}, want: "daytona delegates run execution; --require-artifact is not supported"},
		{name: "islo unsafe require artifact", provider: "islo", args: []string{"--require-artifact", "../manifest.json"}, want: "--require-artifact contains unsupported characters or non-relative path"},
		{name: "e2b require artifact", provider: "e2b", args: []string{"--require-artifact", "reports/data/manifest.json"}, want: "e2b delegates run execution; --require-artifact is not supported"},
		{name: "e2b stop after", provider: "e2b", args: []string{"--stop-after", "never"}, want: "e2b delegates run execution; --stop-after is not supported"},
		{name: "daytona script", provider: "daytona", args: []string{"--script", "testdata/missing.sh"}, want: "daytona delegates run execution; --script is not supported"},
		{name: "e2b fresh pr", provider: "e2b", args: []string{"--fresh-pr", "example-org/my-app#1"}, want: "e2b delegates sync; --fresh-pr is not supported"},
		{name: "e2b full resync", provider: "e2b", args: []string{"--full-resync"}, want: "e2b delegates sync; --full-resync is not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr}
			args := append([]string{"--provider", tt.provider}, tt.args...)
			args = append(args, "--", "true")
			err := app.runCommand(context.Background(), args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("error=%v, want exit 2", err)
			}
			if !strings.Contains(exitErr.Message, tt.want) {
				t.Fatalf("message=%q want %q", exitErr.Message, tt.want)
			}
		})
	}
}

func TestRunCommandAcceptsE2BLeaseOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.runCommand(t.Context(), []string{"--provider", "e2b", "--keep", "--lease-output", path, "--", "true"}); err != nil {
		t.Fatalf("runCommand: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lease output: %v", err)
	}
	var session RunSessionHandle
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatalf("parse lease output: %v", err)
	}
	if session.Provider != "e2b" || session.LeaseID != "tbx_test" || !session.Kept || session.CleanupCommand == "" {
		t.Fatalf("session=%#v", session)
	}
}

const localContainerRunSessionTestLeaseID = "cbx_1260abc12345"

func setupLocalContainerRunSessionTest(t *testing.T, commandScript string) (string, LeaseTarget) {
	t.Helper()
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	if strings.TrimSpace(commandScript) == "" {
		commandScript = "#!/bin/sh\nexit 0\n"
	}
	installWorkspaceOwnerAwareSSH(t, sshPath, commandScript)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
	lease := LeaseTarget{
		LeaseID: localContainerRunSessionTestLeaseID,
		Server: Server{
			CloudID:  "container-secret-id",
			Provider: "local-container",
			Labels: map[string]string{
				"lease":             localContainerRunSessionTestLeaseID,
				"provider":          "local-container",
				"slug":              "session-slug",
				"state":             "ready",
				"target":            targetLinux,
				"provider_metadata": "provider-metadata-secret",
			},
		},
		SSH: SSHTarget{
			User:           "crabbox",
			Host:           "secret-host.invalid",
			Port:           "22",
			Key:            "/tmp/private-secret-key",
			TargetOS:       targetLinux,
			SSHConfigProxy: true,
		},
	}
	runEnvProfileTestAcquireLease = func(AcquireRequest) (LeaseTarget, error) { return lease, nil }
	runEnvProfileTestAcquireHook = nil
	runEnvProfileTestReleaseHook = nil
	runEnvProfileTestReleaseRequestHook = nil
	runEnvProfileTestTouchHook = nil
	runEnvProfileTestReleaseErr = nil
	t.Cleanup(func() {
		runEnvProfileTestAcquireLease = nil
		runEnvProfileTestAcquireHook = nil
		runEnvProfileTestReleaseHook = nil
		runEnvProfileTestReleaseRequestHook = nil
		runEnvProfileTestTouchHook = nil
		runEnvProfileTestReleaseErr = nil
		removeLeaseClaim(localContainerRunSessionTestLeaseID)
	})
	return dir, lease
}

func readRunSessionHandleTest(t *testing.T, path string) (RunSessionHandle, map[string]json.RawMessage, []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lease output: %v", err)
	}
	var session RunSessionHandle
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatalf("parse lease output: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("parse lease output fields: %v", err)
	}
	return session, fields, data
}

func installRunSessionValidationFailureMarkers(t *testing.T, dir string) (string, string) {
	t.Helper()
	commandMarker := filepath.Join(dir, "command-ran")
	syncMarker := filepath.Join(dir, "sync-ran")
	t.Setenv("CRABBOX_COMMAND_MARKER", commandMarker)
	t.Setenv("CRABBOX_SYNC_MARKER", syncMarker)
	if err := os.WriteFile(filepath.Join(dir, "rsync"), []byte("#!/bin/sh\n: > \"$CRABBOX_SYNC_MARKER\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return commandMarker, syncMarker
}

func assertRunSessionValidationStoppedBeforeWork(t *testing.T, sessionPath, commandMarker, syncMarker string) {
	t.Helper()
	for label, path := range map[string]string{
		"lease output": sessionPath,
		"user command": commandMarker,
		"sync":         syncMarker,
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s unexpectedly exists after validation failure: %v", label, err)
		}
	}
}

func TestRunCommandWritesFreshLocalContainerLeaseOutputAfterClaim(t *testing.T) {
	dir, _ := setupLocalContainerRunSessionTest(t, "")
	path := filepath.Join(dir, "session.json")
	releases := 0
	runEnvProfileTestReleaseHook = func() error {
		releases++
		return nil
	}
	touchObserved := false
	runEnvProfileTestTouchHook = func(req TouchRequest) error {
		if _, exists, err := readLeaseClaimWithPresence(req.Lease.LeaseID); err != nil || !exists {
			return fmt.Errorf("touch observed before exact claim: exists=%t err=%v", exists, err)
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("touch observed before lease output: %w", err)
		}
		touchObserved = true
		return nil
	}

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), []string{
		"--provider", "local-container",
		"--keep",
		"--no-sync",
		"--no-hydrate",
		"--lease-output", path,
		"--", "true",
	})
	if err != nil {
		t.Fatalf("runCommand: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if !touchObserved {
		t.Fatal("post-claim touch did not observe the lease output")
	}
	if releases != 0 {
		t.Fatalf("retained run released lease %d time(s)", releases)
	}

	session, fields, data := readRunSessionHandleTest(t, path)
	if session.Provider != "local-container" || session.LeaseID != localContainerRunSessionTestLeaseID || session.Slug != "session-slug" || session.Reused || !session.Kept {
		t.Fatalf("session=%#v", session)
	}
	if !regexp.MustCompile(`^run_[a-f0-9]{12}$`).MatchString(session.RunID) {
		t.Fatalf("runId=%q", session.RunID)
	}
	if want := "crabbox stop --provider local-container --target linux --id " + localContainerRunSessionTestLeaseID; session.CleanupCommand != want {
		t.Fatalf("cleanupCommand=%q want %q", session.CleanupCommand, want)
	}
	wantFields := map[string]bool{"provider": true, "leaseId": true, "slug": true, "reused": true, "kept": true, "runId": true, "cleanupCommand": true}
	if len(fields) != len(wantFields) {
		t.Fatalf("lease output fields=%v", fields)
	}
	for key := range fields {
		if !wantFields[key] {
			t.Fatalf("unexpected lease output field %q", key)
		}
	}
	for _, secret := range []string{"container-secret-id", "secret-host.invalid", "/tmp/private-secret-key", "provider-metadata-secret", "workdir", "keyPath", "claim"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("lease output contains forbidden value %q: %s", secret, data)
		}
	}
}

func TestRunCommandWritesReusedLocalContainerLeaseOutput(t *testing.T) {
	for _, stopAfter := range []string{"", "never"} {
		name := blank(stopAfter, "default")
		t.Run(name, func(t *testing.T) {
			dir, _ := setupLocalContainerRunSessionTest(t, "")
			path := filepath.Join(dir, "session.json")
			releases := 0
			runEnvProfileTestReleaseHook = func() error {
				releases++
				return nil
			}
			args := []string{
				"--provider", "local-container",
				"--id", localContainerRunSessionTestLeaseID,
				"--no-sync",
				"--no-hydrate",
				"--lease-output", path,
			}
			if stopAfter != "" {
				args = append(args, "--stop-after", stopAfter)
			}
			args = append(args, "--", "true")
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), args); err != nil {
				t.Fatalf("runCommand: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
			}
			session, _, _ := readRunSessionHandleTest(t, path)
			if !session.Reused || !session.Kept || session.LeaseID != localContainerRunSessionTestLeaseID {
				t.Fatalf("session=%#v", session)
			}
			if releases != 0 {
				t.Fatalf("reused run released lease %d time(s)", releases)
			}
		})
	}
}

func TestRunCommandLocalContainerLeaseOutputValidationFailureReleasesFreshLease(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		configure   func(*testing.T, string)
		mutateLease func(*LeaseTarget)
		want        string
	}{
		{
			name: "managed capability",
			args: []string{"--desktop"},
			want: "was not created with desktop=true",
		},
		{
			name: "native Windows profile doctor",
			args: []string{"--profile", "qa"},
			configure: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "missing.yaml"), []byte(`
profiles:
  qa:
    doctor:
      enabled: true
      tools: [node]
`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			mutateLease: func(lease *LeaseTarget) {
				lease.SSH.TargetOS = targetWindows
				lease.SSH.WindowsMode = windowsModeNormal
			},
			want: "profile doctor is not supported for native Windows targets",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, lease := setupLocalContainerRunSessionTest(t, "#!/bin/sh\ncase \"$1\" in *session-should-not-run*) : > \"$CRABBOX_COMMAND_MARKER\" ;; esac\nexit 0\n")
			if tc.configure != nil {
				tc.configure(t, dir)
			}
			if tc.mutateLease != nil {
				tc.mutateLease(&lease)
				runEnvProfileTestAcquireLease = func(AcquireRequest) (LeaseTarget, error) { return lease, nil }
			}
			path := filepath.Join(dir, "session.json")
			commandMarker, syncMarker := installRunSessionValidationFailureMarkers(t, dir)
			releases := 0
			runEnvProfileTestReleaseRequestHook = func(req ReleaseLeaseRequest) error {
				releases++
				if req.Lease.LeaseID != localContainerRunSessionTestLeaseID || req.Lease.Server.CloudID != "container-secret-id" {
					t.Errorf("released unexpected lease: %#v", req.Lease)
				}
				return nil
			}

			args := []string{
				"--provider", "local-container",
				"--keep",
				"--lease-output", path,
			}
			args = append(args, tc.args...)
			args = append(args, "--", "session-should-not-run")
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %q\nstdout=%s\nstderr=%s", err, tc.want, stdout.String(), stderr.String())
			}
			if releases != 1 {
				t.Fatalf("fresh unreported lease released %d time(s), want 1", releases)
			}
			assertRunSessionValidationStoppedBeforeWork(t, path, commandMarker, syncMarker)
			if _, exists, readErr := readLeaseClaimWithPresence(localContainerRunSessionTestLeaseID); readErr != nil || exists {
				t.Fatalf("claim after validation failure exists=%t err=%v", exists, readErr)
			}
		})
	}
}

func TestRunCommandLocalContainerLeaseOutputValidationFailureDoesNotReleaseReusedLease(t *testing.T) {
	dir, _ := setupLocalContainerRunSessionTest(t, "#!/bin/sh\ncase \"$1\" in *session-should-not-run*) : > \"$CRABBOX_COMMAND_MARKER\" ;; esac\nexit 0\n")
	path := filepath.Join(dir, "session.json")
	commandMarker, syncMarker := installRunSessionValidationFailureMarkers(t, dir)
	releases := 0
	runEnvProfileTestReleaseHook = func() error {
		releases++
		return nil
	}

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), []string{
		"--provider", "local-container",
		"--id", localContainerRunSessionTestLeaseID,
		"--lease-output", path,
		"--desktop",
		"--", "session-should-not-run",
	})
	if err == nil || !strings.Contains(err.Error(), "was not created with desktop=true") {
		t.Fatalf("error=%v want managed-capability validation failure\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if releases != 0 {
		t.Fatalf("reused lease released %d time(s), want 0", releases)
	}
	assertRunSessionValidationStoppedBeforeWork(t, path, commandMarker, syncMarker)
}

func TestRunCommandLocalContainerLeaseOutputCleanupFailurePreservesValidationError(t *testing.T) {
	dir, _ := setupLocalContainerRunSessionTest(t, "#!/bin/sh\ncase \"$1\" in *session-should-not-run*) : > \"$CRABBOX_COMMAND_MARKER\" ;; esac\nexit 0\n")
	path := filepath.Join(dir, "session.json")
	commandMarker, syncMarker := installRunSessionValidationFailureMarkers(t, dir)
	releases := 0
	runEnvProfileTestReleaseHook = func() error {
		releases++
		return errors.New("release failure sentinel")
	}

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), []string{
		"--provider", "local-container",
		"--keep",
		"--lease-output", path,
		"--desktop",
		"--", "session-should-not-run",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "was not created with desktop=true") {
		t.Fatalf("error=%v want original exit 2 managed-capability failure", err)
	}
	if strings.Contains(err.Error(), "release failure sentinel") {
		t.Fatalf("cleanup failure replaced original validation error: %v", err)
	}
	if releases != 1 {
		t.Fatalf("fresh unreported lease release attempts=%d, want 1", releases)
	}
	if got := stderr.String(); !strings.Contains(got, "lease cleanup stopped=false") || !strings.Contains(got, `error="release failure sentinel"`) {
		t.Fatalf("cleanup failure not reported with lifecycle convention: %q", got)
	}
	assertRunSessionValidationStoppedBeforeWork(t, path, commandMarker, syncMarker)
}

func TestRunCommandRejectsLocalContainerLeaseOutputStopPoliciesBeforeAcquisition(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "fresh auto", args: nil, want: "requires --keep"},
		{name: "fresh keep on failure", args: []string{"--keep-on-failure"}, want: "requires --keep"},
		{name: "fresh stop success", args: []string{"--keep", "--stop-after", "success"}, want: "may release it"},
		{name: "fresh stop failure", args: []string{"--keep", "--stop-after", "failure"}, want: "may release it"},
		{name: "fresh stop always", args: []string{"--keep", "--stop-after", "always"}, want: "may release it"},
		{name: "reused stop success", args: []string{"--id", localContainerRunSessionTestLeaseID, "--stop-after", "success"}, want: "may release it"},
		{name: "reused stop failure", args: []string{"--id", localContainerRunSessionTestLeaseID, "--stop-after", "failure"}, want: "may release it"},
		{name: "reused stop always", args: []string{"--id", localContainerRunSessionTestLeaseID, "--stop-after", "always"}, want: "may release it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
			calls := 0
			runEnvProfileTestAcquireHook = func(AcquireRequest) { calls++ }
			t.Cleanup(func() { runEnvProfileTestAcquireHook = nil })
			args := []string{"--provider", "local-container", "--lease-output", filepath.Join(dir, "session.json")}
			args = append(args, tc.args...)
			args = append(args, "--", "true")
			err := (App{Stdout: io.Discard, Stderr: io.Discard}).runCommand(t.Context(), args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, tc.want) {
				t.Fatalf("error=%v want exit 2 containing %q", err, tc.want)
			}
			if calls != 0 {
				t.Fatalf("backend acquisition/resolution calls=%d, want 0", calls)
			}
		})
	}
}

func TestRunCommandRejectsReadyPoolLeaseOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "without keep", want: "requires --keep"},
		{name: "with keep", args: []string{"--keep"}, want: "--pool uses --pool-return for lifecycle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
			acquireCalls := 0
			runEnvProfileTestAcquireHook = func(AcquireRequest) { acquireCalls++ }
			t.Cleanup(func() { runEnvProfileTestAcquireHook = nil })

			args := []string{
				"--provider", "local-container",
				"--pool", "shared-linux",
				"--pool-return", "drain",
				"--lease-output", filepath.Join(dir, "session.json"),
			}
			args = append(args, tc.args...)
			args = append(args, "--", "true")
			err := (App{Stdout: io.Discard, Stderr: io.Discard}).runCommand(t.Context(), args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, tc.want) {
				t.Fatalf("error=%v, want exit 2 containing %q", err, tc.want)
			}
			if acquireCalls != 0 {
				t.Fatalf("backend acquisition calls=%d, want 0", acquireCalls)
			}
		})
	}
}

func TestRunCommandLocalContainerLeaseOutputSurvivesCommandAndSyncFailure(t *testing.T) {
	for _, phase := range []string{"command", "sync"} {
		t.Run(phase, func(t *testing.T) {
			commandScript := "#!/bin/sh\ncase \"$1\" in *session-command-fail*) test -f \"$CRABBOX_SESSION_PATH\" || exit 98; exit 19 ;; esac\nexit 0\n"
			dir, _ := setupLocalContainerRunSessionTest(t, commandScript)
			path := filepath.Join(dir, "session.json")
			t.Setenv("CRABBOX_SESSION_PATH", path)
			releases := 0
			runEnvProfileTestReleaseHook = func() error {
				releases++
				return nil
			}
			args := []string{
				"--provider", "local-container",
				"--keep",
				"--no-hydrate",
				"--lease-output", path,
			}
			if phase == "command" {
				args = append(args, "--no-sync", "--", "session-command-fail")
			} else {
				rsyncPath := filepath.Join(dir, "rsync")
				rsyncScript := "#!/bin/sh\ntest -f \"$CRABBOX_SESSION_PATH\" || exit 98\nexit 23\n"
				if err := os.WriteFile(rsyncPath, []byte(rsyncScript), 0o755); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--", "true")
			}
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), args); err == nil {
				t.Fatalf("%s failure unexpectedly succeeded\nstdout=%s\nstderr=%s", phase, stdout.String(), stderr.String())
			}
			session, _, _ := readRunSessionHandleTest(t, path)
			if session.LeaseID != localContainerRunSessionTestLeaseID || !session.Kept {
				t.Fatalf("session after %s failure=%#v", phase, session)
			}
			if releases != 0 {
				t.Fatalf("reported retained lease released %d time(s) after %s failure", releases, phase)
			}
		})
	}
}

func TestRunCommandLocalContainerLeaseOutputClaimFailureReleasesFreshLease(t *testing.T) {
	dir, _ := setupLocalContainerRunSessionTest(t, "#!/bin/sh\ncase \"$1\" in *session-should-not-run*) : > \"$CRABBOX_COMMAND_MARKER\" ;; esac\nexit 0\n")
	path := filepath.Join(dir, "session.json")
	marker := filepath.Join(dir, "command-ran")
	t.Setenv("CRABBOX_COMMAND_MARKER", marker)
	runEnvProfileTestAcquireHook = func(AcquireRequest) {
		if err := claimLeaseForRepoProvider(
			localContainerRunSessionTestLeaseID,
			"session-slug",
			"local-container",
			filepath.Join(dir, "other-repo"),
			time.Minute,
			false,
		); err != nil {
			t.Errorf("install conflicting claim: %v", err)
		}
	}
	releases := 0
	runEnvProfileTestReleaseHook = func() error {
		releases++
		return nil
	}

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), []string{
		"--provider", "local-container",
		"--keep",
		"--no-sync",
		"--no-hydrate",
		"--lease-output", path,
		"--", "session-should-not-run",
	})
	if err == nil || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("error=%v want claim failure\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if releases != 1 {
		t.Fatalf("fresh unreported lease released %d time(s), want 1", releases)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("lease output exists after claim failure: %v", statErr)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("user command ran after claim failure: %v", statErr)
	}
}

func TestRunCommandLocalContainerLeaseOutputWriteFailureReleasesFreshLease(t *testing.T) {
	dir, _ := setupLocalContainerRunSessionTest(t, "#!/bin/sh\ncase \"$1\" in *session-should-not-run*) : > \"$CRABBOX_COMMAND_MARKER\" ;; esac\nexit 0\n")
	path := filepath.Join(dir, "session.json")
	marker := filepath.Join(dir, "command-ran")
	t.Setenv("CRABBOX_COMMAND_MARKER", marker)
	runEnvProfileTestAcquireHook = func(AcquireRequest) {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Errorf("replace lease output with directory: %v", err)
		}
	}
	releasedID := ""
	runEnvProfileTestReleaseRequestHook = func(req ReleaseLeaseRequest) error {
		releasedID = req.Lease.LeaseID
		claim, exists, set := ServerLeaseClaimSnapshot(req.Lease.Server)
		if !set || !exists {
			return errors.New("release did not receive the exact recorded claim")
		}
		return removeLeaseClaimIfUnchangedAfter(req.Lease.LeaseID, claim, nil)
	}

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), []string{
		"--provider", "local-container",
		"--keep",
		"--no-sync",
		"--no-hydrate",
		"--lease-output", path,
		"--", "session-should-not-run",
	})
	if err == nil || !strings.Contains(err.Error(), "write "+path) {
		t.Fatalf("error=%v want lease output write failure\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if releasedID != localContainerRunSessionTestLeaseID {
		t.Fatalf("released lease=%q want %q", releasedID, localContainerRunSessionTestLeaseID)
	}
	if _, exists, readErr := readLeaseClaimWithPresence(localContainerRunSessionTestLeaseID); readErr != nil || exists {
		t.Fatalf("claim residue exists=%t err=%v", exists, readErr)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("user command ran before output failure cleanup: %v", statErr)
	}
}

func TestRunCommandAcceptsIsloBoundedFileEvidence(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "proof.txt")
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), []string{
		"--provider", "islo",
		"--no-sync",
		"--require-artifact", "reports/proof.txt",
		"--download", "reports/proof.txt=" + local,
		"--",
		"true",
	})
	if err != nil {
		t.Fatalf("run err=%v\nstderr=%s", err, stderr.String())
	}
	data, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "islo-test-proof" {
		t.Fatalf("download=%q", data)
	}
	if !strings.Contains(stderr.String(), "required artifact reports/proof.txt matched=1") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunCommandRejectsSlugWithExistingLease(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--id", "blue-lobster",
		"--slug", "update-flow-smoke",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("err=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--slug only applies when creating a new lease") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsDelegatedScriptStdinBeforeReading(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  strings.NewReader("should-not-be-consumed"),
	}).runCommand(context.Background(), []string{"--provider", "e2b", "--script-stdin"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "e2b delegates run execution; --script is not supported") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandPassesScriptToModuleDelegatedProvider(t *testing.T) {
	clearConfigEnv(t)
	runModuleRuntimeTestRequests = nil
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	t.Chdir(dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	script := filepath.Join(dir, "worker.mjs")
	if err := os.WriteFile(script, []byte("export default { fetch() { return new Response('ok') } }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "module-runtime-test",
		"--script", script,
	})
	if err != nil {
		t.Fatalf("run error=%v stderr=%q", err, stderr.String())
	}
	if len(runModuleRuntimeTestRequests) != 1 {
		t.Fatalf("run requests=%#v, want one", runModuleRuntimeTestRequests)
	}
	req := runModuleRuntimeTestRequests[0]
	if !req.ScriptRequested || req.Script == nil {
		t.Fatalf("script not loaded in request: %#v", req)
	}
	if req.Script.Source != script || string(req.Script.Data) != "export default { fetch() { return new Response('ok') } }\n" {
		t.Fatalf("script=%#v", req.Script)
	}
	if len(req.Command) != 0 {
		t.Fatalf("command=%v, want none", req.Command)
	}
}

func TestRunCommandInjectsReservedMetadataIntoDelegatedRequest(t *testing.T) {
	clearConfigEnv(t)
	runModuleRuntimeTestRequests = nil
	t.Setenv(runEnvLeaseID, "ambient-lease")
	t.Setenv(runEnvRunID, "ambient-run")
	t.Setenv(runEnvSlug, "ambient-slug")
	script := filepath.Join(t.TempDir(), "worker.mjs")
	if err := os.WriteFile(script, []byte("export default { fetch() { return new Response('ok') } }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "module-runtime-test",
		"--id", "cbx_delegated",
		"--allow-env", strings.Join(reservedRunEnvNames, ","),
		"--script", script,
	})
	if err != nil {
		t.Fatalf("run error=%v stderr=%q", err, stderr.String())
	}
	if len(runModuleRuntimeTestRequests) != 1 {
		t.Fatalf("run requests=%#v, want one", runModuleRuntimeTestRequests)
	}
	env := runModuleRuntimeTestRequests[0].Env
	if env[runEnvLeaseID] != "cbx_delegated" || env[runEnvSlug] != "" {
		t.Fatalf("delegated lease metadata=%#v", env)
	}
	if !regexp.MustCompile(`^run_[a-f0-9]{12}$`).MatchString(env[runEnvRunID]) {
		t.Fatalf("delegated run ID=%q", env[runEnvRunID])
	}
	if runModuleRuntimeTestRequests[0].RunID != env[runEnvRunID] {
		t.Fatalf("delegated request run ID=%q, env run ID=%q", runModuleRuntimeTestRequests[0].RunID, env[runEnvRunID])
	}
	for _, forbidden := range []string{"ambient-lease", "ambient-run", "ambient-slug"} {
		for _, value := range env {
			if value == forbidden {
				t.Fatalf("reserved override %q reached delegated request: %#v", forbidden, env)
			}
		}
	}
}

func TestPrintRunContextSummarySeparatesExecutionAndHistoryRunIDs(t *testing.T) {
	coord := &CoordinatorClient{BaseURL: "https://coordinator.example.test"}
	var local bytes.Buffer
	printRunContextSummary(&local, coord, Config{Provider: "aws", TargetOS: targetLinux}, Server{}, SSHTarget{}, "cbx_123", "run_local", "", "/work/repo", false, "")
	if got := local.String(); !strings.Contains(got, "run=run_local portal=- logs=-") || strings.Contains(got, "/runs/run_local") {
		t.Fatalf("local run context exposed nonexistent history URLs:\n%s", got)
	}

	var recorded bytes.Buffer
	printRunContextSummary(&recorded, coord, Config{Provider: "aws", TargetOS: targetLinux}, Server{}, SSHTarget{}, "cbx_123", "run_execution", "run_history", "/work/repo", false, "")
	for _, want := range []string{
		"run=run_execution",
		"portal=https://coordinator.example.test/portal/runs/run_history",
		"logs=https://coordinator.example.test/v1/runs/run_history/logs",
	} {
		if !strings.Contains(recorded.String(), want) {
			t.Fatalf("recorded run context missing %q:\n%s", want, recorded.String())
		}
	}
}

func TestPrintRunContextSummaryRedactsCoordinatorURLCredentials(t *testing.T) {
	username, userinfo := "fixture-user", "fixture-value"
	coord := &CoordinatorClient{BaseURL: fmt.Sprintf("https://%s:%s@coordinator.example.test/team?key=fixture-query#fixture-fragment", username, userinfo)}
	var output bytes.Buffer
	printRunContextSummary(&output, coord, Config{Provider: "aws", TargetOS: targetLinux}, Server{}, SSHTarget{}, "cbx_123", "run_execution", "run_history", "/work/repo", false, "")

	for _, want := range []string{
		"portal=https://<redacted>@coordinator.example.test/team/portal/runs/run_history",
		"logs=https://<redacted>@coordinator.example.test/team/v1/runs/run_history/logs",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("redacted run context missing %q:\n%s", want, output.String())
		}
	}
	for _, forbidden := range []string{username, userinfo, "fixture-query", "fixture-fragment"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("run context exposed %q:\n%s", forbidden, output.String())
		}
	}
}

func TestRunCommandInjectsReservedMetadataIntoStaticSSH(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	logPath := installRecordingSSH(t, dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	var stdout, stderr bytes.Buffer
	args := []string{
		"--provider", "ssh",
		"--static-host", "127.0.0.1",
		"--static-user", "runner",
		"--static-work-root", "/tmp/crabbox-static-test",
		"--no-sync",
		"--",
		"env",
	}
	for iteration := 0; iteration < 2; iteration++ {
		if err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), args); err != nil {
			t.Fatalf("static SSH run %d error=%v\nstdout=%s\nstderr=%s", iteration+1, err, stdout.String(), stderr.String())
		}
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(data)
	for _, pattern := range []string{
		`CRABBOX_LEASE_ID=.*static_127-0-0-1`,
		`CRABBOX_RUN_ID=.*run_[a-f0-9]{12}`,
		`CRABBOX_SLUG=`,
	} {
		if !regexp.MustCompile(pattern).MatchString(logText) {
			t.Fatalf("static SSH metadata %q missing:\n%s", pattern, logText)
		}
	}
	if got := strings.Count(logText, "protocol_action='acquire'"); got != 2 {
		t.Fatalf("static no-id owner acquisitions=%d want 2:\n%s", got, logText)
	}
}

func TestRunCommandPassesScriptStdinToModuleDelegatedProvider(t *testing.T) {
	runModuleRuntimeTestRequests = nil
	var stdout, stderr bytes.Buffer
	err := (App{
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  strings.NewReader("export default { fetch() { return new Response('stdin') } }\n"),
	}).runCommand(context.Background(), []string{
		"--provider", "module-runtime-test",
		"--script-stdin",
	})
	if err != nil {
		t.Fatalf("run error=%v stderr=%q", err, stderr.String())
	}
	if len(runModuleRuntimeTestRequests) != 1 {
		t.Fatalf("run requests=%#v, want one", runModuleRuntimeTestRequests)
	}
	req := runModuleRuntimeTestRequests[0]
	if req.Script == nil || req.Script.Source != "stdin" || string(req.Script.Data) != "export default { fetch() { return new Response('stdin') } }\n" {
		t.Fatalf("script=%#v", req.Script)
	}
}

func TestRunCommandRejectsModuleDelegatedTrailingCommand(t *testing.T) {
	runModuleRuntimeTestRequests = nil
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "module-runtime-test",
		"--",
		"node",
		"worker.mjs",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "module-runtime-test executes module source; trailing shell commands are not supported") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	if len(runModuleRuntimeTestRequests) != 0 {
		t.Fatalf("delegated run should not be called: %#v", runModuleRuntimeTestRequests)
	}
}

func TestRunCommandRejectsModuleDelegatedScriptStdinTrailingCommandBeforeReading(t *testing.T) {
	runModuleRuntimeTestRequests = nil
	stdin := &countingReader{data: "export default {}"}
	var stdout, stderr bytes.Buffer
	err := (App{
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  stdin,
	}).runCommand(context.Background(), []string{
		"--provider", "module-runtime-test",
		"--script-stdin",
		"--",
		"echo",
		"nope",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "module-runtime-test executes module source; trailing shell commands are not supported") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	if stdin.reads != 0 {
		t.Fatalf("stdin was read %d times before delegated option validation", stdin.reads)
	}
	if len(runModuleRuntimeTestRequests) != 0 {
		t.Fatalf("delegated run should not be called: %#v", runModuleRuntimeTestRequests)
	}
}

func TestRunCommandRejectsModuleDelegatedShellBeforeReadingScriptStdin(t *testing.T) {
	runModuleRuntimeTestRequests = nil
	stdin := &countingReader{data: "export default {}"}
	var stdout, stderr bytes.Buffer
	err := (App{
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  stdin,
	}).runCommand(context.Background(), []string{
		"--provider", "module-runtime-test",
		"--shell",
		"--script-stdin",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "module-runtime-test executes module source; --shell is not supported") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	if stdin.reads != 0 {
		t.Fatalf("stdin was read %d times before delegated option validation", stdin.reads)
	}
	if len(runModuleRuntimeTestRequests) != 0 {
		t.Fatalf("delegated run should not be called: %#v", runModuleRuntimeTestRequests)
	}
}

func TestRunCommandRejectsDelegatedEnvHelper(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "env.profile")
	if err := os.WriteFile(profile, []byte("API_TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "e2b",
		"--allow-env", "API_TOKEN",
		"--env-from-profile", profile,
		"--env-helper", "live",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "e2b delegates run execution; --env-helper is not supported") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsDelegatedProfileDoctor(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
profiles:
  qa:
    doctor:
      enabled: true
      tools: [node]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "e2b",
		"--profile", "qa",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "e2b delegates run execution; profile doctor is not supported") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsSyncOnlyScriptStdinBeforeReading(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  strings.NewReader("should-not-be-consumed"),
	}).runCommand(context.Background(), []string{"--sync-only", "--script-stdin"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--script cannot be combined with --sync-only") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsEnvHelperWithSyncOnly(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "env.profile")
	if err := os.WriteFile(profile, []byte("API_TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--sync-only",
		"--allow-env", "API_TOKEN",
		"--env-from-profile", profile,
		"--env-helper", "live",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--env-helper cannot be combined with --sync-only") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsProofAndArtifactsWithSyncOnly(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "artifact glob",
			args: []string{"--sync-only", "--artifact-glob", ".artifacts/**"},
			want: "--artifact-glob cannot be combined with --sync-only",
		},
		{
			name: "require artifact",
			args: []string{"--sync-only", "--require-artifact", "reports/data/manifest.json"},
			want: "--require-artifact cannot be combined with --sync-only",
		},
		{
			name: "emit proof",
			args: []string{"--sync-only", "--emit-proof", filepath.Join(t.TempDir(), "proof.md")},
			want: "--emit-proof cannot be combined with --sync-only",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), tt.args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("error=%v, want exit 2", err)
			}
			if !strings.Contains(exitErr.Message, tt.want) {
				t.Fatalf("message=%q want %q", exitErr.Message, tt.want)
			}
		})
	}
}

func TestRunCommandRejectsTargetOnlyProfileOutputsBeforeLease(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config string
		args   []string
		want   string
	}{
		{
			name: "native windows doctor",
			config: `
profiles:
  qa:
    doctor:
      enabled: true
      tools: [node]
`,
			args: []string{"--provider", "windows-env-helper-test", "--target", "windows", "--windows-mode", "normal", "--profile", "qa", "--", "true"},
			want: "profile doctor is not supported for native Windows targets",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			cfgPath := filepath.Join(dir, ".crabbox.yaml")
			t.Setenv("CRABBOX_CONFIG", cfgPath)
			if strings.TrimSpace(tt.config) != "" {
				if err := os.WriteFile(cfgPath, []byte(tt.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), tt.args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("error=%v, want exit 2", err)
			}
			if !strings.Contains(exitErr.Message, tt.want) {
				t.Fatalf("message=%q want %q", exitErr.Message, tt.want)
			}
			if strings.Contains(stderr.String(), "leased ") || strings.Contains(stderr.String(), "claim") {
				t.Fatalf("lease work happened before target rejection: %q", stderr.String())
			}
		})
	}
}

func TestRunCommandRejectsExistingLeaseTargetBeforeTouch(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	windowsEnvHelperTestTouchCount = 0
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "windows-env-helper-test",
		"--target", "windows",
		"--windows-mode", "normal",
		"--id", "cbx_win",
		"--artifact-glob", ".artifacts/**",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--artifact-glob is not supported for native Windows targets") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	if windowsEnvHelperTestTouchCount != 0 {
		t.Fatalf("touch count=%d, want 0", windowsEnvHelperTestTouchCount)
	}
}

func TestRunCommandTimingJSONRemainsFinalLineWithCleanup(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, sshPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	installWorkspaceOwnerAwareSSH(t, sshPath, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", sshPort)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))

	var stdout, stderr bytes.Buffer
	err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--timing-json",
		"--stop-after", "success",
		"--", "true",
	})
	if err != nil {
		t.Fatalf("runCommand error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) == 0 {
		t.Fatal("stderr was empty")
	}
	last := lines[len(lines)-1]
	var report TimingReport
	if err := json.Unmarshal([]byte(last), &report); err != nil {
		t.Fatalf("last stderr line is not timing JSON: %q\nfull stderr:\n%s", last, stderr.String())
	}
	if strings.Contains(last, "lease cleanup") {
		t.Fatalf("cleanup log appended to timing JSON: %q", last)
	}
	if report.LeaseStopped == nil || !*report.LeaseStopped {
		t.Fatalf("leaseStopped=%v, want true", report.LeaseStopped)
	}
}

func TestRunCommandTimingJSONSurfacesCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, sshPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	installWorkspaceOwnerAwareSSH(t, sshPath, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", sshPort)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	runEnvProfileTestReleaseErr = errors.New("release API unavailable")
	t.Cleanup(func() { runEnvProfileTestReleaseErr = nil })

	var stdout, stderr bytes.Buffer
	err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--timing-json",
		"--stop-after", "success",
		"--", "true",
	})
	if err != nil {
		t.Fatalf("runCommand error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	last := lines[len(lines)-1]
	var report TimingReport
	if err := json.Unmarshal([]byte(last), &report); err != nil {
		t.Fatalf("last stderr line is not timing JSON: %q\nfull stderr:\n%s", last, stderr.String())
	}
	if report.LeaseStopped == nil || *report.LeaseStopped {
		t.Fatalf("leaseStopped=%v, want false", report.LeaseStopped)
	}
	if !strings.Contains(report.LeaseStopErr, "release API unavailable") {
		t.Fatalf("leaseStopError=%q", report.LeaseStopErr)
	}
}

func TestRunCommandRequireArtifactFailsAfterSuccessfulCommand(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	downloadPath := filepath.Join(dir, "manifest.json")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, sshPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *"base64 <"*) printf 'ZG93bmxvYWRlZAo='; exit 0 ;;
  *"check_artifact_file()"*) printf 'missing required artifact: reports/data/manifest.json\n' >&2; exit 8 ;;
esac
exit 0
`
	installWorkspaceOwnerAwareSSH(t, sshPath, script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", sshPort)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))

	var stdout, stderr bytes.Buffer
	err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--keep-on-failure",
		"--timing-json",
		"--require-artifact", "reports/data/manifest.json",
		"--download", "reports/data/manifest.json=" + downloadPath,
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("error=%v, want exit 7\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	for _, want := range []string{"require artifacts", "missing required artifact: reports/data/manifest.json"} {
		if !strings.Contains(exitErr.Message, want) {
			t.Fatalf("message missing %q: %q", want, exitErr.Message)
		}
	}
	if _, err := os.Stat(downloadPath); !os.IsNotExist(err) {
		t.Fatalf("download ran before required artifact failure, stat err=%v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logData), "test -f 'reports/data/manifest.json' && base64 < 'reports/data/manifest.json'") {
		t.Fatalf("download command ran before required artifact check:\n%s", logData)
	}
	if !strings.Contains(stderr.String(), "keep-on-failure: kept lease=cbx_env_profile_test") {
		t.Fatalf("missing keep-on-failure hint after required artifact failure:\n%s", stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var report TimingReport
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
		t.Fatalf("last stderr line is not timing JSON: %q\nfull stderr:\n%s", lines[len(lines)-1], stderr.String())
	}
	if report.ExitCode != 7 {
		t.Fatalf("timing exitCode=%d, want 7\nreport=%#v", report.ExitCode, report)
	}
}

func TestRunCommandMacOSMissingRequiredArtifactE2E(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	downloadPath := filepath.Join(dir, "manifest.json")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, sshPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *"check_artifact_file()"*) printf 'missing required artifact: reports/data/manifest.json\n' >&2; exit 8 ;;
esac
exit 0
`
	installWorkspaceOwnerAwareSSH(t, sshPath, script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", sshPort)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	runEnvProfileTestAcquireLease = func(AcquireRequest) (LeaseTarget, error) {
		return LeaseTarget{
			Server:  Server{Provider: "run-env-profile-test"},
			SSH:     SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: sshPort, TargetOS: targetMacOS},
			LeaseID: "cbx_env_profile_test",
		}, nil
	}
	t.Cleanup(func() { runEnvProfileTestAcquireLease = nil })
	releases := 0
	runEnvProfileTestReleaseHook = func() error {
		releases++
		file, openErr := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if openErr != nil {
			return openErr
		}
		_, writeErr := file.WriteString("RELEASE\n")
		return errors.Join(writeErr, file.Close())
	}
	t.Cleanup(func() { runEnvProfileTestReleaseHook = nil })

	var stdout, stderr bytes.Buffer
	err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--stop-after", "always",
		"--require-artifact", "reports/data/manifest.json",
		"--download", "reports/data/manifest.json=" + downloadPath,
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("error=%v, want exit 7\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if releases != 1 {
		t.Fatalf("release calls=%d, want 1", releases)
	}
	if _, err := os.Stat(downloadPath); !os.IsNotExist(err) {
		t.Fatalf("download ran before required artifact failure, stat err=%v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	requireIndex := strings.Index(logText, "check_artifact_file()")
	releaseIndex := strings.Index(logText, "RELEASE")
	if requireIndex < 0 || releaseIndex <= requireIndex {
		t.Fatalf("required check and release were not ordered correctly:\n%s", logText)
	}
	if strings.Contains(logText, "test -f 'reports/data/manifest.json' && base64 < 'reports/data/manifest.json'") || strings.Contains(logText, "-artifacts.tgz") {
		t.Fatalf("download or artifact collection ran after missing required evidence:\n%s", logText)
	}
}

func TestRunCommandSSHArtifactE2E(t *testing.T) {
	for _, targetOS := range []string{targetLinux, targetMacOS} {
		t.Run(targetOS, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Chdir(dir)

			sshPath := filepath.Join(dir, "ssh")
			logPath := filepath.Join(dir, "ssh.log")
			remoteRoot := filepath.Join(dir, "remote-root")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					_ = conn.Close()
				}
			}()
			_, sshPort, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			script := `#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  mkdir\ -p*|cd\ *|bash\ -lc*|/bin/bash\ -lc*) exec sh -c "$cmd" ;;
esac
exit 0
`
			if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
			t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
			t.Setenv("CRABBOX_FAKE_SSH_PORT", sshPort)
			t.Setenv("CRABBOX_WORK_ROOT", remoteRoot)
			runEnvProfileTestAcquireLease = func(AcquireRequest) (LeaseTarget, error) {
				return LeaseTarget{
					Server:  Server{Provider: "run-env-profile-test"},
					SSH:     SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: sshPort, TargetOS: targetOS},
					LeaseID: "cbx_env_profile_test",
				}, nil
			}
			t.Cleanup(func() { runEnvProfileTestAcquireLease = nil })
			releases := 0
			runEnvProfileTestReleaseHook = func() error {
				releases++
				remoteArchives, globErr := filepath.Glob(filepath.Join(remoteRoot, "cbx_env_profile_test", "crabbox", ".crabbox", "*-artifacts.tgz"))
				if globErr != nil {
					return globErr
				}
				if len(remoteArchives) != 0 {
					return fmt.Errorf("remote archives still present at release: %v", remoteArchives)
				}
				file, openErr := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if openErr != nil {
					return openErr
				}
				_, writeErr := file.WriteString("RELEASE\n")
				return errors.Join(writeErr, file.Close())
			}
			t.Cleanup(func() { runEnvProfileTestReleaseHook = nil })

			var stdout, stderr bytes.Buffer
			err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
				"--provider", "run-env-profile-test",
				"--no-sync",
				"--stop-after", "always",
				"--require-artifact", "reports/data/manifest.json",
				"--artifact-glob", "reports/data/*.txt",
				"--", "sh", "-c", "mkdir -p reports/data && printf '{}\n' > reports/data/manifest.json && printf 'ok\n' > reports/data/quality.txt && printf 'data run complete\n'",
			})
			if err != nil {
				t.Fatalf("runCommand error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
			}
			if releases != 1 {
				t.Fatalf("release calls=%d, want 1", releases)
			}
			if !strings.Contains(stdout.String(), "data run complete") {
				t.Fatalf("stdout missing command output:\n%s", stdout.String())
			}
			for _, want := range []string{"required artifact reports/data/manifest.json matched=1", "artifact kind=artifact-glob"} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
				}
			}
			matches, err := filepath.Glob(filepath.Join(dir, ".crabbox", "runs", "*", "*-artifacts.tgz"))
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 1 {
				t.Fatalf("artifact tarballs=%#v, want exactly one", matches)
			}
			names := tarGzNames(t, matches[0])
			for _, want := range []string{"reports/data/manifest.json", "reports/data/quality.txt"} {
				if !stringSliceContains(names, want) {
					t.Fatalf("archive missing %q: %#v", want, names)
				}
			}
			if info, err := os.Stat(filepath.Dir(matches[0])); err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("artifact directory mode info=%v err=%v, want 0700", info, err)
			}
			if info, err := os.Stat(matches[0]); err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("artifact file mode info=%v err=%v, want 0600", info, err)
			}
			logData, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			logText := string(logData)
			previous := -1
			for _, want := range []string{"check_artifact_file()", "tar -czf", "base64 <", "rm -f --", "RELEASE"} {
				index := strings.Index(logText, want)
				if index < 0 {
					t.Fatalf("ssh log missing %q:\n%s", want, logText)
				}
				if index <= previous {
					t.Fatalf("ssh log has %q out of order:\n%s", want, logText)
				}
				previous = index
			}
		})
	}
}

func TestDelegatedRunArtifactScriptSkipsSlashNormalizedMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "etc", "passwd"), []byte("repo fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tarLog := filepath.Join(dir, "tar.log")
	tarPath := filepath.Join(binDir, "tar")
	if err := os.WriteFile(tarPath, []byte(`#!/bin/sh
printf '%s\n' "$@" > "$CRABBOX_FAKE_TAR_LOG"
out=""
prev=""
for arg do
  if [ "$prev" = "-czf" ]; then out="$arg"; break; fi
  prev="$arg"
done
: > "$out"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_TAR_LOG", tarLog)
	cmd := exec.Command("bash", "-c", DelegatedRunArtifactScript(nil, []string{".//etc/passwd"}, 16, 1024*1024))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("delegated artifact script failed: %v\n%s", err, out)
	}
	logData, err := os.ReadFile(tarLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logData), "/etc/passwd") {
		t.Fatalf("tar received slash-normalized path:\n%s", logData)
	}
	if !strings.Contains(string(logData), "--files-from\n/dev/null") {
		t.Fatalf("tar should receive an empty archive after rejecting slash-normalized matches:\n%s", logData)
	}
}

func TestDelegatedRunArtifactScriptRejectsDanglingRequiredArtifact(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join("reports", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing.json"), filepath.Join("reports", "data", "proof.json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	out, err := exec.Command("bash", "-c", DelegatedRunArtifactScript([]string{"reports/data/**/*.json"}, nil, 16, 1024*1024)).CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 8 {
		t.Fatalf("error=%v, want exit 8; output=%s", err, out)
	}
	if !strings.Contains(string(out), "missing required artifact: reports/data/**/*.json") {
		t.Fatalf("missing required artifact output:\n%s", out)
	}
}

func TestDelegatedRunArtifactScriptRejectsIntermediateSymlinkRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll("safe", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "proof.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join("safe", "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	out, err := exec.Command("bash", "-c", DelegatedRunArtifactScript([]string{"safe/link/*.txt"}, nil, 16, 1024*1024)).CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 8 {
		t.Fatalf("error=%v, want exit 8; output=%s", err, out)
	}
	if !strings.Contains(string(out), "missing required artifact: safe/link/*.txt") {
		t.Fatalf("missing required artifact output:\n%s", out)
	}
}

func TestRunCommandCleansEnvProfileWhenProbeFails(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, sshPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
cmd=""
for arg do
  cmd="$arg"
done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *"secret=true"*) exit 9 ;;
esac
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", sshPort)
	profile := filepath.Join(dir, "env.profile")
	if err := os.WriteFile(profile, []byte("API_TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--allow-env", "API_TOKEN",
		"--env-from-profile", profile,
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("error=%v, want exit 7", err)
	}
	if !strings.Contains(exitErr.Message, "probe env profile") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(logData), "rm -f --") || !regexp.MustCompile(`\.crabbox/env/run_[a-f0-9]{12}\.env`).Match(logData) {
		t.Fatalf("cleanup command missing from ssh log:\n%s", logData)
	}
}

func TestRunCommandHardFailsMissingJSRuntimeBeforeCommand(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	script := `#!/bin/sh
cmd=""
for arg do
  cmd="$arg"
done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *"command -v"*) printf '` + missingRemoteToolPrefix + `pnpm\n' ;;
esac
exit 0
`
	installWorkspaceOwnerAwareSSH(t, sshPath, script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--keep-on-failure",
		"--", "pnpm", "test:docs",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("error=%v, want exit 5", err)
	}
	for _, want := range []string{
		"remote raw workspace missing JS runtime tool(s): pnpm",
		"command starts with \"pnpm\"",
		"would fail before project code runs",
	} {
		if !strings.Contains(exitErr.Message, want) {
			t.Fatalf("message missing %q in %q", want, exitErr.Message)
		}
	}
	if strings.Contains(stderr.String(), "running on ") {
		t.Fatalf("remote command should not start after JS preflight failure:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "keep-on-failure: kept lease=cbx_env_profile_test") {
		t.Fatalf("keep-on-failure hint missing:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "releasing cbx_env_profile_test") {
		t.Fatalf("preflight failure should keep lease:\n%s", stderr.String())
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logData), "pnpm test:docs") {
		t.Fatalf("user command reached ssh log:\n%s", logData)
	}
}

func TestRunCommandKeepOnFailureKeepsLeaseAfterLocalActionsHydrationFailure(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	rsyncPath := filepath.Join(dir, "rsync")
	logPath := filepath.Join(dir, "ssh.log")
	configPath := filepath.Join(dir, ".crabbox.yaml")
	if err := os.WriteFile(configPath, []byte(`actions:
  workflow: .github/workflows/hydrate.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
cmd=""
for arg do
  cmd="$arg"
done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *"nohup sh -c"*) printf '123\n'; exit 0 ;;
  *"kill -0 '123'"*) printf 'exit=unknown\nno marker written\n'; exit 0 ;;
esac
exit 0
`
	installWorkspaceOwnerAwareSSH(t, sshPath, script)
	if err := os.WriteFile(rsyncPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	releases := 0
	runEnvProfileTestReleaseHook = func() error {
		releases++
		return nil
	}
	t.Cleanup(func() { runEnvProfileTestReleaseHook = nil })
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--keep-on-failure",
		"--", "pnpm", "test:docs",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("error=%v, want exit 7\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(exitErr.Message, "local Actions hydration exited before writing marker") {
		t.Fatalf("message missing local Actions hydration failure: %q", exitErr.Message)
	}
	if !strings.Contains(stderr.String(), "keep-on-failure: kept lease=cbx_env_profile_test") {
		t.Fatalf("keep-on-failure hint missing:\n%s", stderr.String())
	}
	if releases != 0 {
		t.Fatalf("local Actions hydration failure released kept lease %d time(s)\n%s", releases, stderr.String())
	}
}

func TestRunCommandSyncOnlyIgnoresJSCommandRuntime(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	script := `#!/bin/sh
cmd=""
for arg do
  cmd="$arg"
done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *"command -v"*) printf 'pnpm\n' ;;
esac
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--sync-only",
		"--", "pnpm", "test",
	})
	if err != nil {
		t.Fatalf("sync-only should ignore command runtime: %v", err)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logData), "command -v") {
		t.Fatalf("sync-only should not probe command runtime:\n%s", logData)
	}
}

func TestRunCommandSkipsJSRuntimePreflightWithForwardedPATH(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	script := `#!/bin/sh
cmd=""
for arg do
  cmd="$arg"
done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *"command -v"*) printf '` + missingRemoteToolPrefix + `pnpm\n' ;;
esac
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--allow-env", "PATH",
		"--", "pnpm", "test",
	})
	if err != nil {
		t.Fatalf("forwarded PATH should skip hard runtime preflight: %v", err)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logData), "command -v") {
		t.Fatalf("forwarded PATH should skip command runtime probe:\n%s", logData)
	}
	if !strings.Contains(string(logData), "pnpm") {
		t.Fatalf("user command missing from ssh log:\n%s", logData)
	}
}

func TestValidateRunEnvHelperTargetRejectsNativeWindows(t *testing.T) {
	err := validateRunEnvHelperTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, ".crabbox/env/live")
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--env-helper is not supported for native Windows targets yet") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	if err := validateRunEnvHelperTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, ".crabbox/env/live"); err != nil {
		t.Fatalf("wsl2 helper rejected: %v", err)
	}
}

func TestRunCommandRejectsWindowsEnvHelperBeforeRemoteCommands(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	script := `#!/bin/sh
printf 'ssh called\n' >> "$CRABBOX_FAKE_SSH_LOG"
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	profile := filepath.Join(dir, "env.profile")
	if err := os.WriteFile(profile, []byte("API_TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "windows-env-helper-test",
		"--target", "windows",
		"--windows-mode", "normal",
		"--static-host", "203.0.113.10",
		"--static-user", "crabbox",
		"--static-work-root", `C:\crabbox-test`,
		"--no-sync",
		"--allow-env", "API_TOKEN",
		"--env-from-profile", profile,
		"--env-helper", "live",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--env-helper is not supported for native Windows targets yet") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	if _, readErr := os.ReadFile(logPath); !os.IsNotExist(readErr) {
		t.Fatalf("ssh should not run before Windows env-helper rejection, readErr=%v", readErr)
	}
}

func isolateRunTestUserDirs(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "xdg-state"))
}

func TestFullResyncPrunesEvenWhenDeleteDisabled(t *testing.T) {
	for _, tt := range []struct {
		name       string
		delete     bool
		fullResync bool
		want       bool
	}{
		{name: "normal delete off", want: false},
		{name: "normal delete on", delete: true, want: true},
		{name: "full resync delete off", fullResync: true, want: true},
		{name: "full resync delete on", delete: true, fullResync: true, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPruneRemoteSync(tt.delete, tt.fullResync); got != tt.want {
				t.Fatalf("shouldPruneRemoteSync=%t want %t", got, tt.want)
			}
		})
	}
}

func TestFullResyncSeedsPruneManifestFromGit(t *testing.T) {
	if !shouldSeedRemotePruneManifest(false, true) {
		t.Fatal("full-resync should seed old manifest from git before pruning")
	}
	if !shouldSeedRemotePruneManifest(true, false) {
		t.Fatal("hydrated actions workspace should seed old manifest from git before pruning")
	}
	if shouldSeedRemotePruneManifest(false, false) {
		t.Fatal("normal non-hydrated sync should not seed old manifest from git")
	}
}

type fullResyncActionsTestOptions struct {
	noHydrate        bool
	syncOnly         bool
	failInvalidation bool
	adoptedWorkspace string
	workflow         string
	mutateWorkflow   bool
}

type fullResyncActionsTestResult struct {
	err             error
	stdout          string
	stderr          string
	events          []string
	resetCommand    string
	syncTarget      string
	hydrationScript string
}

func runFullResyncActionsTest(t *testing.T, opts fullResyncActionsTestOptions) fullResyncActionsTestResult {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell ssh fixture")
	}
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	repoRoot := filepath.Join(dir, "crabbox")
	workflowPath := filepath.Join(repoRoot, ".github", "workflows", "hydrate.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := opts.workflow
	if workflow == "" {
		workflow = `name: Hydrate
on:
  workflow_dispatch:
    inputs:
      crabbox_id:
        required: true
      crabbox_runner_label:
        required: true
      crabbox_keep_alive_minutes:
        required: true
jobs:
  hydrate:
    steps:
      - run: echo prepared-snapshot
`
	}
	if err := os.WriteFile(workflowPath, []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Crabbox Test"},
		{"add", "."},
		{"commit", "-qm", "fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Chdir(repoRoot)
	sshPath := filepath.Join(dir, "ssh")
	rsyncPath := filepath.Join(dir, "rsync")
	eventsPath := filepath.Join(dir, "events")
	hydratedPath := filepath.Join(dir, "hydrated")
	resetCommandPath := filepath.Join(dir, "reset-command")
	syncTargetPath := filepath.Join(dir, "sync-target")
	hydrationScriptPath := filepath.Join(dir, "hydration-script")
	configPath := filepath.Join(dir, ".crabbox.yaml")
	sshScript := `#!/bin/sh
remote=""
for arg do remote="$arg"; done
current=$remote
decoded_view=""
decode_depth=0
while [ "$decode_depth" -lt 3 ]; do
  case "$current" in
    *'payload_b64="'*'"; decoded=; if command -v base64'*)
      payload_b64=${current#*'payload_b64="'}
      payload_b64=${payload_b64%%'"; decoded=; if command -v base64'*}
      current=$(printf %s "$payload_b64" | /usr/bin/base64 --decode 2>/dev/null) ||
        current=$(printf %s "$payload_b64" | /usr/bin/base64 -d 2>/dev/null) ||
        current=$(printf %s "$payload_b64" | /usr/bin/base64 -D 2>/dev/null) || break
      decoded_view="$decoded_view
$current"
      decode_depth=$((decode_depth + 1))
      ;;
    *) break ;;
  esac
done
if [ -n "$decoded_view" ]; then remote=$decoded_view; fi
case "$remote" in
  *"protocol_action='acquire'"*) printf 'owner-acquire\n' >> "$CRABBOX_FAKE_EVENTS"; printf ACQUIRED; exit 0 ;;
  *"protocol_action='renew'"*) printf RENEWED; exit 0 ;;
  *"protocol_action='inspect'"*)
    if [ -e "$CRABBOX_FAKE_OWNER_CHILD" ]; then printf CHILD; else printf OWNED; fi
    exit 0
    ;;
  *"protocol_action='release'"*) printf 'owner-release\n' >> "$CRABBOX_FAKE_EVENTS"; printf RELEASED; exit 0 ;;
  *"rsync-stop."*"phase_live="*)
    : > "$CRABBOX_FAKE_OWNER_CHILD"
    printf '123\n'
    exit 0
    ;;
  *'touch "$HOME/.crabbox/workspace-owners/'*"rsync-stop."*)
    rm -f "$CRABBOX_FAKE_OWNER_CHILD"
    exit 0
    ;;
  *"nohup"*"cbx_env_profile_test.local.sh"*)
    printf 'hydrate\n' >> "$CRABBOX_FAKE_EVENTS"
    : > "$CRABBOX_FAKE_HYDRATED"
    printf '123\n'
    ;;
  *"rm -f"*".crabbox/actions/cbx_env_profile_test.env.sh"*)
    printf 'clear\n' >> "$CRABBOX_FAKE_EVENTS"
    ;;
  *"rm -f"*".crabbox/actions/cbx_env_profile_test.env"*)
    printf 'invalidate\n' >> "$CRABBOX_FAKE_EVENTS"
    if [ "${CRABBOX_FAKE_INVALIDATE_FAIL:-0}" = "1" ]; then
      printf 'marker invalidation denied\n' >&2
      exit 1
    fi
    if [ "${CRABBOX_FAKE_MUTATE_WORKFLOW:-0}" = "1" ]; then
      cat > "$CRABBOX_FAKE_WORKFLOW_PATH" <<'EOF'
jobs:
  hydrate:
    steps:
      - run: echo "${{ matrix.node }}"
EOF
    fi
    ;;
  *"rm -rf --"*)
    printf 'reset\n' >> "$CRABBOX_FAKE_EVENTS"
    printf '%s\n' "$remote" > "$CRABBOX_FAKE_RESET_COMMAND"
    ;;
  *"cat >"*"cbx_env_profile_test.local.sh"*)
    /bin/cat > "$CRABBOX_FAKE_HYDRATION_SCRIPT"
    exit 0
    ;;
  *"cat "*".crabbox/actions/cbx_env_profile_test.env"*)
    if [ -e "$CRABBOX_FAKE_HYDRATED" ]; then
      printf 'WORKSPACE=%s\n' "$CRABBOX_FAKE_CANONICAL_WORKSPACE"
      printf '%s\n' \
        'RUN_ID=local-cbx_env_profile_test' \
        'ENV_FILE=/home/crabbox/.crabbox/actions/cbx_env_profile_test.env.sh'
    else
      printf 'WORKSPACE=%s\n' "$CRABBOX_FAKE_ADOPTED_WORKSPACE"
      printf '%s\n' \
        'RUN_ID=123' \
        'ENV_FILE=/home/crabbox/.crabbox/actions/cbx_env_profile_test.env.sh'
    fi
    ;;
  *"pnpm"*"test"*)
    printf 'command\n' >> "$CRABBOX_FAKE_EVENTS"
    ;;
esac
/bin/cat >/dev/null || true
exit 0
`
	rsyncScript := `#!/bin/sh
printf 'sync\n' >> "$CRABBOX_FAKE_EVENTS"
last=""
for arg do last="$arg"; done
printf '%s\n' "$last" > "$CRABBOX_FAKE_SYNC_TARGET"
exit 0
`
	if err := os.WriteFile(sshPath, []byte(sshScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rsyncPath, []byte(rsyncScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("actions:\n  workflow: .github/workflows/hydrate.yml\nsync:\n  fingerprint: false\n  gitSeed: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
	t.Setenv("CRABBOX_FAKE_EVENTS", eventsPath)
	t.Setenv("CRABBOX_FAKE_HYDRATED", hydratedPath)
	t.Setenv("CRABBOX_FAKE_RESET_COMMAND", resetCommandPath)
	t.Setenv("CRABBOX_FAKE_SYNC_TARGET", syncTargetPath)
	t.Setenv("CRABBOX_FAKE_HYDRATION_SCRIPT", hydrationScriptPath)
	t.Setenv("CRABBOX_FAKE_WORKFLOW_PATH", workflowPath)
	t.Setenv("CRABBOX_FAKE_OWNER_CHILD", filepath.Join(dir, "owner-child"))
	canonicalWorkspace := remoteJoin(defaultConfig(), "cbx_env_profile_test", "crabbox")
	adoptedWorkspace := opts.adoptedWorkspace
	if adoptedWorkspace == "" {
		adoptedWorkspace = canonicalWorkspace
	}
	t.Setenv("CRABBOX_FAKE_CANONICAL_WORKSPACE", canonicalWorkspace)
	t.Setenv("CRABBOX_FAKE_ADOPTED_WORKSPACE", adoptedWorkspace)
	if opts.failInvalidation {
		t.Setenv("CRABBOX_FAKE_INVALIDATE_FAIL", "1")
	}
	if opts.mutateWorkflow {
		t.Setenv("CRABBOX_FAKE_MUTATE_WORKFLOW", "1")
	}
	args := []string{
		"--provider", "run-env-profile-test",
		"--id", "cbx_env_profile_test",
		"--full-resync",
	}
	if opts.noHydrate {
		args = append(args, "--no-hydrate")
	}
	if opts.syncOnly {
		args = append(args, "--sync-only")
	} else {
		args = append(args, "--", "pnpm", "test")
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), args)
	eventsData, readErr := os.ReadFile(eventsPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	readOptional := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return string(data)
	}
	return fullResyncActionsTestResult{
		err:             err,
		stdout:          stdout.String(),
		stderr:          stderr.String(),
		events:          strings.Fields(string(eventsData)),
		resetCommand:    readOptional(resetCommandPath),
		syncTarget:      readOptional(syncTargetPath),
		hydrationScript: readOptional(hydrationScriptPath),
	}
}

func assertRunEvents(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events=%v want %v", got, want)
	}
}

func TestRunCommandFullResyncRehydratesAdoptedActionsWorkspaceInOrderUnderOwner(t *testing.T) {
	result := runFullResyncActionsTest(t, fullResyncActionsTestOptions{})
	if result.err != nil {
		t.Fatalf("run error=%v\nstdout=%s\nstderr=%s", result.err, result.stdout, result.stderr)
	}
	assertRunEvents(t, result.events, []string{"owner-acquire", "invalidate", "reset", "sync", "clear", "hydrate", "command", "owner-release"})
	canonicalWorkspace := remoteJoin(defaultConfig(), "cbx_env_profile_test", "crabbox")
	for name, value := range map[string]string{
		"reset command":    result.resetCommand,
		"sync target":      result.syncTarget,
		"hydration script": result.hydrationScript,
	} {
		if !strings.Contains(value, canonicalWorkspace) {
			t.Fatalf("%s does not use canonical workspace %q:\n%s", name, canonicalWorkspace, value)
		}
	}
}

func TestRunCommandFullResyncRejectsUnsupportedWorkflowBeforeRemoteMutation(t *testing.T) {
	result := runFullResyncActionsTest(t, fullResyncActionsTestOptions{workflow: `jobs:
  hydrate:
    steps:
      - run: echo "${{ matrix.node }}"
`})
	var exitErr ExitError
	if !AsExitError(result.err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2\nstdout=%s\nstderr=%s", result.err, result.stdout, result.stderr)
	}
	assertRunEvents(t, result.events, []string{"owner-acquire", "owner-release"})
}

func TestRunCommandFullResyncUsesPreparedWorkflowSnapshot(t *testing.T) {
	result := runFullResyncActionsTest(t, fullResyncActionsTestOptions{mutateWorkflow: true})
	if result.err != nil {
		t.Fatalf("run error=%v\nstdout=%s\nstderr=%s", result.err, result.stdout, result.stderr)
	}
	if !strings.Contains(result.hydrationScript, "prepared-snapshot") || strings.Contains(result.hydrationScript, "matrix.node") {
		t.Fatalf("hydration did not use prepared workflow snapshot:\n%s", result.hydrationScript)
	}
}

func TestRunCommandFullResyncRejectsNonCanonicalAdoptedActionsWorkspace(t *testing.T) {
	result := runFullResyncActionsTest(t, fullResyncActionsTestOptions{adoptedWorkspace: "/work/actions/crabbox"})
	var exitErr ExitError
	if !AsExitError(result.err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2\nstdout=%s\nstderr=%s", result.err, result.stdout, result.stderr)
	}
	if !strings.Contains(exitErr.Message, "local hydration uses") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	assertRunEvents(t, result.events, []string{"owner-acquire", "owner-release"})
}

func TestRunCommandFullResyncNoHydrateFailsBeforeResetOrCommand(t *testing.T) {
	result := runFullResyncActionsTest(t, fullResyncActionsTestOptions{noHydrate: true})
	var exitErr ExitError
	if !AsExitError(result.err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2\nstdout=%s\nstderr=%s", result.err, result.stdout, result.stderr)
	}
	if !strings.Contains(exitErr.Message, "cannot rehydrate") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	assertRunEvents(t, result.events, []string{"owner-acquire", "owner-release"})
}

func TestRunCommandFullResyncInvalidationFailureStopsBeforeResetOrCommand(t *testing.T) {
	result := runFullResyncActionsTest(t, fullResyncActionsTestOptions{failInvalidation: true})
	var exitErr ExitError
	if !AsExitError(result.err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("error=%v, want exit 7\nstdout=%s\nstderr=%s", result.err, result.stdout, result.stderr)
	}
	if !strings.Contains(exitErr.Message, "invalidate GitHub Actions hydration marker") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	assertRunEvents(t, result.events, []string{"owner-acquire", "invalidate", "owner-release"})
}

func TestRunCommandFullResyncSyncOnlyInvalidatesWithoutRehydrate(t *testing.T) {
	const adoptedWorkspace = "/work/actions/crabbox"
	result := runFullResyncActionsTest(t, fullResyncActionsTestOptions{syncOnly: true, adoptedWorkspace: adoptedWorkspace})
	if result.err != nil {
		t.Fatalf("run error=%v\nstdout=%s\nstderr=%s", result.err, result.stdout, result.stderr)
	}
	assertRunEvents(t, result.events, []string{"owner-acquire", "invalidate", "reset", "sync", "owner-release"})
	if !strings.Contains(result.resetCommand, adoptedWorkspace) || !strings.Contains(result.syncTarget, adoptedWorkspace) {
		t.Fatalf("sync-only did not preserve adopted workspace reset/sync:\nreset=%s\nsync=%s", result.resetCommand, result.syncTarget)
	}
	if result.hydrationScript != "" {
		t.Fatalf("sync-only unexpectedly installed hydration script:\n%s", result.hydrationScript)
	}
}

func TestAllowRemoteSyncMassDeletionsForIncludeWhitelist(t *testing.T) {
	cfg := baseConfig()
	t.Setenv("CRABBOX_ALLOW_MASS_DELETIONS", "")
	if allowRemoteSyncMassDeletions(cfg, false) {
		t.Fatal("ordinary sync should keep mass deletion guard enabled")
	}
	if !allowRemoteSyncMassDeletions(cfg, true) {
		t.Fatal("hydrated actions workspace should allow mass deletions")
	}
	cfg.Sync.Includes = []string{"src"}
	if !allowRemoteSyncMassDeletions(cfg, false) {
		t.Fatal("sync.include should allow intentional whitelist pruning")
	}
	cfg.Sync.Includes = nil
	t.Setenv("CRABBOX_ALLOW_MASS_DELETIONS", "1")
	if !allowRemoteSyncMassDeletions(cfg, false) {
		t.Fatal("env override should allow mass deletions")
	}
}

func TestRunCommandRejectsApplyLocalPatchWithoutFreshPR(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{"--apply-local-patch", "--", "true"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--apply-local-patch requires --fresh-pr") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsFullResyncWithNoSync(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{"--full-resync", "--no-sync", "--", "true"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--full-resync cannot be combined with --no-sync") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandPreflightsLocalOutputOptions(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "malformed download", args: []string{"--download", "out.bin", "--", "true"}, want: "--download expects remote=local"},
		{name: "missing capture directory", args: []string{"--capture-stdout", filepath.Join(t.TempDir(), "missing", "stdout.bin"), "--", "true"}, want: "capture stdout:"},
		{name: "missing stderr capture directory", args: []string{"--capture-stderr", filepath.Join(t.TempDir(), "missing", "stderr.bin"), "--", "true"}, want: "capture stderr:"},
		{name: "missing lease output directory", args: []string{"--lease-output", filepath.Join(t.TempDir(), "missing", "session.json"), "--", "true"}, want: "lease output:"},
		{name: "same lease output and proof path", args: []string{"--lease-output", "session.json", "--emit-proof", "session.json", "--", "true"}, want: "lease output and emit proof paths must be different"},
		{name: "same capture path", args: []string{"--capture-stdout", "run.log", "--capture-stderr", "run.log", "--", "true"}, want: "paths must be different"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), tt.args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("error=%v, want exit 2", err)
			}
			if !strings.Contains(exitErr.Message, tt.want) {
				t.Fatalf("message=%q want %q", exitErr.Message, tt.want)
			}
		})
	}
}

func TestFailureStreamCaptureRetainsMetadataAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.bin")
	capture := &failureStreamCapture{label: "stdout", explicitPath: path}
	writer, explicit, err := capture.writer(io.Discard, &phaseMarkerWriter{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !explicit {
		t.Fatal("explicit capture was not selected")
	}
	if _, ok := capture.metadata(); ok {
		t.Fatal("capture metadata became available before close")
	}
	payload := []byte{0, 1, 2, 3, 4}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := capture.closeAfterStream(nil, 0, io.Discard); err != nil {
		t.Fatal(err)
	}
	metadata, ok := capture.metadata()
	if !ok {
		t.Fatal("capture metadata missing after close")
	}
	if metadata.Label != "stdout" || metadata.Path != path || metadata.Bytes != int64(len(payload)) {
		t.Fatalf("metadata=%+v", metadata)
	}
}

func TestRunCommandProofReportsExplicitStreamCaptures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh fixture")
	}
	tests := []struct {
		name          string
		mode          string
		captureStdout bool
		captureStderr bool
		stdoutData    []byte
		stderrData    []byte
		proofContains []string
		proofExcludes []string
	}{
		{
			name:          "text stdout capture",
			mode:          "text-stdout",
			captureStdout: true,
			stdoutData:    []byte("captured text stdout\n"),
			proofExcludes: []string{"captured text stdout", "(no console output captured)"},
		},
		{
			name:          "binary control stdout capture",
			mode:          "binary-stdout",
			captureStdout: true,
			stdoutData:    []byte{0, 1, 0x1b, '[', '3', '1', 'm', 'S', 'E', 'C', 'R', 'E', 'T', 0xff},
			proofExcludes: []string{"SECRET", "\x00", "\x01", "\x1b", "(no console output captured)"},
		},
		{
			name:          "both streams captured",
			mode:          "both",
			captureStdout: true,
			captureStderr: true,
			stdoutData:    []byte("stdout-only\n"),
			stderrData:    []byte("stderr-only\n"),
			proofExcludes: []string{"stdout-only", "stderr-only", "(no console output captured)"},
		},
		{
			name:          "live stderr and captured stdout",
			mode:          "live-stderr",
			captureStdout: true,
			stdoutData:    []byte("hidden stdout\n"),
			stderrData:    []byte("visible live stderr\n"),
			proofContains: []string{"visible live stderr"},
			proofExcludes: []string{"hidden stdout", "(no console output captured)"},
		},
		{
			name:          "no output and no captures",
			mode:          "none",
			proofContains: []string{"(no console output captured)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			sshPath := filepath.Join(dir, "ssh")
			sshScript := `#!/bin/sh
remote=""
for arg do remote="$arg"; done
case "$remote" in
  *"proof-capture-fixture"*)
    case "$CRABBOX_TEST_CAPTURE_MODE" in
      text-stdout) printf 'captured text stdout\n' ;;
      binary-stdout) printf '\000\001\033[31mSECRET\377' ;;
      both) printf 'stdout-only\n'; printf 'stderr-only\n' >&2 ;;
      live-stderr) printf 'hidden stdout\n'; printf 'visible live stderr\n' >&2 ;;
    esac
    ;;
esac
exit 0
`
			installWorkspaceOwnerAwareSSH(t, sshPath, sshScript)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
			t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
			t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
			t.Setenv("CRABBOX_TEST_CAPTURE_MODE", tt.mode)

			proofPath := filepath.Join(dir, "proof.md")
			args := []string{
				"--provider", "run-env-profile-test",
				"--no-sync",
				"--no-hydrate",
				"--keep",
				"--emit-proof", proofPath,
			}
			var stdoutPath, stderrPath string
			if tt.captureStdout {
				stdoutPath = filepath.Join(dir, "stdout`\n# proof-injection.bin")
				args = append(args, "--capture-stdout", stdoutPath)
			}
			if tt.captureStderr {
				stderrPath = filepath.Join(dir, "stderr.bin")
				args = append(args, "--capture-stderr", stderrPath)
			}
			args = append(args, "--", "proof-capture-fixture")
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), args); err != nil {
				t.Fatalf("run error=%v\nstdout=%q\nstderr=%s", err, stdout.Bytes(), stderr.String())
			}
			proofData, err := os.ReadFile(proofPath)
			if err != nil {
				t.Fatal(err)
			}
			proof := string(proofData)
			if tt.captureStdout {
				got, err := os.ReadFile(stdoutPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, tt.stdoutData) {
					t.Fatalf("stdout capture=%q want=%q", got, tt.stdoutData)
				}
				metadata := fmt.Sprintf("captured stream=stdout path=%s bytes=%d", quoteProofCapturePath(stdoutPath), len(tt.stdoutData))
				if !strings.Contains(proof, metadata) {
					t.Fatalf("proof missing stdout metadata %q:\n%s", metadata, proof)
				}
			}
			if tt.captureStderr {
				got, err := os.ReadFile(stderrPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, tt.stderrData) {
					t.Fatalf("stderr capture=%q want=%q", got, tt.stderrData)
				}
				metadata := fmt.Sprintf("captured stream=stderr path=%s bytes=%d", quoteProofCapturePath(stderrPath), len(tt.stderrData))
				if !strings.Contains(proof, metadata) {
					t.Fatalf("proof missing stderr metadata %q:\n%s", metadata, proof)
				}
			}
			if tt.captureStdout && tt.captureStderr {
				if strings.Index(proof, "captured stream=stdout") > strings.Index(proof, "captured stream=stderr") {
					t.Fatalf("capture metadata order is not stdout then stderr:\n%s", proof)
				}
			}
			for _, want := range tt.proofContains {
				if !strings.Contains(proof, want) {
					t.Fatalf("proof missing %q:\n%s", want, proof)
				}
			}
			for _, forbidden := range tt.proofExcludes {
				if strings.Contains(proof, forbidden) {
					t.Fatalf("proof contains forbidden %q:\n%s", forbidden, proof)
				}
			}
		})
	}
}

func TestWriteRunLeaseOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	session := &RunSessionHandle{
		Provider:       "blacksmith-testbox",
		LeaseID:        "tbx_abc123",
		Slug:           "blue-lobster",
		Kept:           true,
		CleanupCommand: "crabbox stop --provider blacksmith-testbox tbx_abc123",
	}
	if err := writeRunLeaseOutput(path, session); err != nil {
		t.Fatal(err)
	}
	var got RunSessionHandle
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider != session.Provider || got.LeaseID != session.LeaseID || !got.Kept || got.CleanupCommand != session.CleanupCommand {
		t.Fatalf("session=%#v", got)
	}
	if err := writeRunLeaseOutput(filepath.Join(t.TempDir(), "unsupported.json"), nil); err == nil || !strings.Contains(err.Error(), "provider did not return a session handle") {
		t.Fatalf("unsupported err=%v", err)
	}
}

func TestEnvForwardingSummaryRedactsSecretValues(t *testing.T) {
	t.Setenv("CRABBOX_ENV_ALLOW", "OPENAI_API_KEY,CI")
	var buf bytes.Buffer
	printEnvForwardingSummary(&buf, "aws", "forwarded", []string{"OPENAI_API_KEY", "CI"}, map[string]string{
		"OPENAI_API_KEY": "sk-live-secret",
		"CI":             "1",
	})
	got := buf.String()
	if strings.Contains(got, "sk-live-secret") {
		t.Fatalf("summary leaked value: %s", got)
	}
	for _, want := range []string{
		"provider=aws",
		"behavior=forwarded",
		"OPENAI_API_KEY=set len=14 secret=true",
		"CI=set",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q in %q", want, got)
		}
	}
}

func TestPhaseMarkerParsingAndTimingJSON(t *testing.T) {
	if name, ok := phaseNameFromLine("CRABBOX_PHASE: install deps"); !ok || name != "install-deps" {
		t.Fatalf("phase=%q ok=%t", name, ok)
	}
	if _, ok := phaseNameFromLine("not a marker"); ok {
		t.Fatal("unexpected phase marker")
	}

	var buf bytes.Buffer
	err := writeTimingJSON(&buf, timingReportFromRun("aws", "cbx_123", "blue-crab", runTimings{
		command: 1500 * time.Millisecond,
		commandPhases: []timingPhase{
			{Name: "install", Ms: 500},
			{Name: "build", Ms: 1000},
		},
	}, 2*time.Second, 1))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		CommandPhases []struct {
			Name string `json:"name"`
			Ms   int64  `json:"ms"`
		} `json:"commandPhases"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.CommandPhases) != 2 || got.CommandPhases[0].Name != "install" || got.CommandPhases[1].Ms != 1000 {
		t.Fatalf("unexpected command phases: %#v", got.CommandPhases)
	}
}

func TestPhaseMarkerWritersKeepStreamBuffersSeparate(t *testing.T) {
	tracker := newCommandPhaseTracker(time.Now())
	stdoutWriter := &phaseMarkerWriter{tracker: tracker}
	stderrWriter := &phaseMarkerWriter{tracker: tracker}

	if _, err := stdoutWriter.Write([]byte("CRABBOX_PHASE:build")); err != nil {
		t.Fatal(err)
	}
	if _, err := stderrWriter.Write([]byte("CRABBOX_PHASE:test\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stdoutWriter.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}

	phases := tracker.Finish(time.Now())
	names := make(map[string]bool)
	for _, phase := range phases {
		names[phase.Name] = true
	}
	if !names["build"] || !names["test"] {
		t.Fatalf("phases=%#v want independent stdout/stderr markers", phases)
	}
}

func TestPhaseMarkerWriterParsesCompleteLinesBeforePendingTruncation(t *testing.T) {
	tracker := newCommandPhaseTracker(time.Now())
	writer := &phaseMarkerWriter{tracker: tracker}

	if _, err := writer.Write([]byte("CRABBOX_PHASE:build\n" + strings.Repeat("x", phaseMarkerPendingBytes*2))); err != nil {
		t.Fatal(err)
	}
	if len(writer.pending) > phaseMarkerPendingBytes {
		t.Fatalf("pending=%d want <=%d", len(writer.pending), phaseMarkerPendingBytes)
	}

	phases := tracker.Finish(time.Now())
	names := make(map[string]bool)
	for _, phase := range phases {
		names[phase.Name] = true
	}
	if !names["build"] {
		t.Fatalf("phases=%#v want build marker from large chunk", phases)
	}
}

func TestRemotePreflightRawWorkspaceHydrateWarning(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	lines := remotePreflightWorkspaceLines(cfg, SSHTarget{TargetOS: targetLinux}, "cbx_123", "/work/crabbox/cbx_123/repo", false, "", true)
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"workspace=raw",
		"workdir=/work/crabbox/cbx_123/repo",
		"hydrate_supported=true",
		"hydrate_suggestion=crabbox actions hydrate --id cbx_123 --provider aws --target linux --workflow .github/workflows/hydrate.yml",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("preflight text missing %q in %q", want, got)
		}
	}
}

func TestRemotePreflightNativeWindowsHydrateSuggestionUsesGitHubRunner(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	lines := remotePreflightWorkspaceLines(cfg, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, "cbx_123", `C:\crabbox\cbx_123\repo`, false, "", true)
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"hydrate_supported=true",
		"--target windows",
		"--windows-mode normal",
		"--github-runner",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("preflight text missing %q in %q", want, got)
		}
	}
}

func TestRemotePreflightRawWorkspaceSkipsHydrateSuggestionWithoutWorkflow(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	lines := remotePreflightWorkspaceLines(cfg, SSHTarget{TargetOS: targetLinux}, "cbx_123", "/work/crabbox/cbx_123/repo", false, "", true)
	got := strings.Join(lines, "\n")
	if strings.Contains(got, "hydrate_suggestion=") {
		t.Fatalf("unexpected hydrate suggestion without workflow: %q", got)
	}
	if !strings.Contains(got, "workspace=raw") {
		t.Fatalf("preflight text missing workspace: %q", got)
	}
}

func TestRemoteCapabilityPreflightCommandUsesCommandEnvironment(t *testing.T) {
	got := remoteCapabilityPreflightCommand("/home/runner/work/repo/repo", map[string]string{"CI": "1"}, []string{"/home/runner/.crabbox/actions/cbx-123.env.sh"}, []string{"node", "bun"})
	for _, want := range []string{
		"cd '/home/runner/work/repo/repo'",
		". '/home/runner/.crabbox/actions/cbx-123.env.sh'",
		"CI='1'",
		"pwd -P",
		`exe="$1"; shift`,
		"preflight_cmd '\\''node'\\'' '\\''node'\\'' node --version",
		"preflight_cmd '\\''bun'\\'' '\\''bun'\\'' bun --version",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("preflight command missing %q in %q", want, got)
		}
	}
}

func TestWindowsRemoteCapabilityPreflightCommandUsesCommandEnvironment(t *testing.T) {
	got := windowsRemoteCapabilityPreflightCommand(`C:\crabbox\repo`, map[string]string{"CI": "1"}, []string{`.crabbox\env\run.env`}, []string{"powershell", "node", "bun", "pwsh"})
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\repo'`,
		`Import-CrabboxEnvFile '.crabbox\env\run.env'`,
		`$env:CI = '1'`,
		`Test-Value "user" { whoami }`,
		`Test-Value "cwd" { (Get-Location).Path }`,
		`Test-Value "powershell"`,
		`Test-Tool 'node' 'node' @('--version')`,
		`Test-Tool 'bun' 'bun' @('--version')`,
		`Test-Tool 'pwsh' 'pwsh' @('--version')`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("windows preflight command missing %q in %q", want, decoded)
		}
	}
}

func TestWindowsRemoteCapabilityPreflightUploadsScriptBeforeRunning(t *testing.T) {
	dir := t.TempDir()
	logPath := installRecordingSSH(t, dir)
	cfg := defaultConfig()
	cfg.Run.PreflightTools = []string{"node"}
	target := SSHTarget{
		User:        "crabbox",
		Host:        "127.0.0.1",
		Port:        "22",
		TargetOS:    targetWindows,
		WindowsMode: windowsModeNormal,
	}
	env := map[string]string{"CRABBOX_LONG_ENV": strings.Repeat("x", 40000)}

	var out bytes.Buffer
	printRemoteCapabilityPreflight(context.Background(), &out, cfg, Server{}, target, "cbx_123", `C:\crabbox\repo`, []string{`.crabbox\env\run.env`}, true, "", true, env)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	commands := recordedSSHCommands(string(data))
	if len(commands) != 3 {
		t.Fatalf("ssh commands=%d want 3:\n%s", len(commands), data)
	}
	upload := decodePowerShellCommand(t, commands[0])
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\repo'`,
		`$stdin.CopyTo($memory)`,
		`.crabbox/preflight/`,
	} {
		if !strings.Contains(upload, want) {
			t.Fatalf("upload command missing %q in %q", want, upload)
		}
	}
	run := decodePowerShellCommand(t, commands[1])
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\repo'`,
		`$__crabboxPreflight = '.crabbox/preflight/`,
		`-File $__crabboxPreflight`,
	} {
		if !strings.Contains(run, want) {
			t.Fatalf("run command missing %q in %q", want, run)
		}
	}
	cleanup := decodePowerShellCommand(t, commands[2])
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\repo'`,
		`$__crabboxPreflight = '.crabbox/preflight/`,
		`if (Test-Path -LiteralPath $__crabboxPreflight)`,
		`Remove-Item -LiteralPath $__crabboxPreflight -Force -ErrorAction Stop`,
	} {
		if !strings.Contains(cleanup, want) {
			t.Fatalf("cleanup command missing %q in %q", want, cleanup)
		}
	}
	if strings.Contains(run, env["CRABBOX_LONG_ENV"]) || strings.Contains(upload, env["CRABBOX_LONG_ENV"]) || strings.Contains(cleanup, env["CRABBOX_LONG_ENV"]) {
		t.Fatalf("large preflight script leaked into encoded commands")
	}
}

func recordedSSHCommands(log string) []string {
	parts := strings.Split(log, "---\n")
	commands := make([]string, 0, len(parts))
	for _, part := range parts {
		command := strings.TrimSpace(part)
		if command != "" {
			commands = append(commands, command)
		}
	}
	return commands
}

func TestPreflightToolsForTargetFiltersByOS(t *testing.T) {
	got := preflightToolsForTarget(SSHTarget{TargetOS: targetMacOS}, []string{"node", "apt", "powershell", "bun"})
	if strings.Join(got, ",") != "node,bun" {
		t.Fatalf("mac tools=%v", got)
	}
	got = preflightToolsForTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, []string{"node", "apt", "powershell", "bun"})
	if strings.Join(got, ",") != "node,powershell,bun" {
		t.Fatalf("windows tools=%v", got)
	}
	got = preflightToolsForTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, []string{"node", "apt", "powershell", "bun"})
	if strings.Join(got, ",") != "node,apt,bun" {
		t.Fatalf("wsl2 tools=%v", got)
	}
	got = preflightToolsForTarget(SSHTarget{TargetOS: targetLinux}, []string{"none"})
	if len(got) != 0 {
		t.Fatalf("none tools=%v", got)
	}
	got = preflightToolsForTarget(SSHTarget{TargetOS: targetMacOS}, []string{"apt", "powershell"})
	if len(got) != 0 {
		t.Fatalf("unsupported mac tools=%v", got)
	}
}

func TestWindowsWSL2RemoteCapabilityPreflightUsesBoundedWrapper(t *testing.T) {
	dir := t.TempDir()
	logPath := installRecordingSSH(t, dir)
	stdinLog := filepath.Join(dir, "ssh.stdin")
	t.Setenv("CRABBOX_FAKE_SSH_STDIN_LOG", stdinLog)
	cfg := defaultConfig()
	cfg.Run.PreflightTools = []string{"node"}
	target := SSHTarget{
		User:        "crabbox",
		Host:        "127.0.0.1",
		Port:        "22",
		TargetOS:    targetWindows,
		WindowsMode: windowsModeWSL2,
	}

	var out bytes.Buffer
	printRemoteCapabilityPreflight(context.Background(), &out, cfg, Server{}, target, "cbx_123", `/work/repo`, nil, false, "", true, nil)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	commands := recordedSSHCommands(string(data))
	if len(commands) != 1 {
		t.Fatalf("ssh commands=%d want 1:\n%s", len(commands), data)
	}
	decoded := decodePowerShellCommand(t, commands[0])
	for _, want := range []string{
		`[Console]::OpenStandardInput().CopyTo($script)`,
		`$process.WaitForExit(15000)`,
		`throw "WSL2 command timed out after 15s"`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("WSL2 preflight command missing %q in %q", want, decoded)
		}
	}
	if strings.Contains(decoded, `preflight_cmd`) {
		t.Fatalf("WSL2 preflight script should be sent over stdin, not embedded in command: %q", decoded)
	}
	payloadBytes, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(payloadBytes)
	if !strings.Contains(payload, `preflight_cmd '\''node'\'' '\''node'\'' node --version`) {
		t.Fatalf("WSL2 preflight payload missing node probe in %q", payload)
	}
}

func TestRemotePreflightNonePrintsWorkspaceOnly(t *testing.T) {
	cfg := defaultConfig()
	cfg.Run.PreflightTools = []string{"none"}
	var out bytes.Buffer
	printRemoteCapabilityPreflight(context.Background(), &out, cfg, Server{}, SSHTarget{TargetOS: targetLinux}, "cbx_123", "/work/repo", nil, false, "", false, nil)
	got := out.String()
	if !strings.Contains(got, "remote preflight workspace=raw workdir=/work/repo hydrate_supported=false") {
		t.Fatalf("missing workspace summary: %q", got)
	}
	if strings.Contains(got, "remote preflight failed") || strings.Contains(got, "remote preflight user=") || strings.Contains(got, "remote preflight cwd=") {
		t.Fatalf("none should skip remote probes, got %q", got)
	}
}

func TestRemotePreflightPrintsEffectiveArchitectureEvidence(t *testing.T) {
	cfg := defaultConfig()
	cfg.Run.PreflightTools = []string{"none"}
	cfg.architectureExplicit = true
	server := Server{Labels: map[string]string{"architecture": ArchitectureARM64}}
	var out bytes.Buffer
	printRemoteCapabilityPreflight(context.Background(), &out, cfg, server, SSHTarget{TargetOS: targetLinux}, "cbx_123", "/work/repo", nil, false, "", false, nil)
	if !strings.Contains(out.String(), "remote preflight architecture=arm64\n") {
		t.Fatalf("missing architecture evidence: %q", out.String())
	}
}

func TestRemotePreflightOmitsUnassertedArchitectureLabel(t *testing.T) {
	cfg := defaultConfig()
	cfg.Run.PreflightTools = []string{"none"}
	server := Server{Labels: map[string]string{"architecture": ArchitectureARM64}}
	var out bytes.Buffer
	printRemoteCapabilityPreflight(context.Background(), &out, cfg, server, SSHTarget{TargetOS: targetLinux}, "cbx_123", "/work/repo", nil, false, "", false, nil)
	if strings.Contains(out.String(), "remote preflight architecture=") {
		t.Fatalf("omitted architecture printed stale evidence: %q", out.String())
	}
}

func TestValidatePreflightToolsRejectsUnknown(t *testing.T) {
	if err := validatePreflightTools([]string{"node", "bogus"}); err == nil {
		t.Fatal("expected unknown preflight tool error")
	}
	if err := validatePreflightTools([]string{"default", "bun"}); err != nil {
		t.Fatalf("default tools should validate: %v", err)
	}
}

func TestPythonPreflightToolValidationAndTargetFiltering(t *testing.T) {
	if err := validatePreflightTools([]string{"python", "python3"}); err != nil {
		t.Fatalf("python tools should validate: %v", err)
	}
	err := validatePreflightTools([]string{"python", "bogus", "python3"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, `unknown preflight tool "bogus"`) {
		t.Fatalf("error=%v, want exit 2 for bogus tool", err)
	}

	targets := []struct {
		name   string
		target SSHTarget
	}{
		{name: "linux", target: SSHTarget{TargetOS: targetLinux}},
		{name: "macos", target: SSHTarget{TargetOS: targetMacOS}},
		{name: "wsl2", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}},
		{name: "windows", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}},
	}
	for _, tt := range targets {
		t.Run(tt.name, func(t *testing.T) {
			got := preflightToolsForTarget(tt.target, []string{"python", "python3"})
			if strings.Join(got, ",") != "python,python3" {
				t.Fatalf("tools=%v", got)
			}
		})
	}

	for _, tool := range preflightToolsForTarget(SSHTarget{TargetOS: targetLinux}, nil) {
		if tool == "python" || tool == "python3" {
			t.Fatalf("%s must remain opt-in, default tools=%v", tool, defaultPreflightToolNames)
		}
	}
}

func TestPythonPreflightPOSIXProbeUsesLiteralExecutableAndMissingContract(t *testing.T) {
	script := remoteCapabilityPreflightCommand("/work/repo", nil, nil, []string{"python", "python3"})
	for _, want := range []string{
		`preflight_cmd '\''python'\'' '\''python'\'' python --version`,
		`preflight_cmd '\''python3'\'' '\''python3'\'' python3 --version`,
		`printf '\''%s=missing\n'\'' "$label"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("POSIX preflight script missing %q in %q", want, script)
		}
	}
}

func TestPythonPreflightWindowsProbeUsesLiteralExecutableAndMissingContract(t *testing.T) {
	script := windowsRemoteCapabilityPreflightScript(`C:\crabbox\repo`, nil, nil, []string{"python", "python3"})
	for _, want := range []string{
		`Test-Tool 'python' 'python' @('--version')`,
		`Test-Tool 'python3' 'python3' @('--version')`,
		`Write-Output ($Label + "=missing")`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("Windows preflight script missing %q in %q", want, script)
		}
	}
}

func TestCMakePreflightToolValidationAndTargetFiltering(t *testing.T) {
	if err := validatePreflightTools([]string{"cmake"}); err != nil {
		t.Fatalf("cmake should validate: %v", err)
	}

	targets := []struct {
		name   string
		target SSHTarget
	}{
		{name: "linux", target: SSHTarget{TargetOS: targetLinux}},
		{name: "macos", target: SSHTarget{TargetOS: targetMacOS}},
		{name: "wsl2", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}},
		{name: "native_windows", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}},
	}
	for _, tt := range targets {
		t.Run(tt.name, func(t *testing.T) {
			got := preflightToolsForTarget(tt.target, []string{"cmake"})
			if strings.Join(got, ",") != "cmake" {
				t.Fatalf("tools=%v", got)
			}
		})
	}

	for _, tool := range preflightToolsForTarget(SSHTarget{TargetOS: targetLinux}, nil) {
		if tool == "cmake" {
			t.Fatalf("cmake must remain opt-in, default tools=%v", defaultPreflightToolNames)
		}
	}
	tools := normalizePreflightToolNames([]string{"default,cmake,cmake"})
	if count := strings.Count(","+strings.Join(tools, ",")+",", ",cmake,"); count != 1 {
		t.Fatalf("default plus duplicate cmake tools=%v, cmake count=%d", tools, count)
	}
	if len(tools) != len(defaultPreflightToolNames)+1 {
		t.Fatalf("default plus cmake tools=%v, want %d entries", tools, len(defaultPreflightToolNames)+1)
	}
}

func TestCMakePreflightLiteralCommandGeneration(t *testing.T) {
	const posixProbe = `preflight_cmd '\''cmake'\'' '\''cmake'\'' cmake --version`
	posix := remoteCapabilityPreflightCommand("/work/repo", nil, nil, []string{"cmake"})
	if !strings.Contains(posix, posixProbe) {
		t.Fatalf("POSIX preflight script missing %q in %q", posixProbe, posix)
	}
	wsl2Tools := preflightToolsForTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, []string{"cmake"})
	wsl2 := remoteCapabilityPreflightCommand("/work/repo", nil, nil, wsl2Tools)
	if !strings.Contains(wsl2, posixProbe) {
		t.Fatalf("WSL2 preflight script missing %q in %q", posixProbe, wsl2)
	}
	windows := windowsRemoteCapabilityPreflightScript(`C:\crabbox\repo`, nil, nil, []string{"cmake"})
	if !strings.Contains(windows, `Test-Tool 'cmake' 'cmake' @('--version')`) {
		t.Fatalf("native Windows preflight script does not use literal cmake command: %q", windows)
	}
	for name, script := range map[string]string{"posix": posix, "wsl2": wsl2, "windows": windows} {
		if strings.Contains(script, "cmake3") {
			t.Fatalf("%s preflight script contains unsupported alias: %q", name, script)
		}
	}
}

func TestCMakePreflightPOSIXPresentFirstLineAndMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell behavior is covered on non-Windows CI")
	}

	run := func(t *testing.T, installCMake bool) string {
		t.Helper()
		binDir := t.TempDir()
		bash := "#!/bin/sh\nif [ \"$1\" = \"-lc\" ]; then exec /bin/bash --noprofile --norc -c \"$2\"; fi\nexec /bin/bash \"$@\"\n"
		if err := os.WriteFile(filepath.Join(binDir, "bash"), []byte(bash), 0o700); err != nil {
			t.Fatal(err)
		}
		for name, path := range map[string]string{"id": "/usr/bin/id", "sed": "/usr/bin/sed", "whoami": "/usr/bin/whoami"} {
			if err := os.Symlink(path, filepath.Join(binDir, name)); err != nil {
				t.Fatal(err)
			}
		}
		if installCMake {
			cmake := "#!/bin/sh\nprintf 'cmake version 9.8.7\\nignored second line\\n'\n"
			if err := os.WriteFile(filepath.Join(binDir, "cmake"), []byte(cmake), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		command := remoteCapabilityPreflightCommand(t.TempDir(), map[string]string{"PATH": binDir}, nil, []string{"cmake"})
		cmd := exec.Command("/bin/sh", "-c", command)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run POSIX preflight: %v\n%s", err, out)
		}
		return string(out)
	}

	present := run(t, true)
	if !strings.Contains(present, "cmake=cmake version 9.8.7\n") || strings.Contains(present, "ignored second line") {
		t.Fatalf("present output did not preserve first-line contract: %q", present)
	}
	missing := run(t, false)
	if !strings.Contains(missing, "cmake=missing\n") {
		t.Fatalf("missing output did not preserve diagnostic-only contract: %q", missing)
	}
}

func TestCMakePreflightNativeWindowsPresentAndMissing(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows PowerShell behavior requires windows-latest")
	}

	run := func(t *testing.T, installCMake bool) string {
		t.Helper()
		binDir := t.TempDir()
		if installCMake {
			cmake := "@echo off\r\necho cmake version 9.8.7\r\necho ignored second line\r\n"
			if err := os.WriteFile(filepath.Join(binDir, "cmake.cmd"), []byte(cmake), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		script := windowsRemoteCapabilityPreflightScript(t.TempDir(), map[string]string{"Path": binDir}, nil, []string{"cmake"})
		out, err := runWindowsPowerShellScript(t, script)
		if err != nil {
			t.Fatalf("run native Windows preflight: %v\n%s", err, out)
		}
		return strings.ReplaceAll(string(out), "\r\n", "\n")
	}

	present := run(t, true)
	if !strings.Contains(present, "cmake=cmake version 9.8.7\n") || strings.Contains(present, "ignored second line") {
		t.Fatalf("present output did not preserve first-line contract: %q", present)
	}
	missing := run(t, false)
	if !strings.Contains(missing, "cmake=missing\n") {
		t.Fatalf("missing output did not preserve diagnostic-only contract: %q", missing)
	}
}

func TestCMakePreflightUserAndRepositoryConfig(t *testing.T) {
	for _, source := range []string{"user", "repository"} {
		t.Run(source, func(t *testing.T) {
			clearConfigEnv(t)
			root := t.TempDir()
			home := filepath.Join(root, "home")
			repo := filepath.Join(root, "repo")
			if err := os.MkdirAll(repo, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("CRABBOX_CONFIG", "")
			t.Chdir(repo)

			path := filepath.Join(repo, ".crabbox.yaml")
			if source == "user" {
				path = userConfigPath()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, []byte("run:\n  preflightTools: [cmake]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(cfg.Run.PreflightTools, ","); got != "cmake" {
				t.Fatalf("%s run.preflightTools=%q", source, got)
			}
			if err := validatePreflightTools(cfg.Run.PreflightTools); err != nil {
				t.Fatalf("%s cmake config should validate: %v", source, err)
			}
		})
	}
}

func TestCMakeUnknownPreflightToolFailsBeforeAcquire(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	acquireCalls := 0
	runEnvProfileTestAcquireHook = func(AcquireRequest) { acquireCalls++ }
	t.Cleanup(func() { runEnvProfileTestAcquireHook = nil })

	err := (App{Stdout: io.Discard, Stderr: io.Discard}).runCommand(t.Context(), []string{
		"--provider", runEnvProfileTestProvider{}.Name(),
		"--preflight",
		"--preflight-tools", "cmake,cmake3",
		"--no-sync",
		"--no-hydrate",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, `unknown preflight tool "cmake3"`) {
		t.Fatalf("error=%v, want exit 2 for unknown cmake alias", err)
	}
	if acquireCalls != 0 {
		t.Fatalf("Acquire called %d time(s) before unknown preflight tool rejection", acquireCalls)
	}
}

func TestRawSocketPreflightToolValidationAndTargetFiltering(t *testing.T) {
	if err := validatePreflightTools([]string{rawSocketPreflightTool}); err != nil {
		t.Fatalf("raw_socket should validate: %v", err)
	}

	tests := []struct {
		name   string
		target SSHTarget
		want   string
	}{
		{name: "linux", target: SSHTarget{TargetOS: targetLinux}, want: rawSocketPreflightTool},
		{name: "wsl2", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, want: rawSocketPreflightTool},
		{name: "macos", target: SSHTarget{TargetOS: targetMacOS}},
		{name: "native_windows", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(preflightToolsForTarget(tt.target, []string{rawSocketPreflightTool}), ",")
			if got != tt.want {
				t.Fatalf("tools=%q want %q", got, tt.want)
			}
		})
	}

	for _, tool := range preflightToolsForTarget(SSHTarget{TargetOS: targetLinux}, nil) {
		if tool == rawSocketPreflightTool {
			t.Fatalf("raw_socket must remain opt-in, default tools=%v", defaultPreflightToolNames)
		}
	}
}

func TestRawSocketPreflightConfigRoundTripAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crabbox.yaml")
	if err := os.WriteFile(path, []byte("run:\n  preflightTools: [raw_socket]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	if err := applyConfigFile(&cfg, path, configPathTrust{trusted: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Run.PreflightTools, ","); got != rawSocketPreflightTool {
		t.Fatalf("run.preflightTools=%q", got)
	}
	if err := validatePreflightTools(cfg.Run.PreflightTools); err != nil {
		t.Fatalf("configured raw_socket should validate: %v", err)
	}
}

func TestRawSocketOnlyPreflightUsesOneBoundedRemoteCommand(t *testing.T) {
	dir := t.TempDir()
	logPath := installRecordingSSH(t, dir)
	cfg := defaultConfig()
	cfg.Run.PreflightTools = []string{rawSocketPreflightTool}
	target := SSHTarget{
		User:     "crabbox",
		Host:     "127.0.0.1",
		Port:     "22",
		TargetOS: targetLinux,
	}

	var out bytes.Buffer
	printRemoteCapabilityPreflight(context.Background(), &out, cfg, Server{}, target, "cbx_123", "/work/repo", nil, false, "", true, nil)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	commands := recordedSSHCommands(string(data))
	if len(commands) != 1 {
		t.Fatalf("raw_socket-only SSH commands=%d want 1:\n%s", len(commands), data)
	}
	if !strings.Contains(commands[0], rawSocketProbePrefix) {
		t.Fatalf("raw_socket command missing protocol: %q", commands[0])
	}
	if !strings.Contains(out.String(), "remote preflight raw_socket=unavailable\n") {
		t.Fatalf("unexpected preflight output: %q", out.String())
	}
}

func TestRawSocketPreflightScriptIsMinimalAndProtocolBound(t *testing.T) {
	script := rawSocketPreflightScript()
	for _, want := range []string{
		"for interpreter_name in python3 python",
		`"$interpreter" -B -E -S -c ` + shellQuote(rawSocketPythonProbe) + ` >/dev/null 2>&1`,
		`for sudo_executable in '/usr/bin/sudo' '/bin/sudo' '/usr/local/bin/sudo' '/run/wrappers/bin/sudo' '/run/setuid-programs/sudo'`,
		`"$sudo_executable" -n -- /bin/sh -c `,
		`case "$1" in`,
		`python3|python) ;;`,
		rawSocketSudoPATH,
		rawSocketProbeDirect,
		rawSocketProbeSudo,
		rawSocketProbeUnavailable,
		rawSocketProbeMissing,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("raw socket script missing %q in %q", want, script)
		}
	}
	if strings.Count(script, rawSocketPythonProbe) != 2 {
		t.Fatalf("direct and sudo attempts must use identical code: %q", script)
	}
	if strings.Contains(script, "command -v sudo") || strings.Contains(script, " sudo -n") {
		t.Fatalf("sudo must not resolve through workload PATH: %q", script)
	}
	if strings.Count(rawSocketPythonProbe, "socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_RAW)") != 1 || strings.Count(rawSocketPythonProbe, "probe.close()") != 1 {
		t.Fatalf("unexpected Python probe: %q", rawSocketPythonProbe)
	}
	if strings.Contains(rawSocketPythonProbe, "PermissionError") || strings.Contains(script, " -I ") || !strings.Contains(script, " -B -E -S -c ") {
		t.Fatalf("probe must remain bytecode-free and compatible with the literal python fallback: %q", script)
	}
	for _, forbidden := range []string{".bind(", ".connect(", ".send(", ".sendto(", ".recv(", "scapy", "tcpdump", "setcap", "cap_net_raw", "apt ", "install "} {
		if strings.Contains(strings.ToLower(rawSocketPythonProbe), forbidden) {
			t.Fatalf("Python probe contains forbidden operation %q: %q", forbidden, rawSocketPythonProbe)
		}
	}
	ordinary := remoteCapabilityPreflightCommand("/work/repo", nil, nil, []string{"node"})
	if strings.Contains(ordinary, rawSocketProbePrefix) || strings.Contains(ordinary, "sudo -n --") {
		t.Fatalf("ordinary preflight gained raw-socket elevation: %q", ordinary)
	}

	workdir := "/work/runner's repo"
	envFile := "/work/env file's/run.env"
	command := rawSocketCapabilityPreflightCommand(workdir, map[string]string{"CI": "value with ' quote"}, []string{envFile})
	for _, want := range []string{
		"cd " + shellQuote(workdir),
		". " + shellQuote(envFile),
		"CI=" + shellQuote("value with ' quote"),
		shellQuote(rawSocketPythonProbe),
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("quoted raw socket command missing %q in %q", want, command)
		}
	}
}

func TestRawSocketPreflightStatesAndPermissionFallback(t *testing.T) {
	tests := []struct {
		name          string
		pythonName    string
		directExit    int
		sudoExit      int
		installPython bool
		installSudo   bool
		want          string
		wantSudo      bool
		errexit       bool
	}{
		{name: "direct", pythonName: "python3", directExit: 0, sudoExit: 0, installPython: true, installSudo: true, want: "direct"},
		{name: "sudo", pythonName: "python3", directExit: 77, sudoExit: 0, installPython: true, installSudo: true, want: "sudo", wantSudo: true},
		{name: "sudo_with_errexit", pythonName: "python3", directExit: 77, sudoExit: 0, installPython: true, installSudo: true, want: "sudo", wantSudo: true, errexit: true},
		{name: "permission_denied_without_sudo", pythonName: "python3", directExit: 77, sudoExit: 0, installPython: true, want: "unavailable"},
		{name: "permission_denied_sudo_fails", pythonName: "python3", directExit: 77, sudoExit: 78, installPython: true, installSudo: true, want: "unavailable", wantSudo: true},
		{name: "other_error_does_not_elevate", pythonName: "python3", directExit: 78, sudoExit: 0, installPython: true, installSudo: true, want: "unavailable"},
		{name: "python_fallback", pythonName: "python", directExit: 0, installPython: true, want: "direct"},
		{name: "probe_missing", want: "probe_missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			sudoLog := filepath.Join(dir, "sudo.log")
			if tt.installPython {
				python := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' '%s'
if [ "${CRABBOX_TEST_RAW_SOCKET_SUDO:-}" = 1 ]; then exit %d; fi
exit %d
`, rawSocketProbeDirect, tt.sudoExit, tt.directExit)
				if err := os.WriteFile(filepath.Join(dir, tt.pythonName), []byte(python), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tt.installSudo {
				sudo := `#!/bin/sh
printf 'called\n' >> ` + shellQuote(sudoLog) + `
if [ "$1" = -n ]; then shift; fi
if [ "$1" = -- ]; then shift; fi
CRABBOX_TEST_RAW_SOCKET_SUDO=1
export CRABBOX_TEST_RAW_SOCKET_SUDO
exec "$@"
`
				if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(sudo), 0o700); err != nil {
					t.Fatal(err)
				}
			}

			script := rawSocketPreflightScriptWithSudoEnvironment([]string{filepath.Join(dir, "sudo")}, dir)
			if tt.errexit {
				script = "set -e\n" + script
			}
			cmd := exec.Command("/bin/bash", "-c", script)
			cmd.Env = []string{"PATH=" + dir}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("probe script: %v: %s", err, out)
			}
			if got := parseRawSocketProbeOutput(string(out)); got != tt.want {
				t.Fatalf("state=%q want %q, output=%q", got, tt.want, out)
			}
			_, statErr := os.Stat(sudoLog)
			if gotSudo := statErr == nil; gotSudo != tt.wantSudo {
				t.Fatalf("sudo called=%t want %t", gotSudo, tt.wantSudo)
			}
		})
	}
}

func TestRawSocketPreflightTriesEachTrustedSudoCandidate(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sudo.log")
	python := `#!/bin/sh
if [ "${CRABBOX_TEST_RAW_SOCKET_SUDO:-}" = 1 ]; then exit 0; fi
exit 77
`
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte(python), 0o700); err != nil {
		t.Fatal(err)
	}
	firstSudo := filepath.Join(dir, "sudo-first")
	if err := os.WriteFile(firstSudo, []byte("#!/bin/sh\nprintf 'first\\n' >> "+shellQuote(logPath)+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	secondSudo := filepath.Join(dir, "sudo-second")
	sudo := `#!/bin/sh
printf 'second\n' >> ` + shellQuote(logPath) + `
if [ "$1" = -n ]; then shift; fi
if [ "$1" = -- ]; then shift; fi
CRABBOX_TEST_RAW_SOCKET_SUDO=1
export CRABBOX_TEST_RAW_SOCKET_SUDO
exec "$@"
`
	if err := os.WriteFile(secondSudo, []byte(sudo), 0o700); err != nil {
		t.Fatal(err)
	}

	script := rawSocketPreflightScriptWithSudoEnvironment([]string{firstSudo, secondSudo}, dir)
	cmd := exec.Command("/bin/bash", "-c", script)
	cmd.Env = []string{"PATH=" + dir}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe script: %v: %s", err, out)
	}
	if got := parseRawSocketProbeOutput(string(out)); got != "sudo" {
		t.Fatalf("state=%q want sudo, output=%q", got, out)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "first\nsecond\n" {
		t.Fatalf("sudo attempts=%q", got)
	}
}

func TestRawSocketPreflightFallsBackAfterBrokenPython3(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "interpreters.log")
	python3 := "#!/bin/sh\nprintf 'python3\\n' >> " + shellQuote(logPath) + "\nexit 78\n"
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte(python3), 0o700); err != nil {
		t.Fatal(err)
	}
	python := "#!/bin/sh\nprintf 'python\\n' >> " + shellQuote(logPath) + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "python"), []byte(python), 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/bash", "-c", rawSocketPreflightScriptWithSudoEnvironment(nil, dir))
	cmd.Env = []string{"PATH=" + dir}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe script: %v: %s", err, out)
	}
	if got := parseRawSocketProbeOutput(string(out)); got != "direct" {
		t.Fatalf("state=%q want direct, output=%q", got, out)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "python3\npython\n" {
		t.Fatalf("interpreter order=%q", got)
	}
}

func TestRawSocketPreflightDoesNotResolveSudoInterpreterFromWorkloadPATH(t *testing.T) {
	dir := t.TempDir()
	sudoPath := t.TempDir()
	privilegedMarker := filepath.Join(dir, "privileged")
	python := `#!/bin/sh
if [ "${CRABBOX_TEST_RAW_SOCKET_SUDO:-}" = 1 ]; then
  printf 'ran\n' > ` + shellQuote(privilegedMarker) + `
  exit 0
fi
exit 77
`
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte(python), 0o700); err != nil {
		t.Fatal(err)
	}
	sudoLog := filepath.Join(dir, "sudo.log")
	sudo := `#!/bin/sh
printf 'called\n' >> ` + shellQuote(sudoLog) + `
if [ "$1" = -n ]; then shift; fi
if [ "$1" = -- ]; then shift; fi
CRABBOX_TEST_RAW_SOCKET_SUDO=1
export CRABBOX_TEST_RAW_SOCKET_SUDO
exec "$@"
`
	sudoExecutable := filepath.Join(dir, "sudo")
	if err := os.WriteFile(sudoExecutable, []byte(sudo), 0o700); err != nil {
		t.Fatal(err)
	}

	script := rawSocketPreflightScriptWithSudoEnvironment([]string{sudoExecutable}, sudoPath)
	cmd := exec.Command("/bin/bash", "-c", script)
	cmd.Env = []string{"PATH=" + dir}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe script: %v: %s", err, out)
	}
	if got := parseRawSocketProbeOutput(string(out)); got != "unavailable" {
		t.Fatalf("state=%q want unavailable, output=%q", got, out)
	}
	if _, err := os.Stat(sudoLog); err != nil {
		t.Fatalf("sudo fallback not attempted: %v", err)
	}
	if _, err := os.Stat(privilegedMarker); !os.IsNotExist(err) {
		t.Fatalf("workload PATH interpreter ran through sudo: %v", err)
	}
}

func TestRawSocketPythonProbeIgnoresWorkdirModuleShadow(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "shadow-imported")
	shadow := "from pathlib import Path\nPath(" + fmt.Sprintf("%q", marker) + ").write_text('imported')\n"
	if err := os.WriteFile(filepath.Join(dir, "socket.py"), []byte(shadow), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-B", "-E", "-S", "-c", rawSocketPythonProbe)
	cmd.Dir = dir
	_ = cmd.Run()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("workdir socket.py was imported: %v", err)
	}
	cmd = exec.Command(python, "-B", "-E", "-S", "-c", `import sys; print("site" in sys.modules, sys.dont_write_bytecode)`)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("check isolated Python startup: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "False True" {
		t.Fatalf("isolated bytecode-free Python startup not active: %q", got)
	}
}

func TestRawSocketPreflightProtocolRejectsAmbiguousMarkersAndToleratesNoise(t *testing.T) {
	valid := map[string]string{
		rawSocketProbeDirect:                       "direct",
		rawSocketProbeSudo:                         "sudo",
		rawSocketProbeUnavailable:                  "unavailable",
		rawSocketProbeMissing:                      "probe_missing",
		"login banner\n" + rawSocketProbeDirect:    "direct",
		rawSocketProbeSudo + "\nlogout diagnostic": "sudo",
	}
	for output, want := range valid {
		if got := parseRawSocketProbeOutput(output); got != want {
			t.Fatalf("output %q produced %q want %q", output, got, want)
		}
	}
	for _, out := range []string{
		rawSocketProbeDirect + "\n" + rawSocketProbeUnavailable,
		rawSocketProbePrefix + "unknown",
		"raw_socket=direct",
		"direct",
		"",
	} {
		if got := parseRawSocketProbeOutput(out); got != "unavailable" {
			t.Fatalf("ambiguous output %q produced %q", out, got)
		}
	}
}

func TestRawSocketPreflightAttemptIsBounded(t *testing.T) {
	started := time.Now()
	state := runRawSocketCapabilityPreflightWithRunner(context.Background(), 20*time.Millisecond, func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return rawSocketProbeDirect, ctx.Err()
	})
	if state != "unavailable" {
		t.Fatalf("state=%q want unavailable", state)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("probe timeout took %s", elapsed)
	}
}

func TestDelegatedPreflightPrintsUnsupportedMessage(t *testing.T) {
	var stderr bytes.Buffer
	printDelegatedPreflightUnsupported(&stderr, "e2b")
	got := stderr.String()
	for _, want := range []string{"provider=e2b", "delegated unsupported", "provider owns workspace and command transport"} {
		if !strings.Contains(got, want) {
			t.Fatalf("message missing %q in %q", want, got)
		}
	}
}

func TestRemoteFailureCaptureCommandAvoidsDuplicateDirectoryChildren(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for POSIX capture command test")
	}
	workdir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workdir, "test-results"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "test-results", "failure.log"), []byte("failure"), 0o666); err != nil {
		t.Fatal(err)
	}

	command := remoteFailureCaptureCommand(workdir, ".crabbox/capture.tar.gz")
	if out, err := exec.Command("bash", "-lc", command).CombinedOutput(); err != nil {
		t.Fatalf("capture command failed: %v\n%s", err, out)
	}

	file, err := os.Open(filepath.Join(workdir, ".crabbox", "capture.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	counts := make(map[string]int)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		counts[header.Name]++
	}
	if counts["test-results/failure.log"] != 1 {
		t.Fatalf("test-results/failure.log count=%d entries=%#v", counts["test-results/failure.log"], counts)
	}
}

func TestRemoteRemoveFailureCaptureCommandRemovesBundle(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for POSIX cleanup command test")
	}
	workdir := t.TempDir()
	captureDir := filepath.Join(workdir, ".crabbox")
	if err := os.Mkdir(captureDir, 0o777); err != nil {
		t.Fatal(err)
	}
	capturePath := filepath.Join(captureDir, "capture.tar.gz")
	if err := os.WriteFile(capturePath, []byte("secret logs"), 0o666); err != nil {
		t.Fatal(err)
	}

	command := remoteRemoveFailureCaptureCommand(workdir, ".crabbox/capture.tar.gz")
	if out, err := exec.Command("bash", "-lc", command).CombinedOutput(); err != nil {
		t.Fatalf("cleanup command failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(capturePath); !os.IsNotExist(err) {
		t.Fatalf("capture bundle should be removed, stat err=%v", err)
	}
}

func TestFailureEnvSummaryRedactsSecretValues(t *testing.T) {
	got := failureEnvSummary([]string{"API_TOKEN", "CI", "MISSING"}, map[string]string{
		"API_TOKEN": "secret-value",
		"CI":        "1",
	})
	if strings.Contains(got, "secret-value") {
		t.Fatalf("summary leaked secret: %s", got)
	}
	for _, want := range []string{
		"API_TOKEN=present len=12 secret=true",
		"CI=present",
		"MISSING=missing",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q in %q", want, got)
		}
	}
}

func TestWriteLocalFailureBundleIncludesMetadataStreamsAndRemoteFiles(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for POSIX capture command test")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := filepath.Join(dir, "stderr.log")
	if err := os.WriteFile(stdoutPath, []byte("remote stdout\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, []byte("remote stderr\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	remoteWorkdir := filepath.Join(dir, "remote")
	if err := os.MkdirAll(filepath.Join(remoteWorkdir, "test-results"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteWorkdir, "test-results", "failure.log"), []byte("failure"), 0o666); err != nil {
		t.Fatal(err)
	}
	command := remoteFailureCaptureCommand(remoteWorkdir, ".crabbox/remote.tar.gz")
	if out, err := exec.Command("bash", "-lc", command).CombinedOutput(); err != nil {
		t.Fatalf("remote capture command failed: %v\n%s", err, out)
	}

	const (
		urlUser     = "url-user"
		urlPassword = "url-password"
	)
	knownSecrets := []string{
		"configured-overlap-secret",
		"overlap-secret",
		"configured-profile-secret",
		"forwarded-exact-secret",
		"q7x",
		"z9q",
		urlUser,
		urlPassword,
		"query-token",
		"assignment-token",
		"client-token",
		"header-token",
	}
	commandDisplay := strings.Join([]string{
		"deploy --region us-west-2 --artifact report.json",
		"--endpoint=https://" + urlUser + ":" + urlPassword + "@example.test/v1?token=query-token&trace=keep",
		"api-key=assignment-token client-secret:client-token --token=forwarded-exact-secret",
		"configured-overlap-secret overlap-secret configured-profile-secret",
		"forwarded-exact-secret forwarded-exact-secret q7x",
		"--header 'Authorization: Bearer header-token'",
		"z9q",
	}, " ")
	local, _, err := writeLocalFailureBundle("bundle.tar.gz", filepath.Join(remoteWorkdir, ".crabbox", "remote.tar.gz"), FailureCaptureMetadata{
		Provider:       "aws",
		LeaseID:        "cbx_123",
		Slug:           "blue-crab",
		RunID:          "run_123",
		CommandDisplay: commandDisplay,
		Workdir:        "/work/crabbox/cbx_123/repo",
		ExitCode:       7,
		Timing:         timingReport{Provider: "aws", LeaseID: "cbx_123", ExitCode: 7},
		EnvAllow:       []string{"API_TOKEN", "OVERLAP_TOKEN", "SHORT_SECRET", "TRAILING_SECRET"},
		Env: map[string]string{
			"API_TOKEN":       "forwarded-exact-secret",
			"OVERLAP_TOKEN":   "overlap-secret",
			"SHORT_SECRET":    "q7x",
			"TRAILING_SECRET": "z9q ",
		},
		Config: Config{
			Provider:    "aws",
			TargetOS:    targetLinux,
			Class:       "standard",
			IdleTimeout: time.Minute,
			TTL:         time.Hour,
			WorkRoot:    "/work/crabbox",
			CoordToken:  "configured-overlap-secret",
			Profiles: map[string]ProfileConfig{
				"proof": {Env: map[string]string{"SERVICE_TOKEN": "configured-profile-secret"}},
			},
		},
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	contents := readTarGzContents(t, local)
	for _, want := range []string{
		"crabbox-artifacts/crabbox-run.json",
		"crabbox-artifacts/timings.json",
		"crabbox-artifacts/env.redacted.txt",
		"crabbox-artifacts/config.redacted.txt",
		"crabbox-artifacts/stdout.log",
		"crabbox-artifacts/stderr.log",
		"crabbox-artifacts/remote/test-results/failure.log",
	} {
		if _, ok := contents[want]; !ok {
			t.Fatalf("bundle missing %q; entries=%#v", want, contents)
		}
	}
	var runMetadata struct {
		Command string `json:"command"`
		RunID   string `json:"runId"`
	}
	if err := json.Unmarshal(contents["crabbox-artifacts/crabbox-run.json"], &runMetadata); err != nil {
		t.Fatalf("decode crabbox-run.json: %v", err)
	}
	if runMetadata.RunID != "run_123" {
		t.Fatalf("run metadata ID=%q, want durable run_123", runMetadata.RunID)
	}
	for _, want := range []string{"deploy", "--region us-west-2", "--artifact report.json", "example.test/v1", "trace=keep"} {
		if !strings.Contains(runMetadata.Command, want) {
			t.Fatalf("redacted command lost harmless context %q: %q", want, runMetadata.Command)
		}
	}
	if !strings.Contains(runMetadata.Command, diagnosticRedaction) {
		t.Fatalf("command metadata contains no redaction marker: %q", runMetadata.Command)
	}
	for name, data := range contents {
		for _, secret := range knownSecrets {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatalf("bundle entry %s leaked known secret %q", name, secret)
			}
		}
	}
}

func TestWriteLocalFailureBundleConfinesRemoteLinks(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	remoteTar := filepath.Join(dir, "remote.tar.gz")
	file, err := os.Create(remoteTar)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	entries := []struct {
		header tar.Header
		data   string
	}{
		{header: tar.Header{Name: "logs/result.txt", Typeflag: tar.TypeReg, Mode: 0o600, Size: 6}, data: "result"},
		{header: tar.Header{Name: "logs/result-link", Typeflag: tar.TypeSymlink, Linkname: "result.txt", Mode: 0o777}},
		{header: tar.Header{Name: "logs/result-hard", Typeflag: tar.TypeLink, Linkname: "logs/result.txt", Mode: 0o600}},
		{header: tar.Header{Name: "logs/symlink-out", Typeflag: tar.TypeSymlink, Linkname: "../../outside", Mode: 0o777}},
		{header: tar.Header{Name: "logs/windows-link-out", Typeflag: tar.TypeSymlink, Linkname: `C:\outside`, Mode: 0o777}},
		{header: tar.Header{Name: "logs/hardlink-out", Typeflag: tar.TypeLink, Linkname: "/etc/passwd", Mode: 0o600}},
		{header: tar.Header{Name: "logs/empty-link", Typeflag: tar.TypeSymlink, Mode: 0o777}},
		{header: tar.Header{Name: "logs/pipe", Typeflag: tar.TypeFifo, Mode: 0o600}},
	}
	for _, entry := range entries {
		if err := tarWriter.WriteHeader(&entry.header); err != nil {
			t.Fatal(err)
		}
		if entry.data != "" {
			if _, err := io.WriteString(tarWriter, entry.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	local, _, err := writeLocalFailureBundle("bundle.tar.gz", remoteTar, FailureCaptureMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	headers := readTarGzHeaders(t, local)
	regular := headers["crabbox-artifacts/remote/logs/result.txt"]
	if regular == nil || regular.Typeflag != tar.TypeReg {
		t.Fatalf("safe regular entry missing: %#v", regular)
	}
	symlink := headers["crabbox-artifacts/remote/logs/result-link"]
	if symlink == nil || symlink.Typeflag != tar.TypeSymlink || symlink.Linkname != "result.txt" {
		t.Fatalf("safe symlink=%#v", symlink)
	}
	hardlink := headers["crabbox-artifacts/remote/logs/result-hard"]
	if hardlink == nil || hardlink.Typeflag != tar.TypeLink || hardlink.Linkname != "crabbox-artifacts/remote/logs/result.txt" {
		t.Fatalf("safe hardlink=%#v", hardlink)
	}
	for _, name := range []string{"symlink-out", "windows-link-out", "hardlink-out", "empty-link", "pipe"} {
		if header := headers["crabbox-artifacts/remote/logs/"+name]; header != nil {
			t.Fatalf("unsafe remote entry preserved: %#v", header)
		}
	}
}

func TestNativeWindowsFailureBundleUsesLocalStreams(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := filepath.Join(dir, "stderr.log")
	if err := os.WriteFile(stdoutPath, []byte("native stdout\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, []byte("native stderr\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	local, _, err := captureFailureBundle(context.Background(), SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, "C:\\crabbox\\repo", "cbx_win", "run_win", FailureCaptureMetadata{
		Provider:       "aws",
		LeaseID:        "cbx_win",
		RunID:          "run_win",
		CommandDisplay: "dotnet test --configuration Release",
		Workdir:        "C:\\crabbox\\repo",
		ExitCode:       9,
		Timing:         timingReport{Provider: "aws", LeaseID: "cbx_win", ExitCode: 9},
		Config:         Config{Provider: "aws", TargetOS: targetWindows, WindowsMode: windowsModeNormal},
		StdoutPath:     stdoutPath,
		StderrPath:     stderrPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	contents := readTarGzContents(t, local)
	if !bytes.Contains(contents["crabbox-artifacts/stdout.log"], []byte("native stdout")) {
		t.Fatalf("stdout missing: %#v", contents["crabbox-artifacts/stdout.log"])
	}
	if !bytes.Contains(contents["crabbox-artifacts/stderr.log"], []byte("native stderr")) {
		t.Fatalf("stderr missing: %#v", contents["crabbox-artifacts/stderr.log"])
	}
	var runMetadata struct {
		Command string `json:"command"`
		RunID   string `json:"runId"`
	}
	if err := json.Unmarshal(contents["crabbox-artifacts/crabbox-run.json"], &runMetadata); err != nil {
		t.Fatalf("decode native Windows crabbox-run.json: %v", err)
	}
	if runMetadata.Command != "dotnet test --configuration Release" || runMetadata.RunID != "run_win" {
		t.Fatalf("native Windows run metadata=%+v", runMetadata)
	}
	if _, ok := contents["crabbox-artifacts/remote/.crabbox/capture-manifest.txt"]; ok {
		t.Fatalf("native Windows bundle should be local-only: %#v", contents)
	}
}

func TestFailureBundleStreamsLargeStreamFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := filepath.Join(dir, "stderr.log")
	stdoutData := bytes.Repeat([]byte("stdout0123456789\n"), 128*1024)
	stderrData := []byte("stderr\n")
	if err := os.WriteFile(stdoutPath, stdoutData, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, stderrData, 0o666); err != nil {
		t.Fatal(err)
	}
	local, _, err := writeLocalFailureBundle("large-streams.tar.gz", "", FailureCaptureMetadata{
		Provider:   "aws",
		LeaseID:    "cbx_large",
		RunID:      "run_large",
		ExitCode:   1,
		Timing:     timingReport{Provider: "aws", LeaseID: "cbx_large", ExitCode: 1},
		Config:     Config{Provider: "aws", TargetOS: targetLinux},
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	contents := readTarGzContents(t, local)
	if !bytes.Equal(contents["crabbox-artifacts/stdout.log"], stdoutData) {
		t.Fatalf("stdout data mismatch: got=%d want=%d", len(contents["crabbox-artifacts/stdout.log"]), len(stdoutData))
	}
	if !bytes.Equal(contents["crabbox-artifacts/stderr.log"], stderrData) {
		t.Fatalf("stderr data mismatch: got=%q want=%q", contents["crabbox-artifacts/stderr.log"], stderrData)
	}
}

func TestCappedFailureBundleStreamBoundsImplicitCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := NewCappedFailureBundleStream(file)
	chunk := bytes.Repeat([]byte("x"), 1024*1024)
	for i := 0; i < 24; i++ {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > failureBundleStreamCaptureBytes+256 {
		t.Fatalf("implicit capture grew too large: %d", info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("failure-bundle stream truncated")) {
		t.Fatalf("missing truncation marker")
	}
}

func readTarGzContents(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	entries := make(map[string][]byte)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			data, err := io.ReadAll(tarReader)
			if err != nil {
				t.Fatal(err)
			}
			entries[header.Name] = data
		} else {
			entries[header.Name] = nil
		}
	}
	return entries
}

func readTarGzHeaders(t *testing.T, path string) map[string]*tar.Header {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	entries := make(map[string]*tar.Header)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		copy := *header
		entries[header.Name] = &copy
	}
}

func TestPrintKeepOnFailureSSHHint(t *testing.T) {
	var buf bytes.Buffer
	cfg := Config{Provider: "aws", IdleTimeout: 30 * time.Minute, TTL: 90 * time.Minute}
	server := Server{Labels: map[string]string{"slug": "blue-crab", "expires_at": "1777777777"}}
	target := SSHTarget{User: "crabbox", Host: "203.0.113.10", Port: "22", Key: "/tmp/key"}
	printKeepOnFailureSSHHint(&buf, cfg, "cbx_123", server, target)
	got := buf.String()
	for _, want := range []string{
		"keep-on-failure: kept lease=cbx_123",
		"ssh: crabbox ssh --provider aws --id blue-crab",
		"ssh-direct:",
		"stop: crabbox stop --provider aws blue-crab",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("hint missing %q in %q", want, got)
		}
	}
}

func TestStreamTailBufferBoundsLongPendingLine(t *testing.T) {
	tail := newStreamTailBuffer(2)
	chunk := strings.Repeat("a", failureTailLineBytes*3)
	if _, err := tail.Write([]byte(chunk)); err != nil {
		t.Fatal(err)
	}
	if len(tail.pending) > failureTailLineBytes+len("[truncated] ") {
		t.Fatalf("pending length=%d", len(tail.pending))
	}
	lines := tail.Lines()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "[truncated] ") {
		t.Fatalf("unexpected lines: %#v", lines)
	}
	if _, err := tail.Write([]byte("\n" + strings.Repeat("b", failureTailLineBytes*2) + "\n")); err != nil {
		t.Fatal(err)
	}
	lines = tail.Lines()
	if len(lines) != 2 {
		t.Fatalf("lines=%d want 2", len(lines))
	}
	for _, line := range lines {
		if len(line) > failureTailLineBytes+len("[truncated] ") {
			t.Fatalf("line length=%d", len(line))
		}
	}
}

func TestApplyCapacityMarketFlag(t *testing.T) {
	fs := newFlagSet("test", io.Discard)
	market := fs.String("market", "spot", "")
	if err := parseFlags(fs, []string{"--market", "on-demand"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	if err := applyCapacityMarketFlag(&cfg, fs, *market); err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity.Market != "on-demand" {
		t.Fatalf("market=%s want on-demand", cfg.Capacity.Market)
	}

	fs = newFlagSet("test", io.Discard)
	market = fs.String("market", "spot", "")
	if err := parseFlags(fs, []string{"--market", "reserved"}); err != nil {
		t.Fatal(err)
	}
	if err := applyCapacityMarketFlag(&cfg, fs, *market); err == nil {
		t.Fatal("expected invalid market failure")
	}
}

func TestAutoRouteClaimLeaseProvider(t *testing.T) {
	newFlags := func(t *testing.T, configured string, args ...string) (*flag.FlagSet, *string) {
		t.Helper()
		fs := newFlagSet("test", io.Discard)
		provider := fs.String("provider", configured, "")
		if err := parseFlags(fs, args); err != nil {
			t.Fatal(err)
		}
		return fs, provider
	}

	t.Run("exact id canonicalizes fixed AWS marker", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		const leaseID = "cbx_1257aaaa0001"
		if err := claimLeaseForRepoProvider(leaseID, "fixed", FixedAWSClaimProvider, "/repo", time.Minute, false); err != nil {
			t.Fatal(err)
		}
		fs, provider := newFlags(t, "hetzner")
		cfg := Config{Provider: *provider}
		if err := autoRouteClaimLeaseProvider(&cfg, fs, leaseID); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "aws" {
			t.Fatalf("provider=%q want aws", cfg.Provider)
		}
		if cfg.providerSelectionSource != providerSelectionLeaseContext {
			t.Fatalf("provider source=%q want %q", cfg.providerSelectionSource, providerSelectionLeaseContext)
		}
	})

	t.Run("exact id wins over slug collision", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		const leaseID = "cbx_1257aaaa0002"
		if err := claimLeaseForRepoProvider(leaseID, "exact-owner", "run-prepare-test", "/repo-a", time.Minute, false); err != nil {
			t.Fatal(err)
		}
		if err := claimLeaseForRepoProvider("cbx_1257bbbb0002", leaseID, "local-container", "/repo-b", time.Minute, false); err != nil {
			t.Fatal(err)
		}
		fs, provider := newFlags(t, "hetzner")
		cfg := Config{Provider: *provider}
		if err := autoRouteClaimLeaseProvider(&cfg, fs, leaseID); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "run-prepare-test" {
			t.Fatalf("provider=%q want exact claim provider", cfg.Provider)
		}
	})

	t.Run("unique slug routes provider", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		if err := claimLeaseForRepoProvider("cbx_1257aaaa0003", "Blue Lobster", "local-container", "/repo", time.Minute, false); err != nil {
			t.Fatal(err)
		}
		fs, provider := newFlags(t, "hetzner")
		cfg := Config{Provider: *provider}
		if err := autoRouteClaimLeaseProvider(&cfg, fs, "blue-lobster"); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "local-container" {
			t.Fatalf("provider=%q want local-container", cfg.Provider)
		}
		if cfg.providerSelectionSource != providerSelectionLeaseContext {
			t.Fatalf("provider source=%q want %q", cfg.providerSelectionSource, providerSelectionLeaseContext)
		}
	})

	t.Run("duplicate slug within one provider defers scope resolution", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		for _, leaseID := range []string{"cbx_1257aaaa0007", "cbx_1257bbbb0007"} {
			if err := claimLeaseForRepoProviderScope(leaseID, "Scoped Slug", "local-container", leaseID, "/repo", time.Minute, false); err != nil {
				t.Fatal(err)
			}
		}
		fs, provider := newFlags(t, "hetzner")
		cfg := Config{Provider: *provider}
		if err := autoRouteClaimLeaseProvider(&cfg, fs, "scoped-slug"); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "local-container" {
			t.Fatalf("provider=%q want local-container", cfg.Provider)
		}
	})

	t.Run("aliases of one provider do not create ambiguity", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		for i, providerName := range []string{"external", "exec-provider"} {
			leaseID := fmt.Sprintf("cbx_1257eeee000%d", i)
			if err := claimLeaseForRepoProviderScope(leaseID, "External Alias", providerName, leaseID, "/repo", time.Minute, false); err != nil {
				t.Fatal(err)
			}
		}
		fs, provider := newFlags(t, "hetzner")
		cfg := Config{Provider: *provider}
		if err := autoRouteClaimLeaseProvider(&cfg, fs, "external-alias"); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "external" {
			t.Fatalf("provider=%q want external", cfg.Provider)
		}
	})

	t.Run("ambiguous slug fails with guidance", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		if err := claimLeaseForRepoProvider("cbx_1257aaaa0004", "Shared Slug", "local-container", "/repo-a", time.Minute, false); err != nil {
			t.Fatal(err)
		}
		if err := claimLeaseForRepoProvider("cbx_1257bbbb0004", "Shared Slug", "run-prepare-test", "/repo-b", time.Minute, false); err != nil {
			t.Fatal(err)
		}
		fs, provider := newFlags(t, "hetzner")
		cfg := Config{Provider: *provider}
		err := autoRouteClaimLeaseProvider(&cfg, fs, "shared-slug")
		if err == nil || !strings.Contains(err.Error(), "canonical lease id") || !strings.Contains(err.Error(), "--provider") {
			t.Fatalf("err=%v, want ambiguity guidance", err)
		}
	})

	t.Run("explicit provider remains authoritative", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		const leaseID = "cbx_1257aaaa0005"
		if err := claimLeaseForRepoProvider(leaseID, "explicit", "local-container", "/repo", time.Minute, false); err != nil {
			t.Fatal(err)
		}
		fs, provider := newFlags(t, "hetzner", "--provider", "run-prepare-test")
		cfg := Config{Provider: *provider}
		if err := autoRouteClaimLeaseProvider(&cfg, fs, leaseID); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "run-prepare-test" {
			t.Fatalf("provider=%q want explicit provider", cfg.Provider)
		}
	})

	t.Run("legacy claim without provider preserves configured provider", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		const leaseID = "cbx_1257aaaa0006"
		if err := claimLeaseForRepo(leaseID, "legacy", "/repo", time.Minute, false); err != nil {
			t.Fatal(err)
		}
		fs, provider := newFlags(t, "hetzner")
		cfg := Config{Provider: *provider}
		if err := autoRouteClaimLeaseProvider(&cfg, fs, leaseID); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "hetzner" {
			t.Fatalf("provider=%q want configured provider", cfg.Provider)
		}
	})

	t.Run("provider-empty slug preserves configured provider", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		if err := claimLeaseForRepo("cbx_1257aaaa0008", "Legacy Slug", "/repo", time.Minute, false); err != nil {
			t.Fatal(err)
		}
		fs, provider := newFlags(t, "hetzner")
		cfg := Config{Provider: *provider}
		if err := autoRouteClaimLeaseProvider(&cfg, fs, "legacy-slug"); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "hetzner" {
			t.Fatalf("provider=%q want configured provider", cfg.Provider)
		}
	})

	t.Run("missing identifier preserves configured provider", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		fs, provider := newFlags(t, "hetzner")
		cfg := Config{Provider: *provider}
		if err := autoRouteClaimLeaseProvider(&cfg, fs, "missing-slug"); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "hetzner" {
			t.Fatalf("provider=%q want configured provider", cfg.Provider)
		}
	})

	t.Run("empty identifier preserves configured provider", func(t *testing.T) {
		fs, provider := newFlags(t, "hetzner")
		cfg := Config{Provider: *provider}
		if err := autoRouteClaimLeaseProvider(&cfg, fs, ""); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "hetzner" {
			t.Fatalf("provider=%q want configured provider", cfg.Provider)
		}
	})

	t.Run("malformed exact claim fails", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		const leaseID = "cbx_1257aaaa0009"
		path, err := leaseClaimPath(leaseID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		fs, provider := newFlags(t, "hetzner")
		cfg := Config{Provider: *provider}
		if err := autoRouteClaimLeaseProvider(&cfg, fs, leaseID); err == nil || !strings.Contains(err.Error(), "parse claim") {
			t.Fatalf("err=%v, want malformed exact claim failure", err)
		}
	})

	t.Run("malformed claim directory entry fails slug routing", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		path, err := leaseClaimPath("cbx_1257aaaa0010")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		fs, provider := newFlags(t, "hetzner")
		cfg := Config{Provider: *provider}
		if err := autoRouteClaimLeaseProvider(&cfg, fs, "some-slug"); err == nil || !strings.Contains(err.Error(), "parse claim") {
			t.Fatalf("err=%v, want malformed claim directory failure", err)
		}
	})
}

func TestApplyLeaseCreateFlagsForExistingAWSMacOSLeaseDefaultsOnDemand(t *testing.T) {
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaultConfig())
	if err := parseFlags(fs, []string{"--provider", "aws", "--target", "macos"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Coordinator = "https://broker.example.test"
	if err := applyLeaseCreateFlagsForLease(&cfg, fs, values, "cbx_123"); err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity.Market != "on-demand" {
		t.Fatalf("market=%s want on-demand", cfg.Capacity.Market)
	}
}

func TestApplyLeaseCreateFlagsForExistingAWSMacOSLeaseRejectsExplicitSpot(t *testing.T) {
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaultConfig())
	if err := parseFlags(fs, []string{"--provider", "aws", "--target", "macos", "--market", "spot"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Coordinator = "https://broker.example.test"
	err := applyLeaseCreateFlagsForLease(&cfg, fs, values, "cbx_123")
	if err == nil || !strings.Contains(err.Error(), "requires --market on-demand") {
		t.Fatalf("err=%v, want explicit spot rejection", err)
	}
}

func TestApplyServerTypeFlagOverridesUsesTargetAwareAWSDefaults(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "macos",
			args: []string{"--provider", "aws", "--target", "macos", "--class", "standard"},
			want: "mac2.metal",
		},
		{
			name: "windows",
			args: []string{"--provider", "aws", "--target", "windows", "--class", "standard"},
			want: "m7i.large",
		},
		{
			name: "windows wsl2",
			args: []string{"--provider", "aws", "--target", "windows", "--windows-mode", "wsl2", "--class", "standard"},
			want: "m8i.large",
		},
		{
			name: "windows mode only",
			args: []string{"--windows-mode", "wsl2"},
			want: "m8i.4xlarge",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Provider:    "aws",
				TargetOS:    targetWindows,
				WindowsMode: windowsModeNormal,
				Class:       "beast",
				ServerType:  "c7a.48xlarge",
				WorkRoot:    defaultWindowsWorkRoot,
			}
			fs := newFlagSet("test", io.Discard)
			provider := fs.String("provider", cfg.Provider, "")
			class := fs.String("class", cfg.Class, "")
			serverType := fs.String("type", "", "")
			targetFlags := registerTargetFlags(fs, cfg)
			if err := parseFlags(fs, tt.args); err != nil {
				t.Fatal(err)
			}
			cfg.Provider = *provider
			cfg.Class = *class
			if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
				t.Fatal(err)
			}
			applyServerTypeFlagOverrides(&cfg, fs, *serverType)
			if cfg.ServerType != tt.want {
				t.Fatalf("serverType=%q want %q", cfg.ServerType, tt.want)
			}
			if cfg.WindowsMode == windowsModeWSL2 && cfg.WorkRoot != defaultPOSIXWorkRoot {
				t.Fatalf("workRoot=%q want %q", cfg.WorkRoot, defaultPOSIXWorkRoot)
			}
			if cfg.ServerTypeExplicit {
				t.Fatal("ServerTypeExplicit=true, want false")
			}
		})
	}
}

func TestApplyTargetFlagOverridesRefreshesDefaultWorkRoot(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		args []string
		want string
	}{
		{
			name: "native windows to wsl2",
			cfg: Config{
				TargetOS:    targetWindows,
				WindowsMode: windowsModeNormal,
				WorkRoot:    defaultWindowsWorkRoot,
			},
			args: []string{"--windows-mode", "wsl2"},
			want: defaultPOSIXWorkRoot,
		},
		{
			name: "wsl2 to native windows",
			cfg: Config{
				TargetOS:    targetWindows,
				WindowsMode: windowsModeWSL2,
				WorkRoot:    defaultPOSIXWorkRoot,
			},
			args: []string{"--windows-mode", "normal"},
			want: defaultWindowsWorkRoot,
		},
		{
			name: "custom root is preserved",
			cfg: Config{
				TargetOS:    targetWindows,
				WindowsMode: windowsModeNormal,
				WorkRoot:    `/custom/root`,
			},
			args: []string{"--windows-mode", "wsl2"},
			want: `/custom/root`,
		},
		{
			name: "linux to macos",
			cfg: Config{
				Provider:    "aws",
				TargetOS:    targetLinux,
				WindowsMode: windowsModeNormal,
				SSHUser:     baseConfig().SSHUser,
				WorkRoot:    defaultPOSIXWorkRoot,
			},
			args: []string{"--target", "macos"},
			want: defaultMacOSWorkRoot,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFlagSet("test", io.Discard)
			targetFlags := registerTargetFlags(fs, tt.cfg)
			if err := parseFlags(fs, tt.args); err != nil {
				t.Fatal(err)
			}
			cfg := tt.cfg
			if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
				t.Fatal(err)
			}
			if cfg.WorkRoot != tt.want {
				t.Fatalf("workRoot=%q want %q", cfg.WorkRoot, tt.want)
			}
		})
	}
}

func TestApplyServerTypeFlagOverridesPreservesExplicitType(t *testing.T) {
	cfg := Config{
		Provider:    "aws",
		TargetOS:    targetLinux,
		WindowsMode: windowsModeNormal,
		Class:       "beast",
		ServerType:  "c7a.48xlarge",
	}
	fs := newFlagSet("test", io.Discard)
	provider := fs.String("provider", cfg.Provider, "")
	class := fs.String("class", cfg.Class, "")
	serverType := fs.String("type", "", "")
	targetFlags := registerTargetFlags(fs, cfg)
	if err := parseFlags(fs, []string{"--provider", "aws", "--target", "macos", "--class", "standard", "--type", "mac1.metal"}); err != nil {
		t.Fatal(err)
	}
	cfg.Provider = *provider
	cfg.Class = *class
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		t.Fatal(err)
	}
	applyServerTypeFlagOverrides(&cfg, fs, *serverType)
	if cfg.ServerType != "mac1.metal" {
		t.Fatalf("serverType=%q want mac1.metal", cfg.ServerType)
	}
	if !cfg.ServerTypeExplicit {
		t.Fatal("ServerTypeExplicit=false, want true")
	}
}

func TestCommandNeedsHydrationHint(t *testing.T) {
	if !commandNeedsHydrationHint([]string{"env NODE_OPTIONS=--max-old-space-size=4096 pnpm test"}, true) {
		t.Fatal("expected shell pnpm command to need hydration hint")
	}
	if !commandNeedsHydrationHint([]string{"pnpm", "test:docs"}, false) {
		t.Fatal("expected pnpm docs command to need hydration hint")
	}
	if !commandNeedsHydrationHint([]string{"node", "scripts/check.mjs"}, false) {
		t.Fatal("expected node script command to need hydration hint")
	}
	if commandNeedsHydrationHint([]string{"go", "test", "./..."}, false) {
		t.Fatal("go test should not need hydration hint")
	}
}

func TestShouldAutoHydrateActions(t *testing.T) {
	cfg := defaultConfig()
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	if !shouldAutoHydrateActions(cfg, false, false, FreshPRSpec{}, false) {
		t.Fatal("configured workflow should auto-hydrate normal runs")
	}
	if shouldAutoHydrateActions(cfg, true, false, FreshPRSpec{}, false) {
		t.Fatal("--no-hydrate should disable auto hydration")
	}
	if shouldAutoHydrateActions(cfg, false, true, FreshPRSpec{}, false) {
		t.Fatal("--no-sync should disable auto hydration")
	}
	if shouldAutoHydrateActions(cfg, false, false, FreshPRSpec{Owner: "example-org", Repo: "my-app", Number: 1}, false) {
		t.Fatal("--fresh-pr should disable auto hydration")
	}
	if shouldAutoHydrateActions(cfg, false, false, FreshPRSpec{}, true) {
		t.Fatal("--sync-only should disable auto hydration")
	}
	cfg.Actions.Workflow = ""
	if shouldAutoHydrateActions(cfg, false, false, FreshPRSpec{}, false) {
		t.Fatal("missing workflow should disable auto hydration")
	}
}

func TestCommandRuntimePreflightToolsFocusesEntrypoint(t *testing.T) {
	if got := strings.Join(commandRuntimePreflightTools([]string{"env CI=1 pnpm test"}, true), ","); got != "pnpm" {
		t.Fatalf("tools=%q want pnpm", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"env -u NODE_OPTIONS pnpm test"}, true), ","); got != "pnpm" {
		t.Fatalf("tools=%q want pnpm through env -u", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"env", "--unset=NODE_OPTIONS", "pnpm", "test"}, false), ","); got != "pnpm" {
		t.Fatalf("tools=%q want pnpm through env --unset", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"/usr/bin/env", "pnpm", "test"}, false), ","); got != "pnpm" {
		t.Fatalf("tools=%q want pnpm through /usr/bin/env", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"/opt/node/bin/pnpm", "test"}, false), ","); got != "/opt/node/bin/pnpm" {
		t.Fatalf("tools=%q want explicit pnpm path", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"./scripts/pnpm", "test"}, false), ","); got != "" {
		t.Fatalf("repo-relative wrapper should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"env PATH=/opt/node/bin:$PATH pnpm test"}, true), ","); got != "" {
		t.Fatalf("custom PATH command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"export PATH=/opt/node/bin:$PATH; pnpm test"}, true), ","); got != "" {
		t.Fatalf("export PATH setup should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"bash", "-lc", "source ~/.nvm/nvm.sh && pnpm test"}, false), ","); got != "" {
		t.Fatalf("bash setup wrapper should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"powershell -NoProfile -Command \"$env:Path = 'C:\\node;' + $env:Path; node -v\""}, true), ","); got != "" {
		t.Fatalf("PowerShell wrapper should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"pwsh.exe", "-NoProfile", "-Command", "$env:Path = 'C:\\node;' + $env:Path; pnpm test"}, false), ","); got != "" {
		t.Fatalf("pwsh wrapper should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"sudo apt-get update && sudo apt-get install -y nodejs npm && npm test"}, true), ","); got != "" {
		t.Fatalf("sudo runtime setup command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"corepack enable && pnpm install"}, true), ","); got != "" {
		t.Fatalf("corepack setup command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"npm install -g pnpm && pnpm test"}, true), ","); got != "" {
		t.Fatalf("npm global setup command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"cd web && pnpm test"}, true), ","); got != "pnpm" {
		t.Fatalf("shell cd prefix should still preflight pnpm, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"echo starting; pnpm install"}, true), ","); got != "pnpm" {
		t.Fatalf("shell echo prefix should still preflight pnpm, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{`echo "ok&&pnpm"`}, true), ","); got != "" {
		t.Fatalf("quoted shell separator should not expose pnpm preflight, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"echo 'ok; pnpm'"}, true), ","); got != "" {
		t.Fatalf("quoted semicolon should not expose pnpm preflight, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"node --version && pnpm test"}, true), ","); got != "node,pnpm" {
		t.Fatalf("multi-segment JS command should preflight node and pnpm, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"pnpm test && bash scripts/post.sh"}, true), ","); got != "pnpm" {
		t.Fatalf("later setup wrapper should not erase earlier pnpm preflight, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"pnpm test && curl -fsSL https://example.invalid/setup.sh | bash"}, true), ","); got != "pnpm" {
		t.Fatalf("later installer command should not erase earlier pnpm preflight, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"sudo", "-E", "pnpm", "test"}, false), ","); got != "pnpm" {
		t.Fatalf("sudo JS command should preflight pnpm, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"sudo", "env", "CI=1", "pnpm", "test"}, false), ","); got != "pnpm" {
		t.Fatalf("sudo env JS command should preflight pnpm, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"sudo", "CI=1", "pnpm", "test"}, false), ","); got != "pnpm" {
		t.Fatalf("sudo assignment JS command should preflight pnpm, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"apt-get update && apt-get install -y nodejs npm && npm test"}, true), ","); got != "" {
		t.Fatalf("runtime setup command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"pnpm --version || npm --version"}, true), ","); got != "" {
		t.Fatalf("shell fallback command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"pnpm --version||npm --version"}, true), ","); got != "" {
		t.Fatalf("compact shell fallback command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"pnpm", "--version", "||", "npm", "--version"}, false), ","); got != "" {
		t.Fatalf("argv fallback command should not be preflight-blocked, got %q", got)
	}
}

func TestRunEnvProvidesPathHandlesWindowsCasing(t *testing.T) {
	if !runEnvProvidesPath(map[string]string{"PATH": "/opt/node/bin"}, SSHTarget{TargetOS: targetLinux}) {
		t.Fatal("POSIX PATH should skip runtime preflight")
	}
	if runEnvProvidesPath(map[string]string{"Path": "/opt/node/bin"}, SSHTarget{TargetOS: targetLinux}) {
		t.Fatal("POSIX Path should not skip runtime preflight")
	}
	if !runEnvProvidesPath(map[string]string{"Path": `C:\node`}, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}) {
		t.Fatal("native Windows Path should skip runtime preflight")
	}
}

func TestRemoteMissingToolsCommandUsesLoginShell(t *testing.T) {
	got := remoteMissingToolsCommand([]string{"pnpm"})
	if !strings.HasPrefix(got, "bash -lc ") {
		t.Fatalf("command=%q want bash -lc wrapper", got)
	}
	if !strings.Contains(got, "command -v") {
		t.Fatalf("command=%q want command -v probe", got)
	}
	if !strings.Contains(got, missingRemoteToolPrefix) {
		t.Fatalf("command=%q want missing tool sentinel", got)
	}
}

func TestParseMissingRemoteToolsOutputIgnoresShellNoise(t *testing.T) {
	got := strings.Join(parseMissingRemoteToolsOutput("Welcome\n"+missingRemoteToolPrefix+"pnpm\nwarning\n"+missingRemoteToolPrefix+"pnpm\n"), ",")
	if got != "pnpm" {
		t.Fatalf("missing=%q want pnpm", got)
	}
}

func TestRawJSRuntimeMissingErrorMessage(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	err := rawJSRuntimeMissingError(cfg, []string{"pnpm"}, []string{"pnpm", "test:docs"}, false, "crabbox actions hydrate --id cbx_123 --provider aws")
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("error=%v, want exit 5", err)
	}
	for _, want := range []string{
		"remote raw workspace missing JS runtime tool(s): pnpm",
		"command starts with \"pnpm\"",
		"hydrate first: crabbox actions hydrate --id cbx_123 --provider aws",
		"include Node/Corepack/package-manager setup",
		"provider/image with the JS toolchain",
	} {
		if !strings.Contains(exitErr.Message, want) {
			t.Fatalf("message missing %q in %q", want, exitErr.Message)
		}
	}
}

func TestRawJSRuntimeHydrateSuggestionAvoidsReleasedLease(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	target := SSHTarget{TargetOS: targetLinux}
	got := rawJSRuntimeHydrateSuggestion(cfg, target, "cbx_released", true, false, false)
	if strings.Contains(got, "cbx_released") || !strings.Contains(got, "--keep") {
		t.Fatalf("suggestion=%q should not target released lease", got)
	}
	got = rawJSRuntimeHydrateSuggestion(cfg, target, "cbx_kept", true, true, false)
	if !strings.Contains(got, "cbx_kept") {
		t.Fatalf("suggestion=%q should target kept lease", got)
	}
}

func TestPrintCommandNotFoundHint(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	var out bytes.Buffer
	printCommandNotFoundHint(&out, cfg, SSHTarget{TargetOS: targetLinux}, "cbx_123", []string{"pnpm", "test"}, false, 127, false, "crabbox actions hydrate --id cbx_123 --provider aws")
	got := out.String()
	for _, want := range []string{"exit 127", "pnpm", "crabbox actions hydrate --id cbx_123"} {
		if !strings.Contains(got, want) {
			t.Fatalf("hint missing %q in %q", want, got)
		}
	}
}

func TestPrintCommandNotFoundHintAvoidsReleasedLease(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	var out bytes.Buffer
	suggestion := rawJSRuntimeHydrateSuggestion(cfg, SSHTarget{TargetOS: targetLinux}, "cbx_released", true, false, false)
	printCommandNotFoundHint(&out, cfg, SSHTarget{TargetOS: targetLinux}, "cbx_released", []string{"pnpm", "test"}, false, 127, false, suggestion)
	got := out.String()
	if strings.Contains(got, "cbx_released") || !strings.Contains(got, "--keep") {
		t.Fatalf("hint should avoid released lease and suggest --keep, got %q", got)
	}
}

func TestRecordRunFailureCapturesShadowedReturnErrors(t *testing.T) {
	var recorded error
	func() {
		if err := errors.New("sync failed"); err != nil {
			_ = recordRunFailure(&recorded, err)
			return
		}
	}()
	if recorded == nil || recorded.Error() != "sync failed" {
		t.Fatalf("recorded=%v", recorded)
	}
	_ = recordRunFailure(&recorded, nil)
	if recorded == nil || recorded.Error() != "sync failed" {
		t.Fatalf("nil failure should not clear recorded error, got %v", recorded)
	}
}

func TestLocalContainerDockerSocketSyncUsesResolvedLabels(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "local-container"
	server := Server{
		Provider: "local-container",
		Labels:   map[string]string{"docker_socket": "1"},
	}
	if !localContainerDockerSocketSync(cfg, server) {
		t.Fatal("socket-enabled resolved lease should sync without preserving mtimes")
	}
	server.Labels["docker_socket"] = "0"
	cfg.LocalContainer.DockerSocket = true
	if !localContainerDockerSocketSync(cfg, server) {
		t.Fatal("socket-enabled config should sync without preserving mtimes")
	}
	cfg.Provider = "aws"
	server.Provider = "aws"
	if localContainerDockerSocketSync(cfg, server) {
		t.Fatal("non-local-container provider should preserve normal rsync defaults")
	}
}

func TestApplyResolvedServerConfigRestoresLocalContainerSocketConfig(t *testing.T) {
	cfg := defaultConfig()
	server := Server{
		Provider: "local-container",
		Labels: map[string]string{
			"docker_socket": "1",
			"work_root":     "/tmp/crabbox-local-container-work",
		},
	}
	applyResolvedServerConfig(&cfg, server)
	if cfg.Provider != "local-container" || !cfg.LocalContainer.DockerSocket {
		t.Fatalf("local-container socket labels not restored: provider=%s local=%#v", cfg.Provider, cfg.LocalContainer)
	}
	if cfg.WorkRoot != server.Labels["work_root"] || cfg.LocalContainer.WorkRoot != server.Labels["work_root"] {
		t.Fatalf("work roots not restored: workRoot=%q local=%q", cfg.WorkRoot, cfg.LocalContainer.WorkRoot)
	}
	if !localContainerDockerSocketConfig(cfg) {
		t.Fatal("restored socket config should use no-times sync")
	}
}

func TestApplyResolvedServerConfigRestoresTargetAndWorkRoot(t *testing.T) {
	tests := []struct {
		name        string
		targetOS    string
		windowsMode string
		workRoot    string
	}{
		{name: "custom Windows root", targetOS: targetWindows, windowsMode: windowsModeWSL2, workRoot: `D:\crabbox`},
		{name: "POSIX default on macOS", targetOS: targetMacOS, windowsMode: windowsModeNormal, workRoot: defaultPOSIXWorkRoot},
		{name: "macOS default on Linux", targetOS: targetLinux, windowsMode: windowsModeNormal, workRoot: defaultMacOSWorkRoot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			server := Server{Labels: map[string]string{
				"target":       tt.targetOS,
				"windows_mode": tt.windowsMode,
				"work_root":    tt.workRoot,
			}}

			applyResolvedServerConfig(&cfg, server)

			if cfg.TargetOS != tt.targetOS || cfg.WindowsMode != tt.windowsMode || cfg.WorkRoot != tt.workRoot {
				t.Fatalf("resolved config=%#v", cfg)
			}
		})
	}
}

func TestApplyResolvedLeaseConfigPreservesServerPlatformDefaultWorkRoot(t *testing.T) {
	cfg := defaultConfig()
	target := SSHTarget{TargetOS: targetLinux, User: cfg.SSHUser}
	server := Server{Labels: map[string]string{
		"target":    targetMacOS,
		"work_root": defaultPOSIXWorkRoot,
	}}

	applyResolvedLeaseConfig(&cfg, server, &target)

	if cfg.TargetOS != targetMacOS || cfg.WorkRoot != defaultPOSIXWorkRoot {
		t.Fatalf("resolved config=%#v", cfg)
	}
	if target.TargetOS != targetMacOS {
		t.Fatalf("resolved target=%#v", target)
	}
}

func TestApplyResolvedLeaseConfigPrefersStoredLabelsAndUpdatesTarget(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	target := SSHTarget{TargetOS: targetLinux, User: cfg.SSHUser}
	server := Server{Labels: map[string]string{
		"target":       targetWindows,
		"windows_mode": windowsModeWSL2,
		"work_root":    "/work/crabbox",
	}}

	applyResolvedLeaseConfig(&cfg, server, &target)

	if cfg.TargetOS != targetWindows || cfg.WindowsMode != windowsModeWSL2 || cfg.WorkRoot != "/work/crabbox" || cfg.SSHUser != "Administrator" {
		t.Fatalf("resolved config=%#v", cfg)
	}
	if target.TargetOS != targetWindows || target.WindowsMode != windowsModeWSL2 || target.User != "Administrator" {
		t.Fatalf("resolved target=%#v", target)
	}
}

func TestApplyResolvedLeaseConfigPreservesProviderTargetUser(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	target := SSHTarget{TargetOS: targetLinux, User: "image-admin"}
	server := Server{Labels: map[string]string{
		"target":       targetWindows,
		"windows_mode": windowsModeWSL2,
	}}

	applyResolvedLeaseConfig(&cfg, server, &target)

	if target.User != "image-admin" {
		t.Fatalf("resolved target user=%q, want provider user", target.User)
	}
}
