package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGitSeedFailurePhaseAndPreservation(t *testing.T) {
	f := newGitCoherenceFixture(t)
	for _, phase := range []string{"clone", "checkout", "verify"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			workdir := filepath.Join(root, "work")
			if err := os.Mkdir(workdir, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(workdir, "preserve.txt")
			mustWriteTestFile(t, marker, "existing workspace\n")
			plan := f.plan(t, f.b)
			switch phase {
			case "clone":
				plan.RemoteURL = filepath.Join(root, "missing-private-origin")
			case "checkout":
				plan.Target = strings.Repeat("f", 40)
			case "verify":
				plan.Tree = strings.Repeat("f", 40)
			}
			var out []byte
			var err error
			if runtime.GOOS == "windows" {
				out, err = runDecodedWindowsPowerShell(t, windowsGitSeed(workdir, plan))
			} else {
				out, err = exec.Command("bash", "-lc", remoteGitSeed(workdir, plan)).CombinedOutput()
			}
			if err == nil || !strings.Contains(string(out), "crabbox-git-seed phase="+phase) {
				t.Fatalf("missing %s failure diagnostic: err=%v output=%q", phase, err, out)
			}
			var warning bytes.Buffer
			warnRemoteGitSeedFailure(&warning, string(out), err)
			if !strings.Contains(warning.String(), "phase="+phase) || strings.Contains(warning.String(), root) || strings.Contains(warning.String(), plan.RemoteURL) {
				t.Fatalf("unsafe or incorrect warning: %q", warning.String())
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "existing workspace\n" {
				t.Fatalf("failed seed changed existing workspace: data=%q err=%v", data, err)
			}
			leftovers, err := filepath.Glob(filepath.Join(root, ".seed*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("seed residue=%v err=%v", leftovers, err)
			}
		})
	}
}

func TestGitSeedDiagnosticLabelsAndPrivacy(t *testing.T) {
	fixtureURL := &url.URL{Scheme: "https", Host: "example.invalid", Path: "/private", User: url.UserPassword("fixture-user", "do-not-forward")}
	for _, tc := range []struct{ name, output, want string }{
		{"auth", "Authentication failed for " + fixtureURL.String(), "authentication/access"},
		{"prompt", "could not read Username for 'https://secret': terminal prompts disabled", "authentication/access"},
		{"dns", "Could not resolve host: private.example", "dns"},
		{"connection", "Failed to connect to private.example port 443", "connectivity"},
		{"tls", "SSL certificate problem: private certificate", "tls"},
		{"repo", "fatal: '/private/secret' does not appear to be a git repository", "repository/ref"},
		{"ref", "fatal: Remote branch secret not found in upstream origin", "repository/ref"},
		{"unknown", "private helper says secret\x1b[2J\runknown", "unknown"},
		{"localized", "fatal: dépôt secret introuvable", "unknown"},
		{"forged field", "crabbox-git-seed phase=private-secret\nreason=private-secret", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			warnRemoteGitSeedFailure(&out, "crabbox-git-seed phase=clone\n"+tc.output, errors.New("private transport error"))
			want := "warning: remote git seed failed: phase=clone reason=" + tc.want + " exit=unknown; continuing with file sync; Git metadata was not seeded or verified\n"
			if out.String() != want {
				t.Fatalf("warning=%q want=%q", out.String(), want)
			}
		})
	}
	for _, tc := range []struct{ output, want string }{
		{"crabbox-git-seed phase=prerequisite\nprivate command lookup", "reason=missing-git"},
		{"crabbox-git-seed phase=clone\nSSL certificate problem\ncrabbox-git-seed phase=verify\n", "reason=verification"},
	} {
		var out bytes.Buffer
		warnRemoteGitSeedFailure(&out, tc.output, context.Canceled)
		if !strings.Contains(out.String(), tc.want) || !strings.Contains(out.String(), "exit=cancelled") {
			t.Fatalf("warning=%q", out.String())
		}
	}
}

func TestGitSeedCaptureLimitPreservesExecutionAndRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell SSH fixture")
	}
	for _, tc := range []struct {
		name, body string
		wantCalls  int
		wantError  bool
	}{
		{"remote failure", "exit 128", 1, true},
		{"transport failure", "exit 255", 2, true},
		{"success", "exit 0", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			drained := filepath.Join(dir, "drained")
			script := "#!/bin/sh\nprintf 'call\\n' >> " + shellQuote(calls) + "\nprintf 'crabbox-git-seed phase=clone\\n'\nhead -c 1048576 /dev/zero\nprintf secret >&2\nprintf complete > " + shellQuote(drained) + "\n" + tc.body + "\n"
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := runIdempotentSSHCombinedOutputLimit(ctx, SSHTarget{Host: "fixture.invalid", Port: "22", FallbackPorts: []string{}}, "true", 0, gitSeedDiagnosticLimit)
			if (err != nil) != tc.wantError || out != "" {
				t.Fatalf("out length=%d err=%v", len(out), err)
			}
			data, readErr := os.ReadFile(calls)
			if readErr != nil || strings.Count(string(data), "call\n") != tc.wantCalls {
				t.Fatalf("calls=%q err=%v", data, readErr)
			}
			if data, err := os.ReadFile(drained); err != nil || string(data) != "complete" {
				t.Fatalf("producer did not drain: %q %v", data, err)
			}
			if tc.wantError {
				var warning bytes.Buffer
				warnRemoteGitSeedFailure(&warning, out, err)
				if !strings.Contains(warning.String(), "phase=unknown reason=unknown") || strings.Contains(warning.String(), "secret") {
					t.Fatalf("warning=%q", warning.String())
				}
			}
		})
	}
	capture := synchronizedBuffer{limit: gitSeedDiagnosticLimit}
	if n, err := io.Copy(&capture, strings.NewReader(strings.Repeat("x", 1<<20))); err != nil || n != 1<<20 || capture.buf.Len() > gitSeedDiagnosticLimit || !capture.truncated || capture.String() != "" {
		t.Fatalf("capture n=%d err=%v retained=%d truncated=%v", n, err, capture.buf.Len(), capture.truncated)
	}
}

func TestGitOriginAttemptRetainsBoundedTruncatedDiagnosticsWithoutFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell SSH fixture")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'fatal: Authentication failed\\n'\nhead -c 1048576 /dev/zero\nexit 78\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := runIdempotentSSHGitOriginAttempt(t.Context(), SSHTarget{Host: "fixture.invalid", Port: "22", FallbackPorts: []string{}}, "true", 0)
	if err == nil || len(out) != gitSeedDiagnosticLimit {
		t.Fatalf("diagnostic bytes=%d err=%v", len(out), err)
	}
	var truncated *gitOriginDiagnosticsTruncatedError
	if !errors.As(err, &truncated) {
		t.Fatalf("truncated diagnostic error type lost: %T", err)
	}
	if reason, fallback := gitOriginRuntimeFallbackResult("https://example.test/repo.git", out, err); fallback || reason != "" {
		t.Fatalf("truncated output authorized fallback=%t reason=%q", fallback, reason)
	}
	var warning bytes.Buffer
	reportRemoteGitSeedFailure(&warning, out, err, "aborting before file sync")
	want := "warning: remote git seed failed: phase=unknown reason=unknown exit=78; aborting before file sync; Git metadata was not seeded or verified\n"
	if warning.String() != want {
		t.Fatalf("warning=%q want=%q", warning.String(), want)
	}
}

func TestActionsSeedDiagnosticFailsSafely(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process fixture for Actions hydration")
	}
	f := newGitCoherenceFixture(t)
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	ssh := `#!/bin/sh
last=
for arg do last="$arg"; done
case "$last" in
  *'git clone --quiet'*)
    printf 'seed\n' >> "$SEED_TEST_CALLS"
    printf 'crabbox-git-seed phase=clone\n'
    printf 'fatal: repository secret-private-endpoint not found\n' >&2
    exit 128 ;;
  *'update-ref --no-deref HEAD'*) printf 'finalize\n' >> "$SEED_TEST_CALLS" ;;
esac
cat >/dev/null
`
	for name, script := range map[string]string{
		"ssh":   ssh,
		"rsync": "#!/bin/sh\nprintf 'rsync\\n' >> \"$SEED_TEST_CALLS\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SEED_TEST_CALLS", calls)
	cfg := baseConfig()
	cfg.Sync.BaseRef = "main"
	repo := Repo{Root: f.source, Name: "repo", RemoteURL: f.origin, Head: f.b, BaseRef: "main"}
	var stderr bytes.Buffer
	app := App{Stdout: io.Discard, Stderr: &stderr}
	_, err := app.syncLocalActionsWorkspace(context.Background(), cfg, repo, SSHTarget{User: "builder", Host: "fixture.invalid", Port: "22", FallbackPorts: []string{}, TargetOS: targetLinux}, "/work/repo", false)
	if err == nil || !strings.Contains(err.Error(), "remote git seed failed") {
		t.Fatalf("sync error=%v\n%s", err, stderr.String())
	}
	warning := "warning: remote git seed failed: phase=clone reason=repository/ref exit=128; aborting before file sync; Git metadata was not seeded or verified"
	if strings.Count(stderr.String(), warning) != 1 || strings.Contains(stderr.String(), "secret-private-endpoint") {
		t.Fatalf("unsafe or missing diagnostic: %q", stderr.String())
	}
	data, err := os.ReadFile(calls)
	if err != nil || string(data) != "seed\n" {
		t.Fatalf("production transition order=%q err=%v", data, err)
	}
}
