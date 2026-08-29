package blacksmith

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

type admissionClock struct{ calls int }

func (c *admissionClock) Now() time.Time { c.calls++; return time.Time{} }

func blacksmithAdmissionFixture(t *testing.T, provider string) (root, processLog, gitLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake executable traps require /bin/sh")
	}
	dirs := testutil.IsolateUserDirs(t)
	root = dirs.Root
	for _, key := range []string{
		"CRABBOX_PROVIDER", "CRABBOX_PROFILE", "CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE",
		"CRABBOX_COORDINATOR_TOKEN", "CRABBOX_COORDINATOR_TOKEN_COMMAND", "CRABBOX_ENV_ALLOW",
		"CRABBOX_WORKDIR", "CRABBOX_WORK_ROOT", "CRABBOX_NETWORK", "CRABBOX_TAILSCALE",
		"CRABBOX_BLACKSMITH_ORG", "CRABBOX_BLACKSMITH_WORKFLOW", "CRABBOX_BLACKSMITH_JOB", "CRABBOX_BLACKSMITH_REF",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	repo := filepath.Join(root, "repo")
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{repo, bin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(repo)
	config := filepath.Join(repo, "config.yaml")
	t.Setenv("CRABBOX_CONFIG", config)
	if err := os.WriteFile(config, []byte("provider: "+provider+"\nblacksmith:\n  workflow: testbox.yml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	processLog, gitLog = filepath.Join(t.TempDir(), "process-calls"), filepath.Join(t.TempDir(), "git-calls")
	t.Setenv("CRABBOX_TEST_PROCESS_LOG", processLog)
	t.Setenv("CRABBOX_TEST_GIT_LOG", gitLog)
	for _, name := range []string{"blacksmith", "ssh", "ssh-keygen", "rsync", "curl", "git"} {
		script := "#!/bin/sh\nprintf 'unexpected process\\n' >> \"$CRABBOX_TEST_PROCESS_LOG\"\nexit 99\n"
		if name == "git" {
			script = "#!/bin/sh\nprintf 'git\\n' >> \"$CRABBOX_TEST_GIT_LOG\"\nexit 1\n"
		}
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return root, processLog, gitLog
}

func blacksmithAdmissionState(t *testing.T, root string) map[string]string {
	t.Helper()
	entries := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data := []byte{}
		if !entry.IsDir() {
			data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		entries[path] = fmt.Sprintf("%s:%s", info.Mode(), data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return entries
}

func assertBlacksmithAdmissionError(t *testing.T, err error) {
	t.Helper()
	var ee core.ExitError
	if !core.AsExitError(err, &ee) || ee.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	for _, want := range []string{"blacksmith-testbox delegates sync", "--no-sync is not supported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error=%v, missing %q", err, want)
		}
	}
}

func assertBlacksmithNoProcess(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if data, err := os.ReadFile(path); !os.IsNotExist(err) {
			t.Errorf("unexpected process log %s: %q (%v)", path, data, err)
		}
	}
}

func TestBlacksmithPrewarmProbeAdmissionBeforeActivity(t *testing.T) {
	for _, selection := range []string{"canonical", "alias", "config", "config-alias"} {
		for _, dry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dry=%v", selection, dry), func(t *testing.T) {
				provider := blacksmithTestboxProvider
				if selection == "config-alias" {
					provider = "blacksmith"
				}
				root, processLog, gitLog := blacksmithAdmissionFixture(t, provider)
				// Use the real registered adapter, not internal/cli's test double.
				for _, name := range []string{blacksmithTestboxProvider, "blacksmith"} {
					registered, err := core.ProviderFor(name)
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := registered.(Provider); !ok {
						t.Fatalf("registered %s adapter is %T", name, registered)
					}
					if _, ok := registered.(core.RunOptionsValidator); !ok {
						t.Fatal("real provider lacks early admission")
					}
				}
				args := []string{"prewarm", "--probe-command", "git status --short; git diff HEAD --"}
				switch selection {
				case "canonical":
					args = append(args, "--provider", blacksmithTestboxProvider)
				case "alias":
					args = append(args, "--provider", "blacksmith")
				}
				if dry {
					args = append(args, "--dry-run")
				}
				before := blacksmithAdmissionState(t, root)
				var stdout, stderr bytes.Buffer
				err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
				assertBlacksmithAdmissionError(t, err)
				if !strings.Contains(err.Error(), "omit --probe-command or choose a provider that supports the probe options") {
					t.Errorf("missing prewarm remediation: %v", err)
				}
				assertBlacksmithNoProcess(t, processLog, gitLog)
				if !reflect.DeepEqual(before, blacksmithAdmissionState(t, root)) {
					t.Error("rejection mutated fixture config/claim/key/cache state")
				}
				if stdout.Len() != 0 || stderr.Len() != 0 {
					t.Errorf("rejection printed plan/execution output: stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
			})
		}
	}
}

func TestBlacksmithBackendRejectsNoSyncBeforeActivity(t *testing.T) {
	for _, id := range []string{"", "tbx_unallocated"} {
		t.Run("id="+id, func(t *testing.T) {
			root, processLog, gitLog := blacksmithAdmissionFixture(t, blacksmithTestboxProvider)
			runner := &blacksmithFuncRunner{}
			clock := &admissionClock{}
			backend := newTestBlacksmithBackend(baseConfig(), runner)
			backend.rt.Clock = clock
			before := blacksmithAdmissionState(t, root)
			_, err := backend.Run(t.Context(), RunRequest{ID: id, Repo: Repo{Root: filepath.Join(root, "repo")}, NoSync: true, Command: []string{"true"}})
			assertBlacksmithAdmissionError(t, err)
			if clock.calls != 0 || len(runner.calls) != 0 {
				t.Errorf("clock=%d runner=%d calls before rejection", clock.calls, len(runner.calls))
			}
			assertBlacksmithNoProcess(t, processLog, gitLog)
			if !reflect.DeepEqual(before, blacksmithAdmissionState(t, root)) {
				t.Error("rejection changed fresh state")
			}
		})
	}
}

func TestBlacksmithRunProviderAdmission(t *testing.T) {
	for _, id := range []string{"", "tbx_unallocated"} {
		t.Run("id="+id, func(t *testing.T) {
			root, processLog, _ := blacksmithAdmissionFixture(t, blacksmithTestboxProvider)
			before := blacksmithAdmissionState(t, root)
			var stdout, stderr bytes.Buffer
			err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{
				"run", "--provider", "blacksmith", "--id", id, "--no-sync", "--no-hydrate", "--shell", "--", "true",
			})
			assertBlacksmithAdmissionError(t, err)
			assertBlacksmithNoProcess(t, processLog)
			if !reflect.DeepEqual(before, blacksmithAdmissionState(t, root)) {
				t.Error("rejection changed fresh state")
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Errorf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestBlacksmithPrewarmWithoutProbeDryRun(t *testing.T) {
	root, processLog, gitLog := blacksmithAdmissionFixture(t, blacksmithTestboxProvider)
	before := blacksmithAdmissionState(t, root)
	var stdout, stderr bytes.Buffer
	err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"prewarm", "--dry-run"})
	if err != nil {
		t.Fatalf("plain prewarm failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "crabbox warmup") || strings.Contains(stdout.String(), "crabbox run") || strings.Contains(stdout.String(), "actions hydrate") {
		t.Fatalf("plain prewarm plan=%q", stdout.String())
	}
	assertBlacksmithNoProcess(t, processLog, gitLog)
	if !reflect.DeepEqual(before, blacksmithAdmissionState(t, root)) {
		t.Error("dry-run changed fresh state")
	}
}
