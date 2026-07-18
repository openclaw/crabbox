package cloudrunsandbox

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

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
