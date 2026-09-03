package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type sshScriptTestProvider struct {
	runEnvProfileTestProvider
	spec    ProviderSpec
	backend Backend
}

func (p *sshScriptTestProvider) Name() string                               { return p.spec.Name }
func (p *sshScriptTestProvider) Spec() ProviderSpec                         { return p.spec }
func (p *sshScriptTestProvider) Configure(Config, Runtime) (Backend, error) { return p.backend, nil }

type sshScriptTestBackend struct {
	runEnvProfileTestBackend
	lease                             LeaseTarget
	activityPath                      string
	activityErr                       error
	requests                          []RunRequest
	warmups, acquired, starts, joined int
}

func (b *sshScriptTestBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	b.acquired++
	return b.lease, nil
}
func (b *sshScriptTestBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return b.lease, nil
}
func (b *sshScriptTestBackend) Warmup(context.Context, WarmupRequest) error { b.warmups++; return nil }
func (b *sshScriptTestBackend) Run(_ context.Context, req RunRequest) (RunResult, error) {
	b.requests = append(b.requests, req)
	return RunResult{}, nil
}
func (b *sshScriptTestBackend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, nil
}
func (b *sshScriptTestBackend) Stop(context.Context, StopRequest) error { return nil }
func (b *sshScriptTestBackend) BeginSSHRunActivity(ctx context.Context, lease LeaseTarget) (func(), error) {
	if claim, err := readLeaseClaim(lease.LeaseID); err != nil || claim.Provider != b.spec.Name {
		return nil, fmt.Errorf("activity started before admission: %v", err)
	}
	if b.starts == 0 {
		if _, err := os.Stat(filepath.Join(filepath.Dir(b.activityPath), "ssh.log")); !os.IsNotExist(err) {
			return nil, errors.New("SSH setup preceded activity")
		}
	}
	b.starts++
	if b.activityErr != nil {
		return nil, b.activityErr
	}
	if err := os.WriteFile(b.activityPath, nil, 0o600); err != nil {
		return nil, err
	}
	activityCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-activityCtx.Done()
		_ = os.Remove(b.activityPath)
	}()
	return func() { cancel(); <-done; b.joined++ }, nil
}

func setupSSHScriptRun(t *testing.T) (*sshScriptTestProvider, *sshScriptTestBackend, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH script fixture")
	}
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	t.Chdir(dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	t.Setenv("CRABBOX_WORK_ROOT", filepath.Join(dir, "remote workspace"))
	p := &sshScriptTestProvider{spec: ProviderSpec{
		Name: "ssh-script-test", Kind: ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureSSHScriptRun},
		Coordinator: CoordinatorNever,
	}}
	b := &sshScriptTestBackend{runEnvProfileTestBackend: runEnvProfileTestBackend{spec: p.spec}, activityPath: filepath.Join(dir, "activity")}
	b.lease = LeaseTarget{LeaseID: "cbx_123456789abc", Server: Server{Provider: p.Name()}, SSH: SSHTarget{
		User: "synthetic-script-credential", Host: "fixture.invalid", Port: "22", TargetOS: targetLinux, SSHConfigProxy: true, AuthSecret: true,
	}}
	p.backend = b
	RegisterProvider(p)
	t.Cleanup(func() { delete(providerRegistry, p.Name()) })
	t.Setenv("CRABBOX_SCRIPT_ACTIVITY", b.activityPath)
	t.Setenv("CRABBOX_SCRIPT_SSH_LOG", filepath.Join(dir, "ssh.log"))
	ssh := `#!/bin/sh
for arg do
  if [ "$arg" = -G ]; then exec /usr/bin/ssh "$@"; fi
done
cmd=""
for arg do cmd="$arg"; done
printf '%s\n' "$cmd" >> "$CRABBOX_SCRIPT_SSH_LOG"
decoded=$cmd
while :; do
  case "$decoded" in
    *'payload_b64="'*'"; decoded=; if command -v base64'*)
      payload=${decoded#*'payload_b64="'}
      payload=${payload%%'"; decoded=; if command -v base64'*}
      decoded=$(printf %s "$payload" | /usr/bin/base64 --decode 2>/dev/null) ||
        decoded=$(printf %s "$payload" | /usr/bin/base64 -D) ;;
    *) break ;;
  esac
done
case "$decoded" in
  *"/usr/local/bin/crabbox-ready"*) exit 0 ;;
esac
exec sh -c "$cmd"
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(ssh), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rsync"), []byte("#!/bin/sh\nexit 91\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return p, b, dir
}

func TestHybridSSHScriptRun(t *testing.T) {
	for _, tc := range []struct {
		name, prefix string
		code         int
	}{
		{name: "file-shebang", prefix: "#!/bin/sh\n"},
		{name: "stdin-bash", code: 23},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, dir := setupSSHScriptRun(t)
			source := tc.prefix + "set -eu\ntest -f " + shellQuote(b.activityPath) + "\n" + `test "$API_TOKEN" = 'synthetic-profile-value'
test "$1" = 'literal $arg; with spaces'
test "$2" = 'single quote '\'' argument'
printf 'script-out:%s\n' "$PWD"
printf 'script-err\n' >&2
` + fmt.Sprintf("exit %d\n", tc.code)
			profile := filepath.Join(dir, "env.profile")
			if err := os.WriteFile(profile, []byte("API_TOKEN=synthetic-profile-value\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--provider", p.Name(), "--no-sync", "--no-hydrate", "--keep", "--allow-env", "API_TOKEN", "--env-from-profile", profile}
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(source)}
			if tc.prefix == "" {
				args = append(args, "--script-stdin")
			} else {
				file := filepath.Join(dir, "source.sh")
				if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--script", file)
			}
			args = append(args, "--", "literal $arg; with spaces", "single quote ' argument")
			err := app.runCommand(t.Context(), args)
			if exitCodeForError(err, 0) != tc.code {
				t.Fatalf("run=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
			}
			if b.acquired != 1 || len(b.requests) != 0 || b.starts != 1 || b.joined != 1 {
				t.Fatalf("routes: %+v", b)
			}
			if !strings.Contains(stdout.String(), "script-out:"+filepath.Join(dir, "remote workspace")) || strings.Contains(stdout.String(), "script-err") || !strings.Contains(stderr.String(), "script-err") {
				t.Fatalf("stdout=%s\nstderr=%s", stdout.String(), stderr.String())
			}
			if _, err := os.Stat(b.activityPath); !os.IsNotExist(err) {
				t.Fatal("activity outlived run")
			}
			uploads, _ := filepath.Glob(filepath.Join(dir, "remote workspace", "*", "*", ".crabbox", "scripts", "*"))
			if len(uploads) != 1 {
				t.Fatalf("uploads=%v", uploads)
			}
			data, err := os.ReadFile(uploads[0])
			if err != nil || string(data) != source {
				t.Fatalf("script upload: %v", err)
			}
			if info, err := os.Stat(uploads[0]); err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("private upload: %v %v", info, err)
			}
			log, err := os.ReadFile(filepath.Join(dir, "ssh.log"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(log), b.lease.SSH.User) || strings.Contains(string(log), "synthetic-profile-value") || strings.Contains(string(log), "script-out:") {
				t.Fatal("private input leaked into SSH argv")
			}
			if err := app.runCommand(t.Context(), []string{"--provider", p.Name(), "--no-sync", "--", "ordinary", "arg"}); err != nil {
				t.Fatal(err)
			}
			if err := app.warmup(t.Context(), []string{"--provider", p.Name()}); err != nil {
				t.Fatal(err)
			}
			if len(b.requests) != 1 || !reflect.DeepEqual(b.requests[0].Command, []string{"ordinary", "arg"}) || b.warmups != 1 || b.starts != 1 {
				t.Fatal("ordinary command or warmup changed transport")
			}
		})
	}
}

func TestHybridSSHScriptRejectsInvalidRouteBeforeInputOrActivity(t *testing.T) {
	for _, mode := range []string{"missing-ssh-feature", "module", "missing-ssh-backend"} {
		t.Run(mode, func(t *testing.T) {
			p, b, _ := setupSSHScriptRun(t)
			switch mode {
			case "missing-ssh-feature":
				p.spec.Features = FeatureSet{FeatureSSHScriptRun}
			case "module":
				p.spec.Features = append(p.spec.Features, FeatureModuleRun)
			case "missing-ssh-backend":
				p.backend = &probeAdmissionBackend{p: &probeAdmissionProvider{spec: p.spec}, rt: Runtime{Stdout: io.Discard}}
			}
			b.spec = p.spec
			input := strings.NewReader("must remain unread")
			err := (App{Stdout: io.Discard, Stderr: io.Discard, Stdin: input}).runCommand(t.Context(), []string{"--provider", p.Name(), "--no-sync", "--script-stdin"})
			if exitCodeForError(err, 0) != 2 || !strings.Contains(err.Error(), "SSH") || input.Len() != len("must remain unread") || b.acquired != 0 || b.starts != 0 {
				t.Fatalf("error=%v unread=%d acquired=%d activity=%d", err, input.Len(), b.acquired, b.starts)
			}
		})
	}
}

func TestHybridSSHScriptPrewarmAdmissionUsesScriptRoute(t *testing.T) {
	p, b, _ := setupSSHScriptRun(t)
	p.spec.Kind = ProviderKindDelegatedRun
	args := []string{"--provider", p.Name(), "--no-sync", "--script-stdin", "--capture-stdout", "stdout.txt"}
	if err := admitPrewarmProbe(args); err != nil {
		t.Fatal(err)
	}
	p.spec.Features = FeatureSet{FeatureSSH, FeatureSSHScriptRun, FeatureModuleRun}
	if err := admitPrewarmProbe(args); exitCodeForError(err, 0) != 2 {
		t.Fatalf("invalid route accepted: %v", err)
	}
	if b.acquired != 0 || b.starts != 0 || len(b.requests) != 0 {
		t.Fatal("prewarm admission caused provider activity")
	}
}

func TestHybridSSHScriptCancellationAndSameLeaseReplay(t *testing.T) {
	p, b, dir := setupSSHScriptRun(t)
	started := filepath.Join(dir, "script-started")
	stop := filepath.Join(dir, "script-stop")
	source := "#!/bin/sh\nprintf '%s\\n' $$ > " + shellQuote(started) + "\nwhile [ ! -e " + shellQuote(stop) + " ]; do sleep 0.05; done\n"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- (App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(source)}).runCommand(ctx, []string{
			"--provider", p.Name(), "--no-sync", "--no-hydrate", "--keep", "--script-stdin",
		})
	}()
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			<-done
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-done:
			joined = true
			t.Fatalf("run ended before script started: %v\n%s", err, stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("script did not start")
		}
	}
	pidBytes, err := os.ReadFile(started)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(stop, nil, 0o600)
		deadline := time.Now().Add(time.Second)
		for child.Signal(syscall.Signal(0)) == nil && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if child.Signal(syscall.Signal(0)) == nil {
			_ = child.Kill()
		}
		_ = child.Release()
	})
	cancel()
	err = <-done
	joined = true
	if err == nil || b.joined != 1 {
		t.Fatalf("cancel=%v activity joins=%d", err, b.joined)
	}
	stdout.Reset()
	stderr.Reset()
	replayMarker := filepath.Join(dir, "replayed")
	replayCtx, stopReplay := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer stopReplay()
	err = (App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("touch " + shellQuote(replayMarker) + "\n")}).runCommand(replayCtx, []string{
		"--provider", p.Name(), "--id", b.lease.LeaseID, "--no-sync", "--no-hydrate", "--keep", "--script-stdin",
	})
	if err == nil || !strings.Contains(err.Error(), "workspace owner") || b.joined != 2 || len(b.requests) != 0 {
		t.Fatalf("ambiguous same-lease replay=%v joins=%d\nstdout=%s\nstderr=%s", err, b.joined, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(replayMarker); !os.IsNotExist(err) {
		t.Fatal("replayed while the earlier workspace witness was unsettled")
	}
}

func TestHybridSSHScriptActivityFailureStopsBeforeSetup(t *testing.T) {
	p, b, dir := setupSSHScriptRun(t)
	b.activityErr = errors.New("activity unavailable")
	err := (App{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader("exit 0\n")}).runCommand(t.Context(), []string{"--provider", p.Name(), "--no-sync", "--keep", "--script-stdin"})
	if err == nil || !strings.Contains(err.Error(), "activity unavailable") || b.starts != 1 || b.joined != 0 {
		t.Fatalf("error=%v starts=%d joined=%d", err, b.starts, b.joined)
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh.log")); !os.IsNotExist(err) {
		t.Fatal("setup ran after activity admission failed")
	}
}
