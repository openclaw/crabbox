package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHerdrPluginContextCWD(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "current focused pane",
			raw:  `{"focused_pane_cwd":"/repo/feature","workspace_cwd":"/repo"}`,
			want: "/repo/feature",
		},
		{
			name: "current workspace fallback",
			raw:  `{"workspace_cwd":"/repo"}`,
			want: "/repo",
		},
		{
			name: "json escaped path",
			raw:  `{"focused_pane_cwd":"/repo/a \"quoted\" dir"}`,
			want: `/repo/a "quoted" dir`,
		},
		{
			name: "boundary whitespace",
			raw:  `{"focused_pane_cwd":" /repo/with boundary whitespace "}`,
			want: ` /repo/with boundary whitespace `,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := herdrPluginContextCWD(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("cwd=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestHerdrPluginContextCWDRejectsMissingOrInvalidContext(t *testing.T) {
	for _, raw := range []string{"", "{", `{}`, `{"pane":{"cwd":"/repo/unsupported"}}`} {
		if cwd, err := herdrPluginContextCWD(raw); err == nil {
			t.Fatalf("raw=%q cwd=%q, want error", raw, cwd)
		}
	}
}

func TestHerdrPluginContextCWDCommand(t *testing.T) {
	t.Setenv(herdrPluginContextEnv, `{"focused_pane_cwd":"/repo/worktree"}`)
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}, Stdin: strings.NewReader("")}
	if err := app.herdrPlugin(context.Background(), []string{"context-cwd"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "/repo/worktree\n" {
		t.Fatalf("stdout=%q", got)
	}
}

func TestHerdrPluginPickRetriesAndSelects(t *testing.T) {
	var output bytes.Buffer
	got, err := herdrPluginPick(strings.NewReader("nope\n3\n2\n"), &output, "lease", []string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "two" {
		t.Fatalf("selected=%q, want two", got)
	}
	if !strings.Contains(output.String(), "Enter a number from 1 to 2") {
		t.Fatalf("missing retry guidance: %s", output.String())
	}
}

func TestHerdrPluginPickCanCancel(t *testing.T) {
	_, err := herdrPluginPick(strings.NewReader("q\n"), &bytes.Buffer{}, "job", []string{"test"})
	if !errors.Is(err, errHerdrPluginSelectionCancelled) {
		t.Fatalf("err=%v, want selection cancelled", err)
	}
}

func TestHerdrPluginLeaseID(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{
			line: "server-1 name ready standard 192.0.2.1 lease=cbx_123 slug=blue-lobster keep=true target=linux",
			want: "cbx_123",
		},
		{
			line: "server-1 name ready standard 192.0.2.1 lease=- slug=blue-lobster keep=true target=linux",
			want: "blue-lobster",
		},
		{line: "native-provider-id ready", want: "native-provider-id"},
		{line: "", want: ""},
	}
	for _, tt := range tests {
		if got := herdrPluginLeaseID(tt.line); got != tt.want {
			t.Fatalf("line=%q id=%q, want %q", tt.line, got, tt.want)
		}
	}
}

func TestHerdrPluginRefreshDelay(t *testing.T) {
	if got, err := herdrPluginRefreshDelay(""); err != nil || got != 3*time.Second {
		t.Fatalf("default delay=%s err=%v", got, err)
	}
	if got, err := herdrPluginRefreshDelay("5s"); err != nil || got != 5*time.Second {
		t.Fatalf("configured delay=%s err=%v", got, err)
	}
	for _, value := range []string{"invalid", "500ms"} {
		if _, err := herdrPluginRefreshDelay(value); err == nil {
			t.Fatalf("delay %q unexpectedly accepted", value)
		}
	}
}

func TestNonBlankLines(t *testing.T) {
	got := nonBlankLines(" one \n\n two\n")
	if strings.Join(got, ",") != "one,two" {
		t.Fatalf("lines=%q", got)
	}
}

func TestHerdrPluginInWorkspace(t *testing.T) {
	dir := prepareHerdrPluginTestWorkspace(t)
	called := false
	app := App{}
	err := app.herdrPluginInWorkspace(context.Background(), []string{"one"}, func(_ context.Context, args []string) error {
		called = true
		if cwd, _ := os.Getwd(); cwd != dir {
			t.Fatalf("cwd=%q, want %q", cwd, dir)
		}
		if strings.Join(args, ",") != "one" {
			t.Fatalf("args=%q", args)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("workspace callback was not called")
	}
}

func TestHerdrPluginDispatchesSafeHelpPaths(t *testing.T) {
	prepareHerdrPluginTestWorkspace(t)
	for _, command := range []string{"doctor", "prewarm", "warmup"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("")}
			err := app.herdrPlugin(context.Background(), []string{command, "--help"})
			var exitErr ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 0 {
				t.Fatalf("%s help err=%v stderr=%s", command, err, stderr.String())
			}
		})
	}

	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.herdrPlugin(context.Background(), nil); err == nil {
		t.Fatal("missing command unexpectedly accepted")
	}
	if err := app.herdrPlugin(context.Background(), []string{"unknown"}); err == nil {
		t.Fatal("unknown command unexpectedly accepted")
	}
}

func TestHerdrPluginBoxesRendersProviderInventoryOnce(t *testing.T) {
	dir := prepareHerdrPluginTestWorkspace(t)
	installHerdrPluginTestBackend(t)
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("")}
	if err := app.herdrPlugin(context.Background(), []string{"boxes", "--once"}); err != nil {
		t.Fatalf("boxes failed: %v\nstderr=%s", err, stderr.String())
	}
	for _, want := range []string{"Crabbox boxes", "workspace: " + dir, "lease=cbx_plugin123"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("boxes output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestHerdrPluginConnectCanCancelBeforeSSH(t *testing.T) {
	prepareHerdrPluginTestWorkspace(t)
	installHerdrPluginTestBackend(t)
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("q\n")}
	if err := app.herdrPlugin(context.Background(), []string{"connect"}); err != nil {
		t.Fatalf("cancel connect failed: %v\nstderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Select lease") {
		t.Fatalf("missing lease picker:\n%s", stdout.String())
	}
}

func TestHerdrPluginJobCanCancelBeforeProvisioning(t *testing.T) {
	dir := prepareHerdrPluginTestWorkspace(t)
	configPath := filepath.Join(dir, ".crabbox.yaml")
	if err := os.WriteFile(configPath, []byte("jobs:\n  smoke:\n    command: go test ./...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("q\n")}
	if err := app.herdrPlugin(context.Background(), []string{"job"}); err != nil {
		t.Fatalf("cancel job failed: %v\nstderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1) smoke") {
		t.Fatalf("missing job picker:\n%s", stdout.String())
	}
}

func TestHerdrPluginJobPreservesNameWithSpaces(t *testing.T) {
	for _, name := range []string{"smoke test", "-smoke"} {
		t.Run(name, func(t *testing.T) {
			dir := prepareHerdrPluginTestWorkspace(t)
			configPath := filepath.Join(dir, ".crabbox.yaml")
			if err := os.WriteFile(configPath, []byte(fmt.Sprintf("jobs:\n  %s: {}\n", name)), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("1\n")}
			err := app.herdrPlugin(context.Background(), []string{"job"})
			want := fmt.Sprintf(`job %q requires command or syncOnly`, name)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err=%v, want selected full job name\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
			}
		})
	}
}

func TestHerdrPluginCaptureLines(t *testing.T) {
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	lines, err := app.herdrPluginCaptureLines(context.Background(), "items", func(app App) error {
		fmt.Fprintln(app.Stdout, " one")
		fmt.Fprintln(app.Stdout)
		fmt.Fprintln(app.Stdout, "two ")
		return nil
	})
	if err != nil || strings.Join(lines, ",") != "one,two" {
		t.Fatalf("lines=%q err=%v", lines, err)
	}
	if _, err := app.herdrPluginCaptureLines(context.Background(), "items", func(App) error { return nil }); err == nil {
		t.Fatal("empty capture unexpectedly accepted")
	}
}

func prepareHerdrPluginTestWorkspace(t *testing.T) string {
	t.Helper()
	clearConfigEnv(t)
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	})
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	contextJSON, err := json.Marshal(map[string]string{"focused_pane_cwd": dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(herdrPluginContextEnv, string(contextJSON))
	return dir
}

type herdrPluginTestBackend struct {
	testSSHBackend
}

func (b *herdrPluginTestBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return []LeaseView{{
		CloudID: "server-plugin-123",
		Name:    "herdr-box",
		Status:  "ready",
		Labels: map[string]string{
			"lease":  "cbx_plugin123",
			"slug":   "herdr-box",
			"target": "linux",
		},
	}}, nil
}

func installHerdrPluginTestBackend(t *testing.T) {
	t.Helper()
	backend := &herdrPluginTestBackend{testSSHBackend: testSSHBackend{spec: testAWSProvider{}.Spec()}}
	testAWSBackendOverride = backend
	t.Cleanup(func() { testAWSBackendOverride = nil })
	t.Setenv("CRABBOX_PROVIDER", "aws")
}
