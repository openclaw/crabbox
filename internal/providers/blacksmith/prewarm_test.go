//go:build !windows

package blacksmith

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestBlacksmithPrewarmProbeAdmission(t *testing.T) {
	for _, provider := range []string{"blacksmith-testbox", "blacksmith"} {
		for _, dryRun := range []bool{false, true} {
			name := provider + "/normal"
			if dryRun {
				name = provider + "/dry_run"
			}
			t.Run(name, func(t *testing.T) {
				for _, probe := range []struct {
					name string
					args []string
				}{
					{name: "probe", args: []string{"--probe-command", "node -v"}},
					{name: "plain"},
					{name: "empty", args: []string{"--probe-command", ""}},
					{name: "whitespace", args: []string{"--probe-command", " \t\n"}},
				} {
					t.Run(probe.name, func(t *testing.T) {
						const leaseID = "tbx_prewarm123"
						const existingID = "tbx_existing123"
						repo, callsPath := blacksmithCallerFixture(t, "provider: blacksmith-testbox\nblacksmith:\n  org: example-org\n  workflow: testbox.yml\n  job: check\n  ref: main\n")
						if err := claimLeaseForRepoProvider(existingID, "existing-box", blacksmithTestboxProvider, repo, time.Minute, false); err != nil {
							t.Fatal(err)
						}
						before, err := readLeaseClaim(existingID)
						if err != nil {
							t.Fatal(err)
						}
						args := append([]string{"prewarm", "--provider", provider}, probe.args...)
						if dryRun {
							args = append(args, "--dry-run")
						}
						var stdout, stderr bytes.Buffer
						runErr := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), args)
						calls, err := os.ReadFile(callsPath)
						if err != nil && !os.IsNotExist(err) {
							t.Fatal(err)
						}
						claim, err := readLeaseClaim(leaseID)
						if err != nil {
							t.Fatal(err)
						}
						after, err := readLeaseClaim(existingID)
						if err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(before, after) {
							t.Error("prewarm changed an existing claim")
						}
						if probe.name == "probe" {
							var exitErr ExitError
							if !core.AsExitError(runErr, &exitErr) || exitErr.Code != 2 {
								t.Errorf("prewarm error=%v, want exit 2", runErr)
							}
							message := stderr.String()
							if runErr != nil {
								message += runErr.Error()
							}
							for _, want := range []string{"--probe-command", "--no-sync", "blacksmith-testbox", "not supported"} {
								if !strings.Contains(message, want) {
									t.Errorf("diagnostic missing %q: %q", want, message)
								}
							}
							if stdout.Len() != 0 {
								t.Errorf("probe was admitted before rejection:\n%s", &stdout)
							}
						} else {
							if runErr != nil {
								t.Fatalf("plain prewarm: %v\nstdout=%s stderr=%s", runErr, &stdout, &stderr)
							}
							if strings.Contains(stdout.String(), "crabbox run") {
								t.Errorf("blank probe planned a run: %s", &stdout)
							}
							if !dryRun {
								if !strings.Contains(stdout.String(), "hydration=provider-owned") || claim.LeaseID != leaseID {
									t.Errorf("plain prewarm did not retain a provider-owned lease: claim=%q stdout=%s", claim.LeaseID, &stdout)
								}
								if lines := strings.Split(strings.TrimSpace(string(calls)), "\n"); len(lines) != 2 || lines[0] != "keygen" || !strings.HasPrefix(lines[1], "blacksmith testbox warmup ") {
									t.Errorf("plain prewarm calls=%q, want keygen and warmup only", calls)
								}
								return
							}
						}
						if len(calls) != 0 {
							t.Errorf("calls before admission=%s, want ZERO warmup/run/stop/keygen calls", calls)
						}
						if claim.LeaseID != "" {
							t.Errorf("retained claim=%q, want no acquired lease", claim.LeaseID)
						}
						keyPath, err := testboxKeyPath(leaseID)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := os.Stat(filepath.Dir(filepath.Dir(keyPath))); !os.IsNotExist(err) {
							t.Errorf("lease key directory created before admission: %v", err)
						}
					})
				}
			})
		}
	}
}

func blacksmithCallerFixture(t *testing.T, config string) (string, string) {
	t.Helper()
	dirs := testutil.IsolateUserDirs(t)
	// App uses the production command runner, so intercept only its
	// external executables; provider registration and policy stay real.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "CRABBOX_") || strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, "")
		}
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dirs.Home, "config.yaml"))
	if err := os.WriteFile(os.Getenv("CRABBOX_CONFIG"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	t.Chdir(repo)
	if out, err := exec.Command("git", "init", "--quiet", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	bin := t.TempDir()
	callsPath := filepath.Join(t.TempDir(), "calls")
	t.Setenv("BLACKSMITH_CALLER_TEST_CALLS", callsPath)
	fixtures := map[string]string{
		"blacksmith": `#!/bin/sh
set -eu
if [ "$1" = --org ]; then shift 2; fi
printf 'blacksmith %s\n' "$*" >> "$BLACKSMITH_CALLER_TEST_CALLS"
case "$1 $2" in
  'testbox warmup') printf 'ready tbx_prewarm123\n' ;;
  'testbox run'|'testbox stop') exit 0 ;;
  *) exit 99 ;;
esac
`,
		"ssh-keygen": `#!/bin/sh
set -eu
printf 'keygen\n' >> "$BLACKSMITH_CALLER_TEST_CALLS"
while [ "$#" -gt 0 ]; do
  if [ "$1" = -f ]; then
    printf 'test-only dummy private key\n' > "$2"
    printf 'ssh-ed25519 AAAA test-only-dummy\n' > "$2.pub"
    exit 0
  fi
  shift
done
exit 99
`,
	}
	for tool, script := range fixtures {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return repo, callsPath
}
